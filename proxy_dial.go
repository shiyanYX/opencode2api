package main

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// dialNode 按节点协议建立到目标 addr（"host:port"）的出口 TCP 连接。
// 所有代理协议（ss/vmess/vless/trojan/hysteria2/anytls/socks5）统一经
// 内置 mihomo 内核拨号，见 mihomo.go。
func dialNode(ctx context.Context, n *ProxyNode, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errors.New("dialNode: only tcp is supported")
	}
	switch n.Protocol {
	case "socks5", "ss", "vless", "vmess", "trojan", "anytls", "hy2":
		return mihomoDial(ctx, n, addr)
	default:
		return nil, fmt.Errorf("unsupported proxy protocol %q", n.Protocol)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
