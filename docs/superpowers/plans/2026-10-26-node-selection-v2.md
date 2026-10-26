# 节点打分纠偏 + 风暴遏制 + 超时分层 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: subagent-driven-development。步骤用 `- [ ]` 跟踪。
> 用户已选定方案 A（2026-10-26）。

**Goal:** 让「节点分」先说真话，再让「被迫换节点」这一刻选得对；同时把 403 风暴关在笼子里、把最差等待从 171s 砍到秒级。

**Architecture:** 三段式，互不耦合：① 打分只负责「估得准」（修 blip / 探测生命周期 / 403 归因，并输出正交信号）；② 选择只负责「被迫换时选得对」（订阅级 403 熔断、池级 halt、换组、优先近期成功）；③ 传输层只负责「别干等」（建连硬超时、首字节 deadline）。分数**不**做加权随机、**不**做触发器。

**Tech Stack:** Go 1.x（标准库为主）、现有 `node_scoring.go` / `proxy_node.go` / `bun_egress.go` / `idle_read.go`、`go test`。

**Spec:** `docs/research/node-scoring-spec.md`（§1.2 不做触发器、§3.4 失败分类、§3.6 冷启动）+ `.research/scoring2/{01-code-audit,02-constraints,03-empirical,04-prior-art}.md`（本轮实证，**数字以此为准**）。

## Global Constraints

- 429（`FreeUsageLimitError` / `quota_halt`）**永不计入节点质量**；403 只有在「单订阅集中」时才算节点错。
- `pick(false)` 的粘滞语义**不得改变**：无失败不换。
- 分数**不得**成为选择权重、不得做定时重评、不得做触发器（spec §1.2）。
- 不碰 `static/*`、不碰生产容器、不部署、不 push、不合并 main。
- **禁止 git commit**：所有改动留在工作区（可以 `git add` 记录意图，但不提交）。只有用户在验收通过后明确同意，才由 Lead 统一提交。
- **测试文件仅本地保留**（仓库公约：`.gitignore:26` 忽略 `*_test.go`）：禁止 `git add -f`，测试文件永不入库。
- 测试必须真跑逻辑；禁止只断言常量的空测试。每个任务 ≥4 个变异测试（把实现改坏后测试必须变红）。
- 验证命令：`export PATH=$PATH:/usr/local/go/bin GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"; go test -count=1 ./... && go vet ./... && gofmt -l .`（gofmt 输出必须为空）。
- 同一文件同一时刻只允许一个 owner；需要改别人文件时先报告 Lead，不自行编辑。

## Review Focus

1. 池级抖动窗口内的失败**不能再被算成成功**（0/1031 的节点必须掉到 50 分以下）。
2. 探测恢复后节点必须在 1 小时内能回到 70 分以上（不能被 24h 封顶锁死）。
3. 多订阅同时 403（形态级）不得把责任摊到每个节点；单订阅 403 必须算节点错。
4. 403 熔断**不得**在「全节点都 403」时把订阅全禁掉导致池空；必须有池级 halt 兜底且 fail-fast。
5. 换节点时**不得**选同 /24 的兄弟节点；`pick(false)` 必须仍然粘滞。
6. 建连超时收紧后，**不得**误杀 ttft 正常但建连偏慢的成功请求（需给实测比例）。

---

### Task 1: 打分纠偏 + 正交信号

**Files:**
- Modify: `node_scoring.go`
- Test: `node_scoring_test.go`（新增用例）

**Interfaces:**
- Produces（JSON 新字段，`nodeScoreView`）: `reach float64`、`latency_score float64`、`quota_state string`（`ok`|`cooling`）、`last_ok_age_s int`（无成功=-1）、`auto_eligible bool`。旧字段全部保留。
- Produces（内部）: `var scoring403PoolWindow = 10 * time.Minute`、`var scoring403PoolSubs = 2`、`var scoringProbeFailRamp = 2 * time.Hour`、`var scoringProbeCapWindow = 30 * time.Minute`。

- [ ] **Step 1: 写红测试**（`node_scoring_test.go`）
  - `TestBlipFailureNeverCountsAsSuccess`：构造 1031 次失败全部落在 blip 窗口（池失败率≥50%），断言 `score < 50` 且 `EffS == 0`。
  - `TestProbeRecoveryWithinHour`：节点质量 0.95，`ProbeFail24h=120`、最近一次探测失败在 60 分钟前、之后连续 2 次成功，断言 `score > 70` 且 `Caps` 不含 `probe_24h≤65`。
  - `TestPoolLevel403NotBlamed`：同一 10 分钟窗口内两个订阅各有 403，断言两节点 `EffN` 不增（或增 0）且 `reasons` 含 `pool_403`。
  - `TestSingleSub403Blamed`：仅一个订阅 403，断言该节点 `Fail.Cred` +1 且 `EffN` 增加。
  - `TestProbePenaltyDecays`：探测失败后 0/30/60/120 分钟四点的 `probe` 分项单调回升。
  - `TestOrthogonalFields`：断言五个新字段存在且 `quota_state` 在 exhausted 时为 `cooling`。
- [ ] **Step 2: 跑红** `go test -run 'TestBlip|TestProbe|TestPoolLevel403|TestSingleSub403|TestOrthogonal' -count=1 ./` ⇒ FAIL
- [ ] **Step 3: 实现**
  - blip 分支（`node_scoring.go:776` 附近）：失败在 blip 内改为 `EffN += 0.2`、`EffS += 0`（当前是把它记成 0.8 成功）。
  - 探测：新增 `probePenalty(lastProbeAt, lastProbeFailAt, now)`：最后一次探测成功 ⇒ 1.0；否则 `min(1, 0.4 + 0.6*elapsed/scoringProbeFailRamp)`。`probe` 分项乘该系数；硬封顶 `score<=65` 只在 `elapsed < scoringProbeCapWindow` 时生效。连续 2 次探测成功清空 `ProbeFail24h`。
  - 403：新增 `scoring403Tracker`（`map[sub]time.Time`，sub = 节点名 `::` 前缀）。事件为 `http 403` 时，若窗口内不同订阅数 ≥ `scoring403PoolSubs` ⇒ 该次不计质量、`reasons` 加 `pool_403`；否则 `Fail.Cred++`、按 1.0 失败计入。
  - 输出五个新字段；`auto_eligible` 从 `json:"-"` 改为输出（值 = `state != dead && state != exhausted && score >= 70 && EffN >= 20`）。
- [ ] **Step 4: 跑绿** `go test -count=1 ./... && go vet ./... && gofmt -l .`
- [ ] **Step 5: 变异自检**：逐个把 blip 改回 0.8 成功、把封顶窗口改回 24h、把 403 阈值改成 1，确认对应测试变红，然后还原。
- [ ] **Step 6: 复算对比**：用生产 `call_log`（只读副本）重算 212 节点，输出「修复前/后」分数表，重点贴 `红杏::日本02`（0/1031）与任意 3 个恢复节点。写进 `docs/research/scoring2/06-task1-report.md`。

---

### Task 2: 403 风暴遏制 + 被迫换节点选得对

**Files:**
- Modify: `proxy_node.go`、`main.go`（仅 403/429 分支与换节点调用点）
- Test: 新增 `proxy_node_quarantine_test.go`（或复用现有测试文件）

**Interfaces:**
- Produces（`nodePool` 内部）: `sub403 map[string]*sub403State`、`poolHaltUntil time.Time`；`func (p *nodePool) quarantineSub(sub string, now time.Time)`；`func (p *nodePool) poolHalted(now time.Time) bool`。
- Produces（日志）: call_log `events[]` 新增 `type:"switch"` 的 `reason` 字段，取值 `switch_after_fail|switch_quota|sub_quarantine|pool_halt`；并新增 `candidates int`。
- Consumes: Task 1 的 `quota_state`（仅展示用，不参与选择）。

- [ ] **Step 1: 写红测试**
  - `TestSubQuarantineAfterThree403`：同一订阅 10 分钟内 3 次已确认 403 ⇒ 该订阅全部节点 `usable=false` 且 `pick` 不返回它们；10 分钟后恢复。
  - `TestQuarantineExponentialBackoff`：连续两轮触发 ⇒ 隔离时长 10→20 分钟，上限 30 分钟。
  - `TestPoolHaltStopsSwitching`：10 分钟内 ≥10 个不同节点 403 ⇒ `poolHalted=true`，换节点调用返回「不再换」且不 mark 节点。
  - `TestAllSubsQuarantinedFailsFast`：全部订阅被隔离 ⇒ 返回错误，**不得**清空/放宽隔离。
  - `TestSwitchSkipsSameSubnet`：候选里同 /24 的兄弟节点被跳过；无其他候选时退化为原行为。
  - `TestSwitchPrefersRecentSuccess`：近期成功且 n≥3 的节点排在同分但无近期成功者之前。
  - `TestStickyUnchanged`：`pick(false)` 在无失败时仍返回当前节点（防回归）。
- [ ] **Step 2: 跑红** `go test -run 'TestSubQuarantine|TestQuarantine|TestPoolHalt|TestAllSubs|TestSwitch|TestSticky' -count=1 ./` ⇒ FAIL
- [ ] **Step 3: 实现**
  - 订阅键 = `strings.SplitN(name, "::", 2)[0]`（无 `::` 时用节点名）。
  - 确认口径沿用现有 confirm-then-mark：只有「本次请求最终成功」时，之前的 403 才算 confirmed；全池 403 不 confirm（保持既有语义），此时走池级 halt。
  - **被迫换节点时必须排除当前节点所属订阅**（有其它候选才排除，无则退化）。2026-10-26 评审裁定：不排除同订阅，403 换节点预算会全花在同样被封的兄弟上，confirmed 永远只有 1 次（10-24 实测 1/4,405），订阅熔断 0 触发。
  - 订阅 strike 必须在所有抑制（halt / shape_latch / pool_floor）判定**之后**才累加；被抑制的请求不记 strike。
  - 熔断：`count>=3 within 10min` ⇒ `quarantineUntil = now + 10min*2^(strikes-1)`，上限 30min；30 分钟无新 403 ⇒ strikes 归零。
  - 池级 halt：窗口内 403 的不同节点数 ≥ `max(8, min(24, ceil(0.35*窗口内被尝试节点数)))` ⇒ `poolHaltUntil = now+10min`；halt 期间 `pick(true)` 返回 nil 并由调用方 fail-fast 返回 502/503，不 mark。
  - 换节点候选：排除当前订阅、排除同 /24（IPv4）/ /64（IPv6，按 `Address` 解析；域名不排除）；排序键 `(recentSuccessWithin1h && n>=3) desc → score desc → 原序`。
  - 埋点：`switch` 的 `reason` / `candidates` 必须是独立 JSON 字段（`calllog.go` 的 event/attempt 结构），不得拼进 `Detail` 字符串。
- [ ] **Step 4: 跑绿** 同 Task 1 Step 4。
- [ ] **Step 5: 变异自检**：把阈值 3 改 1、把 halt 判据改成 100%、把同 /24 排除删掉、把 `pick(false)` 改成轮询，确认对应测试变红，还原。
- [ ] **Step 6: 10-24 回放**：用生产日志回放（只读），必须给出三张表：① 403 总量与订阅分布；② **实现口径**下 confirmed 数、订阅熔断触发次数、被隔离后本可避免的 403 数；③ 池级 halt 触发次数。**口径裁定修正（2026-10-26 评审）**：上一版说「订阅熔断能兜住 10-24」是错的——实测 confirmed 只有 1 次（0.02%），0 触发。修复靠「换节点排除同订阅」让 confirmed 转起来；池级 halt 在 10-24 仍预期 0 触发（只有 4 个不同 403 节点），它只负责形态级风暴。写进 `docs/research/scoring2/07-task2-report.md`。

---

### Task 3: 超时分层（等 Task 2 完成后再开工，文件重叠）

**Files:**
- Modify: `proxy_node.go`（`dialTimeout` 常量/var 区）、`bun_egress.go`、`idle_read.go`、`main.go`（首字节 deadline 调用点）
- Test: 新增 `timeout_tier_test.go`

**Interfaces:**
- Produces: `var connectTimeout = 5 * time.Second`（替代 `defaultDialTimeout=60s` 的生效值，保留旧常量作注释）；`func firstByteDeadline(globalP95 time.Duration) time.Duration` = `max(15s, globalP95*1.5)`。
- Consumes: **全局** ttft p95（不按节点）。依据 `05-latency.md`：上线后节点对 ttft 的 ICC = 0.0%，按节点取 p95 只会引入噪声。

**目标修正（2026-10-26，`05-latency.md` 实证）**：T3 **不是**压 p95 的手段。p95 47.7s 由 mimo 的长生成驱动（completion 截 512 → 18.7s；节点只贡献 +2.2pp 方差；全换 deepseek → 8.4s）。按节点总选最优 ttft 也只 -359ms p50 / -458ms p95。T3 的唯一目标是**给病态等待设上限**：建连 60s→5s、首字节无限等→有界，把最差 171s 砍掉。

- [ ] **Step 1: 先量再定**：用生产日志统计「成功请求的 ttft 分布」与「connect_error 的耗时分布」，给出 5s / 8s 两档的误杀率；若 5s 误杀 >0.5%，用 8s。数字写进报告，不写死在代码注释里。
- [ ] **Step 2: 写红测试**：`TestConnectTimeoutCapsAt5s`（模拟慢 dial，断言 5s 内返回错误）、`TestFirstByteDeadlineUsesGlobalP95`（p95=2s ⇒ 15s；p95=20s ⇒ 30s）、`TestSlowButSuccessfulNotKilled`（ttft 12s 的成功请求在 p95=20s 时不被杀）、`TestIdleReadTimeoutUnchanged`。
- [ ] **Step 3: 跑红** ⇒ FAIL
- [ ] **Step 4: 实现**：建连阶段用 `connectTimeout`；首字节等待用 `firstByteDeadline`；首字节之后的空闲读保持现有 `idle_read.go` 语义不变；总预算语义不变（只在 attempt 之间判预算）。
- [ ] **Step 5: 跑绿** + 变异自检（把 5s 改回 60s、把 deadline 改成固定 120s、把 idle read 改短，对应测试必须红）。
- [ ] **Step 6: 报告** `docs/research/scoring2/08-task3-report.md`，含「最差等待从 171s 降到 X s」的实测（用日志回放估算）。

---

### Task 4: 独立验收（Task 1–3 全部完成后）

**Files:** 只写 `docs/research/scoring2/09-acceptance.md`，不改代码。

- [ ] 独立复算 212 节点分数（自己写脚本，不复用实现里的函数）。
- [ ] 对每个任务跑 ≥5 个变异，必须变红。
- [ ] 用 10-24 日志回放验收：**实现口径**下的 confirmed 数、订阅熔断触发次数与覆盖率（不再用 greedy 的 96.4%）、池级 halt 触发次数、换组率。
- [ ] 用 10-26 日志验收：稳态成功率不下降、p95 不上升、无新增失败类型。
- [ ] 检查 Global Constraints 逐条：429 不计质量、`pick(false)` 粘滞、无加权随机、无定时重评。
- [ ] 给结论：`通过 / 有条件通过 / 不通过`，并列出「未覆盖的风险」。
