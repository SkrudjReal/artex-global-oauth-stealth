package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/externalcli"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Real Norma harness: the CLI proposes a call; only host permissions may execute it.
func TestLiveCodexARTEXWorkers(t *testing.T) { testCLIWorkers(t, "codex-oauth", "gpt-6.1-sol") }
func TestLiveAGYARTEXWorkers(t *testing.T)   { testCLIWorkers(t, "agy-oauth", "gemini-3.8-flash-high") }
func testCLIWorkers(t *testing.T, kind, model string) {
	if os.Getenv("ARTEX_CLI_GENERATION") != "1" {
		t.Skip("live OAuth CLI generation disabled")
	}
	home, _ := os.UserHomeDir()
	m := externalcli.New(t.Context(), filepath.Join(home, "artex"))
	t.Cleanup(m.Close)
	cfg := ConfigFrom(kind, model, "", "", "")
	cfg.CLIManager = m
	cfg.CLIProfileID = 999991
	provider, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"worker-alpha", "worker-bravo"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			var calls atomic.Int32
			echo := tool.Build(tool.Spec{Name: "artex_local_echo", Description: "Return this worker's private marker", Schema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}, ReadOnly: func(json.RawMessage) bool { return true }, Concurrent: func(json.RawMessage) bool { return true }, Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
				return permission.Allowed()
			}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
				calls.Add(1)
				return tool.Text(name + "-only-74291"), nil
			}})
			reply, err := agentcore.Run(ctx, agentcore.Options{Provider: provider, SessionID: "cli-worker-test-" + name + "-" + transcript.NewSessionID(), WorkingDir: t.TempDir(), Transcript: transcript.NewStore(t.TempDir()), SystemPrompt: []string{"Call artex_local_echo once and report its exact result. Do not use other tools."}, Tools: []tool.CoreTool{echo}, PermissionMode: permission.ModeDefault, MaxTurns: 3}, "Use the tool and report its result.")
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || !strings.Contains(reply, name+"-only-74291") {
				t.Fatalf("host tool execution/history lost: calls=%d reply=%s", calls.Load(), reply)
			}
		})
	}
}

func TestLiveCodexARTEXPermissionDenial(t *testing.T) { testCLIDenial(t, "codex-oauth", "gpt-6.1-sol") }
func TestLiveAGYARTEXPermissionDenial(t *testing.T) {
	testCLIDenial(t, "agy-oauth", "gemini-3.8-flash-high")
}
func testCLIDenial(t *testing.T, kind, model string) {
	if os.Getenv("ARTEX_CLI_GENERATION") != "1" {
		t.Skip("live OAuth CLI generation disabled")
	}
	home, _ := os.UserHomeDir()
	m := externalcli.New(t.Context(), filepath.Join(home, "artex"))
	t.Cleanup(m.Close)
	cfg := ConfigFrom(kind, model, "", "", "")
	cfg.CLIManager = m
	provider, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var decisions atomic.Int32
	denied := tool.Build(tool.Spec{Name: "artex_denied_action", Description: "A permission-gated local test action", Schema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}, Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
		decisions.Add(1)
		return permission.Denied("ARTEX_TEST_DENIED")
	}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		calls.Add(1)
		return tool.Text("should never execute"), nil
	}})
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	reply, err := agentcore.Run(ctx, agentcore.Options{Provider: provider, SessionID: "cli-denial-test-" + transcript.NewSessionID(), WorkingDir: t.TempDir(), Transcript: transcript.NewStore(t.TempDir()), Tools: []tool.CoreTool{denied}, SystemPrompt: []string{"Call artex_denied_action exactly once. If denied, say DENIED and stop; do not retry or use other tools."}, MaxTurns: 3}, "Attempt the test action once.")
	if err != nil {
		t.Fatal(err)
	}
	if decisions.Load() == 0 || calls.Load() != 0 || !strings.Contains(strings.ToUpper(reply), "DENIED") {
		t.Fatalf("permission boundary failed: calls=%d reply=%s", calls.Load(), reply)
	}
}
