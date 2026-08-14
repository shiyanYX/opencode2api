package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveWebshare 起一个 webshare API mock，可按页返回代理列表。
func serveWebshare(t *testing.T, pages [][]webshareProxyDTO) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Token test-api-key" {
			http.Error(w, fmt.Sprintf(`{"detail":"invalid token %q"}`, got), http.StatusUnauthorized)
			return
		}
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			fmt.Sscanf(p, "%d", &page)
		}
		if page < 1 || page > len(pages) {
			http.Error(w, `{"detail":"page out of range"}`, http.StatusNotFound)
			return
		}
		next := "null"
		if page < len(pages) {
			next = `"` + srv.URL + "/api/v2/proxy/list/?page=" + fmt.Sprint(page+1) + "&page_size=100&mode=direct" + `"`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"count":%d,"next":%s,"results":`, 0, next)
		if err := json.NewEncoder(w).Encode(pages[page-1]); err != nil {
			t.Errorf("encode page: %v", err)
		}
		fmt.Fprint(w, `}`)
	}))
	return srv
}

func TestFetchWebshare(t *testing.T) {
	valid := true
	invalid := false
	pages := [][]webshareProxyDTO{
		{
			{Username: "u1", Password: "p1", ProxyAddress: "38.153.152.244", Port: "9594", Valid: &valid, CountryCode: "us", CityName: "Piscataway"},
			{Username: "u2", Password: "p2", ProxyAddress: "10.0.0.2", Port: float64(10001), Valid: &invalid, CountryCode: "DE"},
			{Username: "u3", Password: "p3", ProxyAddress: "10.0.0.3", Port: 10002, Valid: nil},
			{Username: "u4", Password: "p4", ProxyAddress: "10.0.0.4", Port: "invalid", Valid: &valid},
		},
		{
			{Username: "u5", Password: "p5", ProxyAddress: "10.0.0.5", Port: "10003", Valid: &valid},
		},
	}
	srv := serveWebshare(t, pages)
	defer srv.Close()

	old := webshareListURL
	webshareListURL = srv.URL + "/api/v2/proxy/list/"
	defer func() { webshareListURL = old }()

	nodes, err := fetchWebshare(context.Background(), WebshareConfig{Name: "ws", APIKey: "test-api-key"})
	if err != nil {
		t.Fatalf("fetchWebshare: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("want 3 valid proxies, got %d: %+v", len(nodes), nodes)
	}
	byAddr := map[string]*ProxyNode{}
	for _, n := range nodes {
		byAddr[n.Address] = n
	}
	// 第一页：字符串端口 + 有效
	n := byAddr["38.153.152.244"]
	if n == nil {
		t.Fatal("missing 38.153.152.244")
	}
	if n.Protocol != "socks5" || n.Port != 9594 || n.UserID != "u1" || n.Password != "p1" {
		t.Fatalf("node mismatch: %+v", n)
	}
	if !strings.HasPrefix(n.Name, "US-") {
		t.Fatalf("name should be country-prefixed, got %q", n.Name)
	}
	// 第二页节点也合并进来了
	if byAddr["10.0.0.5"] == nil || byAddr["10.0.0.5"].Port != 10003 {
		t.Fatalf("page-2 node not merged: %+v", nodes)
	}
	// 指纹可去重
	fps := map[string]bool{}
	for _, n := range nodes {
		if fps[n.Fingerprint] {
			t.Fatalf("duplicate fingerprint %s", n.Fingerprint)
		}
		fps[n.Fingerprint] = true
	}
}

func TestFetchWebshareAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"Invalid token."}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	old := webshareListURL
	webshareListURL = srv.URL + "/"
	defer func() { webshareListURL = old }()

	_, err := fetchWebshare(context.Background(), WebshareConfig{Name: "ws", APIKey: "wrong"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("want 401 error, got %v", err)
	}
}

func TestFetchWebshareEmpty(t *testing.T) {
	page := []webshareProxyDTO{}
	srv := serveWebshare(t, [][]webshareProxyDTO{page})
	defer srv.Close()

	old := webshareListURL
	webshareListURL = srv.URL + "/api/v2/proxy/list/"
	defer func() { webshareListURL = old }()

	_, err := fetchWebshare(context.Background(), WebshareConfig{Name: "ws", APIKey: "test-api-key"})
	if err == nil {
		t.Fatal("want error for empty proxy list")
	}
}

func TestWebshareToNodePorts(t *testing.T) {
	valid := true
	cases := []struct {
		dto  webshareProxyDTO
		want int
		ok   bool
	}{
		{webshareProxyDTO{ProxyAddress: "1.2.3.4", Port: float64(8080)}, 8080, true},
		{webshareProxyDTO{ProxyAddress: "1.2.3.4", Port: "8080"}, 8080, true},
		{webshareProxyDTO{ProxyAddress: "1.2.3.4", Port: 0}, 0, false},
		{webshareProxyDTO{ProxyAddress: "1.2.3.4", Port: "abc"}, 0, false},
		{webshareProxyDTO{ProxyAddress: "", Port: float64(80), Valid: &valid}, 0, false},
	}
	for i, c := range cases {
		n := webshareToNode(c.dto)
		if c.ok && (n == nil || n.Port != c.want) {
			t.Fatalf("case %d: want port %d, got %+v", i, c.want, n)
		}
		if !c.ok && n != nil {
			t.Fatalf("case %d: want nil node, got %+v", i, n)
		}
	}
}
