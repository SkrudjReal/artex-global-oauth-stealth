package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func TestConfigProviderRejectsTruncatedToolHTTPStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "100000")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"probe\",\"function\":{\"name\":\"CommitProbe\",\"arguments\":\"\"}}]}}]}\n\n")
	}))
	defer srv.Close()
	p, err := (Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, Model: "local-test"}).NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	var streamErr error
	for event, err := range p.Stream(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("local probe")}}) {
		if err != nil {
			streamErr = err
			break
		}
		if event.Type == llm.SEToolUseStart || event.Type == llm.SEToolInputJSON {
			t.Fatal("truncated tool reached the SDK")
		}
	}
	if !errors.Is(streamErr, io.ErrUnexpectedEOF) {
		t.Fatalf("error=%v, want unexpected EOF", streamErr)
	}
}

func TestToolCommitRejectsInterruptedCallsBeforeSDKExecution(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		streamErr   error
		stop        bool
		wantCalls   int
	}{
		{"missing_arguments_eof", "", io.ErrUnexpectedEOF, false, 0},
		{"partial_arguments_eof", `{"value":`, io.ErrUnexpectedEOF, false, 0},
		{"complete_arguments_eof", `{"value":1}`, io.ErrUnexpectedEOF, false, 0},
		{"missing_stop", `{"value":1}`, nil, false, 0},
		{"invalid_arguments", `{"value":`, nil, true, 0},
		{"complete", `{"value":1}`, nil, true, 1},
		{"no_arguments", "", nil, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			probe := tool.Build(tool.Spec{
				Name: "CommitProbe", Description: "local test probe",
				Schema: map[string]any{"type": "object"},
				Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
					calls++
					return tool.Result{Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "ok"}}}, nil
				},
			})
			provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
				if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "probe", ToolName: "CommitProbe"}, nil) {
					return
				}
				if tc.input != "" && !yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: tc.input}, nil) {
					return
				}
				if tc.streamErr != nil {
					yield(llm.StreamEvent{}, tc.streamErr)
					return
				}
				if tc.stop {
					yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
				}
			}}
			var activities []db.Activity
			_, _, err := captureRun(context.Background(), agentcore.Options{
				Provider: toolCommitProvider{Provider: provider}, Tools: []tool.CoreTool{probe},
				PermissionMode: permission.ModeBypass, MaxTurns: 1,
			}, "run the local test probe", func(a db.Activity) { activities = append(activities, a) })
			if calls != tc.wantCalls {
				t.Fatalf("tool executions=%d, want %d", calls, tc.wantCalls)
			}
			if tc.wantCalls == 0 {
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("error=%v, want unexpected EOF", err)
				}
				for _, a := range activities {
					if a.Kind == "tool_use" {
						t.Fatal("SDK announced an uncommitted tool as running")
					}
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
