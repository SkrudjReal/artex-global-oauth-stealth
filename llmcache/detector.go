package llmcache

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// CacheStatus describes the observed prompt caching capability state for a profile.
type CacheStatus string

const (
	StatusUnknown            CacheStatus = "unknown"
	StatusObservedHit        CacheStatus = "observed_hit"
	StatusObservedMiss       CacheStatus = "observed_miss"
	StatusMetricsUnavailable CacheStatus = "metrics_unavailable"
	StatusNativeUnsupported  CacheStatus = "native_unsupported"
)

// Strategy describes how requests should be shaped for a profile.
type Strategy string

const (
	StrategyAutomatic Strategy = "automatic"
	StrategyNative    Strategy = "native"
	StrategyStandard  Strategy = "standard"
)

// MetricPresence captures the exact wire state of a cache metric field.
type MetricPresence string

const (
	PresenceAbsent   MetricPresence = "absent"
	PresenceNull     MetricPresence = "null"
	PresenceZero     MetricPresence = "zero"
	PresencePositive MetricPresence = "positive"
	PresenceInvalid  MetricPresence = "invalid"
)

// NormalizedUsage holds provider-normalized token and cache metrics extracted from
// raw HTTP responses (JSON or SSE), preserving extra provider-specific fields.
type NormalizedUsage struct {
	InputTokens           int            `json:"input_tokens"`
	UncachedInputTokens   int            `json:"uncached_input_tokens"`
	CachedReadTokens      int            `json:"cached_read_tokens"`
	CacheWriteTokens      int            `json:"cache_write_tokens"`
	OutputTokens          int            `json:"output_tokens"`
	Presence              MetricPresence `json:"presence"`
	ProviderUsageReported bool           `json:"provider_usage_reported"`
	CacheAnomaly          bool           `json:"cache_anomaly,omitempty"`
	Extra                 map[string]any `json:"extra,omitempty"`
	NativeError           bool           `json:"native_error,omitempty"`
	NativeErrorReason     string         `json:"native_error_reason,omitempty"`
}

// ProfileStats stores persistent observed cache telemetry and configuration for an LLM profile.
type ProfileStats struct {
	ProfileID               int64          `json:"profile_id"`
	Status                  CacheStatus    `json:"status"`
	Strategy                Strategy       `json:"strategy"`
	Hits                    int64          `json:"hits"`
	Misses                  int64          `json:"misses"`
	MetricsUnavailableCount int64          `json:"metrics_unavailable_count"`
	RequestsWithUsage       int64          `json:"requests_with_usage"`
	CachedInputTokens       int64          `json:"cached_input_tokens"`
	UncachedInputTokens     int64          `json:"uncached_input_tokens"`
	CacheWriteTokens        int64          `json:"cache_write_tokens"`
	OutputTokens            int64          `json:"output_tokens"`
	LastCacheHitAt          *time.Time     `json:"last_cache_hit_at,omitempty"`
	LastObservedAt          *time.Time     `json:"last_observed_at,omitempty"`
	LastRequestInputTokens  int            `json:"last_request_input_tokens"`
	LastRequestCachedTokens int            `json:"last_request_cached_tokens"`
	LastRequestUncached     int            `json:"last_request_uncached_tokens"`
	LastPresence            MetricPresence `json:"last_presence,omitempty"`
	ProviderMeta            map[string]any `json:"provider_meta,omitempty"`
}

// HitRatio returns cached_input_tokens / (cached + uncached), or 0 when no input tokens exist.
func (s ProfileStats) HitRatio() float64 {
	total := s.CachedInputTokens + s.UncachedInputTokens
	if total <= 0 {
		return 0
	}
	return float64(s.CachedInputTokens) / float64(total)
}

// ApplyObservation updates stats with a single normalized request observation and returns the updated state.
func ApplyObservation(stats ProfileStats, obs NormalizedUsage, providerFormat string, now time.Time) ProfileStats {
	if stats.Status == "" {
		stats.Status = StatusUnknown
	}
	if stats.Strategy == "" {
		stats.Strategy = DefaultStrategyForFormat(providerFormat)
	}
	ts := now.UTC()
	stats.LastObservedAt = &ts

	if obs.NativeError {
		stats.Status = StatusNativeUnsupported
		stats.Strategy = StrategyStandard
		if stats.ProviderMeta == nil {
			stats.ProviderMeta = make(map[string]any)
		}
		if obs.NativeErrorReason != "" {
			stats.ProviderMeta["native_error_reason"] = obs.NativeErrorReason
		}
		return stats
	}

	if obs.ProviderUsageReported {
		stats.RequestsWithUsage++
		stats.LastRequestInputTokens = obs.InputTokens
		stats.LastRequestCachedTokens = obs.CachedReadTokens
		stats.LastRequestUncached = obs.UncachedInputTokens
		stats.CachedInputTokens += int64(obs.CachedReadTokens)
		stats.UncachedInputTokens += int64(obs.UncachedInputTokens)
		stats.CacheWriteTokens += int64(obs.CacheWriteTokens)
		stats.OutputTokens += int64(obs.OutputTokens)
	}
	stats.LastPresence = obs.Presence

	switch obs.Presence {
	case PresencePositive:
		stats.Hits++
		stats.LastCacheHitAt = &ts
		stats.Status = StatusObservedHit
	case PresenceZero:
		stats.Misses++
		if stats.Status != StatusObservedHit && stats.Status != StatusNativeUnsupported {
			stats.Status = StatusObservedMiss
		}
	default:
		stats.MetricsUnavailableCount++
		if stats.Status == "" || stats.Status == StatusUnknown {
			stats.Status = StatusMetricsUnavailable
		}
	}

	if len(obs.Extra) > 0 {
		if stats.ProviderMeta == nil {
			stats.ProviderMeta = make(map[string]any)
		}
		for k, v := range obs.Extra {
			stats.ProviderMeta[k] = v
		}
	}
	if obs.CacheAnomaly {
		if stats.ProviderMeta == nil {
			stats.ProviderMeta = make(map[string]any)
		}
		stats.ProviderMeta["cache_anomaly_detected"] = true
	}

	stats.Strategy = SelectStrategy(stats.Status, providerFormat, obs.InputTokens)
	return stats
}

// DefaultStrategyForFormat returns the safe initial adaptive strategy for a provider format
// without assuming hard-coded model vendors support or lack caching.
func DefaultStrategyForFormat(providerFormat string) Strategy {
	switch strings.ToLower(strings.TrimSpace(providerFormat)) {
	case "anthropic":
		return StrategyNative
	case "openai", "openai-responses":
		return StrategyAutomatic
	default:
		return StrategyStandard
	}
}

// SelectStrategy chooses the compatible caching strategy based on observed state and adapter capabilities.
func SelectStrategy(status CacheStatus, providerFormat string, _ int) Strategy {
	if status == StatusNativeUnsupported {
		return StrategyStandard
	}
	return DefaultStrategyForFormat(providerFormat)
}

// DetectNativeUnsupportedError inspects non-2xx HTTP bodies for documented unsupported cache parameter errors.
func DetectNativeUnsupportedError(statusCode int, body []byte) (bool, string) {
	if statusCode < 400 || statusCode >= 500 || len(body) == 0 {
		return false, ""
	}
	lower := strings.ToLower(string(body))
	cacheParamHints := []string{
		"cache_control",
		"prompt_cache_key",
		"ephemeral",
		"cache_creation",
		"prompt caching",
		"prompt_cache",
	}
	errHints := []string{
		"unsupported",
		"unknown",
		"invalid",
		"not allowed",
		"unrecognized",
		"extra inputs are not permitted",
		"does not support",
		"unexpected",
	}
	matchedParam := ""
	for _, p := range cacheParamHints {
		if strings.Contains(lower, p) {
			matchedParam = p
			break
		}
	}
	if matchedParam == "" {
		return false, ""
	}
	for _, e := range errHints {
		if strings.Contains(lower, e) {
			return true, strings.TrimSpace(truncateReason(string(body), 240))
		}
	}
	return false, ""
}

func truncateReason(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// DetectFromRawResponse inspects a complete non-streaming HTTP response payload.
func DetectFromRawResponse(statusCode int, body []byte, providerFormat string) NormalizedUsage {
	if unsupported, reason := DetectNativeUnsupportedError(statusCode, body); unsupported {
		return NormalizedUsage{
			Presence:          PresenceAbsent,
			NativeError:       true,
			NativeErrorReason: reason,
		}
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return NormalizedUsage{Presence: PresenceAbsent}
	}
	return detectFromDecodedObject(root, providerFormat)
}

// DetectFromSSELine parses a single SSE `data: ...` payload (or raw JSON line) and updates usage
// when a frame carrying usage metadata arrives. Returns (usage, foundUsage).
func DetectFromSSELine(dataPayload []byte, providerFormat string) (NormalizedUsage, bool) {
	trimmed := strings.TrimSpace(string(dataPayload))
	if trimmed == "" || trimmed == "[DONE]" {
		return NormalizedUsage{}, false
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(trimmed), &root); err != nil {
		return NormalizedUsage{}, false
	}
	u := detectFromDecodedObject(root, providerFormat)
	if !u.ProviderUsageReported {
		return NormalizedUsage{}, false
	}
	return u, true
}

// MergeSSEUsage merges sequential SSE usage events (e.g. Anthropic message_start + message_delta).
func MergeSSEUsage(prev, next NormalizedUsage) NormalizedUsage {
	if !prev.ProviderUsageReported {
		return next
	}
	if !next.ProviderUsageReported {
		return prev
	}
	merged := prev
	if next.InputTokens > 0 {
		merged.InputTokens = next.InputTokens
	}
	if next.OutputTokens > 0 {
		merged.OutputTokens = next.OutputTokens
	}
	if next.CachedReadTokens > 0 || (next.Presence != PresenceAbsent && prev.Presence == PresenceAbsent) {
		merged.CachedReadTokens = next.CachedReadTokens
		merged.Presence = next.Presence
	}
	if next.CacheWriteTokens > 0 {
		merged.CacheWriteTokens = next.CacheWriteTokens
	}
	// Recompute uncached cleanly
	if merged.CachedReadTokens > merged.InputTokens && merged.InputTokens > 0 {
		merged.CacheAnomaly = true
		merged.UncachedInputTokens = 0
	} else {
		merged.UncachedInputTokens = maxInt(0, merged.InputTokens-merged.CachedReadTokens-merged.CacheWriteTokens)
		if prev.Presence != PresenceAbsent && merged.Presence != PresencePositive && next.Presence == PresenceZero {
			merged.Presence = PresenceZero
		}
	}
	if len(next.Extra) > 0 {
		if merged.Extra == nil {
			merged.Extra = make(map[string]any)
		}
		for k, v := range next.Extra {
			merged.Extra[k] = v
		}
	}
	return merged
}

func detectFromDecodedObject(root map[string]any, providerFormat string) NormalizedUsage {
	usageObj := extractUsageObject(root)
	if usageObj == nil {
		return NormalizedUsage{Presence: PresenceAbsent}
	}
	return detectFromUsageMap(usageObj, providerFormat)
}

func extractUsageObject(root map[string]any) map[string]any {
	if u, ok := asMap(root["usage"]); ok {
		return u
	}
	// OpenAI Responses streaming puts usage under response.usage in response.completed / response.incomplete
	if resp, ok := asMap(root["response"]); ok {
		if u, ok := asMap(resp["usage"]); ok {
			return u
		}
	}
	// Anthropic streaming message_start puts usage under message.usage
	if msg, ok := asMap(root["message"]); ok {
		if u, ok := asMap(msg["usage"]); ok {
			return u
		}
	}
	return nil
}

func detectFromUsageMap(usage map[string]any, providerFormat string) NormalizedUsage {
	out := NormalizedUsage{
		ProviderUsageReported: true,
		Presence:              PresenceAbsent,
	}

	isAnthropic := strings.EqualFold(providerFormat, "anthropic") ||
		hasAnyKey(usage, "cache_read_input_tokens", "cache_creation_input_tokens", "cache_creation")

	if isAnthropic {
		rawIn, _ := parseNonNegInt(usage["input_tokens"])
		outTokens, _ := parseNonNegInt(usage["output_tokens"])
		cacheRead, readPres := parseMetricField(usage, "cache_read_input_tokens")
		cacheWrite, writePres := parseMetricField(usage, "cache_creation_input_tokens")

		// Check if total > 0 without anomaly: Anthropic reports uncached in `input_tokens`
		out.CachedReadTokens = cacheRead
		out.CacheWriteTokens = cacheWrite
		out.UncachedInputTokens = maxInt(0, rawIn)
		out.InputTokens = out.UncachedInputTokens + out.CachedReadTokens + out.CacheWriteTokens
		out.OutputTokens = maxInt(0, outTokens)

		out.Presence = readPres
		if readPres == PresenceAbsent && (writePres == PresenceZero || writePres == PresencePositive) {
			// API reported cache_creation_input_tokens without cache_read_input_tokens -> 0 cache read.
			out.Presence = PresenceZero
		}
		out.Extra = extractExtraFields(usage, "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens")
		return out
	}

	// OpenAI Responses vs OpenAI Chat Completions (and compatible endpoints)
	promptTokens, hasPrompt := parseNonNegInt(usage["prompt_tokens"])
	inputTokens, hasInput := parseNonNegInt(usage["input_tokens"])
	totalInput := promptTokens
	if !hasPrompt && hasInput {
		totalInput = inputTokens
	}
	if totalInput < 0 {
		out.CacheAnomaly = true
		totalInput = 0
	}

	compTokens, hasComp := parseNonNegInt(usage["completion_tokens"])
	respOutTokens, hasRespOut := parseNonNegInt(usage["output_tokens"])
	outTokens := compTokens
	if !hasComp && hasRespOut {
		outTokens = respOutTokens
	}
	if outTokens < 0 {
		outTokens = 0
	}

	var cached int
	pres := PresenceAbsent
	var extraDetail map[string]any

	if details, ok := asMap(usage["prompt_tokens_details"]); ok {
		cached, pres = parseMetricField(details, "cached_tokens")
		extraDetail = extractExtraFields(details, "cached_tokens")
	} else if details, ok := asMap(usage["input_tokens_details"]); ok {
		cached, pres = parseMetricField(details, "cached_tokens")
		extraDetail = extractExtraFields(details, "cached_tokens")
	} else if _, hasKey := usage["prompt_tokens_details"]; hasKey && usage["prompt_tokens_details"] == nil {
		pres = PresenceNull
	} else if _, hasKey := usage["input_tokens_details"]; hasKey && usage["input_tokens_details"] == nil {
		pres = PresenceNull
	}

	// Preserve DeepSeek/compatible top-level cache fields if standard detail field is absent
	if pres == PresenceAbsent {
		if v, p := parseMetricField(usage, "prompt_cache_hit_tokens"); p != PresenceAbsent {
			cached, pres = v, p
		}
	}

	out.InputTokens = totalInput
	out.OutputTokens = outTokens
	out.CachedReadTokens = maxInt(0, cached)
	out.Presence = pres
	if pres == PresenceInvalid {
		out.CacheAnomaly = true
	}
	if out.CachedReadTokens > out.InputTokens && out.InputTokens > 0 {
		out.CacheAnomaly = true
		out.UncachedInputTokens = 0
	} else if out.CachedReadTokens > out.InputTokens {
		out.CacheAnomaly = true
		out.UncachedInputTokens = 0
	} else {
		out.UncachedInputTokens = maxInt(0, out.InputTokens-out.CachedReadTokens)
	}

	extra := extractExtraFields(usage,
		"prompt_tokens", "completion_tokens", "total_tokens",
		"input_tokens", "output_tokens",
		"prompt_tokens_details", "input_tokens_details",
	)
	for k, v := range extraDetail {
		if extra == nil {
			extra = make(map[string]any)
		}
		extra["details."+k] = v
	}
	if len(extra) > 0 {
		out.Extra = extra
	}
	return out
}

// NormalizeFromNormaUsage converts norma's fallback llm.Usage into NormalizedUsage when raw HTTP bodies
// were not intercepted (e.g., OAuth CLI adapters). Because norma collapses absent/null/0 into 0,
// fallback conversion marks CacheReadTokens == 0 as PresenceAbsent rather than asserting a confirmed miss.
func NormalizeFromNormaUsage(u llm.Usage, providerFormat string) NormalizedUsage {
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 {
		return NormalizedUsage{Presence: PresenceAbsent}
	}
	uncached := u.InputTokens - u.CacheReadTokens - u.CacheWriteTokens
	anomaly := false
	if uncached < 0 {
		anomaly = true
		uncached = 0
	}
	pres := PresenceAbsent
	if u.CacheReadTokens > 0 {
		pres = PresencePositive
	}
	return NormalizedUsage{
		InputTokens:           maxInt(0, u.InputTokens),
		UncachedInputTokens:   uncached,
		CachedReadTokens:      maxInt(0, u.CacheReadTokens),
		CacheWriteTokens:      maxInt(0, u.CacheWriteTokens),
		OutputTokens:          maxInt(0, u.OutputTokens),
		Presence:              pres,
		ProviderUsageReported: true,
		CacheAnomaly:          anomaly,
	}
}

func combinePresence(a, b MetricPresence) MetricPresence {
	if a == PresencePositive || b == PresencePositive {
		return PresencePositive
	}
	if a == PresenceInvalid || b == PresenceInvalid {
		return PresenceInvalid
	}
	if a == PresenceZero || b == PresenceZero {
		return PresenceZero
	}
	if a == PresenceNull || b == PresenceNull {
		return PresenceNull
	}
	return PresenceAbsent
}

func parseMetricField(m map[string]any, key string) (int, MetricPresence) {
	raw, exists := m[key]
	if !exists {
		return 0, PresenceAbsent
	}
	if raw == nil {
		return 0, PresenceNull
	}
	n, ok := parseIntAny(raw)
	if !ok || n < 0 {
		return 0, PresenceInvalid
	}
	if n == 0 {
		return 0, PresenceZero
	}
	return n, PresencePositive
}

func parseNonNegInt(raw any) (int, bool) {
	if raw == nil {
		return 0, false
	}
	n, ok := parseIntAny(raw)
	if !ok {
		return 0, false
	}
	return n, true
}

func parseIntAny(raw any) (int, bool) {
	switch v := raw.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v > float64(math.MaxInt32) || v < float64(math.MinInt32) {
			return 0, false
		}
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}

func asMap(v any) (map[string]any, bool) {
	if v == nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

func hasAnyKey(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

func extractExtraFields(m map[string]any, skip ...string) map[string]any {
	skipSet := make(map[string]struct{}, len(skip))
	for _, k := range skip {
		skipSet[k] = struct{}{}
	}
	var extra map[string]any
	for k, v := range m {
		if _, ok := skipSet[k]; ok {
			continue
		}
		if extra == nil {
			extra = make(map[string]any)
		}
		extra[k] = v
	}
	return extra
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
