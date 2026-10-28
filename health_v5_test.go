package main

import (
	"context"
	"testing"
	"time"
)

func TestNodeHealthTickForceSkipsLegacyProbes(t *testing.T) {
	setupProbeHTTP(t, []*ProxyNode{{Name: "sub::n", Protocol: "socks5", Address: "10.0.0.1", Port: 1080}})
	t.Setenv("NODE_SYSTEM", "force")
	var dead, avail, alerts int
	oldDead, oldAvail, oldAlert := nodeHealthCheckDeadHook, nodeHealthCheckAvailableHook, nodeHealthAlertHook
	nodeHealthCheckDeadHook = func(context.Context) int { dead++; return 0 }
	nodeHealthCheckAvailableHook = func(context.Context) int { avail++; return 0 }
	nodeHealthAlertHook = func() { alerts++ }
	t.Cleanup(func() {
		nodeHealthCheckDeadHook, nodeHealthCheckAvailableHook, nodeHealthAlertHook = oldDead, oldAvail, oldAlert
	})
	var next time.Time
	runNodeHealthTick(&next)
	if dead != 0 || avail != 0 || alerts != 1 {
		t.Fatalf("dead=%d avail=%d alerts=%d", dead, avail, alerts)
	}
}

func TestNodeHealthTickOffKeepsLegacyProbe(t *testing.T) {
	setupProbeHTTP(t, []*ProxyNode{{Name: "sub::n", Protocol: "socks5", Address: "10.0.0.1", Port: 1080}})
	t.Setenv("NODE_SYSTEM", "off")
	var dead, avail, alerts int
	oldDead, oldAvail, oldAlert := nodeHealthCheckDeadHook, nodeHealthCheckAvailableHook, nodeHealthAlertHook
	nodeHealthCheckDeadHook = func(context.Context) int { dead++; return 0 }
	nodeHealthCheckAvailableHook = func(context.Context) int { avail++; return 0 }
	nodeHealthAlertHook = func() { alerts++ }
	t.Cleanup(func() {
		nodeHealthCheckDeadHook, nodeHealthCheckAvailableHook, nodeHealthAlertHook = oldDead, oldAvail, oldAlert
	})
	var next time.Time
	runNodeHealthTick(&next)
	if dead != 0 || avail != 1 || alerts != 1 {
		t.Fatalf("dead=%d avail=%d alerts=%d", dead, avail, alerts)
	}
}

func TestPoolStateCountsUsesV5Availability(t *testing.T) {
	p := setupProbeHTTP(t, []*ProxyNode{
		{Name: "sub::n1", Protocol: "socks5", Address: "10.0.0.1", Port: 1080},
		{Name: "sub::n2", Protocol: "socks5", Address: "10.0.0.2", Port: 1080},
	})
	oldPool := proxyPool
	proxyPool = p
	t.Cleanup(func() { proxyPool = oldPool })
	t.Setenv("NODE_SYSTEM", "force")
	resetV5ForTest()
	rt := v5GetRuntime()
	now := time.Now()
	rt.adm.mu.Lock()
	for _, n := range p.snapshot() {
		rt.adm.st.Egress["node:"+n.Fingerprint] = &EgressState{Lifecycle: "dead", DeadUntil: now.Add(time.Hour).UnixMilli()}
	}
	rt.adm.mu.Unlock()
	avail, total := poolStateCounts()
	if avail != 0 || total != 2 {
		t.Fatalf("avail=%d total=%d", avail, total)
	}
}
