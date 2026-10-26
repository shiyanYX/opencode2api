# Task 2 独立评审：403 风暴遏制 + 被迫换节点选得对

- 评审对象：commit `3ab39e9`（`852fc9c..3ab39e9`，main.go +84 / proxy_node.go +370 / 报告 +126）
- 评审方式：独立回放 2026-10-24 生产日志（自写 `replay.py`，不复用实现方脚本）+ 文件级变异 + 代码/锁序核对。测试文件按公约不入库，直接读磁盘 `proxy_node_quarantine_test.go`。
- 证据目录：`.research/scoring2/t2_review/`（`replay.py` / `replay.out` / `baseline.log` / `mutations.log` / `fullsuite.log`）。独立编译/变异在 `git archive 3ab39e9` 的 `/tmp/t2review` 副本里做；仓库代码与既有测试文件未改，仅新增本报告与 `.research/scoring2/t2_review/` 证据。工作区另有 T3 的未提交改动（`bun_egress.go`/`idle_read.go`/`main.go`/`proxy_node.go`），**不属于本评审对象**；本评审只针对 `3ab39e9` 版本（副本 md5：`proxy_node.go` b7176c46…、`main.go` 40b713bc…）。

## 结论

| 维度 | 结论 | 依据 |
| --- | --- | --- |
| Spec 符合 | **❌ 不完全符合** | ①`switch` 的 `reason`/`candidates` 未按接口新增字段，塞进 `Detail` 字符串；②任务书自身矛盾：§2.1 规定只有 confirmed 才喂订阅熔断，§5/裁定又要求它对 10-24 单订阅风暴兜底——两者不可同时成立 |
| Quality | **不通过** | 1 个 Critical（订阅熔断在唯一实测风暴中 **0 触发 / 0% 覆盖**）+ 3 个 Important（gate 顺序、日志接口、硬约束 3 无有效测试） |
| 能否进入 T4 | **暂不能** | Critical 必须先修（改选路排除同订阅或改 raw 判据）；Important 可并行，但 T4 验收前应关闭 |

## Findings

### [Critical] 订阅熔断在 10-24 风暴中 0 触发、覆盖率 0.00%；报告 96.4% 不是实现口径

- 位置：`proxy_node.go:646`（`confirmSub403`）→ `main.go:3047`、`main.go:3492`（只在「本请求最终成功且换到不同节点」时调用）
- 问题：实现按任务书要求「confirmed-only」。但在 10-24 的 **4,405 次 FreeTier 403** 中，只有 **1 次**满足确认条件（0.02%）→ 10 分钟 3 次的门槛永远达不到 → 熔断 **0 触发**，对 4,405 次 403 的覆盖为 **0.00%**。也就是说 10-24 这类风暴，T2 的订阅层完全没有生效。
- 证据：`replay.out`（独立回放）：
  - 403 全量 4,405 = terminal `http 403` 2,183 + `freetier:probe_alt_node`（换节点 403）2,222；全部 `红杏`，4 节点（日本02 2,020 / 香港01 2,003 / 日本01 317 / 新加坡01 65）
  - confirmed 事件 **1**；`impl_confirmed_triggers=0`、`impl_confirmed_coverage=0/4405=0.00%`
  - 报告 §5 的 96.4% 可由「terminal-only + 600s greedy」**精确复现**（2,105/2,183=96.4%），但它既不是 confirmed-only + 3 次门槛，也漏算了 2,222 次换节点 403；全量 greedy 为 98.2%
  - 参照：若按 raw 403 三次门槛，覆盖率 **4,393/4,405=99.7%**（1,467 次触发），升级退避后 99.9%
- 影响：任务书 §5 的裁定前提「单订阅风暴由订阅熔断兜底」不成立；10-24 的遏制实际只靠既有的「每请求 1 次换节点预算」，T2 没有新增任何约束。且 91.2%（1,967/2,156）的 403 后下一次尝试仍落在同一订阅（4 个红杏节点互为候选），换节点预算基本浪费在兄弟节点上。
- 建议（二选一，需 Lead/产品定）：
  1. **首选（不改确认语义）**：被迫换节点时**同时排除同订阅**（有其它订阅候选才排除，无候选退化原行为，仿 /24 规则）。这样 403→换到异订阅成功→confirm 红杏→3 次后熔断生效；代价是换节点候选更少，需实测池容量。
  2. **备选（改计数口径）**：订阅熔断改记 **raw 403**，但加「单订阅主导」守卫（10min ≥3 次 且 该订阅占窗口 403 事件 ≥80%，或窗口内仅 1 个订阅），避免形态级风暴把每个订阅都禁掉；节点 quality 的 mark 仍保持 confirmed-only（Review Focus 3 不受影响）。回放数据支持 99.7%，但需构造形态风暴反例验证不误伤。
- 验收意义：不修此项，Task 2 的核心目标「把 403 风暴关在笼子里」在唯一实测样本上未达成。

### [Important] `confirmSub403` 在 halt / gate 抑制之前执行，抑制期仍会累积订阅 strike

- 位置：`main.go:3047-3048`、`main.go:3492-3493`（先 `confirmSub403`，再判断 halt；再进 `freeTierMarkAllowed` 的 `shape_latch` / `pool_floor` 抑制）
- 问题：halt 期间（硬约束 3）虽然不 mark 节点，但订阅 strike 照记；`shape_latch`（T3 形状闩）和 `pool_floor`（池底保护）抑制节点 mark 时，同样照记。池底保护的本意是「池子快空了别再 mark」，但订阅隔离会进一步缩小可选集合，可能把保护变成 503。
- 证据：代码顺序 + 回放语义（10-24 有 1 次 confirm 会记 strike，虽未达阈值）；`freeTierMarkAllowed` 的抑制原因见 `proxy_node.go:914-926`。
- 建议：把 `confirmSub403` 移到同样的守卫内（halt、`shape_latch`、`pool_floor` 全部抑制），或抽出一个 `confirmOutcome()` 同时决定「喂熔断 / mark / 抑制原因」，保证两条路径同生共死。

### [Important] `switch` 埋点未按接口新增字段，`reason`/`candidates` 挤进 `Detail` 字符串

- 位置：`main.go:2999/3077/3122/3217`（非流式）、`3457/3522/3567/3658`（流式）；`calllog.go:31` 的 `CallEvent` 只有 `Type/Node/NodeName/Detail/At`
- 问题：Task 2 Interfaces 明确要求「`switch` 事件新增 `reason` 字段（4 值）+ `candidates int`」。实现为 `detail="... reason=switch_after_fail candidates=12"`，下游（面板/审计/回放）无法结构化解析，只能正则；`reason` 与 `candidates` 也没有 schema 保证。
- 证据：`git diff 852fc9c..3ab39e9` 未见 `CallEvent`/`callLogEvent` 签名变更；报告 §1 自述「为不改 calllog.go 编码进 Detail」。
- 建议：给 `CallEvent` 增加 `Reason string` / `Candidates int`（omitempty），`callLogEvent` 加可选参数或新函数；或由 Lead 明确接受 Detail 编码并更新计划接口（当前属未申报的接口偏离）。

### [Important] 硬约束 3「halt 期间不 mark」没有有效自动化覆盖

- 位置：`proxy_node_quarantine_test.go:96-125`；实现侧 `main.go:3048`、`3493`
- 问题：现有断言的快照点在「触发 halt 的 10 次 note403 之后」，因此「触发那一刻 mark」与「main 确认路径 mark」两种变异都不红：
  - M5（在 `maybeHaltPoolLocked` 触发时把窗口内 403 节点标 dead）→ **GREEN 存活**
  - M6（删掉 main.go 成功确认路径的 `poolHalted` 抑制，halt 期间照 mark）→ **GREEN 存活**
  - 只有 M5b（在 `pick(true)` halt 分支里 mark 一个节点）→ RED
- 证据：`mutations.log` 6 条记录。
- 建议：① 把 halt 前快照提前，或在 `note403` 前拍快照再比较；② 把 main 的确认决策抽成可单测函数（输入 pending/halt/gate，输出 mark/suppress），补「halt+成功确认→不 mark、不喂熔断」用例；否则该硬约束只靠 code review。

### [Important] 池级 halt 判据对「中等规模形态风暴」不灵敏

- 位置：`proxy_node.go:254`（`pool403Min=10`）、`713-716`（`max(10, ceil(attempted/2))`）
- 问题：分母是窗口内**不同尝试节点数**，10-24 实测窗口 attempts 最高 89 ⇒ 阈值 45，而 403 只有 4 个不同节点；硬下限 10 在 attempts>20 后基本被 ratio 覆盖。形态级风暴若命中池子的 20–40%（例如 89 个尝试节点中 20–35 个不同 IP 被拒），既达不到 45，也未必达到 10（小窗口时）→ 不触发。
- 证据：`replay.out`：`halt_max_distinct403_window=4`、`halt_max_attempts_window=89`、`halt_triggers_10=0`（floor 8/6 也为 0）；84 个 10min 桶、单桶最多 148 次 403、中位 45 次。
- 建议值：`threshold = max(8, min(24, ceil(0.35 × attempted)))`。效果：10-24（4 个不同节点）仍为 0 触发（符合既定裁定）；89 个尝试节点时阈值 24（而非 45）；20 个尝试、8 个不同 403 节点即触发。前提是 Critical 项先修——否则「0 触发」在 10-24 上只是碰巧正确。

### [Minor] 退避 escalations 永不重置

- 位置：`proxy_node.go:619-621`
- 问题：`escalations` 只增不减；某订阅上周触发过一次 10min，本周再触发就直接 20min（再触发 30min），与任务书「连续两轮触发 10→20」的「连续」不符，最长会多关 20min。
- 建议：静默期（如 ≥1h 无 confirmed 403）清零 `escalations`（保留 `until`）。

### [Minor] `lastSwitchCandidates` 全局单值，高并发下埋点会失真

- 位置：`proxy_node.go:244`、`739-742`；读取点 `main.go:2921`、`3387`
- 问题：每次 `pick` 开头把 `lastCandidates` 置 0，读点在 `egress.Get` 之后；并发时可能读到别的请求的候选数或被清 0。10-24 实测：**18.8%** 的 403 事件发生时 ≥2 个请求在飞（峰值 13 并发），日志里的 `candidates` 有约两成不可信。
- 影响：只影响 switch 事件埋点（/24 排除效果、halt 候选证据），不影响选路/熔断。
- 建议：让 `pickWithMeta` 的候选数随 `egress` 结果返回给本请求（或放 ctx），`lastSwitchCandidates` 删除；短期可接受，但不要用该字段做「排除是否生效」的审计结论。

### [Minor] 订阅隔离不反映到 `/api/nodes` 的 `usable`，面板「可用但选不到」

- 位置：`node_scoring.go:328`（`usable = state != dead && state != exhausted`）；隔离只改 `eligible`（`proxy_node.go:379-385`）
- 裁定：**算缺陷，但不阻塞 T4**。隔离期 ≤30min、整订阅范围，面板却显示全部节点可用，值班会误判「池子健康但无流量」。不建议把隔离写进 `usable`（会把「路由资格」和「节点健康」混在一起，且 usable 语义被别处消费）；建议在 nodeScoreView 增加只读 `quarantine_until` / `routing_eligible`（按订阅 join），T4 验收前补上即可。

### [Minor] 其它

- `proxy_node.go:604` `quarantineSub` 仅测试使用（生产走 `confirmSub403`），属死包装，建议删除或标 test-only。
- `proxy_node.go:274-291` `switchRankFor` 在 `p.mu` 写锁内逐候选取 scoring 锁并拷贝 `Fail/Trend/ProbeFailTimes`（O(N)）：未发现 ABBA（`node_scoring.go` 不回调 pool），但强制换节点会拉长全局选路锁持有时间；建议每轮只取一次快照或缓存。本次未做基准，列为待观察。

## 独立回放表（2026-10-24，`/tmp/scoring2/call_log/2026-10-24.jsonl`）

| 指标 | 实测 | 说明 |
| --- | --- | --- |
| 请求 | 2,476（ok 84 / fail 2,392） | 含 403 的请求 2,244 |
| 403 全量 | **4,405** = terminal 2,183 + switched 2,222 | 报告只数了 terminal 的 2,183 |
| 订阅分布 | 红杏 4,405（100%） | 与报告一致 |
| 不同 403 节点 | **4**：日本02 2,020 / 香港01 2,003 / 日本01 317 / 新加坡01 65 | 与报告「4 个」一致（数值不同因口径） |
| 已确认 403 | **1 / 4,405 = 0.02%** | 仅 00:32:24 一次 confirmed_bad |
| 实现口径熔断 | **0 触发，覆盖 0.00%** | 3 次 confirmed/10min 永远达不到 |
| raw 三次门槛（参照） | 覆盖 **99.7%**（4,393/4,405），1,467 次触发 | 供备选方案评估 |
| 报告 96.4% 口径 | 2,105/2,183 = 96.4%（精确复现） | 非实现口径，且漏 2,222 次 403 |
| 全量 greedy 600s | 4,326/4,405 = 98.2% | 仅作对照 |
| 403 后下一次尝试同订阅 | **1,967/2,156 = 91.2%** | 换节点预算基本命中兄弟节点 |
| 池级 halt（现行） | **0 触发**（窗口不同 403 节点最多 4、attempts 最多 89） | 理由核实：并非「只有 4 个不同 403 节点」这一条，ratio 阈值 45 同样够不到 |
| 10min 桶 | **84** 个（单桶最多 148 次，中位 45） | 与报告 84 一致 |

## 变异红绿证据（`/tmp/t2review` 副本，逐条还原后 md5 校验）

| 变异 | 结果 | 捕获/说明 |
| --- | --- | --- |
| M1 `sub403Threshold 3→1` | **RED** | `TestSubQuarantineAfterThree403`（2 次即隔离） |
| M2 退避改固定 10min | **RED** | `TestQuarantineExponentialBackoff`（第二轮应为 20m，得 10m） |
| M3 删除同 /24 排除 | **RED** | `TestSwitchSkipsSameSubnet`（选了 10.3.0.2 而非 10.3.1.1） |
| M4 `pick(false)` 改轮询 | **RED** | `TestStickyUnchanged`（返回 a 而非粘滞 b） |
| M5 halt 触发时 mark 窗口内 403 节点 | **GREEN 存活** | 快照点在触发之后，抓不到 → 测试缺口 |
| M5b halt 期间 `pick` 时 mark 一个节点 | **RED** | `TestPoolHaltStopsSwitching`（available→dead） |
| M6 删 main.go halt 期间 mark 抑制 | **GREEN 存活** | main 确认路径无测试覆盖 → 测试缺口 |

基线：`go test -run 'TestSubQuarantine|…|TestSticky|TestFT'` **ok 0.319s**；`go vet ./...` 无输出；`gofmt -l` 空；`go test -skip TestIP4RV -count=1 ./...` **ok 10.321s（exit 0）**。测试文件 10 个用例均真实走逻辑，不是常量断言。

代码核对（均通过）：`pick(false)` 粘滞分支原样保留（`proxy_node.go:476-495`）；confirm-then-mark 仅在成功侧触发（`main.go:3042-3065`）；halt 用 raw 403（`main.go:3073`/`3518`），不依赖确认；无加权随机/定时重评（diff 无 `rand`/`Ticker`/weight 新增）；`switchCandidatesLocked` 的排序键与退化逻辑符合接口。

## 4 个疑点裁定

1. **`lastSwitchCandidates` 全局单值**：失真到「约 19% 的 switch 事件候选数不可信」（并发 ≥2 时；被清 0 或串到别的请求），但**不影响选路/熔断**。对 T2 可接受，对「证明 /24 排除有效」的审计不可接受。建议随 `pick` 返回值传递，改动很小。
2. **隔离未反映到 `/api/nodes.usable`**：**是缺陷，但可延后**。不要改 `usable`（会混淆健康与路由资格），加只读 `quarantine_until`/`routing_eligible`；T4 验收前补齐。
3. **halt 10 节点下限 vs 形态级风暴**：现行 `max(10, 50% attempts)` **不够灵敏**（10-24 的 89 attempts ⇒ 45 distinct 才触发；中等形态风暴 20–40% 会漏）。建议 `max(8, min(24, ceil(0.35×attempted)))`；10-24 仍 0 触发，符合你的裁定。**但该裁定的正确性依赖 Critical 项已修**——当前它是「两个网都漏」而非「订阅网兜住」。
4. **403 计数用 confirmed 还是 raw**：halt 用 **raw**（`note403` 在每个 FreeTier 403、确认之前调用），所以 **不存在「confirm 不了→halt 永不触发」的死锁**——这一点核实通过。但订阅熔断用 confirmed，已经实际掉进同一个陷阱：10-24 确认率 0.02% ⇒ 0 触发。若 halt 也改成 confirmed 才会死锁；现状是「halt 安全、订阅层饿死」，修复见 Critical。

## 无法验证项

- 无 T2 上线后的真实 `call_log`：`reason/candidates` 在实际日志/面板中的可读性、真实并发下 halt 表现未验证（只有代码与合成回放）。
- 无「形态级风暴」样本：halt 的真阳性/误伤无法用生产数据验证，建议构造合成场景或灰度观察。
- 未做并发/锁竞争压测与 `switchRankFor` 基准；锁序仅静态核对（未发现 `node_scoring → proxy_node` 反向加锁路径）。
- 4,405 次 403 是否全部为 `isFreeTierErrorBody` 只能由日志推断（日志不含 body）；非 FreeTier 403 不计入 halt 属预期。
- 全量测试跑了 `-skip TestIP4RV`（该 drift 在基线即失败，与本任务无关）；未跑不跳过版本。
- 未渲染 `/api/nodes` 面板核对「可用但选不到」的实际观感（按代码判定）。

## 复审（fix round 1）

- 评审对象：当前工作区（HEAD=`3ab39e9` + 未提交 R1–R5）。T2 相关源码在评审时：`proxy_node.go` md5 `0f37b86a…`、`main.go` `72256297…`、`calllog.go` `739917ee…`（与实现者 fix round 1 一致；本复审未改源码、未 commit）。
- 范围：只裁 R1–R5 是否 ADDRESSED 及是否引入新问题；T3 超时/T6 429 只看是否破坏 T2 语义，不审其本身。所有场景测试/变异都在 `/tmp/r1review`（当前工作区副本）跑，仓库代码未动。

### 逐条裁定

| 项 | 裁定 | 证据 |
|---|---|---|
| R1 同订阅排除 | **ADDRESSED** | 独立 `TestReviewR1_SameSubExclusion` PASS：A 有兄弟 + B ⇒ 选 B；仅 A ⇒ 选 A 兄弟；跨订阅与 /24 冲突时按「先跨订阅、再 /24、空则退化」；变异 MR1（删跨订阅过滤）→ 实现者 `TestSwitchSkipsSameSubscription` 与我的场景测试均 RED |
| R2 抑制顺序 | **ADDRESSED** | 独立 `TestReviewR2_HaltNoStrikeInternal` PASS：halt 生效时成功请求后直接读内部状态 `len(sub403)==0`、`n-A` 状态/时间戳不变；既有 `TestFT_HaltDuringSuccess_NoMarkNoStrike` PASS；M6（删 halt 守卫）/M8（confirm 提前）均 RED |
| R3 halt 判据 | **ADDRESSED** | 代码 `(attempted*35+99)/100` + floor 8 + cap 24；独立 `TestReviewR3_Coefficient` PASS：attempted=10→8、30→11、89→24、100→24；10-24 回放 max_distinct403=4 / max_attempts=89 ⇒ threshold=24、0 触发 |
| R4 switch 字段 | **ADDRESSED**（字段）/ 值语义有残留 | 真实 403→换节点成功后的 CallRecord JSON：`"switch_reason":"switch_after_fail"`、`"switch_candidates":3` 均为**顶层字段**，`detail` 为 `"freetier:probe_alt_node"`，无 `reason=`/`candidates=`；18 处调用点全改；未发现针对 `reason=`/`candidates=` 的旧解析（`switch` 事件在 scoring 归因中被直接跳过，静态页不解析 Detail）；MR4（字段不落值）RED |
| R5 静默重置 | **ADDRESSED** | 独立 `TestReviewR5_SilenceReset` PASS：10→20→30m 后，31m 静默再触发回到 10m；M9（删重置）→ 既有 `TestQuarantineExponentialBackoff` 与我的测试均 RED |
| 报告诚实性 | **ADDRESSED** | §5.2 明确三行：旧实现/严格观测链均 **confirmed=1、触发=0、avoided=0**；97.28% 行标注「R1 反事实上界」并有「不是生产观测值」说明；96.4% 已降级为旧口径 |
| T3/T6 干扰 | **未发现破坏 T2 语义** | `noteAttemptTransportError` 新增 `if nodeFp != "" && !pool.poolHalted(...)`（更强 halt 不 mark）；告警喂入改为 `err == nil \|\| isFirstByteTimeout(err)` 只影响告警极性；`poolHaltUntil`/`sub403`/`callLogSwitchEvent` 均在，403/halt/confirm 顺序未变；截至评审时 T6 的 429 改动不在这条 403 路径上（若后续改动 403/halt 顺序需复核） |
| Wiring 前缀匹配 | **未失去意义，但略弱** | `zz_c5_verify_wiring_test.go:464` 仍要求 `nodeAlerts.recordNodeAttempt(nodeFp != "",` 恰好 2 次 + 仅 2 个调用点；`zz_c5v2_verify_wiring_test.go:147`、`zz_node_alert_test.go:453` 仍钉住完整新表达式；W01/W02 行为测试 PASS。残留：W09 单独不再约束第二实参 |

### 变异证据（`/tmp/r1review2`，每条恢复后 md5 校验）

| 变异 | 结果 | 捕获 |
|---|---|---|
| MR1 删除跨订阅排除 | **RED** | `TestSwitchSkipsSameSubscription`、`TestReviewR1_SameSubExclusion` |
| M6 删除 `freeTierConfirmAllowed` 的 halt 守卫 | **RED** | `TestSuppressedConfirmDoesNotStrike`、`TestFT_HaltDuringSuccess_NoMarkNoStrike` |
| M8 `confirmSub403` 提前到抑制判定之前 | **RED** | `TestFT_HaltDuringSuccess_NoMarkNoStrike` |
| M9 删除 30m 静默重置 | **RED** | `TestQuarantineExponentialBackoff`、`TestReviewR5_SilenceReset` |
| Mc3 系数 0.35→0.50 | **RED（仅我的测试）** | `TestReviewR3_Coefficient`；实现者 `TestPoolHaltThresholdClamp` 存活 → 系数未被其测试钉住 |
| MR4 switch 字段不落值 | **RED** | `TestSwitchEventStructuredFields`、`TestReviewR4_RealSwitchJSON` |

### 构建/全量状态

- `go build ./...`：OK；`go vet ./...`：无输出（BUILD_VET_OK）。
- `go test -skip 'TestIP4RV' -count=1 ./...`：**ok，244.633s，exit=0**（工作区副本；先补拷 `free_models_cache.json`；未补拷时仅 T23 两个测试因该文件缺失失败，属环境缺件，不是代码回归）。
- 基线定向套件（13 个 T2 测试 + 12 个 FT 测试 + W01/W02/W09 + 新增复审测试）：全绿。

### 新问题（均不阻塞 T2）

1. **[Minor] R4 `switch_candidates` 的值仍是「上一次 pick」的近似**：真实 3 节点场景 JSON 写出 `3`，而本次被迫换节点的实际候选是 2（含被 403 的自身/非换节点计数）；并发下仍可能被其它请求覆盖（原 Q1）。字段结构化已修，值语义未修。建议让 `pickWithMeta` 把候选数随 egress 结果返回，或强制换节点后再采样。
2. **[Minor] `switch_candidates` 带 `omitempty`**：halt/全订阅隔离时传 0，字段会从 JSON 消失，审计无法区分「0 候选」与老记录。若需要显式 0，去掉 omitempty。
3. **[Minor] R3 测试卫生 + 系数覆盖**：`TestPoolHaltThresholdClamp` 用 `string(rune('a'+i))` 生成地址，非法 octet 触发 `net.LookupIP` DNS，单测约 20s；且 0.35→0.50 对其存活。建议改数字地址并加 attempted=30/阈值 11 的用例。
4. **[Minor] R2 的 check-then-mark 仍有 TOCTOU**：`freeTierConfirmAllowed` 与 `markFreeTierRejected` 分开取锁，halt 可在两者之间生效；这是既有竞态，R2 未新增但也未关闭。另：`markFreeTierRejected` 返回 false 时不再计订阅 strike（与「确认与 mark 同生共死」一致，但节点已被其它路径摘除时会少记一次）。
5. **[Minor] Detail 前缀变化**：首字节超时的 switch 事件 Detail 从 `connect_error:` 变 `ttft_timeout:`（T3 引起）；仓内无解析者，外部消费者需知会。

### T4 结论

**R1–R5 全部 ADDRESSED，无新增 Critical/Important，T2 可以进入 T4**。但有两个验收前提：① 原 Critical 的「实测覆盖」仍未被生产数据证明——严格口径仍是 confirmed=1/触发=0/avoided=0，R1 的效果目前只有反事实上界（97.28% 不得当作实测验收值），T4 需把上线后的 confirmed/触发计数列为观察项；② R4 候选数值语义、R3 测试卫生/系数覆盖作为 T4 期间的 Minor 收尾。
