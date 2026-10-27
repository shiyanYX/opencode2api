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
	Direct bool
}

type Attempt struct {
	Model  string
	Egress string
	Direct bool

	release func()
	once    sync.Once

	sub       string
	probe     bool
	lifecycle bool
}

type Admission struct {
	st         *NodeStateV5
	K          int
	candidates func(string) []Candidate
	clock      Clock

	mu           sync.Mutex
	sems         map[string]chan struct{}
	probeActive  map[string]bool
	probing      bool
	probeBackoff time.Duration
	probeTimeout time.Duration
	lastPrune    int64
}

var pairCooldownLadder = [...]time.Duration{
	60 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
	5 * time.Minute,
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
		st:           st,
		K:            k,
		candidates:   candidates,
		sems:         map[string]chan struct{}{},
		probeActive:  map[string]bool{},
		probeBackoff: probeBackoffLadder[0],
		probeTimeout: 20 * time.Second,
	}
}

func (a *Admission) now() time.Time {
	if a.clock != nil {
		return a.clock.Now()
	}
	return Now()
}

func (a *Admission) Admit(model string, exclude map[string]bool) (*Attempt, string) {
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
		eligible = append(eligible, c)
	}
	if len(eligible) == 0 {
		if onlySubBlocked {
			return nil, "sub_quarantine"
		}
		return nil, "no_candidate"
	}

	a.sortCandidatesLocked(eligible, model)
	for _, c := range eligible {
		sem := a.semLocked(c.Egress)
		select {
		case sem <- struct{}{}:
			return a.attemptLocked(c, model, false, false, sem), ""
		default:
		}
	}
	return nil, "all_inflight"
}

func (a *Admission) AdmitProbe(model string) (*Attempt, string) {
	now := a.now()
	nowMS := now.UnixMilli()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.maybePruneLocked(now)
	if a.probing {
		return nil, "all_inflight"
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
			at := a.attemptLocked(c, model, true, false, sem)
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
		return nil, "all_inflight"
	}

	keys := make([]string, 0, len(a.st.Egress))
	for egress := range a.st.Egress {
		keys = append(keys, egress)
	}
	sort.Strings(keys)
	for _, egress := range keys {
		e := a.st.Egress[egress]
		if e == nil || e.Lifecycle != "dead" || a.probeActive[egress] {
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
	if at.probe {
		delete(a.probeActive, at.Egress)
		a.probing = false
	}

	if at.lifecycle {
		a.observeLifecycleLocked(at, class, now, nowMS)
		return
	}

	switch class {
	case Outcome2xx:
		p := a.writePairLocked(at.Model, at.Egress)
		p.A++
		p.Strikes = 0
		p.Hard = 0
		p.Until = 0
		p.LastOK = nowMS
		if at.probe {
			a.clearSubLocked(at.sub)
			a.probeBackoff = probeBackoffLadder[0]
		}
		a.egressSuccessLocked(at.Egress)
	case Outcome429:
		p := a.writePairLocked(at.Model, at.Egress)
		p.B++
		p.LastFail = nowMS
		if at.probe {
			minUntil := nowMS + a.probeBackoff.Milliseconds()
			if p.Until < minUntil {
				p.Until = minUntil
			}
			a.advanceProbeBackoffLocked()
		} else {
			if p.Strikes >= 4 {
				p.Hard++
				if p.Hard > 6 {
					p.Hard = 6
				}
				p.Strikes = 4
				p.Until = now.Add(hardBlockDuration(p.Hard)).UnixMilli()
			} else {
				p.Strikes++
				p.Until = now.Add(pairCooldownLadder[p.Strikes-1]).UnixMilli()
			}
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
		a.observe403Locked(at, now, nowMS)
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

func (a *Admission) attemptLocked(c Candidate, model string, probe, lifecycle bool, sem chan struct{}) *Attempt {
	return &Attempt{
		Model:     model,
		Egress:    c.Egress,
		Direct:    c.Direct || c.Egress == "direct",
		release:   func() { <-sem },
		sub:       c.Sub,
		probe:     probe,
		lifecycle: lifecycle,
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
	if e := a.st.Egress[c.Egress]; e != nil {
		if e.Lifecycle == "dead" || e.DeadUntil > nowMS {
			return false, false
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

func (a *Admission) sortCandidatesLocked(cands []Candidate, model string) {
	sort.SliceStable(cands, func(i, j int) bool {
		pi := a.pairScoreLocked(model, cands[i].Egress)
		pj := a.pairScoreLocked(model, cands[j].Egress)
		if pi != pj {
			return pi > pj
		}
		li := a.pairLastOKLocked(model, cands[i].Egress)
		lj := a.pairLastOKLocked(model, cands[j].Egress)
		if li != lj {
			return li > lj
		}
		return cands[i].Egress < cands[j].Egress
	})
}

func (a *Admission) pairScoreLocked(model, egress string) float64 {
	p := a.pairLocked(model, egress)
	if p == nil {
		return 0.5
	}
	return (p.A + 0.5) / (p.A + p.B + 1)
}

func (a *Admission) pairLastOKLocked(model, egress string) int64 {
	p := a.pairLocked(model, egress)
	if p == nil {
		return 0
	}
	return p.LastOK
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

func (a *Admission) egressSuccessLocked(egress string) {
	e := a.egressLocked(egress)
	e.ConsecFail = 0
	if e.Lifecycle == "probation" || e.Lifecycle == "dead" {
		e.Lifecycle = "active"
		e.DeadUntil = 0
	}
}

func (a *Admission) egressFailureLocked(egress string, now time.Time) {
	e := a.egressLocked(egress)
	e.ConsecFail++
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
	key := PairKey(at.Model, at.Egress)
	s.Pairs = append(s.Pairs, key)
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
	if quarantine {
		s.Until = now.Add(10 * time.Minute).UnixMilli()
		s.Reason = "403"
	}
}

func (a *Admission) observeLifecycleLocked(at *Attempt, class OutcomeClass, now time.Time, nowMS int64) {
	e := a.egressLocked(at.Egress)
	switch class {
	case Outcome2xx:
		e.Lifecycle = "active"
		e.ConsecFail = 0
		e.DeadUntil = 0
		a.probeBackoff = probeBackoffLadder[0]
	case Outcome429:
		a.advanceProbeBackoffLocked()
	case Outcome403:
		a.observe403Locked(at, now, nowMS)
	case OutcomeNetwork, OutcomeTimeout:
		e.Lifecycle = "dead"
		e.DeadUntil = now.Add(30 * time.Minute).UnixMilli()
		e.ConsecFail = 0
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
