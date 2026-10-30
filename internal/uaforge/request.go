package uaforge

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

type generatedHintsKey struct{}

// Only low-entropy Chromium hints are defaults; high-entropy hints require
// Accept-CH negotiation, which these non-browser clients do not implement.
func Apply(req *http.Request, defaults map[string]string) {
	generated := []string{}
	explicitUA := false
	for k := range req.Header {
		if strings.EqualFold(k, "User-Agent") {
			explicitUA = true
		}
	}
	ua := defaults["User-Agent"]
	chromium := strings.Contains(ua, "Chrome/") || strings.Contains(ua, "Chromium/")
	for k, v := range defaults {
		canonical := http.CanonicalHeaderKey(k)
		exists := false
		for supplied := range req.Header {
			if strings.EqualFold(supplied, k) {
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		if strings.HasPrefix(strings.ToLower(k), "sec-ch-") {
			if explicitUA || !chromium || req.URL.Scheme != "https" {
				continue
			}
			switch canonical {
			case "Sec-Ch-Ua", "Sec-Ch-Ua-Mobile", "Sec-Ch-Ua-Platform":
			default:
				continue
			}
		}
		req.Header.Set(k, v)
		if strings.HasPrefix(strings.ToLower(k), "sec-ch-") {
			generated = append(generated, canonical)
		}
	}
	*req = *req.WithContext(context.WithValue(req.Context(), generatedHintsKey{}, generated))
}

func Redirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	for _, previous := range via {
		if !strings.EqualFold(previous.URL.Host, req.URL.Host) || previous.URL.Scheme != req.URL.Scheme {
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			req.Header.Del("Proxy-Authorization")
			break
		}
	}
	// Never carry generated browser hints to an insecure redirect target.
	if req.URL.Scheme != "https" {
		generated, _ := req.Context().Value(generatedHintsKey{}).([]string)
		for _, k := range generated {
			req.Header.Del(k)
		}
	}
	return nil
}
