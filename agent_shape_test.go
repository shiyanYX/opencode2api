package main

import (
	"encoding/json"
	"testing"
)

// 聚合上游 SSE 时内容/推理/工具/用量都要正确还原。
func TestAggregateUpstreamSSE(t *testing.T) {
	sse := "data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"mimo-v2.5-free\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"He\"}}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"mimo-v2.5-free\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"think\",\"content\":\"llo\"}}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"mimo-v2.5-free\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"cmd\\\":\"}}]}}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"mimo-v2.5-free\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"ls\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"mimo-v2.5-free\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n" +
		"data: [DONE]\n\n"

	out := aggregateUpstreamSSE([]byte(sse), "mimo-v2.5-free", nil, nil)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("聚合结果不是合法 JSON: %v\n%s", err, out)
	}
	if got["object"] != "chat.completion" {
		t.Fatalf("object = %v", got["object"])
	}
	if got["id"] != "gen-1" {
		t.Fatalf("id = %v", got["id"])
	}
	choices, _ := got["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices 数量 = %d", len(choices))
	}
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hello" {
		t.Fatalf("content = %q，期望 Hello", msg["content"])
	}
	if msg["reasoning_content"] != "think" {
		t.Fatalf("reasoning_content = %q", msg["reasoning_content"])
	}
	tc := msg["tool_calls"].([]any)
	if len(tc) != 1 {
		t.Fatalf("tool_calls 数量 = %d", len(tc))
	}
	fn := tc[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" || fn["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("tool_call 参数拼接错误: %v", fn)
	}
	usage, _ := got["usage"].(map[string]any)
	if usage == nil || usage["total_tokens"].(float64) != 10 {
		t.Fatalf("usage 未透传: %v", got["usage"])
	}
}

// 没有 finish_reason 的流要兜底成 stop，否则下游拿不到终止原因。
func TestAggregateUpstreamSSEDefaultsFinishReason(t *testing.T) {
	sse := "data: {\"id\":\"g\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n"
	var got map[string]any
	if err := json.Unmarshal(aggregateUpstreamSSE([]byte(sse), "m", nil, nil), &got); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	choice := got["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v，期望 stop", choice["finish_reason"])
	}
}
