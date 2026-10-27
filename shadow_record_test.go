package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestR8ShadowRecord(t *testing.T) {
	if got := NewShadowRecord().capacity; got != 4096 {
		t.Fatalf("default capacity = %d, want 4096", got)
	}

	r := newShadowRecord(4)
	for i := 1; i <= 6; i++ {
		r.Append(ShadowEntry{
			TS:         int64(i),
			Model:      fmt.Sprintf("m%d", i),
			Egress:     fmt.Sprintf("node:%d", i),
			CandidateN: i,
			Decision:   "admit",
		})
	}
	if got := r.Len(); got != 4 {
		t.Fatalf("Len() = %d, want 4", got)
	}
	snap := r.Snapshot()
	if len(snap) != 4 || snap[0].TS != 3 || snap[1].TS != 4 || snap[2].TS != 5 || snap[3].TS != 6 {
		t.Fatalf("snapshot = %+v, want ts 3..6", snap)
	}

	snap[0].Model = "mutated"
	if again := r.Snapshot(); again[0].Model != "m3" {
		t.Fatal("Snapshot exposed internal storage")
	}

	path := filepath.Join(t.TempDir(), "shadow.jsonl")
	if err := r.AppendJSONL(path); err != nil {
		t.Fatalf("AppendJSONL: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 {
		t.Fatalf("JSONL lines = %d, want 4", len(lines))
	}
	for i, line := range lines {
		var e ShadowEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d invalid JSON: %v", i, err)
		}
		if e.TS != int64(i+3) {
			t.Fatalf("line %d ts = %d, want %d", i, e.TS, i+3)
		}
	}
}
