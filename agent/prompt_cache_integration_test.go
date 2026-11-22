package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/llmcache"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

func TestPromptCache_FunVibeRegressionStreamAndNonStream(t *testing.T) {
	var callNum atomic.Int32
	var capturedBodies [][]byte
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		capturedBodies = append(capturedBodies, body)
		mu.Unlock()

		n := callNum.Add(1)
		var reqPayload struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &reqPayload)

		if !reqPayload.Stream {
			w.Header().Set("Content-Type", "application/json")
			switch n {
			case 1:
				// Exact FunVibe Request #1 (no prompt_tokens_details)
				_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"ok1"},"finish_reason":"stop"}],"usage":{"completion_tokens":2,"prompt_tokens":2633,"total_tokens":2635}}`))
			default:
				// Exact FunVibe Request #2 (with prompt_tokens_details.cached_tokens=2560)
				_, _ = w.Write([]byte(`{"id":"chatcmpl-2","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"ok2"},"finish_reason":"stop"}],"usage":{"completion_tokens":2,"prompt_tokens":2633,"prompt_tokens_details":{"cached_tokens":2560},"total_tokens":2635}}`))
			}
			return
		}

		// Streaming SSE: intermediate usage:null chunk + final chunk with cached_tokens
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"id":"chatcmpl-3","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"},"finish_reason":null}],"usage":null}`)
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, `data: {"id":"chatcmpl-3","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}],"usage":{"completion_tokens":2,"prompt_tokens":2633,"prompt_tokens_details":{"cached_tokens":2560},"total_tokens":2635}}`)
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, `data: [DONE]`)
		_, _ = fmt.Fprintln(w)
	}))
	defer srv.Close()

	store := llmcache.NewMemoryStore()
	cfg := Config{
		Format:             llm.FormatOpenAI,
		BaseURL:            srv.URL,
		APIKey:             "test-key",
		Model:              "glm-5.3",
		SystemInstructions: "Profile instruction alpha",
	}
	rawProv, err := cfg.NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	ap := llmcache.Wrap(rawProv, llmcache.AdaptiveConfig{
		ProfileID:          12,
		ProviderFormat:     "openai",
		SystemInstructions: cfg.SystemInstructions,
		Store:              store,
	})

	req := llm.CompletionRequest{
		System:   []string{"ARTEX Worker role prompt", "Runtime tools spec"},
		Messages: []llm.Message{llm.UserText("ping")},
	}

	// Non-streaming Request #1 -> metrics_unavailable
	if _, _, _, err := ap.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete 1 failed: %v", err)
	}
	st := ap.Stats()
	if st.Status != llmcache.StatusMetricsUnavailable || st.UncachedInputTokens != 2633 || st.CachedInputTokens != 0 {
		t.Fatalf("after FunVibe Req #1: expected metrics_unavailable & 2633 uncached, got %+v", st)
	}

	// Non-streaming Request #2 -> observed_hit
	if _, _, _, err := ap.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete 2 failed: %v", err)
	}
	st = ap.Stats()
	if st.Status != llmcache.StatusObservedHit || st.Hits != 1 || st.CachedInputTokens != 2560 || st.UncachedInputTokens != 2633+73 {
		t.Fatalf("after FunVibe Req #2: unexpected stats %+v", st)
	}

	// Streaming Request #3 -> still observed_hit, adds another 2560 cached + 73 uncached
	var gotText strings.Builder
	for ev, err := range ap.Stream(context.Background(), req) {
		if err != nil {
			t.Fatalf("Stream failed: %v", err)
		}
		if ev.Type == llm.SETextDelta {
			gotText.WriteString(ev.Text)
		}
	}
	if gotText.String() != "hello" {
		t.Fatalf("expected streamed 'hello', got %q", gotText.String())
	}
	st = ap.Stats()
	if st.Hits != 2 || st.CachedInputTokens != 5120 || st.RequestsWithUsage != 3 {
		t.Fatalf("after streaming hit: unexpected stats %+v", st)
	}

	// Ensure EVERY request carried ALL system blocks in order: Profile -> Role -> Runtime
	mu.Lock()
	defer mu.Unlock()
	if len(capturedBodies) != 3 {
		t.Fatalf("expected 3 HTTP calls, got %d", len(capturedBodies))
	}
	for i, b := range capturedBodies {
		s := string(b)
		pIdx := strings.Index(s, "Profile instruction alpha")
		rIdx := strings.Index(s, "ARTEX Worker role prompt")
		tIdx := strings.Index(s, "Runtime tools spec")
		if pIdx < 0 || rIdx < 0 || tIdx < 0 || !(pIdx < rIdx && rIdx < tIdx) {
			t.Fatalf("call %d missing or misordered system instructions: %s", i+1, s)
		}
	}
}

func TestPromptCache_AnthropicNativeFallbackAndResponsesAPI(t *testing.T) {
	// Test Anthropic native cache_control rejection -> one fallback retry without cache_control -> native_unsupported
	var antCalls atomic.Int32
	var sawCacheControl []bool
	antSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hasCC := strings.Contains(string(body), `"cache_control"`)
		sawCacheControl = append(sawCacheControl, hasCC)
		n := antCalls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"cache_control is unsupported by this endpoint"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"fallback ok"}],"stop_reason":"end_turn","usage":{"input_tokens":50,"output_tokens":5}}`))
	}))
	defer antSrv.Close()

	cfg := Config{
		Format:             llm.FormatAnthropic,
		BaseURL:            antSrv.URL,
		APIKey:             "ant-key",
		Model:              "claude-test",
		SystemInstructions: "Ant custom",
	}
	raw, err := cfg.NewProvider()
	if err != nil {
		t.Fatalf("NewProvider anthropic: %v", err)
	}
	ap := llmcache.Wrap(raw, llmcache.AdaptiveConfig{
		ProfileID:          77,
		ProviderFormat:     "anthropic",
		SystemInstructions: cfg.SystemInstructions,
		Store:              llmcache.NewMemoryStore(),
	})

	msg, _, _, err := ap.Complete(context.Background(), llm.CompletionRequest{
		System:   []string{"Static role"},
		Messages: []llm.Message{llm.UserText("hi")},
	})
	if err != nil || msg.Text() != "fallback ok" {
		t.Fatalf("expected transparent fallback success, got msg=%q err=%v", msg.Text(), err)
	}
	if len(sawCacheControl) != 2 || !sawCacheControl[0] || sawCacheControl[1] {
		t.Fatalf("expected 1st attempt with cache_control and 2nd without, got %v", sawCacheControl)
	}
	if st := ap.Stats(); st.Status != llmcache.StatusNativeUnsupported || st.Strategy != llmcache.StrategyStandard {
		t.Fatalf("expected native_unsupported/standard after fallback, got %+v", st)
	}

	// OpenAI Responses API detection
	respSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"resp ok"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"input_tokens_details":{"cached_tokens":768}}}`))
	}))
	defer respSrv.Close()

	rcfg := Config{Format: llm.FormatOpenAIResponses, BaseURL: respSrv.URL, APIKey: "r-key", Model: "gpt-5"}
	rraw, err := rcfg.NewProvider()
	if err != nil {
		t.Fatalf("NewProvider responses: %v", err)
	}
	rap := llmcache.Wrap(rraw, llmcache.AdaptiveConfig{
		ProfileID:      88,
		ProviderFormat: "openai-responses",
		Store:          llmcache.NewMemoryStore(),
	})
	if _, _, _, err := rap.Complete(context.Background(), llm.CompletionRequest{
		System:   []string{"Role"},
		Messages: []llm.Message{llm.UserText("hi")},
	}); err != nil {
		t.Fatalf("responses Complete: %v", err)
	}
	if rst := rap.Stats(); rst.Status != llmcache.StatusObservedHit || rst.CachedInputTokens != 768 || rst.UncachedInputTokens != 232 {
		t.Fatalf("unexpected responses stats: %+v", rst)
	}
}

func TestPromptCache_WorkerPrefixStabilityAndToolCallingLoop(t *testing.T) {
	// Verify workerStaticSystem + deferredSystem puts runDir AFTER DynamicBoundary,
	// keeping static prefix identical across different intents.
	def := DeferredInfo{GlobalNames: []string{"mcp__demo__ping"}}
	sys1, b1 := deferredSystem(workerStaticSystem("", "", "/data"), def)
	sys1 = append(sys1, workerArtifactSpec("/data/tasks/1/i101"))
	sys2, b2 := deferredSystem(workerStaticSystem("", "", "/data"), def)
	sys2 = append(sys2, workerArtifactSpec("/data/tasks/1/i202"))

	if b1 != b2 || b1 <= 0 {
		t.Fatalf("expected equal positive DynamicBoundary, got %d vs %d", b1, b2)
	}
	for i := 0; i < b1; i++ {
		if sys1[i] != sys2[i] {
			t.Fatalf("static prefix segment %d diverged across worker intents", i)
		}
	}
	if strings.Contains(strings.Join(sys1[:b1], "\n"), "/i101") {
		t.Fatalf("per-intent runDir leaked into static prefix before DynamicBoundary")
	}

	// Verify tool calling still works cleanly end-to-end through AdaptiveProvider
	var turn atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := turn.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"echo_tool","arguments":"{\"v\":\"hi\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":400,"completion_tokens":12}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"c2","choices":[{"index":0,"message":{"role":"assistant","content":"tool result verified"},"finish_reason":"stop"}],"usage":{"prompt_tokens":500,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":384}}}`))
	}))
	defer srv.Close()

	cfg := Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, APIKey: "k", Model: "glm-5.3", SystemInstructions: "Be concise"}
	raw, _ := cfg.NewProvider()
	ap := llmcache.Wrap(raw, llmcache.AdaptiveConfig{
		ProfileID:          55,
		ProviderFormat:     "openai",
		SystemInstructions: cfg.SystemInstructions,
		Store:              llmcache.NewMemoryStore(),
	})

	echoCalled := false
	echoTool := actool.Build(actool.Spec{
		Name:        "echo_tool",
		Description: "echo test tool",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"v": map[string]any{"type": "string"}},
		},
		ReadOnly: func(json.RawMessage) bool { return true },
		Run: func(_ context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			echoCalled = true
			return actool.Text("echoed:" + string(in)), nil
		},
	})

	reply, err := agentcore.Run(context.Background(), agentcore.Options{
		Provider:        ap,
		Tools:           []actool.CoreTool{echoTool},
		SystemPrompt:    sys1,
		DynamicBoundary: b1,
		PermissionMode:  acperm.ModeBypass,
		MaxTurns:        3,
		NonStreaming:    true,
	}, "run echo_tool")
	if err != nil {
		t.Fatalf("agentcore.Run failed: %v", err)
	}
	if !echoCalled || !strings.Contains(reply, "tool result verified") {
		t.Fatalf("expected tool execution and reply, echoCalled=%v reply=%q", echoCalled, reply)
	}
	if st := ap.Stats(); st.Status != llmcache.StatusObservedHit || st.CachedInputTokens != 384 {
		t.Fatalf("expected hit after multi-turn tool execution, got %+v", st)
	}
}
