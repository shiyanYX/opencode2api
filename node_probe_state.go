package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	probeStateVersion = 1
	probeStateMax     = 2000
	probeStateTTL     = 7 * 24 * time.Hour
)

type probeStateFile struct {
	Version int                    `json:"version"`
	SavedAt time.Time              `json:"saved_at"`
	Results map[string]probeResult `json:"results"`
}

func probeStatePathFor(configPath string) string {
	if configPath == "" {
		return "probe_state.json"
	}
	return filepath.Join(filepath.Dir(configPath), "probe_state.json")
}

// configure points the manager at its persistent file and loads it. A missing,
// corrupt, or version-mismatched file is a warning + empty state, never fatal.
func (m *probeManager) configure(path string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.statePath = path
	m.mu.Unlock()
	if path == "" {
		return
	}
	m.loadState(path)
}

func (m *probeManager) loadState(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("probe state load failed", "path", path, "error", err)
		}
		return
	}
	var st probeStateFile
	if err := json.Unmarshal(b, &st); err != nil || st.Version != probeStateVersion {
		slog.Warn("probe state ignored", "path", path, "error", err, "version", st.Version)
		return
	}
	now := probeClock()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.savedAt = st.SavedAt
	for fp, r := range st.Results {
		if fp == "" || r.Fingerprint == "" {
			continue
		}
		if now.Sub(r.At) > probeStateTTL {
			continue
		}
		m.results[fp] = r
	}
	m.trimLocked(now)
}

func (m *probeManager) trimLocked(now time.Time) {
	for fp, r := range m.results {
		if !r.At.IsZero() && now.Sub(r.At) > probeStateTTL {
			delete(m.results, fp)
		}
	}
	if len(m.results) <= probeStateMax {
		return
	}
	type item struct {
		fp string
		at time.Time
	}
	items := make([]item, 0, len(m.results))
	for fp, r := range m.results {
		items = append(items, item{fp, r.At})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].at.After(items[j].at) })
	for _, it := range items[probeStateMax:] {
		delete(m.results, it.fp)
	}
}

func (m *probeManager) saveState() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.statePath == "" {
		m.mu.Unlock()
		return
	}
	now := probeClock()
	m.trimLocked(now)
	m.savedAt = now
	st := probeStateFile{Version: probeStateVersion, SavedAt: now, Results: make(map[string]probeResult, len(m.results))}
	for fp, r := range m.results {
		st.Results[fp] = r
	}
	path := m.statePath
	m.mu.Unlock()

	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		slog.Warn("probe state marshal failed", "error", err)
		return
	}
	if err := atomicWriteFile(path, data, 0o644); err != nil {
		slog.Warn("probe state save failed", "path", path, "error", err)
	}
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	if path == "" {
		return fmt.Errorf("empty path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

func (m *probeManager) SavedAt() time.Time {
	if m == nil {
		return time.Time{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.savedAt
}
