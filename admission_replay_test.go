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

func TestReplay653e(t *testing.T) {
	got := replayPair(t, "pair_653e_mimo.jsonl", "mimo-v2.6-flash-free", "node:653e")
	t.Logf("653e mimo sent429=%d sent2xx=%d suppressed=%d", got.sent429, got.sent2xx, got.suppressed)
	if got.sent429 > 8 {
		t.Fatalf("653e mimo sent 429 = %d, want <=8 (suppressed=%d)", got.sent429, got.suppressed)
	}
}

func TestReplay653eDeepseek(t *testing.T) {
	got := replayPair(t, "pair_653e_deepseek.jsonl", "deepseek-v4.1-flash-free", "node:653e")
	t.Logf("653e deepseek sent429=%d sent2xx=%d suppressed=%d", got.sent429, got.sent2xx, got.suppressed)
	if got.sent429 != 0 || got.sent2xx != 5 || got.suppressed != 0 {
		t.Fatalf("653e deepseek result = %+v, want 0/5/0", got)
	}
}

func TestReplay41d(t *testing.T) {
	got := replayPair(t, "pair_41d_mimo.jsonl", "mimo-v2.6-flash-free", "node:41d")
	t.Logf("41d mimo sent429=%d sent2xx=%d suppressed=%d", got.sent429, got.sent2xx, got.suppressed)
	if got.sent429 > 8 {
		t.Fatalf("41d mimo sent 429 = %d, want <=8 (suppressed=%d)", got.sent429, got.suppressed)
	}
}

func TestReplay4ad(t *testing.T) {
	got := replayPair(t, "pair_4ad_mimo.jsonl", "mimo-v2.6-flash-free", "node:4ad")
	t.Logf("4ad mimo sent429=%d sent2xx=%d suppressed=%d", got.sent429, got.sent2xx, got.suppressed)
	if got.sent429 > 4 {
		t.Fatalf("4ad mimo sent 429 = %d, want <=4 (suppressed=%d)", got.sent429, got.suppressed)
	}
}
