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

func TestPairCoolOnFirst429(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	at := mustAdmit(t, a, "m")
	a.Observe(at, Outcome429)

	p := st.Pairs[PairKey("m", "node:e1")]
	if p == nil || p.Strikes != 1 || p.Until != clock.Now().Add(60*time.Second).UnixMilli() {
		t.Fatalf("first 429 state = %+v", p)
	}
	if got := a.RetryAfter("m"); got < 59*time.Second || got > 60*time.Second {
		t.Fatalf("RetryAfter = %v, want ~60s", got)
	}
	if at, reason := a.Admit("m", nil); at != nil || reason != "no_candidate" {
		t.Fatalf("Admit during cooldown = (%v,%q), want no_candidate", at, reason)
	}

	clock.Advance(60*time.Second + time.Millisecond)
	at = mustAdmit(t, a, "m")
	a.Observe(at, Outcome2xx)
	if p.Strikes != 0 || p.Until != 0 || p.A != 1 {
		t.Fatalf("2xx did not recover: %+v", p)
	}
}

func TestInflightAtomic(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	st.Pairs[PairKey("m", "node:e1")] = &PairState{A: 100, B: 50, LastOK: clock.Now().UnixMilli(), LastFail: clock.Now().UnixMilli(), LastDecay: clock.Now().UnixMilli()}

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

func TestPairCoolNoCrossModel(t *testing.T) {
	a, _, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m1", Sub: "s"})
	at := mustAdmit(t, a, "m1")
	a.Observe(at, Outcome429)

	at = mustAdmit(t, a, "m2")
	a.Observe(at, Outcome2xx)
	if p := st.Pairs[PairKey("m1", "node:e1")]; p == nil || p.Strikes != 1 {
		t.Fatalf("m1 pair changed: %+v", p)
	}
	if p := st.Pairs[PairKey("m2", "node:e1")]; p == nil || p.A != 1 {
		t.Fatalf("m2 pair not independent: %+v", p)
	}
}

func TestAllCooling(t *testing.T) {
	candidates := make([]Candidate, 43)
	for i := range candidates {
		candidates[i] = Candidate{Egress: "node:e" + string(rune('A'+i)), Model: "m", Sub: "s"}
	}
	a, _, st := newAdmissionTest(t, 8, candidates...)
	for range candidates {
		at := mustAdmit(t, a, "m")
		a.Observe(at, Outcome429)
	}
	if at, reason := a.Admit("m", nil); at != nil || reason != "no_candidate" {
		t.Fatalf("all cooling Admit = (%v,%q)", at, reason)
	}
	if got := a.RetryAfter("m"); got <= 0 || got > 5*time.Minute {
		t.Fatalf("RetryAfter = %v", got)
	}
	probe := mustAdmitProbe(t, a, "m")
	a.Observe(probe, Outcome2xx)
	if at := mustAdmit(t, a, "m"); at == nil {
		t.Fatal("probe success did not recover a pair")
	} else {
		a.Observe(at, Outcome2xx)
	}
	_ = st
}

func TestAllSubQuarantined(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s"},
	)
	st.Subs["s"] = &SubState{Until: clock.Now().Add(10 * time.Minute).UnixMilli()}
	if at, reason := a.Admit("m", nil); at != nil || reason != "sub_quarantine" {
		t.Fatalf("Admit = (%v,%q), want sub_quarantine", at, reason)
	}
	if got := a.RetryAfter("m"); got < 9*time.Minute || got > 10*time.Minute {
		t.Fatalf("RetryAfter = %v, want ~10m", got)
	}
	probe := mustAdmitProbe(t, a, "m")
	a.Observe(probe, Outcome2xx)
	if st.Subs["s"].Until != 0 {
		t.Fatalf("probe 2xx did not clear sub: %+v", st.Subs["s"])
	}
	at := mustAdmit(t, a, "m")
	a.Observe(at, Outcome2xx)
}

func TestProbeBypassesCooldown(t *testing.T) {
	a, _, _ := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	at := mustAdmit(t, a, "m")
	a.Observe(at, Outcome429)
	if at, reason := a.Admit("m", nil); at != nil || reason != "no_candidate" {
		t.Fatalf("normal Admit during cooldown = (%v,%q)", at, reason)
	}
	probe := mustAdmitProbe(t, a, "m")
	a.Observe(probe, Outcome2xx)
	at = mustAdmit(t, a, "m")
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

func TestHardBlockBounded(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	want := []time.Duration{
		60 * time.Second, 2 * time.Minute, 5 * time.Minute, 5 * time.Minute,
		time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 16 * time.Hour, 24 * time.Hour,
	}
	key := PairKey("m", "node:e1")
	for i, d := range want {
		at := mustAdmit(t, a, "m")
		now := clock.Now()
		a.Observe(at, Outcome429)
		p := st.Pairs[key]
		wantStrikes := i + 1
		if wantStrikes > 4 {
			wantStrikes = 4
		}
		wantHard := 0
		if i >= 4 {
			wantHard = i - 3
		}
		if p.Strikes != wantStrikes || p.Hard != wantHard {
			t.Fatalf("step %d strikes/hard = %d/%d, want %d/%d", i, p.Strikes, p.Hard, wantStrikes, wantHard)
		}
		if p.Until != now.Add(d).UnixMilli() {
			t.Fatalf("step %d until = %d, want %d", i, p.Until, now.Add(d).UnixMilli())
		}
		if i+1 < len(want) {
			clock.Advance(d + time.Millisecond)
		}
	}
	hardUntil := st.Pairs[key].Until
	probe := mustAdmitProbe(t, a, "m")
	a.Observe(probe, Outcome429)
	if st.Pairs[key].Until != hardUntil {
		t.Fatalf("probe 429 shortened hard block: %d -> %d", hardUntil, st.Pairs[key].Until)
	}
}

func TestRetryAfter(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	now := clock.Now()
	st.Pairs[PairKey("m", "node:e1")] = &PairState{Until: now.Add(2 * time.Minute).UnixMilli()}
	st.Subs["s"] = &SubState{Until: now.Add(5 * time.Minute).UnixMilli()}
	st.Egress["node:e1"] = &EgressState{Lifecycle: "active", DeadUntil: now.Add(time.Minute).UnixMilli()}

	if got := a.RetryAfter("m"); got < 59*time.Second || got > time.Minute {
		t.Fatalf("RetryAfter dead = %v, want 1m", got)
	}
	st.Egress["node:e1"].DeadUntil = 0
	if got := a.RetryAfter("m"); got < 119*time.Second || got > 2*time.Minute {
		t.Fatalf("RetryAfter pair = %v, want 2m", got)
	}
	st.Pairs[PairKey("m", "node:e1")].Until = 0
	if got := a.RetryAfter("m"); got < 299*time.Second || got > 5*time.Minute {
		t.Fatalf("RetryAfter sub = %v, want 5m", got)
	}
	st.Subs["s"].Until = 0
	if got := a.RetryAfter("m"); got != time.Second {
		t.Fatalf("RetryAfter empty = %v, want 1s", got)
	}
}

func TestDeadExpiresToProbation(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	st.Egress["node:e1"] = &EgressState{Lifecycle: "dead", DeadUntil: clock.Now().Add(30 * time.Minute).UnixMilli()}
	if at, reason := a.Admit("m", nil); at != nil || reason != "no_candidate" {
		t.Fatalf("dead before expiry = (%v,%q), want no_candidate", at, reason)
	}
	clock.Advance(30*time.Minute + time.Millisecond)
	at := mustAdmit(t, a, "m")
	e := st.Egress["node:e1"]
	if e.Lifecycle != "probation" || e.DeadUntil != 0 {
		t.Fatalf("expired dead before success = %+v, want probation", e)
	}
	a.Observe(at, Outcome2xx)
	if e.ConsecOK != 1 {
		t.Fatalf("expired dead after success = %+v, want consec_ok=1", e)
	}
}

func TestProbationNeedsThreeSuccesses(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	st.Egress["node:e1"] = &EgressState{Lifecycle: "dead", DeadUntil: clock.Now().Add(30 * time.Minute).UnixMilli()}
	clock.Advance(30*time.Minute + time.Millisecond)

	at := mustAdmit(t, a, "m")
	a.Observe(at, Outcome2xx)
	e := st.Egress["node:e1"]
	if e.Lifecycle != "probation" || e.ConsecOK != 1 {
		t.Fatalf("after 1 connected = %+v, want probation/1", e)
	}

	at = mustAdmit(t, a, "m")
	a.Observe(at, Outcome429)
	if e.Lifecycle != "probation" || e.ConsecOK != 2 {
		t.Fatalf("after 2 connected = %+v, want probation/2", e)
	}
	clock.Advance(61 * time.Second)

	at = mustAdmit(t, a, "m")
	a.Observe(at, Outcome5xx)
	if e.Lifecycle != "active" || e.ConsecOK != 0 {
		t.Fatalf("after 3 connected = %+v, want active", e)
	}
}

func TestSub403WindowExpires(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s"},
	)
	at, reason := a.Admit("m", map[string]bool{"node:e2": true})
	if at == nil {
		t.Fatalf("admit e1: %s", reason)
	}
	a.Observe(at, Outcome403)
	if st.Subs["s"].Until != 0 {
		t.Fatalf("one 403 quarantined sub: %+v", st.Subs["s"])
	}

	clock.Advance(11 * time.Minute)
	at, reason = a.Admit("m", map[string]bool{"node:e1": true})
	if at == nil {
		t.Fatalf("admit e2: %s", reason)
	}
	a.Observe(at, Outcome403)
	if st.Subs["s"].Until != 0 {
		t.Fatalf("403 11m apart quarantined sub: %+v", st.Subs["s"])
	}

	at, reason = a.Admit("m", map[string]bool{"node:e2": true})
	if at == nil {
		t.Fatalf("admit e1 again: %s", reason)
	}
	a.Observe(at, Outcome403)
	if st.Subs["s"].Until <= clock.Now().UnixMilli() {
		t.Fatalf("two 403s within 10m did not quarantine: %+v", st.Subs["s"])
	}
}

func TestProbe403RefreshesQuarantine(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	probe := mustAdmitProbe(t, a, "m")
	a.Observe(probe, Outcome403)
	s := st.Subs["s"]
	if s == nil || s.Until != clock.Now().Add(10*time.Minute).UnixMilli() {
		t.Fatalf("probe 403 did not refresh quarantine: %+v", s)
	}
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
