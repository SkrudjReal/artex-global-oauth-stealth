package sessionfetch

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func TestReviewRedirects(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/start" {
			if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Cookie") != "session=secret" || r.UserAgent() != "Explicit/1" {
				t.Error("initial headers changed")
			}
			http.Redirect(w, r, "http://sub.example.test/next", 302)
			return
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("credentials leaked")
		}
		if r.URL.Path == "/next" {
			http.Redirect(w, r, "http://example.test/end", 302)
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	client := newFetchClient(WebFetchConfig{})
	client.Transport = &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}}
	req, _ := http.NewRequest("GET", "http://example.test/start", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("User-Agent", "Explicit/1")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls != 3 {
		t.Fatal(calls)
	}
	// Verify explicit tool headers override the generated profile too.
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != "Explicit/1" {
			t.Error("tool overwrote explicit UA")
		}
		w.WriteHeader(204)
	}))
	defer direct.Close()
	input, _ := json.Marshal(map[string]any{"url": direct.URL, "headers": map[string]string{"User-Agent": "Explicit/1"}})
	result, err := NewWebFetch(WebFetchConfig{Headers: map[string]string{"User-Agent": "Generated/1"}}).Call(t.Context(), input, &actool.ToolContext{})
	if err != nil || result.IsError {
		t.Fatalf("fetch: %v %s", err, result.Flatten())
	}
}
