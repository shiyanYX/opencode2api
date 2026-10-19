package main

import (
	"context"
	"log/slog"
	"time"
)

// poolStateCounts 返回池中 available 节点数与总数。
// 口径与面板「healthy」一致（State == available），这样告警和面板显示的数字
// 永远是同一个数，不会出现「面板说 115 个可用、告警说 0 个」的打架。
func poolStateCounts() (avail, total int) {
	for _, n := range proxyPool.snapshot() {
		if n.State == NodeAvailable {
			avail++
		}
		total++
	}
	return avail, total
}

// startNodeHealthCheck 后台健康循环：
//   - 每分钟复探 dead 节点（事件恢复：探测成功即解除，失败仅记录，防刷屏）；
//   - 每探测周期（默认 15 分钟）对 available 节点全量巡检，失败标 dead；
//   - exhausted 节点不参与探测（配额冷却到期由节点池清扫定时恢复）。
//
// 池为空时跳过。
// probeInterval 设为 0 时禁用全部健康检查（包括 dead 节点复探）。
func startNodeHealthCheck() {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		runNodeHealthLoop(ticker.C)
	}()
}

// runNodeHealthLoop 是循环本体。tick 从外面传进来是为了可测：
// 单测能注入假 tick，直接断言「跑一拍会不会评估告警」，
// 否则这段接线只能靠读代码相信 —— 而接错线在这里的代价是线上永远不告警。
func runNodeHealthLoop(ticks <-chan time.Time) {
	var next time.Time
	for range ticks {
		runNodeHealthTick(&next)
	}
}

func runNodeHealthTick(next *time.Time) {
	interval := proxyPool.healthInterval()
	if interval <= 0 {
		// probeInterval 负数表示禁用健康检查：不探测 ≠ 不告警。
		// 出口整体不可用这件事照样要报，否则运维一拨开关就把告警一起拨瞎了。
		slog.Debug("node health check disabled")
		evaluateNodeAlert()
		return
	}
	now := time.Now()

	// 每分钟：dead 节点复探
	if proxyPool.hasDeadNodes() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		checked := proxyPool.checkDead(ctx)
		cancel()
		slog.Debug("node dead re-probe done", "checked", checked)
	}

	// 周期到点：available 节点全量巡检
	if next.IsZero() || !now.Before(*next) {
		*next = now.Add(interval)
		if proxyPool.nodeCount() > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			checked := proxyPool.checkAvailable(ctx)
			cancel()
			slog.Info("node health check done", "checked", checked, "interval", interval.String())
		}
	}

	evaluateNodeAlert()
}

// evaluateNodeAlert 判一次节点层告警（C-5）。
// 每分钟跑一次，不能只在巡检周期跑：判据窗口是 10 分钟，15 分钟一轮就漏判了。
// 这里读到的是最新池状态 + 上游重试循环实时喂进来的尝试样本。
func evaluateNodeAlert() {
	avail, total := poolStateCounts()
	if fire, reason := nodeAlerts.evaluate(time.Now(), avail, total); fire {
		slog.Error("node_layer_alert",
			"reason", reason,
			"available", avail,
			"total", total,
			"hint", "先查订阅抓取是否走了代理（会让整池节点停在旧缓存上），再看出口 IP 是否被上游封禁",
		)
	}
}
