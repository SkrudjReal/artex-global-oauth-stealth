package externalcli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
)

func (m *Manager) agy(ctx context.Context, s *session, prompt string, emit func(Event) error) error {
	if s.p != nil {
		select {
		case <-s.p.done:
			s.p = nil
		default:
		}
	}
	if s.p == nil {
		args := []string{"--print=", "--input-format", "stream-json", "--output-format", "stream-json", "--model", s.Model, "--mode", "plan", "--sandbox", "--disable-slash-commands", "--log-file", "/dev/null"}
		if s.ThreadID != "" {
			args = append(args, "--conversation", s.ThreadID)
		}
		p, err := m.start(Agy, filepath.Join(s.dir, "work"), args...)
		if err != nil {
			return err
		}
		s.p = p
	}
	p := s.p
	stop := context.AfterFunc(ctx, p.close)
	defer stop()
	if err := p.send(map[string]any{"event": "user", "message": map[string]string{"role": "user", "content": prompt}}); err != nil {
		return err
	}
	textSeen := false
	for {
		raw, err := p.receive(ctx)
		if err != nil {
			return err
		}
		var event struct {
			Event string `json:"event"`
			Init  struct {
				ConversationID string `json:"conversation_id"`
				SessionID      string `json:"session_id"`
			} `json:"init"`
			ConversationID string `json:"conversation_id"`
			SessionID      string `json:"session_id"`
			Text           string `json:"text"`
			Delta          string `json:"delta"`
			Step           struct {
				Type string `json:"step_type"`
				Text string `json:"text_delta"`
			} `json:"step_update"`
			Result struct {
				Status         string          `json:"status"`
				Error          string          `json:"error"`
				Response       string          `json:"response"`
				ConversationID string          `json:"conversation_id"`
				Usage          json.RawMessage `json:"usage"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &event) != nil {
			return errors.New("invalid agy event")
		}
		switch event.Event {
		case "init":
			id := event.Init.ConversationID
			if id == "" {
				id = event.ConversationID
			}
			if id == "" {
				id = event.Init.SessionID
			}
			if id == "" {
				id = event.SessionID
			}
			if id != "" {
				s.ThreadID = id
				if err := s.save(); err != nil {
					return err
				}
			}
			if err := emit(Event{Type: "status", Status: "running"}); err != nil {
				return err
			}
		case "step_update":
			if event.Step.Type != "agent_response" {
				if err := emit(Event{Type: "status", Status: "working"}); err != nil {
					return err
				}
			}
			if event.Step.Type == "agent_response" && event.Step.Text != "" {
				textSeen = true
				if err := emit(Event{Type: "text_delta", Text: event.Step.Text}); err != nil {
					return err
				}
			}
		case "text_delta":
			delta := event.Delta
			if delta == "" {
				delta = event.Text
			}
			if delta != "" {
				textSeen = true
				if err := emit(Event{Type: "text_delta", Text: delta}); err != nil {
					return err
				}
			}
		case "error":
			return cliError("agy", raw)
		case "result":
			if !strings.EqualFold(event.Result.Status, "success") {
				return cliError("agy", raw)
			}
			if !textSeen && event.Result.Response != "" {
				if err := emit(Event{Type: "text_delta", Text: event.Result.Response}); err != nil {
					return err
				}
				textSeen = true
			}
			if !textSeen {
				return errors.New("agy completed without response text")
			}
			if event.Result.ConversationID != "" {
				s.ThreadID = event.Result.ConversationID
			}
			if s.ThreadID == "" {
				return errors.New("agy did not provide a conversation ID; restart-safe continuation is unavailable")
			}
			if err := s.save(); err != nil {
				return err
			}
			if len(event.Result.Usage) > 0 {
				if err := emit(Event{Type: "usage", Usage: event.Result.Usage}); err != nil {
					return err
				}
			}
			return emit(Event{Type: "result", Status: "completed"})
		}
	}
}
