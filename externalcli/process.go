// Package externalcli runs official OAuth CLIs as external agents, never as
// replacements for Norma's tool-calling llm.Provider contract.
package externalcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const Codex = "codex-oauth"
const Agy = "agy-oauth"

func IsExternal(kind string) bool { return kind == Codex || kind == Agy }

type wire struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
	Event  string          `json:"event"`
}
type process struct {
	cmd       *exec.Cmd
	in        io.WriteCloser
	events    chan json.RawMessage
	done      chan struct{}
	stopped   chan struct{}
	pending   []json.RawMessage
	holdTools bool
	once      sync.Once
	next      int
}

func (p *process) close() {
	select {
	case <-p.done:
		return
	default:
	}
	p.once.Do(func() { close(p.stopped); _ = p.in.Close(); killProcess(p.cmd) })
}
func (p *process) send(v any) error { return json.NewEncoder(p.in).Encode(v) }
func (p *process) receive(ctx context.Context) (json.RawMessage, error) {
	if len(p.pending) > 0 {
		raw := p.pending[0]
		p.pending = p.pending[1:]
		return raw, nil
	}
	return p.receiveRaw(ctx)
}
func (p *process) receiveRaw(ctx context.Context) (json.RawMessage, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case raw, ok := <-p.events:
		if !ok {
			return nil, errors.New("CLI connection closed")
		}
		return raw, nil
	}
}
func (p *process) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p.next++
	id := p.next
	if err := p.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		raw, err := p.receiveRaw(ctx)
		if err != nil {
			return nil, err
		}
		var msg wire
		if json.Unmarshal(raw, &msg) != nil {
			return nil, errors.New("invalid CLI protocol")
		}
		var got int
		_ = json.Unmarshal(msg.ID, &got)
		if msg.Method != "" && len(msg.ID) > 0 {
			if p.holdTools && msg.Method == "item/tool/call" {
				if len(p.pending) >= 64 {
					return nil, errors.New("too many pending tool calls")
				}
				p.pending = append(p.pending, raw)
			} else {
				p.decline(msg)
			}
			continue
		}
		if got != id {
			if msg.Method != "" {
				if len(p.pending) >= 64 {
					p.close()
					return nil, errors.New("too many pending CLI notifications")
				}
				p.pending = append(p.pending, raw)
			}
			continue
		}
		if len(msg.Error) > 0 && string(msg.Error) != "null" {
			return nil, errors.New("CLI rejected " + method + "; check authorization, model and account limits")
		}
		return msg.Result, nil
	}
}
func (p *process) decline(msg wire) {
	// No automatic approval and no execution of CLI tools through ARTEX.
	var result any = map[string]any{"decision": "cancel"}
	if strings.Contains(msg.Method, "tool/call") {
		result = map[string]any{"success": false, "contentItems": []any{map[string]string{"type": "inputText", "text": "ARTEX tools are unavailable in External Agent mode"}}}
	}
	if strings.Contains(msg.Method, "requestUserInput") {
		result = map[string]any{"answers": map[string]any{}}
	}
	if strings.Contains(msg.Method, "permissions") {
		result = map[string]any{"permissions": map[string]any{}, "scope": "turn"}
	}
	_ = p.send(map[string]any{"id": msg.ID, "result": result})
}

func (m *Manager) environment(kind string) []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "CODEX_HOME" || key == "HOME" || key == "XDG_CONFIG_HOME" || key == "XDG_DATA_HOME" || key == "XDG_CACHE_HOME" || key == "CURL_HOME" || key == "WGETRC" || key == "USER_AGENT" || strings.HasPrefix(key, "OPENAI_") || strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "GEMINI_") || strings.HasPrefix(key, "AGY_") || key == "GOOGLE_API_KEY" || key == "GOOGLE_APPLICATION_CREDENTIALS" {
			continue
		}
		env = append(env, entry)
	}
	root := filepath.Join(m.root, strings.TrimSuffix(kind, "-oauth"))
	if kind == Codex {
		env = append(env, "HOME="+root, "CODEX_HOME="+root)
	} else {
		env = append(env, "HOME="+root)
	}
	env = append(env, "XDG_CONFIG_HOME="+filepath.Join(root, ".config"), "XDG_DATA_HOME="+filepath.Join(root, ".local/share"), "XDG_CACHE_HOME="+filepath.Join(root, ".cache"))
	return env
}
func (m *Manager) binary(kind string) (string, error) {
	if !IsExternal(kind) {
		return "", errors.New("unknown external provider")
	}
	name := strings.TrimSuffix(kind, "-oauth")
	local := filepath.Join(m.root, name, "bin", name)
	if info, err := os.Stat(local); err == nil && !info.IsDir() {
		return local, nil
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("install the native Linux %s CLI", name)
	}
	resolved, _ := filepath.EvalSymlinks(path)
	if strings.HasSuffix(strings.ToLower(resolved), ".exe") {
		return "", errors.New("Windows CLI cannot provide Linux session isolation; install native agy in ~/artex/agy/bin/agy")
	}
	return path, nil
}
func (m *Manager) start(kind, dir string, args ...string) (*process, error) {
	path, err := m.binary(kind)
	if err != nil {
		return nil, err
	}
	select {
	case m.processSlots <- struct{}{}:
	default:
		return nil, errors.New("CLI process limit reached; wait for idle timeout or disconnect unused provider")
	}
	started := false
	defer func() {
		if !started {
			<-m.processSlots
		}
	}()
	cmd := exec.CommandContext(m.ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = m.environment(kind)
	setupProcess(cmd)
	cmd.WaitDelay = 2 * time.Second
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, err
	}
	cmd.Stderr = io.Discard // CLI diagnostics may contain login URLs, codes or tokens.
	if err := cmd.Start(); err != nil {
		input.Close()
		return nil, err
	}
	p := &process{cmd: cmd, in: input, events: make(chan json.RawMessage, 32), done: make(chan struct{}), stopped: make(chan struct{})}
	started = true
	m.mu.Lock()
	m.processes[p] = kind
	m.mu.Unlock()
	go func() {
		defer func() { <-m.processSlots; m.mu.Lock(); delete(m.processes, p); m.mu.Unlock() }()
		defer close(p.done)
		defer close(p.events)
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 65536), 4<<20)
		for scanner.Scan() {
			raw := append(json.RawMessage(nil), scanner.Bytes()...)
			if !json.Valid(raw) {
				continue
			}
			select {
			case p.events <- raw:
			case <-m.ctx.Done():
				p.close()
				_ = cmd.Wait()
				return
			case <-p.stopped:
				_ = cmd.Wait()
				return
			}
		}
		if scanner.Err() != nil {
			p.close()
		}
		_ = cmd.Wait()
	}()
	return p, nil
}

// Status/logout helpers share the same process budget and shutdown context.
func (m *Manager) commandOutput(ctx context.Context, kind string, args ...string) ([]byte, error) {
	path, err := m.binary(kind)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	select {
	case m.processSlots <- struct{}{}:
		defer func() { <-m.processSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = filepath.Join(m.root, strings.TrimSuffix(kind, "-oauth"))
	cmd.Env = m.environment(kind)
	setupProcess(cmd)
	cmd.WaitDelay = 2 * time.Second
	if kind == Codex {
		return cmd.CombinedOutput()
	}
	return cmd.Output()
}
