# Context — opencode2api

## Glossary

- **日志分级（log level filtering & switching）**：WebUI 运行日志页的两项能力——(1) 按 `debug/info/warn/error` 级别过滤日志展示；(2) 运行时切换进程日志级别（仅影响当前进程内存，不持久化，重启恢复启动参数配置）。
- **用量趋势（usage trends）**：WebUI 概览页的统计图卡片（替换原「实时趋势」），由调用日志（call_log）按时间聚簇驱动，每个时段桶含请求/成功/失败/输入/输出/缓存创建/缓存读取 Token（图例另含缓存命中率 = 命中÷(命中+创建)，双零时隐藏）；支持今日（小时粒度）/7 天/30 天（天粒度）范围切换，以及可配置的自动刷新（关闭/5s/10s/30s/60s，不持久化，仅当前会话内存）。缓存字段与 OpenAI/Claude 双格式归一（cache_creation_input_tokens、cache_read_input_tokens、prompt_tokens_details.cached_tokens、input_tokens_details.cached_tokens），流式请求的 token 在流结束时补录。

## 部署注意事项

- **时区**：趋势图与调用日志按进程本地时区归桶/记录（`time.Local`）。Docker 镜像默认 `TZ=Asia/Shanghai`，容器部署可 `-e TZ` 覆盖；若进程时区为 UTC 而用户在东八区，图与日志会差 8 小时。历史 UTC 记录无需迁移，归桶时自动换算。