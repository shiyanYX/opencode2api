# 订阅刷新假告警 + `/api/nodes` 订阅计数恒 0：修复报告（task-59 / 看板 task-60）

> 状态：**实现 + 自证完成，待独立验收**（未自行宣布完成）
> 用户反馈：「面板上明明有正常的节点，但是日志里面报错没有可用节点」
> 改动文件：`subscription.go`（+84/−19）、`subscription_runtime.go`（+24/−6）、新增 `zz_t59_subscription_refresh_test.go`（gitignored，留盘）
> **`main.go` 一行未改**（task-59 未触碰；当前工作树里 `main.go` 的改动属于并发的 calllog 日分文件线 task-56，与本任务无关）—— `/api/nodes` handler 本身是对的（见 §2.3），问题在写入侧
> sha256：`subscription.go` = `902773432ae8512cf54113a1d1075924edce6affaa314c6c2e55a20f281bab92`
> 　　　　`subscription_runtime.go` = `af3b0a46ee0701d127654643cad08a4a750aa506ac7c93d1d29a7e86b5d5c96f`
> 　　　　测试文件 = `549fe0479d9447e3f47ef3e2f8ecfd088b4d2cdef4121c356d1d98a4dc1b82cc`

## 0. 结论

**假告警根因（实测复现，非推断）**：`refresh(force=false)` 时「未到 24h 更新间隔」的源被当成**「成功但返回 0 个节点」**处理（`result{}` 空值），于是：

1. `len(all)==0` 且 `errs` 为空 ⇒ 命中 `没有任何可用节点: %s` 分支 ⇒ ticker 每 10 分钟打一条 WARN（尾部 `: ` 后空白，铁证）；
2. 同一个分支把 `lastCount` **清零** ⇒ 面板 `/api/nodes` 每个订阅源 `nodes` 恒为 0（这正是「顺手修」那一条的真正根因：handler 读 `lastCount` 是对的）；
3. 同一分支还把 `lastUpdated` **刷成 now** ⇒ `now - lastUpdated` 永远是 10 分钟 < 24h ⇒ **订阅源再也不会到期，24h 自动更新永远不触发**（只能靠启动/手动强制刷新）。这是比假告警更严重的连带缺陷，一并修掉。

**修法**：未到期的源显式标记 `skipped`，**不碰任何状态、不贡献节点/错误**；`len(all)==0` 时按三种语义分别返回：

| 情况 | 行为 | 日志级别 |
| --- | --- | --- |
| 有源到期但拉取失败 | `error("订阅源拉取失败: 订阅 源A(url): ...")`（**带源名**） | WARN（1 条/轮） |
| 有源到期、拉取成功但解析出 0 个节点 | `error("订阅源返回 0 个节点: ...")`（与失败分开措辞） | WARN |
| 无源到期 / 没配源 | 返回成功（当前池大小），**不清池、不报错** | DEBUG 一行 |

**行为红线保持**：`len(all)==0` 时三种分支都**不调用** `setNodes`（不因本轮没拉到而清空现有节点池）—— T4 + 变异 MUT-2 钉住。

**端到端实测（真机、真实 10 分钟 ticker、同一 config）**：

| 观察点 | 修复前（:9126） | 修复后（:9127） |
| --- | --- | --- |
| `+10min` tick 日志 | `WARN subscription refresh failed when=ticker error="没有任何可用节点: "` | `DEBUG subscription refresh: 本轮无到期源 when=ticker skipped=1 nodes=2` |
| `/api/nodes` 每源 `nodes` | 2 → **0**（被清零） | 2 → **2**（保持真实值） |
| `/api/nodes` `last_updated_at` | 18:50:54 → **19:00:54**（假刷新） | 18:50:54 → **18:50:54**（未被篡改） |
| 节点池大小 | 2（未清空） | 2（未清空） |

## 1. 复现证据

### 1.1 用当前代码跑一次 ticker 分支（`force=false`，无 sleep）

```go
m.configure([]SubscriptionConfig{{Name: "飞鸟云", URL: dataURI}}, nil, nil, t.TempDir())
m.lastUpdated[url] = time.Now().Add(-10 * time.Minute) // 模拟「启动时刚刷新过」
m.lastCount[url] = 31
n, err := m.refreshAll(ctx, false)
// 复刻 subscription_runtime.go 的 ticker 分支
if err != nil { slog.Warn("subscription refresh failed", "when", "ticker", "error", err) }
```

输出（修复前）：

```
2026/10/20 18:42:35 WARN subscription refresh failed when=ticker error="没有任何可用节点: "
    refreshAll 返回: nodes=0 err=没有任何可用节点: 
    lastCount[飞鸟云] 被改成: 0（应为 31）
    lastUpdated[飞鸟云] 被改成: 18:42:35（距现在 0.0 分钟，原来应为 10 分钟）
```

与生产那条逐字一致（连 `error=` 引号里的尾部空格都一样）。

### 1.2 生产只读佐证（`docker logs`，未 exec、未改容器）

```
=== 今日抓到的同一条假 WARN ===
time=2026-10-20T18:52:42.088+08:00 level=WARN msg="subscription refresh failed" when=ticker error="没有任何可用节点: "
=== 其 error 为「没有任何可用节点: 」的条数 ===
1        （docker 仅保留最近窗口；容器 12:40 重启后日志已滚动）
```

### 1.3 真机 before/after（真实二进制 + 真实 10 分钟 ticker）

两套完全相同的 config（1 个 24h 订阅源，`data:` 内联节点便于本地复现，无 webshare/手填节点），分别起修复前/后二进制，18:50:54 同时启动，等 `+10min` tick：

```
=== BEFORE(:9126) ===
18:50:54.569 level=INFO  msg="subscription refresh done" when=startup nodes=2
19:00:54.534 level=WARN  msg="subscription refresh failed" when=ticker error="没有任何可用节点: "

=== AFTER(:9127) ===
18:50:54.569 level=INFO  msg="subscription refresh done" when=startup nodes=2 due=1 skipped=0
19:00:54.529 level=DEBUG msg="subscription refresh: 本轮无到期源" when=ticker skipped=1 nodes=2

=== /api/nodes（tick 之后） ===
:9126  sub.nodes=0 last_updated_at=2026-10-20T19:00:54+08:00 pool_nodes=2
:9127  sub.nodes=2 last_updated_at=2026-10-20T18:50:54+08:00 pool_nodes=2
```

一次 tick 同时复现并验证了三件事：假 WARN、面板计数被清零、`last_updated_at` 被伪造；修复后三者全部正常。

## 2. 根因与连带缺陷（修复前 `file:line`）

### 2.1 主链

- `subscription_runtime.go:26-32`：ticker **每 10 分钟** `refresh(false, "ticker")`；
- `subscription.go:30-35` / `webshare.go:30-35`：订阅与 webshare 默认间隔都是 **24h**（生产三个源显式配 24h）；
- `subscription.go:225-233`（修前）：未到期的源 `results <- result{sub: s}` —— **nodes=nil、err=nil**；
- `subscription.go:239-254`（修前）：收集端 `if r.err != nil {...} else { lastUpdated=now; lastCount=len(nodes)=0; ... }` ⇒ 空结果被当成「成功但 0 节点」；
- `subscription.go:295-297`（修前）：`if len(all)==0 { return 0, fmt.Errorf("没有任何可用节点: %s", strings.Join(errs,"; ")) }` ⇒ `errs` 也为空，返回 `没有任何可用节点: `。

### 2.2 三个连带后果

1. ticker 每 10 分钟 1 条 WARN（≈144 条/天），且文本误导排障（看起来像节点池空了，实际池子 215 节点正常）；
2. `lastCount` 清零 ⇒ `/api/nodes` 每源 `nodes` 恒 0（生产实测飞鸟云/红杏/3 个 webshare 全 0）；
3. `lastUpdated` 每 10 分钟被刷成 now ⇒ **24h 间隔永远不到期 ⇒ 订阅永久不再自动更新**（只靠启动 force / 手动强制）。这是本次复查才发现、比假告警更严重的问题；修复后 T1/T3 断言 `lastUpdated` 原样不动。

### 2.3 「顺手修」结论：handler 没错，不用改 `main.go`

- `main.go:7327`：`"subscriptions": subManager.snapshot()`；
- `subscription.go:128-170` `snapshot()` 里 `Nodes: m.lastCount[s.URL]` / `m.lastCount[key]` —— 字段名（`nodes`）、读取源（`lastCount`）都是对的；
- 错在 `refreshAll` 每轮把 `lastCount` 清零 ⇒ 只需改写入侧，`main.go`/前端无需任何改动（T5 直接打 `adminNodesHandler` 验证）。

## 3. 修法

### 3.1 `subscription.go`

- `result` / `wsResult` 增加 `skipped bool`；未到期的源 `skipped: true`（`subscription.go:257`、`:304`）；
- 收集端 `if r.skipped { skipped++; continue }` —— **在拿锁之前**跳过，不碰 `lastCount`/`lastUpdated`/`lastMeta`/`lastError`（`subscription.go:266`、`:313`）；
- 到期源成功但 0 节点 ⇒ 记入 `emptySources`（`subscription.go:281`、`:327`），与失败（`errs`）分开；
- `len(all)==0` 三路分支（`subscription.go:339-357`）：
  - 有失败 ⇒ `订阅源拉取失败: <errs; 0节点源>`（**每条错误都带源名**，如 `订阅 源A(ftp://...): 不支持的订阅协议 "ftp"`）；
  - 无失败但有 0 节点源 ⇒ `订阅源返回 0 个节点: 订阅 源B(...)`；
  - 都没有 ⇒ 成功返回 `proxyPool.nodeCount()`，**不调用 setNodes**，DEBUG 语义交给 runtime；
- 真正有节点时才 `subApplyNodes(all)` + `subApplyMihomo(all)`（`subscription.go:362-363`）；
- 两个副作用抽成包级变量（`subApplyNodes` / `subApplyMihomo`，`subscription.go:186-189`）：测试可断言「未被调用」，且单测不会启动真实 mihomo executor；
- 新增 `refreshOutcome{Nodes, Due, Skipped, Applied}`（`subscription.go:192-197`）与 `refreshAllWithOutcome`（`:215`）；`refreshAll(ctx, force) (int,error)` 保留为薄包装（`main.go:7358/7384` 两处强制刷新调用点**零改动**）。

### 3.2 `subscription_runtime.go`

- 新增 `refreshOnce(ctx, force, when)`（`:17-32`），按语义打日志：失败 WARN、无到期源 DEBUG、正常刷新 INFO（带 `due`/`skipped` 字段便于排障）；
- `startSubscriptionTicker` 改为 `subManager.refreshOnce(...)`，ticker 间隔/启动 force 行为不变（`subscription_runtime.go:42`）。

### 3.3 为什么不是别的修法

| 备选 | 评价 |
| --- | --- |
| 只把 `len(all)==0 && len(errs)==0` 当成功 | ✗ 只消掉 WARN；`lastCount`/`lastUpdated` 仍被清零与刷新 ⇒ 面板计数仍 0、24h 永远不到期 |
| 把 ticker 间隔改成 24h | ✗ 治标：手工强制刷新/启动仍会踩同一分支；且失去「频繁检查到期」的能力 |
| 在 handler 里「计数为 0 时回退旧值」 | ✗ 掩盖写入侧状态污染；`lastUpdated` 被伪造的问题还在 |
| 删除 ticker | ✗ 订阅再也不会自动更新 |
| **显式 skipped + 三语义（采用）** | ✓ 一次修掉三个连带后果；语义准确；行为红线保持；日志噪声归零 |

## 4. TDD 证据（先红后绿）

### 4.1 行为红（修复前代码，T1 核心断言）

```
=== RUN   TestT59T1CoreOnOldCode
    zz_t59_t1_oldapi_test.go:25: T1：全部源未到期却被当成失败 ⇒ ticker 会打 WARN：err=没有任何可用节点: 
--- FAIL: TestT59T1CoreOnOldCode (0.00s)
FAIL
```

### 4.2 编译红（新 API 未实现时）

```
./zz_t59_subscription_refresh_test.go:165:24: undefined: subApplyNodes
./zz_t59_subscription_refresh_test.go:165:39: undefined: subApplyMihomo
./zz_t59_subscription_refresh_test.go:188:14: m.refreshOnce undefined (...)
FAIL	github.com/shiyanYX/opencode2api [build failed]
```

### 4.3 绿（实现后，5 个用例 0.009s，无 sleep）

```
--- PASS: TestT59NoDueSourcesReturnsSuccessWithoutWarn (0.00s)
--- PASS: TestT59AllDueFailuresReportSourceNames (0.00s)
--- PASS: TestT59PartialDueSuccessAppliesOnlyDueNodes (0.00s)
--- PASS: TestT59EmptyResultDoesNotClearPool (0.00s)
--- PASS: TestT59APINodesReportsPerSourceCounts (0.00s)
ok  	github.com/shiyanYX/opencode2api	0.009s
```

| 用例 | 钉住的契约 |
| --- | --- |
| T1 `NoDueSourcesReturnsSuccessWithoutWarn` | 全部未到期 ⇒ 无 WARN、无 INFO（只 1 条 DEBUG）；`lastCount` 31 保持、`lastUpdated` 原样、`lastMeta` 不被覆盖；不写池/不写 mihomo |
| T2 `AllDueFailuresReportSourceNames` | 两个到期源全失败（`ftp://` 不支持协议，零网络）⇒ 错误含 `源A`/`源B`/`拉取失败`；WARN 恰好 1 条；不写池 |
| T3 `PartialDueSuccessAppliesOnlyDueNodes` | 1 个到期成功（3 节点）+ 1 个未到期 ⇒ 成功、写池 1 次恰好 3 节点、mihomo 1 次、到期源计数 3、未到期源计数保持 5 且 `lastUpdated` 不动；恰好 1 条 INFO |
| T4 `EmptyResultDoesNotClearPool` | ①有源但全未到期 ②一个源都没配 ⇒ 两种情况都**不调用 setNodes**（红线） |
| T5 `APINodesReportsPerSourceCounts` | 直接打 `adminNodesHandler` GET `/api/nodes` ⇒ 每源 `nodes` == `lastCount`（12 / 7），`last_updated_at` 非空 |

## 5. 变异测试（5/5 CAUGHT，恢复后 sha256 一致）

脚本 `/tmp/t59mut/run_mutations.sh`：改坏 → `go test -run '^TestT59'` → 从备份恢复 → 校验 sha256。

| # | 变异（把修复改坏） | 结果 | 被哪条测试抓住 |
| --- | --- | --- | --- |
| MUT-1 | **「未到期」重新算成「成功但 0 节点」**（= 原始 bug 回归） | **CAUGHT** | T1（`没有源到期却被当成失败：err=订阅源返回 0 个节点...`）、T3（`未到期源 lastCount 被改成 0`）、T4 |
| MUT-2 | `len(all)==0` 时仍调 `setNodes([])`（清空节点池） | **CAUGHT** | T1（`无到期源不该写池/写 mihomo：pool=1`）、T4 |
| MUT-3 | 错误文本丢掉源名 | **CAUGHT** | T2（`错误文本缺 "源A"`） |
| MUT-4 | 跳过源仍清零 `lastCount` / 刷新 `lastUpdated`（面板计数 + 24h 到期再坏） | **CAUGHT** | T1（`lastCount 被改成 0（应为 31）`）、T3 |
| MUT-5 | 无到期源改成 INFO（144 条/天的新噪声回归） | **CAUGHT** | T1（`应恰好 1 条 DEBUG「本轮无到期源」，实际 0 条`） |

```
== restored sha: subscription.go=9027734… subscription_runtime.go=af3b0a4…
== restore OK
== after-restore green: ok  github.com/shiyanYX/opencode2api  0.010s
```

## 6. 回归

**两轮都跑了：先隔离快照（抢时间），后真实工作树（最终证据）**

| 命令 | 隔离快照 `/tmp/t59/reg-src`（18:56） | **真实工作树（19:06–19:09，最终）** |
| --- | --- | --- |
| `gofmt -l`（我改的 3 个文件） | 无输出（clean） | 无输出（clean） |
| `go vet .` | OK | OK |
| `go test -race -count=1 -run 'TestT59\|TestSubscription\|TestSubFetch' ./` | `ok ... 1.077s` | **`ok ... 1.074s`**，无 DATA RACE |
| `go test -count=1 ./`（全量） | `ok ... 187.618s` | **`ok ... 167.296s`**，**全绿** |

> 为什么先有快照：首轮回归期间，工作树被**并发的 calllog 日分文件任务（task-56）**的中间态破坏（`calllog.go:9 "compress/gzip" imported and not used`；`calllog_daily_test.go` / `calllog_retention_test.go` 引用尚未实现的 `dailySegments`/`historyQuery` 等），整个包无法编译（`vet: ./calllog_retention_test.go:152:28: callLog.historyQuery undefined`）。快照 = 当时工作树 + `git show HEAD:calllog.go` 还原该并发线已提交版本 + 暂移其未提交测试文件（与既有「task8_bucket_query_test.go 移开」同法）；**我的 3 个文件与工作树逐字节一致**。该并发线恢复可编译后（19:06:27 `go vet` 通过），已在**真实工作树**复跑上面全部命令并全绿 —— 上表右列即最终回归结论。

## 7. 生产现状（只读，未 exec、未改容器）

- 假 WARN 仍在生产发生：`2026-10-20T18:52:42.088` 抓到逐字一致的一条（`error="没有任何可用节点: "`）；
- 容器 12:40 重启，`docker logs` 仅保留最近窗口（今日抓到 1 条；任务书里 Lead 观测的是最近 6h 35 条，量级一致 ≈ 10 分钟/条）；
- 修复未部署；部署后预期：`subscription refresh failed` 归零、`/api/nodes` 每源 `nodes` 恢复真实值、24h 自动更新恢复（此前它从未真正生效过）。

## 8. 残留风险 / 说明

1. **错误文本变了**（`没有任何可用节点: …` → `订阅源拉取失败: …` / `订阅源返回 0 个节点: …`）：若外部告警规则 grep 旧文本需同步；这是刻意为之（旧文本无法区分「没源到期」与「真的全失败」）。
2. **未到期源不再刷新 `last_updated_at`**：面板上会如实显示「上次真正更新的时间」，不再每 10 分钟伪造一次「刚刚更新」。
3. **`lastCount` 语义未变**：成功拉取后记「本轮解析出的节点数」，与旧行为一致（含之后可能被健康检查判死的节点）。
4. **全部源成功但都返回 0 节点、同时存在手填节点**：`all>0` 走成功路径（不报错）—— 与「手填节点兜底」的既有语义一致；只有真·空池时才报 `返回 0 个节点`。
5. **并发强制刷新**（`main.go:7358/7384` 面板保存后 `force=true`）与 ticker 之间没有共享锁（runtime 的 `mu` 只在 ticker 内部）—— 这是**既有**行为，本次未改；两个 `refreshAll` 并发时都以「最后一次写入」为准，不丢状态（各源状态独立），仅在极端同时刻有短暂重复拉取。属既有边界，未纳入本次改动。
6. **webshare 分支**：跳过/失败/0 节点三态与订阅同构（代码结构对称），但生产 webshare 是 24h 且同样会走到「跳过」分支，故同样受益；单测用 `WebshareConfig` 覆盖了 skip 分支（T1 仅订阅 + T5 计数含 webshare），webshare 真实拉取失败路径依赖网络，未在单测里造（避免 flaky），由代码对称性与 `go vet` 保证。

## 9. 待办 / 交接

- [x] 主树被 task-56 并发修改恢复可编译后，已在真实工作树重跑全量 + `-race`（19:06–19:09 全绿，见 §6 右列）；
- [ ] 部署后只读验证：`docker logs opencode2api --since 15m | grep -c 'subscription refresh failed'`（预期 0）、面板 `/api/nodes` 每源 `nodes` 非 0、`last_updated_at` 不再每 10 分钟跳；
- [ ] 观察第一个 24h 周期：订阅是否真的自动更新（修复前从未生效，需实测确认）。

## 10. 交付物

| 文件 | sha256 | 说明 |
| --- | --- | --- |
| `subscription.go` | `902773432ae8512cf54113a1d1075924edce6affaa314c6c2e55a20f281bab92` | +84/−19，刷新结果三语义 + skipped 显式标记 + 注入副作用 |
| `subscription_runtime.go` | `af3b0a46ee0701d127654643cad08a4a750aa506ac7c93d1d29a7e86b5d5c96f` | +24/−6，`refreshOnce` 按语义打日志 |
| `zz_t59_subscription_refresh_test.go` | `549fe0479d9447e3f47ef3e2f8ecfd088b4d2cdef4121c356d1d98a4dc1b82cc` | 新建 5 用例（gitignored，留盘） |
| `/tmp/t59_repro.log` | — | 修复前 ticker 复现原始输出 |
| `/tmp/t59mut/mutations.log` | — | 5 变异原始输出 |
| `/tmp/t59/verify_snapshot.log` | — | 隔离快照 vet/gofmt/race/全量 原始输出 |
| `/tmp/t59/{before,after}/app.log`、`config.json` | — | 真机 before/after 端到端日志与配置 |
| `/tmp/t59/red_phase*.log` | — | TDD 红（编译红 + 行为红）原始输出 |
