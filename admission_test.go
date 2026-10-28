package main

import (
	"sync"
	"testing"
	"time"
)

type admissionTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *admissionTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *admissionTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newAdmissionTest(t *testing.T, k int, candidates ...Candidate) (*Admission, *admissionTestClock, *NodeStateV5) {
	t.Helper()
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	c := &admissionTestClock{now: now}
	SetClock(c)
	t.Cleanup(func() { SetClock(nil) })
	st := NewNodeStateV5(now)
	a := NewAdmission(st, k, func(string) []Candidate { return candidates })
	a.clock = c
	return a, c, st
}

func mustAdmit(t *testing.T, a *Admission, model string) *Attempt {
	t.Helper()
	at, reason := a.Admit(model, nil)
	if at == nil {
		t.Fatalf("Admit(%q) rejected: %s", model, reason)
	}
	return at
}

func TestInflightAtomic(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	st.Pairs[PairKey(legacyPairModel, "node:e1")] = &PairState{A: 100, B: 50, LastOK: clock.Now().UnixMilli(), LastFail: clock.Now().UnixMilli(), LastDecay: clock.Now().UnixMilli()}

	const n = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := make([]*Attempt, 0, n)
	reasons := map[string]int{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			at, reason := a.Admit("m", nil)
			mu.Lock()
			if at != nil {
				admitted = append(admitted, at)
			} else {
				reasons[reason]++
			}
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if len(admitted) != 8 || reasons["all_inflight"] != 24 {
		t.Fatalf("admitted=%d all_inflight=%d, want 8/24", len(admitted), reasons["all_inflight"])
	}
	for _, at := range admitted {
		a.Observe(at, Outcome2xx)
	}
}

func TestDirectCooldown(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8,
		Candidate{Egress: "direct", Model: "m1", Direct: true},
		Candidate{Egress: "direct", Model: "m2", Direct: true},
	)
	at := mustAdmit(t, a, "m1")
	a.Observe(at, Outcome429)
	if st.Direct.Until != clock.Now().Add(60*time.Second).UnixMilli() {
		t.Fatalf("direct until = %d", st.Direct.Until)
	}
	if at, reason := a.Admit("m2", nil); at != nil || reason != "no_candidate" {
		t.Fatalf("other direct model admitted during cooldown: (%v,%q)", at, reason)
	}
	clock.Advance(60*time.Second + time.Millisecond)
	at = mustAdmit(t, a, "m2")
	a.Observe(at, Outcome2xx)
}

func mustAdmitProbe(t *testing.T, a *Admission, model string) *Attempt {
	t.Helper()
	at, reason := a.AdmitProbe(model)
	if at == nil {
		t.Fatalf("AdmitProbe(%q) rejected: %s", model, reason)
	}
	return at
}

func TestProbeDeadline(t *testing.T) {
	a, _, _ := newAdmissionTest(t, 1, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	a.probeTimeout = 25 * time.Millisecond
	probe := mustAdmitProbe(t, a, "m")
	if at, reason := a.AdmitProbe("m"); at != nil || reason != "probe_busy" {
		t.Fatalf("second probe = (%v,%q), want probe_busy", at, reason)
	}
	if at, reason := a.Admit("m", nil); at != nil || reason != "all_inflight" {
		t.Fatalf("slot before deadline = (%v,%q), want all_inflight", at, reason)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		at, reason := a.Admit("m", nil)
		if at != nil {
			a.Observe(at, Outcome2xx)
			_ = probe
			return
		}
		if reason != "all_inflight" {
			t.Fatalf("unexpected reason %q", reason)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("probe slot was not released after deadline")
}

func TestDirectImmuneToLifecycle(t *testing.T) {
	a, _, st := newAdmissionTest(t, 8, Candidate{Egress: "direct", Model: "m", Direct: true})
	for i := 0; i < 5; i++ {
		at := mustAdmit(t, a, "m")
		a.Observe(at, OutcomeNetwork)
	}
	if e := st.Egress["direct"]; e != nil && e.Lifecycle == "dead" {
		t.Fatalf("direct was marked dead: %+v", e)
	}
	at := mustAdmit(t, a, "m")
	a.Observe(at, Outcome2xx)
}
