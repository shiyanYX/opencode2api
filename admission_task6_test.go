package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestWinterGateIgnoresQuota429(t *testing.T) {
	a, _, st := newAdmissionTest(t, 8)
	for i := 0; i < 31; i++ {
		a.Observe(normalAttempt("m", "node:e"+string(rune('A'+i))), Outcome429)
	}
	if a.GatePaused() {
		t.Fatalf("429-only window tripped winter gate: %+v", st.Gate)
	}
}

func TestWinterGateLogShapeDoesNotTrip(t *testing.T) {
	a, _, _ := newAdmissionTest(t, 8)
	for i := 0; i < 22430; i++ {
		a.Observe(normalAttempt("m", "node:e"+string(rune('A'+i%31))), Outcome429)
	}
	if a.GatePaused() {
		t.Fatal("10-28 quota storm tripped winter gate")
	}
}

func TestWinterGateCanary429ClearsGate(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s"},
	)
	st.Gate = &GateState{Active: true, PausedUntil: clock.Now().Add(10 * time.Minute).UnixMilli(), NextCanary: clock.Now().UnixMilli(), CanaryLevel: 2}
	st.Direct = &DirectState{}
	at, reason := a.AdmitCanary("m")
	if at == nil {
		t.Fatalf("canary rejected: %s", reason)
	}
	a.Observe(at, Outcome429)
	if g := st.Gate; g.Active || g.CanaryLevel != 0 {
		t.Fatalf("canary 429 did not clear gate: %+v", g)
	}
}

func TestWinterGateCanaryFailureAdvances(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8)
	st.Gate = &GateState{Active: true, PausedUntil: clock.Now().Add(10 * time.Minute).UnixMilli(), NextCanary: clock.Now().UnixMilli()}
	st.Direct = &DirectState{}
	at, reason := a.AdmitCanary("m")
	if at == nil {
		t.Fatalf("canary rejected: %s", reason)
	}
	a.Observe(at, Outcome5xx)
	if g := st.Gate; !g.Active || g.CanaryLevel != 1 || g.NextCanary != clock.Now().Add(60*time.Second).UnixMilli() {
		t.Fatalf("canary 5xx state: %+v", g)
	}
}

func TestWinterGateProbe429ReschedulesOnly(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s"},
	)
	st.Gate = &GateState{Active: true, PausedUntil: clock.Now().Add(10 * time.Minute).UnixMilli(), CanaryOK: true, NextNodeProbe: clock.Now().UnixMilli()}
	at, reason := a.AdmitGateProbe("m")
	if at == nil {
		t.Fatalf("gate probe rejected: %s", reason)
	}
	a.Observe(at, Outcome429)
	g := st.Gate
	if !g.Active || g.CanaryLevel != 0 || g.NodeProbeUsed || g.NextNodeProbe == 0 {
		t.Fatalf("probe 429 changed gate: %+v", g)
	}
}

func TestWinterGateProbeFailureAdvances(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s"},
	)
	st.Gate = &GateState{Active: true, PausedUntil: clock.Now().Add(10 * time.Minute).UnixMilli(), CanaryOK: true, NextNodeProbe: clock.Now().UnixMilli()}
	at, reason := a.AdmitGateProbe("m")
	if at == nil {
		t.Fatalf("gate probe rejected: %s", reason)
	}
	a.Observe(at, Outcome5xx)
	g := st.Gate
	if !g.Active || g.NodeProbeUsed || g.NextNodeProbe <= clock.Now().UnixMilli() || g.NextCanary == 0 {
		t.Fatalf("probe 5xx did not back off: %+v", g)
	}
}

func Test403LadderQuietWindowResetsStrike(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8)
	a.Observe(normalAttempt("m", "node:e1"), Outcome403)
	a.Observe(normalAttempt("m", "node:e1"), Outcome403)
	p := st.Pairs[PairKey(legacyPairModel, "node:e1")]
	if p == nil || p.Strike != 2 || p.Until != 0 {
		t.Fatalf("first two 403 = %+v", p)
	}
	clock.Advance(9*time.Minute + 59*time.Second)
	a.Observe(normalAttempt("m", "node:e1"), Outcome403)
	p = st.Pairs[PairKey(legacyPairModel, "node:e1")]
	if p.Strike != 3 || p.Until != clock.Now().Add(10*time.Minute).UnixMilli() {
		t.Fatalf("third 403 = %+v", p)
	}

	// A full quiet 10m resets the strike counter; the next 403 is first again.
	clock.Advance(10*time.Minute + time.Millisecond)
	a.Observe(normalAttempt("m", "node:e1"), Outcome403)
	p = st.Pairs[PairKey(legacyPairModel, "node:e1")]
	if p.Strike != 1 || p.Until > clock.Now().UnixMilli() {
		t.Fatalf("post-quiet 403 = %+v", p)
	}
}

func TestQuotaGateShadowAndEnforce(t *testing.T) {
	a, _, st := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s"},
	)
	a.SetQuotaGateEnforce(false)
	for _, e := range []string{"node:e1", "node:e2"} {
		a.Observe(&Attempt{Model: "m", Egress: e, Sub: "s", Nodes: 2}, Outcome429)
	}
	if q := st.Quota["s|m"]; q == nil || q.Until == 0 {
		t.Fatalf("quota not recorded in shadow: %+v", q)
	}
	if _, reason := a.Admit("m", nil); reason != "" {
		t.Fatalf("shadow quota blocked admission: %s", reason)
	}

	a.SetQuotaGateEnforce(true)
	d, reason := a.RetryAfter(RetryRequest{Model: "m"})
	if reason != "quota" || d < 599*time.Second || d > 602*time.Second {
		t.Fatalf("enforced quota retry=%v reason=%q", d, reason)
	}
}

func TestShadowQuotaLogThrottled(t *testing.T) {
	old := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(old)

	a, clock, st := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s"},
		Candidate{Egress: "node:e3", Model: "m", Sub: "s"},
	)
	a.quotaGateEnforce = false
	now := clock.Now().UnixMilli()
	st.Quota[quotaKey("s", "m")] = &QuotaState{Until: now + 600000}
	c := Candidate{Egress: "node:e1", Model: "m", Sub: "s"}
	for i := 0; i < 3; i++ {
		if ok, _ := a.eligibleForTraffic(c, "m", now); !ok {
			t.Fatal("shadow quota must not block")
		}
	}
	if a.shadowQuotaBlocks != 3 {
		t.Fatalf("blocks=%d want 3", a.shadowQuotaBlocks)
	}
	if got := strings.Count(buf.String(), "v5_quota_shadow_blocks"); got != 1 || !strings.Contains(buf.String(), "total=1") {
		t.Fatalf("first aggregate log=%q", buf.String())
	}
	// Within the throttle window only the counter moves.
	a.eligibleForTraffic(c, "m", now+1000)
	if a.shadowQuotaBlocks != 4 || strings.Count(buf.String(), "v5_quota_shadow_blocks") != 1 {
		t.Fatalf("throttled blocks=%d log=%q", a.shadowQuotaBlocks, buf.String())
	}
	// Five seconds later one aggregate line carries the cumulative total.
	a.eligibleForTraffic(c, "m", now+5000)
	if a.shadowQuotaBlocks != 5 || strings.Count(buf.String(), "v5_quota_shadow_blocks") != 2 || !strings.Contains(buf.String(), "total=5") {
		t.Fatalf("second aggregate blocks=%d log=%q", a.shadowQuotaBlocks, buf.String())
	}
	// Enforce mode must not record shadow hits.
	a.quotaGateEnforce = true
	before := a.shadowQuotaBlocks
	if ok, _ := a.eligibleForTraffic(c, "m", now+6000); ok {
		t.Fatal("enforce quota should block")
	}
	if a.shadowQuotaBlocks != before {
		t.Fatalf("enforce moved shadow counter: %d -> %d", before, a.shadowQuotaBlocks)
	}
}

func TestGateEpochStaleInFlight(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 20,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s"},
		Candidate{Egress: "node:e3", Model: "m", Sub: "s"},
	)
	at, reason := a.AdmitPreferred("m", nil, "")
	if at == nil {
		t.Fatalf("admit: %s", reason)
	}
	if at.GateEpoch != st.Gate.Epoch {
		t.Fatalf("attempt epoch=%d gate=%d", at.GateEpoch, st.Gate.Epoch)
	}

	// Simulate a clear while ten attempts are already in flight.
	a.clearGateLocked(clock.Now().UnixMilli())
	for i := 0; i < 10; i++ {
		a.Observe(&Attempt{Model: "m", Egress: "node:e" + string(rune('1'+i%3)), GateCount: true, GateEpoch: 0}, Outcome5xx)
	}
	if st.Gate.Active {
		t.Fatalf("stale in-flight 5xx rebuilt gate: %+v", st.Gate)
	}

	// Attempts stamped with the new epoch can rebuild it.
	for i := 0; i < 10; i++ {
		a.Observe(&Attempt{Model: "m", Egress: "node:e" + string(rune('1'+i%3)), GateCount: true, GateEpoch: st.Gate.Epoch}, Outcome5xx)
	}
	if !st.Gate.Active {
		t.Fatalf("new-epoch 5xx did not rebuild gate: %+v", st.Gate)
	}
	if st.Gate.Epoch < 2 {
		t.Fatalf("trip did not advance epoch: %+v", st.Gate)
	}

	// A stale 2xx from before the trip must not clear the new gate.
	a.Observe(&Attempt{Model: "m", Egress: "node:e1", GateCount: true, GateEpoch: 0}, Outcome2xx)
	if !st.Gate.Active {
		t.Fatal("stale 2xx cleared new gate")
	}
}
