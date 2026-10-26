# R4 对抗复审 · 节点系统 v3.1

> 输入：`docs/superpowers/plans/2026-10-27-node-system-v3.md`（v3.1，126 行）、`11-shape-drift-design.md`、`12-shape-drift-review.md`、`16-plan-review-r3.md`。
> 基线：工作区当前 HEAD `3013acb`（v3.1 文档提交），代码状态为 `456256f`（`da85f2b` 是其父提交）。R4 未写代码、未 commit、未改任何生产文件。
> 结论：**仍需修订（NO-GO），不可按本版进入实现。** 6 条 blocker 见 §0/§9；§7 的六个攻击点逐条回答。

## 0. 结论与 blocker

**结论：仍需修订。** v3.1 的方向可保留（per-`(model,egress)` 默认、全局严格例外、单一优先级），但当前版本把「唯一裁决点」写成了一个**没有提交原语、缺少选择阶段输入**的纯函数，并且 per-pair 状态机、canary 退避、持久化/回退、计数器与测试时钟都没有闭合。现在开 S1 会把 R3 已修掉的问题以新的形式带回来。

| ID | 严重度 | blocker | 最短证据 |
|---|---|---|---|
| B1 | Critical | `Decide` 无法实现「同一事务更新 + 单飞令牌」：签名无 receiver/`Commit`，`GateInput` 没有 trial-in-flight / token-valid / next-probe / 候选数；`EgressKey`、`PairBlocked` 在选路前不可知 | 计划 §1 只给 `Decide(in)`，同节要求同一事务；当前 `egress.Get` 在 gate 之后才选出口（`egress.go:123`） |
| B2 | Critical | 五层优先级在 32 种组合里 `D&S=1` 共 8 行（其中 1 行 D 是唯一拒绝层，是实际 false reject）、`G&M=1` 与 `P&M=1` 各 8 行（有重叠，429 被 503 掩掉）、1 行「无层拒绝且无路」未定义；`selection` 不在 `Reason` 枚举、没有状态码 | 附录 A；§2 具体行 |
| B3 | Critical | v3.1 只定义了 GLOBAL_SHAPE 的六个条件，**没有 per-pair 触发/冷却状态机**；单出口 167 连发 `distinct=1`、`consecutive=1` 永不触发全局；S1 却要求事故仿真「单出口命中 ≤1」 | §3 单出口场景；§3 S1 |
| B4 | Critical | canary 没有成功阈值/单飞：单次 2xx 若清 pair 会回到 5m 循环（288 次/日）；冷却到期 100 并发会同时打同一 IP；5h canary 本身可在 24h 打同一 pair 8 次，而 D4 预算只统计 shape-suspect | §4 算术；§0 D5/D4 |
| B5 | Critical | 持久化/迁移/回退未定义：当前 shape 状态纯内存，重启清零；`proxy_state` 无 pair/global/budget 字段且 v1；旧 `NodeExhausted` 5h marks 部署后仍按旧时间到期；`SHAPE_DRIFT_GATE=off` 在当前代码不存在，且「回到 HEAD」与保留 pool403/quota 冲突 | §6；`proxy_node.go:1996,209`；`shape_drift.go:240` |
| B6 | Important→Critical | `AttemptsLast60s` / `Ratio429Last60s` / `DistinctNew429` / `ConsecutiveNew429` 在现有代码没有计数器；没有可注入时钟（5h 虚拟时间测试不可做）；per-model map 无上限/清理；`单出口命中`/`主 IP 命中` 断言不可直接自动化 | §5/§7；`time.Now()` 在三个核心文件 23 处 |

> 说明：B4 的「24h 每 pair 8 次」不是说 24h 内一定触发封禁，而是说 **canary 自身就能把 D4 的 8 次预算打满**；同时因为预算只统计 shape-suspect，普通配额 canary 不会计入硬排除，形成「预算说安全、实际仍打 IP」的盲区。

## 1. 方法与边界

- 只做纸面/代码/数值复核，不改代码、不 commit。
- 基线核对：`git log` 显示当前 HEAD 为 `3013acb`（v3.1 文档），其父 `456256f` 为实现；`da85f2b` 是 `456256f` 的父提交。输入里写的 HEAD `da85f2b`/`456256f` 与当前 checkout 不一致，本报告以实际工作区为准。工作区另有他人未提交的 `main.go` +4 行（`recordNodeSuccess` 的 2xx probation 证据，沿流式/非流式两路径），不是 R4 的改动；报告的行号/结论以该实际代码为基线，T4 上线前需确认这 4 行的归属。
- 代码事实来自：`shape_drift.go`、`quota_halt.go`、`proxy_node.go`、`egress.go`、`main.go`、`proxy_state.json`。
- 数值脚本：`/tmp/r4matrix2.py`（交互矩阵）、`/tmp/r4boundary.py`（边界场景）。两脚本只读/打印，不写仓库。
- 数据限制：本 checkout 的 `proxy_state.json` 为空（0 节点）；没有 10-27 的完整 `call_log` 副本，不能在本机独立复算 00:20 的 167 次；§3 的边界结论来自**按 §2 条件构造的确定性事件流**，不是生产数字。凡引用生产事实的地方都注明来源文件/代码行，不把 R3 的数字当成本轮证据。

## 2. 五层交互矩阵

### 2.1 记号与假设

`P=Pool403Halted`、`G=GlobalShapeArmed`、`M=ModelQuotaBlocked`、`D=DirectQuotaBlocked`、`S=SelectionHasRoute`（排除本 model 的 pair 后仍有可用候选）。

`M` 在 v3.1 只有一个 bool，未定义是「当前 pair 被 quota 拒」还是「该 model 所有 pair 都被拒」。矩阵按**当前 pair** 解释 `M=1`（因此可与 `S=1` 同时出现）；如果实现按「所有 pair 都 blocked」解释，则 `M=1,S=1` 是矛盾输入，调用方不应产生，但必须测试拒绝这种快照——两种解释的期望行为不同，正是 B1/B2 的核心。

`D` 同样只有一个 bool：如果 direct 一旦 429 就置 true，行 1 会 false reject；如果只在「本次选路会落到 direct」时置 true，则 `Decide` 必须先知道候选/路由结果，v3.1 的 `GateInput` 又没有这个字段。

按 v3.1 §1 的优先级（高→低）`P > G > M > D > selection`，假定 `P/G/D → 503`、`M → 429`、`selection` 只在最后。附录 A 穷举 2^5=32 种组合。

### 2.2 必看的 7 行（具体输入 → 期望输出）

| # | P | G | M | D | S | 场景（具体输入） | §1 实际结果 | R4 期望/判定 |
|---|---:|---:|---:|---:|---:|---|---|---|
| 1 | 0 | 0 | 0 | 1 | 1 | 模型 `mimo`；节点 `node:fp-a` 可用；直连出口昨天 429；`DirectQuotaBlocked=true` | **503 `direct_quota`** | **应走 `node:fp-a` 并 200/正常转发**；D 只在「本次选路会落到 direct」时生效。v3.1 无条件拒绝 ⇒ **false reject**（B2） |
| 2 | 0 | 0 | 0 | 0 | 0 | 节点全在冷却/隔离，`S=0`；无 pool/global/quota/direct 闩 | **`Allow`/`Reason` 未定义**（`Reason` 枚举没有 selection） | 应 `503 reason=no_candidate`（禁止回退直连）；否则 `egress.Get` 会在 `egress.go:143` 回退 direct，恰好违反 S1「禁止回退直连」 |
| 3 | 0 | 0 | 1 | 0 | 1 | 模型 `mimo` 的 pair A 已 blocked；pair B（`node:fp-b`）仍健康；`PairBlocked=true` 只表示 A | **429 `model_quota`** | 应先选 B；若 B 可用则正常转发，只有「该 model 所有 pair 都 blocked」才 429。`GateInput.PairBlocked` 单 bool 无法表达 pair-level ⇒ **false 429**（B1/B2） |
| 4 | 0 | 1 | 1 | 0 | 1 | global armed；model `mimo` 的 pair A blocked；trial token 尚空 | **503 `global_shape`**；probe 允许但 token 未绑定 model/pair | trial 只能选 `S` 中未 blocked 的 egress；若选到 A 得 429，会把 **model 配额**误算成 shape probe 失败并延长 global（B1/B2） |
| 5 | 1 | 0 | 1 | 0 | 1 | pool403 halt 中；model pair 也 blocked | **503 `pool403`** | 状态码相同但 `model_quota` 被吞；pool 解除后同一 model 变 429，客户端观察到无关层改变状态码。需定义 dominant reason/`SuppressBy`（B2） |
| 6 | 0 | 1 | 0 | 0 | 0 | global armed；没有可用 egress | **503 `global_shape` 且 `probe=yes`** | probe 不能凭空选中候选；应 `probe=false` / `503 no_candidate`，或先算 `candidate_count`（B1/B2） |
| 7 | 0 | 0 | 1 | 1 | 1 | model pair blocked；direct 也 429 | **429 `model_quota`**（M>D） | 与 D7「direct→503」冲突：同一输入只因 M 的优先级变成 429；必须明确「多个拒绝时哪个状态码优先」并写测试（B2） |

### 2.3 统计与结论

- 32 行中：`D=1,S=1` 共 **8** 行（其中 1 行无更高层，D 是唯一拒绝层）；`G=1,M=1` 共 **8** 行（503 掩掉 429）；`P=1,M=1` 共 **8** 行；`G=1,S=0` 且 `P=0` 共 **4** 行（probe 放行但无候选）；`M=1,S=1` 且 `P=G=0` 共 **2** 行（可能还有别的 pair，却直接 429）；无层拒绝且 `S=0` **1** 行未定义。
- **「某层放行、另一层仍拒绝」**：
  - `D=1,S=1`（行 1）：selection 可走节点，direct 层仍 503。
  - `M=1,S=1`（行 3）：selection 可换 pair，model 层仍 429。
  - `G=1,S=0`（行 6）：global 放行 probe，selection 无候选。
- **「两层同时拒绝但状态码冲突」**：`M=1` 与 `P/G` 同时为真时 503（`pool403`/`global_shape`）压掉 429；`M=1,D=1` 时 429 压掉 503。D7 只在「只有 per-model 配额」时要求 429，但没有规定组合优先级；这会让同一 model 的客户端在 pool/global 状态变化时看到 429/503 跳变，且 `Retry-After` 语义不同。不是必须反对的优先级，但必须在 `GateDecision`/测试中写死。
- **`selection` 不是一层可拒绝的 scope**：`Reason` 注释只有 `pool403|global_shape|model_quota|direct_quota|ok`，没有 `selection`/`no_candidate`/`all_models`；`StatusCode` 注释只有 503/429；S1 要求「禁止回退直连」，但没有合法 decision 表达这个拒绝。B2 的 API 缺口，不是文案问题。

## 3. 边界数值（attempts≥20 / ratio≥0.9 / no-2xx 60s / K=5）

以下用 `attempts`=最近 60s **上游发送尝试数**（含同一客户端请求的重试），`ratio`=429/attempts，`distinct`=最近 600s 去重 egress 的 429 数，`consecutive`=自上次 2xx 起的新 egress 429 连续计数（同一 egress 重复不递增）。事件流由 `/tmp/r4boundary.py` 生成。

| 场景 | 60s attempts | distinct | consecutive | ratio | 全局是否触发 | 首次 armed | 触发前实际发送 |
|---|---:|---:|---:|---:|---|---:|---:|
| 低流量 0.1 req/s，1 attempt/请求，全 429 | 6 | 5 | 5 | 1.0 | **否**（attempts<20） | 永不 | 30 |
| 低流量 0.1 req/s，8 attempts/请求，全 429 | 24 @30s | 5 | 5 | 1.0 | **是** | t=30s | 32 |
| 突发 50 req/s ×10s，last 2xx t=-1s | 500 | 5 | 5 | 1.0 | 是，但**等到 age=60s** | t=59s（流量已归零） | **500** |
| 同上，last 2xx t=-30s | 500 | 5 | 5 | 1.0 | 是 | t=30s | 500 |
| 同上，last 2xx t=-59s | 500 | 5 | 5 | 1.0 | 是 | t=1s | 51 |
| 同上，无 2xx | 500 | 5 | 5 | 1.0 | 是 | t=0.4s | 21 |
| 单出口 167 连发 | 167 | **1** | **1** | 1.0 | **否** | 永不 | **167**（若没有 per-pair） |
| 同窗口 40 ok + 411 429（t=60 窗口；1 个 2xx 在边界外） | 450 | 5 | 5 | 0.913 | **否**（有 2xx） | — | 451 |
| 同上，最后一次 2xx 后 60s（t=70） | 410 | 5 | 5 | 1.0 | **是** | t=70s | 451 |

精确边界（布尔表达式直接代入）：

| 输入 | 结果 |
|---|---|
| attempts=19, ratio=1, distinct=5, consecutive=5, age=60 | **false** |
| attempts=20, ratio=1, distinct=5, consecutive=5, age=60 | **true** |
| attempts=20, 17/20=0.850, distinct=5, consecutive=5, age=60 | false |
| attempts=20, 18/20=0.900, distinct=5, consecutive=5, age=60 | **true**（≥ 边界） |
| age=59.999 | false |
| age=60.000 | true |
| K=4（consecutive=4） | false |
| K=5 | true |

解释：

1. **低流量「永远不触发」只对 1 attempt/请求成立**。`attempts` 若按上游发送计，0.1 req/s × 8 次重试 = 24/60s，30s 就能 armed；若按客户端请求计则永远 <20。v3.1 没有定义分母，直接把「低流量靠 per-pair」当作安全前提不成立；必须写清 `attempts` 是发送尝试还是请求，且测试固定。
2. **`last_any_2xx_age>=60s` 是 60 秒的延迟窗，不是发送上限**。last 2xx 在 burst 前 1s 时，500 次全部发完才 armed；last 2xx 在 59s 前时只发 51 次。这不是「是否该拦」的价值判断，而是必须在 S1 事故测试里覆盖的边界；否则「无 5,520 次发送」可能只是在某个 last-2xx 位置成立。
3. **单出口 167 不会触发全局**：`distinct=1`、`consecutive=1`，六个条件中至少两个永远不到 5。v3.1 §2 只有全局条件；如果 per-pair 没有「同一 pair 3/10min」或「5 次重复」的独立规则，167 会全部发出。S1 的 `单出口命中 ≤1` 因此**没有对应机制**（B3）。
4. **`consecutive` 的重置语义未定义**：若「同出口重复 429」既不加也不重置，5 个新出口后可保持 5；若重复会重置，则 5 个新出口后一旦重复就回到 0，永远不满足 `consecutive>=5`。两种实现都能从「同出口重复不递增」读出，必须在 §2 写成明确状态转移。
5. **40 ok + 411 429 永不触发全局**（ratio 0.913 但 2xx 硬否决），这是设计意图；但 `40 ok` 只要出现一次，就能把全局推迟到最后一个 2xx 后 60s。若 40 ok 持续存在，全局永不 armed，全部靠 per-pair；这对保护成功流量是对的，但不是「411 个 429 会被全局拦住」。
6. **ratio 的边界是 ≥0.90**：17/20=0.85 不触发、18/20=0.90 触发；`attempts` 19 时不触发。所有边界需 ±1 单测，否则实现者很容易写成 `>`。

## 4. canary / 冷却：假性恢复与单调性

### 4.1 递进冷却的算术

单 pair 从 t=0 首次 429 开始，按 5m→30m→2h→5h（失败即升级，strikes 封顶 4）重试：

```text
t=0        首次 429（初始命中）
t=5m       第 1 次 canary
t=35m      第 2 次
t=2h35m    第 3 次
t=7h35m    第 4 次
t=12h35m   第 5 次
t=17h35m   第 6 次
t=22h35m   第 7 次
```

**一个 pair 在 24h 内就是 8 次命中**（包含初始），正好等于 D4 的 `H_bad=8`。若同一 egress 上有 3 个 model，各自 pair 独立 canary，则同一物理 IP 24h 可达 **24 次**；而 v3.1 的 24h 预算只统计 shape-suspect，普通配额 canary 不计数 ⇒ 预算无法阻止这条路径（B4）。

### 4.2 单次成功会不会重新引入假性恢复

会，除非定义滞回。当前 `quota_halt.go` 已经因为「单次成功不可信」采用 `haltRecoveryStreak=2` + 两次成功间隔 `>=60s`（`quota_halt.go:42-47,71-80,125-160`）；v3.1 D5 只说「真实请求 canary」，没有写：

- canary 成功几次才清 pair；
- 成功是否把 strikes 重置为 0；
- 第一次成功与第二次 429 之间是否重新进入普通流量。

若「1 次 2xx → available + strikes=0」，则：

```text
5m 后 canary 成功（瞬时恢复/偶发 2xx）
→ 普通流量恢复
→ 下一次 429
→ strikes=0，重新 5m
→ 24h 约 288 个 5m 周期
```

这不是「慢信号」——每个周期都可能放出一批普通请求；如果每周期只成功 1 次，单 IP 也会被 288 次命中，远超 10–16 的保守封禁阈值。**必须复用 K=2/60s，或至少「成功只进入 verifying，不立即 available；连续成功/稳定窗口后才 reset strikes」**（B4）。

### 4.3 并发 canary 会把「一次证据」变成「一次风暴」

v3.1 的「单飞令牌绑 egress」只写在全局 trial；per-pair 冷却到期没有说明单飞。冷却到期瞬间，100 个并发请求都可能看到 `PairBlocked=false`/`until` 已过：

- 100 个请求同时打同一 pair；
- 每个 429 都可能 `strikes++`（或至少 100 次命中）；
- strikes 封顶只会让 `until` 变成 5h，不会撤销已经发出的 100 次命中；
- 若上游按 IP 计数，这 100 次已经足够触发封禁。

必须 per-pair 单飞（`inflight` 只能有一个 canary；其余请求 fail-fast/等结果），并与全局 trial 竞争同一个 egress 时串行。测试：100 goroutine + 冷却到期，断言实际发送 **恰好 1**、其余 suppressed。

### 4.4 canary 的绑定与结果映射

- canary 必须强制选 `(model, egress)` 的那个 egress；若选择器返回别的 egress，2xx 不能清原 pair（否则是「借成功洗掉坏 pair」）。
- 2xx 才清；429 升级 strikes；5xx/timeout 不能当作配额成功或失败（按 transport/dead 路径），否则一次超时会把 pair 误放行/误升级。
- 全局 trial token 目前只绑 egress，不绑 model/expiry/single-use；若 trial 命中一个 model 已 blocked 的 pair，会把 model 配额 429 当成 global shape 证据（§2 行 4）。
- v3.1 没有给 global trial 的退避表（旧实现有 60→120→300s），也没有 `nextProbeAt` 字段；TTL 到后「只推 nextProbeAt」在 `Decide` 中无处表达（B1/B4）。

## 5. 可实施性：输入、计数器、内存

### 5.1 现有代码能否直接拿到这些输入

| 输入 | 现状 | 结论 |
|---|---|---|
| `Pool403Halted` | `proxyPool.poolHalted(now)` 有锁读，`proxy_node.go:899` | 可拿；但 `Decide` 快照与发送之间会变化，必须发送前复核 |
| `PairBlocked` / `ModelQuotaBlocked` | `modelQuota` map 已存在（`proxy_node.go:223,1734,1751`），但**未接入选路**、无清理/持久化；pair 的 strikes/canary 不存在 | 不能直接拿；S1 需先建 pair 状态机（B3） |
| `AttemptsLast60s`、`Ratio429Last60s` | 无内存计数器；`call_log` 是事后聚合（`calllog.go`） | 需新增 gate meter；不能每次请求扫日志 |
| `DistinctNew429`、`ConsecutiveNew429` | 无 | 需新增 600s 去重表 + 连续状态 |
| `LastAny2xx` | 无内存时间戳 | 需新增；注意 2xx 来自两条循环/直连/节点 |
| `EligibleCandidates`/`UnblockedCandidates` | `previewExclusionVerdict` 已有只读预检（`proxy_node.go:1930`），但 v3.1 `GateInput` 没有这两个字段 | 必须加入；否则 D/S 矩阵行无法判 |
| trial 状态（in-flight/nextProbe/token） | 旧实现有 `trialInFlight`/`trialBackoff`（`shape_drift.go:60`），v3.1 `GateInput` 删除 | 必须加回；否则无法单飞/退避 |

### 5.2 计数器设计（建议的最低实现）

- **一个 `gateMeter`，单一 `sync.Mutex`（或每秒原子桶 + 一次快照锁）**：60 个 1s 桶保存 `attempts`、`n429`；`last2xx`；总数不必扫 60 个桶，可用环形累加。
- **600s distinct**：`map[egressKey]time.Time`，只保留最近 600s；egress 上限=节点数+direct（当前约 212+1），可加硬上限；每次 429 时写入，过期懒清理/定时清理。
- **consecutive**：`last2xx` 后按「新 egress」递增；明确重复 egress 是「不加也不重置」还是「重置」；建议按 §2 的措辞选「不加不重置」，并在测试里固定。
- **写入点**：发送前记 attempts；收到 429/2xx/其它结果分别记；suppressed 不能记 attempts/ratio（否则分母被自己的拒绝污染）。两条 main 循环（stream/non-stream）、direct/node、trial 都要走同一接口；最好在 `Decide` 的 `Commit` 里记，避免上游调用方漏记。
- **读取原子性**：`Attempts/Ratio/Distinct/Consecutive/Last2xx` 必须在**同一个锁/版本**下生成快照；否则 `attempts=19` 与 `ratio=1` 可能来自不同时刻，边界测试会 flaky。
- **内存上界**：meter 固定 ~60 桶 + ≤213 egress；pair 状态按 model 数 × egress 数；若 512 model × 212 egress ≈108k pair，每项约 64–96B ⇒ **约 7–10MB**（估算，未实测），可接受；但必须设 model cap/空闲 TTL/最大 pair 数，且不能 LRU 淘汰已 armed/blocked 的证据（旧 `shapeDriftMaxModels` 的 C2 教训，`shape_drift.go:42`）。不设 cap 时客户端随机 model 名可打爆内存。
- **时钟**：当前 3 个文件 23 处 `time.Now()`；5h 虚拟时间无法在现有 API 上跑。必须把 `Now` 传入 gate/pair/meter（`ProxyPool.sweepExpiredLocked(now)` 已经是这个形状），测试用 fake clock；否则 S1 的「5h 虚拟时间」只能靠真实 sleep 或改全局变量，不可靠。

### 5.3 建议的 API 形状

```go
type Admission struct{ /* gate state, meter, pool */ }
func (a *Admission) Snapshot(now time.Time, model string) GateInput
func (a *Admission) Decide(in GateInput) GateDecision
func (a *Admission) Commit(d GateDecision, now time.Time) error
// GateInput 至少补：CandidateN, UnblockedN, DirectAvailable,
// PairChosen, PairBlocked, ModelAllPairsBlocked, PairCooldownRemaining,
// TrialInFlight, TrialTokenValid, NextProbeIn, GlobalBlockedSet
```

`Decide` 只读快照；`Commit` 在 gate 锁内做 CAS/状态转移；发送前再复核 `Pool403Halted`（当前 main 的双重检查 `main.go:2946/2967` 必须保留）。仅靠「纯函数 Decide」无法满足 §1 的「同一事务」。

## 6. 迁移与回退

### 6.1 `SHAPE_DRIFT_GATE=off` 的语义

- 当前代码**没有**该 env（`grep -R SHAPE_DRIFT_GATE` 只命中计划文档）；S1 要新增。
- 「off 时回到 HEAD」当前 HEAD 的 shape 行为：按 model 分桶、3 distinct/10min、硬 TTL 10min 清 `seen`、内存态；这与 v3.1 的「blocked 不因 TTL 清除、5m/30m/2h/5h canary」是两套状态机。要把两套都保留，off 走旧路径；否则 off 只能算「关闭新闸」，不是 HEAD。
- off **不能**顺带关掉 T2 pool403 和 quota_halt/直连配额；否则回滚会带来新事故。需明确 `off` 只影响 shape 层，或提供分层的 `SHAPE_DRIFT_GATE`/`POOL403_GATE` 开关。
- 建议：off = 加载旧路径 + 旧 TTL 行为；至少用 18:03/00:20 的回放做 golden（发送数、suppressed 数、状态码）；env 非法值默认 on（安全侧）并测试。

### 6.2 持久化与旧状态

- 当前 `shapeDriftByModel` 只存在内存（`shape_drift.go:29`）；重启清零。`TestShapeDriftResetAfterRestart` 明确记录了这个旧行为。v3.1 §5 写「禁止靠重启清闩（会清掉 blocked 证据）」，但 S1 文件清单没有状态文件，也没有 schema；**按现计划实现会与 §5 自相矛盾**。
- `proxy_state.json` 当前 v1，`nodeRuntimeState` 只有 `state/marked_at/cooldown_until/last_error/...`（`proxy_node.go:209`），没有 pair/global/budget；`proxyStateVersion=1`（`:1996`），读取时版本大于 1 整份忽略（`:2043`）。若把新状态塞进该文件并升 v2，**回滚旧二进制会丢掉全部节点状态**；若另建 `gate_state.json`，则要定义原子写、损坏时的 fail-open/fail-closed、上限与 TTL，并保证 off 时忽略它。
- 旧 `NodeExhausted` 5h：生产 `node_cooldown_exhausted_hours` 可配 5h；升级前已有的 `cooldown_until` 不会因为 S2 改成 5m 而缩短。`sweepExpiredLocked` 仍等到旧 `CooldownUntil` 到期才转 probation（`proxy_node.go:494`），所以部署后最多仍有 5h 的旧闩；S3 的「旧状态无字段 ⇒ unknown」只覆盖新 per-model 字段，不覆盖这些旧 node marks。必须在部署时做一次迁移（按新规则重算/清掉旧的 quota exhausted，保留 dead/GateState rejected），否则 S2 的「5m 首次」在存量节点上不成立。
- 若选择「内存态、重启清零」作为产品决策，必须在计划里删掉「禁止靠重启」并加运维禁令；不能既声称持久化又不写 schema。

## 7. 测试可验证性

### 7.1 「单出口命中 ≤1、主 IP 命中 0」能否自动化

**可以，但必须先定义「命中」和「主 IP」。** 建议：

- 用注入的 `http.RoundTripper`（或 `egress.Get` 的 fake client）按 `EgressResult.EgressKey()` 统计**真实发出的上游请求**；不要把 `call_log` 的 `nodes` 字段当发送计数（失败/切换也写 node）。
- `单出口命中 ≤1`：在 10min 风暴窗内，对每个 `node:fp` 断言 `sent[e] <= 1`；在 5h 窗口内，按 canary 规则断言 `sent[e] <= 1 + canary 次数`（例如 ≤4/model），**不能**同时要求 5h 内 ≤1（与 D5 的 5m canary 冲突；S1 现在的写法会让测试要么失败要么逼实现去掉 canary）。
- `主 IP 命中 0`：把「主 IP」定义为 `EgressKeyDirect`；断言 `sent[EgressKeyDirect] == 0` 且 `candidate_count > 0`。若直接 IP 也允许作为正常出口，则这个断言不成立，需要改成「非白名单 direct 发送=0」。
- 事故回放输入应是**记录事件流**（`ts, model, egress, outcome, req_id`），不是「18:03/00:20 两段参数」；否则 hit 数依赖注入器假设，无法证明是上线代码的行为。建议 fixture 路径与 golden 写入测试，逐条断言 `suppressed` 的 `suppressed_by/would_be_egress/candidate_count`。

自动化断言示例（伪代码）：

```go
for _, e := range sent {
    if e.Egress == "direct" && candN > 0 { t.Fatalf("direct sent") }
    if strings.HasPrefix(e.Egress,"node:") && e.At.After(stormStart) && e.At.Before(stormEnd) {
        if hits[e.Egress] > 1 { t.Fatalf("pair hit >1: %s", e.Egress) }
    }
}
// 每条拒绝必须对应一条 suppressed，且没有 transport 调用
```

### 7.2 虚拟时间

S1 的「5h 虚拟时间」在现有 API 上不可做（`time.Now()` 未注入）。需要：

- `GateInput.Now` 已存在；
- pair/meter/canary 的所有 `now` 都由调用方传入（`shapeDriftObserve429` 现在有 now，但内部 `shapeDriftLastGlobalTrial` 仍用真实时间/全局变量）；
- 测试用 `t0 := time.Date(...)` 逐步推进，不 sleep；
- `proxyPool.sweepExpiredLocked(now)` 已有 now 参数，保持；`quotaHalt` 的 `time.Now()` 需要同样处理（若 canary 依赖它）。

### 7.3 32 行矩阵与边界测试

- `Decide` 表驱动测试覆盖附录 A 全部 32 行；每个组合断言 `Allow/Reason/StatusCode/RetryAfter/Probe/TrialToken`。
- attempts：19/20、ratio 17/20 / 18/20、age 59.999/60、consecutive 4/5、distinct 4/5。
- 503 header；model 429 的 header；禁止上游 429 透传（body/status/header 三处）。
- 单飞：100 goroutine 冷启动/冷却到期，断言发送 1；token 错配/过期/重复使用。
- 持久化：旧 v0/v1 `proxy_state` 加载、旧 5h marks 迁移、损坏文件、版本回滚；off golden。
- 24h 预算：8 次 shape hit 硬排除、普通配额 429 不计数、同 egress 多 model canary ≤上限。

## 8. §7 六个攻击点的回答

1. **优先级是否能消除「一处放行、另一处闩死」**：不能只看优先级。§2 行 1/3/6 是反例；根因是 `Decide` 没有「候选/选路后 pair」输入，且 `D`/`M` 没有条件化。优先级本身可保留（P>G>M>D>selection），但必须把每层改成「对该请求/该 egress 是否真的适用」，并补 selection 的拒绝语义。
2. **attempts≥20 / ratio≥0.9 在低流量/突发下的边界**：见 §3。1 attempt/请求的低流量永不触发（6<20）；8 次重试的 0.1 req/s 在 30s 触发；50 req/s 突发在 last-2xx 1s 前会先发 500 次再在 59s armed；单出口 167 永不触发；40 ok+411 429 永不触发。必须固定 attempts 分母、last-2xx 语义和 per-pair 规则。
3. **冷却 + canary 是否重新引入假性恢复**：会（§4）。单次 2xx 清 pair + strikes=0 ⇒ 5m 循环；并发 canary ⇒ 一次冷却到期 100 次命中；5h canary 自身 24h 8 次/pair，多 model 叠加 24 次/egress，而预算只统计 shape-suspect。需要 K=2/60s、单飞、pair 绑定、单调 strikes 和 per-egress 总预算。
4. **24h 预算只算 shape-suspect 是否可被伪造**：规则本身不可判定（429/FreeUsageLimit 与 shape 不可区分，见 §0 前提）。因此「只算 shape」要么漏掉真实 shape（单出口 167 永不进预算），要么把普通配额算进去（误杀）。客户端可通过多 model 名放大 per-pair 状态与 canary 次数；上游一次配额耗尽可在 5 个 egress 上产生 5 个 distinct 429 + 20 attempts，触发全局——这是**单 model 配额耗尽误判全局**的具体路径。建议：预算按 egress 统计**所有上游 429 接触**，8 作为安全上限；只有 403/confirm 证据才允许 24h 硬排除，429-only 不硬排除。
5. **503 后客户端重试/惊群**：`Retry-After: 30` 对 5h 的 global gate 意味着每客户端 ~600 次重试；所有客户端同一秒重试（无 jitter）会形成波次。单飞只保护上游，不保护 ingress/日志。model 429 没有规定 `Retry-After`，更差。建议 `Retry-After = min(nextProbe/cooldown, 上限)` + 随机抖动，429 也要带；测试 header 与重试预算。
6. **迁移/回退**：`proxy_state` 需要迁移（旧 NodeExhausted 5h、旧 probation、GateState rejected），新 pair/global/budget 需要明确文件与版本；当前内存态重启清零，与「禁止重启清闩」冲突；`SHAPE_DRIFT_GATE=off` 当前不存在，且「回到 HEAD」不能简单返回 Allow（会绕过 pool403/quota）。见 §6。

## 9. 修订要求（blocker 级，按顺序）

1. **先改 API 和事务（B1）**：`Admission` 持有 gate 状态；`Snapshot/Decide/Commit` 或一个在锁内的 `DecideAndCommit`；补 `CandidateN/UnblockedN/DirectAvailable/PairChosen/PairCooldownRemaining/TrialInFlight/TrialTokenValid/NextProbeIn`；`Decide` 不许调用有副作用函数；发送前复核 pool403。验收：32 行矩阵 + 100 并发单飞 + 发送前 halt 竞态。
2. **把五层语义条件化（B2）**：`D` 仅在选路会落到 direct 时拒绝；`M` 仅在「该 model 的所有 pair 都 blocked」时拒绝；`S=0` 返回 `503 no_candidate`（禁止 direct fallback）；`selection` 加入 `Reason`/状态码；定义多拒绝时的 dominant status/`SuppressBy`。验收：§2 行 1/2/3/6/7。
3. **补 per-pair 状态机（B3）**：触发（建议 3/10min 或 5 次重复，单出口 167 必须在第 3 次停止）、5m/30m/2h/5h、canary 单飞、成功阈值、失败/超时映射；把 §3 S1 的「单出口命中 ≤1」改成时间窗/canary 可满足的断言。
4. **canary 与预算（B4）**：K=2/60s（或稳定窗）才 reset；100 并发只发 1；pair 强制 egress/model；全局 trial 退避表；per-egress 总 canary 上限；24h 预算定义可判定（所有 FUL 接触或 403-confirmed，不能二选一地含糊）。
5. **持久化/迁移/off（B5）**：定义 `gate_state.json` 或 `proxy_state` v2；原子写、损坏策略、版本回滚；旧 5h `NodeExhausted` 一次性迁移；off 走 HEAD 旧 shape 路径但不绕过 pool403/quota；golden 回放。验收：重启不清 blocked；旧 state 加载；off 对比。
6. **meter/clock/cap/可测性（B6）**：新增 60s/600s 计数器与同一快照；发送/结果唯一写入点；`Now` 注入；model cap 512/空闲 TTL；10 万随机 model 内存测试；5h 虚拟时间；instrumented transport 统计 hit。

> 修复后需要再跑一轮 R4（不是本报告的 R4）；在此之前不建议开 S1。若 Lead 选择先做「纯 S1 引擎 + 旧行为 off」，也必须先闭环 B1/B2/B3，否则测试无法验证「单出口 ≤1」。

## 10. 复现命令与限制

```bash
# 只读检查；不修改代码
cd /home/jingyx/project/opencode2api-test
git log --oneline -3
git show -s --format='%h %s' HEAD
python3 /tmp/r4matrix2.py        # 32 行矩阵 + 计数
python3 /tmp/r4boundary.py       # 边界场景
grep -RIn 'SHAPE_DRIFT_GATE' --include='*.go' .   # 当前无实现
grep -RIn 'modelQuota' --include='*.go' .        # 已有 map，未接选路
```

- 本机 `proxy_state.json` 为 0 节点；未在生产/远端执行任何命令；没有 10-27 `call_log` 副本，167/00:20 的生产行为未在本机重算，只按 v3.1 条件构造事件流。
- 报告中的内存数字是结构体/条目数估算，不是 heap profile；§3 的「发送数」是事件流模拟，不是生产 replay。T4 前的生产验证仍需真实日志。

## 附录 A：五层 32 组合穷举

| P | G | M | D | S | 优先级结果 | probe | R4 备注 |
|---|---|---|---|---|---|---|---|
| 0 | 0 | 0 | 0 | 0 | 未定义 | - | 无层拒绝 + 无路：未定义 |
| 0 | 0 | 0 | 0 | 1 | 200 ok | - | — |
| 0 | 0 | 0 | 1 | 0 | 503 direct_quota | - | — |
| 0 | 0 | 0 | 1 | 1 | 503 direct_quota | - | selection 有路，direct 仍 503 |
| 0 | 0 | 1 | 0 | 0 | 429 model_quota | - | — |
| 0 | 0 | 1 | 0 | 1 | 429 model_quota | - | model 429，但可能还有别的 pair 可用 |
| 0 | 0 | 1 | 1 | 0 | 429 model_quota | - | — |
| 0 | 0 | 1 | 1 | 1 | 429 model_quota | - | model 429，但可能还有别的 pair 可用 |
| 0 | 1 | 0 | 0 | 0 | 503 global_shape | Y | global 放行 probe 但无候选 |
| 0 | 1 | 0 | 0 | 1 | 503 global_shape | Y | — |
| 0 | 1 | 0 | 1 | 0 | 503 global_shape | Y | global 放行 probe 但无候选 |
| 0 | 1 | 0 | 1 | 1 | 503 global_shape | Y | — |
| 0 | 1 | 1 | 0 | 0 | 503 global_shape | Y | global 503 掩掉 model 429; global 放行 probe 但无候选 |
| 0 | 1 | 1 | 0 | 1 | 503 global_shape | Y | global 503 掩掉 model 429 |
| 0 | 1 | 1 | 1 | 0 | 503 global_shape | Y | global 503 掩掉 model 429; global 放行 probe 但无候选 |
| 0 | 1 | 1 | 1 | 1 | 503 global_shape | Y | global 503 掩掉 model 429 |
| 1 | 0 | 0 | 0 | 0 | 503 pool403 | - | — |
| 1 | 0 | 0 | 0 | 1 | 503 pool403 | - | — |
| 1 | 0 | 0 | 1 | 0 | 503 pool403 | - | — |
| 1 | 0 | 0 | 1 | 1 | 503 pool403 | - | — |
| 1 | 0 | 1 | 0 | 0 | 503 pool403 | - | pool403 503 掩掉 model 429 |
| 1 | 0 | 1 | 0 | 1 | 503 pool403 | - | pool403 503 掩掉 model 429 |
| 1 | 0 | 1 | 1 | 0 | 503 pool403 | - | pool403 503 掩掉 model 429 |
| 1 | 0 | 1 | 1 | 1 | 503 pool403 | - | pool403 503 掩掉 model 429 |
| 1 | 1 | 0 | 0 | 0 | 503 pool403 | - | — |
| 1 | 1 | 0 | 0 | 1 | 503 pool403 | - | — |
| 1 | 1 | 0 | 1 | 0 | 503 pool403 | - | — |
| 1 | 1 | 0 | 1 | 1 | 503 pool403 | - | — |
| 1 | 1 | 1 | 0 | 0 | 503 pool403 | - | global 503 掩掉 model 429; pool403 503 掩掉 model 429 |
| 1 | 1 | 1 | 0 | 1 | 503 pool403 | - | global 503 掩掉 model 429; pool403 503 掩掉 model 429 |
| 1 | 1 | 1 | 1 | 0 | 503 pool403 | - | global 503 掩掉 model 429; pool403 503 掩掉 model 429 |
| 1 | 1 | 1 | 1 | 1 | 503 pool403 | - | global 503 掩掉 model 429; pool403 503 掩掉 model 429 |
> 计数：`D&S=1` 8 行；`G&M=1` 8 行；`P&M=1` 8 行（与 G&M 有 4 行重叠）；`G=1,S=0,P=0` 4 行；`M=1,S=1,P=G=0` 2 行；无层拒绝且 `S=0` 1 行。


## 附录 B：canary 24h 命中序列

| 次数 | 时间（从首次 429 起） | 说明 |
|---:|---:|---|
| 1 | 0 | 初始 429 |
| 2 | 5m | canary 1（若失败 → 30m） |
| 3 | 35m | canary 2（若失败 → 2h） |
| 4 | 2h35m | canary 3（若失败 → 5h） |
| 5 | 7h35m | canary 4 |
| 6 | 12h35m | canary 5 |
| 7 | 17h35m | canary 6 |
| 8 | 22h35m | canary 7 |

> 结论仍是：**NO-GO**。3 个最可能翻车的点：① `Decide` 没有 Commit / 选路后 pair 输入，导致放行与闩死竞态；② canary 单次成功重置 + 并发 canary，5m 循环/一次到期打 100 次；③ 持久化/旧 5h 迁移/off 语义，重启或回滚后 blocked 丢/旧闩继续 5h。
