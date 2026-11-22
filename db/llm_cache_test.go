package db

import (
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/llmcache"
)

func TestProfileSystemInstructionsAndIsolation(t *testing.T) {
	dsn := testDSN(t)
	d, err := Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	// Default empty SystemInstructions + two independent profiles for same model
	idA, err := d.SaveProfile(&LLMProfile{
		Name: "p-iso-a", Format: "openai", BaseURL: "https://llm.example/v1", Model: "test-model", APIKey: "test-key-A", Streaming: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, idA) })

	idB, err := d.SaveProfile(&LLMProfile{
		Name: "p-iso-b", Format: "openai", BaseURL: "https://llm.example/v1", Model: "test-model", APIKey: "test-key-B", Streaming: true,
		SystemInstructions: "Strict instruction for Profile B",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, idB) })

	pA, err := d.ProfileByID(idA)
	if err != nil {
		t.Fatal(err)
	}
	if pA.SystemInstructions != "" {
		t.Fatalf("expected empty default SystemInstructions, got %q", pA.SystemInstructions)
	}
	pB, err := d.ProfileByID(idB)
	if err != nil {
		t.Fatal(err)
	}
	if pB.SystemInstructions != "Strict instruction for Profile B" {
		t.Fatalf("unexpected Profile B instructions: %q", pB.SystemInstructions)
	}

	// Update A (with blank APIKey to preserve existing key), set instructions + manual rates
	inRate := 1.5
	outRate := 6.0
	pA.APIKey = ""
	pA.SystemInstructions = "Profile A custom\nline 2"
	pA.InputRatePer1M = &inRate
	pA.OutputRatePer1M = &outRate
	if _, err := d.SaveProfile(pA); err != nil {
		t.Fatal(err)
	}

	// Record hit on B only; verify A stays unknown and B retains hit + key isolation
	if _, err := d.RecordProfileCacheObservation(idB, "openai", llmcache.NormalizedUsage{
		ProviderUsageReported: true, InputTokens: 2633, UncachedInputTokens: 73, CachedReadTokens: 2560, OutputTokens: 2, Presence: llmcache.PresencePositive,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	gotA, _ := d.ProfileByID(idA)
	gotB, _ := d.ProfileByID(idB)
	if gotA.APIKey != "test-key-A" || gotA.SystemInstructions != "Profile A custom\nline 2" {
		t.Fatalf("keyless update failed on A: key=%q sys=%q", gotA.APIKey, gotA.SystemInstructions)
	}
	if gotA.InputRatePer1M == nil || *gotA.InputRatePer1M != 1.5 || gotA.CachedInputRatePer1M != nil {
		t.Fatalf("unexpected rates on A: in=%v cached=%v", gotA.InputRatePer1M, gotA.CachedInputRatePer1M)
	}
	if gotA.PromptCache.Status != llmcache.StatusUnknown || gotB.PromptCache.Status != llmcache.StatusObservedHit {
		t.Fatalf("profile isolation failed: A=%s B=%s", gotA.PromptCache.Status, gotB.PromptCache.Status)
	}

	// Clear instructions on A cleanly
	gotA.APIKey = ""
	gotA.SystemInstructions = ""
	if _, err := d.SaveProfile(gotA); err != nil {
		t.Fatal(err)
	}
	clearedA, _ := d.ProfileByID(idA)
	if clearedA.SystemInstructions != "" {
		t.Fatalf("expected cleared SystemInstructions, got %q", clearedA.SystemInstructions)
	}
}

func TestProfileCacheConcurrencyRestartPersistenceAndReset(t *testing.T) {
	dsn := testDSN(t)
	d, err := Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	id, err := d.SaveProfile(&LLMProfile{
		Name: "p-cache-conc", Format: "openai", BaseURL: "https://llm.example/v1", Model: "test-model", APIKey: "test-key",
		SystemInstructions: "Keep me after reset",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })

	store := NewPGCacheStore(d)
	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			store.Record(id, "openai", llmcache.NormalizedUsage{
				ProviderUsageReported: true,
				InputTokens:           2633,
				UncachedInputTokens:   73,
				CachedReadTokens:      2560,
				OutputTokens:          2,
				Presence:              llmcache.PresencePositive,
			}, time.Now())
		}()
	}
	wg.Wait()

	// Simulate ARTEX restart: new *DB connection & fresh PGCacheStore
	d2, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	storeAfterRestart := NewPGCacheStore(d2)
	st, ok := storeAfterRestart.Get(id)
	if !ok {
		t.Fatal("expected cache stats to persist across restart")
	}
	if st.Status != llmcache.StatusObservedHit || st.Hits != workers || st.CachedInputTokens != workers*2560 || st.UncachedInputTokens != workers*73 {
		t.Fatalf("concurrency/persistence mismatch: %+v", st)
	}

	// Reset cache stats only -> keeps SystemInstructions & APIKey intact!
	fresh := storeAfterRestart.Reset(id, "openai")
	if fresh.Status != llmcache.StatusUnknown || fresh.Hits != 0 || fresh.CachedInputTokens != 0 {
		t.Fatalf("expected reset to unknown zero stats, got %+v", fresh)
	}
	profAfter, err := d2.ProfileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if profAfter.SystemInstructions != "Keep me after reset" || profAfter.APIKey != "test-key" {
		t.Fatalf("reset erased profile settings: sys=%q key=%q", profAfter.SystemInstructions, profAfter.APIKey)
	}
}
