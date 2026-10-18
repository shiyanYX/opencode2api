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

	// FreeTierError 表示上游拒绝我们的客户端身份（例如 x-opencode-session
	// 形态不合规），属于网关侧问题，不是"免费模型已下线"。它的 message
	// 里包含 "free tier" 子串，若继续改写会被误报成模型下线，掩盖真实原因，
	// 因此这里直接透传原始错误，保留可诊断性。
	if strings.EqualFold(errType, "FreeTierError") {
		// ★ 这里不是"原样透传"而是"【保留原样再补一句可读的说明】"。
		//
		// 为什么要补：403 会被绝大多数客户端解读成「你的 API key 不对」，
		// 于是用户去换 key —— 而 key 是对的，被拒的是【客户端形态】（上游要求
		// 免费层只能从合规客户端发起）。原样透传保留可诊断性，但没解决误读。
		//
		// ★ 为什么不改成 5xx：状态码是下游重试策略的开关，改成 503 会让客户端
		//   把它当瞬时故障疯狂重试（403 本来是"不重试"的档位），反而放大上游压力；
		//   而 FreeTierError 在调用方看来确实【不该重试】—— 重试也换不来形态合规。
		//   所以：保留 403 + 保留 error.type（让本函数的透传判定继续成立，
		//   不会被下游的"模型已下线"启发式误改写）+ 只增强 message。
		return annotateFreeTierError(obj, body)
	}

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

// annotateFreeTierError 给 FreeTierError 补一段说明，让下游看得出
// 「这是上游对客户端形态的要求，不是你的 key 有问题」。
// ★ 只增强 message 并加一个无歧义的标记字段，**不改状态码、不改 error.type**。
// 解析失败时原样返回 —— 绝不因为补充说明而破坏原有的可诊断性。
func annotateFreeTierError(obj map[string]any, body []byte) []byte {
	const explain = " This is an upstream client-compliance requirement, not an invalid API key: " +
		"OpenCode's free tier is only available to clients it recognises. " +
		"Your key is valid. Retrying or switching nodes will not help."
	em, _ := obj["error"].(map[string]any)
	if em == nil {
		return body // 结构不认识，宁可不动
	}
	msg, _ := em["message"].(string)
	lower := msg
	if lower == "" {
		return body
	}
	// 幂等：已补过就不要再补（同一个 body 可能被 rewrite 多次）
	if len(lower) > 140 && containsFold(lower, "client-compliance requirement") {
		return body
	}
	em["message"] = msg + explain
	em["upstream_compliance_required"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// containsFold 是大小写无关的子串判断（避免为此再引 strings 的额外用法差异）。
func containsFold(s, sub string) bool {
	n, m := len(s), len(sub)
	if m == 0 {
		return true
	}
	for i := 0; i+m <= n; i++ {
		if strings.EqualFold(s[i:i+m], sub) {
			return true
		}
	}
	return false
}
