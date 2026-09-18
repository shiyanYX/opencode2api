package main

// ======================== 上游请求体“agent 形态”适配 ========================
//
// OpenCode Zen 免费层网关按“请求像不像官方 opencode 客户端”鉴权。截至
// 2026-09-18，实测共四道条件，缺一即返回
//
//	403 {"type":"error","error":{"type":"FreeTierError",
//	     "message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}
//
//  1. x-opencode-session 必须是 ses_ + 12 位小写十六进制 + 14 位 base62（见 opencode_id.go）
//  2. User-Agent 必须是 opencode/<semver>，且不低于 1.17.0（我们取 npm latest）
//  3. body.stream 必须为 true
//  4. body.tools 必须包含官方小写工具名 bash / glob / grep / read
//
// 第 3、4 条的实测依据（合规 session + 官方 headers，仅改 body）：
//
//	stream=true  + 上述四个工具名        -> 200
//	stream=false 或缺失                  -> 403
//	缺 glob / grep / bash / read 任一个  -> 403
//	缺 edit 或 write                     -> 200   （因此必需集合只有四个）
//	工具名写成 Bash / Read（大小写不同） -> 403   （大小写敏感）
//	同名工具重复出现                     -> 400   （不是 403，去重是硬要求）
//	四个必需名 + 60 个自定义工具         -> 200   （无数量上限）
//
// 因此本文件做三件事：
//   - 强制 stream=true（非流式客户端由 aggregateUpstreamSSE 本地聚合）；
//   - 把客户端工具名中命中官方名的改写为官方小写名（沿用客户端自己的 schema），
//     去重后补齐缺失的必需工具，并返回 上游名 -> 客户端原名 的回映射；
//   - 响应侧把工具调用名映射回客户端原名，并丢弃“补齐的占位工具”产生的调用，
//     使下游 agent 完全无感（工具名与 schema 契约不变）。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// officialOpenCodeToolNames 官方 opencode 客户端声明的工具名（小写）。
var officialOpenCodeToolNames = []string{
	"bash", "edit", "glob", "grep", "read", "skill", "task",
	"todowrite", "webfetch", "websearch", "write",
}

// requiredOfficialTools 是上游免费层强制要求出现的官方工具名。
// 实测 edit / write 不要求，但这四个缺任一个都是 403。
var requiredOfficialTools = []string{"bash", "glob", "grep", "read"}

func isOfficialToolName(name string) bool {
	l := strings.ToLower(name)
	for _, n := range officialOpenCodeToolNames {
		if l == n {
			return true
		}
	}
	return false
}

func placeholderToolNamed(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": "Internal gateway placeholder. Not available in this session.",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// shapeToolsForUpstream 调整上游请求的 tools，使其满足免费层要求。
// 返回 mapping（上游名 -> 客户端原名，仅含被改名的工具）与
// injected（本次补齐的占位工具名集合，客户端没有对应工具）。
func shapeToolsForUpstream(bodyMap map[string]any) (mapping map[string]string, injected map[string]bool) {
	mapping = map[string]string{}
	injected = map[string]bool{}
	rev := map[string]string{}

	clientTools, _ := bodyMap["tools"].([]any)
	hadClientTools := len(clientTools) > 0

	out := make([]any, 0, len(clientTools)+len(requiredOfficialTools))
	seen := map[string]bool{}

	for _, raw := range clientTools {
		tc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tc["function"].(map[string]any)
		if !ok {
			out = append(out, raw) // 非 function 形状的工具原样保留
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			out = append(out, raw)
			continue
		}
		upName := name
		if isOfficialToolName(name) {
			upName = strings.ToLower(name)
		}
		if seen[upName] {
			continue // 上游对重名工具返回 400，必须去重（Bash 与 bash 视为同一个）
		}
		seen[upName] = true
		if upName == name {
			out = append(out, raw)
			continue
		}
		nf := cloneMap(fn)
		nf["name"] = upName
		ntc := cloneMap(tc)
		ntc["function"] = nf
		out = append(out, ntc)
		mapping[upName] = name
		rev[name] = upName
	}

	for _, req := range requiredOfficialTools {
		if seen[req] {
			continue
		}
		seen[req] = true
		injected[req] = true
		out = append(out, placeholderToolNamed(req))
	}

	bodyMap["tools"] = out

	if !hadClientTools {
		// 客户端完全没带工具：禁止模型调用我们补的占位工具，对客户端完全不可见。
		bodyMap["tool_choice"] = "none"
	} else if tcObj, ok := bodyMap["tool_choice"].(map[string]any); ok {
		// 强制指定某个工具时，名字要跟着改写，保持请求自洽。
		if fn, ok := tcObj["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok {
				if up, ok := rev[n]; ok {
					nf := cloneMap(fn)
					nf["name"] = up
					ntc := cloneMap(tcObj)
					ntc["function"] = nf
					bodyMap["tool_choice"] = ntc
				}
			}
		}
	}
	return mapping, injected
}

// ensureAgentUpstreamShape 把上游请求体调整为免费层要求的形态，返回工具名回映射
// 与补齐的占位工具集合，供响应侧还原/过滤。
func ensureAgentUpstreamShape(bodyMap map[string]any) (map[string]string, map[string]bool) {
	bodyMap["stream"] = true
	// 聚合需要用量，而上游只在 include_usage 时于末尾 chunk 返回 usage。
	if _, ok := bodyMap["stream_options"]; !ok {
		bodyMap["stream_options"] = map[string]any{"include_usage": true}
	}
	return shapeToolsForUpstream(bodyMap)
}

// remapToolCallsInDelta 就地改写 delta 里的工具调用：命中 mapping 的改回客户端原名，
// 命中 injected（客户端没有对应工具）的整条丢弃。返回是否发生改动。
func remapToolCallsInDelta(delta map[string]any, mapping map[string]string, injected map[string]bool) bool {
	tcs, ok := delta["tool_calls"].([]any)
	if !ok || len(tcs) == 0 {
		return false
	}
	kept := make([]any, 0, len(tcs))
	changed := false
	for _, raw := range tcs {
		tc, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		name := ""
		if fn != nil {
			name, _ = fn["name"].(string)
		}
		if name == "" {
			kept = append(kept, raw) // 仅带 arguments 的分片
			continue
		}
		if injected[name] {
			changed = true
			continue
		}
		if client, ok := mapping[name]; ok {
			nf := cloneMap(fn)
			nf["name"] = client
			ntc := cloneMap(tc)
			ntc["function"] = nf
			kept = append(kept, ntc)
			changed = true
			continue
		}
		kept = append(kept, raw)
	}
	if changed {
		if len(kept) == 0 {
			delete(delta, "tool_calls")
		} else {
			delta["tool_calls"] = kept
		}
	}
	return changed
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
// chat.completion 响应（上游现在是 stream=true，非流式客户端需要本地聚合）。
// 非 SSE 输入原样返回，保证错误体等场景行为不变。
func aggregateUpstreamSSE(body []byte, model string, mapping map[string]string, injected map[string]bool) []byte {
	if !looksLikeSSE(body) {
		return body
	}

	type toolAcc struct {
		ID    string
		Type  string
		Name  string
		Index int
		Args  strings.Builder
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

		remapToolCallsInDelta(delta, mapping, injected)
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
				acc = &toolAcc{Index: idx}
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
			continue // 占位工具的参数分片：名字已被丢弃，这里自然跳过
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

// ======================== 流式工具名回映射 ========================

// toolNameRewriter 包装上游 SSE 响应体，在转发前把工具调用名映射回客户端原名，
// 并丢弃占位工具产生的调用。逐行处理，保持 SSE 分帧不变。
type toolNameRewriter struct {
	src      io.ReadCloser
	br       *bufio.Reader
	buf      bytes.Buffer
	mapping  map[string]string
	injected map[string]bool
	eof      bool
}

func newToolNameRewriter(src io.ReadCloser, mapping map[string]string, injected map[string]bool) io.ReadCloser {
	if len(mapping) == 0 && len(injected) == 0 {
		return src
	}
	return &toolNameRewriter{src: src, br: bufio.NewReader(src), mapping: mapping, injected: injected}
}

func (r *toolNameRewriter) rewriteLine(line string) string {
	trimmed := strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(trimmed, "data:") {
		return line
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return line
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return line
	}
	choices, _ := chunk["choices"].([]any)
	changed := false
	for _, c := range choices {
		choice, _ := c.(map[string]any)
		if choice == nil {
			continue
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			if remapToolCallsInDelta(delta, r.mapping, r.injected) {
				changed = true
			}
		}
	}
	if !changed {
		return line
	}
	out, err := json.Marshal(chunk)
	if err != nil {
		return line
	}
	return "data: " + string(out) + line[len(trimmed):]
}

func (r *toolNameRewriter) Read(p []byte) (int, error) {
	for r.buf.Len() == 0 {
		if r.eof {
			return 0, io.EOF
		}
		line, err := r.br.ReadString('\n')
		if err != nil {
			r.eof = true
		}
		if line != "" {
			r.buf.WriteString(r.rewriteLine(line))
		}
		if err != nil && r.buf.Len() == 0 {
			return 0, err
		}
	}
	return r.buf.Read(p)
}

func (r *toolNameRewriter) Close() error {
	return r.src.Close()
}
