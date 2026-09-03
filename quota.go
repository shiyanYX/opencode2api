package main

import (
	"encoding/json"
	"strings"
)

// ======================= 免费额度耗尽判定 =======================

func effectiveMaxQuotaNodeSwitches() int {
	quotaSignalsMu.Lock()
	defer quotaSignalsMu.Unlock()
	if maxQuotaNodeSwitches > 0 {
		return maxQuotaNodeSwitches
	}
	return defaultMaxQuotaNodeSwitches
}

func effectiveQuotaSignals() ([]string, []string) {
	quotaSignalsMu.Lock()
	defer quotaSignalsMu.Unlock()
	errorTypes := quotaErrorTypes
	if errorTypes == nil {
		errorTypes = defaultQuotaErrorTypes()
	}
	keywords := quotaMessageKeywords
	if keywords == nil {
		keywords = defaultQuotaMessageKeywords()
	}
	et := make([]string, len(errorTypes))
	copy(et, errorTypes)
	mk := make([]string, len(keywords))
	copy(mk, keywords)
	return et, mk
}

// classifyQuota 判定上游响应是否为"免费额度耗尽"，返回 (是, 原因)。
// 判定规则（与状态码无关，只看 body 签名）：
//  1. error.type 命中配置的 error_types（默认含 FreeUsageLimitError 等）→ 耗尽
//  2. error.message 命中配置的 message_keywords → 耗尽
//  3. 403 无签名 → 耗尽（区域限制，视为不可用）
//  4. 429 无签名 → 不耗尽（普通限流，同节点重试）
func classifyQuota(status int, body []byte) (bool, string) {
	if len(body) == 0 {
		return false, ""
	}
	errorTypes, keywords := effectiveQuotaSignals()

	var obj struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		Type    string         `json:"type"`
		Message string         `json:"message"`
		Detail  map[string]any `json:"detail"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return false, ""
	}

	errType := obj.Error.Type
	if errType == "" {
		errType = obj.Type
	}
	msg := obj.Error.Message
	if msg == "" {
		msg = obj.Message
	}
	if obj.Detail != nil {
		if t, _ := obj.Detail["type"].(string); errType == "" && t != "" {
			errType = t
		}
		if s, _ := obj.Detail["message"].(string); msg == "" && s != "" {
			msg = s
		}
	}
	errTypeL, msgL := strings.ToLower(errType), strings.ToLower(msg)

	for _, t := range errorTypes {
		if t != "" && errTypeL != "" && strings.Contains(errTypeL, strings.ToLower(t)) {
			return true, "type:" + t
		}
	}
	for _, k := range keywords {
		if k != "" && msgL != "" && strings.Contains(msgL, strings.ToLower(k)) {
			return true, "keyword:" + k
		}
	}
	if status == 403 && errType == "" && msg == "" {
		return true, "status:403"
	}
	return false, ""
}

// ======================= 区域限制错误检测 =======================

// regionRestrictionKeywords 定义区域限制错误的关键词（大小写不敏感）。
var regionRestrictionKeywords = []string{
	"regionerror",
	"region restricted",
	"not available in your region",
	"not available in your country",
	"access denied from this region",
	"unavailable in your region",
	"unavailable in your country",
	"geo-restricted",
	"geo restricted",
	"location restricted",
	"area restricted",
	"not supported in your region",
	"not supported in your country",
	"only available in",
	"restricted to",
	"available in",
}

// classifyRegionRestriction 检测上游响应是否为区域限制错误。
// 返回 (是, 检测到的区域)。区域可能为空字符串（表示检测到限制但无法确定具体区域）。
func classifyRegionRestriction(status int, body []byte) (bool, string) {
	if status != 403 || len(body) == 0 {
		return false, ""
	}

	var obj struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		Type    string         `json:"type"`
		Message string         `json:"message"`
		Detail  map[string]any `json:"detail"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return false, ""
	}

	errType := obj.Error.Type
	if errType == "" {
		errType = obj.Type
	}
	msg := obj.Error.Message
	if msg == "" {
		msg = obj.Message
	}
	if obj.Detail != nil {
		if t, _ := obj.Detail["type"].(string); errType == "" && t != "" {
			errType = t
		}
		if s, _ := obj.Detail["message"].(string); msg == "" && s != "" {
			msg = s
		}
	}

	combined := strings.ToLower(errType + " " + msg)

	// 检查是否包含区域限制关键词
	for _, kw := range regionRestrictionKeywords {
		if strings.Contains(combined, kw) {
			// 尝试从消息中提取区域信息（如 "only available in US"）
			region := extractRegionFromMessage(combined)
			return true, region
		}
	}

	// 403 且无其他错误信息，也视为区域限制
	if errType == "" && msg == "" {
		return true, ""
	}

	return false, ""
}

// extractRegionFromMessage 从错误消息中尝试提取区域标识。
func extractRegionFromMessage(msg string) string {
	msg = strings.ToLower(msg)

	// 尝试匹配 "only available in XX" 或 "restricted to XX" 模式
	for _, pattern := range []string{"only available in ", "restricted to ", "available in "} {
		if idx := strings.Index(msg, pattern); idx >= 0 {
			rest := msg[idx+len(pattern):]
			// 取下一个单词作为区域
			for i, ch := range rest {
				if ch == ' ' || ch == '.' || ch == ',' || i == len(rest)-1 {
					end := i
					if i == len(rest)-1 {
						end = i + 1
					}
					candidate := rest[:end]
					// 直接匹配
					if region, ok := regionPatterns[candidate]; ok {
						return region
					}
					// 尝试匹配前缀（如 "singapore" 匹配 "sg"）
					for pattern, region := range regionPatterns {
						if len(pattern) >= 2 && strings.HasPrefix(candidate, pattern) {
							return region
						}
					}
					break
				}
			}
		}
	}

	return ""
}
