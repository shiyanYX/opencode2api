# 节点系统 v5：一次全局重构（资格 · 选择 · 可观测）

> **状态：DRAFT v5.0，供用户审阅。** 这是把「熔断 + 打分 + 生命周期 + 403/quota + 面板」收进一个模型的整体方案，不是又一个补丁。
> 前情：T1（打分纠偏）、T2（403 遏制）、T3（超时分层）已上线；v3.x 被 R3/R4/R5 判过度设计；v4.1 熔断方向被 R6 认可、R7 复审中。v5 把 v4.1 作为 N2 纳入。

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

**结论：不是缺一个更聪明的分数，是缺一套分层正确、状态唯一、可观测的系统。**

## 2. 统一模型

### 2.1 实体

```text
Egress   出口身份（node:<fp> 或 direct）。属性：IP、region、subscription、静态配置。
Pair     (model, egress)。配额/限流的真实单位。状态：后验、熔断、配额。
Sub      订阅（红杏/…）。403 是订阅级信号。
Global   只保留 lastAny2xx 与告警时间，不参与裁决。
```

### 2.2 唯一流水线（所有尝试必须走这里）

```text
Admit(model, now) -> Lease{model, egress, kind: normal|canary|global_probe} | Reject{reason}
   ↓ 发送（流式/非流式/direct/trial 全部）
Observe(lease, outcome, now)   // exactly once，defer 保证
   ↓
更新：pair 后验、breaker、quota、sub 状态、call_log、metrics
```

- `Admit` 是唯一选路入口；`egress.go` 不再自行回退直连。
- `Observe` 是唯一结果入口；两条 main 循环、direct、canary、trial 都调用它。
- 抑制（Reject）不产生 attempt，不更新后验（I8）。

### 2.3 状态机

**Pair 配额/熔断**

```text
ok --(60s 内 ≥3 次 429 或 ≥4 次中 ≥50%)--> cooling(5m) --到期--> canary
canary --2 次 2xx(≥60s)--> ok（24h 最多 3 次清零；超出每日降 1 级）
canary --失败--> cooling 升档(30m/2h/5h) 
任意 --24h 内 8 次 429--> hard_blocked(24h)
```

**Egress 生命周期**

```text
probation --3 次连通成功--> active
active --连续 5 次连接失败--> dead(30m) --到期--> probation
active --配额耗尽--> 不改变生命周期（配额是 pair 状态，不是节点死亡）
```

**Sub 状态**

```text
ok --403 确认（≥2 个不同 pair 或同一 pair 2 次）--> quarantine(10m) --到期--> 半开
quarantine 期间该 sub 的 egress 不进入候选
全部 sub 都 quarantine ⇒ 等价 no_candidate（503），不需要单独的 pool halt
```

### 2.4 删除/合并清单

| 现有机制 | 处置 |
|---|---|
| `shape_drift.go` 全局闩 | **删除**；由 pair breaker + no_candidate 取代 |
| `modelQuota` map（未接选路） | 合并进 pair 状态 |
| `quota_halt.go`（direct 429） | direct 作为 egress，合并进 pair |
| `proxyPool.poolHalted`（403 全局 halt） | 改为「sub 全 quarantine ⇒ no_candidate」；保留 403 归因 |
| `node_scoring.go` v2 分数 | **保留为展示口径**；选路改用 pair 后验（见 §3） |
| T3 超时分层 / idle_read | 不动 |

## 3. 选择层：per-pair 后验 + 有界探索

### 3.1 后验

```text
pair.a, pair.b  先验 a0=8, b0=2（新 pair = 80 分）
2xx        -> a += 1, lastOK=now
429/FUL    -> b += 1, lastFail=now
403        -> 不计入 pair（订阅级），只记 sub
网络/超时  -> 不计入 pair（生命周期处理）
半衰期 7d，a/b 按 exp(-Δt/7d) 衰减
```

### 3.2 选择

```text
候选 = Admit 资格层放行的 pair
p    = (a+0.5)/(a+b+1)
n    = a+b-a0-b0
n<5  -> 冷启动优先（尝试次数最少；同一出口同时 1 个，复用 canary 门）
n>=5 -> UCB: p + 0.05*sqrt(2*ln(N)/n)
tie  -> lastOK 更近 > 近 1h 失败更少 > 轮询
sticky 会话粘住，失败才换；换出口排除同 sub、同 /24
```

### 3.3 为什么不是静态总分
- 稳态 99%+ 成功率，任何排序都差不多；静态分反而引入噪声（−2.6pp）。
- 风暴日需要「快速降权 + 快速恢复」，只有在线后验 + canary 能做到。
- oracle +84.7pp 的上界要求持续探索；UCB 的探索项是有界的，且被 breaker 的 3/60s 兜底。

### 3.4 面板口径
- 节点卡片：`quality=100*a/(a+b)`（按流量加权聚合所有 model）、`n`、`Δ7d`、`lifecycle`、`quota_state`、`last_ok_age`、`auto_eligible`。
- 展开：该节点 × 各 model 的 pair 后验与最近失败原因。
- 缺数据「未提供」，不补 0；图内不写解释句。

## 4. 可观测

### 4.1 事件（call_log 新 schema，向后兼容）
```json
{"type":"admit","model":"...","egress":"node:fp","kind":"normal|canary|global_probe","candidate_n":N}
{"type":"suppress","model":"...","reason":"no_candidate|all_inflight|sub_quarantine","would_be_egress":"...","candidate_n":N}
{"type":"outcome","model":"...","egress":"...","class":"2xx|429|403|network|timeout|5xx|cancel","ms":123}
{"type":"breaker","egress":"...","strikes":2,"until":"..."}
{"type":"lifecycle","egress":"...","from":"active","to":"dead"}
```

### 4.2 指标（/api/stats 扩展）
- `admit_total{kind,result}`、`suppress_total{reason}`、`outcome_total{class}`；
- `egress_sends_24h{egress}`（预算可视化）、`breaker_active`、`pair_unknown`；
- `success_rate_5m`、`last_any_2xx_age`。

### 4.3 面板新增
- 顶部健康条：5m 成功率、最近 2xx 时间、活跃熔断数、被隔离订阅数；
- 节点表：资格、quality、n、quota、最近失败；
- 事件流：admit/suppress/breaker/lifecycle（可过滤）。

## 5. 配置（全部集中，带默认值）

| 键 | 默认 | 说明 |
|---|---|---|
| `pair_trigger_429_60s` | 3 | 60s 内 429 次数阈值 |
| `pair_trigger_ratio` | 0.5 | ≥4 次尝试时的占比阈值 |
| `pair_ladder` | 5m/30m/2h/5h | 冷却递进 |
| `pair_reset_successes` | 2 | 清零所需 2xx 次数 |
| `pair_reset_min_gap` | 60s | 两次成功最小间隔 |
| `pair_resets_24h` | 3 | 每 pair 每日清零上限 |
| `pair_bad_24h` | 8 | 每 pair 24h 429 硬预算 |
| `pair_inflight_max` | 4 | 每出口在途上限 |
| `global_probe_interval` | 60s | 无候选时全局探测间隔 |
| `sub_quarantine` | 10m | 订阅隔离时长 |
| `lifecycle_dead_fails` | 5 | 连续连接失败判死 |
| `lifecycle_dead_ttl` | 30m | 判死冷却 |
| `ucb_c` | 0.05 | 探索系数 |
| `cold_start_n` | 5 | 冷启动样本阈值 |
| `pair_half_life` | 7d | 后验衰减 |

## 6. 数据与迁移

- 新状态文件 `node_state_v5.json`（原子写 tmp+rename；损坏 fail-open 并告警）：
  ```json
  {"version":5,
   "pairs":{"model|egress":{"a":8,"b":2,"strikes":0,"until":0,"bad24":0,"window":0,"resets24":0,"lastOK":0,"lastFail":0}},
   "egress":{"fp":{"lifecycle":"active","consecFail":0,"deadUntil":0,"lastProbe":0}},
   "subs":{"sub":{"until":0,"reason":"403"}}}
  ```
- 上限：pair 最多 50k（512 model × 212 egress 的活跃子集），空闲 30min 清理，**不淘汰 armed/blocked**。
- 迁移：旧 `NodeExhausted` 且 quota ⇒ 首次冷却 5m；保留 dead/rejected；旧 `shape_drift` 状态丢弃（本来就不该持久化）。
- `proxy_state.json` 保持 v1 不动（节点静态配置与现有 mark），避免回滚丢状态。

## 7. 上线：影子 → 强制

1. **影子模式**（`NODE_SYSTEM=v5-shadow`，默认 24h）：v5 只做决策记录，不改变实际发送；对比「v5 会选谁」与「实际选谁」，统计分歧率与预测成功率。
2. **强制**（`NODE_SYSTEM=v5`）：Admit/Observe 接管；观察 30min 指标。
3. **回滚**（`NODE_SYSTEM=legacy`）：回到 HEAD 行为（shape_drift + 旧选路），不绕过 pool403/quota。
4. 回滚判据：成功率低于基线 10pp / 可用出口 <20 / 单出口 24h 发送 >10 / 全局 503 比例 >5%。

## 8. 切片与验收

| 切片 | 内容 | 验收 |
|---|---|---|
| **N1 数据层** | pair/egress/sub 状态、meter、clock 注入、`node_state_v5.json`、迁移、影子记录 | 重启不清 blocked；旧状态加载；10 万 model 内存；fake clock 5h |
| **N2 资格层** | Admit/Observe、breaker（v4.1 规格）、lifecycle、sub quarantine、no_candidate、all_inflight | 00:20 单出口第 3 次熔断；18:03 有 2xx 的出口不被误封；N=22/K=4 无假 503；每出口 24h 429 ≤8；变异 ≥10 |
| **N3 选择层** | pair 后验、UCB、冷启动、sticky、同 sub//24 排除 | 回放：稳态不低于基线 0.5pp；风暴日 ≥+5pp；每出口 429 ≤ 基线；可用出口不降 >10% |
| **N4 可观测** | 事件、指标、面板健康条/节点表/事件流 | 面板数字与 call_log 重算一致；缺数据显示「未提供」 |
| **N5 切换** | 影子 24h → 强制 → 回滚演练 | 影子分歧报告；回滚 5 分钟内完成 |

依赖：N1 → N2 → N3；N4 可与 N2/N3 并行；N5 最后。

## 9. 测试策略

- 单元：状态机迁移（ladder、canary、budget、lifecycle、sub）。
- 并发：100 并发同一 pair 只发 1 个 canary；K=4 在途；租约泄漏。
- 回放：10-26/10-27 全量日志 + fake clock，跑 18:03、00:20、稳态三段。
- 混沌：全出口 429、单出口 167 连发、403 风暴、网络抖动、上游 5xx、客户端取消。
- 变异：每切片 ≥8 个变异必须全红。
- 影子对比：v5 决策 vs 实际决策的分歧率、预测校准（Brier）。

## 10. 风险与未决

1. **per-pair 状态量**：512×212 最坏 10 万 pair；用活跃子集 + 上限 5 万 + 保护 blocked 条目。
2. **UCB 探索烧 IP**：探索被 breaker 的 3/60s 与 24h 8 次硬预算兜底；影子期先量化。
3. **sub quarantine 误伤**：403 归因需 ≥2 个不同 pair 或同 pair 2 次；影子期观察误伤率。
4. **面板口径变化**：quality 从「加权总分」变为「后验均值」，需要标注样本量，避免用户误读。
5. **T7 研究未完成**：AUC/校准/MNAR 还没做；N3 先按本方案实现，T7 完成后校准参数。
