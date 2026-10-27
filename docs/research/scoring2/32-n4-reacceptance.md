# N4 复验收（32，F1–F3 后续）

- 对象：`9b3e8d0` 之后的 F2/F3 修复（本轮）
- 前置：`docs/research/scoring2/31-n4-acceptance.md` §12
- 结论：**两个 blocker 已修复；dead 候选语义按设计保留并加测试钉死。**

## 修 1：空 Egress 完全不进入窗口与 pair 账本

- `Admission.Observe` 在 pair 写入和 gate 观测之前直接返回 `at.Egress == ""`。
- `observeGateOutcomeLocked` 同样在窗口重置前返回，作为直接调用该函数的防线。
- `pairLocked` / `writePairLocked` 对 `egress == ""` 返回 nil，任何 `PairKey(model,"")` 或 `PairKey("*","")` 的读写都不会被创建/命中。

证据：

```text
TestEmptyEgressDoesNotTouchGateOrPairs PASS
  - 3 次空 Egress 后 WindowStart=0 / Attempts=0 / Failures=0 / Egresses 空
  - Pairs 无 model| 或 *| 键
  - 真实 429 建立窗口后，空 Egress 不改变 WindowStart/Attempts/Failures
```

变异：把空 Egress 的 return 挪到 `observeGateOutcomeLocked` 之后（窗口重置后），
`TestEmptyEgressDoesNotTouchGateOrPairs` FAIL。

## 修 2：force 下旧池 noteAttempt/note403 不迁移状态

- `nodePool.noteAttempt` 和 `note403` 在 `v5LegacyGatesBypassed()` 时直接返回。
- `pool.mark` 的 force 旁路沿用 `noteAttemptTransportError` 的 `v5LegacyGatesBypassed()` 守卫。
- 纯统计计数在 force 下也不写；旧池状态保持进入 force 前的快照。

证据：

```text
TestForceNote403DoesNotHaltLegacyPool PASS
  - 8 个节点 noteAttempt + note403 后 poolHalted=false
  - 退出 force 后节点 State 与进入前一致
TestForceTransportErrorDoesNotMarkLegacyState PASS
```

变异：移除 `note403` 的 `v5LegacyGatesBypassed()` 条件，测试 FAIL（poolHalted 被置位）。

## 不修：dead 节点仍进 v5 候选（语义测试）

`v5Candidates` 注释已写明：不按旧池 `State/Dead/Cooldown` 过滤；force 下旧 dead 是陈旧
信号，隔离是设计要求。

```text
TestForceIgnoresLegacyDead PASS
  - 旧池 mark(NodeDead) + 删除 client 后，v5Candidates 仍包含该节点
```

变异：在 `v5Candidates` 增加 `n.State == NodeDead` 过滤，测试 FAIL；这证明测试确实钉住了
“不读旧 dead 字段”的语义。

## 验证

```text
gofmt -l admission.go main.go n4_test.go node_system_v5.go proxy_node.go  # 无输出
git diff --check                                                          # 无输出
go vet ./...                                                              # exit 0
go test -run 'TestV5|TestGate|TestPair|TestNoReburn|TestMaxTwo|TestWinter|TestNoCandidate|TestForce|TestEmptyEgress' -race -count=1 .
ok github.com/shiyanYX/opencode2api 1.164s

go test -count=1 ./...
FAIL github.com/shiyanYX/opencode2api 217.303s
--- FAIL: TestExhaustedExpiryEntersProbationNotAvailable
--- FAIL: TestIP4RVLiveServedDrift
--- FAIL: TestIP4RVDocsDrift
```

- `TestExhaustedExpiryEntersProbationNotAvailable` 的失败原文是
  `TempDir RemoveAll cleanup: ... directory not empty`；单独重跑 `ok 0.008s`，是既有异步
  状态保存与 TempDir 清理竞争。
- 两个 IP4RV 失败来自未跟踪的 `zz_ip4agg_reverify_test.go`，与本次改动无关，单独重跑仍复现。
- 本轮没有新增断言失败。

## 改动

- `admission.go`：空 Egress 在 Observe/gate/pair 三处隔离
- `proxy_node.go`：noteAttempt/note403 force 旁路
- `node_system_v5.go`：候选枚举注释明确不读旧状态
- `n4_test.go`：3 个新验收/语义测试

提交仅到 `test` 分支。
