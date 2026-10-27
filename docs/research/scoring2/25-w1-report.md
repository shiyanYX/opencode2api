# W1 影子接线报告（只观测，不改选路）

范围：新增 `node_system_v5.go`（运行时/分类/影子事件/Stats）、`node_system_v5_test.go`；`main.go` 的 4 个上游循环各插入一个 attempt 级 `v5Observe` 闭包及 3 个互斥分支调用点（2xx / transport error / 非 2xx），并在 `statsRangeResponse` 增加 `v5` 字段；`calllog.go` 的 `CallEvent` 增加 5 个 v5 字段。未改 `egress.Get`、`proxy_node.go` 或 `admission.go`。默认 `NODE_SYSTEM` 未设置 = `off`。

## 1. 运行时与分类

- `v5Mode`：`off / shadow / force`；`v5Enabled()` 每次读 `NODE_SYSTEM`，未知值 WARN 后按 off。
- 懒初始化 `v5Runtime`：`NewNodeStateV5(now)`、`NewAdmission(state, 8, v5Candidates)`、`shadowOnly=true`；首版只实现 shadow 行为。
- `classifyV5(status, errBody, err, attemptVer, pinnedVer)` 为纯函数：
  - `context.Canceled` → `cancel`；`context.DeadlineExceeded` 或 `net.Error.Timeout()` 或首字节超时文案 → `timeout`；其余 err → `network`。
  - 2xx → `2xx`，5xx → `5xx`。
  - 403 仅 `isFreeTierErrorBody && attemptVer == pinnedVer` → `403`；版本不一致、区域限制、426、普通 429、400 等 → `other`。
  - `classifyQuota` 为真（仅配额类 429）→ `429`。
- `other` 只落 `v5_outcome`，不调用 `Observe`；不污染 pair 后验。

## 2. `main.go` 接线

两个函数（`callOpenCodeAPI`、`callOpenCodeAPIStream`）各在 attempt 作用域创建：

```go
v5Seen := false
v5Observe := func(status int, body []byte, err error) {
    if v5Seen { return }
    v5Seen = true
    v5ObserveUpstream(ctx, modelID, egResult, status, body, err, attemptVer)
}
```

调用点（每个循环 3 个互斥分支，闭包保证一次）：

1. `err != nil`，在 `noteAttemptTransportError` 之后：`v5Observe(0, nil, err)`。
2. 2xx 分支，在 `recordNodeSuccess` 之后：`v5Observe(resp.StatusCode, nil, nil)`。
3. 非 2xx，读完 `errBody`、`logUpstreamError` 之后：`v5Observe(resp.StatusCode, errBody, nil)`。

`egResult` 已包含 BunTransport 处理后的实际出口；`attemptVer` 是本次 attempt 实际携带的 UA 版本，避免 403 回退被错记。

## 3. 影子行为与 Stats

- `v5ObserveUpstream` 在 shadow 下：
  1. `adm.Predict(modelID, nil)`：与 `Admit` 同样的冷启动/UCB/衰减口径，但纯读（`v5PairStats` 不写 `LastDecay`/A/B），不取信号量、不推进 `rr`/dead。
  2. `actual := eg.EgressKey()`，统计 agree/disagree；不一致记 `predicted`/`actual`/`reason`。
  3. `adm.ObserveVirtual(...)`：构造无 sem 的 `Attempt` 复用 `Observe`。
  4. `v5_shadow` 只在分歧或每 100 次时落；`v5_outcome` 每条 attempt 落。
- `v5Candidates()` 用 `proxyPool.mu.RLock` 只读快照（不 sweep）：每个有 client 的节点 `node:<fp>` + 一个 `direct` 候选（影子不发 direct）。
- `/api/stats?range=` 新增 `v5` 对象：`mode/since/requests/agree/disagree/diverge_rate/by_class/cooling_pairs/quarantined_subs`。无请求时 `diverge_rate` 和两个计数为 JSON `null`，不是 0；off 时整个字段省略。

## 4. 测试与证据

新增 6 个测试，`classifyV5` 表驱动 16 个子用例：

- `TestV5ClassifyTable`
- `TestV5ShadowDivergence`
- `TestV5PredictNoSideEffect`
- `TestV5StatsNullWhenNoData`
- `TestV5ModeOffDoesNothing`
- `TestV5CandidateBuild`

```bash
GOMODCACHE=$PWD/.modcache GOPROXY=off go test -race -run 'TestV5' -count=1 .
# ok github.com/shiyanYX/opencode2api 1.046s
```

影响面回归（带 range 的 stats、无 range 的逐字节兼容）：

```bash
GOMODCACHE=$PWD/.modcache GOPROXY=off go test -run \
  'TestV5|TestAdminStatsHandlerWithoutRangeIsByteIdentical|TestAdminStatsHandlerWithRangeReturnsWindowedStats' \
  -count=1 .
# ok github.com/shiyanYX/opencode2api 0.027s
```

`go vet .` rc=0；`gofmt -l node_system_v5.go node_system_v5_test.go main.go calllog.go` 无输出；`git diff --check` 无输出。

### 变异（最终代码上重跑，全部红）

| 变异 | 测试 | 结果 |
| --- | --- | --- |
| 403 忽略 `attemptVer == pinnedVer` | TestV5ClassifyTable/403_version_mismatch | FAIL |
| 无样本时 `diverge_rate` 写 0 而非 null | TestV5StatsNullWhenNoData | FAIL |
| 纯候选构造器丢弃 direct | TestV5CandidateBuild | FAIL |
| 影子 `agree` 恒为 true | TestV5ShadowDivergence | FAIL |

## 5. 全量测试与既有失败

```bash
GOMODCACHE=$PWD/.modcache GOPROXY=off go test -count=1 .
# FAIL github.com/shiyanYX/opencode2api 165.596s
```

全量只报 3 个失败：

- `TestIP4RVLiveServedDrift`、`TestIP4RVDocsDrift`：既有 R-A/R-B 口径问题（`Σattempts` 与 `totals` 不一致）。临时撤掉 `main.go` 的 W1 接线后仍同样失败，确认不是 W1 引入。
- `TestProbationWithoutEvidenceExpiresBackToCooldown`：临时目录 `RemoveAll` 清理竞态；单独 `-count=3` 通过（3/3），非 W1 行为失败。

W1 新增测试与受影响 stats 测试均绿；off 默认下没有 `v5` 事件/字段。

## 6. 未覆盖/已知边界

- 没有真实上游的端到端测试；`main.go` 接线正确性由编译、分支审查和闭包单次保护保证，未有 fake-upstream 集成测试。
- `v5Candidates` 的 direct 候选会让影子在“节点从未实际走 direct”时长期预测 direct（direct 保持冷启动、按 egress 字典序排前），因此影子分歧率早期偏高；这不影响真实选路，但不能把 W1 的 `diverge_rate` 当作生产准确率。
- `force` 目前只是模式名，行为仍是 shadow；真实强制切换留给后续切片。
- `v5` Stats 是进程内计数，不落盘、不跨重启。
