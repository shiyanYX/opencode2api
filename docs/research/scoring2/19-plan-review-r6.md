# R6 对抗复审 · 节点系统 v4（按出口熔断 + fail-fast）

> 输入：`docs/superpowers/plans/2026-10-27-node-system-v4.md`（DRAFT v4.0，105 行，08:28）、`docs/superpowers/plans/2026-10-27-node-system-v3.md`（v3.2，147 行）、`docs/research/scoring2/18-plan-review-r5.md`（R5，R5-1…R5-8）。
> 代码基线：`HEAD 465f5df`（只改文档）+ 工作区未提交改动；`shape_drift.go` sha256 `1201512362cb6f4f…`（00:23），`proxy_node.go` 已含 `456256f` 的 I4 幂等与 probation。
> 数据基线：`/tmp/cl-1026b.jsonl`（13,724 行，10-26 15:16–23:59）+ `/tmp/cl-1027b.jsonl`（541 行，10-27 00:00–00:23），共 14,265 条 call_log。
> 约束：只读评审，不改代码、不 commit、不启动服务；本报告是唯一新增文件。

## 0. 结论（先给）

**仍需修订（NO-GO），不可按 v4.0 进入 S1 实现。**

但方向是对的：**删掉全局闩是正确决定**，而且有生产数据直接支持（§Q6：00:20 闩死时 `27436cb4` 仍在服务 33 次成功、`41d39652` 独吞 165 次失败）。v4 的问题不是「太简单」，而是**在 4 个关键点上把「简化」做成了「缺能力」**；补 4 个局部规格即可，不需要退回 v3.2 的五层 + per-pair + 24h 预算。

### 0.1 Blocker（R6-B1…B4，按严重度）

| ID | 严重度 | Blocker | 最短证据 |
|---|---|---|---|
| R6-B1 | Critical | **1 次 429 就开熔断**，把 2.7% 噪声率的健康出口判死 | 实测 2,671 条成功中 **464 条（17.4%）** 发生在同出口首次 429 之后；`4ad1982e` 6 次 429 vs 219 次成功（97.3% 成功率）、`27436cb4` 1 次 429 vs 1,022 次成功 |
| R6-B2 | Critical | **`inflight<1`** 把「有出口但在途」当 `no_candidate`，503 数 = `max(0, C−N)` | 实测峰值并发 37、峰值到达 36 req/s；N=212 时 0，事故后 N≈22 时 **15/37=40% 503**；且流式整段占租约 |
| R6-B3 | Critical | **I3 未实现**：canary 只在「候选为空」时放行；有 212 个出口时 `until` 到期即**直接重新进入正常候选**，`canaryInFlight` 在正常路径从不置位 | v4 §1 第 3 步 vs §2 的 I3 行自相矛盾；`strikes>0` 的出口到期后只受 `inflight<1` 限并发、不限次数 |
| R6-B4 | Critical | **真风暴下恢复延迟 ≤5h**：ladder 封顶 5h，全部出口同时 429 ⇒ 下一次探测在 5h 后；没有快速全局探测 | 事故本身就是这样恢复的：`f82d885d` gap=**455min**（5h 冷却到期）后首条仍是 429；v4 把这个「5h」从 bug 变成了设计 |

### 0.2 v4 是否真的消掉了 R5 的八条 blocker

| R5 blocker | v4 处理 | 裁决 |
|---|---|---|
| R5-1 三段式 `Snapshot/Decide/Commit` 竞态 | **删除**，改为 `Admit/Complete` 单锁 | ✅ 结构性消除 |
| R5-2 五层条件化 M/D/S 与 direct 回退矛盾 | 五层删除；但 `egress.go:143` 的 direct 回退仍在，v4 未写怎么移进 `Admit` | ⚠️ 残留（§Q5、I1） |
| R5-3 per-pair 首次 5m 与「10min 第 3 次」互斥 | per-pair 删除，改 per-egress | ✅ 消除；但换成 B1（1 次 429 即封） |
| R5-4 24h 预算把普通 FUL 锁 24h | 预算删除 | ✅ 消除 |
| R5-5 双文件持久化 / stale 复活 | `gate_state.json` 删除 | ✅ 消除；但变成「重启全丢」（§Q6 重启） |
| R5-6 meter/clock 可测性 | meter 删除；`Admit/Complete` 收 `now` | ⚠️ 部分（fake clock 仍要覆盖 pool/quota） |
| R5-7 切片依赖倒置 | v4 只有 S1/S2，S3 可选 | ✅ 消除 |
| R5-8 验收断言不可自动化 | 给了 15 测试/8 变异，但断言未逐条写死 | ⚠️ 部分（§5 给最小套件） |

**一句话**：v4 把 R5 的 8 条里 5 条连根删掉了，这是净收益；代价是引入了 4 条新的、可以用数字证明的缺能力。

## 1. 方法与边界

- **不是**再找补丁：先按「删机制」的方向独立推演 v4 的行为，再用 10-26/10-27 全量 call_log 做反事实回放（per-egress 事件序列套 v4 状态机），最后逐条对撞 v4 文本。
- 反事实回放的边界：call_log 只记「实际用到的出口」，v4 会改选路，所以回放结果是**单出口上界**，不是全系统精确值；用于回答「同一出口的事件序列在 v4 下会发多少次/封不封」，不用于预测总量。
- 并发用 `duration_ms` 重叠计算（峰值 37）；到达率用 `ts` 按秒计数（峰值 36/s）；流式只有 3 条（最长 4.4s），不足以代表长流，§Q4 用代码上界（`idleReadTimeout=120s`）补。
- 代码事实：`egress.go:143`（全冷却回退 direct）、`proxy_node.go` `markLocked`（I4 幂等已存在）、`node_scoring.go:947`（`oc.quota → QuotaEvents++`，429 不进质量样本）、`main.go:2878/3397`（`quotaHaltGuard.gate`）、`main.go:2946/2967/3147`（`poolHalted`）、`main.go:3328-3343`（direct 429 → `quota_halt.halt`）。

## 2. 七问逐条

### Q1 真 shape 风暴下，每出口 canary 的 24h 最坏发送数

**v4 §3 的「8 次/日」只在 `strikes` 单调不减时成立。** 按 v4 的 ladder（5m/30m/2h/5h 封顶）且从不 reset：

```text
t=0 发送#1 → 429 → strikes=1, until=+5m
+5m #2 → +30m #3 → +2h35m #4 → +7h35m #5 → +12h35m #6 → +17h35m #7 → +22h35m #8
24h 合计 8 次/出口（< 10–16 阈值 ✓）；212 出口合计 1,696 次/日。
首小时：212 个 canary 在数秒内并发 → 212 次；+5m 212；+35m 212；+2h35m 212 ⇒ 前 3h 848 次，每 IP 4 次（安全）。
```

**但 v4 §1 的 `Complete` 里有 `canaryOK>=2 且间隔>=60s → strikes=0, until=zero`。** 一旦 reset，梯子从头爬，8 次/日的上界立刻失效。生产数据证明「首次 429 之后出口仍会成功」不是理论：

- 全量 2,671 条带节点成功中，**464 条（17.4%）发生在同一出口首次 429 之后**；
- `4ad1982e`：首次 429 在 17:20:37，之后 **519s 内还有 31 次成功**（17:20–17:29 桶），17:30 桶再 53 次、17:40 桶再 23 次；
- `cbaac8eb`：首次 429 后 319s 内 **107 次成功**；
- `51924bd3`：首次 429 后仍有 24 次成功，最后一次在 19:17:17（首 429 是 18:12:56）。

按 v4 的 reset 规则做最坏推演：每个「2 次成功（≥60s 间隔）」把 `strikes` 清零 ⇒ 一个 5–6 分钟的周期可以是「2 次成功 + 1 次 reset 后 429」= 3 次上游发送，其中 1 次是 429。24h ≈ 240–288 个周期：

```text
单出口 24h 最坏（有 reset）≈ 240–720 次，其中 429 命中 240–720 次
→ 是 10–16 阈值的 15–45 倍
212 出口合计最坏 ≈ 5.1 万–15.3 万次/日
```

**结论**：阈值是按 IP/日的，所以 212 出口的总量不是关键；关键是**单出口有 reset 时无法被 ladder 约束**。v4 的「8 次/日」论证漏掉了自己写的 reset 分支。修法见 §4-B1/B3：reset 必须是「慢衰减」而不是清零（例如 `strikes=max(0,strikes-1)` 且 24h 内最多降 1 档），或 reset 要求 `K=3` 次成功且窗口 ≥10min。

### Q2 出口级误伤：同一出口 A 模型 429、B 模型正常

**存在，而且不是小数。** 逐 egress 统计（全量 14,265 条）：

| 出口 | 429（model） | 2xx（model） | 1-strike 会误杀 |
|---|---|---|---|
| `4ad1982e` | 6 次，全为 `mimo-v2.6-flash-free` | 219 次：mimo 121 + **deepseek 98** | 第 1 次 429 就封 5m ⇒ 98 条 deepseek 成功被挡 |
| `cbaac8eb` | 1 次，mimo | 201 次：mimo 163 + **deepseek 38** | 同上 |
| `27436cb4` | 1 次，mimo | **1,022 次**：mimo 816 + deepseek 206 | 00:20 正在服务的节点被判死 |
| `51924bd3` | 5,114 次：mimo 4,712 + deepseek 402 | 178 次 | 应封（71 次 429/min），封得对 |

按 (egress, 10min) 桶算：154 个桶里「有 429 且有 2xx」11 个（**7.1%**），其中 429 的 model 不在 2xx model 集合里的只有 1 个（**0.6%**）。**但 0.6% 是桶级口径，低估了真实代价**：按请求量算，**464/2,671 = 17.4% 的成功会被 1-strike 压掉**；若只看事故窗口（18:00–23:07 的 1,015 条成功），这个比例是 **359/1,015 = 35.4%**。

**结论**：
1. 「跨模型不同步」不是主因（0.6% 桶），**「单次 429 不代表出口坏」才是主因（17.4% 成功）**；v4 把两者混在一个 `strikes++` 里。
2. 1-strike 的收益（挡掉 11,417 次失败）远大于代价（464 次成功），所以**不是「不要熔断」，而是「触发条件必须是速率而不是计数」**（§4-B1）。
3. `27436cb4` 是最刺眼的例子：1 次 429 / 1,023 次请求（0.1% 错误率）在 v4 下会被封 5 分钟；而它正是 00:20 事故里唯一在服务的出口。

### Q3 `inflight<1` 的假 `no_candidate`

**`inflight<1` 把「在途满」和「没有候选」合并成同一个 503，这是设计错误，不是实现细节。**

- 实测并发：峰值 **37**（`duration_ms` 重叠），峰值到达 **36 req/s**（20:28:11）；p50 1.3s、p95 29s、p99 57s、max 120s。
- v4 的容量 = 可用出口数 `N`（每个出口同时只允许 1 个在途）。因此 503 数 = `max(0, C−N)`：
  - `N=212`（v4 假设的池）→ C=37 时 **0**，正常；
  - `N=22`（事故后 190 个 `NodeExhausted` 5h 冷却，若 S2 把它们迁成 breaker strikes）→ **15/37 = 40% 503**；
  - `N=3`（事故中实际在用的出口数）→ **34/37 = 92% 503**。
- 与现有重试循环的兼容性：`Admit` 返回 503 后，客户端/中间层会重试；重试再 `Admit`，如果 `N<C` 就再 503，**形成 503 忙等**（每个重试都消耗一次准入判定，但不发上游）。v4 §1 说「禁止空转」，但 `inflight<1` 恰好制造了一种新的空转：有出口、但全在途。
- 流式：租约必须在整个流期间持有（`main.go:3028/3542` 的 `newIdleReadCloser` 只限 idle，不限总时长；实测流最长 4.4s，但 p99 非流式已 57s）。`inflight<1` 下一条长流独占一个出口；若流超过 5min 租约，sweep 释放后会被第二个请求复用 ⇒ **同一出口并发 2 个**（租约泄漏的另一面）。

**修法**：`inflight < K`（K=4，或 `max(1, ceil(1.5·C/N))` 的自适应值）；把「在途满」与「无候选」分成两个 reason：`all_inflight`（503 + 更短 Retry-After）vs `no_candidate`（503 + 长 Retry-After）。验收：`C=37, N=22` 仿真断言 503 ≤5%，且 `all_inflight` 可观测。

### Q4 租约泄漏（panic / 超时未 `Complete`）

v4 只写了「租约超时（例如 5 分钟未 `Complete`）由 sweep 释放」，没写：

1. **sweep 间隔**：必须 ≤ 租约/2（否则一个泄漏租约让该出口在最坏 2×5min 内不可用；`inflight<1` 下等于出口消失）。
2. **panic 路径**：`Admit` 之后到 `Complete` 之间要 `defer Complete(lease, aborted)`；`http.Server` 会 recover 每个 handler 的 panic，所以 defer 能跑到。但 **`os.Exit`/进程崩溃**跑不到 ⇒ 只能靠 sweep；重启后内存态全丢（v4 没有持久化，见 §Q6）。
3. **T3 首字节超时**：`firstByteTimeout = firstByteDeadline(globalTTFTP95)`（动态，测试里 19.6s），`idleReadTimeout=120s`。v4 说「outcome 归为其他失败，不计 breaker」——**但租约仍必须 `Complete`**。如果实现里 `continue` 到下一个出口时忘了先 `Complete` 旧租约，旧出口就永久 `inflight=1`（直到 sweep）。
4. **跨出口重试**：一个请求最多 `maxQuotaSwitches=5` 次；每次换出口必须换租约。如果租约绑在「请求」而不是「attempt」，旧出口的租约要挂到请求结束（最长 120s），期间该出口不可用。
5. **5min 租约 vs 长流**：流式生成可以超过 5min（虽然本次日志只有 3 条、最长 4.4s）。建议流式租约用 `max(5min, 2×idleReadTimeout)` 或按「最后一个字节」续租。

**验收**：fake clock 下构造「Admit 后不 Complete」→ 断言 sweep 在 ≤2×租约后释放；构造 panic → 断言租约释放；构造 first-byte timeout → 断言旧租约已释放且新租约已获取。

### Q5 `EGRESS_BREAKER=off` 是否真回到 HEAD 且不绕过 pool403/quota_halt

**当前 HEAD 的拒绝顺序**（`main.go`）：

```text
quotaHaltGuard.gate(auth)        # 2878 非流式 / 3397 流式  → 503
loop:
  poolHalted(now)                # 2946/2967（非流式）、3147；流式 3466/3487/3645 → 503
  egress.Get(...)                # 选路；全冷却 → direct（egress.go:143）
  ...
  429 → quotaHaltGuard.halt(egressKey, "direct_429", ...)   # 3328-3343（仅 direct）
```

- `off` 必须插在 `quotaHaltGuard.gate` **之后**、`egress.Get` **之前**；若插在最前面（「完全绕过」的字面实现），就会绕过 pool403/quota_halt，直接违反 v4 自己的约束。
- `off` 下 `egress.Get` 的 direct 回退会恢复：全冷却时打主 IP（`egress.go:143`）。这是 HEAD 行为，可接受，但**必须写进回滚判据**（off 后 direct 429 数上升 = 预期）。
- `off` 不应关闭 `quotaHaltGuard.halt`（v4 说「不绕过 quota_halt」✓）；但 `halt` 只在 direct 429 时触发，而 `off` 下 direct 回退变多 ⇒ **quota_halt 触发概率上升**，需要在 off 的观察项里加 `quota_halt_entered`。
- **验收**：off 下构造 pool403 触发 → 断言仍 503 + `suppressed_by=pool403`；构造 quota_halt 触发 → 断言仍 503；构造全出口 blocked → 断言回到 direct（与 HEAD golden 一致）。

### Q6 能力遗漏：逐条对照 8 条不变量 + 事故证据

| 不变量 | v4 文本 | 实测裁决 |
|---|---|---|
| I1 不得把流量钉在已知 429 出口 | 429 出口进 `until` | ✅ 主路径；⚠️ `egress.Get` 的 direct 回退未移进 `Admit`；⚠️ 重试必须重新 `Admit`（否则复用旧租约） |
| I2 抑制必须留痕 | `suppressed_by` + `would_be_egress` | ⚠️ `candidate_n==0` 时 `would_be_egress` 为空；应改记 `candidate_n` + `blocked_by` 直方图 |
| I3 恢复必须有证据 | 「`until` 到期只给 1 次 canary，2 次 2xx 才清零」 | ❌ **未实现**：canary 只在「候选为空」分支；有候选时到期即正常放行；`canaryInFlight` 正常路径不置位（B3） |
| I4 mark 幂等 | 「已在 456256f」 | ✅ 已核实：`markLocked` 对同 `(state, reason)` 且冷却内重复写入返回 `changed=false`；⚠️ 依赖 reason 字符串稳定（`"quota:"+quotaReason`） |
| I5 闩必须自愈 | 「没有闩；breaker 到期即给 canary」 | ⚠️ 到期即放行（同 I3），且封顶 5h ⇒ 自愈延迟 ≤5h（B4） |
| I6 halt fail-fast | 无候选 503 | ✅；但 `inflight<1` 制造的「假无候选」也 503（B2） |
| I7 打分不把准入当质量 | breaker 与 scoring 分离 | ✅ 已核实：`node_scoring.go:947` `oc.quota → QuotaEvents++; continue`，429 不进质量样本 |
| I8 未尝试 ≠ 失败 | `suppressed` 与 attempt 分开 | ✅；但 503 是新的下游失败模式，必须带 `Retry-After`（v4 有） |

**事故证据逐条**：

1. **18:03 多出口 429（22 个 distinct egress）**：v4 会封这 22 个、其余继续 —— 比旧实现（全局闩）好。但注意 22 个里有多少是「1 次 429 就封」的误伤？见 Q2：`4ad1982e` 就是 18:03 窗口内 6 次 429、219 次成功的出口。
2. **00:20 单出口 165 连发**：v4 会在 `41d39652` 第 1 次 429 后封它，下一条请求选 `27436cb4`（当时 33 次成功）⇒ **v4 确实能修这个事故**。这是 v4 最强的证据。
3. **5h 停机**：事故是「3 个出口被闩死 + 不 mark + 不切换」；v4 没有全局闩，不会钉死；但**恢复延迟**换成 ladder 的 5h（B4），停机形态从「钉死 1 个出口」变成「全池 5h 冷却」——**总停机时间可能一样**。
4. **3 个出口 vs 212 个出口同时 429**：v4 不需要显式区分，行为自然不同（3 个 → 209 个候选；212 个 → `no_candidate` 503）。**但 `pool403` 仍是全局的**：`pool403Min=8`、10min 窗口、`poolHaltFor=10min`，8 个出口 403 就 503 全站 10 分钟 —— 这是 v4 保留的**另一个全局闩**，与「删掉全局闩」的宣言矛盾；且 403 不计入 breaker（v4 的 `Complete` 只认 FUL/429），所以 403 风暴会每 10 分钟循环一次 halt，breaker 永远学不到。本次日志 403 事件为 **0**，所以不是当前事故的主因，但必须在计划里写清 403 是否 strike。
5. **重启**：v4 删了 `gate_state.json`，breaker 全在内存 ⇒ 风暴中重启 = 冷启动把全池再烧一遍（每次重启 +212 次首击）。至少要持久化 `strikes/until`，或把「重启后首小时发送数」写进回滚判据。

### Q7 是否过度简化 / 过度设计

**v4 比 v3.2 好，但欠 4 处；不是过度设计。**

- **v3.2 是过度设计**：五层裁决 + per-pair 状态机 + 24h 预算 + 双文件持久化，R5 的 8 个 blocker 里 5 个（R5-1/2/3/4/5）直接来自这些机制；它们要解决的问题（「A 模型 429 但 B 正常」）在数据里只占 0.6% 桶（Q2），不值得用 5 层去换。
- **v4 的核心判断正确**：全局闩在 00:20 把流量钉在 `41d39652`（165 次失败），而 `27436cb4` 同期 33 次成功 —— 这是「删掉全局闩」最硬的生产证据。
- **v4 欠的是 4 个局部规格**，不是新机制：
  1. 触发条件从「1 次 429」改成「60s 内 ≥3 次 429 或 ≥4 次尝试中 429 占比 ≥50%」（B1）；
  2. `inflight<1` → `inflight<K`，并拆 `all_inflight`/`no_candidate`（B2）；
  3. `strikes>0` 时强制单飞 canary（B3）；
  4. `candidate_n==0` 时每 60s 一个全局探测（B4）。
- 这 4 条加起来约 150 行 + 4 组表驱动测试，**远小于 v3.2 的五层**；补完后 v4 是「正确的最小系统」。

## 3. 与 T2 / T3 / quota_halt / direct 的交互矩阵

| 场景 | v4 行为 | 与现有机制的冲突 | 结论 |
|---|---|---|---|
| 429 风暴（本次事故） | 每个出口第 1 次 429 → `until=+5m`；3 个出口 → 209 个候选；212 个 → 503 | 无 | ✅ 修「钉死」；❌ 误伤（B1）、恢复慢（B4） |
| 403 风暴（T2） | `pool403` 在 `Admit` 最前 → 503；403 不进 breaker | **保留了一个全局闩**：8 distinct/10min → 全站 503 10min；10min 后 403 再来一轮 | ⚠️ 与「删全局闩」矛盾；403 是否 strike 必须写死 |
| `quota_halt`（旧） | 只在 `direct` 429 时 `halt(egressKey)`；`gate` 需要 `!hasEligibleNode()` | breaker 不 mark ⇒ `hasEligibleNode()` 恒真 ⇒ `gate` 永不触发（除非池真空） | ✅ 风暴中不会叠加；但 off 下 direct 回退增多 ⇒ `halt` 概率上升 |
| `NodeExhausted` 迁移（S2） | 旧 5h 冷却 → breaker strikes | 迁移时 190 个 5h 冷却若映射成 strikes=4（5h），池子从 212 变 22 ⇒ `inflight<1` 下 40% 503 | ❌ **迁移必须压平**（映射到 strikes=1/5m），否则上线即 503 |
| T3 首字节超时 | 「其他失败」不计 breaker | 租约必须 `Complete`；跨出口重试要换租约 | ⚠️ 规格缺失（Q4） |
| direct 回退（`egress.go:143`） | 「禁止回退直连」 | `Admit` 之后仍调 `egress.Get` 时会回退；off 下也回退 | ❌ 必须移进 `Admit` 或显式 `ExcludeDirect` |
| 重启 | 内存态全丢 | 风暴中重启 = 全池再烧一遍 | ⚠️ 至少持久化 `strikes/until` |
| 长流（>5min） | 5min 租约 sweep | 同一出口可能并发 2 个 | ⚠️ 租约按「最后字节」续租 |

**「3 个出口同时 429」与「212 个出口同时 429」如何区分**：v4 不需要显式区分，行为自然分叉（3 → 209 候选；212 → 503）。**但前提是 N 真的是 212**；事故后 190 个 5h 冷却让 N≈22，这时「3 个 429」就已经等于「N 的 14%」，而 `inflight<1` 会把「在途满」误报成 `no_candidate`。所以 B2 和 S2 迁移必须一起做。

## 4. 最小必要补充（4 个 patch 的规格）

### B1 触发条件：计数 → 速率

```text
状态：breaker[e] = {strikes, until, lastHit, windowStart, hits429, attempts60s, canaryInFlight, canaryOK, lastOK}
Complete(e, outcome, now):
  attempts60s = 60s 滑动窗口内的尝试数（含本次）
  hits429    = 60s 窗口内的 429 数（含本次）
  if outcome == FUL/429:
      if hits429 >= 3 || (attempts60s >= 4 && hits429/attempts60s >= 0.5):
          strikes++; until = now + ladder[strikes]; canaryOK = 0
      else:
          # 单次 429 不封：只记 lastHit，供告警/后续速率判断
          pass
  if outcome == 2xx:
      canaryOK++; lastOK = now
      if strikes > 0 && canaryOK >= 3 && now-lastOK >= 10min:
          strikes = max(0, strikes-1)   # 慢衰减，不清零
          if strikes == 0: until = zero
```

- `ladder` 保持 `5m/30m/2h/5h`。
- **为什么是 3/60s**：`4ad1982e` 6 次 429 分布在 40 分钟（≈0.15/min），不会触发；`51924bd3` 71 次/min，3 秒内触发。这是用真实序列能分开两者最简的阈值。
- **为什么 reset 用「降一档 + 10min + K=3」**：v4 的「2 次 60s 清零」在 17.4% 的 flap 率下会让梯子无限重来（Q1）。
- 测试（表驱动，直接喂 Q2 的真实序列）：`4ad1982e` 不封；`cbaac8eb` 不封；`27436cb4` 不封；`51924bd3` 在第 3 次 429（同一 60s）后封；`30a03abd` 在首次 429 后封（它 100% 失败，60s 内 ≥3 次）。

### B2 并发门：`inflight<K`

```text
候选 = 未 blocked 且 inflight[e] < K，K=4（可配 EGRESS_INFLIGHT_MAX）
若候选为空:
   if any inflight[e] >= K 且 e 未 blocked -> 503 reason=all_inflight, Retry-After: 1s
   else if canary 可用 -> 放行 1 个 canary（见 B3）
   else -> 503 reason=no_candidate, Retry-After: 30s
```

- `K=4` 的依据：峰值并发 37、峰值到达 36/s；N=212 时 `K=1` 也不会 503，但 N=22 时 `K=4` 可撑 88 并发，覆盖 37 峰值 2.4×。
- 验收：`C=37,N=22,K=4` 仿真 503 ≤5%；`C=100,N=22,K=4` 断言 `all_inflight` 而非 `no_candidate`，且 503 带 `Retry-After: 1`。

### B3 canary 单飞：`strikes>0` 时生效

```text
若 strikes[e] > 0 且 until[e] <= now:
    e 只能以 canary 身份被选中：
      - 若 canaryInFlight[e] -> 跳过
      - 否则 canaryInFlight[e]=true，lease.canary=true
Complete(e, outcome):
    if lease.canary: canaryInFlight[e]=false
    # 成功/失败按 B1 的 canaryOK 规则；失败 strikes++
```

- 这样「until 到期只给 1 次探测」才是真的；v4 现在的「候选为空才 canary」在有 212 个出口时永远不触发。
- 验收：到期后 100 并发 → 只发 1 次；canary 成功 1 次 → `strikes` 不变；成功 3 次且 ≥10min → 降一档；canary 失败 → `strikes++` 且 `until` 按新档。

### B4 全局探测：`candidate_n==0` 时 60s 一次

```text
if candidate_n == 0 && now >= nextGlobalProbe:
    e = 轮转挑一个 blocked 出口（按 lastHit 最旧优先）
    if !globalProbeInFlight: 放行 1 个 canary（lease.canary=true）
    nextGlobalProbe = now + 60s
```

- 这是 1 个定时器 + 1 个令牌，不是全局闩：它**不阻止**任何正常候选，只在全 blocked 时提供「快速发现恢复」。
- 没有它，ladder 封顶 5h ⇒ 恢复延迟 ≤5h（B4 的证据：`f82d885d` gap=455min）。
- 验收：fake clock，t=0 全 blocked，t=10min 上游恢复 → 断言 t+11min 内出现 2xx；断言探测 ≤1/min；断言探测失败只让该出口 `strikes++`，不影响其他出口。

## 5. 实现顺序、每步验收命令、最可能翻车的 3 个地方

### 5.1 实现顺序（每步一个可回滚提交）

| 步 | 内容 | 依赖 | 回滚 |
|---|---|---|---|
| S1a | `breaker.go`：`Admit/Complete` + B1 速率触发 + B2 `inflight<K` + B3 canary 单飞；**不接线**（main 走旧路径） | 无 | 删除文件即可 |
| S1b | 接线两条 main 循环；`EGRESS_BREAKER=off` 默认；把 `egress.Get` 的 direct 回退移进 `Admit` | S1a | 关开关 |
| S1c | B4 全局探测 + 结构化事件（`suppressed_by`/`candidate_n`/`would_be_egress`/`all_inflight`） | S1b | 关开关 |
| S2 | `NodeExhausted` 迁移：5h → `strikes=1`（**压平，不要映射成 5h**）；删除 `recordProbeSuccess` 对 `NodeProbation` 的提升 | S1b | 保留旧状态文件 |
| S3 | per-`(model,node)` 状态：**只有 Q2 的 0.6% 变成 >5% 时才做**；默认不做 | S2 | 不做 |

### 5.2 每步验收命令

```bash
export PATH=$PATH:/usr/local/go/bin
export GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"

# 编译/静态
go build ./... && go vet ./... && gofmt -l .

# 全量（IP4RV 是已知的独立失败，跳过）
go test -skip 'TestIP4RV' -count=1 ./...

# S1a 定向
go test -run 'TestBreaker' -count=1 -v ./...
go test -run 'TestBreakerRateTrigger|TestBreakerInflightK|TestBreakerCanarySingleFlight' -count=1

# 反事实回放（本报告 Q2 的序列）
go test -run 'TestBreakerRealSequence' -count=1 -v ./...
#   断言：4ad1982e 不封、27436cb4 不封、51924bd3 第 3 次同 60s 429 后封

# fake clock 5h 仿真
go test -run 'TestBreakerStorm5h' -count=1 -v ./...
#   断言：单出口发送 ≤8（无 flap）/ ≤720（有 flap 的最坏）；direct=0；恢复 ≤11min（B4）

# 变异（每个都必须红）
for m in no-rate-trigger no-inflight-k no-canary-single-flight no-global-probe \
         no-2xx-decay no-pool403-first no-direct-exclude no-suppressed-event; do
  ./scripts/mutate.sh "$m" && echo "MUTATION $m DID NOT GO RED" && exit 1
done
```

### 5.3 最可能翻车的 3 个地方

1. **B1 的速率窗口实现**：`attempts60s`/`hits429` 如果按「每次 `Complete` 重建窗口」而不是滑动窗口，会在低流量时永远凑不满 3 次（出口永远不被封），在高流量时又会把 3 次 429 算成 30 次（过度封禁）。必须用 60 个 1s 桶或环形缓冲，并在测试里固定 `now`。
2. **B2 的 `inflight` 计数与租约生命周期不一致**：`Admit` 加、`Complete` 减；任何 early return / panic / 跨出口重试都会让计数漂移。一旦漂移，出口会永久 `inflight>=K`（等价于 v4 的 `inflight<1` 永久泄漏）。必须在 `defer` 里做幂等的 `Complete`（`sync.Once` per lease），并加 sweep 对账（`inflight` 与活跃 lease 集合对不上就告警）。
3. **off/direct/pool403 的顺序**：`off` 插在最前面会绕过 pool403/quota_halt；`Admit` 之后仍调 `egress.Get` 会走 direct 回退；S2 迁移把 5h 映射成 5h 会让上线即 40% 503。这三个都是「顺序/映射」错误，测试要专门覆盖（off + pool403、Admit 后 direct、迁移后 N=22 并发 37）。

## 6. 未验证 / 数据缺口

1. **403 事件为 0**：本次 14,265 条日志里没有 403，所以 T2 的 `pool403` 在本事故中未触发；但 v4 保留了它，且 403 不进 breaker。403 风暴的行为**没有生产数据**，只能纸面推演。
2. **流式样本只有 3 条**（最长 4.4s）：长流（>5min）与租约的交互没有实测，只能按 `idleReadTimeout=120s` 推。
3. **call_log 不记被抑制的尝试**：无法直接测「v4 会少发多少」，只能用 per-egress 反事实回放；回放假设「同一出口的事件序列不变」，会高估单出口发送数。
4. **生产节点数未知**：本工作区 `proxy_state.json` 是空的（`{"version":1,"nodes":{}}`），212 是计划里的数字；日志里 6 小时只出现 93 个 distinct egress，18:00 窗口 22 个。**N 到底是 212 还是 22，直接决定 B2 的 503 率**，必须在生产 state 上核对。
5. **190 个 `NodeExhausted`**：来自 v3 文档的计数，未在本工作区复现；S2 迁移前必须在生产 `proxy_state.json` 上跑一次分布统计（strikes/until 分布、按 5h 到期的数量）。

## 附录 A 复现命令

```bash
# 1) 逐 egress 统计（Q2）
python3 - <<'PY'
import json,collections,datetime as dt
rows=[]
for p in ['/tmp/cl-1026b.jsonl','/tmp/cl-1027b.jsonl']:
    for line in open(p):
        line=line.strip()
        if not line: continue
        r=json.loads(line)
        if r.get('ts'): rows.append((dt.datetime.fromisoformat(r['ts']),r))
by=collections.defaultdict(list)
for ts,r in rows:
    for n in (r.get('nodes') or []): by[n].append((ts,r.get('status')))
for n,evs in sorted(by.items(),key=lambda kv:-len(kv[1]))[:6]:
    evs.sort(); fi=next((i for i,(t,s) in enumerate(evs) if s=='fail'),None)
    after=sum(1 for t,s in evs[fi:] if s=='ok') if fi is not None else 0
    print(n[:8],len(evs),'after_first_429_ok=',after)
PY

# 2) 峰值并发（Q3）
python3 - <<'PY'
import json,datetime as dt
ev=[]
for p in ['/tmp/cl-1026b.jsonl','/tmp/cl-1027b.jsonl']:
    for line in open(p):
        line=line.strip()
        if not line: continue
        r=json.loads(line)
        if not r.get('ts'): continue
        t=dt.datetime.fromisoformat(r['ts']); d=r.get('duration_ms') or 0
        ev.append((t,1)); ev.append((t+dt.timedelta(milliseconds=d),-1))
ev.sort(); cur=mx=0
for _,v in ev: cur+=v; mx=max(mx,cur)
print('peak_concurrency',mx)
PY
```
