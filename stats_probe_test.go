package main

import (
	"testing"
	"time"
)

func TestProbeRecordsExcludedFromStats(t *testing.T) {
	useTempCallLogRaw(t, "")
	ts := time.Now().Format(time.RFC3339Nano)
	probe := CallRecord{ReqID: "p1", TS: ts, Model: "m", Status: "ok", RouteMode: "probe", OutputSpeed: 40, TTFTMs: 10}
	normal := CallRecord{ReqID: "n1", TS: ts, Model: "m", Status: "ok", OutputSpeed: 50, TTFTMs: 20}
	callLog.append(probe)
	callLog.append(normal)

	callLog.mu.Lock()
	var n, ok int64
	for _, b := range hourBuckets {
		n += b.N
		ok += b.OK
	}
	callLog.mu.Unlock()
	if n != 1 || ok != 1 {
		t.Fatalf("probe entered stats: n=%d ok=%d", n, ok)
	}

	ms := modelStatsFromCallLog("1d")
	if got := ms.TotalRequests; got != 1 {
		t.Fatalf("model stats requests=%d", got)
	}
	if m := ms.Models["m"]; m == nil || m.RequestCount != 1 {
		t.Fatalf("model stats model=%+v", m)
	}
	var trendN int64
	for _, p := range trendsFromCallLog("1d") {
		trendN += p.Requests
	}
	if trendN != 1 {
		t.Fatalf("trend requests=%d", trendN)
	}
}

func TestProbeRecordsExcludedFromScoring(t *testing.T) {
	s := newNodeScoringStore("")
	rec := CallRecord{ReqID: "p", TS: time.Now().Format(time.RFC3339Nano), Model: "m", Status: "ok", RouteMode: "probe", Nodes: []string{"n"}}
	s.observeCallRecord(rec, time.Now())
	if len(s.nodes) != 0 {
		t.Fatalf("probe entered scoring: %d", len(s.nodes))
	}
}
