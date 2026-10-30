package externalcli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
)

func (m *Manager) codexProcess(ctx context.Context, dir string) (*process, error) {
	p, err := m.start(Codex, dir, "app-server", "--listen", "stdio://")
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, p.close)
	defer stop()
	_, err = p.rpc(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "artex_external", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}})
	if err == nil {
		err = p.send(map[string]any{"method": "initialized", "params": map[string]any{}})
	}
	if err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}
func (m *Manager) codex(ctx context.Context, s *session, prompt string, emit func(Event) error) error {
	if s.p != nil {
		select {
		case <-s.p.done:
			s.p = nil
		default:
		}
	}
	if s.p == nil {
		p, err := m.codexProcess(ctx, filepath.Join(s.dir, "work"))
		if err != nil {
			return err
		}
		s.p = p
		stop := context.AfterFunc(ctx, p.close)
		params := map[string]any{"cwd": filepath.Join(s.dir, "work"), "model": s.Model, "sandbox": "read-only", "approvalPolicy": "untrusted"}
		method := "thread/start"
		if s.ThreadID != "" {
			method = "thread/resume"
			params["threadId"] = s.ThreadID
		}
		raw, err := p.rpc(ctx, method, params)
		stop()
		if err != nil {
			return err
		}
		var response struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if json.Unmarshal(raw, &response) != nil || response.Thread.ID == "" {
			return errors.New("missing Codex thread ID")
		}
		s.ThreadID = response.Thread.ID
		if err := s.save(); err != nil {
			return err
		}
	}
	p := s.p
	stop := context.AfterFunc(ctx, p.close)
	defer stop()
	raw, err := p.rpc(ctx, "turn/start", map[string]any{"threadId": s.ThreadID, "input": []any{map[string]any{"type": "text", "text": prompt}}, "approvalPolicy": "untrusted"})
	if err != nil {
		return err
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(raw, &started)
	if err := emit(Event{Type: "status", Status: "running"}); err != nil {
		return err
	}
	textSeen := false
	for {
		raw, err := p.receive(ctx)
		if err != nil {
			return err
		}
		var msg wire
		if json.Unmarshal(raw, &msg) != nil {
			return errors.New("invalid Codex event")
		}
		if len(msg.ID) > 0 && msg.Method != "" {
			p.decline(msg)
			if err := emit(Event{Type: "status", Status: "action_denied_external_mode"}); err != nil {
				return err
			}
			continue
		}
		var params struct {
			ThreadID string `json:"threadId"`
			Delta    string `json:"delta"`
			Turn     struct {
				ID     string          `json:"id"`
				Status string          `json:"status"`
				Error  json.RawMessage `json:"error"`
			} `json:"turn"`
			TokenUsage struct {
				Last json.RawMessage `json:"last"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(msg.Params, &params) != nil {
			continue
		}
		if params.ThreadID != "" && params.ThreadID != s.ThreadID {
			continue
		}
		switch msg.Method {
		case "item/agentMessage/delta":
			if params.Delta != "" {
				textSeen = true
				if err := emit(Event{Type: "text_delta", Text: params.Delta}); err != nil {
					return err
				}
			}
		case "thread/tokenUsage/updated":
			if err := emit(Event{Type: "usage", Usage: params.TokenUsage.Last}); err != nil {
				return err
			}
		case "error":
			return cliError("Codex", msg.Params)
		case "turn/completed":
			if started.Turn.ID != "" && params.Turn.ID != started.Turn.ID {
				continue
			}
			if params.Turn.Status != "completed" {
				if len(params.Turn.Error) > 0 {
					return cliError("Codex", params.Turn.Error)
				}
				return errors.New("Codex turn " + params.Turn.Status)
			}
			if !textSeen {
				return errors.New("Codex completed without text")
			}
			return emit(Event{Type: "result", Status: "completed"})
		}
	}
}
