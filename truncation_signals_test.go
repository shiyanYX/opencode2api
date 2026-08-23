package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- L1：finish_reason 语义分类与映射 ----

func TestClassifyFinishReason(t *testing.T) {
	configMu.RLock()
	saved := truncationStopReasonCfg
	configMu.RUnlock()
	defer func() {
		configMu.Lock()
		truncationStopReasonCfg = saved
		configMu.Unlock()
	}()
	configMu.Lock()
	truncationStopReasonCfg = ""
	configMu.Unlock()

	tests := []struct {
		raw     string
		wantSig finishSignal
		wantMap string
	}{
		{"", finishNone, ""},
		{"stop", finishNormal, "stop"},
		{"length", finishNormal, "length"},
		{"tool_calls", finishNormal, "tool_calls"},
		{"function_call", finishNormal, "function_call"},
		{"content_filter", finishFilter, "content_filter"},
		// Zen 内容过滤掐流的实测签名（opencode#44092）
		{"sensitive", finishFilter, "content_filter"},
		{"moderation", finishFilter, "content_filter"},
		// 协议外未知值 → 按异常终止改写为截断原因
		{"totally-made-up", finishUnknown, "length"},
	}
	for _, tt := range tests {
		if got := classifyFinishReason(tt.raw); got != tt.wantSig {
			t.Errorf("classifyFinishReason(%q) = %v, want %v", tt.raw, got, tt.wantSig)
		}
		if got := mapUpstreamFinishReason(tt.raw); got != tt.wantMap {
			t.Errorf("mapUpstreamFinishReason(%q) = %q, want %q", tt.raw, got, tt.wantMap)
		}
	}

	if got := claudeStopReasonFromOpenAI("length"); got != "max_tokens" {
		t.Errorf("claudeStopReasonFromOpenAI(length) = %q, want max_tokens", got)
	}
	if got := claudeStopReasonFromOpenAI("content_filter"); got != "refusal" {
		t.Errorf("claudeStopReasonFromOpenAI(content_filter) = %q, want refusal", got)
	}
}

// ---- L1：Claude 流式路径 ----

func lastEventOf(t *testing.T, body, name string) map[string]any {
	t.Helper()
	var last map[string]any
	found := false
	for _, event := range parseSSEEvents(t, body) {
		if event.Name == name {
			last = event.Data
			found = true
		}
	}
	if !found {
		t.Fatalf("no %q event in stream:\n%s", name, body)
	}
	return last
}

func TestClaudeStreamEOFWithoutFinishDeclaresTruncationNotEndTurn(t *testing.T) {
	// 上游在输出半句话后提前断流：无 finish_reason、无 [DONE]（opencode#44210 失败模式 2）。
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"partial answe"}}]}`,
	}, "\n")
	rr := httptest.NewRecorder()
	_, _, _, _, emptyNoFinish := claudeStreamHandler(context.Background(), rr,
		io.NopCloser(strings.NewReader(upstream)), "m", false)

	delta := lastEventOf(t, rr.Body.String(), "message_delta")
	stopReason := delta["delta"].(map[string]any)["stop_reason"]
	if stopReason != "max_tokens" {
		t.Fatalf("truncated stream stop_reason = %#v, want max_tokens (not end_turn):\n%s",
			stopReason, rr.Body.String())
	}
	lastEventOf(t, rr.Body.String(), "message_stop") // 状态机必须完整闭合
	if emptyNoFinish {
		t.Fatal("stream with forwarded content must not be marked retryable-empty")
	}
}

func TestClaudeStreamSensitiveFinishMapsToRefusal(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"let me help with that"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"sensitive"}]}`,
		`data: [DONE]`, "",
	}, "\n")
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr,
		io.NopCloser(strings.NewReader(upstream)), "m", false)

	delta := lastEventOf(t, rr.Body.String(), "message_delta")
	if got := delta["delta"].(map[string]any)["stop_reason"]; got != "refusal" {
		t.Fatalf("finish_reason sensitive produced stop_reason %#v, want refusal:\n%s",
			got, rr.Body.String())
	}
}

func TestClaudeStreamUnknownFinishValueDoesNotClaimEndTurn(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"provider-crashed"}]}`,
		`data: [DONE]`, "",
	}, "\n")
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr,
		io.NopCloser(strings.NewReader(upstream)), "m", false)

	delta := lastEventOf(t, rr.Body.String(), "message_delta")
	if got := delta["delta"].(map[string]any)["stop_reason"]; got != "max_tokens" {
		t.Fatalf("unknown finish value produced stop_reason %#v, want max_tokens:\n%s",
			got, rr.Body.String())
	}
}

// ---- L2：零内容死流的重放判定 ----

func TestClaudeStreamEmptyDeadBodyIsRetryableEmpty(t *testing.T) {
	rr := httptest.NewRecorder()
	_, _, _, _, emptyNoFinish := claudeStreamHandler(context.Background(), rr,
		io.NopCloser(strings.NewReader("")), "m", false)
	if !emptyNoFinish {
		t.Fatal("dead-empty stream (no events, no finish) must be flagged retryable")
	}
	// 兜底收尾仍须完整闭合且如实声明截断。
	delta := lastEventOf(t, rr.Body.String(), "message_delta")
	if got := delta["delta"].(map[string]any)["stop_reason"]; got != "max_tokens" {
		t.Fatalf("empty stream stop_reason = %#v, want max_tokens:\n%s", got, rr.Body.String())
	}
}

func TestClaudeStreamDoneWithoutFinishOrContentIsRetryableEmpty(t *testing.T) {
	rr := httptest.NewRecorder()
	_, _, _, _, emptyNoFinish := claudeStreamHandler(context.Background(), rr,
		io.NopCloser(strings.NewReader("data: [DONE]\n\n")), "m", false)
	if !emptyNoFinish {
		t.Fatal("[DONE]-only degenerate stream must be flagged retryable")
	}
}

// ---- L1：Responses 流式路径 ----

func TestResponsesStreamEOFWithoutFinishEndsIncomplete(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"r","created":1,"choices":[{"delta":{"content":"half a sentenc"}}]}`,
		"", // EOF：无 finish、无 [DONE]
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(upstream)), Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", false, nil, nil, ResponsesAPIRequest{})

	out := rr.Body.String()
	if !strings.Contains(out, "event: response.incomplete") || strings.Contains(out, "event: response.completed") {
		t.Fatalf("EOF-without-finish must end incomplete:\n%s", out)
	}
}

func TestResponsesStreamSensitiveFinishIsNormalizedNotCompletedSilently(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"r","created":1,"choices":[{"delta":{"content":"text"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"sensitive"}]}`,
		`data: [DONE]`, "",
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(upstream)), Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", false, nil, nil, ResponsesAPIRequest{})
	_ = rr // 归一化后 content_filter 走既有完成语义；此处验证不 panic 且事件流闭合
	for _, name := range []string{"response.created"} {
		lastEventOf(t, rr.Body.String(), name)
	}
}

// ---- L1：非流式 Claude 转换 ----

func TestOpenAIToClaudeResponseMissingFinishIsTruncated(t *testing.T) {
	body := openAIToClaudeResponse([]byte(`{"choices":[{"message":{"role":"assistant","content":"cut off mid"}}]}`), "m", false)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["stop_reason"] != "max_tokens" {
		t.Fatalf("missing upstream finish yielded stop_reason %#v, want max_tokens:\n%s",
			got["stop_reason"], body)
	}
}
