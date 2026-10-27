package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type v5TestTimeout struct{}

func (v5TestTimeout) Error() string   { return "i/o timeout" }
func (v5TestTimeout) Timeout() bool   { return true }
func (v5TestTimeout) Temporary() bool { return true }

func TestV5ClassifyTable(t *testing.T) {
	freeTier := []byte(`{"error":{"type":"FreeTierError","message":"client rejected"}}`)
	quota := []byte(`{"error":{"type":"FreeUsageLimitError","message":"quota exceeded"}}`)
	ordinary429 := []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)
	region := []byte(`{"error":{"type":"RegionError","message":"not available in your region"}}`)

	cases := []struct {
		name       string
		status     int
		body       []byte
		err        error
		attemptVer string
		pinnedVer  string
		want       OutcomeClass
	}{
		{"200", 200, nil, nil, "", "", Outcome2xx},
		{"204", 204, nil, nil, "", "", Outcome2xx},
		{"500", 500, nil, nil, "", "", Outcome5xx},
		{"503", 503, nil, nil, "", "", Outcome5xx},
		{"403 pinned freetier", 403, freeTier, nil, "1.18.33", "1.18.33", Outcome403},
		{"403 version mismatch", 403, freeTier, nil, "1.19.0", "1.18.33", "other"},
		{"403 region", 403, region, nil, "1.18.33", "1.18.33", "other"},
		{"429 quota", 429, quota, nil, "", "", Outcome429},
		{"429 ordinary", 429, ordinary429, nil, "", "", "other"},
		{"426", 426, nil, nil, "", "", "other"},
		{"cancel", 0, nil, context.Canceled, "", "", OutcomeCancel},
		{"deadline", 0, nil, context.DeadlineExceeded, "", "", OutcomeTimeout},
		{"net timeout", 0, nil, v5TestTimeout{}, "", "", OutcomeTimeout},
		{"first byte timeout", 0, nil, errors.New("bun transport: timeout awaiting response headers: x"), "", "", OutcomeTimeout},
		{"network", 0, nil, errors.New("connection reset"), "", "", OutcomeNetwork},
		{"400", 400, nil, nil, "", "", "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyV5(tc.status, tc.body, tc.err, tc.attemptVer, tc.pinnedVer); got != tc.want {
				t.Fatalf("classifyV5()=%q want %q", got, tc.want)
			}
		})
	}
}

func TestV5ShadowDivergence(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "shadow")
	resetV5ForTest()
	v5CandidatesHook = func(model string) []Candidate {
		return []Candidate{
			{Egress: "node:A", Sub: "sub-a", Model: model},
			{Egress: "node:B", Sub: "sub-b", Model: model},
		}
	}
	ctx := beginCallLog(context.Background(), "/v1/chat/completions", "m", false, "test")
	v5ObserveUpstream(ctx, "m", EgressResult{NodeFP: "B"}, 200, nil, nil, pinnedOCVersion)

	rt := v5GetRuntime()
	if rt == nil {
		t.Fatal("shadow runtime not initialized")
	}
	if got := rt.requests; got != 1 {
		t.Fatalf("requests=%d want 1", got)
	}
	if got := rt.disagree; got != 1 {
		t.Fatalf("disagree=%d want 1", got)
	}
	if got := rt.agree; got != 0 {
		t.Fatalf("agree=%d want 0", got)
	}

	cr := callRecorderFrom(ctx)
	cr.mu.Lock()
	events := append([]CallEvent(nil), cr.rec.Events...)
	cr.mu.Unlock()
	if len(events) < 2 {
		t.Fatalf("events=%d want shadow+outcome", len(events))
	}
	var shadow *CallEvent
	for i := range events {
		if events[i].Type == "v5_shadow" {
			shadow = &events[i]
			break
		}
	}
	if shadow == nil {
		t.Fatal("missing v5_shadow event")
	}
	if shadow.Predicted != "node:A" || shadow.Actual != "node:B" {
		t.Fatalf("shadow predicted/actual=%q/%q", shadow.Predicted, shadow.Actual)
	}
	if shadow.Agree == nil || *shadow.Agree {
		t.Fatalf("shadow agree=%v want false", shadow.Agree)
	}
	if shadow.Class != string(Outcome2xx) || shadow.Reason != "ok" {
		t.Fatalf("shadow class/reason=%q/%q", shadow.Class, shadow.Reason)
	}
}

func TestV5PredictNoSideEffect(t *testing.T) {
	st := NewNodeStateV5(time.Now())
	now := time.Now().UnixMilli()
	st.Pairs[PairKey("m", "node:A")] = &PairState{A: 20, B: 0, LastOK: now, Until: now + int64(time.Hour/time.Millisecond)}
	adm := NewAdmission(st, 8, func(model string) []Candidate {
		return []Candidate{
			{Egress: "node:A", Model: model},
			{Egress: "node:B", Model: model},
		}
	})
	before := map[string]PairState{}
	for k, v := range st.Pairs {
		before[k] = *v
	}
	beforeInflight := map[string]int{}
	for k, v := range adm.exploreInflight {
		beforeInflight[k] = v
	}

	pred, reason := adm.Predict("m", nil)
	if pred != "node:B" || reason != "ok" {
		t.Fatalf("Predict()=%q,%q want node:B,ok", pred, reason)
	}
	if !reflect.DeepEqual(before, pairSnapshot(st.Pairs)) {
		t.Fatalf("Predict mutated pairs: before=%+v after=%+v", before, st.Pairs)
	}
	if !reflect.DeepEqual(beforeInflight, adm.exploreInflight) {
		t.Fatalf("Predict mutated inflight: before=%+v after=%+v", beforeInflight, adm.exploreInflight)
	}
	if adm.rr != 0 {
		t.Fatalf("Predict advanced rr=%d", adm.rr)
	}
}

func pairSnapshot(m map[string]*PairState) map[string]PairState {
	out := make(map[string]PairState, len(m))
	for k, v := range m {
		out[k] = *v
	}
	return out
}

func TestV5StatsNullWhenNoData(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "shadow")
	resetV5ForTest()
	st := v5StatsForResponse()
	if st == nil {
		t.Fatal("stats nil in shadow")
	}
	if st.DivergeRate != nil {
		t.Fatalf("diverge_rate=%v want nil", *st.DivergeRate)
	}
	if st.CoolingPairs != nil || st.QuarantinedSubs != nil {
		t.Fatalf("cooling/subs=%v/%v want nil", st.CoolingPairs, st.QuarantinedSubs)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"diverge_rate":null`) {
		t.Fatalf("json=%s want diverge_rate:null", b)
	}
}

func TestV5ModeOffDoesNothing(t *testing.T) {
	t.Setenv("NODE_SYSTEM", "off")
	resetV5ForTest()
	ctx := beginCallLog(context.Background(), "/v1/chat/completions", "m", false, "test")
	v5ObserveUpstream(ctx, "m", EgressResult{NodeFP: "A"}, 200, nil, nil, pinnedOCVersion)

	v5RTMu.Lock()
	rt := v5RT
	v5RTMu.Unlock()
	if rt != nil {
		t.Fatal("off initialized runtime")
	}
	cr := callRecorderFrom(ctx)
	cr.mu.Lock()
	n := len(cr.rec.Events)
	cr.mu.Unlock()
	if n != 0 {
		t.Fatalf("off wrote %d events", n)
	}
	if st := v5StatsForResponse(); st != nil {
		t.Fatalf("off stats=%+v want nil", st)
	}
}

func TestV5CandidateBuild(t *testing.T) {
	nodes := []*ProxyNode{
		{Name: "sub-a::n1", Fingerprint: "f1"},
		{Name: "sub-b::n2", Fingerprint: "f2"},
		{Name: "sub-c::no-client", Fingerprint: "f3"},
	}
	hasClient := func(n *ProxyNode) bool { return n.Fingerprint != "f3" }
	got := v5CandidatesFrom("m", nodes, hasClient)
	if len(got) != 3 {
		t.Fatalf("candidates=%d want 2 nodes + direct", len(got))
	}
	if got[0].Egress != "node:f1" || got[0].Sub != "sub-a" || got[0].Model != "m" {
		t.Fatalf("node candidate=%+v", got[0])
	}
	if got[1].Egress != "node:f2" || got[1].Sub != "sub-b" {
		t.Fatalf("node candidate=%+v", got[1])
	}
	if got[2].Egress != "direct" || !got[2].Direct || got[2].Model != "m" {
		t.Fatalf("direct candidate=%+v", got[2])
	}
}
