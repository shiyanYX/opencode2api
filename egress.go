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
	"context"
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
	// Model 是 v5 force 选路的 pair 维度；旧路径忽略。
	Model string
	// Context 仅用于 v5_fallback 事件；旧路径忽略，可空。
	Context context.Context
	// ExcludeEgress 是本次选路必须排除的出口键（EgressKey，如 node:<fp>/direct）。
	// 429 漂移守卫按 model 传入已 429 的出口集合，避免换节点又撞回同一批出口。
	ExcludeEgress map[string]bool
}

// EgressResult 持有选中的客户端和失效上下文。
// 调用者使用 Client 发请求，失败时调用 Invalidate() 解除 sticky 绑定。
type EgressResult struct {
	Client *http.Client // 永不为 nil
	NodeFP string       // "" = 直连/sticky（非节点池追踪的节点）
	Direct bool         // true = 真·直连出口（区别于静态 socks5）

	key string        // sticky key，Invalidate() 用
	eg  *EgressClient // 反向引用
	v5  *Attempt      // force 下由 Admission 取得的租约；旧路径为 nil

	// NoCandidate 由 force 选路返回：本轮不允许发送，main 必须回 429+Retry-After，
	// 绝不能把 Client 当成直连回退。
	NoCandidate bool
}

// V5 返回 force 选路取得的租约。旧路径/shadow 为 nil。
func (r EgressResult) V5() *Attempt { return r.v5 }

// EgressKeyDirect 是「真·直连出口」在按出口分桶时的桶键。
// 上游按出口 IP 分桶限流，所以桶的身份必须能唯一指出「哪个出口」，
// 否则一个出口被限流会把另一个出口一起挡掉。
const EgressKeyDirect = "direct"

// EgressKey 返回该出口的分桶键：节点出口用其指纹，直连用固定键。
//
// ★ 「出口身份」用哪个字段：EgressResult.NodeFP。节点池选出来的出口一律带
//
//	指纹；NodeFP == "" 且 Direct == true 才是真·直连出口。
//	（NodeFP 为空但 Direct 为 false 是静态 socks5 —— 它也有自己的 IP，
//	按理也该单独一个桶，但当前熔断只在 Direct 时触发，先与直连合并，
//	避免在没有实例的场景下造出永远不会被解除的假桶。）
func nodeEgressKey(fp string) string { return "node:" + fp }

func (r EgressResult) EgressKey() string {
	if r.Direct || r.NodeFP == "" {
		return EgressKeyDirect
	}
	return nodeEgressKey(r.NodeFP)
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

	// v5 force：节点选择完全交给 Admission。放在付费直连快速路径之后，
	// 未命中时仍按既定语义回退真直连。
	if v5Enabled() == v5Force {
		ctx := r.Context
		if ctx == nil {
			ctx = context.Background()
		}
		if at, ok := v5AdmitContext(ctx, r.Model, r.RequiredRegion, r.ExcludeEgress); ok {
			if at.Direct {
				return EgressResult{Client: eg.direct, Direct: true, v5: at}
			}
			fp := v5NodeFP(at.Egress)
			if fp != "" {
				if c := proxyPool.getClient(fp); c != nil {
					return EgressResult{Client: c, NodeFP: fp, v5: at}
				}
			}
			v5DropAttempt(at)
			v5Fallback(ctx, r.Model, "node_missing")
		}
		// force 下没有租约就是 NoCandidate：由 main 返回 429，绝不直连。
		return EgressResult{Client: eg.direct, NoCandidate: true}
	}

	// 节点池活跃 → 从池中选取
	if proxyPool.nodeCount() > 0 {
		n := proxyPool.pickForRegionExcluding(r.ForceSwitch, r.RequiredRegion, r.ExcludeEgress)
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
