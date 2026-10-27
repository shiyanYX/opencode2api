package main

import (
	"sort"
	"sync"
	"time"
)

type OutcomeClass string

const (
	Outcome2xx     OutcomeClass = "2xx"
	Outcome429     OutcomeClass = "429"
	Outcome403     OutcomeClass = "403"
	OutcomeNetwork OutcomeClass = "network"
	OutcomeTimeout OutcomeClass = "timeout"
	Outcome5xx     OutcomeClass = "5xx"
	OutcomeCancel  OutcomeClass = "cancel"
)

type Candidate struct {
	Egress string
	Sub    string
	Model  string
	Region string // 节点区域；direct 为空
	Direct bool
}

type Attempt struct {
	Model  string
	Egress string
	Direct bool
	// gateProbe 标记这是 winter gate 的节点探测；任何非 2xx 都必须重置探测状态。
	gateProbe bool

	release func()
	once    sync.Once

	sub       string
	probe     bool
	lifecycle bool
	explore   bool
}

type Admission struct {
	st         *NodeStateV5
	K          int
	candidates func(string) []Candidate
	clock      Clock

	mu              sync.Mutex
	sems            map[string]chan struct{}
	probeActive     map[string]bool
	probing         bool
	probeBackoff    time.Duration
	probeTimeout    time.Duration
	lastPrune       int64
	rr              int
	exploreInflight map[string]int
}

var pairCooldownLadder = [...]time.Duration{
	30 * time.Minute,
	2 * time.Hour,
	8 * time.Hour,
	24 * time.Hour,
	72 * time.Hour,
}

var probeBackoffLadder = [...]time.Duration{
	60 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
	6 * time.Hour,
}

func NewAdmission(st *NodeStateV5, k int, candidates func(string) []Candidate) *Admission {
	if st == nil {
		st = emptyNodeStateV5()
	}
	if k <= 0 {
		k = 8
	}
	if st.Pairs == nil {
		st.Pairs = map[string]*PairState{}
	}
	if st.Egress == nil {
		st.Egress = map[string]*EgressState{}
	}
	if st.Direct == nil {
		st.Direct = &DirectState{}
	}
	if st.Subs == nil {
		st.Subs = map[string]*SubState{}
	}
	return &Admission{
		st:              st,
		K:               k,
		candidates:      candidates,
		sems:            map[string]chan struct{}{},
		probeActive:     map[string]bool{},
		probeBackoff:    probeBackoffLadder[0],
		probeTimeout:    20 * time.Second,
		exploreInflight: map[string]int{},
	}
}

func (a *Admission) now() time.Time {
	if a.clock != nil {
		return a.clock.Now()
	}
	return Now()
}

func (a *Admission) Admit(model string, exclude map[string]bool) (*Attempt, string) {
	return a.AdmitPreferred(model, exclude, "")
}

func (a *Admission) AdmitPreferred(model string, exclude map[string]bool, prefer string) (*Attempt, string) {
	now := a.now()
	nowMS := now.UnixMilli()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.maybePruneLocked(now)

	if a.candidates == nil {
		return nil, "no_candidate"
	}
	cands := a.candidates(model)
	if len(cands) == 0 {
		return nil, "no_candidate"
	}

	eligible := make([]Candidate, 0, len(cands))
	onlySubBlocked := true
	for _, c := range cands {
		if c.Egress == "" {
			onlySubBlocked = false
			continue
		}
		if exclude != nil && exclude[c.Egress] {
			onlySubBlocked = false
			continue
		}
		ok, subBlocked := a.eligibleLocked(c, model, nowMS, false)
		if !ok {
			if !subBlocked {
				onlySubBlocked = false
			}
			continue
		}
		if p := a.pairLocked(model, c.Egress); p != nil {
			a.decayPairLocked(p, nowMS)
		}
		eligible = append(eligible, c)
	}
	if len(eligible) == 0 {
		if onlySubBlocked {
			return nil, "sub_quarantine"
		}
		return nil, "no_candidate"
	}

	if prefer != "" {
		for _, c := range eligible {
			if c.Egress != prefer {
				continue
			}
			n := a.pairNLocked(model, c.Egress)
			if n < pairExploreCutoff && a.exploreInflight[c.Egress] > 0 {
				break
			}
			at, ok := a.acquireLocked(c, model, n < pairExploreCutoff)
			if !ok {
				return nil, "all_inflight"
			}
			return at, ""
		}
	}

	a.sortCandidatesLocked(eligible, model)

	for _, c := range eligible {
		if a.pairNLocked(model, c.Egress) >= pairExploreCutoff {
			continue
		}
		if a.exploreInflight[c.Egress] > 0 {
			continue
		}
		at, ok := a.acquireLocked(c, model, true)
		if ok {
			return at, ""
		}
	}
	for _, c := range eligible {
		if a.pairNLocked(model, c.Egress) < pairExploreCutoff {
			continue
		}
		at, ok := a.acquireLocked(c, model, false)
		if ok {
			return at, ""
		}
	}
	return nil, "all_inflight"
}

func (a *Admission) acquireLocked(c Candidate, model string, explore bool) (*Attempt, bool) {
	sem := a.semLocked(c.Egress)
	select {
	case sem <- struct{}{}:
		if explore {
			a.exploreInflight[c.Egress]++
		}
		return a.attemptLocked(c, model, false, false, explore, sem), true
	default:
		return nil, false
	}
}

func (a *Admission) AdmitProbe(model string) (*Attempt, string) {
	now := a.now()
	nowMS := now.UnixMilli()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.maybePruneLocked(now)
	if a.probing {
		return nil, "probe_busy"
	}

	if a.candidates == nil {
		return nil, "no_candidate"
	}
	cands := a.candidates(model)
	eligible := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if c.Egress == "" {
			continue
		}
		ok, _ := a.eligibleLocked(c, model, nowMS, true)
		if ok {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		return nil, "no_candidate"
	}
	a.sortCandidatesLocked(eligible, model)
	for _, c := range eligible {
		if a.probeActive[c.Egress] {
			continue
		}
		sem := a.semLocked(c.Egress)
		select {
		case sem <- struct{}{}:
			a.probeActive[c.Egress] = true
			a.probing = true
			at := a.attemptLocked(c, model, true, false, false, sem)
			a.startProbeTimerLocked(at)
			return at, ""
		default:
		}
	}
	return nil, "all_inflight"
}

func (a *Admission) AdmitLifecycleProbe() (*Attempt, string) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.maybePruneLocked(now)
	if a.probing {
		return nil, "probe_busy"
	}

	keys := make([]string, 0, len(a.st.Egress))
	for egress := range a.st.Egress {
		keys = append(keys, egress)
	}
	sort.Strings(keys)
	for _, egress := range keys {
		e := a.st.Egress[egress]
		if egress == "direct" || e == nil || e.Lifecycle != "dead" || a.probeActive[egress] {
			continue
		}
		sem := a.semLocked(egress)
		select {
		case sem <- struct{}{}:
			a.probeActive[egress] = true
			a.probing = true
			at := &Attempt{
				Egress:    egress,
				Direct:    egress == "direct",
				release:   func() { <-sem },
				probe:     true,
				lifecycle: true,
			}
			a.startProbeTimerLocked(at)
			return at, ""
		default:
		}
	}
	return nil, "no_candidate"
}

func (a *Admission) Observe(at *Attempt, class OutcomeClass) {
	if at == nil {
		return
	}
	observed := false
	at.once.Do(func() {
		observed = true
		if at.release != nil {
			at.release()
		}
	})
	if !observed {
		return
	}

	now := a.now()
	nowMS := now.UnixMilli()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.maybePruneLocked(now)
	if at.explore {
		if n := a.exploreInflight[at.Egress]; n > 0 {
			a.exploreInflight[at.Egress] = n - 1
		}
	}
	if at.probe {
		delete(a.probeActive, at.Egress)
		a.probing = false
	}

	if at.lifecycle {
		a.observeLifecycleLocked(at, class, now, nowMS)
		return
	}
	a.observeGateOutcomeLocked(at, class, now, nowMS)

	switch class {
	case Outcome2xx:
		p := a.writePairLocked(at.Model, at.Egress)
		a.decayPairLocked(p, nowMS)
		p.A++
		p.LastOK = nowMS
		// Half-open: after a cooldown expires, the pair returns at 0.05 weight
		// and needs two consecutive 2xx before it is fully trusted again.
		// A 2xx that arrives while the pair is still cooling (an older lease)
		// must not shorten the cooldown.
		if p.Strikes > 0 && p.Until > 0 {
			if p.Until <= nowMS {
				p.OKStreak++
				if p.OKStreak >= 2 {
					p.Strikes = 0
					p.Hard = 0
					p.Until = 0
					p.OKStreak = 0
				}
			}
		} else {
			p.Strikes = 0
			p.Hard = 0
			p.Until = 0
			p.OKStreak = 0
		}
		if at.probe {
			a.clearSubLocked(at.sub)
			a.probeBackoff = probeBackoffLadder[0]
		}
		a.egressConnectedLocked(at.Egress)
		if at.Direct && a.st.Direct != nil {
			a.st.Direct.Until = 0
		}
	case Outcome429:
		a.egressConnectedLocked(at.Egress)
		p := a.writePairLocked(at.Model, at.Egress)
		a.decayPairLocked(p, nowMS)
		p.B++
		p.LastFail = nowMS
		p.OKStreak = 0
		if at.probe {
			minUntil := nowMS + a.probeBackoff.Milliseconds()
			if p.Until < minUntil {
				p.Until = minUntil
			}
			a.advanceProbeBackoffLocked()
		} else {
			p.Strikes++
			if p.Strikes > len(pairCooldownLadder) {
				p.Strikes = len(pairCooldownLadder)
			}
			p.Until = now.Add(pairCooldownLadder[p.Strikes-1]).UnixMilli()
			p.Trips24h = append(p.Trips24h, nowMS)
			p.Trips24h = pruneTrips(p.Trips24h, now.Add(-24*time.Hour).UnixMilli())
		}
		if at.Direct {
			if a.st.Direct == nil {
				a.st.Direct = &DirectState{}
			}
			a.st.Direct.Until = now.Add(60 * time.Second).UnixMilli()
		}
	case Outcome403:
		a.egressConnectedLocked(at.Egress)
		a.observe403Locked(at, now, nowMS)
	case Outcome5xx:
		a.egressConnectedLocked(at.Egress)
	case OutcomeNetwork, OutcomeTimeout:
		a.egressFailureLocked(at.Egress, now)
	}
}

func (a *Admission) RetryAfter(model string) time.Duration {
	now := a.now()
	nowMS := now.UnixMilli()
	a.mu.Lock()
	defer a.mu.Unlock()

	var minUntil int64
	consider := func(v int64) {
		if v > nowMS && (minUntil == 0 || v < minUntil) {
			minUntil = v
		}
	}
	if a.candidates != nil {
		for _, c := range a.candidates(model) {
			if c.Egress == "" {
				continue
			}
			if p := a.pairLocked(model, c.Egress); p != nil {
				consider(p.Until)
			}
			if c.Sub != "" {
				if s := a.st.Subs[c.Sub]; s != nil {
					consider(s.Until)
				}
			}
			if e := a.st.Egress[c.Egress]; e != nil {
				consider(e.DeadUntil)
			}
			if c.Direct || c.Egress == "direct" {
				if a.st.Direct != nil {
					consider(a.st.Direct.Until)
				}
			}
		}
	}
	if minUntil == 0 {
		return time.Second
	}
	d := time.Duration(minUntil-nowMS) * time.Millisecond
	if d < time.Second {
		return time.Second
	}
	return d
}

func (a *Admission) attemptLocked(c Candidate, model string, probe, lifecycle, explore bool, sem chan struct{}) *Attempt {
	return &Attempt{
		Model:     model,
		Egress:    c.Egress,
		Direct:    c.Direct || c.Egress == "direct",
		release:   func() { <-sem },
		sub:       c.Sub,
		probe:     probe,
		lifecycle: lifecycle,
		explore:   explore,
	}
}

func (a *Admission) startProbeTimerLocked(at *Attempt) {
	if a.probeTimeout <= 0 {
		return
	}
	time.AfterFunc(a.probeTimeout, func() {
		a.Observe(at, OutcomeTimeout)
	})
}

func (a *Admission) eligibleLocked(c Candidate, model string, nowMS int64, probe bool) (bool, bool) {
	if c.Direct || c.Egress == "direct" {
		if a.st.Direct != nil && a.st.Direct.Until > nowMS {
			return false, false
		}
	}
	if !(c.Direct || c.Egress == "direct") {
		if e := a.st.Egress[c.Egress]; e != nil {
			if e.Lifecycle == "dead" {
				if e.DeadUntil > nowMS {
					return false, false
				}
				e.Lifecycle = "probation"
				e.ConsecOK = 0
				e.DeadUntil = 0
			}
		}
	}
	if !probe {
		if p := a.pairLocked(model, c.Egress); p != nil && p.Until > nowMS {
			return false, false
		}
	}
	if c.Sub != "" && !probe {
		if s := a.st.Subs[c.Sub]; s != nil && s.Until > nowMS {
			return false, true
		}
	}
	return true, false
}

func (a *Admission) pairLocked(model, egress string) *PairState {
	if p := a.st.Pairs[PairKey(model, egress)]; p != nil {
		return p
	}
	return a.st.Pairs[PairKey("*", egress)]
}

func (a *Admission) writePairLocked(model, egress string) *PairState {
	key := PairKey(model, egress)
	p := a.st.Pairs[key]
	if p == nil {
		p = &PairState{}
		a.st.Pairs[key] = p
	}
	if model != "*" {
		delete(a.st.Pairs, PairKey("*", egress))
	}
	return p
}

func (a *Admission) semLocked(egress string) chan struct{} {
	sem := a.sems[egress]
	if sem == nil {
		sem = make(chan struct{}, a.K)
		a.sems[egress] = sem
	}
	return sem
}

func (a *Admission) maybePruneLocked(now time.Time) {
	nowMS := now.UnixMilli()
	if a.lastPrune != 0 && nowMS-a.lastPrune < time.Minute.Milliseconds() {
		return
	}
	a.st.pruneLocked(now)
	a.lastPrune = nowMS
}

func (a *Admission) egressConnectedLocked(egress string) {
	if egress == "" || egress == "direct" {
		return
	}
	e := a.egressLocked(egress)
	e.ConsecFail = 0
	switch e.Lifecycle {
	case "dead":
		e.Lifecycle = "probation"
		e.ConsecOK = 1
		e.DeadUntil = 0
	case "probation":
		e.ConsecOK++
		if e.ConsecOK >= 3 {
			e.Lifecycle = "active"
			e.ConsecOK = 0
		}
	case "active", "":
		e.Lifecycle = "active"
		e.ConsecOK = 0
	}
}

func (a *Admission) egressFailureLocked(egress string, now time.Time) {
	if egress == "" || egress == "direct" {
		return
	}
	e := a.egressLocked(egress)
	e.ConsecFail++
	e.ConsecOK = 0
	if e.ConsecFail >= 5 {
		e.Lifecycle = "dead"
		e.DeadUntil = now.Add(30 * time.Minute).UnixMilli()
		e.ConsecFail = 0
	}
}

func (a *Admission) egressLocked(egress string) *EgressState {
	if a.st.Egress == nil {
		a.st.Egress = map[string]*EgressState{}
	}
	e := a.st.Egress[egress]
	if e == nil {
		e = &EgressState{Lifecycle: "active"}
		a.st.Egress[egress] = e
	}
	return e
}

func (a *Admission) clearSubLocked(sub string) {
	if sub == "" {
		return
	}
	s := a.st.Subs[sub]
	if s == nil {
		return
	}
	s.Until = 0
	s.Reason = ""
	s.N403 = 0
	s.Last403 = 0
	s.Pairs = nil
	s.PairTimes = nil
}

func (a *Admission) observe403Locked(at *Attempt, now time.Time, nowMS int64) {
	if at.sub == "" {
		return
	}
	if a.st.Subs == nil {
		a.st.Subs = map[string]*SubState{}
	}
	s := a.st.Subs[at.sub]
	if s == nil {
		s = &SubState{}
		a.st.Subs[at.sub] = s
	}
	s.N403++
	s.Last403 = nowMS
	if len(s.PairTimes) != len(s.Pairs) {
		s.Pairs = nil
		s.PairTimes = nil
	}
	key := PairKey(at.Model, at.Egress)
	s.Pairs = append(s.Pairs, key)
	s.PairTimes = append(s.PairTimes, nowMS)
	cutoff := nowMS - (10 * time.Minute).Milliseconds()
	kept := 0
	for i, p := range s.Pairs {
		if s.PairTimes[i] < cutoff {
			continue
		}
		s.Pairs[kept] = p
		s.PairTimes[kept] = s.PairTimes[i]
		kept++
	}
	s.Pairs = s.Pairs[:kept]
	s.PairTimes = s.PairTimes[:kept]
	if len(s.Pairs) > 32 {
		drop := len(s.Pairs) - 32
		s.Pairs = s.Pairs[drop:]
		s.PairTimes = s.PairTimes[drop:]
	}
	counts := map[string]int{}
	for _, p := range s.Pairs {
		counts[p]++
	}
	quarantine := len(counts) >= 2
	for _, n := range counts {
		if n >= 2 {
			quarantine = true
		}
	}
	if quarantine || at.probe {
		s.Until = now.Add(10 * time.Minute).UnixMilli()
		s.Reason = "403"
	}
}

func (a *Admission) observeLifecycleLocked(at *Attempt, class OutcomeClass, now time.Time, nowMS int64) {
	if at.Egress == "direct" {
		return
	}
	e := a.egressLocked(at.Egress)
	switch class {
	case Outcome2xx:
		a.egressConnectedLocked(at.Egress)
		a.probeBackoff = probeBackoffLadder[0]
	case Outcome429:
		a.egressConnectedLocked(at.Egress)
		a.advanceProbeBackoffLocked()
	case Outcome403:
		a.egressConnectedLocked(at.Egress)
		a.observe403Locked(at, now, nowMS)
	case Outcome5xx:
		a.egressConnectedLocked(at.Egress)
	case OutcomeNetwork, OutcomeTimeout:
		e.Lifecycle = "dead"
		e.DeadUntil = now.Add(30 * time.Minute).UnixMilli()
		e.ConsecFail = 0
		e.ConsecOK = 0
	}
}

func (a *Admission) advanceProbeBackoffLocked() {
	for i, d := range probeBackoffLadder {
		if a.probeBackoff == d {
			if i+1 < len(probeBackoffLadder) {
				a.probeBackoff = probeBackoffLadder[i+1]
			}
			return
		}
	}
	a.probeBackoff = probeBackoffLadder[len(probeBackoffLadder)-1]
}

const (
	gateWindow       = 5 * time.Minute
	gateDistinct     = 3
	gateQuiet        = 10 * time.Minute
	gateBasePause    = 60 * time.Second
	gateMaxPause     = 15 * time.Minute
	gateCanaryPeriod = 60 * time.Second

	gateMinAttempts  = 10
	gateEscapeDelay  = 10 * time.Minute
	gateEscapePeriod = 5 * time.Minute
)

func (a *Admission) gateLocked(now time.Time) *GateState {
	if a.st.Gate == nil {
		a.st.Gate = &GateState{Egresses: map[string]int64{}}
	}
	if a.st.Gate.Egresses == nil {
		a.st.Gate.Egresses = map[string]int64{}
	}
	// States written before v5.8 have no NextNodeProbe. An active old gate
	// must still get the 10-minute time escape instead of staying forever.
	if a.st.Gate.Active && a.st.Gate.NextNodeProbe == 0 {
		base := a.st.Gate.LastTrip
		if base == 0 {
			base = now.UnixMilli()
		}
		a.st.Gate.NextNodeProbe = base + gateEscapeDelay.Milliseconds()
	}
	return a.st.Gate
}

// GatePaused reports whether normal node traffic must be turned into 429.
func (a *Admission) GatePaused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.gateLocked(a.now())
	return g.Active
}

// GateRetryAfter is the fixed client-facing retry interval while the gate is active.
func (a *Admission) GateRetryAfter() time.Duration { return gateCanaryPeriod }

func (a *Admission) observeGateOutcomeLocked(at *Attempt, class OutcomeClass, now time.Time, nowMS int64) {
	g := a.gateLocked(now)
	if g.Active {
		switch {
		case class == Outcome2xx && at.Direct:
			// Direct canary succeeded: permit exactly one node probe.
			g.CanaryOK = true
			g.NodeProbeUsed = false
			g.NextCanary = nowMS + gateCanaryPeriod.Milliseconds()
		case class == Outcome2xx && !at.Direct && (at.gateProbe || g.CanaryOK):
			// Node probe succeeded: the pool is no longer in winter.
			a.clearGateLocked(nowMS)
			return
		case class != Outcome2xx && at.gateProbe:
			// Any failed node probe (429/403/5xx/network/timeout/cancel/other)
			// must reopen the state machine, otherwise both canary and node
			// probe entrances can stay closed forever.
			g.CanaryOK = false
			g.NodeProbeUsed = false
			g.NextCanary = nowMS + gateCanaryPeriod.Milliseconds()
			g.NextNodeProbe = nowMS + gateEscapePeriod.Milliseconds()
			return
		case class != Outcome2xx && at.Direct:
			// Direct canary failed: retry it after one minute, keep the
			// 10-minute time escape for a node probe.
			g.CanaryOK = false
			g.NodeProbeUsed = false
			g.NextCanary = nowMS + gateCanaryPeriod.Milliseconds()
			return
		case class != Outcome2xx && !at.Direct && g.CanaryOK:
			// Probe admitted after a canary but failed for a non-429 reason.
			g.CanaryOK = false
			g.NodeProbeUsed = false
			g.NextCanary = nowMS + gateCanaryPeriod.Milliseconds()
			g.NextNodeProbe = nowMS + gateEscapePeriod.Milliseconds()
			return
		}
		return
	}

	// Tumbling 5-minute sample window. This is intentionally reset at the
	// boundary so the failure ratio is based on a bounded, cheap counter.
	if g.WindowStart == 0 || nowMS-g.WindowStart >= gateWindow.Milliseconds() {
		g.WindowStart = nowMS
		g.Attempts = 0
		g.Failures = 0
		g.Egresses = map[string]int64{}
	}
	g.Attempts++
	if class == Outcome429 {
		g.Failures++
	}
	if class != Outcome429 || at.Egress == "" {
		return
	}
	cutoff := nowMS - gateWindow.Milliseconds()
	for k, ts := range g.Egresses {
		if ts < cutoff {
			delete(g.Egresses, k)
		}
	}
	g.Egresses[at.Egress] = nowMS
	if len(g.Egresses) < gateDistinct {
		return
	}
	if g.Attempts < gateMinAttempts || g.Failures*2 < g.Attempts {
		return
	}
	if g.LastTrip > 0 && nowMS-g.LastTrip >= gateQuiet.Milliseconds() {
		g.Backoff = 0
	}
	g.Backoff++
	pause := gateBasePause * time.Duration(1<<(g.Backoff-1))
	if pause > gateMaxPause {
		pause = gateMaxPause
	}
	g.Active = true
	g.PausedUntil = nowMS + pause.Milliseconds()
	g.NextCanary = g.PausedUntil
	g.NextNodeProbe = nowMS + gateEscapeDelay.Milliseconds()
	g.CanaryOK = false
	g.NodeProbeUsed = false
	g.Egresses = map[string]int64{}
	g.WindowStart = nowMS
	g.Attempts = 0
	g.Failures = 0
	g.LastTrip = nowMS
}

func (a *Admission) clearGateLocked(nowMS int64) {
	g := a.gateLocked(a.now())
	g.Active = false
	g.PausedUntil = 0
	g.CanaryOK = false
	g.NodeProbeUsed = false
	g.NextCanary = 0
	g.NextNodeProbe = 0
	g.Egresses = map[string]int64{}
	g.WindowStart = nowMS
	g.Attempts = 0
	g.Failures = 0
}

func (a *Admission) GateCanaryDue() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.gateLocked(a.now())
	return g.Active && !g.CanaryOK && a.now().UnixMilli() >= g.NextCanary
}

func (a *Admission) GateNodeProbeReady() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.gateLocked(a.now())
	if !g.Active || g.NodeProbeUsed {
		return false
	}
	if g.CanaryOK {
		return true
	}
	return g.NextNodeProbe > 0 && a.now().UnixMilli() >= g.NextNodeProbe
}

// AdmitGateProbe is the single node probe after a successful direct canary,
// or the 10-minute time-escape probe when direct itself is unhealthy.
// It deliberately ignores the pair cooldown by selecting with probe=true, but the
// Attempt is marked probe=false so a 429 still advances the normal pair ladder.
// It still honors sub-quarantine and inflight limits; a 2xx clears the pool gate.
func (a *Admission) AdmitGateProbe(model string) (*Attempt, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.gateLocked(a.now())
	now := a.now()
	nowMS := now.UnixMilli()
	if !g.Active || g.NodeProbeUsed {
		return nil, "probe_not_ready"
	}
	if !g.CanaryOK && (g.NextNodeProbe == 0 || nowMS < g.NextNodeProbe) {
		return nil, "probe_not_ready"
	}
	if a.candidates == nil {
		return nil, "no_candidate"
	}
	cands := a.candidates(model)
	eligible := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if c.Direct || c.Egress == "" {
			continue
		}
		if ok, _ := a.eligibleLocked(c, model, nowMS, true); ok {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		return nil, "no_candidate"
	}
	a.sortCandidatesLocked(eligible, model)
	for _, c := range eligible {
		sem := a.semLocked(c.Egress)
		select {
		case sem <- struct{}{}:
			g.NodeProbeUsed = true
			at := a.attemptLocked(c, model, false, false, false, sem)
			at.gateProbe = true
			return at, ""
		default:
		}
	}
	return nil, "all_inflight"
}

func (a *Admission) markGateNodeProbe() {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.gateLocked(a.now())
	g.NodeProbeUsed = true
}

// AdmitCanary takes only the direct egress during an active gate. It ignores
// the direct cooldown/pair state: it is an explicit, one-shot recovery probe.
func (a *Admission) AdmitCanary(model string) (*Attempt, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.gateLocked(a.now())
	if !g.Active || g.CanaryOK {
		return nil, "not_due"
	}
	nowMS := a.now().UnixMilli()
	if nowMS < g.NextCanary {
		return nil, "canary_wait"
	}
	sem := a.semLocked(EgressKeyDirect)
	select {
	case sem <- struct{}{}:
		g.NextCanary = nowMS + gateCanaryPeriod.Milliseconds()
		return a.attemptLocked(Candidate{Egress: EgressKeyDirect, Direct: true, Model: model}, model, false, false, false, sem), ""
	default:
		return nil, "all_inflight"
	}
}

func hardBlockDuration(hard int) time.Duration {
	if hard < 1 {
		hard = 1
	}
	if hard > 6 {
		hard = 6
	}
	d := time.Hour * time.Duration(1<<(hard-1))
	if d > 24*time.Hour {
		return 24 * time.Hour
	}
	return d
}
