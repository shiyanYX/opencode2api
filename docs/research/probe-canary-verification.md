# 探测 canary 修复 · 独立验收（task-37 / IP-3）

> 验收人：`verify-c5v2`（独立于实现者 `impl-ip1`）
> 验收对象：task-34 交付 —— `proxy_node.go`（工作区未提交改动）+ 两个 gitignored 测试文件
> 依据：`docs/research/health-probe-blip-diagnosis.md`（§6 方案 A / B1）+ Lead 的 task-37 任务书
> 本报告所有数字均为**本机复跑**所得；实现者的自测结论、`/tmp/t34_mut` 的 7 个变异、报告 §1 的归属表**均未采信**，只作为对照输入。

## 结论：**GO**（可上线，附 §9 残留风险与 §10 上线观测建议）

一句话：两笔改动都在范围内且生效；43% 固定坏子集在生产形态下第 3 轮收敛到 `dead=91 / available=122`，真·全局抖动前 3 轮**零误杀**且能自愈；被修改的 2 条既有断言经双向实验证明是**契约纠错而非放水**（新断言在旧实现上仍然变红）；我的 10 个独立变异全部被全量套件抓住。

---

## 1. 交付物核对（先查「是不是它说的那个东西」）

| 项 | 实现者声明 | 我的复测 | 判定 |
| --- | --- | --- | --- |
| `proxy_node.go` sha256 | `4fdd1935…48d65` | `4fdd19352f0e31d2cdc980cddd82a5dde37212b8829e53ee2443484f93048d65` | ✅ 一致 |
| `git diff --numstat` | `337 29` | `337 29 proxy_node.go` | ✅ 一致 |
| hunk 数 | 17 | `git diff -U1` = **17**（`-U0`=24，`-U3`=12；实现者口径是 `-U1`） | ✅ 可复现 |
| 源码改动范围 | 只有 `proxy_node.go` | `git status` 中唯一被修改的 `.go` 源码即它（`static/admin.html` 等是面板线他人在改，与本任务无关） | ✅ 无夹带 |
| `zz_t30_alert_safety_test.go` sha256 | `eb67075b…794ee` | `eb67075bb767bf0d273cd632ff5365d7ef29aa0b20bb0efcffed3f5a57e794ee` | ✅ |
| `zz_t34_probe_canary_test.go` sha256 | `d004986e…e9001` | `d004986eb3138f24828170de934f494cacffa7f1d67a3eea713c4ade620e9001` | ✅ |
| `gofmt -l` / `go vet` | clean | 无输出 / 无输出 | ✅ |

### 1.1 逐 hunk 归属（独立重算，证明只有 A/B 两笔）

`git diff -U1 proxy_node.go` 的 17 个 hunk，**全部**可归入 A（canary 对照组）或 B（loadState/C-7）；29 行删除也全部落在两笔内（旧 `loadState()` 主体 9+1+2+1+1 行、旧 `newProxyPool` 调用 1 行、旧注释/变量块等），无「顺手重构/夹带」：

| hunk | 新文件行 | +/− | 归属 | 内容 |
| --- | --- | --- | --- | --- |
| h01 | 9 | +1/−0 | A | import `hash/fnv` |
| h02 | 227 | +8/−0 | A+B | `nodePool.lastProbeOK`（A）+ `stateRestored`（B） |
| h03 | 294 | +3/−1 | B | `newProxyPool` 删掉失效的 `loadState()` 调用 |
| h04 | 648 | +4/−1 | A | 老闸门注释改为「无对照组兜底」 |
| h05 | 662 | +169/−0 | A | canary 常量/枚举/`decideProbeRound`/`canaryPick`/`probeCanaries` 全段 |
| h06–h11 | 956–1082 | +72/−18 | A | `checkFiltered`：`okFP`/`probedFP` 收集、`guarded` 判定、四种裁决、成功集合刷新 |
| h12 | 1272 | +16/−0 | B | `setNodes` 首次装入后恢复 |
| h13–h16 | 1334–1426 | +62/−17 | B | `proxyStateVersion`、`readPersistentState`、`restoreStateLocked` |
| h17 | 1432 | +2/−1 | B | `persistentState.Version` |

## 2. 核心判据一：43% 固定坏子集必须被判死（生产形态）

**我自己写的测试**（`zz_t37_verify_test.go`，不复用实现者的 helper/断言）：213 节点、固定 91 个坏（= 诊断 §2.7 的 `failed=91/213=42.7%`），逐轮真实驱动 `checkAvailable`：

```
TestIP3V_ProductionShapeConvergesTo122:
  已确认(i)：213 节点 / 91 个固定坏 ⇒ 第 1 轮抑制、第 2 轮 streak=1、第 3 轮 dead=91、
  available=122（与诊断的真值 122/214 一致），第 4 轮只探 122 个
```

逐条断言（都真实执行，不是日志）：

1. 第 1 轮（冷启动、无对照组）：`dead=0`、`streaked=0`、`blipStreak=1`（先抑制）。
2. 第 2 轮（对照组 5 个全部来自上一轮成功集合、且**结构上不可能**落在坏子集）：`dead=0`、`streaked=91`（每个坏节点 `streak=1`）、`blipStreak=0`。
3. 第 3 轮：`dead=91`、`available=122`；逐节点断言坏节点全 dead、**122 个健康节点全部 available 且零误杀**。
4. 第 4 轮：状态稳定；探针调用数 = **122**（只探 available，对照组都在本轮集合内 ⇒ 零额外探针）。

> 与实现者的 216/92 合成池（→ dead=92/available=124）一致；两者都落在诊断外推的 `122.6/214` 附近。

## 3. 核心判据二：真·全局抖动一个都不判死

```
TestIP3V_RealGlobalBlipMarksNobodyThenRecovers:
  已确认(ii)：真抖动前 3 轮 dead=0（其中第 1 轮对照组 5/5 挂）、第 4 轮起限量 10 个、
  每 2 轮判死 10 个；抖动结束后 checkDead 全部复活。持续抖动 >4 轮会开始限量判死
```

- 第 2 轮（对照组 5/5 挂 = canary 100% ≥60%）：`dead=0`、`streaked=0`、`blipStreak=1`。
- 第 3–4 轮（整池全挂 ⇒ 成功集合空 ⇒ 退回老口径）：仍 `dead=0`，`blipStreak=3`。
- 第 5 轮（升级）：`dead=0 / streaked=10`（只让 10 个进入 `streak=1`，受 `probeFailStreakThreshold=2` 守卫）。
- 第 6 轮：`dead=10 / available=110`（单轮上限生效，不整池抽干）。
- 抖动结束：`checkDead` 复探 10 个 dead 全部成功 ⇒ `dead=0 / available=120`（**可自愈**）。

## 4. Lead 重点 1：被修改的 2 条既有断言，是纠错还是放水？（双向实验）

被改文件：`zz_t30_alert_safety_test.go`（gitignored）。原版冻结副本：`/tmp/t41b/frozen/…`（md5 `a8fc47f0…`，mtime 早于本次实现）。改动仅两条用例：`TestT30GateNowJudgesFixedSubsetDead`（第 2 轮 `dead=92` → 第 3 轮 `dead=92`，新增 `streak`/`blipStreak`/`available` 断言）与 `TestT30CriterionCPoolEmptyReachability`（升级轮 `dead=10` → 下一轮 `dead=10`）。

两个方向的实验（**均跑全量套件**，不是子集）：

| 实验 | 组合 | 全量结果 | 原始失败信息 |
| --- | --- | --- | --- |
| E-A | **新实现 + 原始（冻结）断言** | FAIL，89.3s，2 条红 | `:122 第 2 轮 dead=0，期望 92`；`:227 第 4 轮 dead=0，期望 10` |
| E-B | **旧实现（`git show HEAD:proxy_node.go`）+ 新断言** | FAIL，124.2s，2 条红 | `:125 第 2 轮后 blipStreak=2，期望 0`；`:254 第 4 轮 dead=0 streak=0，期望 dead=0 streak=10` |
| 基线 | 新实现 + 新断言 | `ok 93.967s` 全绿 | — |

判定：**实现者的「断言错、不是实现错」成立，且不是放水。**

- 原断言错在哪（根因，可独立复算）：`markProbeDead` 有 `probeFailStreakThreshold=2` 的连续失败守卫，且被抑制轮次**不累积** streak（`TestProbeGlobalBlipMarksNobody` 钉住的既有契约）。所以「第 2 轮就 dead=92」在**任何保留该守卫的正确实现**下都不可能成立；正确序列是 第 1 轮抑制 → 第 2 轮 streak=1 → 第 3 轮 streak=2 判死。E-A 用真输出证明了这一点。
- 新断言**仍能抓住原缺陷**：E-B 把实现退回 HEAD（老闸门），新断言在第 2 轮就以 `blipStreak=2≠0` 变红，第 4 轮再红一次；不是「退回旧 bug 仍然绿」。
- 新断言严格更强：额外钉住 `streak` 精确值（1/2/10）、`blipStreak`、`available`、好节点零误杀；未删断言、未加 `t.Skip`、未放宽阈值。

## 5. Lead 重点 2：`probeCanaryMinSample=3`（任务书外的边界常数）

**事实认定**：该常数不在 task-30/34 任务书里，是实现者自己加的边界。它只在「上一轮成功集合 <3 个」时改变行为。

- 它要防什么：`lastProbeOK` 只有 1–2 个节点时，1/1、2/2 天然 100% ≥60%，会被当成「全局抖动」⇒ 小池子**永不判死**（正是本次要修的缺陷的翻版）。
- 实现者称「是既有用例 `TestProbeSuccessResetsFailStreak`（2 节点池）抓出来的」——**我复跑证实属实**：把 3 改成 2 或 1，既有用例 `zz_healthguard_test.go:131` 立刻变红（`成功应把 streak 归零，得 0`），因为第 3 轮被误判抖动、streak 没累积到 1。所以不是拍脑袋、也不是为通过新写测试而调。
- 边界扫描（全量/定向复跑，我的独立用例 + 既有用例 + 实现者边界表）：

| 取值 | 结果 | 抓住它的用例 |
| --- | --- | --- |
| 1 或 2 | CAUGHT | **既有** `TestProbeSuccessResetsFailStreak` + 实现者边界表（2/2 两子例）+ 我的 `TestIP3V_TwoSampleCanaryMustNotSuppress`（`第 2 轮 dead=0 期望 4`） |
| **3（交付值）** | 全绿 | — |
| 5 | CAUGHT | 仅实现者边界表（`对照组3个全挂-刚到最小样本` 期望 blip，实测 trusted） |

- 不变区间分析：`decideProbeRound` 只在 `canaryProbed∈{1,2}` 且失败率 ≥60%（即 1/1、2/2）时受该常数影响；`canaryProbed≥3` 或 `canaryProbed=0` 时与它无关。生产形态上一轮成功 ≈122 ⇒ 对照组恒为 5 ⇒ **生产行为与 3/4/5 无关**，因此不存在「为生产调参」的空间。
- 会不会误判：`minSample=3` 下，3–4 个样本全挂仍会判抖动（抑制 3 轮后限量升级，最终仍会判死，只是慢 2–3 轮，且 `checkDead` 可自愈）；`minSample=5` 则会把「3/3、4/4 全挂」直接采信判死——两者都自愈，3 是「n≥3 才算非退化样本」的最小值，方向可接受。
- **结论**：越界但合理；属于「规格外新增的可调参数」。建议在 task book/规格里补一句登记（不阻塞上线）。

## 6. 我自己的变异测试（10 个，全量套件，均不复用实现者的 MUT 编号/结论）

方法：`go test -overlay` 把 `proxy_node.go` 映射到 `/tmp/ip3v/mut/*.go`，**每个变异跑完整套件**（`-count=1 ./`，仅跳过唯一真实网络用例 `TestT30ProbeCauseAttribution`），避免实现者 M4 那种「子集里绿」的假象。

| 变异 | 改了什么 | 全量结果 | 抓住它的用例（节选） |
| --- | --- | --- | --- |
| MV-A | canary 判据短路（`if false`，永不判抖动） | **CAUGHT** 105.4s | T34 边界表 4 子例、`T34RealGlobalBlipMarksNobody`、我的 RealGlobalBlip / ProbeCost / CanaryAllDie |
| MV-B | 判据反向（<60% 判抖动） | **CAUGHT** 102.7s | 9 个用例，含我的 4 个 |
| MV-C | 删除升级（`prevBlipStreak>=3` 永不触发） | **CAUGHT** 118.7s | `T30CriterionCPoolEmptyReachability`、`T34NoCanaryDegradesThenEscalatesCapped`（15 轮 60/60 未判死）、我的 RealGlobalBlip |
| MV-D | `setNodes` 不再恢复状态（C-7 复现） | **CAUGHT** 119.2s | `T34LoadStateRestoresDeadAndExhausted`、`T34LoadStateOnlyOnFirstPopulation`、我的 LoadStateRestores（`dead 节点重启后变成 available`） |
| MV-E | 抖动分支**不刷新**成功集合（去掉自愈） | **CAUGHT** 99.3s | `T30GateNowJudgesFixedSubsetDead` + 我的 4 个（`自愈轮 dead=0 streaked=0`、`第 1 轮成功集合=0`） |
| MV-F | 阈值 60→90 | **CAUGHT** 99.8s | T34 边界表（3/5、6/10） |
| MV-G1 | minSample 3→1 | **CAUGHT** 103.7s | 既有 `TestProbeSuccessResetsFailStreak` + T34 表 + 我的 TwoSample |
| MV-G2 | minSample 3→2 | **CAUGHT** 98.8s | 同上 |
| MV-G5 | minSample 3→5 | **CAUGHT** 103.7s | T34 表（3/3） |
| MV-H | 无对照组判死 cap 10→1000 | **CAUGHT** 98.0s | `T30CriterionC`（`streak=216`）、我的 RealGlobalBlip（`streaked=120`） |

结论：这 10 个「把实现改坏」的方向全部被现有测试网（实现者 + 既有 + 我的）在**全量**下抓住；同时反向说明这些测试不是靠子集才绿。

## 7. loadState / C-7（B1）独立验证

我的用例（`zz_t37_verify_test.go`）：

- ⑦ 三态恢复：`dead/exhausted/available` + `marked_at/cooldown_until/last_error/latency_ms` 全部恢复；`exhausted` 缺 `cooldown_until` ⇒ 跳过（不永久封禁）；未知状态字符串 ⇒ available；**指纹不属于本池 ⇒ 忽略**；`flush` 之后再 `setNodes`（订阅刷新）**不再读盘**（内存状态不被旧磁盘覆盖）。
- ⑦b 8 种坏文件（空 / 非 JSON / 截断 / version=99 / version=-1 / 坏时间 / `nodes:null` / `nodes:[…]`）：**全部不 panic、不误标 dead**，降级为 available。
- ⑦c 升级安全：旧文件**没有 `version` 字段**（Version=0，在 `[0,1]` 接受区间）照常恢复 dead —— 上线不会把已判死的节点全部复活。

原 bug 的因果链（实现者 B1 说法）我也独立复核：`newProxyPool()` 调用时 `p.nodes` 为空，旧 `loadState()` 的 `for _, n := range p.nodes` 一次都不执行；MV-D 复现即「重启后 dead→available」。修复点（首次 `setNodes`、装入后、`saveState` 前）位置正确：若在 `saveState` 之后，第一次 `setNodes` 会先把「全 available」刷盘覆盖旧状态（实现者注释所述，符合代码顺序）。

## 8. 成本、`probeDead` 轮与对抗场景

- **成本**（我的计数钩子逐次统计，非日志推断）：稳态轮额外探针 **+0**（对照组都在 `probeAvailable` 本轮集合里，`probeCanaries` 复用结果）；对照组本轮不在集合时（如刚被标 exhausted）**恰好 +5**，总调用 `55+5=60`，且补探**不改状态、不清零/不累积 `streak`、不覆盖 `LastError`**。最坏 +5/轮 = +2.3%，与诊断 §6 估算一致。
- **`probeDead` 轮**：3 轮复探只探 dead（`20×3=60` 次），`blipStreak` 保持人为设置的 7 不变、成功集合不变、dead 节点复探失败不改 streak；复探成功即复活（`dead=0 available=50`）。**复探轮不参与抖动判定**成立。
- **对抗方向 A（上一轮 5 个样本好、池子大部分坏）**：第 2 轮对照组 5/5 健康 ⇒ 采信 ⇒ 第 3 轮 `dead=91/available=122`。对照组抽自「上一轮成功集合」，坏节点**结构上不可能**进入（除非它上一轮恰好成功一次，见 §9.2），因此不会因为「池子大部分坏」而误判为抖动、也不会卡住 dead 信号。
- **对抗方向 B（上一轮成功的 5 个随后全挂）**：第 2 轮被当成抖动（`dead=0`）——这是 canary 判据的固有代价（局部 5 个坏被掩 1 轮）；但抖动分支**仍刷新成功集合**，第 3 轮对照组换成另外 5 个健康节点 ⇒ 第 3 轮 `streaked=5`、第 4 轮 `dead=5 / available=115`，**不会永久卡住**。MV-E 证明该刷新是承重的（去掉后第 3 轮永远 dead=0）。

## 9. 量化：修复后生产 `available` 落点 + 是否会误杀 122 个好节点

- 诊断口径（`health-probe-blip-diagnosis.md` §2.7，独立于本次实现）：按来源成功率外推 `可用 ≈ 122.6 / 214`（失败 ≈91.4，42.7%），与生产日志 `failed=91/213` 吻合。面板当时报 `healthy=213 / dead_count=0`。
- 状态机复跑（§2）：213 节点 / 91 固定坏 ⇒ 第 3 轮 `dead=91`、`available=122`，逐节点断言 122 个健康节点**零误杀**、91 个坏节点全部 dead。
- 结论：上线后约 3 轮（≈45 分钟，含冷启动首轮抑制）面板应落到 **`available≈122`、`dead≈91`**（216 节点生产抽样口径则 ≈124/92）；dead 信号恢复，`ROADMAP §2b` 的「探测判定节点死了」输入不再是空集。
- **会不会误杀 122 个好节点**：不会新增误杀路径——`markProbeDead` 只会对被探针判失败的节点调用；好节点每轮成功即 `recordProbeSuccess` 清零 streak。canary 闸门只做「**少判**」方向的抑制，不做额外判死。唯一误杀可能仍来自既有 `probeFailStreakThreshold=2` 之下连续两轮真失败（老行为，未被本次改动改变）。
- 代价：上线后**首 2 轮面板仍可能显示满编**（第 1 轮无对照组先抑制、第 2 轮只累积 streak），第 3 轮才落地——这不是缺陷，是 streak 守卫的既定延迟。

## 10. 回归

| 命令 | 结果 |
| --- | --- |
| `go vet ./` | 无输出（clean） |
| `gofmt -l proxy_node.go zz_t30_alert_safety_test.go zz_t34_probe_canary_test.go zz_t37_verify_test.go` | 无输出（clean） |
| `go test -count=1 -skip 'TestT30ProbeCauseAttribution' ./` | **`ok github.com/shiyanYX/opencode2api 78.107s` 全绿**（最终树，含我的 9 个验收用例；唯一跳过的是打真实网络的 `TestT30ProbeCauseAttribution`——本环境网络受限，实现者声明其本机 137.2s 含它全绿；早前同命令在建全量对照时也跑过一次全绿 93.967s） |
| `go test -race -count=1 -run 'Probe|Node|State' ./` | 见下（补填） |
| E-A / E-B 全量对照 | 各 2 条预期红（见 §4），其余全绿 |
| 10 个变异全量 | 10/10 CAUGHT（见 §6） |

## 11. 残留风险与建议（不阻塞上线）

1. **持续真抖动 >4 轮会开始限量判死**：连续 3 轮无对照组抑制后，每轮让 10 个进入 streak、每 2 轮真正判死 10 个。若是「本机出口故障持续 >45–60 分钟」，2 小时会误杀 ~30 个（可被 `checkDead` 每分钟复探 + 成功复活自愈）。判定：可接受，但建议上线后若出现 `blipStreak≥3` 告警，优先查本机出口而不是节点池。
2. **局部 5 个坏会被掩 1 轮**：若某轮恰好只有 5 个节点坏且它们全在对照组里 ⇒ 该轮判抖动（`dead=0`），下一轮自愈后再判死。方向安全（少判不误杀）。
3. **对照组按指纹哈希抽、不按来源分层**：若某来源占池 ≥50% 且整片故障，5 个对照全落在其中的概率 ≈ 0.5⁵ ≈ 3%，该轮会被误判抖动（漏判 1 轮，自愈）。实现者报告 §6.1 已列，我确认概率口径无误。
4. **`probeCanaryMinSample=3` 是规格外参数**（见 §5）：建议登记进规格；生产行为与 3/4/5 无关。
5. **部署后首 2 轮面板仍显示满编**（设计使然，见 §9）。
6. **未部署生产实测**：本验收只在合成池 + 真实网络诊断用例上复跑；生产 213 节点回放依赖部署后观测。

## 12. 复现命令（本机）

```bash
cd /home/jingyx/project/opencode2api-test
export PATH=$PATH:/usr/local/go/bin GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"

# 我的独立验收用例（9 个）
go test -count=1 -run 'TestIP3V' -v ./

# 全量回归（跳过唯一真实网络用例）
go test -count=1 -skip 'TestT30ProbeCauseAttribution' ./

# 断言牙齿双向实验
go test -count=1 -overlay=/tmp/ip3v/ov/frozen.json  -skip 'TestT30ProbeCauseAttribution' ./   # 新实现 + 原始断言 → 红
go test -count=1 -overlay=/tmp/ip3v/ov/oldimpl.json -skip 'TestT30ProbeCauseAttribution' ./   # 旧实现 + 新断言 → 红

# 变异（10 个，见 /tmp/ip3v/run_muts.sh；每个都是全量）
/tmp/ip3v/run_muts.sh && cat /tmp/ip3v/mut_results.txt
```
