# 日志系统现状盘查 + 精简方案

- 任务：task-52（只读盘查，不改代码/配置）
- 数据快照：**2026-10-20 12:56**（生产容器 `opencode2api` 运行中）
- 报告人：`capture-429`（独立盘查，数字全部本机复算）
- 复算脚本：`.research/t52/{audit.py,deep.py,deep2.py,perday_clean.py,runlog_stats.py}`
- 证据文件：`.research/t52/evidence/`（`tables.txt`、`summary.json`、`fields.json`、`days.json`、`deep.json`、`deep2.txt`、`perday_clean.json`、`runlog-stats.txt`、`snap.sha256`、`call_log.jsonl.gz`）

> 本文所有「行数/字节/占比」都来自**全量**而不是抽样；每个数字后面都给了复算命令（见 §11）。

---

## 0. 结论摘要（先看这段）

### 0.1 两种日志的现状一句话

| | 运行日志 | 调用日志 |
|---|---|---|
| 落盘位置 | `/data/opencode2api.log`（lumberjack 轮转）+ **同时写 docker stdout（json-file，无上限）** | `/data/call_log.jsonl`（单文件，**无轮转、无归档、无保留策略**） |
| 当前体积 | 73.67 MB（373,747 行，2026-10-06 21:12 → 10-20 12:56） | **124.52 MB（233,444 行，2026-08-13 17:40 → 10-20 12:50）** |
| 有没有上限 | **有**：单文件 100 MB 轮转，保留 7 份/14 天，轮转后 gzip | **没有**（只增不减，直到用户点「清空」） |
| 面板能看到多少 | 内存环 5000 条；`/api/logs` 只回最近 **500** 条（≈ 20 分钟~5 小时，随流量） | `/api/call-log` 只回内存环最近 **2000 条**（`limit` 默认 500），**查不到历史** |
| 用户期望 | 「即时的、缓存一部分、超上限丢最旧」→ **基本已实现**（内存环 + 轮转），差的是「丢最旧」的用户可感知语义与 docker 副本 | 「长久保存 + 前端可查」→ **保存做到了，查询没做到**（前端只有 2000 条窗口） |

### 0.2 最重要的 5 个发现

1. **`events` 仍是调用日志最大字段（36.7% / 195.7 B/行）**，但**「干净记录不落盘 events」的裁剪已在 10-07 部署生效**：
   10-07 之后每天写盘的 events 只占 0.0~46%，而 8-13~10-06 的存量里 100% 都有。
   ⇒ 存量 124.5 MB 里有 **25.7 MB（20.7%）是历史遗留的、以后不会再写的 events**。
   （Lead 抽样给的 46.7% / 269 B/行 偏高，原因是样本落在 9 月中下旬事件密集期；全量实测 36.7% / 195.7 B/行。）
2. **真正零读者的字段只有 2 个**：`final_status`（4,210 行 / 75.8 KB）与 `dial_ms`（997 行 / 13.5 KB）——都是 10-19 新加、前端和统计都还没读。省不了多少（0.07%），但属于纯浪费。
3. **可无损省掉的大头不是「删字段」，而是「省略默认值 + 去掉冗余副本」**：
   `path`（98.5% 是 `/v1/chat/completions`，28.9 B/行）与 `route_mode`（99.997% 是 `public`，21.0 B/行）可按 `omitempty` 省 **11.6 MB（9.4%）**；
   `events[].node` / `events[].node_name` 与顶层 `nodes` / `node_names` **100% 重复**（233,750/246,086 引用全部能在顶层找到），可省 **16.8 MB（13.5%）**。
4. **运行日志 9.1% 是自触发的无效日志**：`config hot-reloaded` 在 10-09 19:08 ~ 10-20 11:09 之间以**精确 5 秒**间隔打了 **67,763 条 / 6.7 MB**。
   根因是 `startConfigWatcher`（`main.go:1240-1274`）检测到 mtime 变化后 `saveConfig()` 重写文件 → mtime 又变 → 下一轮再触发，**自己触发自己**。
   我在本机用当前代码 22 秒复现：`touch` 一次配置后产生 4 条 hot-reload、config.json 的 mtime 持续前进（证据 §2.4）。
5. **docker 那份副本是无上限的**：容器 `LogConfig=json-file` 且 **没有 `max-size`/`max-file`**（`docker inspect` 实测），
   而服务同时 `stdout=true`，所以每条运行日志在 docker 的 json-file 里再存一份、永不轮转。当前实例 12:40 才起，副本还小；长期跑会与文件副本等量增长。

### 0.3 一句话建议（详见 §6/§7）

- 运行日志：**先修 hot-reload 自触发循环**（一行 if 判断即可，收益 −9%），再把轮转从 `100MB/7份/14天` 收到 `20MB/10份/7天`（磁盘从 ~80 MB 降到 ~22 MB，丢 14 天前历史），
  内存环保持；面板「运行日志」想看更久需要把环从 5000 调大或加「导出」入口（已有 `/api/logs/export`）。
- 调用日志：**保留追加语义**，新行做三件低成本事：默认值省略（`path`/`route_mode`）、停写 `events[].node_name`、停写零读者字段；
  存量不动（或离线 gzip 归档到 11 MB）；再加**按天分文件 + gzip + 30 天保留**，30 天占用从 ~65 MB 降到 **2.4~5 MB（压缩后）**。
- 前端历史查询：`/api/call-log` 加 `before/after/model/status` 参数，配合按天文件**不需要索引**（每天 ≤5 MB，倒读 2000 行 ~1 ms）。

---

## 1. 快照与方法

```bash
# 只读抓取（docker exec，未重启、未改配置）
docker exec opencode2api ls -la /data/
docker exec opencode2api cat /data/call_log.jsonl > /tmp/t52/call_log.jsonl
docker exec opencode2api cat /data/opencode2api.log > /tmp/t52/run.log
docker exec opencode2api cat /data/opencode2api-2026-10-06T21-12-02.050.log.gz > /tmp/t52/run-2026-10-06.log.gz
sha256sum /tmp/t52/call_log.jsonl /tmp/t52/run.log | tee /tmp/t52/snap.sha256
```

```console
25d6d42eb3693ae5dd7e0811d03472611ba244ac3ca01007f61dd33d658575f7  call_log.jsonl
6731d068c8903ac29040355d42e3d6583cfa2318ada2717cdab965ccf16c5be6  run.log
233444 call_log.jsonl
373747 run.log
```

```console
$ docker exec opencode2api ls -la /data/
-rw-r--r-- 1 opencode opencode 124520431 Oct 20 12:51 call_log.jsonl
-rw-r--r-- 1 opencode opencode      1479 Oct 20 12:40 config.json
-rw-r--r-- 1 opencode opencode       420 Oct 20 12:41 free_models_cache.json
-rw------- 1 opencode opencode   5360725 Oct  6 21:12 opencode2api-2026-10-06T21-12-02.050.log.gz
-rw------- 1 opencode opencode  73671118 Oct 20 12:56 opencode2api.log
-rw-r--r-- 1 opencode opencode     62126 Oct 20 12:43 proxy_state.json
-rw-r--r-- 1 opencode opencode      4095 Oct 20 12:51 stats.json
```

> ⚠️ 与 Lead 初测的差异：Lead 记的是 **118.9 MB / 233,444 行 / 509 B/行**；本次快照是 **124.52 MB / 233,444 行 / 533.4 B/行**。
> 行数完全一致，字节差 5.6 MB 是**日志仍在增长**（12:51 又写入了若干条）。以下全部以本次快照为准。

---

## 2. 运行日志（`opencode2api.log`）

### 2.1 写入路径与轮转（`file:line`）

| 事实 | 位置 |
|---|---|
| 轮转库是 lumberjack | `logging.go:16`（`gopkg.in/natefinch/lumberjack.v2`） |
| 写盘句柄：`Filename=Filename, MaxSize=logMaxSize, MaxBackups=logMaxBackups, MaxAge=logMaxAge, Compress=logCompress, LocalTime=true` | `logging.go:165-172` |
| 同时写 stdout（`docker logs` 也有一份） | `logging.go:148-149`，`io.MultiWriter(writers...)` `logging.go:182` |
| 所有业务日志走 `slog`（`slog.Info/Warn/Error`），默认 handler 为 `slog.NewTextHandler` | `logging.go:183-190` |
| 参数默认值：`-log-file=opencode2api.log`、`-log-max-size=100`(MB)、`-log-max-backups=7`、`-log-max-age=14`(天)、`-log-compress=true`、`-log-level=info`、`-log-bodies=false` | `main.go:7555-7562` |
| 生产实际生效值（日志自述） | 见下 |

生产日志里的配置自述行（每次启动打一条）：

```console
$ grep "logging configured" /tmp/t52/run.log | tail -1
time=2026-10-20T12:40:59.350+08:00 level=INFO msg="logging configured" path=/data/opencode2api.log
  level=info stdout=true log_bodies=false max_size_mb=100 max_backups=7 max_age_days=14 compress=true
```

⇒ 轮转策略是**明确的**：100 MB 轮转、保留 7 份、14 天、gzip。
`/data/` 里只有一个 `…2026-10-06T21-12-02.050.log.gz`（5.36 MB）是因为：
① 当前文件自 10-06 21:12 起才 73.7 MB，**还没到 100 MB 阈值**；
② 更早的轮转副本已被 `MaxAge=14 天` 删掉（今天是 10-20，10-06 那份也快到龄）。

### 2.2 有没有「环形缓冲」

**有，但只服务面板，不影响磁盘**：

| 事实 | 位置 |
|---|---|
| 内存环容量 **5000** 条（`const logRingCapacity = 5000`） | `logring.go:17` |
| 环的写入：`captureHandler.Handle` 把已脱敏的 `slog.Record` 塞进环再交给下层写盘 | `logring.go:114-133` |
| `/api/logs` 快照只回最近 **500** 条（`const maxLogRows = 500`） | `logring.go:158,165-172` |
| `/api/logs/stream` SSE 增量、`/api/logs/export` 导出全部 5000 条 | `logring.go:186-283` |
| 面板读的就是这几个端点 | `static/admin.html` 运行日志页 |

⇒ 用户说的「缓存一部分，超过上限去掉最先的」**在面板侧已经实现了**（5000 条环形、丢最旧）。
但要注意量级：生产忙日 57,226 行/天（10-18）⇒ **5000 条环只能覆盖约 2 小时**，`/api/logs` 的 500 条只覆盖约 12 分钟。
所以「丢失感」主要来自**面板窗口小**，不是磁盘没轮转。

### 2.3 实测：体积、增长率、按天分布

```console
$ python3 /tmp/t52/runlog_stats.py
行数=373747 字节=73671118 平均B/行=197.1
最早 2026-10-06T21:12:02.046+08:00 最新 2026-10-20T12:56:32.714+08:00 带time行数 373747
```

- 平均 **197 B/行**，**5.39 MB/天**（13.66 天口径）；**最近 7 天 6.19 MB/天**。按此速率，100 MB 轮转约 **16~18 天**一次。
- 磁盘稳态估算：当前文件 ≤100 MB + 14 天内轮转副本（每份约 5.3 MB gz）× 1~2 份 ≈ **80~105 MB**。

按天（`run.log` 前 10 天与后 5 天摘录，完整见 `evidence/runlog-stats.txt`）：

| 日期 | 行数 | 体积 |
|---|---|---|
| 2026-10-06 | 14,477 | 3.36 MB |
| 2026-10-07 | 32,447 | 7.46 MB |
| 2026-10-08 | 13,813 | 3.11 MB |
| 2026-10-09 | 10,615 | 2.01 MB |
| 2026-10-10 | 31,632 | 5.06 MB |
| 2026-10-15 | 36,582 | 7.23 MB |
| 2026-10-16 | 35,744 | 8.02 MB |
| 2026-10-17 | 33,206 | 7.47 MB |
| **2026-10-18** | **57,226** | **11.86 MB** |
| 2026-10-19 | 24,926 | 5.23 MB |
| 2026-10-20 | 6,685 | 1.20 MB（截止 12:56） |

### 2.4 体积构成：级别 + 消息类型（这是「精简」的抓手）

| level | 行数 | 字节 | 占比 |
|---|---|---|---|
| INFO | 311,430 | 61,075,472 | **82.9%** |
| WARN | 43,370 | 8,798,416 | 11.9% |
| ERROR | 18,882 | 3,782,698 | 5.1% |
| DEBUG | 65 | 14,532 | 0.0% |

| msg | 行数 | 字节 | 占比 | B/行 |
|---|---|---|---|---|
| `request_plan` | 37,034 | 13,372,504 | **18.2%** | 361.1 |
| `upstream_attempt` | 49,215 | 9,990,922 | 13.6% | 203.0 |
| `node marked dead by health check` | 31,618 | 8,161,315 | 11.1% | 258.1 |
| `node health probe failed` | 33,872 | 7,659,579 | 10.4% | 226.1 |
| `request_done` | 43,502 | 7,232,611 | 9.8% | 166.3 |
| **`config hot-reloaded`** | **67,763** | **6,708,537** | **9.1%** | **99.0** |
| `upstream_result` | 37,000 | 6,267,114 | 8.5% | 169.4 |
| `request_result` | 19,389 | 4,322,956 | 5.9% | 223.0 |
| `upstream error` | 16,060 | 3,437,289 | 4.7% | 214.0 |
| `stream_result` | 7,717 | 2,309,988 | 3.1% | 299.3 |

**发现 A：`config hot-reloaded` 是自触发循环（已复现）。**

```console
$ grep 'msg="config hot-reloaded"' /tmp/t52/run.log | tail -3
time=2026-10-20T11:09:15.951+08:00 level=INFO msg="config hot-reloaded" aliases=0 api_key_set=true
time=2026-10-20T11:09:20.951+08:00 level=INFO msg="config hot-reloaded" aliases=0 api_key_set=true
time=2026-10-20T11:09:25.952+08:00 level=INFO msg="config hot-reloaded" aliases=0 api_key_set=true
# 间隔分布：67745/67763 次是精确 5 秒；区间 2026-10-09 19:07 → 2026-10-20 11:09（此后容器重启而停止）
```

本机复现（用当前仓库代码构建到 `/tmp`，不碰仓库、不碰生产；22 秒窗口）：

```console
$ /tmp/t52/bin/oc2api -port 9333 -config /tmp/t52/cfg/config.json -password "" \
    -log-file /tmp/t52/cfg/run.log -log-stdout=false &
$ curl -s -o /dev/null -w 'health=%{http_code}\n' http://127.0.0.1:9333/health
health=200
$ touch /tmp/t52/cfg/config.json          # 只触发一次
$ sleep 22; grep -c "config hot-reloaded" /tmp/t52/cfg/run.log
4
time=2026-10-20T13:09:35.753+08:00 level=INFO msg="config hot-reloaded" aliases=0 api_key_set=false
time=2026-10-20T13:09:40.752+08:00 level=INFO msg="config hot-reloaded" aliases=0 api_key_set=false
time=2026-10-20T13:09:45.753+08:00 level=INFO msg="config hot-reloaded" aliases=0 api_key_set=false
time=2026-10-20T13:09:50.753+08:00 level=INFO msg="config hot-reloaded" aliases=0 api_key_set=false
$ stat -c '%y' /tmp/t52/cfg/config.json   # 每轮都被重写
2026-10-20 13:09:50.752063505 +0800
```

根因（`main.go:1238-1277`）：

```go
ticker := time.NewTicker(5 * time.Second)      // main.go:1244
...
if !info.ModTime().After(lastModTime) { continue }  // main.go:1251
lastModTime = info.ModTime()
...
applyConfig(newCfg)
if err := saveConfig(configPath, newCfg); err != nil { ... }  // main.go:1271 ← 重写 → mtime 变
slog.Info("config hot-reloaded", ...)          // main.go:1274
```

⇒ `saveConfig` 无条件重写文件 ⇒ mtime 每 5 秒前进一次 ⇒ 永远满足「mtime 变大」⇒ **无限循环**。
影响：① 9.1% 的日志体积；② 每 5 秒重写一次 config.json（写盘 + mtime 抖动，热加载其它消费者也会被带着跑）；
③ 排查真正的配置变更时被噪声淹没。

**发现 B：docker 副本无上限。**

```console
$ docker inspect -f 'LogOpts={{.HostConfig.LogConfig.Type}} {{.HostConfig.LogConfig.Config}}' opencode2api
LogOpts=json-file map[]
$ docker inspect -f 'LogPath={{.LogPath}}' opencode2api
LogPath=/var/lib/docker/containers/bb30…/bb30…-json.log
```

`map[]` = 没配 `max-size`/`max-file`，而服务 `stdout=true` ⇒ 每条日志在 docker json-file 里**再存一份、无轮转**。
（当前容器 12:40 才启动，副本尚小；长期运行会与文件副本同时增长。取证受限：`/var/lib/docker` 无读权限，无法 `du`。）

---

## 3. 调用日志（`call_log.jsonl`）——全量实测

### 3.1 总量、行数、时间范围

```console
$ python3 /tmp/t52/audit.py /tmp/t52/call_log.jsonl
行数: 233444  字节: 124520431  平均 B/行: 533.4
最早: 2026-08-13T17:40:27.744Z  最新: 2026-10-20T12:50:56.797+08:00  坏行: 0
行长 p50/p95/max: 483 / 983 / 4194
```

- 68 天存量 **124.52 MB / 233,444 行 / 533.4 B/行**；无坏行。
- 纯行框（`{}`、逗号、换行）合计 **3.50 MB**；字段内容合计 121.02 MB。

### 3.2 按天分布（全量，完整表见 `evidence/days.json`）

| 日期 | 行数 | 体积 | ok | fail |
|---|---|---|---|---|
| 2026-08-13 | 102 | 0.04 MB | 97 | 5 |
| 2026-08-16 | 1,501 | 0.65 MB | 1,480 | 21 |
| 2026-09-16 | 1,895 | 1.25 MB | 1,698 | 197 |
| 2026-09-17 | 1,680 | 1.34 MB | 1,363 | 317 |
| **2026-09-18** | **16,568** | **9.17 MB** | 14,744 | 1,824 |
| **2026-09-19** | **15,188** | **9.70 MB** | 11,013 | 4,175 |
| 2026-09-20 | 12,073 | 7.10 MB | 9,769 | 2,304 |
| 2026-09-21 | 16,686 | 9.26 MB | 13,385 | 3,301 |
| 2026-09-27 | 14,996 | 7.70 MB | 14,783 | 213 |
| 2026-09-30 | 17,232 | 9.21 MB | 16,650 | 582 |
| 2026-10-07 | 2,003 | 0.92 MB | 675 | 1,328 |
| 2026-10-15 | 4,400 | 2.30 MB | 4,022 | 378 |
| 2026-10-16 | 6,531 | 2.70 MB | 6,410 | 121 |
| 2026-10-17 | 4,687 | 2.72 MB | 2,930 | 1,757 |
| 2026-10-18 | 7,636 | 4.72 MB | 3,103 | 4,533 |
| 2026-10-19 | 3,516 | 1.84 MB | 2,838 | 678 |
| 2026-10-20 | 794 | 0.37 MB | 493 | 301 |

- 68 天平均 **1.83 MB/天**；其中 9 月中下旬（事件密集期）7~9.7 MB/天。
- **10-07 之后（events 裁剪上线后）平均 1.35 MB/天**（18.87 MB / 14 天）；最近 7 天 2.18 MB/天（流量上升）。
- 9 月高峰期每天 1.5~1.7 万行；10 月平均 ~2,600 行/天（最近 7 天 ~4,100 行/天）。

### 3.3 字段体积表（全量，**含「缺失率」与「零读者」列**）

> 字节数 = 该字段在原始行里占的字节（`"key":value`，含引号，不含分隔逗号）；
> 缺失率 = 该行没有这个键的比例（`omitempty` 或历史无此字段）；
> 零读者 = 代码/前端/统计都不读 → 纯浪费（依据见 §4）。

| # | 字段 | 出现行数 | 缺失率 | 字节 | 字节占比 | B/行 | 读者 | 零读者？ |
|---|---|---|---|---|---|---|---|---|
| 1 | `events` | 207,251 | 11.2% | 45,688,475 | **36.7%** | 195.7 | 面板时间线/徽章/时段表（`admin.html:2206 clHasIssue`、`2299`、`2311`） | 否（但**干净记录已被裁**，见 3.5） |
| 2 | `nodes` | 222,273 | 4.8% | 10,460,237 | 8.4% | 44.8 | 面板节点链/节点视图/搜索（`admin.html:2279 clNodeChain`、`2320 clRenderNode`、`2246` 搜索） | 否 |
| 3 | `node_names` | 211,545 | 9.4% | 9,264,992 | 7.4% | 39.7 | 面板节点链（`admin.html:2279-2282 clNodeChain`） | 否（但**可由 nodes 解析**，见 3.6） |
| 4 | `ts` | 233,444 | 0% | 8,401,534 | 6.7% | 36.0 | 统计桶/趋势 + 面板（`calllog.go:780,1622`；`admin.html:2264` 日期过滤、`2302` 时间显示） | 否 |
| 5 | `path` | 233,444 | 0% | 6,741,724 | 5.4% | 28.9 | 仅面板（`admin.html:2300` 详情、`2246` 搜索）；98.5% 是默认值 | 否（但**可省略默认值**） |
| 6 | `model` | 233,444 | 0% | 5,751,918 | 4.6% | 24.6 | 统计核心 + 面板（`calllog.go:780`；`admin.html:2302` 模型列、`2246` 搜索） | 否 |
| 7 | `req_id` | 233,444 | 0% | 5,369,212 | 4.3% | 23.0 | 面板展开键/显示/搜索（`admin.html:2302` `clToggle` 键、`2300` 详情、`2246` 搜索） | 否 |
| 8 | `route_mode` | 233,444 | 0% | 4,902,306 | 3.9% | 21.0 | 仅面板（`admin.html:2300`）；99.997% 是 `public` | 否（但**可省略默认值**） |
| 9 | `cache_read_tokens` | 200,295 | 14.2% | 4,888,039 | 3.9% | 20.9 | 统计聚合（`calllog.go:408,2141`） | 否 |
| 10 | `completion_tokens` | 207,628 | 11.1% | 4,779,137 | 3.8% | 20.5 | 统计 + 面板（`calllog.go:406,439`；`admin.html:2300`） | 否 |
| 11 | `prompt_tokens` | 207,629 | 11.1% | 4,300,453 | 3.5% | 18.4 | 统计 + 面板（`calllog.go:405`；`admin.html:2300`） | 否 |
| 12 | `duration_ms` | 230,160 | 1.4% | 4,272,431 | 3.4% | 18.3 | 统计（吞吐守卫 `calllog.go:442`）+ 面板（`admin.html:2302,2311`） | 否 |
| 13 | `status` | 233,444 | 0% | 3,084,808 | 2.5% | 13.2 | 统计核心 + 面板（`calllog.go:400,1624`；`admin.html:2302` 徽章、`2311` 时段表） | 否 |
| 14 | `output_speed` | 40,201 | 82.8% | 1,302,135 | 1.0% | 5.6 | **仅统计**（`calllog.go:409,440` → `throughput_tps`）；面板无原始字段引用（0 处） | 否 |
| 15 | `err_msg` | 25,018 | 89.3% | 725,854 | 0.6% | 3.1 | 面板（`admin.html:2300`、`2246` 搜索） | 否 |
| 16 | `ttft_ms` | 40,201 | 82.8% | 568,482 | 0.5% | 2.4 | **仅统计**（`calllog.go:413` → `avg_ttft_ms`）；面板 0 处 | 否 |
| 17 | `stream` | 33,075 | 85.8% | 429,975 | 0.3% | 1.8 | 仅面板（`admin.html:2300`） | 否 |
| 18 | `final_status` | 4,210 | 98.2% | 75,780 | 0.1% | 0.3 | **无任何读者**（只有写入 `calllog.go:1876`） | **✅ 是** |
| 19 | `dial_ms` | 997 | 99.6% | 13,527 | 0.0% | 0.1 | **无任何读者**（只有写入 `calllog.go:1936`） | **✅ 是** |
| 20 | `unmapped` | 81 | 100% | 1,296 | 0.0% | 0.0 | 统计（`statscan.go:156`、`calllog.go:1628,1637,1809`）；面板 0 处 | 否 |
| — | *`cache_creation_tokens`* | **0** | 100% | **0** | 0% | 0 | 统计（若出现会被读 `calllog.go:407`） | 历史 0 出现 |
| — | *`input_tokens` / `cache_creation_input_tokens`* | **0** | 100% | 0 | 0% | 0 | 无（从未写入过） | 历史 0 出现 |

**复算命令**：`python3 /tmp/t52/audit.py /tmp/t52/call_log.jsonl`（输出即此表，另存 `evidence/tables.txt`）。

> 与 Lead 抽样初测的差异（逐条解释）：
> - `events` 46.7% / 269 B/行 → 全量 **36.7% / 195.7 B/行**：样本落在 9 月中下旬事件密集期（那几天 events 占 50%+），全量被 10-07 之后的裁剪摊薄。
> - `ttft_ms`/`output_speed` 1.3% → 全量 **17.2%**（40,201 行）：这两个字段 9-18 之后才写入，且只对流式请求；样本若取 9 月早期（写入前）就会接近 0。
> - `nodes` 45,364/50,000 → 全量 **95.2% 出现**；`node_names` 90.6%，与抽样基本一致。
> - `cache_creation_tokens`/`input_tokens` 0 出现 → **全量确认 0**（233,444 行里一个都没有）。

### 3.4 events 内部构成（45.69 MB 里都是什么）

| 子字段 | 引用次数 | 字节 | B/引用 | 说明 |
|---|---|---|---|---|
| `type` | 274,204 | 5,366,553 | 19.6 | 8 种：`connect_ok` 183,462、`upstream_error` 38,401、`connect_error` 28,123、`switch` 20,695、`quota_halt` 3,320、`stream_interrupt` 178、其余 25 |
| `node` | 246,086 | 10,089,526 | 41.0 | 32 位指纹 |
| `node_name` | 233,750 | 8,931,040 | 38.2 | **与顶层 `node_names` 100% 重复** |
| `detail` | 90,742 | 7,693,023 | 84.8 | 主要来自 `connect_error`(3.63 MB)/`switch`(2.94 MB)/`upstream_error`(0.86 MB)/`quota_halt`(0.26 MB) |
| `at` | 274,204 | 9,868,429 | 36.0 | 事件时间（RFC3339，28 字符） |
| 行框（`{}`/`,`/`[]`） | — | ~3,739,904 | — | |

事件类型分布（全量）：`connect_ok` 183,462（66.9%）、`upstream_error` 38,401、`connect_error` 28,123、`switch` 20,695、`quota_halt` 3,320、`stream_interrupt` 178。

### 3.5 「干净记录的 events 不落盘」已经上线（部署边界实测）

按 `CallRecord.HasIssue()`（`calllog.go:97-110`，与前端 `CL_ISSUE_TYPES` 同集合）分类：

| 类别 | 记录数 | 占比 | events 字节 |
|---|---|---|---|
| 干净记录（ok 且无异常事件） | 201,567 | 86.3% | **25,721,562（占文件 20.7%）** |
| 异常记录（fail 或含异常事件） | 31,877 | 13.7% | 19,966,913（16.0%） |

按天看 events 出现率（`evidence/perday_clean.json`）：

| 日期 | 行数 | 有 events 行数 |
|---|---|---|
| 2026-08-13 ~ 2026-10-06 | — | **100%（每天都一样）** |
| 2026-10-07 | 2,003 | 1,334 |
| 2026-10-08 | 1,691 | 548 |
| 2026-10-09 | 466 | 25 |
| 2026-10-11 | 615 | **1** |
| 2026-10-14 | 1,304 | 190 |
| 2026-10-17 | 4,687 | 1,993 |
| 2026-10-18 | 7,636 | 4,641 |

⇒ **10-07 03:25 重启后开始生效**（与 `logging configured` 时间点一致），此后干净记录确实不写 events（`clean_events_bytes = 0`）。
所以 25.7 MB 是**不可回收的存量**，不是「以后还会继续浪费」；除非做离线重写归档（不建议，见 §8）。

### 3.6 冗余与默认值（可无损压缩的空间，全部实测）

| 冗余 | 依据 | 可省字节 | 占文件 |
|---|---|---|---|
| `events[].node_name` 全部能在顶层 `node_names` 找到 | 233,750 / 233,750 = **100%** | 8,931,040 | 7.2% |
| `events[].node` 全部能在顶层 `nodes` 找到 | 246,086 / 246,086 = **100%** | 10,089,526 | 8.1% |
| `path` = `/v1/chat/completions` | 229,925 / 233,444 = **98.5%** | ≈ 6,617,000 | 5.3% |
| `route_mode` = `public` | 233,436 / 233,444 = **99.997%** | ≈ 4,900,000 | 3.9% |
| `final_status` / `dial_ms` 零读者 | §4 | 89,307 | 0.07% |
| `ts` 用 RFC3339（28 字符）可改 epoch ms（13 字符） | 每行省 ≈13 B | ≈ 3,034,772 | 2.4% |
| 全文件 gzip（离线归档） | 实测 11.3× | → 11,024,826 | **−91.1%** |

（`path`/`route_mode` 的省字节数是「省略该字段」的近似值，含引号与逗号；实测字段总字节减去默认值行占比。）

---

## 4. 每个字段的读者（`file:line`）与「零读者」判定

### 4.1 统计侧读者（`calllog.go` 聚合 + `statscan.go` 解析）

`statscan.go:19-50` 的 `slimRec` 明确只解析 **10 个字段**：`ts / model / status / prompt_tokens / completion_tokens / cache_creation_tokens / cache_read_tokens / ttft_ms / output_speed / duration_ms`（+ `unmapped`/`saw_unmapped`），并注释：

> `events / nodes / node_names / path / req_id / route_mode / err_msg / stream 从不被统计访问。`

逐字段核对（grep `.字段名` 于 `calllog.go`）：

| 字段 | 统计读者（行号） | 用途 |
|---|---|---|
| `ts` | `calllog.go:780,1622`（`hourKeyFor`） | 小时桶 / 趋势 / 日分布 |
| `model` | `calllog.go:780,790,1622` | 桶键、模型统计、`everServed` |
| `status` | `calllog.go:400,790,1624` | ok/fail 计数、`everServed` |
| `prompt_tokens` | `calllog.go:405,2138` | 模型统计求和 |
| `completion_tokens` | `calllog.go:406,439` | 求和 + 吞吐 |
| `cache_creation_tokens` | `calllog.go:407,2140` | 模型统计（历史 0 出现） |
| `cache_read_tokens` | `calllog.go:408,2141` | 模型统计 |
| `ttft_ms` | `calllog.go:413` | `avg_ttft_ms` |
| `output_speed` | `calllog.go:409,440` | `avg_output_speed`/`throughput_tps` |
| `duration_ms` | `calllog.go:442` | 吞吐守卫（物理上界） |
| `unmapped` | `statscan.go:156`、`calllog.go:1628,1637,1809` | 统计口径/回填 |

### 4.2 前端读者（`static/admin.html`，调用日志页）

> 行号基于 `static/admin.html` 2026-10-20 09:55 版本（202,719 B）；面板有并发任务在改，若行号漂移请按符号名（`clVisible`/`clRenderList`/`clRenderHour`/`clRenderNode`/`clNodeChain`/`clHasIssue`）定位。

| 字段 | 行号 | 用途 |
|---|---|---|
| `status` | `admin.html:2302,2311` | 成功/失败徽章、成功率、时段表 |
| `ts` | `admin.html:2264,2302` | 时间显示、日期过滤 |
| `model` | `admin.html:2302,2246` | 模型列、搜索 |
| `path` | `admin.html:2300,2246` | 展开详情、搜索 |
| `stream` | `admin.html:2300` | 展开详情「stream: 是/否」 |
| `route_mode` | `admin.html:2300` | 展开详情「路由: …」 |
| `req_id` | `admin.html:2300,2302,2246` | 展开键、详情、搜索 |
| `nodes` | `admin.html:2279,2320,2246` | 节点链、节点视图、搜索 |
| `node_names` | `admin.html:2279,2246` | 节点显示名、搜索 |
| `events` | `admin.html:2206,2299,2311,2334,2342` | 徽章/标签/时间线/时段表/节点表 |
| `prompt_tokens` | `admin.html:2300` | 展开详情「输入」 |
| `completion_tokens` | `admin.html:2300` | 展开详情「输出」 |
| `duration_ms` | `admin.html:2302,2311` | 耗时列/时段均值 |
| `err_msg` | `admin.html:2300,2246` | 错误文案、搜索 |
| — | — | `ttft_ms`/`output_speed`/`dial_ms`/`final_status`/`unmapped` **0 处引用** |

### 4.3 零读者字段（纯浪费）

| 字段 | 写入 | 读取 | 结论 |
|---|---|---|---|
| `final_status` | `calllog.go:1876`（`cr.rec.FinalStatus = status`） | **无**（grep `.FinalStatus` 全仓库仅此一处） | 纯浪费（4,210 行 / 75.8 KB） |
| `dial_ms` | `calllog.go:1936-1937` | **无**（grep `.DialMS` 仅写入两处） | 纯浪费（997 行 / 13.5 KB） |

> 注意：这两个字段是 10-19 刚加的「为将来观测预留」，**现在确实没人读**。按「只保留前端显示+统计真读」的口径算零读者；
> 若 Lead 认为将来要用，可保留（成本仅 0.07%）。

### 4.4 `/api/call-log` 的返回形态

- 内存环 `callLogCapacity = 2000`（`calllog.go:136`），启动时从文件尾部倒读 2000 条（`loadCallLogFromFile` `calllog.go:1411-1437`，`tailReadLastLines` `calllog.go:1346`）。
- `GET /api/call-log?limit=N` → `callLog.latest(limit)`（`calllog.go:1687-1699`），**默认 500**、上限 2000；最新在前。
- `DELETE /api/call-log` → 清空文件+内存+桶+`everServed`（`calllog.go:2415-2427`）。
- 返回的是 `CallRecord` 全量 JSON（内存里保留完整 events；只有「重启后从磁盘读回」的干净记录会因落盘时被裁而少 events——UI 本来也不显示）。

---

## 5. 前端查询能力现状

| 能力 | 现状 | 依据 |
|---|---|---|
| 看最近调用 | ✅ 最近 500（默认）/最多 2000 条 | `calllog.go:2401-2413` |
| 看历史（超过 2000 条） | ❌ **做不到**（内存环之外没有读取路径） | 同上；文件只被启动倒读 2000 行 |
| 按时间过滤 | ⚠️ 只能对**已加载的 2000 条**做前端过滤（日期下拉） | `admin.html:2239-2248`（`clVisible`） |
| 按模型/关键字过滤 | ⚠️ 同上（前端 `filter`，含 model/path/err_msg/req_id/nodes/node_names） | `admin.html:2246` |
| 按状态过滤 | ⚠️ 同上（「只看异常」复选框） | `admin.html:2244` |
| 运行日志历史 | ✅ 面板环形 5000 条（≈2 小时忙时）；`/api/logs/export` 可导出 | `logring.go:19,170,254` |

⇒ 用户「调用日志长久保存 + 前端能查」目前只满足前半句：**文件存着，前端只能看最近 2000 条**。

---

## 6. 精简方案 A：运行日志

### 6.1 现状 vs 用户期望

| 用户期望 | 现状 | 差距 |
|---|---|---|
| 即时 | ✅ slog 同步写，面板 SSE 实时 | 无 |
| 缓存一部分 | ✅ 内存环 5000 条 | 环偏小（忙时 ~2h；`/api/logs` 只回 500 条 ≈12min） |
| 超上限丢最旧 | ✅ 环满覆盖 + lumberjack 轮转 + MaxAge 删旧 | 「丢」发生在**磁盘**（用户看不到丢弃提示）；docker 副本**不丢**（无上限） |

### 6.2 选项对比（量化按当前 6.2 MB/天）

| 选项 | 实现复杂度 | 磁盘影响 | 排障能力影响 |
|---|---|---|---|
| ① 只调轮转参数：`-log-max-size=20 -log-max-backups=10 -log-max-age=7` | **极低**（改启动参数/环境变量，0 行代码） | 稳态从 ~80–105 MB → **~22 MB**（20 MB 当前 + 7 天内约 2 份 gz×1.1 MB） | 保留期 14 天 → 7 天；更早历史丢失（但面板环本来也只有 ~2h） |
| ② 只加内存环 | **已完成**（`logring.go`，5000 条） | 0（+~1 MB RAM） | 面板可看最近 5000 条；重启即丢；不影响磁盘 |
| ③ ①+②（推荐组合） | 极低 | ~22 MB | 面板实时 + 磁盘 7 天可回查；两处都保留「丢最旧」语义 |
| ④ 降日志量：修 hot-reload 自触发 + 把 per-request `request_plan`/`upstream_attempt`/`request_done` 等成功路径降到 DEBUG | 低（hot-reload 是 1 行 `if`；级别调整是几处 `slog.Info`→`slog.Debug`） | **−9.1%（hot-reload）**，若再降成功路径可 −35~40% ⇒ 6.2→**~3.7 MB/天** | hot-reload 噪声消失后**排障更容易**；per-request 细节需要 `-log-level=debug` 才能看（可按需开） |
| ⑤ 关 docker stdout 副本（`stdout=false`）或给 docker 加 `max-size=20m,max-file=3` | 低（配置/重建容器） | 去掉/限制一份等量副本（长期省 ~70 MB+） | `docker logs` 不再能直接看；排障需进容器看文件（或保留 stdout 但加 docker 轮转） |

**推荐：③+④+⑤**（参数 + 修自触发 + docker 轮转；面板环保持）。
一次性动作排序：先修 hot-reload（无损、纯收益），再改启动参数，最后给 docker 加 log-opts（需重建容器，建议与下次发布一起）。

---

## 7. 精简方案 B：调用日志

### 7.1 字段分级（按「前端显示 + 统计真读」口径）

| 级别 | 字段 | 依据 |
|---|---|---|
| **必备** | `ts` `model` `status` `prompt_tokens` `completion_tokens` `cache_read_tokens` `duration_ms` `unmapped` | 统计桶/趋势/口径 + 面板展示 |
| **必备（流式统计）** | `ttft_ms` `output_speed` | 只被统计读（`avg_ttft_ms`/`throughput_tps`），面板不显示原始值 |
| **条件必备** | `events`（仅异常记录） | 面板徽章/时间线；干净记录已在 10-07 起停写 |
| **展示可选** | `req_id` `path` `stream` `route_mode` `err_msg` `nodes` `node_names` | 只服务面板详情/搜索/节点视图 |
| **可删（零读者）** | `final_status` `dial_ms` | §4.3 |
| **可省略默认值** | `path`（98.5% 默认）`route_mode`（99.997% 默认） | 读者都有默认值（`admin.html:2300`），省略后语义不变 |
| **可去冗余** | `events[].node` / `events[].node_name` | 100% 可由顶层 `nodes`/`node_names` 解析 |
| **历史从未出现** | `cache_creation_tokens` / `input_tokens` | 0 行（可继续省略） |

### 7.2 精简后体积（公式 + 实测）

```
S0 现状（全量存量）           = 124,520,431 B
S1 = S0 − 零读者字段           = 124,520,431 − 89,307            = 124,431,124 B  (−0.07%)
S2 = S1 − events[].node_name  = S1 − 8,931,040                   = 115,500,084 B  (−7.2%)
S3 = S2 − 顶层 node_names     = S2 − 9,264,992                   = 106,235,092 B  (−7.4%)
S4 = S3 − path/route_mode 默认值省略 = S3 − (6,617,000+4,900,000) =  94,718,092 B  (−9.3%)
S5 = S4 − events@clean（仅存量，新数据已 0） = S4 − 25,721,562    =  68,996,530 B  (−20.7%)
S6 = S5 − ts 改 epoch ms（仅 v2 格式）= S5 − 3,034,772           =  65,961,758 B  (−2.4%)
gzip 归档（任意阶段）         = 实测 11.3× ⇒ 124.5 MB → 11.0 MB（−91.1%）
```

- **只做「新行」的低风险三项（S1+S2+S4）**：新数据体积 **−16.6%**（对存量等价 −29.8 MB）。
- 加 S3（顶层 `node_names` 停写，靠 API 响应时从节点池/事件解析名字）：−7.4%。
- S5 只对存量有效（25.7 MB 历史 events），新数据已实现。
- S6 需要 v2 格式（改 `ts` 编码），**与其他字段删除一样要双读兼容**，建议放到「按天分文件」一起做。

### 7.3 存储策略：按天分文件 + gzip + 保留 N 天

| 方案 | 每天 | 保留 30 天 | 保留 7 天 | 说明 |
|---|---|---|---|---|
| 现状（单文件、无压缩） | 1.35 MB（14 天均值）/ 2.18 MB（近 7 天） | 40~65 MB（且无限增长） | 9~15 MB | 当前 124.5 MB 是 68 天积累 |
| 按天文件（不压缩） | 同上 | 40~65 MB | 9~15 MB | `call_log-YYYY-MM-DD.jsonl` |
| 按天 + gzip 归档（>N 天） | — | **2.4~5.0 MB** | 1.2~2.6 MB | gzip 实测 11.3× |
| 按天 + 字段精简（S1+S2+S4）+ gzip | 1.13~1.82 MB | **2.0~4.2 MB** | 1.0~2.2 MB | 推荐组合 |

> 用户要「长久保存」，建议**保留 90 天**：按天 gzip 后 **6~15 MB**，完全可接受；保留 30 天则 2~4 MB。

### 7.4 推荐组合（按改动成本排序）

1. **零代码**：现有单文件不动，加每日定时 `gzip` 归档 ≥30 天的行？——**不行**（单文件无法只压旧行，见 §8 红线）。所以要么停机拆分，要么新写按天文件。
2. **小改（新行）**：`path`/`route_mode` 默认值省略（`omitempty`+`""` 语义）、停写 `final_status`/`dial_ms`、停写 `events[].node_name`。
   → 新数据 −16.6%；对存量无影响；读取侧零改动（读者本就有默认值/不读）。
3. **中改**：按天文件 + 后台 gzip + 保留 30/90 天；启动倒读只读最后 N 个文件；`parsedTo` 语义改为「按天文件各自的偏移」或「当前文件偏移」。
4. **可选**：`ts` epoch / `nodes` 指纹改短 ID（v2，需双读）。

---

## 8. 兼容性红线与迁移方案

### 8.1 谁在读 `call_log.jsonl`

| 读者 | 读什么 | 风险 |
|---|---|---|
| `calllog.go` 回填（`ensureBucketsUpToDate`/`parseStatsLine`） | 从 `parsedTo` 偏移起增量吸行 → 小时桶 / 模型统计 / 趋势 | **唯一统计来源**；改格式会让历史桶错乱 |
| `everServed`（task-39） | `status=="ok"` 的 `model` | 删/改 `status`/`model` 会破坏「曾经成功过」判据 |
| `unmapped` 口径（task-21/2b） | `unmapped` 键是否存在 + 值 | 删该键会把新行当「老行重算」，违反「落盘判定权威」 |
| 启动倒读 2000 行 | 全字段 | 面板最近 2000 条 |
| `DELETE /api/call-log` | 清空文件+桶+`everServed` | — |

### 8.2 红线（不可违反）

1. **只许追加，不许删改/重写历史行**：`parsedTo` 不变量是「已吸字节恰好构成文件前缀」（`calllog.go:1440-1600` 注释）。任何原地重写/截断都会让桶与文件脱节。
2. **字段只增不删**（对已写入的行）；要删只能「新行不写 + 读取侧兜底」。具体：
   - `ts`/`model`/`status`/`unmapped`：**永远不能缺**（删了统计/口径就错）。
   - `prompt_tokens` 等数值：缺省按 0 处理（现状已如此），可省略。
   - `events`：统计不读，删只影响面板历史展示（可接受）。
3. **新字段必须 `omitempty` 或带默认语义**；老行没有新键必须能反序列化（`encoding/json` 零值 + `statscan.go` 忽略未知键，已验证）。
4. **归档/压缩不得改写正在被回填读取的文件**：只能在「已吸字节之后」或停机/清空后做；gzip 归档必须保留原文件直到 `parsedTo` 越过它（或重启后按天文件独立回填）。
5. **按天分文件**要保证回填顺序与小时桶一致（按文件名日期升序），且**跨天边界的小时桶**不能重复/漏计（当前 `hourKeyFor` 用记录自带 `ts`，天然安全）。

### 8.3 迁移方案（建议分两步）

- **第一步（无格式变更，纯省字节）**：新行省略默认值 + 零读者字段 + `events[].node_name`。读取侧零改动、回填零改动。
- **第二步（按天分文件）**：
  1. 新写入路径：`call_log-YYYY-MM-DD.jsonl`（按记录 `ts` 的本地日期，与现有时区混存兼容）；
  2. 启动/回填：列出目录、按日期升序逐文件回填，`parsedTo` 改为 `(file, offset)` 二元组；
  3. 保留策略：>N 天的文件 gzip（改名 `.jsonl.gz`），>M 天删除；回填只读 `.jsonl`（gz 需要解压流式读，或保留未压缩直到回填完成）；
  4. 兼容：启动时若发现旧单文件 `call_log.jsonl`，**继续作为「最老的一天」参与回填**，不拆分不重写（安全）。

---

## 9. 前端历史查询最小改动

现状：`GET /api/call-log?limit=N` → 内存环最近 2000 条，无任何服务端过滤。

**最小方案（配合按天文件，无需索引）**：

```
GET /api/call-log?limit=200&before=<RFC3339|seq>&after=<RFC3339>&model=<substr>&status=ok|fail&q=<kw>
```

- `limit` 默认 200、上限 2000；
- 有 `before`/`after`/`model`/`status` 时走「历史路径」：从最新天文件往前倒读，逐行过滤，直到凑够 limit 或越过 `after`；
- 无过滤参数时保持现状（内存环、最新在前）——**向后兼容**；
- 性能：单天文件 ≤5 MB，倒读 2000 行用现有 `tailReadLastLines`（`calllog.go:1346`）≈1 ms；跨天最多扫 90 个文件但按 `before` 定位可 O(1) 选文件；
- **不需要索引**；若将来要「全文搜索整个 90 天」，再考虑每天文件建 1 个 `.idx`（行偏移+ts+model+status，约 60 B/行 → 90 天 ≈ 15 MB）或上 SQLite，当前不必。

前端改动：日期选择 → 传 `after/before`；「加载更多」→ 传 `before=当前最旧一条的 ts`；搜索/状态过滤 → 带参数请求服务端（或继续前端过滤最近 2000 条，二者可并存）。

---

## 10. 残留风险 / 需要 Lead 拍板的点

1. **hot-reload 自触发循环是代码缺陷**（`main.go:1240-1277`），修它超出本任务边界（只读）。它同时导致 config.json 每 5 秒被重写一次，建议列为独立小任务。
2. **docker json-file 无上限**：需改容器 `log-opts`（`max-size`/`max-file`），要重建容器；建议与下次发布一起做。
3. **「长久保存」的保留期**：30 天 / 90 天 / 永久？按 gzip 后 2~15 MB，建议 90 天；需用户确认。
4. **存量 124.5 MB 是否离线归档**：gzip 后 11 MB，但「重写历史文件」违反 §8 红线；建议**保留原文件不动**（磁盘充足时）或停机后整文件 gzip 备份到别处。
5. `events[].node` 与 `node_names` 的取舍：删 `node_name` 保 `node`（指纹）→ UI 需按顶层链映射名字；若节点已从节点池消失，名字会退化成指纹。需产品确认可接受。
6. 面板运行日志「看多久」：环 5000 条忙时仅 ~2 小时；若用户期望「最近一天」，要么调大环（5000→50000，约 +10 MB RAM），要么加「按时间导出」。

---

## 11. 复算命令（全部可重跑）

```bash
# 0) 抓快照（只读）
mkdir -p /tmp/t52 && docker exec opencode2api cat /data/call_log.jsonl > /tmp/t52/call_log.jsonl
docker exec opencode2api cat /data/opencode2api.log > /tmp/t52/run.log
sha256sum /tmp/t52/call_log.jsonl /tmp/t52/run.log

# 1) 全量字段体积表 + 按天 + events 内层（约 40 s）
python3 .research/t52/audit.py /tmp/t52/call_log.jsonl

# 2) 干净/异常记录的 events 字节 + 冗余引用（约 21 s）
python3 .research/t52/deep.py
python3 .research/t52/deep2.py

# 3) 按天 events 裁剪生效边界
python3 .research/t52/perday_clean.py

# 4) 运行日志：行数/字节/按天/级别/msg Top（<1 s）
python3 .research/t52/runlog_stats.py

# 5) 关键事实抽查
grep -m1 "logging configured" /tmp/t52/run.log
grep -c 'msg="config hot-reloaded"' /tmp/t52/run.log
docker inspect -f '{{.HostConfig.LogConfig.Type}} {{.HostConfig.LogConfig.Config}}' opencode2api
gzip -c /tmp/t52/call_log.jsonl | wc -c     # 11,024,826
```

---

## 12. 给 Lead 的三句话

1. **运行日志**：轮转/上限/环形缓冲**都有**（100 MB×7 份×14 天 + 5000 条内存环），用户期望基本满足；真正的问题是 **9.1% 的体积来自 hot-reload 自触发循环**（已复现）+ docker 副本无上限 + 面板窗口偏小。
2. **调用日志**：124.5 MB / 233,444 行 / 68 天，`events` 占 36.7% 且「干净记录已自 10-07 起不写」；剩余可省的是**默认值**（path/route_mode 9.3%）与**冗余副本**（event.node/node_name 13.5%），零读者只有 `final_status`/`dial_ms`（0.07%）。
3. **前端查历史**：现在是「存着但查不到」（只有最近 2000 条）；按天分文件 + `before/after/model/status` 参数即可，**不需要索引**。
