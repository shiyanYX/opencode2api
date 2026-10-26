# T6 · IP-1 confirm-then-mark 实现说明（Lead 亲自实现）

> 背景：两个队友在同一任务上连续失败且零写入。经核对工作区干净后由 Lead 直接实现。
> 规格：`.research/T1-403-design-v2.md`（673 行，已过对抗性评审）。
> 分支：test @ `a19d3a4` 基础上的**未提交工作区改动**（按约束未 commit）。

## 1. 改了哪些文件

| 文件 | 改动 | 说明 |
|---|---|---|
| `main.go` | +133 / -? | 两条重试循环各加：`attemptVer`、块(3) 换节点、确认侧 mark、`freetier_switch_noop`；`buildOCRequestWithEndpoint` 增加 `ver` 参数；新增 `ocClientVersion()` 访问器 |
| `proxy_node.go` | +89 | `ProxyNode.GateState` / `nodeRuntimeState.gate_state`（落盘）；`sweepExpiredLocked` 豁免；`unmark` 清 gate_state；`availableCount()`；`markFreeTierRejected()`；T3 接线钩子 |
| `quota.go` | +7 | `classifyRegionRestriction` 先排除 FreeTierError（v2 §9-M2 的必然要求，见 §4） |
| `zz_freetier_confirm_test.go` | 新增（gitignored） | 9 条回归测试，含 E2E / 变异 teeth / C1 并发 / 复活契约 |

`git diff --stat`（不含未跟踪的测试文件）：
```
 main.go       | 133 ++++++++++++++++++++++++++++++++++++++++++++--
 proxy_node.go |  89 ++++++++++++++++++++++++++++++++++++++
 quota.go      |   7 ++
```

## 2. v2 要求 → 实现位置

| v2 条目 | 实现 | 位置 |
|---|---|---|
| **C1** `pendingVer` + mark 前版本比对 | `pendingVer` 与 `pendingBadFp` 成对记录；成功侧要求 `pendingVer == attemptVer`，否则发 `freetier_confirm_rejected_version_changed` | `main.go` 两处成功路径；事件在 `callLogEvent` |
| **C2** 真 E2E | 用 `testNodeClient` + fake `RoundTripper` 驱动**真实** `callOpenCodeAPI`；含变异 teeth 测试 | `zz_freetier_confirm_test.go` |
| **C3** 复活契约（a′） | `gate_state ∈ {"", ok, rejected}`；`markFreeTierRejected` 置 rejected；`sweepExpiredLocked` 在 `gateReviveRequiresProbe && rejected` 时跳过；`unmark` 清 gate_state | `proxy_node.go` |
| **I1** 删守卫 B | 新增块(3) **没有** `!versionRefreshed` 条件 | `main.go` |
| **I2** 版本来源 | builder 收 `ver` 参数并直接用它；`ocClientVersion()` 持锁读；**未消除**既有 `ocClientVer` 无锁读 race（按 v2 §5.2 不扩范围），但「实际发出的版本」现在由请求自己携带 | `main.go:2517+` |
| **I3** mark 位置 | 两处都在 2xx 分支的 `return` **正前方**（非流式 `return b,…` / 流式 `return newToolNameRewriter(…)`） | `main.go` |
| **I4** 流式 header 级 | 明确注释「流式侧成功 = 响应头级确认，0.67% 取舍见 v2 §6.2」，选定备选 0（接受） | `main.go` 流式成功路径 |
| **I5** 预算 | `proxy_node.go` 常量 `freeTierSwitchBudget = 1` | `proxy_node.go` |
| **I6** 空转可观测 | 每次 attempt 取到出口后，若 `nodeFp == pendingBadFp` ⇒ `freetier_switch_noop` + Warn | `main.go` 两处 |
| **I7** 因果链 | 注释按 v2 §8.2 的准确版本（池空只回退直连；只有直连吃 429 才关闸） | 无代码改动，靠 `quota_halt.go` 既有逻辑 |
| **M2** 文案变体 | 新增 `classifyRegionRestriction` 早退：FreeTierError ⇒ 不是区域限制 | `quota.go` |
| **M4** 两处都改 | 测试 `TestFT_BothLoopsPatched` 断言关键符号各出现 2 次且 mark 在 2xx 块内 | 测试 |

## 3. 测试结果（原文）

```
=== RUN   TestFT_FormRegression_NoMark
    OK 形态回归：status=403 节点零变更 事件=[switch upstream_error]
--- PASS
=== RUN   TestFT_Mutant_FailurePathMarkDetected
    OK teeth：检测器报警 n-A {available …} -> {exhausted, LastError:mutant:failure_path_mark}
--- PASS
=== RUN   TestFT_AltNodeSucceeds_MarksBadNode
    OK 确认侧：status=200 n-A=exhausted/rejected 事件=[switch connect_ok freetier_node_confirmed_bad]
--- PASS
=== RUN   TestFT_VersionChangedBetweenAttempts_NoMark
    OK C1：status=200 n-A 未被标记 事件=[switch switch connect_ok freetier_confirm_rejected_version_changed]
--- PASS
=== RUN   TestFT_SweepSkipsGateRejected        OK 开关关→复活，开关开→保持 exhausted  --- PASS
=== RUN   TestFT_UnmarkClearsGateState          OK unmark 清 gate_state                  --- PASS
=== RUN   TestFT_ClassifyQuotaIgnoresFreeTierError                                        --- PASS
=== RUN   TestFT_RegionClassifierNotHijacked                                             --- PASS
=== RUN   TestFT_BothLoopsPatched                                                        --- PASS
ok  github.com/shiyanYX/opencode2api  0.010s
```

- 全量 `go test .`（临时移开基线坏文件 `task8_bucket_query_test.go`）：**ok 35.463s**，跑完按 sha256 还原（`fadbdb3e…` 一致）。
- `go test -race -run 'TestFT_' .`：**ok 1.044s**（*注：首次跑时基线坏文件被失败的队友移走未还原，已从 `/tmp/t6/…bak` 按 sha256 恢复*）。
- `go build ./...`：exit 0。

## 4. 与 v2 的偏差（全部显式列出）

1. **`gate_revive_requires_probe` 用包级 var，不是配置项。** 理由：新增配置要同步 struct/校验/持久化/面板表单（项目既有先例：`var dialTimeout` 就是单点覆盖而非配置项）。T3 上线时把它置 `true` 即可。
2. **`shapeSuspectNow` / `freeTierFloor` 是钩子变量**，默认「不抑制 / floor=0」。T3 未实现，无法真正接线；默认值保证 T1 单独上线不会误伤（形态回归仍然零 mark，因为「另一节点成功」这个条件已经天然挡住）。
3. **抑制事件合并为一个** `freetier_confirm_suppressed`（detail 带 `suppressed:shape_latch|pool_floor`），而不是 v2 里的两个独立事件名——按理由分流比按名字分流更好 grep。
4. **额外修了 `classifyRegionRestriction`**：v2 §9-M2 要求"加文案变体单测"，而单测直接暴露 FreeTierError 文案会被误判成区域限制（`available in OpenCode` 命中 `available in`）。这不是"顺手改"，是该单测通过的前提。
5. **`TestFT_BothLoopsPatched` 用符号计数 + 2xx 块包含关系**，不是 v2 §M4 的完整大括号配对解析（同一意图，实现更简、更稳）。

## 5. 已知残留风险

| # | 风险 | 状态 |
|---|---|---|
| U3 | 两个不同 fingerprint 可能共用同一出口 IP（`proxy_state.json` 无地址字段）⇒「另一节点成功」≠「另一出口成功」 | **未消除**（v2 已标注为 A1 唯一的原理性缺口） |
| — | **形态回归现在每个请求多 1 次上游尝试**（预算 1：A 403 → 换 B → 仍 403 → 停）。次数从 1 变 2，但**零标记** | 设计使然，需在观测里盯住请求量 |
| — | `ocClientVer` 既有无锁读 race **只收窄未消除** | 按 v2 §5.2 不扩范围 |
| — | **`gate_state` 重启后不会读回**：`loadState()` 在包级初始化时对**空池**执行（`proxy_node.go:263` → `newProxyPool` 里 `p.nodes` 还是空的），`proxy_state.json` 实际**只写不读**。这是**既有缺陷**（不影响本次，但 C3 契约的"持久"前提因此不成立） | 已在 T7-I4 确认；建议后续单独修（把状态应用移到 `setNodes` 之后） |
| — | 流式 header 级确认的 0.67% 假阳性 | 接受（v2 §6.2 备选 0） |

## 6. 下一步

任务书要求「实现 + **独立验收**」。本实现由 Lead 完成，因此必须由**未参与实现**的队友做 T7 验收：
变异测试（把 `pendingVer` 守卫改坏，看 E2E 是否真的会红）、并发路径走查、复活契约副作用、两处循环对称性、独立复跑命令。

## 7. T7 独立验收结论与处置（2026-10-18）

**结论：Blocker 0 → go**（`.research/T7-verify-ip1.md`，322 行）。关键确认：
- 变异 `pendingVer` 守卫 ⇒ `VersionChangedBetweenAttempts_NoMark` **真红**；
- 变异「把 mark 搬进失败路径」⇒ `FormRegression_NoMark` **真红** ⇒ 不是纸面绿；
- 夹具可信（2 次 attempt、`byID=3`、无真网络）、无死锁、`-race` 全绿、真区域限制未被误杀。

**已按 T7 的最小修法处理（本轮）**：

| T7 项 | 处置 |
|---|---|
| **I3** gate_state 陈旧泄漏（flip 开关的前置） | ✅ 修：`mark()` 拆成 `markLocked(..., gate)`，**任何非 FreeTier 原因的 mark 都复位** `GateState=unknown`；`markFreeTierRejected` 的置位并入同一次加锁。新增测试 `NonFreeTierMarkResetsGateState` |
| **I6** 新增锁点撞上 `refreshOCSession` 持锁 5s 网络 | ✅ 修：`fetchOCVersionDirect()` 移到 `ocInitMu` 临界区**外**，先取后加锁赋值 |
| **I2** M4 位置断言无效（启发式仍绿） | ✅ 修：改成**大括号深度归属**判定（`braceDepth`），mark 必须在 2xx 守卫块内部 |
| **I5** 预算零覆盖 + ≥2 时 `pendingBadFp` 被覆盖 | ✅ 修：block(3) 只记第一个被拒节点（不覆盖）；新增断言「形态回归恰好 1 个 `switch` + 1 个 `upstream_error`」锁住预算=1 |
| **I1** C3 豁免默认关闭 ⇒ 运行时**不生效** | ⚠️ 照 v2 保留默认 false（降级），但**明确申报**：今天 `gate_state` 是**只写不读**，被 mark 的节点仍是 5h 后无条件复活，等价 v1 行为。**打开 `gateReviveRequiresProbe` 前必须先有 I3 的修复（已完成）** |
| **I4** `loadState` 是 no-op（重启丢状态） | ⚠️ 不改（既有缺陷、扩范围风险），已在上表与本节如实记录；`proxy_state.json` 目前只写不读 |
| M1/M2/M3/M4/M5 | M1 已补「真区域限制仍命中」正对照；M4 死代码已删；其余记入已知项 |

**新增测试（12 条全绿）**：`RegionClassifierStillCatchesRealRegionErrors`、`NonFreeTierMarkResetsGateState`，并强化了 `FormRegression_NoMark`（attempt 数/事件计数）与 `BothLoopsPatched`（真块归属）。

