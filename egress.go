package main

// ======================== 出口客户端模块（EgressClient）========================
//
// 统一管理出口客户端选择：节点池、静态 socks5（含 sticky 会话）、直连。
// 调用者只需 egress.Get(req) 一行，不再需要了解底层路由决策。
//
// 设计：
//   - EgressClient: 路由决策（节点池 vs socks5 vs 直连）
//   - stickySession: 会话粘性管理（TTL 清理、容量驱逐、FNV 哈希绑定）
//   - EgressResult: 持有选中客户端 + Invalidate() 失效上下文

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ======================== 公开类型 ========================

// EgressRequest 打包调用者已有的所有数据，不需要额外收集。
type EgressRequest struct {
	Auth           UpstreamAuth
	ForceSwitch    bool
	BodyMap        map[string]any // sticky key 推导用
	RequiredRegion string         // "" = 无区域限制
}

// EgressResult 持有选中的客户端和失效上下文。
// 调用者使用 Client 发请求，失败时调用 Invalidate() 解除 sticky 绑定。
type EgressResult struct {
	Client *http.Client // 永不为 nil
	NodeFP string       // "" = 直连/sticky（非节点池追踪的节点）
	Direct bool         // true = 真·直连出口（区别于静态 socks5）

	key string        // sticky key，Invalidate() 用
	eg  *EgressClient // 反向引用
}

// Invalidate 解除 sticky 绑定，下次同一会话换出口。
// 节点池或直连路径下为 no-op。
func (r *EgressResult) Invalidate() {
	if r.eg != nil && r.key != "" {
		r.eg.sticky.invalidate(r.key)
	}
}

// ======================== EgressClient ========================

// EgressClient 是出口客户端的唯一入口。
type EgressClient struct {
	direct *http.Client

	// 配置（只由 Configure 写，热路径只读）
	cfgMu         sync.RWMutex
	proxies       []Socks5Proxy
	activeAddr    string // "" = 直连, "__round_robin__" = 轮询, 其他 = 固定代理
	paidDirect    bool
	stickyEnabled bool

	sticky *stickySession

	// 单代理缓存（固定地址模式）
	rrIndex      uint32
	cachedClient *http.Client
	cachedAddr   string
}

// NewEgressClient 启动时构造，注入直连客户端。
func NewEgressClient(direct *http.Client) *EgressClient {
	return &EgressClient{
		direct: direct,
		sticky: newStickySession(),
	}
}

// Configure 配置热更新。清空所有 sticky 绑定（代理列表变化后旧绑定可能失效）。
func (eg *EgressClient) Configure(
	proxies []Socks5Proxy,
	activeAddr string,
	paidDirect, sticky bool,
) {
	eg.cfgMu.Lock()
	eg.proxies = append([]Socks5Proxy(nil), proxies...) // 防御性复制
	eg.activeAddr = activeAddr
	eg.paidDirect = paidDirect
	eg.stickyEnabled = sticky
	eg.cachedClient = nil
	eg.cachedAddr = ""
	atomic.StoreUint32(&eg.rrIndex, 0)
	eg.cfgMu.Unlock()

	eg.sticky.flush()
}

// Get 返回本次请求的出口客户端。
func (eg *EgressClient) Get(r EgressRequest) EgressResult {
	tier := r.Auth.tier()

	// 快速路径：付费层 + paidDirect → 直连
	eg.cfgMu.RLock()
	paidDirect := eg.paidDirect
	eg.cfgMu.RUnlock()
	if tier == TierPaid && paidDirect {
		return EgressResult{Client: eg.direct, Direct: true}
	}

	// 节点池活跃 → 从池中选取
	if proxyPool.nodeCount() > 0 {
		n := proxyPool.pickForRegion(r.ForceSwitch, r.RequiredRegion)
		if n != nil {
			return EgressResult{
				Client: proxyPool.getClient(n.Fingerprint),
				NodeFP: n.Fingerprint,
			}
		}
		return EgressResult{Client: eg.direct, Direct: true} // 全部冷却中 → 直连
	}

	// 静态 socks5
	return eg.getSocks5Client(r)
}

// getSocks5Client 处理 sticky vs 轮询 vs 固定代理的选择。
func (eg *EgressClient) getSocks5Client(r EgressRequest) EgressResult {
	eg.cfgMu.RLock()
	proxies := eg.proxies
	active := eg.activeAddr
	sticky := eg.stickyEnabled
	eg.cfgMu.RUnlock()

	if active == "" || len(proxies) == 0 {
		return EgressResult{Client: eg.direct, Direct: true}
	}

	// 固定单代理（非轮询）；地址在列表中找不到时内部回退直连，需如实标记
	if active != socks5RR {
		c := eg.getSingleProxyClient(active, proxies)
		return EgressResult{Client: c, Direct: c == eg.direct}
	}

	// 轮询但无 sticky → 简单轮询
	if !sticky {
		idx := atomic.AddUint32(&eg.rrIndex, 1) % uint32(len(proxies))
		return EgressResult{Client: buildProxyClient(proxies[idx])}
	}

	// 轮询 + sticky → 会话绑定
	key := stickyKeyForRequest(r.Auth, r.BodyMap)
	client, _ := eg.sticky.acquire(key, proxies, buildProxyClient)
	return EgressResult{Client: client, key: key, eg: eg}
}

// getSingleProxyClient 返回固定代理地址的缓存客户端。
func (eg *EgressClient) getSingleProxyClient(addr string, proxies []Socks5Proxy) *http.Client {
	eg.cfgMu.RLock()
	if eg.cachedClient != nil && eg.cachedAddr == addr {
		c := eg.cachedClient
		eg.cfgMu.RUnlock()
		return c
	}
	eg.cfgMu.RUnlock()

	for _, p := range proxies {
		if p.Addr == addr {
			c := buildProxyClient(p)
			eg.cfgMu.Lock()
			eg.cachedClient = c
			eg.cachedAddr = addr
			eg.cfgMu.Unlock()
			return c
		}
	}
	return eg.direct
}

// NodesActive 节点池是否有节点（供外部守卫检查用）。
func (eg *EgressClient) NodesActive() bool {
	return proxyPool.nodeCount() > 0
}

// GetSocks5Config 返回当前 socks5 配置快照（管理面板回显用）。
func (eg *EgressClient) GetSocks5Config() (proxies []Socks5Proxy, activeAddr string, paidDirect bool) {
	eg.cfgMu.RLock()
	defer eg.cfgMu.RUnlock()
	proxies = append([]Socks5Proxy(nil), eg.proxies...)
	return proxies, eg.activeAddr, eg.paidDirect
}

// PickForRegion 供区域预检使用：检查指定区域是否有可用节点。
// 返回节点指纹（空 = 无匹配）。
func (eg *EgressClient) PickForRegion(region string) string {
	n := proxyPool.pickForRegion(false, region)
	if n != nil {
		return n.Fingerprint
	}
	return ""
}

// ======================== stickySession ========================

// stickySession 管理会话粘性：同一会话固定出口代理，
// 不同会话之间轮询分散。TTL 过期或显式失效后重新分配。
type stickySession struct {
	mu        sync.Mutex
	entries   map[string]*stickyEntry
	rebindSeq uint32
}

type stickyEntry struct {
	client   *http.Client
	lastUsed time.Time
}

const (
	stickyTTL    = 15 * time.Minute
	stickyMax    = 256
	stickyPublic = "cli://public-shared" // 无会话标识的 public 请求共用同一出口
)

func newStickySession() *stickySession {
	return &stickySession{entries: make(map[string]*stickyEntry)}
}

// acquire 获取或创建 sticky 绑定。返回客户端和是否命中已有绑定。
func (s *stickySession) acquire(key string, proxies []Socks5Proxy, build func(Socks5Proxy) *http.Client) (*http.Client, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	// 懒清理：过期条目
	for k, e := range s.entries {
		if now.Sub(e.lastUsed) > stickyTTL {
			delete(s.entries, k)
		}
	}
	// 超容量驱逐最旧
	if len(s.entries) >= stickyMax {
		var oldestKey string
		var oldest time.Time
		for k, e := range s.entries {
			if oldestKey == "" || e.lastUsed.Before(oldest) {
				oldestKey, oldest = k, e.lastUsed
			}
		}
		delete(s.entries, oldestKey)
	}

	// 命中已有绑定
	if e, ok := s.entries[key]; ok {
		e.lastUsed = now
		return e.client, true
	}

	// 新绑定：FNV32a(key) + 递增序号 → 确定性但每次失效后换出口
	if len(proxies) == 0 {
		return nil, false
	}
	seq := atomic.AddUint32(&s.rebindSeq, 1)
	idx := int((fnv32a(key)+seq)%uint32(len(proxies))) % len(proxies)
	client := build(proxies[idx])
	s.entries[key] = &stickyEntry{client: client, lastUsed: now}
	return client, false
}

// invalidate 解除指定会话的 sticky 绑定。
func (s *stickySession) invalidate(key string) {
	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
}

// flush 清空所有 sticky 绑定（配置变更时调用）。
func (s *stickySession) flush() {
	s.mu.Lock()
	s.entries = make(map[string]*stickyEntry)
	s.mu.Unlock()
}

// ======================== 包级辅助 ========================

func fnv32a(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// stickyKeyForRequest 生成会话粘性键。优先级：账号 token > 会话 user > 公共兜底。
func stickyKeyForRequest(auth UpstreamAuth, bodyMap map[string]any) string {
	if auth.Token != "" {
		return "tok:" + auth.Token
	}
	if u, ok := bodyMap["user"].(string); ok && u != "" {
		return "usr:" + u
	}
	return stickyPublic
}

// buildProxyClient 为指定代理构建带 SOCKS5 dial 的 HTTP 客户端。
// 不设 Client.Timeout——流式 SSE 响应可持续数分钟，全局超时会误杀正常流。
// 改用 ResponseHeaderTimeout 保护头部阶段，Body 读取阶段不受限。
func buildProxyClient(proxy Socks5Proxy) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           socks5Dial(proxy),
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}
