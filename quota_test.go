package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// ======================= 配额切换预算（E-1：循环上限 = 重试上限 + 配额预算） =======================

func quotaBudgetTestSetup(t *testing.T) (oldMax int) {
	t.Helper()
	stubOCClient(t)
	oldPool := proxyPool
	oldTestClient := testNodeClient
	t.Cleanup(func() {
		proxyPool = oldPool
		testNodeClient = oldTestClient
	})
	// 固定全局配额预算为默认 5，避免其他测试污染该全局值。
	quotaSignalsMu.Lock()
	oldMax = maxQuotaNodeSwitches
	maxQuotaNodeSwitches = 0
	quotaSignalsMu.Unlock()
	t.Cleanup(func() {
		quotaSignalsMu.Lock()
		maxQuotaNodeSwitches = oldMax
		quotaSignalsMu.Unlock()
	})
	return oldMax
}

// quotaNode 构造一个 NAME 有序、地址唯一的测试节点（保证轮询顺序确定）。
func quotaNode(name string) *ProxyNode {
	return &ProxyNode{Name: name, Protocol: "socks5", Address: "198.51.100." + name, Port: 1080}
}

func quotaErrBody() string {
	return `{"error":{"type":"FreeUsageLimitError","message":"free usage limit exceeded"}}`
}

// TestQuotaSwitchUsesFullBudget：5 个配额耗尽节点 + 1 个正常节点。
// 预算生效后配额切换次数可超过旧上限 3（本测试断言 ≥4 个节点被标耗尽，
// 随后落在最后一个未试节点上成功 200）。旧实现循环上限 3，最多标 3 个即以
// 403 结束——本测试即为回归锚点。
func TestQuotaSwitchUsesFullBudget(t *testing.T) {
	quotaBudgetTestSetup(t)
	ns := make([]*ProxyNode, 6)
	responses := map[string][]fakeUpstreamResponse{}
	for i := range ns {
		ns[i] = quotaNode(fmt.Sprintf("n%d", i+1))
	}
	for i := 0; i < 5; i++ {
		responses[computeFingerprint(ns[i])] = []fakeUpstreamResponse{
			{status: http.StatusForbidden, body: quotaErrBody()},
		}
	}
	responses[computeFingerprint(ns[5])] = []fakeUpstreamResponse{{status: http.StatusOK, body: `{"id":"chatcmpl_q","choices":[]}`}}

	proxyPool = newProxyPool("")
	proxyPool.setNodes(ns)
	fake := &quotaFakeClient{t: t, responses: responses, hits: make(map[string]int)}
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
		t.Fatalf("status = %d, want 200 (budget exhausted switches then success)", status)
	}
	if string(body) != `{"id":"chatcmpl_q","choices":[]}` {
		t.Fatalf("body = %q", body)
	}
	if got := fake.hits[computeFingerprint(ns[5])]; got != 1 {
		t.Fatalf("ok node hits = %d, want 1", got)
	}
	states := proxyPool.snapshot()
	exhausted := 0
	for _, s := range states {
		if s.State == NodeExhausted {
			exhausted++
		}
	}
	// 新实现：配额切换次数可超过旧上限 3（默认 5 的预算生效）；
	// 旧实现循环上限 3，最多切换 3 个节点即以 403 结束，不会走到 200。
	if exhausted < 4 {
		t.Fatalf("exhausted = %d, want >= 4 (old loop cap 3 would fail here)", exhausted)
	}
}

// TestQuotaBudgetExhaustsReturnsLastError：全部 6 个节点都配额耗尽时，
// 预算 5 用尽后返回最后一个配额错误：6 次尝试命中 6 个不同节点（各 1 次）、
// 恰好 5 个被标记 exhausted、最后 1 个因预算耗尽未被标记（保持可用）。
func TestQuotaBudgetExhaustsReturnsLastError(t *testing.T) {
	quotaBudgetTestSetup(t)
	ns := make([]*ProxyNode, 6)
	responses := map[string][]fakeUpstreamResponse{}
	for i := range ns {
		ns[i] = quotaNode(fmt.Sprintf("n%d", i+1))
		responses[computeFingerprint(ns[i])] = []fakeUpstreamResponse{
			{status: http.StatusForbidden, body: quotaErrBody()},
		}
	}
	proxyPool = newProxyPool("")
	proxyPool.setNodes(ns)
	fake := &quotaFakeClient{t: t, responses: responses, hits: make(map[string]int)}
	testNodeClient = fake.clientFor

	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "primary-model"}}
	goModelsCache = nil
	modelMu.Unlock()

	body, status, _, err := callOpenCodeAPI(context.Background(),
		[]byte(`{"model":"primary-model","messages":[]}`), "primary-model",
		UpstreamAuth{Mode: AuthRoutePublic})
	if err == nil {
		t.Fatal("expected error when quota budget exhausted")
	}
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(string(body), "free usage limit") {
		t.Fatalf("body = %q, want last quota error", body)
	}
	// pick 每次只选未标记节点 → 6 次尝试命中 6 个不同节点，各恰好 1 次。
	for _, n := range ns {
		if got := fake.hits[computeFingerprint(n)]; got != 1 {
			t.Fatalf("%s hits = %d, want 1", n.Name, got)
		}
	}
	states := proxyPool.snapshot()
	exhausted, available := 0, 0
	for _, s := range states {
		switch s.State {
		case NodeExhausted:
			exhausted++
		case NodeAvailable:
			available++
		}
	}
	if exhausted != 5 {
		t.Fatalf("exhausted = %d, want 5 (budget)", exhausted)
	}
	if available != 1 {
		t.Fatalf("available = %d, want 1 (last node unmarked, budget spent)", available)
	}
}

// TestRetryCapUnchangedWithQuotaBudget：循环上限放大到 3+5 后，
// 普通可重试错误（429）仍按重试闸门封顶 3 次，不因预算放大而多打请求。
func TestRetryCapUnchangedWithQuotaBudget(t *testing.T) {
	quotaBudgetTestSetup(t)
	n1 := quotaNode("n1")
	proxyPool = newProxyPool("")
	proxyPool.setNodes([]*ProxyNode{n1})
	fake := &quotaFakeClient{
		t: t,
		responses: map[string][]fakeUpstreamResponse{
			computeFingerprint(n1): {
				{status: http.StatusTooManyRequests, body: `{"error":{"message":"rate limited"}}`},
				{status: http.StatusTooManyRequests, body: `{"error":{"message":"rate limited"}}`},
				{status: http.StatusTooManyRequests, body: `{"error":{"message":"rate limited"}}`},
			},
		},
		hits: make(map[string]int),
	}
	testNodeClient = fake.clientFor

	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "primary-model"}}
	goModelsCache = nil
	modelMu.Unlock()

	_, status, _, err := callOpenCodeAPI(context.Background(),
		[]byte(`{"model":"primary-model","messages":[]}`), "primary-model",
		UpstreamAuth{Mode: AuthRoutePublic})
	if err == nil {
		t.Fatal("expected error on exhausted retries")
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if got := fake.hits[computeFingerprint(n1)]; got != 3 {
		t.Fatalf("hits = %d, want 3 (retry cap unchanged by quota budget)", got)
	}
	if s := proxyPool.snapshot()[0]; s.State != NodeAvailable {
		t.Fatalf("429 without quota signature must not mark exhausted, got %s", s.State)
	}
}

// TestQuotaSwitchThenRetryCapped：配额切换后落到新节点，
// 新节点的 429 重试仍按闸门封顶（2 次命中后结束），配额预算不放大重试。
func TestQuotaSwitchThenRetryCapped(t *testing.T) {
	quotaBudgetTestSetup(t)
	n1 := quotaNode("n1")
	n2 := quotaNode("n2")
	proxyPool = newProxyPool("")
	proxyPool.setNodes([]*ProxyNode{n1, n2})
	fake := &quotaFakeClient{
		t: t,
		responses: map[string][]fakeUpstreamResponse{
			computeFingerprint(n1): {
				{status: http.StatusForbidden, body: quotaErrBody()},
			},
			computeFingerprint(n2): {
				{status: http.StatusTooManyRequests, body: `{"error":{"message":"rate limited"}}`},
				{status: http.StatusTooManyRequests, body: `{"error":{"message":"rate limited"}}`},
			},
		},
		hits: make(map[string]int),
	}
	testNodeClient = fake.clientFor

	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "primary-model"}}
	goModelsCache = nil
	modelMu.Unlock()

	_, status, _, err := callOpenCodeAPI(context.Background(),
		[]byte(`{"model":"primary-model","messages":[]}`), "primary-model",
		UpstreamAuth{Mode: AuthRoutePublic})
	if err == nil {
		t.Fatal("expected error")
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if got := fake.hits[computeFingerprint(n1)]; got != 1 {
		t.Fatalf("n1 hits = %d, want 1", got)
	}
	if got := fake.hits[computeFingerprint(n2)]; got != 2 {
		t.Fatalf("n2 hits = %d, want 2 (retry cap)", got)
	}
	for _, s := range proxyPool.snapshot() {
		if s.Name == "n1" && s.State != NodeExhausted {
			t.Fatalf("n1 state = %s, want exhausted", s.State)
		}
		if s.Name == "n2" && s.State != NodeAvailable {
			t.Fatalf("n2 state = %s, want available", s.State)
		}
	}
}

// TestQuotaSwitchStreamUsesFullBudget：流式路径同样完整消耗 5 次配额预算
// （6 个节点全配额 → 6 次尝试 6 个不同节点、预算耗尽后返回最后一个 403）。
func TestQuotaSwitchStreamUsesFullBudget(t *testing.T) {
	quotaBudgetTestSetup(t)
	ns := make([]*ProxyNode, 6)
	responses := map[string][]fakeUpstreamResponse{}
	for i := range ns {
		ns[i] = quotaNode(fmt.Sprintf("n%d", i+1))
		responses[computeFingerprint(ns[i])] = []fakeUpstreamResponse{
			{status: http.StatusForbidden, body: quotaErrBody()},
		}
	}

	proxyPool = newProxyPool("")
	proxyPool.setNodes(ns)
	fake := &quotaFakeClient{t: t, responses: responses, hits: make(map[string]int)}
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
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (budget exhausted)", status)
	}
	body, _ := io.ReadAll(reader)
	if !strings.Contains(string(body), "free usage limit") {
		t.Fatalf("body = %q, want last quota error", body)
	}
	for _, n := range ns {
		if got := fake.hits[computeFingerprint(n)]; got != 1 {
			t.Fatalf("%s hits = %d, want 1", n.Name, got)
		}
	}
	exhausted, available := 0, 0
	for _, s := range proxyPool.snapshot() {
		switch s.State {
		case NodeExhausted:
			exhausted++
		case NodeAvailable:
			available++
		}
	}
	if exhausted != 5 || available != 1 {
		t.Fatalf("states: exhausted=%d available=%d, want 5/1", exhausted, available)
	}
}
