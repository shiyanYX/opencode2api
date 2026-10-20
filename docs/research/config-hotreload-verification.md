# config hot-reload 自触发循环：修复独立验收（task-57）

> 验收方 `healthcheck-probe`。被测实现方 `impl-ip1`（task-55）。**不采信其结论与测试，全部自己复跑。**
> 报告按步骤增量落盘（防丢）。

## 0. 结论

**`go`（可验收通过）** —— 缺陷用 HEAD 干净副本独立复现（touch 一次 ⇒ 2 条 reload、mtime 被自写持续推进）；
修复后用**我自己的**测试 + 真二进制端到端证明「真实改内容恰好 1 条且真的生效、纯 touch 0 条且 mtime 不再被无谓推进」；
自造 4 个变异 4/4 CAUGHT；全量 159.7s / `-race` / vet / gofmt 全绿；`main.go` 只改 watcher 区域无夹带。
唯一需要落发布说明的语义变化：**`touch` 不再触发重载（内容不变时）**——判定可接受，见 §5。

## 1. 被测对象与钉桩

| | 值 | 状态 |
|---|---|---|
| 修复后 `main.go` sha256 | `c5195b79bef3ff72cd312bce85f952510ddbd6d3727df10dd7fa7861d228350a` | ✅ 已核（与 Lead 给的钉桩一致） |
| `git diff --numstat -- main.go` | `87  38`（2 hunk） | ✅ 已核 |
| 基线 = HEAD | `git show HEAD:main.go`（未修复版） | 用 `git archive HEAD` 取干净副本，**不动工作树** |
| 分支 / 路径 | `test` / `/home/jingyx/project/opencode2api-test` | 未 commit、未改工作树 |

**边界**：不改任何生产代码（`main.go`/`calllog.go`/`statscan.go`/`proxy_node.go`/`node_alert.go`/`static/admin.html`）；只新增本报告 + 自己的 `zz_verify_t57_*.go` 测试；基线/变异都在 `/tmp/chr/` 的**副本**里做；不碰生产容器、不用 8000、不 git。

## 2. 缺陷复核（代码级）—— 状态：✅ 完成

**HEAD 的 watcher 源码**（`git show HEAD:main.go`，`startConfigWatcher` 段）关键 4 行：

```go
if !info.ModTime().After(lastModTime) { continue }   // 判据 = mtime
...
applyConfig(newCfg)
if err := saveConfig(configPath, newCfg); err != nil { ... }   // ★ 应用后把配置写回文件
slog.Info("config hot-reloaded", ...)
```

⇒ 自触发闭环成立：**外部任何一次 mtime 变化**（哪怕只是 touch、或一次真实修改）→ 应用 → `saveConfig` 重写文件 → mtime 再变 → 5 秒后再次应用 → 无限循环。且 `saveConfig` 用 `json.MarshalIndent` 规范化，写回字节与用户原文件必然不同 ⇒ 即使内容语义不变也会持续触发。

**修复后源码**（工作树 `main.go:1234-1327`）：判据改为**配置内容 SHA-256 指纹**，`configReloadOnce()` 在 `saveConfig` 之后**重新读回文件**、用"自己写下去的字节"更新指纹；并抽出 `configWatchLoop(path, interval, stop)` 便于注入短间隔（测试用），生产仍 5s。

## 3. 基线复现（HEAD 未修复代码）—— 状态：✅ 缺陷确认复现

**装置**：`git archive HEAD | tar -x -C /tmp/chr/head`（干净副本，未动工作树）→ 放我自己的
`zz_verify_t57_baseline_test.go`（HEAD 只有全局 `configPath` + `startConfigWatcher()`，只能这样驱动）
→ 用自定义 `slog.Handler` 捕获 `config hot-reloaded` 条数。

```bash
$ cd /tmp/chr/head && go test -count=1 -run TestBaselineMtimeWatcherSelfLoops -v .
=== RUN   TestBaselineMtimeWatcherSelfLoops
    zz_verify_t57_baseline_test.go:83: touch-only: reloads=2 ; 之后真实改内容: reloads=3
    zz_verify_t57_baseline_test.go:84: mtime: 初始=15:13:46.725 → touch后=15:13:56.728 → 结束=15:14:01.727
    zz_verify_t57_baseline_test.go:85: 自写推进: touch后 mtime 是否被推进=true ; 结束是否继续前进=true
--- PASS: TestBaselineMtimeWatcherSelfLoops (17.30s)
ok  	github.com/shiyanYX/opencode2api	17.315s
```

- **纯 touch（内容一字不改）→ 11 秒内 2 条 reload**（5s 一拍，与生产 67,763 条的节奏一致）；
  再改一次真实内容 → 累计 3 条，**且不停止**。
- **mtime 被程序自写持续推进**：touch 把 mtime 设到 15:13:48 级别，程序 5 秒后已把它改成 15:13:56.728，
  再过 5 秒又到 15:14:01.727 ⇒ 自触发闭环实证。

## 4. 修复后验证（包内测试，25ms 间隔驱动）—— 状态：✅ 6/6 PASS

测试文件 `zz_verify_t57_hotreload_test.go`（在 `/tmp/chr/cur` 副本里跑，避免往工作树塞测试文件），
驱动 `configWatchLoop(path, 25ms, stop)` + `configReloadOnce`，并用 `configMu` 守护读取运行时全局量确认「真的生效」。

```bash
$ cd /tmp/chr/cur && go test -count=1 -run 'TestT57' -v .
--- PASS: TestT57PureTouchDoesNotReload (0.40s)
    PASS 纯 touch: reload=0, mtime 保持 touch 值（初始 mtime=15:16:56.951）
--- PASS: TestT57RealChangeReloadsExactlyOnceAndApplies (0.43s)
    初始生效值: api_key="" text_only_models=[deepseek]
    PASS 一次真实修改: reload=1, api_key→k2, text_only_models→[zzB], 之后 250ms 无新增且 mtime 不再前进
--- PASS: TestT57TwoExternalChangesEachOnce (0.55s)
    PASS 连续两次外部修改: reload=2，最终 api_key=kB
--- PASS: TestT57NormalizedSelfWriteDoesNotRetrigger (0.43s)
    PASS 自写不重触发: reload=1，写回为规范化 JSON（179 字节）且 api_key 保持 kC
--- PASS: TestT57InvalidJSONWarnsOnceNoLoop (0.70s)
    PASS 坏 JSON: warn=1、reload=0、重复写盘不重复告警、恢复后 reload=1 且生效
--- PASS: TestT57DeleteAndRecreate (0.43s)
    PASS 删除/重建: 缺失期间 reload=0 且不 panic，重建后 reload=1 并生效
PASS   ok github.com/shiyanYX/opencode2api 2.957s
```

覆盖与判定：

| 断言 | 结果 |
|---|---|
| 纯 touch（内容不变）→ **0 条** reload，且程序不改写文件（mtime 保持 touch 值） | ✅ |
| 真实改字段值 → **恰好 1 条**，`api_key`/`text_only_models` **运行时真的变了**（不是只打日志） | ✅ |
| 之后 10 个间隔（250ms）无新增 reload、mtime 不再前进 | ✅ |
| 连续两次外部修改 → 各 1 条（累计 2），最终值 = 最后一次 | ✅ |
| 用户单行压缩 + 未知字段写法 → reload 1 条；程序规范化写回后**不重复触发**，且写回内容保留用户值 | ✅ |
| 坏 JSON → warn 1 次、reload 0、重复写同一坏内容不重复告警；恢复合法配置后 reload 1 且生效 | ✅ |
| 配置文件被删除期间 → 不 panic、0 reload；重建后 reload 1 且生效 | ✅ |


### 4.1 端到端（真二进制 + 真 5s 轮询 + 文件系统）—— 决定性证据

由**当前工作树**编译二进制（`/tmp/chr/oc2api-fixed`），以独立配置文件启动在 9151（不碰 8000/生产），
外部用 `printf`/`touch` 改文件，读进程日志 + `GET /api/config`（该接口用「磁盘 ∪ 运行时全局量」合并，
因此能同时观察「磁盘写了什么」与「运行时应用了什么」）。

```bash
$ /tmp/chr/e2e.sh        # 完整脚本：启动 → S1 touch → S2 改字段 → S3 再改字段
=== 启动 ===
reload=0
api/config: model_alias={'e2e-a': 'gpt-4o-mini'} api_key=k1 force_disable_thinking=False
mtime=2026-10-20 15:23:24.296817489
=== S1 纯 touch（内容一字不改，mtime +5s） ===
touch 后 mtime=2026-10-20 15:23:34.329563188
12s 后 mtime=2026-10-20 15:23:34.329563188        # ← 程序没改写文件
reload 增量=0（期望 0）
=== S2 外部真实修改：{"api_key":"k2"}（去掉 model_alias） ===
首次 reload 延迟≈4.43s                          # ← 5s 轮询的正常延迟
reload 增量=1（期望 1）
api/config: model_alias={'e2e-a': 'gpt-4o-mini'} api_key=k2 force_disable_thinking=False
磁盘内容（规范化写回，多行缩进）: "model_alias": null, "reasoning_effort_map": null, "force_disable_thinking": false, "api_key": "k2"
再等 12s: reload 总计=1（期望仍 1），mtime=2026-10-20 15:23:45.697409800（12s 前同值）
=== S3 第二次外部修改：{"api_key":"k3","force_disable_thinking":true} ===
reload 增量=1（期望 1，累计 2）
api/config: model_alias={'e2e-a': 'gpt-4o-mini'} api_key=k3 force_disable_thinking=True
再等 12s: 累计=1（期望仍 1），mtime 是否变化=否
=== 汇总 ===
日志中 reload 总行数=2        # S2 + S3 各 1 条，其余时间 0
日志中任何 invalid JSON 警告=0
```

**这一组是「没修死」的决定性证据**：
- S2 外部文件里**已删掉** `model_alias`，磁盘写回也是 `"model_alias": null`，但 `/api/config` 仍显示
  `{'e2e-a': 'gpt-4o-mini'}` —— 这正是 `applyConfig` 的语义（nil 字段不覆盖运行时），**证明运行时真的被应用了**，
  而不只是磁盘回显 / 只打了日志。`api_key=k2`、S3 的 `force_disable_thinking=True` 同样即时生效。
- S1 纯 touch **0 条**且 mtime 未被程序改写；S2/S3 各**恰好 1 条**，之后 12 秒不再增长、mtime 不再变 ⇒ 循环已断。

## 5. 语义判断：纯 touch 现在不热加载，是否可接受 —— 状态：✅ 可接受（不是退化）

任务书写的是「touch 一次 ⇒ 恰好 1 次 reload」，实现方落地为「**内容变了才 reload；纯 touch（内容不变）= 0 条**」。我的判断：**这个语义更正确，应接受**，理由：

1. **热加载的语义本体是「配置内容」**。mtime 只是「内容可能变了」的一个廉价代理信号；判据换成内容指纹后，
   「代理信号触发了，但内容与已应用的一致」⇒ 没有可加载的东西 ⇒ 不 reload 是正确的收敛，不是偷懒。
2. **原实现里 touch 会 reload 恰恰是缺陷的一半**：它 reload 后必然 `saveConfig` 写回，mtime 再变 ⇒ 循环。
   如果保留「touch 也 reload」，就必然要保留「自写抑制」逻辑（时间窗/进程内标记），那类方案会**漏掉用户在同一窗口内的真实修改**——
   正是热加载要解决的场景，等于用一个缺陷换另一个缺陷（实现方注释也这么说，我复核后同意）。
3. **谁在 touch**：(a) 运维 `touch config.json` 想「触发重载」——在修复前这么做的唯一效果是点燃循环；
   真要重载，正确做法是**改内容再改回来**或重启（项目也没有 reload API）；(b) 备份/同步工具改 mtime 但内容不变——
   这类工具本来就不该触发业务重载。(c) 编辑器保存但内容相同（只改缩进/换行）——实际上格式化差异会改变字节，
   仍会 reload 1 次（见 §4「规范化自写」用例：用户单行写法会触发 1 次），所以「内容不变」是严格的字节相等，很少误伤。
4. **代价**：极少数「想靠 touch 强制重载」的习惯失效。这属于未被文档化的隐式用法，且修复前的行为是缺陷。
   若将来真需要强制重载，应加显式的 `/api/config/reload` 或 `-reload-config` 信号，而不是依赖 touch。
5. **反向验证**：我没有只看它「不 loop」，还验证了 **真实内容修改 100% 生效**（§4 包内 + §4.1 端到端），
   即修复没有把热加载修死。

> 结论：**语义可接受**。唯一需要写进发布说明的是这句话：「`touch config.json` 不再触发重载（内容未变时），请改内容或重启」。



## 6. 变异测试（我自己造的 4 个，确认测试会红）—— 状态：✅ 4/4 CAUGHT

对**修复后副本**逐一注入变异，跑我同一套测试（预期「测试变红 = CAUGHT」，全绿反而说明测试太弱）。

| # | 变异 | 注入方式 | 结果 | 被哪条抓 |
|---|---|---|---|---|
| M1 | **退回 mtime 判定** | `configWatcherState` 加 `lastMtime`，判据换成 `!info.ModTime().After(lastMtime)`（不再比 hash） | **CAUGHT** | 6 条全红，如「启动后无修改却触发 6 次 reload」「第 1 次外部修改后期望 1 次，实际 5」 |
| M2 | **自写永远被当外部变更** | 指纹改记「读到的字节」（`st.lastHash = sum`）而不是写回后的字节 | **CAUGHT** | 「自写规范化后出现额外 reload: 总计 2（期望 1）」「两次修改后总计 4（期望 2）」「规范化自写后重复触发: 2」 |
| M3 | **reload 恒不触发**（把热加载修死） | `configReloadOnce` 读到文件后直接 `return AppConfig{}, false` | **CAUGHT** | 「真实改内容后期望 1 次，实际 0」「重建后期望 1 次，实际 0」等 5 条红 |
| M4 | **坏 JSON 不记指纹**（每次轮询都告警/重试） | 删掉 invalid 分支的 `st.lastHash, st.seeded = sum, true` | **CAUGHT** | 「坏 JSON 告警 11 次（期望 1~3 且不循环）」「坏内容停留期间告警仍在增长」 |

原始输出（摘录）：

```
================ 变异 m1 ================
--- FAIL: TestT57PureTouchDoesNotReload (0.15s)
    zz_verify_t57_hotreload_test.go:99: 启动后无修改却触发 6 次 reload（期望 0）
--- FAIL: TestT57RealChangeReloadsExactlyOnceAndApplies (0.15s)
    zz_verify_t57_hotreload_test.go:130: 启动期不该 reload，实际 5
--- 退出码: 1 ---
================ 变异 m2 ================
--- FAIL: TestT57RealChangeReloadsExactlyOnceAndApplies (0.43s)
    zz_verify_t57_hotreload_test.go:147: 自写规范化后出现额外 reload: 总计 2（期望 1）
--- FAIL: TestT57TwoExternalChangesEachOnce (0.53s)
    zz_verify_t57_hotreload_test.go:172: 两次修改后出现额外 reload: 总计 4（期望 2）
--- 退出码: 1 ---
================ 变异 m3 ================
--- FAIL: TestT57RealChangeReloadsExactlyOnceAndApplies (3.16s)
    zz_verify_t57_hotreload_test.go:134: 真实改内容后期望恰好 1 次 reload，实际 0
--- 退出码: 1 ---
================ 变异 m4 ================
--- FAIL: TestT57InvalidJSONWarnsOnceNoLoop (0.40s)
    zz_verify_t57_hotreload_test.go:222: 坏 JSON 期望只警告 1 次（同一次坏编辑），实际 11
--- 退出码: 1 ---
```

补充：M2 正是这次修复**最容易写错的那一行**（指纹记「读到的字节」而不是「写下去的字节」），我的测试对它有专门用例，
说明这套测试不是只覆盖「循环」一条，而是覆盖「为什么不能循环」。

## 7. 回归（全量 + -race + vet/gofmt）—— 状态：✅ 全绿

环境：`go1.26.5`，`GOCACHE=$PWD/.gocache GOMODCACHE=$PWD/.modcache GOTMPDIR=$PWD/.gotmp`（工作树，非副本）。

```bash
$ go test -count=1 ./
ok  	github.com/shiyanYX/opencode2api	159.685s
EXIT=0

$ T57_VERIFY=1 go test -count=1 -race -run 'TestT57' -v .
--- PASS: TestT57PureTouchDoesNotReload (0.40s)
--- PASS: TestT57RealChangeReloadsExactlyOnceAndApplies (0.43s)
--- PASS: TestT57TwoExternalChangesEachOnce (0.56s)
--- PASS: TestT57NormalizedSelfWriteDoesNotRetrigger (0.43s)
--- PASS: TestT57InvalidJSONWarnsOnceNoLoop (1.18s)
--- PASS: TestT57DeleteAndRecreate (0.46s)
ok  	github.com/shiyanYX/opencode2api	4.504s          # 无 DATA RACE（grep DATA RACE 无命中）

$ go vet ./...
（无输出，退出码 0）

$ gofmt -l main.go zz_verify_t57_hotreload_test.go
（无输出 = clean）
```

- 全量 159.7s **0 失败**（含实现方自己的 `zz_t55_config_watcher_test.go`；我的测试默认 **SKIP**，只有 `T57_VERIFY=1` 才跑，
  避免给常规全量测试引入 5s 级抖动/共享状态副作用）。
- `gofmt -l *.go` 还会列出 6 个**与本任务无关的既有**测试文件（`bench_test.go`、`fmt_test.go`、`zz_c5v2_*`、`zz_t23_*`、`zz_t30_*`），
  它们未被本次修改触碰（`git diff` 里没有它们），不计入本次结论。


## 8. 范围/无夹带 —— 状态：✅ 只动 watcher

```bash
$ sha256sum main.go
c5195b79bef3ff72cd312bce85f952510ddbd6d3727df10dd7fa7861d228350a  main.go

$ git diff --stat -- main.go
 main.go | 125 ++++++++++++++++++++++++++++++++++++++++++++--------------------
 1 file changed, 87 insertions(+), 38 deletions(-)

$ git diff -U0 -- main.go | grep '^@@'
@@ -7,0 +8 @@ import (                                   ← 只加了 "crypto/sha256"
@@ -1236,2 +1237,68 @@ func saveConfig(...)               ← 新增 configWatcherState / newConfigWatcherState / configReloadOnce / configWatchInterval / configWatchLoop
@@ -1239,28 +1306,15 @@ func startConfigWatcher()            ← 原 mtime 循环体换成起 goroutine 调 configWatchLoop
@@ -1269,5 +1322,0 @@ func startConfigWatcher()            ← 删除旧 apply/save/log
@@ -1275,2 +1324,2 @@ func startConfigWatcher()            ← 删除旧 apply/save/log
@@ -1279 +1328 @@ func startConfigWatcher()            ← 删除旧 apply/save/log
```

- **6 个 hunk 全部落在 `import` + `saveConfig`/`startConfigWatcher` 区域（1236–1328 行）**，未触碰任何其它函数。
- `git diff --name-only -- '*.go'` ⇒ **只有 `main.go`**。接线仍在 `main.go:7675 startConfigWatcher()`，生产间隔仍 `5s`（`:1302`）。
- 工作树里其它 dirty 文件**不属于本任务**，是别的任务线既有改动：`static/admin.html` 293/118 = U3 面板 rev2（md5 `4fe4f101…`，
  已由我上一轮独立验收）、`ROADMAP.md` +7 / `docs/research/F7-banner-refresh-verification.md` / `docs/mockups/u2/f7_stub.py`
  是 F7/U2 线；`docs/research/config-hotreload-fix.md` 是实现方自己的报告。
- 我方新增文件：`docs/research/config-hotreload-verification.md`（本报告）+ `zz_verify_t57_hotreload_test.go`（默认 SKIP 的验收测试），
  **未改任何生产代码**（复核前后 `main.go` sha256 恒为 `c5195b79…`）。
- 基线/变异副本在 `/tmp/chr/{head,cur,mut-m1..m4}`，未写进仓库。


## 9. 量化：修复收益 —— 状态：✅ 已算

**基线（生产实测，来自任务书/审计）**：10-09 19:08 → 10-20 11:09（10.67 天）共 **67,763 条** `config hot-reloaded`，
6.7 MB，占运行日志 **9.1%**。

| 指标 | 修复前 | 修复后（稳态，无外部编辑） | 降幅 |
|---|---|---|---|
| `config hot-reloaded` 条数 | 67,763 / 10.67 天 ≈ **6,351 条/天**（≈4.4 条/分，5s 轮询量级） | **0 条/天**（每次真实外部编辑仅 +1） | **≈100%** |
| 运行日志体积 | 6.7 MB / 10.67 天 ≈ **0.63 MB/天**（占总量 9.1%） | 0 | **−9.1%** |
| `config.json` 写入次数 | 每 5 秒一拍 ⇒ 循环期 ≈ 6,351 次/天（理论上限 17,280 次/天） | 0；每次真实编辑 1 次 | **≈100%** |
| `config.json` mtime churn | 每 5 秒前进一次（备份/同步工具会跟着动） | 只在真实编辑时变 | 同左 |
| 磁盘写放大 | ≈1.3 MB/天（按 200B/次 × 6,351） | ≈0（仅编辑时） | ≈100% |

- 修好后日志里只剩：启动 0 条（用磁盘内容做种子）、每次真实外部编辑 **1 条**、坏编辑 ≤1 条 warn。
- 若担心「上报的 9.1% 被低估」：按 5s 满速上限算是 17,280 条/天 ≈ 1.7 MB/天，仍与实测同量级，收益结论不变。

## 10. go / no-go —— 状态：✅ 结论

# **go**

修复有效、未把热加载修死、测试有牙齿（4/4 自造变异全 CAUGHT）、回归全绿、无夹带。**可以作为 task-55 的验收通过。**

### 放行附带 3 条（均不阻断）

1. **发布说明必须写一句**：「`touch config.json` 不再触发重载；要热加载请改内容」。这是唯一可感知的语义变化（§5 判定：可接受）。
   若团队确实需要「强制重载」，后续加显式 `/api/config/reload` 或信号，而不是恢复 mtime 判据。
2. **非原子写入的瞬态告警（残余、无害）**：外部用 `echo > file` 这类「截断再写」的方式编辑时，watcher 可能读到
   截断后的空/半截内容，从而多打 1~2 条 `invalid JSON` warn（每条对应一个不同的中间内容指纹）。**不会循环**，
   且最终完整内容一定仍会 reload（指纹不同）。我的测试对告警次数留了 1~3 的容差并验证「稳定期不再增长」；
   若要彻底消掉，可在 watcher 里对同一 mtime 的连续读取做 debounce，但收益极小，**不建议本轮做**。
3. **实现方的 `zz_t55_config_watcher_test.go` 继续保留**（全量测试里 PASS）。我的验收不依赖它；两套测试并存无冲突。

### 若未来出现 no-go 的最小修法（备查）

保持「内容指纹」判据不变，只修「指纹记什么」这一行：`saveConfig` 成功后**重新读回文件**并用读回字节算指纹（当前实现已如此）。
若再回归，优先查这一点，而不是回到 mtime 或加时间窗抑制。

## 11. 复跑方法（给 Lead / 下一位验收方）

```bash
cd /home/jingyx/project/opencode2api-test
export PATH=$PATH:$HOME/.local/opt/go/bin GOCACHE=$PWD/.gocache GOMODCACHE=$PWD/.modcache GOTMPDIR=$PWD/.gotmp
T57_VERIFY=1 go test -count=1 -run TestT57 -v .        # 6 条断言
T57_VERIFY=1 go test -count=1 -race -run TestT57 .     # race
go test -count=1 ./                                     # 全量
# 基线复现 / 变异：/tmp/chr/{head,cur,mutation.sh,e2e.sh}（脚本原文见本报告各节）
```
