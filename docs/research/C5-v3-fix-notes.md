# C-5 v3 修复说明（task-17）

- 实现方：`impl-ip1`（共享任务板 task-17，缩范围版：只做修复 + 核心证据；变异电池/最终复验归 task-18）
- 被测对象：`node_alert.go`（唯一改动的源文件）
- 基线：C-5 v2 被独立验收判 **no-go** —— `docs/research/C5-v2-verification.md`（§4.1–4.4）
- 验收方测试文件 `zz_c5v2_verify_engine_test.go` / `_replay_test.go` / `_wiring_test.go`：**只读只跑，未修改**
- 我自己新增的测试：`zz_c5v3_fix_test.go`（8 条，每条修复至少一条断言直接钉住它）
- 工作目录：`/home/jingyx/project/opencode2api-test`，分支 `test`；日期：2026-10-19

## 0. 结论（先给结论）

| 验收项 | 结果 |
| --- | --- |
| 三条缺陷探针（`MV2_DEFECT_PROBES=1`） | **D1/D2/D3 全部 PASS（不再是 SKIP）** |
| `go test -run 'TestMV2_'` | **全绿（19 PASS + 3 探针 PASS）** |
| 承诺必报的零成功段漏报 | **9/168 → 0/168（168/168 = 100%）** |
| 事故窗 00:00–09:00 Active 覆盖 | **474/540 = 87.8%（与 v2 持平，未退化）** |
| 稀疏段 00:16:13–00:33:42 | **17/17 = 100%（未退化）** |
| 健康时段误报率（≥20 次口径） | **87/23000 = 0.38% → 93/23000 = 0.40%**（★ 唯一未达成的阈值，见 §6.3） |
| 报→清→报闪烁 | **32 次 → 2 次** |
| 全量 `go test -count=1 ./` + `-race` | **均绿**（49.709s / 16.074s，见 §6.4） |

## 1. 改了什么（四条缺陷 → 规定修法 → 落点）

| 缺陷 | 规定修法 | 落点（`node_alert.go`） |
| --- | --- | --- |
| **D3** 承诺必报的零成功段被整体漏报 | 已结束段只在「新段更大 / 已存段已过期 / 还没存过」时覆盖 | 新增 `rememberEndedRunLocked`；`recordNodeAttempt` 成功分支改为调用它 |
| **D1** ≥100 连败后横幅永不恢复 | burst 分支补 `!stale` | `evaluate` 判据：`case runN >= nodeAlertRunBurst && !stale:` |
| **D2** 恢复清除后旧段把告警拉回 | 真正清除的那一刻把 `lastRun*` 一并归零 | `evaluate` 恢复分支：清 `lastRunFailures/lastRunStart/lastRunEnd` |
| **D4** 段跨度用墙钟 | 跨度按样本自身：进行中 `runLastAt-runStart`，已结束 `lastRunEnd-lastRunStart` | `judgedRun`（含「未来样本不得冒充跨度」的 clamp） |

另外：`node_alert.go` **文件头的设计说明已同步更新**（新增 v3 段：四条缺陷的成因与修法、跨度口径、以及 §5.1 披露的取舍）——任务书要求这份注释必须跟着改。

## 2. D3：`rememberEndedRunLocked`（漏报 9 段 → 0 段）

```go
func (m *nodeAlertMonitor) rememberEndedRunLocked(now time.Time) {
	if m.runFailures <= 0 {
		return
	}
	if m.runFailures > m.lastRunFailures || m.lastRunEnd.IsZero() || now.Sub(m.lastRunEnd) > nodeAlertWindow {
		m.lastRunFailures, m.lastRunStart, m.lastRunEnd = m.runFailures, m.runStart, m.runLastAt
	}
}
```

三个条件都要，缺一不可：① **更大**（大段是更强的故障证据，后随的 1/2/3/11 次小段不该挤掉它 —— 这就是 9 段漏报的根因）；② **还没存过**（否则第一段永远进不来）；③ **已过期**（否则一个很老的大段永久占位，后面的小段永远进不来）。控制组测试 `TestC5v3_D3_FirstEndedRunAlwaysRecorded` / `TestC5v3_D3_ExpiredEndedRunCanBeReplaced` 分别钉住 ② 与 ③。

**核心证据（探针，修复前红 → 修复后绿）**：

```console
# 修复前
--- FAIL: TestMV2_DefectD3_PromisedRunLostBetweenTicks
    逐拍：…[5 13:01 - att=138 succ=120 runN=18 runMin=2.8][6 13:02 - att=143 succ=122 runN=1 runMin=0.0][7 13:03 - att=123 succ=102 runN=1 runMin=0.0]
    D3 复现：承诺必须报的段（20 连败、段自身跨度 3m10s）被整体漏掉；末拍 rate=0.829 runN=1 reason=""

# 修复后
--- PASS: TestMV2_DefectD3_PromisedRunLostBetweenTicks
    逐拍：…[5 13:01 - att=138 succ=120 runN=18 runMin=2.8][6 13:02 A! att=143 succ=122 runN=20 runMin=3.2][7 13:03 - att=123 succ=102 runN=0 runMin=0.0]
```

**生产回放（真数据，`/tmp/prod_node_events.tsv`，224,425 条）**：

```console
⑥b 设计承诺必须报的段（n>=100 或 跨度>=3min）：报出 168/168 = 100.0%   # v2 = 159/168 = 94.6%（漏 9 段）
```

## 3. D1：burst 分支补 `!stale`

```go
case runN >= nodeAlertRunBurst && !stale:
	// 否则 ≥100 连败后 6 小时零尝试：stale 兜底清一拍、下一拍 burst 又拉回来 ⇒ 永久复亮
```

**核心证据（探针）**：

```console
# 修复前：兜底后仍 30/60 分钟 Active，6.8 小时 120 次翻转 → FAIL
# 修复后：
    第 361 分钟 Active -> false（reason="" runN=150）
    D1：6 小时兜底后 1 小时内 Active 分钟数 = 0/60，状态翻转 1 次
--- PASS: TestMV2_DefectD1_BurstThenSilenceNeverStaysClear
```

我的 `TestC5v3_D1_BurstThenSilenceStaysCleared` 更严：从第 2 分钟起逐分钟检查，要求清除之后到第 480 分钟**一次都不许再亮**（并断言清除发生在 ~361 分钟）。

## 4. D2：清除时丢弃已结束段

```go
if m.active && recovered {
	slog.Info("node_layer_alert_cleared", …)
	// ★ 恢复证据已经推翻了那段旧失败数据 ⇒ 连同「已结束段」一起丢弃
	m.lastRunFailures, m.lastRunStart, m.lastRunEnd = 0, time.Time{}, time.Time{}
	m.active, m.reason, m.since = false, "", time.Time{}
	return false, ""
}
```

**核心证据（探针）**：

```console
# 修复前：4:ACTIVE 5:ACTIVE 6:clear 7:ACTIVE(FIRE) 8:clear 9:ACTIVE(FIRE) … 14:clear（翻转 10 次、ERROR 5 次）→ FAIL
# 修复后：
    D2 状态序列：4:ACTIVE 5:ACTIVE 6:clear 7:clear 8:clear 9:clear 10:clear 11:clear 12:clear 13:clear 14:clear
    D2：14 分钟内状态翻转 2 次，ERROR 触发 1 次
--- PASS: TestMV2_DefectD2_RecoveryFlapsWithEndedRun
```

生产回放：闪烁循环 **32 次 → 2 次**；全部翻转 417 → 355，ERROR 272 → 226。

## 5. D4：跨度一律按样本自身（附一处**披露过的取舍**）

```go
n, start, end := m.runFailures, m.runStart, m.runLastAt   // ← 不再是 now
if m.lastRunFailures > n && … { n, start, end = m.lastRunFailures, m.lastRunStart, m.lastRunEnd }
if end.After(now) { end = now }   // 未来样本（时钟回拨）不得冒充跨度
```

- 进行中的段：`runLastAt - runStart`；已结束的段：`lastRunEnd - lastRunStart`；`now` 只用于 stale 与冷却。
- ★ 额外补的 **clamp** 是必需的：D4 改完后 `TestMV2_ClockRollbackAndFutureSamples` 立刻变红 —— 时钟回拨时「最后一次失败」在未来，样本自身跨度会把 2h0m24s 的伪造跨度算进去（实测 `run_minutes=120.4`、并触发告警）。clamp 后该测试恢复绿，且回拨场景 `run_minutes=0`。

**核心证据**：

```console
# 生产里被写成「跨 14h18m45s」的那一段（43 连败其实只在 31.885 秒内打完）
D4 转绿：样本自身跨度 = 0.5314 分钟（31.884999972s）；
        +3 分钟墙上时间下 run_minutes=0.5314，+14h18m45s 后仍是 0.5314
# 文案联动（D3 段）：节点出口自 12:58:10 起已连续失败 20 次、跨 3m10s，期间零成功
```

### 5.1 ★ 一处刻意保留：A/A' 的最短跨度仍按「窗口内最早样本 → now」

任务书要求「A/A' 的最短跨度也改成窗口内失败样本自身的最早→最晚」。**我没有改这一条**，原因是它与验收方的只读测试直接冲突：

- `zz_c5v2_verify_engine_test.go:286` `TestMV2_MinSpanGateStillHolds` 断言：15 次失败/2.8 秒，**第 3 拍必须报**（理由写的是「窗口跨度满 3 分钟、窗口 15 次零成功」），并在日志里点名"这是「窗口跨度用 now-oldest 而非样本自身跨度」的后果"；
- 若把 A/A' 改成失败样本自身跨度，2.8s < 3min ⇒ 第 3 拍不报 ⇒ **该测试变红**，而它**不可修改**（验收标准 #2 又要求 `TestMV2_` 全绿）。

因此我保留 A/A' 的窗口跨度（它表达的是「该窗口已观察满 3 分钟且零成功」），并把它显式固化成 `TestC5v3_D4_RateCriteriaStillUseWindowSpan_Disclosed`，同时在 `node_alert.go` 文件头写明。**代价如实报出**：回放里「刻意不判的短促爆发仍报出」仍是 **66/111**（与 v2 相同；D4 只消掉了走 B 墙钟跨度的那些）。

如果 Lead/验收方认为必须把 66 压下去，改法是一行：把 A/A' 的 `now.Sub(oldest)` 换成「窗口内失败样本的最早→最晚」（buckets 需补 failure 首末时刻），**但 task-18 必须先改 `TestMV2_MinSpanGateStillHolds` 的期望**，否则二者不可同时成立。

## 6. 验收证据（原始输出）

### 6.1 三条探针：PASS（不是 SKIP）

```console
$ MV2_DEFECT_PROBES=1 go test -run 'TestMV2_Defect' -count=1 ./ 
ok  	github.com/shiyanYX/opencode2api	0.009s
# -v 明细：D1 PASS（0/60、翻转 1）、D2 PASS（翻转 2、ERROR 1）、D3 PASS（13:02 A! runN=20 runMin=3.2）
```

修复前同一命令：`FAIL`（D1/D2/D3 三条全红，见 §2–§4 的对照输出）。

### 6.2 `TestMV2_` 全绿（含 3 个探针由 SKIP → PASS）

```console
$ go test -run 'TestMV2_' -count=1 ./ 
ok  	github.com/shiyanYX/opencode2api	2.477s
```

### 6.3 回放三指标 + 承诺段召回

```console
$ go test -run 'TestMV2_ReplayProductionData' -count=1 -v ./
① 事故窗 00:00–09:00 Active 覆盖率 = 474/540 = 87.8%
② 稀疏段 00:16:13–00:33:42 Active 覆盖率 = 17/17 = 100.0%（v1 = 0/17）
③ 健康时段误报率（≥20 次尝试） = 93/23000 = 0.40%      ← v2 = 87/23000 = 0.38%
③ 健康时段误报率（≥1 次尝试）  = 609/27597 = 2.21%     ← v2 = 603/27597 = 2.19%
④ 低成功率时段覆盖率 = 239/242 = 98.8%
⑤ 全部 95646 拍：Active 10347 拍、状态翻转 355 次、ERROR 226 次   ← v2 = 417 / 272
⑥b 设计承诺必须报的段：报出 168/168 = 100.0%                      ← v2 = 159/168 = 94.6%（漏 9）
⑥b 刻意不判的短促爆发：111 段，其中仍报出 66 段                   ← 与 v2 相同（见 §5.1）
⑦ 闪烁（报→清→报）共 2 次                                        ← v2 = 32
```

★ **③ 是唯一未达成的验收阈值**：0.40% > 0.38%（+6 个采样分钟，+0.026pp）。
根因是 D3 的召回修复本身：新报出的 9 段真实故障（20–40 连败、3m8s–4m0s）里，有 6 个「段结束后第一拍」在回放口径下被归进"健康时段"（该口径是「近 60 分钟成功率 ≥90%」，而一次 40 连败的零成功段之后 60 分钟成功率仍然很高）。
**这两件事在现有判据下互斥**：要 168/168 就必然多这 6 个采样分钟；要 ≤0.38% 就得放弃那 9 段召回（回到 v2 的 94.6%）。我没有为了一个数字去牺牲漏报 —— 请 Lead/任务-18 裁决是否接受 0.40%。

### 6.4 全量 + `-race`

```console
$ MV2_DEFECT_PROBES=1 go test -run 'TestMV2_|TestC5v3_|TestNodeAlert|TestVRepro|TestVReplay|TestReplayProduction' -count=1 ./
ok  	github.com/shiyanYX/opencode2api	1.955s
$ MV2_DEFECT_PROBES=1 go test -race -run 'TestMV2_|TestC5v3_|TestNodeAlert|TestVRepro|TestVReplay|TestReplayProduction' -count=1 ./
ok  	github.com/shiyanYX/opencode2api	16.074s          # -race 无报告
$ go test -count=1 ./                                    # 全量（task8_bucket_query_test.go 保持移开）
ok  	github.com/shiyanYX/opencode2api	49.709s
```

## 7. 我自己新增的测试（每条修复至少一条断言）

```console
$ go test -run 'TestC5v3_' -count=1 -v ./
--- PASS: TestC5v3_D3_EndedRunNotOverwrittenBySmallRun         # D3：大段不得被小段挤掉 + 下一拍必报
--- PASS: TestC5v3_D3_FirstEndedRunAlwaysRecorded              # D3 条件②
--- PASS: TestC5v3_D3_ExpiredEndedRunCanBeReplaced             # D3 条件③
--- PASS: TestC5v3_D1_BurstThenSilenceStaysCleared             # D1：清除后到 480 分钟一次不亮
--- PASS: TestC5v3_D2_ClearDiscardsEndedRun                    # D2：清除时 lastRun* 归零 + 不再自亮
--- PASS: TestC5v3_D4_InProgressSpanNeverInflatesWithWallClock # D4：31.885s 段不得长成 14h
--- PASS: TestC5v3_D4_FutureSamplesDoNotFakeSpan               # D4 时钟边界：回拨后 run_minutes=0
--- PASS: TestC5v3_D4_RateCriteriaStillUseWindowSpan_Disclosed # §5.1 的披露项
```

注：我第一版 `TestC5v3_D2_*` 把事件一次性喂完再逐拍评估（时钟先走到 5:19 再退回 0 分钟），触发了自己的 bug —— 已改成与验收方同形的「拍前喂 dt<=k」时序。这条弯路写在这里，避免复验时误读。

## 8. 不该动的文件与状态

- `main.go` / `health_runtime.go` / `static/admin.html`：**未改**（`git status --porcelain` 见 §9）。
- 验收方三个 `zz_c5v2_verify_*.go`：**未改**。
- `task8_bucket_query_test.go`：按任务书要求**保持移开**（字节副本 `/tmp/t17_task8_hold.go`、`/tmp/t8_hold.go`，sha256 均为 `fadbdb3e…`）。
- 未接触生产容器、未使用 8000 端口。

## 9. sha256 记录

```console
$ sha256sum node_alert.go
# pristine（本任务开工前，与 v2 验收方核对过一致）
bb22117220553056d6ebe93efbc26bba63fd73ce6c5c5077aa1458c6dfbe5d51  node_alert.go
# 修复后
b0783b4ae8b8292a34ccf0dde662588f533b64d306fe19215d009379b2da7e30  node_alert.go
$ sha256sum zz_c5v3_fix_test.go
cc96a5011849ea90d744a881ffc148a509f4a73c64643a797ed3cd7d3dcad61e  zz_c5v3_fix_test.go
$ git status --porcelain
 M ROADMAP.md          # Lead 的文档改动，与本任务无关
 M node_alert.go       # ★ 本任务唯一改动的源文件
 M static/admin.html   # F7 验收方（task-14）的改动，与本任务无关
?? docs/research/C5-v3-fix-notes.md   # 本报告
# 我的测试文件 zz_c5v3_fix_test.go 被 .gitignore 排除，不在 porcelain 里
```

`gofmt -l node_alert.go zz_c5v3_fix_test.go` → 无输出；`go build ./...` → exit 0；`go vet .` → 无输出。
