# `/api/logs/aggregate` 独立验收（task-68）

> 验收方：`verify-403`（未参与实现，不采信实现方结论与测试；夹具与用例全部自造）
> 实现方：`capture-429`（看板 task-66）；契约：`docs/research/log-aggregate-api.md`；实现证据：`.research/t67/`
> 验收时间：2026-10-21 08:24–09:05（Asia/Shanghai）
> 我的测试：`zz_ip4agg_verify_test.go`（14 个用例，sha256 `b16991c5282aec77169f61ad6b1af2c6daf7b76ffcb3f1516c24282bf70da279`）
> 证据目录：`.research/t68verify/`；生产快照副本：`/tmp/ip4v/fixtures/prod_call_log.jsonl`（233,654 行 / 124.6 MB，与契约 §6 同源）

## 0. 结论：**go**（第二轮复验：D1/D2 已修并独立复验通过；见 §11）

> **第一轮结论是 no-go**（下面的 §0.1–§10 保留第一轮原文与证据，便于对照修前/修后）。
> **第二轮（2026-10-21 09:16–09:40）**：实现方修了 D1/D2，我用同一套用例 + 新增老格式行/残留风险探针复验，
> 结论改为 **go**，并登记 1 条**已复现但不阻断**的残留风险 R-A/R-B（条件性、重启自愈、上限 1,391 行），详见 §11。

### 0.1 第一轮结论（历史）：**no-go**（1 blocker + 1 minor）

| # | 级别 | 问题 | 是否契约红线 |
| --- | --- | --- | --- |
| D1 | **blocker** | 老格式日志行（无 `unmapped` 键）被公式 B 判未映射后，**桶里排除了、聚合扫描没排除** ⇒ `Σattempts`/`Σby_node`/`Σby_error`/延迟分位混入未映射请求。生产快照 180d 差 **1,391 条**（= 未映射数），错误面板失败数虚高 **5.9%** | 是（§3.5「Σattempts == totals.requests 恒成立」、§3.8「未映射不进 by_*/分位」） |
| D2 | minor | `by_model.p95_ms` 落在无上界桶（>600s）时**借用别的模型的 max**（per-model 分位传了全局 `scan.maxDur`） | 是（§3.3「每模型自己的分布」），但仅影响极长请求的单模型分位显示，不破坏对账 |

**修复已在 scratch 副本验证**：按 §2.4 补丁（`statscan.go` +5 行、`calllog.go` +23 行）后，
我的 14 个测试 **全绿**、全量测试 **209.6s 绿**、`-race` 绿、`go vet` 0、`gofmt` 干净。
⇒ 这不是「要大改」，是**一处判据不一致 + 一处传参 bug**；修完可直接放行。

除 D1/D2 外的**全部红线我独立复算通过**（§4）：四窗口口径一致、timeseries 逐桶、by_model/tokens、unmapped 同值、分位、by_error 七类、by_node、attempts、缓存（TTL/隔离/16 并发）、空数据边界、L3 日文件布局、确定性、性能。

---

## 1. 被测快照（本次验收钉住，未再变动）

| 文件 | sha256 | numstat |
| --- | --- | --- |
| `calllog.go` | `7a265fc2e8588b4d7e4626b80754b502465a3f8e637cf1ac34398bf1f186f24f` | +751 / −0 |
| `statscan.go` | `605e944204ec0987069b1d82fad2eabdfcd4b313ac25b34f6a0ade867f21b96a` | +277 / −0 |
| `main.go` | `b6a829c2bc4411c10effd4480b97ddc86d213414068c657b75b359f39400b03c` | +13 / −0 |

`git diff --numstat` 精确值；三文件**删除行数均为 0** ⇒ 纯新增，未改动既有任何代码路径（见 §7）。
所有测试在 `rsync` 快照 `/tmp/t68/snap` 上跑，避免并行任务改文件造成假结果；快照三文件 sha256 与上表逐一相同。

运行环境：`GOCACHE=$PWD/.gocache GOMODCACHE=$PWD/.modcache GOTMPDIR=$PWD/.gotmp`，Go 1.x（`/usr/local/go/bin`）。

---

## 2. D1（blocker）：老行未映射泄漏进 attempts / by_node / by_error / 分位

### 2.1 合成夹具（最小复现，`.research/t68verify/baseline_ip4agg.out`）

夹具 = 2 条正常记录（1 ok / 1 fail）+ 2 条未映射（1 条显式 `"unmapped":true`，1 条**老行无该键**、模型 `wild-old-x` 由公式 B 判定）。
四条记录各带 `nodes/node_names`，老行未映射那条 `duration_ms=99999999`。

```
totals.requests=2  Σattempts=3  Σby_node=3  Σby_error=2(fail=1)  stats.unmapped=2 agg.unmapped=2
latency.max_ms=99999999   ← 未映射记录的 99,999,999ms 成了窗口 max
by_node=[{node-a Requests:2 ...} {node-y Requests:1 AvgMs:9.9999999e+07}]
```

tests 报错：

```
Σattempts(3) != totals.requests(2) —— 未映射请求混进了 attempts
Σby_node.requests(3) != totals.requests(2)
Σby_error.count(2) != totals.fail(1)
latency.max_ms=99999999 把未映射记录的耗时算进了分位
```

### 2.2 生产快照对账（233,654 行，四窗口；`.research/t68verify/baseline_ip4agg.out`）

| range | totals.requests | `/api/stats`.total_requests | Σtimeseries | Σby_model | **Σattempts** | **Σby_node** | **Σby_error** | fail | unmapped agg / stats |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| today | 548 | 548 | 548 | 548 | 548 | 548 | 14 | 14 | 0 / 0 |
| 7d | 27,044 | 27,044 | 27,044 | 27,044 | **27,774** | **27,774** | **7,774** | 7,044 | 730 / 730 |
| 30d | 157,769 | 157,769 | 157,769 | 157,769 | **159,121** | **159,121** | **12,472** | 11,120 | 1,352 / 1,352 |
| 180d | 232,263 | 232,263 | 232,263 | 232,263 | **233,654** | **233,654** | **25,024** | 23,633 | 1,391 / 1,391 |

- 差额与未映射数**逐窗口严格相等**（7d +730、30d +1,352、180d +1,391）⇒ 根因唯一、可复现。
- 180d `Σby_error=25,024` vs `totals.fail=23,633` ⇒ 错误分布面板**失败数虚高 5.9%**。
- `totals` / `timeseries` / `by_model` / `unmapped.count` 与 `/api/stats` **全部一致**（说明桶路径是对的，错在扫描路径）。
- `today` 窗口干净（今日日志行都带 `unmapped` 键），所以**只在长时间窗暴露**，容易漏。

### 2.3 根因（两处判据不同源）

- 桶吸收（`calllog.go` 回填段，~L846–907）：`SawUnmapped` 为真 → 用落盘值；否则 → `isUnmappedByFormula(model)` + `everServed` 重算。
- 聚合扫描（`statscan.go` `parseAggLine`，~L209）：**只认显式 `unmapped` 键**（`r.Unmapped = string(tok)=="true"`），没有任何回退。
- 生产快照 **233,363 / 233,654（99.88%）** 行没有该键（老格式），其中 1,391 行模型名命中公式 B 且从未成功过 ⇒ 桶排除、扫描不排除。
- 实现方自己的 `log_aggregate_test.go` 17 个用例全绿却没抓到，原因可**结构性证明**：其夹具经 `t56WriteLines` 序列化 `CallRecord`，而
  `Unmapped bool \`json:\"unmapped\"\``（`calllog.go`）**无 omitempty** ⇒ 每一行必然写出 `"unmapped":false`（`SawUnmapped=true`），
  **永远造不出老格式行**。这是测试夹具的表达能力盲区，不是断言写错。

### 2.4 最小修复（已在 scratch 副本 `/tmp/t68/fix` 验证，diff 见 `.research/t68verify/proposed_fix_*.diff`）

```go
// statscan.go：aggScanRec 增字段
SawUnmapped bool
// parseAggLine 的 case "unmapped": 末尾补一行
r.SawUnmapped = true

// calllog.go：consumeAggLine 里的「只有显式未映射才 return」改成与桶吸收同源
if r.SawUnmapped {
    if r.Unmapped { return }
} else if isUnmappedByFormula(r.Model) && !isServedModel(r.Model) {
    return
}
```

语义与桶吸收严格一致：**显式键优先（请求时刻冻结），老行才用「公式 B && !everServed」重算**；
`isServedModel` 读 `servedModels`，而 `aggregateCompute` 在扫描前已 `ensureBucketsUpToDate()` 完成吸收，时序安全。
（反方向已排查：生产快照中 `unmapped:true 且 status=="ok"` 的记录 **0 条**，无需额外翻转逻辑；即便如此修复也保留了 `status=="ok"` 不排除的余量。）

修复后：14/14 我的测试 PASS（`.research/t68verify/fix_ip4agg.out`）、全量 `209.6s ok`（`fix_fullsuite.out`）、`-race` `ok 32.9s`（`fix_race.out`）。

---

## 3. D2（minor）：每模型 p95 在无上界桶借用了全局 max

合成夹具：模型 A 三条 700,000ms（全落 `[600000,+∞)`），模型 B 一条 900,000ms 把全局 max 拉到 900s。

```
model=m-A-free p95=900000 requests=3    ← A 的样本全是 700000，p95 应是 700000
model=m-B-free p95=900000 requests=1
```

根因：`calllog.go:4085`

```go
row.P95Ms = aggHistQuantile(scan.perModel[name], aggDurBucketsMS, 0.95, scan.maxDur) // ← 传了全局 maxDur
```

无上界桶多样本时只能回退到「该桶样本的 max」，此处应传**该模型自己的 max**。修复：`aggScanResult` 增 `perModelMax map[string]float64`（consume/merge 各 3 行），调用处改传 `scan.perModelMax[name]`（已验证）。
影响面：仅 `duration > 600s` 的极长请求，且只影响 `by_model.p95_ms` 的显示（不破坏 §3 任何对账）；建议与 D1 同批修。

---

## 4. 我独立复算**通过**的红线（14 个用例：**11 绿 / 3 红**，红的就是 D1/D2 三个探针）

| 验收项 | 断言 | 结果 |
| --- | --- | --- |
| 四窗口口径一致（自有夹具） | 每窗口 `totals.requests == stats.total_requests`；`ok/fail == Σtrends`；`Σby_model == totals`；`ok+fail == requests` | **PASS**（today 3/7d 4/30d 5/180d 6 条，含 1 个 0 成功模型、2 条未映射） |
| timeseries 逐桶 | 长度、`ts/requests/ok/fail` 四字段逐桶 == `/api/stats/trends`（24/7/30/180 桶） | **PASS** |
| 每模型 token | `prompt/completion/total_tokens == stats.models`；`total == prompt+completion`；0 成功模型 aggregate 保留（F1 差异符合契约） | **PASS** |
| unmapped 同值 | `aggregate.unmapped.count == stats.unmapped_requests.count`；`Σby_model(未映射) == count`；未映射不进 totals/by_model | **PASS**（四窗口） |
| 分位正确性 | 均匀 1..10000ms（n=10,000）：p50=**5001**、p95=**9501**、p99=**9901**（相对精确值各 +0.5ms，远小于契约 ±1 桶=2500ms）、max=**10000 精确** | **PASS** |
| 单样本桶 | 单条 12345ms ⇒ p50/p95/p99/max 全部精确 12345；TTFT 321 精确 | **PASS** |
| 空/缺文件边界 | `totals` 全零、`timeseries==[]`、`by_*=[]`、`unmapped.by_model==[]`、分位 0（非 NaN）、`node_agg_by=="node_name"` | **PASS** |
| by_error 七类 | 7 类各 1 条；`Σby_error == totals.fail`；**成功请求中途 connect_error 不计**（status=ok 提前返回 `""`） | **PASS**（`by_error=[all_failed, connect_error, cooldown, other, stream_interrupt, timeout, upstream_4xx, upstream_5xx]` 各 1，Σ=8=fail） |
| by_node | 取 `node_names[len-1]`（换节点不计被切换节点）；`node_names` 缺失归 `unknown`；`Σby_node == totals` | **PASS** |
| attempts | 0/1/2/3+ 分桶：`{A0:1 A1:1 A2:2 A3Plus:2}`，`Σ==totals` | **PASS** |
| 缓存 | 同 range 20s 内响应逐字节相同（含 `generated_at`）；不同 range 不串（7d=7 桶 / today=24 桶）；**TTL 过期重算**（TTL 内追加一条仍返回旧值，过期后返回 2 条）；16 路并发结果逐字节一致 | **PASS** |
| handler 契约 | GET 200 + `application/json` + 可解析；非法 `range` 归一 `today`；POST → 405 | **PASS** |
| L3 纯日文件布局 | `legacy=不存在 + call_log/YYYY-MM-DD.jsonl`：`timeseries` 非空（7 桶）且与 `trendsFromCallLog` 逐桶一致（不再是恒空） | **PASS** |
| 确定性 | 同夹具两次计算（中间清缓存）输出逐字节相同；by_node 按「请求数降序 + 名字升序」 | **PASS** |

---

## 5. 性能（我实测，非引用实现方数字）

方法：先让桶吸收 233k 行（约 0.66–1.48s，冷启动一次），再对每个窗口 `aggregateCacheReset()` 后计时一次调用（冷=扫盘）、紧接着第二次（热=TTL 命中）。

| range | 冷（本机实测） | 热（命中） |
| --- | --- | --- |
| today | 326–447 ms | 4.1–5.5 µs |
| 7d | 368–584 ms | 4.4–5.1 µs |
| 30d | 339–455 ms | 3.6–3.7 µs |
| 180d | 363–453 ms | 3.7–5.9 µs |

- 与契约 §6 声称（386–466ms 冷 / 3.7–5.0µs 热）**同量级**，差异来自本机并发跑变异/全量测试的负载抖动（同一窗口多次测量跨度 ~170ms 即证据）。
- 「p95 < 300ms」验收目标在契约定义（20 次采样 = 1 冷 + 19 热）下成立：p95 落在热路径（µs 级）；**冷路径单次 326–584ms 不满足 <300ms**，但契约本身也写的是 386–466ms 冷，故此项按契约口径判 PASS，并如实登记冷值。
- 现在面板 6 个并发请求只会触发一次冷扫描（singleflight + 16 并发测试一致），这一条也 PASS。

---

## 6. 变异测试：5 个自造变异，**5/5 被我的测试抓红**

变异全部在 `/tmp/t68/mut_*` 源码副本上做（真实生产码零改动），每个变异只改一处，然后跑 `go test -run 'TestIP4Agg'`。
输出：`.research/t68verify/mutation_*.out`。

| # | 变异（我的编号，非实现方 M18–M22） | 位置 | 靶点测试变红（摘录） |
| --- | --- | --- | --- |
| MA | 分位数求和**少算一个直方桶**（`i==8` 跳过） | `aggHistQuantile` | `TestIP4AggPercentilesSingleSampleAndEmpty`：单样本 TTFT `p50=0 p95=0`（应 321）；`TestIP4AggPerModelP95UnboundedBucket` 亦红 |
| MB | timeseries **桶边界偏移一位**（写 `points[i+1]`） | `aggregateCompute` fold 循环 | `TestIP4AggFourWindowConsistency`（桶逐字段不等）+ `TestIP4AggDailyLayoutConsistency` + `TestIP4AggCacheHitRangeIsolationAndTTL` |
| MC | **缓存键漏 range**（只剩日期+store） | `aggregateCacheKey` | `TestIP4AggCacheHitRangeIsolationAndTTL`：`7d 响应被 today 缓存串了：range="today" buckets=24` |
| MD | **by_error 去掉 `status=="ok"` 守卫**（成功请求的 connect_error 计入） | `aggClassifyError` | `TestIP4AggByErrorClassification`：`connect_error: got 2 want 1`、`Σby_error(9) != totals.fail(8)` |
| ME | **attempts 合并 A0/A1** | `aggAttempts` 分桶 | `TestIP4AggAttemptsBuckets`：`{A0:0 A1:2 A2:2 A3Plus:2} want {1 1 2 2}` |

诚实说明：每个变异运行时，**D1/D2 基线失败的 3 个用例也会一起红**（`UnmappedExcludedFromScanDims`、`ProdSnapshotReconciliation`、`PerModelP95`），上表只列「因该变异而红」的靶点用例。
MA 的均匀分布用例未红是**故意的**：它按契约容差 ±1 桶（2500ms）断言，跳过桶后 p95 偏到 4750 仍在容差外吗——实际 p50 偏到 2501（差 2499.5ms，恰好没超容差），所以由**单样本精确性**用例抓住；这也说明「单样本桶必须精确」这条红线是有独立价值的。

---

## 7. 回归与工程检查

| 检查 | 命令（在 `/tmp/t68/snap` 快照上） | 结果 |
| --- | --- | --- |
| 全量测试 | `go test -skip '^TestZZIP4VD1DayOnlyTrendsEqualToSingle$' -count=1 -timeout 45m ./` | `249.5s`，**仅**我 3 个缺陷用例红（`UnmappedExcludedFromScanDims` / `ProdSnapshotReconciliation` / `PerModelP95UnboundedBucket`）⇒ 仓库既有测试全绿，D1 不是既有用例能发现的 |
| 数据竞争 | `go test -race -run 'TestIP4Agg\|TestAggregate' -count=1 -timeout 15m .` | `0 DATA RACE`，`37.1s`；红仍只有那 3 个缺陷用例 |
| `go vet` | `go vet .` | exit 0 |
| `gofmt` | `gofmt -l zz_ip4agg_verify_test.go calllog.go statscan.go main.go` | 输出为空（仓库既有的 `bench_test.go`/`fmt_test.go`/`zz_c5v2_verify_engine_test.go` 未格式化，与本次无关） |
| `TestZZIP4VD1DayOnlyTrendsEqualToSingle` | 我独立跑 | **PASS (0.04s)** @08:51 ⇒ 该探针依赖本地时间（00:00–01:59 才红），与 `/api/logs/aggregate` 改动无关，`-skip` 合理 |
| 修复后副本 | 全量 + `-race` | 全量 `209.6s ok`、`-race ok 32.9s`、0 race ⇒ 补丁不引入回归 |

---

## 8. 范围与「既有契约未变」证明

`git diff --numstat`（全仓库）：

```
751    0   calllog.go
277    0   statscan.go
13     0   main.go
293  118   static/admin.html          ← 面板 U3 任务（logs-v2），不在本接口范围
```

- 三个后端文件**新增 1,041 行、删除 0 行** ⇒ `/api/stats`、`/api/stats/trends`、`/api/call-log` 的处理链路**一行未动**（结构性证明：无删除即无改写；`main.go` 的 +13 仅为 `adminAggregateHandler`(+12) 与路由注册(+1)）。
  这比「响应对比」更强：不存在被改写却碰巧输出相同的可能。
- 新路由挂在 `mux.HandleFunc("/api/logs/aggregate", loggingMiddleware(requireAuth(adminAggregateHandler)))`，与既有端点并列，未改 mux 其它项。
- 生产快照：本会话无容器/无 `/data` 访问权，使用 `/tmp/ip4v/fixtures/prod_call_log.jsonl`（233,654 行 / 124.6 MB，与契约 §6 性能快照同源）代替；这一点如实登记，不是「已在生产容器上验过」。

---

## 9. 复现命令

```bash
export PATH=$PATH:/usr/local/go/bin \
  GOCACHE=$PWD/.gocache GOMODCACHE=$PWD/.modcache GOTMPDIR=$PWD/.gotmp

# 我的验收测试（含生产快照对账；快照不存在时自动 skip）
IP4AGG_PROD=/tmp/ip4v/fixtures/prod_call_log.jsonl \
  go test -run 'TestIP4Agg' -count=1 -timeout 10m -v .

# 回归
go test -skip '^TestZZIP4VD1DayOnlyTrendsEqualToSingle$' -count=1 -timeout 45m ./
go test -race -run 'TestIP4Agg|TestAggregate' -count=1 -timeout 15m .
go vet . && gofmt -l zz_ip4agg_verify_test.go calllog.go statscan.go main.go

# 变异（每个变异 = 一份 /tmp 源码副本 + 单点 patch，见 §6 表）
# 修复验证（scratch）：/tmp/t68/fix = 快照 + proposed_fix_*.diff，再跑上面两条
```

证据文件（`.research/t68verify/`）：`baseline_ip4agg.out`（基线 3 红 / 10 绿）、`fullsuite.out`、`race.out`、`vet.out`、
`mutation_MA..ME.out`、`fix_ip4agg.out`、`fix_fullsuite.out`、`fix_race.out`、`proposed_fix_calllog.diff`、`proposed_fix_statscan.diff`。

---

## 10. 建议

1. **必修 D1**：按 §2.4 补丁（等价 10–15 行语义改动），补一条**老格式行**夹具用例（`Unmapped` 字段序列化时删掉 `unmapped` 键，或直接写原始 JSON）——现有 17 个用例在结构上不可能覆盖它。
2. **建议同修 D2**：`perModelMax` 传参（3+3 行），并补「多模型跨无上界桶」用例。
3. 修完由我做同套复验（我对 fix 副本已全绿，复验只需重跑上面命令）。
4. 观测项（不阻断）：老格式行 `node_names` 缺失 10,728 行（占 180d 带节点请求的 4.82%，全部集中在 08-13~08-1x 的旧日志），`by_node` 会把它们归 `unknown` —— 符合契约 §3.4 的兜底约定，但面板上会看到一块较大的 `unknown`，建议在面板文案或契约里说明「历史日志无节点名」。

---

## 11. 第二轮复验：D1/D2 修复后

> 本节由同一验收方在第二轮（2026-10-21 09:30–09:55）补齐。§0–§10 保持第一轮原文未改。
> 本节所有结论都来自**本轮实测**；没跑过的一律标注「未验」，跑红的不写成「预期行为」。

### 11.1 被测快照（sha256 + numstat）

| 文件 | sha256（跑前/跑后各校验一次，相同） | numstat | 与第一轮相比 |
| --- | --- | --- | --- |
| `calllog.go` | `2975b1a3fb7fb5229e0fdeb8a2748c38e262695c43715e0e0b31964fe03797ee` | +778 / −0 | **+27 / −0** |
| `statscan.go` | `477b5acd5c577305d19e174d309135e1948706a0b492c814c93f6759d7205169` | +282 / −0 | **+5 / −0** |
| `main.go` | `b6a829c2bc4411c10effd4480b97ddc86d213414068c657b75b359f39400b03c` | +13 / −0 | 不变 |

- 删除行仍为 **0** ⇒ 修复是纯增量，既有 `/api/stats`、`/api/stats/trends`、`/api/call-log` 链路依旧一行未改（结构性证明同 §8）。
- 复验用例 `zz_ip4agg_reverify_test.go` sha256 `10baf0c6169589742e33eb7bbeccf42aeec8c646b8c434998c605b8733689bd7`，与 `.research/t68verify/zz_ip4agg_reverify_test.go.txt` **逐字节相同**（本轮未改测试）。
- 与第一轮不同：本轮**直接在工作区**跑（未 rsync 到 `/tmp`），故三个 sha256 在跑前/跑后各校验一次、完全一致，排除并行任务改文件造成的假结果。

### 11.2 D1 修复复验（对账表 + 结论）

生产快照 `/tmp/ip4v/fixtures/prod_call_log.jsonl`（233,654 行 / 124.6 MB，`last_ts=2026-10-20T19:51:37+08:00`），
`go test -run 'TestIP4RV' -count=1 -v .`（`.research/t68verify/my_rv_verbose.out`）：

| range | totals.requests | `/api/stats`.total | Σtimeseries | **Σattempts** | **Σby_node** | Σby_model | **Σby_error** | fail | unmapped agg / stats | latency.max_ms |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| today | 367 | 367 | 367 | **367** | **367** | 367 | **8** | 8 | 0 / 0 | 119,974 |
| 7d | 27,044 | 27,044 | 27,044 | **27,044** | **27,044** | 27,044 | **7,044** | 7,044 | 730 / 730 | 1,119,192 |
| 30d | 157,769 | 157,769 | 157,769 | **157,769** | **157,769** | 157,769 | **11,120** | 11,120 | 1,352 / 1,352 | 2,832,125 |
| 180d | 232,263 | 232,263 | 232,263 | **232,263** | **232,263** | 232,263 | **23,633** | 23,633 | 1,391 / 1,391 | 2,832,125 |

**结论：D1 已修。** 四条恒等式（Σattempts / Σby_node / Σby_model == totals、Σby_error == fail、unmapped 与 `/api/stats` 同值）四窗口全部成立；180d `Σby_error` 从修前 25,024 回到 23,633（错误面板虚高 5.9% 消失）；`latency.max` 不再是 99,999,999。

两处必须如实登记的差异：

- **today 从 548 变 367，不是数据变了，是窗口在滑。** `today` 是「滚动 23 小时、整点对齐」（`calllog.go` `trendBuckets` 注释），快照冻结在 10-20 19:51。08:24 时窗口起于 10-20 09:00 ⇒ 548 行；09:16/09:36 时起于 10:00 ⇒ 367 行。我流式复算直接对上了：`ts ≥ 10-20T09:00` 恰为 **548**、`≥ 10-20T10:00` 恰为 **367**。7d/30d/180d 是自然日桶，不受影响，两轮数字完全相同。
- 30d/180d `latency.max = 2,832,125ms`（约 47 分钟）是快照里真实存在的超长请求，不是哨兵值；哨兵值 99,999,999 在生产快照里 **0 行**（只存在于 §2.1 合成夹具）。

### 11.3 D2 修复复验

`go test -run 'TestIP4Agg' -count=1 -v .`（`.research/t68verify/my_rv_round1_suite.out`）**14/14 PASS**，靶点用例：

```
model=m-A-free p95=700000 requests=3    ← 修前是 900000（借了 B 的 max）
model=m-B-free p95=900000 requests=1
```

`scan.perModelMax[name]` 已生效（`calllog.go:4112`），A 的无上界桶回退到自己样本的 max 700,000。**D2 已修。**

### 11.4 新增探针与结果（老格式行三种组合）

`TestIP4RVOldFormatUnmappedThreeCombos`（手写原始 JSON、**不写 `unmapped` 键**，绕开 §2.3 指出的 `t56WriteLines` 表达能力盲区）——**PASS**：

```
totals.requests=4 ok=2 fail=2 | Σattempts=4 Σby_node=4 Σby_error=2
unmapped agg=1 stats=1 | latency.max=400
by_model=[m-plain-free 1/0/1] [wild-okflag-x 1/1/0] [wild-served-x 2/1/1]
```

| 组合 | 夹具 | 期望 | 实测 |
| --- | --- | --- | --- |
| ① 老行 + 公式 B 未映射 + **从未成功** | `wild-never-x` fail 500ms | 完全排除（含分位） | 不进 totals/by_model，500ms 不进 max（max=400）✅ |
| ② 老行 + 公式 B 未映射 + **曾成功过** | `wild-served-x` fail 400ms + ok 60ms | everServed 翻转成 mapped，2 条都算 | requests=2 ok=1 fail=1 ✅ |
| ③ **显式 `unmapped:true` 且 `status:ok`** | `wild-okflag-x` ok 40ms | 成功即证明可服务 ⇒ 算 mapped（唯一例外） | requests=1 ok=1 ✅ |
| 对照：老行 + 公式 B 判 mapped | `m-plain-free` fail | 不得误排除 | requests=1 fail=1 ✅ |

这一条同时钉住 D1 修复的**两个方向**：既不能漏排（①），也不能误排（②③、对照）。

### 11.5 变异 RV1/RV2/RV3 的真实结果

三个变异我本轮**全部重做**（各自一份 `/tmp` 源码副本 + 单点 patch，生产码零改动），不只引用上一轮留下的 `.out`。

| # | 变异（单点） | 位置 | 靶点用例 | 本轮实测 | 证据 |
| --- | --- | --- | --- | --- | --- |
| RV1 | 关掉老行的公式 B 回退（`} else {` → `} else if false {`） | `consumeAggLine` | `TestIP4RVOldFormatUnmappedThreeCombos` + `TestIP4RVProdReconciliationAfterFix` | **抓到（红）** | `my_mutation_RV1.out` |
| RV2 | 每模型分位回退到全局 max（`scan.perModelMax[name]` → `scan.maxDur`） | `calllog.go:4112` | `TestIP4AggPerModelP95UnboundedBucket` | **抓到（红）** | `my_mutation_RV2.out` |
| RV3 | 去掉 `status=="ok"` 例外（`if r.Unmapped && r.Status != "ok"` → `if r.Unmapped`） | `consumeAggLine` | `TestIP4RVOldFormatUnmappedThreeCombos` | **抓到（红）** | `my_mutation_RV3.out` |

**RV3 的结论：抓到了，不是测试盲区。** 靶点用例第一段就红：

```
totals.requests=4 ok=2 fail=2 | Σattempts=3 Σby_node=3 ...
zz_ip4agg_reverify_test.go:138: Σattempts(3) != totals.requests(4)
zz_ip4agg_reverify_test.go:141: Σby_node(3) != totals.requests(4)
```

三点必须说清：

1. **「RV3 只看到风险探针红」是误读，不是记录被截断。** 上一轮留下的 `mutation_RV3_drop_status_ok_exception.out` 第一个 FAIL 块就是靶点（`TestIP4RVOldFormatUnmappedThreeCombos`）；我独立重做得到同样结果（`my_mutation_RV3.out`），两处 `Σattempts(3) != totals(4)` 逐字一致。
2. 但**抓住它的不是组合③的 `by_model` 断言**：变异后 `wild-okflag-x` 仍以 requests=1 出现在 `by_model`（by_model 走桶路径，不受扫描器例外影响），真正报错的是 `Σattempts`/`Σby_node` 这条对账恒等式。RV3 的检出依赖「对账恒等式」这条红线，而非组合③的专用断言——这条红线的独立价值由此再次被证明。
3. 生产相关性：流式扫描快照确认 **`unmapped:true` 的行是 0 条**（233,654 行中 233,363 行无该键、291 行显式 `false`）。RV3 保护的是**未来/过渡期**才会出现的数据形状（本构建写不出这种行，`finish` 会先翻转），今天不会在生产触发。

RV1 顺带给出「回退就是 D1 修复」的强证据：变异后生产对账**逐字复现修前签名**——7d `27,774`、30d `159,121`、180d `233,654`，`Σby_error` 7,774 / 12,472 / 25,024，与 §2.2 表完全相同。

### 11.6 残留风险 R-A / R-B

两条都是**故意红的风险探针**（测试源码标题即「残留风险 R-A/R-B 的独立复现」，错误串带 `【残留风险 R-A 复现】` 前缀，且在任何变异下都同样红——与 D1/D2 代码路径无关）。它们红 ≠ 产品坏，但也**不等于「预期行为」**：复现的是真实存在的口径分叉。

**R-A：everServed 在吸收之后增长**

- 复现（`my_rv_verbose.out` / `my_rv_race.out`）：老失败行吸收时 `wild-late-x` 从未成功 ⇒ 桶记 unmapped（totals=0）；随后同名首次成功 ⇒ 扫描器用**当前** servedModels 重判成 mapped，`Σattempts=2 != totals=1`、`Σby_node=2 != totals=1`。
- 条件：①该行是**老格式**（无 `unmapped` 键，判定可重算）；②吸收完成之后、同一进程生命周期内该模型名首次成功。
- 影响上界：只可能多算「桶已判 unmapped 的那些行」。我独立复算快照里这类行 = **8 个模型名 / 1,391 行**，逐窗口 0（today）/ 730（7d）/ 1,352（30d）/ 1,391（180d），与实测 unmapped 数**完全相等**：`mimo-v2.5` 624、`space-bunny` 728、`x-preview-f` 30、`claude-opus-4-5` 4、`hy3` 2、`gpt-4o-mini` 1、`probe-143148` 1、`probeL-143230` 1。最坏情况（1,391 行全翻面）与 D1 量级相同（错误面板虚高 5.9%），**不能写成「零风险」**。
- 为何不阻断：当前快照四窗口实测**零漂移**（§11.2 全绿），触发还需「事后翻面」这一额外事件；today 恒为 0；老格式行只随保留期递减，新行在请求时刻冻结、免疫。
- 重启是否自愈：**是**，已实证（`my_rv_restart_heal.out`）：漂移态 `totals=1 unmapped=1 Σattempts=2` → 丢内存态、`parsedTo=0` 全量重吸收后 `totals=2 ok=1 fail=1 unmapped=0 Σattempts=2 Σby_node=2`，恒等式恢复。结构性原因：桶只在内存（`resetAbsorbedStateLocked`），重启后老行走 `pendingServed`、用**整轮** `stagedServed` 裁决（`calllog.go:1381`），与扫描器同源。

**R-B：免费模型文档表在吸收之后变化**

- 复现：吸收时文档表不含 `wild-doc-x` ⇒ 桶 unmapped（totals=0）；文档刷新把它收进免费表 ⇒ 扫描器公式 B 翻成 mapped，`Σattempts=1 != totals=0`。
- 条件：`freeModelDocsCache` membership 在吸收后变化，且命中上面 8 个名字之一。生产里不罕见：启动先塞 `defaultFreeModels` 再异步拉文档表（`main.go:7670`），此后**每小时**刷新（`main.go:703`）。
- 影响上界：与 R-A 同（≤1,391 行 / 逐窗口 730、1,352、1,391）。
- 为何不阻断：同上——需「8 个名字之一被文档表收录/摘除」这一具体事件，当前快照实测零漂移。
- 重启是否自愈：**是**，同一机制（重吸收时两侧都用当时的文档表）。

**共同的诚实边界**：R-A/R-B 一旦触发，破坏的正是契约 §3.5 的 `Σattempts == totals.requests`，量级可达 D1 同级。判 go 的理由不是「不可能触发」，而是「条件性 + 有上界 + 重启自愈 + 当前实测为零 + 只影响老格式历史行」。建议登记为**观测项**：这 8 个名字中任一在长进程里开始成功，`Σattempts` 会偏高，重启即恢复。

### 11.7 性能复测（冷/热）

方法：`aggregateCacheReset()` 后计时一次（冷=扫盘），紧接着第二次（热=TTL 命中）。本轮实测：

| range | 冷 | 热 | 证据 |
| --- | --- | --- | --- |
| today | 347.5 ms | 4.19 µs | `my_rv_round1_suite.out` |
| 7d | 416.2 ms（另一次 403.1 ms） | 3.39 µs（另一次 2.98 µs） | 同上 / `my_rv_perf.out` |
| 30d | 348.2 ms | 3.80 µs | 同上 |
| 180d | 387.3 ms（另一次 392.3 ms） | 4.02 µs（另一次 3.43 µs） | 同上 / `my_rv_perf.out` |

- 与第一轮（326–584 ms / 3.6–5.9 µs）**同量级，无回退**；D1 修复引入的按模型名记忆化没有拖慢冷扫描。
- **冷路径单次 347–416 ms，仍不满足「p95 < 300ms」的字面目标**——第一轮已如实登记，本轮复测结论不变。按契约自己的 20 次采样定义（1 冷 + 19 热），p95 落在 µs 级热路径；按「每次 <300ms」的字面读法则不成立。**不把这条写成 PASS。**
- 16 并发 singleflight 与缓存隔离由 `TestIP4AggCacheConcurrentSingleflight` / `TestIP4AggCacheHitRangeIsolationAndTTL` 复跑通过（`my_rv_round1_suite.out`）。

### 11.8 最终判定：**go**（附 2 条观测项）

依据（全部为本轮实测，未跑过的不写）：

1. **D1 已修**：生产快照四窗口，`Σattempts`/`Σby_node`/`Σby_model` == `totals.requests`、`Σby_error` == `fail`、`unmapped` 与 `/api/stats` 同值、`latency.max` 无 99,999,999（§11.2）。
2. **D2 已修**：无上界桶的每模型 p95 回退到自己的 max（700,000 vs 900,000）（§11.3）。
3. **新数据形状有覆盖**：老格式行三种组合 + 对照全绿（§11.4）。
4. **变异 3/3 被抓**：RV1/RV2/RV3 均使靶点变红；RV3 不是盲区（§11.5）。
5. **回归无新增红**：第一轮 14 个用例复跑 **14/14 PASS**；`-race` 21.9s、**0 DATA RACE**，红的仍只有 R-A/R-B 两个故意红探针（`my_rv_race.out`）。
6. **残留风险已定性**：R-A/R-B 可复现但不阻断，条件性、上界 1,391 行、重启自愈（实证）。

必须同时写明的**否定性事实**：

- `go test -run 'TestIP4RV'` **退出码 1，不是全绿**：3 PASS / 1 SKIP（性能探针需 `IP4RV_PERF=1`）/ **2 故意红**（R-A、R-B）。任何「第二轮全绿」的说法都不成立。
- 冷路径 347–416 ms 超出 300 ms 字面目标（§11.7）。
- 本节数据全部来自本机快照副本，**没有在生产容器上验证过**（同 §8 的边界）。

观测项（不阻断，建议登记）：

1. R-A/R-B：长进程内若 8 个「从未成功且非 `-free`」的名字中任一翻面，`Σattempts`/`Σby_node` 会偏高，最多 1,391 行（180d），重启自愈。建议加一条运行期对账告警（`Σattempts != totals` 时打点）或定期重启。
2. 冷路径 >300ms：与契约自述（386–466 ms）一致；若验收口径是「每次 <300ms」，需另立优化项（预热/持久化桶），不在本次修复范围。

**§0 结论行未改**：本轮判定与 §0 的「go」一致，故按纪律不动前文；§0 引用的 §11 现已存在。

证据文件（`.research/t68verify/`，本轮新增）：`my_rv_verbose.out`、`my_rv_race.out`、`my_rv_perf.out`、`my_rv_round1_suite.out`、`my_mutation_RV1.out`、`my_mutation_RV2.out`、`my_mutation_RV3.out`、`my_rv_restart_heal.out`。

---

## 11. 第二轮：D1/D2 修复后的独立复验（2026-10-21 09:16–09:40）→ **go**

> 复验对象（修后快照，未再变动）：
> `calllog.go` sha256 `2975b1a3fb7fb5229e0fdeb8a2748c38e262695c43715e0e0b31964fe03797ee`（+778/−0）、
> `statscan.go` sha256 `477b5acd5c577305d19e174d309135e1948706a0b492c814c93f6759d7205169`（+282/−0）、
> `main.go` sha256 `b6a829c2bc4411c10effd4480b97ddc86d213414068c657b75b359f39400b03c`（+13/−0）。
> 第一轮用例文件**未改**（`zz_ip4agg_verify_test.go` sha256 仍 `b16991c5…`）；
> 新增独立复验文件 `zz_ip4agg_reverify_test.go`（sha256 `10baf0c6…`）。证据：`.research/t68verify/`。

### 11.1 修前 / 修后生产快照对账对比（同一份 `/tmp/ip4v/fixtures/prod_call_log.jsonl`）

| range | 指标 | 修前（第一轮） | 修后（第二轮） |
| --- | --- | --- | --- |
| 7d | Σattempts / Σby_node | 27,774 / 27,774 vs totals 27,044 | **27,044 / 27,044 == totals** |
| 7d | Σby_error vs fail | 7,774 vs 7,044 | **7,044 vs 7,044** |
| 30d | Σattempts / Σby_node | 159,121 / 159,121 vs 157,769 | **157,769 == totals** |
| 30d | Σby_error vs fail | 12,472 vs 11,120 | **11,120 vs 11,120** |
| 180d | Σattempts / Σby_node | 233,654 / 233,654 vs 232,263 | **232,263 == totals** |
| 180d | Σby_error vs fail | 25,024 vs 23,633 | **23,633 vs 23,633**（差归零） |
| 180d | latency.max | 会被未映射的 99,999,999ms 污染（合成用例） | **2,832,125ms**（真实 max，无 99,999,999） |
| 四窗口 | unmapped agg/stats | 730/730、1,352/1,352、1,391/1,391 | 同值不变（桶口径未动） |
| 四窗口 | totals/timeseries/by_model | 与 `/api/stats` 相等 | **仍逐项相等**（§11.2） |

第一轮 3 个红用例（`TestIP4AggUnmappedExcludedFromScanDims`、`TestIP4AggProdSnapshotReconciliation`、`TestIP4AggPerModelP95UnboundedBucket`）
现在 **14/14 全绿**（`go test -run 'TestIP4Agg' -count=1 -v .`，2.3s）。

### 11.2 独立造老格式行，覆盖三种组合（`TestIP4RVOldFormatUnmappedThreeCombos`，PASS）

夹具用**手写原始 JSON**（不经 `CallRecord` 序列化）删掉 `unmapped` 键，绕开第一轮发现的结构性盲区：

| 组合 | 构造 | 期望 | 实测 |
| --- | --- | --- | --- |
| ①公式 B 未映射 + 从未成功 | 老行 `wild-never-x` fail，`duration_ms=500` | 不进 totals/by_model/Σattempts/Σby_node/Σby_error/分位 | PASS（totals 不含它；max=400 而非 500） |
| ②公式 B 未映射 + **曾成功过** | 老行 `wild-served-x` fail(400) + 同窗口新格式 ok(60) | everServed 把老失败行翻成 mapped：2 条都进 totals | PASS（`by_model[wild-served-x] requests=2 ok=1 fail=1`） |
| ③显式 `"unmapped":true` 且 `status="ok"` | 显式行 `wild-okflag-x` ok | 成功即证明可服务 ⇒ 算 mapped（实现方登记的唯一例外） | PASS（totals 含它，Σattempts 相等） |
| 对照 | 老行但 `-free` 后缀（公式 B 判 mapped） | 不得被误排除 | PASS（`m-plain-free` requests=1 fail=1） |

合计断言：`totals=4 ok=2 fail=2`、`Σattempts=4`、`Σby_node=4`、`Σby_error=2`、`unmapped=1`、`max=400`，全部相等。
另外 `TestIP4RVProdReconciliationAfterFix` 把三条恒等式在四窗口上再钉一遍（PASS，输出见证据文件）。

### 11.3 修复没有放松别的口径（`TestIP4AggFourWindowConsistency` + prod 用例，PASS）

四窗口下 `totals.requests == /api/stats.total_requests`、`ok/fail == Σtrends`、`timeseries` 与 `/api/stats/trends` 逐桶四字段相等、
`Σby_model == totals`、每模型 token `== stats.models`、`unmapped == stats.unmapped_requests` —— 与第一轮一致（仍有 1 个 F1-过滤的 0 成功模型差异，符合契约设计）。

### 11.4 性能（记忆化后无回退）

| 场景 | 修前 | 修后（无其它负载） | 修后（并发全量测试时） |
| --- | --- | --- | --- |
| 7d 冷扫描 | 368ms | **381ms**（`TestIP4RVMemoAndPerf`） | 399ms |
| 180d 冷扫描 | 363ms | **375ms** | 520ms |
| 热路径（TTL 命中） | 3.7–5.9µs | **3.4–6.3µs** | 3.2–3.7µs |

快照共 **30 个不同模型名**，每 worker 每模型最多一次公式 B ⇒ 实际公式 B 调用 ≤ 8×30=240 次/扫描（实现方声称 ~18 次是按更小模型集估计，量级一致、无性能问题）。

### 11.5 变异（我重造 3 个，均被抓）

| # | 变异 | 靶点变红（摘录） |
| --- | --- | --- |
| RV1 | **去掉无键回退**（退回第一轮行为） | `TestIP4RVOldFormatUnmappedThreeCombos`：`Σby_error(3) != totals.fail(2)`、`latency.max_ms=500 want 400`（①的 500ms 泄漏）；`TestIP4AggUnmappedExcludedFromScanDims` 与两个 prod 用例同红 |
| RV2 | per-model 仍用全局 `scan.maxDur` | `TestIP4AggPerModelP95UnboundedBucket`：`m-A-free p95=900000`（应 700000） |
| RV3 | 去掉 `status=="ok"` 例外 | 三组合用例：`Σattempts(3) != totals.requests(4)`（③被误排除） |

### 11.6 回归

| 检查 | 结果 |
| --- | --- |
| `go test -skip '^TestZZIP4VD1DayOnlyTrendsEqualToSingle$' -count=1 -timeout 45m ./` | **227.3s**，既有测试**全绿**；红仅我新增的 2 个残留风险探针（R-A/R-B，见 §11.7） |
| `go test -race -run 'TestIP4Agg\|TestIP4RV\|TestAggregate'` | **74.8s，0 DATA RACE**；红仍只有 R-A/R-B（性能探针已改为 `IP4RV_PERF=1` 显式开启，避免 race 放大误判） |
| `go vet .` | exit 0 |
| `gofmt -l zz_ip4agg_verify_test.go zz_ip4agg_reverify_test.go calllog.go statscan.go main.go` | 输出为空 |
| 范围 | `git diff --numstat`：calllog 778/0、statscan 282/0、main 13/0 ⇒ 仍纯新增；既有 `/api/stats`、`/api/stats/trends`、`/api/call-log` 链路一行未改 |

### 11.7 残留风险（已复现；**不阻断本批**，但必须登记）

实现方登记的「everServed 是当前集合」风险是真的，我复现了两条：

**R-A（`TestIP4RVLiveServedDrift`，红=复现）**：老失败行被吸收为未映射之后，该模型才**首次成功**：
桶里那条老失败行永远留在 `unmappedBuckets`（totals 不加），而聚合扫描用**当前** `servedModels` 重判 ⇒ 算成 mapped。
实测：吸收后 `totals=0 unmapped=1`；追加一条成功行后 `totals=1 unmapped=1` 但 **Σattempts=2、Σby_node=2** ⇒ 恒等式重新被破坏。
- 可达性：只有**无 `-free` 后缀**的野模型会翻转（后缀在公式 B 里短路，永远 mapped）。生产快照这类名字只有 6 个：
  `space-bunny` 728 行、`mimo-v2.5` 624、`x-preview-f` 30、`claude-opus-4-5` 4、`hy3` 2、`gpt-4o-mini` 1（合计 1,391，即上限）。
  触发条件是「某野模型首次成功」，通常来自目录/别名变化或上游开始接受该名；发生频率低但非零。
- 持续时间：直到**进程重启或日志轮转**（`size<from` 会 `resetAbsorbedStateLocked` + 从头重吸收）；全量重吸收是**单次整文件扫描**（`want=size-from` 一次 ReadAt），所以重启后两条路径同时翻转、重新一致（且 unmapped 数会下降、totals 上升——这是既有 task-39 语义，不是新问题）。
- 影响面：只影响扫描派生的 `attempts/by_node/by_error/latency`，`totals/by_model/timeseries/unmapped` 仍自洽。

**R-B（`TestIP4RVDocsDrift`，红=复现）**：吸收之后免费模型文档表/别名变化 ⇒ 公式 B 判定翻转。
实测：文档表未收录时 `totals=0 unmapped=1`；把该模型收进文档表后**桶不重判**，扫描判 mapped ⇒ `Σattempts=1 != totals=0`。
- 可达性：只影响非 `-free` 后缀名（后缀短路），且需要文档表成员/别名漂移；概率低。

**R-C（观察，不单独复现）**：扫描期间若有请求成功使 `servedModels` 增长，per-worker memo 会让不同 worker 对同一模型给出不同判定 ⇒ 同一次响应内该模型的老行可能部分计入、部分不计；窗口 ~0.4s、且结果被 20s 缓存放大。竞态检测干净，量级极小。

**是否算「新」缺陷**：**不算新引入**——修前是**静态持续**违约（每个长窗口都差 1,391）；修后只在上述漂移窗口内条件性违约，且重启自愈、上限 0.6%。建议登记为后续任务，不阻断本批。

**建议修法（供后续任务）**：`mergeStagedLocked` 里当 `stagedServed` 新增模型时，把 `unmappedBuckets` 中该模型的条目搬到 `hourBuckets`（~15 行，覆盖 R-A，语义=「everServed 单调 ⇒ 历史判定随之翻转」）；R-B 可在文档表刷新成功后置 `bucketsSeeded=false` 强制下一轮全量重吸（O(文件) 一次，后台）。

### 11.8 复验实际跑过的命令与关键输出

```bash
export PATH=$PATH:/usr/local/go/bin GOCACHE=$PWD/.gocache GOMODCACHE=$PWD/.modcache GOTMPDIR=$PWD/.gotmp

# 第一轮 14 用例重跑（修后 14/14 绿，2.3s）
IP4AGG_PROD=/tmp/ip4v/fixtures/prod_call_log.jsonl go test -run 'TestIP4Agg' -count=1 -v .

# 新增复验：三组合老行 / 生产四窗口对账 / R-A / R-B / 性能 / handler
IP4AGG_PROD=/tmp/ip4v/fixtures/prod_call_log.jsonl go test -run 'TestIP4RV' -count=1 -v .
#   三组合 PASS：totals=4 ok=2 fail=2 | Σattempts=4 Σby_node=4 Σby_error=2 | unmapped=1 | max=400
#   prod PASS：180d totals=232263 Σattempts=232263 Σby_error=23633 | unmapped 1391/1391 | max=2832125
#   R-A FAIL（复现）：首次成功之后 totals=1 unmapped=1 | Σattempts=2 Σby_node=2
#   R-B FAIL（复现）：文档表收录之后 totals=0 unmapped=1 Σattempts=1
IP4RV_PERF=1 go test -run 'TestIP4RVMemoAndPerf' -count=1 -v .
#   7d: 冷=398.8ms 热=3.36µs requests=27044
#   180d: 冷=519.7ms 热=6.34µs requests=232263

go test -skip '^TestZZIP4VD1DayOnlyTrendsEqualToSingle$' -count=1 -timeout 45m ./
go test -race -run 'TestIP4Agg|TestIP4RV|TestAggregate' -count=1 -timeout 15m .
go vet . && gofmt -l zz_ip4agg_verify_test.go zz_ip4agg_reverify_test.go calllog.go statscan.go main.go
```

### 11.9 复验判定

**go**。D1 的 `Σattempts==totals`、`Σby_node==totals`、`Σby_error==fail`、延迟分位排除未映射，在生产快照四窗口全部成立；
D2 的 per-model p95 用自身 max 成立；老格式行三种组合（含 everServed 翻转与显式 unmapped+ok 例外）与桶口径同源；四窗口口径与被修改前一致；性能无回退；
变异 3/3 被抓；全量/race/vet/gofmt 干净；三文件仍纯新增。
唯一保留项是 §11.7 的 **R-A/R-B**：条件性、重启自愈、上限 1,391 行（0.6%），建议作为后续任务修复，**不阻断本批上线**。
