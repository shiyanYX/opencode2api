package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type granularityClock struct{ now time.Time }

func (c granularityClock) Now() time.Time { return c.now }

func TestQuotaStateRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	st := NewNodeStateV5(now)
	st.Quota["sub-a|mimo"] = &QuotaState{
		Until: now.Add(10 * time.Minute).UnixMilli(),
		Since: now.Add(-5 * time.Minute).UnixMilli(),
		Seen:  map[string]int64{"node:a": now.UnixMilli()},
		Hits:  3,
		Nodes: 7,
	}
	st.Gate.CanaryLevel = 3
	st.Gate.NodeProbeUsed = true

	path := filepath.Join(t.TempDir(), "node_state_v5.json")
	adm := NewAdmission(st, 8, nil)
	if err := SaveNodeStateV5(path, st, adm); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["quota"]; !ok {
		t.Fatal("quota field missing")
	}
	got := LoadNodeStateV5(path)
	q := got.Quota["sub-a|mimo"]
	if q == nil || q.Hits != 3 || q.Nodes != 7 || q.Seen["node:a"] == 0 {
		t.Fatalf("quota lost: %+v", q)
	}
	if got.Gate.CanaryLevel != 3 || !got.Gate.NodeProbeUsed {
		t.Fatalf("gate fields lost: %+v", got.Gate)
	}
}

func TestNodeStateV5OldStateMigration(t *testing.T) {
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	old := map[string]any{
		"version": 5,
		"epoch":   "old",
		"pairs": map[string]any{
			"m|node:a": map[string]any{"a": 4.0, "b": 1.0, "strikes": 0, "until": 0},
			"m|node:b": map[string]any{"a": 8.0, "b": 2.0, "strikes": 1, "until": now.Add(time.Hour).UnixMilli()},
			"m|direct": map[string]any{"a": 8.0, "b": 2.0},
			"m|node:c": map[string]any{"a": 1.0, "b": 1.0},
			"m|node:d": map[string]any{"a": 8.0, "b": 2.0, "strikes": 1, "until": 0},
		},
		"egress": map[string]any{"node:a": map[string]any{"lifecycle": "active"}},
		"gate":   map[string]any{"active": true, "canaryLevel": 2},
		"subs":   map[string]any{"old": map[string]any{"until": 1}},
	}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "v5_state.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadNodeStateV5(path)
	if got.Version != nodeStateV6Version {
		t.Fatalf("version=%d, want %d", got.Version, nodeStateV6Version)
	}
	if len(got.Quota) != 0 || got.Quota == nil {
		t.Fatalf("quota=%v, want empty non-nil", got.Quota)
	}
	if len(got.Subs) != 0 {
		t.Fatalf("subs=%v, want empty", got.Subs)
	}
	if len(got.Pairs) != 1 || got.Pairs["*|node:a"] == nil {
		t.Fatalf("pairs=%v, want only *|node:a", got.Pairs)
	}
	if !got.Gate.Active || got.Gate.CanaryLevel != 2 {
		t.Fatalf("gate lost: %+v", got.Gate)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "v5_state.v5.bak")); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
}

func TestQuotaCapEvictsExpired(t *testing.T) {
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	SetClock(granularityClock{now: now})
	defer SetClock(nil)
	st := NewNodeStateV5(now)
	quotaDropped.Store(0)
	for i := 0; i < nodeStateV6MaxQuota; i++ {
		st.Quota[quotaKey("s", string(rune(i)))] = &QuotaState{
			Until: now.Add(-time.Hour).UnixMilli(),
			Since: now.Add(-time.Hour).UnixMilli(),
		}
	}
	if !st.setQuotaLocked("s|new", &QuotaState{Until: now.Add(time.Minute).UnixMilli(), Since: now.UnixMilli()}) {
		t.Fatal("expired quota should be evicted to make room")
	}
	if len(st.Quota) != 1 {
		t.Fatalf("len=%d, want 1 after pruning all expired", len(st.Quota))
	}
	if st.Quota["s|new"] == nil {
		t.Fatal("new quota missing")
	}
	if quotaDropped.Load() != 0 {
		t.Fatalf("dropped=%d", quotaDropped.Load())
	}
}

func TestQuotaCapRejectsActive(t *testing.T) {
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	SetClock(granularityClock{now: now})
	defer SetClock(nil)
	st := NewNodeStateV5(now)
	quotaDropped.Store(0)
	for i := 0; i < nodeStateV6MaxQuota; i++ {
		st.Quota[quotaKey("s", string(rune(i)))] = &QuotaState{
			Until: now.Add(time.Hour).UnixMilli(),
			Since: now.UnixMilli(),
		}
	}
	if st.setQuotaLocked("s|new", &QuotaState{Until: now.Add(time.Minute).UnixMilli()}) {
		t.Fatal("active cap must reject new key")
	}
	if len(st.Quota) != nodeStateV6MaxQuota {
		t.Fatalf("len=%d", len(st.Quota))
	}
	if quotaDropped.Load() != 1 {
		t.Fatalf("dropped=%d, want 1", quotaDropped.Load())
	}
	if !st.setQuotaLocked(quotaKey("s", string(rune(0))), &QuotaState{Until: now.Add(2 * time.Hour).UnixMilli()}) {
		t.Fatal("existing key must remain updatable")
	}
}

func TestNodeStateV5SaveGuard(t *testing.T) {
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	st := NewNodeStateV5(now)
	path := filepath.Join(t.TempDir(), "node_state_v5.json")
	if err := SaveNodeStateV5(path, st, nil); err == nil {
		t.Fatal("nil admission must fail")
	}
	adm := NewAdmission(st, 8, nil)
	st.migrationFailed = true
	if err := SaveNodeStateV5(path, st, adm); err == nil {
		t.Fatal("migrationFailed must fail")
	}
	st.migrationFailed = false
	if err := SaveNodeStateV5(path, st, adm); err != nil {
		t.Fatalf("save: %v", err)
	}
}

func TestNodeStateV5Helpers(t *testing.T) {
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	SetClock(granularityClock{now: now})
	defer SetClock(nil)
	st := NewNodeStateV5(now)
	st.Pairs["m|node:a"] = &PairState{Until: now.Add(30 * time.Second).UnixMilli()}
	st.Pairs["*|node:b"] = &PairState{Until: now.Add(2 * time.Minute).UnixMilli()}
	st.Egress["node:a"] = &EgressState{DeadUntil: now.Add(3 * time.Minute).UnixMilli()}
	st.Quota["s|m"] = &QuotaState{Until: now.Add(10 * time.Minute).UnixMilli()}
	st.Quota["s|other"] = &QuotaState{Until: now.Add(20 * time.Minute).UnixMilli()}
	st.Direct = &DirectState{Until: now.Add(4 * time.Minute).UnixMilli()}
	st.Subs["s"] = &SubState{Until: now.Add(5 * time.Minute).UnixMilli()}
	a := NewAdmission(st, 8, nil)

	cases := []struct {
		name string
		got  int64
		want int64
	}{
		{"pair", a.pairUntilLocked("m", "node:a"), now.Add(30 * time.Second).UnixMilli()},
		{"pair wildcard", a.pairUntilLocked("m", "node:b"), now.Add(2 * time.Minute).UnixMilli()},
		{"pair none", a.pairUntilLocked("x", "node:z"), 0},
		{"dead", a.egressDeadUntil("node:a"), now.Add(3 * time.Minute).UnixMilli()},
		{"quota", a.quotaUntilLocked("s", "m"), now.Add(10 * time.Minute).UnixMilli()},
		{"quota empty sub", a.quotaUntilLocked("", "m"), 0},
		{"direct", a.directUntilLocked(), now.Add(4 * time.Minute).UnixMilli()},
		{"sub", a.subUntilLocked("s"), now.Add(5 * time.Minute).UnixMilli()},
		{"min quota", a.minQuotaUntilLocked("m"), now.Add(10 * time.Minute).UnixMilli()},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s=%d, want %d", tc.name, tc.got, tc.want)
		}
	}
	if d, reason := a.noCandidateRetry("m"); d < 9*time.Minute || d > 10*time.Minute+time.Second || reason != "no_candidate" {
		t.Fatalf("retry=%v reason=%s", d, reason)
	}
	if d, reason := a.noCandidateRetry("none"); d != 60*time.Second || reason != "no_candidate" {
		t.Fatalf("empty retry=%v reason=%s", d, reason)
	}
	var nilAdm *Admission
	if nilAdm.pairUntilLocked("m", "node:a") != 0 || nilAdm.quotaUntilLocked("s", "m") != 0 {
		t.Fatal("nil admission helpers must be zero")
	}
}

func TestNodeStateV5MigrationDropCount(t *testing.T) {
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	old := NewNodeStateV5(now)
	old.Pairs["m|node:healthy"] = &PairState{A: 8, B: 2}
	old.Pairs["m|node:cooling"] = &PairState{A: 8, B: 2, Until: now.Add(time.Hour).UnixMilli()}
	got, dropped := migrateV5State(old, now)
	if dropped != 1 {
		t.Fatalf("dropped=%d, want 1", dropped)
	}
	if len(got.Pairs) != 1 || got.Pairs["*|node:healthy"] == nil {
		t.Fatalf("pairs=%v, want only healthy", got.Pairs)
	}
}
