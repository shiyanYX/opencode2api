# 节点系统统一设计 v1（2026-10-27）

> 用户判断：打分、429 恢复、生命周期不是三件事，而是同一个节点系统的不同层。本文件是统一设计，不是补丁清单。
> 依据：2026-10-26/27 生产日志（13,113 + 数百行）、`call_log` 全量、docker 日志、当前工作区实现（未提交）。

## 0. 结论先行

1. 10-26 18:03–23:07 的 5 小时事故，**不是**「上游封了我们的 shape」这一个原因，而是四个缺陷串成的闭环：
   - 配额失败被当成节点质量失败（4xx→5h 全节点熔断）；
   - 冷却到期**无证据恢复**（固定 5h）；
   - 重复 mark 会不断后推冷却；
   - 全局 429 闩**永不解除**（窗口被同一出口重复刷新），且不分模型。
2. 这四个缺陷的共同点：**把「准入/配额」和「质量/健康」混为一谈，并且用时间代替证据**。
3. 因此设计的第一原则：**资格（能不能用）与质量（好不好用）必须分开；任何状态迁移必须有证据；任何抑制必须留痕。**

## 1. 事故证据（全部来自生产日志）

| 时间 | 事实 | 含义 |
| --- | --- | --- |
| 18:03:55 | 同一节点 `BR-45.39.2.144` 在 **1 秒内被 mark 20+ 次** `exhausted until=+5h` | mark 非幂等；每次都可能后推冷却 |
| 18:03:58 | `shape_drift_detected`（mimo） | 3 个不同出口 429 ⇒ 全局闩 |
| 18:00–23:00 | 9,000+ 条失败，`nodes` 数组长度**恒为 1**；20 点整小时 4,255 条全失败 | 换节点被完全禁用 |
| 19:00 | deepseek 109 ok/117 fail；mimo 671 ok/2,321 fail | 配额是 **per model** 的，闩却不分模型 |
| 23:03 | 21 个节点冷却到期（=18:03+5h）；00:33 再 29 个；01:00 再 9 个 | 恢复靠固定时长，不是证据 |
| 23:07 | 生产恢复（零失败） | 与 23:03 到期吻合 ⇒ 时间恢复生效 |
| 23:00–23:06 | 137 条失败**全部钉在** `DE-64.137.10.153`，每条 1 次尝试 | 到期节点未验证即接流；闩仍在 |
| 00:20:58 | 再次 `shape_drift_detected`；00:21 一分钟 136 条失败 | 闭环复现 |

**闭环**：节点 429 → 全节点 5h 熔断 → 到期无证据恢复 → 立刻再 429 → 10 分钟内 3 个不同出口 → 全局闩死 → 流量钉在单节点 → 打分只观测到该节点。

## 2. 根因分层

1. **生命周期层**：`NodeExhausted` 是「整节点 5 小时」的全局状态，但 429 是 `(model, node)` 维度的配额问题。
2. **恢复层**：`sweepExpiredLocked` 只看 `CooldownUntil`，到期直接 `available`，没有任何探测/试探。
3. **写入层**：`mark` 非幂等，重复写入刷新 `MarkedAt/CooldownUntil`，高并发下同节点被反复标记。
4. **全局闩层**：窗口用「最近一次」时间且被同一出口刷新；解除只看窗口空；不分 model；闩期间不记录被抑制的尝试。
5. **打分层**：把 429（准入）计入质量；未尝试被当成缺失但离线评估按观测样本算，产生 MNAR 偏差。

## 3. 不变量（硬规则，违反即缺陷）

- **I1 不得把流量钉在已知 429 的出口上**：只要该 `(model, egress)` 在窗口内 429 过，必须允许离开它（受全局限速，但不是禁止）。
- **I2 任何抑制必须留痕**：`suppressed_by` / `model` / `node_fp` / `would_be_egress`，禁止用 Detail 字符串代替结构化字段。
- **I3 恢复必须有证据**：时间到期最多进入 `probation`，不能直接 `available`。
- **I4 mark 幂等**：同一 `(fp, reason)` 在冷却期内重复 mark 不得延长冷却、不得重复写日志。
- **I5 全局闩必须自愈**：有 TTL、有成功清除、有半开试探；且按 `model` 分桶。
- **I6 任何 halt 必须 fail-fast**：明确错误 + 事件，不得让请求挂住。
- **I7 打分不把准入当质量**：429/FreeUsageLimitError 只影响资格，不计入质量分。
- **I8 未尝试 ≠ 失败**：打分/回放必须能区分，选择层必须记录被抑制的尝试。

## 4. 分层架构

### 4.1 生命周期（每节点）

```
disabled ──enable──> available
available ──quota(429)──> exhausted(quota, per-model)
available ──dial/5xx 连续──> dead
exhausted ──cooldown 到期──> probation        （I3）
probation ──1 次证据成功──> available（低权重）
probation ──证据 429──> exhausted（退避，指数）
probation ──其他失败──> dead
dead ──探测成功──> available
```

- `probation` 同时最多 `probationMaxConcurrent = 4` 个（全局限流，防止一次到期潮把池子同时打出去）。
- `probation` 只放行 **1 次**真实请求（或探测），结果决定去留；超时未验证则回 `exhausted`（短退避）。

### 4.2 资格（per `model × node`）

- `quota[model].state ∈ {unknown, ok, exhausted}` + `until` + `evidence`（探测/真实请求）。
- 429 只写该 model 的配额状态，**不改节点整体状态**；其他 model 仍可选中该节点。
- 到期后该 `(model,node)` 进入 `probation`，而非直接 `ok`。

### 4.3 打分（质量）

- 只在「有观测」时更新；`probation` 的尝试单独标记（`probation=true`），不进主质量样本或降权。
- 延迟权重继续保留，但依据 ICC=0.0%，延迟只用于同质量内的排序，不参与 tier。
- 输出：`quality`（0–100）、`confidence`（有效样本）、`quota_state`（per model）、`last_ok_age`、`eligible`。**不再输出「一个分数决定一切」。**

### 4.4 选择

候选构造顺序：
1. 排除 `dead` / `disabled` / `exhausted`（当前 model）；
2. 排除窗口内该 model 已 429 的出口（`shapeDriftBlockedEgress`）；
3. 排除同订阅、同 /24、同 /64（T2 已有）；
4. `probation` 节点占用全局试探额度（≤4）；
5. 排序：`recentSuccess && n≥3` → `quality` → 原序；
6. 被迫换节点时记录 `switch_reason` / `switch_candidates` / `suppressed_by`。

### 4.5 全局 429 闩（shape drift）

当前工作区实现（已具备）：
- per-model；`seen` 只记首次时间；窗口 10min；TTL 10min；任一真实 2xx 清闩；
- 半开：全局 5s 最多一次试探，退避 60→120→300s，试探目标单独上报；
- 被抑制的出口集合用于候选排除；抑制事件结构化落盘。

需补：闩期间「不 mark 节点」保持不变（T2 语义），但**必须允许试探**（I1）。

## 5. 交互矩阵

| 场景 | 生命周期 | 资格 | 打分 | 选择 | 全局闩 |
| --- | --- | --- | --- | --- | --- |
| 单节点 429 | 该 model 进 exhausted(短) | 该 model 不可用 | 不计质量 | 换节点 | 记 1 个 distinct |
| 3 个不同节点 429（10min） | 各自 exhausted | 各自不可用 | 不计质量 | 只允许半开试探 | armed，非试探请求 fail-fast |
| 真 shape 回归 | 不 mark | 不可用 | 不计质量 | 试探失败即停 | armed + 退避，等 TTL |
| 冷却到期 | exhausted→probation | probation 可用 1 次 | 试探结果单独记 | 占用试探额度 | 无关 |
| 直连 429 | 无 | 直连不可用 | 不计质量 | 走节点池 | 记 distinct（若适用） |
| pool_halt(403) | 不 mark | 不变 | 不计质量 | fail-fast | 优先于 drift |
| 首字节超时 | 不 mark | 不变 | 不计 connect 失败 | 换节点 | 无关 |

## 6. 参数与依据

| 参数 | 值 | 依据 | 调整条件 |
| --- | --- | --- | --- |
| `shapeDriftWindow` | 10min | 事故聚集窗口 | 若误报率高则放宽 |
| `shapeDriftMinimum` | 3 distinct | 事故最小复现 | 需 T7 证据复核（可能偏低） |
| `shapeDriftTTL` | 10min | 自愈上限 | — |
| 试探全局间隔 | 5s | 防惊群 | 高并发下按 QPS 调整 |
| 试探退避 | 60/120/300s | 指数 + 上限 | — |
| `NodeExhausted` 冷却 | 5h（现状） | 无证据支撑，待 T7 用「429→真实恢复」分布替换 | 必须改 |
| probation 并发 | 4 | 防到期潮 | 按池大小 |
| probation 证据次数 | 1 | 最小成本 | 视假性恢复率 |

## 7. 可观测性

事件/字段：
- `switch_reason` / `switch_candidates`（顶层字段）
- `suppressed_by` / `suppressed_candidates` / `model` / `node_fp` / `would_be_egress`
- `shape_drift`：`model / reason(confirmed|half_open_trial) / armed / distinct / last_trial / blocked[]`
- 生命周期迁移：`from / to / reason / evidence / cooldown_until / mark_count`

上线后判据：
- `suppressed_by=shape_drift` 的条数应 >0 且 `nodes` 长度不再恒为 1；
- 闩 armed 时长 p95 < 10min；
- 到期→probation 的首次尝试成功率（假性恢复率）应显著 <100%，否则冷却时长/证据策略要改；
- 同一 fp 的 mark 次数在 1 秒窗口内应 ≤1。

回滚判据：若 armed 期间成功率低于改前基线，或 probation 把可用节点压到 <20，回滚。

## 8. 落地顺序

1. **批 1（工作区已有，待验收）**：drift 闩（首次时间/TTL/per-model/半开）+ 抑制留痕 + T2 403 + T3 超时 + T1 打分纠偏。
2. **批 2**：mark 幂等/节流 + 冷却到期进 probation（`node_probation_test.go` 已是验收规格）。
3. **批 3**：per `(model,node)` 配额状态（含持久化迁移、面板字段）。
4. **批 4**：按 T7 研究替换打分架构与 tier。

## 9. 回放推演

**18:03 若按新设计**：
- 首个 429 只写 `quota[mimo][BR-45]=exhausted(短)`，节点对其他 model 仍可用；
- 重复 mark 幂等，冷却不再后推；
- 第 3 个 distinct 429 触发 armed，但半开每 5s 放行一次试探，且**永远允许离开已 429 的出口** ⇒ 不会出现「所有请求钉在单节点」；
- 任一出口 2xx 立即清闩，不必等 10 分钟。

**00:20 若按新设计**：
- 00:33 到期的 29 个节点先进入 probation（并发 ≤4），只有证据成功的才接流；
- 假性恢复节点在 probation 就被识别，不会凑齐 3 个「直接可用但立刻 429」的出口 ⇒ 闩不会再次被假性恢复触发。

## 10. 未决问题

1. `NodeExhausted` 冷却 5h 的真实依据缺失；需 T7 给出「429→真实恢复」分布。
2. per-model 配额状态的持久化格式与迁移（当前状态文件只存节点级）。
3. `shapeDriftMinimum=3` 是否偏低（高峰期正常配额耗尽也可能凑够 3 个）。
4. probation 证据用真实请求还是主动探测（成本/延迟权衡）。
5. 打分与选择的分工边界（T7 结论未回）。
