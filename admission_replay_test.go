package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type replayEvent struct {
	T int64  `json:"t"`
	C string `json:"c"`
}

type replayResult struct {
	sent429    int
	sent2xx    int
	suppressed int
}

func replayPair(t *testing.T, name, model, egress string) replayResult {
	t.Helper()
	path := filepath.Join("testdata", "replay", name)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	now := time.Date(2026, 10, 27, 0, 15, 0, 0, time.UTC)
	c := &admissionTestClock{now: now}
	SetClock(c)
	t.Cleanup(func() { SetClock(nil) })
	st := NewNodeStateV5(now)
	st.Egress[egress] = &EgressState{Lifecycle: "active"}
	a := NewAdmission(st, 8, func(string) []Candidate {
		return []Candidate{{Egress: egress, Model: model, Sub: "s"}}
	})
	a.clock = c

	var out replayResult
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev replayEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		c.mu.Lock()
		c.now = time.UnixMilli(ev.T)
		c.mu.Unlock()
		at, _ := a.Admit(model, nil)
		if at == nil {
			out.suppressed++
			continue
		}
		a.Observe(at, OutcomeClass(ev.C))
		switch ev.C {
		case "429":
			out.sent429++
		case "2xx":
			out.sent2xx++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}
