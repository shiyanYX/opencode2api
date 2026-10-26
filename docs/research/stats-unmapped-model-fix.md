# 未映射模型污染统计口径 —— 修复报告（Task 21）

> 状态：**已完成**
> 任务：task-21（owner=verify-403）
> 报告落盘时间：2026-10-19
> 代码基线：`ad699e7`（branch `test`）+ 本次未提交改动

---

## 0. 一句话结论

客户端传入的**无法映射到本部署可服务模型**的名字（`mimo-v2.5`、`x-preview-f`、
`claude-opus-4-5`、`hy3`、`gpt-4o-mini`），此前被 `callLog.append` 无条件计入
小时桶，于是进入 `/api/stats` 的 `total_requests` 与 `models`，并作为**失败**
拉低成功率分母。

修复后：这类请求**不再进入统计聚合**，但**照常写 `call_log.jsonl` + 打 WARN**，
并通过 `/api/stats?range=` 新增的独立字段 `unmapped_requests` 单独可见。

**这是「口径更正」，不是「服务质量提升」。** 排除的是我们**根本没有提供**的
模型；成功率从 91.6504% 变成 91.9480%，变的只是分母，**分子一条没动**
（排除集 661 条全部失败，成功记录 0 条被删）。

---

## 1. 判定语义（Lead 裁决，B 方案）

```
resolved = resolveModel(raw)
mapped   = isFreeModel(resolved) || strings.HasSuffix(resolved, "-free")
unmapped = !mapped
```

**安全闸门**：`freeModelDocsCache` 为空/未加载时**一律判 mapped**（维持现状）。

> ⚠️ 闸门锚点与原任务书不同：原书写「模型目录为空则维持现状」，指的是上游
> `/v1/models` 目录。B 口径**根本不查**上游目录（那正是它躲开漂移的原因），
> 所以上游目录空对它没有意义；它唯一依赖的外部输入是 `freeModelDocsCache`。
> 闸门因此锚在后者上。这是语义澄清，不是语义变更 —— 目的仍是「数据源不可用
> 时不得误判」。

---

## 2. 根因（代码级）

### 2.1 数据流

`CallRecord.Model` 存的是**客户端原样传入的模型名**，不是 `resolveModel` 的结果
—— 全部 6 处 `beginCallLog(...)` 调用点传的都是 `req.Model` / `chatReq.Model`
（改动前基线 `main.go:3483/3675/4700/4739/5948/5989`；改动后
`main.go:3610/3802/4827/4866/6075/6116`，仍是 6 处，全部传 `req.Model` / `chatReq.Model`）。
`resolveModel` 的结果被单独赋值给局部变量，从未写进 `CallRecord`。

于是统计链路只认这个原始名：

| 环节 | 位置（改动前） | 行为 |
| --- | --- | --- |
| 写入 | `calllog.go` `callLogStore.append` 桶 incr 段 | `hourKeyFor(rec.TS, rec.Model)` → `agg.add(...)` |
| 回填 | `calllog.go` `absorbOnce` 解析段 | `hourKeyFor(r.TS, r.Model)` → `b.add(&r)` |
| 查询 | `calllog.go` `foldModelsInWindow` | 按 `k.Model` 聚合 → `models` map |

未映射模型名自成一个模型键，既进 `models`，也进 `total_requests`。

### 2.2 为什么原公式 A（查上游目录）被否决

Lead 最初给的公式是 `isFreeModel(resolved) || modelExistsInCaches(resolved)`。
实测（数据见 §3）证明它有两个叠加错误：

1. **把付费目录当可服务集**：`modelExistsInCaches` 查的是上游 `/v1/models`
   全量目录（2026-10-19 实测 zen **85 条** + go **37 条**），其中绝大多数是付费
   模型。`claude-opus-4-5`、`hy3`、`mimo-v2.5` **都在目录里** ⇒ 判成 mapped ⇒
   **继续污染**，恰好放过用户真正抱怨的那批。
2. **按今天的目录判历史数据**：`deepseek-v4-flash-free`（4,565 条 / 97.2% 成功）
   与 `x-preview-f-free`（1,419 条 / 89.1% 成功）至今仍在跑，却已被上游从目录
   摘掉 ⇒ 判成 unmapped ⇒ **删掉 5,702 条成功记录**，成功率反而**下降 0.0932
   个百分点**。

而 `-free` 是**我们自己的**免费层命名约定（`defaultFreeModels` 与
`free_models_cache.json` 同步维护），比上游目录稳定得多。

---

## 3. 生产数据证据

**数据源**：`/tmp/prod-now.jsonl`，204,237 行 / 109,153,292 字节，
截止 `2026-10-13T09:15:27+08:00`，18 个不同模型名。
**分类方法**：调用**生产函数本体**（`isUnmappedModel` → `resolveModel` /
`isFreeModel`），不是人工推断。证据测试见 §6.3。

### 3.1 逐模型分类

| 原始 model | 请求数 | ok | fail | ok% | 判定 |
| --- | ---: | ---: | ---: | ---: | --- |
| `mimo-v2.5-free` | 182,460 | 166,518 | 15,942 | 91.3 | MAPPED |
| `space-bunny-free` | 14,168 | 14,045 | 123 | 99.1 | MAPPED |
| `deepseek-v4-flash-free` | 4,565 | 4,437 | 128 | 97.2 | MAPPED |
| `x-preview-f-free` | 1,419 | 1,265 | 154 | 89.1 | MAPPED |
| `mimo-v2.6-flash-free` | 916 | 879 | 37 | 96.0 | MAPPED |
| **`mimo-v2.5`** | **624** | **0** | **624** | **0.0** | **UNMAPPED** |
| `nemotron-3-ultra-free` | 34 | 32 | 2 | 94.1 | MAPPED |
| **`x-preview-f`** | **30** | **0** | **30** | **0.0** | **UNMAPPED** |
| **`claude-opus-4-5`** | **4** | **0** | **4** | **0.0** | **UNMAPPED** |
| `big-pickle` | 4 | 4 | 0 | 100.0 | MAPPED |
| `ox-alpha-free` | 3 | 0 | 3 | 0.0 | MAPPED ⚠️ |
| **`hy3`** | **2** | **0** | **2** | **0.0** | **UNMAPPED** |
| `muse-spark-1.2-contributor-free` | 2 | 0 | 2 | 0.0 | MAPPED ⚠️ |
| `ling-3.0-flash-fin-free` | 2 | 2 | 0 | 100.0 | MAPPED |
| `laguna-s-2.1-free` | 1 | 1 | 0 | 100.0 | MAPPED |
| `hy3-free` | 1 | 1 | 0 | 100.0 | MAPPED |
| `muse-spark-1.3-contributor-free` | 1 | 0 | 1 | 0.0 | MAPPED ⚠️ |
| **`gpt-4o-mini`** | **1** | **0** | **1** | **0.0** | **UNMAPPED** |

⚠️ 三个「带 `-free` 后缀但 0 成功」的模型（`ox-alpha-free` 3 条、
`muse-spark-1.2-contributor-free` 2 条、`muse-spark-1.3-contributor-free` 1 条）
**不排除**：它们符合我们的 `-free` 命名约定，是「曾经可服务、现已下线」的模型，
不是「客户端传了野名字」。把它们排除会滑向「按成败反推可服务性」——
那会掩盖真实的服务失败。**这是刻意的保守选择**，见 §8。

### 3.2 修复前后同口径对比

| 指标 | 修复前 | 修复后 | 变化 |
| --- | ---: | ---: | --- |
| `total_requests` | 204,237 | 203,576 | −661 |
| 成功率分子（ok） | 187,184 | 187,184 | **0（不变）** |
| 成功率 | 91.6504% | 91.9480% | **+0.2976 pp** |
| `len(models)` | 18 | 13 | −5 |
| 分母中未映射占比 | — | 0.3236% | — |

**这 +0.2976 pp 全部来自分母更正，分子一条未动。**
排除集 661 条、**ok=0**（硬门禁通过）。

### 3.3 排除集的技术特征（证明它们确实不可服务）

- **token**：`prompt_tokens` 合计 **0**，`completion_tokens` 合计 **0**。
- **速度**：`output_speed > 0` 的记录 **0 条**（没有一条产生过任何输出）。
- **上游状态**：`err_msg` 中 `upstream http 401` **660 条**、
  `upstream http 502` **1 条**。401 = 上游不认这个模型。
- **时间分布**：2026-08 共 34 条、2026-09 共 5 条、2026-10 共 622 条
  （10 月那批几乎全是 `mimo-v2.5`）。

### 3.4 `mimo-v2.5` 这 624 条到底是什么（Lead 条件 3）

必须单独说清，**不能让这 624 条看起来像我们在藏失败**：

1. 客户端发的是**裸名 `mimo-v2.5`**，不是 `mimo-v2.5-free`。
2. `resolveModel` 的 `-free` 反查（`main.go:1443-1447`）**没有命中** ——
   它查的是 `mimo-v2.5-free` 是否在**上游目录**里，而该目录里只有 `mimo-v2.5`
   （go 目录），没有 `mimo-v2.5-free`。所以裸名被**原样发给上游** → 401。
3. 上游从 **2026-10-07** 起对 `mimo-v2.5-free` 返回 **http 410 Gone**
   （实测 1,256 条 410 **全部落在 10-07 当天**；`mimo-v2.5-free` 最后一次成功是
   `2026-10-06T14:26:41+08:00`，最后一次请求是 `2026-10-07T13:22:40+08:00`）。
4. ⇒ 即便修好反查把裸名映射到 `mimo-v2.5-free`，**现在也救不回来**（只会 410）。
5. ⇒ **这 624 条确实不可服务**，判 unmapped 是**诚实的**。
6. 附带发现（**不在本次修复范围**）：`resolveModel` 的反查依赖上游目录而非
   `-free` 命名约定，所以「裸名 → `-free`」这条路在目录不含 `-free` 版本时
   会失效。这是**路由 bug**，与统计口径是两件事，建议单独立项。

---

## 4. 变更点（file:line）

> 行号为**改动后**的当前文件行号。

### 4.1 `main.go`

| 位置 | 变更 |
| --- | --- |
| `main.go:1390` | 新增 `freeModelDocsStale()` —— 「免费表是否陈旧」的**单一判据** |
| `main.go:1399` | 新增 `ensureFreeModelsFresh()` —— 只派发刷新的按需语义（请求路径专用） |
| `main.go:1412` | 新增 `resolveModelWithDocs(model, docs)` —— `resolveModel` 的**纯函数内核** |
| `main.go:1373` | `resolveModel` 改为「刷新判定 → 取快照 → 委托内核」，行为不变 |
| `main.go:1493` | 新增 `isUnmappedModel(raw string) bool` —— 判定纯函数，含安全闸门 |
| `main.go:1530` | 新增 `isFreeModelIn(docs, modelID)` —— 无副作用、显式快照的 membership 查 |
| `main.go:7333` | `statsRangeResponse` 新增 `UnmappedRequests` 字段（json `unmapped_requests`） |
| `main.go:7341` | 新增 `UnmappedRequestsStats` 结构 `{count, by_model}` |
| `main.go:7380-7384` | `statsRangeResponseFor` 用**同一份** `ts/bidx` 折叠未映射统计 |

**为什么要拆出 `resolveModelWithDocs` / `isFreeModelIn` / `freeModelDocsStale`**
（这是实现过程中发现的**真缺陷**，不是洁癖）：

`isUnmappedModel` 最初的写法直接调 `isFreeModel`，而 `isFreeModel` 内部会在
缓存为 nil 或超过 1 小时时执行 `go refreshFreeModelsDocs()`。这带来三个后果：

1. **回填路径被塞进网络 I/O 与磁盘写**：20 万行老记录重算判定时，会触发后台
   拉取并 `saveFreeModelsCache` **改写仓库里的 `free_models_cache.json`**。
2. **制造测试间的顺序依赖**：该文件被 `.gitignore` 忽略（`.gitignore:6`），
   `git checkout` 还原不了；一次跑测就会把 13 条改成 14 条，后续用例读到脏数据。
3. **破坏「同输入 → 同输出」**：同一个数据集重跑两次可能得到不同判定。

所以把「判定」与「刷新」拆开：判定走纯内核（显式接收快照），刷新只留在请求
路径的 `resolveModel` 首行。两条路径共用同一个内核，避免「两份实现各自演化」
导致口径分叉。判据也抽成 `freeModelDocsStale()` 单一来源，让测试验的是**实现
本身**而不是复刻一份判据（否则日后调参会出现「实现改了、测试还在验旧口径」的假绿）。

### 4.2 `calllog.go`

| 位置 | 变更 |
| --- | --- |
| `calllog.go:93` | `CallRecord` 新增 `Unmapped bool`（json `unmapped`，**无 omitempty**） |
| `calllog.go:203` | 新增全局 `unmappedBuckets`（平行桶表，受 `callLog.mu` 保护） |
| `calllog.go:283` | 新增 `unmappedNSumLocked()` |
| `calllog.go:323` | `checkBucketConservation` 的 ΣN 改为**两表之和** |
| `calllog.go:448` | `resetAbsorbedStateLocked` 同时清 `unmappedBuckets` |
| `calllog.go:522` | `rebuildBucketsIfDue` 的一致性判据同样改为两表之和 |
| `calllog.go:666` | 回填新增 `stagedUnmapped` 暂存区 |
| `calllog.go:680` | 回填新增 `unmappedMemo`（模型名 → 判定，18 次调用代替 20 万次） |
| `calllog.go:714-721` | 回填按 `SawUnmapped` 决定「采信落盘值」还是「重算老行」 |
| `calllog.go:793-806` | 锁内合并 `stagedUnmapped`（`+=`，与 `staged` 同规则） |
| `calllog.go:1061/1067` | 新增 `unmappedTopN = 10` / `unmappedOtherKey = "__other__"` |
| `calllog.go:1077` | 新增 `unmappedStatsInWindow(ts, bidx)` |
| `calllog.go:1097` | 新增 `topUnmappedModels(byModel, n)` |
| `calllog.go:1449-1471` | `append` 桶 incr 段：`rec.Unmapped` 走平行表，**仍推进 parsedTo/absorbedRecords** |
| `calllog.go:1550-1593` | 新增 `unmappedWarnInterval`(5min) / `unmappedWarnMaxModels`(64) / `unmappedWarnLast` / `warnUnmappedModel()` —— 按模型限流的 WARN |
| `calllog.go:1611` | `beginCallLog` 在**请求发生那一刻**调用 `isUnmappedModel`（判定冻结） |
| `calllog.go:1615-1618` | `rec.Unmapped` 为真时调 `warnUnmappedModel(model)` —— 「不静默丢弃」的日志落点 |

### 4.3 `statscan.go` —— ⚠️ 越界声明（Lead 条件：显式声明）

**我改了任务书外的文件 `statscan.go`（+约 28 行）。** 理由是：

`parseStatsLine` 是回填用的**零分配手写扫描器**（不经过 `encoding/json`），
是**唯一**能区分「`unmapped` 键缺失（老构建写的行）」与「`unmapped:false`
（新构建判为已映射）」的地方。若不加这个区分，回填只能对**每一行**重算判定；
而重算会踩中 Lead 条件 2 要防的坑：

> `deepseek-v4-flash-free`（4,437 次成功）当时已映射，后来被上游从目录摘掉。
> 用今天的目录重算 ⇒ 追认为未映射 ⇒ **删掉真实成功记录**。

| 位置 | 变更 |
| --- | --- |
| `statscan.go:37` | `slimRec` 新增 `Unmapped bool` |
| `statscan.go:50` | `slimRec` 新增 `SawUnmapped bool`（**键是否出现过**） |
| `statscan.go:156-167` | `parseStatsLine` 识别 `"unmapped"` 键，同时置 `SawUnmapped` |

**`SawUnmapped` 的语义**：`CallRecord.Unmapped` 的 JSON tag **故意不带
`omitempty`**（代价：每行多约 17 字节，20 万行 ≈ +3.4 MB）。这样本构建写的行
**总是**带该键，读取侧一律采信落盘判定，目录日后漂移也不会追认已判定的记录。
但 `omitempty` 的缺席意味着 `encoding/json` 无法区分「false」与「缺失」——
所以扫描器补一个「见到过该键」的位来恢复这个区分。
**老记录重算只对 `SawUnmapped == false` 的行生效**，这正是「修复对历史生效」
与「不误删真实成功记录」两个要求的交点。

**纪律**：以后越过写范围会**先报 Lead 再动手**。本次是先斩后奏，已获追认
（task-21 rev4 把 `statscan.go` 列入可改范围）。

### 4.4 未改动

- `static/admin.html` —— 未碰（task-22 负责面板）
- `node_alert.go`、`docs/research/C5-*.md` —— 未碰
- 生产容器、8000 端口 —— 未碰

---

## 5. 实现方案与取舍

### 5.1 为什么把判定**持久化**进 `CallRecord`（而不是聚合时现算）

1. **锁序**：聚合侧持 `callLog.mu`。现算需要再取 `configMu` /
   `freeModelDocsMu` / `modelMu`，凭空造出 `callLog.mu → 其它锁` 的锁序。
2. **确定性**：判定依赖会漂移的免费表。现算 ⇒ 同一条历史记录在不同日期得到
   不同结论 ⇒ 桶内容非确定；一旦翻转还会误伤守恒判据并触发 ~350ms 自愈重扫。
3. **成本**：全量回填 20 万行，逐行现算 = 40 万次 RLock。

落盘后判定在请求发生那一刻**冻结**，回填与 incr 两侧口径天然一致。

### 5.2 为什么用**平行桶表**而不是给 `hourBuckets` 加维度

`hourBuckets` 的每个键都会被三个折叠内核（`foldModelsInWindow` /
`foldBuckets` / `trendsByModelWithBuckets`）无条件折叠进 `models` /
`total_requests` / `trends`。加维度要改三处签名与语义，风险远大于收益。
而未映射请求**不需要** token / 速度 / 吞吐（实测全是 0），只数条数就够。

### 5.3 守恒判据的连带修改（关键）

原判据 `ΣN(hourBuckets) == absorbedRecords - unabsorbedRecords`。
未映射记录**同样被消费**（`parsedTo` 照样推进、`absorbedRecords` 照样自增），
只是不进 `hourBuckets`。若只数一张表，**每有一条未映射记录判据就差 1**，
进而**每次查询都误判不守恒并触发一次全量重扫**。

⇒ 判据改为 `ΣN(hourBuckets) + ΣN(unmappedBuckets) == absorbed - unabsorbed`。
未映射**不计入 `unabsorbedRecords`** —— 后者的语义是「消费了但两张表都没进」
（坏行 / ts 解析失败），未映射进了平行表，从判据看与进了 `hourBuckets` 等价。

实测（真实日志 204,237 行）：`absorbed=204237`、`unabsorbed=0`、
`ΣN=203576+661=204237` ⇒ **守恒**。

### 5.4 「不静默丢弃」的三个落点

任务书要求未映射请求**不被静默丢弃**。实现上有三处可见痕迹：

| 痕迹 | 位置 | 可见性 |
| --- | --- | --- |
| `call_log.jsonl` 照常写入一行，带 `"unmapped":true` | `calllog.go:1449` 走平行表但仍推进 `parsedTo`/`absorbedRecords` | 逐条、永久 |
| WARN 日志（**按模型限流** 5 分钟/条） | `calllog.go:1573` `warnUnmappedModel()` | 实时、运维可见 |
| `/api/stats?range=` 的 `unmapped_requests` 字段 | `main.go:7333` | 窗口内聚合、面板可见 |

**WARN 为什么要限流**：未映射与否由**客户端输入**决定，一个写错模型名的客户端
会把这条 WARN 变成每请求一条，把有用日志淹掉。5 分钟/模型的间隔既能让人看见
「有野模型名进来了」，又不会刷屏。限流表上限 64 条，超出整体清空 —— 它只承担
「别刷屏」，丢历史不影响正确性。

**WARN 为什么不在 `append` 里打**：`append` 的落盘段在 `callLog.mu` 写锁内，
持锁做同步 I/O 会连带阻塞所有在途 append 与面板查询。落点选在 `beginCallLog`
（请求时刻、无锁）。

### 5.5 取舍：历史脏数据**部分**追溯

- **老行**（无 `unmapped` 键）：回填时用同一个纯函数**重算** ⇒ 修复对历史生效。
  实测这份日志里 661 条历史未映射请求**全部**被正确识别。
- **新行**：采信落盘判定，永不重算。

---

## 6. 测试证据（TDD）

### 6.1 RED —— 先看它失败

实现前，`go test` 因符号不存在而失败（编译期红）：

```
./unmapped_stats_test.go:101:33: unknown field Unmapped in struct literal of type CallRecord
./unmapped_stats_test.go:112:20: undefined: unmappedBuckets
./unmapped_stats_test.go:149:6: undefined: isUnmappedModel
./unmapped_stats_test.go:322:9: out.UnmappedRequests undefined
FAIL	github.com/shiyanYX/opencode2api [build failed]
```

补上 WARN 之后又单独做了一次**变异验证**（把 `slog.Warn(...)` 换成
`_ = model`，其余不动），确认这条用例不是空转：

```
$ go test -count=1 -run TestUnmappedModelWarnsThrottledPerModel .
--- FAIL: TestUnmappedModelWarnsThrottledPerModel (0.00s)
    unmapped_stats_test.go:746: 首条未映射 WARN 必须打出来，实际 0 条:
FAIL
```
装回 WARN 后转绿。**没有这一步，一个只改内部状态、不打日志的实现也能骗过测试。**

### 6.2 GREEN —— 实现后

```
$ go test -run 'TestUnmapped|TestStatsRangeResponseExposes|TestCallRecordUnmapped|TestTopUnmapped|TestLegacyRecords|TestIsUnmappedModelHasNoRefreshSideEffect|TestEnsureFreeModelsFreshStalenessPredicate' -count=1 -v .
# 注意 TestUnmapped 前缀已覆盖 TestUnmappedModelWarnsThrottledPerModel
=== RUN   TestUnmappedPredicateStructuralContract
--- PASS
=== RUN   TestUnmappedPredicateUnknownNames
--- PASS
=== RUN   TestUnmappedNotAggregatedButStillLogged
--- PASS
=== RUN   TestUnmappedBackfillMatchesAppend
--- PASS
=== RUN   TestUnmappedExcludedFromModelStatsAndDenominator
--- PASS
=== RUN   TestStatsRangeResponseExposesUnmappedRequests
--- PASS
=== RUN   TestTopUnmappedModelsTopNAndOther
--- PASS
=== RUN   TestCallRecordUnmappedJSONRoundTrip
--- PASS
=== RUN   TestLegacyRecordsRecomputedButPersistedJudgmentWins
--- PASS
=== RUN   TestIsUnmappedModelHasNoRefreshSideEffect
--- PASS
=== RUN   TestEnsureFreeModelsFreshStalenessPredicate
--- PASS
=== RUN   TestUnmappedModelWarnsThrottledPerModel
--- PASS
PASS
ok  	github.com/shiyanYX/opencode2api	0.075s
```

**测试覆盖的验收点**：

| 验收点 | 用例 |
| --- | --- |
| 已映射不受影响（含 alias） | `TestUnmappedPredicateStructuralContract` 2) |
| `-free` 反查 / 改名后的显示名 | `TestUnmappedPredicateStructuralContract` 3) |
| 空目录（冷启动）不误判 | `TestUnmappedPredicateStructuralContract` 4) |
| 未映射不聚合但仍落盘 | `TestUnmappedNotAggregatedButStillLogged` |
| 回填与 append 口径一致 | `TestUnmappedBackfillMatchesAppend` |
| 成功率分母正确 | `TestUnmappedExcludedFromModelStatsAndDenominator` |
| 新 `/api/stats` 字段存在且类型正确 | `TestStatsRangeResponseExposesUnmappedRequests` |
| Top-N + `__other__` 边界与确定性 | `TestTopUnmappedModelsTopNAndOther` |
| JSON 往返 + 老行兼容 | `TestCallRecordUnmappedJSONRoundTrip` |
| 老行重算 vs 落盘判定优先 | `TestLegacyRecordsRecomputedButPersistedJudgmentWins` |
| 判定函数**无刷新副作用**（不写盘、不打网络） | `TestIsUnmappedModelHasNoRefreshSideEffect` |
| 请求路径**保留**按需刷新语义（未被误删） | `TestEnsureFreeModelsFreshStalenessPredicate` |
| **不静默丢弃**：未映射必打 WARN，且按模型限流 | `TestUnmappedModelWarnsThrottledPerModel` |

### 6.3 全量套件

```
$ go test -count=1 ./
ok  	github.com/shiyanYX/opencode2api	41.136s

$ go test -race -count=1 ./
ok  	github.com/shiyanYX/opencode2api	275.468s
```

两次全量跑完后 `free_models_cache.json` 的 md5 **逐字节未变**
（`md5sum -c` 通过）—— 这是上面纯度重构的直接验收：测试不再污染工作区。

> 说明：`task8_bucket_query_test.go` 全程**保持移开**（按本仓库既定状态放在
> `/tmp/t8_hold.go`，md5 `24073da1…`，17713 B）。该文件引用了已删除的
> `forEachBucketedCallRecord`，是**既有**编译错误（基线即失败），与本次改动无关。
> 任务书要求「保持移开状态，不要动它」，故收尾**未**放回仓库 —— 放回会让
> 任何人的全量测试都无法编译。

### 6.4 真实数据端到端（走真实回填路径 + 真实 `/api/stats` 内核）

```
===== B. 真实回填（109153292 字节，耗时 462.98ms）=====
-- hourBuckets: 812 桶 / ΣN=203576
-- unmappedBuckets: 16 桶 / ΣN=661
-- absorbed=204237 unabsorbed=0 ⇒ 守恒 true（ΣN=204237）
-- /api/stats?range=180d:
     total_requests      = 203576
     len(models)         = 13
     unmapped_requests.count = 661
     unmapped_requests.by_model = {claude-opus-4-5=4, gpt-4o-mini=1, hy3=2, mimo-v2.5=624, x-preview-f=30}
-- 修复前：total=204237（含未映射）  修复后：total=203576
-- 分母减少 661 条 (0.3236%)
-- 已映射 ok=187184（unmapped 全部失败，故分子不变）
-- 成功率：修复前 91.6504% → 修复后 91.9480%
-- 校验：hourBuckets ΣN=203576 vs total_requests=203576（应相等）
-- 校验：unmappedBuckets ΣN=661 vs unmapped_requests.count=661（应相等）
```

---

## 7. 新 API 字段形状

`GET /api/stats?range=today|7d|30d|180d` 新增**顶层**字段：

```json
"unmapped_requests": {
  "count": 661,
  "by_model": {
    "mimo-v2.5": 624,
    "x-preview-f": 30,
    "claude-opus-4-5": 4,
    "hy3": 2,
    "gpt-4o-mini": 1
  }
}
```

约定（task-22 面板按此实现）：

- **`count` 是窗口内口径**，与同层 `total_requests` / `models` **共用同一份
  `ts/bidx`**（`statsRangeResponseFor` 只取一次窗口后传给三个折叠内核）。
- **`by_model` 给前 10 个 + 其余归 `"__other__"`**（`unmappedTopN = 10`）。
  排序是**全序**：条数降序，条数相同按模型名升序 —— 否则并列时 Top-N 边界
  不稳定，同一次查询连发两次可能得到不同的 `by_model`。
- **对象而非裸数字**：旧后端没有该字段，面板据此显示 `—` 而不是 `0`。
  判空方式：`if (!d.unmapped_requests) { 显示 "—" }`。
- **只在 `?range=` 路径出现**，不在不带 range 的路径：后者有逐字节向后兼容
  红线（`TestAdminStatsHandlerWithoutRangeIsByteIdentical` 把空响应钉成
  `{"total_requests":0,"models":{}}`）；且本指标语义本来就是窗口内的。
- **未映射请求不在** `total_requests` / `models` 里 —— 这正是修复的目的。

---

## 8. 残留风险与未决项

### R1. 老记录重算的结果**随日期漂移**（Lead 条件 2 要求写明）

回填对**没有 `unmapped` 键**的老行用当前免费表重算。因此：

> **同一份历史数据，在不同日期重算可能得出不同结论。**

原因：`isUnmappedModel` 依赖 `isFreeModel` → `freeModelDocsCache`，而该缓存由
`refreshFreeModelsDocs()` 定期从上游文档页刷新，**内容会变**（上游会增删模型）。

实测漂移敏感性：用「今天（13 条）」与「10-17 采集（14 条）」两份免费表
对同一份 204,237 行数据重算，**本次 0 个模型翻转** —— 但这是**运气**，不是保证。
一旦上游把某个 `-free` 之外的免费模型（如 `big-pickle`，靠文档表而非 `-free`
后缀判定）从文档里摘掉，老数据重算就会把它追认成未映射，把它的成功记录
从统计里删掉。

**缓解**：① 新构建写的行总是带 `unmapped` 键 ⇒ 采信落盘值、永不重算，
重算面随时间单调收缩；② `-free` 后缀这条判据不依赖任何外部数据，是稳定兜底。
**未根治**：要根治需把判定冻结进日志（已是新行的做法），老行只能重算。

### R2. `-free` 后缀是**约定**，不是上游事实

若上游将来推出一个**不以 `-free` 结尾**、也不在文档免费表里的免费模型，
它会被判 unmapped 而从统计里消失。**当前无此实例**（生产 18 个模型名全部
落在「`-free` 后缀」或「文档免费表」或「确实不可服务」三类里）。
监控方式：`unmapped_requests.by_model` 若出现某个**有成功记录**的模型，
即触发告警 —— 那说明判据过严。**这就是硬门禁的线上版本。**

### R3. 三个 `-free` 模型 0 成功但**不排除**（刻意保守）

`ox-alpha-free`(3)、`muse-spark-1.2-contributor-free`(2)、
`muse-spark-1.3-contributor-free`(1) 共 6 条，全部失败。它们符合 `-free`
命名约定，属于「曾经可服务、现已下线」，**不是**「客户端传了野名字」。
不排除它们会保留 6 条失败在分母里（影响约 0.003 pp）。
**理由**：若按成败反推可服务性，就会把真实的服务故障伪装成「模型不存在」，
掩盖问题。这是刻意的取舍。

### R4. 不带 range 的 `/api/stats` 路径**不含**该字段

那条路径读的是内存累计计数器 `tokenStats`（落 `stats.json`），与小时桶是
两套数据。本次**未**改动它：① 面板对该路径零读；② 加字段即持久化 schema 变更。
⇒ 若将来有消费者从无 range 路径读未映射数，会拿不到。

### R5. 单快照缺失（既有问题，非本次引入）

`statsRangeResponseFor` 共享的是**窗口**而不是**快照**：三个折叠内核各自取
RLock 读桶，中间落进一次 `append` 仍会让各字段对不上。这是既有延后决策，
本次沿袭，未加剧（未映射统计同样共享窗口，不共享快照）。

### R6. `resolveModel` 的 `-free` 反查依赖上游目录（**独立 bug，未修**）

见 §3.4 第 6 点。裸名 `mimo-v2.5` 无法映射到 `mimo-v2.5-free`，因为反查查的是
上游目录而非命名约定。这是**路由**问题，与统计口径无关，建议单独立项。

### R7. 判定函数曾带副作用，测试一度改写仓库文件（**已修**）

实现过程中发现并修掉的真缺陷（详见 §4.1 末段）：`isUnmappedModel` 最初直接
调 `isFreeModel`，后者在缓存陈旧时会 `go refreshFreeModelsDocs()` —— 回填
20 万行老记录时会派发网络请求并 `saveFreeModelsCache` **改写仓库里的
`free_models_cache.json`**（14 条 / 413 字节，原为 13 条 / 403 字节）。
该文件被 `.gitignore:6` 忽略，`git checkout` 还原不了，只能手工恢复。

修复：判定走纯内核 `resolveModelWithDocs` + 显式快照 `isFreeModelIn`，
刷新只留在请求路径的 `resolveModel`。验收方式是把该文件的 md5 在跑测前后
对比 —— 现在全量 `go test` 与 `go test -race` 跑完 md5 均不变。

**教训**：任何「纯判定」函数都不允许隐式触发 I/O；测试若会写工作区文件，
本身就是缺陷信号。

---

## 9. 附录：复现命令

```bash
cd /home/jingyx/project/opencode2api-test
export PATH=$PATH:/usr/local/go/bin \
       GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"

# 1) 单元测试（含 TDD 全部验收点）
go test -run 'TestUnmapped|TestStatsRangeResponseExposes|TestCallRecordUnmapped|TestTopUnmapped|TestLegacyRecords|TestIsUnmappedModelHasNoRefreshSideEffect|TestEnsureFreeModelsFreshStalenessPredicate' -count=1 -v .
# 注意 TestUnmapped 前缀已覆盖 TestUnmappedModelWarnsThrottledPerModel

# 2) 全量套件
go test -count=1 ./
go test -race -count=1 ./

# 3) 生产数据证据（需临时恢复 zz_t21_evidence_test.go；报告定稿后已删除该文件，
#    其数字见 §3 与 §6.4）
# go test -run 'TestT21Evidence' -count=1 -v .

# 4) 上游目录快照（用于复核 A 方案的反例）
export HTTP_PROXY=http://192.168.1.3:7890 HTTPS_PROXY=http://192.168.1.3:7890
curl -s https://opencode.ai/zen/v1/models    -o /tmp/v403/zen.json   # 85 条
curl -s https://opencode.ai/zen/go/v1/models -o /tmp/v403/go.json    # 37 条
```

**证据文件**：
- `/tmp/prod-now.jsonl` —— 204,237 行生产日志（只读）
- `/tmp/v403/zen.json`、`/tmp/v403/go.json` —— 上游目录快照
- `zz_t21_evidence_test.go` —— 证据测试（**临时，已删除**；其产出的数字已固化在 §3 / §6.4，重跑需从 `docs/research/` 的历史版本取回该文件）
- `unmapped_stats_test.go` —— 验收测试（**保留**，回归防线）

---

## 10. 措辞纪律（Lead 条件 4）

- 本次 +0.2976 pp **是口径更正，不是服务质量提升**。
- 排除的是我们**根本没有提供**的模型。
- 用户诉求「面板别显示假数字」⇒ 排除从未提供的模型让数字**变诚实**，
  不是变好看。
- **不得**在 ROADMAP / 任何战果里把它算进「成功率提升」。
