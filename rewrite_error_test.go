package main

import (
	"encoding/json"
	"testing"
)

func TestRewriteUpstreamError_FreePromotionEnded(t *testing.T) {
	// Anthropic 格式
	input := []byte(`{"type":"error","error":{"type":"ModelError","message":"Free promotion has ended for DeepSeek V4 Flash Free. You can continue using the model by subscribing to OpenCode Go - https://opencode.ai/go"}}`)
	got := rewriteUpstreamError(input)

	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	errObj := obj["error"].(map[string]any)
	msg := errObj["message"].(string)
	if msg == "" {
		t.Fatal("message should not be empty")
	}
	if msg == "Free promotion has ended for DeepSeek V4 Flash Free. You can continue using the model by subscribing to OpenCode Go - https://opencode.ai/go" {
		t.Fatal("message should have been rewritten, not original")
	}
	if errObj["type"] != "free_model_ended" {
		t.Fatalf("type should be free_model_ended, got %v", errObj["type"])
	}
	if errObj["original_type"] != "ModelError" {
		t.Fatalf("original_type should be ModelError, got %v", errObj["original_type"])
	}
}

func TestRewriteUpstreamError_OpenAIFreeModelEnded(t *testing.T) {
	// OpenAI 格式
	input := []byte(`{"error":{"message":"This model is not available for free users","type":"invalid_request_error"}}`)
	got := rewriteUpstreamError(input)

	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errObj := obj["error"].(map[string]any)
	msg := errObj["message"].(string)
	if msg == "This model is not available for free users" {
		t.Fatal("message should have been rewritten")
	}
	if errObj["type"] != "free_model_ended" {
		t.Fatalf("type should be free_model_ended, got %v", errObj["type"])
	}
}

func TestRewriteUpstreamError_NotFreeEnded_NotRewritten(t *testing.T) {
	// 普通错误不应被改写
	input := []byte(`{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`)
	got := rewriteUpstreamError(input)
	if string(got) != string(input) {
		t.Fatalf("non-free-ended errors should not be rewritten\ngot:  %s\nwant: %s", got, input)
	}
}

func TestRewriteUpstreamError_EmptyBody(t *testing.T) {
	got := rewriteUpstreamError(nil)
	if got != nil {
		t.Fatalf("nil body should return nil, got %v", got)
	}
	got = rewriteUpstreamError([]byte{})
	if len(got) != 0 {
		t.Fatalf("empty body should return empty, got %v", got)
	}
}

func TestRewriteUpstreamError_InvalidJSON(t *testing.T) {
	input := []byte("not json at all")
	got := rewriteUpstreamError(input)
	if string(got) != string(input) {
		t.Fatalf("invalid JSON should be returned as-is")
	}
}

func TestRewriteUpstreamError_FreeTierPattern(t *testing.T) {
	input := []byte(`{"error":{"message":"Access denied: free tier quota exceeded for this model","type":"access_error"}}`)
	got := rewriteUpstreamError(input)

	var obj map[string]any
	json.Unmarshal(got, &obj)
	errObj := obj["error"].(map[string]any)
	msg := errObj["message"].(string)
	if msg == "Access denied: free tier quota exceeded for this model" {
		t.Fatal("free tier message should have been rewritten")
	}
}

func TestRewriteUpstreamError_ClaudeFormat_V2(t *testing.T) {
	// Anthropic v2 格式（error 字段内无 type）
	input := []byte(`{"type":"error","error":{"message":"No longer available for free"}}`)
	got := rewriteUpstreamError(input)

	var obj map[string]any
	json.Unmarshal(got, &obj)
	errObj := obj["error"].(map[string]any)
	if errObj["type"] != "free_model_ended" {
		t.Fatalf("type should be free_model_ended, got %v", errObj["type"])
	}
}
