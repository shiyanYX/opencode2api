package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ======================== 节点状态机 ========================

type NodeState int

const (
	NodeAvailable NodeState = iota
	NodeExhausted
	NodeDead
)

func (s NodeState) String() string {
	switch s {
	case NodeExhausted:
		return "exhausted"
	case NodeDead:
		return "dead"
	default:
		return "available"
	}
}

func nodeStateFromString(s string) NodeState {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "exhausted":
		return NodeExhausted
	case "dead":
		return NodeDead
	default:
		return NodeAvailable
	}
}

// ======================= 节点模型 =======================

// RealityConfig 是 vless REALITY 传输的参数（订阅节点或手填节点）。
type RealityConfig struct {
	PublicKey   string `json:"public_key,omitempty"`
	ShortID     string `json:"short_id,omitempty"`
	SpiderX     string `json:"spider_x,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// ProxyNode 表示一个出口代理节点。凭据部分为配置期数据，状态部分为运行时数据。
type ProxyNode struct {
	Name     string         `json:"name"`               // 显示名，订阅节点通常为 "订阅名::节点名"
	Protocol string         `json:"protocol"`           // socks5 | ss | vless | anytls | hy2
	Address  string         `json:"address"`            // 服务器地址（IP 或域名）
	Port     int            `json:"port"`               // 服务器端口
	UserID   string         `json:"user_id,omitempty"`  // vless uuid / socks5 用户名
	Password string         `json:"password,omitempty"` // ss/anytls/hy2 密码、socks5 密码
	Method   string         `json:"method,omitempty"`   // ss 加密方式
	SNI      string         `json:"sni,omitempty"`      // TLS server name
	Flow     string         `json:"flow,omitempty"`     // vless flow（仅支持空 ""）
	Insecure bool           `json:"insecure,omitempty"` // 跳过 TLS 证书校验
	Reality  *RealityConfig `json:"reality,omitempty"`

	Fingerprint string `json:"-"` // 跨订阅去重与状态挂载键

	// ---- 运行时状态（持久化到 proxy_state.json） ----
	State         NodeState `json:"-"`
	MarkedAt      time.Time `json:"-"`
	CooldownUntil time.Time `json:"-"`
	LastError     string    `json:"-"`
	LastUsedAt    time.Time `json:"-"`
}

// ProxyNodeConfig 是配置（config.json / 面板）中可手填的节点精简形态。
type ProxyNodeConfig struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	UserID   string `json:"user_id,omitempty"`
	Password string `json:"password,omitempty"`
	Method   string `json:"method,omitempty"`
	SNI      string `json:"sni,omitempty"`
	Flow     string `json:"flow,omitempty"`
	Insecure bool   `json:"insecure,omitempty"`

	Reality *RealityConfig `json:"reality,omitempty"`
}

func (c ProxyNodeConfig) toNode() *ProxyNode {
	proto := strings.ToLower(c.Protocol)
	// 归一化别名：订阅解析用 "hy2"，手配/面板可能写 "hysteria2"。
	switch proto {
	case "hysteria2", "hy":
		proto = "hy2"
	case "socks":
		proto = "socks5"
	}
	n := &ProxyNode{
		Name:     c.Name,
		Protocol: proto,
		Address:  c.Address,
		Port:     c.Port,
		UserID:   c.UserID,
		Password: c.Password,
		Method:   c.Method,
		SNI:      c.SNI,
		Flow:     c.Flow,
		Insecure: c.Insecure,
	}
	if c.Reality != nil {
		r := *c.Reality
		n.Reality = &r
	}
	return n
}

// computeFingerprint 基于协议、服务器与凭据计算节点指纹。
func computeFingerprint(n *ProxyNode) string {
	h := sha256.New()
	rk, sid, fpr := "", "", ""
	if n.Reality != nil {
		rk, sid, fpr = n.Reality.PublicKey, n.Reality.ShortID, n.Reality.Fingerprint
	}
	fmt.Fprintf(h, "%s|%s|%d|%s|%s|%s|%s|%t|%s|%s|%s|%s",
		n.Protocol, strings.ToLower(n.Address), n.Port, n.UserID, n.Password, n.Method, n.SNI, n.Insecure, n.Flow, rk, sid, fpr)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// ======================= 持久化状态 =======================

type nodeRuntimeState struct {
	State         string    `json:"state"`
	MarkedAt      time.Time `json:"marked_at,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	LastUsedAt    time.Time `json:"last_used_at,omitempty"`
}

var zeroTime time.Time

// ======================= 节点池 =======================

// nodePool 管理全部出口节点（订阅 + 自定义 + 旧 socks5），负责选路与状态机。
type nodePool struct {
	mu sync.RWMutex

	nodes []*ProxyNode          // 按名称升序，展示与轮询共用
	byID  map[string]*ProxyNode // 指纹 → 节点

	activeID   string // 当前生效节点指纹（空 = 直连）
	rrIndex    int    // 轮询游标
	manualPick string // 面板手动指定的优先节点指纹（空 = 自动）

	exhaustedCooldown time.Duration
	deadCooldown      time.Duration

	clients   map[string]*http.Client // 节点指纹 → 缓存客户端
	statePath string
}

var proxyPool = newProxyPool("proxy_state.json")

func newProxyPool(statePath string) *nodePool {
	p := &nodePool{
		byID:              make(map[string]*ProxyNode),
		clients:           make(map[string]*http.Client),
		exhaustedCooldown: 24 * time.Hour,
		deadCooldown:      time.Minute,
		statePath:         statePath,
	}
	p.loadState()
	return p
}

func (p *nodePool) effectiveExhaustedCooldown() time.Duration {
	if p.exhaustedCooldown > 0 {
		return p.exhaustedCooldown
	}
	return 24 * time.Hour
}

func (p *nodePool) effectiveDeadCooldown() time.Duration {
	if p.deadCooldown > 0 {
		return p.deadCooldown
	}
	return time.Minute
}

// eligible 判断节点现在是否可用（冷却期内的标记节点不可用）。
func (p *nodePool) eligible(n *ProxyNode, now time.Time) bool {
	if n.State == NodeAvailable {
		return true
	}
	if n.CooldownUntil.IsZero() {
		return false
	}
	return now.After(n.CooldownUntil)
}

// orderedLogic 返回 [可用节点..., 不可用节点...] 与池是否非空。
func (p *nodePool) orderedLocked() ([]*ProxyNode, bool) {
	if len(p.nodes) == 0 {
		return nil, false
	}
	now := time.Now()
	usable := make([]*ProxyNode, 0, len(p.nodes))
	rest := make([]*ProxyNode, 0, len(p.nodes))
	for _, n := range p.nodes {
		if p.eligible(n, now) {
			usable = append(usable, n)
		} else {
			rest = append(rest, n)
		}
	}
	if len(usable) == 0 {
		return rest, true // 非空但全部不可用（调用方回退直连）
	}
	return append(usable, rest...), true
}

// pick 选择当前生效节点。force=true 表示本轮必须换一个节点（配额切换）。
// 返回 nil 表示池为空或没有可用节点（调用方直连）。
func (p *nodePool) pick(force bool) *ProxyNode {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	ordered, anyPool := p.orderedLocked()
	if !anyPool || len(ordered) == 0 {
		return nil
	}
	hasUsable := false
	for _, n := range ordered {
		if p.eligible(n, now) {
			hasUsable = true
			break
		}
	}
	if !hasUsable {
		return nil // 全部冷却中：直连
	}

	// 1) 不强制且当前仍可用 → 沿用
	if !force && p.activeID != "" {
		if cur := p.byID[p.activeID]; cur != nil && p.eligible(cur, now) {
			return cur
		}
	}
	// 2) 手动指定优先
	if p.manualPick != "" {
		if pref := p.byID[p.manualPick]; pref != nil {
			if !(force && pref.Fingerprint == p.activeID) && p.eligible(pref, now) {
				p.activeID = pref.Fingerprint
				return pref
			}
		} else {
			p.manualPick = ""
		}
	}
	// 3) 轮询/强制切换：按序选第一个可用（force 时排除当前）
	var candidates []*ProxyNode
	for _, n := range ordered {
		if !p.eligible(n, now) {
			continue
		}
		if force && n.Fingerprint == p.activeID {
			continue
		}
		candidates = append(candidates, n)
	}
	if len(candidates) == 0 {
		return nil
	}
	if p.rrIndex >= len(candidates) {
		p.rrIndex = 0
	}
	n := candidates[p.rrIndex]
	p.rrIndex = (p.rrIndex + 1) % len(candidates)
	p.activeID = n.Fingerprint
	return n
}

// consumeNode 获取当前节点（不换）。等价 pick(false) 但保持返回值语义。
func (p *nodePool) current() *ProxyNode {
	return p.pick(false)
}

// switchToNext 手动切换到下一个可用节点，返回新指纹（空 = 没有可用节点可切）。
// manual 面板手动指定：此后 pick 优先返回该节点（若可用）。返回激活的指纹。
func (p *nodePool) manual(fp string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := p.byID[fp]; n == nil {
		return p.activeID
	} else if !p.eligible(n, time.Now()) {
		return p.activeID
	}
	p.manualPick = fp
	p.activeID = fp
	return fp
}

// pickState 只读视图：当前激活指纹 + 手动指定指纹（不切换）。
func (p *nodePool) pickState() (active, manual string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.activeID, p.manualPick
}

func (p *nodePool) switchToNext() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ordered, anyPool := p.orderedLocked()
	if !anyPool || len(ordered) == 0 {
		return ""
	}
	for _, n := range ordered {
		if n.Fingerprint != p.activeID && p.eligible(n, time.Now()) {
			p.activeID = n.Fingerprint
			return n.Fingerprint
		}
	}
	return ""
}

// mark 标记节点状态并触发变更日志。返回 false 表示指纹不存在。
func (p *nodePool) mark(fp string, st NodeState, reason string) bool {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.byID[fp]
	if n == nil {
		return false
	}
	n.State = st
	n.MarkedAt = now
	n.LastError = reason
	switch st {
	case NodeExhausted:
		n.CooldownUntil = now.Add(p.effectiveExhaustedCooldown())
	case NodeDead:
		n.CooldownUntil = now.Add(p.effectiveDeadCooldown())
	default:
		n.CooldownUntil = zeroTime
	}
	if p.activeID == fp {
		p.activeID = ""
	}
	if c := p.clients[fp]; c != nil {
		c.CloseIdleConnections()
		delete(p.clients, fp)
	}
	slog.Info("node state changed",
		"node", n.Name, "fp", fp, "state", st.String(),
		"reason", reason, "until", n.CooldownUntil.Format(time.RFC3339))
	go p.saveState()
	return true
}

func (p *nodePool) unmark(fp string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.byID[fp]
	if n == nil {
		return false
	}
	n.State = NodeAvailable
	n.MarkedAt = time.Time{}
	n.CooldownUntil = time.Time{}
	n.LastError = ""
	go p.saveState()
	return true
}

// currentFp 返回当前生效指纹（无锁读用，仅日志/统计）。
func (p *nodePool) currentFp() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.activeID
}

// testNodeClient 测试钩子：非 nil 时优先构造节点客户端（供集成测试注入 fake）。
var testNodeClient func(n *ProxyNode) *http.Client

// getClient 返回某个节点的 HTTP 客户端（按指纹缓存）。
func (p *nodePool) getClient(fp string) *http.Client {
	if testNodeClient != nil {
		if n := p.byID[fp]; n != nil {
			if c := testNodeClient(n); c != nil {
				return c
			}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.clients[fp]; c != nil {
		return c
	}
	n := p.byID[fp]
	if n == nil {
		return nil
	}
	client := &http.Client{
		Timeout: 300 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialNode(ctx, n, network, addr)
			},
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	p.clients[fp] = client
	return client
}

// setNodes 用新列表重置池；按指纹保留运行时状态，持久化。
func (p *nodePool) setNodes(nodes []*ProxyNode) {
	p.mu.Lock()
	old := p.byID
	next := make([]*ProxyNode, 0, len(nodes))
	byID := make(map[string]*ProxyNode, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		n.Fingerprint = computeFingerprint(n)
		if prev, ok := old[n.Fingerprint]; ok {
			n.State = prev.State
			n.MarkedAt = prev.MarkedAt
			n.CooldownUntil = prev.CooldownUntil
			n.LastError = prev.LastError
			n.LastUsedAt = prev.LastUsedAt
		}
		next = append(next, n)
		byID[n.Fingerprint] = n
	}
	sort.SliceStable(next, func(i, j int) bool { return next[i].Name < next[j].Name })
	p.nodes = next
	p.byID = byID
	p.activeID = ""
	p.rrIndex = 0
	p.mu.Unlock()
	p.saveState()
}

func (p *nodePool) nodeCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.nodes)
}

// snapshot 深拷贝快照（管理面板用）。
func (p *nodePool) snapshot() []*ProxyNode {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*ProxyNode, 0, len(p.nodes))
	for _, n := range p.nodes {
		out = append(out, n)
	}
	return out
}

// ======================= 状态持久化 =======================

func (p *nodePool) saveState() {
	p.mu.RLock()
	st := persistentState{Nodes: map[string]nodeRuntimeState{}}
	for _, n := range p.nodes {
		st.Nodes[n.Fingerprint] = nodeRuntimeState{
			State:         n.State.String(),
			MarkedAt:      n.MarkedAt,
			CooldownUntil: n.CooldownUntil,
			LastError:     n.LastError,
			LastUsedAt:    n.LastUsedAt,
		}
	}
	p.mu.RUnlock()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(p.statePath, data, 0644); err != nil {
		slog.Warn("proxy state save failed", "error", err)
	}
}

func (p *nodePool) loadState() {
	data, err := os.ReadFile(p.statePath)
	if err != nil {
		return
	}
	var st persistentState
	if err := json.Unmarshal(data, &st); err != nil {
		return
	}
	p.mu.Lock()
	for _, n := range p.nodes {
		if rs, ok := st.Nodes[n.Fingerprint]; ok {
			n.State = nodeStateFromString(rs.State)
			n.MarkedAt = rs.MarkedAt
			n.CooldownUntil = rs.CooldownUntil
			n.LastError = rs.LastError
			n.LastUsedAt = rs.LastUsedAt
		}
	}
	p.mu.Unlock()
}

type persistentState struct {
	Nodes map[string]nodeRuntimeState `json:"nodes"`
}
