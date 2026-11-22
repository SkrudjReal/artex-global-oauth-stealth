package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

// Norma finalizes partial tool calls even on stream errors; hold tool events
// until the response succeeds so truncated arguments can never be executed.
type toolCommitProvider struct{ llm.Provider }

func (p toolCommitProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		var pending []llm.StreamEvent
		var input strings.Builder
		var toolName string
		stopped := false
		validate := func() error {
			raw := strings.TrimSpace(input.String())
			if toolName != "" && raw != "" && !json.Valid([]byte(raw)) {
				return fmt.Errorf("incomplete or invalid JSON arguments for tool %s: %w", toolName, io.ErrUnexpectedEOF)
			}
			return nil
		}
		for event, err := range p.Provider.Stream(ctx, req) {
			if err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			if event.Type == llm.SEToolUseStart {
				if err := validate(); err != nil {
					yield(llm.StreamEvent{}, err)
					return
				}
				toolName = event.ToolName
				input.Reset()
			}
			if event.Type == llm.SEToolInputJSON {
				input.WriteString(event.Text)
			}
			if event.Type == llm.SEMessageStop {
				stopped = true
			}
			if pending != nil || event.Type == llm.SEToolUseStart {
				pending = append(pending, event)
			} else if !yield(event, nil) {
				return
			}
		}
		if ctx.Err() != nil {
			yield(llm.StreamEvent{}, ctx.Err())
			return
		}
		if len(pending) > 0 {
			if !stopped {
				yield(llm.StreamEvent{}, io.ErrUnexpectedEOF)
				return
			}
			if err := validate(); err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
		}
		for _, event := range pending {
			if !yield(event, nil) {
				return
			}
		}
	}
}
