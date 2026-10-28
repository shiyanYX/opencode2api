package main

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestProbePreflightTimeoutSixSeconds(t *testing.T) {
	var remaining time.Duration
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			if d, ok := r.Context().Deadline(); ok {
				remaining = time.Until(d)
			}
			return nil, context.DeadlineExceeded
		})}
	}
	defer func() { probeClientFor = defaultProbeClient }()
	res := probeOne(context.Background(), ProxyNode{Fingerprint: "fp1"}, "m", "https://example.invalid/models")
	if !res.Timeout || res.Class != probeClassUnreachable || res.Stage != string(ProbeStagePreflight) {
		t.Fatalf("res=%+v, want unreachable/timeout/preflight", res)
	}
	if remaining < 5500*time.Millisecond || remaining > 6500*time.Millisecond {
		t.Fatalf("preflight deadline remaining=%v, want ~6s", remaining)
	}
}

func TestManualProbePreflightTimeoutIsWeak(t *testing.T) {
	a, _, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Sub: "sub", Model: "m"})
	at := &Attempt{Model: "m", Egress: "node:e1", Sub: "sub", Stage: ProbeStagePreflight}
	a.ObserveManualProbe(at, OutcomeTimeout)
	e := st.Egress["node:e1"]
	if e != nil && e.Lifecycle == "dead" {
		t.Fatal("one preflight timeout must not declare dead")
	}
	if _, ok := st.Pairs[PairKey(legacyPairModel, "node:e1")]; ok {
		t.Fatal("preflight timeout must not write pair")
	}
	a.ObserveManualProbe(&Attempt{Model: "m", Egress: "node:e1", Sub: "sub", Stage: ProbeStagePreflight}, OutcomeTimeout)
	e = st.Egress["node:e1"]
	if e == nil || e.Lifecycle != "dead" {
		t.Fatalf("two preflight timeouts lifecycle=%+v, want dead", e)
	}
}

func TestManualProbePreflightHardFailureStillDead(t *testing.T) {
	a, _, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Sub: "sub", Model: "m"})
	a.ObserveManualProbe(&Attempt{Model: "m", Egress: "node:e1", Sub: "sub", Stage: ProbeStagePreflight, HardFail: true}, OutcomeNetwork)
	e := st.Egress["node:e1"]
	if e == nil || e.Lifecycle != "dead" {
		t.Fatalf("hard preflight failure lifecycle=%+v, want dead", e)
	}
}

func TestManualProbe2xxRevivesDeadEgress(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Sub: "sub", Model: "m"})
	st.Egress["node:e1"] = &EgressState{Lifecycle: "dead", DeadUntil: clock.Now().Add(30 * time.Minute).UnixMilli()}
	a.ObserveManualProbe(&Attempt{Model: "m", Egress: "node:e1", Sub: "sub", Stage: ProbeStageRequest}, Outcome2xx)
	e := st.Egress["node:e1"]
	if e == nil || e.Lifecycle != "active" || e.DeadUntil != 0 {
		t.Fatalf("manual 2xx recovery=%+v, want active/0", e)
	}
}
