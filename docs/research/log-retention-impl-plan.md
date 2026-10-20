# 调用日志按天分文件 + 90 天保留 + 字段精简 + 历史查询 —— 实现方案（task-56 · 阶段 1）

> **给执行者**：本文件是**方案**，不是实现。阶段 2 经 Lead 批准后按 §7 的任务逐条 TDD 实现。
> 实现前先读：`docs/research/log-system-audit.md`（现状审计，本方案的所有数字来源）。

**Goal:** 把 `call_log.jsonl` 单文件改造成「legacy 段 + 按天分文件」、90 天保留、新行字段精简，并让 `/api/call-log` 能查 90 天历史，**且四个窗口（today/7d/30d/180d）的统计结果逐字节不变**。

**Architecture:** 写入侧按「append 时刻的本地日期」滚动到 `call_log/YYYY-MM-DD.jsonl`；读取/回填侧把「legacy 文件 + 日文件（升序）」当作一个**有序段列表**，用 `(segIdx, segOff)` 游标替代单一 `parsedTo`，按段顺序吸收，段内提交时校验游标未被 append 推进（沿用现有 handoff 守卫）。字段精简只在**落盘副本**上做（`diskRec`），内存环与 API 响应不变。历史查询走文件路径（无索引），无参数时仍走内存环。

**Tech Stack:** Go 1.x（现有代码，无新依赖；gzip 用标准库 `compress/gzip`）、原生 `net/http`、单文件前端 `static/admin.html`。

**Spec:** `docs/research/log-system-audit.md`（task-52 审计：字段体积、读者矩阵、保留策略、前端查询现状）。

## 0. 结论摘要

- **布局**：`<data>/call_log.jsonl`（legacy，冻结只读）+ `<data>/call_log/YYYY-MM-DD.jsonl`（新日文件）；legacy 恒为第 0 段。
- **回填**：把「legacy + 日文件升序」当有序段列表，`parsedTo` 换成 `(segIdx, segOff)` 游标，逐段解析、逐段提交；段序 = 原文件字节序 ⇒ 统计结果不变。实测：61 个日文件与单文件扫描**桶表逐字段完全相等**（233,444 行 / 1,089 桶）。
- **冷启动成本**：现状 124 MB = 668 ms（HTTP 端到端）；90 天 ≈ 0.75~0.95 s；180 天 gz ≈ 1.4~1.7 s。文件数开销可忽略（61 次 open < 2 ms）。
- **字段精简**（只改落盘副本，内存/API 不变）：path/route_mode 默认值省略、events 里的 node_name 停写、final_status/dial_ms 停写 ⇒ 新行 **−16.5%**（2.18 → 1.82 MB/天）；读取侧兜底共 3 处（`admin.html:2300` 的 route_mode 默认值、`admin.html:2283-2287+2299` 的 node 名映射、其余零改动）。
- **历史查询**：`/api/call-log` 加 `limit/before/after/model/status/q`，无参数时仍走内存环（完全向后兼容）；**不需要索引**（单日 ≤32 ms、2000 命中 25 ms、尾部 2000 行 179 µs~1.95 ms）。
- **保留**：热明文 7 天 + gz 归档 + 90 天删除；新增部分封顶 ~25 MB（legacy 124.5 MB 常驻不变）。
- **唯一需要拍板**：90 天保留 vs 180d 窗口重启可重现（§8 D1）。

## Global Constraints

- 保留期 **90 天**（Lead 已定）；存量 `call_log.jsonl`（124.5 MB）**原样保留、不重写**。
- 字段**只增不删**：老行必须仍可读；新数据靠「新行不写 + 读取侧兜底」。
- `call_log.jsonl` 是统计回填**唯一来源**（小时桶 / `everServed` / 2b 口径）：任何改动必须能证明四个窗口统计不变，且**只追加、不重写历史**。
- 不改 `stats.json` 持久化 schema（现有 `TokenStatsData` 一个字节不动）。
- 阶段 1 只读：不改 `*.go`、不改配置；不 git、不重启生产容器、不用 8000 端口。测试/基准端口 9xxx，临时目录 `/tmp/t56/**`。
- 现有约定：写盘、桶 incr、游标推进必须在 `callLog.mu` 同一临界区（`calllog.go:1560-1690` 的注释是血泪教训）；全量解析绝不持 `callLog.mu`；`ensureBucketsUpToDate` 必须惰性调用（`calllog.go:586-598`）。

## Review Focus

实现者最容易在这几处写出「看起来对、四窗口却变了」的代码，逐条列在最前：

1. **时区**：日志混存 `+08:00` 与 `Z`（生产注释实测 185,722 / 490 行，`calllog.go:383-398`）。日文件名必须用 `time.Local` 归一（与 `hourKeyFor` 同口径），否则同一小时的记录被切到两个文件、文件字典序与记录时序不一致。
2. **游标与 append 的竞态**：`append` 在 seeded 时「写盘 + incr + 推进游标」一体；吸收在段内提交前必须复核 `(segIdx,segOff)` 未被推进，否则重复计数（`calllog.go:835-885` 记录了实测 +58%~78% 的重复计数事故）。
3. **跨午夜长请求**：`rec.TS` 是请求开始时间，可能在昨天；写入目标必须按 append 时刻的日期并保证**单调不回退**（只能写最后一段），否则新字节落进已吸收的旧段 → 双计。
4. **非最后段的半行**：只有最后一段允许「尾部无换行、留待下轮」；旧段末尾残留半行（崩溃）必须在吸收时消费掉并计入 `unabsorbedRecords`，否则守恒判据永久差 1 → 每次查询触发全量重扫。
5. **保留期与 180d 窗口的冲突**：冷启动只重放保留的文件。若按 90 天删，**90 天后的重启会让 180d 窗口少一半**。这是本方案唯一需要 Lead 拍板的点（§8 D1）。

---

## 1. 文件布局与迁移语义

### 1.1 目录与命名

```
<data-dir>/                          # = filepath.Dir(configPath)，生产为 /data
├── call_log.jsonl                   # legacy 段：冻结的存量（124.5 MB），只读，永不追加
├── call_log/                        # 新目录（首次 daily 写入时创建，0755）
│   ├── 2026-10-21.jsonl             # 热段：最近 N 天，明文，可追加（只有"今天"会被追加）
│   ├── 2026-10-20.jsonl
│   ├── 2026-10-19.jsonl.gz          # 冷段：>gzipAfterDays 的日文件，gzip 归档
│   └── ...
├── stats.json                       # 不动
└── config.json
```

- 文件名：`YYYY-MM-DD.jsonl`，日期 = `t.In(time.Local)` 的日期，`t` 来自 **append 时刻**（`time.Now()`），不是 `rec.TS`（见 Review Focus 3）。
- 排序：**legacy 恒为第 0 段**，其后日文件按文件名字典序升序（`YYYY-MM-DD` 字典序 == 日期序）；`.jsonl` 与 `.jsonl.gz` 视作同一天，**同日只允许存在一种**。
- 段类型：`legacy`（单文件、可跨任意天数）、`plain`（日文件）、`gzip`（归档日文件）。
- 段列表构建（`segments()`）：读目录，过滤 `^\d{4}-\d{2}-\d{2}\.jsonl(\.gz)?$`；未知文件**忽略并 WARN 一次**（不崩、不吸）；同日 plain+gz 并存 → 选 plain、WARN、不删（留给运维）。

> **为什么 legacy 不是「最旧一天」**：它的 233,444 行覆盖 2026-08-13~10-20 共 68 天，不能按天命名。语义定为「**冻结前缀段**」：排序上恒在最前，内容视为部署前的完整历史。新日文件从部署日（或次日）开始，因此「legacy 全部早于新日文件」这一保序前提成立。

### 1.2 模式开关（兼容与回滚的抓手）

`callLogStore` 增加两个字段（`calllog.go:1306-1315`）：

```go
type callLogStore struct {
    mu      sync.RWMutex
    records []CallRecord
    path    string   // legacy 文件路径（语义不变；dir=="" 时就是唯一文件）
    dir     string   // 日文件目录；"" ⇒ 单文件模式（现有测试全部走这条）
}
```

- `dir == ""` ⇒ **单文件模式**：行为与当前代码逐字节一致（写入 `path`、吸收 `path`、`clear` 删 `path`）。**现有 96 处测试引用（15 个文件）因此零改动**——这是刻意设计，不是偷懒。
- `dir != ""` ⇒ **daily 模式**：`path` 只作 legacy 段；新写入进 `dir`。
- 生产选择：`initCallLog(configPath)`（`calllog.go:1317`，`main.go:7637` 调用）设 `dir = filepath.Join(filepath.Dir(configPath), "call_log")`，并受启动参数控制：
  - `-calllog-layout`：`daily`（默认）/ `single`（回滚/排障用；此时只读 legacy，不读日文件，与旧二进制语义相同）
  - `-calllog-gzip-after-days`：默认 `2`（后天及更早压缩归档）
  - `-calllog-retain-days`：默认 `90`（Lead 已定；见 §5/§8 D1）
- daily 目录创建失败（只读卷/ENOSPC）⇒ **降级为单文件模式 + WARN**，不阻断服务：日志比新布局更重要。

### 1.3 写入路径（`calllog.go:1508-1690` 的 `append`）

改动点集中在三处，其余逻辑（events 裁剪、锁内落盘、失败截断回滚、incr、游标推进）**保持不变**：

1. 目标文件：
   ```go
   // append 内、Marshal 之前
   target := p // 单文件模式
   if s.dir != "" {
       target = s.currentDayPathLocked() // 见下：保证单调
   }
   ```
   `currentDayPathLocked()`：`day := time.Now().In(time.Local).Format("2006-01-02")`；若 `day` 早于**最后一段**的日期（时钟回拨/跨午夜长请求），返回最后一段路径（**只能追加到最后一段**）；否则 `dir/day+".jsonl"`（首次写入时 `os.MkdirAll`，失败返回原 `p` 并 WARN 一次）。
2. 落盘：`writeCallLogLine(target, line)`；失败时 `truncateCallLog(target, sizeBefore)` 回滚（同现状）。
3. 游标推进：`if bucketsSeeded { 段内校验 target == lastSegPath() → incr; cursor.off += len(line) }`；若 target 不是最后一段（不应发生，防御）→ 只写盘、不 incr、不推进，让下次回填吸收（等价于 seeded=false 的行为），并 ERROR 一次。

> 日切换不需要显式「滚动」动作：每次 append 现算日期路径即可（`O_APPEND|O_CREATE` 语义不变）。上一段是否完整由 §2.4 的非最后段半行规则兜底。

### 1.4 `/api/call-log` 的 DELETE 与统计清理

`clear()`（`calllog.go:1701-1730`）扩展：单文件模式删 `path`；daily 模式删 `path`（legacy）**和** `dir` 下所有日文件（**这是用户显式清空，不算"重写存量"**），然后 `resetAbsorbedStateLocked()`、`cursor=(0,0)`、`bucketsSeeded=true`、`statsGen++`、`servedModels=nil`（与现状同语义）。删除顺序与在途 append 的互斥仍由 `callLog.mu` 保证（现有注释已论证）。

---

## 2. 回填兼容（最关键）

### 2.1 现状机制（必须理解后再改）

| 机制 | 位置 | 语义 |
|---|---|---|
| `parsedTo int64` | `calllog.go:204-207` | 已吸收字节偏移，恒为最后一个完整换行之后；约束「已吸字节恰好构成文件前缀」 |
| `bucketsSeeded bool` | `calllog.go:212-237` | 文件里已无未吸字节；为 false 时 `append` 只写文件、不 incr、不推进 |
| `absorbedRecords` / `unabsorbedRecords` | `calllog.go:239-255` | 消费侧账 / 其中未进桶（坏行）的条数；守恒判据 `ΣN(hour)+ΣN(unmapped) == absorbed - unabsorbed`（`calllog.go:345-379`） |
| `absorbOnce` | `calllog.go:659-900` | 快照 `(path, from, gen, seeded)` → 锁外读 `[from,size)` 解析 → 锁内校验 `parsedTo==from` 再合并提交 |
| `ensureBucketsUpToDate` | `calllog.go:613-650` | `absorbMu` single-flight + 吸收 + 守恒检查 + 自愈重建（退避 30s，`calllog.go:588`） |
| `markBucketsSeededIfFullyAbsorbed` | `calllog.go:483-491` | 锁内 `Stat` 复核 `size==parsedTo` 才置位（防"Stat 与取锁之间又落了 append"） |
| `handleBackfillOpenError` | `calllog.go:519-560` | 只有在**文件确实不存在**时清桶并置 seeded；其它打开失败一个字节都不许动 |
| `everServed` 播种 | `callLogFromFile` `calllog.go:1411-1462` | 启动时从尾部 2000 条记录播种 `servedModels`；更久历史由首次回填补齐 |
| `stagedServed` + `pendingServed` | `calllog.go:780-830` | 老日志行（无 `unmapped` 键）判未映射必须等**整轮扫完**再用 `everServed ∪ staged` 裁决（与行序无关） |
| 2b 口径 | `calllog.go:800-860`、`statscan.go:156` | 新行 `SawUnmapped` 为真 → 采信落盘值；老行无键 → 公式 B + everServed 重算 |

### 2.2 新数据结构

```go
type callLogSegment struct {
    name string  // "call_log.jsonl"（legacy）或 "2026-10-20.jsonl"/"…jsonl.gz"
    path string  // 绝对路径
    gz   bool    // 需要解压
    legacy bool
    size int64   // 最近一次 Stat 的大小（快照用）
}

type absorbCursor struct {
    segIdx int   // 当前段下标，恒在 [0, len(segs)-1]
    segOff int64 // 段内偏移，恒为最后一个完整换行之后
}
// 「全部吸完」的判据：segIdx == len(segs)-1 && segOff == segs[last].size
// （等价于旧的 parsedTo == 文件大小，由 cursorAtEndLocked 在锁内 Stat 复核）
```

`callLogStore` 增加 `dir string`、`segs []callLogSegment`、`cur absorbCursor`、`segsVer uint64`（段列表版本，便于检测目录变化）；`parsedTo` **删除**（由 `cur` 取代），所有引用点逐一改造（`grep parsedTo` 共 20 处）。

### 2.3 升序回填算法（替代 `absorbOnce` 的单文件逻辑）

```
absorbOnce():
  snapshot: segsVer, cur, gen, seeded           # RLock
  segs := s.segs（副本）+ 每段 size 用 Stat 刷新
  if 段列表变化（目录新增/删除/压缩）: 锁内 rebuildSegsLocked() 后重新快照
  for i := cur.segIdx; i < len(segs); i++:
      from := (i == cur.segIdx ? cur.segOff : 0)
      data := readSegment(segs[i], from)       # gz: gzip.NewReader + io.ReadAll（单日 ≤ ~10 MB）
      # 注：gz 段必然已整段吸收过才会被压缩，故 gz 的 from 恒为 0；
      #     若真出现 from>0 的 gz 段（异常），readSegment 解压后按 from 切片。
      staged/… 同现有解析循环（parseStatsLine → hourKeyFor → unmapped 判定 → add）
      if i == len(segs)-1 && 尾部无 '\n':
          # 最后一段的半行留到下一轮（与现状一致）
          consumedEnd = 最后一个 '\n' 之后
      else:
          # 非最后段的尾部半行：消费掉并计入 unabsorbed（否则守恒永久差 1）
          consumedEnd = len(data); skipped++
      锁内提交:
          if statsGen != gen: 丢弃 return
          if (i, from) != 当前 cur: 丢弃 return        # handoff 守卫：append 推进过 → 重来
          merge staged（与现状同：staged→hourBuckets、stagedUnmapped→unmappedBuckets、stagedServed→servedModels）
          absorbedRecords += lines; unabsorbedRecords += skipped
          cur = (i, consumedEnd)
          if 已是最后一段且 consumedEnd == size: bucketsSeeded = true
```

要点：

- **顺序保证**：`segs` 由 `legacy` + 日文件名字典序构成；legacy 的历史全部早于部署日，日文件从部署日起 ⇒ 段序即时间序。段内是文件追加序。**两者合起来就是原单文件的字节序**（阶段 1 已实测等价，§2.5）。
- **每段独立提交**：长文件不会一次吃满内存；单段最大 ≈ 忙日 9.7 MB（现状冷启动是一次 `make([]byte, 124MB)`，这反而是**内存改善**）。
- **handoff 守卫按段做**：只有最后一段会被 append 推进，所以非最后段提交时 `(i, from)` 不可能被人动过；最后一段沿用现有「`cur` 变了就丢弃重试」的收敛论证（`calllog.go:835-885`）。
- **截短检测**：`readSegment` 前若 `seg.size < from`（文件被外部截断/替换）→ 走现状同款：锁内 `resetAbsorbedStateLocked()`、`bucketsSeeded=false`、`cur=(0,0)`，全量重吸。
- **打开失败**：`os.IsNotExist` 且**段列表为空** ⇒ `resetAbsorbedStateLocked()` + `bucketsSeeded=true`（等价现状 `handleBackfillOpenError`，`calllog.go:519-560`）；gzip 损坏（`gzip.NewReader` 失败）⇒ WARN + 把该段当作「已消费但 0 行」（`cur` 前进到段尾，`absorbed`/`unabsorbed` 都不加）——统计该段数据丢失但守恒成立、其余段照常；**绝不因一个坏归档让整次查询 500**。
- **段列表刷新**：吸完当前段、发现 `i+1 == len(segs)` 但目录里出现了新文件（日切）⇒ `segsVer++`、重建列表、继续；重建后 `cur` 指向新段 `(i,0)`（旧段已吸完）。
- `markBucketsSeededIfFullyAbsorbed` 改写为 `cursorAtEndLocked()`：`cur.segIdx == len(segs)-1 && cur.segOff == segs[last].size`（锁内 Stat 复核最后一段，逐条沿用现有注释的窗口论证）。
- `checkBucketConservation`（`calllog.go:374-379`）**不改**：它只依赖 `absorbedRecords/unabsorbedRecords` 与两张桶表，按段累加后语义不变。

### 2.4 冷启动要扫几个文件、多久

| 场景 | 文件数 | 冷启动吸收量 | 实测/估算 |
|---|---|---|---|
| 部署当天（第一个 daily 启动） | 1 legacy + 1 日文件 | 124.5 MB + 当天新数据 | **668 ms**（实测：`/tmp/t56/bin/oc2api-baseline` 冷查 `?range=180d`，含 590ms 解析；第 2 次 5.7 ms） |
| 运行 90 天后重启 | 1 legacy + 90 日文件（84 个 gz） | ≈ 138 MB（字段精简后） | **≈ 0.75~0.95 s**（按 124 MB/704 ms gzip 回放实测线性外推） |
| 同上，若保留 180 天（D1 选项 B） | 1 + 180 | ≈ 276 MB raw-equiv | **≈ 1.4~1.7 s** |

实测明细（`.research/t56/evidence/measurements.txt`，脚本 `.research/t56/z_t56_bench*_test.go`）：

- 61 个日文件全量解析：**485 ms（256 MB/s）**；同字节单文件：593 ms（210 MB/s）——**文件数几乎无开销**（61 次 open 合计 < 2 ms）。
- 61 个 gz 日文件 inflate+解析：**704 ms**（gz 10.8 MB，11.5×）。
- 结论：冷启动回填从当前 ~0.6 s 变成 ~0.8~1.0 s，**仍在启动预算内**（`ensureBucketsUpToDate` 是首个 stats 查询惰性触发；`main.go:7637` 的启动路径不变）。

### 2.5 四个窗口统计不变的证明（阶段 2 验收门槛）

**解析级等价性（阶段 1 已实测，PASS）**：把生产快照按 `time.Local` 切成 61 个日文件，分别「单文件扫描」与「日文件升序扫描」，断言桶表**逐键逐字段完全相等**：

```
files=61 lines=233444 bytes=124286987 buckets=1089
single-file scan: 592.7ms   daily-files scan: 485.2ms
断言：len(buckets) 相等，且对每个 hourModelKey，*a == *b（结构体整体比较）
```

**为什么这足以保证四窗口不变**：段序 = 原文件字节序（legacy 在前、日文件按日期升序、段内追加序），`absorbOnce` 是按字节序单向累加的；唯一的顺序敏感逻辑（老行未映射裁决）已经在 `pendingServed` 里被设计成「整轮扫完再判」（`calllog.go:790-830`），阶段 1 的等价性测试覆盖了全部字段（含 ok/fail、PT/CT/CC/CR、Speed*、TTFT*、Tps*）。

**端到端验收（阶段 2 必须写，T8）**：同一份字节，三种布局各起一个实例，逐个 API **字节比对**：

| 布局 | 内容 |
|---|---|
| L1（现状） | `call_log.jsonl` = 整份快照，无 `call_log/` |
| L2（迁移当天） | `call_log.jsonl` = 前 60 天，`call_log/2026-10-20.jsonl` = 最后一天 |
| L3（纯新装） | 无 legacy，`call_log/` 下 61 个日文件 |

比对端点（同一进程启动时刻、同一秒内取数）：`GET /api/stats?range=today|7d|30d|180d`（**4 个窗口逐字节**）、`GET /api/trends?range=…`、`/api/stats?group=model` 趋势、`unmapped_requests`、以及 `checkBucketConservation` 的 `(ΣN, absorbed, unabsorbed, ok)`。阶段 1 已在 fixture 上固化基线（运行日 2026-10-20）：today `total_requests=873`、7d `27,739`、30d `173,846`、180d `231,654`（JSON 存于 `.research/t56/evidence/baseline-*.json`；today/7d 依赖运行日，测试须同一天跑两实例比对，或只断言 L1==L2==L3）。

---

## 3. 新行字段精简 + 读取侧兜底

**原则**：只改**落盘副本** `diskRec`（`diskRec := rec` 在 `calllog.go:1550`，现有做法），内存环 `s.records` 与 `GET /api/call-log`（无参数路径）在**本次进程内**保持完整字段。老行不受影响。
> ⚠️ 「重启后从磁盘恢复的最近 2000 条」本来就从落盘行反序列化（`loadCallLogFromFile`），因此这些记录的 `events[].node_name` 会缺失——由 §3.1 第 3 行的前端映射兜底，与现有「干净记录 events 不落盘」的重启差异同类。

### 3.1 逐字段改动点

| # | 字段 | 写入点 | 落盘规则 | 读取侧（老行/新行都要能读） | 省下（审计实测） |
|---|---|---|---|---|---|
| 1 | `path` | `beginCallLog` `calllog.go:1802`（`Path: path`） | `diskRec.Path == "/v1/chat/completions"` ⇒ 置 `""`（`omitempty` 自然省略） | `admin.html:2300` 已 `r.path||'/v1/chat/completions'` **无需改**；关键字搜索 `admin.html:2246` 空串无影响；Go 侧无读者（`grep .Path` 仅测试构造） | 6.62 MB / 5.3% |
| 2 | `route_mode` | `calllog.go:1802` | `diskRec.RouteMode == "public"` ⇒ 置 `""` | `admin.html:2300` 现为 `r.route_mode||'-'` → **改为 `r.route_mode||'public'`**（一行，保留语义）；Go 侧无读者 | 4.90 MB / 3.9% |
| 3 | `events[].node_name` | `callRecorder.event` `calllog.go:1826-1836`（`NodeName: name`） | `diskRec.Events` 里把 `NodeName` 清空（保留 `Node` 指纹与 `Type/Detail/At`） | `admin.html:2283-2287` `clNodeLabel(ev)` 现用 `ev.node_name||ev.node` → **改为 `clNodeLabel(ev,r)`**：`ev.node_name || (r.node_names[r.nodes.indexOf(ev.node)] ?? '') || ev.node`；调用点 `admin.html:2299` 传 `r`。顶层 `node_names` **保留写入**，所以映射 100% 可还原 | 8.93 MB / 7.2% |
| 4 | `final_status` | `finish` `calllog.go:1876` | 停写：`diskRec.FinalStatus = ""` | **零读者**（`grep FinalStatus` 全仓库仅写入 1 处；`statscan.go` 忽略未知键；`admin.html` 0 处） | 75.8 KB / 0.06% |
| 5 | `dial_ms` | `calllog.go:1936-1937` | 停写：`diskRec.DialMS = 0` | **零读者**（同上） | 13.5 KB / 0.01% |
| — | *（保留）* `nodes` / `node_names` / `req_id` / `ts` / `events[].node` | — | 不动 | 统计/前端都读 | — |

合计新行体积 **−16.5%**（124.5 MB 存量口径 ≈ −20.5 MB；新数据 2.18 MB/天 → **1.82 MB/天**；14 天均值 1.35 → **1.13 MB/天**）。

### 3.2 老行可读性论证（逐字段）

- `path` 缺失：`admin.html:2300` 有默认值；`/api/stats` 不读（`statscan.go:11-16` 明示）；`CallRecord.Path` 为 `omitempty` 字符串，`json.Unmarshal` 缺键 = `""`，所有读者都按「空 = 默认」处理。
- `route_mode` 缺失：同上，前端改默认 `'public'` 后语义等价。
- `events[].node_name` 缺失：`admin.html` 回退到指纹（可接受），有 `r.node_names` 时映射回名字；`clNodeChain` 只读顶层 `nodes/node_names`（`admin.html:2279-2282`）不受影响。
- `final_status` / `dial_ms` 缺失：无读者；`CallRecord` 字段零值即可。
- 统计侧：`statscan.go:11-16` 声明 `events / nodes / node_names / path / req_id / route_mode / err_msg / stream 从不被统计访问`；`parseStatsLine` 对未知键忽略、缺失键留零值（`statscan.go:72-170`）——**新行省略这些字段对回填零影响**。

### 3.3 需要同步修改的现有测试

- `calllog_test.go:66` 断言落盘键包含 `"path"` ——改为「默认值必须缺席、非默认值必须存在」两条断言。
- `discev_test.go:162-185` 断言 issue 记录的 `events` 落盘 —— 需改为「落盘 events 保留 `node`/`type`/`detail`/`at`，但 `node_name` 可缺」。
- `calllog_test.go:30-80` 构造的 `CallRecord` 含 `NodeName`：只影响内存断言（内存不变），预计无需改。

---

## 4. `/api/call-log` 契约

### 4.1 参数

`GET /api/call-log`（`adminCallLogHandler` `calllog.go:2398-2413`）：

| 参数 | 类型/取值 | 默认 | 语义 |
|---|---|---|---|
| `limit` | int 1..2000 | 无参数路径 `500`；历史路径 `200` | 返回条数上限（硬上限 2000，超出截断） |
| `before` | RFC3339（如 `2026-10-19T12:00:00+08:00`） | 无 | **排他**上界：`ts < before`；也是分页游标 |
| `after` | RFC3339 | 无 | **包含**下界：`ts >= after` |
| `model` | 字符串（精确匹配） | 无 | 过滤 `model` |
| `status` | `ok` / `fail` / `all` | `all` | 过滤 `status`（`fail` 含非 ok 的一切） |
| `q` | 字符串 | 无 | 对**原始行**做子串匹配（大小写不敏感；覆盖 model/path/err_msg/req_id/nodes/node_names，与前端现有搜索语义一致） |

- **并存而非替换**：无 `before/after/model/status/q` 任一参数 ⇒ 走现有内存环路径（`latest(limit)`，最新在前，默认 500）——**前端现有行为逐字节不变**。有任一参数 ⇒ 走文件历史路径。
- 参数非法（`before/after` 解析失败、`status` 不认识、`limit` 非正）⇒ `400 {"error":"..."}`。
- 响应：**仍为 JSON 数组**（`[]CallRecord`，最新在前），不换包裹结构（保护第三方消费者）。附加响应头：
  - `X-Call-Log-Source: memory|file`
  - `X-Call-Log-Has-More: true|false`（是否还有更早匹配）
  - `X-Call-Log-Next-Before: <最旧一条的 ts>`（下一页游标；`Has-More: false` 时缺省）
  - `X-Call-Log-Truncated: true`（扫描触及保留期边界/扫描预算时出现）
- 分页去重边界：`ts` 有毫秒级重复可能，游标用 **`(ts, req_id)` 二元组**做严格序（`X-Call-Log-Next-Before` 只带 ts，前端同时传 `before_ts`+`before_req_id`；`before_req_id` 可选，缺省时用 `ts <= before` 会重复一条——文档里写清，前端传两个）。

### 4.2 实现骨架（无索引）

```
queryFromDisk(filter) []CallRecord:
  segs := 段列表（新→旧：日文件降序，legacy 最后）
  for each seg in segs:
      if filter.before 早于该段最早可能时间（用文件名日期快速判断）: continue/break
      // 段内倒读 256 KiB 块（复用 tailReadLastLines 的分块逻辑，见 calllog.go:1300-1345），
      // 逐行反向：parseStatsLine 取 (ts,model,status) 做快速谓词（零分配），
      // q 命中先做 bytes.Contains（不解析），命中后 json.Unmarshal 整行
      if 累计 == limit: break
      if 行 ts < filter.after: return  // 倒序扫到 after 以下，整次查询结束
```

- 复用的两个已有实现：反向分块读（`calllog.go:1300-1345` 的分块策略：256 KiB）、快解析谓词（`statscan.go:72` 的 `parseStatsLine`，比 `encoding/json` 快 5.9×）。
- **扫描预算**：`maxScanBytes = 256 MiB`/请求（防御性；90 天全量 ≈ 138 MB，正常不会触发）→ 触发时返回已有结果 + `X-Call-Log-Truncated: true`。
- 与 append 并发：倒读可能看到最后一行半写（`O_APPEND` 单次 write ≤ PIPE_BUF 不保证；实际上单行 < 4 KiB 且单写者，撕裂概率极低）→ `json.Unmarshal` 失败的行**跳过**（现有 `loadCallLogFromFile` 同款容错）。

### 4.3 是否需要索引：不需要（实测依据）

| 操作 | 数据量 | 实测 |
|---|---|---|
| 尾部取最后 2000 行（小日文件 369 KB） | 2,000 行 | **179 µs** |
| 尾部取最后 2000 行（忙日 9.7 MB） | 2,000 行 | **1.95 ms** |
| 单日全量扫描 + 过滤（369 KB / 9.7 MB / 9.2 MB） | 493~17,232 行 | **0.94 ms / 29.0 ms / 32.4 ms** |
| 「倒读 + limit=2000 命中即停 + 命中行全量解析」（忙日 9.7 MB） | 2,000 命中 | **25.3 ms** |

⇒ 单日历史查询 ≤ ~32 ms；gz 归档天需先解压（单日 gz ≈ 0.8~1.2 MB → 解压 ~3~8 ms，实测 61 文件 10.8 MB gz 全解压+解析 704 ms）；跨天按 `before/after` 直接选文件（文件名字典序），最坏 90 天全扫 ≈ 138 MB / 210 MB/s ≈ **0.7 s**（且有 256 MiB 预算与 `after` 提前退出）。**不建索引**；将来若真需要「90 天全文检索」，再考虑每日 `.idx`（约 60 B/行 → 90 天 ~15 MB）或 SQLite——本阶段 YAGNI。

### 4.4 前端最小接线（可选任务 T9，需 Lead 决定是否本轮做）

- `admin.html` 调用日志页：加「日期范围」两个 `<input type=date>` → 首页请求传 `after/before`；「加载更多」按钮 → 用 `X-Call-Log-Next-Before` 再请求；搜索框/「只看异常」在历史模式下改传 `q`/`status`（现有关键字前端过滤保留给内存模式）。
- 不改现有布局/样式；改 `clVisible`/`clRefreshView` 之外只加数据加载函数（`callLogData` 追加而非替换）。

---

## 5. 90 天保留 / 归档策略

### 5.1 规则

| 年龄 | 形态 | 动作 |
|---|---|---|
| 今天 | `call_log/YYYY-MM-DD.jsonl`（可追加） | 正常写入 |
| 1 ~ `gzip-after-days`（默认 2）天 | `.jsonl` 明文 | 保留（便于 grep 排障） |
| 更早 ~ `retain-days`（默认 90） | `.jsonl.gz` | **压缩归档**（后台，不阻塞请求） |
| 超过 `retain-days` | — | **删除** |

- 压缩时机：后台 ticker（如每小时）+ 日切后触发；条件：`bucketsSeeded == true`（说明所有段已吸收）且该段**不是最后一段**。流程：`os.Create(tmp)` → `gzip.Copy` → `Sync` → `os.Remove(plain)` → `os.Rename(tmp, gz)`（任何一步失败保留 plain，下次重试）。
- 崩溃恢复：plain+gz 并存 ⇒ 选 plain（数据更全），下次压缩覆盖 gz；gz 损坏 ⇒ WARN + 跳过（该段统计丢失，其余正常）。
- 删除时机：吸收完成（seeded）后执行；只删**非最后一段**且日期 < cutoff 的段。
- **对回填的影响**：删除只发生在「该段已被吸收进内存桶」之后 → 当前进程统计不受影响；**但下次冷启动只重放保留的段**。所以：
  - `retain-days >= 180` ⇒ 180d 窗口永远可重现；
  - `retain-days = 90` ⇒ 第 91 天起的重启会让 180d 窗口**少掉最老的那部分**（today/7d/30d 不受影响，因为都在 90 天内）。见 §8 D1。
- `DELETE /api/call-log` 语义不变（清空全部段 + 桶 + `everServed` + `tokenStats`），见 §1.4。
- **legacy 段永不参与压缩/删除**（Lead 已定「存量原样不动」）；它的 124.5 MB 是常驻成本，`retain-days` 只作用于新日文件。

### 5.2 磁盘估算

| 项 | 当前 | 上线 90 天后 | 上线 180 天后（D1-B） |
|---|---|---|---|
| legacy（冻结） | 124.5 MB | 124.5 MB | 124.5 MB |
| 热段（≤7 天明文） | — | 8~13 MB | 8~13 MB |
| gz 归档 | 0 | ~10~13 MB（83 天 × 0.12~0.16 MB） | ~21~26 MB（173 天） |
| **合计** | **124.5 MB，且 +1.8 MB/天无上限** | **~143~150 MB（封顶）** | **~154~163 MB（封顶）** |

> 现有「无上限」：68 天 124.5 MB ⇒ 一年约 600~650 MB。新布局把「新增部分」压到 ≤25 MB 且不再增长；legacy 的 124.5 MB 是用户要求原样保留的一次性成本。

---

## 6. 风险与回滚

### 6.1 风险表（按「会不会让四窗口变错」排序）

| # | 风险 | 触发条件 | 检测/兜底 |
|---|---|---|---|
| R1 | 时区/日期切错段，文件字典序 ≠ 记录时序 | 日文件名用 UTC 或 `rec.TS` | 统一 `time.Local`（与 `hourKeyFor` 同口径）；T3 用「混存 +08:00/Z」fixture 断言 |
| R2 | 跨午夜长请求写进旧段 → 新字节落在已吸收区 | 用 `rec.TS` 选文件 | 按 append 时刻选文件 + 单调规则（§1.3）；T2 测试把时钟注入到 23:59:59 场景 |
| R3 | append 与吸收双重计数 | 段内提交未复核游标 | 沿用现有 handoff 守卫；T3 加 `backfillAfterStat` 注入 + `-race` 并发压测（现有 `backfill_timing_test.go` 模式） |
| R4 | 非最后段尾部半行未消费 | 崩溃留下半行 | §2.3 非最后段消费规则；T3 测试构造「昨天文件缺末尾换行」 |
| R5 | 保留期删文件导致 180d 窗口缩水 | `retain-days < 180` 且重启 | §8 D1 决策；`-calllog-retain-days` 可调；启动 WARN 提示「保留期 < 180 天，180d 窗口在重启后不完整」 |
| R6 | gzip 压缩与吸收竞态 | 压缩了尚未吸收的段 | 只在 seeded 且非最后段压缩；compress+prune 持独立 `archiveMu` 并复核 seeded；T7 测试 |
| R7 | `DELETE` 与在途 append | 现有已论证锁内安全 | 保持 `clear()` 同一临界区删除；T7 复用 `clear_log_test.go` 模式 |
| R8 | 现有 96 处测试引用单文件路径 | 重构 `path` 语义 | `dir==""` ⇒ 单文件模式，测试零改动（§1.2）；新增 daily-mode 测试覆盖 |
| R9 | 目录只读/ENOSPC 导致日志全丢 | 挂载异常 | 写失败回滚 + 降级单文件 + WARN；绝不 panic |

### 6.2 回滚到单文件（值班手册，供阶段 2 写进 docs）

**触发条件**：上线后任一四窗口数字不等、或 daily 目录导致服务异常。

```bash
# 1) 停服务（由运维执行；本任务/阶段1 不碰容器）
# 2) 合并：legacy 已在位，按「日期升序」把日文件追加到 call_log.jsonl
cd /data
for f in $(ls call_log/*.jsonl 2>/dev/null | sort); do cat "$f" >> call_log.jsonl; done
for f in $(ls call_log/*.jsonl.gz 2>/dev/null | sort); do zcat "$f" >> call_log.jsonl; done
# 注意：gz 是更早的天，必须先解压追加再追加明文；若两者跨天，先 gz 后明文
mv call_log call_log.rollback-$(date +%Y%m%d%H%M%S)
# 3) 用旧二进制（或新二进制 -calllog-layout=single）启动；启动后核对
#    /api/stats?range=today|7d|30d|180d 与回滚前快照一致
```

- 合并是**纯追加**，不改历史字节顺序（legacy 在前，日文件按日期升序 = 原时序）；合并后单文件与原单文件**语义等价**（阶段 1 已证明分割/合并等价）。
- 风险：合并期间若仍在写入会交错 → 必须在停机窗口做。
- 新代码保留 `-calllog-layout=single` 一个发布周期（只读 legacy、写 legacy），配合合并脚本即可在**不换二进制**的情况下回退。

---

## 7. 阶段 2 任务拆解（TDD，等 Lead 批准后执行）

> 每个任务独立可交付、独立可回滚；提交粒度建议按任务（本仓库规则：阶段 1 不 git，阶段 2 是否提交按 Lead 指示）。测试文件放 `calllog_split_test.go` / `calllog_history_test.go` / `calllog_retention_test.go`（新增），现有测试尽量不动。

- **T1 段列表 + 游标抽象（无行为变更）**
  - Files: `calllog.go`（新增 `callLogSegment`/`absorbCursor`/`segments()`/`rebuildSegsLocked()`）、`calllog_split_test.go`
  - 测试：段排序（legacy 在前、日文件升序、同日 plain>gz、未知文件忽略）、单文件模式下 `dir==""` 段列表 = [legacy]。
  - 验证：`go test ./ -run 'TestSegments|TestCursor' -race`；全量旧测试仍绿。
- **T2 daily 写入路径**
  - Files: `calllog.go`（`initCallLog`/`append`/`currentDayPathLocked`）、`calllog_split_test.go`
  - 测试：日切写新文件；跨午夜 `rec.TS` 旧日期仍写最后一段；写失败回滚截断到 sizeBefore（注入短写）；目录创建失败降级单文件；`-calllog-layout=single` 写 legacy。
  - 验证：`go test ./ -run TestAppendDaily -race`。
- **T3 多段升序回填**
  - Files: `calllog.go`（`absorbOnce`/`markBucketsSeededIfFullyAbsorbed`→`cursorAtEndLocked`/`handleBackfillOpenError`）、`calllog_split_test.go`
  - 测试：L1/L2/L3 布局桶表逐字段相等（§2.5 扩展为单元级）；乱序/同 ms 记录；非最后段半行；截短重建；gz 段吸收；`backfillAfterStat` 注入的 handoff 重试。
  - 验证：`go test ./ -run TestAbsorbMultiSegment -race`；`go test ./` 全绿。
- **T4 内存环尾部加载 + everServed 播种**
  - Files: `calllog.go`（`loadCallLogFromFile`）
  - 测试：最后一天不足 2000 条时跨段向前取；`servedModels` 播种与单文件一致；空目录/legacy 缺失不 panic。
  - 验证：`go test ./ -run TestLoadCallLogSegments -race`。
- **T5 字段精简 + 前端兜底**
  - Files: `calllog.go`（`append` 的 `diskRec` 处理）、`static/admin.html`（`clNodeLabel` 签名 + 调用点 + `route_mode` 默认值）、`calllog_split_test.go`
  - 测试：默认 path/route_mode 不落盘、非默认落盘；events 落盘保留 `node` 丢 `node_name`；`final_status`/`dial_ms` 不落盘；内存环与 API（无参数）保持完整；老行（含全部旧字段）仍能解析展示。
  - 验证：`go test ./ -run TestDiskTrim -race` + 手工 `curl /api/call-log` 抽查（9xxx 端口）。
- **T6 历史查询契约**
  - Files: `calllog.go`（`adminCallLogHandler` + `queryCallLogFromDisk`）、`calllog_history_test.go`
  - 测试：limit 默认/上限/非法；before/after 边界（排他/包含）；model/status/q 过滤；分页无重复无遗漏（(ts,req_id) 游标）；`X-*` 响应头；无参数时与旧行为字节一致；扫描预算截断标志；保留期边界返回空 + Truncated。
  - 验证：`go test ./ -run TestCallLogHistory -race` + `curl` 实测。
- **T7 保留/压缩/删除**
  - Files: `calllog.go`（`archiveOnce`/`pruneOnce`/`startCallLogArchiver`）、`calllog_retention_test.go`
  - 测试：只压缩非最后段且已吸收；plain+gz 并存选 plain；gz 损坏跳过；删除只删 >retain-days；`retain-days` 边界；`DELETE` 清空全部段；压缩后冷启动仍能回填。
  - 验证：`go test ./ -run TestRetention -race`。
- **T8 四窗口不变验收（门禁）**
  - Files: `calllog_invariance_test.go`（或 `tools/t56_invariance_test.go`）+ `docs/research/log-retention-impl.md`（阶段 2 报告）
  - 测试：L1/L2/L3 三布局实例化 → `today/7d/30d/180d` JSON **逐字节**相等；`/api/trends`、`unmapped_requests` 相等；守恒判据成立；`-race` 全量测试；生产快照 fixture 冷启动耗时记录。
  - 验证：`go test ./ -race` 全绿 + 报告附命令输出。
- **T9（可选）前端历史查询接线**
  - Files: `static/admin.html`
  - 测试：手工/截图（本仓库无前端测试框架）；`/api/call-log?after=…` 返回渲染；加载更多游标；内存模式不受影响。

---

## 8. 待 Lead 拍板

- **D1 保留期与 180d 窗口的冲突**：按「90 天」删文件后，第 91 天起的冷启动只能重放 90 天数据 ⇒ 180d 窗口会缩水（today/7d/30d 不受影响）。
  - 选项 A（严格 90 天，按你已定）：`-calllog-retain-days=90`，接受 180d 在重启后不完整；磁盘 ~145 MB 封顶。
  - 选项 B（推荐，`retain-days=180`）：只多 ~12 MB gz 归档，180d 窗口永远可重现；「90 天」改为「90 天内可在线查询 + 90~180 天仅归档」。请确认是否接受这一处偏离。
  - 选项 C（后续再说）：把小时桶持久化（`hourBuckets` 快照），与文件保留解耦——新增持久化 schema，风险更高，本阶段不做。
- **D2 `events[].node_name` 停写后**：UI 时间线节点名靠「顶层 `nodes`/`node_names` 映射」还原；若节点已从节点池消失也无影响（map 在记录里）。确认这个兜底可接受（否则保留 `node_name`，少省 8.9 MB 存量的 7.2%）。
- **D3 前端历史查询（T9）是否本轮做**：后端契约先上、前端另派，还是同任务一起交付？用户目标「前端能查到历史」最终必须做，但涉及 `static/admin.html`（与 U3 等并发改动冲突风险），建议单独排期。

## 9. 复算命令（阶段 1 实测）

```bash
# 输入：生产快照（只读）
sha256sum -c .research/t52/evidence/snap.sha256   # 25d6d42e…  call_log.jsonl

# 分文件等价性与性能（临时副本 /tmp/t56/repo，脚本见 .research/t56/z_t56_bench*_test.go）
cd /tmp/t56/repo && export PATH=$PATH:/usr/local/go/bin \
  GOCACHE=/home/jingyx/project/opencode2api-test/.gocache \
  GOMODCACHE=/home/jingyx/project/opencode2api-test/.modcache GOTMPDIR=/tmp/t56/gotmp
go test -run 'TestT56SplitEquivalence' -v ./          # 桶逐字段相等 + 61 文件耗时
go test -run 'TestT56TailRead|TestT56DayFilterCost' -v ./
go test -run 'TestT56TailBusyDay|TestT56GzipReplay' -v ./
go test -run 'TestT56HistoryQueryCost' -v ./

# 冷启动端到端（fixture = /tmp/t41b/fixture/orig，124 MB 单文件）
/tmp/t56/bin/oc2api-baseline -port 9537 -config /tmp/t41b/fixture/orig/config.json -password "" \
  -log-file /tmp/t56/cold3.log -log-stdout=false &
time curl -s -o /dev/null 'http://127.0.0.1:9537/api/stats?range=180d'   # 668 ms
curl -s 'http://127.0.0.1:9537/api/stats?range=today' | head -c 200
```

证据目录：`.research/t56/evidence/`（`measurements.txt`、`baseline-{today,7d,30d,180d}.json`）。
