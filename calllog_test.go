package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTrendsFromCallLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "call_log.jsonl")
	now := time.Now()
	yesterday := now.AddDate(0, 0, -1)
	lines := []string{
		`{"req_id":"a","ts":"` + now.Format(time.RFC3339) + `","status":"ok","prompt_tokens":100,"completion_tokens":50,"cache_creation_tokens":10,"cache_read_tokens":20}`,
		`{"req_id":"b","ts":"` + now.Format(time.RFC3339) + `","status":"fail","prompt_tokens":0,"completion_tokens":0}`,
		`{"req_id":"c","ts":"` + yesterday.Format(time.RFC3339) + `","status":"ok","prompt_tokens":7,"completion_tokens":9,"cache_read_tokens":2}`,
	}
	if err := os.WriteFile(path, []byte(lines[0]+"\n"+lines[1]+"\n"+lines[2]+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	callLog.mu.Lock()
	callLog.path = path
	callLog.mu.Unlock()
	defer func() {
		callLog.mu.Lock()
		callLog.path = ""
		callLog.mu.Unlock()
	}()

	today := trendsFromCallLog("today")
	if len(today) != 24 {
		t.Fatalf("today buckets = %d", len(today))
	}
	h := now.Hour()
	if today[h].Requests != 2 || today[h].OK != 1 || today[h].Fail != 1 {
		t.Fatalf("today hour %d = %+v", h, today[h])
	}
	if today[h].PromptTokens != 100 || today[h].CompletionTokens != 50 {
		t.Fatalf("today tokens = %+v", today[h])
	}
	if today[h].CacheCreation != 10 || today[h].CacheRead != 20 {
		t.Fatalf("today cache = %+v", today[h])
	}

	week := trendsFromCallLog("7d")
	if len(week) != 7 {
		t.Fatalf("7d buckets = %d", len(week))
	}
	if week[6].Requests != 2 || week[5].Requests != 1 {
		t.Fatalf("7d tail = %+v / %+v", week[5], week[6])
	}

	month := trendsFromCallLog("30d")
	if len(month) != 30 || month[29].Requests != 2 || month[28].Requests != 1 {
		t.Fatalf("30d tail mismatch: len=%d last=%+v prev=%+v", len(month), month[29], month[28])
	}
}

func TestTrendsFromCallLogNoFile(t *testing.T) {
	callLog.mu.Lock()
	callLog.path = filepath.Join(t.TempDir(), "missing.jsonl")
	callLog.mu.Unlock()
	defer func() {
		callLog.mu.Lock()
		callLog.path = ""
		callLog.mu.Unlock()
	}()
	if pts := trendsFromCallLog("today"); pts != nil {
		t.Fatalf("expected nil for missing file, got %+v", pts)
	}
}

func TestCallLogHasIssue(t *testing.T) {
	ok := CallRecord{Status: "ok"}
	if ok.HasIssue() {
		t.Fatal("ok record should not have issue")
	}
	switched := CallRecord{Status: "ok", Events: []CallEvent{{Type: "switch", Node: "n1"}}}
	if !switched.HasIssue() {
		t.Fatal("switch event should mark issue")
	}
	failed := CallRecord{Status: "fail"}
	if !failed.HasIssue() {
		t.Fatal("fail status should mark issue")
	}
	if got := switched.IssueLabel(); got != "已切换节点" {
		t.Fatalf("IssueLabel = %q", got)
	}
}

func TestCallLogStoreAppendLatest(t *testing.T) {
	callLog.mu.Lock()
	callLog.path = ""
	callLog.records = nil
	callLog.mu.Unlock()

	for i := 0; i < 5; i++ {
		callLog.append(CallRecord{ReqID: string(rune('a' + i))})
	}
	latest := callLog.latest(2)
	if len(latest) != 2 || latest[0].ReqID != "d" || latest[1].ReqID != "e" {
		t.Fatalf("latest = %+v", latest)
	}
	all := callLog.latest(0)
	if len(all) != 5 {
		t.Fatalf("latest(0) = %d", len(all))
	}
	callLog.clear()
	if n := len(callLog.latest(100)); n != 0 {
		t.Fatalf("after clear = %d", n)
	}
}

func TestUsageFromOpenAIBody(t *testing.T) {
	pt, ct, cc, cr := usageFromOpenAIBody([]byte(`{"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33,"prompt_tokens_details":{"cached_tokens":5}}}`))
	if pt != 11 || ct != 22 || cc != 0 || cr != 5 {
		t.Fatalf("pt=%d ct=%d cc=%d cr=%d", pt, ct, cc, cr)
	}
	pt, ct, cc, cr = usageFromOpenAIBody([]byte(`{"usage":{"input_tokens":11,"output_tokens":22,"cache_creation_input_tokens":3,"cache_read_input_tokens":4}}`))
	if pt != 11 || ct != 22 || cc != 3 || cr != 4 {
		t.Fatalf("claude-style pt=%d ct=%d cc=%d cr=%d", pt, ct, cc, cr)
	}
	pt, ct, cc, cr = usageFromOpenAIBody([]byte(`{"error":"x"}`))
	if pt != 0 || ct != 0 || cc != 0 || cr != 0 {
		t.Fatalf("bad body pt=%d ct=%d cc=%d cr=%d", pt, ct, cc, cr)
	}
}

func TestCallRecorderFinish(t *testing.T) {
	callLog.mu.Lock()
	callLog.path = ""
	callLog.records = nil
	callLog.mu.Unlock()

	ctx := beginCallLog(context.Background(), "/v1/chat/completions", "deepseek-v4-flash", false, "public")
	callLogEvent(ctx, "connect_ok", "hk-01", "")
	callLogFinish(ctx, 200, "", 7, 9, 3, 4)

	recs := callLog.latest(10)
	if len(recs) != 1 {
		t.Fatalf("records = %d", len(recs))
	}
	r := recs[0]
	if r.Status != "ok" || r.Model != "deepseek-v4-flash" || r.PromptTokens != 7 || r.CompletionTokens != 9 {
		t.Fatalf("rec = %+v", r)
	}
	if r.CacheCreation != 3 || r.CacheRead != 4 {
		t.Fatalf("cache rec = %+v", r)
	}
	if len(r.Nodes) != 1 || r.Nodes[0] != "hk-01" || len(r.Events) != 1 {
		t.Fatalf("nodes=%v events=%+v", r.Nodes, r.Events)
	}
	if r.DurationMS < 0 || r.ReqID == "" || r.TS == "" {
		t.Fatalf("bad meta: %+v", r)
	}
}
