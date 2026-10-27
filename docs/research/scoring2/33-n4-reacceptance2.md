# N4 复验收 2（33，F4/F5）

- 对象：`9b220f1` 之后的 F4/F5 修复
- 结论：**F4、F5 均关闭；无新增断言失败。**

## F4：force 下旧池写入全部收口

- `nodePool.markLocked` 开头统一旁路：`v5LegacyGatesBypassed()` → `(false,false)`；
  覆盖 `mark`、`markFreeTierRejected` 以及 main 的 quota 429 `mark(NodeExhausted)`。
- `quarantineSub`、`confirmSub403` 各自在入口旁路，避免订阅隔离/403 确认在 force 下写旧状态。
- 调用方控制流未改：429 仍切节点、`quotaSwitches++`、`quota_node_switch` 日志仍在。

`TestForceLegacyWritesNoop` 覆盖 4 条路径：`mark`、`markFreeTierRejected`、
`quarantineSub`、`confirmSub403`。force 下逐节点/逐订阅深拷贝前后一致，
`poolHalted` 前后一致；`TestLegacyWritesOffStillApply` 证明 off 下 `mark` 仍生效。

变异：删除 `markLocked` 的 force 守卫 → `TestForceLegacyWritesNoop` FAIL（红）。

## F5：probeActive key 与 Observe 删除一致

- `Attempt` 新增 `probeKey`；`AdmitProbe` / `AdmitLifecycleProbe` 注册
  `probeActive[c.Egress]` 时同步写入 `at.probeKey`。
- `Observe` 删除 `at.probeKey`，为空才回退 `at.Egress`；`probing` 改为
  `len(probeActive)>0`，不再无条件 false。
- 空 Egress 的 Observe 仍会先完成 probe key 释放，再走 F2 的空 Egress 隔离。
- admission miss（无候选）不注册 key；正常 probe 的 `sync.Once` 仍只释放一次。

测试：

```text
TestProbeKeyReleasesMismatchedEgress PASS  # 重写 at.Egress 后无残留；空 Egress 亦无残留
TestAdmitProbeMissNoResidue PASS           # no_candidate 不注册 key
TestProbeObserveReleasesOnce PASS          # 二次 Observe 不重复释放、sem 为空
```

变异：`Observe` 改回 `delete(a.probeActive, at.Egress)` →
`TestProbeKeyReleasesMismatchedEgress` FAIL（红）。

## 验证

```text
gofmt -l admission.go n4_test.go proxy_node.go node_system_v5.go   # 无输出
git diff --check                                                   # 无输出
go vet ./...                                                       # exit 0
go test -run 'TestV5|TestGate|TestPair|TestNoReburn|TestMaxTwo|TestWinter|TestNoCandidate|TestForce|TestEmptyEgress|TestProbe' -race -count=1 .
ok github.com/shiyanYX/opencode2api 1.433s

go test -count=1 ./...
FAIL github.com/shiyanYX/opencode2api 194.004s
--- FAIL: TestIP4RVLiveServedDrift
--- FAIL: TestIP4RVDocsDrift
```

- 两个 IP4RV 失败仍来自未跟踪的 `zz_ip4agg_reverify_test.go`，与本次改动无关。
- 上一轮偶发的 `TestExhaustedExpiryEntersProbationNotAvailable` cleanup flake 本轮未出现。
- 无新增断言失败。

## 改动

- `admission.go`：`probeKey` 注册/删除、`probing` 一致性
- `proxy_node.go`：`markLocked` / `quarantineSub` / `confirmSub403` force 旁路
- `n4_test.go`：F4 4 路径深度快照、off 回归、3 个 probe 测试

提交仅到 `test` 分支。
