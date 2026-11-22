package db

import (
	"database/sql"
	"encoding/json"
	"math"
	"sync"
	"time"

	"github.com/Autumn-27/artex/llmcache"
)

func nullableRate(r *float64) any {
	if r == nil || math.IsNaN(*r) || math.IsInf(*r, 0) || *r < 0 {
		return nil
	}
	return *r
}

// GetProfileCacheStats returns the persisted prompt caching telemetry for profileID.
func (d *DB) GetProfileCacheStats(profileID int64) (llmcache.ProfileStats, error) {
	if profileID <= 0 {
		return llmcache.ProfileStats{Status: llmcache.StatusUnknown}, nil
	}
	p, err := d.ProfileByID(profileID)
	if err != nil {
		return llmcache.ProfileStats{}, err
	}
	return p.PromptCache, nil
}

// RecordProfileCacheObservation atomically updates prompt cache statistics on llm_profiles
// using row-level locking so concurrent worker calls never drop increments or clobber settings.
func (d *DB) RecordProfileCacheObservation(profileID int64, providerFormat string, obs llmcache.NormalizedUsage, now time.Time) (llmcache.ProfileStats, error) {
	if profileID <= 0 {
		return llmcache.ApplyObservation(llmcache.ProfileStats{
			ProfileID: profileID,
			Status:    llmcache.StatusUnknown,
			Strategy:  llmcache.DefaultStrategyForFormat(providerFormat),
		}, obs, providerFormat, now), nil
	}
	tx, err := d.Begin()
	if err != nil {
		return llmcache.ProfileStats{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	var (
		dbFormat                   string
		cacheStatus, cacheStrategy string
		st                         llmcache.ProfileStats
		lastHit, lastObs           sql.NullTime
		lastPres                   string
		metaRaw                    []byte
	)
	err = tx.QueryRow(`SELECT format,
		COALESCE(cache_status,'unknown'),
		COALESCE(cache_strategy,''),
		COALESCE(cache_hits,0),
		COALESCE(cache_misses,0),
		COALESCE(cache_metrics_unavailable,0),
		COALESCE(cache_requests_with_usage,0),
		COALESCE(cache_cached_input_tokens,0),
		COALESCE(cache_uncached_input_tokens,0),
		COALESCE(cache_write_tokens,0),
		COALESCE(cache_output_tokens,0),
		last_cache_hit_at,
		last_cache_observed_at,
		COALESCE(last_request_input_tokens,0),
		COALESCE(last_request_cached_tokens,0),
		COALESCE(last_request_uncached_tokens,0),
		COALESCE(last_cache_presence,''),
		COALESCE(cache_provider_meta,'{}')
	FROM llm_profiles WHERE id=$1 FOR UPDATE`, profileID).Scan(
		&dbFormat,
		&cacheStatus,
		&cacheStrategy,
		&st.Hits,
		&st.Misses,
		&st.MetricsUnavailableCount,
		&st.RequestsWithUsage,
		&st.CachedInputTokens,
		&st.UncachedInputTokens,
		&st.CacheWriteTokens,
		&st.OutputTokens,
		&lastHit,
		&lastObs,
		&st.LastRequestInputTokens,
		&st.LastRequestCachedTokens,
		&st.LastRequestUncached,
		&lastPres,
		&metaRaw,
	)
	if err == sql.ErrNoRows {
		return llmcache.ProfileStats{}, ErrLLMProfileNotFound
	}
	if err != nil {
		return llmcache.ProfileStats{}, err
	}

	effFormat := providerFormat
	if effFormat == "" {
		effFormat = dbFormat
	}
	st.ProfileID = profileID
	st.Status = llmcache.CacheStatus(cacheStatus)
	st.Strategy = llmcache.Strategy(cacheStrategy)
	st.LastPresence = llmcache.MetricPresence(lastPres)
	if lastHit.Valid {
		t := lastHit.Time.UTC()
		st.LastCacheHitAt = &t
	}
	if lastObs.Valid {
		t := lastObs.Time.UTC()
		st.LastObservedAt = &t
	}
	if len(metaRaw) > 0 && string(metaRaw) != "{}" {
		_ = json.Unmarshal(metaRaw, &st.ProviderMeta)
	}

	updated := llmcache.ApplyObservation(st, obs, effFormat, now)
	metaJSON := []byte("{}")
	if len(updated.ProviderMeta) > 0 {
		if b, mErr := json.Marshal(updated.ProviderMeta); mErr == nil {
			metaJSON = b
		}
	}

	_, err = tx.Exec(`UPDATE llm_profiles SET
		cache_status=$2,
		cache_strategy=$3,
		cache_hits=$4,
		cache_misses=$5,
		cache_metrics_unavailable=$6,
		cache_requests_with_usage=$7,
		cache_cached_input_tokens=$8,
		cache_uncached_input_tokens=$9,
		cache_write_tokens=$10,
		cache_output_tokens=$11,
		last_cache_hit_at=$12,
		last_cache_observed_at=$13,
		last_request_input_tokens=$14,
		last_request_cached_tokens=$15,
		last_request_uncached_tokens=$16,
		last_cache_presence=$17,
		cache_provider_meta=$18
	WHERE id=$1`,
		profileID,
		string(updated.Status),
		string(updated.Strategy),
		updated.Hits,
		updated.Misses,
		updated.MetricsUnavailableCount,
		updated.RequestsWithUsage,
		updated.CachedInputTokens,
		updated.UncachedInputTokens,
		updated.CacheWriteTokens,
		updated.OutputTokens,
		updated.LastCacheHitAt,
		updated.LastObservedAt,
		updated.LastRequestInputTokens,
		updated.LastRequestCachedTokens,
		updated.LastRequestUncached,
		string(updated.LastPresence),
		string(metaJSON),
	)
	if err != nil {
		return llmcache.ProfileStats{}, err
	}
	if err := tx.Commit(); err != nil {
		return llmcache.ProfileStats{}, err
	}
	return updated, nil
}

// ResetProfileCacheStats clears accumulated prompt cache telemetry for profileID
// while preserving profile configuration, API credentials, and SystemInstructions.
func (d *DB) ResetProfileCacheStats(profileID int64) (llmcache.ProfileStats, error) {
	tx, err := d.Begin()
	if err != nil {
		return llmcache.ProfileStats{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	var format string
	err = tx.QueryRow(`SELECT format FROM llm_profiles WHERE id=$1 FOR UPDATE`, profileID).Scan(&format)
	if err == sql.ErrNoRows {
		return llmcache.ProfileStats{}, ErrLLMProfileNotFound
	}
	if err != nil {
		return llmcache.ProfileStats{}, err
	}

	strategy := llmcache.DefaultStrategyForFormat(format)
	_, err = tx.Exec(`UPDATE llm_profiles SET
		cache_status='unknown',
		cache_strategy=$2,
		cache_hits=0,
		cache_misses=0,
		cache_metrics_unavailable=0,
		cache_requests_with_usage=0,
		cache_cached_input_tokens=0,
		cache_uncached_input_tokens=0,
		cache_write_tokens=0,
		cache_output_tokens=0,
		last_cache_hit_at=NULL,
		last_cache_observed_at=NULL,
		last_request_input_tokens=0,
		last_request_cached_tokens=0,
		last_request_uncached_tokens=0,
		last_cache_presence='',
		cache_provider_meta='{}'
	WHERE id=$1`, profileID, string(strategy))
	if err != nil {
		return llmcache.ProfileStats{}, err
	}
	if err := tx.Commit(); err != nil {
		return llmcache.ProfileStats{}, err
	}
	return llmcache.ProfileStats{
		ProfileID: profileID,
		Status:    llmcache.StatusUnknown,
		Strategy:  strategy,
	}, nil
}

// PGCacheStore implements llmcache.Store backed by PostgreSQL (llm_profiles)
// with an in-memory cache so reads during request preparation have zero DB latency.
type PGCacheStore struct {
	pg  *DB
	mem *llmcache.MemoryStore

	mu     sync.RWMutex
	cached map[int64]llmcache.ProfileStats
}

// NewPGCacheStore wraps d with an in-memory synchronized cache store.
func NewPGCacheStore(d *DB) *PGCacheStore {
	return &PGCacheStore{
		pg:     d,
		mem:    llmcache.NewMemoryStore(),
		cached: make(map[int64]llmcache.ProfileStats),
	}
}

func (s *PGCacheStore) Get(profileID int64) (llmcache.ProfileStats, bool) {
	if profileID <= 0 || s.pg == nil {
		return s.mem.Get(profileID)
	}
	s.mu.RLock()
	st, ok := s.cached[profileID]
	s.mu.RUnlock()
	if ok {
		return st, true
	}
	loaded, err := s.pg.GetProfileCacheStats(profileID)
	if err != nil {
		return llmcache.ProfileStats{}, false
	}
	s.mu.Lock()
	s.cached[profileID] = loaded
	s.mu.Unlock()
	return loaded, true
}

func (s *PGCacheStore) Record(profileID int64, providerFormat string, obs llmcache.NormalizedUsage, now time.Time) llmcache.ProfileStats {
	if profileID <= 0 || s.pg == nil {
		return s.mem.Record(profileID, providerFormat, obs, now)
	}
	updated, err := s.pg.RecordProfileCacheObservation(profileID, providerFormat, obs, now)
	if err != nil {
		return s.mem.Record(profileID, providerFormat, obs, now)
	}
	s.mu.Lock()
	s.cached[profileID] = updated
	s.mu.Unlock()
	return updated
}

func (s *PGCacheStore) Reset(profileID int64, providerFormat string) llmcache.ProfileStats {
	if profileID <= 0 || s.pg == nil {
		return s.mem.Reset(profileID, providerFormat)
	}
	fresh, err := s.pg.ResetProfileCacheStats(profileID)
	if err != nil {
		return s.mem.Reset(profileID, providerFormat)
	}
	s.mu.Lock()
	s.cached[profileID] = fresh
	s.mu.Unlock()
	return fresh
}

// Invalidate drops any cached entry for profileID so the next Get reloads from Postgres.
func (s *PGCacheStore) Invalidate(profileID int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.cached, profileID)
	s.mu.Unlock()
}
