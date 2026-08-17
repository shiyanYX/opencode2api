package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	Protocol string         `json:"protocol"`           // socks5 | ss | vless | vmess | trojan | anytls | hy2
	Address  string         `json:"address"`            // 服务器地址（IP 或域名）
	Port     int            `json:"port"`               // 服务器端口
	UserID   string         `json:"user_id,omitempty"`  // vless/vmess uuid / socks5 用户名
	Password string         `json:"password,omitempty"` // ss/trojan/anytls/hy2 密码、socks5 密码
	Method   string         `json:"method,omitempty"`   // ss 加密方式
	SNI      string         `json:"sni,omitempty"`      // TLS server name
	Flow     string         `json:"flow,omitempty"`     // vless flow（仅支持空 ""）
	Insecure bool           `json:"insecure,omitempty"` // 跳过 TLS 证书校验
	Reality  *RealityConfig `json:"reality,omitempty"`

	// vmess/trojan 传输与安全选项
	Network  string `json:"network,omitempty"`  // tcp | ws（默认 tcp）
	Path     string `json:"path,omitempty"`     // ws path
	Host     string `json:"host,omitempty"`     // ws Host 头
	AlterIDs uint16 `json:"alter_id,omitempty"` // vmess alterId
	Security string `json:"security,omitempty"` // vmess 加密: auto/aes-128-gcm/chacha20-poly1305/zero/none
	TLS      bool   `json:"tls,omitempty"`      // 是否 TLS 包裹

	Fingerprint string `json:"-"` // 跨订阅去重与状态挂载键

	// ---- 运行时状态（持久化到 proxy_state.json） ----
	State         NodeState `json:"-"`
	MarkedAt      time.Time `json:"-"`
	CooldownUntil time.Time `json:"-"`
	LastError     string    `json:"-"`
	LastUsedAt    time.Time `json:"-"`
	// LatencyMs 最近一次健康探测延迟（毫秒）；0/未探测表示尚无结果。
	LatencyMs   int64     `json:"-"`
	LastProbeAt time.Time `json:"-"`
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

	Network  string `json:"network,omitempty"`
	Path     string `json:"path,omitempty"`
	Host     string `json:"host,omitempty"`
	AlterIDs uint16 `json:"alter_id,omitempty"`
	Security string `json:"security,omitempty"`
	TLS      bool   `json:"tls,omitempty"`

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
		Network:  strings.ToLower(c.Network),
		Path:     c.Path,
		Host:     c.Host,
		AlterIDs: c.AlterIDs,
		Security: c.Security,
		TLS:      c.TLS,
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
	fmt.Fprintf(h, "%s|%s|%d|%s|%s|%s|%s|%t|%s|%s|%s|%s|%s|%s|%s|%d|%s|%t",
		n.Protocol, strings.ToLower(n.Address), n.Port, n.UserID, n.Password, n.Method, n.SNI, n.Insecure, n.Flow, rk, sid, fpr,
		strings.ToLower(n.Network), n.Path, n.Host, n.AlterIDs, strings.ToLower(n.Security), n.TLS)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// ======================= 持久化状态 =======================

type nodeRuntimeState struct {
	State         string    `json:"state"`
	MarkedAt      time.Time `json:"marked_at,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	LastUsedAt    time.Time `json:"last_used_at,omitempty"`
	LatencyMs     int64     `json:"latency_ms,omitempty"`
	LastProbeAt   time.Time `json:"last_probe_at,omitempty"`
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

	probeURL      string        // 健康探测目标（空 = 默认 gstatic 204）
	probeInterval time.Duration // 健康检查周期（0 = 默认 15min）

	clients   map[string]*http.Client // 节点指纹 → 缓存客户端
	statePath string
	saveWG    sync.WaitGroup // 跟踪异步状态写入（测试可排空等待）
}

// 健康检查默认值（Clash url-test 语义：真实连接 + HTTP 2xx 判定可用）。
const (
	defaultHealthInterval = 15 * time.Minute
	defaultProbeURL       = "https://www.gstatic.com/generate_204"
	probeTimeout          = 20 * time.Second
)

var proxyPool = newProxyPool("proxy_state.json")

func newProxyPool(statePath string) *nodePool {
	p := &nodePool{
		byID:              make(map[string]*ProxyNode),
		clients:           make(map[string]*http.Client),
		exhaustedCooldown: time.Hour,
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
	return time.Hour
}

func (p *nodePool) effectiveDeadCooldown() time.Duration {
	if p.deadCooldown > 0 {
		return p.deadCooldown
	}
	return time.Minute
}

// eligible 判断节点当前是否参与路由选路。仅 available 可用：
//   - exhausted：配额冷却到期后由 sweepExpiredLocked 定时恢复（翻回 available）；
//   - dead：只由探测成功（recordProbeSuccess）事件恢复，冷却时间不参与判定。
func (p *nodePool) eligible(n *ProxyNode, now time.Time) bool {
	return n.State == NodeAvailable
}

// sweepExpiredLocked 把冷却已过期的 exhausted 节点翻回 available 并清空标记
// （定时恢复：配额冷却到期即恢复，面板徽标/计数与路由选路保持一致）。
// dead 节点不受影响：其恢复由探测成功驱动（事件恢复）。
// 调用方必须持有写锁；仅在确有翻转时异步持久化一次。
func (p *nodePool) sweepExpiredLocked(now time.Time) {
	dirty := false
	for _, n := range p.nodes {
		if n.State != NodeExhausted {
			continue
		}
		if n.CooldownUntil.IsZero() || !now.After(n.CooldownUntil) {
			continue
		}
		slog.Info("node auto-recovered from exhausted",
			"node", n.Name, "fp", n.Fingerprint,
			"reason_before", n.LastError,
			"until", n.CooldownUntil.Format(time.RFC3339))
		n.State = NodeAvailable
		n.MarkedAt = time.Time{}
		n.CooldownUntil = time.Time{}
		n.LastError = ""
		dirty = true
	}
	if dirty {
		p.saveWG.Add(1)
		go func() { defer p.saveWG.Done(); p.saveState() }()
	}
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
	p.sweepExpiredLocked(now)

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
	p.sweepExpiredLocked(time.Now())
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
	p.sweepExpiredLocked(time.Now())
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
	p.saveWG.Add(1)
	go func() { defer p.saveWG.Done(); p.saveState() }()
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
	p.saveWG.Add(1)
	go func() { defer p.saveWG.Done(); p.saveState() }()
	return true
}

// ======================= 健康检查 =======================

// healthIntervalLocked 返回当前探测周期（调用方持锁）。
func (p *nodePool) healthIntervalLocked() time.Duration {
	if p.probeInterval > 0 {
		return p.probeInterval
	}
	return defaultHealthInterval
}

// probeURL 返回有效探针目标（调用方持锁或用只读辅助）。
func (p *nodePool) effectiveProbeURL() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.probeURL != "" {
		return p.probeURL
	}
	return defaultProbeURL
}

// healthInterval 返回探测周期（无锁只读辅助）。
func (p *nodePool) healthInterval() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.healthIntervalLocked()
}

// probeNode 经节点真实探测目标 URL：完整协议握手 + HTTP 请求，
// 2xx 判定可用，返回毫秒延迟（含握手与 HTTP 往返，Clash url-test 语义）。
func (p *nodePool) probeNode(ctx context.Context, n *ProxyNode, probeURL string) (int64, error) {
	client := p.getClient(n.Fingerprint)
	if client == nil {
		return 0, errors.New("node client unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("probe status %d", resp.StatusCode)
	}
	return time.Since(start).Milliseconds(), nil
}

// probeFilter 限定一次探测的节点子集。
type probeFilter int

const (
	probeAll       probeFilter = iota // 全量（含 exhausted）：面板手动"测速"使用
	probeAvailable                   // 计划巡检：仅 available（dead 由 probeDead 复探，exhausted 由冷却恢复）
	probeDead                        // 每分钟复探 dead 节点（事件恢复）
)

// checkNodes 并发探测池内全部节点并更新状态。失败标记 dead（冷却=探测周期），
// 成功恢复 available 并记录延迟。配额标记（exhausted）不会被探测成功解除。
// 返回检查节点数。面板手动"测速"与测试使用。
func (p *nodePool) checkNodes(ctx context.Context) int {
	return p.checkFiltered(ctx, probeAll)
}

// checkAvailable 计划巡检：只探测 available 节点（刷新延迟/标 dead）。
// exhausted 由配额冷却到期自动恢复，dead 由 checkDead 复探，均不在此轮探测。
func (p *nodePool) checkAvailable(ctx context.Context) int {
	return p.checkFiltered(ctx, probeAvailable)
}

// checkDead 每分钟复探 dead 节点：探测成功即恢复 available；失败仅记录（防刷屏）。
func (p *nodePool) checkDead(ctx context.Context) int {
	return p.checkFiltered(ctx, probeDead)
}

// hasDeadNodes 是否存在待复探的 dead 节点（健康循环据此跳过空转）。
func (p *nodePool) hasDeadNodes() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, n := range p.nodes {
		if n.State == NodeDead {
			return true
		}
	}
	return false
}

func (p *nodePool) checkFiltered(ctx context.Context, filter probeFilter) int {
	probeURL := p.effectiveProbeURL()
	nodes := p.snapshot() // 快照即触发清扫：面板看到的节点状态始终与路由一致
	if len(nodes) == 0 {
		return 0
	}
	var (
		wg    sync.WaitGroup
		probed int
	)
	for _, n := range nodes {
		switch filter {
		case probeAvailable:
			if n.State != NodeAvailable {
				continue
			}
		case probeDead:
			if n.State != NodeDead {
				continue
			}
		}
		probed++
		wg.Add(1)
		go func(n *ProxyNode) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			ms, err := p.probeNode(pctx, n, probeURL)
			if err != nil {
				p.markProbeDead(n.Fingerprint, "probe: "+err.Error())
				slog.Warn("node health probe failed", "node", n.Name, "error", err)
			} else {
				p.recordProbeSuccess(n.Fingerprint, ms)
				slog.Debug("node health probe ok", "node", n.Name, "latency_ms", ms)
			}
		}(n)
	}
	wg.Wait()
	return probed
}

// markProbeDead 探测失败标记：冷却至下一次探测时刻（跟随探测周期），
// 无独立自动重试计时——恢复仅由后续探测成功驱动。
// 已耗尽（exhausted）的节点不会被覆盖：配额冷却优先，探测失败仅作日志。
// 已是 dead 的节点（每分钟复探）失败时仅记录，不重复标记/延长冷却/刷日志。
func (p *nodePool) markProbeDead(fp, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.byID[fp]
	if n == nil {
		return
	}
	now := time.Now()
	if n.State == NodeExhausted {
		slog.Debug("node health probe failed while exhausted (marker kept)",
			"node", n.Name, "fp", fp, "error", reason,
			"until", n.CooldownUntil.Format(time.RFC3339))
		return
	}
	if n.State == NodeDead {
		slog.Debug("node still dead (re-probe failed)",
			"node", n.Name, "fp", fp, "error", reason)
		return
	}
	n.State = NodeDead
	n.MarkedAt = now
	n.LastError = reason
	n.LatencyMs = -1 // 探测失败：无效延迟（前端显示「超时」）
	n.CooldownUntil = now.Add(p.healthIntervalLocked())
	if p.activeID == fp {
		p.activeID = ""
	}
	if c := p.clients[fp]; c != nil {
		c.CloseIdleConnections()
		delete(p.clients, fp)
	}
	slog.Info("node marked dead by health check",
		"node", n.Name, "fp", fp, "reason", reason,
		"until", n.CooldownUntil.Format(time.RFC3339))
	p.saveWG.Add(1)
	go func() { defer p.saveWG.Done(); p.saveState() }()
}

// recordProbeSuccess 探测成功：记录延迟；仅解除 dead 标记（exhausted 不受影响）。
func (p *nodePool) recordProbeSuccess(fp string, ms int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.byID[fp]
	if n == nil {
		return
	}
	n.LatencyMs = ms
	n.LastProbeAt = time.Now()
	if n.State == NodeDead {
		n.State = NodeAvailable
		n.MarkedAt = time.Time{}
		n.CooldownUntil = time.Time{}
		n.LastError = ""
	}
	p.saveWG.Add(1)
	go func() { defer p.saveWG.Done(); p.saveState() }()
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
			n.LatencyMs = prev.LatencyMs
			n.LastProbeAt = prev.LastProbeAt
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

// waitStateSaves 等待所有异步状态写入完成（测试清理用）。
func (p *nodePool) waitStateSaves() {
	p.saveWG.Wait()
}

// snapshot 深拷贝快照（管理面板用）；快照前先清扫，保证面板看到的
// exhausted 节点在冷却到期后即显示为 available（与路由选路一致）。
func (p *nodePool) snapshot() []*ProxyNode {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepExpiredLocked(time.Now())
	out := make([]*ProxyNode, 0, len(p.nodes))
	for _, n := range p.nodes {
		out = append(out, n)
	}
	return out
}

// ======================= 状态持久化 =======================

func (p *nodePool) saveState() {
	p.saveWG.Add(1)
	defer p.saveWG.Done()
	p.mu.RLock()
	st := persistentState{Nodes: map[string]nodeRuntimeState{}}
	for _, n := range p.nodes {
		st.Nodes[n.Fingerprint] = nodeRuntimeState{
			State:         n.State.String(),
			MarkedAt:      n.MarkedAt,
			CooldownUntil: n.CooldownUntil,
			LastError:     n.LastError,
			LastUsedAt:    n.LastUsedAt,
			LatencyMs:     n.LatencyMs,
			LastProbeAt:   n.LastProbeAt,
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
			n.LatencyMs = rs.LatencyMs
			n.LastProbeAt = rs.LastProbeAt
		}
	}
	p.mu.Unlock()
}

type persistentState struct {
	Nodes map[string]nodeRuntimeState `json:"nodes"`
}
