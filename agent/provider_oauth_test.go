package agent

import (
	"github.com/Autumn-27/artex/externalcli"
	"github.com/Autumn-27/norma/llm"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOAuthCannotReplaceToolCallingProvider(t *testing.T) {
	for _, kind := range []string{"codex-oauth", "agy-oauth"} {
		cfg := ConfigFrom(kind, "model", "", "", "")
		if cfg.Provider() != kind {
			t.Fatal("OAuth format was silently converted to API Key")
		}
		if _, err := cfg.NewProvider(); err == nil {
			t.Fatal("OAuth CLI accepted as a tool-calling LLM")
		}
	}
	m := externalcli.New(t.Context(), t.TempDir())
	defer m.Close()
	for _, kind := range []string{"codex-oauth", "agy-oauth"} {
		c := ConfigFrom(kind, "model", "", "", "")
		c.CLIManager = m
		if _, err := c.NewProvider(); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"openai", "anthropic", "openai-responses"} {
		cfg := ConfigFrom(kind, "model", "http://127.0.0.1:1", "test-key", "")
		if _, err := cfg.NewProvider(); err != nil {
			t.Fatalf("API Key provider %s broken: %v", kind, err)
		}
	}
}

func TestAPIKeyWireFormatsUnaffected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-CH-UA") != "" || r.Header.Get("Sec-CH-UA-Platform") != "" {
			t.Error("laboratory headers leaked into LLM API")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch r.URL.Path {
		case "/v1/chat/completions":
			if r.Header.Get("Authorization") != "Bearer test-key" {
				t.Error("API Key missing")
			}
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		case "/v1/responses":
			if r.Header.Get("Authorization") != "Bearer test-key" {
				t.Error("API Key missing")
			}
			io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		case "/v1/messages":
			if r.Header.Get("X-Api-Key") != "test-key" {
				t.Error("API Key missing")
			}
			io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"test\",\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	for _, kind := range []string{"openai", "anthropic", "openai-responses"} {
		base := server.URL + "/v1"
		if kind == "anthropic" {
			base = server.URL
		}
		cfg := ConfigFrom(kind, "test-model", base, "test-key", "")
		provider, err := cfg.NewProvider()
		if err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		for event, err := range provider.Stream(t.Context(), llm.CompletionRequest{System: []string{"test system"}, Messages: []llm.Message{llm.UserText("test")}}) {
			if err != nil {
				t.Fatal(err)
			}
			if event.Type == llm.SETextDelta {
				text.WriteString(event.Text)
			}
		}
		if text.String() != "OK" {
			t.Fatalf("%s: %q", kind, text.String())
		}
	}
}
