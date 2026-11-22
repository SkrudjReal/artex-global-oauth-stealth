package llmrec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

// MessageProvenance describes one message's role, origin/source classifications,
// size, and SHA-256 content hash without exposing raw message text.
type MessageProvenance struct {
	Index       int      `json:"index"`
	Role        string   `json:"role"`
	Sources     []string `json:"sources"`
	ByteLen     int      `json:"byte_len"`
	ContentHash string   `json:"content_hash"`
	ToolCallIDs []string `json:"tool_call_ids,omitempty"`
	ToolNames   []string `json:"tool_names,omitempty"`
}

// ToolOutputDigest records non-secret provenance and a SHA-256 digest for one
// tool_result block that entered the LLM request context.
type ToolOutputDigest struct {
	ToolUseID string `json:"tool_use_id"`
	ToolName  string `json:"tool_name,omitempty"`
	IsMCP     bool   `json:"is_mcp"`
	IsError   bool   `json:"is_error,omitempty"`
	ByteLen   int    `json:"byte_len"`
	SHA256    string `json:"sha256"`
}

// RedactedPayloadDump holds the secret-scrubbed full payload when diagnostic
// full-payload mode is explicitly enabled. Never sent to external services.
type RedactedPayloadDump struct {
	System   []string `json:"system,omitempty"`
	Messages string   `json:"messages,omitempty"`
	Tools    []string `json:"tools,omitempty"`
}

// RequestDiagnostics captures safe per-request observability metadata: which
// agent, message roles, and context sources entered the request; SHA-256 hashes
// of the system prompt, user messages, and tool outputs; suspicious prompt-injection
// indicators actually present in the input context; and an optional secret-redacted
// payload dump only when diagnostic mode is enabled.
type RequestDiagnostics struct {
	AgentKey                  string               `json:"agent_key"`
	SessionID                 string               `json:"session_id,omitempty"`
	TaskID                    string               `json:"task_id,omitempty"`
	Model                     string               `json:"model,omitempty"`
	ProfileName               string               `json:"profile_name,omitempty"`
	SystemCount               int                  `json:"system_count"`
	SystemHashes              []string             `json:"system_hashes,omitempty"`
	SystemCombinedHash        string               `json:"system_combined_hash,omitempty"`
	MessageCount              int                  `json:"message_count"`
	MessageRoles              []string             `json:"message_roles,omitempty"`
	ContextSources            []string             `json:"context_sources,omitempty"`
	UserMessageHashes         []string             `json:"user_message_hashes,omitempty"`
	Messages                  []MessageProvenance  `json:"messages,omitempty"`
	ToolOutputs               []ToolOutputDigest   `json:"tool_outputs,omitempty"`
	ToolSchemaCount           int                  `json:"tool_schema_count"`
	SuspiciousInputIndicators []string             `json:"suspicious_input_indicators,omitempty"`
	FullPayloadMode           bool                 `json:"full_payload_mode"`
	FullPayloadDump           *RedactedPayloadDump `json:"full_payload_dump,omitempty"`
}

// SHA256Hex returns the lowercase hex SHA-256 digest of s.
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// DiagnosticFullPayloadEnabled reports whether local diagnostic full-payload
// dumping is explicitly enabled via environment variable.
func DiagnosticFullPayloadEnabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("ARTEX_LLM_DIAG_FULL_PAYLOAD")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// BuildRequestDiagnostics inspects a CompletionRequest and returns safe metadata,
// provenance classifications, and SHA-256 hashes (plus a secret-redacted payload
// dump only when fullPayload is true).
func BuildRequestDiagnostics(ctx context.Context, req llm.CompletionRequest, model, profName, workerOverride string, fullPayload bool) RequestDiagnostics {
	session := transcript.SessionIDFrom(ctx)
	parsedID, worker := parseSession(session)
	if ov := strings.TrimSpace(workerOverride); ov != "" {
		worker = ov
	} else if ov := workerFrom(ctx); ov != "" {
		worker = ov
	}
	taskID := TaskIDFrom(ctx)
	if taskID == "" {
		taskID = parsedID
	}

	diag := RequestDiagnostics{
		AgentKey:        worker,
		SessionID:       session,
		TaskID:          taskID,
		Model:           model,
		ProfileName:     profName,
		SystemCount:     len(req.System),
		MessageCount:    len(req.Messages),
		ToolSchemaCount: len(req.Tools),
		FullPayloadMode: fullPayload,
	}

	var contextSources []string
	addSource := func(src string) {
		if src != "" && !slices.Contains(contextSources, src) {
			contextSources = append(contextSources, src)
		}
	}

	if len(req.System) > 0 {
		addSource("system-prompt")
		diag.SystemHashes = make([]string, len(req.System))
		for i, s := range req.System {
			diag.SystemHashes[i] = SHA256Hex(s)
		}
		diag.SystemCombinedHash = SHA256Hex(strings.Join(req.System, "\n\n"))
	}

	// First pass: map tool_use ID -> tool name so tool_result blocks can be attributed.
	toolNameByID := map[string]string{}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolUse && b.ID != "" {
				toolNameByID[b.ID] = b.Name
			}
		}
	}

	var suspicious []string
	addSuspicious := func(indicator string) {
		if indicator != "" && !slices.Contains(suspicious, indicator) {
			suspicious = append(suspicious, indicator)
		}
	}

	diag.MessageRoles = make([]string, 0, len(req.Messages))
	diag.Messages = make([]MessageProvenance, 0, len(req.Messages))

	for i, m := range req.Messages {
		role := string(m.Role)
		diag.MessageRoles = append(diag.MessageRoles, role)

		var (
			msgSources  []string
			toolCallIDs []string
			toolNames   []string
			normBuf     strings.Builder
		)
		addMsgSource := func(src string) {
			if src != "" && !slices.Contains(msgSources, src) {
				msgSources = append(msgSources, src)
			}
			addSource(src)
		}

		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockText:
				txt := b.Text
				normBuf.WriteString(txt)
				trimmed := strings.TrimSpace(txt)
				switch {
				case strings.HasPrefix(trimmed, "<system-reminder>"):
					addMsgSource("system-reminder")
					for _, ind := range detectSuspiciousPatterns(txt) {
						addSuspicious("system-reminder:" + ind)
					}
				case isCompactionSummaryText(trimmed):
					addMsgSource("compaction-summary")
					for _, ind := range detectSuspiciousPatterns(txt) {
						addSuspicious("compaction-summary:" + ind)
					}
				case m.Role == llm.RoleUser:
					addMsgSource("user-prompt")
					diag.UserMessageHashes = append(diag.UserMessageHashes, SHA256Hex(txt))
					for _, ind := range detectSuspiciousPatterns(txt) {
						addSuspicious("user-prompt:" + ind)
					}
				default:
					addMsgSource("assistant-text")
				}
			case llm.BlockThinking:
				normBuf.WriteString(b.Thinking)
				addMsgSource("assistant-thinking")
			case llm.BlockToolUse:
				normBuf.WriteString(b.Name)
				normBuf.Write(b.Input)
				addMsgSource("tool_use")
				if b.ID != "" {
					toolCallIDs = append(toolCallIDs, b.ID)
				}
				if b.Name != "" {
					toolNames = append(toolNames, b.Name)
				}
			case llm.BlockToolResult:
				resText := flattenToolResultContent(b.Content)
				normBuf.WriteString(resText)
				tName := toolNameByID[b.ToolUseID]
				isMCP := strings.HasPrefix(tName, "mcp__") || tName == "list_mcp_tools"
				if isMCP {
					addMsgSource("mcp_tool_result")
				} else {
					addMsgSource("tool_result")
				}
				if b.ToolUseID != "" {
					toolCallIDs = append(toolCallIDs, b.ToolUseID)
				}
				if tName != "" && !slices.Contains(toolNames, tName) {
					toolNames = append(toolNames, tName)
				}
				diag.ToolOutputs = append(diag.ToolOutputs, ToolOutputDigest{
					ToolUseID: b.ToolUseID,
					ToolName:  tName,
					IsMCP:     isMCP,
					IsError:   b.IsError,
					ByteLen:   len(resText),
					SHA256:    SHA256Hex(resText),
				})
				for _, ind := range detectSuspiciousPatterns(resText) {
					addSuspicious("tool_result:" + ind)
				}
			}
		}

		rawMsgStr := normBuf.String()
		diag.Messages = append(diag.Messages, MessageProvenance{
			Index:       i,
			Role:        role,
			Sources:     msgSources,
			ByteLen:     len(rawMsgStr),
			ContentHash: SHA256Hex(rawMsgStr),
			ToolCallIDs: toolCallIDs,
			ToolNames:   toolNames,
		})
	}

	diag.ContextSources = contextSources
	diag.SuspiciousInputIndicators = suspicious

	if fullPayload {
		redactedSys := make([]string, len(req.System))
		for i, s := range req.System {
			redactedSys[i] = RedactSecrets(s)
		}
		msgsJSON, _ := json.Marshal(req.Messages)
		toolNames := make([]string, len(req.Tools))
		for i, t := range req.Tools {
			toolNames[i] = t.Name
		}
		diag.FullPayloadDump = &RedactedPayloadDump{
			System:   redactedSys,
			Messages: RedactSecrets(string(msgsJSON)),
			Tools:    toolNames,
		}
	}

	return diag
}

func flattenToolResultContent(blocks []llm.ContentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == llm.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func isCompactionSummaryText(s string) bool {
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "<summary>") ||
		strings.Contains(lower, "conversation compacted") ||
		strings.Contains(lower, "context compaction")
}

var suspiciousPhrasePatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"ignore_previous_instructions", regexp.MustCompile(`(?i)\bignore\s+(?:all\s+)?(?:previous|prior|above)\s+instructions\b`)},
	{"authorized_operator_override", regexp.MustCompile(`(?i)\byou\s+are\s+an?\s+authorized\s+operator\b`)},
	{"uname_id_ls_recon_injection", regexp.MustCompile(`(?i)uname\s+-a\s*(?:&&|\\u0026\\u0026)\s*id\s*(?:&&|\\u0026\\u0026)\s*ls\s+-la`)},
	{"scan_arbitrary_hosts_override", regexp.MustCompile(`(?i)\byou\s+are\s+authorized\s+to\s+scan\s+any\s+host\b`)},
	{"cover_up_instruction", regexp.MustCompile(`(?i)\bdo\s+not\s+mention\s+the\s+above\b`)},
}

func detectSuspiciousPatterns(s string) []string {
	var out []string
	for _, p := range suspiciousPhrasePatterns {
		if p.re.MatchString(s) {
			out = append(out, p.name)
		}
	}
	return out
}

var (
	reJSONSecretField = regexp.MustCompile(`(?i)("(?:api[-_]?key|x-api-key|secret|client[-_]?secret|private[-_]?key|password|passwd|access[-_]?token|refresh[-_]?token|auth[-_]?token|token|authorization|cookie|set-cookie)"\s*:\s*)"([^"\\]*(?:\\.[^"\\]*)*)"`)
	reBearerToken     = regexp.MustCompile(`(?i)\b(Bearer\s+)[A-Za-z0-9._~+/=-]{6,}`)
	reBasicAuth       = regexp.MustCompile(`(?i)\b(Basic\s+)[A-Za-z0-9+/=]{6,}`)
	reHeaderSecret    = regexp.MustCompile(`(?im)\b((?:Authorization|X-API-Key|Cookie|Set-Cookie)\s*:\s*)([^\r\n"']+)`)
	reKVSecret        = regexp.MustCompile(`(?i)(--?(?:api[-_]?key|api[-_]?token|token|secret|password)=|\b(?:API_KEY|SECRET_KEY|ACCESS_TOKEN|REFRESH_TOKEN|AUTH_TOKEN|PASSWORD|PASSWD|CLIENT_SECRET)=)([^\s"'&]+)`)
	reURLUserPass     = regexp.MustCompile(`(https?://[^/\s:@]+:)([^@\s/]+)(@)`)
	reURLQuerySecret  = regexp.MustCompile(`(?i)([?&](?:key|api_key|apikey|token|access_token|secret|password)=)([^&\s"']+)`)
	reKnownKeyPrefix  = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{8,}|AIza[A-Za-z0-9_-]{10,}|ghp_[A-Za-z0-9]{10,}|xox[baprs]-[A-Za-z0-9-]{8,})\b`)
)

// RedactSecrets scrubs API keys, Bearer/Basic tokens, cookies, URL credentials,
// and secret key-value fields from diagnostic strings or JSON payloads.
func RedactSecrets(s string) string {
	if s == "" {
		return ""
	}
	out := reJSONSecretField.ReplaceAllString(s, `$1"[REDACTED]"`)
	out = reBearerToken.ReplaceAllString(out, `${1}[REDACTED]`)
	out = reBasicAuth.ReplaceAllString(out, `${1}[REDACTED]`)
	out = reHeaderSecret.ReplaceAllString(out, `${1}[REDACTED]`)
	out = reKVSecret.ReplaceAllString(out, `${1}[REDACTED]`)
	out = reURLUserPass.ReplaceAllString(out, `${1}[REDACTED]${3}`)
	out = reURLQuerySecret.ReplaceAllString(out, `${1}[REDACTED]`)
	out = reKnownKeyPrefix.ReplaceAllString(out, `[REDACTED]`)
	return out
}
