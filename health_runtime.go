package main

import (
	"context"
	"log/slog"
	"time"
)

// startNodeHealthCheck 后台健康循环：
//   - 每分钟复探 dead 节点（事件恢复：探测成功即解除，失败仅记录，防刷屏）；
//   - 每探测周期（默认 15 分钟）对 available 节点全量巡检，失败标 dead；
//   - exhausted 节点不参与探测（配额冷却到期由节点池清扫定时恢复）。
//
// 池为空时跳过。
func startNodeHealthCheck() {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		var next time.Time
		for range ticker.C {
			interval := proxyPool.healthInterval()
			if interval <= 0 {
				interval = defaultHealthInterval
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
			if !next.IsZero() && now.Before(next) {
				continue
			}
			next = now.Add(interval)
			if proxyPool.nodeCount() == 0 {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			checked := proxyPool.checkAvailable(ctx)
			cancel()
			slog.Info("node health check done", "checked", checked, "interval", interval.String())
		}
	}()
}