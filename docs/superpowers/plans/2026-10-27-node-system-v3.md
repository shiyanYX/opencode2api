# 节点系统 v3.1 执行计划（2026-10-27）

> **状态：DRAFT v3.1 —— 已过 R3 校对，待 R4 复审。** R3 结论 NO-GO（3 条 blocker）已全部写入本版。
> 纪律：同一时刻只允许一个代理在跑；R4 通过前不开工。

## 0. 决策记录（R3 裁决后）

**默认路径：per-`(model, egress)` 排除。全局闸降为严格例外。**

| 分歧 | v3.0 | v3.1（R3 裁决） |
|---|---|---|
| D1 状态粒度 | 全局单闸 × per-model 二选一 | **默认 per-`(model,egress)`**；全局仅在严格布尔条件下启用 |
| D2 触发条件 | 3 distinct/10min 或 K=5 | **K=5 + distinct_new_egress≥5 + 60s 无任何 2xx + attempts≥20 + 429 比例≥0.90** |
| D3 闩退出 | TTL 10min 退出 | **TTL 只推 `nextProbeAt`，绝不清 `blocked`**；退出唯一条件是任一真实 2xx |
| D4 每 IP 预算 | 24h 8 次 | 接受，但**只统计 shape-suspect**，不统计普通配额 429 |
| D5 冷却 5h | 固定 5h | **5m→30m→2h→5h 递进 + 真实请求 canary**；190 个首次标记 100% 为 +5.00h，无依据 |
| D6 probation 证据 | 真实请求 vs 探测未定 | 配额 probation **只用真实请求**；主动探测只用于 dead/transport；**删除 `recordProbeSuccess` 对 NodeProbation 的提升** |
| D7 对外状态码 | 混用 | 全局/pool403/direct → **503 + Retry-After**；仅 per-model 配额 → 429；**禁止透传上游 429** |
| D8 三套 halt | 并存 | **唯一 `Decide(GateInput) GateDecision`**，优先级 `pool403 > global shape > model quota > direct quota > selection` |

**更正一处先前错误**：我此前说「23:03/00:33/01:00 有 21/29/9 个节点到期」是**日志行数**，distinct fp 只有 **3/1/2**；18:03 的 21 行只涉及 2 个节点。涉及这些数字的论证已按 R3 复算修正。

## 1. 唯一裁决点

```go
type GateInput struct {
    Now               time.Time
    Model             string
    EgressKey         string
    AttemptsLast60s   int
    Ratio429Last60s   float64
    DistinctNew429    int   // 600s，去重 egress
    ConsecutiveNew429 int
    LastAny2xx        time.Time
    Pool403Halted     bool
    PairBlocked       bool
    ModelQuotaBlocked bool
    DirectQuotaBlocked bool
    GlobalArmed       bool
}
type GateDecision struct {
    Allow      bool
    Reason     string // pool403 | global_shape | model_quota | direct_quota | ok
    StatusCode int    // 503 或 429
    RetryAfter int
    TrialToken string // 单飞探测令牌（绑 egress）
    SuppressBy string
}
func Decide(in GateInput) GateDecision
```

优先级（高→低）：`pool403 > global_shape > model_quota > direct_quota > selection`。
- 同一事务内更新全局闸与 per-pair 状态，禁止只更新一边。
- 禁止在 gate 内调用 `hasEligibleNode()` 之类有副作用的函数。

## 2. 全局 shape 的可判定条件（R3 §2.3）

```text
GLOBAL_SHAPE =
  no_2xx_any_model(last 60s)                      // 硬否决：任一 2xx 立即 false
  AND last_any_2xx_age >= 60s
  AND distinct_new_egress_429(last 600s) >= 5     // 同一 egress 被不同 model 429 只算 1 个
  AND consecutive_new_egress_429 >= 5              // 同出口重复 429 不递增
  AND attempts(last 60s) >= 20
  AND 429_count(last 60s) / attempts(last 60s) >= 0.90
```

- 只有 1 个出口反复 429（如 00:20 的 `US-191.96.254.138`，167 次）**永远不触发全局**，由 per-pair 处理。
- 低流量（attempts<20）不启用全局，靠 per-pair + fail-fast。
- 全局闸生效时：`blocked` 保留；TTL 只推 `nextProbeAt`；除单飞探测外零上游发送。
- 任一真实 2xx（任意 model）⇒ 同事务清除全局闸与对应 `(model,egress)`。

## 3. 切片（每个切片 = 一个可独立回滚的提交）

### S0 拆分提交（已完成基础）
- `da85f2b`（T1/T2/T3 + 草案）、`456256f`（probation + mark 幂等）。测试文件不入库。

### S1 闸门引擎 + 唯一裁决点
- 文件：`shape_drift.go`（重写为引擎）、`gate.go`（新增 `Decide`）、`main.go`（两条循环改为先 `Decide` 再发）、`calllog.go`（结构化抑制字段）。
- 必须：per-pair 递进冷却 5m/30m/2h/5h；24h shape-suspect 预算 8；单飞令牌绑 egress；全局条件如上；禁止回退直连；`SHAPE_DRIFT_GATE=off` 时行为回到 HEAD。
- 测试 ≥20（含 D1 条件逐条布尔、单出口 167 连发不升级、同窗口 40 ok 禁止全局、TTL 不清 blocked、令牌错配、24h 预算、503 header、off 开关、并发 100 单飞）。
- 变异 ≥8 全红。
- 事故仿真：18:03 与 00:20 两段参数各跑 5h 虚拟时间；断言单出口命中 ≤1、主 IP 命中 0、无 5,520 次发送、全局 armed 连续 <10min。
- 独立评审 PASS 后才进 S2。

### S2 生命周期收尾（probation 已在 `456256f`）
- 删除 `recordProbeSuccess` 对 NodeProbation 的提升；冷却改 5m/30m/2h/5h + canary；补测试与变异。

### S3 per `(model,node)` 配额状态 + 持久化迁移
- 旧状态文件无字段 ⇒ `unknown`；面板只读字段缺失显示「未提供」。

### S4 打分架构（等 T7 完成；`13-scoring-architecture.md` 目前仍是半成品）

## 4. 验证门禁

1. `go build` / `go vet` / gofmt；
2. `go test -skip 'TestIP4RV' -count=1 ./...`；
3. 变异全红；
4. 事故仿真通过；
5. **独立评审**（非实现者）go/no-go；
6. 提交信息含切片号与回滚方式。

## 5. 上线与回滚

- 开关：`SHAPE_DRIFT_GATE=off|on`（默认 on）、`SHAPE_DRIFT_K`、`SHAPE_DRIFT_PROBE_TTL`。
- 观察 30min：成功率、上游发送总量、`suppressed_by` 分布、单出口命中分布、`Retry-After` 命中率。
- 回滚：成功率低于部署前 10pp / 可用节点 <20 / 全局 armed >15min 未退出 ⇒ 关开关或回滚 S1。
- 禁止靠重启清闩（会清掉 blocked 证据）。

## 6. 校对轮次

| 轮次 | 评审者 | 结论 | 本版处理 |
|---|---|---|---|
| R1 | review-t1-design | 现实现 no-go（C1–C4）+ G1 | 采纳发送前闸门/令牌/不清 blocked |
| R2 | healthcheck-probe | 提出 C10 + 烧 IP 上界 | 采纳 per-pair 递进冷却 + 24h 预算 |
| R3 | verify-c5v2 | DRAFT NO-GO，8 条裁决 + 3 个 blocker | 全部写入 v3.1（本文件） |
| R4（待跑） | 待定（非 R1–R3） | — | — |

## 7. R4 必须攻击的点

1. `Decide` 的优先级是否真的能消除「一处放行、另一处闩死」；用交互矩阵穷举。
2. 全局条件里 `attempts≥20` 与 `ratio≥0.9` 在低流量/突发流量下的边界；会不会因为流量小永远不触发、或因为 1 个 2xx 永远不触发。
3. per-pair 冷却到期 + canary 是否会重新引入「假性恢复」；canary 失败后的退避是否单调。
4. 24h 预算只统计 shape-suspect 的判定规则是否可被伪造（客户端/上游行为）。
5. 切换 503 后客户端重试行为（惊群）与 `Retry-After` 的实际效果。
6. 迁移：旧持久化状态、`SHAPE_DRIFT_GATE=off` 的回退语义。
