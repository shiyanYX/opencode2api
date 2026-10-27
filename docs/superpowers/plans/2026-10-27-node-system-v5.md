# 节点系统 v5.6：一次全局重构（资格 · 选择 · 可观测）

> **状态：DRAFT v5.6，B3/B4 已修，待终审。** 相对 v5.2 的核心修正：熔断从「60s 窗口 + 比例」改成 **「首个 429 即冷却」**，单位回到 **pair**。依据是真实日志回放：653e×mimo 的 429 从 **1,106 → ≤8（回放 4–5，同毫秒事件顺序不同）**，41d×mimo **402 → ≤8（回放 3–4）**，4ad×mimo **20 → 1**，而 653e×deepseek 的 **0 个 429 完全不受影响**。
> 前情：T1/T2/T3 已写进 `test`（代码提交 `da85f2b`），生产仍是 10-26 15:16 旧镜像。v3.x 被 R3/R4/R5 判过度设计；v4.1 被 R6 认可；v5/v5.1 被 R8 判 NO-GO；v5.2 被 R8-delta 判 6 个 blocker。

## 1. 现状为什么必须重构

| 现象 | 根因 |
|---|---|
| 闩死后每个请求仍发一次上游 | 全局闩只改「探测/正常」模式，不阻止发送 |
| TTL 清空 blocked ⇒ 每 10 分钟重扫全池 | `shape_drift.go` 用 TTL 当恢复手段，烧 79–127 IP/小时 |
| 一个出口 429，全池停摆 | 全局闩 + pool403 都是全局状态 |
| 恢复靠 5h 冷却到期 | 没有「单飞探测」，只有等 |
| 打分算得再准，稳态也没收益 | 静态总分选路 0~−2.6pp；oracle +84.7pp 需要在线自适应 |
| 同一节点对不同模型表现相反，分数却只有一个 | 状态按节点聚合，没按 (model, 出口) 分开 |
| 面板数字对不上日志 | 抑制、未尝试、429 归因混在一起 |

**决定性证据（10-27 00:15–00:45 生产日志复算）**

- 1,847 请求 → **1,529 次上游 429**，只有 3 个 switch、1 个 `shape_drift` 事件。
- 单个出口 `653e51eb` 在 11 分钟内挨 **1,106 次 429**，同一时刻 212 个节点里 124 个 available。
- 但 `653e51eb` **不是坏出口**：同期它给 deepseek 服务 5 次全部 2xx，给 mimo 是 182 次 2xx / 1,106 次 429（86% 失败）。**失败是 (model, 出口) 级的，不是出口级的。**
- `41d39652` 两个 model 都是 0 成功（mimo 402×429、deepseek 10×429），是真坏。
- 链路：`shape_drift.go:306 d.Halt = armed && !Trial` → `main.go:3284 if drift.Halt { … break }`：**既不 mark 也不换节点**，下一条请求 `pick()` 又选回同一出口。闩本意是保护出口，结果制造了正反馈。

**结论：不是缺一个更聪明的分数，是缺一套分层正确、状态唯一、可观测的系统；熔断必须落在 pair 上，并且必须在第一个 429 就生效。**

## 2. 统一模型

### 2.1 实体

```text
Egress   出口身份（node:<fp> 或 direct）。属性：IP、region、subscription。
         承载：生命周期、在途（inflight）、订阅归属。direct 特殊：429 时整条直连冷却。
Pair     (model, egress)。承载：质量后验、熔断（strikes/until/trips24h）。
         配额与限流按 (model, 出口) 生效——653e×mimo 失败、653e×deepseek 正常即为证据。
Sub      订阅（红杏/…）。403 是订阅级信号。
Global   只保留 lastAny2xx 与告警时间，不参与裁决。
```

### 2.2 唯一流水线（所有尝试必须走这里）

```text
Admit(model, exclude map[egress]bool, now) -> Attempt{model, egress} | Reject{reason}
   ↓ 发送（流式/非流式/direct/probe 全部）
Observe(attempt, outcome, now)   // 每次发送恰好一次，defer 保证
   ↓
更新：pair 后验/熔断、egress 生命周期、sub、inflight、call_log、metrics
```

- `Admit` 是唯一选路入口；`egress.go` 不再自行回退直连（全冷却返回 `NoCandidate`）。
- `Observe` 是唯一结果入口；两条 main 循环、direct、probe 都调用它。
- **无租约、无 TTL、无跨出口持有**。`inflight` 是每 egress 一个**带缓冲 channel 信号量**（cap=K），`Admit` 非阻塞 `select` 取名额，`Observe` 释放。这是原子预留，不存在 check-then-act 竞态（R8-delta D2）。

```text
Admit:
  select sem <- struct{}{} ; default -> Reject(all_inflight)
  defer func(){ if rejected { <-sem } }()      // 取到名额后任何校验失败都立即归还
  校验 pair.until / lifecycle / sub / exclude
  返回 Attempt{model, egress, release: sync.Once{ <-sem }}
Observe(attempt, outcome, now):
  defer attempt.release()                       // exactly-once，第二次 Observe 直接返回
  请求路径统一 defer Observe(Attempt{}, abandoned) // panic/提前 return 也不泄漏
probe:
  probeMu 只保护 nextProbeAt / probing 两个字段，网络调用在锁外
  Observe(probe) 结束时 probing=false，并推进 nextProbeAt
```
- `Admit` 返回的 egress 必须满足：`pair.until<=now && egress.lifecycle!=dead && !sub.quarantined && !exclude[egress] && inflight 名额已取到`。
- pair 查询顺序：`model|egress` → 未命中回退 `*|egress`（迁移占位，见 §6）。
- 抑制（Reject）不产生 attempt，不更新后验（I8），且必须归还已取名额。

### 2.3 状态机

**Pair 熔断（核心规则：第一个 429 就冷却）**

```text
任意 429（含 FreeUsageLimitError）
  -> strikes = min(strikes+1, 4)
  -> until   = now + ladder[strikes-1]      // 60s / 2m / 5m / 5m
  -> trips24h 追加一条
任意 2xx
  -> strikes = 0；hard = 0；until = 0
strikes 将要超过 4（即连续第 5 次 429，期间无任何 2xx）
  -> hard = min(hard+1, 6)
  -> until = now + min(1h * 2^(hard-1), 24h)   // 1h/2h/4h/8h/16h/24h
  -> strikes 保持 4
probe 429
  -> 不升 strikes/hard；until = max(until, now + probe_backoff)   // 只延后，绝不缩短已有的 1h 硬封
```

- **不再用 60s 窗口、不再用比例、不再用 nAtt。** 理由：真实数据里 653e×mimo 在 60s 窗口内是 28×429 / 28×2xx（正好 50%），任何比例阈值都会在「熔断」和「放行」之间摇摆；而「首个 429 冷却 60s」在回放中把该 pair 的上游 429 从 1,106 降到 **≤8**（实测 4–5，取决于同毫秒事件顺序），且完全不误伤同出口的 deepseek。
- 单次冷却很短（60s），对 124 个候选的池子代价可忽略；重复失败自动升档，最长 5m。
- `strikes` 上限 4，`ladder` 长度 4，越界不写（R8-delta D6）。
- **hard block 只看连续失败，不看 24h 累计**：`strikes` 只有在 2xx 时才归零，所以「第 5 次连续 429」意味着 4 轮冷却（60s+2m+5m+5m≈13min）到期后的第一个请求仍然 429、期间没有任何 2xx——这是真死。一天里偶发 6 次孤立 429（每次都有 2xx 隔开）永远不会触发 hard block（R8-delta-2 B1）。
- **hard block 从 1h 起翻倍、封顶 24h，且 probe 失败不升级**：模型级配额风暴会让所有 pair 同时连续 429；若首次就封 24h，风暴过去后仍会瘫痪一天。1h 起步 + probe 探测（不升 strikes/hard）保证最坏 1h 内自动复测，任意 2xx 清零 `strikes/hard`。
- `trips24h` 只用于面板与告警，**不参与任何裁决**；按 24h 滚动清理（cap 16 仅作内存上限）。
- `until` 只影响该 pair；同一 egress 的其他 model 不受影响（653e×deepseek 0 个 429）。
- **direct**：`pair=(model,direct)`，但 429 时额外设置 `direct.until = now + 60s`（顶层键），所有 model 的 direct pair 一并排除——直连是本机 IP，不按 model 分账。

**恢复：2xx 即恢复（不设 canary）**

```text
冷却到期后第一个 2xx -> strikes=0, until=0
无流量时由 probe 触发：单飞、退避 60s -> 2m -> 5m -> 15m -> 1h -> 6h
probe 2xx -> 同「2xx 即恢复」；probe 429 -> 只推进 probe 退避，不升 strikes/hard
```

- 删除 canary：真实流量的 2xx 就是最强的恢复证据；190 个被旧版标 5h 的出口迁移为 `strikes=1, until=now+5m`，到期后正常流量或 probe 的 2xx 即清零（R8-delta D3）。
- probe 单飞：`probeMu` + `nextProbeAt`，全局同时最多 1 个 probe；`Observe(probe)` 与普通 `Observe` 共用同一条恢复路径。

**无候选 / 全冷却时的探测（B3）**

```text
min_until(model) = min(
    { pair.until | pair.until > now },                              // 该 model 仍在冷却的 pair
    { sub.until  | sub.until  > now, sub 拥有该 model 的候选 egress }, // 订阅隔离
    { egress.deadUntil | > now }                                    // 出口判死
  )
  三者都为空（全在途）-> Retry-After = 1

AdmitProbe(model, egress):
  与 Admit 共用同一信号量，但**跳过 pair.until 与 sub.until**（探测的目的就是验证冷却/隔离是否该结束）
  仍然要求：egress.lifecycle != dead、inflight 有名额（probe 最多占 K 中的 1 个）
  必须带 deadline（20s）：超时按 `Observe(probe, timeout)` 归还名额
  probe 2xx -> strikes=0/hard=0/until=0，并清除该 sub 隔离；probe 403 -> 刷新 sub 隔离
AdmitLifecycleProbe(egress):
  忽略 deadUntil（探测出口是否复活），只要求 inflight 有名额，同样带 20s deadline
  probe 结果走 Observe，与正常流量同一条恢复路径（2xx -> strikes=0）

触发：
  1. 无流量时：now >= nextProbeAt -> 选 until 最早、strikes 最低的 pair
  2. no_candidate 时：now >= nextProbeAt -> 同上，单飞，不占用户请求名额
  3. 全部 egress dead：改为 lifecycle probe（probation），不经过 pair
退避：60s -> 2m -> 5m -> 15m -> 1h -> 6h；任意 2xx 重置为 60s
```

- `no_candidate` 的 `Retry-After = max(1, min_until(model) - now)`；`min_until` 为空（全在途）时取 1（B3 定义）。
- probe 单飞：`probeMu` 只保护 `nextProbeAt/probing`，网络调用在锁外；`probing=true` 期间不再发起第二个 probe。

**Egress 生命周期**

```text
probation --3 次连通成功--> active
active --连续 5 次连接失败--> dead(30m) --到期--> probation
配额 429 不改变生命周期（配额是 pair 状态，不是节点死亡）
```

**Sub 状态**

```text
ok --403 确认（≥2 个不同 pair 或同一 pair 2 次）--> quarantine(10m) --到期--> 半开
quarantine 期间该 sub 的 egress 不进入候选；全部 sub 隔离 ⇒ no_candidate(503)
```

**状态码**

| Reject.reason | HTTP | 头 | body |
|---|---|---|---|
| `no_candidate` | 503 | `Retry-After: max(1, min_until(model)-now)`，空则 1 | `{"error":{"type":"no_candidate"}}` |
| `all_inflight` | 503 | `Retry-After: 1` | `{"error":{"type":"all_inflight"}}` |
| `sub_quarantine` | 503 | `Retry-After: 30` | `{"error":{"type":"sub_quarantine"}}` |

### 2.4 删除/合并清单

| 现有机制 | 处置 |
|---|---|
| `shape_drift.go` 全局闩 | **删除**；由 pair 冷却 + no_candidate 取代 |
| `modelQuota` map（未接选路） | 合并进 pair |
| `quota_halt.go`（direct 429） | 并入 `pair=(model,direct)` + 顶层 `direct.until` |
| `proxyPool.poolHalted`（403 全局 halt） | 改为「sub 全隔离 ⇒ no_candidate」；保留 403 归因 |
| `node_scoring.go` v2 分数 | **保留为展示口径**；选路改用 pair 后验 |
| v5.1 的 canary / 租约 / 500ms 排队 / `NODE_SYSTEM=legacy` | **删除** |
| v5.2 的 egress 级熔断 / 60s 窗口 / trips 分类 | **删除**（被 v5.3 首个 429 规则取代） |
| T3 超时分层 / idle_read | 不动 |

### 2.5 与现有代码的落点

| 现有符号 | 处置 |
|---|---|
| `nodePool.pick/pickWithMeta/pickWithMetaExcluding`（`proxy_node.go:566-583`） | 拆成 `Admit` 的候选枚举；`force` 参数删除，粘滞由会话层传入 `exclude` |
| `nodePool.poolHaltUntil/poolHalt/poolHalted`（`proxy_node.go:270,872-902`） | **删除**；403 全局停摆改由 sub quarantine + `no_candidate` 表达 |
| `nodePool.sub403/quarantineSub`（`proxy_node.go:264-283,739-777`） | **保留**，与 v5 `subs` 合并为唯一一份；退避 10/20/30m 不变 |
| `shape_drift.go` 的 `Halt/Trial`（`shape_drift.go:306`） | **删除闩**；drift 指标保留为纯观测 |
| `main.go` 两处重试循环（`drift.Halt` 分支） | 改为 `Admit`→发送→`Observe`；删除 `if drift.Halt { break }` |
| `egress.go` 直连回退 | 全冷却**不再回退直连**，返回 `NoCandidate`；direct 本身是 `pair=(model,direct)` |
| `node_scoring.go` v2 分数 | 保留为面板展示口径，**选路不再读取** |
| T3 超时分层 / `idle_read.go` | 不动 |

## 3. 选择层：per-pair 后验 + 有界探索

### 3.1 后验

```text
pair.a, pair.b  先验 a0=8, b0=2（新 pair = 80 分）
2xx        -> a += 1, lastOK=now
429/FUL    -> b += 1, lastFail=now
403        -> 不计入 pair（订阅级），只记 sub
网络/超时  -> 不计入 pair（生命周期处理）
半衰期 7d：只衰减证据、先验不动（与 `scoringHalfLifeDays` 同口径）：a=a0+(a-a0)f、b=b0+(b-b0)f，f=0.5^(Δt/7d)，记录 lastDecay。n 衰减；p 单调收敛到先验 p0=(a0+0.5)/(a0+b0+1)=8.5/11≈0.7727。平滑项使 p 不可能严格不变：令 S=a+b（证据总量），衰减 f 后 |p-p0| 的比值为 f·(11+S)/(11+fS)——S 越大越接近 1，即衰减主要缩小 n、对 p 影响很小
```

### 3.2 选择

```text
候选 = Admit 资格层放行的 pair（pair 未冷却、egress 未 dead、sub 未隔离、inflight 有名额、不在 exclude）
p    = (a+0.5)/(a+b+1)
n    = a+b-a0-b0
n<5  -> 冷启动优先（尝试次数最少；同一出口同时 1 个）
n>=5 -> UCB: p + 0.05*sqrt(2*ln(N)/n)
tie  -> lastOK 更近 > 近 1h 失败更少 > 轮询
sticky 会话粘住，失败才换；换出口排除同 sub、同 /24
```

**在途**：`inflight` 按 egress 计，`K=8`（`egress_inflight_max`），**不排队**。全候选都在途 ⇒ 立即 503 `all_inflight` + `Retry-After: 1`。N=1/C=32 ⇒ 同时上游 ≤8，其余 24 个毫秒级 503。

### 3.3 为什么不是静态总分
- 稳态 99%+ 成功率，任何排序都差不多；静态分反而引入噪声（−2.6pp）。
- 风暴日需要「首个 429 立即降权 + 2xx 立即恢复」，只有在线后验 + 冷却能做到。
- oracle +84.7pp 的上界要求持续探索；UCB 的探索项有界，且被 pair 冷却兜底。

### 3.4 面板口径
- 节点卡片：`quality=100*a/(a+b)`（按流量加权聚合所有 model）、`n`、`Δ7d`、`lifecycle`、`quota_state`、`last_ok_age`、`auto_eligible`。
- 展开：该节点 × 各 model 的 pair 后验、`strikes`、`until`、最近失败原因。
- 缺数据「未提供」，不补 0；图内不写解释句。

## 4. 可观测

### 4.1 事件（call_log 新 schema，向后兼容）
```json
{"type":"admit","model":"...","egress":"node:fp","candidate_n":N}
{"type":"suppress","model":"...","reason":"no_candidate|all_inflight|sub_quarantine","would_be_egress":"...","candidate_n":N}
{"type":"outcome","model":"...","egress":"...","class":"2xx|429|403|network|timeout|5xx|cancel","ms":123}
{"type":"pair_cool","model":"...","egress":"...","strikes":2,"until":"...","trips24h":3}
{"type":"lifecycle","egress":"...","from":"active","to":"dead"}
```

### 4.2 指标（/api/stats 扩展）
- `admit_total{result}`、`suppress_total{reason}`、`outcome_total{class}`；
- `pair_cool_active`、`pair_trips_24h{pair}`、`inflight{egress}`；
- `success_rate_5m`、`last_any_2xx_age`。

### 4.3 面板新增
- 顶部健康条：5m 成功率、最近 2xx 时间、活跃冷却 pair 数、被隔离订阅数；
- 节点表：资格、quality、n、quota、最近失败；
- 事件流：admit/suppress/pair_cool/lifecycle（可过滤）。

## 5. 配置（全部集中，带默认值）

| 键 | 默认 | 说明 |
|---|---|---|
| `pair_cooldown_ladder` | 60s/2m/5m/5m | 首个 429 起的冷却递进，长度 4 |
| `pair_trips_24h_cap` | 16 | 仅内存上限/展示，不参与裁决；hard block 由「连续第 5 次 429」触发 |
| `pair_hard_block` | 1h | 硬封基础时长，按 2 倍递增，封顶 24h |
| `egress_inflight_max` | 8 | 每出口在途上限（唯一口径） |
| `direct_cooldown` | 60s | 直连 429 后整条直连冷却 |
| `global_probe_interval` | 60s | 无流量时首次探测间隔，退避至 6h |
| `sub_quarantine` | 10m | 订阅隔离时长 |
| `lifecycle_dead_fails` | 5 | 连续连接失败判死 |
| `lifecycle_dead_ttl` | 30m | 判死冷却 |
| `ucb_c` | 0.05 | 探索系数 |
| `cold_start_n` | 5 | 冷启动样本阈值 |
| `pair_half_life` | 7d | 后验衰减 |

## 6. 数据与迁移

```json
{"version":5,
 "epoch":"2026-10-27T12:00:00+08:00",
 "pairs":{"model|egress":{"a":8,"b":2,"lastOK":0,"lastFail":0,"lastDecay":0,
                          "strikes":0,"hard":0,"until":0,"trips24h":[]}},
 "egress":{"fp":{"lifecycle":"active","consecFail":0,"deadUntil":0,"lastProbe":0}},
 "direct":{"until":0},
 "subs":{"sub":{"until":0,"reason":"403","n403":0,"last403":0,"pairs":[]}}}
```

- 时间一律 int64 毫秒；`trips24h` 按 24h 滚动清理、cap 16（仅内存上限，不参与裁决）；`inflight` **不持久化**。
- `direct` 的 pair key 写作 `model|direct`；egress 段不存 `direct`（无生命周期），整条直连的冷却存顶层 `direct.until`。
- 上限：pair 最多 50k（活跃子集），空闲 30min 清理，**不淘汰 blocked**。
- 迁移：旧 `NodeExhausted` 且 quota ⇒ `*|node:<fp>` 占位 pair，`strikes=1, until=now+5m`；保留 dead/rejected；旧 `shape_drift` 状态丢弃。
- **`*` 是迁移占位**（v2 没有 model 维度）：`Admit` 查 `model|egress` 未命中时回退读 `*|egress` 的 `strikes/until`（只读，用于挡住 5 分钟）；该 pair 首次真实 outcome 后写入具体 `model|egress`，并删除 `*|egress`。
- **重启语义**：`strikes/until/trips24h` 保留；`inflight` 清零，最坏多放行 K 个请求（接受）。
- 回滚后再升级：`node_state_v5.json` 带 `epoch`（部署时间戳），版本不匹配则整体丢弃重建（R8-delta Q4）。

## 7. 上线：影子 → 强制

1. **影子**（`NODE_SYSTEM=v5-shadow`，默认 24h）：只记录决策，不改变发送；对比分歧率与预测成功率。
2. **强制**（`NODE_SYSTEM=v5`）：Admit/Observe 接管；观察 30min。
3. **回滚 = 重新部署上一镜像**（`sha-<prev>`），**不保留 `legacy` 代码路径**；`node_state_v5.json` 带 epoch，旧版不读、新版不继承。
4. 回滚判据（10min 内任一）：成功率低于基线 10pp / 可用出口 <20 / 单 pair 24h 发送 >10 / 全局 503 比例 >5%。

## 8. 切片与验收

| 切片 | 内容 | 验收 |
|---|---|---|
| **N1 数据层** | pair/egress/sub 状态、clock 注入、`node_state_v5.json`、迁移、影子记录 | `TestR8StateRoundTrip`、`TestR8RestartBlocked`、`TestR8MemoryCap`、`TestR8Migration190` |
| **N2 资格层** | Admit/Observe、pair 首个 429 冷却、trips24h、lifecycle、sub、no_candidate/all_inflight、inflight 信号量 | `TestPairCoolOnFirst429`、`TestReplay653e`、`TestReplay41d`、`TestReplay4ad`、`TestInflightAtomic`、`TestDirectCooldown` |
| **N3 选择层** | pair 后验、UCB、冷启动、sticky、同 sub//24 排除 | 回放：稳态不低于基线 0.5pp；风暴日 ≥+5pp；每 pair 429 ≤ 基线 |
| **N4 可观测** | 事件、指标、面板健康条/节点表/事件流 | 面板数字与 call_log 重算一致；缺数据显示「未提供」 |
| **N5 切换** | 影子 24h → 强制 → 镜像回滚演练 | 影子分歧报告；回滚 5 分钟内完成 |

**N2 硬断言（全部用真实日志时间戳回放，不用合成夹具）**

> 断言只对**被观测 pair** 成立；把 1,106 次请求改派到别的 pair 后对方会怎样是反事实、不可知，所以**总 429 数不作为 N2 硬断言**，由影子期（§7）实测。

| 断言 | 期望 |
|---|---|
| `TestReplay653e`：10-27 00:15–00:45，pair `653e51eb×mimo` | 上游 429 ≤ **8**（当前 1,106）；`653e51eb×deepseek` 429 = **0** |
| `TestReplay41d`：同期 pair `41d39652×mimo` | 上游 429 ≤ **8**（当前 402） |
| `TestReplay4ad`：10-26 18:00–18:10，pair `4ad1982e×mimo` | 上游 429 ≤ **4**（当前 20，全部为已在途请求） |
| `TestReplay0024`：00:24 222 并发 / N=1 | 上游发送 ≤ K；第 1 个 429 后 `Admit` 不再返回该 pair |
| `TestInflightAtomic`：32 goroutine、N=1 | 恰好 8 个上游、24 个 `all_inflight` 503，无竞态（`-race`） |
| `TestDirectCooldown`：direct 429 | 所有 model 的 direct pair 同时冷却 60s |
| `TestPairCoolNoCrossModel`：653e×mimo 429 | 653e×deepseek 仍可选 |
| `TestAllCooling`：43 个 pair 同时 429 | 503 带有限 `Retry-After`，probe 60s 内发起且能恢复 1 个 pair |
| `TestAllSubQuarantined`：全部 sub 隔离 | `Retry-After` = sub 最早到期，不是 1；probe 能穿过隔离并清除它 |
| `TestProbeDeadline`：probe 卡住 | 20s 后归还名额，`inflight` 回到 0 |
| `TestHardBlockBounded`：模型级风暴 | 1h→2h→4h→8h→16h→24h 共 6 档；probe 429 不缩短已有封禁 |
| `TestProbeBypassesCooldown`：pair 冷却中 | `AdmitProbe` 可选中该 pair，普通 `Admit` 不可 |

## 9. 测试策略

- 单元：冷却阶梯、strikes 上限、hard 1h→24h 递增、probe 失败不升级、2xx 清零、lifecycle、sub。
- 并发：`-race` 下 32/100 并发；信号量不泄漏；`sync.Once` 只释放一次。
- 回放：10-26/10-27 全量日志 + fake clock，跑 18:03、00:20、稳态三段；**断言值来自真实时间戳**。
- 混沌：全出口 429、单出口 1,106 连发、403 风暴、网络抖动、5xx、客户端取消。
- 变异：每切片 ≥8 个变异必须全红。
- 影子：分歧率、Brier。

## 10. 风险与未决

1. **首个 429 即冷却的误伤**：非配额型 429（如请求体问题）会冷却该 pair 60s；代价可接受，影子期统计「单次 429 后 60s 内该 pair 成功率」验证。
2. **per-pair 状态量**：512×212 最坏 10 万；活跃子集 + 上限 5 万 + 保护 blocked。
3. **UCB 探索烧 IP**：被 pair 冷却 + hard 1h 起步兜底；影子期量化。
4. **sub 隔离误伤**：403 归因需 ≥2 个不同 pair 或同 pair 2 次。
5. **重启丢 inflight**：最坏多放行 K 个请求；接受。
6. **T7 未完成**：AUC/校准/MNAR 未做；N3 先实现，T7 后校准。
7. **直连无 pair 级隔离**：direct 是共享 IP，只能整条冷却；若需要更细，后续再拆。

## 11. 历次评审对照

| 轮次 | 结论 | 关键处置 |
|---|---|---|
| R5（v4） | 8 blocker | 删全局闩，改按出口熔断 |
| R6（v4.1） | 认可方向 | 事务化 Admission、条件化五层 |
| R7（v4.1） | 5 Critical + 2 High | 精确 60s 队列、AND 触发、K=8、canary 规范、探测退避、strikes=0 |
| R8（v5.1） | NO-GO，5 硬矛盾 | 配额升 egress、trips24h、删 canary/租约/排队/legacy、schema 数组 |
| R8-delta（v5.2） | NO-GO，6 blocker | **v5.3 全部重做**：熔断回 pair、首个 429 冷却、删 60s 窗口与比例、信号量原子在途、probe 单飞、2xx 即恢复、epoch |

## 12. R8-delta 六个 blocker 到 v5.3 的处置

| Blocker | v5.3 |
|---|---|
| **D1** 653e 第 3 个 429 距最近 2xx 仅 4.32s，(c) 为假，第 3 次不熔断 | **不再依赖静默期或比例**：第一个 429 就冷却。回放实测 653e×mimo 429 1,106→≤8，且不再声称「0 个 2xx」——事实是 182×2xx / 1,106×429 |
| **D2** `inflight<K` 无原子预留 | 每 egress 一个 cap=K 的 channel 信号量，`select` 非阻塞取、`sync.Once` 释放；`TestInflightAtomic` 在 `-race` 下断言恰好 K |
| **D3** probe 无单飞；190 个正常 2xx 不减 strikes | `probeMu + nextProbeAt` 单飞；**任意 2xx 即 strikes=0**，不需要等 probe |
| **D4** `win*` cap 64 不精确、缺字段 | **删除 win 窗口**；新增 `lastDecay`、`sub.n403/last403`、顶层 `direct.until`、`epoch`；`trips24h` cap 16 |
| **D5** direct 429 未真正并入 | `pair=(model,direct)` + 顶层 `direct.until` 双写；`TestDirectCooldown` |
| **D6** egress 熔断误伤其他 model；strikes 越界；trips24h=3 过硬 | **熔断回 pair**，653e×deepseek 实测 0 个 429 不受影响；strikes 上限 4；hard block 只看「连续第 5 次 429」，有 2xx 即清零（v5.4） |
