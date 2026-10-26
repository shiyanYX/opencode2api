package main

// ======================== Bun uTLS 传输层（免费层反检测） ========================
//
// 本文件移植自上游 opencode2api v1.4.4 的 internal/httpx/bun.go + bun_dial.go
// （GPL-3.0，jasonxu114514/opencode2api），按本仓库单包结构改写。
//
// ★ 为什么需要它（2026-10-25 对照实验定案，见 .research/ 与记忆库）：
//   OpenCode 免费层 v2 gate 在 TLS 握手层识别客户端：官方 CLI 跑在 Bun 上，
//   ClientHello 是 Bun 内置 BoringSSL 的形状（JA4 t13d1713h1）。Go 标准库
//   crypto/tls 永远发 Go 自己的 ClientHello——一握手就暴露。
//   实测证据链（同 IP 同 headers 同 body）：
//     - v1.4.4 直连（uTLS 伪装）            -> 200
//     - 同一请求经明文代理（OpenSSL TLS）    -> 429 FreeUsageLimitError
//     - 手写形态（Go/Python TLS）           -> 429（10+ 次后该 IP 被当日封禁，
//                                            真 CLI 也 429 —— issue #148 行为）
//   因此本传输层不是可选优化，而是免费层可用性的硬前提。
//
// 与上游实现的差异（有意为之）：
//   - 去掉了 BunOptions.Proxy 分支：本仓库的出口选择在 egress 层完成
//     （direct / socks5 / mihomo 节点池），通过 DialContext 注入到本传输层，
//     uTLS 握手仍由本层完成。上游用 proxy URL 分支做的事，这里由
//     egress.go 的 buildProxyClient / newNodeTransport 等拨号函数承担。
//   - 连接池、空闲看护、复用重试、响应解压逻辑保持一致。
//
// BunTransport 是一个 http.RoundTripper：
//   - TCP 连接：经注入的 dial（fork 的 direct/socks5/mihomo 拨号）；
//   - TLS：uTLS 用 Bun 1.4.3 抓获的 BoringSSL ClientHello 握手
//     （bunClientHello，无 GREASE、无 padding、ALPN 只有 http/1.1、
//     首个 key share 是 X25519MLKEM768）；
//   - HTTP/1.1：请求头按 Bun fetch 的排版规则手工写头（bunRequestHead）：
//     先写 bunCommonHeaders 里的（按该顺序与拼写），再写其余头（小写、
//     按字典序），最后是 fetch 自己补的 Connection/Host/Accept-Encoding/
//     Content-Length。net/http 的头序和大小写规整是另一层指纹。

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	utls "github.com/refraction-networking/utls"
)

// bunHandshakeTimeout 约束新建连接的 TLS 握手时长（与上游一致）。
const bunHandshakeTimeout = 10 * time.Second

// BunOptions 配置一个 BunTransport。ConnectTimeout 等字段对应
// http.Transport 的同名项；DialContext 由调用方注入（fork 的出口拨号）。
type BunOptions struct {
	// DialContext 替代直连拨号；出口选择（direct/socks5/mihomo）由它决定。
	// uTLS 握手在返回的连接上进行。
	DialContext           func(ctx context.Context, network, address string) (net.Conn, error)
	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration
	IdleConnTimeout       time.Duration
	MaxIdleConnsPerHost   int
	DisableKeepAlives     bool
}

// BunTransport 按官方 OpenCode 客户端（Bun 运行时）的方式把请求放上网络。
// 见文件头注释。
type BunTransport struct {
	dialContext           func(context.Context, string, string) (net.Conn, error)
	dialer                *net.Dialer
	responseHeaderTimeout time.Duration
	idleTimeout           time.Duration
	maxIdlePerHost        int
	keepAlive             bool

	mu   sync.Mutex
	idle map[string][]*bunConn
}

// NewBunTransport 从 options 构造 BunTransport。MaxIdleConnsPerHost 为零时
// 取 http.DefaultMaxIdleConnsPerHost（与 http.Transport 行为一致）。
func NewBunTransport(options BunOptions) (*BunTransport, error) {
	if options.DialContext == nil {
		return nil, errors.New("bun transport: DialContext is required (fork routes egress through it)")
	}
	maxIdle := options.MaxIdleConnsPerHost
	if maxIdle == 0 {
		maxIdle = http.DefaultMaxIdleConnsPerHost
	}
	return &BunTransport{
		dialContext:           options.DialContext,
		dialer:                &net.Dialer{Timeout: options.ConnectTimeout, KeepAlive: 30 * time.Second},
		responseHeaderTimeout: options.ResponseHeaderTimeout,
		idleTimeout:           options.IdleConnTimeout,
		maxIdlePerHost:        maxIdle,
		keepAlive:             !options.DisableKeepAlives,
		idle:                  map[string][]*bunConn{},
	}, nil
}

// bunClientHello 返回 Bun fetch 的 ClientHello（从 Bun 1.4.3 抓获）。
// Bun 静态链接自己的 BoringSSL，所以这个 hello 在所有平台一致：
// JA4 t13d1713h1，无 GREASE、无 padding，ALPN 只有 http/1.1，
// 首个 key share 是 X25519MLKEM768。
// uTLS 把握手状态放在 extensions 里，所以每条连接都要一份新 spec。
func bunClientHello() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin: utls.VersionTLS12,
		TLSVersMax: utls.VersionTLS13,
		CipherSuites: []uint16{
			utls.TLS_AES_128_GCM_SHA256,
			utls.TLS_AES_256_GCM_SHA384,
			utls.TLS_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			utls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_RSA_WITH_AES_256_CBC_SHA,
		},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519MLKEM768, utls.X25519, utls.CurveP256, utls.CurveP384}},
			&utls.SupportedPointsExtension{SupportedPoints: []uint8{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				utls.ECDSAWithP256AndSHA256,
				utls.PSSWithSHA256,
				utls.PKCS1WithSHA256,
				utls.ECDSAWithP384AndSHA384,
				utls.PSSWithSHA384,
				utls.PKCS1WithSHA384,
				utls.PSSWithSHA512,
				utls.PKCS1WithSHA512,
				utls.PKCS1WithSHA1,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519MLKEM768}, {Group: utls.X25519}}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		},
	}
}

// bunConn 是一条上游连接。池化期间一个看护 goroutine 阻塞在单字节 Peek 上，
// 这样服务端关连接会在它被再次取用前被发现；watched 在该 goroutine 返回后
// 关闭，watchErr 保存 Peek 的结束原因。
type bunConn struct {
	net.Conn
	key      string
	br       *bufio.Reader
	bw       *bufio.Writer
	idleAt   time.Time
	timer    *time.Timer
	watched  chan struct{}
	watchErr error
}

// RoundTrip 发送 req 并返回响应头；响应体直接从连接上读。
// 请求体在发送前被完整读出。头名与值必须是合法 HTTP 字段。
// 请求 context 结束后，错误即其 cause（与 http.Transport 一致）。
func (t *BunTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := readBunRequestBody(req)
	if err != nil {
		return nil, err
	}
	u := req.URL
	if u == nil || u.Host == "" || u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("bun transport: unsupported request URL %q", u)
	}
	head, decompress, err := bunRequestHead(req, body)
	if err != nil {
		return nil, err
	}
	ctx := req.Context()
	trace := httptrace.ContextClientTrace(ctx)
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	addr := net.JoinHostPort(u.Hostname(), port)
	key := u.Scheme + "://" + addr
	for retried := false; ; retried = true {
		c, info, err := t.getConn(ctx, u.Scheme, key, addr, u.Hostname())
		if err != nil {
			return nil, bunContextError(ctx, err)
		}
		if trace != nil && trace.GotConn != nil {
			trace.GotConn(info)
		}
		resp, dropped, err := t.exchange(ctx, c, req, head, body, decompress, trace)
		if err == nil {
			return resp, nil
		}
		if info.Reused && dropped && !retried && ctx.Err() == nil {
			continue
		}
		return nil, bunContextError(ctx, err)
	}
}

// readBunRequestBody 读取并关闭 req 的 body。无 body 的请求返回 nil；
// 其余返回非 nil（可能为空）切片。
func readBunRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	defer req.Body.Close()
	return io.ReadAll(req.Body)
}

// bunContextError 在 ctx 已结束时返回其 cause，否则返回 err。
func bunContextError(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}

// dial 打开到 addr 的连接（经注入的拨号函数）并完成 Bun TLS 握手。
// 拨号失败原样返回（fork 的 egress 层负责把 connect_error 归因到节点）。
func (t *BunTransport) dial(ctx context.Context, scheme, addr, serverName string) (net.Conn, error) {
	conn, err := t.dialContext(ctx, "tcp", addr)
	if err != nil || scheme != "https" {
		return conn, err
	}
	tlsConn := utls.UClient(conn, &utls.Config{ServerName: serverName}, utls.HelloCustom)
	if err := tlsConn.ApplyPreset(bunClientHello()); err != nil {
		conn.Close()
		return nil, err
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, bunHandshakeTimeout)
	defer cancel()
	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

// getConn 返回 key 的池化连接，跳过空闲期间死掉的；否则拨新连接。
func (t *BunTransport) getConn(ctx context.Context, scheme, key, addr, serverName string) (*bunConn, httptrace.GotConnInfo, error) {
	for c := t.takeIdle(key); c != nil; c = t.takeIdle(key) {
		if c.reclaim() {
			return c, httptrace.GotConnInfo{Conn: c.Conn, Reused: true, WasIdle: true, IdleTime: time.Since(c.idleAt)}, nil
		}
		c.Close()
	}
	conn, err := t.dial(ctx, scheme, addr, serverName)
	if err != nil {
		return nil, httptrace.GotConnInfo{}, err
	}
	c := &bunConn{Conn: conn, key: key, br: bufio.NewReader(conn), bw: bufio.NewWriter(conn)}
	return c, httptrace.GotConnInfo{Conn: conn}, nil
}

// exchange 在 c 上写一个请求并读响应头。成功时响应体持有 c；失败时关闭 c，
// dropped 报告服务端是否在第一个响应字节前掉线（复用连接可重试的情形）。
func (t *BunTransport) exchange(ctx context.Context, c *bunConn, req *http.Request, head, body []byte, decompress bool, trace *httptrace.ClientTrace) (*http.Response, bool, error) {
	stop := context.AfterFunc(ctx, func() { c.Close() })
	fail := func(err error, dropped bool) (*http.Response, bool, error) {
		stop()
		c.Close()
		var timeout net.Error
		if t.responseHeaderTimeout > 0 && ctx.Err() == nil && errors.As(err, &timeout) && timeout.Timeout() {
			err = fmt.Errorf("bun transport: timeout awaiting response headers: %w", err)
		}
		return nil, dropped, err
	}
	_, err := c.bw.Write(head)
	if err == nil {
		_, err = c.bw.Write(body)
	}
	if err == nil {
		err = c.bw.Flush()
	}
	if trace != nil && trace.WroteRequest != nil {
		trace.WroteRequest(httptrace.WroteRequestInfo{Err: err})
	}
	if err != nil {
		return fail(err, isConnDropped(err))
	}
	if t.responseHeaderTimeout > 0 {
		_ = c.SetReadDeadline(time.Now().Add(t.responseHeaderTimeout))
	}
	if _, err := c.br.Peek(1); err != nil {
		return fail(err, isConnDropped(err))
	}
	if trace != nil && trace.GotFirstResponseByte != nil {
		trace.GotFirstResponseByte()
	}
	resp, err := http.ReadResponse(c.br, req)
	// 跳过 1xx 中间响应（如 103 Early Hints）。
	for err == nil && resp.StatusCode < 200 && resp.StatusCode != http.StatusSwitchingProtocols {
		resp, err = http.ReadResponse(c.br, req)
	}
	if err != nil {
		return fail(err, false)
	}
	_ = c.SetReadDeadline(time.Time{})
	raw := &bunBody{t: t, c: c, ctx: ctx, stop: stop, src: resp.Body, reusable: t.keepAlive && !req.Close && !resp.Close}
	if resp.Body == http.NoBody {
		raw.finish(true)
		return resp, false, nil
	}
	resp.Body = raw
	if encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); decompress && isBunEncoding(encoding) {
		resp.Body = &bunDecoder{body: raw, encoding: encoding}
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		resp.Uncompressed = true
	}
	return resp, false, nil
}

// isConnDropped 报告 err 是否表示服务端关闭或重置了连接。
func isConnDropped(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE)
}

// takeIdle 从池中取走 key 最近放入的连接；池空返回 nil。
func (t *BunTransport) takeIdle(key string) *bunConn {
	t.mu.Lock()
	defer t.mu.Unlock()
	conns := t.idle[key]
	if len(conns) == 0 {
		return nil
	}
	c := conns[len(conns)-1]
	t.setBunIdle(key, conns[:len(conns)-1])
	return c
}

// putIdle 在响应读完后将 c 放回池；池满则直接关闭。启动空闲看护。
func (t *BunTransport) putIdle(c *bunConn) {
	t.mu.Lock()
	if len(t.idle[c.key]) >= t.maxIdlePerHost {
		t.mu.Unlock()
		c.Close()
		return
	}
	c.idleAt = time.Now()
	c.watched = make(chan struct{})
	c.timer = nil
	if t.idleTimeout > 0 {
		c.timer = time.AfterFunc(t.idleTimeout, func() {
			if t.removeIdle(c) {
				c.Close()
			}
		})
	}
	t.idle[c.key] = append(t.idle[c.key], c)
	t.mu.Unlock()
	go func() {
		_, c.watchErr = c.br.Peek(1)
		if t.removeIdle(c) {
			c.Close()
		}
		close(c.watched)
	}()
}

// removeIdle 把 c 从池中移除，返回它是否还在池里。
func (t *BunTransport) removeIdle(c *bunConn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	conns := t.idle[c.key]
	index := slices.Index(conns, c)
	if index < 0 {
		return false
	}
	t.setBunIdle(c.key, slices.Delete(conns, index, index+1))
	return true
}

// setBunIdle 存储 key 的池化连接，空表项删除。调用方须持有 mu。
func (t *BunTransport) setBunIdle(key string, conns []*bunConn) {
	if len(conns) == 0 {
		delete(t.idle, key)
		return
	}
	t.idle[key] = conns
}

// reclaim 停止刚取出的连接的空闲看护，并报告连接是否仍可用：
// 看护的 Peek 必须是因这里设置的 deadline 结束的，而不是因服务端数据或
// 关闭结束。读超时不动 TLS 状态，所以连接还能承载下一个请求。
func (c *bunConn) reclaim() bool {
	if c.timer != nil {
		c.timer.Stop()
	}
	_ = c.SetReadDeadline(time.Unix(1, 0))
	<-c.watched
	return errors.Is(c.watchErr, os.ErrDeadlineExceeded) && c.SetReadDeadline(time.Time{}) == nil
}

// CloseIdleConnections 关闭所有池化连接。http.Client 的同名方法会调用它。
func (t *BunTransport) CloseIdleConnections() {
	t.mu.Lock()
	idle := t.idle
	t.idle = map[string][]*bunConn{}
	t.mu.Unlock()
	for _, conns := range idle {
		for _, c := range conns {
			c.Close()
		}
	}
}

// bunBody 是直接从连接上读的响应体。读到末尾把连接还回池（可复用时）；
// 提前 Close、读错误或请求 context 结束则关闭连接——带未读完 body 的
// HTTP/1.1 连接不能再承载下一个请求。src 从不关闭：关闭 net/http 未读完
// 的 body 会排空它，对事件流会阻塞。
type bunBody struct {
	t        *BunTransport
	c        *bunConn
	ctx      context.Context
	stop     func() bool
	src      io.Reader
	reusable bool
	once     sync.Once
}

func (b *bunBody) Read(p []byte) (int, error) {
	n, err := b.src.Read(p)
	if err == io.EOF {
		b.finish(true)
	} else if err != nil {
		err = bunContextError(b.ctx, err)
		b.finish(false)
	}
	return n, err
}

func (b *bunBody) Close() error {
	b.finish(false)
	return nil
}

// finish 释放连接：body 在仍活着的可复用连接上干净结束时还回池，否则关闭。
func (b *bunBody) finish(ended bool) {
	b.once.Do(func() {
		if b.stop() && ended && b.reusable {
			b.t.putIdle(b.c)
			return
		}
		b.c.Close()
	})
}

// isBunEncoding 报告 encoding 是否是本传输层解码的 Content-Encoding，
// 即其 Accept-Encoding 提供的那几种。
func isBunEncoding(encoding string) bool {
	switch encoding {
	case "gzip", "x-gzip", "deflate", "br", "zstd":
		return true
	}
	return false
}

// bunDecoder 解压响应体。解码器在第一次 Read 时才构建，RoundTrip 不用等
// 压缩头。
type bunDecoder struct {
	body     *bunBody
	encoding string
	reader   io.Reader
	release  func()
	err      error
}

func (d *bunDecoder) Read(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	if d.reader == nil {
		if d.reader, d.release, d.err = newBunDecoder(d.encoding, d.body); d.err != nil {
			return 0, d.err
		}
	}
	n, err := d.reader.Read(p)
	if err != nil {
		d.err = err
		if d.release != nil {
			d.release()
		}
		if err == io.EOF {
			// 解码器可能停在自己的流末尾，留下传输分帧（如最后一个 chunk）没读。
			// 把它消费掉，连接才能回池。
			_, _ = io.Copy(io.Discard, io.LimitReader(d.body, 4<<10))
		}
	}
	return n, err
}

func (d *bunDecoder) Close() error {
	return d.body.Close()
}

// newBunDecoder 返回解码 r 的 reader；解码器持有资源时返回释放函数。
// body 大小以 8 MiB zstd 窗口为上限（RFC 9659 对 HTTP 的限制）。
func newBunDecoder(encoding string, r io.Reader) (io.Reader, func(), error) {
	switch encoding {
	case "deflate":
		// "deflate" 指 zlib 包装数据（RFC 9110），但有些服务器发原始 DEFLATE 流，两者都接受。
		reader := bufio.NewReader(r)
		header, err := reader.Peek(2)
		if err != nil {
			return nil, nil, err
		}
		if header[0]&0x0f == 8 && (uint16(header[0])<<8|uint16(header[1]))%31 == 0 {
			decoder, err := zlib.NewReader(reader)
			return decoder, nil, err
		}
		return flate.NewReader(reader), nil, nil
	case "br":
		return brotli.NewReader(r), nil, nil
	case "zstd":
		// 单线程解码器在读 goroutine 上跑，不启动自己的。
		decoder, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(8<<20))
		if err != nil {
			return nil, nil, err
		}
		return decoder, decoder.Close, nil
	default:
		decoder, err := gzip.NewReader(r)
		return decoder, nil, err
	}
}

// bunCommonHeaders 列出 WebKit 按名字认识的头，按 Bun fetch 写它们的
// 拼写与顺序：排在其他所有头之前，按字母序。
var bunCommonHeaders = []string{
	"Accept", "Accept-Charset", "Accept-Encoding", "Accept-Language", "Authorization", "Cache-Control",
	"Connection", "Content-Encoding", "Content-Language", "Content-Length", "Content-Type", "Cookie", "DNT",
	"Expect", "Host", "If-None-Match", "Keep-Alive", "Last-Event-Id", "Origin", "Pragma",
	"Proxy-Authorization", "Range", "Referer", "Sec-Fetch-Mode", "TE", "Upgrade-Insecure-Requests",
	"User-Agent",
}

// bunRequestHead 按 Bun fetch 写 req 的请求行与头（body 为 nil 表示无 body）：
//  1. bunCommonHeaders 里的头，按该顺序与拼写；
//  2. 其余所有头，小写（两个 OpenCode 客户端都是小写给 fetch 的），按 1.x
//     AI SDK 的传递顺序排序（字典序）；
//  3. fetch 自己补的头：Connection、然后 User-Agent 与 Accept（调用方没设时）、
//     Host、Accept-Encoding（调用方没设时）、有 body 时的 Content-Length。
//
// Accept "*/*" 写在 fetch 加默认值的位置，因为官方客户端的 Accept 就出现在那。
// Host、Connection、Content-Length、Transfer-Encoding 属于传输层，调用方的
// 这些值被忽略。decompress 报告传输层是否加了 Accept-Encoding（从而要解码响应）。
func bunRequestHead(req *http.Request, body []byte) ([]byte, bool, error) {
	values := make(map[string]string, len(req.Header))
	for name, list := range req.Header {
		for _, value := range list {
			if !validHeaderField(name, value) {
				return nil, false, fmt.Errorf("bun transport: invalid header field %q", name)
			}
		}
		lower := strings.ToLower(name)
		if len(list) == 0 || lower == "host" || lower == "connection" || lower == "content-length" || lower == "transfer-encoding" {
			continue
		}
		value := strings.Join(list, ", ")
		if previous, ok := values[lower]; ok {
			value = previous + ", " + value
		}
		values[lower] = value
	}
	if values["accept"] == "*/*" {
		delete(values, "accept")
	}
	_, hasUserAgent := values["user-agent"]
	_, hasAccept := values["accept"]
	_, hasEncoding := values["accept-encoding"]
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	var head strings.Builder
	head.WriteString(method + " " + req.URL.RequestURI() + " HTTP/1.1\r\n")
	// fetch 有 body 时自己算 Content-Length，所以它出现在 common 头块里而不是
	// 请求尾。头序是指纹的一部分，要写在运行时放它的位置。
	wroteLength := false
	for _, name := range bunCommonHeaders {
		lower := strings.ToLower(name)
		if lower == "content-length" && body != nil {
			head.WriteString(name + ": " + strconv.Itoa(len(body)) + "\r\n")
			delete(values, lower)
			wroteLength = true
			continue
		}
		if value, ok := values[lower]; ok {
			head.WriteString(name + ": " + value + "\r\n")
			delete(values, lower)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		head.WriteString(name + ": " + values[name] + "\r\n")
	}
	head.WriteString("Connection: keep-alive\r\n")
	if !hasUserAgent {
		head.WriteString("User-Agent: Bun/1.3.14\r\n")
	}
	if !hasAccept {
		head.WriteString("Accept: */*\r\n")
	}
	// fetch 从解析出的 URL 取 Host，默认端口被省略。
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	if req.URL.Scheme == "https" {
		host = strings.TrimSuffix(host, ":443")
	} else {
		host = strings.TrimSuffix(host, ":80")
	}
	head.WriteString("Host: " + host + "\r\n")
	if !hasEncoding {
		head.WriteString("Accept-Encoding: gzip, deflate, br, zstd\r\n")
	}
	if body != nil && !wroteLength {
		head.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	}
	head.WriteString("\r\n")
	return []byte(head.String()), !hasEncoding, nil
}

// validHeaderField 报告 name 是否为 HTTP token 且 value 不含 tab 以外的控制
// 字符，一个字段不能拆分请求。
func validHeaderField(name, value string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if b := name[i]; b <= ' ' || b >= 0x7f || strings.IndexByte(`"(),/:;<=>?@[\]{}`, b) >= 0 {
			return false
		}
	}
	for i := 0; i < len(value); i++ {
		if b := value[i]; b < ' ' && b != '\t' || b == 0x7f {
			return false
		}
	}
	return true
}
