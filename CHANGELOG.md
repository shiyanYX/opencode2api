# Changelog

## Unreleased

- 模型映射免费模型自动映射：上游目录中以 `-free` 结尾的免费模型无需再逐个手动配置别名——请求时（`resolveModel`）已自动把去后缀名映射到 `xxx-free`，管理面板「模型映射」现在自动列出这些映射行（带「自动」徽标、跟随上游模型列表变动，刷新/重载后自动更新），「添加别名」按钮保留，自定义映射（含同名覆盖自动项）照常保存；自动行右侧有「隐藏」按钮，点击后该条自动映射不再显示（适用于已下线的免费模型，如 `deepseek-v4-flash-free`）；`collectAliases` 跳过自动行，不会把派生映射写回 `config.json`。`config.example.json` 精简为单个自定义别名示例，`docs/CONFIGURATION.md` 与 `README.md` 同步说明，DEMO 数据补充 `-free` 模型演示自动行。
- 免费模型已下线错误改写：当下游请求的免费模型（通过自动映射解析到 `-free` 上游）已停止服务时，上游返回的原始错误信息（如 "Free promotion has ended"）会被网关改写为清晰的中文提示——"该免费模型已停止服务，请更换其他免费模型（管理面板 → 模型映射 中可查看当前可用的免费模型），或通过订阅获取更多模型。"，错误类型标记为 `free_model_ended`，原始类型保留在 `original_type` 字段。该改写覆盖 OpenAI/Anthropic/Responses 三种协议的所有错误路径（stream + 非 stream）。
- 概览·模型能力只显示免费模型：表格过滤为仅 `-free` 上游模型（带「免费」徽标并高亮），与「模型映射」自动行数量一致；`/api/models` 从纯 ID 字符串改为完整模型对象，并填充 models.dev 目录的上下文窗口/最大输出/输入模态。
- 自动映射行与手动别名独立展示：修复免费模型同时存在付费版与免费版（如 `hy3` + `hy3-free`）时，请求去后缀名不再映射到免费版的问题——`resolveModel` 现在只要 `xxx-free` 存在于上游目录就总是映射过去（本项目只服务免费模型），手动别名仍优先。模型映射表中自动行不再因同名手动别名而隐藏，两者独立展示。
- 自动映射支持取消隐藏：模型映射中被隐藏的自动行以「已隐藏」状态淡显展示，右侧「取消隐藏」按钮可恢复显示。

## v0.4.5

- 节点状态恢复语义（#1）：`exhausted` 改为**定时恢复**——配额冷却到期后由惰性清扫 `sweepExpiredLocked()` 在路由/快照入口把节点翻回 `available` 并清空标记（原实现冷却一到期就放行流量、但 State 从不清回，导致面板徽标/`exhausted_count` 与路由选路脱节；重启后 past-cooldown 标记也立即放行）。`dead` 改为**事件恢复**：`eligible()` 塌缩为只认 `available`，dead 仅由探测成功解除；健康循环每分钟复探 dead 节点（`node_cooldown_dead_minutes` 语义重定义为复探间隔，默认 1 分钟，故障恢复延迟从 ≤15 分钟降到 ≤1 分钟）；调度巡检只探测 `available`（跳过 exhausted），面板手动「测速」才全量探测。`markProbeDead` 对已 dead 节点幂等（复探失败仅记录，不再重复标记/延长冷却/刷日志）。面板「故障冷却（分钟）」标签改为「故障复探间隔（分钟）」，配额提示补充自动恢复语义。
- 配额切换预算真正生效（#2）：`callOpenCodeAPI` 与 `callOpenCodeAPIStream` 的重试循环上限从 `max(重试3, 401重试3)=3` 改为「重试上限 3 + 配额预算 5」= 8——配额切换 `continue` 不再挤占重试次数，`max_quota_node_switches`（默认 5）预算首次真正可达（旧实现最多切 2-3 个节点即被循环上限截断）。普通重试（401/429/5xx）仍由 `canRetry` 闸门封顶 3 次，预算用尽后落回原有错误返回。面板配额「耗尽冷却（小时）」空值回退修正为 1（原先 '24' 与渲染默认不一致）。
- 测试：新增节点状态恢复语义测试 6 项（清扫翻回/冷却内不翻/manual 与快照清扫/dead 塌缩语义/巡检跳过 exhausted/markProbeDead 防刷屏）与配额预算测试 5 项（预算 5 生效/预算耗尽返回末错/重试仍封顶 3/切换后重试封顶/流式同测）；新增 `docs/adr/0001-node-state-recovery.md`，`CONTEXT.md` 补充节点状态/复探间隔/配额预算术语，`docs/CONFIGURATION.md` 与 `README.md` 同步恢复语义与预算说明。

## v0.4.4

- Webshare 代理池支持按源配置 `proxy_url`（webshare 面板「代理」列 / `config.json` `webshare[].proxy_url`）：拉取 webshare API 时走指定代理，支持 `http://`、`https://`、`socks5://`；留空则回退进程环境变量 `HTTP(S)_PROXY`（直连）。适用于本机到 webshare 网络不佳、需经中转的场景。面板表格新增代理列并随配置保存、回显。

## v0.4.3

- Webshare proxy pool: `config.json` `webshare[]` sources (name / api_key / mode / update_interval_hours) are fetched from the webshare.io API v2 proxy list (`Authorization: Token`, paginated page_size=100, `valid != false` only, string-or-number ports) and merged into the node pool as SOCKS5 nodes (`source::US-addr:port`, `webshare API` snapshot info shown in the panel). Panel 「订阅源」page gains a Webshare card with per-source node count / last fetch / error; saved via `/api/nodes` `save` from `webshare` rows. Deploy support: entrypoint generates the `webshare` config section on first boot from `OPENCODE2API_WEBSHARE_API_KEY` / `_NAME` / `_MODE` / `_INTERVAL` (optional, coexist with Tor/WARP socks5), all three compose templates pass the variables through, example config + docs updated.
- Quota panel fixes: GET `/api/config` now echoes the effective defaults when the config file omits them, so the error.type / error.message / switch-limit / cooldown boxes show real values (default signatures `FreeUsageLimitError, insufficient_quota, credits_error, billing_error` and keywords `free usage limit, quota, insufficient, limit exceeded`); `applyConfig` treats explicit empty signal arrays as "use defaults" so saving empty text boxes can no longer silently disable quota detection. Tests cover echo + empty-array fallback.
- `config.example.json` uses an empty `"webshare": []` so copy-and-run deployments don't trigger real API calls with a placeholder key.
- Call log gains cache token accounting: `cache_creation_tokens` / `cache_read_tokens` per record, normalized across OpenAI (`cache_creation_input_tokens` / `cache_read_input_tokens` / `prompt_tokens_details.cached_tokens`) and Claude (`input_tokens_details.cached_tokens`) usage shapes; usage trends buckets now include input / output / cache-created / cache-read tokens plus a cache hit-rate legend (hit ÷ (hit + create), hidden when both are zero), refreshed by the existing call-log aggregation.
- Dockerfile sets `TZ=Asia/Shanghai` (trend charts and call log bucket by process-local time) and `GOPROXY=https://goproxy.cn,direct` for builds.
- Health-check probe timeouts no longer overwrite a quota-exhausted marker: `markProbeDead` now keeps `NodeExhausted` (state, cooldown, error reason) untouched and only logs; exhaustion recovery stays on the quota cooldown / manual reset instead of being downgraded to a dead mark with a probe-interval cooldown.
- Quota-exhausted node cooldown default shortened from 24h to 1h: `mark(NodeExhausted)` now cools the node down for 1h by default (once daily-reset style quotas are per-IP/account and usually recover much earlier than 24h; node rotation via `max_quota_node_switches` stays the primary mitigation). Panel defaults and the quota-stats "自动恢复" label updated accordingly; `node_cooldown_exhausted_hours` config override unchanged.
- Fix crash `fatal error: sync: unlock of unlocked mutex`: `refreshOCSession` reset the shared `sync.Once` while another request was inside `initOCSession`'s `Once.Do`, corrupting the Once state under concurrent quota-switch / panel-reload traffic. Replaced the Once with a mutex + done flag (same semantics: refresh marks the session for re-initialization) and updated the test stubs.
- Call log (参考 opencode2api_enhance 的调用日志设计): every upstream request is recorded as one structured `CallRecord` (req_id / ts / path / model / stream / route mode / node chain / event timeline / status / tokens / duration / err_msg) into an in-memory ring buffer (2000) with JSONL persistence next to `config.json` (restored on restart; `/api/call-log` GET reads latest N, DELETE clears). Events are captured inside `callOpenCodeAPI` / `callOpenCodeAPIStream` (`connect_ok` / `connect_error` / `upstream_error` / `switch` on quota rotation) and finished by the three protocol handlers (chat completions, Anthropic messages, responses), including `stream_interrupt` when a stream ends without `[DONE]` or errors mid-flight. The panel 运行日志 page gains a 原始日志 / 调用日志 tab switch; the call-log view shows the summary bar (共/成功/失败/异常切换), keyword / per-day / only-issues filters, expandable event timelines for failed or switched requests, and two analysis views: 时段分析 (24h bar chart + per-hour table) and 节点分析 (per-node success-rate bar + table), auto-refreshing every 5s (demo data in demo mode).
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
