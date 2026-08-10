package main

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
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
