# task-62 独立验收报告：调用日志分文件 + 保留策略 + 历史查询 + 字段精简

> 验收人：verify-c5v2（task-61）｜实现者：capture-429｜范围：`calllog.go`(+1083/−66)、`main.go`(+8)
> 本报告**增量落盘**：每完成一项就更新，最后给出 go / no-go 结论。
> **rev2（task-63，2026-10-20 21:0x）**：D1/D2 修复已由原验收方复验，结论见 **§13**（D1 ✅、D2 同 ts ✅、门禁加固 ✅、变异 9/9 ✅，但复验**新发现 D3**：真实数据 ts 乱序下分页仍漏/重）。
> **rev3（task-65，2026-10-20 22:5x）**：D3 位置游标修复复验，结论见 **§14** —— **go**（全量 233,654 行 468 页 0 漏 0 重；错误契约/头语义/门禁破坏实验/13 变异全过；残留风险 3 条均判可接受）。
> 所有数字都来自本机真实命令输出，不采信实现者自测与 `.research/t56/` 的证据文件。

## 0. 结论速览（滚动更新）

| # | 验收项 | 结果 | 证据 |
| --- | --- | --- | --- |
| 1 | 四窗口逐字节不变：L1/L2/L3 | ⚠️ **L1=L2 ✅，L3 有缺陷** | §2.1 / D1 |
| 1b | 四窗口逐字节不变：新旧实现 | ✅ **生产全量 233,654 行逐字节相同** | §2.1 |
| 2 | append-only 兼容（精简→还原语义等价） | ✅ 仅 `final_status`/`dial_ms` 丢失（已批准的零读者字段） | §3 |
| 2b | `events[].node_name` 还原 | ✅ 生产 233,783 个 node_name 事件 **0 例不可还原**；合成错位场景会丢（残留风险） | §3.2 |
| 3 | 历史查询：无参数路径 | ✅ 内存环、最新在前、无文件头 | §4 |
| 3b | 历史查询：过滤 + 唯一 ts 分页 | ✅ 不重不漏 | §4.1 |
| 3c | 历史查询：**同 ts 记录分页** | ❌ **会漏记录（实测生产 2,266 组同 ts）** | D2 |
| 4 | 保留策略：未 seeded 不动手 / 归档删除后四窗口不变 / legacy 不动 / 并发不丢行 | ✅ | §5 |
| 5 | 回滚开关 `-calllog-layout=single` | ✅ 无日目录、旧格式落盘、保留策略 no-op | §6 |
| 6 | ≥5 个独立变异 | ✅ **7 个变异全部被我的测试抓住** | §7 |
| 7 | 范围审计（只动 calllog.go/main.go） | ✅ | §1 |
| 8 | 测试注水审计 | ✅ 无空断言/未改既有测试；有覆盖盲区 + 硬门禁默认 skip | §8 |
| 9 | 性能 / 磁盘投影 | ✅ 冷启动 635~704ms；90 天 141.1MB / 180 天 154.9MB | §9 |
| 10 | `-race`：本任务子系统干净 + 复核既有 race | ✅ 子系统 0 race；既有 race 在 HEAD 干净副本复现 | §10 |

**结论：no-go**。硬门禁「L1/L2/L3 四窗口逐字节一致」在 L3 下不成立（D1，`/api/stats/trends` 恒空）；
新增历史查询 API 的分页在同 ts 多记录时会漏数据（D2，生产日志真实可达）。两处最小修法见 §11。
除这两点外，本次改动质量很高：生产全量 233,654 行新旧实现四窗口逐字节相同、保留策略/回滚/字段精简/
并发安全均通过独立复算，全量 `go test` 绿。

## 1. 范围与哈希审计

生产容器只读核对（`docker exec opencode2api`）：

| 项 | 值 |
| --- | --- |
| `/data/call_log.jsonl` | 124,617,389 B / **233,654 行** / md5 `efa3f23b9faf28fcee5f41fcdfd20bcb` |
| 本机 `sha256sum calllog.go` | `27e6c185f24e106d6717adb395e0162abf98272474413842bc1fadfe42057a33`（与实现者声明一致） |
| 本机 `sha256sum main.go` | `4394576f81b3b83cca65ee569da678a53cac58fb7f5f7c9593d7367f7b57d124`（一致） |
| `git diff --stat` | `calllog.go` 1149 行(+1083/−66)、`main.go` 8 行(+8)、`static/admin.html` 411 行（**他人任务**，见下） |
| `main.go` 改动内容 | 3 个 flag：`-calllog-layout=daily|single`、`-calllog-gzip-after-days=2`、`-calllog-retain-days=180`，加 `startCallLogArchiver()` |
| 未改动（`git diff` 无记录，逐字节核对 HEAD） | `proxy_node.go`、`node_alert.go`、`statscan.go`、`ROADMAP.md` |
| 工作树其它改动 | `docs/mockups/u2/f7_stub.py`、`docs/research/F7-banner-refresh-verification.md`、`static/admin.html` 属其它任务，非本次改动 |

验收夹具（冻结后 md5/行数固定）：
- `/tmp/ip4v/fixtures/prod_call_log.jsonl`：生产 233,654 行原样（md5 同上）。
- `/tmp/ip4v/fix/{prod,syn}/{L1,L2,L3}`：把同一份字节序列拆成三种布局，**拼接后 md5 与原文件相同**（prod `efa3f23b…`、syn `1c8d1f97…`），保证「差异只来自布局」。
- HEAD 干净参照树 `/tmp/ip4v/ref/tree`：工作树全量拷贝，`calllog.go`/`main.go` 用 `git show HEAD:` 覆盖，并移除实现者新增的 3 个测试文件。

## 2. 硬门禁：四窗口逐字节不变

对拍探针：`zz_ip4v_shared_test.go`（只用两版本都存在的符号，可原样拷进 HEAD 参照树）。
覆盖 `/api/stats?range={today,7d,30d,180d}`、`/api/stats/trends?range=…[&group=model]`、`/api/stats`（无 range 兼容红线）、`/api/call-log[?limit=100]`。

### 2.1 结果（真实输出）

| 对拍 | md5 | 结论 |
| --- | --- | --- |
| 生产 L1 旧实现 | `f93ef566773ed05dbcf955018d52ffa6` | — |
| 生产 L1 新实现 | `f93ef566773ed05dbcf955018d52ffa6` | **逐字节相同 ✅** |
| 生产 L2 新实现 | `f93ef566773ed05dbcf955018d52ffa6` | **L1==L2 ✅** |
| 生产 L3 新实现 | `328548ccab862f12253f60f636504d75` | ❌ 与 L1 差 4 行（**D1**） |
| 合成 L1 旧 vs 新 | `d9a4336c633af679090e745611e61103` | **逐字节相同 ✅** |
| 合成 L1/L2/L3 新 | L1=L2=`14ebeb44…`、L3≠ | L1==L2 ✅，L3 同 D1 |

进程内复跑（同一时钟，`TestZZIP4VLayoutsStatsByteEqual`）：合成夹具 L1/L2/L3 四窗口 JSON 逐字节一致 ✅（0.06s）。
冷启动耗时（`go test` 内计时，含尾部装载+全量吸收+四窗口）：L1 0.518s / L2 0.525s / L3 0.533s；旧实现 L1 0.825s。

### D1（缺陷）：纯日文件布局下 `/api/stats/trends` 恒为空

- 复现：目录里只有 `call_log/2026-10-20.jsonl`、没有 `call_log.jsonl` → `/api/stats?range=today` 返回 2 条请求，但 `/api/stats/trends?range=today` 返回 `[]`。
- 根因：`callLogFileExists()`（calllog.go:1469）只 `os.Stat` legacy 的 `call_log.jsonl`；daily 模式下 `trendsFromCallLog()` 走 `if !callLogFileExists() { return nil }`（calllog.go:2822），handler 把 nil 变 `[]`。
- 影响面：L3 布局（legacy 被删/迁移）与**全新部署**（新数据目录用 daily 布局时 legacy 永远不会被创建）→ 面板热力图、`/api/stats/trends` 永远空白；而同一份数据的 `/api/stats?range=` 完全正常。
- 现有测试为何没发现：见 §8 测试注水审计。

## 3. append-only 兼容与字段还原

探针 `TestZZIP4VAppendOnlyCompat`：同一批记录分别用 single（旧格式）与 daily（精简格式）落盘 → 模拟重启 → 比 `/api/call-log`。

| 断言 | 结果 |
| --- | --- |
| daily 落盘确实更小、且不含 `final_status`/`dial_ms`/`node_name` | ✅ |
| 重启后记录条数相同、**除 `final_status`/`dial_ms` 外逐字段相等** | ✅ |
| single 落盘仍是旧格式（`final_status:500` 等全在） | ✅ |

### 3.2 node_name 还原

- 生产全量独立复算：233,783 个 `events[].node_name` 非空事件，按 `nodes`/`node_names` 索引还原**失败 0 例**（实现者声明 233,750/233,750，随日志增长，结论一致）。
- 合成反例：`events[].node_name` 存在但 `node` 不在顶层 `nodes` 里（或 `node_names` 缺该下标）时，精简落盘后 `node_name` **永久丢失**（single 模式不丢）。生产当前 0 例，属**残留风险**，建议将来在 `diskRecordForWrite` 里对不可还原的 node_name 不省略。
- 另：旧行若 `path`/`route_mode` 为空（生产 0 例），新实现会"还原"成默认值（`/v1/chat/completions`/`public`），即「省略」与「显式空」不可区分（`TestZZIP4VOldLineOmittedDefaultsFabricated` 记录现状）。

## 4. 历史查询 API

- 无参数：`X-Call-Log-Source: memory`、最新在前、默认 500 条，无文件查询头 ✅（`TestZZIP4VCallLogNoParamIsMemoryPath`）。
- 过滤：`model`/`status=ok|fail|all`/`q`（大小写不敏感命中原始行）/`after`/`before` 全部符合预期；非法 `status`/时间参数 400 ✅（`TestZZIP4VHistoryFilters`）。
- 唯一 ts 分页：limit=1/5/37/100 连翻，并集 == 全集、无重复 ✅（`TestZZIP4VHistoryPaginationUniqueTS`）。

### D2（缺陷）：同 ts 多记录的 (ts,req_id) 游标会漏记录

- 复现：14 条记录、2 组各 4 条同 ts（req_id 与行序无关），limit=3 连翻 → **漏 3 条**（`d1-b d1-z d2-m`）。
- 根因：`historyQuery` 扫描顺序是**文件倒序**，而游标按 `(ts desc, req_id desc)` 的字符串序过滤（`probe.ReqID >= f.beforeReqID` 跳过）。同 ts 行在文件中的先后与 req_id 大小无关，于是「尚未返回但 req_id 较大」的行被跳过。
- 生产影响：生产日志真实存在同 ts 多记录 —— **2,266 组 / 2,497 额外行**，其中 **1,185 组（52%）行序不是 req_id 升序**。只要页边界落在这类组内（limit 越小越容易），分页就会漏数据。
- 最小修法：同 ts 组整组读完再切页（先收集 ts==before 的全部匹配行，按 req_id 降序排序后接续），或改用「扫描位置游标」而非 (ts,req_id)。

## 5. 保留策略（gzip 归档 + 180 天删除）

`TestZZIP4VRetention`：legacy(300 天前) + 200 天 ~ 今天每天 2 条日文件。

| 断言 | 结果 |
| --- | --- |
| 未 seeded（刚启动、桶空）时 `maintainRetention` 不压缩/不删除 | ✅ |
| 全量吸收后：>180 天日文件删除、2 天前~180 天 gzip、今天仍明文 | ✅ |
| legacy 文件内容逐字节不变 | ✅ |
| 180 天前的分段保留（边界 `>=` cutoff） | ✅ |
| 归档+删除后**模拟重启，四窗口逐字节不变** | ✅ |

`TestZZIP4VRetentionConcurrentAppend`：120 条并发 append 期间持续跑维护 → 磁盘行数不少、重启后 `checkBucketConservation` 守恒 ✅。

## 6. 回滚开关 `-calllog-layout=single`

`TestZZIP4VRollbackSingle`：不建 `call_log/`、全部写 legacy、落盘含 `path`/`route_mode`/`final_status`/`node_name`（旧格式）、`maintainRetention` no-op、无参数仍走内存环 ✅。跨版本落盘字节对拍见 §7（变异基线）。

## 6b. 回滚模式落盘字节与旧实现逐字节相同（跨版本对拍）

`ZZ_MODE=append` 探针在旧/新两棵树上跑同一批 append，比较「落盘文件字节 + 无参数接口响应」：

| 对拍 | md5 | 结论 |
| --- | --- | --- |
| 新实现 single 模式 | `ef3ba3d49ac122ac4ed43272ddd42f84` | — |
| HEAD 旧实现 | `ef3ba3d49ac122ac4ed43272ddd42f84` | **逐字节相同 ✅**（回滚开关的落盘格式确实退回旧版） |

## 7. 独立变异（7/7 全部被我的测试抓住）

变异用 `go test -overlay` 注入（不改工作树），每个变异跑我的正常测试集；
命令与原始输出：`/tmp/ip4v/mutate.py`（可复跑），结果：

| 变异 | 说明 | 变红的验收测试 |
| --- | --- | --- |
| M1 | 分文件读取乱序：日文件按日期**降序**排 | `TestZZIP4VLayoutsStatsByteEqual` |
| M2 | 游标跨段不接续：未 seeded 时也把游标推进到新日文件 | `TestZZIP4VColdAppendThenStats` |
| M3 | 保留期删未 seeded 段：去掉两处 seeded 保护 + 游标保护 | `TestZZIP4VRetention` |
| M4 | 省字段后不还原：`restoreDiskRecord` 直接返回 | `TestZZIP4VAppendOnlyCompat`、`TestZZIP4VOldLineOmittedDefaultsFabricated` |
| M5 | 无参数路径改走文件：`useFile` 恒 true | `TestZZIP4VCallLogNoParamIsMemoryPath`、`TestZZIP4VRollbackSingle` |
| M6 | 分页游标 `>=` 改 `>`（同 ts 边界重复） | `TestZZIP4VHistoryPaginationUniqueTS` |
| M7 | 段内续吸失效：`seek` 恒从 0（重复消费已吸前缀） | `TestZZIP4VAbsorbSegmentResume` |

实现者自报 12 个变异（`.research/t56/evidence/mutation_results.txt`）**全部被其自带测试抓住**，但这份清单
不覆盖：① 无 legacy（L3/全新部署）布局下 `/api/stats/trends` 的出口判空（D1）；② 同 ts 多记录的分页
完整性（D2）；③ 保留策略执行后「重启仍四窗口不变」的硬门禁（其不变性测试默认 skip）。

## 8. 测试注水审计（21 个新测试）

| 检查项 | 结果 |
| --- | --- |
| 测试函数数 | `calllog_daily_test.go` 9 + `calllog_invariance_test.go` 1 + `calllog_retention_test.go` 11 = **21**，与实现者声明一致 |
| 「只断言不 panic」的空断言 | **0 个**（逐个函数扫描：全部含 `t.Fatal*/t.Error*`） |
| 断言密度 | 112 处 `t.Fatalf`（daily 40 / invariance 22 / retention 50），非空壳 |
| 是否改动既有测试来放水 | **未改动**：除这 3 个新文件外，今天 19:00 后无其它 `*_test.go` 被修改（`find -newermt` 列表；19:19 的 `zz_verify_t60_*` 属另一任务） |
| 默认即 skip 的硬门禁 | **有 1 处**：`TestT56FourWindowInvariance` 仅在 `T56_DAY_DIR` 设置时运行，常规 `go test ./` 里是 SKIP —— 即「四窗口不变」这条硬门禁**不在 CI 默认执行** |
| 覆盖盲区（直接导致 D1/D2 漏网） | ① 该 invariance 测试只比 `/api/stats?range=` 响应，**不含** `/api/stats/trends` 与 `/api/call-log`；其 L3 夹具故意把 legacy 指向不存在的路径，stats 路径能过，但没触发 trends 的判空分支 → D1 漏网。② `t56HistoryFixture` 注释写明「时间严格递增」，夹具**没有同 ts 多记录** → D2 漏网 |
| 断言强度反证 | 我的 M6 变异（游标 `>=`→`>`）会被其 `TestCallLogHistoryPaginationNoGapsNoDupes` 的精确并集断言抓住；但其夹具无同 ts，D2 类缺陷无法被发现 |

结论：**没有发现刻意放水/删除既有断言**；问题是覆盖盲区 + 硬门禁默认跳过。

## 9. 性能 / 磁盘（独立复算，`ZZ_PERF=1`，生产 233,654 行夹具）

| 指标 | L1 单文件 | L2 legacy+最后一天 | L3 纯日文件 | 实现者自测 |
| --- | --- | --- | --- | --- |
| 启动尾部装载 | 23ms | 24ms | 27ms | — |
| 冷启动全量吸收 + 四窗口 | **635ms** | **704ms** | **670ms** | 574.6 / 485.9 / 580.7ms |

（同数量级、同结论：三布局冷启动都在 0.6~0.7s，均优于旧实现 L1 的 0.825s；差异来自夹具/cache 状态，不构成回归。）

磁盘投影（独立实现：把生产 233,654 行逐条经 `diskRecordForWrite` 转精简格式 → 按天明文/gzip 实测）：

- legacy 冻结 = 124.6 MB；精简后日均明文 = 1,439,128 B，日均 gzip = 154,138 B（压缩比 ≈ 9.34:1）。
- **投影 90 天 = 141.1 MB，180 天 = 154.9 MB** vs 实现者声明 143.7 / 156.0 MB —— 吻合（差 <2%，我的模型更保守：留 2 天明文）。
- 反例提示：`callLogHistoryMaxScanBytes=256MB` 的注释说「180 天全量 ≈ 276 MB」，与实测投影 154.9MB 有出入（276MB 是按**未压缩**明文推算的），但该预算只影响最坏全量扫描的保护阈值，不影响正确性。

## 10. `-race`

| 检查 | 命令 | 结果 |
| --- | --- | --- |
| 本任务子系统（我的验收测试，含并发归档/并发 append） | `go test -race -run 'TestZZIP4V' -skip 'TestZZIP4VDefect' -count=1 ./` | **0 个 DATA RACE，ok 11.116s ✅** |
| 实现者声明的既有 race（HEAD 干净副本） | `go test -race -run 'TestT30CriterionCPoolEmptyReachability' ./`（`/tmp/ip4v/ref/tree`） | **复现**：`WARNING: DATA RACE`，read `proxy_node.go:1347 (*nodePool).saveState`（goroutine 1105 `markProbeDead.func1`）←→ write `zz_t30_alert_safety_test.go:274`（goroutine 11） |
| 同 race 在当前工作树 | 同上，工作树 | 复现（同一对栈），证明与 task-56 无关：`proxy_node.go` 本次未改动（§1），测试文件 mtime 11:29（早于本任务 19:0x） |

即：**该 race 是既有问题，不是本次回归**；本次改动的子系统 `-race` 干净。

## 11. 最小修法建议

1. **D1**：`callLogFileExists()`（或 `trendsFromCallLog` 的判空处）在 daily 模式下改为「任意段存在即算存在」——例如复用 `callLog.readSegments()` 判断 `len(segs)>0`；单文件模式保持原语义。一行级改动，不影响 L1/L2。
2. **D2**：`historyQuery` 同 ts 组内改为「整组收集 + 按 req_id 降序排序后切页」，游标继续用 `(ts,req_id)`；或在页边界记录扫描偏移。需补一个「同 ts 多记录连翻」的回归测试（现有测试见 §8 为何没拦住）。

## 12. 复跑与证据

### 12.1 验收人新增文件（未改任何生产代码）

| 文件 | sha256 |
| --- | --- |
| `zz_ip4v_shared_test.go`（跨版本对拍探针，可拷进 HEAD 树） | `fc0a29ca29cde7f681cfa8617ef53750ce04c1312b71b2cd6ca3ff71c1563866` |
| `zz_ip4v_verify_test.go`（本任务独立验收测试，17 个用例） | `14749942c81354461a791387c2f48c89b80542c3e4843d3887a8727c93cdb86c` |
| `zz_ip4v_layout_new.go`（全局复位/布局开关） | `a88d1f8bb2edd87c4ed7cd478b5d69ae57ee14a82ee16644626f91c4f3c4cb28` |
| `docs/research/log-retention-verification.md`（本报告） | —（自引用，略） |

生产代码 sha256 与实现者交付时一致（验证人未改动）：
`calllog.go` = `27e6c185…57a33`，`main.go` = `4394576f…d124`。

### 12.2 关键命令

```bash
# 全量回归（排除缺陷取证测试；缺陷测试名以 TestZZIP4VDefect 开头）
go test -skip 'TestZZIP4VDefect' -count=1 -timeout 45m ./
#   => ok  github.com/shiyanYX/opencode2api  168.401s

# 缺陷取证（预期红）
go test -run 'TestZZIP4VDefect' -count=1 -v ./
#   => TestZZIP4VDefectL3TrendsEmpty / TestZZIP4VDefectPaginationDupTS 均 FAIL（见 §2.2 / §4.2）

# 本任务子系统 -race
go test -race -run 'TestZZIP4V' -skip 'TestZZIP4VDefect' -count=1 ./
#   => ok 11.116s，0 个 DATA RACE

# 四窗口跨布局（生产夹具留档命令，探针走 ZZ_MODE=dump）
ZZ_MODE=dump ZZ_CFG=/tmp/ip4v/fix/prod/L1/config.json ZZ_OUT=…/prod_L1_new.txt ZZ_LAYOUT=daily go test -run TestZZIp4vDump -count=1 ./

# 旧实现参照树（HEAD 干净副本）
rsync -a … ./ /tmp/ip4v/ref/tree/ && cp <HEAD versions> …   # calllog.go/main.go 用 git show HEAD: 覆盖
```

### 12.3 遗留风险（不阻塞，但建议记录）

1. `events[].node_name` 若与顶层 `nodes`/`node_names` 不一致，精简落盘后永久丢失（生产 0 例，测试覆盖了该反例）。建议 `diskRecordForWrite` 只对「可还原」的 node_name 省略。
2. 旧行 `path`/`route_mode` 为空时被「还原」为默认值（生产 0 例）。若将来允许显式空值，需要版本位区分「省略」与「空」。
3. `final_status`/`dial_ms` 在 daily 落盘被丢弃且不还原（已批准，部署面板 `static/admin.html` 实测 0 引用）；但即将上线的 U3 面板 mockup（`docs/mockups/u3/logs-v2.html`）引用了 `finalStatus`，届时 daily 模式记录会显示 0/缺失，需要重新评估。
4. 历史查询每页都从最新段重扫（无索引），生产 233k 行、limit=500 时连翻 400+ 页的 IO 成本是 O(页数 × 前缀字节)；当前面板未启用分页，若 U3 面板接入需评估。
5. task-56 引入全局 `callLog.dir` 后，历史测试中只设置 `callLog.path` 的用例（如 `t39Isolate`）会被前序测试残留的 `dir` 污染（本验收期间我自己的测试曾因此导致全量红，已修正为每个用例 `t.Cleanup` 复位；实现者的新测试本身有正确复位）。生产路径只由 `initCallLog` 同时设置两者，不受影响。

### 12.4 结论

- **no-go**：D1（L3/全新部署 `/api/stats/trends` 恒空）与 D2（同 ts 分页漏记录）必须修（最小修法见 §11），修后建议重跑 §12.2 第 1、2 条命令 + 全量回归。
- 其余验收项全部通过：生产全量数据四窗口逐字节不变、保留/归档/回滚/字段精简/并发/性能/磁盘投影均独立复算通过。

---

# rev2：task-63 复验（D1/D2 修复）

> 复验方：verify-c5v2（原验收方，task-63）｜实现方：capture-429｜基准：`calllog.go` sha256 `4b6433265f397b7e3278a8583e0b9b974dc0ed22d4fb244d0c39b02880c64691`（与其声称一致）
> 原则：**不复用实现方测试/夹具**；全部用自己的 `zz_ip4v_rev2_test.go` 与独立生成的夹具/对拍。

## 13.1 结论（先给裁决）

| 项 | 结论 | 证据 |
| --- | --- | --- |
| D1：L3/全新部署 trends 恒空 | ✅ **已修好**（自造 3 场景 + 门禁破坏实验） | §13.2 / §13.4 |
| D2：同 ts 多记录分页漏行 | ✅ **按要求修好**（跨段同 ts、limit=1..12、并集==全集、无重、顺序==单文件；回退分支语义与首轮一致；未重排） | §13.3 |
| 门禁加固（T8 覆盖 trends/call-log、缺夹具 Fatal） | ✅ **真实加固**；破坏实验证明 teeth 就来自新增的 trends 比较 | §13.4 |
| 自造变异 7 个（首轮）+ 2 个（针对新代码） | ✅ **9/9 CAUGHT** | §13.5 |
| 首轮红线（新旧逐字节、回滚、顺序、四窗口） | ✅ 未回归（同钟对拍 old==new；三布局全等） | §13.6 |
| **新发现 D3：真实数据 ts 乱序下分页漏/重** | ❌ **D2 的根因只修了同 ts 子类，不同 ts 的乱序仍会漏/重**（全量 468 页：漏 1,124 / 重 1,317） | §13.7 |
| 残留风险（游标行所在段两页间被删） | ✅ **可接受**（触发条件极稀有；且位置游标方案可一并解决） | §13.8 |
| 回归/范围/哈希 | ✅ 全量 `ok 198.814s`、子系统 `-race` 0 race、`calllog.go` 哈希与声称一致、只动 calllog.go+main.go | §13.9 |

**裁决建议**：D1/D2 的修复本身验收通过；但 **D2 的根因类别（游标是 ts 序、物理扫描序不是 ts 序）在生产真实数据上仍然漏/重**，按首轮同一验收标准（分页并集==全集）应当 **no-go**。
现实影响面：**当前已部署面板只调 `/api/call-log?limit=500`（无分页）**，所以 D3 现在没有线上影响；U3 面板一旦接分页就会踩到。
⇒ 我的建议：**在本批合并前先修 D3**（新 API 还没有客户端，现在改游标格式代价最低，最小修法见 §13.10）；
若 Lead 裁决「本期不开放分页、面板只拉最近 500 条」，则可放行，但应把 D3 记为 **U3 接入分页前的阻塞项**，并把我的生产切片分页探针接入门禁。

## 13.2 D1 复验（自造三场景，不复用实现方测试）

新文件 `zz_ip4v_rev2_test.go`（独立夹具、独立期望）：

1. **只有日文件、无 legacy**（`TestZZIP4VD1DayOnlyTrendsEqualToSingle`）：
   - `callLogFileExists()` = true；`trendsFromCallLog("today")` 非 nil；
   - trends 4 窗口 × plain/group=model **全部非空**，且与「同一批字节拼成单文件」布局**逐字节相同**。
   ```
   --- PASS: TestZZIP4VD1DayOnlyTrendsEqualToSingle
       D1 纯日文件 vs 单文件：trends 4 窗口 × 2 口径逐字节相同，且非空
   ```
2. **真正全新部署（空目录）首次写入**（`TestZZIP4VD1FreshDeploymentFirstWrite`）：写入前 `callLogFileExists()=false`、trends=nil（正确）；append 3 条后 `callLogFileExists()=true`、trends(today) 合计 `requests=3`、非空，且与单文件布局 trends 逐字节相同。
   ```
   --- PASS: TestZZIP4VD1FreshDeploymentFirstWrite
       D1 全新部署首次写入：trends 非空、requests=3、与单文件布局逐字节相同
   ```
3. **红线未破**（`TestZZIP4VD1SingleFileMissingStillNil`）：single 布局且 legacy 不存在时，`callLogFileExists()=false`、`trendsFromCallLog` 返回 nil、handler 输出 `[]`。
   ```
   --- PASS: TestZZIP4VD1SingleFileMissingStillNil
   ```
另有首轮缺陷探针在修复后转绿（原始输出，见 `/tmp/ip4v/out/defects.log`）：
```
--- PASS: TestZZIP4VDefectL3TrendsEmpty
--- PASS: TestZZIP4VDefectPaginationDupTS
```

## 13.3 D2 复验（同 ts 组、跨日文件边界、limit 全枚举）

自造夹具（`zzDupTSFixture`，文件内 ts 升序 = 真实追加形态；同 ts 组内 req_id 与行序无关；
另有**同 ts 组 sp-a/sp-b 被拆在相邻两个日文件**、扫描时相接）：

- 扫描序（单文件与日文件布局必须一致）：`[g4-a,g4-m,g4-z,g4-b,d2-p,sp-a,sp-b,solo,g1-q,g1-c]`
- `TestZZIP4VD2DupTSPaginationCrossSegment`：**limit=1..12 逐个连翻**，断言并集==全集、无重复、顺序与期望一致；并逐页比较 daily 与 single（`pagesDaily == pagesSingle`）。
  ```
  --- PASS: TestZZIP4VD2DupTSPaginationCrossSegment
      D2 同 ts 跨段分页：limit=1..12 全部并集==全集且顺序正确；daily==single 逐页一致
  ```
- `TestZZIP4VD2FallbackOrderingKey`（合成游标回退分支）：
  - 游标行不存在（`before_req_id=zzz`）⇒ 旧排序键语义：同 ts 组全部（req_id<zzz）+ 更早全部 = 10 条，与首轮 `/zzz` 期望一致；
  - 游标行真实存在（`g4-m`）⇒ 位置语义：只返回它之后 = 8 条 `[g4-z,g4-b,d2-p,sp-a,sp-b,solo,g1-q,g1-c]`——**不退化、不漏行**；
  - daily 与 single 两布局结果相同。
  ```
  --- PASS: TestZZIP4VD2FallbackOrderingKey
      D2 回退分支：合成游标=旧排序键语义（10 条）、真实游标=位置语义（8 条），两布局一致
  ```
- `TestZZIP4VHistoryOrderEqualsMemoryRing`（实现方声称「故意不重排」）：文件侧首页与内存环无参数出口**逐字节相同**，同 ts 组保持文件倒序 `[d-2,d-4,d-1,d-3]`（不是 req_id 排序）。
  ```
  --- PASS: TestZZIP4VHistoryOrderEqualsMemoryRing
  ```
- 首轮回归：`TestZZIP4VHistoryPaginationUniqueTS`、`TestZZIP4VHistoryFilters`（含 `/zzz` 期望 4 条）等 16 个首轮测试全部 PASS。

## 13.4 门禁是否真加固（破坏实验，`-overlay` 注入，不动工作树）

1. **升级后的 T8 自身**（实现方夹具，61 个日文件）：
   ```
   T8 PASS（trends 4 窗口 × plain/group=model 三布局逐字节相等且非空；
            call-log 前 500 条三布局逐字节相等；守恒通过 ΣN=233,444）
   ```
2. **换我自己的夹具**（`T56_DAY_DIR=/tmp/ip4v/fix/prod/L3/call_log`，独立生成）：`ok 2.644s` → **不是只对实现方夹具有效**。
3. **缺夹具有齿**：`T56_REQUIRE_FIXTURE=1` 且未设 `T56_DAY_DIR` ⇒ `Fatal`：
   ```
   calllog_invariance_test.go:54: T56_REQUIRE_FIXTURE=1 但 T56_DAY_DIR 未设置：CI 门禁不允许静默跳过
   --- FAIL: TestT56FourWindowInvariance
   ```
4. **teeth 实验**（关键）：
   - A. 用 overlay 把 `callLogFileExists()` 的 daily 分支**还原成 D1 缺陷版**（只看 legacy）→ 升级版 T8 **红**：
     ```
     calllog_invariance_test.go:233: trends[today/plain]: L1 与 L3 不一致（纯日文件布局下趋势曾恒空，见 task-63 D1）
     ```
   - B. 同一 D1 回归 + **删掉 T8 里 trends 比较**的弱化版 → **绿**（`ok 7.646s`）。
     ⇒ 证明「加固」确有其事，而且牙齿就来自新增的 trends 比较；不是换个说法。
   - C. 正常代码 + 弱化版 T8 → 绿（sanity）。
5. **门禁缺口（需补）**：升级后的 T8 只比较 **call-log 前 500 条（首页）**，不做分页；
   因此 **D3 这类分页缺陷不会被 T8 抓住**。建议把「生产序（ts 有逆序）夹具 + limit 全枚举分页并集==全集」加入 T8/CI（我的 `TestZZIP4VDefectProdSlicePagination` 可直接改成非依赖 `/tmp` 的小夹具版）。

## 13.5 变异测试（7 个首轮 + 2 个新代码，9/9 CAUGHT）

`/tmp/ip4v/mutate.py`（`go test -overlay`，不改工作树）重跑：

| 变异 | 说明 | 变红的测试 |
| --- | --- | --- |
| M1 | 分文件读取乱序（日文件降序） | LayoutsStatsByteEqual + D2 两个 |
| M2 | 游标跨段不接续（未 seeded 也推进游标） | ColdAppendThenStats |
| M3 | 保留期删未 seeded 段（两处 seeded + 游标保护） | Retention |
| M4 | 省字段后不还原 | AppendOnlyCompat、OldLineOmittedDefaultsFabricated |
| M5 | 无参数路径改走文件 | CallLogNoParamIsMemoryPath、RollbackSingle |
| M6 | 位置游标 +1→+0（游标行重复返回） | D2DupTSPaginationCrossSegment、D2FallbackOrderingKey、HistoryPaginationUniqueTS |
| M7 | 段内续吸 seek 恒 0 | AbsorbSegmentResume |
| **M8** | **位置游标回退分支被删** | D2FallbackOrderingKey、HistoryFilters |
| **M9** | **组内定位改用 req_id 而非行位置（D2 原缺陷归还）** | D2DupTSPaginationCrossSegment、D2FallbackOrderingKey |

`未被测试抓住的变异: 0`。

## 13.6 首轮红线复跑（同钟对拍）

跨进程 dump 必须**同一时钟**下生成：窗口是相对 `time.Now()` 的滚动窗口，隔一小时再比会因窗口滑动产生字节差异（**不是代码差异**）。
本次把 HEAD 干净副本（`/tmp/ip4v/ref/tree`，`calllog.go` sha `6ee8ed59…`）与新实现**背靠背**各 dump 6 份（prod/syn × L1/L2/L3）：

| 对比 | md5 | 结论 |
| --- | --- | --- |
| old prod L1（21:02:42） | `685312c448e82882a8e1b9dc6ef2b517` | — |
| **new prod L1/L2/L3（21:02:59~21:03:04）** | **`685312c4…` ×3** | **old==new 且三布局全等 ✅** |
| old syn L1（21:03:0x） | `d4281b705cdafa5b357d3c25d892588d` | — |
| **new syn L1/L2/L3** | **`d4281b70…` ×3** | **old==new 且三布局全等 ✅** |
| （对照）old prod L2 / old prod L3 | `afed16a7…` / `62027258…` | 旧实现没有日文件概念，读不到日段，故与 L1 不同——旧代码在 L2/L3 夹具上的 dump 无意义，只用于 L1 红线 |

- 实现方声称的 `f93ef566…`：那是 20:02（首轮验收时）同一时钟下的 digest；本次同一时钟下是 `685312c4…`。**证据是「同钟 old==new」与「三布局全等」，digest 绝对值随时钟变化属预期**（趋势窗口今天滑动了 1 小时）。
- 历史查询输出顺序未变：见 §13.3 的 `HistoryOrderEqualsMemoryRing`（文件首页与内存环逐字节相同）。
- 回滚未变：`TestZZIP4VRollbackSingle` PASS（无日目录、旧格式、保留 no-op；首轮跨版本 append 落盘 md5 相同的结论未受影响）。
- `-calllog-layout=single` 语义未动：`TestZZIP4VD1SingleFileMissingStillNil` 额外锁死 single 模式「文件不存在⇒nil」。

## 13.7 ★ 新发现 D3：真实数据 ts 乱序导致分页漏行/重复（D2 根因未彻底修复）

### 13.7.1 生产数据的真实形态（独立统计生产快照 233,654 行）

| 指标 | 实测 |
| --- | --- |
| 相邻行 ts **逆序**次数 | **68,381 次（29.3%）**，中位 15.7s，p90 91.5s，p99 377s，max **2,801s（46 分钟）** |
| 同 ts 多行组 | 2,266 组（首轮已统计），其中 **1,141 组（50%）在文件里不连续**（被其它 ts 打断） |
| 原因 | 文件按「请求完成序」追加，ts 是「请求开始时刻」；流式长请求完成后写入 → 完成序≠开始序 |

### 13.7.2 为什么 still 漏：机制

`historyQuery` 按文件倒序扫描，游标是「上一页最后一条的 ts」：
- 上一页停在某条后，下一页会 `continue` 跳过任何 `ts > before` 的行；
- 但物理扫描序里，**游标之后**完全可能存在 ts 更大的行（乱序）⇒ 这些行被跳过且永远不会返回（**漏**）；
- 反之，`ts < before` 但物理位置在游标**之前**（更靠近文件尾）的行，上一页没吐到（被 limit 截断）⇒ 下一页会再次吐（**重**）。

### 13.7.3 量化（用真实生产字节，走真实 handler 与真实分页游标；`TestZZIP4VDefectProdSlicePagination`）

| 数据 | limit | 页数 | 漏 | 重 |
| --- | --- | --- | --- | --- |
| 生产尾部 10,000 行 | 500 | 21 | **7** | **39** |
| 生产尾部 30,000 行 | 200 | 151 | **117** | **250** |
| 生产尾部 30,000 行 | 500 | 60 | **125** | **73** |
| **生产全量 233,654 行** | 500 | **468** | **1,124** | **1,317** |
| 对照：同 10,000 行**按 ts 升序重排**后写盘 | 500 | — | **0** | **0** |

原始输出（全量）：
```
生产尾部切片 233654 行（limit=500）：分页 468 页，漏 1124 行（例 [yuk32ju6bcw5 …]），重复 1317 行，唯一 req_id 233654
缺陷复现：真实数据 ts 逆序（29.3%）使 (ts,req_id) 分页漏 1124 行/重 1317 行
```
单调对照组：
```
单调控制组：同一批行按 ts 升序写盘后分页 0 漏 0 重
```
⇒ 漏/重的直接原因是 **ts 序与文件扫描序不一致**，不是夹具/过滤器问题。

### 13.7.4 影响面

- **不是本次 D2 修复引入的回归**：D2 修复前同类问题更严重（同 ts 子类也漏）；D2 修好了同 ts 子类。
- **当前无线上影响**：已部署 `static/admin.html:2229` 只调 `/api/call-log?limit=500`（无分页，走内存环）；U3 mockup 里目前也未接分页。
  **但**：这是 task-56 新增「历史查询接口」的核心承诺之一；任何分页 UI/脚本一接就会看到重复与静默缺行（每页 ~1~3 条）。
- 首轮我判定 D2 时用的合成夹具是「同 ts 组 + 时间单调」，因此没暴露这一类；本轮按生产真实形态补测后暴露。

## 13.8 残留风险判读（实现方自述：游标行所在段两页之间被删/归档）

**结论：可接受（不单独构成阻塞），但建议随 D3 一起用位置游标解决。**

实测（`TestZZIP4VD2RiskCursorSegmentPruned`）：
- 形态 A（正常：同 ts 组不跨段）——page1 后删除游标行所在段，page2 仍正确返回剩余段全部行，**不漏不重**；
- 形态 B（人为构造：同 ts 组 sp-a/sp-b 跨两个日文件）——page1 游标停在 sp-a，删除 sp-a 所在段后，page2 回退到旧排序键语义，`sp-b` 在磁盘上但被漏（1 条）。

触发条件为什么稀有：
1. 必须**同 ts 组跨两个日文件**——按「记录 ts 的本地日」落盘时物理上不可能（同一 ts 同一天），只有人为/异常迁移才出现；实测生产 0 例（同 ts 组都在同一文件）。
2. 或者游标行所在段在两页之间被保留策略删除——删除只针对 **>180 天**段；要分页到 180 天以前需翻越全部 180 天数据（limit=500 时 400+ 页），且那时该段数据本来就在按策略删除，不算"分页丢数据"。
3. 归档（gzip）不影响：gz 段仍可读，游标行还能找到（段列表重建后路径变 .gz，但行仍在）。

⇒ 可接受，但 D3 的修法（位置游标）会顺带让这条风险归零。

## 13.9 回归 / 范围 / 哈希

| 项 | 结果 |
| --- | --- |
| 全量回归 | `go test -skip 'TestZZIP4VDefect' -count=1 -timeout 45m ./` → **`ok 162.178s`**（最终一轮，含 rev2 全部测试；缺陷取证测试单独跑：D3 探针 FAIL 属预期，首轮两条缺陷探针已转 PASS） |
| 本任务子系统 `-race` | `go test -race -run 'TestZZIP4V' -skip 'TestZZIP4VDefect' -count=1 ./` → **`ok 3.868s`，0 个 DATA RACE** |
| `sha256sum calllog.go` | `4b6433265f397b7e3278a8583e0b9b974dc0ed22d4fb244d0c39b02880c64691` —— **与实现方声称一致** |
| `sha256sum main.go` | `4394576f…`（与 task-56 交付时相同，本次修复未动入口） |
| `git diff --stat -- calllog.go main.go` | `calllog.go` 1226 行（+1165/−69 总变更）、`main.go` 8 行；`statscan.go` 未改 |
| 6 个保护文件 | `proxy_node.go` / `node_alert.go` / `statscan.go` / `ROADMAP.md` 与 HEAD **逐字节相同**；`static/admin.html` 的 diff（293/118）是**另一并行任务**（mtime 09:55，早于本次修复，首轮验收时已是 411 行 diff） |
| 我的探针未被改动 | `zz_ip4v_shared_test.go` `fc0a29ca…`、`zz_ip4v_verify_test.go` `14749942…`、`zz_ip4v_layout_new.go` `a88d1f8b…`（与 task-61 记录一致）；新增 `zz_ip4v_rev2_test.go` `c182e93c…` |
| 实现方新测试 | `TestDailyOnlyDeploymentTrendsNotEmpty` / `TestSingleFileModeTrendsStillNilWhenFileMissing` / `TestBeforeReqIDUnknownCursorFallsBackToOrderingKey` 独立跑均 PASS（未采信，仅交叉确认） |

## 13.10 D3 最小修法（三选一，建议 A）

**A. 位置游标（推荐，约 40 行）**
- 新增响应头 `X-Call-Log-Next-Cursor`（opaque：`base64(段标识:行号)`，段标识用 legacy 或 `YYYY-MM-DD`，不用路径，避免 plain→gz 改名失效）；
- 请求带 `cursor=` 时，从该行**下一条物理行**继续扫描（后续页不再从头重扫，**同时解决 O(页数×前缀) 的重复 IO**），再套用 `q/model/status/after/before` 过滤；
- 保留 `before/before_req_id` 旧语义（无 cursor 时行为与现在完全一致），旧调用方零影响；
- 正确性：并集==全集、无重复，**与 ts 是否有序无关**（顺带消除 §13.8 风险）。
- 代价：gz 段需要按行号顺序跳到游标位置（解压到该行为止），但只发生在跨 gz 段边界的页，可接受。

**B. 临时修（约 15 行）**
- 分页模式（带 before/after/model/status/q 时）改为「全量读取→按 `(ts desc, req_id desc)` 稳定排序→切页」；正确但每页 O(n)，且分页输出变成严格 ts 序。
- 因只影响分页路径（无参数/内存环路径不变），首轮「新旧逐字节相同」红线不受影响；但面板顺序会从"文件倒序"变成"严格 ts 倒序"（对用户更符合直觉，但与 T8 现有 cross-layout 比较需同步调整）。

**C. 不修（不推荐）**：文档标注「分页仅在 ts 单调时有保证」+ 面板禁用分页 —— 与"历史查询接口"目标不符。

**门禁建议**：把「生产序（ts 有逆序）夹具 + limit 全枚举分页并集==全集」加入 T8/CI（可直接用我的 `TestZZIP4VDefectProdSlicePagination` 改造为小夹具版，不依赖 `/tmp`）。

---

# rev3：task-65（task-66）D3 位置游标修复复验

> 复验方：verify-c5v2（原验收方）｜实现方：capture-429｜基准：`calllog.go` sha256 `37153e123ef7a51b1d38ba6b11b9560336316c3a01b3634eaac31ae332630c5c`（与其声称一致）
> 原则：不复用实现方测试/夹具；全部用自己新增的 `zz_ip4v_rev3_test.go`（5 个新用例）+ 已有首轮/rev2 探针 + 独立对拍。

## 14.1 结论：**go**

| 项 | 结论 | 证据 |
| --- | --- | --- |
| D3 本体（真实 ts 乱序分页漏/重） | ✅ **已修好**（合成生产序全枚举 + 生产全量 233,654 行 468 页 0/0） | §14.2 |
| 响应头语义变化（`Next-Before-Req`: 裸 req_id → 不透明 token） | ⚠️ **旧调用方原样成对回传无感**（实测 0/0）；**手工解析该值的调用方会坏**；头名/API 文档应标注（不阻断 go） | §14.3 |
| 错误契约（bad-cursor / stale-cursor / 回退标记 / 跨 gz 改名） | ✅ 全部符合声称 | §14.4 |
| 门禁是否真覆盖分页 | ✅ **真覆盖**；off-by-one 变异 → T8 红，删掉分页判据的弱化版 → 绿（teeth 实证） | §14.5 |
| 首轮/§12/§13 红线 | ✅ 未回归（同钟 old==new、三布局全等、回滚 append 逐字节相同、旧 before 语义/内存环/single 全绿） | §14.6 |
| 变异（7 首轮 + 2 上轮 + 4 本轮） | ✅ **13/13 CAUGHT** | §14.7 |
| 自述 3 条残留风险 | ✅ 均 **可接受**，另给 2 条改进建议（非阻断） | §14.8 |
| 回归/范围 | ✅ 全量 `ok 209.479s`、子系统 `-race` `ok 50.527s` 0 race、sha 与声称一致、只动 calllog.go+main.go | §14.9 |

**没有发现阻断项。** 前两轮缺陷（D1/D2）与本轮 D3 均已闭环；建议按 §14.8 的两条非阻断项（面向调用方的文档标注 token 语义；T8 分页 helper 加页数上限）顺手处理，不阻塞合并。

## 14.2 D3 本体：独立验证（不复用实现方测试）

**(a) 合成「生产真实序」夹具**（`zzRev3ProdOrder`，400 条：ts=开始时刻、相邻 1.5s；按「完成时刻=开始+1~37s 抖动」追加；**实测相邻 ts 逆序 35.1%**，断言 ≥10%）：
- `limit ∈ {1,2,3,5,7,13,50,199,500}` 逐个连翻（新位置游标）：**并集==全集、无重复、顺序与「单页全集（limit=2000）」逐条一致**；
- daily（2 段日文件）与 single（1 段）**逐页**结果一致（limit=1/3/7/199 逐页比对）；
- **旧调用方路径**（只读 `Next-Before` / `Next-Before-Req` 两个头并原样回传）：limit=7 连翻 58 页，同样 0 漏 0 重、顺序一致。
```
--- PASS: TestZZIP4VRev3ProdOrderCursorPagination
    合成生产序夹具：400 条，相邻 ts 逆序 35.1%
    D3 本体 PASS：[1 2 3 5 7 13 50 199 500] 全部并集==全集、无重、顺序==单页全集；daily==single 逐页一致；旧头回传 58 页同样正确
```

**(b) 生产快照真实字节**（我上轮的探针 `TestZZIP4VDefectProdSlicePagination` **一行未改**，现在转绿）：

| 数据 | limit | 页数 | 漏 | 重 | 上轮对照（修前） |
| --- | --- | --- | --- | --- | --- |
| 生产尾部 10,000 行 | 500 | 20 | **0** | **0** | 漏 7 / 重 39 |
| 生产尾部 30,000 行 | 200 | 150 | **0** | **0** | 漏 117 / 重 250 |
| **生产全量 233,654 行** | 500 | **468** | **0** | **0** | 漏 1,124 / 重 1,317 |
| 单调对照组（同批行按 ts 升序重排） | 500 | — | 0 | 0 | 0 / 0 |
```
生产尾部切片 233654 行（limit=500）：分页 468 页，漏 0 行（例 []），重复 0 行，唯一 req_id 233654
单调控制组：同一批行按 ts 升序写盘后分页 0 漏 0 重
--- PASS: TestZZIP4VDefectProdSlicePagination
```

**(c) 过滤参数组合分页**（面板真实路径；`status=ok|fail` / `model` / `q` / `after` 各配 limit=3 连翻）：并集==同过滤单页全集、无重、顺序一致：
```
过滤分页 PASS：status=ok → 48 条/16 页；status=fail → 12/4；model=m-a → 40/14；q=ft-01 → 10/4；after=… → 40/14（均 0 漏 0 重、顺序一致）
```

## 14.3 响应头语义变化判定（要求给明确结论）

**事实**：`X-Call-Log-Next-Before-Req` 在 `Has-More: true` 时回传**不透明 token**（`c1_` 前缀），不再等于本页最后一条的 `ReqID`；`Has-More: false` 时仍回传裸 `ReqID`（同一响应头两种取值形态）。新增独立头 `X-Call-Log-Next-Cursor` 也回传同款 token。

**判定**：
1. **旧调用方（成对原样回传 `Next-Before` + `Next-Before-Req`）确实无感** —— handler 识别 `c1_` 前缀自动走位置续扫，实测 0 漏 0 重（§14.2a 旧头路径）。
2. **手工解析该头、把它当 req_id 用的调用方会坏**：值不再是 req_id（不能拿去显示/比对/当主键）；反过来，若调用方只回传 token 而**漏掉** `before` 时间头，则不会进入文件历史路径、token 不被识别（会退化成重复首页），即「无感」的前提是**两个头成对原样回传**。
3. **头名已名不副实**（叫 `...-Req` 但值可能是游标）。建议：面向调用方的 API 文档标注「不透明分页游标，勿解析；新代码以 `X-Call-Log-Next-Cursor` 为准」，发布说明里写明该头的取值形态变化；后续若做 v2 契约可考虑改名（`X-Call-Log-Next-Cursor` 已有，旧头仅作兼容）。
4. 结论：**属契约变更但兼容旧用法，不构成 go 阻断**；需在文档/发布说明标注（实现方报告 §13.2 已写，但 `log-retention-impl-plan.md` 面向前端的分页描述仍是旧语义，建议同步）。

## 14.4 错误契约逐个验证

```
--- PASS: TestZZIP4VRev3CursorErrorContract
    bad-cursor ×3（"zzz"、非法 base64、解出空段）→ 400 + X-Call-Log-Error: bad-cursor
    stale-cursor（未知段 "1999-01-01"、行号越界 99999）→ 400 + X-Call-Log-Error: stale-cursor
    合法游标 → 200 + X-Call-Log-Source: file
    段明文→.jsonl.gz 改名后：旧 token 仍 200（段标识=日期，非路径），改名后从头连翻 0 漏 0 重
--- PASS: TestZZIP4VRev3LegacyHeaderStaleFallback
    旧头回传方遇到「游标段被删」：200 + X-Call-Log-Cursor-Fallback: before，且返回行严格早于 before
    同一 token 用显式 ?cursor= 请求：400 stale-cursor（不静默给残缺页）
--- PASS: TestZZIP4VRev3HeaderSemantics
    Next-Before-Req = c1_… ≠ 最后一条 ReqID；旧头成对回传 6 条无漏无重；手工传裸 req_id 仍走旧排序键语义（返回更早 4 条、不含游标行）
```

## 14.5 门禁破坏实验（证明「补门禁」是真的）

1. **T8 现在确实跑分页判据**（我用自己的夹具独立复跑也 PASS）：
```
分页门禁：位置游标 limit=500 全量连翻 467 页 / 233444 条，0 漏 0 重；三布局前 3 页顺序一致
T8 PASS（50.15s；换我的 /tmp/ip4v/fix/prod/L3 夹具：ok 55.536s）
T56_REQUIRE_FIXTURE=1 缺夹具：Fatal（红）
```
2. **破坏实验 A**（`-overlay`，不改工作树）：把位置游标改成 off-by-one（`jFrom = cursorLine - 1` → `cursorLine`，游标行重复返回）→ **原版 T8 红**：
```
calllog_invariance_test.go:288: 全量分页并集 233911 条，期望 absorbed=233444（漏/重即 D3 回归）
--- FAIL: TestT56FourWindowInvariance (34.94s)
```
3. **破坏实验 B**：同一 off-by-one 变异 + 删掉 T8 分页判据的**弱化版** → **绿**（`ok 2.649s`）⇒ 牙齿就来自新增的分页判据，不是别的比较兜住的。
4. **门禁缺口/建议（非阻断）**：
   - `t56PaginateRun` 无页数上限：若游标**完全**失效（每页从头返回）会死循环到 `go test` 超时而非快速失败；建议 `maxPages = absorbed/limit + 10` 防御。
   - T8 直调 `historyQuery`，不覆盖 handler 接线（`?cursor=` 解析、400 错误体、`Next-Cursor` 头输出）。这些由我的 §14.4 与实现方 CI 用例覆盖；建议把「bad-cursor 400 + Next-Cursor 头存在」的 handler 级契约也纳入常驻 CI（实现方 `calllog_prodorder_test.go` 已有，交界处无洞）。

## 14.6 红线复跑（同钟对拍）

老实现（HEAD 干净副本 `/tmp/ip4v/ref/tree`，`calllog.go` sha `6ee8ed59…`）与新实现**背靠背**同一时钟各 dump 6 份（prod/syn × L1/L2/L3）：

| 对比 | md5 | 结论 |
| --- | --- | --- |
| **new prod L1/L2/L3**（22:52） | **`990b3117490950ce576e979679e2cc95` ×3** | 三布局全等 ✅ |
| **old prod L1**（22:51） | `990b3117…` | **old==new ✅**（与实现方声称的 `990b3117…` 完全一致） |
| **new syn L1/L2/L3** | **`c71d2246fe648c7fecb513af8a41a3e6` ×3** | 三布局全等 ✅ |
| **old syn L1** | `c71d2246…` | **old==new ✅**（与声称一致） |
| old prod L2 / L3 | `65954562…` / `62027258…` | 旧实现没有日文件概念，读不到日段 ⇒ 与 L1 不同属**旧实现缺陷形态**，非回归（与实现方报告数值一致） |

**回滚/落盘红线**：`ZZ_MODE=append`（single 布局）旧 vs 新：
```
app_old.txt = ef3ba3d49ac122ac4ed43272ddd42f84
app_new.txt = ef3ba3d49ac122ac4ed43272ddd42f84   ⇒ 逐字节相同 ✅
落盘 call_log.jsonl 两侧 md5 均 b4cc4877b07fb7e6f56958af1bf6877e（相同）
```
**旧语义/内存环/回滚行为**：`TestZZIP4V*` 全部探针转绿（28 PASS，2 个环境门控 SKIP 为既有探针）：
- `before`/`before_req_id` 旧语义：`TestZZIP4VHistoryFilters`（`/zzz` 期望 4 条、排他边界）、`TestZZIP4VHistoryPaginationUniqueTS` PASS；
- 无参数仍走内存环：`TestZZIP4VCallLogNoParamIsMemoryPath` PASS；dumps 里 `/api/call-log` 与 `?limit=100` 已含在 old==new 字节中；
- `-calllog-layout=single` 回滚：`TestZZIP4VRollbackSingle` PASS。

## 14.7 变异测试：13/13 CAUGHT

`/tmp/ip4v/mutate3.py`（`go test -overlay`，不改工作树；测试集=首轮/rev2/rev3 我的探针）：

| 变异 | 说明 | 变红的测试 |
| --- | --- | --- |
| M1 | 日文件读取降序 | Rev3ProdOrderCursorPagination、LayoutsStatsByteEqual 等 6 个 |
| M2 | 游标跨段不接续 | ColdAppendThenStats |
| M3 | 保留期删未 seeded 段 | Retention |
| M4 | 省字段不还原 | AppendOnlyCompat、OldLineOmittedDefaultsFabricated |
| M5 | 无参数改走文件 | CallLogNoParamIsMemoryPath、RollbackSingle |
| M6 | D2 组内游标 +1→+0 | D2FallbackOrderingKey、Rev3HeaderSemantics |
| M7 | 段内续吸 seek 恒 0 | AbsorbSegmentResume |
| M8 | D2 回退分支被删 | D2FallbackOrderingKey、HistoryFilters |
| M9 | D2 组内按 req_id 定位 | D2FallbackOrderingKey |
| **M15** | D3 游标 off-by-one | Rev3ProdOrderCursorPagination、Rev3CursorErrorContract、Rev3HeaderSemantics 等 |
| **M16** | D3 段标识用文件路径 | Rev3CursorErrorContract、Rev3LegacyHeaderStaleFallback（gz 改名测试） |
| **M17** | D3 位置游标不产出 | Rev3ProdOrderCursorPagination、Rev3CursorErrorContract、Rev3LegacyHeaderStaleFallback、Rev3HeaderSemantics |
| **M18** | D3 失效游标不报错（行号越界静默空页） | Rev3CursorErrorContract |

`未被测试抓住的变异: 0`。

## 14.8 实现方自述 3 条残留风险：逐条判读

1. **「旧头回退 + 180 天删段 + 同 ts 跨段才静默回退」** ⇒ **可接受**。
   实测（§14.4）：显式 `?cursor=` 段被删/越界一律 **400 stale-cursor**（调用方明确知道要重分页，不会拿到看似成功的残缺页）；只有**旧头回传方**才回退 `before` 语义并回 `X-Call-Log-Cursor-Fallback: before`（可观测），且返回行严格早于 before。触发条件：①游标段在两页之间被保留策略删除（只删 >180 天段，需翻 400+ 页且数据本就在删）；②同 ts 组跨段（按 ts 本地日落盘物理不可能，生产 0 例）。回退本身会漏掉「游标之后但段已删」的行——那正是被保留策略删除的数据，语义上合理。
2. **「分页需带过滤参数（如 `status=all`）」** ⇒ **可接受**。无参数走内存环是首轮红线（逐字节兼容）；`cursor`/`before`/`after`/`model`/`status`/`q` 任一即进文件路径（`cursor` 单独传也能进，§14.4 已验）。已在文档写明，前端「加载更多」传 `status/all`+日期即可。
3. **「单文件 legacy 布局每页整文件读建立行号，非回归」** ⇒ **可接受**。旧实现本来就每页从尾部重扫，新实现只多一次整文件行切分；实测全量 233,654 行单文件 468 页共 114s（≈0.24s/页，面板一次只拉一页无感），日文件布局每页只读 1~2 段（T8 467 页 35~50s）。single 本身是回滚用过渡布局。

**我的 2 条非阻断建议**：① `Next-Before-Req` 的 token 语义写进面向调用方的文档/发布说明（并在 `hasMore=false` 时统一行为或标注该头此时无意义）；② T8 的 `t56PaginateRun` 加页数上限，防游标完全失效时挂到超时。

## 14.9 回归 / 范围 / 哈希

| 项 | 结果 |
| --- | --- |
| 全量回归 | `go test -count=1 -timeout 45m ./` → **`ok 209.479s`** |
| 本子系统 `-race` | `go test -race -run 'TestZZIP4V' -count=1 ./` → **`ok 50.527s`，0 DATA RACE** |
| 我的全部探针（含 D3 缺陷探针） | `go test -run 'TestZZIP4V'` → **`ok 8.317s`，28 PASS（2 个既有环境门控 SKIP）**；D3 探针已由 FAIL 转 **PASS** |
| `sha256sum calllog.go` | `37153e123ef7a51b1d38ba6b11b9560336316c3a01b3634eaac31ae332630c5c` —— **与实现方声称一致** |
| `sha256sum main.go` | `4394576f…`（与 task-56 相同，本次未动入口） |
| `git diff --stat -- calllog.go main.go` | `calllog.go` 1402 行（+1339/−71）、`main.go` 8 行；`statscan.go` 未改 |
| 保护文件 | `proxy_node.go` / `node_alert.go` / `statscan.go` / `ROADMAP.md` 与 HEAD **逐字节相同**；`static/admin.html` diff（293/118）为**另一并行任务**（mtime 09:55，早于本轮修复） |
| 我的探针未被碰 | `zz_ip4v_shared_test.go` `fc0a29ca…`、`zz_ip4v_verify_test.go` `14749942…`、`zz_ip4v_layout_new.go` `a88d1f8b…`、`zz_ip4v_rev2_test.go` `c182e93c…`（与上轮记录一致）；本轮新增 `zz_ip4v_rev3_test.go` `cba2650a0cb4b1e7211c858f71fe48b7cfeeb984f13dce6121932dd6cb8f9817` |

**产物**：`zz_ip4v_rev3_test.go`（5 个独立用例：生产序全枚举/过滤组合/错误契约/旧头回退/头语义）；`/tmp/ip4v/out/rev3_*`（生产分页原始输出、全量回归、变异表、dump、append 对拍）；`/tmp/ip4v/rev3/`（门禁破坏 overlay 与弱化 T8）；`/tmp/ip4v/mutate3.py`（13 变异）。
