package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestDialNodeSocks5Reachable(t *testing.T) {
	n := &ProxyNode{
		Name:     "local-clash",
		Protocol: "socks5",
		Address:  "192.168.1.3",
		Port:     7890,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := dialNode(ctx, n, "tcp", "example.com:80")
	if err != nil {
		t.Fatalf("dialNode: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	body, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "HTTP/1.1 200") {
		t.Fatalf("unexpected response: %q", body[:min(len(body), 80)])
	}
}

func TestDialNodeSocks5DeadServer(t *testing.T) {
	n := &ProxyNode{
		Name:     "local-clash-auth",
		Protocol: "socks5",
		Address:  "127.0.0.1",
		Port:     1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := dialNode(ctx, n, "tcp", "example.com:80")
	if err == nil && conn != nil {
		conn.Close()
		t.Fatalf("expected dial failure, got success")
	}
}
