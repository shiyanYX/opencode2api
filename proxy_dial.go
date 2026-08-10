package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/anytls/sing-anytls"
	"github.com/sagernet/sing-quic/hysteria2"
	loggerx "github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	aTLS "github.com/sagernet/sing/common/tls"
	"github.com/shadowsocks/go-shadowsocks2/core"
	xcnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	xcuuid "github.com/xtls/xray-core/common/uuid"
	"github.com/xtls/xray-core/proxy/vless"
	"github.com/xtls/xray-core/proxy/vless/encoding"
	"github.com/xtls/xray-core/transport/internet/reality"
)

const (
	tcpDialTimeout      = 15 * time.Second
	tlsHandshakeTimeout = 15 * time.Second
)

// dialNode 按节点协议建立到目标 addr（"host:port"）的出口 TCP 连接。
func dialNode(ctx context.Context, n *ProxyNode, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errors.New("dialNode: only tcp is supported")
	}
	switch n.Protocol {
	case "socks5":
		return dialSocks5Node(ctx, n, addr)
	case "ss":
		return dialSSNode(ctx, n, addr)
	case "vless":
		return dialVlessNode(ctx, n, addr)
	case "anytls":
		return dialAnyTLSNode(ctx, n, addr)
	case "hy2":
		return dialHysteria2Node(ctx, n, addr)
	default:
		return nil, fmt.Errorf("unsupported proxy protocol %q", n.Protocol)
	}
}

// ======================= 基础工具 =======================

// dialTCP 直连节点服务器 TCP。
func dialTCP(ctx context.Context, n *ProxyNode) (net.Conn, error) {
	addr := net.JoinHostPort(n.Address, strconv.Itoa(n.Port))
	d := net.Dialer{Timeout: tcpDialTimeout}
	return d.DialContext(ctx, "tcp", addr)
}

// serverTLSConfig 构造 stdlib TLS 配置：SNI 优先取 n.SNI，否则回退到 fallbackHost。
func serverTLSConfig(n *ProxyNode, fallbackHost string) *tls.Config {
	sni := n.SNI
	if sni == "" {
		sni = fallbackHost
	}
	return &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: n.Insecure,
		MinVersion:         tls.VersionTLS12,
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

// ======================= socks5 =======================

func dialSocks5Node(ctx context.Context, n *ProxyNode, addr string) (net.Conn, error) {
	conn, err := dialTCP(ctx, n)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()
	return socks5Dial(Socks5Proxy{
		Addr:     net.JoinHostPort(n.Address, strconv.Itoa(n.Port)),
		Username: n.UserID,
		Password: n.Password,
		Name:     n.Name,
	})(ctx, "tcp", addr)
}

// ======================= shadowsocks（经典 AEAD，经 go-shadowsocks2） =======================

func dialSSNode(ctx context.Context, n *ProxyNode, addr string) (net.Conn, error) {
	switch strings.ToLower(n.Method) {
	case "aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", "chacha20-poly1305":
	default:
		return nil, fmt.Errorf("unsupported ss method %q（仅支持 aes-128-gcm / aes-256-gcm / chacha20-ietf-poly1305）", n.Method)
	}
	ciph, err := core.PickCipher(strings.ToUpper(n.Method), nil, n.Password)
	if err != nil {
		return nil, fmt.Errorf("ss cipher: %w", err)
	}
	conn, err := dialTCP(ctx, n)
	if err != nil {
		return nil, err
	}
	return ciph.StreamConn(conn), nil
}

// ======================= vless（TLS / REALITY，仅空 flow） =======================

func dialVlessNode(ctx context.Context, n *ProxyNode, addr string) (net.Conn, error) {
	if n.Flow != "" {
		return nil, fmt.Errorf("vless flow %q 暂不支持（仅支持空 flow）", n.Flow)
	}
	raw, err := dialTCP(ctx, n)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			raw.Close()
		}
	}()

	u, err := xcuuid.ParseString(n.UserID)
	if err != nil {
		return nil, fmt.Errorf("invalid vless uuid: %w", err)
	}
	id := protocol.NewID(u)

	dest, err := xcnet.ParseDestination(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid target %q: %w", addr, err)
	}

	var conn net.Conn = raw
	if n.Reality != nil && n.Reality.PublicKey != "" && n.Reality.Fingerprint != "" {
		rac := &reality.Config{
			ServerName:  firstNonEmpty(n.SNI, n.Address, dest.Address.String()),
			Fingerprint: n.Reality.Fingerprint,
			PublicKey:   []byte(n.Reality.PublicKey),
			ShortId:     []byte(n.Reality.ShortID),
			SpiderX:     n.Reality.SpiderX,
		}
		rConn, rerr := reality.UClient(raw, rac, ctx, dest)
		if rerr != nil {
			return nil, fmt.Errorf("reality: %w", rerr)
		}
		conn = rConn
	} else {
		tc := tls.Client(raw, serverTLSConfig(n, dest.Address.String()))
		if err := tc.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("tls handshake: %w", err)
		}
		conn = tc
	}

	req := &protocol.RequestHeader{
		Version: encoding.Version,
		User:    &protocol.MemoryUser{Account: &vless.MemoryAccount{ID: id, Flow: ""}},
		Command: protocol.RequestCommandTCP,
		Address: dest.Address,
		Port:    dest.Port,
	}
	var buf bytes.Buffer
	if err := encoding.EncodeRequestHeader(&buf, req, nil); err != nil {
		return nil, fmt.Errorf("vless encode: %w", err)
	}
	if _, err := conn.Write(buf.Bytes()); err != nil {
		return nil, fmt.Errorf("vless write header: %w", err)
	}
	if _, err := encoding.DecodeResponseHeader(bufio.NewReader(conn), req); err != nil {
		return nil, fmt.Errorf("vless handshake: %w", err)
	}
	success = true
	return conn, nil
}

// ======================= anytls =======================

func dialAnyTLSNode(ctx context.Context, n *ProxyNode, addr string) (net.Conn, error) {
	raw, err := dialTCP(ctx, n)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			raw.Close()
		}
	}()
	tlsCfg := serverTLSConfig(n, n.Address)
	cli, err := anytls.NewClient(ctx, anytls.ClientConfig{
		Password:           n.Password,
		IdleSessionTimeout: 5 * time.Minute,
		DialOut: func(ctx context.Context) (net.Conn, error) {
			tc := tls.Client(raw, tlsCfg)
			if err := tc.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, fmt.Errorf("anytls tls handshake: %w", err)
			}
			return tc, nil
		},
		Logger: singLogBridge{},
	})
	if err != nil {
		return nil, fmt.Errorf("anytls new client: %w", err)
	}
	c, err := cli.CreateProxy(ctx, M.ParseSocksaddr(addr))
	if err != nil {
		return nil, fmt.Errorf("anytls create proxy: %w", err)
	}
	success = true
	return c, nil
}

// ======================= hysteria2 =======================

func dialHysteria2Node(ctx context.Context, n *ProxyNode, addr string) (net.Conn, error) {
	tlsCfg := serverTLSConfig(n, n.Address)
	client, err := hysteria2.NewClient(hysteria2.ClientOptions{
		Context:       ctx,
		Dialer:        plainDialer{},
		ServerAddress: M.ParseSocksaddr(net.JoinHostPort(n.Address, strconv.Itoa(n.Port))),
		Password:      n.Password,
		TLSConfig:     &singTLSAdapter{cfg: tlsCfg, timeout: tlsHandshakeTimeout},
		Logger:        singLogBridge{},
	})
	if err != nil {
		return nil, fmt.Errorf("hy2 new client: %w", err)
	}
	c, err := client.DialConn(ctx, M.ParseSocksaddr(addr))
	if err != nil {
		return nil, fmt.Errorf("hy2 dial: %w", err)
	}
	return c, nil
}

// plainDialer 直连拨号器（hy2 的 UDP QUIC 底层）。
type plainDialer struct{}

func (plainDialer) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, destination.String())
}

func (plainDialer) ListenPacket(_ context.Context, _ M.Socksaddr) (net.PacketConn, error) {
	return net.ListenPacket("udp", "")
}

// singTLSAdapter 把 stdlib tls.Config 适配为 sing 的 tls.Config 接口。
type singTLSAdapter struct {
	cfg     *tls.Config
	timeout time.Duration
}

func (w *singTLSAdapter) ServerName() string                  { return w.cfg.ServerName }
func (w *singTLSAdapter) SetServerName(s string)              { w.cfg.ServerName = s }
func (w *singTLSAdapter) NextProtos() []string                { return w.cfg.NextProtos }
func (w *singTLSAdapter) SetNextProtos(p []string)            { w.cfg.NextProtos = p }
func (w *singTLSAdapter) HandshakeTimeout() time.Duration     { return w.timeout }
func (w *singTLSAdapter) SetHandshakeTimeout(d time.Duration) { w.timeout = d }
func (w *singTLSAdapter) STDConfig() (*aTLS.STDConfig, error) { return w.cfg, nil }
func (w *singTLSAdapter) Clone() aTLS.Config {
	return &singTLSAdapter{cfg: w.cfg.Clone(), timeout: w.timeout}
}
func (w *singTLSAdapter) Client(conn net.Conn) (aTLS.Conn, error) {
	return &singTLSConn{Conn: tls.Client(conn, w.cfg.Clone())}, nil
}

type singTLSConn struct{ *tls.Conn }

func (c *singTLSConn) NetConn() net.Conn { return c.Conn }

// singLogBridge 把 sing logger 桥接到 slog。
type singLogBridge struct{}

var _ loggerx.ContextLogger = singLogBridge{}

func (singLogBridge) Trace(args ...any)                        { slog.Debug(fmt.Sprint(args...)) }
func (singLogBridge) Debug(args ...any)                        { slog.Debug(fmt.Sprint(args...)) }
func (singLogBridge) Info(args ...any)                         { slog.Info(fmt.Sprint(args...)) }
func (singLogBridge) Warn(args ...any)                         { slog.Warn(fmt.Sprint(args...)) }
func (singLogBridge) Error(args ...any)                        { slog.Error(fmt.Sprint(args...)) }
func (singLogBridge) Fatal(args ...any)                        { slog.Error(fmt.Sprint(args...)) }
func (singLogBridge) Panic(args ...any)                        { slog.Error(fmt.Sprint(args...)) }
func (singLogBridge) TraceContext(_ context.Context, a ...any) { singLogBridge{}.Trace(a...) }
func (singLogBridge) DebugContext(_ context.Context, a ...any) { singLogBridge{}.Debug(a...) }
func (singLogBridge) InfoContext(_ context.Context, a ...any)  { singLogBridge{}.Info(a...) }
func (singLogBridge) WarnContext(_ context.Context, a ...any)  { singLogBridge{}.Warn(a...) }
func (singLogBridge) ErrorContext(_ context.Context, a ...any) { singLogBridge{}.Error(a...) }
func (singLogBridge) FatalContext(_ context.Context, a ...any) { singLogBridge{}.Fatal(a...) }
func (singLogBridge) PanicContext(_ context.Context, a ...any) { singLogBridge{}.Panic(a...) }
