package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAdminConfigQuotaDefaultsEcho verifies GET /api/config returns the
// effective default quota signals when the config file omits them, so the
// panel text boxes show real values instead of being empty.
func TestAdminConfigQuotaDefaultsEcho(t *testing.T) {
	oldPath := configPath
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.json")
	t.Cleanup(func() { configPath = oldPath })
	if err := os.WriteFile(configPath, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	adminConfigHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Quota struct {
			ErrorTypes      []string `json:"error_types"`
			MessageKeywords []string `json:"message_keywords"`
		} `json:"quota_error_signals"`
		MaxSwitches    int `json:"max_quota_node_switches"`
		CooldownExh    int `json:"node_cooldown_exhausted_hours"`
		CooldownDead   int `json:"node_cooldown_dead_minutes"`
		HealthInterval int `json:"node_health_interval_minutes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Quota.ErrorTypes) == 0 ||
		!strings.Contains(strings.Join(resp.Quota.ErrorTypes, ","), "FreeUsageLimitError") {
		t.Fatalf("error_types should echo defaults, got %#v", resp.Quota.ErrorTypes)
	}
	if len(resp.Quota.MessageKeywords) == 0 {
		t.Fatalf("message_keywords should echo defaults, got %#v", resp.Quota.MessageKeywords)
	}
	if resp.MaxSwitches != defaultMaxQuotaNodeSwitches {
		t.Fatalf("max_quota_node_switches = %d, want %d", resp.MaxSwitches, defaultMaxQuotaNodeSwitches)
	}
	if resp.CooldownExh != 1 || resp.CooldownDead != 1 {
		t.Fatalf("cooldowns = %dh/%dm, want 1/1", resp.CooldownExh, resp.CooldownDead)
	}
}

// TestAdminConfigQuotaEmptyArrayKeepsDefaults verifies that saving an empty
// quota signal list via the panel does NOT silently disable quota detection:
// applyConfig treats empty arrays as "use defaults".
func TestAdminConfigQuotaEmptyArrayKeepsDefaults(t *testing.T) {
	oldPath := configPath
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.json")
	t.Cleanup(func() { configPath = oldPath })
	if err := os.WriteFile(configPath, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// 面板「保存配额设置」提交空数组（文本框被清空时）
	patch := `{"quota_error_signals":{"error_types":[],"message_keywords":[]},"max_quota_node_switches":0}`
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(patch))
	rec := httptest.NewRecorder()
	adminConfigHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	// 默认签名仍生效：error.type 命中
	if ok, _ := classifyQuota(http.StatusForbidden, []byte(`{"error":{"type":"FreeUsageLimitError"}}`)); !ok {
		t.Fatal("type signature lost after empty-array save, want still exhausted")
	}
	// 默认关键词仍生效：error.message 命中
	if ok, _ := classifyQuota(http.StatusForbidden, []byte(`{"error":{"message":"your free usage limit has been reached"}}`)); !ok {
		t.Fatal("keyword signature lost after empty-array save, want still exhausted")
	}
	// 非命中保持非耗尽
	if ok, _ := classifyQuota(http.StatusForbidden, []byte(`{"error":{"message":"model not found"}}`)); ok {
		t.Fatal("unrelated error classified as quota signal")
	}
}
