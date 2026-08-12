package main

import (
	"context"
	"testing"
)

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
	pt, ct := usageFromOpenAIBody([]byte(`{"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`))
	if pt != 11 || ct != 22 {
		t.Fatalf("pt=%d ct=%d", pt, ct)
	}
	pt, ct = usageFromOpenAIBody([]byte(`{"error":"x"}`))
	if pt != 0 || ct != 0 {
		t.Fatalf("bad body pt=%d ct=%d", pt, ct)
	}
}

func TestCallRecorderFinish(t *testing.T) {
	callLog.mu.Lock()
	callLog.path = ""
	callLog.records = nil
	callLog.mu.Unlock()

	ctx := beginCallLog(context.Background(), "/v1/chat/completions", "deepseek-v4-flash", false, "public")
	callLogEvent(ctx, "connect_ok", "hk-01", "")
	callLogFinish(ctx, 200, "", 7, 9)

	recs := callLog.latest(10)
	if len(recs) != 1 {
		t.Fatalf("records = %d", len(recs))
	}
	r := recs[0]
	if r.Status != "ok" || r.Model != "deepseek-v4-flash" || r.PromptTokens != 7 || r.CompletionTokens != 9 {
		t.Fatalf("rec = %+v", r)
	}
	if len(r.Nodes) != 1 || r.Nodes[0] != "hk-01" || len(r.Events) != 1 {
		t.Fatalf("nodes=%v events=%+v", r.Nodes, r.Events)
	}
	if r.DurationMS < 0 || r.ReqID == "" || r.TS == "" {
		t.Fatalf("bad meta: %+v", r)
	}
}
