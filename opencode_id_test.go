package main

import (
	"regexp"
	"strings"
	"testing"
)

// 上游 OpenCode Zen 免费层网关会校验 x-opencode-session 的形态：
// 形如 "<prefix>_<12 位小写十六进制时间戳><14 位 base62>"，总长 26（不含前缀）。
// 实测：不符合该形态的 session 一律返回 403 FreeTierError
// （"OpenCode's free tier can only be used from within OpenCode"）。
var opencodeIDRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

func TestNewOpenCodeIDMatchesUpstreamShape(t *testing.T) {
	for i := 0; i < 200; i++ {
		id := newOpenCodeID("ses")
		if !opencodeIDRe.MatchString(id) {
			t.Fatalf("session ID %q 不符合上游要求的形态 %s", id, opencodeIDRe.String())
		}
		if len(id) != 4+opencodeIDLen {
			t.Fatalf("session ID %q 长度为 %d，期望 %d", id, len(id), 4+opencodeIDLen)
		}
	}
}

func TestNewOpenCodeIDIsUnique(t *testing.T) {
	seen := make(map[string]bool, 500)
	for i := 0; i < 500; i++ {
		id := newOpenCodeID("ses")
		if seen[id] {
			t.Fatalf("session ID 重复: %q", id)
		}
		seen[id] = true
	}
}

// 记录回归：旧实现 "ses_"+randomString(24) 正是导致 403 FreeTierError 的原因。
func TestLegacySessionFormatIsRejectedShape(t *testing.T) {
	legacy := "ses_" + randomString(24)
	if opencodeIDRe.MatchString(legacy) {
		t.Fatalf("旧格式 %q 不应满足上游形态（否则该测试失去意义）", legacy)
	}
}

func TestNewOpenCodeIDHonoursPrefix(t *testing.T) {
	if got := newOpenCodeID("msg"); !strings.HasPrefix(got, "msg_") {
		t.Fatalf("前缀错误: %q", got)
	}
	if got := newOpenCodeID("req"); !strings.HasPrefix(got, "req_") {
		t.Fatalf("前缀错误: %q", got)
	}
}
