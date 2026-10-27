package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestR8Migration190(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	src := newNodeScoringStore(filepath.Join(dir, scoringStateFileName))
	legacy := persistentState{Version: proxyStateVersion, Nodes: map[string]nodeRuntimeState{}}

	for i := 0; i < 190; i++ {
		fp := fmt.Sprintf("fp%03d", i)
		legacy.Nodes[fp] = nodeRuntimeState{
			State:         "exhausted",
			CooldownUntil: now.Add(-time.Minute),
			LastError:     "quota",
		}
		src.nodes[fp] = &scoringStats{Name: fp, QuotaEvents: 1}
	}
	legacy.Nodes["deadfp"] = nodeRuntimeState{
		State:         "dead",
		CooldownUntil: now.Add(time.Hour),
	}
	legacy.Nodes["rejfp"] = nodeRuntimeState{
		State:     "available",
		GateState: GateStateRejected,
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "proxy_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	got := MigrateV2(src, now)
	if got == nil || got.Version != 5 {
		t.Fatalf("migrate returned %#v", got)
	}
	if len(got.Pairs) != 190 {
		t.Fatalf("pairs = %d, want 190", len(got.Pairs))
	}
	wantUntil := now.Add(5 * time.Minute).UnixMilli()
	for i := 0; i < 190; i++ {
		fp := fmt.Sprintf("fp%03d", i)
		key := PairKey("*", "node:"+fp)
		p := got.Pairs[key]
		if p == nil {
			t.Fatalf("missing pair %s", key)
		}
		if p.Strikes != 1 || p.Until != wantUntil {
			t.Fatalf("%s = %+v, want strikes=1 until=%d", key, p, wantUntil)
		}
		if got.Egress[fp] == nil || got.Egress[fp].Lifecycle != "active" {
			t.Fatalf("egress %s = %+v", fp, got.Egress[fp])
		}
	}
	if got.Egress["deadfp"] == nil || got.Egress["deadfp"].Lifecycle != "dead" {
		t.Fatalf("dead egress = %+v", got.Egress["deadfp"])
	}
	if got.Egress["rejfp"] == nil || got.Egress["rejfp"].Lifecycle != "rejected" {
		t.Fatalf("rejected egress = %+v", got.Egress["rejfp"])
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "shape_drift") {
		t.Fatal("shape_drift state leaked into v5")
	}
	if got.Epoch == "" {
		t.Fatal("epoch is empty")
	}

	again := MigrateV2(src, now)
	if !reflect.DeepEqual(got, again) {
		t.Fatal("migration is not idempotent")
	}
}
