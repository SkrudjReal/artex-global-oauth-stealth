package externalcli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func TestAGYUnreadPromptResponses(t *testing.T) {
	refusal := "The prompt could not be submitted. The prompt contains sensitive words that violate Google's [Generative AI Prohibited Use policy](https://policies.google.com/terms/generative-ai/use-policy). Try rephrasing the prompt."
	for _, tc := range []struct {
		name, response string
		refusal        bool
	}{
		{"upstream policy refusal", refusal, true},
		{"ordinary unread answer", "Done, I followed your instructions.", false},
		{"policy link alone", "See https://policies.google.com/terms/generative-ai/use-policy", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureManager(t)
			script := strings.Replace(fixture, "args=sys.argv[1:]", "args=sys.argv[1:]\nif args==['--version']:print('1.4.12');sys.exit(0)", 1)
			encoded, _ := json.Marshal(tc.response)
			script = strings.Replace(script, "  prompt=msg['message']['content'];out=reply(prompt)", "  prompt=msg['message']['content']\n  with open('requests.txt','a') as f:f.write('request\\n')\n  out="+string(encoded), 1)
			if err := os.WriteFile(filepath.Join(m.root, "agy/bin/agy"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			p := &Provider{Kind: Agy, Manager: m, Model: "gemini-fixture-high"}
			ctx := transcript.WithSessionID(t.Context(), tc.name)
			req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hey")}}
			msg, reason, _, err := p.Complete(ctx, req)
			if tc.refusal {
				if err != nil || reason != "end_turn" || msg.Text() != refusal {
					t.Fatalf("upstream refusal was masked: reason=%s error=%v text=%q", reason, err, msg.Text())
				}
			} else if err == nil || !strings.Contains(err.Error(), "did not read") || msg.Text() != "" {
				t.Fatalf("unread agent answer accepted: %v %q", err, msg.Text())
			}
			for _, s := range m.sessions {
				if s.Provider.Dirty == tc.refusal || s.Provider.Awaiting || s.agy.bridge.promptRead.Load() != 0 {
					t.Fatal("incorrect refusal/agent checkpoint or prompt-read state")
				}
				raw, err := os.ReadFile(filepath.Join(s.dir, "work/requests.txt"))
				want := 2
				if tc.refusal {
					want = 1
				}
				if err != nil || strings.Count(string(raw), "request\n") != want {
					t.Fatalf("unexpected reminder count: %v %q", err, raw)
				}
			}
		})
	}
}

func TestAGYPromptFileTransport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.txt")
	if err := writeAGYPrompt(path, []string{"AGENT_A"}, []llm.Message{llm.UserText("REQUEST_A")}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("prompt is not private")
	}
	bridge, err := newAGYBridge(t.Context(), nil, path)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.close()
	bridge.promptGeneration.Add(1)
	read := func() string {
		response, err := http.Post(bridge.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"artex_read_prompt","arguments":{"path":"/etc/passwd"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result.Result.Content[0].Text
	}
	first := read()
	if !strings.Contains(first, "AGENT_A") || !strings.Contains(first, "REQUEST_A") || strings.Contains(first, "root:x:") {
		t.Fatal("read escaped the session prompt file")
	}
	if bridge.promptRead.Load() != bridge.promptGeneration.Load() {
		t.Fatal("read not recorded")
	}
	if err := writeAGYPrompt(path, []string{"AGENT_B"}, []llm.Message{llm.UserText("REQUEST_B"), {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("call", "RESULT_B", false)}}}); err != nil {
		t.Fatal(err)
	}
	bridge.promptGeneration.Add(1)
	if bridge.promptRead.Load() == bridge.promptGeneration.Load() {
		t.Fatal("previous read accepted for new request")
	}
	second := read()
	if !strings.Contains(second, "AGENT_B") || !strings.Contains(second, "RESULT_B") || strings.Contains(second, "REQUEST_A") {
		t.Fatal("prompt not replaced")
	}
}
