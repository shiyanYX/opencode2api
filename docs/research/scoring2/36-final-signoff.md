# 最终复验：`4f670f9`（P0 租约回滚）

## 结论

**PASS**。原 P0「force→off/shadow 的 in-flight 租约泄漏」已关闭；8 轮切换后 `sem` 立即归零、无 `all_inflight`，切回 force 仍可正常取租约。探索名额归还与 `Observe` 的锁顺序安全，2000 轮三方交错在 `-race` 下无残留。自造变异可稳定打红。tracked-only 全量 race **0 失败、0 race**，`go vet` 通过。

**是否可 merge `main`：可以。** 前提是按本轮口径使用 tracked-only 全量测试；工作树中未跟踪的旧 `zz_*` 回放测试仍会让“原样全量”失败，与本提交无关。

## 验收对象

- HEAD：`4f670f9`（`test` 分支）
- 修复内容：
  - 新增 `releaseAttempt`，在 off/shadow/runtime=nil 等不 Observe 的路径兜底归还租约（`admission.go:43-55`）。
  - 探索名额归还从 `Observe` 移到 `release` 闭包（`admission.go:479-501`）；`releaseAttempt` 不经过 `Observe` 时也能归还。
  - `v5ObserveUpstream` 在 off/shadow 早退前调用 `releaseAttempt(eg.V5())`（`node_system_v5.go:286-299`）。
- 未修改生产代码、未提交；临时验收测试已删除。

## 逐项判定

| 项 | 判定 | 证据 |
|---|---|---|
| force 取租约 → 切 off → Observe，`sem` 立即归零 | PASS | `TestFinalRollbackInFlightLease` 8 轮，每轮 `sem len 1 -> 0` |
| force→shadow 同样归还 | PASS | 同一测试 shadow 分支，`sem len=0` |
| 连做 8 轮不出现 `all_inflight` | PASS | 8 轮全部 admit 成功；第 9 次切回 force 仍能 admit |
| 切回 force 后租约池可正常取用 | PASS | 测试末尾 `AdmitPreferred` 成功；`releaseAttempt` 归还 |
| `Observe` 的 `once.Do` 在取锁前，无自锁 | PASS | `admission.go:322-333` 先 `at.once.Do(release)`，之后才 `a.mu.Lock()`；release 闭包取 `a.mu` |
| 2000 次 `Admit(explore)+Observe+releaseAttempt` 交错 | PASS | `TestSignoffExploreReleaseInterleave`，`-race`，0.09s；每轮 `exploreInflight=0`、`sem=0`，无负数 |
| explore 冷却/名额回归 | PASS | `TestColdStartPrefersFewestAttempts`、`TestColdStartSingleInflight`、`TestUCBExploresLowConfidence`、`TestV5DropAttemptClearsExploreInflight`、`TestV5CandidatesColdStart` 全 PASS |
| 自造变异：删除 release 中的 explore 归还 | PASS（测试变红） | 变异体 `round 0 exploreInflight residue=1`，0.024s FAIL |
| tracked-only 全量 `go test -race -count=1 ./...` | PASS | 354.949s，0 失败，0 race 报告 |
| `go vet ./...` | PASS | exit 0 |

## 关键命令与输出

### 1. 原始 P0 测试（8 轮 off + shadow）

```text
go test -race -count=1 -run '^TestFinalRollbackInFlightLease$' -v .
--- PASS: TestFinalRollbackInFlightLease (0.00s)
PASS
ok github.com/shiyanYX/opencode2api 1.038s
```

该测试在每轮 force `/` off `/` force 之间检查 `len(rt.adm.sems["node:e1"])`：
`1 -> 0`；第 8 轮结束后再 admit 成功；随后单独走 force→shadow，`sem len=0`。

### 2. 探索名额并发（2000 轮）

```text
go test -race -count=1 -run '^TestSignoffExploreReleaseInterleave$' -v .
--- PASS: TestSignoffExploreReleaseInterleave (0.09s)
PASS
ok github.com/shiyanYX/opencode2api 1.116s
```

每轮：
1. `AdmitPreferred` 产生 `explore=true` 的 attempt；
2. 3 个 goroutine 并发 `Observe(2xx)`、`releaseAttempt(at)`、`releaseAttempt(at)`；
3. 断言 `sems[cur]` 长度 0，`exploreInflight[cur]==0`。

锁顺序确认：`Observe` 的 `at.once.Do` 在 `a.mu.Lock()` 之前；release 闭包内部才取 `a.mu`。因此不会自锁，且 `once` 保证探索名额只归还一次。

### 3. 回归

```text
go test -race -count=1 -run '^(TestV5DropAttemptClearsExploreInflight|TestColdStartPrefersFewestAttempts|TestColdStartSingleInflight|TestUCBExploresLowConfidence|TestV5CandidatesColdStart|TestFinalRollbackInFlightLease|TestSignoffExploreReleaseInterleave)$' -v .
PASS
ok github.com/shiyanYX/opencode2api 1.122s
```

### 4. 变异

工作树：`/tmp/signoffmut`，基线 `4f670f9`，复制审计测试。

- 基线：`TestSignoffExploreReleaseInterleave` PASS。
- 变异：删除 `release` 闭包中的 `if explore { ... exploreInflight-- ... }`。
- 结果：`--- FAIL: TestSignoffExploreReleaseInterleave`，`round 0 exploreInflight residue=1`，`FAIL`，`0.024s`。
- 说明：该变异只用于验证并发审计的灵敏度；工作树随后丢弃，未合并。

### 5. tracked-only 全量 race

将全部未跟踪 `zz_*_test.go` 移到 `/tmp/signoff_hold`，运行后还原：

```text
go test -race -count=1 ./...
ok github.com/shiyanYX/opencode2api 354.949s
EXIT:0
```

0 失败，0 数据竞争报告。`go vet ./...` exit 0。

## 残余说明

1. **原样全量仍会被未跟踪 `zz_*` 拖垮**：`zz_t23_verify_test.go`、`zz_ip4agg_reverify_test.go`、`zz_t39_served_models_test.go` 等历史回放测试在 race 下超时/失败；本轮按 lead 要求采用 tracked-only，不能把原样输出当作本提交失败。
2. 新 release 闭包在释放 `sem` 后取 `a.mu` 递减 explore；依赖 `Observe` 在锁外执行 `once`。当前实现和 2000 轮交错已证明安全；后续若有人把 `once.Do` 移进 `a.mu` 临界区，会重新引入自锁，建议加注释或单测锁定顺序。
3. 本轮是窄范围 P0 复验；未重新评估 35 号报告中的面板冻结、冷启动、旧 observer 写入和 CPU/内存风险。它们不阻塞本次 merge，但部署时仍应按 35 号报告观察。
4. 8 轮测试使用单 egress/单 pair；真实 212 节点下 `releaseAttempt` 路径相同，但压测规模未覆盖全池。

## Merge 结论

**PASS，可 merge `main`。** P0 已关闭，探索名额无泄漏，tracked-only 全量 race 与 vet 全绿；唯一的失败来源是工作树中未跟踪的历史 `zz_*` 回放测试，不属于本提交。
