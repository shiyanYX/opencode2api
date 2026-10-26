# T3 · 健康检查盲区（原因 C）：代码级证据、候选方案对比与探测成本模型

> 作者：teammate `healthcheck-probe`（task-3）
> 工作区：`/home/jingyx/project/opencode2api-test` @ `a19d3a4`
> 取证时间：**2026-10-17 23:29 ~ 23:38 (+08:00)**（所有"生产实测"数字都是这个时间点的快照；生产日志在持续增长）
> 本文只写这一个文件，未改动任何其它仓库文件 / 生产配置 / 生产容器。
> 所有成本算术都给了可复算的输入；所有代码结论都带 `文件:行号`。

---

## 0. 结论速览

1. **盲区被代码坐实，且有生产日志的强证据**：健康探测打的是 `GET https://opencode.ai/zen/v1/models`
   （`proxy_node.go:222`），它是**目录端点**，不经过免费层推理闸门（条件全部只作用在推理 POST 上，见 §1.4）。
   生产日志全生命周期里 `probe status 403` 出现 **0 次**，而真实流量产生了 **2,445 次** `http 403`。
2. **比"看漏"更糟**：盲探针会**主动把坏节点从 dead 复活成 available**（`proxy_node.go:695-712`，且**不打日志**）。
   2026-10-17 那条坏节点 `1::CL-64.137.121.210:6465` 的证据链：20:43:00.931 被探测判死 → 21:18:56.879
   被真实流量判死（connect_error）→ **1 分钟内被 1 分钟一次的 dead 复探静默复活** → 21:33:32.810 ~
   22:56:47.033 之间向它发出 **231 次 `http 403`**，而这 84 分钟里**一次探测失败都没有**。
3. **探测预算的真实规模被严重低估**：健康循环不只做 15 分钟巡检，它**每分钟**复探全部 dead 节点
   （`health_runtime.go:18,31-36` + `proxy_node.go:621-624`）。按 23:29 快照（93 dead）算，
   现状探测请求 = `128×96 + 93×1440` = **146,208 次/天**，是真实流量的 **39.4 倍**（均值）/ **209.8 倍**（中位）。
4. **推荐方案**：**不做周期性全池闸门探测**（方案 a 否决），改为事件驱动的**闸门感知探针（GASP）**：
   检测主路径交给 T1 的 confirm-then-mark（零额外请求），探针层只做两件事 ——
   (i) **疑点确认**（(c)：同节点 10 分钟内累积 2 次 `FreeTierError` 归因 → 打 1 次闸门探测）；
   (ii) **接管复活判据**（IP 拒绝型节点的复活只能由闸门探测 2xx 触发，间隔 2h，与台账 §六 一致，上限 1,404 次/天）。
   成本：**≤ 1,404 次探测/天（0.38× 真实均值）**，token 占比 ≤ 0.17%。
5. **不会打空整池的三道闸**：被动见证（近 W 分钟内有**别的**出口拿到过 2xx）+ 池底（单轮标记后 available ≥ floor）
   + 全局形状闩（任一见证 403 → 本窗口内禁止一切节点级标记）。证明在 §4.3。

---

## 1. 探测路径代码复核

### 1.1 探测目标与配置通路

| 事实 | 证据 |
|---|---|
| 默认探测目标 = `https://opencode.ai/zen/v1/models` | `proxy_node.go:222` |
| 默认探测周期 = 15 分钟 | `proxy_node.go:221` |
| 单节点探测超时 = 20 秒 | `proxy_node.go:223`（`probeTimeout`） |
| 配置键 `node_health_interval_minutes` 的语义：`<0` 禁用 / `0` 用默认 15min / `>0` 直接用 | `main.go:1046-1048`（注释）、`main.go:1336-1345`（接线）、`proxy_node.go:505-513` |
| 配置键 `node_health_probe_url` 可覆盖探测 URL | `main.go:1049`、`main.go:1345`、`proxy_node.go:516-523` |
| 生产 `/data/config.json` 实际值 | `node_health_interval_minutes: 15`，**没有** `node_health_probe_url`（即吃默认 `/zen/v1/models`） |
| `main.go:426` 是**另一条**同 URL 的路径（`fetchModels()`，走 `mgmtClient` 直连、不经过节点） | `main.go:423-449`；调用点 `main.go:772 / 3535 / 3700 / 6529 / 7169` |

> 注意 `main.go:426` 与探测**没有调用关系**：它只是"用同一个不设闸门的目录端点"去拉模型列表。
> 两者共用同一个"目录端点不打闸门"的性质，但修复健康检查**不需要**动 `fetchModels()`。

### 1.2 巡检循环与状态机

`health_runtime.go:16-52`，1 分钟 ticker：

```go
18  ticker := time.NewTicker(time.Minute)
22      interval := proxyPool.healthInterval()      // <0 才禁用
31      if proxyPool.hasDeadNodes() {               // ★ 每分钟，无条件
33          checked := proxyPool.checkDead(ctx)     //    复探【全部】dead 节点
39      if !next.IsZero() && now.Before(next) { continue }
42      next = now.Add(interval)                    // 每 15min 一次
47      checked := proxyPool.checkAvailable(ctx)    //    巡检【全部】available 节点
49      slog.Info("node health check done", ...)
```

- `checkAvailable` → `checkFiltered(probeAvailable)`，只挑 `State == NodeAvailable`（`proxy_node.go:617-620`）。
- `checkDead` → `checkFiltered(probeDead)`，只挑 `State == NodeDead`（`proxy_node.go:621-624`）——**没有冷却判断**，
  所以只要节点还是 dead，下一分钟又被探一次。
- 轮内并发：每个节点一个 goroutine，`wg.Wait()` 收口（`proxy_node.go:628-648`）。整轮墙钟 ≈ 最慢节点，被
  `probeTimeout=20s` 与 5 分钟 ctx 双重约束（`proxy_node.go:630`、`health_runtime.go:46`）。
- 状态转移：
  - 失败 → `markProbeDead`（`proxy_node.go:656-692`）：`exhausted` 被保护（:664-669）、`dead` 只记日志不重复标记
    （:670-674）；available→dead 时 `CooldownUntil = now + healthIntervalLocked()`（:679）、清空 `activeID`（:680）。
  - 成功 → `recordProbeSuccess`（`proxy_node.go:695-712`）：写 `LatencyMs`/`LastProbeAt`；
    **`if n.State == NodeDead { → available }`（:704-708），且函数里没有任何 slog**。
  - `eligible()` 只看 `State == NodeAvailable`（`proxy_node.go:288-290`）；`pick()` 无可用节点返回 `nil`（`proxy_node.go:361-363`）。

### 1.3 `latency_ms` 到底测的是什么

`proxy_node.go:532-563`，答案是：**冷路径全流程（代理 TCP 连接 + 代理协议握手 + 目标 CONNECT + TLS 握手 + HTTP 请求/响应头 + 最多 4096 字节 body）**。

```go
542  req, _ := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
547  if strings.Contains(probeURL, "opencode.ai") {
548      req.Header.Set("Authorization", "Bearer public")
549      if sid := getOrCreateSessionByNode(n.Fingerprint); sid != "" { req.Header.Set("x-opencode-session", sid) }
553  start := time.Now()          // ★ 计时起点在 client.Do 之前
554  resp, err := client.Do(req)  //    DialContext → dialNode → mihomoDial
558  io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
563  return time.Since(start).Milliseconds(), nil
```

- 拨号路径：`proxy_node.go:762-765`（`DialContext` 内套 `dialTimeout=60s`）→ `proxy_dial.go:13-24` →
  `mihomo.go:196+`（内嵌 mihomo 内核按节点指纹拨号）⇒ 起点包含代理服务器 TCP 连接 + 协议握手 + 目标 CONNECT。
  随后 `http.Transport` 完成到 `opencode.ai:443` 的 TLS 握手。
- 所以 `latency_ms` **不是** RTT、**不是**上游处理时间，而是"Clash url-test 语义"的端到端冷路径。
  生产实测佐证：23:29 快照里 128 个 available 节点的 `latency_ms` 分布
  `p50=2,274ms · mean=2,835ms · p90=4,605ms · p99=14,803ms · max=16,819ms`（n=128）。
  这个分布与台账 §六"单次探测约 2~3 秒"**独立吻合**（该数字本就由节点 RTT 主导，不是推理耗时）。
- ★ **校准依赖**：`defaultDialTimeout=60s` 的取值依据是"取自节点池探针记录的 latency_ms，n=141"
  （`proxy_node.go:237-244`）。**如果把 `latency_ms` 改用来存推理探测耗时，这条校准就失去依据。**

### 1.4 ★ 为什么这个探测对"推理闸门 403"完全盲 —— 三条独立证据

闸门条件（`docs/UPSTREAM-COMPAT.md:19-24`、`docs/adr/0002-zen-free-tier-client-fingerprint.md`）**全部施加在推理 POST 上**：
路径必须是 `/inference/v1/`、`body.stream=true`、`body.tools` 必须含 11 个官方工具名且 `parameters.properties` 非空、
UA 必须是 `opencode/<semver>` 且落在闸门认得的区间、`x-opencode-session` 必须是 descending 形态。
`/zen/v1/models` 是 **GET、无 body**，上述条件里**有 4 条根本无法施加**。

**证据 1（文档实测）**：`docs/UPSTREAM-COMPAT.md:41-42` 明确写
"（`/zen/v1/models`、`/inference/v1/models`、`/zen/go/v1/models` **三个目录端点目前都还是 200**，
所以只改了推理路径，目录抓取仍走 `/zen/v1/models`。）"

**证据 2（代码）**：探测是 `http.MethodGet`（`proxy_node.go:542`），`probeNode` 只判 2xx（:560），
既不发 `ensureAgentUpstreamShape`、也不带 UA / `x-opencode-client` / `x-opencode-project` / `x-opencode-request`
（对比真实推理请求构造 `main.go:2517-2546`）。它**结构上不可能**触发推理闸门的任何一条判据。

**证据 3（生产日志，全量）**：

```bash
docker exec opencode2api sh -c 'grep -o "probe status [0-9]*" /data/opencode2api.log | sort | uniq -c'
#   1 probe status 500
#   2 probe status 503          ← 整个日志生命周期里，探测路径的非 2xx 只有这 3 次
docker exec opencode2api sh -c 'grep -c "probe status 403" /data/opencode2api.log'   # → 0
docker exec opencode2api sh -c 'grep -c "node marked dead by health check" /data/opencode2api.log'   # → 28,874
#   其中 28,871 条 reason="probe: Get \"https://opencode.ai/zen/v1/models\": ..."（超时/EOF/拨号失败）
#   2 条 probe status 503，1 条 probe status 500，0 条 403
```

同一时间窗内真实流量：`grep -o '"detail":"http 403"'` 形态的 `upstream_error` 事件 **2,445 条**。
⇒ **探测路径从未见过一次 403；推理路径见过 2,445 次。这就是盲区的量化定义。**

### 1.5 ★ 附带发现：盲探针会静默复活坏节点（这是本次故障真正的放大器）

坏节点 `1::CL-64.137.121.210:6465`（fp `e347afddb581bf8f7301396470103c7e`）的完整证据链：

| 时间 (+08:00) | 事件 | 来源 |
|---|---|---|
| 20:43:00.931 | `node marked dead by health check`（探测超时） | app log |
| 21:18:56.879 | `node state changed state=dead reason="connect_error: ... EOF"`（真实流量，5 分钟冷却） | app log |
| ~21:24（推断） | **静默复活为 available**（`checkDead` 每分钟复探 → `/models` 200 → `recordProbeSuccess`） | `proxy_node.go:695-712` **无日志** |
| 21:33:32.810 ~ 22:56:47.033 | 该节点收到 **231 次 `http 403`** | `/data/call_log.jsonl` |
| 该 84 分钟窗内 | **0 次** `node marked dead by health check`（该 fp 最后一次是 20:43:00.931） | app log |
| 分析文档记录 | 22:46:44 该节点 `state=available, latency_ms=557, last_probe_at=22:46:44` | 分析文档 §5.2 |

复算命令：

```bash
docker exec opencode2api sh -c 'grep -o "{\"type\":\"upstream_error\".*\"node_name\":\"1::CL-64.137.121.210:6465\",\"detail\":\"http 403\",\"at\":\"[^\"]*\"" /data/call_log.jsonl | wc -l'   # → 231
docker exec opencode2api sh -c 'grep "node marked dead by health check" /data/opencode2api.log | grep "fp=e347afddb581bf8f7301396470103c7" | tail -1'   # → 2026-10-17T20:43:00.931
```

**结论**：探测层不只是"看不见"403，它在 21:18 之后**主动把一个被推理闸门拒绝的节点重新放回路由池**，
然后该节点又跑了 231 次必然失败的上游调用。任何依赖"探测成功 = 节点健康"的设计，
在闸门 403 场景下都是**负价值**的。⇒ 修复的最小充分条件是：**复活判据不能再用 `/models` 的成功**。

---

## 2. 候选方案（a~d）逐个评估

### 2.0 共同前提：一个"能过闸门"的探针必须长什么样（实测尺寸）

因为闸门条件里有 4 条作用在 body 上（§1.4），**"最小推理探测"不能是 `{"messages":[...]}` 那种极简体**：
它必须复用 `ensureAgentUpstreamShape`（`agent_shape.go`）注入的 11 个官方工具（`agent_shape.go:38-49`）+
非空 `parameters.properties`（占位定义 `agent_shape.go:80-93`）。我按 `placeholderToolNamed` 的
生成逻辑离线拼了一个探针 body 并量了尺寸：

```bash
python3 - <<'EOF'
import json
names=["bash","edit","glob","grep","read","skill","task","todowrite","webfetch","websearch","write"]
ph=lambda n:{"type":"function","function":{"name":n,"description":"Internal gateway placeholder. Not available in this session.","parameters":{"type":"object","properties":{"input":{"type":"string"}}}}}
body={"model":"space-bunny-free","stream":True,"stream_options":{"include_usage":True},"max_tokens":1,
      "messages":[{"role":"user","content":"ok"}],"tool_choice":"none","tools":[ph(n) for n in names]}
print(len(json.dumps(body,separators=(',',':'),ensure_ascii=False)))   # → 2344
EOF
```

| 量 | 值 |
|---|---|
| 探针 body 字节数 | **2,344 B**（其中 tools 占 2,173 B） |
| token 估算（chars/4） | 586 |
| 台账 §六 反解出的每探针输入 | **≈ 429 token**（见 §3.4） |
| 一致性检查 | 2,344 / 429 = **5.47 B/token** —— JSON 密集重复串的正常区间 ⇒ 台账的 token 估算可信 |

⇒ 结论：**"便宜"只能在「请求数」维度上做文章，token 维度天然可忽略**（2,344 B 的 body 不可能贵）。

### 2.1 方案 (a)：定期最小推理探测（全池巡检）

**做法**：把 15 分钟巡检的探测体换成"形态完整的推理 POST，`max_tokens=1`，读到第一个 SSE 帧即断开"，
2xx→available、403 FreeTierError→dead。

**评估**：
- ✅ 唯一能在**没有任何流量**时也发现节点被拒的方案；检测延迟 ≤ 巡检间隔。
- ❌ **成本灾难**：`128 nodes × 96 次/天 = 12,288 次/天`（仅 available 环）；若连同 dead 复探路径一起改，
  `+93×1440 = 133,920 次/天`。详见 §3。
- ❌❌ **失效模式致命**：形态回归时**每个节点都会 403**（§4.1），整池 available→dead，
  `pick()` 返回 nil（`proxy_node.go:361-363`）→ `EgressResult{Direct:true}`（`egress.go:130-138`）
  → 系统退化成"直连硬试 + 全 403"，比不修更糟。
- ❌ 与 T1 的检测能力**重复**：真实流量归因在 1~2 个请求内就能定位坏节点，比 15 分钟巡检快一个数量级。
- ⚠️ 副作用：会污染 `latency_ms` 的语义，破坏 `defaultDialTimeout` 的校准依据（§1.3）。

### 2.2 方案 (b)：真实流量 `FreeTierError` 403 按节点归因（与 T1 分工）

**探测层与 T1 的分工（本节只划边界，不重复设计 confirm-then-mark）**：

| 职责 | 归属 | 说明 |
|---|---|---|
| **何时把某节点判为"IP 被拒"** | **T1** | 403 时先换节点重试；换节点后成功 ⇒ 原节点坏；仍 403 ⇒ 形态问题、不标记。这是**零额外请求**的检测路径。 |
| **判为坏之后，谁来把它放回来** | **T3（本文）** | 当前是"`/models` 2xx 即复活"（`proxy_node.go:704-708`）或"exhausted 冷却到期无条件翻回"（`proxy_node.go:296-319`）。**这两条都必须换掉**，否则 T1 的正确标记会在 ≤1 分钟内被盲探针撤销（§1.5 的实证）。 |
| **探测层是否参与"判定"** | 否 | 探测层只在"疑点确认/复活"两个时点提供**独立的、过闸门的**证据，不改变 T1 的判定规则。 |
| **安全联锁的拥有者** | **T3** | 被动见证 + 池底 + 全局形状闩（§4），对**两条路径同时生效**（T1 的 mark 与探针的 mark 都受约束）。 |

**评估**：
- ✅ 零额外请求、检测延迟最低、对"IP 级拒绝"语义最精确。
- ❌ 依赖有流量；对"从未被路由到的节点"无覆盖（现实中 128 节点 / 中位 697 请求/天 ≈ 5.4 次/节点/天，覆盖足够）。
- ❌ 单靠它无法解决"复活"问题 —— 这是必须由探测层补上的洞。
- ⚠️ 注意：confirm-then-mark **天生不会打空池子**（它需要"另一个节点成功"才标记 ⇒ 恒有节点活着）。
  真正需要池底保护的是**探针驱动的标记**（§4.3）。

### 2.3 方案 (c)：怀疑触发式探测（N 次 403 归因才探一次）★ 重点评估形态回归

**做法**：为每个节点维护一个 `FreeTierError` 403 归因计数器；同一节点 10 分钟窗口内累积到 `N=2` ⇒
对该节点打 1 次闸门感知探测；探测 403 ⇒ 标记坏（进 IP 拒绝态）；探测 2xx ⇒ 清零计数、不标记。

**评估**：
- ✅ 成本极低：正常情况下 ≈ 0 次/天，只在该节点真的出问题时付 1 次（量级：个位数~几十次/天）。
- ✅ 检测延迟 = "第 2 次失败请求 + 1 次探测"（秒级），优于任何周期巡检。
- ✅ 比"直接标记"多一层证据：**在真实流量归因（T1）因故未生效时仍能独立工作**（defense in depth）。
- ⚠️ **形态回归是本方案唯一必须处理的坑**：全池 403 ⇒ 每个节点都会累积到 N ⇒ 每个节点都被探 1 次 ⇒
  每次都 403（同一道闸门）⇒ 若不设联锁就会把整池标空。
  **本方案的解法（§4.2/4.3）是：探测结果本身不足以标记节点，必须同时满足「被动见证存在」+「池底未破」，
  且任一 403 见证会把系统切进"全局形状闩"、本窗口内禁止一切标记。** 证明见 §4.3。
- ⚠️ N 与窗口取值影响误杀：`N=2 / W=10min` 的理由是"单次 403 可能是上游抖动或形态边缘"；
  分析文档 §4.2 的实测归因是 218/220 集中在一个节点 ⇒ N=2 已经足够收敛。
- ✅ 探针与 T1 不冲突：两者可以共享同一份"节点级 403 归因计数器"，或各自独立计数（建议独立，
  探针计数由 call_log 事件驱动，不与 T1 的 switch 预算耦合）。

### 2.4 方案 (d)：有没有既便宜又确实经过推理闸门的现成端点？

**结论：找不到。** 依据：

1. 全仓库只出现 5 个上游 URL（`grep -rn "opencode.ai" --include=*.go`）：

   | URL | 位置 | 是否过闸门 |
   |---|---|---|
   | `GET /zen/v1/models` | `proxy_node.go:222`、`main.go:426` | **否**（`UPSTREAM-COMPAT.md:41-42`） |
   | `GET /zen/go/v1/models` | `main.go:454` | **否**（同上） |
   | `POST /zen/go/v1/chat/completions` | `main.go:2525` | 是（但属 Go 面，非当前主面） |
   | `POST /inference/v1/chat/completions` | `main.go:2531` | 是（当前主推理路径） |
   | `GET /v2/docs/console/models/` | 面板文档链接 | 不适用 |

2. `count_tokens.go` 是**纯本地启发式估算**（`count_tokens.go:12` 自述"不调用上游"）⇒ 没有可用作闸门探针的
   `count_tokens` 端点。
3. 闸门本身没有"预检"端点：`UPSTREAM-COMPAT.md` / `ADR-0002` 记录的判定条件全部通过**推理请求的形态**判定，
   上游未暴露"给我判一下这个请求合不合规"的接口。
4. ⇒ **唯一能过闸门的请求就是一次形态完整的推理 POST**，最小成本 = 1 次请求 + 2,344 B body + `max_tokens=1`。

**一个值得花 1 次请求验证的候选（需 Lead 批准，见 §6-U1）**："**伪模型探针**" ——
发一个形态完整但 `model` 不存在的请求（如 `"__probe__"`）。如果上游**先过闸门、再校验模型**，
那么：闸门通过的节点 → 404/400（无 token 消耗），被拒 IP → 403 FreeTierError。
若成立，这可把每次探测的**输出 token 降到 0**，但**请求数与输入 token 不变**（body 仍必须完整形态），
所以它**不改变 §3 的成本结论**，只是让探测更"干净"。若不成立（未知模型先返回 404 之前也走 403），就放弃。

### 2.5 方案对比表

| 方案 | 额外请求/天 | 检测延迟 | 能发现 IP 拒绝 | 形态回归安全性 | 复活能力 | 结论 |
|---|---|---|---|---|---|---|
| (a) 15min 全池推理巡检 | **12,288**（+dead 复探则 146,208） | ≤15min | ✅ | ❌ 会把池打空（除非外加联锁） | ✅ | **否决**（成本高 + 与 (b) 重复 + 危险） |
| (b) 真实流量归因（T1 主设计） | **0** | 1~2 请求 | ✅ | ✅ 天生安全（需"别处成功"才标记） | ❌ 不提供复活 | **采纳为主检测路径** |
| (c) 怀疑触发探测 + 联锁 | **≈0~几十** | 秒级 | ✅ | ✅ 联锁后安全（§4） | ✅（配 (b) 的复活接管） | **采纳（探针层推荐）** |
| (d) 现成便宜闸门端点 | — | — | — | — | — | **不存在**（§2.4）；"伪模型探针"待验 |
| **(c)+复活接管（推荐）** | **≤1,404**（复活探针上限） | 秒级 | ✅ | ✅ | ✅ | ★ 见 §5 |

---

## 3. 成本模型（可复算）

### 3.1 节点数：三个数字冲突，必须说清来源

| 来源 | 取值 | 口径 / 时间 | 可信度 |
|---|---|---|---|
| **本文实测** `/data/proxy_state.json`（`docker cp` 到 `/tmp/prod-proxy_state.json`，容器内文件 mtime 23:27，我 23:29 读取） | **227 总 = 128 available / 93 dead / 6 exhausted** | 当前池（webshare 源每 24h 轮换，**节点集合会变**） | ★ 最新、最准 |
| 分析文档 §5.3 | **227 总 = 120 available / 101 dead / 6 exhausted** | 同一份 state 文件的更早快照（约 23:19），紧邻 1.6h 故障之后 | 与上一条同源、时间更早 ⇒ **差异是漂移，不是冲突** |
| 台账 §一 | **234**（承载过请求）/ **239**（候选全集） | 55 天历史去重（`/tmp/prod-now.jsonl`），**时间累计口径** | 历史累计，**不能当"当前池规模"用** |
| 台账 §六 | **117 冷却节点** | §六 24h 冷却设计的中间量（当时 exhausted+部分 dead 的口径） | 设计输入，非当前状态 |

复算：

```bash
docker exec opencode2api cat /data/proxy_state.json > /tmp/prod-proxy_state.json
python3 -c "
import json,collections
d=json.load(open('/tmp/prod-proxy_state.json'))['nodes']
print(len(d), collections.Counter(v['state'] for v in d.values()))"
# → 227 Counter({'available': 128, 'dead': 93, 'exhausted': 6})
```

**本文一律用 23:29 实测值 `N_av=128 / N_dead=93 / N_all=227`**，并在每处标明；凡是引用 120/101/6 或 117 的地方都单独标注来源。

### 3.2 每天探测次数（现状 vs 各候选）

真实流量基线（`/tmp/prod-now.jsonl`，204,237 行 / 55 天）：
`中位 697 次/天 · 均值 3,713.4 次/天 · 最高 17,232 · 最低 27`（复算命令见 §7-C）。

**现状（代码推导，§1.2 的循环）**：

| 路径 | 算式 | 次数/天 |
|---|---|---|
| 15min 巡检 available | `128 × (1440/15) = 128 × 96` | **12,288** |
| **1min 复探 dead** | `93 × 1440` | **133,920** |
| **合计** | | **146,208 / 天** |
| 相对真实流量 | `146,208 / 3,713.4` = **39.4×** · `146,208 / 697` = **209.8×** | |

> ⚠️ 这一项**只能从代码推导，日志无法直接证实**：`checkDead` 的收口日志是 `slog.Debug`（`health_runtime.go:35`），
> 而生产日志里 DEBUG 行总共只有 **65 条**（32 条 `node still dead (re-probe failed)` + 32 条 `node health probe failed`，
> 且全部集中在日志起点 2026-10-06T21:12:02，说明启动瞬间是 Debug、随后被抬到 Info）。
> **但代码路径是确定的**：`health_runtime.go:31-36` 每分钟无条件 `checkDead`，
> `proxy_node.go:621-624` 不做任何冷却判断 ⇒ 只要还有 dead 节点，每分钟就被全部复探一次。
> 把它列为 §6-U2（不确定项），并建议顺手加一个 Info 级计数以便证实/证伪。

**候选方案的请求数**（都以 available 集合为分母，`I` = 间隔分钟）：

| I | 次数/天 (`128 × 1440/I`) | ×真实均值 | ×真实中位 |
|---|---|---|---|
| 15 min | 12,288 | 3.31× | 17.63× |
| 30 min | 6,144 | 1.65× | 8.81× |
| 60 min | 3,072 | 0.83× | 4.41× |
| 120 min | 1,536 | 0.41× | 2.20× |
| 360 min | 512 | 0.14× | 0.73× |

台账 §六 已定设计的量：`117 冷却节点 × 12 次/天 = **1,404 次/天** = 0.378× 真实均值 / 2.01× 真实中位`。
**推荐方案的上限就用这个数**：复活探针只打"处于 IP 拒绝态"的节点，间隔 2h，上限 1,404 次/天
（实际会远低于上限，因为被拒绝的节点通常只有个位数）。

### 3.3 单次探测耗时

| 来源 | 值 | 口径 |
|---|---|---|
| 台账 §六 | 约 **2~3 秒**（p1≈2.1s / p5≈3.1s / p10=3.8s；"低于 1 秒的成功请求只有 4 条"） | 成功请求侧 |
| **本文实测**（23:29 快照，128 个 available 节点的 `latency_ms`） | p50 **2,274ms** · mean **2,835ms** · p90 4,605ms · p99 14,803ms · max 16,819ms | 当前 `/models` 探测（冷路径，§1.3） |
| 闸门感知探测的预期 | **≥ 当前值**：2,344 B body 上行 + 首次 SSE 帧等待（`max_tokens=1`）。**预期 3~5 秒**，硬上限 `probeTimeout=20s` | **未实测**（§6-U3） |

⇒ 台账的 2~3 秒与实测 p50 吻合，**可以引用**；但对"闸门探测"要额外加上推理 TTFT，故我按 **3~5 秒** 建模，
并明确标注为估算。整轮墙钟（全并发）= 最慢节点 ≈ `max(latency)` ≤ 20s，两个 ctx 上限分别是 20s/节点与 5 分钟/轮。

### 3.4 token 占比（复算台账 §六 的 0.077% / 0.048%）

基线取生产 `/data/stats.json` 的**生命周期累计值** ÷ 55 天（与 `/tmp/prod-now.jsonl` 的天数一致）：

```bash
docker exec opencode2api cat /data/stats.json > /tmp/prod-stats.json
python3 -c "
import json;d=json.load(open('/tmp/prod-stats.json'))
pt=sum(m.get('prompt_tokens',0) for m in d['models'].values())
ct=sum(m.get('completion_tokens',0) for m in d['models'].values())
print('requests',d['total_requests'],'prompt',pt,'completion',ct)" 
# → requests 610,448  prompt 42,982,255,876  completion 604,692,655
```

- `prompt_tokens` 基线 = 42,982,255,876 / 55 = **781,495,561 / 天**
- `completion_tokens` 基线 = 604,692,655 / 55 = **10,994,412 / 天**
- 探针量取台账 §六 的 **1,404 次/天**

**输入侧**（`P_in` = 每次探测的输入 token）：

| P_in | token/天 | 占日输入 | 说明 |
|---|---|---|---|
| 382 | 536,328 | 0.0686% | 悲观下限 |
| **429** | 602,316 | **0.0771%** | ★ **与台账 §六 的 0.077% 精确吻合** |
| 586 | 822,744 | 0.1053% | 我按 chars/4 的估算 |
| 940 | 1,319,760 | 0.1689% | 极端悲观（chars/2.5） |

**输出侧**（`m_out` = 每次探测的输出 token，`max_tokens=1` ⇒ 实际 ≤1，且 **403 的探测产出 0 token**）：

| m_out | token/天 | 占日输出 |
|---|---|---|
| 1 | 1,404 | 0.0128% |
| 4 | 5,616 | 0.0511% |
| — | — | **台账 0.048% ⇒ 反解 m_out = 3.76** |

**反解台账的假设**：`P_in = 0.00077 × 781,495,561 / 1404 = **428.6 token`**，
`m_out = 0.00048 × 10,994,412 / 1404 = **3.76 token`**。
⇒ 台账的 0.077% / 0.048% **可以复算出来**，且其隐含的 `P_in≈429` 与我实测的 2,344 B body
（5.47 B/token）**自洽**。**token 维度不是风险**（即使按最悲观的 940 token 也只有 0.17%）。

### 3.5 按【请求数】计费的风险（真正的风险在这）

台账 §六 已标：*"负反馈风险在「请求数」维度"*，且 §五 明确 *"上游是否按请求数计费 ⇒ 仍未验证"*。把它摊开算：

| 场景 | 探测请求/天 | ÷ 真实均值 3,713.4 | ÷ 真实中位 697 |
|---|---|---|---|
| **现状（含 1min dead 复探）** | **146,208** | **39.4×** | **209.8×** |
| 现状（仅 15min 巡检） | 12,288 | 3.31× | 17.63× |
| 若把 (a) 加到 available（15min） | 12,288（增量） | +3.31× | +17.63× |
| 若 (a) 只做 2h | 1,536 | 0.41× | 2.20× |
| 台账 §六 设计（2h / 冷却节点） | 1,404 | 0.378× | 2.01× |
| **推荐方案上限** | **1,404**（复活）+ 几十（疑点） | **≈0.39×** | **≈2.1×** |
| 上界（若全池 dead） | 227 × 1440 = 326,880 | 88× | 469× |

**风险陈述**（可复算、不夸大）：
- 探测目前已经贡献了真实流量 **39 倍**的请求数；如果上游哪天改成按请求数计费，
  **探测路径单独就会让账单变成原来的 ~40 倍**（且这 146k 里有 133,920 来自 1 分钟 dead 复探，完全是"空转"）。
- 推荐方案把这个暴露压到 **≤1,404/天**，相对现状**降低 99%**（146,208 → 1,404，按上限算）。
- 反向证据（支持"目前没有按请求计费"）：全生命周期里**探测路径从未出现过 429**（`probe status` 直方图里 0 条 429），
  而真实流量有 6,522 次 429（台账 §一）。但这**只能说明 /models 未被限流，不能证明不计费** ⇒ 保持 §6-U4 未验证。

### 3.6 推荐方案的成本（把上面几项合起来）

| 成本项 | 数值 | 依据 |
|---|---|---|
| 疑点探测（(c)） | `≈0` 正常日；坏节点出现时 `≤ 几十 次/天`（每个坏节点每 10 分钟窗口最多 1 次） | §2.3 |
| 复活探测 | `≤ 1,404 次/天`（上限：117 个 IP 拒绝态节点 × 每 2h） | 台账 §六 |
| 被动见证 | **0 次**（复用真实流量的 2xx；`main.go:2684 / 2998` 已有 2xx 钩子） | §4.2 |
| 额外 token | ≤0.17% 日输入 / ≤0.10% 日输出（极端上界） | §3.4 |
| 额外墙钟 | 每次探测 3~5s；复活探测并发 ⇒ 每轮 ≤20s；每 2h 一轮 | §3.3 |
| 新增代码 | 1 个探测函数 + 1 个状态/字段 + 1 个见证登记 + 联锁；不新增配置即默认安全 | §5.2 |

---

## 4. 失效模式分析：整个出口被上游按形态拒绝

### 4.1 为什么"整池打空"是必须正面处理的坑（代码路径）

形态回归（`UPSTREAM-COMPAT.md` / `ADR-0002` 记录过 6 轮，每轮都是"代理立刻全线 403"）时，
**每个出口（含直连、含全部 128 个节点）对推理请求都会返回 403 FreeTierError**。此时如果探针
"看到 403 就标记坏"：

```
所有 available 节点 → markProbeDead → 池内 0 个 available
  → pick() 返回 nil                                      (proxy_node.go:361-363)
  → orderedLocked() 非空但全不可用 → 调用方回退直连        (proxy_node.go:336-339)
  → EgressResult{Client: eg.direct, Direct: true}        (egress.go:130-138)
  → 直连也 403（形态问题与出口无关）
  → 而 quota_halt 的闩【只由直连 429 拉起】               (台账 §七；实测行号 main.go:2831 / main.go:3132，reason=direct_429)
  → 403 不触发熔断 ⇒ gate() 放行、每请求都打直连          (quota_halt.go:229-244)
```

**后果**：用户可见行为完全没变（还是 403），但 (1) 128 个节点全被标死，(2) 切换预算被烧光，
(3) 即便形态问题 10 分钟后被上游修好，池子也要靠逐节点复活才能恢复，**(4) 面板/告警会显示"池子全坏"，
把运维引向错误的排查方向**。这是必须避免的**比现状更糟**的结局。

同时要注意"另一种误判"：如果探针**完全不做闸门探测**（现状），系统会显示"池子健康"（128 available）
却全量 403 —— 这正是 10-17 事故的形态。**要避免的不是"显示健康"，而是"把全池标死"；
正确行为是"池子状态不动 + 高声报警 + 指向形态问题"。**

### 4.2 三道闸（设计）

**闸 1 · 被动见证（passive witness，零成本）**
> 一条"某节点探测返回 403 FreeTierError"的证据，只有在**最近 `W=10` 分钟内存在另一个不同出口拿到过真实上游 2xx**
> 时才被采信为节点级证据；否则一律归因为系统性形状问题。

- 数据源现成：`main.go:2684` 与 `main.go:2998` 已在**每个真实 2xx** 上调用
  `quotaHaltGuard.observeSuccess(egResult.EgressKey())`。只需在旁边加一个极轻的全局登记
  （`atomic` 存 `last2xxUnix` + 最近成功出口集合），**不增加任何上游请求**。
- 为什么成立：形态回归 ⇒ 全出口 403 ⇒ **10 分钟窗口内不存在任何 2xx** ⇒ 所有节点级标记被拒。
  节点 IP 级拒绝 ⇒ 其它 127 个节点持续成功（真实流量均值 3,713/天 ≈ 每 23 秒 1 次）⇒ 见证存在 ⇒ 正常标记。
- 边界：全公司出口 IP 段一起被封（真实存在但概率低）⇒ 见证缺失 ⇒ **安全侧失败**（不标记、只告警）。
  这是可接受的，因为把 128 个节点全标死**并不会让任何请求成功**。

**闸 2 · 池底（pool floor，防止任何单轮/连续轮把池子标空）**
> 探针驱动的 `available → IP拒绝态` 标记，只有在标记后 `available` 仍 `≥ floor = max(8, ceil(0.25 × 标记前 available))`
> 时才提交；触底即停止标记并升级告警。

- 只约束**探针路径**。T1 的 confirm-then-mark 天生不需要它（它要求"另一个节点成功"才标记 ⇒ 恒有节点活着，§2.2）。
- 但**复活**路径要对称：如果池子已经触底，复活探针的优先级要提升（每 15 分钟而不是 2 小时），
  否则"池底"会变成"永久退化到直连"。

**闸 3 · 全局形状闩（shape latch）**
> 一旦"见证 403"或"控制探测 403"发生 ⇒ 置 `shapeSuspect=true` 持续 `W=10` 分钟，窗口内：
> (a) 禁止一切节点级标记（T1 与探针两条路径同时禁用），(b) **不改变任何节点状态**，
> (c) 打 `WARN freetier_shape_regression` + `callLogEvent("shape_regression", ...)`，
> (d) 在 `/api/nodes` 暴露 `shape_suspect` 供面板告警。

- 这条闸的意义：把"全池 403"从一个**节点健康问题**重新分类成一个**全局形态问题**——这才是它的真实性质
  （`UPSTREAM-COMPAT.md` 记录的 6 轮都是全局形态问题）。
- 它**不是**新增的降级路径：闩期间请求照旧走节点（或直连），与现状一致；只是不再做**错误的**节点标记。
- 与既有自愈路径的分工：形态问题由 `revertToPinnedOCVersion`（`main.go:2733-2742`，403+版本更新时回退）
  与 426 刷新路径（`main.go:2718-2728`）负责；本设计不重复也不干扰它们。

### 4.3 行为表 + 不变量证明

记 `A` = 时刻 `t` 的 available 节点数，`A0` = 进入一轮时的 `A`。

| 场景 | 见证（近 10min 有别的出口 2xx） | 探针结果 | 系统行为 | 池子结果 |
|---|---|---|---|---|
| 单节点 IP 被拒 | 有 | 该节点 403 | 标记该节点进 IP 拒绝态，`A -= 1` | `A ≥ 1`（其它节点健康） |
| 一批节点被拒（如某机房段） | 有 | 逐个 403 | 逐个标记，直到触及 floor 停止 | **`A ≥ floor ≥ 8`** |
| **形态回归（全出口 403）** | **无** | 全 403 | **闸 1 拒绝采信 ⇒ 0 标记**；闸 3 置闩 + 告警 | **`A` 完全不变（= A0）** |
| 形态回归 + 池子里恰好有坏节点 | 无 | 403 | 同上，0 标记（不能区分，宁可不标） | `A` 不变 |
| 全出口 IP 段被封（非形态） | 无 | 403 | 同上，安全侧失败 + 告警 | `A` 不变 |
| 池子触底后上游恢复 | 有（恢复后） | 复活探针 2xx | 逐节点复活，`A` 回升 | 单调回升 |

**不变量 I（池子不会被探针打空）**
1. 由闸 1：任何探针驱动的标记都要求存在"别的出口 2xx"的见证。
2. 由闸 2：提交标记前检查 `A_after ≥ floor`，`floor = max(8, ceil(0.25 × A0)) ≥ 8`。
3. 由闸 3：见证缺失/控制 403 时，标记数恒为 0。
⇒ 对任意轮次，`A_after ≥ min(A0, floor) ≥ 8 > 0`。**池子不可能变成 0 个 available 节点。** ∎

**不变量 II（系统不会被"误判成池子健康"而回退直连硬试）**
- 代码上的"回退直连"只有一条触发路径：`egress.Get` 里 `pickForRegion` 返回 `nil`（`egress.go:130-138`），
  即"池子非空但全部不可用"。由不变量 I，该条件在探针路径下不可达；
  而 T1 的 confirm-then-mark 需要"另一节点成功"才标记（§2.2），同样不可达。
- 反向地，本设计**不引入**"池子看起来坏 → 主动直连"的逻辑 ⇒ 不存在第二条回退路径。 ∎
- 附带说明：形态回归期间**直连也不是解法**（直连同样 403，且 403 不触发 `quota_halt`，见 §4.1），
  所以"保持池子不动 + 报警 + 让版本自愈路径工作"是正确行为。

**不变量 III（可观测性不会撒谎）**
- 面板的 `healthy` 计数（`main.go:6907`、`static/admin.html:1102`）在形态回归期间**保持不变**，
  同时 `shape_suspect=true`。运维看到的是"池子没坏，是上游形态/闸门问题"——这正是事实。
- 如果 `shape_suspect=false` 而 `gate_rejected_count` 在涨，那就是**真的**有节点被 IP 拒绝，
  可以据此直接切换/剔除。两种信号不会互相污染。

---

## 5. 推荐方案

### 5.1 方案：事件驱动的闸门感知探针（GASP）+ 复活判据接管

**不新增周期性全池推理巡检。** 探针层只做两件事，检测交给 T1：

```
[检测]  T1 confirm-then-mark（真实流量，0 额外请求）         ← 主路径
          └─ 独立旁路：节点级 FreeTierError 计数 ≥ 2/10min
             → GASP 探针 1 次（§5.1-A）                     ← defense in depth
[联锁]  闸1 被动见证 + 闸2 池底 + 闸3 全局形状闩             ← 对两条路径同时生效
[复活]  IP 拒绝态节点的唯一复活判据 = 闸门探测 2xx（§5.1-B） ← 替代 /models 2xx 与冷却到期
```

**A. 疑点确认探测（对应方案 c）**
- 触发：同一 fp 在 10 分钟滚动窗口内累积 2 次 `FreeTierError` 403 归因（来源：call_log 的
  `upstream_error detail="http 403"` + `err_msg` 含 `FreeTierError`；10-17 的实测归因见 §1.5）。
- 动作：**1 次** `POST /inference/v1/chat/completions`，经该节点的既有 client（`proxy_node.go:739-776`），
  请求体由 `ensureAgentUpstreamShape` 产出形态完整的最小体（`agent_shape.go`；`max_tokens=1`、`stream=true`），
  头部复用 `buildOCRequestWithEndpoint`（`main.go:2517-2546`）以免形成第二份会漂移的形态实现。
- 判定：2xx ⇒ 清计数、不改状态；403 + FreeTierError ⇒ 进入"IP 拒绝态"。
  **必须经过 §4.2 三道闸**；未过闸 ⇒ 不改状态、只记日志。
- 实现位置建议：新文件（如 `health_gate.go`）新增 `checkGateSuspects(ctx)`，
  **不要复用 `checkFiltered`**（`proxy_node.go:605-650`：它的失败分支直接调 `markProbeDead`，正是危险动作）。

**B. 复活判据接管（这是修掉 §1.5 的最小充分改动）**
- 现状的两条"盲复活"路径必须改变行为：
  1. `recordProbeSuccess`（`proxy_node.go:695-712`）：`/models` 的 2xx **不再**把 IP 拒绝态节点翻回 available。
  2. `sweepExpiredLocked`（`proxy_node.go:296-319`）：exhausted 冷却到期**无条件**翻回 available
     （与早已记录的缺陷 B5 同源：*"全程没有一次探测"*）⇒ 对 IP 拒绝态节点必须先过闸门探测。
- 复活探测间隔 **2 小时**（与台账 §六 一致；理由：恢复中位 18.6h、≤1h 恢复率 0.0%），
  单次探测 1 个请求，**上限 117 节点 × 12 = 1,404 次/天**。
- 状态选择有两条路（实现细节由 Lead/T1 定，本文只提约束）：
  - 复用 `NodeExhausted` + 打标 `LastError="freetier:ip_rejected"`：改动最小，但需同时改 `sweepExpiredLocked`
    的复活条件（否则 5 小时后又被无条件放回）。
  - 新增状态（如 `NodeGateRejected`）：语义最干净，但要动 `NodeState` 枚举、持久化
    （`nodeStateFromString`，`proxy_node.go:42-50`）、面板（`main.go:6854`、`static/admin.html`）。
- ⚠️ **不要把闸门探测挂到 1 分钟 dead 复探路径上**（`health_runtime.go:31-36`）：那会把请求数从 ~0 抬到
  `93 × 1440 = 133,920/天`（§3.2）。

### 5.2 代价

| 项 | 代价 |
|---|---|
| 额外上游请求 | **≤1,404/天（上限）**，正常日 ≈ 0~几十；相对现状（146,208/天）**降低 ≥99%** |
| 额外 token | ≤0.17% 日输入 / ≤0.10% 日输出（§3.4 极端上界） |
| 额外延迟 | 疑点探测异步后台执行，**不进请求关键路径**；每轮墙钟 ≤20s（`probeTimeout`） |
| 代码量 | 估 150~250 行 + 测试：1 个探测函数、1 个计数/状态、1 个见证登记（`main.go:2684/2998` 旁）、3 道闸、`nodeAdminView` 加字段 |
| 运维 | 无需新配置项（默认即安全）；`node_health_probe_url` 保持不变（它现在只负责连通性） |
| 风险引入 | 新增状态需同步 `proxy_state.json` 反序列化（`proxy_node.go:882-904`）与面板两处 |

### 5.3 收益

1. **直接消灭本次事故的放大器**：坏节点不再被盲探针静默复活（§1.5 的 21:18→21:33 链条），
   也不会在 15 分钟巡检里"考满分"。
2. **把检测延迟从"永不"降到秒级**（疑点探测）/ 1~2 请求（T1 主路径）；对 10-17 那种 1.6 小时、
   229 次请求全打在一个坏节点上的事故，等效缩短故障窗口 ≥77%（98 分钟 → ≤15 分钟，按 15 分钟巡检点算；
   疑点路径更快）。
3. **把探测请求数从 146,208/天 压到 ≤1,404/天** —— 即使未来上游改按请求数计费，暴露也只有现在的 1%。
4. **给系统一个正确的"全局形态问题"信号**：形态回归期间池子不变 + 明确告警，而不是把 128 个节点标死
   再退化到直连（§4.1）。这是本次改动对"下一次上游收紧"的直接保险。

### 5.4 残留风险

| # | 残留风险 | 缓解 |
|---|---|---|
| R1 | **被动见证在低流量时段失效**（凌晨中位 697/天、最低 27/天 ⇒ 可能出现 >10 分钟无 2xx 的窗口） | 见证窗口 `W` 取 10 分钟而非 2 分钟；无见证时**安全侧失败**（不标记，只记 `gate_probe_unwitnessed`），并让疑点计数继续累积、下一个有流量的窗口再判。**代价：检测可能推迟到有流量为止**（可接受，低流量时段用户少） |
| R2 | 探针自己因为**形态漂移**而 403（例如上游又加了一条闸门条件，探针没跟上） | 探针体复用 `ensureAgentUpstreamShape` + `buildOCRequestWithEndpoint`，与真实请求同源；且闸 1 的见证天然拦截"探针全面 403"的情况（真实流量那时也会 403） |
| R3 | 池底 `floor=8` 在真实"一大片节点被拒"时太小，导致长期退化到少数节点 | 触底即升 WARN；若 `gate_rejected_count/A > 50%` 触发运维告警（换订阅源/换机房），而不是继续标 |
| R4 | 新增状态与已有 `exhausted`/`dead` 转移矩阵冲突（历史上已有 B4/B5 这类缺陷，见 `.superpowers/sdd/2026-10-03-log-perf/audit-proxy-node.md:215-232`） | 先补状态转移单测再改代码（转移完备性表已在审计文档里，可直接扩一行） |
| R5 | `latency_ms` 语义被误改，破坏 `defaultDialTimeout=60s` 的校准依据 | 闸门探测延迟**另存字段**（`gate_latency_ms`），`latency_ms` 保持"冷路径连通性探测"语义（§1.3） |
| R6 | 上游若真的**按请求数计费**，1,404/天 仍占真实中位的 2.0× | 把间隔做成配置项（默认 2h），并保留"发现一次成功即停止"（台账 §六 的策略）；后续可用 §6-U1 抓到的 429 头/body 定论后再调 |

### 5.5 可观测性（怎么在面板/日志里看出它在工作）

**日志事件（建议命名，全部 Info/Warn，不用 Debug —— 生产 DEBUG 总量只有 65 行，等于不可观测）**：

| 事件 | 级别 | 关键字段 | 含义 |
|---|---|---|---|
| `gate_probe_ok` | Info | `node fp latency_ms` | 疑点/复活探测通过闸门（2xx） |
| `gate_probe_rejected` | **Warn** | `node fp status=403 reason=freetier` | 节点被推理闸门拒绝 ⇒ 进 IP 拒绝态（唯一的合法标记路径） |
| `gate_probe_unwitnessed` | Warn | `node fp` | 探测 403 但**无见证** ⇒ 拒绝采信、未改状态（形态回归的指纹） |
| `freetier_shape_regression` | **Warn** | `witness_age_ms available_before marks_suppressed` | 闸 3 生效：全局形状闩，压制了 N 次标记 |
| `gate_probe_floor_hit` | **Warn** | `available floor marks_suppressed` | 闸 2 生效：池底保护，停止标记 |
| `node_gate_rejected` | Info | `node fp reason=freetier:ip_rejected` | 节点进入 IP 拒绝态（`node state changed` 的姊妹事件） |
| `node_gate_revived` | Info | `node fp` | 复活探测通过，节点回到 available（**必须补日志**：现状的复活是静默的，§1.5） |

**API / 面板（`nodeAdminView` `main.go:6848-6863` + `static/admin.html:1102-1104`）**：

- 每节点新增：`gate_state`（`unknown`/`ok`/`rejected`）、`last_gate_probe_at`、`gate_latency_ms`、
  `gate_403_count`（滚动窗口计数）。
- 顶层新增：`gate_rejected_count`、`shape_suspect`（bool）、`shape_suspect_since`、
  `marks_suppressed_total`、`last_2xx_witness_age_ms`。
- 面板：`ovHealthy` 旁加一个红色 `ovGateRejected`；`shape_suspect=true` 时在总览顶部挂一条
  **"上游闸门疑似形态变更，节点池未做任何改动"** 的横幅 —— 这条横幅就是"设计在正确工作"的直接证据。
- **自检口诀**（写进 runbook）：
  1. `shape_suspect=true` 且 `available` 不变 ⇒ **设计正常工作**（形态问题，别去翻节点）；
  2. `available` 下降且 `gate_rejected_count` 上升且 `shape_suspect=false` ⇒ 真实节点级拒绝，
     按节点名去 `call_log.jsonl` 复核；
  3. 出现 `gate_probe_unwitnessed` 但无 `freetier_shape_regression` ⇒ 见证机制本身要查（R1）。

### 5.6 与 T1 的接口（明确不重复设计）

- 本文**不定义** confirm-then-mark 的判定规则、预算、mark 位置、与 `revertToPinnedOCVersion` 的优先级 ——
  这些全部留给 T1（`task-1` / `.research/T1-403-design.md`）。
- 本文向 T1 提出**三条接口约束**：
  1. **mark 的状态必须是"闸门探测才能复活"的那一种**（或触发 `sweepExpiredLocked` 的复活条件改造），
     否则 ≤1 分钟后会被 `/models` 探针静默撤销（§1.5 实证）。
  2. T1 的 mark 也应受**闸 3（全局形状闩）**约束 —— 虽然它的"另一节点成功"条件已天然安全，
     但形态闩能让"全池都没成功"这种极端情况走同一条显式路径，日志/面板只报一个信号。
  3. `latency_ms` 保持连通性语义（R5）。

---

## 6. 不确定项（必须如实标注）

| ID | 不确定项 | 现状 | 影响 | 建议动作 |
|---|---|---|---|---|
| **U1** | 上游是否"先过闸门、再校验模型"（"伪模型探针"能否零输出 token 地区分形状/IP） | 未验证 | 若成立，探测更干净（输出 token = 0）；**不改变 §3 的成本结论** | **需 Lead 批准 1 次请求**：从**一个**节点发 1 次形态完整、`model="__probe__"` 的 POST，看是 404/400 还是 403 FreeTierError。成本：1 次请求 + 2,344 B 输入 + ~0 输出，≈3~5s |
| **U2** | "93 个 dead 节点每分钟被复探一次"（⇒ 133,920 请求/天） | **代码确定**，日志无法直接证实（收口日志是 Debug，生产 DEBUG 仅 65 行） | 决定"现状探测成本"的 92%；也决定"绝不能把闸门探测挂到这个路径上" | 不改配置的前提下：建议在 `health_runtime.go:35` 把 `checked` 升为 Info 或加一个计数器，一次上线即可证实/证伪 |
| **U3** | 闸门感知探测的真实耗时 | 未实测；预期 3~5s（连通 p50 2,274ms + 首次 SSE 帧） | 影响每轮墙钟与并发；不影响请求数结论 | 与 U1 同一次实验顺带测量（有真实流量时的 `connect_ok → 首个 SSE` 也可作为旁证） |
| **U4** | 上游是否按**请求数**计费 | 台账 §五/§六 均标"仍未验证"；T2（`capture-429`）正在取证 | 决定"探测请求数"是不是真金白银 | 等 T2 的 429 头/body 结论；在此之前按"可能计费"设计（即本文的成本最小化） |
| **U5** | `/models` 端点是否消耗免费层配额 | 反向证据：全生命周期 0 次探测 429 | 若消耗，则现状 146,208/天 已经在持续消耗免费额度 | 同 U4 |
| **U6** | 坏节点在 21:24 前后"被复活"是**推断**（`recordProbeSuccess` 无日志；我只有"21:18:56 判死 + 之后无探测判死 + 21:33 起在提供服务"三条间接证据） | 证据链完整但**无直接日志** | 支撑 §1.5 的核心论点；结论方向不受影响（`recordProbeSuccess` 是**唯一**能把 dead 翻回 available 的代码路径，`sweepExpiredLocked` 只管 exhausted） | 接受为推断；补 `node_gate_revived` / 复活日志后即可闭合 |
| **U7** | 生产 `proxy_state.json` 里那条坏节点已不存在（webshare 源 24h 轮换） | 23:29 快照的 227 个 fp 中无 `64.137.121.210` | 无法再从 state 文件复核"22:46:44 latency_ms=557" | 该数字引用分析文档 §5.2；本文用 call_log 的 231 次 403 独立复核了同一结论 |
| **U8** | 403 归因的判据口径 | 我用了 `node_name="1::CL-64.137.121.210:6465"` **精确匹配**得到 231 次；分析文档用 `detail` 含 `403` 的宽松匹配得到 218/220 | 宽松匹配会跨节点/跨错误串味（我第一次用 `'403' in str(ev)` 得到 1,441 条即是此类误报） | 实现时用"`upstream_error` 事件 + `detail=="http 403"` + 该行 `err_msg` 含 `FreeTierError`"三条件精确归因 |

---

## 7. 复算命令清单

**A. 代码证据**
```bash
cd /home/jingyx/project/opencode2api-test
sed -n '218,224p'   proxy_node.go      # defaultHealthInterval / defaultProbeURL / probeTimeout
sed -n '505,530p'   proxy_node.go      # healthIntervalLocked / effectiveProbeURL
sed -n '532,564p'   proxy_node.go      # probeNode：GET、计时起点、2xx 判定
sed -n '605,650p'   proxy_node.go      # checkFiltered：probeAvailable/probeDead、markProbeDead 调用
sed -n '695,712p'   proxy_node.go      # recordProbeSuccess：静默复活
sed -n '16,52p'     health_runtime.go  # 1min ticker / 每分钟 dead 复探 / 15min 巡检
sed -n '2517,2546p' main.go            # 真实推理请求的形态（对比探测）
sed -n '1336,1345p' main.go            # node_health_interval_minutes 接线
```

**B. 生产只读取证（不改配置、不重启、不打节点）**
```bash
docker exec opencode2api cat /data/config.json        # node_health_interval_minutes=15
docker exec opencode2api cat /data/proxy_state.json > /tmp/prod-proxy_state.json
docker exec opencode2api cat /data/stats.json        > /tmp/prod-stats.json
```

**C. 节点数 / 延迟 / 真实流量**
```bash
python3 -c "
import json,collections,statistics
d=json.load(open('/tmp/prod-proxy_state.json'))['nodes']
print(len(d), collections.Counter(v['state'] for v in d.values()))
lat=sorted(v['latency_ms'] for v in d.values() if v['state']=='available' and v['latency_ms']>0)
print('n',len(lat),'p50',lat[len(lat)//2],'mean',round(statistics.mean(lat)),'max',lat[-1])"
# → 227 Counter({'available':128,'dead':93,'exhausted':6}) ; n 128 p50 2274 mean 2835 max 16819

python3 -c "
import json,collections
D=collections.Counter()
for line in open('/tmp/prod-now.jsonl',encoding='utf-8',errors='replace'):
    try: D[json.loads(line)['ts'][:10]]+=1
    except: pass
v=sorted(D.values()); print('days',len(v),'median',v[len(v)//2],'mean',round(sum(v)/len(v),1))"
# → days 55 median 697 mean 3713.4
```

**D. 盲区与坏节点的生产证据**（本文 §1.4 / §1.5）
```bash
docker exec opencode2api sh -c 'grep -o "probe status [0-9]*" /data/opencode2api.log | sort | uniq -c'   # 1×500 2×503，0×403
docker exec opencode2api sh -c 'grep -c "node marked dead by health check" /data/opencode2api.log'        # 28,874
docker exec opencode2api sh -c 'grep -o "\"type\":\"upstream_error\".*\"detail\":\"http 403\"" /data/call_log.jsonl | wc -l'          # 2,445
docker exec opencode2api sh -c 'grep -o "\"node_name\":\"1::CL-64.137.121.210:6465\",\"detail\":\"http 403\"" /data/call_log.jsonl | wc -l'  # 231
docker exec opencode2api sh -c 'grep "node marked dead by health check" /data/opencode2api.log | grep "fp=e347afddb581bf8f7301396470103c7" | tail -1'   # 20:43:00.931
docker exec opencode2api sh -c 'grep -c "level=DEBUG" /data/opencode2api.log'                            # 65
docker exec opencode2api sh -c 'grep -c "msg=\"node health check done\"" /data/opencode2api.log'          # 1,028（15min × 11.1 天 ✓）
```

**E. 探针 body 尺寸**：见 §2.0 的内联 python。
