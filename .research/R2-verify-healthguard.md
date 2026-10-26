# R2 · 独立验收：健康检查加固（防批量误判）+ 请求级总时间预算

> 验收人：teammate `review-t1-design`（**未参与实现**，实现者为 Lead）· 看板 task-9
> 被验对象：test 分支工作区**未提交**改动 `main.go` +42 / `proxy_node.go` +72（`git diff --stat`；HEAD `7fa92493`）+ `zz_healthguard_test.go`（gitignored，留盘，9 条）
> **只读声明**：本报告是唯一写入。未改工作区代码/ROADMAP/台账、未 git add/commit、**未碰 main / 未部署 / 未动生产容器**；生产仅 `docker ps/inspect/logs/exec cat`（只读）。
> **变异测试方法**：全部在 `/tmp/t9mut` 副本（`cp *.go` + `go.mod/go.sum` + `static/*.html`，不含坏文件）上做，**工作区文件一次都没被写**；副本已 `rm -rf`（§12）。唯一工作区临时操作是把基线坏文件移开跑测试，跑完按 **sha256 还原并校验**。

---

## 0. 结论速览

| 项 | 结果 |
|---|---|
| 5 条变异测试 | **(a) 红 ✔ · (b) 红 ✔ · (c) 绿 ✘ · (d) 红 ✔ · (e) 绿 ✘** |
| 是否纸面绿 | **(c)(e) 两条是**（测试自身的 oracle 缺陷，不是代码缺陷）；按任务书规则 **报 blocker** |
| 我另加的流式变异 (f) | 红 ✔ —— 但**官方 9 条测试完全没有覆盖流式路径**（缺口 I3） |
| 代码是否可提交 | **no-go（需先修 2 处测试 oracle + 裁决 75s 预算的数据风险）** |
| 最重的发现 | ★ **75s 非流式预算会砍掉当前"成功"的请求**：全量 ok 非流式 n=173,323，**8.69% >75s**；近三日 10-17 5.55% / 10-18 2.09% / 10-19 4.72%。设计注释「不砍掉慢但会成功的那一次」被数据反驳（§5） |
| 前提是否成立 | 成立且我独立复现：**00:17 一轮 90 个节点被判死**（reason=context deadline exceeded），至 01:52 其中 **81 个仍未恢复**（≈95 分钟）；blip 判据对这几轮全部命中（§3） |

---

## 1. 独立复跑（我自己的输出）

```bash
cd /home/jingyx/project/opencode2api-test
export PATH=$PATH:/usr/local/go/bin GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
go build ./...            # BUILD_EXIT=0

# 基线坏文件 sha256 = fadbdb3e29a245877420bb6432d8c0b0b048bc77edb9b9294058755eee8effc4（跑前 sha 一致）
# 临时移开 → 跑 → 按原样放回 → 再校验 sha（一致，见 §12）
go test -count=1 -run '<9 条>' -v .
--- PASS: TestProbeRoundLooksLikeGlobalBlip
--- PASS: TestProbeNeedsTwoConsecutiveFailuresToMarkDead
--- PASS: TestProbeSuccessResetsFailStreak
--- PASS: TestProbeGlobalBlipMarksNobody
--- PASS: TestProbeConcurrencyIsBounded (0.07s)
    zz_healthguard_test.go:199: OK 总预算：elapsed=454.267158ms attempts=3 status=503
--- PASS: TestUpstreamLoopHonoursTotalBudget (0.46s)
--- PASS: TestUpstreamLoopBudgetDoesNotBreakFastSuccess
--- PASS: TestUpstreamBudgetPatchedInBothLoops
    zz_healthguard_test.go:260: OK 预算 context 已生效：elapsed=300.902999ms status=502
--- PASS: TestUpstreamRequestCarriesBudgetContext (0.30s)
ok github.com/shiyanYX/opencode2api 0.857s

go test -count=1 -race -run 'TestProbe|TestUpstream|TestFT_' .   # ok 2.032s
go test -count=1 .                                              # ok 37.530s（临时移开坏文件）
```
9/9 绿。★ 第一条日志已经埋了 (e) 的伏笔：**`attempts=3` 是被 `canRetry` 截断的**（503 → maxAttempts=3），**不是墙钟 break**（详见 §2-e）。

---

## 2. 变异测试（副本 `/tmp/t9mut`；每条都先 `go build ./...` 确认不是编译错误）

| # | 变异 | 期望 | 实测 |
|---|---|---|---|
| a | 删掉 streak 守卫（`ProbeFailStreak++` 后直接判死） | 红 | **红** ✔ `--- FAIL: TestProbeNeedsTwoConsecutiveFailuresToMarkDead` |
| b | 删掉 blip 闸（整块） | 红 | **红** ✔ `--- FAIL: TestProbeGlobalBlipMarksNobody` |
| c | `probeMaxConcurrent` 8 → 1000 | 红 | **绿 ✘ 纸面绿**（见下） |
| d | 删 `up = up.WithContext(loopCtx)`（两处→`_ = loopCtx`） | 红 | **红** ✔ `上游请求没有带上预算 context：耗时 5.001853991s` |
| e | 删墙钟 break（两处→`_ = upstreamLoopStart`） | 红 | **绿 ✘ 纸面绿**（见下） |
| f | （我加）给**流式**路径套超时 ctx | 红 | **红** ✔（用我写的流式测试；官方 9 条无人察觉） |

### (c) 纸面绿：并发上限的 oracle 引用了被测常量本身

```go
// zz_healthguard_test.go:151
if maxSeen > int64(probeMaxConcurrent) { t.Fatalf(...) }
```
把常量改成 1000 后，断言变成 `maxSeen > 1000` ⇒ 永不触发。我另写仪表测试实测真实峰值：
```
probeMaxConcurrent=1000 → 实测并发峰值 maxSeen=60   （60 个节点同时打 = 复现生产"86 路并发"）
probeMaxConcurrent=8    → 实测并发峰值 maxSeen=8
```
⇒ **机制是对的，测试测不出来**。该测试能抓住"整块删掉信号量"（那时 maxSeen=60 > 8 会红），但抓不住"把上限调大"——而这正是最可能发生的回归。
**最小修法**：断言里用硬编码常量，例如 `if maxSeen > 8`（或 `maxSeen <= probeMaxConcurrent && probeMaxConcurrent <= 16`）。

### (e) 纸面绿：墙钟 break 在该测试里根本不可能被触发

- 测试用 `503 + {"error":"slow"}`，`shouldRetryUpstreamStatus(503)=true` 但 `maxAttemptsForUpstreamStatus(503)=3` ⇒ **最多 3 次 attempt**；budget=400ms、每次 150ms ⇒ 总量 450ms，**第 3 次 attempt 不存在**，`attempt > 0 && elapsed >= 400ms` 的检查永远轮不到。
- 断言 `elapsed > 2s` 与实测 452ms 差 4.4 倍；`calls > 4` 在该场景下**结构上不可能**（上限 3）。
- 实测删掉 break 后：`elapsed=452.677116ms attempts=3 status=503` → **PASS**。
**最小修法（我已经验证过能变红）**：`upstreamLoopBudget = 100ms` 且单次 attempt=150ms ⇒ break 必然在 attempt1 前触发 ⇒ 断言 `attempts <= 1`：
```
pristine : elapsed=151.607293ms attempts=1 status=503   → PASS
MUT-E    : elapsed=453.220845ms attempts=3 status=503   → FAIL ✔ 红
```
另一个更贴规格的写法（我已验证）：把假响应换成**配额信号 429**（`{"error":{"type":"FreeUsageLimitError",...}}`）让循环有机会超过 3 次 attempt，再断言 `attempts <= 2`：
```
pristine : elapsed=403.026866ms attempts=2 status=502   → PASS
MUT-E    : elapsed=603.923994ms attempts=3 status=502   → FAIL ✔ 红
```

### (f) 我补的流式变异：官方 9 条完全没有覆盖流式路径

```python
# 把流式路径的 loopCtx := ctx 换成 WithTimeout + defer cancel（Lead 最担心的那种改法）
loopCtx, mutFcancel := context.WithTimeout(ctx, upstreamLoopBudget); defer mutFcancel()
```
我写的流式测试立刻红：`流式 body 在函数返回后读失败（被超时 ctx 掐断？）: context canceled`。
⇒ **性质本身有测试可测（~30 行），但官方测试里一条都没有**（9 条里没有 `callOpenCodeAPIStream` 的任何调用）。

---

## 3. 生产前提独立复现（只读）

```bash
docker logs --since 2026-10-18T16:20:00 opencode2api | grep 'node marked dead by health check'
```
- 按分钟聚合（本容器 00:15:55 启动）：**00:16 3 个 · 00:17 90 个 · 00:33 89 个 · 00:49 112 个 · 01:04 5 · 01:20 9 · 01:36 24**（共 332）。
- 死因分布：`context deadline exceeded` **107**、`timeout` 39、`dns resolve failed` 2 ⇒ **一次约 2 分钟的出口/网络抖动**（与背景叙述一致）。
- 当前 `/data/proxy_state.json`：227 节点 `{dead:87, available:125, exhausted:15}`；**dead 里 81 个的 `marked_at` 仍是 `2026-10-19T00:17`** ⇒ 距现在（≈01:52）**95 分钟未恢复**，独立证实「一轮 86 个判死、70 分钟不恢复」（我量到 90 个 / 95 分钟）。
- blip 判据复算（我按代码重算）：
```
(218,90)=True  (227,86)=True  (125,89)=True  (125,112)=True  (143,28)=True   # 生产各轮全部命中
(100,5)=False  (8,5)=False    (3,3)=False                                    # 零星/小池子不误伤
```
⇒ **设计要解决的问题真实存在，且 blip 闸对真实那几轮全部命中**。

---

## 4. 流式安全（Lead 最担心的点）——**性质成立，我用临时测试验证过**

我在副本里写的三条（都通过）：

| 测试 | 做法 | 结果 |
|---|---|---|
| `TestT9_StreamBodyReadableAfterExtraBudget` | budget=200ms；假 body **绑定请求 ctx**（ctx 取消即读失败，模拟真实 transport）；`callOpenCodeAPIStream` 返回后**再等 600ms** 才读 body | **PASS**：`len=240`，完整读到 `[DONE]` ⇒ 流式路径没有被套超时 ctx ✔ |
| `TestT9_ClientCancelPropagatesToStream` | 调用后 100ms 取消客户端 ctx | **PASS**：`err=context canceled` ⇒ `up.WithContext` 确实把调用方 ctx 接到了上游（期望行为）✔ |
| `TestT9_NonStreamBodyCutByBudget` | budget=300ms，假 body 900ms 后才产出 | **PASS**：`status=502`（不是 200、也没挂到 2s+）⇒ 非流式预算能切断**单次**慢 body，不再只是「attempt 之间」✔ |

结论：**流式不套超时、非流式套超时**这个二分是正确的；`up.WithContext` 的副作用（客户端断开 ⇒ 取消上游）是期望行为，不误伤流式（前提是 handler 在 `ServeHTTP` 返回前读完 body——它就是这么做的）。唯一缺口是**官方测试没有覆盖它**（I3）。

---

## 5. ★★ Critical：75s 预算会砍掉当前"成功"的请求（数据反驳设计注释）

设计注释：`取值 75s：大于单次建连上限 60s（不砍掉「慢但会成功」的那一次）`。用生产 `live /data/call_log.jsonl` 按 `stream` 拆分（`CallRecord.Stream`，`calllog.go:38`）独立复算：

```
ok  stream=false（= 走 callOpenCodeAPI 的非流式路径）: n=173,323  p50=11.1s  p95=101.8s  p99=232.7s  max=2832s
     >75s = 15,066  (8.69%)      >60s = 22,678
ok  stream=true                                        : n=32,119   p95=44.3s   max=1020s   >75s=741
fail stream=false                                      : n=23,346   p95=285.6s  max=2381s
```
近三日（与改动动机同窗口）：
```
2026-10-17: ok 非流式 2505，>75s 139 (5.55%) | fail 非流式 1745，p95=92.983s
2026-10-18: ok 非流式 3103，>75s  65 (2.09%) | fail 非流式 4529，p95=48.534s
2026-10-19: ok 非流式  339，>75s  16 (4.72%) | fail 非流式  117，p95=105.588s p99=142.482s max=147.933s
```
- Lead 的前提（10-19 失败 p95≈95s / max≈138.5s）**与 10-19 数据吻合**（我量到 p95=105.6s / p99=142.5s / max=147.9s）⇒ 改动动机成立。
- **但**：`p95(成功非流式) = 101.8s > 75s`，且 10-19 当天 **4.72% 的"成功"请求 > 75s**。抽 3 条样本的事件链是 `[connect_error, connect_error, connect_ok]` / `[connect_error, connect_ok]` ⇒ 这些正是**多 attempt、最后一次慢但成功**的请求 —— 也就是 `loopCtx` 超时会**在途掐断**的那一类。墙钟 break 本不会砍它们（break 只在 attempt **之间**检查，最后一次已在途的会跑完），**是 `context.WithTimeout` 把它们砍掉的**。
- ⇒ 该改动把「**为了失败更快**」的收益，建立了「**一部分成功变成失败**」的代价之上。按近三日口径，**≈2~5% 的原成功非流式请求会新变成 502**（全量口径 8.69%）。
- **诚实的不确定**：其中一部分长"成功"可能是**客户端早已放弃、只是网关仍在跑**（旧代码 ctx 是 Background，客户端断开会话也不会取消上游）——那部分被砍掉**不损失用户体验、还省上游配额**。call_log 无法区分这两种，所以 2~5% 是**上界**。
- 建议（三选一，由 Lead 裁决）：
  1. 把预算按数据抬高：≥120s ⇒ 10-17..10-19 的误杀率从 3.7% 降到 ≈2.4%（98+38+8=144 / 5947）；
  2. **只保留墙钟 break，把非流式的 `loopCtx` 也改成 `ctx`**（保留 `up.WithContext(ctx)` 以修客户端断开取消）——这样在途的成功尝试不会被掐，病态尾部靠「不再起新 attempt」收敛；
  3. 明确接受该代价，并把「预期成功损失 ≈2~5%」写进提交说明与上线观测项（这至少让它是**有意识的**取舍，而不是被注释误导）。

---

## 6. 面板「测速」语义（blip 闸会抑制 probeAll）——**会抑制，建议保留但补可见性**

```
TestT9_ManualProbeSuppressedByBlip: 手动测速 checkNodes：probed=25 被判死=0（25 全失败 ⇒ 触发 blip 闸）
```
- 机制：`checkNodes` → `checkFiltered(probeAll)`，同一把闸 ⇒ **操作者主动测速时，只要 ≥20 个节点失败，整轮标记被丢弃**。生产池 227 个（约 90 dead），probeAll 必然 ≥20 失败 ⇒ **手动测速永远不会通过探测去判死任何节点**。
- 我的判断：**保留闸是可接受的、甚至更安全**——操作者点"测速"时如果本机出口正在抖动（正是 10-19 的场景），没有闸会把整池（200+）一次性判死，那比"测速没标死"严重得多。而且 `probeMaxConcurrent=8` 已经把"手动测速自己把自己打瘫"的风险限住了。
- 但**必须补可见性**：`checkFiltered` 在 blip 时也只 `return probed`（`proxy_node.go:797`），面板无法区分「真的一轮全好」与「本轮被不采信」。建议让 `checkFiltered` 把 `suppressed` 一并返回（或返回结构体），面板显示「本轮疑似本机网络问题，未采信 N 个失败」；否则运维会以为"测速过了 ⇒ 都健康"。

---

## 7. 两个设计取舍的判定

**7.1 streak 只在内存、`setNodes` 后不继承 —— 方向安全 ✔（实测）**
```
setNodes 后 streak=0（新对象）
刷新后一次失败仍 available（streak=1）⇒ 方向保守
```
`ProbeFailStreak` 是 `json:"-"` 且 `setNodes` 的 `prev` 分支不复制它 ⇒ 每次订阅刷新后重新数两次连续失败。方向是**保守**（宁可留着节点，也不误杀），符合本次改动的目标；代价是"每次刷新后要重新观察一个周期"，在 15 分钟巡检周期下可忽略。✔ 可接受。

**7.2 blip 闸会不会长期掩盖"真的全池坏" —— 会掩盖"部分真坏"，但不会掩盖"全池坏"，且有兜底 ✔/⚠️**
- **量化**（我写的边界测试）：
```
失败=19/120：5 轮后已判死 19 个      ← 正常判死
失败=20/120：5 轮后已判死  0 个      ← 永远不判死（绝对阈值 ≥20）
```
  ⇒ 阈值在 20 处是一个**悬崖**：任何"一轮 20+ 个探测失败"的**真实**事故（例如某订阅/ASN 整段不可达），**探测层永远不会判死**，这些节点会长期留在 `available`。
- 为什么后果可控（三条兜底）：
  1. **真实流量仍会判死**：这些节点被选中后会产生 `connect_error` → `main.go:2722/3113` 的 `proxyPool.mark(NodeDead, ...)` 与探测层**完全独立**，不受闸限制；
  2. 闸只影响"判死"，**不影响探测延迟写入**（成功仍写 `LatencyMs`）与面板显示；
  3. 闸只在**一轮整体**上生效，不累积 streak，所以一旦某一轮失败数掉到 20 以下（例如节点被流量判死后不再被探测），正常判死立即恢复。
- 但"全池坏"这一侧确实是**刻意掩盖**：如果本机出口彻底坏了，全部节点保持 `available`、`hasEligibleNode()==true`，面板显示"池子健康"——这正是设计想要的（不要把本机问题记成节点问题），但**没有任何升级机制**：连续 N 轮 blip（例如 3 轮 = 45 分钟）与一次性抖动在日志/面板上无区别。
- **建议**：加一个「连续 blip 轮次」计数器，连续 ≥3 轮（≈45 分钟，远超 2 分钟抖动）时打 Warn / 面板告警（`pool_shape_suspect`），提示"可能是真的全池不可达，请查本机出口"。

---

## 8. 其它风险（我补的，实现者未提）

- **P1 · 探测轮墙钟变长（`probeMaxConcurrent=8` 的副作用）**：一轮 N 个节点从"一次并发"变成 `ceil(N/8)` 批。生产当前 87 dead：11 批 ×（成功 p50 ≈2.3s）≈ 25s（可接受）；**全超时最坏 11×20s=220s**，而 `checkDead` 由**每分钟**的 ticker 触发、单轮 ctx 上限 **5 分钟**（`health_runtime.go:29-35`）⇒ dead 复探节奏从 ~1 分钟退化为最坏 ~3.7 分钟（ticker 丢拍），dead 节点"恢复后多久被复活"的时延从 ≤1 分钟变长。available 全量轮（125 个）最坏 16×20s=320s，**超过 5 分钟 ctx** ⇒ 会被截断，剩余节点以 ctx 错误计入失败（此时必然 ≥20 ⇒ 触发 blip ⇒ 反而安全）。**建议**：把 dead 复探与 available 巡检的语义写清，或给大轮更高的并发/更短的 probeTimeout。
- **P2 · blip 判据用的是"调度数"而非"完成数"**：`probed++` 在 `sem <- struct{}{}` **之前**递增（`proxy_node.go` 的 `checkFiltered`），所以 ctx 被取消后快速失败的那批也会计入 `probed` 与 `failedFP`。后果：**被截断的一轮看起来就是 blip**（安全侧），但也意味着"探测自己失败"与"节点失败"无法区分。可接受，建议注释写明。
- **P3 · 流式路径的墙钟 break 无测试**：官方 9 条里没有任何 `callOpenCodeAPIStream` 调用；流式的 break 若被删/写错，测试全绿。我补的流式测试（§4）可直接纳入。
- **P4 · `TestUpstreamBudgetPatchedInBothLoops` 是源码计数断言**（`upstreamLoopStart := time.Now()` 出现 2 次、`up.WithContext(` ≥1 次）——它只锁"改了两处"，不锁行为；`up.WithContext(` 用 `>=1` 而非 `==2`，删掉流式那一处也不红。建议改成 `==2`。

---

## 9. Issues

### Critical (Must Fix)

- **C1 · `main.go:2643-2646`（非流式 `context.WithTimeout`）· 会砍掉当前成功的请求（数据见 §5）**
  为什么重要：成功非流式 p95=101.8s、近三日 2.09%~5.55% 的成功请求 >75s；这些请求是**多 attempt 后最后一次慢成功**，`loopCtx` 会在途掐断（墙钟 break 不会）。
  怎么修：按 §5 的三选一（抬到 ≥120s / 非流式也用调用方 ctx 只留墙钟 break / 明确接受并记录观测项）。**在决定之前不应提交**——因为它改变的是**成功率**，不是延迟。
- **C2 · `zz_healthguard_test.go:151`（并发上限断言自引用）· 变异 (c) 绿**
  为什么重要：把 `probeMaxConcurrent` 调大（本次改动要防的正是"86 路并发"）不会被任何测试发现；实测峰值会从 8 涨到 60。
  怎么修：`if maxSeen > 8 { t.Fatalf(...) }`（硬编码），或再加 `if probeMaxConcurrent > 16 { t.Fatalf("上限被放宽") }`。
- **C3 · `zz_healthguard_test.go:161-200`（总预算测试）· 变异 (e) 绿**
  为什么重要：墙钟 break 是"请求级总预算"在流式路径上的**唯一**机制，也是非流式的第二道；测试却因 `503 ⇒ 最多 3 次 attempt` 结构上不可能触发它。
  怎么修：`upstreamLoopBudget=100ms` + 单次 150ms + 断言 `attempts <= 1`（我已验证：pristine 1 次 / 删 break 后 3 次，稳定变红）。

### Important (Should Fix)

- **I1 · `proxy_node.go:625-633`（blip 绝对阈值 20）· 20+/轮的真实故障永久不被判死**（实测 19/120 全判死、20/120 五轮零判死）。修：加"连续 blip 轮次"计数器与告警（≥3 轮 ⇒ 疑似真故障），或把绝对阈值改为"≥20 **且** ≥25% 池子"这类相对判据（10-19 那轮 90/218=41%，仍会命中）。
- **I2 · `proxy_node.go:797`（blip 时只 `return probed`）· 面板/运维无法区分"真的全好"与"本轮被不采信"**。修：让 `checkFiltered` 返回抑制标志，面板/日志展示「本轮未采信 N 个失败」。
- **I3 · 官方测试零覆盖流式路径**（`callOpenCodeAPIStream` 在 9 条里 0 次调用；流式 break、流式不套超时 ctx 均无测试）。修：把 §4 的三条临时测试纳入 `zz_healthguard_test.go`（我已验证全绿，且对变异 (f) 会红）。
- **I4 · `probeMaxConcurrent=8` 的节奏代价未申报**：dead 复探最坏从 ~20s/轮退化到 ~220s/轮（>1 分钟 ticker），available 全量轮最坏 320s 触及 5 分钟 ctx 上限被截断（§8-P1）。修：把两个轮次的 `probeTimeout`/并发分开配置，或在 `health_runtime.go` 里把 dead 复探改成"每轮最多 N 个"。

### Minor (Nice to Have)

- **M1 · `zz_healthguard_test.go:190-195`**：`elapsed > 2s`（预算 400ms）与 `calls > 4`（上限 3）都不会触发，属结构性死断言；建议随 C3 一起改。
- **M2 · `zz_healthguard_test.go:223`**：`strings.Count(..., "up.WithContext(") < 1` 应为 `!= 2`（两处循环各一次），否则"只改一处"不会被发现。
- **M3 · `checkFiltered` 的 `probed` 语义**（调度数 vs 完成数）建议注释写明，避免后人误读 blip 判据的分母。
- **M4 · `probeRoundLooksLikeGlobalBlip` 的 `total < 8` 分支**：小池子（<8）永不触发 blip，此时一次抖动仍会把整个小池判死（两轮后）。当前生产池 227，无影响；若将来支持小池部署需重新评估。
- **M5 · `health_runtime.go:31-35`**：dead 复探的 5 分钟 ctx 与 `probeMaxConcurrent` 一起会把"截断"变成常态（大池 + 超时），建议把 `checked`（已完成数）与 `scheduled` 分开记录。

---

## 10. 最小修法（按优先级）

1. **C3**：`upstreamLoopBudget=100ms` + 150ms attempt + 断言 `attempts <= 1`（3 行，已验证变红）。
2. **C2**：并发断言改为硬编码 `maxSeen > 8`（1 行，已验证：改常量到 1000 时 maxSeen=60）。
3. **C1**：要么把 `upstreamLoopBudget` 抬到数据支持的值（≥120s），要么非流式也用 `ctx`（保留 `up.WithContext(ctx)`）只靠墙钟收敛；无论选哪个，都把「预期成功损失」写进提交说明。
4. **I3**：把三条流式/预算临时测试并入 `zz_healthguard_test.go`（我已验证全绿 + 对 (f) 变红）。
5. **I1/I2**：加连续 blip 计数 + 让 `checkFiltered` 暴露"本轮被抑制"，否则"真的全池坏"只有日志没有信号。
6. **I4**：给 dead 复探单独的并发/超时预算（或每轮限量），避免 1 分钟节奏被 8 并发拖成 ~4 分钟。

---

## 11. Assessment

**Ready to submit? No（no-go）** —— 需要先做 3 件事（C1/C2/C3）。其中 C2/C3 是纯测试 oracle 修复（各 1~3 行，我已给出并验证的写法）；**C1 是产品取舍**：当前形态会把 2%~5%（全量 8.69%）的**成功**非流式请求变成 502，而设计注释声称不会，必须由 Lead 明确裁决后再提交。

**Reasoning:** 健康检查加固这一半我判定为**高质量、方向正确、且我用生产数据独立证实了它的必要性**——00:17 一轮 90 个节点因 `context deadline exceeded` 被判死、95 分钟后 81 个仍未恢复；streak 守卫（变异 a 红）、blip 闸（变异 b 红）都真实有效，`probeMaxConcurrent` 的机制也真实生效（峰值 8 vs 无闸 60），`up.WithContext` 的修复有决定性测试（变异 d 红，5.0s vs 0.3s）。问题集中在两处：**5 条官方变异里有 2 条是纸面绿**（(c)(e) 的 oracle 缺陷，不是代码缺陷），以及**75s 非流式预算的代价没有用数据核对**——而按 call_log 复算，它砍掉的是"慢但成功"的真实流量，不只是病态失败尾部。

---

## 12. 收尾与还原说明

```bash
$ rm -rf /tmp/t9mut /tmp/t9gotmp /tmp/t9_*.go /tmp/t9_*.txt /tmp/t9_task8.*.bak \
         /tmp/zz_hg_state.json /tmp/zz_t9_*.json /tmp/zz_ft_state.json
$ cd /home/jingyx/project/opencode2api-test
$ git status --short          #  M main.go ; M proxy_node.go ; ?? .research/ …（与开始时一致）
$ git diff --stat             #  main.go | 42 ++ … ; proxy_node.go | 72 ++ …
$ sha256sum task8_bucket_query_test.go
fadbdb3e29a245877420bb6432d8c0b0b048bc77edb9b9294058755eee8effc4   # 与任务书要求一致
```
**如何还原变异测试**：无需还原——全部变异只发生在 `/tmp/t9mut` 的**副本**上（工作区文件从未被写），副本目录已删除；工作区唯一的临时动作（移开基线坏文件跑 `go test`）已按原 sha256 放回并校验。生产只做了只读查询（`docker ps/inspect/logs/exec cat`），未触碰 main/部署/配置。

---

# 13. 复审增量（R2b · Lead 落地 4 条修法后的独立复查）

> 增量范围：`main.go` +44（75s→120s、**非流式也取消超时 ctx**、注释）、`proxy_node.go` +83（新增 `blipStreak` 升级告警）、`zz_healthguard_test.go` 9→11 条（预算断言收紧、并发断言硬编码、+2 条流式）。
> 方法与硬约束同上：变异全部在 `/tmp/t9b` 副本做，工作区零写入；基线坏文件跑完按 sha256 还原（`fadbdb3e…` 一致）。

## 13.0 复跑（我的输出）

```bash
go build ./...                                   # exit 0
go test -count=1 -run '<11 条>' -v .             # 11/11 PASS
    OK 总预算：elapsed=151.138835ms attempts=1 status=503        ← (e) 现在只有 1 次 attempt，断言真正咬住了
    OK 调用方 ctx 已生效：elapsed=201.885399ms status=502
    OK 客户端断开已传导到上游：context canceled
ok  github.com/shiyanYX/opencode2api 1.050s
go test -count=1 -race -run 'TestProbe|TestUpstream|TestStream|TestFT_' .   # ok 2.249s
go test -count=1 .                                # ok 37.529s（临时移开坏文件，跑后还原）
```

## 13.1 六条变异：**6/6 全红**（上一轮 (c)(e) 的纸面绿已修好）

| 变异 | 目标测试 | 实测 | 红的原因（我捕获的断言原文） |
|---|---|---|---|
| (a) 去 streak 守卫 | `TestProbeNeedsTwoConsecutiveFailuresToMarkDead` | **红** ✔ | 「第 1 次探测失败就把 … 判死了」 |
| (b) 去 blip 闸 | `TestProbeGlobalBlipMarksNobody` | **红** ✔ | 节点被判死 / `blipStreak` 断言 |
| (c) `probeMaxConcurrent` 8→1000 | `TestProbeConcurrencyIsBounded` | **红** ✔ | 峰值 60 > 硬编码上限 8 |
| (d) 删 `up = up.WithContext(loopCtx)`（两处） | `TestUpstreamRequestCarriesCallerContext` | **红** ✔ | 「上游请求没收到调用方取消（elapsed=9.0045s）」 |
| (e) 删墙钟 break（两处） | `TestUpstreamLoopHonoursTotalBudget` | **红** ✔ | 「尝试了 3 次（每次 150ms，预算内只该试 1 次）」 |
| (f) 流式路径套超时 ctx | `TestStreamBodyReadableAfterFunctionReturns` | **红** ✔ | 「流式路径的上游请求被套了 deadline ctx」 |

⇒ **(c)(e) 两处 oracle 缺陷已确认修复**（(c) 用测试侧硬编码 `probeConcurrencyAssertBound = 8`；(e) 用 100ms/150ms ⇒ break 必然在 attempt1 前触发，我另测「删 break 后会试到 3 次」）。**(f) 的修法也对**：Lead 自述第一版因假 RT 不理会 ctx 而假绿，现在改成直接断言 `r.Context().Deadline()` 存在，我验证它确实会红。

## 13.2 ★ 新发现的假绿：`TestStreamUpstreamCancelledWhenClientDisconnects` 抓不到 (d)

同一 (d) 变异下（`up.WithContext` 两处删除），该测试**仍然 PASS**（3.00s），且它自己打印的日志暴露了真相：

```
--- PASS: TestStreamUpstreamCancelledWhenClientDisconnects (3.00s)
    OK 客户端断开已传导到上游：ctx 未挂到上游请求：客户端断开后 3s 内未取消
```
原因：它在 3s 超时分支里 `pw.CloseWithError(errors.New("ctx 未挂到上游请求…"))`，于是 `io.ReadAll` **照样返回非 nil error**，`if rerr == nil { t.Fatalf }` 分辨不出「被取消」与「3 秒后自己造了个错」。⇒ 这条测试对**它自己命名的性质**（客户端断开 ⇒ 上游取消）**完全没有判别力**；流式 ctx 传播目前只有**非流式**那条 `TestUpstreamRequestCarriesCallerContext` 在兜。
**最小修法**（3 行）：3s 分支置 `timedOut.Store(true)`，末尾 `if timedOut.Load() || !errors.Is(rerr, context.Canceled) { t.Fatalf(...) }`。

## 13.3 ★★ 新发现的假警报：`checkDead` 轮也会命中 blip 闸 ⇒ 生产常态下 `blipStreak` 每分钟 +1

`checkFiltered` 同时服务三种轮次：`probeAvailable`（15 分钟）、`probeDead`（**每分钟**）、`probeAll`（面板手动测速）。blip 闸与 `blipStreak` 在**三种轮次共用同一段代码**，而 dead 轮天然是「大面积失败」（它们本来就是死的）。

我写的验证（副本内）：
```
第 1 轮 checkDead：probed=90 blipStreak=1
第 2 轮 checkDead：probed=90 blipStreak=2
第 3 轮 checkDead：probed=90 blipStreak=3      ← 第 3 轮起打 ERROR
健康 available 轮后 blipStreak=0
```
生产现状是 `{dead:87, available:125}`（§3 实测），即 **每一个 dead 轮都 ≥20 失败 ⇒ 每一分钟都被判为「全局抖动」**：
- `probe_round_global_blip` **WARN 每分钟一条**（≈1440 条/天）；
- `probe_round_global_blip_persistent` **ERROR 启动后第 3 分钟就来，此后每 10 轮（≈10 分钟）一次**，永久；
- 后果：**这条新告警在正常状态就是常亮的**，真出现本机网络长期异常时反而无法区分（方向被反转）；同时它把运维的注意力从真实信号上引开。
- 注意：dead 轮的 *标记* 后果为零（`markProbeDead` 对 `NodeDead` 早退），所以这纯属**可观测性缺陷**，不影响节点状态。
**最小修法**（3~5 行）：只在“会判死的轮次”计数与告警 —— 例如 `checkFiltered` 按 `filter` 判断：`probeDead` 轮不参与 blip 判定（也无意义），只对 `probeAvailable`/`probeAll` 累加 `blipStreak`；或撤掉 dead 轮对闸门的调用。

同一根因的第二个副作用：`blipStreak` 是**包级全局**，官方测试没有复位它 ⇒ `go test -count=2 -run TestProbeGlobalBlipMarksNobody` 会红：
```
--- FAIL: TestProbeGlobalBlipMarksNobody
    连续抖动计数应为 3，实际 6
```
（测试隔离问题，`t.Cleanup(func(){ blipStreak.Store(0) })` 即可。）

## 13.4 Q2：取消超时 ctx 后的新残留风险（单次 attempt 的 body 无 deadline）

**事实**：现在两条路径都只有 `loopCtx = ctx`；上游请求的时限来源只剩
`dialTimeout=60s`（`proxy_node.go:233`）+ `ResponseHeaderTimeout: 60s`（transport）+ 调用方 ctx。
`http.Server{Addr, Handler}`（`main.go:7415`）**没有 Read/Write/IdleTimeout**；`readAllMarkingFirstData`（`main.go:7446`）只认 EOF/错误，**没有空闲读超时**；`ttft_timeout`/`silence_timeout` 只有 calllog 里的名字、**没有任何发射点**（`grep` 确认）⇒ 两条路径都没有“停滞读”保护。
⇒ **「2xx 之后 body 永久挂住」在机制上可行**（回到 W1/W4 之前的状态）。

**量级（live `/data/call_log.jsonl` 实测）**：
```
ok 非流式 >120s：6,734 条，其中 completion_tokens=0（200 + 零输出，高度疑似挂死）148 条；这些的 max = 2,321,801ms（38.7 分钟）
ok 非流式 >900s：198 条（p50 completion=131072 ⇒ 大多数是合法的 128k 长生成，但 min=0 说明夹杂挂死）
fail 非流式：>120s 3,854 条，>600s 83 条，>1200s 9 条，max 2,381,503ms（39.7 分钟）
```
⇒ 概率低（148/173,344 ≈ **0.085%**）、但**时长无上界**（实测 38 分钟）。每个这样的请求会占住一个 handler goroutine + 一条上游连接；在没有并发上限的路径上累积会造成 slow-leak。

**「空闲读超时」是否更优？——是，而且是正确的形状**：
- 语义：**每读到字节就重置计时**（例如 120s 内没有收到任何字节 ⇒ 中止），对应 `readAllMarkingFirstData` 的每次 `Read` 与流式 body 的每次 `Read`。
- 为什么不会误杀长生成：长生成的**字节一直在流**（实测 >900s 的 198 条 p50 输出 131072 tokens ⇒ 持续有帧），空闲计时不会触发；真正被中止的只有“连接活着但不再出数据”的挂死。
- 实现要点：`context` 的 deadline 不能重置，所以要用 `ReadCloser` 包装（每次 `Read` 前起 `time.Timer`，超时则 `Close` 底层 body 并返回错误）；流式路径要包在**返回给调用方的那个 ReadCloser** 上（`wrapRawSSE` 之后），否则函数返回后仍无人管。
- 与本次改动的关系：**不是本提交的 blocker**（属于“回到改动前行为”+ 客户端断开会取消上游），但建议作为独立任务；若不做，请把 §13.4 的数字写进已知残留风险。

## 13.5 Q3：120s 是否站得住 —— **站得住（方向对），但“砍掉 138s 尾部”只对了一半**

**独立复算（live call_log，非流式 ok）**：
```
最近 4000 条：>75s = 102 (2.55%)，>120s = 60 (1.50%)      ← 与 Lead 的 2.34% / 1.66% 吻合（抽样噪声内）
全历史      ：>75s = 15,068 (8.69%)，>120s = 6,734 (3.88%)
```
⇒ Lead 的 120s 取值有数据支撑；相对上一版 75s（会把 8.69% 的成功请求的“>75s 部分”全部掐断），120s 把**上界**降到 **1.50~3.88%**。

**墙钟 break 的真实杀伤半径（上界）**：
```
ok 非流式 且 duration>120s 且【有失败尝试】= 3,158 条（1.822% of ok）   ← 这条链可能因为「成功那次开始时已 ≥120s」被 break 掉
ok 非流式 且 duration>120s 且【单次尝试】= 3,576 条                    ← 不受墙钟影响（成功那次一开始就在跑）
```
⇒ 真实被切掉的比例 **< 1.82%**（只有“成功那次的**起始**时刻已过 120s”才会被切，很多 120s+ 的链是“单次慢成功”或“成功那次起于 60s”）。

**但它并不能把尾部压到 120s**：墙钟 break 只在 **attempt 之间**决定“还要不要再试”，**绝不能打断在途响应** ⇒ 单次 attempt 的时长无上界（这正是 §13.4 的暴露）。生产 fail 非流式 >300s 仍有 1,061 条、>1200s 9 条、max 39.7 分钟 —— 这些不会因此消失。
⇒ 准确表述应是：「**把重试叠加的病态链**从 8 次尝试压到 ≤120s 起手；**单个慢 attempt 的尾部另有量级，需空闲读超时才能收**」。当前注释里“覆盖旧的 138s 尾部”偏乐观，建议改成上面这句。

## 13.6 Q4：「断言不带 deadline」是否等价于「不会掐断 body」—— **必要但不充分**

- **能证明的**：`hadDeadline=false` 排除了「网关自己给上游流套预算 deadline」这一**唯一新增的**失败模式；这恰好是 (f) 变异要抓的回归。
- **不能证明的**（其他可能掐断 body 的来源）：① 调用方 ctx 自带 deadline（`ctx` 来自 `r.Context()`，若前面有反代/中间件设了超时，body 仍会到点被取消 —— 这是**委托**给调用方，属预期）；② transport 的 `IdleConnTimeout`（只管空闲连接、不管活跃 body）；③ 底层 `CloseIdleConnections()`（只在失败分支调用）；④ 下游断开（写入失败，不是上游 body）。
- **该测试的第二个弱点**：假 RT 返回的 body 是 `io.NopCloser(strings.NewReader(...))`，**不理会 ctx** ⇒ 那段「延迟 600ms 再读」的行为断言在“套了超时 ctx”时也会通过（Lead 已自述第一版踩到）。现在靠 deadline 断言兜住，属**正确的组合**，但建议再加一层：让假 body 在 `Read` 时检查 `r.Context().Err()`（ctx 取消即返回错误）——这样行为断言本身也有牙，即使将来有人删掉 deadline 断言也不会退化（我 R2 初版就是这么写的）。
- 结论：该断言**值得保留**，但注释应写成「证明网关未新增 deadline」，而不是「证明 body 不会被掐断」。

## 13.7 Q5：`blipStreak` 的并发正确性 / 告警充分性 / 手动测速可见性

- **并发**：`atomic.Int64` 的 `Add`/`Store` 无数据竞争（`-race` 通过 ✔）。但**语义**有两个问题：
  (i) §13.3 的 dead 轮污染（最要紧）；
  (ii) 轮次之间没有串行化——`checkDead`/`checkAvailable` 在同一个 ticker goroutine 里是串行的，但**面板手动测速**会从 HTTP handler 并发调用 `checkNodes`。于是「手动轮（非 blip）把 `blipStreak` 清零」会擦掉刚累积的“本机网络长期异常”信号（原子但不互斥）。建议加 `probeRoundMu sync.Mutex` 串行整轮，或让计数只由定时巡检轮维护。
- **告警是否足够**：方向对（连续 N 轮才升级），但**当前阈值 n==3 与 dead 轮污染叠加后变成“常亮 ERROR”**；修掉 §13.3 之后，3 轮 × 15 分钟 = 45 分钟才是合理语义（远超 2 分钟抖动）。建议同时把 `consecutive` 计入 `/api/nodes` 的响应，让面板也能看到。
- **手动测速被抑制的可见性 —— 未解决**：`checkFiltered` 仍然只 `return probed`（`proxy_node.go:816`），面板无法区分「真的全好」与「本轮被不采信」；而且现在 dead 轮的 WARN 每分钟一条，**把这条日志信号本身淹没了**。建议：`checkFiltered` 返回 `(probed int, suppressed int)`（或结构体），`/api/nodes` 的测速响应带上 `suppressed_failures`，面板显示「本轮疑似本机网络问题，未采信 N 个失败」。

## 13.8 对 4 条落地修法的逐条验收

| # | Lead 的修法 | 我的判定 |
|---|---|---|
| 1 | (e) 预算 100ms + 150ms/次 + `attempts<=1` | **已修好** ✔ 变异红，且断言有 50ms 余量、不依赖 canRetry |
| 2 | (c) 新增 `probeConcurrencyAssertBound = 8` 硬编码 | **已修好** ✔ 变异红（峰值 60 > 8） |
| 3 | C1：非流式也取消超时 ctx + 75s→120s + 保留 `WithContext` | **已修好且方向正确** ✔ 数据支持（§13.5）；代价上界从 8.69% 降到 <1.82%；但注释「覆盖 138s 尾部」需按 §13.5 改写，且**单次 attempt 无 body 时限**的新残留要登记（§13.4） |
| 4 | 并入流式覆盖（3 条） | **2/3 有效** ⚠️ `TestStreamBodyReadableAfterFunctionReturns` ✔（(f) 红）、`TestUpstreamRequestCarriesCallerContext` ✔（(d) 红）、**`TestStreamUpstreamCancelledWhenClientDisconnects` 对 (d) 仍绿**（§13.2） |
| 5 | `blipStreak` + 第 3 轮 ERROR | **实现有缺陷** ✘ dead 轮污染 ⇒ 常态常亮（§13.3）；手动测速可见性仍未解决（§13.7） |

## 13.9 残留 Issues（增量）

### Important (Should Fix → 提交前)

- **D1 · `proxy_node.go:800-815`（blip 闸 + `blipStreak`）· `checkDead` 轮把常态判成“全局抖动”**
  证据：90 个 dead 节点 → 三轮 `checkDead` 使 `blipStreak` 1→2→3；生产当前 87 dead ⇒ 每分钟 WARN、每 10 分钟 ERROR、永久。
  影响：新告警在正常态常亮（信号方向被反转），并淹没手动测速被抑制的信号；节点状态无影响。
  修：只在 `probeAvailable`/`probeAll` 轮参与 blip 判定与计数（dead 轮本来也不会 mark）；`zz_healthguard_test.go` 里给 `blipStreak` 加 `t.Cleanup` 复位。
- **D2 · `zz_healthguard_test.go:325-368`（`TestStreamUpstreamCancelledWhenClientDisconnects`）· 对 (d) 假绿**
  证据：(d) 变异下 3.00s **PASS**，日志自曝 `ctx 未挂到上游请求：客户端断开后 3s 内未取消`。
  修：3s 分支置标志位并 `t.Fatalf`，或断言 `errors.Is(rerr, context.Canceled)`。

### Minor

- **D3 · `main.go:2685-2687`（非流式 `up.WithContext` 注释）**：仍写「预算超时与客户端断开取消靠这一行才会生效」，但非流式已无预算超时；改为「客户端断开取消」（流式那段注释是对的）。
- **D4 · `main.go:2594-2597` 注释**「取值 120s：…覆盖旧的 138s 尾部」偏乐观（§13.5：墙钟只压重试叠加，单次 attempt 无上界）。
- **D5 · `zz_healthguard_test.go:137`** `blipStreak` 断言依赖全局状态（`-count=2` 红：期望 3 实际 6）；加复位。
- **D6 · `TestProbeGlobalBlipMarksNobody` 未覆盖「连续 3 轮后 ERROR 升级」的实际分支**（只断言计数 == 3）；建议直接断言 `n==3` 时打了 `probe_round_global_blip_persistent`（可用 `slog` handler 捕获）。
- **D7 · 手动测速可见性未解决**（§13.7）；`checkFiltered` 只返回 `probed`。
- **D8 ·（承接 §13.4）单次 attempt 无 body 时限**：148 条「200 + 0 token + >120s」（max 38.7 分钟）证明会真实发生；建议独立任务做**空闲读超时**（每次读到字节重置，120s 无字节中止），不要用固定总时长。

## 13.10 最小修法（增量，共约 12 行）

1. **D1**：`checkFiltered` 里 dead 轮跳过 blip 分支（或给 `probeRoundLooksLikeGlobalBlip` 加 `filter` 参数只在 available/all 轮生效）；测试里 `t.Cleanup(func(){ blipStreak.Store(0) })`。
2. **D2**：流式取消测试改成断言 `errors.Is(rerr, context.Canceled)`（并保留 3s 超时的失败标志）。
3. **D3/D4**：改两句注释（非流式 WithContext 的目的、120s 的准确表述）。
4. （建议，非阻塞）**D7**：`checkFiltered` 返回 `suppressed`，面板展示；**D8**：登记为空闲读超时的后续任务。

## 13.11 Assessment（增量）

**Ready to submit? No（as-is）→ 落 D1 + D2（约 12 行）后即为 go。**

**Reasoning:** 上一轮的 3 个 blocker 已实质性解决：**(e)/(c) 两条纸面绿变异现在都红了**（6/6 变异全红），**C1（75s 砍成功请求）按数据改对**——非流式取消超时 ctx、120s 只有墙钟语义、保留 `WithContext` 用于客户端断开取消，我用 live 数据复核了 2.55%/1.50% 与“代价上界从 8.69% 降到 <1.82%”，方向正确。但增量引入了两处**可观测性/测试有效性的新缺陷**：`blipStreak` 被 `checkDead` 轮污染 ⇒ 新加的 ERROR 告警在**生产常态**（87 个 dead）下每 10 分钟误报一次、WARN 每分钟一条，恰好反转了它要提供的信号；以及新加的流式取消测试**对自己命名的性质没有判别力**（我删掉 `up.WithContext` 后它仍 PASS 并打印“OK … ctx 未挂到上游请求”）。两者都是小修（各 3~5 行），修完即可提交 test；`§13.4` 的“无 body 空闲超时”建议登记为独立任务，不阻塞本次。

## 13.12 增量收尾

```bash
$ rm -rf /tmp/t9b /tmp/t9bgotmp /tmp/t9b_*.go /tmp/t9b_task8.*.bak /tmp/zz_t9b_*.json
$ cd /home/jingyx/project/opencode2api-test
$ git status --short          #  M main.go ; M proxy_node.go ; ?? .research/ …
$ git diff --stat             #  main.go | 44 ++ ; proxy_node.go | 83 ++   （与复审开始时一致）
$ sha256sum task8_bucket_query_test.go
fadbdb3e29a245877420bb6432d8c0b0b048bc77edb9b9294058755eee8effc4
```
**还原**：变异只在 `/tmp/t9b` 副本（工作区零写入），副本已删；基线坏文件已按原 sha256 放回。生产仅只读（`docker logs/exec cat`），未碰 main/部署/配置。
