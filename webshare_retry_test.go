package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func withWebshareTestServer(t *testing.T, h func(http.ResponseWriter, *http.Request, int32)) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		h(w, r, n)
	}))
	oldURL := webshareListURL
	webshareListURL = srv.URL
	t.Cleanup(func() { webshareListURL = oldURL; srv.Close() })
	return srv, &calls
}

func webshareOKBody() string {
	return `{"count":1,"next":"","results":[{"username":"u","password":"p","proxy_address":"1.2.3.4","port":1080,"valid":true}]}`
}

func TestWebshareRetriesEOFThenSucceeds(t *testing.T) {
	_, calls := withWebshareTestServer(t, func(w http.ResponseWriter, r *http.Request, n int32) {
		if n <= 2 {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("short"))
			return
		}
		_, _ = w.Write([]byte(webshareOKBody()))
	})
	var sleeps []time.Duration
	oldSleep := webshareSleep
	webshareSleep = func(ctx context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	t.Cleanup(func() { webshareSleep = oldSleep })

	nodes, err := fetchWebshare(context.Background(), WebshareConfig{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || atomic.LoadInt32(calls) != 3 {
		t.Fatalf("nodes=%d calls=%d", len(nodes), atomic.LoadInt32(calls))
	}
	if len(sleeps) != 2 || sleeps[0] != 500*time.Millisecond || sleeps[1] != 1500*time.Millisecond {
		t.Fatalf("sleeps=%v", sleeps)
	}
}

func TestWebshare401NoRetry(t *testing.T) {
	_, calls := withWebshareTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int32) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	oldSleep := webshareSleep
	webshareSleep = func(context.Context, time.Duration) error { t.Fatal("unexpected retry sleep"); return nil }
	t.Cleanup(func() { webshareSleep = oldSleep })
	_, err := fetchWebshare(context.Background(), WebshareConfig{Name: "x"})
	if err == nil {
		t.Fatal("want error")
	}
	if atomic.LoadInt32(calls) != 1 {
		t.Fatalf("calls=%d want 1", atomic.LoadInt32(calls))
	}
}

func TestWebshareThreeEOFs(t *testing.T) {
	_, calls := withWebshareTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int32) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("short"))
	})
	var sleeps []time.Duration
	oldSleep := webshareSleep
	webshareSleep = func(ctx context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	t.Cleanup(func() { webshareSleep = oldSleep })
	_, err := fetchWebshare(context.Background(), WebshareConfig{Name: "x"})
	if err == nil {
		t.Fatal("want error")
	}
	if atomic.LoadInt32(calls) != 3 || len(sleeps) != 2 {
		t.Fatalf("calls=%d sleeps=%v", atomic.LoadInt32(calls), sleeps)
	}
}
