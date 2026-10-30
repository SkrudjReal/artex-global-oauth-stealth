package externalcli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Event struct {
	Type   string          `json:"type"`
	Text   string          `json:"text,omitempty"`
	Status string          `json:"status,omitempty"`
	Usage  json.RawMessage `json:"usage,omitempty"`
}
type Request struct {
	Kind      string
	Model     string
	ProfileID int64
	TaskID    string `json:"task_id"`
	Role      string `json:"role"`
	SessionID string `json:"session_id"`
	Prompt    string `json:"prompt"`
}
type session struct {
	mu        sync.Mutex
	p         *process
	agy       *agyRuntime
	Kind      string         `json:"kind"`
	Model     string         `json:"model"`
	ThreadID  string         `json:"thread_id"`
	Provider  *providerState `json:"provider,omitempty"`
	TaskID    string         `json:"task_id"`
	Role      string         `json:"role"`
	SessionID string         `json:"session_id"`
	dir       string
	last      time.Time
}
type Manager struct {
	ctx          context.Context
	cancel       context.CancelFunc
	root         string
	mu           sync.Mutex
	sessions     map[string]*session
	controls     map[string]*session
	disabled     map[string]bool
	limit        chan struct{}
	processSlots chan struct{}
	processes    map[*process]string
}

func New(ctx context.Context, root string) *Manager {
	ctx, cancel := context.WithCancel(ctx)
	m := &Manager{ctx: ctx, cancel: cancel, root: root, sessions: map[string]*session{}, controls: map[string]*session{}, disabled: map[string]bool{}, limit: make(chan struct{}, 4), processSlots: make(chan struct{}, 4), processes: map[*process]string{}}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				m.stopAll("")
				return
			case <-ticker.C:
				m.reap()
			}
		}
	}()
	return m
}
func (m *Manager) Close() {
	m.cancel()
	m.mu.Lock()
	processes := make([]*process, 0, len(m.processes))
	for p := range m.processes {
		processes = append(processes, p)
		p.close()
	}
	m.mu.Unlock()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for _, p := range processes {
		select {
		case <-p.done:
		case <-timer.C:
			return
		}
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for len(m.processSlots) > 0 {
		select {
		case <-ticker.C:
		case <-timer.C:
			return
		}
	}
}
func (m *Manager) reap() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.mu.TryLock() {
			if s.p != nil && time.Since(s.last) > 5*time.Minute {
				s.p.close()
				s.p = nil
			}
			s.mu.Unlock()
		}
	}
	for _, s := range m.controls {
		if s.mu.TryLock() {
			if s.p != nil && time.Since(s.last) > 15*time.Minute {
				s.p.close()
				s.p = nil
			}
			s.mu.Unlock()
		}
	}
}
func (m *Manager) stopAll(kind string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for p, k := range m.processes {
		if kind == "" || kind == k {
			p.close()
		}
	}
}
func secureDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}
func (m *Manager) init(kind string) error {
	if !IsExternal(kind) {
		return errors.New("invalid provider")
	}
	for _, path := range []string{m.root, filepath.Join(m.root, strings.TrimSuffix(kind, "-oauth"))} {
		if err := secureDir(path); err != nil {
			return err
		}
	}
	return nil
}
func (m *Manager) session(req Request) (*session, error) {
	if !IsExternal(req.Kind) || (req.Role != "external" && !(IsExternal(req.Kind) && (req.Role == "provider" || strings.HasPrefix(req.Role, "provider/")))) || req.SessionID == "" || len(req.Role) > 64 || len(req.SessionID) > 128 || len(req.TaskID) > 64 || len(req.Model) > 128 {
		return nil, errors.New("a supported CLI role and bounded task/session ID/model are required")
	}
	if err := m.init(req.Kind); err != nil {
		return nil, err
	}
	parts, _ := json.Marshal([]any{req.Kind, req.ProfileID, req.TaskID, req.Role, req.SessionID})
	key := string(parts)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabled[req.Kind] {
		return nil, errors.New("provider disconnected; connect first")
	}
	if _, err := os.Stat(filepath.Join(m.root, strings.TrimSuffix(req.Kind, "-oauth"), "disconnected")); err == nil {
		return nil, errors.New("provider disconnected; connect first")
	}
	if s := m.sessions[key]; s != nil {
		if s.Model != req.Model {
			return nil, errors.New("session model changed; start a new session")
		}
		return s, nil
	}
	parent := filepath.Join(m.root, strings.TrimSuffix(req.Kind, "-oauth"), "sessions")
	if err := secureDir(parent); err != nil {
		return nil, err
	}
	dir := filepath.Join(parent, hash)
	if err := secureDir(dir); err != nil {
		return nil, err
	}
	if err := secureDir(filepath.Join(dir, "work")); err != nil {
		return nil, err
	}
	s := &session{Kind: req.Kind, Model: req.Model, TaskID: req.TaskID, Role: req.Role, SessionID: req.SessionID, dir: dir}
	data, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err == nil {
		if err := json.Unmarshal(data, s); err != nil {
			return nil, errors.New("invalid saved CLI session")
		}
		if s.Kind != req.Kind || s.Model != req.Model || s.TaskID != req.TaskID || s.Role != req.Role || s.SessionID != req.SessionID {
			return nil, errors.New("session model/provider changed; start a new session")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	m.sessions[key] = s
	return s, nil
}
func (s *session) save() error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, "session.json")
	f, err := os.CreateTemp(s.dir, ".session-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func (m *Manager) Run(ctx context.Context, req Request, emit func(Event) error) error {
	if req.Role != "external" {
		return errors.New("Run requires External Agent role")
	}
	if len(req.Prompt) == 0 || len(req.Prompt) > 1<<20 {
		return errors.New("prompt must contain 1–1048576 bytes")
	}
	s, err := m.session(req)
	if err != nil {
		return err
	}
	if !s.mu.TryLock() {
		return errors.New("this CLI session already has an active request")
	}
	defer s.mu.Unlock()
	select {
	case m.limit <- struct{}{}:
		defer func() { <-m.limit }()
	case <-ctx.Done():
		return ctx.Err()
	case <-m.ctx.Done():
		return m.ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	// The shared limit also bounds idle processes, not just active turns.
	m.reap()
	defer func() { s.last = time.Now(); _ = harden(filepath.Join(m.root, strings.TrimSuffix(req.Kind, "-oauth"))) }()
	if err := emit(Event{Type: "status", Status: "starting"}); err != nil {
		return err
	}
	if req.Kind == Codex {
		err = m.codex(ctx, s, req.Prompt, emit)
	} else {
		err = m.agy(ctx, s, req.Prompt, emit)
	}
	if err != nil && s.p != nil {
		s.p.close()
		s.p = nil
	}
	return err
}

// CLI-owned state stays private; binaries retain their executable mode.
func harden(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "bin" {
				return filepath.SkipDir
			}
			return os.Chmod(path, 0700)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0111 != 0 {
			return os.Chmod(path, 0700)
		}
		return os.Chmod(path, 0600)
	})
}
