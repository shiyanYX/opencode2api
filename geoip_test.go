package main

import (
	"testing"
)

// ======================== countryToRegion 测试 ========================

func TestCountryToRegion(t *testing.T) {
	tests := []struct {
		country  string
		expected string
	}{
		{"US", "us"},
		{"JP", "jp"},
		{"SG", "sg"},
		{"HK", "hk"},
		{"TW", "tw"},
		{"KR", "kr"},
		{"AU", "au"},
		{"CA", "ca"},
		{"GB", "eu"},
		{"DE", "eu"},
		{"FR", "eu"},
		{"RU", "ru"},
	}

	for _, tt := range tests {
		t.Run(tt.country, func(t *testing.T) {
			got, ok := countryToRegion[tt.country]
			if !ok {
				t.Errorf("countryToRegion[%s] not found", tt.country)
				return
			}
			if got != tt.expected {
				t.Errorf("countryToRegion[%s] = %q, want %q", tt.country, got, tt.expected)
			}
		})
	}
}

// ======================== inferRegionFromAddress 测试 ========================

func TestInferRegionFromAddress(t *testing.T) {
	// 测试私有 IP（应该返回空）
	tests := []struct {
		name     string
		address  string
		expected string
	}{
		{"private ip", "192.168.1.1", ""},
		{"loopback", "127.0.0.1", ""},
		{"invalid", "not-an-ip", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferRegionFromAddress(tt.address)
			if got != tt.expected {
				t.Errorf("inferRegionFromAddress(%q) = %q, want %q", tt.address, got, tt.expected)
			}
		})
	}
}

// ======================== GeoIP 缓存测试 ========================

func TestGeoIPCache(t *testing.T) {
	// 清空缓存
	geoIPCacheMu.Lock()
	geoIPCache = map[string]string{}
	geoIPCacheMu.Unlock()

	// 设置缓存
	geoIPCacheMu.Lock()
	geoIPCache["8.8.8.8"] = "us"
	geoIPCacheMu.Unlock()

	// 验证缓存
	geoIPCacheMu.RLock()
	region := geoIPCache["8.8.8.8"]
	geoIPCacheMu.RUnlock()

	if region != "us" {
		t.Errorf("geoIPCache[8.8.8.8] = %q, want us", region)
	}
}

// ======================== regionPatterns 完整性测试 ========================

func TestRegionPatternsCompleteness(t *testing.T) {
	// 确保所有预期的区域都在 regionPatterns 中
	expectedRegions := []string{"us", "eu", "jp", "sg", "hk", "tw", "kr", "au", "ca"}

	for _, region := range expectedRegions {
		found := false
		for _, v := range regionPatterns {
			if v == region {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("region %q not found in regionPatterns", region)
		}
	}
}

// ======================== extractRegionFromMessage 完整性测试 ========================

func TestExtractRegionFromMessageEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"uppercase", "ONLY AVAILABLE IN US", "us"},
		{"mixed case", "Available In Jp Only", "jp"},
		{"with punctuation", "This model is only available in us.", "us"},
		{"multiple matches", "available in us and eu", "us"}, // 应该返回第一个匹配
		{"no space after", "available inus", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRegionFromMessage(tt.input)
			if got != tt.expected {
				t.Errorf("extractRegionFromMessage(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}
