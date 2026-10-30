package uaforge

import (
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/net/http/httpguts"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// UAForge's session seed keeps identities stable when a conversation is resumed.
func Headers(ctx context.Context, sessionID string) (map[string]string, error) {
	python := os.Getenv("ARTEX_UAFORGE_PYTHON")
	if python == "" {
		python = "python3"
		relative := filepath.Join(".venv-uaforge", "bin", "python3")
		if runtime.GOOS == "windows" {
			relative = filepath.Join(".venv-uaforge", "Scripts", "python.exe")
		}
		candidates := []string{relative}
		if executable, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Join(filepath.Dir(executable), relative))
		}
		for _, candidate := range candidates {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				python, _ = filepath.Abs(candidate)
				break
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", `import json, sys
def deny_network(event, args):
    if event in ("socket.connect", "socket.getaddrinfo"):
        raise RuntimeError("UAForge runtime network access is disabled")
sys.addaudithook(deny_network)
from uaforge.core.generator import UserAgentGenerator
print(json.dumps(UserAgentGenerator().generate(session=sys.argv[1]).get_headers()))`, sessionID)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("UAForge: %w; install uaforger==0.1.24 and set ARTEX_UAFORGE_PYTHON to its Python interpreter", err)
	}
	var headers map[string]string
	if err := json.Unmarshal(output, &headers); err != nil {
		return nil, fmt.Errorf("UAForge headers: %w", err)
	}
	return validate(headers)
}

func validate(headers map[string]string) (map[string]string, error) {
	if strings.TrimSpace(headers["User-Agent"]) == "" {
		return nil, fmt.Errorf("UAForge returned an empty User-Agent")
	}
	for k, v := range headers {
		if !httpguts.ValidHeaderFieldName(k) || !httpguts.ValidHeaderFieldValue(v) {
			return nil, fmt.Errorf("UAForge returned invalid header %q", k)
		}
	}
	return headers, nil
}

var profileMu sync.Mutex

// Persist the actual profile so library/data updates cannot change a resumed session.
func Load(ctx context.Context, dir, sessionID string) (map[string]string, error) {
	profileMu.Lock()
	defer profileMu.Unlock()
	path := filepath.Join(dir, "headers.json")
	data, err := os.ReadFile(path)
	if err == nil {
		var headers map[string]string
		if err := json.Unmarshal(data, &headers); err != nil {
			return nil, err
		}
		return validate(headers)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	headers, err := Headers(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	data, err = json.Marshal(headers)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dir, ".headers-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return nil, err
	}
	return headers, nil
}
