package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// startSubscriptionTicker 在后台周期刷新订阅并把节点合并进池。
// 首个周期立即执行一次（启动后非阻塞）；失败仅告警，不影响服务。
func startSubscriptionTicker() {
	var mu sync.Mutex
	refresh := func(force bool, when string) {
		mu.Lock()
		defer mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		n, err := subManager.refreshAll(ctx, force)
		if err != nil {
			slog.Warn("subscription refresh failed", "when", when, "error", err)
			return
		}
		slog.Info("subscription refresh done", "when", when, "nodes", n)
	}
	go refresh(true, "startup")
	ticker := time.NewTicker(10 * time.Minute)
	go func() {
		for range ticker.C {
			refresh(false, "ticker")
		}
	}()
}
