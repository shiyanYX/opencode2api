package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func clientTool(name string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": "d", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}
}

func toolNames(tools []any) []string {
	out := make([]string, 0, len(tools))
	for _, raw := range tools {
		tc, _ := raw.(map[string]any)
		fn, _ := tc["function"].(map[string]any)
		n, _ := fn["name"].(string)
		out = append(out, n)
	}
	return out
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// 上游免费层要求 tools 至少包含官方小写名 bash/glob/grep/read（实测 edit/write 不要求）。
func TestShapeToolsInjectsRequiredOfficialNames(t *testing.T) {
	body := map[string]any{"model": "m"}
	_, injected := shapeToolsForUpstream(body)

	names := toolNames(body["tools"].([]any))
	for _, req := range requiredOfficialTools {
		if !hasName(names, req) {
			t.Fatalf("缺少必需工具 %q，实际 %v", req, names)
		}
		if !injected[req] {
			t.Fatalf("补齐的 %q 应记为 injected", req)
		}
	}
	if body["tool_choice"] != "none" {
		t.Fatalf("客户端无工具时应设 tool_choice=none，实际 %v", body["tool_choice"])
	}
}

// 客户端自带工具时必须保留，且不能设 tool_choice=none（否则会禁掉客户端自己的工具）。
func TestShapeToolsKeepsClientToolsAndChoice(t *testing.T) {
	body := map[string]any{
		"tools":       []any{clientTool("alpha"), clientTool("beta")},
		"tool_choice": "auto",
	}
	shapeToolsForUpstream(body)

	names := toolNames(body["tools"].([]any))
	for _, want := range []string{"alpha", "beta"} {
		if !hasName(names, want) {
			t.Fatalf("客户端工具 %q 丢失，实际 %v", want, names)
		}
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("不应覆盖客户端 tool_choice，实际 %v", body["tool_choice"])
	}
}

// 大小写命中官方名时改写成官方小写名，并记录回映射；客户端 schema 原样保留。
func TestShapeToolsNormalizesCaseAndMapsBack(t *testing.T) {
	body := map[string]any{"tools": []any{clientTool("Bash"), clientTool("Glob"), clientTool("Grep"), clientTool("Read")}}
	mapping, injected := shapeToolsForUpstream(body)

	names := toolNames(body["tools"].([]any))
	for _, want := range []string{"bash", "glob", "grep", "read"} {
		if !hasName(names, want) {
			t.Fatalf("应改写成官方小写名 %q，实际 %v", want, names)
		}
	}
	if mapping["bash"] != "Bash" || mapping["read"] != "Read" {
		t.Fatalf("回映射错误: %v", mapping)
	}
	if len(injected) != 0 {
		t.Fatalf("客户端已覆盖全部必需工具，不应注入: %v", injected)
	}
}

// 上游对重名工具返回 400，因此去重是硬要求（Bash 与 bash 视为同一个）。
func TestShapeToolsDedupesCaseInsensitively(t *testing.T) {
	body := map[string]any{"tools": []any{clientTool("Bash"), clientTool("bash"), clientTool("BASH")}}
	shapeToolsForUpstream(body)

	names := toolNames(body["tools"].([]any))
	count := 0
	for _, n := range names {
		if strings.EqualFold(n, "bash") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("bash 应只出现一次，实际 %d 次: %v", count, names)
	}
	// 其余必需工具仍应补齐
	for _, req := range requiredOfficialTools {
		if !hasName(names, req) {
			t.Fatalf("缺少必需工具 %q: %v", req, names)
		}
	}
}

// 非官方名的客户端工具必须原样保留。
func TestShapeToolsLeavesUnknownToolsAlone(t *testing.T) {
	body := map[string]any{"tools": []any{clientTool("MCPSearch"), clientTool("NotebookEdit")}}
	mapping, _ := shapeToolsForUpstream(body)

	names := toolNames(body["tools"].([]any))
	for _, want := range []string{"MCPSearch", "NotebookEdit"} {
		if !hasName(names, want) {
			t.Fatalf("未知工具 %q 应原样保留，实际 %v", want, names)
		}
	}
	if len(mapping) != 0 {
		t.Fatalf("未改名的工具不应产生映射: %v", mapping)
	}
}

// 强制指定某个工具时，tool_choice 里的名字要跟着改写，保持请求自洽。
func TestShapeToolsRemapsToolChoiceName(t *testing.T) {
	body := map[string]any{
		"tools":       []any{clientTool("Bash")},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "Bash"}},
	}
	shapeToolsForUpstream(body)

	tc, _ := body["tool_choice"].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Fatalf("tool_choice 未同步改名: %v", fn["name"])
	}
}

func TestEnsureAgentUpstreamShapeForcesStream(t *testing.T) {
	body := map[string]any{"stream": false}
	ensureAgentUpstreamShape(body)
	if body["stream"] != true {
		t.Fatalf("stream 应被强制为 true")
	}
}

// 非流式聚合：客户端原始名的工具调用要保留，占位注入的调用要丢弃。
func TestAggregateUpstreamSSERemapsAndDropsInjected(t *testing.T) {
	sse := "data: {\"id\":\"g\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[" +
		"{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}," +
		"{\"index\":1,\"id\":\"c2\",\"type\":\"function\",\"function\":{\"name\":\"grep\",\"arguments\":\"{}\"}}]}}]}\n\n"

	out := aggregateUpstreamSSE([]byte(sse), "m", map[string]string{"bash": "Bash"}, map[string]bool{"grep": true})
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("应只剩 1 个客户端工具调用，实际 %d: %v", len(tcs), tcs)
	}
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "Bash" {
		t.Fatalf("工具名应回映射为客户端原名 Bash，实际 %v", fn["name"])
	}
}

// 流式改写器：逐行改写 SSE 里的工具名，且不改动非工具行。
func TestToolNameRewriterRewritesSSE(t *testing.T) {
	in := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	r := newToolNameRewriter(io.NopCloser(strings.NewReader(in)), map[string]string{"bash": "Bash"}, nil)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, `"name":"Bash"`) {
		t.Fatalf("工具名未改写: %s", s)
	}
	if !strings.Contains(s, `"content":"hi"`) {
		t.Fatalf("内容行被破坏: %s", s)
	}
	if !strings.Contains(s, "data: [DONE]") {
		t.Fatalf("DONE 行丢失: %s", s)
	}
	if strings.Count(s, "\n\n") != 3 {
		t.Fatalf("SSE 分帧被破坏: %q", s)
	}
}

// 非 SSE 响应必须原样透传（错误体等）。
func TestAggregateUpstreamSSEPassesThroughPlainJSON(t *testing.T) {
	plain := []byte(`{"error":{"type":"FreeTierError","message":"x"}}`)
	if got := aggregateUpstreamSSE(plain, "m", nil, nil); string(got) != string(plain) {
		t.Fatalf("非 SSE 应原样透传，实际 %s", got)
	}
}
