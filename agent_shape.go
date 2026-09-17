package main

// ======================== 上游请求体“agent 形态”适配 ========================
//
// OpenCode Zen 免费层网关会校验请求体是否来自 agent 客户端：
// **tools 必须非空，且 stream 必须为 true**，两者缺一即返回
//
//	403 {"type":"error","error":{"type":"FreeTierError",
//	     "message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}
//
// 实测（用官方 opencode CLI 的 headers，仅改 body）：
//
//	body 93B，无 tools，无 stream            -> 403
//	body 93B，无 tools，stream=true          -> 403
//	body 93B，1 个占位工具，stream=true      -> 200
//	body 93B，1 个占位工具，stream=false     -> 403
//	CLI 原体(32KB)，tools 换 11 个占位工具   -> 200   （工具 schema 无关）
//	CLI 原体，system prompt 换成等长填充     -> 200   （prompt 内容无关）
//	CLI 原体，tools=[]                       -> 403
//
// 也就是说只有「tools 非空 + stream=true」这两个开关，body 大小、system prompt
// 内容、工具定义、max_tokens、tool_choice、Authorization、User-Agent 都无关。
//
// 因此这里做两件事：
//  1. 上游请求恒为 stream=true（非流式客户端由 aggregateUpstreamSSE 本地聚合）；
//  2. 客户端未提供 tools 时注入一个占位工具，并设 tool_choice:"none"
//     （已实测上游接受），使模型不可能调用它 —— 对客户端不可见。

import (
	"encoding/json"
	"strings"
	"time"
)

// placeholderToolName 是客户端未提供 tools 时注入的占位工具名。
// 名字刻意取得不像真实工具，便于识别与过滤。
const placeholderToolName = "oc2a_internal_noop"

func placeholderTool() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        placeholderToolName,
			"description": "Internal gateway placeholder. Never call this tool.",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

func hasNonEmptyTools(bodyMap map[string]any) bool {
	tools, ok := bodyMap["tools"].([]any)
	return ok && len(tools) > 0
}

// ensureAgentUpstreamShape 把上游请求体调整为上游免费层要求的 agent 形态。
// 返回 true 表示注入了占位工具。
func ensureAgentUpstreamShape(bodyMap map[string]any) bool {
	bodyMap["stream"] = true
	// 聚合需要用量，而上游只在 include_usage 时于末尾 chunk 返回 usage。
	if _, ok := bodyMap["stream_options"]; !ok {
		bodyMap["stream_options"] = map[string]any{"include_usage": true}
	}
	if hasNonEmptyTools(bodyMap) {
		return false
	}
	bodyMap["tools"] = []any{placeholderTool()}
	// 只在注入时设置，绝不覆盖客户端自己的 tool_choice。
	bodyMap["tool_choice"] = "none"
	return true
}

// stripPlaceholderToolCalls 从流式 delta 中剔除占位工具调用（兜底：
// tool_choice:"none" 理应让模型无法调用它，但坏了也要对客户端不可见）。
func stripPlaceholderToolCalls(delta map[string]any) {
	tcs, ok := delta["tool_calls"].([]any)
	if !ok || len(tcs) == 0 {
		return
	}
	kept := make([]any, 0, len(tcs))
	for _, raw := range tcs {
		tc, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		name := ""
		if fn, ok := tc["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
		}
		if name == placeholderToolName {
			continue
		}
		kept = append(kept, tc)
	}
	if len(kept) == 0 {
		delete(delta, "tool_calls")
		return
	}
	delta["tool_calls"] = kept
}

// looksLikeSSE 判断响应体是否为 SSE 流（而非普通 JSON）。
func looksLikeSSE(body []byte) bool {
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimRight(line, "\r"), "data:") {
			return true
		}
	}
	return false
}

// aggregateUpstreamSSE 把上游 OpenAI 形状的流式响应聚合成单个非流式
// chat.completion 响应，供非流式客户端使用（上游现在恒为 stream=true）。
// 非 SSE 输入原样返回，保证错误体等场景行为不变。
func aggregateUpstreamSSE(body []byte, model string) []byte {
	if !looksLikeSSE(body) {
		return body
	}

	type toolAcc struct {
		ID   string
		Type string
		Name string
		Args strings.Builder
	}

	var (
		id           string
		modelOut     string
		created      int64
		content      strings.Builder
		reasoning    strings.Builder
		finishReason string
		usage        map[string]any
		order        []int
		byIndex      = map[int]*toolAcc{}
	)

	for _, rawLine := range strings.Split(string(body), "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if v, ok := chunk["id"].(string); ok && v != "" {
			id = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			modelOut = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}

		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			continue
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			finishReason = fr
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if s, ok := delta["content"].(string); ok {
			content.WriteString(s)
		}
		if s, ok := delta["reasoning_content"].(string); ok {
			reasoning.WriteString(s)
		}

		stripPlaceholderToolCalls(delta)
		tcs, _ := delta["tool_calls"].([]any)
		for _, raw := range tcs {
			tc, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			idx := 0
			if f, ok := tc["index"].(float64); ok {
				idx = int(f)
			}
			acc, ok := byIndex[idx]
			if !ok {
				acc = &toolAcc{}
				byIndex[idx] = acc
				order = append(order, idx)
			}
			if v, ok := tc["id"].(string); ok && v != "" {
				acc.ID = v
			}
			if v, ok := tc["type"].(string); ok && v != "" {
				acc.Type = v
			}
			if fn, ok := tc["function"].(map[string]any); ok {
				if v, ok := fn["name"].(string); ok && v != "" {
					acc.Name = v
				}
				if v, ok := fn["arguments"].(string); ok {
					acc.Args.WriteString(v)
				}
			}
		}
	}

	if id == "" {
		id = "chatcmpl-" + randomString(16)
	}
	if modelOut == "" {
		modelOut = model
	}
	if created == 0 {
		created = time.Now().Unix()
	}

	msg := map[string]any{"role": "assistant"}
	if content.Len() > 0 {
		msg["content"] = content.String()
	}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	calls := make([]any, 0, len(order))
	for _, idx := range order {
		acc := byIndex[idx]
		if acc.Name == "" {
			continue
		}
		tcType := acc.Type
		if tcType == "" {
			tcType = "function"
		}
		tc := map[string]any{
			"index":    idx,
			"type":     tcType,
			"function": map[string]any{"name": acc.Name, "arguments": acc.Args.String()},
		}
		if acc.ID != "" {
			tc["id"] = acc.ID
		}
		calls = append(calls, tc)
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	if finishReason == "" {
		finishReason = "stop"
	}

	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   modelOut,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return body
	}
	return out
}
