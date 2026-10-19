# C-5 v2 独立对抗性验收报告

- 任务：task-13（共享任务板）
- 验收方：`verify-c5v2`（独立于实现者，未使用实现者给的任何数字或测试）
- 被测对象：`node_alert.go`（v2，工作区版本 `sha256=bb22117220553056d6ebe93efbc26bba63fd73ce6c5c5077aa1458c6dfbe5d51`）
- 工作目录：`/home/jingyx/project/opencode2api-test`，分支 `test`
- 日期：2026-10-19

## 结论：**no-go**

v1 的四个阻断项（F1 漏报 / F2 误清 / F4 高密度盲区 / F5 97% 不报）**v2 确实修好了**，我独立复现了全部三项回放指标。但任务点名的对抗面里，「闪烁 flapping」「横幅永不恢复」「漏报」在 v2 上仍然存在真实、可复现的缺陷：

| 编号 | 缺陷 | 性质 | 证据 |
| --- | --- | --- | --- |
| **D3** | 被判据承诺必须报的零成功段（≥20 连败且**段自身跨度**≥3 分钟）可被整体漏掉 | **漏报（阻断）** | 生产回放 168 段里 **9 段（5.4%）** 全程静默；确定性最小复现见 §4.3 |
| **D1** | ≥100 连败之后节点层再无任何尝试时，6 小时兜底只清一拍，下一拍 burst 判据又把告警拉回来 | 横幅永久闪烁/卡死（阻断） | 探针实测：兜底后 1 小时内仍 **30/60 分钟 Active**、6.8 小时 **120 次翻转**（§4.1） |
| **D2** | 真实恢复证据清掉告警后，只要「刚结束的失败段」还在 10 分钟窗口内，下一拍又用旧段把告警拉回来 | 报→清→报 每分钟闪烁 + ERROR 刷屏（阻断） | 探针：14 分钟内 **10 次翻转、5 次 ERROR**；生产回放录到 **32 次闪烁循环**（§4.2、§2.6） |
| **D4** | 段跨距用 `now - runStart`（墙钟）而不是样本自身跨度 | 误报面（非阻断，建议修） | 生产回放：**66/111 段**「刻意不判」的短促爆发仍然报警；告警文案出现「已连续失败 43 次、**跨 14h18m45s**」，而该段真实跨度只有 31 秒（§4.4） |

判 no-go 的理由：任务书把「闪烁（报→清→报）」「横幅永不恢复」「漏报」列为必须对抗的面，D1/D2/D3 分别命中这三条，且 D3 是**生产数据上真实发生的漏报**（9 段中 6 段是 20–40 连败、跨度 3–4 分钟），不是构造出来的边角。三条都给出了最小复现，D3 还验证了修复候选。

---

## 0. 方法与环境

```console
$ export PATH=$PATH:/usr/local/go/bin GOCACHE=$PWD/.gocache GOMODCACHE=$PWD/.modcache GOTMPDIR=$PWD/.gotmp
$ sha256sum node_alert.go
bb22117220553056d6ebe93efbc26bba63fd73ce6c5c5077aa1458c6dfbe5d51  node_alert.go
$ go test -count=1 -run 'TestMV2_' .
ok  	github.com/shiyanYX/opencode2api	1.006s
$ go test -count=1 -race -run 'TestMV2_' .
ok  	github.com/shiyanYX/opencode2api	10.131s
```

- 测试文件（`*_test.go` 按仓库约定被 `.gitignore` 忽略，不进提交）：
  - `zz_c5v2_verify_engine_test.go`（16 个用例 + 3 个缺陷探针）
  - `zz_c5v2_verify_replay_test.go`（生产数据回放）
  - `zz_c5v2_verify_wiring_test.go`（健康循环接线 / 并发 / 静态形状）
- 生产数据：`/tmp/prod_node_events.tsv`，224,425 条，`RFC3339Nano<TAB>0|1`，2026-08-13T17:40:29Z .. 2026-10-19T11:44:38+08:00。
- 回放驱动与生产一致：事件按真实时间喂 `recordNodeAttempt`，**每分钟评估一次**（对应 `health_runtime.go` 的 `NewTicker(time.Minute)` + 每拍 `evaluateNodeAlert()`），池状态固定 `avail=1/total=1`（事故当天面板显示 115 available，池非空），使判据只可能来自流量。
- 缺陷探针默认 `t.Skip`（断言的是**应有行为**，在 v2 上预期变红），用 `MV2_DEFECT_PROBES=1` 运行取证。
- **未改动** `node_alert.go` / `main.go` / `health_runtime.go` / `static/admin.html`；变异电池每次改坏后都还原并核对 sha256（见 §3）。`task8_bucket_query_test.go` 保持移开在 `/tmp/t8_hold.go`。

---

## 1. 独立测试清单（19 PASS + 3 SKIP）

| 测试 | 断言什么 | 对抗面 |
| --- | --- | --- |
| `TestMV2_F1_ClusteredOutageRunAnchor` | 8 连败 + 15 分钟空档 + 15 连败（窗口里只有 30 秒的第二簇）必须用「连败段锚点」报出来；首报 reason 必须说明连败；触发后不得掉出 Active | F1 漏报 |
| `TestMV2_APrime_SparseLowVolumeRate` | 15 次失败/4 分钟（<A 的 20 样本、<B 的 20 连败）只可能靠 A' 成立；第 3 拍首次触发，且那拍窗口样本 ∈[10,20) | A' 稀疏判据 |
| `TestMV2_SampleFloorNineFailuresSilent` | 9 次失败跨 20 分钟全程不报（样本地板） | 少量样本不判 |
| `TestMV2_F4_HighDensityBurstBeforeSpan` | 400 次/分钟必须第 1 拍就报（不等 3 分钟跨度） | F4 高密度盲区 |
| `TestMV2_MinSpanGateStillHolds` | 15 次/2.8 秒在跨度不满 3 分钟前不报 | 最短跨度闸 |
| `TestMV2_F5_NinetySevenPercentFailureAlerts` | 96.7% 失败率 20 分钟全程 Active、只翻 1 次、只 ERROR 1 次 | F5 + 零星成功不清 |
| `TestMV2_F2_SingleSuccessAndRecoveryEvidence` | 单次成功不清（F2）；85% 成功率的恢复流量滚满窗口后必须清（85%<95%，钉住 clearRate=0.80） | F2 误清 / 恢复证据 |
| `TestMV2_HoldsThroughGapThenStaleRecovers` | 25 连败跨 26 分钟空档保持 Active；7 小时无尝试后 stale 兜底清除，且**清除后 5 拍不得自亮** | 跨空档保持 / 永不恢复 |
| `TestMV2_PoolEmptyOutranksHealthyTraffic` | 池空必须报；池空期间健康流量不得清；池恢复+健康窗口必须清 | 判据 C 与证据优先级 |
| `TestMV2_NoPoolNoTrafficSilent` | `total=0` 且无流量时沉默（poolEmpty 的 `total>0` 前置条件） | 池空判据边界 |
| `TestMV2_CooldownSuppressesRepeatKeepsActive` | 35 分钟持续故障只 ERROR 2 次（首报+30 分钟冷却），期间 Active 不掉 | 冷却/日志不刷屏 |
| `TestMV2_SnapshotSameSourceAsReason` | 每一拍把 reason 里的数字回查同一拍 snapshot 字段（attempts/successes/rate/run_failures/run_minutes）；四种文案（A / B / 实时未恢复 / 已结束段）必须都覆盖到 | 快照与判定同源 |
| `TestMV2_ClockRollbackAndFutureSamples` | 未来样本不计窗口、回拨后 run_minutes=0、前跳 7 小时 stale 仍工作 | 时钟回拨 |
| `TestMV2_FutureSampleExcludedFromWindow` | 窗口必须排除 `minute > now` 的桶，且不得据此报警 | 未来样本 |
| `TestMV2_DirectEgressNotCounted` | direct 出口 300 次失败一根样本都不进窗口 | direct 不计入 |
| `TestMV2_ReplayProductionData` | 生产回放：三项指标 + 6 项对抗统计（见 §2） | 综合 |
| `TestMV2_Wiring_HealthTickEvaluatesWithPoolCounts` | `runNodeHealthTick` 真会 evaluate，且把真实池状态带进 snapshot；池变空下一拍必须 Active | 接线 |
| `TestMV2_Race_ConcurrentFeedEvaluateSnapshot` | 3 喂 + 1 评估 + 2 快照 + 1 时钟抖动，`-race` 无报告，快照不变量成立 | 并发 `-race` |
| `TestMV2_Wiring_SourceShape` | 静态：两条上游循环各 1 个喂样本点且带 `nodeFp != ""` 守卫；`/api/nodes` 暴露 snapshot；健康循环两分支都 evaluate；面板渲染 `run_failures` 与「无数据」 | 接线（静态证据） |
| `TestMV2_DefectD1/D2/D3_*` | 缺陷探针（默认 SKIP，`MV2_DEFECT_PROBES=1` 时断言应有行为 → 当前实现变红） | D1/D2/D3 |

---

## 2. 生产回放（真实实现 + 真实数据）

命令：

```console
$ go test -count=1 -run 'TestMV2_ReplayProductionData' -v . > /tmp/mv2_replay.log 2>&1; echo $?
0
$ grep -E "载入|回放完成|①|②|③|④|⑤|⑥|⑦" /tmp/mv2_replay.log
```

真实输出：

```
    zz_c5v2_verify_replay_test.go:139: 载入 /tmp/prod_node_events.tsv：224425 条尝试，2026-08-13T17:40:29Z .. 2026-10-19T11:44:38+08:00
    zz_c5v2_verify_replay_test.go:169: 回放完成：95646 拍（每分钟一拍），喂入 224425 条事件
    zz_c5v2_verify_replay_test.go:184: ① 事故窗 00:00–09:00 Active 覆盖率 = 474/540 = 87.8%
    zz_c5v2_verify_replay_test.go:199: ② 稀疏段 00:16:13–00:33:42 Active 覆盖率 = 17/17 = 100.0%（v1 = 0/17）
    zz_c5v2_verify_replay_test.go:220: ③ 健康时段误报率（≥20 次尝试） = 87/23000 = 0.38%
    zz_c5v2_verify_replay_test.go:221: ③ 健康时段误报率（≥1 次尝试） = 603/27597 = 2.19%
    zz_c5v2_verify_replay_test.go:240: ④ 低成功率时段（60 分钟 ≤5%、≥20 次）Active 覆盖率 = 239/242 = 98.8%
    zz_c5v2_verify_replay_test.go:241: ④b 低成功率时段（实现自己的 10 分钟窗口 ≤5%、≥20 次）Active 覆盖率 = 265/274 = 96.7%
    zz_c5v2_verify_replay_test.go:276: ⑤ 全部 95646 拍：Active 10352 拍（10.8%）、状态翻转 417 次、ERROR 触发 272 次
    zz_c5v2_verify_replay_test.go:277: ⑤ 告警起始 209 次：瞬时爆发(窗口样本自身跨度<=1min)=2，跨空档后再来流量=207，窗口无样本=0
    zz_c5v2_verify_replay_test.go:325: ⑥ 零成功故障段（连续失败 >=20 次，共 279 段）：至少报过一次 184 段（65.9%），段内分钟覆盖率 1611/2607 = 61.8%
    zz_c5v2_verify_replay_test.go:368: ⑥b 设计承诺必须报的段（n>=100 或 跨度>=3min）：报出 159/168 = 94.6%
    zz_c5v2_verify_replay_test.go:369: ⑥b 刻意不判的短促爆发（n<100 且 跨度<3min）：111 段，其中仍报出的 66 段（应为 0 或少）
    zz_c5v2_verify_replay_test.go:416: ⑦ 闪烁（报→清→报，两段间隔均 <=3 分钟）共 32 次；全部翻转 417 次
```

三个必做指标（**量化到分钟，不是触发次数**）：

| 指标 | 结果 | 判定 |
| --- | --- | --- |
| ① 事故窗 2026-10-19 00:00–09:00 CST Active 覆盖率 | **474/540 = 87.8%**（v1 = 5 分钟 = 1.0%） | 通过 |
| ② 稀疏段 00:16:13–00:33:42 覆盖率 | **17/17 = 100%**（v1 = 0%） | 通过 |
| ③ 健康时段（近 60 分钟 ≥20 次尝试且成功率 ≥90%）误报率 | **87/23000 = 0.38%** | 通过（<2%） |
| ③ 同上但门槛放宽到 ≥1 次尝试 | 603/27597 = **2.19%** | 供参考：低流量健康分钟更容易落在 10 分钟坏窗口的尾巴上 |
| ④ 低成功率时段（60 分钟 ≤5%、≥20 次）覆盖率 | 239/242 = **98.8%**（10 分钟口径 96.7%） | 通过 |

三项数字与实现者自报一致（87.8% / 100% / 0.38%），但这是我自己复算的结果，不是引用。

### 2.5 承诺形状的召回：**9 段被漏掉（94.6%）**

设计承诺（`node_alert.go` 注释与 B 判据）：连续失败 ≥20 次且段自身跨度 ≥3 分钟，或 ≥100 次，必须报。按这个形状筛，168 段里 9 段全程静默（`/tmp/mv2_replay.log` 原文）：

```
    ✗ 漏报段 n=28 自身跨度=3m6.166s 起点=2026-09-18 12:56:46；段结束后第一拍: 13:00 lastRunN=1 curRunN=1 curSpan=0s att10=118 rate10=0.68 reason=""
    ✗ 漏报段 n=40 自身跨度=3m20.738s 起点=2026-09-18 15:40:07；段结束后第一拍: 15:44 lastRunN=1 curRunN=1 curSpan=0s att10=131 rate10=0.64 reason=""
    ✗ 漏报段 n=24 自身跨度=4m0.452s 起点=2026-09-19 19:31:48；段结束后第一拍: 19:36 lastRunN=1 curRunN=1 curSpan=2s att10=118 rate10=0.78 reason=""
    ✗ 漏报段 n=31 自身跨度=3m28.895s 起点=2026-09-19 23:30:02；段结束后第一拍: 23:34 lastRunN=3 curRunN=3 curSpan=0s att10=75 rate10=0.47 reason=""
    ✗ 漏报段 n=36 自身跨度=3m22.777s 起点=2026-09-21 14:22:16；段结束后第一拍: 14:26 lastRunN=2 curRunN=2 curSpan=0s att10=167 rate10=0.75 reason=""
    ✗ 漏报段 n=36 自身跨度=3m24.822s 起点=2026-09-21 14:36:21；段结束后第一拍: 14:40 lastRunN=1 curRunN=1 curSpan=11s att10=139 rate10=0.70 reason=""
    ✗ 漏报段 n=20 自身跨度=3m36.756s 起点=2026-09-21 19:03:29；段结束后第一拍: 19:08 lastRunN=11 curRunN=11 curSpan=0s att10=146 rate10=0.64 reason=""
    ✗ 漏报段 n=21 自身跨度=3m2.968s 起点=2026-09-25 07:15:43；段结束后第一拍: 07:19 lastRunN=1 curRunN=1 curSpan=0s att10=71 rate10=0.62 reason=""
    ✗ 漏报段 n=27 自身跨度=3m8s 起点=2026-10-04 10:37:05；段结束后第一拍: 10:41 lastRunN=2 curRunN=2 curSpan=0s att10=101 rate10=0.44 reason=""
```

9 段的签名完全一致：段结束后的第一拍，`lastRun*` 已经是 **1/2/3/11 次**的小段（原本 20–40 次的段被后随的小段覆盖），当前段也是 0–11 次，`att10` 的成功率 0.44–0.78（健康窗口，A/A' 都不成立）⇒ 判据全落空，`reason=""`。根因见 §4.3。

### 2.6 闪烁与起始形态

- 全部 95,646 拍里 Active 10,352 拍（10.8%），状态翻转 **417 次**，ERROR 触发 272 次。
- 209 次告警起始里，窗口内样本自身跨度 ≤1 分钟的「瞬时爆发」只有 2 次，其余 207 次都是「跨空档后再来流量」——说明起始形态总体是合理的。
- 但 **32 次**是「报→清→报，两段间隔都 ≤3 分钟」的闪烁循环，例如：

```
    ⚠ 闪烁 报@09-19 03:48 -> 清@03:51(3min) -> 再报@03:52(1min)
    ⚠ 闪烁 报@09-19 03:52 -> 清@03:53(1min) -> 再报@03:54(1min)
    ⚠ 闪烁 报@09-19 05:31 -> 清@05:32(1min) -> 再报@05:33(1min)
    ⚠ 闪烁 报@09-20 18:15 -> 清@18:18(3min) -> 再报@18:19(1min)
```

这些是 D2 在真实数据上的表现（每次 >20 连败恢复后都会进入这个窗口）。

---

## 3. 变异电池（6 个变异 6 个被抓住，改回后全绿）

方法：备份 pristine `node_alert.go` → 每次只改一处 → 跑目标测试（预期红）→ 还原 → `sha256sum` 核对 → 再跑（预期绿）。原始输出在 `/tmp/mv2_mut_M*.log`（红）与 `*.green.log`（绿）。pristine sha256 = `bb22117220553056…`，**6 次还原后 sha256 全部一致**。

| # | 变异 | diff | 目标测试 | 变异后 | 还原后 |
| --- | --- | --- | --- | --- | --- |
| M1 | 删掉稀疏判据 A'（样本门槛 10→100000） | `-nodeAlertSparseSamples = 10` / `+ = 100000` | `TestMV2_APrime_SparseLowVolumeRate` | **红**：`期望第 3 拍…首次触发，实际 -1；obs=[0 21:28 - att=1…][3 21:31 - att=12 succ=0 runN=12]` | 绿 |
| M2 | 恢复证据退化为「1 次成功即清」（clearStreak 20→1） | `-nodeAlertClearStreak = 20` / `+ = 1` | `TestMV2_F5_…\|TestMV2_F2_…` | **红**：`一次成功就把告警清了（v1 F2 复现）：obs[5]={active:false … rate:0.032}` | 绿 |
| M3 | 删掉最短跨度闸（minSpan 3min→0） | `-nodeAlertMinSpan = 3 * time.Minute` / `+ = 0 * time.Minute` | `TestMV2_MinSpanGateStillHolds` | **红**：`跨度不满 3 分钟不该触发：obs[1]={fire:true … 成功率 0%}` | 绿 |
| M4 | 删掉高密度爆发判据（runBurst 100→100000） | `-nodeAlertRunBurst    = 100` / `+ = 100000` | `TestMV2_F4_HighDensityBurstBeforeSpan` | **红**：`期望第 1 拍…触发，实际第 3 拍；obs=[1 08:15 - att=400 runN=400][3 08:17 A!]` | 绿 |
| M5 | 反转连败段的 stale 条件（`!stale`→`stale`） | `-…&& !stale:` / `+…&& stale:` | `TestMV2_HoldsThroughGapThenStaleRecovers` | **红**：`--- FAIL` | 绿 |
| M6 | 快照与判定不同源（snapshot 不走 `judgedRun`） | `-runN, _, runSpan := m.judgedRun(now)` / `+ = m.runFailures, time.Time{}, time.Duration(0)` | `TestMV2_SnapshotSameSourceAsReason` | **红**：`口径不同源 @14:00:00：reason 说「连续失败 25 次」，snapshot.run_failures=0 … reason 说跨 4m48s，snapshot.run_minutes=0.0000` | 绿 |

说明：M6 证明「reason 与 snapshot 同源」这条测试有牙齿；M2 同时覆盖 F2（单次成功误清）与 97% 场景；M1/M3/M4 就是任务书点名的三条。20 个非探针用例在 pristine 上全绿（含 `-race`），变异只让对应测试变红。

---

## 4. 缺陷（含最小复现）

### 4.1 D1：≥100 连败后节点层彻底没有流量 → 6 小时兜底只清一拍，横幅永久复亮

`node_alert.go:259` 的 burst 分支（`case runN >= nodeAlertRunBurst`）**没有 `!stale` 守卫**，而且 `runFailures` 只在成功时才归零。于是 ≥100 连败之后哪怕 6 小时没有任何尝试，`runFailures` 仍然是 100+：

1. stale 拍：`recovered := … || stale` 为真 → 清掉（`active=false`），返回 false；
2. 下一拍：reason 又被 burst 分支填上（非空），而 `active` 已经是 false → 跳过恢复分支 → 直接 `active=true`、`fire=true`（ERROR 再来一条）；
3. 如此循环，直到来一次成功。

探针输出（`MV2_DEFECT_PROBES=1 go test -count=1 -run TestMV2_DefectD1 -v .`）：

```
    zz_c5v2_verify_engine_test.go:766: D1：6 小时兜底后 1 小时内 Active 分钟数 = 30/60，状态翻转 120 次
    zz_c5v2_verify_engine_test.go:768: D1 复现：stale(6h) 兜底后横幅仍亮着 30/60 分钟（burst 判据不吃 stale，runFailures 永不归零）
```

最小复现：150 连败（20 秒内）→ 之后 8 小时零尝试、每分钟评估一次。期望：超过 6 小时兜底后保持 cleared；实际：`clear/ACTIVE` 每 2 分钟交替一次，最后 1 小时仍有 30 分钟在告警，并持续抛 ERROR。这**推翻了实现者「超过 6 小时完全没有节点尝试（兜底，防止横幅永久卡死）」的设计承诺**。

建议修法（未实施）：burst 分支同样加 `!stale`；或 stale 清除时把 `runFailures/runStart/lastRun*` 一起归零。

### 4.2 D2：真实恢复之后，旧失败段把告警拉回来 → 报→清→报 + ERROR 刷屏

`lastRun*` 记录「刚结束、仍在 10 分钟窗口内」的失败段，`judgedRun` 会优先用它。恢复证据（20 连胜 / 窗口 ≥20 样本且 ≥80%）清掉告警后，`lastRun*` **没有被清**：下一拍 reason 又由旧段成立（`case runN >= nodeAlertRunFailures && runSpan >= nodeAlertMinSpan && !stale`），此时 `active=false` 所以恢复分支不进 → 立刻重新 `active=true` + ERROR；再下一拍恢复证据还在 → 又清；如此每分钟交替，直到旧段超出 10 分钟窗口。

探针输出：

```
    zz_c5v2_verify_engine_test.go:796: D2 状态序列：4:ACTIVE 5:ACTIVE 6:clear 7:ACTIVE(FIRE) 8:clear 9:ACTIVE(FIRE) 10:clear 11:ACTIVE(FIRE) 12:clear 13:ACTIVE(FIRE) 14:clear
    zz_c5v2_verify_engine_test.go:798: D2：14 分钟内状态翻转 10 次，ERROR 触发 5 次
    zz_c5v2_verify_engine_test.go:800: D2 复现：恢复后仍在闪烁（翻转 10 次、ERROR 5 次），期望「清掉后不再自亮」
```

最小复现：25 连败跨 4 分钟（触发告警）→ 紧接着 20 连胜（真实恢复证据）→ 之后每分钟评估。期望：`clear` 后不再自亮；实际：`clear`/`ACTIVE(FIRE)` 交替。生产回放里对应 32 次闪烁循环（§2.6）。

建议修法（未实施）：恢复证据生效、真正清除的那一刻把 `lastRun*` 一起清掉；或让「已结束段」在**存在恢复证据时**不参与判据。

### 4.3 D3：承诺必须报的零成功段整体漏掉（**这是生产数据上的真实漏报**）

机制（三段合起来才成立）：

1. 评估每分钟一次，判据 B 需要**同时**满足「段数 ≥20」和「跨度 ≥3 分钟」。若这两个条件的交点落在两拍之间，段在下一拍之前就结束了；
2. 段结束时若紧跟一次成功，`runFailures` 归零，段被记进 `lastRun*`（本可以救回来）；
3. 但 `lastRun*` 只保留**最近一个**结束的段，段结束后紧跟的一次小失败段（比如 1 次）会把它覆盖掉 ⇒ `judgedRun` 返回 1 次 ⇒ A/A'（窗口里是健康流量，成功率 0.44–0.78）与 B/B' 全部不成立 ⇒ 静默。

确定性最小复现（`TestMV2_DefectD3_PromisedRunLostBetweenTicks`）：背景健康流量 20 次/分钟（分钟 −6..+1），故障段 20 连败从 +2m10s 到 +5m20s（段自身跨度 3m10s），随后 `成功→1 次失败→成功`，每分钟评估一次：

```
    zz_c5v2_verify_engine_test.go:831: 逐拍：[0 12:56 - att=121 succ=121 runN=0 runMin=0.0][1 12:57 …][2 12:58 …][3 12:59 - att=166 succ=160 runN=6 runMin=0.8][4 13:00 - att=152 succ=140 runN=12 runMin=1.8][5 13:01 - att=138 succ=120 runN=18 runMin=2.8][6 13:02 - att=143 succ=122 runN=1 runMin=0.0][7 13:03 - att=123 succ=102 runN=1 runMin=0.0]
    zz_c5v2_verify_engine_test.go:833: D3 复现：承诺必须报的段（20 连败、段自身跨度 3m10s）被整体漏掉；末拍 att=123 succ=102 rate=0.829 runN=1 reason=""
```

注意第 5 拍 `runN=18`（差一次到 20），第 6 拍已经变成 `runN=1`——判据承诺的形状从头到尾没有任何一拍成立。

**修复候选已验证**（只在本地临时应用、随后还原）：把「记录已结束段」的条件从 `if m.runFailures > 0` 改成 `if m.runFailures > m.lastRunFailures`（只被更大的段覆盖）：

```
$ python3 …   # 临时改成 if m.runFailures > m.lastRunFailures
已应用修复候选：lastRun* 只被更大的段覆盖
修复候选下 TestMV2_DefectD3     rc=0 GREEN
还原 node_alert.go: sha256=bb22117220553056 与原始一致
```

（同一次实验里 `TestMV2_` 整体 rc=1 是因为 `MV2_DEFECT_PROBES=1` 把 D1/D2 两个探针也放进来跑了——它们本来就预期红；pristine 上不设该环境变量时全绿，见 §0。）

### 4.4 D4：跨距用墙钟而不是样本自身跨度（误报面，非阻断）

`judgedRun` 对**进行中**的段返回 `span = now - runStart`。于是只要段之后没有新流量，跨度会随时间自己长大：一次 2.8 秒内打完的 15 连败，3 分钟后就会被 A' 报出来（`TestMV2_MinSpanGateStillHolds` 记录了当前行为：`瞬时爆发（15 次/2.8s）在第 3 拍被报出`）；生产回放里 **111 段**「刻意不判」的短促爆发（n<100 且真实跨度<3 分钟）仍有 **66 段**报了警。极端例子（回放起始日志原文）：

```
起始 2026-09-11 09:44 [instant-burst] att10=7 rate10=0.00 样本自身跨度=31.885s 距上一样本=14h15m49.72s runN=43 reason="节点出口自 19:25:15 起已连续失败 43 次、跨 14h18m45s，期间零成功"
```

该告警本身是「43 连败（<100、真实跨度只有 31 秒）+ 14 小时静默 + 再来一次瞬时爆发」拼出来的——按设计意图（短促爆发不判）不该报；而且面板文案「跨 14h18m45s」与事实不符（真实 31 秒）。健康时段误报率 0.38%（≥20 次尝试口径）／2.19%（≥1 次口径）里有一部分就是它贡献的。

建议修法（未实施）：段跨距改用样本自身跨度（`runLastAt - runStart`）；若要覆盖「跨空流量空档」，单独用一个「距上次尝试时长」的判据表达，别让墙钟冒充段跨度。

---

## 5. 剩余风险与我还不确定的地方

1. **回放的池状态是固定 `avail=1/total=1`**：我只验证了流量判据；「池空」判据在真实数据上没有池状态可回放，只用单测覆盖（`TestMV2_PoolEmptyOutranksHealthyTraffic`）。生产上 `poolStateCounts()` 与面板同源，我没有质疑这条接线。
2. **健康时段误报率的分母定义**：0.38% 用的是「近 60 分钟 ≥20 次尝试且成功率 ≥90%」；放宽到 ≥1 次尝试是 2.19%。如果运维把「低流量但仍健康」的分钟也算进误报分子，观感会比 0.38% 差。两种定义我都给了数。
3. **`internal` 与 `js` 侧未做端到端**：面板只做了静态核对（`renderNodeAlert(d.node_alert)`、`run_failures`、null 成功率显示「无数据」），没有真开浏览器；`/api/nodes` 的字段形状我按 `main.go:7113` 的静态事实核对。
4. **长跑漂移**：95,646 拍（67 天）回放里 ERROR 272 次、翻转 417 次，D1/D2 会在这类长跑里放大（D1 是永久性的）。我没有测进程重启/长时间内存，但环形桶 + O(1) 结构没有明显泄漏面。
5. **D3 的 9 段是否都算「该报」**：判定依据是设计注释里的承诺（≥20 连败且段自身跨度 ≥3 分钟）。其中 3 段（20/21/24 连败、跨度 3m0s–4m0s）刚好压线；如果实现者认为「压线段可以不报」，那应改的是注释与 `nodeAlertMinSpan` 的语义，而不是把 6 段 27–40 连败、跨度 3m8s–4m0s 也说成不该报。我按现状文字判：应报未报。
6. **变异 M5 的证据行**：M5 的红是 `--- FAIL` 行（该测试先因 `first<0` 直接 fatal），变红原因与我预期一致（连败段在非 stale 时不再成立），但日志里没有更细的断言文案。

---

## 6. 边界与复现命令

- **未修改**受保护文件：`node_alert.go`（验收前后 sha256 均为 `bb22117220553056…`）、`main.go`、`health_runtime.go`、`static/admin.html`。变异电池的改坏—还原逐次核对了 sha256。
- 未接触生产容器、未使用 8000 端口；跑的是 `go test`（仓库既有测试使用 9031+/9041+，我这一批不监听端口）。
- `task8_bucket_query_test.go` 保持移开状态（`/tmp/t8_hold.go`）。
- 我的测试文件全部匹配 `*_test.go`（仓库 `.gitignore` 既有约定），不会进提交。

复现全部证据：

```console
$ export PATH=$PATH:/usr/local/go/bin GOCACHE=$PWD/.gocache GOMODCACHE=$PWD/.modcache GOTMPDIR=$PWD/.gotmp
$ go test -count=1 -run 'TestMV2_' -v .                       # 19 PASS + 3 SKIP
$ go test -count=1 -race -run 'TestMV2_' .                    # ok 10.131s
$ go test -count=1 -run 'TestMV2_ReplayProductionData' -v .   # 三项指标 + 6 项对抗统计
$ MV2_DEFECT_PROBES=1 go test -count=1 -run 'TestMV2_Defect' -v .   # D1/D2/D3 三条红
$ python3 /tmp/mv2_mutate.py                                  # 变异电池（6/6 抓住，逐次还原核对 sha256）
$ sha256sum node_alert.go                                     # bb22117220553056…
```

---

# 7. v3 复验（task-18，原验收方，独立于实现方）

> 本节按 task-18 追加。上方 §0–§6 的 v2 记录（含 v2 的 **no-go**）保持原样，不改写历史。
> 被测：`node_alert.go` v3（task-17），`sha256=b0783b4ae8b8292a34ccf0dde662588f533b64d306fe19215d009379b2da7e30`，改动范围 = 仅 §4.1–4.4 四条缺陷的修法（`git diff` 全文见 `/tmp/mv3_node_alert.v3.orig.go` vs v2 备份）。
> v2/v3 对比一律用**同一份回放代码、同一份生产数据**，只通过 `-overlay` 换 `node_alert.go`（见 §7.10.1），不是两次不同口径的跑。

## 7.0 结论：**go**（带 3 条记录在案的残余风险）

| task-18 的 go 条件 | 结论 | 证据 |
| --- | --- | --- |
| 三缺陷真修好（探针 PASS 非 SKIP） | ✅ | D1/D2/D3 在 `MV2_DEFECT_PROBES=1` 下 **PASS**（§7.3） |
| D4 如实交代 | ✅ | 文案失真 701/895 → **0/641**；短促爆发 66/111 复算一致（§7.4） |
| 无新增漏报 | ✅ | 承诺必报段 **159/168 → 168/168**；进行中故障分钟 **434/434**；v2→v3 差分 56 个变暗分钟**全部**可归因于 D4 语义/ D2 闪烁残影，无真回归（§7.6） |
| 无新增闪烁 | ✅ | 闪烁 **32 → 2**，翻转 417→355，ERROR 272→226；剩下 2 次成因明确且 67 天 2 次（§7.7） |
| 四指标不退化 | ✅ | ①87.8%（持平）②100%（持平）③原始 0.40% / 剔除口径假象 **0.14%**（v2 0.15%，略优）④100%（§7.6） |
| 变异电池 ≥3 | ✅ | **6 个**，其中 1 个（M11）第一轮**没被抓住**，补了 V3-G 后抓住（§7.9，如实记录） |

残余风险（不阻断 go，但建议排期）：**R1** `successStreak≥20` 这条恢复证据在窗口样本充足（≥20）且成功率 <80% 时也会清告警 —— v3 有 121/177 次清除属此类，其中 11 次 5 分钟内复亮（v2 是 132/208、40 次复亮，v3 已改善）；这与实现注释「只有当窗口样本不足时它才划算」不符。**R2** 60 天回放里有 2 次报→清→报（1–3 分钟），成因即 R1，非新增缺陷。**R3** 09-11 那条「跨 14h18m42s」文案仍在，但它现在等于**真实样本跨度**（已验证：段内最大失败间隔 14h15m14s，即中间确实长期无流量），属 UX 观感而非数字造假。

## 7.1 裁决 1：健康时段误报「双数」+ 我独立确认那几分钟是真实故障分钟

**我先复算，再对照 Lead 的裁决。读数与裁决有出入，如实记录。**

```console
$ MV2_DUMP_CSV=... go test -overlay=<v3> -run TestMV2_ReplayProductionData .   # 逐拍 CSV
$ MV2_DUMP_CSV=... go test -overlay=<v2> -run TestMV2_ReplayProductionData .   # 同一代码跑 v2
```

真实输出（v3 / v2，同一回放）：

```
   ③ 健康时段误报率（≥20 次尝试，原始） = 93/23000 = 0.40%
   ③ 健康时段误报率（≥20 次尝试，剔除真实故障段口径假象 60 分钟） = 33/23000 = 0.14%
   ③ 健康时段误报率（≥1 次尝试） = 609/27597 = 2.21%
   —— v2（overlay 换回 v2 实现，同一测试）——
   ③ 健康时段误报率（≥20 次尝试，原始） = 87/23000 = 0.38%
   ③ 健康时段误报率（≥20 次尝试，剔除真实故障段口径假象 53 分钟） = 34/23000 = 0.15%
   ③ 健康时段误报率（≥1 次尝试） = 603/27597 = 2.19%
```

所以按 task-18 要求，**两个数**都报：

| 数 | v3 | v2 | 说明 |
| --- | --- | --- | --- |
| **原始**（近 60 分钟 ≥20 次且成功率 ≥90% 里的 Active 比例） | **93/23000 = 0.40%** | 87/23000 = 0.38% | 与 Lead/实现方自报的 0.40% 一致 |
| **剔除「口径假象」后** | **33/23000 = 0.14%** | 34/23000 = 0.15% | 剔除 60 分钟落在**真实**零成功段窗口内的拍 |

**独立确认（不采信实现方解释）**：v3 相对 v2 **新增**的误报分钟是 **13 分钟**（93−87 是净值，另有 7 分钟 v2 有 v3 没有），来自 **4 条真实故障段**。我用原始事件流逐条验证（不依赖引擎）：

```
=== 13 个新增误报分钟的原始数据验证（每个段：连败次数/零成功/自身跨度）===
  段 2026-09-18T12:56:46 起：连败 28 次、期间零成功、自身跨度 0:03:06.790000 ⇒ 设计承诺必报=True（v2 漏，v3 报）
  段 2026-09-18T15:40:07 起：连败 40 次、期间零成功、自身跨度 0:03:20.749000 ⇒ 设计承诺必报=True（v2 漏，v3 报）
  段 2026-09-21T14:22:16 起：连败 36 次、期间零成功、自身跨度 0:03:22.921000 ⇒ 设计承诺必报=True（v2 漏，v3 报）
  段 2026-09-21T18:13:23 起：连败 47 次、期间零成功、自身跨度 0:03:31.551000 ⇒ 设计承诺必报=True（v2 漏，v3 报）
```

这 4 条正是 v2 §2.5 里那 9 个漏报段中的 4 个（其余 5 段的报警落在不属该 60 分钟口径的拍上）。**判定：裁决 1 成立**（那几分钟确实是真实故障分钟，是 60 分钟 ≥90% 口径把它们收成了「健康」）——但注意数字对齐：Lead 说的「6 分钟」是净值 93−87，实际新增误报分钟是 **13**、落进口径的假象总量是 **60**；两个口径我都列在上面。**不因 0.40% > 0.38% 判 no-go**；而且剔除假象后 v3 的 0.14% 比 v2 的 0.15% 还略好。

## 7.2 裁决 2：A/A' 最短跨度维持「窗口跨度」 —— 我**接受**这条限制

**明确表态：接受，不改 `TestMV2_MinSpanGateStillHolds`（:286）的期望。**

理由（三条，都是我自己的数据）：
1. 那条测试是我在 v2 轮写下的**不可放宽要求**：`15 次失败 / 2.8 秒 ⇒ 第 3 拍必须报`。它守的是「密集爆发不许被静默忽略」——C-6 已证明单轮抖动能让整池集体超时，如果改成样本自身跨度，这种 3 秒打完全池的爆发就永远不会被告警，等于把 F4 的另一半又修回去。
2. 「仍会报」的 66/111 里，绝大多数发生在**层正在高频失败**的时段，不是健康时段误报：控制指标 ③ 的线是「近 60 分钟 ≥90%」，而这条线的原始误报是 0.40% / 剔除假象后 0.14%（§7.1）。把 66 段算作「误报」是把两个不同口径混在一起。
3. 代价可控且已被指标钉住：⑥b 的 111/66 是本节的固定体检项，一旦未来改坏会立刻可见。

所以：**A/A' 用窗口跨度 + B/B' 用样本自身跨度**，我认为是正确的非对称设计（前者回答「这个窗口观察满 3 分钟且几乎零成功吗」，后者回答「这一段连续失败持续了多久」），不要求统一。

## 7.3 三条缺陷探针：v2 = FAIL → v3 = **PASS（不是 SKIP）**

```console
$ MV2_DEFECT_PROBES=1 go test -count=1 -run 'TestMV2_Defect' -v .
```

真实输出（逐条）：

```
    zz_c5v2_verify_engine_test.go:766: D1：6 小时兜底后 1 小时内 Active 分钟数 = 0/60，状态翻转 1 次
--- PASS: TestMV2_DefectD1_BurstThenSilenceNeverStaysClear (0.00s)
    zz_c5v2_verify_engine_test.go:796: D2 状态序列：4:ACTIVE 5:ACTIVE 6:clear 7:clear 8:clear 9:clear 10:clear 11:clear 12:clear 13:clear 14:clear
    zz_c5v2_verify_engine_test.go:798: D2：14 分钟内状态翻转 2 次，ERROR 触发 1 次
--- PASS: TestMV2_DefectD2_RecoveryFlapsWithEndedRun (0.00s)
    zz_c5v2_verify_engine_test.go:831: 逐拍：[0 12:56 att=121 succ=121 runN=0 …][5 13:01 - att=138 succ=120 runN=18 runMin=2.8][6 13:02 A! att=143 succ=122 runN=20 runMin=3.2][7 13:03 - att=123 succ=102 runN=0 runMin=0.0]
--- PASS: TestMV2_DefectD3_PromisedRunLostBetweenTicks (0.00s)
--- PASS: TestMV2_DefectD1_BurstThenSilenceNeverStaysClear (0.00s)
--- PASS: TestMV2_DefectD2_RecoveryFlapsWithEndedRun (0.00s)
--- PASS: TestMV2_DefectD3_PromisedRunLostBetweenTicks (0.00s)
```

- **D1**（≥100 连败 + 零流量 → 6 小时兜底失效）：v2 是 `30/60 分钟仍 Active、6.8h 翻转 120 次`，v3 是 **`0/60`、翻转 1 次**。`runFailures` 仍留在 150，但 stale 后 burst 分支不再成立 ⇒ 不再复亮。
- **D2**（恢复后旧段把告警拉回来）：状态序列从第 6 拍起一路 `clear`（v2 是 `7:ACTIVE(FIRE) 8:clear 9:ACTIVE(FIRE)…`），翻转 10→**2**、ERROR 5→**1**（那 1 次是首报）。
- **D3**（承诺必报的段被小段挤掉）：v3 在**第 6 拍**（13:02）就以 `runN=20 runMin=3.2` 报出（v2 该拍 `runN=1` 静默）。注意它的报警原因是 A'（`att=143 succ=122` 时窗口成功率 15%）——即 v3 事实上让该段以另一条判据也能落地，双保险。

## 7.4 D4 复算

**① 文案是否还有与真实段跨度不符的数字**：v3 **0/641** 拍（v2 是 701/895 拍）。

```
   ⑧ D4 文案审计：带「跨 X」的 reason 共 641 拍，其中与样本自身跨度不符的 0 拍（v2 曾出现「跨 14h18m45s」实为 31 秒）
   —— v2 同一审计 ——
   ⑧ D4 文案审计：带「跨 X」的 reason 共 895 拍，其中与样本自身跨度不符的 701 拍
```

**但 09-11 那条 14h 级的文案仍在**，我逐条查了原始数据，确认它现在**不是**造假：

```
  失败 52 次、成功 20 次；首失败 09-10 19:25:15 末失败 09-11 09:47:05 跨度 14:21:50.034000
  段内最大失败间隔 14:15:14.541000 ⇒ 中间确实长期无流量
```

即：那 43 连败确实是连续失败（期间零成功），只是中间夹了 14 小时完全无流量，所以「样本自身跨度」真的是 14h18m。v2 的错在用它冒充**墙钟跨度**（当时段只持续 31 秒也会写成 14h），v3 已改为样本跨度 —— 数字现在与「首末失败样本的间隔」严格一致，剩下的是**观感**问题（读者会以为持续坏了 14 小时）。建议后续文案补一句「（期间 N 分钟无流量）」，不阻断 go。

**② 短促爆发报警数**（我 §4.4 口径，n<100 且段自身跨度<3 分钟）：**66/111**，与 v2 相同（复算一致，未引用实现方数字）：

```
   ⑥b 刻意不判的短促爆发（n<100 且 跨度<3min）：111 段，其中仍报出的 66 段（应为 0 或少）
```

一致是**预期**的：D4 只改了 B/B'（运行段）的跨度语义，A/A' 按裁决 2 保持窗口跨度，而 66 段正是靠 A/A' 报出来的。

**③ 裁决 2 的处理**：见 §7.2 —— **接受**，不改测试期望。

## 7.5 回归

```console
$ go test -count=1 -run 'TestMV2_' .                       # 26 PASS + 3 SKIP
ok  	github.com/shiyanYX/opencode2api	1.438s
$ MV2_DEFECT_PROBES=1 go test -count=1 -race -run 'TestMV2_' .
ok  	github.com/shiyanYX/opencode2api	10.737s
$ go test -count=1 .                                       # 全量包
ok  	github.com/shiyanYX/opencode2api	49.624s
```

- v2 的 19 个非探针用例在 v3 上**全部 PASS**（无回归），v3 新增 7 个（V3A–V3G，见 §7.8），合计 26 PASS + 3 SKIP（SKIP 只在没开 `MV2_DEFECT_PROBES` 时出现）。
- `-race`（探针开）无报告。
- ⚠ **全量包测试的说明**：本节所有 `go test` 都加了 `-overlay`，原因见 §7.10.1（队友未完成的 `unmapped_stats_test.go` 使 test 二进制无法编译）。`go build .` 本身正常；`-overlay` 只桩掉那一个文件、不动仓库。

## 7.6 四指标复算（全部我自己算，附 v2 差分）

同一回放代码 + 同一数据，`-overlay` 只换实现：

| 指标 | v2 | v3 | 判定 |
| --- | --- | --- | --- |
| ① 事故窗 2026-10-19 00:00–09:00 Active 覆盖率（分钟） | 474/540 = 87.8% | **474/540 = 87.8%** | 持平（未退化） |
| ② 稀疏段 00:16:13–00:33:42 覆盖率 | 17/17 = 100% | **17/17 = 100%** | 保持 100% |
| ③ 健康时段误报（≥20 次；原始 / 剔除口径假象） | 0.38% / 0.15% | **0.40% / 0.14%** | 裁决 1 已接受；剔除后略优 |
| ④ §2.5「承诺必报零成功段」漏报 | 159/168 = 94.6%（漏 9） | **168/168 = 100.0%（漏 0）** | ✅ 期望达成 |
| ④b 低成功率时段（60 分钟 ≤5%、≥20 次）覆盖率 | 98.8% | **98.8%** | 持平 |

**v2→v3 逐拍差分**（107 分钟 Active 状态不同）——我逐条做了归类，确认**没有真回归**：

```
Active 状态不同的分钟数: 107
  v3 新亮(召回增加): 51   v3 变暗(召回减少/回归): 56

=== 56 个变暗分钟的归类 ===
  D4墙钟假跨度        17      # v2 靠「now-runStart」凑出的假跨度 >=3min，真实样本跨度 <3min ⇒ v3 不该报
  D2闪烁残影         39      # v2 亮一拍/灭一拍的孤岛（49/56 前后邻拍都暗），v3 清掉后不再自亮
  其他             0
```

**关键召回检验**（防「修 D4 把 F1 修回去」）：把每一拍按 v3 的 B 判据承诺重算 —— 「截至该拍的进行中段 n≥20 且样本自身跨度≥3 分钟」：

```
承诺必报的进行中故障分钟: 434，v3 报出 434 = 100.0%
漏报分钟数: 0
```

另外 51 个「新亮」分钟里，32 个落在真实段的 [start, end+3min] 内、其余 19 个落在段结束后 10 分钟窗口内的保持拍（`lastRun*` 仍在窗口内 ⇒ 按设计保持 Active），无一是无依据的点亮。

## 7.7 闪烁与永不恢复

```
   ⑤ 全部 95646 拍：Active 10347 拍（10.8%）、状态翻转 355 次、ERROR 触发 226 次
   ⑦ 闪烁（报→清→报，两段间隔均 <=3 分钟）共 2 次；全部翻转 355 次
```

（v2 对应值：Active 10352 拍、翻转 **417**、ERROR **272**、闪烁 **32**。实现方自报 355/226/2 与我复算一致，但这是我自己算出来的。）

剩下的 2 次我逐个查了成因：

```
 报@2026-09-16T00:36 -> 清@00:39(3min) -> 再报@00:41(2min)
   清那一拍 rate10=0.37（窗口 114 次尝试），清除前 11 分钟最大连胜 = 22（末次 00:38:59）
 报@2026-09-30T13:50 -> 清@13:52(2min) -> 再报@13:53(1min)
   清那一拍 rate10=0.29（窗口 307 次尝试），清除前 11 分钟最大连胜 = 35（末次 13:42:26）
```

**判定：不是新缺陷，是可接受的边界（但成因值得记录）。** 两次都是 `successStreak ≥ 20` 这条恢复证据在**窗口样本充足、成功率却只有 29%/37%** 的坏层里把告警清了（20 连胜在 30% 成功率的层里概率极低但真的发生了），1–3 分钟后 A 判据又把告警点亮。它属 R1，v2 同样存在且更频繁：

| | v2 | v3 |
| --- | --- | --- |
| 清除总次数 | 208 | 177 |
| 其中「窗口 ≥20 样本且成功率 <80%」仍被清（只能靠 20 连胜） | 132 | **121** |
| 这些清除后 5 分钟内复亮 | 40 | **11** |

即 v3 把这类清除和复亮都压掉了近 3/4。**D1 那条路径（≥100 连败 + 零流量）确认不再复亮**（§7.3：兜底后 1 小时 0/60 分钟 Active、翻转 1 次）。

## 7.8 新增对抗面（task-18 §6 四条，全部写成测试）

| 测试 | 针对 | 结果 |
| --- | --- | --- |
| `TestMV2_V3A_OngoingOutageNotLostWhenDiscardingEndedRun` | 「丢弃已结束段」会不会漏掉真实持续故障（40 连败 → 1 次成功 → 又 30 连败） | PASS：第 3 拍起持续 Active 到末拍，中途那次成功没把持续故障清掉 |
| `TestMV2_V3B_CrossGapOutageStillAlerts` | 段跨度改成样本自身跨度后，跨空流量空档的真实故障还报不报（F1 回归面） | PASS：20 连败（自身跨度 3m10s）+ 17 分钟无流量 ⇒ 第 3 拍报出；20 分钟后 `run_minutes=3.17`（墙钟会是 20 分钟） |
| `TestMV2_V3C_BigEndedRunNotSqueezedBySmallOne` | D3 复发面：大段记忆被小段挤掉 | PASS：40 连败后跟 1 次小段，末拍 `run_failures=40` |
| `TestMV2_V3D_ClockClampDoesNotSuppressNormalJudgement` | clamp 会不会把正常判定也压掉 | PASS：正常 25 连败照报，`run_minutes=4.80`（= 25×12s 的精确样本跨度） |
| `TestMV2_V3E_FutureRunStartDoesNotFakeSpan` | 未来样本 + clamp 组合 | PASS：回拨时 `run_minutes=0` 且不报；前跳到段真实发生后又正常报（clamp 不是永久压制） |
| `TestMV2_V3F_ExpiredBigRunMustBeReplaced` | 过期大段是否让位 | PASS（但见 §7.9 的坦白：这条**抓不住** M11） |
| `TestMV2_V3G_ExpiredBigRunReplacedByFreshEndedRun` | 同上，**有判别力**的形状（新段会结束，走 rememberEndedRunLocked） | PASS：第 22 拍 `run_failures=15`（新段），不是过期的 40 |

## 7.9 变异电池（6 个新变异；如实记录 1 个第一轮没抓住）

方法同 v2：备份 → 单点改坏 → 跑目标测试（预期红）→ 还原 → 核对 `sha256sum node_alert.go = b0783b4a…` → 再跑（预期绿）。**6 次还原后 sha256 全部一致。**

| # | 变异（对应 v3 的哪处修复） | 目标测试 | 变异后 | 还原后 |
| --- | --- | --- | --- | --- |
| M7 | 撤销 D3：`rememberEndedRunLocked` 退回无条件覆盖 | D3 探针 \| V3C | **红**：`D3 复现：承诺必须报的段…被整体漏掉；末拍 runN=1` | 绿 |
| M8 | 撤销 D1：burst 分支去掉 `!stale` | D1 探针 | **红**：`stale(6h) 兜底后横幅仍亮着 30/60 分钟` | 绿 |
| M9 | 撤销 D2：清除时不丢弃 `lastRun*` | D2 探针 | **红**：`恢复后仍在闪烁（翻转 10 次、ERROR 5 次）` | 绿 |
| M10 | 撤销 D4：`end = m.runLastAt` 退回 `now` | V3B \| V3D | **红**：`run_minutes 应约等于 3.2 分钟，实际 20.00`；`实际 8.0000 应约等于 288.0s` | 绿 |
| M11 | **修过头**：只记更大的段，删掉「已过期可覆盖」 | D2 探针 \| F2 | ⚠ **第一轮仍绿（没抓住）** → 补 V3-G 后 **红**：`第 22 拍 run_failures=0，应为刚结束的新段 15 —— 过期的 40 大段仍在占位` | 绿 |
| M12 | clamp 写反方向（`end.After(now)` → `end.Before(now)`） | V3D \| V3E | **红**：`run_minutes=8.0000 应约等于 288.0s，clamp 误伤正常跨度` | 绿 |

**关于 M11（重要，不掩饰）**：这条变异在第一轮没被抓住。原因是 `judgedRun` 自己也有「`now-lastRunEnd <= 窗口`」的新鲜度判断，过期大段在那一层就被忽略，所以「删掉 rememberEndedRunLocked 的过期条件」在多数场景下是等价变异。我据此补了 `V3-G`（形状：过期大段 → **新段会结束** → 检查新段有没有被记成 lastRun），补后 M11 变红。这也说明 M11 本身**危害有限**（judgedRun 兜了一层），但覆盖要求仍然成立，故补齐。

## 7.10 剩余风险、不确定处与我踩到的环境问题

### 7.10.1 ⚠ 队友未完成的测试文件导致全量 `go test` 无法编译（需要 Lead 协调）

- 现象：`unmapped_stats_test.go`（untracked，16:04:10 写入，队友正在做 stats-unmapped-model 相关任务）引用了尚不存在的符号，例如 `isUnmappedModel`、`unmappedBuckets`、`CallRecord.Unmapped`、`TokenStatsData.UnmappedRequests`。
- 影响：**整个 test 二进制无法编译**，我的 `go test` 全部失败（`go build .` 正常）。这与 task-18 的「全量 `go test -count=1 ./`」要求直接冲突。
- 我的处置（**没有动队友的文件**）：用 `go test -overlay=/tmp/mv3_overlay.json`，overlay 把该文件替换成一个只含 `package main` 的桩：
  ```console
  $ go vet .                                          # 失败
  # github.com/shiyanYX/opencode2api
  vet: ./unmapped_stats_test.go:101:33: unknown field Unmapped in struct literal of type CallRecord
  $ go test -count=1 -overlay=/tmp/mv3_overlay.json .  # 通过
  ok  	github.com/shiyanYX/opencode2api	49.624s
  ```
- **待办**：等该队友的实现落地后，必须在**不加 overlay** 的情况下重跑一遍 `go test -count=1 ./`；本节的 go 结论建立在「除该文件外全量通过 + v3 相关用例全绿」之上。

### 7.10.2 其他残余风险

1. **R1（最值得排期）**：`successStreak≥20` 在窗口样本充足且成功率 <80% 时也会清告警（v3 121/177 次清除属此类，11 次 5 分钟内复亮）。实现注释写的是「只有当窗口样本**不足**时它才划算」，但代码没有这个前置条件。v2 更严重（132/208、40 次复亮），所以不是 v3 引入的；但建议后续加「窗口样本 < nodeAlertClearSamples 时才允许用连胜证据」。
2. **R3**：14 小时级段跨度文案（§7.4）属实但观感误导，建议补「期间无流量」说明。
3. **CLI 之外未测**：面板我只做了静态核对（`renderNodeAlert`/`run_failures`/null 成功率），未开浏览器；`/api/nodes` 形状按 `main.go` 静态事实核对。
4. **口径未放宽**：四个指标、66/111、v2/v3 差分全部用与 v2 轮**同一份回放代码**（新增的只是新的统计项），阈值未调。
5. **闪烁判据**：我用的三元组判据是「两段间隔均 ≤3 分钟」，与 v2 轮一致，没有为了 v3 放宽到 5 分钟；若按 5 分钟算，v3 是 11 次（v2 是 40 次），我两个口径都留在这里备查。

## 7.11 复现命令

```console
$ export PATH=$PATH:/usr/local/go/bin GOCACHE=$PWD/.gocache GOMODCACHE=$PWD/.modcache GOTMPDIR=$PWD/.gotmp
$ sha256sum node_alert.go                     # b0783b4ae8b8292a34ccf0dde662588f533b64d306fe19215d009379b2da7e30

# 队友未完成文件存在时，所有 go test 需加 -overlay（见 §7.10.1）；该文件修好后去掉即可
$ go test -count=1 -overlay=/tmp/mv3_overlay.json -run 'TestMV2_' -v .              # 26 PASS + 3 SKIP
$ MV2_DEFECT_PROBES=1 go test -count=1 -overlay=/tmp/mv3_overlay.json -run 'TestMV2_Defect' -v .   # 三条 PASS（非 SKIP）
$ MV2_DEFECT_PROBES=1 go test -count=1 -race -overlay=/tmp/mv3_overlay.json -run 'TestMV2_' .       # ok
$ go test -count=1 -overlay=/tmp/mv3_overlay.json .                                 # 全量包 ok
$ go test -count=1 -overlay=/tmp/mv3_overlay.json -run 'TestMV2_ReplayProductionData' -v .          # 四指标
$ MV2_DUMP_CSV=/tmp/mv3_ticks_v3.csv go test -count=1 -overlay=/tmp/mv3_overlay.json -run 'TestMV2_ReplayProductionData' .
$ MV2_DUMP_CSV=/tmp/mv3_ticks_v2.csv go test -count=1 -overlay=/tmp/mv3_overlay_v2.json -run 'TestMV2_ReplayProductionData' .   # v2 差分
$ python3 /tmp/mv3_mutate.py                  # 变异电池（6 个，逐次还原核对 sha256）
```
