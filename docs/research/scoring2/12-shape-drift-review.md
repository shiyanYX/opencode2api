# T6b · 429「形态漂移闩死」方案 —— 对抗性评审（反对派）

> 立场：**反对派**。默认假设这是错的，除非证据推翻它。
> 只读评审：不改任何源码、不 commit、不碰生产容器。
> 评审人：review-t1-design（独立于 T6a/T6 实现方）。
> 数据：`/tmp/prod_call_log.jsonl`（10-19 快照 230,793 条）+ 事故口径（Lead 提供：18:03–23:00、9,000+ 429、失败请求 `nodes` 长度=1、手动恢复后**又复发一次**）+ 工作区源码。
> 复现环境：`/tmp/t6rev`（源码副本 + 我写的 `zz_review_t6_test.go`，不进仓库）。

**分节落盘**：本文件由多节追加而成，每节写完即落盘；节末标注该节的复现命令。

## 0. TL;DR（结论先行）

**结论：当前实现 no-go（2 个可复现的 Critical + 1 个并发 Critical）。但存在一个比它更简单、状态更少、效果强约 88 倍的等价方案（本文 §2 的 G1）。**

### 0.1 三个 Critical（都已用真实函数复现，不是纸面推理）

| # | 缺陷 | 一句话证据 |
|---|---|---|
| **C1** | **没有「发送前闸门」**：`Halt` 只在拿到 429 **之后**生效，所以闩住期间每个请求仍然向上游发 1 次；叠加硬 TTL 每 10min 清空 `seen`，等于**每 10 分钟把 212 个出口重新扫一遍** | 真实函数仿真 5h：`sent=5520`、`max_hits_per_node=30`、`direct_sends=30`；且**与请求速率无关**（R=0.5/5/50 结果相同）。30 次/出口 > 封禁阈值 ~16 ⇒ 2h40m 全池烧穿 |
| **C2** | **512 上限的 LRU 淘汰会删掉「已 arm 的桶」** | 先让 `victim` arm，再灌 512 个新 model 名 ⇒ `victim` 被淘汰、`armed=false`；model 名客户端可控 |
| **C3** | **并发/乱序时间戳被误判成「时钟回跳」**，整桶 `resetLocked(true)` | 用比已存时间早 **1µs** 的 `now` 调一次 `shapeDriftObserve429` ⇒ `armed=false, distinct=1`（风暴中高频 429 必然出现这种倒序采样） |
| **C4** | **"静默不切"没有任何结构化记录**：被抑制的尝试无法与失败区分 | `shape_drift` 事件只有 `model/reason/armed/distinct/last_trial`，没有 `suppressed_by`/`would_be_egress`/`blocked_n`；`all_egress_blocked` 只有一句话；`nodes[]` 只含真正发过的出口。按用户规则 ⇒ **Critical**（详见 §5A） |

### 0.2 半开试探烧 IP 的上界（一句话）

**试探本身不贵**：60→120→300s 封顶，≤13 次发送/小时。**真正烧 IP 的是「硬 TTL 清空 `seen`」**：每 10min 一次全池重扫 ⇒ **6 次命中/出口/小时**；按 ~16 次/日封禁，**2h40m 后 212 个出口 + 主 IP 全部达到封禁阈值**（等效 **≈79.5 个 IP/小时**进入封禁）。我的方案同一 5h 场景：**63 次发送、每出口最多 1 次、直连 0 次**。

### 0.3 更简单的等价方案（G1）

**全局 armed + 发送前闸门 + 保留 blocked 集合 + 单飞试探**。相比现实现：去掉 per-model 生命周期/512 上限/TTL 清空/`trialInFlight` 布尔，只留「一个出口集合 + 3 个时间戳」。它同时消掉 C1/C2/C3 和 §6 的惊群/锯齿（细节见 §2）。

### 0.4 两个流程性问题

1. **实现跑在它自己的设计前面**：评审时 `docs/research/scoring2/11-shape-drift-design.md` 仍是**空骨架**（00:07 创建，全部小节「待填」，包括 §7「半开试探的烧 IP 速率上界」）——而实现已经改到第二版（23:32）。本轮 Lead 点名要的那个上界，设计文档里**不存在**。
2. **最关键的接线没有任何测试**：去掉 `ExcludeEgress` 接线（M7）或让试探也 mark（M8），**全量 334s 仍然全绿**（M7 实测；M8 见 §9）。

## 1. 评审基线与方法

### 1.1 基线（sha256 钉死）

| 文件 | sha256 | 大小/时间 |
|---|---|---|
| `shape_drift.go` | `5472a826dd1deb9f38a67c92fc5fa6ac0eae6ae83c3359dab7dbff1a856bef10` | 10,708 B · 10-26 23:32 |
| `main.go` | `b9bb6917e1f3212a…`（shape 接线 6 处；同文件还混有 T2/T3/R4 未提交改动） | 275 KB |
| `shape_drift_recovery_test.go` | 随源码副本 | 15 条既有测试 |
| `docs/research/scoring2/11-shape-drift-design.md` | — | **1,205 B，空骨架，§0–§11 全部「待填」** |

> 说明：`shape_drift.go` 在我上一轮读完之后（23:24→23:32）被实现方改过一次，新增了 512 上限、5s 全局试探节流、抖动、时钟回跳保护。**本评审针对的是 23:32 这一版**；23:24 版的问题（无上限、无节流）已不再成立，文中不重复计分。
> `main.go` 改动不是 T6 独占（T2 的 pool403 halt、T3 的超时分类、R4 的 `callLogSwitchEvent` 都在同一文件）。凡涉及接线，我用 `git diff` 逐段归因。

### 1.2 方法（四路交叉，互不替代）

1. **代码路径**：`shape_drift.go` 全文逐行；接线点 `main.go:2957/2971/3250/3062`（非流式）与 `:3454/3466/3723/3549`（流式）；选路 `egress.go:123-148`；池 `proxy_node.go:1910/1683/788`；熔断 `quota_halt.go:228`。
2. **真实函数仿真**：在 `/tmp/t6rev`（源码副本）里写 Go 测试，**直接调用** `shapeDriftObserve429` / `shapeDriftBlockedEgress` / `pickForRegionExcluding` / `egress.Get` / `nodePool.mark`，按 `main.go` 入站循环的控制流逐请求推进 5 小时虚拟时间。这不是「读代码猜」，是拿生产同一份状态机跑事故参数。
3. **变异测试**：在副本上改坏 8 处，跑仓库自带测试；按「是否变红」判断测试是否纸面绿。**不采信实现方自述的「9 条变异全红」。**
4. **数学上界**：把仿真结果收敛成与 R 无关的公式（§3），并给出与封禁阈值的对照。

### 1.3 我对「事故口径」的独立理解

Lead 给的事实：18:03–23:00 共 9,000+ 429、每条失败请求 `nodes` 长度=1、手动恢复后**又复发一次**。这三条推翻了两个常见假设：

- 「只试 1 个节点」说明**旧闩确实在工作**（它禁止了换节点）——所以事故不是「闩没生效」，而是「**闩只进不出**」。修复的真正目标不是「让闩生效」，而是「**让闩能自己退出**」。
- 「手动恢复后又复发」说明**恢复动作本身不可作为方案**（重启/切节点会清内存态？不——重启会清闩，但也会清掉所有 blocked 证据，于是再烧 3 个出口重新闩上）。任何「靠人」或「靠重启」的路径都必须排除。
- 因此我评的是：**在没有任何人工干预的前提下，5 小时风暴里它能否自己解除，且解除前后不把 IP 烧穿。**

> 数据缺口（如实标注）：10-26 的 call_log 我拿不到（生产只读且不在本机快照里）；`/tmp/prod_call_log.jsonl` 是 10-19 的快照。事故的**宏观事实**采用 Lead 口径，**微观频率**（每出口每 10min 被选中几次）用仿真得出，不冒充实测。

## 2. 独立构造：我认为正确的方案（先不看设计文档）

> 按要求：先自己构造，再对撞。构造时**没有**读 `11-shape-drift-design.md`（当时它也是空的）。

### 2.1 从事故反推的硬约束（任何方案都必须同时满足）

| # | 约束 | 从哪来 |
|---|---|---|
| K1 | **零人工、零重启也能自愈** | 手动恢复后又复发 |
| K2 | 解除判据不能是「窗口滑空」 | 旧闩根因：被钉住的出口持续刷新时间戳 |
| K3 | 必须减少**上游发送次数**，而不只是单请求内的切换次数 | 事故里每条请求只试 1 个节点，5h 仍烧掉 9,000 次 |
| K4 | 闩必须是**全局**的（账号/出口级） | 212 个出口同窗 429；形态闸门按 IP/账号，不按 model |
| K5 | 客户端可控的 model 名不能成为「状态爆炸」或「状态删除」的杠杆 | OpenAI 兼容入口，model 是请求体里的自由字符串 |
| K6 | 全系统只能有**一个**「是否允许发上游」的裁决 | 已有 T2 pool403 halt、quota_halt、NodeExhausted 三套，再叠一套必然出现「一处放行、另一处闩死」 |

### 2.2 我的方案 G1（Global gate）

```text
state (全局一份，不按 model 分叉):
  blocked   map[egressKey]time.Time   // 首次 429 时间；只增不刷新
  armed     bool
  armedAt   time.Time
  backoff   time.Duration             // 60s 起，×2，封顶 600s
  nextTrial time.Time
  trial     struct{key string; at time.Time}  // 单飞令牌（可为空）

# 每个请求，在【选出口之前】调用：
allowSend(now) bool:
  if !armed                         -> true
  if now.Before(nextTrial)          -> false        # 闸门：不发、不选、不烧 IP
  if trial.token != ""              -> false        # 已有试探在飞，单飞
  trial = {key: 选一个 !blocked 的出口; at: now}
  nextTrial = now + backoff + jitter(0..20%)
  return true                                        # 只放行这一个试探

# 429 回来时：
observe429(key):
  if _, ok := blocked[key]; !ok { blocked[key] = now }   # 首次，不刷新
  if !armed && len(blocked) >= 3 { armed = true; armedAt = now; backoff = 60s; nextTrial = now + jitter(60s) }
  if armed && trial.key == key {                         # 只有【这个试探】的失败才算试探失败
     trial = {}; backoff = min(backoff*2, 600s); nextTrial = now + backoff + jitter
  }

# 真实 2xx 时：
observeSuccess():
  armed = false; blocked = {}; trial = {}; backoff = 60s
  # 连续 K=2 次成功（间隔 ≥60s）才算恢复，可选项；见 §6.5

# 闸门拦截时对外返回：
  503 + Retry-After: 30（不要透传上游的 429 —— 429 是"可重试"，会引来惊群）
```

**关键设计点（也是与现实现的分歧点）：**

1. **闸门在发送前**：armed 时除了单飞试探，一个上游请求都不发。这是把烧 IP 从「R 次/秒」降到「1 次/退避窗口」的唯一办法。
2. **blocked 集合只在成功时清空**，TTL 不给「清空」的权力，只给「下一次试探」的权力。这样 212 个出口在一个风暴里**最多各挨 1 次**。
3. **全局一份状态**：形态拒绝是账号/IP 级的；按 model 分桶只会让「A 模型闩住、B 模型继续烧」，且把烧 IP 的轮数乘以模型数。
4. **试探令牌绑到具体出口**：避免现实现里「任意并发 429 都能冒充试探结果」的归因错误。
5. **对外 503 + Retry-After**：把惊群摊平；同时不把「上游的 429 语义」原样传给客户端。

### 2.3 与现实现的差异表

| 维度 | 现实现（23:32） | G1 | 差异后果 |
|---|---|---|---|
| 状态粒度 | per-model map（≤512 桶） | 全局 1 份 | 现实现：多模型风暴烧 N 倍；且可被 client model 名攻击 |
| armed 时是否发新请求 | **发**（Halt 只在 429 后） | **不发** | 现实现：烧 IP 与请求速率同阶 |
| TTL | 10min **清 `seen`** | 只推 `nextTrial`，不清 `blocked` | 现实现：每 10min 全池重扫 |
| 试探归属 | 布尔 `trialInFlight` | 令牌绑 `key` | 现实现：无关 429 会吞掉试探结果 |
| 退避 | 失败升级，成功/TTL 归 base | 失败升级，成功归 base（可选滞回） | 两者都锯齿；G1 无 TTL 归零 |
| model 名 | 分桶键（需 512 上限 + LRU） | 只作日志字段 | G1 不需要任何上限/淘汰，也就不会被淘汰删闩 |
| 状态量 | 512 × (桶 + 最大 213 条 seen) ≈ 6.55 MB 上界 | ≤213 条 + 4 个字段 | G1 小 3 个数量级 |

> 结论：G1 不是「另一个方案」，而是把现实现里**四个互相冲突的补丁**（TTL、per-model、512 上限、trialInFlight）换成一个更小的状态机。它更简单，同时更强。


## 3. 攻击 ①：半开试探 / 闩状态的烧 IP 速率上界

> 结论先行：**烧 IP 的主凶不是半开试探，是「硬 TTL 清空 `seen`」**。试探本身 ≤13 次/小时；TTL 让它变成**每出口 6 次/小时**，5 小时 30 次，超过封禁阈值 10–16 次。

### 3.1 机制：为什么「闩住」不等于「不烧 IP」

现实现的 `Halt` 是**事后**判定，链路是：

```text
main.go:2957  blockedEgress := shapeDriftBlockedEgress(modelID, now)   // 只含已 429 的出口
main.go:2958  egress.Get(... ExcludeEgress: blockedEgress)             // 选一个【未 429】的出口
main.go:3xxx  发上游 ← 这一次已经发生了
main.go:3250  drift := shapeDriftObserve429(...)                       // 拿到 429 之后才知道
main.go:3259  if drift.Halt { break }                                  // 只是不【继续换】，不是不发
```

于是：

1. **闩住期间，每个新请求仍然发 1 次上游**（只是不再一发 429 就换下一个出口）。事故里「每条失败请求只试 1 个节点」正是这个形态——所以 9,000 次失败 = 9,000 次上游命中。
2. 每个请求会**消耗一个尚未 429 的出口**（`ExcludeEgress` 只排除已 429 的），所以 `seen` 会一格格涨到 213（212 节点 + direct）。
3. 涨满后，`egress.Get` 的所有候选都被排除 → 回退 **direct**（`egress.go:143` 的 `// 全部冷却中 → 直连`），此时 `blocked["direct"]` 还是 false → **主 IP 挨一发**；这一发若也是 FreeUsageLimit，才会把 `"direct"` 写进 `seen`，之后才在 `main.go:2971` fail-fast。
4. **硬 TTL 到期**：`shape_drift.go:116-120` 的 `now.Sub(armedAt) >= shapeDriftTTL` → `resetLocked(true)` → **`seen` 被清空** → 212 个出口全部重新变成候选 → 从第 1 步重新来一遍。

第 4 步是放大器：**它把「一次风暴烧一轮」变成「每 10 分钟烧一轮」**。

### 3.2 公式（与请求速率无关的部分）

记号：`N=212`（出口数）、`T=10min`（硬 TTL）、`D`（风暴时长）、`R`（429 请求速率）、`H`（单 IP 当日封禁阈值，实测 **10–16**：`bun_transport.go:15` 写「10+ 次后该 IP 被当日封禁」，`main.go:3248` 写「实测 ~16 次量级」）。

```text
每个 TTL 周期：
  可扫完的前提       R·T ≥ N+1            （R ≥ 0.355/s；事故 0.5/s 满足）
  发送次数/周期      N + 1 = 213          （212 节点各 1 次 + 直连 1 次）
  每出口命中/周期    1
  每出口命中/小时    60/T = 6
  每小时上游发送     6 × 213 = 1,278

达到封禁阈值：
  每出口命中 = D/T  ≥ H
  ⇒ D ≥ H·T
  ⇒ H=10 时 100 分钟；H=16 时 160 分钟
  ⇒ 即 1 小时 40 分 ～ 2 小时 40 分，212 个出口 + 主 IP 全部达到封禁线

等效封禁速率：
  212 / (H·T/60) = 212/1.667 ～ 212/2.667
  ⇒ 127 ～ 79.5 个 IP/小时
```

半开试探自己的贡献：`backoff` 60→120→300 封顶，即 `3600/300 + 1 ≈ 13` 次/小时。**它比 TTL 的 1,278 次/小时小两个数量级。**

若 `R < 0.355/s`（扫不满一轮）：每出口命中 = `R·D/N`，封禁时间 `D = H·N/R`。R=0.1/s 时约 **9.4 小时**才烧穿——所以低流量下它是安全的；**事故的 0.5/s 恰好落在危险区**。

### 3.3 真实函数仿真（不是手算假设）

用生产同一份 `shapeDriftObserve429` / `shapeDriftBlockedEgress` / `pickForRegionExcluding` / `egress.Get` / `nodePool.mark`，按 `main.go` 循环逐请求推进虚拟时间 5 小时（212 个健康节点）：

```text
R=0.5/s 5h: requests=9000  sent=5520 distinct_egress=212 direct_sends=30 direct_from=+6m56s max_hits_per_node=30 marks=60
R=5.0/s 5h: requests=90000 sent=5520 distinct_egress=212 direct_sends=30 direct_from=+42s  max_hits_per_node=30 marks=60
R=50/s  5h: requests=900000 sent=5520 distinct_egress=212 direct_sends=30 direct_from=+4s   max_hits_per_node=30 marks=60
```

三个关键读数：

- **`max_hits_per_node=30` = 5h / 10min**：每个被选中的出口恰好**每 TTL 周期挨一次**。这就是「TTL 周期数 = 每出口命中数」的直接证据。
- **`sent` 与 R 无关**（5,520 恒定）：闩住后每个请求只发 1 次、且被 `seen` 去重；加大 R 不会多烧（但也不会少烧）。**唯一能降低它的旋钮是 TTL**。
- **`direct_sends=30`**：每次 TTL 清空后，第一个「全池已 blocked」的请求会落到主 IP（6m56s 时第一次）。之后 `"direct"` 进了 `seen` 才 fail-fast。**主 IP 也是 30 次 > 10–16 的封禁线。**
- `marks=60`：每周期 2 个「arm 前的普通换节点」会 `mark(NodeExhausted)`，5h 累计 **60 个节点**进入 exhausted 冷却（生产 `node_cooldown_exhausted_hours` 按 Lead 口径 5h）→ 风暴结束后**仍有 60 个节点 5 小时不可用**。

> 仿真保真度：发送次数只在「真正会走到 `client.Do`」的分支计数（`blocked[EgressKey]` 命中时不计），选路用真实 `pickForRegionExcluding`（含 sticky/评分/冷却）。未建模传输层失败差异，因为真风暴下每一次发送都会 429。

### 3.4 给 Lead 的直答

- **「半开试探最坏每小时烧多少个 IP」**：试探 **≤13 次/小时**；但**闩的整体状态**（TTL 清空）是 **212 个出口各 6 次/小时** ⇒ 等价 **79.5–127 个 IP/小时**进入封禁。
- **最坏 5 小时**：212 个出口 + 主 IP 全部达到封禁线；上游发送 5,520 次（事故 9,000 次请求里有 5,520 次真的打到了上游）。
- **修复方向**：不是「把试探退避调大」（它本来就不贵），而是**不要在 TTL 时清 `blocked`**，并且在 armed 时**发送前就拦**。

### 3.5 同一场景下 G1 的数字

```text
PROPOSED: sent=63 direct_sends=0 max_hits_per_node=1 marks=2 armed=true seen=63
```

**5,520 → 63 次发送（88×）**；每出口最多 1 次；主 IP 0 次；NodeExhausted 只 mark 2 个（arm 前的 2 次普通换节点）。这就是「更简单的方案反而更强」的量化依据。

> 复现：`cd /tmp/t6rev && go test -run 'TestRev_StormBurnRate|TestRev_ProposedDesignBurn' -v .`（源码与脚本见 §12）

## 4. 攻击 ②：按 model 分桶的内存与卡片爆炸

### 4.1 model 名确实客户端可控

调用链（全部有代码依据）：

```text
main.go:3883-3884   modelIn := req.Model            // 请求体里的 "model" 字段
                    req.Model = resolveModel(req.Model)
main.go:1465-1499   resolveModelWithDocs:
                      - 命中别名/免费目录 → 返回目录 ID
                      - 否则 `return m`  ← 【原样透传客户端字符串】
main.go:4034/4125   callOpenCodeAPI(..., req.Model, ...)
main.go:3250        shapeDriftObserve429(modelID, ...)   // 用这个字符串做 map key
```

- **没有白名单**：`grep 'knownModels|allowedModels|isKnownModel'` 无命中；未知 model 不是本地 400，而是转发给上游。
- 因此「能不能造出任意 key」只取决于**上游对未知 model 返回什么**：
  - 若上游对未知 model 返回 4xx（model_not_found）→ 不会进入 `classifyQuota` → 不会建桶 → **不可利用**；
  - 若上游对未知 model 也返回 `429 FreeUsageLimitError`（免费层按账号限流时很常见）→ **每个新 model 名一次 429 就建一个桶** → 可利用。
- **我无法验证上游行为**（禁止调用生产上游），所以这一条按「条件性 Critical」记：**只要上游的 429 体不区分 model 合法性，攻击成立**。判定方法见 §8 的上线观察项（`shape_drift_model_cap_evicted` 日志）。

> 注：现实现已加 `shapeDriftMaxModels=512` + 空闲 30min 清理 + LRU 淘汰，**内存爆炸这一半已经堵住**（这是 23:32 版的改进，我确认有效）。真正的洞在下一节。

### 4.2 上限与内存（实测）

- 上限：`shapeDriftMaxModels=512`（`shape_drift.go:41`）；空闲淘汰 30min（`:42`, `:161-174`）；LRU 淘汰（`:178-200`）。
- **最坏内存**（512 桶 × 每桶 213 个出口键，实测）：

```text
RESULT: 512 桶 × 213 出口，heap 增量 6.55 MB（12793 B/桶）
```

**6.55 MB 上界**——对一台网关可以接受，不是 OOM 级问题。但它比「512 × 一个空桶」的直觉大 50 倍：真实风暴下每桶的 `seen` 会涨到 213。

- **CPU/锁**：`shapeDriftMu` 是全局锁；`maybeSweepLocked` 每 1s 扫一遍（O(512)）；`evictOneModelLocked` 在**每次新 model 插入**时 O(512) 全表扫。攻击者用 1 万个不同 model 名持续 429 ⇒ 每次插入都在锁内扫 512 个桶。按每次 ~5µs 估，**约 2,000 次新模型插入/秒就能把这个全局锁打满**，而这条锁在**每个请求的每个 attempt** 上都要拿（`shapeDriftBlockedEgress`）。这是「卡片」风险的真实形态：不是 OOM，是**锁竞争**。

### 4.3 致命点：LRU 淘汰会删掉「已 arm 的桶」（C2）

`evictOneModelLocked` 只看 `lastSeen`，**不区分 armed**。复现（真实函数）：

```text
先 arm victim（3 个出口）→ armed=true distinct=3
再灌 512 个「更新」的 model 名（每个 1 次 429）
RESULT: 灌 512 个新模型名后 victim 仍在=false armed=false distinct=0 buckets=512
⇒ 已 armed 的闩被 LRU 淘汰，闸门失效
```

`lastSeen` 在 `shapeDriftBlockedEgress`（每个请求）里都会被刷新，所以攻击者只要**持续用 512 个新名字发请求**，受害桶（真实模型）就会变成「最久未活跃」而被淘汰。**这是用客户端可控输入删除安全状态的典型杠杆**——比内存爆炸严重得多。

### 4.4 修法（两行级）

1. **淘汰时跳过 armed 桶**：`if b.armed { continue }`；若 512 个都 armed（只有目录级模型才会发生，实际 ≤25），拒绝新建桶或复用「unknown」共享桶。
2. 更彻底：桶键用**解析后的目录 ID**，未知 model 统一落 `__unknown__`（一个桶）。这样键空间 ≤ 目录大小（生产 25 个），**根本不需要 512/LRU**。
3. 若采纳 G1，则整个 per-model map 都不需要——model 只进日志。

> 复现：`go test -run 'TestRev_EvictionDropsArmedState|TestRev_MemWorstCase' -v .`

## 5. 攻击 ③：与 T2 pool403 halt / quota_halt / NodeExhausted 的交互矩阵

### 5.1 四套机制的真实作用域（代码核对）

| 机制 | 触发条件 | 拦截点 | 作用域 | **发送前**拦截？ |
|---|---|---|---|---|
| shape drift（T6） | 10min 内 3 个不同出口 `FreeUsageLimitError` | 选路排除（`ExcludeEgress`）+ 429 后 `break` | **per-model** | ❌ 事后 |
| pool403 halt（T2） | 10min 内 ≥`max(8, 35%·attempted)`（上限 24）个不同节点 403 | 循环顶部 `return 503`（`main.go:2944/3413`） | 池级 | ✅ |
| quota_halt `gate` | 有出口被闩（429）**且** `!hasEligibleNode()` **且** 无出口确认恢复（2 次成功、间隔 ≥60s） | 入口 `return 503 + Retry-After:30`（`main.go:2878/3374`） | 免费层 | ✅ |
| NodeExhausted mark | 非试探的 quota 换节点（`if !drift.Trial`） | 节点 `exhausted` → 5h 不可选（生产口径） | per-node | —（容量） |
| direct 回退 | 所有候选被 `ExcludeEgress` 排除（`egress.go:143`） | 直连主 IP | — | ❌ |

### 5.2 核心结论：**纯 429 风暴下，两个发送前闸门同时失效**

1. **pool403 halt 只认 403**（`pool403` 表由 403 路径写入）。429 形态风暴里没有 403 ⇒ 永不触发。
2. **quota_halt.gate 需要 `!hasEligibleNode()`**（`quota_halt.go:238`）。shape 路径**刻意不 mark**（`main.go:3270` 注释「漂移期 mark 会把健康出口误标 exhausted」）⇒ 池里始终有 eligible 节点 ⇒ `gate` 永远返回 false。
3. 所以「是否发上游」这件事，在 429 风暴里**只剩 shape drift 一家说了算**，而它是事后判定（§3.1）⇒ **没有任何东西能阻止每个请求烧 1 个出口**。

### 5.3 交互矩阵（逐组合核对）

| # | 组合 | 实际行为 | 判定 |
|---|---|---|---|
| 1 | shape armed + pool403 未触发（纯 429） | 全放行，每请求发 1 次 | ❌ **C1**（§3） |
| 2 | shape armed + pool403 触发（混合 403/429） | 403 halt 在循环顶部 503，**反而救场**；但 `poolHaltFor=10min` 到期后回到 #1 | ⚠️ 偶然互补，不可依赖 |
| 3 | shape armed + quota_halt 有 halted 桶 | `hasEligibleNode()==true` ⇒ gate 不生效，仍发 | ❌ 同 #1 |
| 4 | shape armed + **全池** NodeExhausted（超长风暴才可能） | gate 生效 → 503；但 `exhausted` 5h ⇒ **风暴结束后仍闩 5h** | ⚠️ 「一处放行、另一处闩死」 |
| 5 | shape armed + 窗口滑空（10min 内无新 429） | `armed && len(seen)==0` → `resetLocked(false)` → 解闩，**但 `seen` 已空 ⇒ 惊群**（§6.2） | ⚠️ |
| 6 | shape armed + direct | 每 TTL 周期 1 次主 IP 命中（仿真 `direct_sends=30/5h`） | ❌ 见 5.4(a) |
| 7 | pool403 halt 生效 + shape armed | pool403 返回 503，**shape 状态原封不动**；10min 后 403 halt 到期，429 仍在 ⇒ 回到 #1 | ⚠️ |
| 8 | quota_halt 已有多个 halted 桶 | gate 不看它们（池有 eligible 就放行）；被闩的出口**仍可能被选中**（quota_halt 不参与选路） | ❌ 重复 429 |

### 5.4 「一处放行、另一处仍闩死」的确切答案

**没有完全死锁，但有两个半死锁 + 一个定时炸弹：**

**(a) 403-on-direct：最严重的放行漏洞（High，可能升级为 Critical）。**
`main.go:3330` 的 429 分支才调 `shapeDriftObserve429`；**403 走的是 `main.go:3337` 的 `upstream_error` + `break`，根本不进 shape drift**。于是：
- 212 个节点被 429 闩住后，`egress.Get` 回退 direct；
- 若 direct 返回的是 **403 FreeTierError**（不是 429），`"direct"` **永远进不了 `seen`**；
- ⇒ `blocked["direct"]` 永远 false ⇒ **每一个请求都直连、打在主 IP 上**，没有上限、没有退避。
- 而 pool403 halt 只统计**节点**（fp），direct 的 fp 为 ""，通常不被计入 ⇒ 也不会触发。
- 这与 T6 注释「宁可 fail-fast 也不能烧新 IP」完全相反。**「反复触发会升级为封禁」的路径正是 429→403**（`bun_transport.go:15`），所以这不是理论边角。

**(b) 超长风暴后的 5h 容量闩。** shape TTL 10min 放行了，但每周期 2 个 `NodeExhausted`（仿真 `marks=60`）⇒ 60 个节点在风暴后仍 `usable=false`（`node_scoring.go:~330`）最长 5h。健康检查/选路容量骤降 28%，且面板解释为「额度冷却」。不是全闩，但「一处放行、另一处仍限流」。

**(c) 定时炸弹：quota_halt 的历史 halted 桶。** 每次 429 都 `quotaHaltGuard.halt(egressKey)`；形状恢复后，这些桶要「2 次成功、间隔 ≥60s」才 `recovered`。风暴期间几乎没有成功 ⇒ 桶长期 halted。今天 gate 因为 `hasEligibleNode()` 不触发；**一旦有人为了修 C1 把 `hasEligibleNode` 条件去掉（很自然的一步），这些历史桶会让 gate 立刻全闩**，且只能用成功解锁。修 C1 时必须同时清理/隔离 quota_halt 的 halted 状态。

### 5.5 修法

**只保留一个「是否允许发送」的裁决**（G1 的 `allowSend`）。pool403 halt / quota_halt / NodeExhausted 全部降级为**证据与容量**层：
- 它们可以写状态、影响**选哪个**出口，但不能各自决定「发不发」；
- shape 的全局 armed 是唯一能拦发送的闩；403 形态拒绝必须**并进同一个闩**（403 与 429 共用 `seen`，或至少让 403 也走 `shapeDriftObserve429` 的等价记录）；
- direct 必须被同等对待：`blocked` 里没有 `"direct"` 时，**全候选被排除也必须 fail-fast**，不能回退直连。

## 5A. 攻击 ⑤：闩死对**打分数据**的污染（censoring）

> 用户新增维度：闩/半开是否产生足够 censored 观测，让离线打分被系统性误导。
> 结论：**按用户规则判 Critical（C4）**——现设计是「静默不切」，被抑制的尝试**没有结构化记录**，无法区分「未尝试」与「失败」。但要说得准确：节点**质量分**不会被误判坏（quota 不进质量分母），污染体现在**样本冻结、归因错位、流量分布被改写**三处。

### 5A.1 结构证据：被抑制的尝试在哪？（答：不在任何地方）

| 需要的字段 | 现实现 | 证据 |
|---|---|---|
| `suppressed_by` | ❌ 不存在 | `grep -rn 'suppressed' *.go` 仅命中 quota_halt 的注释 |
| `would_be_egress`（本来会试哪个） | ❌ 不存在 | `shape_drift` 事件 detail 只有 `model= reason= armed= distinct= last_trial=`（`main.go:3255-3260`） |
| `blocked_n` / 候选列表 | ❌ 不存在 | `all_egress_blocked` 只有一句话（`main.go:2974/3469`），`node` 常为空 |
| `model` / `node_fp` | ⚠️ 部分 | `model` 只在 detail 字符串里；`node_fp` 是**刚 429 的那个**，不是被抑制的候选 |
| 「未尝试」标记 | ❌ 不存在 | `nodes[]` 只记真正 `client.Do` 过的出口（`calllog.go:51`）；被 `ExcludeEgress` 排除的 212 个候选完全不出现 |

**⇒ 用 `call_log.jsonl` 无法回答三个关键问题**：
1. 这条请求本来会试哪个节点？（无 `would_be_egress`）
2. 有多少次上游尝试被闩抑制了？（无计数）
3. 某个节点这 5 小时「没被记失败」是因为它健康，还是因为它根本没被允许尝试？（无法区分）

按用户给的判据——「若设计是静默不切，则无法区分未尝试与失败，必须判 Critical」——**判 C4 Critical**。

### 5A.2 三条污染路径（带打分代码行号）

**(1) 样本冻结（censoring，最隐蔽）。**
`node_scoring.go:998-1005` 只为 `rec.Nodes` 里的出口累加 `Attempts/EffN`；被 blocked 的出口 0 次尝试 ⇒ `Attempts/EffN/LastTrafficAt` 全部不更新。`stale_evidence` 要「72h 无成功」才触发（`node_scoring.go:~420`），5h 风暴内**不会**触发。⇒ 分数**冻结在风暴前**，风暴期间与风暴后看起来一样好。对「节点质量」这是**正确**的（形态拒绝不是节点错）；但对「样本是否可信」是灾难：离线回算会把这段当成正常流量，**低估风暴对池子的实际影响**。

**(2) 归因错位（面板层）。**
每个被抽中的节点每周期 `QuotaEvents++`（`node_scoring.go:947`），5h ≈ 30 次/节点。面板 reason 文案是「额度耗尽 N 次（不计质量分，只说明当前无额度）」——**把账号级形态拒绝显示成单节点没额度**。没被抽中的节点没有这条 ⇒ 同一场全局风暴在面板上呈现为「一部分节点没额度」，误导人工判断与容量决策。

**(3) 流量分布被改写（选路层）。**
每周期 2 个 `mark(NodeExhausted)`（仿真 `marks=60`）⇒ 60 个节点 `usable=false` 5h（`node_scoring.go:~330` 的 `usable/autoEligible`）。风暴后选路只剩 152 个，**打分器看到的流量份额被闩改写**；若离线按流量份额做质量归因，会把「闩后的集中」误读成「这些节点更好」（选择偏差 / survivorship）。

### 5A.3 半开试探的节点偏差（用户特别问）

- **现实现**：armed 时每个请求仍发 1 次、每次换一个未 blocked 的出口 ⇒ **每个出口每周期被均匀抽 1 次**，不存在「只试探少数节点」。但「哪次 429 算试探结果」由布尔 `trialInFlight` 决定，**不绑定出口**（§6.4）⇒ 试探的失败/成功落在随机节点上，`QuotaEvents` 的分布是**抽样噪声而非节点差异**。
- **G1（我的方案）**：单飞试探会把 63 次发送集中在**一个**出口。若固定同一个出口，该节点会累积 `connect/quota`，在质量分布上**显得更差**——这正是用户担心的偏差。**因此 G1 必须补两条**（已写回 §2）：① 试探目标**轮转**（least-recently-tried）；② 试探事件记录 `would_be_egress` 与 `trial=true`，离线可剔除。

### 5A.4 上线后如何判断打分是否被污染（具体查询）

数据：`/data/call_log/2026-10-27.jsonl`（`calllog.go` 的 `CallRecord`：`model/nodes[]/events[].type/events[].node/events[].detail/status`）。
时间窗：风暴期 `18:03–23:00`，对照期取前 5h。

**Q1 结构完整性（现在必然 0%，修好后应 ≈100%）**
```bash
# shape_drift 事件里带 would_be_egress 的比例
jq -r 'select(.events[]?.type=="shape_drift") | .events[] | select(.type=="shape_drift") | .detail' call_log.jsonl   | awk 'BEGIN{t=0;c=0} {t++} /would_be_egress=/{c++} END{printf "with_would_be=%d/%d (%.1f%%)
",c,t,100*c/t}'
```
判定：`<50%` ⇒ 打分无法区分未尝试/失败，**风暴窗口不能用于训练/回填**。

**Q2 抑制比例（censored ratio）**
```bash
# 有 429 证据的失败请求里，nodes 为空（= 静默抑制、根本没发）的比例
jq -r 'select(.status=="fail" and ([.events[]?.detail // ""]|any(test("429"))))
       | if ((.nodes|length)==0) then "suppressed" else "attempted" end' call_log.jsonl | sort | uniq -c
```
阈值：风暴期 `suppressed/(suppressed+attempted) < 10%`；`>50%` ⇒ 该窗口的可用样本不足，打分置信度必须置 `low_sample`。

**Q3 抽样偏差（谁被记了 quota）**
```bash
# 有 429 事件的节点数 / 池大小；以及每节点 429 计数分布
jq -r 'select([.events[]?.detail // ""]|any(test("429")))
       | .events[] | select(.type=="upstream_error" and (.detail|test("429"))) | .node' call_log.jsonl  | sort | uniq -c | sort -rn | head -20
```
判定：全局形态风暴下应为「≈全池、每节点≈TTL 周期数」；若只有一小部分节点有记录 ⇒ 抽样偏差，面板的「额度耗尽」不可按节点解释。

**Q4 分数稳定性 A/B（最硬的一条）**
用 `node_scoring.go` 的回放（或 `.research/scoring2/scripts/aggregate.py`）分别**含/不含**风暴窗口重算 5h 后的节点分：
```bash
# 伪命令：两次 replay，输出 fp,score
./replay --from 13:00 --to 18:03  > before.tsv
./replay --from 13:00 --to 23:00  > with_storm.tsv
join before.tsv with_storm.tsv | awk '{d=$3-$2; if(d<0)d=-d; if(d>10) print $1,d}'
```
判定：任一节点 `|Δscore| > 10`，或 Top-10 排名 Kendall τ < 0.9 ⇒ **风暴窗口污染了打分**，必须从训练/回填中剔除（或加 `censored=true` 掩码）。

**Q5（辅助）QuotaEvents 均匀性**
期望 ≈ `D/TTL = 30` 次/节点；若出现「部分节点 0、部分 30」⇒ 是抽样，不是节点差异。

### 5A.5 最小修法（让打分可用）

1. `shape_drift` 事件追加：`blocked_n=<len(seen)>`、`would_be_egress=<下一个候选>`、`suppressed_n=<被排除候选数>`、`trial=<bool>`、`global_armed=<bool>`。
2. 新增事件 `shape_suppressed`（type 独立）：带 `model`、`would_be_egress`、`blocked_n`、`reason=armed|all_blocked`。**让「未尝试」可被按节点归因**。
3. 打分侧：`classifyScoringEvent` 把 `shape_suppressed` 归 `class:"excluded"`（`st.Excluded++`，**永不进质量分母**），与 `switch` 一样不覆盖前一条错误归因。
4. `QuotaEvents` 增加 `scope=global` 标记：形态风暴期间不按节点展示「额度耗尽」，改在池级汇总（`shape_drift` 面板）。
5. 回算约定：`armed=true` 的区间默认标 `censored`，训练/回填跳过；若要保留，按 `would_be_egress` 做逆概率加权（被抑制概率 = 1 - 1/候选数）。

## 7. 攻击 ⑤：进程重启、时钟跳变、并发时间戳

### 7.1 进程重启：内存态清零 = 重新烧 3 个出口

`shapeDriftByModel` / `quotaHaltGuard` 都是包级内存变量，不落盘。重启后：
- `armed=false`、`seen={}`、`backoff=60s`、quota_halt 的 halted 桶全空；
- 下一次 429 从「第 1 个出口」重新计数 ⇒ 再烧 3 个出口才重新 arm；
- 若在 CrashLoop/滚动发布中，**每次重启都重复这 3 次**。

**影响**：单次重启的代价可控（3 次命中），但它让「重启 = 人工恢复」这条危险路径变得有吸引力（§1.3：手动恢复后又复发）。**建议**：把 `armedUntil = now + TTL` 这个**粗粒度**状态落盘（与 node 状态同一文件），重启后先进入「只允许单飞试探」60s；不落 `seen`（避免旧证据永久闩死）。

### 7.2 时钟：两个方向都「fail open」

`expireLocked`（`shape_drift.go:100-125`）对时钟的处理：

```go
if b.armed && !b.armedAt.IsZero() && now.Before(b.armedAt) { b.resetLocked(true); return }  // 回拨 → 清闩
for key, first := range b.seen {
    if now.Before(first) { b.resetLocked(true); return }                                     // 回拨 → 清闩
    if now.Sub(first) > shapeDriftWindow { delete(b.seen, key) }                             // 前跳 → 过期
}
if b.armed && now.Sub(b.armedAt) >= shapeDriftTTL { b.resetLocked(true); return }            // 前跳 → 清闩
```

- **回拨**：立即 `resetLocked(true)` ⇒ 解闩 + 清证据 ⇒ **风暴中放行全量**。
- **前跳 ≥10min**：所有 `seen` 过期 + TTL 触发 ⇒ 同样 `resetLocked(true)` ⇒ **放行全量**。
- 生产用 `time.Now()`，Go 的 `Time` 带单调读数 ⇒ **真正的 NTP 步进不会触发**（`now.Sub` 用单调钟）。所以「系统时钟跳变」这条**在本进程内是安全的**——我实测：

```text
monotonic: now.Sub(now-3h)=6h0m0.000000133s (单调读数存在=true)
纯墙钟回拨 1h: now.Sub(armedAt)=-1h0m0s ⇒ TTL(10m) 不触发
```

  但**代价**是：这段「防时钟回跳」的代码把**任何** `now` 早于已存时间的调用都当成回跳——而 `now` 是在**取锁之前**采样的（`main.go:3250` `time.Now()`）。并发下这是**必然**发生的（见 7.3）。

**修法**：① 只在回拨幅度 > 5s 时重置（区分「真回跳」与「并发采样倒序」）；② `now` 在**锁内**取，或每桶维护 `maxSeenNow` 并 `clamp`。

### 7.3 并发时间戳倒序 = 真实的 Critical（C3）

代码路径：`main.go:3250` 先 `time.Now()` 再进 `shapeDriftObserve429`（内部才 `Lock`）。两个 goroutine 的采样与加锁顺序可以颠倒：

```text
G1: now=T1         G2: now=T2 (T2>T1)
                   G2 先拿到锁 → seen[nodeX]=T2, armedAt=T2
G1 拿到锁 → expireLocked(T1) → T1.Before(T2) → resetLocked(true)  ⇒ 整桶清空
```

实测（不需要真并发，1µs 倒序即可复现）：

```text
先 armed（distinct=3）
用 T+2s+2µs 观察 d，再用 T+2s+1µs 观察 e
RESULT: armed=false distinct=1（整桶被假时钟回跳重置）
```

**为什么风暴中必然发生**：0.5 req/s 时每秒 0.5 次采样，两个请求相差 µs 级的概率不高；但**风暴是并发的**（几十上百个在途），且 `shapeDriftBlockedEgress`（每个 attempt 都调）也在同一把锁上做 `expireLocked`。只要有一次倒序落在 `seen` 有值的窗口内，整桶就没了——**然后要重新烧 3 个出口**。这与「闩能自愈」的目标**直接冲突**：自愈靠的是「闩住不烧」，而它在风暴中反复自毁。

**现有测试为什么没抓到**：`TestShapeDriftConcurrentBurst` 给所有 goroutine **同一个 `now`**（相等不触发 `Before`）。测试缺口，见 §9。

### 7.4 小结

| 场景 | 现实现 | 判定 |
|---|---|---|
| 进程重启 | 内存清零，重烧 3 个出口 | ⚠️ Important（可接受，但别把重启当恢复手段） |
| 系统时钟回拨/前跳 | 单调钟保护，实际不触发 | ✅ 安全（但代码里的保护是 C3 的根因） |
| 并发采样倒序 | **整桶重置** | ❌ **Critical (C3)** |
| 锁内 vs 锁外取 now | 锁外取 | 修法：锁内取或 clamp |

## 8. 上线观察指标与回滚判据（可直接执行的查询）

> 前提：先加 §5A.5 的 `would_be_egress` / `suppressed_n` / `shape_suppressed` 字段，否则 S6 无法算。S1–S5 用现有字段即可。

### 8.1 「修复有效」的判据（S1–S5，全部给字段/阈值/窗口）

数据：`/data/call_log/2026-10-27.jsonl`；窗口：每小时对齐；基线：事故 18:03–23:00。

**S1 闩能自愈（最重要，直接对应「手动恢复后又复发」）**
```bash
# 每个 model 的 armed 连续时长（用 shape_drift 事件的 armed=true 与后续解除的间隔）
jq -r 'select(.events[]?.type=="shape_drift") | [.ts,.model,(.events[]|select(.type=="shape_drift")|.detail)] | @tsv' call_log.jsonl  | awk -F'	' '{print $2"	"$1"	"$3}' | sort > /tmp/sd.tsv
# 判定：同一 model 的 armed 首末间隔；阈值 ≤ 20min（TTL 10min + 缓冲）
```
- **PASS**：24h 内每个 model 的 `armed` 连续段 ≤ 20min，且每段里至少出现 1 次 `reason=half_open_trial`（证明试探真的跑了）。
- **FAIL**：出现 `armed` 段落 > 30min 且无 `half_open_trial` ⇒ 试探被 §6.4 顶掉或卡死 ⇒ 回滚。

**S2 上游 429 速率**
```bash
jq -r 'select(.events[]?.detail // "" | test("http 429")) | .ts[0:13]' call_log.jsonl | sort | uniq -c
```
- 基线 ~1,800/h；**PASS** < 180/h（10× 降）；**FAIL** > 360/h 持续 15min。

**S3 烧 IP 广度（每小时被 429 的不同节点数）**
```bash
jq -r 'select([.events[]?.detail // ""]|any(test("429")))
       | .ts[0:13] + "	" + ([.events[]|select(.type=="upstream_error" and (.detail|test("429")))|.node]|unique|length|tostring)'   call_log.jsonl | awk -F'	' '{a[$1]+=$2} END{for(h in a) print h,a[h]}' | sort
```
- **PASS**：≤ 5 个不同节点/小时（3 个 arm + 1 个试探 + 余量）。
- **FAIL**：> 20 个/小时 ⇒ TTL 清空仍在扫全池（C1）⇒ 立即回滚。

**S4 单请求尝试数（防止「修好后反而多烧」）**
```bash
jq -r 'select(.status=="fail") | (.nodes|length)' call_log.jsonl | sort -n | uniq -c
```
- 事故形态：≈1。**PASS**：429 失败请求 `len(nodes) ≤ 1.2` 平均；**FAIL**：>2（说明闩没拦住换节点）。

**S5 主 IP 命中（direct 漏洞哨兵）**
```bash
jq -r 'select([.events[]?.detail // ""]|any(test("429")))
       | .events[] | select(.type=="upstream_error" and (.detail|test("429")) and (.node=="" or .node=="direct")) | .ts'   call_log.jsonl | cut -c1-13 | sort | uniq -c
```
- **PASS**：0–1 次/小时（每个 TTL 周期最多 1 次）；**FAIL**：≥5 次/小时 ⇒ §5.4(a) 的 403-on-direct 被触发 ⇒ 立刻回滚。

**S6 打分污染（§5A 的 Q1/Q4）**
- Q1 `would_be_egress` 覆盖率 ≥ 90%；
- Q4 含/不含风暴窗口的节点分：`max|Δ| ≤ 10` 且 Top-10 Kendall τ ≥ 0.9。
- 任一不满足 ⇒ 风暴窗口从训练/回填剔除。

### 8.2 回滚判据（任一命中立即回滚）

| # | 条件 | 含义 |
|---|---|---|
| R1 | 某 model `armed` 连续 >30min 且期间 0 次 `half_open_trial` | 试探卡死（§6.4） |
| R2 | 上游 429 >360/h 持续 15min | 闩没减少 burn |
| R3 | 每小时 distinct 429 节点 >20 | TTL 扫全池（C1） |
| R4 | 主 IP/direct 429 ≥5 次/h | 403-on-direct（§5.4a） |
| R5 | 出现 `shape_drift_model_cap_evicted` 日志（若实现方加）或 `shapeDriftModelCount` 长期=512 | 桶被灌满/armed 桶可能被淘汰（C2） |
| R6 | 客户端 5xx（非上游透传）占比 >10% 持续 15min 且池内可用节点 ≥100 | 闩误伤健康流量 |

### 8.3 回滚动作（必须先具备，否则「判据」是空话）

**当前没有 kill switch**（`grep -n 'SHAPE_DRIFT_DISABLE\|shapeDriftEnabled' *.go` 无命中）。回滚只能改代码重发。
**最小要求**：加一个环境变量/配置项 `shape_drift_disabled`，让 `shapeDriftObserve429` 直接返回零值（`Halt=false, Trial=false`），**保留事件日志**。这样 R1–R6 任一命中时，运维可以在**不重发**的情况下把闩降级为「只观测」。
> 注意：**不要**用「调大 TTL」当回滚——按 §3 的公式，TTL 越大，每出口命中数越多（D/T），是反效果。

## 9. 变异测试：5 个必须变红 + 我实际跑出的红/绿

方法：`/tmp/t6rev` 副本上逐条改坏，跑**仓库自带**测试（不看我自己的测试），对照「是否变红」。全部改回后 `diff` 校验（`ALL_RESTORED`）。

### 9.1 五个「必须变红」（全部通过）

| # | 变异（改坏什么） | 期望被谁抓到 | 实测 |
|---|---|---|---|
| **M2** | `now.Sub(b.armedAt) >= shapeDriftTTL` → `>= 100*time.Hour`（拆掉硬 TTL） | `TestShapeDriftHardTTLDisarms` | ✅ **红**（另 + `TestShapeDriftTTLBoundaryPlusMinus1ms`） |
| **M3** | 删掉 `if len(shapeDriftByModel) >= shapeDriftMaxModels { evictOneModelLocked(now) }` | `TestShapeDriftModelNameCardinalityCap` | ✅ **红** |
| **M4** | 去掉全局 5s 试探节流（`if true` 常真） | `TestShapeDriftTrialGlobalRateLimit` | ✅ **红** |
| **M5** | `shapeDriftObserveSuccess` 不清闩（删掉 `resetLocked(true)`） | `TestShapeDriftTrialSuccessDisarms` | ✅ **红**（另 + `TestShapeDriftModelIsolation`） |
| **M6** | `nextShapeDriftTrialBackoff` 直接 `return d`（退避不升级） | `TestShapeDriftHalfOpenSingleTrialAndBackoff` | ✅ **红** |

原始输出（节选）：

```text
## M2 硬 TTL 失效
--- FAIL: TestShapeDriftHardTTLDisarms
--- FAIL: TestShapeDriftTTLBoundaryPlusMinus1ms
## M3 去掉 512 模型桶上限
--- FAIL: TestShapeDriftModelNameCardinalityCap
## M4 去掉全局 5s 试探节流
--- FAIL: TestShapeDriftTrialGlobalRateLimit
## M5 2xx 不清闩
--- FAIL: TestShapeDriftTrialSuccessDisarms
--- FAIL: TestShapeDriftModelIsolation
## M6 试探退避不升级
--- FAIL: TestShapeDriftHalfOpenSingleTrialAndBackoff
```

### 9.2 三个「本该红、却是绿」的变异 —— 测试缺口（这才是重点）

| # | 变异 | 直觉上应该抓它的测试 | 实测 | 结论 |
|---|---|---|---|---|
| **M1** | `if _, ok := b.seen[key]; !ok { b.seen[key]=now }` → **`b.seen[key]=now`**（同一出口重复 429 也刷新首次时间 —— **事故根因本身**） | `TestShapeDriftArmsAfterThreeDistinctAndDoesNotRefreshFirstSeen` | ❌ **绿** | **测试是纸面绿**：该测试的 `shapeDriftTTL == shapeDriftWindow == 10min`，TTL 在窗口滑空之前就先把 `seen` 清了，**根本走不到「窗口是否被刷新」的判定**。要抓它必须在测试里把 `TTL` 调大（如 1h）或断言 `seen[key]` 的首次时间不变。 |
| **M7** | 去掉两处 `ExcludeEgress: blockedEgress` 接线（选路不再排除已 429 出口） | 应有一条集成测试断言「闩住后不会换回已 429 的出口」 | ❌ **全量 334s 全绿** | `TestPickExcludesShapeDriftBlockedEgress` 只测 `pickWithMetaExcluding` **本身**，不测 `main.go` 有没有把集合传进去。**接线是零覆盖**。 |
| **M8** | `if !drift.Trial { mark(...) }` → **试探也 mark**（半开试探会把健康节点误判死 5h） | 应有一条测试断言「`drift.Trial=true` 时不 mark」 | ❌ **全量 226s 全绿** | 同上：`drift.Trial` 的**唯一安全语义**（试探不记节点责）没有任何测试。 |

原始输出：

```text
## M1 同一出口重复 429 也刷新首次时间
--- PASS: TestShapeDriftArmsAfterThreeDistinctAndDoesNotRefreshFirstSeen
...
## M7 去掉 ExcludeEgress 接线（全量）
ok  github.com/shiyanYX/opencode2api  334.201s
## M8 试探也 mark（全量）
ok  github.com/shiyanYX/opencode2api  226.484s
```

### 9.3 判定

- 「5 个必须变红」**满足**（M2–M6 全红），说明**状态机主体的单元测试是真实的**。
- 但 **3 个更致命的性质没有测试**：①事故根因（不刷新首次时间）被 TTL 掩盖；②选路接线；③试探不记节点责。这三条恰好是「修好了但下一次重构会静默退回」的位置。**上线前至少补 ①③ 两条断言，② 加到集成测试。**

> 复现：见 §12.4（脚本逐条给出）。

## 10. 与设计文档（11-shape-drift-design.md）及实现逐条对撞

### 10.1 设计文档现状：**空骨架，无法对撞**

评审时（00:07 创建，1,205 B）全文只有标题与 12 个「待填」。这意味着：

- Lead 点名要的 **§7「半开试探的烧 IP 速率上界」在文档里不存在**——而实现已经写到第二版（23:32）。**实现跑在它自己的设计前面**，这是流程风险：23:32 那版新增的 512 上限、5s 节流、抖动、时钟回跳保护，**没有任何设计文档背书**，也没有对应的评审。
- 文档 §9「对当前工作区 T6 实现（capture-429）的核对意见」也是空的 ⇒ 设计与实现从未对撞过。

### 10.2 用「实现的注释」当设计，逐条对撞

| 实现的自我声明（`shape_drift.go` 文件头） | 实际代码 | 判定 |
|---|---|---|
| 「每个出口只记【首次】429；窗口过期即删除」 | ✅ 属实（但 TTL 会整表清空，见下） | ⚠️ 半真 |
| 「解除三条任一：窗口空 / 任一出口真实 2xx / 硬 TTL 10min」 | ✅ 属实 | ✅ |
| 「armed 后半开试探：每 60s 允许切到一个不在 429 集合里的出口」 | ❌ **不准确**：试探不是定时器，而是**只有在下一条 429 到来时**才可能被授予（`shapeDriftObserve429` 内部）；没有 429 就没有试探，有 429 时试探被随机那条请求拿走 | ⚠️ 语义偏差 |
| 「试探成功立即清闩」 | ✅ 但**单次**成功即清（§6.1） | ⚠️ 过松 |
| 「再 429 ⇒ 升级退避（上限 300s）」 | ⚠️ 会被 §6.4 的无关 429 提前升级，也会被一次成功/TTL 清零 | ⚠️ 非单调 |
| 「全局最多每 5s 放行一次试探，避免所有请求同一毫秒惊群」 | ✅ 试探层面成立；**但解除后的惊群完全没覆盖**（§6.2） | ⚠️ 位置不对 |
| 「状态按 model 分桶；必须有 512 上限 + 空闲淘汰，防止被打爆内存」 | ✅ 属实，且我实测 6.55MB 上界 | ✅ 但引入 C2（淘汰删 armed） |
| 「硬 TTL 10min：清闩并清空该 model 的 429 集合，避免刚清闩又被旧记录立刻 arm」 | ✅ 代码如此 | ❌ **这正是 §3 的烧 IP 引擎**：清空集合 = 全池重新可扫 |

### 10.3 结论

- 设计文档**目前不能作为评审对象**。等它填完后，必须至少回答：①烧 IP 上界（§3 已给出可用公式）；②为什么 armed 期间还要发请求；③TTL 清空 blocked 的理由；④与 quota_halt/403 的唯一裁决点。
- 实现的**自我声明与代码有 3 处语义偏差**（试探非定时、单次成功即清、全局节流位置）。
- 若按本文 §2 的 G1 重写，设计文档可以缩短到「一个状态机 + 一张交互矩阵 + 一组 S1–S6 指标」，比现在更容易评审。

## 11. 缺陷清单（Critical / Important / Minor）与 go/no-go

### 11.1 Critical（必须修，否则不建议上线）

| ID | 缺陷 | 证据 | 最小修法 |
|---|---|---|---|
| **C1** | armed 期间**无发送前闸门** + TTL 清空 `seen` ⇒ 每 10min 全池重扫，2h40m 烧穿 212 个出口 + 主 IP | §3.3 仿真：`sent=5520, max_hits=30, direct=30`；公式 `D ≥ H·T` | `allowSend()` 前置闸门；**TTL 不清 `seen`**，只推 `nextTrial` |
| **C2** | LRU 淘汰删掉 **armed 桶**（客户端可控） | §4.3：灌 512 个新 model 名 → `victim` 被淘汰、`armed=false` | 淘汰跳过 armed；或按目录 ID + `__unknown__` 分桶；或 G1（不分桶） |
| **C3** | 并发/乱序时间戳被当成时钟回跳 ⇒ **整桶重置** | §7.3：早 1µs 的 `now` → `armed=false, distinct=1` | 仅大幅回拨才重置；`now` 锁内取或 clamp |
| **C4** | **静默不切**：被抑制的尝试无结构化记录，无法区分「未尝试/失败」 | §5A.1：无 `suppressed_by`/`would_be_egress`/`blocked_n`；`nodes[]` 只含真发出的 | 加 `shape_suppressed` 事件 + 4 个字段；打分侧归 `excluded` |

### 11.2 Important（应在同一批修，或至少上线前有缓解）

| ID | 缺陷 | 证据/位置 |
|---|---|---|
| I1 | 403-on-direct：403 不进 shape 闩，`direct` 永不入 `seen` ⇒ 每请求直连烧主 IP | `main.go:3337` vs `:3250`；§5.4(a) |
| I2 | 退避**不单调**：一次 2xx 或 TTL 把 300s 清零 | §6.1 实测 |
| I3 | 解除惊群：透传 429、无 `Retry-After`、同步解除 | §6.2 |
| I4 | `trialInFlight` 不绑出口，0.5 req/s 下试探结果 ~100% 被顶掉 | §6.4 实测 |
| I5 | 60 个 `NodeExhausted` 5h mark ⇒ 风暴后容量 -28% | §3.3 `marks=60` |
| I6 | **没有 kill switch**：回滚只能重发；且「调大 TTL」是反效果 | §8.3 |
| I7 | quota_halt 历史 halted 桶是定时炸弹（一旦去掉 `hasEligibleNode` 条件就全闩） | §5.4(c) |
| I8 | 测试缺口：M1（事故根因）/M7（接线）/M8（试探不记责）全绿 | §9.2 |
| I9 | 设计文档空骨架，实现无设计背书 | §10.1 |
| I10 | 打分污染三路径：样本冻结 / QuotaEvents 归因错位 / 流量分布改写 | §5A.2 |

### 11.3 Minor

| ID | 问题 |
|---|---|
| m1 | 全局锁 `shapeDriftMu` 在**每个 attempt** 上都要拿；512 上限满时 `evictOneModelLocked` O(512) 在锁内 ⇒ 新 model 洪泛可打满锁（§4.2） |
| m2 | `blockedCopyLocked` 每次 attempt 分配一个 map（空时也分配）⇒ 健康态也有分配开销 |
| m3 | `shapeDriftShouldHalt` 遗留入口会创建 `""` 空桶（生产已无调用，仅测试） |
| m4 | 文件头注释「每 60s 允许切到…」与代码语义不符（试探非定时） |
| m5 | `OnceLocked` 淘汰按 `lastSeen`，而 `lastSeen` 被**每个请求**刷新 ⇒ 「空闲」不等于「不活跃」 |

### 11.4 有没有更简单等价方案？

**有。** §2 的 **G1**：全局 armed + 发送前闸门 + 保留 blocked + 单飞试探（令牌绑出口）+ 503/Retry-After。状态从「512 桶 × 213 条 seen + 6 个字段」缩到「1 个 ≤213 的集合 + 4 个字段」；仿真同一 5h 场景 **5,520 → 63 次发送**，每出口 30 → 1 次，主 IP 30 → 0 次；同时天然消掉 C2（无桶可淘汰）、C3（无 per-bucket 时钟判定）、I2（无 TTL 清零）、I3（闸门+503）、I4（令牌）。
**代价**：armed 时对客户端返回 503 而非上游 429（语义变化，需要产品确认）；恢复期需要「单飞爬坡」（G1 已含）。

### 11.5 go / no-go

**当前实现：NO-GO。**
- 不建议以「修了 TTL/per-model」的形态直接上线：C1 会在下一次真风暴里把可持续 5h 的事故变成「2h40m 烧穿全池 + 主 IP」。
- 若必须上，**至少**打三个补丁：① TTL 不清 `seen`（1 行）；② 淘汰跳过 armed（1 行）；③ 加发送前闸门 + `would_be_egress` 记录（约 40 行，见 §2.2）。补丁后可进灰度，但 C3（并发倒序）仍需修（3 行）。
- **推荐**：按 G1 重写（约 150 行，可保留现有事件/日志字段），并补齐 §9.2 的三条测试。

## 12. 附录：复现命令与原始输出

### 12.1 环境

```bash
SRC=/home/jingyx/project/opencode2api-test
DST=/tmp/t6rev
rm -rf $DST && mkdir -p $DST/static
cp $SRC/*.go $SRC/go.mod $SRC/go.sum $SRC/oc_prompt_v2.txt $SRC/oc_tools_v2.json $SRC/free_models_cache.json $DST/
cp $SRC/static/*.html $DST/static/
rm -f $DST/task8_bucket_query_test.go          # 未跟踪且已移走的文件，不还原
export PATH=$PATH:/usr/local/go/bin \
  GOCACHE=$SRC/.gocache GOMODCACHE=$SRC/.modcache GOTMPDIR=/tmp/t6rev_gotmp
```

### 12.2 仿真（§3/§4/§6/§7 的所有数字）

```bash
cd /tmp/t6rev
go test -count=1 -run 'TestRev_' -v . 2>&1 | grep -E 'RESULT|PROPOSED|R=|PASS|FAIL'
```

原始输出（2026-10-27 00:1x 重跑）：

```text
R=0.5/s 5h: requests=9000  sent=5520 distinct_egress=212 direct_sends=30 direct_from=+6m56s max_hits_per_node=30 marks=60
R=5.0/s 5h: requests=90000 sent=5520 distinct_egress=212 direct_sends=30 direct_from=+42s  max_hits_per_node=30 marks=60
R=50/s  5h: requests=900000 sent=5520 distinct_egress=212 direct_sends=30 direct_from=+4s   max_hits_per_node=30 marks=60
RESULT: 两个出口全被排除 → Get 返回 Direct=true，EgressKey()="direct"，blocked[direct]=false ⇒ 主 IP 仍会发请求
RESULT: 512 个 model 桶，11 分钟后仍 512 个，约 3 B/桶（注：这是插入/淘汰churn 的净值；稳态上界见内存测试 6.55MB）
RESULT: A 的试探刚放行 1s，B 的无关 429 把 trialInFlight 清成 false、退避推到 2m0s（A 的真实结果被忽略）
RESULT: 退避升到 5m0s 后，一次 2xx 立即 armed=false seen=0、退避回到 1m0s（锯齿，非单调）
RESULT: TTL(10min) 到期后 seen 被清空 ⇒ 之前 429 的出口重新进入候选，无试探、无单飞，全部流量同时回流
RESULT: 灌 512 个新模型名后 victim 仍在=false armed=false distinct=0 buckets=512 ⇒ 已 armed 的闩被 LRU 淘汰
RESULT: 后到的 1µs 早时间戳 → armed=false distinct=1（整桶被假时钟回跳重置）
RESULT: m1.trial=true m2.trial=false（全局 5s 节流下同一窗口只应放行一个）
monotonic: now.Sub(now-3h)=6h0m0.000000133s (单调读数存在=true)
纯墙钟回拨 1h: now.Sub(armedAt)=-1h0m0s ⇒ TTL(10m) 不触发
PROPOSED: sent=63 direct_sends=0 max_hits_per_node=1 marks=2 armed=true seen=63
RESULT: 512 桶 × 213 出口，heap 增量 6.55 MB（12793 B/桶）
```

### 12.3 变异（§9）

```bash
# M1–M6：改 shape_drift.go，跑 go test -run 'ShapeDrift|PickExcludes' .
# M7：删两处 ExcludeEgress，go test -skip 'TestIP4RV' ./...   -> ok 334.201s
# M8：删两处 if !drift.Trial，go test -skip 'TestIP4RV' ./... -> ok 226.484s
```

### 12.4 我写的测试文件（不进仓库）

`/tmp/t6rev/zz_review_t6_test.go`，包含：`TestRev_StormBurnRate`、`TestRev_ProposedDesignBurn`、`TestRev_AllBlockedFallsBackToDirect`、`TestRev_BucketUnboundedNeverDeleted`、`TestRev_EvictionDropsArmedState`、`TestRev_TimestampInversionResets`、`TestRev_TrialAttributionRace`、`TestRev_FlapResetsBackoff`、`TestRev_TTLResetReopensBlocked`、`TestRev_TrialGlobalThrottle`、`TestRev_ClockJump`、`TestRev_MemWorstCase`。

### 12.5 未验证项（如实标注）

1. **10-26 事故日志**：本机没有该快照（`/tmp/prod_call_log.jsonl` 是 10-19），事故宏观事实采信 Lead 口径；微观频率用仿真。
2. **上游对未知 model 的响应体**：未验证（禁止打生产上游）⇒ C2 的攻击可行性是条件性的。
3. **真实 Bun + TLS 下的 403-on-direct**：未构造端到端（无现成 harness），结论来自代码路径。
4. **设计文档**：评审时为空骨架，§10 的对撞基于实现注释与代码。
