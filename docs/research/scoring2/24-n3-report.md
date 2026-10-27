# N3 选择层实现报告

范围：新增 `selection.go`、`selection_test.go`；修改 `admission.go` 的排序、衰减、在途计数与 `AdmitPreferred`；N2 的 `TestInflightAtomic` 仅调整测试前置状态（见 §4）。未改 `main.go`、`proxy_node.go`。

## 1. 实现

### 1.1 后验表示

`PairState.A/B` 继续保存**观测证据**，不把先验写进持久化计数；读取后验时加 `a0=8,b0=2`：

```text
p = (A+a0+0.5)/(A+B+a0+b0+1)
n = A+B
```

这样 N2 的既有计数语义（一次 2xx 后 `A==1`）不变，同时新 pair 的后验仍是 `8.5/11=0.772727...`。衰减只作用于证据：

```text
f = 0.5^(Δt/7d)
A = A*f, B = B*f
```

先验 `a0/b0` 不衰减；`LastDecay==0` 时只置位，不衰减。`Observe` 在写 `A/B` 前先调用 `decayPairLocked`，所以“闲置 7 天后一次 2xx”是 `0.5*旧n+1`，不是“先加后衰减”。

### 1.2 排序与冷启动

- `n<5`：`n` 升序 → `LastOK` 降序 → egress 升序；同一 egress 同时只允许 1 个 `explore` 在途。
- `n>=5`：`UCB=p+0.05*sqrt(2*ln(max(N,2))/n)` 降序；tie `LastOK` 降序 → `LastFail` 升序（0 最早）→ 成熟并列组按 `rr` 轮询。
- `prefer` 非空且在资格候选内：直接选该 pair（冷启动时同样受单在途限制）；被冷却/隔离/排除时忽略 prefer，回到正常排序；prefer 可拿名额但名额已满返回 `all_inflight`。
- 冷启动候选全部被 `exploreInflight` 占满时，允许退到成熟候选；仍无名额则 `all_inflight`。

### 1.3 API

```go
func (a *Admission) AdmitPreferred(model string, exclude map[string]bool, prefer string) (*Attempt, string)
func (a *Admission) Admit(model string, exclude map[string]bool) (*Attempt, string) // 委托 prefer=""
```

## 2. 测试

新增 9 个 `selection_test.go` 测试：

- `TestColdStartPrefersFewestAttempts`
- `TestColdStartSingleInflight`
- `TestUCBExploresLowConfidence`
- `TestUCBPrefersHighPosterior`
- `TestTieBreakLastOK`
- `TestDecayEvidenceTowardPrior`
- `TestDecayDoesNotShrinkPrior`
- `TestDecayAppliedBeforeIncrement`
- `TestStickyPreferred`

N2 19 项与 N3 9 项合并运行：

```bash
go test -race -count=1 -run 'Test(PairCoolOnFirst429|InflightAtomic|DirectCooldown|PairCoolNoCrossModel|AllCooling|AllSubQuarantined|ProbeBypassesCooldown|ProbeDeadline|HardBlockBounded|RetryAfter|DeadExpiresToProbation|ProbationNeedsThreeSuccesses|Sub403WindowExpires|Probe403RefreshesQuarantine|DirectImmuneToLifecycle|Replay653e|Replay653eDeepseek|Replay41d|Replay4ad|ColdStartPrefersFewestAttempts|ColdStartSingleInflight|UCBExploresLowConfidence|UCBPrefersHighPosterior|TieBreakLastOK|DecayEvidenceTowardPrior|DecayDoesNotShrinkPrior|DecayAppliedBeforeIncrement|StickyPreferred)$' .
# ok ... 1.119s，28/28
```

`gofmt -l` 无输出；`go vet ./...` 无输出。

## 3. 与给定测试数值的两处算术修正

1. **p 不可能严格不变**：`a=a0+(a-a0)f,b=b0+(b-b0)f` 下，`p=(a+.5)/(a+b+1)` 的距离比是 `f*(S+n+1)/(S+nf+1)`，不是 `f`。例：`n=100,S=10,f=.5`，`p .545045 -> .565574`，不是 `±1e-9` 不变。测试因此按 `TestDecayEvidenceTowardPrior` 验证：`n` 减半、`|p-p0|` 单调下降，并用上式校验比值；100 天仍达不到 `|p-p0|<1e-6`，所以 70 天那条改用 200 天（`n<0.01` 且 `|p-p0|<1e-6`）。
2. **p=0.50,n=5 在先验 8/2 下不可达**：`n=5` 时最小后验是 `A=0,B=5` 的 `8.5/16=0.53125`。`TestUCBExploresLowConfidence` 用 `e2 p=0.53125,n=5`，仍验证 UCB 会选低置信候选；`e1 p=0.55,n=100`。

## 4. N2 测试前置状态调整

N3 规定冷启动同一 egress 只能 1 个在途，而 N2 `TestInflightAtomic` 原来在 `n=0` 上断言 `8/24`。为了让该测试继续验证“成熟 pair 的 K=8 原子名额”，测试前置写入 `A=100,B=50` 的成熟 pair；断言仍是 `admitted=8, all_inflight=24`。冷启动单在途由 N3 的 `TestColdStartSingleInflight` 覆盖。这是测试场景调整，不是 N2 行为回归。

## 5. 变异表

对 `selection.go`/`admission.go` 临时变异，运行对应测试后恢复：

| 变异 | 测试 | 结果 |
| --- | --- | --- |
| UCB 探索系数 `0.05→0` | TestUCBExploresLowConfidence | FAIL（选 e1，未选 e2） |
| 冷启动 `n` 升序改降序 | TestColdStartPrefersFewestAttempts | FAIL（选 e2） |
| 去掉 `exploreInflight` 单在途检查 | TestColdStartSingleInflight | FAIL（第二次仍放行） |
| 衰减 `f` 改为 1 | TestDecayAppliedBeforeIncrement | FAIL（51/50，期望 26/25） |
| prefer 从 `cands` 而非 `eligible` 取 | TestStickyPreferred | FAIL（冷却 e2 仍被选中） |

5 个变异全部红；恢复后 28/28 绿。

## 6. 未覆盖/边界

- 本轮只实现选择层，不接入 `main.go`；因此没有真实请求的端到端证据。
- 全量 `go test -race ./...` 仍受既有 `proxy_node.go:2005` 竞态与 T39/IP4 失败阻断，非 N3 改动。
- `exploreInflight` 只在同一 `(model,egress)` 的冷启动路径计数；不同 model 的同一 egress 是否共享“冷启动单在途”是下一层接线时需要确认的口径。
