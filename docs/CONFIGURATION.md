# 配置说明

默认配置文件是 `config.json`。首次运行可以从示例复制：

```bash
cp config.example.json config.json
```

## 字段

### `model_alias`

模型别名映射。键是客户端请求的模型名，值是实际传给上游的模型名。

**免费模型自动映射**：上游目录中以 `-free` 结尾的免费模型无需手动配置——客户端用去掉 `-free` 后缀的名称请求时会被自动映射到对应免费上游模型（如 `deepseek-v4-flash` → `deepseek-v4-flash-free`），管理面板「模型映射」也会自动列出这些映射并跟随上游列表变动。若某免费模型已下线（如 `deepseek-v4-flash-free` 返回 401），点击自动行右侧的「隐藏」即可跳过该条。只有自定义别名（含覆盖自动映射）才需要写在这里：

```json
{
  "model_alias": {
    "my-fast-model": "deepseek-v4-flash-free"
  }
}
```

### `reasoning_effort_map`

把客户端传入的 `reasoning_effort` 映射到上游可接受的值。

```json
{
  "reasoning_effort_map": {
    "minimal": "low",
    "medium": "medium",
    "high": "high"
  }
}
```

### `force_disable_thinking`

设为 `true` 时，服务会尽量禁用 thinking/reasoning，并从返回中移除 reasoning 内容。

### `socks5_proxies`

SOCKS5 代理列表。

```json
{
  "socks5_proxies": [
    {
      "name": "local",
      "addr": "127.0.0.1:1080",
      "username": "",
      "password": ""
    }
  ]
}
```

### `active_socks5`

启用的代理。

- 空字符串：直连
- 某个 `addr`：固定使用该代理
- `__round_robin__`：在多个代理之间轮询

### `socks5_sticky`

会话粘性出口（仅作用于静态 `socks5_proxies` + `__round_robin__` 轮询模式）。

- 不填或 `true`（默认）：同一账号（按 API token）或同一客户端会话（按 Claude metadata 的 session_id / 请求 user 字段）固定走同一出口代理。上游免费层的 prompt 缓存按出口 IP 隔离，随机轮换出口会导致缓存命中率归零；固定出口后缓存持续累积（上游实测固定出口命中 99.8% vs 随机轮换 ~0%）。不同会话之间仍分散到不同出口。
- `false`：纯轮询，每次请求随机换出口。
- 上游 429/5xx/连接错误重试前自动切断当前绑定、换下一个出口重试；代理配置变化时清空全部绑定。
- 节点池（订阅/webshare）路径本身是全局粘性（单 active 节点），不依赖此开关。

```json
{
  "socks5_sticky": true
}
```

### `prompt_cache_retention`

注入上游请求的 prompt 前缀缓存保留时长。zen 网关默认只保留 ~5 分钟（in_memory），agent 任务间歇即过期；注入后拉长到一天。

- 不填或 `"24h"`：注入 `prompt_cache_retention: "24h"`
- `"in_memory"`：注入 `"in_memory"`（上游默认行为）
- `"off"`：不注入

客户端 `extra_body` 中显式传入的值优先于注入默认值。

```json
{
  "prompt_cache_retention": "24h"
}
```

### `cache_control_breakpoints`

是否向支持该字段的上游模型注入 Anthropic 风格的 `cache_control` 断点（`{"type":"ephemeral","ttl":"1h"}`），显式标记缓存前缀边界并拉长 TTL。

- 缺省或 `true`：注入（对支持的上游提升缓存命中；GLM/Zhipu 模型会拒绝该字段，自动跳过）
- `false`：不注入

运行时观测：`stats.json` 中每个模型的 `cache_read_tokens` / `cache_created_tokens` 聚合（来自上游 `cache_read_input_tokens` / `cache_creation_input_tokens`、DeepSeek 的 `prompt_cache_hit_tokens` 或 `prompt_tokens_details.cached_tokens`）。Claude 协议面 `input_tokens` 已对齐 Anthropic 语义——扣除缓存读取部分（input/read/creation 互斥），客户端不会重复计费。DeepSeek 的 `prompt_cache_miss_tokens` 是普通未命中输入，不计为缓存写入。

```json
{
  "cache_control_breakpoints": true
}
```

### `text_only_models`

只接受文本输入的上游模型前缀列表。请求解析到这些模型时，消息里的图片/文档内容会被**静默降级**为文本标注（`[image attached]` / `[document attached]`）后继续转发，而不是把无法处理的多模态内容交给上游报错。

匹配是**大小写不敏感的前缀匹配**：一个前缀覆盖该模型的所有变体。例如 `"deepseek"` 同时匹配 `deepseek-v4-flash` 和 `deepseek-v4-flash-free`。

不填时默认 `["deepseek"]`；显式设置（即使是空数组）会替换默认值。此字段同样作用于 Chat、Responses、Claude 三条协议面。

```json
{
  "text_only_models": ["deepseek"]
}
```

### `socks5_paid_direct`

控制**带 key / 付费**上游请求是否绕过 SOCKS5。

- 不填或 `false`（默认）：只要配置了 `active_socks5`，public 与带 key 请求都走代理
- `true`：带 key 请求直连；仅 public / 免费层走代理（旧行为）

```json
{
  "active_socks5": "127.0.0.1:1080",
  "socks5_paid_direct": false
}
```

### `subscriptions` / `manual_nodes`（订阅节点池）

节点池（v0.14+）把上游请求通过订阅节点转发（进程内直连，无需 mihomo 子进程）。**池非空时所有请求都走池；池为空时回退旧 SOCKS5 逻辑。**

```json
{
  "subscriptions": [
    {
      "name": "my-sub",
      "url": "https://example.com/sub?token=xxx",
      "interval_hours": 12
    }
  ],
  "manual_nodes": [
    {
      "name": "hk1",
      "protocol": "vless",
      "address": "1.2.3.4",
      "port": 443,
      "user_id": "uuid-here",
      "flow": "",
      "sni": "example.com",
      "reality": {
        "public_key": "xxx",
        "short_id": "xxxxxx",
        "spider_x": "/"
      }
    }
  ]
}
```

- 支持协议：`vless`（含 reality）、`ss`、`hysteria2`、`anytls`、`socks5`（`socks5://` URI 走账户限制，clash 配置按原样处理）。
- 订阅内容支持：Clash YAML（只读 `proxies`）、整条 base64 包裹、每行一条 `vless://`/`ss://`/`hysteria2://`/`anytls://`/`socks5://` URI。
- 订阅 URL 需要认证时：`url` 可直接附 query；base64 包裹的订阅会自动解码。
- 免费额度耗尽自动切换：请求遇 `FreeUsageLimitError`/`insufficient_quota`/`credits_error`/`billing_error`（`error.type`）或 `free usage limit`/`quota`/`insufficient`/`limit exceeded`（`error.message`）时，标记当前节点为 **已耗尽**，透明切换下一个节点重试；单个请求最多切换 `max_quota_node_switches`（默认 5）次（配额预算与重试上限独立：循环上限 = 重试 3 次 + 配额预算 5 次，普通重试仍封顶 3 次）。**403 无签名视为耗尽、429 无签名不视为耗尽**。已耗尽节点在配额冷却到期后自动恢复（标记清除、重新参与路由）；健康探测失败标记的 **故障** 节点每分钟复探，探测成功即恢复；调度巡检只探测可用节点，不打扰已耗尽节点。
- 判定可定制；`error_types` / `message_keywords` 为空数组、`max_quota_node_switches` 为 0 时按默认值处理（面板文本框未配置时回显默认值）：

```json
{
  "quota_error_signals": {
    "error_types": ["FreeUsageLimitError", "insufficient_quota"],
    "message_keywords": ["free usage limit", "quota", "limit exceeded"]
  },
  "max_quota_node_switches": 5,
  "node_cooldown_exhausted_hours": 1,
  "node_cooldown_dead_minutes": 1
}
```

- 节点状态持久化在配置同目录 `proxy_state.json`（运行时按订阅指纹学习、重启不丢失）；节点客户端与订阅缓存缓存在 `.subscriptions/` 目录。
- 管理面板新增「节点池」卡片：查看状态/冷却、手动切换节点、解除耗尽标记、重新加载订阅。

### `webshare`（Webshare 代理池）

通过 [webshare.io](https://webshare.io) 代理列表 API v2 拉取代理并转为 SOCKS5 节点，并入节点池（与订阅共用池、轮询与健康检查）。

```json
{
  "webshare": [
    {
      "name": "webshare-main",
      "api_key": "your-webshare-api-token",
      "mode": "direct",
      "proxy_url": "http://127.0.0.1:7890",
      "update_interval_hours": 12
    }
  ]
}
```

- `api_key`：在 webshare 面板「API Keys」生成，请求头为 `Authorization: Token <key>`。
- `mode`：`direct`（默认）或 `backbone`，对应 API 的 `mode` 参数。
- `proxy_url`：可选。拉取 API 时走的代理，支持 `http://`/`https://`/`socks5://`（如 `http://127.0.0.1:7890`）；留空则使用进程环境变量 `HTTP(S)_PROXY`（即默认 Go 行为，通常直连）。本机到 webshare 网络不佳时建议配置。
- `update_interval_hours`：刷新间隔，0/缺省 = 24h。
- 只导入 `valid != false` 的代理；每页最多 100 条自动分页拉全。
- 节点名形如 `webshare-main::US-38.153.152.244:9594`，与订阅节点一样参与健康探测、配额切换和耗尽标记。

## 管理面板

打开 `http://127.0.0.1:8000/` 可进入管理面板。面板可以修改配置、刷新模型和查看 token 统计。

默认管理密码是 `123456`，生产部署必须修改：

```bash
./opencode2api -password "your-strong-password"
```

`GET/POST /api/config` 额外返回/接受运行时日志字段（不写入 `config.json`）：

- `log_level`：`debug` / `info` / `warn` / `error`
- `log_bodies`：是否在 Debug 下记录 body 形状摘要

## 日志与排障

默认写入 `opencode2api.log` 并由 lumberjack 按大小轮换；同时写 stdout。

关键字段：

| 事件 | 用途 |
|------|------|
| `request_plan` | 协议决策：模型、auth_mode、thinking、reasoning_effort、stream |
| `upstream_attempt` / `upstream_result` | 上游重试与回退链 |
| `stream_result` | 流式结果摘要；`empty_reply=true` 时为 Warn |
| `request_result` | 非流式结果摘要 |

密钥字段（`authorization` / `token` / `sk-…`）会被脱敏，永不落完整密钥。
