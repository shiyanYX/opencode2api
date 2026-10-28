package main

import (
	"math"
	"testing"
	"time"
)

func seedSelectionPair(st *NodeStateV5, model, egress string, a, b float64, now time.Time) *PairState {
	p := &PairState{
		A:         a,
		B:         b,
		LastOK:    now.UnixMilli(),
		LastFail:  now.UnixMilli(),
		LastDecay: now.UnixMilli(),
	}
	st.Pairs[PairKey(legacyPairModel, egress)] = p
	return p
}

func TestColdStartPrefersFewestAttempts(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s"},
	)
	seedSelectionPair(st, "m", "node:e2", 3, 0, clock.Now())

	at, reason := a.Admit("m", nil)
	if at == nil {
		t.Fatalf("Admit rejected: %s", reason)
	}
	if at.Egress != "node:e1" || !at.explore {
		t.Fatalf("cold start chose %q explore=%v, want node:e1 explore=true", at.Egress, at.explore)
	}
}

func TestColdStartSingleInflight(t *testing.T) {
	a, _, _ := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})

	first, reason := a.Admit("m", nil)
	if first == nil || first.Egress != "node:e1" || !first.explore {
		t.Fatalf("first cold Admit = (%v,%q), want node:e1 explore", first, reason)
	}
	if at, reason := a.Admit("m", nil); at != nil || reason != "all_inflight" {
		t.Fatalf("second cold Admit = (%v,%q), want all_inflight", at, reason)
	}
	a.Observe(first, Outcome2xx)
	third, reason := a.Admit("m", nil)
	if third == nil || third.Egress != "node:e1" {
		t.Fatalf("Admit after Observe = (%v,%q), want node:e1", third, reason)
	}
}

func TestUCBExploresLowConfidence(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s"},
	)
	seedSelectionPair(st, "m", "node:e1", 52.55, 47.45, clock.Now())
	seedSelectionPair(st, "m", "node:e2", 0, 5, clock.Now())

	at, reason := a.Admit("m", nil)
	if at == nil {
		t.Fatalf("Admit rejected: %s", reason)
	}
	if at.Egress != "node:e2" {
		t.Fatalf("UCB chose %q, want low-confidence node:e2", at.Egress)
	}
}

func TestUCBPrefersHighPosterior(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s"},
	)
	seedSelectionPair(st, "m", "node:e1", 52.55, 47.45, clock.Now())
	seedSelectionPair(st, "m", "node:e2", 50, 50, clock.Now())

	at, reason := a.Admit("m", nil)
	if at == nil {
		t.Fatalf("Admit rejected: %s", reason)
	}
	if at.Egress != "node:e1" {
		t.Fatalf("UCB chose %q, want high-posterior node:e1", at.Egress)
	}
}

func TestTieBreakLastOK(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s"},
	)
	p1 := seedSelectionPair(st, "m", "node:e1", 50, 50, clock.Now())
	p2 := seedSelectionPair(st, "m", "node:e2", 50, 50, clock.Now())
	p1.LastOK = clock.Now().Add(-time.Minute).UnixMilli()
	p2.LastOK = clock.Now().UnixMilli()

	at, reason := a.Admit("m", nil)
	if at == nil {
		t.Fatalf("Admit rejected: %s", reason)
	}
	if at.Egress != "node:e2" {
		t.Fatalf("tie-break chose %q, want latest LastOK node:e2", at.Egress)
	}
}

func TestDecayEvidenceTowardPrior(t *testing.T) {
	a, clock, _ := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	now := clock.Now()
	p := &PairState{A: 50, B: 50, LastDecay: now.Add(-7 * 24 * time.Hour).UnixMilli()}

	pBefore := (pairPriorA + 50 + 0.5) / (pairPriorA + pairPriorB + 100 + 1)
	a.decayPairLocked(p, now.UnixMilli())
	pAfter := pairEffectivePosterior(p)
	p0 := (pairPriorA + 0.5) / (pairPriorA + pairPriorB + 1)

	if math.Abs((p.A+p.B)-50) > 1e-6 {
		t.Fatalf("n after 7d = %v, want 50", p.A+p.B)
	}
	f := 0.5
	wantRatio := f * (pairPriorA + pairPriorB + 100 + 1) / (pairPriorA + pairPriorB + 50 + 1)
	gotRatio := math.Abs(pAfter-p0) / math.Abs(pBefore-p0)
	if math.Abs(gotRatio-wantRatio) > 1e-6 {
		t.Fatalf("distance ratio = %v, want %v", gotRatio, wantRatio)
	}
	if pairEffectiveA(p) < pairPriorA || pairEffectiveB(p) < pairPriorB {
		t.Fatalf("prior shrank: a=%v b=%v", pairEffectiveA(p), pairEffectiveB(p))
	}

	p2 := &PairState{A: 50, B: 50, LastDecay: now.Add(-200 * 24 * time.Hour).UnixMilli()}
	a.decayPairLocked(p2, now.UnixMilli())
	if p2.A+p2.B >= 0.01 {
		t.Fatalf("n after 200d = %v, want <0.01", p2.A+p2.B)
	}
	if math.Abs(pairEffectivePosterior(p2)-p0) >= 1e-6 {
		t.Fatalf("p after 200d = %v, want %v", pairEffectivePosterior(p2), p0)
	}
}

func TestDecayDoesNotShrinkPrior(t *testing.T) {
	a, clock, _ := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	now := clock.Now()
	p := &PairState{A: 0, B: 0, LastDecay: now.Add(-200 * 24 * time.Hour).UnixMilli()}
	a.decayPairLocked(p, now.UnixMilli())
	if pairEffectiveA(p) != pairPriorA || pairEffectiveB(p) != pairPriorB {
		t.Fatalf("prior = %v/%v, want %v/%v", pairEffectiveA(p), pairEffectiveB(p), pairPriorA, pairPriorB)
	}
	if p.A != 0 || p.B != 0 {
		t.Fatalf("empty evidence changed: %+v", p)
	}
}

func TestDecayAppliedBeforeIncrement(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	now := clock.Now()
	p := seedSelectionPair(st, "m", "node:e1", 50, 50, now)
	p.LastDecay = now.Add(-7 * 24 * time.Hour).UnixMilli()

	at, reason := a.Admit("m", nil)
	if at == nil {
		t.Fatalf("Admit rejected: %s", reason)
	}
	a.Observe(at, Outcome2xx)

	if math.Abs(p.A-26) > 1e-6 || math.Abs(p.B-25) > 1e-6 {
		t.Fatalf("after decay+2xx = %v/%v, want 26/25", p.A, p.B)
	}
	if math.Abs((p.A+p.B)-51) > 1e-6 {
		t.Fatalf("n = %v, want 51", p.A+p.B)
	}
}

func TestStickyPreferred(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s"},
	)
	at, reason := a.AdmitPreferred("m", nil, "node:e2")
	if at == nil || at.Egress != "node:e2" {
		t.Fatalf("prefer = (%v,%q), want node:e2", at, reason)
	}
	a.Observe(at, Outcome2xx)

	p := st.Pairs[PairKey(legacyPairModel, "node:e2")]
	p.Until = clock.Now().Add(time.Minute).UnixMilli()
	at, reason = a.AdmitPreferred("m", nil, "node:e2")
	if at == nil {
		t.Fatalf("fallback Admit rejected: %s", reason)
	}
	if at.Egress != "node:e1" {
		t.Fatalf("cooling prefer chose %q, want fallback node:e1", at.Egress)
	}
}

func pairEffectivePosterior(p *PairState) float64 {
	a := pairEffectiveA(p)
	b := pairEffectiveB(p)
	return (a + 0.5) / (a + b + 1)
}
