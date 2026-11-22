package server

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestTaskLLMUnexpectedEOFRetryBoundary(t *testing.T) {
	defer withZeroRetryBackoff()()
	for _, committed := range []bool{false, true} {
		name := "before-output"
		if committed {
			name = "after-output"
		}
		t.Run(name, func(t *testing.T) {
			provider := &scriptedLLMProvider{err: io.ErrUnexpectedEOF}
			if committed {
				provider.events = []llm.StreamEvent{{Type: llm.SETextDelta, Text: "partial"}}
			} else {
				provider.events = []llm.StreamEvent{{Type: llm.SEMessageStart}}
			}
			hooks := taskLLMStreamHooks{
				current: func() (taskLLMSelection, error) {
					return taskLLMSelection{provider: provider}, nil
				},
			}
			events, err := collectTaskLLMStream(streamTaskLLM(context.Background(), "test", llm.CompletionRequest{}, hooks))
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("lost transport error: %v", err)
			}
			wantCalls := sameProviderStreamRetries + 1
			if committed {
				wantCalls = 1
			}
			if provider.calls != wantCalls {
				t.Fatalf("calls=%d, want %d", provider.calls, wantCalls)
			}
			text := ""
			for _, event := range events {
				if event.Type == llm.SETextDelta {
					text += event.Text
				}
			}
			if committed && text != "partial" {
				t.Fatalf("partial output lost or duplicated: %q", text)
			}
		})
	}
}
