package main

import (
	"encoding/json"
	"testing"
)

// FreeTierError 的 message 含 "free tier" 子串，曾被"免费模型已下线"改写规则
// 误伤，把"客户端身份被拒"显示成"模型已停止服务"，掩盖真实原因。
// 回归：FreeTierError 必须原样透传。
func TestRewriteUpstreamErrorLeavesFreeTierErrorIntact(t *testing.T) {
	in := []byte(`{"type":"error","error":{"type":"FreeTierError","message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}`)
	out := rewriteUpstreamError(in)

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%s)", err, out)
	}
	errObj, _ := got["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("缺少 error 对象: %s", out)
	}
	if errObj["type"] != "FreeTierError" {
		t.Fatalf("error.type 被改写为 %v，期望 FreeTierError", errObj["type"])
	}
	if msg, _ := errObj["message"].(string); msg != "Error from provider (Console): OpenCode's free tier can only be used from within OpenCode" {
		t.Fatalf("error.message 被改写为 %q，期望原样透传", msg)
	}
}

// 真正的"免费模型下线"错误仍应被改写为可读提示。
func TestRewriteUpstreamErrorStillRewritesRetiredFreeModel(t *testing.T) {
	in := []byte(`{"error":{"type":"invalid_request_error","message":"This free promotion has ended for this model"}}`)
	out := rewriteUpstreamError(in)

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%s)", err, out)
	}
	errObj, _ := got["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("缺少 error 对象: %s", out)
	}
	if errObj["type"] != "free_model_ended" {
		t.Fatalf("error.type = %v，期望 free_model_ended", errObj["type"])
	}
}
