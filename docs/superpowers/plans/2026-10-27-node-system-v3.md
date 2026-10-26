# 节点系统 v3 执行计划（2026-10-27）

> 回答「实现计划规划好了吗」：**之前没有**。之前只有设计方向 + 4 个粗批次；两份独立研究（`11-shape-drift-design.md` 的 C10、`12-shape-drift-review.md` 的 G1）刚刚证明工作区里那版漂移闩 **no-go**，并给出了更强的方案。
> 本文件才是执行计划：每个切片有文件、验收、变异数、提交边界、回滚方式。

## 0. 决策记录

**采纳：C10 × G1 合并方案。**
- 来自 C10（healthcheck-probe）：`(model, egress)` 级排除 + 5m/30m/2h/5h 递进冷却 + 每 IP 24h 坏形态预算 + 探测只允许单飞。
- 来自 G1（review-t1-design）：**发送前闸门**（probe_only 时除单飞探测外一律不发）+ blocked 集合只由证据清除、**TTL 不清空** + 试探令牌绑定具体出口 + 对外 503 + `Retry-After`。
- 合并后的关键取舍：
  1. **排除状态按 `(model, egress)`**（因为配额是 per-model，有 deepseek 成功而 mimo 失败的实证）；
  2. **probe_only 是全局单闸**（形态拒绝是账号/IP 级；避免 M 个 model 放大探测）；
  3. **触发条件改为「连续 K=5 个新出口 429 且期间无 2xx，且距上次 2xx >60s」**，不用「3 个不同出口/10min」这个伪信号；
  4. **任何真实 2xx 立即清 probe_only**（跨 model）；
  5. **probe_only 期间禁止回退直连**（`allow_direct_in_probe=false`），全排除时 fail-fast 503；
  6. **每次抑制都写结构化事件**，没有留痕的方案不合格。

**明确否决**：现有 23:32 版（per-model 桶 + 10min TTL 清空 seen + 事后 Halt）——评审实测 5h 内发送 5,520 次、每出口命中 30 次、主 IP 命中 30 次，2h40m 烧穿全池。

## 1. 切片（每个切片 = 一个可独立回滚的提交）

### S0 前置：拆干净工作区
- 把当前混在一起的 T1/T2/T3/T6 改动按切片拆分提交到 `test`（允许提交，`main` 仍需用户批准）。
- `*_test.go` 永不入库（`.gitignore:26`）。
- 验收：每个提交单独 `go build ./...` 通过；`git log` 里 T1/T2/T3 各自可 revert。
- **为什么先做**：现在 12 个文件、1,253 行改动混在一起，任何回滚都会带走无关修复。

### S1 漂移闩替换为合并方案
- 文件：`shape_drift.go`（重写）、`main.go`（两条循环：发送前闸门 + 503/Retry-After + 试探令牌绑定 + 成功清闩）。
- 必须实现：
  - 全局 `probeOnly` + `nextProbeAt` + 单飞 `probe{key,at}` + `blocked`（首次时间，不刷新）；
  - 每 `(model,egress)` 排除，strikes 冷却 5m/30m/2h/5h；24h 内单 egress 命中预算 8；
  - 触发 K=5 连续新出口失败 + 无 2xx + last_success>60s；
  - 退出：任一 2xx / TTL 10min（退出后每小时最多重入 6 次，且消耗预算）；
  - 全排除时 503 + `Retry-After: 30`，不回退直连；
  - 抑制事件字段：`model`、`node_fp`、`would_be_egress`、`suppressed_by=shape_drift_probe_only`、`blocked_n`。
- 测试 ≥18：触发阈值边界、TTL 不清 blocked、单飞、令牌错配不升级退避、2xx 清闩、model 名 10 万不爆内存、24h 预算、直连禁止、503 头。
- 变异 ≥8 全红（删发送前闸门、TTL 清 blocked、去单飞、去预算、去 2xx 清闩、per-model 换成全局、503 换 429、抑制不留痕）。
- 验收：`go test -skip 'TestIP4RV' -count=1 ./...` + 事故仿真（下节）+ 独立评审 PASS。

### S2 生命周期：probation + mark 幂等
- 文件：`proxy_node.go`、`main.go`（成功/失败调用点）。
- 冷却到期 → `probation`；单次证据请求；成功 → available；429 → 递进冷却；其他失败 → dead。
- mark 幂等：重复 mark 不延长 `CooldownUntil`、不重复写日志。
- 测试已有规格：`node_probation_test.go`（本地）。
- 验收：该文件全绿 + 变异 ≥5 全红。

### S3 资格：per `(model, node)` 配额状态
- 文件：`proxy_node.go`、`calllog.go`、状态持久化（`proxy_state` 迁移，需向后兼容：旧文件无该字段时视为 `unknown`）。
- 429 只写该 model 的配额；节点对其他 model 仍可选。
- 面板新增只读字段（缺失显示「未提供」）。
- 验收：迁移测试（旧状态文件可加载）、跨 model 隔离测试、变异 ≥5。

### S4 打分架构（等 T7 结论）
- 按 `13-scoring-architecture.md` 落地；不阻塞 S1–S3。

## 2. 事故仿真验收（每个切片都要跑）

用 `18:03–23:00` 与 `00:20–00:25` 的真实参数（R≈0.5 req/s、212 出口、H=10–16）跑虚拟时间 5h：
- 断言：`sent`（上游发送总数）≤ 请求数 × 0 + 探测数（probe_only 生效时应 ≈ 探测量级，而不是 5,520）；
- 单出口命中 ≤ 1 次/风暴（除非证据恢复后再次失败）；
- 主 IP 命中 = 0；
- 闩最长连续 armed < 10min，且期间至少 1 次成功退出；
- 24h 后无出口达到封禁阈值。
仿真脚本入库到 `docs/research/scoring2/replay/`（只读日志、可复跑）。

## 3. 上线与回滚

- **开关**：`SHAPE_DRIFT_GATE=off|on`（默认 on，panic 时可关闭回到旧行为）；`SHAPE_DRIFT_PROBE_ONLY_TTL`、`SHAPE_DRIFT_K` 可调。
- **部署节奏**：S0+S1 一次部署（止血+正确性），S2 紧随，S3 单独。
- **观察窗口**：每次部署后 30min 看四项——成功率、`sent` 总量、`suppressed_by` 计数、单出口命中分布。
- **回滚判据**：成功率低于部署前 10 个百分点，或可用节点 <20，或 probe_only 连续 >15min 未退出 ⇒ 关开关/回滚。
- **不许**：靠重启清闩（会清掉 blocked 证据，重新烧）。

## 4. 验证门禁（每个切片）

1. `go build ./...`、`go vet ./...`、改动文件 gofmt 干净；
2. `go test -skip 'TestIP4RV' -count=1 ./...` 通过（`TestIP4RV*` 与缺 `free_models_cache.json` 的 T23 为既有基线）；
3. 变异全红（数量见各切片）；
4. 事故仿真通过；
5. **独立评审**（非实现者）出 go/no-go；
6. 提交信息写清切片编号与回滚方式。

## 5. 当前阻塞与依赖

- S1 需要重写 `shape_drift.go`：现有 400 行实现方向对但触发/闸门错，**不能增量修补**。
- S2 已有测试规格，可并行。
- S3 需要状态文件迁移设计（尚未做）。
- S4 等 T7（`13-scoring-architecture.md` 仍是半成品，167 行，结论未回填）。
- 研究文档状态：`11-shape-drift-design.md` 736 行（完整）、`12-shape-drift-review.md` 757 行（完整）、`13-scoring-architecture.md` 167 行（未完成）。
