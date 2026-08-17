# Context — opencode2api

## Glossary

- **日志分级（log level filtering & switching）**：WebUI 运行日志页的两项能力——(1) 按 `debug/info/warn/error` 级别过滤日志展示；(2) 运行时切换进程日志级别（仅影响当前进程内存，不持久化，重启恢复启动参数配置）。
- **用量趋势（usage trends）**：WebUI 概览页的统计图卡片（替换原「实时趋势」），由调用日志（call_log）按时间聚簇驱动，每个时段桶含请求/成功/失败/输入/输出/缓存创建/缓存读取 Token（图例另含缓存命中率 = 命中÷(命中+创建)，双零时隐藏）；支持今日（小时粒度）/7 天/30 天（天粒度）范围切换，以及可配置的自动刷新（关闭/5s/10s/30s/60s，不持久化，仅当前会话内存）。缓存字段与 OpenAI/Claude 双格式归一（cache_creation_input_tokens、cache_read_input_tokens、prompt_tokens_details.cached_tokens、input_tokens_details.cached_tokens），流式请求的 token 在流结束时补录。
- **节点状态（NodeAvailable / NodeExhausted / NodeDead）**：节点池运行时状态机，`eligible()`（`proxy_node.go`）只认 `available`，其余两种状态一律不参与路由。`exhausted`=配额耗尽标记（冷却 `node_cooldown_exhausted_hours`，默认 1h），**定时恢复**：冷却到期由 `sweepExpiredLocked()` 在路由/快照入口清扫翻回 available 并清空标记（MarkedAt/CooldownUntil/LastError）。`dead`=健康探测失败标记，**事件恢复**：仅由探测成功（`recordProbeSuccess`）解除，冷却时间不参与判定（冷却到期不会让 dead 复活）。恢复机制不对称是刻意的：exhausted 可预期自动恢复，dead 必须验证真实可用。
- **复探间隔（dead re-probe interval）**：`node_cooldown_dead_minutes`（默认 1 分钟）重定义后为「dead 节点每分钟复探间隔」——健康循环每分钟探测 dead 节点，成功即恢复；调度变体只探测 available（跳过 exhausted），面板手动「测速」才全量探测（含 exhausted，但只刷延迟、不解除 exhausted 标记）。历史称呼「故障冷却（分钟）」已废弃，含义不同勿混用。
- **配额切换预算（quota switch budget）**：`max_quota_node_switches`（默认 5）= 单个请求可切换配额耗尽节点的次数上限；重试循环上限 = 上游重试上限（3）＋ 配额预算（5）= 8，两条闸门独立封顶不互挤占——普通重试（401/429/5xx）仍按重试闸门封顶 3 次，配额切换直到预算用尽为止。

## 部署注意事项

- **时区**：趋势图与调用日志按进程本地时区归桶/记录（`time.Local`）。Docker 镜像默认 `TZ=Asia/Shanghai`，容器部署可 `-e TZ` 覆盖；若进程时区为 UTC 而用户在东八区，图与日志会差 8 小时。历史 UTC 记录无需迁移，归桶时自动换算。