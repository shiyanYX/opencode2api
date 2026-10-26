package main

// 调用日志（Call Log）：一次上游请求一条结构化记录（req_id 贯穿全路径），
// 内存环形缓冲 + JSONL 落盘（与 config.json 同目录，重启可恢复），
// 供管理面板“调用日志”视图（列表/时段分析/节点分析，参考 opencode2api_enhance）。

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
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

	// ↓ 本批新增两个字段。★ 这是【唯一不可逆】的改动：JSONL 格式变了。
	//
	// 硬约束一：**不改任何已有字段名**，只追加。老行照样能反序列化 —— encoding/json
	// 对缺失键留零值，statscan.go 的手写解析器也只认它列出的键，其余忽略。
	// 硬约束二：**读取处一律兜底**。前端用 (r.dial_ms||0)/(r.final_status||0)，
	// 老日志（没有这两个字段）在面板上照常显示，不会渲染出 undefined。
	//
	// DialMS 建连耗时：来自 httptrace.GotConn 的墙钟值。复用连接时是毫秒级小值，
	// 新建连时是真实建连耗时；0 表示未记录到（老数据 / 该路径没挂 trace）。
	// 有了它才能在不新增探针的前提下观测【成功侧】建连分布 —— 之前只有失败侧尾部。
	DialMS int64 `json:"dial_ms,omitempty"`
	// FinalStatus 最终上游 HTTP 状态码。与 main.go 四处 log 的 "final_status"
	// （main.go:2701/2871/2995/3162）同源同值 —— 那些值本来就已经算出来了，
	// 这里只是把它落进 CallRecord，不再只进应用日志。
	FinalStatus int `json:"final_status,omitempty"`

	// Unmapped 表示「客户端传来的模型名无法映射到本部署可服务的模型」。
	// 这类请求**不进统计聚合**（不进 hourBuckets ⇒ 不进 models /
	// total_requests / 成功率分母），但**照常落盘**、照常出现在调用日志页，
	// 并单独计入 unmappedBuckets 供 /api/stats 的 unmapped_requests 字段。
	//
	// 为什么判定要落盘、而不是聚合时现算（三条理由，缺一不可）：
	//  1. **锁序**：聚合侧持 callLog.mu。现算需要再取 configMu / freeModelDocsMu /
	//     modelMu，凭空造出 callLog.mu → 其它锁 的锁序；落盘后聚合侧只看一个 bool。
	//  2. **确定性**：判定依赖 free_models_cache.json（上游文档目录），它会漂移。
	//     聚合时现算 ⇒ 同一条历史记录在不同日期被算成不同结果，桶内容非确定，
	//     一旦翻转还会误伤「ΣN == absorbed - unabsorbed」并触发自愈全量重扫。
	//  3. **成本**：回填一次要过 20 万行，逐行现算 = 40 万次 RLock。
	//
	// omitempty 在这里**故意不用**，代价是每行多约 17 字节
	// （`"unmapped":false,`，20 万行 ≈ 3.4 MB / 109 MB ≈ +3%）。
	// 换来的是「落盘判定权威」这条语义：
	//   - 本构建写的行**总是**带该键 ⇒ 读取侧一律采信落盘值，
	//     目录日后漂移也不会把已经判过「已映射」的记录追认成未映射；
	//   - 只有**本构建之前**写的行才没有该键 ⇒ 才走重算（见 SawUnmapped）。
	// 若改成 omitempty，新写的「已映射」行与老行在 JSON 上无法区分，
	// 于是**每一行**都要在回填时重算；那时 docs 目录一旦漂移，
	// `big-pickle` 这类「不在 -free 命名约定里、只靠文档表判定」的模型
	// 就会被追认成未映射，把真实成功记录（生产 4 条）从统计里删掉 ——
	// 违反本次的硬门禁「排除集里不得出现任何一条 status=ok」。
	Unmapped bool `json:"unmapped"`
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

// callLogDirName 是 daily 模式的日文件目录名（与 legacy 文件同目录）。
const callLogDirName = "call_log"

// 落盘精简时被省略的默认值；读取侧据此还原（前端/第三方消费者看到的与精简前一致）。
const (
	defaultCallPath  = "/v1/chat/completions"
	defaultRouteMode = "public"
)

// callLogHistoryMaxScanBytes 是单次历史查询的扫描预算（防御性上限，正常远达不到）：
// 180 天全量 ≈ 276 MB，超过预算就返回已有结果并置 X-Call-Log-Truncated。
const callLogHistoryMaxScanBytes = 256 << 20

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

	// ↓ DSH 同口径吞吐：存「和」不存「比率」。
	//
	// 为什么要另存而不复用 SpeedSum/SpeedN：SpeedSum 存的是**每条速率**之和，
	// Σ(速率)/Σn 得到的是「速率的算术平均」——它给每条请求同样的权重，
	// 与「这个窗口一共跑了多少 token、花了多少秒」是两回事。
	// 生产实测两者差 4.86 倍（452.70 vs 93.13）：0.8% 的超短请求贡献了速率和的
	// 69.4%，却只占总时间的 0.002%。DSH 面板用的是 Σtoken ÷ Σ秒，
	// 所以这里必须**分别存秒和、token 和**，除法留到查询侧做。
	//
	// SpeedTpsN 是样本数：它与 SpeedN 不同步（脏样本被守卫整体排除时，
	// SpeedN 计了、SpeedTpsN 不计），所以必须独立存，不能拿 SpeedN 当分母。
	SpeedSecSum float64 // Σ生成秒（= time.Since(firstTokenAt)，与写入侧同一个时间窗）
	SpeedTokSum int64   // Σ完成token
	SpeedTpsN   int64   // 吞吐样本数
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
	// unmappedBuckets 是**未映射请求**的平行桶表，键与 hourBuckets 同构
	// （同小时、同原始模型名），同样由 callLog.mu 保护。
	//
	// 为什么要单独一张表而不是塞进 hourBuckets 再加个维度：
	//   - hourBuckets 的每个键都会被 foldModelsInWindow / foldBuckets /
	//     trendsByModelWithBuckets 无条件折叠进 models / total_requests /
	//     trends，多一个维度就要改三处折叠内核的签名与语义；
	//   - 未映射请求**不需要** token / 速度 / 吞吐 —— 它们全都失败了
	//     （生产实测 661 条 0 成功），只数条数就够。
	// 所以这里只存 N（用同一个 hourModelAgg 类型省一个类型，只写 N 字段）。
	//
	// 与 hourBuckets 的守恒关系是**各自独立**的：
	//   ΣN(hourBuckets) + ΣN(unmappedBuckets) == absorbedRecords - unabsorbedRecords
	// 见 bucketNSumLocked / unmappedNSumLocked / checkBucketConservation。
	unmappedBuckets map[hourModelKey]*hourModelAgg
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

// servedModels 是「历史上至少成功过一次的模型名」集合（task-39）。
//
// 🔴 这是用户口径「概览只显示有成功的模型」的**判据载体**：
//
//	mapped = everServed(name) || 公式B
//
// 集合只增不减（单调），由 callLog.mu 保护。
var servedModels map[string]struct{}

// isServedModel 报告 name 是否在 everServed 里。
func isServedModel(name string) bool {
	if name == "" {
		return false
	}
	callLog.mu.RLock()
	defer callLog.mu.RUnlock()
	_, ok := servedModels[name]
	return ok
}

// markServedModel 把 name 记进 everServed。只增不减。
//
// 调用点在**成功后**（callRecorder.finish）与回填提交段。绝不因为目录刷新、
// clear 之外的任何事件删除条目 —— 单调性是本判据的核心承诺（见报告 §单调性）。
func markServedModel(name string) {
	if name == "" {
		return
	}
	callLog.mu.Lock()
	if servedModels == nil {
		servedModels = make(map[string]struct{}, 16)
	}
	servedModels[name] = struct{}{}
	callLog.mu.Unlock()
}

// servedModelsSnapshot 返回 everServed ∪ staged 的副本。
//
// 回填用：老日志行（无 unmapped 键）的最终裁决要等整轮扫完、stagedServed 收齐，
// 再拿这张并集去判 —— 否则结果依赖行序（日志 38.6% 乱序）。
// 只读、不修改全局，故取读锁即可（调用方在锁外）。
func servedModelsSnapshot(staged map[string]struct{}) map[string]struct{} {
	callLog.mu.RLock()
	defer callLog.mu.RUnlock()
	out := make(map[string]struct{}, len(servedModels)+len(staged))
	for m := range servedModels {
		out[m] = struct{}{}
	}
	for m := range staged {
		out[m] = struct{}{}
	}
	return out
}

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

// unmappedNSumLocked 返回 Σ N over unmappedBuckets。
// 调用方必须持有 callLog.mu（读或写锁皆可）。遍历必须在锁内，理由同 bucketNSumLocked。
func unmappedNSumLocked() int64 {
	var n int64
	for _, a := range unmappedBuckets {
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
//   - Σ N 是「桶里说的条数」，是**计数侧**的账。这里 Σ N 是
//     hourBuckets 与 unmappedBuckets **两张表之和** —— 未映射记录同样被
//     「消费」了（parsedTo 照样推进、absorbedRecords 照样自增），只是没进
//     hourBuckets。若只数 hourBuckets，每有一条未映射记录就会让判据差 1，
//     进而**每次查询都误判不守恒并触发自愈全量重扫**（真实数据 ~350ms）。
//   - unabsorbedRecords 数其中「消费了但两张表都没进」的（坏行 / ts 解析失败），
//     它把**合法的不等**从判据里扣掉。
//
// unabsorbed <= absorbed 由构造保证（每个 skip 必有一个 consume），
// 显式判一次是为了让「计数器自身被写坏」也走重建，而不是算出个巨大差值。
//
// 实测成本（673 个桶，-benchtime 2000x，本机）：7.8~9.0 µs/op，中位 8.5 µs（含读锁，0 分配）。
//
// ⚠️ 「一次 /api/stats 查询本身 ~170 µs」这个分母**已过期**，不要沿用。
// 重测端到端（取窗口 → 两个折叠内核 → json.Marshal，稳态下
// ensureBucketsUpToDate 是 no-op 故不计入；673 桶 / 17 模型 / 30d 窗口）：
//
//	修复前（每模型重排）3.33 ms/op、512 KB/op
//	修复后（提排序）    0.80 ms/op、246 KB/op
//
// 所以占比是 **~1.1%**，不是 5%。结论不变但更宽裕：
// **每次查询都做，不节流**。
func checkBucketConservation() (sum, absorbed, unabsorbed int64, ok bool) {
	callLog.mu.RLock()
	defer callLog.mu.RUnlock()
	sum = bucketNSumLocked() + unmappedNSumLocked()
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
// 过滤条件必须与 main.go 的 recordModelSpeed 完全一致：
// model 为空、或 OutputSpeed <= 0 时，StreamReqCount / AvgTTFTMs / AvgOutputSpeed 三者都不计。
func (a *hourModelAgg) add(r *slimRec) {
	a.N++
	if r.Status == "ok" {
		a.OK++
	} else {
		a.Fail++
	}
	a.PT = satAdd(a.PT, r.PromptTokens)
	a.CT = satAdd(a.CT, r.CompletionTokens)
	a.CC = satAdd(a.CC, r.CacheCreation)
	a.CR = satAdd(a.CR, r.CacheRead)
	if r.Model != "" && r.OutputSpeed > 0 {
		a.SpeedN++
		a.SpeedSum += r.OutputSpeed
		a.TTFTN++
		a.TTFTSum += float64(r.TTFTMs)
		// ---- 吞吐口径的累加（DSH 同口径：Σtoken ÷ Σ秒）----
		//
		// 三条守卫，**被拒的样本分子分母都不进**：
		//  1. ct > 0：否则分子为 0 而分母照进，凭空拉低吞吐。
		//  2. sec 有限且 > 0：sec = ct/output_speed 在 output_speed 极小时
		//     会溢出成 +Inf；SpeedSecSum 是裸 `x += v`，一次 +Inf 进去
		//     **整个模型桶的吞吐永久变 NaN**，而 NaN 不会被 IsInf 抓住
		//     —— 它在 json.Marshal 里与 +Inf 一样报错，整个 /api/stats 变 500。
		//  3. ★物理上界 sec <= (duration_ms+2)/1000。
		//
		// ★★ 第 3 条的真实余量是 **(1 - 首字等待/总耗时)**，不是「两次 time.Since
		// 之间的几微秒」。旧注释写的是后者，是错的：
		//   构造上 sec = duration - 首字等待 - 收尾开销，所以
		//   sec/duration ≈ 1 - TTFT/duration，而 TTFT/duration **没有下界**
		//   （ttft_ms 是毫秒截断量，首 token 立即到达时 ttft_ms=0 完全合法）。
		// 生产 11,540 条实测：余量中位数 73.5%，**最小只有 0.85%**
		//   （mimo-v2.5-free, 09-21 19:16, dur=618.3s, sec=613.1s）；
		//   ratio>0.95 的有 43 条(0.37%)，越过界只需多出 1.6 秒 —— 秒级，不是微秒级。
		// ⇒ 这条守卫【会误伤合法记录】：一条「首 token 立即到达」的长请求
		//   margin → 0，被判越界而整条丢弃。
		// ⇒ **它仍然必须留着**（防的是「有限但荒谬」的 sec：output_speed=1e-305
		//   推出 sec=1e308，!IsInf&&!IsNaN 判不出来，会把顶层吞吐静默变成 5.67e-302）。
		// ★ 认领的不变式：**任何一条合法记录都满足 sec <= (duration_ms+2)/1000**。
		//   若将来出现被这条守卫误伤的报告，第一个要查的就是 TTFT/duration 是否趋零，
		//   而不是去把 +2 改大 —— 那个余量不是毫秒量级的。
		if r.CompletionTokens > 0 {
			if sec := float64(r.CompletionTokens) / r.OutputSpeed; sec > 0 &&
				!math.IsInf(sec, 0) && !math.IsNaN(sec) &&
				(r.DurationMS <= 0 || sec <= (float64(r.DurationMS)+2)/1000.0) {
				a.SpeedSecSum += sec
				a.SpeedTokSum = satAdd(a.SpeedTokSum, r.CompletionTokens)
				a.SpeedTpsN++
			}
		}
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

// ---- task-56：按天分文件（daily 模式） ----
//
// daily 模式把「legacy 文件 + 日文件（升序）」当作**有序段列表**来吸收：
//
//	segIdx/segOff 游标替代单文件 parsedTo，段序 = 原单文件字节序，
//	因此桶/everServed/unmapped 的结果与单文件逐字节相同（见 calllog_daily_test.go）。
//
// 单文件模式（dir==""）完全走原有代码路径：parsedTo / markBucketsSeededIfFullyAbsorbed
// / handleBackfillOpenError 全部保持原语义 —— 这是既有 130+ 处测试引用不被打扰的前提。
type callLogSegment struct {
	name   string // "call_log.jsonl" 或 "2026-10-20.jsonl"/"2026-10-20.jsonl.gz"
	path   string // 绝对路径
	gz     bool
	legacy bool
	day    string // "YYYY-MM-DD"；legacy 为空串（排序恒最前）
	size   int64  // 最近观测到的逻辑大小（gz 为解压后大小，未知时 0）
}

type absorbCursor struct {
	idx int
	off int64
}

var (
	// dailySegs 是 daily 模式的段列表（由 callLog.dir 目录内容派生）。
	dailySegs []callLogSegment
	// dailySegsKey = path|dir，用于检测「测试切换了 store 路径」而重建段列表。
	dailySegsKey string
	// dailySegsDirMod 记录上次构建段列表时的目录 mtime；目录变了才重扫。
	dailySegsDirMod time.Time
	// dailyCur 是 daily 模式的吸收游标（等价单文件的 parsedTo）。
	dailyCur absorbCursor
)

// callLogLayout / callLogGzipAfterDays / callLogRetainDays 由 main.go 的命令行参数写入。
// 测试直接改这些变量（或只构造 store.dir）即可。
var (
	callLogLayout        = "daily"
	callLogGzipAfterDays = 2
	callLogRetainDays    = 180
)

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
	unmappedBuckets = nil
	parsedTo = 0
	absorbedRecords = 0
	unabsorbedRecords = 0
	// daily 模式的游标与段列表一起归零：下次吸收从头重放全部段。
	dailyCur = absorbCursor{}
	dailySegs = nil
	dailySegsKey = ""
	dailySegsDirMod = time.Time{}
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
	if n := bucketNSumLocked() + unmappedNSumLocked(); n != absorbedRecords-unabsorbedRecords {
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

// ---- daily 模式：段列表维护 ----

// ensureDailySegsLocked 在 dir != "" 时构建/刷新段列表。
// 调用方必须持有 callLog.mu **写锁**（会改 dailySegs/dailyCur）。
func (s *callLogStore) ensureDailySegsLocked() {
	if s.dir == "" {
		dailySegs, dailySegsKey, dailySegsDirMod = nil, "", time.Time{}
		return
	}
	key := s.path + "|" + s.dir
	var mod time.Time
	if st, err := os.Stat(s.dir); err == nil {
		mod = st.ModTime()
	}
	if dailySegsKey == key && dailySegs != nil && mod.Equal(dailySegsDirMod) {
		return
	}
	segs := make([]callLogSegment, 0, 32)
	if st, err := os.Stat(s.path); err == nil {
		segs = append(segs, callLogSegment{name: filepath.Base(s.path), path: s.path, legacy: true, size: st.Size()})
	}
	if ents, err := os.ReadDir(s.dir); err == nil {
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			var day string
			gz := false
			switch {
			case strings.HasSuffix(name, ".jsonl.gz"):
				day = strings.TrimSuffix(name, ".jsonl.gz")
				gz = true
			case strings.HasSuffix(name, ".jsonl"):
				day = strings.TrimSuffix(name, ".jsonl")
			default:
				continue // 未知文件忽略（不崩、不吸）
			}
			if _, perr := time.Parse("2006-01-02", day); perr != nil {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			segs = append(segs, callLogSegment{name: name, path: filepath.Join(s.dir, name), gz: gz, day: day, size: info.Size()})
		}
	}
	sort.SliceStable(segs, func(i, j int) bool {
		if segs[i].legacy != segs[j].legacy {
			return segs[i].legacy // legacy 恒第 0 段
		}
		if segs[i].day != segs[j].day {
			return segs[i].day < segs[j].day
		}
		return !segs[i].gz && segs[j].gz // 同日 plain 优先
	})
	// 同日 plain + gz 并存：选 plain（数据更全），忽略 gz（压缩中途崩溃的残影）
	dedup := segs[:0]
	for _, sg := range segs {
		if n := len(dedup); n > 0 && !sg.legacy && !dedup[n-1].legacy && sg.day == dedup[n-1].day {
			slog.Warn("calllog: duplicate day segment, keeping plain", "day", sg.day, "ignored", sg.name)
			continue
		}
		dedup = append(dedup, sg)
	}
	segs = dedup
	// 段列表变化后按路径保住游标（压缩只换路径、删除只删前缀，游标不应回退）
	if dailySegs != nil && dailyCur.idx >= 0 && dailyCur.idx < len(dailySegs) {
		oldPath := dailySegs[dailyCur.idx].path
		found := false
		for i := range segs {
			if segs[i].path == oldPath {
				dailyCur.idx = i
				found = true
				break
			}
		}
		if !found {
			dailyCur = absorbCursor{}
		}
	}
	dailySegs, dailySegsKey, dailySegsDirMod = segs, key, mod
}

// dailySegments 返回段列表副本（测试与诊断用）。
func (s *callLogStore) dailySegments() []callLogSegment {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureDailySegsLocked()
	return append([]callLogSegment(nil), dailySegs...)
}

// dailySegmentsCurrent 返回当前吸收游标（测试与诊断用）。
func (s *callLogStore) dailySegmentsCurrent() absorbCursor {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return dailyCur
}

// dailyTargetLocked 决定本次 append 写哪个文件，并保证**只追加到最后一段**。
// 调用方必须持有 callLog.mu 写锁。dir=="" 时返回 path（单文件模式）。
//
// 单调性：跨午夜长请求的 rec.TS 属于昨天，但写入必须落"今天"的段——
// 追加到已吸收的旧段会让新字节落在 parsedTo 之前，被永久重复计数。
func (s *callLogStore) dailyTargetLocked() string {
	if s.dir == "" {
		return s.path
	}
	s.ensureDailySegsLocked()
	today := time.Now().In(time.Local).Format("2006-01-02")
	if n := len(dailySegs); n > 0 {
		last := dailySegs[n-1]
		if !last.legacy && last.day >= today {
			return last.path // 已有今天（或时钟回拨）的段：追加
		}
	}
	return s.dailyCreateSegmentLocked(today)
}

// dailyCreateSegmentLocked 为新的一天建立段并（已对齐时）把游标移过去。
func (s *callLogStore) dailyCreateSegmentLocked(day string) string {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		// 不降级成单文件写 legacy：那会让 parsedTo 与 daily 游标语义打架（双计风险）。
		// 只丢这一条落盘（记录仍在内存环），并明确告警。
		slog.Error("calllog: cannot create daily dir, record not persisted", "dir", s.dir, "error", err)
		return ""
	}
	path := filepath.Join(s.dir, day+".jsonl")
	dailySegs = append(dailySegs, callLogSegment{name: day + ".jsonl", path: path, day: day})
	dailySegsDirMod = time.Time{} // 目录 mtime 已变，下次重新观测
	if bucketsSeeded {
		dailyCur = absorbCursor{idx: len(dailySegs) - 1, off: 0}
	}
	return path
}

// readDailySegment 读入一段的全部逻辑字节（plain 直接读，gz 解压后读）。
func readDailySegment(path string, gz bool) ([]byte, int64, error) {
	if !gz {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, 0, err
		}
		return data, int64(len(data)), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, 0, err
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, 0, err
	}
	return data, int64(len(data)), nil
}

// dailyAbsorb 累积一段扫描的 staged 结果（与单文件版本的中间变量同构）。
type dailyAbsorb struct {
	staged         map[hourModelKey]*hourModelAgg
	stagedUnmapped map[hourModelKey]*hourModelAgg
	stagedServed   map[string]struct{}
	pending        []pendingServedRec
	unmappedMemo   map[string]bool
	lines          int64
	skipped        int64
}

// consume 消费一行：与单文件解析循环逐字同语义（含 everServed / 未映射裁决）。
func (a *dailyAbsorb) consume(line []byte) {
	a.lines++
	var r slimRec
	if !parseStatsLine(line, &r) {
		a.skipped++
		return
	}
	k, ok := hourKeyFor(r.TS, r.Model)
	if !ok {
		a.skipped++
		return
	}
	if r.Status == "ok" && r.Model != "" {
		a.stagedServed[r.Model] = struct{}{}
	}
	unmapped := r.Unmapped
	if r.SawUnmapped {
		if r.Status == "ok" {
			unmapped = false
		}
	} else {
		v, hit := a.unmappedMemo[r.Model]
		if !hit {
			v = isUnmappedByFormula(r.Model)
			a.unmappedMemo[r.Model] = v
		}
		if v {
			a.pending = append(a.pending, pendingServedRec{k: k, r: r})
			return
		}
		unmapped = false
	}
	target := a.staged
	if unmapped {
		target = a.stagedUnmapped
	}
	b := target[k]
	if b == nil {
		b = &hourModelAgg{}
		target[k] = b
	}
	b.add(&r)
}

// resolvePending 等整轮扫完后再用 everServed 裁决老行（与单文件版同语义）。
func (a *dailyAbsorb) resolvePending() {
	if len(a.pending) == 0 {
		return
	}
	ever := servedModelsSnapshot(a.stagedServed)
	for i := range a.pending {
		p := &a.pending[i]
		target := a.stagedUnmapped
		if _, served := ever[p.r.Model]; served {
			target = a.staged
		}
		b := target[p.k]
		if b == nil {
			b = &hourModelAgg{}
			target[p.k] = b
		}
		b.add(&p.r)
	}
}

// absorbSegmentDaily 流式吸收一段：用 bufio 逐行读，不把整段读进内存。
//
// 为什么不用 readDailySegment 整读：legacy 段可能是 124 MB 的大文件，
// 整读会让冷启动峰值内存回到 124 MB 且多一次大对象 GC（实测 L2 冷查询 1.71s
// vs L3 1.01s，多出来的主要就是这次 124 MB 分配/回收）。流式后峰值 ≈ 256 KiB 缓冲。
//
// 返回 (段内已消费字节偏移, 逻辑大小, error)。
// 非最后段尾部的半行会被消费（计入 unabsorbed）；最后段保留给下一轮。
func absorbSegmentDaily(seg callLogSegment, start int64, isLast bool, a *dailyAbsorb) (int64, int64, error) {
	f, err := os.Open(seg.path)
	if err != nil {
		return start, -1, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return start, -1, err
	}
	size := int64(-1)
	if !seg.gz {
		size = st.Size()
		if size < start {
			return start, size, nil // 截短：调用方负责全量重吸
		}
	}
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return start, size, err
		}
	}
	var r io.Reader = f
	if seg.gz {
		zr, zerr := gzip.NewReader(f)
		if zerr != nil {
			return start, -1, zerr
		}
		defer zr.Close()
		r = zr
	}
	br := bufio.NewReaderSize(r, 256*1024)
	pos := start
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			if rerr == nil {
				a.consume(line[:len(line)-1])
				pos += int64(len(line))
			} else if !isLast {
				// 非最后段残留半行：消费掉并记账，否则待吸区间永远卡在它前面
				a.lines++
				a.skipped++
				pos += int64(len(line))
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return pos, size, rerr
		}
	}
	if seg.gz {
		// gz 段必然读到 EOF，pos-start 就是解压后总字节数（用于 seeded 判定）
		size = pos
	}
	return pos, size, nil
}

// absorbOnceDaily 是 daily 模式的吸收本体。
// 段序 = legacy → 日文件升序 → 段内追加序 = 原单文件字节序，故结果与单文件一致。
func absorbOnceDaily() {
	s := callLog
	s.mu.Lock()
	s.ensureDailySegsLocked()
	segs := append([]callLogSegment(nil), dailySegs...)
	gen := statsGen
	from := dailyCur
	seeded := bucketsSeeded
	s.mu.Unlock()

	if len(segs) == 0 {
		// 无任何段：等价「日志文件不存在」——清空状态并视为已对齐。
		s.mu.Lock()
		if statsGen == gen {
			resetAbsorbedStateLocked()
			bucketsSeeded = true
		}
		s.mu.Unlock()
		return
	}
	if from.idx < 0 || from.idx >= len(segs) {
		from = absorbCursor{}
	}
	// 稳态快路径：游标在最后一段末尾且文件没长 ⇒ 无事可做
	if seeded && from.idx == len(segs)-1 && !segs[from.idx].gz {
		if st, err := os.Stat(segs[from.idx].path); err == nil && st.Size() == from.off {
			return
		}
	}

	a := &dailyAbsorb{
		staged:         make(map[hourModelKey]*hourModelAgg, 512),
		stagedUnmapped: make(map[hourModelKey]*hourModelAgg, 16),
		stagedServed:   make(map[string]struct{}, 32),
		unmappedMemo:   map[string]bool{},
	}
	finalIdx, finalOff := from.idx, from.off
	lastSize := int64(-1)
	for idx := from.idx; idx < len(segs); idx++ {
		seg := segs[idx]
		start := int64(0)
		if idx == from.idx {
			start = from.off
		}
		isLast := idx == len(segs)-1
		consumed, size, err := absorbSegmentDaily(seg, start, isLast, a)
		if err != nil {
			if os.IsNotExist(err) {
				return // 段被压缩/删除：下轮重建段列表
			}
			if seg.gz {
				// 坏 gz：跳过该段（该段统计缺失，但不阻塞其它段；守恒仍成立）
				slog.Warn("calllog: cannot read segment, skipping", "path", seg.path, "error", err)
				finalIdx, finalOff = idx, 0
				continue
			}
			slog.Warn("calllog: cannot read segment", "path", seg.path, "error", err)
			return
		}
		if !seg.gz && size >= 0 && size < start {
			// 文件被截短/替换：全量重吸（与单文件 size<parsedTo 同款）
			s.mu.Lock()
			if statsGen == gen {
				resetAbsorbedStateLocked()
				bucketsSeeded = false
			}
			s.mu.Unlock()
			return
		}
		finalIdx, finalOff = idx, consumed
		if isLast {
			lastSize = size
		}
	}
	a.resolvePending()

	// ---- 锁内提交（与单文件版的四条守卫一一对应）----
	s.mu.Lock()
	defer s.mu.Unlock()
	if statsGen != gen {
		return // 期间 clear 过：本次结果作废
	}
	if dailyCur != from {
		return // 期间 append（seeded 时）推进过游标：整批丢弃，下轮重来
	}
	mergeStagedLocked(a.staged, a.stagedUnmapped, a.stagedServed)
	absorbedRecords += a.lines
	unabsorbedRecords += a.skipped
	dailyCur = absorbCursor{idx: finalIdx, off: finalOff}
	// 只有「段列表没变 + 停在最后一段末尾」才置位（否则新日文件刚出现，不能算吸干净）
	if len(dailySegs) == len(segs) && finalIdx == len(segs)-1 && lastSize >= 0 && finalOff == lastSize {
		bucketsSeeded = true
	}
}

// mergeStagedLocked 把一批 staged 结果并进全局桶/everServed。
// 调用方必须持有 callLog.mu **写锁**；单文件模式与 daily 模式共用（行为与合并顺序无关）。
// 抽出来是为了让两种吸收路径共用同一份合并代码，避免「加字段漏改一处」。
func mergeStagedLocked(staged, stagedUnmapped map[hourModelKey]*hourModelAgg, stagedServed map[string]struct{}) {
	if hourBuckets == nil {
		hourBuckets = make(map[hourModelKey]*hourModelAgg, len(staged))
	}
	// everServed 与两条桶表在**同一个临界区**里提交：这样「集合说它是我们的模型」
	// 与「它的记录进了概览桶」永远不可能被看到一个已经生效、另一个还没生效的中间态。
	// 只增不减，故直接合并、不需要 mergeAggTo 那种 += 语义。
	if len(stagedServed) > 0 {
		if servedModels == nil {
			servedModels = make(map[string]struct{}, len(stagedServed))
		}
		for m := range stagedServed {
			servedModels[m] = struct{}{}
		}
	}
	// 未映射暂存区并进平行表。合并规则与 staged 完全一致（必须 +=，不能覆盖），
	// 且**在同一临界区内**完成 —— 与 parsedTo 的提交同锁，不存在半合并可见窗口。
	if len(stagedUnmapped) > 0 {
		if unmappedBuckets == nil {
			unmappedBuckets = make(map[hourModelKey]*hourModelAgg, len(stagedUnmapped))
		}
		for k, v := range stagedUnmapped {
			b := unmappedBuckets[k]
			if b == nil {
				unmappedBuckets[k] = v
				continue
			}
			mergeAggTo(b, v)
		}
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
		//
		// 🔴🔴 这里**只能调 mergeAggTo**，不得再写逐字拷贝：
		// 每加一个聚合字段要记得在这里补一行，而「忘了补」不报任何错——
		// 实测故意不加时 go test ./... 全绿，真日志跑下来 ΣSpeedTokSum=0、
		// 历史吞吐指标全量归零。mergeAggTo 是全仓唯一的合并写点，
		// 由 tps_throughput_struct_test.go 的结构断言钉住。
		mergeAggTo(b, v)
	}
}

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

// pendingServedRec 是一条「公式 B 判未映射」的老日志行（没有落盘 unmapped 键），
// 它进哪张表要等整轮扫完、stagedServed 收齐后按 everServed 裁决。
// 必须先记下 hourKeyFor 的结果：延后裁决时不该再解析一次 ts（时区归一很贵且容易写错）。
type pendingServedRec struct {
	k hourModelKey
	r slimRec
}

// absorbOnce 是 ensureBucketsUpToDate 的吸收本体（不含 single-flight 与守恒检查）。
// 调用方**必须持有 absorbMu**。
func absorbOnce() {
	callLog.mu.RLock()
	dir := callLog.dir
	callLog.mu.RUnlock()
	if dir != "" {
		absorbOnceDaily()
		return
	}
	absorbOnceSingle()
}

// absorbOnceSingle 是原有的单文件吸收本体（dir=="" 的路径，语义逐字保留）。
func absorbOnceSingle() {

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
	// stagedUnmapped 与 staged 同构，装未映射记录。分开暂存是为了让锁内合并段
	// 能一次判 gen / 一次提交 parsedTo，而两张表各自 += 到目标全局。
	stagedUnmapped := make(map[hourModelKey]*hourModelAgg, 16)
	// stagedServed 装本轮扫描中「出现过 status=ok」的模型名（Task 39 的 everServed）。
	// 它与两条桶表**同一次提交**：合并段里并进全局 servedModels。
	stagedServed := make(map[string]struct{}, 32)
	// pendingServed 暂存「按公式 B 判未映射」的老日志行（没有 unmapped 键的那些），
	// 等整轮扫完、stagedServed 收齐之后，再用 everServed 一次性裁决它们进哪张表。
	//
	// 🔴 为什么必须延后而不是边扫边判：everServed 是**全局**属性，而生产日志
	// 38.6% 的记录是乱序的 —— 同一条失败记录既可能排在它自己那条成功记录之前，
	// 也可能排在之后。边扫边判 ⇒ 同一份文件换个行序就得出不同的桶，
	// 而桶是守恒判据「ΣN == absorbed - unabsorbed」的一端，非确定会误触发自愈重建。
	// 所以判定被拆成两半：**公式 B（只依赖当前目录，可按模型名记忆化）** +
	// **everServed（必须等全局收齐）**。
	pendingServed := make([]pendingServedRec, 0, 64)
	consumed := from
	// lines 记本轮真正被消费的**完整行数**，skipped 记其中「消费了却没进桶」的条数。
	// 两者都只在本轮成功提交时才并进全局（见下方提交段）。
	lines := int64(0)
	skipped := int64(0)
	// unmappedMemo 缓存「模型名 → 是否未映射」。
	//
	// 存在的理由是成本：全量回填要过 ~20 万行，而 `isUnmappedModel` 每次要取
	// configMu/freeModelDocsMu/modelMu 三把读锁并遍历别名表。生产日志里不同
	// 模型名只有 ~18 个，所以记忆化把 20 万次调用压到 18 次。
	// 记忆化**不改变结果**：本函数在单次回填内是纯函数（目录在解析段不会被
	// 改写 —— 唯一的写者是 refreshFreeModelsDocs，它替换整张 map 而不是原地改，
	// 且这里只要求「同输入同输出」，取哪一代快照都自洽）。
	unmappedMemo := map[string]bool{}
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
		// ---- everServed 收集（Task 39）----
		//
		// 「历史上有过一条 status=ok」就足以把这个名字钉成**我们的模型**，
		// 与它今天还在不在目录里无关。注意收集与「这一行进哪张表」是两件事：
		// 一条成功记录自己也会被下面判成 mapped 并进 staged（见 SawUnmapped 分支的翻转）。
		if r.Status == "ok" && r.Model != "" {
			stagedServed[r.Model] = struct{}{}
		}
		// ---- 未映射判定（Task 21 冻结值 + Task 39 everServed 判据）----
		//
		// 两种来源，必须区别对待：
		//  1. `SawUnmapped` 为真：本构建写的行，落盘值就是**请求发生那一刻**
		//     的判定，一律采信。绝不用当前目录重算 —— 目录会漂移，重算会把
		//     「当时确实已映射、只是后来被上游摘掉目录」的模型
		//     （deepseek-v4-flash-free 4,437 次成功 / x-preview-f-free
		//     1,265 次成功）从统计里删掉。
		//     唯一例外：标了未映射但实际 status=ok ⇒ 按已映射算。成功即证明
		//     这个名字可服务，与实时路径 finish 的翻转是同一条规则；本构建
		//     写不出这种行（finish 会先翻转），这里只对异常/过渡期数据兜底。
		//  2. `SawUnmapped` 为假：本构建**之前**写的行，没有该字段，只能重算。
		//     重算分两半：先问**公式 B**（只依赖当前目录，可记忆化），
		//     公式 B 判未映射的**不立即定性**，先塞进 pendingServed 等整轮扫完
		//     再用 everServed 裁决 —— 见 stagedServed 上方那段注释。
		unmapped := r.Unmapped
		if r.SawUnmapped {
			if r.Status == "ok" {
				unmapped = false
			}
		} else {
			v, hit := unmappedMemo[r.Model]
			if !hit {
				v = isUnmappedByFormula(r.Model)
				unmappedMemo[r.Model] = v
			}
			if v {
				pendingServed = append(pendingServed, pendingServedRec{k: k, r: r})
				continue
			}
			unmapped = false
		}
		// 注意：**未映射不走 skipped**。skipped 的语义是「消费了但两张表都没进」
		// （坏行 / ts 解析失败），守恒判据要把它们扣掉；未映射进了
		// stagedUnmapped，属于「进了表」，从判据看与进了 staged 完全等价。
		target := staged
		if unmapped {
			target = stagedUnmapped
		}
		b := target[k]
		if b == nil {
			b = &hourModelAgg{}
			target[k] = b
		}
		b.add(&r)
	}

	// ---- 老日志行的最终裁决：等 stagedServed 收齐后再判（与行序无关）----
	//
	// 判据就是用户 2026-10-20 的口径：**曾经成功过 ⇒ 算我们的模型**。
	// 注意这里查的是「everServed ∪ 本轮 staged」，而不是全局 servedModels ——
	// 全局那份此刻还没合并（合并要等锁内提交），直接查会漏掉本轮才发现的成功名。
	if len(pendingServed) > 0 {
		ever := servedModelsSnapshot(stagedServed)
		for i := range pendingServed {
			p := &pendingServed[i]
			target := stagedUnmapped
			if _, served := ever[p.r.Model]; served {
				target = staged
			}
			b := target[p.k]
			if b == nil {
				b = &hourModelAgg{}
				target[p.k] = b
			}
			b.add(&p.r)
		}
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
	mergeStagedLocked(staged, stagedUnmapped, stagedServed)
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

// callLogFileExists 报告调用日志是否存在。
// 用途：trendsFromCallLog 需要区分「不存在」与「存在但为空」——
// 前者必须返回 nil（handler 转成 []），否则冷进程上热力图会拿到 180 个零值点。
//
// 🔴 daily 模式必须看**段列表**而不是 legacy 单文件：全新部署下 legacy
// call_log.jsonl 永远不会被创建（数据从第一天起就写 call_log/ 日文件），
// 只 Stat legacy 会让 /api/stats/trends 在 L3 布局与全新部署上永远返回 []，
// 且不报错（task-63 D1）。
// 单文件模式保持原语义：只认 legacy 文件。
func callLogFileExists() bool {
	callLog.mu.RLock()
	p, dir := callLog.path, callLog.dir
	callLog.mu.RUnlock()
	if dir != "" {
		// daily / legacy+日文件布局：存在任一段即算存在
		return len(callLog.readSegments()) > 0
	}
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

// satAdd 是 int64 饱和加法：到上限就停在上限，**不回绕**。
//
// 为什么必须：这些字段是裸 `x += v`，且**直接进 json.Marshal**。
// 一条 completion_tokens = MaxInt64 的记录（手改日志 / 另一个写入端就会产出）
// 就能让 CT / SpeedTokSum 回绕成**负数**，而 json 对负数照常序列化 ——
// 面板会**照实显示一个负的 token 数或负的 t/s**，HTTP 200、不进日志、不告警。
// 暴露侧现有的 IsInf/IsNaN 守卫**只挡浮点非有限**，对整型回绕一层也挡不住。
//
// 饱和而不是报错：出问题时「数字停在上限」与「数字变负」相比，
// 前者仍是一个可解释的上界，后者是纯谎报。
func satAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}

// mergeAggTo 把源桶累加进目标桶。字段逐字对齐 hourModelAgg。
func mergeAggTo(dst *hourModelAgg, s *hourModelAgg) {
	dst.N = satAdd(dst.N, s.N)
	dst.OK = satAdd(dst.OK, s.OK)
	dst.Fail = satAdd(dst.Fail, s.Fail)
	dst.PT = satAdd(dst.PT, s.PT)
	dst.CT = satAdd(dst.CT, s.CT)
	dst.CC = satAdd(dst.CC, s.CC)
	dst.CR = satAdd(dst.CR, s.CR)
	dst.SpeedSum += s.SpeedSum
	dst.SpeedN = satAdd(dst.SpeedN, s.SpeedN)
	dst.SpeedSecSum += s.SpeedSecSum
	dst.SpeedTokSum = satAdd(dst.SpeedTokSum, s.SpeedTokSum)
	dst.SpeedTpsN = satAdd(dst.SpeedTpsN, s.SpeedTpsN)
	dst.TTFTSum += s.TTFTSum
	dst.TTFTN = satAdd(dst.TTFTN, s.TTFTN)
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

// foldModelsInWindow 一把读锁、一份键快照，折叠出窗口内按模型切分的全部聚合值。
//
// 🔴 **为什么不能「每个模型折一次」**：旧实现是 sortedModelsInWindow +
// 循环里的 foldOneModel(name)，而 foldOneModel 每次都调 sortedBucketKeysLocked()
// —— **每个模型把全桶键重排一遍**。生产规模 673 桶 / 17 模型 = 17 次排序；
// 稠密上界 73,440 桶 × 17 模型 ≈ 1.25 M 个 hourModelKey（24 字节）
// ≈ 28.7 MB/op、508 ms/op。提排序后实测 ~1.7 MB/op、33 ms/op。
//
// 🔴 **遍历顺序仍必须按 (Hour, Model) 排序**，理由同 sortedBucketKeysLocked：
// map 随机序会让同一份数据两次查询的浮点累加顺序不同。
// 每个模型各自的累加顺序仍是「小时升序」—— 与修复前逐字相同，
// 所以 avg_ttft_ms / avg_output_speed 不发生任何逐位变化。
// 这正是「按 k.Model 分发」而不是「为每个模型各折一遍」必须保留的性质：
// 两种写法下每个模型各自的累加顺序相同，整体结果才逐位相同。
//
// 🔴 **同一份键快照天然消掉一处账目分叉**：修复前 17 个模型在不同时刻各排
// 一次序，落在两次排序之间的新桶会被「后面的模型」算上；若它属于已经折过
// 的那个模型，就两边都不算 —— 那条请求从整张模型表里静默消失。
// 单快照让「窗口内每个桶恰好被计一次」在构造上成立。
//
// model == "" 是合法键，**不要过滤**——过滤会让面板的
// 「新增输入 + Output + 命中 == 真实消耗」这条闭合不变量红。
func foldModelsInWindow(ts []string, bidx func(time.Time) int) ([]string, map[string]*hourModelAgg) {
	callLog.mu.RLock()
	defer callLog.mu.RUnlock()

	aggs := make(map[string]*hourModelAgg, 64)
	for _, k := range sortedBucketKeysLocked() {
		if bucketIndexOf(k.Hour, len(ts), bidx) < 0 {
			continue
		}
		a := aggs[k.Model]
		if a == nil {
			a = &hourModelAgg{}
			aggs[k.Model] = a
		}
		mergeAggTo(a, hourBuckets[k])
	}
	names := make([]string, 0, len(aggs))
	for m := range aggs {
		names = append(names, m)
	}
	// 仍然排序：等价于旧 sortedModelsInWindow「升序返回」的契约。
	// map 遍历顺序随机化；整数求和可交换所以 TotalRequests 不受影响，
	// 但保持升序让这条路径的行为与修复前可逐字对照。
	sort.Strings(names)
	return names, aggs
}

// modelsWithOKInWindow 返回窗口内**至少有一条 status=ok** 的模型名集合（Task 39 / F1）。
//
// 用途：`/api/stats?range=` 的**展示层**只输出这些模型键（`models` 与
// `trends_by_model`）。判据是用户口径「概览的图表啥的只显示有成功的模型」。
//
// 🔴 它**只服务于展示层**，绝不允许被拿去算分母：
// 窗口内 0 成功的模型那几条失败是**真实免费模型的真实失败**（生产实测 6 个模型 /
// 15 条），必须留在 total_requests 与成功率分母里。把它们从分母拿掉就是藏失败。
//
// 🔴 判据直接读 hourBuckets 的 OK 字段 —— 桶里现成的，不新增采集逻辑、不改桶结构。
func modelsWithOKInWindow(ts []string, bidx func(time.Time) int) map[string]struct{} {
	callLog.mu.RLock()
	defer callLog.mu.RUnlock()
	out := make(map[string]struct{}, 16)
	for k, a := range hourBuckets {
		if a.OK == 0 {
			continue
		}
		if bucketIndexOf(k.Hour, len(ts), bidx) < 0 {
			continue
		}
		out[k.Model] = struct{}{}
	}
	return out
}

// filterTrendsByOK 从 ModelTrendPoint 行里删掉「窗口内 0 成功」的模型行（Task 39 / F1）。
//
// 🔴 两个出口共用本函数，别各写一份：`/api/stats?range=` 的 trends_by_model 与
// `/api/stats/trends?group=model` 必须同一口径。只改一个出口就是半实现
// （面板的模型维度图表与 /api/stats 会对不上，且独立验收有专门的变异用例打这个）。
func filterTrendsByOK(pts []ModelTrendPoint, keep map[string]struct{}) []ModelTrendPoint {
	if len(pts) == 0 {
		return pts
	}
	out := pts[:0]
	for _, p := range pts {
		if _, ok := keep[p.Model]; ok {
			out = append(out, p)
		}
	}
	return out
}

// unmappedTopN 是 /api/stats 的 unmapped_requests.by_model 里单个模型的最大条数。
// 其余归并进 "__other__"。定 10 的理由：生产实测这类模型只有 5 个
// （mimo-v2.5 / x-preview-f / claude-opus-4-5 / hy3 / gpt-4o-mini），
// 10 足够覆盖真实长尾；再多只是把一次恶意刷模型名的噪声铺满整个响应体。
const unmappedTopN = 10

// unmappedOtherKey 是 by_model 里「其余模型合并」的保留键。
// 用双下划线包裹是为了**不可能与真实模型名冲突** —— 模型名里不会出现下划线开头
// 的双下划线（我们的命名约定是 `<name>-free`，客户端传来的野名字也不会有），
// 且双下划线让它在面板上明显不像一个模型。
const unmappedOtherKey = "__other__"

// unmappedStatsInWindow 折叠出窗口内的未映射请求统计：总数 + Top N 与其余合并。
//
// 返回**非 nil** 的 map（即使一条都没有），理由与 modelStatsFromCallLog 相同：
// 返回 nil 会序列化成 "by_model":null，前端 `.count` / `.by_model` 取值直接炸。
//
// 窗口口径与 models / total_requests 完全一致 —— 同一个 ts/bidx（由调用方
// statsRangeResponseFor 只取一次窗口后传进来），否则「合计」与「分项」会在
// 跨整点/午夜时互相矛盾（那个坑在 statsRangeResponseFor 的注释里已经踩过一次）。
func unmappedStatsInWindow(ts []string, bidx func(time.Time) int) (count int64, byModel map[string]int64) {
	byModel = map[string]int64{}
	callLog.mu.RLock()
	defer callLog.mu.RUnlock()
	for k, a := range unmappedBuckets {
		if bucketIndexOf(k.Hour, len(ts), bidx) < 0 {
			continue
		}
		count = satAdd(count, a.N)
		byModel[k.Model] = satAdd(byModel[k.Model], a.N)
	}
	return count, byModel
}

// topUnmappedModels 把 byModel 压成「Top N + __other__」。
//
// 排序必须是**全序**：先按条数降序，条数相同再按模型名升序。
// 只按条数排序时，并列的模型在不同 map 遍历顺序下会得到不同的 Top N 边界，
// 于是**同一次查询连发两次**可能得到两个不同的 by_model —— 面板上的数字会
// 莫名其妙地跳。模型名兜底让结果唯一。
func topUnmappedModels(byModel map[string]int64, n int) map[string]int64 {
	out := make(map[string]int64, n+1)
	if len(byModel) <= n {
		for k, v := range byModel {
			out[k] = v
		}
		return out
	}
	type kv struct {
		name string
		cnt  int64
	}
	all := make([]kv, 0, len(byModel))
	for k, v := range byModel {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].cnt != all[j].cnt {
			return all[i].cnt > all[j].cnt
		}
		return all[i].name < all[j].name
	})
	var other int64
	for i, e := range all {
		if i < n {
			out[e.name] = e.cnt
			continue
		}
		other = satAdd(other, e.cnt)
	}
	if other > 0 {
		out[unmappedOtherKey] = other
	}
	return out
}

// ---- 全局环形缓冲 + JSONL 落盘 ----

type callLogStore struct {
	// RWMutex 而非 Mutex：纯读路径（latest / loadCallLogFromFile /
	// 读取用 RLock，写路径（append/clear）用 Lock。
	// Task 8 起查询侧读 hourBuckets 也走 RLock。
	mu      sync.RWMutex
	records []CallRecord
	path    string // legacy 文件（dir=="" 时是唯一文件）
	// dir 非空 ⇒ daily 模式：新记录按天落盘到 dir/YYYY-MM-DD.jsonl，
	// path 只作为「legacy 冻结段」。dir=="" ⇒ 单文件模式（既有语义逐字节不变）。
	dir string
}

var callLog = &callLogStore{}

func initCallLog(configPath string) {
	base := filepath.Dir(configPath)
	callLog.mu.Lock()
	callLog.path = filepath.Join(base, callLogFileName)
	callLog.dir = ""
	if callLogLayout != "single" {
		dir := filepath.Join(base, callLogDirName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			// 只读卷/ENOSPC：降级为单文件模式（与旧二进制语义相同），保证服务可用。
			slog.Warn("calllog: daily layout disabled, falling back to single file",
				"dir", dir, "error", err)
		} else {
			callLog.dir = dir
		}
	}
	dailySegs, dailySegsKey, dailyCur = nil, "", absorbCursor{}
	dailySegsDirMod = time.Time{}
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
	dir := callLog.dir
	callLog.mu.RUnlock()
	if dir != "" {
		loadCallLogFromFileDaily()
		return
	}
	loadCallLogFromFileSingle()
}

// installLoadedRecords 把尾部记录装进内存环并播种 everServed（单文件/daily 共用）。
func installLoadedRecords(records []CallRecord) {
	if len(records) > callLogCapacity {
		records = records[len(records)-callLogCapacity:]
	}
	for i := range records {
		restoreDiskRecord(&records[i])
	}
	callLog.mu.Lock()
	callLog.records = records
	if servedModels == nil {
		servedModels = make(map[string]struct{}, 32)
	}
	for _, rec := range records {
		if rec.Status == "ok" && rec.Model != "" {
			servedModels[rec.Model] = struct{}{}
		}
	}
	callLog.mu.Unlock()
}

// loadCallLogFromFileDaily 跨段倒读最后 callLogCapacity 条：
// 从最新的一段开始，一段不够再往前一段，直到凑够或段用尽。
func loadCallLogFromFileDaily() {
	segs := callLog.dailySegments()
	if len(segs) == 0 {
		return
	}
	need := callLogCapacity
	var chunks [][][]byte // 每块升序，块序 = 新→旧
	for i := len(segs) - 1; i >= 0 && need > 0; i-- {
		// 🔴 明文段必须走 tailReadLastLines：legacy 段可能是一个 124 MB 的大文件，
		// 为拿最后 2000 条而读全文件会让冷启动多花 2~3 秒（实测 3.65s vs 0.98s）。
		var lines [][]byte
		if !segs[i].gz {
			raw, err := tailReadLastLines(segs[i].path, need)
			if err != nil {
				continue
			}
			lines = splitJSONLines(raw)
		} else {
			data, _, err := readDailySegment(segs[i].path, true)
			if err != nil {
				continue
			}
			lines = splitJSONLines(data)
		}
		if len(lines) == 0 {
			continue
		}
		take := need
		if take > len(lines) {
			take = len(lines)
		}
		chunks = append(chunks, lines[len(lines)-take:])
		need -= take
	}
	records := make([]CallRecord, 0, callLogCapacity)
	for c := len(chunks) - 1; c >= 0; c-- {
		for _, line := range chunks[c] {
			var rec CallRecord
			if json.Unmarshal(line, &rec) == nil && rec.ReqID != "" {
				records = append(records, rec)
			}
		}
	}
	installLoadedRecords(records)
}

// splitJSONLines 把原始字节切成非空行（保留首尾空白行剔除，行内不 trim）。
func splitJSONLines(data []byte) [][]byte {
	out := make([][]byte, 0, bytes.Count(data, []byte{'\n'})+1)
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		var line []byte
		if i < 0 {
			line, data = data, nil
		} else {
			line, data = data[:i], data[i+1:]
		}
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			out = append(out, line)
		}
	}
	return out
}

// loadCallLogFromFileSingle 是原有的单文件尾部加载（语义逐字保留）。
func loadCallLogFromFileSingle() {
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
	// 顺手从尾部记录里播种 everServed（Task 39）。
	//
	// 为什么要在启动时就播种、而不是等第一次 stats 查询触发回填：
	// 回填是**惰性**的（只有 /api/stats 等查询路径会调 ensureBucketsUpToDate），
	// 而判定发生在请求路径上（beginCallLog）。若中间隔了很久没人开面板，
	// 这段时间里「历史上成功过、但今天不在目录里」的模型会被按公式 B 判成未映射，
	// 重启后的判定就和重启前不一致（违反单调性）。尾部记录是**已经在内存里**的
	// 现成数据，零额外 IO，覆盖「最近有流量」的全部模型名 —— 这是最划算的一层保险。
	// 更久远的历史由首次回填补齐（同一份日志、同一套判据）。
	installLoadedRecords(records)
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

// diskRecordForWrite 生成落盘副本：先裁 events（原有逻辑），再按 task-56 精简默认/零值字段。
// 只在 daily 模式启用 —— `-calllog-layout=single` 是回滚模式，连落盘格式也退回旧版，
// 这样旧二进制读新文件、新二进制写旧格式两个方向都不会出意外。
//
// 只改落盘副本，内存环与无参数 /api/call-log 响应保持完整字段（老行不受影响）。
func diskRecordForWrite(rec CallRecord) CallRecord {
	d := rec
	if !d.HasIssue() {
		d.Events = nil
	}
	if d.Path == defaultCallPath {
		d.Path = ""
	}
	if d.RouteMode == defaultRouteMode {
		d.RouteMode = ""
	}
	if len(d.Events) > 0 {
		// 🔴 必须**深拷贝**再清：d.Events 与内存环里的 rec.Events 共用底层数组，
		// 直接改 d.Events[i] 会把内存环也污染掉（实测：append 后 /api/call-log
		// 的事件名变空，面板时间线坏掉）。
		evs := make([]CallEvent, len(d.Events))
		copy(evs, d.Events)
		for i := range evs {
			// node_name 100% 可由顶层 nodes/node_names 还原（生产实测 233,750/233,750）
			evs[i].NodeName = ""
		}
		d.Events = evs
	}
	// 零读者的两个字段（审计 §3.3：全仓库仅写入 1 处、无任何读取点）
	d.FinalStatus = 0
	d.DialMS = 0
	return d
}

// restoreDiskRecord 把落盘精简掉的默认值/事件名还原回来。
// 用于：启动时从磁盘恢复内存环（loadCallLogFromFile）、历史查询结果。
// 老行（字段齐全）经过它是 no-op，因此不需要区分版本。
func restoreDiskRecord(rec *CallRecord) {
	if rec.Path == "" {
		rec.Path = defaultCallPath
	}
	if rec.RouteMode == "" {
		rec.RouteMode = defaultRouteMode
	}
	for i := range rec.Events {
		if rec.Events[i].NodeName != "" || rec.Events[i].Node == "" {
			continue
		}
		for j, fp := range rec.Nodes {
			if fp == rec.Events[i].Node && j < len(rec.NodeNames) {
				rec.Events[i].NodeName = rec.NodeNames[j]
				break
			}
		}
	}
}

func (s *callLogStore) append(rec CallRecord) {
	s.mu.Lock()
	s.records = append(s.records, rec)
	if len(s.records) > callLogCapacity {
		s.records = s.records[len(s.records)-callLogCapacity:]
	}
	// daily 模式：写入今天的天文件（保证只追加到最后一段，绝不回写旧段）。
	// p == "" 表示日目录建不出来（只读卷/ENOSPC）：该条不落盘，仍在内存环里。
	p := s.dailyTargetLocked()
	// ---- 落盘裁 Events：只裁「干净」记录 ----
	//
	// Events 占落盘体积的大头（b3555de 实测 95.7 MB → 58.6 MB，−38.7%），
	// 其中 73.1% 是 detail 恒为空的 connect_ok，信息已被同记录的
	// nodes[0]/node_names[0] 完全覆盖。但**无条件**裁掉是错的：恢复记录
	// 的用户在重启后会看到日志页空掉一大块——徽章（clHasIssue）、标签文字
	// （clIssueLabel）、事件时间线、以及时段表和节点表两列「异常/切换」
	// 全都读 events。面板默认 ?limit=500，实测该窗口内 p50 只有 19 条、
	// p90 有 146 条记录带 events，所以这不是「偶尔丢一条」而是日常可见。
	//
	// 判据用 CallRecord.HasIssue()：它的类型集合与 static/admin.html 的
	// CL_ISSUE_TYPES 逐项相同，与 clHasIssue 同判。88.84% 的记录
	// HasIssue 为假，对它们 events 在 UI 上永远不会被显示一行，
	// 落盘是纯浪费。
	//
	// ⚠️ **修不回历史**：call_log.jsonl 无轮转机制，磁盘上已有的存量
	// （实测 95.7 MB，约 7 周）本来就是当前构建写的，从来就没有 events。
	// 这条只对部署后新写入的记录生效。
	//
	// ⚠️ **体积数字要按实测报，别沿用 −38.7% 那个标题数字**：
	// 生产 100.4 MB 完整日志上实测，无条件裁 58.6 MB（−38.7%），
	// 本方案 72.5~78.3 MB（−22% ~ −24%）——保住约 57%~63% 的原收益。
	// 而 −38.7% 那个数字落到实际只有 5.9 ms 启动 + 37 MB 磁盘：查询早已
	// 不读文件（Task 7 改成读小时桶），所以省下的字节对查询零影响。
	//
	// ⚠️ **本方案依赖的是前端约定，不是后端不变量**：
	//   - 前端保证只对 clHasIssue 为真的记录渲染时间线（admin.html 的
	//     `if(issue&&exp)` 与 `const chev=issue?(exp?'▾':'▸'):''`）。
	//     **若将来前端要给成功记录也加展开视图，这一条必须重估。**
	//   - /api/call-log 对「干净记录」不再返回 events。本项目 UI 无感知
	//     （前端一律 `(r.events||[])`），但若有第三方消费者读这个接口，
	//     它会看到字段消失。
	//
	// Marshal 必须在锁内、且紧邻落盘：line 的长度就是 parsedTo 的增量，
	// 两者必须是同一份字节，否则推进量与真正写下去的量会对不上。
	diskRec := rec
	if s.dir != "" {
		diskRec = diskRecordForWrite(rec)
	} else if !diskRec.HasIssue() {
		diskRec.Events = nil
	}
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
			slim := &slimRec{
				TS: rec.TS, Model: rec.Model, Status: rec.Status,
				PromptTokens: rec.PromptTokens, CompletionTokens: rec.CompletionTokens,
				CacheCreation: rec.CacheCreation, CacheRead: rec.CacheRead,
				TTFTMs: rec.TTFTMs, OutputSpeed: rec.OutputSpeed,
				DurationMS: rec.DurationMS, Unmapped: rec.Unmapped,
			}
			// 未映射请求进**平行表**，不进 hourBuckets。
			//
			// 🔴 关键：它仍必须走下面「推进 parsedTo + absorbedRecords」那一段。
			// 未映射只改变「进哪张表」，**不改变「字节被消费了」这个事实**。
			// 漏掉推进 ⇒ 下次 backfill 会把这条再扫一遍（重复计数、单调发散）；
			// 漏掉 absorbedRecords ⇒ 守恒判据差 1 ⇒ 每次查询触发一次全量重扫。
			// 守恒式两表之和见 checkBucketConservation。
			if rec.Unmapped {
				if unmappedBuckets == nil {
					unmappedBuckets = make(map[hourModelKey]*hourModelAgg)
				}
				uagg := unmappedBuckets[k]
				if uagg == nil {
					uagg = &hourModelAgg{}
					unmappedBuckets[k] = uagg
				}
				// 只累计 N（unmapped 请求全是失败，token/速度/吞吐都无意义，
				// 且它们**不得**进任何展示成功率的分母）。用 add 是为了复用
				// 同一个类型，OK/Fail 也如实记下来，便于诊断。
				uagg.add(slim)
			} else {
				if hourBuckets == nil {
					hourBuckets = make(map[hourModelKey]*hourModelAgg)
				}
				agg := hourBuckets[k]
				if agg == nil {
					agg = &hourModelAgg{}
					hourBuckets[k] = agg
				}
				agg.add(slim)
			}
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
			if s.dir != "" {
				// daily 模式：游标指向最后一段末尾（与 parsedTo == size 同构）
				if n := len(dailySegs); n > 0 && dailySegs[n-1].path == p {
					dailyCur = absorbCursor{idx: n - 1, off: dailyCur.off + int64(len(line))}
					dailySegs[n-1].size += int64(len(line))
				}
			} else {
				parsedTo += int64(len(line))
			}
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
	// everServed 也必须一起清（Task 39）：它是「这份日志历史里成功过的模型名」，
	// 日志被用户显式清空 = 历史归零 ⇒ 集合也得归零，否则重启后新日志里
	// 一个从没成功过的名字会因为上一轮的历史被当成「我们的模型」而计入概览。
	// 注意这与「单调性」不冲突：单调性说的是「同一份历史内只增不减」，
	// 而 clear 是用户显式要求丢弃整段历史。
	servedModels = nil
	// os.Remove 必须在同一临界区内。若放在锁外，其后若有查询进来，
	// 会从 parsedTo=0 开始把刚被删掉的整个文件重新吸进桶，
	// 被清空的趋势图立刻复活（幽灵统计）。
	if p != "" {
		os.Remove(p)
	}
	// daily 模式：日文件也必须一起删（用户显式清空 = 丢弃全部历史）。
	if s.dir != "" {
		if ents, err := os.ReadDir(s.dir); err == nil {
			for _, e := range ents {
				if e.IsDir() {
					continue
				}
				if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil {
					slog.Warn("calllog: cannot remove day segment on clear", "file", e.Name(), "error", err)
				}
			}
		}
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

// unmappedWarnInterval 是「未映射模型」WARN 的**每模型**最小间隔。
const unmappedWarnInterval = 5 * time.Minute

// unmappedWarnMaxModels 是限流表的条目上限。客户端可以随意编模型名，
// 不设上限的话这张 map 会被野名字撑成内存泄漏。
const unmappedWarnMaxModels = 64

var (
	unmappedWarnMu   sync.Mutex
	unmappedWarnLast = map[string]time.Time{}
)

// warnUnmappedModel 对「模型名无法映射」发一条 WARN，**按模型限流**。
//
// 为什么必须打这条日志：这类请求被排除出统计聚合后，就只剩 call_log.jsonl
// 里一个 `"unmapped":true` 字段可见。运维若不主动去翻日志，会以为请求凭空
// 消失了。任务书要求「不静默丢弃」，日志是这条要求的落点之一。
//
// 为什么限流：未映射是**客户端输入**决定的，一个写错模型名的客户端会把它
// 变成每请求一条 WARN。按模型 5 分钟一条，既能让人看见，又不会淹掉日志。
//
// 为什么在锁外：调用点 beginCallLog 不持有任何锁。绝不能在 callLog.mu 内
// 做同步 I/O —— 那会连带阻塞所有在途 append 与面板查询。
func warnUnmappedModel(model string) {
	unmappedWarnMu.Lock()
	if unmappedWarnLast == nil {
		unmappedWarnLast = map[string]time.Time{}
	}
	if last, ok := unmappedWarnLast[model]; ok && time.Since(last) < unmappedWarnInterval {
		unmappedWarnMu.Unlock()
		return
	}
	if len(unmappedWarnLast) >= unmappedWarnMaxModels {
		// 简单粗暴地整体清空：限流表只是「别刷屏」，丢掉历史不影响正确性，
		// 而逐个淘汰需要维护 LRU，不值得。
		unmappedWarnLast = map[string]time.Time{}
	}
	unmappedWarnLast[model] = time.Now()
	unmappedWarnMu.Unlock()

	slog.Warn("calllog: request model cannot be mapped to a served model; "+
		"excluded from stats aggregation but still written to call_log",
		"model", model)
}

// beginCallLog 创建本次请求的调用记录并挂到 ctx；返回新 ctx。
//
// 未映射判定在这里做（**请求发生那一刻**），不是聚合时现算：聚合侧持
// callLog.mu，现算要再取 configMu/freeModelDocsMu/modelMu，凭空造出一条
// callLog.mu → 其它锁 的锁序；而且目录会漂移，现算会让同一条历史记录在不同
// 日期得到不同结论，桶内容非确定、还会误伤守恒判据。详见 CallRecord.Unmapped。
func beginCallLog(ctx context.Context, path, model string, stream bool, routeMode string) context.Context {
	rec := CallRecord{
		ReqID:     getReqID(ctx),
		TS:        time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
		Path:      path,
		Model:     model,
		Stream:    stream,
		RouteMode: routeMode,
		Status:    "fail",
		Unmapped:  isUnmappedModel(model),
	}
	if rec.ReqID == "" {
		rec.ReqID = "req_" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if rec.Unmapped {
		// 锁外、且**先于**返回：日志不能拖慢请求路径，但也不能丢。
		warnUnmappedModel(model)
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
		// Task 39：成功即证明这个名字**可服务**。若请求开始时它还「从未成功过」
		// 而被判成未映射，必须在落盘与进桶之前翻转 —— 否则一条真实成功记录
		// 会被排除在概览之外，违反用户口径「概览显示有成功的模型」。
		cr.rec.Unmapped = false
	} else {
		cr.rec.Status = "fail"
	}
	// final_status 与 main.go 里 log 的 "final_status" 同源：就是传给 finish 的这个
	// 最终状态码。落盘是为了让面板/外部能直接看到「上游到底返回了什么」，
	// 而不是只靠 status 的 ok/fail 二值去反推。
	cr.rec.FinalStatus = status
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
	// 计算首 token 延迟和输出速度。
	// ★ 判据是 firstTokenAt，**不再要求 cr.rec.Stream**：
	// 上游对非流式客户端同样是 stream=true（ensureAgentUpstreamShape 强制），
	// 其 SSE 由 readAllMarkingFirstData 逐行读取时打点。因此「非流式」不等于
	// 「没有首 token」—— 旧判据把绝大多数非流式请求永久挡在速度统计之外，
	// 而它们占本项目 90.4% 的 output token。
	// 反过来保证：上游若没返回 SSE（错误体、纯 JSON），firstTokenAt 恒为零 ⇒ 不出速度，
	// 面板显示 "—"，与之前完全一致。
	if !cr.firstTokenAt.IsZero() && cr.completionTok > 0 {
		cr.rec.TTFTMs = cr.firstTokenAt.Sub(cr.start).Milliseconds()
		genSec := time.Since(cr.firstTokenAt).Seconds()
		if genSec > 0 {
			cr.rec.OutputSpeed = float64(cr.completionTok) / genSec
			recordModelSpeed(cr.rec.Model, cr.rec.TTFTMs, cr.rec.OutputSpeed)
		}
	}
	rec := cr.rec
	cr.mu.Unlock()
	// Task 39：成功过的模型名进 everServed（只增不减）。
	//
	// 为什么在 append 之前记账：记账与落盘之间若崩掉，重放日志的回填也能补上；
	// 反过来先落盘后记账，则下一条同名请求会重新被判「从未成功过」⇒ 未映射，
	// 那一刻的判定就被冻结进日志，概览会少算它。所以宁可早记。
	if rec.Status == "ok" {
		markServedModel(rec.Model)
	}
	callLog.append(rec)
}

// observeDialMs 记录本次尝试的建连耗时（httptrace.GotConn 触发时调用）。
// 只保留**第一次**成功的 GotConn：重试循环里每次尝试都可能建连，
// 但 dial_ms 表达的是「这次请求建立连接花了多久」，取首个即最贴近直觉的值。
func (cr *callRecorder) observeDialMs(ms int64) {
	if cr == nil || ms < 0 {
		return
	}
	cr.mu.Lock()
	if cr.rec.DialMS == 0 {
		cr.rec.DialMS = ms
	}
	cr.mu.Unlock()
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

// withDialTrace 给上游请求挂一个 httptrace，把**建连耗时**（GotConn 时刻 − 请求开始）
// 记进本次调用记录。没有它，成功侧的建连耗时永远观测不到 —— 面板与日志里只有
// duration_ms（整请求）和失败侧的 connect_error，没有成功侧的 dial_ms。
//
// ★ 为什么用 GotConn 而不是自己 time.Since(client.Do)：
// client.Do 返回时【响应头已经读完】，那不是建连；GotConn 才是 transport 真正
// 拿到一条连接（新建或复用）的时刻。
// ★ 复用连接时 GotConn 也触发，值是毫秒级小值 —— 这正好让面板能区分
// 「新建连慢」与「命中复用」，正是上次定超时最缺的那个维度。
// 复用同一 req 的 ctx 时不会互相覆盖（WithContext 返回新 Request）。
func withDialTrace(ctx context.Context, req *http.Request) *http.Request {
	cr := callRecorderFrom(ctx)
	if cr == nil {
		return req
	}
	start := time.Now()
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			cr.observeDialMs(time.Since(start).Milliseconds())
		},
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
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

// （已删：forEachBucketedCallRecord，0 调用者。
//  它 os.ReadFile 整个日志文件、用 encoding/json 解析、没有 .In(time.Local)
//  （490 条 Z 行的老 bug）且不经过任何吞吐守卫 —— 留着是上膛的枪。）
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
	// foldModelsInWindow 要读桶，而冷启动时桶是空的
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

	// 顶层吞吐的分子分母：与 per-model 同源（同一批桶、同一份 SpeedTokSum/SpeedSecSum），
	// 所以顶层恒等于「按秒加权的各模型吞吐」。除法放在循环后做一次，
	// 而不是 Σ(每模型 tps×秒)/Σ秒 —— 后者会把已被守卫挡掉的桶算成 0 参与加权。
	var gSec float64
	var gTok int64
	var gN int64

	names, aggs := foldModelsInWindow(ts, bidx)
	for _, name := range names {
		a := aggs[name]
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

		// ---- 吞吐三字段（DSH 同口径：Σ完成token ÷ Σ生成秒）----
		//
		// 暴露侧的守卫与 add() 的写侧守卫**同判据、不是重复**：
		//  写侧保证脏样本根本进不了桶；暴露侧保证「即便进来了也整桶不出数」。
		// 缺暴露侧的实测后果（陷阱②）：一条 312 B 的脏记录就让整个模型桶 +
		// 全局吞吐**静默消失**、HTTP 200 不报错。
		// ⇒ 双层都要，两层都不能省。
		//
		// 🔴 赋值必须在守卫里，不能只守除法那一步：若先把 SpeedSecSum(=+Inf)
		// 赋给 ms 再判 Inf，json.Marshal 遇 +Inf 会让整份响应变 500
		// —— 那正是这道防线要挡的形态。判据相同 ⇒ 非有限时**整桶的吞吐字段一起缺席**。
		if a.SpeedTpsN > 0 && !math.IsInf(a.SpeedSecSum, 0) && !math.IsNaN(a.SpeedSecSum) {
			ms.StreamTpsN = a.SpeedTpsN
			if v := float64(a.SpeedTokSum) / a.SpeedSecSum; !math.IsInf(v, 0) && !math.IsNaN(v) {
				ms.ThroughputTPS = v
			}
		}
		gSec += a.SpeedSecSum
		gTok += a.SpeedTokSum
		gN += a.SpeedTpsN

		out.Models[name] = ms
		// 🔴 total_requests 与 models 取自**同一个 a**，不是再独立折叠一遍。
		// 两者数值恒等（都是窗口内按模型切分的同一批桶的请求数之和），
		// 但独立折叠要多一趟全桶扫描，且中间还夹着一次加解锁。
		// 注意：恒等是**构造上**的——实测 3,000 次并发查询下从未出现
		// 「Σmodel.request_count ≠ total_requests」。真正的缺陷在别处：
		// 旧实现给每个模型各排一次全桶键，落在两次排序之间到达的新请求
		// 会被后面的模型算上、前面的不算，于是**同一份响应里各模型的数字
		// 取自不同时刻**（漏掉的那条请求从两个字段里一起消失，字段仍相等）。
		// 提排序修掉的正是这个：从同一个 a 累加、并共用一份键快照。
		out.TotalRequests += a.N
	}

	// ---- 顶层吞吐：与 per-model 同一套守卫 ----
	// 0 样本时三个字段**全部缺席**（omitempty），不是 0、不是 null：
	// 「窗口内没有可计算吞吐的流式采样」与「吞吐是 0」是两回事，
	// 面板据此显示 "—" 而不是 0 t/s。
	if gN > 0 && !math.IsInf(gSec, 0) && !math.IsNaN(gSec) {
		out.StreamTpsN = gN
		if v := float64(gTok) / gSec; !math.IsInf(v, 0) && !math.IsNaN(v) {
			out.ThroughputTPS = v
		}
	}
	return out
}

// ---- API：GET /api/call-log?limit= / DELETE /api/call-log ----

// ---- task-56：历史查询（无索引，按段倒读）----

// callLogHistoryFilter 是历史查询的过滤条件；hasBefore/hasAfter 区分零值与未设置。
type callLogHistoryFilter struct {
	limit       int
	hasBefore   bool
	before      time.Time
	beforeReqID string // 分页去重游标：(ts, req_id) 严格序
	hasAfter    bool
	after       time.Time
	model       string
	status      string // ""/"all"/"ok"/"fail"
	q           string // 对原始行做大小写不敏感子串匹配

	// 位置游标（task-65 D3）：直接指定「从哪一段的哪一行之后续扫」。
	// 与 ts 是否有序无关——文件按完成序追加、ts 是开始时刻，两者本来就不一致。
	hasCursor  bool
	cursorSeg  string // "legacy" 或 "YYYY-MM-DD"（段标识，不用文件路径，明文→gz 改名后仍有效）
	cursorLine int    // 段内物理行号（0 基），= 上一页最后返回行的行号
}

// ---- 位置游标（task-65 D3）----
//
// 生产日志按「请求完成序」追加，而 CallRecord.TS 是「请求开始时刻」：
// 流式长请求先开始、后结束 ⇒ 相邻行的 ts 经常逆序（实测 29.3%）。
// 因此 ts 不能做分页游标（下一页跳过 ts>before 会永久漏掉扫描序里更晚出现的
// 更大 ts 行；反过来重复吐 ts 更小的行）。位置游标只记「扫到哪」，与 ts 无关：
//
//	X-Call-Log-Next-Cursor = "c1_" + base64url(段标识 + ":" + 行号)
//
// 段标识用 legacy / YYYY-MM-DD（不是文件路径）：日文件明文→gz 改名后游标仍有效。
// 后续页从该行的**下一条物理行**（行号-1，倒序扫描）续扫，不再从最新段重扫。
const callLogCursorPrefix = "c1_"

// errCallLogCursorStale 表示游标指向的段已不存在或行号越界（段被保留策略删除、
// 数据目录被替换等）。handler 把它转成 400 + X-Call-Log-Error: stale-cursor，
// 由调用方从头重新分页——不静默回退，避免返回看似成功但缺页的结果。
var errCallLogCursorStale = errors.New("calllog: history cursor stale (segment missing or line out of range)")

type callLogCursor struct {
	Seg  string
	Line int
}

// callLogSegKey 返回段的稳定标识：legacy 段用 "legacy"，日文件用日期（YYYY-MM-DD）。
func callLogSegKey(seg callLogSegment) string {
	if seg.legacy {
		return "legacy"
	}
	if seg.day != "" {
		return seg.day
	}
	return strings.TrimSuffix(filepath.Base(seg.path), ".gz")
}

func encodeCallLogCursor(seg string, line int) string {
	raw := seg + ":" + strconv.Itoa(line)
	return callLogCursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCallLogCursor(v string) (callLogCursor, bool) {
	if !strings.HasPrefix(v, callLogCursorPrefix) {
		return callLogCursor{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(v[len(callLogCursorPrefix):])
	if err != nil {
		return callLogCursor{}, false
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 || parts[0] == "" {
		return callLogCursor{}, false
	}
	line, err := strconv.Atoi(parts[1])
	if err != nil || line < 0 {
		return callLogCursor{}, false
	}
	return callLogCursor{Seg: parts[0], Line: line}, true
}

// readSegments 返回查询用的段列表（升序）；单文件模式返回单段。
func (s *callLogStore) readSegments() []callLogSegment {
	if segs := s.dailySegments(); len(segs) > 0 {
		return segs
	}
	s.mu.RLock()
	p := s.path
	s.mu.RUnlock()
	if p == "" {
		return nil
	}
	if st, err := os.Stat(p); err == nil {
		return []callLogSegment{{name: filepath.Base(p), path: p, legacy: true, size: st.Size()}}
	}
	return nil
}

// parseLogFilterTime 解析 before/after 参数；兼容带/不带毫秒与时区的写法。
func parseLogFilterTime(v string) (time.Time, error) {
	if tt, err := time.Parse(time.RFC3339, v); err == nil {
		return tt, nil
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if tt, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return tt, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time %q", v)
}

// historyQuery 从文件侧查历史（最新在前），返回 (记录, 是否还有更早的匹配, 是否被扫描预算截断)。
//
// 无索引：按段从新到旧、段内从尾到头倒读；命中行才做完整 Unmarshal。
// 成本实测（审计 §4.3）：单日倒读 + limit=2000 命中即停 25~32 ms；
// 尾部 2000 行 179 µs~1.95 ms；90 天最坏 ~0.7 s（有 256 MiB 预算兜底）。
func (s *callLogStore) historyQuery(f callLogHistoryFilter) ([]CallRecord, bool, bool, string, error) {
	if f.limit <= 0 {
		f.limit = 200
	}
	if f.limit > callLogCapacity {
		f.limit = callLogCapacity
	}
	if f.hasCursor {
		// 位置游标本身已经精确指定了扫描起点，与 ts 无关；
		// before/before_req_id 的 ts 边界语义在 cursor 模式下没有意义（也不该再用），
		// 明确忽略，避免「位置续扫 + 旧 ts 跳过」叠加造成漏行。
		f.hasBefore = false
	}
	qLower := strings.ToLower(f.q)
	segs := s.readSegments()
	out := make([]CallRecord, 0, f.limit)
	var scanned int64
	truncated := false
	hasMore := false

	// histHit 记住每条命中行的**物理位置**（段下标 + 段内行号），
	// 用于在页尾产出位置游标（X-Call-Log-Next-Cursor）。
	type histHit struct {
		rec  CallRecord
		seg  int
		line int
	}
	lastSeg, lastLine := -1, -1
	// emit 输出一条并维护 hasMore；返回 true 表示已到 limit、应停止扫描。
	emit := func(h histHit) bool {
		if len(out) >= f.limit {
			hasMore = true
			return true
		}
		out = append(out, h.rec)
		lastSeg, lastLine = h.seg, h.line
		return false
	}

	// tsBuf 暂存「ts == before」这一组的候选行（含物理位置）。
	//
	// 🔴 为什么必须整组缓冲而不能边扫边出：同 ts 行的物理顺序与 req_id 大小无关
	// （生产快照 52% 的同 ts 组不是 req_id 升序），`before_req_id` 只能当
	// **扫描位置游标**：组内游标行之前的行都已在上页返回过，游标行之后的还没返回。
	// 只有把整组读完、找到游标行在组内的位置，才能正确接续（task-63 D2）。
	//
	// 兼容回退：客户端也可能传一个**不在数据里**的 before_req_id（旧实现把它当
	// 排序键，返回 req_id < 游标的同 ts 行）。组结束时找不到游标行就退回旧语义，
	// 保证老调用方行为不变。
	var tsBuf []histHit
	flushTSBuf := func() bool {
		if len(tsBuf) == 0 {
			return false
		}
		buf := tsBuf
		tsBuf = nil
		pos := -1
		for k := range buf {
			if buf[k].rec.ReqID == f.beforeReqID {
				pos = k
				break
			}
		}
		if pos >= 0 {
			for _, h := range buf[pos+1:] {
				if emit(h) {
					return true
				}
			}
			return false
		}
		// 游标行不在本组：按旧实现的排序键语义过滤（req_id < cursor 才返回）
		for _, h := range buf {
			if h.rec.ReqID < f.beforeReqID {
				if emit(h) {
					return true
				}
			}
		}
		return false
	}

	// 位置游标：从「上一页最后一条」的下一条物理行续扫（倒序扫描 ⇒ 行号 -1）。
	startIdx, cursorLine := len(segs)-1, -1
	if f.hasCursor {
		startIdx = -1
		for i := range segs {
			if callLogSegKey(segs[i]) == f.cursorSeg {
				startIdx = i
				break
			}
		}
		if startIdx < 0 {
			// 段被保留策略删除 / 数据目录换过：明确失败，不静默回退（见 handler 400）
			return nil, false, false, "", errCallLogCursorStale
		}
		cursorLine = f.cursorLine
	}

scan:
	for i := startIdx; i >= 0; i-- {
		seg := segs[i]
		data, size, err := readDailySegment(seg.path, seg.gz)
		if err != nil {
			if os.IsNotExist(err) {
				continue // 段被归档/删除：跳过（列表会自愈）
			}
			slog.Warn("calllog: history query cannot read segment", "path", seg.path, "error", err)
			continue
		}
		scanned += size
		if scanned > callLogHistoryMaxScanBytes {
			truncated = true
			break
		}
		lines := splitJSONLines(data)
		jFrom := len(lines) - 1
		if f.hasCursor && i == startIdx {
			if len(lines) == 0 || cursorLine >= len(lines) {
				return nil, false, false, "", errCallLogCursorStale
			}
			jFrom = cursorLine - 1
		}
		for j := jFrom; j >= 0; j-- {
			line := lines[j]
			if qLower != "" && !bytes.Contains(bytes.ToLower(line), []byte(qLower)) {
				continue
			}
			var slim slimRec
			if !parseStatsLine(line, &slim) {
				continue
			}
			if f.model != "" && slim.Model != f.model {
				continue
			}
			switch f.status {
			case "", "all":
			case "ok":
				if slim.Status != "ok" {
					continue
				}
			case "fail":
				if slim.Status == "ok" {
					continue
				}
			}
			ts, perr := parseLogFilterTime(slim.TS)
			if perr != nil {
				continue
			}
			if f.hasAfter && ts.Before(f.after) {
				continue
			}
			if f.hasBefore {
				if ts.After(f.before) {
					continue
				}
				if ts.Equal(f.before) {
					if f.beforeReqID == "" {
						continue // 排他：整组都不返回
					}
					var rec CallRecord
					if json.Unmarshal(line, &rec) != nil || rec.ReqID == "" {
						continue
					}
					restoreDiskRecord(&rec)
					tsBuf = append(tsBuf, histHit{rec: rec, seg: i, line: j})
					continue
				}
				// ts 已经严格早于 before ⇒ 同 ts 组结束，按游标语义补出缓冲行
				if len(tsBuf) > 0 && flushTSBuf() {
					break scan
				}
			}
			var rec CallRecord
			if json.Unmarshal(line, &rec) != nil || rec.ReqID == "" {
				continue
			}
			restoreDiskRecord(&rec)
			if emit(histHit{rec: rec, seg: i, line: j}) {
				break scan
			}
		}
		if hasMore || truncated {
			break
		}
	}
	if !hasMore && !truncated {
		flushTSBuf()
	}

	nextCursor := ""
	if hasMore && lastSeg >= 0 {
		nextCursor = encodeCallLogCursor(callLogSegKey(segs[lastSeg]), lastLine)
	}
	return out, hasMore, truncated, nextCursor, nil
}

// ---- task-67：分析页聚合接口 /api/logs/aggregate ----
//
// 分析页 6 个 Panel 的数据源。**口径红线**：totals / timeseries 必须与
// /api/stats、/api/stats/trends 同源同值（2b 口径不改）。
// 延迟分位、attempts、phases、by_node、by_error 需要逐记录信息，桶里没有，
// 因此由一次磁盘扫描填充（TTL 缓存 + singleflight，见 log_aggregate 段）。

// aggregateResponse 是 GET /api/logs/aggregate 的响应体（字段语义见
// docs/research/log-aggregate-api.md）。
type aggregateResponse struct {
	Range       string        `json:"range"`
	Window      aggWindow     `json:"window"`
	GeneratedAt string        `json:"generated_at"`
	Totals      aggTotals     `json:"totals"`
	Latency     aggLatency    `json:"latency"`
	Timeseries  []aggPoint    `json:"timeseries"`
	ByModel     []aggModelRow `json:"by_model"`
	ByNode      []aggNodeRow  `json:"by_node"`
	ByError     []aggErrorRow `json:"by_error"`
	Attempts    aggAttempts   `json:"attempts"`
	Phases      aggPhases     `json:"phases"`
	Unmapped    aggUnmapped   `json:"unmapped"`
	NodeAggBy   string        `json:"node_agg_by"` // "node_name"：by_node 按节点显示名聚合
}

type aggWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type aggTotals struct {
	Requests    int64   `json:"requests"`
	OK          int64   `json:"ok"`
	Fail        int64   `json:"fail"`
	SuccessRate float64 `json:"success_rate"`
}

type aggLatency struct {
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
	P99Ms    float64 `json:"p99_ms"`
	MaxMs    float64 `json:"max_ms"`
	TTFT50Ms float64 `json:"ttft_p50_ms"`
	TTFT95Ms float64 `json:"ttft_p95_ms"`
}

type aggPoint struct {
	TS       string `json:"ts"`
	OK       int64  `json:"ok"`
	Fail     int64  `json:"fail"`
	Requests int64  `json:"requests"`
}

type aggModelRow struct {
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	OK               int64   `json:"ok"`
	Fail             int64   `json:"fail"`
	SuccessRate      float64 `json:"success_rate"`
	P95Ms            float64 `json:"p95_ms"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
}

type aggNodeRow struct {
	Name        string  `json:"name"`
	Requests    int64   `json:"requests"`
	OK          int64   `json:"ok"`
	Fail        int64   `json:"fail"`
	SuccessRate float64 `json:"success_rate"`
	AvgMs       float64 `json:"avg_ms"`
}

type aggErrorRow struct {
	Kind   string `json:"kind"`
	Count  int64  `json:"count"`
	Sample string `json:"sample"`
}

type aggAttempts struct {
	A0     int64 `json:"a0"`
	A1     int64 `json:"a1"`
	A2     int64 `json:"a2"`
	A3Plus int64 `json:"a3plus"`
}

type aggPhases struct {
	QueueAvgMs float64 `json:"queue_avg_ms"`
	TTFTAvgMs  float64 `json:"ttft_avg_ms"`
	GenAvgMs   float64 `json:"gen_avg_ms"`
}

type aggUnmapped struct {
	Count   int64              `json:"count"`
	ByModel []aggUnmappedModel `json:"by_model"`
}

type aggUnmappedModel struct {
	Model string `json:"model"`
	Count int64  `json:"count"`
}

// 直方桶边界（毫秒）。分位数用「直方桶 + 桶内线性插值」，不保存全量样本。
// 分布跨度大（几 ms ~ 数分钟）用近对数刻度；低延迟区（<1s）密一些。
var aggDurBucketsMS = []float64{0, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 20000, 30000, 60000, 120000, 300000, 600000}
var aggTTFTBucketsMS = []float64{0, 5, 10, 25, 50, 100, 150, 200, 300, 500, 800, 1200, 2000, 4000, 8000, 16000, 30000, 60000}

// aggBucketBoundsFor 返回值 v 落在的直方桶闭区间（测试用；生产侧插值也用它）。
func aggBucketBoundsFor(v float64) (lo, hi float64) {
	bs := aggDurBucketsMS
	lo = bs[len(bs)-1]
	hi = lo * 2
	for i := 0; i < len(bs); i++ {
		if v >= bs[i] {
			lo = bs[i]
		} else {
			hi = bs[i]
			return lo, hi
		}
	}
	return lo, hi
}

// ---- 缓存：20s TTL + singleflight ----

// aggregateCacheTTL 是分析页聚合的缓存时长。15~30s 是面板可接受的新鲜度，
// 也是「分析页连续切 range / 多点几下」不重复扫盘的窗口。测试可临时改小。
var aggregateCacheTTL = 20 * time.Second

type aggCacheEntry struct {
	expire time.Time
	resp   aggregateResponse
}

type aggCacheCall struct {
	done chan struct{}
	resp aggregateResponse
}

var aggCache = struct {
	mu       sync.Mutex
	entries  map[string]aggCacheEntry
	inflight map[string]*aggCacheCall
}{entries: map[string]aggCacheEntry{}, inflight: map[string]*aggCacheCall{}}

// aggregateCacheKey 是缓存键：必须同时含 range 与当日日期（跨零点自然失效）。
// 「today」是滚动 24h 窗口（按整点对齐），一小时一变；把小时也编进键会让
// 缓存命中率随小时边界变化但没有正确性收益（TTL 20s 远小于 1h），
// 因此键只到「日」——跨零点时日期变化即自然失效。
func aggregateCacheKey(rng string, now time.Time) string {
	// 键里带 store 身份（path|dir）：生产进程内不变，但**测试会反复替换夹具目录**。
	// 不带它的话，上一个测试针对同一 range/日期缓存的响应会串到下一个夹具，
	// 造成「换了一份数据还返回旧数字」的假失败/假通过。
	callLog.mu.RLock()
	storePath, storeDir := callLog.path, callLog.dir
	callLog.mu.RUnlock()
	return rng + "|" + now.In(time.Local).Format("2006-01-02") + "|" + storePath + "|" + storeDir
}

// aggregateCacheReset 清空缓存（测试与数据变更用）。
func aggregateCacheReset() {
	aggCache.mu.Lock()
	aggCache.entries = map[string]aggCacheEntry{}
	aggCache.inflight = map[string]*aggCacheCall{}
	aggCache.mu.Unlock()
}

// aggregateResponseFor 组装分析页聚合响应（TTL 缓存 + singleflight）。
//
// singleflight 不是可选优化：冷启动首次聚合要扫一遍磁盘（生产 23 万行
// 约数百 ms），面板一次刷新 6 个 Panel 会并发打进来，没有单飞就是 6 次
// 全量扫描叠加。这里用「每个 key 一个 call + done channel」实现，
// 不引入外部依赖。
func aggregateResponseFor(rng string) aggregateResponse {
	rng = normalizeTrendRange(rng)
	now := time.Now()
	key := aggregateCacheKey(rng, now)

	aggCache.mu.Lock()
	if e, ok := aggCache.entries[key]; ok && now.Before(e.expire) {
		aggCache.mu.Unlock()
		return e.resp
	}
	if c, ok := aggCache.inflight[key]; ok {
		aggCache.mu.Unlock()
		<-c.done
		return c.resp
	}
	c := &aggCacheCall{done: make(chan struct{})}
	aggCache.inflight[key] = c
	aggCache.mu.Unlock()

	resp := aggregateCompute(rng)

	aggCache.mu.Lock()
	c.resp = resp
	delete(aggCache.inflight, key)
	aggCache.entries[key] = aggCacheEntry{expire: time.Now().Add(aggregateCacheTTL), resp: resp}
	aggCache.mu.Unlock()
	close(c.done)
	return resp
}

// ---- 直方桶 ----

// aggHist 是「计数 + 值和」的直方桶。和只用于**单样本桶**回真值：
// 桶里只有 1 条时（如单条记录夹具、p99 落在尾部稀疏处）插值会给出桶边界，
// 而 Σ/1 给出精确值。多样本桶仍用桶内线性插值（不保存全量样本的硬约束）。
type aggHist struct {
	counts []int64
	sums   []float64
}

func newAggHist(capacity int) *aggHist {
	return &aggHist{counts: make([]int64, capacity), sums: make([]float64, capacity)}
}

// aggHistAdd 把 v 放进 bounds 定义的直方桶。bounds 最后一个元素是「无上界桶」的下界。
func aggHistAdd(h *aggHist, bounds []float64, v float64) {
	if v < 0 {
		v = 0
	}
	i := len(bounds) - 1
	for k := 0; k < len(bounds)-1; k++ {
		if v < bounds[k+1] {
			i = k
			break
		}
	}
	h.counts[i]++
	h.sums[i] += v
}

// aggHistQuantile 用「直方桶 + 桶内线性插值」求 q 分位（q∈[0,1]）。
// maxV 是窗口内实际最大值，用于无上界桶的回退。
func aggHistQuantile(h *aggHist, bounds []float64, q, maxV float64) float64 {
	if h == nil {
		return 0
	}
	var n int64
	for _, c := range h.counts {
		n += c
	}
	if n == 0 {
		return 0
	}
	rank := int64(math.Ceil(q * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	var cum int64
	for i, c := range h.counts {
		if c == 0 {
			continue
		}
		if cum+c >= rank {
			lo := bounds[i]
			if c == 1 {
				// 单样本桶：用精确值（见 aggHist 注释）。这个分支也让
				// 「单条记录时 p50=p95=p99=max」成立。
				return h.sums[i]
			}
			hi := math.Inf(1)
			if i+1 < len(bounds) {
				hi = bounds[i+1]
			}
			if math.IsInf(hi, 1) {
				if maxV <= 0 {
					return lo
				}
				return maxV
			}
			frac := float64(rank-cum) / float64(c)
			if frac < 0 {
				frac = 0
			}
			if frac > 1 {
				frac = 1
			}
			return lo + (hi-lo)*frac
		}
		cum += c
	}
	return maxV
}

// ---- 聚合扫描 ----

type aggNodeAcc struct {
	req, ok, fail int64
	durSum        float64
}

type aggErrorAcc struct {
	count  int64
	sample string
}

// aggScanResult 是一次磁盘扫描得到的「桶里没有」的分析维度。
type aggScanResult struct {
	durHist     *aggHist
	ttftHist    *aggHist
	maxDur      float64
	maxTTFT     float64
	perModel    map[string]*aggHist
	perModelMax map[string]float64 // 每模型自己的 max（无上界桶的分位回退要用它，不能借全局 max）
	byNode      map[string]*aggNodeAcc
	byError     map[string]*aggErrorAcc
	attempts    aggAttempts
	queueSum    float64
	queueN      int64
	ttftSum     float64
	ttftN       int64
	genSum      float64
	genN        int64
}

const aggErrorSampleMax = 120

// aggregateScanExtras 扫一遍窗口内的调用日志，填充分位/节点/错误/attempts/phases。
//
// 窗口过滤与 /api/stats 用**同一份** bidx（trendBuckets 返回），保证「哪些记录
// 算进窗口」与 totals/timeseries 完全一致。ts 解析失败的行走 parseLogFilterTime
// 的宽容分支；仍失败则跳过该行（与桶吸收的跳过语义一致）。
func aggregateScanExtras(ts []string, bidx func(time.Time) int) *aggScanResult {
	// 先把所有窗口相关段的物理行收集起来，再按行号切块并行消费。
	//
	// 为什么必须做并行：生产快照单文件 233,654 行、124.6 MB，单线程冷扫描
	// 实测 ~800 ms（time.Parse + 手写 JSON 扫描）。分析页冷请求目标是
	// p95 < 300 ms，单线程达不到；按行切块后 8 核实测降到 ~200 ms。
	// 单文件布局无法按段并行（只有一段），按行切块对两种布局都有效。
	var lines [][]byte
	for _, seg := range callLog.readSegments() {
		data, _, err := readDailySegment(seg.path, seg.gz)
		if err != nil {
			continue
		}
		lines = append(lines, splitJSONLines(data)...)
	}
	if len(lines) == 0 {
		return newAggScanResult()
	}

	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	if workers < 1 {
		workers = 1
	}
	if len(lines) < 4000 {
		workers = 1 // 小夹具/小库：并行的调度成本大于收益
	}
	parts := make([]*aggScanResult, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo := w * len(lines) / workers
		hi := (w + 1) * len(lines) / workers
		wg.Add(1)
		go func(w, lo, hi int) {
			defer wg.Done()
			part := newAggScanResult()
			memo := make(map[string]bool, 16) // 模型名 → 是否老行未映射（每 worker 私有）
			for _, line := range lines[lo:hi] {
				part.consumeAggLine(line, bidx, memo)
			}
			parts[w] = part
		}(w, lo, hi)
	}
	wg.Wait()
	return mergeAggScanResults(parts)
}

// newAggScanResult 初始化空的（部分）结果。
func newAggScanResult() *aggScanResult {
	return &aggScanResult{
		durHist:     newAggHist(len(aggDurBucketsMS)),
		ttftHist:    newAggHist(len(aggTTFTBucketsMS)),
		perModel:    make(map[string]*aggHist),
		perModelMax: make(map[string]float64),
		byNode:      make(map[string]*aggNodeAcc),
		byError:     make(map[string]*aggErrorAcc),
	}
}

// mergeAggScanResults 按 worker 顺序合并部分结果。
// 顺序合并保证 by_error.sample 取「文件里最早」的那条（与单线程语义一致），
// 不依赖 goroutine 完成顺序。
func mergeAggScanResults(parts []*aggScanResult) *aggScanResult {
	out := newAggScanResult()
	for _, p := range parts {
		if p == nil {
			continue
		}
		mergeAggHist(out.durHist, p.durHist)
		mergeAggHist(out.ttftHist, p.ttftHist)
		if p.maxDur > out.maxDur {
			out.maxDur = p.maxDur
		}
		if p.maxTTFT > out.maxTTFT {
			out.maxTTFT = p.maxTTFT
		}
		for m, v := range p.perModelMax {
			if v > out.perModelMax[m] {
				out.perModelMax[m] = v
			}
		}
		for m, h := range p.perModel {
			if out.perModel[m] == nil {
				out.perModel[m] = newAggHist(len(aggDurBucketsMS))
			}
			mergeAggHist(out.perModel[m], h)
		}
		for name, na := range p.byNode {
			dst := out.byNode[name]
			if dst == nil {
				dst = &aggNodeAcc{}
				out.byNode[name] = dst
			}
			dst.req += na.req
			dst.ok += na.ok
			dst.fail += na.fail
			dst.durSum += na.durSum
		}
		for kind, ea := range p.byError {
			dst := out.byError[kind]
			if dst == nil {
				dst = &aggErrorAcc{}
				out.byError[kind] = dst
			}
			dst.count += ea.count
			if dst.sample == "" {
				dst.sample = ea.sample
			}
		}
		out.attempts.A0 += p.attempts.A0
		out.attempts.A1 += p.attempts.A1
		out.attempts.A2 += p.attempts.A2
		out.attempts.A3Plus += p.attempts.A3Plus
		out.queueSum += p.queueSum
		out.queueN += p.queueN
		out.ttftSum += p.ttftSum
		out.ttftN += p.ttftN
		out.genSum += p.genSum
		out.genN += p.genN
	}
	return out
}

// mergeAggHist 把 src 的计数与值和累加进 dst。
func mergeAggHist(dst, src *aggHist) {
	for i := range src.counts {
		dst.counts[i] += src.counts[i]
		dst.sums[i] += src.sums[i]
	}
}

// consumeAggLine 消费一行：窗口过滤 + 全部分析维度累计。
// 与单线程版本逐字同语义；抽出来是为了按行切块并行。
func (res *aggScanResult) consumeAggLine(line []byte, bidx func(time.Time) int, memo map[string]bool) {
	var r aggScanRec
	if !parseAggLine(line, &r) {
		return
	}
	// 未映射判据必须与桶吸收**逐字同源**（verify-403 D1）：
	//  1. 有显式 unmapped 键 ⇒ 采信落盘值（请求发生那一刻冻结）。唯一例外与桶吸收
	//     一致：status=ok 视为已映射（成功即证明这个名字可服务）。
	//  2. 没有该键（本构建之前写的老行，生产占 99.88%）⇒ 用
	//     isUnmappedByFormula(model) && !everServed(model) 重算。
	// 只认显式键会让老行未映射泄漏进 attempts/by_node/by_error/分位，破坏
	// 「Σattempts == totals.requests」对账（生产快照 180d 曾差 1,391 条）。
	if r.SawUnmapped {
		if r.Unmapped && r.Status != "ok" {
			return
		}
	} else {
		v, hit := memo[r.Model]
		if !hit {
			// 与桶吸收同判据：公式 B && !everServed（曾成功过的名字永久 mapped）。
			v = isUnmappedByFormula(r.Model) && !isServedModel(r.Model)
			memo[r.Model] = v
		}
		if v {
			return
		}
	}
	at, err := parseLogFilterTime(r.TS)
	if err != nil {
		return
	}
	if bidx(at) < 0 {
		return
	}
	dur := float64(r.DurationMS)
	if dur < 0 {
		dur = 0
	}
	aggHistAdd(res.durHist, aggDurBucketsMS, dur)
	if dur > res.maxDur {
		res.maxDur = dur
	}
	if dur > res.perModelMax[r.Model] {
		res.perModelMax[r.Model] = dur
	}
	mh := res.perModel[r.Model]
	if mh == nil {
		mh = newAggHist(len(aggDurBucketsMS))
		res.perModel[r.Model] = mh
	}
	aggHistAdd(mh, aggDurBucketsMS, dur)

	if r.TTFTMs > 0 {
		aggHistAdd(res.ttftHist, aggTTFTBucketsMS, float64(r.TTFTMs))
		if float64(r.TTFTMs) > res.maxTTFT {
			res.maxTTFT = float64(r.TTFTMs)
		}
		res.ttftSum += float64(r.TTFTMs)
		res.ttftN++
	}

	switch {
	case r.Nodes <= 0:
		res.attempts.A0++
	case r.Nodes == 1:
		res.attempts.A1++
	case r.Nodes == 2:
		res.attempts.A2++
	default:
		res.attempts.A3Plus++
	}

	// by_node：按「最后一次尝试的节点显示名」聚合（无名字记 unknown）。
	name := r.LastNodeName
	if name == "" {
		name = "unknown"
	}
	na := res.byNode[name]
	if na == nil {
		na = &aggNodeAcc{}
		res.byNode[name] = na
	}
	na.req++
	na.durSum += dur
	if r.Status == "ok" {
		na.ok++
	} else {
		na.fail++
	}

	// phases：queue = 首个事件时间 − ts；ttft = TTFTMs；gen = duration − queue − ttft。
	if r.FirstEventAt != "" {
		if ft, err := parseLogFilterTime(r.FirstEventAt); err == nil {
			q := ft.Sub(at).Milliseconds()
			if q < 0 {
				q = 0
			}
			if int64(dur) > 0 && q > int64(dur) {
				q = int64(dur)
			}
			res.queueSum += float64(q)
			res.queueN++
			gen := int64(dur) - q
			if r.TTFTMs > 0 {
				gen -= r.TTFTMs
			}
			if gen < 0 {
				gen = 0
			}
			res.genSum += float64(gen)
			res.genN++
		}
	}

	if kind := aggClassifyError(&r); kind != "" {
		ea := res.byError[kind]
		if ea == nil {
			ea = &aggErrorAcc{}
			res.byError[kind] = ea
		}
		ea.count++
		if ea.sample == "" {
			sample := r.ErrMsg
			if sample == "" {
				sample = "status=" + r.Status
			}
			if len(sample) > aggErrorSampleMax {
				sample = sample[:aggErrorSampleMax]
			}
			ea.sample = sample
		}
	}
}

// aggClassifyError 把一条失败记录归到 by_error 的类别。
// 优先级：连接失败 → 超时 → 流中断 → 上游状态码 → 全失败 → 其它。
func aggClassifyError(r *aggScanRec) string {
	// 只给**失败请求**分类：一次成功请求中途可能有 connect_error / switch
	// （换节点后成功），它属于「重试成功」而不是错误面板的一条错误。
	// 不加这条守卫，by_error 合计会大于 totals.fail。
	if r.Status == "ok" {
		return ""
	}
	switch {
	case r.HasConnectError:
		return "connect_error"
	case r.HasTTFTTimeout || r.HasSilenceTime:
		return "timeout"
	case r.HasStreamInterr || r.HasStreamError:
		return "stream_interrupt"
	case strings.Contains(strings.ToLower(r.ErrMsg), "cooldown") || strings.Contains(r.ErrMsg, "冷却"):
		return "cooldown"
	case r.FinalStatus >= 500 || strings.Contains(r.ErrMsg, "http 5"):
		return "upstream_5xx"
	case r.FinalStatus >= 400 || strings.Contains(r.ErrMsg, "http 4"):
		return "upstream_4xx"
	case r.HasAllFailed:
		return "all_failed"
	case r.Status != "ok":
		return "other"
	}
	return ""
}

// aggregateWindow 从桶标签推出窗口 [start,end)（RFC3339 本地时区）。
func aggregateWindow(ts []string) (string, string) {
	if len(ts) == 0 {
		return "", ""
	}
	if p, err := time.ParseInLocation("2006-01-02T15:04", ts[0], time.Local); err == nil {
		return p.Format(time.RFC3339), p.Add(24 * time.Hour).Format(time.RFC3339)
	}
	if p, err := time.ParseInLocation("2006-01-02", ts[0], time.Local); err == nil {
		end := p.AddDate(0, 0, len(ts))
		return p.Format(time.RFC3339), end.Format(time.RFC3339)
	}
	return "", ""
}

// aggregateCompute 是一次真正的聚合计算（缓存未命中时执行）。
func aggregateCompute(rng string) aggregateResponse {
	ensureBucketsUpToDate()
	ts, bidx := trendBuckets(rng)
	winStart, winEnd := aggregateWindow(ts)

	out := aggregateResponse{
		Range:       rng,
		Window:      aggWindow{Start: winStart, End: winEnd},
		GeneratedAt: time.Now().Format(time.RFC3339),
		NodeAggBy:   "node_name",
		Timeseries:  make([]aggPoint, len(ts)),
		ByModel:     []aggModelRow{},
		ByNode:      []aggNodeRow{},
		ByError:     []aggErrorRow{},
		Unmapped:    aggUnmapped{ByModel: []aggUnmappedModel{}},
	}
	// timeseries / totals：与 trendsFromCallLog 同一份 foldBuckets 数学。
	points := make([]aggPoint, len(ts))
	for i, label := range ts {
		points[i] = aggPoint{TS: label}
	}
	for i, a := range foldBuckets(ts, bidx) {
		if i < 0 || i >= len(points) {
			continue
		}
		points[i].Requests += a.N
		points[i].OK += a.OK
		points[i].Fail += a.Fail
		out.Totals.Requests += a.N
		out.Totals.OK += a.OK
		out.Totals.Fail += a.Fail
	}
	// 与 trendsFromCallLog 相同：文件缺失时返回空数组（不是 24 个假零桶）。
	if callLogFileExists() {
		out.Timeseries = points
	} else {
		out.Timeseries = []aggPoint{}
	}
	if out.Totals.Requests > 0 {
		out.Totals.SuccessRate = float64(out.Totals.OK) / float64(out.Totals.Requests)
	}

	// 模型口径直接读 /api/stats 的同一批桶（modelStatsWithBuckets + foldModelsInWindow），
	// 2b 口径（TotalTokens=PT+CT、输入=prompt−cache_read）一个字不改。
	st := modelStatsWithBuckets(ts, bidx)
	names, aggs := foldModelsInWindow(ts, bidx)
	scan := aggregateScanExtras(ts, bidx)

	for _, name := range names {
		a := aggs[name]
		if a == nil {
			continue
		}
		row := aggModelRow{
			Model:    name,
			Requests: a.N,
			OK:       a.OK,
			Fail:     a.Fail,
		}
		if a.N > 0 {
			row.SuccessRate = float64(a.OK) / float64(a.N)
		}
		if ms := st.Models[name]; ms != nil {
			row.PromptTokens = ms.PromptTokens
			row.CompletionTokens = ms.CompletionTokens
			row.TotalTokens = ms.TotalTokens
		}
		row.P95Ms = aggHistQuantile(scan.perModel[name], aggDurBucketsMS, 0.95, scan.perModelMax[name])
		out.ByModel = append(out.ByModel, row)
	}

	out.Latency = aggLatency{
		P50Ms:    aggHistQuantile(scan.durHist, aggDurBucketsMS, 0.50, scan.maxDur),
		P95Ms:    aggHistQuantile(scan.durHist, aggDurBucketsMS, 0.95, scan.maxDur),
		P99Ms:    aggHistQuantile(scan.durHist, aggDurBucketsMS, 0.99, scan.maxDur),
		MaxMs:    scan.maxDur,
		TTFT50Ms: aggHistQuantile(scan.ttftHist, aggTTFTBucketsMS, 0.50, scan.maxTTFT),
		TTFT95Ms: aggHistQuantile(scan.ttftHist, aggTTFTBucketsMS, 0.95, scan.maxTTFT),
	}

	// by_node：请求数降序、同数按名字升序（输出顺序确定，不泄漏 map 遍历序）。
	for name, na := range scan.byNode {
		row := aggNodeRow{Name: name, Requests: na.req, OK: na.ok, Fail: na.fail}
		if na.req > 0 {
			row.SuccessRate = float64(na.ok) / float64(na.req)
			row.AvgMs = na.durSum / float64(na.req)
		}
		out.ByNode = append(out.ByNode, row)
	}
	sort.Slice(out.ByNode, func(i, j int) bool {
		if out.ByNode[i].Requests != out.ByNode[j].Requests {
			return out.ByNode[i].Requests > out.ByNode[j].Requests
		}
		return out.ByNode[i].Name < out.ByNode[j].Name
	})

	// by_error：数量降序、同数按类别升序。
	for kind, ea := range scan.byError {
		out.ByError = append(out.ByError, aggErrorRow{Kind: kind, Count: ea.count, Sample: ea.sample})
	}
	sort.Slice(out.ByError, func(i, j int) bool {
		if out.ByError[i].Count != out.ByError[j].Count {
			return out.ByError[i].Count > out.ByError[j].Count
		}
		return out.ByError[i].Kind < out.ByError[j].Kind
	})

	out.Attempts = scan.attempts
	if scan.queueN > 0 {
		out.Phases.QueueAvgMs = scan.queueSum / float64(scan.queueN)
	}
	if scan.ttftN > 0 {
		out.Phases.TTFTAvgMs = scan.ttftSum / float64(scan.ttftN)
	}
	if scan.genN > 0 {
		out.Phases.GenAvgMs = scan.genSum / float64(scan.genN)
	}

	// unmapped：与 /api/stats 的 unmapped_requests 同源同值（同一份 ts/bidx）。
	uc, ub := unmappedStatsInWindow(ts, bidx)
	out.Unmapped.Count = uc
	top := topUnmappedModels(ub, unmappedTopN)
	umModels := make([]string, 0, len(top))
	for m := range top {
		umModels = append(umModels, m)
	}
	sort.Strings(umModels)
	for _, m := range umModels {
		out.Unmapped.ByModel = append(out.Unmapped.ByModel, aggUnmappedModel{Model: m, Count: top[m]})
	}

	return out
}

// ---- task-56：保留策略（gzip 归档 + 按天删除）----

// compressSegmentFile 把 plain 日文件压成 .jsonl.gz（tmp + fsync + rename + 删 plain）。
// 崩溃在 rename 之后、remove 之前 ⇒ plain 与 gz 并存，重建段列表时选 plain，
// 下次归档重试覆盖（数据不会丢）。
func compressSegmentFile(path string) (string, error) {
	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	tmp := path + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	zw := gzip.NewWriter(out)
	if _, err := io.Copy(zw, in); err != nil {
		zw.Close()
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := zw.Close(); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	gzPath := path + ".gz"
	if err := os.Rename(tmp, gzPath); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return gzPath, err
	}
	return gzPath, nil
}

// maintainRetention 执行一次保留策略：压缩 gzipAfterDays 之前的明文段、
// 删除 retainDays 之前的段。只在「已全部吸收（seeded）」时动手 ——
// 否则删除会让尚未吸收的字节消失（统计永久缺失）。
// 返回 (压缩段数, 删除段数)。
func (s *callLogStore) maintainRetention(now time.Time) (compressed, pruned int, err error) {
	if s.dir == "" || callLogRetainDays <= 0 {
		return 0, 0, nil
	}
	s.mu.RLock()
	seeded := bucketsSeeded
	cur := dailyCur
	s.mu.RUnlock()
	if !seeded {
		return 0, 0, nil
	}

	localNow := now.In(time.Local)
	gzCutoff := localNow.AddDate(0, 0, -callLogGzipAfterDays).Format("2006-01-02")
	// 1) 归档：只压缩「非最后一段 + 非游标段 + 超过归档天数」的明文段
	segs := s.dailySegments()
	for i, sg := range segs {
		if i >= len(segs)-1 || i >= cur.idx {
			break
		}
		if sg.legacy || sg.gz || sg.day == "" || sg.day > gzCutoff {
			continue
		}
		if _, cerr := compressSegmentFile(sg.path); cerr != nil {
			slog.Warn("calllog: gzip archive failed (will retry next hour)", "path", sg.path, "error", cerr)
			continue
		}
		compressed++
	}
	if compressed > 0 {
		s.mu.Lock()
		dailySegsKey = "" // 路径变了：强制重建
		s.ensureDailySegsLocked()
		s.mu.Unlock()
	}

	// 2) 删除：只删游标之前的段，且保留 legacy（存量原样不动）
	retainCutoff := localNow.AddDate(0, 0, -callLogRetainDays).Format("2006-01-02")
	s.mu.Lock()
	defer s.mu.Unlock()
	if !bucketsSeeded {
		return compressed, 0, nil
	}
	s.ensureDailySegsLocked()
	for i, sg := range dailySegs {
		if i >= dailyCur.idx {
			break
		}
		if sg.legacy || sg.day == "" || sg.day >= retainCutoff {
			continue
		}
		if rerr := os.Remove(sg.path); rerr != nil && !os.IsNotExist(rerr) {
			slog.Warn("calllog: prune failed", "path", sg.path, "error", rerr)
			continue
		}
		pruned++
	}
	if pruned > 0 {
		dailySegsKey = ""
		s.ensureDailySegsLocked() // 重建列表并按路径保住游标
	}
	return compressed, pruned, nil
}

// startCallLogArchiver 每小时跑一次保留策略（仅 daily 模式）。
// 放在后台而不是请求路径：压缩/删除是磁盘 I/O，不该让面板查询等它。
func startCallLogArchiver() {
	callLog.mu.RLock()
	dir := callLog.dir
	callLog.mu.RUnlock()
	if dir == "" {
		return
	}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for range t.C {
			if _, _, err := callLog.maintainRetention(time.Now()); err != nil {
				slog.Warn("calllog: retention maintenance failed", "error", err)
			}
		}
	}()
}

func adminCallLogHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		limit := 500
		if v := q.Get("limit"); v != "" {
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
				limit = n
			}
		}
		if limit > callLogCapacity {
			limit = callLogCapacity
		}
		// 无任何过滤参数 ⇒ 走内存环（旧行为逐字节兼容：最新在前、默认 500）。
		filter := callLogHistoryFilter{limit: limit}
		useFile := false
		badCursor := func() {
			w.Header().Set("X-Call-Log-Error", "bad-cursor")
			http.Error(w, `{"error":"bad-cursor"}`, http.StatusBadRequest)
		}
		// 位置游标（task-65 D3）：?cursor=c1_xxx 精确指定「从哪一段的哪一行之后续扫」，
		// 与 ts 是否有序无关；显式给 cursor 时 before/before_req_id 的 ts 语义不再参与。
		if v := q.Get("cursor"); v != "" {
			dec, ok := decodeCallLogCursor(v)
			if !ok {
				badCursor()
				return
			}
			filter.hasCursor, filter.cursorSeg, filter.cursorLine = true, dec.Seg, dec.Line
			useFile = true
		}
		var legacyBefore time.Time
		legacyBeforeOK := false
		legacyTokenCursor := false
		if v := q.Get("before"); v != "" && !filter.hasCursor {
			tt, err := parseLogFilterTime(v)
			if err != nil {
				http.Error(w, `{"error":"invalid before"}`, http.StatusBadRequest)
				return
			}
			legacyBefore, legacyBeforeOK = tt, true
			filter.hasBefore, filter.before, filter.beforeReqID = true, tt, q.Get("before_req_id")
			// 兼容旧调用方：它们只回传 X-Call-Log-Next-Before-Req。新实现该头返回的是
			// 不透明位置游标（c1_ 前缀），这里识别出来走位置续扫；裸 req_id（老实现/手工构造）
			// 仍按原排序键语义处理，零影响。
			if strings.HasPrefix(filter.beforeReqID, callLogCursorPrefix) {
				dec, ok := decodeCallLogCursor(filter.beforeReqID)
				if !ok {
					badCursor()
					return
				}
				filter.hasCursor, filter.cursorSeg, filter.cursorLine = true, dec.Seg, dec.Line
				filter.hasBefore = false
				legacyTokenCursor = true
			}
			useFile = true
		}
		if v := q.Get("after"); v != "" {
			tt, err := parseLogFilterTime(v)
			if err != nil {
				http.Error(w, `{"error":"invalid after"}`, http.StatusBadRequest)
				return
			}
			filter.hasAfter, filter.after = true, tt
			useFile = true
		}
		if v := q.Get("model"); v != "" {
			filter.model, useFile = v, true
		}
		if v := q.Get("status"); v != "" {
			switch v {
			case "all", "ok", "fail":
				filter.status, useFile = v, true
			default:
				http.Error(w, `{"error":"invalid status"}`, http.StatusBadRequest)
				return
			}
		}
		if v := q.Get("q"); v != "" {
			filter.q, useFile = v, true
		}
		w.Header().Set("Content-Type", "application/json")
		if useFile {
			recs, hasMore, truncated, nextCursor, err := callLog.historyQuery(filter)
			if err != nil && errors.Is(err, errCallLogCursorStale) && legacyTokenCursor && legacyBeforeOK {
				// 旧调用方只回传响应头、无法处理 400：游标段在分页之间被保留策略删除时
				// 优雅回退到旧 before 语义（排他 ts），并用 X-Call-Log-Cursor-Fallback 标记。
				// 显式 ?cursor= 的新调用方相反——明确 400 stale-cursor，由它从头重分页。
				fallback := callLogHistoryFilter{
					limit: filter.limit, hasBefore: true, before: legacyBefore,
					hasAfter: filter.hasAfter, after: filter.after,
					model: filter.model, status: filter.status, q: filter.q,
				}
				recs, hasMore, truncated, nextCursor, err = callLog.historyQuery(fallback)
				if err == nil {
					w.Header().Set("X-Call-Log-Cursor-Fallback", "before")
				}
			}
			if err != nil {
				if errors.Is(err, errCallLogCursorStale) {
					w.Header().Set("X-Call-Log-Error", "stale-cursor")
					http.Error(w, `{"error":"stale-cursor"}`, http.StatusBadRequest)
					return
				}
				http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
				return
			}
			w.Header().Set("X-Call-Log-Source", "file")
			w.Header().Set("X-Call-Log-Has-More", fmt.Sprintf("%t", hasMore))
			if truncated {
				w.Header().Set("X-Call-Log-Truncated", "true")
			}
			if nextCursor != "" {
				w.Header().Set("X-Call-Log-Next-Cursor", nextCursor)
			}
			if len(recs) > 0 {
				oldest := recs[len(recs)-1]
				w.Header().Set("X-Call-Log-Next-Before", oldest.TS)
				if nextCursor != "" {
					// 新实现：Next-Before-Req 回传不透明位置游标（不再是裸 req_id）。
					// 旧调用方照原样回传即可得到精确续页——裸 req_id 会被当排序键，
					// 在 ts 乱序的真实数据上会漏/重（task-65 D3）。
					w.Header().Set("X-Call-Log-Next-Before-Req", nextCursor)
				} else {
					w.Header().Set("X-Call-Log-Next-Before-Req", oldest.ReqID)
				}
			}
			_ = json.NewEncoder(w).Encode(recs)
			return
		}
		recs := callLog.latest(limit)
		// 最新在前（前端直接渲染）
		for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
			recs[i], recs[j] = recs[j], recs[i]
		}
		w.Header().Set("X-Call-Log-Source", "memory")
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
