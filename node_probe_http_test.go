package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func setupProbeHTTP(t *testing.T, nodes []*ProxyNode) *nodePool {
	t.Helper()
	p := newProxyPool(filepath.Join(t.TempDir(), "proxy_state.json"))
	p.setNodes(nodes)
	oldPool := proxyPool
	oldProber := nodeProber
	oldPath := configPath
	oldClient := probeClientFor
	oldClock := probeClock
	proxyPool = p
	nodeProber = newProbeManager()
	configPath = filepath.Join(t.TempDir(), "config.json")
	t.Cleanup(func() {
		proxyPool = oldPool
		nodeProber = oldProber
		configPath = oldPath
		probeClientFor = oldClient
		probeClock = oldClock
	})
	return p
}

func probeHTTPJSON(t *testing.T, rr *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type=%q", ct)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), dst); err != nil {
		t.Fatalf("json: %v body=%s", err, rr.Body.String())
	}
}

func probeHTTPPost(handler http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rr := httptest.NewRecorder()
	handler(rr, req)
	return rr
}

func TestNodesProbeSingleCooldownAndSuccess(t *testing.T) {
	now := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	p := setupProbeHTTP(t, []*ProxyNode{{Name: "n1", Protocol: "socks5", Address: "10.0.0.1", Port: 1080}})
	fp := p.snapshot()[0].Fingerprint
	var calls atomic.Int64
	probeClock = func() time.Time { return now }
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/models") {
				return probeMgrResponse(204, ""), nil
			}
			calls.Add(1)
			return probeMgrResponse(200, `{}`), nil
		})}
	}

	body := `{"fingerprint":"` + fp + `"}`
	rr := probeHTTPPost(adminNodeProbeHandler, "/api/nodes/probe", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got probeResult
	probeHTTPJSON(t, rr, &got)
	if got.Class != probeClassOK || got.Fingerprint != fp || calls.Load() != 1 {
		t.Fatalf("result=%+v calls=%d", got, calls.Load())
	}

	now = now.Add(59 * time.Second)
	rr = probeHTTPPost(adminNodeProbeHandler, "/api/nodes/probe", body)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("within cooldown status=%d body=%s", rr.Code, rr.Body.String())
	}
	var errBody struct {
		Error      string `json:"error"`
		RetryAfter int    `json:"retry_after_s"`
	}
	probeHTTPJSON(t, rr, &errBody)
	if errBody.Error != "probe cooldown" || errBody.RetryAfter <= 0 || calls.Load() != 1 {
		t.Fatalf("cooldown body=%+v calls=%d", errBody, calls.Load())
	}

	now = now.Add(2 * time.Second) // 61s after first
	rr = probeHTTPPost(adminNodeProbeHandler, "/api/nodes/probe", body)
	if rr.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("after cooldown status=%d calls=%d body=%s", rr.Code, calls.Load(), rr.Body.String())
	}
}

func TestNodesProbeNotFoundDoesNotCallClient(t *testing.T) {
	p := setupProbeHTTP(t, []*ProxyNode{{Name: "n1", Protocol: "socks5", Address: "10.0.0.1", Port: 1080}})
	_ = p
	var calls atomic.Int64
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return probeMgrResponse(200, `{}`), nil
		})}
	}
	rr := probeHTTPPost(adminNodeProbeHandler, "/api/nodes/probe", `{"fingerprint":"missing"}`)
	if rr.Code != http.StatusNotFound || calls.Load() != 0 {
		t.Fatalf("status=%d calls=%d body=%s", rr.Code, calls.Load(), rr.Body.String())
	}
	var body map[string]string
	probeHTTPJSON(t, rr, &body)
	if body["error"] != "node not found" {
		t.Fatalf("body=%v", body)
	}
}

func TestNodesProbeListFieldNullAndPresent(t *testing.T) {
	p := setupProbeHTTP(t, []*ProxyNode{
		{Name: "n1", Protocol: "socks5", Address: "10.0.0.1", Port: 1080},
		{Name: "n2", Protocol: "socks5", Address: "10.0.0.2", Port: 1080},
	})
	nodes := p.snapshot()
	fp := nodes[1].Fingerprint
	nodeProber.mu.Lock()
	nodeProber.results[fp] = probeResult{Fingerprint: fp, Class: probeClassOK, Status: 200, At: time.Now()}
	nodeProber.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	rr := httptest.NewRecorder()
	adminNodesHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Nodes []nodeAdminView `json:"nodes"`
	}
	probeHTTPJSON(t, rr, &resp)
	if len(resp.Nodes) != 2 {
		t.Fatalf("nodes=%d", len(resp.Nodes))
	}
	byFP := map[string]nodeAdminView{}
	for _, n := range resp.Nodes {
		byFP[n.Fingerprint] = n
	}
	if byFP[nodes[0].Fingerprint].Probe != nil {
		t.Fatalf("untested probe=%+v", byFP[nodes[0].Fingerprint].Probe)
	}
	if got := byFP[fp].Probe; got == nil || got.Fingerprint != fp {
		t.Fatalf("tested probe=%+v", got)
	}
	if strings.Contains(rr.Body.String(), `"probe":null`) == false {
		t.Fatalf("missing probe null: %s", rr.Body.String())
	}
}

func TestNodesProbeAllStartStatusBusyUnknown(t *testing.T) {
	p := setupProbeHTTP(t, []*ProxyNode{
		{Name: "n1", Protocol: "socks5", Address: "10.0.0.1", Port: 1080},
		{Name: "n2", Protocol: "socks5", Address: "10.0.0.2", Port: 1080},
	})
	_ = p
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/models") {
				return probeMgrResponse(204, ""), nil
			}
			once.Do(func() { close(entered) })
			<-release
			return probeMgrResponse(200, `{}`), nil
		})}
	}

	rr := probeHTTPPost(adminNodeProbeAllHandler, "/api/nodes/probe_all", `{}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("start status=%d body=%s", rr.Code, rr.Body.String())
	}
	var started struct {
		JobID string `json:"job_id"`
		Total int    `json:"total"`
	}
	probeHTTPJSON(t, rr, &started)
	if started.JobID == "" || started.Total != 2 {
		t.Fatalf("started=%+v", started)
	}
	<-entered
	rr = probeHTTPPost(adminNodeProbeAllHandler, "/api/nodes/probe_all", `{}`)
	if rr.Code != http.StatusTooManyRequests || !strings.Contains(rr.Body.String(), "probe already running") {
		t.Fatalf("busy status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Unknown/empty status and cancel.
	req := httptest.NewRequest(http.MethodGet, "/api/nodes/probe_all?id=nope", nil)
	rr = httptest.NewRecorder()
	adminNodeProbeAllHandler(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = probeHTTPPost(adminNodeProbeAllCancelHandler, "/api/nodes/probe_all/cancel", `{"job_id":"nope"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown cancel=%d body=%s", rr.Code, rr.Body.String())
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	var view probeJobView
	var ok bool
	for time.Now().Before(deadline) {
		req = httptest.NewRequest(http.MethodGet, "/api/nodes/probe_all?id="+started.JobID, nil)
		rr = httptest.NewRecorder()
		adminNodeProbeAllHandler(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status poll=%d body=%s", rr.Code, rr.Body.String())
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
			t.Fatalf("view json: %v", err)
		}
		ok = true
		if !view.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok || view.Running || view.Done != 2 {
		t.Fatalf("final view=%+v", view)
	}
}

func TestNodesProbeAllCancelSuccess(t *testing.T) {
	p := setupProbeHTTP(t, []*ProxyNode{{Name: "n1", Protocol: "socks5", Address: "10.0.0.1", Port: 1080}})
	_ = p
	entered := make(chan struct{})
	release := make(chan struct{})
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/models") {
				return probeMgrResponse(204, ""), nil
			}
			close(entered)
			<-release
			return probeMgrResponse(500, `{"error":"x"}`), nil
		})}
	}
	rr := probeHTTPPost(adminNodeProbeAllHandler, "/api/nodes/probe_all", `{}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("start=%d body=%s", rr.Code, rr.Body.String())
	}
	var started struct {
		JobID string `json:"job_id"`
	}
	probeHTTPJSON(t, rr, &started)
	<-entered
	rr = probeHTTPPost(adminNodeProbeAllCancelHandler, "/api/nodes/probe_all/cancel", `{"job_id":"`+started.JobID+`"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("cancel=%d body=%s", rr.Code, rr.Body.String())
	}
	close(release)
	v := waitProbeJob(t, nodeProber, started.JobID)
	if v.Running {
		t.Fatalf("running after cancel: %+v", v)
	}
}

func TestNodesProbeConfigDefault(t *testing.T) {
	setupProbeHTTP(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rr := httptest.NewRecorder()
	adminConfigHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		ProbeModel string `json:"probe_model"`
	}
	probeHTTPJSON(t, rr, &body)
	if body.ProbeModel != defaultProbeModel {
		t.Fatalf("probe_model=%q want %q", body.ProbeModel, defaultProbeModel)
	}
}

func TestProbeCooldownRemainingMethods(t *testing.T) {
	now := time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC)
	probeClock = func() time.Time { return now }
	m := newProbeManager()
	if got := m.SingleCooldownRemaining("fp"); got != 0 {
		t.Fatalf("initial single=%d", got)
	}
	m.mu.Lock()
	m.lastSingle["fp"] = now
	m.lastAllEnd = now
	m.mu.Unlock()
	if got := m.SingleCooldownRemaining("fp"); got != 60 {
		t.Fatalf("single=%d", got)
	}
	if got := m.AllCooldownRemaining(); got != 600 {
		t.Fatalf("all=%d", got)
	}
	now = now.Add(59*time.Second + 999*time.Millisecond)
	if got := m.SingleCooldownRemaining("fp"); got != 1 {
		t.Fatalf("ceil single=%d", got)
	}
	if got := m.AllCooldownRemaining(); got != 541 {
		t.Fatalf("ceil all=%d", got)
	}
	now = now.Add(time.Second)
	if got := m.SingleCooldownRemaining("fp"); got != 0 {
		t.Fatalf("expired single=%d", got)
	}
}
