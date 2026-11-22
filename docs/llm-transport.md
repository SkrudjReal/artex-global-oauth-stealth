# LLM transport compatibility

All API endpoints use normal HTTP protocol negotiation and incremental streaming by default. No gateway hostname receives special handling.

For a gateway that requires HTTP/1.1, set `ARTEX_LLM_HTTP1_ONLY=1` before starting ARTEX. This affects API providers in that ARTEX process; it does not change CLI OAuth or MCP connections.

Set `ARTEX_LLM_BUFFER_STREAM=1` to hold a whole streamed model reply until it completes. Incomplete replies caused by a transport error are discarded and retried using the configured stream retry count and interval (default: two retries). Text appears after completion instead of incrementally. Uncommitted tool calls are never executed. Cancellation stops retries, and deterministic API errors are not retried by this wrapper. Failed attempts may still be billed by the gateway.

These compatibility switches are opt-in process settings. They do not repair a VPN, proxy or upstream that closes every long request. Compare the same request on another permitted network route before increasing retries. `ARTEX_LLM_PROXY` configures environment-based providers; persisted profiles use their own proxy setting. An empty proxy setting means a direct connection and ignores shell HTTP proxy variables.

LLM transport diagnostics log host, protocol, failure stage, byte count and elapsed time, without prompt bodies, API credentials or URL query strings.
