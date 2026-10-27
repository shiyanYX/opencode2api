# N4 最终验收（`eb640df`）

## 0. 结论

**NO-GO（不放行 merge 到 `main`）**。强制路径、gate、pair、NoCandidate、持久化和大部分失效场景通过；但“force 运行中切回 `off`”存在可复现的租约泄漏，最终会把 `Admission` 的 K=8 信号量耗尽，导致回滚后新请求全部 `all_inflight`。这是本轮要求的回滚验收项，属于部署阻断。

另有两项非阻断但必须记录的残余风险：

- `/api/nodes` 仍是旧池视图；v5 pair 已冷却时接口仍显示 `state=available`、`cooldown_until=null`、`quota_state=ok`（面板冻结）。
- 强制模式下旧 `recordNodeSuccess` / `quotaHaltGuard.observeSuccess` / `shapeDriftObserveSuccess` / `shapeDriftObserve429` 仍有写入；v5 决策不读这些旧状态，但回滚到旧镜像可能继承它们。

## 1. 验收对象与基线

- HEAD：`eb640df`（`test` 分支）
- 前序：`9b220f1` → `23f2da7` → `eb640df`
- 计划：`docs/superpowers/plans/2026-10-27-node-system-v5.md` §12/§14
- 未修改生产代码、未提交；临时验收测试 `zz_n4_final_audit_test.go` 仅用于本轮，最终删除。
- 回放数据：
  - `c2026-10-24.jsonl`：2,478 条，fail 2,392
  - `c2026-10-26.jsonl`：13,724 条，fail 11,421
  - `c27.jsonl`：18,284 条（10-27 00:00–15:08），fail 15,784

## 2. §12/§14 逐条判定

| 项 | 判定 | 证据 |
|---|---|---|
| force candidate → 选路 → attempt → Observe → eligibility | PASS | `TestFinalReplayExactChain`、`TestFinalReplaySelection`；18:00/14:30/403 风暴均无 pair 冷却违规 |
| 429 率、NoCandidate 占比 | PASS | 见 §3；三窗口实际/策略 429 率与 NoCandidate 占比均有数字 |
| 同 pair 冷却期不二次尝试 | PASS | 三窗口 `cooldownViolations=0`；`TestFinalPairCooldownAndDirect` 首 429 后 30m 冷却，直接走 direct；direct 429 后 no_candidate；两次 2xx 恢复 |
| gate 只触发一次 | PASS | 18:00 0 次；14:30 1 次；403 风暴 1 次；`TestFinalReplayExactChain` 断言 |
| 18:00 / 19:55 低失败率窗口不触发 | PASS | 18:00 本次 0 次；19:55 前序 N4 回放 0 次，当前 gate 逻辑未改变该判定 |
| 10 attempts / 5 failures 边界 | PASS | `TestAuditN4Boundaries` 已在 `9b3e8d0` 修复：第 10 次为 2xx 也触发；9 attempts 不触发 |
| 空 Egress 不计数 | PASS | 5 个真实 429 + 5 个空 Egress 后 `Attempts=5 Failures=5 Egresses=3`；空 Egress 不创建 pair、不推进 gate 窗口 |
| NoCandidate 绝不直连 | PASS | `egress.go:168` 返回 `NoCandidate` 且不带 lease；`main.go:2974/3526` 两循环均先判 NoCandidate，返回 429 + `Retry-After: 60`；grep 仅这两个 `egress.Get` 调用点 |
| force 旧闸门旁路 | PASS（有限） | `markLocked`、`noteAttempt`、`note403`、`markFreeTierRejected`、`quarantineSub`、`confirmSub403` 在 force 下不生效；但见 §6 的旧 observer 残余 |
| v5 决策不读旧 `poolHalted`/shape/legacy score | PASS | `v5Candidates`/`v5AdmitContext` 只读节点列表、v5 pair/sub/direct；不调用 `poolHalted`/`shapeDriftBlockedEgress` |
| force → off 回滚 | **FAIL** | 正常请求切 off 可 200，旧池状态快照不变；但 in-flight 租约泄漏，见 §5 |
| force → off → force 从持久化恢复 | PASS | `TestFinalRollbackForceToOff` 将 pair 写入 `v5StatePath`，`resetV5ForTest` 后重新加载，pair 仍在 |
| 进程重启持久化 | PASS | `TestFinalProcessRestartPersistence` 用子进程读取状态；pair/gate/backoff round-trip |
| 损坏/截断/版本不符 | PASS | 全部返回空态 + error，不 panic；`TestFinalCorruptRuntimeDoesNotBlock` 仍可 Admit |
| 只读路径 | PASS（不阻断） | `/proc/1/...` Save 返回错误；load 得到空态，不 panic，不阻断 Admission |
| 全节点 cooldown / 全 dead / direct 429 | PASS | `TestFinalPairCooldownAndDirect`、`TestFinalAllDeadAndClockRollback` |
| 时钟回拨 | PASS | 未来 `Until` 不缩短、不 panic；过期后可恢复 |
| `/api/nodes` 不 panic、字段完整 | PASS | `TestFinalAdminNodesForce`：200，node 必填字段齐全，`healthy+exhausted+dead==len(nodes)` |
| 面板与 v5 冷却一致性 | **PASS（已知冻结，P2）** | v5 pair 已冷却时接口仍 `state=available`、`cooldown_until=null`、`quota_state=ok`；接口不暴露 v5 状态，无同一 payload 内字段互相冲突，但会误导操作者 |
| `go vet ./...` | PASS | exit 0 |
| 目标 race 测试 | PASS | `go test -race -count=1 -run 'TestV5|TestGate|TestPair|TestNoReburn|TestMaxTwo|TestWinter|TestNoCandidate|TestForce|TestFinal' .` ok，7.028s |
| 全量 `go test -race ./...` | **FAIL（非 N4 阻断）** | 见 §7：仓库内未跟踪的 `zz_*` 回放测试超时/失败；N4 目标测试无失败 |
| 自造变异 ≥4 且全红 | PASS（5/5 红） | 见 §8 |

## 3. 回放复算（真实 call_log）

### 3.1 exact-chain（每次尝试强制使用真实 egress，验证 Admission 冷却/NoCandidate）

| 窗口 | 请求 | 请求失败 | 尝试 | admitted | NoCandidate | gate 抑制 | 真实 429 | 实际放行 429 | pair 违规 | gate 触发 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 10-26 18:00–18:10 | 188 | 2 | 209 | 184 | 25 | 0 | 22（10.53%） | 3（1.63%） | 0 | 0 |
| 10-27 14:30–15:07 | 5,077 | 5,076 | 5,110 | 10 | 5,100 | 5,092 | 5,108（99.96%） | 9（90% of 10） | 0 | 1 |
| 10-24 07:00–07:30 403 风暴 | 48 | 48 | 200 | 10 | 190 | 190 | 134（67.0%） | 6（60% of 10） | 0 | 1 |

- 18:00 NoCandidate 25/209 = 12.0%；14:30 5,100/5,110 = 99.8%；403 风暴 190/200 = 95.0%。
- 18:00 实际日志 429 22 次，策略只放行 3 次；说明 pair 冷却把重复 429 压掉。
- 14:30 gate 在 10 次尝试内触发一次；此后 5,092 次尝试被 gate 抑制，未再次触发，符合“只触发一次”。
- 403 风暴窗口包含 19 次 403、134 次 429、38 次 connect_error；gate 由 429 触发一次，403 不触发 gate，但 sub 403 由 `observe403Locked` 记录。

### 3.2 selection（每次请求使用真实候选集）

| 窗口 | 请求 | admitted | NoCandidate | gate 抑制 | 放行 429 | 与真实首节点不同 |
|---|---:|---:|---:|---:|---:|---:|
| 18:00 | 188 | 188 | 0 | 0 | 1 | 20 |
| 14:30 | 5,077 | 10 | 5,067 | 5,067 | 9 | 9 |
| 403 | 48 | 10 | 38 | 38 | 8 | 9 |

选择与真实首节点不同是 UCB/探索的预期行为；关键是没有 pair 冷却违规，且 429 被压制。

## 4. 持久化与失效场景

- `TestFinalPersistenceRoundTripAndCorrupt`：
  - pair + gate 的 `SaveNodeStateV5`/`loadNodeStateV5` 完整 round-trip。
  - `"{bad"`、`"{\"version\":999}"`、空文件均返回空状态和 error，不 panic。
  - 只读路径 Save 错误，load 空态，不阻断。
- `TestFinalProcessRestartPersistence`：
  - 新进程（`os.Args[0] -test.run=TestFinalRestartChild`）读取 `/tmp/.../v5.json`；
  - `Pairs[PairKey("m","node:e1")]`、`Gate.Active=true`、`Gate.Backoff=2` 均恢复。
- `TestFinalCorruptRuntimeDoesNotBlock`：
  - 损坏的 `/data` 替代路径启动 `v5GetRuntime`，日志 `v5_state_load_failed`，仍能 Admit 并 Observe 2xx。
- `TestFinalAllDeadAndClockRollback`：
  - 全 dead → no_candidate；DeadUntil 过期 → probation；
  - 时钟回拨 1h 不缩短未来 Until；再前进 9h 可恢复。

## 5. 阻断缺陷：force→off 的 in-flight 租约泄漏

最小复现（临时测试 `TestFinalRollbackInFlightLease`）：

```text
force, Admit -> 取得 node:e1 的 *Attempt（sem 长度 1）
NODE_SYSTEM=off
v5ObserveUpstream(ctx, "m", EgressResult{v5: at}, 200, nil, nil, pinnedOCVersion)
=> force->off leaked in-flight lease: sem=1
```

代码路径：

- `node_system_v5.go:286` 先读 `mode := v5Enabled()`；
- `node_system_v5.go:287-289` 在 `mode == v5Off` 时直接 `return`；
- `v5ObserveUpstream` 因此没有执行 `rt.adm.Observe(at, class)`，也没有 `v5DropAttempt`；
- `Attempt.release` 只由 `Observe` 的 `once.Do` 执行，K=8 的 `sems["node:e1"]` 永远保留。

后果：切回 `off` 后旧路由仍可用，但同一进程再切回 `force` 时 `Admit` 会看到未释放的 sem；累计 8 个 in-flight 请求后，该 egress 永久 `all_inflight`，只能重启进程清除。`force→shadow` 同样会走 `v5ObserveUpstream` 的非 force 分支，风险相同。

修复方向（不实现）：租约必须在 `eg.V5()!=nil` 时无条件 `Observe`，或由 `callOpenCodeAPI`/`callOpenCodeAPIStream` 对 `Get` 后的 attempt 建 `defer` 归还；不能用“当前 mode”决定是否归还。

## 6. 旧状态写入残余

强制模式下 v5 决策不读旧状态；已验证 `markLocked`/`noteAttempt`/`note403`/`markFreeTierRejected`/sub 403 强制旁路。但下列旧 observer 仍执行：

- `main.go:3101/3645` `proxyPool.recordNodeSuccess`
- `main.go:3106/3650` `quotaHaltGuard.observeSuccess`
- `main.go:3107/3651` `shapeDriftObserveSuccess`
- `main.go:3299/3829` `shapeDriftObserve429`（旧 shape drift 状态）

它们不参与 v5 的 `Admit`，本次回放未造成 pair/gate 违规；但 force 运行后回滚到旧镜像时，旧 shape/quota 状态可能已改变，需要 drain 或接受“旧状态不等于进入 force 前”的解释。

## 7. 测试结果

- `go vet ./...`：exit 0。
- 目标 race：exit 0，`ok ... 7.028s`。
- 全量 `go test -race -count=1 ./...`（按工作树原样）：**FAIL**，`600.102s`。
  - `TestIP4RVLiveServedDrift` / `TestIP4RVDocsDrift`（`zz_ip4agg_reverify_test.go`）失败；
  - `TestT23RealLogHardGate` 2m4s 后触发 10m 整包超时。
  - 两个文件均为 `.gitignore` 的未跟踪 `zz_*_test.go`，不属于 N4 改动。
- 排除 `zz_t23_verify_test.go`、`zz_ip4agg_reverify_test.go` 后：仍 `600.186s` FAIL，`TestT39RealLogOverviewCriterion` 超时；`TestProbationPickedOnceThenFailureRecools` 出现 TempDir cleanup flake。
- 再把全部未跟踪 `zz_*_test.go` 移到 `/tmp/n4_ignored`（只保留本轮审计），`go test -race -count=1 ./...` 为 `317.829s` FAIL，**唯一失败就是 `TestFinalRollbackInFlightLease`**（`force->off leaked in-flight lease: sem=1`）。这说明其余 tracked 测试在 race 下通过；全量全绿被 §5 的回滚缺陷单独阻断，而不是被历史回放测试掩盖。
- 结论：原样全量 race 不能作为 N4 绿色证据；剔除历史/临时 `zz_*` 后，N4 目标测试仅剩 §5 的回滚阻断。

## 8. 变异证据（5/5 红）

工作树：`/tmp/n4finalmut`，基线 `eb640df`，复制本轮审计测试。

| 变异 | 目标测试 | 结果 |
|---|---|---|
| `eligibleLocked` 去掉 pair `Until` 判断 | `TestFinalPairCooldownAndDirect` | 红：`all cooling admit=(...node:e1...)` |
| `pairUCBLocked` 后验/UCB 符号反转 | `TestFinalSelectionPreferenceAndEligibility` | 红：`wrong posterior selected: node:b` |
| `gateMinAttempts=10` → 1e9 | `TestFinalReplayExactChain` | 红：`14:30 gateTrips=0 want 1` |
| 删除 `markLocked` 的 force 早退 | `TestForceLegacyWritesNoop` | 红：`force mark must be no-op` |
| `NoCandidate:true` → `false` | `TestFinalNoCandidateMainBothLoops` | 红：`stream=false status=400 want 429` |

日志：`/tmp/n4mut_pair.log`、`/tmp/n4mut_selection.log`、`/tmp/n4mut_gate.log`、`/tmp/n4mut_force.log`、`/tmp/n4mut_nocand.log`。变异后源文件均已 `git checkout --` 恢复。

## 9. 部署残余风险

1. **P0 回滚租约泄漏**：force→off/shadow 时 in-flight attempt 不 Observe，K=8 sem 泄漏；切换前必须 drain，或修复归还逻辑。当前不能把 `NODE_SYSTEM=off` 当作安全回滚。
2. **P1 冷启动**：v5 状态文件缺失/空时，`v5Candidates` 会列出所有节点（包括旧 `dead/exhausted`），只过滤无 client；`n<5` 探索会把首次请求送到陈旧节点。建议预热状态或明确接受冷启动探索。
3. **P2 面板冻结**：`/api/nodes` 不暴露 v5 pair/gate；v5 已冷却时仍显示 `available/ok`。面板不能作为 force 排障唯一依据。
4. **P2 旧 observer 写入**：`recordNodeSuccess`/`shapeDriftObserve*` 仍可能在 force 下改变旧状态；回滚旧镜像前需 drain 或接受旧状态偏差。
5. **CPU/内存**：`v5Candidates` 每次 `Get` 快照全部节点（约 212）并逐候选 `getClient`；`mark` 删除 client 后冷启动会新建 transport。本轮无生产压测，不能给出 RPS 上限；建议在 N1 前做 212 节点基准。
6. **gate 恢复延迟**：14:30 回放 gate 触发后 5,092 次被抑制；10m 后 canary/单节点探测才会恢复。`TestGateEscapeAfter10m` 单测通过，但真实回放未跨 10m。
7. **全量测试被历史回放拖垮**：原样 `go test -race ./...` 会命中未跟踪 `zz_*` 的超时/flake；剔除后 tracked 套件只剩 §5 的 in-flight 回滚失败。合并前应在干净 worktree 重跑。

## 10. 放行结论

**不放行 merge 到 `main`（NO-GO）**，直到：

1. 修复 force→off/shadow in-flight 租约归还，并加入本报告的最小复现；
2. 用干净 worktree 重跑全量 race（排除历史 `zz_*` 后仍应证明 N4 目标全绿）；
3. 对面板冻结与旧 observer 写入给出接受记录或补齐 v5 状态暴露。

若产品明确接受“回滚必须重启进程、不能热切 off”，可把 P0 降级为操作约束，但当前验收要求明确包含 force→off，因此本轮判定 FAIL。

## 补充：F7 修复、tracked-only race 与 212 节点开销

本节针对 `eb640df` 的最终 NO-GO 唯一阻断项 F7；原 §0/§5 的结论保留为历史记录，以本节为准。

### F7：force→off/shadow 租约泄漏（已关闭）

- `admission.go` 新增 `releaseAttempt(at *Attempt)`：`at.once.Do` 内只调用 `at.release()`，不写 pair/egress/gate 指标，幂等；`Observe` 之后再调用不会二次释放。
- `node_system_v5.go` 的 `v5ObserveUpstream` 在 `mode==off`、`v5GetRuntime()==nil`、`mode!=force`（shadow）三条早退分支均先 `releaseAttempt(at)`；force 路径仍走 `Observe`。
- 探索名额也随租约归还：`attemptLocked` 的 `release` 闭包在 `explore` 时递减 `exploreInflight`；`Observe` 不再重复递减。否则 `releaseAttempt` 虽释放 sem，却会让该 egress 永久 `all_inflight`。
- `TestFinalRollbackInFlightLease`：8 轮 force 取租约 → off Observe → `sem=0` → force 重取；再补一轮 force→shadow → `sem=0`。变异 `TestFinalRollbackInFlightLease` 去掉早退 `releaseAttempt`，首轮 `sem=1` FAIL；去掉 shadow 分支同样 FAIL。

判定：**P0 解除，F7 关闭。** 回滚不再需要重启进程，也不会在 8 个 in-flight 请求后进入 `all_inflight`。

### 量化 1：tracked-only 全量 race

排除未跟踪的历史 `zz_*`（35 个文件，含测试和测试 helper，先移至 `/tmp/n4-zz-moved`）后：

```text
gofmt -l admission.go node_system_v5.go n4_test.go node_system_v5_bench_test.go  # 无输出
git diff --check                                                                  # 无输出
go vet ./...                                                                      # exit 0
go test -race -count=1 ./...
ok   github.com/shiyanYX/opencode2api   316.863s
```

- 失败项：0；race：0；超时：0。
- 跑完已把 35 个 `zz_*` 全部还原（`ls zz_* | wc -l` = 35）。
- 定向 race（含 F7）：`go test -run 'TestFinalRollbackInFlightLease|TestV5|TestGate|TestPair|TestNoReburn|TestMaxTwo|TestWinter|TestNoCandidate|TestForce|TestF6' -race -count=1 .` → ok 1.263s。

### 量化 2：212 节点开销

环境：Intel i7-7500U @ 2.70GHz，`go test -benchmem`，212 合成节点 + direct。

| 基准 | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkV5Candidates212`（纯 `v5CandidatesFrom` 枚举） | 17,906 | 19,776 | 213 |
| `BenchmarkV5Admit212`（候选预构建，仅 `Admit` + 释放） | 149,477 | 16,760 | 6 |

100 rps 粗估（单核时间占比）：

- candidates：17,906 ns × 100 / 1e9 = **0.179%**
- admit：149,477 ns × 100 / 1e9 = **1.495%**
- 合计约 **1.67%** 单核；分配约 **3.65 MB/s**。

但真实 `v5AdmitContext` 对每次 `Get` 调用了两次 `v5Candidates`：一次用于 region/exclude 过滤，一次在 `AdmitPreferred` 内部。按两次枚举估算，candidates 约 **0.36% CPU / 5.6 MB/s 分配**，合计约 **1.85% CPU**；这还未计入 `proxyPool.snapshot()` 的锁和 `getClient` 懒创建。149µs 级 admit 主要花在 213 候选的排序/UCB，100 rps 尚可，但 213 allocs/请求会给 GC 持续压力。

优化建议（本次不改）：每次请求只构建一次候选快照并直接传给 `Admit`，消除第二次枚举；或按 model 缓存候选快照并在节点集合/客户端状态变更时失效；Benchmark 因而应覆盖“真实一次 `v5AdmitContext`”而不只是预构建候选的 `Admit`。

### 最终判定

- F7：**closed**；P0 阻断解除。
- tracked-only race：**green**。
- 212 节点：100 rps 下 CPU 可接受；分配偏高，属后续优化项，不是 N4 阻断。
- 原始 §6 的旧 observer 残余（`recordNodeSuccess`/`quotaHaltGuard`/`shapeDrift` 写入）仍是 P2，不在本轮 F7 范围。
