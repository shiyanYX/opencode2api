# R7 对抗复审 · 节点系统 v4.1（按出口熔断 + fail-fast）

> 复审对象：`docs/superpowers/plans/2026-10-27-node-system-v4.md` v4.1（HEAD `fbc43de`）
> 复审方式：只读代码/日志，不写实现、不 commit、不启动服务。
> 状态：**已完成；结论见 §0。**

## 0. 结论（先给）

**仍需修订（NO-GO），v4.1 不可直接进入 S1a 实现。**

方向是对的：删掉 429 全局闩、按出口熔断、fail-fast，都是对 v3.2 过度设计的正确收敛；R6 指出的 4 个能力缺口也确实写进了 v4.1。但 v4.1 把「4 个缺口」写成了「4 个新歧义 + 2 个旧残留」，其中 5 个会直接导致事故场景行为错误，不能用「实现时再注意」带过。

| 编号 | 严重度 | 问题 | 证据/后果 |
|---|---|---|---|
| R7-B1 | Critical | 429 计数单位未定义；ring[8] 与「60s 内 ≥3 次」不等价；canary/global 单次失败不升档 | `4ad1982e` 20 个 429 尝试在 0.829s 内；按 attempt 计第 3 次即封，按 request 计仅 1 次不封；计划第 50/111/131 行的验收自相矛盾 |
| R7-B2 | Critical | K=4 只证明了 N=22；事故时 N=4，容量 16 < 并发 31 | 00:20 的 15/31=48.4% `all_inflight`；`41d39652` 被封后 N=3，19/31=61.3% |
| R7-B3 | Critical | 强制 canary 的伪代码没有唯一裁决：normal 先选且无 return，canary 要么不可达、要么同次选两个/泄漏租约 | 190 个 5h 出口在 normal 22 存在时可能永远不 canary；S2 又没有 5h→strikes 的映射 |
| R7-B4 | Critical | 60s 全局探测与「每出口 8 次/24h 硬预算」数学上不能同时满足「≤2min 恢复」 | N=3：不封顶 480/IP/日（阈值的 60 倍）；封顶则 24min 后全天无探测，最后一次封禁到 24h24m 才到期 |
| R7-Q1 | Critical | `strikes` 降到 0 时 `until=now+ladder[0]` 仍非零；`normal` 要求 `until.IsZero()`、`canary` 要求 `strikes>0` ⇒ 健康节点永久排除 | 第 81 行；3 次 reset 后的健康出口可永久既不 normal 也不 canary |
| R7-Q4/Q5 | High | 租约 TTL/幂等/sweep/长流未定义；`egress.go:143` 的直连回退仍在，绕过 `Admit`/全局探测 | 10min TTL 与长流；全冷却 → 直连 429 → `quota_halt`，与「全 blocked 时 60s 探测」直接冲突 |
| R7-Q6 | High | S2 迁移映射缺失；重启无 breaker 持久化；403 无策略；suppressed 字段与计划不一致 | 190 个 5h 冷却的恢复路径未定义；R5-5 的「重启全丢」未关闭 |

一句话：**v4.1 的架构方向已收敛，规格还没有收敛；需要 v4.2 关闭上述 5 个 Critical + 2 个 High，才可进入 S1a。**

## 1. 方法与数据边界

### 1.1 数据

| 文件 | 范围 | 行数 | 用途 |
|---|---|---|---|
| `/tmp/cl-1026b.jsonl` | 2026-10-26 15:16–23:59 | 13,724 | 18:03 多出口 429、20:13 峰值并发 |
| `/tmp/cl-1027b.jsonl` | 2026-10-27 00:00–00:23 | 541 | 00:20 单出口连发、00:20:58 闩死 |
| 合计 | 10-26/10-27 | 14,265 | 本报告全部数字 |

字段口径：

- `status=ok/fail` 是**请求级**最终结果；`events[].type=upstream_error` 且 `detail` 含 `http 429` 是**尝试级** 429。两者不能混用。
- `nodes[]` 是一次请求尝试过的出口；`events[].node` 是该事件对应的出口；`duration_ms` 用于计算并发重叠。
- 并发用 `[ts, ts+duration_ms]` 区间重叠；到达率用 `ts` 按秒计数；流式只有 3 条、最长 4.4s，不能代表长流，长流结论用代码上界补充。

### 1.2 代码

- HEAD `fbc43de`；`proxy_node.go`、`main.go`、`quota_halt.go`、`egress.go` 只读。
- 当前工作区只有未跟踪的 `.research/scoring2/`、`docs/mockups/`、`zz_ip4v_layout_new.go`；本次不改代码。
- 关键位置：`egress.go:143` 直连回退；`main.go:2878` quotaHalt gate；`main.go:2946` poolHalted；`main.go:2964-2965` `blockedEgress` + `egress.Get`；`main.go:3329-3344` 直连 429 → `quota_halt`；`proxy_node.go:31/456/494/1003/1664` 的 `NodeProbation`/mark/sweep/recordProbeSuccess。

### 1.3 边界

- 生产节点总数未知：计划写 212，日志 6 小时只出现 86 个 distinct egress；18:03 窗口 22 个，00:20 窗口 4 个。本报告把 N 作为场景参数，不把 212 当事实。
- 只回放日志，不启动服务；不验证真实上游 429 的恢复曲线。
- 不评价 T7 打分、T2 403 的具体实现，只评价 v4.1 与它们的交互。

## 2. B1：60s 触发条件与真实序列

### 2.1 三个关键出口的数字

| 出口 | 请求级 ok/fail | 尝试级 429 | 429 时间范围 | v4.1 按 attempt | v4.1 按 request |
|---|---:|---:|---|---|---|
| `4ad1982e75f81f0427b298af7b078754` | 219 / 6 | **20** | 18:03:55.108–18:03:55.937（**0.829s**） | 第 3 个 429 即封 5m | 仅 1 个请求最终 429，不封 |
| `27436cb4ff2a9ef75ccc5b7ddf56143c` | 1,022 / 1 | **1** | 00:20:51.180 | 不封 | 不封 |
| `41d39652cad7593b2a8320299ef4ae62` | 0 / 171 | **171** | 00:20:59.825–00:23:19.251 | 第 3 个 429 即封 | 第 3 个失败请求即封 |

`4ad1982e` 的细节必须说清楚，因为计划第 50 行和 R6 的引用都把它说反了：

- 219 个成功中：121 个 mimo、98 个 deepseek；全部在 18:03:55.108 之前。
- 6 个失败记录中：**只有 1 条最终是 429**（`d2wvs97gy1fd`，18:03:51.873 发起，18:03:55.108 收到 429）；另 5 条是 `connect_ok` 后的其他失败，不是 429。
- 20 个 429 事件来自 **20 个不同请求的尝试**：其中 19 个请求换出口后成功，1 个失败。
- 因此 R6 的「6 次 429 vs 219 成功」既不是尝试级（实际 20 次），也不是「6 个 429 请求」；它是 6 条失败记录，其中只有 1 条是 429。
- 计划第 50 行「`4ad1982e`（6 次 429 vs 219 成功）不会被 5m 封死」在**按 attempt 计数**下是假的：第 3 个 429 就封。计划没有定义 `Complete` 是按 attempt 还是按 request 调用，这是 S1a 的第一个必须钉死的语义。

`41d39652` 的数字：

- 171 个 429：166 个 mimo、5 个 deepseek；171 条请求全部失败。
- 前 3 个 429：00:20:59.825、00:21:00.156、00:21:00.217，间隔 **0.392s**；第 3 个即触发。
- 首个 429 后 60s 内有 **143** 个 429；全量 171 个跨 2m19.426s。
- 00:20:58 的 `shape_drift` 请求 `hbqdmv402hwv` 尝试 `27436cb4`/`30a03abd`/`f33f790b`，在第 3 个 429 处进入闩；这与 v4.1「第 3 次熔断」一致。

结论：**B1 对 00:20 连发的目标是能达到的；对 `4ad1982e` 的「不误封」目标取决于计数单位，而计划没有定义，按 attempt 会失败。**

### 2.2 ring[8] 与「60s 内 ≥3 次」不等价

计划第 23 行用 `ring[8]` 保存最近 8 次结果，第 43-45 行在 ring 上数 60s 内的 429。反例（同一出口，60s 内）：

```text
t=0   429
t=1..8  2xx ×8
t=9   429
t=10..17 2xx ×8
t=18  429
```

- 第 3 个 429（t=18）到达时，ring 只有 t=10..17 的 8 个 2xx 和 t=18 的 429；`n429=1`，不触发。
- 真实 60s 内 429 数是 3；按计划文字应触发。
- 该序列 429 占比只有 3/19，不满足「≥4 次中 ≥50%」的第二条件；如果这是有意的「低频 429 不封」，就应删掉「≥3 次」的绝对条件，而不是让 ring 偷偷实现另一个规则。

**修法**：用 60 个 1s 桶（或按时间裁剪的切片）统计 `attempts60s`/`hits429`，不要用固定 8 条 ring；并在测试里固定 `now` 覆盖「3 次 429 被 8 个 2xx 隔开」的序列。

### 2.3 canary/global 失败不升档

计划第 83 行说「canary 失败 = strikes+1」，但紧接着「见 1.1」；1.1 只在 `n429>=3` 或比例 ≥50% 时升档。于是：

- 单次 canary 429 → 不满足 3/60s → `strikes` 不变、`until` 不变。
- 60s 后同一个出口可以再次 canary，再次 429，仍然不升档。
- 全局探测第 90 行同样说「失败按 1.1 升档」，单次失败也不升档；如果全局探测每 60s 命中同一个出口，就会无限重试而不进入 5m/30m/2h/5h。

这直接违反 B3/B4 的「失败退避」目的。**修法**：`Complete(lease.kind==canary|global_probe, 429/FUL)` 必须无条件 `strikes++` 并按新档设置 `until`；只有 normal 路径才使用 3/60s 速率触发。

### 2.4 `badSends24h` 的硬预算插入位置

计划第 42 行在 ring 计数之前：

```text
if badSends24h >= 8: until=now+24h; strikes=4; return
```

这会把「第 8 次 429」直接变成 24h 封禁，且不检查这 8 次是否分布在 24h 内、是否是同一模型的 429、是否是 canary 探测。R6 的 Q1 已经指出「reset 分支无法被 ladder 约束」；v4.1 又加了硬预算，但没有定义 `badSends24h` 的滚动窗口和衰减。见 §5/Q1。

## 3. B2：K=4、N=22/37 与事故时 N=4

### 3.1 生产并发与出口数

| 窗口 | 请求 | ok | fail | distinct egress | 峰值并发 | 峰值到达 |
|---|---:|---:|---:|---:|---:|---:|
| 18:00–23:00 | 12,286 | 1,012 | 11,274 | 86（6h 累计） | **37**（20:13:20） | 36/s |
| 18:03 分钟 | 36 | 35 | 1 | **22** | 31 | — |
| 00:20–00:24 | 205 | 33 | 172 | **4** | 31 | 30/s |

计划的 B2 验收是 `C=37,N=22,K=4`：容量 `22×4=88 > 37`，**0 个 503**。这个结论在 N=22 时成立，但它不是事故时的 N：

- 00:20 只有 4 个 distinct egress；`C=31,N=4,K=4` 容量 16，至少 15 个并发请求看到 `inflight>=K`，即 **15/31=48.4%**。
- `41d39652` 在第 3 个 429 被封后，N=3；容量 12，**19/31=61.3%**。
- 若按 R6 的 C=37、N=3，容量 12，**25/37=67.6%**。
- 这不是「假 503」：K=4 下容量确实不够；但它说明**计划的 K=4 不能覆盖事故后 N≈22 的说法，更不能覆盖 00:20 的 N=4**。B2 的验收必须写成「N=22 时 0 假 503；N=4/3 时 503 是真实容量不足，需另定策略（提高 K、恢复 N、排队或明确接受）」，不能用一个 N=22 的数字代替事故场景。

### 3.2 `all_inflight` 的 1s Retry-After

`all_inflight` 的 503 带 `Retry-After: 1s`（计划第 63 行）。按每秒重试估算：

```text
excess = max(0, C - N*K)
retry_rate ≈ excess / 1s
```

- `N=4,C=31,K=4`：15/s；`N=3`：19/s；`N=3,C=37`：25/s。
- `N=22,C=37`：0/s；`N=22,C=100`：12/s。
- 15–25/s 不是「雪崩」，但会把每一个 503 变成每秒一次的固定负载；如果客户端不遵守 `Retry-After`，会更高。计划没有 jitter、没有队列、没有「all_inflight 超过 X 时改用 30s」的策略。
- 更重要的是：这些 503 发生在 **N 已经很小**的时候，而 N 小正是 B3 迁移/5h 冷却造成的；B2 与 B3 必须一起验收。

### 3.3 长流与租约

计划第 15 行给租约 10min TTL；生产只有 3 条流式、最长 4.4s，无法验证长流。若一条流超过 10min：

- sweep 若把租约回收，`inflight--`，同一出口可被再次分配，实际并发超过 K。
- 若流式不续租，TTL 到期即误回收；若续租，计划没有续租协议。
- 这不是 00:20 的直接原因，但会让 K=4 的容量在长流下失真；见 §7。

## 4. B3：强制 canary 与 190 个 5h 冷却出口

### 4.1 伪代码没有唯一裁决

计划第 12 行说「`strikes>0` 的出口强制单飞 canary，不进入正常候选」；第 57-60 行却是：

```text
normal = {...}
canary = {...}
if normal 非空: 选一个 ... lease{kind:normal}
if canary 非空: 选一个 ... lease{kind:canary}
if candidate_n==0: ...
```

这里没有 `return`、没有 `else`、没有优先级：

- 若实现按「normal 先选并 return」→ normal 非空时 canary **永远不可达**，退回 R6-B3 的原缺陷。
- 若两个 `if` 都执行 → 同一次 `Admit` 选了两个出口、加了两次 `inflight`，但只能返回一个 lease；另一个租约泄漏，`inflight` 永久偏高。
- 若实现按「canary 优先」→ 第 12 行的意图成立，但伪代码没有写，且没有测试「normal 非空 + canary 非空时只返回 canary」。
- 第 60 行 `canaryInFlight=true` 只在选中后设置；第 74 行 `Complete` 清；但 `Admit` 的两条路径谁先谁后没有定义。

**这是 Critical，不是文字瑕疵**：S1a 的实现者会在这里做出三种不同系统，而验收测试第 111 行只写「canary 单飞」，没有写「normal 非空时 canary 仍必须被选中」和「同次只返回一个 lease」。

### 4.2 190 个 5h 冷却的可用池曲线

S2 第 120 行只说「旧 NodeExhausted 迁移为 breaker strikes」，没有映射。两种合理解释给出完全不同的曲线：

| 时间 | 迁移为 `strikes=4, until=+5h`（5h 冷却） | 迁移为 `strikes=1, until=+5m`（平档） |
|---|---|---|
| t=0 | normal 22，canary 0，有效槽 88 | normal 22，canary 0（until 未到），有效槽 88 |
| t=5m | normal 22，canary 0（until 未到），88 | normal 22，canary 190（若 canary 优先），848 |
| t=5h | normal 22，canary 190（若 canary 优先；若 normal-first 则 0），88/848 | 已按 canary 结果收敛 |
| 全部 canary 2xx | 约 60–120s 内从 22 恢复到 212（每出口 2 次 2xx 且间隔 ≥60s；37 并发下约两轮） | 同左 |
| 全部 canary 429 | normal 22 保持；190 按 5m/30m/2h/5h 退避；有效槽 88 | 同左，但首轮更早 |

关键结论：

- **从 212 骤降到 22 是事实**；但 `22×K=88 > 37`，在 C=37 时不会立刻 503。所以「骤降」不等于「不可接受」，取决于 K 和并发。若 K=1，22<37 就会 503；若 C>88，也会 503。
- **真正的故障是自愈缺失**：如果实现按 normal-first，normal 22 永远非空，190 个 `strikes>0` 永远不会被 canary；即使 5h 到期，它们仍然不会进入 normal（`strikes>0`），也不会被选中，直到 22 个 normal 全部失败、candidate_n 变 0。也就是说「190 个 5h 冷却」可能变成「190 个永久冷却」，而计划第 140 行问的正是这个场景。
- 如果按 canary 优先，190 个健康出口可在约 2 分钟内恢复；如果全部 429，则保持 22 normal + 190 退避，有效槽 88，能撑 C=37，但不会恢复。这个曲线是「可接受」还是「不可接受」取决于是否要求 212 全恢复；计划没有定义验收阈值。

### 4.3 S2 迁移与 `recordProbeSuccess`

当前代码 `proxy_node.go:1664` 的 `recordProbeSuccess` 会把 `NodeProbation` 提升为 available（探测即证据）；v4.1 S2 要删除这个提升，但：

- 删除后，旧 `NodeProbation` 的 2xx 真实成功路径由谁处理？计划没有写。
- 旧 `NodeExhausted` 的 `CooldownUntil` 是 5h；迁移到 `strikes=4/until=+5h` 还是 `strikes=1/until=+5m`？没有写。
- `456256f` 已经引入 mark 幂等和 probation；S2 若删除提升，必须保留「2xx 证据 → available」的等价路径，否则旧 probation 会卡住。

这三项合起来，使 B3 的「190 个出口恢复」无法从计划直接实现。

## 5. B4：60s 全局探测 + 每出口 8 次/24h

### 5.1 两个约束的数学冲突

全局探测是「全 blocked 时每 60s 最多 1 次」（计划第 89 行）；每出口硬预算 8 次/24h（第 42/92 行）。对 N 个全 blocked 出口：

| N | 全局探测上限/日 | 不封顶时每出口/日 | 封顶 8 次耗尽时间 | 最后一次封禁到期（从 t=0） | 轮询全部出口 |
|---:|---:|---:|---:|---:|---:|
| 3 | 1,440 | **480**（阈值 8 的 60 倍） | 24min | **24h24m** | 3min |
| 4 | 1,440 | 360 | 32min | 24h32m | 4min |
| 22 | 1,440 | 65.5 | 176min | 26h56m | 22min |
| 212 | 1,440 | 6.8 | 24h 内不会耗尽 | — | **212min** |

- 若硬预算生效：N=3 在 24min 后三个出口全部 `until=now+24h`，之后**没有任何探测**；上游在 25min 恢复也要等到第二天。这与第 91 行「≤2 分钟」直接矛盾。
- 若硬预算不生效：N=3 每出口 480 次/日，是阈值 8 的 60 倍；计划第 92 行「单 IP ≤8」不成立。
- 若 N=212：每出口 6.8 次/日，预算不超；但 1 次/分钟轮询全部出口要 **212 分钟**，不是 2 分钟。只有「上游恢复是全局的，且下一次探测恰好命中已恢复出口」才可能 ≤60s；per-egress 的 shape drift 不保证这一点。
- 所以「60s 全局探测 + 8/24h」与「≤2min 恢复」不是调参关系，是**约束冲突**；必须改一个：提高预算/只对全局探测豁免、增加探测并发、或接受更长恢复时间。

### 5.2 `badSends24h` 没有滚动/衰减

计划第 41 行写「窗口 24h 滚动」，但状态只有 `badSends24h int`（第 30 行），没有时间戳环、没有每日清零、没有 `now-24h` 的减法。后果：

- 8 次 429 后 `until=now+24h`；24h 后 `until` 过期，但 `badSends24h` 仍是 8；下一次 429 立即再次 `until=now+24h` → **永久 24h 封禁**。
- 即使上游已恢复，只要该出口再出现 1 次 429，就重新封 24h；这比 5h ladder 更糟。
- 需要 `badSends24h` 的 24h 滑动窗口（时间戳数组）或显式每日重置；并且 canary/global 失败是否计入要写清楚。

### 5.3 选择顺序与轮转

计划第 62 行「选 `until` 最早者」；R6 建议「按 `lastHit` 最旧优先」。两者不同：

- 如果所有 blocked 出口的 `until` 相同，`until` 最早无法区分，可能每次选同一个；如果失败后 `until` 被正确推后，才会轮转。
- 但 §2.3 已指出单次 canary/global 失败可能不推后 `until`；此时同一个出口会被每 60s 反复选中，预算瞬间烧穿，其他出口永远不被探测。
- 必须用 `lastHit`/`lastProbe` 做 tie-break，并测试「N=3 全 blocked，连续 3 次探测命中不同出口」。

### 5.4 `1,696/日` 的口径

`8×212=1,696` 是「212 个出口各 8 次」的总上界，算术没错；但：

- 60s 全局探测单独最多 1,440/日，不是 1,696；1,696 只有在 canary/normal 也发送时才可能达到。
- 这个上界不能证明「≤2min 恢复」；它只证明「总量不超过 1,696」。两者是不同维度，计划把它们写在同一段，容易让读者以为预算同时保证了恢复速度。

## 6. Q1：`resets24h<3`、每日重置与永久降级

### 6.1 永久排除的具体路径

计划第 79-81 行：

```text
if canaryOK>=2 && now-lastCanaryOK>=60s:
   if resets24h < 3: strikes=0; until=zero; resets24h++
   else: strikes=max(strikes-1,0); until=now+ladder[strikes]
```

取 `resets24h=3`、`strikes=1`、canary 成功 2 次且间隔 ≥60s：

- 走 else：`strikes=max(0,0)=0`；`until=now+ladder[0]`。
- `normal` 的条件是 `strikes==0 && until.IsZero()`；此时 `until` 非零 → 不是 normal。
- `canary` 的条件是 `strikes>0`；此时 `strikes=0` → 不是 canary。
- 该出口既不在 normal，也不在 canary；如果还有其他 normal，`candidate_n != 0`，全局探测也不会选它；它**永久不服务**。
- 若 `ladder` 是 1-based，`ladder[0]` 未定义/越界；若 0-based 且 `ladder[0]=5m`，5 分钟后 `until` 仍非零，仍然永久排除。两种解释都错。

这是「健康节点被永久降级」的直接证明，不是理论担心。Q1 的答案是：**会，而且不需要 4 天，只要一次 2xx 就进入永久状态。**

### 6.2 每日配额重置与 `resets24h`

- 计划没有定义 `resets24h` 在什么时候归零：是自然日、滚动 24h，还是进程生命周期？如果是进程生命周期，3 次 reset 后用尽，之后只能每天衰减 1 级；`strikes=4` 要 4 天才能回到 0，期间每个 canary 都受 5m/30m/2h/5h 约束。
- 每日配额重置（上游 reset）不会自动清 breaker 的 `strikes`/`until`；一个上游已恢复的健康节点，仍可能因为 breaker 历史被降级。计划没有「上游 reset 信号 → 清 strikes」的路径。
- `canaryOK` 在 429 时没有清零；`lastCanaryOK` 的更新条件也没写。一个 canary 先成功、后失败、再成功，可能用两次非连续成功就满足「2 次 2xx」；这不是「无证据」，但语义不清。
- 需要：`if strikes==0 { until=zero }` 无条件执行；`resets24h` 明确自然日/滚动窗口；`canaryOK=0` on 429；`lastCanaryOK` 只在 canary/global 2xx 更新；测试「4 次 reset 后健康节点 5m 内回到 normal」。

## 7. Q4/Q5：租约、defer、跨出口重试、off 顺序

### 7.1 租约

计划第 15 行只有一句「租约 TTL 10min + defer 释放 + 跨出口重试先释放旧租约」；S1a 第 111 行有「租约泄漏、跨出口重试释放」测试，但没有规格：

- **sweep 间隔**：R6 Q4 要求 ≤TTL/2；v4.1 没写。若 sweep 10min 一次，租约最多 20min 才回收；若 5min，长流可能被误回收。
- **幂等 Complete**：`defer Complete` + 跨出口显式释放可能调用两次；没有 `sync.Once`/lease generation 时，`inflight--` 两次 → 负数 → 容量被高估，K 失效。这比泄漏更危险。
- **panic/early return**：`Admit` 后到 `Complete` 之间任何 early return 都要 defer；计划只说 defer，没有要求 handler 所有路径。
- **长流续租**：>10min 的流需要 heartbeat/renew；计划没有。生产只有 3 条短流，不能证明。
- **跨出口重试**：旧 lease 必须在 `Admit` 新出口前 `Complete`；否则旧出口 inflight 永久 +1。计划只有 prose，没有伪代码/测试。
- **direct 回退**：`egress.Get` 的 direct 结果没有 lease；如果 `Admit` 只包 node 路径，direct 路径的 inflight 语义要单独定义。

### 7.2 `off` 顺序

计划第 16 行说 `EGRESS_BREAKER=off` 插在 `quotaHaltGuard.gate` 之后，不绕过 pool403/quota_halt。方向正确，但需要精确到代码：

- `main.go:2878` 是 quotaHalt gate；`main.go:2946` 是 poolHalted；`main.go:2964-2965` 是 `blockedEgress` + `egress.Get`。`off` 若插在 2878 之后、2946 之前，仍然会经过 poolHalted；若插在 2965 之后，会绕过 `blockedEgress` 但仍可能经过 `egress.Get` 的 direct。
- `Admit` 第 56 行有 `pool403Halted` 检查；off 走旧路径时也必须保留 poolHalted 检查；否则「一处放行、另一处拒绝」。
- `main.go:3329-3344` 直连 429 → `quota_halt`；off 不应绕过这个路径。需要矩阵测试：`off × pool403 × quotaHalt × direct429`。

### 7.3 `egress.go:143` 的直连回退

这是 Q5 的核心，不是 off 的细节：

```go
if proxyPool.nodeCount() > 0 {
    n := proxyPool.pickForRegionExcluding(...)
    if n != nil { return ... }
    return EgressResult{Client: eg.direct, Direct: true} // 全部冷却中 → 直连
}
```

- 当所有出口都被 breaker blocked，`Admit` 认为 `candidate_n==0`，应触发全局探测；但 `egress.Get` 直接返回 direct，根本不经过 `Admit`。
- direct 请求 429 后，`main.go:3329` 进入 `quota_halt`；这正是「全 blocked 时直连同一个 IP 空转」的旧行为。
- 所以 v4.1 的 B4 全局探测在真实全 blocked 场景可能**永远不会执行**；I1「不钉在已知 429 出口」也被 direct 绕过。
- 必须把 direct 回退移进 `Admit`（作为显式的 `kind=direct` 候选，受 pool403/quota_halt/预算约束），或在 `egress.Get` 之前拦截；S1b 计划没有写。

## 8. Q6：8 条不变量与真实能力缺口

### 8.1 逐条对照

| 不变量 | v4.1 状态 | 说明 |
|---|---|---|
| I1 不钉在已知 429 出口 | ⚠️ 部分 | normal 排除 blocked；但 `egress.go:143` 全冷却直连绕过 Admit；canary/global 是允许的探测，不算钉死 |
| I2 抑制留痕 | ⚠️ 部分 | 计划要求 `suppressed_by`/`would_be_egress`/`model`/`candidate_n`；当前 `CallEvent` 有 `SuppressedBy`/`WouldBeEgress`/`SuppressedCandidates`/`Model`，**没有 `candidate_n` 字段**；需要统一 schema 并测试 0 上游 + 匹配 suppressed 事件 |
| I3 恢复必须有证据 | ❌ 未实现 | B3 canary 选择歧义；Q1 `until` 永久非零；canary 单次失败不升档 |
| I4 mark 幂等 | ✅ 已修 | `456256f`；但 S2 删除 `recordProbeSuccess` 提升后，2xx 证据路径要重新定义 |
| I5 无全局闩 | ✅ 方向正确 | 删除 429 全局闩；60s 全局探测是单令牌，不是闩；但 direct 回退会绕过它 |
| I6 fail-fast | ✅ 基本 | `all_inflight`/`no_candidate` 503；但 N=4 时 48–61% 是真实容量不足，不是「假 503」；需要明确验收 |
| I7 429 不进质量 | ✅ 已核实 | `node_scoring.go:947`；本方案不改 |
| I8 未尝试 ≠ 失败 | ⚠️ 部分 | suppressed 与 attempt 分开；direct 回退会把「未尝试」变成「尝试直连」，语义要写清 |

### 8.2 v4.1 仍缺的真实能力

1. **S2 迁移映射**：`NodeExhausted` 的 5h 冷却如何变成 `strikes`/`until`？`NodeProbation` 删除后 2xx 成功路径是什么？没有这两条，190 个出口的恢复曲线无法实现（§4）。
2. **重启持久化**：breaker 全在内存；重启后 `strikes/until/badSends24h` 全丢。R6 Q6 要求至少持久化 `strikes/until` 或给出重启验收；v4.1 没有。R5-5 的「双文件 stale」问题通过删除 gate_state 解决了，但变成了「重启全丢」。
3. **403 策略**：`pool403` 是全局 halt，403 不计入 breaker；如果 403 风暴持续，T2 halt 会一直 503，v4.1 没有恢复/降级策略。计划第 17 行说「保留 T2」，但没有说 403 与 breaker 的交互。
4. **direct 回退**：见 §7.3；这是 B4 和 I1 的直接漏洞。
5. **suppressed schema**：计划第 99 行要 `candidate_n`，代码没有；`at`/`reason` 也没有统一；没有「latch 期间 0 上游调用 + 每个决策都有 suppressed 事件」的测试。
6. **租约/长流**：见 §7.1；S1a 测试只有「泄漏/跨出口」，没有 TTL 边界、双 Complete、长流续租。
7. **指标/告警**：`all_inflight` 比例、canary 成功率、`badSends24h` 接近上限、全局探测连续失败都没有指标；事故中无法判断是「容量不足」还是「breaker 误封」。

### 8.3 18:03 / 00:20 证据是否被覆盖

- **18:03**：22 个 distinct egress，`4ad1982e` 20 个 429 尝试在 0.829s 内；v4.1 按 attempt 会在第 3 个封它，但 19 个请求已换出口成功。计划第 131 行「有 2xx 的出口不被误封」与第 50 行「4ad 不被封」冲突：`4ad` 有 219 个 2xx（虽然都在 429 之前），按 attempt 仍会被封。必须先定义「有 2xx」的时间窗口和计数单位。
- **00:20**：`41d39652` 171 个 429，前 3 个在 0.392s 内，v4.1 第 3 个封，符合预期；但封后 N=3，K=4 容量 12 < 并发 31，B2 的 503 问题暴露。`27436cb4` 1 个 429，不封，符合预期。
- **190 个 5h**：v4.1 没有迁移映射，也没有 canary 优先级测试；这是 B3 的核心场景，未覆盖。
- **00:20:58 闩死**：v4.1 删除了全局闩，方向正确；但它用 breaker + canary 替代，必须保证「一个出口 429 不封全池」和「全 blocked 有全局探测」；direct 回退会破坏后者。

## 9. 裁决与 blocker 清单

### 9.1 裁决

**仍需修订（NO-GO）。**

不是「v4.1 方向错」，而是「v4.1 还不能作为 S1a 的施工图」：

- B1 的计数单位、ring、canary 失败语义没有钉死；`4ad1982e` 的验收在按 attempt 时必然红。
- B2 的 K=4 只对 N=22 成立；事故时 N=4/3，503 是真实容量不足，计划没有策略。
- B3 的 canary 伪代码没有唯一裁决；190 个 5h 出口可能永久不恢复。
- B4 的 60s + 8/24h 与 ≤2min 恢复数学冲突；`badSends24h` 没有滚动。
- Q1 的 `until` 在 `strikes=0` 时不清零，健康节点可永久排除。
- Q4/Q5 的 direct 回退、租约幂等、off 矩阵仍是 High 风险。
- Q6 的 S2 迁移、重启持久化、403 策略、suppressed schema 未关闭。

### 9.2 必须关闭的 blocker

| 编号 | 关闭条件（可验收） |
|---|---|
| R7-B1 | 定义 attempt/request 计数单位；用 60s 桶替代 ring[8]；`4ad1982e` 在选定单位下的期望写死并测试；canary/global 单次 429 必须 `strikes++`；补「3 个 429 被 8 个 2xx 隔开」测试 |
| R7-B2 | 验收矩阵覆盖 `(N,C) = (22,37),(4,31),(3,31),(3,37)`；明确 N<8 时 503 是容量不足还是需要 K/队列；`all_inflight` 重试速率上限与 jitter |
| R7-B3 | `Admit` 明确 canary 优先级/唯一返回；`normal 非空 + canary 非空` 只返回 canary；190 个 5h 的迁移映射写死；2xx 恢复曲线测试 |
| R7-B4 | 二选一：全局探测豁免 8/24h（并给烧 IP 上界）或接受恢复上界；`badSends24h` 改 24h 滑动窗口；`lastHit` tie-break；N=3/22/212 的恢复时间表 |
| R7-Q1 | `strikes==0 ⇒ until=zero`；`resets24h` 明确每日/滚动；`canaryOK` 429 清零；健康节点 4 次 reset 后回 normal 的测试 |
| R7-Q4/Q5 | lease 幂等 `sync.Once`、sweep ≤TTL/2、长流续租、跨出口先释放；direct 回退移入 `Admit`；off × pool403 × quotaHalt × direct429 矩阵 |
| R7-Q6 | S2 迁移表；breaker 至少 `strikes/until` 持久化或重启验收；403 策略；suppressed `candidate_n` schema + 0 上游测试 |

### 9.3 什么情况下可以进入 S1a

当且仅当：

1. 上述 5 个 Critical 全部有文字规格 + 可执行验收测试；
2. 2 个 High 至少给出实现位置和测试矩阵；
3. 计划第 50/111/131/132 行的验收断言与真实数据一致（尤其 `4ad1982e` 和 N=4）；
4. 不再出现「一处放行、另一处拒绝」：direct、off、pool403、quotaHalt 四条路径有唯一裁决。

## 10. 若按修订进入 S1a：顺序、验收命令、翻车点

### 10.1 建议顺序（v4.2）

| 步骤 | 内容 | 依赖 | 回滚 |
|---|---|---|---|
| S0 | 冻结数据夹具：`4ad1982e`/`27436cb4`/`41d39652`/190 节点；定义 attempt/request 单位；修正计划第 50/111/131 行 | 无 | 文档 |
| S1a | `breaker.go` 纯状态机：60s 桶、K、canary 优先级、Q1 until 清零、lease 幂等；**不接线** | S0 | 删文件 |
| S1b | `Admit` 接入两条 main 循环；direct 回退移入；off/pool403/quotaHalt 矩阵；sweep/长流 | S1a | 开关 |
| S1c | 全局探测、24h 滑动预算、S2 迁移映射、事件 schema | S1b | 开关 |
| S2 | `NodeProbation`/`recordProbeSuccess` 收尾；持久化；403 策略 | S1c | 回滚迁移 |
| S3 | per-model 桶（仅当 Q2 的 0.6% 变 >5%） | S2 | 不做 |

### 10.2 每步验收命令

```bash
cd /home/jingyx/project/opencode2api-test
export PATH=$PATH:/usr/local/go/bin
export GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"

# 编译/格式/静态
gofmt -l breaker.go main.go egress.go proxy_node.go quota_halt.go
go build ./...
go vet ./...

# 定向（新测试名可按实际调整；关键是断言内容）
go test -run 'TestR7_B1|TestR7_B2|TestR7_B3|TestR7_B4|TestR7_Q1|TestR7_Q4|TestR7_Q5' -count=1 -v ./

# 全量（IP4RV 是已知独立失败）
go test -skip 'TestIP4RV' -count=1 ./...

# 反事实回放（建议落成脚本，固定 fake clock）
python3 .research/scoring2/r7_replay.py \
  --log /tmp/cl-1026b.jsonl --log /tmp/cl-1027b.jsonl \
  --assert 4ad_not_sealed_by_attempt=false \
  --assert 41d_sealed_at_third=true \
  --assert n4_all_inflight_le_5pct=false
```

验收断言必须写成表：

- B1：`4ad` 按 attempt 第 3 个封（若这是选择）；按 request 仅 1 个不封（若这是选择）；二者只能选一个并写进计划。`27436` 不封；`41d` 第 3 个封。
- B2：`(22,37)` 0 假 503；`(4,31)` 若 K=4 则 15 个 `all_inflight`，计划必须明确这是容量不足还是需要 K=8/队列；`(3,31)` 19 个。
- B3：`normal 非空 + canary 非空` 只返回 canary；190 个 5h 在 t=5h 进入 canary；全部 2xx 时 ~2min 回到 212；全部 429 时保持 22 normal。
- B4：N=3/22/212 的恢复时间表；`badSends24h` 滑动窗口；全局探测轮转。
- Q1：4 次 reset 后 `strikes=0`、`until=zero`、5m 内 normal。
- Q4：panic/early return/double Complete/11min 流/跨出口五条路径。
- Q5：off × pool403 × quotaHalt × direct429 四象限。

### 10.3 最可能翻车的 3 个地方

1. **B1 的窗口/单位**：`ring[8]` 与「60s 内 ≥3」不等价；`4ad1982e` 的 20 个 429 会被算成 3 个或 20 个，取决于 `Complete` 的调用点。实现时最容易在「每次 attempt Complete」和「请求结束 Complete」之间摇摆，导致同一份日志跑出「封/不封」两种结果。
2. **B3 + Q1 的状态机**：canary 优先级、`canaryInFlight`、`strikes` 降档、`until` 清零、`resets24h` 上限，五个变量互相耦合；任何一个分支漏写，就会出现「canary 永远不跑」或「strikes=0 但 until 非零」的永久排除。190 个 5h 出口会把这个问题放大到全池。
3. **B4 的预算/恢复**：60s 单令牌 + 8/24h 硬预算 + per-egress 429 的恢复，三者不可能同时满足 ≤2min；实现时若为了「不烧 IP」加 cap，就会在 N=3 时 24h 不探测；若为了「快速恢复」绕过 cap，就会 480/IP/日。必须先做策略选择，不能留给 S1c。

第 4 个风险是 **direct 回退**：`egress.go:143` 让全 blocked 直接绕过 `Admit`，B4 的全局探测和 I1 都会失效；这行代码在 S1b 必须显式处理，否则前面所有 breaker 逻辑在事故场景都可能不执行。

## 11. 复现命令与数据口径

### 11.1 关键数字

```bash
python3 - <<'PY'
import json, datetime as dt, collections
rows=[]
for p in ['/tmp/cl-1026b.jsonl','/tmp/cl-1027b.jsonl']:
    for line in open(p):
        r=json.loads(line); r['_t']=dt.datetime.fromisoformat(r['ts']); rows.append(r)

def window(a,b):
    rs=[r for r in rows if dt.datetime.fromisoformat(a)<=r['_t']<dt.datetime.fromisoformat(b)]
    ev=[]
    for r in rs:
        ev.append((r['_t'],1)); ev.append((r['_t']+dt.timedelta(milliseconds=r.get('duration_ms') or 0),-1))
    ev.sort(); cur=mx=0
    for _,v in ev:
        cur+=v; mx=max(mx,cur)
    print(a,b,'records',len(rs),'ok',sum(r['status']=='ok' for r in rs),
          'fail',sum(r['status']=='fail' for r in rs),
          'distinct',len({n for r in rs for n in (r.get('nodes') or [])}),'peak',mx)

window('2026-10-26T18:00:00+08:00','2026-10-26T23:00:00+08:00')
window('2026-10-27T00:20:00+08:00','2026-10-27T00:24:00+08:00')
PY
```

### 11.2 `4ad1982e` 的 429 尝试

```bash
python3 - <<'PY'
import json
n='4ad1982e75f81f0427b298af7b078754'
for p in ['/tmp/cl-1026b.jsonl','/tmp/cl-1027b.jsonl']:
    for line in open(p):
        r=json.loads(line)
        if n not in (r.get('nodes') or []): continue
        for e in r.get('events') or []:
            if e.get('type')=='upstream_error' and '429' in e.get('detail','') and e.get('node')==n:
                print(e['at'],r['req_id'],r['model'],r['status'])
PY
```

### 11.3 数据缺口

- 生产 N 未知；212 是计划数字，不是日志事实。
- 流式样本只有 3 条，长流/租约行为未验证。
- `4ad1982e` 的 219 个成功全部在 18:03:55.108 之前；不能把它说成「429 之后仍有 219 个成功」。
- R6 的「6 次 429」是 6 条失败记录（其中仅 1 条 429）；本报告用尝试级 20 个 429 重新计算。
- `41d39652` 的 171 个 429 与 R6 的 165/167 有差异，本报告以日志事件计数 171 为准；差异可能来自窗口截断/去重口径，需在回放脚本中固定。
