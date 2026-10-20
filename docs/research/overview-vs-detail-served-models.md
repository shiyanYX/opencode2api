# 概览 vs 详细层：`everServed` 判据 + 展示层 `ok>0` 过滤（task-39 交付报告）

> 状态：**实现完成，待独立验收（Part B）**。交付后本 agent 停手，不再改 `main.go` / `calllog.go` / `statscan.go`。
>
> 代码 pin（sha256，交付态）：
>
> | 文件 | sha256 | 说明 |
> | --- | --- | --- |
> | `main.go` | `f5c971b02d0ab0481c4b892a6e70197943e6a4c6c3d33b83b8ef4e3c57125b82` | 判据拆分 + 两个出口的过滤 |
> | `calllog.go` | `6ee8ed5952fef8bdb00d3584fc82ff6f02dd6643853d3aa8b23eec3d2a97c4d6` | everServed 生命周期 + 回填 + 过滤 helper |
> | `statscan.go` | `47a066beabad1bea8653a6946f24880e118d892ecc63ff28de324b1b694201cb` | **未改**（与 Lead 记录的 pin 相同） |
> | `static/admin.html` | md5 持续变化（我最后一次观察：`374ba326…` → `c05fd2e0…` → `4fe4f101…`） | **本 agent 未碰**。交付窗口里该文件正被另一任务（U3 面板重写）连续改写，md5 的变化源不是我（证据见 §4.3 的 T29 说明） |
>
> 数据快照：`/data/call_log.jsonl` **233,152 行**（2026-10-20 09:2x 从生产容器 `docker cp` 抓取），
> 免费表缓存 `/data/free_models_cache.json` 14 条。生产日志持续增长，后文所有绝对数字都绑定这份快照；
> 复算命令见 §8，换快照只需换环境变量。

---

## 0. 一页结论

1. **判据升级**：概览口径从「公式 B」升级为
   **`mapped = everServed(解析后的名字) || 公式B`**，
   其中 `everServed(name)` = 「历史任何一条 `status=ok` 记录里出现过的模型名」。
   效果：**曾经成功过的模型永远留在概览**（哪怕上游目录已把它摘掉、哪怕历史记录是乱序的）。
2. **展示层过滤（F1，Lead 追加裁决）**：`/api/stats?range=` 的 `models` / `trends_by_model`
   与 `/api/stats/trends?group=model` 里，**窗口内 `OK == 0` 的模型键一律不输出**
   （后端做，面板零改动）。**分母/成功率/`total_requests` 一律不动** ——
   被剔除的 6 个模型 / 15 条请求是**真实免费模型的真实失败**，留在分母里。
3. **当前快照上的真实数字**（命令见 §8.1）：
   - 详细层：233,152 条；
   - 概览分母：`total_requests = 231,761`、`ok = 208,141`、成功率 **89.808466%**；
   - 未映射（everServed ∪ 公式B 的排除集）：**1,391 条，ok = 0**；
   - 展示层过滤掉：**6 个模型 / 15 条**（0 token、0 流式样本），过滤后 `models` 键数 **22 → 16**；
   - `total_requests` 与成功率**过滤前后逐位相同**（89.808466% → 89.808466%）。
4. **everServed 在今天的快照上改变 0 条记录**：所有 `ok>0` 的名字要么带 `-free`、要么仍在目录里
   （唯一裸名 `big-pickle` 11/11 成功且仍在 docs）。它的价值是**堵住未来退化**：
   测试 b 证明「上游把 `big-pickle` 从目录摘掉」时公式 B 会逐出 11 条成功记录，而 everServed 不会。
   **诚实的结论：这是防退化，不是今天的数字修正。**
5. 测试：`go test -count=1 ./` 仅剩 2 个**他人**失败（`TestT30GateNowJudgesFixedSubsetDead`、
   `TestT30CriterionCPoolEmptyReachability`，task-34 在改，A/B 基线证明与本任务无关）；
   `-race` 结果见 §4.3；变异测试 **12 条全红**（§5）。
6. F1 是**已批准的语义变更 / 口径更正**，不是「服务质量提升」：我们没有让任何失败请求变成功，
   只是把「窗口内从未成功过的模型」从**展示层**拿掉，分母一条未动。
7. 测试结论（交付态）：`go test -count=1 ./` 只剩 2 个**他人**失败（`TestT30GateNowJudgesFixedSubsetDead`、
   `TestT30CriterionCPoolEmptyReachability`，task-34 在改）；另有 `TestT29PlainIsByteIdenticalToEmbeddedPanel`
   在**全量跑**时偶发失败，原因是**他人正在改写 `static/admin.html`**（每次失败信息里磁盘/内嵌两枚 md5 都在变），
   与 task-39 无关；单独 fresh-build 跑 T29 全绿（含 `-race`）。详见 §4.3。

---

## 1. 用户裁决与口径定义

### 1.1 三条裁决（按时间）

| # | 裁决 | 原文/要点 | 本任务的落点 |
| --- | --- | --- | --- |
| R1 | 不做自动降级 | 「使用下架模型是配置的问题，不是我们的问题，不需要修」 | 网关只如实转发；本报告不涉及修复上游 410/400 |
| R2 | 概览只显示有成功的模型，详细层记录全部 | 「概览的图表啥的只显示有成功的模型，至于更详细的统计才记录全部请求」 | §2.1 判据升级 + §6 展示层过滤 |
| R3 | 展示层过滤放后端、不动面板 | Lead F1 裁决：并集做分母 + 展示层 `ok>0` 过滤 | §2.7（两个出口共用函数） |

### 1.2 判据

```
everServed(name) := ∃ 记录 r: r.model == name ∧ r.status == "ok"      // 单调：只增不减
公式B(name)      := !isFreeModelIn(docs, resolveModel(name)) ∧ !strings.HasSuffix(resolveModel(name), "-free")
mapped(name)     := everServed(name) || 公式B(name)
```

- 空模型名恒 `mapped`（它不是一次模型请求，见 `isUnmappedModel` 首段）；
- **文档免费表不可用（空）⇒ 一律 `mapped`**（安全闸门，宁可漏排除不可误排除）；
- `everServed` 的键是**落盘记录里的原始模型名**（`CallRecord.Model`），不做 resolve ——
  与概览的模型键完全同源，避免「判定用 A 名字、展示用 B 名字」的口径分叉。

### 1.3 三层数据的关系（这是全部争议的根源）

| 层 | 数据源 | 包含 | 观众 |
| --- | --- | --- | --- |
| 分母（成功率） | `hourBuckets`（窗口折叠） | 所有 `mapped` 的请求（**含 0 成功模型**） | `total_requests`、成功率 |
| 展示层（图表） | 同一个 `hourBuckets` + **`ok>0` 过滤** | 仅窗口内**有过成功**的模型键 | 面板的甜甜圈/表格/趋势图 |
| 详细层 | `call_log.jsonl` / `/api/call-log` / `/api/logs` | **全部请求**（未映射的带 `unmapped:true` + WARN） | 排障 |

---

## 2. 实现：改了什么

### 2.1 `main.go`

| 位置（交付态行号） | 改动 | 说明 |
| --- | --- | --- |
| `main.go:1468` | `isUnmappedModel` 改为**组合入口** | 空名 → `false`；`isServedModel(m)` → `false`；否则 `isUnmappedByFormula(m)` |
| `main.go:1532` | 新增 `isUnmappedByFormula` | 原公式 B 函数体**原样搬移**（含全部注释），判定路径不再直调；只给回填用 |
| `main.go:7439-7445` | `/api/stats?range=` 出口过滤 | `keep := modelsWithOKInWindow(ts,bidx)` → 删 `models` 的键 → `filterTrendsByOK(out.TrendsByModel, keep)` |
| `main.go:7516` | `/api/stats/trends?group=model` 同口径 | 内联 `ensureBucketsUpToDate()+trendBuckets(rng)`，**与 `trendsByModelWithBuckets` 共用同一份 ts/bidx**，再走同一个 `filterTrendsByOK` |
| 非模型分支（热力图） | **零改动** | `trendsFromCallLog(rng)` 原样返回；加了注释说明「日总量不许按模型筛，那是藏失败」 |

> 判据拆分是必要的：回填路径必须**只用公式 B 做第一层判定**（把候选行暂存起来），
> 等整轮扫完 `everServed` 收齐后再裁决 —— 否则就是「先来先判」，行序会改变结果。

### 2.2 `calllog.go`：everServed 的存储与重建（一句话版）

**`everServed` 是进程内一个 `map[string]struct{}`（`calllog.go:274`，受 `callLog.mu` 保护），
与小时桶由同一份 `call_log.jsonl` 全量回填**：回填时把 `status=ok` 的模型名收进 `stagedServed`，
与 `hourBuckets`/`unmappedBuckets` **在同一次锁内提交**；启动时另外从尾部 2000 条记录播种一次
（覆盖「懒回填还没跑」的窗口）；实时请求成功时在 `finish()` 里即时记入；`clear()` 清空。

### 2.3 回填管线（`absorbOnce`，`calllog.go:729-930`）

```
读 [from,size) 快照 → 逐行 parseStatsLine → slimRec
  ├─ 新格式行（有 unmapped 键，SawUnmapped=true）：落盘判定权威
  │    └─ status==ok 且落盘 unmapped=true ⇒ 翻成 mapped（成功即证明可服务）
  ├─ 老格式行（无 unmapped 键）：公式 B 判未映射的 → **先塞 pendingServed 暂存**
  │    （不立即定性：此行的最终归属取决于整轮里有没有同名成功记录）
  └─ 所有 status=ok 的模型名 → stagedServed
整轮扫完：
  ever := servedModelsSnapshot(stagedServed)        // 全局 ∪ 本轮
  pendingServed 里的行：served(ever) → staged      // 曾经成功过 ⇒ 计入概览
                      否则        → stagedUnmapped
锁内一次性提交：parsedTo / 两张桶表 / stagedServed 并进全局 servedModels
```

- **行序无关**：实测本快照里「失败出现在同名第一次成功之前」的记录有 **5 条**
  （占全部失败 25,011 条的 0.02%，命令见 §8.2）。数量小但后果不小：先来先判会让这 5 条失败
  被**永久**逐出概览（它们当时确实意味着「这个模型还没成功过」），而且这类顺序在
  「下架 → 恢复」窗口里必然出现。暂存 + 整轮裁决是唯一能保证「同一份日志 ⇒ 同一答案」的写法
  （变异 M12「先来先判」会红，见 §5）。
- **与桶守恒同锁提交**：`ΣN(两张桶) == absorbedRecords - unabsorbedRecords` 这条不变量不受影响
  （`stagedServed` 只加不减，且在同一次 `callLog.mu` 临界区内合并）。
- `mergeAggTo` 仍是唯一的桶合并点；`stagedServed` 的合并紧随其后（`calllog.go:910`）。

### 2.4 实时路径（`finish()`，`calllog.go:1917-1925`）

- 2xx：先把 `cr.rec.Unmapped` 翻成 `false`（**落盘记录不得再写 `unmapped:true`**），
  解锁后在 `callLog.append(rec)` **之前** `markServedModel(rec.Model)`。
- 为什么先记账后落盘：记账与落盘之间若进程崩溃，重放日志的回填能补上；
  反过来先落盘后记账，下一条同名请求会在「它已成功过」之前被冻结成 unmapped。

### 2.5 启动播种（`loadCallLogFromFile`，`calllog.go:1440-1456`）

在加载尾部 2000 条记录的同一临界区里，把这些记录里 `status=ok` 的模型名并入 `servedModels`。
作用：冷启动后**懒回填尚未发生**的那一小段时间里，判定也已经是「曾经成功过」的正确判定。

### 2.6 `clear()`（`calllog.go:1713-1718`）

`servedModels = nil`。语义：用户显式清空调用日志 = 丢弃整段历史，
`everServed` 是「这段历史里成功过的名字」，必须同步归零；
否则新日志里一个从没成功过的名字会因为**上一轮历史**而被当成「我们的模型」计入概览。
这与单调性不冲突：单调性说的是「同一份历史内只增不减」。

### 2.7 F1 过滤（两个出口共用一套判据）

| 位置 | 复用 | 说明 |
| --- | --- | --- |
| `calllog.go:1194` | `modelsWithOKInWindow(ts,bidx)` | 一次 RLock 扫 `hourBuckets`，只取 `OK>0` 且在窗口内的模型名。**桶里现成字段，不新增采集、不改桶结构** |
| `calllog.go:1215` | `filterTrendsByOK(pts, keep)` | 从 `[]ModelTrendPoint` 里删掉不在 keep 的模型行，**两个出口都调它** |
| `main.go:7439` | `/api/stats?range=` | 删 `models` 键 + `trends_by_model` 行 |
| `main.go:7516` | `/api/stats/trends?group=model` | 同一 keep 集合 → 同一 `filterTrendsByOK` |
| 无 range 的 `GET /api/stats` | **完全没进过滤函数** | 逐字节不变，§6.4 有 sha256 证据 |
| `/api/stats/trends?range=180d`（热力图） | **完全没进过滤函数** | 返回日总量（无 model 键），逐字节不变，§6.4 有证据 |

### 2.8 动过的测试文件（全部 gitignored，不进提交）

| 文件 | 改动 |
| --- | --- |
| `zz_t39_served_models_test.go` | **新增**（本任务全部 TDD 用例，9 个 Test + 真日志子用例） |
| `unmapped_stats_test.go` | 修测试隔离：`t21IsolateCallLog` 快照/还原 `servedModels`；把「冻结判定优先」用例里的 ok 行换成独立模型名（否则新语义下会与同名失败行互相影响） |
| `stats_window_test.go` | 契约同步：`trends_by_model` 行数 14 → 7，并新增「m2（0 成功）不得出现在 models 里」断言 |

---

## 3. 单调性（Lead 必答题）

### 3.1 `everServed` 本身：严格单调

三个写入点**只增不删**：`markServedModel`（实时）、`stagedServed` 合并（回填）、启动播种。
唯一的清空点是 `clear()`（用户显式重置历史），不是数据驱动的删除。
⇒ 「同一份历史内，一个名字一旦进过 everServed，就永远在」**严格成立**。

### 3.2 组合判据：**不严格单调**，且这是无法避免的（诚实结论）

`mapped = everServed || 公式B`，公式 B 依赖会漂移的 docs 表。因此存在一种翻转：

```
某名字从未成功过（everServed=false）
  今天 docs 里有它（或带 -free 后缀）            ⇒ mapped
  明天上游把它从 docs 摘掉且不带 -free 后缀      ⇒ unmapped    ← mapped → unmapped
```

这个翻转**正是用户要的口径**（「只显示有成功的模型」：从没成功过的名字失去目录保护后就该退出概览）。
要让它严格单调，只能把「从未成功但曾判过 mapped」的名字也永久钉住 —— 那就等于回到
「概览里保留 0 成功模型」，与 R2/F1 裁决直接冲突。**所以：严格单调性不可兼得，我选择与裁决一致。**

配套要区分的是**另一个方向**：`mapped → unmapped` **绝不会**发生在「曾经成功过」的名字上
（`everServed` 一旦为真，组合判据恒为真）。这条由测试 T3/T4 与变异 M1/M2/M3/M5 共同盯住。

### 3.3 方向表

| 场景 | 结果 | 是否符合裁决 |
| --- | --- | --- |
| 曾经成功，之后下架、之后一直失败 | **永远 mapped**；新失败计入分母 | ✅ 用户明确要的 |
| 只要 1 条成功、4,999 条失败 | 留在展示层与分母 | ✅ F1 用例 2 |
| 从未成功、目录里有它 | mapped（展示层被 `ok>0` 过滤掉） | ✅ F1 |
| 从未成功、目录把它摘掉 | unmapped（mapped→unmapped 合法翻转） | ✅ 用户要的语义 |
| 从未成功、只有失败 | 不进概览，全量留在详细层 | ✅ |
| 日志不可读 / 空 | everServed 空 ⇒ 退回公式 B（再空 ⇒ 全 mapped） | ✅ 安全降级 |

---

## 4. Red → Green 证据

> 环境变量（所有命令都必须带，否则真日志用例自动 skip）：
>
> ```bash
> export PATH=$PATH:/usr/local/go/bin GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
> export T39_LOG=/tmp/t39/call_log.jsonl T39_DOCS=/tmp/t39/docs_cache.json
> ```

### 4.1 交付前（红）

**第一轮（实现尚不存在时，用真实 failing 用例。）**

| 用例 | 红输出（原样） |
| --- | --- |
| T1 裸名历史保留 | `概览口径错了：big-pickle 曾经成功过 ⇒ 它的记录必须进 hourBuckets，实际 N=0 OK=0` |
| T3 成功翻转落盘标记 | `成功的请求不得在落盘记录里仍标记 unmapped` |
| T5a everServed 收集 | `big-pickle 历史上成功过，必须进 everServed` |
| T5b 排除集里不得有成功记录 | `新判据的排除集里出现了 11 条成功记录 …（map[big-pickle:[11 11] …]）` |
| F1 过滤（两个出口） | `0 成功模型出现在 /api/stats/trends?group=model 里`、`窗口内 0 成功的模型不得出现在 models 里` |

**第二轮（用「短路判据」模拟交付前行为，可复现。）** 把 everServed 短路（等价于公式 B 口径）
⇒ `TestT39EverServedBareNameKeepsHistoryInOverview` / `TestT39LiveSuccessFlipsRecordAndIsMonotone` 红；
把 F1 过滤整体短路 ⇒ `TestT39OverviewHidesZeroSuccessModels` 红。输出见 §5 的 M1/M3/M6/M10 行。

### 4.2 交付后（绿，完整输出）

```
$ go test -run 'TestT39' -count=1 -v .
=== RUN   TestT39EverServedBareNameKeepsHistoryInOverview
--- PASS  (0.00s)
=== RUN   TestT39NeverServedNameStaysOutOfOverview
--- PASS  (0.00s)
=== RUN   TestT39LiveSuccessFlipsRecordAndIsMonotone
--- PASS  (0.00s)
=== RUN   TestT39DegradesSafelyWhenLogUnreadable
--- PASS  (0.00s)
=== RUN   TestT39OrderIndependenceAcrossRestart
    success_first / success_middle / success_last   --- PASS
=== RUN   TestT39RealLogOverviewCriterion
    a_real_docs_snapshot: 真日志 233152 条：概览 total=231761 ok=208141 (89.81%) 未映射=1391 ok=0 回填 702ms
    c_f1_zero_success_models_hidden_but_denominator_intact:
        窗口内 0 成功的模型 6 个 / 15 条：[beam-free(2条) jev-1.13-free(1条)
          muse-spark-1.2-contributor-free(2条) muse-spark-1.3-contributor-free(1条)
          nemotron-3.5-super-vl-free(6条) ox-alpha-free(3条)]
        被剔除模型的 token 合计：prompt=0 completion=0 cache_read=0 cache_creation=0 流式=0
        顶层吞吐不变：throughput_tps=77.87946233884321 stream_tps_n=39914
        窗口内 total=231761 ok=208141 成功率=89.808466%
        过滤后：total_requests=231761（不变）成功率=89.808466%（不变）models 键数 16
        两个出口一致：16 个模型（/api/stats models 与 trends?group=model 的模型集相同）
        /api/stats/trends（热力图）逐字节不变：26261 字节
    b_upstream_drops_bare_name: 摘掉 big-pickle 后：概览 total=231761 ok=208141 未映射=1391；big-pickle 在概览里 ok=11
--- PASS  TestT39RealLogOverviewCriterion (2.06s)
=== RUN   TestT39OverviewHidesZeroSuccessModels / ...KeepsModelWithSingleSuccessAmongManyFails (5000 条)
              / ...FilterKeepsTotalsAndRateIntact / ...EmptyAndAllFailWindowsSafe
              / TestT39OnlyModelsAndTrendsDifferFromUnfiltered / TestT39TrendsEndpointSameCriterion
--- PASS
ok      github.com/shiyanYX/opencode2api
```

### 4.3 全量测试与 `-race`

```
$ go test -count=1 ./
--- FAIL: TestT30GateNowJudgesFixedSubsetDead     ← 他人（task-34 在改）
--- FAIL: TestT30CriterionCPoolEmptyReachability  ← 他人
（其余全部 PASS；A/B 基线：把我的测试文件移开后同样是这 2 个失败）
```

**交付态实测（原始日志 `/tmp/t39/final_full4.log`、`final_race_t39.log`、`final_race_full.log`）：**

```
# ① 全量（T29 因他人改写面板产生「陈旧 embed」竞态，先看 ②）
$ go test -count=1 ./
--- FAIL: TestT29PlainIsByteIdenticalToEmbeddedPanel (0.01s)   ← 他人改 static/admin.html 的竞态，非本任务
--- FAIL: TestT30GateNowJudgesFixedSubsetDead (0.12s)          ← 他人（task-34）
--- FAIL: TestT30CriterionCPoolEmptyReachability (0.01s)       ← 他人（task-34）
FAIL    github.com/shiyanYX/opencode2api   144.057s

# ② 全量、只排除上面那条「文件竞态」用例 ⇒ 只剩 2 个他人失败
$ go test -count=1 -skip 'TestT29PlainIsByteIdenticalToEmbeddedPanel' ./
--- FAIL: TestT30GateNowJudgesFixedSubsetDead (0.16s)
--- FAIL: TestT30CriterionCPoolEmptyReachability (0.01s)
FAIL    github.com/shiyanYX/opencode2api   142.354s

# ③ T29 的失败是「编译后磁盘文件又被改了」，不是代码问题：
$ go test -run TestT29 -count=1 .            → ok 0.378s（fresh build，内嵌==磁盘）
$ go test -race -run TestT29 -count=1 .      → ok 2.697s

# ④ -race 定向（本任务 + 受影响的旧用例）
$ go test -race -run 'TestT39|TestLegacyRecords|TestT23|TestAdminStats|TestTrendsByModel|TestBucket' -count=1 .
ok      github.com/shiyanYX/opencode2api   188.554s      ← 无 DATA RACE

# ⑤ -race 全量（跳过他人的长探测用例）
$ go test -race -count=1 -skip 'TestT30ProbeCauseAttribution' -timeout 35m ./
FAIL    github.com/shiyanYX/opencode2api   482.406s
  --- FAIL: TestT29PlainIsByteIdenticalToEmbeddedPanel   ← 与 ③ 同一竞态（md5 又变了）
  --- FAIL: TestT30GateNowJudgesFixedSubsetDead          ← 他人
  --- FAIL: TestT30CriterionCPoolEmptyReachability       ← 他人
  （无 DATA RACE 报告）
```

> A/B 基线：把我的 `zz_t39_served_models_test.go` 移开跑全量，失败集合同样只有 T30×2（09:20 那次 T29 还是绿的，
> 说明 T29 的偶发失败开始于「他人开始改写面板」之后）。

### 4.4 四个必答场景（逐条）

| 场景 | 用例 | 断言（绿） |
| --- | --- | --- |
| 裸名有过成功，不得被误判未映射 | `TestT39EverServedBareNameKeepsHistoryInOverview` + 真日志 b | `big-pickle` 2 条记录都进 `hourBuckets`；把 docs 里的 `big-pickle` 摘掉后它的 11 条成功仍在概览 |
| 从未成功过的名字不进概览 | `TestT39NeverServedNameStaysOutOfOverview` + 真日志 a/c | 1,391 条排除集 `ok=0`；6 个 0 成功模型在 `models`/`trends_by_model`/`trends?group=model` 里都不出现 |
| 跨重启一致（数字不跳变） | `TestT39OrderIndependenceAcrossRestart`（行序 3 变体 × 两轮全量重扫） | 三种行序、两轮重扫结果都是 `mappedN/OK/unmappedN = 3/1/0` |
| 日志不可读时安全降级 | `TestT39DegradesSafelyWhenLogUnreadable` | 读不到日志 ⇒ everServed 空 ⇒ 退回公式 B；docs 也空 ⇒ 一律 mapped（不误删） |

### 4.5 真日志三口径复算（task-23 的独立验收口径）

```
公式 B 排除集（task-21）          1,391 条 ok=0
everServed ∪ 公式B（本任务）      1,391 条 ok=0      ← 差集为空
被 everServed 救回的成功记录      0 条（本快照）
```

逐模型（全部 `ok=0`，命令见 §8.2）：

| 模型 | 条数 | 归属 |
| --- | --- | --- |
| `space-bunny` | 728 | 公式 B（裸名，目录里只有 `space-bunny-free`） |
| `mimo-v2.5` | 624 | 公式 B |
| `x-preview-f` | 30 | 公式 B |
| `claude-opus-4-5` | 4 | 公式 B |
| `hy3` | 2 | 公式 B |
| `gpt-4o-mini` | 1 | 公式 B |
| `probe-143148` | 1 | 公式 B |
| `probeL-143230` | 1 | 公式 B |
| **合计** | **1,391** | `ok = 0`（硬闸门） |

> 与 task-23 的 661 差异：那是 204,237 行旧快照上的数字（661 = 1,391 − 728 − 2 探测名）。
> 现在日志已 233,152 行，`space-bunny` 从 727 涨到 728。**数字不同是快照不同，不是判据变了。**

---

## 5. 变异测试（12 条，全部红；还原后绿）

命令：每轮把一处实现替换成错误版本 → 跑对应用例（期望红）→ 还原 → 全绿。
脚本与产物：`/tmp/t39mut/mutate2.py`（+ 手工补的 M12）。

| # | 变异 | 攻的用例 | 红输出（首个断言） |
| --- | --- | --- | --- |
| M1 | 去掉 `isUnmappedModel` 的 everServed 短路（退化成公式 B） | `TestT39LiveSuccess…` | `曾经成功过就必须永远算已映射（单调性被破坏）` |
| M2 | `isServedModel` 恒真 | `TestT39Degrades…` / `LiveSuccess` | `everServed 不可用时必须退回公式 B 判定` |
| M3 | 回填不收集 `stagedServed` | `TestT39EverServedBareName…` | `概览口径错了：…实际 N=0 OK=0` |
| M4 | 成功时不翻转落盘 `unmapped` | `TestT39LiveSuccess…` | `成功的请求不得在落盘记录里仍标记 unmapped` |
| M5 | `finish()` 不记 everServed | `TestT39LiveSuccess…` | `成功之后必须进 everServed` |
| M6 | `/api/stats` 完全不筛（恒真） | `HidesZeroSuccessModels` / `OnlyModelsAndTrendsDiffer` | `窗口内 0 成功的模型不得出现在 models 里` |
| M7 | 过滤恒假（全筛） | `KeepsModelWithSingleSuccess…` | `有 1 条成功的模型必须出现（哪怕 4999 条失败）` |
| M8 | 判据错成 `N>0` | `HidesZeroSuccessModels` | `…不得出现在 models 里` |
| M9 | 把过滤也套到 `total_requests`（藏失败） | `FilterKeepsTotalsAndRateIntact` / `Hides…` | `total_requests=4，期望 10（0 成功模型的失败必须留在分母）` |
| M10 | **只删 models、漏 `/api/stats/trends`**（半实现） | `TrendsEndpointSameCriterion` | `--- FAIL: TestT39TrendsEndpointSameCriterion` |
| M11 | `filterTrendsByOK` 恒真 | `TrendsEndpointSameCriterion` / `OnlyModels…` | `…还有别的差异（首个差异字节 30390 / 期望 len=30438 实际 len=61758）` |
| M12 | 回填「先来先判」（不暂存 pendingServed） | `OrderIndependenceAcrossRestart` | `第 1 轮：mappedN/OK/unmappedN=[2 1 1]，期望 [3 1 0]` / `[1 1 2]` |

> M12 是最关键的一条：它证明「行序无关」不是靠运气 —— 本快照实测有 **5 条**失败记录早于同名首次成功
> （§8.2 命令），先来先判会把这 5 条永久逐出概览。

---

## 6. F1 精算清单（Lead 必答）

### 6.1 被剔除的模型（窗口 180d = 全部历史）

| 模型 | 请求数 | ok | tokens | 流式样本 |
| --- | --- | --- | --- | --- |
| `nemotron-3.5-super-vl-free` | 6 | 0 | 0 | 0 |
| `ox-alpha-free` | 3 | 0 | 0 | 0 |
| `muse-spark-1.2-contributor-free` | 2 | 0 | 0 | 0 |
| `beam-free` | 2 | 0 | 0 | 0 |
| `muse-spark-1.3-contributor-free` | 1 | 0 | 0 | 0 |
| `jev-1.13-free` | 1 | 0 | 0 | 0 |
| **合计 6 个** | **15** | **0** | **0 / 0 / 0 / 0** | **0** |

（`muse-spark-1.2` / `1.3` 在生产里的完整名字带 `-contributor-free` 后缀，Lead 转述时省略了。）

### 6.2 分母 / 成功率 / token：过滤前后完全相同

| 指标 | 过滤前 | 过滤后 |
| --- | --- | --- |
| `total_requests` | 231,761 | **231,761** |
| `ok`（展示层 ΣOK） | 208,141 | **208,141** |
| 成功率 | 89.808466% | **89.808466%** |
| `models` 键数 | 22 | **16** |
| `throughput_tps` | 77.87946233884321 | **不变** |
| `stream_tps_n` | 39,914 | **不变** |
| `unmapped_requests.count` | 1,391 | **不变** |
| 被剔除模型的 token（prompt/completion/cache_read/cache_creation） | 0/0/0/0 | 0/0/0/0 |
| 被剔除模型的流式样本 | 0 | 0 |

**token 影响 0、流式计数 0** 是断言出来的（真日志子用例 c 会 `t.Fatalf`），不是口头保证。

### 6.3 两个出口都过滤的证据

- `/api/stats?range=180d`：`models` 16 个键，6 个 0 成功模型缺席；
- `/api/stats/trends?range=180d&group=model`：模型集与上面**完全相同**（测试断言双向相等）；
- 半实现变异 M10（只删 models、漏 trends）**红**。

### 6.4 「其余字段逐字节不变」怎么证明

1. **`/api/stats?range=`**：`TestT39OnlyModelsAndTrendsDifferFromUnfiltered` 独立（不复用生产 helper）
   构造「未过滤 + 只删这些键」的期望响应，与真实响应 `json.Marshal` **逐字节**比对；
   生产若顺手改了 `total_requests` 或用了错判据（M7/M8/M9）立刻红。
   产出：期望 30,438 字节 == 实际，首个差异字节不存在。
2. **无 range 的 `GET /api/stats`**：与 `HEAD`（`ad699e7`，无本任务代码）逐字节比对
   （命令见 §8.4）：

   | 用例 | HEAD 的 sha256 | 交付态 sha256 | 长度 |
   | --- | --- | --- | --- |
   | 空 `tokenStats` | `78234cdfbee973c8d1d0062885528cc5bedfe13283e7d40ab3c222e05d3b6f1d` | **同左** | 32 |
   | 固定样例 | `d2bf5c773c67ee97176819664499c07d8d91ca5a7fafd5fa0398efa95ede5a9c` | **同左** | 282 |

3. **`/api/stats/trends?range=180d`（热力图，非模型维度）**：真日志下 **26,261 字节逐字节不变**；
   用例 `TestT39TrendsEndpointSameCriterion` ③ 与真日志子用例 `⑤` 双重断言。

### 6.5 被收窄的旧验收口径（必须写明）

| 旧口径 | 现在 | 原因 |
| --- | --- | --- |
| `/api/stats` 无 `range` 响应逐字节不变 | **不变**（仍逐字节） | 面板零读，只有 DELETE 用 |
| `/api/stats?range=` 的 `models` 含窗口内**全部已映射**模型 | **收窄**：只含窗口内 `OK>0` 的已映射模型；`trends_by_model` 同步 | R2/F1 裁决 |
| `/api/stats?range=` 的其余字段逐字节不变 | **不变**（§6.4） | — |
| `stats_window_test.go` 的「2 模型 × 7 桶 = 14 行」 | 改为 7 行，并新增「0 成功模型不出现」 | 契约同步 |
| `/api/stats/trends?group=model` 返回窗口内全部模型 | 只返回 `OK>0` 的模型 | 与 `/api/stats` 同口径 |
| `/api/stats/trends`（日总量/热力图） | **不变** | 无模型维度，按模型筛等于藏失败 |

### 6.6 已批准的语义变更：这是「口径更正」，不是「服务质量提升」

- 没有任何失败请求被变成功；成功率的分子分母**一条都没动**（89.808466% 前后完全相同）。
- 被隐藏的只有「窗口内从未成功过的模型」的**展示行**；它们仍是 `total_requests` 的一部分，
  详细层仍逐条可查（带 `unmapped` 标记 + 当次 WARN）。
- 已知可见副作用（用户已选）：`Σ models.request_count` 会比 `total_requests` 少这 15 条
  （15 / 233,152 ≈ **0.0064%**）。这是「并集做分母 + 展示层过滤」的必然结果，不是 bug。

---

## 7. 已知残余风险（诚实清单）

| # | 风险 | 影响 | 现状/缓解 |
| --- | --- | --- | --- |
| R1 | **写时冻结 vs 读时重算**：`unmapped` 在请求发生时冻结；老日志行在读时重算 | 某名字**第一次成功之前**的失败会永久留在排除集（冻结为 true），成功之后的失败才计入分母 | 方向正确（只会「少算失败」不会「少算成功」）；这是「曾经成功过⇒永远算我们的模型」的必然边界；测试 T3/T4 |
| R2 | **展示层与 `total_requests` 有 0.0064% 的口径差** | `Σ models.request_count ≠ total_requests` | 用户裁决的必然结果；§6.6 已列 |
| R3 | **公式 B 依赖 docs 快照漂移** | 从未成功过的裸名会 mapped→unmapped（§3.2） | 与裁决一致；docs 为空时全 mapped 安全降级 |
| R4 | **everServed 不持久化** | 重启后靠全量回填重建（+尾部 2000 条播种）；若日志被删/轮转，重建集变小 | 本仓库 `call_log.jsonl` 无轮转（只 append，`truncateCallLog` 只回滚在飞写入）；日志被用户删除时本来就等于历史归零 |
| R5 | **`clear()` 会清空 everServed** | 用户显式重置历史后，下一条 0 成功模型的同名请求可能短暂被当作「从未成功」 | 用户显式操作，语义正确 |
| R6 | **`-race` 全量依赖他人长用例** | 全包 `-race` 需 `-skip TestT30ProbeCauseAttribution` + `-timeout 35m` | 本次交付按此跑；结论见回执 |
| R7 | **回填期间的黑箱**：一次全量回填 233k 行 ~0.7s | 这段时间查询会阻塞在 `callLog.mu` | 既有限制，本任务未恶化（只多收一个 map，条目数 = 成功过的模型数，≤ 30） |
| R8 | **`trends?group=model` 无面板消费者** | 该出口目前是 API-only；过滤后行为变更无 UI 影响 | 但为了让「两个出口同口径」成立仍然改了（Lead 要求） |

---

## 8. 复现命令（每个数字都有出处）

### 8.1 数据快照与全量口径

```bash
docker cp opencode2api:/data/call_log.jsonl /tmp/t39/call_log.jsonl
docker exec opencode2api cat /data/free_models_cache.json > /tmp/t39/docs_cache.json
wc -l /tmp/t39/call_log.jsonl                     # 233152
export PATH=$PATH:/usr/local/go/bin GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
export T39_LOG=/tmp/t39/call_log.jsonl T39_DOCS=/tmp/t39/docs_cache.json
go test -run 'TestT39RealLogOverviewCriterion' -count=1 -v . | grep -E '真日志|窗口内|过滤后|两个出口|热力图'
```

### 8.2 排除集 / everServed 差集（纯 python，独立于 Go）

```bash
python3 - <<'EOF'
import json, collections
docs = json.load(open('/tmp/t39/docs_cache.json'))
agg = collections.defaultdict(lambda: [0, 0])
rows = 0
for line in open('/tmp/t39/call_log.jsonl'):
    line = line.strip()
    if not line: continue
    rows += 1
    r = json.loads(line); m = r.get('model') or ''
    agg[m][0] += 1
    if r.get('status') == 'ok': agg[m][1] += 1
print('rows', rows, 'distinct', len(agg))
formulaB = {m: v for m, v in agg.items() if not (m in docs or m.endswith('-free'))}
everServed = {m for m, v in agg.items() if v[1] > 0}
print('公式B 排除', sum(v[0] for v in formulaB.values()), 'ok=', sum(v[1] for v in formulaB.values()))
print('差集', set(formulaB) - (set(formulaB) - everServed))
EOF

# 行序统计：失败记录里「出现在同名首次成功之前」的有多少条
python3 - <<'EOF'
import json
first_ok = {}; rows = []; idx = 0
for line in open('/tmp/t39/call_log.jsonl'):
    line = line.strip()
    if not line: continue
    r = json.loads(line); m = r.get('model') or ''; ok = r.get('status') == 'ok'
    rows.append((m, ok)); idx += 1
    if ok and m not in first_ok: first_ok[m] = idx
fails = sum(1 for _, ok in rows if not ok)
bad = sum(1 for i, (m, ok) in enumerate(rows, 1) if not ok and m in first_ok and i < first_ok[m])
print(f'失败记录={fails} 乱序(失败早于同名首次成功)={bad}')
EOF
```

### 8.3 F1 精算（0 成功模型清单）

```bash
go test -run 'TestT39RealLogOverviewCriterion/c' -count=1 -v . | grep -E '窗口内|剔除|顶层|过滤后'
```

### 8.4 无 range `/api/stats` 逐字节不变（对 HEAD 比对）

```bash
rm -rf /tmp/t39head && mkdir -p /tmp/t39head
git archive HEAD | tar -x -C /tmp/t39head
# 把下面这个临时用例同时放进 HEAD 树与工作树（用 withTokenStats 会依赖未入库的测试文件，
# 所以用例里直接操作 tokenStats）
cat > /tmp/norange_sha_test.go <<'EOF'
package main
import ("crypto/sha256";"fmt";"net/http";"net/http/httptest";"testing")
func TestNoRangeStatsResponseSHA(t *testing.T) {
	cases := []*TokenStatsData{
		{Models: map[string]*ModelStats{}},
		{TotalRequests: 2, Models: map[string]*ModelStats{
			"m1": {RequestCount: 1, PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, CacheReadTokens: 4, AvgTTFTMs: 100, AvgOutputSpeed: 20, StreamReqCount: 1},
			"m2": {RequestCount: 1, PromptTokens: 20, CompletionTokens: 3, TotalTokens: 23}}},
	}
	tokenStatsMu.Lock(); old := tokenStats; tokenStatsMu.Unlock()
	defer func(){ tokenStatsMu.Lock(); tokenStats = old; tokenStatsMu.Unlock() }()
	for i, ts := range cases {
		tokenStatsMu.Lock(); tokenStats = ts; tokenStatsMu.Unlock()
		rec := httptest.NewRecorder()
		adminStatsHandler(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
		fmt.Printf("NR%d sha256=%x len=%d\n", i, sha256.Sum256(rec.Body.Bytes()), len(rec.Body.Bytes()))
	}
}
EOF
cp /tmp/norange_sha_test.go /tmp/t39head/ && cp /tmp/norange_sha_test.go ./
( cd /tmp/t39head && go test -run TestNoRangeStatsResponseSHA -count=1 -v . | grep '^NR' )
go test -run TestNoRangeStatsResponseSHA -count=1 -v . | grep '^NR'
rm -f zz_norange_sha_test.go /tmp/t39head/zz_norange_sha_test.go
```

### 8.5 变异测试

```bash
python3 /tmp/t39mut/mutate2.py   # 11 条：M1–M11；M12 见 /tmp/t39mut 手工脚本
```

---

## 9. 测试清单（`zz_t39_served_models_test.go`）

| 用例 | 盯什么 |
| --- | --- |
| `TestT39EverServedBareNameKeepsHistoryInOverview` | 裸名成功过 ⇒ 历史记录留在概览（含「上游摘掉裸名」变体） |
| `TestT39NeverServedNameStaysOutOfOverview` | 0 成功名字不进 `hourBuckets`，进 `unmappedBuckets`（`ok=0` 硬闸门） |
| `TestT39LiveSuccessFlipsRecordAndIsMonotone` | 实时成功：翻转落盘标记 + 进 everServed + 同名后续请求不再判未映射 |
| `TestT39DegradesSafelyWhenLogUnreadable` | 日志不可读 ⇒ 退回公式 B；docs 空 ⇒ 全 mapped |
| `TestT39OrderIndependenceAcrossRestart` | 成功行在头/中/尾 × 两轮全量重扫 ⇒ 结果相同（行序无关 + 跨重启一致） |
| `TestT39RealLogOverviewCriterion`（a/b/c） | 真日志：概览口径、目录漂移保护、F1 精算 + 两出口一致 + 热力图逐字节不变 |
| `TestT39OverviewHidesZeroSuccessModels` | F1 用例 1 |
| `TestT39OverviewKeepsModelWithSingleSuccessAmongManyFails` | F1 用例 2（1 成功 / 4,999 失败） |
| `TestT39OverviewFilterKeepsTotalsAndRateIntact` | F1 用例 3（分母/成功率不变） |
| `TestT39OverviewEmptyAndAllFailWindowsSafe` | F1 用例 4（空窗口 / 全 0 成功不 panic） |
| `TestT39OnlyModelsAndTrendsDifferFromUnfiltered` | 「其余字段逐字节不变」 |
| `TestT39TrendsEndpointSameCriterion` | 两个出口同口径 + 热力图分支逐字节不变 |
