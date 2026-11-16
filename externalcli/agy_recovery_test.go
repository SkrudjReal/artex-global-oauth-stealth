package externalcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func TestAGYModelEffortOverride(t *testing.T) {
	for _, tc := range []struct{ model, effort, want string }{
		{"gemini-3.8-flash-high", "", "gemini-3.8-flash-high"},
		{"gemini-3.8-flash-high", "low", "gemini-3.8-flash"},
		{"gemini-3.8-flash-low", "high", "gemini-3.8-flash"},
		{"gpt-oss-120b-medium", "medium", "gpt-oss-120b"},
		{"claude-opus-4-6-thinking", "high", "claude-opus-4-6-thinking"},
	} {
		if got := agyModel(tc.model, tc.effort); got != tc.want {
			t.Errorf("%s effort %q: got %s, want %s", tc.model, tc.effort, got, tc.want)
		}
	}
}

func TestExternalCLIEffort(t *testing.T) {
	for _, kind := range []string{Agy, Codex} {
		t.Run(kind, func(t *testing.T) {
			m := fixtureManager(t)
			script := strings.Replace(fixture, "args=sys.argv[1:]", "args=sys.argv[1:]\nwith open('effort.txt','w') as f:f.write(args[args.index('--effort')+1] if '--effort' in args else '')", 1)
			script = strings.Replace(script, " if method=='turn/start':", " if method=='turn/start':\n  with open('effort.txt','w') as f:f.write(params.get('effort',''))", 1)
			if err := os.WriteFile(filepath.Join(m.root, strings.TrimSuffix(kind, "-oauth"), "bin", strings.TrimSuffix(kind, "-oauth")), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			req := Request{Kind: kind, Model: "fixture-model", Effort: "low", Role: "external", SessionID: "effort", Prompt: "hello"}
			for _, effort := range []string{"low", "high", ""} {
				req.Effort = effort
				if _, err := runText(t.Context(), m, req); err != nil {
					t.Fatal(err)
				}
				s, _ := m.session(req)
				raw, err := os.ReadFile(filepath.Join(s.dir, "work/effort.txt"))
				if err != nil || string(raw) != effort {
					t.Fatalf("effort %q not applied: %v %q", effort, err, raw)
				}
			}
		})
	}
}

func TestAGYRecoveryBoundaries(t *testing.T) {
	user := []llm.Message{llm.UserText("request")}
	tool := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "call", Name: "action"}}}
	for _, tc := range []struct {
		name  string
		state providerState
		ok    bool
	}{
		{"legacy before tools", providerState{Dirty: true, History: user}, true},
		{"legacy before send", providerState{Dirty: true}, true},
		{"legacy with tools", providerState{Dirty: true, History: append(append([]llm.Message{}, user...), tool)}, false},
		{"lost pending call", providerState{Dirty: true, Awaiting: true, AGYRetry: &agyRetryState{}}, false},
		{"safe checkpoint after prior tools", providerState{Dirty: true, History: user, AGYRetry: &agyRetryState{History: []llm.Message{tool}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &session{ThreadID: "old", Provider: &tc.state, dir: t.TempDir()}
			checkpoint := tc.state.AGYRetry
			err := s.recoverAGY()
			if (err == nil) != tc.ok {
				t.Fatalf("recovery error: %v", err)
			}
			if !tc.ok {
				if !s.Provider.Dirty || s.ThreadID != "old" {
					t.Fatal("unsafe session was modified")
				}
				return
			}
			if s.Provider.Dirty || s.Provider.AGYRetry != nil {
				t.Fatal("recovery did not clear checkpoint")
			}
			if checkpoint != nil && (s.ThreadID != "old" || !sameItems(s.Provider.History, checkpoint.History)) {
				t.Fatal("retry lost completed native history")
			}
		})
	}
}

func TestAGYRetryAfterTransportErrorAndRestart(t *testing.T) {
	m := fixtureManager(t)
	script := strings.Replace(fixture, "args=sys.argv[1:]", `args=sys.argv[1:]
if args==['--version']:print('1.4.12');sys.exit(0)
with open('effort.txt','w') as f:f.write(args[args.index('--effort')+1] if '--effort' in args else '')`, 1)
	script = strings.Replace(script, "  prompt=msg['message']['content'];out=reply(prompt)", `  prompt=msg['message']['content'];out=reply(prompt)
  if not os.path.exists('failed-once'):
   open('failed-once','w').close()
   send({'event':'error','error':'transport unavailable'});continue`, 1)
	if err := os.WriteFile(filepath.Join(m.root, "agy/bin/agy"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	p := &Provider{Kind: Agy, Manager: m, Model: "fixture-model", Effort: "low"}
	ctx := transcript.WithSessionID(t.Context(), "retry")
	req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hello")}}
	if _, _, _, err := p.Complete(ctx, req); err == nil {
		t.Fatal("expected injected transport error")
	}
	m.Close()
	p.Manager = New(t.Context(), m.root)
	defer p.Manager.Close()
	reply, _, _, err := p.Complete(ctx, req)
	if err != nil || reply.Text() != "hello" {
		t.Fatalf("saved session retry failed: %v %q", err, reply.Text())
	}
	p.Effort = "high"
	req.Messages = append(req.Messages, reply, llm.UserText("next"))
	if _, _, _, err = p.Complete(ctx, req); err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Manager.sessions {
		raw, err := os.ReadFile(filepath.Join(s.dir, "work/effort.txt"))
		if err != nil || string(raw) != "high" {
			t.Fatalf("effort update not applied: %v %q", err, raw)
		}
	}
}

func TestLiveAGYSafeRetry(t *testing.T) {
	if os.Getenv("ARTEX_CLI_GENERATION") != "1" {
		t.Skip("requires AGY OAuth")
	}
	home, _ := os.UserHomeDir()
	m := New(t.Context(), filepath.Join(home, "artex"))
	defer m.Close()
	ctx, cancel := context.WithTimeout(transcript.WithSessionID(t.Context(), "agy-retry-"+transcript.NewSessionID()), 90*time.Second)
	defer cancel()
	p := &Provider{Kind: Agy, Manager: m, Model: "gemini-3.8-flash-high", ProfileID: 999998, Effort: "low"}
	req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("Reply exactly ARTEX_RETRY_OK. Do not use task tools.")}}
	interrupted, stop := context.WithCancel(ctx)
	var failure error
	for ev, err := range p.Stream(interrupted, req) {
		if err != nil {
			failure = err
			break
		}
		if ev.Type == llm.SETextDelta {
			stop()
		}
	}
	stop()
	if !errors.Is(failure, context.Canceled) {
		t.Fatalf("expected interrupted request: %v", failure)
	}
	reply, _, _, err := p.Complete(ctx, req)
	if err != nil || !strings.Contains(reply.Text(), "ARTEX_RETRY_OK") {
		t.Fatalf("safe retry failed: %v %q", err, reply.Text())
	}
}
