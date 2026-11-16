package server

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/externalcli"
	"net/url"
)

func TestLiveOAuthHTTPStreaming(t *testing.T) {
	if os.Getenv("ARTEX_CLI_GENERATION") != "1" {
		t.Skip("requires authorized native CLIs")
	}
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("oauth_http_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	cfg, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	query := cfg.Query()
	query.Set("search_path", schema)
	cfg.RawQuery = query.Encode()
	pg, err := db.Open(cfg.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	var activeSchema string
	if err := pg.QueryRow(`SELECT current_schema()`).Scan(&activeSchema); err != nil || activeSchema != schema {
		t.Fatal("test schema isolation failed")
	}
	home, _ := os.UserHomeDir()
	service := externalcli.New(t.Context(), filepath.Join(home, "artex"))
	defer service.Close()
	s := &Server{ctx: t.Context(), m: &Manager{pg: pg}, external: service, jwtKey: []byte(strings.Repeat("x", 32)), provByProfile: map[int64]*provEntry{}}
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/llm/oauth/{kind}", s.oauthStatus)
	mux.HandleFunc("POST /api/llm/test", s.testLLM)
	mux.HandleFunc("POST /api/llm/profiles/{id}/external/stream", s.externalStream)
	server := httptest.NewServer(s.requireAuth(mux))
	defer server.Close()
	denied, err := http.Get(server.URL + "/api/llm/oauth/codex-oauth")
	if err != nil {
		t.Fatal(err)
	}
	denied.Body.Close()
	if denied.StatusCode != 401 {
		t.Fatal("OAuth status is not protected")
	}
	for _, kind := range []string{externalcli.Codex, externalcli.Agy} {
		t.Run(kind, func(t *testing.T) {
			status, err := service.Status(t.Context(), kind)
			if err != nil {
				t.Fatal(err)
			}
			if !status.Authenticated || len(status.Models) == 0 {
				t.Skip("OAuth login required")
			}
			id, err := pg.SaveProfile(&db.LLMProfile{Name: kind, Format: kind, Model: status.Models[0], ReasoningEffort: "low"})
			if err != nil {
				t.Fatal(err)
			}

			if kind == externalcli.Agy {
				if err := pg.SetActiveProfile(id); err != nil {
					t.Fatal(err)
				}
				if cfg, ok := s.loadLLMConfig(); !ok || !cfg.Ready() {
					t.Fatal("active OAuth profile unavailable for ordinary agents")
				}
				profile, _ := pg.ProfileByID(id)
				if !s.resolutionFromProfile(profile, "test").Available {
					t.Fatal("OAuth role resolution failed")
				}
				body, _ := json.Marshal(map[string]any{"provider": kind, "model": status.Models[0], "profile_id": id, "reasoning_effort": "low"})
				check, _ := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/api/llm/test", bytes.NewReader(body))
				check.Header.Set("Authorization", "Bearer "+token)
				response, err := http.DefaultClient.Do(check)
				if err != nil {
					t.Fatal(err)
				}
				var result struct {
					OK    bool   `json:"ok"`
					Reply string `json:"reply"`
					Error string `json:"error"`
				}
				err = json.NewDecoder(response.Body).Decode(&result)
				response.Body.Close()
				if err != nil || !result.OK || !strings.Contains(result.Reply, "OK") {
					t.Fatalf("ordinary provider HTTP check failed: %v %s", err, result.Error)
				}
			}
			payload, _ := json.Marshal(map[string]string{"role": "external", "session_id": fmt.Sprintf("http-%d", time.Now().UnixNano()), "prompt": "Reply only OK. Do not use tools or execute commands."})
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/llm/profiles/%d/external/stream", server.URL, id), bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			started := time.Now()
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != 200 || !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
				t.Fatal("SSE not available")
			}
			scanner := bufio.NewScanner(response.Body)
			var text strings.Builder
			result := false
			first := true
			for scanner.Scan() {
				line := scanner.Text()
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event externalcli.Event
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				if first {
					first = false
					if event.Type != "status" || time.Since(started) > 2*time.Second {
						t.Error("SSE waited for model completion")
					}
				}
				if event.Type == "text_delta" {
					text.WriteString(event.Text)
				}
				if event.Type == "error" {
					t.Fatal(event.Status)
				}
				if event.Type == "result" {
					result = true
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if !result || !strings.Contains(strings.ToUpper(text.String()), "OK") {
				t.Fatalf("missing model response: %q", text.String())
			}
			t.Log("authenticated HTTP endpoint streamed status, native model text and completion")
		})
	}
}
