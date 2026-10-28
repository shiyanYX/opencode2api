package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestR8StateRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	st := NewNodeStateV5(now)
	pairKey := PairKey("mimo", "node:653e")
	st.Pairs[pairKey] = &PairState{
		A:         9,
		B:         3,
		LastOK:    now.Add(-time.Minute).UnixMilli(),
		LastFail:  now.Add(-2 * time.Minute).UnixMilli(),
		LastDecay: now.Add(-time.Hour).UnixMilli(),
		Strikes:   2,
		Hard:      1,
		Until:     now.Add(5 * time.Minute).UnixMilli(),
		Trips24h:  []int64{now.Add(-time.Hour).UnixMilli()},
	}
	st.Egress["653e"] = &EgressState{
		Lifecycle:  "active",
		ConsecFail: 1,
		DeadUntil:  now.Add(30 * time.Minute).UnixMilli(),
		LastProbe:  now.UnixMilli(),
	}
	st.Direct = &DirectState{Until: now.Add(time.Minute).UnixMilli()}
	st.Subs["red"] = &SubState{
		Until:   now.Add(10 * time.Minute).UnixMilli(),
		Reason:  "403",
		N403:    2,
		Last403: now.UnixMilli(),
		Pairs:   []string{pairKey},
	}

	path := filepath.Join(t.TempDir(), "node_state_v5.json")
	if err := SaveNodeStateV5(path, st, NewAdmission(st, 8, nil)); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := LoadNodeStateV5(path)
	if !reflect.DeepEqual(got, st) {
		t.Fatalf("round trip mismatch:\n got %#v\nwant %#v", got, st)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version int `json:"version"`
		Pairs   map[string]struct {
			LastOK    int64   `json:"lastOK"`
			LastFail  int64   `json:"lastFail"`
			LastDecay int64   `json:"lastDecay"`
			Trips24h  []int64 `json:"trips24h"`
		} `json:"pairs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("json: %v", err)
	}
	if doc.Version != nodeStateV6Version {
		t.Fatalf("version = %d, want %d", doc.Version, nodeStateV6Version)
	}
	p := doc.Pairs[pairKey]
	if p.LastOK == 0 || p.LastFail == 0 || p.LastDecay == 0 || len(p.Trips24h) != 1 {
		t.Fatalf("schema field mismatch: %+v", p)
	}
	if PairKey("m", "node:fp") != "m|node:fp" {
		t.Fatalf("PairKey = %q", PairKey("m", "node:fp"))
	}
}

func TestR8RestartBlocked(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	st := NewNodeStateV5(now)
	key := PairKey("mimo", "node:653e")
	until := now.Add(time.Hour).UnixMilli()
	st.Pairs[key] = &PairState{Strikes: 2, Hard: 1, Until: until}

	path := filepath.Join(t.TempDir(), "node_state_v5.json")
	if err := SaveNodeStateV5(path, st, NewAdmission(st, 8, nil)); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := LoadNodeStateV5(path)
	if got.Pairs[key] == nil {
		t.Fatal("blocked pair lost after reload")
	}
	if got.Pairs[key].Until != until || got.Pairs[key].Strikes != 2 || got.Pairs[key].Hard != 1 {
		t.Fatalf("blocked state changed on reload: %+v", got.Pairs[key])
	}

	got.pruneLocked(now.Add(2 * time.Hour))
	if got.Pairs[key] == nil {
		t.Fatal("blocked pair pruned after cooldown expiry")
	}
}

func TestR8MemoryCap(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	st := NewNodeStateV5(now)
	for i := 0; i < 50100; i++ {
		key := fmt.Sprintf("active|node:%05d", i)
		st.Pairs[key] = &PairState{
			LastOK: now.Add(-time.Duration(i) * time.Millisecond).UnixMilli(),
		}
	}
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("blocked|node:%02d", i)
		st.Pairs[key] = &PairState{
			LastOK:  now.UnixMilli(),
			Strikes: 1,
			Until:   now.Add(time.Hour).UnixMilli(),
		}
	}

	st.pruneLocked(now)
	if got := len(st.Pairs); got != nodeStateV5MaxPairs {
		t.Fatalf("pairs = %d, want exactly %d", got, nodeStateV5MaxPairs)
	}
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("blocked|node:%02d", i)
		if st.Pairs[key] == nil {
			t.Fatalf("blocked pair %s was evicted", key)
		}
	}
	if st.Pairs["active|node:00000"] == nil || st.Pairs["active|node:49979"] == nil {
		t.Fatal("newest active pairs were evicted")
	}
	for i := 49980; i < 50100; i++ {
		key := fmt.Sprintf("active|node:%05d", i)
		if st.Pairs[key] != nil {
			t.Fatalf("oldest active pair %s survived", key)
		}
	}
}

func TestR8CorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node_state_v5.json")
	if err := os.WriteFile(path, []byte(`{"version":5,"pairs":`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadNodeStateV5(path)
	if got == nil || got.Version != nodeStateV6Version || len(got.Pairs) != 0 || len(got.Egress) != 0 {
		t.Fatalf("corrupt load = %#v, want empty v5 state", got)
	}

	if err := os.WriteFile(path, []byte(`{"version":4,"pairs":{"x":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got = LoadNodeStateV5(path)
	if got == nil || got.Version != nodeStateV6Version || len(got.Pairs) != 0 {
		t.Fatalf("version mismatch load = %#v, want empty v5 state", got)
	}
}
