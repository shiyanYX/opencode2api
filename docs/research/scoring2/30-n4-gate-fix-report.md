# N4 闸门修复报告（v5.8，B1–B4）

对象：`docs/superpowers/plans/2026-10-27-node-system-v5.md` §14（`7ed9b8b`）
复核来源：`docs/research/scoring2/29-n4-lead-review.md`

结论：**B1–B4 已修复，目标测试 `-race` 通过；未引入新的架构级矛盾。** 全量测试只剩既有的两个 IP4RV 漂移失败。

## B1 触发条件补失败率

`GateState` 新增 `WindowStart/Attempts/Failures`（持久化），5 分钟窗口按 attempt 计数；窗口边界清空 distinct egress 与计数。触发条件改为：

- `distinct 429 egress >= 3`
- `attempts >= 10`
- `429/attempts >= 50%`
- 仅在一个 429 attempt 上评估

`TestGateNeedsFailureRatio`：3 个 distinct 429 / 100 attempt 不触发；30 个 429 / 50 attempt 触发。`TestGateTrips` / `TestNoReburnWithin10m` 已改为 5 个 429 / 10 attempt 的合法触发样本。

## B2 闸的时间兜底

`GateState` 新增 `NextNodeProbe`。首次触发时设为 `LastTrip+10m`；`GateNodeProbeReady` 在 direct canary 长期失败时，于 10 分钟后允许一次节点探测，之后失败每 5 分钟重试。`AdmitGateProbe` 不再硬性要求 `CanaryOK`。旧状态没有该字段时，`gateLocked` 会按 `LastTrip+10m`（LastTrip 缺失则 now+10m）补齐，避免升级后旧活跃闸永久卡死。

`TestGateEscapeAfter10m`：触发后前进 10 分钟，节点探测可用；节点 2xx 解除暂停。

## B3 探测失败不冻结状态机

`Attempt` 新增内部标记 `gateProbe`。`AdmitGateProbe` 置位；`observeGateOutcomeLocked` 对探测的任何非 2xx（429/403/5xx/网络/超时/取消/other）统一：

- `CanaryOK=false`
- `NodeProbeUsed=false`
- `NextCanary=now+60s`
- 时间兜底探测排到 `now+5m`

`TestGateProbeFailureDoesNotFreeze`：direct canary 2xx → 节点探测 403 → 60 秒后 canary 仍可发起，随后 canary 2xx 重新打开节点探测。

同时修正 `AdmitGateProbe` 的注释：选路使用 `probe=true`（只用于忽略 pair 冷却），但 `Attempt` 实际标记 `probe=false`，429 仍走正常 pair 阶梯。

## B4 日志节流与 NoCandidate 语义

- `v5Winter` 与 `v5NoCandidate` 共用 `v5GateLog`，按 `type+reason` 每分钟最多一条。
- force 下无租约（冷却耗尽/区域排除等）发 `v5_no_candidate`；仅闸暂停发 `v5_winter`。
- main 的 suppressed attempt 事件通过 `v5NoCandidateEventName()` 区分两者。
- `TestWinterLogThrottled` 验证同一 reason 两次调用只落一条 `v5_winter`；既有 force no-candidate 测试已改为断言 `v5_no_candidate`。

## 验证证据

```
go vet ./...                                                   # exit 0
go test -run 'TestV5|TestPair|TestNoReburn|TestGate|TestWinter|TestNoCandidate|TestMaxTwo' -race -count=1 .
ok   github.com/shiyanYX/opencode2api  1.148s
```

全量最终复跑（含本次修改）：

```
go vet ./...                                                   # exit 0
go test -count=1 ./...
FAIL github.com/shiyanYX/opencode2api  305.996s
--- FAIL: TestIP4RVLiveServedDrift
--- FAIL: TestIP4RVDocsDrift
```

这两个失败是既有 IP4RV 漂移检查，与本次闸门改动无关；使用 `zz_ip4v_layout_new.go` 工作区未跟踪文件时同样存在。

## 变异检查

| 变异 | 结果 |
| --- | --- |
| B1 `gateDistinct=99` | `TestGateNeedsFailureRatio` / `TestGateTrips` 红 |
| B2 `gateEscapeDelay=1000h` | `TestGateEscapeAfter10m` 红 |
| B3 仅 429 才算探测失败 | `TestGateProbeFailureDoesNotFreeze` 红（`failed probe froze canary entrance`） |
| B4 关闭 `v5GateLog` 节流 | `TestWinterLogThrottled` 红（2 条，期望 1 条） |

B3 的变异同时移除 `at.gateProbe` 与 `CanaryOK` 两条非 2xx 重置分支；只移除前者时，canary 路径仍会被通用非 2xx 分支修复，因此测试保持绿是符合实现的。

## 改动文件

- `admission.go`：GateState 窗口/时间兜底、`gateProbe`、B3 状态迁移、注释修正
- `node_state_v5.go`：GateState 持久化字段
- `node_system_v5.go`：`v5_no_candidate`、按 reason 节流、事件名选择
- `main.go`：suppressed attempt 的 `v5_winter` / `v5_no_candidate` 区分
- `n4_test.go`、`node_system_v5_force_test.go`：B1–B4 与既有测试更新

## 最终全量

`/tmp/n4fix-final-full.log`：`go vet ./...` exit 0；`go test -count=1 ./...` 305.996s，仅 `TestIP4RVLiveServedDrift` / `TestIP4RVDocsDrift` 两个既有失败。
