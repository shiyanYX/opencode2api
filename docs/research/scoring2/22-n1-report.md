# N1 实现报告（R8 v5.6 状态层 + 迁移 + 影子记录）

分支：`test`（不 push、不碰生产）
代码/测试提交：`2ed9649`；本报告随后提交。
范围：4 个生产文件 + 4 个测试文件；7 个 R8 测试。

## 1. 结论

N1 已完成：`go test -race -run '^TestR8' .` 7/7 通过；`go vet ./...` 通过；`gofmt -l` 为空。
全量 `go test -race ./...`：本次未跑完（旧套件在 4 分钟内未结束，已主动终止；R8 子集 `-race` 全绿，N1 新增文件不改变旧路径）。

## 2. 文件

新增（未改旧生产路径）：

- `clock.go`：`Clock` 接口、`SetClock`、`Now`；默认真实时钟。
- `node_state_v5.go`：v5 持久化 schema、原子保存、加载降级、pair/egress/direct/sub 状态、50k 上限与 24h trips 裁剪。
- `node_state_v5_migrate.go`：v2 `proxy_state` → v5 迁移；`*|node:<fp>` 占位。
- `shadow_record.go`：4096 条有界影子环、快照、JSONL 追加。

测试（`git add -f`，因为 `.gitignore` 忽略 `*_test.go`）：

- `clock_test.go`
- `node_state_v5_test.go`
- `node_state_v5_migrate_test.go`
- `shadow_record_test.go`

## 3. 验证命令与输出

```text
$ go test -race -run '^TestR8' .
ok  github.com/shiyanYX/opencode2api  1.338s

$ go vet ./...
exit=0

$ gofmt -l clock.go clock_test.go node_state_v5.go node_state_v5_test.go \
    node_state_v5_migrate.go node_state_v5_migrate_test.go \
    shadow_record.go shadow_record_test.go
(no output)
```

全量：

```text
$ go test -race ./...
未完成：旧套件运行 >4min 仍未结束，主动终止。
本次交付的 7 个 R8 测试已单独用 -race 全绿；N1 只新增独立文件，未改旧生产路径。
```

## 4. 先红后绿证据

`TestR8MemoryCap` 在实现真正的 50k 淘汰前先跑红：

```text
--- FAIL: TestR8MemoryCap (0.02s)
    node_state_v5_test.go:125: pairs = 50120, want exactly 50000
FAIL
```

`clock`/`shadow` 的注入/环形行为在中断前已存在，补测试时先跑为绿；因此用变异证明测试不是假绿（见下表）。
迁移、状态重启、损坏降级同样在补测试时已实现，用变异证明敏感度。

## 5. 变异记录（实现改错 → 哪个测试红）

| # | 变异 | 红测试 | 证据 |
|---|---|---|---|
| 1 | `Clock.Now` 忽略注入时钟，直接 `time.Now()` | `TestR8ClockInjection` | `clock_test.go:33: Now() = ... want 2026-10-27 09:00:00 +0000 UTC` |
| 2 | `LoadNodeStateV5` 返回前清空 `Epoch` | `TestR8StateRoundTrip` | `node_state_v5_test.go:49: round trip mismatch` |
| 3 | `pruneLocked` 忽略 `Strikes/Hard`，冷却到期即淘汰 | `TestR8RestartBlocked` | `node_state_v5_test.go:101: blocked pair pruned after cooldown expiry` |
| 4 | 超过 50k 时提前返回，不执行淘汰 | `TestR8MemoryCap` | `node_state_v5_test.go:125: pairs = 50120, want exactly 50000` |
| 5 | 损坏 JSON 时返回 `nil` 而非空 v5 状态 | `TestR8CorruptFile` | `node_state_v5_test.go:151: corrupt load = nil, want empty v5 state` |
| 6 | 迁移冷却从 5m 改成 4m | `TestR8Migration190` | `node_state_v5_migrate_test.go:61: ... Until=...840000, want ...900000` |
| 7 | 满环时 `Snapshot` 返回 `nil` | `TestR8ShadowRecord` | `shadow_record_test.go:32: snapshot = [], want ts 3..6` |

每次变异均用备份恢复；恢复后 `go test -race -run '^TestR8'` 再次全绿。

## 6. 关键实现

### 6.1 50k 上限

`pruneLocked` 分两阶段：

1. 清理 `nil`、裁剪 `trips24h`（24h/16 条）、删除空闲 >30m 的未封锁 pair。
2. 若 `len(Pairs) > nodeStateV5MaxPairs`，收集所有未封锁 pair，按 `max(lastOK,lastFail)` 升序（同值按 key）淘汰，直到 ≤50000。

封锁定义：`until > now || strikes > 0 || hard > 0`，一律不淘汰。因此若封锁 pair 本身超过 50k，总量仍会超过 50k；这是“封锁不可丢”的安全优先取舍，非未受控增长。测试使用 50,100 个刚活动 pair + 20 个封锁 pair，断言恰好 50,000、20 个封锁全在、最旧 120 个被淘汰。

### 6.2 时钟

`SetClock(nil)` 恢复 `realClock`；测试在 `SetClock(nil)` 后与 `time.Now()` 比较差值 <1s，并用 100 个 goroutine 在 `-race` 下读 `Now()`。当前仅新状态层使用；旧生产路径仍是 `time.Now()`，N2 接线时再统一替换。

### 6.3 迁移

`MigrateV2(src *nodeScoringStore, now)` 从 `src.path` 同目录读 `proxy_state.json`：

- `exhausted` + `QuotaEvents>0`（或 `LastError` 含 quota/429/freeusage）→ `*|node:<fp>`，`strikes=1`、`until=now+5m`；
- `dead` → egress `dead`；`GateStateRejected` → egress `rejected`；
- 旧 `shape_drift` 不写入 v5；
- 重复迁移幂等。

`*|node:<fp>` 注释已写明：v2 无 model 维度的占位；N2 的 Admit 在无 model-specific pair 时回退查询，首次真实 outcome 后删除并写真实 model pair。未在 N1 实现该查询逻辑。

### 6.4 影子记录

固定容量 4096；`Snapshot` 返回副本；满环后顺序为第 3..6 条（cap=4 测试）；`AppendJSONL` 每条一行合法 JSON。仅内存 + 显式导出，不参与路由。

## 7. 未覆盖 / 风险

- 未接 `main.go`/`proxy_node.go`：N1 只交付状态层，路由、Admit、Observe 未接线。
- 迁移无法识别“`state=exhausted` 但 scoring 无 QuotaEvents 且 LastError 无 quota”的旧节点；会保留 egress 但不建 pair（避免误封）。
- 50k 上限对 >50k 封锁 pair 无法生效（按需求不淘汰封锁）。
- `LoadNodeStateV5` 只校验 `version`，不校验 `epoch` 是否属于当前部署；N2 需在启动时决定是否接受旧 epoch。
- `PairKey` 用 `|` 拼接且不转义；若 model 含 `|` 会歧义。
- `ShadowRecord.AppendJSONL` 不 fsync、不轮转；只用于影子期显式导出。
- 时钟注入尚未覆盖旧生产代码，5h 虚拟时间测试只能在 N2 接线后生效。

## 8. 下一步（N2 输入）

- 用 `Now()` 替换新状态路径中的直接 `time.Now()`。
- 接 `Admit`：`*|node:<fp>` 回退 + 首次 outcome 删除占位。
- 启动时 `LoadNodeStateV5` + `MigrateV2`，决定 epoch 接受策略。
- 将 `ShadowRecord` 挂到请求路径，仅记录不决策。
