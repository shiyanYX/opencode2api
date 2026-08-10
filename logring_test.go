package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func t0() time.Time { return time.Unix(0, 0) }

func TestLogRingSnapshotAndAfter(t *testing.T) {
	r := &logRing{}
	r.push(logEntry{Time: t0(), Level: "INFO", Message: "m0"})
	r.push(logEntry{Time: t0(), Level: "INFO", Message: "m1"})
	if got := len(r.snapshot(10)); got != 2 {
		t.Fatalf("snapshot len=%d want 2", got)
	}
	after := r.after(0)
	if len(after) != 1 || after[0].Message != "m1" {
		t.Fatalf("after(0) = %#v", after)
	}
	if r.after(99) != nil {
		t.Fatalf("after beyond end should be nil")
	}
	// 容量覆盖：写入超过容量后，snapshot 仍是尾部的 limit 条
	for i := 2; i < logRingCapacity+5; i++ {
		r.push(logEntry{Time: t0(), Level: "INFO", Message: "m"})
	}
	if got := len(r.snapshot(10)); got != 10 {
		t.Fatalf("snapshot len=%d want 10", got)
	}
	last := r.snapshot(1)[0]
	if last.Message != "m" || last.Seq != logRingCapacity+4 {
		t.Fatalf("last entry seq=%d msg=%q", last.Seq, last.Message)
	}
	// 全量导出（snapshot 容量上限）
	if got := len(r.snapshot(logRingCapacity)); got != logRingCapacity {
		t.Fatalf("full snapshot len=%d want %d", got, logRingCapacity)
	}
}

func TestCaptureHandlerRedactsAndLevelFilters(t *testing.T) {
	r := &logRing{}
	inner := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})
	ch := &captureHandler{inner: inner, ring: r}
	l := slog.New(ch)
	l.Info("login attempt", "user", "alice", "password", "super-secret-abc", "api_key", "sk-abcdef1234567890")

	entries := r.snapshot(10)
	if len(entries) != 1 {
		t.Fatalf("entries=%d want 1", len(entries))
	}
	e := entries[0]
	if e.Message != "login attempt" {
		t.Fatalf("msg=%q", e.Message)
	}
	for _, kv := range e.Attrs {
		if kv.Key == "password" && !strings.Contains(kv.Value, "…") {
			t.Fatalf("password not redacted: %q", kv.Value)
		}
		if kv.Key == "api_key" && !strings.Contains(kv.Value, "…") {
			t.Fatalf("api_key not redacted: %q", kv.Value)
		}
	}
	// Enabled 代理：高于当前级别的日志不产生条目
	innerInfo := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})
	ch2 := &captureHandler{inner: innerInfo, ring: r}
	slog.New(ch2).Debug("hidden", "k", "v")
	if n := len(r.snapshot(10)); n != 1 {
		t.Fatalf("debug leaked into buffer: %d entries", n)
	}
}

func TestAdminLogsSnapshotAndExport(t *testing.T) {
	globalLogRing.push(logEntry{Time: t0(), Level: "WARN", Message: "quota exhausted",
		Attrs: []logAttrKV{{Key: "model", Value: "free-m"}}})

	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	rec := httptest.NewRecorder()
	adminLogsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"msg":"quota exhausted"`) {
		t.Fatalf("snapshot missing entry: %s", body[:200])
	}
	if !strings.Contains(body, `"next_seq"`) {
		t.Fatalf("snapshot missing next_seq")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/logs/export", nil)
	rec2 := httptest.NewRecorder()
	adminLogsHandler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("export status=%d", rec2.Code)
	}
	out := rec2.Body.String()
	if !strings.Contains(out, `level=WARN msg="quota exhausted"`) {
		t.Fatalf("export text wrong:\n%s", out[:400])
	}
	if !strings.Contains(rec2.Header().Get("Content-Disposition"), "opencode2api-logs.txt") {
		t.Fatalf("export missing content-disposition")
	}
}

func TestAdminLogsStreamFirstFrameAndCancel(t *testing.T) {
	globalLogRing.push(logEntry{Time: t0(), Level: "INFO", Message: "stream probe"})

	req := httptest.NewRequest(http.MethodGet, "/api/logs/stream", nil)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		adminLogsHandler(rec, req)
		close(done)
	}()
	// 等待首帧写入后再取消：handler 应干净退出（无死锁、无 panic）。
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("stream handler did not exit on context cancel")
	}
	out := rec.Body.String()
	if !strings.Contains(out, "id: ") || !strings.Contains(out, "data: ") {
		t.Fatalf("first frame missing id/data: %q", out[:Min(len(out), 200)])
	}
	if !strings.Contains(out, `"msg":"stream probe"`) {
		t.Fatalf("first frame missing entry: %q", out[:Min(len(out), 200)])
	}
}

func Min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
