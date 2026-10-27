# W2 force 接管：独立复核

> 复核对象：`83d2d08` + `e466913`（`test` 分支，HEAD `e466913`）
> 基线：`4d62dc3`（W1 shadow）
> 计划：`docs/superpowers/plans/2026-10-27-node-system-v5.md` §2/§3/§7
> 实现者报告：`docs/research/scoring2/26-w2-report.md`
> 复核方式：只读代码 + 独立复现测试 + 变异测试；未修改生产文件，未提交。

## 0. 结论

**NO-GO。不能启用 `NODE_SYSTEM=force`。**

当前 W2 的主干方向正确：`egress.Get` 的 force 分支确实调用 `v5AdmitContext`，普通请求路径也确实通过 `v5Observe` 在 2xx / 非 2xx / 网络错误上观察一次；off/shadow 的路由语义没有变化；已有 `TestV5` 测试和 `-race` 全绿。

但实现与计划在四个关键点上不一致，其中前两个会直接伤到生产流量：

1. **B1：无候选时仍然回退直连，且这个回退没有租约。** 计划 §2.2 明确写「`egress.go` 不再自行回退直连（全冷却返回 `NoCandidate`）」，计划 §6 删除清单也再次写「全冷却不再回退直连」。代码在 `egress.go:162` 无条件返回 `Direct:true`；该直连不经过 `Admit`，因此不经过 UCB、不检查 `direct.until`、不占 K=8 信号量、不产生 pair 观察、也不受 `RequiredRegion` 约束。全冷却时每个请求都会打本机 IP，形成无上限直连风暴。
2. **B2：force 路径没有 defer 归还租约。** 计划 §2.2 要求「每次发送恰好一次，defer 保证」以及「panic/提前 return 也不泄漏」。`callOpenCodeAPI` / `callOpenCodeAPIStream` 只在 build 错误处手动 `v5DropAttempt`（`main.go:3020-3022`、`3557-3558`），没有 defer；一旦 Get 之后 panic，租约永久不释放。
3. **B3：旧闸门的“决策”旁路完整，但“旧状态写入”没有旁路。** `main.go:2869` 的 `poolHalted` 判断、`main.go:3094-3098` / `3625-3628` 的 `recordNodeSuccess`、`quotaHaltGuard.observeSuccess`、`shapeDriftObserveSuccess` 在 force 下仍执行。它们不直接改本次路由，但会清掉旧 shape/quota 闩、改旧 node 生命周期；回滚到 off 后旧闸门状态已被 force 流量污染。
4. **B4：force 没有加载或持久化 v5 状态，也没有 probe 循环。** `v5GetRuntime` 在 `node_system_v5.go:113` 直接 `NewNodeStateV5(time.Now())`；生产代码没有任何 `LoadNodeStateV5` / `SaveNodeStateV5` 调用。`AdmitProbe` / `AdmitLifecycleProbe` 只有定义，没有生产调用点。重启后所有 strikes/until 清零；全冷却时没有 probe 自动恢复。

此外，已有 `TestV5` 测试**不能**证明上述约束：变异 M2（删 egress 的 `v5DropAttempt`）、M4（删 region 过滤）、M7（删首个 poolHalt 的 bypass）在现有全套 `TestV5` 下仍然全绿。实现者报告里的“mutation 全红”是加了复核测试后的结果，不是仓库测试的结果。

## 1. 复核基线、版本与命令

### 1.1 提交与工作区

- `4d62dc3 feat(v5): add W1 shadow wiring (observe only)`：基线。
- `83d2d08 feat(v5): add W2 force routing takeover`：force 选路。
- `e466913 fix(v5): release force leases on build errors`：build 错误归还。
- `git status` 只有未跟踪的 `.research/scoring2/`、`docs/mockups/`、`docs/research/scoring2/20-plan-review-r7.md`、`21-plan-review-r8.md`、`27-w2-review.md`、`zz_ip4v_layout_new.go`；W2 生产文件没有未提交改动。
- W2 diff（`git diff --stat 4d62dc3..e466913`）：`admission.go +1`、`bun_egress.go +1/-1`、`egress.go +31`、`main.go +80/-33`、`node_system_v5.go +117`、`node_system_v5_force_test.go +240`、`proxy_node.go +7`；共 603 插入 / 33 删除。

### 1.2 计划版本漂移（必须先确认）

任务里写的是计划 **v5.1**，但磁盘上的
`docs/superpowers/plans/2026-10-27-node-system-v5.md` 已经是 **v5.3**（358 行，§12 明确写 R8-delta 到 v5.3）。v5.1 的 3/60s AND、K=8+500ms 排队、canary、5m/30m/2h/5h 已被 v5.3 替换为：

- 首个 429 立即冷却，阶梯 `60s / 2m / 5m / 5m`；
- 2xx 清零 `strikes/hard/until`；
- 无 canary，probe 退避 `60s → 2m → 5m → 15m → 1h → 6h`；
- 全冷却返回 `NoCandidate`，不回退直连；
- K=8，不排队，全在途返回 `all_inflight`；
- direct 是 `pair=(model,direct)`，429 时另设顶层 `direct.until=now+60s`；
- 回滚 = 重新部署上一镜像，不保留 `legacy` 路径。

本报告以**当前磁盘 v5.3 + HEAD `e466913`** 为准；如果评审对象确实是 v5.1，那么计划文件本身已经漂移，不能拿当前的实现去对一份不存在的 v5.1 文本做 GO 判定。

### 1.3 执行的验证

```bash
# 已有 W2 测试
GOMODCACHE=$PWD/.modcache GOPROXY=off \
  go test -run 'TestV5' -race -count=1 .
# => ok, 0 failures

# 复核用临时测试（已删除，避免污染工作区）
# zz_w2_audit_test.go
# -race -count=1 逐条运行
# 结果见 §2/§5/§6/§7

# 全量测试（移除复核测试后）
GOMODCACHE=$PWD/.modcache GOPROXY=off go test ./...
# => 仅有 TestIP4RVLiveServedDrift / TestIP4RVDocsDrift 两个失败，
#    来自未跟踪的 zz_ip4agg_reverify_test.go，与 W2 无关
```

工作树：`/tmp/w2base`（`4d62dc3`）、`/tmp/w2review-mut`（`e466913`）；`/tmp/w2mul` 是他人既有产物，未复用。复核测试在 `/tmp/w2review-mut` 上做变异，生产工作区未植入测试文件。

## 2. 租约守恒

### 2.1 `Get` 之后每条路径的归还情况

以非流式 `callOpenCodeAPI` 为准（流式 `callOpenCodeAPIStream` 同构，行号对应 3510 以后）：

| 位置 | 路径 | 是否归还 |
|---|---|---|
| `main.go:2970` | `egress.Get` 取得 `egResult.V5()` | — |
| `:2972-2977` | force 下旧 `poolHalted` 提前 503 | 被 `v5LegacyGatesBypassed()` 跳过，不触发 |
| `:2982` | `bunEgressResult` | 保留 `v5`（`bun_egress.go:80-85`） |
| `:2984-2987` | `shapeDriftSetTrialEgress` | 被 bypass 跳过 |
| `:2988-2992` | 免费层 + direct + 无 eligible node：只记 `node_cooldown` | **不 return**，继续发送（见 §6 的可观测问题） |
| `:2993-2999` | `blockedEgress` 503 | 被 bypass 跳过 |
| `:3004-3007` | `allSubsQuarantined` 503 | 被 bypass 跳过 |
| `:3019-3022` | `buildOCRequestWithEndpoint` 失败 | **手动 `v5DropAttempt`，正确** |
| `:3051` | `client.Do` 错误 | `v5Observe(0,nil,err)`，正确 |
| `:3089-3098` | 2xx | `v5Observe(200...)`，正确（在读 body 之前） |
| 非 2xx | 403/426/429/5xx | `v5Observe(status,...)`，正确（在重试/continue 之前） |
| `:3099-3102` | 2xx 读 body 失败 | 已 Observe，正确 |
| `:3289` | 旧 `shapeDriftObserve429` | 被 bypass 跳过 |
| `:3349` | direct 429 + 免费层旧 `quota_halt` | 被 bypass 跳过 |
| panic | Get 之后任意 panic | **不归还** |

结论：**普通成功/失败/网络错误路径确实恰好 Observe 一次**；`v5DropAttempt` 也正确处理了“未发出”的 explore 计数（`node_system_v5.go:332-336`）。但计划要求的 defer 保障不存在，panic 会泄漏。

### 2.2 复核测试结果

临时测试（均带 `-race`）：

- `TestAuditRepeatedDropNoLeak`：连续 64 次 `v5Admit` + `v5DropAttempt`，结束后 sem 长度 0、`exploreInflight` 0。**PASS**。
- `TestAuditNoClientDropsLease`：候选节点没有 client，`egress.Get` 走 `egress.go:160` 的 drop；sem 长度 0。**PASS**。
- `TestAuditModeChangeLeaksLease`：force 下取得租约，再把 `NODE_SYSTEM` 改成 `shadow` 后调用 `v5ObserveUpstream`。sem 仍为 1。**FAIL**（`node_system_v5.go:212-218` 在 shadow 分支不 Observe 租约）。
- `TestAuditPaidDirectUsesAdmission`：`TierPaid + paidDirect + force` 在 `egress.go:139-140` 的快速路径直接返回 direct，没有租约。**FAIL**（如果付费直连是计划内的例外，需要计划明确写出；当前计划 §2.2 写的是“所有尝试必须走这里”）。

`TestAuditModeChangeLeaksLease` 在生产里是否可达取决于 `NODE_SYSTEM` 是否会在进程内变化；当前实现每次都读环境变量，但正常部署是进程级固定值，所以它更像**测试/热切换风险**，不是主 blocker。真正的主 blocker 是 panic 无 defer；而 build 错误路径已经由 `e466913` 修好。

### 2.3 结论

- **普通路径：通过。**
- **panic 路径：不通过（计划 §2.2 明确要求）。**
- **模式切换：不通过（当前测试可复现泄漏）。**
- **付费直连：不在 v5 租约体系内，计划未写明例外。**

## 3. 旧闸门旁路

### 3.1 决策点

我按 `grep` 把所有旧闸门调用点分成“会改变路由/提前失败”和“只写旧状态”两类。

**已正确包在 `!v5LegacyGatesBypassed()` 里的决策点**：

- `quotaHaltGuard.gate`：`main.go:2878-2880`、`3417-3419`；
- region 预检查：`main.go:2902`、`3444`；
- `poolHalted`：`main.go:2948`、`2972`、`3164`、`3488`、`3512`、`3682`；
- `shapeDriftBlockedEgress`：`main.go:2967-2969`、`3507-3509`；
- `shapeDriftSetTrialEgress`：`main.go:2984-2986`、`3522-3524`；
- `blockedEgress` 的 503：`main.go:2993-2999`；
- `allSubsQuarantined`：`main.go:3004-3007`、`3542-3545`；
- `direct 429` 的旧 `quota_halt`：`main.go:3349-...`、`3863-...`。

所以：**force 下没有任何旧闸门能让本次请求提前 503/429，或把选路改回旧节点池**。这一条主干是成立的。

### 3.2 未包在 bypass 里的旧状态写入（bug）

以下调用在 force 下仍执行：

| 位置 | 调用 | 影响 |
|---|---|---|
| `main.go:2869` | `noteAttemptTransportError` 内的 `pool.poolHalted(time.Now())` | force 的传输错误仍受旧 pool halt 影响；旧 halt 期间不 mark 旧节点 dead |
| `main.go:3094` / `3625` | `proxyPool.recordNodeSuccess(nodeFp)` | force 的 2xx 会把旧 node 从 probation 提升、清旧失败计数 |
| `main.go:3096` / `3627` | `quotaHaltGuard.observeSuccess(egressKey)` | force 的 2xx 会推进旧 quota 闩的恢复滞回，可能提前解闩 |
| `main.go:3098` / `3628` | `shapeDriftObserveSuccess(modelID, egressKey, now)` | force 的 2xx 会立即清旧 shape drift 闩 |
| `main.go:3002` / `3540` | `proxyPool.noteAttempt(nodeFp, now)` | 旧节点尝试计数被 force 流量污染 |

实现者报告承认“旧 `pool403`/`sub` 的记账未删除，force 只是不再让它们否决新选路”。这句话对“本次路由”成立，但对**回滚**不成立：回滚到 off 后，旧闸门会读到被 force 2xx 清掉的闩/被提升的节点，旧逻辑不再处于切换前状态。尤其 `shapeDriftObserveSuccess` 是**单次 2xx 立即解闩**，一条 force 流量就能把旧全局闩清掉。

### 3.3 可观测性错误

`main.go:2988-2992` 在 force + direct + 无 eligible node 时记录：

```go
callLogSuppressedAttempt(ctx, modelID, "", "node_cooldown", EgressKeyDirect, nil)
```

但这一条 direct 实际会继续发送（`egress.go:162` 的回退），并不是被抑制的 attempt。结果：

- 面板会看到一条“node_cooldown 抑制”，实际请求已发到 direct；
- 真正的 direct 回退没有 attempt/lease，call_log 里没有对应的 `admit`，无法重放；
- `v5Fallback` 同一 reason 每分钟最多一条（`node_system_v5.go:386-416`），风暴时大量回退被合并，离线无法估计比例。

这不改变路由，但会污染验收和事后归因，属于必须修的观测缺陷。

### 3.4 结论

- **旧闸门决策旁路：通过。**
- **旧状态写入旁路：不通过。**
- **回退可观测性：不通过。**

## 4. off/shadow 行为不变

### 4.1 代码语义

- `v5Enabled()` 返回 `off` 时，`egress.Get` 不进入 force 分支（`egress.go:145`），直接走原 `proxyPool.pickForRegionExcluding` / 静态 socks5 路径（`egress.go:165-178`）。
- `v5LegacyGatesBypassed()` 只在 `v5Enabled()==v5Force` 时为 true（`node_system_v5.go:325`）；off/shadow 下所有旧闸门条件保持原判断。
- `v5ObserveUpstream` 在 off 下第一行返回（`node_system_v5.go:212-215`）；shadow 走 `Predict` / `ObserveVirtual`，不取信号量、不改变发送路径。
- `v5GetRuntime` 只在非 off 时创建；off 下 `v5StatsForResponse` 返回 nil 保留旧 JSON（`node_system_v5.go:425-434`）。

### 4.2 对照测试

在 `/tmp/w2base`（`4d62dc3`）与 HEAD 各跑一个最小对照：

- 单节点、`NODE_SYSTEM` 未设置：两者都走旧节点，`EgressResult.V5()==nil`，无 v5 租约。
- `NODE_SYSTEM=off`、`NODE_SYSTEM=v5-shadow`：HEAD 仍走旧节点，不进入 force 分支；shadow 只记录，不改变 `Direct/NodeFP`。
- 结果：**PASS**。

限制：这是语义对照，不是完整基线测试套件的逐项 diff；但 W2 的 off 分支没有新增决策逻辑，`EgressResult` 只多了一个 `v5` 字段，调用方不使用。就 W2 范围而言，off/shadow 路由不变成立。

### 4.3 结论

**通过（语义对照）**。需要在 N1 前补一条正式回归测试，防止未来把 force 分支放到 off 前面。

## 5. region 语义

### 5.1 正常路径

`v5AdmitContext` 在 `node_system_v5.go:362-369` 对 `region != ""` 做排除：

```go
if region != "" && (c.Direct || c.Egress == EgressKeyDirect || c.Region != region) {
    ex[c.Egress] = true
}
```

- direct 被排除；
- region 不匹配的 node 被排除；
- 匹配的 node 才进入 `AdmitPreferred`。

`TestV5ForceRegionFilter` 只覆盖了“jp 环境 + jp 候选”的正向路径，验证了 node:jp 被选中；它**没有**覆盖“jp 环境 + 只有 us 候选”或“jp 环境 + 只有 direct”的负向路径。

### 5.2 复核测试

- `TestAuditRegionMismatchRejected`：jp 只有 us 候选时，`v5Admit("m","jp")` 返回 nil。**PASS**（资格层正确）。
- `TestAuditRegionMismatchMustNotDirect`：同一场景下 `egress.Get` 走 `egress.go:162` 回退 direct。**FAIL**：region=jp 的请求最终打到了不受 region 约束的 direct。
- `TestAuditPaidDirectUsesAdmission`：`TierPaid + paidDirect` 在 `egress.go:139` 直接返回 direct，也在 region 检查之前。**FAIL**（若付费直连是预期例外，计划需要写清楚；当前计划没有）。

### 5.3 结论

- **Admission 的 region 过滤：通过。**
- **egress 层的 region 最终语义：不通过**，因为 no-candidate 回退 direct 绕过了 region 排除；付费直连快速路径也绕过。

这和 B1 是同一个根因：`Admit` 拒绝了 direct，但 `egress.Get` 在拒绝之后又自己把 direct 拿回来了。

## 6. direct 语义

### 6.1 direct 作为候选

`v5Candidates` 在 `node_system_v5.go:125-160` 末尾追加：

```go
Candidate{Egress: EgressKeyDirect, Direct: true, Model: model}
```

`Admission` 的 `eligibleLocked` 在 `admission.go:464-468` 检查顶层 `st.Direct.Until`；`Observe` 在 `admission.go:380-385` 对 direct 429 设置 `Direct.Until = now+60s`。因此：

- direct 确实参与 `Admit` 的资格与 UCB 排序；
- direct 429 会冷却所有 model 的 direct pair；
- 这部分**符合计划 §2.3**。

### 6.2 no-candidate 回退

`egress.go:150-162`：

```go
if at, ok := v5AdmitContext(...); ok {
    if at.Direct { return ... v5: at }
    ...
}
return EgressResult{Client: eg.direct, Direct: true}
```

`v5AdmitContext` 返回 false 的情形包括：

- 所有 pair 都在 `until`；
- 所有 sub 隔离；
- 所有 egress dead；
- K=8 全在途；
- region 不匹配；
- direct 自己也在 `Direct.Until`。

这些情形下**全部**无条件返回 direct。具体后果：

1. **绕过 `direct.until`**：direct 刚 429 冷却 60s，下一次 no-candidate 仍会打 direct。复核测试 `TestAuditNoCandidateIgnoresDirectCooldown` 复现：`Direct.Until` 已设置，`egress.Get` 仍返回 `Direct:true`。**FAIL**。
2. **绕过 K=8**：回退 direct 没有 `Attempt`、没有 sem，所有并发都放行。风暴时本机 IP 的上游并发没有上限。
3. **绕过 region**：见 §5。
4. **不产生 pair 观察**：回退 direct 的 2xx/429 不会进 `Admission.Observe`，所以 direct 的 UCB 后验、`Direct.Until` 都不会更新。direct 被 429 后仍然继续被回退使用，形成“越 429 越打”的循环。
5. **call_log 没有 attempt**：只有 `v5_fallback`（每分钟同 reason 一条）和实际 outcome；无法还原每次回退的占比。

### 6.3 付费直连

`egress.go:135-141` 的 `TierPaid && paidDirect` 在 force 之前返回 direct，不经过 `Admit`、不经过 region、不产生 v5 pair。如果这是产品上有意保留的付费快速路径，计划必须明确写成例外；否则它与 §2.2“所有尝试必须走这里”冲突。

### 6.4 结论

- **direct 作为 UCB 候选：通过。**
- **direct 429 的 pair 冷却：通过（仅对经过 Admit 的 direct）。**
- **no-candidate / region / K 全在途时的 direct 回退：不通过，是 B1 的核心。**

## 7. 变异测试与测试充分性

### 7.1 变异结果（临时测试加入后）

在 `/tmp/w2review-mut`（`e466913`）上做 7 个变异，每个变异后跑带复核测试的 `-race`：

| 变异 | 结果 | 捕获测试 |
|---|---|---|
| M1 `v5DropAttempt` 改回裸 release | 红 | `TestAuditRepeatedDropNoLeak`（exploreInflight=1） |
| M2 删除 `egress.go:160` 的 `v5DropAttempt` | 红 | `TestAuditNoClientDropsLease`（sem=1） |
| M3 `v5LegacyGatesBypassed` 恒 false | 红 | 旧 poolHalt 测试 |
| M4 删除 `c.Region != region` | 红 | `TestAuditRegionMismatchRejected`（jp 选中 us） |
| M5 `bunEgressResult` 不保留 `v5` | 红 | `TestV5ForceBunPreservesLease` |
| M6 删除 force 的 `Observe` | 红 | `TestAuditNoClientDropsLease` / 租约测试 |
| M7 删除首个 poolHalt 的 bypass | 红 | `TestAuditForceBypassesPoolHalt` |

**结论：复核测试能杀死 7/7。**

### 7.2 仓库现有测试的缺口

同一组变异，**只跑仓库自带的 `TestV5`**：

```text
M2（删 egress drop）      -> ok, 0 failures
M4（删 region 过滤）      -> ok, 0 failures
M7（删 poolHalt bypass）  -> ok, 0 failures
```

原因很具体：

- `TestV5ForceRegionFilter` 只测正向 jp 选中，不测 jp + us 的拒绝；
- 没有测试“候选存在但 client 缺失”的租约归还；`TestV5ForceGetUsesAdmission` 用的是有 client 的候选；
- 没有测试真正进入 `callOpenCodeAPI` 的 poolHalt 集成路径；`TestV5ForceBypassHelper` 只测 helper 布尔值。

因此实现者报告里“mutation 全红”不能作为“仓库测试充分”的证据。**当前测试套件不能阻止 B1/B4 类回归。**

### 7.3 `TestV5ForceNoCandidateFallsBackDirect` 的问题

这个测试**把错误行为固化了**：它断言 no-candidate 返回 direct。计划要求 no-candidate 返回 503/`NoCandidate`，测试与计划相反。修复时应删除或改写成：

```text
no candidate -> EgressResult.Direct == false / NoCandidate == true
```

并新增 `TestV5ForceNoCandidateReturns503`。

## 8. 与计划、实现者报告的偏差

| 计划/报告 | 代码 | 判定 |
|---|---|---|
| 计划 §2.2：`egress.go` 不回退直连，全冷却 `NoCandidate` | `egress.go:162` 无条件 direct | **B1，违反计划** |
| 计划 §2.2：`Observe` 恰好一次，defer 保证，panic 不泄漏 | 无 defer；panic 泄漏 | **B2，违反计划** |
| 计划 §2.3：direct 是 pair，429 后 `Direct.Until` | 经 Admit 的 direct 正确；回退 direct 绕过 | **部分通过；回退路径违反** |
| 计划 §3.2：K=8，不排队，全在途 503 | 回退 direct 绕过 K，直接发送 | **违反** |
| 计划 §2.2：region 不匹配不选 | Admission 不选；回退 direct 仍发送 | **违反** |
| 计划 §7：重启保留 `strikes/until` | `v5GetRuntime` 从不加载/保存 | **B4，违反** |
| 计划 §2.3：无流量 probe 恢复 | `AdmitProbe` 无生产调用点 | **B4，违反** |
| 报告：无候选回退直连是“已知偏差” | 代码确实如此 | 报告诚实，但计划未接受；不能 GO |
| 报告：mutation 全红 | 复核测试 7/7 红；仓库 `TestV5` 漏 M2/M4/M7 | **测试证据不成立** |

## 9. 残余风险

1. **重启风暴（高）**：force 进程重启后 `NewNodeStateV5` 空状态，所有 429 冷却、strikes/hard、sub 隔离、direct 冷却全部丢失；10-26/10-27 那种风暴会立刻重演。计划 §7 明确要求保留，当前实现没有。
2. **无 probe 恢复（高）**：所有 pair 冷却或全部 dead 时，`AdmitProbe` 没有调用者；如果按 B1 修掉 direct 回退而没有补 probe，所有请求会永久 503，直到外部流量触发；当前 direct 回退掩盖了这个问题。
3. **panic 租约泄漏（中高）**：Get 之后 panic 不会释放 sem；K=8 的出口被永久占满。需要 defer 或统一 `Attempt` 生命周期。
4. **旧状态污染（中高）**：force 2xx 清 `shapeDrift`、推进 `quotaHalt` 恢复、提升旧 probation；回滚后不是切换前状态。
5. **回退风暴（高，若 B1 不改）**：全冷却时 direct 无 K、无 UCB、无 region、无冷却，直接打本机 IP，可能触发上游对本机 IP 的封禁。
6. **可观测性（中）**：`node_cooldown` 把未抑制的 direct 回退记成 suppressed attempt；`v5_fallback` 每 reason 1/min 合并，离线无法算回退率/倾向。
7. **付费直连例外未定义（中）**：计划说所有尝试走 Admit，代码在付费 fast path 绕过；需要产品/计划明确。
8. **UCB tie-break 与计划不完全一致（低）**：`selection.go:94-150` 的成熟组 tie-break 用总 `LastFail`，计划写“近 1h 失败更少”；不是 W2 路由 blocker，但 N3 前要对齐。
9. **K 硬编码（低）**：`NewAdmission(st, 8, ...)` 固定 8，计划写 `egress_inflight_max`；配置项没有接。
10. **测试充分性（高）**：现有 `TestV5` 对 M2/M4/M7 全绿；必须把复核测试并入仓库，否则类似问题会再次漏过。

## 10. 最终判定与最小修复清单

**判定：NO-GO（不能启用 force；代码可继续留在默认 off 后，但当前提交不能作为 force 上线依据）。**

进入下一轮前必须完成：

### P0（blocker）

1. **删除 `egress.go:162` 的 direct 回退**，改为显式 `NoCandidate`：
   - `EgressResult` 增加 `NoCandidate bool` 或返回结构化 reject；
   - 两条 main 循环返回 503 + `Retry-After`（用 `Admission.RetryAfter(model)`，不是固定 1）；
   - `RequiredRegion` 不匹配时也走 `NoCandidate`，不得 direct；
   - direct 只通过 `Admit` 返回，必须带租约、进 UCB、受 `Direct.Until` 和 K 约束。
2. **补 defer 归还**：在 `Get` 之后注册 `defer`，若 `v5Seen==false` 则 `v5DropAttempt(egResult.V5())`；panic 路径也必须释放。
3. **force 期间不写旧状态**：`recordNodeSuccess`、`quotaHaltGuard.observeSuccess`、`shapeDriftObserveSuccess`、`noteAttemptTransportError` 的旧 `poolHalted` 分支必须在 `!v5LegacyGatesBypassed()` 内；或明确让 force 只写 v5 状态。回滚前旧状态必须与切换前一致。
4. **加载/保存 `node_state_v5.json`**：`v5GetRuntime` 使用 `LoadNodeStateV5`，退出/周期保存；重启后 `strikes/until` 保留。
5. **接 probe**：在 no-candidate / 全 dead 时启动单飞 `AdmitProbe` / `AdmitLifecycleProbe`，遵守 v5.3 的 `60s→2m→5m→15m→1h→6h`，网络调用在锁外，带 20s deadline。否则 B1 修掉回退后会永久 503。

### P1（必须随 P0 或紧随）

6. 改写 `TestV5ForceNoCandidateFallsBackDirect` 为 no-candidate 拒绝测试；新增 region mismatch、direct cooldown、panic、mode-change、legacy-state-untouched、restart、probe 测试。
7. 把复核测试的 M2/M4/M7 纳入仓库测试，确保三个变异都能杀死。
8. 修 `main.go:2998`：force 下 direct 回退不再记 `node_cooldown` suppressed attempt；如果保留 fallback，必须记录真实 attempt/lease（但 P0 已要求删除 fallback）。
9. 明确付费直连 `TierPaid+paidDirect` 是否为计划例外；如果是，写进计划并加 region/上限说明；如果不是，走 Admit。

### P2（N3 前）

10. UCB tie-break 的“近 1h 失败”与计划对齐；K 从配置读取；`v5Fallback` 记录完整 attempt id/原因，不只每分钟一条。

---

## 附录 A：复核测试与命令

复核测试文件为临时 `zz_w2_audit_test.go`，已删除；核心断言如下（可在 `e466913` 上复建）：

```go
// 1) no candidate 不得 direct
v5CandidatesHook = func(string) []Candidate { return nil }
got := eg.Get(EgressRequest{Model: "m"})
// 期望 got.Direct == false / NoCandidate；当前 got.Direct == true

// 2) region 不匹配不得 direct
v5CandidatesHook = auditCands("m", Candidate{Egress: "node:us", Region: "us"})
got := eg.Get(EgressRequest{Model: "m", RequiredRegion: "jp"})
// 期望拒绝；当前 got.Direct == true

// 3) direct 429 后 no-candidate 不得再 direct
at := eg.Get(...); rt.adm.Observe(at.V5(), Outcome429)
v5CandidatesHook = nil
got = eg.Get(...)
// 期望拒绝；当前 got.Direct == true

// 4) panic/模式切换归还
at := eg.Get(...); t.Setenv("NODE_SYSTEM","shadow")
v5ObserveUpstream(...) // sem 仍为 1
```

运行：

```bash
cd /tmp/w2review-mut
GOMODCACHE=/home/jingyx/project/opencode2api-test/.modcache GOPROXY=off \
  go test -run 'TestAudit' -race -count=1 -v .
```

## 附录 B：关键行号索引

- `egress.go:135-141` 付费直连快速路径；`:145-162` force 分支；`:160` 缺 client drop；`:162` 无条件 direct 回退。
- `node_system_v5.go:104-121` runtime 空状态；`:211-235` Observe 入口/模式分支；`:328-344` drop；`:351-384` Admit + region；`:386-416` fallback 事件。
- `admission.go:118-204` AdmitPreferred；`:304-393` Observe；`:441-451` attemptLocked；`:464-468` direct eligibility。
- `node_state_v5.go:90` Load；`:123` Save；两者均无生产调用。
- `main.go:2869` 未 bypass 的 poolHalted；`:2878`/`:3417` quota gate；`:2970`/`:3510` Get；`:3019-3022` build drop；`:3094-3098`/`:3625-3628` 旧状态写入；`:3349`/`:3863` direct 429。
- `selection.go:94-150` UCB/tie-break。
- 计划：`docs/superpowers/plans/2026-10-27-node-system-v5.md:41-63`（唯一流水线）、`:95-138`（冷却/恢复/probe）、`:180-185`（删除清单）、`:280-289`（重启/上线）、`:295-315`（N1/N2 验收）。
