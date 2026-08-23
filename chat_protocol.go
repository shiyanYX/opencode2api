package main

// normalizeFinishReason maps Anthropic stop reasons onto the closed set used
// by Chat Completions.
func normalizeFinishReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence", "stop":
		return "stop"
	case "max_tokens", "length":
		return "length"
	case "tool_use", "tool_calls", "function_call":
		return "tool_calls"
	case "refusal", "content_filter":
		return "content_filter"
	default:
		return reason
	}
}

// ======================== finish_reason 语义分类 ========================
//
// 上游（尤其 Zen 网关的隐身预览模型）会出现三类异常收尾：
//  1. 非标准 finish 值：如内容过滤掐流时返回的 "sensitive"（opencode#44092）；
//  2. 协议外未知值；
//  3. 干脆没有 finish_reason 就结束（SSE 提前 EOF，opencode#44210 失败模式 2）。
//
// 老行为把它们一律洗白成正常完成（end_turn/stop），下游无法感知截断。
// 这里提供统一分类，让各输出路径给出诚实信号。

// finishSignal 分类上游 finish_reason 值的语义。
type finishSignal int

const (
	finishNone    finishSignal = iota // 缺失或空：流未以合法 finish 收尾
	finishNormal                      // OpenAI 协议内已知值
	finishFilter                      // content_filter 及已知过滤别名（如 Zen 的 "sensitive"）
	finishUnknown                     // 非空但协议外，视为异常终止
)

// classifyFinishReason 判定上游 finish_reason 的语义类别。
func classifyFinishReason(raw string) finishSignal {
	switch raw {
	case "":
		return finishNone
	case "stop", "length", "tool_calls", "function_call":
		return finishNormal
	case "content_filter", "sensitive", "moderation", "safety":
		// "sensitive"/"moderation"/"safety" 是 Zen 内容过滤掐流的实测签名。
		return finishFilter
	default:
		return finishUnknown
	}
}

// mapUpstreamFinishReason 把上游 finish_reason 规范化为 OpenAI 语义值：
// 过滤别名归一为 content_filter；无法识别的值按异常终止处理（改用配置的截断原因）；
// 空值原样返回（由调用方决定缺失时的兜底）。
func mapUpstreamFinishReason(raw string) string {
	switch classifyFinishReason(raw) {
	case finishNormal:
		return raw
	case finishFilter:
		return "content_filter"
	case finishUnknown:
		return truncationStopReason()
	default:
		return ""
	}
}

// claudeStopReasonFromOpenAI 把 OpenAI 语义停止原因映射到 Anthropic stop_reason。
func claudeStopReasonFromOpenAI(reason string) string {
	switch reason {
	case "max_tokens", "length":
		return "max_tokens"
	case "tool_calls", "tool_use":
		return "tool_use"
	case "refusal", "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

func anthropicUsageToChat(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	out := make(map[string]any, len(usage)+3)
	for k, v := range usage {
		out[k] = v
	}
	if v, ok := usage["input_tokens"]; ok {
		out["prompt_tokens"] = v
	}
	if v, ok := usage["output_tokens"]; ok {
		out["completion_tokens"] = v
	}
	if p, pok := numberAsFloat(out["prompt_tokens"]); pok {
		if c, cok := numberAsFloat(out["completion_tokens"]); cok {
			out["total_tokens"] = p + c
		}
	}
	delete(out, "input_tokens")
	delete(out, "output_tokens")
	return out
}

func numberAsFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}
