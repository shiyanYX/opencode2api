package main

// C-5 节点层告警。
//
// 为什么需要它（2026-10-19 生产事故）：节点池里 227 个节点、面板显示 115 个
// available，但节点出口整层不可用，真实流量全靠 direct 出口在撑。也就是说：
//
//	「池子看起来健康」和「池子真的能用」是两件独立的事。
//
// v3（task-17）修 v2 被独立验收判 no-go 的四条缺陷（docs/research/C5-v2-verification.md §4.1–4.4）：
//
//	D3 漏报：lastRun*（已结束段）原先**无条件覆盖**，被后随的 1/2/3/11 次小段挤掉 ⇒
//	  生产回放 168 段「承诺必报」的零成功段漏 9 段。v3 只在「新段更大 / 已存段已过期
//	  （now-lastRunEnd > 窗口）/ 还没存过」时覆盖（rememberEndedRunLocked）。
//	D1 永不恢复：burst 分支（连败 >=100）原先没吃 stale ⇒ 6 小时兜底只清一拍，下一拍又被
//	  burst 拉回来（实测 6.8 小时 120 次翻转）。v3 给 burst 补 `!stale`，与 B/B' 一致。
//	D2 闪烁：恢复证据清掉告警时没有丢弃已结束段 ⇒ 旧段（仍在窗口内）下一拍把告警拉回来
//	  （生产回放 32 次报→清→报）。v3 在**真正清除**的那一刻把 lastRun* 一并归零 ——
//	  恢复证据已经推翻了那段旧数据，它不该再参与判据。
//	D4 跨度用墙钟：段跨度原先是 now-runStart，于是「43 连败、真实跨度 31 秒」被写成
//	  「跨 14h18m45s」。v3 段跨度一律按**样本自身**算：进行中的段 = runLastAt-runStart，
//	  已结束的段 = lastRunEnd-lastRunStart；now 只用于 stale 与冷却，不再冒充段跨度。
//	  ★ 一处**刻意保留**（修复说明 §5 有完整交代）：A/A' 的最短跨度仍用「窗口内最早样本 → now」
//	  （`oldest`），因为它表达的是「这个窗口已观察满 3 分钟且零成功」；且验收方的只读测试
//	  TestMV2_MinSpanGateStillHolds 明确把该行为钉成预期（短促爆发 + 3 分钟零成功 ⇒ 必报）。
//	  改成失败样本自身跨度会让那条不可改的测试变红；要收紧这一条需验收方同步改测试。
//
// v1（b30b939）被独立对抗性验收判 no-go，阻断项全是设计缺陷，本文件是 v2：
//
//	F1 漏报：v1 的跨度锚点取「窗口内最老样本」，于是「两簇失败 + 中间一段无流量」
//	   （生产 00:16:13..00:33:42，60 次尝试零成功，跨 17m29s）任何窗口都不满足 ⇒ 全程静默。
//	F2 误清：v1 判据是 successes==0，且清除不需要任何证据，一次成功就整段清掉
//	   ⇒ 事故里 Active 只覆盖 5 分钟 / 8 小时（1.0%）。
//	F4 高密度盲区：v1 的 512 样本上限 + 跨度闸在 >≈170 次/分钟时永不成立。
//	F5 97% 失败不报：v1 只在「严格 0 成功」时报，每 9 分钟来一次成功就永不告警。
//
// v2 判据（任一成立 ⇒ 告警）：
//
//	A  速率      ：10 分钟窗口内 ≥20 次尝试、成功率 ≤35%、且窗口跨度 ≥3 分钟；
//	A' 稀疏速率  ：10 分钟窗口内 ≥10 次尝试、成功率 ≤20%、且窗口跨度 ≥3 分钟（低流量场景）；
//	B  连续失败段：连续失败 ≥20 次且该段跨度 ≥3 分钟（跨空档有效，不受样本上限裁剪）；
//	              或连续失败 ≥100 次（高密度爆发不必等跨度，修 F4）；
//	B' 慢速失败段：连续失败 ≥10 次且该段跨度 ≥10 分钟（每次尝试都很慢的低流量场景）；
//	C  池空      ：available=0 且 total>0。
//
// 统计不再存样本数组，而是**每分钟一格**的环形桶（O(1) 内存、无上限裁剪 ⇒ 修 F4），
// 连续失败段单独维护计数器与段起点，锚点是「故障起点」而不是「窗口最老样本」⇒ 修 F1。
//
// 清除必须**有恢复证据**（修 F2），而且**证据优先于判据**（证据描述的是「刚刚发生的
// 事实」，比窗口里的旧样本可信）。三条任一：
//   - 窗口 ≥20 次样本且成功率 ≥80%（主力路径：证据强度与样本量匹配）；
//   - 连续成功 ≥20 次（补「窗口样本不足」这条边角；门槛必须高 —— 生产回放实测
//     设成 5 时，15% 成功率的坏层里也会出现 5 连胜，告警被反复清掉）；
//   - 池里一个节点都没有（total==0，此时「节点层告警」没有意义），或超过 6 小时完全
//     没有节点尝试（兜底，防止横幅永久卡死）。
//
// 反过来，**没有判据成立不等于恢复**：流量被切走、窗口变空时告警保持 Active，只把原因
// 换成「尚未恢复到健康线」的实时口径。这是刻意的 —— 那正是 8 小时事故里最危险的形态：
// 节点层坏了 → 流量绕开节点 → 没有坏样本 → 面板一片安静。
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

// 监控内存有 10 个桶、一格一分钟 —— 即窗口长度。
const nodeAlertBucketCount = 10

var (
	// 滑动统计窗口（桶的粒度是一分钟，所以实际窗口是 10~11 分钟）。
	nodeAlertWindow = time.Duration(nodeAlertBucketCount) * time.Minute
	// A：速率判据 —— 窗口内至少这么多样本，成功率低于阈值即告警。
	nodeAlertRateSamples = 20
	nodeAlertRateMax     = 0.35
	// A'：低流量版本的速率判据（样本更少，所以要求成功率更低才判）。
	nodeAlertSparseSamples = 10
	nodeAlertSparseRateMax = 0.20
	// 窗口/失败段的最短跨度：把「一次请求连翻若干节点、几秒打完」的单次抖动排除掉。
	nodeAlertMinSpan = 3 * time.Minute
	// B：连续失败段。
	nodeAlertRunFailures = 20
	nodeAlertRunBurst    = 100 // 连续失败到这个数就不必再等跨度（高密度爆发）
	// B'：慢速失败段（每次尝试都很慢时的低流量形态）。
	nodeAlertSlowRunFailures = 10
	nodeAlertSlowRunSpan     = 10 * time.Minute
	// 恢复证据。
	nodeAlertClearSamples = 20
	nodeAlertClearRate    = 0.80
	// 连续成功多少次要单独作为恢复证据：只有当窗口样本**不足**时它才划算 ——
	// 生产回放实测：门槛设 5 时，15% 成功率的坏层里照样会出现 5 连胜，
	// 于是告警被反复清掉（日志里 window_successes=9/18 就清了），事故窗覆盖率掉到 16.9%。
	// 20 连胜在坏层里基本不可能出现，同时把"低流量时窗口样本不足"这条边角补上。
	nodeAlertClearStreak = 20
	// 超过这么久没有任何节点尝试 ⇒ 数据陈旧、不足以判定（兜底清除）。
	nodeAlertStaleAfter = 6 * time.Hour
	// 同一场故障的重复告警冷却：故障持续 8 小时不该刷 8 小时日志，
	// 但冷却期内告警态保持 Active（面板横幅不闪断），冷却过后再提醒一次。
	nodeAlertCooldown = 30 * time.Minute
)

type nodeAlertBucket struct {
	minute          int64 // Unix 分钟
	n, succ         int
	firstAt, lastAt time.Time
}

// nodeAlertMonitor 记录节点出口的尝试并判定是否告警。
type nodeAlertMonitor struct {
	mu      sync.Mutex
	nowFn   func() time.Time
	buckets [nodeAlertBucketCount]nodeAlertBucket

	// 连续失败段：runStart 是**首个失败**的时刻，不是窗口最老样本。
	runFailures   int
	runStart      time.Time
	runLastAt     time.Time
	successStreak int
	lastAttemptAt time.Time

	// 刚结束、但仍在统计窗口内的失败段。为什么需要它：生产回放里有一段
	// 08-19 21:41:14..22:00:45（10 次尝试、19m31s、零成功）完全没告警 ——
	// 第 10 次失败之后紧接着来了一个成功，下一拍评估时 runFailures 已被清零，
	// 判据成立与否变成了"采样时刻撞得准不准"。记住已结束的段，判据就不再依赖采样时点。
	lastRunFailures int
	lastRunStart    time.Time
	lastRunEnd      time.Time

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
	RunFailures       int      `json:"run_failures"`
	RunMinutes        float64  `json:"run_minutes"`
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
	m.lastAttemptAt = now
	if transportOK {
		m.successStreak++
		if m.runFailures > 0 { // 这一段失败了，记下来（仍在窗口内时参与判定）
			// ★ D3：不能无条件覆盖 —— 后随的小段（1/2/3/11 次）会把 20~40 次的大段挤掉。
			m.rememberEndedRunLocked(now)
		}
		m.runFailures, m.runStart = 0, time.Time{}
	} else {
		// ★ 失败必须把连胜清零：漏了这一步，「连续成功」这条恢复证据会永远成立，
		// 告警会被反复误清（v2 开发时在模拟脚本里真踩过一次）。
		m.successStreak = 0
		if m.runFailures == 0 {
			m.runStart = now
		}
		m.runFailures++
		m.runLastAt = now
	}
	m.addLocked(now, transportOK)
}

// rememberEndedRunLocked 记下一段刚结束（最后一次失败 = runLastAt）的失败段。
// 调用方必须已持锁，且在调用前 runFailures > 0。
//
// ★ D3：只在三种情况下覆盖已存的段 ——
//  1. 还没存过（lastRunEnd 为零值）；
//  2. 新段**更大**（失败次数更多）：大段是更强的故障证据，小段不该挤掉它；
//  3. 已存段**已过期**（now-lastRunEnd 超过窗口）：不让一个很老的大段永久占位，
//     否则后面的正常小段永远进不来，judgedRun 会拿着过时的证据反复判。
//
// 生产回放（v2）：168 段「承诺必报」的零成功段里 9 段全程静默，签名都是
// 「段结束后第一拍 lastRun*=1/2/3/11（被小段覆盖）、当前段又是 0 次」⇒ 判据全落空。
func (m *nodeAlertMonitor) rememberEndedRunLocked(now time.Time) {
	if m.runFailures <= 0 {
		return
	}
	if m.runFailures > m.lastRunFailures || m.lastRunEnd.IsZero() || now.Sub(m.lastRunEnd) > nodeAlertWindow {
		m.lastRunFailures, m.lastRunStart, m.lastRunEnd = m.runFailures, m.runStart, m.runLastAt
	}
}

// addLocked 把一次尝试记进它所属的分钟桶。
func (m *nodeAlertMonitor) addLocked(at time.Time, ok bool) {
	minute := at.Unix() / 60
	idx := minute % nodeAlertBucketCount
	if idx < 0 {
		idx += nodeAlertBucketCount
	}
	b := &m.buckets[idx]
	if b.minute != minute || b.n == 0 {
		*b = nodeAlertBucket{minute: minute, firstAt: at}
	}
	b.n++
	b.lastAt = at
	if ok {
		b.succ++
	}
}

// windowAt 统计窗口内的尝试数、成功数与最老样本时刻。调用方必须已持锁。
// 未来样本（minute 晚于 now 所在分钟）不计入 —— 判定的前提是「已经发生的事实」。
func (m *nodeAlertMonitor) windowAt(now time.Time) (attempts, successes int, oldest time.Time) {
	nowMinute := now.Unix() / 60
	cutMinute := now.Add(-nodeAlertWindow).Unix() / 60
	for i := range m.buckets {
		b := &m.buckets[i]
		if b.n == 0 || b.minute > nowMinute || b.minute < cutMinute {
			continue
		}
		attempts += b.n
		successes += b.succ
		if oldest.IsZero() || b.firstAt.Before(oldest) {
			oldest = b.firstAt
		}
	}
	return attempts, successes, oldest
}

// judgedRun 返回判定用的失败段：优先「当前正在进行」的，否则「刚结束且仍在窗口内」的
// （见 lastRunFailures 字段上的说明）。evaluate 与 snapshot 必须用同一份口径，
// 否则会出现"报了但快照看不出为什么报"——验收方就按快照核对判据，会直接判成误报。
//
// ★ D4：跨度只按**样本自身**算，不用墙钟 ——
//   - 进行中的段：end = runLastAt（最后一次失败的时刻），不是 now。否则一次 31 秒打完的
//     43 连败，在之后 14 小时没有新样本时会被写成「跨 14h18m45s」（真实请求里的文案 bug）；
//   - 已结束的段：end = lastRunEnd；
//   - `now` 只用于「已结束段是否还在窗口内」这个新鲜度判断（now-lastRunEnd <= 窗口），
//     它是**过期判断**，不是段跨度。
func (m *nodeAlertMonitor) judgedRun(now time.Time) (n int, start time.Time, span time.Duration) {
	n, start, end := m.runFailures, m.runStart, m.runLastAt
	if m.lastRunFailures > n && !m.lastRunEnd.IsZero() && now.Sub(m.lastRunEnd) <= nodeAlertWindow {
		n, start, end = m.lastRunFailures, m.lastRunStart, m.lastRunEnd
	}
	// ★ 未来样本（时钟回拨）不得冒充跨度：end 只取【已经发生】的样本时刻。
	// 回拨到段起点之前时 end<=start ⇒ span=0（验收方 TestMV2_ClockRollbackAndFutureSamples
	// 明确要求回拨后 run_minutes=0，且未来样本不得触发告警）。
	if end.After(now) {
		end = now
	}
	if !start.IsZero() && end.After(start) {
		span = end.Sub(start)
	}
	return n, start, span
}

// evaluate 判定当前是否应告警。返回 true 时调用方负责把 reason 写进日志；
// 返回 false 且告警态由 active 转 false 时，本函数已打出恢复日志。
func (m *nodeAlertMonitor) evaluate(now time.Time, poolAvailable, poolTotal int) (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.poolAvailable, m.poolTotal = poolAvailable, poolTotal

	attempts, successes, oldest := m.windowAt(now)
	rate := -1.0
	if attempts > 0 {
		rate = float64(successes) / float64(attempts)
	}
	stale := !m.lastAttemptAt.IsZero() && now.Sub(m.lastAttemptAt) > nodeAlertStaleAfter
	poolEmpty := poolTotal > 0 && poolAvailable == 0

	runN, runStart, runSpan := m.judgedRun(now)

	reason := ""
	switch {
	case poolEmpty:
		reason = fmt.Sprintf("节点池无可用节点：available=0（共 %d 个），流量只能走 direct 出口", poolTotal)
	case attempts >= nodeAlertRateSamples && rate >= 0 && rate <= nodeAlertRateMax &&
		!oldest.IsZero() && now.Sub(oldest) >= nodeAlertMinSpan:
		reason = fmt.Sprintf("最近 %v 内 %d 次节点出口尝试只有 %d 次成功（成功率 %.0f%%），低于 %.0f%% 的健康线",
			nodeAlertWindow, attempts, successes, rate*100, nodeAlertRateMax*100)
	case attempts >= nodeAlertSparseSamples && rate >= 0 && rate <= nodeAlertSparseRateMax &&
		!oldest.IsZero() && now.Sub(oldest) >= nodeAlertMinSpan:
		reason = fmt.Sprintf("最近 %v 内 %d 次节点出口尝试只有 %d 次成功（成功率 %.0f%%）",
			nodeAlertWindow, attempts, successes, rate*100)
	case runN >= nodeAlertRunBurst && !stale:
		// ★ D1：burst 分支必须和 B/B' 一样吃 `!stale`。否则 ≥100 连败之后哪怕 6 小时
		// 完全没有尝试，runFailures 仍是 100+ ⇒ stale 兜底清一拍、下一拍又被拉回来，
		// 横幅永久复亮（实测 6.8 小时 120 次翻转）。
		reason = fmt.Sprintf("节点出口已连续失败 %d 次（期间零成功），整层基本不可用", runN)
	case runN >= nodeAlertRunFailures && runSpan >= nodeAlertMinSpan && !stale:
		reason = fmt.Sprintf("节点出口自 %s 起已连续失败 %d 次、跨 %v，期间零成功",
			runStart.Format("15:04:05"), runN, runSpan.Round(time.Second))
	case runN >= nodeAlertSlowRunFailures && runSpan >= nodeAlertSlowRunSpan && !stale:
		reason = fmt.Sprintf("节点出口自 %s 起已连续失败 %d 次、跨 %v，期间零成功",
			runStart.Format("15:04:05"), runN, runSpan.Round(time.Second))
	}

	// ★ 恢复证据优先于**判据**（证据是「刚刚发生的事实」，比窗口里的旧失败样本可信），
	// 但必须排在判据**之后**才能拿到 reason —— 反过来的话判据一旦成立，
	// 恢复分支就永远进不去（v2 开发时真踩过这个坑：连清都清不掉）。
	//
	// 三条边界：
	//   - 池空（poolEmpty）期间不接受恢复证据：池状态是权威事实，清掉只会立刻重报、横幅闪烁；
	//   - poolTotal==0 只有在**没有任何判据成立**时才当恢复证据。否则它会和流量判据对打：
	//     判据说"报"、它说"清"，一拍报一拍清，ERROR 日志刷屏（验收方 W07 抓到过）；
	//   - 数据陈旧（stale）兜底，防止横幅永久卡死。
	if !poolEmpty {
		recovered := m.successStreak >= nodeAlertClearStreak ||
			(attempts >= nodeAlertClearSamples && rate >= nodeAlertClearRate) ||
			stale ||
			(poolTotal == 0 && reason == "")
		if m.active && recovered {
			slog.Info("node_layer_alert_cleared",
				"window_attempts", attempts, "window_successes", successes,
				"success_streak", m.successStreak, "pool_available", poolAvailable, "pool_total", poolTotal)
			// ★ D2：恢复证据已经**推翻了**那段旧失败数据 ⇒ 连同「已结束段」一起丢弃。
			// 不丢的话，下一拍 judgedRun 又拿旧段当判据把告警拉回来：报→清→报 每分钟闪烁，
			// ERROR 刷屏（探针 14 分钟 10 次翻转；生产回放 32 次闪烁循环）。
			m.lastRunFailures, m.lastRunStart, m.lastRunEnd = 0, time.Time{}, time.Time{}
			m.active, m.reason, m.since = false, "", time.Time{}
			return false, ""
		}
	}

	if reason == "" {
		// 「没有判据成立」本身**不是**恢复证据（那正是 v1 的 F2：流量一停、窗口一空，
		// v1 就把告警清了，于是 8 小时事故只亮 5 分钟）。所以这里保持 Active，
		// 只把原因换成实时口径 —— 不假装还没发生的能力，也不假装已经恢复。
		if m.active {
			if attempts < nodeAlertClearSamples {
				m.reason = fmt.Sprintf("节点层此前判定故障，最近 %v 只有 %d 次尝试（样本不足以确认恢复：需 ≥%d 次且成功率 ≥%.0f%%）",
					nodeAlertWindow, attempts, nodeAlertClearSamples, nodeAlertClearRate*100)
			} else {
				m.reason = fmt.Sprintf("节点层此前判定故障，尚未恢复到健康线：最近 %v 内成功率 %.0f%%（%d/%d），健康线 %.0f%%",
					nodeAlertWindow, rate*100, successes, attempts, nodeAlertClearRate*100)
			}
		}
		return false, ""
	}

	// 已在告警中且未过冷却：不重复刷屏，但保持 Active 并刷新原因
	//（原因可能从"成功率过低"变成"池子空了"）。
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
	attempts, successes, _ := m.windowAt(now)
	runN, _, runSpan := m.judgedRun(now)
	snap := nodeAlertSnapshot{
		Active:          m.active,
		Reason:          m.reason,
		WindowMinutes:   nodeAlertWindow.Minutes(),
		WindowAttempts:  attempts,
		WindowSuccesses: successes,
		RunFailures:     runN,
		RunMinutes:      runSpan.Minutes(),
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
