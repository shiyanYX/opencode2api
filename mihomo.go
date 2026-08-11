package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/hub/executor"
	"github.com/metacubex/mihomo/tunnel"
	"gopkg.in/yaml.v3"
)

// mihomoManager 内置 mihomo（Clash.Meta）内核：所有代理协议（ss/vmess/vless/
// trojan/hysteria2/anytls/socks5）的拨号统一交给 mihomo 的 outbound 实现，
// 节点池刷新（订阅更新/手动节点变更）时全量重建内核配置。
type mihomoManager struct {
	mu      sync.RWMutex
	applied bool
	lastErr string
	count   int
}

var mihomoMgr = &mihomoManager{}

// apply 用给定节点池重建 mihomo 内核配置；转换失败的单节点跳过。
func (m *mihomoManager) apply(nodes []*ProxyNode) error {
	proxies := make([]map[string]any, 0, len(nodes))
	skipped := 0
	for _, n := range nodes {
		p, err := proxyNodeToMihomo(n)
		if err != nil {
			skipped++
			continue
		}
		proxies = append(proxies, p)
	}
	raw := map[string]any{
		"log-level": "warning",
		"proxies":   proxies,
	}
	buf, err := yaml.Marshal(raw)
	if err != nil {
		m.recordErr(err, len(proxies))
		return err
	}
	cfg, err := executor.ParseWithBytes(buf)
	if err != nil {
		m.recordErr(err, len(proxies))
		return fmt.Errorf("mihomo parse: %w", err)
	}
	executor.ApplyConfig(cfg, true)
	m.mu.Lock()
	m.applied = true
	m.lastErr = ""
	m.count = len(proxies)
	m.mu.Unlock()
	if skipped > 0 {
		slog.Warn("mihomo 节点转换跳过", "skipped", skipped)
	}
	return nil
}

func (m *mihomoManager) recordErr(err error, count int) {
	m.mu.Lock()
	m.applied = false
	m.lastErr = err.Error()
	m.count = count
	m.mu.Unlock()
}

func (m *mihomoManager) proxy(tag string) (constant.Proxy, bool) {
	px := tunnel.Proxies()[tag]
	return px, px != nil
}

// proxyNodeToMihomo 把节点转换为 mihomo proxies 配置项；tag 用指纹
// （hex，无 mihomo 保留字符，天然唯一）。
func proxyNodeToMihomo(n *ProxyNode) (map[string]any, error) {
	tag := n.Fingerprint
	if tag == "" {
		return nil, fmt.Errorf("节点 %s 缺少指纹", n.Name)
	}
	base := map[string]any{
		"name":   tag,
		"server": n.Address,
		"port":   n.Port,
		"udp":    true,
	}
	switch n.Protocol {
	case "socks5":
		base["type"] = "socks5"
		if n.UserID != "" {
			base["username"] = n.UserID
		}
		if n.Password != "" {
			base["password"] = n.Password
		}
	case "ss":
		base["type"] = "ss"
		base["cipher"] = n.Method
		base["password"] = n.Password
	case "vmess":
		base["type"] = "vmess"
		base["uuid"] = n.UserID
		base["alterId"] = n.AlterIDs
		base["cipher"] = mihomoVmessCipher(n.Security)
		mihomoStreamFields(base, n)
	case "vless":
		base["type"] = "vless"
		base["uuid"] = n.UserID
		mihomoStreamFields(base, n)
		if n.Reality != nil {
			// REALITY 必须在 TLS 之上，且需要 uTLS 指纹
			base["tls"] = true
			ro := map[string]any{
				"public-key": n.Reality.PublicKey,
				"short-id":   n.Reality.ShortID,
			}
			if n.Reality.Fingerprint != "" {
				base["client-fingerprint"] = n.Reality.Fingerprint
			}
			base["reality-opts"] = ro
		}
	case "trojan":
		base["type"] = "trojan"
		base["password"] = n.Password
		mihomoStreamFields(base, n)
	case "hy2":
		base["type"] = "hysteria2"
		base["password"] = n.Password
		if n.SNI != "" {
			base["sni"] = n.SNI
		}
		if n.Insecure {
			base["skip-cert-verify"] = true
		}
	case "anytls":
		base["type"] = "anytls"
		base["password"] = n.Password
		if n.SNI != "" {
			base["sni"] = n.SNI
		}
		if n.Insecure {
			base["skip-cert-verify"] = true
		}
	default:
		return nil, fmt.Errorf("不支持 mihomo 转换的协议 %q", n.Protocol)
	}
	return base, nil
}

func mihomoVmessCipher(security string) string {
	switch strings.ToLower(security) {
	case "none", "zero":
		return "none"
	case "chacha20-poly1305", "chacha20-poly1305-aead":
		return "chacha20-poly1305"
	default:
		return "auto"
	}
}

// mihomoStreamFields 填 ws 传输与 TLS 字段（vmess/vless/trojan 通用）。
func mihomoStreamFields(base map[string]any, n *ProxyNode) {
	if n.Network == "ws" {
		base["network"] = "ws"
		opts := make(map[string]any)
		if n.Path != "" {
			opts["path"] = n.Path
		}
		if n.Host != "" {
			opts["headers"] = map[string]any{"Host": n.Host}
		}
		if len(opts) > 0 {
			base["ws-opts"] = opts
		}
	}
	// trojan 协议本身强制 TLS；vmess/vless 按节点标记。
	if n.TLS || n.Protocol == "trojan" {
		base["tls"] = true
		if n.SNI != "" {
			base["servername"] = n.SNI
		}
		if n.Insecure {
			base["skip-cert-verify"] = true
		}
	}
}

// mihomoDial 经 mihomo 内核按节点指纹拨号到目标地址（仅 tcp）。
func mihomoDial(ctx context.Context, n *ProxyNode, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid target %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid target port in %q", addr)
	}
	px, ok := mihomoMgr.proxy(n.Fingerprint)
	if !ok {
		return nil, fmt.Errorf("mihomo proxy %q 未加载", n.Fingerprint)
	}
	meta := &constant.Metadata{
		NetWork: constant.TCP,
		Type:    constant.SOCKS5,
		Host:    host,
		DstPort: uint16(port),
	}
	conn, err := px.DialContext(ctx, meta)
	if err != nil {
		return nil, err
	}
	return conn, nil
}
