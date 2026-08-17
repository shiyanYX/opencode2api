# ADR-0001：节点状态恢复机制（exhausted 定时恢复 / dead 事件恢复）

## 状态

已采纳（2026-08-17）

## 背景

节点池以 `NodeExhausted`（配额耗尽）与 `NodeDead`（健康探测失败）标记不可用节点，并为两者都挂冷却时间。但原 `eligible()` 按冷却时间放行（`now.After(CooldownUntil)`），而已标记节点并不会在冷却到期时自动清回 `available`，导致状态字段与路由选路脱节：

- 面板徽标 / `exhausted_count` 仍显示 exhausted，流量却在冷却一到期就切回该节点（冷却期内标记为「不可用」的语义从未被路由真正遵守）；
- 重启加载持久化状态（`loadState`）时，冷却已过期（past-cooldown）的标记立即放行，面板与路由继续不一致；
- 配额切换预算（`max_quota_node_switches`，默认 5）被重试循环上限吞掉：循环 bound = `max(3,3)` = 3，配额 `continue` 也消耗 attempt，5 节点预算形同虚设。

## 决策

- **恢复机制分类**：
  - `exhausted` = 定时恢复。配额冷却到期后由惰性清扫 `sweepExpiredLocked()` 翻回 `available` 并清空标记（MarkedAt / CooldownUntil / LastError），在路由与快照入口（`pick` / `manual` / `switchToNext` / `snapshot`）触发，仅在确有翻转时异步持久化一次。
  - `dead` = 事件恢复。仅由探测成功（`recordProbeSuccess`）解除；健康循环每分钟复探 dead 节点（复探间隔 = `node_cooldown_dead_minutes`，默认 1 分钟），冷却时间不参与判定。
- **`eligible()` 塌缩**为 `n.State == NodeAvailable`：冷却不再决定路由可用性，面板状态即路由事实。
- **健康循环拆分**：调度巡检只探测 `available`（跳过 exhausted，配额冷却自行恢复）；每分钟复探 `dead`；面板手动「测速」才全量探测（含 exhausted——仅刷新延迟、不解除 exhausted 标记）。
- **配额预算生效（E-1）**：`callOpenCodeAPI` 与 `callOpenCodeAPIStream` 的循环上限 = 重试上限（3）＋ 配额预算（5）= 8；普通重试仍由 `canRetry` 闸门封顶 3 次；配额切换用尽预算后落回现有错误返回路径。

## 被否选项

- **D-3 对称时间制**（dead 也按冷却自动恢复）：会让徽标再次说谎——冷却期内面板显示 dead 但流量照走，正是本次要修的根因；且故障节点恢复前应验证真实可用。
- **D-4 请求传输失败即标 dead**：传输错误重试同一节点、不升级为节点级故障，避免一次抖动误杀整节点（保留给后续迭代）。
- **E-2 配额切换独立内层循环**：预算边界难收敛、双循环交互复杂易错。

## 后果

- 正面：面板状态与路由选路一致；冷却到期自动恢复，不再依赖手动 unmark；配额 5 次切换预算真正生效；故障恢复延迟从「≤15 分钟探测周期」降到「≤1 分钟」。
- 代价：`node_cooldown_dead_minutes` 语义从「故障冷却」重定义为「复探间隔」（配置字段不变，旧配置兼容自动生效）；dead 节点不会因冷却时间过去而复活，必须等待下一轮探测成功。