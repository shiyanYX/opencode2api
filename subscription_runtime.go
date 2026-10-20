package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// refreshOnce 执行一轮订阅/webshare 刷新，并按三种语义分别打日志（task-60）：
//   - 有源到期但拉取失败（或成功却 0 节点）⇒ WARN，错误文本带源名；
//   - 本轮**没有任何源到期** ⇒ DEBUG 一行，不能打 WARN/INFO
//     （订阅默认 24h 到期、ticker 每 10 分钟跑一次 ⇒ 老代码每轮都伪造一条
//     「subscription refresh failed error="没有任何可用节点: "」的假告警，
//     144 条/天，误导排障：看起来像节点池空了，实际池子 215 节点正常）；
//   - 确实刷新并写入了节点池 ⇒ INFO。
func (m *subscriptionManager) refreshOnce(ctx context.Context, force bool, when string) (int, error) {
	out, err := m.refreshAllWithOutcome(ctx, force)
	if err != nil {
		slog.Warn("subscription refresh failed", "when", when, "error", err)
		return 0, err
	}
	if out.Due == 0 {
		slog.Debug("subscription refresh: 本轮无到期源",
			"when", when, "skipped", out.Skipped, "nodes", out.Nodes)
		return out.Nodes, nil
	}
	slog.Info("subscription refresh done",
		"when", when, "nodes", out.Nodes, "due", out.Due, "skipped", out.Skipped)
	return out.Nodes, nil
}

// startSubscriptionTicker 在后台周期刷新订阅并把节点合并进池。
// 首个周期立即执行一次（启动后非阻塞）；失败仅告警，不影响服务。
func startSubscriptionTicker() {
	var mu sync.Mutex
	refresh := func(force bool, when string) {
		mu.Lock()
		defer mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = subManager.refreshOnce(ctx, force, when)
	}
	go refresh(true, "startup")
	ticker := time.NewTicker(10 * time.Minute)
	go func() {
		for range ticker.C {
			refresh(false, "ticker")
		}
	}()
}
