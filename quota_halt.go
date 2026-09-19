package main

// ======================== 全局配额熔断（quota halt）========================
//
// 场景：节点池已无可用节点 → 出口回退直连 → 上游依然返回 429 额度限制。
// 此时任何重试都打在同一个 IP 上，只会空转：实测单个请求会烧满
// maxAttempts(3) + maxQuotaSwitches(5) 的上游调用预算，且每个新请求重来一遍，
// 面板上表现为永不停止的 429 与 quota_signal。
//
// 因此一旦判定该状态就进入熔断：
//   - 免费层请求直接返回 429（重放最后一次上游错误 body），不再访问上游；
//   - 每个请求惰性检查节点池，出现可用节点（含配额冷却到期自动翻回的）即自动解除；
//   - 付费层不受影响：付费层的 insufficient_quota / credits_error 是账号计费
//     问题，与出口 IP 无关，熔断会误伤。

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// quotaHaltFallbackBody 是没保存到上游错误体时的兜底响应（理论上不会用到）。
var quotaHaltFallbackBody = []byte(`{"error":{"type":"FreeUsageLimitError","message":"所有出口节点额度耗尽且直连出口被上游限流，网关已暂停转发；节点池恢复可用节点后自动恢复。"}}`)

type quotaHalt struct {
	mu     sync.RWMutex
	halted bool
	at     time.Time
	reason string
	body   []byte
}

var quotaHaltGuard = &quotaHalt{}

// halt 进入熔断并保存最后一次上游错误 body（幂等：重复调用刷新原因与 body）。
// 返回是否发生了 false → true 的状态翻转。
func (h *quotaHalt) halt(reason string, body []byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	first := !h.halted
	h.halted = true
	h.at = time.Now()
	h.reason = reason
	h.body = append([]byte(nil), body...)
	return first
}

// active 报告当前是否处于熔断状态。
func (h *quotaHalt) active() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.halted
}

// clear 解除熔断，返回是否发生了 true → false 的状态翻转。
func (h *quotaHalt) clear() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.halted {
		return false
	}
	h.halted = false
	h.at = time.Time{}
	h.reason = ""
	h.body = nil
	return true
}

// info 返回熔断快照（供日志/面板使用）。
func (h *quotaHalt) info() (bool, time.Time, string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.halted, h.at, h.reason
}

func (h *quotaHalt) bodyCopy() []byte {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]byte(nil), h.body...)
}

// gate 是上游调用入口的熔断闸门。halted=true 时调用方必须直接把 body 以
// 429 返回下游，不做任何上游尝试。只有免费层受熔断影响。
func (h *quotaHalt) gate(auth UpstreamAuth) (halted bool, body []byte) {
	if auth.tier() != TierFree || !h.active() {
		return false, nil
	}
	// 节点池一旦出现可用节点（含冷却到期被 sweep 翻回的）立即恢复转发。
	if proxyPool.hasEligibleNode() {
		if h.clear() {
			slog.Info("quota_halt_cleared", "reason", "node_available")
		}
		return false, nil
	}
	return true, h.bodyCopy()
}

// quotaHaltResponse 构造熔断期间返回给下游的响应：429 + 保存的上游错误 body
// （形态与既有上游错误透传一致，下游客户端按限流处理即可）。
func quotaHaltResponse(ctx context.Context, body []byte) ([]byte, int, http.Header, error) {
	if len(body) == 0 {
		body = quotaHaltFallbackBody
	}
	callLogEvent(ctx, "quota_halt", "", "blocked: 无可用节点且直连出口 429，等待节点池恢复")
	return body, http.StatusTooManyRequests, nil, nil
}
