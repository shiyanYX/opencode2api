# N4 独立验收（31）

- 验收对象：`5e64827` + `c9bcee3` + 修复 `3626bc1`（当前 HEAD `3626bc1`）
- 计划：`docs/superpowers/plans/2026-10-27-node-system-v5.md`（磁盘版本 v5.3，358 行；任务消息写作 v5.1，存在版本漂移，本报告按磁盘当前 §13/§14 验收）
- 前置评审：`docs/research/scoring2/29-n4-lead-review.md`
- 结论：**原验收 FAIL；F1–F3 已修复并复验（见 §12）**
- 我没有修改生产代码，未 commit；临时审计测试文件在验收结束后删除。

## 0. 结论摘要

| 验收项 | 结果 | 证据 |
| --- | --- | --- |
| 1. 真实 429 回放：pair 10 分钟不重发 / 14:30 只触发一次 / 18:00、19:55 不触发 | **PASS** | 见 §1 |
| 2. 边界：10/5 触发、9 不触发、3 distinct/49% 不触发、空 Egress 不计数 | **FAIL** | 10/5 的第 10 个是 2xx 时不触发；空 Egress 使窗口计数变成 10/10 |
| 3. 卡死对抗：canary 429、probe 403/timeout、probe drop、持久化 Active 重启 | **PASS** | 见 §3 |
| 4. 持久化：保存/重载、截断/版本/只读降级 | **PASS** | 见 §4 |
| 5. force NoCandidate 两循环均 429+Retry-After，不使用 direct client | **PASS** | 见 §5 |
| 6. force 下旧状态写入完全旁路，v5 决策不依赖旧状态 | **FAIL（部分）** | 见 §6 |
| 7. ≥5 个独立变异全红 | **PASS** | 见 §7 |
| 8. `go vet` / 定向 race / 全量测试 | vet PASS；定向 PASS；全量 FAIL（2 个已知 IP4RV + 1 个 TempDir 清理 flake） | 见 §8 |

**不可放行的直接原因：**

1. `TestAuditN4Boundaries` 的最后一个用例失败：**10 attempts / 5 failures，若第 10 个 attempt 是 2xx，gate 不触发**。计划要求“恰好 10 attempts / 5 failures 触发”，当前实现只在 429 分支评估比例，结果依赖事件顺序。
2. 空 Egress 仍被计入 `Gate.Attempts/Failures`（5 个真实 429 + 5 个空 Egress 后窗口为 `10/10`，期望 `5/5`）。这违反“空 Egress 不计数”，并会污染后续窗口比例。
3. force 的旧状态旁路不完整：`noteAttemptTransportError`（`main.go:2869`）仍调用 `pool.mark`，`proxy_node.go:1043` 会 `delete(p.clients, fp)`；`v5Candidates` 在 `node_system_v5.go:207` 又以 `clients[fp] == nil` 过滤候选。旧状态因此能移除 v5 的候选，force 决策并非完全独立。

## 1. 真实 429 回放复算

### 1.1 数据与口径

- `/tmp/c2026-10-24.jsonl`：2,478 条，`fail=2,392`，`ok=86`。
- `/tmp/c2026-10-26.jsonl`：13,724 条，`fail=11,421`，`ok=2,303`。
- `/tmp/c27.jsonl`：18,284 条，`fail=15,784`，`ok=2,500`（10-27 00:00–15:08）。
- 429 attempt：`events[].type == "upstream_error"` 且 `detail` 含 `http 429 quota_signal`；每条事件视为一次 attempt；成功 attempt 可能没有 `connect_ok` 事件，因此不能只用事件数推导全部 attempt。
- 我同时算了两种口径：
  - **event-time**：按 attempt 事件时间驱动 `Admission`；
  - **request-start**：按请求 `ts` 归属窗口，统计最终请求失败、attempt 和 429。
  两者边界不同（请求跨窗口、隐式成功），但结论一致：18:00、19:55 都远低于 50% attempt 失败率。

### 1.2 复算数字

| 窗口 | 请求数 | 最终失败 | attempt | 429 | distinct 429 egress | gate |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 10-26 18:00–18:05（event-time） | — | — | 94 | 22 | 3 | 0 次 |
| 10-26 19:55–20:00（event-time） | — | — | 118 | 0 | 0 | 0 次 |
| 10-27 14:30–15:07（event-time） | — | — | 5,110 | 5,108 | 33 | **1 次** |
| 10-26 18:00–18:05（request-start） | 94 | 2（2.1%） | 116 | 22（19.0%） | 3 | 0 次 |
| 10-26 19:55–20:00（request-start） | 150 | 3（2.0%） | 162 | 12（7.4%） | 4 | 0 次 |
| 10-27 14:30–15:08（request-start） | 5,305 | 5,304 | 10,642 | 5,336 | 33 | 1 次 |
| 10-27 14:30–15:07（request-start） | 5,077 | 5,076 | 10,186 | 5,108 | 33 | 1 次 |

- 18:00 窗口的“2%”是**最终请求失败率**（2/94），不是 attempt 失败率（22/116 ≈ 19%）；两者都低于 50%，不应触发。
- 19:55 窗口同样：请求失败 3/150 = 2.0%，attempt 429 12/162 ≈ 7.4%，不应触发。
- 14:30–15:07 是单出口连续 429 风暴，event-time 口径 5,108/5,110 = 99.96% 429，distinct=33；gate 只触发一次，满足“风暴只触发一次”。

复现命令（临时审计测试，已删除）：

```bash
go test -count=1 -run 'TestAuditN4ReplayGate' -v .
```

输出：

```text
window 18:00:00..18:05:00 attempts=94 429=22 distinct429=3 trips=0 paused=false
window 19:55:00..20:00:00 attempts=118 429=0 distinct429=0 trips=0 paused=false
window 14:30:00..15:07:00 attempts=5110 429=5108 distinct429=33 trips=1 paused=true
PASS
```

### 1.3 pair 10 分钟不重发

把每个 `(model, egress)` 的全部 429 事件按时间回放，每次 `Admit` 得到真实租约后才 `Observe(lease, Outcome429)`；比较**相邻两次成功 admission**，不是相邻原始 429 事件：

```text
pairs=290 repeated429Within10m=27134 admissionsViolating10m=0
PASS
```

结论：在首个 429 冷却 30m 的规则下，真实数据中没有同一 pair 在 10 分钟内被第二次放行。该测试最初用“相邻原始事件”比较，且裸 release 租约，属于测试错误；修正后通过。

## 2. 边界验收

临时测试 `TestAuditN4Boundaries` 覆盖计划 §14 的边界：

| 用例 | 期望 | 实际 | 结果 |
| --- | --- | --- | --- |
| 10 attempts / 5 failures，3 distinct，最后一个是 429 | 触发 | 触发 | PASS |
| 9 attempts / 5 failures | 不触发 | 不触发 | PASS |
| 3 distinct，恰好 49% failures | 不触发 | 不触发 | PASS |
| 5 个真实 429 + 5 个空 Egress | 空 Egress 不计数，不触发 | 不触发，但窗口 `Attempts=10, Failures=10` | **FAIL** |
| 10 attempts / 5 failures，第 10 个是 2xx | 触发（该瞬间已达 10/5） | **不触发** | **FAIL** |

失败原文：

```text
--- FAIL: TestAuditN4Boundaries
    zz_n4_audit_test.go:237: 10 attempts / 5 failures did not trip when the 10th was 2xx
```

最小复现（纯状态机）：

```go
a, _, _ := newAdmissionTest(t, 8)
for _, eg := range []string{"node:e1", "node:e2", "node:e3", "node:e1", "node:e2"} {
    a.Observe(&Attempt{Model: "m", Egress: eg}, Outcome429)
}
n4observe(a, "node:e1", Outcome2xx, 5)
if !a.GatePaused() {
    // 当前代码到这里为 false
}
```

根因：`admission.go:798-817` 先累加 `Attempts/Failures`，但触发判断位于 `if class != Outcome429 || at.Egress == "" { return }` 之后；2xx 永远不进入 10/5 判断。结果依赖“第 10 个事件是不是 429”。

空 Egress 的内部污染：

```text
empty egress polluted gate window: attempts=10 failures=10, want 5/5
```

对应 `admission.go:798-804`：`Attempts++` 和 `Failures++` 发生在空 Egress 提前返回之前。虽然空 Egress 不会直接触发 gate（因为不会走到 distinct 判定），但它改变了窗口比例，后续真实 429 会以被污染的 `Attempts/Failures` 判断。

## 3. 卡死对抗

临时测试 `TestAuditN4ProbeFreeze`、`TestAuditN4DropProbeFreeze`、`TestAuditN4PersistedGateRecovers`：

| 场景 | 结果 | 证据 |
| --- | --- | --- |
| direct canary 永远 429 | PASS | 60s 后 canary 重开；10 分钟时间逃逸后 node probe 可进入；不会永久 Active |
| node probe 403 | PASS | 失败 probe 重置 `CanaryOK/NodeProbeUsed`，下一轮 canary 可再开 |
| node probe 超时/network/other | PASS | 非 2xx 走同一失败恢复分支 |
| probe 被 `v5DropAttempt` 丢弃 | PASS | 丢弃后 canary 仍能重开并最终 probe 2xx 清 gate |
| 持久化 `Active=true` 后重载 | PASS | 重载后 61s 可发 canary；canary 2xx 后 node probe；probe 2xx 清 gate |

关键输出：

```text
TestAuditN4ProbeFreeze PASS
TestAuditN4DropProbeFreeze PASS
TestAuditN4PersistedGateRecovers PASS
```

未发现“永久卡住”的运行时状态；但 gate 恢复路径的区域过滤问题见 §6.2，属于选路正确性风险，不是卡死。

## 4. 持久化

`TestAuditN4Persistence`：`SaveNodeStateV5` → `loadNodeStateV5`，逐字段比较 `Active/PausedUntil/Backoff/LastTrip/NextCanary/NextNodeProbe/Attempts/Failures`，PASS。

`TestAuditN4PersistenceDegrades`：

- 截断 JSON：返回空状态，不 panic；
- 垃圾内容：返回空状态，不 panic；
- `version=999`：返回空状态并返回版本错误；
- 只读目录：`SaveNodeStateV5` 返回错误，不 panic。

全部 PASS。加载失败不会阻断请求；`v5GetRuntime` 仍能建立空状态。

一个非阻塞观察：`gateLocked` 对旧 `Active=true` 且 `NextNodeProbe==0` 的 gate 会补 `NextNodeProbe=LastTrip+10m`，但没有 `markDirty`；重启后这个补值只存在内存，直到下一次 `markDirty` 才写回。它不影响本次验收的恢复路径（canary 仍可用），但会造成“刚重启后的面板/持久化字段与内存短暂不一致”。

## 5. 无直连回退

`egress.Get` 只有两个生产调用点：

```text
main.go:2974  非流式
main.go:3526  流式
```

两处都在使用 `egResult.Client` 之前检查：

```go
if egResult.NoCandidate {
    callLogSuppressedAttempt(ctx, modelID, "", v5NoCandidateEventName(), EgressKeyDirect, nil)
    return nil, http.StatusTooManyRequests, http.Header{"Retry-After": {"60"}}, fmt.Errorf("v5 gate: no candidate")
}
```

`egress.go:167-168` 的 force 无租约分支返回 `NoCandidate: true`，main 不会发送；`TestAuditN4NoCandidateMain` 在普通和流式两条循环都断言：

- status = 429；
- `Retry-After: 60`；
- 没有使用 `egResult.Client`。

PASS。

需要单独记录：`egress.go:135-140` 的 `TierPaid && paidDirect` 快速路径在 force 分支之前直接返回 direct，不经过 `Admit`。这不在“NoCandidate”断言内，但它是 force 之外的付费直连旁路，若计划要求所有流量统一进 `Admit`，应另开任务处理；本报告不把它算作 N4 的 NoCandidate 失败。

## 6. 双状态机与旧状态

### 6.1 已确认旁路的部分

以下旧闸门/写入在 force 下由 `v5LegacyGatesBypassed()` 包裹，未改变 v5 路由：

- `quotaHaltGuard.gate`
- `proxyPool.poolHalted` 的早期拒绝
- `shapeDriftBlockedEgress`
- `allSubsQuarantined`
- `recordNodeSuccess`
- `quotaHaltGuard.observeSuccess/observeFailure`
- `shapeDriftObserveSuccess`

普通和流式循环的早期 gate 调用点均检查了 bypass；`TestAuditN4NoCandidateMain` 也覆盖了 force 下不会被旧 `poolHalted` 提前拒绝。

### 6.2 未完全旁路：FAIL

`noteAttemptTransportError`（`main.go:2857-2871`）没有 `v5LegacyGatesBypassed()` 判断：

```go
if nodeFp != "" && !pool.poolHalted(time.Now()) {
    pool.mark(nodeFp, NodeDead, "connect_error:"+err.Error())
}
```

`pool.mark` → `markLocked` 在 `proxy_node.go:1041-1044`：

```go
if c := p.clients[fp]; c != nil {
    c.CloseIdleConnections()
    delete(p.clients, fp)
}
```

`v5Candidates`（`node_system_v5.go:195-211`）又按旧池的 `clients` 过滤：

```go
if clients == nil || clients[n.Fingerprint] == nil {
    continue
}
```

因此 force 下一次传输错误会写旧池状态并删除 client，v5 下一轮候选集立即少一个出口；**旧状态写入没有被完全旁路，v5 候选也不是独立于旧状态**。这违反 N4 第 6 项“确认 force 下旧写入旁路后，v5 决策不读旧状态”。

另外两处非决策但应记录：

- `main.go:3009/3565` 附近的 `proxyPool.noteAttempt` 在 force 下仍更新旧池的尝试/活跃信息（不直接进入 v5 Eligibility，但会污染回滚后的旧状态）。
- `main.go:2998` 的 `node_cooldown` 日志在 force 下仍会为 direct 回退打印旧语义，属于可观测性误导。

### 6.3 gate 恢复路径未传 region

`v5AdmitContext` 在 gate active 分支（`node_system_v5.go:441-458`）直接调用 `AdmitCanary(model)` / `AdmitGateProbe(model)`，没有把 `region` 传进去；`AdmitGateProbe`（`admission.go:878-926`）只按 `Candidate.Direct/Egress` 过滤，没有 region 过滤。结果是：当 gate 需要 node probe 时，可能选到不符合当前 `requiredRegion` 的节点。

这不是“卡死”，但是真实的区域越界风险；应作为 High/Medium 残余风险修复（至少在 `AdmitGateProbe` 增加 region 参数，或在 `v5AdmitContext` 的 gate 分支先构造 region 过滤后的候选）。

## 7. 变异测试

在独立 worktree `/tmp/n4mut`（`3626bc1`）上，用本报告的临时审计测试做变异；基线定向测试 PASS。

| 变异 | 结果 | 证据 |
| --- | --- | --- |
| M1 去掉空 Egress 保护 `|| at.Egress == ""` | **RED** | `TestAuditN4EmptyEgress`：`empty egress counted...` |
| M2 `pairCooldownLadder[0]` 30m → 0 | **RED** | 修正后的 pair 测试：`admissionsViolating10m=53` |
| M3 `v5DropAttempt` 改成裸 release | **RED** | `TestAuditN4DropProbeFreeze`：`dropped probe froze canary` |
| M4 force attempt limit 2 → 3 | **RED** | `TestAuditN4AttemptLimit`：`force attempt limit=3` |
| M5 删除流式 `NoCandidate` 分支 | **RED** | `TestAuditN4NoCandidateMain`：stream status=400，实际访问了上游 |

说明：M2 最初把 30m 改成 1s 没有变红，是因为旧 pair 测试只比较“相邻原始 429”且裸 release 租约；修正为比较相邻 admission、并用真实 `Observe` 后，30m→0 的变异变红（53 次 10 分钟内重放行）。这不是产品通过，而是原测试无效。

## 8. 命令与耗时

环境：`HEAD=3626bc1`；Go 测试使用 `GOPROXY=off`。

```bash
go vet ./...
# exit 0

go test -race -count=1 -run 'TestV5|TestGate|TestPair|TestNoReburn|TestMaxTwo|TestWinter' .
# exit 0
# ok ... 1.125s (test) / 105.61s (elapsed)

go test -count=1 ./...
# exit 1, 300.772s (删除临时审计测试后)
# FAIL TestProbationWithoutEvidenceExpiresBackToCooldown
# FAIL TestIP4RVLiveServedDrift
# FAIL TestIP4RVDocsDrift
```

- 两个 IP4RV 失败来自未跟踪的 `zz_ip4agg_reverify_test.go`，不是 N4 改动；但在本环境里全量命令确实非绿。
- `TestProbationWithoutEvidenceExpiresBackToCooldown` 的失败原文是 `TempDir RemoveAll cleanup: ... directory not empty`；单独重跑该测试 PASS，属于测试清理与异步保存竞争造成的 flake，不是 N4 断言失败。
- 全量中的 N4 边界失败来自临时 `TestAuditN4Boundaries`；该文件已删除，因此删除后的全量不再包含它，但本报告 §2 的失败证据仍成立。
- 临时审计测试文件 `zz_n4_audit_test.go` 已在验收结束后删除；没有生产代码改动。

## 9. 缺陷清单（按严重度）

### Blocker 1：gate 10/5 触发依赖事件顺序

- 位置：`admission.go:798-817`
- 复现：3 个不同 egress 的 5 个 429 + 5 个 2xx；第 10 个是 2xx。
- 期望：`GatePaused()==true`。
- 实际：`false`；`TestAuditN4Boundaries` 原文 `10 attempts / 5 failures did not trip when the 10th was 2xx`。
- 影响：失败率恰好 50% 且窗口末尾是成功时，gate 不进入冬季，风暴可能继续；边界行为不可预测。
- 最小修复方向：把比例/样本判定从“429 分支”移到一个统一的 `maybeTripGateLocked(now)`，在每次 attempt 计数后调用，而不是只在非空 429 后调用。

### Blocker 2：空 Egress 污染窗口

- 位置：`admission.go:798-804`
- 复现：5 个真实 429（3 distinct）+ 5 个空 Egress 429；窗口 `Attempts=10, Failures=10`。
- 期望：空 Egress 完全不计数，窗口 `5/5`。
- 实际：`10/10`；`TestAuditN4EmptyEgressDoesNotPolluteWindow` 失败。
- 影响：空 Egress 会改变后续失败率，可能提前触发或改变 gate 的 10 次样本边界。
- 最小修复方向：在 `g.Attempts++` 之前对 `at.Egress == ""` 直接返回，或把空 Egress 过滤放在 `Observe` 入口。

### High 1：force 旧状态旁路不完整

- 位置：`main.go:2869`；`proxy_node.go:1043`；`node_system_v5.go:207`
- 复现：force 下一次 transport error → `noteAttemptTransportError` → `pool.mark(NodeDead)` → 删除 `clients[fp]` → `v5Candidates` 过滤掉该节点。
- 期望：force 下旧写入不改变 v5 候选；或 v5 候选只来自 v5 自有状态。
- 实际：旧状态能移除 v5 候选；双状态机未隔离。
- 影响：一次本应由 v5 判定的传输错误会通过旧池提前淘汰候选，可能造成不必要的 no_candidate，也让回滚/影子统计不一致。

### Medium 1：gate probe 不遵守 region

- 位置：`node_system_v5.go:441-458`；`admission.go:878-926`
- 复现：gate active + `requiredRegion=jp`，`AdmitGateProbe` 没有 region 参数，可能返回 `Region=us` 的节点。
- 影响：区域约束在恢复探测路径被绕过；真实请求可能打到错误区域。
- 最小修复方向：`AdmitGateProbe(model, region, exclude)`，或在 `v5AdmitContext` gate 分支构造过滤后的候选。

### Medium 2：现有测试覆盖不足

- `n4_test.go` 的 `TestGateNeedsFailureRatio` 第二个用例注释写“30/50”，实际构造 10/10 且依赖最后一个 429；没有覆盖“第 10 个是 2xx”。
- `TestNoReburnWithin10m` 没有回放真实 pair；本报告的 290 pair/27,134 重复 429 才覆盖真实序列。
- 没有空 Egress 窗口计数断言、没有 gate probe region 断言、没有 force 旧状态隔离断言。
- 这些缺口让实现者报告中的“全绿”不足以证明 N4 边界。

## 10. 残余风险

1. **回放口径**：event-time 与 request-start 的窗口边界不同；本报告同时给出两者，结论一致，但 18:00 的 2% 是最终请求失败率，不是 attempt 失败率。验收/面板应固定口径，避免再次把 2% 与 50% 直接比较。
2. **14:30 风暴的 33 个 distinct**：gate 只触发一次符合要求，但单次触发后 backoff 是否足以覆盖 33 个出口的持续风暴，需要更长窗口（>15:08）验证；本报告只覆盖到 15:08。
3. **gate probe region**：当前静态代码确认缺口，未写失败测试阻塞放行；修复时应补测试。
4. **force 旧状态**：即使补齐 `noteAttemptTransportError` 的 bypass，`v5Candidates` 仍读取 `proxyPool.nodes/clients` 作为候选源；若要求完全双状态隔离，需要把候选快照移到 v5 自有注册表，这是架构级改动，不应塞进 N4。
5. **全量测试非绿**：两个 IP4RV 失败来自未跟踪测试；`TestProbationWithoutEvidenceExpiresBackToCooldown` 是 TempDir 清理 flake（单独重跑 PASS）。N4 自身边界失败必须先修。
6. **付费直连快速路径**：`egress.go:135-140` 的 `TierPaid && paidDirect` 在 force 分支之前，绕过 `Admit`；若产品要求所有 force 流量统一，需要单独决策。

## 11. 结论

**FAIL，不可进入下一阶段。** N4 的主路径（真实 429 回放、pair 冷却、NoCandidate、持久化降级、卡死恢复）大部分可用，但：

- **10/5 边界在窗口末尾为 2xx 时不触发**，是明确的验收失败；
- **空 Egress 仍污染 gate 窗口计数**；
- **force 旧状态旁路不完整**，旧 `pool.mark` 能移除 v5 候选；
- gate probe 的 region 过滤缺失是恢复路径的真实风险。

建议先修 Blocker 1/2 和 High 1，并为三条补最小测试；修完后重新跑本报告的 `TestAuditN4ReplayGate`、`TestAuditN4Boundaries`、`TestAuditN4EmptyEgress*`、`TestAuditN4PairNoReburn10m`、`TestAuditN4NoCandidateMain`，再决定是否进入 N5。

未提交任何代码；临时审计测试已删除。


## 12. F1–F3 修复复验（2026-10-27 17:23）

### 12.1 代码修复

- **F1**：`admission.go` 的 5 分钟窗口先计数（`Attempts`/`Failures`），distinct 仅由 429 更新；阈值 `Attempts>=10 && Failures*2>=Attempts` 在**每次 attempt** 后判定，不再只在 429 分支判定。活跃闸仍直接 return。
- **F2**：`at.Egress == ""` 在 `Attempts++` **之前**直接 return，空 Egress 不进入任何窗口计数、不更新 distinct。
- **F3**：
  - `v5Candidates` 不再读取 `proxyPool.clients` 快照过滤，而是对每个节点调用 `proxyPool.getClient(fp)`；缓存命中直接返回，未命中时懒创建，创建失败才跳过。冷启动不再只剩 direct。
  - `main.go` 的 `noteAttemptTransportError` 在 `v5LegacyGatesBypassed()` 时跳过 `pool.mark`，force 下旧池状态不再删除 client；client 缓存删除本身保留，下一次枚举会重新创建。

### 12.2 新增断言

```text
TestGateThresholdOnTenthAttempt        PASS  # 10th attempt 是 2xx 仍触发 5/10
TestGateNeedsTenAttempts               PASS  # 9 attempts 不触发
TestGateBelowFiftyPercent              PASS  # 25 个 2xx 后 24 个 429（24/49）不触发
TestGateEmptyEgressNotCounted          PASS  # 5 真实 429 + 5 空 Egress -> 5/5
TestV5CandidatesColdStart              PASS  # clients 为空、3 节点 -> 3 节点 + direct
TestV5CandidateSurvivesLegacyClientDelete PASS # 删除 client 后重新枚举仍包含节点
TestForceTransportErrorDoesNotMarkLegacyState PASS # force 下 transport error 不改旧 node state
```

### 12.3 变异（全部必须红）

| 变异 | 观察 |
| --- | --- |
| F1：阈值判定仅限 429 分支 | `TestGateThresholdOnTenthAttempt` FAIL |
| F2：空 Egress 正常计数 | `TestGateEmptyEgressNotCounted` FAIL |
| F3：恢复 `clients[fp] == nil` 过滤 | `TestV5CandidatesColdStart` FAIL |
| F3：去掉 `v5LegacyGatesBypassed()` 保护 | `TestForceTransportErrorDoesNotMarkLegacyState` FAIL |

四个变异退出码均为 1，恢复源码后定向 `-race` 通过。

### 12.4 复验命令与结果

```text
gofmt -l admission.go main.go node_system_v5.go n4_test.go   # 无输出
git diff --check                                             # 无输出
go vet ./...                                                 # exit 0
go test -run 'TestV5|TestGate|TestPair|TestNoReburn|TestMaxTwo|TestWinter|TestNoCandidate|TestForceTransportError' -race -count=1 .
ok github.com/shiyanYX/opencode2api 1.195s

go test -count=1 ./...
FAIL github.com/shiyanYX/opencode2api 261.662s
--- FAIL: TestProbationSuccessBecomesAvailable
--- FAIL: TestProbationPickedOnceThenFailureRecools
--- FAIL: TestProbationWithoutEvidenceExpiresBackToCooldown
--- FAIL: TestIP4RVLiveServedDrift
--- FAIL: TestIP4RVDocsDrift
```

- 三个 `TestProbation*` 的失败原文均为 `TempDir RemoveAll cleanup: ... directory not empty`，单独重跑：
  `go test -run 'TestProbationSuccessBecomesAvailable|TestProbationPickedOnceThenFailureRecools|TestProbationWithoutEvidenceExpiresBackToCooldown' -count=1 .` → `ok 0.024s`。属于既有异步状态保存与 TempDir 清理竞争，不是本次断言失败。
- 两个 IP4RV 失败来自未跟踪的 `zz_ip4agg_reverify_test.go`，单独重跑仍复现，与 N4/F1–F3 无关。
- 结论：**F1–F3 的 blocker 已关闭；全量仍有 2 个既有 IP4RV 失败和 3 个已复现的清理 flake，不能声称全量全绿。**

未提交生产代码；本次修复只进入 `test` 分支。
