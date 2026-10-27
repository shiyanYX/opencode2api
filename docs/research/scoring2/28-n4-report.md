# N4 实施报告（v5.7 §13）

日期：2026-10-27
分支：`test`
状态：**实现完成，单元/回放级验证通过；未做生产流量验证**

## 1. 提交

| 提交 | 内容 |
|---|---|
| `5e64827` | N4a：pair 30m/2h/8h/24h/72h 阶梯、半开 `okStreak`、v5 状态持久化 |
| `c9bcee3` | N4b/N4c：全池 winter gate、NoCandidate、attempt 上限、旧状态写入旁路 |
| 本文件提交 | N4 验证报告 |

## 2. N4a：429 阶梯 + 半开 + 持久化

### 实现

- `admission.go:pairCooldownLadder` 改为 `30m, 2h, 8h, 24h, 72h`，第 5 次后封顶 72h；删除 60s/2m/5m 与独立 hard-block 递增。
- `PairState.OKStreak`（JSON `okStreak`）：
  - 正常 2xx：清零 `strikes/until/okStreak`；
  - `until` 到期后的半开 2xx：`okStreak++`，连续 2 次才清零；半开期间 `pairPosterior` 乘 `0.05`；
  - 半开期间再次 429：`strikes` 进入下一档。
- 不再从路由调用 `AdmitProbe`；pair 冷却到期直接以 0.05 权重重新参与 `Admit`。
- `node_state_v5.go` 新增 `PairState.OKStreak` 与顶层 `GateState`；`NodeStateV5.Gate` 随 pairs/subs/direct 一起保存。
- `node_system_v5.go` 启动时 `loadNodeStateV5("/data/v5_state.json")`；损坏/版本不符则空状态并 `slog.Warn("v5_state_load_failed")`。force 的 `Observe` 后 `markDirty`，`persistIfDue` 2s 防抖写临时文件 + rename。

### 测试

```bash
GOMODCACHE=$PWD/.modcache GOPROXY=off \
  go test -run 'TestPairLadder|TestV5StatePersist' -race -count=1 .
```

结果：`ok  github.com/shiyanYX/opencode2api`

覆盖：
- 首 429 = 30m；
- 到期后第一次 2xx 仍为半开、第二次恢复；
- 半开 429 进入 2h；
- 完整恢复后重新从首档开始；
- 保存/重载 `strikes/until/okStreak` 一致。

## 3. N4b：全池闸 + NoCandidate + attempt 上限

### 实现

- `GateState`：`active/pausedUntil/backoff/lastTrip/egresses/nextCanary/canaryOK/nodeProbeUsed`。
- `admission.go`：
  - 5 分钟窗口按 egress 去重记录 429；第 3 个不同 egress 触发 gate；
  - 首次暂停 60s，连续触发翻倍，封顶 15min；安静 10min 后 backoff 重置；
  - gate 激活后普通 `Admit` 路径由 `v5AdmitContext` 拦截；
  - 每 60s 只允许 direct canary；direct 2xx 后才允许一次 `AdmitGateProbe`（忽略 pair 冷却，但仍受 sub/inflight 约束）；节点 2xx 才清 gate；
  - `GatePaused` 不因 `PausedUntil` 到期自动放行，只由成功 canary 链解除。
- `egress.go`：`EgressResult.NoCandidate`；force 无租约时返回 `NoCandidate`，**不回退直连**；`Get` 的 force 分支不再调用 direct fallback。
- `main.go`：两个上游循环检测 `NoCandidate`，返回 HTTP 429 + `Retry-After: 60`，`callLogSuppressedAttempt(..., "v5_winter")`；force 的 `maxAttempts/loopBudget` 强制为 2（`v5AttemptLimit`）。
- 事件：`v5_winter`（reason `gate_paused`/`canary_*`/`node_probe_*`），`v5_fallback` 保持 W1 语义。

### 测试

```bash
GOMODCACHE=$PWD/.modcache GOPROXY=off \
  go test -run 'TestGateTrips|TestNoReburnWithin10m|TestNoCandidateNoDirect|TestMaxTwoAttempts' -race -count=1 .
```

结果：`ok  github.com/shiyanYX/opencode2api`

覆盖：
- 3 个不同 egress 的 429 触发 gate；gate 内额外 429 不重新触发/不翻倍；
- `PausedUntil` 前 canary 为 `canary_wait`；60s 后 direct canary 2xx 解锁 node probe；node 2xx 清 gate；
- 无候选返回 `NoCandidate` 且 `Direct=false`；
- force `v5AttemptLimit()==2`，off 返回 0。

## 4. N4c：force 下旁路旧写入

- `main.go` 两个 2xx 分支的 `proxyPool.recordNodeSuccess`、`quotaHaltGuard.observeSuccess`、`shapeDriftObserveSuccess` 均加 `!v5LegacyGatesBypassed()`。
- 两个 direct 429 分支的 `quotaHaltGuard.observeFailure`、`quotaHaltGuard.halt`、`shapeDriftObserve429` 整块在 force 下不进入。
- 旧 `shapeDrift` 决策与 `blockedEgress` 读取也在 force 下旁路；force 只由 v5 `Observe` 写 pair/egress/gate。

## 5. 全量验证

```bash
GOMODCACHE=$PWD/.modcache GOPROXY=off go test -run 'TestV5' -race -count=1 .
# ok  github.com/shiyanYX/opencode2api

GOMODCACHE=$PWD/.modcache GOPROXY=off go vet ./...
# VET_EXIT=0

GOMODCACHE=$PWD/.modcache GOPROXY=off go test -count=1 ./...
# EXIT=1; only pre-existing unrelated failures:
#   TestIP4RVLiveServedDrift
#   TestIP4RVDocsDrift
```

全量日志：`/tmp/n4-full2.log`；耗时 282s。IP4RV 两测在本轮改动前即失败，不属于 N4。

## 6. 变异（全部红）

| 变异 | 失败测试 |
|---|---|
| M1 阶梯首级改 60s | `TestPairLadder` |
| M2 gate 阈值 3→99 | `TestGateTrips`、`TestNoReburnWithin10m` |
| M3 NoCandidate 改回直连 | `TestNoCandidateNoDirect` |
| M4 attempt 上限 2→11 | `TestMaxTwoAttempts` |
| M5 持久化不落盘 | `TestV5StatePersist` |
| M6 direct canary 直接清 gate | `TestGateTrips` |
| M7 force 旁路判定关闭（旧状态写入路径恢复） | `TestV5ForceBypassHelper`（连带 force 测试） |

原始输出：`/tmp/n4-mutations.log`。

## 7. 未覆盖 / 风险

- 未做真实生产 30min 影子/force 演练；canary 与 gate 的 60s 节奏只在 fake clock 单测验证。
- `v5_state.json` 损坏事件目前是 `slog.Warn`，不是 call_log 事件；若面板要求可见，需要补 `callLogEvent` 启动期入口。
- 旧写入旁路只有 guard 级测试与变异，没有端到端“force 2xx 不提升 probation”的请求级测试。
- `pairPosterior ×0.05` 的半开权重未单独断言数值，仅通过选路/冷却测试间接覆盖。
- 全量测试仍受两个既有 IP4RV 漂移测试阻塞，不能声称 `go test ./...` 全绿。

## 8. 结论

N4a/N4b/N4c 已完成并通过目标单测、race、vet 与 7 个变异；**可进入下一阶段**；进入生产前仍需影子/force 演练与上述未覆盖项。
