package main

// ======================== 出口级配额熔断（quota halt）========================
//
// 场景：某个出口（直连或某个节点）被上游按 IP 分桶限流。此时反复重试只会打在
// 同一个 IP 上空转：实测单个请求会烧满 maxAttempts(3) + maxQuotaSwitches(5)
// 的上游调用预算，且每个新请求重来一遍。
//
// ★ 本版修掉的三处错配（与上一版逐条对应）：
//
//  1. 【解闩权错配】旧版闩只由「直连 429」拉起，却只由「节点池状态」解除
//     （hasEligibleNode()）。但「直连被限流」推不出「节点池不可用」——
//     两者是独立的两个事实。池子健康时也会被闩住，实测三天误拒 3,284 条，
//     而那批直连放行后成功率有 35.5%（约 1,166 条本可成功）。
//     ⇒ 现在判据是【两者同时成立才继续闩】：既没有可用节点，
//       **且**所有被闩的出口都还没拿到权威的恢复确认。
//
//  2. 【单次成功不可信】实测 429 后 ≤1 小时恢复率 0.0%、恢复中位 18.6 小时。
//     一次 2xx 绝不足以宣告恢复。⇒ 滞回：同一出口需**连续 2 次**成功，
//     且**两次间隔 ≥60 秒**，才认定恢复。
//
//  3. 【503 + Retry-After 必须同时上】旧版返回 429 且 header 为 nil：
//     429 会被下游与中间层理解为「你自己额度用完了，重试无意义」，
//     而 header 为 nil 又让标准客户端拿不到重试时刻。503 是「网关暂时不可用」
//     的正确语义（服务本身没坏）。Retry-After 30 给出明确的短冷却：
//     ★ 30 秒远小于实测恢复 p10（5.6 小时）—— 它只是**削峰**用的短退避，
//       绝不宣称「30 秒后就能服务」。真正放行仍由上面的恢复判据决定。
//
// 付费层不受影响：付费层的 insufficient_quota / credits_error 是账号计费问题，
// 与出口 IP 无关，熔断会误伤。

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	// haltRecoveryStreak 解除熔断所需的连续成功次数（滞回 K）。
	// 依据：≤1h 恢复率 0.0%，单次成功不可信。
	haltRecoveryStreak = 2
	// haltRecoveryInterval 两次成功之间至少间隔这么久，避免同一批慢请求
	// 在几十秒内连打两次，把「上游短暂抖动」误判成「已恢复」。
	haltRecoveryInterval = 60 * time.Second
	// haltRetryAfterSeconds 熔断期间 503 响应带的 Retry-After（秒）。
	// 削峰用；远小于恢复 p10，只告诉客户端「先别立刻再来」。
	haltRetryAfterSeconds = 30
)

// quotaHaltFallbackBody 是没保存到上游错误体时的兜底响应（理论上不会用到）。
var quotaHaltFallbackBody = []byte(`{"error":{"type":"GatewayUnavailable","message":"所有可用出口额度耗尽且直连出口被上游限流，网关已暂停转发；出口恢复后将自动恢复。"}}`)

// haltState 是【单个出口】的熔断状态。
//
// 按出口分桶（而不是全局一个 bool）：一个出口被限流不该影响其它出口——
// 旧版只有一个 halted 布尔，任何一次 direct_429 就把所有后续请求
// （含本可以走健康节点的）一起挡掉。
type haltState struct {
	halted    bool
	at        time.Time
	reason    string
	body      []byte
	streak    int       // 当前连续成功次数（滞回计数）
	streakAt  time.Time // 本轮连续成功第一次发生的时间
	lastProbe time.Time // 最近一次成功观察的时间
}

// recovered 报告该出口是否已拿到权威的恢复确认：连续 haltRecoveryStreak 次成功
// 且跨度 ≥ haltRecoveryInterval。单次成功只累加计数，不足以恢复。
func (s *haltState) recovered(now time.Time) bool {
	if s.streak < haltRecoveryStreak {
		return false
	}
	if s.streakAt.IsZero() || now.Sub(s.streakAt) < haltRecoveryInterval {
		return false
	}
	return true
}

type quotaHalt struct {
	mu     sync.RWMutex
	states map[string]*haltState
}

var quotaHaltGuard = &quotaHalt{states: map[string]*haltState{}}

func (h *quotaHalt) stateLocked(key string) *haltState {
	if h.states == nil {
		h.states = map[string]*haltState{}
	}
	s := h.states[key]
	if s == nil {
		s = &haltState{}
		h.states[key] = s
	}
	return s
}

// halt 拉起某个出口的熔断并保存最后一次上游错误 body
// （幂等：重复调用刷新原因与 body）。返回是否发生了 false → true 的翻转。
func (h *quotaHalt) halt(key, reason string, body []byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.stateLocked(key)
	first := !s.halted
	s.halted = true
	s.at = time.Now()
	s.reason = reason
	s.body = append([]byte(nil), body...)
	// 新的限流证据 ⇒ 恢复计数清零，否则旧的 2 次成功会污染下一次判定。
	s.streak = 0
	s.streakAt = time.Time{}
	return first
}

// observeSuccess 记录该出口一次**真实上游 2xx**。
//
// ★ 这是「解闩权」的来源：不再看节点池状态（那是另一个事实），只看这个出口
//
//	自己有没有真的拿到成功。滞回 K=2 + 两次间隔 ≥60s。
func (h *quotaHalt) observeSuccess(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.stateLocked(key)
	if !s.halted {
		return
	}
	now := time.Now()
	// 与上一次成功间隔不足 ⇒ 本轮重新计数（等于把滞回又往后推 60s）。
	if s.streak > 0 && !s.lastProbe.IsZero() && now.Sub(s.lastProbe) < haltRecoveryInterval {
		s.streak = 1
	} else {
		s.streak++
	}
	if s.streakAt.IsZero() {
		s.streakAt = now
	}
	s.lastProbe = now
}

// observeFailure 记录该出口又一次失败：恢复计数立刻清零。
func (h *quotaHalt) observeFailure(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.stateLocked(key)
	s.streak = 0
	s.streakAt = time.Time{}
}

// clear 解除某个出口的熔断，返回是否发生了 true → false 的翻转。
func (h *quotaHalt) clear(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.stateLocked(key)
	if !s.halted {
		return false
	}
	s.halted = false
	s.at = time.Time{}
	s.reason = ""
	s.body = nil
	s.streak = 0
	s.streakAt = time.Time{}
	s.lastProbe = time.Time{}
	return true
}

// allHaltedUnrecovered 报告：(是否有出口被闩, 是否所有被闩出口都未确认恢复)。
// 没有任何出口被闩 ⇒ 第一个返回值 false（根本没闩，不需要判）。
func (h *quotaHalt) allHaltedUnrecovered() (anyHalted, allUnrecovered bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	now := time.Now()
	anyRecovered := false
	for _, s := range h.states {
		if !s.halted {
			continue
		}
		anyHalted = true
		if s.recovered(now) {
			anyRecovered = true
		}
	}
	return anyHalted, anyHalted && !anyRecovered
}

// active 报告当前是否处于熔断状态。
func (h *quotaHalt) active() bool {
	anyHalted, allUnrecovered := h.allHaltedUnrecovered()
	return anyHalted && allUnrecovered
}

// info 返回熔断快照（供日志/面板使用）。
func (h *quotaHalt) info() (bool, time.Time, string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.states {
		if s.halted {
			return true, s.at, s.reason
		}
	}
	return false, time.Time{}, ""
}

// bodyCopy 取任一被闩出口保存的上游错误体（作为熔断响应内容）。
func (h *quotaHalt) bodyCopy() []byte {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.states {
		if s.halted && len(s.body) > 0 {
			return append([]byte(nil), s.body...)
		}
	}
	return nil
}

// gate 是上游调用入口的熔断闸门。blocked=true 时调用方必须把 body 以 503
// 返回下游，不再做任何上游尝试。只有免费层受熔断影响。
//
// ★ 与旧版的差异（这就是修 bug 的地方）：
//
//	旧：halted && !hasEligibleNode()  → 池子健康时也闩 ⇒ 误拒 3,284 条。
//	新：(有出口被闩) && (池无可用节点) && (所有被闩出口都未确认恢复) 才闩。
//
//	池子一旦有活节点、或被闩的直连出口已拿到 2 次（间隔 ≥60s）成功，都立即放行。
func (h *quotaHalt) gate(auth UpstreamAuth) (blocked bool, body []byte) {
	if auth.tier() != TierFree {
		return false, nil
	}
	anyHalted, allUnrecovered := h.allHaltedUnrecovered()
	if !anyHalted {
		return false, nil
	}
	// 节点池一旦出现可用节点（含冷却到期被 sweep 翻回的）立即恢复转发。
	if proxyPool.hasEligibleNode() {
		return false, nil
	}
	// 池子真空时，只有在所有被闩出口都【尚未】确认恢复时才继续闩。
	if !allUnrecovered {
		return false, nil
	}
	return true, h.bodyCopy()
}

// quotaHaltResponse 构造熔断期间返回给下游的响应。
//
// ★ 503 而非 429：熔断是「网关出口暂时不可用」，不是「调用方额度用完了」；
//
//	429 会被客户端与中间层误解为不可重试。配套必须带 Retry-After ——
//	只改状态码而不带重试时刻会立刻引发重试风暴。
//	两者必须【同时】上，这是本条不可拆分的原因。
func quotaHaltResponse(ctx context.Context, body []byte) ([]byte, int, http.Header, error) {
	if len(body) == 0 {
		body = quotaHaltFallbackBody
	}
	callLogEvent(ctx, "quota_halt", "", "blocked: 无可用节点且直连出口 429，等待出口恢复")
	h := make(http.Header)
	h.Set("Retry-After", "30")
	return body, http.StatusServiceUnavailable, h, nil
}

// snapshot 导出各出口桶的状态（测试与面板可观测）。
func (h *quotaHalt) snapshot() map[string]haltBucketView {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := map[string]haltBucketView{}
	now := time.Now()
	for k, s := range h.states {
		out[k] = haltBucketView{Halted: s.halted, Recovered: s.recovered(now), Streak: s.streak}
	}
	return out
}

type haltBucketView struct {
	Halted    bool
	Recovered bool
	Streak    int
}

// logState 把各出口桶状态打进日志（调试用）。
func (h *quotaHalt) logState(event string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for k, s := range h.states {
		slog.Info(event, "egress", k, "halted", s.halted, "streak", s.streak, "reason", s.reason)
	}
}
