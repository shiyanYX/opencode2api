package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
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
	// NodeProbation 是 exhausted 冷却到期后的「试探态」：只放行一次证据请求，
	// 成功才转 available，失败重新冷却。见 I3「恢复必须有证据」。
	NodeProbation
)

func (s NodeState) String() string {
	switch s {
	case NodeExhausted:
		return "exhausted"
	case NodeDead:
		return "dead"
	case NodeProbation:
		return "probation"
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
	case "probation":
		return NodeProbation
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
	// ---- probation（冷却到期后的试探态，I3） ----
	// ProbationUntil 是本次试探证据的截止时间；到期仍无证据则回 exhausted。
	ProbationUntil time.Time `json:"-"`
	// ProbationTried 表示这轮 probation 的唯一一次证据请求是否已被选中发出。
	ProbationTried bool `json:"-"`
	// ProbationFailures 是连续 probation 失败次数，用于指数退避（60s→120s→300s→…→30min）。
	ProbationFailures int `json:"-"`
	// LastAttemptKind 最近一次被选中的用途（normal | probation），供观测/测试读取。
	LastAttemptKind string `json:"-"`
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
	State             string    `json:"state"`
	MarkedAt          time.Time `json:"marked_at,omitempty"`
	CooldownUntil     time.Time `json:"cooldown_until,omitempty"`
	LastError         string    `json:"last_error,omitempty"`
	LastUsedAt        time.Time `json:"last_used_at,omitempty"`
	LatencyMs         int64     `json:"latency_ms,omitempty"`
	LastProbeAt       time.Time `json:"last_probe_at,omitempty"`
	GateState         string    `json:"gate_state,omitempty"`
	ProbationUntil    time.Time `json:"probation_until,omitempty"`
	ProbationTried    bool      `json:"probation_tried,omitempty"`
	ProbationFailures int       `json:"probation_failures,omitempty"`
}

// modelQuotaEntry 是 per (fingerprint, model) 的配额冷却记录。
// 429 只应影响该 model 的资格，不影响节点对其他 model 的可用性（设计 §4.2）。
type modelQuotaEntry struct {
	Until    time.Time
	Reason   string
	MarkedAt time.Time
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

	// stateRestored 表示「本进程内已经把 proxy_state.json 合并进节点列表」。
	// 只为首次装入节点时恢复一次：进程内的状态比磁盘新，后续 setNodes（订阅刷新）
	// 若再读盘，会把刚判死的节点写回 available。
	stateRestored bool
	// lastProbeOK 是「上一轮可用节点巡检」里探测成功的指纹集合（对照组候选池）。
	// 由 checkFiltered(probeAvailable) 在每轮结束时整体替换；只读快照给选样用。
	lastProbeOK []string

	// T2：403 风暴遏制。sub403 是「已确认」403 的订阅级熔断状态；
	// pool403 / poolAttempts 是池级 10 分钟窗口内的原始 403 与尝试节点集合。
	// poolHaltUntil 非零且未到期时，强制换节点 fail-fast（粘滞 pick(false) 不受影响）。
	sub403         map[string]*sub403State
	pool403        map[string]time.Time
	poolAttempts   map[string]time.Time
	poolHaltUntil  time.Time
	lastCandidates int // 最近一次 pick 的候选数（switch 事件埋点，近似并发安全）

	// modelQuota 是 per (指纹, model) 的配额冷却表：429 只让该 model 的该出口暂不可用。
	// 键为 fp+"\x00"+model，避免拼接歧义。
	modelQuota map[string]modelQuotaEntry
}

const (
	sub403Window      = 10 * time.Minute
	sub403Threshold   = 3
	sub403BaseBackoff = 10 * time.Minute
	sub403MaxBackoff  = 30 * time.Minute
	sub403Silence     = 30 * time.Minute // 30min 无新 confirmed 403 ⇒ escalations 归零

	pool403Window = 10 * time.Minute
	pool403Min    = 8
	pool403Max    = 24
	poolHaltFor   = 10 * time.Minute

	// probation（I3）：冷却到期只进试探态，证据成功才 available。
	defaultProbationDuration = 10 * time.Minute
	// probationMaxConcurrent 限制全池同时在途的试探数，防止一次到期潮把池子同时打出去。
	probationMaxConcurrent = 4
	probationBackoffMax    = 30 * time.Minute
)

// probationBackoff 返回 probation 连续失败后的冷却时长：60s → 120s → 300s → 翻倍至 30min。
func probationBackoff(failures int) time.Duration {
	switch {
	case failures <= 1:
		return 60 * time.Second
	case failures == 2:
		return 120 * time.Second
	case failures == 3:
		return 300 * time.Second
	}
	d := 300 * time.Second
	for i := 3; i < failures; i++ {
		d *= 2
		if d >= probationBackoffMax {
			return probationBackoffMax
		}
	}
	return d
}

type sub403State struct {
	strikes     int
	windowStart time.Time
	until       time.Time
	escalations int
	last403     time.Time
}

type switchRank struct {
	recentSuccess bool
	samples       int
	score         float64
}

// switchRankFor 默认从 nodeScoring 取「近期成功 + 有效样本 + 分数」。
// 测试可替换，避免依赖全局评分存储。
var switchRankFor = func(n *ProxyNode, now time.Time) switchRank {
	st, ok := nodeScoring.statsCopy(n.Fingerprint)
	if !ok {
		return switchRank{}
	}
	r := switchRank{
		recentSuccess: !st.LastSuccessAt.IsZero() && now.Sub(st.LastSuccessAt) <= time.Hour,
		samples:       int(st.EffN),
	}
	if r.samples < 3 {
		r.recentSuccess = false
	}
	views := nodeScoring.scoreSnapshot([]nodeScoringSnapshotInput{{
		FP: n.Fingerprint, Name: n.Name, State: n.State.String(),
		LatencyMs: st.LatencyMs, LastProbeAt: st.LastProbeAt,
		LastError: st.LastProbeError, LastUsedAt: n.LastUsedAt,
	}}, now)
	if len(views) > 0 {
		r.score = views[0].Score
	}
	return r
}

// 健康检查默认值（Clash url-test 语义：真实连接 + HTTP 2xx 判定可用）。
// 默认探测目标改为实际上游域名（opencode.ai），避免 gstatic.com 可达但上游不可达的假性正常。
const (
	defaultHealthInterval = 15 * time.Minute
	defaultProbeURL       = "https://opencode.ai/zen/v1/models"
	probeTimeout          = 20 * time.Second
)

// T3 超时分层（2026-10-26）：节点池【建连】阶段的硬上限，替代旧值 60s。
//
// 旧值 60s（legacyDialTimeout）把失败侧的病态等待留在原地：单个建连可等 135s
// （SYN 重传耗尽），单次失败最差 171s（04-prior-art 生产记录；本仓库 call_log 快照
// 里单请求上限 120s 是 loopBudget 截断的结果，不是建连超时）。
//
// 新值 8s 是「先量后定」的结果：用生产 call_log 的成功请求做误杀率上界（ttft 覆盖
// 建连+响应头+首 token，因此是纯建连误杀率的严格上界）：
//
//	5s → 29.1%（10-26 窗口 n=864） / 21.8%（全窗口 n=9,045）
//	8s → 15.1% / 13.0%
//
// 两档都远高于 0.5% 门槛，按计划规则「5s 误杀 >0.5% 则取 8s」采用 8s。上界包含
// 建连之后的响应头与首 token 等待，真实建连阶段远小于此；口径与数据见
// docs/research/scoring2/08-task3-report.md。
const legacyDialTimeout = 60 * time.Second // 旧值：仅保留作历史对照，不再生效

var connectTimeout = 8 * time.Second

// dialTimeout 实际生效的建连超时。保留旧名字：既有调用点（newNodeTransport、
// bun_egress）与测试都读它；production 取值 = connectTimeout。
var dialTimeout = connectTimeout

// globalTTFTP95 是【全局】首字节等待 p95（不按节点）。
// 依据 .research/scoring2/05-latency.md：上线后节点对 ttft 的 ICC=0.0%，按节点取
// p95 只会引入噪声。取值 13.06s = 2026-10-26 生产 call_log 中 864 条成功请求的
// 精确 ttft p95（口径见 08-task3-report.md）。
var globalTTFTP95 = 13060 * time.Millisecond

// firstByteDeadline 返回首字节（响应头）等待上限 = max(15s, 全局 ttft p95 × 1.5)。
// T3 的目标不是压 p95（p95 由 mimo 长生成驱动），只是给「永远等不到首字节」设上限。
func firstByteDeadline(globalP95 time.Duration) time.Duration {
	d := globalP95 * 3 / 2
	if d < 15*time.Second {
		return 15 * time.Second
	}
	return d
}

// firstByteTimeout 实际生效的响应头等待上限。三处 Transport（直连 / 节点池 / Bun）
// 共用同一个值，保证「首字节」在各个出口语义一致。做成 var 便于测试注入。
var firstByteTimeout = firstByteDeadline(globalTTFTP95)

// tlsHandshakeTimeout TLS 握手阶段的上限。
// 为什么必须有它：Go 里 http.Transport.TLSHandshakeTimeout 的零值表示【不超时】。
// 2026-10-19 生产实测：节点接受 TCP 但 TLS 握手被黑洞时，单次 upstream attempt 挂了
// 940~1034 秒（15~17 分钟）才报 transport_error，请求日志里 status=0（连响应头都没拿到）
// ⇒ 挂住的阶段只能是建连或 TLS 握手，而这两者此前只有建连有 ctx 上限。
// 做成 var 便于测试注入更短的值。
var tlsHandshakeTimeout = 15 * time.Second

var proxyPool = newProxyPool("proxy_state.json")

func newProxyPool(statePath string) *nodePool {
	p := &nodePool{
		byID:              make(map[string]*ProxyNode),
		clients:           make(map[string]*http.Client),
		sub403:            make(map[string]*sub403State),
		pool403:           make(map[string]time.Time),
		poolAttempts:      make(map[string]time.Time),
		modelQuota:        make(map[string]modelQuotaEntry),
		exhaustedCooldown: time.Hour,
		deadCooldown:      time.Minute,
		statePath:         statePath,
	}
	// ★ 这里【不能】调用 loadState：此刻 p.nodes 还是空的，循环体一次都不会执行，
	// proxy_state.json 写了也从来没被读进来（C-7）。状态恢复改在 setNodes 首次
	// 装入节点列表之后进行（见 setNodes 里的 restoreStateLocked 调用点）。
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

// eligible 判断节点当前是否参与路由选路：
//   - available：正常可用；
//   - probation：可以再放行一次证据请求（ProbationTried 用掉后就不再选）；
//   - exhausted：冷却到期后由 sweepExpiredLocked 转 probation（绝不直接 available）；
//   - dead：只由探测成功（recordProbeSuccess）事件恢复，冷却时间不参与判定。
func (p *nodePool) eligible(n *ProxyNode, now time.Time) bool {
	switch n.State {
	case NodeProbation:
		if n.ProbationTried {
			return false
		}
		if !n.ProbationUntil.IsZero() && now.After(n.ProbationUntil) {
			return false
		}
		// 全池在途试探额度：已发出、尚未得到成功/失败结论的 probation 节点数。
		if p.probationInFlightLocked() >= probationMaxConcurrent {
			return false
		}
	case NodeAvailable:
		// 继续走下面的订阅隔离判断。
	default:
		return false
	}
	// T2：订阅级 403 熔断。隔离只影响选路，不改节点状态/分数（可恢复、可解释）。
	return !p.subQuarantinedLocked(n.Name, now)
}

// probationInFlightLocked 统计「已发出证据请求、结果尚未落定」的 probation 节点。
// 调用方至少持有读锁。
func (p *nodePool) probationInFlightLocked() int {
	count := 0
	for _, n := range p.nodes {
		if n.State == NodeProbation && n.ProbationTried {
			count++
		}
	}
	return count
}

// sweepExpiredLocked 处理时间驱动的状态迁移：
//   - exhausted 冷却到期 ⇒ probation（只给一次证据机会，不得直接 available）；
//   - probation 到期仍无证据 ⇒ 按指数退避回 exhausted。
//
// dead 节点不受影响：其恢复由探测成功驱动（事件恢复）。
// 调用方必须持有写锁；仅在确有翻转时异步持久化一次。
func (p *nodePool) sweepExpiredLocked(now time.Time) {
	dirty := false
	for _, n := range p.nodes {
		switch n.State {
		case NodeExhausted:
			// C3 复活契约：被上游按出口 IP 拒绝的节点不接受「冷却到期」的时间复活。
			// 降级开关默认 false（T3 的闸门探针未上线时退化为旧的 5h 冷却，避免永久封禁）。
			if gateReviveRequiresProbe && n.GateState == GateStateRejected {
				continue
			}
			if n.CooldownUntil.IsZero() || !now.After(n.CooldownUntil) {
				continue
			}
			slog.Info("node entered probation after cooldown",
				"node", n.Name, "fp", n.Fingerprint,
				"reason_before", n.LastError,
				"until", n.CooldownUntil.Format(time.RFC3339),
				"probation_until", now.Add(defaultProbationDuration).Format(time.RFC3339))
			n.State = NodeProbation
			n.ProbationUntil = now.Add(defaultProbationDuration)
			n.ProbationTried = false
			n.CooldownUntil = time.Time{}
			dirty = true
		case NodeProbation:
			if n.ProbationUntil.IsZero() || now.Before(n.ProbationUntil) {
				continue
			}
			// 超时未拿到证据（或结果未回写）：不得恢复为 available，重新冷却。
			n.ProbationFailures++
			cooldown := probationBackoff(n.ProbationFailures)
			slog.Info("node probation expired without evidence",
				"node", n.Name, "fp", n.Fingerprint,
				"failures", n.ProbationFailures,
				"cooldown", cooldown.String())
			n.State = NodeExhausted
			n.MarkedAt = now
			n.CooldownUntil = now.Add(cooldown)
			n.LastError = "probation:no_evidence"
			n.ProbationUntil = time.Time{}
			n.ProbationTried = false
			dirty = true
		}
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
// 返回 nil 表示池为空、没有可用节点、或 403 风暴 halt（调用方 fail-fast）。
func (p *nodePool) pick(force bool) *ProxyNode {
	n, _ := p.pickWithMeta(force)
	return n
}

// pickWithMeta 与 pick 相同，额外返回本次被迫换节点的候选数（switch 事件埋点用）。
func (p *nodePool) pickWithMeta(force bool) (*ProxyNode, int) {
	return p.pickWithMetaExcluding(force, nil)
}

// pickWithMetaExcluding 在 pickWithMeta 基础上排除指定出口（429 漂移守卫）。
// nil/空 map 等价于原行为；非强制路径若当前节点被排除，也会立即改选。
func (p *nodePool) pickWithMetaExcluding(force bool, exclude map[string]bool) (*ProxyNode, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.lastCandidates = 0
	if force && p.poolHaltedLocked(now) {
		p.lastCandidates = 0
		return nil, 0
	}
	p.sweepExpiredLocked(now)

	ordered, anyPool := p.orderedLocked()
	if !anyPool || len(ordered) == 0 {
		return nil, 0
	}
	hasUsable := false
	for _, n := range ordered {
		if p.eligible(n, now) {
			hasUsable = true
			break
		}
	}
	if !hasUsable {
		return nil, 0 // 全部冷却/隔离中：直连（调用方另做全订阅隔离 fail-fast）
	}

	excluded := func(n *ProxyNode) bool {
		return n != nil && len(exclude) > 0 && exclude[nodeEgressKey(n.Fingerprint)]
	}
	// 1) 不强制且当前仍可用 → 沿用（T2 硬约束：粘滞语义一个字不改；
	//    但 429 漂移集合里的出口例外——必须换出去）。
	if !force && p.activeID != "" {
		if cur := p.byID[p.activeID]; cur != nil && p.eligible(cur, now) && !excluded(cur) {
			p.markUsedLocked(cur, now)
			return cur, 0
		}
	}
	// 2) 手动指定优先
	if p.manualPick != "" {
		if pref := p.byID[p.manualPick]; pref != nil {
			if !(force && pref.Fingerprint == p.activeID) && p.eligible(pref, now) && !excluded(pref) {
				p.activeID = pref.Fingerprint
				p.markUsedLocked(pref, now)
				return pref, 0
			}
		} else {
			p.manualPick = ""
		}
	}
	// 3) 轮询/强制切换：按序选第一个可用（force 时排除当前）
	from := p.activeID
	candidates := make([]*ProxyNode, 0, len(ordered))
	for _, n := range ordered {
		if !p.eligible(n, now) {
			continue
		}
		if force && n.Fingerprint == p.activeID {
			continue
		}
		if excluded(n) {
			continue
		}
		candidates = append(candidates, n)
	}
	if force {
		candidates = p.switchCandidatesLocked(candidates, from, now)
	}
	p.lastCandidates = len(candidates)
	if len(candidates) == 0 {
		return nil, 0
	}
	var n *ProxyNode
	if force {
		n = candidates[0] // 已按「近期成功 → 分数 → 原序」稳定排序
	} else {
		if p.rrIndex >= len(candidates) {
			p.rrIndex = 0
		}
		n = candidates[p.rrIndex]
		p.rrIndex = (p.rrIndex + 1) % len(candidates)
	}
	p.activeID = n.Fingerprint
	p.markUsedLocked(n, now)
	return n, len(candidates)
}

// switchCandidatesLocked 对被迫换节点候选做同网段排除 + 稳定排序。
// 同 /24（IPv4）/ /64（IPv6）只针对失败节点；域名不排除。无其它候选时退化为原行为。
func (p *nodePool) switchCandidatesLocked(candidates []*ProxyNode, fromFp string, now time.Time) []*ProxyNode {
	if len(candidates) == 0 {
		return candidates
	}
	if from := p.byID[fromFp]; from != nil {
		// R1：先排除当前订阅；有其它订阅候选才排除，否则退化。
		fromSub := subKeyOf(from.Name)
		crossSub := make([]*ProxyNode, 0, len(candidates))
		for _, n := range candidates {
			if subKeyOf(n.Name) != fromSub {
				crossSub = append(crossSub, n)
			}
		}
		if len(crossSub) > 0 {
			candidates = crossSub
		}
		// 再排除同 /24、/64；无其它候选时退化到当前候选集。
		filtered := make([]*ProxyNode, 0, len(candidates))
		for _, n := range candidates {
			if sameNodeSubnet(from, n) {
				continue
			}
			filtered = append(filtered, n)
		}
		if len(filtered) > 0 {
			candidates = filtered
		}
	}
	ranks := make(map[string]switchRank, len(candidates))
	for _, n := range candidates {
		ranks[n.Fingerprint] = switchRankFor(n, now)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := ranks[candidates[i].Fingerprint], ranks[candidates[j].Fingerprint]
		if a.recentSuccess != b.recentSuccess {
			return a.recentSuccess
		}
		return a.score > b.score
	})
	return candidates
}

func sameNodeSubnet(a, b *ProxyNode) bool {
	if a == nil || b == nil || a.Address == "" || b.Address == "" {
		return false
	}
	ia, ib := net.ParseIP(a.Address), net.ParseIP(b.Address)
	if ia == nil || ib == nil {
		return false // 域名不排除
	}
	a4, b4 := ia.To4(), ib.To4()
	if a4 != nil && b4 != nil {
		return a4.Mask(net.CIDRMask(24, 32)).Equal(b4.Mask(net.CIDRMask(24, 32)))
	}
	if a4 == nil && b4 == nil {
		return ia.Mask(net.CIDRMask(64, 128)).Equal(ib.Mask(net.CIDRMask(64, 128)))
	}
	return false
}

func subKeyOf(name string) string {
	if i := strings.Index(name, "::"); i >= 0 {
		return name[:i]
	}
	return name
}

func (p *nodePool) subQuarantinedLocked(name string, now time.Time) bool {
	st := p.sub403[subKeyOf(name)]
	return st != nil && st.until.After(now)
}

// quarantineSub 记一次「已确认 403」。10 分钟内累计 3 次 ⇒ 10/20/30 分钟退避。
func (p *nodePool) quarantineSub(sub string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.quarantineSubLocked(sub, now)
}

func (p *nodePool) quarantineSubLocked(sub string, now time.Time) {
	sub = subKeyOf(sub)
	if sub == "" {
		return
	}
	if p.sub403 == nil {
		p.sub403 = make(map[string]*sub403State)
	}
	st := p.sub403[sub]
	if st == nil {
		st = &sub403State{}
		p.sub403[sub] = st
	}
	if !st.last403.IsZero() && now.Sub(st.last403) > sub403Silence {
		st.escalations = 0
		st.strikes = 0
		st.windowStart = now
	}
	st.last403 = now
	if st.windowStart.IsZero() || now.Sub(st.windowStart) > sub403Window {
		st.strikes = 0
		st.windowStart = now
	}
	st.strikes++
	if st.strikes < sub403Threshold {
		return
	}
	st.escalations++
	d := sub403BaseBackoff
	for i := 1; i < st.escalations; i++ {
		d *= 2
		if d >= sub403MaxBackoff {
			d = sub403MaxBackoff
			break
		}
	}
	st.until = now.Add(d)
	st.strikes = 0
	st.windowStart = now
}

// confirmSub403 在 confirm-then-mark 的确认侧调用：把节点映射到订阅并记一次已确认 403。
func (p *nodePool) confirmSub403(fp string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := p.byID[fp]; n != nil {
		p.quarantineSubLocked(subKeyOf(n.Name), now)
	}
}

// freeTierConfirmAllowed 统一判定「换节点后成功」的确认侧是否允许落地：
// 先判定 halt / shape_latch / pool_floor；被抑制则调用方不得 mark，也不得喂订阅 strike。
// 这是 R2 的强制顺序：确认与 mark 同生共死，不能先喂熔断再决定抑制。
func (p *nodePool) freeTierConfirmAllowed(now time.Time) (suppressed string, allowed bool) {
	if p.poolHalted(now) {
		return "pool_halt", false
	}
	if ok, why := p.freeTierMarkAllowed(); !ok {
		return why, false
	}
	return "", true
}

func (p *nodePool) subQuarantined(name string, now time.Time) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.subQuarantinedLocked(name, now)
}

func (p *nodePool) allSubsQuarantined(now time.Time) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.nodes) == 0 {
		return false
	}
	for _, n := range p.nodes {
		if !p.subQuarantinedLocked(n.Name, now) {
			return false
		}
	}
	return true
}

// noteAttempt 记录池级 403 窗口的「被尝试节点」（分母）。
func (p *nodePool) noteAttempt(fp string, now time.Time) {
	if fp == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prunePool403Locked(now)
	if p.poolAttempts == nil {
		p.poolAttempts = make(map[string]time.Time)
	}
	p.poolAttempts[fp] = now
}

// note403 记录一次原始 403（不要求已确认）；达到池级阈值立即 halt。
func (p *nodePool) note403(fp string, now time.Time) {
	if fp == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prunePool403Locked(now)
	if p.pool403 == nil {
		p.pool403 = make(map[string]time.Time)
	}
	p.pool403[fp] = now
	p.maybeHaltPoolLocked(now)
}

func (p *nodePool) prunePool403Locked(now time.Time) {
	cut := now.Add(-pool403Window)
	for fp, at := range p.pool403 {
		if at.Before(cut) {
			delete(p.pool403, fp)
		}
	}
	for fp, at := range p.poolAttempts {
		if at.Before(cut) {
			delete(p.poolAttempts, fp)
		}
	}
}

func (p *nodePool) maybeHaltPoolLocked(now time.Time) {
	if p.poolHaltUntil.After(now) {
		return
	}
	attempted := len(p.poolAttempts)
	if attempted == 0 {
		attempted = len(p.pool403)
	}
	// R3：max(8, min(24, ceil(0.35 * 窗口内被尝试节点数))).
	threshold := (attempted*35 + 99) / 100
	if threshold < pool403Min {
		threshold = pool403Min
	}
	if threshold > pool403Max {
		threshold = pool403Max
	}
	if len(p.pool403) >= threshold {
		p.poolHaltUntil = now.Add(poolHaltFor)
		slog.Warn("pool 403 halt",
			"distinct_403_nodes", len(p.pool403), "attempted_nodes", attempted,
			"threshold", threshold, "until", p.poolHaltUntil.Format(time.RFC3339))
	}
}

func (p *nodePool) poolHaltedLocked(now time.Time) bool {
	return p.poolHaltUntil.After(now)
}

func (p *nodePool) poolHalted(now time.Time) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.poolHaltedLocked(now)
}

// lastSwitchCandidates 返回最近一次 pick 的候选数（仅用于 switch 事件埋点）。
func (p *nodePool) lastSwitchCandidates() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastCandidates
}

// markUsedLocked 记录节点最近一次被真实调用的时间（task-67：last_used_at 此前从未写入）。
// 调用方必须持写锁。
func (p *nodePool) markUsedLocked(n *ProxyNode, now time.Time) {
	if n == nil {
		return
	}
	n.LastUsedAt = now
	if n.State == NodeProbation {
		// probation 只放行一次证据请求：选中即视为用掉，并留下可观测标记。
		n.ProbationTried = true
		n.LastAttemptKind = "probation"
		slog.Info("node_probation_trial",
			"node", n.Name, "fp", n.Fingerprint,
			"probation_until", n.ProbationUntil.Format(time.RFC3339),
			"probation_failures", n.ProbationFailures)
		return
	}
	n.LastAttemptKind = "normal"
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
	ok, changed := p.markLocked(fp, st, reason, GateStateUnknown)
	p.mu.Unlock()
	if !ok {
		return false
	}
	if !changed {
		return true
	}
	p.saveWG.Add(1)
	go func() { defer p.saveWG.Done(); p.saveState() }()
	return true
}

// markLocked 在已持有写锁的前提下执行标记；gate 为要写入的 gate_state。
// 返回 (节点存在, 是否发生变更)。同一 (fp, reason) 在冷却期内重复 mark 是幂等的：
// 不改状态、不后推 CooldownUntil、不刷新 MarkedAt、不重复写 state changed 日志。
func (p *nodePool) markLocked(fp string, st NodeState, reason, gate string) (found, changed bool) {
	now := time.Now()
	n := p.byID[fp]
	if n == nil {
		return false, false
	}
	// I4：冷却期内的同一 (fp, reason) 重复写入直接丢弃。必须放在锁内，保持原子。
	if n.State == st && n.LastError == reason &&
		!n.CooldownUntil.IsZero() && now.Before(n.CooldownUntil) {
		return true, false
	}
	// probation 的 429/quota 失败走指数退避，而不是普通 exhausted 冷却。
	probationFailure := n.State == NodeProbation && st == NodeExhausted

	n.State = st
	n.MarkedAt = now
	n.LastError = reason
	n.GateState = gate
	n.ProbeFailStreak = 0 // 显式状态变更后重新计数，避免跨状态累积
	switch st {
	case NodeExhausted:
		if probationFailure {
			n.ProbationFailures++
			n.CooldownUntil = now.Add(probationBackoff(n.ProbationFailures))
		} else {
			n.CooldownUntil = now.Add(p.effectiveExhaustedCooldown())
		}
	case NodeDead:
		n.CooldownUntil = now.Add(p.effectiveDeadCooldown())
	default:
		n.CooldownUntil = zeroTime
	}
	// 状态已落定： probation 字段不再生效（成功路径由 recordNodeSuccess 清理）。
	n.ProbationUntil = zeroTime
	n.ProbationTried = false
	if p.activeID == fp {
		p.activeID = ""
	}
	if c := p.clients[fp]; c != nil {
		c.CloseIdleConnections()
		delete(p.clients, fp)
	}
	slog.Info("node state changed",
		"node", n.Name, "fp", fp, "state", st.String(),
		"reason", reason, "until", n.CooldownUntil.Format(time.RFC3339),
		"probation_failures", n.ProbationFailures)
	return true, true
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
	ok, _ := p.markLocked(fp, NodeExhausted, freeTierRejectedReason, GateStateRejected)
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
//
// ⚠️ 这个判据只在**没有对照组可用**时使用（见 decideProbeRound）：
// 在 213 节点的池子上 failed>=20 只有 9.4%，任何长期坏 10% 的池子都会被它永久
// 锁在「抖动」态、dead 永不标记（task-30 诊断的直接成因）。主判据是 canary 对照组。
func probeRoundLooksLikeGlobalBlip(total, failed int) bool {
	if total < 8 || failed <= 0 {
		return false
	}
	if failed >= 20 {
		return true
	}
	return failed*4 >= total*3
}

// ======================= canary 对照组（task-30 §6 方案 A） =======================
//
// 判据换成语义正确的那个：**抖动的定义是「连最稳的节点也挂了」，不是「失败数超过某个数字」**。
// 每轮从「上一轮探测成功」的节点里确定性抽 5 个当对照：
//   - 对照组失败率 >=60% ⇒ 真·全局抖动 ⇒ 整轮不采信（不判死、不累积 streak）；
//   - 否则 ⇒ 照常按 probeFailStreakThreshold 判死（长期坏的固定子集因此能被判死）。
//
// 「上一轮成功集合」天然排除了长期坏节点（它们从来不会成功），所以对照组反映的是
// **变化**而不是池子固有的坏比例 —— 这正是 42.7% 固定子集坏掉时仍然能判死的原因。
const (
	// probeCanaryCount 每轮对照组大小。成本只 +5 探针/轮（+2.3% 探测预算）。
	probeCanaryCount = 5
	// probeCanaryMinSample 采信对照组结论所需的最小样本量。少于这个数时失败率没有
	// 统计意义（1/1、2/2 都是 100%），此时**不采信对照组**，退回「无对照组」分支。
	// 触发场景：小池子（2~3 节点）、或上一轮只有个别节点成功。
	probeCanaryMinSample = 3
	// probeCanaryBlipPercent 对照组失败率（百分比）达到多少判全局抖动。
	probeCanaryBlipPercent = 60
	// probeUnverifiedBlipMaxRounds 没有对照组（冷启动 / 整池坏）时，先按老口径抑制
	// 多少轮；超过之后必须升级为「限量判死」，否则会永不判死。
	probeUnverifiedBlipMaxRounds = 3
	// probeNoCanaryMaxDeaths 无对照组升级后，单轮最多判死多少个。
	// 防「整池一次性判死」：每轮推进一批，既不会卡住也不会一次性抽干池子。
	probeNoCanaryMaxDeaths = 10
)

// probeRoundVerdict 是一轮可用节点巡检的采信结论。
type probeRoundVerdict int

const (
	// probeRoundTrusted 采信本轮失败：按 streak 判死。
	probeRoundTrusted probeRoundVerdict = iota
	// probeRoundBlipVerified 对照组失败率 >=60%：真·全局抖动，整轮不采信。
	probeRoundBlipVerified
	// probeRoundBlipUnverified 无对照组且老口径判定像全局抖动：先抑制。
	probeRoundBlipUnverified
	// probeRoundEscalate 无对照组且已连续多轮：必须升级，限量判死。
	probeRoundEscalate
)

func (v probeRoundVerdict) String() string {
	switch v {
	case probeRoundTrusted:
		return "trusted"
	case probeRoundBlipVerified:
		return "blip_verified"
	case probeRoundBlipUnverified:
		return "blip_unverified"
	case probeRoundEscalate:
		return "escalate"
	}
	return "unknown"
}

// decideProbeRound 是本轮采信决策的**纯函数**（边界可测，无副作用）。
//
// 优先级：
//  1. 对照组样本足够（>=probeCanaryMinSample）⇒ 只看对照组失败率（>=60% 抖动）。
//     43% 子集坏、对照组正常 ⇒ 采信并判死。
//  2. 对照组样本不足 / 没有对照组 ⇒ 退回老口径（probeRoundLooksLikeGlobalBlip）：
//     - 尚未连续抑制 probeUnverifiedBlipMaxRounds 轮 ⇒ 抑制（防冷启动/真抖动误杀）；
//     - 已经连续抑制够了 ⇒ 升级为限量判死（防「永不判死」）。
//     老口径也没命中（例如小池子全挂）⇒ 直接采信（那种池子本来就该判死）。
//
// 为什么要有最小样本量：2 节点池里「上一轮成功的那 2 个这轮都挂了」既是 100% 对照组
// 失败、也完全可能只是这 2 个节点真的坏了。1~2 个样本的失败率无法区分「全局抖动」
// 与「局部故障」，硬判抖动会让小池子永不判死，所以此时退回老口径（老口径对小池子
// 要求失败数 >=20 或 >=75%，本身就很保守）。
func decideProbeRound(canaryProbed, canaryFailed, probed, failed int, prevBlipStreak int64) probeRoundVerdict {
	if canaryProbed >= probeCanaryMinSample {
		if canaryFailed*100 >= canaryProbed*probeCanaryBlipPercent {
			return probeRoundBlipVerified
		}
		return probeRoundTrusted
	}
	if probeRoundLooksLikeGlobalBlip(probed, failed) {
		if prevBlipStreak >= probeUnverifiedBlipMaxRounds {
			return probeRoundEscalate
		}
		return probeRoundBlipUnverified
	}
	return probeRoundTrusted
}

// canaryPick 从「上一轮探测成功」的指纹集合里确定性抽 k 个作对照组。
//
// 抽样方式：按 FNV-1a 指纹哈希排序后取前 k 个。这样：
//   - 与输入顺序、池内顺序都无关（订阅刷新换了顺序也不会换对照组）；
//   - 同一个成功集合永远抽出同一批节点 ⇒ 测试可复现，抽到谁可被审计；
//   - 哈希分散，不会永远只抽到「名字排前面」的那几个来源。
//
// 刻意不用随机数：随机抽样会让「对照组恰好全落在故障子集里」的概率不可观测，
// 也无法在测试里稳定复现。
func canaryPick(successFP []string, k int) []string {
	if k <= 0 || len(successFP) == 0 {
		return nil
	}
	ranked := make([]string, len(successFP))
	copy(ranked, successFP)
	sort.Slice(ranked, func(i, j int) bool {
		hi, hj := fnv64a(ranked[i]), fnv64a(ranked[j])
		if hi != hj {
			return hi < hj
		}
		return ranked[i] < ranked[j]
	})
	if len(ranked) > k {
		ranked = ranked[:k]
	}
	return ranked
}

func fnv64a(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// lastProbeOKSnapshot 返回上一轮成功指纹集合的副本（对照组候选）。
func (p *nodePool) lastProbeOKSnapshot() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, len(p.lastProbeOK))
	copy(out, p.lastProbeOK)
	return out
}

// setLastProbeOK 用本轮成功集合替换对照组候选池。
func (p *nodePool) setLastProbeOK(fps []string) {
	cp := make([]string, len(fps))
	copy(cp, fps)
	p.mu.Lock()
	p.lastProbeOK = cp
	p.mu.Unlock()
}

// probeCanaries 在本轮结果之上补齐对照组的观测：
// 对照组节点通常就在本轮被探测的集合里（上一轮成功的节点本轮仍是 available），
// 此时**零额外探针**；只有不在本轮集合里的（例如刚被标 exhausted）才补探一次，
// 最多 probeCanaryCount 次。补探只用于计数，不改节点状态（不是一次真实的巡检判定）。
func (p *nodePool) probeCanaries(ctx context.Context, canary []string, probeURL string, probedSet, failedSet map[string]bool) (probed, failed int) {
	for _, fp := range canary {
		if probedSet[fp] {
			probed++
			if failedSet[fp] {
				failed++
			}
			continue
		}
		p.mu.RLock()
		n := p.byID[fp]
		p.mu.RUnlock()
		if n == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			break
		}
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		_, err := p.probeNode(pctx, n, probeURL)
		cancel()
		probed++
		if err != nil {
			failed++
		}
	}
	return probed, failed
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
		wg        sync.WaitGroup
		probed    int
		mu        sync.Mutex
		failedFP  []string
		okFP      []string
		probedFP  = map[string]bool{}
		failedSet = map[string]bool{}
		failWhy   = map[string]string{}
		sem       = make(chan struct{}, probeMaxConcurrent)
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
				failedSet[n.Fingerprint] = true
				probedFP[n.Fingerprint] = true
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
				mu.Lock()
				okFP = append(okFP, n.Fingerprint)
				probedFP[n.Fingerprint] = true
				mu.Unlock()
			}
		}(n)
	}
	wg.Wait()

	// 全局抖动闸【只对可用节点巡检轮次生效】。
	// dead 复探轮天然大面积失败（它们本来就是 dead），若参与判定会让 WARN 每分钟一条、
	// ERROR 每 10 分钟一条且永久常亮 —— 恰好反转了这条告警本该提供的信号（R2 复审 D1）。
	guarded := filter == probeAvailable
	if !guarded {
		for _, fp := range failedFP {
			p.markProbeDead(fp, "probe: "+failWhy[fp])
		}
		return probed
	}

	// ---- 对照组判定（task-30 §6 方案 A）----
	// 从「上一轮探测成功」的节点里确定性抽 5 个：真抖动时它们也会挂（≥60% 判抖动），
	// 而固定的坏子集从来不会进入成功集合，所以它们挂不掉 ⇒ 本轮采信、坏节点被判死。
	canary := canaryPick(p.lastProbeOKSnapshot(), probeCanaryCount)
	canaryProbed, canaryFailed := p.probeCanaries(ctx, canary, probeURL, probedFP, failedSet)
	canaryRate := 0.0
	if canaryProbed > 0 {
		canaryRate = float64(canaryFailed) / float64(canaryProbed)
	}
	verdict := decideProbeRound(canaryProbed, canaryFailed, probed, len(failedFP), blipStreak.Load())

	// 每轮一行观测：排查「为什么这轮没判死 / 判死了」只需要这一行。
	// 健康轮降 Debug，避免每 15 分钟一条 Info 噪声。
	logProbeRound := slog.Debug
	if len(failedFP) > 0 || verdict != probeRoundTrusted {
		logProbeRound = slog.Info
	}
	logProbeRound("probe_round_canary",
		"filter", filter.String(),
		"probed", probed, "failed", len(failedFP),
		"canary_probed", canaryProbed, "canary_failed", canaryFailed,
		"canary_rate", canaryRate, "canary_used", canaryProbed >= probeCanaryMinSample,
		"verdict", verdict.String(), "consecutive", blipStreak.Load(),
	)

	switch verdict {
	case probeRoundBlipVerified, probeRoundBlipUnverified:
		n := blipStreak.Add(1)
		slog.Warn("probe_round_global_blip",
			"filter", filter.String(),
			"probed", probed, "failed", len(failedFP), "consecutive", n,
			"canary_probed", canaryProbed, "canary_failed", canaryFailed, "canary_rate", canaryRate,
			"canary_used", canaryProbed >= probeCanaryMinSample,
			"verified", verdict == probeRoundBlipVerified,
			"action", "整轮失败不予采信（疑似本机出口/网络问题）")
		if n == 3 || n%10 == 0 {
			slog.Error("probe_round_global_blip_persistent", "consecutive", n,
				"hint", "连续多轮抖动：节点不会被判死，但本机网络/出口可能长期异常，需人工检查")
		}
		// 抖轮里的成功集合仍然更新：下一轮对照组据此自愈（成功集合天然排除长期坏节点）。
		p.setLastProbeOK(okFP)
		return probed
	case probeRoundEscalate:
		// 无对照组且已经连续抑制 probeUnverifiedBlipMaxRounds 轮：不能再静默，
		// 但也不能把整池一次性判死 ⇒ 按指纹排序后限量处理。
		n := blipStreak.Add(1)
		sort.Strings(failedFP)
		skipped := 0
		if len(failedFP) > probeNoCanaryMaxDeaths {
			skipped = len(failedFP) - probeNoCanaryMaxDeaths
			failedFP = failedFP[:probeNoCanaryMaxDeaths]
		}
		slog.Warn("probe_round_global_blip_escalated",
			"filter", filter.String(),
			"probed", probed, "failed", len(failedFP)+skipped, "consecutive", n,
			"canary_probed", 0, "canary_failed", 0, "canary_rate", 0.0,
			"judged", len(failedFP), "skipped", skipped, "cap", probeNoCanaryMaxDeaths,
			"action", "无对照组可用（上一轮没有任何成功节点）：本轮起限量判死，防整池一次性判死也防永不判死")
	default:
		blipStreak.Store(0)
	}
	p.setLastProbeOK(okFP)
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
		nodeScoring.observeProbeResult(fp, n.Name, false, -1, reason, "exhausted", now)
		slog.Debug("node health probe failed while exhausted (marker kept)",
			"node", n.Name, "fp", fp, "error", reason,
			"until", n.CooldownUntil.Format(time.RFC3339))
		return
	}
	if n.State == NodeDead {
		nodeScoring.observeProbeResult(fp, n.Name, false, -1, reason, "dead", now)
		slog.Debug("node still dead (re-probe failed)",
			"node", n.Name, "fp", fp, "error", reason)
		return
	}
	// 连续失败守卫：一次失败只累计 streak，不判死 —— 一次网络抖动不该丢掉一个出口 IP
	// （2026-10-19 生产：一轮抖动让 86 个节点同时被判死，其中至少一个 TCP 实际可达）。
	n.ProbeFailStreak++
	if n.ProbeFailStreak < probeFailStreakThreshold {
		nodeScoring.observeProbeResult(fp, n.Name, false, -1, reason, "available", now)
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
	// task-67：判死事件进环形缓冲 + dead_marks_7d 计数
	nodeScoring.observeProbeResult(fp, n.Name, false, -1, reason, "dead", now)
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
	// task-67：探测历史环形缓冲（20 轮，落盘 scoring_state.json）+ 24h/7d 计数
	nodeScoring.observeProbeResult(fp, n.Name, true, ms, "", n.State.String(), time.Now())
	switch n.State {
	case NodeDead:
		n.State = NodeAvailable
		n.MarkedAt = time.Time{}
		n.CooldownUntil = time.Time{}
		n.LastError = ""
	case NodeProbation:
		// 探测成功也是 evidence：probation 可以转 available（设计 §4.1）。
		p.promoteProbationLocked(n)
	}
	p.saveWG.Add(1)
	go func() { defer p.saveWG.Done(); p.saveState() }()
}

// recordNodeSuccess 记录一次真实请求成功。probation 节点拿到证据即转 available；
// 其他状态的成功不改生命周期（dead 只由探测恢复，exhausted 只由时间+证据恢复）。
func (p *nodePool) recordNodeSuccess(fp string) bool {
	p.mu.Lock()
	n := p.byID[fp]
	if n == nil {
		p.mu.Unlock()
		return false
	}
	changed := false
	if n.State == NodeProbation {
		p.promoteProbationLocked(n)
		changed = true
	}
	p.mu.Unlock()
	if changed {
		p.saveWG.Add(1)
		go func() { defer p.saveWG.Done(); p.saveState() }()
	}
	return true
}

// promoteProbationLocked 把拿到证据的 probation 节点提升为 available，并清空试探字段。
// 调用方必须持有写锁。
func (p *nodePool) promoteProbationLocked(n *ProxyNode) {
	slog.Info("node probation succeeded",
		"node", n.Name, "fp", n.Fingerprint,
		"probation_failures", n.ProbationFailures)
	n.State = NodeAvailable
	n.MarkedAt = time.Time{}
	n.CooldownUntil = time.Time{}
	n.LastError = ""
	n.LastAttemptKind = "normal"
	n.ProbationUntil = time.Time{}
	n.ProbationTried = false
	n.ProbationFailures = 0
}

// ======================= per (model, node) 配额 =======================

func modelQuotaKey(fp, model string) string {
	return fp + "\x00" + strings.ToLower(strings.TrimSpace(model))
}

// markModelQuota 记录某 (节点, 模型) 的配额冷却；只影响该 model，不写节点整体状态。
func (p *nodePool) markModelQuota(fp, model, reason string, until time.Time) {
	if fp == "" || model == "" {
		return
	}
	now := time.Now()
	if until.IsZero() || !until.After(now) {
		until = now.Add(p.effectiveExhaustedCooldown())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.modelQuota == nil {
		p.modelQuota = make(map[string]modelQuotaEntry)
	}
	p.modelQuota[modelQuotaKey(fp, model)] = modelQuotaEntry{Until: until, Reason: reason, MarkedAt: now}
}

// modelQuotaCooling 报告该 (节点, 模型) 是否仍在配额冷却期内。
func (p *nodePool) modelQuotaCooling(fp, model string, now time.Time) bool {
	if fp == "" || model == "" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.modelQuota[modelQuotaKey(fp, model)]
	if !ok {
		return false
	}
	if !now.Before(e.Until) {
		// 到期记录懒清理（仍需写锁，这里只读判断即可；残留条目无副作用）。
		return false
	}
	return true
}

// currentFp 返回当前生效指纹（无锁读用，仅日志/统计）。
func (p *nodePool) currentFp() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.activeID
}

// manualFp 返回面板手动指定的优先节点指纹（只读，不切换）。
func (p *nodePool) manualFp() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.manualPick
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
		Transport: newNodeTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialNode(ctx, n, network, addr)
		}),
	}
	p.clients[fp] = client
	return client
}

// newNodeTransport 构造节点出口用的 http.Transport。
// 抽成函数是为了让测试能注入 dial 并验证各阶段的上界真的生效。
//
// 每个阶段都必须有上界，否则「节点接受 TCP 但后续被黑洞」就会挂到天荒地老：
//   - 建连：套 ctx 超时（dialNode 走内嵌 mihomo，没有 net.Dialer 可套，只能靠 ctx）；
//   - TLS 握手：TLSHandshakeTimeout（★ 2026-10-19 生产缺失 ⇒ 单次尝试挂 940~1034 秒）；
//   - 响应头：ResponseHeaderTimeout。
//
// body 读取阶段仍不设限（长生成/流式不能被误杀）；「卡住不返回」的正解是空闲读超时。
func newNodeTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dctx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			return dial(dctx, network, addr)
		},
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: firstByteTimeout,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
	}
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
			n.ProbationUntil = prev.ProbationUntil
			n.ProbationTried = prev.ProbationTried
			n.ProbationFailures = prev.ProbationFailures
			n.LastAttemptKind = prev.LastAttemptKind
		}
		next = append(next, n)
		byID[n.Fingerprint] = n
	}
	sort.SliceStable(next, func(i, j int) bool { return next[i].Name < next[j].Name })
	p.nodes = next
	p.byID = byID
	p.activeID = ""
	p.rrIndex = 0
	restored := 0
	if !p.stateRestored {
		// ★ C-7 修复点：状态恢复必须在这里、在节点列表装入之后、在 saveState 之前。
		//   - 在这里：原来的 loadState() 在 newProxyPool() 里调，那时 p.nodes 还是空的，
		//     循环体一次都不执行 ⇒ proxy_state.json 写了从来没读进来 ⇒ 每次重启把
		//     dead/exhausted 全部复活（也解释了 14:03 那轮 probed=216）。
		//   - 在 saveState 之前：否则第一次 setNodes 会先把「全 available」刷回磁盘，
		//     把旧状态抹掉，再读也读不到了。
		//   - 只在首次：进程内的状态比磁盘新，订阅刷新再读盘会把刚判死的节点写回 available。
		p.stateRestored = true
		restored = p.restoreStateLocked()
	}
	p.mu.Unlock()
	if restored > 0 {
		slog.Info("proxy state restored from disk",
			"path", p.statePath, "nodes", restored)
	}
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

// previewExclusionVerdict 只读地统计可路由节点，供 429 漂移闸门在【发送前】判定
// 「全排除」并计算 would_be_egress。它不修改 sticky / active / rrIndex / lastCandidates，
// 因此不会污染 pick(false) 的粘滞语义；只做与选路一致的冷却清扫。
//
// 返回：first = 本应选中的出口（当前 active 未被排除时优先，复刻粘滞语义）；
// eligible = 区域匹配的可路由节点数；unblocked = 其中未被 exclude 的节点数。
// eligible > 0 && unblocked == 0 即「池里有可用节点，但全被 429 排除」。
func (p *nodePool) previewExclusionVerdict(requiredRegion string, exclude map[string]bool) (first string, eligible, unblocked int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.sweepExpiredLocked(now)
	activeUnblocked := ""
	firstUnblocked := ""
	for _, n := range p.nodes {
		if !p.eligible(n, now) {
			continue
		}
		if requiredRegion != "" && n.Region != requiredRegion {
			continue
		}
		eligible++
		if len(exclude) > 0 && exclude[nodeEgressKey(n.Fingerprint)] {
			continue
		}
		unblocked++
		if firstUnblocked == "" {
			firstUnblocked = nodeEgressKey(n.Fingerprint)
		}
		if n.Fingerprint == p.activeID {
			activeUnblocked = nodeEgressKey(n.Fingerprint)
		}
	}
	switch {
	case activeUnblocked != "":
		return activeUnblocked, eligible, unblocked
	case firstUnblocked != "":
		return firstUnblocked, eligible, unblocked
	default:
		return "none_available", eligible, unblocked
	}
}

// previewExclusionCounts 是 previewExclusionVerdict 的计数版（main 在 egress.Get
// 回退直连后复核用）。
func (p *nodePool) previewExclusionCounts(requiredRegion string, exclude map[string]bool) (eligible, unblocked int) {
	_, eligible, unblocked = p.previewExclusionVerdict(requiredRegion, exclude)
	return eligible, unblocked
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

// proxyStateVersion 是 proxy_state.json 的格式版本。
// 写入时带上；读取时版本不在 [0, proxyStateVersion] 内就整份忽略（安全降级）。
// 0 表示旧文件（没有 version 字段）—— 兼容接受，字段语义与 v1 相同。
const proxyStateVersion = 1

func (p *nodePool) saveState() {
	p.saveWG.Add(1)
	defer p.saveWG.Done()
	p.mu.RLock()
	st := persistentState{Version: proxyStateVersion, Nodes: map[string]nodeRuntimeState{}}
	for _, n := range p.nodes {
		st.Nodes[n.Fingerprint] = nodeRuntimeState{
			State:             n.State.String(),
			MarkedAt:          n.MarkedAt,
			CooldownUntil:     n.CooldownUntil,
			LastError:         n.LastError,
			LastUsedAt:        n.LastUsedAt,
			LatencyMs:         n.LatencyMs,
			LastProbeAt:       n.LastProbeAt,
			GateState:         n.GateState,
			ProbationUntil:    n.ProbationUntil,
			ProbationTried:    n.ProbationTried,
			ProbationFailures: n.ProbationFailures,
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

// readPersistentState 读取并校验状态文件。任何异常（不存在/坏 JSON/版本不符）
// 都返回 ok=false —— 调用方据此安全降级（不 panic、不把节点误标死）。
func readPersistentState(path string) (persistentState, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("proxy state read failed (ignored)", "path", path, "error", err)
		}
		return persistentState{}, false
	}
	var st persistentState
	if err := json.Unmarshal(data, &st); err != nil {
		slog.Warn("proxy state file corrupt (ignored)", "path", path, "error", err)
		return persistentState{}, false
	}
	if st.Version < 0 || st.Version > proxyStateVersion {
		slog.Warn("proxy state version unsupported (ignored)",
			"path", path, "version", st.Version, "supported", proxyStateVersion)
		return persistentState{}, false
	}
	return st, true
}

// restoreStateLocked 把磁盘上的运行时状态合并进当前节点列表（调用方必须持写锁）。
//
// 逐条校验、逐条跳过：一条坏记录只影响它自己，绝不让整池降级成 available 或 dead。
//   - 指纹对不上（换了池子/订阅）⇒ 跳过该条；
//   - 未知状态字符串 ⇒ 按 available 处理（宁可漏标记，不能误标死）；
//   - exhausted 但 cooldown_until 为空 ⇒ 跳过该条：这种节点永远不会被 sweepExpiredLocked
//     翻回 available，等于永久封禁，属于坏数据。
func (p *nodePool) restoreStateLocked() int {
	st, ok := readPersistentState(p.statePath)
	if !ok || len(st.Nodes) == 0 {
		return 0
	}
	applied := 0
	for _, n := range p.nodes {
		rs, ok := st.Nodes[n.Fingerprint]
		if !ok {
			continue
		}
		state := nodeStateFromString(rs.State)
		trimmed := strings.ToLower(strings.TrimSpace(rs.State))
		if trimmed != "" && trimmed != "available" && trimmed != "dead" && trimmed != "exhausted" && trimmed != "probation" {
			slog.Warn("proxy state entry has unknown state (treated as available)",
				"node", n.Name, "fp", n.Fingerprint, "state", rs.State)
		}
		if state == NodeExhausted && rs.CooldownUntil.IsZero() {
			slog.Warn("proxy state entry ignored: exhausted without cooldown_until",
				"node", n.Name, "fp", n.Fingerprint)
			continue
		}
		// probation 没有截止时间就无法收敛，等同于坏数据：跳过（保持 available）。
		if state == NodeProbation && rs.ProbationUntil.IsZero() {
			slog.Warn("proxy state entry ignored: probation without probation_until",
				"node", n.Name, "fp", n.Fingerprint)
			continue
		}
		n.State = state
		n.MarkedAt = rs.MarkedAt
		n.CooldownUntil = rs.CooldownUntil
		n.LastError = rs.LastError
		n.LastUsedAt = rs.LastUsedAt
		n.LatencyMs = rs.LatencyMs
		n.LastProbeAt = rs.LastProbeAt
		n.GateState = rs.GateState
		n.ProbationUntil = rs.ProbationUntil
		n.ProbationTried = rs.ProbationTried
		n.ProbationFailures = rs.ProbationFailures
		applied++
	}
	return applied
}

type persistentState struct {
	Version int                         `json:"version"`
	Nodes   map[string]nodeRuntimeState `json:"nodes"`
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
	n, _ := p.pickForRegionMeta(force, requiredRegion)
	return n
}

func (p *nodePool) pickForRegionExcluding(force bool, requiredRegion string, exclude map[string]bool) *ProxyNode {
	if requiredRegion == "" {
		n, _ := p.pickWithMetaExcluding(force, exclude)
		return n
	}
	n, _ := p.pickForRegionMetaExcluding(force, requiredRegion, exclude)
	return n
}

func (p *nodePool) pickForRegionMeta(force bool, requiredRegion string) (*ProxyNode, int) {
	return p.pickForRegionMetaExcluding(force, requiredRegion, nil)
}

func (p *nodePool) pickForRegionMetaExcluding(force bool, requiredRegion string, exclude map[string]bool) (*ProxyNode, int) {
	if requiredRegion == "" {
		return p.pickWithMetaExcluding(force, exclude)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.lastCandidates = 0
	if force && p.poolHaltedLocked(now) {
		p.lastCandidates = 0
		return nil, 0
	}
	p.sweepExpiredLocked(now)

	ordered, anyPool := p.orderedLocked()
	if !anyPool || len(ordered) == 0 {
		return nil, 0
	}

	// 过滤：仅选匹配区域，且排除指定出口。
	from := p.activeID
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
		if len(exclude) > 0 && exclude[nodeEgressKey(n.Fingerprint)] {
			continue
		}
		candidates = append(candidates, n)
	}
	if len(candidates) == 0 {
		return nil, 0 // 无匹配区域的可用节点
	}
	if force {
		candidates = p.switchCandidatesLocked(candidates, from, now)
	}
	p.lastCandidates = len(candidates)
	var n *ProxyNode
	if force {
		n = candidates[0]
	} else {
		if p.rrIndex >= len(candidates) {
			p.rrIndex = 0
		}
		n = candidates[p.rrIndex]
		p.rrIndex = (p.rrIndex + 1) % len(candidates)
	}
	p.activeID = n.Fingerprint
	p.markUsedLocked(n, now)
	return n, len(candidates)
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
