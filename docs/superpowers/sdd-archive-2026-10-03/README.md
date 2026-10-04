# 测试文件归档 — 2026-10-03 日志性能改造

本目录是 `test` 分支 `*_test.go` 的**版本化副本**。

## 为什么需要它

仓库约定 `.gitignore:26` 排除 `*_test.go`（用户 2026-09-19 的 commit `c342ea1`
「测试文件仅保留本地，不再入库」的明确决定）。该约定本身没问题，但有一个后果：

**测试文件的修复也不入库。**`git clean`、换机器、重建工作区，
这些测试会全部消失且无人察觉。

本轮日志性能改造中，这些测试覆盖了七类回归，其中四类在生产上是**静默数据损坏**：

| 测试 | 守住的回归 |
|---|---|
| `calllog_tail_test.go`（8 例） | 启动尾部倒读的边界：截断、撕裂尾行、单行超块、别名 |
| `statscan_test.go`（7 例） | 扫描器：字符串感知的容器跳过、转义、饱和值、键免分配 |
| `bucket_test.go` | 小时桶：seeded 交接、短写回滚、`clear` 与在途 append |
| `calllog_test.go` | 落盘裁 Events 不污染内存环、裁剪不改调用方记录 |
| `proxy_node_test.go` | 健康探测重复失败降 Debug 不刷屏 |
| `trends_test.go` | 跨午夜竞态、等长原地改写可证真红 |
| `backfill_timing_test.go` | backfill 是 O(增量) 而非 O(文件) |

## 怎么用

```sh
cp docs/superpowers/sdd-archive-2026-10-03/*_test.go .   # 恢复到工作区
go test ./...
```

## 不是什么

这**不是**要推翻 `.gitignore` 的约定——测试仍然不入库、仍然不在 CI 里跑。
这只是把关键回归用例**留档**，让它们不会在某次 `git clean` 后无声消失。

若将来要真正入库，删掉本目录并移除 `.gitignore:26` 即可。
