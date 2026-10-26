# Task-109 / Task 1 独立复核

- 复核人：verify-c5v2（独立 reviewer，不是实现者）
- 复核对象：当前工作区 `node_scoring.go` + `node_scoring_test.go` 的实际字节内容
- 复核依据：`docs/superpowers/plans/2026-10-26-node-selection-v2.md` 的 Task 1 / Global Constraints / Review Focus；`docs/research/scoring2/06-task1-report.md`；`/tmp/review-t1.diff`（dee27cf..c288e5a，2,292 行）
- 文件指纹：
  - `node_scoring.go` sha256 `4e6a302fce26d20d5a6f97feddc2705d8bdfcceac41bfc813396875d4645bd89`
  - `node_scoring_test.go` sha256 `c4bc3bee3e2f4360bce856db9cb6cc7936ac56e74289f20aa6838991065cd83e`
- 复核时 HEAD 已从 Lead 告知的 `dee27cf` 前进到 `3ab39e9`；`852fc9c` 已包含 `node_scoring.go` 与报告，测试文件按仓库 `.gitignore:26`（`*_test.go`）保持本地、未入库。**本复核按工作区当前内容判定，不按某个尚未确认的 commit 判定。**
- 未修改任何仓库内 `.go` 文件；所有复算/变异均在 `/tmp/t1v/` 的临时副本完成。

## 结论

| 项 | 结论 |
|---|---|
| 规格符合性（Task 1 Acceptance） | **❌ 不符合** |
| 实现质量（可进 T4 验收） | **不通过** |
| 能否进入 T4 | **不能**（需先修 P0 级 403 跨记录归因问题并重跑验收） |

一句话结论：blip 不再给分子、探测恢复、五个正交字段、429 隔离、5 个变异都基本正确；但 **403 池级判定只在“当前记录”内生效，不能回溯同一 10 分钟窗口里更早那条记录的 403**，Task 1 指定的“两个订阅各有 403 ⇒ 两节点 EffN 都不增”跨记录用例实际失败。报告 §5 的 before/after 还因旧 blip 权重近似出现最多约 18.7 分的系统性偏低，不能作为验收证据。

---

## 1. 独立复算（不采用实现者 replay/compare 结果）

### 1.1 方法

自己写的 Python 回放：`/tmp/t1v/recompute.py`，只读输入：

- `/tmp/t109/call_log/{2026-10-20,21,22,23,24,26}.jsonl`（11,538 条，2026-10-20T23:15:34.923+08:00..2026-10-26T17:32:50.011+08:00；没有 10-25 文件）
- `/tmp/t109/scoring_state.json`（保存于 18:33:27，348 节点）
- `/tmp/lat/nodes.json`（17:38:13 API 快照，212 节点，旧 v1 字段）
- 实现值来自 `/tmp/t109/compare.csv` 的 `after` 列（与报告正文一致），**不是**复用实现者的回放代码

回放口径独立实现：事件分类、10 分钟 blip 桶、403 订阅窗口、`w = 0.5^(age/7)`、Wilson(α0=8, β0=2, z=1.645)、probe 斜坡/封顶、latency 分项。逐节点先验证 `attempts / success / fail` 与 state 是否对得上，再算新 `EffS/EffN/score`。

> 关键数据事实：state 的 `Attempts/BlipFails` 是**生命周期累计计数**，不是只含 6 天日志窗口。例如 `1::BR-45.38.67.97:7029` state `attempts=2293`、`blip_fails=444`，但只读副本里只有 15 次尝试、11 次 blip。因此对旧节点只能用“窗口内逐事件 + 窗口外上界”估算；这正是下面 Finding F2 偏差的来源。

### 1.2 指定节点

“直接重放”= 只用 10-20..10-26 日志、自己分类后重算；“状态差法”= 从 state 的旧 `EffS/EffN` 扣掉 `0.8 × 自己重放的 blip 权重`（用于和实现 after 做同口径对比）。所有分数取 state 保存时刻 18:33:27。

| 节点 | attempts（日志/state） | 自己 EffS/EffN（直接） | 自己分数（直接） | 状态差法分数 | 实现 after | 判定 |
|---|---:|---:|---:|---:|---:|---|
| `红杏::🇯🇵 日本02` | 1031 / 1031 | 0 / 259.624 | **41.0** | 42.2 | 42.2 | <50 ✅ |
| `红杏::🇭🇰 香港01` | 985 / 985 | 0 / 244.628 | **41.1** | 42.3 | 42.3 | <50 ✅ |
| `红杏::🇸🇬 新加坡原生` | 130 / 131 | 85.265 / 87.007 | **94.1** | 94.0 | 94.0 | >70 ✅ |
| `红杏::🇯🇵 日本原生` | 11 / 13 | 2.652 / 3.676 | **73.5** | 71.3 | 71.3 | >70 ✅ |
| `红杏::🇨🇳 台湾01` | 1 / 1 | 0 / 0.138 | **70.5** | 70.5 | 70.5 | >70 ✅ |
| `1::AU-81.21.233.45:5751` | 34 / 34 | 32.840 / 33.835 | **87.2** | 87.3 | 87.3 | 普通高分，差 0.1 ✅ |
| `1::JP-166.88.149.69:6533` | 0 / 0 | 0 / 0 | **72.4** | 72.4 | 72.4 | 普通高分，差 0 ✅ |

- 两档要求都满足：日本02/香港01 在两种独立算法下都 **<50**；新加坡原生/日本原生/台湾01 都 **>70**；AU 与 JP 两个普通高分节点与实现差 **0.1 / 0.0**。
- 日本02/香港01 的“直接重放”比实现 after 低约 1.2 分：state 的 blip 计数是 923/891，而只读日志按时间戳重放是 888/854（生产按落盘/摄入顺序遇到同一桶的边界不同），两边都远低于 50，阈值结论不受影响。
- 日本原生直接重放 73.5、实现 71.3：日志副本少 2 次旧尝试（13 vs 11），且实现 after 用了旧状态近似；两种算法都 >70。
- 日本02 状态差法仍留下 `EffS=6.269` 而不是 0，是 state 旧账近似扣减的残差；**代码对新产生的 blip 事件确实 `EffS += 0`**（变异 m1 可证明）。报告 after 并非从零重放，不能把 `a_effs=6.269` 当成“新公式下仍有 6 个成功”。

### 1.3 其它静态检查

| 检查项 | 结果 | 证据 |
|---|---|---|
| 429 不计质量 | ✅ | 自己的临时测试 `TestReview429NeverCountsAsQuality`：`http 429 quota_signal` / `http 429` / `quota_halt` 三连击后 `Attempts=0, EffN=0, EffS=0, BlipFails=0, QuotaEvents=3`。生产侧 `main.go:3194/3269` 的 detail 永远带 `http <code>`，分类器先命中 429。 |
| `pick(false)` 未被 T1 触碰 | ✅（仅 diff 范围） | `/tmp/review-t1.diff` 只有 report + `node_scoring.go` + `node_scoring_test.go` 三个文件；`pick` 不在这份 diff。工作区另有 `proxy_node.go`/`main.go` 的 Task2 改动，不能算到 T1。 |
| 分数不做选择权重 / 不做定时重评 / 不做触发器 | ✅ | T1 diff 没有新增选择路径；`node_scoring.go` 中没有把 score 当 weight 的代码；Ticker 是已有持久化轮询/保存，不是重评触发器。 |
| 五个字段 JSON 名/类型 | ✅ | 自己的 `TestReviewOrthogonalContract` 通过：`reach: float64`、`latency_score: float64`、`quota_state: string`、`last_ok_age_s: float64`（JSON 数值）、`auto_eligible: bool`；无成功 `last_ok_age_s=-1`；exhausted `quota_state=cooling`、`auto_eligible=false`。 |
| `scoringVersion=2` | ✅ | 常量在 `node_scoring.go:37`；自己的契约测试断言通过。 |

---

## 2. 变异测试（临时副本，仓库未改）

全部在 `/tmp/t1v/pkg`（工作区文件复制，非 in-place 改仓库）执行；每轮结束恢复并校验 sha256。日志：`/tmp/t1v/mutations.log`。

| # | 变异 | 运行测试 | 结果 | 红证据（节选） |
|---|---|---|---|---|
| m1 | blip 失败改回 0.8 成功 | `TestBlipFailureNeverCountsAsSuccess|TestScoringBlipSuppression` | **KILLED** | `eff_s=0.800，期望 0`；`eff_s=822.400，期望 0` |
| m2 | 65 分封顶时效 30min → 24h | `TestProbeRecoveryWithinHour` | **KILLED** | `最近失败 60 分钟后 score=65.0（caps=[probe_fail≤65]）` |
| m3 | 403 池级阈值 2 → 1 | `TestSingleSub403Blamed` | **KILLED** | `单订阅 403 cred=0，期望 2` |
| m4 | 删除「连续 2 次探测成功清零」 | `TestProbeRecoveryWithinHour` | **KILLED** | `probe_fail_24h 仍为 120，期望 0` |
| m5 | `auto_eligible` 不再排除 exhausted | `TestOrthogonalFields` | **KILLED** | `exhausted auto_eligible=true，期望 false` |

5/5 变异全部杀死；恢复后 `node_scoring.go` sha256 与原文件一致。满足计划“≥4 个变异”的要求。

---

## 3. 测试/构建状态

### 3.1 T1 定向测试

在临时副本执行：

```
go test -count=1 -run 'TestScoringParityWithReferenceScores|TestScoringBlipSuppression|TestBlipFailureNeverCountsAsSuccess|TestProbeRecoveryWithinHour|TestPoolLevel403NotBlamed|TestSingleSub403Blamed|TestProbePenaltyDecays|TestOrthogonalFields' .
ok   github.com/shiyanYX/opencode2api  0.058s
```

8 个指定测试全绿（包括实现者的 6 个新测试）。`go vet .` 退出 0；`gofmt -l node_scoring.go node_scoring_test.go` 无输出。

### 3.2 自己的补充测试

在 `/tmp/t1v/pkg/zz_review_t109_test.go`（不写入仓库）执行：

- `TestReview429NeverCountsAsQuality`：**PASS**
- `TestReviewOrthogonalContract`：**PASS**
- `TestReview403WindowAcrossRecords`：**FAIL**（见 Finding F1；这是本次复核的关键发现）

### 3.3 全量测试

`go test -count=1 ./...` 在临时副本运行 154s，**未全绿**，5 个失败：

- `TestReview403WindowAcrossRecords`：本次独立测试（F1）。
- `TestIP4RVLiveServedDrift`、`TestIP4RVDocsDrift`：IP4 聚合既有残留风险复现（R-A/R-B），与 T1 diff 无关。
- `TestT23RealLogHardGate`、`TestT23RealLogBackfillEqualsDirect`：临时副本缺少 `free_models_cache.json`，环境问题。
- 其余 T1 相关测试通过。**不能据此宣称“全仓全绿”**；但也没有发现 T1 改动引入的其它失败。

注：临时副本 `gofmt -l *.go` 有 11 个无关文件（Task2/T30/ip4v 的本地测试或新文件），T1 两个文件不在列表内。

---

## 4. Findings

### [Critical] `node_scoring.go:842-854` / `node_scoring_test.go:1008-1050` — 池级 403 不能回溯更早记录，跨记录同窗用例失败

**问题**：`record403Subs` 先只收集**当前记录**的 403 订阅，`distinct403` 再把 `s.sub403` 里过去 10 分钟出现过的订阅加进来；如果第一条记录只有一个订阅 403，它已经被计入 `Fail.Cred`/`EffN`，第二条记录出现另一个订阅 403 时只能影响第二条，**无法撤销第一条**。同一 10 分钟窗口内“两个订阅各有 403”的结果是：先到的那条记录的节点继续背 1 次 cred，后到的节点被豁免，而不是两个节点都不增。

**证据**（自己的测试，构造两条记录间隔 1 分钟）：

```
--- FAIL: TestReview403WindowAcrossRecords
    zz_review_t109_test.go:53: fp-red 跨记录同窗 403 未豁免：att=1 effn=1.000 cred=1 pool=0
```

实现自带的 `TestPoolLevel403NotBlamed` 把两个订阅的 403 放在**同一条** `CallRecord` 里（`node_scoring_test.go:1011-1023`），所以在“当前记录”路径上通过；它没有覆盖计划 Task 1 Step 1 所说的“同一 10 分钟窗口内两个订阅各有 403”（计划 `docs/superpowers/plans/2026-10-26-node-selection-v2.md:56-60`）。这是测试构造掩盖了实现缺陷，不是测试通过就代表规则成立。

**影响**：形态级 403 风暴中，先出现的节点被错误记成凭据失败；如果 10 分钟窗口里 403 交替出现，会随机惩罚一部分本应豁免的节点。虽然 10-24 实际数据全是红杏单订阅（本次复核 `pool403=0`，见 §5(c)），但规则不能只在“事后已知”的历史数据上成立。若产品决定采用“只向未来生效、不回溯”的语义，必须同时改 Task 1 的验收文本和测试；当前计划写的是“两节点 EffN 都不增”，所以按现规格判失败。

**建议**：在 `scoring403Tracker` 里保留窗口内已提交 403 的逐条贡献（node/fp、时间、权重、是否 blip、是否已计入），当某条记录使 `distinct403 >= scoring403PoolSubs` 时，**回溯扣掉窗口内所有被计过的 403 质量**并改为 `Pool403`/`pool_403`；或者把 403 证据延迟到窗口边界再提交。测试必须改成**两条独立记录**，并断言两个节点的 `EffN` 都为 0、`Pool403` 都为 1、reasons 都含 `pool_403`。

### [Important] `docs/research/scoring2/06-task1-report.md:134,394` — 存量 blip 权重用 0.7762 近似，after 表对 168/212 节点系统性偏低；不是 1–2 分

**问题**：state 的 `blip_fails` 是生命周期累计值；报告用 `W_blip = blip_fails × 0.7762` 还原旧证据，0.7762 是 6 天窗口的平均衰减权重，不是生命周期平均权重。窗口外证据年龄 >6.7 天，权重上界只有 `0.5^(6.73/7)=0.513`；用 0.7762 会**多扣** `EffS/EffN`，after 分数偏低。报告 §8 仍写“个别节点偏低约 1–2 分”，与实测不符。

**证据**：

- 212 节点中 **168 个**存在窗口外 blip 证据（state `attempts` > 6 天日志重放 attempts，且 `blip_fails` > 重放 blip 数）。
- 用**最保守**的上界（所有旧 blip 都按最老的 0.513 权重）计算：**148/212** 个节点的报告 after 低于这个保守下界；**20 个**偏低 ≥5 分，最大偏低 **18.7 分**。
  - `1::BR-45.38.67.97:7029`：state `attempts=2293`、`blip=444`，日志只重放出 15 次尝试/11 次 blip；报告 after **57.2**，保守下界 **75.9**，偏低 ≥18.7。
  - `1::CY-45.131.92.24:6635`：state `attempts=5604`、`blip=3597`，报告 after **72.5**，保守下界 **81.8**，偏低 ≥9.3。
  - `飞鸟云::hy2台湾06`：after **38.1**，保守下界 **47.0**，偏低 ≥8.9。
- 指定节点日本02/香港01/新加坡/日本原生/台湾01 的旧证据很少或没有，所以 §1.2 的阈值结论仍稳固；但报告“212 节点里 174 个下降、最大 −39.6”“before/after”这些全局结论不可直接采信。

**建议**：不要再用固定 0.7762。T4 的全量复算应按事件时间戳逐条求 `w`；若旧日志不可得，至少用**年龄上界**（≤0.513）给出区间，不要把 0.7762 的近似写到结论区。报告 §5 表需要重算，特别是旧节点（BR/CY 这类）不能继续显示被 T1 “打下去 20–30 分”的假象。

### [Minor] `docs/research/scoring2/06-task1-report.md:99` — “只有 4 个节点精确对账”属实，但这 4 个全是零样本冷启动节点，核心公式没有得到精确对账

**问题**：`TestScoringParityWithReferenceScores` 跳过任何 `BlipFails>0 || ProbeFail24h>0 || Fail.cred>0` 的节点后只剩 4 个；这 4 个在 `.research/scoring/node_stats.json` 里都是 `eff_s=0, eff_n=0`（无真实样本），只验证了先验 8/2 的冷启动路径。报告把它们称为“精确对账”，容易让人误以为 quality/Wilson/blip 公式已验证。

**证据**：`scores.json` 219 节点；独立统计满足 skip 条件的 4 个为：
`红杏::🇭🇰 香港01`、`红杏::🇰🇷 韩国-首尔`、`红杏::🇺🇸 美国01`、`红杏::🇺🇸 美国原生02`，四个的 `eff_s=eff_n=0`。其余 215 个全部被 skip。

**建议**：保留这 4 个的结构/冷启动断言，但不要把它当作核心公式的 parity 证据；T4 必须做 212 节点的完整重放（本复核 §1 已给出指定节点的独立结果）。如果要扩参考对账，至少把“旧账有证据但输入不受 Task-109 影响”的节点单独列出来，而不是靠 skip 后只剩零样本。

### [Minor] `docs/research/scoring2/06-task1-report.md:139,397` — 55 分钟 before/after 时间差 + 手工“清零”使恢复节点的精确增量不可验证

**问题**：before 是 17:38 API 快照，after 是 18:33 state；恢复节点的探测成功发生在两者之间。报告 after 还把 `ProbeOKStreak>=2` 的节点手工视为 `probe_fail_24h=0`，但代码只在**部署后新的 probe 成功**时才清空 `ProbeFailTimes`（`node_scoring.go:981-985`）；从 state 加载时不会自动清零。因此：
- after 表是“新代码 + 假设已经发生下一次成功探测”的结果，不是同一时刻的 A/B；
- 恢复节点的 +6.3/+10.9 等精确 delta 不是纯代码效果。

**证据**：报告自己承认 55 分钟差；代码清零逻辑只在 `observeProbe` 成功分支；`观察` 表中日本原生/台湾01 等恢复值正好与“手工清零”口径一致。生产上下一轮探测（15 分钟）会完成清零，仍满足“1 小时内回到 70+”的计划要求，**不是功能 bug**，但 after 表的 delta 不能当验收数字。

**建议**：T4 用同一份 state、同一个 `now` 做 before/after；恢复解锁需单独给“从失败时刻起 0/15/30/60 分钟”的时间序列，而不是跨快照差值。

### [Minor] `node_scoring.go:881-884` — pool 403 不计 `Attempts`，`sample_size` 会少算形态级尝试

**问题**：池级 403 分支只 `Pool403++`，既不 `Attempts++` 也不写 `Fail`/`EffN`。计划只说“该次不计质量”，没有说“不算尝试”；API 的 `sample_size` 来自 `Attempts`，形态级风暴期间会显著低估样本量。不影响分数，但面板/低样本判断会有偏差。

**建议**：明确语义；若 `sample_size` 是“真实尝试数”，pool 403 也应 `Attempts++`（但不加质量），并在报告中说明；否则在字段注释里写清 `Attempts` 不含 pool 403。

---

## 5. 五个疑点的裁决

### (a) “只有 4 个节点能精确对账，是否还能用更朴素复算覆盖更多？”

**裁决：4 个的数字属实，但“精确对账”样本没有质量证据；能覆盖更多，但不能靠旧参考分的 skip 规则。**

- 219 参考节点中，`blip_fails=0 && probe_fail_24h=0` 的只有 4 个；再加 `Fail.cred=0` 仍是 4 个，且四个都是 `eff_s=eff_n=0` 的冷启动节点。所以报告“只剩 4 个”是技术事实。
- 这 4 个只能验证“无样本先验 + 探测/延迟”路径；Wilson、blip、403 的公式没有被这组对账覆盖。
- 更朴素的覆盖方式是**对 212 个当前节点做自己的全量重放**（本次已对指定节点和两个普通高分节点完成；对 168 个旧节点需要事件级旧日志，当前副本没有，只能给区间）。T4 应做这个，而不是把 4 个零样本节点当成公式正确性的证明。

### (b) `blip_fails × 0.7762` 在 10-20 之前的存量节点上是否有系统性偏低？量级多大？

**裁决：有，而且是系统性的；量级远大于报告说的 1–2 分。**

- 根因：state 的 `blip_fails` 是生命周期累计值；0.7762 是 6 天窗口平均权重，窗口外旧证据的真实权重更小（年龄 >6.7 天 ⇒ ≤0.513）。
- 保守下界实验：对 168 个有窗口外证据的节点，把旧证据都按最大可能权重 0.513 计算，仍有 **148/212** 个报告 after 低于该下界；**20 个偏低 ≥5 分**，最大 **≥18.7 分**（BR-45：57.2 vs 下界 75.9；CY：72.5 vs 81.8）。
- 指定节点（日本02/香港01/新加坡/日本原生/台湾01）旧证据少，结论不受影响；但报告 §5 的全量“174 个下降、最大 −39.6”不能作为 T1 的验收依据。
- 修复方向：逐事件按时间戳算权重；旧日志不可得时，用年龄上界给出区间，不要用固定 0.7762。

### (c) 10-24 的 2,183 条 403 全是红杏——正确的节点归因，还是形态级误伤？

**裁决：按 Task 1 的规则是“正确的单订阅归因”，但根因是订阅/出口级，不是单节点质量；T2 的订阅隔离才是解药。**

- `.research/scoring2/03-empirical.md:82`：10-24 失败 2,392，其中 403 2,183（91.26%）；403 100% 来自红杏。其他订阅当天有约 481 次尝试、0 次 403，所以**不是“所有订阅都在 403”的形态级事件**。
- 自己的回放：212 个当前节点的 `pool403` 合计为 **0**；10-24 红杏 403 按单订阅进入 `Fail.Cred`/`EffN`，符合 Global Constraints“403 只有在单订阅集中时才算节点错”。
- 但这仍是“红杏订阅的上游/出口问题”，把 2,183 次平均摊到红杏各节点会拉低所有红杏节点（日本02 42.2、香港01 42.3、日本01 45.9）；不是节点自身的独立质量差异。T2 的 `sub403` 隔离/池级 halt 处理的是这个层面，T1 不应为此改规则。
- 结论：**不是池级误伤，规则上可以接受；要在报告里明确“订阅级根因”，不要把它说成“节点确实差”**。

### (d) 5 个 `success=0` 的节点仍 50–58.7，是否违反“0 成功不能看起来可用”？是否该加 `EffS=0 && EffN>=20 ⇒ cap 49`？

**裁决：不违反 Task 1；不建议在 T1 加这个 cap，T5 选择器用 `quality`/`auto_eligible` 把关。**

- 5 个节点（state 口径，全部 `success=0 && attempts>=20`，after 50–58.7）：
  - `红杏::🇸🇬 新加坡01`：30 次，58.7
  - `2::US-104.164.49.38:7693`：43 次，56.4
  - `3::US-23.26.71.145:5628`：44 次，55.2
  - `2::US-23.26.71.145:5628`：43 次，53.8
  - `3::DE-64.137.10.153:5803`：49 次，53.3
- 它们都在 **观察档（50–69）**，不是“良/可用”（≥70）；`auto_eligible` 需 `score>=70 && EffN>=20`，这 5 个都是 false，自动流量不会选。
- 计划 Task 1 没有要求零质量封顶；报告 §8 也明确留给 T5。加 `cap 49` 会改变评分契约（0.6q+0.2probe+0.2lat 的下限设计），属于产品规则变更，不应由 T1 私自加。
- 若担心 UI 把 `usable=true` 误解成“可用”，应在 T4/T5 把 `usable` 语义或展示文案与 `auto_eligible` 区分；不要在 T1 改分数。

### (e) before/after 快照相隔 55 分钟，是否影响结论？

**裁决：影响“精确 delta/恢复解锁时点”，不影响指定节点的阈值结论，但报告不能把 after 当同一时刻 A/B。**

- 指定日本02/香港01：17:38 到 18:33 之间探测/延迟只带来 ±1–2 分噪声，两者都远低于 50；独立重放也分别 41.0/41.1，结论稳。
- 新加坡/日本原生/台湾01：after 表依赖“17:38–18:33 之间探测成功 + 手工清零”，精确 +6.3/+10.9 不是纯代码效果；但“1 小时内回到 70+”的规则由代码和 m2/m4 变异证明，方向成立。
- 普通高分 AU/JP 与实现差 0.1/0.0，不受 55 分钟影响。
- 结论：**阈值验收可过；after 表全量 delta、恢复时点、174 个下降等结论需要 T4 同刻重放后才能引用。**

---

## 6. 不能仅从 diff 验证的事项

1. **10-20 之前的完整事件级证据**：只有 6 天 call_log 副本；state 是生命周期累计，无法精确还原旧节点的 `W_blip`。本报告对旧节点给的是保守下界/区间，不是精确 after。
2. **部署后的运行时行为**：`ProbeFailTimes` 在下一次真实探测成功后才清空；无法在 diff 里证明生产 probe 调度一定在 1 小时内触发（只能证明代码路径）。
3. **跨记录 403 的生产影响**：10-24 实际是单订阅，跨记录池级路径未被生产数据触发；本次用合成测试证明行为缺陷，未能在真实日志中复现多订阅风暴。
4. **全仓全绿**：临时副本全量测试因缺少 `free_models_cache.json`、IP4 既有残留、网络探测失败而不绿；不能宣称“全仓无回归”，只能确认 T1 定向测试和静态检查。
5. **当前 HEAD/提交边界**：复核期间 HEAD 从 `dee27cf` 前进到 `3ab39e9`；`852fc9c` 已含 `node_scoring.go`/报告，测试文件按 `.gitignore` 本地保留。若 Lead 要按某个 commit 验收，需要重新核对该 commit 的文件内容；本报告按工作区字节判定。
6. **`pick(false)` 的运行时语义**：T1 diff 未触碰，但工作区 `proxy_node.go` 有 Task2 改动；若把整个工作区一起验收，需要在 T2 的 diff 里再验证粘滞。

---

## 7. 建议的处置顺序

1. **修 F1（必须）**：让池级 403 在 10 分钟窗口内可回溯，或延迟提交；把 `TestPoolLevel403NotBlamed` 改成两条独立记录并断言两节点 `EffN=0`、`Pool403=1`、reasons 含 `pool_403`。重跑全部 T1 测试 + 变异 + 自己的跨记录测试。
2. **重做 §5 的 after 表**：按事件时间戳算 `W_blip`，不要用固定 0.7762；旧证据不可得时标注区间。至少 BR-45/CY 这类节点不能继续展示错误的 57.2/72.5。
3. **T4 同刻 A/B**：同一 state、同一 `now`，不要用 17:38 API 分数直接对 18:33 state。
4. **T2 继续处理 403 订阅级根因**：T1 的“单订阅算节点错”规则不改，但报告要明确这是订阅/出口级故障，不是节点独立质量。

在 F1 修复并重跑前，**T1 不能进入 T4 验收**；T4 可以先做独立全量回放/工具准备，但不能把 T1 标记为通过。

---

## 8. 复核产物

- `/tmp/t1v/recompute.py`：独立回放/评分
- `/tmp/t1v/bias.py`：旧 blip 权重偏差下界
- `/tmp/t1v/mutate_t109.py`、`/tmp/t1v/mutations.log`：5 个变异及红证据
- `/tmp/t1v/pkg/zz_review_t109_test.go`：429/跨记录 403/字段契约独立测试
- `/tmp/t1v/pkg`：临时包副本（仓库 `.go` 未被修改）
- `/tmp/t1v/fulltest.log`：全量测试原始输出

---

## 复审（fix round 1）

> 本节结论取代上文旧的“T1 不能进入 T4”；上文保留为首轮记录。

- 复审范围：`/tmp/review-t1-fix.diff`（HEAD `3ab39e9` → 工作区，`node_scoring.go` + `06-task1-report.md`，852 行）及本地未入库的 `node_scoring_test.go`。
- 复核对象指纹：`node_scoring.go` sha256 `e418ecd7b2c2b26c077bfbca415a8bd0f14f30be023b5462648c1cf094a41868`；测试文件 sha256 `8360e74dc7c7a0f6400ca50b0656a8824634a6caec16b266d3e9e7352911b50f`（工作区当前内容）。
- 独立验证全部在 `/tmp/t1v/pkg` 临时副本完成，未改仓库源码、未 commit。

### F1 [Critical] 跨记录回溯

**ADDRESSED（主路径已修复；新增两个边界缺口，见“新问题”）**

证据（自己写的测试，不是只读实现者测试）：

- `TestReview403WindowAcrossRecords`：红杏 403 在第 1 条记录、飞鸟云 403 在第 2 条记录（间隔 1min）→ 两节点 `Attempts=0`、`EffN=0`、`Fail.Cred=0`、`Pool403=1`；相隔 11min 的对照组两节点仍 `Attempts=1 / EffN=1 / cred=1 / Pool403=0`。**PASS**。
- `TestFixReviewBlipRollback`：把当前 10min 桶先打成 blip，再让红杏 403（应只加 `0.2w`），随后飞鸟云 403 触发回溯 → 红杏/飞鸟云都 `Attempts=0`、`EffN=0`、`BlipFails=0`、`cred=0`、`Pool403=1`，账本清空。**PASS**，证明 `c.Weight` 已按 blip 的 0.2 权重回退。
- `TestFixReviewHistoricalBlipRollback`：`historical=true` 回填的 blip 403 同样回退到 `Attempts=0 / EffN=0 / BlipFails=0 / cred=0`。**PASS**。
- `TestPool403WindowExpiryNoRetroactive`（实现者测试）通过：11min 后不回溯；`TestPool403ActiveWindowExtends` 通过：生效窗口内后续 403 不计质量；`TestPool403SuccessDoesNotExtend` 通过：普通成功记录不会续期。
- `TestFixReviewLedgerCleanupAndSuccessNoExtend`：11min 后账本清空；普通成功不设置 active；旧窗口外的新 403 不回溯。**PASS**。
- 高压检查 `TestFixReviewLedgerBounded`：同窗 500 条 403 后账本 `len=500`，11min 后 `len=1`（只保留新记录）→ 窗口清理确实删除条目，没有无界增长。**PASS**。

因此原 F1 的“先到 403 也被回溯”及“>10min 不回溯”在当前进程内成立；`pool403ActiveUntil` 不会被普通成功记录续期。

### F2 [Important] blip 口径

**ADDRESSED（报告数值；严格“保守下界”的证明仍不完整）**

- 独立复算 3 个节点（不调用实现者 replay/Go 代码）：使用自己的 `W_blip = W_window + max(0, state.blip_fails - window_blip) × 0.513`，得到：
  - `1::BR-45.38.67.97:7029`：`W=230.104`，`EffS=96.87`，`EffN=140.53`，**75.9**（报告 75.9）
  - `1::CY-45.131.92.24:6635`：`W=1847.515`，`EffS=1183.69`，`EffN=1585.84`，**81.8**（报告 81.8）
  - `飞鸟云::hy2台湾06`：`W=29.414`，`EffS=11.144`，`EffN=33.924`，**47.0**（报告 47.0）
  - 三个值全部精确复现；BR-45 从旧版 57.2 修正为 75.9。
- `0.5^(6.73/7)=0.51355`，所以如果所有窗口外证据确实至少 6.73 天老，0.513 是旧证据权重的上界，报告方法成立。
- **但**这一点无法从只读副本直接证明：副本最早记录是 `2026-10-20T23:15:34`，距 state 保存时刻只有 **5.804 天**，对应权重 `0.563`。如果任何窗口外 blip 落在 5.804–6.73 天之间（state 的 lifetime 计数可能来自 10-20 00:00 之后、但不在副本里的记录），真实权重会 >0.513，报告表就不是保守下界。用 0.563 重算同样三节点为 **BR-45=73.2、CY=80.2、飞鸟云=45.4**；阈值方向不变，但 `75.9` 不是严格下界。
- 结论：**报告数值的修正已生效（BR-45=75.9 属实）**；严格“保守下界”需要补一句假设“所有旧证据早于 10-20 00:00”，或改用可证明的 0.563。若采用 0.563，报告必须把 BR-45 等值改为 73.2 一类，不得继续称 75.9 为下界。

### F3 [Minor] 非零样本对账

**ADDRESSED**

- 独立手算（Python，不调用 Go 实现）：
  - 第一组 `Wilson(68,110)=0.540011`，`lat(3000)=0.841176`，`raw=69.2247` ⇒ **69.2**；
  - 第二组 `Wilson(38,50)=0.648963`，10min 探测惩罚 `0.4+0.6×600/7200=0.45`，`lat(777)=1.0`，`raw=67.9378`；最近失败 <30min ⇒ **65.0**。
- `TestScoringFormulaParityNonZeroSample` 的两个期望值正确，且测试是硬编码独立计算值、不是调用 `scoreNode` 回读；不是“只断言常量”的空测试。测试只覆盖两组，但足以覆盖 F3 的非零样本缺口。

### 修复 diff 引入的新问题

1. **[Important] `contribs403` / `pool403ActiveUntil` 未持久化，重启会丢失跨记录窗口**
   - 证据：`scoringPersistFile` 只保存 `Nodes`/`LastSeen`（`node_scoring.go:1248-1253`），不保存 `contribs403`/`sub403`/`pool403ActiveUntil`。模拟重启：第一条红杏 403 保存 → 新 store `load` → 1min 后飞鸟云 403，结果红杏 `att=1 effn=1.000 cred=1 pool=0`、飞鸟云 `att=1 effn=1.000 cred=1 pool=0`，**两节点都被计错**。
   - 影响：进程在 10min 窗口内重启/滚动发布时，F1 的跨记录回溯失效；`sub403` 原本也是内存态，所以这是“新增账本没有补持久化”的缺口，不是回归，但会让 F1 的保证只在进程生命周期内成立。
   - 建议：把 `sub403`/`contribs403`/`pool403ActiveUntil` 持久化并随 state 加载；或在启动回填时把 `lastSeen-10min` 的 403 记录重放一遍。若明确接受“仅进程内保证”，在报告和 T4 中写清。

2. **[Important/语义] `pool403ActiveUntil` 会把“只剩单订阅”的后续 403 无限期豁免**
   - 证据：`node_scoring.go:919-925` 只要 `record403Subs` 非空且 `ts < pool403ActiveUntil` 就设为 pool，随后每个 403 记录都把 active 续到 `ts+10min`；`TestPool403ActiveWindowExtends` 正是断言 t0+12 只有青云单订阅 403 仍 `EffN=0 / Pool403=2`。
   - 影响：一次多订阅 403 触发后，只要某个单订阅每 <10min 继续 403，该订阅的所有 403 都不会算节点错，和 Global Constraints“单订阅 403 必须算节点错”在当前窗口的直观语义冲突。它不是原 F1 回归，但新增的 active 规则需要产品明确：这是“同一场风暴的滑动窗口”还是“单订阅持续故障”。
   - 建议：仅允许触发过 pool 的参与订阅续期，或给 active 设总时长上限；补一个“红杏/飞鸟云出窗后，只有青云持续 403”的测试并按产品语义断言。

3. **[Minor] 回溯把 `Attempts--` 后，`sample_size` 变成“质量尝试数”而不是原始尝试数**
   - 证据：`revertPool403Locked` 在 `node_scoring.go:813-814` 递减 `Attempts`；`scoreNode` 的 `SampleSize` 直接取 `Attempts`（`node_scoring.go:464`），`LowConfidence` 取 `EffN<20`（`:465`）。两节点各一次 pool 403 后 `sample_size=0`、`low_confidence=true`（新的 `TestPoolLevel403NotBlamed` 明确断言 `Attempts=0`）。
   - 影响：低置信判断不受影响（本来就按 EffN），但 API 的 `sample_size` 会显示 0 次尝试，和实际发生过 2 次请求不一致；若有 UI/选择器依赖原始尝试数，需要区分。
   - 建议：在字段说明里明确它是“计入质量的尝试数”，或保留原始 `Attempts` 另设 `QualityAttempts`；当前实现至少要在报告里说明这一语义变化。

4. **[Minor] 0.513 的“保守下界”依赖未写明的旧证据年龄假设**
   - 证据见 F2：副本最早记录年龄 5.804 天（w=0.563），报告却称窗口外年龄≥6.73 天；使用 0.563 时 BR-45=73.2，不是 75.9。该问题只影响报告 §5 的下界解释，不影响代码权重。
   - 建议：报告补上“旧 state 证据均早于 10-20 00:00”的证据/假设，或改用 0.563 并把表值改低；不要把 75.9 无条件称为严格下界。

### 新增测试质量检查

- 4 个 403 相关新测试都实际构造记录并观察 `EffN/Attempts/Fail.Cred/BlipFails/Pool403`，没有只断言常量的空测试。
- `TestScoringFormulaParityNonZeroSample` 使用独立硬编码期望；不是空测试，但仅两组样本，覆盖有限。
- 全量 T1 定向测试通过；`go vet .` exit 0；`gofmt -l node_scoring.go node_scoring_test.go` 无输出。临时副本全量 `go test -skip 'TestIP4RV'` 仍有既有 `TestT23RealLogHardGate` / `TestT23RealLogBackfillEqualsDirect` 失败（缺 `free_models_cache.json`，非 T1），未发现 T1 定向回归。

### 复审结论与 T4

- **F1：ADDRESSED（主路径）**；**F2：ADDRESSED（数值）/ NOT fully ADDRESSED（严格下界证明）**；**F3：ADDRESSED**。
- 修复 diff 的新问题：`contribs403`/active 未持久化（Important）、active 对单订阅的无限续期（Important/需产品确认）、`sample_size` 语义（Minor）、0.513 假设（Minor）。
- **T1 可以进入 T4 的独立全量复算**：原 Critical 的跨记录主路径已修复，F3 已补；但 **不能把报告 §5 的 75.9 当作严格下界**，T4 必须用自己的事件级重放，并覆盖重启与 active 窗口边界。若产品要求“单订阅 403 必须实时归节点”或“重启后窗口仍有效”，应先修上述两个 Important 新问题，再签 T1 通过。
