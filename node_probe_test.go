package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClassifyProbeResponse(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{200, `{"ok":true}`, "ok"},
		{204, ``, "ok"},
		{429, `{"error":{"type":"FreeUsageLimitError","message":"free usage limit"}}`, "quota"},
		{429, `{"error":{"type":"rate_limit"}}`, "error"},
		{403, `{"error":{"message":"free tier not available","type":"FreeTierError"}}`, "denied"},
		{403, `{"unrelated":1}`, "error"},
		{502, `bad gateway`, "error"},
	}
	for _, c := range cases {
		if got := classifyProbeResponse(c.status, []byte(c.body)); got != c.want {
			t.Fatalf("status %d body %s = %q, want %q", c.status, c.body, got, c.want)
		}
	}
}

func TestProbeOnePreflightFailureSendsNoModelRequest(t *testing.T) {
	var modelHits atomic.Int64
	probeClientFor = func(fp string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/models") {
				return nil, errors.New("dial tcp: connection refused")
			}
			modelHits.Add(1)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
		})}
	}
	defer func() { probeClientFor = defaultProbeClient }()

	got := probeOne(context.Background(), ProxyNode{Fingerprint: "fp1"}, "m", "https://opencode.ai/zen/v1/models")
	if got.Class != probeClassUnreachable || modelHits.Load() != 0 {
		t.Fatalf("got %+v modelHits=%d, want unreachable/0", got, modelHits.Load())
	}
}

func TestProbeOnePreflightStatusFailureSendsNoModelRequest(t *testing.T) {
	var modelHits atomic.Int64
	probeClientFor = func(fp string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/models") {
				return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("offline")), Header: http.Header{}}, nil
			}
			modelHits.Add(1)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
		})}
	}
	defer func() { probeClientFor = defaultProbeClient }()

	got := probeOne(context.Background(), ProxyNode{Fingerprint: "fp1"}, "m", "https://opencode.ai/zen/v1/models")
	if got.Class != probeClassUnreachable || modelHits.Load() != 0 {
		t.Fatalf("got %+v modelHits=%d, want unreachable/0", got, modelHits.Load())
	}
}

func TestProbeOneSendsExactlyOneModelRequest(t *testing.T) {
	var modelHits atomic.Int64
	probeClientFor = func(fp string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/models") {
				return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
			}
			modelHits.Add(1)
			return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"FreeUsageLimitError"}}`)), Header: http.Header{}}, nil
		})}
	}
	defer func() { probeClientFor = defaultProbeClient }()

	got := probeOne(context.Background(), ProxyNode{Fingerprint: "fp1"}, "m", "https://opencode.ai/zen/v1/models")
	if modelHits.Load() != 1 {
		t.Fatalf("model requests = %d, want exactly 1", modelHits.Load())
	}
	if got.Class != probeClassQuota {
		t.Fatalf("class = %s, want quota", got.Class)
	}
}

func TestProbeOneNilClient(t *testing.T) {
	probeClientFor = func(string) *http.Client { return nil }
	defer func() { probeClientFor = defaultProbeClient }()

	got := probeOne(context.Background(), ProxyNode{Fingerprint: "fp1"}, "m", "https://opencode.ai/zen/v1/models")
	if got.Class != probeClassUnreachable || got.Error == "" {
		t.Fatalf("got %+v, want unreachable with error", got)
	}
}
