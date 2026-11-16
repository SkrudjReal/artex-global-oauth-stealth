package externalcli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Status struct {
	Compatibility   string   `json:"compatibility,omitempty"`
	Installed       bool     `json:"installed"`
	Authenticated   bool     `json:"authenticated"`
	State           string   `json:"state"`
	Instruction     string   `json:"instruction,omitempty"`
	Models          []string `json:"models"`
	VerificationURL string   `json:"verification_url,omitempty"`
	UserCode        string   `json:"user_code,omitempty"`
}

func shellArg(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func (m *Manager) instruction(kind string) string {
	root := filepath.Join(m.root, strings.TrimSuffix(kind, "-oauth"))
	path, err := m.binary(kind)
	if err != nil {
		return err.Error()
	}
	if kind == Codex {
		return "CODEX_HOME=" + shellArg(root) + " " + shellArg(path) + " login --device-auth"
	}
	return "HOME=" + shellArg(root) + " " + shellArg(path) + " (Google Account login; Linux keyring must be unlocked). To revoke shared keyring credentials, enter /logout in that terminal."
}
func (m *Manager) control(ctx context.Context, kind string) (*session, error) {
	if err := m.init(kind); err != nil {
		return nil, err
	}
	m.mu.Lock()
	s := m.controls[kind]
	if s == nil {
		s = &session{Kind: kind}
		m.controls[kind] = s
	}
	m.mu.Unlock()
	if !s.mu.TryLock() {
		return nil, errors.New("OAuth control request is busy")
	}
	if s.p != nil {
		select {
		case <-s.p.done:
			s.p = nil
		default:
		}
	}
	if s.p == nil {
		p, err := m.codexProcess(ctx, filepath.Join(m.root, "codex"))
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		s.p = p
	}
	s.last = time.Now()
	return s, nil // caller unlocks
}
func (m *Manager) Status(ctx context.Context, kind string) (Status, error) {
	result := Status{State: "not_installed", Models: []string{}}
	if kind == Agy {
		result.Compatibility = "MCP tools; Gemini prompts via session .txt files (AGY 1.4.11+)"
	} else {
		result.Compatibility = "ARTEX tool calling"
	}
	if err := m.init(kind); err != nil {
		return result, err
	}
	_, err := m.binary(kind)
	if err != nil {
		result.Instruction = err.Error()
		return result, nil
	}
	defer harden(filepath.Join(m.root, strings.TrimSuffix(kind, "-oauth")))
	result.Installed = true
	result.Instruction = m.instruction(kind)
	m.mu.Lock()
	disabled := m.disabled[kind]
	m.mu.Unlock()
	if _, err := os.Stat(filepath.Join(m.root, strings.TrimSuffix(kind, "-oauth"), "disconnected")); err == nil {
		disabled = true
	}
	if disabled {
		result.State = "disconnected"
		return result, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if kind == Codex {
		output, err := m.commandOutput(ctx, kind, "login", "status")
		result.Authenticated = err == nil && strings.Contains(strings.ToLower(string(output)), "chatgpt")
		if !result.Authenticated {
			result.State = "login_required"
			return result, nil
		}
		s, err := m.control(ctx, kind)
		if err != nil {
			return result, err
		}
		defer s.mu.Unlock()
		stop := context.AfterFunc(ctx, s.p.close)
		defer stop()
		raw, err := s.p.rpc(ctx, "model/list", map[string]any{"limit": 100})
		if err != nil {
			return result, err
		}
		var models struct {
			Data []struct {
				Model string `json:"model"`
				ID    string `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(raw, &models)
		for _, item := range models.Data {
			model := item.Model
			if model == "" {
				model = item.ID
			}
			if model != "" {
				result.Models = append(result.Models, model)
			}
		}
	} else {
		// models is the CLI's own authenticated capability check; no token files read.
		output, err := m.commandOutput(ctx, kind, "models")
		if err != nil {
			result.State = "interactive_login_required"
			return result, nil
		}
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.SplitN(line, "\t", 2)
			if len(fields) == 2 && strings.TrimSpace(fields[0]) != "" {
				result.Models = append(result.Models, strings.TrimSpace(fields[0]))
			}
		}
		result.Authenticated = len(result.Models) > 0
	}
	if result.Authenticated {
		result.State = "authenticated"
	} else {
		result.State = "interactive_login_required"
	}
	return result, nil
}
func (m *Manager) Connect(ctx context.Context, kind string) (Status, error) {
	if err := m.init(kind); err != nil {
		return Status{}, err
	}
	m.mu.Lock()
	delete(m.disabled, kind)
	m.mu.Unlock()
	path := filepath.Join(m.root, strings.TrimSuffix(kind, "-oauth"), "disconnected")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return Status{}, err
	}
	if err := m.linkExistingAuth(kind); err != nil {
		return Status{}, err
	}
	status, err := m.Status(ctx, kind)
	if err != nil || status.Authenticated || !status.Installed || kind == Agy {
		return status, err
	}
	s, err := m.control(ctx, kind)
	if err != nil {
		return status, err
	}
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, s.p.close)
	defer stop()
	raw, err := s.p.rpc(ctx, "account/login/start", map[string]string{"type": "chatgptDeviceCode"})
	if err != nil {
		status.State = "terminal_login_required"
		return status, nil
	}
	var login struct {
		URL  string `json:"verificationUrl"`
		Code string `json:"userCode"`
	}
	if json.Unmarshal(raw, &login) != nil {
		return status, errors.New("invalid CLI device login response")
	}
	status.State = "login_pending"
	status.VerificationURL = login.URL
	status.UserCode = login.Code
	return status, nil
}
func (m *Manager) Disconnect(ctx context.Context, kind string) error {
	if err := m.init(kind); err != nil {
		return err
	}
	m.mu.Lock()
	m.disabled[kind] = true
	m.mu.Unlock()
	m.stopAll(kind)
	if err := os.WriteFile(filepath.Join(m.root, strings.TrimSuffix(kind, "-oauth"), "disconnected"), []byte("disconnected\n"), 0600); err != nil {
		return err
	}
	authPath := m.authPath(kind)
	if info, err := os.Lstat(authPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return os.Remove(authPath)
	}
	if kind == Codex {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if _, err := m.commandOutput(ctx, kind, "logout"); err != nil {
			return errors.New("Codex logout failed; use isolated CODEX_HOME logout in terminal")
		}
	}
	// AGY /logout revokes the OS-wide shared keyring entry. Local disconnect must
	// not silently log out the user's IDE and other CLI clients.
	return nil
}

func (m *Manager) authPath(kind string) string {
	if kind == Codex {
		return filepath.Join(m.root, "codex", "auth.json")
	}
	return filepath.Join(m.root, "agy", ".gemini", "antigravity-cli", "antigravity-oauth-token")
}

// Share only the CLI-owned authorization file, never config, history or tools.
func (m *Manager) linkExistingAuth(kind string) error {
	target := m.authPath(kind)
	if _, err := os.Lstat(target); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	source := filepath.Join(home, ".codex", "auth.json")
	if kind == Agy {
		source = filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	}
	if source == target {
		return nil
	}
	info, err := os.Stat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("existing CLI auth path is not a regular file")
	}
	if kind == Agy {
		if err := secureDir(filepath.Join(m.root, "agy", ".gemini")); err != nil {
			return err
		}
		if err := secureDir(filepath.Dir(target)); err != nil {
			return err
		}
	}
	if err := os.Symlink(source, target); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}
