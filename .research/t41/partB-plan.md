# Part B 计划与验收口径（task-41，Lead 2026-10-20 裁定后冻结）

> 状态：**准备完成，等 Lead 的「Part B 开始」信号**（`$WORK/START` 出现后 `run_partB.sh` 才开跑）。
> 本文件只写计划、口径、已完成的干跑证据；**不含任何实现结论**。实跑证据在 `$WORK/results/`。

---

## 1. F1 裁决：选项 2（并集做分母 + 展示层 `ok>0` 过滤）

Lead 裁定（2026-10-20）：后端 `/api/stats` 聚合出口剔除 `models` 与 `trends_by_model` 里**窗口内 `OK==0`** 的键；
分母（`total_requests` / 成功率）保持并集口径不变——真实免费模型的失败照算，只是不出现在图表里。

否掉另外两条的理由（已记录，避免以后回滚）：
- ① everServed 单用 = 把 6 个真实免费模型的 15 条失败从分母删掉 = **藏失败**，不是口径更正；
- ③ 只写说明不实现 = 用户明确要求，能低成本做到就不留偏差。

### 1.1 收窄后的 `/api/stats` 验收口径（我据此写断言）

| # | 断言 | 判据 |
|---|---|---|
| A | `/api/stats`（**无 range**） | **逐字节不变**（sha256 相等）。绝对，无例外；filter 只许加在 ranged 出口 |
| B | `/api/stats?range=*` | 只允许「窗口 `OK==0` 的模型」从 `models` 消失、其整条序列从 `trends_by_model` 消失；**其余字段逐字面相等**（保留模型的每个数值、`total_requests`、`throughput_tps`、`stream_tps_n`、`unmapped_requests`） |
| C | 被删请求仍在分母 | `total_requests` 不变；`Σ models.request_count == total_requests - Σ被删.request_count`；被删请求的 prompt/completion/cache 与 `stream_req_count` 全为 0（token 影响必须为 0） |
| D | `/api/stats/trends?group=model` | **与 B 同一判据**（Lead 2026-10-20 裁决 Q1）：只许「窗口 `OK==0` 的模型」的序列消失，其余点逐字面相等；且被删集合必须与**同窗口** `/api/stats` 的被删集合一致（同一判据必须同一出口语义） |
| E | `/api/call-log`、`/data/call_log.jsonl`、`/api/logs` | 详细层一条不少（条数、req_id、status 不变；仅 `unmapped` 键为 task-21 已批新增） |

### 1.2 各窗口「被删模型」精确清单（Part A 独立算出的期望值，token 影响已逐个核过）

| 窗口 | 被删模型（条数） | 合计请求 | token 影响 |
|---|---|---|---|
| `today`（整点滚动 24h） | `beam-free` 1、`jev-1.13-free` 1 | **2** | 0 |
| `7d`（10-14..10-20） | `beam-free` 2、`jev-1.13-free` 1、`nemotron-3.5-super-vl-free` 6 | **9** | 0 |
| `30d`（09-21..10-20） | 上面 3 个 + `deepseek-v4-flash-free` 1 | **10** | 0 |
| `180d`（=全量） | 上面 4 个 + `muse-spark-1.2-contributor-free` 2、`muse-spark-1.3-contributor-free` 1、`ox-alpha-free` 3 | **15** | 0 |

```console
model                                n     pt     ct   cc   cr stream
beam-free                            2      0      0    0    0      0
jev-1.13-free                        1      0      0    0    0      0
muse-spark-1.2-contributor-free      2      0      0    0    0      0
muse-spark-1.3-contributor-free      1      0      0    0    0      0
nemotron-3.5-super-vl-free           6      0      0    0    0      0
ox-alpha-free                        3      0      0    0    0      0
合计                                 15      0      0    0    0      0
```

⇒ 被删 15 条**全部是失败请求**（0 token、0 流式），所以 `models` 少掉的只是「0 成功」的行，
**`total_requests` 与成功率完全不变**，与 Lead 的口径一致。

---

## 2. 两个基线，必须分开用（干跑时发现的坑）

干跑发现：`git HEAD`（`2854784`）**不含 task-21 公式 B**——

```console
$ for pat in isUnmappedByFormula everServed unmappedBuckets unmapped_requests; do ...
isUnmappedByFormula      0        # HEAD 里没有
everServed               0
unmappedBuckets          0
unmapped_requests        0
$ cd repo && grep -c "unmappedBuckets|isUnmappedByFormula|unmapped_requests" main.go calllog.go
main.go:6   calllog.go:19        # 工作树里有（task-21 + task-39，未提交）
```

所以 HEAD 构建 ≈ **线上现状口径**（日志里 0 行 `unmapped` 也印证部署版本更早）。于是：

- **`--mode isolation`（F1 主验收）**：基线 = 冻结代码 **只把 F1 过滤中和掉** 的构建（变异 M5）。单一变量，
  `total_requests` 必须不变，只许删键。
- **`--mode e2e`（端到端汇总）**：基线 = HEAD。变化 = task-21 未映射口径 + task-39 everServed + F1 过滤的合集，
  `total_requests` 恰好减少被删模型的请求数，允许新增 `unmapped_requests` 字段（task-21 已批）。
  用于给 Lead 一份「从线上口径到目标口径」的完整账，**不与 F1 的收窄口径混为一谈**。

> 观察（2026-10-20 09:3x，仅记录不做结论）：实现者已写 `modelsWithOKInWindow`（`calllog.go:1194`，
> 判据读 `hourBuckets[k].OK`，与我的 oracle 等价），目前只在 `/api/stats` ranged 出口接线（`main.go:7439`）；
> `/api/stats/trends?group=model` 尚未接线——正是 Q1/M8 断言要抓的点。

---

## 3. 变异性（变异清单已固化）

| ID | 变异 | 预期 | find 状态 |
|---|---|---|---|
| M1 | everServed 短路恒假（`isServedModel` 恒 false + 回填快照只留 staged） | 红 | ✅ 当前工作树唯一命中 |
| M2 | everServed 恒真（`isUnmappedModel` 所有非空名直接 mapped） | 红 | ✅ |
| M3 | 只写不读历史（回填 `ever := map[string]struct{}{}`） | 红 | ✅ |
| M4 | 删 `-free` 后缀分支（5,702 条成功会被删出概览） | 红 | ✅ |
| M5 | F1 过滤恒真（= 不过滤） | 红 | ⏳ 等实现落地补 find（也兼任 isolation 基线构建） |
| M6 | F1 过滤恒假（删光所有模型） | 红 | ⏳ |
| M7 | F1 半实现（只过滤 `models`、漏 `trends_by_model`） | 红 | ⏳ |
| M8 | Q1 半实现（`/api/stats` 已过滤，但 `/api/stats/trends?group=model` 出口未过滤） | 红 | ⏳ |

共 8 条变异 + 1 次基线 `go test`。

驱动规则：`find` 命中数 ≠ 1 → 报 **MUTATION_DRIFT** 并计入失败，**不静默跳过**；基线（未变异）必须绿，
否则结论作废；red 判据 = 出现基线没有的 `--- FAIL: TestX`（避免被仓库里既有的失败测试污染）。

---

## 4. 实验设计与干跑证据（已完成部分）

| 实验 | 设计 | 干跑结果 |
|---|---|---|
| 夹具 | 从生产快照生成 4 个 case：`orig` / `shuffled`（同多重集、seed 固定）/ `nopickle`（文档缓存去掉 big-pickle，使 everServed 成为唯一防线）/ `docs14` | 233,045 行、`multi_set_equal=true` |
| 服务黑盒化 | 夹具目录当 cwd，`HTTP_PROXY=127.0.0.1:9` 让文档/目录抓取确定性失败 → 判据只用本地缓存（等价于 Part A 的 docs13，结论与 docs14 相同） | `old` 构建 5.9s 编译通过 |
| `/api/stats` 比较器 | oracle = 基线 `trends_by_model` 每模型窗口 Σok==0；`--selftest` 用合成过滤验证比较器本身 | ✅「合成过滤→PASS；未过滤→被检出；e2e 合成→PASS」 |
| 负对照① | `old` vs `old`（都未过滤）跑 isolation | ✅ 被判失败，逐窗口列出应删键 |
| 负对照② | `old`（线上口径）跑重启时序测试 | ✅ 4 类断言全红（`unmapped` 字段缺失、`space-bunny` 出现在概览）——证明测试能抓 |
| 负对照③（Q1） | `old` vs `old` 跑 isolation，检查按模型趋势端点 | ✅ 报 `trends_7d_model` 15→9、`trends_180d_model` 30→16 未过滤 |
| 负对照④ | `old` 构建跑行序测试（含绝对数值与 Q1 跨端点一致性） | ✅ 全红：缺 `unmapped_requests`、total/models 不符、趋势端点含从未成功模型 |
| 面板双 pin | md5 + sha256 双向自检（正确 pin→通过；篡改 md5→失败） | ✅ `374ba326…` / `6364641c…` 均按预期 |
| 面板/措辞 | 面板不依赖 `unmapped/everServed`；措辞扫描排除否定语境 | 原始命中 7 行、排除否定语境后 0 行 ✅ |

---

## 5. 四个问题已由 Lead 裁决（2026-10-20 09:3x）

| # | 裁决 | 落到断言 |
|---|---|---|
| Q1 | **要一起过滤**。`/api/stats/trends?group=model` 是面板概览图表数据源（`admin.html:1172`），同一判据必须同一出口语义，否则两个视图模型数不一致 | D 条升格为 B 类；新增 `trends_7d_model` / `trends_180d_model` 两个断言 + 跨端点被删集合一致性；已做 RED 负对照 |
| Q2 | 面板以 `total_requests` 为准，**不许**做「分项之和 == 总计」校验；占比/饼图分母用 `total_requests` | 我只断言 `total_requests` 与成功率不变（C 条），不管面板画法 |
| Q3 | 无 range 路径不动；面板不读它（只有 `DELETE /api/stats` 清空在用），所以强证据零代价 | A 条保留为绝对断言（sha256 不变） |
| Q4 | 更正：`6364641c…` 是 **sha256**，md5 是 `374ba326…`（同一文件、2,472 行）；theme 任务（`verify-panel-f7`）已停手冻结在 md5=`374ba326` | 改用**双 pin（md5+sha256）**判面板零改动；Part B 期间若有变化 → 不是 theme 干的 → 记 finding（已通过正/反两向自检） |

放行时 Lead 会给一张完整冻结 pin 表；我的 `pin_list` 已扩到**仓库根全部 `*.go`（80 个，含测试）+ 面板 + 免费模型缓存**（82 行），
任何代码/测试文件在 Part B 期间被改都会被抓到（比只钉 6 个文件更严）。

---

## 6. 运行方式与预算（信号到达后一条命令）

```bash
touch /tmp/t41b/START            # Lead 发「Part B 开始」后
/tmp/t41b/run_partB.sh           # stage 0 冻结 pin → 构建 → 夹具 → 8 个阶段 → 汇总
# 产物：/tmp/t41b/results/{summary.md,api-diff-isolation.json,order-test.json,
#                          restart-test.json,mutations.json,panel-wording.txt,
#                          pin-freeze.txt,pin-end.txt,run.log}
```

| 阶段 | 内容 | 预算 |
|---|---|---|
| 0 | pin 冻结（6 文件 sha256）+ 冻结副本 + 构建 `old`(HEAD)/`new`(冻结树) | ~15 s |
| 1 | 夹具 3~4 份（各 124 MB） | ~40 s |
| 2 | `/api/stats` 差异 + 按模型趋势一致性（isolation 主验收） | ~4 s |
| 3 | 行序无关（原序 vs 打乱，逐字节；含绝对数值交叉核对 1,391 / 231,654 / 16） | ~5 s |
| 4 | 重启时序（3 次 cold + 1 次 warm，每次 3 个探针请求） | 3~5 min |
| 5 | 变异 M1–M8（基线 + 8 次 `go test -count=1 ./`） | 25~45 min |
| 6 | `go test -count=1 ./` + `-race`（冻结副本） | 5~10 min |
| 7 | 面板/措辞 | <1 s |
| 8 | pin 复核（变了 → 本轮 INVALID） | <1 s |

脚本：`lib.sh`（环境/构建/起停服务）、`mkfixture.py`、`api_diff.py`、`order_test.py`、
`restart_test.py`、`mutate.py` + `mutations.json`、`panel_wording_check.sh`、`run_partB.sh`。

---

## 7. 附：Part A 报告的两处随后更正（引用请注意）

1. **§9.1 窗口口径**：面板 `trendBuckets` 的 `7d/30d/180d` 是**本地日历日**对齐（含今天共 n 天），
   `today` 是「23 小时前对齐整点 + 24 个桶」的滚动 24h，不是滑动 24h×n。已按面板真实口径重算：
   `today` 73.7127%→97.8417%（**+24.1290 pp**，原稿滑动口径写 +22.9969 pp）、`7d` +1.9020 pp、`30d` +0.7078 pp。
   逐日表不受影响。
2. **§9.1 的 everServed 单用列**已换成实测值（97.929793% / 74.201226% / 91.718679% / 89.810869%），此前是估值。
