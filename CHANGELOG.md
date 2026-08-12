# Changelog

## Unreleased

- Panel fixes: 「刷新会话 & 模型」 now also reloads the 模型能力 table (`loadCaps`); failed node probes clear the stale latency (dead nodes show 超时 instead of the previous probe result); node-pool action buttons (重新加载订阅/解除全部标记/测速/刷新节点) moved above the table so they stay reachable with large node lists.
- Model capabilities in WebUI + `/v1/models`: fetch the models.dev catalog (async at startup, re-fetched on panel 「刷新会话 & 模型」) and attach per-model `context_window`, `max_output_tokens` and `input_modalities` (text/image/audio/video/pdf) to model listings, resolved by the real upstream model ID with alias-name fallback. Overview page gains a 「模型能力」 table showing context / max output / input types for the client-visible models (K/M token formatting, Chinese modality labels).
- Unified gateway key panel: the api_key input now has a 「生成」button that randomly generates an `sk-` prefixed key (24 bytes via `crypto.getRandomValues`, Math.random fallback) and fills the input; it takes effect after saving.
- Fix panic `invalid WriteHeader code 0` when `callOpenCodeAPI` fails at the transport level (status stays 0): non-stream responses now fall back to `502 Bad Gateway` before writing the status line; the stream path already returned 500 for transport errors.
- Unified gateway key (`config.json` `api_key`): clients that require an API key in their UI can use this single key on `/v1/*`; the gateway accepts it as authentication while upstream traffic stays on the public/free tier. No-key access remains fully functional (free tier). The key survives panel saves and is not exposed through the panel POST patch. Panel 「代理与模型」page shows the unified gateway card (reference `opencode2api_enhance` UI): the API base URL (`//host/v1`, derived from the current location) and the current key are displayed with one-click copy (clipboard API with `execCommand` fallback for non-secure contexts), and the display box refreshes immediately after saving.
- Replace the hand-rolled dialing layer (xray-core based vless/vmess/trojan, gorilla ws, sing anytls/hysteria2) with an embedded mihomo (Clash.Meta) core: the node pool is rebuilt into a mihomo config on every subscription refresh (`refreshAll`), and all protocol dialing (`socks5`/`ss`/`vless`/`vmess`/`trojan`/`hy2`/`anytls`) goes through `tunnel.Proxies()` keyed by node fingerprint. Fixes vmess+ws nodes returning empty responses that were unreachable with the previous in-process dialer; drops xray-core/gorilla dependencies. Verified with real vless/vmess/trojan ws+tls nodes (debug test `TestDebugProbeQyJP`).
- WebSocket transport for vless / vmess / trojan nodes (Clash `network: ws` + `ws-opts`, wss on TLS nodes): field mapping aligned with the reference manager (sing-box): `servername` takes precedence over `sni` for server_name; vless nodes now carry `tls`/ws fields from Clash.
- vmess / trojan node support: URI (`vmess://` v2rayN-JSON, `trojan://`) and Clash YAML parsing, manual-node panel sections, and SNI fallback to `host`. Fixes a pre-existing bug where `vmess://` nodes parsed without a protocol and were unroutable.

## v0.4.1

- Node health check (Clash url-test semantics): periodic real-probe through each node against a configurable URL (default `https://www.gstatic.com/generate_204`), 2xx judged available, per-node latency recorded and shown in the panel. Failed nodes are marked dead and skipped until the next successful probe (recovery is probe-driven, no timer); manual "测速" button runs the same probe on demand.
- Panel fixes: new authenticated `/api/models` endpoint returns the real upstream model IDs for the model-mapping dropdown (no anonymous free-only filter); "刷新会话 & 模型" now shows the real model count and re-populates the dropdown; "刷新数据" re-fetches the model list so the page visibly updates.
- Fix timestamps showing `NaN-NaN-NaN` in the panel: the run-log viewer's `fmtTime` shadowed the table version (duplicate function name).
- Stop tracking `proxy_state.json` (runtime state, now gitignored).

## v0.4.0

- Node pool with manual config and Clash/URI/base64 subscriptions (vless/reality, ss, socks5, hysteria2, anytls): in-process dialing, no mihomo dependency; subscription nodes deduped by fingerprint and refreshed periodically.
- Quota-exhaustion auto-switch: mark the current node and rotate to the next on configured `error.type` / `error.message` signals, with per-request switch limit, 24h exhausted cooldown, and 1min failure cooldown.
- Rebuilt Dark-OLED admin panel: overview stats, node pool management (switch / unmark / reload subscriptions), editable subscriptions & manual nodes & quota signals, and a proxy/misc page; partial config merge on save so panel edits never wipe unrelated settings.
- Run log viewer page: live SSE stream with `id` resume, level filtering and keyword search, runtime log-level switching (in-memory only), and plain-text export; all captured entries are redacted with the same rules as the log file.
- Fix SOCKS5 deletion from the panel (existing rows could not be removed) and stop leftover SOCKS5 config being promoted into the node pool.
- Pin CI and Docker builds to Go 1.26.

## v0.3.10

- Add `socks5_paid_direct` (default `false`): when an `active_socks5` proxy is set, keyed/paid upstream traffic also uses SOCKS5 unless this flag is explicitly enabled for the old paid-direct bypass.

## v0.3.9

- Stop cross-model upstream fallback for both public and keyed auth; retry transient 401/429/5xx and transport errors on the same requested model only.
- Treat upstream `CreditsError` / insufficient balance as non-retryable so Anthropic and Chat requests return the original billing error instead of silently trying other catalog models.

## v0.3.8

- Fix Claude Code `/v1/messages` streaming: wait for the OpenAI usage-only chunk after `finish_reason` before emitting `message_delta`, so token usage (and cache fields when present) is no longer dropped.

## v0.3.7

- Fix Docker image build: copy `go.sum` so lumberjack dependency resolves during multi-arch image builds.

## v0.3.6

- Claude Code `/v1/messages` → Chat upstream conversion: accept `x-api-key` (reject `sk-ant-`), merge mid-conversation `system` into one leading system message, convert `tool_result` images to follow-up `image_url`, map `tool_choice.disable_parallel_tool_use` → `parallel_tool_calls=false`, narrow `metadata.user_id` JSON to `session_id`, skip server tools without `input_schema`, and log intentional drops (`context_management`, `cache_control`, betas) in `request_plan`.
- Map Claude Code `output_config.effort` (`--effort` / `CLAUDE_CODE_EFFORT_LEVEL`) onto upstream `reasoning_effort`, and treat `thinking.type=adaptive` as enabled.
- Add structured request logging with lumberjack file rotation (default `opencode2api.log` + stdout), `request_id` tracing, protocol/upstream/stream summaries, secret redaction, and runtime `log_level` / `log_bodies` via `/api/config`.
- Restore `/v1/messages` default CoT passthrough for Claude Code (thinking off only when force-disabled or `thinking.type=disabled`), while keeping empty-reply fallbacks from v0.3.5.
- Fix reasoning effort: stop stripping `reasoning_effort` before upstream calls; forward Claude `thinking` (including `budget_tokens`) and derive effort from budget when needed.
- Hide upstream `-free` suffix in `/v1/models` responses and resolve stripped names back to free upstream IDs.

## v0.3.5

- Fix empty Claude Code / OpenAI replies when the Go gateway puts the answer in `reasoning_content` (#37635): promote to `content`/`text` when thinking is not requested, and fall back to a text block if a thinking-only stream would otherwise end empty.
- Temporarily made `/v1/messages` thinking opt-in; reverted in Unreleased because Claude Code lost visible CoT.

## Prior

- Projectized the provided Go program.
- Added Go module metadata, local build targets, and release packaging script.
- Added CI and tag-driven multi-platform release automation.
- Changed release automation to parallel matrix builds with a final publish job.
- Added README, API, configuration, deployment, release, contribution, and security docs.
- Added build metadata and `-version` flag.
