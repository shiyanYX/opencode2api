package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWriteV5Failure(t *testing.T) {
	cases := []struct {
		reason string
		retry  time.Duration
		want   string
	}{
		{"quota", 600 * time.Second, "quota_exhausted"},
		{"gate", 60 * time.Second, "gate"},
		{"no_candidate", time.Second, "no_candidate"},
	}
	for _, tc := range cases {
		status, h, err := v5NoCandidateHTTP(EgressResult{NoCandidateReason: tc.reason, RetryAfter: tc.retry})
		rec := httptest.NewRecorder()
		if !writeV5Failure(rec, status, h, err) {
			t.Fatalf("%s: not recognized as v5 failure", tc.reason)
		}
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("%s: status=%d", tc.reason, rec.Code)
		}
		wantRA := map[string]string{"quota": "600", "gate": "60", "no_candidate": "1"}[tc.reason]
		if got := rec.Header().Get("Retry-After"); got != wantRA {
			t.Fatalf("%s: Retry-After=%q want %q", tc.reason, got, wantRA)
		}
		var body struct {
			Error struct {
				Type       string `json:"type"`
				RetryAfter int    `json:"retry_after_s"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body=%s", tc.reason, rec.Body.String())
		}
		if body.Error.Type != tc.want || body.Error.RetryAfter < 1 {
			t.Fatalf("%s: body=%s", tc.reason, rec.Body.String())
		}
	}
}

func TestWriteV5FailureClaude(t *testing.T) {
	status, h, err := v5NoCandidateHTTP(EgressResult{NoCandidateReason: "quota", RetryAfter: 600 * time.Second})
	rec := httptest.NewRecorder()
	if !writeV5FailureClaude(rec, status, h, err) {
		t.Fatal("not recognized")
	}
	if got := rec.Header().Get("Retry-After"); got != "600" {
		t.Fatalf("Retry-After=%q", got)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Type != "error" || body.Error.Type != "quota_exhausted" {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestWriteV5FailureIgnoresNonV5(t *testing.T) {
	rec := httptest.NewRecorder()
	if writeV5Failure(rec, 429, nil, nil) {
		t.Fatal("nil error recognized as v5")
	}
}
