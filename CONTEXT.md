# Context — opencode2api

## Glossary

- **日志分级（log level filtering & switching）**：WebUI 运行日志页的两项能力——(1) 按 `debug/info/warn/error` 级别过滤日志展示；(2) 运行时切换进程日志级别（仅影响当前进程内存，不持久化，重启恢复启动参数配置）。
- **用量趋势（usage trends）**：WebUI 概览页的统计图卡片（替换原「实时趋势」），由调用日志（call_log）按时间聚簇驱动，每个时段桶含请求/成功/失败/输入/输出/缓存创建/缓存读取 Token（图例另含缓存命中率 = 命中÷(命中+创建)，双零时隐藏）；支持今日（小时粒度）/7 天/30 天（天粒度）范围切换，以及可配置的自动刷新（关闭/5s/10s/30s/60s，不持久化，仅当前会话内存）。缓存字段与 OpenAI/Claude 双格式归一（cache_creation_input_tokens、cache_read_input_tokens、prompt_tokens_details.cached_tokens、input_tokens_details.cached_tokens），流式请求的 token 在流结束时补录。
- **节点状态（NodeAvailable / NodeExhausted / NodeDead）**：节点池运行时状态机，`eligible()`（`proxy_node.go`）只认 `available`，其余两种状态一律不参与路由。`exhausted`=配额耗尽标记（冷却 `node_cooldown_exhausted_hours`，默认 1h），**定时恢复**：冷却到期由 `sweepExpiredLocked()` 在路由/快照入口清扫翻回 available 并清空标记（MarkedAt/CooldownUntil/LastError）。`dead`=健康探测失败标记，**事件恢复**：仅由探测成功（`recordProbeSuccess`）解除，冷却时间不参与判定（冷却到期不会让 dead 复活）。恢复机制不对称是刻意的：exhausted 可预期自动恢复，dead 必须验证真实可用。
- **复探间隔（dead re-probe interval）**：`node_cooldown_dead_minutes`（默认 1 分钟）重定义后为「dead 节点每分钟复探间隔」——健康循环每分钟探测 dead 节点，成功即恢复；调度变体只探测 available（跳过 exhausted），面板手动「测速」才全量探测（含 exhausted，但只刷延迟、不解除 exhausted 标记）。历史称呼「故障冷却（分钟）」已废弃，含义不同勿混用。
- **配额切换预算（quota switch budget）**：`max_quota_node_switches`（默认 5）= 单个请求可切换配额耗尽节点的次数上限；重试循环上限 = 上游重试上限（3）＋ 配额预算（5）= 8，两条闸门独立封顶不互挤占——普通重试（401/429/5xx）仍按重试闸门封顶 3 次，配额切换直到预算用尽为止。
- **配额熔断（quota halt）**：节点池无可用节点（出口回退直连）且直连出口被上游 429 限流时进入的进程级熔断（`quota_halt.go`）。触发即在同一次请求内跳出重试循环、把上游 429 原始 body 原样返回下游，并置全局标记；熔断期间**免费层请求在 `callOpenCodeAPI`/`callOpenCodeAPIStream` 入口直接返回 429**（重放保存的错误 body，零上游调用、零节点切换）。恢复是**惰性 + 事件驱动**：每个请求检查 `nodePool.hasEligibleNode()`，一旦出现 `available` 节点（含配额冷却到期被 `sweepExpiredLocked` 翻回的）立即解除，无定时器、不持久化、不跨重启。仅免费层（`TierFree`，即 `路由: public`）受影响；付费层的 `insufficient_quota`/`credits_error` 是账号计费问题，与出口 IP 无关，不参与熔断。注意与「配额切换预算」区分：预算管单请求内切几个节点，熔断管「所有节点都没了且直连也被限流」时整段停止转发。
- **上游客户端形态（upstream client shape）**：OpenCode Zen 免费层的鉴权依据——上游按「请求像不像官方 opencode 客户端」放行，而非按账号或出口 IP。截至 2026-10-15 实测条件：**推理路径必须是 `/inference/v1/`**（旧 `/zen/v1/` 已只放行完全真客户端）、`x-opencode-session` 的官方 ID 形态且 12 位 hex 前缀必须是 **descending 编码**、`User-Agent` 的 `opencode/<semver>` 形态且版本在**区间内**（>= 1.18.0 且不高于上游闸门认到的版本）、上游 `stream:true`、上游 `tools` 含官方工具名且每个工具的 `parameters.properties` 非空。**失配错误要分清**：形态/路径/版本过高返回 `FreeTierError`（403）；版本过低返回 `UpgradeRequired`（**426**）；模型下线返回 `ModelDeprecated`（410）；出口 IP 被限流返回 `FreeUsageLimitError`（429）。换节点/换 key 对前几类无效。完整矩阵与重新定位流程见 `docs/UPSTREAM-COMPAT.md`，决策见 `docs/adr/0002-zen-free-tier-client-fingerprint.md`。
- **标识符方向（identifier direction）**：官方 ID 的时间分量有 ascending（`bytes(ms<<12+counter)`）与 descending（`bytes(~(ms<<12+counter))`）两种编码，**官方按用途分方向且上游按方向校验**：session 用 descending（`newSessionID()`），message/request 用 ascending（`newOpenCodeID()`）。形态（长度与字符集）相同、只有方向不同，因此正则匹配查不出来——单测必须做位级比对。另注意时间分量是 53 位截断进 48 位，直接解码会回绕成 1970 年代，不能把前缀还原成当前毫秒。counter 实测有上界（>= 64 被拒），`maxSafeCounter` 保守取 15，越界时借用下一毫秒。
- **UA 版本区间与闸门滞后（OC version window & gate lag）**：上游对 UA 版本是**双向**校验——`minFreeTierOCVersion`（1.18.0）是下限，低于则 426 `UpgradeRequired`；**高于上游闸门认到的版本则 403 `FreeTierError`**，而闸门通常**滞后 npm latest 数天**（2026-10-15 实测 npm latest=1.19.0 被 403、1.18.33 通过）。因此默认必须用**已验证**的 `pinnedOCVersion`（1.18.33）而不是 npm latest；`oc_version.go` 在收到 426 时向 npm latest 升级，在收到 403 `FreeTierError` 且当前版本比 pinned 新时回退到 pinned 重试一次（`revertToPinnedOCVersion`）。历史事故：兜底常量曾低于下限（`1.15.3`）导致抖动即全量 426；随后又因直用 npm latest 导致发布新版即全量 403。
- **必需官方工具（required official tools）**：上游 `tools` 中必须出现的官方工具。2026-10-15 起除名字外还要求每个工具的 `parameters.properties` **非空**（空 properties 判为无效工具；改名或换自定义名亦 403；同大小写的重名会返回 400）。本项目直接注入全部 11 个官方工具名（最小但非空的 schema，约 1.6KB），客户端自带工具时只补缺失名字，并在响应侧丢弃空壳工具的调用。
- **工具名回映射（tool name remap）**：为过闸门把客户端工具名改写为官方小写名（沿用客户端自己的 schema），响应侧再按 `上游名 -> 客户端原名` 还原（如客户端 `Bash` → 上游 `bash` → 响应 `Bash`），使下游 agent 的工具名/schema/`tool_choice` 契约完全不变。流式由 `toolNameRewriter` 包装上游 SSE 统一收口。
- **非流式本地聚合（local SSE aggregation）**：上游免费层要求 `stream:true`，因此非流式客户端的上游请求也是流式，由 `aggregateUpstreamSSE` 把 SSE 聚合回单个 `chat.completion`（内容/推理/工具调用参数分片/`finish_reason`/`usage` 全部还原），三种协议的非流式调用点无需感知。

## 部署注意事项

- **时区**：趋势图与调用日志按进程本地时区归桶/记录（`time.Local`）。Docker 镜像默认 `TZ=Asia/Shanghai`，容器部署可 `-e TZ` 覆盖；若进程时区为 UTC 而用户在东八区，图与日志会差 8 小时。历史 UTC 记录无需迁移，归桶时自动换算。
---

## 统计层的实测事实（2026-10-13 七轮审计得出）

- **吞吐 tps 的口径**：与 DSH 同口径 = `Σ完成token ÷ Σ生成秒`，时间窗 = 首 token → 结束（不含首字等待）。写入侧 `genSec := time.Since(cr.firstTokenAt)` 一直如此，**只有聚合方式需要改**（原先是每条请求速率的算术平均，被 0.8% 的超短请求拉高 4.86×）。旧字段 `avg_output_speed` 保留，语义未变。
- **非流式请求也有速度**：`agent_shape.go:165` 强制上游恒 `stream=true`，首 token 时刻一直存在，只是被 `aggregateUpstreamSSE`（收已读完整的 body 再 `strings.Split`）吞掉。改为逐行扫到第一个 `data: ` 时调同一个 `callLogFirstToken()`，之后整段 `io.ReadAll`。8 种响应形态逐字节等价；分配 38 vs 原 33，耗时 +3%，占请求耗时 0.0001%。★ 这是必须的：**87% 的真实请求是非流式**（记忆插件的本地数据库），不含它们则速度只覆盖 4.7%。
- **数据源分工**：`opencode.ai/v2/docs/console/models/`（官方）是免费名单与废弃日期的出处——旧的 `docs.opencode.ai/docs/zen/` 已 404 且那张服务端渲染的定价表在新站已不存在。`models.dev/api.json` 是上下文/输出上限/模态的出处（OpenCode 官方文档明写"The built-in provider and model names can be found on Models.dev"），项目本来就在拉它，只是结构体丢了 `cost` 与 `limit.input`。
- **`limit.input` 覆盖率低**：models.dev 全库 28 个带该键的模型中，`input < context` 的 26 个、`input == context` 的 2 个。面板只在 `0 < input < context` 时显示"输入上限"，否则不显示——覆盖率决定要不要加列，不是显示难度。
- **守卫在真实数据上零触发**：18.6 万条真实记录里负数 0、零值 0、四条吞吐防护拒绝 0 条、int64 回绕差真实数据 11 个数量级。★ 评估防护值不值先看真实分布，不要用人造极端值。
- **写饱和加法而非跳过**：累加溢出应饱和（停在上限），不可跳过——跳过会把"显眼的缺席"换成"貌似合理的错数"（`calllog.go:1844` 的无条件累加正是让全局层暴露守卫得以触发的前提）。

## 方法论（踩过的坑，勿重犯）

- **静默失败只有两个根因**：失败被渲染成一个值、没有截止时间。看到"N 个问题"先找这 2 个，别当成 N 件事做。
- **单测全绿 ≠ 测到了真路径**。出现过两次：单测 22/22 + 变异全杀，二进制里 token 仍明文；加第 14 个字段，自家测试全绿但数据真丢。必须有"跑真二进制 + curl 实际响应"的端到端断言。
- **起服务测一遍必须校验属主**：端口被上一轮/外来服务占着时，start 拒绝启动但 curl 打到的是旧进程，20+ 条"PASS"全是空跑。今天踩了三次。校验 PID + `/proc/<pid>/exe` + 响应形状。
- **自己写的探针会量错对象**：量了纯函数（必然相等）而非真实读数；量的是"回传真值"而非"回传掩码"。写探针的人不怀疑自己的探针，需要独立验收。
- **变异体必须由【上一轮真实缺陷】派生**，不由"我实现了什么"派生——7 个无牙齿变异体里 0 个覆盖真实危险方向。
- **没抓到 ≠ 通过**，只能记"未知"。收敛判据是"明确列出查过哪些地方、都没问题"，不是"这一轮没报新问题"。
- **正则解析 HTML 很脆**：文档站改版会静默失效（404 或表格不再服务端渲染）。能用 JSON API 就别抓 HTML；但也要注意 JSON 侧同样有滞后（models.dev 网页称 122 个模型，api.json 只有 120）。

## 待办（已知未修）

- `FreeTierError`（403）透传给下游时会被客户端一律读成"API key 无效"。代码已正确处理它两处（`rewrite_error.go:31` 专门透传不误报成模型下线；`main.go:1066` 专门排除不当配额信号换节点），根因（`x-opencode-session` 编码方向）已修。剩下的只是错误码语义误读，等它真再出现时处理。
- `jev-1.13-free` 不在任何数据源里，第三方称其有 64K "状态配额"（语义与上下文窗口不同）。面板按"未知不显示"处理——把推测显示成"无限制"比不显示更危险。
- 轮转检测只认 `size < from`，无 inode/dev/mtime 校验：外部 `logrotate` 后统计历史被静默清空，而 `ΣN == absorbed` 仍成立、守恒显示绿、日志零行。logrotate 是 Linux 默认动作，必然发生。
