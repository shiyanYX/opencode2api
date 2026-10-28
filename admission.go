package main

import (
	"log/slog"
	"sort"
	"sync"
	"time"
)

type OutcomeClass string

const (
	Outcome2xx          OutcomeClass = "2xx"
	Outcome429          OutcomeClass = "429"
	Outcome403          OutcomeClass = "403"
	OutcomeNetwork      OutcomeClass = "network"
	OutcomeTimeout      OutcomeClass = "timeout"
	Outcome5xx          OutcomeClass = "5xx"
	OutcomeCancel       OutcomeClass = "cancel"
	OutcomeQuotaUnknown OutcomeClass = "quota_unknown"
)

type AttemptKind uint8

const (
	AttemptNormal AttemptKind = iota
	AttemptManualProbe
	AttemptProbe
	AttemptGateProbe
	AttemptCanary
)

type ProbeStage string

const (
	ProbeStagePreflight ProbeStage = "preflight"
	ProbeStageRequest   ProbeStage = "request"
)

type Candidate struct {
	Egress string
	Sub    string
	Model  string
	Region string // 节点区域；direct 为空
	Direct bool
}

// RetryRequest carries the same static candidate filters used by AdmitPreferred.
type RetryRequest struct {
	Model   string
	Exclude map[string]bool
	Prefer  string
}

// AdmitError is the single carrier of a no-lease decision. RetryAfter is the
// value returned by RetryAfter(RetryRequest), not a second calculation.
type AdmitError struct {
	Reason     string
	RetryAfter time.Duration
	Status     int
}

func (e *AdmitError) Error() string {
	if e == nil {
		return ""
	}
	return e.Reason
}

type Attempt struct {
	Model  string
	Egress string
	Direct bool
	Kind   AttemptKind
	// GateCount controls the winter gate 5-minute window. Normal traffic must
	// set it explicitly; probe/gate/canary attempts never count.
	GateCount bool
	GateEpoch int64
	Stage     ProbeStage
	HardFail  bool
	Sub       string
	Nodes     int
	// gateProbe 标记这是 winter gate 的节点探测；任何非 2xx 都必须重置探测状态。
	gateProbe bool

	release func()
	once    sync.Once

	sub       string
	probe     bool
	probeKey  string
	lifecycle bool
	explore   bool
}

// releaseAttempt 只归还租约、不记录状态；用于 force 租约在 off/shadow/
// runtime 缺失等「不 Observe」路径上兜底，避免信号量永久占用。
func releaseAttempt(at *Attempt) {
	if at == nil {
		return
	}
	at.once.Do(func() {
		if at.release != nil {
			at.release()
		}
	})
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

	probeCountsTowardGate bool
	quotaGateEnforce      bool
	shadowQuotaBlocks     uint64
	lastShadowLog         int64
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

	raw := a.retryCandidatesLocked(model, exclude)
	if len(raw) == 0 {
		return nil, "no_candidate"
	}
	eligible, onlySubBlocked := a.eligibleCandidatesLocked(raw, model, nowMS, false)
	if len(eligible) == 0 {
		if onlySubBlocked {
			return nil, "sub_quarantine"
		}
		// Quota is a real traffic gate, not merely a Retry-After hint.
		for _, c := range raw {
			if a.quotaGateEnforce && c.Sub != "" && !c.Direct && c.Egress != "direct" &&
				a.quotaUntilLocked(c.Sub, model) > nowMS {
				_, reason := a.retryAfterLocked(RetryRequest{Model: model, Exclude: exclude})
				return nil, reason
			}
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
			at.Nodes = a.nodesForSubLocked(model, c.Sub)
			a.stampGateEpochLocked(at)
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
			at.Nodes = a.nodesForSubLocked(model, c.Sub)
			a.stampGateEpochLocked(at)
			return at, ""
		}
	}
	for _, c := range eligible {
		if a.pairNLocked(model, c.Egress) < pairExploreCutoff {
			continue
		}
		at, ok := a.acquireLocked(c, model, false)
		if ok {
			at.Nodes = a.nodesForSubLocked(model, c.Sub)
			a.stampGateEpochLocked(at)
			return at, ""
		}
	}
	return nil, "all_inflight"
}

func (a *Admission) stampGateEpochLocked(at *Attempt) {
	if at == nil {
		return
	}
	at.GateEpoch = a.gateLocked(a.now()).Epoch
}

// retryCandidatesLocked applies only static filters (empty egress/exclude), so
// RetryAfter can reason about the full candidate set rather than the set that
// the currently active gates would allow.
func (a *Admission) retryCandidatesLocked(model string, exclude map[string]bool) []Candidate {
	if a.candidates == nil {
		return nil
	}
	cands := a.candidates(model)
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if c.Egress == "" || (exclude != nil && exclude[c.Egress]) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// eligibleCandidatesLocked is pure filtering (plus pair decay for scoring); it
// does not mutate dead/probation lifecycle state.
func (a *Admission) eligibleCandidatesLocked(cands []Candidate, model string, nowMS int64, probe bool) ([]Candidate, bool) {
	out := make([]Candidate, 0, len(cands))
	onlySubBlocked := true
	for _, c := range cands {
		var ok, subBlocked bool
		if probe {
			ok, subBlocked = a.eligibleForManualProbe(c, model, nowMS)
		} else {
			ok, subBlocked = a.eligibleForTraffic(c, model, nowMS)
		}
		if !ok {
			if !subBlocked {
				onlySubBlocked = false
			}
			continue
		}
		if p := a.pairLocked(model, c.Egress); p != nil {
			a.decayPairLocked(p, nowMS)
		}
		out = append(out, c)
	}
	return out, onlySubBlocked
}

// nodesForSubLocked counts all candidates in the subscription, not just those
// currently eligible. Pair cooldown/dead/quota must not turn a 2-node
// subscription into a fake single-node subscription.
func (a *Admission) nodesForSubLocked(model, sub string) int {
	if a.candidates == nil {
		return 0
	}
	n := 0
	for _, c := range a.candidates(model) {
		if c.Sub == sub && !c.Direct && c.Egress != "direct" && c.Egress != "" {
			n++
		}
	}
	return n
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
			at.probeKey = c.Egress
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
				Kind:      AttemptProbe,
				GateCount: false,
				Stage:     ProbeStageRequest,
				release:   func() { <-sem },
				probe:     true,
				probeKey:  egress,
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
		key := at.probeKey
		if key == "" {
			key = at.Egress
		}
		delete(a.probeActive, key)
		a.probing = len(a.probeActive) > 0
	}
	// Empty egress has no pair and no gate sample; it must never create a
	// PairKey(model, "") or advance the gate window.
	if at.Egress == "" {
		return
	}

	switch at.Kind {
	case AttemptManualProbe:
		if at.lifecycle {
			a.observeLifecycleLocked(at, class, now, nowMS)
			return
		}
		a.observeGateOutcomeLocked(at, class, now, nowMS)
		// Manual probes are the only evidence channel while the gate is active;
		// never discard their quota signal just because the gate is open.
		a.observeUpstreamLocked(at, class, now, nowMS, true)
	case AttemptGateProbe, AttemptCanary:
		a.observeGateOutcomeLocked(at, class, now, nowMS)
	default:
		if at.lifecycle {
			a.observeLifecycleLocked(at, class, now, nowMS)
			return
		}
		wasActive := a.gateLocked(now).Active
		a.observeGateOutcomeLocked(at, class, now, nowMS)
		// Normal traffic is not served while the winter gate is active. If the
		// gate just tripped on this 429, still suppress the quota write; a 2xx
		// that happens to be the decisive sample still updates its pair.
		if wasActive || (a.gateLocked(now).Active && (class == Outcome429 || class == OutcomeQuotaUnknown)) {
			return
		}
		a.observeUpstreamLocked(at, class, now, nowMS, false)
	}
}

var pair403Ladder = [3]time.Duration{10 * time.Minute, 20 * time.Minute, 30 * time.Minute}

const quotaGateWindow = 10 * time.Minute

func (a *Admission) nodePairLocked(egress string) *PairState {
	if egress == "" {
		return nil
	}
	key := PairKey(legacyPairModel, egress)
	p := a.st.Pairs[key]
	if p == nil {
		p = &PairState{}
		a.st.Pairs[key] = p
	}
	return p
}

func (a *Admission) clearQuotaLocked(sub, model string) {
	if sub == "" || sub == "direct" {
		return
	}
	delete(a.st.Quota, quotaKey(sub, model))
}

func (a *Admission) recordQuotaLocked(sub, model, egress string, nodes int, now time.Time) {
	if sub == "" || sub == "direct" || egress == "" || model == "" {
		return
	}
	if a.st.Quota == nil {
		a.st.Quota = map[string]*QuotaState{}
	}
	nowMS := now.UnixMilli()
	key := quotaKey(sub, model)
	q := a.st.Quota[key]
	created := false
	if q == nil {
		q = &QuotaState{Since: nowMS, Seen: map[string]int64{}, Nodes: nodes}
		created = true
	} else {
		// Nodes is the current candidate count for this subscription; it is
		// refreshed on every quota event, including while the gate is active.
		q.Nodes = nodes
	}
	if q.Seen == nil {
		q.Seen = map[string]int64{}
	}
	window := quotaGateWindow.Milliseconds()
	if nowMS-q.Since >= window {
		q.Since = nowMS
		q.Seen = map[string]int64{}
		q.Hits = 0
	}
	q.Seen[egress] = nowMS
	q.Hits++
	cutoff := nowMS - window
	for k, ts := range q.Seen {
		if ts < cutoff {
			delete(q.Seen, k)
		}
	}
	if len(q.Seen) >= 2 || (q.Nodes <= 1 && q.Hits >= 3) {
		if q.Until <= nowMS {
			q.Until = now.Add(quotaGateWindow).UnixMilli()
		}
	}
	if created && !a.st.setQuotaLockedAt(key, q, nowMS) {
		return
	}
	if !created {
		a.st.setQuotaLockedAt(key, q, nowMS)
	}
}

func (a *Admission) observeUpstreamLocked(at *Attempt, class OutcomeClass, now time.Time, nowMS int64, manual bool) {
	sub := at.Sub
	if sub == "" {
		sub = at.sub
	}
	switch class {
	case Outcome2xx:
		p := a.nodePairLocked(at.Egress)
		a.decayPairLocked(p, nowMS)
		p.A++
		p.LastOK = nowMS
		if (p.Strikes > 0 || p.Strike >= 3) && p.Until > 0 && p.Until <= nowMS {
			p.OKStreak++
			if p.OKStreak >= 2 {
				p.Strikes = 0
				p.Strike = 0
				p.Hard = 0
				p.Until = 0
				p.OKStreak = 0
			}
		} else if (p.Strikes > 0 || p.Strike >= 3) && p.Until > nowMS {
			// An older lease must not shorten an active cooldown.
		} else {
			p.Strikes = 0
			p.Strike = 0
			p.Hard = 0
			p.Until = 0
			p.OKStreak = 0
		}
		if manual {
			if e := a.egressLocked(at.Egress); e.Lifecycle == "dead" {
				e.Lifecycle = "active"
				e.DeadUntil = 0
				e.ConsecFail = 0
				e.ConsecOK = 0
			} else {
				a.egressConnectedLocked(at.Egress)
			}
		} else {
			a.egressConnectedLocked(at.Egress)
		}
		a.clearQuotaLocked(sub, at.Model)
		if !manual && at.Direct && a.st.Direct != nil {
			a.st.Direct.Until = 0
		}
	case Outcome429, OutcomeQuotaUnknown:
		// 429 is not a pair failure: quota state is per (sub, model).
		if sub != "" {
			a.recordQuotaLocked(sub, at.Model, at.Egress, at.Nodes, now)
		}
		if !manual && at.Direct {
			if a.st.Direct == nil {
				a.st.Direct = &DirectState{}
			}
			a.st.Direct.Until = now.Add(60 * time.Second).UnixMilli()
		}
	case Outcome403:
		p := a.nodePairLocked(at.Egress)
		if p != nil {
			if p.Strike > 0 && nowMS-p.Last403 >= (10*time.Minute).Milliseconds() {
				p.Strike = 0
			}
			p.B++
			p.LastFail = nowMS
			p.Last403 = nowMS
			p.Strike++
			if p.Strike >= 3 {
				idx := p.Strike - 3
				if idx >= len(pair403Ladder) {
					idx = len(pair403Ladder) - 1
				}
				until := now.Add(pair403Ladder[idx]).UnixMilli()
				if until > p.Until {
					p.Until = until
				}
			}
		}
	case Outcome5xx:
		if p := a.nodePairLocked(at.Egress); p != nil {
			p.B++
			p.LastFail = nowMS
		}
	case OutcomeNetwork, OutcomeTimeout:
		if manual && at.Stage == ProbeStagePreflight {
			// Preflight hard evidence (connection refused/network error) is
			// still decisive. Timeouts are weak evidence: two consecutive
			// timeouts degrade the egress instead of killing it on one sample.
			if at.HardFail {
				a.markEgressDeadLocked(at.Egress, now)
			} else {
				a.egressWeakFailureLocked(at.Egress, now)
			}
			return
		}
		if p := a.nodePairLocked(at.Egress); p != nil {
			p.B++
			p.LastFail = nowMS
		}
		if !manual {
			a.egressFailureLocked(at.Egress, now)
		}
	}
}

func (a *Admission) observeManualProbeLocked(at *Attempt, class OutcomeClass, now time.Time, nowMS int64) {
	a.observeUpstreamLocked(at, class, now, nowMS, true)
}

func (a *Admission) RetryAfter(req RetryRequest) (time.Duration, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.retryAfterLocked(req)
}

func (a *Admission) retryAfterLocked(req RetryRequest) (time.Duration, string) {
	now := a.now()
	nowMS := now.UnixMilli()
	a.maybePruneLocked(now)

	cands := a.retryCandidatesLocked(req.Model, req.Exclude)
	if len(cands) == 0 {
		return a.noCandidateRetry(req.Model)
	}

	const (
		rankQuota = iota
		rankGate
		rankDead
		rankDirect
		rankSub
		rankPair
	)
	better := func(aUntil int64, aRank int, bUntil int64, bRank int) bool {
		if bUntil == 0 {
			return true
		}
		if aUntil != bUntil {
			return aUntil < bUntil
		}
		return aRank < bRank
	}
	var minUntil int64
	var minRank int
	for _, c := range cands {
		until := int64(0)
		rank := rankPair
		consider := func(v int64, r int) {
			if v > until || (v == until && r < rank) {
				until, rank = v, r
			}
		}
		consider(a.pairUntilLocked(req.Model, c.Egress), rankPair)
		consider(a.egressDeadUntil(c.Egress), rankDead)
		if c.Sub != "" {
			if a.quotaGateEnforce {
				consider(a.quotaUntilLocked(c.Sub, req.Model), rankQuota)
			}
			consider(a.subUntilLocked(c.Sub), rankSub)
		}
		if c.Direct {
			consider(a.directUntilLocked(), rankDirect)
		}
		if until > nowMS && better(until, rank, minUntil, minRank) {
			minUntil, minRank = until, rank
		}
	}

	gateActive := a.gateLocked(now).Active
	retry := time.Second
	reason := "no_candidate"
	if minUntil > nowMS {
		retry = time.Duration(minUntil-nowMS) * time.Millisecond
		switch minRank {
		case rankQuota:
			reason = "quota"
		case rankDead:
			reason = "dead"
		case rankDirect:
			reason = "direct"
		case rankSub:
			reason = "sub"
		default:
			reason = "pair"
		}
	}
	if gateActive {
		gateRetry := a.GateRetryAfter()
		if retry < gateRetry {
			retry = gateRetry
			reason = "gate"
		}
	}
	if retry < time.Second {
		retry = time.Second
	}
	if retry > 600*time.Second {
		retry = 600 * time.Second
	}
	// Spread retries slightly to avoid a synchronized thundering herd.
	retry += time.Duration(now.UnixNano()%2000) * time.Millisecond
	return retry, reason
}

func (a *Admission) attemptLocked(c Candidate, model string, probe, lifecycle, explore bool, sem chan struct{}) *Attempt {
	return &Attempt{
		Model:     model,
		Egress:    c.Egress,
		Direct:    c.Direct || c.Egress == "direct",
		Kind:      AttemptNormal,
		GateCount: true,
		Stage:     ProbeStageRequest,
		Sub:       c.Sub,
		release: func() {
			<-sem
			// 探索名额也属于租约的一部分；releaseAttempt 不经过 Observe 时
			// 同样要归还，否则该 egress 会永久被 exploreInflight 挡住。
			if explore {
				a.mu.Lock()
				if n := a.exploreInflight[c.Egress]; n > 0 {
					a.exploreInflight[c.Egress] = n - 1
				}
				a.mu.Unlock()
			}
		},
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

// eligibleLocked is kept for legacy call sites only. Traffic and probe
// paths use the explicit three entry points below.
func (a *Admission) eligibleLocked(c Candidate, model string, nowMS int64, probe bool) (bool, bool) {
	if probe {
		return a.eligibleForManualProbe(c, model, nowMS)
	}
	return a.eligibleForTraffic(c, model, nowMS)
}

// eligibleForTraffic enforces all normal admission gates.
func (a *Admission) eligibleForTraffic(c Candidate, model string, nowMS int64) (bool, bool) {
	if c.Direct || c.Egress == "direct" {
		if a.st.Direct != nil && a.st.Direct.Until > nowMS {
			return false, false
		}
	} else {
		if e := a.st.Egress[c.Egress]; e != nil && e.Lifecycle == "dead" && e.DeadUntil > nowMS {
			return false, false
		}
	}
	if c.Sub != "" && !c.Direct && c.Egress != "direct" {
		if qUntil := a.quotaUntilLocked(c.Sub, model); qUntil > nowMS {
			if a.quotaGateEnforce {
				return false, false
			}
			a.recordShadowQuotaBlockLocked(c.Sub, model, qUntil, nowMS)
		}
	}
	if p := a.pairLocked(model, c.Egress); p != nil && p.Until > nowMS {
		return false, false
	}
	if c.Sub != "" {
		if s := a.st.Subs[c.Sub]; s != nil && s.Until > nowMS {
			return false, true
		}
	}
	return true, false
}

// recordShadowQuotaBlockLocked counts every would-be quota block, but emits at
// most one aggregate warning per 5 seconds to avoid log floods in shadow mode.
func (a *Admission) recordShadowQuotaBlockLocked(sub, model string, until, nowMS int64) {
	a.shadowQuotaBlocks++
	if a.lastShadowLog != 0 && nowMS-a.lastShadowLog < 5000 {
		return
	}
	a.lastShadowLog = nowMS
	slog.Warn("v5_quota_shadow_blocks", "total", a.shadowQuotaBlocks, "sub", sub, "model", model, "until", until, "nodes", a.nodesForSubLocked(model, sub))
}

// eligibleForGateProbe rejects dead nodes and quarantined subscriptions, but
// skips pair and quota gates so the gate's single recovery probe can run.
func (a *Admission) eligibleForGateProbe(c Candidate, model string, nowMS int64) (bool, bool) {
	if !(c.Direct || c.Egress == "direct") {
		if e := a.st.Egress[c.Egress]; e != nil && e.Lifecycle == "dead" && e.DeadUntil > nowMS {
			return false, false
		}
	}
	if c.Sub != "" {
		if s := a.st.Subs[c.Sub]; s != nil && s.Until > nowMS {
			return false, true
		}
	}
	return true, false
}

// eligibleForManualProbe deliberately ignores dead/probation, pair and quota
// state so an operator can probe and revive a node. Only subscription
// quarantine remains a hard stop.
func (a *Admission) eligibleForManualProbe(c Candidate, model string, nowMS int64) (bool, bool) {
	_ = model
	_ = nowMS
	if c.Sub != "" {
		if s := a.st.Subs[c.Sub]; s != nil && s.Until > nowMS {
			return false, true
		}
	}
	return true, false
}

func (a *Admission) pairLocked(model, egress string) *PairState {
	if egress == "" {
		return nil
	}
	if p := a.st.Pairs[PairKey(model, egress)]; p != nil {
		return p
	}
	return a.st.Pairs[PairKey("*", egress)]
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

func (a *Admission) egressWeakFailureLocked(egress string, now time.Time) {
	if egress == "" || egress == "direct" {
		return
	}
	e := a.egressLocked(egress)
	e.ConsecFail++
	e.ConsecOK = 0
	if e.ConsecFail >= 2 {
		e.Lifecycle = "dead"
		e.DeadUntil = now.Add(30 * time.Minute).UnixMilli()
		e.ConsecFail = 0
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

func (a *Admission) markEgressDeadLocked(egress string, now time.Time) {
	if egress == "" || egress == "direct" {
		return
	}
	e := a.egressLocked(egress)
	e.Lifecycle = "dead"
	e.DeadUntil = now.Add(30 * time.Minute).UnixMilli()
	e.ConsecFail = 0
	e.ConsecOK = 0
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
	case OutcomeNetwork, OutcomeTimeout:
		e.Lifecycle = "dead"
		e.DeadUntil = now.Add(30 * time.Minute).UnixMilli()
		e.ConsecFail = 0
		e.ConsecOK = 0
	}
	// 429/403/5xx are deliberately not written for lifecycle probes.
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

func canaryBackoff(level int) time.Duration {
	ladder := [...]time.Duration{60 * time.Second, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute}
	if level < 0 {
		level = 0
	}
	if level >= len(ladder) {
		level = len(ladder) - 1
	}
	return ladder[level]
}

// ObserveManualProbe is the single entry for operator/scheduled probes. It
// never acquires or releases a lease and never changes inflight counters.
// SetProbeCountsTowardGate hot-reloads whether manual probes count toward the
// winter gate. It takes a.mu itself so callers do not rely on admission lock
// ordering.
func (a *Admission) SetProbeCountsTowardGate(v bool) {
	a.mu.Lock()
	a.probeCountsTowardGate = v
	a.mu.Unlock()
}

// SetQuotaGateEnforce hot-reloads whether quota state blocks admission.
// Recording remains unconditional; only eligibility/RetryAfter change.
func (a *Admission) SetQuotaGateEnforce(v bool) {
	a.mu.Lock()
	a.quotaGateEnforce = v
	a.mu.Unlock()
}

func (a *Admission) ObserveManualProbe(at *Attempt, class OutcomeClass) {
	if at == nil {
		return
	}
	a.mu.Lock()
	at.Kind = AttemptManualProbe
	at.GateCount = a.probeCountsTowardGate
	if at.Sub == "" {
		at.Sub = at.sub
	}
	if at.Nodes == 0 && at.Sub != "" {
		at.Nodes = a.nodesForSubLocked(at.Model, at.Sub)
	}
	a.mu.Unlock()
	a.Observe(at, class)
	if class == Outcome429 || class == OutcomeQuotaUnknown {
		a.activateManualQuota(at, class)
	}
}

// activateManualQuota makes an explicit probe's quota signal immediately
// authoritative. Normal traffic still needs the ordinary 2-egress/3-hit
// evidence, but a manual probe is a deliberate single-egress test.
func (a *Admission) activateManualQuota(at *Attempt, class OutcomeClass) {
	if at == nil || at.Egress == "" || (class != Outcome429 && class != OutcomeQuotaUnknown) {
		return
	}
	sub := at.Sub
	if sub == "" {
		sub = at.sub
	}
	if sub == "" || sub == "direct" || at.Model == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	nowMS := now.UnixMilli()
	key := quotaKey(sub, at.Model)
	q := a.st.Quota[key]
	if q == nil {
		q = &QuotaState{Since: nowMS, Seen: map[string]int64{}, Nodes: at.Nodes}
	}
	if q.Until <= nowMS {
		q.Until = now.Add(quotaGateWindow).UnixMilli()
	}
	if q.Nodes == 0 {
		q.Nodes = at.Nodes
	}
	a.st.setQuotaLockedAt(key, q, nowMS)
}

func (a *Admission) observeGateOutcomeLocked(at *Attempt, class OutcomeClass, now time.Time, nowMS int64) {
	g := a.gateLocked(now)
	if at.Egress == "" {
		return
	}
	kind := at.Kind
	if kind == AttemptNormal && at.gateProbe {
		kind = AttemptGateProbe
	}
	if g.Active {
		switch kind {
		case AttemptCanary:
			if class == Outcome2xx {
				g.CanaryOK = true
				g.NodeProbeUsed = false
				g.CanaryLevel = 0
				g.NextCanary = nowMS + gateCanaryPeriod.Milliseconds()
				if a.st.Direct != nil {
					a.st.Direct.Until = 0
				}
			} else if class == Outcome429 || class == OutcomeQuotaUnknown {
				// A 429 from the direct canary is quota evidence, not an egress
				// failure. Keeping the gate active here was the 14-hour outage.
				a.clearGateLocked(nowMS)
				return
			} else {
				g.CanaryOK = false
				g.NodeProbeUsed = false
				g.NextCanary = nowMS + canaryBackoff(g.CanaryLevel).Milliseconds()
				if g.CanaryLevel < 4 {
					g.CanaryLevel++
				}
			}
		case AttemptGateProbe:
			if class == Outcome2xx {
				a.clearGateLocked(nowMS)
				return
			}
			if class == Outcome429 || class == OutcomeQuotaUnknown {
				// Quota is not a node failure; only reschedule this probe.
				g.NodeProbeUsed = false
				g.NextNodeProbe = nowMS + gateEscapePeriod.Milliseconds()
				return
			}
			g.CanaryOK = false
			g.NodeProbeUsed = false
			g.NextCanary = nowMS + gateCanaryPeriod.Milliseconds()
			g.NextNodeProbe = nowMS + gateEscapePeriod.Milliseconds()
		default:
			// Normal/manual attempts never mutate an active gate.
		}
		return
	}
	if !at.GateCount {
		return
	}
	if at.GateEpoch != g.Epoch {
		return
	}

	// Tumbling 5-minute sample window.
	if g.WindowStart == 0 || nowMS-g.WindowStart >= gateWindow.Milliseconds() {
		g.WindowStart = nowMS
		g.Attempts = 0
		g.Failures = 0
		g.Egresses = map[string]int64{}
	}
	g.Attempts++
	switch class {
	case Outcome5xx, OutcomeNetwork, OutcomeTimeout:
		// Quota (429/quota_unknown) and 403 are not egress-health failures.
		g.Failures++
		cutoff := nowMS - gateWindow.Milliseconds()
		for k, ts := range g.Egresses {
			if ts < cutoff {
				delete(g.Egresses, k)
			}
		}
		g.Egresses[at.Egress] = nowMS
	}
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
	g.Epoch++
	g.PausedUntil = nowMS + pause.Milliseconds()
	g.NextCanary = g.PausedUntil
	g.NextNodeProbe = nowMS + gateEscapeDelay.Milliseconds()
	g.CanaryOK = false
	g.NodeProbeUsed = false
	g.CanaryLevel = 0
	g.Egresses = map[string]int64{}
	g.WindowStart = nowMS
	g.Attempts = 0
	g.Failures = 0
	g.LastTrip = nowMS
}

func (a *Admission) clearGateLocked(nowMS int64) {
	g := a.gateLocked(a.now())
	g.Epoch++
	g.Active = false
	g.PausedUntil = 0
	g.CanaryOK = false
	g.NodeProbeUsed = false
	g.CanaryLevel = 0
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
		if ok, _ := a.eligibleForGateProbe(c, model, nowMS); ok {
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
			at.Kind = AttemptGateProbe
			at.GateCount = false
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
	if a.st.Direct != nil && a.st.Direct.Until > nowMS {
		return nil, "direct_cooldown"
	}
	sem := a.semLocked(EgressKeyDirect)
	select {
	case sem <- struct{}{}:
		g.NextCanary = nowMS + gateCanaryPeriod.Milliseconds()
		at := a.attemptLocked(Candidate{Egress: EgressKeyDirect, Direct: true, Model: model}, model, false, false, false, sem)
		at.Kind = AttemptCanary
		at.GateCount = false
		return at, ""
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
