# N2 实现报告：Admission 状态机与回放

日期：2026-10-27  
分支：`test`  
范围：只新增 `admission.go`、`admission_test.go`、`admission_replay_test.go`；未修改 `main.go`、`proxy_node.go`、`egress.go`、`shape_drift.go`。

## 1. 交付内容

### 1.1 `admission.go`

按 N2 任务实现，不依赖现有全局状态：

- `PairKey(model, egress)`：`lower(model) + "|" + egress`。
- `PairState`：`Strikes`、`Until`、`Trips24h`、`LastOK`。
- `EgressState`：`DirectUntil`、`Pairs`、`Sub`、`Inflight`、`ConsecFail`、`DeadUntil`、`Lifecycle`。
- `Admission`：`K`（默认 8）、`Clock`（默认 `Now()`）、`candidates`、per-egress channel semaphore、`probeActive`、`probing`、`probeBackoff`、`probeTimeout`。
- 对外 API：
  - `Admit(model string, exclude map[string]bool) (*Attempt, string)`
  - `AdmitProbe(model string) (*Attempt, string)`
  - `AdmitLifecycleProbe() (*Attempt, string)`
  - `Observe(at *Attempt, class OutcomeClass)`
  - `RetryAfter(model string) time.Duration`
  - `AllCandidatesCooling(model string) bool`
  - `AllSubQuarantined(model string) bool`
- `Attempt` 用 `sync.Once` 保证一次 `Observe`；`release` 也幂等。请求路径必须 `defer Observe(at, OutcomeAbandoned)`。
- 429 规则：首个 429 即 `strikes+1`，`until=now+ladder[strikes]`；ladder 为 60s/2m/5m/5m；任意 2xx 清 `strikes/until`；第 5 次连续 429 进入 24h hard block，hard 上限 16h，`Now >= HardUntil` 时重置。
- direct 双写：direct pair 的 `until` 同时写 `EgressState.DirectUntil`，其他 model 的 direct pair 通过 `DirectUntil` 被排除。
- `RetryAfter` 取所有候选的 `pair.until`、`sub.until`、`deadUntil`、`directUntil` 的最小未来值；全冷却返回 60s，并设置 `all_inflight` 原因。
- `AdmitProbe` 跳过 pair/sub cooldown，仍检查 dead/inflight；最多占一个 slot；全局 `probing` 单飞；429 只把 `until` 推到 `max(until, now+probe_backoff)`，不升 strikes/hard。
- `AdmitLifecycleProbe` 忽略 `deadUntil`，选 `lifecycle=="dead"` 的出口，成功由 `Observe` 正常恢复。
- 429 不触发 sub quarantine；403 才累计 sub，2 个不同 pair 或同 pair 2 次进入 10m sub quarantine；`429+2xx` 清 sub streak。

### 1.2 测试

`admission_test.go` 10 个单测：

`TestPairCoolOnFirst429`、`TestInflightAtomic`、`TestDirectCooldown`、`TestPairCoolNoCrossModel`、`TestAllCooling`、`TestAllSubQuarantined`、`TestProbeBypassesCooldown`、`TestProbeDeadline`、`TestHardBlockBounded`、`TestRetryAfter`。

`admission_replay_test.go` 4 个回放：

`TestReplay653e`、`TestReplay653eDeepseek`、`TestReplay41d`、`TestReplay4ad`。

fixture：`testdata/replay/pair_653e_mimo.jsonl`（1288 行）、`pair_653e_deepseek.jsonl`（5 行）、`pair_41d_mimo.jsonl`（402 行）、`pair_4ad_mimo.jsonl`（41 行）。

## 2. 回放结果（真实日志）

命令：

```bash
go test -race -count=1 -v -run 'Test(PairCoolOnFirst429|InflightAtomic|DirectCooldown|PairCoolNoCrossModel|AllCooling|AllSubQuarantined|ProbeBypassesCooldown|ProbeDeadline|HardBlockBounded|RetryAfter|Replay653e|Replay653eDeepseek|Replay41d|Replay4ad)$' .
```

实测：

```text
653e mimo sent429=4 sent2xx=162 suppressed=1122
653e deepseek sent429=0 sent2xx=5 suppressed=0
41d mimo sent429=3 sent2xx=0 suppressed=399
4ad mimo sent429=1 sent2xx=10 suppressed=30
PASS
ok  github.com/shiyanYX/opencode2api  1.098s
```

与 lead 的 5/4/1/0 对照：

| pair | lead | 本实现 | 差异 |
| --- | ---: | ---: | --- |
| 653e×mimo | 5 | 4 | 首个 429 同毫秒排序/窗口边界，差 1 |
| 41d×mimo | 4 | 3 | 同上，差 1 |
| 4ad×mimo | 1 | 1 | 一致 |
| 653e×deepseek | 0 | 0 | 一致 |

653e 日志实际是 182 个 2xx；回放中 20 个 2xx 落在 pair 冷却期，所以 `sent2xx=162`，总 2xx 仍为 182。差异不影响 `sent429 <= 8` 的验收线。

## 3. 变异测试（每个测试至少一个红）

用 `sed`/Python 临时改 `admission.go`，跑单测后恢复；14/14 全部变红：

| 测试 | 变异 | 结果 |
| --- | --- | --- |
| TestPairCoolOnFirst429 | 60s→61s | FAIL |
| TestInflightAtomic | `make(chan struct{}, a.K)`→`K-1` | FAIL |
| TestDirectCooldown | 删除 direct 写 `DirectUntil` | FAIL |
| TestPairCoolNoCrossModel | `PairKey(model, egress)`→`PairKey("*", egress)` | FAIL |
| TestAllCooling | 冷却判断改为始终生效 | FAIL |
| TestAllSubQuarantined | `sub_quarantine`→`no_candidate` | FAIL |
| TestProbeBypassesCooldown | probe 不跳过 pair cooldown | FAIL |
| TestProbeDeadline | 删除 20s timer | FAIL（2.01s） |
| TestHardBlockBounded | hard 上限 16h→30m | FAIL |
| TestRetryAfter | 忽略 `DeadUntil` | FAIL |
| TestReplay653e | 429 后不设 until | FAIL |
| TestReplay653eDeepseek | 2xx 后错误设 until | FAIL |
| TestReplay41d | 429 后清零 strikes 且不设 until | FAIL（402 次） |
| TestReplay4ad | 429 后不设 until | FAIL |

## 4. 验证命令

```bash
gofmt -l admission.go admission_test.go admission_replay_test.go
# 无输出

go vet ./...
# 无输出，exit 0

go test -race -count=1 -run 'Test(PairCoolOnFirst429|InflightAtomic|DirectCooldown|PairCoolNoCrossModel|AllCooling|AllSubQuarantined|ProbeBypassesCooldown|ProbeDeadline|HardBlockBounded|RetryAfter|Replay653e|Replay653eDeepseek|Replay41d|Replay4ad)$' .
# ok ... 1.098s
```

全量 `go test -race ./...`：首次 10m 默认超时；20m 复跑在 666s 失败，均为既有测试/既有代码：

- `WARNING: DATA RACE`：`proxy_node.go:2005 saveState` 与 `TestT30CriterionCPoolEmptyReachability`（`zz_t30_alert_safety_test.go:274`）并发读写；
- `TestT39RealLogOverviewCriterion` 的 `c_f1_zero_success_models_hidden_but_denominator_intact` 失败；
- `TestIP4RVDocsDrift` 失败（`zz_ip4agg_reverify_test.go:282`）。

这些路径不在 N2 改动范围（`admission.go`/两个测试文件），故不能通过修改 N2 修复。N2 定向 `-race` 14/14 全绿（1.098s）。

## 5. 未覆盖 / 边界

- N2 只交付 Admission 纯状态机；尚未接入 `main.go`/`proxy_node.go`，所以真实请求路径的 `Observe`/`defer release` 集成不在本切片。
- `AdmitProbe` 的 deadline 使用真实 `time.AfterFunc`，没有注入 clock；单测把 `probeTimeout` 调成 25ms，生产默认 20s。虚拟时间测试需后续注入。
- `probeMu + nextProbeAt` 的“全局单飞”在 Admission 内用 `probing` 实现；调度 probe 的 backoff 由调用方负责，Admission 不持有 `nextProbeAt`。
- 回放对同毫秒事件按文件顺序稳定排序；lead 的 5/4/1/0 与本实现 4/3/1/0 的 1 次差异来自此边界，验收线仍为 `<=8`。
- 全量套件在 `-race` 下存在既有竞态（`proxy_node.go:2005`）和既有 T39/IP4 失败；未修改既有测试/代码，避免越界。N2 交付以定向 `-race` 为验收证据。

## 6. 提交

提交到 `test` 分支，只包含 `admission.go`、两个测试文件（`.gitignore` 忽略 `*_test.go`，需 `git add -f`）和本报告。
