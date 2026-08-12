package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type quotaFakeClient struct {
	t         *testing.T
	mu        sync.Mutex
	responses map[string][]fakeUpstreamResponse
	hits      map[string]int
}

func (f *quotaFakeClient) clientFor(n *ProxyNode) *http.Client {
	return &http.Client{Transport: &nodeRoundTripper{f, computeFingerprint(n)}}
}

type nodeRoundTripper struct {
	f  *quotaFakeClient
	fp string
}

func (r *nodeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		r.f.t.Fatalf("unmarshal request body: %v", err)
	}
	model, _ := payload["model"].(string)
	if model != "primary-model" {
		r.f.t.Fatalf("requested model = %q, want primary-model", model)
	}
	r.f.mu.Lock()
	i := r.f.hits[r.fp]
	r.f.hits[r.fp] = i + 1
	r.f.mu.Unlock()

	next := r.f.responses[r.fp][i]
	return &http.Response{
		StatusCode: next.status,
		Header:     next.header,
		Body:       io.NopCloser(strings.NewReader(next.body)),
		Request:    req,
	}, nil
}

func (r *nodeRoundTripper) CloseIdleConnections() {}

// stubOCClient 桩住 OCSession/版本探测，避免测试里发生真实网络请求。
func stubOCClient(t *testing.T) {
	t.Helper()
	oldOCClientVer := ocClientVer
	oldOCSessionID := ocSessionID
	oldOCProjectID := ocProjectID
	ocInitMu.Lock()
	ocInitDone = true
	ocInitMu.Unlock()
	ocClientVer = "test-version"
	ocSessionID = "ses_test"
	ocProjectID = "project_test"
	noSessionRefresh = true
	t.Cleanup(func() {
		ocClientVer = oldOCClientVer
		ocSessionID = oldOCSessionID
		ocProjectID = oldOCProjectID
		noSessionRefresh = false
	})
}

func TestQuotaSwitchNodesNonStream(t *testing.T) {
	stubOCClient(t)
	oldPool := proxyPool
	oldTestClient := testNodeClient
	t.Cleanup(func() {
		proxyPool = oldPool
		testNodeClient = oldTestClient
	})

	n1 := &ProxyNode{Name: "n1", Protocol: "socks5", Address: "1.2.3.4", Port: 1080}
	n2 := &ProxyNode{Name: "n2", Protocol: "socks5", Address: "5.6.7.8", Port: 1080}
	proxyPool = newProxyPool("")
	proxyPool.setNodes([]*ProxyNode{n1, n2})

	fake := &quotaFakeClient{
		t: t,
		responses: map[string][]fakeUpstreamResponse{
			computeFingerprint(n1): {
				{status: http.StatusForbidden, body: `{"error":{"code":"FreeUsageLimitError","message":"free usage limit exceeded"}}`},
			},
			computeFingerprint(n2): {
				{status: http.StatusOK, body: `{"id":"chatcmpl_q","choices":[]}`},
			},
		},
		hits: make(map[string]int),
	}
	testNodeClient = fake.clientFor

	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "primary-model"}}
	goModelsCache = nil
	modelMu.Unlock()

	body, status, _, err := callOpenCodeAPI(context.Background(),
		[]byte(`{"model":"primary-model","messages":[]}`), "primary-model",
		UpstreamAuth{Mode: AuthRoutePublic})
	if err != nil {
		t.Fatalf("callOpenCodeAPI error = %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if string(body) != `{"id":"chatcmpl_q","choices":[]}` {
		t.Fatalf("body = %q", body)
	}
	if got, want := fake.hits[computeFingerprint(n1)], 1; got != want {
		t.Fatalf("n1 hits = %d, want %d", got, want)
	}
	if got, want := fake.hits[computeFingerprint(n2)], 1; got != want {
		t.Fatalf("n2 hits = %d, want %d", got, want)
	}

	states := proxyPool.snapshot()
	for _, s := range states {
		switch s.Name {
		case "n1":
			if s.State != NodeExhausted {
				t.Fatalf("n1 state = %s, want exhausted", s.State)
			}
		case "n2":
			if s.State != NodeAvailable {
				t.Fatalf("n2 state = %s, want available", s.State)
			}
		}
	}
}

func TestQuotaSwitchNodesStream(t *testing.T) {
	stubOCClient(t)
	oldPool := proxyPool
	oldTestClient := testNodeClient
	t.Cleanup(func() {
		proxyPool = oldPool
		testNodeClient = oldTestClient
	})

	n1 := &ProxyNode{Name: "n1", Protocol: "socks5", Address: "1.2.3.4", Port: 1080}
	n2 := &ProxyNode{Name: "n2", Protocol: "socks5", Address: "5.6.7.8", Port: 1080}
	proxyPool = newProxyPool("")
	proxyPool.setNodes([]*ProxyNode{n1, n2})

	fake := &quotaFakeClient{
		t: t,
		responses: map[string][]fakeUpstreamResponse{
			computeFingerprint(n1): {
				{status: http.StatusForbidden, body: `{"error":{"code":"FreeUsageLimitError","message":"free usage limit exceeded"}}`},
			},
			computeFingerprint(n2): {
				{status: http.StatusOK, body: "data: ok\n\n"},
			},
		},
		hits: make(map[string]int),
	}
	testNodeClient = fake.clientFor

	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "primary-model"}}
	goModelsCache = nil
	modelMu.Unlock()

	reader, status, _, err := callOpenCodeAPIStream(context.Background(),
		[]byte(`{"model":"primary-model","messages":[],"stream":true}`),
		"primary-model", UpstreamAuth{Mode: AuthRoutePublic})
	if err != nil {
		t.Fatalf("callOpenCodeAPIStream error = %v", err)
	}
	defer reader.Close()
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	body, _ := io.ReadAll(reader)
	if string(body) != "data: ok\n\n" {
		t.Fatalf("body = %q", body)
	}
	if got, want := fake.hits[computeFingerprint(n1)], 1; got != want {
		t.Fatalf("n1 hits = %d, want %d", got, want)
	}
	if got, want := fake.hits[computeFingerprint(n2)], 1; got != want {
		t.Fatalf("n2 hits = %d, want %d", got, want)
	}
}
