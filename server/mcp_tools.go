package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"
)

const (
	toolKeyListMCPTools    = "list_mcp_tools"
	defaultMCPLiveTimeout  = 15 * time.Second
	listMCPToolsDesc       = "Read-only inspection of MCP servers and tool metadata visible to the current agent (tool_count, status, source=cached/live). By default (live=false, include_names=false, include_descriptions=false) returns only compact counts and status metadata from the database cache without listing tool names or descriptions and without starting external processes. For mainagent (the task orchestrator), default inspection also surfaces non-secret metadata of MCP servers available to subordinate task roles (worker, planner, retester, reporter) with callable=false and delegatable_to so mainagent can delegate execution via add_intent. An empty servers list for a single role never means the global MCP registry is empty (check registered_enabled_server_count). Keep include_names=false and include_descriptions=false when only tool_count or status is needed. Set include_names=true to return only tool_names (without descriptions). Set include_descriptions=true or pass tool/filter only when detailed tool descriptions or specific tool lookups are explicitly required. Never exposes env, API keys, credentials, or full configurations."
	listMCPToolsBindFlagV1 = "list_mcp_tools_default_bindings_v1"
)

// mcpLiveTimeout bounds a live=true tools/list probe (capped at 15s; overridable in tests).
var mcpLiveTimeout = defaultMCPLiveTimeout

// defaultListMCPToolsAgents lists the roles that receive list_mcp_tools by default
// in System -> Tools (both chat and task agents).
var defaultListMCPToolsAgents = []string{
	"auto", "planner", "worker", "mainagent", "pentest", "reporter", "retester",
}

// mainAgentSubordinateRoles lists the task-scoped subordinate roles whose MCP
// availability mainagent may inspect (read-only, callable=false) to route work.
var mainAgentSubordinateRoles = []string{
	"worker", "planner", "retester", "reporter", "goals",
}

func isMainAgentSubordinateRole(role string) bool {
	return slices.Contains(mainAgentSubordinateRoles, role)
}

func effectiveMCPLiveTimeout() time.Duration {
	if mcpLiveTimeout <= 0 || mcpLiveTimeout > defaultMCPLiveTimeout {
		return defaultMCPLiveTimeout
	}
	return mcpLiveTimeout
}

func listMCPToolsSchema() map[string]any {
	return objSchema(map[string]any{
		"server":               strParam("Optional: single MCP server name to inspect (e.g. hexstrike, cveintel); required when live=true"),
		"agent":                strParam("Optional: agent role key whose visible MCP metadata to inspect (defaults to current agent; platform operator auto may inspect any role, and task orchestrator mainagent may inspect subordinate task roles worker/planner/retester/reporter/goals)"),
		"live":                 map[string]any{"type": "boolean", "default": false, "description": "Optional: connect live to the specified server and run tools/list (default false reads cache with source=cached; true requires a single server and returns source=live without modifying cache)"},
		"include_names":        map[string]any{"type": "boolean", "default": false, "description": "Optional: include only the array of tool names (tool_names) without descriptions; keep false when only tool_count or server status is requested"},
		"include_descriptions": map[string]any{"type": "boolean", "default": false, "description": "Optional: include tool descriptions (tools); keep false unless descriptions are explicitly required, and prefer combining with 'tool' or 'filter' to inspect specific tools"},
		"tool":                 strParam("Optional: specific tool name (full mcp__<server>__<tool> or short <tool>) to inspect without returning all tools on the server"),
		"filter":               strParam("Optional: case-insensitive substring filter on tool name to narrow returned tool_names or tools"),
	})
}

// mcpLiveSessionCache deduplicates live MCP tools/list results within a single
// agent turn / tool-assembly request so the same MCP server is never connected
// twice in one request.
type mcpLiveSessionCache struct {
	mu    sync.Mutex
	tools map[string][]db.MCPTool
}

func newMCPLiveSessionCache() *mcpLiveSessionCache {
	return &mcpLiveSessionCache{tools: map[string][]db.MCPTool{}}
}

func (c *mcpLiveSessionCache) get(serverName string) ([]db.MCPTool, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ts, ok := c.tools[serverName]
	if !ok {
		return nil, false
	}
	out := make([]db.MCPTool, len(ts))
	copy(out, ts)
	return out, true
}

func (c *mcpLiveSessionCache) put(serverName string, tools []db.MCPTool) {
	if c == nil {
		return
	}
	cp := make([]db.MCPTool, len(tools))
	copy(cp, tools)
	c.mu.Lock()
	c.tools[serverName] = cp
	c.mu.Unlock()
}

// mcpServerSummary is the sanitized, non-secret metadata view of one MCP server.
// Sensitive fields (Env, API keys, credentials, Command, Args, URL, Insecure) are
// intentionally excluded so they can never leak into tool outputs or transcripts.
type mcpServerSummary struct {
	ID               int64        `json:"id"`
	Name             string       `json:"name"`
	Transport        string       `json:"transport"`
	Enabled          bool         `json:"enabled"`
	Status           string       `json:"status"`
	Source           string       `json:"source"`
	Visible          bool         `json:"visible"`
	Callable         bool         `json:"callable"`
	SkillGated       bool         `json:"skill_gated,omitempty"`
	VisibleAgents    []string     `json:"visible_agents,omitempty"`
	DelegatableTo    []string     `json:"delegatable_to,omitempty"`
	ToolCount        int          `json:"tool_count"`
	CachedToolCount  int          `json:"cached_tool_count"`
	MatchedToolCount *int         `json:"matched_tool_count,omitempty"`
	CacheMismatch    bool         `json:"cache_mismatch,omitempty"`
	InSyncWithCache  *bool        `json:"in_sync_with_cache,omitempty"`
	ToolNames        []string     `json:"tool_names,omitempty"`
	Tools            []db.MCPTool `json:"tools,omitempty"`
}

type listMCPToolsInput struct {
	Server              string `json:"server"`
	Agent               string `json:"agent"`
	Live                bool   `json:"live"`
	IncludeNames        bool   `json:"include_names"`
	IncludeDescriptions *bool  `json:"include_descriptions"`
	Tool                string `json:"tool"`
	Filter              string `json:"filter"`
}

type listMCPToolsTool struct {
	actool.CoreTool
	pg           *db.DB
	skillDir     string
	callerAgent  string
	sessionCache *mcpLiveSessionCache
}

func (l *listMCPToolsTool) withCallerAgent(agentKey string) actool.CoreTool {
	if agentKey == "" || l.callerAgent == agentKey {
		return l
	}
	return newListMCPToolsTool(l.pg, l.skillDir, agentKey, l.sessionCache)
}

func (s *Server) toolListMCPTools(callerAgent string) actool.CoreTool {
	var pg *db.DB
	if s != nil && s.m != nil {
		pg = s.m.pg
	}
	var skillDir string
	if s != nil {
		skillDir = s.skillDir
	}
	return newListMCPToolsTool(pg, skillDir, callerAgent, newMCPLiveSessionCache())
}

func newListMCPToolsTool(pg *db.DB, skillDir, callerAgent string, sessionCache *mcpLiveSessionCache) actool.CoreTool {
	if sessionCache == nil {
		sessionCache = newMCPLiveSessionCache()
	}
	base := roTool(
		toolKeyListMCPTools,
		listMCPToolsDesc,
		listMCPToolsSchema(),
		func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			return runListMCPTools(ctx, pg, skillDir, callerAgent, sessionCache, raw)
		},
	)
	return &listMCPToolsTool{
		CoreTool:     base,
		pg:           pg,
		skillDir:     skillDir,
		callerAgent:  callerAgent,
		sessionCache: sessionCache,
	}
}

func runListMCPTools(ctx context.Context, pg *db.DB, skillDir, callerAgent string, sessionCache *mcpLiveSessionCache, raw json.RawMessage) (actool.Result, error) {
	if pg == nil {
		return actool.Errorf("database not initialized"), nil
	}
	var in listMCPToolsInput
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &in); err != nil {
			return actool.Errorf("invalid arguments: " + err.Error()), nil
		}
	}

	caller := strings.TrimSpace(callerAgent)
	reqAgent := strings.TrimSpace(in.Agent)
	reqServer := strings.TrimSpace(in.Server)

	if in.Live && reqServer == "" {
		return actool.Errorf("live=true is only supported for a single specified MCP server; please provide the 'server' parameter"), nil
	}

	targetAgent := caller
	if reqAgent != "" && reqAgent != caller {
		switch caller {
		case "auto":
			targetAgent = reqAgent
		case "mainagent":
			if reqAgent == "*" || strings.EqualFold(reqAgent, "all") || isMainAgentSubordinateRole(reqAgent) {
				targetAgent = reqAgent
			} else {
				return actool.Errorf(fmt.Sprintf("permission denied: task orchestrator %q may only inspect its own or subordinate task roles (%s), not %q", caller, strings.Join(mainAgentSubordinateRoles, ", "), reqAgent)), nil
			}
		default:
			return actool.Errorf(fmt.Sprintf("permission denied: current role %q is not allowed to inspect MCP metadata of role %q", caller, reqAgent)), nil
		}
	}

	// Resolve caller's own MCP visibility to accurately report callable status.
	var (
		callerDirect map[int64]bool
		callerGated  map[string]bool
	)
	if caller != "" {
		if callerAg, err := pg.GetAgentByKey(caller); err == nil && callerAg != nil {
			callerDirect, callerGated = agentMCPVisibility(pg, skillDir, callerAg.ID)
		}
	}

	// Resolve target agent's MCP visibility (unless auto/mainagent requested "*" / "all").
	inspectAll := (caller == "auto" || caller == "mainagent") && (targetAgent == "*" || strings.EqualFold(targetAgent, "all"))
	var (
		targetDirect map[int64]bool
		targetGated  map[string]bool
	)
	if !inspectAll && targetAgent != "" {
		targetAg, err := pg.GetAgentByKey(targetAgent)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if targetAg == nil {
			return actool.Errorf(fmt.Sprintf("agent %q not found", targetAgent)), nil
		}
		targetDirect, targetGated = agentMCPVisibility(pg, skillDir, targetAg.ID)
	}

	allServers, err := pg.ListMCP()
	if err != nil {
		return actool.Errorf("failed to list MCP servers: " + err.Error()), nil
	}

	// Map agent IDs -> keys and precompute skill-gated visibility for operator / orchestrator inspection.
	var (
		agentKeyByID    map[int64]string
		subordinateVis  map[string]map[int64]bool
		subordinateGate map[string]map[string]bool
	)
	if caller == "auto" || caller == "mainagent" {
		if ags, err := pg.ListAgents(); err == nil {
			agentKeyByID = make(map[int64]string, len(ags))
			subordinateVis = make(map[string]map[int64]bool)
			subordinateGate = make(map[string]map[string]bool)
			for _, ag := range ags {
				agentKeyByID[ag.ID] = ag.Key
				if caller == "mainagent" && isMainAgentSubordinateRole(ag.Key) {
					d, g := agentMCPVisibility(pg, skillDir, ag.ID)
					subordinateVis[ag.Key] = d
					subordinateGate[ag.Key] = g
				}
			}
		}
	}

	serverVisibleAndDelegatable := func(m *db.MCPServer) (visAgents []string, delegatable []string) {
		if len(agentKeyByID) == 0 {
			return nil, nil
		}
		roleSet := map[string]bool{}
		if agIDs, err := pg.ResourceAgents("mcp", m.ID); err == nil {
			for _, id := range agIDs {
				if k := agentKeyByID[id]; k != "" {
					roleSet[k] = true
				}
			}
		}
		for role, gMap := range subordinateGate {
			if gMap[m.Name] {
				roleSet[role] = true
			}
		}
		for role, dMap := range subordinateVis {
			if dMap[m.ID] {
				roleSet[role] = true
			}
		}
		for k := range roleSet {
			visAgents = append(visAgents, k)
			if isMainAgentSubordinateRole(k) {
				delegatable = append(delegatable, k)
			}
		}
		slices.Sort(visAgents)
		slices.Sort(delegatable)
		return visAgents, delegatable
	}

	buildSummary := func(m *db.MCPServer, visToTarget, gatedToTarget, visToCaller bool) (mcpServerSummary, error) {
		cachedTools, err := cachedMCPTools(pg, m)
		if err != nil {
			return mcpServerSummary{}, err
		}
		status := "enabled"
		if !m.Enabled {
			status = "disabled"
		}
		summary := mcpServerSummary{
			ID:              m.ID,
			Name:            m.Name,
			Transport:       m.Transport,
			Enabled:         m.Enabled,
			Status:          status,
			Source:          "cached",
			Visible:         visToTarget,
			Callable:        m.Enabled && visToCaller,
			SkillGated:      gatedToTarget,
			ToolCount:       len(cachedTools),
			CachedToolCount: len(cachedTools),
		}
		if caller == "auto" || caller == "mainagent" {
			visAgents, delegatable := serverVisibleAndDelegatable(m)
			if len(visAgents) > 0 {
				summary.VisibleAgents = visAgents
			}
			if caller == "mainagent" && !summary.Callable && len(delegatable) > 0 {
				summary.DelegatableTo = delegatable
			}
		}

		activeTools := cachedTools
		if in.Live {
			if !m.Enabled {
				return mcpServerSummary{}, fmt.Errorf("MCP server %q is disabled; cannot perform live discovery", m.Name)
			}
			liveTools, ok := sessionCache.get(m.Name)
			if !ok {
				var err error
				liveTools, err = queryLiveMCPTools(ctx, m, effectiveMCPLiveTimeout())
				if err != nil {
					return mcpServerSummary{}, err
				}
				sessionCache.put(m.Name, liveTools)
			}
			mismatch := mcpToolsMismatch(cachedTools, liveTools)
			inSync := !mismatch
			summary.Source = "live"
			summary.ToolCount = len(liveTools)
			summary.CacheMismatch = mismatch
			summary.InSyncWithCache = &inSync
			activeTools = liveTools
		}

		toolQuery := strings.TrimSpace(in.Tool)
		filterQuery := strings.TrimSpace(in.Filter)
		hasFilter := toolQuery != "" || filterQuery != ""

		targetTools := activeTools
		if hasFilter {
			targetTools = filterMCPTools(activeTools, m.Name, toolQuery, filterQuery)
			matchedCount := len(targetTools)
			summary.MatchedToolCount = &matchedCount
		}

		incDesc := false
		if in.IncludeDescriptions != nil {
			incDesc = *in.IncludeDescriptions
		} else if toolQuery != "" {
			// Requesting a specific tool by name returns its detailed description
			// unless include_descriptions=false was explicitly specified.
			incDesc = true
		}

		incNames := in.IncludeNames
		if hasFilter && !incDesc {
			incNames = true
		}

		if incNames {
			names := make([]string, len(targetTools))
			for i, t := range targetTools {
				names[i] = t.Name
			}
			summary.ToolNames = names
		}
		if incDesc {
			toolsCopy := make([]db.MCPTool, len(targetTools))
			copy(toolsCopy, targetTools)
			summary.Tools = toolsCopy
		}
		return summary, nil
	}

	if reqServer != "" {
		target := findMCPServerByName(allServers, reqServer)
		if target == nil {
			return actool.Errorf(fmt.Sprintf("MCP server %q not found", reqServer)), nil
		}
		visToTarget := inspectAll || targetDirect[target.ID] || targetGated[target.Name]
		gatedToTarget := targetGated[target.Name]
		visToCaller := callerDirect[target.ID] || callerGated[target.Name]
		_, delegatable := serverVisibleAndDelegatable(target)
		if inspectAll && caller == "mainagent" {
			visToTarget = visToCaller || len(delegatable) > 0
		}

		if !visToTarget {
			allowedCrossInspect := (caller == "auto" && reqAgent == "") ||
				(caller == "mainagent" && reqAgent == "" && len(delegatable) > 0)
			if !allowedCrossInspect {
				return actool.Errorf(fmt.Sprintf("MCP server %q is not visible or authorized for agent %q", target.Name, targetAgent)), nil
			}
		}

		summary, err := buildSummary(target, visToTarget, gatedToTarget, visToCaller)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		resp := map[string]any{
			"server":            summary.Name,
			"agent":             targetAgent,
			"caller_agent":      caller,
			"source":            summary.Source,
			"transport":         summary.Transport,
			"enabled":           summary.Enabled,
			"status":            summary.Status,
			"visible":           summary.Visible,
			"callable":          summary.Callable,
			"tool_count":        summary.ToolCount,
			"cached_tool_count": summary.CachedToolCount,
		}
		if summary.SkillGated {
			resp["skill_gated"] = true
		}
		if len(summary.VisibleAgents) > 0 {
			resp["visible_agents"] = summary.VisibleAgents
		}
		if len(summary.DelegatableTo) > 0 {
			resp["delegatable_to"] = summary.DelegatableTo
		}
		if summary.MatchedToolCount != nil {
			resp["matched_tool_count"] = *summary.MatchedToolCount
		}
		if in.Live {
			resp["cache_mismatch"] = summary.CacheMismatch
			resp["in_sync_with_cache"] = !summary.CacheMismatch
		}
		if summary.ToolNames != nil {
			resp["tool_names"] = summary.ToolNames
		}
		if summary.Tools != nil {
			resp["tools"] = summary.Tools
		}
		return jsonResult(resp)
	}

	// List mode (cached only; live=true without server was rejected above).
	items := make([]mcpServerSummary, 0)
	var disabledServers []string
	totalTools := 0
	registeredEnabledCount := 0
	directServerCount := 0
	subordinateServerCount := 0
	includeSubordinates := caller == "mainagent" && (reqAgent == "" || inspectAll)

	for _, m := range allServers {
		if m.Enabled {
			registeredEnabledCount++
		}
		visToCaller := callerDirect[m.ID] || callerGated[m.Name]
		visToTarget := inspectAll || targetDirect[m.ID] || targetGated[m.Name]
		_, delegatable := serverVisibleAndDelegatable(m)
		if inspectAll && caller == "mainagent" {
			visToTarget = visToCaller || len(delegatable) > 0
		}

		subordinateOnly := false
		if !visToTarget {
			if includeSubordinates && len(delegatable) > 0 {
				subordinateOnly = true
			} else {
				continue
			}
		}
		if !m.Enabled {
			disabledServers = append(disabledServers, m.Name)
			continue
		}
		summary, err := buildSummary(m, visToTarget, targetGated[m.Name], visToCaller)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if subordinateOnly {
			subordinateServerCount++
		} else {
			directServerCount++
		}
		totalTools += summary.ToolCount
		items = append(items, summary)
	}

	resp := map[string]any{
		"source":                          "cached",
		"agent":                           targetAgent,
		"caller_agent":                    caller,
		"server_count":                    len(items),
		"registered_enabled_server_count": registeredEnabledCount,
		"tool_count":                      totalTools,
		"servers":                         items,
	}
	if includeSubordinates {
		resp["direct_server_count"] = directServerCount
		resp["subordinate_server_count"] = subordinateServerCount
		if subordinateServerCount > 0 {
			resp["delegation_note"] = "MCP servers with callable=false and delegatable_to=[...] are registered in ARTEX and available to subordinate task roles (e.g. worker/planner). MainAgent cannot invoke their MCP tools directly; delegate execution to worker via add_intent and read results via get_worker_output / list_facts / list_findings instead of running Bash wrapper scripts."
		}
	}
	if len(items) == 0 && registeredEnabledCount > 0 {
		resp["registry_note"] = fmt.Sprintf("Role %q has 0 directly bound MCP servers, but %d enabled MCP server(s) are registered in the global ARTEX registry for other roles. Do not interpret server_count=0 for the current role as an empty global MCP registry.", targetAgent, registeredEnabledCount)
	}
	if len(disabledServers) > 0 {
		resp["disabled_servers"] = disabledServers
	}
	return jsonResult(resp)
}

func findMCPServerByName(servers []*db.MCPServer, name string) *db.MCPServer {
	for _, m := range servers {
		if m.Name == name {
			return m
		}
	}
	for _, m := range servers {
		if strings.EqualFold(m.Name, name) {
			return m
		}
	}
	return nil
}

func agentMCPVisibility(pg *db.DB, skillDir string, agentID int64) (direct map[int64]bool, gated map[string]bool) {
	mcpIDs, _ := pg.AgentVisible(agentID, "mcp")
	direct = idSet(mcpIDs)
	gated = map[string]bool{}
	if skillDir == "" {
		return direct, gated
	}
	if names, _ := pg.AgentSkillNames(agentID); len(names) > 0 {
		nameSet := make(map[string]bool, len(names))
		for _, n := range names {
			nameSet[n] = true
		}
		if allReg, err := skill.LoadDir(skillDir); err == nil && allReg != nil {
			for _, s := range allReg.List() {
				if s.Dir != "" && nameSet[filepath.Base(s.Dir)] {
					for _, srv := range s.MCPs {
						gated[srv] = true
					}
				}
			}
		}
	}
	return direct, gated
}

func cachedMCPTools(pg *db.DB, m *db.MCPServer) ([]db.MCPTool, error) {
	detailed, err := pg.MCPToolsDetailed(m.ID)
	if err != nil {
		return nil, err
	}
	if len(detailed) > 0 || len(m.Tools) == 0 {
		return normalizeMCPTools(detailed), nil
	}
	fallback := make([]db.MCPTool, len(m.Tools))
	for i, name := range m.Tools {
		fallback[i] = db.MCPTool{Name: name}
	}
	return normalizeMCPTools(fallback), nil
}

func toDBMCPTools(ts []actool.CoreTool) []db.MCPTool {
	out := make([]db.MCPTool, 0, len(ts))
	for _, t := range ts {
		out = append(out, db.MCPTool{
			Name:        t.Name(),
			Description: t.Description(),
		})
	}
	return normalizeMCPTools(out)
}

func normalizeMCPTools(in []db.MCPTool) []db.MCPTool {
	out := make([]db.MCPTool, len(in))
	copy(out, in)
	slices.SortFunc(out, func(a, b db.MCPTool) int {
		return cmp.Compare(a.Name, b.Name)
	})
	return out
}

func shortMCPToolName(fullName, serverName string) string {
	prefix := "mcp__" + serverName + "__"
	if strings.HasPrefix(fullName, prefix) {
		return strings.TrimPrefix(fullName, prefix)
	}
	if strings.HasPrefix(fullName, "mcp__") {
		rest := strings.TrimPrefix(fullName, "mcp__")
		if idx := strings.Index(rest, "__"); idx >= 0 {
			return rest[idx+2:]
		}
	}
	return fullName
}

// filterMCPTools narrows tools by specific tool name (exact match on full or
// short name, falling back to case-insensitive substring) and/or substring filter.
func filterMCPTools(tools []db.MCPTool, serverName, toolQuery, filterQuery string) []db.MCPTool {
	out := tools
	if toolQuery != "" {
		var exact []db.MCPTool
		for _, t := range out {
			short := shortMCPToolName(t.Name, serverName)
			if strings.EqualFold(t.Name, toolQuery) || strings.EqualFold(short, toolQuery) {
				exact = append(exact, t)
			}
		}
		if len(exact) > 0 {
			out = exact
		} else {
			q := strings.ToLower(toolQuery)
			var sub []db.MCPTool
			for _, t := range out {
				short := strings.ToLower(shortMCPToolName(t.Name, serverName))
				if strings.Contains(strings.ToLower(t.Name), q) || strings.Contains(short, q) {
					sub = append(sub, t)
				}
			}
			out = sub
		}
	}
	if filterQuery != "" {
		q := strings.ToLower(filterQuery)
		var sub []db.MCPTool
		for _, t := range out {
			short := strings.ToLower(shortMCPToolName(t.Name, serverName))
			if strings.Contains(strings.ToLower(t.Name), q) || strings.Contains(short, q) {
				sub = append(sub, t)
			}
		}
		out = sub
	}
	return out
}

func mcpToolsMismatch(cached, live []db.MCPTool) bool {
	if len(cached) != len(live) {
		return true
	}
	cNorm := normalizeMCPTools(cached)
	lNorm := normalizeMCPTools(live)
	for i := range cNorm {
		if cNorm[i].Name != lNorm[i].Name {
			return true
		}
	}
	return false
}

// queryLiveMCPTools dials a single MCP server via connectMCP, invokes tools/list
// within timeout (<=15s), and always closes the client before returning. It never
// executes any MCP tool and never writes to mcp_tools_cache.
func queryLiveMCPTools(ctx context.Context, m *db.MCPServer, timeout time.Duration) ([]db.MCPTool, error) {
	liveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type dialResult struct {
		cl  mcpClient
		err error
	}
	dialCh := make(chan dialResult, 1)
	go func() {
		cl, err := connectMCP(liveCtx, m)
		dialCh <- dialResult{cl: cl, err: err}
	}()

	var cl mcpClient
	select {
	case res := <-dialCh:
		if res.err != nil {
			if res.cl != nil {
				_ = res.cl.Close()
			}
			if errors.Is(res.err, context.DeadlineExceeded) || errors.Is(liveCtx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("MCP server %q live connect timeout after %s (source=live): %w", m.Name, timeout, context.DeadlineExceeded)
			}
			return nil, fmt.Errorf("MCP server %q live connect failed (source=live): %w", m.Name, res.err)
		}
		cl = res.cl
	case <-liveCtx.Done():
		go func() {
			if res := <-dialCh; res.cl != nil {
				_ = res.cl.Close()
			}
		}()
		return nil, fmt.Errorf("MCP server %q live connect timeout after %s (source=live): %w", m.Name, timeout, liveCtx.Err())
	}

	var closeOnce sync.Once
	closeClient := func() {
		closeOnce.Do(func() {
			_ = cl.Close()
		})
	}
	defer closeClient()

	type toolsResult struct {
		tools []actool.CoreTool
		err   error
	}
	toolsCh := make(chan toolsResult, 1)
	go func() {
		ts, err := cl.Tools(liveCtx)
		toolsCh <- toolsResult{tools: ts, err: err}
	}()

	select {
	case res := <-toolsCh:
		closeClient()
		if res.err != nil {
			if errors.Is(res.err, context.DeadlineExceeded) || errors.Is(liveCtx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("MCP server %q tools/list timeout after %s (source=live): %w", m.Name, timeout, context.DeadlineExceeded)
			}
			return nil, fmt.Errorf("MCP server %q tools/list failed (source=live): %w", m.Name, res.err)
		}
		return toDBMCPTools(res.tools), nil
	case <-liveCtx.Done():
		closeClient()
		return nil, fmt.Errorf("MCP server %q tools/list timeout after %s (source=live): %w", m.Name, timeout, liveCtx.Err())
	}
}

// seedListMCPTools seeds the list_mcp_tools row into the tools catalog (first-insert
// only) and runs a one-time binding backfill so existing databases also expose it to
// the standard roles without overwriting subsequent user edits in System -> Tools.
func seedListMCPTools(pg *db.DB) {
	if pg == nil {
		return
	}
	tool := newListMCPToolsTool(pg, "", "", nil)
	schema, _ := json.Marshal(tool.InputSchema())
	agents, _ := json.Marshal(defaultListMCPToolsAgents)
	if err := pg.SeedTool(tool.Name(), tool.Description(), schema, agents); err != nil {
		log.Printf("[tools] seed %s failed: %v", tool.Name(), err)
		return
	}
	const descRefreshFlag = "list_mcp_tools_en_desc_v3"
	if v, _, _ := pg.GetSetting(descRefreshFlag); v != "true" {
		if err := pg.RefreshToolDefaults(tool.Name(), tool.Description(), schema); err == nil {
			_ = pg.SetSetting(descRefreshFlag, "true")
		}
	}
	const workerOutputRefreshFlag = "get_worker_output_wait_schema_v1"
	if v, _, _ := pg.GetSetting(workerOutputRefreshFlag); v != "true" {
		for _, sd := range agent.BuiltinToolSeeds() {
			if sd.Key == "get_worker_output" {
				s, _ := json.Marshal(sd.Schema)
				if err := pg.RefreshToolDefaults(sd.Key, sd.Desc, s); err == nil {
					_ = pg.SetSetting(workerOutputRefreshFlag, "true")
				}
				break
			}
		}
	}
	if v, _, _ := pg.GetSetting(listMCPToolsBindFlagV1); v == "true" {
		return
	}
	bindRoles := append([]string{}, defaultListMCPToolsAgents...)
	if ags, err := pg.ListAgents(); err == nil {
		for _, ag := range ags {
			if !ag.Builtin && ag.Role == "assistant" && !contains(bindRoles, ag.Key) {
				bindRoles = append(bindRoles, ag.Key)
			}
		}
	}
	for _, role := range bindRoles {
		if err := pg.AddAgentToToolBinding(role, []string{toolKeyListMCPTools}); err != nil {
			log.Printf("[tools] %s default binding for %s failed: %v", toolKeyListMCPTools, role, err)
			return
		}
	}
	_ = pg.SetSetting(listMCPToolsBindFlagV1, "true")
}
