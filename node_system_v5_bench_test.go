package main

import (
	"fmt"
	"testing"
	"time"
)

func synthetic212() []*ProxyNode {
	nodes := make([]*ProxyNode, 212)
	for i := range nodes {
		nodes[i] = &ProxyNode{
			Fingerprint: fmt.Sprintf("fp%03d", i),
			Name:        fmt.Sprintf("node-%03d", i),
			Region:      "jp",
		}
	}
	return nodes
}

func BenchmarkV5Candidates212(b *testing.B) {
	nodes := synthetic212()
	hasClient := func(*ProxyNode) bool { return true }
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got := v5CandidatesFrom("bench-model", nodes, hasClient)
		if len(got) != 213 {
			b.Fatalf("len=%d, want 213", len(got))
		}
	}
}

func BenchmarkV5Admit212(b *testing.B) {
	nodes := synthetic212()
	hasClient := func(*ProxyNode) bool { return true }
	cands := v5CandidatesFrom("bench-model", nodes, hasClient)
	st := NewNodeStateV5(time.Now())
	adm := NewAdmission(st, 8, func(string) []Candidate { return cands })
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		at, reason := adm.Admit("bench-model", nil)
		if at == nil {
			b.Fatalf("admit: %s", reason)
		}
		releaseAttempt(at)
	}
}
