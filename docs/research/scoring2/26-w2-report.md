# W2 force 接管报告

- 分支：`test`
- 基线：`4d62dc3`（W1 shadow wiring）
- 结论：**force 路径已实现并通过定向测试/竞态/变异检查**；完整 `go test ./...` 仍被 3 个既有非 W2 测试拖红，见 §5。
- 范围：只改选路与观测接线；没有改 `proxy_node` 生命周期、`admission` 算法、`main` 的旧重试/计费逻辑。

## 1. 实现

### 1.1 force 的唯一选路入口

`egress.Get` 在付费直连快速路径之后、节点池旧选路之前加入：

```go
if v5Enabled() == v5Force {
    at, ok := v5AdmitContext(ctx, r.Model, r.RequiredRegion, r.ExcludeEgress)
    ...
}
```

- `EgressRequest` 新增 `Model string`、`Context context.Context`；生产只有 2 个 `egress.Get` 调用点（`main.go:2965` 附近、`main.go:3496` 附近），均已传 `Model:modelID, Context:ctx`。
- `EgressResult` 新增 `v5 *Attempt` 与 `V5() *Attempt`；`bunEgressResult` 复制该字段，避免 free-tier Bun 包装后丢失租约导致 semaphore 泄漏。
- `v5AdmitContext`（`v5Admit` 是 3 参兼容包装）：
  - 通过 `v5Candidates(model)` 构造候选；
  - 按 `RequiredRegion` 过滤（同时排除 direct）；
  - prefer 顺序为 `proxyPool.manualFp()` → `proxyPool.currentFp()`，转成 `node:<fp>`；
  - 调 `Admission.AdmitPreferred`（非阻塞取名额）。
- 节点 `getClient` 为空或候选失败时，`v5DropAttempt` 归还租约并回退真直连；无租约请求仍能完成（`v5_fallback` 事件，同一 reason 每分钟最多 1 条）。

### 1.2 观测

`v5ObserveUpstream` force 分支：

1. `eg.V5()` 非 nil 时调用 `adm.Observe(at, class)`（内部 `sync.Once` 释放名额）；
2. 更新 `requests`/`by_class`；
3. `class != "other"` 落 `v5_outcome`；
4. `other` 只释放、不学习，避免区域/版本回退污染 pair 后验。

`Attempt` 的释放只经过 `Observe` 或 `v5DropAttempt` 的 `sync.Once`，正常 attempt 恰好一次。

### 1.3 旧闸门旁路

force 下由 `v5LegacyGatesBypassed()` 旁路：

- `quotaHaltGuard.gate`（非流式 + 流式）；
- 所有 `proxyPool.poolHalted` 检查（循环前/`Get` 后/403 后）；
- 区域预检查、`shapeDriftBlockedEgress` 及其后置检查；
- `trialSwitchPending`/`shapeDriftSetTrialEgress`；
- `allSubsQuarantined`；
- `shapeDriftObserve429` 整块（不更新旧状态、不写旧事件、不 Halt）；
- direct 429 的旧 `quotaHaltGuard.halt`。

旧 `pool403`/`sub` 的 **记账** 未被删除；force 只是不再让它们否决新选路，避免它们与新 pair 状态双写。

### 1.4 健康探测

`proxyPool.checkNodes` 不走 `egress.Get`，因此不存在“探测污染 v5 租约”的路径。`v5Candidates` 只读快照，不 sweep、不改变生命周期。

## 2. 新增测试

`node_system_v5_force_test.go`（9 个）：

- `TestV5ForceBypassHelper`
- `TestV5ForceGetUsesAdmission`
- `TestV5ForceNoCandidateFallsBackDirect`
- `TestV5ForceObserveReleasesOnce`
- `TestV5ForceRegionFilter`
- `TestV5ForceOtherClassReleases`
- `TestV5ForceManualPreferred`
- `TestV5ForceStatsShowsMode`
- `TestV5ForceBunPreservesLease`

覆盖：mode 判定、`Get` 委托、无候选 fallback + 事件限流、`Observe` 单次释放、region 排除、`other` 释放、manual 优先、stats mode、Bun 包装保留租约。

## 3. 验证命令与真实结果

```bash
GOMODCACHE=$PWD/.modcache GOPROXY=off go test -run 'TestV5' -race -count=1 .
# ok github.com/shiyanYX/opencode2api 1.047s

GOMODCACHE=$PWD/.modcache GOPROXY=off go test -run 'TestV5Force' -count=1 .
# ok github.com/shiyanYX/opencode2api 0.011s

GOMODCACHE=$PWD/.modcache GOPROXY=off go vet ./...
# (no output)

GOMODCACHE=$PWD/.modcache GOPROXY=off go test -count=1 ./...
# FAIL 179.450s；仅 3 个既有失败，见 §5
```

## 4. 变异检查

逐条改生产代码、跑对应测试、恢复；结果：

| 变异 | 目标 | 结果 |
|---|---|---|
| `v5LegacyGatesBypassed` 恒 false | 旧闸门旁路判定 | KILLED（`TestV5ForceBypassHelper`） |
| force 分支不调用 `Observe` | 租约释放/学习 | KILLED（`TestV5ForceObserveReleasesOnce`） |
| `Get` 的 force 分支恒 false | force 选路入口 | KILLED（`TestV5ForceGetUsesAdmission`） |
| `bunEgressResult` 丢弃 `v5` | Bun 包装租约 | KILLED（`TestV5ForceBunPreservesLease`） |

Bun 变异第一次“存活”是因为测试用了 `&http.Client{}`（无 Transport，`bunClientFor` 直接返回 nil，没走到复制分支）；把测试改为 `&http.Client{Transport:&http.Transport{}}` 后同一变异变红。这个测试修正本身也说明：Bun 包装路径必须用真实 `*http.Transport` 才覆盖。

## 5. 完整套件与既有失败

`/tmp/w2-off-full.log` 失败集合：

- `TestExhaustedExpiryEntersProbationNotAvailable`（`proxy_node` 既有语义，非 W2 文件）
- `TestIP4RVLiveServedDrift`（既有 R-A/R-B 统计口径）
- `TestIP4RVDocsDrift`（既有 R-A/R-B 统计口径）

W1 报告 `25-w1-report.md` 已记录：临时撤掉 W1 接线后同样失败，确认不是 W1/W2 引入。W2 没改这些路径，因此不把它们算作 W2 回归；但“off 下整个 `go test ./...` 全绿”这一验收口径仍被这 3 个存量失败挡住，需要由后续专门修 R-A/R-B 或调整门禁，不能在本切片假装全绿。

## 6. 已知边界/风险

1. **候选依赖 client**：`v5Candidates` 只收 `proxyPool.clients[fp] != nil` 的节点（W1 约束）。若 force 启动时健康探测尚未为节点建 client，候选只剩 direct，会回退直连；上线前应确认健康循环先于首批请求，或在后续切片显式懒建 client。
2. **fallback 直连不学习**：无租约的 direct send 没有 `Attempt`，不做 pair 观测，只落限流后的 `v5_fallback`；这是“不得让请求无出口”的代价。
3. **fallback 事件限流是全局 reason 维度**，不是 per-model；多模型会共享每分钟 1 条。
4. **未注入 clock**：本切片测试用真实 `time.Now`；跨 24h/5h 的 force 行为仍应留给虚拟时钟切片。
5. **stats 为进程内累计**：force 下 `diverge_rate` 为 null（没有 shadow 对比），`requests/by_class` 仍累计。

## 7. 下一批（非本切片）

- force 灰度：先 1 个出口/1 个模型，观察 `v5_outcome`、`v5_fallback`、503 比例；
- 把 `v5Candidates` 的 client 初始化竞态纳入启动顺序验收；
- 为 `bunEgressResult` 增加 free-tier 真实请求级测试（当前只测字段保留）。
