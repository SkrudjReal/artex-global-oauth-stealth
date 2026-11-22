package llmrec

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"github.com/google/uuid"
)

type completeProvider struct{}

func (completeProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(func(llm.StreamEvent, error) bool) {}
}

func (completeProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			{Type: llm.BlockThinking, Thinking: "reasoning"},
			llm.TextBlock("answer"),
		},
	}, "stop", llm.Usage{InputTokens: 7, OutputTokens: 3}, nil
}

func TestTaskIDContextUsesExplicitRegistryID(t *testing.T) {
	ctx := WithTaskID(context.Background(), " 42 ")
	if got := TaskIDFrom(ctx); got != "42" {
		t.Fatalf("TaskIDFrom()=%q want 42", got)
	}
	if got := TaskIDFrom(WithTaskID(ctx, "   ")); got != "42" {
		t.Fatalf("blank task id should preserve parent context, got %q", got)
	}
	if got := TaskIDFrom(nil); got != "" {
		t.Fatalf("nil context returned %q", got)
	}
}

func TestParseSessionFallbackIsExplorationScoped(t *testing.T) {
	taskID, worker := parseSession("exp12-worker-i99")
	if taskID != "12" || worker != "worker" {
		t.Fatalf("parseSession()=(%q,%q)", taskID, worker)
	}
	if taskID, worker := parseSession("not-a-task"); taskID != "" || worker != "" {
		t.Fatalf("unexpected non-session parse: (%q,%q)", taskID, worker)
	}
}

func TestCompleteForwardsAtomicResponse(t *testing.T) {
	recorder := Wrap(completeProvider{}, nil, "model", "profile", "", "", func() bool { return false })
	msg, stopReason, usage, err := recorder.Complete(context.Background(), llm.CompletionRequest{})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	if got := msg.Text(); got != "answer" {
		t.Fatalf("Complete() text=%q want answer", got)
	}
	if stopReason != "stop" {
		t.Fatalf("Complete() stop reason=%q want stop", stopReason)
	}
	if usage.InputTokens != 7 || usage.OutputTokens != 3 {
		t.Fatalf("Complete() usage=%+v", usage)
	}
}

type meteredStreamProvider struct{ completeProvider }

func (meteredStreamProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 23}}, nil) {
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, Usage: llm.Usage{OutputTokens: 5}}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func TestSideUsageRecordedOnceOnConsumerCancellation(t *testing.T) {
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	// llm_usage 는 db.Open 의 기본 스키마에 들어 있지 않고 EnsureLLMUsageTable 로만
	// 만들어진다(운영에서는 server/manager.go 가 호출한다). 격리된 빈 데이터베이스에서
	// 이 테스트를 단독으로 돌릴 때 다른 패키지가 같은 데이터베이스에 테이블을 먼저
	// 만들어 주기를 기대하지 않도록, 여기서 직접 테이블 존재를 보장한다.
	if err := pg.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	for _, early := range []bool{false, true} {
		profile := "btw-metering-" + uuid.NewString()
		ctx, cancel := context.WithCancel(transcript.WithSessionID(t.Context(), "exp0-btw-test"))
		recorder := Wrap(meteredStreamProvider{}, pg, "fixture", profile, "", "", func() bool { return false })
		for event, err := range recorder.Stream(ctx, llm.CompletionRequest{}) {
			if err != nil {
				t.Fatal(err)
			}
			if early && event.Type == llm.SEMessageDelta {
				cancel()
				break // Consumer exits before the provider can return a cancellation event.
			}
		}
		cancel()
		var count, input, output int
		var worker, status string
		err = pg.QueryRow(`SELECT count(*),max(input_tokens),max(output_tokens),max(worker),max(status)
FROM llm_usage WHERE profile_name=$1`, profile).Scan(&count, &input, &output, &worker, &status)
		_, _ = pg.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile)
		if err != nil {
			t.Fatal(err)
		}
		wantStatus := "ok"
		if early {
			wantStatus = "error"
		}
		if count != 1 || input != 23 || output != 5 || worker != "btw" || status != wantStatus {
			t.Fatalf("early=%v: count=%d usage=%d/%d worker=%q status=%q", early, count, input, output, worker, status)
		}
	}
}

func TestRequestDiagnostics_HashesProvenanceAndSecretRedaction(t *testing.T) {
	sysPrompt := "System prompt with api_key=\"sk-live-secret1234567890\" and Authorization: Bearer tok_abc123456789"
	reminder := "<system-reminder>\n# currentDate\nToday's date is 2026-11-16.\n</system-reminder>"
	userClean := "Вызови list_mcp_tools с параметрами: {\"server\":\"hexstrike\",\"agent\":\"worker\",\"live\":false}"
	toolOut := `{"server":"hexstrike","source":"cached","tool_count":36}`

	req := llm.CompletionRequest{
		System: []string{sysPrompt},
		Messages: []llm.Message{
			{
				Role: llm.RoleUser,
				Content: []llm.ContentBlock{
					llm.TextBlock(reminder),
					llm.TextBlock(userClean),
				},
			},
			{
				Role: llm.RoleAssistant,
				Content: []llm.ContentBlock{
					{
						Type:  llm.BlockToolUse,
						ID:    "call_mcp_1",
						Name:  "list_mcp_tools",
						Input: json.RawMessage(`{"server":"hexstrike","agent":"worker","live":false}`),
					},
				},
			},
			{
				Role: llm.RoleUser,
				Content: []llm.ContentBlock{
					llm.ToolResultText("call_mcp_1", toolOut, false),
				},
			},
		},
		Tools: []llm.ToolSchema{
			{Name: "list_mcp_tools", Description: "inspect mcp"},
		},
	}

	ctx := transcript.WithSessionID(context.Background(), "conv-25")
	ctx = WithWorker(ctx, "auto")

	// 1. Default mode (full payload disabled): captures hashes & provenance, no raw payload dump.
	rec := Wrap(completeProvider{}, nil, "qwen-test", "redteam-prof", "", "", func() bool { return false })
	_, _, _, err := rec.Complete(ctx, req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	diag, ok := rec.LastDiagnostics()
	if !ok {
		t.Fatal("expected LastDiagnostics to be recorded")
	}
	if diag.AgentKey != "auto" || diag.SessionID != "conv-25" || diag.Model != "qwen-test" {
		t.Fatalf("unexpected identity in diagnostics: %+v", diag)
	}
	if diag.FullPayloadMode || diag.FullPayloadDump != nil {
		t.Fatalf("default mode must not include full payload dump: %+v", diag)
	}
	if len(diag.SystemHashes) != 1 || diag.SystemHashes[0] != SHA256Hex(sysPrompt) {
		t.Fatalf("unexpected system hash: %+v", diag.SystemHashes)
	}
	if len(diag.UserMessageHashes) != 1 || diag.UserMessageHashes[0] != SHA256Hex(userClean) {
		t.Fatalf("unexpected user message hash: %+v", diag.UserMessageHashes)
	}
	if len(diag.ToolOutputs) != 1 || diag.ToolOutputs[0].ToolName != "list_mcp_tools" || !diag.ToolOutputs[0].IsMCP || diag.ToolOutputs[0].SHA256 != SHA256Hex(toolOut) {
		t.Fatalf("unexpected tool output digest: %+v", diag.ToolOutputs)
	}
	for _, wantSrc := range []string{"system-prompt", "system-reminder", "user-prompt", "tool_use", "mcp_tool_result"} {
		if !slices.Contains(diag.ContextSources, wantSrc) {
			t.Fatalf("missing context source %q in %v", wantSrc, diag.ContextSources)
		}
	}
	if len(diag.SuspiciousInputIndicators) != 0 {
		t.Fatalf("clean request must have 0 suspicious input indicators, got %v", diag.SuspiciousInputIndicators)
	}
	diagJSON, _ := json.Marshal(diag)
	if strings.Contains(string(diagJSON), "sk-live-secret1234567890") || strings.Contains(string(diagJSON), "tok_abc123456789") {
		t.Fatalf("default diagnostics leaked secrets: %s", string(diagJSON))
	}

	// 2. Full diagnostic mode enabled: includes FullPayloadDump with secrets redacted.
	rec.SetDiagnosticFullPayload(func() bool { return true })
	_, _, _, err = rec.Complete(ctx, req)
	if err != nil {
		t.Fatalf("Complete (full diag): %v", err)
	}
	fullDiag, ok := rec.LastDiagnostics()
	if !ok || !fullDiag.FullPayloadMode || fullDiag.FullPayloadDump == nil {
		t.Fatalf("expected full payload dump when enabled: %+v", fullDiag)
	}
	fullJSON, _ := json.Marshal(fullDiag)
	for _, secret := range []string{"sk-live-secret1234567890", "tok_abc123456789"} {
		if strings.Contains(string(fullJSON), secret) {
			t.Fatalf("full diagnostic payload leaked secret %q: %s", secret, string(fullJSON))
		}
	}
	if !strings.Contains(string(fullJSON), "[REDACTED]") {
		t.Fatalf("expected [REDACTED] marker in full diagnostic dump: %s", string(fullJSON))
	}

	// 3. RedactSecrets covers cookies, URL userinfo, query keys, and passwords.
	rawWithSecrets := `Cookie: session_id=xyz987654; Set-Cookie: auth=sec12345; url=https://admin:myPass999@internal.local/api?api_key=urlKey8888&token=tok7777 {"password":"pw-secret-111","x-api-key":"xkey-222"}`
	redacted := RedactSecrets(rawWithSecrets)
	for _, secret := range []string{"xyz987654", "sec12345", "myPass999", "urlKey8888", "tok7777", "pw-secret-111", "xkey-222"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("RedactSecrets leaked %q in: %s", secret, redacted)
		}
	}
}
