# Task 3 交付：超时分层（建连硬超时 + 首字节 deadline）

- 任务：`docs/superpowers/plans/2026-10-26-node-selection-v2.md` Task 3（task-111）
- 状态：**已按独立评审 F1/F2/F3 修复，待 Lead 复验**（未提交、未部署）
- 评审意见：`docs/research/scoring2/08-task3-review.md`（spec ✅ / quality ❌：3 个断言洞 + 1 个未申报副作用，无代码缺陷）
- 结论：建连 60s → **8s**；首字节（= 首 token）等待 无限/60s → **19.59s**（全局 ttft p95 13.06s × 1.5，下限 15s）。
  **最差等待 171s → ≤27.6s（单次尝试）**；整请求回放最差 120.0s → 87.4s（理论上界 147.6s）。
- 明确**不**压 p95：p95 由 mimo 长生成驱动（05-latency §0：节点 ICC=0.0%、node 只加 2.2pp 方差），本任务只给病态等待设上限。

---

## 0. 评审修复（F1 / F2 / F3 + 复审 N2 / halt 缺口）

| Finding | 处理 |
|---|---|
| **F1** 节点池 8s 建连上限无行为测试 | 新增 `TestNodePoolDialCappedByConnectTimeout`（2s 慢 dial + `dialTimeout=150ms`，断言 ≤1.5s）；新增 `TestDirectDialTimeoutWired`（`directDialer.Timeout == connectTimeout == 8s`）；新增 `TestGlobalTTFTP95ConcreteValue`（`globalTTFTP95==13060ms`、`firstByteTimeout==19590ms`，不再是自指恒真）。变异 M6/M8/M9 均变红 |
| **F2** 首字节超时把节点判死 | **行为已改**：`isFirstByteTimeout()` 识别 `timeout awaiting response headers`（Go/Bun 两路），`connect_error` 分支不再 `mark(NodeDead)`，只重试/换节点；建连阶段超时仍 mark dead。事件改用既有 `ttft_timeout`（面板「首字超时」、聚合归 timeout、node_scoring 不计 connect 失败权重）；switch 事件 reason 相应改为 `ttft_timeout:`。两个重试循环（非流式/流式）都改。测试 `TestFirstByteTimeoutDoesNotMarkNodeDead` / `TestDialTimeoutStillMarksNodeDead`；变异 M7 变红 |
| **F3** 报告口径 | §1 改写：不再声称「先量后定测过建连」，如实写成「按两档规则选 8s；真实建连分布不可观测，待 T5 的 `dial_ms` 复核」；补独立 10-19 快照区间与失败侧不对称证据。§2 改写：deadline 实际卡的是**慢 prefill/慢首 token**，不是「响应头不来」 |
| **N2**（复审）告警层仍把首字节超时算传输失败 | 两处改为 `nodeAlerts.recordNodeAttempt(nodeFp != "", err == nil \|\| isFirstByteTimeout(err))`：连接已建好 ⇒ 出口可达，不给 C-5 `runFailures` 加账。同步更新 3 个本地源码形状测试（`zz_c5v2_verify_wiring_test.go`、`zz_node_alert_test.go`、`zz_c5_verify_wiring_test.go`） |
| **halt 守卫**（既往缺口） | `noteAttemptTransportError` 的 mark 增加 `!pool.poolHalted(time.Now())`：池级 halt 期间建连失败也不 mark（与 T2「halt 期间不 mark 任何节点」一致）。测试 `TestHaltSuppressesMarkOnConnectError`；变异 M10 变红 |

---

## 1. 8s 是怎么选的：按两档规则，不是「测出 5s 不行」

### 1.1 事实：纯建连耗时**不可观测**

- 生产 `call_log` 的成功样本只有 `ttft_ms`（请求开始 → 上游首个 `data:`），**没有 `dial_ms`**（字段在 `calllog.go` 里有，但 10-26 快照 0 条输出；T5 才会产出）。
- 评审用 10-19 独立快照（n=230,793）复算：响应头到达时间 `hdr` 与 `ttft` 几乎相同（干净子集 n=13,778：`ttft−hdr` p50=**1ms**、p90=**244ms**）⇒ **不存在更紧的 dial 代理变量**。
- 因此只能给区间，不能给点估计：

| 口径 | 5s | 8s | 说明 |
|---|---|---|---|
| 成功请求 ttft（10-26 快照 n=864） | 29.05% | 15.05% | 上界：含建连+响应头+首 token |
| 成功请求 ttft（10-26 全窗口 n=9,045） | 21.84% | 12.96% | 同上 |
| 成功请求 ttft（10-19 评审快照） | — | **18.69%** | 独立复核 |
| 成功请求 `hdr`（10-19 评审快照） | — | **8.95%** | 比 ttft 更紧的上界 |
| 节点探测整请求延迟（10-26 `/api/nodes`，n=144，实现方自报） | 9.03% | 6.25% | 整请求 RTT，仍非 dial |
| **真实 `dial>8s`** | — | **[0, 8.95%~18.69%]** | **不可观测** |

### 1.2 决策：按计划的两档规则取 8s，并接受「0.5% 门槛未满足」

- 5s 档在所有口径下都远高于 0.5%（≥9.03%），被规则排除。
- 8s 档上界 8.95%~18.69% 仍高于 0.5% ⇒ **计划自己设的 0.5% 门槛并未满足**。报告如实记录这一点，不写成「已测过、达标」。
- 选择 8s 的理由是**风险不对称**，不是误杀率达标：
  - **失败侧收益确定**：评审快照里 `connect_error` 等待 **94.6% > 8s、p50=22s、p90=135s**（实现方 10-26 快照同向：n=1,901，p50=16.3s、p95=60.0s、max=108.9s）。171s 的主体就是这条尾部。
  - **误杀代价可自愈**：dead 冷却仅 1 分钟（`deadCooldown=time.Minute`），被误判的节点冷却后可再次被选中/被健康探测复探；而 8s 之上的失败等待是分钟级。
  - **误杀率是上界**：真实 dial 只是 ttft 的一小段（ttft p50 仅 3.2s），`hdr>8s` 的 8.95% 也含响应头与首 token 排队。
- **待复核**：T5 产出 `dial_ms` 后，用真实 dial 分布重算 5s/8s 误杀率；若 `dial>8s` 的真实占比 >0.5%，再按数据调整（回 15s 或按节点分档）。

---

## 2. 首字节 deadline：全局 p95 × 1.5（不按节点）

- 依据：`.research/scoring2/05-latency.md` §0 —— 上线后节点对 ttft 的 ICC = **0.0%**（95% CI 0–3.8%），按节点取 p95 只会引入噪声。
- 全局 p95（2026-10-26 快照，n=864）：**13,060 ms**（p50 3,204 / p99 31,176）。
- `firstByteDeadline = max(15s, p95 × 1.5)` = **19.59s**。
- **它实际卡的是什么（评审修正）**：本上游 `hdr ≈ ttft`（差 p50 1ms / p90 244ms），所以 `ResponseHeaderTimeout` 等价于**首 token 上限**——它截断的是**慢 prefill / 慢首 token**，不是「响应头永远不来」。
  - 影响面：成功请求里 ttft > 19.59s 占 **2.20%**（10-26，n=864；全窗口 4.48%；评审 10-19 快照 2.19%）。被截断的请求不是直接失败，而是**重试/换节点**（见 F2：不再判节点死刑）。
  - **不杀长生成**：首字节之后 body 仍由 `idle_read.go`（120s 无字节才断）兜底；长输出只要还有字节就不杀。
- 实现位置：三处 Transport 的 `ResponseHeaderTimeout` 统一取 `firstByteTimeout`（直连 `httpClient`、节点池 `newNodeTransport`、Bun 出口 `bunClientFor`）。

---

## 3. 最差等待：171s → X s

| 口径 | 改动前 | 改动后 |
|---|---|---|
| 单次失败/单次尝试最差（04-prior-art 生产记录：135s 建连 + 首字节） | **171s** | **≤27.6s**（8s 建连 + 19.59s 首字节） |
| 整请求回放（本仓库 10-26 快照：859 条含 connect_error 的失败，按每次尝试 min(实测, 27.59s) + 120s 预算重放） | p50 49.4s / p95 96.0s / **max 120.0s** | p50 48.3s / **p95 60.4s** / **max 87.4s** |
| 整请求理论上界（预算只在 attempt 之间判，语义不变） | 120s 预算 + 末次最长尝试（旧 60+60s）≈ 240s | 120s + 27.6s ≈ **147.6s** |
| 回放中 >60s 的失败请求 | 302 / 859 | **73 / 859** |

要点：

- 171s 的主体是**单个建连**（SYN 重传耗尽 127s+），现在单次尝试上限 27.6s，砍掉一个数量级；**主要功臣是 8s 建连，不是 19.59s 首字节**。
- 总预算语义**没有改**（`upstreamLoopBudget = 120s`，只在 attempt 之间判），整请求最差仍可能到「120s + 末次尝试」；回放实测 120.0s → 87.4s，>60s 失败占比 35.2% → 8.5%。
- 回放脚本与快照为临时分析产物，未入库（评审 §6 要求可复现）；如需入库可另行补。

---

## 4. 改动清单

| 文件 | 改动 |
|---|---|
| `proxy_node.go` | `legacyDialTimeout=60s`（仅历史值）、`connectTimeout=8s`、`dialTimeout=connectTimeout`；`globalTTFTP95=13,060ms`、`firstByteDeadline()`、`firstByteTimeout`；`newNodeTransport` 的 `ResponseHeaderTimeout` → `firstByteTimeout` |
| `bun_egress.go` | `ResponseHeaderTimeout` → `firstByteTimeout`；给注入的 DialContext 显式套 `dialTimeout` ctx 上限（`bun_transport.go` 的 dial 直接调 `dialContext`，`ConnectTimeout` 字段实际不生效） |
| `main.go` | 直连：抽出 `directDialer{Timeout: connectTimeout}`、`ResponseHeaderTimeout` → `firstByteTimeout`；新增 `isFirstByteTimeout()` / `noteAttemptTransportError()`（F2）；两个重试循环的 `connect_error` 分支改为「首字节超时不 mark、事件记 `ttft_timeout`」；N2：告警样本喂入对首字节超时按「可达」计；halt 守卫：halt 期间建连失败不 mark |
| `idle_read.go` | 仅补注释：T3 明确不改 idle read 语义（120s 无字节），不改任何逻辑 |
| `timeout_tier_test.go` | 新增 13 个测试（**仅本地保留，未 `git add`**，`.gitignore:26`） |
| `docs/research/scoring2/08-task3-report.md` | 本报告 |

未动：`node_scoring.go`、`static/*`、`egress.go`、`bun_transport.go`、`calllog.go`；未 commit / 未 push / 未部署。

---

## 5. 测试与变异

### 5.1 测试（`timeout_tier_test.go`，13 条全绿）

| 测试 | 断言 |
|---|---|
| `TestConnectTimeoutDecision` | `connectTimeout==8s`、`dialTimeout==connectTimeout`、旧值保留 60s |
| `TestFirstByteDeadlineUsesGlobalP95` | 2s→15s、10s→15s、20s→30s、13.06s→19.59s、0→15s |
| `TestGlobalTTFTP95ConcreteValue` | `globalTTFTP95==13060ms`、`firstByteTimeout==19590ms`（具体值，非自指） |
| `TestFirstByteTimeoutWiring` | 直连/节点池/Bun 三处 `ResponseHeaderTimeout == firstByteTimeout`，且不是旧 60s |
| `TestDirectDialTimeoutWired` | `directDialer.Timeout == connectTimeout == 8s`；直连 `ResponseHeaderTimeout` 接线正确 |
| `TestNodePoolDialCappedByConnectTimeout` | 节点池注入 2s 慢 dial + 150ms 上限 ⇒ ≤1.5s 报错（**F1 主路径行为测试**） |
| `TestBunDialCappedByConnectTimeout` | Bun 注入 2s 慢 dial + 150ms ⇒ 150ms 级报错 |
| `TestNodeTransportFirstByteDeadline` | 首字节 300ms：deadline 100ms 失败 / 2s 成功 |
| `TestSlowButSuccessfulNotKilled` | p95=20s ⇒ deadline 30s > 12s；scaled 120ms/800ms 成功 |
| `TestFirstByteTimeoutDoesNotMarkNodeDead` | 原生/Bun 两种「timeout awaiting response headers」⇒ 节点仍 available、仍可被 `pick`；返回 firstByte=true（进入重试/换节点） |
| `TestDialTimeoutStillMarksNodeDead` | `dial tcp … i/o timeout` ⇒ mark dead（建连失败语义不变） |
| `TestHaltSuppressesMarkOnConnectError` | 池级 halt 生效时建连失败 ⇒ 节点状态不变（不 mark） |
| `TestIdleReadTimeoutUnchanged` | `idleReadTimeout==120s`、`upstreamLoopBudget==120s`；有字节持续到达不被杀、停住 40ms 才报 idle timeout |

### 5.2 变异自检（10 个，全部变红）

| # | 变异 | 变红的测试 | 证据 |
|---|---|---|---|
| M1 | `connectTimeout` 8s → 5s | `TestConnectTimeoutDecision` | FAIL |
| M2 | 去掉 `max(15s, …)` 下限 | `TestFirstByteDeadlineUsesGlobalP95` / `TestSlowButSuccessfulNotKilled` | FAIL |
| M3 | 节点池 `ResponseHeaderTimeout` 改回 60s | `TestNodeTransportFirstByteDeadline` / `TestFirstByteTimeoutWiring` | FAIL |
| M4 | Bun 去掉 `dialCap` | `TestBunDialCappedByConnectTimeout` | FAIL：2.001s 未被截断 |
| M5 | `idleReadTimeout` 120s → 5s | `TestIdleReadTimeoutUnchanged` | FAIL |
| **M6** | **节点池 DialContext 删掉 `context.WithTimeout(ctx, dialTimeout)`** | `TestNodePoolDialCappedByConnectTimeout` | FAIL 2.011s |
| **M7** | **首字节超时也 `mark(NodeDead)`** | `TestFirstByteTimeoutDoesNotMarkNodeDead` | FAIL |
| **M8** | **直连 `directDialer.Timeout` 8s → 60s** | `TestDirectDialTimeoutWired` | FAIL |
| **M9** | **`globalTTFTP95` 13060ms → 11027ms** | `TestGlobalTTFTP95ConcreteValue` | FAIL |
| **M10** | **`noteAttemptTransportError` 去掉 `!pool.poolHalted(...)`** | `TestHaltSuppressesMarkOnConnectError` | FAIL：halt 期间仍 mark 成 dead |

### 5.3 验收命令（修复后实跑）

```text
$ go test -skip 'TestIP4RV' -count=1 ./...
ok  	github.com/shiyanYX/opencode2api	141.332s

$ go vet ./...
(无输出，rc=0)

$ gofmt -l proxy_node.go bun_egress.go idle_read.go main.go timeout_tier_test.go
(空)
```

> `TestIP4RV*` 为既有基线失败，按 Lead 要求跳过；本任务未触碰相关代码。

---

## 6. 风险 / 未覆盖

1. **纯建连分布不可观测**：8s 的真实误杀率只能给区间 [0, 8.95%~18.69%]，**0.5% 门槛并未证明满足**；依赖失败侧收益与「dead 1 分钟冷却可自愈」做不对称判断。待 T5 `dial_ms` 复核，不达标就回调。
2. **首字节 deadline 截断的是慢 prefill/慢首 token**（`hdr≈ttft`），影响面 2.2%（10-26）/ 2.19%（10-19）；被截断请求会重试。若上线后重试率明显上升，优先观察而不是继续抬阈值。
3. **F2 的边界**：识别依赖错误串 `timeout awaiting response headers`（Go 原生与 Bun 两路都已覆盖）；若未来换 Transport、错误串变化，会退回「判死」旧行为——测试会把这种变化变红（Bun 分支单独断言）。
4. 首字节超时现在对**告警样本与节点状态**都按「出口可达」处理（N2）：不再记 `runFailures`、不 mark。残留只在错误串依赖：若未来换 Transport 导致错误文案变化，会同时退回「判死 + 记失败」旧行为——`TestFirstByteTimeoutDoesNotMarkNodeDead` 会变红。
5. **`egress.buildProxyClient` 仍是 60s**（`egress.go` 不在本任务 write scope）：若生产走独立 SOCKS 代理出口，该路径无 T3 上限；只覆盖直连/节点池/Bun 三条在用 Transport。
6. `globalTTFTP95` 是静态快照值（13.06s），刻意不做定时重评；模型结构变化后需人工复核。
7. 回放与 `/api/nodes` 交叉校验为临时分析，未入库；评审要求的可复现脚本可另行补交。

---

## 7. 交付边界

- 全部改动留在工作区，**未 commit / 未 push / 未部署**；`timeout_tier_test.go` 未 `git add`（符合仅本地保留公约）。
- 待 Lead 复验（`通过 / 有条件通过 / 不通过` 由你判定）。
