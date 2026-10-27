package main

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func forceTestCandidates(model string, cands ...Candidate) func(string) []Candidate {
	return func(m string) []Candidate {
		for i := range cands {
			cands[i].Model = m
		}
		return cands
	}
}

func installForceNode(t *testing.T, region string) (*nodePool, *ProxyNode) {
	t.Helper()
	p := newProxyPool(filepath.Join(t.TempDir(), "proxy_state.json"))
	p.setNodes([]*ProxyNode{{Name: "n-" + region, Protocol: "socks5", Address: "10.0.0.1", Port: 1080, Region: region}})
	n := p.snapshot()[0]
	p.mu.Lock()
	p.clients[n.Fingerprint] = &http.Client{}
	p.mu.Unlock()
	old := proxyPool
	proxyPool = p
	t.Cleanup(func() { proxyPool = old })
	return p, n
}

func TestV5ForceBypassHelper(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{"", false}, {"off", false}, {"shadow", false}, {"force", true},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv("NODE_SYSTEM", tc.mode)
			if got := v5LegacyGatesBypassed(); got != tc.want {
				t.Fatalf("v5LegacyGatesBypassed()=%v want %v", got, tc.want)
			}
		})
	}
}

func TestV5ForceGetUsesAdmission(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "force")
	resetV5ForTest()
	_, n := installForceNode(t, "jp")
	v5CandidatesHook = forceTestCandidates("m", Candidate{Egress: nodeEgressKey(n.Fingerprint), Sub: subKeyOf(n.Name), Region: "jp"})
	eg := NewEgressClient(&http.Client{})
	got := eg.Get(EgressRequest{Model: "m"})
	if got.Direct || got.NodeFP != n.Fingerprint {
		t.Fatalf("force Get=%+v want node %s", got, n.Fingerprint)
	}
	if got.V5() == nil {
		t.Fatal("force Get did not return v5 lease")
	}
	rt := v5GetRuntime()
	rt.adm.Observe(got.V5(), Outcome2xx)
}

func TestV5ForceNoCandidateFallsBackDirect(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "force")
	resetV5ForTest()
	v5CandidatesHook = func(string) []Candidate { return nil }
	ctx := beginCallLog(context.Background(), "/v1/chat/completions", "m", false, "test")
	eg := NewEgressClient(&http.Client{})
	got := eg.Get(EgressRequest{Model: "m", Context: ctx})
	if !got.Direct || got.V5() != nil {
		t.Fatalf("fallback=%+v want direct without v5", got)
	}
	cr := callRecorderFrom(ctx)
	cr.mu.Lock()
	events := append([]CallEvent(nil), cr.rec.Events...)
	cr.mu.Unlock()
	count := 0
	for _, ev := range events {
		if ev.Type == "v5_fallback" {
			count++
			if ev.Reason != "no_candidate" || ev.Model != "m" {
				t.Fatalf("fallback event=%+v", ev)
			}
		}
	}
	if count != 1 {
		t.Fatalf("fallback events=%d want 1", count)
	}
	// 同一 reason 一分钟内不重复落事件。
	eg.Get(EgressRequest{Model: "m", Context: ctx})
	cr.mu.Lock()
	defer cr.mu.Unlock()
	count = 0
	for _, ev := range cr.rec.Events {
		if ev.Type == "v5_fallback" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("rate-limited fallback events=%d want 1", count)
	}
}

func TestV5ForceObserveReleasesOnce(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "force")
	resetV5ForTest()
	v5CandidatesHook = forceTestCandidates("m", Candidate{Egress: "node:e1", Sub: "s"})
	rt := v5GetRuntime()
	at, reason := rt.adm.AdmitPreferred("m", nil, "")
	if at == nil {
		t.Fatalf("AdmitPreferred failed: %s", reason)
	}
	eg := EgressResult{NodeFP: "e1", v5: at}
	ctx := context.Background()
	v5ObserveUpstream(ctx, "m", eg, 200, nil, nil, "")
	v5ObserveUpstream(ctx, "m", eg, 200, nil, nil, "")
	if got := len(rt.adm.sems["node:e1"]); got != 0 {
		t.Fatalf("sem len=%d want 0", got)
	}
	if p := rt.adm.st.Pairs[PairKey("m", "node:e1")]; p == nil || p.A != 1 || p.B != 0 {
		t.Fatalf("pair=%+v want A=1 B=0", p)
	}
}

func TestV5ForceRegionFilter(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "force")
	resetV5ForTest()
	v5CandidatesHook = forceTestCandidates("m",
		Candidate{Egress: "node:jp", Sub: "s-jp", Region: "jp"},
		Candidate{Egress: "node:us", Sub: "s-us", Region: "us"},
	)
	at, ok := v5Admit("m", "jp", nil)
	if !ok || at == nil || at.Egress != "node:jp" {
		t.Fatalf("v5Admit(jp)=%+v,%v want node:jp", at, ok)
	}
	v5DropAttempt(at)
}

func TestV5ForceOtherClassReleases(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "force")
	resetV5ForTest()
	v5CandidatesHook = forceTestCandidates("m", Candidate{Egress: "node:e1", Sub: "s"})
	rt := v5GetRuntime()
	at, _ := rt.adm.AdmitPreferred("m", nil, "")
	if at == nil {
		t.Fatal("AdmitPreferred failed")
	}
	v5ObserveUpstream(context.Background(), "m", EgressResult{NodeFP: "e1", v5: at}, 400, nil, nil, "")
	if got := len(rt.adm.sems["node:e1"]); got != 0 {
		t.Fatalf("sem len=%d want 0", got)
	}
	if p := rt.adm.st.Pairs[PairKey("m", "node:e1")]; p != nil && (p.A != 0 || p.B != 0) {
		t.Fatalf("other unexpectedly changed pair: %+v", p)
	}
}

func TestV5ForceManualPreferred(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "force")
	resetV5ForTest()
	_, n1 := installForceNode(t, "jp")
	// second node with a different address
	p := proxyPool
	p.setNodes([]*ProxyNode{
		{Name: "n-jp", Protocol: "socks5", Address: "10.0.0.1", Port: 1080, Region: "jp"},
		{Name: "n-us", Protocol: "socks5", Address: "10.0.0.2", Port: 1080, Region: "us"},
	})
	nodes := p.snapshot()
	for _, n := range nodes {
		p.mu.Lock()
		p.clients[n.Fingerprint] = &http.Client{}
		p.mu.Unlock()
	}
	p.mu.Lock()
	p.manualPick = nodes[1].Fingerprint
	p.mu.Unlock()
	v5CandidatesHook = nil
	at, ok := v5Admit("m", "", nil)
	if !ok || at == nil || at.Egress != nodeEgressKey(nodes[1].Fingerprint) {
		t.Fatalf("manual preferred=%+v,%v want %s", at, ok, nodeEgressKey(nodes[1].Fingerprint))
	}
	if n1 != nil && n1.Fingerprint == nodes[1].Fingerprint {
		t.Fatal("test node setup invalid")
	}
	v5DropAttempt(at)
}

func TestV5ForceStatsShowsMode(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "force")
	resetV5ForTest()
	st := v5StatsForResponse()
	if st == nil || st.Mode != "force" {
		t.Fatalf("stats=%+v want mode force", st)
	}
	_ = time.Now
}

func TestV5ForceBunPreservesLease(t *testing.T) {
	at := &Attempt{Egress: "node:e1"}
	got := bunEgressResult(EgressResult{Client: &http.Client{Transport: &http.Transport{}}, v5: at})
	if got.V5() != at {
		t.Fatal("bunEgressResult dropped v5 lease")
	}
}

func TestV5DropAttemptClearsExploreInflight(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "force")
	resetV5ForTest()
	v5CandidatesHook = forceTestCandidates("m", Candidate{Egress: "node:e1", Sub: "s"})
	rt := v5GetRuntime()
	at, reason := rt.adm.AdmitPreferred("m", nil, "")
	if at == nil {
		t.Fatalf("first Admit failed: %s", reason)
	}
	if !at.explore {
		t.Fatal("cold-start first admit should be explore")
	}
	if got := len(rt.adm.sems["node:e1"]); got != 1 {
		t.Fatalf("sem len=%d want 1", got)
	}
	v5DropAttempt(at)
	if got := len(rt.adm.sems["node:e1"]); got != 0 {
		t.Fatalf("sem len after drop=%d want 0", got)
	}
	if got := rt.adm.exploreInflight["node:e1"]; got != 0 {
		t.Fatalf("exploreInflight after drop=%d want 0", got)
	}
	at2, reason := rt.adm.AdmitPreferred("m", nil, "")
	if at2 == nil || at2.Egress != "node:e1" {
		t.Fatalf("second Admit after drop=%+v,%s; want same egress explore", at2, reason)
	}
	if !at2.explore {
		t.Fatal("second admit should still be explore")
	}
	v5DropAttempt(at2)
}
