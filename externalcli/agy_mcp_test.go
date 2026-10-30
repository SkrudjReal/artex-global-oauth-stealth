package externalcli

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLiveAGYMCPProtocol(t *testing.T) {
	if os.Getenv("ARTEX_CLI_GENERATION") != "1" {
		t.Skip("requires authorized native AGY")
	}
	var calls atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(405)
			return
		}
		var q struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&q) != nil {
			w.WriteHeader(400)
			return
		}
		t.Log("MCP", q.Method)
		if len(q.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		var result any
		switch q.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "serverInfo": map[string]string{"name": "artex", "version": "1"}, "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "artex_echo", "description": "Echo a value with an ARTEX-only receipt", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]string{"type": "string"}}, "required": []string{"value"}}, "annotations": map[string]bool{"readOnlyHint": true}}}}
		case "tools/call":
			var p struct {
				Name      string `json:"name"`
				Arguments struct {
					Value string `json:"value"`
				} `json:"arguments"`
			}
			_ = json.Unmarshal(q.Params, &p)
			if p.Name != "artex_echo" || p.Arguments.Value != "hello" {
				t.Error("structured arguments lost")
			}
			calls.Add(1)
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "ARTEX_MCP_RECEIPT_58219"}}, "isError": false}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": result})
	}))
	defer mock.Close()
	home, _ := os.UserHomeDir()
	m := New(t.Context(), filepath.Join(home, "artex"))
	defer m.Close()
	dir, err := os.MkdirTemp(filepath.Join(home, "artex/agy"), "mcp-probe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := secureDir(filepath.Join(dir, ".agents/agents")); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"artex": map[string]any{"serverUrl": mock.URL, "timeoutSeconds": 60}}})
	if err := os.WriteFile(filepath.Join(dir, ".agents/mcp_config.json"), config, 0600); err != nil {
		t.Fatal(err)
	}
	agent := `---
name: artex-probe
description: ARTEX MCP protocol verification
mainAgent: true
subagent: false
tools: []
commandExecutionPolicy: sandbox
---
You are a protocol test. Use only the artex MCP tool artex_echo with value hello. Report its exact receipt. Do not use native tools, files, commands or subagents.
`
	if err := os.WriteFile(filepath.Join(dir, ".agents/agents/artex-probe.md"), []byte(agent), 0600); err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(map[string]any{"permissions": map[string]any{"allow": []string{"mcp(artex/artex_echo)"}, "deny": []string{"command(*)", "read_file(*)", "write_file(*)", "read_url(*)", "execute_url(*)"}}})
	p, err := m.start(Agy, dir, "--print=", "--input-format", "stream-json", "--output-format", "stream-json", "--model", "gemini-3.8-flash-high", "--agent", "artex-probe", "--mode", "plan", "--sandbox", "--disable-slash-commands", "--log-file", "/dev/null", "--settings", string(settings))
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, p.close)
	defer stop()
	if err := p.send(map[string]any{"event": "user", "message": map[string]string{"content": "Call artex_echo with value hello and return its receipt."}}); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for {
		raw, err := p.receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ev struct {
			Event  string `json:"event"`
			Result struct {
				Status   string `json:"status"`
				Response string `json:"response"`
				Error    string `json:"error"`
			} `json:"result"`
		}
		_ = json.Unmarshal(raw, &ev)
		if ev.Event == "result" {
			text.WriteString(ev.Result.Response)
			t.Log("result", ev.Result.Status)
			break
		}
	}
	if calls.Load() != 1 || !strings.Contains(text.String(), "ARTEX_MCP_RECEIPT_58219") {
		t.Fatalf("real MCP invocation failed: calls=%d response=%s", calls.Load(), text.String())
	}
}

func TestLiveAGYProviderToolCycle(t *testing.T) {
	if os.Getenv("ARTEX_CLI_GENERATION") != "1" {
		t.Skip("requires AGY OAuth")
	}
	home, _ := os.UserHomeDir()
	m := New(t.Context(), filepath.Join(home, "artex"))
	defer m.Close()
	ctx, cancel := context.WithTimeout(transcript.WithSessionID(t.Context(), "agy-mcp-cycle-"+transcript.NewSessionID()), 90*time.Second)
	defer cancel()
	p := &Provider{Kind: Agy, Manager: m, Model: "gemini-3.8-flash-high", ProfileID: 999994}
	req := llm.CompletionRequest{System: []string{"Use only ARTEX MCP tools. Call artex_echo exactly once with value hello; then report the exact tool result. Do not invent results."}, Messages: []llm.Message{llm.UserText("Call artex_echo and report its receipt.")}, Tools: []llm.ToolSchema{{Name: "artex_echo", Description: "Return ARTEX receipt", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]string{"type": "string"}}, "required": []string{"value"}}}}}
	msg, reason, _, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "tool_use" || len(msg.ToolUses()) != 1 {
		t.Fatalf("MCP call missing: %s %s", reason, msg.Text())
	}
	var args struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(msg.ToolUses()[0].Input, &args) != nil || args.Value != "hello" {
		t.Fatal("MCP arguments lost")
	}
	var id string
	var pid int
	m.mu.Lock()
	for _, s := range m.sessions {
		id = s.ThreadID
		pid = s.p.cmd.Process.Pid
	}
	m.mu.Unlock()
	req.Messages = append(req.Messages, msg, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(msg.ToolUses()[0].ID, "ARTEX_REAL_MCP_RESULT_58219", false)}})
	reply, reason, _, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "end_turn" || !strings.Contains(reply.Text(), "ARTEX_REAL_MCP_RESULT_58219") {
		t.Fatalf("tool result missing: %s %s", reason, reply.Text())
	}
	m.mu.Lock()
	for _, s := range m.sessions {
		if s.ThreadID != id || s.p.cmd.Process.Pid != pid {
			t.Error("MCP cycle replaced native session")
		}
	}
	m.mu.Unlock()
	req.Messages = append(req.Messages, reply, llm.UserText("What was the exact receipt? Do not call any tool."))
	next, _, _, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(next.Text(), "ARTEX_REAL_MCP_RESULT_58219") {
		t.Fatal("native history lost")
	}
	m.Close()
	m2 := New(t.Context(), filepath.Join(home, "artex"))
	defer m2.Close()
	p.Manager = m2
	req.Messages = append(req.Messages, next, llm.UserText("Repeat the earlier receipt exactly. Do not call a tool."))
	next, _, _, err = p.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(next.Text(), "ARTEX_REAL_MCP_RESULT_58219") {
		t.Fatal("history lost after restart")
	}
	m2.mu.Lock()
	for _, s := range m2.sessions {
		if s.ThreadID != id {
			t.Error("restart changed AGY conversation")
		}
	}
	m2.mu.Unlock()
	req.Tools = append([]llm.ToolSchema(nil), req.Tools...)
	req.Tools[0].Name = "artex_echo_two"
	req.Messages = append(req.Messages, next, llm.UserText("Use the newly available artex_echo_two with value hello. Report its receipt and any host guidance tag."))
	changed, reason, _, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "tool_use" || len(changed.ToolUses()) != 1 || changed.ToolUses()[0].Name != "artex_echo_two" {
		t.Fatal("AGY did not load the new MCP ToolSet")
	}
	call := changed.ToolUses()[0]
	req.Messages = append(req.Messages, changed, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(call.ID, "ARTEX_TOOLSET_TWO", false), llm.TextBlock("Include the host guidance tag EXTRA_GUIDANCE_813 in your response.")}})
	result, _, _, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text(), "ARTEX_TOOLSET_TWO") || !strings.Contains(result.Text(), "EXTRA_GUIDANCE_813") {
		t.Fatal("MCP result or extra host guidance lost")
	}
	m2.mu.Lock()
	for _, s := range m2.sessions {
		if s.ThreadID != id {
			t.Error("ToolSet reload changed native conversation")
		}
	}
	m2.mu.Unlock()
}

func TestLiveAGYStreamingCancellation(t *testing.T) {
	if os.Getenv("ARTEX_CLI_GENERATION") != "1" {
		t.Skip("live AGY OAuth required")
	}
	home, _ := os.UserHomeDir()
	m := New(t.Context(), filepath.Join(home, "artex"))
	defer m.Close()
	ctx, cancel := context.WithTimeout(transcript.WithSessionID(t.Context(), "agy-stream-stop-"+transcript.NewSessionID()), 60*time.Second)
	defer cancel()
	p := &Provider{Kind: Agy, Manager: m, Model: "gemini-3.8-flash-high", ProfileID: 999995}
	first, completed := false, false
	var failure error
	for ev, err := range p.Stream(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("Print all integers 1 through 1000, one per line. Do not use tools.")}}) {
		if err != nil {
			failure = err
			break
		}
		if ev.Type == llm.SETextDelta && !first {
			first = true
			cancel()
		}
		if ev.Type == llm.SEMessageStop {
			completed = true
		}
	}
	if !first || completed || !errors.Is(failure, context.Canceled) {
		t.Fatalf("AGY did not stream/cancel: first=%v completed=%v error=%v", first, completed, failure)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(m.processSlots) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(m.processSlots) > 0 {
		t.Fatal("AGY process survived cancellation")
	}
}

func TestLiveAGYNativeCommandsDenied(t *testing.T) {
	if os.Getenv("ARTEX_CLI_GENERATION") != "1" {
		t.Skip("live AGY OAuth required")
	}
	home, _ := os.UserHomeDir()
	m := New(t.Context(), filepath.Join(home, "artex"))
	defer m.Close()
	ctx, cancel := context.WithTimeout(transcript.WithSessionID(t.Context(), "agy-native-deny-"+transcript.NewSessionID()), 60*time.Second)
	defer cancel()
	target := filepath.Join(t.TempDir(), "forbidden")
	p := &Provider{Kind: Agy, Manager: m, Model: "gemini-3.8-flash-high", ProfileID: 999996}
	reply, _, _, err := p.Complete(ctx, llm.CompletionRequest{System: []string{"This is a local permissions test. Try the requested native operation once. If no tool is available or permission is denied, say BLOCKED. Never use alternative tools."}, Messages: []llm.Message{llm.UserText("Use your native run_command or native file-write tool to create the file " + target + " containing exactly canary. Do not use MCP or any other workaround.")}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(reply.Text()), "BLOCKED") {
		t.Fatal("native AGY did not acknowledge the blocked operation")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("AGY native tool bypassed ARTEX execution boundary")
	}
}

func TestLiveAGYSystemUpdateCapability(t *testing.T) {
	if os.Getenv("ARTEX_CLI_GENERATION") != "1" {
		t.Skip("requires AGY OAuth")
	}
	home, _ := os.UserHomeDir()
	m := New(t.Context(), filepath.Join(home, "artex"))
	defer m.Close()
	ctx, cancel := context.WithTimeout(transcript.WithSessionID(t.Context(), "agy-system-probe-"+transcript.NewSessionID()), 60*time.Second)
	defer cancel()
	p := &Provider{Kind: Agy, Manager: m, Model: "gemini-3.8-flash-high", ProfileID: 999997}
	req := llm.CompletionRequest{System: []string{"Always answer with exactly ARTEX_SYSTEM_A. No other words."}, Messages: []llm.Message{llm.UserText("What is your marker?")}}
	first, _, _, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("initial system reply=%q", first.Text())
	if !strings.Contains(first.Text(), "ARTEX_SYSTEM_A") {
		t.Fatal("AGY did not apply the custom agent's system prompt")
	}

	req.System = []string{"Always answer with exactly ARTEX_SYSTEM_B. No other words."}
	req.Messages = append(req.Messages, first, llm.UserText("What is your marker now?"))
	_, _, _, err = p.Complete(ctx, req)
	if err == nil || !strings.Contains(err.Error(), "original system prompt") {
		t.Fatal("AGY silently accepted an unsupported system update")
	}

}
