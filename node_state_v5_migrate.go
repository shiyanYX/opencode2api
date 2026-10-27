package main

import (
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// legacyPairModel is the wildcard model used when migrating a v2 exhausted
// node, because the old proxy_state has no per-model dimension.
//
// `*|node:<fp>` is a v2 placeholder only. N2's Admit falls back to this key
// when no model-specific pair exists; the first real outcome for that egress
// deletes the placeholder and writes the actual model pair.
const legacyPairModel = "*"

// MigrateV2 converts the legacy proxy_state (NodeExhausted/dead/rejected) and
// the scoring quota evidence into a fresh v5 state. The scoring store supplies
// the directory of proxy_state.json and per-fingerprint QuotaEvents; legacy
// shape_drift state is intentionally ignored.
func MigrateV2(src *nodeScoringStore, now time.Time) *NodeStateV5 {
	st := NewNodeStateV5(now)
	if src == nil {
		return st
	}

	proxyPath := "proxy_state.json"
	if src.path != "" {
		proxyPath = filepath.Join(filepath.Dir(src.path), "proxy_state.json")
	}
	legacy, ok := readPersistentState(proxyPath)
	if !ok || len(legacy.Nodes) == 0 {
		return st
	}

	keys := make([]string, 0, len(legacy.Nodes))
	for fp := range legacy.Nodes {
		keys = append(keys, fp)
	}
	sort.Strings(keys)

	for _, fp := range keys {
		rs := legacy.Nodes[fp]
		lifecycle := "active"
		switch {
		case strings.EqualFold(rs.State, "dead"):
			lifecycle = "dead"
		case rs.GateState == GateStateRejected:
			lifecycle = "rejected"
		}
		st.Egress[fp] = &EgressState{
			Lifecycle:  lifecycle,
			DeadUntil:  msOrZero(rs.CooldownUntil),
			LastProbe:  msOrZero(rs.LastProbeAt),
			ConsecFail: rs.ProbationFailures,
		}

		if !strings.EqualFold(rs.State, "exhausted") {
			continue
		}
		if !legacyQuota(src, fp) && !legacyQuotaText(rs.LastError) {
			continue
		}
		key := PairKey(legacyPairModel, "node:"+fp)
		st.Pairs[key] = &PairState{
			A:        8,
			B:        2,
			LastFail: now.UnixMilli(),
			Strikes:  1,
			Until:    now.Add(5 * time.Minute).UnixMilli(),
		}
	}
	return st
}

func legacyQuota(src *nodeScoringStore, fp string) bool {
	src.mu.Lock()
	defer src.mu.Unlock()
	ss := src.nodes[fp]
	return ss != nil && ss.QuotaEvents > 0
}

func legacyQuotaText(lastError string) bool {
	d := strings.ToLower(lastError)
	return strings.Contains(d, "quota") ||
		strings.Contains(d, "429") ||
		strings.Contains(d, "freetier") ||
		strings.Contains(d, "freeusage") ||
		strings.Contains(d, "feusage")
}

func msOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
