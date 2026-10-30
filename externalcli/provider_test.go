package externalcli

import (
	"context"
	"errors"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveProviderToolCycle(t *testing.T) {
	if os.Getenv("ARTEX_CLI_GENERATION") != "1" {
		t.Skip("live CLI generation disabled")
	}
	home, _ := os.UserHomeDir()
	m := New(context.Background(), filepath.Join(home, "artex"))
	defer m.Close()
	ctx, cancel := context.WithTimeout(transcript.WithSessionID(context.Background(), "provider-test-"+transcript.NewSessionID()), 90*time.Second)
	defer cancel()
	p := &Provider{Manager: m, Model: "gpt-6.1-sol", ProfileID: 999990}
	req := llm.CompletionRequest{System: []string{"Use only ARTEX tools. First call artex_echo with value hello, then report the exact tool result. Never invent a result."}, Messages: []llm.Message{llm.UserText("Call artex_echo now and report its result.")}, Tools: []llm.ToolSchema{{Name: "artex_echo", Description: "Echo a value through ARTEX", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]string{"type": "string"}}, "required": []string{"value"}, "additionalProperties": false}}}}
	msg, reason, _, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "tool_use" || len(msg.ToolUses()) != 1 {
		t.Fatalf("expected native tool call: %s %+v", reason, msg)
	}

	var originalPID int
	var originalThread string
	m.mu.Lock()
	for _, s := range m.sessions {
		if s.p != nil {
			originalPID = s.p.cmd.Process.Pid
			originalThread = s.ThreadID
		}
	}
	m.mu.Unlock()
	call := msg.ToolUses()[0]
	req.Messages = append(req.Messages, msg, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(call.ID, "ARTEX_RESULT_74291", false)}})
	reply, reason, _, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	for _, s := range m.sessions {
		if s.p == nil || s.p.cmd.Process.Pid != originalPID || s.ThreadID != originalThread {
			t.Error("app-server was not reused")
		}
	}
	m.mu.Unlock()
	if reason != "end_turn" || !strings.Contains(reply.Text(), "ARTEX_RESULT_74291") {
		t.Fatalf("tool result lost: %s %s", reason, reply.Text())
	}
	m.Close()
	m2 := New(context.Background(), filepath.Join(home, "artex"))
	defer m2.Close()
	p.Manager = m2
	req.Messages = append(req.Messages, reply, llm.UserText("What exact result did the tool return earlier? Do not call a tool again."))
	resumed, _, _, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	m2.mu.Lock()
	for _, s := range m2.sessions {
		if s.ThreadID != originalThread {
			t.Error("thread changed after restart")
		}
	}
	m2.mu.Unlock()
	if !strings.Contains(resumed.Text(), "ARTEX_RESULT_74291") {
		t.Fatal("history lost after restart")
	}

}

func TestLiveProviderCancellation(t *testing.T) {
	if os.Getenv("ARTEX_CLI_GENERATION") != "1" {
		t.Skip("live generation disabled")
	}
	home, _ := os.UserHomeDir()
	m := New(t.Context(), filepath.Join(home, "artex"))
	defer m.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	p := &Provider{Manager: m, Model: "gpt-6.1-sol", ProfileID: 999992}
	first := false
	completed := false
	var failure error
	for ev, err := range p.Stream(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("Print numbers 1 through 1000, one per line. Do not use tools.")}}) {
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
		t.Fatalf("stream/cancellation failed: first=%v completed=%v error=%v", first, completed, failure)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(m.processSlots) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(m.processSlots) != 0 {
		t.Fatal("cancelled process was not cleaned up")
	}
}
