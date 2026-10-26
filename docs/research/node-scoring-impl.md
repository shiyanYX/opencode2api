# 节点打分后端实现（task-67）

> 权威规格：`docs/research/node-scoring-spec.md`（§0 公式 / §2 输入 / §3 七个坑 / §4 生产回算 / §5 前端契约）。
> 本文只记录「规格 → 代码 → 证据」，不复述规格。

## 0. 结论

| 项 | 结果 |
| --- | --- |
| `/api/nodes` 每节点追加字段 | `score` `tier` `usable` `quality` `effective_sample` `sample_size` `low_confidence` `score_reasons` `probe_latency_ms` `last_success_at` `last_traffic_at` `last_error` `fail{connect,cred,http5xx,timeout,stream}` `quota_events` `caps` `probe_fail_24h` `dead_marks_7d` `trend[{ts,score,probe_ok}]`（≤20 轮） |
| 顶层追加 | `computed_at`、`scoring{version, params}` |
| 与回算 `scores.json` 对账 | 219 节点**最大分差 0.000，>2 分的节点 0 个**，档位分布 优53/良41/观察34/停用91 与 §4.3 完全一致（`TestScoringParityWithReferenceScores`） |
| `/api/nodes` 耗时（219 节点、含 JSON 编码） | min 1.09ms / **p50 1.38ms** / p95 6.86ms（预算 <50ms，`TestT67APINodesLatency219`） |
| `go vet ./` | 干净（无输出） |
| `gofmt -l`（本任务 5 个文件） | 空 |
| `go test -race -run 'Node\|Scoring'` | ok（4.37s） |
| 落盘 | `scoring_state.json`（探测环形缓冲 20 轮、衰减累加量、24h/7d 探测计数），30s debounce 原子写；重启不丢 |
| 未改契约 | 未改 `/api/stats`、`/api/logs/*`、`web/`、`static/`；未 git |

新文件：`node_scoring.go`（1470 行，打分/归因/持久化/回填全在此）、`node_scoring_test.go`（12 测试）、`zz_t67_sample_dump_test.go`（样本与耗时证据生成器）。
改动：`proxy_node.go`（探测写点 4 处 + `last_used_at` 补写 2 处，+20 行）、`main.go`（`/api/nodes` 视图与顶层字段 + `initNodeScoring`，+71/−17 行）。

## 1. spec 章节 → 代码位置

| spec 章节 | 代码位置 |
| --- | --- |
| §0 公式（60% 质量下界 + 20% 探测 + 20% 延迟，再硬封顶） | `node_scoring.go:234` `scoreNode`（raw 在 262 行，封顶 265-285） |
| §3.1 Wilson 单侧 95% 下界（z=1.645） | `node_scoring.go:88` `scoringWilsonLB` |
| §3.2 证据 7 天半衰期衰减 | `node_scoring.go:517` `scoringStats.decayTo`（增量累加，O(1) 等价于逐条 `0.5^(age/7)`）；历史回填按证据年龄加权 `node_scoring.go:705`-760 |
| §3.3 探测只封顶不抬分 / dead×0.30 / probe_24h≤65 | `node_scoring.go:234`-285 |
| §3.4 失败归因（节点坏 1.0 / 上游抖 0.5 / 额度不计 / 非节点错剔除） | `node_scoring.go:628` `classifyScoringEvent`、`:651` `classifyScoringUpstream`、`:705` `observeCallRecordLocked` |
| §3.5 池级抖动抑制（10min 窗口失败率≥50% ⇒ 失败按 20% 计责） | `node_scoring.go:598` `bucketLocked` + `:705` 内 `blip` 判定 |
| §3.6 池内先验 α0=8/β0=2、冷启动 no_data 兜底观察档 | `node_scoring.go:67` `defaultScoringParams`、`:277` no_data 分支 |
| §3.7 档位（优≥85/良70-84/观察50-69/停用<50） | `node_scoring.go:117` `scoringBand`（对外只给英文枚举） |
| §4.2 生产回算口径（每节点最后一条事件、ok 无事件记最后节点、switch 不覆盖） | `node_scoring.go:705` |
| §4.6 小样本收缩 / 对账 | `TestScoringParityWithReferenceScores`（`node_scoring_test.go:213`） |
| §5 前端字段契约 | `node_scoring.go:199` `nodeScoreView`；`main.go:7264` `nodeAdminView` 内嵌；`main.go:7291` 打分、`:7343` 顶层 `computed_at`/`scoring` |
| §5.4 探测历史环形缓冲 20 轮 + 持久化 | `node_scoring.go:827` `observeProbe`、`:1032` `saveLocked`、`:1059` `load` |
| 探测数据写入点 | `proxy_node.go:1108` `markProbeDead`（4 处分支）、`:1162` `recordProbeSuccess` |
| `last_used_at`（此前从未写入） | `proxy_node.go:445` `markUsedLocked`，调用点 `:403` `:412` `:439` `:1580` |
| 启动/回填/轮询 | `node_scoring.go:1165` `backfillFromLog`、`:1252` `pollCallLog`、`:1300` `start`、`:1343` `initNodeScoring`（`main.go:7676` 调用） |

## 2. 测试清单（`node_scoring_test.go`，12 个测试 / 22 个用例）

| 测试 | 钉住什么 |
| --- | --- |
| `TestScoringWilsonMatchesSpecShrinkageTable` | §4.6 收缩表 3/3→62.5%、12/12→75.9%、40/40→88.6%、200/200→97.2%、2000/2000→99.7%；无先验 3/3=52.6% |
| `TestScoringLatencyComponent` | 延迟分段：未知(-1)→0.45、1500→1.0、5750→0.55、10000→0.10 |
| `TestScoringBandThresholds` | 四档边界（85/70/50），英文枚举 + 中文映射 |
| `TestScoringParityWithReferenceScores` | **219 节点逐节点对账**（用 `node_stats.json` 未四舍五入的 eff_s/eff_n + `scores.json` 状态），比对 score/quality/raw/tier/caps/原因码顺序/usable/low_confidence/auto_eligible/probe_latency 归一化；最大分差 0.000，>2 分 0 个，档位分布 53/41/34/91 |
| `TestScoringAttributionClassifiesEvents`（10 子用例） | connect_error→1.0、403→cred 1.0、403 FreeTier→额度不计、503→0.5、429→额度、400→剔除、stream_interrupt→0.5、ok 无事件记最后节点、connect_ok、switch 不覆盖 |
| `TestScoringBlipSuppression` | 10min 窗口 5 条全失败 ⇒ `blip_fails>0`、失败只背 20% 锅、eff_n 不丢 |
| `TestScoringProbeHistoryRingBufferPersists` | 25 轮只留 20 轮、首尾 ts 正确、连续成功/失败计数、落盘后新 store 读回一致 |
| `TestScoringStatePersistenceRoundTrip` | attempts/success/fail/eff_s/eff_n/last_success_at 重启不丢 |
| `TestScoringAPINodesContract` | `/api/nodes` 字段齐全、老字段保留、空值归一（-1→null、零值时间→null）、tier 英文枚举、裁定 4 禁止字段不出现、reason 带 code/level/text |
| `TestScoringLastUsedAtWrittenOnPick` | pick 后 `last_used_at` 非零 |
| `TestScoringSnapshotUnder50ms` | 219 节点 × 200 条证据打分 <50ms（实测 1.7ms） |
| `TestScoringBackfillFromCallLog` | 从 call_log 全量回填、按证据年龄衰减、增量 since 过滤 |

TDD 红证据（实现前）：`go vet .` → `vet: ./node_scoring_test.go:24:7: undefined: defaultScoringParams`；实现后 `go test -run 'TestScoring' ./` → `ok ... 0.055s`。

## 3. 与 `.research/scoring/scores.json` 对账

**方法**：`scores.json` 的 `eff_s/eff_n` 只有 1 位小数（直接用会让 Wilson 下界漂移 ~0.002），所以输入取
`.research/scoring/node_stats.json` 的原始聚合（`eff_s/eff_n/attempts/success/fail/blip_fails/probe_fail_24h/dead_marks_7d`），
节点状态字段（`state/latency_ms/last_probe_at/last_error/cooldown_until`）取 `scores.json`，
`ref=ref_ts`（stale_evidence 用）、`log_now=generated_at`（探测新鲜度用）。

**结果**：219/219 节点 score 完全一致（最大分差 **0.000**，>2 分 **0 个**），质量、raw、tier、caps、原因码顺序全部一致；档位分布 优53/良41/观察34/停用91 与 spec §4.3 一致。

**手算样例**（验收方独立手算可直接对）：

| 节点 | 输入 | 手算 | 回算 |
| --- | --- | --- | --- |
| `110b6af8…`（优） | eff_s=122.8603, eff_n=139.6364, probe=1.0, lat=0.997 | q=wilson(130.86,149.64)=0.823221；raw=100×(0.6×0.8232+0.2+0.2×0.997)=89.333 → **89.3** | 89.3 ✓ |
| `024221f0…`（观察） | eff_s=15.0956, eff_n=28.6999, probe=1.0, lat=0.881 | q=wilson(23.10,38.70)=0.464898；raw=65.514 → **65.5** | 65.5 ✓ |
| `011631f7…`（停用） | eff_s=2.4437, eff_n=2.8197, dead, probe=0, lat=0.45 | q=wilson(10.44,12.82)=0.588574；raw=44.314；dead×0.30 → **13.3** | 13.3 ✓ |

**运行时预期差异（同 fp 分差 >2 的节点应能被下面两条解释）**：

1. **探测计数冷启动**：`probe_fail_24h`/`dead_marks_7d` 从部署时刻开始累计（探测历史不进 call_log）。
   回算里只有 **3 个**节点吃 `probe_24h≤65` 封顶，若计数缺失最多上浮 **16.2 分**（均值 12.0）——部署后 24h 内自动收敛。
2. **衰减参考时刻不同**：回算在 `ref_ts` 打分，运行时在 `now` 打分。用 `ref_ts` 证据在 `+9.35h` 后重算：
   最大分差 **0.2**，>2 分 **0 个**，档位变化 **0 个**。
3. 其余：`call_log` 14 天保留 vs 回算 90 天窗口（7 天半衰期下 14 天前证据权重≤0.25，只轻微降低 eff_n）；
   在线抖动窗口按「已见记录」判定、批处理用整批 pass1（边界记录可能差一次计责，影响 <0.1 分）。

## 4. 与前端契约对齐的 5 条裁定（Lead 2026-10-21 裁定，已落地并写进测试）

1. **`tier` 用英文枚举** `excellent|good|watch|bad`，中文只在前端映射 → 不返回 `tier_label`（`nodeScoreView` 里已改 `json:"-"`；`TestScoringAPINodesContract` 钉住）。
2. **`trend[]` 用 `[{ts,score,probe_ok}]`**，最多 20 轮（不是 spec 早期草案的 `{ts,ok,latency_ms}`）；环形缓冲照做、落盘、重启不丢 → `observeProbe` + `TestScoringProbeHistoryRingBufferPersists`。
3. **`score` 对外 1 位小数**（0–100，排序要精度）；「不显示小数」是 UI 规则，后端不取整、不加 `score_exact`。
4. **`caps[]` 保留**；不加 `p50_ms`/`p95_ms`/`auto_eligible`/`stale`（`auto_eligible` 仍需内部判定，故保留结构体字段但 `json:"-"`；`TestScoringAPINodesContract` 断言这些字段不出现）。
5. **归一化在后端做**：`probe_latency_ms`: `-1`→`null`；`last_probe_at`/`last_success_at`/`cooldown_until`/`last_error`: 零值/空→`null`（`main.go` 视图层改为 `*string`）。

额外裁决（spec 内部冲突）：`exhausted`（额度冷却）按 §3.4 视为 `usable=false` 并附 `quota_cooldown` 原因，而非 §5.1 的「非 dead 即可用」。

## 5. 脱敏 `/api/nodes` 响应样本

生成方式：`go test -run TestT67SampleDump ./` → `/tmp/t67_api_sample.json`（4 个合成节点：优/观察/停用/新节点未探测；真实响应由 `TestT67APINodesLatency219` 在 219 节点上验证）。节选两个节点 + 顶层：

```json
{
  "computed_at": "2026-10-21T15:19:41+08:00",
  "scoring": {
    "version": 1,
    "params": {"prior_a":8,"prior_b":2,"z":1.645,"w_q":0.6,"w_p":0.2,"w_l":0.2,
               "lat_full":1500,"lat_floor":10000,"dead_factor":0.3,"cap_probe24":65,
               "stale_probe_min":45,"half_life_days":7,"blip_factor":0.2}
  },
  "healthy": 3, "exhausted_count": 0, "dead_count": 1,
  "nodes": [
    {
      "name": "示例-优-东京", "fingerprint": "fp-example-…", "protocol": "socks5",
      "address": "10.0.0.1", "port": 1080, "state": "available", "active": false, "manual": false,
      "latency_ms": 1200, "last_probe_at": "2026-10-21T15:19:41+08:00",
      "score": 90.5, "tier": "excellent", "usable": true,
      "quality": 0.8412, "effective_sample": 25, "sample_size": 25,
      "low_confidence": false, "caps": [],
      "probe_latency_ms": 1200, "last_success_at": "2026-10-21T15:19:41+08:00",
      "last_traffic_at": "2026-10-21T15:19:41+08:00", "last_error": null,
      "fail": {"connect":0,"cred":0,"http5xx":0,"timeout":0,"stream":0},
      "quota_events": 0, "probe_fail_24h": 0, "dead_marks_7d": 0,
      "score_reasons": [
        {"code":"real_traffic_good","level":"good","text":"真实调用成功率下界 84%（有效样本 25）"},
        {"code":"fast","level":"good","text":"探测延迟 1200 ms，快"}
      ],
      "trend": [{"ts":"2026-10-21T15:19:41+08:00","score":90.5,"probe_ok":true}]
    },
    {
      "name": "示例-停用-汉堡", "state": "dead", "latency_ms": -1,
      "cooldown_until": "2026-10-21T15:34:41+08:00",
      "score": 12.4, "tier": "bad", "usable": false, "quality": 0.5408,
      "effective_sample": 0, "sample_size": 0, "low_confidence": true,
      "caps": ["dead×0.30"],
      "probe_latency_ms": null, "last_success_at": null, "last_traffic_at": null,
      "last_error": "probe: Get \"https://opencode.ai/zen/v1/models\": timeout",
      "fail": {"connect":0,"cred":0,"http5xx":0,"timeout":0,"stream":0},
      "quota_events": 0, "probe_fail_24h": 1, "dead_marks_7d": 1,
      "score_reasons": [
        {"code":"probe_dead","level":"high","text":"探测判死：Get … timeout",
         "evidence":{"cooldown_until":"2026-10-21T15:34:41+08:00"}},
        {"code":"low_sample","level":"mid","text":"有效样本不足（n_eff=0 < 20），置信度低"}
      ],
      "trend": [{"ts":"2026-10-21T15:19:41+08:00","score":12.4,"probe_ok":false}]
    }
  ]
}
```

新节点（无调用、无探测）样本：`score=51.4, tier=watch, usable=true, caps=["no_data→观察"], effective_sample=0, low_confidence=true, probe_latency_ms=null, last_probe_at=null, trend=[]`（不诬告成停用，§3.6）。

## 6. 硬证据（2026-10-21 15:2x 实测）

```
$ go vet ./
（无输出）

$ gofmt -l node_scoring.go node_scoring_test.go zz_t67_sample_dump_test.go proxy_node.go main.go
（无输出）

$ go test -count=1 -run 'TestScoringSnapshotUnder50ms' -v ./
    node_scoring_test.go:770: 219 节点打分耗时 1.722106ms（预算 50ms）

$ go test -count=1 -run 'TestT67APINodesLatency219' -v ./
    zz_t67_sample_dump_test.go:158: 219 节点 /api/nodes 端到端：min=1.088074ms p50=1.382627ms p95=6.858488ms max=6.858488ms（预算 50ms）

$ go test -race -count=1 -run 'Node|Scoring' ./
ok  	github.com/shiyanYX/opencode2api	4.367s

$ go test -count=1 -run 'TestScoringParityWithReferenceScores' -v ./
    node_scoring_test.go:326: 对账通过：219 节点，最大分差 0.000，分差>2 的节点 0 个，档位分布 map[bad:91 excellent:53 good:41 watch:34]
```

全量 `go test` 中相邻线已知失败（与本任务无关，均为他线未提交改动）：`TestMV2_Wiring_SourceShape`（static/admin.html 面板契约）、
`TestIP4RVLiveServedDrift`/`TestIP4RVDocsDrift`（IP4 聚合的「残留风险 R-A/R-B 复现」漂移探针）。隔离快照
（`static/admin.html` 取 HEAD、其余含本任务改动）下 `go vet ./` 干净、`-race -run 'Node|Scoring'` 绿、全量仅剩上述 IP4 两个已知探针失败。

## 7. 实现取舍与残留风险

**取舍**
- 不编辑 `calllog.go`（当时正被 task-56 并发修改）：实时归因走 `callLog.latest(500)` 5s 轮询 + 启动时从 call_log 磁盘回填；`lastSeen` 时间戳持久化，重启只补停机缺口，回填与轮询以启动时刻 `cutoff` 为界不重复计数。
- 衰减用「增量累加器」（EffS/EffN + EffRef），每加一条证据前先 `decayTo(ts)`，数学上等价于逐条 `0.5^(age/7)` 求和，避免每轮全量重算。
- `scoring_state.json` 原子写（tmp+rename）、30s debounce（task-55 写放大教训）；探测失败/判死时间序列 7 天窗口，24h/7d 计数由时间序列推导而非墙钟计时器 ⇒ 重启不丢、无需定期清零。
- `/api/nodes` 不建缓存层：219 节点纯内存打分 p50 1.4ms，缓存只会引入一致性成本和「computed_at 不新鲜」的解释负担。
- `exhausted` 视为不可用（§3.4 额度冷却语义），与 §5.1「仅 dead 不可用」冲突处以 §3.4 为准并在此记录。

**残留风险**
1. 首次部署后 **≤24h** 内 `probe_fail_24h` 计数偏低（最多 3 个节点、≤16.2 分），24h 后自愈；`dead_marks_7d` 同理需 7 天填满（只展示，不参与打分）。
2. `trend` 在部署后逐步填充（健康检查 15min/轮 ⇒ 约 5h 满 20 轮）；重启不丢但不会追平部署前历史。
3. 停机 >7 天的节点：探测时间序列被裁剪，`probe_fail_24h`/`dead_marks_7d` 归零（dead 状态本身仍由 `proxy_state.json` 恢复并继续封顶）。
4. 单实例假设：`scoring_state.json` 无跨进程锁；多副本共享同一数据目录会互相覆盖。
5. 节点改名/换指纹：统计按指纹聚合，改名只影响展示名（旧统计可能成为孤儿条目，不回填）。

## 8. 交付物与哈希

| 文件 | sha256 |
| --- | --- |
| `node_scoring.go`（新，1470 行） | `c94e944cf9f5e90f440673b25851aa04b45dda22c0646c7937bae58e2d3d281d` |
| `node_scoring_test.go`（新，12 测试） | `1815b845a906b53a4ebb3e19525cd25eccac99e772f4ddddb87f18afc078a010` |
| `zz_t67_sample_dump_test.go`（新，样本/耗时证据） | `2864d165c143d2b2f917990917661613fabcc3ba37e3bc2714d484cc5fd912ef` |
| `proxy_node.go`（改，+20 行） | `41cfd73c7c6915194398bd0b73bafdf1d5f86b7b108f372b5c966aa736be2cad` |
| `main.go`（改，+71/−17 行） | `712f11f6bc64a2fa8d3ea69351ffe6c0b8a59f6399426697639d6275bf10fced` |

> 说明：`*_test.go` 被 `.gitignore` 忽略是本仓库既有约定（测试留盘、不提交）；`node_scoring.go` / `proxy_node.go` / `main.go` 为源码交付。未执行 git commit。
