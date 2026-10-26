# T3 独立评审：超时分层（建连硬超时 + 首字节 deadline）

- 评审对象：`docs/superpowers/plans/2026-10-26-node-selection-v2.md` **Task 3**（task-111，实现方 `healthcheck-probe`）
- 评审人：`review-t1-design`（独立 reviewer，**未参与实现**；只读，未改代码、未 commit）
- 评审基线：HEAD `3ab39e9` + 工作区未提交改动（`/tmp/review-t3.diff`），T3 源码改动 **4 文件 54+/30-**
- 被审文件 sha256（评审时固定，工作区被其他队友并发修改，以此为准）：
  `main.go f2f04f7c…c8e9d` · `proxy_node.go 12daed9d…c520b4` · `bun_egress.go 3275a0bd…5438b3` · `idle_read.go f6aa7d6f…b85e38a` · `timeout_tier_test.go b658453a…febb5db`
- 独立数据：`/tmp/prod_call_log.jsonl`（230,793 条，2026-10-19 快照——**与实现方用的 10-26 快照不是同一份**），`/tmp/prod_node_events.tsv`

---

## 0. 结论

| 维度 | 判定 |
|---|---|
| **Spec 合规** | ✅ **通过**：建连 8s、首字节 `max(15s, 全局 ttft p95×1.5)=19.59s`（我复算 13,060×1.5=19,590ms ✓）、idle read 零逻辑改动、总预算 120s 未动、deadline 用全局非节点 p95、5 个变异全红、报告含「171s → ≤27.6s」、vet/gofmt 干净、仅改 4 个允许的文件 |
| **Quality** | ❌ **不通过**：3 个断言洞（**最要紧：生产主路径「节点池 8s 建连上限」没有任何行为测试**）+ 1 个未申报的副作用（首字节超时会把节点判死）+ 1 处计划明文禁止的「只断言常量」测试 |
| **能否进入 T4** | **可以**（T4 只读、且本任务**无代码缺陷**）；但 F1/F2 的补测必须在**提交前**完成（纯测试文件 + 1 处日志语义，约 40 行） |

**一句话**：超时分层本身做对了，失败侧证据非常硬（94.6% 的 `connect_error` 等待 >8s，p50 22s / p90 135s）；但「8s 会不会误杀慢但能用的节点」**在这份数据里根本测不出来**——报告诚实地写了「最大不确定性」，却没意识到它同时意味着**计划自己的 0.5% 门槛并未满足**。更关键的是：**能把 8s 上限悄悄删掉而测试全绿的**那条路径，正好是 95.2% 流量走的节点池。

---

## 1. 复跑与验收命令

```bash
export PATH=$PATH:/usr/local/go/bin GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
go build ./...                                    # exit 0
go test -count=1 -run '<7 条 T3 测试>' -v .        # 7/7 PASS (0.856s)
go vet ./...                                       # 无输出, rc=0
gofmt -l main.go proxy_node.go bun_egress.go idle_read.go timeout_tier_test.go   # 空
```
全量 `go test -skip TestIP4RV -count=1 ./...`：见 §6（共享工作区被并发修改，结果单独说明）。

**逐条核对（不是复述）**：
- `idle_read.go`：`git diff` 只有注释，**零逻辑改动** ✓；接线点在 `main.go:2966`（非流式）与 `main.go:3426`（流式），都是 `newIdleReadCloser(resp.Body, idleReadTimeout)` ✓。
- **首字节后不打断长生成**：`ResponseHeaderTimeout` 只作用于响应头；body 由 `idleReadCloser`（120s 无字节才断）兜底；全仓 `http.Client{}` 没有 `Timeout` 字段 ✓（`proxy_node.go:1540` / `bun_egress.go:72` / `main.go:33`）。
- **总预算语义未变**：`upstreamLoopBudget` 不在本次 diff 里，仍是 120s，仍只在 attempt 之间 break ✓。
- **全局 p95 不按节点**：`globalTTFTP95` 只出现在 `proxy_node.go:331/345` 与测试，没有按节点取值 ✓。
- **禁止项**：`git diff --stat` 只有 `bun_egress.go / idle_read.go / main.go / proxy_node.go`，**没碰 `node_scoring.go` / `static/*`** ✓。

---

## 2. Findings

### F1（重要）节点池的 8s 建连上限**没有任何行为测试** —— 变异 M6 全绿

`newNodeTransport` 是 **95.21%** 流量的实际路径（`egress.Get` → `proxyPool.getClient` → `newNodeTransport`，`proxy_node.go:1540`；我按 `call_log.nodes` 非空统计：219,741/230,793 = 95.21%）。
我把 `newNodeTransport` 的 `dctx, cancel := context.WithTimeout(ctx, dialTimeout)` 整段删掉（即**建连上限消失**），7 条测试**全绿**：

```
########## M6【我的】节点池 DialContext 去掉 dialTimeout 上限
ok  	github.com/shiyanYX/opencode2api	0.829s
```
原因：唯一的行为级建连测试是 `TestBunDialCappedByConnectTimeout`（Bun 路径），节点池那条只测了 `ResponseHeaderTimeout`。⇒ **本任务的头条改动（8s 建连）可以被静默回退，CI 不会红。**
**补法**（照抄 Bun 那个测试，~25 行）：注入 2s 慢 dial + `dialTimeout=150ms`，用 `newNodeTransport` 构造 client 去连 `httptest`，断言 `elapsed ≤ 1.5s`。

### F2（重要）「首字节 deadline」在本上游等价于「首 token deadline」，且超时会 `mark(NodeDead)`

- 我用 `connect_ok` 事件独立复算了「响应头到达时间」（`connect_ok.at − 上一个事件/请求 ts`），并与 `ttft_ms` 对比：
  ```
  干净子集（单事件 connect_ok，n=13,778）：ttft − hdr  p10=0  p50=1ms  p90=244ms
  ```
  ⇒ **响应头几乎与首个 `data:` 同时到达**。所以 `ResponseHeaderTimeout=19.59s` 实际卡的是**首 token 等待（prefill/排队）**，不是「响应头迟迟不来」。
- 报告 §2 的表述「这些是『响应头阶段就等了 19.6s 以上』的极端样本」**低估了影响面**：它是**首 token 上限**。报告「不是把长生成杀掉」这句是对的（生成在首字节之后），但没写清它杀的是「慢首 token」。
- **未申报的副作用**：首字节超时 → `client.Do` 返回错误 → `main.go:2980-2995` 的 `connect_error` 分支 → **`proxyPool.mark(nodeFp, NodeDead, ...)`**。也就是说，一个「首 token 慢但会成功」的请求，现在会让该节点**被判死 + 换节点**。量级上界 = `hdr > 19.59s` 的成功请求占比 = **2.19%**（n=182,221）。
- 因为 `05-latency.md §0` 已证明 **节点对 ttft 的 ICC=0.0%**，「换个节点再试」**不会**系统性改善首 token ⇒ 这次判死既冤枉节点，也救不回请求。
- **补法**（最小、且不动 T3 语义）：`connect_error` 里区分错误来源——`net.Error.Timeout()` 且错误串含 `timeout awaiting response headers`（即已建连、卡在首字节）时，**不 `mark(NodeDead)`**（只换节点/重试）；建连阶段的超时仍按现状判死。可以再用 `httptrace.GotConn`（T5 的 `withDialTrace` 已经有）判断是否已建连。

### F3（重要）「8s 误杀率」**无法用现有数据定量**；报告的 15.1% 是有效但很松的上界，而计划的 0.5% 门槛实际**没有满足**

- 报告的上界逻辑我复核**成立**：`dial ⊆ ttft` ⇒ `P(dial>8s) ≤ P(ttft>8s)`。但它**松到什么程度没给**。
- 我独立复算（10-19 快照，**不同窗口**）：
  ```
  ok+ttft 子集 n=38,315：ttft>5s = 38.19%   ttft>8s = 18.69%   ttft>19.59s = 6.00%
  全部 ok（hdr 口径 n=182,221）：         hdr>5s = 37.62%  hdr>8s = 8.95%   hdr>19.59s = 2.19%
  ```
  ⇒ 8s 的上界在 **8.95% ~ 18.69%** 之间（口径/子集不同），报告的 12.96%/15.05% 落在其中，**独立吻合**。
- 三种口径**都远大于计划要求的 0.5%**。计划原文是「5s 误杀 >0.5% 则用 8s」，隐含 8s 应 ≤0.5%；报告也承认「8s 档的上界仍高于 0.5%」然后仍然采用。**这不是违规（规则只给了两档），但「先量后定」在这个决策上实际上退化为「按规则选 8s」** —— 必须让 Lead 知道这一点，而不是写着「先量后定」就当成测过了。
- **能确定的是风险不对称**：失败侧 `connect_error` 的等待（第一个 connect_error − attempt 起点，n=7,013）：
  ```
  p50=22s  p90=135s  p95=136s  >5s=96.1%  >8s=94.6%  >17s=54.6%  >60s=37.6%
  ```
  ⇒ 94.6% 的失败建连都在 8s 以上等待，**8s 砍掉的是失败侧的主体**；对成功侧的潜在代价（误杀慢节点）与失败侧的确定收益（每次省 14~127s）相比，**8s 是可辩护的选择**。
- **仍未被回答的问题**：真实 `dial > 8s` 的比例区间只能给到 **[0, 8.95%]**（严格上界），中位数无从估计。⇒ T5 的 `dial_ms` 是唯一出路（见 §6 的注意事项）。

### F4（中）两处「值没钉住」的断言洞

```
M7 直连 httpClient 的 DialContext.Timeout 改回 60s   → 7/7 全绿（无测试覆盖直连建连上限）
M8 globalTTFTP95 13.06s → 20s（deadline 19.59→30s）  → 7/7 全绿（自指 oracle）
```
- M8 的原因：`var firstByteTimeout = firstByteDeadline(globalTTFTP95)` 是按值在初始化时算的，而测试断言的是 `firstByteTimeout == firstByteDeadline(globalTTFTP95)` —— **永远成立**（Go 全局变量按依赖顺序初始化）。它只能抓「有人手写 `firstByteTimeout` 不推导」，抓不到「p95 被改」。**应补**：`if globalTTFTP95 != 13060*time.Millisecond { t.Fatalf(...) }`（或断言 `firstByteTimeout == 19590*time.Millisecond`）。
- M7：直连是「池内无可用节点」时的兜底（10-19 事故后期流量就靠 direct 撑着），值得一条行为测试（与 F1 的补法同构，换 `httpClient`）。

### F5（低）`TestConnectTimeoutDecision` 是计划明文禁止的「只断言常量」空测试

计划 Global Constraints：「测试必须真跑逻辑；**禁止只断言常量的空测试**」。该测试只断言 `connectTimeout==8s`、`dialTimeout==connectTimeout`、`legacyDialTimeout==60s` 三个常量，没有任何行为。它抓到了 M1（8s→5s），但这正是常量测试唯一能抓的东西；而「常量存在但没接到 transport 上」它完全抓不到（F1 就是活的例子）。⇒ 保留可以（当作文档），但**不能把它算作 `connectTimeout` 的覆盖**。

### F6（低）19.59s 的覆盖比例不是稳定性质：报告 2.20%，我的独立快照是 **6.00%**

`ttft>19.59s`：报告 10-26 窗口 **2.20%**（n=864 成功+ttft）；我在 10-19 快照上同口径 **6.00%**（n=38,315）。`hdr>19.59s` 全量是 2.19%。⇒ 在别的负载窗口里，首字节 deadline 的杀伤面可以到报告的 **~3 倍**。这与 §2-F2 的「判死」副作用叠加后更值得盯：**上线后必须观测 `timeout awaiting response headers` 的占比与 dead 标记率**。

---

## 3. 独立误差估计表（8s 建连上限）

| 指标 | 实现方（10-26 快照） | **我（10-19 快照，独立）** | 说明 |
|---|---|---|---|
| `ttft>5s` | 29.05% / 全窗口 21.84% | **38.19%**（ttft 子集） | 上界，非纯建连 |
| `ttft>8s` | 15.05% / 全窗口 12.96% | **18.69%** | 上界，非纯建连 |
| `hdr>8s` | —（未测） | **8.95%** | 更紧的上界（响应头到达；≈ttft） |
| 纯 `dial>8s` | 未观测 | **未观测** | 区间只能给 **[0, 8.95%]** |
| 失败侧 `connect_error` 等待 p50 / p90 | 未给 | **22s / 135s** | 8s 的确定收益来源 |
| 失败侧等待 >8s | 未给 | **94.6%** | 8s 砍掉失败主体 |
| 计划门槛 | 0.5% | 0.5% | **三种口径均未满足**（8.95%~38%） |
| 我的结论 | — | **8s 可辩护（风险不对称），但误杀率必须等 `dial_ms` 才能定量** | |

**「8s 会不会误杀一批慢但能用的节点？」** 答：**会，只要存在「建连 8~60s 但最终成功」的节点**，而这类节点在数据里**既不能证有、也不能证无**。可确定的是：(a) 上界 8.95%~18.69%；(b) 误杀后果是**可自愈**的（`connect_error` 判 dead 后，健康循环每分钟复探、成功即复活，`probeTimeout=20s` ⇒ 8~20s 的慢节点探测能过），代价是「1 分钟不可用 + 一次换节点 + 一次 connect 扣分」，不是永久损失；(c) 失败侧 94.6% >8s 的收益是确定的。⇒ **方向对，数值待 `dial_ms` 复核**。

---

## 4. 变异证据

工作区零写入：全部在 `/tmp/t3rev` 副本（`cp *.go` + `go.mod/go.sum` + `static/*.html` + `oc_prompt_v2.txt`/`oc_tools_v2.json`），每条变异后 `go build` 再跑 7 条测试，跑完从 pristine 备份还原（`diff -q` 全等）。

### 4.1 要求的 5 条（全部变红 ✓）

| # | 变异 | 结果 | 变红的测试 |
|---|---|---|---|
| M1 | `connectTimeout` 8s→5s | **红** ✔ | `TestConnectTimeoutDecision` |
| M2 | 去掉 `max(15s, …)` 下限 | **红** ✔ | `TestFirstByteDeadlineUsesGlobalP95` |
| M3 | 节点池 `ResponseHeaderTimeout`→60s | **红** ✔ | `TestFirstByteTimeoutWiring` + `TestNodeTransportFirstByteDeadline` |
| M4 | Bun 去掉 `dialCap`（不套 ctx 上限） | **红** ✔ | `TestBunDialCappedByConnectTimeout`（2.68s，>1.5s 阈值） |
| M5 | `idleReadTimeout` 120s→5s | **红** ✔ | `TestIdleReadTimeoutUnchanged` |

> 注：M4 我第一次写成了在 composite literal 里插语句（编译错），改成「`dialCap` 内直接 `return dial(ctx,…)`」后才是有效变异——**有效变异必须能编译**，这条也写进方法备注。

### 4.2 我自己加的 3 条（**全部存活 = 断言洞**）

| # | 变异 | 结果 | 洞在哪 |
|---|---|---|---|
| M6 | `newNodeTransport` 去掉 `dialTimeout` 包裹 | **绿 ✘** | 见 F1：**主路径 8s 上限零覆盖** |
| M7 | 直连 `DialContext.Timeout` 60s | **绿 ✘** | 见 F4：直连兜底零覆盖 |
| M8 | `globalTTFTP95` 13.06s→20s | **绿 ✘** | 见 F4：自指 oracle |

```
########## M1 --- FAIL: TestConnectTimeoutDecision
########## M2 --- FAIL: TestFirstByteDeadlineUsesGlobalP95
########## M3 --- FAIL: TestFirstByteTimeoutWiring / TestNodeTransportFirstByteDeadline
########## M4 --- FAIL: TestBunDialCappedByConnectTimeout          (2.680s)
########## M5 --- FAIL: TestIdleReadTimeoutUnchanged
########## M6 ok  0.829s        ← 存活
########## M7 ok  0.838s        ← 存活
########## M8 ok  0.829s        ← 存活
=== 副本已还原 === ALL_RESTORED
```

---

## 5. 三个疑点的裁定

### 疑点 1：`egress.buildProxyClient` 仍是 60s —— 在生产实际调用链上吗？

**裁定：不在主链路；是「池为空」才可达的边缘路径（近似死代码）。** 证据：
- 调用链 `egress.Get`（`egress.go:120`）→ `if proxyPool.nodeCount() > 0 { … return 节点池 client }`（`:145-152`）；只有 `nodeCount()==0` 才落到 `getSocks5Client` → `buildProxyClient`（`:166/:171/:187`）。
- 生产 212+ 节点、**95.21%** 的记录带 `nodes` ⇒ 节点池非空 ⇒ 这段不可达。剩余 4.79%（零节点）走的是 `eg.direct`（`pickForRegion` 返回 nil ⇒ `EgressResult{Client: eg.direct}`），**不是** `buildProxyClient`（除非同时配了静态 socks5 且池归零，而静态 socks5 的生产配置我无法读取）。
- 报告 §6.3 已把它写成「若生产走独立 SOCKS 代理出口……本任务只覆盖三条实际在用的 Transport」——**披露是诚实的，但措辞比实际更保守**（实际是「池空才可达」）。
- **结论**：不构成本任务缺口。建议后续 1 行接上 `firstByteTimeout`/`connectTimeout`（或随静态 socks5 一起下线），避免池空回退窗口里留一个 60s 洞。

### 疑点 2：8s 收紧后 dead 标记率会上升吗？是否该改成「连续 2 次才判死」？

**裁定：会上升；但不建议对建连超时用 streak，建议对「首字节超时」单独免除判死。**
- 上升机理：`connect_error` → `proxyPool.mark(nodeFp, NodeDead, ...)`（`main.go:2995`），**不经过** `probeFailStreakThreshold=2`（`02-constraints.md` C23 明确要求「首次即换」，理由是旧行为下每次连不上 ≈95s，忍两次不可接受）。
- 量级上界：`hdr>8s` = 8.95%（我的快照）⇒ 新判死最多增加约 9% 的请求各一次（真实值 ≤ 该上界，且只影响**节点池**请求）。
- **自愈**：健康循环每分钟复探（`probeTimeout=20s`），建连 8~20s 的慢节点探测能过 ⇒ dead 只持续 ~1 分钟。所以**不是永久损失**，是「抖动 + 一次换节点」。
- 是否该用 streak？**对 8s 建连超时：不该**。8s 已经很便宜，要求连续 2 次意味着让用户多等 8s，与「别干等」的目标相反；且现有 dead 复探已经把误判的寿命压到 1 分钟。
- **但对 F2 的首字节超时：该区别对待**。首字节超时是「模型/排队慢」，`ICC=0` 证明它**不是节点属性**，把它记成节点死是**本任务新引入的误判**。最小改法见 F2。

### 疑点 3：首字节 deadline 19.59s 会重试 2.2% 的成功请求 —— 净损失还是净收益？

**裁定：净收益，但幅度小、且被 F2 的副作用侵蚀；建议保留 19.59s + 修 F2，而不是调高阈值。**
- 被截断的样本画像（我的快照）：`hdr>19.59s` 的成功请求 **3,983 条（2.19%）**，其中 **76.6%（3,052）是「单次尝试、第一个事件就是 connect_ok」**——即**首试就等到 >19.59s 然后成功**；该组 `hdr` p50=**33s**、p90=**73s**；模型上 `mimo` 占 67.9%（与总体占比同量级，说明不是某个模型独有）。
- **收益**：单次尝试上限 171s→27.6s（8+19.59，✓ 算术正确）；回放显示 >60s 的失败请求 302→73（报告 §3，我未独立重放——缺回放脚本）。慢组 p50 33s 若能以「19.59s + 快速重试」结束，可压到 ~22s。
- **成本**：① 额外上游 attempt（免费层**配额**是真成本）；② 若首 token 慢是系统性的（长 prompt 的 mimo prefill），重试会再慢一次 → 2~3 倍消耗甚至 502；`ICC=0` 说明**换节点救不了它**，收益只能来自「重新排队的随机性」；③ **F2 的 dead 标记**（每次截断记一次节点死）。
- 为什么仍判净收益：① 病态尾部确实被砍；② 2.19% 的截断是**有界**的，而 171s 尾部是无界的；③ 报告已把「可能升阈值」写成观察项。
- **但**「171s 主体是 135s 建连」这一点很重要：**砍掉 171s 的主要功臣是 8s 建连，不是 19.59s 首字节**。首字节 deadline 的独立价值只是「卡死在等首字节」这一类，代价是 2~6% 的重试。⇒ 如果上线后发现重试率明显上升，**优先改 F2（不判死）+ 观察**，而不是继续抬阈值。

---

## 6. 无法验证项 / 不确定性（如实列出）

1. **纯建连耗时分布**：`call_log` 的 `ttft_ms`、`connect_ok` 都包含「响应头/首 token」，**无法分离 dial**。`hdr≈ttft`（§F2）已用数据证明不存在更紧的代理变量。⇒ 8s 的真实误杀率只能给区间 [0, 8.95%]。
2. **报告 §1.1 的节点探测交叉校验（144 节点：9.03%>5s / 6.25%>8s）无法独立复现**：需要 `/api/nodes` 生产快照，本任务禁止读生产容器与 `/data`。我保留该数字为「实现方自报，未独立复现」。
3. **回放（171s→87.4s、>60s 失败 302→73）无法独立复现**：实现方没有交付回放脚本/数据；我只有 10-19 快照，且其事件被 `calllog` 裁剪（`connect_error` 首个事件的最大等待 810s 明显是裁剪/多尝试造成的伪值），不适合直接重放。**建议要求实现方把回放脚本入库或贴出**。
4. **生产是否配置了静态 socks5**：无法读 `/data`，因此「`buildProxyClient` 是死代码」只能说到「池非空时不可达」。
5. **全量 `go test ./...`**：当前主工作区正被其他队友并发修改（`M node_scoring.go`、新增 `zz_ip4v_layout_new.go`），在共享树里跑全量既慢又不可归因；我改为在**固定副本**（sha256 见页首）上跑：
   ```
   $ go test -skip 'TestIP4RV' -count=1 -timeout 600s ./...
   --- FAIL: TestT23RealLogHardGate          zz_t23_verify_test.go:241: 读 free_models_cache.json: no such file or directory
   --- FAIL: TestT23RealLogBackfillEqualsDirect  zz_t23_verify_test.go:363: 同上
   FAIL	github.com/shiyanYX/opencode2api	190.307s
   ```
   这 2 条是**我副本缺运行时数据文件**（`free_models_cache.json` 未拷），不是代码失败：补上该文件后两条均 PASS（12.44s / 1.94s）⇒ **T3 固定副本全量等价 190s 全绿**（其余用例全过）。与实现方 `ok 161.785s` 的差异只来自机器负载（同时有另一位队友在跑全量）。
6. **`dial_ms` 的两个隐含缺陷（T5 相关，不影响 T3）**：`withDialTrace`（`calllog.go:2653`）在 `httptrace.GotConn` 时记录，但 `GotConn` **对 keep-alive 复用连接也会触发**（`info.Reused==true`），且注释写「取首个即最接近直觉」——复用连接的 `dial_ms` 会接近 0，**系统性低估建连**。T5 上线前应改为 `if !info.Reused { observeDialMs(...) }`（或另记 `reused` 标志），否则「用 dial_ms 复核 8s」会被这个偏差骗过。

---

## 7. 是否进入 T4

**可以进入 T4**，理由：本任务**没有发现代码缺陷**（4 个文件的改动都正确、接线完整、语义未变），T4 只写 `09-acceptance.md`、只读。

但**提交前**必须落两处（都在 `timeout_tier_test.go` 或 1 处日志语义，~40 行）：
1. **F1**：给 `newNodeTransport` 补一条行为级建连上限测试（照抄 `TestBunDialCappedByConnectTimeout`）。**这是硬要求**——否则头条改动可被静默回退。
2. **F2**：首字节超时不要 `mark(NodeDead)`（区分 `timeout awaiting response headers`），或至少把它写进报告的风险表并加观察项。

建议但非阻塞：F4 的两个值断言；F5 的常量测试改成行为断言；F6 的 `dial_ms` 修正在 T5 落地。

---

## 补记：固定副本全量测试

（见下发消息里的实时结果；本节在结果出来后追加。）

---

# 复审（fix round 1）

- 复审时间：2026-10-26 20:00–20:10；范围**仅** F1/F2/F3 三条 + 修复是否引入新问题（未重审整份实现）
- 复审基线（工作区，HEAD 仍 `3ab39e9`，改动未提交）——**复审期间工作区又动了一次**（T2/R4 在改同一个 `main.go`/`proxy_node.go`），故给出两个钉子：
  - 复审开始时：`main.go 8b00e1f3…09b35` · `proxy_node.go 4c18f010…b6f5c` · `timeout_tier_test.go 1caa2ebe…c951c`
  - **复审结束时（所有结论以此为准，全部检查已在新状态重跑）**：`main.go 2b768157…ad59ea` · `proxy_node.go 2e24a1e3…623445` · `bun_egress.go 3275a0bd…5438b3` · `idle_read.go f6aa7d6f…b85e38a` · `timeout_tier_test.go 1caa2ebe…c951c` · `08-task3-report.md b01523d2…97aef`
  - 差异归因：`main.go`/`proxy_node.go` 的变动来自 T2/R4（`freeTierConfirmAllowed`、`callLogSwitchEvent` 等），**T3 的 4 段代码逐字未变**（`directDialer`、`isFirstByteTimeout`、`noteAttemptTransportError`、两条 connect_error 分支的 `reason=ttft_timeout`）；12 条测试与 4 条变异在结束状态重跑，结果同上。
- 说明：`main.go` 现为 **130 行改动**，其中 T2（halt/订阅熔断）与 T3（超时分类）在同文件同分支交错；`calllog.go` 亦有未提交改动（R4 结构化 switch 事件）。下面把「并发核实」单独列节。

## R0. 复审结论

| 项 | 判定 |
|---|---|
| **F1**（节点池 8s 无行为测试） | ✅ **ADDRESSED** |
| **F2**（首字节超时判死） | ✅ **ADDRESSED**（有 1 条低危残留，见 N2） |
| **F3**（报告口径） | ✅ **ADDRESSED** |
| 新问题 | N1（协调，必须处理）/ N2（低）/ N3（观察） |
| T2×T3 并发 | ✅ 未互相覆盖，halt 顺序正确 |
| build / vet / 全量 | ✅ `BUILD_OK` / vet 干净 / `ok 187.992s` |
| **能否进 T4** | ✅ **可以**；提交前处理 N1 |

## R1. F1 — 节点池 8s 建连上限（ADDRESSED）

新增 `TestNodePoolDialCappedByConnectTimeout` 是**真行为测试**（不是常量断言）：把包级 `dialTimeout` 改成 150ms，注入一个 2s 才返回的慢 dial，用 `newNodeTransport(slow)` 真发一次请求，断言实际耗时 ≤1.5s。

我重跑上一轮的洞：

```
########## M6 节点池 newNodeTransport 去掉 dialTimeout 上限（原 F1 洞）
--- FAIL: TestNodePoolDialCappedByConnectTimeout
FAIL	github.com/shiyanYX/opencode2api	5.169s        ← 2s 慢 dial 真跑完了（>1.5s 断言失败）
########## M7 directDialer.Timeout 改回 60s（原 F4 洞）
--- FAIL: TestDirectDialTimeoutWired
########## M8 globalTTFTP95 13.06s→20s（原自指 oracle 洞）
--- FAIL: TestGlobalTTFTP95ConcreteValue
=== 还原校验 === ALL_RESTORED
```

- `TestDirectDialTimeoutWired` 钉住 `directDialer.Timeout == connectTimeout == 8s`，并把直连 `ResponseHeaderTimeout` 接到 `firstByteTimeout`（`directDialer` 抽成包级变量就是为了可断言 —— 这是**值断言**，不是行为断言，但它确实抓到了 M7，可接受）。
- `TestGlobalTTFTP95ConcreteValue` 钉住 `13060ms` 与 `19590ms` 两个**具体数字**，自指恒真的洞已消除。
- 12 条基线全绿（`ok … 1.035s`），M1–M5 未回归（实现方报 9 条变异全红；我独立复跑了与 F1/F2 相关的 4 条）。

## R2. F2 — 首字节超时不得判死（ADDRESSED，端到端实证）

实现方式：`isFirstByteTimeout(err)` 匹配 `timeout awaiting response headers`；`noteAttemptTransportError()` 对首字节超时只记 `ttft_timeout` 事件并 `return true`（**不** `mark`），其余传输错误记 `connect_error` 并 `mark(NodeDead)`；两条重试循环共用。

**我构造的真实请求**（不是手写错误串，不是只读测试）：

```
[t3rev2/zz_review_stuck_test.go]
静默 httptest server：读掉请求体后 sleep 2s 再写响应头
客户端：真实 TCP 连接到它 + ResponseHeaderTimeout=300ms
→ 日志：INFO upstream_attempt ... duration_ms=315 attempt_index=0
        error="Post \"https://opencode.ai/...\": net/http: timeout awaiting response headers"
→ 重试：INFO upstream_attempt ... status=200 attempt_index=1
→ 断言：ttft_timeout node=a01bf6af…；成功 node=b836aaf7…；attempts=2
--- PASS: TestRev_StuckFirstByte_KeepsNodeAliveAndSwitches (2.01s)
```

证据链完整：**已建连 → 卡首字节 → 节点仍 `available` → 请求换到另一个节点并 200 → 事件是 `ttft_timeout`（0 条 `connect_error`）→ switch 事件归因 `ttft_timeout`**。

建连侧对照（同样走真实循环）：

```
真实拨号 192.0.2.1:9（不可路由）+ 300ms dial 上限
→ INFO node state changed node=n-A state=dead reason="connect_error:... dial tcp 192.0.2.1:9: i/o timeout"
→ 事件 connect_error=1，ttft_timeout=0，节点 dead，重试换节点后 200
--- PASS: TestRev_DialFailure_StillMarksDead (0.31s)
```

⛔ 变异回归：把 helper 里的 `return true` 去掉（首字节超时也一路 `mark`）⇒ **红**：

```
########## M7p 把首字节超时改成也 mark（F2 回归）
--- FAIL: TestFirstByteTimeoutDoesNotMarkNodeDead
--- FAIL: TestRev_StuckFirstByte_KeepsNodeAliveAndSwitches
```

**Bun 路径变体专项核对**（Lead 特别要求）：
- `bun_transport.go:292-295` 在 `exchange()` 的 `fail()` 里对**读响应头阶段**的任何 `net.Error` timeout 统一加前缀 → `bun transport: timeout awaiting response headers: %w` ⇒ 一定命中 ✓；
- **Bun 建连失败不走这条路**：`getConn()` 的错误在 `RoundTrip:213-215` 直接 `return bunContextError(ctx, err)`，不带该前缀 ⇒ `dial tcp …: i/o timeout` 不匹配 ⇒ 仍 mark dead ✓（这是正确的，不能把拨号失败也放过）；
- `bunContextError` 只在**父 ctx 有 cause**（120s 预算/客户端断开）时改写，此时 `ctx.Err()!=nil`，`fail()` 的改写条件也不成立 ⇒ 预算取消仍走 `connect_error`+mark（与 HEAD 一致）。
- 实现方单测已覆盖 `bun transport: timeout awaiting response headers: context deadline exceeded` 这一形态。
- **残留（低）**：没有「真实 Bun + TLS 静默服务端」的端到端用例（仓库无现成 Bun TLS harness），Bun 侧结论来自代码路径 + 错误串单测，不是真实请求。列入 R6。

## R3. F3 — 报告口径（ADDRESSED）

- §1 已改成：「**按计划的两档规则取 8s**，并接受『0.5% 门槛未满足』」；明确写「纯建连耗时不可观测」「8s 档上界 8.95%~18.69% 仍高于 0.5% ⇒ **计划自己设的 0.5% 门槛并未满足**。报告如实记录这一点，**不写成『已测过、达标』**」（原文）。
- §1.2 给出选 8s 的三条理由（失败侧收益确定、误杀可自愈、上界不等于真值）+ 「T5 `dial_ms` 后重算，若 `dial>8s` >0.5% 再回调」。
- 开篇结论已改为「首字节（**= 首 token**）等待」——上一轮 F2 的措辞问题也一并修掉。
- 未发现残留的「测过了/达标」表述。

## R4. 新问题

**N1【协调·必须】T3 现在编译依赖 R4/T2 的未提交代码。** `main.go` 的 4 处 `callLogSwitchEvent(ctx, node, detail, reason, candidates)` 定义在 `calllog.go:2652`，而 **HEAD 的 `calllog.go`/`main.go` 里 `callLogSwitchEvent` 出现 0 次**（`git show HEAD:calllog.go | grep -c` = 0）。⇒ **T3 不能单独提交/单独编译**：必须与 R4 的结构化 switch 事件同批提交，否则 build 断。若 R4 要拆分，T3 侧应退回 `callLogEvent(ctx, "switch", …)`（或用 build 兼容的包装）。

**N2【低】首字节超时仍被 C-5 告警层记成「传输失败」。** `main.go` 两条循环里的 `nodeAlerts.recordNodeAttempt(nodeFp != "", err == nil)` 没跟着改：一次 `ttft_timeout` 会 `successStreak=0` 并 `runFailures++`（`node_alert.go:170-195`），即**连接其实通了、节点也健康，却给节点告警贡献一次失败样本**。19.59s 截断面 2.19%~6% ⇒ 可能制造误告警（**不会**改节点状态：`node_alert.go` 不触碰 `proxyPool`/`mark`）。建议 `err == nil || isFirstByteTimeout(err)`；注意这会让 `zz_c5v2_verify_wiring_test.go:146` 与 `zz_node_alert_test.go:453` 的**源码字符串断言**失效，需同步改（都是本地 gitignored 测试）。

**N3【观察】`ttft_timeout` 对节点打分完全不可见。** `node_scoring.go` 只认 `connect_error`/`upstream_error`（`grep ttft_timeout node_scoring.go` 无命中），`statscan.go:430` 只把它计入面板 `HasTTFTTimeout`。这正是「ICC=0 不该记节点责」的意图，但副作用是：**系统性上游变慢（全节点都慢）不会留下任何节点级信号**，事后只能靠 `ttft_timeout` 事件计数排查。建议在上线观察项里写一句（不是代码问题）。

## R5. T2 × T3 并发核实（Lead 新增必查）

| 检查 | 结果 |
|---|---|
| 两套逻辑都在 | ✅ `poolHalted` 6 处、`note403`、`sub403`（T2）与 `isFirstByteTimeout`/`noteAttemptTransportError`/`ttft_timeout`（T3）共存于当前 `main.go` |
| 是否互相覆盖 | ✅ 无。diff 中唯一被删掉的 `if proxyPool.poolHalted(...)`（2 处）是 **T2 自己的 R2 重构**（`freetier_confirm_suppressed` → `freeTierConfirmAllowed()`），新函数 `proxy_node.go:678-686` 第一句就是 `if p.poolHalted(now)` ⇒ halt 抑制保留完整 |
| halt 与 T3 顺序 | ✅ 两条循环对称：循环顶部 halt 检查（非流式 `:2944`，流式 `:3413`）→ `egress.Get` 后再查一次（`:2958`/`:3427`）→ 才发 attempt；`connect_error` 分支（`:3009`/`:3465`）→ `continue` → 回到顶部 **必先过 halt 检查** ⇒ 首字节超时路径**不会绕过 halt fail-fast** |
| 403 分支 halt | ✅ `:3115`/`:3563`（`note403` 之后立即 fail-fast），T3 未触碰该段 |
| 残留 | ⚠️ `connect_error` 分支**没有** halt 守卫（`noteAttemptTransportError` 直接 `mark`）。核查：**HEAD 的该分支本来也没有**（`git show HEAD:main.go` 该段 `poolHalted` 计数 = 0）⇒ **非 T3 引入**，T2 的「halt 期间不 mark」原本只覆盖 403 confirm 路径。若要求严格覆盖，在 helper 里加 1 行 `pool.poolHalted` 判断即可（建议，非阻塞） |
| build / vet / 全量 | ✅ `go build ./...` = 0；`go vet ./...` 干净；`go test -skip 'TestIP4RV' -count=1 ./...` → **`ok 187.992s` RC=0**（同时段另一位队友也在跑全量，耗时偏高属正常） |

> 归因说明：`main.go` 的 130 行改动里，`callLogSwitchEvent` 替换、halt 检查、403/sub403 属 T2/R4；`directDialer`、`noteAttemptTransportError`、`ttft_timeout` 归因属 T3。复核时按分支逐段对应，未发现 T3 误删 T2 逻辑。

## R6. 本轮仍未覆盖 / 不确定性

1. **Bun + TLS 静默服务端的真实端到端**：无现成 harness，未做；Bun 侧依据是代码路径确定性（`fail()` 无条件加前缀）+ 错误串单测。若要求等价强度，建议实现方补一个 `BunTransport` + `httptest.NewTLSServer`（handler 先读请求再 sleep）的用例。
2. **9 条变异**：我只独立复跑了与 F1/F2 直接相关的 4 条（M6/M7/M8/M7p）；其余 5 条（M1–M5）采信实现方，未逐条重跑。
3. **`node_alerts` 误告警的实际触发阈值**：只确认了它会累计 `runFailures`，未构造足够的连续样本去验证是否真的会走到告警（`node_alert.go` 的阈值/窗口逻辑不在本次范围）。

## R7. 结论与建议

- **F1 / F2 / F3 三条全部 ADDRESSED**，且 F1/F2 都有**我独立跑出来的**变红证据与真实请求证据（不是读实现方的测试报告）。
- 修复**没有**改变 8s/19.59s/idle read/总预算的任何语义；T2 的 halt 语义未被 T3 破坏。
- **T3 可以进入 T4**。提交前必须处理 **N1**（与 R4 同批提交或退回 `callLogEvent`），建议顺手处理 **N2**（1 行 + 2 处本地测试字符串），**N3** 写进上线观察项。
