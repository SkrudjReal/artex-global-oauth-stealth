package agent

import (
	"context"
	"errors"
	"io"
	"iter"
	"log"
	"net"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// Opt-in buffering allows retries before any response text or tool is committed.
type responseRetryProvider struct {
	llm.Provider
	retry RetryConfig
}

func (p responseRetryProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		retries := p.retry.StreamAttempts
		if retries == 0 {
			retries = 2
		}
		for attempt := 0; ; attempt++ {
			var pending []llm.StreamEvent
			var streamErr error
			stopped := false
			for ev, err := range p.Provider.Stream(ctx, req) {
				if err != nil {
					streamErr = err
					break
				}
				pending = append(pending, ev)
				stopped = stopped || ev.Type == llm.SEMessageStop
			}
			if ctx.Err() != nil {
				streamErr = ctx.Err()
			} else if streamErr == nil && !stopped {
				streamErr = io.ErrUnexpectedEOF
			}
			if streamErr == nil {
				for _, ev := range pending {
					if !yield(ev, nil) {
						return
					}
				}
				return
			}
			if attempt >= retries || ctx.Err() != nil || !retryableStreamTransportError(streamErr) {
				yield(llm.StreamEvent{}, streamErr)
				return
			}
			delay := p.retry.StreamInterval
			if delay <= 0 {
				delay = 500 * time.Millisecond * time.Duration(1<<min(attempt, 3))
			}
			log.Printf("[llm-stream] discarded incomplete response; retry %d/%d error=%s", attempt+1, retries, transportErrorKind(streamErr))
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				yield(llm.StreamEvent{}, ctx.Err())
				return
			case <-timer.C:
			}
		}
	}
}

func retryableStreamTransportError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var networkError net.Error
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &networkError)
}
