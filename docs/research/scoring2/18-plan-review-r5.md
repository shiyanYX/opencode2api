# R5 对抗复审 · 节点系统 v3.2

> 输入：`docs/superpowers/plans/2026-10-27-node-system-v3.md`（v3.2，147 行）、`docs/research/scoring2/16-plan-review-r3.md`（D1–D8）、`docs/research/scoring2/17-plan-review-r4.md`（B1–B6，342 行）。
> 代码基线：HEAD `465f5df` 只改文档；实际代码 = `9c3cc59`（`git diff 9c3cc59 HEAD -- '*.go'` 为空）。评审前工作区仅有未跟踪的 `.research/`、`docs/mockups/`、`zz_ip4v_layout_new.go`，无生产代码改动；本报告只新增 `docs/research/scoring2/18-plan-review-r5.md`。
> 约束：只读评审，不改代码、不 commit、不启动服务。

## 0. 结论（先给）

**仍需修订（NO-GO），不可按 v3.2 进入 S1a 实现。**

v3.2 把 R4 的六个问题都写进了文档，但多数只写到「要什么」，没有写到「怎么保证」；其中 B1（事务/竞态）、B2（条件化 M/D 与选路顺序）、B3（per-pair 首次触发与并发）仍是 blocker，B4/B5/B6 有可实施性缺口。逐条见 §2–§7。

### 0.1 Blocker（按严重度）

| ID | 严重度 | Blocker | 最短证据 |
|---|---|---|---|
| R5-1 | Critical | `Snapshot` 在选路前调用，却要求 `GateInput.PairChosen`；`Commit` 无版本/无返回值，「CAS」无法实现，发送前只复核 pool403 | §1.1、§1.2；`egress.Get` 在 gate 之后才选出口（`egress.go:123`） |
| R5-2 | Critical | 条件化 M 仍会在「所有 node pair 被 quota 阻塞但 direct 可用」时 429；S=0 与 direct 回退互相矛盾；32 行矩阵仍是 R4 的旧 M 语义 | §2.2；`egress.go:143` 全冷却回退 direct |
| R5-3 | Critical | per-pair 首次 FUL 即 5m 冷却，与「同一 pair 10min 内第 3 次才触发」互斥；167 并发无首次单飞，仍会全部发出 | §2.3；状态机自相矛盾 |
| R5-4 | Critical | 24h 预算把「FUL」计入硬排除，普通配额耗尽即可把健康 egress 锁 24h；预算窗口/成功是否计数/解锁路径未定义，滑窗下可自锁 | §2.4；R4 §8.4 已警告 |
| R5-5 | Important→Critical | `gate_state.json` 与 `proxy_state.json` 无跨文件事务、无 epoch/合并顺序；回滚旧二进制后 stale gate_state 会复活；off 的 HEAD 语义/差异未定义，golden 当前不可复现 | §2.5；两份文件独立原子写 |
| R5-6 | Important | meter 的 attempts 在 `Commit` 记、但「真正发送」在 Commit 之后；无结果回写 API；`Now` 只进 gate/pair/meter，pool/quota/egress 仍用 `time.Now`，5h 虚拟时间不可做 | §2.6；`Commit(d,now)` 无 outcome 参数 |
| R5-7 | Important | S1a 要实现「M=所有 pair blocked」但 pair 状态在 S1b；S1a 的 32 行矩阵/100 单飞/全局 token 也跨片；切片不能独立回滚/上线 | §3；S1a/S1b 依赖倒置 |
| R5-8 | Important | 32 行矩阵、golden 回放、单出口 167 第 3 次、重启不清 blocked 的验收断言未定义到可自动化；变异 ≥6 无清单 | §4；计划 §3 |

### 0.2 B1–B6 逐条核对总表

| R4 | v3.2 写法 | R5 裁决 | 关键缺口 |
|---|---|---|---|
| B1 | Snapshot/Decide/Commit + 同锁/版本 + 发送前复核 pool403 | **未闭环** | `PairChosen` 在 Snapshot 前不可知；Commit 无 version/error，CAS 无法表达；只复核 pool403，不覆盖 pair/global；三把锁无锁序 |
| B2 | M=所有 pair blocked；D 仅 direct；S=0 503 | **未闭环** | M 未纳入 direct（direct 可用仍 429）；S=0 与 direct fallback 三种解释；32 行矩阵仍是 R4 旧 M 语义；canary/trial 维度缺失 |
| B3 | strikes 1→5m…；3/10min 触发；canary 单飞 | **未闭环** | 首击 5m 与 3/10min 互斥；167 串行第 1 次就停；并发无首次在途门；canary 成功/失败/超时映射缺失 |
| B4 | K=2/60s；24h 8 次 canary；24h 8 次预算硬排除 | **未闭环** | 普通 FUL 计入硬排除（false positive）；两个 8 是否同一计数未定义；窗口 reset/累计/成功计数未定义；配额重置不解锁 |
| B5 | gate_state + 原子写 + fail-open + off + 迁移 | **未闭环** | 两份文件无跨文件事务/恢复顺序；回滚后 stale gate_state 复活；off 的 HEAD 定义与代码结构缺失；旧 per-node→per-pair 不可判定；S1b 内存态与「重启不清」矛盾 |
| B6 | 60s 桶/600s distinct/512 cap/fake clock/instrumented transport | **部分** | Commit 记 meter 与「真正发送才记」矛盾；无 outcome 回写；clock 只注入 gate，pool/quota/egress 仍 time.Now；cap 溢出/600s map 上限策略缺失 |

## 1. 方法与边界

- 只做纸面/代码/状态机推演，不写实现、不改生产文件、不 commit。
- 代码事实以 `9c3cc59` 为准：`shape_drift.go`、`quota_halt.go`、`proxy_node.go`、`main.go`、`egress.go`、`proxy_state.json`。
- 计划中的 167/00:20 生产数字沿用 R3/R4 的数据边界；本报告不把它们当新证据。R5 的矩阵/时序是确定性构造，不是生产回放。
- 未启动服务、未跑生产命令；只读 `git`/`grep`/`sed`。

## 2. B1–B6 逐条裁决

### 2.1 B1：Snapshot/Decide/Commit 是否消除「快照到发送」竞态

**裁决：没有消除。** 见 §2.1.1–§2.1.4。

#### 2.1.1 具体时序反例（pool403 之外的层）

```text
t0  A: Snapshot(model=m, now) → PairChosen=X, PairBlocked=false, GlobalArmed=false
t1  B: X 返回 429 → Commit 把 pair(m,X) 置 blocked，strikes=1
t2  A: Decide(in) → Allow（in 是 t0 的快照，看不到 t1）
t3  A: Commit(decision, now) → 签名无 version/无 error；即使实现里查一次 pair，
        也无法知道 decision 是基于旧快照；若查到了，也只能返回 void（无法让 A 重试）
t4  A: 发送到 X —— 已 blocked 的 egress
```

`Commit(d GateDecision, now time.Time)` 没有 `Version`/`Epoch`/`error`；`GateDecision` 也没有版本。计划 §1.1 说「Commit 在锁内做 CAS」，但 CAS 需要「期望版本」，当前 API 无法表达。**这不是实现细节，是签名缺口。**

即使给 `Commit` 加版本并返回 error，`Commit` 与「真正发送」之间仍有窗口：t3 Commit 成功，t4 前另一个请求把 global armed 或 pool403 halt 拉起，A 仍会发送。计划只保留「发送前再复核 `Pool403Halted`」（§1.1 最后一条），没有复核 pair/global/canary；因此：

```text
t3.5 B: global six conditions 成立 → GlobalArmed=true
t4   A: 普通请求发送（不是 trial）→ 违反 §1.3「全局生效时除单飞探测外零上游发送」
```

**结论：** 要消除竞态，必须把「选路 + 准入 + 预留（lease/reservation）」做成一个原子操作，或在发送前复核**全部** deny 层并持有可撤销的 lease；仅 `Snapshot/Decide/Commit` 三段式不够。

#### 2.1.2 `PairChosen` 在 Snapshot 阶段不可知

`Snapshot(now, model)` 只有 model，没有候选/排除集/区域；而 `PairChosen` 取决于：
- 当前 sticky active 是否 eligible；
- `shapeDriftBlockedEgress` 的排除集；
- 区域过滤；
- `modelQuota`/pair blocked；
- `egress.Get` 的 direct fallback。

现有顺序是 `blockedEgress := shapeDriftBlockedEgress(...)` → `egress.Get(...)`（`main.go:2964-2965`），**选路在 gate 之后**。计划 §1.1 的 `Snapshot` 却要求 `PairChosen`，只有两种可能：
1. Snapshot 内部先选路：那它就不是纯快照，且选路结果会受 `Decide` 的拒绝影响（鸡生蛋）；
2. 调用方先选路再 Snapshot：那 `Snapshot` 的「同一把锁/版本」就不覆盖选路，仍有时序窗口。

计划没有给出 `PlanRoute`/`Select` 的签名，也没有说明 `PairChosen` 由谁填。**B1 的「GateInput 包含 PairChosen」只是把矛盾写进字段，没有解决。**

#### 2.1.3 「同一把锁/版本」跨三个子系统不可实现

`Snapshot` 需要的输入来自：
- `proxyPool.mu`（CandidateN/UnblockedN/Pool403Halted/节点状态）；
- `admission` 的 pair/global/meter（计划新增，锁未定义）；
- `quotaHalt.mu`（direct quota）；
- `egress` 的 sticky/active（选路）。

现有 `shapeDriftMu`、`quotaHalt.mu`、`proxyPool.mu` 是三把独立锁；计划没有锁序、没有版本号、没有「一个 gate 锁包住 pool 快照」的设计。实现者只能拼一个「近似快照」，边界测试（attempts=19/ratio=1 来自不同时刻）会 flaky。**B1 的原子性没有落地。**

#### 2.1.4 发送前复核只写 pool403

计划 §1.1 明确只「发送前再复核 `Pool403Halted`」。这保留了 T2 的双重检查，但把 pair blocked/global armed/canary token 的竞态留在原地。R5 攻击点 1 问的正是「尤其 pool403、pair blocked 变更」；计划只回答了 pool403。**B1 未闭环。**

### 2.2 B2：条件化五层是否自洽（32 行 + direct/canary/trial）

**裁决：不自洽。** 三个具体问题 + 矩阵不可复用。

#### 2.2.1 M 的「所有 pair blocked」仍会 false 429（direct 可用时）

设 model=m，节点 pair 全部因 quota 被 blocked，但 `DirectAvailable=true`、`DirectQuotaBlocked=false`、direct 本身健康。计划 §1.2 的 M 条件只看「该 model 的所有 pair 都 blocked」→ 429 `model_quota`，**即使 direct 可用**。direct 不是 pair，M 没有把 direct 纳入「可用路」判断。

这与 R4 行 1/3 是同一类错误：`D` 已改成「只在会落到 direct 时拒绝」，但 `M` 没有改成「所有 pair blocked **且** 没有可用 direct」。正确的条件应是：
```text
M = (所有 node pair blocked) AND (DirectUnavailable OR DirectQuotaBlocked)
```
或者把 direct 也建模成一个 egress pair，让 M 统计「所有 egress」而不是「所有 node pair」。计划没有写。

#### 2.2.2 S=0 与 direct fallback 互斥

计划 §1.2：
- S 层：`CandidateN==0`（含全部 blocked）→ 503 `no_candidate`；
- 同一节又说「禁止回退直连：`DirectAvailable=false` 时 `S=0` 直接 503」。

这两句只在 `DirectAvailable=false` 时一致。若 `DirectAvailable=true`、`CandidateN==0`：
- 按 S 层字面 → 503 no_candidate，direct 明明可用却被拒；
- 按「禁止回退直连」的限定 → 只有 DirectAvailable=false 才 503，那 DirectAvailable=true 时应允许 direct；
- 按现有 `egress.Get`（`egress.go:143`）→ 全冷却直接返回 direct。

三种解释三种行为。`CandidateN` 到底是否包含 direct、S 是否包含 direct，计划没有定义。**这是 B2 的核心缺口，不是文案。**

#### 2.2.3 32 行矩阵不能直接复用

计划 S1a 验收写「32 行交互矩阵（R4 附录 A）」。但 R4 附录 A 的 M 是「当前 pair 被 quota 拒」（R4 §2.1 明确按此解释），v3.2 把 M 改成「所有 pair blocked」；R4 自己说「如果 M 按所有 pair blocked 解释，则 M=1,S=1 是矛盾输入」。因此：
- 旧矩阵中 `M=1,S=1` 的行在新语义下**不应存在**；
- 旧矩阵的期望 `429 model_quota` 与「先选 B」冲突；
- 新语义下需要 `PairChosen`/`DirectRoute`/`Canary` 维度，不是 5 个 bool。

**S1a 的验收标准引用了一份与实现语义不一致的矩阵，测试无法写。** 必须重新生成矩阵（见 §2.2.5 建议维度）。

#### 2.2.4 direct/canary/trial 组合未覆盖

R5 要求「32 行之外含 direct、canary、trial 再穷举」。计划没有给：
- `DirectAvailable=true/false` × `DirectQuotaBlocked` × `CandidateN` 的组合；
- pair 处于 canary（cooldown 到期、待单飞）时，`PairBlocked`/`ModelAllPairsBlocked` 如何取值；若所有 pair 都在 canary，M 是否=1 而把 canary 自己 429 掉？
- global trial token 的 `TrialInFlight/TrialTokenValid` 与 pair canary 同时命中同一 egress 时谁先；
- `GlobalBlockedSet` 与 `CandidateN` 是否重复排除；
- canary 成功/失败的结果如何回写（Decide 只读，Commit 无 outcome）。

这些不是「再加 32 行」，而是状态空间多了一个 `RouteKind` 和两个 token 维度；计划没有定义。

#### 2.2.5 建议的新矩阵维度

至少 6 维（可分组表驱动，不要求 2^6 全穷举，但每维边界要覆盖）：
```text
P  pool403 halted
G  global armed
M  all node pairs quota-blocked
D  direct quota blocked
R  route kind: node / direct / none
C  chosen pair state: healthy / canary-eligible / blocked
T  trial/canary token: none / valid / in-flight-other
```
期望输出：`Allow/Reason/Status/RetryAfter/Probe/Canary/Lease`。至少覆盖：
- `M=1,R=direct,D=0` → 允许 direct（不能 429）；
- `M=1,R=none,D=0,DA=true` → 允许 direct 或明确 503（二选一写死）；
- `R=node,C=canary-eligible` → 只放 1 个 canary，其余 503/429；
- `G=1,T=valid` → 只放 trial，且 trial 绑定 egress/model；
- `G=1,R=none` → 503 no_candidate，不发 trial。

#### 2.2.6 扩展组合表（direct/canary/trial）

从 R4 的 32 行里抽出会被 v3.2 改语义的行，再加 direct/canary/trial：

| # | P | G | M | D | R | C | T | v3.2 字面 | 应有/问题 |
|---|---:|---:|---:|---:|---|---|---|---|---|
| E1 | 0 | 0 | 1 | 0 | direct | - | none | 429 model_quota | direct 可用却被 M 先拒；M 应含 direct |
| E2 | 0 | 0 | 1 | 0 | none | - | none | 429 model_quota | 若 direct 不可用，应 503 no_candidate（不是 429）；若可用，应 allow direct |
| E3 | 0 | 0 | 0 | 1 | node | - | none | 503 direct_quota（若 D 无条件） | D 只应在 R=direct 时拒绝；R=node 应 allow |
| E4 | 0 | 0 | 0 | 1 | direct | - | none | 503 direct_quota | 正确（direct 真被 quota 拒） |
| E5 | 0 | 0 | 0 | 0 | node | canary-eligible | none | 无 canary token，普通流量可能继续放行 | 冷却到期应只放 1 个 canary；`GateInput` 没有 `CanaryInFlight`/token 字段 |
| E6 | 0 | 0 | 1 | 0 | node | canary-eligible | none | 429 model_quota | 若「all pairs blocked」包含 canary，canary 被 M 锁死；M 应排除可 canary 的 pair |
| E7 | 0 | 1 | 0 | 0 | none | - | valid | 503 global_shape + probe | 无候选不能 probe；应 503 no_candidate / probe=false |
| E8 | 0 | 1 | 0 | 0 | node | canary-eligible | valid | 503 global_shape + probe | 两个 token 同时命中同一 egress 时谁先？计划未定义 |
| E9 | 0 | 0 | 0 | 0 | none | - | none | S=0 → 503 no_candidate | direct 可用（DA=true）却被 S 拒；与 `egress.go:143` 回退冲突 |
| E10 | 1 | 1 | 1 | 1 | node | blocked | valid | 503 pool403 | 正确（P 最高），但 SuppressBy 必须记录 G/M/D/S |

这 10 行里 E1/E2/E5/E6/E7/E8/E9 都是 v3.2 未定义或自相矛盾的组合；**不是再补 32 行能解决的，必须先加 `R/C/T` 维度和 token 归属。**

### 2.3 B3：per-pair 状态机能否让「单出口 167 连发」在第 3 次停

**裁决：不能；状态机自相矛盾，且并发下更差。**

#### 2.3.1 触发序列矛盾

计划 §1.4：
- `strikes: 1→5m, 2→30m, 3→2h, 4+→5h`；
- `触发排除：同一 pair 在 10min 内第 3 次 FUL（不等全局）`。

若第一次 FUL 就按 `strikes=1` 冷却 5m，则：
```text
t=0    第 1 次 FUL → strikes=1, until=t+5m, pair blocked
t=0..5m 后续请求看到 blocked；若这是唯一 pair，M=all pairs blocked → 429（不再发上游）
t=5m   canary（第 2 次 FUL 的机会）
t=5m+  canary 失败 → strikes=2, until=t+30m
```
对**初始/持续坏的 pair**，第 3 次 FUL 最早在 t=35m，**不可能落在「10min 内」**；真正生效的是「第一次 FUL 就 5m」。只有 canary 连续成功、`strikes` 被 reset 之后重新累计，才可能出现「10min 内第 3 次」——但那时 5m 首击已经先把同一波流量拦掉，3/10min 规则对 167 连发没有增量作用。**首击冷却与 3/10min 触发是两套互斥的初始语义，计划没有说明何时用哪套。**

若意图是「前两次只记录、不 block，第 3 次才 block」，那 `strikes 1→5m` 的 5m 从哪来？第 1 次 FUL 后到第 3 次之间，pair 仍是可用的，167 连发会继续打；需要在 10min 内累计 3 次，且这 3 次之间不能 block。计划没有写「前两次不 block」的状态。**两种读法都推不出一个可实现的单一状态机。**

#### 2.3.2 串行 167 的行为也不是「第 3 次停」

若按「首次 FUL 即 block」，串行 167 会在第 1 次就停（后续全部 429/suppressed），不是第 3 次。若按「第 3 次才 block」，则前两次会真的发出，第 3 次后停——这符合「第 3 次停」，但与 `strikes 1→5m` 的首次冷却冲突。**计划同时要求两个互斥行为。**

#### 2.3.3 并发 167 会在第 1 次响应前全部发出

现有 gate 流程是「先选路、后发送」，没有 per-pair 首次单飞：
```text
167 goroutines 同时 Snapshot：pair 未 blocked，全部 Decide=Allow
167 goroutines Commit：即使 Commit 串行化，它们都在任何 429 响应回来之前完成
167 次上游发送 → 之后才收到 429
```
计划只给 canary 加了「同一 pair 同时只允许 1 个 canary」（§1.4），没有给**首次 FUL 前**的普通请求加单飞/在途计数。因此「167 连发在第 3 次停」在并发下不成立；要满足，必须在 Commit 里对每个 pair 做 `inflight` 预留（首个请求占位，其余 fail-fast/等待），或把 167 个请求串行化。计划没有这个原语。

#### 2.3.4 canary 的 K=2/60s 与失败映射未定义

- 第一次 canary 成功后，pair 是否立即 available？若立即 available，普通流量会涌入，第二次成功不是 canary；若仍 blocked，第二次 canary 何时放行？计划只说「K=2 次成功且间隔≥60s 才 reset」，没有调度规则。
- 失败映射：429 才算失败？5xx/timeout 算不算 strikes+1？若算，一次网络抖动会升级冷却；若不算，canary 永远不失败。R4 §4.4 明确要求区分 2xx/429/5xx-timeout，计划没有写。
- canary 是否强制使用该 pair 的 egress？计划没有写「2xx 必须来自该 pair」；若选择器切到别的 egress，2xx 可能洗掉坏 pair（R4 §4.4）。
- 100 并发 canary 单飞：计划说「同时只允许 1 个」，但没有 token 释放/超时；请求崩溃/超时后 token 永久占用，pair 永久不能 canary。

### 2.4 B4：canary K=2/60s + 24h 8 次预算，配额每日重置下会不会永久锁死

**裁决：按计划当前写法，存在「永久锁死」或「至少锁过每日重置」的路径；必须先定义窗口/计数/解锁。**

#### 2.4.1 预算把普通 FUL 也算硬排除（false positive）

计划 §1.4：
> 24h 预算：统计所有「接触过 FUL 或 403-confirmed」的发送；达到 8 次 ⇒ 该 egress 硬排除 24h。

FUL 与普通配额耗尽不可区分（R3/R4 的前提）。一个健康 egress 只因某个 model 的普通配额耗尽、重试 8 次，就会被硬排除 24h；同 egress 上其他 model 也被连带。R4 §8.4 明确建议「只有 403/confirm 证据才允许 24h 硬排除，429-only 不硬排除」，v3.2 反而把 FUL 纳入。**B4 没有吸收 R4 的关键建议。**

#### 2.4.2 滑窗 + canary 失败 → 自锁

若「24h 内 8 次」按滑动窗口实现：
```text
t=0..22h35  8 次命中（初始 + 7 次 canary 失败）→ 硬排除至 t=46h35
t=24h       上游配额每日重置（节点实际已恢复）
t=46h35     预算窗口滑出旧命中 → 解锁，放 1 次 canary
            若 canary 失败 → 新命中，窗口内仍可能 ≥8（取决于旧命中是否全滑出）
```
在 8 次命中集中在 22.5h 内的情况下，解锁点旧命中刚好滑出，单次失败不会立刻回到 8；但若实现把 `windowStart` 固定为首次命中、或命中计数不清零，则会立即再次满 8 → 再锁 24h。**计划用 `{hits, windowStart}` 暗示固定窗口，却没有写窗口到期重置、成功是否清零、canary 成功是否计数；实现者可以写出「永久锁死」的版本。** 这是 blocker：语义未定义，不能验收。

#### 2.4.3 每日重置不构成解锁路径

即使预算最终会滑出，节点在配额重置后仍可能被锁最多 ~24h（8 次命中在重置前发生，硬排除到 8th+24h）。对「每日重置」的节点，这等于**错过一整个可用日**。计划把「配额每日重置」当作恢复场景，但没有给出「重置即解锁」的路径；解锁只靠预算窗口，与配额无关。运维需要知道：**配额重置不会清 gate_state/budget；只有窗口到期 + canary K=2 成功（或人工清状态）才恢复。**

#### 2.4.4 可接受的解锁路径（建议）

1. 预算改为**固定 24h 窗口**（`windowStart` 到期即 `hits=0`，明确重置），或滑动窗口但每次解锁只允许 1 次 canary，且 canary 失败不把窗口重新填满（按时间衰减）。
2. 硬排除只对 `403-confirmed` 生效；FUL/429 只影响 per-pair 冷却，不进 24h 硬预算。
3. canary 成功 1 次只进入 `verifying`，K=2 成功才清 strikes；成功不增加 budget hits。
4. 提供人工解锁：删除 `gate_state.json` 的对应 egress 条目（或 `SHAPE_DRIFT_GATE=off` 后重启）——但必须文档化「off 不清 blocked」还是「off 忽略 blocked」。
5. 在 `gate_state.json` 里记录 `budgetWindowStart` 与 `hits`，并在加载时校验 `now-windowStart>=24h ⇒ hits=0`。

### 2.5 B5：gate_state.json 与 proxy_state.json 崩溃/回滚不一致；off 是否回到 HEAD

**裁决：未闭环。** 两份状态没有合并协议，off 的「HEAD」定义不清。

#### 2.5.1 两份文件的写入不是跨文件事务

计划 §1.5 只保证每个文件「原子写（tmp+rename）」。但一次 429 可能同时：
- 写 `gate_state.json`（pair blocked/global/budget）；
- 写 `proxy_state.json`（节点状态/冷却）。

若在两次 rename 之间崩溃：
- gate_state 新、proxy_state 旧 → 新 pair block 指向的节点可能已被旧状态标为 available/dead；路由可能仍选它（pair block 是否进入选路？计划没说）。
- gate_state 旧、proxy_state 新 → 节点被标 exhausted，但 pair 没有记录；重启后 pair 状态丢失，节点冷却由 proxy_state 管，可能提前/延后恢复。

计划没有「恢复顺序」：先读哪个、以谁为准、如何 reconcile。**R5 攻击点 4 直接命中。**

建议的最小恢复顺序（需写进计划）：
1. 读 `proxy_state.json`（先校验 `version`；v1 节点状态是事实源）。
2. 读 `gate_state.json`；校验 `version` 和 `proxyStateGeneration`。若 generation 不匹配（旧二进制回滚过）→ **丢弃 gate_state 并告警**，不要复活 stale block。
3. reconcile：pair 的 `egressKey` 必须能在当前 pool/egress 配置中解析；解析不到 → 删除 orphan pair 并记日志；`proxy_state` 说节点 dead 而 pair blocked → 保留 pair（blocked 证据不因节点 dead 丢失），但不再选它；pair 已过期 → 删除。
4. 写回：每次状态变更先写 `proxy_state` 再写 `gate_state`（或反之），但**启动时以步骤 2/3 的合并结果为准**；`gate_state` 写失败不能回滚已写出的 `proxy_state`，下次启动按合并规则修复。
5. 回滚：旧二进制保存 `proxy_state` 时递增 `proxyStateGeneration`；新二进制看到不匹配即忽略 gate_state。**没有 generation 就无法区分「崩溃后旧文件」与「正常旧状态」。**

#### 2.5.2 回滚旧二进制后 stale gate_state 复活

- 部署新二进制 → 写 gate_state（pair blocked）。
- 回滚旧二进制：旧代码不读 gate_state，节点状态从 proxy_state 恢复；期间可能把节点用起来（旧逻辑没有 pair block）。
- 再升级新二进制：读到回滚前的 gate_state，把已经健康的 pair 再次 block。计划说「新增 gate_state 不塞进 proxy_state，避免回滚旧二进制丢节点状态」，但没有说**回滚期间 gate_state 应被忽略/失效**。需要 `proxy_state` 的 generation/epoch 与 gate_state 绑定：proxy_state 被旧二进制重写后，gate_state 的 epoch 不匹配 → 丢弃或迁移。

#### 2.5.3 off 不是 HEAD

计划 §1.5：
> `SHAPE_DRIFT_GATE=off`：走 HEAD 旧 shape 路径（per-model/3 distinct/TTL 清 seen），不绕过 pool403 与 quota_halt。

问题：
1. 当前代码没有 `SHAPE_DRIFT_GATE`（`grep` 只命中计划）；「HEAD 旧 shape 路径」是 `shape_drift.go` 的 `shapeDriftObserve429`/`shapeDriftObserveSuccess`，它们与 T1/T2/T3、probation、`recordNodeSuccess` 混在 `main.go` 两条循环里。S1a 要把循环改成 Admission；off 分支必须保留旧调用，但计划没有代码结构（是 `if gateOn { ... } else { old }`？还是 Admission 内部调用旧函数？）。
2. `shape_drift.go` 有随机 jitter（`shapeDriftJitterFn`，0–20%，虽是 var 可注入，但计划未要求固定）和真实时钟（`shapeDriftSnapshot` 调 `time.Now()`，`shapeDriftLastGlobalTrial` 是全局变量）；golden 回放要「发送数/抑制数/状态码一致」，必须固定 jitter/clock 并隔离全局 trial 状态。计划没有提这些测试注入。
3. 「HEAD」到底指 `9c3cc59`（含 T1/T2/T3/probation）还是 v3 之前的 HEAD？如果 off 只关 shape，T2 pool403/quota 仍在；如果 off 关整个 Admission，可能把 T2/T3 也关掉。计划只说「不绕过 pool403/quota」，没有说 T3/直连配额/probation 是否保留。
4. 动态切换：env 只在启动读？运行中 off/on 的状态如何迁移（旧 shape 状态与新 gate 状态互不兼容）？计划没有定义。

**off 要复现的旧行为（必须逐条列进 golden）**：
| 维度 | HEAD 旧 shape（`shape_drift.go`） | v3.2 新 gate |
|---|---|---|
| 键 | `model` | `(model, egress)` |
| 触发 | 3 个 distinct/10min | 全局六条件（5 distinct/5 consecutive/20 attempts/0.9） |
| 解除 | 窗口空 / 任一 2xx / 硬 TTL 10min **清 seen** | blocked 保留，TTL 只推 `nextProbeAt` |
| trial | 60→120→300s，全局 5s 节流 | K=2/60s canary + 全局单飞 |
| 抖动 | 0–20% `shapeDriftJitterFn` | 计划未定义 |

只要其中一行不一致，off 的 golden 就会「发送数/抑制数/状态码」对不上；计划只说「走 HEAD」，没有列差异，也没有说明 off 时 `shapeDriftLastGlobalTrial` 等全局状态如何隔离。

#### 2.5.4 旧 NodeExhausted 迁移不可判定

计划 §1.5：
> 旧 `NodeExhausted` 且 `LastError` 含 quota ⇒ 按新规则重算冷却（首次 5m），保留 dead / `GateStateRejected`。

- 旧 mark 是 **per node**，新状态是 **per (model, egress)**；一个旧 node mark 无法知道是哪个 model 导致的。映射到所有 model 会 false block；映射到 unknown 又不阻止该 model。
- `LastError` 不一定含 quota（R4 指出 190 个 +5h marks 无数据依据）；条件可能迁移 0 个，旧 5h 仍在。
- 迁移幂等性、迁移后是否清 `CooldownUntil`、是否写 gate_state 都没有定义。
- 保留 `GateStateRejected` 的节点在 `sweepExpiredLocked` 中 `gateReviveRequiresProbe` 为 true 时永不复位（`proxy_node.go:498`），需要与 S2 的 5m/30m/2h/5h 协调；否则旧 rejected 节点永久 dead。

#### 2.5.5 重启不清 blocked 与 S1b 内存态矛盾

计划 S1b 说「`gate_state.json` 结构（先内存，S1c 再落盘）」，但 §1.5 说「重启不清 blocked」。如果 S1b 单独上线，重启就清 blocked，违反产品约束；如果 S1b 不单独上线，那「每个切片一个可回滚提交」不等于「每片可部署」。计划没有给部署门（默认 off/feature flag/只合并不发版）。**半成品上线风险。**

### 2.6 B6：meter/clock/cap 是否可实现、内存上界、10 万 model、5h 虚拟时间

**裁决：部分可实现，但关键接口缺失。**

#### 2.6.1 attempts 的写入点自相矛盾

- §1.1：「meter 的 attempts 只在『真正发送』时记，抑制不记」。
- §1.1：「`Commit` 在锁内做 CAS 与状态迁移；meter 写入」。
- §1.6：「写入点唯一：`Admission.Commit`（两条 main 循环、direct/node、canary 全走同一接口）」。

`Commit` 发生在发送之前；如果 Commit 记 attempts，那么：
- 发送前被 `egress.Get` 失败/transport 建连失败/请求取消，attempts 已记但没发出 → ratio 分母污染；
- 发送成功但结果还没回来，attempts 已记（这是对的），但 429/2xx 结果没有 API 回写。

`Commit(d GateDecision, now)` 没有 outcome 参数，也没有 `ObserveResult`。计划没有定义「发送结果 → 更新 meter/pair/global」的入口。**B6 的唯一写入点无法同时满足「真正发送时记」和「结果回写」。**

#### 2.6.2 clock 只注入到 gate/pair/meter，不够 5h 虚拟时间

计划 §1.1：「所有 gate/pair/meter 函数接收 `Now`；测试用 fake clock，可跑 5h 虚拟时间。」

但现有代码：
- `proxyPool.pick*` 内部 `now := time.Now()`（`proxy_node.go:581` 等）；
- `hasEligibleNode`/`sweepExpiredLocked` 的调用方 `time.Now()`（`proxy_node.go:1930` 等）；
- `quotaHalt.halt`/`observeSuccess`/`allHaltedUnrecovered` 用 `time.Now()`（`quota_halt.go:110,131,176`）；
- `egress`/`main` 多处 `time.Now()`；
- `shape_drift.go` 旧路径用 `time.Now()`。

如果 5h 测试只推进 gate 的 fake clock，pool 的 `pick` 仍看真实时间，节点冷却不会按虚拟时间到期；`quotaHalt` 也不会恢复。**5h 虚拟时间不可做，除非给 pool/quota/egress/main 全部注入 Clock，或测试只覆盖纯 gate 状态机（但那就不是事故仿真）。** 计划只写「fake clock interface」在 S1a 文件清单，没有定义接口、注入路径、哪些函数改签名。

#### 2.6.3 内存上界与 10 万 model 测试

- 计划说 `modelCap=512` + 空闲 30min 清理，且「不得淘汰已 armed/blocked 的证据」。
- 若 512 个 model 都 blocked，新 model 无法入表；此时是拒绝新 model、允许其绕过 gate、还是溢出？计划没定义。若允许溢出，内存不设上界；若拒绝，gate 对新 model 失效。
- 10 万随机 model 测试只写「`modelCap` 生效且内存不增长」；没有定义度量（map len？heap？）、没有定义 blocked 保留的断言方式。可自动化，但计划没给测试形状。
- pair 数 = model × egress；512×213≈109k，每项若含 map/字符串，可能 >10MB；计划只给 64–96B 估算（R4），没有 heap profile 门槛。

#### 2.6.4 600s distinct map 上限 213+1

- 计划写 `map[egress]time.Time` 上限 213+1；当前节点约 212+direct，未来节点增多时上限会截断新 egress，`distinct_new_egress_429` 少计 → 全局更不容易 arm（fail-open）。
- 溢出策略（拒绝新 egress/淘汰最旧/动态 cap）未定义；如果淘汰最旧，可能把仍在 600s 内的 429 删掉，distinct 少计。
- `consecutive` 的「同出口重复不加不重置」需要状态机；计划只在 §1.3 一句带过，没有字段/测试。

#### 2.6.5 5h 仿真与「每 egress 发送次数 ≤ 预算」

- 计划 S1b 验收「5h 虚拟时间下每 egress 发送次数 ≤ 预算」。预算=8；但 canary 自身 8 次（初始+7）就正好 8，断言「≤8」几乎恒真，无法区分「正确 canary」与「没有 canary 但普通流量 8 次」。
- 应断言**精确序列**（例如 0/5m/35m/2h35/7h35/12h35/17h35/22h35 各 1 次，无其它发送）和每 egress 的 `suppressed` 原因。
- instrumented transport 能统计发送，但 `bunEgressResult` 会替换 client（`main.go:2976`），需要在替换前/传输层埋点；计划没有说埋点位置。

### 2.7 确定性构造验证（独立于生产日志）

为避免只做文字推演，用 5-bool 枚举 + 状态机模拟跑了一轮（Python，一次性脚本，不写仓库）：

| 构造 | 结果 | 含义 |
|---|---|---|
| 32 行 5-bool 枚举（P>G>M>D>S） | `M=1,S=1` **8 行**；`M=1,D=0,S=0` **4 行**（其中 `P=G=0` 1 行） | 新 M 语义下 `M=1,S=1` 是矛盾输入；`M=1,D=0,S=0` 在 M 层直接 429，direct 可用也救不回来 |
| 串行 167，首次 FUL 即 5m 冷却 | 发送 **1** 次，之后全部 suppressed；没有第 3 次 | 与「第 3 次停」不符 |
| 串行 167，仅第 3 次 FUL 触发 | 发送 **3** 次（t=0/1ms/2ms），第 3 次后 block | 与 `strikes 1→5m` 不符 |
| 8 次 canary 命中（0,5m,35m,2h35,7h35,12h35,17h35,22h35） | 硬排除至 **46h35**；滑窗在解锁点 24h 内命中 **0** 次 | 滑窗实现不会永久锁死；固定窗口/累计实现会立即再锁；配额 24h 重置不解锁 |

结论：B3 的两种读法各自能跑，但**不能同时满足**计划的两句话；B4 的「永久」取决于未定义的窗口/累计语义，不能验收。

## 3. 附加攻击 1：自相矛盾

| # | 矛盾 | 位置 | 后果 |
|---|---|---|---|
| C1 | 「重启不清 blocked」vs S1b「先内存，S1c 再落盘」 | §1.5 vs §2 S1b | S1b 单独上线重启清空；不单独上线则切片不可部署 |
| C2 | 「M 所有 pair blocked」vs S1a 无 pair 状态 | §1.2 vs §2 S1a/S1b | S1a 无法实现 M；只能先用旧 modelQuota，语义不同 |
| C3 | 「首次 FUL 5m」vs「10min 内第 3 次触发」 | §1.4 | 第 3 次永远不触发；167 第 3 次停不可能 |
| C4 | 「24h 8 次 canary 上限」vs「24h 预算 8 次硬排除」 | §1.4 | 两个 8 是否同一计数未定义；若同一，canary 自身打满预算；若不同，实际可达 16 次/24h，超出 R4 安全上限 |
| C5 | 「off 走 HEAD 旧 shape」vs S1a 重写 main 循环 | §1.5 vs §2 S1a | off 分支必须保留旧调用；计划无结构，易绕过 T2/T3 |
| C6 | 「meter 只在真正发送记」vs「Commit 写 meter」 | §1.1/§1.6 | Commit 在发送前，attempts 可能多记 |
| C7 | 「32 行矩阵（R4 附录 A）」vs M 语义已改 | §2 S1a vs R4 §2.1 | 测试矩阵与实现不一致 |
| C8 | 「全局生效时零上游发送」vs 无全局退出/TTL | §1.3 | 无 2xx 时全局永久 armed；只有 trial 发送，trial 失败则永远不退出 |
| C9 | 「S=0 503」vs「DirectAvailable=true 可回退 direct」 | §1.2 vs `egress.go:143` | 同一输入两种行为 |
| C10 | 「每片可回滚」vs S1a 依赖 S1b pair 状态 | §2 | 回滚 S1b 会让 S1a 的 M 失效 |

其中 C3/C4/C8/C9 是 blocker 级语义矛盾；C1/C2/C5/C10 是切片/部署级。

### 3.1 全局闸没有退出条件（C8 展开）

§1.3 只写「任一真实 2xx ⇒ 同事务清全局与对应 pair」；没有写：
- 全局 armed 后，若长期没有 2xx（因为零发送），是否/何时解除？
- `GlobalArmed` 是 latch 还是每请求重算？若是 latch，`attempts(60s)>=20` 在 armed 后衰减，条件不再成立，但 latch 仍开；若是重算，armed 会自动关，但「blocked 保留」如何表达？
- trial 退避上限？`NextProbeIn` 从哪来？旧 `shape_drift` 有 60→120→300s；v3.2 没有给全局 trial 退避表。
- 「TTL 只推 nextProbeAt」中的 TTL 是 pair TTL 还是 global TTL？§1.5 的 gate_state 有 `global.nextProbeAt`，没有 `global.until`。

结果：全局闸可能像旧 bug 一样永久闩死（R3 D3 的教训），而计划把它当作已解决。**必须给出 `global.until`/退出条件/最大 armed 时长，或明确「只能人工 off/重启」。**

## 4. 附加攻击 2：切片边界与半成品风险

### 4.1 S1a/S1b/S1c 边界

| 切片 | 计划内容 | 依赖/缺口 | 能否独立回滚 |
|---|---|---|---|
| S1a | Admission API + meter + 条件化五层；main 两循环改写 | M 需要 pair 状态（S1b）；32 行矩阵是旧语义；100 单飞需要 canary/global token（S1b/S1c）；`GateDecision` 未定义 | 只能回滚到旧 shape，但旧 shape 调用已被改写；若 S1a 单独上线，M 用旧 modelQuota，行为不是最终语义 |
| S1b | per-pair + canary + 24h 预算（先内存） | 无持久化，重启清 blocked；依赖 S1a 的 Admission；`gate_state.json` 结构在 S1c | 回滚 S1b 会让 S1a 的 M 失效；不能独立部署 |
| S1c | 全局闸 + off + 持久化 + 迁移 | 全局 trial token/退避未定义；off/golden 未定义；迁移 per-node→per-pair 未定义 | 最大切片；回滚会同时失去持久化/off，风险集中 |

### 4.2 半成品上线风险

- **S1a 单独上线**：条件化 M 依赖的 pair 状态不存在；若用 `modelQuota`（per model,node 但未接选路）会得到「当前 pair 被 block 就 429」的旧语义，正是 R4 B2 的 false 429。**不可上线。**
- **S1b 单独上线**：无 gate_state，重启清 blocked；且 canary/预算在内存，进程重启后 24h 预算清零，可能重复打 IP。**不可上线。**
- **S1c 单独上线**：全局闸 + off + 持久化 + 迁移一次做完，无法分段验证；回滚会同时丢掉三件事。

**建议**：S1a/S1b/S1c 都默认 `SHAPE_DRIFT_GATE=off` 合并，直到 S1c 完成并 golden 通过；或者把 S1a 改成「只加 meter + 纯函数 Decide（不接线）」，S1b 加 pair 状态，S1c 一次性接线+持久化。计划当前写法不是这样。

### 4.3 S2/S3 与 S1 的重叠

- S2 又写「冷却 5m/30m/2h/5h + canary」，与 S1b 重复；S2 还删除 `recordProbeSuccess` 对 probation 的提升，但 S1b 的 canary 可能依赖 probation 证据；顺序/职责不清。
- S3 per `(model,node)` 配额状态与 S1b 的 per-pair（model,egress）重叠；S3 说旧状态无字段 ⇒ unknown，但 S1b 的 pair 状态已经知道 blocked；面板字段来源需统一。
- S4 等 T7，OK。

## 5. 附加攻击 3：验收断言可自动化吗

| 断言 | 可自动化？ | 缺口 |
|---|---|---|
| 32 行矩阵逐行断言 | 否（当前） | 矩阵未随 v3.2 更新；M/D/S 语义变了；缺 `RouteKind`/`Canary`/`Trial` 维度 |
| 并发 100 单飞 | 部分 | 只定义了 canary/global；首次 FUL 167 无单飞；token 释放/超时未定义 |
| 单出口 167 第 3 次停 | 否 | 状态机矛盾（C3）；并发下无首次在途门；串行下第 1 次就停 |
| 5h 虚拟时间 | 否（当前 API） | pool/quota/egress/main 仍 `time.Now()`；只注入 gate 的 Now 不够 |
| 每 egress 发送次数 | 是 | instrumented transport 可行；但 `bunEgressResult` 替换 client，需在替换前埋点；计划未指定 |
| golden 回放 | 否（当前） | 旧路径随机 jitter（可注入但计划未固定）、真实时钟、全局 trial 状态；没有 fixture schema/golden 文件 |
| 重启不清 blocked | 部分 | 需要重启 harness/状态文件注入；计划没给测试形状；S1b 内存态不满足 |
| 10 万 model 内存 | 部分 | 需要 heap 度量与 blocked 保留断言；cap 满时策略未定义 |
| 变异 ≥6/片 | 否（当前） | 无变异清单；无法判断「全红」覆盖了哪些语义 |

### 5.1 建议的最小自动化套件

1. **纯 Decide 表驱动**：新矩阵（6 维边界，不必 2^6 全量），断言 `Allow/Reason/Status/RetryAfter/Probe/Lease`。
2. **原子 Admission 并发测试**：100 goroutines 同时请求同一 pair/egress；instrumented transport 断言发送数 = 1（canary）或 0（已 blocked）；用 `-race`。
3. **状态机序列测试**：fake clock 推进 0/5m/35m/2h35/7h35/12h35/17h35/22h35，断言每次发送/抑制的精确序列，而不是 ≤8。
4. **持久化/重启**：写 gate_state + proxy_state，模拟进程重启，断言 blocked/预算/global 保留；损坏文件 fail-open；旧 v1/5h 迁移。
5. **off golden**：固定 jitter/clock，跑同一事件流，比较发送数/抑制原因/状态码；明确 off 不绕过 T2/T3。
6. **变异**：至少覆盖 M（all vs any）、D（unconditional vs route）、S（allow direct vs 503）、canary K（1 vs 2）、预算窗口（reset vs cumulative）、off（bypass pool403）。

## 6. 附加攻击 4：有没有更简单且等价的方案

**有。** v3.2 的三段式 + 五层 + per-pair + global + canary + budget + 两文件持久化，在 S1a/S1b/S1c 三片内实现，复杂度和竞态面都偏大。更简单的等价骨架：

### 6.1 单一原子 `Admit` 取代 Snapshot/Decide/Commit

```go
type RoutePlan struct {
    Kind      RouteKind // node/direct/none
    EgressKey string
    Pair      PairKey   // model+egress
    Canary    bool
}
type Decision struct {
    Allow bool
    Reason Reason
    Status int
    RetryAfter time.Duration
    Plan RoutePlan
    LeaseID uint64 // 发送成功后 Finish(lease, outcome)
}
func (a *Admission) Admit(now time.Time, model string, req RouteRequest) Decision
func (a *Admission) Finish(leaseID uint64, outcome Outcome, now time.Time)
```
- `Admit` 在一把锁内完成：选路（复用 `previewExclusionVerdict` + `modelQuota`）、五层判断、pair 预留、meter 预留（attempts 在真正发送前由 `Finish`/`BeginSend` 确认）。返回的 `Decision` 自带 lease；发送前若 lease 失效（pool403/global/pair 变更）则 `Finish(aborted)` 并重试/503。
- 不需要 `Snapshot`/`Decide`/`Commit` 三段；没有「快照到发送」的窗口，因为 lease 是预留，不是快照。
- 如果坚持三段式，至少给 `GateInput.Version` + `Commit` 返回 `ErrStale`，并在发送前复核全部 deny 层。

### 6.2 先选路，再 gate（解决 `PairChosen` 鸡生蛋）

- `SelectRoute` 先根据 `exclude`、sticky、region、pair blocked 选出 `RoutePlan`（node/direct/none）。
- `Admit(route)` 只判断该 route 是否允许，不再需要 `PairChosen` 输入。
- 这样 D 只在 `route.Kind==direct` 时生效；M 只在 `route.Kind==none` 且所有 node pair quota-blocked 时生效；S 只在 `route.Kind==none` 且 direct 不可用时 503。
- 选路与 gate 的顺序必须写死；现有 `egress.Get` 的 direct fallback 要移进 `SelectRoute`，不能在 gate 后再 fallback。

### 6.3 预算简化

- 24h 预算只统计 `403-confirmed`（或 shape-confirmed），不统计普通 FUL；FUL 只触发 per-pair 冷却。
- 固定 24h 窗口，`windowStart` 到期 `hits=0`；canary 成功不计数；失败计数但不超过窗口。
- 解锁：窗口到期 + canary K=2；提供人工删除 gate_state 条目的运维路径。

### 6.4 持久化简化

- 一个 `gate_state.json`，带 `version` + `proxyStateGeneration`；加载顺序：proxy_state → gate_state；若 generation 不匹配，丢弃 gate_state（fail-open 并告警）。
- 先写 proxy_state 再写 gate_state（或反过来），但加载时以 gate_state 的 pair block 为准、以 proxy_state 的节点状态为准；缺失节点跳过并清理 orphan。
- 回滚旧二进制时，proxy_state 的 generation 递增；新二进制看到 generation 不匹配 → 忽略 gate_state，避免 stale block 复活。

### 6.5 切片简化

- S1a：只加 `admission.go` 的纯函数 + meter + 表驱动测试，**不接线**（main 仍走旧路径）。
- S1b：加 `SelectRoute` + pair 状态 + canary，接线但默认 off；测试用 fake clock。
- S1c：加 global + persistence + off + migration，默认 on；golden 通过后发版。
- 这样每片可独立回滚，不会半成品上线。

> 结论：方向（per-pair 默认、全局严格例外、单一优先级、canary）可保留；但 v3.2 的 API/矩阵/状态机/预算/持久化需要按上述骨架重写，才具备可实施性。

## 7. 实现顺序、验收命令、最可能翻车的 3 个地方（若 Lead 选择先修 blocker）

> 当前结论是 NO-GO；以下是「修完后如何进 S1a」的建议，不是对 v3.2 的放行。

### 7.1 实现顺序

1. **修 B1**：确定 `SelectRoute` 在 gate 前；`Admission.Admit` 原子返回 lease；`Finish` 回写 outcome；`GateInput` 去掉 `PairChosen` 或改由 `RoutePlan` 提供；发送前复核 lease。
2. **修 B2**：重写矩阵（6 维），M 纳入 direct；S=0 与 direct fallback 二选一写死；D 只在 route=direct。
3. **修 B3**：定义 per-pair 状态（首次是否 block；3/10min 与 5m 的关系）；加首次在途单飞；定义 canary 成功/失败/超时映射与 token 释放。
4. **修 B4**：预算只算 403-confirmed；固定窗口；canary 成功不计数；解锁路径。
5. **修 B5**：gate_state schema/epoch/加载顺序/回滚失效；off 代码结构与 golden 确定性。
6. **修 B6**：meter 的 Begin/Finish 接口；Clock 注入 pool/quota/egress/main；cap 溢出策略；10 万 model heap 测试。
7. 再按 S1a（纯函数+不接线）→ S1b（接线默认 off）→ S1c（持久化+global+on）推进。

### 7.2 每步验收命令（建议）

```bash
cd /home/jingyx/project/opencode2api-test
export PATH=$PATH:/usr/local/go/bin GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"

gofmt -l admission.go main.go proxy_node.go quota_halt.go shape_drift.go
go build ./...
go vet ./...
go test -race -count=1 -run 'TestAdmission|TestGateMatrix|TestPairState|TestCanary|TestBudget|TestPersistence|TestOffGolden' ./
go test -skip 'TestIP4RV' -count=1 ./...
```

- 矩阵：`go test -run TestGateMatrix -v`，每行输出 `P G M D R C T => allow/reason/status`。
- 并发：`go test -race -run TestAdmissionSingleFlight -count=20`，instrumented transport 断言 `sent==1`。
- 虚拟时间：`go test -run TestFiveHourCanary -v`，打印每次发送时间/egress/outcome。
- 持久化：`go test -run TestGateStateRestart -v`，子进程重启后断言 blocked/预算。
- off：`SHAPE_DRIFT_GATE=off go test -run TestOffGolden -v`，对比 golden JSON。

### 7.3 最可能翻车的 3 个地方

1. **`Admit` 的 lease 与发送分离**：即使加了 lease，发送前仍可能 pool403/global 变化；如果 lease 不覆盖这些层，167/并发仍会穿透。最可能在这里回归成「Commit 成功但发送已过期」。
2. **M/D/S 与 direct fallback 的组合**：`egress.Get` 在 gate 之后回退 direct，任何一处顺序写错就会出现「节点全 block 却打 direct」或「direct 可用却 429」。这是 R4 行 1/3 的复发点。
3. **预算窗口/canary 成功计数**：`{hits, windowStart}` 的 reset、canary 成功是否计数、FUL 是否计入硬排除，三者任一写错就会 24h 锁死或 false positive；这是 B4 的复发点。

## 8. 未验证/数据缺口

- 未跑 167/00:20 生产回放；R3/R4 的数字未在本轮独立复算（无 10-27 完整 call_log）。
- 未测真实上游 429/403 分布；预算/阈值的生产影响是推演。
- 未实现/未跑 v3.2 代码；所有结论是纸面/代码事实。
- `proxy_state.json` 本机为空（0 节点），迁移/回滚的现场状态未验证。
