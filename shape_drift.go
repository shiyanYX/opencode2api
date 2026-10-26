package main

// ======================== 形态漂移检测（shape drift guard） ========================
//
// 2026-10-25 实验定案：上游免费层把【形态被拒】伪装成 429 FreeUsageLimitError
// （与真额度耗尽不可区分），反复触发会升级为 IP 当日封禁。见 oc_shape_v2.go
// 文件头与 bun_transport.go 文件头。
//
// 2026-10-26 事故：旧实现只记录「最近一次」，且解除条件只有「窗口滑空」。
// 被钉住的出口持续 429 会不断刷新自己的时间戳 ⇒ 窗口永不空 ⇒ 永久闩死
// （18:00–23:00 每条失败请求 nodes 长度=1）。本版修复：
//   - 每个出口只记【首次】429；窗口过期即删除。
//   - 解除三条任一：窗口空 / 任一出口真实 2xx / 硬 TTL 10min。
//   - armed 后半开试探：按 model 的 60→120→300s 退避，叠加 ≤20% 抖动；
//     全局最多每 5s 放行一次试探，避免所有请求同一毫秒惊群。
//     试探成功立即清闩；再 429 ⇒ 升级退避（上限 300s）。
//   - 状态按 model 分桶；模型名来自客户端，必须有 512 上限 + 空闲淘汰，
//     防止 OpenAI 兼容入口被随机 "model":"xxx1" 打爆内存。
//
// 判据刻意保守：只统计 FreeUsageLimitError 签名。窗口、TTL、阈值做成 var
// 供测试/演练调整。

import (
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

var (
	shapeDriftMu      sync.Mutex
	shapeDriftByModel = map[string]*shapeDriftModelState{} // model -> 状态

	shapeDriftWindow    = 10 * time.Minute
	shapeDriftTTL       = 10 * time.Minute // armed 后的硬上限，不依赖窗口内是否仍有记录
	shapeDriftMinimum   = 3
	shapeDriftTrialBase = 60 * time.Second
	shapeDriftTrialMax  = 300 * time.Second

	// 客户端可控 model 名的内存护栏。超出后淘汰最久未活跃的桶。
	shapeDriftMaxModels = 512
	shapeDriftModelIdle = 30 * time.Minute

	// 全局半开试探节流：所有 model 合计最多每 5s 放行一次，单次请求失败后
	// 不会让同一毫秒的 N 个请求一起换出口。
	shapeDriftTrialGlobalMin  = 5 * time.Second
	shapeDriftLastGlobalTrial time.Time

	// 抖动：在退避上限内随机加 0–20%，把同一窗口内的重试摊开。
	// 测试可注入恒等函数以获得确定的边界断言。
	shapeDriftJitterFn = func(d time.Duration) time.Duration {
		if d <= 0 {
			return d
		}
		return d + time.Duration(rand.Float64()*0.20*float64(d))
	}

	shapeDriftLastSweep   time.Time
	shapeDriftLastCapWarn time.Time
)

type shapeDriftModelState struct {
	seen          map[string]time.Time // egressKey -> 首次 429 时间（同一出口不刷新）
	armed         bool
	armedAt       time.Time
	lastTrial     time.Time
	trialBackoff  time.Duration
	trialInFlight bool   // 已放行一次试探，等待该次请求的 2xx/429 结果
	trialFrom     string // 放行试探时的旧出口；未设置 target 时用于兼容测试/旧调用
	trialEgress   string // 试探实际切到的出口；只有它的 429 才算试探失败
	lastSeen      time.Time
}

type shapeDriftDecision struct {
	Halt      bool
	Trial     bool
	Confirmed bool
	Armed     bool
	Distinct  int
	LastTrial time.Time
	Blocked   map[string]bool
}

func newShapeDriftModelState() *shapeDriftModelState {
	return &shapeDriftModelState{
		seen:         map[string]time.Time{},
		trialBackoff: shapeDriftTrialBase,
	}
}

func (b *shapeDriftModelState) resetLocked(clearSeen bool) {
	b.armed = false
	b.armedAt = time.Time{}
	b.lastTrial = time.Time{}
	b.trialBackoff = shapeDriftTrialBase
	b.trialInFlight = false
	b.trialFrom = ""
	b.trialEgress = ""
	if clearSeen {
		b.seen = map[string]time.Time{}
	}
}

// expireLocked 清理过期/时钟跳变状态。时钟往回跳时（now 早于任一记录）
// 直接重置该 model，避免旧闩被“未来时间”永久保护。
func (b *shapeDriftModelState) expireLocked(now time.Time) {
	if b.armed && !b.armedAt.IsZero() && now.Before(b.armedAt) {
		b.resetLocked(true)
		return
	}
	for key, first := range b.seen {
		if now.Before(first) {
			b.resetLocked(true)
			return
		}
		if now.Sub(first) > shapeDriftWindow {
			delete(b.seen, key)
		}
	}
	if b.armed && !b.armedAt.IsZero() && now.Sub(b.armedAt) >= shapeDriftTTL {
		// 硬 TTL：清闩并清空该 model 的 429 集合，避免刚清闩又被旧记录立刻 arm。
		b.resetLocked(true)
		return
	}
	if b.armed && len(b.seen) == 0 {
		// 窗口滑空：解除，恢复正常配额切换。
		b.resetLocked(false)
	}
}

func (b *shapeDriftModelState) blockedCopyLocked() map[string]bool {
	out := make(map[string]bool, len(b.seen))
	for k := range b.seen {
		out[k] = true
	}
	return out
}

func (b *shapeDriftModelState) trialDelay() time.Duration {
	d := b.trialBackoff
	if d <= 0 {
		d = shapeDriftTrialBase
	}
	if shapeDriftJitterFn != nil {
		d = shapeDriftJitterFn(d)
	}
	if d < 0 {
		d = 0
	}
	return d
}

func nextShapeDriftTrialBackoff(d time.Duration) time.Duration {
	switch {
	case d < 120*time.Second:
		return 120 * time.Second
	case d < shapeDriftTrialMax:
		return shapeDriftTrialMax
	default:
		return shapeDriftTrialMax
	}
}

// sortedBlockedKeys 返回稳定的候选/闩集合键序，供结构化事件与回放使用。
func sortedBlockedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// maybeSweepLocked 低频清理空闲 model 桶；避免 10 万随机模型名长期占用 map。
func maybeSweepLocked(now time.Time) {
	if !shapeDriftLastSweep.IsZero() && !now.Before(shapeDriftLastSweep) &&
		now.Sub(shapeDriftLastSweep) < time.Second {
		return
	}
	shapeDriftLastSweep = now
	for model, b := range shapeDriftByModel {
		b.expireLocked(now)
		if !b.armed && len(b.seen) == 0 && !b.lastSeen.IsZero() &&
			now.Sub(b.lastSeen) > shapeDriftModelIdle {
			delete(shapeDriftByModel, model)
		}
	}
}

// evictOneModelLocked 淘汰最久未活跃的 model 桶。所有桶都活跃时也淘汰，
// 内存上限优先；同时限频告警，避免攻击者用随机模型名制造日志风暴。
func evictOneModelLocked(now time.Time) {
	var victim string
	var oldest time.Time
	for model, b := range shapeDriftByModel {
		if victim == "" || b.lastSeen.Before(oldest) {
			victim = model
			oldest = b.lastSeen
		}
	}
	if victim == "" {
		return
	}
	delete(shapeDriftByModel, victim)
	if shapeDriftLastCapWarn.IsZero() || now.Sub(shapeDriftLastCapWarn) >= time.Minute {
		shapeDriftLastCapWarn = now
		slog.Warn("shape_drift_model_cap_evicted",
			"cap", shapeDriftMaxModels,
			"evicted_model", truncateLogValue(victim, 64),
			"models", len(shapeDriftByModel),
			"detail", "client-controlled model names reached shape drift bucket cap; evicting least-recently-seen model",
		)
	}
}

func truncateLogValue(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func ensureShapeDriftModelLocked(model string, now time.Time) *shapeDriftModelState {
	b := shapeDriftByModel[model]
	if b == nil {
		maybeSweepLocked(now)
		if len(shapeDriftByModel) >= shapeDriftMaxModels {
			evictOneModelLocked(now)
		}
		b = newShapeDriftModelState()
		shapeDriftByModel[model] = b
	}
	b.lastSeen = now
	return b
}

// shapeDriftObserve429 记录一次 FreeUsageLimitError，并返回本次是否应停切换/是否放行试探。
// egressKey 是报错出口的分桶键（EgressResult.EgressKey()），model 是调用方的模型 ID。
func shapeDriftObserve429(model, egressKey, quotaReason string, now time.Time) shapeDriftDecision {
	d := shapeDriftDecision{}
	if !isFreeUsageLimitReason(quotaReason) {
		return d
	}

	shapeDriftMu.Lock()
	defer shapeDriftMu.Unlock()

	b := ensureShapeDriftModelLocked(model, now)
	b.expireLocked(now)
	maybeSweepLocked(now)

	// 只记首次时间；同一出口重复 429 绝不刷新。
	if _, ok := b.seen[egressKey]; !ok {
		b.seen[egressKey] = now
	}

	if !b.armed && len(b.seen) >= shapeDriftMinimum {
		b.armed = true
		b.armedAt = now
		b.lastTrial = now // 从触发时刻起算半开窗口
		b.trialBackoff = shapeDriftTrialBase
		b.trialInFlight = false
		b.trialFrom = ""
		b.trialEgress = ""
		d.Confirmed = true
	}

	if b.armed {
		if !shapeDriftLastGlobalTrial.IsZero() && now.Before(shapeDriftLastGlobalTrial) {
			// 时钟回跳：清掉全局节流时间，避免“未来”时间锁死。
			shapeDriftLastGlobalTrial = time.Time{}
		}
		switch {
		case b.trialInFlight && b.trialEgress != "" && egressKey == b.trialEgress:
			// 试探目标出口又 429：收口并升级退避。
			b.trialInFlight = false
			b.trialEgress = ""
			b.trialBackoff = nextShapeDriftTrialBackoff(b.trialBackoff)
			b.lastTrial = now
		case b.trialInFlight && b.trialEgress == "" && egressKey == b.trialFrom:
			// 兼容：调用方未上报试探目标时，旧出口的 429 视为试探失败。
			b.trialInFlight = false
			b.trialBackoff = nextShapeDriftTrialBackoff(b.trialBackoff)
			b.lastTrial = now
		case b.trialInFlight:
			// 并发请求在旧出口上的 429：不是这次试探的结果，不能升级退避。
		case b.lastTrial.IsZero() || now.Sub(b.lastTrial) >= b.trialDelay():
			// 半开：全局每 5s 最多放行一次，避免惊群。
			if shapeDriftLastGlobalTrial.IsZero() ||
				now.Sub(shapeDriftLastGlobalTrial) >= shapeDriftTrialGlobalMin {
				shapeDriftLastGlobalTrial = now
				b.trialInFlight = true
				b.trialFrom = egressKey
				b.trialEgress = ""
				b.lastTrial = now
				d.Trial = true
			}
		}
	}

	d.Armed = b.armed
	d.Distinct = len(b.seen)
	d.LastTrial = b.lastTrial
	d.Blocked = b.blockedCopyLocked()
	d.Halt = b.armed && !d.Trial
	return d
}

// shapeDriftSetTrialEgress 由 main 在试探请求真正选到新出口后调用。
// 只有该出口的 429 才被算作本次试探失败；并发旧出口的 429 不再误升级退避。
func shapeDriftSetTrialEgress(model, egressKey string, now time.Time) {
	shapeDriftMu.Lock()
	defer shapeDriftMu.Unlock()
	b := shapeDriftByModel[model]
	if b == nil || !b.trialInFlight {
		return
	}
	b.lastSeen = now
	b.trialEgress = egressKey
}

// shapeDriftObserveSuccess 由真实 2xx 路径调用：任一出口成功即清掉该 model 的闩。
func shapeDriftObserveSuccess(model, _ string, now time.Time) {
	shapeDriftMu.Lock()
	defer shapeDriftMu.Unlock()
	b := shapeDriftByModel[model]
	if b == nil {
		return
	}
	b.lastSeen = now
	b.expireLocked(now)
	b.resetLocked(true)
}

// shapeDriftBlockedEgress 返回该 model 当前窗口内已 429 的出口集合（候选排除用）。
func shapeDriftBlockedEgress(model string, now time.Time) map[string]bool {
	shapeDriftMu.Lock()
	defer shapeDriftMu.Unlock()
	maybeSweepLocked(now)
	b := shapeDriftByModel[model]
	if b == nil {
		return map[string]bool{}
	}
	b.lastSeen = now
	b.expireLocked(now)
	return b.blockedCopyLocked()
}

// shapeDriftSnapshotAt 暴露 armed / distinct / last_trial（日志、面板与测试用）。
func shapeDriftSnapshotAt(model string, now time.Time) (armed bool, distinct int, lastTrial time.Time) {
	shapeDriftMu.Lock()
	defer shapeDriftMu.Unlock()
	maybeSweepLocked(now)
	b := shapeDriftByModel[model]
	if b == nil {
		return false, 0, time.Time{}
	}
	b.expireLocked(now)
	return b.armed, len(b.seen), b.lastTrial
}

func shapeDriftSnapshot(model string) (armed bool, distinct int, lastTrial time.Time) {
	return shapeDriftSnapshotAt(model, time.Now())
}

func shapeDriftModelCount() int {
	shapeDriftMu.Lock()
	defer shapeDriftMu.Unlock()
	return len(shapeDriftByModel)
}

// shapeDriftShouldHalt 保留旧的非 model 化入口（仅兼容旧调用/测试）；新代码用
// shapeDriftObserve429，按 model 分桶。
func shapeDriftShouldHalt(egressKey, quotaReason string, now time.Time) (halt, driftConfirmed bool) {
	d := shapeDriftObserve429("", egressKey, quotaReason, now)
	return d.Halt, d.Confirmed
}

// isFreeUsageLimitReason 报告 quotaReason 是否 FreeUsageLimitError 签名
// （type 或 keyword 命中皆算——上游两种错误体形态都出现过）。
func isFreeUsageLimitReason(quotaReason string) bool {
	return quotaReason == "type:FreeUsageLimitError" ||
		quotaReason == "keyword:free usage limit"
}
