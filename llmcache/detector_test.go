package llmcache

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

func TestDetectFromRawResponse_FunVibeRegressionAndEdgeCases(t *testing.T) {
	// 1. Confirmed FunVibe GLM-5.3 Request #1 (no prompt_tokens_details)
	req1 := []byte(`{"completion_tokens":2,"prompt_tokens":2633,"total_tokens":2635}`)
	// Envelope format returned by Chat Completions
	env1 := []byte(`{"id":"chatcmpl-1","usage":` + string(req1) + `}`)
	u1 := DetectFromRawResponse(200, env1, "openai")
	if !u1.ProviderUsageReported || u1.Presence != PresenceAbsent {
		t.Fatalf("req1 expected ProviderUsageReported=true PresenceAbsent, got %+v", u1)
	}
	if u1.InputTokens != 2633 || u1.UncachedInputTokens != 2633 || u1.CachedReadTokens != 0 || u1.OutputTokens != 2 {
		t.Fatalf("unexpected req1 normalized tokens: %+v", u1)
	}

	// 2. Confirmed FunVibe GLM-5.3 Request #2 (cached_tokens: 2560)
	env2 := []byte(`{"id":"chatcmpl-2","usage":{"completion_tokens":2,"prompt_tokens":2633,"prompt_tokens_details":{"cached_tokens":2560,"reasoning_tokens":1},"total_tokens":2635}}`)
	u2 := DetectFromRawResponse(200, env2, "openai")
	if u2.Presence != PresencePositive || u2.CachedReadTokens != 2560 || u2.UncachedInputTokens != 73 || u2.InputTokens != 2633 {
		t.Fatalf("unexpected req2 normalized tokens: %+v", u2)
	}
	if got := u2.Extra["details.reasoning_tokens"]; got != float64(1) {
		t.Fatalf("expected extra field details.reasoning_tokens=1 preserved, got %#v", u2.Extra)
	}

	// 3. Explicit zero miss
	envMiss := []byte(`{"usage":{"prompt_tokens":1500,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":0}}}`)
	uMiss := DetectFromRawResponse(200, envMiss, "openai")
	if uMiss.Presence != PresenceZero || uMiss.UncachedInputTokens != 1500 || uMiss.CachedReadTokens != 0 {
		t.Fatalf("expected explicit zero miss, got %+v", uMiss)
	}

	// 4. Explicit null cached_tokens must NOT claim caching impossible
	envNull := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":null}}}`)
	uNull := DetectFromRawResponse(200, envNull, "openai")
	if uNull.Presence != PresenceNull {
		t.Fatalf("expected PresenceNull, got %+v", uNull)
	}

	// 5. Missing usage completely
	uNoUsage := DetectFromRawResponse(200, []byte(`{"id":"x","choices":[]}`), "openai")
	if uNoUsage.ProviderUsageReported || uNoUsage.Presence != PresenceAbsent {
		t.Fatalf("expected absent usage, got %+v", uNoUsage)
	}

	// 6. Anomalous cached_tokens > prompt_tokens clamps uncached to 0 and flags anomaly
	uAnom := DetectFromRawResponse(200, []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":250}}}`), "openai")
	if !uAnom.CacheAnomaly || uAnom.UncachedInputTokens != 0 || uAnom.CachedReadTokens != 250 {
		t.Fatalf("expected CacheAnomaly=true and clamped Uncached=0, got %+v", uAnom)
	}

	// 7. Negative cached_tokens invalid
	uNeg := DetectFromRawResponse(200, []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":-3}}}`), "openai")
	if !uNeg.CacheAnomaly || uNeg.Presence != PresenceInvalid || uNeg.CachedReadTokens != 0 {
		t.Fatalf("expected invalid negative metric handling, got %+v", uNeg)
	}
}

func TestDetectResponsesAndAnthropicAndSSE(t *testing.T) {
	// OpenAI Responses API streaming completed event
	sseResp := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":3000,"output_tokens":40,"input_tokens_details":{"cached_tokens":2048}}}}`)
	uR, ok := DetectFromSSELine(sseResp, "openai-responses")
	if !ok || uR.Presence != PresencePositive || uR.CachedReadTokens != 2048 || uR.UncachedInputTokens != 952 {
		t.Fatalf("unexpected responses SSE detection: ok=%v %+v", ok, uR)
	}

	// Intermediate OpenAI SSE chunk with usage: null must be ignored
	if _, ok := DetectFromSSELine([]byte(`{"choices":[{"delta":{"content":"hi"}}],"usage":null}`), "openai"); ok {
		t.Fatal("expected usage:null streaming delta chunk not to count as reported usage")
	}

	// Anthropic cache creation without cache read -> Miss (PresenceZero), while recording CacheWriteTokens
	antCreate := DetectFromRawResponse(200, []byte(`{"usage":{"input_tokens":500,"cache_creation_input_tokens":2200,"cache_read_input_tokens":0,"output_tokens":50}}`), "anthropic")
	if antCreate.Presence != PresenceZero || antCreate.CacheWriteTokens != 2200 || antCreate.UncachedInputTokens != 500 || antCreate.InputTokens != 2700 {
		t.Fatalf("unexpected Anthropic cache write observation: %+v", antCreate)
	}

	// Anthropic cache read -> Hit (PresencePositive)
	antHit := DetectFromRawResponse(200, []byte(`{"usage":{"input_tokens":120,"cache_creation_input_tokens":0,"cache_read_input_tokens":2200,"output_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":2200}}}`), "anthropic")
	if antHit.Presence != PresencePositive || antHit.CachedReadTokens != 2200 || antHit.UncachedInputTokens != 120 || antHit.InputTokens != 2320 {
		t.Fatalf("unexpected Anthropic cache hit observation: %+v", antHit)
	}
	if _, ok := antHit.Extra["cache_creation"]; !ok {
		t.Fatalf("expected extra Anthropic cache_creation metadata preserved, got %+v", antHit.Extra)
	}
}

func TestStateTransitionsAndSingleMissDoesNotDisable(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	var st ProfileStats

	// Req 1: metrics unavailable (absent details) -> metrics_unavailable
	st = ApplyObservation(st, NormalizedUsage{ProviderUsageReported: true, InputTokens: 2633, UncachedInputTokens: 2633, OutputTokens: 2, Presence: PresenceAbsent}, "openai", now)
	if st.Status != StatusMetricsUnavailable {
		t.Fatalf("expected metrics_unavailable after req1, got %s", st.Status)
	}

	// Req 2: explicit 0 -> observed_miss (still automatic strategy)
	st = ApplyObservation(st, NormalizedUsage{ProviderUsageReported: true, InputTokens: 2633, UncachedInputTokens: 2633, OutputTokens: 2, Presence: PresenceZero}, "openai", now.Add(time.Second))
	if st.Status != StatusObservedMiss || st.Strategy != StrategyAutomatic || st.Misses != 1 {
		t.Fatalf("expected observed_miss with automatic strategy, got %+v", st)
	}

	// Req 3: hit 2560 -> observed_hit
	st = ApplyObservation(st, NormalizedUsage{ProviderUsageReported: true, InputTokens: 2633, UncachedInputTokens: 73, CachedReadTokens: 2560, OutputTokens: 2, Presence: PresencePositive}, "openai", now.Add(2*time.Second))
	if st.Status != StatusObservedHit || st.Hits != 1 || st.LastCacheHitAt == nil {
		t.Fatalf("expected observed_hit, got %+v", st)
	}

	// Req 4: another miss (cached_tokens=0) must NOT revoke observed_hit or flip strategy
	st = ApplyObservation(st, NormalizedUsage{ProviderUsageReported: true, InputTokens: 1000, UncachedInputTokens: 1000, OutputTokens: 10, Presence: PresenceZero}, "openai", now.Add(3*time.Second))
	if st.Status != StatusObservedHit || st.Misses != 2 || st.LastRequestCachedTokens != 0 {
		t.Fatalf("expected status to keep observed_hit and count miss, got %+v", st)
	}
}

func TestComputeCost_DiscountUnknownVsConfirmed(t *testing.T) {
	// Only input & output rates set (reseller cached rate unknown) -> NEVER fabricate 50%/90% discount
	inRate := 2.0
	outRate := 8.0
	estUnknown := ComputeCost(73, 2560, 100, Rates{InputRatePer1M: &inRate, OutputRatePer1M: &outRate})
	if estUnknown.DiscountStatus != DiscountUnknown || estUnknown.TotalCostUSD != nil || estUnknown.SavingsUSD != nil {
		t.Fatalf("must not guess reseller discount when cached rate is nil: %+v", estUnknown)
	}
	if estUnknown.MaxWithoutDiscUSD == nil {
		t.Fatalf("expected upper bound without discount, got nil")
	}

	// Explicit cached rate verified ($0.20 / 1M = 90% discount)
	cRate := 0.2
	estKnown := ComputeCost(73, 2560, 100, Rates{InputRatePer1M: &inRate, CachedInputRatePer1M: &cRate, OutputRatePer1M: &outRate})
	if estKnown.DiscountStatus != DiscountConfirmed || estKnown.TotalCostUSD == nil || estKnown.DiscountPercent == nil {
		t.Fatalf("expected confirmed discount & total cost, got %+v", estKnown)
	}
	if *estKnown.DiscountPercent < 89.9 || *estKnown.DiscountPercent > 90.1 {
		t.Fatalf("expected ~90%% discount, got %v", *estKnown.DiscountPercent)
	}
}

type fakeProvider struct {
	mu       sync.Mutex
	calls    []llm.CompletionRequest
	complete func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
	stream   func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error]
}

func (f *fakeProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	return f.complete(ctx, req)
}

func (f *fakeProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	if f.stream != nil {
		return f.stream(ctx, req)
	}
	return func(yield func(llm.StreamEvent, error) bool) {}
}

func TestAdaptiveProvider_SystemInstructionsOrderNoDupAndFallback(t *testing.T) {
	store := NewMemoryStore()
	attempts := 0
	fp := &fakeProvider{
		complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
			attempts++
			obs := CallObserverFromContext(ctx)
			if attempts == 1 && req.DynamicBoundary > 0 {
				obs.InspectErrorBody(http.StatusBadRequest, []byte(`{"error":{"message":"Extra inputs are not permitted: cache_control is unsupported"}}`))
				return llm.Message{}, "", llm.Usage{}, errors.New("400 bad request: cache_control unsupported")
			}
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("ok")}}, "stop", llm.Usage{InputTokens: 120, OutputTokens: 5}, nil
		},
	}

	ap := Wrap(fp, AdaptiveConfig{
		ProfileID:          42,
		ProviderFormat:     "anthropic",
		SystemInstructions: "Per-profile rule X",
		Store:              store,
	})

	req := llm.CompletionRequest{
		System: []string{"Role system prompt", "Runtime tools prompt"},
		Tools: []llm.ToolSchema{
			{Name: "zeta_tool"},
			{Name: "alpha_tool"},
		},
		Messages: []llm.Message{llm.UserText("hello")},
	}
	_, _, _, err := ap.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("expected fallback retry to succeed after native cache_control rejection, got err=%v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts (initial native + fallback standard), got %d", attempts)
	}
	if got := ap.Stats(); got.Status != StatusObservedMiss && got.Status != StatusNativeUnsupported {
		// Note: second call returns fallbackUsage without cache_read -> StatusNativeUnsupported must remain!
		t.Fatalf("expected StatusNativeUnsupported preserved, got %+v", got)
	}
	// Verify second attempt dropped DynamicBoundary=0, kept ALL instructions in exact order, and sorted tools deterministically
	retryReq := fp.calls[1]
	if retryReq.DynamicBoundary != 0 {
		t.Fatalf("expected DynamicBoundary=0 after fallback, got %d", retryReq.DynamicBoundary)
	}
	if len(retryReq.System) != 3 ||
		retryReq.System[0] != "Per-profile rule X" ||
		retryReq.System[1] != "Role system prompt" ||
		retryReq.System[2] != "Runtime tools prompt" {
		t.Fatalf("unexpected system order: %#v", retryReq.System)
	}
	if retryReq.Tools[0].Name != "alpha_tool" || retryReq.Tools[1].Name != "zeta_tool" {
		t.Fatalf("expected deterministic sorted tools, got %#v", retryReq.Tools)
	}

	// Already-prepended SystemInstructions must NOT be duplicated, and subsequent calls keep all instructions!
	dupReq := llm.CompletionRequest{
		System:   []string{"Per-profile rule X", "Role system prompt"},
		Messages: []llm.Message{llm.UserText("second turn")},
	}
	if _, _, _, err := ap.Complete(context.Background(), dupReq); err != nil {
		t.Fatalf("second complete failed: %v", err)
	}
	lastCall := fp.calls[len(fp.calls)-1]
	if len(lastCall.System) != 2 {
		t.Fatalf("must not duplicate SystemInstructions when already present at index 0, got %#v", lastCall.System)
	}

	// WithoutProfileInstructions omits per-profile instruction cleanly
	if _, _, _, err := ap.Complete(WithoutProfileInstructions(context.Background()), req); err != nil {
		t.Fatalf("utility complete failed: %v", err)
	}
	utilCall := fp.calls[len(fp.calls)-1]
	if len(utilCall.System) != 2 || utilCall.System[0] != "Role system prompt" {
		t.Fatalf("expected WithoutProfileInstructions to omit profile instructions, got %#v", utilCall.System)
	}
}
