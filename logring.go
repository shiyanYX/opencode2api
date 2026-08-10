package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 运行日志页（WebUI）的内存日志环：捕获脱敏后的结构化日志条目，
// 供 /api/logs（快照）、/api/logs/stream（SSE 增量）、/api/logs/export（导出）使用。

const logRingCapacity = 5000

type logAttrKV struct {
	Key   string `json:"k"`
	Value string `json:"v"`
}

type logEntry struct {
	Seq     uint64      `json:"seq"`
	Time    time.Time   `json:"time"`
	Level   string      `json:"level"`
	Message string      `json:"msg"`
	Attrs   []logAttrKV `json:"attrs,omitempty"`
}

type logRing struct {
	mu       sync.Mutex
	entries  []logEntry
	startSeq uint64
	endSeq   uint64
	notify   chan struct{}
}

var globalLogRing = &logRing{
	entries: make([]logEntry, 0, logRingCapacity),
	notify:  make(chan struct{}, 1),
}

func (r *logRing) push(e logEntry) {
	r.mu.Lock()
	e.Seq = r.endSeq
	r.endSeq++
	if len(r.entries) < logRingCapacity {
		r.entries = append(r.entries, e)
	} else {
		r.entries[e.Seq%logRingCapacity] = e
		r.startSeq++
	}
	r.mu.Unlock()
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// snapshot 返回最近 limit 条（旧→新，按 seq 序，规避环形覆盖后的位置错位）。
func (r *logRing) snapshot(limit int) []logEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.entries)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]logEntry, 0, limit)
	for seq := r.endSeq - uint64(limit); seq < r.endSeq; seq++ {
		out = append(out, r.entries[seq%logRingCapacity])
	}
	return out
}

// after 返回 seq 严格大于 last 的条目（旧→新，最多 cap 条防覆盖跳跃）。
func (r *logRing) after(last uint64) []logEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last+1 < r.startSeq {
		last = r.startSeq - 1
	}
	if last >= r.endSeq {
		return nil
	}
	n := int(r.endSeq - last - 1)
	if n > logRingCapacity {
		n = logRingCapacity
	}
	out := make([]logEntry, 0, n)
	for seq := last + 1; seq < r.endSeq; seq++ {
		out = append(out, r.entries[seq%logRingCapacity])
	}
	return out
}

// captureHandler 包装在 TextHandler 外层：把每条将要写盘的记录（已按
// redactLogAttr 同规则脱敏）存入环形缓冲，再交给内层 handler 输出。
// Enabled 代理内层，保证缓冲内容与落盘/stdout 语义一致。
type captureHandler struct {
	inner slog.Handler
	ring  *logRing
}

func newCaptureHandler(inner slog.Handler) *captureHandler {
	return &captureHandler{inner: inner, ring: globalLogRing}
}

func (h *captureHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *captureHandler) Handle(ctx context.Context, rec slog.Record) error {
	ring := h.ring
	if ring == nil {
		ring = globalLogRing
	}
	e := logEntry{
		Time:    rec.Time,
		Level:   rec.Level.String(),
		Message: rec.Message,
	}
	rec.Attrs(func(a slog.Attr) bool {
		if redacted := redactLogAttr([]string{}, a); redacted.Key != "" && redacted.Key != slog.TimeKey {
			e.Attrs = append(e.Attrs, logAttrKV{Key: redacted.Key, Value: attrString(redacted)})
		}
		return true
	})
	ring.push(e)
	return h.inner.Handle(ctx, rec)
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return &captureHandler{inner: h.inner.WithAttrs(attrs), ring: h.ring}
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	return &captureHandler{inner: h.inner.WithGroup(name), ring: h.ring}
}

func attrString(a slog.Attr) string {
	switch v := a.Value.Any().(type) {
	case string:
		return v
	case time.Time:
		return v.Format("2006-01-02T15:04:05.000Z07:00")
	default:
		return a.Value.String()
	}
}

// ---- API 端点：adminLogsHandler（/api/logs 前缀，requireAuth 保护） ----

const maxLogRows = 500

type logsSnapshot struct {
	Entries []logEntry `json:"entries"`
	NextSeq uint64     `json:"next_seq"`
}

func adminLogsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/logs":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(logsSnapshot{
			Entries: globalLogRing.snapshot(maxLogRows),
			NextSeq: globalLogRing.endSeq,
		})
	case "/api/logs/stream":
		serveLogStream(w, r)
	case "/api/logs/export":
		serveLogExport(w, r)
	default:
		http.NotFound(w, r)
	}
}

func serveLogStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming unsupported"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// 首帧推最近 maxLogRows 条（含重连场景，前端按 seq 去重），再增量推送。
	last := uint64(0)
	for _, e := range globalLogRing.snapshot(maxLogRows) {
		if !writeSSE(w, flusher, e) {
			return
		}
		last = e.Seq
	}

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			// 注释行心跳，防中间层断连。
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-globalLogRing.notify:
			for _, e := range globalLogRing.after(last) {
				if !writeSSE(w, flusher, e) {
					return
				}
				last = e.Seq
			}
		}
	}
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, e logEntry) bool {
	payload, err := json.Marshal(e)
	if err != nil {
		return true
	}
	var sb strings.Builder
	sb.WriteString("id: ")
	sb.WriteString(strconv.FormatUint(e.Seq, 10))
	sb.WriteString("\ndata: ")
	sb.Write(payload)
	sb.WriteString("\n\n")
	if _, err := w.Write([]byte(sb.String())); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func serveLogExport(w http.ResponseWriter, r *http.Request) {
	entries := globalLogRing.snapshot(logRingCapacity)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="opencode2api-logs.txt"`)

	var sb strings.Builder
	sb.WriteString("# opencode2api log export — ")
	sb.WriteString(strconv.Itoa(len(entries)))
	sb.WriteString(" entries (seq ")
	if len(entries) > 0 {
		sb.WriteString(strconv.FormatUint(entries[0].Seq, 10))
		sb.WriteString("..")
		sb.WriteString(strconv.FormatUint(entries[len(entries)-1].Seq, 10))
	} else {
		sb.WriteString("-")
	}
	sb.WriteString(")\n")
	_, _ = w.Write([]byte(sb.String()))

	for _, e := range entries {
		writeTextLine(w, e)
	}
}

func writeTextLine(w http.ResponseWriter, e logEntry) {
	var sb strings.Builder
	sb.WriteString("time=")
	sb.WriteString(e.Time.Format("2006-01-02T15:04:05.000Z07:00"))
	sb.WriteString(" level=")
	sb.WriteString(strings.ToUpper(e.Level))
	sb.WriteString(" msg=")
	sb.WriteString(strconv.Quote(e.Message))
	for _, kv := range e.Attrs {
		sb.WriteByte(' ')
		sb.WriteString(kv.Key)
		sb.WriteByte('=')
		sb.WriteString(strconv.Quote(kv.Value))
	}
	sb.WriteByte('\n')
	_, _ = w.Write([]byte(sb.String()))
}
