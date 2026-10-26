# `/api/logs/aggregate` 契约（分析页聚合接口）

> 看板 **task-66**（标题 task-67）交付。分析页 6 个 Panel 的**唯一数据源**。
> 用户已裁决分析页照 freellm Analytics 骨架做（`docs/research/U2b-freellm-style-recipe.md` P2–P5），
> **前端不从 23 万行明细本地聚合**（本地实测 2,029 ms，页面会卡死）。
>
> 本文件是给前端消费的契约：字段语义、单位、空值约定、示例 JSON、实测性能、缓存 TTL。
> 实现：`calllog.go`（聚合/缓存/分位）、`statscan.go`（聚合扫描解析器）、`main.go`（handler + 路由）。

---

## 1. 接口

```
GET /api/logs/aggregate?range=today|7d|30d|180d
```

| 项 | 值 |
| --- | --- |
| 方法 | 仅 `GET`（其它方法 `405`） |
| 鉴权 | 与现有面板接口一致：`requireAuth`（未登录 302 到 `/login`；未设密码时放行） |
| `range` 缺省/非法 | 归一为 `today`（与 `/api/stats/trends` 同一归一函数 `normalizeTrendRange`），不报错 |
| 响应 | `200 application/json`，UTF-8 |
| 缓存 | 服务端 20 s TTL + singleflight（见 §6） |

窗口定义**与 `/api/stats?range=`、`/api/stats/trends?range=` 完全同源**：

| range | 窗口 | 桶 |
| --- | --- | --- |
| `today` | 滚动最近 24 小时，起点 = `now-23h` 截断到整点，终点 = 起点 + 24h | **24 个小时桶**（`ts` = `2006-01-02T15:04`，本地时区） |
| `7d` / `30d` / `180d` | 含今天在内的最近 N 个自然日，起点 = (今天 00:00) − (N−1) 天，终点 = 明天 00:00 | **N 个天桶**（`ts` = `2006-01-02`，本地时区） |

> 窗口随日期滚动（`today` 还随小时滚动）。前端**不要**把 `window.start/end` 当固定值，
> 也不要在测试里写死日期。

---

## 2. 响应字段

```jsonc
{
  "range": "today",
  "window": { "start": "…RFC3339…", "end": "…RFC3339…" },
  "generated_at": "…RFC3339…",

  "totals":   { "requests": 0, "ok": 0, "fail": 0, "success_rate": 0 },
  "latency":  { "p50_ms": 0, "p95_ms": 0, "p99_ms": 0, "max_ms": 0,
                "ttft_p50_ms": 0, "ttft_p95_ms": 0 },
  "timeseries": [ { "ts": "…", "ok": 0, "fail": 0, "requests": 0 } ],

  "by_model": [ { "model": "…", "requests": 0, "ok": 0, "fail": 0,
                  "success_rate": 0, "p95_ms": 0,
                  "prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0 } ],
  "by_node":  [ { "name": "…", "requests": 0, "ok": 0, "fail": 0,
                  "success_rate": 0, "avg_ms": 0 } ],
  "by_error": [ { "kind": "…", "count": 0, "sample": "…" } ],

  "attempts": { "a0": 0, "a1": 0, "a2": 0, "a3plus": 0 },
  "phases":   { "queue_avg_ms": 0, "ttft_avg_ms": 0, "gen_avg_ms": 0 },

  "unmapped": { "count": 0, "by_model": [ { "model": "…", "count": 0 } ] },

  "node_agg_by": "node_name"
}
```

### 2.1 字段语义与单位

| 字段 | 类型 | 单位 | 语义 |
| --- | --- | --- | --- |
| `range` | string | — | 归一后的窗口名（`today`/`7d`/`30d`/`180d`） |
| `window.start` / `window.end` | string | RFC3339 | 左闭右开的窗口边界（本地时区偏移） |
| `generated_at` | string | RFC3339 | 本次**计算**时刻（缓存命中时可能是 ≤20 s 前的值） |
| `totals.requests` | int | 条 | 窗口内计入统计的请求数，**恒等于** `/api/stats?range=` 的 `total_requests` |
| `totals.ok` / `totals.fail` | int | 条 | `status=="ok"` / 其它；`ok+fail==requests` |
| `totals.success_rate` | float | 0–1 | `ok/requests`；`requests==0` 时为 `0`（不是 NaN） |
| `latency.p50/p95/p99_ms` | float | ms | 窗口内**全部计入统计的请求**（含失败、**不含未映射**，见 §3.9）的墙钟耗时分位 |
| `latency.max_ms` | float | ms | 窗口内计入请求的最大 `duration_ms`（精确值；未映射的异常占位耗时不计入） |
| `latency.ttft_p50/p95_ms` | float | ms | **仅流式请求里 `ttft_ms>0` 的样本**的分位；无样本时为 `0` |
| `timeseries[]` | array | — | `today`→24 个小时桶；`7d/30d/180d`→N 个天桶；桶内 `requests/ok/fail` 与 `/api/stats/trends` 逐桶相等 |
| `by_model[].model` | string | — | 模型名（空字符串 `""` 是合法键，表示未记录模型） |
| `by_model[].requests/ok/fail` | int | 条 | 该模型窗口内请求数/成功/失败，**含失败**；Σ 等于 `totals` |
| `by_model[].success_rate` | float | 0–1 | `ok/requests` |
| `by_model[].p95_ms` | float | ms | **该模型各自分布**的 p95（不是全局 p95） |
| `by_model[].prompt_tokens` | int | token | 与 `/api/stats.models[].prompt_tokens` 同源同值（2b 口径：原始 prompt，不扣 cache） |
| `by_model[].completion_tokens` | int | token | 同上 |
| `by_model[].total_tokens` | int | token | **恒等于** `prompt_tokens + completion_tokens`（2b 口径） |
| `by_node[].name` | string | — | 节点**显示名**（`node_names[]`，不是指纹）；无记录名时归入 `"unknown"` |
| `by_node[].requests/ok/fail` | int | 条 | 按「**请求最后一次尝试的节点**」聚合（一个请求只计一次，Σ 等于 `totals.requests`） |
| `by_node[].success_rate` | float | 0–1 | `ok/requests` |
| `by_node[].avg_ms` | float | ms | 该节点组请求的平均耗时（含失败） |
| `by_error[].kind` | string | — | 错误类别枚举，见 §3.6 |
| `by_error[].count` | int | 条 | 该类别失败请求数（Σ 等于 `totals.fail`，一条只归一类；未映射已排除） |
| `by_error[].sample` | string | — | 该类别的第一个非空错误样本（≤120 字节，已截断；可能只是 `status=fail`） |
| `attempts.a0/a1/a2/a3plus` | int | 条 | 尝试过的节点数分布：`0` / `1` / `2` / `≥3`（来自 `len(nodes)`）；四者之和 == `totals.requests` |
| `phases.queue_avg_ms` | float | ms | `首个事件时间 − ts` 的平均值（仅统计有事件的请求；见 §3.7） |
| `phases.ttft_avg_ms` | float | ms | `ttft_ms>0` 样本的平均 TTFT |
| `phases.gen_avg_ms` | float | ms | `max(0, duration_ms − queue − ttft)` 的平均值（仅统计有事件的请求） |
| `unmapped.count` | int | 条 | 窗口内「模型名无法映射」的请求数，**不计入** `totals`；与 `/api/stats?range=` 的 `unmapped_requests.count` 同源同值 |
| `unmapped.by_model[]` | array | — | Top N（N=10）+ `"__other__"` 合并，与 `/api/stats` 同源；按模型名升序 |
| `node_agg_by` | string | — | 固定 `"node_name"`：明确 `by_node` 按**名称**聚合（节点池允许同名多实例） |

---

## 3. 计算口径（红线）

### 3.1 totals / timeseries：与 `/api/stats`、`/api/stats/trends` 同源同值

`aggregate` 的 `totals` 与 `timeseries` **不重新扫盘**，直接复用现有小时桶的折叠内核：

- `totals.requests/ok/fail` ← 与 `trendsFromCallLog` 同一份 `foldBuckets(ts,bidx)` 数学；
- `by_model` 的 token ← `modelStatsWithBuckets(ts,bidx)`（与 `/api/stats?range=` 同一个函数）；
- `unmapped` ← `unmappedStatsInWindow(ts,bidx)` + `topUnmappedModels`；
- 窗口配置 `ts/bidx` 只取**一次**（`trendBuckets(rng)`），一次响应内不会出现「合计与分项跨窗口」的老问题。

由 `TestAggregateTotalsAndTimeseriesMatchStats` 在**同一份夹具**下逐窗口断言：

- `aggregate.totals.requests == /api/stats.total_requests`
- `aggregate.timeseries[i].{ts,requests,ok,fail} == /api/stats/trends[i].{ts,requests,ok,fail}`（逐桶）
- `Σ by_model.requests/ok/fail == totals.*`
- `by_model` 的 token 与 `/api/stats.models` 逐模型同值
- `aggregate.unmapped` 与 `/api/stats.unmapped_requests` 同值

> 2b 口径（`TotalTokens = prompt_tokens + completion_tokens`、输入 = prompt − cache_read）
> **一个字都没改**：`aggregate` 只读取既有字段，没有新的 token 定义。

`by_model` 与 `/api/stats.models` 有一处**有意的不同**：`/api/stats` 的展示层只显示
「窗口内至少 1 条成功」的模型（F1 过滤），而 `aggregate.by_model` 输出**窗口内全部模型**。
原因：分析页的 by_model 表要能回答「哪个模型在失败」，过滤掉 0 成功模型会把失败藏起来。
`Σ by_model.requests` 因此等于 `totals.requests`（不是 `/api/stats` 展示层的和）。

### 3.2 延迟分位

数据来源：一次磁盘扫描（`aggregateScanExtras`）统计窗口内每条记录的 `duration_ms` / `ttft_ms`。
**未映射请求已排除**（与 `/api/stats` 同口径，判定见 §3.9），否则它们的异常耗时（生产里出现
`99,999,999ms` 的占位值）会把 `max_ms` 与高分段位整体污染。
算法：**直方桶 + 桶内线性插值**（不保存全量样本）。桶边界（ms）：

```
duration: 0 5 10 25 50 100 250 500 1000 2500 5000 10000 20000 30000 60000 120000 300000 600000 (+∞)
ttft:     0 5 10 25 50 100 150 200 300 500 800 1200 2000 4000 8000 16000 30000 60000 (+∞)
```

- 分位取 nearest-rank：`rank = ceil(q*n)`，落在桶 `[lo,hi)` 时返回 `lo + (hi-lo)·(rank−cum)/count`；
- **单样本桶**（`count==1`）直接返回该样本的精确值（和 / 1），保证「单条记录时 p50=p95=p99=max」；
- 无上界桶（`>600s`）回退到该模型**自己的**实际最大值（`perModelMax[model]`），
  不能借全局 `max_ms`（否则模型 A 的 p95 会被模型 B 的更长样本顶掉，见 §12 D2）；
  全局 `max_ms` 仍由窗口内所有计入请求的真实最大值得到，TTFT max 同理；
- 空样本 → `0`。
- 已知分布断言容差：±1 个直方桶（`TestAggregatePercentilesKnownDistribution`）。

> 为什么不用精确分位：23 万行 × 全量样本内存/传输都不可接受；直方桶 18 个 `int64` + 值和的成本是常数级。

### 3.3 by_model

- `requests/ok/fail` ← 小时桶按模型折叠（`foldModelsInWindow`），**含失败**；
- `p95_ms` ← 每模型各自的直方图（`scan.perModel[model]`），不是全局 p95；
- token 字段 ← `modelStatsWithBuckets`（2b 口径，同 `/api/stats`）。

### 3.4 by_node（按名称聚合）

**口径**：一个请求只计一次，归到它**最后一次尝试的节点显示名**（`node_names[len-1]`；
若 `node_names` 缺失则记 `"unknown"`）。因此：

- `Σ by_node.requests == totals.requests`（可对账；未映射请求已排除，与 `/api/stats` 同口径）；
- 成功率/平均耗时回答「最终落在这个节点上的请求怎么样」；
- 不是「每个请求对所有尝试过的节点各计一次」（那样合计会 > 总请求数）。
- 响应里用 `node_agg_by:"node_name"` 明确标注按名称聚合：节点池允许同名多实例，
  名称维度是用户语义，指纹维度见调用日志明细页。

### 3.5 attempts（重试次数分布）

`attempts.aN` ← `len(nodes)`（节点指纹链长度）：0=没有可用节点（早失败）、1=一次成功/失败、
2=切换一次、3+=多次重试。**0 与 1 分开统计**，不得合并（合并会把「没有节点可用」的
系统性问题伪装成普通单次请求）。四者之和 == `totals.requests`（未映射请求已排除，
否则对账不成立）。

### 3.6 by_error 分类

只对 **`status!="ok"` 的失败请求**分类（一条只归一类）。成功请求中途的
`connect_error`/`switch`（换节点后成功）**不计入** by_error —— 那是「重试成功」，
不是错误面板的一条错误；不加这条守卫，`Σby_error.count` 会大于 `totals.fail`。
优先级（从高到低）：

| kind | 判据 |
| --- | --- |
| `connect_error` | `events[]` 含 `connect_error`（建连失败/EOF） |
| `timeout` | `events[]` 含 `ttft_timeout` 或 `silence_timeout` |
| `stream_interrupt` | `events[]` 含 `stream_interrupt` / `stream_error` |
| `cooldown` | `err_msg` 含 `cooldown` / `冷却`（冷却拒绝；无独立事件类型，按错误文本归类） |
| `upstream_5xx` | `final_status>=500` 或 `err_msg` 含 `http 5` |
| `upstream_4xx` | `final_status>=400` 或 `err_msg` 含 `http 4`（含 429 quota） |
| `all_failed` | `events[]` 含 `all_failed` |
| `other` | 其它 `status!="ok"`（样本回退为 `status=fail`） |

### 3.7 phases（为什么慢）

| 字段 | 公式 | 样本 |
| --- | --- | --- |
| `queue_avg_ms` | `首个事件时间(at) − ts`（clamp 到 `[0, duration]`） | 有 `events[]` 的请求 |
| `ttft_avg_ms` | `ttft_ms` 的算术平均 | `ttft_ms>0`（流式） |
| `gen_avg_ms` | `max(0, duration_ms − queue − ttft_ms)` 的算术平均 | 有 `events[]` 的请求 |

> 老日志没有 `events[]` 时只影响 queue/gen 两个均值（分母不含它们），不影响其它字段。

### 3.8 unmapped

与 `/api/stats?range=` 的 `unmapped_requests` **同一份** `ts/bidx` + 同一函数：
`count` 只数窗口内被判定为未映射的请求条数；这些请求**不在** `totals` / `by_model` /
`by_node` / `attempts` / `by_error` / 延迟分位里（与 `/api/stats` 的「不计入统计」口径一致）——
它们只在 `unmapped` 里统计，所以 `Σattempts == totals.requests`、`Σby_node.requests == totals.requests`、
`Σby_error.count == totals.fail` 这三条对账恒成立。判定来源与「老行回退」见 §3.9。

### 3.9 未映射排除口径（含老格式行回退）

**判据必须与桶吸收逐字同源**（`calllog.go` 回填段 / `statscan.go parseAggLine`）：

| 行格式 | 判定 | 依据 |
| --- | --- | --- |
| 有显式 `unmapped` 键（本构建写的行） | 采信落盘值；唯一例外：`status=="ok"` 视为已映射 | 请求发生那一刻冻结，目录漂移不追认历史 |
| 无 `unmapped` 键（**本构建之前**写的老行） | `isUnmappedByFormula(model) && !isServedModel(model)` | 公式 B（`-free` 后缀 + 当前免费目录）+ everServed（历史成功过 ⇒ 永久 mapped） |

- 生产快照 233,654 行里 **233,363 行（99.88%）没有该键** —— 只认显式键会把 1,391 条
  「公式 B 判未映射」的老行泄漏进 `attempts` / `by_node` / `by_error` / 延迟分位
  （task-68 D1 blocker；修复记录见 §12）。
- 老行判定按**模型名记忆化**（每 worker 私有 map），233k 行只需 ~18 次公式 B 调用；
  公式 B 只读目录快照、不触发刷新，`isServedModel` 读全局 everServed（单调集合）。
- 语义边界：everServed 是「至今为止」的集合，桶对老行的裁决发生在吸收那一轮；
  若某个模型在**回填之后**才首次成功，其回填前的失败行在桶里已被判未映射、而扫描
  （用当前 everServed）会把它算回来 —— 生产上仅影响「上线后才首次成功」的模型的历史
  失败行，量级极小；测试夹具都在同一轮吸收，无此漂移。
- 未映射行的节点名/错误/耗时**一律不进** by_node/by_error/分位；它们只体现在
  `unmapped.count` 与 `unmapped.by_model`（与 `/api/stats?range=` 的 `unmapped_requests` 同值）。

---

## 4. 空值与边界约定

| 场景 | 行为 |
| --- | --- |
| 日志文件/目录不存在 | `totals` 全 0；`timeseries == []`（与 `/api/stats/trends` 的 `nil→[]` 一致，**不返回 24 个假零桶**）；`by_*` / `unmapped.by_model` 为 `[]`（不是 `null`） |
| 文件存在但窗口内无记录 | `timeseries` 是该 range 的完整零桶数组（24 / 7 / 30 / 180） |
| 窗口内无流式样本 | `ttft_p50_ms = ttft_p95_ms = 0`，`phases.ttft_avg_ms = 0` |
| 无失败 | `by_error == []`，`success_rate = 1`（有请求时） |
| 未映射 0 条 | `unmapped:{count:0, by_model:[]}` |
| `range` 非法/缺失 | 归一为 `today`，响应 `range:"today"` |
| 记录 `ts` 解析失败 | 该行跳过（与桶吸收的跳过语义一致） |
| 模型名为空 | `by_model[].model == ""` 是合法键，不过滤（与 `/api/stats` 一致） |

---

## 5. 缓存 / 并发

| 项 | 值 |
| --- | --- |
| TTL | **20 s**（`aggregateCacheTTL`，15–30 s 区间） |
| 缓存键 | `range + "|" + 当日日期(YYYY-MM-DD，本地时区) + "|" + storePath + "|" + storeDir` ⇒ **跨零点自然失效**、不同 range 不串；带 store 身份是为了测试替换夹具时不串数据（生产路径恒定） |
| 单飞 | 每个 key 一个 in-flight call + `done` channel；并发请求共享一次计算 |
| 生成时刻 | 缓存命中时 `generated_at` 可能是 ≤20 s 前的计算时刻（前端不要用它判断“实时性”） |
| 并发安全 | `go test -race -run TestAggregate` 通过；`TestAggregateConcurrentSingleflight` 16 并发结果逐字节一致 |

> 为什么缓存里不编「小时」：`today` 是滚动 24h，TTL 20 s 远小于 1 小时桶宽，
> 把小时编进键只降低命中率、不增加正确性；跨零点用日期即可自然失效。

---

## 6. 性能实测

环境：本机 4 核，Go 1.26.5，生产快照 `/tmp/ip4v/fixtures/prod_call_log.jsonl`
（233,654 行 / 124.6 MB，含未映射/乱序 ts/同 ts）。冷=清缓存后首次；热=TTL 内；
p95=「1 冷 + 19 热」连打 20 次的 95 分位：

| range | 窗口内请求数 | 冷（缓存未命中，首次扫盘） | 热（缓存命中） |
| --- | --- | --- | --- |
| `today` | 367 | 317 ms | **4.3 µs** |
| `7d` | 27,044 | 339 ms | **3.9 µs** |
| `30d` | 157,769 | 348 ms | **3.8 µs** |
| `180d` | 232,263 | 383 ms | **3.6 µs** |

> 上表是 D1/D2 修复后的复测（`.research/t67/evidence/perf_after_d1d2.txt`）；修复前同机
> 同快照测得 386–466 ms / 3.7–5.0 µs（`.research/t67/evidence/perf_prod_snapshot.txt`）。
> 冷值随机器负载有 ~±70 ms 抖动，p95（1 冷 + 19 热）始终落在微秒级热路径。

- **热路径 ≪ 300 ms 目标**（微秒级：只做桶折叠 + 上一轮扫描结果的缓存返回）；
- **p95 ≪ 300 ms**：与单条记录数无关，因为冷扫描只占 20 次请求里的 1 次（TTL 20 s 内其余全命中）；
- 冷路径 ~0.4–0.5 s（4 核）：把物理行按行号切成 `min(GOMAXPROCS,8)` 块并行消费
  （`aggregateScanExtras` + `consumeAggLine` + `mergeAggScanResults`），单线程实测 803 ms → 并行 463 ms；
  单文件布局只有一段，按段并行的方案无效，所以按**行**切块；
- 因为 TTL 20 s + singleflight，面板一次刷新并发打进来的 6 个请求只会产生**一次**冷扫描；
  连续切换 range 时最坏每种 range 各一次；冷扫描只在「进程启动后第一次请求」或
  「TTL 过期后的第一次请求」发生，不是每次请求全量扫盘；
- 分位数用直方桶，扫描结果只保留 18×2 个计数/值和 + 每模型 18 个计数，无全量样本。

> 如果未来把冷路径也压到 <300 ms，可把分位/节点/错误累计移到桶吸收阶段
> （桶结构 + 一个 `hourNodeBucket`/`hourErrorBucket` 平行表）；本次选择**独立扫描 + 缓存**，
> 好处是零改动现有桶结构/守恒不变量（`ΣN == absorbed - unabsorbed` 等），
> 风险面只在新增代码。

---

## 7. 示例 JSON

（真实响应，只把 `timeseries` 压成非零桶及其相邻桶；`generated_at` 固定为文档示例值）

```json
{
  "range": "today",
  "window": {
    "start": "2026-10-20T01:00:00+08:00",
    "end": "2026-10-21T01:00:00+08:00"
  },
  "generated_at": "2026-10-21T00:40:00+08:00",
  "totals": { "requests": 4, "ok": 2, "fail": 2, "success_rate": 0.5 },
  "latency": {
    "p50_ms": 1200, "p95_ms": 30000, "p99_ms": 30000, "max_ms": 30000,
    "ttft_p50_ms": 120, "ttft_p95_ms": 300
  },
  "timeseries": [
    { "ts": "2026-10-20T21:00", "ok": 0, "fail": 0, "requests": 0 },
    { "ts": "2026-10-20T22:00", "ok": 2, "fail": 2, "requests": 4 },
    { "ts": "2026-10-20T23:00", "ok": 0, "fail": 0, "requests": 0 }
  ],
  "by_model": [
    { "model": "deepseek-v4-flash-free", "requests": 2, "ok": 1, "fail": 1,
      "success_rate": 0.5, "p95_ms": 4800,
      "prompt_tokens": 240, "completion_tokens": 160, "total_tokens": 400 },
    { "model": "space-bunny-free", "requests": 2, "ok": 1, "fail": 1,
      "success_rate": 0.5, "p95_ms": 30000,
      "prompt_tokens": 240, "completion_tokens": 160, "total_tokens": 400 }
  ],
  "by_node": [
    { "name": "node-b", "requests": 2, "ok": 1, "fail": 1, "success_rate": 0.5, "avg_ms": 2750 },
    { "name": "node-a", "requests": 1, "ok": 1, "fail": 0, "success_rate": 1, "avg_ms": 1200 },
    { "name": "node-c", "requests": 1, "ok": 0, "fail": 1, "success_rate": 0, "avg_ms": 30000 }
  ],
  "by_error": [
    { "kind": "timeout", "count": 1, "sample": "status=fail" },
    { "kind": "upstream_4xx", "count": 1, "sample": "upstream http 429 quota_signal" }
  ],
  "attempts": { "a0": 0, "a1": 3, "a2": 1, "a3plus": 0 },
  "phases": { "queue_avg_ms": 30, "ttft_avg_ms": 210, "gen_avg_ms": 9040 },
  "unmapped": { "count": 0, "by_model": [] },
  "node_agg_by": "node_name"
}
```

夹具说明（可复现）：4 条记录，2 个模型 / 3 个节点，其中 1 条 429 quota、1 条 ttft timeout；
所有请求发生在同一个小时桶。`attempts.a1=3, a2=1`；`phases.queue=30ms`（首事件在 ts+30ms）。

---

## 8. 前端 6 个 Panel 的映射建议（本次不改前端）

| freellm Analytics 区块 | 直接用 |
| --- | --- |
| 请求量时间分布（折线/柱） | `timeseries[]`（`ts/ok/fail/requests`；today→小时，其余→天） |
| 成功率与延迟（4 个 Stat 卡） | `totals.success_rate`、`latency.p50/p95/p99_ms`、`latency.ttft_p95_ms`；`max_ms` 放 tooltip |
| 分布（by_model / by_node / by_error） | `by_model[]` / `by_node[]` / `by_error[]`（环形图/条形图直接用） |
| 为什么慢（attempts + phases） | `attempts` 四段柱状 + `phases` 三段堆叠（queue→ttft→gen） |
| by_model 表 | `by_model[]`（`requests/ok/fail/success_rate/p95_ms/prompt_tokens/completion_tokens`） |
| by_node 表 | `by_node[]`（`name/requests/ok/fail/success_rate/avg_ms`；卡头注明「按名称聚合」） |
| 页头标「未映射」提示 | `unmapped.count`（>0 时展示徽章，明细跳调用日志） |

一次请求即可喂满 6 块，**不要**再让前端拉 `/api/call-log` 全量明细做本地聚合。

---

## 9. 已知限制 / 风险

1. **冷路径 ~0.4–0.5 s**（生产 233k 行 / 4 核）。TTL 20 s + singleflight 已把冷扫描压到「每 range 每 20 s 最多一次」；
   热路径与 p95 都是微秒级。若产品要求**冷启动首屏**硬性 <300 ms，需要把累计下沉到桶吸收阶段
   （改动桶结构/守恒不变量，延后决策），或再优化 ts 解析（当前冷扫描的大头是逐行 `time.Parse`）。
2. **分位是直方桶插值**：单样本桶精确，多样本桶在桶内均匀假设下近似（最大桶宽即误差上界）；
   `max_ms` 与 TTFT max 是精确值。前端展示到整数 ms 足够。
3. **by_node 是「最后尝试节点」口径**，不是「所有触达节点」；如果产品要「节点被尝试次数」，
   需要另加 `by_node_attempts`（本次不做，避免同一张表两个分母）。
4. **phases 的 queue/gen 依赖 `events[]`**：老日志行没有事件时这两个均值只覆盖有事件的子集，
   分母不是全量（响应未暴露分母，文档已注明）。
5. **缓存命中时 `generated_at` 不是当前时刻**（≤20 s 偏差），前端不要拿它做「数据实时性」判定。
6. **不做持久化缓存**：进程重启后第一次请求重新冷扫描。
7. **未映射判定与桶吸收同源**：有显式键的行采信落盘值（请求时刻冻结），老行按
   「公式 B && !everServed」回退；目录漂移不会追认历史记录（判定见 §3.9）。
8. **everServed 的时序边界**：老行回退用的是当前 everServed（单调只增）。若某模型在回填之后
   才首次成功，其回填前的失败行会被扫描算回、而桶里仍算未映射 ⇒ 这两类记录的对账会出现
   极小偏差；生产当前快照无此情况（全部行同一轮回填），测试夹具也不构造该跨轮场景。

---

## 10. 测试与门禁

| 测试 | 断言 |
| --- | --- |
| `TestAggregateTotalsAndTimeseriesMatchStats` | 4 个 range：totals == `/api/stats`、timeseries == `/api/stats/trends` 逐桶、by_model 合计与 token 同源、unmapped 同源 |
| `TestAggregateEmptyDataset` | 无文件：全 0、`timeseries==[]`、数组字段非 null |
| `TestAggregateSingleRecord` | 单条：totals、p50=p95=p99=max、attempts、by_node |
| `TestAggregateAllFailed` | 全失败：success_rate=0、by_error 合计==失败数、attempts |
| `TestAggregatePercentilesKnownDistribution` | 已知均匀分布：p50/p95/p99 在期望值 ±1 桶内、max 精确、TTFT 分位 |
| `TestAggregateAttemptsPhasesAndErrors` | attempts 0/1/2/3+ 分开、phases 公式（queue=40ms, gen=760ms）、by_node 最后尝试节点、`node_agg_by` |
| `TestAggregateByErrorKinds` | 7 类错误分类齐全（upstream_5xx/4xx、timeout、connect_error、stream_interrupt、cooldown、other）、样本非空且 ≤200 字节 |
| `TestAggregateByModelP95DistinguishesModels` | 快/慢两模型各自 p95，不用全局直方图 |
| `TestAggregateWindowBoundaries` | 窗口下界排他/上界包含，与 `/api/stats` 一致 |
| `TestAggregateUnmappedConsistency` | 未映射不计入 totals，也不进 attempts/by_node/by_error（对账恒等）、count/by_model 与 `/api/stats` 同值 |
| `TestAggregateProductionOrderInvariance` | 生产真实序（完成序追加、ts 乱序）与 ts 有序序结果逐字节一致；两次调用稳定 |
| `TestAggregateCacheKeyIncludesRangeAndDate` | 键含 range 与当日日期、跨零点变化 |
| `TestAggregateCacheTTLAndInvalidation` | TTL 内返回缓存；失效后重算；不同 range 不串 |
| `TestAggregatePerformanceWarmUnder300ms` | 生产快照冷/热各一次，热 <300 ms |
| `TestAggregateConcurrentSingleflight` | 16 并发结果逐字节一致（`-race` 通过） |
| `TestAggregateHandlerShapeAndValidation` | Content-Type、range 归一、window/generated_at、405 |
| `TestAggregateTokenCaliberUnchanged` | `total_tokens == prompt+completion`（2b 口径） |
| `TestAggregateOldRowsUnmappedExcludedFromScanDims` | **手写原始 JSON 行并删掉 `unmapped` 键**造老格式行（结构性盲区修复）：4 窗口 `Σattempts/Σby_node/Σby_error` 全部对账、未映射行的节点/耗时进不来、everServed 老行保留 |
| `TestAggregateProdSnapshotReconciliation` | 生产快照 233,654 行（可用 `IP4AGG_PROD` 指定）：4 窗口扫描维度与桶口径对账；180d `totals=232,263`、`Σattempts` 差 0、`fail=23,633`、`unmapped=1,391` |
| `TestAggregatePerModelP95UnboundedOwnMax` | 模型 A 3×700,000ms、B 1×900,000ms：A 的 p95=700000（不借全局 max） |

变异测试（`.research/t67/mutation_harness.py`，7/7 CAUGHT，真实断言失败而非编译/setup 失败）：

| 变异 | 被哪条测试抓住 |
| --- | --- |
| M18 分位桶算错（所有值塞进第 0 桶） | `TestAggregatePercentilesKnownDistribution` |
| M19 by_model 漏算失败（`fail` 恒 0） | `TestAggregateTotalsAndTimeseriesMatchStats` |
| M20 attempts 把 0 与 1 合并 | `TestAggregateAttemptsPhasesAndErrors` |
| M21 缓存键漏 range | `TestAggregateCacheKeyIncludesRangeAndDate` |
| M22 by_model p95 用全局直方图 | `TestAggregateByModelP95DistinguishesModels` |
| M23 **老行未映射不回退**（只认显式 `unmapped` 键，即 D1 修复前行为） | `TestAggregateOldRowsUnmappedExcludedFromScanDims` |
| M24 **per-model p95 仍借全局 max**（即 D2 修复前行为） | `TestAggregatePerModelP95UnboundedOwnMax` |

复跑证据（D1/D2 修复后，最终代码）：
- 全量：`go test -skip '^TestZZIP4VD1DayOnlyTrendsEqualToSingle$' -count=1 ./` → **`ok 189.049s`**
  （`.research/t67/evidence/full_suite_after_d1d2.out`）。`-skip` 的是**验收方探针自身的时间依赖缺陷**
  （本地 00:00–01:59 时 `now-25h` 与 `now-2h` 落在同一日期、两个日文件同名互相覆盖；
  复现见 `.research/t67/evidence/zz_d1_probe_time_flake.txt`），与本接口/本任务改动无关。
  其余全部用例（含 22 个 `TestZZIP4V*` 探针与验收方 14 个 `TestIP4Agg*`）均通过。
- `go test -race -run 'TestAggregate|TestIP4Agg' -count=1 ./` → **`ok 69.278s`，0 DATA RACE**
  （`.research/t67/evidence/race_after_d1d2.out`）。
- 本接口 20 个用例：`go test -run 'TestAggregate' -count=1 -v ./` → 20/20 PASS
  （`.research/t67/evidence/aggregate_tests_green_after_d1d2.txt`）。
- `go vet ./` 干净、`gofmt -l` 空。
- 修前红：`.research/t67/evidence/red_d1d2_before_fix.txt`（把 D1 回退与 D2 传参退回修复前行为，
  新用例真实报错：7d `Σattempts=27774≠27044`、A 的 p95=900000≠700000）。
- 证据目录：`.research/t67/evidence/`（修前红、修后绿、变异、性能、哈希）。

---

## 11. 边界与不可变约定

- 未改 `/api/stats`、`/api/stats/trends`、`/api/call-log` 的**现有**契约；只新增
  `/api/logs/aggregate` 路由与 handler。
- 未改 `proxy_node.go`、`node_alert.go`、`static/admin.html`、`ROADMAP`；
  未碰生产容器、未用 8000 端口、未执行 git 写操作。
- 只改 `calllog.go`（聚合/缓存/分位/并行扫描/老行未映射回退/每模型 max，+778 行）、
  `statscan.go`（扫描解析器 + `SawUnmapped`，+282 行）、`main.go`（handler + 路由，+13 行）+
  新测试 `log_aggregate_test.go` + 本文件。**三文件删除行数均为 0**（纯新增）。
- 代码哈希（D1/D2 修复后，本次交付）：`calllog.go 2975b1a3…`、`statscan.go 477b5acd…`、
  `main.go b6a829c2…`、`log_aggregate_test.go 022c050f…`。

---

## 12. 修复记录（task-68 独立验收 D1 / D2）

> 背景：verify-403 的独立验收报 1 blocker + 1 minor（报告 `docs/research/log-aggregate-verification.md`）。
> 本节记录修复前后证据，便于复验方逐项对照。

### 12.1 D1（blocker）：老格式行未映射泄漏进扫描维度

- **现象**（验收方在生产快照 233,654 行上复算）：`totals` 排除了未映射，但
  `attempts` / `by_node` / `by_error` / 延迟分位没有。7d `Σattempts=27,774` vs totals `27,044`（+730）；
  30d +1,352；180d `Σattempts=233,654` vs totals `232,263`（+1,391）；180d
  `Σby_error=25,024` vs fail `23,633`（错误面板失败数虚高 5.9%）；未映射记录的
  `99,999,999ms` 还成了 `latency.max_ms`。
- **根因**：桶吸收的判据是「显式 `unmapped` 键优先，否则公式 B + `!everServed`」，
  而 `statscan.go parseAggLine` 只认显式键、没有回退；生产 **99.88%** 的行没有该键
  ⇒ 1,391 条「公式 B 判未映射」的老行进了扫描维度。原 17 个用例抓不到是**夹具表达能力盲区**：
  `CallRecord.Unmapped` 没有 `omitempty`，`t56WriteLines` 序列化时每行必带 `"unmapped":false`，
  永远造不出老格式行。
- **修复**：
  1. `statscan.go`：`aggScanRec` 增 `SawUnmapped`，`parseAggLine` 见到 `unmapped` 键即置位；
  2. `calllog.go consumeAggLine`：按 §3.9 判据回退 —— 有键采信落盘值（`status=="ok"` 除外），
     无键用 `isUnmappedByFormula(model) && !isServedModel(model)`；按模型名记忆化
     （每 worker 私有 map），233k 行只做 ~18 次公式 B 判定，冷扫描无可测退化。
- **修复后生产快照对账**（`TestAggregateProdSnapshotReconciliation` 实测）：

  | range | totals | Σattempts | Σby_node | Σby_error | fail | unmapped | latency.max |
  | --- | --- | --- | --- | --- | --- | --- | --- |
  | today | 367 | 367 | 367 | 8 | 8 | 0 | 119,974 ms |
  | 7d | 27,044 | 27,044 | 27,044 | 7,044 | 7,044 | 730 | 1,119,192 ms |
  | 30d | 157,769 | 157,769 | 157,769 | 11,120 | 11,120 | 1,352 | 2,832,125 ms |
  | 180d | 232,263 | 232,263 | 232,263 | 23,633 | 23,633 | 1,391 | 2,832,125 ms |

  （`Σby_node` 列是求和，不是节点行数；四窗口与 `/api/stats` 的 totals/unmapped 逐项相等。）
- **测试**：新增 `TestAggregateOldRowsUnmappedExcludedFromScanDims`（手写原始 JSON 行并删 `unmapped` 键，
  4 窗口对账 + everServed 老行保留 + 未映射节点/耗时排除）与生产对账用例；
  修前红证据 `.research/t67/evidence/red_d1d2_before_fix.txt`；变异 M23（去掉回退）被抓红。

### 12.2 D2（minor）：per-model p95 在无上界桶借用了全局 max

- **现象**：模型 A 三条 700,000ms（全落 `[600000,+∞)`），模型 B 一条 900,000ms 把全局 max 拉到 900s，
  A 的 `p95_ms` 输出 900,000 而非 700,000。
- **根因**：`calllog.go` 组装 by_model 时给 `aggHistQuantile` 传的是全局 `scan.maxDur`。
- **修复**：`aggScanResult` 增 `perModelMax map[string]float64`（结构体/初始化/merge/consume 共约 10 行），
  `by_model.p95_ms` 改传该模型自己的 max；全局 `latency.max_ms` 语义不变。
- **测试**：`TestAggregatePerModelP95UnboundedOwnMax`（A=700000、B=900000、全局 max=900000）；
  变异 M24（仍传全局 max）被抓红。

### 12.3 复跑结论（最终代码，见 §10 证据）

- 本接口 20/20 用例 PASS（17 原有 + 3 新增）；
- 变异 7/7 CAUGHT（M18–M24）；
- 全量 `ok 189.049s`（skip 验收方时间依赖探针，见 §10）、`-race ok 69.278s`、`go vet` 干净、`gofmt -l` 空；
- 性能：冷 317–383 ms、热 3.6–4.3 µs（§6）。
