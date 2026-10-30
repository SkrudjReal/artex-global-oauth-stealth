package uaforge

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewHeaderFormat(t *testing.T) {
	for _, ua := range []string{"Mozilla/5.0 Chrome/152.0.0.0 Safari/537.36", "Mozilla/5.0 Firefox/147.0", "Mozilla/5.0 Version/26.0 Safari/605.1.15"} {
		defaults := map[string]string{"User-Agent": ua, "Sec-CH-UA": `"Chromium";v="152"`, "Sec-CH-UA-Mobile": "?0", "Sec-CH-UA-Platform": `"Linux"`, "Sec-CH-UA-Model": `"Model"`}
		for _, scheme := range []string{"https", "http"} {
			req, _ := http.NewRequest("GET", scheme+"://example.test", nil)
			Apply(req, defaults)
			hints := strings.Contains(ua, "Chrome/") && scheme == "https"
			if (req.Header.Get("Sec-CH-UA") != "") != hints {
				t.Fatalf("incorrect hints: %s %s", ua, scheme)
			}
			if req.Header.Get("Sec-CH-UA-Model") != "" {
				t.Fatal("high entropy hint without negotiation")
			}
			req, _ = http.NewRequest("GET", scheme+"://example.test", nil)
			req.Header.Set("User-Agent", "Explicit/1")
			req.Header.Set("X-User", "keep")
			Apply(req, defaults)
			if req.UserAgent() != "Explicit/1" || req.Header.Get("X-User") != "keep" || req.Header.Get("Sec-CH-UA") != "" {
				t.Fatal("explicit header overridden")
			}
		}
	}
	if _, err := validate(map[string]string{"User-Agent": "bad\r\nInjected: yes"}); err == nil {
		t.Fatal("accepted CRLF")
	}
	// Even a library import that attempts a download must fail before connecting.
	dir := t.TempDir()
	pkg := filepath.Join(dir, "uaforge")
	if err := os.MkdirAll(pkg, 0700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("runtime network access") }))
	defer server.Close()
	code := "import urllib.request\nurllib.request.urlopen(" + `"` + server.URL + `"` + ")\n"
	if err := os.WriteFile(filepath.Join(pkg, "__init__.py"), []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PYTHONPATH", dir)
	if _, err := Headers(t.Context(), "offline"); err == nil {
		t.Fatal("runtime download was allowed")
	}
}
