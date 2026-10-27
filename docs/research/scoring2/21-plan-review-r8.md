# R8 对抗复审 · 节点系统 v5.1

> 复审对象：`docs/superpowers/plans/2026-10-27-node-system-v5.md` v5.1（HEAD `3491aad`）
> 复审方式：只读代码/日志，不写实现、不 commit、不启动服务。
> 数据基线：`/tmp/c1027.jsonl`（10-27 00:00–09:00）、`/tmp/cl-1026b.jsonl`、`/tmp/cl-1027b.jsonl`；代码基线 HEAD `3491aad`（v5.1 只改文档，代码仍是 `456256f` 之后的工作区状态）。

## 0. 结论（先给）

**仍需修订（NO-GO），v5.1 不能进入 N1 实现。**

v5.1 确实吸收了 R7 的大部分文字：attempt 计数、精确 60s、AND 触发、K=8、canary 单次失败升档、`strikes=0 ⇒ until=zero`、禁直连回退、5m 迁移都写进去了。但**规格没有收敛**：v5.1 把 R7 的 5 个 Critical 中 3 个改成“只写了结论、没有写可执行状态/接口”，并在 24h 预算、per-egress 在途、legacy 回滚三处引入了新的硬矛盾。最硬的一条是：**按 pair 的 24h 8 次预算会把 `4ad1982e` 直接封 24h，和计划自己“4ad 不被误封”的验收相反**；这不是实现细节，是设计自相矛盾。

| 编号 | 严重度 | Blocker | 最短证据 |
|---|---|---|---|
| R8-B1 | Critical | 24h 硬预算按 **pair** 而不是 **egress**；`4ad1982e` 20 次 429 会触发 24h hard block，违反“有 2xx 不误封”；多模型在同一出口可把 8 次/24h 放大成 `8×模型数` | v5 §2.3 第 54 行、§6 第 173 行；`4ad` 20 个 429 全为 mimo；§8 第 193 行又要求“每出口 24h 429 ≤8” |
| R8-B2 | Critical | K=8 的作用域未定义：正文说“每出口”，配置叫 `pair_inflight_max=4`，状态里没有 egress `inflight`；N=1/C=32 时 500ms 排队与“直接 fail-fast”冲突 | v5 §3.2 第 115 行、§5 第 159 行、§6 第 173 行；00:24 N=1、222 请求/222 个 429 |
| R8-B3 | Critical | canary 没有 normative 分支：§2.3 说 cooling→canary，§3.2 的候选只来自 Admit 资格层；状态无 `canaryInFlight/canaryOK/lastCanaryOK`；190 个 5h 出口可能永远不被 canary | v5 §2.3 第 51–53 行、§3.2 第 106 行、§11 第 223 行；R7-B3 原缺陷未真正关闭 |
| R8-B4 | Critical | `node_state_v5.json` 无法表达“精确 60s 时间戳队列”“24h 滚动 bad24/resets24”“canary 单飞”；`window/bad24/resets24` 都是 int；重启后 60s 窗口丢失 | v5 §2.3 第 48 行、§6 第 173 行；状态字段清单没有 `canaryOK/canaryInFlight/lastDecay`，也没有 egress `inflight` |
| R8-B5 | Critical | `legacy` 回滚与删除清单互斥：§2.4 删除 `shape_drift`/`quota_halt`/`pool403`，§7 又要求 `NODE_SYSTEM=legacy` 回到“shape_drift + 旧选路”；没有 off 矩阵、没有旧状态映射、没有 5 分钟回滚测试 | v5 §2.4 第 81–86 行、§7 第 185 行；`main.go:3290/3786` 的 Halt 分支仍存在 |
| R8-B6 | High | 选择层不可按当前 `Admit(model, now)` 实现：没有 session/exclude/attempt/direct 入参，sticky、同 sub//24 排除、重试都无处落；UCB 的 `N`、衰减后负 `n`、`ln(0)` 未定义；`+84.7pp` 是全天事后固定最优，不是 UCB 可实现的时点收益 | v5 §2.2 第 34 行、§3.1–3.2 第 94–113 行；`.research/scoring2/03-empirical.md:347` 明确写“不是时点可实现” |
| R8-B7 | High | 全局探测会绕过 sub quarantine / lifecycle dead，且没有 `globalProbeInFlight`、没有 `lastHit` tie-break；N=212 时“单令牌 60s”没有恢复上界 | v5 §2.3 第 61 行、§5 第 160 行；`sub`/`lifecycle` 状态机与探测没有交叉条件 |
| R8-B8 | High | 状态码/影子模式/403 能力未闭合：`Reject{reason}` 没有到 503/429 的映射；shadow 只记录决策却要求 `Observe` 更新后验；删 `pool403` 后 403 风暴的池级遏制没有等价验收 | v5 §2.2、§4.1、§7；`quota_halt.go` 当前是 503+`Retry-After: 30`，v5 没有对应规则 |

一句话：**方向比 v4.1 更收敛，但 v5.1 仍是“文字收敛、状态未收敛”；上述 5 个 Critical 只要有一个按字面实现，事故场景就会重现 00:30 的 1,106 连发或 4ad 的 24h 误封。**

## 1. 方法与数据边界

### 1.1 输入

| 文件 | 用途 |
|---|---|
| `docs/superpowers/plans/2026-10-27-node-system-v5.md` | 227 行，v5.1；本报告引用行号均指此文件 |
| `docs/research/scoring2/20-plan-review-r7.md` | R7 的 5 Critical + 2 High 与 v4.1 证据 |
| `docs/research/scoring2/19-plan-review-r6.md` | R6 的 7 问与 4 个 patch |
| `docs/research/scoring2/18-plan-review-r5.md` | 历史 8 个 blocker |
| `.research/scoring2/03-empirical.md` | 选择层收益的真实口径（oracle 是全天事后最优） |

### 1.2 本次独立复算的数据

| 窗口/对象 | 数字 |
|---|---|
| 10-27 00:15–00:45 | 1,847 请求；321 ok、1,526 fail；**1,529** 次上游 429；仅 **3** 个 switch、**1** 个 shape_drift；6 个 distinct egress；峰值并发 32 |
| `653e51eb663a96f046b559cfe37ef734` | 00:30:27.338–00:41:29.083（11 分钟）**1,106** 次 429；1,106 个不同请求；全部 `mimo-v2.6-flash-free`；`nodes` 长度全为 1 |
| `41d39652cad7593b2a8320299ef4ae62` | 00:20:59.825–00:25:09.375 **412** 次 429；402 mimo + 10 deepseek；412 个不同请求 |
| 00:24 分钟 | 222 个失败请求；`nodes` 长度全为 1；只有 `upstream_error`；**0 个 switch**；222 个 429；N=1 |
| 18:03 `4ad1982e75f81f0427b298af7b078754` | 20 个 429 尝试在 18:03:55.108–18:03:55.937（0.829s）；**首次 429 前 60s 内 35 个 2xx、36 个请求**；20 个 429 全为 mimo；全量 219 个 2xx（多数在 429 之前） |
| 4ad 的 60s 占比 | `20/(35+20)=36.4%`，不是计划写的 8.4%；8.4% 是 `20/(219+20)`，把全量 2xx 都算进了一个 60s 窗口 |

> 数据边界：`/tmp/c1027.jsonl` 覆盖 00:00–09:00，不是全天生产库；`/tmp/cl-1026b.jsonl` 13,724 行、`/tmp/cl-1027b.jsonl` 541 行是 18:03/00:20 的历史副本。本文不把 212 当生产事实（212 是计划数字；本次窗口只出现 6 个 egress）。`403` 在 00:15–00:45 为 0 次，不能用本窗口证明 403 策略好坏。

### 1.3 代码事实

- `main.go:2964/3484` 先取 `shapeDriftBlockedEgress`，`main.go:2984/3502` 再检查 shape blocked；`main.go:3270/3766` 调 `shapeDriftObserve429`；`main.go:3290/3786` 的 `drift.Halt` 分支只 `callLogSuppressedAttempt`、设置 `lastBody/lastStatus`、`break`，**不 mark、不 switch**，下一条请求仍会选回同一出口。
- `main.go:2878/3397` 是 `quotaHaltGuard.gate`；`main.go:2946/2967/3147/3466/3487/3645` 是 `poolHalted`；`main.go:3329–3344/3827–3828` 是 direct 429 → `quota_halt.halt`。
- `egress.go:126–143`：付费层 `paidDirect` 直接返回 direct；节点池活跃但 `pickForRegionExcluding` 返回 nil 时，**回退 `Direct: true`**；`EgressKey()` 把 `Direct` 或 `NodeFP==""` 都映射成 `direct`，静态 socks5 也落在这个桶（`egress.go:47–61`）。
- `quota_halt.go`：`haltRecoveryStreak=2`、`haltRecoveryInterval=60s`、`haltRetryAfterSeconds=30`；`gate` 需要 `!hasEligibleNode`；只有 direct 429 调 `halt`。
- `proxy_node.go:264–287`：`sub403` 当前 3 次/10m、10/20/30m 退避；`pool403` 是 8 个 distinct 403/10m → 10m 全局 halt；`pool403Min=8`、`pool403Max=24`。
- `proxy_node.go:1996` `proxyStateVersion=1`；`proxy_node.go:209–220` 的 `nodeRuntimeState` 只有 `cooldown_until/probation_*`，没有 v5 的 pair/egress 字段。

## 2. R7 的 5 Critical + 2 High：逐条关闭核对

### 2.1 总表

| R7 条目 | v5.1 处置（行号） | 裁决 | 关键缺口 |
|---|---|---|---|
| Critical-1 计数/窗口/AND/canary 单次升档 | §2.3 第 48–58 行；§11 第 221 行 | **部分关闭** | attempt、60s、AND、canary 单次升档都写了；但状态存不下 60s 队列，`4ad` 的 60s 数字写错，24h 预算把 4ad 另行封死 |
| Critical-2 K=8 + 500ms 排队 | §3.2 第 115 行；§5 第 159 行；§8 第 193 行 | **未关闭** | 正文 K=8、配置 K=4；作用域“每出口”但状态/Lease 是 pair；N=1 的 222 请求没有验收；排队后不重查 pair |
| Critical-3 canary 唯一裁决/190 个 5h | §2.3 第 51–53 行；§11 第 223 行 | **未关闭** | 没有伪代码、没有 `canaryInFlight` 字段、没有“normal 非空仍 must canary”的测试；190 个迁移只有 5m 映射 |
| Critical-4 探测退避/预算冲突 | §2.3 第 61 行；§11 第 224 行 | **部分关闭** | 退避写了、探测 429 计入预算写了；但没有单飞、没有 `lastHit`、没有 N=3/22/212 恢复表，预算仍是 pair 级 |
| Critical-5 `strikes=0`/直连回退 | §2.3 第 59 行；§2.2 第 41 行；§11 第 225 行 | **部分关闭** | `strikes=0 ⇒ until=zero` 写了；`egress.Get` 禁回退写了；但 direct 作为候选的资格（paidDirect vs 全冷却 fallback vs 静态 socks5）没有规则，`legacy` 又要恢复回退 |
| High-1 租约 | §2.2 第 44 行；§11 第 226 行 | **部分关闭** | TTL 10min、`sync.Once`、`abandoned` 写了；没有 sweep 间隔、没有长流续租、`abandoned` 不是 outcome class、没有租约注册表 |
| High-2 190 个 5h 迁移 | §6 第 178 行；§11 第 227 行 | **部分关闭** | `NodeExhausted 且 quota → strikes=1, until=+5m` 写了；但非 quota 的 NodeExhausted/probation/dead 没有映射；canary 分支缺失使 5m 后仍可能永远不服务 |

**总裁决：7 条里 0 条完全关闭；C2/C3 仍未关闭，C4 只关闭了退避，预算仍按 pair。**

### 2.2 Critical-1：数字与状态

v5.1 把“attempt”写死了，这是进步；但：

1. **`4ad` 的 60s 占比写错**：§2.3 第 57 行说“20 次 429 但同窗 219 次 2xx，占比 8.4%”。实际首次 429 前 60s 只有 35 个 2xx（30 mimo + 5 deepseek）、36 个请求；20 个 429 都在 18:03:55.108–.937。按 attempt 是 `20/(35+20)=36.4%`；按 request 是 20 个不同请求中的 20 个 429（这 20 个请求都失败/换出口），也不是 8.4%。219 是 18:03:55 之前的全量成功，不是 60s 窗口。
2. **`4ad` 仍会被 24h 预算封死**：§2.3 第 54 行“任意 24h 内 8 次 429 → hard_blocked(24h)”，20 个 429 全在 0.829s，按字面第 8 个就 24h；§2.3 第 57 行又说 4ad 型“不熔断”，§8 第 193 行要求“18:03 有 2xx 的出口不被误封”。三者不能同时成立。
3. **精确 60s 队列没有状态**：§6 第 173 行只有 `"window":0`（int），没有 `[]int64` 时间戳；重启/序列化后无法恢复 60s 窗口。`bad24`、`resets24` 同样是 int，无法做滚动 24h 或每日重置。
4. **`pair_trigger_ratio` 的“≥4 次尝试”与 `nAtt>=3` 冲突**：§2.3 第 51 行是 `nAtt>=3`，§5 第 153 行写“≥4 次尝试时的占比阈值”。实现者会得到两种触发边界（3 次 429/3 次尝试 vs 3 次 429/4 次尝试）。

结论：**B1 的语义只写对了一半，数字和预算分支仍会改变事故行为。**

### 2.3 Critical-2：K=8 的作用域

v5.1 §3.2 第 115 行：“每出口 `inflight < K=8`；全部候选都在途时最多等 500ms 抢一个名额，超时才 503 `all_inflight`。”但：

- §5 第 159 行配置键是 `pair_inflight_max`，默认 **4**；不是 egress，也不是 8。
- §6 第 173–175 行的 `pairs` 没有 `inflight`，`egress` 只有 `lifecycle/consecFail/deadUntil/lastProbe`，也没有 `inflight`。`Lease{model, egress, kind, token}` 是 pair 级；实现者无法从状态判断“这个出口已有几个在途”。
- 若按 pair 计数，N=1 且 6 个模型时容量是 `6×8=48`（或配置 4 时 24），不是 8；§3.2 的 `N=4/C=31` 容量 32 的算术只对“每出口”成立。
- 若按 egress 计数，必须在 `Lease` 中加 egress 级 token/计数，并定义 acquire/release/sweep；v5 没有。
- N=1/C=32 的 00:24 场景：按字面 8 个在途 + 214 个排队 500ms；计划没有规定排队者在 pair 已被 breaker 置 `until` 后**重新检查资格**。如果只等名额、不重查 pair，排队者仍会拿到旧 pair 的 lease，继续发 429；这正是 00:24 的 222 连发。
- “N=1 直接 fail-fast”没有写；计划只说“全部候选在途时等 500ms”，没有“候选数=1 时不要排队”的特例。lead 要求的上限“候选数×K”只隐含在容量里，不是显式规则。

结论：**K=8 只改了数字，没有定义作用域、计数器和排队后的资格重检；C2 未关闭。**

### 2.4 Critical-3：canary 分支

v5.1 §2.3 第 51–53 行只说：

```text
ok --(3/60s AND ratio>=0.5)--> cooling(5m) --到期--> canary
canary --2 次 2xx(>=60s)--> ok
canary --任意 429--> strikes+1 并升档
```

但 §3.2 第 106 行说“候选 = Admit 资格层放行的 pair”，没有说 canary 是独立分支；§2.2 的 `Admit` 签名只有 `Admit(model, now)`，没有 `canaryInFlight`/`strikes` 的选择规则。具体缺口：

1. 如果 normal 候选非空，canary pair 是否仍必须被选中？v5 §11 第 223 行声称“伪代码改为互斥分支 + 显式 return”，但 v5 正文没有这段伪代码；只有一句对照表，不能作为施工图。
2. `node_state_v5.json` 没有 `canaryOK`、`lastCanaryOK`、`canaryInFlight`；§9 第 203 行却要求“100 并发同一 pair 只发 1 个 canary”。没有字段/锁，这个测试无法实现。
3. 190 个 5h 出口迁移为 `strikes=1, until=now+5m` 后，5m 到期需要 canary；如果 normal 有 22 个候选且实现是 normal-first，190 个永远不 canary。R7-B3 的原始缺陷原样保留。
4. `resets24h` 超限后的“strikes 每日降 1 级”没有定义 `canaryOK` 何时清零、`lastCanaryOK` 何时更新；canary 先成功后失败再成功可能用两次非连续成功满足“2 次 2xx”。

结论：**C3 未关闭，且是 190 个出口恢复路径的单点。**

### 2.5 Critical-4：全局探测

v5.1 加了退避（60s→2m→5m→15m→1h→6h），并把探测 429 计入预算；这解决了 R7-B4 的“每 60s 烧 480/IP/日”的一半。但仍缺：

- **单飞**：§2.3 第 61 行“每次只发 1 个”没有 `globalProbeInFlight` 字段/锁；并发 `Admit` 可以同时看到 `candidate_n==0`，各发一个探测。
- **轮转**：选 `until` 最早的出口；同一出口多个 pair 的 `until` 相同，没有 `lastHit/lastProbe` tie-break，可能反复选同一个。
- **恢复上界**：N=212 时一次只探一个，60s 间隔轮完要 212 分钟；计划只说“前两次 ≤2min，之后不承诺”，但没有 N=3/22/212 的恢复表，也没有验收阈值。
- **预算作用域**：探测 429 计入 pair 的 `bad24`，不是 egress；同一出口多个 pair 可以各自消耗 8 次探测预算。
- **与 sub/lifecycle 的交叉**：`no_candidate` 可能是“全部 sub quarantine”或“全部 dead”；计划没有区分原因，探测可能去打一个 10m 内本应隔离的 sub 或 30m 内 dead 的出口，违反 sub/lifecycle 状态机。

结论：**C4 只关闭了退避的一半；单飞、轮转、恢复上界、作用域都未关闭。**

### 2.6 Critical-5：strikes=0 与 direct

`strikes=0 ⇒ until=zero` 写对了；但 direct 的语义没有写清：

- `egress.go:126–131` 付费层 `paidDirect` 是**主动**直连；`egress.go:143` 是节点全冷却时的**回退**直连；`egress.go:159` 是静态 socks5 无代理时的回退。三者都被 `EgressKey()` 归为 `direct`。
- v5 §2.2 第 41 行说“`egress.go` 不再自行回退直连（全冷却时返回 NoCandidate，不是 direct）”，但没有说：付费直连是否仍是合法候选？静态 socks5 是否仍走 `direct` 桶？direct 是否有自己的 pair 状态、24h 预算、lifecycle？§6 的 `egress` 只有 fp，没有 `direct` 条目。
- 若 `Admit` 把 direct 作为候选，全节点 blocked 时仍可能选 direct，等于保留回退；若 `Admit` 排除 direct，付费直连和静态 socks5 用户会被误伤。两条路必须显式二选一，v5 没有。
- `legacy` 又要恢复 `egress.Get` 的回退（§7 第 185 行），与 §2.2 的“禁止回退”冲突。

结论：**C5 只关闭了 `until` 清零；direct 资格与 legacy 回退仍矛盾。**

### 2.7 High-1/High-2

- **租约**：TTL 10min、`sync.Once`、跨出口先 `Observe(abandoned)` 只是 prose；没有 sweep 间隔（R7 要求 ≤TTL/2）、没有长流续租、没有 `abandoned` outcome class、没有租约注册表/代际。`sync.Once` 放在被复制的 `Lease` 值上会失效；实现者需要指针/ID。
- **190 迁移**：`strikes=1, until=+5m` 只覆盖“旧 NodeExhausted 且 quota”；`proxy_node.go` 还有 `NodeProbation/NodeDead/NodeRejected` 和非 quota 的 `NodeExhausted`，v5 没有映射表；`proxy_state.json` 保留 v1 不动，意味着旧状态与新 `node_state_v5` 两套真源，重启/回滚时可能各读一套。

## 3. v5.1 整体自洽性：pair / egress / sub / selection / admission

### 3.1 交叉矩阵

| # | pair | egress/sub/lifecycle | v5.1 字面行为 | 问题 |
|---:|---|---|---|---|
| 1 | 某 pair normal | egress active、sub ok | 选该 pair | 正常 |
| 2 | pair blocked | 同 egress 另一 pair normal | 选另一 pair | per-pair 隔离成立；但 24h 预算/在途仍是 per-pair，egress 总发送无上限 |
| 3 | 全部 pair blocked | egress active | 全局探测 1 个 | 没有单飞；并发可多个；探测哪个 pair 未定义 |
| 4 | pair normal | sub quarantine | 该 sub 的 egress 不进候选 | 若其他 sub 可用，正常；若全 quarantine → `no_candidate` |
| 5 | pair blocked/canary | 全部 sub quarantine | `no_candidate` | 全局探测是否会打 quarantine sub？未定义 |
| 6 | pair normal | lifecycle dead | dead 不进候选 | dead 到期→probation；probation 是否候选未定义 |
| 7 | 全部 pair blocked | 全部 dead | `no_candidate` | 全局探测是否会打 dead？未定义；可能违反 30m dead |
| 8 | 全部候选在途 | N=1、C=32 | 8 在途 + 214 排队 500ms → 503 | 排队者是否重查 pair？未定义；N=1 未 fail-fast |
| 9 | direct paid | 全部节点 blocked | direct 是主动候选还是回退？ | 未定义；可能绕过 no_candidate/预算 |
| 10 | static socks5 | 无节点 | `EgressKey=direct` | 与真直连共桶；pair 预算/403 归因混在一起 |
| 11 | 4ad 20×429 | egress 有 219 个 2xx | 3/60s AND 不触发 5m；但 24h 8 次 hard block | **一处放行、另一处拒绝**：AND 放行，24h 预算封 24h |
| 12 | canary pair | normal 候选非空 | 未定义 canary 优先级 | 可能永远不 canary（190 个 5h） |
| 13 | pair blocked | `all_inflight` | 503 `all_inflight` | 状态码/`Retry-After` 未定义；`no_candidate` 与 `all_inflight` 的区分只在事件名 |
| 14 | 24h 预算耗尽 | 同 egress 另一 pair 健康 | 该 pair 24h blocked，另一 pair 可发 | egress 总 429 可超 8；与“每出口 ≤8”冲突 |
| 15 | 全部 pair blocked | 全局探测 429 | pair strikes+1、预算+1 | 探测 429 计入 pair 而不是 egress；同 egress 多 pair 可绕过 |

### 3.2 “一处放行、另一处拒绝”的具体行

1. **4ad**：§2.3 第 51 行 AND 放行（36.4% < 50%），第 54 行 24h 预算拒绝（8 次即封）。这是最直接的双层冲突。
2. **direct**：§2.2 第 41 行“全冷却返回 NoCandidate”放行/拒绝？`Egress` 实体第 25 行把 direct 列为出口，但 §6 的 egress 状态没有 direct；`Admit` 是否把 direct 放进候选没有规则。
3. **sub vs probe**：§2.3 第 74–76 行 sub quarantine 期间“不进入候选”，但全局探测没有排除 quarantine；如果 `no_candidate` 由全 sub quarantine 触发，探测会绕过 10m 隔离。
4. **dead vs probe**：同上；dead(30m) 不是候选，但探测选 `until` 最早，dead 没有 `until`，行为未定义。
5. **per-pair vs per-egress**：§2.3 第 54 行预算、§3.2 第 115 行在途、§8 第 193 行验收，三者作用域不同；同一个出口的健康 pair 会继续发，egress 总 429 无上限。
6. **status**：`Reject{reason}` 没有到 503/429 的映射；当前 `quota_halt` 是 503+`Retry-After: 30`，`all_inflight` 计划写 503+1s，`no_candidate` 没写。客户端会看到至少三种 503 语义，且上游 429 是否透传未定义（§2.2 只说 `Reject`，§4 事件没有状态码）。

### 3.3 状态互相覆盖

- `Observe` 同时更新 pair 后验、breaker、quota、sub、call_log、metrics；v5 没有规定更新顺序和失败原子性。若 pair 2xx 清 `strikes`，但同一次 `Observe` 的 403 触发 sub quarantine，谁赢？没有优先级。
- `sub` quarantine 到期“半开”没有定义放行哪个 pair、几次、成功如何清；可能一个 403 立即再次 quarantine，形成 10m 振荡。
- `lifecycle` dead 与 pair quota 独立；一个 egress dead 后，pair 仍可能有 `until`；dead 到期→probation，pair 的 canary 状态如何衔接？没有。
- `node_state_v5` 与 `proxy_state` 两套状态：v5 的 egress lifecycle 与旧 `NodeExhausted/Probation` 同时存在；谁优先没有规定。

## 4. 删除清单是否安全：shape_drift / pool403 / quota_halt

### 4.1 `shape_drift` 全局闩

**删除方向正确，但 v5 没有把“停止继续选中同一出口”写成验收。**

当前事故根因不是“闩不够快”，而是 `Halt` 分支 `break` 后既不 mark 也不 switch（`main.go:3290–3300/3786–3796`），下一条请求又选回同一出口。v5 用 pair breaker + no_candidate 取代，理论上正确；但：

- §2.3 的 pair breaker 是 `(model, egress)`；00:24 的 222 个请求全是同一出口，若同一模型，第 3 个 429 后 pair 应 `until`；但计划没有写“排队中的请求在 pair 变化后必须重新 Admit”，所以 8 个在途 + 214 个排队仍可能继续发。
- 计划 N2 验收只有“00:20 单出口第 3 次熔断”（§8 第 193 行），没有“653e 的 1,106 连发在 K 次内停止”“00:24 222 个请求不再产生 222 个 429”。
- 建议验收：`sends_after_pair_trip == 0`（除已发出、尚未返回的 ≤K 个）；`00:24` 上游发送 ≤K（N=1、K=8 时 ≤8），其余全部 503 `no_candidate`/`all_inflight`，且每条有 suppress 事件。

#### 4.1.1 lead 三条的覆盖裁决

| lead 要求 | v5.1 是否覆盖 | 裁决/应写的验收 |
|---|---|---|
| ① 门在拦切换时，必须同时阻止继续选中同一出口 | **部分覆盖**：pair breaker 置 `until` 后，`Admit` 的资格层应排除该 pair；但计划没写“排队者在醒来后重查 pair”，也没写 `Admit` 返回的 `EgressKey` 不能等于刚失败的 egress | 不能直接引用为“已覆盖”；需加 `TestR8PairBreaker_0024`：第 3 个 429 后所有 queued 请求的 `Admit` 都不得返回该 egress；上游发送 ≤K |
| ② 429 后无论是否处于 pool 级状态都要 mark pair 并换 pair，无候选则 503 | **部分覆盖**：§2.3 说 `Observe` 更新 pair，§3.2 说 no candidate → 503；但 `Observe` 的 pool 级分支/`Halt` 旧分支没有删除顺序，`no_candidate` 状态码未定义，`legacy` 仍保留旧 Halt | 需加 `TestR8Observe429AlwaysMarksPair`：在 pool403/sub quarantine/global probe 三种上下文下各注入一个 429，断言 pair `strikes>0/until>0`，且下一次 `Admit` 不返回该 pair；无候选返回 503 |
| ③ K=8 排队在 N=1 时无意义；上限应为候选数×K，N=1 直接 fail-fast | **未覆盖**：正文 K=8，配置 `pair_inflight_max=4`；作用域未写；N=1 仍允许 8 在途+214 排队 500ms | 需加 `TestR8InflightScope`：N=1/C=32 时并发上游 ≤K，且 queued 请求在 pair 变化后不拿旧 lease；N=1 要么直接 503，要么明确接受 ≤K 个在途 |

**结论：lead 三条没有一条可以直接作为“已覆盖”引用；三条都必须转成上述断言，否则 00:24 的 222 连发仍会复现。**

### 4.2 `pool403` → “sub 全 quarantine ⇒ no_candidate”

**有真实能力损失，且没有等价验收。**

- 当前 `pool403` 是 8 个 distinct 403/10m → 10m 全局 halt（`proxy_node.go:279–287/895`）；v5 改成 sub quarantine（≥2 个不同 pair 或同 pair 2 次），阈值和粒度都变了。
- 如果 403 风暴来自多个 sub、每个 sub 只有 1 个 pair，v5 不会触发任何 quarantine；`pool403` 的“池级 403 风暴”能力消失。10-24 的 403 风暴正是池级现象（R6 引用 `02-constraints.md` 的 node ICC 0.7847）；本次 00:15–00:45 没有 403，不能用本窗口证明不会回归。
- `sub` 当前有 10/20/30m 退避；v5 只有 10m 固定，且“到期→半开”没有成功/失败路径，可能每 10m 重新放行一次。
- 计划说“保留 403 归因”，但 `Observe` 的 403 只记 sub，不记 pair；`suppress` 的 `sub_quarantine` 没有 `candidate_n`/`would_be_egress` 的具体规则。

### 4.3 `quota_halt` → pair

**direct 的语义没有闭合。**

- 当前 `quota_halt` 是 per-egress、2 次成功≥60s 恢复、503+30s；v5 把它并入 pair 的 canary（2 次 2xx≥60s），语义接近。
- 但 `quota_halt` 只对 direct 429 触发；v5 的 direct 没有 `egress` 状态（§6 的 `egress` 键是 fp），也没有 `direct` 的 24h 预算/生命周期。`pair` 可以有 `model|direct`，但 `Admit` 何时选 direct、direct 的 `Egress` 属性是什么，没有写。
- 若删除 `quota_halt` 但不删除 `egress.Get` 的回退，direct 429 会重新走 pair，可能被 24h 预算封；若删除回退，付费直连/静态 socks5 的行为改变。两条都需要测试。

### 4.4 8 条不变量

| 不变量 | v5.1 状态 | 说明 |
|---|---|---|
| I1 不钉在已知 429 出口 | ⚠️ 部分 | pair breaker 主路径可以；但全局探测每轮选同一 pair、direct 候选未定义、排队不重查 |
| I2 抑制留痕 | ⚠️ 部分 | `suppress` 有 `candidate_n`，但 `no_candidate` 的 `would_be_egress` 为空；没有状态码/`Retry-After` 字段 |
| I3 恢复必须有证据 | ❌ 未实现 | canary 分支缺失；190 个 5h 可能永远不 canary |
| I4 mark 幂等 | ⚠️ 部分 | 旧 `markLocked` 有幂等，但 v5 迁移/旧 probation 的 2xx 路径没有映射 |
| I5 无全局闩 | ✅ 方向正确 | 删 shape_drift；但全局探测可能变成新的单点，需单飞/原因区分 |
| I6 fail-fast | ⚠️ 部分 | `all_inflight` 有；N=1 排队 500ms 与 fail-fast 冲突；`no_candidate` 状态码未定义 |
| I7 429 不进质量 | ✅ | `node_scoring.go:947` 已核实；v5 保留 |
| I8 未尝试 ≠ 失败 | ⚠️ 部分 | `suppress` 与 attempt 分开；direct 回退/排队可能把未尝试变成尝试 |

## 5. 选择层：后验 + UCB 是否与证据一致

### 5.1 公式本身

v5 §3.1–3.2：

```text
a0=8, b0=2; 2xx: a+=1; 429/FUL: b+=1
p=(a+0.5)/(a+b+1)
n=a+b-a0-b0
n<5: cold start（尝试次数最少；同一出口 1 个，复用 canary 门）
n>=5: p + 0.05*sqrt(2*ln(N)/n)
```

问题：

1. **`N` 未定义**：是全局 attempt 数、pair 数、还是窗口内 attempt？`ln(N)` 在 N=0/1 时退化；没有公式。
2. **衰减后 `n` 可能为负**：§3.1 说 `a/b` 按 `exp(-Δt/7d)` 衰减，但没有 `lastDecay` 字段，也没有 clamp。7 天后 `a+b` 可低于 10，`n<0`，`sqrt` 负数 → NaN；排序不稳定。
3. **冷启动会烧 IP**：212 个 egress × 5 次 = 1,060 次冷启动发送；若每个 pair 3 次 429 就 breaker，仍有 636 次；`pair_bad_24h=8` 是 per pair，不是 per egress。计划说“UCB 探索被 3/60s 和 8 次/24h 兜底”（§10 第 212 行），但 8 次是 pair 级，不能兜住 egress 级。
4. **`Admit(model, now)` 缺少输入**：sticky 需要 session key（当前 `EgressRequest.BodyMap`/`stickyKeyForRequest`），同 sub//24 排除需要候选的 sub/IP，重试需要 attempt 序号，direct 需要 tier/paidDirect。v5 的签名无法实现 §3.2 的 sticky/排除。
5. **canary 门与冷启动冲突**：`n<5` 说“复用 canary 门”，但 canary 是 `strikes>0` 的 blocked pair；冷启动是 `strikes=0` 的新 pair。两者不是同一状态，复用会错误地把新 pair 当 canary 单飞，或把 blocked pair 当冷启动放行。

### 5.2 证据

`.research/scoring2/03-empirical.md` 的原始口径：

| 窗口 | 当前 | oracle 固定最优 | 时点 causal | Top-10 等分 | Top-10 顺序 5 次 |
|---|---:|---:|---:|---:|---:|
| 10-24（403 风暴） | 3.39% | 88.14%（+84.74pp） | 7.22%（+3.83pp） | 10.06%（+6.67pp） | 12.50%（+9.11pp） |
| 10-26（稳态） | 99.62% | 99.50%（−0.12pp） | 99.52%（−0.10pp） | 97.01%（−2.61pp） | 100.00%（+0.38pp） |

原文明确写：**“+84.7pp 是‘全天后见之明’的固定最优，不是时点可实现的”**（`03-empirical.md:347`）。v5 §3.3 第 120 行说“oracle +84.7pp 的上界要求持续探索”，把“事后固定最优”当成“UCB 可逼近的探索收益”，这是口径错误：

- UCB 在 10-24 的时点信息下最多对应 +2~4pp 的 causal oracle；Top-10 等分 +6.67pp、顺序 5 次 +9.11pp 是训练窗排序的上限，不是 UCB 的保证。
- 稳态 10-26 的 Top-10 是 −2.61pp；v5 §8 的 N3 验收“稳态不低于基线 0.5pp、风暴日 ≥+5pp”没有给出基线定义、尝试次数、窗口和回放方法。以现有 14,265 条日志，无法验证 24h 的 UCB 参数。
- 计划 §10 第 215 行说“T7 研究未完成，N3 先按本方案实现，T7 完成后校准参数”；这等于承认 N3 的收益假设未验证，却把 N3 放在 N2 之后、上线之前。至少应把 N3 降为实验开关，不应作为强制路径。

结论：**选择层不是“有证据支持的优化”，而是“用未验证的 UCB 替换已验证的静态分”。可以研究，不能作为 v5.1 的 N3 强制验收。**

## 6. 切片可执行性：N1 → N5、影子、legacy 回滚

### 6.1 依赖表

| 切片 | 计划内容 | 依赖是否真实 | 问题 |
|---|---|---|---|
| N1 数据层 | pair/egress/sub 状态、meter、clock、`node_state_v5.json`、迁移、影子记录 | 真实；N2/N3 都需要 | 状态 schema 不足以表达 60s/24h/canary；N1 验收“10 万 model”与 5 万 pair 上限冲突 |
| N2 资格层 | Admit/Observe、breaker、lifecycle、sub quarantine、no_candidate、all_inflight | 依赖 N1；真实 | N2 验收写 `N=22/K=4`，与 §3.2 K=8 冲突；00:24 N=1 未覆盖；canary 分支缺失 |
| N3 选择层 | 后验、UCB、冷启动、sticky、同 sub//24 排除 | 依赖 N2；真实 | `Admit` 缺 session/exclude；UCB 收益未验证；N3 验收无法用现有日志回放 |
| N4 可观测 | 事件、指标、面板 | 可与 N2/N3 并行；真实 | 事件 schema 缺状态码；`suppress` 的 `no_candidate` 无 `would_be_egress` |
| N5 切换 | 影子 24h → 强制 → 回滚演练 | 最后；真实 | shadow 如何 Observe 未定义；legacy 与删除清单冲突；5 分钟回滚无测试 |

### 6.2 影子模式

计划 §7 第 183 行：“v5 只做决策记录，不改变实际发送；对比 v5 会选谁与实际选谁。” 但：

- 实际发送仍走 HEAD 的 `egress.Get`，v5 的 `Admit` 不产生 lease；`Observe` 如何知道实际 egress/outcome？如果 shadow 只记录 `Admit`，没有真实 outcome，后验永远是先验，分歧率没有意义。
- 如果 shadow 也调 `Observe` 更新 pair 后验，它就会影响后续 `Admit`（即使不改变发送），shadow 不再是只读；计划没有隔离 shadow 状态。
- “预测成功率/Brier”没有定义预测对象（pair 2xx？请求最终成功？），也没有事件字段。
- 建议：shadow 使用独立的内存 state 副本，`Observe` 只写副本；记录 `shadow_admit{chosen, actual, candidate_n}` 与 `shadow_outcome`；不得触碰生产 pair。

### 6.3 legacy 回滚

计划 §7 第 185 行：“`NODE_SYSTEM=legacy`：回到 HEAD 行为（shape_drift + 旧选路），不绕过 pool403/quota。” 这与 §2.4 的删除清单直接冲突：

- §2.4 删除 `shape_drift` 全局闩、`quota_halt`、`pool403` 全局 halt；如果代码真的删除，legacy 无法回到 HEAD；如果保留旧代码，§2.4 的“删除”只是 v5 路径不调用，必须写明保留文件/构建标签。
- legacy 需要 `egress.Get` 的 direct 回退；§2.2 要求 v5 路径禁止回退。必须有两个路径或显式 `AllowDirectFallback`，计划没有。
- `NODE_SYSTEM` 的 off 矩阵没有：`legacy × pool403 × quotaHalt × direct429` 四象限、`v5-shadow × 真实发送`、`v5 × 旧 state` 都没有测试。
- `proxy_state.json` v1 不动，但 v5 启动时会迁移/读取 `node_state_v5`；如果迁移写入了 `node_state_v5`，legacy 回滚后旧代码不读它，旧的 5h mark 仍在 `proxy_state`，可能出现“回滚后旧闩复活”或“新 state 残留”。需要迁移幂等/回滚清理。

### 6.4 内存与上限

§6 第 177 行：pair 最多 50k（512 model × 212 egress 的活跃子集），空闲 30min 清理，不淘汰 armed/blocked。问题：

- 512×212 = 108,544 > 50k；如果 512 个模型都活跃，50k 上限必须淘汰，计划说不淘汰 armed/blocked，可能超过 50k。
- 如果 100k 个模型（N1 验收）各一个 pair，50k 上限会淘汰一半；N1 的“10 万 model 内存”测试与 5 万上限矛盾。
- 不淘汰 blocked 意味着攻击者可以用大量 `(model,egress)` 429 把 map 撑到内存上限；当前 `shape_drift` 有 512 model 上限，v5 没有 model 名长度/数量上限。
- `window` 若是时间戳队列，每个 pair 最多 60s 内的 attempt；50k × 队列长度，内存上界没有算。

## 7. 验收可自动化：逐条断言

| 计划/lead 断言 | 可自动化？ | 需要的断言/问题 |
|---|---|---|
| 00:20 单出口第 3 次熔断 | ✅（固定夹具） | 用 `41d` 前 3 个 429 时间戳；断言第 3 个后 pair `until>0`、后续请求 `no_candidate`。但仅测“第 3 次”不够，需加 `653e` 1,106 与 00:24 222。 |
| 18:03 有 2xx 的出口不被误封 | ❌ 按 v5.1 会失败 | 4ad 20 个 429 → 24h hard block；测试会红。必须先解决 B1。 |
| N=4/C=31 无假 503 | ⚠️ 取决于 K | 只有 per-egress K=8 才有 32 容量；配置 K=4 时 15/31=48.4% 503。断言必须写 `K` 和作用域。 |
| 每出口 24h 429 ≤8 | ❌ 当前不可测 | 状态是 per-pair `bad24`；N=1 多模型可 8×模型数；需要 egress 级时间戳队列。 |
| 重启不清 blocked | ⚠️ 可测但 schema 不足 | 需要 `node_state_v5` 的 `until/strikes/bad24` 持久化；`window` 是 int，60s 队列不持久化；fake clock + 重启进程测试。 |
| 190 个 5h → 5m canary | ❌ 当前不可测 | 没有 canary 分支/`canaryInFlight`；需要 `normal 非空 + canary 非空` 测试。 |
| 100 并发同一 pair 只发 1 个 canary | ❌ 当前不可测 | 没有 canary 单飞字段/锁；测试无法通过。 |
| 面板数字与 call_log 重算一致 | ⚠️ 可测但 schema 缺 | `admit/suppress/outcome` 有；缺 `status/retry_after`，`no_candidate` 的 `would_be_egress` 为空；需要事件级重算脚本。 |
| fake clock 5h | ⚠️ 需要接口 | 计划 N1 说 clock 注入，但当前代码全 `time.Now()`；需要 `Clock` 接口覆盖 pair/egress/sub/probe/sweep。 |
| 10 万 model 内存 | ❌ 与 50k 上限冲突 | 需要明确是“10 万 model 名”还是“10 万 pair”；当前 schema 不能两者兼得。 |
| shadow 分歧报告 | ❌ 未定义 | 需要 shadow state 副本、实际 egress/outcome 关联、Brier 定义。 |
| 回滚 5 分钟 | ❌ 无 harness | 需要 `NODE_SYSTEM=legacy` 的启动/请求/状态 golden 测试；当前 legacy 与删除清单冲突。 |
| N=1/C=32 的 222 请求 | ⚠️ 可测，计划未列 | 断言上游发送 ≤K（≤8），其余 503；`653e` 1,106 应 ≤K。 |
| 429 后不重选同一出口 | ⚠️ 可测，计划未列 | 构造 `429` 后 `Admit` 返回同一 `EgressKey` 即为失败；需要 `Admit` 的 egress 断言。 |

**最小可执行断言（建议直接进测试）：**

```text
T1  TestR8PairBreaker_4ad:        20 个 429 + 35 个 2xx/60s -> 不得 hard_blocked(24h)；若产品选择“IP 保护优先”，则改为期望 24h，并删除“4ad 不误封”验收（二选一）。
T2  TestR8PairBreaker_653e:       1,106 个 429/N=1 -> 上游发送 <= K；其余 no_candidate/all_inflight；无 1,106 连发。
T3  TestR8PairBreaker_0024:       222 个并发/N=1 -> 上游发送 <= K；第 3 个 429 后 queued 请求不得拿旧 pair lease。
T4  TestR8EgressBudget:           同一 egress 6 个 model 各 8 个 429 -> egress 24h 发送 <= 8（若预算按 egress）。
T5  TestR8InflightScope:          N=1、6 model、C=32 -> 同时上游 <= 8（不是 48/24）；N=1 时 500ms 队列不产生 >K 发送。
T6  TestR8CanaryPriority:         normal 非空 + canary 非空 -> 只返回 canary；100 goroutine -> 1 个 canary。
T7  TestR8RestartBlocked:         pair until/strikes 持久化，重启 + fake clock 后仍 blocked。
T8  TestR8LegacyGolden:           legacy 下 pool403/quotaHalt/direct429/全冷却回退与 HEAD golden 一致。
T9  TestR8ShadowIsolation:        shadow Observe 不改变生产 pair；分歧率/Brier 可重算。
T10 TestR8NoUpstreamOnSuppress:   每个 suppress 事件对应 0 个上游发送；每个 admit 对应 1 个 attempt。
```

## 8. Blocker 清单（按严重度）

### Critical

**R8-B1 — 24h 预算作用域错误，4ad 被误封**

- 证据：v5 §2.3 第 54 行、§6 第 173 行、§8 第 193 行；4ad 20×429/0.829s，35 个 2xx/60s。
- 关闭条件：二选一并写死：
  - A. 预算按 **egress**（`bad24` 时间戳队列，每 egress ≤8/24h），并定义 4ad 是否接受 24h 封禁；若接受，删除“4ad 不误封”验收；若不接受，预算只对 canary/global probe 计数或加“有 ≥N 个 2xx 的 pair 不计 normal 429”的条件。
  - B. 预算按 pair，但删除“每出口 ≤8”验收，并证明多模型不会把 egress 发送放大到不可接受。
- 测试：`TestR8EgressBudget` + `TestR8PairBreaker_4ad`。

**R8-B2 — K/在途/排队作用域**

- 证据：v5 §3.2 第 115 行、§5 第 159 行、§6 第 173 行；00:24 N=1 222 请求；00:30 653e 1,106。
- 关闭条件：定义 **egress 级** `inflight` 计数器/lease token；K=8（删 `pair_inflight_max=4` 或改名 `egress_inflight_max=8`）；队列上限 `min(C, N*K)`；N=1 直接 `all_inflight` fail-fast（或明确接受 8 个在途）；排队醒来必须重新 `Admit`/检查 pair `until`；测试 `N=1/C=32`、`N=4/C=31`、`N=3/C=31`。
- 测试：`TestR8InflightScope`、`TestR8PairBreaker_0024`。

**R8-B3 — canary 分支/单飞/190**

- 证据：v5 §2.3 第 51–53 行、§3.2 第 106 行、§11 第 223 行；状态无 `canaryInFlight`；190 个 5h。
- 关闭条件：在 `Admit` 伪代码中明确 canary 优先（normal 非空也要先处理到期 canary）、每 pair 单飞、`canaryInFlight` 原子置位/释放；`canaryOK/lastCanaryOK` 字段；`strikes>0 && until<=now` 才能 canary；`normal 非空 + canary 非空` 测试；190 个迁移 + 全部 2xx/全部 429 曲线。
- 测试：`TestR8CanaryPriority`、`TestR8CanarySingleFlight`、`TestR8Migration190`。

**R8-B4 — 状态 schema 不能表达规格**

- 证据：v5 §2.3 第 48 行、§6 第 173 行；`window/bad24/resets24` 为 int；无 `canaryOK/canaryInFlight/lastDecay`；无 egress `inflight`。
- 关闭条件：给出 `node_state_v5.json` 完整 schema（时间戳数组、24h 队列、canary 字段、egress inflight、lastDecay、sub probe）；定义重启后 60s 窗口/24h 预算/canary 单飞恢复；给出内存上界；N1 10 万 model 与 50k 上限二选一。
- 测试：`TestR8StateRoundTrip`、`TestR8RestartBlocked`、`TestR8MemoryCap`。

**R8-B5 — legacy/删除/迁移互斥**

- 证据：v5 §2.4 第 81–86 行、§7 第 185 行；`main.go:3290/3786` Halt；`proxy_state` v1。
- 关闭条件：明确 `NODE_SYSTEM=legacy` 是否保留 `shape_drift.go/quota_halt.go/pool403`（保留旧文件或构建标签）；给出 `legacy × pool403 × quotaHalt × direct429` 矩阵；旧 `NodeExhausted` 全类型映射（quota/non-quota/probation/dead/rejected）；两文件迁移幂等/回滚；5 分钟回滚演练脚本。
- 测试：`TestR8LegacyGolden`、`TestR8MigrationAllStates`、`TestR8Rollback`。

### High

**R8-B6 — 选择层 API/UCB 证据**

- 证据：v5 §2.2 第 34 行、§3.1–3.2；`.research/scoring2/03-empirical.md:347`。
- 关闭条件：`Admit` 增加 `session/exclude/attempt/tier` 入参；定义 `N`、`n` clamp、`lastDecay`；N3 降为实验开关或给出可复现的 UCB 回放（含 cold start 成本/egress 预算）；N3 验收改为“相对基线 + 置信区间”，不要用 +84.7pp。

**R8-B7 — 全局探测绕过/单飞/恢复**

- 证据：v5 §2.3 第 61 行、§5 第 160 行。
- 关闭条件：`NoCandidateReason` 区分 `all_pairs_blocked/all_sub_quarantine/all_dead/all_inflight`；探测只对 `all_pairs_blocked` 放行；`globalProbeInFlight` 单飞；`lastHit` tie-break；N=3/22/212 恢复表；探测预算按 egress。

**R8-B8 — 状态码/影子/403**

- 证据：v5 §2.2、§4.1、§7；`quota_halt.go` 503+30s。
- 关闭条件：`Reject.reason -> {status, Retry-After, body}` 表（`no_candidate`/`all_inflight`/`sub_quarantine` 503；per-pair quota 429？；不 passthrough 上游 429）；shadow 独立 state + 实际 outcome 关联；403 风暴的池级验收（或明确 sub 级足够）。

## 9. 若关闭 blocker 后进入 N1：第一批文件/测试

> 本节不是批准；只有 §8 的 5 个 Critical 全部关闭后才适用。

### 9.1 文件

1. `node_state_v5.go`：schema、原子读写、损坏 fail-open、迁移（`NodeExhausted` 全类型）、上限/清理。
2. `pair_breaker.go`：纯状态机（时间戳队列、3/60s AND、ladder、canary、bad24/resets24、`strikes=0 ⇒ until=zero`）。
3. `egress_state.go`：egress lifecycle、per-egress inflight、sub quarantine、budget。
4. `clock.go`：可注入 `Clock`，覆盖 pair/egress/sub/probe/sweep；测试用 fake clock。
5. `shadow.go`：独立 state 副本、decision/outcome 记录、Brier/分歧。
6. `node_state_v5_test.go`、`pair_breaker_test.go`、`egress_state_test.go`、`clock_test.go`、`shadow_test.go`。

### 9.2 测试

- `TestR8PairBreaker_4ad`、`_653e`、`_0024`、`_canary`、`_strikesZero`。
- `TestR8EgressBudget`、`TestR8InflightScope`、`TestR8QueueRecheck`。
- `TestR8StateRoundTrip`、`TestR8RestartBlocked`、`TestR8Migration190`、`TestR8MemoryCap`。
- `TestR8LegacyGolden`、`TestR8Rollback`、`TestR8ShadowIsolation`。
- `TestR8NoUpstreamOnSuppress`、`TestR8StatusMapping`。

### 9.3 验收命令（建议）

```bash
cd /home/jingyx/project/opencode2api-test
export PATH=$PATH:/usr/local/go/bin
export GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"

gofmt -l node_state_v5.go pair_breaker.go egress_state.go clock.go shadow.go
go build ./...
go vet ./...
go test -run 'TestR8' -count=1 -v ./
go test -skip 'TestIP4RV' -count=1 ./...
```

## 10. 最可能翻车的 3 个地方

1. **per-egress 预算/在途的作用域**：v5.1 用 pair 的状态做 egress 的 K/24h 限制；N=1、6 个模型时容量/预算是 `8×6`，不是 8。00:24 的 222 请求和 653e 的 1,106 连发都指向这里。实现时最容易在 `Lease`/state 里漏掉 egress 级计数器，导致“pair 已封、同出口另一 pair 继续打”。
2. **canary + legacy 的状态机**：canary 分支、`canaryInFlight`、`strikes` 降档、`until` 清零、190 个 5h 迁移、legacy 的 `shape_drift/quota_halt/pool403` 保留，六个变量互相耦合；任何一个分支漏写，就会出现“canary 永远不跑”或“回滚后旧闩复活”。这也是 R7-B3/Q1/Q6 的遗留点。
3. **状态码/影子/回滚**：`no_candidate/all_inflight/sub_quarantine` 的 503 语义、上游 429 是否透传、shadow 是否写生产后验、legacy 的 5 分钟回滚，都是“一处放行、另一处拒绝”的入口。事故时客户端看到 429 还是 503 会改变重试行为；影子如果污染后验，N5 的对比数据就不可信。

**一句话判断 v5.1 是否收敛：** 方向收敛（删全局闩、per-pair、canary、fail-fast），但规格未收敛；5 个 Critical 中的 B1/B2/B3/B5 仍是施工阻断，不能进入 N1。

## 11. 复现命令与数据口径

### 11.1 00:15–00:45 统计

```bash
python3 - <<'PY'
import json, datetime as dt, collections
rows=[]
for line in open('/tmp/c1027.jsonl'):
    r=json.loads(line); r['_t']=dt.datetime.fromisoformat(r['ts']); rows.append(r)
a=dt.datetime.fromisoformat('2026-10-27T00:15:00+08:00')
b=dt.datetime.fromisoformat('2026-10-27T00:45:00+08:00')
rs=[r for r in rows if a<=r['_t']<b]
print('requests',len(rs),'ok',sum(r['status']=='ok' for r in rs),'fail',sum(r['status']=='fail' for r in rs))
print('429 attempts',sum(1 for r in rs for e in (r.get('events') or [])
      if e.get('type')=='upstream_error' and '429' in e.get('detail','')))
print('switch',sum(1 for r in rs for e in (r.get('events') or []) if e.get('type')=='switch'))
print('egress',len({n for r in rs for n in (r.get('nodes') or [])}))
PY
```

### 11.2 `4ad` 的 60s 口径

```bash
python3 - <<'PY'
import json, datetime as dt
fp='4ad1982e75f81f0427b298af7b078754'
rows=[]
for line in open('/tmp/cl-1026b.jsonl'):
    r=json.loads(line); r['_t']=dt.datetime.fromisoformat(r['ts']); rows.append(r)
errs=[]
for r in rows:
    for e in (r.get('events') or []):
        if e.get('type')=='upstream_error' and '429' in e.get('detail','') and e.get('node')==fp:
            errs.append(dt.datetime.fromisoformat(e['at']))
t=min(errs); lo=t-dt.timedelta(seconds=60)
ok=[r for r in rows if fp in (r.get('nodes') or []) and r['status']=='ok' and lo<=r['_t']<=t]
print('first429',t,'429',len(errs),'2xx_in_60s',len(ok),'ratio',len(errs)/(len(errs)+len(ok)))
PY
```

### 11.3 数据缺口

- `/tmp/c1027.jsonl` 只到 09:00，不能代表全天；`/tmp/cl-1027b.jsonl` 只到 00:23，不含 00:30 的 653e。
- 生产 N 不是 212；本次窗口 6 个 egress，18:03 22 个，00:20 4 个。
- 403 在 00:15–00:45 为 0；403 策略的结论来自历史 `02-constraints.md`/R6，不是本窗口。
- 流式样本很少；长流/租约行为用代码上界（`idleReadTimeout=120s`）补充，不能用本次日志证明。
- `4ad` 的 219 个 2xx 是全天/窗口累计，不能当作 60s 窗口；本报告用首次 429 前 60s 的 35 个 2xx。

## R8-delta（v5.2）

复核对象：`docs/superpowers/plans/2026-10-27-node-system-v5.md` v5.2（HEAD `2be7905`）。只核 5 个硬矛盾 + 3 条断言的处置，不重开全量评审。

**结论：NO-GO：仍有 6 个 blocker。**

- **D1** `(c) now-last2xx>10s` 使真实 `653e` 第 3 次 429 **不熔断**（第 3 次距最近 2xx 仅 4.32s）；按真实时间戳回放 `TestR8PairBreaker_653e` 会红，用合成的 >10s 静默夹具则会掩盖计划对 653e 的误述。
- **D2** `inflight<K` 只写了返回条件，没有原子预留；N=1/C=32 不保证 ≤8，`exclude` 也仍依赖调用方。
- **D3** probe 没有单飞状态位/锁；190 个迁移出口只在“无候选”时才 probe，正常 2xx 不减 `strikes`。
- **D4** `win429/winAtt` cap 64 不能表达精确 60s；schema 仍缺 `pair.lastDecay`、sub 403 计数、direct 身份。
- **D5** direct 429 没有真正并入 egress breaker：§2.4 说合并，§2.3 没有 direct 的 trip 条件，schema 的 `egress` 也没有 direct 键。
- **D6** egress 熔断会误伤同出口其他 model；`probe 429 -> strikes+1` 无上限（`ladder[4]` 越界）；`trips24h=3` 叠加“只有 probe 2xx 才减 strikes”会把可恢复出口硬封 24h。

### 1. R8-1：4ad / 653e / model quota

**不通过。** 4ad 的 egress trip 确实被 (b) 和 (c) 挡住：20 个 429/0.829s，首 429 前 60s 有 35 个 2xx、36 个请求，占比 36.4%，且首 429 距最近 2xx 3.225s（计划写 4s，实测 3.2s）；但“确定不封”仍取决于该 egress 的 `trips24h` 历史，计划没有给 4ad 的 24h 历史，不能无条件断言。653e 不通过：首 429 前 1.557s 还有 2xx，第 3 个 429 距最近 2xx 仅 **4.32s**，`(c)` 为假；按字面它要到 `00:30:35.781`（首 429 后 10s）才可能 trip，不是第 3 次。model-specific quota 由 `pair.consec429>=3` 覆盖，但只覆盖“连续 429”，且 2xx 会清零；这部分通过。**结论：4ad 的 egress trip 通过、653e 第 3 次 trip 不通过、model quota 通过，整题不通过。**

### 2. R8-2/3：egress K=8、无排队、N=1/C=32

**不通过。** 作用域已统一为 egress 级、K=8、无排队，方向正确；但 `Admit` 只承诺“返回满足 `inflight<K`”，没有说 `check+increment` 是原子 CAS/持锁。32 个 goroutine 可以同时看到 `inflight=7`，全部拿到 attempt，上游 >8。`TestR8InflightScope` 可以写成确定性测试（32 个请求 + 阻塞上游 + barrier，断言最大并发 ≤8、其余立即 503），但前提是先补原子预留；否则测试只会间歇性红。

### 3. R8-3：删 canary、probe 恢复、190 个 5h

**不通过。** `until=+5m` 到期后正常请求可以重新选中，服务路径存在；但“恢复”不等于 `strikes` 清零：§2.3 只在 **probe 2xx** 时 `strikes-1`，正常 2xx 不减；而 probe 只在“无候选”时触发。212 个出口的池子里，190 个迁移出口通常不会成为“无候选”，所以它们可能长期 `strikes=1`，下一次 429 直接走 30m 档。probe 单飞没有 `probeInFlight` 字段/锁，`lastProbe` 只是时间戳，不能阻止并发 probe；且 `probe 429 -> strikes+1` 无 clamp，`strikes=4` 时 `ladder[4]` 越界。

### 4. R8-4：删 legacy、镜像回滚、node_state_v5

**通过（互斥已解除），但有残留。** 删除 `NODE_SYSTEM=legacy` 后，§2.4 与 §7 不再要求两套状态机并存；旧镜像不认识 `node_state_v5.json`，不会主动读取/覆盖它。残留是：旧镜像回滚后该文件仍在卷里，再升级到 v5.2 会读到回滚前的 `until/strikes/trips24h`，计划没有 epoch/清理步骤；这不是“旧版误读”，但会让回滚后的再升级继承旧状态，建议回滚脚本显式移走该文件。

### 5. R8-5：win429/winAtt/trips24h/inflight

**不通过。** `trips24h` 用 int64 毫秒数组、cap 16，能表达 24h 滚动；`inflight` 不落盘、重启最多多放 K 个，也可以接受。但 `win429/winAtt` cap 64 与“精确 60s”冲突：00:24 是 N=1、222 个 429/60s，单出口就超过 64，超出丢弃最旧后计数/占比不再是精确 60s（50 req/s 更严重）。仍存不下的字段：`pair.lastDecay`（7d 半衰期需要每个 pair 的衰减时间）、`sub` 的 403 计数/时间戳（同 pair 2 次无法只靠 `pairs` 列表表达）、direct 的 egress 身份、`probeInFlight`。

### 6. ①：Admit + exclude 是否保证不拿旧 egress

**不通过（部分）。** `Admit(model, exclude, now)` 的契约和 `exclude` 方向正确，排队也删了；但计划没有写重试循环必须把刚失败的 egress 加进 `exclude`，`TestR8PairBreaker_0024` 也只测“第 3 个 429 之后的 Admit”，没测重试路径。另一个竞态是 `Observe` 与下一次 `Admit` 之间没有 egress 锁：第 3 个 429 已返回但 `Observe` 尚未执行时，新 `Admit` 仍可能拿到旧 egress；这只能靠“≤K 在途”解释，不能靠 exclude 保证。

### 7. ②：三条状态码 + direct 429

**不通过（direct 部分）。** 429 后无候选 → `no_candidate` 503 已覆盖；`all_inflight`/`sub_quarantine` 也有表。但 direct 429 没有真正并入 egress breaker：§2.4 说“direct 作为 egress，合并进 egress breaker”，§2.3 只写“Egress 熔断”，没有 direct 的 `last2xx/strikes/trips24h` 身份；§6 的 `egress` 只按 `fp` 键，没有 `direct`。删掉 `quota_halt` 后，direct 429 只剩 `pair.consec429`，不是 egress trip；状态表也没有 direct 的 429 出口语义。

### 8. 新矛盾

**有，至少三条。** ① egress 熔断按出口而不是按 model：model A 的配额 429 连续 3 次、10s 无 2xx，会 trip 整个出口，误伤同出口的 model B；§10.7 已承认“可能误伤”，但把它留给影子期，不是规格。② `probe 429 -> strikes+1` 没有上限，`ladder` 只有 4 档，第 5 次 probe 429 会越界；且 probe 429 不计 `trips24h`，与“预算兜底”不一致。③ `trips24h>=3 -> hard_block(24h)` 与“只有 probe 2xx 减 strikes”叠加过严：一个出口在 5m/30m/2h 三次 trip 后硬封 24h，即使中间有正常 2xx 也不减 strikes；4ad 的 pair 只 5m，不受影响，但 653e 类出口会很快进入硬封。

**验证边界：** 本 delta 只读 v5.2 与日志；653e 的 2xx 间隔、4ad 的 3.225s 均为 `/tmp/c1027.jsonl`、`/tmp/cl-1026b.jsonl` 复算。未改代码、未 commit；未运行计划中的测试（测试尚不存在）。

## R8-delta-2（v5.3）

复核对象：`docs/superpowers/plans/2026-10-27-node-system-v5.md` v5.3（HEAD `5c8b64b`）。只回答 7 问，不重开全量评审。

**结论：NO-GO：3 个 blocker。**

- **B1** `trips24h` 只有 cap16、没有 24h 滚动清理；`trips24h>=6 && strikes==4` 可在“6 次历史孤立 429 + 一次 4 连 429”时把 90% 健康 pair 硬封 24h，且 schema 仍缺 `epoch`、`consec429`、direct 键；hard block 的 2xx 清零语义也未定义。
- **B2** `Admit` 取名额后的 `Reject` 归还、`sync.Once` 的作用域、`probeMu` 的持锁时长都没有写成契约；slot 泄漏、probe 并发、probe+正常 2xx 重复释放都没有测试兜底。
- **B3** 首个 429 冷却 + 43 个 egress 的模型级配额风暴可让所有 pair 同时冷却；`no_candidate` 503 后没有全局 probe/降级路径，sticky/UCB 都无候选可用。08:00–09:00 未实际发生，但计划没有测试或保护。

### 1. D1：首个 429 冷却与 5/4/1/0

**规则层面关闭；数字不能完全复现。** 真实日志：`653e×mimo` **182×2xx / 1,106×429**，`41d×mimo` **0×2xx / 402×429**，`4ad×mimo` **22×2xx / 20×429**，`653e×deepseek` **5×2xx / 0×429**（与你的事实一致；另 `41d×deepseek` 10×429）。

用计划阶梯 **60s/2m/5m/5m**、首个 429 起冷却、任意 2xx 清零，我复算：

| pair | 原始 429 | 我复算发送 429 | 你的数字 |
|---|---:|---:|---:|
| 653e×mimo | 1,106 | **4** | 5 |
| 41d×mimo | 402 | **3** | 4 |
| 4ad×mimo | 20 | **1** | 1 |
| 653e×deepseek | 0 | **0** | 0 |

5/4 只在首档 **30s**（如 30/60/120/300）或 60/60/120/300 下成立；计划写 60/2/5/5，却引用 5/4，正文与证据差一档。`TestReplay653e`/`41d` 的“≤8”仍会通过（4/3），但“1,106→5”“402→4”不是该阶梯的精确结果，必须改 ladder 或改数字。

### 2. D2：channel 信号量 + sync.Once

**原子取名额通过，归还契约不够。** 带缓冲 channel 的非阻塞 `select` 是原子预留，D2 的 check-then-act 竞态确实消失；但 `sync.Once` 只保证“最多释放一次”，不保证“一定释放”。`Admit` 取到名额后若因 sub 刚隔离、pair 刚冷却、exclude 变化而返回 `Reject`，计划只有一句“必须归还”，没有 `Attempt.Close()`/`defer release` 的统一路径；panic、客户端取消、上游未返回同样会漏。`TestInflightAtomic` 还缺“32 个全部到达 Admit 后再放行上游”的 barrier，否则首个 429 后部分请求会得到 `no_candidate` 而不是 24 个 `all_inflight`。

### 3. D3：probe 单飞与 190 个 5h

**恢复路径存在，单飞契约不完整。** 190 个迁移 pair 的 `strikes=1, until=now+5m` 到期后会重新进入候选；任意正常 2xx 即清零，所以“有流量”时确实有路径。若一直无流量，probe 退避 60s→6h，190 个出口串行探测最坏要数小时，但不会永久封死。`probeMu + nextProbeAt` 只有把锁从“检查 nextProbeAt”一直持有到 `Observe` 才叫单飞，计划没写持锁边界，也没有 `probeInFlight` 字段；`sync.Once` 若是 per-attempt，probe 与正常 2xx 不会重复释放；若是 per-egress，则会。两种作用域必须写明并加并发测试。

### 4. D4：删 win 后的 schema、trips cap、epoch

**不通过。** `trips24h` 写“追加”+“cap16（超出丢最旧）”，但没有“删除 24h 前记录”的规则；名字叫 24h，实际是**有生以来最近 16 次**，6 次阈值会被历史 trip 触发。cap16 在“6 次即封”下容量够，但前提是先做 24h 滚动清理。schema 还缺顶层 `epoch`（正文说 node_state_v5.json 带 epoch，JSON 示例没有）、`pair.consec429`、`egress.direct` 键；`epoch` 设计本身能防“回滚后再升级继承”，但必须持久化、比较并在不匹配时整体丢弃，不能只写在文字里。

### 5. D5：direct 双写

**概念自洽，落地不自洽。** `pair=(model,direct)` 负责该 model 的 strikes/until，`egress.directUntil` 负责整条直连；`Admit` 若对 direct 检查 `directUntil`，其他 model 的 direct pair 会被排除，方向正确。但 §6 的 `egress` 只按 `fp` 键，没有 `direct` 键；`directUntil` 无处持久化。双写也不是事务：崩溃在两次写之间会留下“pair 已冷却、directUntil 未写”或反之；重启后一个 model 可能仍能走 direct。`TestDirectCooldown` 只测同时冷却，没测崩溃/重启和部分写。

### 6. D6：90% + 6 次 429 是否会被硬封

**不保证。** 6 次**孤立** 429 每次都遇到 2xx，`strikes` 归零，不会被封；2xx/429 交替也不会到 `strikes=4`，这部分成立。反例是：先有 6 次历史 trip（`trips24h=6`，strikes 已被 2xx 清零），随后一次 **4 连 429**、期间无 2xx，则 `trips24h>=6 && strikes==4` 立即硬封 24h；这个 pair 整体仍可能是 90% 成功率。计划把“4 连 429”等同“真死”，但配额风暴下 4 连 429 可以是瞬时的；24h 过重。另一个歧义：hard block 后若 probe 仍被允许发 2xx，“任意 2xx 即清零”会把 24h 解封，hard block 不稳定。

### 7. 新矛盾：首个 429 + sticky + UCB 的全冷却 503

**有风险，但 08:00–09:00 数据未触发。** 我复算该小时：968 请求、873 2xx、95 失败（**90.18%**）、上游 429 仅 **8** 次、197 个 pair、137 个出口出现过；没有出现“所有候选同时冷却”。但架构允许：一个 model 的配额 429 在 60s 内打到 43 个 egress，43 个 pair 同时 `until=+60s`；`Admit` 无候选 → `no_candidate` 503。`probe` 只写“无流量时”触发，没有写“全冷却时绕过 until 探测”；sticky 会粘住失败 pair，UCB 在没有候选时也无意义，客户端重试只会重复 503。当前窗口风险低，模型级配额风暴下风险高；至少应加 `TestAllCooling`（43 pair 同时 429 → 503 有界、probe 能恢复）和全局 503 预算。

**验证边界：** 本 delta 只读 v5.3 与 `/tmp/c1027.jsonl`、`/tmp/cl-1026b.jsonl`；未改代码、未 commit，未运行计划测试（尚不存在）。

## R8-delta-3（v5.4）

复核对象：`docs/superpowers/plans/2026-10-27-node-system-v5.md` v5.4（当前 `c2cf5c3`；lead 给的 `c8ae6e3` 之后还有 direct/config 修订）。只核 B1/B2/B3，不重开全量评审。

**结论：NO-GO：1 个 blocker（B3）。**

### 1. B1：通过（文档/状态机层面）

- `pair_trips_24h_cap` 已改为 16「仅内存上限/展示，不参与裁决」；§2.3 明确 hard block 只看连续第 5 次 429，`trips24h` 只做 24h 滚动清理，schema 已有 `epoch` 与顶层 `direct.until`，`consec429` 残留引用已删。90% 健康 pair 的「6 次孤立 429」不会再触发硬封。
- 仍需 N1 补两个测试：`TestHardBlockOnFifthConsecutive429`（第 4 次只 5m、第 5 次才 24h）与 `TestIsolated429DoesNotHardBlock`（6 次均有 2xx 隔开）。面板/告警补 `pair_trips_24h` 阈值告警即可，不影响架构。
- 注意：`strikes=min(strikes+1,4)` 与「第 5 次」的先后顺序在伪代码里仍靠文字解释；实现必须先判 `strikes==4` 再决定 hard block，否则第 4 次就会被误封。这是测试项，不是新 blocker。

### 2. B2：通过（契约层面）

- `Admit` 取名额后 `defer` 归还、`Attempt.release` 用 per-attempt `sync.Once`、请求路径 `defer Observe(abandoned)`、`probeMu` 只保护 `nextProbeAt/probing` 且网络调用在锁外，slot 泄漏与 probe 并发都有确定行为；probe 与正常 2xx 是不同 Attempt，不会重复释放。
- 需补 `TestProbeSingleFlight`（两个 no_candidate 只产生 1 个上游 probe）与 `TestAdmitRejectReleasesSlot`（sub 在取名额后隔离，slot 必须归还）；`TestInflightAtomic` 已有，覆盖 32/24。

### 3. B3：不通过（架构级）

- **当前 v5.4 正文没有「no_candidate 时发起 probe」**：§2.3 只写「无流量时由 probe 触发」，§2.2/§3.2 的 `no_candidate` 只有 503；lead 描述的「no_candidate 按退避单飞 probe」没有落进计划。
- `Retry-After = max(1, min_until-now)` 的 `min_until` **没有定义**。`no_candidate` 不只来自 pair 冷却，还可来自全部 sub 隔离（`sub.until`）、全部 egress dead（`deadUntil`）、无 pair/全部 exclude；这些情况下 `min_until` 不存在，当前公式会退化成 `Retry-After: 1`，客户端每秒重试，形成 503 风暴。
- probe 若仍走 `Admit`，会被 `pair.until` 挡住；全 pair 冷却时 probe 无法选到任何 pair，计划声称的「no_candidate probe」实际不可达。若 probe 绕过 `until`，又必须明确它占用哪个 egress 的 inflight、是否算 `all_inflight`，以及失败后是否升档。
- 最小修复：定义 `min_until = min(pair.until, egress.deadUntil, sub.until)`（无值则 `now+1s`），并明确 `no_candidate` 的 probe 走独立 `AdmitProbe`（绕过 pair.until，但仍占 egress 名额、单飞）；补 `TestAllCoolingNoCandidate`、`TestNoCandidateRetryAfter`、`TestAllSubQuarantinedRetryAfter`。这三项是架构契约，不是命名细节。

**验证边界：** 本 delta 只读 v5.4 `c2cf5c3`；未改代码、未 commit，未运行计划测试（尚不存在）。

## R8-delta-4（v5.5）

对象：v5.5 HEAD `342d748`。只核 B3 与硬封改动。

**结论：NO-GO：2 个 blocker（B3、B4）。**

### 1. B3：未关闭

- `min_until(model)` 只统计 pair；全部 sub 隔离时为空，`Retry-After=1`，而 `AdmitProbe` 又要求 sub 未隔离——probe 无法启动，客户端每秒重试到 `sub.until` 到期。状态表同时有 `sub_quarantine:30` 与“全部 sub 隔离 ⇒ no_candidate”，reason 也不唯一。
- `AdmitProbe` 共用信号量本身不会与正常请求互相饿死（非阻塞取名额、单飞），但 probe 没有 deadline/TTL：上游挂起时 `Observe` 不返回，名额永久占用、`probing` 永久为真，正常容量少 1。需 `probe_timeout` + `defer release`，测试 `TestProbeTimeoutReleasesSlot`。
- probe 429 写 `until=now+probe_backoff`，会把刚设的 1h hard block 缩短到 60s，硬封时长失效；probe 退避应写独立的 `probeNextAt`，或用 `until=max(until, now+probe_backoff)`。

### 2. B4：硬封新矛盾

- `hard=min(hard+1,5)`、`1h*2^(hard-1)` 的实际序列是 **1/2/4/8/16h**，永远到不了注释和配置写的 24h；要 24h 需 `hard` 上限 6 或显式 `1/2/4/8/24` ladder。
- 2xx 的伪代码只写 `strikes=0; until=0`，没有 `hard=0`；正文却说“任意 2xx 清零 `strikes/hard`”。按伪代码实现，健康 pair 的 `hard` 会跨恢复周期累积，下一次 5 连 429 直接 2h/4h/16h，属于新误封。
- `TestHardBlockBounded` 只验“首次 1h”，没有验 24h 封顶和 2xx 后 `hard` 归零；需 `TestHardBlockCap24h`、`TestHardResetOn2xx`。`probe 429` 不升 hard 与上述 `until` 覆盖叠加，还会让 hard 状态与 until 脱节。

**验证边界：** 本 delta 只读 v5.5 `342d748`；未改代码、未 commit，未运行计划测试（尚不存在）。

## R8-delta-5（v5.6）

对象：v5.6 HEAD `ce18004`。只回答“还有没有架构级矛盾”。

**结论：GO：可进入 N1。**

- B3 已关闭：`min_until` 覆盖 pair/sub/dead，全部 sub 隔离时 `Retry-After` 取 sub 最早到期；`AdmitProbe` 跳过 pair/sub 冷却、20s deadline、最多占 K 中 1 个，probe 与正常请求不会互相饿死，超时归还名额。
- B4 已关闭：`hard` 6 档为 1h/2h/4h/8h/16h/24h；2xx 同时清 `strikes/hard`；probe 429 用 `max(until, now+probe_backoff)`，不缩短硬封。
- 仅剩测试细节：`AdmitProbe` 必须同时绕过 `sub.quarantined`（不能只绕过 `sub.until`），由 `TestAllSubQuarantined` 钉住；`min_until` 只统计该 model 的候选 egress；`TestProbeDeadline` 验证 20s 归还；`TestHardBlockBounded` 验证 6 档，另补 `TestHardResetOn2xx` 与 `TestNoCandidateRetryAfter`。

### N1 第一批文件

| 文件 | 测试 |
|---|---|
| `node_state_v5.go`（pair/egress/sub、epoch、load/save、50k/30min 上限） | `TestR8StateRoundTrip`、`TestR8RestartBlocked`、`TestR8MemoryCap` |
| `node_state_v5_migrate.go`（旧 `proxy_state` → v5、190 个 NodeExhausted） | `TestR8Migration190` |
| `clock.go`（可注入 Clock，替换 `time.Now()`） | `TestR8ClockInjection` |
| `shadow_record.go`（影子决策记录，不改发送） | `TestR8ShadowRecord` |

**验证边界：** 本 delta 只读 v5.6 `ce18004`；未改代码、未 commit，未运行计划测试（尚不存在）。
