# 日志与数据到底存了什么（`/data` 全量清单）

> 用户 2026-10-21 提问：「日志保存到底保存了哪些内容，我都不知道」
> 本文件是**如实盘点**：每个文件里存什么、多大、留多久、敏不敏感、谁在读。
> 生成时间 2026-10-21 08:0x，全部数字来自运行中的生产容器实测。

**一句话结论：存的是「一次请求的过程与结果」（时间、模型、走了哪个节点、耗多久、成功与否、token 数），
不存你和模型说过的任何一句话。**

---

## 1. 调用日志（最核心，也是面板统计的唯一数据源）

### 文件布局

| 文件 | 说明 |
|---|---|
| `/data/call_log.jsonl` | 历史总文件（233,692 行 / 124.6 MB），**冻结**为「第 0 段」，不再写入、永不删除 |
| `/data/call_log/YYYY-MM-DD.jsonl` | 每天一个新文件，逐条追加（当天明文） |
| `/data/call_log/YYYY-MM-DD.jsonl.gz` | 超过 2 天的段自动 gzip |
| 保留期 | **永久**（2026-10-21 08:04 起，`-calllog-retain-days=0`）；此前是 180 天后删除 |

### 一条记录存这些（生产真实样本，12 个字段）

```json
{"req_id":"3du02ydi3mse","ts":"2026-10-20T23:15:34.923+08:00","model":"deepseek-v4.1-flash-free",
 "nodes":["f82d885d319e0c99c4747b13f096e25a"],"node_names":["1::AT-45.39.96.13:5426"],
 "status":"ok","prompt_tokens":721,"completion_tokens":1,"duration_ms":12710,"ttft_ms":12707,
 "output_speed":283.83,"cache_read_tokens":4096,"events":[...]}
```

| 字段 | 含义 | 示例 | 说明 |
|---|---|---|---|
| `req_id` | 请求编号 | `3du02ydi3mse` | 随机生成，不含含义 |
| `ts` | **开始**时间 | `2026-10-20T23:15:34.923+08:00` | 毫秒精度 |
| `model` | 请求的模型名 | `deepseek-v4.1-flash-free` | |
| `nodes` / `node_names` | 这次请求**试过哪些出口节点**（指纹 + 显示名） | `1::AT-45.39.96.13:5426` | 含节点 IP |
| `status` | 结果 | `ok` / `fail` | |
| `prompt_tokens` | 上游口径输入 token | `721` | 含缓存，面板会换算 |
| `completion_tokens` | 输出 token | `1` | |
| `cache_read_tokens` | 命中缓存的 token | `4096` | 为 0 时不写 |
| `duration_ms` | 总耗时 | `12710` | |
| `ttft_ms` | 首字到达耗时 | `12707` | |
| `output_speed` | 输出速度（token/秒） | `283.83` | |
| `unmapped` | 是否是「未映射模型」的请求 | `false` | 用于把不合法请求排除出统计 |
| `events` | 这一次请求经历的事件序列 | 见下 | 失败时才有内容 |

### `events` 里存什么（失败请求的诊断线索）

样本：
```json
{"type":"connect_error","node":"8ffdd871…","node_name":"1::DE-45.151.163.29:5782",
 "detail":"Post \"https://opencode.ai/inference/v1/chat/completions\": context canceled",
 "at":"2026-10-20T18:00:38.806+08:00"}
```
事件类型只有几类：`connect_ok` / `connect_error` / `switch`（换节点）/ `upstream_error` / `quota_halt`。
`detail` 里是**网络错误文本或 HTTP 状态码**（如 `http 400`），不含对话内容。

### 明确**不存**的东西

- ❌ 你的 prompt / messages 全文
- ❌ 模型的回复正文
- ❌ 请求头（包括 `Authorization`）
- ❌ 你的客户端 IP
- ❌ 文件/图片附件内容

### 成本

| 指标 | 数值 |
|---|---|
| 单条原始 | 347 字节（瘦身改造前 533 字节） |
| 单条压缩后 | 约 50 字节 |
| 当前日均请求 | 约 2,400 条 → 约 120 KB/天（压缩后） |
| **每年** | **约 43 MB** |
| 10 年 | 约 430 MB |

---

## 2. 运行日志（排障用，和调用日志不是一回事）

| 项 | 值 |
|---|---|
| 文件 | `/data/opencode2api.log`（当前 74.5 MB / 377,355 行）+ 轮转文件与 `.gz` |
| 轮转策略 | 单文件 100 MB → 轮转，**最多保留 7 个 + 14 天**，自动 gzip |
| 内容 | 服务自身在做什么 |

实际出现过的消息类型（tail 2 万行统计）：

| 次数 | 消息 | 说明 |
|---|---|---|
| 2271 | `config hot-reloaded` | 配置热加载（**这是修复前的旧记录**；修复后本次启动 0 次） |
| 718 | `upstream error` | 上游报错，含 `request_id`/`model`/`status`/**上游错误体** |
| 713 | `node health probe failed` | 节点探测失败（连续失败未达阈值） |
| 710 | `node marked dead by health check` | 节点判死 |
| 256 | `node state changed` | 节点状态变化 |
| 240 | `models auto-refreshed` / `go catalog auto-refreshed` | 模型列表刷新 |
| 178 | `subscription refresh failed` | 订阅刷新失败（**修复前每 10 分钟一条的假告警**） |
| 165 | `node health check done` | 每轮体检汇总 |
| 13 | `session initialized` / `project initialized` / `opencode version` | 启动信息 |
| 12 | `free models refresh failed` / `go catalog refresh failed` | 刷新失败 |

> ⚠️ **唯一可能夹带外部文本的地方**：`upstream error` 会记录上游返回的错误 body。
> 实测样本形如 `body="{\"error\":{\"message\":\"Your request resulted in more tokens than this model's maximum context length allows…\"}}"`，
> 即**只有错误信息**，不含对话内容。若上游某天在错误体里回显请求内容，会进到这里 —— 这是隐私上唯一的边缘风险。

| 项 | 值 |
|---|---|
| 保留 | 14 天 / 100 MB × 7 个，**自动清理**，不需要手管 |
| **保留策略决定** | **用户 2026-10-21 裁决：运行日志保持现状（14 天），不永久保留。** 理由：体积虽是调用日志的 3.7 倍（约 160 MB/年），但价值密度低（大量 INFO 噪声）；真正的请求级现场在调用日志里且已永久保存，回查事故时可从调用日志反推 |
| 是否影响面板 | 否（面板的「运行日志」视图读的是内存环 5,000 条 + 该文件尾部） |

> 口径提示：**调用日志永久（可查每一次请求）＋ 运行日志 14 天（可查最近两周服务动作）**。
> 因此「三周前服务为什么重启/为什么刷订阅」这类问题，届时只能靠调用日志的空白/错误分布去反推，运行日志已过期。

---

## 3. 配置与状态文件（**这些才含敏感信息**）

| 文件 | 大小 | 内容 | 敏感级别 |
|---|---|---|---|
| `config.json` | 1.5 KB | `api_key`、订阅 URL、Webshare 的 api_key、模型映射、冷却参数 | 🔴 **高**（含凭据） |
| `.subscriptions/*.txt` | 4 个 / 约 64 KB | 订阅原文（base64 的节点链接，**含机场节点服务器与口令**） | 🔴 **最高** |
| `proxy_state.json` | 78 KB | 219 个节点的指纹、状态、冷却截止时间、最后一次错误文本 | 🟡 中（含机场节点 IP） |
| `stats.json` | 4 KB | 累计统计快照（`total_requests: 625207` 等） | 🟢 低 |
| `free_models_cache.json` | 0.4 KB | 免费模型列表缓存 | 🟢 低 |

---

## 4. 汇总表

| 文件/目录 | 当前大小 | 保留策略 | 含对话内容 | 敏感 |
|---|---|---|---|---|
| `call_log.jsonl` + `call_log/` | 124.6 MB + 15 KB | **永久**（今天改的） | ❌ | 🟡 节点 IP |
| `opencode2api.log` + 轮转 | 74.5 MB | 14 天 / 700 MB 上限 | ❌（仅上游错误体） | 🟢 低 |
| `config.json` | 1.5 KB | 永久 | ❌ | 🔴 凭据 |
| `.subscriptions/*.txt` | 64 KB | 永久 | ❌ | 🔴 节点凭据 |
| `proxy_state.json` | 78 KB | 永久（覆盖写） | ❌ | 🟡 节点 IP |
| `stats.json` / `free_models_cache.json` | 4.4 KB | 永久（覆盖写） | ❌ | 🟢 低 |

**合计约 200 MB，其中 95% 是历史调用日志与运行日志。**

---

## 5. 三个可调开关

| 开关 | 默认 | 现在 | 含义 |
|---|---|---|---|
| `-calllog-retain-days` | 180 | **0（永久）** | `<=0` = 永不删除 |
| `-calllog-gzip-after-days` | 2 | 2 | 超过 N 天压成 `.gz` |
| `OPENCODE2API_LOG_LEVEL` | info | info | `debug` 会显著增加运行日志量（且更啰嗦），排障后调回 |

---

## 6. 两条必须提醒的事

1. **永久保存 ≠ 不会丢**：数据在单机 Docker volume 里（宿主机 `/var/lib/docker/volumes/compose_opencode2api-data/_data`）。
   硬盘坏了就全没了。真要"永久"，需要每周把 `call_log/` 目录导出到别处（NAS / 另一块盘 / 网盘）。
2. **保存 ≠ 能查**：面板统计窗口目前**最大 180 天**，是代码里写死的。数据即使永久保存，
   面板也不会自动显示两年前的统计 —— 那需要额外做「时间窗扩展 + 启动索引优化」。
