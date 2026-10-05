package main

// 调用日志（Call Log）：一次上游请求一条结构化记录（req_id 贯穿全路径），
// 内存环形缓冲 + JSONL 落盘（与 config.json 同目录，重启可恢复），
// 供管理面板“调用日志”视图（列表/时段分析/节点分析，参考 opencode2api_enhance）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type CallEvent struct {
	Type     string `json:"type"`
	Node     string `json:"node,omitempty"`      // 节点指纹（精确定位）
	NodeName string `json:"node_name,omitempty"` // 节点显示名（快速定位）
	Detail   string `json:"detail,omitempty"`
	At       string `json:"at,omitempty"`
}

type CallRecord struct {
	ReqID            string      `json:"req_id"`
	TS               string      `json:"ts"`
	Path             string      `json:"path,omitempty"`
	Model            string      `json:"model,omitempty"`
	Stream           bool        `json:"stream,omitempty"`
	RouteMode        string      `json:"route_mode,omitempty"`
	Nodes            []string    `json:"nodes,omitempty"`      // 节点指纹链（按首次使用顺序）
	NodeNames        []string    `json:"node_names,omitempty"` // 与 Nodes 对应的显示名链
	Events           []CallEvent `json:"events,omitempty"`
	Status           string      `json:"status,omitempty"`
	PromptTokens     int64       `json:"prompt_tokens,omitempty"`
	CompletionTokens int64       `json:"completion_tokens,omitempty"`
	CacheCreation    int64       `json:"cache_creation_tokens,omitempty"`
	CacheRead        int64       `json:"cache_read_tokens,omitempty"`
	DurationMS       int64       `json:"duration_ms,omitempty"`
	TTFTMs           int64       `json:"ttft_ms,omitempty"`      // 首 token 延迟（仅流式）
	OutputSpeed      float64     `json:"output_speed,omitempty"` // 输出速度 tokens/s（仅流式）
	ErrMsg           string      `json:"err_msg,omitempty"`
}

// HasIssue 是否有切换/异常事件（前端“只看失败/切换”过滤）。
func (r CallRecord) HasIssue() bool {
	if r.Status != "ok" {
		return true
	}
	for _, e := range r.Events {
		switch e.Type {
		case "switch", "ttft_timeout", "silence_timeout", "stream_interrupt",
			"stream_error", "connect_error", "upstream_error", "all_failed":
			return true
		}
	}
	return false
}

// IssueLabel 前端异常徽章文案。
func (r CallRecord) IssueLabel() string {
	for _, ev := range r.Events {
		switch ev.Type {
		case "all_failed":
			return "全部节点失败"
		case "switch":
			return "已切换节点"
		case "ttft_timeout":
			return "首字超时"
		case "silence_timeout":
			return "静默超时"
		case "stream_interrupt":
			return "流中断"
		case "stream_error":
			return "流错误"
		case "connect_error":
			return "连接失败"
		case "upstream_error":
			return "上游错误"
		}
	}
	return "异常"
}

const callLogCapacity = 2000
const callLogFileName = "call_log.jsonl"

// ---- 小时聚合桶 ----
//
// 取代「每次查询 os.ReadFile 全文件 + 逐行 Unmarshal」。实测真实数据只有
// 673 个非空 (小时,模型) 桶，稠密上界 180 天 × 24h × 17 模型 = 73,440 个 ≈ 6.7 MB。
//
// 桶存 sum + count 而不是平均值：跨桶合并时
//
//	存平均值   → 各桶平均的平均 ❌ 小桶被过度加权
//	存 sum+count → Σsum/Σn = 真实总体平均 ✅
//
// AvgTTFTMs / AvgOutputSpeed 与 recordModelSpeed（main.go）的增量均值
// `avg += (v-avg)/n` 数学上等价但浮点不逐位相等（实测相对差 1.3e-16 ~ 1.7e-15）。
// UI 显示精度 0.1 t/s，差异不可见。**不可修**：强行逐位一致需按桶顺序重放增量平均，
// 复杂度回到 O(记录数)，与 O(1) 的目标冲突。

// hourModelAgg 单个 (小时, 模型) 的聚合值。
type hourModelAgg struct {
	N, OK, Fail    int64
	PT, CT, CC, CR int64 // prompt / completion / cache_creation / cache_read
	SpeedSum       float64
	SpeedN         int64
	TTFTSum        float64
	TTFTN          int64
}

// hourModelKey 的 Hour 是「本地整点」的 Unix 秒。
// 用 Unix 秒而非日期字符串：可直接数值排序，且 time.Unix 后 .Local() 能还原。
type hourModelKey struct {
	Hour  int64
	Model string
}

var (
	// hourBuckets 由 callLog.mu 保护（RWMutex）。查询侧读、写入侧 incr。
	hourBuckets map[hourModelKey]*hourModelAgg
	// parsedTo 已吸收到的字节偏移，恒为「最后一个完整换行之后」的偏移。
	// **不可用 os.Stat 的 size**：短写或崩溃残留的半行会被当成已解析，
	// 下一条真实记录与它拼成一行 → Unmarshal 失败 → 该记录永久丢失且不报错。
	parsedTo int64
	// statsGen 单调递增，clear() 时 +1。backfill 记录进入时的 gen，
	// 合并前比对，不一致则丢弃结果。**这是检测文件被重建的唯一可靠手段**：
	// 实测 ext4 上连续 unlink+O_CREAT 200 次只产生 1 个唯一 inode，inode 完全复用。
	statsGen uint64
	// bucketsSeeded 表示「文件里已无未吸字节」，即 backfill 没有待办。
	//
	// 它只管一件事：**开机时那批存量历史**。parsedTo 的含义是
	// 「已吸进桶的字节数」，backfill 从 [parsedTo, size) 续扫 —— 可这只有
	// 在「已吸字节恰好构成文件前缀」时才对。进程刚起时桶是空的、parsedTo=0，
	// 历史字节全都躺在待吸区间里；此时若 append 一边 incr、一边把 parsedTo
	// 推到 A（= 本轮已写字节），待吸区间就变成 [A, size) —— 它不再从文件头起，
	// **必然错位**：A < S 时从旧文件中间起逐行切在半截处（那条静默丢弃），
	// A > S 时落在本轮记录内部，而那 A 字节已被 incr 过一遍，backfill 会再计一次。
	// 开机到面板首次查询之间的流量必然落在这个窗口里，故这不是理论边界情况。
	//
	// 所以两条路径必须二选一、不能都做：backfill 待办期间 append 只写文件、**不** incr；
	// 等 backfill 一次性把 [0, size) 全吸完并置位后，append 才切到 incr + 推进 parsedTo。
	// 二者由同一把 callLog.mu 串行化，不存在交错窗口。
	//
	// 本标志**不再**承担「在途写 vs 丢失写」的判别（曾经被塞过这个语义，是错的）：
	// 落盘已移进 callLog.mu，写失败会截回文件、不 incr、不推进 parsedTo，
	// 那两种情形现在根本不会发生。「已吸字节构成文件前缀」由 append 单独保证，
	// 不依赖本标志。见 append 的落盘段。
	//
	// clear() 之后置 true 是可证的：append 的落盘同样在 s.mu 内，
	// 故临界区内不可能有在途写入，函数返回时桶空、parsedTo=0、文件不存在。
	//
	// ⚠️ Task 7 实现 backfill 时**必须复用本标志**，不要另起一个：
	// 重复声明会编译失败，语义分叉则会重演上面的错位。
	bucketsSeeded bool

	// absorbedRecords 是「已吸收的记录条数」，**只在消费字节的地方**自增，
	// 与 parsedTo 同一处语句、同一临界区。它是**消费侧**的账
	//（「我们从文件里读掉了多少行」）；hourBuckets 是**计数侧**的账
	// （「桶里说有多少条」）。两条账在不同的语句里推进，
	// 「Σ N == absorbedRecords - unabsorbedRecords」才不是恒等式。
	//
	// 🔴 **绝不能把自增挪到 add() 旁边**：那样就退化成
	// 「桶加了什么、计数器就加了什么」，恒等式恒成立，什么都检不出。
	// 锚点必须是「字节被消费」——那也正是 parsedTo 的锚点。
	absorbedRecords int64
	// unabsorbedRecords 是 absorbedRecords 里**消费了但没有 add 进桶**的条数：
	// 结构性截断的坏行、以及 ts 解析不出整点的行。
	//
	// 它存在的原因是判据必须容许「合法的不等」：backfill 对坏行的既定行为是
	// 「推进 parsedTo、跳过 add」（见解析段的注释），那条行从此不再被重扫。
	// 若把它算作损坏，一个含坏行的文件会让**每次查询**都触发一次全量重扫。
	unabsorbedRecords int64
	// bucketRebuilds 累计自愈重建次数（诊断用：暴露「桶到底坏没坏过」）。
	// bucketRebuildNotBefore 是下一次允许重建的最早时刻（退避窗口的末端）。
	//
	// 两者写于 rebuildBucketsIfDue 的写锁内，而**唯一的读点也在
	// ensureBucketsUpToDate 内部**（那里持着 absorbMu），故实际受
	// absorbMu + callLog.mu 双重保护；任何新的读点都必须同样待在 absorbMu 内，
	// 否则 -race 会报。
	bucketRebuilds         uint64
	bucketRebuildNotBefore time.Time
)

// bucketNSumLocked 返回 Σ N over hourBuckets。
// 调用方必须持有 callLog.mu（读或写锁皆可）。
//
// 🔴 **遍历 map 必须在锁内**：RWMutex 挡不住并发 map 写，
// 无锁遍历时落进一个 append 就是
// fatal error: concurrent map iteration and map write —— recover() 无效，直接杀进程。
func bucketNSumLocked() int64 {
	var n int64
	for _, a := range hourBuckets {
		n += a.N
	}
	return n
}

// checkBucketConservation 是守恒检查本体：返回 (Σ N, absorbed, unabsorbed, 是否守恒)。
//
// 它在**自己的读锁内**完成遍历与取数，不要求调用方持锁 —— 调用方（查询路径）
// 此刻并没有锁，硬塞进去反而要求把整条查询链包进锁里。
//
// 判据 `Σ N == absorbedRecords - unabsorbedRecords`：
//   - absorbedRecords 数「从文件里消费掉的完整行」，是**消费侧**的账；
//   - Σ N 是「桶里说的条数」，是**计数侧**的账；
//   - unabsorbedRecords 数其中「消费了但没进桶」的（坏行 / ts 解析失败），
//     它把**合法的不等**从判据里扣掉。
//
// unabsorbed <= absorbed 由构造保证（每个 skip 必有一个 consume），
// 显式判一次是为了让「计数器自身被写坏」也走重建，而不是算出个巨大差值。
//
// 实测成本（673 个桶，-benchtime 2s）：8580 ns/op（8.6 µs，含读锁），
// 而一次 /api/stats 查询本身 ~170 µs —— 占 5%，故**每次查询都做，不节流**。
func checkBucketConservation() (sum, absorbed, unabsorbed int64, ok bool) {
	callLog.mu.RLock()
	defer callLog.mu.RUnlock()
	sum = bucketNSumLocked()
	return sum, absorbedRecords, unabsorbedRecords,
		unabsorbedRecords <= absorbedRecords && sum == absorbedRecords-unabsorbedRecords
}

// hourKeyFor 由记录时间戳算出桶键。解析失败返回 false。
func hourKeyFor(ts string, model string) (hourModelKey, bool) {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return hourModelKey{}, false
	}
	// 必须 .In(time.Local)：生产日志混存 185,722 行 +08:00 与 490 行 Z，
	// 不归一则那 490 条会进错 8 小时的桶。
	lt := t.In(time.Local)
	start := time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), 0, 0, 0, time.Local)
	return hourModelKey{Hour: start.Unix(), Model: model}, true
}

// add 把一条统计记录累加进桶。
// 过滤条件必须与 calllog.go 的 forEachBucketedCallRecord 回调（modelStatsFromCallLog）
// 及 main.go 的 recordModelSpeed 完全一致：
// model 为空、或 OutputSpeed <= 0 时，StreamReqCount / AvgTTFTMs / AvgOutputSpeed 三者都不计。
func (a *hourModelAgg) add(r *slimRec) {
	a.N++
	if r.Status == "ok" {
		a.OK++
	} else {
		a.Fail++
	}
	a.PT += r.PromptTokens
	a.CT += r.CompletionTokens
	a.CC += r.CacheCreation
	a.CR += r.CacheRead
	if r.Model != "" && r.OutputSpeed > 0 {
		a.SpeedN++
		a.SpeedSum += r.OutputSpeed
		a.TTFTN++
		a.TTFTSum += float64(r.TTFTMs)
	}
}

// ---- backfill：把日志文件里未吸的字节吸收进桶 ----

// absorbMu 给 backfill 做 single-flight。
//
// RWMutex 挡不住并发 map 写：两个请求同时进入 ensureBucketsUpToDate，
// 各自在锁外建 staged、再在锁内往同一张 map 写，会触发
// fatal error: concurrent map writes —— 该错误 recover() 无效，直接杀进程。
//
// 🔴 第二个调用者必须**等待**，而不是「发现有人在干就跳过」：
// 跳过会让它读到尚未建立的空桶，把零值当真实统计返回给面板。
var absorbMu sync.Mutex

// backfillAfterStat 是测试注入点，生产为空实现（可内联掉，零开销）。
// 存在的唯一理由：「必须按快照 size 提交 parsedTo」这条约束，
// 其反例只发生在「Stat 之后、提交之前」这个窗口里，而那个窗口窄到
// 只能靠概率撞上 —— 没有这个钩子就没有能稳定复现它的测试，
// 改错了也不会有人发现。
var backfillAfterStat = func() {}

// markBucketsSeededIfFullyAbsorbed 在**持有 callLog.mu 写锁**时置 bucketsSeeded。
//
// 为什么必须锁内重新 Stat，不能复用调用方在锁外拿到的那个 size：
// append 的「写盘 + 推进 parsedTo」整体在同一临界区内，而「锁外 Stat」
// 与「本函数取锁」之间存在窗口 ——
//
//	· Stat 之后、append 落盘之前取到旧值，本函数在 append 之后取锁：
//	  此刻 parsedTo 与文件大小同步前进，用旧值判等会**误判**成「已吸干净」
//	· 反过来，Stat 之后又来了新字节（落盘发生在我们取锁之前）：
//	  此刻文件大小已超过 parsedTo，说明还有字节没吸，必须不置位
//
// 只有锁内观测到的 fileSize==parsedTo 才能证明「文件里已无未吸字节」，
// 而这正是 bucketsSeeded 的定义。不满足时**什么都不做**（保持 false）。
func markBucketsSeededIfFullyAbsorbed(p string) {
	if p == "" {
		return
	}
	st, err := os.Stat(p)
	if err != nil || st.Size() != parsedTo {
		return
	}
	bucketsSeeded = true
}

// resetAbsorbedStateLocked 把「已吸收」的全局状态整体清零。
// 调用方必须持有 callLog.mu **写锁**；bucketsSeeded 由调用方自己定
// （clear/open 失败后置 true，截短与自愈重建后置 false —— 见各处注释）。
//
// 🔴 **凡是 parsedTo = 0 的地方都必须走它**，漏掉任何一处都会让下一次查询
// 必然判不一致 → 触发一次全量重扫（真实数据 ~350ms）。
// 用户每点一次「清空」就吃一发，且重建把 bucketsSeeded 打回 false 之后
// append 会退回「只写文件不 incr」，统计还要再晚一轮才对。
func resetAbsorbedStateLocked() {
	hourBuckets = nil
	parsedTo = 0
	absorbedRecords = 0
	unabsorbedRecords = 0
}

// handleBackfillOpenError 处理日志文件打不开的情况。
//
// 🔴 **必须判 os.IsNotExist，不能只判「Stat 是否成功」**。
// 接手时这里写的是 `if _, err := os.Stat(p); err == nil { return }` ——
// 与自述的「只在确实不存在时才清桶」并不等价：Stat 自己也会失败，
// 而它的失败原因与 open 失败的原因可以毫无关系（父目录 EACCES、
// 软链自环 ELOOP、fd 耗尽 EMFILE/ENFILE……）。
// 那种情形下桶会被整个清空：面板瞬间闪回 0，日志文件却一直好好地在那儿。
//
// 必须在锁内复核文件是否仍不存在：open 失败与取锁之间可能有一个在途 append
// 刚用 O_CREATE 重建了文件并 incr 了桶。此刻若照着过期的 IsNotExist 清空，
// 就会把它刚计入的那一条抹掉 —— 而那条记录已经在文件里，永远补不回来。
//
// ⚠️ Go 把 ENOTDIR 也归进 ErrNotExist（路径查找语义），所以「父级不是目录」
// 仍会被当成「文件不存在」而清桶。这与 os.Stat 的既有语义一致，
// 不在这里另立一套判定。
//
// 置 bucketsSeeded=true 的依据与 clear() 同构：函数返回时桶为空、parsedTo=0、
// 文件不存在，「已吸字节构成文件前缀」是可证的平凡事实。
func handleBackfillOpenError(p string, gen uint64) {
	callLog.mu.Lock()
	defer callLog.mu.Unlock()
	if statsGen != gen {
		return // 期间 clear 过，不动它的结论
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		return // 文件还在（或只是打不开，不是不存在）：桶一个字节都不许动
	}
	resetAbsorbedStateLocked()
	bucketsSeeded = true
}

// bucketRebuildBackoff 是两次自愈重建之间的最小间隔。
//
// 定 30s 的理由：重建 = 一次全量重扫，真实数据实测 ~350ms。
// 30s 把「持续损坏」的最坏开销压到 350ms/30s ≈ 1.2%；再短的话，
// 面板一次刷新并发拉 3 个 stats 接口就可能连续触发重建，
// 于是**自愈机制本身变成故障放大器** —— 桶一坏，服务就彻底不可用。
//
// ⚠️ **状态恢复之后不许把这个窗口清零**：清零会让
// 「重建 → 修好 → 立刻又坏」退化成每次查询一次全量重扫，正是上面要防的。
const bucketRebuildBackoff = 30 * time.Second

// rebuildBucketsIfDue 在**持有 callLog.mu 写锁**时复核不变量；
// 确实不一致、且退避窗口已过，才整体丢弃旧桶、把吸收状态归零。
// 返回是否真的重建了。调用方必须持有 absorbMu（故不存在并发的 backfill）。
//
// 为什么是**重建**而不是「回退到第二套查询实现」：
// 回退会让**检测能力在降级那一刻自我摧毁** —— 桶错了所以改用旧实现，
// 可旧实现无人验证、下一次对拍因为「桶不可信」而无法运行，
// 于是再没有任何东西能发现它也已经错了。重建没有这个问题：
// 下一次吸收走的是**已经被冷启动跑过无数次的同一条 backfill 路径**，
// 不需要新代码、不需要新语义，吸收完自动回到可检验状态。
//
// 🔴 重建是**整体丢弃**旧桶（resetAbsorbedStateLocked 把 map 置 nil），
// 绝不与新桶 +=：+= 会把已经错了的数字原样带进新桶，错误被永久固化
// 且此后每次重建都再叠一层。
//
// 🔴 写锁内**只做内存操作**，全量重扫在锁外由调用方重跑 absorbOnce 完成
// （持写锁重扫会让每个在途代理请求的收尾阻塞 350ms）。
func rebuildBucketsIfDue() bool {
	callLog.mu.Lock()
	defer callLog.mu.Unlock()
	// 复核：checkBucketConservation 的读锁与此刻之间可能落进一个 clear()，
	// 它把状态清成自洽的 0/0。此时重建纯属自伤 —— 白吃一次全量重扫，
	// 还把 bucketsSeeded 打回 false，让 append 退回「只写文件不 incr」。
	if n := bucketNSumLocked(); n != absorbedRecords-unabsorbedRecords {
		if time.Now().Before(bucketRebuildNotBefore) {
			return false
		}
		bucketRebuildNotBefore = time.Now().Add(bucketRebuildBackoff)
		resetAbsorbedStateLocked()
		bucketsSeeded = false
		bucketRebuilds++
		return true
	}
	return false
}

// ensureBucketsUpToDate 把 [parsedTo, EOF) 区间吸收进桶。无新增时立即返回。
// 幂等：重复调用不会重复累加。并发调用时第二个**等待**而非跳过。
//
// **必须惰性调用**：绝不能放进 initCallLog —— backfill 会把启动耗时
// 从 50ms 推到 1.9s，直接违反「启动 <50ms」验收。
// 调用方是 Task 8 的三个查询函数（进查询前调一次）。
//
// 🔴 **绝不能持有 callLog.mu 做全量解析**：那会让每个在途代理请求的收尾
// （callLog.append）阻塞整个解析时长 —— 比现在更糟：现在只是面板慢。
// 因此这里锁外读取与解析、锁内合并。合并段是纯内存 O(桶数) 操作。
//
// 本函数同时是**守恒检查的唯一入口**：每次查询顺手做，零启动成本。
//
// 为什么检查放在这里、且每次查询都做（不节流）：
//   - 覆盖面：它是每个 stats 查询的必经之路（statsRangeResponseFor 只调它一次，
//     trends / trendsByModel / modelStats 三条路都经过它），且**在吸收之后**，
//     于是写入侧 incr 路径也一并被检验 —— 这正是「启动时全量对拍」做不到的
//     （对拍只在 backfill 之后跑，而 backfill 期间 append 只写文件不碰桶，
//     对写入侧的覆盖率是 0）。
//   - 成本：实测 673 个桶 8.6 µs/次（-benchtime 2s），一次 /api/stats 查询
//     本身 ~170 µs，占 5%。为它加节流只会多引入一个「什么时候漏检」的分支。
//   - 检查在锁**外**：checkBucketConservation 自己取读锁遍历 map。
//     放在调用方的锁里既做不到（读锁不可重入），也没必要 —— 它遍历的是
//     已提交的稳定状态，而 absorbMu 已经把并发 backfill 排开了。
func ensureBucketsUpToDate() {
	// single-flight：进入即取锁，第二个调用者在这里排队等待。
	absorbMu.Lock()
	defer absorbMu.Unlock()

	// 守恒 → 不守恒就重建 → **本函数自己再吸收一轮**。
	// 重建后不立刻重吸的话，这次查询会拿全零桶去折叠，
	// 面板先闪一轮 0，得等下一次刷新才对 —— 自愈不该让用户看见中间态。
	// 循环至多两轮：第二轮仍不一致就只报不重建（退避窗口也是这么兜底的）。
	for rebuilt := false; ; rebuilt = true {
		absorbOnce()
		sum, absorbed, unabsorbed, ok := checkBucketConservation()
		if ok {
			return
		}
		if rebuilt || !rebuildBucketsIfDue() {
			// WARN 而不是 ERROR：桶坏了面板数字会错，但代理转发本身好好的。
			// 日志在**锁外**打 —— 持写锁做同步 I/O 会连带阻塞在途 append。
			slog.Warn("calllog: hour buckets inconsistent, stats may be wrong",
				"buckets_n", sum, "expected_n", absorbed-unabsorbed,
				"absorbed", absorbed, "unabsorbed", unabsorbed,
				"rebuilds", bucketRebuilds, "backoff", bucketRebuildBackoff,
				"path", callLogFilePath())
			return
		}
		// 重建了：回到循环头再吸收一轮（锁外全量重扫，不阻塞在途 append）
	}
}

// callLogFilePath 只读 path 供日志用。
func callLogFilePath() string {
	callLog.mu.RLock()
	defer callLog.mu.RUnlock()
	return callLog.path
}

// absorbOnce 是 ensureBucketsUpToDate 的吸收本体（不含 single-flight 与守恒检查）。
// 调用方**必须持有 absorbMu**。
func absorbOnce() {

	// ---- 锁外快照：路径、起点、generation ----
	callLog.mu.RLock()
	p := callLog.path
	from := parsedTo
	gen := statsGen
	seeded := bucketsSeeded
	callLog.mu.RUnlock()
	if p == "" {
		return
	}

	f, err := os.Open(p)
	if err != nil {
		handleBackfillOpenError(p, gen)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return
	}
	// 🔴🔴 size 是**快照**，parsedTo 必须按它提交，绝不能改用此刻的文件大小。
	// 见合并段那段注释（那里写了为什么改用当前大小会永久丢字节）。
	size := st.Size()
	if size < from {
		// 文件被截短（轮转/重建）：全量重吸。
		// bucketsSeeded 必须置回 **false**：此刻 [0, size) 是待吸区间，
		// 置位会让 append 从这个区间的**中间**起 incr 并推进 parsedTo，
		// 下一轮扫描便从记录内部开始切行 —— 静默丢记录且不报错。
		// statsGen 比对是为了不和 clear() 抢结论：期间 clear 过就整个退出。
		callLog.mu.Lock()
		if statsGen != gen {
			callLog.mu.Unlock()
			return
		}
		resetAbsorbedStateLocked()
		bucketsSeeded = false
		callLog.mu.Unlock()
		from = 0
	}
	if size == from {
		// 没有新字节。稳态（已 seeded）下 parsedTo 恒等于文件大小，
		// 于是查询路径每次都落到这里，且**一次锁都不用再取**。
		// 尚未 seeded 时（刚开机、或上一轮撞上解析期间有写入）才复核一次。
		if !seeded {
			callLog.mu.Lock()
			markBucketsSeededIfFullyAbsorbed(p)
			callLog.mu.Unlock()
		}
		return
	}

	// ---- 以下全部在锁外 ----
	// 只读 [from, size)：长度由**快照** size 决定，多出来的并发写入字节
	// 留给下一轮。短读（外部截断）时只认实际读到的字节。
	want := size - from
	buf := make([]byte, want)
	got, err := f.ReadAt(buf, from)
	if err != nil && err != io.EOF {
		return
	}
	buf = buf[:got]

	staged := make(map[hourModelKey]*hourModelAgg, 512)
	consumed := from
	// lines 记本轮真正被消费的**完整行数**，skipped 记其中「消费了却没进桶」的条数。
	// 两者都只在本轮成功提交时才并进全局（见下方提交段）。
	lines := int64(0)
	skipped := int64(0)
	for pos := 0; pos < len(buf); {
		rel := bytes.IndexByte(buf[pos:], '\n')
		if rel < 0 {
			break // 尾部没有换行 —— 撕裂的半行，留到下一轮
		}
		line := buf[pos : pos+rel]
		pos += rel + 1
		// 🔴 坏行**也必须**推进 consumed：不推进的话每次查询都从这行重扫，
		// 而下面的 += 会把它重复累加，且单调发散、永不自愈。
		consumed = from + int64(pos)
		lines++

		var r slimRec
		if !parseStatsLine(line, &r) {
			skipped++
			// 注意：parseStatsLine 比 encoding/json 宽容，**返回 true 不代表
			// 这行是好的** —— 尾随垃圾、数值写成字符串、+7 / 01 这类非法数字
			// 它都收（见 statscan.go）。所以 false 只代表「结构性截断」，
			// 不能拿来当坏行判据；宽容行按扫描器的既定行为计数，与查询侧同源。
			continue
		}
		// 必须走 hourKeyFor：它内部做 t.In(time.Local) 归一。生产日志混存
		// 185,722 行 +08:00 与 490 行 Z（本进程换过 TZ），自己解析 ts 会让
		// 那 490 条进错 8 小时的桶。
		k, ok := hourKeyFor(r.TS, r.Model)
		if !ok {
			skipped++
			continue
		}
		b := staged[k]
		if b == nil {
			b = &hourModelAgg{}
			staged[k] = b
		}
		b.add(&r)
	}

	backfillAfterStat() // 测试注入点，见上方注释

	// ---- 锁内合并 ----
	callLog.mu.Lock()
	if statsGen != gen {
		// 期间发生过 clear：本次结果作废（桶已被清空），丢弃，下轮从头来。
		callLog.mu.Unlock()
		return
	}
	// 🔴🔴 **交接校验：此刻 parsedTo 必须仍等于 from，否则整批丢弃。**
	//
	// 这条不是防御性编程，是修一个实测稳定复现的重复计数：
	//
	//	from = parsedTo 在 RLock 下快照，之后**锁外**才 f.Stat() 拿 size。
	//	中间落进一个 append，且此刻 bucketsSeeded == true →
	//	它「写盘 + incr 进桶 + 推进 parsedTo」三件事一起做，
	//	于是 size > from，backfill 认定 [from, size) 是待吸区间，
	//	把 append 刚计过的那几条**又吸一遍**。
	//
	// 实测（4 核、单写者 goroutine 压满、2000 轮）：
	//	落盘 4,685 条 → 桶内 7,435 条（+58%）；
	//	更大压力下 10,472 条 → 18,686 条（+78%）。
	//	parsedTo 精确等于文件大小，所以「吸干净了」的收尾断言照样全绿，
	//	面板上的请求数 / token / 速度均值却永久虚高、单调发散、永不自愈。
	//
	// 为什么判 parsedTo 就够：parsedTo 只会变大，唯一的增大者是
	// bucketsSeeded 为真时的 append，而那种 append 必然同时 incr 了桶 ——
	// 正好就是「我们要重复计入的那几条」。而 bucketsSeeded 为假时
	// append 只写文件、不碰 parsedTo、不碰桶（本函数整个解析段都是安全的），
	// clear() 推进的 gen 已被上一行拦掉。
	//
	// 丢弃是安全且廉价的：这批字节没被吸、parsedTo 也没推进，
	// 下一次调用会从同一个 from 重来。收敛性有保证 ——
	// seeded 为真时 size > from **只可能**是窗口自身造成的，
	// 待吸增量就是那一条 append 的长度（约 180 字节），
	// 窗口又是微秒级，所以下次调用几乎必然走 size == from 的提前返回。
	// 真正的大批量补吸发生在 seeded == false 期间，
	// 那期间 parsedTo 根本不会动，100% 一次成功。
	if parsedTo != from {
		callLog.mu.Unlock()
		return
	}
	if hourBuckets == nil {
		hourBuckets = make(map[hourModelKey]*hourModelAgg, len(staged))
	}
	for k, v := range staged {
		b := hourBuckets[k]
		if b == nil {
			hourBuckets[k] = v
			continue
		}
		// 🔴 必须是 += ：生产日志 38.6% 乱序，新记录会落进**已存在**的小时桶
		// （实测 610 条落进已结束的桶，最大回退 46 分钟）。
		// 「不存在则新建并赋值」在新建分支正确、在命中分支会把旧值整个抹掉。
		b.N += v.N
		b.OK += v.OK
		b.Fail += v.Fail
		b.PT += v.PT
		b.CT += v.CT
		b.CC += v.CC
		b.CR += v.CR
		b.SpeedSum += v.SpeedSum
		b.SpeedN += v.SpeedN
		b.TTFTSum += v.TTFTSum
		b.TTFTN += v.TTFTN
	}
	// 🔴🔴 **按快照 size 提交**，而不是「此刻的文件大小」。
	//
	// 反例（审查实测丢 1 条）：
	//   Stat 得到 S → 锁外解析 [from, S) → 期间 append 又写了新字节（落在 S 之后）
	//   → 若此刻重新 Stat 并把 parsedTo 提交成新的 size，那段字节
	//     **既没被解析、又扫不到**（parsedTo 已越过它），永久丢失且不报错。
	//
	// consumed 的上界就是 from+len(buf) == S（只会因尾部半行而更小），
	// 天然满足这条要求。注意那一小段撕裂半行正是靠「不越过换行」留给下轮的。
	parsedTo = consumed
	// 🔴 absorbedRecords / unabsorbedRecords **必须与 parsedTo 同一处提交**，
	// 且记的是**行数**不是字节数（判据的一端是 Σ N —— 条数）。
	// 放在解析段里逐行加是不行的：上面两个丢弃分支（gen 变了、parsedTo 被抢）
	// 都在解析之后才判定，那时计数已经加过了、桶却没并进来 ——
	// 留下「计数比桶多」的永久不等，于是**每次**查询都误判不一致。
	// 反过来把提交放在锁外也一样：并发读锁持有者会看到
	// 「计数已进、桶还没进」的瞬时不一致。
	absorbedRecords += lines
	unabsorbedRecords += skipped
	markBucketsSeededIfFullyAbsorbed(p)
	callLog.mu.Unlock()
}

// ---- 查询侧：把小时桶折叠成窗口内的统计 ----
//
// 🔴 三个查询函数（trendsFromCallLog / trendsByModelFromCallLog /
// modelStatsFromCallLog）从这里开始，全部改读内存桶，**不再 os.ReadFile 日志文件**。
// 唯一还在读文件的是 ensureBucketsUpToDate，且它只读「上次之后新增的那一段」。

// callLogFileExists 报告调用日志文件是否存在。
// 用途：trendsFromCallLog 需要区分「文件不存在」与「文件为空」——
// 前者必须返回 nil（handler 转成 []），否则冷进程上热力图会拿到 180 个零值点。
func callLogFileExists() bool {
	callLog.mu.RLock()
	p := callLog.path
	callLog.mu.RUnlock()
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// sortedBucketKeysLocked 返回按 (Hour, Model) 升序排好的全部桶键。
// 调用方必须持有 callLog.mu（读或写锁皆可）。
//
// 🔴 **必须排序**：Go 的 map 遍历顺序是随机化的。直接 `for k := range hourBuckets`
// 会让同一份数据两次查询的浮点累加顺序不同 → 同一请求连发两次的
// avg_ttft_ms / avg_output_speed 不一致，「逐字节相同」在浮点字段上永远不成立。
// 排序把累加顺序钉死成「小时升序、模型名升序」，与文件里的行序无关，因而可复现。
func sortedBucketKeysLocked() []hourModelKey {
	keys := make([]hourModelKey, 0, len(hourBuckets))
	for k := range hourBuckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Hour != keys[j].Hour {
			return keys[i].Hour < keys[j].Hour
		}
		return keys[i].Model < keys[j].Model
	})
	return keys
}

// bucketIndexOf 把「本地整点」的桶键还原成窗口内的桶下标；窗口外返回 -1。
//
// 🔴 窗口边界**只由 bidx 决定**，这里绝不复制 dayIdx / today 那套算式。
// dayIdx 依赖本地零点 + 24 小时除法，把它的算式抄一份就等于埋一个分叉：
// 改 TZ 或遇 DST 时两份算式会各自演化，最终 models 与 trends_by_model 落进不同窗口。
// hourModelKey.Hour 是「本地整点」，time.Unix 还原回该本地时刻后交回 bidx 判定，
// 边界语义与改造前逐字一致（改造前喂给 bidx 的是记录的时间戳，
// 而记录所属的小时键正是由同一个 hourKeyFor 截到整点得到的）。
//
// 已知边界（真实但有界）：today 是滚动 24h，start 由 time.Truncate(time.Hour)
// 得到，而 Truncate 截的是**绝对时间**，在 UTC 偏移不是整小时的时区
// （+05:30 / +05:45 等）会与本地整点网格错开半个 / 四分之三小时。
// 那种时区里，[HH:30, HH+1:00) 的记录会被归到前一个桶下标。
// 7d/30d/180d 不受影响：日桶边界是本地零点，本地整点永远不会跨过本地零点。
// 本项目部署时区为 +08:00（整小时偏移），不落在该边界内。
func bucketIndexOf(hour int64, n int, bidx func(time.Time) int) int {
	i := bidx(time.Unix(hour, 0).Local())
	if i < 0 || i >= n {
		return -1
	}
	return i
}

// mergeAggTo 把源桶累加进目标桶。字段逐字对齐 hourModelAgg。
func mergeAggTo(dst *hourModelAgg, s *hourModelAgg) {
	dst.N += s.N
	dst.OK += s.OK
	dst.Fail += s.Fail
	dst.PT += s.PT
	dst.CT += s.CT
	dst.CC += s.CC
	dst.CR += s.CR
	dst.SpeedSum += s.SpeedSum
	dst.SpeedN += s.SpeedN
	dst.TTFTSum += s.TTFTSum
	dst.TTFTN += s.TTFTN
}

// foldBuckets 把小时桶折叠进 trendBuckets 给定的窗口，返回每个桶索引的聚合值。
//
// 本函数**自己调 ensureBucketsUpToDate**：任何读桶之前必须先把未吸的字节吸进来。
// 反过来做（先折叠、末尾才 ensure）会让冷启动首次查询拿到空模型表。
func foldBuckets(ts []string, bidx func(time.Time) int) map[int]*hourModelAgg {
	ensureBucketsUpToDate()

	callLog.mu.RLock()
	defer callLog.mu.RUnlock()

	out := make(map[int]*hourModelAgg)
	for _, k := range sortedBucketKeysLocked() {
		i := bucketIndexOf(k.Hour, len(ts), bidx)
		if i < 0 {
			continue
		}
		dst := out[i]
		if dst == nil {
			dst = &hourModelAgg{}
			out[i] = dst
		}
		mergeAggTo(dst, hourBuckets[k])
	}
	return out
}

// foldBucketsByModel 保留模型维度地折叠小时桶：模型名 → 桶索引 → 聚合值。
//
// 为什么不复用 foldBuckets：那个函数把模型维度折叠掉了，
// 而这里必须逐模型输出整张网格（否则前端折线断裂）。
//
// 🔴 本函数**不调 ensureBucketsUpToDate**，调用方必须自己先调。
// （早先版本里它靠「trendsFromCallLog 恰好也调了 foldBuckets」被顺带喂活，
//
//	那掩盖了这个洞：一旦只有本函数被调用就会返回空。）
func foldBucketsByModel(ts []string, bidx func(time.Time) int) map[string]map[int]*hourModelAgg {
	callLog.mu.RLock()
	defer callLog.mu.RUnlock()

	out := make(map[string]map[int]*hourModelAgg)
	for _, k := range sortedBucketKeysLocked() {
		i := bucketIndexOf(k.Hour, len(ts), bidx)
		if i < 0 {
			continue
		}
		m := out[k.Model]
		if m == nil {
			m = make(map[int]*hourModelAgg)
			out[k.Model] = m
		}
		dst := m[i]
		if dst == nil {
			dst = &hourModelAgg{}
			m[i] = dst
		}
		mergeAggTo(dst, hourBuckets[k])
	}
	return out
}

// sortedModelsInWindow 返回窗口内出现过请求的模型名（升序）。
// 等价于改造前在 callback 内建键的行为（先过滤窗口再建键）。
//
// 注意 model == "" 是合法键，**不要过滤**——过滤会让面板的
// 「新增输入 + Output + 命中 == 真实消耗」这条闭合不变量红。
func sortedModelsInWindow(ts []string, bidx func(time.Time) int) []string {
	callLog.mu.RLock()
	set := make(map[string]bool)
	for k := range hourBuckets {
		if bucketIndexOf(k.Hour, len(ts), bidx) < 0 {
			continue
		}
		set[k.Model] = true
	}
	callLog.mu.RUnlock()
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// foldOneModel 把某模型的所有窗口内桶合并成一个值。
//
// 🔴 遍历顺序按 (Hour, Model) 排序而非 map 随机序，理由同 sortedBucketKeysLocked。
func foldOneModel(name string, ts []string, bidx func(time.Time) int) *hourModelAgg {
	callLog.mu.RLock()
	defer callLog.mu.RUnlock()
	var dst hourModelAgg
	for _, k := range sortedBucketKeysLocked() {
		if k.Model != name {
			continue
		}
		if bucketIndexOf(k.Hour, len(ts), bidx) < 0 {
			continue
		}
		mergeAggTo(&dst, hourBuckets[k])
	}
	return &dst
}

// ---- 全局环形缓冲 + JSONL 落盘 ----

type callLogStore struct {
	// RWMutex 而非 Mutex：纯读路径（latest / loadCallLogFromFile /
	// forEachBucketedCallRecord）用 RLock，写路径（append/clear）用 Lock。
	// Task 8 起查询侧读 hourBuckets 也走 RLock。
	mu      sync.RWMutex
	records []CallRecord
	path    string
}

var callLog = &callLogStore{}

func initCallLog(configPath string) {
	callLog.mu.Lock()
	callLog.path = filepath.Join(filepath.Dir(configPath), callLogFileName)
	callLog.mu.Unlock()
	loadCallLogFromFile()
}

// tailChunkBytes 是反向读取的块大小。它是最大的调优杠杆：
// 实测 100.4 MB 文件取最后 2000 行时，
//
//	16 KiB → 62 次 ReadAt / 20.23 ms
//	64 KiB → 16 次 ReadAt /  6.40 ms
//	256 KiB → 4 次 ReadAt /   0.90 ms
//
// 跨 15 倍块大小，ReadAt 次数跨 15 倍。取 256 KiB。
// 注意：若记录平均涨到 5 KB，2000 条就是 10 MB，实测 256 KiB 块仍需 ~122 ms——
// tail-read 是 O(想要的条数) 而非 O(文件大小)，但不是免费的。
const tailChunkBytes = 256 * 1024

// tailReadLastLines 从文件尾部反向读取最后 n 行（以换行符计数）。
// 返回的字节首部必然是行首；尾部一定是换行。
//
// 两个必须避开的陷阱：
//  1. 不能写 buf = append(chunk[:got:got], buf...)
//     首轮 buf 是 nil，append(s, 空切片...) **返回 s 本身而不拷贝**，
//     buf 于是别名 chunk；下一轮 ReadAt 覆写 chunk 会把已收集的尾部
//     数据原地覆盖。实测 256KiB 块读 1MB 时有 3 次这样的覆盖。
//  2. 读够 n 个换行就停之后**必须裁剪**。一块 256 KiB 能装下远超 n 行，
//     此时读到文件头就不裁了，不裁会返回整个文件。
func tailReadLastLines(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size == 0 {
		return []byte{}, nil
	}

	chunk := make([]byte, tailChunkBytes)
	var buf []byte
	off := size
	for off > 0 {
		want := int64(tailChunkBytes)
		if off < want {
			want = off
		}
		off -= want
		got, rerr := f.ReadAt(chunk[:want], off)
		if rerr != nil && rerr != io.EOF {
			return nil, rerr
		}
		if got == 0 {
			break
		}
		buf = append(append(make([]byte, 0, len(buf)+got), chunk[:got]...), buf...)
		if bytes.Count(buf, []byte{'\n'}) >= n {
			break
		}
	}

	// 截断到最后 n 行：从后往前数到第 n+1 个换行，切在它之后。
	// 数 n 会切掉首行、只留 n-1 行。
	if bytes.Count(buf, []byte{'\n'}) >= n {
		seen, cut := 0, -1
		for i := len(buf) - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				seen++
				if seen == n+1 {
					cut = i
					break
				}
			}
		}
		if cut >= 0 {
			buf = buf[cut+1:]
		}
	}

	// 丢弃崩溃残留的尾部半行：缓冲区止于最后一个完整换行。
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[:i+1]
	} else {
		buf = nil
	}
	return buf, nil
}

func loadCallLogFromFile() {
	callLog.mu.RLock()
	p := callLog.path
	callLog.mu.RUnlock()
	if p == "" {
		return
	}
	// 尾部倒读：只需最后 callLogCapacity 条，却原本解析了全文件 18.6 万行
	// ——生产实测 2137 ms 花在扔掉 99%。
	raw, err := tailReadLastLines(p, callLogCapacity)
	if err != nil {
		return
	}
	records := make([]CallRecord, 0, callLogCapacity)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec CallRecord
		if json.Unmarshal([]byte(line), &rec) == nil && rec.ReqID != "" {
			records = append(records, rec)
		}
	}
	if len(records) > callLogCapacity {
		records = records[len(records)-callLogCapacity:]
	}
	callLog.mu.Lock()
	callLog.records = records
	callLog.mu.Unlock()
}

// writeBytes 是 os.File.Write 的可注入包装，唯一目的是让测试能制造短写：
// io.Writer 契约允许 n < len(b) 且 err == nil，而真实 fd 上内核要么写满、
// 要么报错，短写根本不出现——可「短写必须当成失败并回滚」这条不变量
// 只在它出现时才可见，不注入就只能靠注释声称覆盖了。
var writeBytes = func(f *os.File, b []byte) (int, error) { return f.Write(b) }

// truncateCallLog 把日志文件截回到 size。size < 0 表示「写入前大小未知」，
// 此时什么都不做——按猜测截断会毁掉尚未 backfill 的存量字节。
func truncateCallLog(path string, size int64) error {
	if size < 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	err = f.Truncate(size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// writeCallLogLine 把一行 JSONL 追加落盘。
// 返回值 sizeBefore 是写入前的文件大小；err 非 nil 表示整条写入失败
// （OpenFile 失败 / f.Write 报错 / 短写，三者合一，调用方不区分）。
func writeCallLogLine(path string, line []byte) (int64, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return -1, err
	}
	// 写前先记下大小：写入失败时调用方要靠它把已落盘的部分字节截回去。
	// 整条 append 都在 callLog.mu 内、全进程没有第二个写者，
	// 故此刻的 size 就是本次写入前的 size。
	sizeBefore := int64(-1)
	if st, serr := f.Stat(); serr == nil {
		sizeBefore = st.Size()
	}
	n, err := writeBytes(f, line)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return sizeBefore, err
}

func (s *callLogStore) append(rec CallRecord) {
	s.mu.Lock()
	s.records = append(s.records, rec)
	if len(s.records) > callLogCapacity {
		s.records = s.records[len(s.records)-callLogCapacity:]
	}
	p := s.path
	// 落盘裁剪：Events 仅供内存环与面板日志页展示，磁盘侧的聚合查询
	// （forEachBucketedCallRecord 及其三个调用方）只读 9 个标量字段，
	// 从不访问 Events。实测 events 占全文件 38.8%，其中 73.1% 是
	// detail 恒为空的 connect_ok，其信息已被同记录的 nodes[0]/node_names[0]
	// 完全覆盖。
	// 代价：重启后内存环里那批记录（约 12.9 小时的量）没有事件时间线。
	//
	// Marshal 必须在锁内、且紧邻落盘：line 的长度就是 parsedTo 的增量，
	// 两者必须是同一份字节，否则推进量与真正写下去的量会对不上。
	diskRec := rec
	diskRec.Events = nil
	var line []byte
	if p != "" {
		// Marshal 失败（OutputSpeed 为 NaN/±Inf）时 line 为 nil，
		// 该记录仍留在内存环里供面板展示，只是不落盘 —— 与改动前同语义。
		if mb, merr := json.Marshal(diskRec); merr == nil {
			// +1 是行尾的 '\n'，line 即真正写下去的那份字节。
			line = append(mb, '\n')
		}
	}
	if line == nil {
		s.mu.Unlock()
		return
	}

	// ---- 落盘 ----
	//
	// 🔴 落盘必须在 callLog.mu 内，与 incr、parsedTo 推进同处一个临界区。
	// 这是「已吸字节恰好构成文件前缀」这条不变量能成立的**唯一**前提。
	// 修复前写盘在 Unlock 之后，于是 clear() 删掉的文件会被这次
	// O_CREATE 重建（实测 size=186、桶空、parsedTo=0、seeded=true），
	// 下次 backfill 把幽灵吸回 —— 用户点「清空」后统计复活。
	// 那时注释里「已吸字节 == 文件大小 == 0 是可证的」是假的：
	// 锁挡得住「先读后删」，挡不住「在途写」。
	//
	// 代价（实测，3000 次 × 3 轮，tmpfs/页缓存热）：
	// 单线程 12.5→14.7 µs/op（噪声内，无回归）；
	// 8/64/256 并发下聚合吞吐 8.5 µs/op → 21 µs/op，约掉 2.4 倍——
	// 串行成本没变，少的是「多条 append 的写并行」。
	// 绝对值仍约 4.8 万次/秒，远高于本服务的请求上限。
	// 真正的风险是**持锁期间 panel 的 latest()/clear() 会排在写盘后面**；
	// 若日后要消除，让 append 走一条专用写协程 + 队列，
	// 或在 store 里常开 *os.File 省掉每次 open/close（clear 时需重开）。
	sizeBefore, werr := writeCallLogLine(p, line)
	if werr != nil {
		// 回滚，两件事：
		//   1) 文件：把可能已落盘的部分字节截回写入前的大小。
		//      不截的话，短写留下的半行会永久粘住下一条 O_APPEND 的内容，
		//      那一行永久不可解析 —— 文件侧就永久丢了一条。
		//      截不回（size<0 或 Truncate 本身失败）时只能让 parsedTo 停在原地，
		//      残留的尾巴由 Task 7 的扫描路径容错（见报告 §遗留项）。
		_ = truncateCallLog(p, sizeBefore)
		//   2) 桶与 parsedTo：**什么都不做**。桶不 incr、parsedTo 不推进，
		//      这条记录下一轮 backfill 会从 [parsedTo, size) 重新吸一遍。
		//      注意这里不需要「减回去」——下面的 incr 被整个放在写成功之后，
		//      失败时压根没加过。
		//
		//      （若把 incr 挪到写之前、回滚就必须逐一撤销 N/OK/Fail/PT/CT/
		//      CC/CR/SpeedN/SpeedSum/TTFTN/TTFTSum 这 10 个字段，还得决定
		//      新建的桶要不要从 map 里删掉；而 SpeedSum/TTFTSum 是 float64，
		//      `x += v` 再 `x -= v` 在 IEEE754 下并不保证还原成 x，
		//      每次写失败都会给均值带来一次永久的尾数漂移。
		//      另外崩溃窗口也反过来：先 incr 后写，崩在中间就留下
		//      parsedTo > size —— 正好触发「全量重建」那条恢复规则。
		//      先写后 incr 崩在中间则是「文件有、桶没有、parsedTo 没推进」，
		//      backfill 会自然补上，自愈。）
		s.mu.Unlock()
		return
	}

	// ---- 桶 incr（写成功之后）----
	//
	// incr 必须在锁内。若放到锁外，并发查询读同一 map 会触发 Go 运行时的
	// fatal error: concurrent map read and map write —— 该错误 recover() 无效，直接杀进程。
	//
	// bucketsSeeded 为 false 时必须跳过：那批记录还躺在 backfill 的
	// 待吸区间 [0, size) 里，此刻 incr 会让待吸区间从文件中间起、被双重计入。
	// 详见 bucketsSeeded 的注释。
	if bucketsSeeded {
		if k, ok := hourKeyFor(rec.TS, rec.Model); ok {
			if hourBuckets == nil {
				hourBuckets = make(map[hourModelKey]*hourModelAgg)
			}
			agg := hourBuckets[k]
			if agg == nil {
				agg = &hourModelAgg{}
				hourBuckets[k] = agg
			}
			agg.add(&slimRec{
				TS: rec.TS, Model: rec.Model, Status: rec.Status,
				PromptTokens: rec.PromptTokens, CompletionTokens: rec.CompletionTokens,
				CacheCreation: rec.CacheCreation, CacheRead: rec.CacheRead,
				TTFTMs: rec.TTFTMs, OutputSpeed: rec.OutputSpeed,
			})
			// 🔴 **必须同时推进 parsedTo**。
			// append 已经把这条计进桶了；若 parsedTo 不跟着走，
			// 下次 backfill 会从 [parsedTo, size) 把刚写的字节**再扫一遍**，
			// 于是每个请求被永久计两次，且单调发散、永不自愈。
			//
			// 实测（把 Task 6 Step 5 + Task 7 Step 3 逐字抄出来跑）：
			//   backfill 历史 1 条 → N=1（真值 1）
			//   再 append 2 条      → N=3（真值 3）
			//   第一次查询（+1 条）  → N=5（真值 4）  ← 已经错
			//   第五次查询（+1 条）  → N=15（真值 8） ← 发散 2 倍
			//
			// 推进量 = len(line) = len(json.Marshal(diskRec)) + 1，
			// 与 writeCallLogLine 刚写下去的字节逐字节同源，且此刻
			// 文件大小恰好也是这个数 —— parsedTo == size 恒成立，
			// 「size < parsedTo → 全量重建」不再被稳态触发
			// （修复前实测 400 次观测命中 83 次，21%）。
			parsedTo += int64(len(line))
			// 🔴 absorbedRecords 必须与 parsedTo 在**同一处**推进（边界情况③）。
			// 落盘失败的两条回滚路径都在上面 return 了，到不了这里 ——
			// 于是「parsedTo 没动但计数动了」不可能发生，两者的差恒为 0。
			absorbedRecords++
		}
	}
	s.mu.Unlock()
}

func (s *callLogStore) latest(max int) []CallRecord {
	// 纯读路径：只拷 s.records，不写任何共享状态，故用 RLock。
	// 写者（append/clear/load）持 Lock 排他，两者互斥关系不变。
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := len(s.records)
	if max <= 0 || max > n {
		max = n
	}
	out := make([]CallRecord, max)
	copy(out, s.records[n-max:])
	return out
}

func (s *callLogStore) clear() {
	s.mu.Lock()
	p := s.path
	s.records = nil
	resetAbsorbedStateLocked()
	// 置 true 的依据：append 的落盘现在也在 s.mu 内（见 append），
	// 所以本临界区里不可能有在途写入。函数返回时桶为空、parsedTo=0、
	// 文件不存在 —— 「已吸字节 == 文件大小 == 0」这次是真的可证。
	// （修复前落盘在锁外，这句话是假的：在途 append 会在 os.Remove 之后
	//  用 O_CREATE 把文件重建出来，那条幽灵记录会被下次 backfill 吸回。）
	bucketsSeeded = true
	statsGen++
	// os.Remove 必须在同一临界区内。若放在锁外，其后若有查询进来，
	// 会从 parsedTo=0 开始把刚被删掉的整个文件重新吸进桶，
	// 被清空的趋势图立刻复活（幽灵统计）。
	if p != "" {
		os.Remove(p)
	}
	s.mu.Unlock()
}

// ---- 请求级录制：ctx 贯穿（无 recorder 时全部 no-op，测试/直连路径零开销） ----

type callRecKey struct{}

type callRecorder struct {
	mu            sync.Mutex
	rec           CallRecord
	start         time.Time
	firstTokenAt  time.Time // 流式首 token 到达时间
	seenNodes     map[string]bool
	promptTok     int64
	completionTok int64
	cacheCreation int64
	cacheRead     int64
}

// beginCallLog 创建本次请求的调用记录并挂到 ctx；返回新 ctx。
func beginCallLog(ctx context.Context, path, model string, stream bool, routeMode string) context.Context {
	rec := CallRecord{
		ReqID:     getReqID(ctx),
		TS:        time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
		Path:      path,
		Model:     model,
		Stream:    stream,
		RouteMode: routeMode,
		Status:    "fail",
	}
	if rec.ReqID == "" {
		rec.ReqID = "req_" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	cr := &callRecorder{rec: rec, start: time.Now(), seenNodes: map[string]bool{}}
	return context.WithValue(ctx, callRecKey{}, cr)
}

func callRecorderFrom(ctx context.Context) *callRecorder {
	cr, _ := ctx.Value(callRecKey{}).(*callRecorder)
	return cr
}

func (cr *callRecorder) event(typ, node, detail string) {
	if cr == nil {
		return
	}
	// 指纹 → 可读节点名：外部（面板/日志消费方）无需再查节点池即可定位问题节点。
	name := proxyPool.nameOf(node)
	cr.mu.Lock()
	if node != "" && !cr.seenNodes[node] {
		cr.seenNodes[node] = true
		cr.rec.Nodes = append(cr.rec.Nodes, node)
		cr.rec.NodeNames = append(cr.rec.NodeNames, name)
	}
	cr.rec.Events = append(cr.rec.Events, CallEvent{
		Type:     typ,
		Node:     node,
		NodeName: name,
		Detail:   detail,
		At:       time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
	})
	cr.mu.Unlock()
}

// observeFirstToken 记录流式首 token 到达时间（仅首次调用生效）。
func (cr *callRecorder) observeFirstToken() {
	if cr == nil {
		return
	}
	cr.mu.Lock()
	if cr.firstTokenAt.IsZero() {
		cr.firstTokenAt = time.Now()
	}
	cr.mu.Unlock()
}

func (cr *callRecorder) finish(status int, errMsg string, promptTok, completionTok, cacheCreation, cacheRead int64) {
	if cr == nil {
		return
	}
	cr.mu.Lock()
	if cr.rec.Status == "ok" && status < 200 {
		// 已完成（流式头部 2xx 后 makeStreamDone）不再改写
	}
	if status >= 200 && status < 300 {
		cr.rec.Status = "ok"
	} else {
		cr.rec.Status = "fail"
	}
	if errMsg != "" && cr.rec.ErrMsg == "" {
		cr.rec.ErrMsg = errMsg
	}
	if promptTok > 0 {
		cr.promptTok = promptTok
	}
	if completionTok > 0 {
		cr.completionTok = completionTok
	}
	if cacheCreation > 0 {
		cr.cacheCreation = cacheCreation
	}
	if cacheRead > 0 {
		cr.cacheRead = cacheRead
	}
	cr.rec.PromptTokens = cr.promptTok
	cr.rec.CompletionTokens = cr.completionTok
	cr.rec.CacheCreation = cr.cacheCreation
	cr.rec.CacheRead = cr.cacheRead
	if !cr.start.IsZero() {
		cr.rec.DurationMS = time.Since(cr.start).Milliseconds()
	}
	// 计算首 token 延迟和输出速度（仅流式且有输出时）
	if cr.rec.Stream && !cr.firstTokenAt.IsZero() && cr.completionTok > 0 {
		cr.rec.TTFTMs = cr.firstTokenAt.Sub(cr.start).Milliseconds()
		genSec := time.Since(cr.firstTokenAt).Seconds()
		if genSec > 0 {
			cr.rec.OutputSpeed = float64(cr.completionTok) / genSec
			recordModelSpeed(cr.rec.Model, cr.rec.TTFTMs, cr.rec.OutputSpeed)
		}
	}
	rec := cr.rec
	cr.mu.Unlock()
	callLog.append(rec)
}

// 便捷包级函数：ctx 无 recorder 时全部安全 no-op。

func callLogEvent(ctx context.Context, typ, node, detail string) {
	callRecorderFrom(ctx).event(typ, node, detail)
}

func callLogFirstToken(ctx context.Context) {
	callRecorderFrom(ctx).observeFirstToken()
}

func callLogFinish(ctx context.Context, status int, errMsg string, promptTok, completionTok, cacheCreation, cacheRead int64) {
	callRecorderFrom(ctx).finish(status, errMsg, promptTok, completionTok, cacheCreation, cacheRead)
}

// usageFromMap 从上游 usage（OpenAI 或 Claude 风格）提取 输入/输出/缓存创建/缓存读取 token。
// 兼容字段：prompt_tokens|input_tokens / completion_tokens|output_tokens /
// cache_creation_input_tokens / cache_read_input_tokens | prompt_tokens_details.cached_tokens | input_tokens_details.cached_tokens。
func usageFromMap(u map[string]any) (pt, ct, cc, cr int64) {
	if u == nil {
		return 0, 0, 0, 0
	}
	if v, ok := usageIntField(u, "prompt_tokens"); ok {
		pt = int64(v)
	} else if v, ok := usageIntField(u, "input_tokens"); ok {
		pt = int64(v)
	}
	if v, ok := usageIntField(u, "completion_tokens"); ok {
		ct = int64(v)
	} else if v, ok := usageIntField(u, "output_tokens"); ok {
		ct = int64(v)
	}
	if v, ok := usageIntField(u, "cache_creation_input_tokens"); ok {
		cc = int64(v)
	}
	if v, ok := usageIntField(u, "cache_read_input_tokens"); ok {
		cr = int64(v)
	} else if d, ok := usageMapField(u, "prompt_tokens_details"); ok {
		if v, ok := usageIntField(d, "cached_tokens"); ok {
			cr = int64(v)
		}
	} else if d, ok := usageMapField(u, "input_tokens_details"); ok {
		if v, ok := usageIntField(d, "cached_tokens"); ok {
			cr = int64(v)
		}
	} else if v, ok := usageIntField(u, "prompt_cache_hit_tokens"); ok {
		cr = int64(v)
	}
	return pt, ct, cc, cr
}

// usageFromOpenAIBody 从 OpenAI 兼容成功响应体提取 token 用量（严格模式，缺失返回 0）。
func usageFromOpenAIBody(b []byte) (int64, int64, int64, int64) {
	var m struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(b, &m); err != nil || m.Usage == nil {
		return 0, 0, 0, 0
	}
	return usageFromMap(m.Usage)
}

// ---- 用量趋势：全量 JSONL 按本地时间聚簇 ----

// TrendsPoint 用量趋势单个时段桶（today 按小时，7d/30d 按天）。
type TrendsPoint struct {
	TS               string `json:"ts"`
	Requests         int64  `json:"requests"`
	OK               int64  `json:"ok"`
	Fail             int64  `json:"fail"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	CacheCreation    int64  `json:"cache_creation_tokens"`
	CacheRead        int64  `json:"cache_read_tokens"`
}

// ModelTrendPoint 用量趋势按模型分组的单桶记录。字段与 TrendsPoint 一致，
// 额外携带 Model，供前端做模型维度筛选。
type ModelTrendPoint struct {
	TS               string `json:"ts"`
	Model            string `json:"model"`
	Requests         int64  `json:"requests"`
	OK               int64  `json:"ok"`
	Fail             int64  `json:"fail"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	CacheCreation    int64  `json:"cache_creation_tokens"`
	CacheRead        int64  `json:"cache_read_tokens"`
}

// trendBuckets 按 range 切分时间桶：返回每个桶的时间标签与「时间→桶索引」映射。
// today 是**滚动最近 24 小时**（不是本地零点起的自然日），返回 24 个整点小时桶；
// 7d/30d/180d 返回逐日桶。均含零数据时段以保证连线连续。
//
// 改成滚动窗口的理由：零点锚定时，用户早上打开面板看的是昨天后半夜的流量，
// 那半天的记录整段落在窗口外，Hero 会显示 0。滚动 24 小时让「刚才还在用的窗口」
// 始终覆盖最近一段真实活动。
func trendBuckets(rng string) ([]string, func(time.Time) int) {
	now := time.Now()
	loc := now.Location()
	switch rng {
	case "7d", "30d", "180d":
		n := map[string]int{"7d": 7, "30d": 30, "180d": 180}[rng]
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(n - 1))
		ts := make([]string, n)
		for i := 0; i < n; i++ {
			ts[i] = start.AddDate(0, 0, i).Format("2006-01-02")
		}
		dayIdx := func(t time.Time) int {
			lt := t.Local()
			dayStart := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, lt.Location())
			return int(dayStart.Sub(start) / (24 * time.Hour))
		}
		return ts, dayIdx
	default: // today：滚动最近 24 小时，桶起点对齐到整点
		// 起于 23 小时前而非 24 小时前：24 个桶必须以「当前这一小时」收尾。
		// 若从 24 小时前起排，最后一个桶是 [now-1h, now)，当前小时被排除，
		// 刚发生的那批请求会整段落空——这正是滚动窗口要解决的问题本身。
		start := now.Add(-23 * time.Hour).Truncate(time.Hour)
		ts := make([]string, 24)
		for i := 0; i < 24; i++ {
			ts[i] = start.Add(time.Duration(i) * time.Hour).Format("2006-01-02T15:04")
		}
		return ts, func(t time.Time) int {
			// 必须显式判起点之前：Go 整数除法**向零截断**，
			// start-30m 的 Sub 为 -30m，int(-30m/1h) == 0 而不是 -1，
			// 于是「窗口起点之前最多整一小时」会全部落进 bucket 0。
			// 只靠 i < 0 兜不住负数半区。
			d := t.In(loc).Sub(start)
			if d < 0 {
				return -1
			}
			i := int(d / time.Hour)
			if i >= 24 {
				return -1
			}
			return i
		}
	}
}

// forEachBucketedCallRecord 用给定的桶配置遍历落在区间内的调用记录，
// fn 收到记录与桶索引。返回 false 表示日志文件不可读。
//
// ⚠️ **Task 8 起本函数不再有任何生产调用方**：三个聚合函数已改读内存小时桶。
// 它被保留下来有两层作用：
//  1. **测试基准**——对拍用例（TestBucketsMatchLegacyScan 等）用它算出「旧实现」的真值，
//     再与读桶的结果逐字段比对。它是改造前后唯一能同时跑通的参照系。
//  2. 它仍是**扫文件语义的唯一在册实现**：若日后要改窗口边界，
//     改桶路径之前可以先用它确认旧语义没有被理解错。
//
// 别把它当生产热路径，也别顺手删掉——删了对拍用例就失去基准。
// 若将来确认对拍用例也已迁移完毕，删除前请先确认没有任何测试再引用它。
func forEachBucketedCallRecord(ts []string, bidx func(time.Time) int, fn func(rec CallRecord, bucket int)) bool {
	// 只读 path，用 RLock。
	// ⚠️ fn 仍在 Unlock **之后**调用（下面那个循环里），顺序不能动：
	// 回调归调用方所有，让它在锁内跑等于把调用方的耗时算进全局写锁，
	// 而且 Task 8 起回调要读 hourBuckets —— 万一有回调回写共享状态，
	// 在锁内执行会直接踩 "concurrent map writes"。
	callLog.mu.RLock()
	p := callLog.path
	callLog.mu.RUnlock()
	data, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec CallRecord
		if json.Unmarshal([]byte(line), &rec) != nil || rec.TS == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, rec.TS)
		if err != nil {
			continue
		}
		i := bidx(t)
		if i < 0 || i >= len(ts) {
			continue
		}
		fn(rec, i)
	}
	return true
}

// 路径未初始化或文件缺失时返回空数组
//
// Task 8：数据来源从「扫文件」换成「读小时桶」，与改造前逐字段等价。
func trendsFromCallLog(rng string) []TrendsPoint {
	ts, bidx := trendBuckets(rng)
	buckets := make([]TrendsPoint, len(ts))
	for i, label := range ts {
		buckets[i] = TrendsPoint{TS: label}
	}
	// foldBuckets 内部先 ensureBucketsUpToDate 再读桶，顺序不能调换。
	for i, a := range foldBuckets(ts, bidx) {
		if i < 0 || i >= len(buckets) {
			continue
		}
		buckets[i].Requests += a.N
		buckets[i].OK += a.OK
		buckets[i].Fail += a.Fail
		buckets[i].PromptTokens += a.PT
		buckets[i].CompletionTokens += a.CT
		buckets[i].CacheCreation += a.CC
		buckets[i].CacheRead += a.CR
	}
	// 日志文件缺失时返回 nil，交给 handler 的 `if pts == nil` 转成 []。
	//
	// 为什么必须保留这个分支：冷进程上 /api/stats/trends?range=180d 会得到
	// 180 个全零对象，前端热力图会画出一条「全 0」的假曲线。
	// 桶化后「文件不存在」与「文件为空」在桶层面确实不可区分，
	// 所以判定必须直接看文件在不在，而不是看桶空不空。
	if !callLogFileExists() {
		return nil
	}
	return buckets
}

// trendsByModelFromCallLog 在 trendsFromCallLog 的切分基础上按模型再分一层。
// 对区间内出现过请求的每个模型输出全部桶（无数据填零），保证该模型的折线
// 覆盖整个区间而非只在有流量的几天出现。输出顺序：模型名升序，桶索引升序。
//
// Task 8：数据来源从「扫文件」换成「读小时桶」，与改造前逐字段等价。
//
// 模型键集的判定与改造前等价：只在窗口内的桶里出现过才输出
// （改造前是在 callback 内先按窗口过滤再建键）。
// 注意 model == "" 是合法键，不要过滤——过滤会让
// 「新增输入+Output+命中 == 真实消耗」这条闭合不变量红。
//
// ⚠️ 本函数与 modelStatsFromCallLog 各自调一次 trendBuckets，而它内部取 time.Now()。
// 单看任一个函数都无从察觉，但把两者放进**同一个响应**就会露馅：
// 两次取窗口之间若跨过整点/午夜，同一份 /api/stats?range= 响应里 models 与
// trends_by_model 会落在不同窗口，两个数字互相矛盾且不报错。
//
// 这条路径已修：adminStatsHandler 走 statsRangeResponseFor（main.go），
// 两个折叠共用同一份 ts/bidx。本函数保留给 /api/stats/trends?group=model
// 那条独立路径——那条路径只出一个字段，不存在「同响应两半互相矛盾」。
//
// ⚠️ 修法只共享了**窗口**，不是**快照**：即便共用 ts/bidx，一次并发的
// callLog.append 仍会让两半对不上（P ≈ append 速率 × models 折叠耗时）。
// 根治要单快照折叠（一次 RLock 内把桶拷出来、两个折叠都从副本算），属延后决策。
func trendsByModelFromCallLog(rng string) []ModelTrendPoint {
	// 🔴 必须在读桶之前 ensure。foldBucketsByModel 自己不调 ensure。
	ensureBucketsUpToDate()
	ts, bidx := trendBuckets(rng)
	return trendsByModelWithBuckets(ts, bidx)
}

// trendsByModelWithBuckets 是 trendsByModelFromCallLog 的内核：
// 接收**已经算好的**窗口配置，自己不再取 time.Now()。
// 这是让同一响应的两个折叠共用同一份窗口的前提——只要有一方自己调
// trendBuckets，「同窗」就无从保证。
func trendsByModelWithBuckets(ts []string, bidx func(time.Time) int) []ModelTrendPoint {
	per := foldBucketsByModel(ts, bidx)

	models := make([]string, 0, len(per))
	for name := range per {
		models = append(models, name)
	}
	sort.Strings(models)

	out := make([]ModelTrendPoint, 0, len(models)*len(ts))
	for _, name := range models {
		row := make([]ModelTrendPoint, len(ts))
		for j, label := range ts {
			row[j] = ModelTrendPoint{TS: label, Model: name}
		}
		for bi, a := range per[name] {
			if bi < 0 || bi >= len(row) {
				continue
			}
			row[bi].Requests += a.N
			row[bi].OK += a.OK
			row[bi].Fail += a.Fail
			row[bi].PromptTokens += a.PT
			row[bi].CompletionTokens += a.CT
			row[bi].CacheCreation += a.CC
			row[bi].CacheRead += a.CR
		}
		out = append(out, row...)
	}
	return out
}

// normalizeTrendRange 把 range 归一为 today|7d|30d|180d：
// 7d/30d/180d 之外一律 today，与 adminTrendsHandler 改动前的逐字判断同规则。
// 该判断原先内联在 adminTrendsHandler 里，现由两处共用同一份实现，
// 保证 /api/stats?range= 与 /api/stats/trends?range= 的窗口边界永不分叉。
func normalizeTrendRange(rng string) string {
	switch rng {
	case "7d", "30d", "180d":
		return rng
	default:
		return "today"
	}
}

// modelStatsFromCallLog 按时间窗重算 /api/stats 的模型统计，形状与累计计数器
// tokenStats 的 TokenStatsData 一致，但数字来自调用日志而非内存计数器——
// 内存计数器没有时间窗，前端切时间下拉时顶部数字不会变。
//
// ⚠️ **生产路径已不走本函数**：/api/stats?range= 改走
// statsRangeResponseFor(main.go)，因为它要与 trends_by_model 共用同一份窗口。
// 本函数保留下来是给单元测试当被测入口的（生产调用方为零，别把它当热路径）。
// 与 trendsByModelFromCallLog 不同，后者仍被 /api/stats/trends?group=model 调用。
//
// 窗口边界完全复用 trendBuckets(rng)：与 /api/stats/trends 是同一套边界，
// 故同一 rng 下「本函数的逐字段求和」与「趋势桶求和」必然相等。
//
// ⚠️ 刻意的口径偏离（**不要**当成 bug「修」回去）：RequestCount 统计
// **全部尝试**，含 Status != "ok" 的失败记录；而累计计数器
// recordTokenUsageWithCache 只在拿到上游 usage 时被调用，故只记成功。
// 两者不一致是有意为之：调用日志记全部尝试，趋势图的「请求次数」tab 也是
// 全部尝试，前端 Hero 的「总请求数」必须与趋势图对齐。演示数据实测差异：
// 成功 5,108 / 全部 5,446（差 338 条 error）。
//
// 其余口径的来源：
//   - TotalTokens = prompt + completion（不是上游报的 total_tokens）。这是前端
//     闭合不变量的前提：(新增输入 + Output + 命中) == 真实消耗，其中
//     新增输入 = prompt - cache_read，故真实消耗必须等于 prompt + completion。
//     注意这与累计计数器里 TotalTokens 取自 usage.total_tokens 的旧口径不同，
//     两者不可直接相减比较。
//   - AvgTTFTMs / AvgOutputSpeed / StreamReqCount：与 recordModelSpeed（main.go）
//     同算法——仅当 rec.OutputSpeed > 0 时计入，增量平均 avg += (v-avg)/n；
//     并且**同样跳过空 model 名**（recordModelSpeed 首行即 model == "" 时 return），
//     免得同一个 model="" 的 key 在同一份 JSON 里出现两种语义。
//     故空 model 名的记录会计入 RequestCount 与各项 token，却不计入速度统计。
//   - 缓存读写直接取 rec.CacheRead / rec.CacheCreation，不做 0 值过滤，
//     以免与趋势桶的求和口径分叉。
//   - 空 model 名按 rec.Model 原样作 key、不过滤（沿用 Ruling 7 的既定裁定）。
//
// 日志文件不可读（全新安装尚未落盘、或被 DELETE 清掉）时返回**非 nil** 的空结构：
// 返回 nil 指针会序列化成 "models":null，前端 renderStats 直接炸
// （本项目在 69ab7dd 踩过一次）。
//
// Task 8：数据来源从「扫文件」换成「读小时桶」，与改造前逐字段等价。
func modelStatsFromCallLog(rng string) *TokenStatsData {
	// ⚠️ 必须在这里、且在读桶【之前】调用。
	// sortedModelsInWindow 与 foldOneModel 都要读桶，而冷启动时桶是空的
	// （要等第一次查询才 backfill）。若把 ensure 放在末尾，
	// out.Models 会是空的而 out.TotalRequests 正常——
	// API 返回 {"models":{},"total_requests":186212}，
	// 数字在、模型全空、不报错、面板一片空白。
	ensureBucketsUpToDate()
	rng = normalizeTrendRange(rng)
	ts, bidx := trendBuckets(rng)
	return modelStatsWithBuckets(ts, bidx)
}

// modelStatsWithBuckets 是 modelStatsFromCallLog 的内核：
// 接收**已经算好的**窗口配置，自己不再取 time.Now()，也不再调 ensure。
// 与 trendsByModelWithBuckets 成对存在，让 statsRangeResponseFor
// 能把同一份窗口喂给两个折叠（见该函数的注释）。
func modelStatsWithBuckets(ts []string, bidx func(time.Time) int) *TokenStatsData {
	out := &TokenStatsData{Models: map[string]*ModelStats{}}

	for _, name := range sortedModelsInWindow(ts, bidx) {
		a := foldOneModel(name, ts, bidx)
		ms := &ModelStats{
			RequestCount:       a.N,
			PromptTokens:       a.PT,
			CompletionTokens:   a.CT,
			TotalTokens:        a.PT + a.CT,
			CacheReadTokens:    a.CR,
			CacheCreatedTokens: a.CC,
			StreamReqCount:     a.SpeedN,
		}
		// 速度均值用 Σsum/Σn，**不是**各桶平均的平均——
		// 后者过度加权小桶（实测 space-bunny-free 在 30d 窗口偏 -23.67%）。
		// 与 recordModelSpeed 的增量均值数学等价但浮点不逐位相等（相对差 ~1e-16），
		// UI 显示到 0.1 t/s 不可见。**不可修**：逐位一致要按桶顺序重放增量平均，
		// 复杂度回到 O(记录数)，与 O(桶数) 的目标冲突。
		//
		// 判据要挡**两种**非有限结果，缺一不可：
		//  1. 0/0 = NaN。两个计数各自判零即可（hourModelAgg.add 里 SpeedN 与
		//     TTFTN 恒同步增减，但不必依赖这条不变式）——写成同一个 if 时一旦
		//     两者发散就是 0/0。
		//  2. 分子本身非有限。add 是裸 `x += v`，两条 1e308 就把 SpeedSum 顶成
		//     +Inf（MaxFloat64 ≈ 1.798e308）；除以任何正数仍是 +Inf。
		//
		// 为什么第 2 种也必须挡：encoding/json 遇 **+Inf 与 NaN 同样**返回
		// 「空输出 + error」（json: unsupported value: +Inf），不是把它序列化成
		// null —— 于是整个 /api/stats?range= 变 500，而不是只脏一个字段。
		//
		// ⚠️ 这是**防御纵深，不是活 bug**。可达性已复核：
		//   - add 只在 OutputSpeed > 0 时累加 → NaN 进不来（NaN > 0 为假）；
		//   - jsonFloat 把 Inf/NaN **字面量**归零 → 字面量进不来；
		//   - 生产实测 output_speed ∈ [1.804, 215152]，两条相加溢出需每条
		//     ≥ 8.99e307，差 300 个数量级；写入侧上界
		//     int64 completionTok / 最小正 genSec ≈ 9.2e27，同样够不着；
		//   - TTFTSum 每项是 float64(int64)，要堆到 1.8e308 需约 1e289 行。
		// 唯一可达路径是**日志文件被手工编辑 / 损坏 / 另一个写入端产出荒谬值**。
		// 留着的理由是代价为零（每次查询两个 IsInf/IsNaN），而漏掉的代价是 500。
		if a.SpeedN > 0 {
			if v := a.SpeedSum / float64(a.SpeedN); !math.IsInf(v, 0) && !math.IsNaN(v) {
				ms.AvgOutputSpeed = v
			}
		}
		if a.TTFTN > 0 {
			if v := a.TTFTSum / float64(a.TTFTN); !math.IsInf(v, 0) && !math.IsNaN(v) {
				ms.AvgTTFTMs = v
			}
		}
		out.Models[name] = ms
		// 🔴 total_requests 与 models 取自**同一个 a**，不是再独立折叠一遍。
		// 两者数值恒等（都是窗口内按模型切分的同一批桶的请求数之和），
		// 但独立折叠要多一趟全桶扫描，且中间还夹着一次加解锁——
		// 并发 append 正好落在两次取锁之间时，Σmodel.request_count 与
		// total_requests 会差一条，面板上「合计对不上分项」。
		// 从同一个 a 累加让这条不变式变成构造上成立，不依赖任何时刻的巧合。
		out.TotalRequests += a.N
	}
	return out
}

// ---- API：GET /api/call-log?limit= / DELETE /api/call-log ----

func adminCallLogHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit := 500
		if v := r.URL.Query().Get("limit"); v != "" {
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
				limit = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		recs := callLog.latest(limit)
		// 最新在前（前端直接渲染）
		for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
			recs[i], recs[j] = recs[j], recs[i]
		}
		_ = json.NewEncoder(w).Encode(recs)
	case http.MethodDelete:
		callLog.clear()
		// 累计计数器必须一起清，否则两个数据源会永久脱节：
		// /api/stats 加了 ?range= 之后，概览页顶部读的是按窗口从调用日志
		// 算出来的聚合。日志被删了而 stats.json 里的历史总量还在，
		// 用户会看到「统计归零但总量没变」的矛盾现象。
		// 语义定死：清空日志 = 统计从头计，与 DELETE /api/stats 一致。
		tokenStatsMu.Lock()
		tokenStats = &TokenStatsData{Models: map[string]*ModelStats{}}
		tokenStatsMu.Unlock()
		saveTokenStats()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
