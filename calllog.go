package main

// 调用日志（Call Log）：一次上游请求一条结构化记录（req_id 贯穿全路径），
// 内存环形缓冲 + JSONL 落盘（与 config.json 同目录，重启可恢复），
// 供管理面板“调用日志”视图（列表/时段分析/节点分析，参考 opencode2api_enhance）。

import (
	"context"
	"encoding/json"
	"fmt"
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

// ---- 全局环形缓冲 + JSONL 落盘 ----

type callLogStore struct {
	mu      sync.Mutex
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

func loadCallLogFromFile() {
	callLog.mu.Lock()
	p := callLog.path
	callLog.mu.Unlock()
	if p == "" {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	records := make([]CallRecord, 0, 64)
	for _, line := range strings.Split(string(data), "\n") {
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

func (s *callLogStore) append(rec CallRecord) {
	s.mu.Lock()
	s.records = append(s.records, rec)
	if len(s.records) > callLogCapacity {
		s.records = s.records[len(s.records)-callLogCapacity:]
	}
	p := s.path
	s.mu.Unlock()
	if p == "" {
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	f.Write(append(b, '\n'))
	f.Close()
}

func (s *callLogStore) latest(max int) []CallRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.records = nil
	p := s.path
	s.mu.Unlock()
	_ = os.Remove(p)
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
			i := int(t.In(loc).Sub(start) / time.Hour)
			if i < 0 || i >= 24 {
				return -1
			}
			return i
		}
	}
}

// forEachBucketedCallRecord 用给定的桶配置遍历落在区间内的调用记录，
// fn 收到记录与桶索引。返回 false 表示日志文件不可读。
func forEachBucketedCallRecord(ts []string, bidx func(time.Time) int, fn func(rec CallRecord, bucket int)) bool {
	callLog.mu.Lock()
	p := callLog.path
	callLog.mu.Unlock()
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
func trendsFromCallLog(rng string) []TrendsPoint {
	ts, bidx := trendBuckets(rng)
	buckets := make([]TrendsPoint, len(ts))
	for i, label := range ts {
		buckets[i] = TrendsPoint{TS: label}
	}
	if !forEachBucketedCallRecord(ts, bidx, func(rec CallRecord, i int) {
		buckets[i].Requests++
		if rec.Status == "ok" {
			buckets[i].OK++
		} else {
			buckets[i].Fail++
		}
		buckets[i].PromptTokens += rec.PromptTokens
		buckets[i].CompletionTokens += rec.CompletionTokens
		buckets[i].CacheCreation += rec.CacheCreation
		buckets[i].CacheRead += rec.CacheRead
	}) {
		return nil
	}
	return buckets
}

// trendsByModelFromCallLog 在 trendsFromCallLog 的切分基础上按模型再分一层。
// 对区间内出现过请求的每个模型输出全部桶（无数据填零），保证该模型的折线
// 覆盖整个区间而非只在有流量的几天出现。输出顺序：模型名升序，桶索引升序。
func trendsByModelFromCallLog(rng string) []ModelTrendPoint {
	ts, bidx := trendBuckets(rng)
	idx := make(map[string][]ModelTrendPoint)
	forEachBucketedCallRecord(ts, bidx, func(rec CallRecord, i int) {
		row, ok := idx[rec.Model]
		if !ok {
			row = make([]ModelTrendPoint, len(ts))
			for j, label := range ts {
				row[j] = ModelTrendPoint{TS: label, Model: rec.Model}
			}
			idx[rec.Model] = row
		}
		row[i].Requests++
		if rec.Status == "ok" {
			row[i].OK++
		} else {
			row[i].Fail++
		}
		row[i].PromptTokens += rec.PromptTokens
		row[i].CompletionTokens += rec.CompletionTokens
		row[i].CacheCreation += rec.CacheCreation
		row[i].CacheRead += rec.CacheRead
	})
	models := make([]string, 0, len(idx))
	for m := range idx {
		models = append(models, m)
	}
	sort.Strings(models)
	out := make([]ModelTrendPoint, 0, len(models)*len(ts))
	for _, m := range models {
		out = append(out, idx[m]...)
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
func modelStatsFromCallLog(rng string) *TokenStatsData {
	rng = normalizeTrendRange(rng)
	ts, bidx := trendBuckets(rng)
	out := &TokenStatsData{Models: map[string]*ModelStats{}}
	if !forEachBucketedCallRecord(ts, bidx, func(rec CallRecord, _ int) {
		ms, ok := out.Models[rec.Model]
		if !ok {
			ms = &ModelStats{}
			out.Models[rec.Model] = ms
		}
		out.TotalRequests++
		ms.RequestCount++
		ms.PromptTokens += rec.PromptTokens
		ms.CompletionTokens += rec.CompletionTokens
		ms.TotalTokens += rec.PromptTokens + rec.CompletionTokens
		ms.CacheReadTokens += rec.CacheRead
		ms.CacheCreatedTokens += rec.CacheCreation
		if rec.Model == "" || rec.OutputSpeed <= 0 {
			return
		}
		ms.StreamReqCount++
		n := float64(ms.StreamReqCount)
		ms.AvgTTFTMs += (float64(rec.TTFTMs) - ms.AvgTTFTMs) / n
		ms.AvgOutputSpeed += (rec.OutputSpeed - ms.AvgOutputSpeed) / n
	}) {
		return &TokenStatsData{Models: map[string]*ModelStats{}}
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
