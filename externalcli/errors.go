package externalcli

import (
	"encoding/json"
	"errors"
	"strings"
)

// Classify without exposing raw CLI diagnostics (which can contain credentials).
func cliError(provider string, raw json.RawMessage) error {
	text := strings.ToLower(string(raw))
	switch {
	case strings.Contains(text, "invalid model selection"), strings.Contains(text, "invalid effort"), strings.Contains(text, "unsupported effort"):
		return errors.New(provider + " model/effort combination is unsupported; choose a supported reasoning effort or model default")
	case strings.Contains(text, "unauthoriz"), strings.Contains(text, "authentication"), strings.Contains(text, "token expired"), strings.Contains(text, "login"):
		return errors.New(provider + " OAuth session expired or unavailable; reconnect through the official CLI")
	case strings.Contains(text, "usagelimit"), strings.Contains(text, "quota"), strings.Contains(text, "credits"), strings.Contains(text, "rate limit"), strings.Contains(text, "429"):
		return errors.New(provider + " account limit reached; wait for the account limit to reset")
	default:
		return errors.New(provider + " request failed; check model access and CLI connection")
	}
}
