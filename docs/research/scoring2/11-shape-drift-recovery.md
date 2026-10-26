# T6 / task-114：429 形态漂移闩死修复

> 状态：Stage 1 实现完成，定向测试通过；Stage 2（半开/全局节流/交互矩阵）实现完成，尚待累计变异与评审。
> 最后更新：2026-10-27 00:10 +08（每跑完一轮验证会原地更新本节）。
> 未 commit；测试文件为本地文件。

## 0. 当前状态快照

| 项 | 状态 / 数字 |
| --- | --- |
| `shape_drift.go` 首次时间 / TTL / model 分桶 / 卡片上限 | 已实现 |
| 任一真实 2xx 清闩 | 已实现（非流式 + 流式） |
| 半开试探 / 抖动 / 全局 5s 限速 / 69 集合排除 / 60→120→300 退避 | 已实现（Stage 2 草案） |
| 定向测试 | 16 个（含旧守卫 2 个）全绿；目标 14 个全绿（见 §5） |
| 全量测试 | 首轮 `go test -skip TestIP4RV -count=1 ./...` 仅 `TestScoringSnapshotUnder50ms` 因并发负载 89ms>50ms 失败；单测隔离重跑 3 次全绿。待复跑 |
| 变异 | Stage 1 待跑；Stage 2 累计待跑 ≥8 |
| 18:00–23:00 回放 | 已跑，见 §7 |

## 1. 事故链（2026-10-26）

1. 上游把「形态被拒」伪装为 429 `FreeUsageLimitError`，与真额度耗尽无法区分。
2. 旧 `shapeDriftSeen` 每次 429 都覆盖同一出口的时间戳；被 active 出口钉住的请求持续 429，就持续刷新自己。
3. 旧解除条件只有「窗口滑空」。刷新使窗口永不空 ⇒ 闩永久不放；`main.go` 的 `if haltDrift { break }` 让请求不再换出口，失败请求 `nodes` 长度恒为 1。
4. 18:00–23:00 的 12,286 条请求中 11,268 条是 429 失败（91.7%），其中 11,263 条只尝试了 1 个出口；20:00 小时 4,255/4,260 失败，21:00、22:00 全部失败。日志里 `shape_drift` 事件在 18:03:58 与 20:00:08 两次确认。
5. 恢复只能靠人工切节点。根因不是「检测太晚」，而是「arm 后没有任何自愈路径」。

## 2. Stage 1：最小止血设计

- **首次时间**：`seen[egress]` 只在该出口第一次 429 时写入；重复 429 不刷新。窗口过期删除条目。
- **硬 TTL**：`armedAt + 10min` 无条件清闩，即使窗口内仍有 429 记录。这是对「持续坏出口把窗口撑满」的兜底。
- **真实 2xx 清闩**：非流式/流式成功路径都调用 `shapeDriftObserveSuccess(model, egress)`；任一出口成功立即清空该 model 的闩与 429 集合。
- **按 model 分桶**：deepseek 与 mimo 的配额窗口不同步，互不清闩、互不 arm。
- **客户端 model 名护栏**：最多 512 个桶；超出淘汰最久未活跃桶；空闲 30min 清理；限频 `slog.Warn`，避免随机 model 名打爆内存或日志。
- **时钟跳变**：检测到 now 早于已记录时间时重置该 model，避免「未来时间」把闩永久保护。
- **为什么足够止血**：即使成功路径完全不可用，10min TTL 也会强制释放；即使一个坏出口持续打，首次时间不再刷新，窗口会按真实首次时间滑空。两条独立释放路径覆盖了旧实现的单点。

## 3. Stage 2：半开试探与惊群控制

- armed 后按 model 每 60s 允许一次单出口试探；试探出口从该 model 的 429 集合中排除。
- 试探成功 ⇒ 立即清闩；试探再 429 ⇒ 退避 60→120→300s，上限 300s。
- 退避上叠加 0–20% 随机抖动（测试可注入恒等函数）。
- 全局限速：所有 model 合计最多每 5s 放行一次试探，防止同一毫秒一堆请求一起换出口。
- 候选排除接入 `EgressRequest.ExcludeEgress` → `pickWithMetaExcluding` / `pickForRegionMetaExcluding`；强制与普通选路都过滤。
- **最坏每小时试探次数**：
  - 全局绝对上限：3600/5 = **720 次/小时**。
  - 单 model 在 10min TTL 内，60s→120s→300s 最多 3 次试探；TTL 清闩后重新 arm，理论 **18 次/小时/model**。
  - 实际线上只有 2 个 free model；即使两桶都长期 armed，约 **36 次/小时**，远低于旧事故的 9,000+ 429。

## 4. 方案选择（为什么不是别的）

| 方案 | 结论 | 原因 |
| --- | --- | --- |
| A. 只把时间戳改成首次 + 只加 TTL | 止血可用，但不恢复 | 10min 内仍然完全停切；对持续坏出口只能等 TTL，恢复太慢；且无法利用真实 2xx 证据。 |
| B. 首次时间 + TTL + 任一 2xx 清闩（Stage 1） | **采用为最小上线版** | 改动面小、两条独立释放路径、无试探带来的额外 IP 风险；测试能覆盖根因。 |
| C. 直接删除守卫 | 否决 | 旧问题会回来：形态漂移时把坏请求散布到 N 个出口，按每 IP ~16 次累积封禁。 |
| D. 全局固定冷却（如 5min 内完全停切） | 否决 | 一个 model/一个坏出口会拖住所有 model；无成功证据时恢复靠猜。 |
| E. 按出口 IP 冷却、不按 model | 否决 | deepseek 与 mimo 配额独立；一个 model 的 429 会误伤另一个 model 的可用出口。 |
| F. 半开试探 + 抖动 + 全局限速（Stage 2） | **采用为恢复层** | 用最少的额外请求换取主动验证；全局 5s 限速把最坏试探压到 720/h；429 集合排除避免反复撞同一批出口。 |
| G. 持久化闩状态到磁盘/重启继承 | 否决 | 事故闩本身就是错误状态；重启应清空而不是继承。Stage 1 测试确认重启后单出口 429 不继承旧闩。 |
| H. 把 model 名完整保留、不设上限 | 否决 | OpenAI 兼容入口的 model 由客户端控制；随机 10 万 model 名会无界增长。采用 512 上限 + 空闲清理 + 淘汰告警。 |

## 5. 测试

本地文件：`shape_drift_recovery_test.go`（14 个测试）。

Stage 1 对应：
- `TestShapeDriftArmsAfterThreeDistinctAndDoesNotRefreshFirstSeen`
- `TestShapeDriftHardTTLDisarms`
- `TestShapeDriftTTLBoundaryPlusMinus1ms`
- `TestShapeDriftModelIsolation`
- `TestShapeDriftModelNameCardinalityCap`
- `TestShapeDriftConcurrentBurst`
- `TestShapeDriftClockJumps`
- `TestShapeDriftResetAfterRestart`
- `TestShapeDriftIdleModelCleanup`
- `TestPickExcludesShapeDriftBlockedEgress`

Stage 2 对应：
- `TestShapeDriftHalfOpenSingleTrialAndBackoff`
- `TestShapeDriftTrialSuccessDisarms`
- `TestShapeDriftTrialJitter`
- `TestShapeDriftTrialGlobalRateLimit`

定向运行（2026-10-27 00:10）：

```bash
go test -run 'TestShapeDrift|TestPickExcludesShapeDrift' -count=1 -v ./
# ok github.com/shiyanYX/opencode2api 0.174s
# 14 个目标测试全部 PASS（另 2 个旧 shape 测试也 PASS）
```

日志：`.research/scoring2/task114_targeted_stage1.log`。

## 6. 变异

待填。

## 7. 18:00–23:00 回放

数据：`/tmp/cl-1026.jsonl`（15:16–23:15，共 5.2MB）。窗口 18:00–23:00 共 12,286 条：
- 429 失败 11,268，其他失败 6，成功 1,012。
- 失败中 11,263 条只尝试 1 个出口（91.67% 的失败）。
- 20:00 小时 4,255/4,260 失败；21:00 936/936、22:00 961/961 失败。20:00 后共 6,152 条 429 失败，6,151 条只尝试 1 个出口。
- 日志中 `shape_drift` 事件：18:03:58、20:00:08；`switch` 事件共 85 次。

回放脚本：`.research/scoring2/task114_replay.py`，输出：`.research/scoring2/task114_replay.json`。

候选口径（全部是反事实上界，不声称该节点当时健康）：
- 同 model 成功出口，且**不同订阅**（T2/R1 同订阅排除）；
- 5min / 30min 内有过成功 / 整个窗口有过成功。

结果：
- 新守卫在回放中**未 arm**：19:59:59 最后一次成功后清闩；20:00 后每个 model 只有 2 个不同失败出口，未达 3 个阈值。因此 11,268 条 429 都保持普通换节点资格。
- 其中 5min 内有不同订阅成功出口：**375 条（3.33%）**；30min 内：**3,123 条（27.72%）**；整个窗口内：**6,155 条（54.62%，乐观上界）**。
- 按旧全局闩回放，18:12 左右 arm；旧闩下可换请求约 2 条（0.018%）。实际日志的 arm 时间是 18:03:58，回放因只按记录最终节点近似而晚约 9 分钟。
- 结论：Stage 1 的「成功即清闩 + 首次时间 + TTL」直接让 worst 2 小时不再闩死；Stage 2 的半开在本次事故窗口里甚至没有触发（未达 3 个 distinct 失败出口），它的价值是未来 3+ 出口同时 429 时仍能恢复。

## 8. 未决

- 全量测试复跑（首轮仅 perf 测试受并发负载影响）。
- Stage 1 变异 ≥5、累计 Stage 2 ≥8，全部 RED。
- T6a/T6b 评审回来后的 Critical 项。
- 与 T2 halt / quota_halt 的交互矩阵测试。
