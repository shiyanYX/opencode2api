package main

import (
	"path/filepath"
	"testing"
)

// resetTokenStatsForTest 隔离全局统计，并把落盘路径指向临时目录，
// 避免测试覆盖仓库里的 stats.json。
func resetTokenStatsForTest(t *testing.T) {
	t.Helper()
	tokenStatsMu.Lock()
	prevStats, prevPath := tokenStats, tokenStatsPath
	tokenStats = &TokenStatsData{Models: map[string]*ModelStats{}}
	tokenStatsPath = filepath.Join(t.TempDir(), "stats.json")
	tokenStatsMu.Unlock()
	t.Cleanup(func() {
		tokenStatsMu.Lock()
		tokenStats, tokenStatsPath = prevStats, prevPath
		tokenStatsMu.Unlock()
	})
}

// DeepSeek/OpenAI 风格用 prompt_cache_hit_tokens 报告「缓存读取」，且
// prompt_tokens 已包含命中部分。历史 bug：调用方把 parseCacheUsage 的返回序
// (read, created) 当成 (created, read) 使用，于是 cache_read 被写进
// cache_created，面板的缓存命中率恒为 0%。
func TestRecordUsageStatsKeepsCacheReadSeparateFromCreated(t *testing.T) {
	resetTokenStatsForTest(t)

	recordUsageStats("m", map[string]any{
		"prompt_tokens":           float64(1000),
		"completion_tokens":       float64(50),
		"total_tokens":            float64(1050),
		"prompt_cache_hit_tokens": float64(900),
	})

	tokenStatsMu.Lock()
	ms := tokenStats.Models["m"]
	tokenStatsMu.Unlock()
	if ms == nil {
		t.Fatal("模型统计未写入")
	}
	if ms.CacheReadTokens != 900 {
		t.Errorf("CacheReadTokens = %d, want 900（缓存读取写错了字段）", ms.CacheReadTokens)
	}
	if ms.CacheCreatedTokens != 0 {
		t.Errorf("CacheCreatedTokens = %d, want 0（读取值污染了写入字段）", ms.CacheCreatedTokens)
	}
	if ms.PromptTokens != 1000 || ms.CompletionTokens != 50 || ms.TotalTokens != 1050 {
		t.Errorf("token 统计 = (%d,%d,%d), want (1000,50,1050)",
			ms.PromptTokens, ms.CompletionTokens, ms.TotalTokens)
	}
	// 面板命中率即 cr/pt，必须为 90%。
	if got := ms.CacheReadTokens * 100 / ms.PromptTokens; got != 90 {
		t.Errorf("缓存命中率 = %d%%, want 90%%", got)
	}
}

// Anthropic 风格：cache_creation_input_tokens 是「写入」，
// cache_read_input_tokens 是「读取」，两者不得互换。
func TestRecordUsageStatsHandlesAnthropicCacheFields(t *testing.T) {
	resetTokenStatsForTest(t)

	recordUsageStats("m", map[string]any{
		"input_tokens":                float64(100),
		"output_tokens":               float64(10),
		"total_tokens":                float64(110),
		"cache_creation_input_tokens": float64(700),
		"cache_read_input_tokens":     float64(200),
	})

	tokenStatsMu.Lock()
	ms := tokenStats.Models["m"]
	tokenStatsMu.Unlock()
	if ms == nil {
		t.Fatal("模型统计未写入")
	}
	if ms.CacheReadTokens != 200 {
		t.Errorf("CacheReadTokens = %d, want 200", ms.CacheReadTokens)
	}
	if ms.CacheCreatedTokens != 700 {
		t.Errorf("CacheCreatedTokens = %d, want 700", ms.CacheCreatedTokens)
	}
}
