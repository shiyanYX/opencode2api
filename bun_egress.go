package main

// ======================== 免费层出口客户端的 Bun 化包装 ========================
//
// 免费层请求必须经 BunTransport 发出（TLS 指纹是硬前提，见 bun_transport.go
// 文件头）。fork 的出口选择（direct / socks5 / 节点池）产出 *http.Client，
// 其 Transport 已携带出口的拨号逻辑（DialContext）。本文件把【拨号函数】
// 从既有 Transport 里取出来，套上 BunTransport 的 uTLS 握手与 HTTP/1.1
// 排版，产出一个新 client —— 出口选择与 sticky 逻辑零改动。
//
// 为什么从 Transport 里抠 DialContext 而不是重新接线：
//   fork 的三种出口都以 http.Transport{DialContext: …} 落地（buildProxyClient /
//   newNodeTransport / 直连 direct），拨号函数是闭包，无法从外部再拿到。
//   http.Transport.DialContext 是可读字段（非接口），类型断言即得。

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

var (
	bunWrapMu    sync.Mutex
	bunWrapCache = map[*http.Client]*http.Client{}
)

// bunClientFor 返回 base 的 Bun 化版本：同一出口拨号，uTLS Bun 指纹传输。
// base 为 nil 时返回 nil（调用方按原逻辑走）。同 base 只构建一次（连接池
// 语义不变）；base 的 Transport 不是 *http.Transport（如测试注入）时，
// 返回 nil（调用方回退 base，形态层退化的风险由测试覆盖面兜底）。
func bunClientFor(base *http.Client) *http.Client {
	if base == nil {
		return nil
	}
	bunWrapMu.Lock()
	defer bunWrapMu.Unlock()
	if c, ok := bunWrapCache[base]; ok {
		return c
	}
	transport, ok := base.Transport.(*http.Transport)
	if !ok {
		return nil
	}
	dial := transport.DialContext
	if dial == nil {
		// 没有显式 DialContext（如 DefaultTransport 克隆）：直连语义。
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	}
	// T3：BunTransport 的 dialer.ConnectTimeout 对注入的 DialContext 不生效
	// （bun_transport.go 的 dial 直接调 dialContext），所以这里显式给建连阶段
	// 套 ctx 上限；ConnectTimeout 仍照传，供未来切换到内部 dialer 时保持语义。
	dialCap := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()
		return dial(dctx, network, addr)
	}
	bt, err := NewBunTransport(BunOptions{
		DialContext:           dialCap,
		ConnectTimeout:        dialTimeout,
		ResponseHeaderTimeout: firstByteTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   20,
	})
	if err != nil {
		return nil
	}
	c := &http.Client{Transport: bt}
	bunWrapCache[base] = c
	return c
}

// bunEgressResult 把出口选择结果 Bun 化（免费层专用）。
// base 无法 Bun 化时原样返回 —— 宁可形态退化也不要断流（会有 429 语义层
// 兜底：见 quota.go 的重判逻辑）。
func bunEgressResult(r EgressResult) EgressResult {
	if c := bunClientFor(r.Client); c != nil {
		return EgressResult{Client: c, NodeFP: r.NodeFP, Direct: r.Direct, key: r.key, eg: r.eg}
	}
	return r
}
