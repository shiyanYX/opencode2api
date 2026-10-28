package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestNodesV5ViewSources(t *testing.T) {
	p := setupProbeHTTP(t, []*ProxyNode{
		{Name: "sub::n1", Protocol: "socks5", Address: "10.0.0.1", Port: 1080},
		{Name: "sub::n2", Protocol: "socks5", Address: "10.0.0.2", Port: 1080},
	})
	nodes := p.snapshot()
	fp := nodes[0].Fingerprint
	oldPath := v5StatePath
	v5StatePath = filepath.Join(t.TempDir(), "v5_state.json")
	t.Cleanup(func() { v5StatePath = oldPath; resetV5ForTest() })
	resetV5ForTest()
	t.Setenv("NODE_SYSTEM", "shadow")
	rt := v5GetRuntime()
	if rt == nil {
		t.Fatal("runtime nil")
	}
	now := time.Now()
	untilPair := now.Add(30 * time.Second).UnixMilli()
	untilQuota := now.Add(8 * time.Minute).UnixMilli()
	rt.adm.mu.Lock()
	rt.adm.st.Pairs[PairKey(legacyPairModel, "node:"+fp)] = &PairState{Until: untilPair}
	rt.adm.st.Quota[quotaKey("sub", "m1")] = &QuotaState{Until: untilQuota + 60_000}
	rt.adm.st.Quota[quotaKey("sub", "m2")] = &QuotaState{Until: untilQuota}
	rt.adm.st.Egress["node:"+fp] = &EgressState{Lifecycle: "probation"}
	rt.adm.st.Gate = &GateState{Active: true, PausedUntil: now.Add(time.Minute).UnixMilli(), CanaryLevel: 2, NextCanary: now.Add(2 * time.Minute).UnixMilli(), NodeProbeUsed: true}
	rt.adm.mu.Unlock()

	rr := httptest.NewRecorder()
	adminNodesHandler(rr, httptest.NewRequest(http.MethodGet, "/api/nodes", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Nodes []nodeAdminView `json:"nodes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var got *v5NodeView
	for i := range resp.Nodes {
		if resp.Nodes[i].Fingerprint == fp {
			got = resp.Nodes[i].V5
		}
	}
	if got == nil {
		t.Fatal("v5 view nil")
	}
	if got.PairUntil != untilPair || got.Lifecycle != "probation" {
		t.Fatalf("view=%+v", got)
	}
	if got.Quota == nil || got.Quota.Model != "m2" || got.Quota.Until != untilQuota || got.QuotaUntil == nil || *got.QuotaUntil != untilQuota {
		t.Fatalf("quota=%+v until=%v", got.Quota, got.QuotaUntil)
	}
	if got.Gate == nil || !got.Gate.Active || got.Gate.CanaryLevel != 2 || !got.Gate.NodeProbeUsed {
		t.Fatalf("gate=%+v", got.Gate)
	}

	t.Setenv("NODE_SYSTEM", "off")
	rr = httptest.NewRecorder()
	adminNodesHandler(rr, httptest.NewRequest(http.MethodGet, "/api/nodes", nil))
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for i := range resp.Nodes {
		if resp.Nodes[i].V5 != nil {
			t.Fatalf("v5 must be null when off: %+v", resp.Nodes[i].V5)
		}
	}
}
