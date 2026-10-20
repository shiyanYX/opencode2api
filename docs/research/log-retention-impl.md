# 调用日志后端改造（按天分文件 / 180 天保留 / 字段精简 / 历史查询）—— 实现报告

**状态：阶段 2 实现已完成；复验判 no-go 的三条缺陷 D1/D2（首轮）与 D3（第二轮，分页游标类根因）均已修复，待原验收方复验。**
本文件是交付报告 + 修复记录，不对「任务完成」下结论。首轮验收报告见
[`log-retention-verification.md`](log-retention-verification.md)（no-go，2 条缺陷，均已修，修复章节见 §12）。

- 任务：task-56 阶段 2（阶段 1 方案见 [`log-retention-impl-plan.md`](log-retention-impl-plan.md)）
- 路径：`/home/jingyx/project/opencode2api-test`，分支 `test`
- 边界遵守：只改 `calllog.go` / `main.go`（接线与 flag），新建 3 个测试文件与本报告；**未碰** `proxy_node.go`、`node_alert.go`、`static/admin.html`、`ROADMAP`；未碰生产容器、未用 8000 端口、未执行任何写 git 的操作（仅只读 `git diff/status` 取证）。
  > 工作区里 `static/admin.html` 等文件的改动是并行任务（U3）的，不是本任务产生的：该文件 mtime 仍是 09:55（我接手前），本任务全程未写它。

---

## 1. 结论摘要

| 项 | 结果 |
|---|---|
| 按天分文件 | `call_log/YYYY-MM-DD.jsonl`（本地日期，与 `hourKeyFor` 同口径），legacy `call_log.jsonl` 作**冻结第 0 段**，永不改写 |
| 回填兼容 | 段序 = 原文件字节序；游标 `(segIdx, segOff)` + 全局 `parsedTo` 镜像（单文件模式字节级不变） |
| **四窗口不变性** | L1/L2/L3 三布局下 `today/7d/30d/180d` + trends + unmapped **逐字节相等**，桶 1,089 个逐字段一致，守恒通过（T8 PASS） |
| 冷启动 | 124.5 MB 夹具：L2（legacy 124MB+当日）**0.68 s**、L3（61 个日文件）**0.46 s**；比阶段 1 基线 668 ms 持平/更快（流式吸收，峰值内存从 124 MB 降到 ~256 KiB 缓冲） |
| 字段精简 | 新行 −16.2%（最近 14 天 1.348 → 1.129 MB/天）；默认值/零读者字段不落盘，**读取侧服务端还原**，面板与第三方消费者无感知（未改前端） |
| 历史查询 | `/api/call-log` 支持 `limit/before/after/model/status/q`，无参数走内存环（旧行为逐字节兼容）；同 ts 多记录分页不漏（D2 修复，§12.3）；无索引，单日倒读 ms 级 |
| 保留 | gzip 归档 + 180 天删除（D1-B）；投影 90 天 143.7 MB / 180 天 156.0 MB 封顶（legacy 124.5 MB 常驻不变） |
| 回滚开关 | `-calllog-layout=single`：连落盘格式都退回旧版，已测试 |
| 变异测试 | 14 个变异 **14/14 被测试抓住**（含 D1/D2 缺陷本体变异 M13/M14，§12.4） |
| **分页（task-65 D3）** | 历史查询分页改**位置游标**（段标识:行号），与 ts 是否有序无关：生产真实序（29.3% 相邻逆序）下 limit=500 全量连翻 467 页 / 233,444 条 **0 漏 0 重**；`before/before_req_id` 旧语义保留（§13） |
| 回归 | `go vet` PASS；`gofmt` 干净；修复后 `go test -count=1 ./` 通过（226.5 s）、子系统 `-race` 通过（317.0 s）；全量 `-race` 失败点是**既有**的 `proxy_node.go`/`zz_t30` 竞争（HEAD 基线可复现，见 §8） |

## 2. 改动清单

```
$ git diff --numstat -- calllog.go main.go
1083    66      calllog.go
8       0       main.go
```
（`statscan.go` 未改；`*_test.go` 是新增文件，且本仓库 `.gitignore:26` 忽略 `*_test.go`，所以它们不会出现在 `git status` —— 与仓库既有测试同款，验收时直接看工作区文件。）

新增文件：
- `calllog_daily_test.go`（418 行）：T1 段布局、T2 写入、T3 多段回填/等价性/gz/幂等、T4 尾部加载、clear、非最后段半行
- `calllog_retention_test.go`（425 行）：T5 落盘精简+还原、T6 历史查询/分页/HTTP 头、T7 归档/删除、layout 开关
- `calllog_invariance_test.go`（227 行）：T8 三布局四窗口逐字节验收（`T56_DAY_DIR` 环境变量驱动，默认 skip）

哈希（提交验收时核对）：
```
$ sha256sum calllog.go main.go statscan.go
27e6c185f24e106d6717adb395e0162abf98272474413842bc1fadfe42057a33  calllog.go
4394576f81b3b83cca65ee569da678a53cac58fb7f5f7c9593d7367f7b57d124  main.go
47a066beabad1bea8653a6946f24880e118d892ecc63ff28de324b1b694201cb  statscan.go
```

新增命令行开关（`main.go:7612-7617`，`initCallLog` 读取，`startCallLogArchiver` 在 `main.go:7645` 接线）：
- `-calllog-layout`（默认 `daily`；`single` = 回滚模式，只读写 legacy 单文件，落盘格式也退回旧版）
- `-calllog-gzip-after-days`（默认 `2`）
- `-calllog-retain-days`（默认 `180`，D1-B）

核心实现（全部在 `calllog.go`）：
- `callLogSegment` / `absorbCursor` / `dailySegs`+`dailyCur`：段列表与游标
- `ensureDailySegsLocked`：目录→段列表（legacy 恒第 0、日文件名字典序、同日 plain 优先、未知文件忽略）
- `dailyTargetLocked` / `dailyCreateSegmentLocked`：**只追加到最后一段**，按 append 时刻的本地日期滚动（跨午夜长请求不回写旧段）
- `absorbSegmentDaily`：**流式**逐行吸收（`bufio` 256 KiB），非最后段尾部半行消费并记 `unabsorbed`，gz 段 `gzip.NewReader` 流式解压
- `absorbOnceDaily`：段序消费 + 与单文件版一一对应的四条提交守卫（`statsGen` / 游标 handoff / 合并 / 计数同临界区）
- `dailySegments`/`loadCallLogFromFileDaily`：跨段倒取最后 2000 条（明文段走 `tailReadLastLines`，不整读 124 MB）
- `diskRecordForWrite`/`restoreDiskRecord`：落盘精简 + 服务端还原（**深拷贝** events，避免污染内存环）
- `historyQuery`：无索引倒读查询 + 分页游标
- `compressSegmentFile`/`maintainRetention`/`startCallLogArchiver`：gzip 归档 + 按天删除（仅 seeded 且非游标段）
- `adminCallLogHandler`：新增查询参数与 `X-Call-Log-*` 响应头；无参数路径不变

## 3. T1–T8 逐项证据

> 所有命令的工作目录与本机 Go 环境：
> `export PATH=$PATH:$HOME/.local/opt/go/bin; export GOCACHE=$PWD/.gocache GOMODCACHE=$PWD/.modcache GOTMPDIR=/tmp/t56/gotmp`

### T1 段列表 + 游标
- 测试：`TestDailySegmentsOrderLegacyFirstThenDaysAscending`
- 断言：legacy 恒第 0；日文件按 `YYYY-MM-DD` 升序；`notes.txt` 等未知文件忽略
- 结果：PASS
- 变异 `M1`（legacy 排最后）→ 该测试 FAIL（抓住）

### T2 daily 写入路径
- 测试：`TestDailyAppendWritesTodaysFileAndIncr`、`TestDailyRolloverNeverWritesBackToOlderSegment`、`TestLayoutSingleFlagFallsBackToSingleFile`、`TestLayoutDailyCreatesDirAndUsesDayFiles`
- 断言：写今天的天文件；seeded 时写盘+incr+游标推进同临界区；**TS 属于昨天但 append 发生在今天 → 写今天，不回写旧段**；`single` 开关不建日目录、只写 legacy
- 结果：PASS
- 变异 `M2`（日切仍写最后一段）→ `TestDailyRollover…` FAIL；`M11`（daily 游标用 `parsedTo` 推进）→ `TestDailyAppend…` FAIL

### T3 多段升序回填
- 测试：`TestDailyAbsorbReplaysAllSegmentsIdempotently`、`TestDailySplitLayoutStatsEqualSingleFile`、`TestDailyGzSegmentAbsorb`、`TestDailyNonLastSegmentPartialLineConsumed`
- 断言：legacy+2 日文件一次吸完（absorbed=5、桶=3、seeded=true）；二次吸收幂等；新增日文件只吸新段；**同一批记录「单文件 vs 分文件」桶表逐字段相等（含混存 `+08:00`/`Z` 时区）**；gz 段可吸；非最后段半行被消费且守恒通过
- 结果：PASS
- 变异 `M4`（半行不消费）→ 半行测试 FAIL；`M10`（gz 当明文）→ gz 测试 FAIL；`M12`（daily 不分派）→ 多段回填测试 FAIL

### T4 内存环 + everServed 播种
- 测试：`TestDailyLoadCallLogTailAcrossSegments`
- 断言：legacy 1500 条 + 日文件 1500 条 ⇒ 内存环 2000 条，最旧来自 legacy 尾部、最新来自最后一天；`servedModels` 播种与单文件共用 `installLoadedRecords`
- 结果：PASS
- 变异 `M5`（只读最后一段）→ FAIL（只剩 1500 条）

### T5 字段精简 + 读取侧兜底
- 测试：`TestDailyDiskTrimDefaultFieldsAndRestore`、`TestDailyDiskTrimKeepsNonDefaultValues`、`TestSingleFileModeKeepsLegacyDiskFormat`
- 断言：`path`/`route_mode` 默认值、`events[].node_name`、`final_status`、`dial_ms` 不落盘；**内存环字段完整**；重启后从磁盘恢复时默认值与事件名被服务端还原；非默认值照常落盘；`single` 模式落盘格式保持旧版
- 结果：PASS
- 变异 `M3`（events 不做深拷贝）→ 内存环被污染，FAIL；`M8`（读取侧不还原 node_name）→ FAIL

### T6 历史查询契约
- 测试：`TestCallLogHistoryFilters`、`TestCallLogHistoryPaginationNoGapsNoDupes`、`TestCallLogHandlerNoParamsStillServesMemory`、`TestCallLogHandlerHistoryHeaders`
- 断言：默认最新在前；`model`/`status`/`q` 过滤；`before` 排他、`after` 包含；`limit`+`hasMore`；分页并集无缺无重；无参数仍返回内存环并在 `X-Call-Log-Source: memory`；历史路径 `X-Call-Log-Source: file` / `Has-More` / `Next-Before`；非法 `before` 400
- 结果：PASS
- 变异 `M7`（`before` 等值边界变包含）→ FAIL

### T7 归档 / 删除
- 测试：`TestRetentionArchiveCompressesOldDaysAndPrunes`、`TestRetentionRequiresSeeded`
- 断言：超期段删除、超过 `gzip-after-days` 的段压缩为 `.jsonl.gz`、归档窗口内保持明文、最后一段不压缩；归档/删除不改变内存桶；**归档后冷启动重放 gz 段统计正确且不重复计数**；未 seeded 时不动任何文件
- 结果：PASS
- 变异 `M6`（删掉 prune 逻辑）→ FAIL

### T8 四窗口不变性（硬门禁）
命令与结果：
```
$ T56_DAY_DIR=/tmp/t56/day go test -run TestT56FourWindowInvariance -count=1 -v ./
L1 单文件:            124,520,431 B  冷启动 574.6 ms  absorbed=233,444 桶=1,089
L3 纯日文件(61 个):                  冷启动 580.7 ms  absorbed=233,444 桶=1,089
L2 legacy+最后一天:                  冷启动 485.9 ms  absorbed=233,444
窗口 today 三布局逐字节相等
窗口 7d    三布局逐字节相等
窗口 30d   三布局逐字节相等
窗口 180d  三布局逐字节相等
守恒通过：ΣN=233,444 == absorbed 233,444 − unabsorbed 0
T8 PASS
```
另加 HTTP 级交叉验证（同一份夹具有两种布局各起一个实例）：
```
L2 cold=0.68s warm=0.00s
L3 cold=0.46s warm=0.00s
两次响应 sha256 相同：d2de8c235a9a8bc6822a3bfee2ae04afbe3eff25f4fa1ae5f73be60e293af611
```
（响应体积 486,932 B，`total_requests`：today 1,289 / 7d 28,868 / 30d 175,597 / 180d 233,444。与阶段 1 方案里的 873/27,739/173,846/231,654 不同，是因为阶段 1 基线取自更早的 fixture 快照；**不变性的判定是「同一份字节、三种布局」**，不依赖这个绝对数。）

## 4. 字段精简明细（与方案的差异）

| 字段 | 落盘 | 读取侧 |
|---|---|---|
| `path` | 等于 `/v1/chat/completions` 时不写 | 服务端 `restoreDiskRecord` 补默认值（面板/第三方无感知） |
| `route_mode` | 等于 `public` 时不写 | 同上 |
| `events[].node_name` | 有 issue 的记录保留 events，但去 `node_name` | 用同记录顶层 `nodes` / `node_names` 映射还原（生产实测 233,750/233,750 可还原） |
| `final_status` / `dial_ms` | 不写 | 无读者，不还原（内存环里仍有） |

**与阶段 1 方案的差异（有意为之）**：方案原打算改 `static/admin.html` 做 3 处兜底；阶段 2 边界明确禁止碰 `admin.html`（它是 U3 的 pin，正在整体改造）。因此改成**服务端还原**：`loadCallLogFromFile`（重启恢复内存环）与 `historyQuery`（历史查询结果）在返回前补齐默认值与事件名。效果等价且前端零改动；等 U3 改造后前端若直接读落盘字段，也可以继续依赖这个服务端还原（不会重复破坏）。

另一个实现期发现：`diskRecordForWrite` 里清 `events[].node_name` **必须深拷贝 events 切片**——直接改 `d.Events[i]` 会污染内存环（实测 append 后 `/api/call-log` 事件名变空）。已由变异 `M3` 与对应测试钉住。

## 5. `/api/call-log` 契约（实现值）

| 参数 | 取值 | 行为 |
|---|---|---|
| `limit` | 1..2000，非法按默认 | 无参数默认 500；历史路径默认 200；超过 2000 截断 |
| `cursor` | `c1_` 开头的不透明 token | **位置游标**（task-65）：从 token 指定行的下一条物理行续扫，与 ts 是否有序无关；非法 → 400 `bad-cursor`，失效 → 400 `stale-cursor`（见 §13.2/§13.7） |
| `before` | RFC3339 或 `YYYY-MM-DD[ HH:MM[:SS]]` | 排他上界；非法 → 400 |
| `before_req_id` | 字符串或位置游标 token | 裸字符串 = 同 ts 的分页严格游标（旧语义）；`c1_` token（新 `X-Call-Log-Next-Before-Req` 回传值）= 位置续扫，段已删时回退 before 语义并回 `X-Call-Log-Cursor-Fallback: before`（见 §13.2） |
| `after` | 同上 | 包含下界；非法 → 400 |
| `model` | 精确匹配 | |
| `status` | `ok`/`fail`/`all` | 其它 → 400 |
| `q` | 子串（大小写不敏感，匹配原始行） | |

- 无任何过滤参数 ⇒ **内存环路径**，行为与改造前逐字节一致，只多一个 `X-Call-Log-Source: memory` 头。
- 有任一参数 ⇒ 文件历史路径：`X-Call-Log-Source: file`、`X-Call-Log-Has-More`、`X-Call-Log-Next-Cursor`（task-65 位置游标）、`X-Call-Log-Next-Before`、`X-Call-Log-Next-Before-Req`（不回退时是同款位置游标 token）；触及 256 MiB 扫描预算时 `X-Call-Log-Truncated: true`；错误时 `X-Call-Log-Error: bad-cursor|stale-cursor`。
- 响应仍是 `[]CallRecord`（最新在前），未改包裹结构；历史路径的记录经过 `restoreDiskRecord`。

## 6. 变异测试（首轮 12/12；task-63 后 14/14；task-65 后累计 **17/17**，新增 M13–M17 见 §12.3/§13.4）

方法：把当前工作区复制到 `/tmp/t56/mut`，逐条注入变异 → 跑对应测试 → 期待失败 → 还原。脚本 `.research/t56/mutation_harness.py`，结果 `.research/t56/evidence/mutation_results.txt`。

| # | 变异 | 抓住它的测试 |
|---|---|---|
| M1 | 段排序把 legacy 放最后 | `TestDailySegmentsOrderLegacyFirstThenDaysAscending` |
| M2 | 日切仍写最后一段（不建新天文件） | `TestDailyRolloverNeverWritesBackToOlderSegment` |
| M3 | events 精简不做深拷贝（污染内存环） | `TestDailyDiskTrimDefaultFieldsAndRestore` |
| M4 | 非最后段半行不消费 | `TestDailyNonLastSegmentPartialLineConsumed` |
| M5 | 尾部加载只读最后一段 | `TestDailyLoadCallLogTailAcrossSegments` |
| M6 | 删掉超期段的 prune 逻辑 | `TestRetentionArchiveCompressesOldDaysAndPrunes` |
| M7 | `before` 等值边界变包含 | `TestCallLogHistoryFilters` |
| M8 | 读取侧不还原 `node_name` | `TestDailyDiskTrimDefaultFieldsAndRestore` |
| M9 | `clear()` 不删日文件 | `TestDailyClearRemovesEverySegment` |
| M10 | 吸收时 gz 段当明文读 | `TestDailyGzSegmentAbsorb` |
| M11 | daily 游标推进改用 `parsedTo` | `TestDailyAppendWritesTodaysFileAndIncr` |
| M12 | daily 模式不分派，走单文件吸收 | `TestDailyAbsorbReplaysAllSegmentsIdempotently` |

**一个等价变异（如实记录）**：最初尝试「删掉 `maintainRetention` 的 `if !seeded { return }`」——测试抓不住，因为游标守卫（`i >= dailyCur.idx`）在未 seeded 时同样阻止了所有动作，属于等价变异（不是测试漏洞）。因此换成 M6（删 prune 本身）才是有观测差异的变异。

## 7. 冷启动 / 磁盘 / 内存实测

见 `.research/t56/evidence/impl-measurements.txt` 与 `disk_projection.txt`。要点：

- **冷启动**（daily 模式二进制，fixture 124.5 MB）：L2 0.68 s、L3 0.46 s（阶段 1 基线：旧代码单文件 HTTP 首查 668 ms）。
- **流式吸收**：`absorbSegmentDaily` 用 256 KiB `bufio` 缓冲，不再把 legacy 124 MB 一次读进内存；实现过程中先做整读版本时 L2 冷查询是 1.71 s，改流式后 0.68 s（顺带把加载路径的 `tailReadLastLines` 用在明文段上，避免为 2000 条读 124 MB）。
- **磁盘**（最近 14 天实测外推）：
  - 新格式 1.129 MB/天（现状 1.348 MB/天，−16.2%）
  - 90 天保留：legacy 124.5 + 热 7.9 + gz 11.3 = **143.7 MB（封顶）**
  - 180 天保留：legacy 124.5 + 热 7.9 + gz 23.6 = **156.0 MB（封顶）**
  - 对照：现状无上限，一年约 492 MB（不含 legacy）
- **gzip 比例**（最近 7 天实测）：原格式 9.61×，新格式 8.28×。

## 8. 回归验证

```
$ go vet ./                                   # PASS（无输出）
$ gofmt -l calllog.go main.go statscan.go calllog_*_test.go   # 无输出
$ go test -count=1 ./                         # ok  github.com/shiyanYX/opencode2api  191.9s
$ go test -race -count=1 -timeout 45m ./      # FAIL：既有 DATA RACE（见下，非本任务引入）
$ T56_DAY_DIR=/tmp/t56/day go test -race -count=1 -timeout 15m \
    -run 'TestDaily|TestCallLog|TestRetention|TestLayout|TestSingleFileModeKeeps|TestAppend|TestAbsorb|TestBackfill|TestBucket|TestConservation|TestClear|TestUnmapped|TestT39|TestT56|TestCallRecorder|TestHour|TestTrend|TestModelStats|TestSlim|TestParseStats' ./   # ok 79.020s
```
新增测试 21 个（`T56_DAY_DIR` 未设置时 T8 skip，其余 20 个常跑），全部 PASS；原仓库普通测试全绿。

**全量 `-race` 的失败点与本任务无关（已用基线证明）**：`go test -race ./` 报 DATA RACE：
`proxy_node.go:1347 (*nodePool).saveState`（后台 goroutine） ←→ `zz_t30_alert_safety_test.go:274 TestT30CriterionCPoolEmptyReachability`。
两个文件本任务都未触碰，且用 **HEAD 原始代码**（`calllog.go` sha256 `6ee8ed59…`，工作区 `calllog.go` 为 `27e6c185…`）单独跑该测试同样报 `WARNING: DATA RACE`：
```
$ cd /tmp/t56/repo && go test -race -run TestT30CriterionCPoolEmptyReachability -count=1 ./
WARNING: DATA RACE ... / TESTT30 ... FAIL
```
⇒ 属既有测试并发问题，不在本任务修复范围（且边界禁止改 `proxy_node.go`）。本任务全部相关子系统 `-race` 干净。
另：首次全量 `-race` 还遇到默认 10 分钟超时（`panic: test timed out` 600.7 s）——本仓库普通测试已需 192 s，`-race` 下 10 分钟不够；这与竞争无关。

## 9. 上线步骤 / 回滚

**上线（下次发布）**
1. 用当前代码构建新二进制；启动参数保持默认（`-calllog-layout=daily`、`-calllog-retain-days=180`）。
2. 首次启动：`initCallLog` 创建 `<data>/call_log/`；`legacy call_log.jsonl` 原样保留、只读。
3. 首次 `/api/stats` 查询触发一次全量回填（124.5 MB 实测 0.68 s，惰性触发，不影响启动）。
4. 此后每小时后台跑一次保留策略：>2 天的日文件 gzip、>180 天的日文件删除（`legacy` 永不删）。
5. `docker log-opts`（run log 无限增长问题）不属本次代码改动，仍按阶段 1 方案随下次发布处理。

**回滚**
- 快速回退（不换二进制）：`-calllog-layout=single` 重启 → 只读写 legacy 单文件，新写入回到旧格式；日目录不再被读取（历史日文件先留着，需要合并时按阶段 1 方案 §6.2 的脚本按日期升序 `cat` 回 `call_log.jsonl`）。
- 只用旧二进制：同样先合并日文件再启动（脚本见方案 §6.2）。
- 验证：回滚后 `GET /api/stats?range=today|7d|30d|180d` 与回滚前快照比对。

## 10. 风险与遗留

1. **归档有最长 1 小时延迟**：后台 ticker 每小时一次，且只在 seeded 后动作；刚过 `gzip-after-days` 的段最多 1 小时后才压缩。可接受（归档不是实时语义）。
2. **保留期与 180d 窗口**：D1-B 已解决（180 天），`-calllog-retain-days` 调到 <180 会重新引入「重启后 180d 缩水」；代码未强制，文档与 flag 说明已写。
3. **历史查询分页假设**：同 ts 的记录用 `(ts, req_id)` 字典序做严格游标；若未来出现同一毫秒大量记录且 req_id 非单调，分页可能重复/漏一条。当前生产 req_id 是 UUID、同 ms 极少，测试覆盖了常规场景。彻底修复需要全局排序索引（本阶段明确不做）。
4. **测试文件被 `.gitignore` 忽略**（`*_test.go`），提交时需 `git add -f`（与仓库既有测试同款）。本任务按要求不执行写 git 操作。
5. **前端历史查询（T9）未做**（D3）：契约已就绪，等 U3 面板整体改造时接。
6. **并发工作区**：`static/admin.html`、`docs/mockups/u3/*` 等改动来自并行任务，本任务未触碰。
7. **D1/D2（首轮验收 no-go 的两条）已修复**，修复记录、修前红/修后绿原始输出、门禁加固与重跑结果见 §12。

## 11. 复算命令

```bash
cd /home/jingyx/project/opencode2api-test
export PATH=$PATH:$HOME/.local/opt/go/bin
export GOCACHE=$PWD/.gocache GOMODCACHE=$PWD/.modcache GOTMPDIR=/tmp/t56/gotmp

# 新增测试（T1-T7）
go test -run 'TestDaily|TestCallLogHistory|TestCallLogHandler|TestRetention|TestLayout|TestSingleFileModeKeeps' -count=1 -v ./

# T8 四窗口不变性（需要夹具：生产快照按本地日期切好的 61 个日文件）
T56_DAY_DIR=/tmp/t56/day go test -run TestT56FourWindowInvariance -count=1 -v ./

# D1/D2 缺陷回归（task-63）
go test -run 'TestDailyOnlyDeploymentTrendsNotEmpty|TestSingleFileModeTrendsStillNilWhenFileMissing|TestSameTS|TestBeforeReqIDUnknownCursorFallsBackToOrderingKey' -count=1 -v ./

# 验收方缺陷探针（修复后应 PASS）
go test -run 'TestZZIP4VDefect' -count=1 -v ./

# D3 位置游标 / 生产序分页（task-65）
go test -run 'TestProdOrder|TestHistoryCursorInvalidAndStale|TestLegacyHeaderStaleCursorFallsBackToBefore|TestCursorSurvivesGzipRename' -count=1 -v ./
ZZ_PROD_LINES=30000 go test -run TestZZIP4VDefectProdSlicePagination -count=1 -v ./

# 全量回归
go test -count=1 ./

# 变异测试（12 个变异，期望 12/12 CAUGHT）
rsync -a --exclude='.gocache' --exclude='.modcache' --exclude='.research' \
  --exclude='.git' --exclude='bin' --exclude='docs' --exclude='docker' \
  --exclude='deploy' --exclude='scripts' --exclude='opencode2api' --exclude='.gotmp' \
  ./ /tmp/t56/mut/ && python3 .research/t56/mutation_harness.py
```

## 12. task-63：首轮验收 no-go 缺陷修复（D1 / D2）

首轮独立验收（[`log-retention-verification.md`](log-retention-verification.md)，task-61）给出 **no-go**：
其余 10 项全通过（含「生产 233,654 行旧实现 vs 新实现逐字节相同」），但 2 条缺陷必须修。本节是修复记录。

### 12.1 D1 —— 纯日文件布局 / 全新部署下 `/api/stats/trends` 恒返回 `[]`

**根因**（已复现确认）：`callLogFileExists()`（修复前 `calllog.go:1469`）只 `os.Stat` legacy
`call_log.jsonl`；`trendsFromCallLog()` 用它区分「文件不存在（返回 nil → handler 输出 []）」与
「存在但为空」。daily 布局下 legacy 只是**冻结的第 0 段**，全新部署时它**永远不会被创建**，
于是 L3 布局与新部署的 trends 恒空、且不报错。

**修法**（`callLog.go`，最小改动）：daily 模式（`dir != ""`）改为「存在任一段即算存在」——
复用 `readSegments()`（段列表有目录 mtime 缓存，代价与原来一次 `Stat` 同量级）；单文件模式
保持原语义（只认 legacy）。

**修前红 / 修后绿**（原始输出，`-overlay` 注入还原修复前代码，不动工作树）：
```
# 修前红：把 daily 分支还原成「只看 legacy」
$ go test -overlay=/tmp/t56/zz/mut/overlay_d1.json -run TestDailyOnlyDeploymentTrendsNotEmpty -v ./
    calllog_defects_test.go:45: daily 布局（无 legacy）下 trendsFromCallLog(today) 返回 nil → handler 会输出 []
--- FAIL: TestDailyOnlyDeploymentTrendsNotEmpty
# 修后绿
$ go test -run 'TestDailyOnlyDeploymentTrendsNotEmpty|TestSingleFileModeTrendsStillNilWhenFileMissing' -v ./
--- PASS: TestDailyOnlyDeploymentTrendsNotEmpty
--- PASS: TestSingleFileModeTrendsStillNilWhenFileMissing   # 单文件模式「文件不存在→nil」红线未破
# 验收方的缺陷探针（修复前 FAIL，修复后 PASS）
$ go test -run 'TestZZIP4VDefect' -v ./
--- PASS: TestZZIP4VDefectL3TrendsEmpty
--- PASS: TestZZIP4VDefectPaginationDupTS
```
新增测试：`TestDailyOnlyDeploymentTrendsNotEmpty`（日文件存在无 legacy + 真正全新部署首次写入两场景，
含 HTTP handler 出口断言：点数非 0、`total requests` 正确）、
`TestSingleFileModeTrendsStillNilWhenFileMissing`（单文件模式红线的反例锁）。

### 12.2 D2 —— 同 ts 多记录的分页会漏行

**根因**（已复现确认）：`historyQuery` 按文件**倒序**扫描，却把 `before_req_id` 当**排序键**用
（`probe.ReqID >= f.beforeReqID` 跳过）。同 ts 行在文件里的物理顺序与 `req_id` 大小无关
（生产快照 **2,266 组同 ts / 额外 2,497 行，其中 1,185 组（52%）行序非 req_id 升序**），
页边界落在组内时，「尚未返回但 req_id 更大」的行会被跳过。

**修法**：`before_req_id` 改当**扫描位置游标**——`ts == before` 的整组候选行先缓冲进 `tsBuf`，
组结束（遇到更早 ts 的行）时在组内定位游标行：游标行**之后**的行才输出（之前的上页已返回）。
组内不重排，严格保持扫描序。

**为什么不用「按 req_id 降序排序后切页」**：那会改变 `/api/call-log` 对同 ts 行的输出顺序，
直接破坏首轮验收已通过的**跨版本红线**——「生产全量数据 L1 旧实现 vs 新实现逐字节相同」。
位置游标在修复 D2 的同时保持了这个顺序：修复后 `ZZ_MODE=dump` 的 prod L1 md5 仍是
`f93ef566773ed05dbcf955018d52ffa6`（与旧实现、与首轮验收报告一致，见 §12.4）。

**兼容回退（第一版修法踩过的坑）**：纯位置游标只认「真实存在」的游标行，会打破客户端传
**合成游标**时的旧语义（旧实现按排序键返回 `req_id < 游标` 的同 ts 行）。首轮验收的
`TestZZIP4VHistoryFilters`（`before=<ts>&before_req_id=zzz` 期望 4 条）在第一次全量回归里因此变红。
最终实现：组内找到游标行 → 位置语义；找不到 → 退回旧排序键语义。并用
`TestBeforeReqIDUnknownCursorFallsBackToOrderingKey` 把两条语义都锁死。

**修前红 / 修后绿**：
```
# 修前红：把位置游标还原成 req_id 字符串过滤
$ go test -overlay=/tmp/t56/zz/mut/overlay_d2.json -run 'TestSameTSGroupPaginationNoLoss' -v ./
    limit=1 分页并集有缺/重/乱序:
    got=c1,u-1100,u-1030,a2,a1,u-0930,b3,b1,u-0800,u-0730
    want=c1,c3,c4,c2,u-1100,u-1030,a2,a4,a1,a3,u-0930,b3,b1,b4,b2,u-0800,u-0730
--- FAIL: TestSameTSGroupPaginationNoLoss
# 修后绿（limit=1/2/3/4/5/7/11/17 全绿；同 ts 组跨日文件、single vs daily 布局顺序一致）
$ go test -run 'TestSameTS|TestBeforeReqID|TestCallLogHistory|TestZZIP4VHistory' -v ./
--- PASS: TestSameTSGroupPaginationNoLoss (8 subtests)
--- PASS: TestSameTSHistoryOrderIdenticalAcrossLayouts
--- PASS: TestBeforeReqIDUnknownCursorFallsBackToOrderingKey
--- PASS: TestZZIP4VHistoryFilters            # 修复前因兼容回退缺失曾 FAIL
--- PASS: TestZZIP4VHistoryPaginationUniqueTS
--- PASS: TestZZIP4VDefectPaginationDupTS
```
新增测试与夹具（`calllog_defects_test.go`，约 390 行）：
- `t56SameTSFixture`：17 行 / 两组 4 条同 ts（组内 req_id 与行序无关）+ **同 ts 组跨两个日文件**
  （23:59 记录一半写在次日文件，模拟跨午夜 append）+ 若干唯一 ts。期望序 = ts 降序、同 ts 保持反向 append 序。
- `TestSameTSGroupPaginationNoLoss`：8 个 limit × 逐页连翻，断言并集无缺/无重/顺序正确。
- `TestSameTSHistoryOrderIdenticalAcrossLayouts`：同一份字节在 daily（两段）/ single（一段）下分页序列一致。
- `TestBeforeReqIDUnknownCursorFallsBackToOrderingKey`：合成游标回退语义 + 位置游标语义双锁。

### 12.3 门禁加固（D1/D2 当初为什么漏网）

1. **T8 硬门禁升级**（`calllog_invariance_test.go`）：
   - 原来只比 `## /api/stats?range=` 四窗口，**不比 trends** → D1 漏网。
   - 现在三布局逐字节比较：4 窗口 stats + **4 窗口 × 2 口径 trends**（plain / group=model）+
     **call-log 历史查询**（前 500 条，夹具里含 2,266 组同 ts，覆盖 D2 排序/分页稳定性）；
     并断言 trends 非空、today 窗口非全 0。
   - 新增 `T56_REQUIRE_FIXTURE=1`：夹具缺失时 **Fatal 而不是 Skip**，防止 CI 门禁被静默跳过。
     CI/发布前必须显式跑：
     `T56_DAY_DIR=/tmp/t56/day T56_REQUIRE_FIXTURE=1 go test -run TestT56FourWindowInvariance -count=1 -v ./`
2. **同 ts 夹具**：`t56HistoryFixture` 注释原写「时间严格递增」——它规避同 ts 场景，是 D2 漏网原因之一，
   已在注释里标注盲区；同 ts 场景由 §12.2 的新夹具覆盖。
3. **变异测试**：把两条缺陷本体加入变异清单：
   - `M13 [D1]`：把 `callLogFileExists` 的 daily 分支改回「只看 legacy」→ `TestDailyOnlyDeploymentTrendsNotEmpty` FAIL
   - `M14 [D2]`：把位置游标改回 `req_id` 字符串过滤 → `TestSameTSGroupPaginationNoLoss` FAIL
   - 修复后重跑：**14/14 全部被抓**（`mutation_after_fix.txt`）。

### 12.4 重跑结果（修复后，原始命令）

**(a) `ZZ_MODE=dump` 三布局对拍（含 trends + call-log，prod/syn 各 L1/L2/L3）**
```
prod: L1=L2=L3 = f93ef566773ed05dbcf955018d52ffa6   （与 HEAD 旧实现 L1 逐字节相同）
syn : L1=L2=L3 = d9a4336c633af679090e745611e61103   （与 HEAD 旧实现 L1 逐字节相同）
```
修复前 prod L3 是 `328548ccab862f12253f60f636504d75`（与 L1 差 4 行 trends）；现在**三布局全相等**，
且新实现 L1 仍与旧实现逐字节相同（D2 的位置游标没有改输出顺序）。

**(b) T8 硬门禁（含 trends + call-log）**
```
L1 单文件: 124,520,431 B 冷启动 634.9 ms absorbed=233,444 桶=1,089
L3 纯日文件:            冷启动 677.4 ms absorbed=233,444 桶=1,089
L2 legacy+最后一天:      冷启动 560.0 ms absorbed=233,444
trends[today/plain] 三布局逐字节相等（3,547 B）
trends[today/group=model] 三布局逐字节相等（41,445 B）
trends[7d/plain] 1,156 B   trends[7d/group=model] 17,891 B
trends[30d/plain] 4,971 B  trends[30d/group=model] 90,794 B
trends[180d/plain] 26,266 B trends[180d/group=model] 893,455 B
trends 非空 + 三布局逐字节相等: PASS
call-log 历史查询三布局逐字节相等（216,931 B）
守恒通过：ΣN=233,444 == absorbed 233,444 − unabsorbed 0
T8 PASS
```

**(c) 全量回归 / race / vet / gofmt**
```
$ go vet ./                                   # PASS（无输出）
$ gofmt -l calllog.go main.go calllog_*_test.go   # 无输出
$ go test -count=1 -timeout 45m ./            # ok  github.com/shiyanYX/opencode2api  230.316s
$ T56_DAY_DIR=... T56_REQUIRE_FIXTURE=1 go test -race -count=1 -timeout 15m \
    -run 'TestDaily|TestCallLog|TestRetention|TestLayout|TestSingleFileModeKeeps|TestAppend|TestAbsorb|TestBackfill|TestBucket|TestConservation|TestClear|TestUnmapped|TestT39|TestT56|TestSameTS|TestBeforeReqID|TestZZIP4V' ./
                                             # ok  70.924s（0 DATA RACE）
```
（全量 `-race` 的唯一失败点仍是 §8 记录的**既有** `proxy_node.go`/`zz_t30` 竞争，未变。）

**(d) 变异测试**：`14/14 CAUGHT`（M1–M12 原有 + M13/M14 缺陷本体）。

### 12.5 改动 / 哈希 / 边界

修复后：
```
$ git diff --stat -- calllog.go main.go statscan.go
 calllog.go | 1226 +++++++++++++++++++++++++++++++++++++++++++++++++++++--- 
 main.go    |    8 +
（statscan.go 未改；修复只动 calllog.go + 新增测试 + 本报告）

$ sha256sum calllog.go main.go statscan.go calllog_defects_test.go
4b6433265f397b7e3278a8583e0b9b974dc0ed22d4fb244d0c39b02880c64691  calllog.go
4394576f81b3b83cca65ee569da678a53cac58fb7f5f7c9593d7367f7b57d124  main.go   （未变）
47a066beabad1bea8653a6946f24880e118d892ecc63ff28de324b1b694201cb  statscan.go（未变）
42ce71f9c594ddaa388f65d1ecfdd2f3eac5159ae3d060a127f503bc94857f40  calllog_defects_test.go
```
新增测试 5 个（含子测试 8 个），累计新增测试 25 个（原 21 + 5，`t56HistoryFixture` 不变）。
测试文件仍被仓库 `.gitignore:26` 忽略（`*_test.go`），提交时需 `git add -f`（本任务按要求不执行写 git）。

边界遵守不变：未碰 `proxy_node.go` / `node_alert.go` / `statscan.go` / `static/admin.html` / `ROADMAP`；
未碰生产容器、未用 8000、未写 git。验收方的 `zz_ip4v_*` 探针文件未被修改（原样用于复跑）。

### 12.6 D2 残留风险（如实记录，不阻塞）

- 位置游标依赖「游标行仍在磁盘上」：若两次分页之间该行所在段被保留策略删除/归档（180 天前的段），
  组内找不到游标行会退回旧排序键语义，同 ts 组的个别行可能漏/重。分页主要发生在最近数据上，
  180 天边界的同 ts 组极罕见；彻底消除需要不依赖行内容的扫描偏移游标（契约变更）。
- 分页期间若有新 append：新行进入最新段并落在 `ts > before` 区间，被 `ts.After(before)` 过滤，
  不影响旧页（与原实现一致）。
- **上述残留风险在 task-65（§13）已用「位置游标」彻底消除**：新 `?cursor=` 客户端遇到游标段被删
  得到明确 400（不静默漏页），旧头回传方则优雅回退 before 语义。

## 13. task-65：D3 修复 —— 分页改「位置游标」（生产真实序下 0 漏 0 重）

第二轮复验（[`log-retention-verification.md`](log-retention-verification.md) §13）判定 D1/D2/门禁/变异/首轮红线全部通过，
但新发现 **D3 阻塞合并**：D2 只修了「同 ts」子类，**不同 ts 的乱序**仍会让分页漏行 + 重复。
本轮按验收方推荐的方案 A（位置游标）修复，并把分页纳入门禁。

### 13.1 D3 根因（与 D2 不同类）

文件按**请求完成序**追加，而 `CallRecord.TS` 记的是**请求开始时刻**。流式长请求先开始、后结束 ⇒
相邻两行的 ts 经常逆序。验收方在生产全量（233,654 行）实测：

- 相邻 ts 逆序 **68,381 次 = 29.3%**（中位 15.7s、p90 91.5s、p99 377s、**max 46 分钟**）；
- 同 ts 组 2,266 组里 **1,141 组（50%）在文件里不连续**；
- 用「ts 做游标」分页：生产全量 limit=500 共 468 页 ⇒ **漏 1,124 行 / 重 1,317 行**；同一批行按 ts 升序重排后 0 漏 0 重。

机制：倒序扫描时下一页跳过 `ts > before` 的行，但**扫描序里游标之后仍可能出现 ts 更大的行** ⇒ 永久跳过（漏）；
反过来 `ts < before` 但物理位置在游标之前的行会在下一页再吐一次（重）。**与 ts 是否有序无关才是解法的前提。**

### 13.2 修法：位置游标（方案 A）

**契约**（`calllog.go`）：

| 项 | 值 |
| --- | --- |
| 响应头（hasMore 时） | `X-Call-Log-Next-Cursor` = `c1_` + base64url(`段标识:行号`)，行号 0 基、= 本页最后返回行的**物理行号** |
| 段标识 | `legacy` 或 `YYYY-MM-DD`（**不用文件路径**：明文 `.jsonl` → 归档 `.jsonl.gz` 后游标仍有效） |
| 请求参数 | `?cursor=<token>`：从该行的**下一条物理行**续扫（倒序 ⇒ 行号 -1），后续页不再从最新段重扫前缀 |
| 旧参数 | `before` / `before_req_id` **语义不变**（无 cursor 时行为与之前完全一致，旧调用方零影响） |
| 兼容旧头回传方 | `X-Call-Log-Next-Before-Req` 现在返回**不透明 token**（不再是裸 req_id）；客户端原样回传给 `before_req_id` 即自动识别为位置游标精确续页。手工传裸 req_id 仍走旧排序键语义 |
| 同时带 cursor + before | cursor 优先，before 忽略（位置续扫 + 旧 ts 跳过会互相打架） |
| 非法 token | **400** + `X-Call-Log-Error: bad-cursor`（body `{"error":"bad-cursor"}`） |
| 失效 token | 显式 `?cursor=` ⇒ **400** + `X-Call-Log-Error: stale-cursor`（段被保留策略删除 / 行号越界；由客户端从头重分页，不静默给残缺页）。旧头回传方无法处理 400 ⇒ **优雅回退 before 语义**（排他 ts）并回 `X-Call-Log-Cursor-Fallback: before` |
| 无参数路径 | 仍是内存环（旧行为逐字节不变）；要分页请带任一过滤参数（如 `status=all`）进入文件路径 |
| 页成本 | 从游标行续扫，**不再 O(页数×前缀) 重扫**；单文件 legacy 布局每页仍需读整个文件建立行号索引（与旧实现同量级，非回归） |

正确性不依赖 ts 顺序：扫描顺序 = 全局「物理行倒序」，游标就是该顺序上的位置；limit 任意切分，
后续页从切点下一条物理行继续 ⇒ **并集==全集、无重复、顺序稳定**。顺带消除了复验方判「可接受」的
残留风险（游标行被删导致的静默漏/重）：新客户端得到明确 400。

### 13.3 修前红 / 修后绿（原始输出）

**修前红**（先写测试 + 先落 API 骨架，行为未实现；这是本轮实现前的真实运行结果）：
```
--- FAIL: TestProdOrderPaginationCursorUnionEqualsFull
    calllog_prodorder_test.go:211: Has-More=true 但缺 X-Call-Log-Next-Cursor（位置游标未实现）
--- FAIL: TestProdOrderPaginationLegacyHeadersUnionEqualsFull
    calllog_prodorder_test.go:228: 旧 Next-Before 头 limit=3: 并集 65 条，期望 400 条（漏/重见下）
--- FAIL: TestHistoryCursorInvalidAndStale
    calllog_prodorder_test.go:248: ?cursor=not-base64 期望 400，实际 200（body="[]"）
--- FAIL: TestProdOrderCursorSameAcrossLayouts
    calllog_prodorder_test.go:278: Has-More=true 但缺 X-Call-Log-Next-Cursor（位置游标未实现）
```
其中「旧头 limit=3：并集 65/400」就是 D3 在合成生产序夹具上的复现（漏 335 条）。

**修后绿**（`go test -run 'TestProdOrder|TestHistoryCursor|TestLegacyHeader|TestCursorSurvives' -count=1 -v ./`）：
```
--- PASS: TestProdOrderPaginationCursorUnionEqualsFull      # limit=1/2/3/5/7/13/50/400 并集==全集、无重、顺序稳定
--- PASS: TestProdOrderPaginationLegacyHeadersUnionEqualsFull
--- PASS: TestHistoryCursorInvalidAndStale                 # bad-cursor / stale-cursor 契约
--- PASS: TestProdOrderCursorSameAcrossLayouts             # single 两段 vs 单段分页序列逐条一致
--- PASS: TestLegacyHeaderStaleCursorFallsBackToBefore     # 旧头回退 before + Cursor-Fallback 标记
--- PASS: TestCursorSurvivesGzipRename                     # 游标段明文→gz 改名后续页不漏不重
```

**生产真实字节（验收方探针 `TestZZIP4VDefectProdSlicePagination`，未改动一行）**：
```
生产尾部切片 10000 行（limit=500）：分页 20 页，漏 0 行，重复 0 行，唯一 req_id 10000
生产尾部切片 30000 行（limit=200）：分页 150 页，漏 0 行，重复 0 行，唯一 req_id 30000
单调控制组：同一批行按 ts 升序写盘后分页 0 漏 0 重
--- PASS: TestZZIP4VDefectProdSlicePagination
```
对照复验报告 §13.7.3 的同数据同参数：1 万行 limit=500 原为 **漏 7 / 重 39**，3 万行 limit=200 原为 **漏 117 / 重 250**。

### 13.4 门禁补齐（D3 抓不到的根因）

1. **T8 硬门禁新增分页判据**（`calllog_invariance_test.go`，用生产快照 233,444 行，与 stats/trends/call-log 同一份夹具）：
   ```
   分页门禁：位置游标 limit=500 全量连翻 467 页 / 233444 条，0 漏 0 重；三布局前 3 页顺序一致
   T8 PASS（34.9 s）
   ```
   同时仍比较 4 窗口 stats + 4 窗口×2 口径 trends + call-log 历史三布局逐字节相等（§12.4 的牙一个不少）。
2. **常驻 CI 测试（不依赖 /tmp 夹具）** `calllog_prodorder_test.go`：
   - 夹具**按完成序追加、ts=开始时刻**（生产同构，测试内断言相邻逆序占比 ≥10%）；
   - `limit ∈ {1,2,3,5,7,13,50,400}` 全枚举连翻：并集==全集、无重复、顺序与单页全集逐条一致；
   - 旧调用方路径（只跟随 `Next-Before`/`Next-Before-Req` 头）同样全绿；
   - single（1 段）vs daily（2 段）分页序列一致；游标段 gz 改名后仍有效；
   - badge/stale/回退语义测试。
3. **变异测试**新增 3 个（累计 17 个，`17/17 CAUGHT`）：
   | 变异 | 说明 | 变红的测试 |
   | --- | --- | --- |
   | **M15** | 位置游标 off-by-one（游标行重复返回） | `TestProdOrderPaginationCursorUnionEqualsFull` |
   | **M16** | 段标识改用文件路径（明文→gz 改名后游标失效） | `TestCursorSurvivesGzipRename` |
   | **M17** | 位置游标不产出（Next-Cursor 缺失） | `TestProdOrderPaginationCursorUnionEqualsFull` |
   （原有 M13=D1、M14=D2 缺陷本体仍在，17 个全抓。）

### 13.5 复跑结果（最终代码）

**(a) `ZZ_MODE=dump` 同钟对拍（新 L1/L2/L3 与 HEAD 旧实现 L1 背靠背生成）**
```
prod: new L1=L2=L3 = 990b3117490950ce576e979679e2cc95
      old L1       = 990b3117490950ce576e979679e2cc95     ⇒ old L1 == new L1/L2/L3 ✅
syn : new L1=L2=L3 = c71d2246fe648c7fecb513af8a41a3e6
      old L1       = c71d2246fe648c7fecb513af8a41a3e6     ⇒ old L1 == new L1/L2/L3 ✅
旧实现 L2/L3 = 65954562… / 62027258…（≠ L1）：修复前缺陷形态（L3 trends 空），现三布局全等。
```
（跨进程 dump 必须同钟：窗口是相对 now 的滚动窗口，隔小时比会因窗口滑动产生字节差异，不是代码差异。）

**(b) 全量回归 / race / vet**
```
$ go vet ./ && gofmt -l calllog.go main.go calllog_*_test.go   # 无输出
$ go test -count=1 -timeout 45m ./                             # ok 226.507s
                                                               # 最终代码+测试再复跑一次：ok 188.477s
$ T56_DAY_DIR=/tmp/t56/day T56_REQUIRE_FIXTURE=1 go test -race -count=1 -timeout 20m \
    -run 'TestDaily|TestCallLog|…|TestProdOrder|TestHistoryCursor|TestLegacyHeader|TestCursorSurvives|TestZZIP4V' ./
                                                               # ok 317.004s，0 DATA RACE
```
验收方 23 个 `TestZZIP4V*` 探针（含 D2/D3 缺陷探针）在当前实现下 **全部 PASS**。

### 13.6 改动 / 哈希 / 边界

```
$ git diff --numstat -- calllog.go main.go statscan.go
1157  69  calllog.go        （D1/D2/D3 累计）
8     0   main.go           （阶段 1 接线，未变）
（statscan.go 未改）
$ sha256sum calllog.go main.go statscan.go calllog_prodorder_test.go
37153e123ef7a51b1d38ba6b11b9560336316c3a01b3634eaac31ae332630c5c  calllog.go
4394576f81b3b83cca65ee569da678a53cac58fb7f5f7c9593d7367f7b57d124  main.go（未变）
47a066beabad1bea8653a6946f24880e118d892ecc63ff28de324b1b694201cb  statscan.go（未变）
ac9ba70acebe2638248ec1212d792ed67598d2790811426904d66b86056f2872  calllog_prodorder_test.go
```
本轮新增 6 个测试（`calllog_prodorder_test.go`，约 400 行）+ T8 分页门禁。
边界不变：只动 `calllog.go` + 新测试 + 本报告；未碰 6 个保护文件、未碰验收方 `zz_ip4v_*` 探针、
未碰生产容器/8000、未写 git。

### 13.7 残留风险（如实记录，不阻塞）

1. **旧头回退路径的静默降级**：只回传 `Next-Before-Req` 的旧调用方，若游标段在两页之间被 180 天策略删除，
   会回退 before 语义——同 ts 组跨段的极端形态仍可能漏 1 条（复验 §13.8 形态 B 原样保留，已用
   `X-Call-Log-Cursor-Fallback: before` 标记）。新 `?cursor=` 调用方不会静默：明确 400 stale-cursor。
   生产数据里同 ts 组跨日文件 0 例，触发需 180 天边界的 400+ 页。
2. **分页入口**：无任何过滤参数时仍走内存环（旧行为红线），此时不返回位置游标；面板接分页时应带
   `status=all`（或任意过滤参数）拿第一页。
3. **单文件 legacy 布局**每页仍需整文件读取来定位行号（旧实现同样如此，非本轮回归）；daily 布局每页只读 1~2 个 2MB 段。
