# opencode2api

`opencode2api` 是一个本地 HTTP 代理，把 OpenAI Chat Completions、OpenAI Responses 和 Anthropic Messages 风格的请求转发到 OpenCode 上游接口，并提供模型别名、reasoning/thinking 兼容、SOCKS5 代理和一个轻量管理面板。

> 这个项目不是 OpenAI、Anthropic 或 OpenCode 的官方项目。请遵守上游服务条款，并只在你有权限的环境中使用。

## 功能

- OpenAI 兼容接口：`/v1/chat/completions`、`/v1/models`
- OpenAI Responses 兼容接口：`/v1/responses`
- Anthropic Messages 兼容接口：`/v1/messages`
- 流式 SSE 转换和 token 用量统计
- 模型别名、reasoning effort 映射、强制禁用 thinking；`-free` 免费模型自动映射（无需逐个手动配置，面板自动列出并跟随上游变动）
- 订阅/手配节点池（vless/reality、ss、hysteria2、anytls、socks5）：进程内直连，无需 mihomo 子进程
- Webshare 代理池：API key 自动拉取 webshare.io 代理列表并入节点池（SOCKS5，分页全量、失效自动排除）
- 免费额度耗尽自动切换节点（FreeUsageLimitError 等，单请求预算 5 次，与重试上限独立）；耗尽节点 1h 冷却到期自动恢复，故障节点每分钟复探、成功即恢复
- 上游客户端形态适配：复刻官方 opencode 客户端的 session ID 形态、强制上游流式（非流式客户端本地聚合）、工具名规范化与回映射，应对 Zen 免费层按「请求像不像官方客户端」鉴权（**对下游 agent 零改动**，见 [docs/UPSTREAM-COMPAT.md](docs/UPSTREAM-COMPAT.md)）
- SOCKS5 直连、指定代理和轮换代理
- Web 管理面板：配置、统计、刷新上游会话、节点池管理（切换/解除标记/重新加载订阅）
- GitHub Actions 自动构建 Linux、macOS、Windows、FreeBSD 多平台 release
- GitHub Actions 自动发布 Docker 镜像到 GHCR

## 快速开始

```bash
git clone https://github.com/6Kmfi6HP/opencode2api.git
cd opencode2api
cp config.example.json config.json
go run . -port 8000 -config config.json -password "change-me"
```

健康检查：

```bash
curl http://127.0.0.1:8000/health
```

查看模型：

```bash
curl http://127.0.0.1:8000/v1/models
```

认证模式：

- 不带 `Authorization`，或使用 `Bearer public`：走 OpenCode public，只可稳定访问 `-free` 结尾的免费 Zen 模型。
- 使用 `Bearer <api-key>`：默认走 Zen；如果请求的是仅存在于 Go 目录中的模型，会自动切到 Go。
- 使用 `Bearer zen:<api-key>`：强制走 Zen，适合你明确要用 Zen 按量计费目录时。
- 使用 `Bearer go:<api-key>`：优先走 Go 订阅目录；共享模型也会按 Go 路径请求。
- 无效或占位 key（如 `no-key-required`）会自动回退到 public 模式。

Chat Completions 示例：

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": false
  }'
```

Go 订阅示例：

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer go:YOUR_OPENCODE_KEY" \
  -d '{
    "model": "glm-5.2",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": false
  }'
```

## 命令行参数

```text
-port string
    服务端口，默认 8000
-config string
    配置文件路径，默认 config.json
-password string
    管理面板密码，默认 123456；留空表示不启用登录验证
-debug
    输出调试日志（等价于将 -log-level 提升到 debug）
-log-level string
    日志级别: debug/info/warn/error，默认 info
-log-file string
    日志文件路径，默认 opencode2api.log；配合自动轮换
-log-stdout
    是否同时写 stdout，默认 true
-log-max-size int
    单日志文件最大 MB，默认 100
-log-max-backups int
    保留旧日志个数，默认 7
-log-max-age int
    旧日志保留天数，默认 14
-log-compress
    轮换后 gzip 压缩，默认 true
-log-bodies
    Debug 下记录截断的 body 形状摘要，默认 false
-version
    显示构建版本
```

第一次部署请务必修改 `-password`。如果把服务暴露到公网，建议只通过反向代理、访问控制或 VPN 暴露管理面板。

### 排障日志

默认同时写文件与 stdout。每个请求带 `request_id`（响应头 `X-Request-Id`），可串联：

`request_started → request_plan → upstream_attempt* → upstream_result → stream_result|request_result → request_done`

常见排查：

```bash
rg 'empty_reply=true' opencode2api.log
rg 'request_id=XXXX' opencode2api.log
rg 'promoted_reasoning=true' opencode2api.log
```

容器内默认日志路径是 `/data/opencode2api.log`（挂载卷持久化），可用环境变量覆盖：

- `OPENCODE2API_LOG_FILE`
- `OPENCODE2API_LOG_LEVEL`
- `OPENCODE2API_LOG_STDOUT`

### 免费模型 403 `FreeTierError`

免费模型报 `403 FreeTierError: OpenCode's free tier can only be used from within OpenCode` 时，
几乎都是**上游收紧了「客户端形态」校验**，而不是你的 key、账号或出口 IP 有问题——换 IP、换 key、
降级版本都无效。本项目已对齐截至 2026-09-18 实测的四道条件：

1. `x-opencode-session` 必须符合官方 ID 形态（`ses_` + 12 位小写十六进制 + 14 位 base62）；
2. `User-Agent` 必须是 `opencode/<semver>` 且不低于 1.17.0；
3. 上游请求 `stream` 必须为 `true`（非流式客户端由网关本地聚合）；
4. 上游 `tools` 必须含官方小写工具名 `bash`/`glob`/`grep`/`read`（网关自动规范化并回映射）。

排查与「上游再次变更时如何重新定位」的完整流程见 [docs/UPSTREAM-COMPAT.md](docs/UPSTREAM-COMPAT.md)。
注意区分两个错误：`FreeTierError`（403，客户端形态被拒，换节点无用）与 `FreeUsageLimitError`
（429，免费额度按出口 IP 限流，靠切换节点缓解）。

## 本地构建

```bash
make fmt
make vet
make build
./bin/opencode2api -version
```

生成本地多平台 release 包：

```bash
make release-snapshot VERSION=v0.1.0
ls dist/
```

## 自动 Release

推送 `v*` tag 后，GitHub Actions 会先运行一次格式和 vet 检查，然后用 matrix 并发构建以下目标：

- `linux/amd64`
- `linux/arm64`
- `linux/arm/v7`
- `darwin/amd64`
- `darwin/arm64`
- `windows/amd64`
- `windows/arm64`
- `freebsd/amd64`
- `freebsd/arm64`

发布命令：

```bash
git tag v0.1.0
git push origin v0.1.0
```

Release 会包含每个平台的 `.tar.gz` 包和统一生成的 `checksums.txt`。

## Docker Compose 部署

项目提供单独运行、Tor 代理、WARP 代理三套 compose 模版：

```bash
export OPENCODE2API_PASSWORD="change-me"
docker compose -f deploy/compose/compose.yml up -d
```

代理部署见 [Docker Compose 部署模版](deploy/compose/README.md)。

## 文档

- [API 兼容说明](docs/API.md)
- [配置说明](docs/CONFIGURATION.md)
- [部署说明](docs/DEPLOYMENT.md)
- [发布流程](docs/RELEASE.md)
- [Docker Compose 部署模版](deploy/compose/README.md)
- [贡献指南](CONTRIBUTING.md)
- [安全说明](SECURITY.md)

## 许可证

当前仓库默认保留全部权利，避免在未确认授权策略前自动开源。需要公开开源时，可将 `LICENSE` 替换为 MIT、Apache-2.0 或其他许可证。
