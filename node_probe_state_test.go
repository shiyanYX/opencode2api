package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProbeStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe_state.json")
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	oldClock := probeClock
	probeClock = func() time.Time { return now }
	t.Cleanup(func() { probeClock = oldClock })

	m := newProbeManager()
	m.configure(path)
	res := probeResult{Fingerprint: "fp1", Class: probeClassOK, Stage: "request", Status: 200, LatencyMS: 42, At: now}
	m.mu.Lock()
	m.results["fp1"] = res
	m.mu.Unlock()
	m.saveState()

	var st probeStateFile
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st.Version != probeStateVersion || len(st.Results) != 1 || st.Results["fp1"].LatencyMS != 42 {
		t.Fatalf("state=%+v", st)
	}

	loaded := newProbeManager()
	loaded.configure(path)
	got, ok := loaded.Snapshot()["fp1"]
	if !ok || got.Class != probeClassOK || got.LatencyMS != 42 || loaded.SavedAt().IsZero() {
		t.Fatalf("loaded=%+v saved=%v", got, loaded.SavedAt())
	}
}

func TestProbeStateCorruptIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe_state.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newProbeManager()
	m.configure(path)
	if got := m.Snapshot(); len(got) != 0 {
		t.Fatalf("corrupt state loaded: %+v", got)
	}
}

func TestProbeStatePrunesSevenDays(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe_state.json")
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	oldClock := probeClock
	probeClock = func() time.Time { return now }
	t.Cleanup(func() { probeClock = oldClock })
	m := newProbeManager()
	m.configure(path)
	m.mu.Lock()
	m.results["old"] = probeResult{Fingerprint: "old", Class: probeClassOK, At: now.Add(-8 * 24 * time.Hour)}
	m.results["new"] = probeResult{Fingerprint: "new", Class: probeClassDenied, At: now.Add(-time.Hour)}
	m.mu.Unlock()
	m.saveState()

	var st probeStateFile
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Results["old"]; ok {
		t.Fatal("8-day-old result was not pruned")
	}
	if _, ok := st.Results["new"]; !ok {
		t.Fatal("fresh result pruned")
	}
}

func TestNodesProbeMetadataAndLoadedAvailability(t *testing.T) {
	p := setupProbeHTTP(t, []*ProxyNode{{Name: "sub::n1", Protocol: "socks5", Address: "10.0.0.1", Port: 1080}})
	fp := p.snapshot()[0].Fingerprint
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	oldClock := probeClock
	probeClock = func() time.Time { return now }
	t.Cleanup(func() { probeClock = oldClock })
	path := probeStatePathFor(configPath)
	m := newProbeManager()
	m.configure(path)
	m.mu.Lock()
	m.results[fp] = probeResult{Fingerprint: fp, Class: probeClassUnreachable, Stage: "preflight", At: now}
	m.mu.Unlock()
	m.saveState()
	nodeProber = newProbeManager()
	nodeProber.configure(path)

	rr := httptest.NewRecorder()
	adminNodesHandler(rr, httptest.NewRequest(http.MethodGet, "/api/nodes", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Nodes      []nodeAdminView `json:"nodes"`
		ProbeModel string          `json:"probe_model"`
		ProbeSaved string          `json:"probe_saved_at"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ProbeModel == "" || resp.ProbeSaved == "" {
		t.Fatalf("metadata model=%q saved=%q", resp.ProbeModel, resp.ProbeSaved)
	}
	for _, n := range resp.Nodes {
		if n.Fingerprint == fp {
			if n.Availability != "unreachable" {
				t.Fatalf("availability=%q", n.Availability)
			}
			return
		}
	}
	t.Fatal("node not found")
}
