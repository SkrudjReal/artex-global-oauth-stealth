package enrich

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/internal/uaforge"
)

func TestClientUsesUAForgeIdentity(t *testing.T) {
	if os.Getenv("ARTEX_UAFORGE_PYTHON") == "" {
		t.Skip("set ARTEX_UAFORGE_PYTHON for UAForge integration check")
	}
	client := buildClient(nil)
	transport := client.Transport.(*identityTransport)
	want, err := uaforge.Headers(t.Context(), transport.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		for k, v := range want {
			if strings.HasPrefix(strings.ToLower(k), "sec-ch-") {
				continue
			}
			if got := r.Header.Get(k); got != v {
				t.Errorf("%s = %q, want %q", k, got, v)
			}
		}
		if r.UserAgent() == "artex-enrich/1.0" {
			t.Error("legacy User-Agent")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	for range 2 {
		resp, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
	other := buildClient(nil).Transport.(*identityTransport)
	if other.sessionID == transport.sessionID {
		t.Fatal("clients share session seed")
	}
}

func TestClientDoesNotSendWithoutUAForge(t *testing.T) {
	t.Setenv("ARTEX_UAFORGE_PYTHON", "/nonexistent/uaforge-python")
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("sent request without UAForge") }))
	defer server.Close()
	if _, err := buildClient(nil).Get(server.URL); err == nil {
		t.Fatal("missing UAForge must return error")
	}
}
