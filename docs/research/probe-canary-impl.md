# 探测抖动闸改 canary 对照组 + 修 loadState（task-34 / IP-3）

> 状态：**实现完成，待独立验收（task-37 / verify-c5v2）**
> 规格来源：`docs/research/health-probe-blip-diagnosis.md` §6 方案 A + 附带必修 B1
> 分支 `test`；源码只改 `proxy_node.go`；新增测试 `zz_t34_probe_canary_test.go`（gitignored，留在磁盘）；
> 另按修复后的契约更新了 `zz_t30_alert_safety_test.go` 的 2 条旧断言（见 §8）
> 基线：`proxy_node.go` sha256 `4fdd19352f0e31d2cdc980cddd82a5dde37212b8829e53ee2443484f93048d65`（1581 行）
> `git diff --numstat -- proxy_node.go` = `337 29 proxy_node.go`（366 行改动，无夹带其他源码文件）

## 0. 结论

1. **抖动闸判据换掉了**：从「整轮失败数 ≥20」改成「**canary 对照组失败率 ≥60%**」。对照组 = 上一轮探测成功的节点里确定性抽 5 个；它们也挂 ⇒ 真·全局抖动（不判死）；它们正常 ⇒ 本轮失败采信（长期坏的固定子集照常按 streak 判死）。
   - 生产形态（216 available、固定 92 个坏 = 42.6%）：老口径第 1 轮就 `failed=92>=20` 永久锁死在「抖动」态、`dead` 恒 0；新判据下第 3 轮这 92 个全部判死（`dead=92`、`available=124`）。
   - 真·全局抖动（对照组 5/5 也挂）：3 轮全挂仍然 `dead=0`、节点 `streak` 不累积。
2. **loadState 从未生效修好了**：`loadState()` 原来在 `newProxyPool()` 里调，那时 `p.nodes` 还是空切片 ⇒ 循环体一次都不执行 ⇒ `proxy_state.json` 写了从来没被读进来，每次重启 dead/exhausted 全部复活成 available。现在恢复点移到 `setNodes()` **首次装入节点列表之后、`saveState()` 之前**，并加 `proxyStateVersion` 与逐条安全降级。
3. **T30 那 2 条用例是「断言错」，不是实现 bug**（详见 §8）。原用例断言「dead 恒 0 forever」，那是 task-30 诊断里记录的**缺陷本身**；修复后契约必然改变。新断言仍钉住同一缺陷的可观测后果（坏子集是否被判死 / 是否会永不判死），且 7 个变异全部被抓到（§3）。
4. **一个计划外的边界补丁**：小池子（上一轮只有 1~2 个成功节点）时对照组失败率没有统计意义（1/1、2/2 都是 100%），会误判抖动 ⇒ 加 `probeCanaryMinSample=3`，样本不足时退回老口径。这条是 `TestProbeSuccessResetsFailStreak`（2 节点池）抓出来的，不是拍脑袋加的。

## 1. 改动点（文件:行号）

### A. canary 对照组（`proxy_node.go`）

| 位置 | 内容 | 行数 |
| --- | --- | --- |
| `proxy_node.go:10` | import `hash/fnv` | +1 |
| `proxy_node.go:232-236` | `nodePool` 新增 `lastProbeOK []string`（上一轮成功指纹集合 = 对照组候选池） | +5 |
| `proxy_node.go:663-687` | canary 段落说明 + 5 个常量：`probeCanaryCount=5`、`probeCanaryMinSample=3`、`probeCanaryBlipPercent=60`、`probeUnverifiedBlipMaxRounds=3`、`probeNoCanaryMaxDeaths=10` | +~25 |
| `proxy_node.go:690-701` | `probeRoundVerdict` 枚举：`trusted` / `blip_verified` / `blip_unverified` / `escalate` | +12 |
| `proxy_node.go:703-711` | `probeRoundVerdict.String()`（日志可读） | +9 |
| `proxy_node.go:713-745` | `decideProbeRound(...)` **纯函数**：本轮采信决策，边界单测直接打它 | +33 |
| `proxy_node.go:747-773` | `canaryPick(successFP, k)`：按 **FNV-1a(指纹) 排序取前 k**，确定性、与输入顺序无关 | +27 |
| `proxy_node.go:775-780` | `fnv64a` | +6 |
| `proxy_node.go:782-801` | `lastProbeOKSnapshot` / `setLastProbeOK`（加锁读写快照） | +20 |
| `proxy_node.go:803-832` | `probeCanaries`：补探对照组；**复用本轮已探结果**，未探过的才补（≤5 次），只计数不改节点状态 | +30 |
| `proxy_node.go:950-1090` | `checkFiltered` 主体改造：收集 `okFP`/`probedFP`/`failedSet`；`guarded` 分支内做对照组判定、日志、四种裁决 | +~90 |
| `proxy_node.go:648-652` | `probeRoundLooksLikeGlobalBlip` **保留**（verifier 用例直接钉它），注释改为「只作无对照组时的兜底」 | +4 |

### B. loadState 生效（`proxy_node.go`）

| 位置 | 内容 | 行数 |
| --- | --- | --- |
| `proxy_node.go:229-232` | `nodePool` 新增 `stateRestored bool`（只在首次装入节点时恢复一次） | +4 |
| `proxy_node.go:287-298` | `newProxyPool` 里 **删除** `p.loadState()`，留注释说明为什么不在这里读 | -1/+3 |
| `proxy_node.go:1272-1290` | `setNodes`：装好 `p.nodes`/`p.byID` 后、`saveState()` 前调 `restoreStateLocked()`，并打 `proxy state restored from disk` | +16 |
| `proxy_node.go:1338-1341` | `const proxyStateVersion = 1`（0 = 旧文件，兼容接受） | +4 |
| `proxy_node.go:1343-1348` | `saveState` 写入 `Version` | ±1 |
| `proxy_node.go:1369-1388` | `readPersistentState(path) (persistentState, bool)`：坏 JSON / 版本越界 ⇒ 整份忽略 + WARN | +20 |
| `proxy_node.go:1397-1430` | `restoreStateLocked() int`：逐条校验逐条跳过（指纹不匹配跳过；未知状态按 available；exhausted 无 cooldown 跳过） | +34 |
| `proxy_node.go:1432-1435` | `persistentState` 新增 `Version int \`json:"version"\`` | ±2 |

> 旧的 `loadState()` 已删除（其逐条恢复逻辑并入 `restoreStateLocked`，语义从「整份覆盖」改为「逐条校验」）。

### 逐 hunk 归属（证明只有这两笔）

`git diff -U1 -- proxy_node.go` 共 17 个 hunk，全部可归入 A/B：

| hunk（新文件行） | 归属 |
| --- | --- |
| `+10`（import fnv） | A |
| `+227-236`（struct 两个字段） | `lastProbeOK`=A，`stateRestored`=B |
| `+294-298`（删 newProxyPool 里的 loadState） | B |
| `+648-651`（老闸门注释） | A |
| `+662-832`（canary 常量+纯函数+选样+补探） | A |
| `+956-1090`（checkFiltered 收集成功集合 / 裁决 / 日志 / 分支） | A |
| `+1272-1290`（setNodes 恢复点） | B |
| `+1334-1337`（proxyStateVersion） | B |
| `+1343-1348`（saveState 写 Version） | B |
| `+1366-1435`（readPersistentState / restoreStateLocked / Version 字段） | B |

### 对照组抽样一句话

> 每轮从「上一轮探测成功」的节点指纹里按 **FNV-1a 哈希排序取前 5 个**当对照组；它们也失败 ≥60% 判全局抖动、整轮不采信，否则采信并按 `probeFailStreakThreshold=2` 判死。

## 2. 两条核心断言的原始输出

命令：

```bash
export PATH=$PATH:/usr/local/go/bin GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
go test -count=1 -run '^(TestT34FortyThreePercentFailuresMarkDead|TestT34RealGlobalBlipMarksNobody)$' -v ./
```

原始输出（`/tmp/t34_mut/core_assertions.log`）：

```
=== RUN   TestT34FortyThreePercentFailuresMarkDead
    zz_t34_probe_canary_test.go:247: 已确认(i)：100 节点里固定 43 个坏（5 个对照组全程健康 5/5）⇒ 连续 2 轮后 dead=43、available=57、blipStreak=0（43% 失败真的判死了，没被抖动闸挡）
--- PASS: TestT34FortyThreePercentFailuresMarkDead (0.08s)
=== RUN   TestT34RealGlobalBlipMarksNobody
    zz_t34_probe_canary_test.go:293: 已确认(ii)：120 节点全挂 3 轮（对照组取自上一轮成功的 5 个节点，它们也全挂）⇒ dead=0、累积 streak 的节点=0、blipStreak=3（真抖动一个都不判死）
--- PASS: TestT34RealGlobalBlipMarksNobody (0.05s)
PASS
ok  	github.com/shiyanYX/opencode2api	0.141s
```

- (i) 直接回答任务要求「43% 失败的一轮必须真的把坏的标死」：`dead=43`（不是框里的 0），同时好节点一个没误杀（`available=57` = 100-43）。
- (ii) 直接回答「真·全局抖动一个都不判死」：`dead=0`、`streak 节点=0`、`blipStreak=3`（连续 3 轮被识别为抖动）。
  - 注意对照组是**真的在被探**（不是空谈）：`probeCanaries` 复用本轮探测结果；测试里对照组取自上一轮成功的 5 个节点，全挂才判抖动。若对照组没生效（变异 MUT-3），这两个断言之一必然变红。

补充：生产形态（216 节点）的等价断言在 `TestT30GateNowJudgesFixedSubsetDead`：

```
    zz_t30_alert_safety_test.go:153: 已确认：canary 修复后 43% 固定坏子集第 3 轮被判死（dead=92、available=124）⇒ dead 信号恢复
```

## 3. 变异表（7 个，全部被抓；恢复后 sha256 一致）

脚本：`/tmp/t34_mut/run_mutations.sh`（逐条替换 `proxy_node.go` 唯一匹配串 → 跑定向用例 → `cp` 还原 → 校验 sha256）

| # | 变异（把修复改坏） | 期望变红的用例 | 结果 | 红的第一条信息 |
| --- | --- | --- | --- | --- |
| MUT-1 | 对照组分支整段短路（`if canaryProbed >= probeCanaryMinSample && probeCanaryBlipPercent < 0`）⇒ 退回老闸门语义 | `TestT30GateNowJudgesFixedSubsetDead`、`TestT34FortyThreePercentFailuresMarkDead` | **CAUGHT** | `第 2 轮后 blipStreak=2，期望 0` / `43% 节点连续两轮失败后只判死 0 个，期望 43` |
| MUT-2 | 对照组失败率判据短路成「恒判抖动」（`if true`） | 同上 | **CAUGHT** | 同上（`blipStreak=2`；`dead=0`） |
| MUT-3 | 对照组失败率判据短路成「恒信任」（`if false`）⇒ 真抖动保护失效 | `TestT34RealGlobalBlipMarksNobody` | **CAUGHT** | `抖动轮次不该累积 streak，t34-000 streak=1` |
| MUT-4 | 无对照组升级逻辑删除（`prevBlipStreak < 0`）⇒ 永不判死 | `TestT30CriterionCPoolEmptyReachability`、`TestT34NoCanaryDegradesThenEscalatesCapped` | **CAUGHT** | `第 4 轮 dead=0 streak=0，期望 dead=0 streak=10` / `升级 15 轮后仍有 60/60 未判死` |
| MUT-5 | 升级后去掉单轮判死上限（`len(failedFP) < 0`）⇒ 整池一次判死 | 同上 | **CAUGHT** | `第 4 轮 dead=0 streak=216，期望 streak=10` / `第 5 轮单轮判死 60 个，超过上限 10` |
| MUT-6 | `setNodes` 里状态恢复调用禁用（`if !p.stateRestored && len(p.nodes) < 0`）⇒ loadState 修复失效 | `TestT34LoadStateRestoresDeadAndExhausted`、`TestT34LoadStateOnlyOnFirstPopulation` | **CAUGHT** | `重启后 dead 节点 a 被复活成 available（loadState 未生效 = C-7）` |
| MUT-7 | 对照组最小样本量 `3→1` ⇒ 小池子 2/2 也判抖动 | `TestProbeSuccessResetsFailStreak`、`TestT34DecideProbeRound` | **CAUGHT** | `成功应把 streak 归零，得 0` / `decideProbeRound(canary 2/2, ...) = blip_verified，期望 trusted` |

恢复校验（脚本尾部输出）：

```
== restored proxy_node.go sha256=4fdd19352f0e31d2cdc980cddd82a5dde37212b8829e53ee2443484f93048d65 (orig=4fdd...)
== restore OK
```

### 那 2 条 T30 用例的变异「有牙齿」证明

Lead 特别要求：新断言必须仍能抓住原缺陷。对应关系：

- 新 `TestT30GateNowJudgesFixedSubsetDead` 钉的是「43% 固定坏子集**必须**被判死」。
  - MUT-1（退回老闸门语义）⇒ `dead` 永远 0 ⇒ 红。
  - MUT-2（恒判抖动）⇒ 同样 `dead` 永远 0 ⇒ 红。
  - 也就是说：只要有人把 canary 判据改回「按失败数判抖动」，这条用例立刻红。
- 新 `TestT30CriterionCPoolEmptyReachability` 钉的是「无对照组时**不能永不判死**」。
  - MUT-4（删升级）⇒ 第 4 轮 `streak=0`、第 5 轮 `dead=0` ⇒ 红。
  - MUT-5（去 cap）⇒ 单轮 `streak=216` ⇒ 红（防整池抽干）。

## 4. loadState 修复前后对比

### 修复前（现状 bug，C-7）

`newProxyPool()` 里调 `loadState()`；此刻 `p.nodes = []` ⇒ `for _, n := range p.nodes` 一次都不进 ⇒ 磁盘状态 0 条被应用 ⇒ 重启后 pool 全部 available。

复现（等价于把修复改坏）：MUT-6 禁用 `setNodes` 里的恢复调用后：

```
--- FAIL: TestT34LoadStateRestoresDeadAndExhausted
    zz_t34_probe_canary_test.go:394: 重启后 dead 节点 a 被复活成 available（loadState 未生效 = C-7）
--- FAIL: TestT34LoadStateOnlyOnFirstPopulation
    zz_t34_probe_canary_test.go:462: 首次装入后应恢复 dead，实际 available
```

### 修复后

- 恢复时机：`setNodes()` 首次装入后、`saveState()` 之前（若在 saveState 之后，第一次 setNodes 会先把「全 available」刷盘，把旧状态抹掉）。
- 只做一次：`p.stateRestored` 守卫 —— 进程内状态比磁盘新，订阅刷新（再次 setNodes）不能把刚判死的节点写回 available。
- 恢复日志：`proxy state restored from disk path=... nodes=2`。
- 安全降级（`TestT34LoadStateSafeOnBadFiles` 6 个子用例全绿）：乱码 JSON / version=99 / version=-1 / 指纹不匹配 / 未知状态字符串 / exhausted 无 cooldown ⇒ 分别 WARN 并跳过，**不会 panic，不会误标死**。
- 版本：旧文件无 `version` 字段 ⇒ 视为 0 接受；未来版本 `>1` ⇒ 整份忽略（新写旧读安全）。

## 5. 回归

最终树（`proxy_node.go` sha256 `4fdd1935…`）：

| 命令 | 结果 |
| --- | --- |
| `go vet .` | OK（无输出） |
| `gofmt -l proxy_node.go zz_t34_probe_canary_test.go zz_t30_alert_safety_test.go` | 无输出（clean） |
| `go test -race -count=1 -run 'Probe\|Node\|State' ./` | `ok github.com/shiyanYX/opencode2api 81.856s`，**无 DATA RACE** |
| `go test -count=1 ./`（`task8_bucket_query_test.go` 已移开，副本在 `/tmp/t17_task8_hold.go`） | `ok github.com/shiyanYX/opencode2api 137.206s`，**全绿**（含真实网络的 `TestT30ProbeCauseAttribution`） |

既有 verifier 用例保持绿（未被改动，逐条 PASS）：`TestProbeRoundLooksLikeGlobalBlip`、`TestProbeNeedsTwoConsecutiveFailuresToMarkDead`、`TestProbeSuccessResetsFailStreak`、`TestProbeGlobalBlipMarksNobody`、`TestDeadReProbeRoundDoesNotTripBlip`、`TestProbeConcurrencyIsBounded`。

## 6. 残留风险

1. **对照组可能被「整片来源故障」骗到**：抽样是按指纹哈希取前 5，不保证跨来源。若本轮坏的是某个占池 ≥50% 的来源/机房，5 个对照全落在里面的概率 ~3% ⇒ 这一轮被误判抖动（不判死）。影响：**漏判 1 轮**，下一轮重新抽样会自愈；且日志 `probe_round_canary` 每轮一行（`canary_probed/failed/rate/used`）可审计。不会误杀好节点（方向安全）。
2. **「上一轮恰好全成功」不会造成误判**：对照组来自上一轮成功集合 —— 上一轮全成功 ⇒ 本轮对照组就是「最稳的 5 个」，真抖动时它们必挂（判抖动，正确）；局部故障时它们不挂（判死，正确）。真正的边界在上一轮成功集合里**混进了刚坏的节点**：最多 5 个全坏 ⇒ 100% 误判抖动 1 轮（同上，自愈）。
3. **样本不足退化**：上一轮成功节点 <3 个时（冷启动 / 极端坏池）退回老口径，43% 判死能力不生效（老口径 `failed>=20` 会抑制）。生产形态（213 available、上一轮成功 ≈124）不触发；小池子（2~3 节点）刻意保守 —— `TestProbeSuccessResetsFailStreak` 就是这条边界。
4. **无对照组升级会缓慢判死**：连续 3 轮无对照组后每轮判 10 个（且每个还需 2 轮 streak 才 dead）。若「本机出口故障持续 >45 分钟且池里 0 个成功」，会每 ~2 轮判死 10 个；恢复后 `recordProbeSuccess` 会复活，dead 复探周期 1 分钟 ⇒ 自愈。216 节点全坏需 ~44 轮（≈11 小时）才判完，远慢于故障恢复速度，且可自愈。
5. **对照组与「测量噪声」**：探针 HTTP 请求偶发超时（20s）会让对照组 0/5 变 2/5（40%）⇒ 仍采信；3/5（60%）⇒ 判抖动 1 轮。阈值 60% 已按 5 样本设计，单点抖动不会翻转结论。
6. **成本**：每轮 +5 探针（+2.3%）；实际几乎为 0 —— 对照组来自 available 池，本轮本来就全池探过，`probeCanaries` 复用结果，只有「对照组节点本轮未被探测」才补探（≤5 次）。
7. **dead 复探轮不受影响**：`probeDead` 轮不参与 canary/blip 记账（`guarded := filter == probeAvailable` 保持不变），所以不会把 dead 复探的大面积失败误当抖动。
8. **日志语义复用**：`blipStreak` 现在计「连续未采信轮数」（canary verified / 无对照组抑制 / 升级都算），字段名 `consecutive`；老的 `probe_round_global_blip_persistent` ERROR 仍在第 3 轮和第 10*n 轮打。
9. **未修项**：B2（`subscription.go:296` 空池每轮刷一条、144 条/天噪声）越界未改，只记录；B3 WARN 文案保留原文（新增 `verified`/`canary_*` 字段可用于区分）；B4 面板 `available` 口径拆分未做（面板线他人负责）。

## 7. 未做项

- **B2**：`subscription.go:296` 的「没有任何可用节点」空转误报（144 条/天）—— 边界禁止改该文件，仅报告，建议由订阅线按 `len(all)==0 && len(errs)==0 ⇒ (0, nil)` 修。
- **B3**：`probe_round_global_blip` 的 `action` 文案仍是「疑似本机出口/网络问题」；现可通过 `verified` + `canary_*` 字段区分「对照组证实」与「无对照组兜底」，文案未改。
- **B4**：`/api/nodes` 的 `available` 口径（面板虚报 213）—— 面板/接口线负责。
- **跨来源分层抽样**：诊断 §6.2 方案 A 提「跨来源抽」，实现只按指纹哈希抽（未按来源分层）。见 §6.1 风险；若要进一步降低「整片来源故障」误判率，可给 `canaryPick` 加来源分层（需从 `ProxyNode` 取来源字段）。
- **真实 213 节点池实测**：只在合成池（100/120/216 节点）与真实网络诊断用例上验证；本改动**未部署生产**（Lead 明确部署镜像未含 `proxy_node.go`），生产回放留待验收后。

## 8. 收口记录：T30 两条用例的根因 + 修法 + 变异证据

### 8.1 根因判断（一句话）

**两条都是「测试断言写错」，不是实现 bug。** 断言把「判死」当成「一次采信就 dead」，忽略了实现里刻意保留的 `probeFailStreakThreshold=2` 连续失败守卫；同时也误把「升级轮」当成「立刻 dead」。实现行为与设计一致（先 streak、后 dead；抑制轮不累积 streak）。

### 8.2 逐条根因

| 用例 | 原断言 | 实际行为（正确） | 为什么原断言错 |
| --- | --- | --- | --- |
| `TestT30GateNowJudgesFixedSubsetDead` | 第 2 轮就期望 `dead=92` | 第 1 轮无对照组被抑制（streak 不累积）；第 2 轮 canary 健康 ⇒ 采信 ⇒ 92 个 `streak=1`（**还不是 dead**）；第 3 轮再采信 ⇒ `streak=2` ⇒ `dead=92` | 忘了 `markProbeDead` 的 `probeFailStreakThreshold=2`（R2 复审明确要求保留的防抖守卫）。被抑制轮次不累积 streak 也是既有契约（`TestProbeGlobalBlipMarksNobody` 钉住）。 |
| `TestT30CriterionCPoolEmptyReachability` | 第 4 轮（升级轮）就期望 `dead=10` | 第 4 轮升级 ⇒ 10 个节点 `streak=1`，`dead=0`；第 5 轮同一批（按指纹排序取前 10，确定性）`streak=2` ⇒ `dead=10` | 同上：升级轮只是「开始判」，不是「判完」；且 10 个是单轮**进入 streak** 的上限，真正 dead 要下一轮。 |

> 附注：这两条用例原身是 task-30 的**缺陷特征化测试**（原名 `TestT30GateKeepsDeadCountZeroForever`，断言「6 轮 42.6% 失败后 dead 恒 0」），那正是本次要修的缺陷。修复后必须改契约，否则「测试绿」与「缺陷修复」互相矛盾。

### 8.3 修法

- 只改测试断言与注释，**未放宽任何阈值、未删断言、未加 `t.Skip`**：
  - 新断言比原缺陷断言**更严**：原断言只要求 dead=0（缺陷），新断言要求 dead=92 且 available=124（修复目标），并额外钉住 `blipStreak`、streak 数量、好节点零误杀。
  - 新增 `t30StreakCount` 辅助断言「抑制轮不累积 streak」「采信轮 streak 精确到 1、2」。
- 实现侧只补了一条计划外边界：`probeCanaryMinSample=3`（见 §0.4 / MUT-7）。

### 8.4 变异证据

见 §3：MUT-1/MUT-2 使 `TestT30GateNowJudgesFixedSubsetDead` 变红，MUT-4/MUT-5 使 `TestT30CriterionCPoolEmptyReachability` 变红；恢复后两条重新变绿（脚本尾部已校验 sha256）。原始输出：

```
MUT-1 | CAUGHT | red: TestT30GateNowJudgesFixedSubsetDead,... | 第 2 轮后 blipStreak=2，期望 0（对照组健康 ⇒ 不是全局抖动）
MUT-2 | CAUGHT | red: TestT30GateNowJudgesFixedSubsetDead,... | 第 2 轮后 blipStreak=2，期望 0（对照组健康 ⇒ 不是全局抖动）
MUT-4 | CAUGHT | red: TestT30CriterionCPoolEmptyReachability,... | 第 4 轮 dead=0 streak=0，期望 dead=0 streak=10（限量 10 个进入 streak=1）
MUT-5 | CAUGHT | red: TestT30CriterionCPoolEmptyReachability,... | 第 4 轮 dead=0 streak=216，期望 dead=0 streak=10（限量 10 个进入 streak=1）
```

### 8.5 交付物哈希

| 文件 | sha256 | 说明 |
| --- | --- | --- |
| `proxy_node.go` | `4fdd19352f0e31d2cdc980cddd82a5dde37212b8829e53ee2443484f93048d65` | 唯一源码改动（337+/29-） |
| `zz_t34_probe_canary_test.go` | `d004986eb3138f24828170de934f494cacffa7f1d67a3eea713c4ade620e9001` | 新建测试（gitignored，留盘） |
| `zz_t30_alert_safety_test.go` | `eb67075bb767bf0d273cd632ff5365d7ef29aa0b20bb0efcffed3f5a57e794ee` | 2 条旧契约用例更新（gitignored，留盘） |
| `/tmp/t34_mut/run_mutations.sh` | — | 变异脚本 |
| `/tmp/t34_mut/verify_final.log` | — | 最终树 race + 全量测试原始输出 |
