# 设置项必要性研究（task-53）：先回答「出口设置到底要不要」，再定精简后的真实设置集

> 用户原话（10-20）：**「走哪个出口：直连还是走代理；付费请求走不走代理。这个设置真的需要吗？不走代理我们的节点池做出来干什么的。再去研究到底哪些设置是真正需要的，研究完了再重新弄页面方案，这个分页格式现在看着也不行。」**
> ⇒ 阶段一（本文）**只做研究、不动 HTML**；「分区页（3 页签）」方案**作废**，阶段二在精简后的真实设置集上重画。
>
> 取证：分支 `test`；面板 `static/admin.html`（md5 `4fe4f101470f9284ea7d1edbbc04d7e5` / 2496 行）只读；后端 `main.go` / `egress.go` / `logring.go` 只读；生产只读副本：`/data/config.json`（1479 B）、`/data/call_log.jsonl`（**233,452 行 / 118.8 MiB**，10-20 13:03 抓取）、`/data/proxy_state.json`（215 节点）、`/data/opencode2api.log`（73 MB）。

---

## 0. 结论速览

| 问题 | 结论 |
| --- | --- |
| **用户那一问（§1）** | **这个设置不需要，应当删掉。用户的直觉是对的。** 生产配置里出口三字段**一个都没配**；代码里只要节点池非空就**永远走节点池**，静态 SOCKS5 分支根本不可达；生产 233,452 条请求里走节点的占 **95.21%**，其余 4.79% 里"真·直连成功"只有 **1.91%**，且集中在节点池故障的两天（10-18 至 10-19 的 1.53% / 21.30% = **全部冷却后的自动兜底**，不是用户选的）。判定：**D 可以删（4 项整组）**，但**字段保留兼容读取、只从 UI 消失**。 |
| ⚠️ 必须纠正的一个前提 | **`route_mode` 不是"出口"**，它是**鉴权档位**（`public` / `auto` / `zen` / `go`，`main.go:2576-2582` `AuthRouteMode`，`logging.go:295` `authModeString`）。拿它统计"有多少请求直连"会得出错结论。真正的出口判定看 **`nodes[]` 是否为空**（§1.3）。 |
| 25 项怎么分 | **A 必须保留 12 · B 可合并 2 · C 可写死 5 · D 可删 4 · E 降级只读 3**（§2、§3） |
| 精简后用户真要改几项 | **8 项**（api_key、订阅/Webshare 两条来源、耗尽冷却、故障冷却、最多换几次、健康检查间隔、日志级别）＋ 模型页 5 项（映射表/禁用或改名/推理力度/强制禁思考）；其余全部"写死 / 只读 / 删"（§3） |
| 阶段二页面规模 | 设置页 ≤ **9 个可编辑控件**、分 **3 组**（接入 / 节点从哪来 / 节点不好用时）；模型类 5 项单开；排障 1 项 + 危险区（§5） |

---

## 1. 用户那一问：出口设置（直连 vs 代理 / 付费是否直连）到底要不要

### 1.1 这个设置现在是哪几个字段、面板在哪、默认值 / 生产值

| 字段（JSON key） | 面板位置 | 语义 | 默认值 | **生产实际值** |
| --- | --- | --- | --- | --- |
| `socks5_proxies`（数组） | `static/admin.html:2084` `renderSocks5Table()`，表 `#socks5Table` | 静态 SOCKS5 代理列表（与节点池是**两套**东西） | 空 | **不存在**（config.json 无此键） |
| `active_socks5`（字符串） | `:2088` `renderSocks5Select()`，`<select>` 第一项就是「**直连（不使用代理）**」（`:805` 同卡片「启用代理」） | `""`=直连 / `"__round_robin__"`=轮询 / 具体地址=固定代理（`main.go:227` 定义的 `socks5RR`，`egress.go:157` 使用） | `""` | **不存在** |
| `socks5_paid_direct`（bool） | `static/admin.html:805`「**带 key / 付费请求直连**」下拉（`走代理（默认）` / `直连`） | 仅当**鉴权档位 = 付费**时绕过代理（`egress.go:121-127`） | `false` | **不存在** |
| `socks5_sticky`（*bool） | 面板没有这项（仅配置文件） | 静态 SOCKS5 的会话粘性（同会话固定出口→缓存命中） | `true` | **不存在**（默认 true） |

> 生产 `config.json` 的全部 12 个键：`model_alias`(空) / `hidden_free_aliases`(1) / `reasoning_effort_map`(4) / `force_disable_thinking`(false) / `api_key`(27 字符) / `subscriptions`(2) / `webshare`(3) / `quota_error_signals`(2 组) / `node_cooldown_exhausted_hours`(5) / `node_cooldown_dead_minutes`(5) / `max_quota_node_switches`(5) / `node_health_interval_minutes`(15)。**出口四件套一个都没有。**

### 1.2 出口到底是怎么决定的（代码只有 4 级，静态代理是第 3 级死角）

`egress.go:116-145` `EgressClient.Get()` 是唯一出口决策点，顺序固定：

```go
// egress.go:121-127  ① 付费档 + paidDirect → 直连（快速路径）
if tier == TierPaid && paidDirect { return EgressResult{Client: eg.direct, Direct: true} }
// egress.go:130-138  ② 节点池非空 → 从池里挑一个；★全部冷却 → 直连兜底
if proxyPool.nodeCount() > 0 {
    n := proxyPool.pickForRegion(r.ForceSwitch, r.RequiredRegion)
    if n != nil { return ... NodeFP: n.Fingerprint }
    return EgressResult{Client: eg.direct, Direct: true}   // 全部冷却中 → 直连
}
// egress.go:141       ③ 静态 socks5（只在节点池为空时可达）
return eg.getSocks5Client(r)
```

⇒ **只要节点池非空（生产 215 个节点），`socks5_proxies` / `active_socks5` / `socks5_paid_direct` / `socks5_sticky` 这四项全部不可达**（付费直连那一级除外，但见 1.4：付费档几乎不存在）。所以面板上那个「启用代理：直连 / 走代理」的下拉，在真实使用中**没有一条路径会读到它**。

**而且 `socks5_sticky` 的承诺在节点池模式下也是空的**：`Get()` 的节点池分支（`:130-138`）压根不读 sticky，只有静态分支 `getSocks5Client()`（`:145-165`）读。⇒ 面板若宣称「会话固定出口、提高缓存命中」，在节点池模式下不成立。

### 1.3 生产日志实测：多少请求真的没走节点？

**判定方法**：`CallRecord.Nodes`（`calllog.go:40`）是节点指纹链，**空数组 = 这次请求没有任何节点参与**（直连 / 闸门拒绝 / 早退）。`route_mode` 不能用（见 §0 纠正）。

```
经过节点数分布（233,452 条）
 nodes=0   11,171   4.79%   ← 没有走节点
 nodes=1  210,412  90.13%
 nodes=2    4,493   1.92%
 nodes=3    7,230   3.10%
 nodes=4+     146   0.06%
```

**没走节点的 11,171 条再细分**（按事件与状态）：

| 分类 | 条数 | 占全量 | 占"没走节点" | 含义 |
| --- | --- | --- | --- | --- |
| 直连·**成功** | 4,470 | **1.91%** | 40.0% | 真·直连出口，且请求成功 |
| 直连·闸门拒绝 | 3,284 | 1.41% | 29.4% | 有 `quota_halt + blocked` 事件：节点全冷却，**请求根本没发出去** |
| 直连·其他失败 | 2,936 | 1.26% | 26.3% | 早退失败（无节点参与） |
| 直连·连不上 | 481 | 0.21% | 4.3% | `connect_error`，直连出口连不上 |

**"直连成功"按日期看，更能说明它不是一种"选择"**：

```
2026-10-07  0 / 2003 = 0.00%      2026-10-14  0 / 1304 = 0.00%
2026-10-08  0 / 1691 = 0.00%      2026-10-15  0 / 4400 = 0.00%
2026-10-09  0 /  466 = 0.00%      2026-10-16  1 / 6531 = 0.02%
2026-10-10  0 / 1849 = 0.00%      2026-10-17  0 / 4687 = 0.00%
2026-10-11  0 /  615 = 0.00%      2026-10-18 117 / 7636 = 1.53%
2026-10-12  0 / 1164 = 0.00%      2026-10-19 749 / 3516 = 21.30%  ← 节点池故障期
2026-10-13  0 /  368 = 0.00%      2026-10-20  0 /  802 = 0.00%
```

⇒ 结论：
1. **正常日子里"直连"是 0.00%**（10-07 ~ 10-17、10-20 全是 0）；
2. 10-18/10-19 的尖峰不是"用户打开了直连开关"，而是 **`egress.go:137` 的自动兜底**（节点全部冷却 → 直连）——这两天日志里同时有大量 `quota_halt`（3,284 条闸门拒绝）；
3. 也就是说：**"直连"从来没有被当作设置使用过，只作为故障兜底存在。**

### 1.4 "付费请求走不走代理"：付费档在生产里几乎不存在

`route_mode` = 鉴权档位（`main.go:2576-2587`）：

| route_mode | 条数 | 占比 | 状态 |
| --- | --- | --- | --- |
| `public`（免费/公开档） | **233,442** | **99.9957%** | ok 208,432 / fail 25,010 |
| `auto` | 7 | 0.0030% | **全部失败** |
| `go` | 1 | 0.0004% | **失败** |

`go`/`auto` 是"带 key 走付费/直连 zen"的档位（`main.go:2612-2625` 前缀路由）。生产里 8 条、且全失败 ⇒ **`socks5_paid_direct` 这个开关在事实层面没有可作用的请求**。它只对 `tier == TierPaid` 生效（`egress.go:125`），而免费档（99.997%）无论如何都不受它影响。

### 1.5 走直连有没有正当业务理由（如果要保留，理由是什么）

代码与文档里能支撑"必须直连"的只有两类：

| 理由 | 代码证据 | 生产是否成立 |
| --- | --- | --- |
| ① 用户自己的**付费 key / 自有上游**不想过第三方节点 | `socks5_paid_direct` 注释（`main.go:1043-1046`）："true: paid/keyed traffic goes direct; only public/free uses SOCKS5"；`main.go:2596-2625` 的 `go:`/`zen:` 前缀路由 | **不成立**：付费档请求 8 条 / 233k，且全部失败 |
| ② 上游对代理 IP 风控，直连反而更稳 | `main.go:1044` 注释语义；以及 `egress.go:118-120` 注释「上游按出口 IP 分桶限流」 | **不成立**：直连成功率反而更低（直连·连不上 481 条；免费额度是按 key+IP 计的，代理存在的目的正是绕限流） |

而用户反问的那一句「**不走代理我们的节点池做出来干什么的**」在数据上是**完全正确**的：节点池就是唯一出口机制（95.21% 请求经过它），没有第二套并行的"直连模式"在用。

### 1.6 删掉会怎样（正面回答"会不会变成无脑全代理"）

| 场景 | 删掉出口设置后的行为 | 判断 |
| --- | --- | --- |
| 正常（节点池有节点） | 100% 走节点池 —— **与今天完全一致** | 无变化 |
| 节点全部冷却 | `egress.go:137` 自动直连兜底 —— **与今天完全一致** | 无变化（不需要开关） |
| 节点池为空 + 没配静态代理 | 直连（`egress.go:141 → 155-157`） | 无变化 |
| 节点池为空 + 配了静态代理 | **会失去静态 SOCKS5 出口**（唯一真实影响） | 生产 0 使用；保留"后端读字段"即可兼容老部署 |
| 用户自带付费 key 想过直连 | **会失去这个开关** | 生产 0 使用；将来真需要时用**自动判断**替代（见结论） |

### 1.7 结论（§1 的判定）

> **D 可以删**：`socks5_proxies` / `active_socks5` / `socks5_paid_direct` / `socks5_sticky` **四项整组从设置界面删除**。
> **落地形式是"删 UI、留字段、留兼容读取"**：后端继续 `egress.Configure(...)` 读这三个字段（`main.go:1322`、`main.go:7583`、`main.go:6932` 的 `GetSocks5Config`），保证老 `config.json` 不炸、无节点池的部署仍可用；但**面板不再提供任何"走哪个出口"的控件**，也不再在 `/api/config` GET 里回显（`:6942-6944` 三行）。
> **不建议 B（合并成"一律走代理"）**，因为"一律走代理"本来就是现状，合并没有信息增量；**也不建议 C（写死）**，因为写死成"代理"会删掉无节点池部署的出口能力，而写死成"直连"会破坏现状。
> 如果将来真要支持"付费 key 不走第三方节点"，正确做法是**自动判断**（配置里有付费 key ⇒ 该档直连；否则全走节点），不需要用户选——这也正好回到用户那句话：**节点池就该是唯一出口**。

---

## 2. 25 项必要性总表（含 1 项面板不可见的隐藏字段）

> 口径：`/api/config` GET 返回 **25 个键**（`main.go:6958-6981`；24 个 `AppConfig` 字段 + `log_level` + `log_bodies`，其中 `truncation_stop_reason` **不在 GET 响应里**，故作为第 26 行列在表尾）。生产值来自 `/data/config.json`（只读）。读者行号以当前 `main.go`/`egress.go`/`logging.go` 为准。

| # | 字段 | 默认值 | 生产值 | 谁在读（file:line） | 改了什么后果 | 判定 |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | `api_key` | `""`（空=不启用网关鉴权） | **有值（27 字符 sk-）** | `main.go:1302` 读取；`main.go:2619-2625` 网关密钥校验（**通过后仍是 `public` 免费档**，不提升档位）；`:2626-2628` 任意合法 `sk-` key → `auto` 档 | 改=所有客户端换密钥；删=退回「任意合法 sk- key 都通」 | **A** |
| 2 | `model_alias` | `{}` | **空** | `main.go:1286`；`main.go:1415`（resolveModelWithDocs 里查表） | 改=客户端模型名映射到别的上游模型；删=自定义别名全部失效（`-free` 自动映射不受影响） | **A**（低频、配错就换模型） |
| 3 | `hidden_free_aliases` | `[]` | **`["deepseek-v4-flash"]`** | `main.go:1290-1293`；`main.go:1417`、`:3970`（列表/鉴权） | 改=某个免费模型从模型列表消失/禁用；删=已隐藏的模型重新出现 | **A**（生产在用） |
| 4 | `edited_free_aliases` | `nil` | 不存在 | `main.go:1288-1289`；`main.go:1416`、`:4005` | 改=免费模型的**显示名**；删=显示名回默认 | **B**（与 #3 合并成"免费模型：改名/禁用"一张表） |
| 5 | `reasoning_effort_map` | `{}` | **4 项**（`high/max/medium→low` 映射） | `main.go:1298-1299`；`main.go:1692-1700` `getReasoningEffortMap()` | 改=推理力度翻译变化（影响思考 token 成本）；删=恢复直传 | **A**（低频但影响成本） |
| 6 | `force_disable_thinking` | `false` | `false` | `main.go:1301`；`main.go:1685-1689` | 改=所有请求强制去掉思考内容；删=默认 false（等同） | **B**（与 #5 合并成"推理与思考"一组） |
| 7 | `model_region_map` | `nil` | 不存在（**由代码自动写入**） | `main.go:1295-1296`；`main.go:1605-1620` `persistModelRegion()` 自动学习；`egress.go:131` `RequiredRegion` 用它挑区域节点 | 用户手改=可能把某模型锁到没节点的区域导致全失败；删字段=区域探测失去持久化 | **E**（只读诊断；写入权归程序） |
| 8 | `socks5_proxies` | `nil` | 不存在 | `main.go:1322`/`7583` `egress.Configure`；`egress.go:145-165` 静态分支 | 见 §1 | **D** |
| 9 | `active_socks5` | `""` | 不存在 | 同上 | 见 §1 | **D** |
| 10 | `socks5_paid_direct` | `false` | 不存在 | `egress.go:121-127`（仅付费档） | 见 §1 | **D** |
| 11 | `socks5_sticky` | `true` | 不存在 | `main.go:1315-1318`；`egress.go:150`（仅静态分支） | 见 §1（节点池模式不读，承诺为空） | **D** |
| 12 | `subscriptions` | `[]` | **2 条**（飞鸟云 / 红杏） | `main.go:7592` `subManager.configure`；保存时同步重建池：`main.go:7309-7336` | 改=节点来源变化；删=失去 36.5% 节点（见 §2.1） | **A** |
| 13 | `webshare` | `[]` | **3 条**（主力） | 同上 | 改同上；删=失去 **63.0%** 节点 | **A**（主力） |
| 14 | `manual_nodes` | `nil` | 不存在 | 同上 | 删=失去手工兜底能力（生产 0 使用） | **B**（并入"节点来源"卡） |
| 15 | `quota_error_signals` | 代码默认 4 type + 4 keyword | **2 组**（type 4 项、keyword 5 项，多了 `quota_signal`） | `main.go:1337-1345`；消费点 `quota.go` 判定 | 改=误判"额度耗尽"→ 触发换节点风暴 / 或漏判→一直撞 429；删=回代码默认（生产会丢 `quota_signal` 这一条） | **E**（只读诊断，改错代价最高） |
| 16 | `node_cooldown_exhausted_hours` | `0`→回 1h | **5**（用户调大过） | `main.go:1326-1330`；`quota.go`/`proxy_node.go` 冷却判定 | 改=耗尽节点被冷却多久（越短越容易再撞额度；越长越少节点可用） | **A** |
| 17 | `node_cooldown_dead_minutes` | `0`→回 60s | **5**（用户调大过） | `main.go:1331-1335` | 改=故障节点多久后复探（"复探"就是这个字段，**没有独立开关**） | **A** |
| 18 | `max_quota_node_switches` | `5` | **5** | `main.go:1346-1349`；`quota.go:13` | 改=单请求最多换几个节点（越大越慢但越可能成功） | **A** |
| 19 | `node_health_interval_minutes` | `0`→15min；负数=禁用 | **15** | `main.go:1354-1360`；`proxy_node.go` 探测定时器 | 改=健康检查频率；删=默认 15min（等同现状） | **A**（低频） |
| 20 | `node_health_probe_url` | `""`→默认 `https://opencode.ai/zen/v1/models` | 不存在 | `main.go:1361` `proxyPool.probeURL` | 改=探测目标（探测 URL 错了会把所有节点判死！） | **C**（写死默认，专家配置才改⇒配置文件） |
| 21 | `prompt_cache_retention` | `""`→运行时 `"24h"` | 不存在 | `main.go:1304-1306`；`main.go:1703-1709` `getPromptCacheRetention()` | 改=注入上游的缓存保留时长（`off` 会让缓存命中暴跌 → 费用上升） | **C**（默认 24h 已最优） |
| 22 | `cache_control_breakpoints` | `true`（指针 nil 时） | 不存在 | `main.go:1307-1309`；`main.go:1712-1716` | 改=关掉 Anthropic 风格 cache_control 断点 → 缓存不生效 | **C**（写死 true） |
| 23 | `text_only_models` | `nil`→`["deepseek"]` | 不存在 | `main.go:1310-1313`；`main.go:2032`（图文降级） | 改=哪些模型只接受文本（写错=图片被静默降级/或没降级导致 400） | **C**（默认已覆盖） |
| 24 | `log_level` | `"info"`（flag `-log-level`） | 无键=默认 `info` | `logging.go:69-73` `setLogLevelString`；`main.go:6981/6994` GET/POST | 改=日志量/排障能力；`debug` 才让 `log_bodies` 生效（`logging.go:568`） | **A**（排障要改；但该放"排障"区） |
| 25 | `log_bodies` | `false`（flag `-log-bodies`） | 不存在 | `logging.go:567-572` `maybeLogBodySummary` | ⚠️ **只开它没用**：还要求日志级别 = debug（`logging.go:568` 的 `logLevelVar.Level() > slog.LevelDebug` 直接 return） | **E**（降级：并入日志级别说明，不单独给开关） |
| 26 | `truncation_stop_reason`（**不在 `/api/config` GET 里**） | `""`→`"length"` | 不存在 | `main.go:1303`；`main.go:1718-1731` `truncationStopReason()` | 改=流被截断时对外声明的 `finish_reason`（配错=客户端拿不到续写语义） | **C**（写死 length，面板不开） |

### 2.1 节点来源四件套：谁在真正供节点（生产数据）

`/data/proxy_state.json` = **215 个节点**（全部 `state: available`）。从生产日志（`/data/opencode2api.log` 末尾 30 MB）里按节点显示名前缀去重统计：

| 来源 | 配置条数 | 去重节点数 | 占比 |
| --- | --- | --- | --- |
| `webshare`（3 条，名字就叫 `1`/`2`/`3`） | 3 | **119** | **63.0%** |
| `subscriptions` → 飞鸟云 | 2 条里 1 条有效 | **69** | **36.5%** |
| `subscriptions` → 红杏 | 1 | **0** | 0%（日志里反复出现 `subscription refresh failed ... "没有任何可用节点"`，如 `12:29:31`/`12:50:59`/`13:00:59`） |
| `manual_nodes` | 0 | 0 | 0% |
| `socks5_proxies`（静态代理，不是节点池） | 0 | 0 | 0% |

⇒ **主力是 `webshare`（63%），其次是飞鸟云订阅（36.5%）**；手动节点与静态 SOCKS5 **完全没有被使用**。所以「四个入口都要吗」的答案是：**保留三个（订阅 / Webshare / 手动节点），静态 SOCKS5 整组删**；其中手动节点可以并进"节点来源"卡的一个「手动添加」折叠项（生产 0 使用，但它是订阅全挂时的兜底）。

---

## 3. 精简后的设置集（数量与归属）

### 3.1 分档汇总

| 档 | 数量 | 项 |
| --- | --- | --- |
| **A 必须保留** | **12** | `api_key`、`subscriptions`、`webshare`、`node_cooldown_exhausted_hours`、`node_cooldown_dead_minutes`、`max_quota_node_switches`、`node_health_interval_minutes`、`log_level` + 模型类 4 项（`model_alias`、`hidden_free_aliases`、`reasoning_effort_map`、`force_disable_thinking`） |
| **B 可以合并** | **2** | `edited_free_aliases`（并入 #3 免费模型表）、`manual_nodes`（并入订阅/Webshare 的"节点来源"卡） |
| **C 可以写死** | **5** | `node_health_probe_url`、`prompt_cache_retention`、`cache_control_breakpoints`、`text_only_models`、`truncation_stop_reason` |
| **D 可以删（UI 删、字段留）** | **4** | `socks5_proxies`、`active_socks5`、`socks5_paid_direct`、`socks5_sticky` |
| **E 降级只读诊断** | **3** | `model_region_map`（程序自动写）、`quota_error_signals`（改错=误判风暴）、`log_bodies`（并入日志级别的说明） |

### 3.2 精简后"用户真的要改的"到底几项

> **答案：设置页 8 个可编辑控件 + 模型页 5 个 = 13 个；其中"日常会碰"的只有 5–6 个。**

| 归属 | 控件 | 频次（按生产值判断） |
| --- | --- | --- |
| 设置页 · 接入 | `api_key` | 一次（配好不动） |
| 设置页 · 节点从哪来 | `subscriptions`（2 条）、`webshare`（3 条）、手动节点（折叠） | 偶尔（换机场/换代理池） |
| 设置页 · 节点不好用时 | 耗尽冷却 `5h`、故障冷却 `5min`、最多换 `5` 次、健康检查 `15min` | 极低频（用户已调过一轮，之后不动） |
| 设置页 · 排障 | `log_level` + 危险区（清空统计 / 清空调用日志 / 解除节点标记 / 退出登录） | 排障时才碰 |
| 模型页 | 映射表、免费模型改名/禁用、推理力度映射、强制禁思考、（缓存三项只读展示） | 低频 |
| **只读诊断（不占设置位）** | 当前出口分布、`model_region_map`、`quota_error_signals`、缓存配置生效值、日志级别**实际**生效值 | 排障时看 |

⇒ 对比现状（面板 25 项 + 表格 4 张 + 一堆按钮，桌面 2.9~3.4 屏 / 手机 4.8~5.7 屏），**可编辑项从 25 降到 8（设置页）+5（模型页）**，页面体量降为约 1/3。

---

## 4. 删 / 合并 / 写死的风险清单（每项：删掉谁受影响）

| 项 | 动作 | 谁会受影响 | 风险与缓解 |
| --- | --- | --- | --- |
| `socks5_proxies` / `active_socks5` | **删 UI，保留字段与读取** | ①只配静态 SOCKS5、没有节点池的部署（出口会退化为直连）②正在用"固定单代理"的用户 | **低**：生产 0 使用；缓解=字段保留 + 在节点池页脚注「静态 SOCKS5 已并入节点池，如需可在配置文件配置」 |
| `socks5_paid_direct` | 同上 | 自带付费 key 且要求直连的用户（**生产 0**） | **低**；将来真需要→自动判断（有付费 key 就直连），不恢复开关 |
| `socks5_sticky` | 同上 | 用静态 SOCKS5 + 依赖会话粘性的用户（**生产 0**） | **低**；注意它今天在节点池模式下**本就不生效**，删掉反而消除了 false promise |
| `edited_free_aliases` | 合并进 #3 | 改过免费模型显示名的人（生产没改） | **低**：合并成同一张表的一列，能力不丢 |
| `manual_nodes` | 合并进"节点来源" | 手工加了兜底节点的人（生产 0） | **低**：折叠入口保留 |
| `node_health_probe_url` | 写死默认 URL | 探测目标需要自定义的网络环境 | **中**：探测 URL 错=所有节点判死 → 必须保留配置文件覆盖能力，并在健康检查卡上给"当前探测地址"只读展示 |
| `prompt_cache_retention` / `cache_control_breakpoints` | 写死（24h / true） | 需要关缓存做对比测试的人 | **低**：只读展示生效值 + 配置文件可覆盖 |
| `text_only_models` | 写死默认 `["deepseek"]` | 新增只接受文本的上游模型时 | **中**：新模型若只吃文本，图片会 400 → 缓解=配置文件可覆盖 + 面板只读展示当前列表 |
| `truncation_stop_reason` | 写死 `length`，面板不开 | 需要别的 `finish_reason` 语义的客户端 | **低**：默认就是 length；本来 UI 也没暴露 |
| `model_region_map` | 降级只读 | 想手动锁区域的人 | **中**：手改会把模型锁到没节点的区域 → 只读 + 显示"由区域探测自动维护" |
| `quota_error_signals` | 降级只读 | 上游换了错误体/新增配额信号 | **中高**：改错会误判风暴（今天生产 keyword 里多了一条 `quota_signal`，正是有人调过）→ 只读展示"当前生效签名"，需要改走配置文件 + 发版说明 |
| `log_bodies` | 并入 `log_level` 说明（不单独开关） | 想抓请求体排障的人 | **中**：`log_bodies` 单独开**什么都不发生**（`logging.go:568` 要 debug）→ 合并后写成「日志级别 = debug 时才记录截断的请求体摘要（4KB）」，避免用户开了没效果 |

---

## 5. 给阶段二（页面重画）的输入

### 5.1 精简后的控件清单（设置页 8 个可编辑 + 3 块只读 + 1 个危险区）

| 组 | 一句话人话标题 | 控件 | 生效方式 |
| --- | --- | --- | --- |
| 1 | **谁能用这个网关** | `api_key`（1 个输入 + 生成/复制） | 即时（下一个请求） |
| 2 | **节点从哪来** | 订阅源表（2 行：名称/URL/间隔/节点数）、Webshare（3 行：名称/key/中转/间隔）、「手动添加节点」折叠 | **保存时会重建节点池，最长 60 秒**（`main.go:7309-7336`） |
| 3 | **节点不好用时** | 耗尽冷却（5h）、故障冷却（5min）、单请求最多换几次（5）、健康检查间隔（15min）+ 只读「当前探测地址」 | 即时 |
| 4 | **模型怎么表现 → 单开模型页** | 映射表、免费模型（改名/禁用）、推理力度映射、强制禁思考、（缓存三项只读） | 即时 |
| 5 | **排障** | 日志级别（并注明「debug 才会记录请求体摘要」）+ 只读：当前出口分布 / 区域锁 / 配额签名 | 即时 |
| 6 | **危险区**（页尾，二次确认） | 清空统计、清空调用日志、解除全部节点标记、退出登录 | 不可撤销 |

### 5.2 阶段二的硬约束（来自本次研究）

1. **不许再用三页签**（用户已否决「分页格式」）；设置项只剩 8 个控件，**单页 ≤1.2 屏**是可达的（当前 2.9~3.4 屏的主因就是 25 项 + 4 张表）。
2. 页面上**不许出现"走哪个出口"**这类控件；如果要解释，用一句只读说明：「出口 = 节点池（全自动，全部冷却时自动直连）」。
3. 模型类 5 项**必须在模型页**，不进设置页。
4. 只读诊断区（出口分布 / 区域锁 / 配额签名 / 缓存生效值）**默认折叠**，标题写「排障时才用」。
5. 组标题不许用工程词（`路由`/`维护`/`配额信号` 都是 jargon），用「节点从哪来 / 节点不好用时」这种问句式人话。
6. 每个"保存会阻塞"的控件保留「保存会重建节点池 · 最长 60 秒」的徽标（`main.go:7309-7336` 实测阻塞点）。

---

## 6. 复算命令（全部只读）

```bash
cd /home/jingyx/project/opencode2api-test

# 1) 生产配置键（不打印密钥）
docker exec opencode2api cat /data/config.json | python3 -c "
import json,sys; d=json.load(sys.stdin)
for k,v in d.items():
    print(k, ('<list len=%d>'%len(v)) if isinstance(v,list) else ('<dict len=%d>'%len(v)) if isinstance(v,dict) else (('<len %d>'%len(v)) if k=='api_key' else v))"

# 2) 节点数 / 节点来源（按显示名前缀去重）
docker exec opencode2api cat /data/proxy_state.json | python3 -c "
import json,sys; d=json.load(sys.stdin); print('nodes', len(d['nodes']))"
docker exec opencode2api sh -c "tail -c 30000000 /data/opencode2api.log" \
 | grep -oE 'node=[^ ]+ fp=[0-9a-f]+' | python3 -c "
import sys,collections
src=collections.defaultdict(set)
for l in sys.stdin:
    nm,fp=l.strip().split(' fp=',1); nm=nm[5:]
    src[nm.split('::')[0] if '::' in nm else nm].add(fp)
print({k:len(v) for k,v in sorted(src.items(), key=lambda x:-len(x[1]))})"

# 3) 出口分布（nodes 为空 = 没走节点）+ 按天的直连成功
docker exec opencode2api cat /data/call_log.jsonl > /tmp/prod.jsonl
python3 - <<'PY'
import json,collections
n=0; nodes0=collections.Counter(); byday=collections.defaultdict(lambda:[0,0])
for line in open('/tmp/prod.jsonl','rb'):
    if not line.strip(): continue
    r=json.loads(line); n+=1
    ns=r.get('nodes') or []; d=(r.get('ts') or '')[:10]
    byday[d][1]+=1
    if not ns:
        ev=[e.get('type') for e in (r.get('events') or [])]
        blocked=any(e.get('type')=='quota_halt' and 'blocked' in (e.get('detail') or '') for e in (r.get('events') or []))
        k='直连·闸门拒绝' if blocked else '直连·连不上' if 'connect_error' in ev else '直连·成功' if r.get('status')=='ok' else '直连·其他失败'
        nodes0[k]+=1
        if k=='直连·成功': byday[d][0]+=1
print('total',n); print('nodes=0 细分:',dict(nodes0))
print('按天直连成功:', {d:'%d/%d'%(a,b) for d,(a,b) in sorted(byday.items())[-14:]})
PY

# 4) route_mode 是鉴权档位，不是出口（读代码即可确认）
sed -n '2576,2587p;  2612,2625p' main.go   # AuthRouteMode 定义
sed -n '290,300p' logging.go              # authModeString()
sed -n '116,145p' egress.go               # 出口四级决策（节点池优先，静态 socks5 是死角）
sed -n '1282,1365p' main.go               # applyConfig：每个字段的默认值与生效点
sed -n '6958,6981p' main.go               # /api/config GET 的 25 个键
```

---

## 7. 边界

- 本轮**只读**：没有改任何 `*.go`、`static/admin.html`、`config.json`；没有 git 操作；没有重启容器；没碰 8000。
- 生产访问全部是 `docker exec ... cat` / `tail` 只读，统计副本落在 `/tmp`。
- 未决（等 Lead 拍板）：`quota_error_signals` 是否接受"只读 + 配置文件"；`node_health_probe_url` 写死后是否需要保留配置文件覆盖；阶段二页面是否接受"设置页 8 控件单页"。
