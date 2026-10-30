package externalcli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func TestProviderPersistentThreadAndRewrites(t *testing.T) {
	m := fixtureManager(t)
	script := strings.Replace(fixture, " if method=='turn/start':", ` if method=='thread/inject_items':
  with open('injections.jsonl','a') as f:f.write(json.dumps(params['items'])+'\n')
  with open('input.txt','w') as f:f.write(params['items'][-1]['content'][0]['text'])
 if method=='turn/start':`, 1)
	script = strings.Replace(script, "prompt=params['input'][0]['text'];out=reply(prompt)", "prompt=params['input'][0]['text'] if params['input'] else open('input.txt').read();out=reply(prompt)", 1)
	if err := os.WriteFile(filepath.Join(m.root, "codex/bin/codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	p := &Provider{Manager: m, Model: "fixture-model", ProfileID: 77}
	ctx := transcript.WithSessionID(t.Context(), "persistent")
	req := llm.CompletionRequest{System: []string{"system"}, Messages: []llm.Message{llm.UserText("first")}}
	first := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("first")}}
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == llm.SEMessageStop {
			break
		}
	}

	key := func(manager *Manager) *session {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		for _, s := range manager.sessions {
			return s
		}
		t.Fatal("missing session")
		return nil
	}
	s := key(m)
	id := s.ThreadID
	pid := s.p.cmd.Process.Pid
	work := filepath.Join(s.dir, "work")
	req.Messages = append(req.Messages, first, llm.UserText("second"))
	req.System = []string{"updated system"}
	second, _, _, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if s.ThreadID != id || s.p.cmd.Process.Pid != pid {
		t.Fatal("normal turn or system update replaced thread/process")
	}
	root := m.root
	m.Close()
	m2 := New(t.Context(), root)
	defer m2.Close()
	p.Manager = m2
	req.Messages = append(req.Messages, second, llm.UserText("third"))
	_, _, _, err = p.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if key(m2).ThreadID != id {
		t.Fatal("restart replaced completed thread")
	}
	raw, err := os.ReadFile(filepath.Join(work, "injections.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var items []any
		if json.Unmarshal([]byte(line), &items) != nil || len(items) != 1 {
			t.Fatalf("old history resent: %s", line)
		}
	}
	req.Messages = []llm.Message{llm.UserText("compacted")}
	compacted, _, _, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	s = key(m2)
	if s.ThreadID == id {
		t.Fatal("rewritten history retained stale context")
	}
	id = s.ThreadID
	req.Messages = append(req.Messages, compacted, llm.UserText("new tools"))
	req.Tools = []llm.ToolSchema{{Name: "echo", InputSchema: map[string]any{"type": "object"}}}
	if _, _, _, err = p.Complete(ctx, req); err != nil {
		t.Fatal(err)
	}
	if s.ThreadID == id {
		t.Fatal("changed tool schema retained stale tools")
	}
}

func TestPendingToolCancellation(t *testing.T) { testPendingCancellation(t, Codex, "gpt-6.1-sol") }
func TestAGYPendingToolCancellation(t *testing.T) {
	testPendingCancellation(t, Agy, "gemini-3.8-flash-high")
}
func testPendingCancellation(t *testing.T, kind, model string) {
	if os.Getenv("ARTEX_CLI_GENERATION") != "1" {
		t.Skip("live generation disabled")
	}
	home, _ := os.UserHomeDir()
	m := New(t.Context(), filepath.Join(home, "artex"))
	defer m.Close()
	ctx, cancel := context.WithCancel(transcript.WithSessionID(t.Context(), "pending-stop-"+transcript.NewSessionID()))
	defer cancel()
	p := &Provider{Kind: kind, Manager: m, Model: model, ProfileID: 999993}
	req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("Call artex_echo once.")}, Tools: []llm.ToolSchema{{Name: "artex_echo", Description: "Echo", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}}}
	msg, reason, _, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "tool_use" || len(msg.ToolUses()) != 1 {
		t.Fatal("native tool call missing")
	}
	cancel()
	m.mu.Lock()
	var done <-chan struct{}
	for _, s := range m.sessions {
		done = s.p.done
	}
	m.mu.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pending CLI process was not cancelled")
	}
}

func TestRPCPreservesDynamicToolRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wire.ndjson")
	output, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	proc := &process{in: output, events: make(chan json.RawMessage, 2), holdTools: true}
	proc.events <- json.RawMessage(`{"id":9001,"method":"item/tool/call","params":{"threadId":"thread-a","callId":"call-a"}}`)
	proc.events <- json.RawMessage(`{"id":1,"result":{}}`)
	if _, err := proc.rpc(t.Context(), "thread/resume", map[string]string{"threadId": "thread-a"}); err != nil {
		t.Fatal(err)
	}
	if len(proc.pending) != 1 {
		t.Fatal("dynamic call was lost while waiting for RPC response")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(strings.TrimSpace(string(raw)), "\n")) != 1 {
		t.Fatal("ARTEX tool request was automatically answered")
	}
}
