package agent

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
)

func TestLLMHTTPDiagnosticsStagesAndRedaction(t *testing.T) {
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(old)
	for _, stage := range []string{"tls", "response_headers", "response_body"} {
		t.Run(stage, func(t *testing.T) {
			output.Reset()
			req, _ := http.NewRequest("POST", "https://example.test/key-in-path?api_key=query-secret", strings.NewReader("private-chat"))
			req.Header.Set("Authorization", "Bearer header-secret")
			callerTrace := 0
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{TLSHandshakeStart: func() { callerTrace++ }}))
			base := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				tr := httptrace.ContextClientTrace(r.Context())
				tr.TLSHandshakeStart()
				if stage == "tls" {
					return nil, io.ErrUnexpectedEOF
				}
				tr.TLSHandshakeDone(tls.ConnectionState{NegotiatedProtocol: "h2"}, nil)
				tr.GotConn(httptrace.GotConnInfo{Reused: true})
				tr.WroteRequest(httptrace.WroteRequestInfo{})
				if stage == "response_headers" {
					return nil, io.ErrUnexpectedEOF
				}
				return &http.Response{StatusCode: 200, Proto: "HTTP/2.0", Body: io.NopCloser(errorReader{})}, nil
			})
			resp, err := diagnosticLLMRoundTrip(base, req)
			if resp != nil {
				_, err = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("error was changed: %v", err)
			}
			if callerTrace != 1 {
				t.Fatal("caller trace was lost")
			}
			text := output.String()
			if !strings.Contains(text, "stage="+stage) || !strings.Contains(text, "unexpected_EOF") {
				t.Fatalf("missing diagnostic: %s", text)
			}
			for _, secret := range []string{"key-in-path", "query-secret", "header-secret", "private-chat"} {
				if strings.Contains(text, secret) {
					t.Fatalf("diagnostic leaked %s", secret)
				}
			}
		})
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
