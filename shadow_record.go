package main

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
)

const shadowRecordCapacity = 4096

type ShadowEntry struct {
	TS         int64  `json:"ts"`
	Model      string `json:"model"`
	Egress     string `json:"egress"`
	CandidateN int    `json:"candidate_n"`
	Decision   string `json:"decision"`
	Reason     string `json:"reason,omitempty"`
}

// ShadowRecord is a bounded, lock-protected ring of shadow decisions.
// It never participates in routing.
type ShadowRecord struct {
	mu       sync.Mutex
	entries  []ShadowEntry
	next     int
	full     bool
	capacity int
}

func NewShadowRecord() *ShadowRecord {
	return newShadowRecord(shadowRecordCapacity)
}

func newShadowRecord(capacity int) *ShadowRecord {
	if capacity <= 0 {
		capacity = shadowRecordCapacity
	}
	return &ShadowRecord{
		entries:  make([]ShadowEntry, 0, capacity),
		capacity: capacity,
	}
}

func (r *ShadowRecord) Append(e ShadowEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) < r.capacity {
		r.entries = append(r.entries, e)
		return
	}
	r.entries[r.next] = e
	r.next = (r.next + 1) % r.capacity
	r.full = true
}

func (r *ShadowRecord) Snapshot() []ShadowEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ShadowEntry, 0, len(r.entries))
	if r.full {
		out = append(out, r.entries[r.next:]...)
		out = append(out, r.entries[:r.next]...)
		return out
	}
	out = append(out, r.entries...)
	return out
}

func (r *ShadowRecord) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func (r *ShadowRecord) AppendJSONL(path string) error {
	if path == "" {
		return errors.New("empty shadow record path")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range r.Snapshot() {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}
