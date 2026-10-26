# task-69 本地 fixture 诚实说明（web/fixtures/，已 gitignore）

生成脚本：`.research/t69/gen_fixtures.py`（可复跑，随机种子固定 20261021）。

| fixture | 真实来源 | 造的字段 |
| --- | --- | --- |
| `aggregate_today.json` | totals 366 / ok 344 / fail 22、24 个小时桶、2 个模型的 requests 与 tokens：全部取自仓库内真实快照 `web/fixtures/stats_today.json` | 每模型 fail 拆分、p95、节点分布、错误分类、延迟分位、by_node/by_error、`cache` |
| `aggregate_7d.json` | totals 27,223 / unmapped 730 / 7 天桶 / 9 个模型的 requests 与 tokens：来自真实快照 `stats_7d.json` + `trends_7d.json` | 每模型 fail 拆分（总额 7,061 与 trends 一致）、p95、节点/错误/延迟、`cache` |
| `aggregate_30d.json` | totals 157,769 / fail 11,120 / unmapped 1,352 / latency.max 2,832,125：来自 task-66 生产快照实测（见 `docs/research/log-aggregate-api.md` §12.1） | 逐日曲线、模型/节点/错误分布、p50/p95/p99、`cache` |
| `aggregate_180d.json` | totals 232,263 / fail 23,633 / unmapped 1,391 / latency.max 2,832,125：同上（生产快照实测） | 逐日曲线、模型/节点/错误分布、p50/p95/p99、`cache` |
| `call_log.json` | **全部合成**（450 条，按生产画像配比：~87% ok、~12% fail、2% unmapped，1–3 节点链、真实 CallRecord 字段） | 全部 |

所有聚合 fixture 都满足真实接口不变量：`Σby_model.requests == totals.requests`、
`Σby_node.requests == totals.requests`、`Σby_error.count == totals.fail`、
`Σtimeseries.requests == totals.requests` 且逐桶 `ok+fail==requests`。
`cache{hit,ttl,range}` 为合成字段（真实 aggregate 接口**不返回** cache；页面缺省按 20s TTL + 页面缓存标注）。
