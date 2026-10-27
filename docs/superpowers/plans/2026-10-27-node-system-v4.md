# 节点系统 v4：按出口熔断 + fail-fast（去掉全局闩）

> **状态：DRAFT v4.0，待 R6 复审。** 这是对 v3.2 的**简化**，不是又一次补丁。
> 触发原因：v3.1/v3.2 连续被 R3/R4/R5 判 NO-GO，8 个 blocker 里 5 个来自「五层裁决 + 全局闩」这套机制本身。按调试纪律（3 次修复失败 ⇒ 质疑架构），先删机制，再加功能。

## 0. 为什么删掉全局闩

事故复盘的关键事实：
1. 18:03 触发闩时，同一窗口 mimo 有 164 个 2xx、18:10 还有 40 个 2xx ⇒ **上游 shape 并没有被整体拒绝**；
2. 00:20 是**单个出口** `US-191.96.254.138` 连发 167 次 429 ⇒ 这是该出口的配额问题，不是 shape；
3. 事故真正的伤害来自「闩住后仍每个请求发一次上游」+「TTL 清空 blocked 导致每 10 分钟重扫全池」。

结论：**我们从来不需要一个「全局闩」。** 需要的是「这个出口现在还能不能用」的按出口熔断，以及「没有可用出口时立刻失败、不要空转」。
全局闩的六条件、per-model 全局状态、24h 预算、五层优先级，全是为了让一个不该存在的机制自洽。

## 1. 核心模型

```text
状态（全局一份 map，单锁）：
  breaker[egressKey] = {strikes int, until time.Time, lastHit time.Time, canaryInFlight bool, canaryOK int}
  inflight[egressKey] = 在途租约数（防止并发打同一出口）
  lastAny2xx time.Time（仅用于告警，不参与裁决）

Admit(model, now) (Lease, Decision):
  1. 若 pool403Halted           -> 拒绝 503 + Retry-After（T2 语义，最高优先）
  2. 候选 = 该 model 下未 blocked 的出口（breaker.until <= now 且 inflight < 1）
  3. 若候选为空：
       - 若有出口 canary 可用（until 到期、无在途）-> 放行 1 个 canary（标记 canaryInFlight）
       - 否则 -> 拒绝 503 + Retry-After（no_candidate，禁止回退直连）
  4. 选择候选（沿用现有打分/最近成功/排除同订阅与同 /24），写入 inflight，返回 Lease{egress}

Complete(lease, outcome, now):
  inflight--
  outcome == FUL/429 -> strikes++; until = now + ladder[strikes]（5m/30m/2h/5h 封顶）; lastHit=now
  outcome == 2xx      -> canaryOK++; 若 strikes>0 且 canaryOK>=2 且两次间隔>=60s -> strikes=0, until=zero
  outcome == 其他失败 -> 不计入 breaker（交给节点健康/打分，避免把网络抖动当配额）
```

- **没有全局 armed / probe_only / 六条件**。
- 全局形态异常只作为**告警**（例如 5 分钟内 ≥10 个不同出口 strikes++ 且 `lastAny2xx` > 5min），不改变裁决。
- 出口级 `canaryInFlight` 保证同一出口同时只有 1 个探测；`inflight<1` 保证不会 100 并发打同一出口。
- 租约超时（例如 5 分钟未 `Complete`）由 sweep 释放，避免泄漏。

## 2. 不变量对照

| 不变量 | v4 如何满足 |
|---|---|
| I1 不得把流量钉在已知 429 出口 | 429 出口立即进入 `until`，`Admit` 不再选它 |
| I2 抑制必须留痕 | 每次拒绝写 `suppressed_by=no_candidate` / `pool403` + `would_be_egress` + `model` |
| I3 恢复必须有证据 | `until` 到期只给 **1 次 canary**，2 次 2xx（间隔≥60s）才清零 strikes |
| I4 mark 幂等 | 独立修复（已在 `456256f`） |
| I5 闩必须自愈 | 没有闩；breaker 到期即给 canary，失败则升档 |
| I6 halt fail-fast | 无候选直接 503，不空转 |
| I7 打分不把准入当质量 | breaker 与 scoring 完全分离，429 不进质量样本 |
| I8 未尝试 ≠ 失败 | `suppressed` 事件与真实 attempt 分开记 |

## 3. 烧 IP 上界（对照 R2/R3 的封禁阈值 10–16 次/IP/日）

- 每个出口的发送次数由 ladder 决定：`5m,30m,2h,5h` 封顶后每 5h 一次。
- 单出口 24h 最坏：1（首次）+1（5m）+1（30m）+1（2h）+约 4（5h 档）= **8 次/日**，低于阈值。
- 全池 212 出口最坏：约 1,700 次/日，但这是「上游对所有出口都拒绝」的极端场景；正常配额耗尽只会影响部分出口。
- 对比 v3.2：省掉了「TTL 清空 ⇒ 每 10 分钟全池重扫」这条 79–127 IP/小时的烧毁路径。

## 4. 切片（v4 只有 2 个代码切片）

### S1 egress breaker + Admit/Complete（新增 `breaker.go`，改 `main.go` 两条循环）
- 单锁 map；`Admit` 返回 lease；`Complete` 回写；sweep 释放超时租约。
- 与 T2 `pool403` 的优先级：pool403 在 `Admit` 最前，直接 503。
- 与 T3 首字节超时：outcome 归为「其他失败」，不计 breaker、不 mark。
- 拒绝时写结构化事件（`suppressed_by`、`would_be_egress`、`model`、`candidate_n`）。
- 开关 `EGRESS_BREAKER=off` ⇒ 完全绕过（回到 HEAD 行为，不绕过 pool403/quota_halt）。
- 测试 ≥15：ladder 边界、canary 单飞、100 并发只发 1、租约超时、2xx 清零、pool403 优先、off、10 万 model（不应有 per-model 状态）、direct 被排除、抑制事件。
- 变异 ≥8：删 inflight 门、删 canary 单飞、删 ladder 递进、429 不计 strikes、2xx 不清零、pool403 不优先、off 不生效、抑制不留痕。
- 仿真：fake clock 5h，18:03（多出口 429）与 00:20（单出口 167 连发）两组；断言单出口发送 ≤8/日、direct 0、无空转。

### S2 生命周期收尾
- 删除 `recordProbeSuccess` 对 NodeProbation 的提升；旧 `NodeExhausted` 5h 迁移为 breaker strikes；冷却 5m/30m/2h/5h。

### S3 per `(model,node)` 配额状态（可选，若 v4 的出口级熔断足够则不做）
- 仅当数据显示「出口对 A 模型 429、对 B 模型仍可用」的比例高到影响可用性时才做。

### S4 打分架构（等 T7）

## 5. 验收门禁

1. `go build` / `go vet` / gofmt；
2. `go test -skip 'TestIP4RV' -count=1 ./...`；
3. 变异全红（S1 ≥8）；
4. 仿真断言（fake clock、instrumented transport 统计每 egress 发送数）；
5. 独立评审 go/no-go；
6. 提交含切片号与回滚方式。

## 6. 上线与回滚

- 开关 `EGRESS_BREAKER=off`；观察 30min：成功率、上游发送总量、`suppressed_by` 分布、每 egress 发送次数。
- 回滚：成功率低于部署前 10pp / 可用节点 <20 / 单出口 24h 发送 >10 ⇒ 关开关或回滚。

## 7. R6 必须攻击的点

1. 去掉全局闩后，真 shape 风暴（所有出口同时 429）会不会因为「每个出口各自 canary」而烧穿？给出 24h 最坏发送数与阈值对比。
2. 出口级 breaker 会不会误伤：同一出口对 A 模型 429、对 B 模型正常时，把 B 也挡掉造成可用性下降？给出用 10-26 日志的量化（有多少比例）。
3. `inflight<1` 与现有选路/重试循环的兼容性：并发下会不会出现「有可用出口但全部 inflight 满」的假 no_candidate。
4. 租约泄漏（请求 panic/超时未 Complete）如何保证释放；与 T3 超时语义是否冲突。
5. `EGRESS_BREAKER=off` 是否真的回到 HEAD，且不绕过 pool403/quota_halt。
6. v4 是否遗漏了 v3.x 里任何**真实必要**的能力（逐条对照 8 条不变量与事故证据）。
