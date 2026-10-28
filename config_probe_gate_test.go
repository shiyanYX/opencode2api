package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigPatchProbeGateBool(t *testing.T) {
	setupProbeHTTP(t, nil)
	post := func(body string) {
		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
		rr := httptest.NewRecorder()
		adminConfigHandler(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST %s: %d %s", body, rr.Code, rr.Body.String())
		}
	}
	get := func() bool {
		rr := httptest.NewRecorder()
		adminConfigHandler(rr, httptest.NewRequest(http.MethodGet, "/api/config", nil))
		var body struct {
			ProbeCountsTowardGate bool `json:"probe_counts_toward_gate"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.ProbeCountsTowardGate
	}
	onDisk := func() bool {
		p := loadConfig(configPath).ProbeCountsTowardGate
		return p != nil && *p
	}

	post(`{"probe_counts_toward_gate":true}`)
	if got := onDisk(); !got {
		t.Fatal("true not persisted")
	}
	if !get() {
		t.Fatal("GET did not return true")
	}

	post(`{"probe_counts_toward_gate":false}`)
	if got := onDisk(); got {
		t.Fatal("false persisted as true")
	}
	if get() {
		t.Fatal("GET did not return false")
	}

	post(`{"subscriptions":[]}`)
	if onDisk() {
		t.Fatal("subscription-only patch changed probe gate")
	}
	post(`{"probe_counts_toward_gate":true}`)
	post(`{"subscriptions":[]}`)
	if !onDisk() || !get() {
		t.Fatal("subscription-only patch cleared probe gate")
	}
}
