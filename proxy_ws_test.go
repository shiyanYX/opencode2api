package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDebugProbeQyJP(t *testing.T) {
	if os.Getenv("OC2_DEBUG_WS") == "" {
		t.Skip("debug-only")
	}
	cases := []*ProxyNode{
		{Name: "dbg-vless-ws", Protocol: "vless", Address: "127.0.0.1", Port: 18443,
			UserID: "50b3733d-551f-4092-a07d-79951636bd86", Network: "ws", Path: "/go",
			SNI: "127.0.0.1", TLS: true, Insecure: true},
		{Name: "dbg-vmess-ws", Protocol: "vmess", Address: "127.0.0.1", Port: 18444,
			UserID: "50b3733d-551f-4092-a07d-79951636bd86", Network: "ws", Path: "/vgo",
			SNI: "127.0.0.1", TLS: true, Insecure: true, Security: "auto"},
		{Name: "dbg-trojan", Protocol: "trojan", Address: "127.0.0.1", Port: 18445,
			Password: "trojan-pass-1", SNI: "127.0.0.1", Insecure: true},
	}
	for _, n := range cases {
		n.Fingerprint = computeFingerprint(n)
	}
	if err := mihomoMgr.apply(cases); err != nil {
		t.Fatalf("mihomo apply: %v", err)
	}
	for _, n := range cases {
		target := "example.com:80"
		expect := "HTTP/1.1"
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		conn, err := dialNode(ctx, n, "tcp", target)
		if err != nil {
			t.Errorf("%s dialNode: %v", n.Name, err)
			cancel()
			continue
		}
		if _, err := conn.Write([]byte("GET /generate_204 HTTP/1.1\r\nHost: www.gstatic.com\r\nConnection: close\r\n\r\n")); err != nil {
			t.Errorf("%s write: %v", n.Name, err)
			conn.Close()
			cancel()
			continue
		}
		resp, err := io.ReadAll(conn)
		conn.Close()
		cancel()
		if strings.Contains(string(resp), expect) {
			t.Logf("%s OK (%s via %s->%s)", n.Name, expect, n.Protocol, n.Network)
		} else if err != nil && !strings.Contains(err.Error(), "use of closed network connection") && !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "ws closed") {
			t.Errorf("%s read: %v", n.Name, err)
		} else {
			t.Errorf("%s unexpected response: %.120q", n.Name, resp)
		}
	}
}
