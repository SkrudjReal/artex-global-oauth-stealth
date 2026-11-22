package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

func TestResponseBufferingIsExplicitOptIn(t *testing.T) {
	for _, value := range []string{"", "1"} {
		t.Setenv("ARTEX_LLM_BUFFER_STREAM", value)
		p, err := (Config{Format: llm.FormatOpenAI, BaseURL: "https://llm.example/v1", Model: "test-model"}).NewProvider()
		if err != nil {
			t.Fatal(err)
		}
		_, buffered := p.(responseRetryProvider)
		if buffered != (value == "1") {
			t.Fatalf("setting=%q buffered=%v", value, buffered)
		}
	}
}

func TestResponseRetryRecoversTruncatedHTTPStream(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", "100000")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"discarded\"}}]}\n\n")
			return
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	inner, err := (Config{Format: llm.FormatOpenAI, BaseURL: server.URL, Model: "local-test"}).NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	p := responseRetryProvider{Provider: inner, retry: RetryConfig{StreamAttempts: 1, StreamInterval: time.Nanosecond}}
	var text string
	for ev, err := range p.Stream(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("probe")}}) {
		if err != nil {
			t.Fatal(err)
		}
		text += ev.Text
	}
	if text != "recovered" || calls.Load() != 2 {
		t.Fatalf("text=%q calls=%d", text, calls.Load())
	}
}

func TestResponseRetryCommitsOnlySuccessfulReply(t *testing.T) {
	for _, cause := range []error{io.ErrUnexpectedEOF, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}} {
		calls := 0
		inner := captureUsageProvider{stream: func(ctx context.Context, yield func(llm.StreamEvent, error) bool) {
			calls++
			text := "discarded partial reply"
			if calls == 2 {
				text = "final reply"
			}
			if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: text}, nil) {
				return
			}
			if calls == 1 {
				if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolName: "discarded_tool"}, nil) {
					return
				}
				yield(llm.StreamEvent{}, cause)
			} else {
				yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
			}
		}}
		p := responseRetryProvider{Provider: inner, retry: RetryConfig{StreamAttempts: 1, StreamInterval: time.Nanosecond}}
		var text string
		for ev, err := range p.Stream(context.Background(), llm.CompletionRequest{}) {
			if err != nil {
				t.Fatal(err)
			}
			if ev.Type == llm.SEToolUseStart {
				t.Fatal("discarded tool escaped buffering")
			}
			text += ev.Text
		}
		if text != "final reply" || calls != 2 {
			t.Fatalf("text=%q calls=%d", text, calls)
		}
	}
}

func TestResponseRetryFailureBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cause    error
		attempts int
		want     int
		cancel   bool
	}{
		{"exhausted", io.ErrUnexpectedEOF, 2, 3, false},
		{"disabled", io.ErrUnexpectedEOF, -1, 1, false},
		{"auth", errors.New("openai: status 401"), 2, 1, false},
		{"cancel", io.ErrUnexpectedEOF, 2, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			inner := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
				calls++
				if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "discard"}, nil) {
					return
				}
				if tc.cancel {
					cancel()
				}
				yield(llm.StreamEvent{}, tc.cause)
			}}
			p := responseRetryProvider{Provider: inner, retry: RetryConfig{StreamAttempts: tc.attempts, StreamInterval: time.Nanosecond}}
			var got error
			for _, err := range p.Stream(ctx, llm.CompletionRequest{}) {
				if err == nil {
					t.Fatal("partial event escaped")
				}
				got = err
			}
			want := tc.cause
			if tc.cancel {
				want = context.Canceled
			}
			if !errors.Is(got, want) || calls != tc.want {
				t.Fatalf("error=%v calls=%d", got, calls)
			}
		})
	}
}

func TestResponseRetryRejectsMissingStop(t *testing.T) {
	inner := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "partial"}, nil)
	}}
	p := responseRetryProvider{Provider: inner, retry: RetryConfig{StreamAttempts: -1}}
	for _, err := range p.Stream(context.Background(), llm.CompletionRequest{}) {
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("error=%v", err)
		}
	}
}
