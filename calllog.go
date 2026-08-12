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
	"strings"
	"sync"
	"time"
)

type CallEvent struct {
	Type   string `json:"type"`
	Node   string `json:"node,omitempty"`
	Detail string `json:"detail,omitempty"`
	At     string `json:"at,omitempty"`
}

type CallRecord struct {
	ReqID            string      `json:"req_id"`
	TS               string      `json:"ts"`
	Path             string      `json:"path,omitempty"`
	Model            string      `json:"model,omitempty"`
	Stream           bool        `json:"stream,omitempty"`
	RouteMode        string      `json:"route_mode,omitempty"`
	Nodes            []string    `json:"nodes,omitempty"`
	Events           []CallEvent `json:"events,omitempty"`
	Status           string      `json:"status,omitempty"`
	PromptTokens     int64       `json:"prompt_tokens,omitempty"`
	CompletionTokens int64       `json:"completion_tokens,omitempty"`
	DurationMS       int64       `json:"duration_ms,omitempty"`
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
	seenNodes     map[string]bool
	promptTok     int64
	completionTok int64
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
	cr.mu.Lock()
	if node != "" && !cr.seenNodes[node] {
		cr.seenNodes[node] = true
		cr.rec.Nodes = append(cr.rec.Nodes, node)
	}
	cr.rec.Events = append(cr.rec.Events, CallEvent{
		Type:   typ,
		Node:   node,
		Detail: detail,
		At:     time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
	})
	cr.mu.Unlock()
}

func (cr *callRecorder) finish(status int, errMsg string, promptTok, completionTok int64) {
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
	cr.rec.PromptTokens = cr.promptTok
	cr.rec.CompletionTokens = cr.completionTok
	if !cr.start.IsZero() {
		cr.rec.DurationMS = time.Since(cr.start).Milliseconds()
	}
	rec := cr.rec
	cr.mu.Unlock()
	callLog.append(rec)
}

// 便捷包级函数：ctx 无 recorder 时全部安全 no-op。

func callLogEvent(ctx context.Context, typ, node, detail string) {
	callRecorderFrom(ctx).event(typ, node, detail)
}

func callLogFinish(ctx context.Context, status int, errMsg string, promptTok, completionTok int64) {
	callRecorderFrom(ctx).finish(status, errMsg, promptTok, completionTok)
}

// usageFromOpenAIBody 从 OpenAI 兼容成功响应体提取 token 用量（严格模式，缺失返回 0）。
func usageFromOpenAIBody(b []byte) (int64, int64) {
	var m struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return 0, 0
	}
	return m.Usage.PromptTokens, m.Usage.CompletionTokens
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
