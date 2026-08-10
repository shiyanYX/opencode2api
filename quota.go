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
