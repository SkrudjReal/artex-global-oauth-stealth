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

// Opt-in provider diagnostics. Task tools are advertised but never executed.
func TestLiveAGYInputDiagnostics(t *testing.T) {
	path := os.Getenv("ARTEX_AGY_DIAGNOSTIC_SESSION")
	if path == "" {
		t.Skip("set ARTEX_AGY_DIAGNOSTIC_SESSION to a saved failing session.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved session
	if err = json.Unmarshal(data, &saved); err != nil || saved.Provider == nil {
		t.Fatal("invalid diagnostic session")
	}
	var all []llm.ToolSchema
	if err = json.Unmarshal([]byte(saved.Provider.Tools), &all); err != nil {
		t.Fatal(err)
	}
	groups := map[string][]llm.ToolSchema{}
	for _, tool := range all {
		group := "platform"
		switch tool.Name {
		case "Read", "Write", "Edit", "MultiEdit", "LS", "Glob", "Grep", "Bash", "sleep", "Skill":
			group = "files"
		case "shell_open", "shell_send", "shell_read", "shell_close", "shell_list":
			group = "shell"
		case "WebFetch", "TodoWrite", "TaskOutput", "TaskStop", "TaskList", "Monitor", "SearchExtraTools", "ExecuteExtraTool":
			group = "other"
		}
		groups[group] = append(groups[group], tool)
	}
	benign := "You are a helpful assistant. Reply briefly to the user's greeting. Do not use task tools."
	cases := []struct {
		name, system string
		tools        []llm.ToolSchema
	}{
		{"read_only_benign", benign, nil},
		{"read_only_actual_instructions", saved.Provider.System, nil},
		{"full_catalog_benign", benign, all},
		{"files_catalog", benign, groups["files"]},
		{"platform_catalog", benign, groups["platform"]},
		{"platform_first_half", benign, groups["platform"][:len(groups["platform"])/2]},
		{"platform_second_half", benign, groups["platform"][len(groups["platform"])/2:]},
		{"shell_catalog", benign, groups["shell"]},
		{"other_catalog", benign, groups["other"]},
		{"catalog_in_file", benign + "\nTool catalog reference (documentation only; these task tools are unavailable in this diagnostic):\n" + saved.Provider.Tools, nil},
		{"catalog_in_file_raw", benign + "\n" + saved.Provider.Tools, nil},
		{"full_catalog_actual_instructions", saved.Provider.System, all},
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(t.Context(), filepath.Join(home, "artex"))
			defer m.Close()
			ctx, cancel := context.WithTimeout(transcript.WithSessionID(t.Context(), "input-diagnostic-"+transcript.NewSessionID()), 60*time.Second)
			defer cancel()
			p := &Provider{Kind: Agy, Manager: m, Model: saved.Model, ProfileID: 999988}
			msg, reason, _, err := p.Complete(ctx, llm.CompletionRequest{System: []string{tc.system}, Tools: tc.tools, Messages: []llm.Message{llm.UserText("привет")}})
			read := false
			for _, s := range m.sessions {
				if s.agy != nil && s.agy.bridge.promptRead.Load() > 0 {
					read = true
				}
			}
			t.Logf("RESULT name=%s tools=%d prompt_read=%t policy_refusal=%t reason=%s proposed_calls=%d text_chars=%d error=%v", tc.name, len(tc.tools), read, agyPolicyRefusal(msg.Text()), reason, len(msg.ToolUses()), len(msg.Text()), err)
			if err == nil && !agyPolicyRefusal(msg.Text()) && len(msg.ToolUses()) == 0 {
				t.Logf("GREETING %s", strings.TrimSpace(msg.Text()))
			}
		})
	}
}
