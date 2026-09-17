package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnsureAgentUpstreamShapeForcesStreamAndInjectsTool(t *testing.T) {
	body := map[string]any{
		"model":    "mimo-v2.5-free",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   false,
	}
	injected := ensureAgentUpstreamShape(body)
	if !injected {
		t.Fatalf("未注入占位工具")
	}
	if body["stream"] != true {
		t.Fatalf("stream 未被强制为 true: %v", body["stream"])
	}
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools 未注入: %v", body["tools"])
	}
	if body["tool_choice"] != "none" {
		t.Fatalf("注入占位工具时必须设 tool_choice=none，实际 %v", body["tool_choice"])
	}
}

func TestEnsureAgentUpstreamShapeLeavesClientToolsAlone(t *testing.T) {
	clientTools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "bash"}}}
	body := map[string]any{
		"model":       "mimo-v2.5-free",
		"tools":       clientTools,
		"tool_choice": "auto",
		"stream":      true,
	}
	injected := ensureAgentUpstreamShape(body)
	if injected {
		t.Fatalf("客户端已有 tools 时不应注入")
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("不应覆盖客户端 tool_choice，实际 %v", body["tool_choice"])
	}
	if len(body["tools"].([]any)) != 1 {
		t.Fatalf("客户端 tools 被改动: %v", body["tools"])
	}
}

func TestEnsureAgentUpstreamShapeTreatsEmptyToolsAsMissing(t *testing.T) {
	body := map[string]any{"tools": []any{}, "stream": true}
	if !ensureAgentUpstreamShape(body) {
		t.Fatalf("tools 为空数组时应视为缺失并注入")
	}
}

func TestStripPlaceholderToolCalls(t *testing.T) {
	delta := map[string]any{"tool_calls": []any{
		map[string]any{"index": 0, "function": map[string]any{"name": placeholderToolName, "arguments": "{}"}},
		map[string]any{"index": 1, "function": map[string]any{"name": "bash", "arguments": "{}"}},
	}}
	stripPlaceholderToolCalls(delta)
	tc, _ := delta["tool_calls"].([]any)
	if len(tc) != 1 {
		t.Fatalf("应只留下 1 个真实工具调用，实际 %d", len(tc))
	}
	fn := tc[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Fatalf("留下了错误的工具调用: %v", fn["name"])
	}
}

func TestAggregateUpstreamSSE(t *testing.T) {
	sse := "data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"mimo-v2.5-free\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"He\"}}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"mimo-v2.5-free\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"think\",\"content\":\"llo\"}}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"mimo-v2.5-free\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"cmd\\\":\"}}]}}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"mimo-v2.5-free\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"ls\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"mimo-v2.5-free\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n" +
		"data: [DONE]\n\n"

	out := aggregateUpstreamSSE([]byte(sse), "mimo-v2.5-free")
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

func TestAggregateUpstreamSSEFiltersPlaceholderAndHandlesPlainJSON(t *testing.T) {
	sse := "data: {\"id\":\"g\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"" + placeholderToolName + "\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"
	out := aggregateUpstreamSSE([]byte(sse), "m")
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, has := msg["tool_calls"]; has {
		t.Fatalf("占位工具调用未被过滤: %v", msg["tool_calls"])
	}

	// 非 SSE 输入（例如上游直接返回错误 JSON）必须原样透传。
	plain := []byte(`{"error":{"type":"FreeTierError","message":"x"}}`)
	if got := aggregateUpstreamSSE(plain, "m"); string(got) != string(plain) {
		t.Fatalf("非 SSE 输入应原样透传，实际 %s", got)
	}
	if !strings.Contains(string(aggregateUpstreamSSE(plain, "m")), "FreeTierError") {
		t.Fatalf("错误信息丢失")
	}
}
