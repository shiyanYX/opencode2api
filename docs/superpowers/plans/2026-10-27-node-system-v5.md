# 节点系统 v5.2：一次全局重构（资格 · 选择 · 可观测）

> **状态：DRAFT v5.2，待 R8-delta 复核。** 相对 v5.1 的改动全部来自 R8 的 5 个硬矛盾，方向是**删机制**而不是加机制：删 canary、删租约、删排队、删 legacy 开关、把配额从 pair 升到 egress。
> 前情：T1（打分纠偏）、T2（403 遏制）、T3（超时分层）已写进 `test`（HEAD `da85f2b`），生产仍是 10-26 15:16 的旧镜像。v3.x 被 R3/R4/R5 判过度设计；v4.1 被 R6 认可、R7 复审；v5/v5.1 被 R8 判 NO-GO。

## 1. 现状为什么必须重构

| 现象 | 根因 |
|---|---|
| 闩死后每个请求仍发一次上游（167 次/秒级） | 全局闩只改「探测/正常」模式，不阻止发送 |
| TTL 清空 blocked ⇒ 每 10 分钟重扫全池 | `shape_drift.go` 用 TTL 当恢复手段，烧 79–127 IP/小时 |
| 一个出口 429，全池停摆 | 全局闩 + pool403 都是全局状态 |
| 恢复靠 5h 冷却到期 | 没有「单飞探测」，只有等 |
| 打分算得再准，稳态也没收益 | 静态总分选路 0~−2.6pp；oracle +84.7pp 需要在线自适应 |
| 同一节点对不同模型表现相反，分数却只有一个 | 状态按节点聚合，没按 (model, 出口) 分开 |
| 面板数字对不上日志 | 抑制、未尝试、429 归因混在一起 |

**决定性证据（10-27 00:15–00:45 生产日志复算）**：1,847 请求 → **1,529 次上游 429**，只有 3 个 switch、1 个 `shape_drift` 事件；单个出口 `653e51eb` 在 11 分钟内挨 **1,106 次 429**，而同一时刻 212 个节点里 124 个 available。链路是 `shape_drift.go:306 d.Halt = armed && !Trial` → `main.go:3284 if drift.Halt { … break }`：**既不 mark 也不换节点**，下一条请求 `pick()` 又选回同一出口。闩本意是保护出口，结果制造了正反馈。

**结论：不是缺一个更聪明的分数，是缺一套分层正确、状态唯一、可观测的系统。**

## 2. 统一模型

### 2.1 实体（R8-1 修正：配额单位是 egress，不是 pair）

```text
Egress   出口身份（node:<fp> 或 direct）。属性：IP、region、subscription、静态配置。
         承载：配额/熔断/生命周期/在途。配额是出口账号级，不随 model 复制。
Pair     (model, egress)。只承载：质量后验 + 模型级连续失败（consec429）。
Sub      订阅（红杏/…）。403 是订阅级信号。
Global   只保留 lastAny2xx 与告警时间，不参与裁决。
```

理由：4ad1982e（20 个 429 在 0.829s）与 653e51eb（1,106 个 429/11min）都是**同一出口跨 model 同时 429**。若预算按 pair 计，6 个 model 会把 8 次预算放大成 48 次，且 4ad 型突发会误封。

### 2.2 唯一流水线（所有尝试必须走这里）

```text
Admit(model, exclude map[egress]bool, now) -> Attempt{model, egress} | Reject{reason}
   ↓ 发送（流式/非流式/direct/trial 全部）
Observe(attempt, outcome, now)   // 每次发送恰好一次，defer 保证
   ↓
更新：egress 熔断/预算/生命周期/inflight、pair 后验/连续失败、sub、call_log、metrics
```

- `Admit` 是唯一选路入口；`egress.go` 不再自行回退直连（全冷却时返回 `NoCandidate`，不是 direct）。
- `Observe` 是唯一结果入口；两条 main 循环、direct、probe 都调用它。
- **R8-4：无租约、无 TTL、无跨出口持有**。attempt 在发送返回时即结束；`inflight` 是内存计数器，`defer` 减一，重启清零。
- `exclude` 由调用方传入：同一请求重试**禁止**返回刚失败的 egress（R8-③）。`Admit` 的返回必须满足 `until<=now && !sub.quarantined && lifecycle!=dead && inflight<K`。
- 抑制（Reject）不产生 attempt，不更新后验（I8）。

### 2.3 状态机（R8-1/2/3 修正）

**Egress 熔断**（单位：attempt；窗口：**精确 60s 时间戳队列**，`win429/winAtt` 各 cap 64）

```text
trip 条件（三条 AND，全部满足才熔断）：
  (a) 60s 内 n429 >= 3
  (b) n429 / nAtt >= 0.5 且 nAtt >= 3
  (c) now - last2xx > 10s（该出口 10s 内有过 2xx ⇒ 不熔断）
trip -> until = now + ladder[strikes]；strikes+1；trips24h 追加一条
ladder = 5m / 30m / 2h / 5h
恢复：probe 2xx -> strikes-1（到 0 时 until=0）；probe 429 -> strikes+1 并升档
24h 内 trips24h >= 3 -> hard_blocked(24h)（计「熔断次数」，不计原始 429 条数）
```

- **为什么加 (c)**：4ad1982e 的 429 前 4s 刚有 2xx，属模型级/瞬时，不能封出口；653e51eb 连续 11 分钟 0 个 2xx，第 3 次 429 时 `last2xx` 早已超过 10s，正常熔断。
- **为什么预算是 trips 不是 429 条数**：breaker 一旦 trip，后续同出口的 429 记 `suppressed429`、不进 `trips24h`，20 连发只算 1 次，天然免疫突发放大（R8-1）。
- `strikes=0` 时 `until` 必须为零，禁止写 `ladder[0]`。

**Pair 模型级失败**（保护用户延迟，覆盖 model-specific quota）

```text
pair.consec429 >= 3（中间无 2xx）-> pair.until = now + 5m
pair 任意 2xx -> consec429 = 0
pair.until 只影响该 (model, egress)，不参与 egress 的 trips24h / hard_block
```

**全局探测（单飞）**

```text
无候选时按 60s -> 2m -> 5m -> 15m -> 1h -> 6h 退避，任一 2xx 重置；
每次只发 1 个，选 until 最早的出口；probe 429 计入该出口 strikes 并升档。
不承诺「上游恢复后 ≤2min」，只承诺前两次探测间隔。
```

**Egress 生命周期**

```text
probation --3 次连通成功--> active
active --连续 5 次连接失败--> dead(30m) --到期--> probation
active --配额耗尽--> 不改变生命周期（配额是 egress 状态，不是节点死亡）
```

**Sub 状态**

```text
ok --403 确认（≥2 个不同 pair 或同一 pair 2 次）--> quarantine(10m) --到期--> 半开
quarantine 期间该 sub 的 egress 不进入候选
全部 sub 都 quarantine ⇒ 等价 no_candidate（503），不需要单独的 pool halt
```

**状态码（R8-②）**

| Reject.reason | HTTP | 头 | body |
|---|---|---|---|
| `no_candidate` | 503 | `Retry-After: 5` | `{"error":{"type":"no_candidate"}}` |
| `all_inflight` | 503 | `Retry-After: 1` | `{"error":{"type":"all_inflight"}}` |
| `sub_quarantine` | 503 | `Retry-After: 30` | `{"error":{"type":"sub_quarantine"}}` |

### 2.4 删除/合并清单

| 现有机制 | 处置 |
|---|---|
| `shape_drift.go` 全局闩 | **删除**；由 egress breaker + no_candidate 取代 |
| `modelQuota` map（未接选路） | 合并进 pair 状态 |
| `quota_halt.go`（direct 429） | direct 作为 egress，合并进 egress breaker |
| `proxyPool.poolHalted`（403 全局 halt） | 改为「sub 全 quarantine ⇒ no_candidate」；保留 403 归因 |
| `node_scoring.go` v2 分数 | **保留为展示口径**；选路改用 pair 后验（见 §3） |
| v5.1 的 canary / 租约 / 500ms 排队 / `NODE_SYSTEM=legacy` | **删除**（见 §12） |
| T3 超时分层 / idle_read | 不动 |

## 3. 选择层：per-pair 后验 + 有界探索

### 3.1 后验

```text
pair.a, pair.b  先验 a0=8, b0=2（新 pair = 80 分）
2xx        -> a += 1, lastOK=now, consec429=0
429/FUL    -> b += 1, lastFail=now, consec429+=1
403        -> 不计入 pair（订阅级），只记 sub
网络/超时  -> 不计入 pair（生命周期处理）
半衰期 7d，a/b 按 exp(-Δt/7d) 衰减
```

### 3.2 选择

```text
候选 = Admit 资格层放行的 pair（egress 未熔断/未 dead/非隔离 sub/inflight<K，且不在 exclude）
p    = (a+0.5)/(a+b+1)
n    = a+b-a0-b0
n<5  -> 冷启动优先（尝试次数最少；同一出口同时 1 个）
n>=5 -> UCB: p + 0.05*sqrt(2*ln(N)/n)
tie  -> lastOK 更近 > 近 1h 失败更少 > 轮询
sticky 会话粘住，失败才换；换出口排除同 sub、同 /24
```

**在途（R8-3 修正）**：`inflight` 按 **egress** 计，`K=8`（配置 `egress_inflight_max`）。**不排队**：全部候选都在途 ⇒ 立即 503 `all_inflight` + `Retry-After: 1`。

- N=1/C=32 ⇒ 同时上游 ≤8，其余 24 个在毫秒级拿到 503，而不是 8 个在途 + 214 个排队 500ms（后者在 00:24 会把 1,106 次 429 原样复现）。
- 稳态 N≥100 ⇒ 容量 ≥800，不会产生假 503。
- 排队被删除的理由：排队醒来时 pair 状态可能已变，需要重查，而重查等于重新 Admit；在 N=1 时排队只会把并发转成延迟，不提高成功率。

### 3.3 为什么不是静态总分
- 稳态 99%+ 成功率，任何排序都差不多；静态分反而引入噪声（−2.6pp）。
- 风暴日需要「快速降权 + 快速恢复」，只有在线后验 + probe 能做到。
- oracle +84.7pp 的上界要求持续探索；UCB 的探索项是有界的，且被 breaker 的 3/60s 兜底。

### 3.4 面板口径
- 节点卡片：`quality=100*a/(a+b)`（按流量加权聚合所有 model）、`n`、`Δ7d`、`lifecycle`、`quota_state`、`last_ok_age`、`auto_eligible`。
- 展开：该节点 × 各 model 的 pair 后验与最近失败原因。
- 缺数据「未提供」，不补 0；图内不写解释句。

## 4. 可观测

### 4.1 事件（call_log 新 schema，向后兼容）
```json
{"type":"admit","model":"...","egress":"node:fp","candidate_n":N}
{"type":"suppress","model":"...","reason":"no_candidate|all_inflight|sub_quarantine","would_be_egress":"...","candidate_n":N}
{"type":"outcome","model":"...","egress":"...","class":"2xx|429|403|network|timeout|5xx|cancel","ms":123}
{"type":"breaker","egress":"...","strikes":2,"until":"...","trips24h":2}
{"type":"lifecycle","egress":"...","from":"active","to":"dead"}
{"type":"pair_trip","model":"...","egress":"...","consec429":3}
```

### 4.2 指标（/api/stats 扩展）
- `admit_total{result}`、`suppress_total{reason}`、`outcome_total{class}`；
- `egress_trips_24h{egress}`、`breaker_active`、`pair_unknown`、`inflight{egress}`；
- `success_rate_5m`、`last_any_2xx_age`。

### 4.3 面板新增
- 顶部健康条：5m 成功率、最近 2xx 时间、活跃熔断数、被隔离订阅数；
- 节点表：资格、quality、n、quota、最近失败；
- 事件流：admit/suppress/breaker/lifecycle/pair_trip（可过滤）。

## 5. 配置（全部集中，带默认值）

| 键 | 默认 | 说明 |
|---|---|---|
| `egress_trip_429_60s` | 3 | 60s 内 429 次数阈值 |
| `egress_trip_ratio` | 0.5 | 尝试占比阈值 |
| `egress_trip_min_att` | 3 | 占比判定的最小尝试数 |
| `egress_trip_quiet` | 10s | 距最近 2xx 的最小间隔（有 2xx 不熔断） |
| `egress_ladder` | 5m/30m/2h/5h | 冷却递进 |
| `egress_trips_24h_cap` | 3 | 24h 熔断次数上限 → hard_block(24h) |
| `pair_consec_429` | 3 | 模型级连续 429 阈值 |
| `pair_cooldown` | 5m | 模型级冷却 |
| `egress_inflight_max` | 8 | 每出口在途上限（egress 级，唯一口径） |
| `global_probe_interval` | 60s | 无候选时首次探测间隔 |
| `sub_quarantine` | 10m | 订阅隔离时长 |
| `lifecycle_dead_fails` | 5 | 连续连接失败判死 |
| `lifecycle_dead_ttl` | 30m | 判死冷却 |
| `ucb_c` | 0.05 | 探索系数 |
| `cold_start_n` | 5 | 冷启动样本阈值 |
| `pair_half_life` | 7d | 后验衰减 |

## 6. 数据与迁移（R8-5 修正：schema 能表达规格）

- 新状态文件 `node_state_v5.json`（原子写 tmp+rename；损坏 fail-open 并告警）：

```json
{"version":5,
 "pairs":{"model|egress":{"a":8,"b":2,"lastOK":0,"lastFail":0,"consec429":0,"until":0}},
 "egress":{"fp":{"lifecycle":"active","consecFail":0,"deadUntil":0,"lastProbe":0,
   "strikes":0,"until":0,"last2xx":0,"win429":[],"winAtt":[],"trips24h":[]}},
 "subs":{"sub":{"until":0,"reason":"403","pairs":["model|egress"]}}}
```

- 时间一律 int64 毫秒。`win429/winAtt` cap 64（60s 窗口），`trips24h` cap 16（24h），超出丢弃最旧；`inflight` **不持久化**。
- 上限：pair 最多 50k（活跃子集），空闲 30min 清理，**不淘汰 armed/blocked**。
- 迁移：旧 `NodeExhausted` 且 quota ⇒ `egress.strikes=1, until=now+5m`（不保留 5h）；保留 dead/rejected；旧 `shape_drift` 状态丢弃。
- **重启语义**：`until/strikes/trips24h` 保留；60s 窗口与 `inflight` 丢失，重启后最坏多放行 K 个请求——接受并在 §10 声明。
- `proxy_state.json` 保持 v1 不动（节点静态配置），避免回滚丢状态。

## 7. 上线：影子 → 强制（R8-4 修正：没有 legacy 运行时开关）

1. **影子模式**（`NODE_SYSTEM=v5-shadow`，默认 24h）：v5 只做决策记录，不改变实际发送；对比「v5 会选谁」与「实际选谁」，统计分歧率与预测成功率。
2. **强制**（`NODE_SYSTEM=v5`）：Admit/Observe 接管；观察 30min 指标。
3. **回滚 = 重新部署上一镜像**：`docker compose pull ghcr.io/shiyanyx/opencode2api:sha-<prev> && up -d`。**不保留 `NODE_SYSTEM=legacy` 代码路径**——两套状态机并存正是 R8-4 指出的互斥根源；镜像回滚 2 分钟内完成，且 `node_state_v5.json` 不被旧版读取。
4. 回滚判据（强制后 10min 内任一满足）：成功率低于基线 10pp / 可用出口 <20 / 单出口 24h 发送 >10 / 全局 503 比例 >5%。

## 8. 切片与验收

| 切片 | 内容 | 验收 |
|---|---|---|
| **N1 数据层** | pair/egress/sub 状态、clock 注入、`node_state_v5.json`、迁移、影子记录 | `TestR8StateRoundTrip`、`TestR8RestartBlocked`、`TestR8MemoryCap`、`TestR8Migration190` |
| **N2 资格层** | Admit/Observe、egress breaker（3 AND）、trips24h、lifecycle、sub quarantine、no_candidate/all_inflight 503、inflight | `TestR8PairBreaker_4ad`、`_653e`、`_0024`、`TestR8EgressBudget`、`TestR8InflightScope`、`TestR8Observe429AlwaysMarksPair`、`TestR8NoUpstreamOnSuppress` |
| **N3 选择层** | pair 后验、UCB、冷启动、sticky、同 sub//24 排除 | 回放：稳态不低于基线 0.5pp；风暴日 ≥+5pp；每出口 429 ≤ 基线；可用出口不降 >10% |
| **N4 可观测** | 事件、指标、面板健康条/节点表/事件流 | 面板数字与 call_log 重算一致；缺数据显示「未提供」 |
| **N5 切换** | 影子 24h → 强制 → 镜像回滚演练 | 影子分歧报告；回滚 5 分钟内完成 |

依赖：N1 → N2 → N3；N4 可与 N2/N3 并行；N5 最后。

**N2 的硬断言（来自 R8 §4.1.1 与 00:24 证据）**

1. `TestR8PairBreaker_0024`：222 并发 / N=1 ⇒ 上游发送 ≤K；第 3 个 429 之后所有 `Admit` 都不返回该 egress。
2. `TestR8Observe429AlwaysMarksPair`：在 sub quarantine / global probe / direct 三种上下文各注入 429，断言 egress `strikes>0` 或 pair `consec429>0`，且下一次 `Admit` 不返回同一 egress；无候选返回 503。
3. `TestR8InflightScope`：N=1、6 model、C=32 ⇒ 同时上游 ≤8（不是 48），其余立即 503。
4. `TestR8PairBreaker_4ad`：20 个 429（前 4s 有 2xx）⇒ 不熔断 egress；对应 pair 在第 3 个连续 429 后 `until>0`。
5. `TestR8PairBreaker_653e`：连续 429 且 10s 无 2xx ⇒ 第 3 次熔断；到第 3 次 trip 时上游发送 ≤ K+3。

## 9. 测试策略

- 单元：状态机迁移（ladder、trips24h、lifecycle、sub、pair consec429）。
- 并发：100 并发同一 egress 只产生 ≤K 上游；inflight 归零；N=1 时无排队泄漏。
- 回放：10-26/10-27 全量日志 + fake clock，跑 18:03、00:20、稳态三段。
- 混沌：全出口 429、单出口 1,106 连发、403 风暴、网络抖动、上游 5xx、客户端取消。
- 变异：每切片 ≥8 个变异必须全红。
- 影子对比：v5 决策 vs 实际决策的分歧率、预测校准（Brier）。

## 10. 风险与未决

1. **per-pair 状态量**：512×212 最坏 10 万 pair；用活跃子集 + 上限 5 万 + 保护 blocked 条目。
2. **UCB 探索烧 IP**：探索被 breaker 的 3/60s 与 trips24h 兜底；影子期先量化。
3. **sub quarantine 误伤**：403 归因需 ≥2 个不同 pair 或同 pair 2 次；影子期观察误伤率。
4. **重启丢 60s 窗口**：最坏多放行 K 个请求；接受，不做持久化。
5. **面板口径变化**：quality 从「加权总分」变为「后验均值」，需要标注样本量。
6. **T7 研究未完成**：AUC/校准/MNAR 还没做；N3 先按本方案实现，T7 完成后校准参数。
7. **egress 级熔断的粗粒度**：若上游配额其实按 model 独立，egress 熔断会误伤同出口的其他 model；影子期用 `pair.consec429` 分布验证，必要时把 (a)(b) 降为 pair 级、只保留 trips 预算在 egress 级。

## 11. R7 复审（v4.1）到 v5.1 的对照

| R7 条目 | v5.1 处置 | v5.2 现状 |
|---|---|---|
| **Critical-1** 计数单位/窗口 | attempt + 精确 60s 队列 + AND 触发 | 保留；新增 (c) 10s 静默条件 |
| **Critical-2** N=4/C=31 容量 | K=8 + 排队 500ms | **排队删除**；egress 级 K=8 + 立即 503 |
| **Critical-3** canary 伪代码 | 互斥分支 + 租约 token | **canary 与租约整体删除**，恢复只走 probe |
| **Critical-4** 探测与预算冲突 | 退避 60s→6h | 保留；probe 429 计入 strikes |
| **Critical-5** strikes=0 / 直连回退 | `strikes=0 ⇒ until=0`；禁直连 | 保留 |
| **High-1** 租约生命周期 | TTL 10min | **租约删除** |
| **High-2** 190 个 5h 迁移 | strikes=1, until=+5m | 保留（迁到 egress） |

## 12. R8 复审（v5.1）到 v5.2 的决议

| R8 条目 | v5.2 处置 |
|---|---|
| **1** 24h 预算按 pair 会封死 4ad，且随 model 数放大 | 配额升到 **egress**；预算改为 **trips24h（熔断次数）**，突发只计 1 次；新增 (c) 10s 内有过 2xx 不熔断；model 级由 `pair.consec429` 独立处理 |
| **2** K=8 作用域未定义（正文 8 / 配置 4 / 状态无 egress inflight） | 统一 **egress 级 K=8**，唯一配置键 `egress_inflight_max`；`inflight` 在 egress 状态内、不持久化 |
| **3** canary 无 normative 分支，190 个 5h 出口可能永不恢复 | **删除 canary**；恢复 = probe 单飞 + strikes 递减；迁移 `until=+5m` 后进入正常探测 |
| **4** legacy 与删除清单互斥 | **删除 `NODE_SYSTEM=legacy`**；回滚改为重新部署上一镜像；状态文件版本隔离 |
| **5** schema 存不下规格（int 无法表达 60s 队列/24h 滚动） | `win429/winAtt/trips24h` 全部改为 int64 毫秒数组并写明 cap；`inflight` 不落盘；声明重启丢窗口 |
| **①** 门拦切换时必须阻止继续选同一出口 | `Admit` 契约 + `exclude` 参数；`TestR8PairBreaker_0024` |
| **②** 429 必须 mark 并换，无候选 503 | 三条状态码表；`TestR8Observe429AlwaysMarksPair` |
| **③** N=1 时排队无意义 | 删排队；`TestR8InflightScope` |
