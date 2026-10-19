package main

// C-5 节点层告警。
//
// 为什么需要它（2026-10-19 生产事故）：节点池里 227 个节点、面板显示 115 个
// available，但连续 8 小时【所有】节点出口请求 100% 失败（connect EOF），
// 真实流量全靠 direct 出口在撑。也就是说：
//
//	「池子看起来健康」和「池子真的能用」是两件独立的事。
//
// 现有告警只覆盖单个节点被判死/熔断闸门，没有任何东西覆盖「整层全灭」，
// 于是这种事故只能靠人肉翻日志发现 —— 晚了 8 小时。
//
// 判定两条，任一成立即告警（window 内统计，滑动窗口，不是"每 N 分钟清零"）：
//
//	A. 窗口内节点出口尝试 ≥ nodeAlertMinSamples 次、成功率 = 0%、
//	   且最早的样本已过去 ≥ nodeAlertMinSpan（防单次抖动的密集爆发误报）。
//	B. 池中 total > 0 但 available = 0（池子空了；与有没有流量无关）。
//
// 三条刻意的边界：
//   - direct 出口的成败不计入（direct 不是节点，它好好的不代表节点可用）；
//   - 只有【传输层】失败才算节点失败：拿到任何上游响应（含 403/429）都说明
//     这条出口是通的，账号级限流不该算到节点头上；
//   - 少量样本不判、短促爆发不判（C-6 已证明单轮抖动能让整池集体超时）。

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

var (
	// 滑动统计窗口：只看最近这么久的节点出口尝试。
	nodeAlertWindow = 10 * time.Minute
	// 窗口内最少样本数：低于它一律不判（避免 2 次失败就报警）。
	nodeAlertMinSamples = 10
	// 最早样本至少已过去这么久：把"一次请求连翻 10 个节点、几秒打完"的
	// 密集爆发排除掉 —— 那是单次抖动，不是节点层持续全灭。
	nodeAlertMinSpan = 3 * time.Minute
	// 同一场故障的重复告警冷却：故障持续 8 小时不该刷 8 小时日志，
	// 但冷却期内告警态保持 Active（面板横幅不闪断），冷却过后再提醒一次。
	nodeAlertCooldown = 30 * time.Minute
	// 样本上限：正常流量下窗口裁剪已足够，这层是内存兜底。
	nodeAlertMaxSamples = 512
)

type nodeAttemptSample struct {
	at time.Time
	ok bool // 传输层是否拿到上游响应（err == nil）
}

// nodeAlertMonitor 记录节点出口的尝试样本并判定是否告警。
type nodeAlertMonitor struct {
	mu      sync.Mutex
	nowFn   func() time.Time
	samples []nodeAttemptSample

	active      bool
	reason      string
	since       time.Time
	lastAlertAt time.Time

	poolAvailable int
	poolTotal     int
}

// nodeAlertSnapshot 是给 /api/nodes（进而给面板）看的告警快照。
// WindowSuccessRate 为 nil 表示窗口内没有样本 => 面板必须渲染「无数据」，
// 而不是 0%（0% 会被读成"节点全挂了"）。
type nodeAlertSnapshot struct {
	Active            bool     `json:"active"`
	Reason            string   `json:"reason,omitempty"`
	Since             string   `json:"since,omitempty"`
	WindowMinutes     float64  `json:"window_minutes"`
	WindowAttempts    int      `json:"window_attempts"`
	WindowSuccesses   int      `json:"window_successes"`
	WindowSuccessRate *float64 `json:"window_success_rate"`
	PoolAvailable     int      `json:"pool_available"`
	PoolTotal         int      `json:"pool_total"`
}

func newNodeAlertMonitor() *nodeAlertMonitor {
	return &nodeAlertMonitor{nowFn: time.Now}
}

// nodeAlerts 是进程级单例：两个上游重试循环往里喂样本，健康检查循环负责评估。
var nodeAlerts = newNodeAlertMonitor()

// recordNodeAttempt 记录一次上游尝试。usedNode=false（走 direct 出口）直接忽略。
func (m *nodeAlertMonitor) recordNodeAttempt(usedNode, transportOK bool) {
	if !usedNode {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.nowFn()
	m.samples = append(m.samples, nodeAttemptSample{at: now, ok: transportOK})
	if len(m.samples) > nodeAlertMaxSamples {
		m.samples = m.samples[len(m.samples)-nodeAlertMaxSamples:]
	}
}

// windowAt 裁剪出窗口内样本并统计。调用方必须已持锁。
func (m *nodeAlertMonitor) windowAt(now time.Time) (attempts, successes int) {
	cut := now.Add(-nodeAlertWindow)
	i := 0
	for i < len(m.samples) && m.samples[i].at.Before(cut) {
		i++
	}
	if i > 0 {
		m.samples = append([]nodeAttemptSample(nil), m.samples[i:]...)
	}
	for _, s := range m.samples {
		if s.ok {
			successes++
		}
	}
	return len(m.samples), successes
}

// evaluate 判定当前是否应告警。返回 true 时调用方负责把 reason 写进日志，
// 返回 false 且告警态由 active 转 false 时，本函数已打出恢复日志。
func (m *nodeAlertMonitor) evaluate(now time.Time, poolAvailable, poolTotal int) (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.poolAvailable, m.poolTotal = poolAvailable, poolTotal
	attempts, successes := m.windowAt(now)

	reason := ""
	switch {
	case poolTotal > 0 && poolAvailable == 0:
		reason = fmt.Sprintf("节点池无可用节点：available=0（共 %d 个），流量只能走 direct 出口", poolTotal)
	case attempts >= nodeAlertMinSamples && successes == 0 && now.Sub(m.samples[0].at) >= nodeAlertMinSpan:
		reason = fmt.Sprintf("最近 %v 内 %d 次节点出口尝试成功率 0%%（全部没拿到上游响应）",
			nodeAlertWindow, attempts)
	}

	if reason == "" {
		if m.active {
			slog.Info("node_layer_alert_cleared",
				"window_attempts", attempts, "window_successes", successes,
				"pool_available", poolAvailable, "pool_total", poolTotal)
		}
		m.active, m.reason, m.since = false, "", time.Time{}
		return false, ""
	}

	// 已在告警中且未过冷却：不重复刷屏，但保持 Active 并刷新原因
	//（原因可能从"成功率 0%"变成"池子空了"）。
	if m.active && now.Sub(m.lastAlertAt) < nodeAlertCooldown {
		m.reason = reason
		return false, ""
	}
	if !m.active {
		m.since = now
	}
	m.active, m.reason, m.lastAlertAt = true, reason, now
	return true, reason
}

// snapshot 供 /api/nodes 使用：窗口统计按"当前时刻"重算，不依赖上次评估时间。
func (m *nodeAlertMonitor) snapshot() nodeAlertSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.nowFn()
	attempts, successes := m.windowAt(now)
	snap := nodeAlertSnapshot{
		Active:          m.active,
		Reason:          m.reason,
		WindowMinutes:   nodeAlertWindow.Minutes(),
		WindowAttempts:  attempts,
		WindowSuccesses: successes,
		PoolAvailable:   m.poolAvailable,
		PoolTotal:       m.poolTotal,
	}
	if !m.since.IsZero() {
		snap.Since = m.since.Format(time.RFC3339)
	}
	if attempts > 0 {
		rate := float64(successes) / float64(attempts)
		snap.WindowSuccessRate = &rate
	}
	return snap
}
