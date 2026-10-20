# 健康探测「每轮 34~43% 失败 + 抖动闸每轮触发」诊断（task-30）

> 结论先行版。所有数字都带出处；测不到的写「未测到」，没有推测冒充实测。
> 代码基线：`ba26d70`（分支 `test`），生产容器 `opencode2api`（镜像 `ghcr.io/shiyanyx/opencode2api:latest`），
> 取证时间 2026-10-19 19:28–19:50 Asia/Shanghai。
> 一次性诊断测试：`zz_t30_probe_cause_test.go`、`zz_t30_alert_safety_test.go`（`*_test.go` 被 `.gitignore:26` 忽略）。

---

## 0. 一句话结论

**「每轮 43% 失败」不是探测判据比真实流量严，也不是出口 IP 被上游 429/403 —— 是池子里真的有 ~91 个节点用不了；
而抖动闸的判据（整轮失败数 ≥20）无法区分「全体抖动」和「固定的一批坏节点」，于是把这 91 个节点永久地判成健康。**

三个关键反证（都在下面有原始数据）：

| 假设 | 数据 | 判定 |
| --- | --- | --- |
| 节点被上游按出口 IP 返回 429/403（`probeNode` 要求 2xx） | 236 次真实探针里 **0 次拿到任何 HTTP 状态码**；13:50 一次性判死的 92 个节点原因是 timeout/tls/eof，**HTTP 状态 0 个** | **证伪** |
| 只是探测目标太严（打 opencode.ai 而真实流量也打它） | 同一批节点换打 `https://www.gstatic.com/generate_204`，**失败集合完全一致**（2×2 交叉：`opencode-FAIL × gstatic-OK = 0`） | **证伪** |
| 是我们本机出口/网络问题（闸门的自我诊断） | webshare `1::`（100 个 socks5，**有节点归属的 1168/1169 个真实请求全部走这一源**）抽样 8/8 全通；失败只集中在订阅的 vless/hy2/trojan 上，且失败发生在**到代理服务器自己**的 TLS 握手上（`209.248.40.224:55676 connect error: remote error: tls: handshake failure`） | **证伪（非全局）** |

真实可用节点数 ≈ **122 / 214（57.2%）**，而面板报 **213 available（99.5%）**，`dead_count=0`。

---

## 1. 现象复核（第 1 问）

### 1.1 取证方式

`docker logs` 已被重启截断，但**容器内的文件日志没有丢**：`/data/opencode2api.log` 有 **363,937 行、回溯到 2026-10-06**，且包含 14:00–18:44 那段（任务书里「那段日志没了」的前提不成立）。

```bash
docker exec opencode2api sh -c 'wc -l /data/opencode2api.log'   # → 363937
docker exec opencode2api sh -c 'grep -c "probe_round_global_blip" /data/opencode2api.log'  # → 24
```

### 1.2 抖动闸上线以来 **21 轮全部触发**，没有一轮例外

`grep -E "probe_round_global_blip" /data/opencode2api.log`（21 条 WARN + 3 条 ERROR）：

| # | 时间 | probed | failed | 失败率 | consecutive |
| ---: | --- | ---: | ---: | ---: | ---: |
| 1 | 14:03:45.526 | 216 | 96 | 44.4% | 1 |
| 2 | 14:19:18.591 | 216 | 96 | 44.4% | 2 |
| 3 | 14:34:13.693 | 216 | 92 | 42.6% | 3 ← ERROR#1 |
| 4 | 14:49:25.712 | 216 | 92 | 42.6% | 4 |
| 5 | 15:04:42.232 | 216 | 93 | 43.1% | 5 |
| 6 | 15:20:39.921 | 216 | 92 | 42.6% | 6 |
| 7 | 15:36:49.216 | 216 | 94 | 43.5% | 7 |
| 8 | 15:52:47.474 | 216 | 94 | 43.5% | 8 |
| 9 | 16:08:33.572 | 214 | 93 | 43.5% | 9 |
| 10 | 16:24:46.545 | 214 | 94 | 43.9% | 10 ← ERROR#2 |
| 11 | 16:40:36.573 | 214 | 93 | 43.5% | 11 |
| 12 | 16:56:34.598 | 214 | 84 | 39.3% | 12 |
| 13 | 17:12:01.838 | 214 | 93 | 43.5% | 13 |
| 14 | 17:28:12.143 | 214 | 92 | 43.0% | 14 |
| 15 | 17:44:34.460 | 214 | 93 | 43.5% | 15 |
| 16 | 17:59:41.962 | 214 | 94 | 43.9% | 16 |
| 17 | 18:15:04.220 | 213 | 93 | 43.7% | 17 |
| 18 | 18:30:07.785 | 213 | 93 | 43.7% | 18 |
| — | **18:44 重启（consecutive 归零）** | | | | |
| 19 | 18:46:49.844 | 213 | 72 | 33.8% | 1 |
| 20 | 19:03:25.146 | 213 | 91 | 42.7% | 2 |
| 21 | 19:18:41.829 | 213 | 91 | 42.7% | 3 ← ERROR#3 |

- 轮次间隔 **15m16s ~ 16m36s**（周期 15m + 单轮耗时 ~160–200 秒，见 §4.3）。
- 失败率区间 **33.8% ~ 44.4%，均值 42.6%**（Lead 的「34–43%」复核通过，略偏保守）。
- 每轮都远超闸门阈值（`failed >= 20`，即 9.4%）⇒ 闸门**在当前失败水平下永远不可能放行**。

### 1.3 `dead_count` 确实长期为 0

```bash
PW=$(grep '^OPENCODE2API_PASSWORD=' /home/jingyx/project/opencode2api/.env | cut -d= -f2-)
curl -s -c /tmp/ck.txt --data-urlencode "password=$PW" http://127.0.0.1:8000/login
curl -s -b /tmp/ck.txt http://127.0.0.1:8000/api/nodes
```

```json
{"healthy":213,"dead_count":0,"exhausted_count":1,
 "node_alert":{"active":false,"window_minutes":10,"window_attempts":31,"window_successes":31,
               "window_success_rate":1,"run_failures":0,"pool_available":213,"pool_total":214}}
```

节点状态分布（214 个）：`available 213 / exhausted 1 / dead 0`。

日志侧同构证据：`grep "node marked dead by health check"` 的**最后一条是 `2026-10-19T13:50:22.312`**，
此后（14:00 新镜像上线 → 19:30）**再没有一条**。也就是说 `dead_count=0` 不是抽样偶然，是 5.5 小时零判死。

---

## 2. 失败原因聚合（第 2 问 · 核心）

### 2.1 方法：一次性只读诊断测试，用生产同一套 dialer 打真实节点

`zz_t30_probe_cause_test.go`：

1. 从 `/data/config.json`（`docker exec cat` 只读取出到 `/tmp`）拿 2 个订阅 + 3 个 webshare 源；
2. 订阅文本用**生产解析器** `parseSubscriptionText()`（`subscription.go:503`）解析，webshare 用**生产函数** `fetchWebshare()`（`webshare.go:107`）拉取 ⇒ 装出与生产同样的 **214 个节点**（订阅 94 / webshare 120）；
3. `mihomoMgr.apply()` + `newProxyPool()` + `setNodes()` ⇒ 与生产完全相同的 `dialNode → mihomoDial → newNodeTransport` 出口链路；
4. 对**真实节点**调**生产函数** `probeNode()`（`proxy_node.go:683`），ctx 上限同生产 `probeTimeout=20s`（`:234`），并发同生产 `probeMaxConcurrent=8`（`:625`）；
5. 每个节点打**两个**目标：
   - `https://opencode.ai/zen/v1/models`（生产现行探针，`proxy_node.go:233`）
   - `https://www.gstatic.com/generate_204`（历史默认探针目标，用来判断「节点本身通不通」）
6. 按 `timeout / tls / eof / dial / proxy-setup / http4xx(401,403,429) / http5xx / other` 归类。

采样：订阅 94/94 全覆盖 + webshare 24/120（按源均分）⇒ **118 个节点 × 2 目标 = 236 次探针，耗时 199 秒**。
只读：不消耗推理配额（两个目标都是免费的 GET）、不改生产配置、不重启、不碰 8000 端口的服务端行为。

### 2.2 表 A：按探针目标 × 节点来源

| 探针目标 | 来源 | n | 成功 | 成功率 | 失败原因 |
| --- | --- | ---: | ---: | ---: | --- |
| opencode.ai | `1::`(webshare-1) | 8 | 8 | **100.0%** | — |
| opencode.ai | `2::`(webshare-2) | 8 | 5 | 62.5% | timeout=3 |
| opencode.ai | `3::`(webshare-3) | 8 | 5 | 62.5% | timeout=3 |
| opencode.ai | 红杏（订阅） | 63 | 10 | **15.9%** | tls=48 eof=4 timeout=1 |
| opencode.ai | 飞鸟云（订阅） | 31 | **0** | **0.0%** | timeout=30 tls=1 |
| gstatic 204 | `1::`(webshare-1) | 8 | 8 | 100.0% | — |
| gstatic 204 | `2::`(webshare-2) | 8 | 5 | 62.5% | timeout=3 |
| gstatic 204 | `3::`(webshare-3) | 8 | 5 | 62.5% | timeout=3 |
| gstatic 204 | 红杏（订阅） | 63 | 10 | 15.9% | tls=45 eof=7 timeout=1 |
| gstatic 204 | 飞鸟云（订阅） | 31 | 0 | 0.0% | timeout=30 tls=1 |

### 2.3 表 B：按探针目标 × 协议（这就是根因的形状）

| 探针目标 | 协议 | n | 成功 | 成功率 | 失败原因 |
| --- | --- | ---: | ---: | ---: | --- |
| opencode.ai | socks5（webshare） | 24 | 18 | 75.0% | timeout=6 |
| opencode.ai | anytls（红杏） | 11 | **10** | **90.9%** | timeout=1 |
| opencode.ai | vless（红杏 REALITY） | 52 | **0** | **0.0%** | tls=48 eof=4 |
| opencode.ai | hy2（飞鸟云） | 28 | **0** | **0.0%** | timeout=28 |
| opencode.ai | trojan（飞鸟云） | 3 | **0** | 0.0% | timeout=2 tls=1 |
| gstatic 204 | socks5 / anytls / vless / hy2 / trojan | 同上 | 同上 | 同上 | 同上（仅 tls/eof 各差 2–3 个） |

### 2.4 表 C：原因直方图（opencode 目标，n=118）

| 原因 | 次数 | 占比 | 说明 |
| --- | ---: | ---: | --- |
| 成功 | 28 | 23.7% | |
| **tls** | **49** | 41.5% | 全部是「到**代理服务器**的 TLS 握手」失败：`remote error: tls: handshake failure` / `first record does not look like a TLS handshake` / `x509: ... no IP SANs` |
| **timeout** | **37** | 31.4% | `net/http: TLS handshake timeout`（15s，`tlsHandshakeTimeout`）/ `context deadline exceeded`（20s，`probeTimeout`） |
| **eof** | **4** | 3.4% | `IP connect error: EOF` |
| **http 401/403/429** | **0** | 0% | |
| **http 4xx 其它 / 5xx** | **0** | 0% | |
| dial / other | 0 | 0% | 没有「TCP 完全连不上」的（hy2 是 UDP/QUIC，见下） |

**关键：236 次探针里，拿到 HTTP 状态码的次数 = 0。** 所有失败都发生在「连上代理服务器 / 与代理服务器 TLS 握手」这一层，
请求根本没走到 opencode.ai 的 HTTP 层。所以 `probeNode`「要求 2xx 才算通」这条判据**一次都没有起作用**。

### 2.5 表 D：2×2 交叉 —— 决定性的那一张

| | gstatic 通 | gstatic 不通 |
| --- | ---: | ---: |
| **opencode 通** | **28** | 0 |
| **opencode 不通** | **0** | **90** |

**没有任何一个节点「只对 opencode.ai 不通」。** 若失败是上游按出口 IP 封禁/限流造成的，这一格必然非零。
它精确地等于 0 ⇒ 失败与目标无关，是节点本身（或它到本机的链路）不可用。

### 2.6 独立复核：13:50 那一次「一次性判死 92 个」的原因分布

14:00 新镜像上线**之前**，旧代码在 `13:50:02–13:50:22` 把 92 个节点一次性判死（`reason=` 原文聚合）：

| 原因 | 数量 | 归类 |
| --- | ---: | --- |
| `context deadline exceeded` | 36 | timeout |
| `IP connect error: remote error: tls: handshake failure` | 49 | tls |
| `IP connect error: EOF` / `EOF` / `failed to create session: EOF` | 5 | eof |
| `timeout: no recent network activity` | 1 | timeout |
| `IP connect error: x509: cannot validate certificate for IP because it doesn't contain any IP SANs` | 1 | tls |

**同样 0 个 HTTP 状态错误。** 与今天的在线探针结论完全一致。
按节点来源拆这 92 条判死：**红杏 53 + 飞鸟云 33（其中 3 条是订阅自带的信息性伪节点）+ webshare 6** ⇒ 订阅侧占 86/92。

### 2.7 节点被选中概率的算术闭合（证明这不是抽样偏差）

用 §2.2 的每组成功率外推到全池：

```
1:: 100 × 100.0%  = 100.0
2::  10 ×  62.5%  =   6.3
3::  10 ×  62.5%  =   6.3
红杏 63 ×  15.9%  =  10.0
飞鸟云 31 ×  0.0%  =   0.0
                    ------
可用 ≈ 122.6 / 214  ⇒  失败 ≈ 91.4 / 214 = 42.7%
```

与生产日志 `failed=91/213=42.7%`（19:03、19:18）**吻合到小数位**。⇒ 探针测到的 43% 是真实现象，不是测量伪影。

### 2.8 根因定位到「节点服务器自己」

用 curl 直接摸订阅节点的入口（与探针无关，绕开全部应用代码）：

| 目标 | TCP 443/端口 | TLS 握手 | 结论 |
| --- | --- | --- | --- |
| `209.248.40.224:55676`（红杏 vless，REALITY） | **通** | `TLS alert handshake failure (552)` | 服务器活着但拒绝握手 |
| `209.248.40.224:4148`（红杏 vless，REALITY） | **通** | 同上 | 同上 |
| `209.248.40.224:28537`（红杏 anytls） | **通** | **握手成功**（Server hello → Certificate → verify 全走完） | 同一 IP、同一端口段，anytls 能用 |
| `tw1.459292.xyz:443`（飞鸟云 hy2） | **不通** | — | 服务器直接连不上 |
| `tw06.459292.xyz:443`（飞鸟云 hy2） | **不通** | — | 同上 |

补充事实（从订阅原文解出来的）：
- **红杏 63 个节点全部指向同一个 IP `209.248.40.224`**，只是端口不同（`address` 去重后 1 个）。
  其中 **52 个是 vless + REALITY**（`security=reality`，`sni=www.tue.nl` / `www.oracle.com`，`fp=chrome`，`flow=xtls-rprx-vision`），
  **11 个是 anytls**（同一端口 `28537`）。⇒ 52 个 REALITY 全灭、anytls 11 个里活了 10 个。
- **飞鸟云 31 个节点用域名**（`tw1.459292.xyz` 等），全部连不上。31 个里 28 hy2 + 3 trojan。

所以：**飞鸟云整源不可用；红杏的 REALITY 端口全部握手被拒、anytls 端口正常。** 这是订阅侧/节点侧的故障，不是我们探针的判据问题。

### 2.9 待验证的可疑点（唯一一处「可能是我们自己」的地方）

`ProxyNode.Flow` 被解析（`subscription_parse.go:80`），注释写「仅支持空 `""`」（`proxy_node.go:74`），
而 `mihomoStreamFields()`（`mihomo.go:169-193`）**只写 tls / servername / skip-cert-verify，不透传 `flow`**；
REALITY 分支（`mihomo.go:117-129`）也只写 `public-key` / `short-id` / `client-fingerprint`。

52 个 vless 节点全部真实流量跑的是 `flow=xtls-rprx-vision`。理论上 flow 缺失影响的是 VLESS 数据层而非 TLS 握手层，
与本观测（握手阶段就失败）不符，**但这是唯一一个「我们侧可能导致 52 个 vless 全灭」的点**。
判定方法（未测到，留给后续）：用独立客户端（xray / sing-box）配同一份 REALITY 参数打同一端口；
或用 mihomo 配置里显式加 `flow: xtls-rprx-vision` 做对照。**本报告不下结论。**

### 2.10 并发因素（`probeMaxConcurrent=8`，213 个节点一轮 27 批）

- 8 并发不足以让上游限流成为原因：失败原因是 TLS 握手/超时，**不是 429**（0 次）。
- 8 并发确实让单轮变慢：213 个节点 / 8 ≈ 27 批，实测单轮 **~162–203 秒**（14:00:22 启动 → 14:03:45 完成 = 203s；18:44:07 → 18:46:49 = 162s）。
- 单轮耗时大头是 91 个失败节点的超时：`91 × 15s ÷ 8 ≈ 171s`，接近整轮耗时。**调大并发能缩短单轮，但不改变失败率。**

---

## 3. 这是不是新引入的？（第 3 问）

**不是。任务书里「14:00–18:44 的日志被重启清空 ⇒ 只能写无法判定」这个前提不成立 —— 日志没丢。**

- 容器文件日志 `/data/opencode2api.log` 从 2026-10-06 连续记到 19:29，**跨重启保留**（lumberjack，`max_size_mb=100 max_backups=7 max_age_days=14`）。
- **抖动闸最早一条 WARN：`2026-10-19T14:03:45.526`**，即 `local-37ed108` 镜像 14:00 上线后的**第一轮**巡检。此后 21/21 轮全部触发。
- **43% 的失败率在闸门存在之前就已经发生**：13:50:02–13:50:22（旧代码，闸门尚未上线）一轮判死 92/216 = **42.6%**，原因分布见 §2.6，与今天一致。
- 更早的「池子被抽干」证据（`node health check done` 的 `checked=` 即本轮探测的 available 数）：

```bash
docker exec opencode2api sh -c 'awk "/^time=2026-10-19T(0[0-9]|1[0-3]):/" /data/opencode2api.log \
  | grep "node health check done" | sed -E "s/^time=([0-9T:-]+)\..*checked=([0-9]+).*/\1 checked=\2/"'
```

| 时间 | checked | 时间 | checked |
| --- | ---: | --- | ---: |
| 07:49:35 | 120 | 11:11:35 | 4 |
| 08:04:35 | 137 | 11:27:25 | 16 |
| 08:20:15 | **3** | 11:43:35 | 2 |
| 08:35:35 | 118 | 11:59:15 | **0** |
| 08:51:35 | 119 | 12:14:15 | 0 |
| 09:06:21 | 19 | 12:29:15 | 0 |
| 09:22:20 | 18 | 12:44:15 | 0 |
| 09:38:21 | 19 | 13:00:15 | 0 |
| 09:54:17 | 19 | 13:16:15 | 0 |
| 10:10:35 | 17 | 13:32:15 | 0 |
| 10:25:15 | **0** | 13:47:15 | 0 |
| 10:40:15 | 0 | 13:50:22 | **216**（13:49 重启后状态被重置） |

⇒ 闸门上线前，池子在 **0 ~ 137 available** 之间剧烈震荡，并且有 **连续 8 轮（11:59→13:47，约 2 小时）available=0**。
这正是「一次抖动就把一批节点判死、越判越空」的形态。**闸门是冲着这个写的，它确实止住了抽干，但换成了另一种失真。**

**结论：失败（~43%）是新代码之前就存在的既有事实；14:00 上线的新代码只是把它从「可见的抽干」变成了「不可见的满编」。**

---

## 4. 后果量化（第 4 问）

### 4.1 `dead_count=0` 的害处，按大小排

1. **面板在说谎（最严重）**：面板/`/api/nodes` 报 `healthy=213 / total=214`，真实可用 ≈ **122 / 214**。
   这精确复现了 10-19 00:00–09:00 事故的定义式——「池子看起来健康」和「池子真的能用」是两件独立的事（`node_alert.go:5-8` 原文：227 个节点、面板显示 115 available，出口整层不可用）。
2. **节点评分（ROADMAP §2b）的换节点触发条件② 永久失效**。`ROADMAP.md:865-867` 写死：
   ```
   触发换节点只有两种情况，都不看统计：
     ① 连续失败（见下面「两种故障分开算」）
     ② 配额耗尽 / 探测判定节点死了
   ```
   闸门让 ② 永不成立 ⇒ 打分系统只剩 ① 一条腿，且「探测判定节点死了」这个输入**恒为空集**。
3. **每次落到坏节点上就浪费一次 attempt**：实测（`call_log.jsonl`，14:00–19:30）
   - 1169 个请求 / 1165 ok / 4 fail（0.34%），只用到 **16 个不同节点**，其中前 3 个占 **1102/1169 = 94.3%**
     （`1::AT-45.39.96.13:5426` 496 次、`1::BR-92.112.171.3:5971` 454 次、`1::BR-45.39.2.144:6576` 152 次）。
   - ⇒ **今天的实际浪费很小，因为选路是粘性的，流量一直贴在 3 个 webshare 节点上**；且 1168/1169 个请求都落在 `1::` 这一个源里，
     也就是说池子里另外 114 个节点（红杏/飞鸟云/`2::`/`3::`）今天几乎没被真实流量碰过。
   - 但代价是**不对称的**：命中 hy2 节点的代价是 `tlsHandshakeTimeout=15s` 白等（28 个 hy2 = 池子的 13%），
     命中 REALITY 节点是立即失败后重试。真实 p99 = 49.3s 已经包含这类白等。
   - **★ 必须先说清的口径问题：Lead 的「108/108 成功（100%）」只覆盖 3~16 个节点，不能用来证明「213 个节点都没坏」。**
     94.3% 的流量压在 3 个节点上，剩下 ~200 个节点在真实流量里**根本没有被检验过**。这条数据支持不了「节点池没坏」。
4. **探测预算的浪费**：96 轮/天 × 213 探针 = **20,448 探针/天**，其中 42.7%（**~8,730 次/天**）是注定失败的；
   失败侧大多是 15–20s 超时 ⇒ 按 15s 计 **8,730 × 15s ≈ 131,000 探针·秒/天 ≈ 36 探针·小时/天**，按 8 并发折合 **~4.5 小时/天的墙钟**纯空转（估算口径见 §4.3）。

### 4.2 日志噪声（可精确计数）

| 日志 | 频率 | 每天 |
| --- | --- | --- |
| `WARN probe_round_global_blip` | 每轮（15 min） | **96 条** |
| `ERROR probe_round_global_blip_persistent` | `consecutive==3 || consecutive%10==0`（`proxy_node.go:831`） | **~10 条**（3,10,20,…,90；日志实测 21 轮里 3 条，与公式一致） |
| `WARN subscription refresh failed error="没有任何可用节点: "` | 每 10 分钟（实测 18:54:04 / 19:04:04 / 19:14:04 / 19:24:04） | **~144 条** |

合计 **~250 条/天** 恒定噪声，且这条 WARN/ERROR 对是**永久常亮**的（consecutive 只增不减，除非重启）。
更糟的是它的文案把人往错方向带：「疑似本机出口/网络问题」——而实测本机出口好得很（webshare 8/8 全通）。

**附带发现（新 bug，与本任务同源）**：那条 144 条/天的 `没有任何可用节点: ` 是**空转误报**。
`subscription.go:296` 在 `len(all)==0` 时返回 error，而「所有源都还在更新间隔内、本轮无需拉取」时 `all` 天然为空、
`errs` 也为空 ⇒ 打出 `error="没有任何可用节点: "`（冒号后面什么都没有就是证据）。它不代表订阅失败。

### 4.3 单轮耗时（估算，标明口径）

- 实测：14:00:22 启动 → 14:03:45 首轮完成 = **203s**；18:44:07 → 18:46:49 = **162s**。
- 估算：失败侧 91 个 × 15s（`tlsHandshakeTimeout`）÷ 8 并发 ≈ **171s**；成功侧 122 个 × ~10s ÷ 8 ≈ 152s；
  两者有并发重叠，量级与实测 162–203s 相符。**这是估算，不是实测分解。**
- 杠杆结论：**把 `probeMaxConcurrent` 从 8 调到 24 能把单轮压到 ~60s，但失败率一点不变**；它治的是耗时，不是本 bug。

---

## 5. 安全性问答（第 6 问 · 必答）

> 问：现行实现下，一次「43% 失败但真实流量 100%」的场景，判据 B（连续失败段）与 C（池子空）是否仍能告警？
> 也就是「抖动闸」只压制 dead 标记，还是把**告警**一起压制了？

### 5.1 代码走查：闸门与告警是**两条完全独立的链路**

```
探测链路（决定节点状态）：
  health_runtime.go:67-75  runNodeHealthTick → proxyPool.checkAvailable(ctx)
  → proxy_node.go:767      checkFiltered(probeAvailable)
  → proxy_node.go:824-833  guarded := filter == probeAvailable
                           if guarded && probeRoundLooksLikeGlobalBlip(probed, len(failedFP)) {
                               blipStreak.Add(1); slog.Warn(...); slog.Error(...);
                               return probed            // ← 唯一的拦截点：不判死
                           }
  → proxy_node.go:838-840  for _, fp := range failedFP { p.markProbeDead(fp, ...) }

告警链路（决定要不要报警）：
  health_runtime.go:77     evaluateNodeAlert()          // 每分钟必跑，与探测结果无关
  → health_runtime.go:84   poolStateCounts()            // 只读节点状态
  → node_alert.go:284      nodeAlerts.evaluate(now, avail, total)
  ← 样本来自 node_alert.go:170  recordNodeAttempt(usedNode, transportOK)  // 真实上游流量喂进来的
```

**`proxy_node.go` 里 `recordNodeAttempt(` 的出现次数 = 0**（`TestT30ProbePathDoesNotFeedAlertMonitor` 断言）。
⇒ 探测怎么做都影响不到 A / A' / B / B' / burst 这五条判据的样本。

### 5.2 判据 B：闸门生效期间**照样报**（构造测试）

`TestT30CriterionBStillFiresWhileGateActive`：216 个节点、92 个探测失败（42.6%，闸门 `consecutive=1` 生效中），
同时喂真实流量「30 次成功 + 20 次连败、跨 3m48s」：

```
已确认：闸门 concurrent=1 生效时，判据 B 仍报：节点出口自 00:02:30 起已连续失败 20 次、跨 3m48s，期间零成功
```

### 5.3 判据 C：唯一被闸门**间接**压制的一条，但仍有别的入口

`TestT30CriterionCPoolEmptyReachability`：

```
已确认：4 轮全失败（100%）后 available 仍为 216 ⇒「探测判死 ⇒ 池空」这条路径在闸门下不可达
已确认：池空仍可由配额路径触发判据 C：节点池无可用节点：available=0（共 216 个），流量只能走 direct 出口
```

判据 C 读的是 `poolStateCounts()`（`health_runtime.go:12-20`），而闸门让 `available` 永不下降 ⇒
**「因为探测认定大批节点坏了」这条路永远走不到池空**。但 C 依然能被「配额耗尽」这条路径触发（构造测试已证）。

### 5.4 「43% 失败 + 真实流量 100%」时一个告警都不报 —— 这是**正确**的，不是漏报

`TestT30NoAlertWhenRealTrafficHealthy`：3 轮 42.6% 探测失败、窗口 31/31 成功 ⇒ `active=false`。
跟生产 `/api/nodes` 的 `node_alert` 快照（`window_attempts=31, window_successes=31, active=false`）**逐字段一致**。
理由是充分的：真实流量确实 100% 成功过（在它覆盖的那 3 个节点上），没有理由报警。

### 5.5 那安全性上的真问题是什么

不是「闸门压掉了告警」，而是：

1. **闸门把「坏」压成了「看不见」**：`available=213` 是虚假的（真值 ≈122），而**面板/告警/打分全都在读这个数**。
2. **判据 C 的语义被掏空**：C 想抓的正是「池子空了、流量只能走 direct」这个 8 小时事故形态；
   但闸门保证 `available` 永远不会因为探测而下降，所以 **C 在「探测发现坏节点」这条主路径上失效**，
   只剩下「配额全耗尽」这个更慢、更罕见的入口。
3. **告警引擎自己的盲区（既有设计，本次未改）**：`node_alert.go:62-64` 有意规定
   「只有**传输层**失败才算节点失败：拿到任何上游响应（含 403/429）都说明这条出口是通的」。
   ⇒ 一批节点被上游按 IP 403/429 时，告警引擎**按设计不会报**。这与 task-3 的探测盲区是同一类问题，不属本任务范围，但一并记录。

**安全性结论一句话**：闸门只压制 dead 标记、不压制 A/A'/B/B'/burst（已用构造测试钉死）；
它对告警的唯一实质影响是把判据 C 的主路径堵死，并让面板的 `available` 长期虚高。

---

## 6. 候选修法对比 + 推荐（第 5 问）

### 6.1 先说被数据否掉的那个

**(b)「传输层通即算通（2xx/4xx 都算活着，只有 dial/EOF/timeout 才算坏）」—— 对本现象零效果，且方向危险。**

- 数据：236 次探针里 **0 次拿到任何 HTTP 状态码**（§2.4），所有失败都在传输层。放宽判据**一个节点都不会变绿**。
- 而且它正好复现 10-17 的形态：403/429 被算成健康 ⇒ IP 被封的节点会被判成「活着」。
- **决定：拒绝。** 这条方案是在解一个不存在的问题（本轮没有 429/403），代价是拆掉唯一能识别「出口被上游针对」的判据。

### 6.2 候选表

| # | 方案 | 改动量 | 收益 | 残留风险 |
| --- | --- | --- | --- | --- |
| **A** | **判据从「整轮失败数」改成「对照组（canary）是否也失败」**：巡检时先/并行探 5 个对照组节点（取上一轮探测成功的节点里随机抽，跨来源），对照组失败率 ≥60% 才判全局抖动（抑制判死），否则照常判死 | ~40 行，`proxy_node.go` | 42.7% 的子集失败 → 正常判死 → `available` 回落到 ≈122（**真值**）；真·全局抖动 → 对照组也挂 → 照旧抑制 | 对照组恰好全落在故障子集里 ⇒ 误判为抖动。用「上一轮成功集合」抽样可自愈（成功集合天然排除长期坏节点）；成本 +5 探针/轮（+2.3% 探测预算） |
| **B** | 闸门改比例：`failed*4 >= total*3`（即 ≥75%）才不采信，`failed>=20` 这条绝对判据删掉；靠既有 `probeFailStreakThreshold=2` 吸收单轮抖动 | **1 行**（`proxy_node.go:639-647`） | 42.7% < 75% → 不触发闸门 → 判死 → 回落到真值 | 真·全局抖动若只挂 60~70% 会判死一批；但 streak=2 需要**连续两轮**才判死，而实测抖动只持续 ~2 分钟（远短于 15 分钟一轮）⇒ 风险低。<br>**注意：`failed>=20` 在 213 节点的池子里等于 9.4% 阈值，任何长期坏 10% 的池子都会永久锁死在「抖动」状态 —— 这是当前故障的直接成因** |
| **C** | WARN/ERROR 里加失败原因直方图（tls/timeout/eof/http4xx/http5xx + 按来源） | ~30 行 | 下一次不用写诊断测试就能看出「是超时还是握手失败、集中在哪个源」；本次结论若早有这条，可省半天 | 不改任何行为，纯可观测 |
| **D** | 撤销闸门，只留 streak=2 | −30 行 | 最小实现；streak 已足够吸收单轮抖动 | 连续两轮的全局抖动（>15 min 出口故障）会判死一大批 → 池子抽干，正是 14:00 之前的状态。**不推荐单独使用** |
| **E** | 放宽 `probeTimeout`/判据 | — | 无用（见 6.1） | 危险 |

### 6.3 推荐：**A 为主 + C 为辅**，同批修掉两个附带 bug

**推荐 A（对照组判定）**，理由：

1. 它不是「调阈值」，而是把判据换成语义正确的那一个 —— 抖动的定义是「连最稳的节点也挂了」，不是「失败数超过某个数字」。
   `failed>=20` 这个绝对阈值在 213 节点的池子上等于 9.4%，**任何长期坏 10% 的池子都会被永久锁在「抖动」状态**，那不是抖动，那是坏节点。
2. 它对两个方向都成立：真抖动 → 对照组也失败 → 抑制（保护池子不抽干）；固定子集坏 → 对照组正常 → 判死（恢复 dead 信号）。
3. 成本极小：每轮 +5 个探针（+2.3%），40 行以内，不改任何数据结构。

**若只想先止血、要 1 行改动**：走 **B**（把绝对阈值 `failed>=20` 删掉/抬高到比例判据），
配合既有的 `probeFailStreakThreshold=2`。这能立刻让 `dead_count` 恢复，代价是真抖动时多判死一批（下一轮探测成功会自动复活，`recordProbeSuccess` `proxy_node.go:899`）。

**附带必修（同源 bug，各 1~10 行）：**

| # | 问题 | 位置 | 修法 | 收益 |
| --- | --- | --- | --- | --- |
| **B1** | **重启丢掉全部节点状态**：`loadState()` 只在 `newProxyPool()` 里调一次，此时 `p.nodes` 还是空的 ⇒ 循环体一次都不执行 ⇒ `proxy_state.json` 写了但**从来没被读进来**；重启后所有 dead/exhausted 全部复活成 available | `proxy_node.go:286`、`:1100-1122` | 在 `setNodes()` 之后补一次 `loadState()` | 现在 91 个坏节点每次重启都会被当成健康节点重新入池；这也是 14:03 那轮 `probed=216`（13:50 刚判死 92 个）的原因 |
| **B2** | 「没有任何可用节点」空转误报，144 条/天 | `subscription.go:296` | `len(all)==0 && len(errs)==0` 时返回 `(0, nil)`（本轮无需刷新不是错误） | 日志噪声/天 −144 |
| **B3** | WARN 文案「疑似本机出口/网络问题」把运维带偏 | `proxy_node.go:830` | 改成「疑似本机出口/网络问题（需与对照组比对）」或等 A 上线后按 A 的结论措辞 | 少一次误判 |
| **B4** | 面板 `available` 口径 | 面板/`/api/nodes` | 增加「最近一轮探测失败」计数，或把 `available` 拆成 `available` / `probe_unreachable` | 面板不再虚报 213 |

### 6.4 运维建议（不属于代码改动）

91 个坏节点里有 **84 个来自订阅**（红杏 53 + 飞鸟云 31）。这不是我们能"修"的：

- **飞鸟云 31 个**：服务器 `*.459292.xyz` / `*.344211.cc` 的 443 **TCP 直接连不上** ⇒ 订阅商换了服务器但订阅没更新，或该源已失效。→ 找订阅商 / 换源。
- **红杏 52 个 vless**：同一 IP `209.248.40.224` 的 REALITY 端口**全部 TLS 握手被拒**，而**同 IP 的 anytls 端口正常** ⇒ 大概率是 REALITY 参数（`pbk` / `sid` / 端口角色）在服务端已变更。→ 找订阅商。
- **★ 唯一需要排除「是我们自己转换错」的点**：`flow`（`xtls-rprx-vision`）没有透传给 mihomo（§2.9）。
  建议用 xray/sing-box 做一次独立对照，再决定要不要补 `flow` 透传。**未测到，不下结论。**

---

## 7. 附录

### 7.1 复现命令

```bash
# 1) 全量抖动闸/r轮次（跨重启保留的文件日志）
docker exec opencode2api sh -c 'grep -E "probe_round_global_blip" /data/opencode2api.log'
docker exec opencode2api sh -c 'grep -c "node marked dead by health check" /data/opencode2api.log'
docker exec opencode2api sh -c 'grep "node marked dead by health check" /data/opencode2api.log | tail -1'   # → 13:50:22.312

# 2) 每轮 available 数（看闸门上线前的抽干）
docker exec opencode2api sh -c 'awk "/^time=2026-10-19T(0[0-9]|1[0-3]):/" /data/opencode2api.log \
  | grep "node health check done" | sed -E "s/^time=([0-9T:-]+)\..*checked=([0-9]+).*/\1 checked=\2/"'

# 3) 面板状态（dead_count / node_alert）
PW=$(grep '^OPENCODE2API_PASSWORD=' /home/jingyx/project/opencode2api/.env | cut -d= -f2-)
curl -s -c /tmp/ck.txt --data-urlencode "password=$PW" http://127.0.0.1:8000/login
curl -s -b /tmp/ck.txt http://127.0.0.1:8000/api/nodes | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["dead_count"],d["healthy"],d["node_alert"])'

# 4) 真实流量的节点覆盖（证明「100% 成功」只覆盖 3~16 个节点）
docker exec opencode2api sh -c 'grep -F "\"ts\":\"2026-10-19T1" /data/call_log.jsonl' \
 | python3 -c 'import sys,json,collections; c=collections.Counter()
for l in sys.stdin:
    r=json.loads(l)
    for n in (r.get("node_names") or []): c[n]+=1
print(c.most_common(5))'

# 5) 一次性诊断测试（真实节点 × 双目标 × 按原因聚合）
export PATH=$PATH:/usr/local/go/bin GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
T30_SAMPLE_SUB=94 T30_SAMPLE_WS=24 go test -run TestT30ProbeCauseAttribution -v -timeout 40m .   # → 199s
go test -run 'TestT30Gate|TestT30NoAlert|TestT30Criterion|TestT30ProbePath' -v .                # → 1.0s
```

### 7.2 本次新增文件（均不改仓库受跟踪文件）

| 文件 | 行数 | 说明 |
| --- | ---: | --- |
| `zz_t30_probe_cause_test.go` | 456 | 一次性只读诊断：真实节点 × 双目标 × 原因聚合（`*_test.go` 被 `.gitignore:26` 忽略） |
| `zz_t30_alert_safety_test.go` | 247 | 5 条安全性构造测试（闸门 vs 告警 / 判据 B / 判据 C / 源码断言） |
| `/tmp/t30/probe_causes.json` | — | 236 条探针原始结果（含原始错误串） |

未改动：`proxy_node.go` / `node_alert.go` / `health_runtime.go` / `main.go` / `static/admin.html` / 生产配置 / 生产容器。
`task8_bucket_query_test.go` 保持在 `/tmp/t8_hold.go` 未动。

### 7.3 关键代码锚点（`ba26d70` 实测，非引用转述）

| 位置 | 内容 |
| --- | --- |
| `proxy_node.go:233` | `defaultProbeURL = "https://opencode.ai/zen/v1/models"` |
| `proxy_node.go:234` | `probeTimeout = 20 * time.Second` |
| `proxy_node.go:622` | `probeFailStreakThreshold = 2` |
| `proxy_node.go:625` | `probeMaxConcurrent = 8` |
| `proxy_node.go:639-647` | `probeRoundLooksLikeGlobalBlip`：`failed>=20` 或 `failed*4>=total*3` |
| `proxy_node.go:683-713` | `probeNode`：GET、**2xx 才达标**、`start` 在 `client.Do` 之前、对 opencode.ai 注入 `Authorization: Bearer public` + `x-opencode-session` |
| `proxy_node.go:767` | `checkFiltered` |
| `proxy_node.go:824-833` | 抖动闸唯一拦截点：`return probed`（不判死） |
| `proxy_node.go:826-831` | `consecutive==3 \|\| n%10==0` → ERROR |
| `proxy_node.go:838-840` | 闸门放行后才 `markProbeDead` |
| `proxy_node.go:871-895` | streak 守卫 + `node marked dead by health check`（`:891`） |
| `proxy_node.go:899` | `recordProbeSuccess`：dead→available **静默复活** |
| `proxy_node.go:286` / `:1100-1122` | `loadState()` 在 `newProxyPool()` 里调用（`p.nodes` 为空 ⇒ 循环体一次不执行，见 B1） |
| `proxy_node.go:74` | `Flow` 字段注释「仅支持空 `""`」 |
| `health_runtime.go:59-64` | 每分钟复探全部 dead 节点（无冷却判断） |
| `health_runtime.go:67-75` | 每 15 min 巡检 available，完成后 `node health check done` |
| `health_runtime.go:77` | `evaluateNodeAlert()` 每分钟必跑（与探测解耦） |
| `health_runtime.go:12-20` | `poolStateCounts()` 读全局池的 available/total |
| `node_alert.go:170-196` | `recordNodeAttempt`（**唯一**告警样本入口，来自真实流量） |
| `node_alert.go:284-378` | `evaluate`：A / A' / B / B' / burst / C |
| `node_alert.go:295` | `poolEmpty := poolTotal>0 && poolAvailable==0` |
| `node_alert.go:62-64` | 刻意边界：「拿到任何上游响应（含 403/429）都说明这条出口是通的」 |
| `subscription.go:503` | `parseSubscriptionText`（诊断测试复用） |
| `subscription.go:296` | `没有任何可用节点: %s`（空转误报，见 B2） |
| `webshare.go:107` | `fetchWebshare`（诊断测试复用） |
| `mihomo.go:117-129` | REALITY 转换：只写 `public-key` / `short-id` / `client-fingerprint` |
| `mihomo.go:169-193` | `mihomoStreamFields`：tls / servername / skip-cert-verify，**不透传 flow** |
| `proxy_dial.go:13` / `mihomo.go:196` | `dialNode` → `mihomoDial`（所有协议统一走内置 mihomo） |
| `ROADMAP.md:865-867` | §2b 换节点触发条件：① 连续失败 ② **探测判定节点死了** |

### 7.4 「未测到」清单（明确不编造）

1. 红杏 52 个 REALITY 节点「握手被拒」是**订阅参数失效**还是**我们转换漏字段（flow / REALITY 参数）** —— 未测到，需独立客户端对照（§2.9）。
2. 飞鸟云服务器是「订阅商换机」还是「临时故障」—— 未测到；只测到 `*.459292.xyz:443` TCP 不可达。
3. 闸门上线前池子被抽干到 0 的**精确因果链**（判死速率 vs 每分钟复探的吞吐瓶颈）—— 只有 `checked=` 序列与 dead 计数的观测事实，未做逐轮归因。
4. 单轮耗时的**逐阶段分解** —— 只有「203s / 162s」两个实测值与 §4.3 的估算。
