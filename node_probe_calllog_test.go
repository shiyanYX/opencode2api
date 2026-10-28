package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestProbeOneWritesCallLog(t *testing.T) {
	p := setupProbeHTTP(t, []*ProxyNode{{Name: "sub::probe-node", Protocol: "socks5", Address: "10.0.0.1", Port: 1080}})
	fp := p.snapshot()[0].Fingerprint
	path := useTempCallLogRaw(t, "")
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/models") {
				return probeMgrResponse(204, ""), nil
			}
			return probeMgrResponse(200, `{}`), nil
		})}
	}
	defer func() { probeClientFor = defaultProbeClient }()

	res := probeOne(context.Background(), ProxyNode{Fingerprint: fp, Name: "sub::probe-node"}, "probe-model", "https://opencode.ai/zen/v1/models")
	if res.Class != probeClassOK {
		t.Fatalf("res=%+v", res)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec CallRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("log=%s err=%v", b, err)
	}
	if rec.Path != "/api/nodes/probe" || rec.Model != "probe-model" || rec.RouteMode != "probe" {
		t.Fatalf("record=%+v", rec)
	}
	if len(rec.Nodes) == 0 || rec.Nodes[0] != fp || len(rec.NodeNames) == 0 || rec.NodeNames[0] != "sub::probe-node" {
		t.Fatalf("nodes=%v names=%v", rec.Nodes, rec.NodeNames)
	}
	if len(rec.Events) == 0 || rec.Events[0].Type != "probe" || !strings.Contains(rec.Events[0].Detail, "class=ok") {
		t.Fatalf("events=%+v", rec.Events)
	}
}

func TestProbeOneUniqueReqIDs(t *testing.T) {
	p := setupProbeHTTP(t, []*ProxyNode{{Name: "sub::n", Protocol: "socks5", Address: "10.0.0.1", Port: 1080}})
	fp := p.snapshot()[0].Fingerprint
	path := useTempCallLogRaw(t, "")
	probeClientFor = func(string) *http.Client {
		return &http.Client{Transport: f6RoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/models") {
				return probeMgrResponse(204, ""), nil
			}
			return probeMgrResponse(200, `{}`), nil
		})}
	}
	defer func() { probeClientFor = defaultProbeClient }()
	ctx := context.WithValue(context.Background(), reqIDKey, "job-shared-req")
	for i := 0; i < 3; i++ {
		probeOne(ctx, ProxyNode{Fingerprint: fp, Name: "sub::n"}, "m", "https://opencode.ai/zen/v1/models")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ids := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec CallRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Path != "/api/nodes/probe" {
			continue
		}
		if rec.ReqID == "" || ids[rec.ReqID] {
			t.Fatalf("duplicate/empty req_id %q", rec.ReqID)
		}
		ids[rec.ReqID] = true
	}
	if len(ids) != 3 {
		t.Fatalf("req_ids=%v", ids)
	}
}
