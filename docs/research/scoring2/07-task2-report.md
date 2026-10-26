# Task 2 报告：403 风暴遏制 + 换节点目标

状态：**R1–R5 评审修复完成，定向测试/变异验证通过**；全量回归见 §7（当前工作区仍受其它任务并行改动影响）。本轮**未提交 git**。

## 1. 范围与文件

- `proxy_node.go`
  - 订阅级 403 熔断、池级 403 halt、换节点候选过滤与排序、R2/R3/R5 修复。
- `main.go`
  - 两条 upstream retry loop（非流式 / 流式）的 403/429 切换点、halt fail-fast、确认侧抑制顺序、switch 事件埋点。
- `calllog.go`
  - `CallEvent` 新增 `switch_reason` / `switch_candidates` 两个独立 JSON 字段（R4）。
- `proxy_node_quarantine_test.go`：13 个 T2 测试。
- `zz_freetier_confirm_test.go`：新增 halt 期间成功确认的集成测试（仅本地测试，`.gitignore` 忽略 `*_test.go`）。
- 未修改 `egress.go`、`node_scoring.go`、`static/*`；未 push/deploy/commit。

## 2. 实现要点

### 2.1 订阅级 403 熔断（R1）

- 订阅 key：`strings.SplitN(name, "::", 2)[0]`；无 `::` 时使用节点名。
- 只有 **confirm-then-mark 的确认侧** 才调用 `confirmSub403`：请求最终成功且版本一致、换到了不同节点，才把之前的 403 计为已确认。
- 10 分钟内 3 次已确认 403 ⇒ 隔离；连续触发退避 10 → 20 → 30 分钟，30 分钟封顶。
- `eligible()` 对隔离订阅返回 false，因此该订阅全部节点不可路由；隔离到期自动恢复，不修改节点状态/分数。
- 全部订阅都隔离时 `pick` 返回 nil；main 在 egress 后显式返回 503，不落入“直连 fallback”。

### 2.2 被迫换节点：排除当前订阅（R1 核心修复）

- 之前 10-24 实测 403 后 91.2% 的下一跳仍落在同一订阅，导致 confirmed 永远只有 1 次、订阅熔断 0 触发。
- `switchCandidatesLocked` 现在先做：
  1. 排除 `subKeyOf(n.Name) == subKeyOf(from.Name)` 的候选；**若排除后仍有其它订阅候选才采用**。
  2. 再排除与失败节点同 `/24`（IPv4）/`/64`（IPv6）的候选；若过滤后为空，退化到上一步候选集。
  3. 如果跨订阅候选为空，才退化为原候选（同订阅兄弟可兜底）。
- 排序保持：近期成功（≤1h 且 `EffN≥3`）优先 → 评分降序 → 原顺序。

### 2.3 池级 403 halt（R3）

- 原始 FreeTier 403 通过 `note403` 进入 10 分钟窗口（**不要求确认**）；每次 egress 选择节点后 `noteAttempt` 记录被尝试节点作为分母。
- 判据改为（R3 评审裁定）：

  ```text
  threshold = max(8, min(24, ceil(0.35 * attemptedNodesInWindow)))
  ```

  整数实现：`(attempted*35 + 99) / 100`，再夹到 `[8,24]`。
- 10-24 只有 4 个不同 403 节点、窗口内最多 89 个被尝试节点 ⇒ threshold=24，**池级 halt 预期 0 触发**；这类单订阅形态风暴由 2.2 的订阅熔断兜底。
- halt 持续 10 分钟：
  - `pick(true)` 直接返回 nil，不 mark 任何节点；
  - main 在 loop 顶、egress 后、403 分支三处 fail-fast 503；
  - 确认成功路径若处于 halt，只记 `freetier_confirm_suppressed`，不 mark、不记订阅 strike。
- `pick(false)` 的粘滞分支完全保留：即使 halt，非强制路径仍返回当前可用节点，不触发换节点。

### 2.4 确认侧顺序（R2）

- 新增 `freeTierConfirmAllowed(now)`，在**任何确认/mark/订阅 strike 之前**依次判定：
  1. `poolHalted` → `pool_halt`
  2. `shapeSuspectNow()` → `shape_latch`
  3. `availableCount() <= freeTierFloor()` → `pool_floor`
- 只有返回 `allowed=true` 才执行 `markFreeTierRejected` 与 `confirmSub403`；被抑制时订阅 strike **完全不增加**。
- 成功侧事件仍为 `freetier_node_confirmed_bad`；抑制侧为 `freetier_confirm_suppressed`，detail 为 `suppressed:<reason>`。

### 2.5 熔断退避窗口重置（R5）

- `sub403State` 增加 `last403`。
- 距上次 confirmed 403 `> 30m` ⇒ `escalations` 与 `strikes` 归零，重新从 10m 起算；10m 窗口逻辑不变。
- `TestQuarantineExponentialBackoff` 增加“31m 静默后回到 10m”断言。

### 2.6 switch 事件结构化字段（R4）

- `CallEvent` 新增：

  ```go
  SwitchReason     string `json:"switch_reason,omitempty"`
  SwitchCandidates int    `json:"switch_candidates,omitempty"`
  ```

- 新增 `callLogSwitchEvent(ctx,node,detail,reason,candidates)` / `callRecorder.switchEvent`，`Detail` 不再拼接 `reason=`、`candidates=`。
- `main.go` 两条循环共 18 个 switch 调用点全部改为结构化赋值：
  - `switch_after_fail`：403 probe、connect/TTFT、upgrade/revert；
  - `switch_quota`：429/quota；
  - `sub_quarantine`：全订阅隔离；
  - `pool_halt`：池级 halt。

## 3. 测试

`proxy_node_quarantine_test.go`（13 个）：

1. `TestSubQuarantineAfterThree403`（2 次不触发、第 3 次触发、到期恢复）
2. `TestQuarantineExponentialBackoff`（10/20/30 封顶；31m 静默重置）
3. `TestPoolHaltStopsSwitching`（halt 后 force 返回 nil、不 mark；快照在触发前）
4. `TestRaw403DoesNotQuarantineSub`（原始 403 只进池窗口；确认侧才隔离）
5. `TestPoolHaltThresholdClamp`（下限 8：10 attempted/7 403 不触发、8 触发；上限 24：100 attempted/23 不触发、24 触发）
6. `TestAllSubsQuarantinedFailsFast`（fail-fast 且不清隔离）
7. `TestSwitchSkipsSameSubscription`（同订阅兄弟被排除；无其它订阅时退化）
8. `TestSwitchSkipsSameSubnet`（/24 排除；无候选退化）
9. `TestSwitchSkipsSameIPv6PrefixNotDomain`（/64 排除；域名不排除）
10. `TestSwitchPrefersRecentSuccess`（近期成功优先）
11. `TestSuppressedConfirmDoesNotStrike`（halt / shape_latch / pool_floor 下 strike 与 mark 均不变）
12. `TestSwitchEventStructuredFields`（Go 字段与 JSON 字段均独立）
13. `TestStickyUnchanged`（`pick(false)` 粘滞；halt 下仍粘滞）

`zz_freetier_confirm_test.go` 新增：

- `TestFT_HaltDuringSuccess_NoMarkNoStrike`：第二次尝试 200 成功前把池置 halt ⇒ 不 mark 第一个 403 节点、不记订阅 strike，并记录 suppressed 事件；可捕获 M6/M8。

定向验证命令：

```bash
go test -run 'TestSubQuarantine|TestQuarantine|TestPoolHalt|TestRaw403|TestAllSubs|TestSwitch|TestSuppressed|TestSticky|TestFT' -count=1 ./
```

结果：**ok（约 50s，含编译/测试）**。

## 4. 变异验证（全部 RED，逐项恢复后转绿）

| 变异 | 结果 | 捕获测试 |
|---|---|---|
| M1 `sub403Threshold 3 → 1` | RED | `TestSubQuarantineAfterThree403` |
| M2 halt 判据改 100%（`threshold := attempted`） | RED | `TestPoolHaltThresholdClamp` |
| M3 删除同 /24 排除（`sameNodeSubnet` 恒 false） | RED | `TestSwitchSkipsSameSubnet` |
| M4 禁用 `pick(false)` 粘滞分支 | RED | `TestStickyUnchanged` |
| M5 halt 触发当刻 mark 节点 | RED | `TestPoolHaltStopsSwitching`（触发前快照） |
| M6 删除 `freeTierConfirmAllowed` 的 `poolHalted` 守卫 | RED | `TestFT_HaltDuringSuccess_NoMarkNoStrike` |
| M7 删除同订阅排除 | RED | `TestSwitchSkipsSameSubscription` |
| M8 把 `confirmSub403` 提前到抑制判定之前 | RED | `TestFT_HaltDuringSuccess_NoMarkNoStrike` |
| M9 删除 30m 静默重置 | RED | `TestQuarantineExponentialBackoff` |
| M10 switch 事件不写 reason/candidates | RED | `TestSwitchEventStructuredFields` |

M1–M3、M5–M7、M9、M10 为定向测试失败；M4 为禁用粘滞分支后测试失败；M8 为 FT 集成测试失败。每次变异均用备份恢复，恢复后定向测试重新通过。

## 5. 10-24 日志回放

数据：`/tmp/scoring2/call_log/2026-10-24.jsonl`（只读）。口径 = terminal `http 403` + `freetier:probe_alt_node` 隐式 403。

### 5.1 403 总量

| 指标 | 数值 |
|---|---|
| 403 总量 | **4,405**（terminal 2,183 + switch 2,222） |
| 订阅分布 | 全部 `红杏`（4,405） |
| 不同 403 节点 | **4**（日本02 2,020 / 香港01 2,003 / 日本01 317 / 新加坡01 65） |
| 含 403 的请求 | 2,244 / 2,476 |
| 403 后下一跳同订阅 | **1,967 / 2,156 = 91.2%** |

### 5.2 实现口径回放

| 口径 | confirmed | 订阅熔断触发 | 被隔离后本可避免的 403 |
|---|---:|---:|---:|
| 旧实现（当前生产日志实测） | **1**（0.02%） | **0** | **0** |
| R1 严格观测链（仅统计日志中 403 后异订阅且最终成功的请求） | 1 | 0 | 0 |
| **R1 反事实上界**（假设 403 后换到异订阅健康节点并成功） | **120** | **40** | **4,285 / 4,405 = 97.28%** |

反事实模型说明（避免把假设当实测）：

- 逐条按日志时间回放 403；未隔离时，R1 会排除当前订阅，换到异订阅候选。日志中异订阅 `1::BR…` 有 59 次尝试 / 52 次最终成功，说明“换到健康异订阅可成功”有现实基础；但日志中**实际** 403 后的异订阅尝试 0 次成功（大多超时/429），所以严格观测链无法证明 R1 后的真实成功数。
- 反事实表假设每个未隔离 403 请求换到异订阅健康节点后成功 ⇒ 计 1 次 confirmed；同一订阅 10 分钟内 3 次 confirmed ⇒ 触发隔离（10/20/30 分钟退避，30 分钟封顶，30 分钟无新 403 重置）。
- 40 次触发共确认 120 次；隔离期间的红杏 403 计为“本可避免”，共 4,285。退避分布：10m ×14、20m ×13、30m ×13。
- 该表用于证明 R1 能把 confirmed 从 1 抬起来并使熔断真正触发；**不是**生产观测值，落地后应以真实 confirmed/触发计数复核。

### 5.3 池级 halt

- 按 R3 公式，10-24 窗口内不同 403 节点最大 **4**，被尝试节点最大 **89** ⇒ threshold = min(24, ceil(31.15)) = **24**，实际 4 < 24 ⇒ **0 触发**。
- 旧报告中的 **96.4%** 是“terminal 403 + 600s greedy 同订阅冷却”口径，不是实现口径；实现口径的 confirmed 基线是 **1/4,405 = 0.02%**，订阅熔断 **0 触发/0% 覆盖**。本轮不再把 96.4% 当作 T2 熔断覆盖率。

## 6. 关键行为核对

- C1 粘滞：`pick(false)` 未改；单测覆盖无失败不换节点。
- C2 确认式 403：原始 403 只计池窗口；订阅隔离只在成功确认侧产生。
- C3 halt：pick 不 mark、main 不换节点/不发新请求、成功确认也抑制 mark 与 strike。
- C4 换节点目标：先排除同订阅，再排除同 /24、/64；域名不排除；无候选退化。
- C5 事件：`switch_reason` / `switch_candidates` 为独立字段；旧 `Detail` 拼接已删除。
- 现有 `TestFT_*` 源码不变量与 confirm-then-mark 测试通过。

## 7. 全量验证与已知阻塞

- `go vet ./...`：**通过**。
- `go test -skip 'TestIP4RV' -count=1 ./...`：**ok，193.063s**。
- `gofmt -l`（本任务及同步维护的测试文件）：**无输出**。
- T3 把 `nodeAlerts.recordNodeAttempt` 的表达式改成 `err == nil || isFirstByteTimeout(err)` 后，旧 C5 源码不变量 `zz_c5_verify_wiring_test.go` 的精确字符串匹配失效（全量唯一失败）。本轮只把该本地测试的匹配改成前缀 `nodeAlerts.recordNodeAttempt(nodeFp != "",`，仍要求出现 2 次，**不改生产逻辑**；随后全量通过。
- 已知环境噪声：`TestIP4RVLiveServedDrift`、`TestIP4RVDocsDrift` 为既有 docs/live drift，按验收命令用 `-skip TestIP4RV` 排除；全目录 `gofmt -l` 会命中 `.gocache/.modcache`。

## 8. 风险 / 待决策

1. **反事实回放不是生产实测**：R1 后的真实 confirmed 需要用新代码上线后的 call_log 复核；当前 10-24 日志无法直接观测“换到异订阅后成功”。
2. `lastSwitchCandidates` 是“最近一次 pick”的近似值；高并发下 candidate 数可能被其它请求覆盖，不影响选路/熔断。
3. 订阅隔离只影响 `nodePool` 选路，未写入评分 view 的 `usable` 字段；若下游依赖评分 view 的 usable，需要 T1/T3 接口对齐。
4. halt/隔离均为进程内状态，不持久化；重启后从零开始（与现有池状态恢复边界一致）。
5. 10-24 池级 halt 仍为 0 触发是 R3 公式的预期结果；它只负责形态级风暴，单订阅风暴由 2.2 的换节点排除 + 订阅熔断负责。
