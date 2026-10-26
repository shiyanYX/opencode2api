# Task 2 报告：403 风暴遏制 + 换节点目标

状态：**实现与定向验证完成**；全量 `go test ./...` 仍受并行任务与既有 drift 测试影响（见 §7），不是本任务代码导致。

## 1. 范围与文件

- `proxy_node.go`
  - 订阅级 403 熔断、池级 403 halt、换节点候选过滤与排序。
- `main.go`
  - 两条 upstream retry loop（非流式 / 流式）的 403/429 切换点、halt fail-fast、确认侧订阅熔断、switch 事件埋点。
- 新增 `proxy_node_quarantine_test.go`（10 个测试）。
- 未修改 `egress.go`、`node_scoring.go`、`static/*`；未 push/deploy。

> `callLogEvent` 的 schema 只持久化 `Type/Node/NodeName/Detail/At`。为不改 `calllog.go`，新增的换节点元数据编码进 `Detail`：`reason=<...> candidates=<N>`；原 `freetier:probe_alt_node` 字面量保留，兼容既有源码不变量测试。

## 2. 实现要点

### 2.1 订阅级 403 熔断

- 订阅 key：`strings.SplitN(name, "::", 2)[0]`；无 `::` 时使用节点名。
- 只有 **confirm-then-mark 的确认侧** 才调用 `confirmSub403`：请求最终成功且版本一致、换到了不同节点，才把之前的 403 计为已确认。
- 10 分钟内 3 次已确认 403 ⇒ 隔离；连续触发退避 10 → 20 → 30 分钟，30 分钟封顶。
- `eligible()` 对隔离订阅返回 false，因此该订阅全部节点不可路由；隔离到期自动恢复，不修改节点状态/分数。
- 全部订阅都隔离时 `pick` 返回 nil；main 在 egress 后显式返回 503，不落入“直连 fallback”。

### 2.2 池级 403 halt

- 原始 FreeTier 403 通过 `note403` 进入 10 分钟窗口（**不要求确认**）；每次 egress 选择节点后 `noteAttempt` 记录被尝试节点作为分母。
- 判据：窗口内不同 403 节点数 ≥ `max(10, ceil(0.5 × 被尝试节点数))`。
- halt 持续 10 分钟：
  - `pick(true)` 直接返回 nil，不 mark 任何节点；
  - main 在 loop 顶、egress 后、403 分支三处 fail-fast 503；
  - 确认成功路径若处于 halt，则只记 `freetier_confirm_suppressed`，不 mark（硬约束 3）。
- `pick(false)` 的粘滞分支完全保留：即使 halt，非强制路径仍返回当前可用节点，不触发换节点。

### 2.3 换节点候选

- 仅对 `force=true` 的换节点候选做过滤/排序；粘滞路径不改。
- 排除与失败节点同 **/24（IPv4）** / **/64（IPv6）** 的候选；域名不参与排除。
- 若过滤后无候选，退回原候选集（不因同网段排除而把池子变空）。
- 排序（稳定）：近期成功（≤1h 且 `EffN≥3`）优先 → 评分降序 → 原顺序。
- switch 事件 `Detail` 追加 `reason` 与候选数：
  - `switch_after_fail`：403 换节点；
  - `switch_quota`：429/quota 换节点；
  - `sub_quarantine`：全订阅隔离 fail-fast；
  - `pool_halt`：池级 halt fail-fast。
  - `connect_error` 换节点同样追加 `reason=switch_after_fail candidates=<N>`。

## 3. 测试

新增 `proxy_node_quarantine_test.go`：

1. `TestSubQuarantineAfterThree403`（2 次不触发、第 3 次触发、到期恢复）
2. `TestQuarantineExponentialBackoff`（10/20/30 封顶）
3. `TestPoolHaltStopsSwitching`（halt 后 force 返回 nil、不 mark）
4. `TestRaw403DoesNotQuarantineSub`（原始 403 只进池窗口；确认侧才隔离）
5. `TestPoolHaltUsesHalfAttempts`（10/20 触发；10/100 不触发）
6. `TestAllSubsQuarantinedFailsFast`（fail-fast 且不清隔离）
7. `TestSwitchSkipsSameSubnet`（/24 排除；无候选退化为原行为）
8. `TestSwitchSkipsSameIPv6PrefixNotDomain`（/64 排除；域名不排除）
9. `TestSwitchPrefersRecentSuccess`（近期成功优先）
10. `TestStickyUnchanged`（`pick(false)` 粘滞；halt 下仍粘滞）

验证命令（与本任务相关）：

```bash
go test -run 'TestSubQuarantine|TestQuarantine|TestPoolHalt|TestRaw403|TestAllSubs|TestSwitch|TestSticky' -count=1 ./
go test -run 'TestFT' -count=1 ./
```

结果：通过。

## 4. 变异验证（全部先 RED，再恢复）

对 `proxy_node.go` 逐项变异，使用文件级编译运行上述测试：

| 变异 | 结果 | 捕获测试 |
|---|---|---|
| `sub403Threshold 3 → 1` | RED | `TestSubQuarantineAfterThree403` |
| halt 判据改 100%（`threshold := attempted`） | RED | `TestPoolHaltUsesHalfAttempts` |
| 删除同 /24 排除（`sameNodeSubnet` 恒 false） | RED | `TestSwitchSkipsSameSubnet` |
| `pick(false)` 改轮询（禁用粘滞分支） | RED | `TestStickyUnchanged` |

每次变异后均已恢复，定向测试重新转绿。

## 5. 10-24 日志回放

数据：`.research/scoring2` 的 10-24 日志。

- 403 事件：2,183 次，全部来自 `红杏`。
- 不同 403 节点：**4 个**（日本02 1024、香港01 982、日本01 148、新加坡01 29）。
- 600 秒订阅冷却可避免：**2,105 / 2,183 = 96.4%**。
- 节点级 90.9%（Q1b 口径）。

池级 halt 回放：

- 按任务书口径（不同 403 节点数 ≥ max(10, 50% 尝试节点)），84 个含 403 的 10 分钟桶中 **0 个触发**（0.0%）。原因是只有 4 个不同 403 节点，低于硬下限 10；10-24 的集中冲击因此由**订阅级熔断**兜底，而不是池级 halt。
- 计划中引用的 **96.7%** 来自 Q3 的“10 分钟窗口 ≥50% 被尝试节点失败（任意失败）”统计，不是“不同 403 节点数”口径；两者不可直接互换。本报告不把 96.7% 冒充池级 403 halt 触发率。

## 6. 关键行为核对

- C1 粘滞：`pick(false)` 未改；单测覆盖无失败不换节点。
- C2 确认式 403：原始 403 只计池窗口；订阅隔离只在成功确认侧产生。
- C3 halt：pick 不 mark、main 不换节点/不发新请求、成功确认也抑制 mark。
- C4 换节点目标：同 /24、/64 排除；域名不排除；无候选退化。
- 现有 `TestFT_*` 源码不变量与 confirm-then-mark 测试通过。

## 7. 全量验证与已知阻塞

`go vet ./...`：通过。

`go test -count=1 ./...` 本次未全绿，失败项与本任务无关：

- `TestScoringParityWithReferenceScores`：T1（`node_scoring.go`）并行改动中的测试/实现未对齐。
- `TestIP4RVLiveServedDrift`、`TestIP4RVDocsDrift`：工作区既有 docs/live drift；基线（未改本任务代码前）已同样失败。

补充：排除上述并行/既有失败后，`go test -skip 'TestScoringParityWithReferenceScores|TestIP4RV' -count=1 ./...` **通过（208.9s）**；本任务相关测试与 `TestFT` 也单独通过。

`main.go` 改动较多，建议 Lead 在 T1 合入后统一跑一次全量。

## 8. 风险 / 待决策

1. **计划口径冲突**：10-24 只有 4 个不同 403 节点，按硬性 `max(10, ...)` 永远不会触发池级 halt；计划同时要求“池级 halt 提前触发 96.7%”无法用该口径复现。若产品希望 10-24 这类单订阅风暴也触发池级 halt，需要改判据（例如按 403 事件数/订阅数），这需要用户/Lead 决策，不能在本任务内擅改。
2. `lastSwitchCandidates` 是“最近一次 pick”的近似值，用于事件埋点；高并发下 candidate 数可能被其它请求覆盖，不影响选路/熔断。
3. 订阅隔离目前只影响 `nodePool` 选路，未写入节点评分 view 的 `usable` 字段；若下游依赖评分 view 的 usable，需要 T1/T3 接口对齐。
4. halt/隔离均为进程内状态，不持久化；重启后从零开始（与现有池状态恢复边界一致）。
