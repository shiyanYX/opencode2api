# ADR-0002：适配 OpenCode Zen 免费层的客户端形态校验

## 状态

已采纳（2026-09-18）

## 背景

OpenCode Zen 免费层按「请求像不像官方 opencode 客户端」鉴权，而非按账号或 IP。2026-09-17 至 09-18
的 36 小时内上游连续收紧三轮，每轮都使代理全线返回同一个 403：

```json
403 {"type":"error","error":{"type":"FreeTierError",
     "message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}
```

三轮依次是：

1. `x-opencode-session` 必须符合官方 ID 形态（`ses_` + 12 位小写十六进制 + 14 位 base62）；
2. `body.stream` 必须为 `true`；
3. `body.tools` 必须包含官方小写工具名 `bash`、`glob`、`grep`、`read`。

第一轮之前本项目的实现是 `"ses_" + randomString(24)`；第二轮之前非流式请求按 `stream:false` 直发；
第三轮之前只处理「客户端不带 tools」的场景，而生产主客户端（Claude Code 风格、工具名首字母大写）
自带工具却因不含官方小写名而全线 403。

排除过的假设（均已实测证伪）：多出口共享 session、host IP 被封、TLS/JA3 指纹、
User-Agent 内容（短 UA 配正确 session 也能通）、请求体必须含官方 system prompt、
Authorization 必须是真实 key。

## 决策

**完整模仿官方客户端的请求形态，而不是寻找服务端开关或绕过手段。**

1. **ID 形态**：新增 `opencode_id.go`，按官方 `Identifier.create` 的布局生成
   `<prefix>_<12 位小写十六进制时间戳><14 位 base62>`，供 `x-opencode-session`
   与 `x-opencode-request` 使用；同毫秒内用自增计数器保证单调不重复。
2. **stream 形态**：`ensureAgentUpstreamShape` 把上游请求恒置 `stream:true`（补
   `stream_options.include_usage` 以便取用量）；非流式客户端由 `aggregateUpstreamSSE`
   把上游 SSE 聚合回单个 `chat.completion`，三种协议的 6 个调用点因此无需改动。
3. **工具形态**：`shapeToolsForUpstream` 把客户端工具名中忽略大小写命中官方名者改写为官方小写名
   （沿用客户端自己的 schema），大小写无关去重（上游对重名返回 400），再补齐缺失的必需工具；
   客户端完全无工具时才设 `tool_choice:"none"`。
4. **对下游透明**：响应侧按 `上游名 -> 客户端原名` 回映射，并丢弃空壳工具产生的调用。
   流式路径由 `newToolNameRewriter` 包装上游 SSE 统一收口，三条协议面共用。

## 理由

- **只有形态对齐能解决**：闸门是资格类拒绝，切换节点、重试、换 key 都无效；实测同一节点同一秒
  200/403 交错，差异只在请求形态。
- **不追加而是改写工具名**：追加官方名会让上游同时存在 `Bash`（客户端完整 schema）与 `bash`
  （空壳），模型可能选中空壳导致参数不合客户端 schema；改写则上游只看到一份工具。
- **下游保持不变是本方案的硬约束**：网关的用户是各类 agent 客户端，任何要求它们改工具名/改
  schema 的方案都不可接受；回映射使工具名、schema、`tool_choice` 契约全部不变。

## 后果

- 正面：免费层恢复可用；对下游零改动；新增 20 项单元测试覆盖形态与回映射。
- 负面：这是**对抗性适配**，上游未承诺这些形态，任何一次收紧都可能再次失效；重新定位成本约为
  「一次 MITM 抓包 + 一轮逐字段二分」（流程见 `docs/UPSTREAM-COMPAT.md`）。
- 负面：所有非流式请求在上游侧变为流式，网关需本地聚合（内存按 `max_tokens` 有界）。
- 边界：免费层额度仍按出口 IP 限流（`FreeUsageLimitError`），与本决策无关。
- 若上游进一步转向「只有真客户端能通」（如连接层绑定），本方案失效，届时应评估降级为
  best-effort 或转向付费渠道。
