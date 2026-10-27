# 节点系统 v3.2 执行计划（2026-10-27）

> **状态：DRAFT v3.2 —— 已吸收 R3 + R4 全部 blocker，待 R5 复审。** R5 通过前不开工。
> 纪律：同一时刻只允许一个代理在跑（用户要求）。

## 0. 版本演进

| 轮次 | 评审者 | 结论 | 本版处理 |
|---|---|---|---|
| R1 | review-t1-design | 现实现 no-go（C1–C4）+ G1 | 采纳发送前闸门/令牌/不清 blocked |
| R2 | healthcheck-probe | C10 + 烧 IP 上界 | 采纳 per-pair 递进冷却 + 24h 预算 |
| R3 | verify-c5v2 | NO-GO，D1–D8 裁决 + 3 blocker | per-pair 默认、全局严格例外、TTL 不清 blocked、Decide 唯一裁决点 |
| R4 | impl-ip1 | NO-GO，B1–B6 | 本版：事务化 API、条件化五层、per-pair 状态机、canary 并发门、持久化/off、meter/clock |

**已更正的事实**：此前「21/29/9 个节点到期」是日志行数，distinct fp 仅 3/1/2；18:03 的 21 行只涉及 2 个节点。190 个首次 `NodeExhausted` 标记 100% 为 `+5.00h`，无数据依据。

## 1. 核心设计（R4 修订后）

### 1.1 Admission 事务 API（B1）

```go
type Admission struct { /* gate state, meter, pair state, pool 引用 */ }

func (a *Admission) Snapshot(now time.Time, model string) GateInput
func (a *Admission) Decide(in GateInput) GateDecision        // 纯函数，无副作用
func (a *Admission) Commit(d GateDecision, now time.Time)     // 锁内状态转移 + meter 写入
```

`GateInput` 必须包含：`Now, Model, CandidateN, UnblockedN, DirectAvailable, PairChosen, PairBlocked, ModelAllPairsBlocked, PairCooldownRemaining, TrialInFlight, TrialTokenValid, NextProbeIn, GlobalArmed, GlobalBlockedSet, Pool403Halted, AttemptsLast60s, Ratio429Last60s, DistinctNew429_600s, ConsecutiveNew429, LastAny2xx`。

规则：
- `Snapshot` 在**同一把锁/版本**下生成所有计数，避免 attempts=19 与 ratio=1 来自不同时刻。
- `Decide` 只读；`Commit` 在锁内做 CAS 与状态迁移；meter 的 attempts 只在「真正发送」时记，抑制不记（否则分母被自己的拒绝污染）。
- **发送前再复核 `Pool403Halted`**（保留 main 现有双重检查）。
- 时钟：所有 gate/pair/meter 函数接收 `Now`；测试用 fake clock，可跑 5h 虚拟时间。

### 1.2 五层条件化语义（B2）

| 层 | 拒绝条件（必须精确） | 状态码 | Reason |
|---|---|---|---|
| P pool403 | `Pool403Halted` | 503 + Retry-After | `pool403` |
| G global shape | §1.3 六条件全真 | 503 + Retry-After | `global_shape` |
| M model quota | **该 model 的所有 pair 都 blocked**（不是有任意一个 blocked） | 429 | `model_quota` |
| D direct quota | **仅当选路会落到 direct 时** | 503 + Retry-After | `direct_quota` |
| S selection | `CandidateN==0`（含全部 blocked） | 503 | `no_candidate` |

- 多层同时拒绝时取**最高优先级**的状态码与 Reason，`SuppressBy` 记录全部命中层（逗号分隔），禁止状态码随无关层跳变。
- 禁止回退直连：`DirectAvailable=false` 时 `S=0` 直接 503。
- 禁止透传上游 429。

### 1.3 全局 shape 条件（严格例外）

```text
GLOBAL_SHAPE =
  no_2xx_any_model(last 60s)                    // 硬否决
  AND last_any_2xx_age >= 60s
  AND distinct_new_egress_429(600s) >= 5        // 同 egress 多 model 只算 1
  AND consecutive_new_egress_429 >= 5           // 同出口重复不加不重置
  AND attempts(60s) >= 20
  AND 429_count(60s)/attempts(60s) >= 0.90
```

- 单出口 167 连发（distinct=1）永不触发全局；由 per-pair 在第 3 次时停。
- 任一真实 2xx（任意 model）⇒ 同事务清全局与对应 pair。
- 全局生效时 `blocked` 保留；TTL 只推 `nextProbeAt`；除单飞探测外零上游发送。

### 1.4 per-pair 状态机（B3）

```text
pair = (model, egressKey)
strikes: 1→5m, 2→30m, 3→2h, 4+→5h（封顶）
触发排除：同一 pair 在 10min 内第 3 次 FUL（不等全局）
canary：冷却到期后只允许 1 次真实请求；K=2 次成功且间隔≥60s 才 reset strikes；失败则 strikes+1
并发门：同一 pair 同时只允许 1 个 canary（100 并发 → 1 次上游）
每 egress 总 canary 上限：24h 内 8 次（含所有 model 叠加）
24h 预算：统计所有「接触过 FUL 或 403-confirmed」的发送；达到 8 次 ⇒ 该 egress 硬排除 24h
```

### 1.5 持久化与 off（B5）

- 新增 `gate_state.json`（**不塞进 `proxy_state.json`**，避免回滚旧二进制丢节点状态）：`{version, blocked: {pair: {until, strikes, lastHit, canaryOK}}, global: {armed, armedAt, nextProbeAt, blocked[]}, budget: {egress: {hits, windowStart}}}`。
- 原子写（tmp+rename）；损坏时 fail-open（清空并告警），不 fail-closed。
- 上限：`modelCap=512` + 空闲 30min 清理；**不得淘汰已 armed/blocked 的证据**（旧 C2 教训）。
- 重启不清 blocked（与 §4 一致）。
- `SHAPE_DRIFT_GATE=off`：走 HEAD 旧 shape 路径（per-model/3 distinct/TTL 清 seen），**不绕过 pool403 与 quota_halt**；env 非法值默认 on。
- 部署时一次性迁移：旧 `NodeExhausted` 且 `LastError` 含 quota ⇒ 按新规则重算冷却（首次 5m），保留 dead / `GateStateRejected`。

### 1.6 meter 与可测性（B6）

- `gateMeter`：60 个 1s 桶（attempts/n429）+ `last2xx`；600s distinct `map[egress]time.Time`（上限 213+1）。
- 写入点唯一：`Admission.Commit`（两条 main 循环、direct/node、canary 全走同一接口）。
- 内存测试：10 万随机 model 名，`modelCap` 生效且内存不增长；已 blocked 的 pair 不被淘汰。
- 事故仿真用 fake clock 跑 5h；用 instrumented transport 统计「每 egress 发送次数」与「direct 发送次数」，替代不可测的「命中 ≤1」字面断言。

## 2. 切片（每个切片一个可回滚提交）

### S0 基线（已完成）
- `da85f2b` T1/T2/T3 + 草案；`456256f` probation + mark 幂等；`9c3cc59` 2xx 接线 + R4 报告。

### S1a Admission API + meter + 条件化五层
- 新增 `admission.go`（Snapshot/Decide/Commit、gateMeter、fake clock 接口）。
- `main.go` 两条循环改为 `Snapshot → Decide → Commit → 发送`，保留 pool403 双重检查。
- 测试：32 行交互矩阵（R4 附录 A）、并发 100 单飞、发送前 halt 竞态、抑制不污染 ratio。
- 验收：矩阵逐行断言；变异 ≥6。

### S1b per-pair 状态机 + canary + 24h 预算
- `admission.go` + `gate_state.json` 结构（先内存，S1c 再落盘）。
- 测试：单出口 167 连发在第 3 次停；100 并发 canary 只发 1；K=2/60s reset；失败退避单调；每 egress 24h 8 次；多 model 叠加。
- 验收：变异 ≥6；5h 虚拟时间下每 egress 发送次数 ≤ 预算。

### S1c 全局闸 + off + 持久化 + 迁移
- 全局六条件、单飞令牌绑 egress、`SHAPE_DRIFT_GATE`、`gate_state.json` 原子读写、旧 NodeExhausted 迁移。
- 测试：重启不清 blocked；旧 state 加载；off 与 HEAD golden 对比；损坏文件 fail-open。
- 验收：变异 ≥6；golden 回放发送数/抑制数/状态码一致。

### S2 生命周期收尾
- 删除 `recordProbeSuccess` 对 NodeProbation 的提升；冷却 5m/30m/2h/5h + canary；补测试。

### S3 per `(model,node)` 配额状态
- 旧状态文件无字段 ⇒ `unknown`；面板只读字段缺失显示「未提供」。

### S4 打分架构（等 T7；`13-scoring-architecture.md` 仍是半成品）

## 3. 验证门禁

1. `go build` / `go vet` / gofmt；
2. `go test -skip 'TestIP4RV' -count=1 ./...`；
3. 变异全红（每片 ≥6，总计 ≥18）；
4. 事故仿真（fake clock 5h，18:03 与 00:20 两组参数）；
5. 独立评审 go/no-go（每片）；
6. 提交信息含切片号与回滚方式。

## 4. 上线与回滚

- 开关：`SHAPE_DRIFT_GATE=off|on`（默认 on）。
- 观察 30min：成功率、上游发送总量、`suppressed_by` 分布、每 egress 发送次数、`Retry-After` 命中。
- 回滚：成功率低于部署前 10pp / 可用节点 <20 / 全局 armed >15min 未退出 ⇒ 关开关或回滚该片。
- 禁止靠重启清闩。

## 5. R5 必须攻击的点

1. `Decide/Commit` 分离后，Snapshot 与发送之间的状态漂移（尤其 pool403、pair blocked 变更）是否仍可能放行一个已 blocked 的 egress；给出竞态时序。
2. 条件化 M（所有 pair blocked）与 per-pair 冷却的交互：会不会出现「pair 还有 1 个未到期但实际不可用」的假放行。
3. canary K=2/60s 与 24h 8 次预算在「配额每日重置」场景下是否会永久锁死节点；给出解锁路径。
4. `gate_state.json` 与 `proxy_state.json` 两份状态在崩溃/回滚时的不一致；给出恢复顺序。
5. off 路径与 HEAD 的 golden 对比是否真的可复现（HEAD 旧路径有 TTL 清 seen，行为差异需列全）。
6. 是否仍存在「一处放行、另一处拒绝」的层间组合；用 32 行矩阵之外的组合（含 direct、canary、trial）再穷举一轮。
