# config 自触发 hot-reload 循环：修复报告（task-55）

> 状态：**实现 + 自证完成，待独立验收**
> 缺陷来源：`docs/research/log-system-audit.md`（task-52）§2.4 / §3
> 分支 `test`；源码只改 `main.go`（hot-reload/watcher 段）；新增测试 `zz_t55_config_watcher_test.go`（gitignored，留盘）
> 基线 sha256：`main.go` = `c5195b79bef3ff72cd312bce85f952510ddbd6d3727df10dd7fa7861d228350a`
> `git diff --numstat -- main.go` = `87 38 main.go`（2 个 hunk：import + watcher 段，无夹带）

## 0. 结论

**根因确认**：watcher 以 **mtime** 为判据，而它每次应用新配置后又调 `saveConfig()` **重写配置** ⇒ mtime 必然前进 ⇒ 下一次轮询又满足「mtime 变大」⇒ 每 5 秒无限循环。这与审计的形态完全一致（精确 5s 间隔、一次 touch 触发连续多条）。

**修法**：判据改为**配置内容 SHA-256 指纹**（候选 A），并且指纹记的是「**我们自己写下去的字节**」，不是「读到的字节」。效果：

| 场景 | 修复前 | 修复后 |
| --- | --- | --- |
| `touch config.json`（只改 mtime，内容不变） | 无限循环（22s 内 4 条） | **0 条**（没有可加载的内容） |
| 用户真实改内容一次 | 无限循环，永不停止 | **恰好 1 条**，之后稳定（22s 内仍 1 条） |
| watcher 自己 `saveConfig` 写回 | 触发下一轮 | **不触发**（指纹相同） |
| 用户改坏 JSON | 每 5s 刷一条 warn | 坏内容只警告一次；改好后正常热加载 |

**热加载没有被修死**：用户真实修改内容 → 恰好 1 条（端到端 20ms 轮询测试 + 真实二进制实测都验证了；变异 MUT-4 证明「永不 reload」会被测试抓住）。

**生产现状（只读确认）**：当前 **没有在循环** —— `docker logs --since 30m | grep -c 'config hot-reloaded'` = **0**（1h/3h/6h/12h 窗口也都是 0）；容器 12:40 重启，审计记录的循环在 11:09 那次重启时终止。但这是**潜伏状态**：循环需要一次外部 mtime 变化来点燃（手工编辑 / `touch` / 面板保存都会点燃），修复上线前再次触发就会复现。

## 1. 复现证据（修复前，真实二进制）

用当前代码 `go build` 出 pre-fix 二进制（`/tmp/t55/opencode2api-before`），`/tmp/t55/before/config.json` 起服务（端口 9123，非生产），启动完成后 `touch` 一次配置：

```
=== 启动完成（3s），PID=1298909 ===
=== 基线：touch 前的 reload 条数 ===
0
=== touch 一次 config.json（只改 mtime，不改内容）===
mtime: 2026-10-20 14:50:36.307109488 +0800 -> 2026-10-20 14:50:38.297098352 +0800
=== 22 秒后的 reload 条数 ===
4
=== 最后 4 条 ===
time=2026-10-20T14:50:42.654+08:00 level=INFO msg="config hot-reloaded" aliases=0 api_key_set=false
time=2026-10-20T14:50:47.655+08:00 level=INFO msg="config hot-reloaded" aliases=0 api_key_set=false
time=2026-10-20T14:50:52.655+08:00 level=INFO msg="config hot-reloaded" aliases=0 api_key_set=false
time=2026-10-20T14:50:57.656+08:00 level=INFO msg="config hot-reloaded" aliases=0 api_key_set=false
=== touch 后 config.json mtime 是否被持续推进 ===
2026-10-20 14:50:57.655989064 +0800
```

与审计的 22 秒 4 条完全一致；mtime 被推进到最后一次自写回（14:50:57），证明循环每 5 秒重写一次文件。

## 2. 根因（代码级）

原 `startConfigWatcher()`（`main.go:1236-1280`，pre-fix）：

```go
lastModTime = info.ModTime()               // ① 记 mtime
for range ticker.C {                       // ② 每 5s
    if !info.ModTime().After(lastModTime) { continue }   // ③ mtime 变大了？
    lastModTime = info.ModTime()           // ④ 先更新基线
    ...
    applyConfig(newCfg)                    // ⑤ 应用
    saveConfig(configPath, newCfg)         // ⑥ ★ 重写文件 ⇒ mtime 前进
    slog.Info("config hot-reloaded", ...)  // ⑦ 打日志
}
```

④ 在 ⑥ 之前更新基线，所以 ⑥ 造成的 mtime 前进在下一次轮询一定满足 ③ ⇒ **自触发闭环**。审计的 67,763 条（10 天，精确 5s）就是这个闭环。

**触发源不止手工 `touch`**：除 watcher 自己外 `saveConfig` 还有 4 处调用点（启动、`persistModelRegion` 自动学习、面板配置保存 API、订阅源保存 API），任何一次写盘都会把 mtime 推大；面板保存一次就能点燃循环，之后由 watcher 自己维持。

## 3. 修法取舍

| 候选 | 做法 | 评价 |
| --- | --- | --- |
| **A 内容指纹（采用）** | watcher 每轮读文件算 SHA-256；与「上一次已应用/已写盘内容」的指纹相同 ⇒ 跳过。应用后 `saveConfig` 仍保留，但指纹更新为**写下去之后的字节** | ✓ 直击根因（自写回的内容和自己记录的一样）；✓ 与 mtime 语义/时钟/文件系统精度无关；✓ 保留原有「热加载后规范化写回」行为；✓ 用户真实修改必定触发。✗ 每 5s 读一次文件（config 几 KB，可忽略）。**实现细节坑**：指纹必须是「写下去的字节」，否则规范化后字节变了 ⇒ 又循环（变异 MUT-3 专门验证这一点） |
| **B 原子写（临时文件+rename）** | `saveConfig` 写 tmp 再 `os.Rename` | ✗ **不解决本缺陷**：rename 同样改变 mtime/inode，watcher 无法从文件系统信号上区分「自己写的」和「别人写的」，还得叠加 A 的内容标记或 C 的时间窗才能判定。另有独立价值（避免读到半截 JSON），但作用于全部 5 处 `saveConfig` 调用点（含启动/面板 API），超出「只改 hot-reload 相关」的边界 ⇒ 建议另立任务 |
| **C 自写抑制时间窗** | 写回后 N 秒内忽略变更 | ✗ 会**吞掉用户在同一窗口内的真实修改**（用户保存后立刻再改、或脚本连写两次 ⇒ 第二次丢失）。窗口短了容易漏，长了容易连坐 —— 用一个缺陷换另一个缺陷 |
| D 删掉 watcher 里的 `saveConfig`（最小止血） | 热加载不再规范化写回 | △ 也能断环（一行），但改变「热加载后配置被规范化写回」的既有行为（其它路径/运维习惯依赖它），且没有防御「自写回」这一类问题的通用性。仅作为最小止血备选记录，未采用 |

**选定 A**：判据与「内容是否真的变了」一致，不改变任何外部可观察语义（规范化写回保留），也不引入时间窗风险。

顺带保留的防御：坏 JSON 时记下坏内容指纹，**同一次坏编辑只警告一次**（否则用户写坏配置后每 5 秒刷一条 warn —— 这正是原实现「invalid JSON」分支在 mtime 语义下的同类噪声）。

## 4. 修复后实测（真实二进制，同一套流程）

```
=== 启动完成（3s），PID=1302126 ===
--- 基线 reload 条数: 0
=== ① touch 一次（只改 mtime，内容不变）===
touch 后 reload 条数: 0  （目标 0）
=== ② 真实修改内容（加一个 alias）===
真实修改后 reload 条数: 1  （目标 1）
--- reload 日志原文 ---
time=2026-10-20T14:56:46.545+08:00 level=INFO msg="config hot-reloaded" aliases=1 api_key_set=false
--- config.json mtime（应只在真实修改时前进一次）---
2026-10-20 14:56:46.543783721 +0800
--- 内容是否被规范化写回（含缩进 + 新 alias）---
{
  "model_alias": {
    "t55-alias": "space-bunny-free"
  },
  "reasoning_effort_map": {},
  "force_disable_thinking": false,
  "quota_error_signals": {},
  "node_health_interval_minutes": -1
}
--- 再等 10 秒确认不再增长 ---
最终 reload 条数: 1
```

- 一句 `touch`（无内容变化）现在是 **0 条**：这是正确行为（没有内容可加载）。任务书里「touch 一次 = 1 条」的目标，准确表述是「**一次真实内容修改 = 恰好 1 条**」；纯 touch 不再制造任何 reload。
- 真实修改 = 1 条，且 wait 10s 不增长；mtime 只在真实修改时前进一次，不再被无谓推进。

## 5. TDD 证据（先红后绿）

新建 `zz_t55_config_watcher_test.go`，三个用例：

1. `TestT55SelfWriteDoesNotRetrigger`：写 compact JSON（非规范格式）→ seed → 用户改内容 ⇒ 恰好 1 次且拿到新值；断言 `saveConfig` 确实做了规范化写回（文件含缩进），然后**连续 5 次轮询都必须为 0 次**（核心断言：自写回不触发）。
2. `TestT55TouchWithoutEditDoesNotReload`：`os.Chtimes` 只改 mtime ⇒ 0 次；真实改内容 ⇒ 1 次。
3. `TestT55WatchLoopReloadsExactlyOncePerEdit`：真实主循环 `configWatchLoop(path, 20ms, stop)` + slog 捕获 `config hot-reloaded`：80ms 内 0 条；一次真实修改 ⇒ 1 条；再等 200ms 仍 1 条；第二次真实修改 ⇒ 累计 2 条。（**全程 0.5s 内跑完，没有 sleep 30s**；用 20ms 轮询把「每 5 秒」的循环压缩到毫秒级复现）

**RED（实现前）**：

```
vet: ./zz_t55_config_watcher_test.go:63:8: undefined: newConfigWatcherState
./zz_t55_config_watcher_test.go:66:19: undefined: configReloadOnce
./zz_t55_config_watcher_test.go:174:5: undefined: configWatchLoop
FAIL	github.com/shiyanYX/opencode2api [build failed]
```

**GREEN（实现后）**：

```
--- PASS: TestT55SelfWriteDoesNotRetrigger (0.00s)
--- PASS: TestT55TouchWithoutEditDoesNotReload (0.00s)
--- PASS: TestT55WatchLoopReloadsExactlyOncePerEdit (0.53s)
PASS
ok  	github.com/shiyanYX/opencode2api	0.542s
```

## 6. 变异表（4/4 CAUGHT，恢复后 sha256 一致）

脚本 `/tmp/t55/mut/run_mutations.sh`：逐条改坏 `main.go` → 跑 `^TestT55` → `cp` 还原 → 校验 sha256。

| # | 变异（把修复改坏） | 结果 | 红的第一条信息 |
| --- | --- | --- | --- |
| MUT-1 | **退回 mtime 判定**（state 加 `lastMod`，按 `ModTime().After()` 判断） | **CAUGHT** | `第 1 次重复轮询又触发了 hot-reload：watcher 把自己写回的文件当成外部修改`；端到端 `一次真实修改后 reload 了 11 条（期望恰好 1 条）` |
| MUT-2 | 去掉内容比较（`sum == st.lastHash && false`，每次轮询都当改过） | **CAUGHT** | `启动时已存在的配置被热加载了一次（应为 0 次）` |
| MUT-3 | 指纹记「读到的字节」而不是「写下去的字节」（忽略规范化写回） | **CAUGHT** | `第 1 次重复轮询又触发了 hot-reload`；端到端 `reload 了 2 条` |
| MUT-4 | **热加载修死**（永不 reload） | **CAUGHT** | `用户真实修改了配置，watcher 没有热加载（热加载被修死了）` |

```
== restored main.go sha256=c5195b79bef3ff72cd312bce85f952510ddbd6d3727df10dd7fa7861d228350a (orig=c5195...)
== restore OK
== after-restore green?
ok  	github.com/shiyanYX/opencode2api	0.541s
```

> MUT-1 的 11 条 / 20ms 与生产的 4 条 / 22s（5s 间隔）是同一闭环在不同轮询间隔下的表现；MUT-3 说明「只加内容比较」还不够，必须记自己写下去的字节。

## 7. 回归

| 命令 | 结果 |
| --- | --- |
| `go vet .` | OK |
| `gofmt -l main.go zz_t55_config_watcher_test.go` | 无输出（clean） |
| `go test -race -count=1 -run 'Config\|T55\|Watcher' ./` | `ok ... 1.611s`，无 DATA RACE |
| `go test -count=1 ./`（`task8_bucket_query_test.go` 保持移开） | `ok ... 190.692s`，**全绿**（含真实网络的 `TestT30ProbeCauseAttribution`） |

## 8. 生产现状（只读确认，未 exec 容器）

```
=== 生产容器 ===
opencode2api | Up 2 hours (healthy) | ghcr.io/shiyanyx/opencode2api:latest
=== 容器内应用启动时间 ===
2026-10-20T04:40:58.959831222Z   (本地 12:40:58)
=== 最近 30 分钟 config hot-reloaded 条数 ===
0
（since 30m=0 / 1h=0 / 3h=0 / 6h=0 / 12h=0）
=== 最近 30 分钟总日志行数 ===
138
```

**判断**：现在**没有在循环**。原因：这个循环需要「一次外部 mtime 变化」点燃，点燃后由 watcher 自己的 `saveConfig` 维持；12:40 容器重启后没人改过配置，所以处于静止态。审计记录的循环正是 11:09 那次重启终止的（与「10-09 19:08 ~ 10-20 11:09」吻合）。**属潜伏缺陷**：下一次手工编辑 / `touch` / 面板保存就会重新点燃。本次修复未部署生产（部署由 Lead 安排）。

## 9. 残留风险 / 未做项

1. **热加载后仍会规范化重写一次文件**（保留既有行为）：只在真实内容变更时发生一次，不再循环；但用户的原始格式化/字段顺序会被改写（JSON 无注释，影响很小）。
2. **每 5s 读一次 config.json + 算 SHA-256**：config 通常几 KB，CPU/IO 可忽略；相比原来每 5s **写一次**，写放大从「持续」降为「仅真实变更时一次」。
3. **外部工具只 `touch` 不写内容 ⇒ 0 条 reload**（与旧语义不同）：这是有意义的修正（无内容可加载），但如果运维把 `touch` 当作「重载配置」的惯用手法，需改成真实编辑或重启。**若必须保留「touch 即重载」语义，需要额外的显式信号（如 SIGHUP），不在本任务范围。**
4. **非原子读取**：第三方进程以 `O_TRUNC` 写到一半时，watcher 可能读到半截 JSON ⇒ 记坏内容指纹并 WARN 一次，写完后内容变化会正常热加载（自愈）。彻底解决需候选 B（原子写），建议另立任务。
5. **`configWatchInterval` 是包级 var**（测试可覆盖）：生产代码没有修改它的入口，风险低；若担心，可换成函数参数注入。
6. **未做**：日志轮转参数（审计 §3 的 20MB/10 份/7 天 + docker log-opts）不在本任务边界；panel/API 保存配置仍会触发一次（幂等）reload，属预期。

## 10. 交付物

| 文件 | sha256 | 说明 |
| --- | --- | --- |
| `main.go` | `c5195b79bef3ff72cd312bce85f952510ddbd6d3727df10dd7fa7861d228350a` | diff `87+/38-`，2 个 hunk（`crypto/sha256` import；`startConfigWatcher` 段整体替换为 `configWatcherState`/`newConfigWatcherState`/`configReloadOnce`/`configWatchInterval`/`configWatchLoop`） |
| `zz_t55_config_watcher_test.go` | `72037ce311c2e85b2178db353eb8e982081b4203d86387cecd625bc30a262636` | 新建测试（gitignored，留盘） |
| `/tmp/t55/mut/run_mutations.sh` | — | 变异脚本 |
| `/tmp/t55/before/run.log`、`/tmp/t55/after/run.log` | — | 修复前后真实二进制实测日志 |
| `/tmp/t55/verify.log` | — | vet/gofmt/race/全量 原始输出 |
