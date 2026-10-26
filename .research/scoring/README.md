# .research/scoring —— 节点打分回算脚本与原始输出

> 配套文档：`docs/research/node-scoring-spec.md`（打分模型设计规范）。
> 纯回算，不改生产代码；数据为生产只读拷贝。

## 数据（不在此目录，体积大）

| 文件 | 来源 | 说明 |
|---|---|---|
| `/tmp/scoring/call_log.jsonl` | `docker cp opencode2api:/data/call_log.jsonl` | 119MB / 233,692 行，2026-08-13 → 2026-10-20 |
| `/tmp/scoring/proxy_state.json` | `docker cp opencode2api:/data/proxy_state.json` | 219 节点最新状态快照 |
| `/tmp/scoring/opencode2api.log` | `docker cp opencode2api:/data/opencode2api.log` | 72MB，探测/状态变更/轮次日志 |

重新生成数据：

```bash
mkdir -p /tmp/scoring
docker cp opencode2api:/data/call_log.jsonl      /tmp/scoring/call_log.jsonl
docker cp opencode2api:/data/proxy_state.json    /tmp/scoring/proxy_state.json
docker cp opencode2api:/data/opencode2api.log    /tmp/scoring/opencode2api.log
```

## 脚本与运行

```bash
cd .research/scoring
python3 aggregate.py                       # 默认 7 天半衰期 → node_stats.json
SCORING_HL=3  python3 aggregate.py          # 敏感性 → node_stats_hl3.json
SCORING_HL=14 python3 aggregate.py          # 敏感性 → node_stats_hl14.json
python3 score.py                           # 全部报表
```

约 7s / 次（单核纯 Python，无第三方依赖）。

## 输出

| 文件 | 内容 |
|---|---|
| `node_stats.json` | 每节点聚合：尝试/成功/失败分类/7 天衰减加权 eff_s、eff_n/延迟分位/探测失败计数/当前状态 |
| `coverage.json` | 归因覆盖率、每日请求量、每日失败量 |
| `probe_rounds.json` | 最近 40 轮全局探测（probed/failed/canary/verdict） |
| `scores.json` | 219 节点最终分 + 分档 + `score_reasons[]` + 各分项（**前端字段契约的实物样例**） |
| `scores.csv` | 同上，扁平表（便于 Excel 核对） |
| `distribution.md` | 分数分布/直方图/样本量分布/available 内部分档 |
| `consistency.md` | 与现判 available/dead 的一致率 + 不一致节点逐条分析 |
| `worst10.md` / `best10.md` | 最差/最好 10 个（含原因与证据） |
| `sensitivity.md` | 半衰期 3/7/14 天、权重变化的秩相关与分档迁移 |
| `shrinkage.md` | 小样本收缩（Wilson 下界 vs 原始成功率）实例 |
| `probe_health.md` | 探测历史可得性 + 最近 40 轮 + `trend[]` 建议存储格式 |
| `profile.md` | 数据画像、归因覆盖率、每日请求量、失败类型分布 |

## 口径要点（详见 spec §2/§4）

1. **归因到「节点尝试」**：`nodes[]` 里每个被尝试的节点算一次尝试；成功/失败由同请求 `events[]` 里该节点的事件判定（`connect_error`/`connect_ok`/`upstream_error`/`stream_interrupt` 等）。
2. **429/quota 不计质量分**；`400/404` 剔除；`5xx` 半权；`context canceled` 无法从 call_log 区分（`err_msg` 里若出现则剔除，回算样本中极少）。
3. **全局抖动抑制**：同一 10 分钟窗口池级失败率 ≥50%（且 ≥5 请求）时，窗口内节点失败只按 20% 计责（否则一次出口抖动会冤枉几十个节点）。
4. **7 天半衰期**指数加权，参考时间取 call_log 最后一条（2026-10-20T23:07+08）。
5. 探测窗口以**运行日志最新时间**（2026-10-21T08:27）为准。
