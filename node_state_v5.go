package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

const (
	nodeStateV6Version  = 6
	nodeStateV5Version  = nodeStateV6Version
	nodeStateV6MaxQuota = 4096
	nodeStateV6MaxSeen  = 64
	nodeStateV5MaxPairs = 50000
	nodeStateV5Idle     = 30 * time.Minute
	nodeStateV5TripCap  = 16
)

// quotaDropped counts new quota keys rejected because all 4096 slots are
// active. It is intentionally package-level and atomic, never a writable global.
var quotaDropped atomic.Int64

func quotaKey(sub, model string) string { return sub + "|" + model }

type QuotaState struct {
	Until int64            `json:"until"`
	Since int64            `json:"since"`
	Seen  map[string]int64 `json:"seen"`
	Hits  int              `json:"hits"`
	Nodes int              `json:"nodes"`
}

// NodeStateV5 is the persisted data layer for the v5 node system.
// All timestamps are Unix milliseconds; inflight is deliberately not stored.
type NodeStateV5 struct {
	Version int                     `json:"version"`
	Epoch   string                  `json:"epoch"`
	Pairs   map[string]*PairState   `json:"pairs"`
	Egress  map[string]*EgressState `json:"egress"`
	Direct  *DirectState            `json:"direct"`
	Subs    map[string]*SubState    `json:"subs"`
	Gate    *GateState              `json:"gate"`
	Quota   map[string]*QuotaState  `json:"quota"`

	migrationFailed bool
}

type PairState struct {
	A         float64 `json:"a"`
	B         float64 `json:"b"`
	LastOK    int64   `json:"lastOK"`
	LastFail  int64   `json:"lastFail"`
	Last403   int64   `json:"last403"`
	LastDecay int64   `json:"lastDecay"`
	Strikes   int     `json:"strikes"`
	Strike    int     `json:"strike"`
	Hard      int     `json:"hard"`
	Until     int64   `json:"until"`
	OKStreak  int     `json:"okStreak"`
	Trips24h  []int64 `json:"trips24h"`
}

// GateState is the persisted pool-wide 429 winter gate. Egresses maps an
// egress key to the last 429 timestamp inside the current 5-minute window.
type GateState struct {
	Active        bool             `json:"active"`
	PausedUntil   int64            `json:"pausedUntil"`
	Backoff       int              `json:"backoff"`
	LastTrip      int64            `json:"lastTrip"`
	Egresses      map[string]int64 `json:"egresses,omitempty"`
	NextCanary    int64            `json:"nextCanary"`
	CanaryOK      bool             `json:"canaryOK"`
	CanaryLevel   int              `json:"canaryLevel"`
	Epoch         int64            `json:"epoch"`
	NodeProbeUsed bool             `json:"nodeProbeUsed"`
	// 5 分钟窗口计数器：B1 触发必须同时满足 distinct >=3、样本 >=10、失败率 >=50%。
	WindowStart int64 `json:"windowStart"`
	Attempts    int   `json:"attempts"`
	Failures    int   `json:"failures"`
	// NextNodeProbe 是 direct canary 长期失败时的时间兜底节点探测时间。
	NextNodeProbe int64 `json:"nextNodeProbe"`
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
		Gate:    &GateState{Egresses: map[string]int64{}},
		Quota:   map[string]*QuotaState{},
	}
}

func emptyNodeStateV5() *NodeStateV5 {
	return &NodeStateV5{
		Version: nodeStateV5Version,
		Pairs:   map[string]*PairState{},
		Egress:  map[string]*EgressState{},
		Direct:  &DirectState{},
		Subs:    map[string]*SubState{},
		Gate:    &GateState{Egresses: map[string]int64{}},
		Quota:   map[string]*QuotaState{},
	}
}

// PairKey is the stable key for a (model, egress) pair.
func PairKey(model, egress string) string {
	return model + "|" + egress
}

// LoadNodeStateV5 reads node_state_v5.json. Missing/corrupt/unsupported files
// degrade to an empty v5 state instead of failing startup.
func LoadNodeStateV5(path string) *NodeStateV5 {
	st, _ := loadNodeStateV5(path)
	return st
}

// loadNodeStateV5 returns the load error so the runtime can emit one event on
// corrupt/unsupported state. A missing file is not an error. Version 5 files
// are migrated to v6 and backed up before being rewritten.
func loadNodeStateV5(path string) (*NodeStateV5, error) {
	st := emptyNodeStateV5()
	if path == "" {
		return st, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		st.migrationFailed = true
		return st, err
	}
	var meta struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		st.migrationFailed = true
		return st, err
	}
	switch meta.Version {
	case nodeStateV6Version:
		var loaded NodeStateV5
		if err := json.Unmarshal(data, &loaded); err != nil {
			st.migrationFailed = true
			return st, err
		}
		loaded.Version = nodeStateV6Version
		initNodeStateV5(&loaded)
		return &loaded, nil
	case 5:
		return migrateV5File(path, data, st)
	default:
		st.migrationFailed = true
		return st, fmt.Errorf("node state version %d, want %d", meta.Version, nodeStateV6Version)
	}
}

func initNodeStateV5(st *NodeStateV5) {
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
	if st.Gate == nil {
		st.Gate = &GateState{}
	}
	if st.Gate.Egresses == nil {
		st.Gate.Egresses = map[string]int64{}
	}
	if st.Quota == nil {
		st.Quota = map[string]*QuotaState{}
	}
}

func migrateV5File(path string, data []byte, failed *NodeStateV5) (*NodeStateV5, error) {
	var old NodeStateV5
	if err := json.Unmarshal(data, &old); err != nil {
		failed.migrationFailed = true
		return failed, err
	}
	out, dropped := migrateV5State(&old, Now())
	if dropped > 0 {
		slog.Warn("v5_migrate_dropped_pairs", "count", dropped)
	}
	backup := v5BackupPath(path)
	if err := os.WriteFile(backup, data, 0o644); err != nil {
		failed.migrationFailed = true
		return failed, fmt.Errorf("backup v5 state: %w", err)
	}
	if err := writeNodeStateFile(path, out); err != nil {
		failed.migrationFailed = true
		return failed, err
	}
	return out, nil
}

func v5BackupPath(path string) string {
	if strings.HasSuffix(path, ".json") {
		return strings.TrimSuffix(path, ".json") + ".v5.bak"
	}
	return path + ".v5.bak"
}

func migrateV5State(old *NodeStateV5, now time.Time) (*NodeStateV5, int) {
	out := emptyNodeStateV5()
	out.Epoch = old.Epoch
	out.Egress = old.Egress
	if out.Egress == nil {
		out.Egress = map[string]*EgressState{}
	}
	out.Direct = old.Direct
	if out.Direct == nil {
		out.Direct = &DirectState{}
	}
	out.Gate = old.Gate
	if out.Gate == nil {
		out.Gate = &GateState{}
	}
	if out.Gate.Egresses == nil {
		out.Gate.Egresses = map[string]int64{}
	}
	out.Subs = map[string]*SubState{}
	out.Quota = map[string]*QuotaState{}
	nowMS := now.UnixMilli()
	dropped := 0
	for key, p := range old.Pairs {
		if p == nil {
			dropped++
			continue
		}
		idx := strings.LastIndex(key, "|")
		if idx < 0 {
			dropped++
			continue
		}
		egress := key[idx+1:]
		if egress == "direct" || p.Until > nowMS || p.Strikes != 0 || p.A+p.B < 3 {
			dropped++
			continue
		}
		nk := PairKey(legacyPairModel, egress)
		dst := out.Pairs[nk]
		if dst == nil {
			dst = &PairState{}
			out.Pairs[nk] = dst
		}
		dst.A += p.A
		dst.B += p.B
		if p.LastOK > dst.LastOK {
			dst.LastOK = p.LastOK
		}
		if p.LastFail > dst.LastFail {
			dst.LastFail = p.LastFail
		}
		if p.LastDecay > dst.LastDecay {
			dst.LastDecay = p.LastDecay
		}
	}
	return out, dropped
}

// SaveNodeStateV5 atomically writes node_state_v5.json (temp file + rename, 0644).
// A nil admission or a failed migration is a hard error: callers must never
// silently replace a state file they failed to read/migrate.
func SaveNodeStateV5(path string, st *NodeStateV5, admission *Admission) error {
	if admission == nil {
		return errors.New("nil admission")
	}
	if st == nil {
		return errors.New("nil node state")
	}
	if st.migrationFailed {
		return errors.New("node state migration failed; refusing to save")
	}
	return writeNodeStateFile(path, st)
}

func writeNodeStateFile(path string, st *NodeStateV5) error {
	if st == nil {
		return errors.New("nil node state")
	}
	if path == "" {
		return errors.New("empty node state path")
	}
	st.Version = nodeStateV6Version
	initNodeStateV5(st)
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

// setQuotaLocked inserts or updates one quota key. It returns false when a new
// key is rejected because all slots are held by active gates.
func (st *NodeStateV5) setQuotaLocked(key string, q *QuotaState) bool {
	return st.setQuotaLockedAt(key, q, Now().UnixMilli())
}

func (st *NodeStateV5) setQuotaLockedAt(key string, q *QuotaState, now int64) bool {
	if st == nil || key == "" || q == nil {
		return false
	}
	if st.Quota == nil {
		st.Quota = map[string]*QuotaState{}
	}
	if q.Seen == nil {
		q.Seen = map[string]int64{}
	}
	window := int64((10 * time.Minute) / time.Millisecond)
	trimQuotaSeen(q)
	if _, exists := st.Quota[key]; exists {
		st.Quota[key] = q
		return true
	}
	// Always clear fully expired windows before applying the cap.
	for k, v := range st.Quota {
		if v != nil && v.Until <= now && v.Since+window <= now {
			delete(st.Quota, k)
		}
	}
	if len(st.Quota) < nodeStateV6MaxQuota {
		st.Quota[key] = q
		return true
	}
	// Then evict only an expired gate, oldest Since first. Never evict active.
	var oldestKey string
	var oldestSince int64
	for k, v := range st.Quota {
		if v == nil || v.Until > now {
			continue
		}
		if oldestKey == "" || v.Since < oldestSince {
			oldestKey, oldestSince = k, v.Since
		}
	}
	if oldestKey != "" {
		delete(st.Quota, oldestKey)
		st.Quota[key] = q
		return true
	}
	quotaDropped.Add(1)
	return false
}

func trimQuotaSeen(q *QuotaState) {
	if q == nil {
		return
	}
	for len(q.Seen) > nodeStateV6MaxSeen {
		var oldestKey string
		var oldest int64
		for k, ts := range q.Seen {
			if oldestKey == "" || ts < oldest {
				oldestKey, oldest = k, ts
			}
		}
		delete(q.Seen, oldestKey)
	}
}

// QuotaDropped returns the number of quota events dropped by the cap.
func QuotaDropped() int64 { return quotaDropped.Load() }

func (a *Admission) pairUntilLocked(model, egress string) int64 {
	if a == nil || a.st == nil || egress == "" {
		return 0
	}
	if p := a.st.Pairs[PairKey(model, egress)]; p != nil {
		return p.Until
	}
	if p := a.st.Pairs[PairKey(legacyPairModel, egress)]; p != nil {
		return p.Until
	}
	return 0
}

func (a *Admission) egressDeadUntil(egress string) int64 {
	if a == nil || a.st == nil || egress == "" {
		return 0
	}
	if e := a.st.Egress[egress]; e != nil {
		return e.DeadUntil
	}
	return 0
}

func (a *Admission) quotaUntilLocked(sub, model string) int64 {
	if a == nil || a.st == nil || sub == "" {
		return 0
	}
	if q := a.st.Quota[quotaKey(sub, model)]; q != nil {
		return q.Until
	}
	return 0
}

func (a *Admission) directUntilLocked() int64 {
	if a == nil || a.st == nil || a.st.Direct == nil {
		return 0
	}
	return a.st.Direct.Until
}

func (a *Admission) subUntilLocked(sub string) int64 {
	if a == nil || a.st == nil || sub == "" {
		return 0
	}
	if s := a.st.Subs[sub]; s != nil {
		return s.Until
	}
	return 0
}

func (a *Admission) minQuotaUntilLocked(model string) int64 {
	if a == nil || a.st == nil || model == "" {
		return 0
	}
	var min int64
	for key, q := range a.st.Quota {
		idx := strings.LastIndex(key, "|")
		if idx < 0 || key[idx+1:] != model || q == nil || q.Until == 0 {
			continue
		}
		if min == 0 || q.Until < min {
			min = q.Until
		}
	}
	return min
}

func (a *Admission) noCandidateRetry(model string) (time.Duration, string) {
	minUntil := a.minQuotaUntilLocked(model)
	if minUntil == 0 {
		return 60 * time.Second, "no_candidate"
	}
	now := Now()
	if a != nil {
		now = a.now()
	}
	d := time.Duration(minUntil-now.UnixMilli()) * time.Millisecond
	if d < time.Second {
		d = time.Second
	}
	return d, "no_candidate"
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
		if p.Until > nowMS || p.Strikes > 0 {
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
