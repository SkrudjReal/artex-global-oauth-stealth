package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/internal/sessionfetch"
	"github.com/Autumn-27/artex/internal/uaforge"
	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func configureSessionHTTP(ctx context.Context, opts *agentcore.Options, sessionID string) error {
	if !opts.EnableWebFetch {
		return nil
	}
	if sessionID == "" {
		sessionID = transcript.NewSessionID()
	}
	workDir, err := filepath.Abs(opts.WorkingDir)
	if err != nil {
		return err
	}
	dir := filepath.Join(workDir, ".http-identity", fmt.Sprintf("%x", sha256.Sum256([]byte(sessionID))))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	headers, err := uaforge.Load(ctx, dir, sessionID)
	if err != nil {
		return err
	}
	var curl, wget strings.Builder
	fmt.Fprintf(&curl, "user-agent = %s\n", strconv.Quote(headers["User-Agent"]))
	fmt.Fprintf(&wget, "user_agent = %s\n", headers["User-Agent"])
	if err := os.WriteFile(filepath.Join(dir, ".curlrc"), []byte(curl.String()), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "wgetrc"), []byte(wget.String()), 0o600); err != nil {
		return err
	}
	// Explicit configs avoid changing unrelated API/update/MCP commands in Bash.
	opts.SystemPrompt = append(opts.SystemPrompt, "\nFor laboratory HTTP requests only, use curl --config "+shellQuote(filepath.Join(dir, ".curlrc"))+" or wget --config="+shellQuote(filepath.Join(dir, "wgetrc"))+". Explicit user headers take priority. Do not use these configs for LLM APIs, updates or MCP.\n")
	fetch := sessionfetch.NewWebFetch(sessionfetch.WebFetchConfig{Proxy: opts.WebFetchProxy, CACert: opts.WebFetchCACert, InsecureTLS: opts.WebFetchInsecureTLS, Headers: headers})
	if opts.Tools == nil {
		opts.Tools = actool.DefaultTools()
	}
	opts.Tools = append(opts.Tools, guardPanic(fetch))
	opts.EnableWebFetch = false
	return nil
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
