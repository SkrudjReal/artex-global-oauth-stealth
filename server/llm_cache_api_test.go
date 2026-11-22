package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func openServerTestPG(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("ARTEX_PG_DSN unset — skipping PG server API test")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	return pg
}

func TestServerPromptCacheAPIAndEndToEndProvider(t *testing.T) {
	pg := openServerTestPG(t)

	var reqCount atomic.Int32
	var receivedSystem []string
	mockAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &p)
		for _, m := range p.Messages {
			if m.Role == "system" {
				receivedSystem = append(receivedSystem, m.Content)
			}
		}
		n := reqCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(`{"id":"1","choices":[{"index":0,"message":{"role":"assistant","content":"r1"},"finish_reason":"stop"}],"usage":{"completion_tokens":2,"prompt_tokens":2633,"total_tokens":2635}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"2","choices":[{"index":0,"message":{"role":"assistant","content":"r2"},"finish_reason":"stop"}],"usage":{"completion_tokens":2,"prompt_tokens":2633,"prompt_tokens_details":{"cached_tokens":2560},"total_tokens":2635}}`))
	}))
	defer mockAPI.Close()

	s := &Server{
		ctx:           context.Background(),
		m:             &Manager{pg: pg},
		provByProfile: map[int64]*provEntry{},
	}

	// 1. Create profile via pgSaveProfile with SystemInstructions & uncached-only rates (no cached discount confirmed)
	inRate := 2.0
	outRate := 8.0
	createBody, _ := json.Marshal(map[string]any{
		"name":                "srv-cache-test",
		"format":              "openai",
		"base_url":            mockAPI.URL,
		"model":               "glm-5.3",
		"api_key":             "sk-srv-test",
		"streaming":           false,
		"system_instructions": "Model custom prompt line 1",
		"input_rate_per_1m":   inRate,
		"output_rate_per_1m":  outRate,
	})
	rec := httptest.NewRecorder()
	s.pgSaveProfile(rec, httptest.NewRequest(http.MethodPost, "/api/llm/profiles", bytes.NewReader(createBody)))
	if rec.Code != 200 {
		t.Fatalf("pgSaveProfile failed: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var saveOut struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &saveOut)
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, saveOut.ID) })

	// 2. Execute two real calls via providerForProfile
	prov, _, ok := s.providerForProfile(saveOut.ID)
	if !ok {
		t.Fatal("expected providerForProfile to succeed")
	}
	req := llm.CompletionRequest{
		System:   []string{"ARTEX Role Prompt"},
		Messages: []llm.Message{llm.UserText("hi")},
	}
	if _, _, _, err := prov.Complete(context.Background(), req); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if _, _, _, err := prov.Complete(context.Background(), req); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if len(receivedSystem) != 2 || !strings.HasPrefix(receivedSystem[0], "Model custom prompt line 1") {
		t.Fatalf("expected profile SystemInstructions prepended first on both calls, got %#v", receivedSystem)
	}

	// 3. Verify pgListProfiles DTO exposes observed_hit + unconfirmed discount (never guessed!)
	listRec := httptest.NewRecorder()
	s.pgListProfiles(listRec, httptest.NewRequest(http.MethodGet, "/api/llm/profiles", nil))
	if listRec.Code != 200 {
		t.Fatalf("pgListProfiles failed: %d", listRec.Code)
	}
	var listResp struct {
		Profiles []LLMProfileDTO `json:"profiles"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	var found *LLMProfileDTO
	wantID := strconv.FormatInt(saveOut.ID, 10)
	for i := range listResp.Profiles {
		if listResp.Profiles[i].ID == wantID {
			found = &listResp.Profiles[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("profile %s missing in list", wantID)
	}
	if found.PromptCache.Status != "observed_hit" || found.PromptCache.CachedInputTokens != 2560 {
		t.Fatalf("unexpected DTO cache stats: %+v", found.PromptCache)
	}
	if found.PromptCacheCost.DiscountStatus != "unknown" || found.PromptCacheCost.TotalCostUSD != nil {
		t.Fatalf("must report unknown discount when cached_input_rate_per_1m is unset: %+v", found.PromptCacheCost)
	}

	// 4. Reset cache stats via HTTP handler -> status becomes unknown while SystemInstructions remain!
	resetReq := httptest.NewRequest(http.MethodPost, "/api/llm/profiles/"+wantID+"/cache-reset", nil)
	resetReq.SetPathValue("id", wantID)
	resetRec := httptest.NewRecorder()
	s.pgResetProfileCache(resetRec, resetReq)
	if resetRec.Code != 200 {
		t.Fatalf("pgResetProfileCache failed: %d %s", resetRec.Code, resetRec.Body.String())
	}
	after, err := pg.ProfileByID(saveOut.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.PromptCache.Status != "unknown" || after.PromptCache.CachedInputTokens != 0 {
		t.Fatalf("expected reset stats, got %+v", after.PromptCache)
	}
	if after.SystemInstructions != "Model custom prompt line 1" {
		t.Fatalf("reset unexpectedly wiped SystemInstructions: %q", after.SystemInstructions)
	}
}

func TestWireAgentAugmentDeterministicDeferredOrder(t *testing.T) {
	pg := openServerTestPG(t)
	hostToolsFn := func() ([]actool.CoreTool, map[string][]string) {
		return nil, map[string][]string{
			"zebra_tool": {"worker"},
			"alpha_tool": {"worker"},
			"mid_tool":   {"worker"},
		}
	}
	wireAgentAugment(pg, t.TempDir(), hostToolsFn)
	defer wireAgentAugment(pg, "", nil)

	_, def1, clean1 := agent.AugmentTools(context.Background(), "worker", nil)
	defer clean1()
	_, def2, clean2 := agent.AugmentTools(context.Background(), "worker", nil)
	defer clean2()

	if len(def1.Deferred) < 3 {
		t.Fatalf("expected deferred tools populated, got %v", def1.Deferred)
	}
	for i := range def1.Deferred {
		if def1.Deferred[i] != def2.Deferred[i] {
			t.Fatalf("non-deterministic deferred order: %v vs %v", def1.Deferred, def2.Deferred)
		}
	}
	_ = actool.DefaultTools()
}
