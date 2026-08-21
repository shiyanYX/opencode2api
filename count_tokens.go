package main

import (
	"encoding/json"
	"io"
	"net/http"
)

// ======================== Claude Messages count_tokens ========================
//
// POST /v1/messages/count_tokens 返回本地启发式估算的输入 token 数。
// 不调用上游、不产生用量；Claude Code 用它做上下文窗口管理和自动压缩，
// 合理的估算值即可满足需求。

const (
	// 内容 token：约 4 字符/token，接近英文 BPE 压缩率。
	charsPerToken = 4
	// 每条消息 / system 块 / 工具定义的结构化开销。
	messageOverhead = 4
	systemOverhead  = 4
	toolOverhead    = 8
	// 多模态块的固定估算（Anthropic 文档的图片近似值；文档取平铺兜底值）。
	imageTokens    = 1600
	documentTokens = 3000
)

// estimateClaudeInputTokens 启发式估算 Claude Messages 请求的输入 token 数。只读 req。
func estimateClaudeInputTokens(req ClaudeRequest) int {
	total := 0
	if sys := extractClaudeSystemText(req.System); sys != "" {
		// system 块自带结构化开销。
		total = systemOverhead + estimateTextTokens(sys)
	}
	for _, msg := range req.Messages {
		total += messageOverhead
		total += estimateContentTokens(msg.Content)
	}
	for _, tool := range req.Tools {
		total += toolOverhead
		if tool.Description != "" {
			total += estimateTextTokens(tool.Description)
		}
		if b, err := json.Marshal(tool.InputSchema); err == nil {
			total += estimateTextTokens(string(b))
		}
	}
	if total <= 0 {
		return 1 // 永不返回 0：0 会干扰客户端上下文管理
	}
	return total
}

// estimateContentTokens 估算单个 Anthropic content 字段（纯字符串、block 数组或任意 JSON）的 token。
func estimateContentTokens(content any) int {
	switch c := content.(type) {
	case string:
		return estimateTextTokens(c)
	case []any:
		total := 0
		for _, item := range c {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := block["type"].(string)
			switch blockType {
			case "text":
				if text, ok := block["text"].(string); ok {
					total += estimateTextTokens(text)
				}
			case "image":
				total += imageTokens
			case "document":
				total += documentTokens
			case "thinking", "redacted_thinking":
				if text, ok := block["thinking"].(string); ok {
					total += estimateTextTokens(text)
				}
				if data, ok := block["data"].(string); ok {
					total += estimateTextTokens(data)
				}
			case "tool_use":
				if name, ok := block["name"].(string); ok {
					total += estimateTextTokens(name)
				}
				if input := block["input"]; input != nil {
					total += estimateTextTokens(jsonString(input))
				}
			case "tool_result":
				if content := block["content"]; content != nil {
					total += estimateContentTokens(content)
				}
				if isErr, _ := block["is_error"].(bool); isErr {
					total += messageOverhead
				}
			default:
				// 未知 block：按原始 JSON 计数兜底。
				total += estimateTextTokens(jsonString(block))
			}
		}
		return total
	default:
		if c == nil {
			return 0
		}
		return estimateTextTokens(jsonString(c))
	}
}

// jsonString 序列化任意值为 JSON 字符串，失败返回 ""。仅用于 token 估算。
func jsonString(v any) string {
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return ""
}

// estimateTextTokens 按 rune（而非字节）计数避免多字节文本高估；向上取整保证非空短串至少计 1。
func estimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	n := len([]rune(s))
	tokens := n / charsPerToken
	if n%charsPerToken != 0 {
		tokens++
	}
	return tokens
}

// claudeCountTokensHandler 处理 POST /v1/messages/count_tokens：本地启发式估算，不触上游。
func claudeCountTokensHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	var claudeReq ClaudeRequest
	if err := json.Unmarshal(body, &claudeReq); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "invalid_request_error",
				"message": "Invalid JSON",
			},
		})
		return
	}
	if claudeReq.Model == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "invalid_request_error",
				"message": "model: field is required",
			},
		})
		return
	}

	inputTokens := estimateClaudeInputTokens(claudeReq)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": inputTokens})
}
