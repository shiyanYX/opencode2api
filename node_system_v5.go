package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// v5Mode 是 NODE_SYSTEM 的三档语义。off 必须保持旧路径零行为变化；
// shadow 只学习/对比；force 留给后续切片。
type v5Mode int

const (
	v5Off v5Mode = iota
	v5Shadow
	v5Force
)

func (m v5Mode) String() string {
	switch m {
	case v5Shadow:
		return "shadow"
	case v5Force:
		return "force"
	default:
		return "off"
	}
}

// v5Enabled 每次读取环境变量，未知值降级为 off 并告警一次/每次调用。
// 默认 off 是 W1 的硬约束：部署后现有选路不能因为影子代码改变。
func v5Enabled() v5Mode {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("NODE_SYSTEM")))
	switch raw {
	case "", "off":
		return v5Off
	case "shadow":
		return v5Shadow
	case "force":
		return v5Force
	default:
		slog.Warn("node_system_unknown_value", "value", raw, "fallback", "off")
		return v5Off
	}
}

// V5Stats 是 /api/stats 的 v5 影子字段。指针字段在无数据时序列化为 null，
// 避免面板把“还没样本”误读成“0% 分歧”。
type V5Stats struct {
	Mode            string           `json:"mode"`
	Since           string           `json:"since"`
	Requests        int64            `json:"requests"`
	Agree           int64            `json:"agree"`
	Disagree        int64            `json:"disagree"`
	DivergeRate     *float64         `json:"diverge_rate"`
	ByClass         map[string]int64 `json:"by_class,omitempty"`
	CoolingPairs    *int             `json:"cooling_pairs"`
	QuarantinedSubs *int             `json:"quarantined_subs"`
}

type v5Runtime struct {
	started    time.Time
	shadowOnly bool
	adm        *Admission

	requests int64
	agree    int64
	disagree int64

	mu      sync.Mutex
	byClass map[string]int64
}

var (
	v5RTMu sync.Mutex
	v5RT   *v5Runtime

	// v5CandidatesHook 仅测试注入，生产为 nil。影子测试需要一个不依赖
	// proxyPool 全局状态的候选集；不能在请求路径上留下可变 hook。
	v5CandidatesHook func(string) []Candidate
)

func resetV5ForTest() {
	v5RTMu.Lock()
	v5RT = nil
	v5RTMu.Unlock()
	v5CandidatesHook = nil
}

func v5GetRuntime() *v5Runtime {
	v5RTMu.Lock()
	defer v5RTMu.Unlock()
	if v5RT != nil {
		return v5RT
	}
	if v5Enabled() == v5Off {
		return nil
	}
	st := NewNodeStateV5(time.Now())
	v5RT = &v5Runtime{
		started:    time.Now(),
		shadowOnly: true,
		adm:        NewAdmission(st, 8, v5Candidates),
		byClass:    map[string]int64{},
	}
	return v5RT
}

// v5Candidates 只读快照：不 sweep、不改变节点生命周期，避免影子观测反过来
// 影响真实选路。每个有 client 的节点 + 一个仅用于对比的 direct。
func v5Candidates(model string) []Candidate {
	if v5CandidatesHook != nil {
		return v5CandidatesHook(model)
	}
	proxyPool.mu.RLock()
	nodes := make([]*ProxyNode, 0, len(proxyPool.nodes))
	for _, n := range proxyPool.nodes {
		if n != nil {
			nodes = append(nodes, n)
		}
	}
	clients := proxyPool.clients
	proxyPool.mu.RUnlock()

	out := make([]Candidate, 0, len(nodes)+1)
	for _, n := range nodes {
		if n.Fingerprint == "" {
			continue
		}
		if clients == nil || clients[n.Fingerprint] == nil {
			continue
		}
		out = append(out, Candidate{
			Egress: nodeEgressKey(n.Fingerprint),
			Sub:    subKeyOf(n.Name),
			Model:  model,
		})
	}
	out = append(out, Candidate{Egress: EgressKeyDirect, Direct: true, Model: model})
	return out
}

// v5CandidatesFrom 是 v5Candidates 的纯逻辑部分，便于测试节点/直连构造。
func v5CandidatesFrom(model string, nodes []*ProxyNode, hasClient func(*ProxyNode) bool) []Candidate {
	out := make([]Candidate, 0, len(nodes)+1)
	for _, n := range nodes {
		if n == nil || n.Fingerprint == "" || hasClient == nil || !hasClient(n) {
			continue
		}
		out = append(out, Candidate{Egress: nodeEgressKey(n.Fingerprint), Sub: subKeyOf(n.Name), Model: model})
	}
	out = append(out, Candidate{Egress: EgressKeyDirect, Direct: true, Model: model})
	return out
}

// classifyV5 是纯分类函数。403 只有在 FreeTier 且版本未回退时才算 403；
// 版本不一致可能走 fallback，计 other（不计账），避免把回退误记成出口问题。
func classifyV5(status int, errBody []byte, err error, attemptVer, pinnedVer string) OutcomeClass {
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return OutcomeCancel
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return OutcomeTimeout
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return OutcomeTimeout
		}
		if isFirstByteTimeout(err) {
			return OutcomeTimeout
		}
		return OutcomeNetwork
	}
	switch {
	case status >= 200 && status < 300:
		return Outcome2xx
	case status >= 500 && status < 600:
		return Outcome5xx
	case status == 403:
		if isFreeTierErrorBody(errBody) && attemptVer == pinnedVer {
			return Outcome403
		}
		return "other"
	case status == 426:
		return "other"
	}
	if quota, _ := classifyQuota(status, errBody); quota {
		return Outcome429
	}
	return "other"
}

// v5ObserveUpstream 是唯一的影子入口：每个 attempt 恰好调用一次。
// off 直接返回；shadow 只写内存计数和 call_log，不取信号量、不阻塞。
func v5ObserveUpstream(ctx context.Context, modelID string, eg EgressResult, status int, errBody []byte, err error, attemptVer string) {
	mode := v5Enabled()
	if mode == v5Off {
		return
	}
	rt := v5GetRuntime()
	if rt == nil {
		return
	}

	class := classifyV5(status, errBody, err, attemptVer, pinnedOCVersion)
	actual := eg.EgressKey()

	rt.mu.Lock()
	rt.requests++
	reqNo := rt.requests
	rt.byClass[string(class)]++
	rt.mu.Unlock()

	if class == "other" {
		// other 只留 outcome 事件，不喂状态机；否则区域/版本回退会污染 pair 后验。
		v5AppendOutcome(ctx, modelID, actual, class)
		return
	}

	predicted, reason := rt.adm.Predict(modelID, nil)
	agree := predicted != "" && predicted == actual
	if agree {
		atomic.AddInt64(&rt.agree, 1)
	} else {
		atomic.AddInt64(&rt.disagree, 1)
	}

	sub := v5SubForEgress(modelID, actual)
	rt.adm.ObserveVirtual(modelID, actual, sub, class)

	if !agree || reqNo%100 == 0 {
		v5AppendShadow(ctx, modelID, predicted, actual, reason, class, agree)
	}
	v5AppendOutcome(ctx, modelID, actual, class)
}

func v5SubForEgress(model string, egress string) string {
	if v5CandidatesHook != nil {
		for _, c := range v5CandidatesHook(model) {
			if c.Egress == egress {
				return c.Sub
			}
		}
		return ""
	}
	proxyPool.mu.RLock()
	defer proxyPool.mu.RUnlock()
	for _, n := range proxyPool.nodes {
		if n != nil && nodeEgressKey(n.Fingerprint) == egress {
			return subKeyOf(n.Name)
		}
	}
	return ""
}

func v5AppendShadow(ctx context.Context, model, predicted, actual, reason string, class OutcomeClass, agree bool) {
	cr := callRecorderFrom(ctx)
	if cr == nil {
		return
	}
	ag := agree
	cr.mu.Lock()
	cr.rec.Events = append(cr.rec.Events, CallEvent{
		Type:      "v5_shadow",
		Model:     model,
		Node:      v5NodeFP(actual),
		At:        time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
		Predicted: predicted,
		Actual:    actual,
		Reason:    reason,
		Class:     string(class),
		Agree:     &ag,
	})
	cr.mu.Unlock()
}

func v5AppendOutcome(ctx context.Context, model, actual string, class OutcomeClass) {
	cr := callRecorderFrom(ctx)
	if cr == nil {
		return
	}
	cr.mu.Lock()
	cr.rec.Events = append(cr.rec.Events, CallEvent{
		Type:   "v5_outcome",
		Model:  model,
		Node:   v5NodeFP(actual),
		At:     time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
		Actual: actual,
		Class:  string(class),
	})
	cr.mu.Unlock()
}

func v5NodeFP(egress string) string {
	if strings.HasPrefix(egress, "node:") {
		return strings.TrimPrefix(egress, "node:")
	}
	return ""
}

// v5StatsForResponse 不触发初始化（off 下返回 nil，保留旧 JSON）。
func v5StatsForResponse() *V5Stats {
	mode := v5Enabled()
	if mode == v5Off {
		return nil
	}
	rt := v5GetRuntime()
	if rt == nil {
		return &V5Stats{Mode: mode.String()}
	}

	rt.mu.Lock()
	requests := rt.requests
	byClass := make(map[string]int64, len(rt.byClass))
	for k, v := range rt.byClass {
		byClass[k] = v
	}
	rt.mu.Unlock()

	out := &V5Stats{
		Mode:     mode.String(),
		Since:    rt.started.UTC().Format(time.RFC3339),
		Requests: requests,
		Agree:    atomic.LoadInt64(&rt.agree),
		Disagree: atomic.LoadInt64(&rt.disagree),
	}
	if len(byClass) > 0 {
		out.ByClass = byClass
	}
	if denom := out.Agree + out.Disagree; denom > 0 {
		r := float64(out.Disagree) / float64(denom)
		out.DivergeRate = &r
	}
	if requests > 0 {
		cooling, subs := rt.adm.SnapshotCounts()
		out.CoolingPairs = &cooling
		out.QuarantinedSubs = &subs
	}
	return out
}

// v5PairStats 复刻 sortCandidatesLocked 的衰减口径，但不落盘：
// A/B 先减去 LastDecay 到 now 的半衰，再叠加 prior。这样影子预测与真实
// 排序在长时间空闲后仍然可比，同时不会改变 PairState。
func v5PairStats(p *PairState, nowMS int64) (n, posterior float64) {
	var a, b float64
	if p != nil {
		a = math.Max(0, p.A)
		b = math.Max(0, p.B)
		if p.LastDecay != 0 && nowMS > p.LastDecay {
			f := math.Pow(0.5, float64(nowMS-p.LastDecay)/(pairHalfLifeDays*24*60*60*1000))
			a *= f
			b *= f
		}
	}
	n = a + b
	posterior = (pairPriorA + a + 0.5) / (pairPriorA + pairPriorB + a + b + 1)
	return
}

func v5PairUCB(p *PairState, totalN float64, nowMS int64) float64 {
	n, posterior := v5PairStats(p, nowMS)
	if n <= 0 {
		return 0
	}
	logN := math.Log(math.Max(totalN, 2))
	return posterior + pairUCBCoeff*math.Sqrt(2*logN/n)
}

func v5LastOK(p *PairState) int64 {
	if p == nil {
		return 0
	}
	return p.LastOK
}

func v5LastFail(p *PairState) int64 {
	if p == nil {
		return 0
	}
	return p.LastFail
}

// Predict 与 Admit 共用同一套后验/UCB/冷启动排序，但不取信号量、不写 rr、
// 不推进 dead 状态，也不落盘 decay。它只回答“当前首选是谁”。
func (a *Admission) Predict(model string, _ []Candidate) (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.now().UnixMilli()
	cands := a.candidates(model)
	if len(cands) == 0 {
		return "", "no_candidate"
	}
	eligible := make([]Candidate, 0, len(cands))
	subBlocked := false
	for _, c := range cands {
		if c.Egress == "" {
			continue
		}
		if c.Direct || c.Egress == EgressKeyDirect {
			if a.st.Direct != nil && a.st.Direct.Until > now {
				continue
			}
		}
		if !(c.Direct || c.Egress == EgressKeyDirect) {
			if e := a.st.Egress[c.Egress]; e != nil && e.Lifecycle == "dead" && e.DeadUntil > now {
				continue
			}
		}
		if p := a.pairLocked(model, c.Egress); p != nil && p.Until > now {
			continue
		}
		if c.Sub != "" {
			if s := a.st.Subs[c.Sub]; s != nil && s.Until > now {
				subBlocked = true
				continue
			}
		}
		eligible = append(eligible, c)
	}
	if len(eligible) == 0 {
		if subBlocked {
			return "", "sub_quarantine"
		}
		return "", "pair_cooldown"
	}

	var totalN float64
	for _, c := range eligible {
		n, _ := v5PairStats(a.pairLocked(model, c.Egress), now)
		totalN += n
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		pi := a.pairLocked(model, eligible[i].Egress)
		pj := a.pairLocked(model, eligible[j].Egress)
		ni, _ := v5PairStats(pi, now)
		nj, _ := v5PairStats(pj, now)
		coldI, coldJ := ni < pairExploreCutoff, nj < pairExploreCutoff
		if coldI != coldJ {
			return coldI
		}
		if coldI {
			if ni != nj {
				return ni < nj
			}
			li := v5LastOK(pi)
			lj := v5LastOK(pj)
			if li != lj {
				return li > lj
			}
			return eligible[i].Egress < eligible[j].Egress
		}
		ui := v5PairUCB(pi, totalN, now)
		uj := v5PairUCB(pj, totalN, now)
		if ui != uj {
			return ui > uj
		}
		li := v5LastOK(pi)
		lj := v5LastOK(pj)
		if li != lj {
			return li > lj
		}
		fi := v5LastFail(pi)
		fj := v5LastFail(pj)
		if fi != fj {
			return fi < fj
		}
		return eligible[i].Egress < eligible[j].Egress
	})
	return eligible[0].Egress, "ok"
}

// ObserveVirtual 构造不带信号量的 Attempt，复用 Observe 的 pair/sub/egress
// 状态机。影子只学习，不持有 slot，因此不会影响真实并发。
func (a *Admission) ObserveVirtual(model, egress, sub string, class OutcomeClass) {
	a.Observe(&Attempt{
		Model:  model,
		Egress: egress,
		Direct: egress == EgressKeyDirect,
		sub:    sub,
	}, class)
}

// SnapshotCounts 供面板读当前冷却 pair / 隔离 sub 数；只读，不 sweep。
func (a *Admission) SnapshotCounts() (coolingPairs, quarantinedSubs int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clock.Now().UnixMilli()
	for _, p := range a.st.Pairs {
		if p != nil && p.Until > now {
			coolingPairs++
		}
	}
	for _, s := range a.st.Subs {
		if s != nil && s.Until > now {
			quarantinedSubs++
		}
	}
	return
}
