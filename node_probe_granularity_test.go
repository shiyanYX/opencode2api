package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestManualProbePreflightNetworkMarksDeadWithoutPair(t *testing.T) {
	a, c, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Sub: "sub", Model: "m"})
	at := &Attempt{Model: "m", Egress: "node:e1", Sub: "sub", Stage: ProbeStagePreflight}
	a.ObserveManualProbe(at, OutcomeNetwork)
	e := st.Egress["node:e1"]
	if e == nil || e.Lifecycle != "dead" {
		t.Fatalf("egress lifecycle=%+v, want dead", e)
	}
	if want := c.now.Add(30 * time.Minute).UnixMilli(); e.DeadUntil != want {
		t.Fatalf("dead until=%d want %d", e.DeadUntil, want)
	}
	if _, ok := st.Pairs[PairKey(legacyPairModel, "node:e1")]; ok {
		t.Fatal("preflight network must not write pair")
	}
}

func TestManualProbeRequestNetworkOnlyWritesPair(t *testing.T) {
	a, _, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Sub: "sub", Model: "m"})
	at := &Attempt{Model: "m", Egress: "node:e1", Sub: "sub", Stage: ProbeStageRequest}
	a.ObserveManualProbe(at, OutcomeTimeout)
	e := st.Egress["node:e1"]
	if e != nil && e.Lifecycle == "dead" {
		t.Fatal("request-stage network failure must not declare dead")
	}
	p := st.Pairs[PairKey(legacyPairModel, "node:e1")]
	if p == nil || p.B != 1 {
		t.Fatalf("pair=%+v, want B=1", p)
	}
}

func TestManualProbePreflight429WritesQuotaNotDead(t *testing.T) {
	a, _, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Sub: "sub", Model: "m"})
	at := &Attempt{Model: "m", Egress: "node:e1", Sub: "sub", Stage: ProbeStagePreflight}
	a.ObserveManualProbe(at, Outcome429)
	if q := st.Quota[quotaKey("sub", "m")]; q == nil || q.Hits != 1 {
		t.Fatalf("quota=%+v, want one hit", q)
	}
	if e := st.Egress["node:e1"]; e != nil && e.Lifecycle == "dead" {
		t.Fatal("preflight 429 must not mark dead")
	}
}

func TestManualProbeRequest403IncrementsPairStrike(t *testing.T) {
	a, _, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Sub: "sub", Model: "m"})
	at := &Attempt{Model: "m", Egress: "node:e1", Sub: "sub", Stage: ProbeStageRequest}
	a.ObserveManualProbe(at, Outcome403)
	p := st.Pairs[PairKey(legacyPairModel, "node:e1")]
	if p == nil || p.B != 1 || p.Strike != 1 {
		t.Fatalf("pair=%+v, want B=1 Strike=1", p)
	}
}

func TestManualProbeGateCountToggle(t *testing.T) {
	mk := func() []Candidate {
		cands := make([]Candidate, 0, 31)
		for i := 0; i < 31; i++ {
			cands = append(cands, Candidate{Egress: fmt.Sprintf("node:e%d", i), Sub: "sub", Model: "m"})
		}
		return cands
	}
	run := func(count bool) bool {
		a, _, st := newAdmissionTest(t, 8, mk()...)
		a.probeCountsTowardGate = count
		for i := 0; i < 31; i++ {
			at := &Attempt{Model: "m", Egress: fmt.Sprintf("node:e%d", i), Sub: "sub", Stage: ProbeStageRequest}
			a.ObserveManualProbe(at, Outcome429)
		}
		return st.Gate.Active
	}
	if run(false) {
		t.Fatal("probe_counts_toward_gate=false must not open gate")
	}
	if !run(true) {
		t.Fatal("probe_counts_toward_gate=true must open gate")
	}
}

func TestManualProbe2xxClearsQuotaButNotGate(t *testing.T) {
	a, _, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Sub: "sub", Model: "m"})
	st.Quota[quotaKey("sub", "m")] = &QuotaState{Until: a.now().Add(10 * time.Minute).UnixMilli()}
	st.Gate.Active = true
	at := &Attempt{Model: "m", Egress: "node:e1", Sub: "sub", Stage: ProbeStageRequest}
	a.ObserveManualProbe(at, Outcome2xx)
	if _, ok := st.Quota[quotaKey("sub", "m")]; ok {
		t.Fatal("manual 2xx must clear quota")
	}
	if !st.Gate.Active {
		t.Fatal("manual 2xx must not clear gate")
	}
	p := st.Pairs[PairKey(legacyPairModel, "node:e1")]
	if p == nil || p.A != 1 {
		t.Fatalf("pair=%+v, want A=1", p)
	}
}

func TestProbeManagerCanceledDoesNotObserve(t *testing.T) {
	var hits atomic.Int64
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(*http.Request) (*http.Response, error) {
			return nil, context.Canceled
		})}
	}
	defer func() { probeClientFor = defaultProbeClient }()
	m := newProbeManager()
	m.observe = func(ProxyNode, probeResult, string) { hits.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := m.Single(ctx, ProxyNode{Fingerprint: "fp1", Name: "sub"}, "m", "https://opencode.ai/zen/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	if res.Class != "canceled" {
		t.Fatalf("class=%q, want canceled", res.Class)
	}
	if hits.Load() != 0 {
		t.Fatalf("observe calls=%d, want 0", hits.Load())
	}
}

func TestProbeManagerObservesNonCanceled(t *testing.T) {
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/models") {
				return probeMgrResponse(204, ""), nil
			}
			return probeMgrResponse(200, `{}`), nil
		})}
	}
	defer func() { probeClientFor = defaultProbeClient }()
	m := newProbeManager()
	var got probeResult
	m.observe = func(_ ProxyNode, res probeResult, _ string) { got = res }
	if _, err := m.Single(context.Background(), ProxyNode{Fingerprint: "fp1", Name: "sub"}, "m", "https://opencode.ai/zen/v1/models"); err != nil {
		t.Fatal(err)
	}
	if got.Class != probeClassOK || got.Stage != string(ProbeStageRequest) {
		t.Fatalf("observed=%+v, want ok/request", got)
	}
}

func TestNoCandidateHTTPRetryAndReason(t *testing.T) {
	status, h, err := v5NoCandidateHTTP(EgressResult{NoCandidateReason: "quota", RetryAfter: 600 * time.Second})
	if status != http.StatusTooManyRequests || h.Get("Retry-After") != "600" || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("status=%d header=%q err=%v", status, h.Get("Retry-After"), err)
	}
	status, h, _ = v5NoCandidateHTTP(EgressResult{NoCandidateReason: "gate", RetryAfter: 60 * time.Second})
	if status != http.StatusTooManyRequests || h.Get("Retry-After") != "60" {
		t.Fatalf("gate status=%d header=%q", status, h.Get("Retry-After"))
	}
}

func TestProbeOnePreflight429Stage(t *testing.T) {
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/models") {
				return probeMgrResponse(429, `{"error":{"type":"FreeUsageLimitError"}}`), nil
			}
			return probeMgrResponse(200, `{}`), nil
		})}
	}
	defer func() { probeClientFor = defaultProbeClient }()
	res := probeOne(context.Background(), ProxyNode{Fingerprint: "fp1"}, "m", "https://opencode.ai/zen/v1/models")
	if res.Class != probeClassQuota || res.Stage != string(ProbeStagePreflight) {
		t.Fatalf("res=%+v, want quota/preflight", res)
	}
}

func TestProbeOneRequest403Stage(t *testing.T) {
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/models") {
				return probeMgrResponse(204, ""), nil
			}
			return probeMgrResponse(403, "forbidden"), nil
		})}
	}
	defer func() { probeClientFor = defaultProbeClient }()
	res := probeOne(context.Background(), ProxyNode{Fingerprint: "fp1"}, "m", "https://opencode.ai/zen/v1/models")
	if res.Class != probeClassDenied || res.Stage != string(ProbeStageRequest) {
		t.Fatalf("res=%+v, want denied/request", res)
	}
}

func TestProbeCountsTowardGateConfig(t *testing.T) {
	dir := t.TempDir()
	for _, want := range []bool{false, true} {
		path := dir + "/config.json"
		body := fmt.Sprintf(`{"probe_counts_toward_gate":%v}`, want)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := loadConfig(path).ProbeCountsTowardGate; got != want {
			t.Fatalf("config=%v want %v", got, want)
		}
	}
}

func TestAdmitErrorQuotaRetryAndHTTP(t *testing.T) {
	a, c, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Sub: "sub", Model: "m"})
	st.Quota[quotaKey("sub", "m")] = &QuotaState{Until: c.now.Add(600 * time.Second).UnixMilli()}
	e := v5AdmitError(&v5Runtime{adm: a}, "m", nil)
	if e.Reason != "quota" || e.RetryAfter < 599*time.Second || e.RetryAfter > 602*time.Second || e.Status != http.StatusTooManyRequests {
		t.Fatalf("err=%+v, want quota/600s/429", e)
	}
	_, h, _ := v5NoCandidateHTTP(EgressResult{NoCandidateReason: e.Reason, RetryAfter: e.RetryAfter})
	if h.Get("Retry-After") != "600" {
		t.Fatalf("retry-after=%q want 600", h.Get("Retry-After"))
	}

	delete(st.Quota, quotaKey("sub", "m"))
	st.Gate.Active = true
	st.Gate.PausedUntil = c.now.Add(60 * time.Second).UnixMilli()
	e = v5AdmitError(&v5Runtime{adm: a}, "m", nil)
	if e.Reason != "gate" || e.RetryAfter < 59*time.Second || e.RetryAfter > 61*time.Second {
		t.Fatalf("err=%+v, want gate/60s", e)
	}
	_, h, _ = v5NoCandidateHTTP(EgressResult{NoCandidateReason: e.Reason, RetryAfter: e.RetryAfter})
	if h.Get("Retry-After") != "60" {
		t.Fatalf("retry-after=%q want 60", h.Get("Retry-After"))
	}
}

func TestManualProbeQuotaWhileGateActive(t *testing.T) {
	a, _, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Sub: "sub", Model: "m"})
	st.Gate.Active = true
	st.Gate.PausedUntil = a.now().Add(10 * time.Minute).UnixMilli()
	at := &Attempt{Model: "m", Egress: "node:e1", Sub: "sub", Stage: ProbeStageRequest}
	a.ObserveManualProbe(at, Outcome429)
	q := st.Quota[quotaKey("sub", "m")]
	if q == nil || q.Until <= a.now().UnixMilli() {
		t.Fatalf("quota=%+v, want active quota during gate", q)
	}
	if !st.Gate.Active || st.Gate.PausedUntil == 0 {
		t.Fatalf("manual probe must not clear active gate: %+v", st.Gate)
	}
}

func TestProbeGateConfigHotReload(t *testing.T) {
	resetV5ForTest()
	t.Setenv("NODE_SYSTEM", "force")
	rt := v5GetRuntime()
	if rt == nil {
		t.Fatal("runtime not created")
	}
	defer resetV5ForTest()
	at := &Attempt{Model: "m", Egress: "node:e1", Sub: "sub"}
	v5ApplyProbeGateConfig(true)
	rt.adm.ObserveManualProbe(at, Outcome2xx)
	if !at.GateCount {
		t.Fatal("GateCount did not follow hot-reloaded true")
	}
	at2 := &Attempt{Model: "m", Egress: "node:e1", Sub: "sub"}
	v5ApplyProbeGateConfig(false)
	rt.adm.ObserveManualProbe(at2, Outcome2xx)
	if at2.GateCount {
		t.Fatal("GateCount did not follow hot-reloaded false")
	}
	resetV5ForTest()
	v5ApplyProbeGateConfig(true) // nil runtime must be a no-op
	if v5RT != nil {
		t.Fatal("hot-reload created a runtime")
	}
}
