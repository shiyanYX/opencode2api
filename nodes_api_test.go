package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminNodesHandler(t *testing.T) {
	oldPool := proxyPool
	t.Cleanup(func() { proxyPool = oldPool })

	n1 := &ProxyNode{Name: "n1", Protocol: "socks5", Address: "1.2.3.4", Port: 1080}
	n2 := &ProxyNode{Name: "n2", Protocol: "ss", Address: "5.6.7.8", Port: 8388, Method: "chacha20-ietf-poly1305", Password: "pw"}
	proxyPool = newProxyPool("")
	proxyPool.setNodes([]*ProxyNode{n1, n2})
	proxyPool.mark(computeFingerprint(n2), NodeExhausted, "quota:test")

	req := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	rec := httptest.NewRecorder()
	adminNodesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	nodes, _ := body["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("nodes len = %d, want 2", len(nodes))
	}
	byName := map[string]map[string]any{}
	for _, n := range nodes {
		byName[n.(map[string]any)["name"].(string)] = n.(map[string]any)
	}
	if byName["n1"]["state"] != "available" {
		t.Fatalf("n1 state = %v, want available", byName["n1"]["state"])
	}
	if byName["n2"]["state"] != "exhausted" {
		t.Fatalf("n2 state = %v, want exhausted", byName["n2"]["state"])
	}

	// POST switch 给不可用节点 → 不应生效（保持空激活）
	req2 := httptest.NewRequest(http.MethodPost, "/api/nodes",
		strings.NewReader(`{"action":"switch","fingerprint":"`+computeFingerprint(n2)+`"}`))
	rec2 := httptest.NewRecorder()
	adminNodesHandler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", rec2.Code)
	}
	// 解冻后手动指定生效
	proxyPool.unmark(computeFingerprint(n2))
	req2b := httptest.NewRequest(http.MethodPost, "/api/nodes",
		strings.NewReader(`{"action":"switch","fingerprint":"`+computeFingerprint(n2)+`"}`))
	rec2b := httptest.NewRecorder()
	adminNodesHandler(rec2b, req2b)
	if rec2b.Code != http.StatusOK {
		t.Fatalf("POST2 status = %d, want 200", rec2b.Code)
	}
	active, manual := proxyPool.pickState()
	if manual != computeFingerprint(n2) || active != computeFingerprint(n2) {
		t.Fatalf("after manual: active=%s manual=%s, want n2", active, manual)
	}

	// POST reset → 状态回退
	proxyPool.mark(computeFingerprint(n2), NodeExhausted, "quota:test")
	req3 := httptest.NewRequest(http.MethodPost, "/api/nodes",
		strings.NewReader(`{"action":"reset","fingerprint":"`+computeFingerprint(n2)+`"}`))
	rec3 := httptest.NewRecorder()
	adminNodesHandler(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200", rec3.Code)
	}
	if got := proxyPool.byID[computeFingerprint(n2)].State; got != NodeAvailable {
		t.Fatalf("after reset state = %s, want available", got.String())
	}

	// 未知 action → 400
	req4 := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(`{"action":"nope"}`))
	rec4 := httptest.NewRecorder()
	adminNodesHandler(rec4, req4)
	if rec4.Code != http.StatusBadRequest {
		t.Fatalf("unknown action status = %d, want 400", rec4.Code)
	}
}
