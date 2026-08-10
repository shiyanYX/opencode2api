package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAdminConfigMerge verifies POST /api/config merges instead of overwriting:
// fields absent from the patch keep their on-disk values.
func TestAdminConfigMerge(t *testing.T) {
	oldPath := configPath
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.json")
	t.Cleanup(func() { configPath = oldPath })

	base := `{
  "model_alias": {"gpt-4o": "deepseek-v3.2"},
  "subscriptions": [{"name": "s1", "url": "https://example.com/sub", "update_interval_hours": 6}],
  "manual_nodes": [{"name": "m1", "protocol": "socks5", "address": "1.2.3.4", "port": 1080}],
  "max_quota_node_switches": 5,
  "node_cooldown_exhausted_hours": 48
}`
	if err := os.WriteFile(configPath, []byte(base), 0644); err != nil {
		t.Fatal(err)
	}

	// 面板只改别名 + 配额预算，缺其它字段
	patch := `{"model_alias":{"g":"nemotron-3-ultra"},"max_quota_node_switches":8}`
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(patch))
	rec := httptest.NewRecorder()
	adminConfigHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	cfg := loadConfig(configPath)
	if cfg.ModelAlias["g"] != "nemotron-3-ultra" {
		t.Fatalf("alias = %v, want patched value", cfg.ModelAlias)
	}
	if len(cfg.Subscriptions) != 1 || cfg.Subscriptions[0].Name != "s1" {
		t.Fatalf("subscriptions lost: %#v", cfg.Subscriptions)
	}
	if len(cfg.ManualNodes) != 1 || cfg.ManualNodes[0].Name != "m1" {
		t.Fatalf("manual_nodes lost: %#v", cfg.ManualNodes)
	}
	if cfg.MaxQuotaNodeSwitches != 8 {
		t.Fatalf("max switches = %d, want 8", cfg.MaxQuotaNodeSwitches)
	}
	if cfg.NodeCooldownExhaustedHours != 48 {
		t.Fatalf("cooldown = %d, want 48", cfg.NodeCooldownExhaustedHours)
	}
}

// TestAdminNodesSaveSubscription verifies the panel "save" action persists
// subscriptions + manual nodes to disk and they survive reload.
func TestAdminNodesSaveSubscription(t *testing.T) {
	oldPath := configPath
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.json")
	t.Cleanup(func() { configPath = oldPath })
	if err := os.WriteFile(configPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	oldPool := proxyPool
	proxyPool = newProxyPool(filepath.Join(dir, "proxy_state.json"))
	t.Cleanup(func() { proxyPool = oldPool })

	body := `{"action":"save","subscriptions":[{"name":"s1","url":"data:text/plain;base64,c29ja3M1OgogIC0gbmFtZTogYQogICAgc2VydmVyOiAxLjEuMS4xCiAgICBwb3J0OiA5MDAw"}],"manual_nodes":[{"name":"direct-socks","protocol":"socks5","address":"2.2.2.2","port":1080}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(body))
	rec := httptest.NewRecorder()
	adminNodesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	cfg := loadConfig(configPath)
	if len(cfg.Subscriptions) != 1 || cfg.Subscriptions[0].Name != "s1" {
		t.Fatalf("subscriptions not persisted: %#v", cfg.Subscriptions)
	}
	if len(cfg.ManualNodes) != 1 || cfg.ManualNodes[0].Address != "2.2.2.2" {
		t.Fatalf("manual_nodes not persisted: %#v", cfg.ManualNodes)
	}
	if proxyPool.nodeCount() < 1 {
		t.Fatalf("pool empty after save+refresh, want >=1 node")
	}
}

// TestAdminConfigMergeClear verifies explicit zero values (false / empty string)
// are saved by the panel patch (pointer semantics), unlike missing fields.
func TestAdminConfigMergeClear(t *testing.T) {
	oldPath := configPath
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.json")
	t.Cleanup(func() { configPath = oldPath })

	base := `{
  "force_disable_thinking": true,
  "active_socks5": "127.0.0.1:1080",
  "socks5_paid_direct": true,
  "max_quota_node_switches": 9
}`
	if err := os.WriteFile(configPath, []byte(base), 0644); err != nil {
		t.Fatal(err)
	}

	patch := `{"force_disable_thinking":false,"active_socks5":"","socks5_paid_direct":false,"max_quota_node_switches":5}`
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(patch))
	rec := httptest.NewRecorder()
	adminConfigHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	cfg := loadConfig(configPath)
	if cfg.ForceDisableThinking {
		t.Fatalf("force_disable_thinking still true, want cleared")
	}
	if cfg.ActiveSocks5 != "" {
		t.Fatalf("active_socks5 = %q, want cleared (direct)", cfg.ActiveSocks5)
	}
	if cfg.Socks5PaidDirect {
		t.Fatalf("socks5_paid_direct still true, want cleared")
	}
	if cfg.MaxQuotaNodeSwitches != 5 {
		t.Fatalf("max switches = %d, want 5", cfg.MaxQuotaNodeSwitches)
	}
}
