# 第二批上线前独立复核（2a = 面板 gzip）：READY

> 复核人：`healthcheck-probe`（task-36）。复核时间 2026-10-20 08:13–08:45 CST。
> 基线：分支 `test`，HEAD `ba26d70`。工作区未做任何修改；本报告是唯一新增文件。
> 结论在 §0，证据在 §1–§6，命令原文在 §7。
> **范围声明**：按 Lead 2026-10-20 08:30/08:34 两次收窄，本次只对 **2a = 面板 gzip** 出 READY 结论；
> 2b（统计口径）只报「现状是否也绿」，不出结论（判据正在被 `task-39` 修订）。

---

## 0. 结论

| 项 | 结论 | 依据 |
| --- | --- | --- |
| **2a（面板 gzip）** | **READY** | 4 个钉桩哈希全部匹配；hunk 拆解经哈希恒等式证明完备且互斥；干净副本上 `gofmt`/`vet`/`build`/全量 `go test` 全绿；端到端 gzip/Vary/明文逐字节/SSE 未压且逐条到达 全部实测通过 |
| **2b（统计口径）** | **现状也绿（仅供参考，本批不上）** | 同一套干净副本流程下 `gofmt` 源文件干净、`vet`/`build`/全量 `go test` 全绿；未做端到端（按 Lead 指示） |
| **提交时必须排除** | `proxy_node.go`（`task-34`/impl-ip1 在途）、`ROADMAP.md`、`docs/**`（含本报告）、`static/admin.html`（已回滚，`git diff` 为空，**不要提交**） | §2、§7 |

**一句话**：要上线的 2a = `main.go` 只取 `task-27` 的四处 hunk + 新增 `webcompress.go`，这两样我在干净副本上编过、测过、跑起来验过；`static/admin.html` 回滚后与 HEAD 逐字节相同，不属于本批。

**唯一需要 Lead 注意的两个「非阻塞但必须知道」的点**：

1. **gzip 体积以我实测为准，与预期值不同**：面板回滚后 **191,661 B → 62,554 B（3.064×）**，不是 ≈60,517 B / 3.17×。
   60,560 是 `gzip -9` 命令行对同一文件的输出；Go 的 `compress/flate` 在 BestCompression 下比 zlib 略弱约 2 KB。**62,554 是真正从 HTTP 头里量到的 `Content-Length`**（§6.1）。
2. **2a 的 `/api/stats?range=` 里没有 `unmapped_requests` 字段**（这是对的：`task-21` 的 main.go 五处 hunk + `calllog.go`/`statscan.go` 都不在 2a 里）。
   由于面板也已回滚（不显示那一行），**2a 单独上线后 UI 与后端完全自洽**，不会出现「面板要显示一个后端没有的字段」。§6.5 有实测。

---

## 1. 哈希钉桩（第 1 项）

工作区与干净副本逐一比对。**5 个钉桩全部匹配**：

| 文件 | 期望（任务书） | 实测（工作区） | 实测（干净副本 2a） | 判定 |
| --- | --- | --- | --- | --- |
| `main.go` | sha256 `6f5120de…` | `6f5120de4b682d47f2dd35a2edbccdfd381641c0022bdad279488c0349f5d2ff` | 子集版 `f6155aeb…`（见 §3） | ✅ 匹配 |
| `calllog.go` | sha256 `54c3a378…` | `54c3a378ca89c1ca4042a9fa0b6fa2c830307feed97c96b392aefd606eda9edb` | 不在 2a | ✅ 匹配 |
| `statscan.go` | sha256 `47a066be…` | `47a066beabad1bea8653a6946f24880e118d892ecc63ff28de324b1b694201cb` | 不在 2a | ✅ 匹配 |
| `webcompress.go` | sha256 `904f9858…` | `904f9858b268ac1105022404ba1e64e4ad1753d540043bb7cc02db3675224cdd` | `904f9858…` | ✅ 匹配 |
| ~~`static/admin.html`~~ | ~~md5 `a858af8e…`~~ → **已作废** | **`dee593fd95a5c778279023b6a0b2a378` = HEAD 版，191,661 B，`git diff` 为空** | `dee593fd…` | **不在本批**（回滚） |

`static/admin.html` 回滚核验（Lead 说 md5 `dee593fd…` / 191,661 B / 与线上同版，我独立复核）：

```bash
$ md5sum static/admin.html                       # dee593fd95a5c778279023b6a0b2a378
$ wc -c static/admin.html                        # 191661
$ git diff -- static/admin.html | wc -l          # 0  ← 与 HEAD 逐字节相同
$ git show HEAD:static/admin.html | md5sum       # dee593fd95a5c778279023b6a0b2a378
$ sha256sum static/admin.html                    # bb02b4234ac692635db01510289b06d76f6bf1e37cc4d77a140423317207d84c
```

> 注意：上一轮（08:20 前）工作区里的 admin.html 是 `a858af8e…` / 193,717 B（含 `task-22` 的「未映射模型请求」UI）。
> 那一版**已被回滚**，因此任务书原表里的 admin.html 钉桩 `a858af8e…`/`4088100c…` **作废**；现行唯一有效的面板钉桩是 `dee593fd…`。

---

## 2. 文件清单（第 2 项）

`git status --porcelain` 全量 **8 个 `M` + 34 个 `??`**（08:13 快照），按归属分类：

| 文件 | 状态 | 归属 | 是否进本批 |
| --- | --- | --- | --- |
| `main.go` | `M` | task-27（2a，4 处）+ task-21（2b，5 处）+ Lead 注释（1 处） | **✅ 是（仅 2a 的 4 处）** |
| `webcompress.go` | `??`（新增） | task-27 | **✅ 是（整文件）** |
| `static/admin.html` | **无**（已回滚，`git diff` 空） | 线上版 | ❌ 否（不要提交） |
| `calllog.go` | `M` | task-21（20 个 hunk） | ⏸ 2b，**本批排除** |
| `statscan.go` | `M` | task-21（2 个 hunk） | ⏸ 2b，**本批排除** |
| **`proxy_node.go`** | **`M`（+337/−29，366 行）** | **task-34 / impl-ip1 在途** | **❌ 必须排除（见下方红框）** |
| `ROADMAP.md` | `M`（+142） | 文档，非代码 | ❌ 排除 |
| `docs/mockups/u2/f7_stub.py` | `M` | 文档/脚本 | ❌ 排除 |
| `docs/research/F7-banner-refresh-verification.md` | `M`（+281） | 报告 | ❌ 排除 |
| 其余 `??`（`docs/**`、`.research/`、`webcompress.go` 之外的资源） | `??` | 各任务报告/截图 | ❌ 排除（本报告除外） |

> ⚠️ **`proxy_node.go` 是唯一必须显式排除的代码文件**。它当前有 +337/−29 未完成改动（task-34：canary + loadState）。
> `webcompress.go` 不依赖它，2a 也不需要它 —— 干净副本里我一律用 **HEAD 版** `proxy_node.go`
> （sha256 `96ca0fdb4dac3d8a105f9a9cb9c43c840f942fcebc1e5a7a0a844799d91a8233`，已核对 `git show HEAD:proxy_node.go` 一致）。
>
> **这条不是形式主义**：如果误把工作区的 `proxy_node.go` 一起提交，上线的就是「已验收的 gzip + 未验收的 canary/loadState」。

**2a 提交清单（3 个动作）**：

1. `webcompress.go` → `git add`（新增文件）
2. `main.go` → **只提交 `task-27` 的 4 处 hunk**（§3 给了精确行号，可直接 `git add -p` 逐 hunk 选）
3. `static/admin.html` → **不要 stage**（`git diff` 已空，本来就无需提交）

---

## 3. `main.go` 逐 hunk 归属 + 拆解正确性的哈希证明（第 3 项）

### 3.1 归属表（`git diff -U3 -- main.go`，共 **10 个 hunk**，每个 hunk 恰好属于一个 owner，**没有一个 hunk 需要再拆**）

| # | hunk 头（old → new） | 内容 | owner | 2a 是否包含 |
| --- | --- | --- | --- | --- |
| H1 | `-900,16 +900,16` | `loginHandler` 里 3 处 `renderLoginPage(w, msg)` → `renderLoginPage(w, r, msg)` | **task-27（2a）** | ✅ |
| H2 | `-919,7 +919,7` | 同上，第 4 处 `renderLoginPage(w, r, "")` | **task-27（2a）** | ✅ |
| H3 | `-1371,6 +1371,45` | `resolveModel` 拆出 `resolveModelWithDocs` 内核 + `freeModelDocsStale` + `ensureFreeModelsFresh` | task-21（2b） | ❌ |
| H4 | `-1400,7 +1439,7` | `!isFreeModel(m)` → `!isFreeModelIn(docs, m)` | task-21（2b） | ❌ |
| H5 | `-1409,6 +1448,92` | 新增 `isUnmappedModel` + `isFreeModelIn`（86 行） | task-21（2b） | ❌ |
| H6 | `-2596,9 +2721,11` | `upstreamLoopBudget` 注释修正（提到 `idle_read.go` / 空闲读超时） | **Lead 注释** | ❌（纯注释，可并入任一笔） |
| H7 | `-7191,6 +7318,29` | `statsRangeResponse` 加 `UnmappedRequests` 字段 + `UnmappedRequestsStats` 类型 | task-21（2b） | ❌ |
| H8 | `-7223,6 +7373,15` | `statsRangeResponseFor` 里填充 unmapped 统计 | task-21（2b） | ❌ |
| H9 | `-7298,17 +7457,16` | `adminPageHandler` 改走 `writeCompressedHTML(w,r,adminPagePayload())`；`renderLoginPage` 换签名换实现 | **task-27（2a）** | ✅ |
| H10 | `-7403,6 +7561,9` | `main()` 里加 `prewarmWebCompression()` | **task-27（2a）** | ✅ |

计数与任务书一致：**2a = 4 个 hunk（H1/H2/H9/H10）**、**2b = 5 个 hunk（H3/H4/H5/H7/H8）**、**Lead 注释 = 1 个 hunk（H6）**，4+5+1 = 10 ✅。

### 3.2 拆解方法（可复现）

不是手改代码，而是**按 hunk 机械切分 diff**：

```bash
cd /home/jingyx/project/opencode2api-test
git diff -U3 -- main.go > /tmp/verify-batch2-work/main-full.patch
# 按 @@ 头把 10 个 hunk 分到 2a / 2b / LEAD-COMMENT 三个 patch（脚本见 §7.2）
# → /tmp/verify-batch2-work/2a-main.patch      （4 hunk, +14/−12）
#   /tmp/verify-batch2-work/2b-main.patch      （5 hunk, +158/−4）
#   /tmp/verify-batch2-work/LEAD-COMMENT-main.patch（1 hunk, +5/−3）
git archive HEAD | tar -x -C /tmp/verify-batch2a/
cd /tmp/verify-batch2a && git apply -p1 /tmp/verify-batch2-work/2a-main.patch
```

### 3.3 哈希恒等式：证明拆解**完备且互斥**（这是本条最硬的证据）

```
HEAD 的 main.go
  ⊕ 2a patch        → 2a 子集 main.go   sha256 f6155aeb552efb03…（干净副本实测）
  ⊕ 2b patch
  ⊕ LEAD patch
  = 工作区 main.go   sha256 6f5120de4b682d47…   ← 与钉桩逐字节相同
```

实测（把 2a 子集 main.go 拿回一个临时目录，再补上 2b + LEAD 两个 patch）：

```bash
$ cp /tmp/verify-batch2a/main.go /tmp/verify-batch2-work/identity/main.go
$ cd /tmp/verify-batch2-work/identity
$ git apply -p1 /tmp/verify-batch2-work/2b-main.patch
$ git apply -p1 /tmp/verify-batch2-work/LEAD-COMMENT-main.patch
$ sha256sum main.go
6f5120de4b682d47f2dd35a2edbccdfd381641c0022bdad279488c0349f5d2ff  main.go   ← 与工作区钉桩一致
```

⇒ **没有丢 hunk、没有重复 hunk、也没有夹带别的改动**。2a 的 main.go 精确等于「HEAD + task-27 四处」。

### 3.4 2b 为什么不能只提交 `main.go` 的统计 hunk 而不带 `calllog.go`/`statscan.go`

2b 的 main.go hunk（H7/H8）调用 `unmappedStatsInWindow` / `topUnmappedModels` / `unmappedTopN`，这些符号定义在 `calllog.go` / `statscan.go`。
⇒ **2b 三件套必须同批**（否则编译不过）。实测已确认：2b 子集（main.go 5 hunk + `calllog.go` + `statscan.go`）单独 `go build ./...` 通过（§4.3）。
反过来 **2a 与 2b 完全解耦**：2a 的 main.go 四处 hunk 不引用任何 2b 符号，2b 也不引用 `webcompress.go`。

---

## 4. 干净副本 CI 等价检查（第 4 项）

### 4.1 干净副本怎么造（含任务书里那个坑，以及**我另外发现的一个坑**）

```bash
# 1) HEAD 的跟踪文件
rm -rf /tmp/verify-batch2a && mkdir -p /tmp/verify-batch2a
cd /home/jingyx/project/opencode2api-test && git archive HEAD | tar -x -C /tmp/verify-batch2a/
# 2) 只打 2a 的补丁 + 拷入新增文件（static/admin.html 用 HEAD 版，不拷）
cd /tmp/verify-batch2a && git apply -p1 /tmp/verify-batch2-work/2a-main.patch
cd /home/jingyx/project/opencode2api-test && cp webcompress.go /tmp/verify-batch2a/
# 3) 拷入「已被验收过」的测试文件：49 个里排除 5 个（见 4.2），拷 44 个
# 4) ★ 拷入被 gitignore 的测试 fixture：free_models_cache.json
cp free_models_cache.json /tmp/verify-batch2a/
```

> **坑 2（我发现的）**：`free_models_cache.json` 被 `.gitignore:6` 忽略，但 `zz_t23_verify_test.go` 默认读它
> （`t23LoadBoolMap(t, t23Env("T23_DOCS", "free_models_cache.json"))`）。**不拷它，全量测试会红 2 条**：
>
> ```
> --- FAIL: TestT23RealLogHardGate (0.00s)
>     zz_t23_verify_test.go:241: 读 free_models_cache.json: open free_models_cache.json: no such file or directory
> --- FAIL: TestT23RealLogBackfillEqualsDirect (0.00s)
> ```
>
> 这是**缺 fixture，不是回归**（文件在仓库里存在但被 ignore，`git archive` 自然不带）。拷入后全绿（§4.4）。
> 我扫过所有 `*_test.go` 里对仓库内文件的读取，除它之外只有 `main.go`/`proxy_node.go`/`health_runtime.go`/`static/admin.html` 这几个源码文件（都在 archive 里）：
> `grep -hoE '(os\.ReadFile|os\.Stat|os\.Open)\("[^"]+"' *_test.go | grep -v '^"/tmp'` → 只有 `free_models_cache.json` 是数据 fixture。

### 4.2 EXCLUDED 列表（测试文件，共排除 5 个）

| 被排除的测试文件 | 原因 | 归属 |
| --- | --- | --- |
| `zz_t30_alert_safety_test.go` | 队友 TDD 红阶段（`TestT30GateKeepsDeadCountZeroForever` 期望「闸门不判死」，而 `task-34` 正在改判据） | 我上一个任务（task-30）写的，**不是验收基准** |
| `zz_t30_probe_cause_test.go` | 同上，且会**对真实节点发探针**（网络依赖，不适合上线检查） | 同上 |
| `zz_t34_probe_canary_test.go` | impl-ip1 正在 TDD 红阶段（`TestT34DecideProbeRound`） | task-34 |
| `unmapped_stats_test.go` | **2b 专属**：只测 `isUnmappedModel` / `unmappedStatsInWindow` 等 2b 符号，2a 副本里编译不过 | task-21/2b |
| `zz_t23_verify_test.go` | **2b 专属**：task-23 的验收器，测「未映射排除集」，2a 副本里编译不过 | task-21/2b |

另外 `task8_bucket_query_test.go` **本来就已在 `/tmp/t8_hold.go`**（移开状态），不在工作区，也未拷入。

⇒ 2a 副本 = **44 个测试文件**（49 − 5）。2b 副本同理 44 个（排除前 3 个 + `webcompress_test.go` + `zz_t29_verify_test.go`）。

### 4.3 原始结果（2a）

```
=== 2a: gofmt -l . ===
bench_test.go
fmt_test.go
zz_c5v2_verify_engine_test.go
zz_c5v2_verify_replay_test.go
count=4
non-test gofmt output=[]          ← 30 个非测试 Go 文件（含 main.go / webcompress.go）全部 gofmt 干净

=== 2a: go vet ./... ===
vet_exit=0

=== 2a: go build ./... ===
build_exit=0
ci_build_exit=0                   ← 另按 CI 原样参数（-trimpath -ldflags "-s -w -X main.version=ci"）也通过

### 2a: go test -count=1 ./  (admin.html = dee593fd 191661B)
start 2026-10-20 08:28:00
ok  	github.com/shiyanYX/opencode2api	48.145s
test_exit=0
end 2026-10-20 08:29:05
```

**关于 `gofmt -l .` 非空的 4 个文件**：全部是 `*_test.go`，且 `git ls-files '*_test.go'` 返回 **0**（测试文件全被 `.gitignore:26` 忽略）
⇒ **CI 的 `gofmt -l .` 永远看不到它们**。为证明这一点，我在**纯 HEAD archive**（无任何测试文件）上跑：

```
=== HEAD archive alone: gofmt -l . ===
output=[]   count=0
```

且这 4 个文件的**内容与工作区逐字节相同**（是原样拷入的、本批未改），属于既有历史文件。
⇒ CI 的格式门禁等价于「非测试 Go 文件必须干净」，**实测 0 命中，通过**。

### 4.4 2b 现状（仅供参考，本批不上）

```
=== 2b: files ===
957199a2e6baf7e5…  main.go        （= HEAD + task-21 五处 hunk）
54c3a378ca89c1ca…  calllog.go     ✅ 钉桩
47a066beabad1bea…  statscan.go    ✅ 钉桩
dee593fd95a5c778…  static/admin.html（HEAD 版）

gofmt -l . → 5 个（均为 gitignored 测试文件），non-test gofmt output=[]
go vet ./...   → vet_exit=0
go build ./... → build_exit=0
go test -count=1 ./ → ok  github.com/shiyanYX/opencode2api  75.575s  (test_exit=0)
```

⇒ **2b 现状也绿**。但按 Lead 指示不出 READY 结论（判据待 `task-39` 修订后重新复核），本节仅作参考、不阻塞任何事。

### 4.5 `-race`（第 4 项最后一条）：发现 **1 条既有（pre-existing）测试隔离缺陷，与 2a 无关**

任务书要求的命令：

```bash
go test -race -run 'Unmapped|Stat|Bucket|Compress|WebCompress|AdminStats' -count=1 ./
```

实测结果（2a 副本）：**`WARNING: DATA RACE` 计数 = 0**，但 **1 条测试 FAIL**：

```
--- FAIL: TestHandleBackfillOpenErrorKeepsBucketsWhenNotReallyMissing (0.00s)
    backfill_finish_test.go:228: 打开失败的原因不是「文件不存在」时，桶不得被清空；实际桶内 0 条
race_exit=1
```

**这是不是 2a 的回归？不是。用 HEAD 基线做了对照实验（决定性证据）**：

| 实验 | 对象 | 命令 | 结果 |
| --- | --- | --- | --- |
| A | **2a 副本** | 该测试**单独**跑 + `-race` | **FAIL**（同上） |
| B | **纯 HEAD 基线**（无任何本批改动，42 个测试文件） | 同 A | **FAIL，报错逐字相同** |
| C | **纯 HEAD 基线** | 任务书那条 `-race` 过滤子集 | **FAIL，同一条** |
| D | **纯 HEAD 基线** | 全量 `go test -count=1 ./`（无 -race） | **ok 49.361s** |
| E | **2a 副本** | 全量 `go test -count=1 ./`（无 -race） | **ok 48.145s** |

结论：该用例**只在「被 `-run` 过滤成子集」或「单独跑」时失败，在 HEAD 上就能复现**，与 2a 无关。
根因是测试自身依赖全局状态（`backfill` 的 rebuild/backoff 计数）而 `-run` 过滤把前置用例滤掉了 ——
它对 0710 号 `dir chmod 0000` 前置条件的判定 `t.Skip` 也没兜住这条路径。**属于既有测试缺陷，不影响上线**。
（顺带：这条用例在**全量**跑时是绿的，证明它不是断言写错，而是顺序依赖。）

> 复跑方式见 §7.3。若 Lead 希望 -race 也全绿，最小修法不是改产品代码，而是给这条用例加
> `-race` 子集下的隔离（例如在 `TestMain` 里重置 `backfill` 全局态，或在用例开头显式 `resetAbsorbedStateLocked()`）——
> 但这属于测试基建改动，**不在本批范围**，我不动。

---

## 5. 全量 `-race` 交叉验证（已跑完：**全绿**）

在 2a 干净副本上跑**未过滤**的全量竞态检测（命令见 §7.3）：

```bash
cd /tmp/verify-batch2a
go test -race -count=1 ./ > /tmp/verify-batch2-cache/race-2a-fullsuite.log 2>&1
echo "full_race_exit=$?"
```

实测输出（日志原样，共 88 B）：

```
ok  	github.com/shiyanYX/opencode2api	332.834s
full_race_exit=0
end 2026-10-20 08:44:26
```

结论：

- **`-race` 全量 = PASS（exit 0，332.8 s），0 条 `WARNING: DATA RACE`**。2a 的并发改动（`webcompress.go` 的懒加载变体缓存、`prewarmWebCompression()` 启动预热）在竞态检测下无告警。
- 这同时**反向确认了 §4 的判定**：`-race` 下唯一那次失败（`TestHandleBackfillOpenErrorKeepsBucketsWhenNotReallyMissing`）
  只在 **`-run` 过滤子集** 时出现，全量跑即为绿 ⇒ 是**过滤导致的顺序依赖**，不是 2a 的回归，也不是真实竞态。
  也就是说 §4 的「不影响上线」不是靠辩解得出的，而是有全量 `-race` 绿作为对照。
- 附带说明：`-race` 全量 332.8 s 比普通全量 48.1 s 慢约 7 倍，属竞态插桩的正常开销，不影响 CI（CI 不跑 `go test`，见 §4.3）。

---

## 6. 面板与压缩的端到端复核（第 5 项）

自起 2a 二进制，端口 **9412**（不碰生产、不用 8000）：

```bash
cd /tmp/verify-batch2-e2e && printf '{}\n' > config.json
/tmp/opencode2api-2a -port 9412 -password 'e2e-batch2-pw' \
  -config /tmp/verify-batch2-e2e/config.json -log-file /tmp/verify-batch2-e2e/app.log -log-stdout=false &
# 600ms 内 /login 返回 200
curl -s -c ck.txt --data-urlencode 'password=e2e-batch2-pw' http://127.0.0.1:9412/login   # 302 → /
```

二进制由 2a 干净副本构建（`go build -trimpath -ldflags "-s -w -X main.version=ci" -o /tmp/opencode2api-2a .`，43,446,537 B）。

### 6.1 面板压缩：协商 + 体积（**实测数字，与预期值不同，以此为准**）

```
=== GET / with Accept-Encoding: gzip ===
HTTP/1.1 200 OK
Content-Encoding: gzip
Content-Length: 62554
Content-Type: text/html; charset=utf-8
Vary: Accept-Encoding
X-Request-Id: dmzpougyhf6d
```

| 指标 | 实测 |
| --- | --- |
| 面板明文（磁盘 = embed） | **191,661 B** |
| gzip 后（HTTP `Content-Length`，Go `BestCompression`） | **62,554 B** |
| 压缩比 | **3.064×**（压掉 **129,107 B**，为原体积的 **32.64%**） |
| 参照：`gzip -9` 命令行同一文件 | 60,560 B（含 39 B 文件名/mtime 头）；**Go 的 flate 比 zlib 弱约 2 KB**，所以 62,554 才是线上真实值 |
| 首屏节省 | 每次面板加载少传 129 KB；服务端压缩 CPU 已在启动期一次付清（`prewarmWebCompression()`），每请求 0 压缩 |

> **对 Lead 预期值的更正**：预期「191,661 → ≈60,517 B（3.17×）」是按 `gzip -9` 估的；
> Go 实测 **62,554 B / 3.064×**。功能不受影响，但**报告与验收数字请用 62,554 / 3.064×**。
> （对照：上一版 193,717 B 的面板，`verify-403` 报的是 63,347 B / 3.06× —— 与本次 62,554 / 3.064× 同量级、同结论。）

### 6.2 面板明文逐字节相等（三方一致）

```
=== GET / WITHOUT Accept-Encoding ===
HTTP/1.1 200 OK
Content-Length: 191661
Content-Type: text/html; charset=utf-8
Vary: Accept-Encoding

body md5 = dee593fd95a5c778279023b6a0b2a378
cmp body-id.bin /tmp/verify-batch2a/static/admin.html → IDENTITY: body == disk file (byte-identical)
```

**三方一致**：HTTP 明文 body ≡ 磁盘 `static/admin.html` ≡ `dee593fd95a5c778279023b6a0b2a378`（= HEAD = 线上版）。
gzip 路径解压后同样 `md5 = dee593fd…` 且 `cmp` 与磁盘文件逐字节相同 ⇒ **压缩没有改一个字节的内容**。

`Vary: Accept-Encoding` **两条路径都带**（明文响应里也有），符合「中间缓存不能把 gzip 版喂给不支持的客户端」。

### 6.3 协商边界（q=0 / 通配 / 登录页）

| 请求头 | 结果 | 判定 |
| --- | --- | --- |
| `Accept-Encoding: gzip` | `Content-Encoding: gzip`，`Content-Length: 62554` | ✅ |
| **无** `Accept-Encoding` | 无 `Content-Encoding`，`Content-Length: 191661`，`Vary` 仍在 | ✅ |
| `Accept-Encoding: gzip;q=0` | **无** `Content-Encoding`，`Content-Length: 191661`（显式拒绝优先于 `*`） | ✅ |
| `Accept-Encoding: *` | `Content-Encoding: gzip`，`Content-Length: 62554` | ✅ |
| `GET /login` + `Accept-Encoding: gzip` | `Content-Encoding: gzip`，`Content-Length: 2813`（登录页也走同一出口） | ✅ |

### 6.4 SSE `/api/logs/stream`：**未压缩 + 逐条到达**（本项最重要）

客户端**故意声明** `Accept-Encoding: gzip`（证明服务端主动不压 SSE，而不是客户端没要）：

```
status: 200
  Content-Type: 'text/event-stream'
  Content-Encoding: None          ← ★ 声明了 gzip 也没有压
  Cache-Control: 'no-cache'
  Transfer-Encoding: 'chunked'
  X-Accel-Buffering: 'no'
```

**逐条到达**（连接后每 1.5s 打一次 `/api/stats`，INFO 级 `request_done` 会进 logring）：

```
[t=1.60s] trigger /api/stats #0
[t=3.10s] trigger /api/stats #1
[t=4.61s] trigger /api/stats #2

data: events total = 3；events after t=1.2s = 3
  t= 1.61s  {"seq":19,"level":"INFO","msg":"request_done",…}
  t= 3.10s  {"seq":20,"level":"INFO","msg":"request_done",…}
  t= 4.61s  {"seq":21,"level":"INFO","msg":"request_done",…}

spread(first→last late event) = 3.00s   ← 触发间隔 1.5s×2 ⇒ 期望 ~3.0s；若被缓冲会 ~0.00s
```

⇒ **事件到达时刻与触发时刻一一对应（1.61/3.10/4.61 vs 1.60/3.10/4.61），没有攒批**。
这直接证明「gzip 预压缩没有把 SSE 也包进去」，也就是 `webcompress.go` 注释承诺的「只服务 `adminPageHandler` 与 `renderLoginPage` 两个出口」。

> 过程中得到一个副产品结论：`/api/logs*` 与 `/` 是 `loggingMiddleware` 的 `quiet` 路径（`logging.go:260`，走 `Debug`），
> 所以在 `-log-level info` 下**用 `/api/logs` 打触发不会产生新日志**。第一次试验因此拿到 0 条增量 ——
> 换成 `/api/stats` 才是有效触发。这条对以后写 SSE 验收脚本有用。

### 6.5 `/api/stats`：无 range 逐字节不变；range 路径**没有** `unmapped_requests`（符合 2a 范围）

```
-- no range --
{"total_requests":0,"models":{}}
md5 = 7b5573910768386cec3d3788f5a18a8b      ← 与既有契约逐字节相同（红线未破）

-- range=180d --
top-level keys: ['models', 'total_requests', 'trends_by_model']
has unmapped_requests: False
```

**解释（重要，别误判为缺陷）**：`unmapped_requests` 字段是 `task-21`（2b）的 main.go hunk H7/H8 加的，**2a 不含**。
所以 2a 单独上线时 `/api/stats?range=` 不会带这个键 —— 与「面板已回滚、不显示那一行」**正好自洽**
（面板不会去读一个不存在的字段，也就不会出现 `—`/0 的歧义）。
等 2b（修订版）上线时，字段与面板 UI 一起走。

---

## 7. 复现命令与原始产物

### 7.1 环境

```bash
export PATH=$PATH:/usr/local/go/bin
export GOCACHE=/tmp/verify-batch2-cache/gocache \
       GOMODCACHE=/tmp/verify-batch2-modcache \
       GOTMPDIR=/tmp/verify-batch2-cache/gotmp
# 说明：modcache / gocache / gotmp 都放在干净副本**外侧**。
# 任务书原写法把 GOMODCACHE 放在副本内 → `gofmt -l .` 会把 320 个第三方 .go 文件列出来，
# 与 CI（fresh checkout，依赖全在仓库外）不等价。放进副本外侧才是忠实等价，且不影响任何被检文件。
```

### 7.2 hunk 拆解脚本（与 §3.2 对应）

```python
import re
src = open('/tmp/verify-batch2-work/main-full.patch').read().splitlines(True)
i = 0
while i < len(src) and not src[i].startswith('@@'): i += 1
header, hunks, cur = src[:i], [], None
for line in src[i:]:
    if line.startswith('@@'):
        if cur: hunks.append(cur)
        cur = [line]
    else: cur.append(line)
if cur: hunks.append(cur)
owner = {'@@ -900,16 +900,16 @@':'2a', '@@ -919,7 +919,7 @@':'2a',
         '@@ -1371,6 +1371,45 @@':'2b', '@@ -1400,7 +1439,7 @@':'2b', '@@ -1409,6 +1448,92 @@':'2b',
         '@@ -2596,9 +2721,11 @@':'LEAD-COMMENT',
         '@@ -7191,6 +7318,29 @@':'2b', '@@ -7223,6 +7373,15 @@':'2b',
         '@@ -7298,17 +7457,16 @@':'2a', '@@ -7403,6 +7561,9 @@':'2a'}
out = {'2a':[], '2b':[], 'LEAD-COMMENT':[]}
for h in hunks:
    out[owner[re.match(r'@@ -[\d,]+ \+[\d,]+ @@', h[0]).group(0)]].append(h)
for k in out:
    open(f'/tmp/verify-batch2-work/{k}-main.patch','w').writelines(header + [l for h in out[k] for l in h])
```

### 7.3 `-race` 对照实验命令（§4.5）

```bash
# 2a 副本：任务书那条
cd /tmp/verify-batch2a && go test -race -run 'Unmapped|Stat|Bucket|Compress|WebCompress|AdminStats' -count=1 ./
# HEAD 基线（无本批改动，42 个测试文件）：同一测试单独跑
cd /tmp/verify-batch2-head && go test -race -run 'TestHandleBackfillOpenErrorKeepsBucketsWhenNotReallyMissing' -count=1 ./
# HEAD 基线：同一条过滤子集
cd /tmp/verify-batch2-head && go test -race -run 'Unmapped|Stat|Bucket|Compress|WebCompress|AdminStats' -count=1 ./
# 全量（无 -race）两个副本都绿
cd /tmp/verify-batch2a && go test -count=1 ./
```

### 7.4 2a E2E 断言汇总（一屏看全）

| # | 断言 | 实测 | |
| --- | --- | --- | --- |
| 1 | `Accept-Encoding: gzip` → `Content-Encoding: gzip` | `content-length: 62554` | ✅ |
| 2 | 两条路径都有 `Vary: Accept-Encoding` | 均有 | ✅ |
| 3 | 解压后 md5 == `dee593fd…` | `dee593fd95a5c778279023b6a0b2a378` | ✅ |
| 4 | 明文 body 与磁盘文件逐字节相同 | `cmp` 通过（191,661 B） | ✅ |
| 5 | 无 `Accept-Encoding` → 不压缩 | 无 `Content-Encoding`，191,661 B | ✅ |
| 6 | `gzip;q=0` → 不压缩 | 191,661 B | ✅ |
| 7 | `*` → gzip | 62,554 B | ✅ |
| 8 | `/api/logs/stream` 无 `Content-Encoding` | `None`（尽管客户端要 gzip） | ✅ |
| 9 | SSE 逐条到达 | 1.61 / 3.10 / 4.61s，spread 3.00s | ✅ |
| 10 | `/api/stats` 无 range 逐字节不变 | `{"total_requests":0,"models":{}}` | ✅ |
| 11 | `/api/stats?range=180d` 无 `unmapped_requests` | `has unmapped_requests: False`（2a 不含 2b，符合范围） | ✅ |

### 7.5 未测到 / 明确不覆盖

1. **未在浏览器里真跑面板**：本轮只做 HTTP 级 E2E（curl），没有开 Chromium 验 JS 执行。
   理由：2a 只改传输编码，不改 HTML 内容（已用 `cmp` 证明字节相同），且面板 JS 未变。若 Lead 需要，可另起一轮浏览器冒烟。
2. **未验生产镜像/容器**：不碰生产（边界要求）。
3. **未做 gzip 在中间层（CDN/反代）下的行为**：本部署无前置代理（已在前序任务确认）。
4. **未做 2b 端到端**：按 Lead 2026-10-20 08:34 指示跳过。
