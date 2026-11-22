package agent

import (
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// Record connection stages on failures without logging credentials or payloads.
func diagnosticLLMRoundTrip(base http.RoundTripper, req *http.Request) (*http.Response, error) {
	start := time.Now()
	var mu sync.Mutex
	stage, protocol, reused := "connect", "", false
	setStage := func(s string) { mu.Lock(); stage = s; mu.Unlock() }
	trace := &httptrace.ClientTrace{
		TLSHandshakeStart: func() { setStage("tls") },
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			mu.Lock()
			defer mu.Unlock()
			protocol = state.NegotiatedProtocol
			if err == nil {
				stage = "request"
			}
		},
		GotConn: func(info httptrace.GotConnInfo) {
			mu.Lock()
			reused, stage = info.Reused, "request"
			mu.Unlock()
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				setStage("response_headers")
			}
		},
	}
	resp, err := base.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if err != nil {
		mu.Lock()
		log.Printf("[llm-http] host=%s stage=%s protocol=%s reused=%t request_bytes=%d elapsed=%s error=%s",
			req.URL.Hostname(), stage, protocol, reused, req.ContentLength, time.Since(start).Round(time.Millisecond), transportErrorKind(err))
		mu.Unlock()
		return resp, err
	}
	if resp != nil && resp.Body != nil {
		resp.Body = &diagnosticLLMBody{ReadCloser: resp.Body, host: req.URL.Hostname(), protocol: resp.Proto, status: resp.StatusCode, start: start}
	}
	return resp, nil
}

type diagnosticLLMBody struct {
	io.ReadCloser
	host, protocol string
	status         int
	start          time.Time
	bytes          int64
	reported       bool
}

func (b *diagnosticLLMBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytes += int64(n)
	if err != nil && !errors.Is(err, io.EOF) && !b.reported {
		b.reported = true
		log.Printf("[llm-http] host=%s stage=response_body protocol=%s status=%d response_bytes=%d elapsed=%s error=%s",
			b.host, b.protocol, b.status, b.bytes, time.Since(b.start).Round(time.Millisecond), transportErrorKind(err))
	}
	return n, err
}

func transportErrorKind(err error) string {
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_EOF"
	case errors.Is(err, io.EOF):
		return "EOF"
	default:
		return "transport_error"
	}
}
