# 上游客户端形态兼容（OpenCode Zen 免费层）

OpenCode Zen 的免费层**不按账号鉴权，而按「请求像不像官方 opencode 客户端」鉴权**。上游一旦收紧
形态校验，代理立刻全线 403，本地没有任何改动——这是设计如此，不是 bug。本文记录已实测的判定条件、
本项目的适配实现，以及上游再次变更时的重新定位流程。

> 现象固定为：
> ```json
> 403 {"type":"error","error":{"type":"FreeTierError",
>      "message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}
> ```

## 一、已实测的四道条件

截至 2026-09-18，以下条件**必须同时满足**，缺一即 403：

| # | 条件 | 本项目实现 |
|---|---|---|
| 1 | `x-opencode-session` = `ses_` + 12 位小写十六进制 + 14 位 base62（前缀后固定 26 字符） | `opencode_id.go` 的 `newOpenCodeID` 复刻官方 `Identifier.create` |
| 2 | `User-Agent` = `opencode/<semver>`，且不低于 1.17.0 | `main.go` 取 npm `opencode-ai` latest，回退 `1.18.31` |
| 3 | `body.stream` 必须为 `true` | `agent_shape.go` 强制置真；非流式客户端由 `aggregateUpstreamSSE` 本地聚合 |
| 4 | `body.tools` 必须包含官方小写工具名 `bash`、`glob`、`grep`、`read` | `agent_shape.go` 的 `shapeToolsForUpstream` 规范化 + 补齐 |

### 条件 1：session 形态（2026-09-17 上线）

官方 CLI 的 ID 布局（`sst/opencode` `packages/opencode/src/id/id.ts`，`LENGTH = 26`）：

```
<prefix>_<6 字节时间戳的 12 位小写十六进制><14 位 base62>
```

实测（同一出口 IP、同一请求体，仅改 session 值）：

| session 值 | 结果 |
|---|---|
| `ses_` + 12 位小写 hex + 14 位任意 | 200 |
| `ses_` + 26 位小写 hex | 200 |
| `ses_` + 24 位小写字母数字（旧实现 `randomString(24)`） | 403 |
| `ses_` + `"z"*26`，或前 12 位含非十六进制字符 | 403 |
| 长度 25 / 27 / 28 / 32 | 403 |
| 真实 CLI 的 session | 200 |
| 真实 CLI 的 session，改首字符 | 403 |
| 真实 CLI 的 session，改末字符 | 200 |

⇒ 只有**前 12 位必须是小写十六进制**，后 14 位内容自由，前缀后长度必须恰好 26。
12 位时间分量**不校验时效**（随机值、一年前的时间戳都能过）。

### 条件 3：stream 必须为 true（2026-09-17 上线）

| body | 结果 |
|---|---|
| `stream: true` | 200 |
| `stream: false` | 403 |
| 无 `stream` 字段 | 403 |

### 条件 4：必需工具名（2026-09-18 上线）

| tools 内容 | 结果 |
|---|---|
| `bash, glob, grep, read`（仅名字，schema 全空） | 200 |
| 上述四个，任意顺序 | 200 |
| 缺 `glob` / `grep` / `bash` / `read` 任一个 | 403 |
| **缺 `edit` 或 `write`** | **200**（必需集合只有四个） |
| 同名工具重复出现（如 `bash` 两次，或 `Bash` + `bash`） | **400**（不是 403） |
| 工具名写成 `Bash` / `Read`（大小写不同） | 403（大小写敏感） |
| 四个必需名 + 6 / 30 / 60 个自定义工具 | 200 |
| 四个必需名使用官方真实 schema | 200 |
| 11 个同体积占位名（`t0`…`t10`） | 403 |

**与上述条件无关的量**（均已实测）：body 大小、system prompt 内容、工具 schema、
`max_tokens`、`tool_choice`（`auto` / `required` / 指向不存在的工具都 200）、
`x-opencode-project`、`x-opencode-request`、`Authorization`（`Bearer public` 与真实 key 均可）。

### 旁证：官方客户端自己也会踩条件 4

官方 CLI 的 auto-compaction 请求是唯一以「单条超大 user 消息 + `tools: []` + 无 system prompt」
发出的请求，因此官方客户端本身的压缩步骤也会被同一道闸门拒绝，而普通 build 请求正常
（见 upstream issue 讨论）。这与条件 4 的实测结论一致。

## 二、为什么工具名要「规范化 + 回映射」而不是「追加」

直接追加官方名会让上游同时看到 `Bash`（客户端，带完整 schema）与 `bash`（我们补的空壳），
模型可能选中空壳版本，导致参数不符合客户端 schema。因此采用：

1. 客户端工具名**忽略大小写命中官方名**时，改写成官方小写名，**沿用客户端自己的 schema**；
2. 大小写无关**去重**（上游对重名返回 400，这是硬要求）；
3. 补齐仍然缺失的必需工具（空壳，`description` 标明不可用）；
4. **客户端完全没带工具**时才设 `tool_choice: "none"`，禁止模型调用空壳；
   客户端自带工具时绝不设置，否则会禁掉它自己的工具；
5. 响应侧按 `上游名 -> 客户端原名` 的映射还原，并丢弃空壳工具产生的调用。

结果是**下游 agent 完全无感**：工具名、schema、`tool_choice` 契约都不变。已实测覆盖：

| 场景 | 结果 |
|---|---|
| chat 非流式 + Claude Code 风格大写工具（要求模型调用） | 200，`tool_calls[0].function.name == "Bash"`（客户端原名） |
| chat 流式 + 同上 | 200，SSE 中工具名为 `Bash`，无空壳名泄漏 |
| chat 非流式 + 无工具 | 200，无空壳名泄漏 |
| `/v1/messages` 非流式 + 大写工具 | 200，`tool_use.name == "Bash"` |
| `/v1/messages` 流式 + 同上 | 200，工具名为 `Bash` |

流式路径由 `newToolNameRewriter` 包装上游 SSE 统一收口（逐行改写，保持分帧与 `[DONE]` 不变），
三条协议面（Chat / Anthropic / Responses）共用，无需各自处理。

## 三、上游再次变更时怎么重新定位

上游近 36 小时内连续上线了三道闸门（session 形态 → stream → 工具名）。再次出现全线 403 时按以下顺序做，
**不要猜**：

1. **先确认是上游门槛而不是限流或本地故障**
   - 报错是资格类拒绝（`FreeTierError`）而不是 `FreeUsageLimitError`；换出口 IP 无效 → 不是 IP 配额问题。
   - 用**真实 opencode CLI** 发同一模型：能通 → 门槛是「像不像官方客户端」；否则才是账号/上游故障。
2. **抓真实客户端的请求**
   - 本地起一个 CONNECT 代理（自签 CA），给 CLI 设 `HTTPS_PROXY` 与 `NODE_EXTRA_CA_CERTS`，
     完整 dump 出 headers 与 body（含 `tools` 全文）。
   - 也可直接读上游源码：请求构造在 `packages/opencode/src/session/llm/request.ts`，
     ID 在 `packages/opencode/src/id/id.ts`，UA 在 `installation/index.ts`。
3. **逐字段二分**
   - 以「真实请求 → 200」为基线，**每次只把一个字段换成我们的值**，逐个定位到失配项。
   - 顺序建议：`User-Agent` → `x-opencode-session` → `body.stream` → `body.tools` → 其余头。
   - 注意保持其余字段不变，否则会得到互相矛盾的结论。
4. **把新结论补进本文档的矩阵**，并在 `agent_shape.go` / `opencode_id.go` 对应位置实现。

定位工具（可复用，未纳入仓库）：本地 CONNECT 代理 + 自签 CA 约 130 行 Python；二分脚本直接
`ssl.wrap_socket` 连上游、按列表构造 header 块并断言状态码。

## 四、边界与风险

- **这是对抗性适配，不是稳定接口**。上游未公开承诺这些形态，任何一次收紧都可能让代理失效；
  重新定位的成本约为「一次 MITM + 一轮二分」。
- 免费层额度本身仍按出口 IP 限流（`FreeUsageLimitError` 429），与本文的四道闸门是两回事：
  前者靠切换节点缓解，后者只能靠形态对齐。
- 条件 3 使所有非流式请求在上游侧变为流式，本网关需本地聚合（内存占用按 `max_tokens` 有界，
  用量从末尾 chunk 取）。
- 若上游进一步转向「只有真客户端能通」（例如对 TLS/连接层做绑定），本方案将失效；
  届时应评估把免费模型降级为 best-effort，或转向付费/其它渠道。
