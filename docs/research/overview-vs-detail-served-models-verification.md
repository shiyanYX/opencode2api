# 「概览只显示有成功的模型」口径 —— 独立数据侧验收（task-41 Part A）

- 验收人：`capture-429`（独立于实现者 `verify-403`）
- 时间：2026-10-20 09:0x（生产日志快照 `2026-10-20T08:52:44+08:00`）
- 对应实现任务：task-39（`everServed` 口径），实现者报告 `docs/research/overview-vs-detail-served-models.md`（本报告刻意独立，不复用其实测）
- 本报告范围：**Part A —— 纯数据侧口径验收**。Part B（代码/测试/面板/重启/变异验证）按 Lead 指令等放行，本报告 §12 只列待验清单，不给结论。

---

## 0. 结论摘要（先给判据）

用户裁决（2026-10-20 08:30，逐字）：

> 「概览的图表啥的只显示有成功的模型，更详细的统计才记录全部请求」
>
> 且已确认：**概览成功率大数字也不含垃圾请求**。

目标公式（task-39）：

```
mapped = everServed(resolveModel(raw)) || 公式B
everServed = 历史上任何一条 status=ok 记录里出现过的模型名
```

三个口径在**生产全量日志**（233,045 行）上的实测：

| 口径 | 概览排除 | 占比 | 排除集内 `status=ok` | 概览 total_requests | 概览成功率 | 相对现状 |
|---|---|---|---|---|---|---|
| 现状（线上，无任何判定） | 0 | 0 | — | 233,045 | **89.269025%** | — |
| ① 公式 B（task-21，已上线代码的判据） | 1,391 | 0.5969% | **0** ✅ | 231,654 | **89.805054%** | +0.536029 pp |
| ② everServed 单用 | 1,406 | 0.6033% | **0** ✅ | 231,639 | **89.810869%** | +0.541844 pp |
| ③ **并集 B ∪ everServed（task-39 目标）** | 1,391 | 0.5969% | **0** ✅ | 231,654 | **89.805054%** | +0.536029 pp |

**三条结论，按重要性排序：**

1. **硬闸门通过**：三个口径的排除集内 `status=ok` 记录数 **都是 0**（逐月复核 08/09/10 也全为 0）。没有任何成功记录被逐出概览。
2. **并集在今天的数据上是公式 B 的数值等价物 —— 一条不多、一条不少**（排除集完全相同，1,391 条）。也就是说 task-39 的代码在当前生产数据上**不会改变任何一个数字**。它的价值不在今天，而在漂移鲁棒性：实测把 `big-pickle` 从文档免费表里摘掉，公式 B 立刻会把 **11 条成功记录**判成未映射（硬闸门破防，成功率虚高 +0.299 pp），而并集靠 everServed 全部保住（硬闸门 0）。§11 R3 有完整实验。
3. **口径修正在短窗口上是用户肉眼可见的量级，全量上不是。** 用户看到的「大数字」是短窗口（面板 `today` 窗口 = 按整点滚动的近 24 小时）：**73.7127% → 97.8417%（+24.129 pp）**；10-20 当天 **26.329% → 94.545%（+68.216 pp）**；`7d` 日历日窗口 72.2751% → 74.1772%（+1.902 pp）。全量（`180d`）只 +0.536 pp。这说明用户裁决的触发场景就是 10-19/10-20 那 728 条垃圾请求（§9）。**但必须同时说清：10-17（62.5%）、10-18（40.6%）的低成功率是 `space-bunny-free` 真实上游故障（500/403/502），口径修正救不了，修正后依旧低**——不能拿这个改动去暗示「服务变好了」。

**发现一处与用户字面口径不符的地方（需 Lead 裁决，F1）**：并集口径下概览**仍会显示 6 个 0 成功模型**（15 条请求），因为公式 B 的那一支会保留「名字带 `-free` / 在免费表里」但一次都没成功过的模型。字面读「只显示有成功的模型」应当是 `everServed` 单用（②），它把这 6 个模型全滤掉。差异 15 条 / 0.0065%，对成功率影响 0.0058 pp。§10 有完整对比和建议。

---

## 1. 判据与验收项

本报告要回答的验收项（来自 task-41 任务书）：

| # | 验收项 | 结论 | 位置 |
|---|---|---|---|
| A1 | 三口径排除数/占比 | ✅ | §4 |
| A2 | 硬闸门：排除集内 ok = 0 | ✅ 三口径皆 0 | §5 |
| A3 | 逐模型明细表（n / ok / `-free` / 在文档表） | ✅ | §6 |
| A4 | 概览 total_requests + 成功率 | ✅ | §4、§9 |
| A5 | 与 task-23 的 661 条排除集逐项对账 | ✅ 完全一致（model × 月 × 计数） | §7 |
| A6 | `deepseek-v4-flash-free`(4,437 ok) / `x-preview-f-free`(1,265 ok) 必须被保留 | ✅ 两口径都保留 | §8.1 |
| A7 | 有成功的裸名（`big-pickle` 等）不得误判 | ✅ `big-pickle` 11/11 保留 | §8.2 |
| A8 | 从未成功的名字（`mimo-v2.5` 624 / `claude-opus-4-5` 4 / `hy3` 2 / `gpt-4o-mini` 1）不得出现在概览 | ✅ 三口径皆排除 | §8.3 |

---

## 2. 数据与版本（可复现锚点）

### 2.1 生产日志

```console
$ docker ps --format '{{.Names}}\t{{.Status}}'
opencode2api	Up 14 hours (healthy)
$ docker inspect opencode2api --format 'Image={{.Image}} StartedAt={{.State.StartedAt}}'
Image=sha256:0b3f8b7cc082fbbeca76e10e41e1698ffbfdc72fd8f38c0247bf2fe57b15a884
StartedAt=2026-10-19T10:44:04.213753911Z
$ docker exec opencode2api sh -c 'ls -la /data/call_log.jsonl; wc -l /data/call_log.jsonl'
-rw-r--r--  1 opencode opencode 124380053 Oct 20 09:00 /data/call_log.jsonl
233111 /data/call_log.jsonl
```

我用的快照（`docker exec cat` 导出，导出后生产继续写入，故比上面 233,111 少 66 行）：

| 文件 | 行数 | 字节 | sha256 | 时间范围 |
|---|---|---|---|---|
| `/tmp/t41/live.jsonl` | 233,045 | 124,352,036 | `c8783886b558283ef0d9883d6bbaa326e78af60dd5c39cb9b4d61f97a7cc8996` | 2026-08-13T17:40:27.744Z .. 2026-10-20T08:52:44.718+08:00 |
| `/tmp/prod-now.jsonl`（task-23 同一份基线，含 08-13..10-13 窗口） | 204,237 | 114,821,xxx | `a4be0497b356d5b11506bcfe085b579d9a7684043444e322626dacb750694348` | 2026-08-13 .. 2026-10-13 |

**线上口径现状证据**：日志里 **0 行**带 `unmapped` 键（`grep -c '"unmapped"' /tmp/t41/live.jsonl` → `0`），且容器启动于 2026-10-19T10:44Z（早于 task-39 实现），所以「现状」行是真实线上数字，不是推演。

```console
$ grep -c '"unmapped"' /tmp/t41/live.jsonl
0
```

### 2.2 目录 / 文档 / 配置快照

| 来源 | 内容 | 用途 |
|---|---|---|
| `/tmp/t41/catalogs.json` `zen` 85 条 + `go` 37 条（去重 99） | 2026-10-20 上游 `/v1/models` 快照 | `resolveModel` 的 `modelExistsInCaches` 输入 |
| `/tmp/t41/catalogs.json` `docs_free` **14 条** | 2026-10-20 文档免费表快照 | `isFreeModelIn` 输入 |
| 仓库 `free_models_cache.json` **13 条**（sha256 `84a8489f…`，内容与 HEAD 一致，`git diff` 空） | 冷启动/文档不可用时的持久化兜底 | 文档表「旧版」敏感性对照 |
| `config.json` = `{"model_alias": null, "reasoning_effort_map": null, "force_disable_thinking": false, "quota_error_signals": {}}` | 生产配置 | 无别名、无改名、无禁用 → `resolveModel` 只剩「TrimSpace → 文档表 → `+'-free'` 目录反查」三条路径 |

文档免费表 14 条：`beam-free, big-pickle, deepseek-v4.1-flash-free, exo-free, glm-5.3-flash-free, jev-1.13-free, ll-alpha-free, longcat-2.5-preview-free, mimo-v2.5-free, mimo-v2.6-flash-free, minimax-m3.1-flash-free, nemotron-3-ultra-free, nemotron-3.5-lightning-free, nemotron-3.5-super-vl-free`。

> 仓库 13 条与今日文档 14 条不同（仓库缺 `beam-free/glm-5.3-flash-free/minimax-m3.1-flash-free/nemotron-3.5-super-vl-free/deepseek-v4.1-flash-free`，多 `step-5-preview-free/ling-3.0-flash-fin-free/ling-3.1-flash-free/muse-spark-1.3-contributor-free/space-bunny-free`）。**实测两版对两个数据集的排除集与成功率结果完全一致**（差异模型全部带 `-free` 后缀，被后缀规则兜住），所以本报告的结论不依赖文档快照的版本——这一点很重要，它意味着 Part B 不必纠结「部署时文档表是哪一版」。

### 2.3 代码版本 pin（注意：Part A 期间实现者正在写，pin 已移动）

```console
$ sha256sum main.go calllog.go statscan.go static/admin.html proxy_node.go node_alert.go
7104c2ea3ba86384a7563645fcddd78261070fcc80efc4cd27e51cebef3e7b63  main.go
e611782da4481f218dcc671b3774c4bc0fcf21e0e63d1aab54e56c5e44c0a188  calllog.go
47a066beabad1bea8653a6946f24880e118d892ecc63ff28de324b1b694201cb  statscan.go
fa122d8091c57a004ba7da29e16353c7bd86bacaa7d343395eb5b63f97553800  static/admin.html
4fdd19352f0e31d2cdc980cddd82a5dde37212b8829e53ee2443484f93048d65  proxy_node.go
b0783b4ae8b8292a34ccf0dde662588f533b64d306fe19215d009379b2da7e30  node_alert.go
```

- 我接手 task-41 时 `main.go` = `6f5120de…`（= task-23 时的纯公式 B），`calllog.go` = `8b74b69f…`；Part A 分析期间实现者把 task-39 写进了 `main.go`（`7104c2ea…`）与 `calllog.go`（`e611782d…`）。**Part B 开始前必须重新 pin**，上面的值只说明 Part A 读的是哪一版。
- Part A 的口径计算**不依赖实现代码**（按任务书公式独立用 Python 复刻，并用 task-23 的 Go 结果做对拍，见 §3.2），所以 pin 移动不影响本报告结论。
- 我只读了代码用于确认「实现意图与我的口径是否同一个语义」（§3.3），**没有改任何一行**。

---

## 3. 方法

### 3.1 三口径的精确定义

按 `main.go:isUnmappedByFormula`（公式 B）与 `main.go:isUnmappedModel`（并集）逐字复刻；生产 `model_alias=null` 使 `resolveModel` 退化为：

```python
def resolve(m):                       # resolveModel
    m = m.strip()
    if m and m not in docs and (m + '-free') in CATALOG:   # modelExistsInCaches
        return m + '-free'
    return m

def B(m):                             # 公式 B
    r = resolve(m)
    return (r in docs) or r.endswith('-free')              # isFreeModelIn || HasSuffix
```

三口径：

- ① **公式 B**：`mapped_B(m) = B(m)`
- ② **everServed 单用**：`E = { r.model for r in log if r.status == 'ok' }`；`mapped_E(m) = resolve(m) in E`
- ③ **并集（目标）**：`mapped_U(m) = B(m) or resolve(m) in E`

概览数字：`total_requests = 总数 - 排除数`，`成功率 = ok总数 / total_requests`（分子不变，因为三个口径的排除集内 ok 都是 0）。

### 3.2 复刻正确性验证（对拍 task-23）

在 task-23 用过的同一份 `/tmp/prod-now.jsonl` 上跑我的 Python 公式 B，与 task-23 报告的 Go 结果**逐项一致**：

| 指标 | task-23 报告（Go） | 我的 Python 复刻 | 一致 |
|---|---|---|---|
| 排除条数 | 661 | **661** | ✅ |
| 排除集内 ok | 0 | **0** | ✅ |
| 概览 total | 203,576 | **203,576** | ✅ |
| 成功率 | 91.947970% | **91.947970%** | ✅ |
| Δ | +0.297584 pp | **+0.297584 pp** | ✅ |
| 排除模型构成 | mimo-v2.5 624 / x-preview-f 30 / claude-opus-4-5 4 / hy3 2 / gpt-4o-mini 1 | **同左，逐一相同** | ✅ |
| 排除集月份分布 | 08:34 / 09:5 / 10:622 | **08:34 / 09:5 / 10:622** | ✅ |

`model × 月 × 计数` 全等，说明不是「总数碰巧相同」，而是**逐条同一批记录**（§7.1 有原始输出）。

### 3.3 我的口径 vs 实现意图是否同一语义

读 `calllog.go` / `main.go` 确认（只读）：

- `isServedModel` 的键是 **`rec.Model` 即客户端原始名**（`calllog.go:1812 finish()` 里 `if rec.Status == "ok" { markServedModel(rec.Model) }`），回填 `stagedServed[r.Model]` 同样按原始名 → **等于我的 `E_raw`**。
- `isUnmappedModel(raw)` 先 `isServedModel(m)` 再 `isUnmappedByFormula(m)` → **等于我的并集口径 ③**（`isServedModel` 用的是 `m` 原始名，不是 `resolve` 结果）。
- 生产无别名，所以 `resolve(raw) ≠ raw` 当且仅当 `raw+'-free' ∈ 目录`，此时 resolved 必带 `-free` 后缀 ⇒ 公式 B 恒为真。**因此「排除集内 ok=0」在本配置下可证**（§5.2），不只是实测。

---

## 4. 三口径对照（生产全量 233,045 行）

```console
$ python3 analyze.py
===== live（docs14 + 上游目录 99 条）: total=233045 ok=208037 models=30 =====
  B          排除   1391 (0.5969%) ok=  0 | 概览 total=231654 rate=89.805054% (base 89.269025%, Δ+0.536029pp) | 保留模型 22 | 保留但0成功 ['beam-free','jev-1.13-free','muse-spark-1.2-contributor-free','muse-spark-1.3-contributor-free','nemotron-3.5-super-vl-free','ox-alpha-free']
  E_raw      排除   1406 (0.6033%) ok=  0 | 概览 total=231639 rate=89.810869% (base 89.269025%, Δ+0.541844pp) | 保留模型 16 | 保留但0成功 []
  union_raw  排除   1391 (0.5969%) ok=  0 | 概览 total=231654 rate=89.805054% (base 89.269025%, Δ+0.536029pp) | 保留模型 22 | 保留但0成功 ['beam-free','jev-1.13-free','muse-spark-1.2-contributor-free','muse-spark-1.3-contributor-free','nemotron-3.5-super-vl-free','ox-alpha-free']
```

（`E_res`/`union_res` 用「resolve 后的名字」建集合，结果与 `E_raw`/`union_raw` **完全相同**：`E_raw-E_res = ∅`，`E_res-E_raw = ∅`。原因是这 16 个有成功的名字 resolve 后都等于自己。所以「raw 名 vs resolve 名」在当前数据上不产生差异，两种定义都安全——但设计上要注意这个命名空间的坑，见 §11 R4。）

读法：

- **并集 = 公式 B**（1,391 完全相同）。everServed 今天一条都没多排除、也没多保留。
- **everServed 单用 = 1,406**，多排除 15 条，正好是 6 个 0 成功模型（§10）。
- 概览模型数：现状 30 → 并集 22 → everServed 单用 16。

---

## 5. 硬闸门

### 5.1 实测

| 口径 | 排除条数 | 其中 `status=ok` |
|---|---|---|
| 公式 B | 1,391 | **0** |
| everServed 单用 | 1,406 | **0** |
| 并集 | 1,391 | **0** |

逐月（并集）：

```console
逐月 B/union 排除集 ok 校验:
  2026-08: 排除   34, 其中 ok 0
  2026-09: 排除    5, 其中 ok 0
  2026-10: 排除 1352, 其中 ok 0
```

对 prod-now 同样为 0（task-23 已确认，我复算一致）。

### 5.2 「everServed 那一支的闸门是同义反复」——必须说清楚

everServed 集合就是「有 ok 的原始名」的集合，所以**思路上它 protecting 自己**：任何 ok 记录，其原始名必然在 E 里。严格一点：设记录 `raw` 为 ok，则 `isServedModel(raw) = true`，并集直接 mapped。**只有一种情况可能漏**：`resolve(raw) ≠ raw` 且 E 用 resolve 名建集合（`E_res` 变体）。生产无别名，`resolve(raw) ≠ raw ⇒ raw+'-free' ∈ 目录 ⇒ resolved 带 -free ⇒ 公式 B 为真 ⇒ 仍 mapped`。故：

> **在 `model_alias`/改名/禁用全空的生产配置下，并集口径「不删成功记录」是可证的恒真命题，不构成独立风险防线。**

它的真实价值只剩两处，都在漂移场景：
1. 公式 B 的**文档表那一支**会因上游目录变化把裸名（不带 `-free`）的历史成功追认为未映射 —— everServed 兜住（§11 R3 实测 11 条）。
2. 冷启动 / 文档表抓取失败时（`isUnmappedByFormula` 里 `docsReady=false` 返回 false，已 mapped），公式 B 本来就保守放行；everServed 在此场景没有必要性。

**建议 Lead/实现者在报告里不要把「硬闸门」写成并集的功劳**，它是公式 B 的功劳 + 并集的结构同义反复。写错会让后续维护者误以为删掉 B 也不会少算成功（实际上 everServed 单用同样不删成功，但会删掉 15 条 0 成功请求与本文 §10 相关）。

---

## 6. 逐模型明细（live，30 个模型全列）

列说明：`docs` = 解析后名字是否在今日文档免费表；`cat` = 解析后名字是否在上游 99 条目录；`B/E/U` = 三口径是否判 mapped；首末日 = 该名字在日志中出现的首末日期（本地日期）。

```console
model                                     n    ok resolved                              docs  cat  B  E union  首日    末日
mimo-v2.5-free                       182460 166518 mimo-v2.5-free                        True False  T  T  True  2026-08-14 2026-10-07
space-bunny-free                      37993  31154 space-bunny-free                     False False  T  T  True  2026-09-24 2026-10-19
deepseek-v4-flash-free                 4566   4437 deepseek-v4-flash-free               False False  T  T  True  2026-08-13 2026-10-13
mimo-v2.6-flash-free                   3441   3064 mimo-v2.6-flash-free                  True True  T  T  True  2026-10-09 2026-10-20
x-preview-f-free                       1419   1265 x-preview-f-free                     False False  T  T  True  2026-08-21 2026-08-26
deepseek-v4.1-flash-free               1337   1311 deepseek-v4.1-flash-free              True True  T  T  True  2026-10-15 2026-10-20
space-bunny                             728      0 space-bunny                          False False  F  F False  2026-10-19 2026-10-20
mimo-v2.5                               624      0 mimo-v2.5                            False True  F  F False  2026-08-21 2026-10-09
step-5-preview-free                     346    221 step-5-preview-free                  False False  T  T  True  2026-10-14 2026-10-15
nemotron-3-ultra-free                    34     32 nemotron-3-ultra-free                 True False  T  T  True  2026-09-03 2026-09-04
x-preview-f                              30      0 x-preview-f                          False False  F  F False  2026-08-27 2026-09-19
glm-5.3-flash-free                       14      7 glm-5.3-flash-free                    True True  T  T  True  2026-10-17 2026-10-20
big-pickle                               11     11 big-pickle                            True True  T  T  True  2026-09-17 2026-10-20
nemotron-3.5-super-vl-free                6      0 nemotron-3.5-super-vl-free            True True  T  F  True  2026-10-17 2026-10-17
nemotron-3.5-lightning-free               6      5 nemotron-3.5-lightning-free           True True  T  T  True  2026-10-20 2026-10-20
minimax-m3.1-flash-free                   4      4 minimax-m3.1-flash-free               True True  T  T  True  2026-10-20 2026-10-20
longcat-2.5-preview-free                  4      4 longcat-2.5-preview-free              True True  T  T  True  2026-10-20 2026-10-20
claude-opus-4-5                           4      0 claude-opus-4-5                      False True  F  F False  2026-08-22 2026-08-22
ox-alpha-free                             3      0 ox-alpha-free                        False False  T  F  True  2026-08-21 2026-08-21
muse-spark-1.2-contributor-free            2      0 muse-spark-1.2-contributor-free      False False  T  F  True  2026-08-30 2026-08-31
ling-3.0-flash-fin-free                    2      2 ling-3.0-flash-fin-free              False False  T  T  True  2026-09-17 2026-09-18
hy3                                        2      0 hy3                                  False True  F  F False  2026-08-21 2026-09-03
beam-free                                  2      0 beam-free                            True True  T  F  True  2026-10-17 2026-10-20
probeL-143230                              1      0 probeL-143230                        False False  F  F False  2026-10-18 2026-10-18
probe-143148                               1      0 probe-143148                         False False  F  F False  2026-10-18 2026-10-18
muse-spark-1.3-contributor-free            1      0 muse-spark-1.3-contributor-free      False False  T  F  True  2026-09-17 2026-09-17
laguna-s-2.1-free                          1      1 laguna-s-2.1-free                    False False  T  T  True  2026-08-22 2026-08-22
jev-1.13-free                              1      0 jev-1.13-free                        True True  T  F  True  2026-10-20 2026-10-20
hy3-free                                   1      1 hy3-free                             False False  T  T  True  2026-08-22 2026-08-22
gpt-4o-mini                                1      0 gpt-4o-mini                          False False  F  F False  2026-09-03 2026-09-03
```

排除集（8 个名字，1,391 条）：`space-bunny` 728、`mimo-v2.5` 624、`x-preview-f` 30、`claude-opus-4-5` 4、`hy3` 2、`probe-143148` 1、`probeL-143230` 1、`gpt-4o-mini` 1。

---

## 7. 与 task-23「661 条排除集」逐项对账

### 7.1 prod-now 上：完全一致（同一批记录）

```console
=== prod-now B 排除集 model x 月 ===
  claude-opus-4-5      2026-08    4
  gpt-4o-mini          2026-09    1
  hy3                  2026-08    1
  hy3                  2026-09    1
  mimo-v2.5            2026-08    2
  mimo-v2.5            2026-10  622
  x-preview-f          2026-08   27
  x-preview-f          2026-09    3
  月合计: {'2026-08': 34, '2026-09': 5, '2026-10': 622} 总计 661
  (task-23 记录: 08:34 09:5 10:622 合计661)
  排除集内 ok 数: 0
```

`model × 月 × 计数` 全等 ⇒ **没有一条记录被不同口径换成另一条记录**。

### 7.2 live 上：661 → 1,391，差异逐条解释

```console
=== live B 排除集 model x 月 ===
  claude-opus-4-5      2026-08    4
  gpt-4o-mini          2026-09    1
  hy3                  2026-08    1
  hy3                  2026-09    1
  mimo-v2.5            2026-08    2
  mimo-v2.5            2026-10  622
  probe-143148         2026-10    1
  probeL-143230        2026-10    1
  space-bunny          2026-10  728
  x-preview-f          2026-08   27
  x-preview-f          2026-09    3
  合计 1391 ;差异 vs task-23(663) = 728
```

| 差异项 | 条数 | 解释 |
|---|---|---|
| task-23 当时的 live 排除 = 663 | 663 | 661（上表前 5 个模型，与 prod-now 同一批）+ 2 条 `probe-143148`/`probeL-143230`（10-18，我方合成探针，`upstream http 400`，0 ok） |
| **新增：`space-bunny`（裸显示名）** | **+728** | 全部落在 **10-19（443 条）与 10-20（285 条）**，全部 `fail`（`upstream http 400` 675 条、`502` 53 条），**0 成功** → 1,391 = 663 + 728，**逐条闭合，无剩余未解释记录** |

`space-bunny` 裸名为什么现在才出现、且被公式 B 判未映射：上游 `/v1/models`（含 zen 85 + go 37）今日**已不含 `space-bunny-free`**，所以 `resolve('space-bunny')` 的反查（`space-bunny-free` 是否在目录）失败，退回 `space-bunny` 本身，既不在文档免费表也无 `-free` 后缀 ⇒ 未映射。这批是客户端拿着面板展示名（面板会把 `-free` 脱掉）直接调用的结果，本身就是不可服务的垃圾请求。**三口径都排除它，符合用户口径**（它一次都没成功）。

---

## 8. 特殊检查（Lead 点名项）

### 8.1 漂移过的 `-free` 模型必须保留 ✅

| 名字 | n | ok | 成功率 | 在文档表 | 在目录 | 靠什么保留 |
|---|---|---|---|---|---|---|
| `deepseek-v4-flash-free` | 4,566 | **4,437** | 97.17% | ❌ | ❌ | `-free` 后缀（公式 B）；everServed 也命中 |
| `x-preview-f-free` | 1,419 | **1,265** | 89.15% | ❌ | ❌ | 同上 |
| `space-bunny-free`（同类漂移，且 7 日内大面积失败） | 37,993 | **31,154** | 82.00% | ❌ | ❌ | 同上 |
| `step-5-preview-free` | 346 | 221 | 63.87% | ❌ | ❌ | 同上 |
| `laguna-s-2.1-free` / `hy3-free` / `ling-3.0-flash-fin-free` | 1 / 1 / 2 | 1 / 1 / 2 | 100% | ❌ | ❌ | 同上 |

Lead 给的基线数对得上：4,437 与 1,265 均为精确匹配。**如果只按上游目录判（被否决的 A 方案），这 5,702 条成功会被删**——公式 B 的后缀规则正是为此存在，task-23 已论证，本次复算再次确认。

### 8.2 有成功的裸名不得误判 ✅

- `big-pickle`：n=11 / ok=**11**（100%），文档表 ✅、目录 ✅。**保两次**：公式 B 走文档表命中，everServed 走原始名命中。§11 R3 的漂移实验证明「文档表摘掉后公式 B 会误删这 11 条，everServed 是唯一防线」。
- `space-bunny`（裸名）：n=728 / ok=**0** → 三口径都判 unmapped。**这不是误判**：它的成功记录全部挂在 `space-bunny-free` 名下（31,154 条），裸名本身从未成功。按用户字面口径（「有成功的模型」）隐藏它是对的；按「同一模型」的语义直觉它可能该显示——见 §11 R2，这是口径的语义选择，不是 bug。

### 8.3 从未成功的名字必须从概览消失 ✅

| 名字 | 条数 | 公式 B | everServed | 并集 | 备注 |
|---|---|---|---|---|---|
| `mimo-v2.5` | 624 | 排除 | 排除 | 排除 | 上游付费名；`mimo-v2.5-free` 自 10-07 起 410 |
| `claude-opus-4-5` | 4 | 排除 | 排除 | 排除 | 上游付费名 |
| `hy3` | 2 | 排除 | 排除 | 排除 | 上游付费名 |
| `gpt-4o-mini` | 1 | 排除 | 排除 | 排除 | 非本项目模型 |

补充边界：日志中 **0 条** 空/空白模型名、0 条首尾带空白（`TrimSpace` 场景不存在）、大小写变体只有 `probeL-143230` 1 条（我方探针）——这些边界都不会影响判定；且乱序重跑 3 次结果恒为 1,391（纯函数、与行序无关）。

---

## 9. 窗口敏感度：用户看到的「大数字」在哪里变

### 9.1 各时间窗口

| 窗口 | 总请求 | ok | 现状成功率 | 并集排除 | 修正后 total | **修正后成功率** | Δ | everServed 单用成功率 |
|---|---|---|---|---|---|---|---|---|
| `180d`（=全量，04-24..10-20 日历日） | 233,045 | 208,037 | 89.2690% | 1,391 | 231,654 | **89.8051%** | +0.5360 pp | 89.8109% |
| 近 30 天（`30d`=09-21..10-20 日历日） | 175,198 | 159,441 | 91.0062% | 1,352 | 173,846 | **91.7139%** | +0.7078 pp | 91.718679% |
| 近 7 天（`7d`=10-14..10-20 日历日） | 28,469 | 20,576 | 72.2751% | 730 | 27,739 | **74.1772%** | +1.9020 pp | 74.201226% |
| **`today`（整点滚动近 24h）** | **2,952** | **2,176** | **73.7127%** | **728** | **2,224** | **97.8417%** | **+24.1290 pp** | 97.929793% |

> ⚠️ 窗口定义修正（2026-10-20 09:0x）：本表按面板**真实**窗口语义重算——`7d/30d/180d` 由 `trendBuckets` 按**本地日历日**对齐（含今天共 n 天），`today` 是「23 小时前对齐整点 + 24 个整点桶」的滚动 24 小时，**不是**滑动 24h×n。初稿用的是滑动小时窗（24h: 74.6766%→97.6734%、+22.9969 pp；7d: +1.8813 pp；30d: +0.6730 pp），口径更正后的面板真实数字见上表。逐日表（§9.2）不受影响。

**这就是用户裁决的真实动因**：全量只差 0.54 pp，但用户盯着的是「最近」那张图，而 10-19/10-20 的 728 条垃圾请求几乎全部落在 `today` 窗口内 → 24 小时成功率被压到 73.71%，修正后 97.84%。

### 9.2 逐日（本地日期）

| 日期 | total | ok | 现状% | 排除 | 排除内 ok | 修正后 total | 修正后% | Δ pp |
|---|---|---|---|---|---|---|---|---|
| 2026-10-13 | 368 | 306 | 83.152 | 0 | 0 | 368 | 83.152 | 0.000 |
| 2026-10-14 | 1,304 | 1,169 | 89.647 | 0 | 0 | 1,304 | 89.647 | 0.000 |
| 2026-10-15 | 4,400 | 4,022 | 91.409 | 0 | 0 | 4,400 | 91.409 | 0.000 |
| 2026-10-16 | 6,531 | 6,410 | 98.147 | 0 | 0 | 6,531 | 98.147 | 0.000 |
| 2026-10-17 | 4,687 | 2,930 | 62.513 | 0 | 0 | 4,687 | 62.513 | 0.000 |
| 2026-10-18 | 7,636 | 3,103 | 40.636 | 2 | 0 | 7,634 | 40.647 | +0.011 |
| **2026-10-19** | 3,516 | 2,838 | 80.717 | 443 | 0 | 3,073 | **92.353** | **+11.636** |
| **2026-10-20** | 395 | 104 | 26.329 | 285 | 0 | 110 | **94.545** | **+68.216** |

**必须同时报告的另一面（不许粉饰）**：10-17 的 62.5% 与 10-18 的 40.6% **不是垃圾请求**，是 `space-bunny-free` 的真实上游故障——

```console
2026-10-17: total=4687 fail=1757 失败模型=[('space-bunny-free',1625),('mimo-v2.6-flash-free',108),('deepseek-v4.1-flash-free',11),('glm-5.3-flash-free',6)]
  失败 err: [('upstream http 403',1158),('upstream http 502',576),('upstream http 400',13),('upstream http 500',10)]
2026-10-18: total=7636 fail=4533 失败模型=[('space-bunny-free',4396),('mimo-v2.6-flash-free',131),('deepseek-v4.1-flash-free',4),('probe-143148',1)]
  失败 err: [('upstream http 500',3037),('upstream http 403',1213),('upstream http 502',278),('upstream http 400',5)]
```

近 7 天保留请求 28,107 条里 7,225 条失败（25.71%），其中 `space-bunny-free` 独占 6,716 条（上游 500/403/502）。**口径修正后近 7 天仍是 74.28%**——修改口径不是「服务变好」，这两件事必须在对外话术里切开。

---

## 10. F1（需 Lead 裁决）：并集口径仍不满足用户「只显示有成功的模型」的字面要求

用户原话是「概览的图表啥的**只显示有成功的模型**」。并集口径下概览保留 22 个模型，其中：

| 保留但 0 成功的模型 | 条数 | 为什么被保留 |
|---|---|---|
| `nemotron-3.5-super-vl-free` | 6 | 在文档免费表 / 带 `-free` |
| `ox-alpha-free` | 3 | 带 `-free`（已从文档表与目录消失） |
| `beam-free` | 2 | 在文档免费表 |
| `muse-spark-1.2-contributor-free` | 2 | 带 `-free`（已消失） |
| `muse-spark-1.3-contributor-free` | 1 | 带 `-free`（已消失） |
| `jev-1.13-free` | 1 | 在文档免费表 |
| **合计** | **15** | 占全部请求 **0.0064%** |

对照 `everServed` 单用（②）：保留 16 个模型，**0 个 0 成功模型**；概览 total 231,639、成功率 89.810869%（比并集高 0.0058 pp，因为多排掉 15 条失败）。

也就是说：

- **并集 = 「删垃圾名 + 保留一切像免费模型的」**，它满足「垃圾请求不进概览」，但**不满足**「概览只显示有成功的模型」——图表上仍会出现 6 个一次都没成功过的模型。
- **everServed 单用 = 用户字面口径的精确实现**，且不删任何成功记录（同样 0 ok 在排除集，§5.1 实测）。

**建议（供 Lead 决策，我不擅自改口径）：**

1. **若按字面执行**：`mapped = everServed(resolve(raw))` 单用（②）。风险：一个刚上线、还没成功过的合法免费模型，会在图表里完全不出现（用户看到「没这个模型」），而且它一成功，历史失败会**追认**回概览（历史数字向下跳，见 R1）。影响面 15 条 / 6 个名字。
2. **若按并集执行**（当前 task-39 实现）：概览会出现 6 个「0 成功」行。若用户在意，可在**前端图表层**加一条 `ok>0` 的渲染过滤（不动分母），这样字面满足、分母口径不变；代价是图表模型数（16）与分母口径（22）不完全对应——但差 15 条请求，量级可忽略。**我倾向这个折中**，因为它同时满足两条用户原话，且不需要改判定内核。
3. 若两者都不动：必须在交付说明里明写「口径 = 垃圾请求不进概览，不等于只显示成功模型」，否则下次用户看到 0 成功行会认为没改。

---

## 11. 漂移与残留风险（Part B 必须盯的地方）

### R1（中）：everServed 只增不减 ⇒ 历史数字可能**向下**跳，且分类在「名字粒度」上是脆的

- 单向性：一旦某个**从未成功**的名字成功一次，它的**全部历史失败**会在下次回填后从「排除」翻成「保留」→ 对应窗口成功率**下降**（允许方向，但用户能看到数字变小）。
- 量化最坏情形：`space-bunny` 728 条（10-19/20）一旦成功一次，10-19 会从 92.35% 掉回 80.72%，24 小时从 97.67% 掉回 74.68%（即这次修正**整体回退**）。它现在被上游 400 硬拒，概率低，但这是「修正后数字并非永久稳定」的实证边界。
- 反向不成立：已经被冻结成 `unmapped:true` 的历史行**不会**因为文档表/目录刷新而重新判定（回填只处理「没有 unmapped 键」的老行，`calllog.go:806-820`）。所以若上游日后**重新上架** `space-bunny-free`，`resolve('space-bunny')` 会变成 `space-bunny-free`（后缀 ⇒ 公式 B 真），新行 mapped、老行仍 frozen unmapped → **同一个模型名在图表里历史被劈成两段**。这是设计选择（宁可历史稳定），但 Part B 的报告必须写清，不能宣称「跨重启/目录刷新完全一致」。

### R2（低，语义待确认）：`space-bunny` 这类「展示名」请求被整族隐藏

面板展示时会把 `-free` 脱掉，用户若照展示名调用就会产生裸名请求（本次 728 条，全部 400 失败）。这些名字一次没成功 ⇒ 被隐藏（符合字面）。但注意：**它和 `space-bunny-free` 在图表里是两个模型**，`space-bunny-free` 的 31,154 条成功不受影响。语义上用户可能以为是「同一个模型」，需要一句说明。

### R3（高价值，正面）：everServed 确实堵住公式 B 的漂移退化 —— 实测

把 `big-pickle` 从文档免费表摘掉（模拟上游/文档下架该免费模型；目录仍在），同一份 live 日志：

| 口径 | 排除 | 排除内 ok | 概览 total | 成功率 | 结论 |
|---|---|---|---|---|---|
| 公式 B（无 everServed） | 1,402 | **11** ❌ | 231,643 | 89.809319% | **硬闸门破防**：11 条成功被删，成功率虚高 |
| everServed 单用 | 1,406 | 0 ✅ | 231,639 | 89.810869% | 保住 |
| **并集（目标）** | **1,391** | **0** ✅ | 231,654 | 89.805054% | **保住** |

prod-now 上同场景：公式 B 排除 665 条、其中 **4 条 ok**（08-13..10-13 窗口内 `big-pickle` 有 4 次成功，与 task-23 记录一致），并集仍 661 / 0 ok。

⇒ **task-39 这次改动的真实收益 = 这条**：让「曾经成功过但已从目录/文档漂移掉」的模型不被追认删除。今天数据上这个收益恰好为 0（因为 `big-pickle` 还在文档表里），属于**为未来漂移买的保险**，报告不要写成「现在修好了某个错」。

### R4（低）：`everServed` 按**原始名**记，与公式 B 的 `resolve` 名是两个命名空间

- 现状安全（16 个有成功的名字 resolve 后都等于自己，`E_raw == E_res`）。
- 但若将来配置 `model_alias`（生产现为 null），别名请求成功时记的是**别名**，而公式 B 判的是**解析后名**。由于 `isUnmappedModel` 先查 `isServedModel(raw)`，ok 记录本身仍恒 safe（同义反复，§5.2），但「别名指向的真实模型下的其它原始名」不会被 everServed 保护。建议实现者顺手把 resolve 后的名字也 `markServedModel` 一次（一行），或在注释里写清「按原始名记是刻意的」。

### R5（低，数据质量）：日志里混着两种时间戳时区格式

- `232,555` 条是 `+08:00` 本地时间，**`490` 条是 `Z`（UTC）**，且这 490 条是文件**最开头连续的一段**（行 0..489，`2026-08-13T17:40:27Z .. 2026-08-14T04:34:47Z`，模型 `deepseek-v4-flash-free` 489 / `mimo-v2.5-free` 1，ok 468 / fail 22）。
- 影响：按本地日切，「08-13」这 102 条其实属于本地 08-14；按 UTC 日切则 08-14 那 388 条要挪走。占全量 0.21%。**对本次口径结论零影响**（这 490 条全部是 mapped 模型），但任何「按日/按小时」的图表在这两天会有归属偏差，属于既有日志写入的历史不一致，记录在此供后续排查。

---

## 12. Part B 待验清单（本报告不下结论，等 Lead 放行）

Lead 已列的项我不重复展开，仅补充 Part A 暴露出的、非验不可的点：

1. **行序无关**：日志 38.6% 乱序，回填用 `stagedServed` 一次性裁决。必须用**打乱行序的同一份日志**跑两遍，断言 unmapped 逐行一致（我可以提供 `/tmp/t41/live_shuffled.jsonl`）。
2. **重启后立刻失败请求**：`servedModels` 冷启动为空、回填未完成时，若此刻来一条「历史上成功过、当前失败」的请求，`finish()` 会不会把它冻结成 `unmapped:true`（之后回填不重判带键行）→ 该行永久错分。这是 §R1 反向的 runtime 版，必须实测（重启 → 立即打一个 `big-pickle` 失败请求 → 查 `unmapped` 字段与概览）。
3. **`space-bunny` 场景回归**：日志里已有 728 条裸名垃圾请求，验证图表/分母确实不含它们，但 `/api/call-log`、`/data/call_log.jsonl`、`/api/logs` 一条不少。
4. **无 range 的 `/api/stats` 字节级不变**：`unmapped` 字段与 `everServed` 不得进无 range 响应体（Lead 已列，实测要抓原始 body 对比 sha256，不是看字段）。
5. **面板零改动**：`static/admin.html` 现在 `fa122d80…`（被 theme 任务改过），Part B 必须证明 task-39 没碰它，且页面 0 个新请求、无 `unmapped` 字段依赖。
6. **变异 ≥3**：`everServed` 恒 false / 恒 true / 只写不回填 —— 每条都要让实现者测试变红；我另加一条 **「E 按 resolve 名建集合」** 的变异（当前数据上不变红是**预期**，正好证明 §5.2 的同义反复，别当成测试缺陷）。
7. **措辞**：报告/注释一律「口径更正」，禁止「成功率提升/服务改善」。若实现者文档里出现「提升」，按 finding 处理——尤其 §9.2 的 10-17/10-18 真实故障会被误读成「改完就好了」。
8. **F1 裁决落地**：Lead 选 ①/②/③ 哪条，Part B 的验收断言随之不同（尤其「概览模型数」是 16 还是 22）。

---

## 13. 复现

```bash
# 1) 取生产日志快照（只读；导出后生产会继续写，行数可能更大）
docker exec opencode2api cat /data/call_log.jsonl > /tmp/t41/live.jsonl
sha256sum /tmp/t41/live.jsonl    # 本报告：c8783886…（233,045 行）
grep -c '"unmapped"' /tmp/t41/live.jsonl   # 本报告：0（线上尚未部署新口径）

# 2) 三口径计算（脚本见附录，或 /tmp/t41/analyze.py）
python3 /tmp/t41/analyze.py
```

对拍基线（task-23 的 Go 结果）用 `/tmp/prod-now.jsonl`（sha256 `a4be0497…`，204,237 行），复算应得 661 / 0 ok / 203,576 / 91.947970%。

附录：`analyze.py` 核心（完整版见 `/tmp/t41/analyze.py`，与本报告同源；文档免费表与目录快照 `/tmp/t41/catalogs.json`）

```python
CATALOG = set(zen_85) | set(go_37)          # 上游 /v1/models 全量
DOCS14  = set(docs_free_14)                 # 文档免费表

def resolve(m):                              # resolveModel（生产无别名/改名/禁用）
    m = (m or '').strip()
    if m and m not in DOCS14 and (m + '-free') in CATALOG:
        return m + '-free'
    return m

def B(m):                                    # 公式 B
    r = resolve(m)
    return (r in DOCS14) or r.endswith('-free')

E = {r['model'] for r in rows if r['status'] == 'ok'}      # everServed（原始名）
mapped = {
    'B':      lambda m: B(m),
    'E_raw':  lambda m: resolve(m) in E,
    'union':  lambda m: B(m) or resolve(m) in E,
}
# 排除集 = [m for m in per_model if not mapped[k](m)]
# 概览 = total - Σn(排除)，成功率 = ok_total / 概览 total
```

---

## 14. 一句话交接

**Part A：数据侧口径成立** —— 三口径硬闸门全 0，并集与公式 B 在当前生产数据上数值等价（1,391 条排除、89.805054%），与 task-23 的 661 条逐项对账闭合（live 差异 = 2 条探针 + 728 条 10-19/10-20 新增裸名垃圾请求）；口径修正的真实可见收益在**近 24 小时（+22.997 pp）与 10-20 当天（+68.216 pp）**，全量仅 +0.536 pp，且 10-17/10-18 的低成功率是真实上游故障、修正救不了；唯一需要 Lead 裁决的是 **F1**（并集仍显示 6 个 0 成功模型，字面口径应用 everServed 单用）。**Part B 未开始，等放行。**
