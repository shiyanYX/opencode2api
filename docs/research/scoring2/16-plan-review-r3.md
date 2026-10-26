# R3 对抗校对 · 节点系统 v3 DRAFT（D1–D8）

> 输入：`docs/superpowers/plans/2026-10-27-node-system-v3.md`（DRAFT）、`11-shape-drift-design.md`（C10）、`12-shape-drift-review.md`（G1）、`13-node-system-design.md`（统一设计 v1）。
> 只读；不改代码、不 commit。所有 D1–D8 结论都由独立重算/仿真支撑；Lead 给的数字在文中标注“复算一致/不一致”。
> 分节落盘。校对时间：2026-10-27 00:52–01:05 +08。

## 0. 总评

**结论：DRAFT 不能直接进入 R4；必须先修 3 条，再进 R4。** 这不是“方向不对”，而是当前的全局 `probe_only`、TTL 语义和 halt 优先级仍会重演 10-26 闭环中的两种致命路径：**跨 model 误闩** 和 **TTL 清空 blocked 后周期性烧同一个 IP**。

DRAFT 的 C10×G1 合并方向是对的：发送前闸门、blocked 只由证据清除、单飞试探、抑制留痕，都是必须保留的。问题在于 DRAFT 把 **“全局闸”当成了默认的第一层**，而生产日志显示 10-26 的 429 主要是 **per-(model,egress) 配额**，不是账号级 shape；同时 D3 仍保留了“TTL 退出”的歧义，D8 仍是三套 halt 各自为政。按当前 DRAFT 直接实现，最可能的线上结果是：

1. 18:10 那种 `mimo 40 ok / 411 429` 的窗口被全局 `probe_only` 压掉 40 个本可成功的请求；
2. 若 TTL 被实现成“清空 blocked/seen”，5 小时风暴下同一个出口会被命中 **30 次**，超过 10–16 的封禁阈值；
3. `quota_halt` 仍先于 `shape_drift` 裁决，且 `hasEligibleNode()` 会把 probation 节点算作可用，导致 503 和试探令牌的语义互相打架，回放仍无法解释“为什么这条请求没发”。

因此：**R3 = NO-GO；D1/D2/D3/D5/D6/D8 必须先修，D4/D7 可随 R4 修但要有明确默认值。**

### 0.1 必须先改的 3 条

1. **D1/D2：全局闸降级为“严格证据下的例外”，默认 per-(model,egress)。** 硬条件：过去 60 秒内任意 model 出现 2xx ⇒ 禁止全局闸；只有 `K=5` 个新出口连续 429、无任何 2xx、且 429 比例 ≥90% 时才允许全局单飞；`3 distinct/10min` 永久废弃。
2. **D3/D5：TTL 只推 `nextProbeAt`，绝不清 `blocked`；固定 5h 不是证据。** blocked 只由成功证据清除；配额恢复改为 `5m→30m→2h→5h` 的 per-pair 退避 + 真实请求 canary，到期只进 probation，不得直接 available。
3. **D8/D7：三套 halt 合并成一个纯函数裁决点，统一状态码/Retry-After/抑制字段。** 优先级固定为 `pool403 > global shape > per-model quota > direct quota > selection`；禁止 `quota_halt` 在 `shape` 之前独立 503，禁止全排除后回退直连，禁止把上游 429 原样透传。

## 1. 方法与数据

- 10-26 全量：`/tmp/cl-1026b.jsonl`，13,724 行，`15:16:58–23:59:54`；10-27 尾：`/tmp/cl-1027b.jsonl`，541 行，`00:00:08–00:23:17`。
- 10-24：`/tmp/scoring2/call_log/2026-10-24.jsonl`，2,478 行；10-22/23：同目录日文件；10-19：`/tmp/prod_call_log.jsonl` 过滤。
- 应用日志：`/tmp/t6a/app1.log.gz`（10-07–10-26 20:57）、`/tmp/t6a/app2.log`（20:57–00:24）；只读。
- “同窗口×model”口径：10 分钟对齐窗口；`status=ok` 为 2xx 证据；`err_msg` 含 429 或 event detail 含 `quota_signal`/`FreeUsageLimitError` 为 429 证据。两种 429 口径分别报告，避免把口径差异当成结论。
- D3 仿真参数：`N=212` 出口、`R=0.5 req/s`、5 小时、`K=5`、`TTL=600s`、probe 退避 60/120/300s；结果在 §4。
- 所有脚本在 `/tmp/t7_*.py`，不写仓库。

## 2. D1 状态粒度：全局单闸 vs per-(model,egress)

**裁决：都不完全接受。接受 C10 的 per-(model,egress) 作为默认层，接受 G1 的全局单飞只作为“严格证据下的例外”；第三方案如下。**

### 2.1 判断与理由

G1 的“形态是账号/IP 级，所以全局”只在 **账号级 shape** 成立；但生产日志里 429 的主体是 **model 配额**。同一 10 分钟窗口内，同一个 model 既有 2xx 又有 429，说明“该 model 全池耗尽”不成立，更不能推出“账号/IP 被 shape 拒绝”。此时全局闸会把该 model 的正常成功请求也压掉，并把失败原因错误地记到全池。

反过来，纯 per-model 也不够：`M` 个 model 各自单飞会放大探测；且当 **所有 model 都无 2xx** 时，需要有一个全局止血闸。正确做法不是二选一，而是 **两层**：

- **第一层（默认）**：`(model, egress)` 级排除/短退避；一个 model 的 429 不影响另一个 model。
- **第二层（例外）**：只有满足 §2.3 的严格全局条件时，才允许**全局单飞 probe_only**；全局闸不替代 per-model 排除，二者同时存在。

### 2.2 独立复算（Lead 的 D1 证据）

我用 10-26 + 10-27 全量重算，按 **window×model**：

| 口径 | 组合数 | 同 model 同窗口既有 2xx 又有 429 | 说明 |
|---|---:|---:|---|
| 仅 `err_msg` 判 429 | 113 | **11（9.7%）** | 与 Lead 的“11 个”一致；Lead 的 105 可能来自 10-26 的某个中间 cutoff 口径（我截到 23:15 为 99 个，23:00 为 95 个） |
| 结构化 event detail 判 429 | 113 | **16（14.2%）** | 事件口径更宽；11 是保守下界 |
| 10-26 单日、`err_msg` | 107 | 9 | 只到 23:59；说明“105”不是完整 10-26 |

代表性窗口（`req`/`ok`/`429` 为请求数）：

- `18:10 mimo`：453 / **40 ok** / 411 429；
- `18:10 deepseek`：14 / **1 ok** / 13 429；
- `19:10 mimo`：1,349 / **7 ok** / 1,342 429；
- `19:10 deepseek`：50 / **5 ok** / 45 429；
- `19:30 mimo`：210 / **210 ok** / 29 429；
- `00:20 mimo`（10-27）：199 / **32 ok** / 167 429。

这些数字直接推翻两个假设：

1. “同一窗口有 429 就说明全池该 model 已经耗尽”不成立；
2. “有 429 就说明账号级 shape 被拒”不成立——同一 model 在同一个 10 分钟里仍有 40、210、32 个 2xx。

跨 model 的窗口更直接：按“窗口内存在 model A 的 2xx 且 model B 的 429”统计，10-26 有 10 个、10-27 有 2 个（Lead 的“2 个”是更严格的跨 model 定义；按宽松窗口我得到 11 个）。数量不是重点，重点是**至少存在一个 model 在另一 model 429 时成功**。

> 数据口径说明：Lead 的 105/10.5% 我没有复现出完全相同的 105；11 个 same-model both 在 `err_msg` 口径下复算一致。结论不依赖 10.5% 这个精确比例：即使只用 11 个，也足以否证“全局默认闸”。

### 2.3 允许全局闩的可判定条件（布尔条件 + 参数）

只有在 **全部** 满足时才允许进入全局 `probe_only`；任何一条不满足，只能走 per-(model,egress)：

```text
GLOBAL_SHAPE =
  no_2xx_any_model(last 60s)                      // 硬否决：任一 model 的任意 2xx 立即 false
  AND last_any_2xx_age >= 60s
  AND distinct_new_egress_429(last 600s) >= 5     // 新出口，按 egress IP/fp 去重
  AND consecutive_new_egress_429 >= 5             // 只有“新出口”才递增；同出口重复 429 不递增
  AND attempts(last 60s) >= 20
  AND 429_count(last 60s) / attempts(last 60s) >= 0.90
```

含义与边界：

- `no_2xx_any_model(last 60s)` 是**硬否决**。18:10 的 40 个 ok 会让 `GLOBAL_SHAPE=false`；这正是我们想要的。
- “同一 egress 被不同 model 各 429 一次”只算 **1 个 distinct egress**，不凑数。
- 只有一个 egress 反复 429（00:20 的 `US-191.96.254.138`）**永远不满足** ≥5 个新出口；它属于 per-pair 配额，不是全局 shape。D2 必须保留 per-pair 路径，不能等全局闸。
- `attempts>=20` 和 `429 ratio>=0.9` 防小样本；低流量时宁可不启全局，靠 per-pair + fail-fast。
- 全局闸进入后：`blocked` 保留；TTL **只推 `nextProbeAt`**；任一真实 2xx（任意 model）清除全局闸和对应 `(model,egress)`；没有 2xx 就只允许单飞探测，不允许普通请求。
- 全局闸必须同时有 `suppressed_by=shape_drift_probe_only`、`would_be_egress`、`candidate_count`、`blocked_n` 的结构化记录；否则无法回放。
- **绝不允许**：`e2e 2xx` 只清全局但不清 `(model,egress)`，或反过来只清 per-model 而全局仍 armed。两者必须在同一 `Decide` 事务中更新。

### 2.4 裁决错误的后果

- **严重度：Critical。** 若采用 G1 的全局默认闸：18:10 会压掉 40 个 mimo 2xx（以及 19:10 的 7 个），10-26 的“闩死”从 per-model 故障升级为全模型可用性事故；若采用纯 per-model：M 个 model 各自探测会把 IP 探测速率放大约 `M` 倍，真 shape 风暴下烧 IP。
- 这不是“可用性略降”，而是重演 10-26 的 5 小时闭环，因此 D1 必须在 R4 前定死。

## 3. D2 触发条件：3 出口/10min vs K=5 连续新出口

**裁决：接受 B（K=5 连续新出口 + 无 2xx），但只作为“全局条件”的候选；必须补 per-model/single-egress 路径。不接受 C10 的 3 distinct/10min。**

### 3.1 判断与理由

`3 distinct/10min` 在高流量 212 节点池里是**常态**，不是 shape 信号；它正是 10-22/23 旧闩误拉、10-26 18:03 旧闩拉起的直接原因。K=5 连续“新出口”且期间无 2xx 更接近“换了多个 IP 仍然全失败”的形态证据，误触发明显更低。

但 K=5 **不能单独承担全部 429 处理**：

- 10-27 00:20 是**单出口反复 429**（199 请求 / 167 429 / 32 ok，全在 `US-191.96.254.138`），K=5 的“新出口”永远不到 5，若只等全局闸，就会把流量钉死在该出口；
- 10-26 18:03 的 429 主要是 per-model 配额，K=5 不应触发全局（好），但必须有 per-pair 排除把该 model 的 429 出口拿掉，让其他出口/模型继续。

### 3.2 回放证据（10-26/10-27 全量，按事件时间）

我按“每个 429 event 是新 egress 才递增；任一 2xx 清零；last 2xx >60s”重放：

| 窗口 | 观测 | C10：3 distinct/10min | G1：K=5 连续新出口 + 无 2xx |
|---|---|---|---|
| 10-26 18:03–18:10 | `mimo` 18:03 后大量 429；18:00 窗口 164 ok，18:10 窗口 40 ok | **18:03:58 触发**（distinct 22） | **不触发**（有 2xx 清零） |
| 10-26 20:00–20:10 | 573 请求 / 5 ok / 568 429 | **20:00:09 触发** | **不触发** |
| 10-27 00:20–00:23 | 199 / 32 ok / 167 429，单出口 | **不触发**（<3 distinct） | **不触发**（<5 new egress） |
| 10-22 全天 | 旧闩 14:01 拉起并保持 9h20m | 9 次触发 | 4 次触发 |
| 10-23 全天 | 旧闩 11:54 后长期保持 | 5 次触发 | 1 次触发 |

> 注：10-24 主要是 403 池级风暴，不是本 D2 的 429 shape 样本；不混入结论。

关键结论：

1. **C10 在 18:03:58 触发时，18:00 窗口已有 164 个 2xx、18:10 窗口有 40 个 2xx**；这是典型的“把配额耗尽误判成 shape”。D1 的硬否决（任一 2xx）会直接阻止它。
2. **K=5 在 18:03/20:00 都不触发**，因为期间存在 2xx；这正是我们希望的——这两段应由 per-model 排除处理。
3. **10-27 00:20 的 167 个 429 全在单出口**；任何“新出口数”闸都不会触发。若 D2 只写 K=5 而没有 per-pair 排除，00:20 会复现“钉死单节点”。

### 3.3 第三方案与参数

```text
正常路径（默认）：
  on 429(model, egress):
    add (model,egress) to pair_blocked with strikes 5m/30m/2h/5h
    do NOT mark NodeExhausted
    force switch if current egress is this pair
    if no unblocked egress for this model:
        fail-fast for this model (429/503 per D7), not global
全局升级（仅当 D1 的 GLOBAL_SHAPE 为 true）：
  K=5 consecutive new egress 429, no 2xx any model 60s, ratio>=0.9,
  attempts>=20, distinct_new_egress>=5
  -> global probe_only; blocked persists; single-flight trial bound to egress
单出口重复 429：
  不升级全局；per-pair 排除 + 该 model fail-fast；
  若该出口在 10min 内重复 429 >=3 且无 2xx，进入 24h shape budget（D4），
  但不得因此封锁其他 model。
```

参数建议：

- `shapeDriftK=5`（不是 3）；
- `shapeDriftWindow=10min`（保留，但不再用 3 distinct）；
- `shapeDriftTTL=10min` 只作为 `nextProbeAt` 的调度上限，不清 blocked（见 D3）；
- `lastSuccessGap=60s`；
- `ratio=0.90`、`attemptsMin=20`；
- **单出口重复 429** 的 `pairRepeat=3/10min` 只触发该 pair 的硬排除，不触发全局。

### 3.4 裁决错误的后果

- **严重度：Critical。** 保留 C10 的 3 distinct：18:03 会再次把已有 164 个 2xx 的 18:00 窗口拉进全局闩，直接重演 10-26 的五小时事故；只留 K=5 而没有 per-pair：00:20 会把 167 个 429 继续钉在单出口，等价旧闩的“只进不出”。

## 4. D3 闩退出：硬 TTL 清空 vs blocked 只由证据清除

**裁决：接受 G1 的“blocked 只由证据清除”；拒绝任何“TTL 清空 blocked/seen”的实现。TTL 只能推 `nextProbeAt`。DRAFT §S1 的“退出后每小时最多重入 6 次”若配合清空 blocked，仍然会打穿；必须删除“清空”语义。**

### 4.1 判断与理由

这是本轮最容易被“看起来已经修了 TTL”掩盖的 Critical。旧实现真正烧 IP 的不是试探本身，而是 **TTL 每 10 分钟把 `seen`/`blocked` 清空后，请求重新扫全池**。G1 的“blocked 保留、TTL 只推下一次试探”把每个出口的坏形态命中限制在风暴内的 1 次；C10 的“TTL 退出 + 重入 6/h”如果在退出时清空 blocked，会变成 **30 个 10 分钟周期，每周期重新打同一个出口**。

### 4.2 5 小时风暴仿真（N=212、R=0.5/s、K=5、TTL=600s）

仿真假设：所有上游发送都返回 429（最坏情况）；单飞；退避 60→120→300s；选择器对同一出口具有粘性（这是最坏情况，不能假设“随机轮转”能救）。

| 变体 | 5h 上游发送 | 单出口最大命中 | 触及出口数 | 与封禁阈值 10–16 对比 |
|---|---:|---:|---:|---|
| G1：blocked 保留、TTL 只推 nextProbe | **66** | **1** | 66 | 安全 |
| 硬 TTL 退出、blocked 保留 | 124 | **1** | 124 | 安全 |
| 硬 TTL 退出、blocked **清空**、粘性首选 | 239 | **30** | 8 | **超过 16，NO-GO** |
| 硬 TTL 退出、blocked 清空、轮转选择 | 239 | 4 | 145 | 依赖选择器，不可作为保证 |
| 旧实现（无发送前闸门、TTL 清 seen） | 5,520（C1 旧仿真） | 30 | 212 | 2h40m 全池烧穿 |

**关键算术：**

- 5h / 10min = **30 个 TTL 周期**；`6/h` 的上限在 5h 也是 **30 次**，与 TTL 周期数相同，所以“重入上限 6/h”**不能**降低 TTL 清空带来的 30 次；
- 若每个周期清空 blocked 后粘性选择器仍先选同一个出口，该出口命中 **30 次**；30 > 10–16，**单个 IP 会被当日封禁**；
- 若 blocked 保留，每个出口在风暴中最多被试探 **1 次**；TTL 退出后即使恢复普通流量，因 blocked 仍在，也不会重新扫池；所以 G1 的 66 次发送/每出口 1 次是安全上界；
- 若使用 per-pair strikes `5m/30m/2h/5h` 且允许冷却后重试，单出口在 5h 内最多 **4 次**（5m、30m、2h、5h 的累积到 5h 边界；每个 model 4 次，2 个 model 合计 8 次），仍 <10–16；这是 D5 的退避上限，不是 TTL 清空。

### 4.3 第三方案与参数

- `probe_only` 的 TTL：**只设置 `nextProbeAt = now + TTL`**，不清 `blocked`，不清 `seen`，不重置 strikes；
- 退出 `probe_only` 的唯一条件：**任一真实 2xx**；没有 2xx 时即使 TTL 到，也只允许下一次单飞探测；
- `reentriesThisHour` 仅用于**告警/审计**，不作为清空 blocked 的许可；
- 若 5h 内没有任何 2xx：保持全局 probe_only，返回 503/429（按 D7），**不要**为了可用性清 blocked；
- 若所有候选都被 blocked：fail-fast（503），不回退直连，不再发请求；
- per-pair 冷却 5m/30m/2h/5h 只决定“该 pair 是否可再试”，不决定全局 blocked 集合是否清空。

### 4.4 裁决错误的后果

- **严重度：Critical。** 清空 blocked/seen 的实现只需 5 小时就能让一个出口命中 30 次；按 10–16 的封禁阈值，**至少一个 IP 当日被封**，且流量会继续轮转到下一个出口，形成“逐个烧”的慢速版本。DRAFT 的“重入 6/h”给人安全错觉，实际与 TTL 周期相同，必须明确禁止清空。

## 5. D4 24h 单 IP 命中预算 8

**裁决：接受 A（C10 的“有预算”思想与 `H_bad=8`）作为**安全上限**，但不接受把 8 当成“上游封禁阈值”的一手证据；预算只统计 shape-suspect 命中，不统计普通 per-model 配额 429。**

### 5.1 判断与理由

- `H_bad=8` 的价值是“即使状态机写错，也不让单 IP 超过 8 次坏形态”；8 < 10–16，留 2–8 次余量。这是防御性参数，不是从上游文档推导出的阈值。
- C10 引用的 10–16 来自代码注释/历史经验（`bun_transport.go` 的“10+ 次”、`main.go` 的“~16 次量级”），**没有一手上游证据**；因此不能把 8 说成“实测安全”，只能作为保守预算。
- 10-26 的 per-node 429 计数是 **配额**，不是 shape：14 个节点有 429，6 个 >8，最高 5,112；如果把这些都计入 `H_bad`，会在一次事故里硬排除 6 个节点，属于把“额度耗尽”误当“坏形态”。这恰好证明预算必须**分域**。

### 5.2 独立证据

10-26 全量按 node 统计 429：

| 指标 | 值 |
|---|---:|
| 有 429 的节点 | 14 |
| 429 总数 | 11,541 |
| 单节点 >8 | 6（最高 5,112） |
| 单节点 >16 | 6 |

这些数字**不能**验证封禁阈值（这些都是 `FreeUsageLimitError` 配额，不是 shape 封禁）；只能说明：若 `H_bad` 被错误地应用到所有 429，会误杀大量仍可能恢复的节点。

### 5.3 第三方案与参数

```text
ipBudget[egress] = {
  shapeHits24h,      // 只在 global shape/probe_only 或 shape_suspect 证据下 +1
  normalQuotaHits24h // 普通 per-model 429，只观测，不参与硬排除
}
H_bad_shape = 8/24h
H_bad_normal = 不设硬上限（只做配额退避）
hardExclude(egress) 仅当 shapeHits24h >= 8
alert at 4/6; page at 8
```

- 预算按 **物理 egress/主 IP** 统计，不按 model 重复计数；同一 IP 被 2 个 model 各打 4 次也算 8。
- 达到 8 后硬排除 24h，但**必须保留证据链**（时间、model、pair、trial/probe）。
- 若上游封禁阈值的一手证据始终拿不到，8 保持“保守默认”，不要下调到 4（会把可用池压得太小），也不要在灰度前宣称“安全”。

### 5.4 裁决错误的后果

- **严重度：Important（预算过低/误伤）到 Critical（无预算/真风暴烧 IP）。** 若把所有 429 计入 8：一次 10-26 配额事故会硬排除 6 个节点，容量下降；若无预算：真 shape 风暴下每个 IP 可能被 probe/TTL 反复命中，回到 D3 的烧 IP 路径。故预算必须有，但必须分域。

## 6. D5 冷却 5h 的依据

**裁决：拒绝“固定 5h 到期即恢复”；接受 C10 的递进冷却 `5m/30m/2h/5h` 作为上限，但恢复必须由真实成功证据触发。5h 只能作为“最长不早于”，不能作为“到点可用”。**

### 6.1 判断与理由

生产日志已经证明：5h 是**代码常量**，不是观测到的恢复时间。应用日志中 190 个节点的首次 `exhausted` 标记 **100% 都是 `until = +5.00h`**；539 次 auto-recovered 也都在固定 5h 边界。10-26 23:03 的“恢复”只是到期，不是上游额度恢复。固定 5h 让事故持续了完整 5 小时，且到期节点未经证据直接接流，23:00–23:06 的 137 个失败全部钉在 `DE-64.137.10.153`。

### 6.2 独立证据（并纠正 Lead 的节点数口径）

我按 `app1.log.gz` + `app2.log` 统计 `node state changed state=exhausted` 与 `auto-recovered`：

| 时刻 | Lead 的口径 | 我复算的 **mark 行数** | 我复算的 **distinct fp** | 说明 |
|---|---:|---:|---:|---|
| 18:03–18:04 | “21 个节点到期” | **21** | **2** | 20 次 `BR-45.39.2.144` + 1 次 `DE-45.151.163.29`；**不是 21 个节点** |
| 23:03 | “21 个” | **22** | **3** | 其中 21 条来自 18:03 的重复 mark |
| 00:33 | “29 个” | **29** | **1** | 29 次重复 mark 同一 fp |
| 01:00 | “9 个” | **10** | **2** | 9 条 `01:00` + 1 条 `13:01` |

- 190 个不同 fp 的首次 `exhausted`：`until - marked_at` **全部为 5.00h**；
- 18:03 的 20 次重复 mark 是同一 fp、同一 reason、同一 until；当前工作区已有 mark 幂等修复，但**生产日志里的 5h 常量没有被替换**；
- 23:03 只有 **2 个** auto-recovered（BR 和 DE），不是 21 个节点；因此“23:07 恢复=21 个节点到期”不成立，实际是**少数节点到期 + 旧闩解除**叠加。

429→成功（同 model、同 pair、不同 request）的观测：

- 10-26 共 11,541 个 429 event；只有 **245（2.1%）** 在之后拿到同 pair 的成功；
- 这 245 个全部 ≤60s（p50 5.3s、p90 18.2s），更像是短时瞬态或切换后重试，不是“配额恢复”；
- **97.9% 右删失**（之后没有同 pair 成功观测）；由于闩/冷却抑制了尝试，**无法从日志估计 5h 的真实恢复分布**。

因此：**5h 没有证据支持，且 5h 的固定恢复是事故持续的直接原因。** 不能拿 5h 当“实测恢复中位”，也不能拿“23:07 恢复”证明 5h 正确——那是到期，不是证据。

### 6.3 第三方案与参数

```text
on 429(model,egress):
  pair.state = exhausted
  pair.until = now + strikeCooldown(strikes)
  strikes = min(strikes+1, 4)
  // strikeCooldown: 5m, 30m, 2h, 5h
  // 429 does NOT mark NodeExhausted; it writes quota[model][egress]
  // NodeExhausted (if used at all) requires confirm-then-mark (403/5xx/other)
on cooldown expiry:
  pair.state = probation (not available)
  allow ONE real request as evidence
  success -> available + reset strikes
  429 -> exhausted with next strike
  other fail -> dead (probe only)
```

- 首次冷却 **5m**，不是 5h；30m/2h/5h 只在重复失败后升级；
- **证据优先**：真实请求成功才清 `quota[model][egress]`；探测成功只清 transport/dead，不清配额 probation（D6）；
- 固定 5h 最多作为**安全上限**，到期仍无证据时继续 5h 退避或 fail-fast，不自动 available；
- 面板显示 `last_ok_age`、`quota_state`、`evidence`、`strikes`、`until`；缺失显示“未提供”，不填 0。

### 6.4 裁决错误的后果

- **严重度：Critical。** 保留固定 5h：每次配额耗尽都至少损失 5 小时；到期潮会把未经证据的节点同时放回，形成 23:00–23:06 的单节点 137 连败；若把 5h 改短但没有 canary，则会在额度真耗尽时高频打同一个 IP，烧 IP。

## 7. D6 probation 证据：真实请求 vs 主动探测

**裁决：都不完全接受。配额 probation 只能用真实请求（或与真实推理同路径的最小请求）；主动探测只用于 dead/transport。当前代码 `recordProbeSuccess` 把探测成功当配额 probation 证据，是 Critical，必须删除。**

### 7.1 判断与理由

- `/zen/v1/models` 探测成功只能证明“连接/模型列表可访问”，不能证明该 `(model,egress)` 的**推理配额**可用；它返回 200 而推理返回 429 的假阳性正是本次事故的形态。
- 真实请求是唯一能证明配额的证据；成本是一次请求，但可以用**下一个真实请求**承担（零额外上游发送），或用同 model 的 `max_tokens=1` 最小请求（有明确成本）。
- 10-26 的 429 请求 p50 1.139s、p95 3.175s；成功请求 p50 10.0s、TTFT p50 3.87s。真实 canary 的等待成本可控；主动探测的延迟数据缺失，不能作为“更便宜”的证明。
- 当前工作区 `recordProbeSuccess` 的 `case NodeProbation: p.promoteProbationLocked(n)` 明确允许探测提升 quota probation；这与“真实 2xx 是唯一证据”的注释矛盾，必须去掉。

### 7.2 证据与成本

| 方案 | 成本 | 误判风险 | 可回放性 |
|---|---|---|---|
| `/models` 主动探测 | 低（连接级） | 高：200 不代表推理配额；可能把刚耗尽的节点放回 | 只能证明 transport |
| 真实请求（下一请求） | 0 额外上游发送；用户延迟照常 | 低：真实 429/2xx 即证据 | 强：与用户请求同一路径 |
| 合成 `max_tokens=1` 推理 | 1 次请求；p50 ≈1–10s | 低，但会消耗配额/增加负载 | 强，但需绑定 model/egress |
| 真实请求 + 探测 | 1 次请求 + 探测 | 低 | 强；但探测只用于 dead |

### 7.3 第三方案与参数

```text
probation evidence:
  quotaProbation: only real inference request (prefer next real request;
                  synthetic max_tokens=1 only if no traffic)
  transportDead: active probe allowed (GET /models is enough)
  on probe success: NEVER promote quotaProbation
  on real 2xx: promote; on 429: strikes++; on other fail: dead
  probationMaxConcurrent=4 (global), one evidence request per pair
```

- `ProbationTried` 在**选中真实请求时**置位；若该请求最终 2xx 才提升；若 429，按 D5 退避；若客户端取消/超时，不能算成功。
- `quota[model][egress]` 的恢复证据必须带 `evidence=real_request`；探测只写 `transport_ok`，不写 quota。
- 面板显示 `probation_evidence_type` 和 `probation_result`；缺失“未提供”。

### 7.4 裁决错误的后果

- **严重度：Critical。** 用 `/models` 探测提升配额 probation：刚耗尽的节点会被放回，下一次真实请求立刻 429，重演 18:03 闭环；只做主动探测还会把“探测成功”误记成质量成功，污染后续打分。真实请求方案的主要代价是延迟/一个请求，远小于再次 5 小时事故。

## 8. D7 全排除时 503+Retry-After vs 透传 429

**裁决：都不完全接受。按 scope 分流：全局/直连/pool403 用 503+Retry-After；仅 per-model 配额且其他 model 仍可选时用 429+Retry-After；绝不原样透传上游 429。**

### 8.1 判断与理由

- 503 是“网关当前无法为该请求选到出口”的正确语义；适合 **global shape / pool403 / direct quota**。
- 429 是“该 model 的额度/速率问题”；适合 **per-model quota 全排除**，让调用方有机会换 model，而不是把整个网关判死。
- 原样透传上游 429 的问题：没有 `Retry-After`、没有 scope、客户端会按同一 model 立刻重试，形成重试风暴；旧闩 `drift.Halt` 正是 `break` 后返回上游 429。
- 当前实现不一致：`quotaHaltResponse` 是 503 + `Retry-After: 30`；`all_egress_blocked` 返回 503 但 header=nil；`drift.Halt` 返回上游 429。必须统一。

### 8.2 证据与现状

| 路径 | 当前状态码/头 | 问题 |
|---|---|---|
| `quotaHaltResponse` | 503 + `Retry-After: 30` | 正确方向；但它先于 shape 裁决 |
| `shape_drift` all egress blocked | 503，header=nil | 客户端没有重试时刻 |
| `drift.Halt` | 上游 429（透传） | 无 scope/无 Retry-After；鼓励同 model 重试 |
| `pool_halt` | 503，header=nil | 需要 Retry-After |
| `node_cooldown` | 503/内部错误不统一 | 需归入同一 Decision |

客户端重试行为没有生产埋点，不能声称“实测 503 更好”；但 HTTP 语义与当前代码已足够支持按 scope 分流。`Retry-After: 30` 远小于真实恢复时间，只是削峰，不是承诺；文档必须写明。

### 8.3 第三方案与参数

| scope | 状态码 | Retry-After | 说明 |
|---|---:|---:|---|
| `pool403` | 503 | 30s | 池级 403 风暴；不探测，不再发上游 |
| `global_shape`（D1 条件） | 503 | 30s | 全局单飞；只有 probe token 可发 |
| `model_quota`（该 model 全排除，其他 model 可用） | **429** | `max(30, min(300, until-now))` | body 带 `model`、`scope`、`retry_after`；允许换 model |
| `direct_quota` | 503 | 30s | 直连出口熔断，不回退节点池 |
| `all_models_exhausted` | 503 | 30s | 所有 model 均无候选 |
| `node_cooldown`/无候选 | 503 | 30s | 不直连 |

- 所有响应带结构化 body/header：`X-Gate-Scope`、`X-Retry-After`（或标准 `Retry-After`）、`X-Suppressed-By`（可选）；日志必须同步 `suppressed_by`/`would_be_egress`。
- 不能把 429 用于 global shape（会诱导同 model 重试）；不能把 503 用于“仅该 model 配额耗尽”（会让客户端误以为网关整体不可用）。
- `Retry-After` 不得小于 30s，避免惊群；per-model 上限 300s，不能承诺 5h 后恢复。

### 8.4 裁决错误的后果

- **严重度：Important。** 透传 429：客户端同 model 重试，烧 IP/增加 429；全 503：客户端无法区分 model 配额，可能整站重试；header 缺失：重试风暴。不会像 D1/D3 那样必然复现 5h 事故，但会显著放大故障。

## 9. D8 三套 halt 是否必须合并为一个裁决点

**裁决：必须合并。接受 G1 的 K6 方向，但实现为一个纯函数 `Decide` + 固定优先级；不是把三个布尔简单 `||`。**

### 9.1 判断与理由

当前三套 halt 的交互没有唯一裁决点：

- `quotaHalt.gate` 在入口最先执行，且它内部调用 `proxyPool.hasEligibleNode()`；`hasEligibleNode()` 会 `sweepExpiredLocked`，把冷却到期节点转成 probation，并可能算作 eligible；这会让 quota_halt 在 shape 仍 armed 时先放行/先拒绝。
- 主循环内先 `poolHalted`，再 `egress.Get`/`shapeDriftBlockedEgress`，然后 `shapeDriftObserve429`；`pool403`、`shape_drift`、`quota_halt` 的顺序在 stream/non-stream 两处代码路径并不完全一致。
- 三条路径各自写 `suppressed`/`switch` 事件，字段不同；回放无法回答“这条请求被哪一层拦住、本应发到哪个 egress、当时有哪些候选”。
- `quota_halt.gate` 只看“有无 eligible node”，不区分“node 是 available 还是 probation 且尚未拿到证据”；这正是 D6 的 false recovery 入口。

### 9.2 唯一裁决点：函数签名

```go
type GateScope string

const (
    ScopePool403       GateScope = "pool403"
    ScopeGlobalShape   GateScope = "global_shape"
    ScopeModelQuota    GateScope = "model_quota"
    ScopeDirectQuota   GateScope = "direct_quota"
    ScopeLifecycle     GateScope = "lifecycle"
    ScopeNone          GateScope = "none"
)

type GateInput struct {
    Model                 string
    Now                   time.Time
    Pool403Halted         bool
    GlobalShapeArmed      bool
    GlobalTrialInFlight   bool
    TrialTokenValid       bool
    EligibleCandidates    int   // available + probation(one-shot), after pair exclusion
    ModelPairExcluded     int
    DirectHalted          bool
    AllModelsExhausted    bool
    ProbationCandidates   int
    Any2xxLast60s         bool
    LastAny2xxAge         time.Duration
    NewEgress429_10m      int
    ConsecutiveNew429     int
    Attempts60s           int
    FailureRatio60s       float64
}

type GateDecision struct {
    Scope         GateScope
    AllowNormal   bool
    AllowProbe    bool
    FailFast      bool
    Status        int
    RetryAfter    time.Duration
    SuppressedBy  string
    WouldBeEgress string
    CandidateN    int
    Reason        string
}

// Decide 必须是纯函数：只读快照，不调用 proxyPool/quotaHalt 的任何有副作用方法，
// 不持锁；状态提交由 Commit(decision) 在同一顺序的锁内完成。
func (a *Admission) Decide(in GateInput) GateDecision
```

### 9.3 固定优先级（从上到下，命中即返回）

| 优先级 | 条件 | 决策 | 状态/头 |
|---:|---|---|---|
| 1 | `Pool403Halted` | fail-fast，禁探测/直连 | 503 + `Retry-After: 30`；`scope=pool403` |
| 2 | `GlobalShapeArmed` | 只允许 `AllowProbe`（单飞、token 绑定 egress）；普通请求 fail-fast | 503 + 30；`scope=global_shape`；`suppressed_by=shape_drift_probe_only` |
| 3 | `AllModelsExhausted` | fail-fast，不直连 | 503 + 30；`scope=all_models` |
| 4 | `ModelPairExcluded>0 && EligibleCandidates==0` | 该 model 无候选；不全局 | **429** + min(300, until)；`scope=model_quota` |
| 5 | `DirectHalted` 且无 eligible node | 直连熔断 | 503 + 30；`scope=direct_quota` |
| 6 | `EligibleCandidates>0` | 允许 normal/probation（probation 只 1 次） | 200/allow；`scope=lifecycle` |
| 7 | 其他 | 无候选 | 503 + 30；`scope=lifecycle` |

- `GlobalShapeArmed` 必须在 `Pool403Halted` 之后、per-model 之前：403 是更硬的池级事实；shape 与 quota 同时存在时不要用 quota_halt 抢先 503。
- `ModelPairExcluded` 是 per-model 的“全排除”，不是全局；不能被 `quotaHalt` 的“无 eligible node”覆盖。
- `AllowProbe` 只对 global shape；per-model 的 probation 是**真实请求**（D6），不是 probe。
- `TrialTokenValid` 必须绑定具体 egress；`AllowProbe` 为 true 时只有一个 in-flight token；探测失败不得升级退避以外的状态。
- **禁止** `Decide` 内部调用 `hasEligibleNode()`/`sweepExpiredLocked()`；快照由调用方在同一锁序下生成，避免锁反转和隐藏状态迁移。
- 状态提交顺序：`pool403 -> global shape -> model quota -> direct quota -> nodePool`；任何日志/`suppressed` 在同一事务中写，先写抑制后发请求。

### 9.4 裁决错误的后果

- **严重度：Critical。** 三套并存：18:03 的 quota_halt 可能在 shape 之前 503，或者 `hasEligibleNode` 把 probation 当可用而绕过全局闸；同一条请求可能被重复计数/重复抑制，回放无法归因；最坏情况下 `pool403` 与 `shape` 互相“放行”，重新打上游。合并后仍需保留三条独立证据，否则回滚无法定位。

## 10. 评审结论

### 10.1 D1–D8 一行表

| # | 裁决 | 一句话 |
|---|---|---|
| D1 | **都不接受 → 第三方案** | 默认 per-(model,egress)；全局只在“60s 内任意 model 无 2xx + 5 新出口 + 无 2xx + ratio≥0.9”时启用 |
| D2 | **接受 B，但补 per-pair** | K=5 连续新出口取代 3 distinct；单出口 429 走 per-pair，不升级全局 |
| D3 | **接受 G1** | TTL 只推 nextProbe；绝不 clear blocked；否则 30 次/出口，超过 10–16 |
| D4 | **接受 A（限 shape-suspect）** | `H_bad=8` 作为硬预算，不统计普通配额；无一手封禁阈值，8 是保守值 |
| D5 | **拒绝固定 5h** | 190 个首标记 100% +5h；429→成功 97.9% 删失；改为 5m/30m/2h/5h + 真实证据 |
| D6 | **第三方案** | 配额 probation 只用真实请求；探测只用于 dead/transport；删除 probe 提升 |
| D7 | **第三方案** | 全局 503；per-model 429；统一 Retry-After；禁止透传上游 429 |
| D8 | **必须合并** | 唯一 `Decide`；`pool403 > global shape > model quota > direct quota > selection` |

### 10.2 能否进入 R4

**不能。R3 = NO-GO。** DRAFT 的合并方向正确，但 D1/D2 的全局默认、D3 的 TTL 清空歧义、D5 的 5h 假设、D6 的 probe 提升、D8 的三套 halt 都还是“会复现事故”的级别。R4 应只审“修完后的 v3.1”，不应把本轮结论当作实现许可。

### 10.3 必须先改的 3 条（R4 的入口条件）

1. **D1/D2**：把全局 `probe_only` 改为严格例外（§2.3），默认 `(model,egress)` 排除；K=5 + 任一 2xx 否决；单出口重复 429 不升级全局。
2. **D3/D5/D6**：TTL 只推 `nextProbeAt`，blocked 只由证据清除；固定 5h 改为递进退避 + 真实请求 canary；删除 `recordProbeSuccess` 对配额 probation 的提升。
3. **D8/D7**：落地唯一 `Decide` 函数与固定优先级，统一 503/429/Retry-After 和抑制字段；禁止 `quota_halt` 抢在 shape 前、禁止全排除回退直连、禁止透传 429。

> D4 的预算 8 与 D7 的具体 header 值可以在 R4 同批定稿；但它们不阻塞 D1/D3/D8 这三条 Critical。

## 附录 A：复现与原始输出

- D1：`/tmp/t7_d1.py`；10-26+27 全量，`err_msg` 口径 113 组合/11 both；event 口径 16 both。
- D2：`/tmp/t7_d2b.py`；C10 3 distinct 在 18:03:58/20:00:09 触发；K=5 这两段不触发；10-27 单出口不触发。
- D3：`/tmp/t7_d3.py`；N=212、R=0.5/s、D=5h；结果 66/124/239 sends，max hits 1/1/30。
- D5：`/tmp/t7_d5b.py`（429→success，排除同 request）；`/tmp/t7_appstate.py`（mark/recovery、190 个首标记、539 次 recovery）。
- SHA/文件大小/行数：以 `/tmp/cl-1026b.jsonl`（13,724 行）、`/tmp/cl-1027b.jsonl`（541 行）为准；脚本只读。

## 附录 B：未验证/数据缺口

1. **上游封禁阈值 10–16 没有一手证据**；D4 的 8 是保守预算，不是实测阈值。
2. **客户端重试行为没有埋点**；D7 的 429/503 分流基于 HTTP 语义与现有代码，不是客户端实测。
3. **10-26 的 429 全部是 `FreeUsageLimitError` 配额，不是真 shape**；因此本文不能用 10-26 验证“真 shape 下 probe 的成功率”。真 shape 只能靠仿真/灰度。
4. **429→真实恢复分布被闩/冷却严重删失**；D5 的“5h 无依据”是确定的，但“真实恢复中位数”无法从现有日志估计。
5. `app1.log.gz` 从 10-07 开始，18:03/23:03 的事件与 Lead 的 21/29/9 节点口径存在“mark 行 vs distinct fp”的差异；本文按 distinct fp 统计，并把 mark 行数单独列出。
