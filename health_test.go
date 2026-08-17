package main

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubRoundTripper 供健康探测测试注入确定性响应。
type stubRoundTripper struct {
	fn func(req *http.Request) (*http.Response, error)
}

func (s stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return s.fn(req)
}

func probeTestPool(t *testing.T, rt http.RoundTripper) (*nodePool, *ProxyNode) {
	t.Helper()
	prev := testNodeClient
	testNodeClient = func(n *ProxyNode) *http.Client {
		return &http.Client{Transport: rt, Timeout: 5 * time.Second}
	}
	t.Cleanup(func() { testNodeClient = prev })

	p := newProxyPool(filepath.Join(t.TempDir(), "state.json"))
	t.Cleanup(p.waitStateSaves)
	p.probeInterval = 5 * time.Minute
	p.probeURL = "http://probe.local/204"
	n := &ProxyNode{Name: "n1", Protocol: "socks5", Address: "127.0.0.1", Port: 1080}
	p.setNodes([]*ProxyNode{n})
	return p, p.nodes[0]
}

func okRT() http.RoundTripper {
	return stubRoundTripper{fn: func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 204, Status: "204 No Content", Body: http.NoBody, Request: req}, nil
	}}
}

func failRT() http.RoundTripper {
	return stubRoundTripper{fn: func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("probe dial failed")
	}}
}

func TestCheckNodesProbeSuccessRecordsLatency(t *testing.T) {
	p, n := probeTestPool(t, okRT())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if checked := p.checkNodes(ctx); checked != 1 {
		t.Fatalf("checked = %d, want 1", checked)
	}
	if n.State != NodeAvailable {
		t.Fatalf("state = %s, want available", n.State)
	}
	if n.LatencyMs < 0 || n.LastProbeAt.IsZero() {
		t.Fatalf("latency/probe time not recorded: %d %v", n.LatencyMs, n.LastProbeAt)
	}
}

func TestCheckNodesProbeFailureMarksDead(t *testing.T) {
	p, n := probeTestPool(t, failRT())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p.checkNodes(ctx)
	if n.State != NodeDead {
		t.Fatalf("state = %s, want dead", n.State)
	}
	if !strings.Contains(n.LastError, "probe") {
		t.Fatalf("last error = %q, want probe reason", n.LastError)
	}
	// 冷却 = 探测周期，dead 节点在下次探测前不可选。
	if p.eligible(n, time.Now()) {
		t.Fatalf("dead node should be ineligible until next probe")
	}
	if n.CooldownUntil.Before(time.Now().Add(4 * time.Minute)) {
		t.Fatalf("cooldown = %v, want ~probe interval", n.CooldownUntil)
	}
}

func TestProbeSuccessLiftsDeadButKeepsExhausted(t *testing.T) {
	p, n := probeTestPool(t, okRT())
	p.mark(n.Fingerprint, NodeExhausted, "quota:test")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p.checkNodes(ctx)
	if n.State != NodeExhausted {
		t.Fatalf("probe success must not lift exhausted, got %s", n.State)
	}
	if n.LatencyMs < 0 {
		t.Fatalf("latency should still be recorded for exhausted node")
	}

	p2, n2 := probeTestPool(t, failRT())
	p2.markProbeDead(n2.Fingerprint, "probe: down")
	if n2.State != NodeDead {
		t.Fatalf("markProbeDead failed to set dead")
	}
	// 换用成功探测 → 恢复 available。
	prev := testNodeClient
	testNodeClient = func(n *ProxyNode) *http.Client {
		return &http.Client{Transport: okRT(), Timeout: 5 * time.Second}
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	p2.checkNodes(ctx2)
	testNodeClient = prev
	if n2.State != NodeAvailable {
		t.Fatalf("probe success should lift dead, got %s", n2.State)
	}
	if !n2.CooldownUntil.IsZero() || n2.LastError != "" {
		t.Fatalf("dead markers not cleared: %v %q", n2.CooldownUntil, n2.LastError)
	}
}

func TestProbeFailureKeepsExhaustedMarker(t *testing.T) {
	p, n := probeTestPool(t, failRT())
	p.mark(n.Fingerprint, NodeExhausted, "quota:test")
	cooldown := n.CooldownUntil
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p.checkNodes(ctx)
	if n.State != NodeExhausted {
		t.Fatalf("probe failure must not overwrite exhausted, got %s", n.State)
	}
	if !n.CooldownUntil.Equal(cooldown) {
		t.Fatalf("exhausted cooldown overwritten: was %v, now %v", cooldown, n.CooldownUntil)
	}
	if !strings.Contains(n.LastError, "quota") {
		t.Fatalf("last error overwritten by probe: %q", n.LastError)
	}
}

func TestMergeConfigPatchHealthFields(t *testing.T) {
	iv := 30
	url := "https://example.com/generate_204"
	out := mergeConfigPatch(AppConfig{}, configPatch{
		NodeHealthIntervalMinutes: &iv,
		NodeHealthProbeURL:        &url,
	})
	if out.NodeHealthIntervalMinutes != 30 || out.NodeHealthProbeURL != url {
		t.Fatalf("health fields not merged: %d %q", out.NodeHealthIntervalMinutes, out.NodeHealthProbeURL)
	}
	// 显式空串 = 清零（回默认）。
	empty := ""
	out2 := mergeConfigPatch(AppConfig{NodeHealthProbeURL: "x"}, configPatch{NodeHealthProbeURL: &empty})
	if out2.NodeHealthProbeURL != "" {
		t.Fatalf("empty probe url should clear, got %q", out2.NodeHealthProbeURL)
	}
}

func TestHealthProbeDefaults(t *testing.T) {
	p := newProxyPool(filepath.Join(t.TempDir(), "state.json"))
	if got := p.effectiveProbeURL(); got != defaultProbeURL {
		t.Fatalf("probe url = %q, want default", got)
	}
	if got := p.healthInterval(); got != defaultHealthInterval {
		t.Fatalf("interval = %v, want default", got)
	}
}

// ======================= 状态恢复语义（exhausted 定时 / dead 事件） =======================

// TestSweepFlipsExpiredExhaustedToAvailable：配额冷却到期后，
// 路由入口（pick 前的清扫）应立即把 exhausted 翻回 available 并清空全部标记。
func TestSweepFlipsExpiredExhaustedToAvailable(t *testing.T) {
	p := newProxyPool(filepath.Join(t.TempDir(), "state.json"))
	t.Cleanup(p.waitStateSaves)
	n := &ProxyNode{Name: "n1", Protocol: "socks5", Address: "1.1.1.1", Port: 1080}
	p.setNodes([]*ProxyNode{n})
	p.mark(n.Fingerprint, NodeExhausted, "quota:FreeUsageLimitError")
	if n.State != NodeExhausted {
		t.Fatalf("mark failed, state = %s", n.State)
	}
	// 冷却拨到过去：模拟配额冷却到期。
	p.mu.Lock()
	n.CooldownUntil = time.Now().Add(-time.Minute)
	p.mu.Unlock()

	got := p.pick(false)
	if got == nil || got.Fingerprint != n.Fingerprint {
		t.Fatalf("pick after cooldown expiry = %v, want n1", got)
	}
	if n.State != NodeAvailable {
		t.Fatalf("state = %s, want available after sweep", n.State)
	}
	if !n.CooldownUntil.IsZero() || !n.MarkedAt.IsZero() || n.LastError != "" {
		t.Fatalf("markers not cleared: until=%v marked=%v err=%q",
			n.CooldownUntil, n.MarkedAt, n.LastError)
	}
}

// TestSnapshotSweepsExpiredExhausted：面板快照同样触发清扫，
// 冷却到期的 exhausted 在面板展示层即为 available（与路由一致）。
func TestSnapshotSweepsExpiredExhausted(t *testing.T) {
	p := newProxyPool("")
	n := &ProxyNode{Name: "n1", Protocol: "socks5", Address: "1.1.1.1", Port: 1080}
	p.setNodes([]*ProxyNode{n})
	p.mark(n.Fingerprint, NodeExhausted, "quota:x")
	p.mu.Lock()
	n.CooldownUntil = time.Now().Add(-time.Minute)
	p.mu.Unlock()

	states := p.snapshot()
	if len(states) != 1 || states[0].State != NodeAvailable {
		t.Fatalf("snapshot after expiry = %d nodes, state %v; want available",
			len(states), states[0].State)
	}
}

// TestSweepKeepsWithinCooldownExhausted：冷却未到期时清扫不得翻回。
func TestSweepKeepsWithinCooldownExhausted(t *testing.T) {
	p := newProxyPool("")
	n := &ProxyNode{Name: "n1", Protocol: "socks5", Address: "1.1.1.1", Port: 1080}
	p.setNodes([]*ProxyNode{n})
	p.mark(n.Fingerprint, NodeExhausted, "quota:x")

	if got := p.pick(false); got != nil {
		t.Fatalf("picked within cooldown: %s", got.Name)
	}
	if n.State != NodeExhausted {
		t.Fatalf("state changed within cooldown: %s", n.State)
	}
}

// TestManualSweepsExpiredExhausted：手动选路入口同样清扫，
// 冷却到期的 exhausted 节点可被手动指向并即时恢复可用。
func TestManualSweepsExpiredExhausted(t *testing.T) {
	p := newProxyPool("")
	n1 := &ProxyNode{Name: "n1", Protocol: "socks5", Address: "1.1.1.1", Port: 1080}
	n2 := &ProxyNode{Name: "n2", Protocol: "socks5", Address: "2.2.2.2", Port: 1080}
	p.setNodes([]*ProxyNode{n1, n2})
	p.mark(n1.Fingerprint, NodeExhausted, "quota:x")
	p.mu.Lock()
	n1.CooldownUntil = time.Now().Add(-time.Minute)
	p.mu.Unlock()

	fp := p.manual(n1.Fingerprint)
	if fp != n1.Fingerprint {
		t.Fatalf("manual = %q, want n1", fp)
	}
	if n1.State != NodeAvailable {
		t.Fatalf("state = %s, want available", n1.State)
	}
}

// TestDeadNeverEligibleByCooldown：dead 是事件恢复（仅探测成功解除），
// 冷却时间过去也不能让 dead 节点重新参与路由。
func TestDeadNeverEligibleByCooldown(t *testing.T) {
	p, n := probeTestPool(t, failRT())
	p.markProbeDead(n.Fingerprint, "probe: down")
	p.mu.Lock()
	n.CooldownUntil = time.Now().Add(-time.Minute)
	p.mu.Unlock()

	if p.eligible(n, time.Now()) {
		t.Fatalf("dead node must never be eligible by cooldown")
	}
	if got := p.pick(false); got != nil {
		t.Fatalf("pick returned dead node: %s", got.Name)
	}
}

// TestScheduledCheckSkipsExhausted：计划巡检只探测 available，
// exhausted 由配额冷却恢复，不被探测打扰。
func TestScheduledCheckSkipsExhausted(t *testing.T) {
	var mu sync.Mutex
	probes := 0
	rt := stubRoundTripper{fn: func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		probes++
		mu.Unlock()
		return &http.Response{StatusCode: 204, Status: "204 No Content", Body: http.NoBody, Request: req}, nil
	}}
	prev := testNodeClient
	testNodeClient = func(n *ProxyNode) *http.Client {
		return &http.Client{Transport: rt, Timeout: 5 * time.Second}
	}
	t.Cleanup(func() { testNodeClient = prev })

	p := newProxyPool(filepath.Join(t.TempDir(), "state.json"))
	t.Cleanup(p.waitStateSaves)
	p.probeInterval = 5 * time.Minute
	p.probeURL = "http://probe.local/204"
	n1 := &ProxyNode{Name: "ok", Protocol: "socks5", Address: "1.1.1.1", Port: 1080}
	n2 := &ProxyNode{Name: "ex", Protocol: "socks5", Address: "2.2.2.2", Port: 1080}
	p.setNodes([]*ProxyNode{n1, n2})
	p.mark(n2.Fingerprint, NodeExhausted, "quota:x")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	checked := p.checkAvailable(ctx)
	if checked != 1 {
		t.Fatalf("checkAvailable = %d, want 1", checked)
	}
	mu.Lock()
	got := probes
	mu.Unlock()
	if got != 1 {
		t.Fatalf("probe calls = %d, want 1 (exhausted skipped)", got)
	}
	if n2.State != NodeExhausted {
		t.Fatalf("exhausted state changed by scheduled check: %s", n2.State)
	}
}

// TestCheckDeadRecoversOnSuccess：每分钟复探 dead 节点，成功即恢复。
func TestCheckDeadRecoversOnSuccess(t *testing.T) {
	p, n := probeTestPool(t, failRT())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p.checkNodes(ctx)
	if n.State != NodeDead {
		t.Fatalf("initial state = %s, want dead", n.State)
	}
	// 换成功探测后复探。
	prev := testNodeClient
	testNodeClient = func(n *ProxyNode) *http.Client {
		return &http.Client{Transport: okRT(), Timeout: 5 * time.Second}
	}
	defer func() { testNodeClient = prev }()
	checked := p.checkDead(ctx)
	if checked != 1 {
		t.Fatalf("checkDead = %d, want 1", checked)
	}
	if n.State != NodeAvailable {
		t.Fatalf("dead node not recovered by probe: %s", n.State)
	}
}

// TestMarkProbeDeadIdempotentOnDead：已 dead 节点复探失败时不重复标记、
// 不延长冷却、不覆盖原因（防刷屏）。
func TestMarkProbeDeadIdempotentOnDead(t *testing.T) {
	p, n := probeTestPool(t, failRT())
	p.markProbeDead(n.Fingerprint, "probe: down")
	cooldown := n.CooldownUntil
	marked := n.MarkedAt

	p.markProbeDead(n.Fingerprint, "probe: down again")
	if n.State != NodeDead {
		t.Fatalf("state = %s, want dead", n.State)
	}
	if !n.CooldownUntil.Equal(cooldown) {
		t.Fatalf("cooldown changed by re-probe failure: %v → %v", cooldown, n.CooldownUntil)
	}
	if !n.MarkedAt.Equal(marked) {
		t.Fatalf("marked time changed by re-probe failure")
	}
	if n.LastError != "probe: down" {
		t.Fatalf("last error overwritten: %q", n.LastError)
	}
}
