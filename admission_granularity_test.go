package main

import (
	"testing"
	"time"
)

func normalAttempt(model, egress string) *Attempt {
	return &Attempt{Model: model, Egress: egress, Kind: AttemptNormal, GateCount: true}
}

func TestGranularityGateTenAttemptsWithFinal2xx(t *testing.T) {
	a, _, _ := newAdmissionTest(t, 8)
	for i := 0; i < 9; i++ {
		a.Observe(normalAttempt("m", "node:e"+string(rune('a'+i))), Outcome429)
	}
	a.Observe(normalAttempt("m", "node:j"), Outcome2xx)
	a.mu.Lock()
	active := a.gateLocked(a.now()).Active
	a.mu.Unlock()
	if !active {
		t.Fatal("gate did not trip on 10th attempt with 2xx")
	}
}

func TestGranularityGateActiveSuppresses429Writes(t *testing.T) {
	a, _, st := newAdmissionTest(t, 8)
	for i := 0; i < 10; i++ {
		a.Observe(normalAttempt("m", "node:e"+string(rune('a'+i))), Outcome429)
	}
	at := normalAttempt("m", "node:x")
	at.Sub = "s"
	at.Nodes = 2
	a.Observe(at, Outcome429)
	if len(st.Pairs) != 0 {
		t.Fatalf("429 wrote pair: %+v", st.Pairs)
	}
	if len(st.Quota) != 0 {
		t.Fatalf("gate-active 429 wrote quota: %+v", st.Quota)
	}
}

func TestGranularity403LadderAndIsolation(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	want := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}
	for i, d := range want {
		at := normalAttempt("m", "node:e1")
		a.Observe(at, Outcome403)
		p := st.Pairs["*|node:e1"]
		if p == nil || p.Strike != i+1 {
			t.Fatalf("403 %d pair=%+v", i+1, p)
		}
		if p.Until != clock.Now().Add(d).UnixMilli() {
			t.Fatalf("403 %d until=%d want %d", i+1, p.Until, clock.Now().Add(d).UnixMilli())
		}
	}
	if len(st.Subs) != 0 {
		t.Fatalf("403 wrote subs: %+v", st.Subs)
	}
	if _, ok := st.Egress["node:e1"]; ok {
		t.Fatal("403 wrote egress lifecycle")
	}
}

func TestGranularity429DoesNotWritePair(t *testing.T) {
	a, _, st := newAdmissionTest(t, 8)
	at := normalAttempt("m", "node:e1")
	at.Sub = "s"
	at.Nodes = 1
	a.Observe(at, Outcome429)
	if len(st.Pairs) != 0 {
		t.Fatalf("429 wrote pair: %+v", st.Pairs)
	}
	if st.Quota["s|m"] == nil {
		t.Fatal("429 did not record quota")
	}
}

func TestGranularityDirect429OnlyDirect(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8)
	at := normalAttempt("m", "direct")
	at.Direct = true
	a.Observe(at, Outcome429)
	if st.Direct == nil || st.Direct.Until != clock.Now().Add(60*time.Second).UnixMilli() {
		t.Fatalf("direct until=%+v", st.Direct)
	}
	if len(st.Quota) != 0 {
		t.Fatalf("direct 429 wrote quota: %+v", st.Quota)
	}
}

func TestGranularityLifecycleDoesNotWrite429403(t *testing.T) {
	a, _, st := newAdmissionTest(t, 8)
	st.Egress["node:e1"] = &EgressState{Lifecycle: "active"}
	for _, class := range []OutcomeClass{Outcome429, Outcome403} {
		at := normalAttempt("m", "node:e1")
		at.lifecycle = true
		a.Observe(at, class)
		if st.Egress["node:e1"].Lifecycle != "active" {
			t.Fatalf("lifecycle %s wrote %+v", class, st.Egress["node:e1"])
		}
	}
}

func TestGranularityQuotaGate(t *testing.T) {
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	c := &admissionTestClock{now: now}
	SetClock(c)
	defer SetClock(nil)
	st := NewNodeStateV5(now)
	a := NewAdmission(st, 8, nil)
	a.clock = c

	a.recordQuotaLocked("s", "m", "node:e1", 1, now)
	a.recordQuotaLocked("s", "m", "node:e1", 1, now)
	if q := st.Quota["s|m"]; q == nil || q.Until != 0 {
		t.Fatalf("single node tripped too early: %+v", q)
	}
	a.recordQuotaLocked("s", "m", "node:e1", 1, now)
	if q := st.Quota["s|m"]; q == nil || q.Until == 0 {
		t.Fatalf("single node did not trip: %+v", q)
	}

	st2 := NewNodeStateV5(now)
	a2 := NewAdmission(st2, 8, nil)
	a2.clock = c
	for i := 0; i < 3; i++ {
		a2.recordQuotaLocked("s2", "m", "node:e1", 2, now)
	}
	if q := st2.Quota["s2|m"]; q == nil || q.Until != 0 {
		t.Fatalf("2-node subscription tripped on same egress: %+v", q)
	}

	// Rolling window: after 10 minutes the same egress starts a new window.
	c.Advance(10*time.Minute + time.Millisecond)
	before := st.Quota["s|m"].Until
	a.recordQuotaLocked("s", "m", "node:e2", 1, c.now)
	q := st.Quota["s|m"]
	if q.Hits != 1 || q.Since != c.now.UnixMilli() {
		t.Fatalf("window did not roll: %+v", q)
	}
	if q.Until != before {
		t.Fatalf("active gate extended: before=%d after=%d", before, q.Until)
	}
}

func TestGranularityPairsOnlyWildcard(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s"},
	)
	a.Observe(normalAttempt("m", "node:e1"), Outcome2xx)
	manual := normalAttempt("m", "node:e1")
	manual.Kind = AttemptManualProbe
	manual.GateCount = false
	manual.Sub = "s"
	a.Observe(manual, Outcome2xx)
	a.Observe(normalAttempt("m", "node:e1"), Outcome403)
	a.Observe(normalAttempt("m", "node:e2"), Outcome5xx)
	a.Observe(normalAttempt("m", "node:e2"), Outcome429)
	for key := range st.Pairs {
		if len(key) < 2 || key[:2] != "*|" {
			t.Fatalf("non-wildcard pair key %q (all=%+v)", key, st.Pairs)
		}
	}
	if len(st.Subs) != 0 {
		t.Fatalf("Subs written: %+v", st.Subs)
	}

	// A wildcard pair still needs two half-open successes to clear strikes.
	p := st.Pairs[PairKey("*", "node:e1")]
	if p == nil || p.Strike == 0 || p.Until == 0 {
		t.Fatalf("403 strike missing: %+v", p)
	}
	p.Strikes = 1
	p.Until = clock.Now().Add(-time.Millisecond).UnixMilli()
	p.OKStreak = 0
	a.Observe(normalAttempt("m", "node:e1"), Outcome2xx)
	if p.Strikes != 1 || p.OKStreak != 1 {
		t.Fatalf("first recovery 2xx = %+v", p)
	}
	a.Observe(normalAttempt("m", "node:e1"), Outcome2xx)
	if p.Strikes != 0 || p.Strike != 0 || p.OKStreak != 0 || p.Until != 0 {
		t.Fatalf("second recovery 2xx = %+v", p)
	}
}

func TestGranularityProbeWithoutLifecycleUsesNormalSemantics(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8)
	st.Direct = &DirectState{Until: clock.Now().Add(time.Minute).UnixMilli()}
	at := normalAttempt("m", "direct")
	at.Kind = AttemptProbe
	at.GateCount = false
	at.Direct = true
	a.Observe(at, Outcome2xx)
	if st.Direct.Until != 0 {
		t.Fatalf("AttemptProbe without lifecycle did not use normal direct-clear path: %+v", st.Direct)
	}
}
