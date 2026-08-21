package main

import (
	"encoding/json"
	"strings"
)

// ======================= 免费模型已下线错误改写 =======================

// rewriteUpstreamError 检测上游错误是否为"免费模型已下线"类报错，
// 若是则改写 error.message 为下游可读的清晰提示，保留原始 HTTP 状态码。
// 支持 Anthropic 格式 {"type":"error","error":{"type":"...","message":"..."}} 和
// OpenAI 格式 {"error":{"message":"...","type":"..."}}。
func rewriteUpstreamError(body []byte) []byte {
	if len(body) == 0 {
		return body
	}

	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}

	// 提取错误信息：兼容 Anthropic 和 OpenAI 两种格式
	errType, msg := extractErrorFields(obj)
	msgL := strings.ToLower(msg)

	// 已知的"免费模型已下线"特征关键词
	freeEndedPatterns := []string{
		"free promotion has ended",
		"free model is no longer available",
		"this model is not available for free",
		"free tier",
		"no longer available",
	}

	matched := false
	for _, p := range freeEndedPatterns {
		if strings.Contains(msgL, p) {
			matched = true
			break
		}
	}
	if !matched {
		return body
	}

	// 构造改写后的清晰提示
	newMsg := "该免费模型已停止服务，请更换其他免费模型（管理面板 → 模型映射 中可查看当前可用的免费模型），或通过订阅获取更多模型。"

	// 按原格式写回
	if errObj, ok := obj["error"].(map[string]any); ok {
		// OpenAI 格式 或 Anthropic 格式都可能有 error 字段
		errObj["message"] = newMsg
		// 记录原始 type（如有），然后替换为 free_model_ended
		if errType != "" {
			errObj["original_type"] = errType
		}
		errObj["type"] = "free_model_ended"
	} else if obj["type"] == "error" {
		// Anthropic 格式没有嵌套 error 对象（理论上不会到这里，但防御性处理）
		obj["error"] = map[string]any{
			"type":    "free_model_ended",
			"message": newMsg,
		}
	}

	out, _ := json.Marshal(obj)
	return out
}

// extractErrorFields 从错误 JSON 中提取 error.type 和 error.message，
// 兼容 Anthropic 格式和 OpenAI 格式。
func extractErrorFields(obj map[string]any) (errType, msg string) {
	// 尝试 Anthropic 格式: {"error":{"type":"...","message":"..."}}
	if errObj, ok := obj["error"].(map[string]any); ok {
		if t, _ := errObj["type"].(string); t != "" {
			errType = t
		}
		if m, _ := errObj["message"].(string); m != "" {
			msg = m
		}
	}
	// 尝试 OpenAI 格式: {"error":{"message":"...","type":"..."}} — 已经处理
	// 尝试顶层字段（某些错误格式）
	if msg == "" {
		if m, _ := obj["message"].(string); m != "" {
			msg = m
		}
	}
	if errType == "" {
		if t, _ := obj["type"].(string); t != "" && t != "error" {
			errType = t
		}
	}
	return errType, msg
}
