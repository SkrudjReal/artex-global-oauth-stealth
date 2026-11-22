package llmcache

import (
	"bytes"
	"cmp"
	"context"
	"io"
	"iter"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/norma/llm"
)

type ctxKeyObserver struct{}
type ctxKeyWithoutInstructions struct{}

// WithCallObserver attaches a CallObserver to ctx so the HTTP transport can record exact raw usage metadata.
func WithCallObserver(ctx context.Context, obs *CallObserver) context.Context {
	return context.WithValue(ctx, ctxKeyObserver{}, obs)
}

// CallObserverFromContext returns the CallObserver on ctx, or nil if absent.
func CallObserverFromContext(ctx context.Context) *CallObserver {
	if ctx == nil {
		return nil
	}
	obs, _ := ctx.Value(ctxKeyObserver{}).(*CallObserver)
	return obs
}

// WithoutProfileInstructions returns a derived context that omits per-profile SystemInstructions
// for isolated internal utility calls (e.g. connection probes, compaction).
func WithoutProfileInstructions(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyWithoutInstructions{}, true)
}

// IsWithoutProfileInstructions reports whether per-profile SystemInstructions injection is suppressed.
func IsWithoutProfileInstructions(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(ctxKeyWithoutInstructions{}).(bool)
	return v
}

// CallObserver inspects raw HTTP responses (non-streaming JSON, SSE streams, and 4xx errors)
// for a single LLM invocation without buffering or delaying streaming output.
type CallObserver struct {
	providerFormat string

	mu          sync.Mutex
	usage       NormalizedUsage
	nativeErr   bool
	nativeCause string
	recorded    bool
}

// NewCallObserver creates an observer tuned for the specified provider format.
func NewCallObserver(providerFormat string) *CallObserver {
	return &CallObserver{providerFormat: providerFormat}
}

// InspectErrorBody records whether a 4xx HTTP error indicates an unsupported native caching parameter.
func (o *CallObserver) InspectErrorBody(statusCode int, body []byte) {
	if o == nil {
		return
	}
	if unsupported, reason := DetectNativeUnsupportedError(statusCode, body); unsupported {
		o.mu.Lock()
		o.nativeErr = true
		o.nativeCause = reason
		o.usage = NormalizedUsage{
			Presence:          PresenceAbsent,
			NativeError:       true,
			NativeErrorReason: reason,
		}
		o.recorded = true
		o.mu.Unlock()
	}
}

// NativeUnsupportedDetected reports whether the HTTP call failed due to unsupported native cache fields.
func (o *CallObserver) NativeUnsupportedDetected() (bool, string) {
	if o == nil {
		return false, ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.nativeErr, o.nativeCause
}

// Snapshot returns the normalized usage observed at the HTTP wire layer so far.
func (o *CallObserver) Snapshot() (NormalizedUsage, bool) {
	if o == nil {
		return NormalizedUsage{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.usage, o.recorded
}

// TeeBody wraps an HTTP 2xx response body to inspect JSON or SSE payloads inline as they are read.
func (o *CallObserver) TeeBody(body io.ReadCloser, statusCode int, contentType string) io.ReadCloser {
	if o == nil || body == nil {
		return body
	}
	isSSE := strings.Contains(strings.ToLower(contentType), "text/event-stream")
	return &teeObserverBody{
		inner:      body,
		obs:        o,
		statusCode: statusCode,
		sseHint:    isSSE,
	}
}

type teeObserverBody struct {
	inner      io.ReadCloser
	obs        *CallObserver
	statusCode int
	sseHint    bool
	decidedSSE bool

	lineBuf []byte
	rawBuf  bytes.Buffer
	closed  bool
}

const maxRawInspectBytes = 2 << 20 // 2 MiB safety cap for non-SSE JSON body inspection

func (t *teeObserverBody) Read(p []byte) (int, error) {
	n, err := t.inner.Read(p)
	if n > 0 {
		t.feed(p[:n])
	}
	if err == io.EOF {
		t.finalize()
	}
	return n, err
}

func (t *teeObserverBody) Close() error {
	t.finalize()
	return t.inner.Close()
}

func (t *teeObserverBody) feed(chunk []byte) {
	if !t.decidedSSE {
		trimmed := bytes.TrimLeft(chunk, " \t\r\n")
		if len(trimmed) > 0 {
			if t.sseHint || bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte("event:")) {
				t.sseHint = true
			}
			t.decidedSSE = true
		}
	}

	if t.sseHint {
		t.lineBuf = append(t.lineBuf, chunk...)
		for {
			idx := bytes.IndexByte(t.lineBuf, '\n')
			if idx < 0 {
				break
			}
			line := bytes.TrimRight(t.lineBuf[:idx], "\r")
			t.lineBuf = t.lineBuf[idx+1:]
			t.processSSELine(line)
		}
		return
	}

	if t.rawBuf.Len() < maxRawInspectBytes {
		remain := maxRawInspectBytes - t.rawBuf.Len()
		if len(chunk) <= remain {
			t.rawBuf.Write(chunk)
		} else {
			t.rawBuf.Write(chunk[:remain])
		}
	}
}

func (t *teeObserverBody) processSSELine(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	u, ok := DetectFromSSELine(payload, t.obs.providerFormat)
	if !ok {
		return
	}
	t.obs.mu.Lock()
	if !t.obs.recorded {
		t.obs.usage = u
		t.obs.recorded = true
	} else {
		t.obs.usage = MergeSSEUsage(t.obs.usage, u)
	}
	t.obs.mu.Unlock()
}

func (t *teeObserverBody) finalize() {
	if t.closed {
		return
	}
	t.closed = true
	if t.sseHint {
		if len(t.lineBuf) > 0 {
			t.processSSELine(t.lineBuf)
			t.lineBuf = nil
		}
		return
	}
	if t.rawBuf.Len() == 0 {
		return
	}
	u := DetectFromRawResponse(t.statusCode, t.rawBuf.Bytes(), t.obs.providerFormat)
	t.obs.mu.Lock()
	if !t.obs.recorded || u.ProviderUsageReported || u.NativeError {
		t.obs.usage = u
		t.obs.recorded = true
	}
	t.obs.mu.Unlock()
	t.rawBuf.Reset()
}

// Store persists and retrieves per-profile prompt cache statistics safely across concurrent goroutines.
type Store interface {
	Get(profileID int64) (ProfileStats, bool)
	Record(profileID int64, providerFormat string, obs NormalizedUsage, now time.Time) ProfileStats
	Reset(profileID int64, providerFormat string) ProfileStats
}

// MemoryStore is an in-memory thread-safe Store keyed by profileID.
type MemoryStore struct {
	mu    sync.RWMutex
	stats map[int64]ProfileStats
}

// NewMemoryStore initializes a MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{stats: make(map[int64]ProfileStats)}
}

// Get returns a copy of stored statistics for profileID.
func (m *MemoryStore) Get(profileID int64) (ProfileStats, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.stats[profileID]
	return st, ok
}

// Record atomically applies an observation to the profile's statistics and returns the updated snapshot.
func (m *MemoryStore) Record(profileID int64, providerFormat string, obs NormalizedUsage, now time.Time) ProfileStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.stats[profileID]
	cur.ProfileID = profileID
	updated := ApplyObservation(cur, obs, providerFormat, now)
	m.stats[profileID] = updated
	return updated
}

// Reset clears observed cache telemetry for profileID while returning a fresh unknown state.
func (m *MemoryStore) Reset(profileID int64, providerFormat string) ProfileStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	fresh := ProfileStats{
		ProfileID: profileID,
		Status:    StatusUnknown,
		Strategy:  DefaultStrategyForFormat(providerFormat),
	}
	m.stats[profileID] = fresh
	return fresh
}

// AdaptiveConfig configures an AdaptiveProvider wrapper for a specific LLM profile.
type AdaptiveConfig struct {
	ProfileID          int64
	ProviderFormat     string
	SystemInstructions string
	Store              Store
	Now                func() time.Time
}

// AdaptiveProvider wraps an underlying llm.Provider to:
// 1. Prepend per-profile SystemInstructions first in deterministic order (without duplicating).
// 2. Maintain deterministic tool ordering and cache-friendly DynamicBoundary.
// 3. Observe raw API cache metrics (streaming & non-streaming) via CallObserver.
// 4. Gracefully fall back once if a provider rejects native cache parameters.
type AdaptiveProvider struct {
	inner llm.Provider
	cfg   AdaptiveConfig
	mu    sync.RWMutex
	sys   string
}

// Wrap wraps provider with adaptive prompt caching and per-profile system instructions.
func Wrap(inner llm.Provider, cfg AdaptiveConfig) *AdaptiveProvider {
	if inner == nil {
		return nil
	}
	if cfg.Store == nil {
		cfg.Store = NewMemoryStore()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &AdaptiveProvider{
		inner: inner,
		cfg:   cfg,
		sys:   strings.TrimSpace(cfg.SystemInstructions),
	}
}

// SetSystemInstructions updates the profile's custom system instructions safely at runtime.
func (p *AdaptiveProvider) SetSystemInstructions(s string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.sys = strings.TrimSpace(s)
	p.mu.Unlock()
}

// Stats returns the current cached observation state for this provider's profile.
func (p *AdaptiveProvider) Stats() ProfileStats {
	if p == nil || p.cfg.Store == nil {
		return ProfileStats{Status: StatusUnknown}
	}
	if st, ok := p.cfg.Store.Get(p.cfg.ProfileID); ok {
		return st
	}
	return ProfileStats{
		ProfileID: p.cfg.ProfileID,
		Status:    StatusUnknown,
		Strategy:  DefaultStrategyForFormat(p.cfg.ProviderFormat),
	}
}

func (p *AdaptiveProvider) now() time.Time {
	if p.cfg.Now != nil {
		return p.cfg.Now()
	}
	return time.Now()
}

func (p *AdaptiveProvider) prepareRequest(ctx context.Context, req llm.CompletionRequest, forceStandard bool) llm.CompletionRequest {
	out := req

	// Clone System slice and prepend per-profile SystemInstructions first (never omitting role/runtime blocks).
	sysCopy := make([]string, len(req.System))
	copy(sysCopy, req.System)

	p.mu.RLock()
	profileInst := p.sys
	p.mu.RUnlock()

	if profileInst != "" && !IsWithoutProfileInstructions(ctx) {
		if len(sysCopy) == 0 || strings.TrimSpace(sysCopy[0]) != profileInst {
			sysCopy = append([]string{profileInst}, sysCopy...)
			if out.DynamicBoundary > 0 {
				out.DynamicBoundary++
			}
		}
	}
	out.System = sysCopy

	// Ensure deterministic tool schema ordering for prompt-cache prefix stability.
	if len(req.Tools) > 1 {
		toolsCopy := make([]llm.ToolSchema, len(req.Tools))
		copy(toolsCopy, req.Tools)
		slices.SortStableFunc(toolsCopy, func(a, b llm.ToolSchema) int {
			return cmp.Compare(a.Name, b.Name)
		})
		out.Tools = toolsCopy
	}

	st := p.Stats()
	if forceStandard || st.Status == StatusNativeUnsupported || st.Strategy == StrategyStandard {
		out.DynamicBoundary = 0
		return out
	}

	// For Anthropic native caching, if caller has system segments and hasn't explicitly split
	// static vs dynamic segments, treat all system blocks as static cacheable prefix.
	if strings.EqualFold(p.cfg.ProviderFormat, "anthropic") && out.DynamicBoundary == 0 && len(out.System) > 0 {
		out.DynamicBoundary = len(out.System)
	}
	return out
}

func (p *AdaptiveProvider) recordResult(obs *CallObserver, fallbackUsage llm.Usage) {
	if p == nil || p.cfg.Store == nil {
		return
	}
	if u, ok := obs.Snapshot(); ok {
		p.cfg.Store.Record(p.cfg.ProfileID, p.cfg.ProviderFormat, u, p.now())
		return
	}
	// Fallback for non-HTTP adapters (e.g., OAuth CLIs or mocks bypassing quotaAwareTransport).
	if fallbackUsage.InputTokens > 0 || fallbackUsage.OutputTokens > 0 || fallbackUsage.CacheReadTokens > 0 || fallbackUsage.CacheWriteTokens > 0 {
		u := NormalizeFromNormaUsage(fallbackUsage, p.cfg.ProviderFormat)
		p.cfg.Store.Record(p.cfg.ProfileID, p.cfg.ProviderFormat, u, p.now())
		return
	}
	// Even without any usage object reported on success, mark observed metrics unavailable if previously unknown.
	p.cfg.Store.Record(p.cfg.ProfileID, p.cfg.ProviderFormat, NormalizedUsage{
		Presence:              PresenceAbsent,
		ProviderUsageReported: false,
	}, p.now())
}

// Complete executes a non-streaming request, observes usage, and handles native caching fallback.
func (p *AdaptiveProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	st := p.Stats()
	forceStd := st.Status == StatusNativeUnsupported || st.Strategy == StrategyStandard

	obs := NewCallObserver(p.cfg.ProviderFormat)
	prepared := p.prepareRequest(ctx, req, forceStd)
	msg, stop, usage, err := p.inner.Complete(WithCallObserver(ctx, obs), prepared)
	if err != nil {
		if !forceStd {
			if unsupported, _ := obs.NativeUnsupportedDetected(); unsupported {
				if u, ok := obs.Snapshot(); ok {
					p.cfg.Store.Record(p.cfg.ProfileID, p.cfg.ProviderFormat, u, p.now())
				}
				retryObs := NewCallObserver(p.cfg.ProviderFormat)
				retryReq := p.prepareRequest(ctx, req, true)
				msg, stop, usage, err = p.inner.Complete(WithCallObserver(ctx, retryObs), retryReq)
				if err == nil {
					p.recordResult(retryObs, usage)
				}
				return msg, stop, usage, err
			}
		}
		return msg, stop, usage, err
	}

	p.recordResult(obs, usage)
	return msg, stop, usage, nil
}

// Stream executes a streaming request, observes SSE usage events, and falls back once on early native-cache rejection.
func (p *AdaptiveProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		st := p.Stats()
		forceStd := st.Status == StatusNativeUnsupported || st.Strategy == StrategyStandard

		if !p.streamAttempt(ctx, req, forceStd, true, yield) {
			return
		}
	}
}

// streamAttempt runs one streaming pass; if allowFallback and an early native cache rejection occurs before
// any output delta is yielded downstream, it records StatusNativeUnsupported and transparently retries once.
func (p *AdaptiveProvider) streamAttempt(
	ctx context.Context,
	req llm.CompletionRequest,
	forceStd bool,
	allowFallback bool,
	yield func(llm.StreamEvent, error) bool,
) bool {
	obs := NewCallObserver(p.cfg.ProviderFormat)
	prepared := p.prepareRequest(ctx, req, forceStd)
	callCtx := WithCallObserver(ctx, obs)

	var fallbackUsage llm.Usage
	emittedContent := false
	needsFallback := false
	stoppedByConsumer := false

	for ev, err := range p.inner.Stream(callCtx, prepared) {
		if err != nil {
			if allowFallback && !forceStd && !emittedContent {
				if unsupported, _ := obs.NativeUnsupportedDetected(); unsupported {
					if u, ok := obs.Snapshot(); ok {
						p.cfg.Store.Record(p.cfg.ProfileID, p.cfg.ProviderFormat, u, p.now())
					}
					needsFallback = true
					break
				}
			}
			yield(ev, err)
			return false
		}

		fallbackUsage.Add(ev.Usage)
		if ev.Type == llm.SETextDelta || ev.Type == llm.SEThinkingDelta || ev.Type == llm.SEToolUseStart {
			emittedContent = true
		}

		if !yield(ev, nil) {
			stoppedByConsumer = true
			break
		}
	}

	if needsFallback {
		return p.streamAttempt(ctx, req, true, false, yield)
	}
	if !stoppedByConsumer {
		p.recordResult(obs, fallbackUsage)
	}
	return true
}
