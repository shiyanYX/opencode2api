# task-39 Part B 独立验收报告（`everServed` + F1 展示层过滤）

- 验收人：`capture-429`（独立验收，不参与实现）
- 放行时间：2026-10-20 10:03（Lead 发「Part B 开始」+ 权威 pin 表）
- 落盘路径：`.research/t41/task39-partB-acceptance.md`
- **结论：见 §0（按老规矩，先跑命令、再下结论；本文件所有数字均来自本轮实际输出）**

---

## 0. 结论

**通过。**

- 权威 pin 81/81 逐文件校验通过，本轮运行期间 pin **零漂移**（`pin-end == pin-freeze`）。
- isolation（F1 主验收）：四个窗口 `total_requests`/`ok`/成功率**逐位不变**，只少了「窗口内 0 成功的模型」：
  180d **6 个模型 / 15 条请求 / token 0 / 流式 0**，`models 22→16`；7d 3/9；30d 4/10；today 2/2。
- 按模型趋势出口（Q1）与 `/api/stats` **同一判据、双向模型集相等**；热力图分支与无 range 响应 **sha256 逐字节不变**。
- e2e 汇总（线上口径→目标口径）：`total` 只减少未映射请求（1,391 / 1,352 / 730），F1 的 15 条仍在分母里。
- 行序无关（整数与计数完全一致）、重启时序 4/4（含 3 次回填前 cold）、**变异 M1–M8 全红**。
- 唯一非绿项：实现者测试 2 条**预期失败**（T30×2，Lead 已声明且我独立复核与本任务无关）→ 记为 known-red，不阻断。
- 本轮**未纳入**：`-race`（非交付项，理由见 §8）、面板改动（Lead 裁决不在验收范围）。

不通过项/阻断项：**无**。

---

## 1. 验收对象与 pin

### 1.1 权威 pin（Lead 提供，`/tmp/t41b/pin_lead.txt`）

```console
$ sha256sum /tmp/t41b/pin_lead.txt
5f7158de4ace768ed0917a5d15624f4f3805722a03b8eae71caf4c2ae2203e1e  /tmp/t41b/pin_lead.txt
$ wc -l /tmp/t41b/pin_lead.txt
81 /tmp/t41b/pin_lead.txt        # 仓库根全部 *.go（80 个，含测试）+ free_models_cache.json
$ sha256sum -c /tmp/t41b/pin_lead.txt | grep -c '成功'
81
```

逐文件校验 81/81 通过（原始输出见 `$WORK/results/pin-check.txt`）。面板 `static/admin.html` 按 Lead 裁决**不在 pin 范围内**
（并发面板任务在改），本轮**不验收**「面板零改动」。

### 1.2 本轮构建（全部由当前 pin 的源码构建）

| tag | 来源 | 用途 |
|---|---|---|
| `new` | 冻结副本（= pin 内容） | 被测实现 |
| `nofilter` | 冻结副本 + **仅中和 F1 过滤**（变异 M5 的补丁） | isolation 基线（单变量） |
| `old` | `git archive HEAD`（`2854784`） | e2e 基线 = **线上现状口径** |

`old` 不含 task-21 公式 B / everServed（独立核实：HEAD 源码里 `isUnmappedByFormula`、`unmappedBuckets`、`unmapped_requests` 命中数均为 0），
所以 `old` 与 `new` 的差异 = task-21 未映射口径 + task-39 everServed + F1 过滤的**合集**，仅作汇总账，不与 F1 的收窄口径混谈。

---

## 2. 方法与独立性声明

1. 全部证据来自**本轮在本机重新执行的命令**；实现方的 12 条变异结果**未被引用**，M1–M8 是在冻结 pin 上自己注入、自己跑出的。
2. 夹具：从生产快照生成，**233,045 行**（`/tmp/t41/live.jsonl`，sha256 `c8783886…`），不联网（`HTTP_PROXY=127.0.0.1:9` 黑洞，文档/目录抓取确定性失败，
   判定只用本地 `free_models_cache.json`），服务端口 9xxx，`-password ""` 仅在本机回环。
3. 全文「通过」= 对应命令退出码 0 + 断言为空；「必红」= 注入变异后测试出现基线没有的 `--- FAIL`。
4. 环境：Go `go1.26.5 linux/amd64`、HEAD `2854784`、夹具 sha256 `c8783886…`（233,045 行）、运行总耗时 10:31:50–10:41:47（约 10 min，
   不含早前两轮因我方脚本 bug 作废的试跑，原始日志保留在 `$WORK/results/run.log`）。

---

## 3. isolation 主验收：F1 展示层过滤（单变量）

基线 `nofilter`（只中和 F1）vs `new`，同一夹具、同一小时内抓取。

### 3.1 四个窗口：只删键，不动分母

| 窗口 | total_requests | ok | 成功率 | models | 被删（模型/请求） | unmapped |
|---|---|---|---|---|---|---|
| `today`（整点滚动 24h） | 2,059 → **2,059** | 2,017 → **2,017** | 97.960175% → **97.960175%** | 10 → **8** | 2 / 2 | 728 |
| `7d`（日历日） | 27,739 → **27,739** | 20,576 → **20,576** | 74.177151% → **74.177151%** | 12 → **9** | 3 / 9 | 730 |
| `30d`（日历日） | 173,846 → **173,846** | 159,441 → **159,441** | 91.713931% → **91.713931%** | 14 → **10** | 4 / 10 | 1,352 |
| `180d`（日历日） | 231,654 → **231,654** | 208,037 → **208,037** | 89.805054% → **89.805054%** | 22 → **16** | 6 / 15 | 1,391 |

`Δrate = 0.000000 pp`（四个窗口全部相同）——被删请求全部 `ok=0`，所以**分子分母都没动**，这不是「成功率变了」，而是同一口径下少显示了 0 成功的行。

### 3.2 被删清单（180d，6 模型 / 15 条 / token 0 / 流式 0）

```console
$ python3 api_diff.py --mode isolation --old nofilter --new new | grep -A7 stats_180d
    - beam-free                          requests=   2 ok=0 tokens=0 stream=0 trend_pts=180
    - jev-1.13-free                      requests=   1 ok=0 tokens=0 stream=0 trend_pts=180
    - muse-spark-1.2-contributor-free    requests=   2 ok=0 tokens=0 stream=0 trend_pts=180
    - muse-spark-1.3-contributor-free    requests=   1 ok=0 tokens=0 stream=0 trend_pts=180
    - nemotron-3.5-super-vl-free         requests=   6 ok=0 tokens=0 stream=0 trend_pts=180
    - ox-alpha-free                      requests=   3 ok=0 tokens=0 stream=0 trend_pts=180
```

断言：`Σmodels.request_count == total_requests - 15`（180d：231,639 = 231,654 - 15）；被删模型 prompt/completion/cache 全 0、`stream_req_count` 全 0 ✓
（C 条：token 影响必须为 0；Lead 要的「精确清单 + 说明 15 条仍在 total 与成功率分母里」即此表）。

### 3.3 按模型趋势出口（Q1）：同一判据、双向一致

```console
NOTE: trends_7d_model: 双向模型集一致（9 个：保留都在、无多余）
NOTE: trends_7d_model: 模型序列 12→9，删 3 个=['beam-free','jev-1.13-free','nemotron-3.5-super-vl-free']
NOTE: trends_180d_model: 双向模型集一致（16 个：保留都在、无多余）
NOTE: trends_180d_model: 模型序列 22→16，删 6 个=[…]
NOTE: Q1 一致性（7d）：/api/stats models=9 个；/api/stats/trends?group=model 仍含 9 个，差集=[]
```

- `/api/stats` 与 `/api/stats/trends?group=model` 的被删集合**完全相同**（同窗口），两个方向都验了（保留模型一个不少 + 无多余模型）。
- 每个窗口的序列点数：被删模型的整条序列消失，保留模型的点逐字面相等。

### 3.4 热力图分支（非 model 维度）与无 range：逐字节不变

```console
nofilter == new 的响应 sha256 对照：
stats_norange        78234cdfbee973c8… == 78234cdfbee973c8…   相同  (32 B)
heatmap_180d         f7bdd2757ff811c7… == f7bdd2757ff811c7…   相同  (26,260 B)
heatmap_7d           31d8dc43c452ea81… == 31d8dc43c452ea81…   相同  (1,153 B)
model_trends_180d    13d705bd697029f5… != be95ee2c39b331ce…   不同  (665,010 → 483,030 B，22→16 模型)
```

- **无 range `/api/stats`**：`sha256=78234cdfbee973c8d1d0062885528cc5bedfe13283e7d40ab3c222e05d3b6f1d`（前后一致，A 条绝对断言 ✓）。
- **热力图分支** `/api/stats/trends?range=180d|7d`（面板 `admin.html:1172` 的图表数据源）逐字节不变 ✓——
  实现有意不筛该分支（它返回日总量，无 model 键；筛了等于把 15 条真实失败从日总量里藏掉）。

---

## 4. e2e 汇总账：线上口径（HEAD）→ 目标口径

`old`（不含量化口径）vs `new`。**与 §3 分开报，不合并成「一个变化」。**

| 窗口 | total | models | 其中：未映射被排除 | 其中：F1 从展示层删 | 结果 |
|---|---|---|---|---|---|
| `today` | 2,787 → 2,059（-728） | 11 → 8 | 728 条（space-bunny） | 2 条 / 2 模型 | ✓ |
| `7d` | 28,469 → 27,739（-730） | 15 → 9 | 730 条（space-bunny 728 + 2 个探针名） | 9 条 / 3 模型 | ✓ |
| `30d` | 175,198 → 173,846（-1,352） | 18 → 10 | 1,352 条 | 10 条 / 4 模型 | ✓ |
| `180d` | 233,045 → 231,654（-1,391） | 30 → 16 | 1,391 条 | 15 条 / 6 模型 | ✓ |

- `total_requests` 的下降量 == `unmapped_requests.count` == Part A 独立算出的排除量（1,391 / 1,352 / 730）✓（跨来源三方一致）。
- F1 删掉的 15 条**仍在 total 里**（`Σmodels = total - 15`），e2e 断言按此口径校验。
- 聚合趋势（热力图）请求数差值 == 未映射条数（1,391 / 730）✓；`/api/call-log` 详细层条数与内容不变，仅多出 `unmapped` 键（task-21 已批）✓。
- 本夹具是 **233,045 行**（08:52 快照）；实现方报告用的是 233,152 行快照、`total_requests=231,761`。两者差 107 行/107 条请求，
  来源是**快照时间不同**（生产日志实时增长），逐项口径一致；本报告**以本夹具为准**，不去凑实现方数字。

---

## 5. 行序无关性

原序 vs 定 seed 乱序（同一多重集），两个独立进程各跑一遍：

```console
$ python3 order_test.py --tag new
✅ 行序无关性通过（原序/乱序逐字节一致；绝对数值亦符合 Part A）
byte_equal: {stats_norange: True, stats_today: False, stats_7d: False, stats_30d: False,
             stats_180d: False, trends_180d_model: True, calllog_2000: False}
- stats_*: 字节不同但数值等价（浮点最大相对误差 2.88e-16；整数与计数完全一致）
- calllog_2000: 已排除行序比较（该端点=最近 2000 条 tail 窗口 + 到达顺序，乱序输入下本应不同）
```

**如实记录**：`models` 里的 `throughput_tps` / `avg_output_speed` 是浮点累加，行序不同会有 **1e-16 量级末位差异**（浮点加法不满足结合律），
**所有整数字段（requests/ok/fail/tokens/models 数/unmapped）逐项完全一致**，无 range 与按模型趋势端点逐字节一致。
另：绝对数值交叉核对通过 —— 180d `total=231,654 / models=16 / unmapped=1,391`、30d `173,846/10/1,352`、7d `27,739/9/730`，
180d `unmapped.by_model` 与 Part A 完全一致（`space-bunny 728, mimo-v2.5 624, x-preview-f 30, claude-opus-4-5 4, hy3 2, probes 2×1, gpt-4o-mini 1`）。

---

## 6. 重启时序：回填前的请求不会被误冻结

用例 `nopickle`（文档缓存去掉 `big-pickle`，使 everServed 成为唯一防线）：3 次 cold（TCP 通了就发请求，**全程不查 stats**＝不触发回填）+ 1 次 warm 对照。

```console
cold1 pre_query=False P1_after_start=12396ms backfill=None P1.unmapped=False P2.unmapped=True P3.unmapped=False
      models=16 big-pickle=True space-bunny=False trend_models_eq=True total=231656
cold2 pre_query=False P1_after_start=5303ms  backfill=None P1.unmapped=False P2.unmapped=True P3.unmapped=False
cold3 pre_query=False P1_after_start=5794ms  backfill=None P1.unmapped=False P2.unmapped=True P3.unmapped=False
warm  pre_query=True  P1_after_start=25955ms backfill=496ms P1.unmapped=False P2.unmapped=True P3.unmapped=False
```

- **P1**（`big-pickle`：历史有成功、文档表已无）在**回填前**被冻结为 `unmapped=false` ✓——历史成功不是靠回填才生效的。
- **P2**（`space-bunny`：从未成功）冻结为 `unmapped=true` ✓；**P3**（回填后的 `big-pickle`）仍 `false` ✓。
- 概览 `models` 含 `big-pickle`、不含 `space-bunny`；`/api/stats` 与 `/api/stats/trends?group=model` 模型集合相等 ✓。
- 注：cold 模式 P1 请求耗时 5–12 s（上游不可达时的重试），整个窗口内没有任何 stats 查询，回填确实未发生。

---

## 7. 变异测试（自做，M1–M8）

基线（未变异，同一 `-run` 子集）：`exit=0 fail=[] 9.4~13.4s` —— 绿。**red 判据** = 出现基线没有的 `--- FAIL: TestX`。

| ID | 变异（我注入的） | 结果 | 抓到它的测试（本轮实际输出） |
|---|---|---|---|
| M1 | everServed 短路恒假（判定 + 回填快照都忽略历史成功） | **RED ✅** | `TestT39LiveSuccessFlipsRecordAndIsMonotone` |
| M2 | everServed 恒真（所有非空名直接 mapped） | **RED ✅** | `TestT39DegradesSafelyWhenLogUnreadable`、`TestT39LiveSuccessFlipsRecordAndIsMonotone`、`TestUnmappedPredicateUnknownNames` |
| M3 | 只写不读历史（回填 `ever := map{}`） | **RED ✅** | `TestT39EverServedBareNameKeepsHistoryInOverview`、`TestT39OrderIndependenceAcrossRestart`、`TestT39RealLogOverviewCriterion` |
| M4 | 删 `-free` 后缀分支 | **RED ✅**（子集漏测 → **全量复跑**抓到） | `TestT23DecisionMatrix`、`TestT23RealLogBackfillEqualsDirect`、`TestT23RealLogHardGate`、`TestT23ReverseRisk` |
| M5 | F1 过滤恒真（去掉 `OK==0` 判据 = 不过滤） | **RED ✅** | `TestAdminStatsHandlerWithRangeReturnsWindowedStats`、`TestT39OnlyModelsAndTrendsDifferFromUnfiltered`、`TestT39OverviewEmptyAndAllFailWindowsSafe`、`TestT39OverviewHidesZeroSuccessModels`、`TestT39RealLogOverviewCriterion`、`TestT39TrendsEndpointSameCriterion` |
| M6 | F1 过滤恒假（keep 传空 → 模型全删） | **RED ✅** | 8 条（含 `TestT39OverviewFilterKeepsTotalsAndRateIntact`、`TestTodayRangeResponseModelsAndTrendsAgree`） |
| M7 | 半实现 A：只删 `models`、漏 `/api/stats` 的 `trends_by_model` | **RED ✅** | `TestAdminStatsHandlerWithRangeReturnsWindowedStats`、`TestT39OnlyModelsAndTrendsDifferFromUnfiltered`、`TestT39OverviewHidesZeroSuccessModels`、`TestT39RealLogOverviewCriterion` |
| M8 | 半实现 B（Q1）：`/api/stats` 已筛、`trends?group=model` 出口未筛 | **RED ✅** | `TestT39RealLogOverviewCriterion`、`TestT39TrendsEndpointSameCriterion` |

方法说明（避免「假红/假绿」）：
1. 基线先跑，必须绿；每条变异 `find` 在冻结 pin 上**唯一命中**（`--check-only` 10/10 OK），命中数≠1 直接报 `MUTATION_DRIFT`，不静默跳过。
2. 为省时间先用相关测试子集，**若某条变异仍绿则自动用全量套件复跑该条**（`--full-suite-on-green`）——M4 正是这样被抓到的：
   子集内 4.3s 绿，全量 152.8s 红（T23 四条），说明「子集绿 ≠ 测试有效」这一步不能省。
3. 全量复跑时排除 Lead 已声明的 2 条无关失败（T30×2），只把其余新增失败计入 red（原始输出保留在 `mutations.json`）。
4. **Lead 点名的「M5/M7/M10 必红」对应我这份清单的 M5 / M7 / M8**（我的清单只有 M1–M8，无第 10 条）：
   M5=过滤恒真、M7=只删 models 漏 trends_by_model、M8=trends 出口未过滤——**三条均已红**。

---

## 8. 实现者测试基线

```console
$ cd <冻结副本> && go test -count=1 -timeout 30m -skip 'TestT29PlainIsByteIdenticalToEmbeddedPanel' ./
--- FAIL: TestT30GateNowJudgesFixedSubsetDead (1.92s)
--- FAIL: TestT30CriterionCPoolEmptyReachability (0.01s)
FAIL	github.com/shiyanYX/opencode2api	230.080s
```

- 失败集合**恰好等于** Lead 说明的两条预期失败，无新增失败 ✓。
- 独立复核这两条与本任务无关：
  - 断言内容为节点池判死（`zz_t30_alert_safety_test.go:122`「第 2 轮 dead=0，期望 92（task-34 的核心修复）」、
    `:227`「第 4 轮 dead=0，期望 10」）；
  - `grep -cE 'unmapped|everServed|modelsWithOKInWindow|filterTrendsByOK|TrendsByModel' zz_t30_*.go zz_t34_*.go` = **全 0**。
- `TestT29PlainIsByteIdenticalToEmbeddedPanel` 按 Lead 裁决 `-skip`：它比编译期内嵌面板与磁盘文件，而面板正被并发任务改写，**不在本次验收范围**。

`-race`：**本轮跳过（非交付项）**。原因：4 核机上全量 race 需 10 min+，且与变异/服务测试抢 CPU 会互相拖慢、风险外溢到其它阶段；
本任务需要覆盖的并发/顺序风险已由替代证据覆盖：全量 `go test`（157–230 s）、行序双进程对照、3 cold + 1 warm 重启时序、8 条变异。
如实记录为 `6c-go-test-race=skipped`，不是「通过」。

---

## 9. 措辞与面板（信息性，不参与判定）

- 实现者交付报告 `docs/research/overview-vs-detail-served-models.md` 已落盘（580 行，出现「口径」19 次），措辞抽查一致。
- 措辞红线扫描（`成功率提升|提升成功率|服务改善|性能提升|服务变好`）：原始命中 7 行，**排除否定语境后 0 行** → 未发现把口径更正说成「提升/改善」的话术 ✓。
- 面板（信息性，不参与判定）：Part B 期间 `static/admin.html` **未变**（md5 `4fe4f101…`、sha256 `84388d30…`、2,496 行）；
  面板不引用 `unmapped/everServed/servedModel`（0 处），使用 `trends_by_model` 7 处（过滤后仍可正常渲染）。

---

## 10. 残留风险与已知限制

1. **everServed 只增不删**：若 `space-bunny` 将来成功一次，历史行会被重新判为 mapped，`today` 概览会从 ~97.96% 掉回 ~73.7% 一类量级——
   这是口径特性（不是本次缺陷），但**用户会看到数字跳变**，建议面板/说明里不要承诺「历史不变」。
2. **已冻结的 `unmapped` 键不再重判**：目录刷新后同一模型名可能出现「历史段未映射、新段映射」的分裂（task-21 的固有设计，已在 Part A 记为 R1）。
3. **浮点末位**：行序会带来 1e-16 相对误差（见 §5），若将来把 `throughput_tps` 提升为「逐字节稳定」的验收项，需要改成定点/整数累加——
   本轮不作为失败项。
4. **`Σmodels.request_count ≠ total_requests`**（差 15 条 / 0.0064%）：这是 Lead 裁决的既定副作用，面板已要求以 `total_requests` 为分母；
   本轮不校验面板实现。
5. 夹具快照与生产实时日志有 ~100 条差（见 §4），不影响判定口径。

---

## 11. 复现

```bash
# 环境
export WORK=/tmp/t41b REPO=/home/jingyx/project/opencode2api-test
export PATH=$PATH:/usr/local/go/bin GOCACHE=$REPO/.gocache GOMODCACHE=$REPO/.modcache
source /tmp/t41b/lib.sh

# harness（已归档 .research/t41/partB/）
touch /tmp/t41b/START && bash /tmp/t41b/run_partB.sh   # 全流程
python3 /tmp/t41b/api_diff.py --mode isolation --old nofilter --new new
python3 /tmp/t41b/api_diff.py --mode e2e       --old old      --new new
python3 /tmp/t41b/order_test.py --tag new
python3 /tmp/t41b/restart_test.py --tag new --repeat 3
python3 /tmp/t41b/mutate.py --full-suite-on-green --run 'T39|Unmapped|StatsRange|ModelStats|Trends|TodayRange|Window'
```

证据目录：`/tmp/t41b/results/`（`summary.md`、`pin-check.txt`、`api-diff-{isolation,e2e}.json`、`order-test.json`、
`restart-test.json`、`mutations.json`、`sha-evidence.json`、`rate-evidence.json`、`gotest*.log`）；harness 归档：`.research/t41/partB/`。

---

## 12. 一句话交接

everServed + F1 展示层过滤**通过独立验收**：分母与成功率逐位不变，只让 6 个「窗口内 0 成功」模型（180d / 15 条 / token 0 / 流式 0）从两个展示出口消失；
无 range 与热力图分支逐字节不变；行序/重启安全；M1–M8 变异在自己手上全红。唯一非绿是 2 条与本任务无关的 T30 预期失败。
