# 未映射模型统计口径修复 —— 独立验收报告（task-23）

**验收人**：capture-429（独立于实现者）
**被测对象**：`/home/jingyx/project/opencode2api-test`（分支 `test`，HEAD `ba26d70`）**未提交工作区改动**
（`main.go` +193/−33、`calllog.go` +299、`statscan.go` +28/32，共 491 插入 / 33 删除）
**实现者报告**：`docs/research/stats-unmapped-model-fix.md`（573 行，**仅作为待验证输入，全部数字我自己重算**）
**生产**：容器 `opencode2api` 跑 `main@22cfe2d`（**不含**本次改动），本报告中一切"生产实测"均为**只读**采样。

---

## 0. 结论

**ACCEPT（通过）** —— 硬门禁（排除集内 `status=ok` 记录数 = **0**）在**两个数据集 × 4 组目录快照**下**全部成立**；实现者报告的核心数字可逐位复现；判定逻辑、统计面、追溯路径、纯函数性、字段形状、变异测试、全量回归均通过。

**附条件（不阻塞合并，但需记录/跟进）**：5 条发现，见 §12。其中**唯一需要监控的线上条件**：文档免费表一旦丢掉 `big-pickle`，老行重算会误杀 7 条真实成功记录（新行不受影响，见 R1 触发条件）。

---

## 1. 数据与方法（可复现）

| 数据 | 规模 | md5 / 说明 |
| --- | --- | --- |
| `/tmp/prod-now.jsonl` | 204,237 行 / 109,153,292 B | `e7567ff5e8e70c40412c8d722790f680`，2026-08-13T17:40Z ~ 2026-10-13T09:15+08:00 |
| 生产日志只读副本 `/tmp/t23/live.jsonl` | **231,992 行 / 123,763,401 B** | `66b82fd49da0afef9ff7d9f1ef5cd44f`，~2026-10-19T18:52:11+08:00（`docker exec opencode2api cat` 取得，未动容器） |
| `docs13` = 仓库 `free_models_cache.json` | 13 条 / 403 B | `11300b016ed08ded2ae81502b7a2b196`（生产持久化的免费表快照） |
| `docs14` = 今天文档页解析 | 14 条 | 抓 `https://opencode.ai/v2/docs/console/models/`（**与代码同一个 URL、同一套正则**，我用 Python 独立复刻） |
| 上游 zen 目录 | 85 条 | `https://opencode.ai/zen/v1/models` |
| 上游 go 目录 | 37 条 | `https://opencode.ai/zen/go/v1/models` |

**两处交叉验证**（消除"我抓的目录不是生产那份"的怀疑）：
- 我用 Python 独立解析文档页得 **14** 条免费模型；本地起测试分支二进制后，服务端日志打印 `free models loaded count=14` —— **一致**。
- 我 curl 上游 zen 得 **85** 条；服务端日志打印 `models=85` —— **一致**。

**版本钉桩（验收时刻，`sha256sum`）**：

```
6f5120de4b682d47f2dd35a2edbccdfd381641c0022bdad279488c0349f5d2ff  main.go
54c3a378ca89c1ca4042a9fa0b6fa2c830307feed97c96b392aefd606eda9edb  calllog.go
47a066beabad1bea8653a6946f24880e118d892ecc63ff28de324b1b694201cb  statscan.go
4088100c5dc144f75cb9bd9afaa0a8d37730f660f4e8861141b3fcb4de3e69ce  static/admin.html
b0783b4ae8b8292a34ccf0dde662588f533b64d306fe19215d009379b2da7e30  node_alert.go
```
（HEAD `ba26d70`；验收结束时五个文件 sha256 **逐一未变** —— `sha256sum -c` 全通过。注意 `static/admin.html` 在我的验收**开始之前**已被其他 teammate（面板 task-22 线）改过（会话早期记录为 `bb02b423…`，我开工时为 `4088100c…`）：我记录的是开工时刻的值，验收全程未变，我全程未碰该文件。）

**判定一律调用函数本体**（`isUnmappedModel(raw)`，`main.go:1493`），不是人工推断；遍历 20 万行用 `bufio` 流式读，模型名记忆化。

---

## 2. 硬门禁（Lead 的第一道门）

`mapped = isFreeModel(resolveModel(raw)) || strings.HasSuffix(resolveModel(raw), "-free")`，排除集 = 判 `unmapped` 的记录。

### 2.1 `/tmp/prod-now.jsonl`（Lead 给的 661 / +0.2976pp 口径）

| docs 快照 | 上游目录 | 排除集 n | **排除集 ok** | total_requests 前→后 | 成功率 前→后 | Δ |
| --- | --- | ---: | ---: | --- | --- | --- |
| docs13（13） | zen85+go37 | **661** | **0** | 204,237 → 203,576 | 91.650387% → 91.947970% | **+0.297584 pp** |
| docs14（14） | zen85+go37 | **661** | **0** | 204,237 → 203,576 | 91.650387% → 91.947970% | +0.297584 pp |
| 硬编码兜底（9） | zen85+go37 | **661** | **0** | 204,237 → 203,576 | 91.650387% → 91.947970% | +0.297584 pp |
| docs13（13） | **空目录** | **661** | **0** | 204,237 → 203,576 | 91.650387% → 91.947970% | +0.297584 pp |

✅ **硬门禁通过：4 组快照下排除集 661 条、ok=0，全部完全相同**（说明该口径对当前目录漂移不敏感）。

### 2.2 生产日志只读副本（部署现场的最新数据）

同 4 组快照，结果再次完全一致：

| 项 | 值 |
| --- | --- |
| 总记录 | 231,992（ok=207,738，89.545329%） |
| 排除集 | **663 条，ok=0** |
| 修复后 total_requests | 231,329 |
| 修复后成功率 | 89.801970%（**Δ = +0.256641 pp**） |
| `len(models)` | 25 → 18 |

> 663 = 661（prod-now 的 5 个模型）+ `probe-143148`(1) + `probeL-143230`(1)，两条都是 10-18 14:31/14:32 的**合成探测记录**（`upstream http 400`，0 成功），见 §12-①。

**排除集技术特征（我独立复核，与实现者 §3.3 逐项一致）**：

| 检查 | prod-now（661 条） | live（663 条） |
| --- | --- | --- |
| `status=ok` | **0** | **0** |
| `prompt_tokens` 合计 | **0** | **0** |
| `completion_tokens` 合计 | **0** | **0** |
| `output_speed > 0` | **0 条** | **0 条** |
| `err_msg` 分布 | `upstream http 401` ×660、`502` ×1 | 401 ×660、400 ×2（probe）、502 ×1 |
| 月份分布 | 08 月 34、09 月 5、10 月 622 | 08 月 34、09 月 5、10 月 624 |

⇒ 这批请求**一个 token 都没产生过**，不存在"藏成功"的可能。

---

## 3. 与实现者报告的对账（不抹平差异）

| # | 报告声称 | 我的实测 | 判定 |
| --- | --- | --- | --- |
| 1 | prod-now 204,237 行；排除 661；ok=0 | 204,237；661；0 | ✅ 一致 |
| 2 | total 204,237→203,576；91.6504%→91.9480%；+0.2976pp | 203,576；91.650387%→91.947970%；**+0.297584pp** | ✅ 一致（四舍五入后逐位相同） |
| 3 | `len(models)` 18→13 | 18→13 | ✅ 一致 |
| 4 | by_model = {mimo-v2.5:624, x-preview-f:30, claude-opus-4-5:4, hy3:2, gpt-4o-mini:1} | 完全相同 | ✅ 一致 |
| 5 | 回填 ΦN：hourBuckets ΣN=203,576 / unmappedBuckets ΣN=661，守恒 true | 203,576 / 661；`absorbed=204,237, unabsorbed=0, ΣN=204,237`，守恒 true | ✅ 一致 |
| 6 | 回填耗时 462.98 ms | **510.31 ms**（同一份 109 MB 文件，本机同时跑着全量测试） | ⚠️ 慢 10%，**性能口径差异，非正确性问题**（不写进验收结论） |
| 7 | §3.3 err_msg：401×660 + 502×1；tokens=0；output_speed>0=0 | 完全相同 | ✅ 一致 |
| 8 | §3.3 月份分布 34 / 5 / 622 | 完全相同 | ✅ 一致 |
| 9 | §3.4 `mimo-v2.5-free` 最后一次成功 `2026-10-06T14:26:41` | 实际最后成功 **`2026-10-06T14:27:43.900+08:00`**（req `49hc29me5pgl`；14:26:41.860 那条 `gocffomv4pwj` 也是成功，但不是最后一条） | ⚠️ **报告少算 1 条**；结论（410 全部落在 10-07 ⇒ 已不可服务）不受影响 |
| 10 | §3.4 1,256 条 410 全部落在 10-07 当天 | 1,256 条，日期集合 = [2026-10-07] | ✅ 一致 |
| 11 | 报告未覆盖：部署后的生产日志 | live：231,992 行、排除 663、Δ=+0.256642 pp | ➕ **补充测量**（见 §2.2、§12-①） |

---

## 4. 逐模型分类表（live，用 `isUnmappedModel` 本体判定）

| 原始 model | n | ok | ok% | 在 docs13 | 在 docs14 | `-free` 后缀 | 判定 |
| --- | ---: | ---: | ---: | :---: | :---: | :---: | --- |
| `mimo-v2.5-free` | 182,460 | 166,518 | 91.26 | ✓ | ✓ | ✓ | MAPPED |
| `space-bunny-free` | 37,835 | 31,016 | 81.98 | ✓ | ✓ | ✓ | MAPPED |
| `deepseek-v4-flash-free` | 4,566 | 4,437 | 97.17 | ✗ | ✗ | ✓ | MAPPED |
| `mimo-v2.6-flash-free` | 3,324 | 2,948 | 88.69 | ✓ | ✓ | ✓ | MAPPED |
| `x-preview-f-free` | 1,419 | 1,265 | 89.15 | ✗ | ✗ | ✓ | MAPPED |
| `deepseek-v4.1-flash-free` | 1,309 | 1,284 | 98.09 | ✗ | ✓ | ✓ | MAPPED |
| **`mimo-v2.5`** | **624** | **0** | 0.00 | ✗ | ✗ | ✗ | **UNMAPPED** |
| `step-5-preview-free` | 346 | 221 | 63.87 | ✓ | ✗ | ✓ | MAPPED |
| `nemotron-3-ultra-free` | 34 | 32 | 94.12 | ✓ | ✓ | ✓ | MAPPED |
| **`x-preview-f`** | **30** | **0** | 0.00 | ✗ | ✗ | ✗ | **UNMAPPED** |
| `glm-5.3-flash-free` | 12 | 6 | 50.00 | ✗ | ✓ | ✓ | MAPPED |
| `big-pickle` | 7 | 7 | 100.00 | ✓ | ✓ | ✗ | MAPPED（只靠文档表） |
| `nemotron-3.5-super-vl-free` | 6 | 0 | 0.00 | ✗ | ✓ | ✓ | MAPPED |
| **`claude-opus-4-5`** | **4** | **0** | 0.00 | ✗ | ✗ | ✗ | **UNMAPPED** |
| `ox-alpha-free` | 3 | 0 | 0.00 | ✗ | ✗ | ✓ | MAPPED ⚠️ |
| **`hy3`** | **2** | **0** | 0.00 | ✗ | ✗ | ✗ | **UNMAPPED** |
| `muse-spark-1.2-contributor-free` | 2 | 0 | 0.00 | ✗ | ✗ | ✓ | MAPPED ⚠️ |
| `ling-3.0-flash-fin-free` | 2 | 2 | 100.00 | ✓ | ✗ | ✓ | MAPPED |
| `hy3-free` | 1 | 1 | 100.00 | ✗ | ✗ | ✓ | MAPPED |
| `laguna-s-2.1-free` | 1 | 1 | 100.00 | ✗ | ✗ | ✓ | MAPPED |
| **`gpt-4o-mini`** | **1** | **0** | 0.00 | ✗ | ✗ | ✗ | **UNMAPPED** |
| `muse-spark-1.3-contributor-free` | 1 | 0 | 0.00 | ✓ | ✗ | ✓ | MAPPED ⚠️ |
| `beam-free` | 1 | 0 | 0.00 | ✗ | ✓ | ✓ | MAPPED |
| `probe-143148` | 1 | 0 | 0.00 | ✗ | ✗ | ✗ | UNMAPPED（合成探测） |
| `probeL-143230` | 1 | 0 | 0.00 | ✗ | ✗ | ✗ | UNMAPPED（合成探测） |

⚠️ 三个「带 `-free` 但 0 成功」的模型共 6 条**刻意不排除**（保留失败在分母，不按成败反推可服务性）——这是**正确的保守取舍**，不是漏判。

**日期漂移敏感性**：docs13 / docs14 / 兜底9 / 空目录 × 有/无上游目录 = 4 组快照，在上述 25 个模型名上**翻转数 = 0**，排除集恒为 663。⇒ 当前数据上"重算随日期漂移"**在观测范围内未发生**（R1 仍是理论风险，触发条件见 §12-R1）。

---

## 5. 判定逻辑验收（`isUnmappedModel`，23 个用例，全部 PASS）

| 场景 | 输入 | 期望 | 实测 | 结论 |
| --- | --- | --- | --- | --- |
| 文档免费表命中 | `mimo-v2.6-flash-free` | mapped | mapped | ✅ |
| 已摘掉目录的 `-free` | `deepseek-v4-flash-free` / `x-preview-f-free` | mapped | mapped | ✅ **4,437+1,265 条成功记录不被误删** |
| 显示名反查 | `mimo-v2.6-flash` / `space-bunny` | mapped | mapped | ✅ |
| 只靠文档表（无 `-free`） | `big-pickle` | mapped | mapped | ✅ 7 条成功保留 |
| 首尾空白 | `"  \t mimo-v2.6-flash-free \n "` | mapped | mapped | ✅ TrimSpace 生效 |
| 空 / 全空白模型名 | `""` / `"   "` | mapped（不算模型请求） | mapped | ✅ |
| 已知野名字 | `mimo-v2.5` / `gpt-4o-mini` | unmapped | unmapped | ✅ |
| 大写变体 | `MIMO-V2.6-FLASH-FREE` | unmapped（不做大小写归一） | unmapped | ✅ 见 §12-② |
| 大写前缀 + 小写后缀 | `MIMO-V2.5-free` | mapped（后缀命中） | mapped | ✅ 后缀判据优先于"整名可服务性" |
| `model_alias` → 免费模型 | `my-flash` → `mimo-v2.6-flash-free` | mapped | mapped | ✅ |
| `model_alias` → 付费模型 | `alias-paid` → `claude-opus-4-5` | unmapped | unmapped | ✅ |
| 用户改名（edited） | `my-flash-name` → `mimo-v2.6-flash-free` | mapped | mapped | ✅ 反向映射生效 |
| 改名 + 已禁用（hidden） | `my-flash-name` | unmapped | unmapped | ✅ 禁用模型确实不再服务 |
| 禁用模型的 `-free` 原名 | `mimo-v2.6-flash-free`（hidden） | mapped | mapped | ✅ 失败留在分母，不掩盖 |
| **安全闸门：docs=nil** | `gpt-4o-mini` / `mimo-v2.5` | **全部 mapped（维持现状）** | mapped | ✅ 冷启动不误杀 |
| 安全闸门：docs=空表 | 同上 | mapped | mapped | ✅ |
| 目录缺 `-free` 孪生 | `mimo-v2.6-flash` 且目录空 | unmapped | unmapped | ✅ 已知 R6 风险，见 §12-③ |
| 完全野名字 | `wild-model-xyz` | unmapped | unmapped | ✅ |

**每个用例都跑了两次**（同输入同输出），无一次抖动。

---

## 6. 统计面验收（"排除"与"不静默丢弃"同时成立）

### 6.1 单元级（`TestT23StatsSurfaceNoSilentDrop`、`TestT23TopNAndOtherAndWindow`）
- 3 条已映射（2 ok + 1 fail）+ 2 条未映射（fail）：`hourBuckets ΣN=3, ΣOK=2`、`unmappedBuckets ΣN=2, ΣOK=0`；
- 未映射模型**不存在于 `hourBuckets` 的任何键**里；
- `/api/stats?range=180d`：`total_requests=3`、`models` 只有 1 个键（不含未映射模型）、`unmapped_requests.count=2`；
- 落盘 5 行**全部带 `unmapped` 键**（`u1` 为 `true`、`m1` 为 `false`）——**没有静默丢弃**；
- **无 range 路径逐字节不变**：`{"total_requests":0,"models":{}}`（32 字节，钉住向后兼容红线）。

### 6.2 真实 HTTP 端到端（本地起测试分支二进制，端口 9041，`/tmp/prod-now.jsonl` 为数据）

```
free models loaded count=14    ← 与我独立抓的 docs14 一致
server starting port=9041 models=85 aliases=0   ← 与 curl 的 zen 85 条一致

GET /api/stats?range=180d  → 200, 394,114 B
  total_requests = 203,576        Σmodels = 203,576（13 个模型）
  unmapped_requests = {"count":661,
    "by_model":{"mimo-v2.5":624,"x-preview-f":30,"claude-opus-4-5":4,"hy3":2,"gpt-4o-mini":1}}
  未映射模型是否也出现在 models 里：[]        ← 口径干净

GET /api/stats（无 range） → 200, 32 B
  {"total_requests":0,"models":{}}            ← 与改动前逐字节相同

POST /v1/chat/completions {"model":"gpt-4o-mini"}  → 400 "Model is unavailable."
  服务端 WARN：calllog: request model cannot be mapped to a served model;
               excluded from stats aggregation but still written to call_log  model=gpt-4o-mini
  call_log 新行：{"req_id":"cl6ss9kuxokd","model":"gpt-4o-mini",...,"unmapped":true}
  GET /api/stats?range=today → total_requests=0, Σmodels=0, unmapped={"count":1,"by_model":{"gpt-4o-mini":1}}
```

✅ 四项一次验证：① `/api/stats` 真的输出该字段且 shape 正确；② 未映射**不进** `total_requests`/`models`/成功率分母；③ 请求**照常尝试上游**（不改客户端行为）、**照常落盘**并打 WARN；④ 无 range 路径不受影响。

### 6.3 字段形状（`{count, by_model}`，窗口内，Top10+`__other__`）
- 12 个未映射模型（条数 12..1）+ 1 条 400 天前记录：`count=78`（400 天前那条**未**计入 180d 窗口 ✅）、`by_model` 恰 11 项 = Top10 + `__other__=3`（最少的两项和）✅；
- 条数相同按模型名升序兜底 ⇒ 同一状态连查两次 `by_model` **逐字段相同**（全序稳定）✅；
- `count` 与 `total_requests` 共用同一次 `ts/bidx`（`statsRangeResponseFor` 只取一次窗口）——代码检查 + 窗口外记录实测一致。

---

## 7. 追溯路径（老记录）与纯函数性

| 检查 | 方法 | 结果 |
| --- | --- | --- |
| 老行（无 `unmapped` 键）按当前免费表重算 | 手写 JSONL：同一模型 `gpt-4o-mini` 一行带键一行不带 | **两行都判 unmapped，进同一张表** ✅ |
| 新行落盘判定权威（`SawUnmapped` 的意义） | 手写一行 `"unmapped":false` + `model:"hy3"`（当前重算会判 unmapped） | 该行**留在 mapped 表且 ok=1** —— 落盘判定未被重算覆盖 ✅ **这正是 Lead 要求覆盖的越界点** |
| 回填 == 直接判定 | 真实 204,237 行：回填后 `unmappedBuckets`/`hourBuckets` 的**分模型分布**与逐行 `isUnmappedModel` 结果 `DeepEqual` | ✅ 完全一致（510 ms） |
| 回填幂等（纯函数性） | 清空桶后重放同一文件两次 | 两张表分模型分布 `DeepEqual` ✅ |
| 判定无副作用 | 14 个输入 × 200 轮；故意把 `freeModelDocsTime` 置为 3 小时前 | 输出恒定；`freeModelDocsCache` 指针未换；`free_modelDocsTime` 未被写；`free_models_cache.json` md5 `11300b01…` 前后不变 ✅ |
| `SawUnmapped` 解析宽容度 | `statscan.go:156` 只把字面量 `true` 判 true，其余按 false | 与 `encoding/json` 脏布尔行为一致，不会因一个坏布尔丢整条统计 ✅ |

> ⚠️ 注释精度问题：`isUnmappedModel` 的注释声称"所有外部输入都来自 `(model, docs)` 两个参数 + 别名表"，但 `resolveModelWithDocs` 的 `-free` 反查走 `modelExistsInCaches()`（`modelsCache`/`goModelsCache`，会周期性刷新）。**实际不是 `(model,docs)` 的纯函数**（是"无副作用"，但非"封闭"）。真实数据上未观测到翻转（§4），结论不变，但注释应更正 —— 见 §12-④。

---

## 8. 反向风险（正常请求不能被误伤）

| 场景 | 模型 | 结果 |
| --- | --- | --- |
| 生产当前在跑的模型（live 最近 24h 只有这三个） | `space-bunny-free`、`mimo-v2.6-flash-free`、`deepseek-v4.1-flash-free` | 全部 mapped ✅ |
| 它们的**显示名**（`/v1/models` 剥掉 `-free` 后的形态，`main.go:2643`） | `space-bunny`、`mimo-v2.6-flash`、`deepseek-v4.1-flash` | 全部 mapped ✅（今天目录里三个 `-free` 孪生都在） |
| 目录已摘掉 `-free` 孪生（漂移） | `deepseek-v4-flash-free`、`x-preview-f-free`、`mimo-v2.5-free`、`step-5-preview-free` | 全部 mapped ✅（`-free` 约定与目录无关） |
| 部分/残缺目录（只收录 1 个模型） | 4 个 `-free` 模型 | 全部 mapped ✅ |
| 刚刷新后文档表缩到只剩 `big-pickle` | `big-pickle` | mapped ✅ |
| **R6 边界**：目录无 `-free` 孪生时，显示名反查失效 | `deepseek-v4-flash` | **判 unmapped**（目录今天没有 `deepseek-v4-flash-free`）→ 见 §12-③ |
| 安全闸门（目录/文档未就绪） | 任意 | 一律 mapped ✅ |

---

## 9. 变异测试（证明实现者的测试真的会红）

在 `/tmp/t23/mut`（工作区完整副本，`rsync` 排除 `.git/.gocache/.modcache`）上逐个注入变异，**同一时刻只跑两套测试**：
- 实现者套件（`TestUnmapped* / TestStatsRangeResponseExposes* / TestCallRecordUnmapped* / TestTopUnmapped* / TestLegacyRecords* / TestIsUnmappedModelHasNoRefreshSideEffect / TestEnsureFreeModelsFresh*`）
- 我的套件（`TestT23*`，8 个用例）

| 变异 | 实现者套件 | 我的套件 |
| --- | --- | --- |
| **基线（未变异）** | 绿 | 绿 |
| **M1** 删除安全闸门（`if !docsReady { return false }`） | 🔴 `TestUnmappedPredicateStructuralContract`、`TestIsUnmappedModelHasNoRefreshSideEffect` | 🔴 `TestT23DecisionMatrix/empty*` |
| **M2** 窄化为"只看 `-free` 后缀"（丢弃文档免费表判据） | 🔴 `TestUnmappedPredicateStructuralContract` | 🔴 `TestT23DecisionMatrix/docs`、`TestT23ReverseRisk` |
| **M3** 删除未映射记录的 `call_log` 落盘（`append` 直接 return） | 🔴 `TestUnmappedNotAggregatedButStillLogged`、`TestUnmappedExcludedFromModelStatsAndDenominator`、`TestStatsRangeResponseExposesUnmappedRequests` | 🔴 `TestT23StatsSurfaceNoSilentDrop`、`TestT23ConservationTwoTables`、`TestT23TopNAndOtherAndWindow` |
| **M4** 用上游目录判据替换 `-free` 命名约定 | 🔴 `TestLegacyRecordsRecomputedButPersistedJudgmentWins` | 🔴 `TestT23DecisionMatrix/suffix*`、`TestT23ReverseRisk` |
| **M5** 取消落盘判定权威性（`SawUnmapped` 失效，老行一律重算） | 🔴 `TestLegacyRecordsRecomputedButPersistedJudgmentWins`、`TestUnmappedBackfillMatchesAppend` | 🔴 `TestT23LegacyVsNewPath` |
| **恢复后复跑** | 绿 | 绿 |

**还原方式与验证**：变异只存在于 `/tmp/t23/mut`；每次变异前从 `/tmp/t23/pristine/` 覆盖回原文件，结束后 `sha256sum` 三文件与原始值逐一相等；工作区 `main.go / calllog.go / statscan.go / static/admin.html / node_alert.go` 五个文件的 sha256 在全程**未被改动**（`sha256sum -c` 全通过）。**没有对工作区做过任何变异。**

---

## 10. 回归

| 命令 | 结果 |
| --- | --- |
| `go test -count=1 ./` | ✅ `ok ... 78.950s`，`full_exit=0`（`task8_bucket_query_test.go` 按既定状态仍在 `/tmp/t8_hold.go`，未动） |
| `go test -race -count=1 ./` | ✅ `ok ... 480.457s`，`race_exit=0` |
| `free_models_cache.json` md5（跑测前后） | `11300b016ed08ded2ae81502b7a2b196` **未变**（无测试污染工作区） |

> 全量套件比实现者报的 41 s 慢（78 s），因为本机同时跑着验收脚本；`-race` 同理。**退出码是判据，耗时不是。**

---

## 11. 措辞检查

- 实现者报告 §0/§10 + 代码注释：`+0.2976 pp` 一律表述为**"口径更正"**，并明确写"不是服务质量提升""不得算进成功率提升"——✅ 无违规表述。
- 我全仓扫 `提升 / 改善 / 变好`：`docs/research/stats-unmapped-model-fix.md` 的 3 处命中全是**否定式**（"不是…提升"）；`main.go/calllog.go/statscan.go` 的新增注释**零命中**。
- `ROADMAP.md` 对 task-21 的描述是"未映射模型计数尚未上线 / 否决公式 A、改用公式 B"，**未**把它写成战果或成功率提升——✅。
- ⚠️ 给下游（面板 task-22 / ROADMAP 战果行）的提醒：这个 +0.30 pp 的正确表述是"**分母去掉从未提供过的模型**"，且**面板文案不得出现"成功率提升"字样**。

---

## 12. 发现与残留（5 条，按严重度）

### ① 生产日志里有 2 条合成探测记录（低，建议清理/标注）
`probe-143148`（2026-10-18T14:31:48.370+08:00）、`probeL-143230`（14:32:30.079+08:00），`upstream http 400`、0 成功，模型名里带时间戳。它们使 live 排除集从 661 变 663、Δ 从 +0.2976 降到 +0.2566。**不影响硬门禁**（0 ok）。建议：后续引用生产数字时用 661（prod-now 口径），或明确说明 live 已含 2 条探测；面板上出现 `probe-*` 属正常。

### ② 大小写不做归一（低，行为正确但需文档化）
`MIMO-V2.6-FLASH-FREE` → unmapped；`MIMO-V2.5-free` → mapped（后缀命中）。这与上游行为一致（上游对大小写也不认），但报告未写明。**不建议改**（归一化会让"客户端手抖大写"这类真实失败从分母消失，与 §8 的保守取舍冲突）。

### ③ R6：显示名反查依赖上游目录（已知，实现者报告已列，建议立独立任务）
`deepseek-v4-flash-free` 仍可服务（4,566 条 / 97.17%），但上游目录今天已不含它 ⇒ 其**显示名** `deepseek-v4-flash` 若被客户端发来，会被判 unmapped（`resolveModel` 反查失败）。当前生产日志里**没有**显示名形态的请求（0 条），所以**没有实际数据被误删**。这是**路由** bug（与统计口径正交），实现者报告 §3.4-6/§R6 已声明"未修"，我确认它真实存在且当前无数据影响。

### ④ 注释把"无副作用"写成"纯函数（只依赖 (model,docs)）"（低，文档精度）
`main.go:1478-1486` 附近的注释与 `calllog.go` 回填段注释都暗示判定只依赖 `(model, docs)+别名`。实际还依赖 `modelsCache/goModelsCache`（`-free` 反查）。**无副作用**这一点我实测成立；**封闭性**不成立。建议把注释改成"无副作用、不刷新、不派发 goroutine；依赖 (model, docs, 别名表, 上游目录快照)"，避免下一个人按"纯函数"假设做缓存/单测。

### ⑤ R1 的具体触发条件（中，需监控）
老行（无 `unmapped` 键）重算依赖文档免费表。唯一"不靠 `-free` 后缀、只靠文档表"的已映射模型是 `big-pickle`（live 7 条、**全部成功**）。**若文档页把 `big-pickle` 移出免费表**：老行重算会把这 7 条成功记录追认成 unmapped 并删出统计；新行（带 `"unmapped":false`）不受影响。
→ **监控断言**：`unmapped_requests.by_model` 里出现任何**有成功记录**的模型名，即说明判据过严（实现者报告 §R2 已提出同一断言）。当前 0 例。

---

## 13. 复现

```bash
cd /home/jingyx/project/opencode2api-test
export PATH=$PATH:/usr/local/go/bin GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"

# 1) 我的独立验收套件（10 个 Test 函数，含 2 个真实日志用例；实测 15.804s）
go test -run 'TestT23' -count=1 -v .

# 2) 单独看硬门禁（4 组目录快照 × 2 个数据集，约 15 s）
go test -run 'TestT23RealLogHardGate' -count=1 -v .

# 3) 真实回填路径 == 直接判定（约 3 s）
go test -run 'TestT23RealLogBackfillEqualsDirect' -count=1 -v .

# 4) 真实 HTTP 端到端（端口 9041，不碰 8000 / 不碰生产容器）
mkdir -p /tmp/t23/e2e && cd /tmp/t23/e2e
cp /tmp/prod-now.jsonl call_log.jsonl
printf '{"model_alias":null,"reasoning_effort_map":null,"force_disable_thinking":false,"quota_error_signals":{}}' > config.json
go build -o /tmp/t23/oc2a /home/jingyx/project/opencode2api-test
/tmp/t23/oc2a -port 9041 -password "" &
curl -s 'http://127.0.0.1:9041/api/stats?range=180d' | python3 -m json.tool | grep -A8 unmapped_requests
curl -s -X POST http://127.0.0.1:9041/v1/chat/completions -H 'Content-Type: application/json' \
     -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
curl -s 'http://127.0.0.1:9041/api/stats?range=today'

# 5) 变异（只在副本上；工作区零改动）
#    /tmp/t23/mut = 工作区副本；/tmp/t23/pristine = 三文件原件
#    脚本：/tmp/t23/mutate2.py（每次变异前 restore，结束后 sha256 校验）

# 6) 原始数据（只读）
docker exec opencode2api wc -l /data/call_log.jsonl      # 231,992 @ 2026-10-19 18:52
docker exec opencode2api cat /data/call_log.jsonl > /tmp/t23/live.jsonl
```

**我的验收文件**（gitignored，不进仓库）：`zz_t23_verify_test.go`（10 个 Test 函数 / 22 个判定子用例，1,051 行）。

---

## 14. 一句话给 Lead

硬门禁 **0 ok 成立**（prod-now 661 条 / live 663 条，4 组目录快照全部相同）；实现者报的 661 / 203,576 / 91.6504%→91.9480% / +0.2976pp / by_model / 守恒全部**逐位复现**；判定逻辑 23 场景、统计面（含真实 HTTP）、追溯路径（含 `SawUnmapped`）、字段形状、反向风险、5 个变异、全量 + `-race` 回归全部通过。**可以合并**；报告有 1 处小错（`mimo-v2.5-free` 最后成功时间少算 1 条，不影响结论），另需记录：2 条 `probe-*` 探测记录、`+0.30pp` 只能说"口径更正"、R6 显示名反查仍是独立 bug。
