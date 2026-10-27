# N4 复验收 3（34，F6）

- 对象：`23f2da7` 之后的 F6 收尾
- 结论：**F6 关闭；force 下确认侧零事件，off 回归正常。**

## F6：force 整块跳过 free-tier 确认侧

`main.go` 两处 2xx 成功分支（非流式 `:3136`、流式 `:3668`）的
`if pendingBadFp != ""` 改为：

```go
if !v5LegacyGatesBypassed() && pendingBadFp != "" {
```

force 下整块跳过：`freeTierConfirmAllowed`、`markFreeTierRejected`、
`confirmSub403`、`freetier_confirm_rejected_version_changed`、
`freetier_node_confirmed_bad`、`freetier_confirm_suppressed` 及两条 Warn 日志。
失败路径仍照常切节点、计 `freeTierSwitches`、写 `freetier:probe_alt_node` switch 事件。

行为已由 `TestF6ForceSkipsFreeTierConfirm` 钉住：

- force 下连续 20 次「n-B 403 → n-A 200」：每次 200；call_log 中
  `freetier_confirm_*` 与 `freetier_node_confirmed_bad` 均为 0。
- 20 次前后节点 `State/MarkedAt/Cooldown/LastError/GateState` 逐字段一致，
  `sub403` 为空，`poolHalted` 不变。
- `TestF6OffStillConfirms`：off 下同场景仍产生 `freetier_node_confirmed_bad`，
  n-A 被标为 `NodeExhausted`。

变异：去掉两处外层 `!v5LegacyGatesBypassed()` 守卫，
`TestF6ForceSkipsFreeTierConfirm` FAIL（首轮即出现 `freetier_confirm_suppressed`）。

## 验证

```text
gofmt -l main.go n4_test.go admission.go proxy_node.go node_system_v5.go  # 无输出
git diff --check                                                          # 无输出
go vet ./...                                                              # exit 0
go test -run 'TestV5|TestGate|TestPair|TestNoReburn|TestMaxTwo|TestWinter|TestNoCandidate|TestForce|TestEmptyEgress|TestProbe|TestF6' -race -count=1 .
ok github.com/shiyanYX/opencode2api 1.543s

go test -count=1 ./...
FAIL github.com/shiyanYX/opencode2api 220.873s
--- FAIL: TestIP4RVLiveServedDrift
--- FAIL: TestIP4RVDocsDrift
```

- 两个 IP4RV 失败仍来自未跟踪的 `zz_ip4agg_reverify_test.go`，与本次改动无关。
- 无新增断言失败。

## 改动

- `main.go`：两处确认侧外层 force 守卫
- `n4_test.go`：F6 force 20 轮无事件/状态不变、off 回归

提交仅到 `test` 分支。
