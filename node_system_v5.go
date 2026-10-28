package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	v5MaxAttempts = 2

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
	statePath  string

	persistMu sync.Mutex
	dirty     bool
	lastSave  time.Time

	requests int64
	agree    int64
	disagree int64

	mu      sync.Mutex
	byClass map[string]int64
}

var (
	v5RTMu sync.Mutex
	v5RT   *v5Runtime

	v5FallbackMu   sync.Mutex
	v5FallbackLast = map[string]time.Time{}

	v5GateLogMu   sync.Mutex
	v5GateLogLast = map[string]time.Time{}

	// v5CandidatesHook 仅测试注入，生产为 nil。影子测试需要一个不依赖
	// proxyPool 全局状态的候选集；不能在请求路径上留下可变 hook。
	v5CandidatesHook func(string) []Candidate
)

// v5StatePathFor keeps the state file next to the active config, so a test
// instance started with -config /tmp/x/config.json cannot touch /data/v5_state.json.
func v5StatePathFor(configPath string) string {
	if configPath == "" {
		return "v5_state.json"
	}
	return filepath.Join(filepath.Dir(configPath), "v5_state.json")
}

func resetV5ForTest() {
	v5RTMu.Lock()
	v5RT = nil
	v5RTMu.Unlock()
	v5CandidatesHook = nil
	v5FallbackMu.Lock()
	v5FallbackLast = map[string]time.Time{}
	v5FallbackMu.Unlock()
	v5GateLogMu.Lock()
	v5GateLogLast = map[string]time.Time{}
	v5GateLogMu.Unlock()
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
	statePath := v5StatePathFor(configPath)
	st, loadErr := loadNodeStateV5(statePath)
	if loadErr != nil {
		slog.Warn("v5_state_load_failed", "path", statePath, "error", loadErr)
	}
	if st == nil {
		st = NewNodeStateV5(time.Now())
	}
	now := time.Now()
	adm := NewAdmission(st, 8, v5Candidates)
	if p := loadConfig(configPath).ProbeCountsTowardGate; p != nil {
		adm.probeCountsTowardGate = *p
	}
	if p := loadConfig(configPath).QuotaGateEnforce; p != nil {
		adm.quotaGateEnforce = *p
	}
	v5RT = &v5Runtime{
		started:    now,
		shadowOnly: true,
		adm:        adm,
		statePath:  statePath,
		lastSave:   now,
		byClass:    map[string]int64{},
	}
	return v5RT
}

// v5ApplyProbeGateConfig updates an already-created runtime in place. It must
// not create a runtime (config may be applied before NODE_SYSTEM is enabled).
func v5ApplyProbeGateConfig(v bool) {
	v5RTMu.Lock()
	rt := v5RT
	v5RTMu.Unlock()
	if rt == nil {
		return
	}
	rt.adm.SetProbeCountsTowardGate(v)
}

// v5ApplyQuotaGateConfig hot-reloads quota enforcement without creating a runtime.
func v5ApplyQuotaGateConfig(v bool) {
	v5RTMu.Lock()
	rt := v5RT
	v5RTMu.Unlock()
	if rt == nil {
		return
	}
	rt.adm.SetQuotaGateEnforce(v)
}

// markDirty records a state mutation; the actual write is debounced by
// persistIfDue (2s) so a burst of 429s does not fsync on every attempt.
func (rt *v5Runtime) markDirty() {
	if rt == nil {
		return
	}
	rt.persistMu.Lock()
	rt.dirty = true
	rt.persistMu.Unlock()
}

func (rt *v5Runtime) persistIfDue(now time.Time) {
	if rt == nil {
		return
	}
	rt.persistMu.Lock()
	if !rt.dirty || now.Sub(rt.lastSave) < 2*time.Second {
		rt.persistMu.Unlock()
		return
	}
	rt.dirty = false
	rt.lastSave = now
	path := rt.statePath
	rt.persistMu.Unlock()

	rt.adm.mu.Lock()
	err := SaveNodeStateV5(path, rt.adm.st, rt.adm)
	rt.adm.mu.Unlock()
	if err != nil {
		slog.Warn("v5_state_save_failed", "path", path, "error", err)
		rt.markDirty()
	}
}

// persistNow is used by tests and shutdown/flush paths; it bypasses the 2s debounce.
func (rt *v5Runtime) persistNow() {
	if rt == nil {
		return
	}
	rt.persistMu.Lock()
	rt.dirty = true
	rt.persistMu.Unlock()
	rt.persistIfDue(time.Now().Add(2 * time.Second))
}

// v5Candidates 枚举节点 + direct；不 sweep、不改变节点生命周期，避免影子
// 观测反过来影响真实选路。这里刻意不按旧池的 State/Dead/Cooldown 过滤：
// force 下 v5 不读旧状态，旧 dead 是应忽略的陈旧信号，隔离是设计要求。
// 每个节点按需懒建/复用 client；创建失败才跳过。
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
	proxyPool.mu.RUnlock()

	out := make([]Candidate, 0, len(nodes)+1)
	for _, n := range nodes {
		if n.Fingerprint == "" {
			continue
		}
		// getClient lazily creates the cached client on a cold start. Relying
		// on a pre-populated clients map made force mode see only direct until
		// the first health probe happened (F3).
		if proxyPool.getClient(n.Fingerprint) == nil {
			continue
		}
		out = append(out, Candidate{
			Egress: nodeEgressKey(n.Fingerprint),
			Sub:    subKeyOf(n.Name),
			Model:  model,
			Region: n.Region,
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
		out = append(out, Candidate{Egress: nodeEgressKey(n.Fingerprint), Sub: subKeyOf(n.Name), Model: model, Region: n.Region})
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
func probeOutcomeClass(res probeResult) (OutcomeClass, bool) {
	switch res.Class {
	case probeClassOK:
		return Outcome2xx, true
	case probeClassQuota:
		return Outcome429, true
	case probeClassQuotaUnknown:
		return OutcomeQuotaUnknown, true
	case probeClassDenied:
		return Outcome403, true
	case probeClassError:
		return Outcome5xx, true
	case probeClassUnreachable:
		if res.Stage == string(ProbeStagePreflight) {
			return OutcomeNetwork, true
		}
		return OutcomeTimeout, true
	default:
		return "", false
	}
}

func v5ObserveProbeResult(node ProxyNode, res probeResult, model string) {
	class, ok := probeOutcomeClass(res)
	if !ok {
		return
	}
	rt := v5GetRuntime()
	if rt == nil {
		return
	}
	at := &Attempt{
		Model:    model,
		Egress:   nodeEgressKey(node.Fingerprint),
		Sub:      subKeyOf(node.Name),
		Stage:    ProbeStage(res.Stage),
		HardFail: !res.Timeout,
	}
	rt.adm.ObserveManualProbe(at, class)
	rt.markDirty()
}

func v5PersistProbeResults() {
	if rt := v5GetRuntime(); rt != nil {
		rt.markDirty()
		rt.persistIfDue(time.Now())
	}
}

func v5ObserveUpstream(ctx context.Context, modelID string, eg EgressResult, status int, errBody []byte, err error, attemptVer string) {
	mode := v5Enabled()
	at := eg.V5()
	if mode == v5Off {
		releaseAttempt(at)
		return
	}
	rt := v5GetRuntime()
	if rt == nil {
		releaseAttempt(at)
		return
	}
	if mode != v5Force {
		// 切到 shadow 也可能持有 force 租约；shadow 不 Observe，必须归还。
		releaseAttempt(at)
	}

	class := classifyV5(status, errBody, err, attemptVer, pinnedOCVersion)
	actual := eg.EgressKey()

	if mode == v5Force {
		if at := eg.V5(); at != nil {
			rt.adm.Observe(at, class)
			rt.markDirty()
			rt.persistIfDue(time.Now())
		}
		rt.mu.Lock()
		rt.requests++
		rt.byClass[string(class)]++
		rt.mu.Unlock()
		if class != "other" {
			v5AppendOutcome(ctx, modelID, actual, class)
		}
		return
	}

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

// v5LegacyGatesBypassed 是 force 下旁路旧闸门的唯一判定。
func v5LegacyGatesBypassed() bool { return v5Enabled() == v5Force }

// v5AttemptLimit is the force-mode upstream attempt cap (N4b). Off/shadow keep
// their historical caps.
func v5AttemptLimit() int {
	if v5Enabled() == v5Force {
		return v5MaxAttempts
	}
	return 0
}

// v5DropAttempt 归还一个未真正发出的租约；once 确保与 Observe 不会双重归还。
func v5DropAttempt(at *Attempt) {
	if at == nil {
		return
	}
	// 未真正发出的租约也必须走 Observe("other")：它同时做 release
	// 和 exploreInflight 递减（见 Admission.Observe）。只调 release()
	// 会让冷启动的 explore 计数永久停在 1，之后同出口无法再探索。
	if rt := v5GetRuntime(); rt != nil {
		rt.adm.Observe(at, OutcomeClass("other"))
		return
	}
	at.once.Do(func() {
		if at.release != nil {
			at.release()
		}
	})
}

// v5Admit 保留给测试/旧调用方的三参入口；Get 使用带 context 的版本以落 fallback 事件。
func v5Admit(model, region string, exclude map[string]bool) (*Attempt, bool) {
	at, err := v5AdmitContext(context.Background(), model, region, exclude)
	return at, err == nil
}

func v5AdmitContext(ctx context.Context, model, region string, exclude map[string]bool) (*Attempt, *AdmitError) {
	rt := v5GetRuntime()
	if rt == nil {
		v5Fallback(ctx, model, "disabled")
		return nil, &AdmitError{Reason: "halt", RetryAfter: time.Minute, Status: http.StatusServiceUnavailable}
	}

	ex := make(map[string]bool, len(exclude)+1)
	for k, v := range exclude {
		ex[k] = v
	}
	for _, c := range v5Candidates(model) {
		if c.Egress == "" {
			continue
		}
		if region != "" && (c.Direct || c.Egress == EgressKeyDirect || c.Region != region) {
			ex[c.Egress] = true
		}
	}

	// The pool-wide winter gate blocks normal traffic. The only exceptions are
	// the periodic direct canary and the one node probe after a direct 2xx.
	if rt.adm.GatePaused() {
		if rt.adm.GateCanaryDue() {
			if at, reason := rt.adm.AdmitCanary(model); at != nil {
				return at, nil
			} else if reason != "" && reason != "canary_wait" {
				v5Winter(ctx, model, "canary_"+reason)
			}
		}
		if rt.adm.GateNodeProbeReady() {
			at, reason := rt.adm.AdmitGateProbe(model)
			if at != nil {
				return at, nil
			}
			v5Winter(ctx, model, "node_probe_"+reason)
		} else {
			v5Winter(ctx, model, "gate_paused")
		}
		return nil, v5AdmitError(rt, model, ex)
	}

	prefer := ""
	if fp := proxyPool.manualFp(); fp != "" {
		prefer = nodeEgressKey(fp)
	} else if fp := proxyPool.currentFp(); fp != "" {
		prefer = nodeEgressKey(fp)
	}

	at, reason := rt.adm.AdmitPreferred(model, ex, prefer)
	if at == nil {
		v5NoCandidate(ctx, model, reason)
		return nil, v5AdmitError(rt, model, ex)
	}
	return at, nil
}

func v5AdmitError(rt *v5Runtime, model string, exclude map[string]bool) *AdmitError {
	if rt == nil {
		return &AdmitError{Reason: "halt", RetryAfter: time.Minute, Status: http.StatusServiceUnavailable}
	}
	retry, reason := rt.adm.RetryAfter(RetryRequest{Model: model, Exclude: exclude})
	return &AdmitError{Reason: reason, RetryAfter: retry, Status: statusFor(reason)}
}

func statusFor(reason string) int {
	if reason == "halt" {
		return http.StatusServiceUnavailable
	}
	return http.StatusTooManyRequests
}

// v5NoCandidateHTTP is the single mapping from a force NoCandidate lease
// decision to the HTTP response. Retry-After is rounded up to whole seconds.
type v5FailureError struct {
	reason     string
	retryAfter time.Duration
}

func (e *v5FailureError) Error() string { return "v5 gate: " + e.reason }

func (e *v5FailureError) errorType() string {
	switch e.reason {
	case "quota":
		return "quota_exhausted"
	case "gate", "halt":
		return "gate"
	default:
		return "no_candidate"
	}
}

func (e *v5FailureError) message() string {
	switch e.errorType() {
	case "quota_exhausted":
		return "upstream quota exhausted"
	case "gate":
		return "upstream gate paused"
	default:
		return "no candidate available"
	}
}

func v5NoCandidateHTTP(res EgressResult) (int, http.Header, error) {
	reason := res.NoCandidateReason
	if reason == "" {
		reason = "no_candidate"
	}
	secs := int64((res.RetryAfter + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return statusFor(reason), http.Header{"Retry-After": {fmt.Sprintf("%d", secs)}},
		&v5FailureError{reason: reason, retryAfter: res.RetryAfter}
}

func v5FailurePayload(err error) (*v5FailureError, int) {
	var f *v5FailureError
	if !errors.As(err, &f) {
		return nil, 0
	}
	secs := int64((f.retryAfter + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return f, int(secs)
}

func writeV5Failure(w http.ResponseWriter, status int, hdr http.Header, err error) bool {
	f, secs := v5FailurePayload(err)
	if f == nil {
		return false
	}
	for k, vals := range hdr {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"message":       f.message(),
		"type":          f.errorType(),
		"retry_after_s": secs,
	}})
	return true
}

func writeV5FailureClaude(w http.ResponseWriter, status int, hdr http.Header, err error) bool {
	f, secs := v5FailurePayload(err)
	if f == nil {
		return false
	}
	for k, vals := range hdr {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"message":       f.message(),
			"type":          f.errorType(),
			"retry_after_s": secs,
		},
	})
	return true
}

// v5Winter records that the pool-wide 429 gate stopped normal selection.
// Same reason is logged at most once per minute.
func v5Winter(ctx context.Context, model, reason string) {
	if reason == "" {
		reason = "gate_paused"
	}
	v5GateLog(ctx, "v5_winter", model, reason)
}

// v5NoCandidate records that force selection found no usable lease (all pairs
// cooling / region-excluded / no candidate), never a direct fallback.
func v5NoCandidate(ctx context.Context, model, reason string) {
	if reason == "" {
		reason = "no_candidate"
	}
	v5GateLog(ctx, "v5_no_candidate", model, reason)
}

func v5GateLog(ctx context.Context, typ, model, reason string) {
	now := time.Now()
	key := typ + "|" + reason
	v5GateLogMu.Lock()
	if last, ok := v5GateLogLast[key]; ok && now.Sub(last) < time.Minute {
		v5GateLogMu.Unlock()
		return
	}
	v5GateLogLast[key] = now
	v5GateLogMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	cr := callRecorderFrom(ctx)
	if cr == nil {
		callLogEvent(ctx, typ, "", "model="+model+" reason="+reason)
		return
	}
	cr.mu.Lock()
	cr.rec.Events = append(cr.rec.Events, CallEvent{
		Type:   typ,
		Model:  model,
		Reason: reason,
		Detail: "reason=" + reason,
		At:     now.Format("2006-01-02T15:04:05.000Z07:00"),
	})
	cr.mu.Unlock()
}

// v5NoCandidateEventName keeps the suppressed-attempt log consistent with the
// real reason: gate pause vs pair cooldown exhaustion.
func v5NoCandidateEventName() string {
	if rt := v5GetRuntime(); rt != nil && rt.adm.GatePaused() {
		return "v5_winter"
	}
	return "v5_no_candidate"
}

// v5Fallback 记录“force 选路无租约、本次回退直连”。同一 reason 每分钟最多一条。
func v5Fallback(ctx context.Context, model, reason string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if reason == "" {
		reason = "no_candidate"
	}
	now := time.Now()
	v5FallbackMu.Lock()
	if last, ok := v5FallbackLast[reason]; ok && now.Sub(last) < time.Minute {
		v5FallbackMu.Unlock()
		return
	}
	v5FallbackLast[reason] = now
	v5FallbackMu.Unlock()

	cr := callRecorderFrom(ctx)
	if cr == nil {
		return
	}
	cr.mu.Lock()
	cr.rec.Events = append(cr.rec.Events, CallEvent{
		Type:   "v5_fallback",
		Model:  model,
		Reason: reason,
		Detail: "reason=" + reason,
		At:     now.Format("2006-01-02T15:04:05.000Z07:00"),
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
	now := a.now().UnixMilli()
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

// v5NodeQuotaView is the panel-facing projection of one subscription quota.
type v5NodeQuotaView struct {
	Until int64  `json:"until"`
	Model string `json:"model"`
}

type v5NodeGateView struct {
	Active        bool  `json:"active"`
	PausedUntil   int64 `json:"paused_until"`
	CanaryLevel   int   `json:"canary_level"`
	NextCanary    int64 `json:"next_canary"`
	NodeProbeUsed bool  `json:"node_probe_used"`
}

type v5NodeView struct {
	PairUntil  int64            `json:"pair_until"`
	QuotaUntil *int64           `json:"quota_until"`
	Quota      *v5NodeQuotaView `json:"quota,omitempty"`
	Gate       *v5NodeGateView  `json:"gate"`
	Lifecycle  string           `json:"lifecycle"`
}

func v5NodeViewFor(fp, sub string, now time.Time) *v5NodeView {
	if v5Enabled() == v5Off {
		return nil
	}
	rt := v5GetRuntime()
	if rt == nil || rt.adm == nil {
		return nil
	}
	egress := "node:" + fp
	nowMS := now.UnixMilli()
	rt.adm.mu.Lock()
	defer rt.adm.mu.Unlock()
	view := &v5NodeView{}
	if p := rt.adm.st.Pairs[PairKey(legacyPairModel, egress)]; p != nil {
		view.PairUntil = p.Until
	}
	if e := rt.adm.st.Egress[egress]; e != nil {
		view.Lifecycle = e.Lifecycle
	}
	var earliest *v5NodeQuotaView
	for key, q := range rt.adm.st.Quota {
		if sub == "" {
			break
		}
		if q == nil || !strings.HasPrefix(key, sub+"|") || q.Until <= nowMS {
			continue
		}
		model := strings.TrimPrefix(key, sub+"|")
		if earliest == nil || q.Until < earliest.Until {
			earliest = &v5NodeQuotaView{Until: q.Until, Model: model}
		}
	}
	if earliest != nil {
		u := earliest.Until
		view.QuotaUntil = &u
		view.Quota = earliest
	}
	g := rt.adm.gateLocked(now)
	view.Gate = &v5NodeGateView{
		Active:        g.Active,
		PausedUntil:   g.PausedUntil,
		CanaryLevel:   g.CanaryLevel,
		NextCanary:    g.NextCanary,
		NodeProbeUsed: g.NodeProbeUsed,
	}
	return view
}
