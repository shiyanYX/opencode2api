# 测试文件归档 — 2026-10-03 日志性能改造

本目录是 `test` 分支 `*_test.go` 的**版本化副本**。

## 为什么需要它

仓库约定 `.gitignore:26` 排除 `*_test.go`（用户 2026-09-19 的 commit `c342ea1`
「测试文件仅保留本地，不再入库」的明确决定）。该约定本身没问题，但有一个后果：

**测试文件的修复也不入库。**`git clean`、换机器、重建工作区，
这些测试会全部消失且无人察觉。

本轮日志性能改造中，这十六份测试覆盖了十六类回归，其中六类在生产上是
**静默数据损坏**（数字悄悄错、不报错）：⬛ 标记的就是。

| 测试 | 例数 | 守住的回归 |
|---|---:|---|
| `calllog_tail_test.go` | 8 | 启动尾部倒读的边界：截断、撕裂尾行、单行超块、别名 |
| `statscan_test.go` | 7 | 扫描器：字符串感知的容器跳过、转义、饱和值、键免分配 |
| `bucket_test.go` | 29 | ⬛ 小时桶：seeded 交接、短写回滚、`clear` 与在途 append |
| `calllog_test.go` | 2 | 落盘裁 Events 不污染内存环、裁剪不改调用方记录 |
| `proxy_node_test.go` | 2 | 健康探测重复失败降 Debug 不刷屏 |
| `trends_test.go` | 3 | 跨午夜竞态、等长原地改写可证真红 |
| `backfill_timing_test.go` | 1 | backfill 是 O(增量) 而非 O(文件) |
| `boundary_test.go` | 5 | 窗口边界：`today` 半桶、最小范围、空/缺失文件 |
| `rolling_today_test.go` | 8 | 滚动窗口跨午夜重算与日桶索引不重不漏 |
| `backfill_attrib_test.go` | 1 | 吸收的行数与进桶的条数同源提交 |
| `backfill_finish_test.go` | 7 | ⬛ 收尾交接：并发 append 不重复计、文件真没了才清桶、乱序 `+=` 进已闭合的小时桶 |
| `clear_log_test.go` | 4 | ⬛ DELETE 清空后不返回残影，且 stats 端点仍响应、清理结果落盘 |
| `conservation_test.go` | 15 | ⬛ `ΣN == absorbed - unabsorbed` 守恒：桶被改坏要能发现并重建，合法不等不误报，重建有退避 |
| `stats_window_test.go` | 10 | 窗口语义：范围外排除、空 range 归一到 today、速度/TTFT 的记数条件与 `recordModelSpeed` 一致 |
| `task8_bucket_query_test.go` | 11 | ⬛ 查询侧只读桶不再重扫日志；新桶结果与旧全量扫描**逐字段**对拍；同输入两次调用位级一致 |
| `timezone_mix_test.go` | 4 | ⬛ 混存时区（185,722 行 `+08:00` + 490 行 `Z`）必须折叠进同一个本地小时桶，且不该合的不合 |

### 故意**不**归档的四个

`bench_test.go` / `fmt_test.go` / `race_flag_test.go` / `race_noflag_test.go` —
基准与 `-race` 构建开关的临时产物：前两个在基线仓库就不合 gofmt
（`ci.yml` 的 Check formatting 会拦），且不构成回归保护，留着只会让这份归档
自身变成 CI 的噪声。

⚠️ **但后两个必须手工补上**，否则恢复出来的测试**编译不过**：
`backfill_timing_test.go:109,140` 用了 `raceEnabled`，而它只由那两个
build-tag 文件定义（`//go:build race` / `//go:build !race` 各定义一个 `const`）。
从归档恢复后把它们放回工作区即可（6 行，gofmt 合规）：

```go
// race_flag_test.go
//go:build race

package main

const raceEnabled = true
```

```go
// race_noflag_test.go
//go:build !race

package main

const raceEnabled = false
```

### 为什么时区那份不用「测试内改 `time.Local`」

`timezone_mix_test.go` 的两种时区写法是**硬编码**的字符串常量，
不是由 `time.Now().Local()` 现场派生的。计划原文（Task 11 Step 1）用后者，
并附警告：TZ=UTC 时两种写法会退化成同一个字符串、测试恒绿。改成硬编码后
退化成因被掐掉，任何宿主机时区下都有鉴别力，且不需要「必须用某个 TZ 跑」的约定。

**不要**把它改成 `time.Local = time.FixedZone(...)`：本包（mihomo / quic-go / cgo）
里任何对 `time.Local` 的写入都会让 `go test -race ./...` 在进程退出时段错误，
`GOTRACEBACK=all` 也拿不到 Go 栈（go1.26.5 实测）。理由与实测见
`.superpowers/sdd/2026-10-03-log-perf/timezone-report.md`。

## 怎么用

```sh
cp docs/superpowers/sdd-archive-2026-10-03/*_test.go.txt . && for f in *_test.go.txt; do mv "$f" "${f%.txt}"; done  # 恢复到工作区
# 再手工补上 race_flag_test.go / race_noflag_test.go（见上一节），否则编译不过
go test ./...
```

实测：从归档恢复 16 份测试后 `go test ./...` 全绿（go1.26.5）。

## 不是什么

这**不是**要推翻 `.gitignore` 的约定——测试仍然不入库、仍然不在 CI 里跑。
这只是把关键回归用例**留档**，让它们不会在某次 `git clean` 后无声消失。

若将来要真正入库，删掉本目录并移除 `.gitignore:26` 即可。
