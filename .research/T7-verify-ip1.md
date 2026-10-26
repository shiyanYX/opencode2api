# T7 · 独立验收：IP-1（confirm-then-mark）实现

> 验收人：teammate `review-t1-design`（**未参与实现**）· 看板 task-7
> 被验对象：test `a19d3a4` 工作区的 **未提交**改动（`main.go` +133 / `proxy_node.go` +89 / `quota.go` +7）+ `zz_freetier_confirm_test.go`（gitignored，留盘）
> 规格：`.research/T1-403-design-v2.md`（673 行）· 实现说明：`.research/T6-impl-notes.md`（88 行）
> **只读声明**：本报告是本次唯一写入。未改任何仓库文件、未 git add/commit、未改 ROADMAP/台账、未动生产容器与配置。
> **变异测试方法与还原**：全部在**临时副本** `/tmp/t7mut`（`cp *.go go.mod go.sum static/*.html`）里做，**工作区文件一次都没被写**；跑完 `rm -rf /tmp/t7mut`（见 §9 收尾）。唯一的工作区临时操作是把基线坏文件 `task8_bucket_query_test.go` 移开跑测试，跑完按 **sha256 还原并校验**（`fadbdb3e…8effc4` 一致，§8）。

**结论：go（可提交）** —— 变异测试全部按预期变红（**不是纸面绿**），并发/锁/竞态干净，两处循环对称，C1 守卫真实有效。**6 条 Important + 4 条 Minor**，其中一条带**明确的前置条件**：`gateReviveRequiresProbe` 置 `true` 之前必须先修 §4-I3（gate_state 陈旧泄漏）。

---

## 0. 本次最关键的三条答案（先给结论）

| 问题 | 答案 |
|---|---|
| **变异测试是否真的红？** | **是。**（a）把 `pendingVer != attemptVer` 改恒假 → `TestFT_VersionChangedBetweenAttempts_NoMark` **FAIL**；（b）把 mark **搬到失败路径**（计数仍=2）→ `TestFT_FormRegression_NoMark` **FAIL**；两者都不是纸面绿。 |
| **有无 blocker？** | **无。** 默认配置（预算 1 + 豁免开关 false）下未发现功能缺陷：换节点会触发、mark 只在成功侧、归因守卫有效、`-race` 干净、无死锁、真区域限制未被误杀。 |
| **go/no-go** | **go**，附 6 条 Important（最紧的一条：Flipping `gateReviveRequiresProbe=true` 前必须先修 gate_state 陈旧泄漏）。 |

---

## 1. 逐条 diff 对规格（v2 全部条目）

| v2 条目 | 落地情况 | 证据（文件:行号） |
|---|---|---|
| **C1** `pendingVer` + mark 前版本比对 | **落地** | `main.go:2738`（`if pendingVer != attemptVer`）/`:3095`（流式同）；记录点 `main.go:2803-2804` 与 `3160-3161`（`pendingBadFp`/`pendingVer` 成对写） |
| **C2** 真 E2E | **落地（我复跑绿）** | `zz_freetier_confirm_test.go:135`（FormRegression）/`:203`（AltNode）/`:236`（C1）；夹具 `:27-84` |
| **C3** 复活契约（a′ 专属字段） | **落地但默认关闭** | `proxy_node.go:97-101`（字段）、`:195`+`:957`+`:989`（持久化）、`:310`（豁免）、`:509`（unmark 清）、`:573-583`（mark 写 rejected）；开关 `:536` **默认 false** |
| **I1** 删守卫 B | **落地** | 新块条件只有 `status==403 && isFreeTierErrorBody && attemptVer==pinnedOCVersion && nodeFp!="" && switches<budget`（`main.go:2795-2796` / `3152-3153`），无 `versionRefreshed` |
| **I2** 版本来源 | **落地** | `main.go:294-298`（`ocClientVersion()` 持锁）、`:2526`（builder 增 `ver` 参数）、`:2548`（**不再读全局**，用 `ver`）、`:2651`/`:3026`（`attemptVer := ocClientVersion()` 后传入） |
| **I3** mark 钉在 `return` 正前方 | **落地** | 非流式 mark `:2744` → return `:2756`；流式 mark `:3101` → return `:3113`；非流式的 `readErr` 早退在 `:2700-2702`，**在 mark 之前** |
| **I4** 流式 header 级确认（含 0.67% 取舍说明） | **落地** | `main.go:3093-3094` 注释 + `:3092` 指明 §6.2 |
| **I5** 预算 = 1 | **落地** | `proxy_node.go:528` `freeTierSwitchBudget = 1`（注释含实测 2.91%/0%） |
| **I6** 空转可观测 | **落地** | `main.go:2654-2657` / `3011-3014`（`freetier_switch_noop` + Warn） |
| **I7** 因果链更正 | **仅注释/文档**（无代码需要改） | 与 `quota_halt.go:228-246` 现状核对一致 |
| **I8** 两条循环「成功」判据不对称写明 | **落地** | 流式 `:3093-3094`；非流式读完整 body（`:2699-2702`） |
| **M1/M3/M5** | v2 文档级已改，代码无对应需求 | — |
| **M2** 文案变体不得判为区域限制 | **落地**（且**超出了 v2**：改了判定器） | `quota.go:124-131` 早退；测试 `zz_freetier_confirm_test.go:337` |
| **M4** 「mark 只在 2xx 块内 + 两处都改」 | **部分落地（偏弱）** | `zz_freetier_confirm_test.go:352-383`：符号计数=2 ✔；**位置检查无效**（§2-c 变异证明） |
| **M6** `Invalidate()` no-op 注释更正 | **落地** | `main.go:2806-2808` / `3163-3165` |
| **M7** `gate_latency_ms` 另存 | **落地（文档级）** | v2 §4.4；T1 侧无代码改动（探针不在本次范围） |
| **v2 §10 的 15 条测试** | **落地 9 条** | 缺 **T1**（未抽纯函数）、**T6/T7**（钩子已实现却没测）、**T9**、**T11**、**T14**、**T15**；T12 用了弱化版。其中 T6/T7/T9/T14 我**今天补测全部通过**（§5），属覆盖缺口而非缺陷；T1 缺失 ⇒ 换节点判定只有 E2E 间接覆盖 |

---

## 2. 变异测试（在 `/tmp/t7mut` 副本上做，工作区零写入）

副本构造：`cp *.go go.mod go.sum` + `static/*.html` + 不复制坏文件 ⇒ `go build ./...`=OK。基线备份 `main.go`/`proxy_node.go` 各一份，每次变异后从备份还原。

### (a) `pendingVer` 归因守卫改坏 → **必须红，实测红**

```bash
sed -i 's/if pendingVer != attemptVer {/if false { \/\/ MUT-A/' main.go     # 两处循环同时命中
$ go test -count=1 -run 'TestFT_VersionChangedBetweenAttempts_NoMark' .
--- FAIL: TestFT_VersionChangedBetweenAttempts_NoMark (0.00s)
    C1 失效：版本变了却仍 mark 了 n-A（{State:available … GateState:} -> {State:exhausted … LastError:freetier:confirmed_ip_rejected GateState:rejected}）
FAIL   MUT_A_EXIT=1
```
⇒ **C1 的守卫不是纸面绿**：守卫一旦失效，测试立刻用「节点状态快照被改动」报警。

### (b) 把 mark 从成功路径挪到失败路径 → **必须红，实测红（但要区分两种"改坏"）**

- **(b1) 直接删掉成功侧 mark 调用**（`if pendingBadFp != "" {` → `if false {`）：
  `TestFT_FormRegression_NoMark` **仍然绿**（没有任何 mark，自然「零变更」），但 `TestFT_AltNodeSucceeds_MarksBadNode` **FAIL**（`期望 n-A 被标记为 NodeExhausted，得 available`）。
  ⇒ 任务书 (b) 括注「或删掉调用 ⇒ FormRegression 必须变红」**按字面不可满足**；正确定性只由**正向测试**把守。
- **(b2) 真正的"位置改坏"**：把 mark 调用**挪进** block(3)（`continue` 之前）——
  `TestFT_FormRegression_NoMark` **FAIL**：`形态回归下节点 n-A 状态被改动：{available…} -> {exhausted…LastError:freetier:confirmed_ip_rejected}` ✔
- **(b3) 移动而非新增**（成功侧删、失败侧加，`proxyPool.markFreeTierRejected(` 计数仍 = 2）：同样 `FormRegression` **FAIL** ✔，但 `TestFT_BothLoopsPatched` **PASS** —— 见下。

### (c) `freeTierSwitchBudget = 2` → **9 条测试全绿（完全无覆盖）**

```bash
sed -i 's/freeTierSwitchBudget = 1$/freeTierSwitchBudget = 2/' proxy_node.go
$ go test -count=1 -run 'TestFT_' -v .      # 9/9 PASS
```
- 仪表：形态回归下 attempt 数 2 → **3**（`hits=[n-A, ?, n-A]`，第三次还抽回了同一个节点）。
- 4 节点场景（A=403, B=403, C=403, D=200）**budget=2**：`status=200 A=available B=available C=exhausted D=available` ⇒ **只有最后一个失败节点被确认，A/B 逃逸**（`pendingBadFp` 被覆盖）；**budget=1** 对照：`status=403` 且零 mark。
⇒ 预算常量**没有任何测试钉住**；`pendingBadFp` 的"只在预算=1 下才正确"这个隐含前提既没写进注释也没防线（有人把 const 改成 2 不会红）。

### (d) 持仓总结

| 变异 | 期望 | 实测 | 判定 |
|---|---|---|---|
| MUT-A `pendingVer` 守卫恒真 | C1 测试红 | **红** | ✅ 有牙 |
| MUT-B1 删 mark 调用 | —（括注不可满足） | FormRegression 绿 / AltNode 红 | ⚠️ 括注表述不成立，正向测试兜住 |
| MUT-B2/B3 mark 进失败路径 | FormRegression 红 | **红** | ✅ 有牙（这是任务书点名的性质） |
| MUT-C 预算=2 | 应有测试察觉 | **全绿** | ❌ 缺口（I5） |

---

## 3. 夹具可信度（Lead 特别问的三点 + 指纹撞车）

自写仪表测试（副本内 `zz_t7_instrument_test.go`，包装 `testNodeClient` 计数 + 记录每次 RT 收到的 UA/host）：

```
formregression(all403) status=403 hookCalls=2 attempts(RT hits)=2 byID=3 distinctFPs=3 realClientsBuilt=0
                       hits=[n-A|opencode/1.18.33 @opencode.ai  opencode/1.18.33 @opencode.ai]
altnode(B ok)          status=200 hookCalls=2 attempts(RT hits)=2 byID=3 distinctFPs=3 realClientsBuilt=0
```
1. **真的驱动了两次 attempt** ✔（两个场景都是 2 次；`hookCalls == 实际 RT 命中数`）。
2. **`testNodeClient` 真的按节点/按 attempt 被调用** ✔（每 attempt 一次，与 `egress.Get`→`getClient` 的调用次数一致）。
3. **没走真实网络** ✔：`realClientsBuilt=0` —— `getClient` 在注入钩子命中时**提前 return**（`proxy_node.go:740-746`），从不构造/缓存真 client；请求 host 是硬编码 `opencode.ai` 但由假 RoundTripper 直接应答（无 TLS/无 SOCKS5）。
4. **指纹不再撞车** ✔：三节点 `127.0.0.1:10001/10002/10003`（端口不同）⇒ `byID=3`、`distinctFPs=3`。第一版"三节点塌成一个"的退化**已消除**（测试文件 `:37-43` 的注释也写明了原因）。
5. 夹具状态文件写在 `/tmp/zz_ft_state.json`（仓库外，`zz_freetier_confirm_test.go:34`）⇒ 不污染工作区。

---

## 4. 并发 / 锁 / 竞态

**4.1 `freeTierMarkAllowed` → `availableCount()`（RLock）不会被持 `p.mu` 写锁的路径调用** ✔
- `freeTierMarkAllowed` 调用点只有 `main.go:2743` / `:3100`（两个 2xx 分支）；`grep -n 'proxyPool\.mu\|proxyPool\.byID' main.go` → **无输出** ⇒ `main.go` 从不直接持有池锁，这些位置前面只有 `egress.Get`（内部自成对锁）、纯函数与日志。
- `availableCount()`（`proxy_node.go:545-555`）只取 `RLock`；`markFreeTierRejected` 是 `p.mark()`（自成对锁）**之后**再单独 `Lock`，两次加锁顺序不嵌套 ⇒ 无自死锁。

**4.2 `ocClientVersion()`（持 `ocInitMu`）没有被三个持锁函数内部调用** ✔
```
ocClientVersion() 调用点：main.go:2523（buildOCRequest，无任何调用者的死代码）、:2651、:3026（两条循环）
ocInitMu.Lock() 出现处：main.go:295（ocClientVersion）、:345（initOCSession）、:363（refreshOCSession）、oc_version.go:65（revertToPinnedOCVersion）
三个持锁函数体内 ocClientVersion 出现次数：0 / 0 / 0
```
⇒ 无重入死锁。`ocClientVer` 的其余引用只有 `:2768`/`:3125` 的日志读（既有、未改）。

**4.3 R1 §判定-3 交错被真堵住**（自写测试，用**持锁**写者复刻 t3，`-race` 下跑）
```
status=200 UA=[opencode/1.18.33 opencode/1.18.33 opencode/1.19.3] probes=[1.18.33 1.18.33 1.19.3]
事件=[switch switch connect_ok freetier_confirm_rejected_version_changed]
attempt0/1/2：发出的 UA == 构建后立刻读到的全局版本  ⇒ I2 落地
n-A 快照不变（未 mark）⇒ C1 生效
```
顺带证实：当块(2) 先发生版本回退时，整条链会走成 **3 次 attempt**（A 403 → 回退 → 同节点 A 再 403 → 换节点），每次 attempt 的 UA 都等于它自己起始时的全局快照。

**4.4 `-race` 干净** ✔
```
$ go test -count=1 -race -run 'TestFT_' .        → ok github.com/shiyanYX/opencode2api 1.040s
$ go test -count=1 -race -run 'TestT7_RaceLockedWriter' -v .   # 200 次调用 + 持锁写者并发
--- PASS: TestT7_RaceLockedWriter (0.23s)        （无 WARNING: DATA RACE）
```
**未发现本次引入的新竞态**：改动把读侧从「裸读」收窄为「持锁读」，`attemptVer` 由本请求携带 ⇒ 实际是**减**竞态。仍存的裸读只有 426 分支的日志读 `:2768`/`:3125`（既有，非本次引入）。

**4.5 ★ 新引入的锁点会放大既有停顿（Important，见 §6-I6）**
`refreshOCSession()` 在**持 `ocInitMu`** 期间做网络调用：`main.go:363-374` → `fetchOCVersionDirect()` → `ocVersionClient()` 超时 **5s**（`oc_version.go:185-188`）。请求入口 `initOCSession()`（`:2591`/`:2969`）本来就持同一把锁，所以这不是全新类别；但本次把锁点从「每请求 1 次」扩到「每请求 + 每 attempt 1 次」，配额切节点风暴（生产日志 44 次 `quota_node_switch`）期间会多出可被阻塞的读者。**未实测**（需要真实慢 npm 请求），仅代码级判断。

---

## 5. C3 契约的副作用 / 持久化（Lead 问的第 5、7 项）

**5.1 `sweepExpiredLocked` 豁免没有破坏既有复活路径** ✔
- 豁免条件 `gateReviveRequiresProbe && n.GateState == GateStateRejected`（`proxy_node.go:308-312`），只跳过 exhaustion 的时间复活，其它状态逻辑不变。
- `TestFT_SweepSkipsGateRejected` 两个分支**都真覆盖**（日志原文：`开关关→复活，开关开→保持 exhausted`）：开关关 ⇒ 走原逻辑翻回 available；开关开 ⇒ 保持 exhausted。
- `recordProbeSuccess` 仍旧只复活 `NodeDead`；我这里补了 v2 T9 的断言（临时测试）：`markFreeTierRejected` 后 `recordProbeSuccess(fp,5)` → `State=exhausted GateState="rejected"` ✔（`/models` 语义不会把 rejected 放回来）。
- 全量既有测试（`go test .`）**ok 37.003s**（§8）⇒ 没有既有复活路径被破坏。

**5.2 ★ `gateReviveRequiresProbe` 默认 false ⇒ C3 豁免在运行时完全不生效**（Important）
- `proxy_node.go:536` 默认 `false`；`gate_state` 的唯一读点就是豁免那一行（`grep GateState` 全量结果里没有第二处读者）⇒ 今天是**只写不读**（除持久化外）。
- 后果：被 confirm 标记的节点仍然是「`NodeExhausted` + 5h 冷却到期无条件复活」= **R1-C3 指出的缺陷在线上依旧成立**；T1 单独上线的行为等价于 v1 的 5h 冷却。
- v2 §4.1 明确建议了这个降级（"把豁免的开关默认关"），所以这是**照 v2 落地**；但 `.research/T6-impl-notes.md` §4.1 只说"T3 上线时置 true 即可"，§5 残留风险表**没有列出"C3 当前不生效"**这一条 ⇒ 属遗漏申报。

**5.3 ★ 持久化：写侧 OK，读回是 no-op**（Important）
- 我构造的往返测试（副本内）：
```
写侧：文件含 gate_state: true          # proxy_state.json 里确实有 "gate_state": "rejected"
重启后：A.State=available A.GateState=""  B.State=available
=> ★ loadState 未生效：重启后 gate_state 丢失（与 State/Cooldown 一样）
```
- 原因（代码级）：`var proxyPool = newProxyPool("proxy_state.json")`（`proxy_node.go:263`）在**包级变量初始化**时运行，`loadState()`（`:970-994`）遍历 `p.nodes` —— 此时**池是空的**；唯一注入节点的是 `subscription.go:252` 的 `setNodes`，而它取"上一版状态"的来源 `old := p.byID`（`:866`）在首次调用时同样是空的。⇒ **所有运行时状态（不只 gate_state）重启后都不会恢复**，`proxy_state.json` 实际是只写文件。
- 这是**既有缺陷**（非本次引入），但 T6 新增字段继承了它，且 `.research/T6-impl-notes.md` §5 写的"`gate_state` 需要重启/重载才从 `proxy_state.json` 读回（`loadState` 在启动时）"是**错的**（它是 no-op）。C3 契约的"持久字段"前提因此不成立。

**5.4 ★ gate_state 陈旧泄漏（Important，且是"开关置 true 前必须修"的那条）**
```
markFreeTierRejected → connect_error 标 NodeDead → recordProbeSuccess 复活：
    复活后：State=available GateState="rejected"        ← 无人清
再被【配额块】标 NodeExhausted（reason=quota:…）+ 冷却到期（开关开）：
    配额冷却到期后：State=exhausted                      ← 配额节点被当成 IP 拒绝，永不时间复活
```
`mark()`/`recordProbeSuccess`/`mark(fp,NodeDead,…)` 都不复位 `gate_state`，只有 `unmark` 清。⇒ 在豁免开关打开后，任何**非 FreeTier 原因**的 exhaustion 都可能继承 rejected 豁免而被永久钉住（只能等闸门探测或人工 unmark）。
修法建议：`mark()` 里对非 `freetier:confirmed_ip_rejected` 的 reason 复位 `gate_state`；或在 `recordProbeSuccess` / `mark(...,NodeDead)` 复位。最稳是让 `gate_state` 的置位/清除只发生在 `markFreeTierRejected` 与 `unmark` 两点，并在 `mark()` 中显式清理不匹配的旧值。

---

## 6. `quota.go` 的额外改动：不会误杀真区域限制（Lead 问的第 6 项）

`quota.go:124-131` 早退只在 body 含 `FreeTierError` 时生效（`isFreeTierErrorBody`）。我做了**正对照**（副本内临时测试）：

| 场景 | body | 期望 | 实测 |
|---|---|---|---|
| 真区域限制（关键词） | `{"error":{"type":"RegionError","message":"This model is not available in your region"}}` | true | **true** |
| 真区域限制（**生产 2026-09-17 的真实形态**：403 无签名 → 空 region） | `{}` | true | **true**（region=""） |
| `only available in japan` | — | true | **true**（region="jp"） |
| FreeTierError 文案变体（含 `only available in`） | — | false | **false** |
| 同时含 `RegionError` 与 `FreeTierError` | — | 取舍 | **false**（唯一取舍点，可接受） |
| 429 配额 | — | false | false |

证据补充：live `/data/call_log.jsonl` 里历史上**唯一**一条 `region_restriction` 事件（`2026-09-17T11:49:08`）的 detail 是 `http 403 region_detected:`（空区域）⇒ 走的是 `quota.go:167` 的"403 无签名"分支，与本次早退**无关**，且该分支仍未被影响 ✔。
⇒ **不会误杀真区域限制**。但注意：现有测试 `TestFT_RegionClassifierNotHijacked`（`:337-348`）**只有反例**，没有"真区域限制仍命中"的正例 ⇒ 若有人把早退改成无条件 `return false`，测试仍绿（test gap，非缺陷；正例内容我已给在 §5 表里）。

---

## 7. 两处循环对称

```bash
# block(3)（从注释行起各取 24 行）
$ diff <(sed -n '2790,2813p' main.go) <(sed -n '3147,3170p' main.go)   → BLOCK3 IDENTICAL (24 lines each)
# 确认块（去注释后）
$ diff <(sed -n '2736,2757p' main.go|grep -v '^\s*//') <(sed -n '3092,3114p' main.go|grep -v '^\s*//')
19c20
< 			return b, resp.StatusCode, resp.Header, nil
---
> 			return newToolNameRewriter(wrapRawSSE(resp.Body), toolMapping, injectedTools), resp.StatusCode, resp.Header, nil
```
⇒ 除**return 语句本身**（流式/非流式固有差异）外**逐字相同**；mark 分别位于两个 `return` 的**正前方**（`:2744`→`:2756`、`:3101`→`:3113`），且非流式的 `readErr` 早退（`:2700-2702`）在 mark **之前** ⇒ I3 落地 ✔。`freetier:probe_alt_node`/`attemptVer := ocClientVersion()`/`proxyPool.markFreeTierRejected(` 三者在 `main.go` 各出现 **2** 次 ✔（两处循环都改了）。

---

## 8. 独立复跑（我的输出 vs 实现者贴的）

| 命令 | 我的输出 | T6 说明里的 | 差异 |
|---|---|---|---|
| `go build ./...` | exit 0 | exit 0 | 一致 |
| `go test -count=1 -run 'TestFT_' -v .` | **9/9 PASS**，`ok … 0.013s` | 9/9 PASS，`ok … 0.010s` | 一致（时间噪声） |
| `go test -count=1 -race -run 'TestFT_' .` | `ok … 1.040s` | `ok … 1.044s` | 一致 |
| `go test -count=1 .`（临时移开坏文件） | **`ok … 37.003s`** | `ok 35.463s` | 结果一致、墙钟不同（机器负载）；无实质不一致 |
| 基线坏文件 | 移开→跑→按 sha256 还原，`fadbdb3e…8effc4` ✔ | 同 | 一致 |

9 条测试清单（我的 `-v` 输出）：`FormRegression_NoMark` / `Mutant_FailurePathMarkDetected` / `AltNodeSucceeds_MarksBadNode` / `VersionChangedBetweenAttempts_NoMark` / `SweepSkipsGateRejected` / `UnmarkClearsGateState` / `ClassifyQuotaIgnoresFreeTierError` / `RegionClassifierNotHijacked` / `BothLoopsPatched`。
★ 注意：不加 `-count=1` 时 Go 会返回 `(cached)`（我第一次就撞到）——实现者贴的是非 cached 输出，一致。

补测（我自己写的临时测试，**全绿**，用于填 v2 §10 的缺口）：
`FixtureInstrumentation`（夹具可信度）、`C1_LockedVersionBump`（C1+I2）、`RaceLockedWriter`、`T6_ShapeLatchSuppressesMark`、`T7_PoolFloorSuppressesMark`、`T9_ProbeSuccessDoesNotReviveRejected`、`RegionClassifierPositiveControl`、`PersistRoundTrip`、`StaleGateStateLeak`、`Budget2Escapes`。

---

## 9. 实现者没做到的 / 漏报的（逐条判断 T6 §4 的 5 条偏差 + §5 的 5 条风险）

### §4 偏差逐条判断

| # | 偏差 | 判断 |
|---|---|---|
| 1 | `gate_revive_requires_probe` 用包级 var 而非配置项 | **可接受**（与 `dialTimeout` 先例一致，改动面最小）。**但漏报后果**：见 I1（默认 false ⇒ C3 不生效），且"置 true"需要改代码重编译，不是运维可调 |
| 2 | `shapeSuspectNow`/`freeTierFloor` 用钩子变量、默认不抑制 floor=0 | **可接受**，与 v2 §4.2/§4.3 一致；我补测证明两条抑制路径行为正确（I2 仅"缺测试"） |
| 3 | 抑制事件合并为 `freetier_confirm_suppressed` | **可接受**（好 grep），但丢了 v2 §4.2 要的 `available_before`/`marks_suppressed` ⇒ 与 T3 的 `freetier_shape_regression` 无法对账"压制了多少次"（Minor） |
| 4 | 额外改了 `classifyRegionRestriction` | **可接受且必要**（我做了正对照，真区域限制仍命中）。**但申报口径偏轻**：v2 §9-M2 只要求"加文案变体单测"，改判定器本身是**行为变更**，应升级为"超出 v2 的功能改动 + 需要正对照测试"（当前只有反例，见 §6） |
| 5 | `TestFT_BothLoopsPatched` 用符号计数替代大括号配对 | **不完全可接受**：我用 MUT-B3（把 mark 搬到失败路径、计数仍=2）证明该断言**仍然 PASS** ⇒ M4 的"mark 只在成功侧"这半没锁住；真正兜住的是 `FormRegression_NoMark`。建议改成真正的块归属判定，或直接删掉位置检查并在注释里指明"由 E2E 兜底" |

### §5 残留风险逐条 + 我补的漏报

| T6 §5 条目 | 判断 |
|---|---|
| U3 同 IP 多 fingerprint | 同意，**无法判定**（`proxy_state.json` 无地址字段；需订阅解析后的 host:port 去重）。这是 A1 唯一的原理性缺口 |
| 形态回归每请求多 1 次上游尝试 | 已证实（我的仪表：all-403 场景 2 次 attempt） |
| `ocClientVer` 既有无锁读只收窄未消除 | 属实；我实测 `-race` 干净（无并发裸写者时不报） |
| `gate_state` 需重启/重载才读回 | **❌ 错**：`loadState` 在空池上是 no-op，重启后**根本不读回**（§5.3） |
| 流式 header 级 0.67% 假阳性 | 同意（v2 §6.2 已接受） |

**我补的新发现（T6 未申报）**：
- **P1** C3 豁免默认关闭 ⇒ R1-C3 缺陷仍在运行时（§5.2）。
- **P2** `loadState` no-op ⇒ 所有运行时状态（含 gate_state）重启丢失；§5 的表述相反（§5.3）。
- **P3** gate_state 陈旧泄漏：非 FreeTier 路径复活/重标时不清 ⇒ 配额节点可能被永久钉住（§5.4）。**这是置 `true` 前的前置修复项。**
- **P4** 预算常量零测试覆盖；budget=2 下 `pendingBadFp` 被覆盖、第一个坏节点逃逸（§2-c）。
- **P5** `markFreeTierRejected` 的 mark 与 gate_state 写入**非原子**（两次加锁，中间可插入 `unmark`）。
- **P6** `availableCount()` 不做到期清扫（与 `hasEligibleNode()` 语义不同）⇒ T3 接真实 floor 后可能低估可用数（floor=0 时无影响）。
- **P7** `ocClientVersion()` 每次 attempt 取全局锁，而 `refreshOCSession` 持该锁跨 5s 超时网络调用 ⇒ 放大既有停顿（§4.5）。
- **P8** v2 §10 缺 6 条测试（T1/T6/T7/T9/T11/T14/T15 中的 6 条），其中 T6/T7/T9/T14 **今天就能测且我补测全绿** ⇒ 是"覆盖缺口"而非"缺陷"。
- **P9** 反例：`TestFT_Mutant_FailurePathMarkDetected` 是"测试的测试"（在假 RT 里注入 mark），不经过生产码路径；它的价值被我的 MUT-B2/B3 覆盖（后者才真正驱动生产分支）。可以在注释里降级其地位，避免后人误以为它守住了生产路径。

---

## 10. Issues

### Critical (Must Fix) —— **无**

### Important (Should Fix)

- **I1 · `proxy_node.go:536`** `gateReviveRequiresProbe` 默认 false ⇒ **C3 契约运行时完全不生效**，被 mark 的节点仍是 5h 后无条件时间复活（= R1-C3 指出的缺陷）。T6 §5 未申报该后果。**怎么修**：要么接受并在残留风险里写清（含"T3 上线前该字段只写不读"），要么本次就连同 §5.1 的 `sweepExpiredLocked` 一起把开关置 true —— 但置 true 前必须先修 I3。
- **I2 · `zz_freetier_confirm_test.go:352-383`** M4 的位置断言无效（MUT-B3 下仍 PASS）。**怎么修**：按 v2 §M4 原意做真正的块归属判定（括号配对/缩进），或删掉位置检查并注明"由 `TestFT_FormRegression_NoMark` 兜底"。
- **I3 · `proxy_node.go:451-483` / `:695-710`** gate_state 陈旧泄漏：非 FreeTier 原因的 mark / `recordProbeSuccess` 复活都不复位 `gate_state`；实测"配额节点继承 IP 拒绝豁免、冷却到期仍 exhausted"。**怎么修**：在 `mark()` 中对 `reason != "freetier:confirmed_ip_rejected"` 复位 `GateState=unknown`（并把 `markFreeTierRejected` 的置位改为在**同一次加锁内**完成，顺带修 P5）。**这是 flip 开关的前置条件。**
- **I4 · `proxy_node.go:970-994` + `:263`** `loadState` 在包级初始化时对空池执行 ⇒ `gate_state`/State/Cooldown 重启后全部丢失，`proxy_state.json` 只写不读；T6 §5 的表述与事实相反。**怎么修**：把 `loadState` 移到 `setNodes` 之后（或在 `setNodes` 内对新节点应用持久状态）；至少把 notes 改成"当前不读回"。
- **I5 · `proxy_node.go:528` + `main.go:2803`** 预算常量无任何测试覆盖，且 budget≥2 时 `pendingBadFp` 被覆盖 ⇒ 只有最后一个失败节点被确认（实测 A/B 逃逸）。**怎么修**：加一条「budget=2 ⇒ 至少断言 attempt 数/被 mark 的节点」或直接把"预算必须为 1"写成 `const` 注释 + 源码断言；并在 block(3) 里对 `pendingBadFp != ""` 时**不覆盖**（或另存列表），让未来升预算不会静默丢节点。
- **I6 · `main.go:294-298`（ocClientVersion 取 `ocInitMu`）+ `:363-374`** 新增的每-attempt 锁点会与"持锁跨 5s 超时网络调用"的 `refreshOCSession` 相互阻塞（配额切节点风暴期间放大既有入口停顿）。**怎么修**（小）：把 `fetchOCVersionDirect()` 移到临界区之外（先取，后加锁赋值）——顺手也修掉请求入口 `initOCSession()` 的既有停顿。

### Minor (Nice to Have)

- **M1 · `quota.go:124-131`** 早退只有反例测试（`TestFT_RegionClassifierNotHijacked`），缺"真区域限制仍命中"的正例；正例内容见 §6 表。
- **M2 · 事件细节** `freetier_confirm_suppressed` 缺 `available_before`/`marks_suppressed`（v2 §4.2 要求），与 T3 的 `freetier_shape_regression` 无法对账。
- **M3 · `proxy_node.go:545-555`** `availableCount()` 不清扫到期 exhausted（`hasEligibleNode()` 会扫）⇒ 语义不一致；T3 接真实 floor 后建议对齐。
- **M4 · `zz_freetier_confirm_test.go:364`** `freetier_node_confirmed_bad` 这条 needle 因 `&& !strings.Contains(needle,"confirmed_bad")` 而**永不断言**（该串在 `main.go` 出现 4 次，写死 ==2 会失败）⇒ 死代码/误导；建议删掉该 needle 或改成"≥2 次且分布在两个 2xx 块内"。
- **M5 · `zz_freetier_confirm_test.go:176-199`** `TestFT_Mutant_FailurePathMarkDetected` 是"测试的测试"（不经生产路径）；建议注释里降级其地位（真正的 teeth 是 MUT-B2/B3 式的移动变异，或直接把它换成 FormRegression 的失败路径变体）。

---

## 11. 最小修法（若 Lead 只想动最小改动面）

1. **I3**（前置）：`mark()` 内对非 freetier reason 复位 `GateState`，并把 gate_state 的置位并入同一次加锁（1 处，~4 行）。
2. **I4**：把 `loadState()` 的调用点移到节点池有节点之后（或让 `setNodes` 应用持久状态）；至少更正 notes 的表述。
3. **I2**：`TestFT_BothLoopsPatched` 的位置检查改成真块归属，或删除并注明由 E2E 兜底。
4. **I5**：给预算加一条可红的断言（或把 `pendingBadFp` 改成不覆盖）。
5. **I1**：在 notes/§5 里显式申报"C3 豁免当前不生效、`gate_state` 只写不读、flip 前必先修 I3"。
6. （可选，1 行）**I6**：把 `fetchOCVersionDirect()` 移出 `ocInitMu` 临界区。

---

## 12. Assessment

**Ready to submit? Yes（go）** —— 在默认配置（预算 1、豁免开关 false）下，本改动是**安全的净收益**：换节点自愈会真实触发，mark 只在「换节点后成功」发生，形态回归下零标记（变异测试证明这条断言有牙，非纸面绿），两处循环对称、并发路径干净、无新竞态、无死锁、真区域限制未被误杀。
**但不要在同一次提交里打开 `gateReviveRequiresProbe`**：置 true 前必须先修 I3（gate_state 陈旧泄漏），否则非 FreeTier 原因的 exhaustion 可能被永久钉住；并同时更正 I4 的持久化表述。
**Reasoning**：实现质量高于我评审 v1 时的预期——R1 的 3 个 blocker 与 8 个 Important 全部真落地并被我的独立变异/并发/正对照测试证实；剩下的 6 条 Important 里 4 条是"申报不足/测试缺口/既有缺陷继承"（I1/I2/I4/I5），2 条是新增代码的边界问题（I3 一个 4 行修法、I6 一行可缓解），都不构成回滚理由。**Blocker 数：0。**

---

## 13. 收尾

```bash
$ rm -rf /tmp/t7mut /tmp/t7gotmp /tmp/t7_*.go /tmp/t7_*.txt /tmp/t7_task8.*.bak
$ cd /home/jingyx/project/opencode2api-test && git status --short
 M ROADMAP.md          # 队友 T2 的既有改动（与 T6 无关）
 M main.go ; M proxy_node.go ; M quota.go      # 被验对象（三份，未被我改动）
?? .research/ ; ?? docs/…                      # 既有未跟踪
$ sha256sum task8_bucket_query_test.go
fadbdb3e29a245877420bb6432d8c0b0b048bc77edb9b9294058755eee8effc4   # 与任务书给的一致
$ git diff --stat
 main.go | 133 ++…  proxy_node.go | 89 ++…  quota.go | 7 ++     # 与开始时逐字一致
```
**如何还原变异测试**：无需还原——变异只发生在 `/tmp/t7mut` 的副本上（工作区文件从未被写），副本目录已 `rm -rf`；工作区唯一临时动作（移开坏文件）已按 sha256 还原并核对。
