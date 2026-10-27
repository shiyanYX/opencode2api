# N4 Lead 复核（v5.7）

复核对象：`5e64827` / `c9bcee3` / `b944d87`。我亲自跑了：

- `go vet ./...` → 0
- N4 目标测试 `-race` → ok
- 全量 `go test -count=1 ./...` → 277s，仅既有 `TestIP4RVLiveServedDrift` / `TestIP4RVDocsDrift` 失败，无 DATA RACE

实现与报告一致，阶梯/半开/持久化/旁路都成立。但闸门有三个设计缺陷，其中两个会导致新的自伤停摆。

## B1（critical）触发条件缺失败率，正常时段会误停全池

现状：5 分钟内 3 个不同 egress 出现 429 即触发。

实测 377 个 5 分钟窗口：**38 个（10.1%）**满足该条件；其中：

- 10-26 18:00：94 请求 / 2 失败（2%），distinct=3
- 10-26 19:55：150 请求 / 3 失败（2%），distinct=4

这两段是正常流量，按现规则会全池 429 60 秒、影响 244 个请求。真正风暴窗口的失败率是 83%–100%。

修法：触发改为「≥3 个不同 egress **且** 窗口失败率 ≥50%（样本 ≥10 请求）」。

## B2（critical）闸只能靠 direct canary 退出，direct 挂了就永久 429

若 direct（本机出口）持续 429 而节点已恢复，canary 永远失败 → 没有节点探测 → 闸永不解除。这与旧全局闩是同一种自伤。

修法：闸激活满 10 分钟后，每 5 分钟允许一次节点探测（不需要 canary 成功）；探测 2xx 即解闸。

## B3（critical）节点探测非 429/2xx 时状态机卡死

`AdmitGateProbe` 先把 `NodeProbeUsed=true`。若探测返回 403/5xx/超时/客户端取消（`Observe` 走 `other`），`observeGateOutcomeLocked` 没有对应分支，`CanaryOK` 保持 true、`NodeProbeUsed` 保持 true：

- `GateCanaryDue()` = false（CanaryOK 为 true）
- `GateNodeProbeReady()` = false（NodeProbeUsed 为 true）

两个入口都关死，闸永久卡住。修法：探测的任何非 2xx 结果都视为失败，重置 `CanaryOK/NodeProbeUsed` 并排下一次 canary。

## B4（minor）`v5Winter` 日志洪泛

闸内每个被拦请求都写一条 `v5_winter`，3 rps 时是每分钟 180 条。按 `v5Fallback` 的做法同 reason 每分钟最多一条。

## 附注

- `AdmitGateProbe` 注释写 “probe=true”，实际 `attemptLocked(..., false, false, false, ...)`。行为可接受（429 走阶梯），改注释即可。
- `NoCandidate` 路径统一记 `v5_winter`，冷却耗尽时语义不准，可顺手改成 `v5_no_candidate`。
