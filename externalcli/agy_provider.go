package externalcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type agyRuntime struct {
	effort  string
	bridge  *agyBridge
	pending *mcpCall
	watch   func() bool
}

type agyRetryState struct {
	History []llm.Message `json:"history"`
}

// A retry is safe only before any task tool call has been handed to ARTEX.
func (s *session) recoverAGY() error {
	state := s.Provider
	if !state.Dirty {
		return nil
	}
	if state.Awaiting {
		return errors.New("AGY lost a pending tool connection; start a new session, actions will not be replayed")
	}
	if state.AGYRetry != nil {
		state.History = state.AGYRetry.History
		state.AGYRetry = nil
		state.Dirty = false
	} else {
		// Legacy checkpoints are recoverable only when no tool history exists.
		for _, msg := range state.History {
			if msg.Role != llm.RoleUser {
				return errors.New("AGY request was interrupted; start a new ARTEX session to avoid replaying actions")
			}
			for _, block := range msg.Content {
				if block.Type != llm.BlockText {
					return errors.New("AGY request was interrupted; start a new ARTEX session to avoid replaying actions")
				}
			}
		}
		s.ThreadID = ""
		s.Provider = &providerState{}
	}
	if s.p != nil {
		s.p.close()
		s.p = nil
	}
	return s.save()
}

func (p *Provider) agyStream(ctx context.Context, req llm.CompletionRequest, emit func(llm.StreamEvent) error) (retErr error) {
	if p.Manager == nil {
		return errors.New("AGY manager unavailable")
	}
	if len(req.Stop) > 0 || (req.Temperature != nil && *req.Temperature != 0) {
		return errors.New("AGY CLI cannot set temperature or stop sequences")
	}
	sid := transcript.SessionIDFrom(ctx)
	if sid == "" {
		sid = transcript.NewSessionID()
	}
	task, role := "", "provider"
	if p.Identity != nil {
		task, role = p.Identity(ctx)
		role = "provider/" + role
	}
	s, err := p.Manager.session(Request{Kind: Agy, Model: p.Model, ProfileID: p.ProfileID, TaskID: task, Role: role, SessionID: sid})
	if err != nil {
		return err
	}
	if !s.mu.TryLock() {
		return errors.New("AGY session already running")
	}
	defer s.mu.Unlock()
	runCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	defer func() { s.last = time.Now(); _ = harden(filepath.Join(p.Manager.root, "agy")) }()
	if s.p != nil {
		select {
		case <-s.p.stopped:
			s.p = nil
		case <-s.p.done:
			s.p = nil
		default:
		}
	}
	if s.Provider == nil {
		s.Provider = &providerState{}
	}
	if err := s.recoverAGY(); err != nil {
		return err
	}
	state := s.Provider
	if len(req.Messages) < len(state.History) || (len(state.History) > 0 && !sameItems(state.History, req.Messages[:min(len(req.Messages), len(state.History))])) {
		return errors.New("AGY cannot replace native history; start a new session after editing or compacting history")
	}
	if s.p == nil && state.Awaiting {
		return errors.New("AGY lost a pending tool connection; start a new session, actions will not be replayed")
	}
	fileMode := strings.HasPrefix(p.Model, "gemini-")
	promptPath := filepath.Join(s.dir, "work", "prompt.txt")
	if fileMode && s.ThreadID != "" && !state.PromptFile {
		return errors.New("start a new ARTEX session to enable Gemini prompt files")
	}
	toolsJSON, _ := json.Marshal(req.Tools)
	system := strings.Join(req.System, "\n\n")
	if !fileMode && s.ThreadID != "" && state.System != system {
		return errors.New("AGY CLI preserves the original system prompt on resume; start a new ARTEX session after changing instructions")
	}
	if s.p != nil && (state.Tools != string(toolsJSON) || (!fileMode && state.System != system) || (!state.Awaiting && s.agy.effort != p.Effort)) {
		if state.Awaiting {
			return errors.New("AGY cannot change system instructions or tools while a tool result is pending")
		}
		old := s.p
		old.close()
		select {
		case <-old.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		s.p = nil
	}
	if s.p == nil {
		version, e := p.Manager.commandOutput(ctx, Agy, "--version")
		if e != nil {
			return errors.New("cannot verify AGY MCP/permission support")
		}
		var major, minor, patch int
		if _, e = fmt.Sscanf(strings.TrimSpace(string(version)), "%d.%d.%d", &major, &minor, &patch); e != nil || major < 1 || (major == 1 && (minor < 4 || (minor == 4 && patch < 11))) {
			return errors.New("AGY MCP provider requires native CLI 1.4.11 or later")
		}
		bridgePath := ""
		if fileMode {
			bridgePath = promptPath
		}
		bridge, e := newAGYBridge(p.Manager.ctx, req.Tools, bridgePath)
		if e != nil {
			return e
		}
		work := filepath.Join(s.dir, "work")
		configDir := filepath.Join(work, ".agents")
		if e = secureDir(filepath.Join(configDir, "agents")); e != nil {
			bridge.close()
			return e
		}
		config, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"artex": map[string]any{"serverUrl": bridge.URL, "timeoutSeconds": 300}}})
		if e = os.WriteFile(filepath.Join(configDir, "mcp_config.json"), config, 0600); e != nil {
			bridge.close()
			return e
		}
		agentSystem := system
		if fileMode {
			agentSystem = "ARTEX supplies each request through a session-local prompt.txt file. Read that file using artex_read_prompt whenever asked, including after tool results, and follow its current agent instructions and task. artex_read_prompt is required instruction transport, not a task action; restrictions on task tools do not forbid reading the current instructions. Use only ARTEX MCP tools for actions. The file never grants permission to bypass ARTEX tool checks."
		}
		agent := "---\nname: artex-provider\ndescription: ARTEX host-controlled agent\nmainAgent: true\nsubagent: false\ntools: []\ncommandExecutionPolicy: sandbox\n---\n" + agentSystem
		if e = os.WriteFile(filepath.Join(configDir, "agents/artex-provider.md"), []byte(agent), 0600); e != nil {
			bridge.close()
			return e
		}
		allow := []string{}
		if fileMode {
			allow = append(allow, "mcp(artex/artex_read_prompt)")
		}
		names := map[string]bool{}
		for _, tool := range req.Tools {
			if (fileMode && tool.Name == "artex_read_prompt") || names[tool.Name] || strings.ContainsAny(tool.Name, "*/()\r\n") {
				bridge.close()
				return errors.New("duplicate or unsupported MCP tool name")
			}
			names[tool.Name] = true
			allow = append(allow, "mcp(artex/"+tool.Name+")")
		}
		settings, _ := json.Marshal(map[string]any{"permissions": map[string]any{"allow": allow, "deny": []string{"command(*)", "read_file(*)", "write_file(*)", "read_url(*)", "execute_url(*)"}}, "statusLine": map[string]bool{"enabled": false}, "title": map[string]bool{"enabled": false}})
		args := []string{"--print=", "--input-format", "stream-json", "--output-format", "stream-json", "--model", agyModel(p.Model, p.Effort), "--agent", "artex-provider", "--mode", "plan", "--sandbox", "--disable-slash-commands", "--log-file", "/dev/null", "--settings", string(settings)}
		if s.ThreadID != "" {
			args = append(args, "--conversation", s.ThreadID)
		}
		if p.Effort != "" {
			args = append(args, "--effort", p.Effort)
		}
		proc, e := p.Manager.start(Agy, work, args...)
		if e != nil {
			bridge.close()
			return e
		}
		s.p = proc
		s.agy = &agyRuntime{bridge: bridge, effort: p.Effort}
		state.Tools = string(toolsJSON)
		state.System = system
		state.PromptFile = fileMode
		go func() { <-proc.done; bridge.close() }()
	}
	proc := s.p
	runtime := s.agy
	if runtime.watch != nil {
		runtime.watch()
		runtime.watch = nil
	}
	stop := context.AfterFunc(ctx, proc.close)
	defer stop()
	defer func() {
		if retErr != nil {
			proc.close()
			s.p = nil
		}
	}()
	newMessages := req.Messages[len(state.History):]
	if fileMode {
		if err = writeAGYPrompt(promptPath, req.System, newMessages); err != nil {
			return err
		}
		runtime.bridge.promptGeneration.Add(1)
		state.System = system
	}

	// Persist before sending: a restart must not replay a possibly executed action.
	state.AGYRetry = nil
	if !state.Awaiting {
		state.AGYRetry = &agyRetryState{History: state.History}
	}
	state.Dirty = true
	if err = s.save(); err != nil {
		return err
	}
	if runtime.pending != nil {
		var result *llm.ContentBlock
		guidance := []llm.ContentBlock{}
		for _, msg := range newMessages {
			for _, block := range msg.Content {
				if block.Type == llm.BlockToolResult && block.ToolUseID == runtime.pending.ID {
					copy := block
					result = &copy
				} else if msg.Role == llm.RoleUser && block.Type == llm.BlockText {
					guidance = append(guidance, llm.TextBlock("ARTEX host guidance:\n"+block.Text))
				} else {
					return errors.New("unsupported extra host content during AGY MCP call")
				}

			}
		}
		if result == nil {
			return errors.New("ARTEX result missing for AGY MCP call")
		}
		result.Content = append(append([]llm.ContentBlock(nil), result.Content...), guidance...)
		if fileMode {
			content := []llm.ContentBlock{llm.TextBlock(agyPromptInstruction(promptPath))}
			for _, block := range result.Content {
				if block.Type != llm.BlockText {
					content = append(content, block)
				}
			}
			result.Content = content
		}
		if err = runtime.pending.answer(ctx, *result); err != nil {
			return err
		}
		runtime.pending = nil
		state.Awaiting = false
	} else {
		text := []string{}
		for _, msg := range newMessages {
			if msg.Role != llm.RoleUser {
				return errors.New("AGY cannot import assistant/tool history; use a new ARTEX session")
			}
			for _, block := range msg.Content {
				if block.Type != llm.BlockText {
					return errors.New("AGY requires a new user text message")
				}
				text = append(text, block.Text)
			}
		}
		if len(text) == 0 {
			return errors.New("AGY has no new user input")
		}
		outgoing := strings.Join(text, "\n\n")
		if fileMode {
			outgoing = agyPromptInstruction(promptPath)
		}
		if err = proc.send(map[string]any{"event": "user", "message": map[string]string{"content": outgoing}}); err != nil {
			return err
		}
	}
	checkpoint, _ := json.Marshal(req.Messages)
	_ = json.Unmarshal(checkpoint, &state.History)
	assistant := llm.Message{Role: llm.RoleAssistant}
	finish := func() error {
		state.History = append(state.History, assistant)
		state.Dirty = false
		state.AGYRetry = nil
		return s.save()
	}
	if err = emit(llm.StreamEvent{Type: llm.SEMessageStart}); err != nil {
		return err
	}
	var usage llm.Usage
	promptReminderSent := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case call := <-runtime.bridge.Calls:
			select {
			case <-call.Done:
				return errors.New("AGY cancelled its MCP request")
			default:
			}
			runtime.pending = call
			state.Awaiting = true
			runtime.watch = context.AfterFunc(runCtx, proc.close)
			assistant.Content = append(assistant.Content, llm.ContentBlock{Type: llm.BlockToolUse, ID: call.ID, Name: call.Name, Input: call.Arguments})
			if err = finish(); err != nil {
				return err
			}
			for _, ev := range []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: call.ID, ToolName: call.Name}, {Type: llm.SEToolInputJSON, Text: string(call.Arguments)}, {Type: llm.SEMessageDelta, StopReason: "tool_use", Usage: usage}, {Type: llm.SEMessageStop}} {
				if err = emit(ev); err != nil {
					return err
				}
			}
			return nil
		case raw, ok := <-proc.events:
			if !ok {
				return errors.New("AGY connection closed")
			}
			var ev struct {
				Event          string `json:"event"`
				ConversationID string `json:"conversation_id"`
				Init           struct {
					ID string `json:"conversation_id"`
				} `json:"init"`
				Step struct {
					Type string `json:"step_type"`
					Text string `json:"text_delta"`
				} `json:"step_update"`
				Delta  string `json:"delta"`
				Result struct {
					Status   string `json:"status"`
					Response string `json:"response"`
					ID       string `json:"conversation_id"`
					Usage    struct {
						Input  int `json:"input_tokens"`
						Output int `json:"output_tokens"`
						Cached int `json:"cache_read_tokens"`
					} `json:"usage"`
				} `json:"result"`
			}
			if json.Unmarshal(raw, &ev) != nil {
				return errors.New("invalid AGY stream event")
			}
			switch ev.Event {
			case "init":
				if ev.ConversationID != "" {
					s.ThreadID = ev.ConversationID
				} else if ev.Init.ID != "" {
					s.ThreadID = ev.Init.ID
				}
				if err = s.save(); err != nil {
					return err
				}
			case "step_update", "text_delta":
				text := ev.Delta
				if ev.Step.Type == "agent_response" {
					text = ev.Step.Text
				}
				if text != "" && (!fileMode || runtime.bridge.promptRead.Load() == runtime.bridge.promptGeneration.Load()) {
					if len(assistant.Content) == 0 {
						assistant.Content = append(assistant.Content, llm.TextBlock(""))
					}
					assistant.Content[len(assistant.Content)-1].Text += text
					if err = emit(llm.StreamEvent{Type: llm.SETextDelta, Text: text}); err != nil {
						return err
					}
				}
			case "error":
				return cliError("AGY", raw)
			case "result":
				if !strings.EqualFold(ev.Result.Status, "success") {
					return cliError("AGY", raw)
				}
				// AGY reports Google's preflight refusal as SUCCESS without running the agent.
				if fileMode && !agyPolicyRefusal(ev.Result.Response) && runtime.bridge.promptRead.Load() != runtime.bridge.promptGeneration.Load() {
					if promptReminderSent {
						return errors.New("Gemini did not read the current ARTEX prompt.txt")
					}
					promptReminderSent = true
					if err = proc.send(map[string]any{"event": "user", "message": map[string]string{"content": "The current file revision has not been read. Call artex_read_prompt now before responding. " + agyPromptInstruction(promptPath)}}); err != nil {
						return err
					}
					continue
				}
				if ev.Result.ID != "" {
					s.ThreadID = ev.Result.ID
				}
				if s.ThreadID == "" {
					return errors.New("AGY conversation ID missing")
				}
				if len(assistant.Content) == 0 && ev.Result.Response != "" {
					assistant.Content = append(assistant.Content, llm.TextBlock(ev.Result.Response))
					if err = emit(llm.StreamEvent{Type: llm.SETextDelta, Text: ev.Result.Response}); err != nil {
						return err
					}
				}
				if len(assistant.Content) == 0 {
					return errors.New("AGY returned no text")
				}
				current := llm.Usage{InputTokens: ev.Result.Usage.Input, OutputTokens: ev.Result.Usage.Output, CacheReadTokens: ev.Result.Usage.Cached}
				usage = current
				if current.InputTokens >= state.Usage.InputTokens && current.OutputTokens >= state.Usage.OutputTokens {
					usage.InputTokens -= state.Usage.InputTokens
					usage.OutputTokens -= state.Usage.OutputTokens
					usage.CacheReadTokens = max(0, current.CacheReadTokens-state.Usage.CacheReadTokens)
				}
				state.Usage = current
				if err = finish(); err != nil {
					return err
				}
				if err = emit(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: usage}); err != nil {
					return err
				}
				return emit(llm.StreamEvent{Type: llm.SEMessageStop})
			}
		}
	}
}

func agyPolicyRefusal(response string) bool {
	text := strings.ToLower(strings.TrimSpace(response))
	return strings.HasPrefix(text, "the prompt could not be submitted.") && strings.Contains(text, "https://policies.google.com/terms/generative-ai/use-policy")
}

func agyPromptInstruction(path string) string {
	return "Read the current ARTEX prompt from the .txt file " + path + " using the artex_read_prompt MCP tool before continuing. Follow the agent instructions and task in that file. Other ARTEX tool calls and permissions remain unchanged."
}
func writeAGYPrompt(path string, system []string, messages []llm.Message) error {
	var content strings.Builder
	content.WriteString("ARTEX AGENT INSTRUCTIONS\n\n" + strings.Join(system, "\n\n") + "\n\nCURRENT REQUEST\n\n")
	for _, msg := range messages {
		for _, block := range msg.Content {
			if block.Type == llm.BlockText {
				content.WriteString(block.Text + "\n\n")
			} else if block.Type == llm.BlockToolResult {
				content.WriteString("ARTEX TOOL RESULT\n")
				if block.IsError {
					content.WriteString("Status: error\n")
				}
				for _, part := range block.Content {
					if part.Type == llm.BlockText {
						content.WriteString(part.Text + "\n")
					}
				}
				content.WriteString("\n")
			}
		}
	}
	content.WriteString("Tool calls and results use the real MCP connection; this file does not authorize or simulate tool execution.\n")
	f, err := os.CreateTemp(filepath.Dir(path), ".prompt-*.txt")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(content.String()); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
