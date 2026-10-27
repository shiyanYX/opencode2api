package main

import (
	"math"
	"sort"
)

const (
	pairPriorA        = 8.0
	pairPriorB        = 2.0
	pairHalfLifeDays  = 7.0
	pairExploreCutoff = 5.0
	pairUCBCoeff      = 0.05
)

// PairState.A/B store observed evidence only. The prior is added when the
// posterior is read, so a fresh pair still starts at 8/2 without changing the
// persisted counter semantics used by the admission state machine.
func pairEffectiveA(p *PairState) float64 {
	if p == nil {
		return pairPriorA
	}
	return pairPriorA + math.Max(0, p.A)
}

func pairEffectiveB(p *PairState) float64 {
	if p == nil {
		return pairPriorB
	}
	return pairPriorB + math.Max(0, p.B)
}

func (a *Admission) decayPairLocked(p *PairState, nowMS int64) {
	if p == nil {
		return
	}
	if p.LastDecay == 0 {
		p.LastDecay = nowMS
		return
	}
	dt := nowMS - p.LastDecay
	if dt <= 0 {
		return
	}
	f := math.Pow(0.5, float64(dt)/(pairHalfLifeDays*24*60*60*1000))
	p.A = math.Max(0, p.A) * f
	p.B = math.Max(0, p.B) * f
	p.LastDecay = nowMS
}

func (a *Admission) pairPosteriorLocked(model, egress string) float64 {
	p := a.pairLocked(model, egress)
	if p == nil {
		return (pairPriorA + 0.5) / (pairPriorA + pairPriorB + 1)
	}
	aa := pairEffectiveA(p)
	bb := pairEffectiveB(p)
	score := (aa + 0.5) / (aa + bb + 1)
	// Half-open pairs re-enter at 0.05 weight until two consecutive 2xx.
	if p != nil && p.Strikes > 0 && p.Until > 0 && p.Until <= a.now().UnixMilli() && p.OKStreak < 2 {
		score *= 0.05
	}
	return score
}

func (a *Admission) pairNLocked(model, egress string) float64 {
	p := a.pairLocked(model, egress)
	if p == nil {
		return 0
	}
	return math.Max(0, p.A) + math.Max(0, p.B)
}

func (a *Admission) pairLastOKLocked(model, egress string) int64 {
	p := a.pairLocked(model, egress)
	if p == nil {
		return 0
	}
	return p.LastOK
}

func (a *Admission) pairLastFailLocked(model, egress string) int64 {
	p := a.pairLocked(model, egress)
	if p == nil {
		return 0
	}
	return p.LastFail
}

func (a *Admission) pairUCBLocked(model, egress string, totalN float64) float64 {
	n := a.pairNLocked(model, egress)
	if n <= 0 {
		return 0
	}
	logN := math.Log(math.Max(totalN, 2))
	return a.pairPosteriorLocked(model, egress) + pairUCBCoeff*math.Sqrt(2*logN/n)
}

func (a *Admission) sortCandidatesLocked(cands []Candidate, model string) {
	nowMS := a.now().UnixMilli()
	var totalN float64
	for _, c := range cands {
		if p := a.pairLocked(model, c.Egress); p != nil {
			a.decayPairLocked(p, nowMS)
		}
		totalN += a.pairNLocked(model, c.Egress)
	}

	sort.SliceStable(cands, func(i, j int) bool {
		ni := a.pairNLocked(model, cands[i].Egress)
		nj := a.pairNLocked(model, cands[j].Egress)
		coldI, coldJ := ni < pairExploreCutoff, nj < pairExploreCutoff
		if coldI != coldJ {
			return coldI
		}
		if coldI {
			if ni != nj {
				return ni < nj
			}
			li := a.pairLastOKLocked(model, cands[i].Egress)
			lj := a.pairLastOKLocked(model, cands[j].Egress)
			if li != lj {
				return li > lj
			}
			return cands[i].Egress < cands[j].Egress
		}
		ui := a.pairUCBLocked(model, cands[i].Egress, totalN)
		uj := a.pairUCBLocked(model, cands[j].Egress, totalN)
		if ui != uj {
			return ui > uj
		}
		li := a.pairLastOKLocked(model, cands[i].Egress)
		lj := a.pairLastOKLocked(model, cands[j].Egress)
		if li != lj {
			return li > lj
		}
		fi := a.pairLastFailLocked(model, cands[i].Egress)
		fj := a.pairLastFailLocked(model, cands[j].Egress)
		if fi != fj {
			return fi < fj
		}
		return cands[i].Egress < cands[j].Egress
	})

	// Round-robin only among an exactly tied mature head group.
	start := 0
	for start < len(cands) && a.pairNLocked(model, cands[start].Egress) < pairExploreCutoff {
		start++
	}
	if start >= len(cands) {
		return
	}
	end := start + 1
	for end < len(cands) && sameMatureRank(a, model, cands[start], cands[end], totalN) {
		end++
	}
	if end-start <= 1 {
		return
	}
	rot := a.rr % (end - start)
	if rot == 0 {
		a.rr++
		return
	}
	group := append([]Candidate(nil), cands[start:end]...)
	for i := range group {
		cands[start+i] = group[(i+rot)%len(group)]
	}
	a.rr++
}

func sameMatureRank(a *Admission, model string, x, y Candidate, totalN float64) bool {
	if a.pairUCBLocked(model, x.Egress, totalN) != a.pairUCBLocked(model, y.Egress, totalN) {
		return false
	}
	if a.pairLastOKLocked(model, x.Egress) != a.pairLastOKLocked(model, y.Egress) {
		return false
	}
	return a.pairLastFailLocked(model, x.Egress) == a.pairLastFailLocked(model, y.Egress)
}
