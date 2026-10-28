package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNodesAvailabilityStatsAndFields(t *testing.T) {
	p := setupProbeHTTP(t, []*ProxyNode{
		{Name: "sub::n1", Protocol: "socks5", Address: "10.0.0.1", Port: 1080},
		{Name: "sub::n2", Protocol: "socks5", Address: "10.0.0.2", Port: 1080},
	})
	t.Setenv("NODE_SYSTEM", "off")
	nodes := p.snapshot()
	nodeProber.mu.Lock()
	nodeProber.results[nodes[0].Fingerprint] = probeResult{Fingerprint: nodes[0].Fingerprint, Class: probeClassUnreachable, Stage: string(ProbeStageRequest), At: time.Now()}
	nodeProber.results[nodes[1].Fingerprint] = probeResult{Fingerprint: nodes[1].Fingerprint, Class: probeClassOK, Stage: string(ProbeStageRequest), At: time.Now()}
	nodeProber.mu.Unlock()

	rr := httptest.NewRecorder()
	adminNodesHandler(rr, httptest.NewRequest(http.MethodGet, "/api/nodes", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Healthy     int             `json:"healthy"`
		Exhausted   int             `json:"exhausted_count"`
		Dead        int             `json:"dead_count"`
		Unreachable int             `json:"unreachable_count"`
		Untested    int             `json:"untested_count"`
		Nodes       []nodeAdminView `json:"nodes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	byFP := map[string]nodeAdminView{}
	for _, n := range resp.Nodes {
		byFP[n.Fingerprint] = n
	}
	if got := byFP[nodes[0].Fingerprint].Availability; got != availabilityUnreachable {
		t.Fatalf("unreachable availability=%q", got)
	}
	if got := byFP[nodes[1].Fingerprint].Availability; got != availabilityOK {
		t.Fatalf("ok availability=%q", got)
	}
	if resp.Healthy != 1 || resp.Unreachable != 1 || resp.Untested != 0 || resp.Dead != 0 || resp.Exhausted != 0 {
		t.Fatalf("stats=%+v", resp)
	}
	if byFP[nodes[0].Fingerprint].AvailabilityReason == "" {
		t.Fatal("missing reason")
	}
}
