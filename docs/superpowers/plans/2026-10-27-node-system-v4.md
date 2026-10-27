# 节点系统 v4.1：按出口熔断 + fail-fast（补 R6 的 4 个局部规格）

> **状态：DRAFT v4.1，待 R7 复审。** 方向不变：**没有全局闩**；只补 R6 指出的 4 个能力缺口。
> 依据：R6 用生产日志证明「1 次 429 就熔断会误杀 17.4% 的成功」、`inflight<1` 在 N=22 时 40% 假 503、canary 在正常路径从不置位、ladder 封顶 5h 会让真风暴恢复延迟 5h。

## 0. 与 v4.0 的差异（R6 blocker）

| # | v4.0 缺陷 | v4.1 修法 |
|---|---|---|
| B1 | 1 次 429 即熔断 | **60s 内 ≥3 次 429，或 ≥4 次尝试且 429 占比 ≥50%** 才熔断；单次 429 只记 outcome |
| B2 | `inflight<1` 把在途满当 no_candidate | `inflight<K`（**K=4**）；区分 `all_inflight`（503 + Retry-After 1s）与 `no_candidate` |
| B3 | canary 只在候选为空时放行 | **`strikes>0` 的出口强制单飞 canary**，不进入正常候选 |
| B4 | ladder 封顶 5h ⇒ 真风暴恢复 ≤5h | **无候选时每 60s 一次全局探测**（单定时器 + 单令牌，不是全局闩） |
| Q1 | `canaryOK>=2` 清零让烧 IP 上界失效 | 清零需 **2 次 2xx 且间隔 ≥60s**，且每出口 **24h 最多 3 次清零**；超过则 strikes 每日衰减 1 级 |
| Q4 | 租约泄漏 | 租约 TTL 10min + defer 释放 + 跨出口重试先释放旧租约 |
| Q5 | off 顺序 | `EGRESS_BREAKER=off` 插在 `quotaHaltGuard.gate` **之后**，不绕过 pool403/quota_halt |
| Q6 | pool403 与「全局闩」混称 | 明确：403 是订阅级信号，T2 halt 保留；本方案删的是 **429 全局闩**，不是 403 |

## 1. 核心状态机

```text
breaker[egressKey] = {
  ring [8]{ts, is429}        // 最近 8 次结果，60s 窗口
  strikes int                // 0..4
  until time.Time
  lastHit time.Time
  canaryInFlight bool
  canaryOK int
  resets24h int
  badSends24h int            // 只统计 429/FUL 与全局探测失败
  windowStart time.Time
}
inflight[egressKey] int      // 当前在途租约数，上限 K=4
```

### 1.1 触发（B1）

```text
on outcome 429/FUL(egress, now):
  ring.push({now, true})
  badSends24h++（窗口 24h 滚动）
  if badSends24h >= 8: until = now+24h; strikes=4; return   // 硬预算
  n429 = count(ring, is429, ts >= now-60s)
  nAtt = count(ring, ts >= now-60s)
  if n429 >= 3 || (nAtt >= 4 && n429*2 >= nAtt):
      strikes = min(strikes+1, 4)
      until = now + ladder[strikes]      // 5m/30m/2h/5h
```

单次 429 不改变 `until`；`4ad1982e`（6 次 429 vs 219 成功）不会被 5m 封死，除非 60s 内确实 ≥3 次。

### 1.2 选择（B2/B3）

```text
Admit(model, now):
  if pool403Halted: return 503 pool403
  normal = {e | strikes==0 && until.IsZero() && inflight[e] < K}
  canary = {e | strikes>0 && until<=now && !canaryInFlight && inflight[e] < K}
  if normal 非空: 选一个（现有打分/最近成功/排除同订阅与 /24），inflight++，lease{kind:normal}
  if canary 非空: 选一个，inflight++，canaryInFlight=true，lease{kind:canary}
  if candidate_n==0（全部 blocked 或全部在途）:
      if 全部 blocked 且 距上次全局探测 >=60s: 选 until 最早者，kind=global_probe，inflight++
      else: 503（reason=all_inflight 或 no_candidate，Retry-After 1/30）
```

- `all_inflight` 与 `no_candidate` 分开记事件。
- K=4：N=22 时容量 88 > 峰值并发 37，B2 的 40% 假 503 消失。

### 1.3 完成（B3/Q1/Q4）

```text
Complete(lease, outcome, now):
  inflight[lease.egress]--
  if lease.kind==canary: canaryInFlight=false
  switch outcome:
    2xx:
      if lease.kind==canary || lease.kind==global_probe:
         canaryOK++
         if canaryOK>=2 && now-lastCanaryOK>=60s:
            if resets24h < 3: strikes=0; until=zero; resets24h++
            else: strikes=max(strikes-1,0); until=now+ladder[strikes]
      lastAny2xx = now（仅告警用）
    429/FUL: 见 1.1（canary 失败 = strikes+1，退避单调）
    其他失败: 不计 breaker（交给节点健康/打分）
```

### 1.4 全局探测（B4）

- 仅在 `candidate_n==0`（全部出口 blocked）时启用；全局单定时器 + 单令牌，**每 60s 最多 1 次**。
- 选 `until` 最早的出口；失败按 1.1 升档，并计入 `badSends24h`。
- 真风暴下恢复时间从 ≤5h 降到 **≤2 分钟**（上游一恢复，下一次探测即 2xx）。
- 烧 IP 上界：每出口 24h `badSends24h<=8` 硬顶；全局探测 60s 一次但受每出口预算约束 ⇒ 全池最坏 1,696 次/日（8×212），单 IP ≤8，低于阈值 10–16。

## 2. 不变量对照（含 R6 的 I3/I7 核实）

| 不变量 | v4.1 |
|---|---|
| I1 不钉在已知 429 出口 | 熔断后仅 canary/全局探测可碰，正常流量绕开 |
| I2 抑制留痕 | `suppressed_by` + `would_be_egress` + `model` + `candidate_n` |
| I3 恢复必须有证据 | strikes>0 ⇒ 强制单飞 canary；2 次 2xx（≥60s）才清零，且 24h 最多 3 次 |
| I4 mark 幂等 | 已修（`456256f`） |
| I5 无全局闩 | 删除；只有 60s 全局探测 |
| I6 fail-fast | no_candidate/all_inflight 直接 503 |
| I7 429 不进质量 | 已核实 `node_scoring.go:947` |
| I8 未尝试 ≠ 失败 | `suppressed` 与 attempt 分开 |

## 3. 切片

### S1a breaker 核心（`breaker.go` + `main.go` 两条循环）
- 状态机、B1 触发、K=4、强制 canary、lease/defer/TTL、事件。
- 测试 ≥18：B1 三档边界（1/2/3 次 429、4 次中 2 次）、`4ad1982e` 型节点不被封、K=4 在 N=22/并发 37 时 0 假 503、canary 单飞、2xx 清零与 24h 3 次上限、租约泄漏、跨出口重试释放、pool403 优先、off、抑制事件。
- 变异 ≥8。

### S1b 全局探测 + 预算 + off
- 60s 全局探测、`badSends24h` 硬预算、`EGRESS_BREAKER=off`（在 quotaHalt 之后）、旧 5h 迁移。
- 测试：无候选时 60s 内恢复、预算封顶、off 行为、旧状态迁移。
- 变异 ≥6。

### S2 生命周期收尾
- 删除 `recordProbeSuccess` 对 NodeProbation 的提升；旧 NodeExhausted 迁移为 breaker strikes。

### S3 打分（等 T7）

## 4. 验收

1. `go build` / `go vet` / gofmt；
2. `go test -skip 'TestIP4RV' -count=1 ./...`；
3. 变异全红（S1a ≥8，S1b ≥6）；
4. 回放断言（用 10-26/10-27 日志 + fake clock）：
   - 00:20 单出口 167 连发：该出口在第 3 次 429 时熔断，后续请求走其他出口；
   - 18:03 多出口：有 2xx 的出口不被误封；
   - 全出口 blocked：≤60s 内出现一次全局探测，上游恢复后 ≤2 分钟回到 2xx；
   - 每出口 24h 429 次数 ≤8。
5. 独立评审 go/no-go。

## 5. R7 必须攻击的点

1. B1 的 60s 窗口实现：ring 只有 8 条，低流量下 3 次 429 可能跨 60s，是否会误封/漏封？
2. K=4 是否足够覆盖峰值并发 37 / N=22；`all_inflight` 的 Retry-After 1s 会不会导致客户端重试风暴？
3. 强制 canary 后，`strikes>0` 的出口在 canary 成功前完全不服务，会不会让可用池骤降（事故后 190 个 5h 冷却的场景）？
4. 全局探测 60s 一次 + 每出口 8 次预算，在最坏风暴下的实际发送曲线；是否仍可能烧穿？
5. `resets24h<3` 与每日配额重置的关系：正常节点每天 3 次清零够不够？会不会出现「健康节点被永久降级」？
6. off/迁移/pool403 顺序是否仍有「一处放行、另一处拒绝」。
