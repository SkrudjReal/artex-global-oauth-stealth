package externalcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A real subprocess fixture exercises pipe streaming and process lifecycle;
// it is not a live OAuth/model test.
const fixture = `#!/usr/bin/python3
import sys,json,os,time,uuid
args=sys.argv[1:]
def send(v):
 print(json.dumps(v),flush=True)
if args[:2]==['login','status']:
 print('Logged in using ChatGPT');sys.exit(0)
if args==['logout']:sys.exit(0)
if args==['models']:
 print('fixture-model\tFixture');sys.exit(0)
agy='--input-format' in args
thread=args[args.index('--conversation')+1] if '--conversation' in args else str(uuid.uuid4())
def path(t):return 'thread-'+t+'.json'
def load(t):
 try:return json.load(open(path(t)))
 except FileNotFoundError:return []
def reply(prompt):
 global thread
 history=load(thread)
 if prompt.startswith('remember:'):history.append(prompt[9:]);out=prompt[9:]
 elif prompt=='recall':out='|'.join(history)
 else:out=prompt
 with open(path(thread),'w') as f:json.dump(history,f)
 return out
if agy:send({'event':'init','conversation_id':thread,'init':{'permission_mode':'request-review'}})
for line in sys.stdin:
 msg=json.loads(line)
 if agy:
  prompt=msg['message']['content'];out=reply(prompt)
  send({'event':'step_update','step_update':{'step_type':'agent_response','text_delta':out}})
  if prompt=='wait':time.sleep(60)
  time.sleep(.04)
  send({'event':'result','result':{'conversation_id':thread,'status':'SUCCESS','response':out,'usage':{'input_tokens':1,'output_tokens':1}}})
  continue
 method=msg.get('method');params=msg.get('params',{});ident=msg.get('id')
 if method=='initialized':continue
 result={}
 if method=='thread/start':thread=str(uuid.uuid4());result={'thread':{'id':thread}}
 if method=='thread/resume':thread=params['threadId'];result={'thread':{'id':thread}}
 if method=='model/list':result={'data':[{'model':'fixture-model'}]}
 if method=='turn/start':
  prompt=params['input'][0]['text'];out=reply(prompt)
  send({'method':'item/agentMessage/delta','params':{'threadId':thread,'delta':out}})
  send({'id':ident,'result':{'turn':{'id':'turn-1'}}})
  if prompt=='wait':time.sleep(60)
  time.sleep(.04)
  send({'method':'turn/completed','params':{'threadId':thread,'turn':{'id':'turn-1','status':'completed'}}})
  continue
 send({'id':ident,'result':result})
`

func fixtureManager(t *testing.T) *Manager {
	t.Helper()
	m := New(t.Context(), t.TempDir())
	t.Cleanup(m.Close)
	for _, name := range []string{"codex", "agy"} {
		dir := filepath.Join(m.root, name, "bin")
		if err := secureDir(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fixture), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return m
}
func runText(ctx context.Context, m *Manager, req Request) (string, error) {
	var text strings.Builder
	err := m.Run(ctx, req, func(e Event) error {
		if e.Type == "text_delta" {
			text.WriteString(e.Text)
		}
		return nil
	})
	return text.String(), err
}
func TestPersistentStreamingIsolationAndRestore(t *testing.T) {
	for _, kind := range []string{Codex, Agy} {
		t.Run(kind, func(t *testing.T) {
			m := fixtureManager(t)
			req := Request{Kind: kind, Model: "fixture-model", ProfileID: 1, TaskID: "12", Role: "external", SessionID: "lane-a", Prompt: "remember:alpha"}
			var wg sync.WaitGroup
			for _, lane := range []string{"a", "b"} {
				wg.Add(1)
				go func(lane string) {
					defer wg.Done()
					r := req
					r.SessionID = "lane-" + lane
					r.Prompt = "remember:" + lane
					text, err := runText(t.Context(), m, r)
					if err != nil || text != lane {
						t.Errorf("parallel lane %s: %q %v", lane, text, err)
					}
				}(lane)
			}
			wg.Wait()
			req.Prompt = "recall"
			text, err := runText(t.Context(), m, req)
			if err != nil || text != "a" {
				t.Fatalf("isolated history: %q %v", text, err)
			}
			s, err := m.session(req)
			if err != nil {
				t.Fatal(err)
			}
			pid := s.p.cmd.Process.Pid
			_, err = runText(t.Context(), m, req)
			if err != nil || s.p.cmd.Process.Pid != pid {
				t.Fatal("CLI was not reused")
			}
			info, err := os.Stat(filepath.Join(s.dir, "session.json"))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("mapping permissions")
			}
			req.Role = "worker"
			if _, err := m.session(req); err == nil {
				t.Fatal("Worker role accepted")
			}
			req.Role = "external"
			root := m.root
			m.Close()
			restored := New(t.Context(), root)
			defer restored.Close()
			text, err = runText(t.Context(), restored, req)
			if err != nil || text != "a" {
				t.Fatalf("restore: %q %v", text, err)
			}
			restored.Close()
		})
	}
}
func TestStreamingCancellationAndBackpressure(t *testing.T) {
	for _, kind := range []string{Codex, Agy} {
		t.Run(kind, func(t *testing.T) {
			m := fixtureManager(t)
			ctx, cancel := context.WithCancel(t.Context())
			req := Request{Kind: kind, Model: "fixture-model", Role: "external", SessionID: "cancel", Prompt: "wait"}
			sawText := false
			err := m.Run(ctx, req, func(e Event) error {
				if e.Type == "text_delta" {
					sawText = true
					cancel()
				}
				return nil
			})
			if !sawText || !errors.Is(err, context.Canceled) {
				t.Fatalf("incremental cancel: %v %v", sawText, err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for len(m.processSlots) > 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if len(m.processSlots) != 0 {
				t.Fatal("cancel left a CLI running")
			}
			req.SessionID = "disconnected-client"
			req.Prompt = "hello"
			broken := errors.New("closed SSE client")
			err = m.Run(t.Context(), req, func(e Event) error {
				if e.Type == "text_delta" {
					return broken
				}
				return nil
			})
			if !errors.Is(err, broken) {
				t.Fatal(err)
			}
		})
	}
}
func TestEnvironmentAndSessionBoundary(t *testing.T) {
	m := fixtureManager(t)
	t.Setenv("OPENAI_API_KEY", "must-not-leak")
	t.Setenv("GEMINI_API_KEY", "must-not-leak")
	t.Setenv("CURL_HOME", "/laboratory")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/keyring")
	env := strings.Join(m.environment(Agy), "\n")
	if strings.Contains(env, "must-not-leak") || strings.Contains(env, "CURL_HOME=") || !strings.Contains(env, "DBUS_SESSION_BUS_ADDRESS=unix:path=/keyring") {
		t.Fatal("credential/environment boundary")
	}
	for _, role := range []string{"planner", "worker", "mainagent"} {
		_, err := m.session(Request{Kind: Codex, Role: role, SessionID: "same"})
		if err == nil {
			t.Fatal("tool-contract role accepted")
		}
	}
}
func TestLiveInstalledCLIStatus(t *testing.T) {
	if os.Getenv("ARTEX_CLI_LIVE") != "1" {
		t.Skip("set ARTEX_CLI_LIVE=1 for actual installed CLI checks")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	m := New(t.Context(), filepath.Join(home, "artex"))
	defer m.Close()
	for _, kind := range []string{Codex, Agy} {
		status, err := m.Status(t.Context(), kind)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s installed=%v authenticated=%v state=%s", kind, status.Installed, status.Authenticated, status.State)
		if !status.Installed {
			t.Fatal("native CLI not installed")
		}
	}
	// Official app-server handshake and a persisted empty thread do not require
	// copying credentials from the user's normal CODEX_HOME.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := m.init(Codex); err != nil {
		t.Fatal(err)
	}
	p, err := m.codexProcess(ctx, filepath.Join(m.root, "codex"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	raw, err := p.rpc(ctx, "thread/start", map[string]any{"sandbox": "read-only", "approvalPolicy": "untrusted"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"id"`) {
		t.Fatal("thread/start returned no id")
	}
	t.Log("native Codex app-server handshake and thread/start passed")
}

func TestLiveGenerationAndRecovery(t *testing.T) {
	if os.Getenv("ARTEX_CLI_GENERATION") != "1" {
		t.Skip("requires authorized isolated CLI accounts")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "artex")
	for _, kind := range []string{Codex, Agy} {
		t.Run(kind, func(t *testing.T) {
			m := New(t.Context(), root)
			defer m.Close()
			status, err := m.Status(t.Context(), kind)
			if err != nil {
				t.Fatal(err)
			}
			if !status.Authenticated || len(status.Models) == 0 {
				t.Skipf("isolated %s authentication required", kind)
			}
			model := status.Models[0]
			if kind == Codex {
				for _, candidate := range status.Models {
					if candidate == "gpt-6.1-sol" {
						model = candidate
					}
				}
			}
			if kind == Agy {
				for _, candidate := range status.Models {
					if candidate == "gemini-3.8-flash-low" {
						model = candidate
					}
				}
			}
			prefix := fmt.Sprintf("live-%d", time.Now().UnixNano())
			var wg sync.WaitGroup
			for _, lane := range []string{"alpha", "bravo"} {
				wg.Add(1)
				go func(lane string) {
					defer wg.Done()
					req := Request{Kind: kind, Model: model, Role: "external", SessionID: prefix + "-" + lane, Prompt: "Remember the exact marker " + lane + ". Reply with only that marker. Do not use tools."}
					text, err := runText(t.Context(), m, req)
					if err != nil || !strings.Contains(strings.ToLower(text), lane) {
						t.Errorf("live %s: %q %v", lane, text, err)
					}
				}(lane)
			}
			wg.Wait()
			if t.Failed() {
				return
			}
			m.Close()
			m = New(t.Context(), root)
			defer m.Close()
			req := Request{Kind: kind, Model: model, Role: "external", SessionID: prefix + "-alpha", Prompt: "What exact marker did I ask you to remember? Reply with only that marker. Do not use tools."}
			text, err := runText(t.Context(), m, req)
			if err != nil || !strings.Contains(strings.ToLower(text), "alpha") || strings.Contains(strings.ToLower(text), "bravo") {
				t.Fatalf("live recovery: %q %v", text, err)
			}
			t.Log("native CLI parallel External Agent contexts and restart recovery passed")
		})
	}
}

func TestLinkedAuthorizationDisconnect(t *testing.T) {
	m := fixtureManager(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, kind := range []string{Codex, Agy} {
		relative := filepath.Join(".codex", "auth.json")
		if kind == Agy {
			relative = filepath.Join(".gemini", "antigravity-cli", "antigravity-oauth-token")
		}
		source := filepath.Join(home, relative)
		if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, []byte("fixture credential, never parsed"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Connect(t.Context(), kind); err != nil {
			t.Fatal(err)
		}
		target, err := os.Readlink(m.authPath(kind))
		if err != nil || target != source {
			t.Fatalf("auth link: %v", err)
		}
		if err := m.Disconnect(t.Context(), kind); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(source); err != nil {
			t.Fatal("disconnect changed original login")
		}
		if _, err := os.Lstat(m.authPath(kind)); !os.IsNotExist(err) {
			t.Fatal("shared authorization was not detached")
		}
	}
}

func TestLiveCancellation(t *testing.T) {
	if os.Getenv("ARTEX_CLI_GENERATION") != "1" {
		t.Skip("requires authorized isolated CLI accounts")
	}
	home, _ := os.UserHomeDir()
	for _, kind := range []string{Codex, Agy} {
		t.Run(kind, func(t *testing.T) {
			m := New(t.Context(), filepath.Join(home, "artex"))
			defer m.Close()
			status, err := m.Status(t.Context(), kind)
			if err != nil {
				t.Fatal(err)
			}
			if !status.Authenticated || len(status.Models) == 0 {
				t.Skip("OAuth login required")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			sawText := false
			req := Request{Kind: kind, Model: status.Models[0], Role: "external", SessionID: fmt.Sprintf("cancel-%d", time.Now().UnixNano()), Prompt: "Write the numbers from 1 to 100, one per line. Do not use tools."}
			err = m.Run(ctx, req, func(e Event) error {
				if e.Type == "text_delta" {
					sawText = true
					cancel()
				}
				return nil
			})
			if !sawText || !errors.Is(err, context.Canceled) {
				t.Fatalf("native cancellation: text=%v error=%v", sawText, err)
			}
			m.Close()
			if len(m.processSlots) != 0 {
				t.Fatal("native CLI not reaped")
			}
		})
	}
}
