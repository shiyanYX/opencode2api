package main

import (
	"context"
	"log/slog"
	"time"
)

// startNodeHealthCheck 后台周期健康检查（Clash url-test 语义）：
// 每 tick 检查是否到点，按当前探测间隔执行；失败节点标记 dead（冷却=探测间隔），
// 恢复完全由后续探测成功驱动；池为空时跳过。
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
			if !next.IsZero() && now.Before(next) {
				continue
			}
			next = now.Add(interval)
			if proxyPool.nodeCount() == 0 {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			checked := proxyPool.checkNodes(ctx)
			cancel()
			slog.Info("node health check done", "checked", checked, "interval", interval.String())
		}
	}()
}