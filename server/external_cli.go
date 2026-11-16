package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/externalcli"
)

func (s *Server) oauthStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.external.Status(r.Context(), r.PathValue("kind"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, status)
}
func (s *Server) oauthConnect(w http.ResponseWriter, r *http.Request) {
	status, err := s.external.Connect(r.Context(), r.PathValue("kind"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, status)
}
func (s *Server) oauthDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.external.Disconnect(r.Context(), r.PathValue("kind")); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) externalStream(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid profile id")
		return
	}
	profile, err := pg.ProfileByID(id)
	if err != nil {
		writeErr(w, 500, "profile lookup failed")
		return
	}
	if profile == nil || !profile.OAuth() {
		writeErr(w, 400, "an OAuth External Agent profile is required")
		return
	}
	var req externalcli.Request
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Role != "external" || req.SessionID == "" || strings.TrimSpace(req.Prompt) == "" {
		writeErr(w, 400, "External Agent role, session ID and prompt are required; use the normal agent API for Planner/Worker")
		return
	}
	req.Kind = profile.Format
	req.Model = profile.Model
	req.Effort = profile.ReasoningEffort
	req.ProfileID = profile.ID
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	var task *Task
	if req.TaskID != "" {
		var ok bool
		task, ok = s.m.Task(req.TaskID)
		if !ok {
			writeErr(w, 404, "task not found")
			return
		}
		state := task.lifecycleSnapshot()
		if state.Paused || state.Queued || isTerminalStatus(state.Status) {
			writeErr(w, 409, "task is not active")
			return
		}
		go func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					live, ok := s.m.Task(req.TaskID)
					if !ok {
						cancel()
						return
					}
					state := live.lifecycleSnapshot()
					if state.Paused || state.Queued || isTerminalStatus(state.Status) {
						cancel()
						return
					}
				}
			}
		}()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	emit := func(event externalcli.Event) error {
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		return controller.Flush()
	}
	// Flush headers/status before starting a CLI; HTTP backpressure reaches its
	// bounded NDJSON reader and cancellation kills the process group.
	if err := emit(externalcli.Event{Type: "status", Status: "external_agent"}); err != nil {
		return
	}
	err = s.external.Run(ctx, req, emit)
	if err != nil {
		status := err.Error()
		if errors.Is(err, context.Canceled) {
			status = "cancelled"
		}
		_ = emit(externalcli.Event{Type: "error", Status: status})
	}
	if task != nil {
		summary := "External Agent completed (" + profile.Name + ")"
		if err != nil {
			summary = "External Agent stopped: " + err.Error()
		}
		s.engine.emitActivity(task, db.Activity{Worker: "external", Kind: "result", Summary: summary, IsError: err != nil})
	}
}

func (s *Server) CloseExternalCLI() {
	if s.external != nil {
		s.external.Close()
	}
}
