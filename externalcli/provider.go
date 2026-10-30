package externalcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

// Provider uses structured app-server history and dynamic tools. ARTEX remains
// the executor; CLI tools and approval requests are never forwarded as approvals.
type Provider struct {
	Kind      string
	Manager   *Manager
	Model     string
	ProfileID int64
	Effort    string
	Identity  func(context.Context) (taskID, role string)
}

type providerState struct {
	Usage       llm.Usage     `json:"native_usage,omitempty"`
	History     []llm.Message `json:"history"`
	Tools       string        `json:"tools"`
	System      string        `json:"system"`
	Dirty       bool          `json:"dirty"`
	Awaiting    bool          `json:"awaiting_tool"`
	cancelWatch func() bool   `json:"-"`
	Pending     *wire         `json:"-"`
}

func sameItems(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func (p *Provider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		active := true
		err := p.stream(ctx, req, func(e llm.StreamEvent) error {
			if !yield(e, nil) {
				active = false
				if e.Type == llm.SEMessageStop {
					return nil
				}
				return context.Canceled
			}
			return nil
		})
		if err != nil && active {
			yield(llm.StreamEvent{}, err)
		}
	}
}
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	msg := llm.Message{Role: llm.RoleAssistant}
	var usage llm.Usage
	reason := ""
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			return msg, reason, usage, err
		}
		switch ev.Type {
		case llm.SETextDelta:
			if len(msg.Content) == 0 || msg.Content[len(msg.Content)-1].Type != llm.BlockText {
				msg.Content = append(msg.Content, llm.TextBlock(""))
			}
			msg.Content[len(msg.Content)-1].Text += ev.Text
		case llm.SEToolUseStart:
			msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockToolUse, ID: ev.ToolID, Name: ev.ToolName})
		case llm.SEToolInputJSON:
			if len(msg.Content) > 0 {
				msg.Content[len(msg.Content)-1].Input = append(msg.Content[len(msg.Content)-1].Input, ev.Text...)
			}
		case llm.SEMessageDelta:
			reason = ev.StopReason
			usage = ev.Usage
		}
	}
	return msg, reason, usage, nil
}

// Convert host messages without flattening tool calls/results into user text.
func codexHistory(messages []llm.Message) ([]any, error) {
	items := []any{}
	for _, msg := range messages {
		for _, b := range msg.Content {
			switch b.Type {
			case llm.BlockText:
				kind := "input_text"
				if msg.Role == llm.RoleAssistant {
					kind = "output_text"
				}
				items = append(items, map[string]any{"type": "message", "role": string(msg.Role), "content": []any{map[string]string{"type": kind, "text": b.Text}}})
			case llm.BlockToolUse:
				if !json.Valid(b.Input) {
					return nil, errors.New("invalid tool arguments in ARTEX history")
				}
				items = append(items, map[string]any{"type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": string(b.Input)})
			case llm.BlockToolResult:
				var text []string
				for _, c := range b.Content {
					if c.Type != llm.BlockText {
						return nil, errors.New("Codex tool results currently require text content")
					}
					text = append(text, c.Text)
				}
				result := strings.Join(text, "\n")
				if b.IsError {
					result = "Tool failed: " + result
				}
				items = append(items, map[string]any{"type": "function_call_output", "call_id": b.ToolUseID, "output": result})
			case llm.BlockThinking: // Provider reasoning is not portable across formats.
			default:
				return nil, fmt.Errorf("unsupported Codex history block %q", b.Type)
			}
		}
	}
	return items, nil
}
func (p *Provider) stream(ctx context.Context, req llm.CompletionRequest, emit func(llm.StreamEvent) error) (retErr error) {
	if p.Kind == Agy {
		return p.agyStream(ctx, req, emit)
	}
	if p.Manager == nil {
		return errors.New("Codex CLI manager is unavailable")
	}
	// shortcut: CLI sampling is fixed; accept the host judge's zero hint until the CLI exposes temperature.
	if len(req.Stop) > 0 || (req.Temperature != nil && *req.Temperature != 0) {
		return errors.New("Codex CLI does not support temperature or stop sequences")
	}
	items, err := codexHistory(req.Messages)
	if err != nil {
		return err
	}
	sid := transcript.SessionIDFrom(ctx)
	if sid == "" {
		sid = transcript.NewSessionID()
	}
	taskID, role := "", "provider"
	if p.Identity != nil {
		taskID, role = p.Identity(ctx)
		role = "provider/" + role
	}
	s, err := p.Manager.session(Request{Kind: Codex, ProfileID: p.ProfileID, Model: p.Model, TaskID: taskID, Role: role, SessionID: sid})
	if err != nil {
		return err
	}
	if !s.mu.TryLock() {
		return errors.New("Codex session already running")
	}
	defer s.mu.Unlock()
	runCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	defer func() { s.last = time.Now(); _ = harden(filepath.Join(p.Manager.root, "codex")) }()
	if s.p != nil {
		select {
		case <-s.p.done:
			s.p = nil
		case <-s.p.stopped:
			s.p = nil
		default:
		}
	}
	reconnected := s.p == nil
	if s.p == nil {
		s.p, err = p.Manager.codexProcess(ctx, filepath.Join(s.dir, "work"))
		if err != nil {
			return err
		}
	}
	proc := s.p
	proc.holdTools = true
	stop := context.AfterFunc(ctx, proc.close)
	defer stop()
	defer func() {
		if retErr != nil {
			proc.close()
			s.p = nil
		}
	}()
	tools := []any{}
	names := map[string]bool{}
	for _, t := range req.Tools {
		if names[t.Name] {
			return errors.New("duplicate ARTEX tool name")
		}
		names[t.Name] = true
		tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
	}
	config := map[string]any{
		"features.shell_tool":                false,
		"features.unified_exec":              false,
		"features.code_mode":                 false,
		"features.code_mode_host":            false,
		"features.multi_agent":               false,
		"agents.enabled":                     false,
		"features.apps":                      false,
		"features.browser_use":               false,
		"features.computer_use":              false,
		"features.image_generation":          false,
		"features.view_image":                false,
		"features.memories":                  false,
		"features.skill_search":              false,
		"features.skip_host_skill_discovery": true,
		"features.hooks":                     false,
		"features.sleep_tool":                false,
		"project_doc_max_bytes":              0,
		"web_search":                         "disabled",
		"tools.update_plan":                  map[string]any{"enabled": false},
	}

	if s.Provider == nil {
		s.Provider = &providerState{}
	}
	state := s.Provider
	if state.cancelWatch != nil {
		state.cancelWatch()
		state.cancelWatch = nil
	}
	toolSignature, _ := json.Marshal(tools)
	previous, _ := codexHistory(state.History)
	compatible := len(req.Messages) >= len(state.History) && len(items) >= len(previous) && sameItems(previous, items[:min(len(items), len(previous))])
	rebuild := state.Dirty || s.ThreadID == "" || state.Tools != string(toolSignature) || !compatible || (reconnected && state.Awaiting)
	if rebuild {
		if s.ThreadID != "" && !reconnected {
			if _, err = proc.rpc(ctx, "thread/archive", map[string]any{"threadId": s.ThreadID}); err != nil {
				return err
			}
		}
		raw, e := proc.rpc(ctx, "thread/start", map[string]any{"cwd": filepath.Join(s.dir, "work"), "model": p.Model, "baseInstructions": strings.Join(req.System, "\n\n"), "dynamicTools": tools, "sandbox": "read-only", "approvalPolicy": "untrusted", "config": config})
		if e != nil {
			return e
		}
		var result struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Thread.ID == "" {
			return errors.New("Codex thread missing")
		}
		s.ThreadID = result.Thread.ID
		state = &providerState{Tools: string(toolSignature), System: strings.Join(req.System, "\n\n")}
		s.Provider = state
		previous = nil
	} else if reconnected || state.System != strings.Join(req.System, "\n\n") {
		if _, err = proc.rpc(ctx, "thread/resume", map[string]any{"threadId": s.ThreadID, "baseInstructions": strings.Join(req.System, "\n\n"), "model": p.Model, "sandbox": "read-only", "approvalPolicy": "untrusted", "config": config}); err != nil {
			return err
		}
		state.System = strings.Join(req.System, "\n\n")
	}
	state.Dirty = true
	if err = s.save(); err != nil {
		return err
	}
	if state.Pending != nil {
		var call struct {
			CallID string `json:"callId"`
		}
		_ = json.Unmarshal(state.Pending.Params, &call)
		var result *llm.ContentBlock
		for _, msg := range req.Messages[len(state.History):] {
			for _, block := range msg.Content {
				if block.Type == llm.BlockToolResult && block.ToolUseID == call.CallID {
					copy := block
					result = &copy
				}
			}
		}
		if result == nil {
			return errors.New("ARTEX tool result missing for pending Codex call")
		}
		content := []any{}
		for _, block := range result.Content {
			if block.Type != llm.BlockText {
				return errors.New("Codex tool results require text")
			}
			content = append(content, map[string]string{"type": "inputText", "text": block.Text})
		}

		// Additional host instructions (e.g. skill disclosure) remain structured input.
		extra := []llm.Message{}
		for _, msg := range req.Messages[len(state.History):] {
			copy := llm.Message{Role: msg.Role}
			for _, block := range msg.Content {
				if block.Type != llm.BlockToolResult || block.ToolUseID != call.CallID {
					copy.Content = append(copy.Content, block)
				}
			}
			if len(copy.Content) > 0 {
				extra = append(extra, copy)
			}
		}
		extraItems, e := codexHistory(extra)
		if e != nil {
			return e
		}
		if len(extraItems) > 0 {
			if _, err = proc.rpc(ctx, "thread/inject_items", map[string]any{"threadId": s.ThreadID, "items": extraItems}); err != nil {
				return err
			}
		}
		// A server request stays pending while ARTEX alone checks and executes the tool.
		if err = proc.send(map[string]any{"id": state.Pending.ID, "result": map[string]any{"success": !result.IsError, "contentItems": content}}); err != nil {
			return err
		}
		state.Pending = nil
		state.Awaiting = false
	} else {
		delta := items[len(previous):]
		if len(delta) == 0 {
			return errors.New("Codex request contains no new host messages")
		}
		if _, err = proc.rpc(ctx, "thread/inject_items", map[string]any{"threadId": s.ThreadID, "items": delta}); err != nil {
			return err
		}
		turnParams := map[string]any{"threadId": s.ThreadID, "input": []any{}}
		if p.Effort != "" {
			turnParams["effort"] = p.Effort
		}
		if _, err = proc.rpc(ctx, "turn/start", turnParams); err != nil {
			return err
		}
	}
	checkpoint, e := json.Marshal(req.Messages)
	if e != nil {
		return e
	}
	if err = json.Unmarshal(checkpoint, &state.History); err != nil {
		return err
	}
	if err = s.save(); err != nil {
		return err
	}
	assistant := llm.Message{Role: llm.RoleAssistant}
	finish := func() error { state.Dirty = false; state.History = append(state.History, assistant); return s.save() }

	if err = emit(llm.StreamEvent{Type: llm.SEMessageStart}); err != nil {
		return err
	}
	var usage llm.Usage
	seen := false
	for {
		raw, e := proc.receive(ctx)
		if e != nil {
			err = e
			return err
		}
		var w wire
		if json.Unmarshal(raw, &w) != nil {
			return errors.New("invalid Codex event")
		}
		if len(w.ID) > 0 && w.Method != "" {
			if w.Method != "item/tool/call" {
				proc.decline(w)
				return fmt.Errorf("Codex requested unsupported native action %s", w.Method)
			}
			var call struct {
				ThreadID  string          `json:"threadId"`
				CallID    string          `json:"callId"`
				Tool      string          `json:"tool"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(w.Params, &call) != nil || call.ThreadID == "" {
				return errors.New("invalid Codex dynamic tool request")
			}
			if call.ThreadID != s.ThreadID {
				continue
			} // Cancelled thread requests can already be queued.
			if call.CallID == "" || !names[call.Tool] || !json.Valid(call.Arguments) {
				return errors.New("invalid or unavailable Codex dynamic tool")
			}

			state.Pending = &w
			state.Awaiting = true
			state.cancelWatch = context.AfterFunc(runCtx, proc.close)
			assistant.Content = append(assistant.Content, llm.ContentBlock{Type: llm.BlockToolUse, ID: call.CallID, Name: call.Tool, Input: call.Arguments})
			if err = finish(); err != nil {
				return err
			}

			for _, ev := range []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: call.CallID, ToolName: call.Tool}, {Type: llm.SEToolInputJSON, Text: string(call.Arguments)}, {Type: llm.SEMessageDelta, StopReason: "tool_use", Usage: usage}, {Type: llm.SEMessageStop}} {
				if err = emit(ev); err != nil {
					return err
				}
			}
			return nil
		}
		var v struct {
			ThreadID string `json:"threadId"`
			Delta    string `json:"delta"`
			Turn     struct {
				Status string          `json:"status"`
				Error  json.RawMessage `json:"error"`
			} `json:"turn"`
			TokenUsage struct {
				Last struct {
					Input  int `json:"inputTokens"`
					Output int `json:"outputTokens"`
					Cached int `json:"cachedInputTokens"`
				} `json:"last"`
			} `json:"tokenUsage"`
		}
		_ = json.Unmarshal(w.Params, &v)
		if v.ThreadID != "" && v.ThreadID != s.ThreadID {
			continue
		}
		switch w.Method {
		case "item/agentMessage/delta":
			seen = true
			if len(assistant.Content) == 0 {
				assistant.Content = append(assistant.Content, llm.TextBlock(""))
			}
			assistant.Content[len(assistant.Content)-1].Text += v.Delta
			if err = emit(llm.StreamEvent{Type: llm.SETextDelta, Text: v.Delta}); err != nil {
				return err
			}
		case "thread/tokenUsage/updated":
			usage.Add(llm.Usage{InputTokens: v.TokenUsage.Last.Input, OutputTokens: v.TokenUsage.Last.Output, CacheReadTokens: v.TokenUsage.Last.Cached})
		case "error":
			return cliError("Codex", w.Params)
		case "turn/completed":
			if v.Turn.Status != "completed" {
				return cliError("Codex", v.Turn.Error)
			}
			if !seen {
				return errors.New("Codex returned no text or tool calls")
			}
			if err = emit(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: usage}); err != nil {
				return err
			}
			if err = finish(); err != nil {
				return err
			}
			return emit(llm.StreamEvent{Type: llm.SEMessageStop})
		}
	}
}
