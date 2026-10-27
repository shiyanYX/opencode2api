package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	nodeStateV5Version  = 5
	nodeStateV5MaxPairs = 50000
	nodeStateV5Idle     = 30 * time.Minute
	nodeStateV5TripCap  = 16
)

// NodeStateV5 is the persisted data layer for the v5 node system.
// All timestamps are Unix milliseconds; inflight is deliberately not stored.
type NodeStateV5 struct {
	Version int                     `json:"version"`
	Epoch   string                  `json:"epoch"`
	Pairs   map[string]*PairState   `json:"pairs"`
	Egress  map[string]*EgressState `json:"egress"`
	Direct  *DirectState            `json:"direct"`
	Subs    map[string]*SubState    `json:"subs"`
}

type PairState struct {
	A         float64 `json:"a"`
	B         float64 `json:"b"`
	LastOK    int64   `json:"lastOK"`
	LastFail  int64   `json:"lastFail"`
	LastDecay int64   `json:"lastDecay"`
	Strikes   int     `json:"strikes"`
	Hard      int     `json:"hard"`
	Until     int64   `json:"until"`
	Trips24h  []int64 `json:"trips24h"`
}

type EgressState struct {
	Lifecycle  string `json:"lifecycle"`
	ConsecFail int    `json:"consecFail"`
	ConsecOK   int    `json:"consec_ok"`
	DeadUntil  int64  `json:"deadUntil"`
	LastProbe  int64  `json:"lastProbe"`
}

type DirectState struct {
	Until int64 `json:"until"`
}

type SubState struct {
	Until     int64    `json:"until"`
	Reason    string   `json:"reason,omitempty"`
	N403      int      `json:"n403"`
	Last403   int64    `json:"last403"`
	Pairs     []string `json:"pairs"`
	PairTimes []int64  `json:"pair_times,omitempty"`
}

func NewNodeStateV5(now time.Time) *NodeStateV5 {
	return &NodeStateV5{
		Version: nodeStateV5Version,
		Epoch:   now.UTC().Format(time.RFC3339),
		Pairs:   map[string]*PairState{},
		Egress:  map[string]*EgressState{},
		Direct:  &DirectState{},
		Subs:    map[string]*SubState{},
	}
}

func emptyNodeStateV5() *NodeStateV5 {
	return &NodeStateV5{
		Version: nodeStateV5Version,
		Pairs:   map[string]*PairState{},
		Egress:  map[string]*EgressState{},
		Direct:  &DirectState{},
		Subs:    map[string]*SubState{},
	}
}

// PairKey is the stable key for a (model, egress) pair.
func PairKey(model, egress string) string {
	return model + "|" + egress
}

// LoadNodeStateV5 reads node_state_v5.json. Missing/corrupt/unsupported files
// degrade to an empty v5 state instead of failing startup.
func LoadNodeStateV5(path string) *NodeStateV5 {
	st := emptyNodeStateV5()
	if path == "" {
		return st
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	var loaded NodeStateV5
	if err := json.Unmarshal(data, &loaded); err != nil {
		return st
	}
	if loaded.Version != nodeStateV5Version {
		return st
	}
	if loaded.Pairs == nil {
		loaded.Pairs = map[string]*PairState{}
	}
	if loaded.Egress == nil {
		loaded.Egress = map[string]*EgressState{}
	}
	if loaded.Direct == nil {
		loaded.Direct = &DirectState{}
	}
	if loaded.Subs == nil {
		loaded.Subs = map[string]*SubState{}
	}
	return &loaded
}

// SaveNodeStateV5 atomically writes node_state_v5.json (temp file + rename, 0644).
func SaveNodeStateV5(path string, st *NodeStateV5) error {
	if st == nil {
		return errors.New("nil node state")
	}
	if path == "" {
		return errors.New("empty node state path")
	}
	st.Version = nodeStateV5Version
	if st.Pairs == nil {
		st.Pairs = map[string]*PairState{}
	}
	if st.Egress == nil {
		st.Egress = map[string]*EgressState{}
	}
	if st.Direct == nil {
		st.Direct = &DirectState{}
	}
	if st.Subs == nil {
		st.Subs = map[string]*SubState{}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
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
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename node state: %w", err)
	}
	return nil
}

// pruneLocked trims trips24h, removes idle unblocked pairs, and enforces the
// 50k cap by evicting the oldest unblocked pairs. Blocked pairs (future until,
// strikes>0, hard>0) are never evicted, so the cap can be exceeded if more
// than 50k pairs are blocked.
func (st *NodeStateV5) pruneLocked(now time.Time) int {
	if st == nil {
		return 0
	}
	nowMS := now.UnixMilli()
	idleCutoff := now.Add(-nodeStateV5Idle).UnixMilli()
	tripCutoff := now.Add(-24 * time.Hour).UnixMilli()
	removed := 0
	for key, p := range st.Pairs {
		if p == nil {
			delete(st.Pairs, key)
			removed++
			continue
		}
		p.Trips24h = pruneTrips(p.Trips24h, tripCutoff)
		if p.Until > nowMS || p.Strikes > 0 || p.Hard > 0 {
			continue
		}
		if r8MaxInt64(p.LastOK, p.LastFail) < idleCutoff {
			delete(st.Pairs, key)
			removed++
		}
	}
	if len(st.Pairs) <= nodeStateV5MaxPairs {
		return removed
	}

	type victim struct {
		key  string
		last int64
	}
	victims := make([]victim, 0, len(st.Pairs))
	for key, p := range st.Pairs {
		if p == nil || p.Until > nowMS || p.Strikes > 0 || p.Hard > 0 {
			continue
		}
		victims = append(victims, victim{key: key, last: r8MaxInt64(p.LastOK, p.LastFail)})
	}
	sort.Slice(victims, func(i, j int) bool {
		if victims[i].last != victims[j].last {
			return victims[i].last < victims[j].last
		}
		return victims[i].key < victims[j].key
	})
	for _, v := range victims {
		if len(st.Pairs) <= nodeStateV5MaxPairs {
			break
		}
		delete(st.Pairs, v.key)
		removed++
	}
	return removed
}

func pruneTrips(trips []int64, cutoff int64) []int64 {
	if len(trips) == 0 {
		return nil
	}
	sort.Slice(trips, func(i, j int) bool { return trips[i] < trips[j] })
	kept := make([]int64, 0, len(trips))
	for _, ts := range trips {
		if ts >= cutoff {
			kept = append(kept, ts)
		}
	}
	if len(kept) > nodeStateV5TripCap {
		kept = kept[len(kept)-nodeStateV5TripCap:]
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func r8MaxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
