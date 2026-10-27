package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPairLadder(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	key := PairKey("m", "node:e1")
	at := mustAdmit(t, a, "m")
	now := clock.Now()
	a.Observe(at, Outcome429)
	if got := st.Pairs[key]; got.Strikes != 1 || got.Until != now.Add(30*time.Minute).UnixMilli() {
		t.Fatalf("first 429 = %+v", got)
	}

	clock.Advance(30*time.Minute + time.Millisecond)
	at = mustAdmit(t, a, "m")
	a.Observe(at, Outcome2xx)
	if got := st.Pairs[key]; got.Strikes != 1 || got.OKStreak != 1 || got.Until == 0 {
		t.Fatalf("first half-open 2xx = %+v, want strikes=1 okStreak=1", got)
	}
	at = mustAdmit(t, a, "m")
	a.Observe(at, Outcome429)
	if got := st.Pairs[key]; got.Strikes != 2 || got.OKStreak != 0 || got.Until != clock.Now().Add(2*time.Hour).UnixMilli() {
		t.Fatalf("429 during half-open = %+v, want strikes=2 2h", got)
	}

	clock.Advance(2*time.Hour + time.Millisecond)
	at = mustAdmit(t, a, "m")
	a.Observe(at, Outcome2xx)
	at = mustAdmit(t, a, "m")
	a.Observe(at, Outcome2xx)
	if got := st.Pairs[key]; got.Strikes != 0 || got.Until != 0 || got.OKStreak != 0 {
		t.Fatalf("two half-open 2xx = %+v, want cleared", got)
	}
	at = mustAdmit(t, a, "m")
	a.Observe(at, Outcome429)
	if got := st.Pairs[key]; got.Strikes != 1 || got.Until != clock.Now().Add(30*time.Minute).UnixMilli() {
		t.Fatalf("after full recovery 429 = %+v, want first level", got)
	}
}

func TestNoReburnWithin10m(t *testing.T) {
	a, clock, _ := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s1"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s2"},
		Candidate{Egress: "node:e3", Model: "m", Sub: "s3"},
	)
	tripGate(t, a)
	if !a.GatePaused() {
		t.Fatal("gate did not trip after 5 429 / 10 attempts and 3 distinct egress")
	}
	g := a.st.Gate
	until, backoff := g.PausedUntil, g.Backoff
	if backoff != 1 || until == 0 {
		t.Fatalf("first trip = %+v", g)
	}
	// Extra 429s during the active gate must not re-arm or double backoff.
	for i := 0; i < 3; i++ {
		a.Observe(&Attempt{Model: "m", Egress: "node:e1"}, Outcome429)
	}
	if g.PausedUntil != until || g.Backoff != backoff {
		t.Fatalf("reburn within gate: %+v, want until=%d backoff=%d", g, until, backoff)
	}
	clock.Advance(10 * time.Minute)
	if !g.Active {
		t.Fatal("gate auto-cleared without a successful canary")
	}
}

func TestGateTrips(t *testing.T) {
	a, clock, _ := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s1"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s2"},
		Candidate{Egress: "node:e3", Model: "m", Sub: "s3"},
	)
	tripGate(t, a)
	if !a.GatePaused() {
		t.Fatal("gate did not trip")
	}
	if _, reason := a.AdmitCanary("m"); reason != "canary_wait" {
		t.Fatalf("canary before pause = %q, want canary_wait", reason)
	}
	clock.Advance(60 * time.Second)
	canary, reason := a.AdmitCanary("m")
	if canary == nil || !canary.Direct {
		t.Fatalf("canary = %v,%q; want direct", canary, reason)
	}
	a.Observe(canary, Outcome2xx)
	if !a.GateNodeProbeReady() {
		t.Fatal("direct 2xx did not unlock node probe")
	}
	probe, reason := a.AdmitGateProbe("m")
	if probe == nil {
		t.Fatalf("node probe rejected: %s", reason)
	}
	a.Observe(probe, Outcome2xx)
	if a.GatePaused() {
		t.Fatal("node probe 2xx did not clear gate")
	}
}

func TestMaxTwoAttempts(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "force")
	if got := v5AttemptLimit(); got != 2 {
		t.Fatalf("force attempt limit=%d want 2", got)
	}
	t.Setenv("NODE_SYSTEM", "off")
	if got := v5AttemptLimit(); got != 0 {
		t.Fatalf("off attempt limit=%d want 0", got)
	}
}

func TestV5StatePersist(t *testing.T) {
	dir := t.TempDir()
	oldPath := v5StatePath
	v5StatePath = filepath.Join(dir, "v5_state.json")
	t.Cleanup(func() { v5StatePath = oldPath; resetV5ForTest() })
	t.Setenv("NODE_SYSTEM", "force")
	resetV5ForTest()
	v5CandidatesHook = forceTestCandidates("m", Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	rt := v5GetRuntime()
	at, reason := rt.adm.AdmitPreferred("m", nil, "")
	if at == nil {
		t.Fatalf("admit: %s", reason)
	}
	rt.adm.Observe(at, Outcome429)
	rt.markDirty()
	rt.persistNow()
	if _, err := os.Stat(v5StatePath); err != nil {
		t.Fatalf("state file not written: %v", err)
	}

	resetV5ForTest()
	t.Setenv("NODE_SYSTEM", "force")
	rt2 := v5GetRuntime()
	p := rt2.adm.st.Pairs[PairKey("m", "node:e1")]
	if p == nil || p.Strikes != 1 || p.Until == 0 {
		t.Fatalf("reloaded pair = %+v", p)
	}
}

func TestNoCandidateNoDirect(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "force")
	resetV5ForTest()
	t.Cleanup(resetV5ForTest)
	v5CandidatesHook = func(string) []Candidate { return nil }
	eg := NewEgressClient(&http.Client{})
	got := eg.Get(EgressRequest{Model: "m", Context: context.Background()})
	if !got.NoCandidate || got.Direct {
		t.Fatalf("Get=%+v want NoCandidate without direct", got)
	}
}

func tripGate(t *testing.T, a *Admission) {
	t.Helper()
	for i := 0; i < 3; i++ {
		a.Observe(&Attempt{Model: "m", Egress: "node:e" + string(rune('1'+i))}, Outcome429)
	}
	for i := 0; i < 5; i++ {
		a.Observe(&Attempt{Model: "m", Egress: "node:e1"}, Outcome2xx)
	}
	a.Observe(&Attempt{Model: "m", Egress: "node:e1"}, Outcome429)
	a.Observe(&Attempt{Model: "m", Egress: "node:e2"}, Outcome429)
}

func TestGateNeedsFailureRatio(t *testing.T) {
	a, _, _ := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s1"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s2"},
		Candidate{Egress: "node:e3", Model: "m", Sub: "s3"},
	)
	// 3 distinct 429 but only 3% failure across 100 attempts: must not trip.
	for i := 0; i < 100; i++ {
		if i < 3 {
			a.Observe(&Attempt{Model: "m", Egress: "node:e" + string(rune('1'+i))}, Outcome429)
		} else {
			a.Observe(&Attempt{Model: "m", Egress: "node:e1"}, Outcome2xx)
		}
	}
	if a.GatePaused() {
		t.Fatal("gate tripped on 3/100 failures")
	}

	b, _, _ := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s1"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s2"},
		Candidate{Egress: "node:e3", Model: "m", Sub: "s3"},
	)
	// 30 failures / 50 attempts: must trip once 3 distinct egress are seen.
	for i := 0; i < 50; i++ {
		if i < 30 {
			b.Observe(&Attempt{Model: "m", Egress: "node:e" + string(rune('1'+i%3))}, Outcome429)
		} else {
			b.Observe(&Attempt{Model: "m", Egress: "node:e1"}, Outcome2xx)
		}
	}
	if !b.GatePaused() {
		t.Fatal("gate did not trip on 30/50 failures")
	}
}

func TestGateEscapeAfter10m(t *testing.T) {
	a, clock, _ := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s1"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s2"},
		Candidate{Egress: "node:e3", Model: "m", Sub: "s3"},
	)
	tripGate(t, a)
	if !a.GatePaused() {
		t.Fatal("gate did not trip")
	}
	clock.Advance(10 * time.Minute)
	if !a.GateNodeProbeReady() {
		t.Fatal("node probe not ready after 10m time escape")
	}
	probe, reason := a.AdmitGateProbe("m")
	if probe == nil {
		t.Fatalf("time-escape probe rejected: %s", reason)
	}
	a.Observe(probe, Outcome2xx)
	if a.GatePaused() {
		t.Fatal("successful time-escape probe did not clear gate")
	}
}

func TestGateProbeFailureDoesNotFreeze(t *testing.T) {
	a, clock, _ := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s1"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s2"},
		Candidate{Egress: "node:e3", Model: "m", Sub: "s3"},
	)
	tripGate(t, a)
	// Use the canary-authorized node probe: this is the path where a failed
	// probe used to leave CanaryOK=true and NodeProbeUsed=true, closing both
	// entrances forever.
	clock.Advance(60 * time.Second)
	canary, reason := a.AdmitCanary("m")
	if canary == nil {
		t.Fatalf("canary rejected: %s", reason)
	}
	a.Observe(canary, Outcome2xx)
	probe, reason := a.AdmitGateProbe("m")
	if probe == nil {
		t.Fatalf("probe rejected: %s", reason)
	}
	a.Observe(probe, Outcome403)
	if a.GateNodeProbeReady() {
		t.Fatal("failed probe left node probe ready")
	}
	clock.Advance(60 * time.Second)
	if !a.GateCanaryDue() {
		t.Fatal("failed probe froze canary entrance")
	}
	canary, reason = a.AdmitCanary("m")
	if canary == nil {
		t.Fatalf("canary after failed probe rejected: %s", reason)
	}
	a.Observe(canary, Outcome2xx)
	if !a.GateNodeProbeReady() {
		t.Fatal("canary 2xx did not reopen node probe after failed probe")
	}
}

func TestWinterLogThrottled(t *testing.T) {
	resetV5ForTest()
	ctx := beginCallLog(context.Background(), "/v1/chat/completions", "m", false, "test")
	v5Winter(ctx, "m", "gate_paused")
	v5Winter(ctx, "m", "gate_paused")
	cr := callRecorderFrom(ctx)
	cr.mu.Lock()
	n := 0
	for _, ev := range cr.rec.Events {
		if ev.Type == "v5_winter" {
			n++
		}
	}
	cr.mu.Unlock()
	if n != 1 {
		t.Fatalf("v5_winter events=%d want 1", n)
	}
}

func TestGateThresholdOnTenthAttempt(t *testing.T) {
	a, _, _ := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s1"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s2"},
		Candidate{Egress: "node:e3", Model: "m", Sub: "s3"},
	)
	for i := 0; i < 5; i++ {
		a.Observe(&Attempt{Model: "m", Egress: "node:e" + string(rune('1'+i%3))}, Outcome429)
	}
	for i := 0; i < 4; i++ {
		a.Observe(&Attempt{Model: "m", Egress: "node:e1"}, Outcome2xx)
	}
	if a.GatePaused() {
		t.Fatal("gate tripped before the 10th attempt")
	}
	a.Observe(&Attempt{Model: "m", Egress: "node:e1"}, Outcome2xx)
	if !a.GatePaused() {
		t.Fatal("10th attempt (2xx) did not evaluate the 5/10 boundary")
	}
}

func TestGateNeedsTenAttempts(t *testing.T) {
	a, _, _ := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s1"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s2"},
		Candidate{Egress: "node:e3", Model: "m", Sub: "s3"},
	)
	for i := 0; i < 5; i++ {
		a.Observe(&Attempt{Model: "m", Egress: "node:e" + string(rune('1'+i%3))}, Outcome429)
	}
	for i := 0; i < 4; i++ {
		a.Observe(&Attempt{Model: "m", Egress: "node:e1"}, Outcome2xx)
	}
	if a.GatePaused() {
		t.Fatal("gate tripped on 9 attempts")
	}
}

func TestGateBelowFiftyPercent(t *testing.T) {
	a, _, _ := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s1"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s2"},
		Candidate{Egress: "node:e3", Model: "m", Sub: "s3"},
	)
	for i := 0; i < 49; i++ {
		class := Outcome2xx
		if i >= 25 {
			class = Outcome429
		}
		a.Observe(&Attempt{Model: "m", Egress: "node:e" + string(rune('1'+i%3))}, class)
	}
	if a.GatePaused() {
		t.Fatal("gate tripped below 50%")
	}
}

func TestGateEmptyEgressNotCounted(t *testing.T) {
	a, _, st := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s1"},
		Candidate{Egress: "node:e2", Model: "m", Sub: "s2"},
		Candidate{Egress: "node:e3", Model: "m", Sub: "s3"},
	)
	for i := 0; i < 5; i++ {
		a.Observe(&Attempt{Model: "m", Egress: "node:e" + string(rune('1'+i%3))}, Outcome429)
	}
	for i := 0; i < 5; i++ {
		a.Observe(&Attempt{Model: "m", Egress: ""}, Outcome429)
	}
	if st.Gate.Attempts != 5 || st.Gate.Failures != 5 || len(st.Gate.Egresses) != 3 {
		t.Fatalf("window=%+v want attempts=5 failures=5 egresses=3", st.Gate)
	}
	if a.GatePaused() {
		t.Fatal("empty egress attempts must not trip the gate")
	}
}

func TestV5CandidatesColdStart(t *testing.T) {
	p := newProxyPool(filepath.Join(t.TempDir(), "proxy_state.json"))
	p.setNodes([]*ProxyNode{
		{Name: "n1", Protocol: "socks5", Address: "10.0.0.1", Port: 1080, Region: "jp"},
		{Name: "n2", Protocol: "socks5", Address: "10.0.0.2", Port: 1080, Region: "us"},
		{Name: "n3", Protocol: "socks5", Address: "10.0.0.3", Port: 1080, Region: "hk"},
	})
	old := proxyPool
	proxyPool = p
	t.Cleanup(func() { proxyPool = old })
	v5CandidatesHook = nil

	got := v5Candidates("m")
	nodes, direct := 0, 0
	for _, c := range got {
		if c.Direct {
			direct++
		} else if c.Egress != "" {
			nodes++
		}
	}
	if nodes != 3 || direct != 1 {
		t.Fatalf("cold candidates=%+v want 3 nodes + direct", got)
	}
}

func TestV5CandidateSurvivesLegacyClientDelete(t *testing.T) {
	p := newProxyPool(filepath.Join(t.TempDir(), "proxy_state.json"))
	p.setNodes([]*ProxyNode{{Name: "n1", Protocol: "socks5", Address: "10.0.0.1", Port: 1080, Region: "jp"}})
	n := p.snapshot()[0]
	old := proxyPool
	proxyPool = p
	t.Cleanup(func() { proxyPool = old })
	v5CandidatesHook = nil
	if got := v5Candidates("m"); len(got) != 2 {
		t.Fatalf("initial candidates=%+v", got)
	}
	p.mu.Lock()
	delete(p.clients, n.Fingerprint)
	p.mu.Unlock()
	got := v5Candidates("m")
	if len(got) != 2 || got[0].Egress != nodeEgressKey(n.Fingerprint) {
		t.Fatalf("after legacy client delete candidates=%+v", got)
	}
}

func TestForceTransportErrorDoesNotMarkLegacyState(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "force")
	p := newProxyPool(filepath.Join(t.TempDir(), "proxy_state.json"))
	p.setNodes([]*ProxyNode{{Name: "n1", Protocol: "socks5", Address: "10.0.0.1", Port: 1080}})
	n := p.snapshot()[0]
	old := proxyPool
	proxyPool = p
	t.Cleanup(func() { proxyPool = old })
	noteAttemptTransportError(context.Background(), p, n.Fingerprint, errors.New("eof"))
	if got := p.snapshot()[0].State; got != NodeAvailable {
		t.Fatalf("force transport error changed legacy state to %v", got)
	}
}

func TestEmptyEgressDoesNotTouchGateOrPairs(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 8,
		Candidate{Egress: "node:e1", Model: "m", Sub: "s1"},
	)
	for i := 0; i < 3; i++ {
		a.Observe(&Attempt{Model: "m", Egress: ""}, Outcome429)
	}
	if st.Gate.WindowStart != 0 || st.Gate.Attempts != 0 || st.Gate.Failures != 0 || len(st.Gate.Egresses) != 0 {
		t.Fatalf("empty egress touched window: %+v", st.Gate)
	}
	if _, ok := st.Pairs[PairKey("m", "")]; ok {
		t.Fatal("empty egress created model pair")
	}
	if _, ok := st.Pairs[PairKey("*", "")]; ok {
		t.Fatal("empty egress created wildcard pair")
	}

	a.Observe(&Attempt{Model: "m", Egress: "node:e1"}, Outcome429)
	start, attempts, failures := st.Gate.WindowStart, st.Gate.Attempts, st.Gate.Failures
	clock.Advance(time.Second)
	a.Observe(&Attempt{Model: "m", Egress: ""}, Outcome429)
	if st.Gate.WindowStart != start || st.Gate.Attempts != attempts || st.Gate.Failures != failures {
		t.Fatalf("empty egress moved window: %+v", st.Gate)
	}
	if _, ok := st.Pairs[PairKey("m", "")]; ok {
		t.Fatal("empty egress created pair after real attempt")
	}
}

func TestForceNote403DoesNotHaltLegacyPool(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "force")
	p := newProxyPool(filepath.Join(t.TempDir(), "proxy_state.json"))
	nodes := make([]*ProxyNode, 0, 8)
	for i := 0; i < 8; i++ {
		nodes = append(nodes, &ProxyNode{Name: "n" + string(rune('0'+i)), Protocol: "socks5", Address: "10.0.0." + string(rune('1'+i)), Port: 1080})
	}
	p.setNodes(nodes)
	now := time.Now()
	before := p.snapshot()
	for _, n := range before {
		p.noteAttempt(n.Fingerprint, now)
		p.note403(n.Fingerprint, now)
	}
	if p.poolHalted(now) {
		t.Fatal("force note403 halted legacy pool")
	}
	t.Setenv("NODE_SYSTEM", "off")
	after := p.snapshot()
	if len(before) != len(after) {
		t.Fatalf("node count changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i].State != after[i].State {
			t.Fatalf("legacy node %s changed %v -> %v", before[i].Fingerprint, before[i].State, after[i].State)
		}
	}
}

func TestForceIgnoresLegacyDead(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "off")
	p := newProxyPool(filepath.Join(t.TempDir(), "proxy_state.json"))
	p.setNodes([]*ProxyNode{{Name: "n1", Protocol: "socks5", Address: "10.0.0.1", Port: 1080}})
	n := p.snapshot()[0]
	if !p.mark(n.Fingerprint, NodeDead, "legacy_dead") {
		t.Fatal("failed to mark legacy dead")
	}
	p.saveWG.Wait()
	p.mu.Lock()
	delete(p.clients, n.Fingerprint)
	p.mu.Unlock()
	t.Setenv("NODE_SYSTEM", "force")
	oldPool := proxyPool
	proxyPool = p
	t.Cleanup(func() { proxyPool = oldPool })
	v5CandidatesHook = nil

	got := v5Candidates("m")
	found := false
	for _, c := range got {
		if c.Egress == nodeEgressKey(n.Fingerprint) {
			found = true
		}
	}
	if !found {
		t.Fatalf("legacy dead node missing from v5 candidates: %+v", got)
	}
}

func TestForceLegacyWritesNoop(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "force")
	resetV5ForTest()
	p := newProxyPool(filepath.Join(t.TempDir(), "proxy_state.json"))
	n1 := &ProxyNode{Name: "subA::n1", Protocol: "socks5", Address: "127.0.0.1", Port: 1080}
	n2 := &ProxyNode{Name: "subA::n2", Protocol: "socks5", Address: "127.0.0.1", Port: 1081}
	p.setNodes([]*ProxyNode{n1, n2})
	p.mu.Lock()
	fp1, fp2 := n1.Fingerprint, n2.Fingerprint
	beforeNodes := make(map[string]ProxyNode, len(p.nodes))
	for k, n := range p.byID {
		beforeNodes[k] = *n
	}
	beforeSubs := make(map[string]sub403State, len(p.sub403))
	for k, v := range p.sub403 {
		beforeSubs[k] = *v
	}
	p.mu.Unlock()
	beforeHalt := p.poolHalted(time.Now())

	if p.mark(fp1, NodeExhausted, "quota") {
		t.Fatal("force mark must be no-op")
	}
	if p.markFreeTierRejected(fp2) {
		t.Fatal("force markFreeTierRejected must be no-op")
	}
	p.quarantineSub("subA", time.Now())
	p.confirmSub403(fp1, time.Now())
	p.saveWG.Wait()

	p.mu.Lock()
	for k, want := range beforeNodes {
		if got := p.byID[k]; got == nil || *got != want {
			p.mu.Unlock()
			t.Fatalf("node %s changed under force: got=%+v want=%+v", k, got, want)
		}
	}
	if len(p.sub403) != len(beforeSubs) {
		p.mu.Unlock()
		t.Fatalf("sub403 changed under force: got=%d want=%d", len(p.sub403), len(beforeSubs))
	}
	for k, want := range beforeSubs {
		if got := p.sub403[k]; got == nil || *got != want {
			p.mu.Unlock()
			t.Fatalf("sub %s changed under force: got=%+v want=%+v", k, got, want)
		}
	}
	p.mu.Unlock()
	if p.poolHalted(time.Now()) != beforeHalt {
		t.Fatal("poolHalted changed under force")
	}
}

func TestLegacyWritesOffStillApply(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "off")
	resetV5ForTest()
	p := newProxyPool(filepath.Join(t.TempDir(), "proxy_state.json"))
	n := &ProxyNode{Name: "sub::n", Protocol: "socks5", Address: "127.0.0.1", Port: 1080}
	p.setNodes([]*ProxyNode{n})
	p.mu.Lock()
	fp := n.Fingerprint
	p.mu.Unlock()
	if !p.mark(fp, NodeExhausted, "quota") {
		t.Fatal("off mark must still apply")
	}
	p.saveWG.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byID[fp].State != NodeExhausted {
		t.Fatalf("off state=%s, want exhausted", p.byID[fp].State)
	}
}

func TestProbeKeyReleasesMismatchedEgress(t *testing.T) {
	a, _, _ := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m"})
	at, reason := a.AdmitProbe("m")
	if at == nil {
		t.Fatalf("AdmitProbe: %s", reason)
	}
	at.Egress = "node:rewritten"
	a.Observe(at, Outcome2xx)
	a.mu.Lock()
	if len(a.probeActive) != 0 || a.probing {
		a.mu.Unlock()
		t.Fatalf("probe residue after rewritten egress: active=%v probing=%v", a.probeActive, a.probing)
	}
	a.mu.Unlock()

	at2, reason := a.AdmitProbe("m")
	if at2 == nil {
		t.Fatalf("second AdmitProbe: %s", reason)
	}
	a.Observe(at2, Outcome2xx)
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.probeActive) != 0 || a.probing {
		t.Fatalf("probe residue after empty egress: active=%v probing=%v", a.probeActive, a.probing)
	}
}

func TestAdmitProbeMissNoResidue(t *testing.T) {
	a, _, _ := newAdmissionTest(t, 8)
	if at, _ := a.AdmitProbe("m"); at != nil {
		t.Fatal("expected miss")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.probeActive) != 0 || a.probing {
		t.Fatalf("miss residue: active=%v probing=%v", a.probeActive, a.probing)
	}
}

func TestProbeObserveReleasesOnce(t *testing.T) {
	a, _, _ := newAdmissionTest(t, 8, Candidate{Egress: "node:e1", Model: "m"})
	at, reason := a.AdmitProbe("m")
	if at == nil {
		t.Fatalf("AdmitProbe: %s", reason)
	}
	a.Observe(at, Outcome2xx)
	a.Observe(at, Outcome2xx)
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.probeActive) != 0 || a.probing || len(a.sems["node:e1"]) != 0 {
		t.Fatalf("double observe residue: active=%v probing=%v sem=%d", a.probeActive, a.probing, len(a.sems["node:e1"]))
	}
}
