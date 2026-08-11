package main

import (
	"net/http/httptest"
	"testing"
)

func TestExtractUpstreamAuthUnifiedKey(t *testing.T) {
	configMu.Lock()
	old := apiKey
	apiKey = "unified-gateway-key-123"
	configMu.Unlock()
	defer func() {
		configMu.Lock()
		apiKey = old
		configMu.Unlock()
	}()

	cases := []struct {
		name     string
		auth     string
		key      string
		wantMode AuthRouteMode
		wantSrc  string
	}{
		{"no key -> public", "", "", AuthRoutePublic, "none"},
		{"bearer public placeholder -> public", "Bearer public", "", AuthRoutePublic, "authorization"},
		{"unified key -> public (gateway-only)", "Bearer unified-gateway-key-123", "", AuthRoutePublic, "authorization"},
		{"unified key via x-api-key -> public", "", "unified-gateway-key-123", AuthRoutePublic, "x-api-key"},
		{"valid sk- key still works", "Bearer sk-abcdefghijklmnop123", "", AuthRouteAuto, "authorization"},
		{"unknown string stays public", "Bearer some-arbitrary-token", "", AuthRoutePublic, "authorization"},
	}

	for _, c := range cases {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		if c.key != "" {
			r.Header.Set("x-api-key", c.key)
		}
		got := extractUpstreamAuth(r)
		if got.Mode != c.wantMode || got.Source != c.wantSrc {
			t.Errorf("%s: got mode=%v src=%q, want mode=%v src=%q", c.name, got.Mode, got.Source, c.wantMode, c.wantSrc)
		}
		if got.Mode == AuthRouteAuto && got.Token == "" {
			t.Errorf("%s: paid auth must carry token", c.name)
		}
	}
}

func TestExtractUpstreamAuthUnifiedKeyDisabled(t *testing.T) {
	configMu.Lock()
	old := apiKey
	apiKey = ""
	configMu.Unlock()
	defer func() {
		configMu.Lock()
		apiKey = old
		configMu.Unlock()
	}()

	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer whatever")
	got := extractUpstreamAuth(r)
	if got.Mode != AuthRoutePublic {
		t.Fatalf("disabled unified key: non-sk token should stay public, got %v", got.Mode)
	}
}
