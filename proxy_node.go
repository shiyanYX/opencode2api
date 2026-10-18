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
	"sync/atomic"
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
	Region   string         `json:"region,omitempty"` // 节点所在区域（us/eu/jp/hk 等），用于模型区域限制路由

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
	// ProbeFailStreak 连续探测失败次数（运行时态，不持久化）。
	// 只有连续 probeFailStreakThreshold 次失败才判 dead —— 一次网络抖动不该丢掉一个 IP
	// （2026-10-19 生产：一轮抖动让 86 个节点同时被判死、70 分钟不恢复）。
	ProbeFailStreak int `json:"-"`
	// GateState 推理闸门状态（unknown | ok | rejected），归 T3 所有。
	// T1 的 confirm-then-mark 只写 "rejected"：该出口被上游按 IP 拒绝。
	// 当 gateReviveRequiresProbe=true 时，rejected 节点不接受「冷却到期」的时间复活，
	// 只能由 T3 的闸门感知探针成功复活（见 .research/T1-403-design-v2.md §4.1）。
	GateState string `json:"-"`
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
	Region  string         `json:"region,omitempty"` // 节点所在区域（us/eu/jp/hk 等）
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
		Region:   c.Region,
	}
	if c.Reality != nil {
		r := *c.Reality
		n.Reality = &r
	}
	// 如果未显式指定区域，按优先级尝试推断：
	// 1. 从节点名称推断（快速，无网络请求）
	// 2. 从节点 IP 地址通过 GeoIP 推断（需要网络请求）
	if n.Region == "" {
		n.Region = inferRegionFromName(n.Name)
	}
	if n.Region == "" {
		n.Region = inferRegionFromAddress(n.Address)
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
	GateState     string    `json:"gate_state,omitempty"`
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
	probeInterval time.Duration // 健康检查周期（负数 = 禁用；0 = 默认 15min）

	clients   map[string]*http.Client // 节点指纹 → 缓存客户端
	statePath string
	saveWG    sync.WaitGroup // 跟踪异步状态写入（测试可排空等待）
}

// 健康检查默认值（Clash url-test 语义：真实连接 + HTTP 2xx 判定可用）。
// 默认探测目标改为实际上游域名（opencode.ai），避免 gstatic.com 可达但上游不可达的假性正常。
const (
	defaultHealthInterval = 15 * time.Minute
	defaultProbeURL       = "https://opencode.ai/zen/v1/models"
	probeTimeout          = 20 * time.Second
)

// defaultDialTimeout 节点池【建连】阶段的上限。
//
// ★ 为什么要设：节点池 Transport 的 DialContext 原本是裸闭包，没有任何建连超时，
//
//	建连失败只能等 TCP 层自己超时 —— 实测失败侧有一个 135.2 秒的尖峰
//	（Linux SYN 重传耗尽：1+2+4+8+16+32+64 ≈ 127s + 开销）。再乘上重试预算
//	（loopBudget = maxAttempts 3 + maxQuotaSwitches 5 = 8 次尝试），
//	含 connect_error 的整请求实测 p50=137 秒、max=1,221 秒。
//
// ★ 为什么是 60 秒（依据是成功侧的实测分布，不是拍脑袋）：
//
//	成功侧建连耗时（取自节点池探针记录的 latency_ms，n=141）：
//	  p50=11.7s · p90=15.4s · p99=18.0s · max=18.8s，>20s 的 0 个。
//	取值 → 误杀率（按成功请求的 connect_ok−请求ts 上界口径，n=171,823）：
//	  10s → 58.2%   ★ 绝对不能用（会把整个快峰全杀）
//	  15s → 13.5%   ★ 不能用
//	  30s → 0.941%
//	  60s → 0.234%  ← 采用
//	60s 对实测 max(18.8s) 有 3.2 倍余量，同时把 135s 的病态尾部砍掉一半以上。
//	★ 成功侧是【强双峰】（58 个在 1~3s、82 个在 10~19s，中间 2.6~9.8s 完全空白），
//	所以这个值是按「池子里最慢的那个来源」定的，不是按中位数。
//
// ★ 只限建连这一个阶段：下方 Transport 的 ResponseHeaderTimeout(60s) 保持不变 ——
//
//	那是「已建连、等响应头」的另一个阶段，两者互不覆盖。
const defaultDialTimeout = 60 * time.Second

// dialTimeout 实际生效的建连超时。做成 var 而非直接用 const，是为了让测试能临时
// 下调到秒级验证机制（真等 60 秒的测试没有实用价值）；生产取值恒为 defaultDialTimeout。
var dialTimeout = defaultDialTimeout

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
		// C3 复活契约：被上游按出口 IP 拒绝的节点不接受「冷却到期」的时间复活。
		// 降级开关默认 false（T3 的闸门探针未上线时退化为旧的 5h 冷却，避免永久封禁）。
		if gateReviveRequiresProbe && n.GateState == GateStateRejected {
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

// freeTierRejectedReason 是 confirm-then-mark 确认侧的固定 reason。
// gate_state 的置位只允许通过 markFreeTierRejected（写 rejected）与 unmark（清 unknown）发生。
const freeTierRejectedReason = "freetier:confirmed_ip_rejected"

// mark 标记节点状态并触发变更日志。返回 false 表示指纹不存在。
// 任何【非 FreeTier 拒绝】原因的 mark 都会复位 gate_state —— 否则配额/连接类标记
// 会继承「IP 拒绝」的复活豁免，冷却到期也不复活（T7-I3；也是打开
// gateReviveRequiresProbe 的前置条件）。
func (p *nodePool) mark(fp string, st NodeState, reason string) bool {
	p.mu.Lock()
	ok := p.markLocked(fp, st, reason, GateStateUnknown)
	p.mu.Unlock()
	if !ok {
		return false
	}
	p.saveWG.Add(1)
	go func() { defer p.saveWG.Done(); p.saveState() }()
	return true
}

// markLocked 在已持有写锁的前提下执行标记；gate 为要写入的 gate_state。
func (p *nodePool) markLocked(fp string, st NodeState, reason, gate string) bool {
	now := time.Now()
	n := p.byID[fp]
	if n == nil {
		return false
	}
	n.State = st
	n.MarkedAt = now
	n.LastError = reason
	n.GateState = gate
	n.ProbeFailStreak = 0 // 显式状态变更后重新计数，避免跨状态累积
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
	// 必须一并清 gate_state：否则「面板解除标记」之后节点仍被豁免时间复活，
	// 运维将失去唯一的手工出路（T1-403-design-v2 §4.5）。
	n.GateState = GateStateUnknown
	p.saveWG.Add(1)
	go func() { defer p.saveWG.Done(); p.saveState() }()
	return true
}

// ======================= 免费层 IP 拒绝（confirm-then-mark / T3 契约） =======================

const (
	// GateStateUnknown 未知（默认；连通性探测成功也只写 ok，不代表过了推理闸门）。
	GateStateUnknown = ""
	// GateStateOK 闸门探测确认可通过（由 T3 写）。
	GateStateOK = "ok"
	// GateStateRejected 被上游按出口 IP 拒绝（由 T1 的 confirm-then-mark 写）。
	GateStateRejected = "rejected"
	// freeTierSwitchBudget 单次请求内因 403 FreeTierError 换节点的预算。
	// =1：实测「替身也被 403」的经验频率为 2.91%（非 CL 家族）/ 0%（其它 1::），
	// 预算 1 已覆盖约 97%；预算 2 必须升级为精确 exclude（改 pick 签名，热路径），
	// 本次不做（见 .research/T1-403-design-v2.md §7.2）。
	freeTierSwitchBudget = 1
)

var (
	// gateReviveRequiresProbe 是 C3 契约的降级开关：默认 false = 旧行为（冷却到期自动复活）。
	// T3 的闸门感知探针上线后才应置 true；否则 gate_state=rejected 的节点将永久封禁
	// （只剩面板 unmark 一条手工出路）。见 .research/T1-403-design-v2.md §4.1。
	gateReviveRequiresProbe = false
	// shapeSuspectNow 由 T3 接线（闸 3 全局形状闩）：为真时抑制节点级 mark。
	// T3 未上线时恒为 false（不抑制）。
	shapeSuspectNow = func() bool { return false }
	// freeTierFloor 由 T3 接线（闸 2 池底保护）：available 数低于等于它会拒绝 mark。
	// 默认 0 ⇒ 仅在池子真的没有可用节点时才拒绝。
	freeTierFloor = func() int { return 0 }
)

// availableCount 报告当前可路由（available）的节点数。
func (p *nodePool) availableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	c := 0
	for _, n := range p.nodes {
		if p.eligible(n, now) {
			c++
		}
	}
	return c
}

// freeTierMarkAllowed 是 confirm-then-mark 确认侧的闸门（T3 闸 3 + 池底）。
// 返回 (是否允许, 抑制原因)。
func (p *nodePool) freeTierMarkAllowed() (bool, string) {
	if shapeSuspectNow() {
		return false, "shape_latch"
	}
	if p.availableCount() <= freeTierFloor() {
		return false, "pool_floor"
	}
	return true, ""
}

// markFreeTierRejected 把「被上游按出口 IP 拒绝」的节点标记为 NodeExhausted，
// 并写入 gate_state="rejected"。只在 confirm-then-mark 的【确认侧】调用
// （即：换节点后确实成功了）。失败路径上绝不调用，形态回归时池子零损伤。
// ★ 状态与 gate_state 在同一次加锁内完成，避免「先标状态、再补字段」中间态。
func (p *nodePool) markFreeTierRejected(fp string) bool {
	p.mu.Lock()
	ok := p.markLocked(fp, NodeExhausted, freeTierRejectedReason, GateStateRejected)
	p.mu.Unlock()
	if !ok {
		return false
	}
	p.saveWG.Add(1)
	go func() { defer p.saveWG.Done(); p.saveState() }()
	return true
}

// ======================= 健康检查 =======================

const (
	// probeFailStreakThreshold 连续探测失败多少次才判 dead。
	// =2：一次网络抖动/上游偶尔的 TLS 失败不判死；两次（相隔一个周期）说明节点确实坏。
	probeFailStreakThreshold = 2
	// probeMaxConcurrent 单轮探测的最大并发数。生产曾出现「86 个 dead ⇒ 每分钟 86 路
	// 并发探测」的自压制循环，必须限并发。
	probeMaxConcurrent = 8
)

// testProbeNode 测试钩子：非 nil 时替代真实探测（与 testNodeClient 同一风格）。
var testProbeNode func(ctx context.Context, n *ProxyNode, probeURL string) (int64, error)

// blipStreak 连续多少轮探测被判为「全局抖动」。
// 抖动轮次一节点都不判死，缺了它就会静默掩盖「本机网络长期异常」——所以要有升级信号。
var blipStreak atomic.Int64

// probeRoundLooksLikeGlobalBlip 判断一整轮探测的失败是否像「我们自己的网络问题」。
// 依据（2026-10-19 生产）：一次约 2 分钟的出口抖动让 90/218 个节点在同一轮超时。
// 判据：一轮里失败 >=20 个，或（该轮 >=8 个且失败占 >=75%）。
// 命中时整轮失败不予采信 —— 不判死、也不累积 streak。
func probeRoundLooksLikeGlobalBlip(total, failed int) bool {
	if total < 8 || failed <= 0 {
		return false
	}
	if failed >= 20 {
		return true
	}
	return failed*4 >= total*3
}

// healthIntervalLocked 返回当前探测周期（调用方持锁）。
// probeInterval 为负数表示禁用健康检查；0 表示使用默认 15min。
func (p *nodePool) healthIntervalLocked() time.Duration {
	if p.probeInterval < 0 {
		return 0 // 禁用
	}
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
//
// 当探测目标为 opencode.ai 时，自动注入 Authorization 与 x-opencode-session 头，
// 模拟真实请求以验证代理对上游域名的可达性（而非仅验证通用互联网连通性）。
func (p *nodePool) probeNode(ctx context.Context, n *ProxyNode, probeURL string) (int64, error) {
	if testProbeNode != nil {
		return testProbeNode(ctx, n, probeURL)
	}
	client := p.getClient(n.Fingerprint)
	if client == nil {
		return 0, errors.New("node client unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return 0, err
	}
	// 上游 opencode.ai 需要认证头；探测时使用公共身份 + 有效 session 格式。
	if strings.Contains(probeURL, "opencode.ai") {
		req.Header.Set("Authorization", "Bearer public")
		if sid := getOrCreateSessionByNode(n.Fingerprint); sid != "" {
			req.Header.Set("x-opencode-session", sid)
		}
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

// String 让日志能区分「是哪一种轮次触发了闸门」。
func (f probeFilter) String() string {
	switch f {
	case probeAll:
		return "all"
	case probeAvailable:
		return "available"
	case probeDead:
		return "dead"
	}
	return "unknown"
}

const (
	probeAll       probeFilter = iota // 全量（含 exhausted）：面板手动"测速"使用
	probeAvailable                    // 计划巡检：仅 available（dead 由 probeDead 复探，exhausted 由冷却恢复）
	probeDead                         // 每分钟复探 dead 节点（事件恢复）
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
		wg       sync.WaitGroup
		probed   int
		mu       sync.Mutex
		failedFP []string
		failWhy  = map[string]string{}
		sem      = make(chan struct{}, probeMaxConcurrent)
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
		sem <- struct{}{}
		go func(n *ProxyNode) {
			defer wg.Done()
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			ms, err := p.probeNode(pctx, n, probeURL)
			if err != nil {
				mu.Lock()
				failedFP = append(failedFP, n.Fingerprint)
				failWhy[n.Fingerprint] = err.Error()
				mu.Unlock()
				// 降 Debug：markProbeDead 已按「首次 / 重复 / exhausted」三级分级
				// （Info / Debug / Debug，见 :650 / :634 / :628），重复失败的节流
				// 已在它内部完成。这条无状态转移信息的重复记录不应再以 Warn 刷屏
				// ——dead 节点每分钟复探一次，实测占应用日志 56.8%。
				// 保留而不删除：markProbeDead 在 p.byID[fp] 已被 setNodes 换掉的
				// 竞态下会提前 return，那条失败会完全无日志。
				slog.Debug("node health probe failed", "node", n.Name, "error", err)
			} else {
				p.recordProbeSuccess(n.Fingerprint, ms)
				slog.Debug("node health probe ok", "node", n.Name, "latency_ms", ms)
			}
		}(n)
	}
	wg.Wait()

	// 全局抖动闸【只对可用节点巡检轮次生效】。
	// dead 复探轮天然大面积失败（它们本来就是 dead），若参与判定会让 WARN 每分钟一条、
	// ERROR 每 10 分钟一条且永久常亮 —— 恰好反转了这条告警本该提供的信号（R2 复审 D1）。
	guarded := filter == probeAvailable
	if guarded && probeRoundLooksLikeGlobalBlip(probed, len(failedFP)) {
		n := blipStreak.Add(1)
		slog.Warn("probe_round_global_blip",
			"filter", filter.String(),
			"probed", probed, "failed", len(failedFP), "consecutive", n,
			"action", "整轮失败不予采信（疑似本机出口/网络问题）")
		if n == 3 || n%10 == 0 {
			slog.Error("probe_round_global_blip_persistent", "consecutive", n,
				"hint", "连续多轮抖动：节点不会被判死，但本机网络/出口可能长期异常，需人工检查")
		}
		return probed
	}
	if guarded {
		blipStreak.Store(0)
	}
	for _, fp := range failedFP {
		p.markProbeDead(fp, "probe: "+failWhy[fp])
	}
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
	// 连续失败守卫：一次失败只累计 streak，不判死 —— 一次网络抖动不该丢掉一个出口 IP
	// （2026-10-19 生产：一轮抖动让 86 个节点同时被判死，其中至少一个 TCP 实际可达）。
	n.ProbeFailStreak++
	if n.ProbeFailStreak < probeFailStreakThreshold {
		slog.Info("node health probe failed (streak, not dead yet)",
			"node", n.Name, "fp", fp, "streak", n.ProbeFailStreak,
			"threshold", probeFailStreakThreshold, "error", reason)
		return
	}
	n.ProbeFailStreak = 0
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
	n.ProbeFailStreak = 0 // 探测成功即清零，避免历史抖动累积成误判
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

// nameOf 返回节点指纹对应的显示名（未命中返回空串）。
// 供调用日志等把指纹翻译成可读节点名，便于外部快速定位问题节点。
func (p *nodePool) nameOf(fp string) string {
	if fp == "" {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if n := p.byID[fp]; n != nil {
		return n.Name
	}
	return ""
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
		Transport: &http.Transport{
			// ★ 建连超时必须在这里加，不能套 net.Dialer{Timeout:}：
			// dialNode 走的是内嵌 mihomo 内核（mihomo.go:196 mihomoDial），
			// 根本没有 net.Dialer 可套。唯一能约束它的就是 ctx，
			// 所以用 context.WithTimeout 把 ctx 截止时间传下去。
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				dctx, cancel := context.WithTimeout(ctx, dialTimeout)
				defer cancel()
				return dialNode(dctx, n, network, addr)
			},
			// ↓ 响应头阶段的上限，【不在本次改动范围】，保持 60s。
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
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
		// 自动通过 GeoIP 补全区域信息
		if n.Region == "" && n.Address != "" {
			n.Region = inferRegionFromAddress(n.Address)
		}
		if prev, ok := old[n.Fingerprint]; ok {
			n.State = prev.State
			n.MarkedAt = prev.MarkedAt
			n.CooldownUntil = prev.CooldownUntil
			n.LastError = prev.LastError
			n.LastUsedAt = prev.LastUsedAt
			n.LatencyMs = prev.LatencyMs
			n.LastProbeAt = prev.LastProbeAt
			n.GateState = prev.GateState
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

// hasEligibleNode 报告池中是否存在可路由的 available 节点。
// 与选路语义一致：先清扫冷却到期的 exhausted 节点（定时恢复），
// dead 节点只认探测成功，不在这里翻回。
func (p *nodePool) hasEligibleNode() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.sweepExpiredLocked(now)
	for _, n := range p.nodes {
		if p.eligible(n, now) {
			return true
		}
	}
	return false
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
			GateState:     n.GateState,
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
			n.GateState = rs.GateState
		}
	}
	p.mu.Unlock()
}

type persistentState struct {
	Nodes map[string]nodeRuntimeState `json:"nodes"`
}

// ======================= 区域推断 =======================

// regionPatterns 定义区域名称到标准区域标识的映射（大小写不敏感）。
var regionPatterns = map[string]string{
	"us":             "us",
	"usa":            "us",
	"united states":  "us",
	"america":        "us",
	"eu":             "eu",
	"europe":         "eu",
	"de":             "eu",
	"germany":        "eu",
	"fr":             "eu",
	"france":         "eu",
	"uk":             "eu",
	"united kingdom": "eu",
	"jp":             "jp",
	"japan":          "jp",
	"hk":             "hk",
	"hong kong":      "hk",
	"sg":             "sg",
	"singapore":      "sg",
	"tw":             "tw",
	"taiwan":         "tw",
	"kr":             "kr",
	"korea":          "kr",
	"au":             "au",
	"australia":      "au",
	"ca":             "ca",
	"canada":         "ca",
}

// inferRegionFromName 从节点名称推断区域。支持格式：
//   - "订阅名::US-xxx" → "us"
//   - "US-38.153.152.244:9594" → "us"
//   - "webshare-main::US-xxx" → "us"
//   - 名称中包含区域关键词（us/eu/jp/hk 等）
//
// 返回空字符串表示无法推断。
func inferRegionFromName(name string) string {
	// 取 :: 后的最后一段（节点名）
	seg := name
	if idx := strings.LastIndex(name, "::"); idx >= 0 {
		seg = name[idx+2:]
	}
	seg = strings.TrimSpace(seg)
	if seg == "" {
		return ""
	}
	lower := strings.ToLower(seg)

	// 尝试匹配 "XX-" 前缀（如 US-38.153.152.244:9594）
	if dashIdx := strings.Index(seg, "-"); dashIdx >= 2 && dashIdx <= 4 {
		prefix := strings.ToLower(seg[:dashIdx])
		if region, ok := regionPatterns[prefix]; ok {
			return region
		}
	}

	// 尝试匹配连字符分隔的完整区域名称（如 hong-kong, united-states）
	for pattern, region := range regionPatterns {
		if len(pattern) >= 3 {
			// 检查连字符分隔的情况（如 "hong-kong-01"）
			normalized := strings.ReplaceAll(strings.ReplaceAll(lower, "-", ""), " ", "")
			patternNormalized := strings.ReplaceAll(pattern, " ", "")
			if strings.Contains(normalized, patternNormalized) {
				return region
			}
			// 检查 "pattern-" 前缀
			if strings.HasPrefix(lower, pattern+"-") || strings.HasPrefix(lower, pattern+" ") {
				return region
			}
		}
	}

	// 尝试匹配整个段的前缀（如 "us01" "jp-tokyo"）
	for pattern, region := range regionPatterns {
		if len(pattern) >= 2 && strings.HasPrefix(lower, pattern) {
			return region
		}
	}
	return ""
}

// pickForRegion 选择当前生效节点，限定指定区域。
// requiredRegion 为空时退化为普通 pick。
// 返回 nil 表示没有可用节点（调用方返回错误或回退直连）。
func (p *nodePool) pickForRegion(force bool, requiredRegion string) *ProxyNode {
	if requiredRegion == "" {
		return p.pick(force)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.sweepExpiredLocked(now)

	ordered, anyPool := p.orderedLocked()
	if !anyPool || len(ordered) == 0 {
		return nil
	}

	// 过滤：仅选匹配区域的节点
	var candidates []*ProxyNode
	for _, n := range ordered {
		if !p.eligible(n, now) {
			continue
		}
		if n.Region != requiredRegion {
			continue
		}
		if force && n.Fingerprint == p.activeID {
			continue
		}
		candidates = append(candidates, n)
	}
	if len(candidates) == 0 {
		return nil // 无匹配区域的可用节点
	}
	if p.rrIndex >= len(candidates) {
		p.rrIndex = 0
	}
	n := candidates[p.rrIndex]
	p.rrIndex = (p.rrIndex + 1) % len(candidates)
	p.activeID = n.Fingerprint
	return n
}

// availableRegions 返回当前可用节点的所有唯一区域标识（含空字符串表示未标记区域）。
func (p *nodePool) availableRegions() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	seen := map[string]bool{}
	var regions []string
	for _, n := range p.nodes {
		if !p.eligible(n, now) {
			continue
		}
		if !seen[n.Region] {
			seen[n.Region] = true
			regions = append(regions, n.Region)
		}
	}
	return regions
}
