package main

import (
	"testing"
	"time"
)

func TestNodeAvailabilityPriority(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	future := now.Add(time.Minute).UnixMilli()
	past := now.Add(-time.Minute).UnixMilli()
	quota := future
	probeOK := &probeResult{Class: probeClassOK, Stage: string(ProbeStageRequest)}
	probeUnreach := &probeResult{Class: probeClassUnreachable, Stage: string(ProbeStagePreflight)}
	probeTimeout := &probeResult{Class: probeClassUnreachable, Stage: string(ProbeStageRequest)}
	probeDenied := &probeResult{Class: probeClassDenied, Stage: string(ProbeStageRequest)}
	probeError := &probeResult{Class: probeClassError, Stage: string(ProbeStageRequest)}

	cases := []struct {
		name string
		old  string
		v5   *v5NodeView
		pr   *probeResult
		want string
	}{
		{"dead future", "available", &v5NodeView{Lifecycle: "dead", DeadUntil: future}, probeOK, "dead"},
		{"dead expired falls to probe", "available", &v5NodeView{Lifecycle: "dead", DeadUntil: past}, probeOK, "ok"},
		{"quota beats cooling", "available", &v5NodeView{QuotaUntil: &quota, PairUntil: future, Lifecycle: "active"}, probeOK, "quota"},
		{"pair cooling", "available", &v5NodeView{PairUntil: future, Lifecycle: "active"}, probeOK, "cooling"},
		{"gate cooling", "available", &v5NodeView{Gate: &v5NodeGateView{Active: true}, Lifecycle: "active"}, probeOK, "cooling"},
		{"probe unreachable preflight", "available", &v5NodeView{Lifecycle: "active"}, probeUnreach, "unreachable"},
		{"probe unreachable request", "available", &v5NodeView{Lifecycle: "active"}, probeTimeout, "unreachable"},
		{"probe denied", "available", &v5NodeView{Lifecycle: "active"}, probeDenied, "denied"},
		{"probe 5xx", "available", &v5NodeView{Lifecycle: "active"}, probeError, "unreachable"},
		{"probe ok", "available", &v5NodeView{Lifecycle: "active"}, probeOK, "ok"},
		{"v5 no probe", "available", &v5NodeView{Lifecycle: "active"}, nil, "untested"},
		{"v5 missing old available", "available", nil, nil, "ok"},
		{"v5 missing old exhausted", "exhausted", nil, nil, "cooling"},
		{"v5 missing old dead", "dead", nil, nil, "dead"},
		{"v5 missing probe ok", "exhausted", nil, probeOK, "ok"},
		{"v5 missing no state", "", nil, nil, "untested"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := nodeAvailability(c.old, c.v5, c.pr, now)
			if got != c.want {
				t.Fatalf("availability=%q want %q (reason=%q)", got, c.want, reason)
			}
			if reason == "" {
				t.Fatal("empty reason")
			}
			if len([]rune(reason)) > 24 {
				t.Fatalf("reason too long: %q", reason)
			}
		})
	}
}
