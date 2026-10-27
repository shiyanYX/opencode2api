package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func probeMgrResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func probeMgrOKClient() *http.Client {
	return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/models") {
			return probeMgrResponse(204, ""), nil
		}
		return probeMgrResponse(200, `{}`), nil
	})}
}

func waitProbeJob(t *testing.T, m *probeManager, id string) probeJobView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v, ok := m.Status(id)
		if ok && !v.Running {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	v, _ := m.Status(id)
	t.Fatalf("probe job %s did not finish: %+v", id, v)
	return probeJobView{}
}

func TestProbeSingleCooldownBoundary(t *testing.T) {
	now := time.Unix(1000, 0)
	probeClock = func() time.Time { return now }
	defer func() { probeClock = time.Now }()
	probeClientFor = func(string) *http.Client { return probeMgrOKClient() }
	defer func() { probeClientFor = defaultProbeClient }()

	m := newProbeManager()
	node := ProxyNode{Fingerprint: "fp1"}
	if _, err := m.Single(context.Background(), node, "m", "https://x/models"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(59*time.Second + 999*time.Millisecond)
	if _, err := m.Single(context.Background(), node, "m", "https://x/models"); !errors.Is(err, errProbeCooldown) {
		t.Fatalf("at 59.999s err=%v, want cooldown", err)
	}
	now = now.Add(2 * time.Millisecond)
	if _, err := m.Single(context.Background(), node, "m", "https://x/models"); err != nil {
		t.Fatalf("at 60.001s err=%v", err)
	}
}

func TestProbeSingleCooldownBeforeUnlock(t *testing.T) {
	now := time.Unix(1000, 0)
	probeClock = func() time.Time { return now }
	defer func() { probeClock = time.Now }()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			once.Do(func() { close(entered) })
			<-release
			if strings.Contains(r.URL.Path, "/models") {
				return probeMgrResponse(204, ""), nil
			}
			return probeMgrResponse(200, `{}`), nil
		})}
	}
	defer func() { probeClientFor = defaultProbeClient }()

	m := newProbeManager()
	node := ProxyNode{Fingerprint: "fp1"}
	done := make(chan struct{})
	go func() {
		_, _ = m.Single(context.Background(), node, "m", "https://x/models")
		close(done)
	}()
	<-entered
	if _, err := m.Single(context.Background(), node, "m", "https://x/models"); !errors.Is(err, errProbeCooldown) {
		t.Fatalf("concurrent second err=%v, want cooldown", err)
	}
	close(release)
	<-done
}

func TestProbeStartAllConcurrencyAndBusy(t *testing.T) {
	var inFlight, peak, hits atomic.Int64
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			n := inFlight.Add(1)
			defer inFlight.Add(-1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			if strings.Contains(r.URL.Path, "/models") {
				return probeMgrResponse(204, ""), nil
			}
			hits.Add(1)
			time.Sleep(20 * time.Millisecond)
			return probeMgrResponse(200, `{}`), nil
		})}
	}
	defer func() { probeClientFor = defaultProbeClient }()

	nodes := make([]ProxyNode, 12)
	for i := range nodes {
		nodes[i].Fingerprint = fmt.Sprintf("fp%d", i)
	}
	m := newProbeManager()
	id, total, err := m.StartAll(context.Background(), nodes, "m", "https://x/models")
	if err != nil || total != 12 || id == "" {
		t.Fatalf("start err=%v total=%d id=%q", err, total, id)
	}
	if _, _, err := m.StartAll(context.Background(), nodes, "m", "https://x/models"); !errors.Is(err, errProbeBusy) {
		t.Fatalf("second start err=%v, want busy", err)
	}
	v := waitProbeJob(t, m, id)
	if v.Done != 12 || v.Total != 12 || hits.Load() != 12 {
		t.Fatalf("done=%d total=%d hits=%d, want 12/12/12", v.Done, v.Total, hits.Load())
	}
	if peak.Load() > 4 {
		t.Fatalf("peak concurrency=%d, want <=4", peak.Load())
	}
	if v.Summary[probeClassOK] != 12 || v.Summary["canceled"] != 0 {
		t.Fatalf("summary=%v", v.Summary)
	}
	if _, _, err := m.StartAll(context.Background(), nodes, "m", "https://x/models"); !errors.Is(err, errProbeCooldown) {
		t.Fatalf("post-job cooldown err=%v", err)
	}
}

func TestProbeStartAllCooldownBoundary(t *testing.T) {
	now := time.Unix(1000, 0)
	probeClock = func() time.Time { return now }
	defer func() { probeClock = time.Now }()
	probeClientFor = func(string) *http.Client { return probeMgrOKClient() }
	defer func() { probeClientFor = defaultProbeClient }()

	m := newProbeManager()
	nodes := []ProxyNode{{Fingerprint: "fp1"}}
	id, _, err := m.StartAll(context.Background(), nodes, "m", "https://x/models")
	if err != nil {
		t.Fatal(err)
	}
	waitProbeJob(t, m, id)
	now = now.Add(10*time.Minute - time.Millisecond)
	if _, _, err := m.StartAll(context.Background(), nodes, "m", "https://x/models"); !errors.Is(err, errProbeCooldown) {
		t.Fatalf("at 9m59.999s err=%v, want cooldown", err)
	}
	now = now.Add(2 * time.Millisecond)
	id, _, err = m.StartAll(context.Background(), nodes, "m", "https://x/models")
	if err != nil {
		t.Fatalf("at 10m00.001s err=%v", err)
	}
	waitProbeJob(t, m, id)
}

func TestProbeCancelKeepsFinished(t *testing.T) {
	var hits atomic.Int64
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/models") {
				return probeMgrResponse(204, ""), nil
			}
			hits.Add(1)
			time.Sleep(10 * time.Millisecond)
			return probeMgrResponse(200, `{}`), nil
		})}
	}
	defer func() { probeClientFor = defaultProbeClient }()

	nodes := make([]ProxyNode, 30)
	for i := range nodes {
		nodes[i].Fingerprint = fmt.Sprintf("fp%d", i)
	}
	m := newProbeManager()
	id, total, err := m.StartAll(context.Background(), nodes, "m", "https://x/models")
	if err != nil || total != 30 {
		t.Fatalf("start err=%v total=%d", err, total)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		v, ok := m.Status(id)
		if !ok {
			t.Fatal("job missing")
		}
		if v.Done >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no probe completed before cancel")
		}
		time.Sleep(time.Millisecond)
	}
	if !m.Cancel(id) {
		t.Fatal("cancel returned false")
	}
	v := waitProbeJob(t, m, id)
	if v.Running {
		t.Fatal("job still running")
	}
	if v.Done >= v.Total {
		t.Fatalf("done=%d total=%d, want done<total", v.Done, v.Total)
	}
	if len(v.Results) != v.Done {
		t.Fatalf("results=%d done=%d", len(v.Results), v.Done)
	}
	canceled := 0
	for _, r := range v.Results {
		if r.Class == "canceled" {
			if r.Error != "canceled" {
				t.Fatalf("canceled error=%q", r.Error)
			}
			canceled++
		}
	}
	if v.Summary["canceled"] != canceled {
		t.Fatalf("summary=%v, want canceled=%d", v.Summary, canceled)
	}
	for class, n := range v.Summary {
		if n == 0 {
			t.Fatalf("summary contains zero key %q: %v", class, v.Summary)
		}
	}
	if canceled == 0 {
		t.Fatal("expected at least one canceled in-flight result")
	}
	if hits.Load() >= int64(v.Total) {
		t.Fatalf("hits=%d, want fewer than %d", hits.Load(), v.Total)
	}
	if m.Cancel(id) {
		t.Fatal("cancel of finished job returned true")
	}
}

func TestProbeJobSurvivesParentCancel(t *testing.T) {
	var hits atomic.Int64
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/models") {
				return probeMgrResponse(204, ""), nil
			}
			hits.Add(1)
			time.Sleep(20 * time.Millisecond)
			return probeMgrResponse(200, `{}`), nil
		})}
	}
	defer func() { probeClientFor = defaultProbeClient }()

	nodes := make([]ProxyNode, 8)
	for i := range nodes {
		nodes[i].Fingerprint = fmt.Sprintf("fp%d", i)
	}
	m := newProbeManager()
	parent, cancel := context.WithCancel(context.Background())
	id, total, err := m.StartAll(parent, nodes, "m", "https://x/models")
	if err != nil || total != 8 {
		t.Fatalf("start err=%v total=%d", err, total)
	}
	cancel()
	v := waitProbeJob(t, m, id)
	if v.Done != v.Total {
		t.Fatalf("done=%d total=%d, job was canceled by parent ctx", v.Done, v.Total)
	}
	if hits.Load() != int64(v.Total) {
		t.Fatalf("hits=%d, want %d", hits.Load(), v.Total)
	}
	if v.Summary[probeClassOK] != v.Total {
		t.Fatalf("summary=%v", v.Summary)
	}
	if _, ok := v.Summary["canceled"]; ok {
		t.Fatalf("zero canceled key leaked: %v", v.Summary)
	}
}

func TestProbeJobCancelStopsRemaining(t *testing.T) {
	var hits atomic.Int64
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/models") {
				return probeMgrResponse(204, ""), nil
			}
			hits.Add(1)
			time.Sleep(10 * time.Millisecond)
			return probeMgrResponse(200, `{}`), nil
		})}
	}
	defer func() { probeClientFor = defaultProbeClient }()

	nodes := make([]ProxyNode, 30)
	for i := range nodes {
		nodes[i].Fingerprint = fmt.Sprintf("fp%d", i)
	}
	m := newProbeManager()
	id, total, err := m.StartAll(context.Background(), nodes, "m", "https://x/models")
	if err != nil || total != 30 {
		t.Fatalf("start err=%v total=%d", err, total)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		v, ok := m.Status(id)
		if !ok {
			t.Fatal("job missing")
		}
		if v.Done >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no probe completed before cancel")
		}
		time.Sleep(time.Millisecond)
	}
	if !m.Cancel(id) {
		t.Fatal("cancel returned false")
	}
	v := waitProbeJob(t, m, id)
	if v.Done >= v.Total {
		t.Fatalf("done=%d total=%d, want done<total", v.Done, v.Total)
	}
	if hits.Load() >= int64(v.Total) {
		t.Fatalf("hits=%d, want fewer than %d", hits.Load(), v.Total)
	}
}

func TestProbeStatusSnapshotCopiesAndUnknown(t *testing.T) {
	probeClientFor = func(string) *http.Client { return probeMgrOKClient() }
	defer func() { probeClientFor = defaultProbeClient }()
	m := newProbeManager()
	node := ProxyNode{Fingerprint: "fp1"}
	if _, err := m.Single(context.Background(), node, "m", "https://x/models"); err != nil {
		t.Fatal(err)
	}
	snap := m.Snapshot()
	res := snap["fp1"]
	res.Class = "mutated"
	snap["fp1"] = res
	if got := m.Snapshot()["fp1"].Class; got != probeClassOK {
		t.Fatalf("Snapshot leaked internal map: class=%s", got)
	}
	if _, ok := m.Status("unknown"); ok {
		t.Fatal("Status(unknown) returned true")
	}
	if m.Cancel("unknown") {
		t.Fatal("Cancel(unknown) returned true")
	}
	id, _, err := m.StartAll(context.Background(), []ProxyNode{node}, "m", "https://x/models")
	if err != nil {
		t.Fatal(err)
	}
	v := waitProbeJob(t, m, id)
	if len(v.Results) != 1 {
		t.Fatalf("results=%d", len(v.Results))
	}
	v.Results[0].Class = "mutated"
	v.Summary[probeClassOK] = 999
	v2, _ := m.Status(id)
	if v2.Results[0].Class != probeClassOK || v2.Summary[probeClassOK] != 1 {
		t.Fatalf("Status leaked internal slices/map: %+v", v2)
	}
}
