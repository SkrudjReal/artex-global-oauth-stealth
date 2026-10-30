# OAuth CLI profiles (Linux)

In `/system/llm/`, create a profile and select **Codex CLI (ChatGPT OAuth)** or
**Antigravity CLI / agy (Google OAuth)**. Connect, select a model from the CLI
catalog, and save. Reopen the profile to check a real response or run a message.
The Stop button, disconnecting the browser, task pause/completion, and server
shutdown cancel the running request and terminate its process group.

Codex is an ARTEX `llm.Provider`: select **Set active** for ordinary chat, or
assign the saved profile in Agent/task settings for Planner and Worker. It is
also eligible for the existing rotation chain. Native dynamic-tool requests
become structured Norma tool calls; execution and permissions stay in ARTEX.
Each ARTEX session keeps one native thread and reuses its app-server process.
Only new messages are injected; tool results answer the pending native JSON-RPC
request in the same turn. Finished threads resume by their persisted ID after a
restart. A replacement thread is created only when host history is rewritten
(compaction/edit), tool schemas change, or an interrupted/pending native request
cannot be recovered after a process restart. System instructions can be updated
on the existing thread. Session metadata and the host-history checkpoint use
atomic private files; incomplete requests are marked before native writes.


Antigravity has a **guarded MCP Provider** for new ARTEX chat, Planner, Worker
and Main Agent sessions, with real MCP calls executed only by Norma's existing
ARTEX ToolSet and permissions. CLI 1.4.11 or newer is required. It is eligible for
profile assignment. It can lead a rotation chain and fall back to API providers,
but is excluded as a fallback destination because it cannot import another
provider's history. Explicit task-chain switches to incompatible history fail
rather than fabricating messages.

The Go bridge binds only to loopback with a random 256-bit capability path, bounded
request bodies, bounded call queue and per-call timeout. Only the exact ARTEX tool
names are allowed through CLI MCP permissions. Native command, file and URL actions
are denied and the CLI sandbox stays enabled. No blanket permission bypass is used.
Each session has a private workspace MCP config and custom agent. MCP calls yield
structured ARTEX tool events; host results return on the same pending HTTP call.
Skill guidance is attached as extra text content in the real MCP tool result.
Streaming and tool events use the existing ARTEX Provider/SSE pipeline.

**Not fully API-compatible:** AGY cannot import/replace structured history,
recover a pending MCP connection after process death, or change the original system
prompt on conversation resume. Those cases fail closed and require a new ARTEX
session; actions are never replayed. Tool schemas can be reloaded between completed
turns by restarting the CLI with the same conversation ID; changing them during a
pending call is rejected. Native usage is cumulative and converted to deltas.

Observed protocol evidence on native AGY 1.4.11: custom-agent body instructed
`ARTEX_SYSTEM_A`; first reply was `ARTEX_SYSTEM_A`. After changing the body to
`ARTEX_SYSTEM_B`, restarting the CLI with `--agent` and the same `--conversation`
still returned `ARTEX_SYSTEM_A`. The adapter rejects this update explicitly.
Geminka's role-labelled text bootstrap is not used to simulate system/history or
tool calls. `open-antigravity` internal language-server access and eager command
approval are not used. A native conversation-import/system-update CLI API (or a
separately verified official SDK provider) is needed for complete API parity.


Codex native shell, subagent, browser, application and image tools are disabled;
other native approval/action requests fail closed. Its read-only sandbox remains
active. agy uses plan mode and sandboxing. ARTEX tools and MCP configuration are
exported to agy only through the session-scoped guarded MCP bridge.

Limits: dynamic tools and history injection require the installed experimental
app-server protocol (verified on 0.165.0). Codex CLI controls its output token
limit; ARTEX MaxTokens is not an exact CLI output cap. Nonzero temperature and stop
sequences are rejected explicitly; the judge's zero temperature hint uses CLI
sampling defaults. Host duration/turn limits still apply.

- Codex: persistent official `app-server` JSON-RPC, incremental notifications,
  explicit `thread/start` / `thread/resume`. Device login is supported through
  `account/login/start` (`chatgptDeviceCode`). Installed protocol schema was
  checked; `exec --json` was not needed. App-server avoids exec startup per turn.
- agy: persistent stdin/stdout NDJSON, `init`, `step_update`, `text_delta`, `result`.
  Continuation always specifies the saved conversation ID, never `--continue`.
  Interactive Google login stays in the terminal; an available model catalog is
  obtained through the CLI. Linux Secret Service/D-Bus environment is retained.
- Root: `~/artex/codex` (`CODEX_HOME`) and `~/artex/agy` (child process `HOME`).
  agy's documented home-relative `.gemini` layout was verified using the native
  Linux binary. A Windows executable found through WSL is not accepted.
- Connect reuses an existing `~/.codex/auth.json` or native
  `~/.gemini/antigravity-cli/antigravity-oauth-token` through a symlink when present.
  Credentials are never parsed by ARTEX. Disconnect detaches shared auth rather
  than revoking the original login. Without a link, Codex uses official logout;
  agy's OS-wide `/logout` must be requested explicitly in its terminal.
- Persistent mapping: profile + task ID + role + session ID -> thread ID,
  with an independent work directory. Changing a session's model requires a new
  session ID. Reuse the session ID after restarting ARTEX to resume.
- Four CLI processes maximum (including auth control), five-minute turn timeout,
  five-minute conversation idle timeout; auth control expires after 15 minutes.
  Backpressure uses bounded NDJSON queues and a ten-second SSE write deadline.
- Profiles store provider/model metadata only, using an additive format-constraint
  migration. Private state directories are 0700 and sensitive files 0600.
  Access/refresh tokens and raw CLI diagnostics never reach PostgreSQL, SSE or logs.

Official protocols: https://learn.chatgpt.com/docs/app-server and
https://antigravity.google/docs/cli/headless .
