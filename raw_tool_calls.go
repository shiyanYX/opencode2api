package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"io"
	"log/slog"
	"strings"
)

// ======================== 原始 DSML/Qwen 工具调用转换 ========================
//
// DeepSeek/Qwen 系模型有时会输出原始 DSML/XML 工具调用标记
// （<｜DSML｜tool_calls>、<|DSML|tool_calls>、<tool_calls>、<tool_call> 等），
// 而不是标准 OpenAI tool_calls。本层在响应到达下游前把标记转换为标准结构：
// 非流式在 callOpenCodeAPI 中归一化，流式由 wrapRawSSE 包装，
// Chat/Claude/Responses 三条协议面共用同一转换层。
// 原生 tool_calls、usage、reasoning_content、finish_reason 与 [DONE] 语义均保留。

const (
	dsmlBar             = "\uff5c"
	dsmlOpen            = "<" + dsmlBar + "DSML" + dsmlBar + "tool_calls>"
	dsmlClose           = "</" + dsmlBar + "DSML" + dsmlBar + "tool_calls>"
	dsmlInvoke          = "<" + dsmlBar + "DSML" + dsmlBar + "invoke"
	dsmlInvokeEnd       = "</" + dsmlBar + "DSML" + dsmlBar + "invoke>"
	dsmlParam           = "<" + dsmlBar + "DSML" + dsmlBar + "parameter "
	dsmlParamEnd        = "</" + dsmlBar + "DSML" + dsmlBar + "parameter>"
	dsmlOpenAlt         = "<|DSML|tool_calls>"
	dsmlCloseAlt        = "</|DSML|tool_calls>"
	dsmlOpenColon       = "<DSML>tool_calls>"
	dsmlCloseColon      = "</DSML:tool_calls>"
	dsmlCloseColonAlt   = "</DSML>tool_calls>"
	dsmlCloseColonSpace = "</DSML tool_calls>"

	rawToolOpen     = "<tool_calls>"
	rawToolClose    = "</tool_calls>"
	rawQwenOpen     = "<tool_call>"
	rawQwenClose    = "</tool_call>"
	rawFunctionOpen = "<function="
	rawFunctionEnd  = "</function>"
)

var rawToolOpenMarkers = []string{
	dsmlOpen,
	dsmlOpenAlt,
	dsmlOpenColon,
	rawToolOpen,
	rawQwenOpen,
	rawFunctionOpen,
}

var rawToolFragmentMarkers = []string{
	dsmlOpen,
	dsmlClose,
	dsmlOpenAlt,
	dsmlCloseAlt,
	"<" + dsmlBar + "DSML" + dsmlBar + "invoke",
	"</" + dsmlBar + "DSML" + dsmlBar + "invoke>",
	"<" + dsmlBar + "DSML" + dsmlBar + "parameter",
	"</" + dsmlBar + "DSML" + dsmlBar + "parameter>",
	"<DSML:",
	"</DSML:",
	dsmlCloseColonAlt,
	dsmlCloseColonSpace,
	"<|DSML|invoke",
	"<|DSML|parameter",
	"</|DSML|invoke",
	"</|DSML|parameter",
	"</|DSML|tool_calls",
	rawToolOpen,
	rawToolClose,
	rawQwenOpen,
	rawQwenClose,
	rawFunctionOpen,
	rawFunctionEnd,
}

// normalizeRawToolCalls 把各种原始标记变体归一化为 parseRawToolCalls 使用的
// 标准全角 DSML 形式。
func normalizeRawToolCalls(text string) string {
	text = strings.ReplaceAll(text, dsmlOpenAlt, dsmlOpen)
	text = strings.ReplaceAll(text, dsmlCloseAlt, dsmlClose)
	text = strings.ReplaceAll(text, "<|DSML|invoke", "<"+dsmlBar+"DSML"+dsmlBar+"invoke")
	text = strings.ReplaceAll(text, "<|DSML|parameter", "<"+dsmlBar+"DSML"+dsmlBar+"parameter")
	text = strings.ReplaceAll(text, "</|DSML|invoke", "</"+dsmlBar+"DSML"+dsmlBar+"invoke")
	text = strings.ReplaceAll(text, "</|DSML|parameter", "</"+dsmlBar+"DSML"+dsmlBar+"parameter")

	if strings.Contains(text, dsmlOpenColon) {
		text = strings.Replace(text, dsmlOpenColon, dsmlOpen, 1)
		text = strings.ReplaceAll(text, dsmlCloseColon, dsmlClose)
		text = strings.ReplaceAll(text, dsmlCloseColonAlt, dsmlClose)
		text = strings.ReplaceAll(text, dsmlCloseColonSpace, dsmlClose)
		text = replaceDSMLColonTags(text)
	} else if strings.Contains(text, rawToolOpen) && strings.Contains(text, rawToolClose) {
		text = strings.Replace(text, rawToolOpen, dsmlOpen, 1)
		text = strings.ReplaceAll(text, rawToolClose, dsmlClose)
		text = replaceBareToolTags(text)
	} else if strings.Contains(text, rawQwenOpen) || strings.Contains(text, rawFunctionOpen) {
		// Qwen 块直接解析，无需 DSML 包装。
		text = strings.ReplaceAll(text, "|DSML|", dsmlBar)
	}
	return text
}

func replaceDSMLColonTags(text string) string {
	text = strings.ReplaceAll(text, "<DSML:invoke ", dsmlInvoke+" ")
	text = strings.ReplaceAll(text, "</DSML:invoke>", dsmlInvokeEnd)
	text = strings.ReplaceAll(text, "<DSML:parameter ", dsmlParam)
	text = strings.ReplaceAll(text, "</DSML:parameter>", dsmlParamEnd)
	return text
}

func replaceBareToolTags(text string) string {
	text = strings.ReplaceAll(text, "<invoke ", dsmlInvoke+" ")
	text = strings.ReplaceAll(text, "</invoke>", dsmlInvokeEnd)
	text = strings.ReplaceAll(text, "<parameter ", dsmlParam)
	text = strings.ReplaceAll(text, "</parameter>", dsmlParamEnd)
	return text
}

func hasCompleteRawToolBlock(text string) bool {
	if strings.Contains(text, dsmlOpen) && strings.Contains(text, dsmlClose) {
		return true
	}
	if strings.Contains(text, dsmlOpenAlt) && strings.Contains(text, dsmlCloseAlt) {
		return true
	}
	if strings.Contains(text, dsmlOpenColon) && (strings.Contains(text, dsmlCloseColon) || strings.Contains(text, dsmlCloseColonAlt) || strings.Contains(text, dsmlCloseColonSpace)) {
		return true
	}
	if strings.Contains(text, rawToolOpen) && strings.Contains(text, rawToolClose) {
		return true
	}
	if strings.Contains(text, rawQwenOpen) && strings.Contains(text, rawQwenClose) {
		return true
	}
	if strings.Contains(text, rawFunctionOpen) && strings.Contains(text, rawFunctionEnd) {
		return true
	}
	return false
}

// findRawToolStart 返回原始工具标记首次出现的下标。仅当文本包含完整块时返回值才有意义。
func findRawToolStart(text string) int {
	normalized := normalizeRawToolCalls(text)
	idx := firstMarkerIndex(normalized, rawToolOpenMarkers)
	if idx >= 0 {
		return idx
	}
	idx = firstMarkerIndex(normalized, rawToolFragmentMarkers)
	if idx >= 0 {
		return idx
	}
	return len(text)
}

func firstMarkerIndex(text string, markers []string) int {
	idx := -1
	for _, marker := range markers {
		if i := strings.Index(text, marker); i >= 0 && (idx == -1 || i < idx) {
			idx = i
		}
	}
	return idx
}

type rawToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function rawToolFunction `json:"function"`
}

type rawToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func parseRawToolCalls(text string) []rawToolCall {
	normalized := normalizeRawToolCalls(text)
	var calls []rawToolCall
	calls = append(calls, parseDSMLToolCalls(normalized)...)
	calls = append(calls, parseQwenToolCalls(normalized)...)
	if len(calls) == 0 && hasCompleteRawToolBlock(normalized) {
		slog.Warn("raw tool block present but could not be parsed", "text", truncateRawToolText(text))
	}
	return calls
}

func parseDSMLToolCalls(text string) []rawToolCall {
	var calls []rawToolCall
	rest := text
	for {
		start := strings.Index(rest, dsmlOpen)
		if start < 0 {
			break
		}
		blockStart := start + len(dsmlOpen)
		end := strings.Index(rest[blockStart:], dsmlClose)
		if end < 0 {
			break
		}
		block := rest[blockStart : blockStart+end]
		calls = append(calls, parseDSMLNameParameters(block)...)
		calls = append(calls, parseDSMLInvokeParameters(block)...)
		rest = rest[blockStart+end+len(dsmlClose):]
	}
	return calls
}

func parseDSMLNameParameters(block string) []rawToolCall {
	var calls []rawToolCall
	rest := block
	for {
		nameStart := strings.Index(rest, "<name>")
		if nameStart < 0 {
			break
		}
		nameEnd := strings.Index(rest[nameStart:], "</name>")
		if nameEnd < 0 {
			break
		}
		paramsStart := strings.Index(rest[nameStart+nameEnd:], "<parameters>")
		if paramsStart < 0 {
			break
		}
		paramsEnd := strings.Index(rest[nameStart+nameEnd+paramsStart:], "</parameters>")
		if paramsEnd < 0 {
			break
		}
		name := strings.TrimSpace(rest[nameStart+len("<name>") : nameStart+nameEnd])
		args := strings.TrimSpace(rest[nameStart+nameEnd+paramsStart+len("<parameters>") : nameStart+nameEnd+paramsStart+paramsEnd])
		if name != "" {
			calls = append(calls, newRawToolCall(name, args))
		}
		rest = rest[nameStart+nameEnd+paramsStart+paramsEnd+len("</parameters>"):]
	}
	return calls
}

func parseDSMLInvokeParameters(block string) []rawToolCall {
	var calls []rawToolCall
	rest := block
	for {
		invokeStart := strings.Index(rest, dsmlInvoke+" ")
		if invokeStart < 0 {
			break
		}
		openEnd := strings.Index(rest[invokeStart:], ">")
		if openEnd < 0 {
			break
		}
		name := attributeValue(rest[invokeStart:invokeStart+openEnd], "name")
		bodyStart := invokeStart + openEnd + 1
		invokeEnd := strings.Index(rest[bodyStart:], dsmlInvokeEnd)
		if invokeEnd < 0 {
			break
		}
		if name != "" {
			params := parseDSMLParameters(rest[bodyStart : bodyStart+invokeEnd])
			calls = append(calls, newRawToolCall(name, string(marshalRawArguments(params))))
		}
		rest = rest[bodyStart+invokeEnd+len(dsmlInvokeEnd):]
	}
	return calls
}

func parseDSMLParameters(block string) map[string]any {
	params := map[string]any{}
	rest := block
	for {
		tag := "<" + dsmlBar + "DSML" + dsmlBar + "parameter "
		start := strings.Index(rest, tag)
		if start < 0 {
			break
		}
		openEnd := strings.Index(rest[start:], ">")
		if openEnd < 0 {
			break
		}
		name := attributeValue(rest[start:start+openEnd], "name")
		bodyStart := start + openEnd + 1
		endTag := "</" + dsmlBar + "DSML" + dsmlBar + "parameter>"
		end := strings.Index(rest[bodyStart:], endTag)
		if end < 0 {
			break
		}
		value := strings.TrimSpace(rest[bodyStart : bodyStart+end])
		if name != "" {
			params[name] = normalizeArgumentValue(value)
		}
		rest = rest[bodyStart+end+len(endTag):]
	}
	return params
}

func parseQwenToolCalls(text string) []rawToolCall {
	var calls []rawToolCall
	rest := text
	for {
		start := strings.Index(rest, rawQwenOpen)
		if start < 0 {
			break
		}
		bodyStart := start + len(rawQwenOpen)
		end := strings.Index(rest[bodyStart:], rawQwenClose)
		if end < 0 {
			break
		}
		block := rest[bodyStart : bodyStart+end]
		calls = append(calls, parseQwenNameParameters(block)...)
		calls = append(calls, parseQwenFunctionParameters(block)...)
		rest = rest[bodyStart+end+len(rawQwenClose):]
	}
	calls = append(calls, parseQwenFunctionParametersOutside(text)...)
	return calls
}

// parseQwenFunctionParametersOutside 解析未被 <tool_call> 包裹的 <function=...> 块，
// 对已包裹形式去重避免重复。
func parseQwenFunctionParametersOutside(text string) []rawToolCall {
	var calls []rawToolCall
	rest := text
	for {
		start := strings.Index(rest, rawFunctionOpen)
		if start < 0 {
			break
		}
		openEnd := strings.Index(rest[start:], ">")
		if openEnd < 0 {
			break
		}
		bodyStart := start + openEnd + 1
		end := strings.Index(rest[bodyStart:], rawFunctionEnd)
		if end < 0 {
			break
		}
		if !insideQwenBlock(text, start) {
			calls = append(calls, parseQwenFunctionBody(rest[start:bodyStart+end+len(rawFunctionEnd)])...)
		}
		rest = rest[bodyStart+end+len(rawFunctionEnd):]
	}
	return calls
}

func insideQwenBlock(text string, pos int) bool {
	openAt := strings.LastIndex(text[:pos], rawQwenOpen)
	if openAt < 0 {
		return false
	}
	closeAt := strings.LastIndex(text[:pos], rawQwenClose)
	return closeAt < openAt
}

func parseQwenNameParameters(block string) []rawToolCall {
	name := xmlElement(block, "name")
	if name == "" {
		return nil
	}
	args := xmlElement(block, "parameters")
	return []rawToolCall{newRawToolCall(name, args)}
}

func parseQwenFunctionParameters(text string) []rawToolCall {
	var calls []rawToolCall
	rest := text
	for {
		start := strings.Index(rest, rawFunctionOpen)
		if start < 0 {
			break
		}
		openEnd := strings.Index(rest[start:], ">")
		if openEnd < 0 {
			break
		}
		bodyStart := start + openEnd + 1
		end := strings.Index(rest[bodyStart:], rawFunctionEnd)
		if end < 0 {
			break
		}
		calls = append(calls, parseQwenFunctionBody(rest[start:bodyStart+end+len(rawFunctionEnd)])...)
		rest = rest[bodyStart+end+len(rawFunctionEnd):]
	}
	return calls
}

func parseQwenFunctionBody(block string) []rawToolCall {
	start := strings.Index(block, rawFunctionOpen)
	if start < 0 {
		return nil
	}
	openEnd := strings.Index(block[start:], ">")
	if openEnd < 0 {
		return nil
	}
	name := strings.TrimSpace(block[start+len(rawFunctionOpen) : start+openEnd])
	bodyStart := start + openEnd + 1
	end := strings.Index(block[bodyStart:], rawFunctionEnd)
	if end < 0 || name == "" {
		return nil
	}
	paramRest := block[bodyStart : bodyStart+end]
	params := map[string]any{}
	for {
		paramStart := strings.Index(paramRest, "<parameter=")
		if paramStart < 0 {
			break
		}
		paramOpenEnd := strings.Index(paramRest[paramStart:], ">")
		if paramOpenEnd < 0 {
			break
		}
		key := strings.TrimSpace(paramRest[paramStart+len("<parameter=") : paramStart+paramOpenEnd])
		valueStart := paramStart + paramOpenEnd + 1
		valueEnd := strings.Index(paramRest[valueStart:], "</parameter>")
		if valueEnd < 0 {
			break
		}
		value := strings.TrimSpace(paramRest[valueStart : valueStart+valueEnd])
		if key != "" {
			params[key] = normalizeArgumentValue(value)
		}
		paramRest = paramRest[valueStart+valueEnd+len("</parameter>"):]
	}
	return []rawToolCall{newRawToolCall(name, string(marshalRawArguments(params)))}
}

func xmlElement(text, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	start := strings.Index(text, open)
	if start < 0 {
		return ""
	}
	end := strings.Index(text[start+len(open):], close)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(text[start+len(open) : start+len(open)+end])
}

func attributeValue(tag, key string) string {
	prefix := key + "=\""
	start := strings.Index(tag, prefix)
	if start < 0 {
		return ""
	}
	rest := tag[start+len(prefix):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return html.UnescapeString(strings.TrimSpace(rest[:end]))
}

func newRawToolCall(name, arguments string) rawToolCall {
	id := "call_" + toolCallIDHash(name+"\x00"+arguments)
	return rawToolCall{
		ID:   id,
		Type: "function",
		Function: rawToolFunction{
			Name:      strings.TrimSpace(name),
			Arguments: normalizeArgumentValue(arguments),
		},
	}
}

func normalizeArgumentValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "{}"
	}
	decoded := html.UnescapeString(value)
	var parsed any
	if err := json.Unmarshal([]byte(decoded), &parsed); err == nil {
		switch parsed.(type) {
		case map[string]any, []any:
			b, _ := json.Marshal(parsed)
			return string(b)
		}
	}
	return value
}

func marshalRawArguments(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func toolCallIDHash(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:12])
}

func truncateRawToolText(text string) string {
	if len(text) <= 800 {
		return text
	}
	return text[:800]
}

// convertRawToolCallsInBody 在助手消息包含完整原始工具块且无原生 tool_calls 时
// 转换非流式 Chat 响应。不适用时返回原始 body。
func convertRawToolCallsInBody(body []byte) []byte {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	converted := convertRawToolCallsInMap(raw)
	if !converted {
		return body
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

func convertRawToolCallsInMap(body map[string]any) bool {
	choices, ok := body["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return false
	}
	msg, ok := choice["message"].(map[string]any)
	if !ok {
		return false
	}
	if calls, exists := msg["tool_calls"]; exists && calls != nil {
		if arr, ok := calls.([]any); ok && len(arr) > 0 {
			return false
		}
	}
	content, _ := msg["content"].(string)
	if content == "" || !hasCompleteRawToolBlock(content) {
		return false
	}
	toolCalls := parseRawToolCalls(content)
	if len(toolCalls) == 0 {
		return false
	}
	start := findRawToolStart(content)
	prefix := strings.TrimSpace(content[:start])
	if prefix == "" {
		msg["content"] = nil
	} else {
		msg["content"] = prefix
	}
	msg["tool_calls"] = toolCalls
	if fr, ok := choice["finish_reason"].(string); !ok || (fr != "tool_calls" && fr != "function_call") {
		choice["finish_reason"] = "tool_calls"
	}
	return true
}

// rawSSEReader 包装上游 Chat SSE 流，把原始 DSML/Qwen 内容改写为标准 delta.tool_calls 事件。
type rawSSEReader struct {
	src       io.ReadCloser
	reader    *bufio.Reader
	pending   [][]byte
	deferred  [][]byte
	text      strings.Builder
	buffering bool
	native    bool
	converted bool
	done      bool
	closed    bool
	chunkID   string
	model     string
	created   any
}

func wrapRawSSE(r io.ReadCloser) io.ReadCloser {
	return &rawSSEReader{src: r, reader: bufio.NewReader(r)}
}

func (r *rawSSEReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.src.Close()
}

func (r *rawSSEReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.EOF
	}
	for len(r.pending) == 0 {
		if r.done {
			return 0, io.EOF
		}
		line, err := r.reader.ReadString('\n')
		if len(line) > 0 {
			r.processLine(line)
		}
		if err != nil {
			r.finishAtEOF()
			r.done = true
			if len(r.pending) == 0 {
				if err == io.EOF {
					return 0, io.EOF
				}
				return 0, err
			}
			break
		}
	}
	if len(r.pending) == 0 {
		return 0, io.EOF
	}
	line := r.pending[0]
	if len(p) >= len(line) {
		r.pending = r.pending[1:]
		return copy(p, line), nil
	}
	n := copy(p, line)
	r.pending[0] = line[n:]
	return n, nil
}

func (r *rawSSEReader) processLine(line string) {
	if strings.TrimSpace(line) == "" {
		if r.buffering {
			r.deferred = append(r.deferred, []byte(line))
		} else if !r.done {
			r.pending = append(r.pending, []byte(line))
		}
		return
	}
	if r.native {
		r.pending = append(r.pending, []byte(line))
		return
	}
	if r.done {
		return
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
		if r.buffering {
			r.deferred = append(r.deferred, []byte(line))
			return
		}
		r.done = true
		r.pending = append(r.pending, []byte(line))
		return
	}
	if !strings.HasPrefix(trimmed, "data:") {
		r.pending = append(r.pending, []byte(line))
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	var chunk map[string]any
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		r.pending = append(r.pending, []byte(line))
		return
	}
	if id, ok := chunk["id"].(string); ok && id != "" {
		r.chunkID = id
	}
	if model, ok := chunk["model"].(string); ok && model != "" {
		r.model = model
	}
	if v, ok := chunk["created"]; ok && v != nil {
		r.created = v
	}
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		if r.buffering {
			r.deferred = append(r.deferred, []byte(line))
		} else {
			r.pending = append(r.pending, []byte(line))
		}
		return
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		r.pending = append(r.pending, []byte(line))
		return
	}
	delta, _ := choice["delta"].(map[string]any)
	if r.converted {
		if u, ok := chunk["usage"]; ok && u != nil {
			r.pending = append(r.pending, []byte(r.makeUsageOnlyLine(chunk)))
		}
		return
	}
	if tc, ok := delta["tool_calls"].([]any); ok && len(tc) > 0 {
		r.native = true
		r.pending = append(r.pending, []byte(line))
		return
	}
	finishReason, _ := choice["finish_reason"].(string)
	if r.buffering {
		text := collectRawDeltaText(delta)
		if text != "" {
			r.text.WriteString(text)
		}
		if hasCompleteRawToolBlock(r.text.String()) {
			r.emitRawToolChunks()
			r.buffering = false
			r.pending = append(r.pending, r.deferred...)
			r.deferred = nil
			r.preserveChunkUsage(chunk)
			// 该行可能同时带 usage 和 finish_reason。在合成的 tool_calls/finish
			// 块之后发出；顺序仍在所有更早的 deferred 事件之后。
			if text == "" {
				r.pending = append(r.pending, []byte(line))
			}
			return
		}
		if text == "" {
			r.deferred = append(r.deferred, []byte(line))
		}
		return
	}
	text := collectRawDeltaText(delta)
	if text == "" {
		r.pending = append(r.pending, []byte(line))
		return
	}
	if hasCompleteRawToolBlock(text) {
		r.text.WriteString(text)
		r.emitRawToolChunks()
		r.preserveChunkUsage(chunk)
		if finishReason != "" {
			r.pending = append(r.pending, []byte(line))
		}
		return
	}
	if hasRawToolFragment(text) {
		r.buffering = true
		r.text.WriteString(text)
		if finishReason != "" {
			r.flushIncompleteBuffer()
			r.pending = append(r.pending, []byte(line))
		}
		return
	}
	r.pending = append(r.pending, []byte(line))
}

func collectRawDeltaText(delta map[string]any) string {
	var b strings.Builder
	for _, field := range []string{"reasoning", "reasoning_content", "content"} {
		if s, ok := delta[field].(string); ok && s != "" {
			b.WriteString(s)
		}
	}
	return b.String()
}

func (r *rawSSEReader) emitRawToolChunks() {
	text := r.text.String()
	r.text.Reset()
	start := findRawToolStart(text)
	prefix := strings.TrimSpace(text[:start])
	toolCalls := parseRawToolCalls(text)
	if len(toolCalls) == 0 {
		// 块完整但无法解析。只保留前缀，不向客户端泄漏原始标记。
		if prefix != "" {
			r.pending = append(r.pending, []byte(r.makeDataChunk(map[string]any{
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"content": prefix, "tool_calls": []any{}},
				}},
			})))
		}
		return
	}
	r.converted = true
	if prefix != "" {
		r.pending = append(r.pending, []byte(r.makeDataChunk(map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": prefix, "tool_calls": []any{}},
			}},
		})))
	}
	for i, tc := range toolCalls {
		idx := i
		r.pending = append(r.pending, []byte(r.makeDataChunk(map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"index":    idx,
						"id":       tc.ID,
						"type":     "function",
						"function": map[string]any{"name": tc.Function.Name, "arguments": ""},
					}},
				},
			}},
		})))
		args := tc.Function.Arguments
		for len(args) > 0 {
			size := 32
			if len(args) < size {
				size = len(args)
			}
			r.pending = append(r.pending, []byte(r.makeDataChunk(map[string]any{
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []any{map[string]any{
							"index":    idx,
							"function": map[string]any{"arguments": args[:size]},
						}},
					},
				}},
			})))
			args = args[size:]
		}
	}
	r.pending = append(r.pending, []byte(r.makeDataChunk(map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "tool_calls",
		}},
	})))
}

func (r *rawSSEReader) preserveChunkUsage(chunk map[string]any) {
	if u, ok := chunk["usage"]; ok && u != nil {
		r.pending = append(r.pending, []byte(r.makeUsageOnlyLine(chunk)))
	}
}

func (r *rawSSEReader) makeUsageOnlyLine(chunk map[string]any) string {
	return r.makeDataChunk(map[string]any{
		"choices": []any{},
		"usage":   chunk["usage"],
	})
}

func (r *rawSSEReader) flushIncompleteBuffer() {
	if !r.buffering {
		return
	}
	text := r.text.String()
	r.text.Reset()
	r.buffering = false
	if text == "" || hasCompleteRawToolBlock(text) {
		return
	}
	start := findRawToolStart(text)
	if start > 0 {
		prefix := strings.TrimSpace(text[:start])
		if prefix != "" {
			r.pending = append(r.pending, []byte(r.makeDataChunk(map[string]any{
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"content": prefix, "tool_calls": []any{}},
				}},
			})))
		}
	}
}

func (r *rawSSEReader) makeDataChunk(body map[string]any) string {
	base := map[string]any{}
	if r.chunkID != "" {
		base["id"] = r.chunkID
	} else {
		base["id"] = "chatcmpl-raw-tool-call"
	}
	base["object"] = "chat.completion.chunk"
	if r.model != "" {
		base["model"] = r.model
	}
	if r.created != nil {
		base["created"] = r.created
	}
	for k, v := range body {
		base[k] = v
	}
	b, _ := json.Marshal(base)
	return "data: " + string(b) + "\n\n"
}

func (r *rawSSEReader) finishAtEOF() {
	if r.buffering && r.text.Len() > 0 {
		text := r.text.String()
		if hasRawToolFragment(text) && !hasCompleteRawToolBlock(text) {
			// 永不泄漏残缺的原始标记前缀。若有可识别的前缀则只冲刷该前缀。
			start := findRawToolStart(text)
			if start > 0 {
				prefix := strings.TrimSpace(text[:start])
				if prefix != "" {
					r.pending = append(r.pending, []byte(r.makeDataChunk(map[string]any{
						"choices": []any{map[string]any{
							"index": 0,
							"delta": map[string]any{"content": prefix, "tool_calls": []any{}},
						}},
					})))
				}
			}
		} else if text != "" {
			r.pending = append(r.pending, []byte(r.makeDataChunk(map[string]any{
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"content": text, "tool_calls": []any{}},
				}},
			})))
		}
		r.buffering = false
		r.text.Reset()
	}
	if len(r.deferred) > 0 {
		r.pending = append(r.pending, r.deferred...)
		r.deferred = nil
	}
}

// hasRawToolFragment 报告文本是否包含已知原始标记或文本结尾可能是某个标记的前缀。
// 刻意保守：普通文本永远不会被延迟。
func hasRawToolFragment(text string) bool {
	normalized := normalizeRawToolCalls(text)
	if firstMarkerIndex(normalized, rawToolFragmentMarkers) >= 0 {
		return true
	}
	tail := text
	if len(tail) > 150 {
		tail = tail[len(tail)-150:]
	}
	for _, marker := range rawToolFragmentMarkers {
		max := len(marker) - 1
		if max > len(tail) {
			max = len(tail)
		}
		for size := 2; size <= max; size++ {
			if strings.HasSuffix(tail, marker[:size]) {
				return true
			}
		}
	}
	return false
}
