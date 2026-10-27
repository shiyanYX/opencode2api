# 真实日志回放夹具

来源：生产 `call_log`（10-26 / 10-27），只保留 `(毫秒时间戳, 结果类别)`，不含请求内容、模型输出或任何用户数据。

| 文件 | 窗口（Asia/Shanghai） | pair | 429 | 2xx |
|---|---|---|---|---|
| `pair_653e_mimo.jsonl` | 10-27 00:15–00:45 | `653e51eb…` × `mimo-v2.6-flash-free` | 1,106 | 182 |
| `pair_653e_deepseek.jsonl` | 10-27 00:15–00:45 | `653e51eb…` × `deepseek-v4.1-flash-free` | 0 | 5 |
| `pair_41d_mimo.jsonl` | 10-27 00:15–00:45 | `41d39652…` × `mimo-v2.6-flash-free` | 402 | 0 |
| `pair_4ad_mimo.jsonl` | 10-26 18:00–18:10 | `4ad1982e…` × `mimo-v2.6-flash-free` | 20 | 21 |

一行一个事件：`{"t":<unix ms>,"c":"2xx|429|403|5xx|other"}`。
`2xx` 取该请求**最后命中的节点**（`nodes` 末位）+ `duration_ms`；失败取 `events[].at`。
同一毫秒内事件顺序不唯一，所以回放结果允许 4–5 的差异；断言用 `≤8`。
