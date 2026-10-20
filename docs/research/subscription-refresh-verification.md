# 订阅刷新假告警 + 24h 自动刷新永不触发：修复独立验收（task-60 / 实现 task-59）

> 验收方 `healthcheck-probe`。实现方 `impl-ip1`。**不采信其结论与测试，全部自己复跑。**
> 报告按步骤增量落盘（防丢）。

## 0. 结论

**`go`（可验收通过）** —— 两条缺陷均代码级成立并独立复现（HEAD 干净副本：假 WARN `error="没有任何可用节点: "`、
`lastCount` 215→0、`lastUpdated` 被每轮冲刷 ⇒ 永远不到期）；修复后 6 条断言全 PASS（含 **24h 到期真的会拉取**、
`/api/nodes` 每源 nodes=真实值）；5/5 自造变异 CAUGHT；全量 183.8s / `-race` / vet / gofmt 全绿；
只动 `subscription.go`+`subscription_runtime.go`（`main.go`+8 / `calllog.go` 属 task-56 另一条线）。
生产只读佐证：旧版每 10 分钟一条假 WARN、5 个源 `nodes` 全 0 且 `last_updated_at` = 最后 tick 时刻，而池里有 217 个节点。

## 1. 被测对象与钉桩

| | 值 | 状态 |
|---|---|---|
| `subscription.go` sha256 | `902773432ae8512cf54113a1d1075924edce6affaa314c6c2e55a20f281bab92` | ✅ |
| `subscription_runtime.go` sha256 | `af3b0a46ee0701d127654643cad08a4a750aa506ac7c93d1d29a7e86b5d5c96f` | ✅ |
| `subscription_parse.go` sha256 | `cf392f0e27af214f5cd41085d08cc31165020d7b2bf9058db43bca67acc43a43`（未改动） | ✅ |
| `git diff --numstat -- subscription*.go` | `subscription.go 84/19`、`subscription_runtime.go 24/6`（合计 108/25）；`subscription_parse.go` 未改 | ✅ |
| 基线 | `git archive HEAD` 干净副本（不动工作树） | ✅ |
| 分支/路径 | `test` / `/home/jingyx/project/opencode2api-test` | 未 commit / 未 git |

**边界**：不改任何生产代码（`subscription*.go`/`main.go`/`calllog.go`/`statscan.go`/`proxy_node.go`/`node_alert.go`/`static/admin.html`）；只写本报告 + 自己的 `zz_verify_t60_*.go`；基线/变异在 `/tmp/t60/` 副本；不碰生产容器（只读日志/接口）、不用 8000、不 git。
**并发注意**：`calllog.go` 正被 task-56 改（工作树 `git diff` 有它），本次核对 `go build` 通过；与订阅线无关，后文 §6 会区分归属。

## 2. 缺陷复核（代码级）—— 状态：✅ 两条都成立

**HEAD 的三段原文**（`git show HEAD:subscription.go` / `HEAD:subscription_runtime.go`）：

```go
// subscription.go:225-243（未到期源走 skip 分支，但结果被当成「成功」处理）
if prev, ok := lastUpdated[s.URL]; !force && ok && now.Sub(prev) < s.updateInterval() {
    results <- result{sub: s}          // ← nodes=nil, err=nil
    return
}
...
} else {                               // ← 被当成「成功但 0 节点」
    m.lastUpdated[key] = time.Now()    // ← ①lastUpdated 每轮刷成 now
    m.lastCount[key] = len(r.nodes)    // ← ②lastCount 被清零
    ...
}
...
if len(all) == 0 {
    return 0, fmt.Errorf("没有任何可用节点: %s", strings.Join(errs, "; "))  // ← errs 为空 ⇒ 末尾冒号后空
}
```

```go
// subscription_runtime.go:14-26（ticker 每 10 分钟一次 force=false）
n, err := subManager.refreshAll(ctx, force)
if err != nil {
    slog.Warn("subscription refresh failed", "when", when, "error", err)   // ← 假 WARN
    return
}
slog.Info("subscription refresh done", "when", when, "nodes", n)
```

**两条缺陷的因果链**：
1. 订阅默认 24h 间隔，ticker 每 10 分钟一轮 ⇒ 绝大多数轮次**所有源都未到期** ⇒ 每轮 `len(all)==0` 且 `errs` 为空
   ⇒ 每轮打一条 `subscription refresh failed error="没有任何可用节点: "` 的**假 WARN**（144 条/天）。
2. 同一轮把每个未到期源的 `lastUpdated` 刷成 `now`、`lastCount` 清零
   ⇒ `now-lastUpdated` 永远只有 10 分钟 < 24h ⇒ **永远没有源到期** ⇒ 24h 自动更新**永不触发**；
   ⇒ `/api/nodes` 的 `subscriptions[].nodes` 恒为 0。
   ⇒ 长跑实例只在启动（`force=true`）时刷新过订阅，之后节点地址一直陈旧。

## 3. 基线复现（HEAD 干净副本）—— 状态：✅ 两条都复现

装置：`git archive HEAD | tar -x -C /tmp/t60/head`（**未动工作树**）→ 自己写
`zz_verify_t60_baseline_test.go`，其中 ticker 刷新闭包**原样复刻** `HEAD:subscription_runtime.go:14-26`
（HEAD 的闭包是局部匿名函数，不可直接调用，故逐行复制同一段代码并注明出处）。

```bash
$ cd /tmp/t60/head && go test -count=1 -run 'TestT60Baseline' -v .
=== RUN   TestT60BaselineFakeWarnAndStateClobber
    ① 返回值: n=0 err=没有任何可用节点: 
    ② lastCount: 215 → 0
    ③ lastUpdated: 19:07:56.681 → 19:17:56.681（被冲刷=true, 距现在=0s）
    ④ 未到期源被真实拉取次数=0（期望 0，说明走的是 skip 分支）
    ⑤ WARN 原文: WARN subscription refresh failed when=ticker error=没有任何可用节点: 
--- PASS: TestT60BaselineFakeWarnAndStateClobber (0.00s)
=== RUN   TestT60BaselineNeverDueLoop
    第 1 轮 ticker: lastCount=0, since(lastUpdated)=0s, 距 24h 还差=24h0m0s, err="没有任何可用节点: "
    第 2 轮 ticker: lastCount=0, since(lastUpdated)=0s, 距 24h 还差=24h0m0s, err="没有任何可用节点: "
    第 3 轮 ticker: lastCount=0, since(lastUpdated)=0s, 距 24h 还差=24h0m0s, err="没有任何可用节点: "
--- PASS: TestT60BaselineNeverDueLoop (0.00s)
=== RUN   TestT60BaselineWouldFetchWhenActuallyStale
    lastUpdated=now-25h 时: n=1 err=<nil> 拉取次数=1
--- PASS: TestT60BaselineWouldFetchWhenActuallyStale (0.06s)
```

- 假 WARN 原文与生产一致：`error=没有任何可用节点: `（冒号后为空，因 `errs` 为空）。
- `lastCount` 215 → **0**；`lastUpdated` 被冲刷到 now；连续 3 轮 `since(lastUpdated)=0s` ⇒ **永远不到期**。
- 对照组：真把 `lastUpdated` 放到 25h 前，HEAD **会**去拉取 ⇒ 说明缺陷不是「不会拉」，而是**永远轮不到**（这正是「长跑实例订阅陈旧」的机制）。

## 4. 修复后验证 —— 状态：✅ 6/6 PASS

测试文件 `zz_verify_t60_subscription_test.go`（工作树，默认 SKIP、`T60_VERIFY=1` 才跑；
`setNodes`/`mihomo.apply` 用实现方抽出的 `subApplyNodes`/`subApplyMihomo` 变量打桩，日志用自定义 `slog.Handler` 捕获级别+属性）。

```bash
$ T60_VERIFY=1 go test -count=1 -run 'TestT60' -v .
--- PASS: TestT60AllNotDueIsSilentNoop
    PASS 全未到期: 无 WARN/INFO、DEBUG=1、lastCount=215 不变、lastUpdated 不变、setNodes=0、拉取=0
--- PASS: TestT60DueAllFailWarnWithSourceName
    PASS 到期全失败: err="订阅源拉取失败: 订阅 机场B(http://127.0.0.1:1/dead-sub): Get ... connection refused"、
         WARN 含源名、setNodes=0
--- PASS: TestT60DueSuccessZeroNodesDistinctWording
    PASS 成功但 0 节点: err="订阅源返回 0 个节点: 订阅 机场C(http://127.0.0.1:34885)"（措辞独立）、setNodes=0
--- PASS: TestT60DueAfter24hActuallyFetches
    PASS 24h 到期: 拉取=1、nodes=2、lastCount=2、lastUpdated=now、INFO due=1；紧接一轮未到期不重复拉取
--- PASS: TestT60ForceIgnoresInterval
    PASS force=true: 无视 24h 间隔，立即拉取
--- PASS: TestT60NodesHandlerPerSourceCount
    PASS /api/nodes: subscriptions[0].nodes=2、last_updated_at=2026-10-20T19:18:46+08:00
ok  	github.com/shiyanYX/opencode2api	0.030s
```

| 验收点 | 结果 |
|---|---|
| 全未到期 ⇒ 无 WARN、**无任何 INFO**（只有 1 条 DEBUG）、`lastCount`/`lastUpdated` 不变、不调 `setNodes` | ✅ |
| 到期且全失败 ⇒ WARN 且错误文本**含源名 + URL**，且不 `setNodes`（不清池） | ✅ |
| 到期成功但 0 节点 ⇒ **单独措辞**「返回 0 个节点」（与「拉取失败」区分），不 `setNodes` | ✅ |
| 24h 到期 ⇒ 真的拉取 | ✅ 见 §5 |
| `force=true`（启动路径）无视间隔 | ✅ |
| `/api/nodes` 每源 `nodes` = 真实计数（2），不再恒 0 | ✅ |

## 5. 24h 自动刷新真的会触发（核心）—— 状态：✅ 已证明

「把可控时钟推过 24h」的等价做法：把该源的 `lastUpdated` 置为 `now-25h`（即「该源上次刷新是 25 小时前」，
与时钟前推 25h 对到期判定完全等价；实现未提供可注入时钟，这是不改造生产码前提下最严格的做法）。

```bash
$ T60_VERIFY=1 go test -count=1 -run 'TestT60DueAfter24h' -v .
    PASS 24h 到期: 拉取=1、nodes=2、lastCount=2、lastUpdated=now、INFO due=1；紧接一轮未到期不重复拉取
```

- `lastUpdated=now-25h` ⇒ 第 1 轮 ticker：**真的发 HTTP 请求（hits=1）**、`out.Due=1`、拉到 2 节点、
  `setNodes` 调用 1 次（2 个节点入池）、`lastCount=2`、`lastUpdated` 刷新为 now、INFO `due=1 skipped=0`。
- 紧接着第 2 轮：未到期 ⇒ **不再拉取**（hits 仍 1）、无 WARN。
- 结合 §4 第一条「未到期不改 `lastUpdated`」⇒ `since(lastUpdated)` 会真实增长，24h 后必然到期
  ⇒ 修复后的稳态是「每 24h/源刷新一次」，而不是「只在启动刷一次」。

## 6. 变异测试（我自造 5 个，确认测试会红）—— 状态：✅ 5/5 CAUGHT

对**修复后副本**（`/tmp/t60/cur`）逐一注入，跑同一套 `TestT60*`（预期「测试变红 = CAUGHT」）。

| # | 变异 | 注入点 | 结果 | 被哪条抓（原始输出摘录） |
|---|---|---|---|---|
| M1 | **未到期算成错误** | skip 分支追加 `errs = append(...)` | **CAUGHT** | `未到期不该报错，实际 err=订阅源拉取失败: 未到期源: http://127.0.0.1:44211`；连带 `第二轮不该报错` |
| M2 | **空结果仍 setNodes([])** | 无到期源的 default 分支加 `subApplyNodes(nil); out.Applied=true` | **CAUGHT** | `outcome 不符: {Nodes:3 Due:0 Skipped:1 Applied:true}（期望 Applied=false）` |
| M3 | **错误文本丢源名** | `fmt.Errorf("订阅源拉取失败")` 去掉 parts | **CAUGHT** | `错误文本必须带源名，实际 "订阅源拉取失败"` |
| M4 | **跳过仍清零 lastCount / 刷 lastUpdated** | skip 分支里写 `lastUpdated=now; lastCount=0` | **CAUGHT** | `状态被改动: lastCount=0（期望 215） lastUpdated 变化=true` |
| M5 | **「无到期源」从 DEBUG 抬成 INFO** | `slog.Debug` → `slog.Info` | **CAUGHT** | `未到期却打 INFO（应为 DEBUG 级，不刷屏）: [INFO subscription refresh: 本轮无到期源 when=ticker skipped=1 nodes=3]` |

原始输出（摘录）：

```
================ 变异 m1 ================
    zz_verify_t60_subscription_test.go:124: 未到期不该报错，实际 err=订阅源拉取失败: 未到期源: http://127.0.0.1:44211
--- FAIL: TestT60AllNotDueIsSilentNoop (0.00s)
--- 退出码: 1 ---
================ 变异 m2 ================
    zz_verify_t60_subscription_test.go:127: outcome 不符: {Nodes:3 Due:0 Skipped:1 Applied:true}（期望 Due=0 Skipped=1 Applied=false）
--- FAIL: TestT60AllNotDueIsSilentNoop (0.00s)
--- 退出码: 1 ---
================ 变异 m3 ================
    zz_verify_t60_subscription_test.go:176: 错误文本必须带源名，实际 "订阅源拉取失败"
--- FAIL: TestT60DueAllFailWarnWithSourceName (0.53s)
--- 退出码: 1 ---
================ 变异 m4 ================
    zz_verify_t60_subscription_test.go:142: 状态被改动: lastCount=0（期望 215） lastUpdated 变化=true lastError=0
--- FAIL: TestT60AllNotDueIsSilentNoop (0.01s)
--- 退出码: 1 ---
================ 变异 m5 ================
    zz_verify_t60_subscription_test.go:154: 未到期却打 INFO（应为 DEBUG 级，不刷屏）: [INFO subscription refresh: 本轮无到期源 when=ticker skipped=1 nodes=3]
--- FAIL: TestT60AllNotDueIsSilentNoop (0.00s)
--- 退出码: 1 ---
```

## 7. 回归 —— 状态：✅ 全绿

```bash
$ go test -count=1 ./
ok  	github.com/shiyanYX/opencode2api	183.758s
EXIT=0

$ T60_VERIFY=1 go test -count=1 -race -run 'TestT60' -v .
--- PASS: TestT60AllNotDueIsSilentNoop (0.00s)
--- PASS: TestT60DueAllFailWarnWithSourceName (0.00s)
--- PASS: TestT60DueSuccessZeroNodesDistinctWording (0.00s)
--- PASS: TestT60DueAfter24hActuallyFetches (0.00s)
--- PASS: TestT60ForceIgnoresInterval (0.00s)
--- PASS: TestT60NodesHandlerPerSourceCount (0.01s)
ok  	github.com/shiyanYX/opencode2api	1.066s          # 无 DATA RACE

$ go vet ./...
（无输出，退出码 0）

$ gofmt -l subscription.go subscription_runtime.go subscription_parse.go zz_verify_t60_subscription_test.go
（无输出 = clean）
```

- 全量 183.8s **0 失败**；`-race` 目标 6 条全 PASS 且无 DATA RACE。
- 我的测试默认 **SKIP**（`T60_VERIFY=1` 才跑），不给常规全量引入副作用。

## 8. 范围 / 无夹带 —— 状态：✅ 只动订阅刷新两文件

```bash
$ sha256sum subscription*.go
902773432ae8512cf54113a1d1075924edce6affaa314c6c2e55a20f281bab92  subscription.go
cf392f0e27af214f5cd41085d08cc31165020d7b2bf9058db43bca67acc43a43  subscription_parse.go   # 未改动
af3b0a46ee0701d127654643cad08a4a750aa506ac7c93d1d29a7e86b5d5c96f  subscription_runtime.go

$ git diff --numstat -- subscription.go subscription_runtime.go
84	19	subscription.go
24	6	subscription_runtime.go

$ git diff -U0 -- subscription.go | grep '^@@'      # 全部落在 refreshAll 区域
...（19 段，全部位于 `refreshAll`/`refreshAllWithOutcome` 函数体内 183–370 行）
```

- 订阅线改动**只在 `subscription.go`（refreshAll 重构）+ `subscription_runtime.go`（refreshOnce 日志语义）**；
  `subscription_parse.go` 未变。
- 工作树里另外两个 `.go` 改动**归属其它任务线**，与本次无关：
  - `main.go` **+8 行** = task-56 的 calllog flags（`-calllog-layout` / `-calllog-gzip-after-days` / `-calllog-retain-days` + `startCallLogArchiver()` 接线），
  - `calllog.go` **+1083/-66**（截至复核时刻，task-56 仍在改，数值会变）= task-56 日志改造。
- 我方新增：`docs/research/subscription-refresh-verification.md` + `zz_verify_t60_subscription_test.go`；生产码未被我改。

## 9. 量化 + 生产只读佐证 —— 状态：✅

**量化**

| 指标 | 修复前 | 修复后 |
|---|---|---|
| 假 WARN `subscription refresh failed` | 每 10 分钟 1 条 ⇒ **144 条/天**（生产实测 4 条/40 分钟，节奏精确） | **0 条**（无到期源只打 DEBUG，默认 info 级不可见） |
| 真实刷新成功日志 | 仅启动 1 条（ticker 轮次全部走失败分支，从不打 done） | 每源每 24h 1 条 INFO（5 源 ≈ 5 条/天），另加 `due/skipped/nodes` 便于观测 |
| 订阅自动刷新 | **永不触发**（每 10 分钟把 lastUpdated 刷成 now，永远差 24h） | **每 24h/源触发一次**（或按 `update_interval_hours` 配置） |
| `/api/nodes` 每源 `nodes` | 恒 0（lastCount 每轮清零） | = 真实节点数 |
| 真实拉取频率 | 仅启动 `force=true` 一次；之后 0 | 启动 1 次 + 每源每 24h 1 次 |

**生产只读佐证**（容器 `opencode2api`，`Up 39 minutes`，StartedAt `2026-10-20T10:42:41Z` = 18:42:41 +08；部署仍是旧版）

```bash
$ docker logs opencode2api --since 40m | grep -c 'subscription refresh failed'
4
$ docker logs opencode2api --since 40m | grep 'subscription refresh failed' | tail -4
18:52:42 level=WARN msg="subscription refresh failed" when=ticker error="没有任何可用节点: "
19:02:42 level=WARN msg="subscription refresh failed" when=ticker error="没有任何可用节点: "
19:12:42 level=WARN msg="subscription refresh failed" when=ticker error="没有任何可用节点: "
19:22:42 level=WARN msg="subscription refresh failed" when=ticker error="没有任何可用节点: "
```

- 节奏**精确 10 分钟**，错误末尾冒号后为空 ⇒ 复现的正是「errs 为空」的假告警。
- 40 分钟 4 条（含启动后首个 ticker）；与「6h 35 条」的速率一致（≈6 条/小时）。

```bash
# 容器内登录（用容器自带 OPENCODE2API_PASSWORD，不落盘、不打印），只读 GET /api/nodes
$ docker exec opencode2api sh -c 'wget -qO- --load-cookies=... http://127.0.0.1:8000/api/nodes' | python3 -c '...'
healthy=126 dead=91 exhausted=0 total_nodes=217
源='飞鸟云' nodes=0 last_updated_at='2026-10-20T19:22:42+08:00' last_error=None
源='红杏'   nodes=0 last_updated_at='2026-10-20T19:22:42+08:00' last_error=None
源='1'      nodes=0 interval=24h last_updated_at='2026-10-20T19:22:42+08:00' last_error=None
源='2'      nodes=0 interval=24h last_updated_at='2026-10-20T19:22:42+08:00' last_error=None
源='3'      nodes=0 interval=24h last_updated_at='2026-10-20T19:22:42+08:00' last_error=None
```

**这条生产数据把两条缺陷同时钉死**：
- 节点池实际有 **217 个节点**（126 healthy / 91 dead），`last_error` 全空 ⇒ 告警是**假**的；
- 5 个订阅源的 `nodes` **全为 0**，且 `last_updated_at` **全部等于最后一次 ticker 时刻 19:22:42**
  （正是第 4 条假 WARN 的时间戳）⇒ `lastCount` 被清零、`lastUpdated` 被每 10 分钟冲刷，
  `since(lastUpdated)` 永远 ≤10 分钟 < 24h ⇒ **自动刷新永不触发**、面板订阅计数恒 0 —— 与 §3 基线完全一致。

## 10. go / no-go —— 状态：✅

# **go**

两条缺陷代码级成立、基线独立复现、修复后逐项达标；24h 自动刷新用「lastUpdated=now-25h」等价推钟证明**真的会拉**；
5/5 自造变异全 CAUGHT；全量 183.8s / `-race` / vet / gofmt 全绿；只动订阅刷新两文件，无夹带。

### 放行附带（均不阻断）

1. **语义**：无到期源现在只打 **DEBUG** 一行（默认 info 级不可见）。这是正确的——「无事可做」不是告警；
   若排障需要，临时把日志级别调到 debug 即可看到 `skipped/due/nodes`。
2. **部分失败**：若部分源失败、其它源成功（`len(all)>0`），行为与旧版一致——只打 INFO，不 WARN（失败源记入 `lastError`，面板可查）。
   本次未改这条语义；如需「部分失败也要 WARN」是后续增强，不属于本缺陷。
3. **webshare 路径**：修复对 webshare 做了对称处理（skip 分支、空结果措辞），我复核了代码与对称性，但测试主要以订阅源为主
   （webshare 需要外部 API 配置，单测成本高）。建议上线后观察一轮。
4. **部署后行为**：重启时 `force=true` 启动刷新仍会全量拉一次（不受 24h 判断影响）；之后按每源 24h 周期自动刷新。
   上线首日建议看一眼 `/api/nodes` 的 `subscriptions[].nodes` 应变为真实计数、`last_updated_at` 只在真实刷新时变。
