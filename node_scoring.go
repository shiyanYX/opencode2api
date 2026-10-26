package main

// node_scoring.go —— task-67：节点打分（权威规格 docs/research/node-scoring-spec.md）
//
// 结构：
//  1. 参数与纯函数（§0 公式 / §3.1 Wilson / §3.2 衰减 / §3.3 探测封顶 / §3.7 档位）
//  2. scoreNode：单节点打分 + reasons（§3.4 失败分类权重、§3.6 冷启动、§4.4 一致性）
//  3. nodeScoringStore：真实调用归因、探测历史环形缓冲、落盘（scoring_state.json）
//  4. 启动：加载/回填历史调用日志 + 后台轮询新记录 + 周期落盘
//
// 数据流：真实调用（call_log）→ 归因成每节点衰减累加量 → 质量分；
//         健康探测 → 探测历史环形缓冲（20 轮，落盘）→ 探测分/封顶/趋势；
//         节点池状态（available/dead/exhausted）→ usable + 硬封顶。
// 三者时效不同（§2），绝不混算：探测只封顶不抬分（§3.3）。

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ======================= 1. 参数与纯函数 =======================

const (
	// scoringVersion 是打分模型版本（对外暴露，口径变了必须递增）。
	scoringVersion = 1
	// scoringStateVersion 是 scoring_state.json 的格式版本。
	scoringStateVersion = 1
	// scoringTrendCapacity 是每节点探测历史环形缓冲的轮数（spec §5.4：20 轮）。
	scoringTrendCapacity = 20
	// scoringHalfLifeDays 是真实调用证据的指数衰减半衰期（§3.2）。
	scoringHalfLifeDays = 7.0
	// scoringBlipFactor 是池级抖动窗口内失败的计责比例（§4.2：按 20% 计责）。
	scoringBlipFactor = 0.2
	// scoringBlipWindowSeconds 是池级抖动窗口（10 分钟）。
	scoringBlipWindowSeconds = 600
	// scoringStateFileName 落盘文件名（与 config.json/proxy_state.json 同目录）。
	scoringStateFileName = "scoring_state.json"
)

// scoringParams 是 §0 公式的全部可调参数；默认值与 spec / 回算脚本逐项一致。
type scoringParams struct {
	PriorA        float64 // 池内先验 α0（伪成功数，§3.6）
	PriorB        float64 // 池内先验 β0（伪失败数，§3.6）
	Z             float64 // Wilson 单侧 95% 下界 z（§3.1）
	WQ            float64 // 质量权重
	WP            float64 // 探测权重
	WL            float64 // 延迟权重
	LatFull       float64 // 延迟满分阈值（ms）
	LatFloor      float64 // 延迟触底阈值（ms）
	DeadFactor    float64 // dead 封顶系数（§3.3）
	CapProbe24    float64 // 近 24h 有探测失败时的封顶分
	StaleProbeMin float64 // 探测新鲜度阈值（分钟）
}

var defaultScoringParams = scoringParams{
	PriorA: 8.0, PriorB: 2.0, Z: 1.645,
	WQ: 0.60, WP: 0.20, WL: 0.20,
	LatFull: 1500, LatFloor: 10000,
	DeadFactor: 0.30, CapProbe24: 65, StaleProbeMin: 45,
}

// scoringParamsView 是对外暴露的参数快照（/api/nodes 顶层 scoring.params）。
func scoringParamsView(p scoringParams) map[string]any {
	return map[string]any{
		"prior_a": p.PriorA, "prior_b": p.PriorB, "z": p.Z,
		"w_q": p.WQ, "w_p": p.WP, "w_l": p.WL,
		"lat_full": p.LatFull, "lat_floor": p.LatFloor,
		"dead_factor": p.DeadFactor, "cap_probe24": p.CapProbe24,
		"stale_probe_min": p.StaleProbeMin,
		"half_life_days":  scoringHalfLifeDays,
		"blip_factor":     scoringBlipFactor,
	}
}

// scoringWilsonLB 是 Wilson 单侧 95% 下界（§3.1）。
func scoringWilsonLB(success, n, z float64) float64 {
	if n <= 0 {
		return 0
	}
	p := success / n
	denom := 1 + z*z/n
	center := p + z*z/(2*n)
	margin := z * math.Sqrt(math.Max(0, p*(1-p)/n+z*z/(4*n*n)))
	return (center - margin) / denom
}

// scoringLatencyComponent 是延迟分项（§0）：<=lat_full 满分，>=lat_floor 触底 0.1，
// 中间线性；未知延迟（-1/0，死节点）按 0.45 处理 —— 绝不能当 0ms 拿满分（§2.4）。
func scoringLatencyComponent(ms int64, p scoringParams) float64 {
	if ms <= 0 {
		return 0.45
	}
	if float64(ms) <= p.LatFull {
		return 1.0
	}
	if float64(ms) >= p.LatFloor {
		return 0.10
	}
	v := 1.0 - (float64(ms)-p.LatFull)/(p.LatFloor-p.LatFull)*0.9
	return math.Round(v*10000) / 10000
}

// scoringBand 把 0-100 分映射成四档（§3.7）：优≥85 / 良70-84 / 观察50-69 / 停用<50。
// tier 是前端契约的英文枚举，tierLabel 是现成文案，tierCN 是中文档名。
func scoringBand(score float64) (tier, tierLabel, tierCN string) {
	switch {
	case score >= 85:
		return "excellent", "可用 · 优", "优"
	case score >= 70:
		return "good", "可用 · 良", "良"
	case score >= 50:
		return "watch", "观察 · 谨慎", "观察"
	default:
		return "bad", "该切 · 停用", "停用"
	}
}

// scoringTierFromCN 供对账测试/内部映射使用。
func scoringTierFromCN(cn string) string {
	switch cn {
	case "优":
		return "excellent"
	case "良":
		return "good"
	case "观察":
		return "watch"
	default:
		return "bad"
	}
}

// ======================= 2. 单节点打分 =======================

// scoreTrendPoint 是探测历史环形缓冲的一条（spec §5.1 trend[]）。
type scoreTrendPoint struct {
	TS      string  `json:"ts"`
	Score   float64 `json:"score"`
	ProbeOK bool    `json:"probe_ok"`
}

// scoringFailCounts 是对外暴露的失败分类计数（§5.1 fail{}）。
type scoringFailCounts struct {
	Connect      int `json:"connect"`
	Cred         int `json:"cred"`
	HTTP5xx      int `json:"http5xx"`
	Timeout      int `json:"timeout"`
	Stream       int `json:"stream"`
	ConfirmedBad int `json:"confirmed_bad,omitempty"`
}

// scoreReason 是一条人话原因（§5.1 score_reasons[]）。
type scoreReason struct {
	Code     string         `json:"code"`
	Level    string         `json:"level"` // high|mid|low|info|good
	Text     string         `json:"text"`
	Evidence map[string]any `json:"evidence,omitempty"`
}

// nodeScoringInput 是单节点打分的全部输入（§2 三类输入 + 归因结果）。
type nodeScoringInput struct {
	FP   string
	Name string
	// 节点池运行时状态
	State         string // available|dead|exhausted
	LatencyMs     int64  // -1/0 = 未知
	LastProbeAt   time.Time
	LastError     string
	CooldownUntil time.Time
	// 真实调用归因（7 天半衰期衰减后）
	Attempts      int
	Success       int
	Fail          map[string]int
	QuotaEvents   int
	BlipFails     int
	EffS          float64
	EffN          float64
	LastSuccessAt time.Time
	LastTrafficAt time.Time
	// 探测历史（24h/7d 计数由环形缓冲推导）
	ProbeFail24h  int
	DeadMarks7d   int
	ProbeOKStreak int
	Trend         []scoreTrendPoint
}

// nodeScoreView 是 /api/nodes 每节点的打分字段（字段名是前端契约，不许改）。
type nodeScoreView struct {
	Score           float64           `json:"score"` // 0-100，1 位小数
	Tier            string            `json:"tier"`  // excellent|good|watch|bad
	Usable          bool              `json:"usable"`
	Quality         float64           `json:"quality"`
	EffectiveSample float64           `json:"effective_sample"`
	SampleSize      int               `json:"sample_size"`
	LowConfidence   bool              `json:"low_confidence"`
	ScoreReasons    []scoreReason     `json:"score_reasons"`
	ProbeLatencyMS  *int64            `json:"probe_latency_ms"` // -1/0 ⇒ null（裁定 5）
	LastSuccessAt   *string           `json:"last_success_at"`  // 零值 ⇒ null（裁定 5）
	LastTrafficAt   *string           `json:"last_traffic_at"`  // 零值 ⇒ null（裁定 5）
	LastError       *string           `json:"last_error"`       // 空 ⇒ null
	Fail            scoringFailCounts `json:"fail"`
	QuotaEvents     int               `json:"quota_events"`
	Caps            []string          `json:"caps"`
	ProbeFail24h    int               `json:"probe_fail_24h"`
	DeadMarks7d     int               `json:"dead_marks_7d"`
	Trend           []scoreTrendPoint `json:"trend"`

	// 内部排查字段：不进接口（裁定 4：不加 p50_ms/p95_ms/auto_eligible/stale 等冗余字段）
	TierLabel        string   `json:"-"`
	TierCN           string   `json:"-"`
	AutoEligible     bool     `json:"-"`
	Probe            float64  `json:"-"`
	LatencyComponent float64  `json:"-"`
	RawScore         float64  `json:"-"`
	ProbeFlags       []string `json:"-"`
}

// scoringLevelOrder 是 reasons 的排序权重（§5.3：high → mid → low → info → good）。
var scoringLevelOrder = map[string]int{"high": 0, "mid": 1, "low": 2, "info": 3, "good": 4}

// scoreNode 对单个节点打分。ref 用于「证据陈旧」判断（历史回算用日志末条时间），
// logNow 用于探测新鲜度判断（回算用日志窗口时间）。实时运行时两者都是 now。
func scoreNode(in nodeScoringInput, ref, logNow time.Time, p scoringParams) nodeScoreView {
	q := scoringWilsonLB(in.EffS+p.PriorA, in.EffN+p.PriorA+p.PriorB, p.Z)

	probe := 1.0
	var probeFlags []string
	if in.State == "dead" {
		probe = 0
		probeFlags = append(probeFlags, "state_dead")
	} else {
		hasProbe := !in.LastProbeAt.IsZero() && in.LastProbeAt.Year() > 1
		if !hasProbe {
			probe = 0.5
			probeFlags = append(probeFlags, "probe_unknown")
		} else {
			ageH := logNow.Sub(in.LastProbeAt).Hours()
			switch {
			case ageH > 3*p.StaleProbeMin/60.0:
				probe = 0.3
				probeFlags = append(probeFlags, fmt.Sprintf("probe_stale%.0fh", ageH))
			case ageH > p.StaleProbeMin/60.0:
				probe = 0.6
				probeFlags = append(probeFlags, fmt.Sprintf("probe_stale%.0fm", ageH*60))
			}
		}
		if in.ProbeFail24h > 0 {
			if probe > 0.4 {
				probe = 0.4
			}
			probeFlags = append(probeFlags, fmt.Sprintf("probe_fail_24h=%d", in.ProbeFail24h))
		}
	}
	latS := scoringLatencyComponent(in.LatencyMs, p)
	raw := 100 * (p.WQ*q + p.WP*probe + p.WL*latS)

	score := raw
	var caps []string
	if in.State == "dead" {
		score = raw * p.DeadFactor // 封顶 30 且保留序（不是一律 30 分，避免排序并列）
		caps = append(caps, "dead×0.30")
	}
	if in.ProbeFail24h > 0 && in.State != "dead" && score > p.CapProbe24 {
		score = p.CapProbe24
		caps = append(caps, "probe_24h≤65")
	}
	score = math.Round(math.Max(0, math.Min(100, score))*10) / 10

	// 冷启动/无数据：没有真实样本也没有最新探测 ⇒ 按「观察」处理，不诬告成停用（§3.6）
	noData := in.EffN == 0 && in.State != "dead" && containsString(probeFlags, "probe_unknown")
	if noData {
		score = math.Max(score, 50)
		caps = append(caps, "no_data→观察") // 与回算脚本一致：只要 no_data 就记 cap（解释为什么压到观察档）
	}

	tier, tierLabel, tierCN := scoringBand(score)
	usable := in.State != "dead" && in.State != "exhausted"
	autoEligible := in.State != "dead" && score >= 70 && in.EffN >= 20

	// ---------- reasons（顺序与 spec §4.2/§5.3 一致，最后按 level 稳定排序） ----------
	var reasons []scoreReason
	fails := in.Fail
	att := in.Attempts
	rate := func(c string) float64 {
		if att <= 0 {
			return 0
		}
		return float64(fails[c]) / float64(att)
	}
	if in.State == "dead" {
		errText := strings.TrimPrefix(in.LastError, "probe: ")
		if len(errText) > 90 {
			errText = errText[:90]
		}
		if errText == "" {
			errText = "连续探测失败"
		}
		ev := map[string]any{}
		if !in.CooldownUntil.IsZero() {
			ev["cooldown_until"] = in.CooldownUntil.Format(time.RFC3339)
		}
		reasons = append(reasons, scoreReason{Code: "probe_dead", Level: "high",
			Text: "探测判死：" + errText, Evidence: ev})
	} else if in.ProbeFail24h > 0 {
		reasons = append(reasons, scoreReason{Code: "probe_flaky", Level: "high",
			Text: fmt.Sprintf("近 24h 探测失败 %d 次（未判死，但已在阈值边缘）", in.ProbeFail24h)})
	}
	if in.State == "exhausted" {
		reasons = append(reasons, scoreReason{Code: "quota_cooldown", Level: "low",
			Text: "额度冷却中（不计质量分，只说明当前没额度）"})
	}
	if fails["connect"] > 0 {
		lv := "mid"
		if rate("connect") >= 0.1 {
			lv = "high"
		}
		reasons = append(reasons, scoreReason{Code: "connect_fail", Level: lv,
			Text: fmt.Sprintf("连接失败 %d 次（占尝试 %.1f%%）", fails["connect"], rate("connect")*100)})
	}
	if fails["cred"] > 0 {
		lv := "mid"
		if rate("cred") >= 0.1 {
			lv = "high"
		}
		reasons = append(reasons, scoreReason{Code: "cred_rejected", Level: lv,
			Text: fmt.Sprintf("被拒/凭据失效 %d 次（%.1f%%：403 FreeTier / 401 / 410）", fails["cred"], rate("cred")*100)})
	}
	if fails["http5xx"] > 0 {
		reasons = append(reasons, scoreReason{Code: "upstream_5xx", Level: "mid",
			Text: fmt.Sprintf("上游 5xx %d 次（%.1f%%，半权计入）", fails["http5xx"], rate("http5xx")*100)})
	}
	if fails["stream"] > 0 {
		reasons = append(reasons, scoreReason{Code: "stream_interrupt", Level: "low",
			Text: fmt.Sprintf("流式中断 %d 次（半权）", fails["stream"])})
	}
	if fails["confirmed_bad"] > 0 {
		reasons = append(reasons, scoreReason{Code: "confirmed_bad", Level: "high",
			Text: fmt.Sprintf("被 freetier 校验确认为坏节点 %d 次", fails["confirmed_bad"])})
	}
	if in.BlipFails > 0 {
		reasons = append(reasons, scoreReason{Code: "env_blip", Level: "info",
			Text: fmt.Sprintf("有 %d 次失败发生在池级抖动窗口（同窗口失败率≥50%%），已按 20%% 计责", in.BlipFails)})
	}
	if in.QuotaEvents > 0 {
		reasons = append(reasons, scoreReason{Code: "quota_events", Level: "low",
			Text: fmt.Sprintf("额度耗尽 %d 次（不计质量分，只说明当前无额度）", in.QuotaEvents)})
	}
	if q < 0.55 && in.EffN >= 5 {
		lv := "mid"
		if q < 0.4 {
			lv = "high"
		}
		reasons = append(reasons, scoreReason{Code: "low_quality", Level: lv,
			Text: fmt.Sprintf("真实调用成功率下界仅 %.0f%%（有效样本 %.0f）", q*100, in.EffN)})
	} else if q >= 0.8 {
		reasons = append(reasons, scoreReason{Code: "real_traffic_good", Level: "good",
			Text: fmt.Sprintf("真实调用成功率下界 %.0f%%（有效样本 %.0f）", q*100, in.EffN)})
	}
	if in.EffN < 20 {
		reasons = append(reasons, scoreReason{Code: "low_sample", Level: "mid",
			Text: fmt.Sprintf("有效样本不足（n_eff=%.0f < 20），置信度低", in.EffN)})
	}
	if !in.LastSuccessAt.IsZero() && in.LastSuccessAt.Year() > 1 {
		if sh := ref.Sub(in.LastSuccessAt).Hours(); sh > 72 {
			reasons = append(reasons, scoreReason{Code: "stale_evidence", Level: "mid",
				Text: fmt.Sprintf("最近一次真实成功在 %.0f 小时前（数据陈旧）", sh)})
		}
	}
	if in.LatencyMs > 0 {
		if in.LatencyMs > 4000 {
			reasons = append(reasons, scoreReason{Code: "slow_probe", Level: "mid",
				Text: fmt.Sprintf("探测延迟 %d ms（池中位约 1959 ms）", in.LatencyMs)})
		} else if float64(in.LatencyMs) <= p.LatFull {
			reasons = append(reasons, scoreReason{Code: "fast", Level: "good",
				Text: fmt.Sprintf("探测延迟 %d ms，快", in.LatencyMs)})
		}
	}
	if noData {
		reasons = append([]scoreReason{{Code: "no_data", Level: "mid",
			Text: "无真实调用样本、也无最新探测数据（新节点/尚未探测），按观察档处理"}}, reasons...)
	}
	if len(reasons) == 0 {
		reasons = append(reasons, scoreReason{Code: "neutral", Level: "info", Text: "无显著信号"})
	}
	sort.SliceStable(reasons, func(i, j int) bool {
		return scoringLevelOrder[reasons[i].Level] < scoringLevelOrder[reasons[j].Level]
	})

	view := nodeScoreView{
		Score:           score,
		Tier:            tier,
		TierLabel:       tierLabel,
		TierCN:          tierCN,
		Usable:          usable,
		Quality:         math.Round(q*10000) / 10000,
		EffectiveSample: math.Round(in.EffN*10) / 10,
		SampleSize:      in.Attempts,
		LowConfidence:   in.EffN < 20,
		AutoEligible:    autoEligible,
		ScoreReasons:    reasons,
		Fail: scoringFailCounts{
			Connect:      fails["connect"],
			Cred:         fails["cred"],
			HTTP5xx:      fails["http5xx"],
			Timeout:      fails["timeout"],
			Stream:       fails["stream"],
			ConfirmedBad: fails["confirmed_bad"],
		},
		QuotaEvents:      in.QuotaEvents,
		Caps:             caps,
		ProbeFail24h:     in.ProbeFail24h,
		DeadMarks7d:      in.DeadMarks7d,
		Trend:            in.Trend,
		Probe:            math.Round(probe*1000) / 1000,
		LatencyComponent: latS,
		RawScore:         math.Round(raw*10) / 10,
		ProbeFlags:       probeFlags,
	}
	if in.LatencyMs > 0 {
		lat := in.LatencyMs
		view.ProbeLatencyMS = &lat
	}
	if !in.LastSuccessAt.IsZero() && in.LastSuccessAt.Year() > 1 {
		s := in.LastSuccessAt.Format(time.RFC3339)
		view.LastSuccessAt = &s
	}
	if !in.LastTrafficAt.IsZero() && in.LastTrafficAt.Year() > 1 {
		s := in.LastTrafficAt.Format(time.RFC3339)
		view.LastTrafficAt = &s
	}
	if in.LastError != "" {
		e := in.LastError
		view.LastError = &e
	}
	if view.Trend == nil {
		view.Trend = []scoreTrendPoint{}
	}
	if view.Caps == nil {
		view.Caps = []string{}
	}
	return view
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ======================= 3. 归因与存储 =======================

// scoringStats 是每节点的真实调用/探测聚合（持久化到 scoring_state.json）。
type scoringStats struct {
	Name string `json:"name,omitempty"`

	// 真实调用（§2.2/§4.2）
	Attempts    int            `json:"attempts"`
	Success     int            `json:"success"`
	Fail        map[string]int `json:"fail,omitempty"`
	QuotaEvents int            `json:"quota_events,omitempty"`
	BlipFails   int            `json:"blip_fails,omitempty"`
	Excluded    int            `json:"excluded,omitempty"`
	Unknown     int            `json:"unknown,omitempty"`
	// 7 天半衰期衰减累加量（EffRef 是衰减参考时刻）
	EffS   float64   `json:"eff_s"`
	EffN   float64   `json:"eff_n"`
	EffRef time.Time `json:"eff_ref,omitempty"`

	LastSuccessAt time.Time `json:"last_success_at,omitempty"`
	LastTrafficAt time.Time `json:"last_traffic_at,omitempty"`

	// 探测（§2.3）：历史环形缓冲 + 24h/7d 计数由时间序列推导
	LastProbeAt     time.Time         `json:"last_probe_at,omitempty"`
	LatencyMs       int64             `json:"latency_ms,omitempty"`
	ProbeOKStreak   int               `json:"probe_ok_streak,omitempty"`
	ProbeFailStreak int               `json:"probe_fail_streak,omitempty"`
	LastProbeError  string            `json:"last_probe_error,omitempty"`
	ProbeFailTimes  []time.Time       `json:"probe_fail_times,omitempty"`
	DeadMarkTimes   []time.Time       `json:"dead_mark_times,omitempty"`
	Trend           []scoreTrendPoint `json:"trend,omitempty"`
}

// nodeScoringStats 是 scoringStats 的对外只读快照（测试用）。
type nodeScoringStats = scoringStats

func (s *scoringStats) failCount(class string) int {
	if s.Fail == nil {
		return 0
	}
	return s.Fail[class]
}

func (s *scoringStats) addFail(class string) {
	if s.Fail == nil {
		s.Fail = map[string]int{}
	}
	s.Fail[class]++
}

// decayTo 把衰减累加量推进到 t（指数半衰期 7 天，§3.2）。
// 这是「增量维护衰减和」的标准做法：每次新增证据前先把旧和衰减到当前时刻，
// 再加 1 权重 ⇒ O(1) 更新且与逐条 0.5^(age/7) 求和等价。
func (s *scoringStats) decayTo(t time.Time) {
	if t.IsZero() {
		return
	}
	if s.EffRef.IsZero() {
		s.EffRef = t
		return
	}
	dt := t.Sub(s.EffRef).Hours() / 24
	if dt <= 0 {
		return
	}
	f := math.Pow(0.5, dt/scoringHalfLifeDays)
	s.EffS *= f
	s.EffN *= f
	s.EffRef = t
}

// scoringBlipBucket 是池级抖动抑制的 10 分钟窗口计数（§4.2）。
type scoringBlipBucket struct {
	Req  int `json:"req"`
	Fail int `json:"fail"`
}

// nodeScoringStore 维护全部节点的打分状态。
type nodeScoringStore struct {
	mu      sync.Mutex
	path    string
	nodes   map[string]*scoringStats
	buckets map[int64]*scoringBlipBucket
	dirty   bool

	// 轮询游标：已归因到的最后一条 call_log 记录（TS + ReqID）
	cursorTS    string
	cursorReqID string
	// lastSeenTS 是已归因过的最新证据时间（持久化，重启后用于增量回填）
	lastSeenTS time.Time
	// started 防止 start() 被重复调用
	started bool
	// lastSave 上次落盘时间（避免写放大）
	lastSave time.Time
	// nowFn 测试可注入时钟
	nowFn func() time.Time
}

var nodeScoring = newNodeScoringStore(scoringStateFileName)

func newNodeScoringStore(path string) *nodeScoringStore {
	return &nodeScoringStore{
		path:    path,
		nodes:   map[string]*scoringStats{},
		buckets: map[int64]*scoringBlipBucket{},
		nowFn:   time.Now,
	}
}

func (s *nodeScoringStore) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

// setPath 设置落盘路径（启动时按 config.json 所在目录决定）。
func (s *nodeScoringStore) setPath(path string) {
	s.mu.Lock()
	s.path = path
	s.mu.Unlock()
}

// nodeLocked 取/建节点统计（调用方持锁）。
func (s *nodeScoringStore) nodeLocked(fp string) *scoringStats {
	st := s.nodes[fp]
	if st == nil {
		st = &scoringStats{}
		s.nodes[fp] = st
	}
	return st
}

// bucketLocked 取/建 10 分钟抖动窗口（调用方持锁），顺手清理 30 分钟前的窗口。
func (s *nodeScoringStore) bucketLocked(bucket int64) *scoringBlipBucket {
	b := s.buckets[bucket]
	if b == nil {
		b = &scoringBlipBucket{}
		s.buckets[bucket] = b
	}
	if len(s.buckets) > 64 {
		cut := bucket - 3*scoringBlipWindowSeconds/scoringBlipWindowSeconds
		for k := range s.buckets {
			if k < cut {
				delete(s.buckets, k)
			}
		}
	}
	return b
}

// ---------- 3.1 失败分类（§3.4：节点坏 1.0 / 上游抖 0.5 / 额度不计 / 非节点错剔除） ----------

var scoringHTTPCodeRe = regexp.MustCompile(`http\s+(\d{3})`)

type scoringOutcome struct {
	class  string  // 空 = 不是尝试结果（switch 等）
	weight float64 // 失败权重：1.0 节点坏 / 0.5 上游抖 / 0 成功
	counts bool    // 是否计入质量分母
	quota  bool    // 额度耗尽（不计质量，单独计数）
	at     time.Time
}

// classifyScoringEvent 与回算脚本 aggregate.py 的 classify_event 一一对应。
func classifyScoringEvent(e CallEvent) scoringOutcome {
	switch e.Type {
	case "switch":
		return scoringOutcome{} // 换节点标记：不覆盖前一条错误归因
	case "connect_ok":
		return scoringOutcome{class: "connect_ok", weight: 0, counts: true}
	case "connect_error":
		return scoringOutcome{class: "connect", weight: 1.0, counts: true}
	case "stream_interrupt":
		return scoringOutcome{class: "stream", weight: 0.5, counts: true}
	case "upstream_error":
		return classifyScoringUpstream(e.Detail)
	case "quota_halt":
		return scoringOutcome{class: "quota", quota: true}
	case "freetier_node_confirmed_bad":
		return scoringOutcome{class: "confirmed_bad", weight: 1.0, counts: true}
	case "region_restriction":
		return scoringOutcome{class: "region_restriction", weight: 0.5, counts: true}
	default:
		return scoringOutcome{}
	}
}

func classifyScoringUpstream(detail string) scoringOutcome {
	d := strings.ToLower(detail)
	code := 0
	if m := scoringHTTPCodeRe.FindStringSubmatch(d); len(m) == 2 {
		code, _ = strconv.Atoi(m[1])
	}
	switch {
	case code == 429 || strings.Contains(d, "quota") || strings.Contains(d, "freetier") || strings.Contains(d, "feusage"):
		return scoringOutcome{class: "quota", quota: true}
	case code == 401 || code == 403 || code == 410 || code == 426:
		return scoringOutcome{class: "cred", weight: 1.0, counts: true}
	case code >= 500 && code <= 599:
		return scoringOutcome{class: "http5xx", weight: 0.5, counts: true}
	case code == 400 || code == 404:
		return scoringOutcome{class: "req_shape"} // 请求形态/模型不存在：剔除
	case code != 0:
		return scoringOutcome{class: "http_other", weight: 0.5, counts: true}
	case strings.Contains(d, "timeout") || strings.Contains(d, "deadline") || strings.Contains(d, "eof") || strings.Contains(d, "reset"):
		return scoringOutcome{class: "timeout", weight: 1.0, counts: true}
	default:
		return scoringOutcome{class: "other_err", weight: 0.5, counts: true}
	}
}

// parseScoringTS 解析 call_log 的 ts；失败时回退 fallback。
func parseScoringTS(v string, fallback time.Time) time.Time {
	if v == "" {
		return fallback
	}
	if tt, err := time.Parse(time.RFC3339, v); err == nil {
		return tt
	}
	return fallback
}

// ---------- 3.2 真实调用归因（§4.2 口径） ----------

// observeCallRecord 把一条调用日志归因进每节点统计。
// 口径与回算脚本一致：nodes[] 中每个节点算一次尝试；events 里该节点最后一条
// 「尝试结果」事件决定成败；无事件且 status=ok ⇒ 最后一个节点记成功（25,566 次的兜底）。
func (s *nodeScoringStore) observeCallRecord(rec CallRecord, now time.Time) {
	ts := parseScoringTS(rec.TS, now)
	if ts.IsZero() {
		ts = now
	}
	// 池级抖动窗口（10 分钟）：窗口内池失败率≥50% 且请求数≥5 ⇒ 失败只背 20% 锅
	bucket := ts.Unix() / scoringBlipWindowSeconds
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observeCallRecordLocked(rec, ts, bucket, false, now)
}

// observeCallRecordLocked 是归因实现（调用方持锁）。
// historical=true 时用「证据年龄」作为衰减权重（回填历史日志）；实时记录权重为 1。
func (s *nodeScoringStore) observeCallRecordLocked(rec CallRecord, ts time.Time, bucket int64, historical bool, now time.Time) {
	b := s.bucketLocked(bucket)
	b.Req++
	if rec.Status == "fail" {
		b.Fail++
	}
	blip := b.Req >= 5 && b.Fail*2 >= b.Req

	// 每节点最后一次尝试结果事件（switch 不覆盖）
	outcomes := make(map[string]scoringOutcome, len(rec.Events))
	for _, e := range rec.Events {
		if e.Node == "" {
			continue
		}
		oc := classifyScoringEvent(e)
		if oc.class == "" {
			continue
		}
		oc.at = parseScoringTS(e.At, ts)
		outcomes[e.Node] = oc
	}

	w := 1.0
	if historical {
		ageDays := math.Max(0, now.Sub(ts).Hours()/24)
		w = math.Pow(0.5, ageDays/scoringHalfLifeDays)
	}

	serving := ""
	if rec.Status == "ok" && len(rec.Nodes) > 0 {
		serving = rec.Nodes[len(rec.Nodes)-1]
	}
	if ts.After(s.lastSeenTS) {
		s.lastSeenTS = ts
	}

	for i, fp := range rec.Nodes {
		if fp == "" {
			continue
		}
		st := s.nodeLocked(fp)
		if st.Name == "" && i < len(rec.NodeNames) {
			st.Name = rec.NodeNames[i]
		}
		if !historical {
			st.decayTo(ts)
		}
		if st.LastTrafficAt.IsZero() || ts.After(st.LastTrafficAt) {
			st.LastTrafficAt = ts
		}
		oc, has := outcomes[fp]
		switch {
		case has && oc.quota:
			st.QuotaEvents++
			continue
		case has && !oc.counts:
			st.Excluded++
			continue
		case has:
			st.Attempts++
			st.EffN += w
			if oc.class == "connect_ok" || oc.weight == 0 {
				st.Success++
				st.EffS += w
				if ts.After(st.LastSuccessAt) {
					st.LastSuccessAt = ts
				}
				continue
			}
			st.addFail(oc.class)
			blame := oc.weight
			if blip {
				blame *= scoringBlipFactor
				st.BlipFails++
			}
			st.EffS += w * (1.0 - blame) // 失败按「1 - 权重」折算成功质量
		case fp == serving:
			st.Attempts++
			st.Success++
			st.EffN += w
			st.EffS += w
			if ts.After(st.LastSuccessAt) {
				st.LastSuccessAt = ts
			}
		case len(rec.Events) == 0 && rec.Status == "ok" && i == len(rec.Nodes)-1:
			st.Attempts++
			st.Success++
			st.EffN += w
			st.EffS += w
			if ts.After(st.LastSuccessAt) {
				st.LastSuccessAt = ts
			}
		default:
			st.Unknown++
		}
	}
	s.dirty = true
}

// ---------- 3.3 探测观测（环形缓冲 20 轮 + 24h/7d 计数） ----------

// probeObservation 是一次健康探测结果。
type probeObservation struct {
	FP         string
	Name       string
	OK         bool
	LatencyMs  int64
	ErrText    string
	State      string // 探测后的节点状态：available|dead|exhausted
	DeadMarked bool   // 本次探测是否触发了 dead 标记（用于 dead_marks_7d）
	At         time.Time
}

// observeProbeResult 记录一次探测结果：更新连续成功/失败、延迟、环形缓冲、趋势。
func (s *nodeScoringStore) observeProbeResult(fp, name string, ok bool, latencyMs int64, errText, state string, at time.Time) {
	s.observeProbe(probeObservation{
		FP: fp, Name: name, OK: ok, LatencyMs: latencyMs, ErrText: errText, State: state,
		DeadMarked: !ok && state == "dead", At: at,
	})
}

// observeProbe 记录一次探测结果（完整版）。
func (s *nodeScoringStore) observeProbe(o probeObservation) {
	if o.FP == "" {
		return
	}
	at := o.At
	if at.IsZero() {
		at = s.now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.nodeLocked(o.FP)
	if o.Name != "" {
		st.Name = o.Name
	}
	if st.LastProbeAt.IsZero() || at.After(st.LastProbeAt) {
		st.LastProbeAt = at
	}
	if o.OK {
		st.ProbeOKStreak++
		st.ProbeFailStreak = 0
		if o.LatencyMs > 0 {
			st.LatencyMs = o.LatencyMs
		}
		st.LastProbeError = ""
	} else {
		st.ProbeOKStreak = 0
		st.ProbeFailStreak++
		st.LatencyMs = -1
		if o.ErrText != "" {
			st.LastProbeError = o.ErrText
		}
		st.ProbeFailTimes = append(st.ProbeFailTimes, at)
		if o.DeadMarked {
			st.DeadMarkTimes = append(st.DeadMarkTimes, at)
		}
	}
	// 环形缓冲只保留最近 scoringTrendCapacity 轮
	trendScore := s.currentScoreLocked(st, at, o.State)
	st.Trend = append(st.Trend, scoreTrendPoint{
		TS:      at.Format(time.RFC3339),
		Score:   trendScore.Score,
		ProbeOK: o.OK,
	})
	if len(st.Trend) > scoringTrendCapacity {
		st.Trend = st.Trend[len(st.Trend)-scoringTrendCapacity:]
	}
	st.pruneTimesLocked(at)
	s.dirty = true
}

// pruneTimesLocked 把 7 天前的探测失败/判死时间戳清掉（24h/7d 计数只依赖这 7 天窗口）。
func (st *scoringStats) pruneTimesLocked(now time.Time) {
	cut := now.Add(-7 * 24 * time.Hour)
	prune := func(list []time.Time) []time.Time {
		out := list[:0]
		for _, t := range list {
			if t.After(cut) {
				out = append(out, t)
			}
		}
		return out
	}
	st.ProbeFailTimes = prune(st.ProbeFailTimes)
	st.DeadMarkTimes = prune(st.DeadMarkTimes)
}

// currentScoreLocked 用当前统计 + 给定状态算一次分数（趋势点用，调用方持锁）。
func (s *nodeScoringStore) currentScoreLocked(st *scoringStats, now time.Time, state string) nodeScoreView {
	cp := *st
	cp.decayTo(now)
	in := nodeScoringInput{
		State:         state,
		LatencyMs:     cp.LatencyMs,
		LastProbeAt:   cp.LastProbeAt,
		LastError:     cp.LastProbeError,
		Attempts:      cp.Attempts,
		Success:       cp.Success,
		Fail:          cp.Fail,
		QuotaEvents:   cp.QuotaEvents,
		BlipFails:     cp.BlipFails,
		EffS:          cp.EffS,
		EffN:          cp.EffN,
		LastSuccessAt: cp.LastSuccessAt,
		LastTrafficAt: cp.LastTrafficAt,
		ProbeFail24h:  countSince(cp.ProbeFailTimes, now.Add(-24*time.Hour)),
		DeadMarks7d:   len(cp.DeadMarkTimes),
		ProbeOKStreak: cp.ProbeOKStreak,
	}
	return scoreNode(in, now, now, defaultScoringParams)
}

// countSince 统计列表中晚于 cut 的条数。
func countSince(list []time.Time, cut time.Time) int {
	n := 0
	for _, t := range list {
		if t.After(cut) {
			n++
		}
	}
	return n
}

// statsCopy 返回某节点的统计快照（测试/排查用）。
func (s *nodeScoringStore) statsCopy(fp string) (nodeScoringStats, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.nodes[fp]
	if !ok {
		return nodeScoringStats{}, false
	}
	cp := *st
	if st.Fail != nil {
		cp.Fail = make(map[string]int, len(st.Fail))
		for k, v := range st.Fail {
			cp.Fail[k] = v
		}
	}
	cp.Trend = append([]scoreTrendPoint(nil), st.Trend...)
	cp.ProbeFailTimes = append([]time.Time(nil), st.ProbeFailTimes...)
	cp.DeadMarkTimes = append([]time.Time(nil), st.DeadMarkTimes...)
	return cp, true
}

// ---------- 3.4 /api/nodes 快照打分 ----------

// nodeScoringSnapshotInput 是节点池运行时状态（handler 从 proxyPool.snapshot() 组装）。
type nodeScoringSnapshotInput struct {
	FP            string
	Name          string
	State         string // available|dead|exhausted
	LatencyMs     int64
	LastProbeAt   time.Time
	LastError     string
	CooldownUntil time.Time
	LastUsedAt    time.Time
}

// scoreSnapshot 对一批节点打分（纯内存、O(节点数)，219 节点 << 50ms 预算）。
func (s *nodeScoringStore) scoreSnapshot(inputs []nodeScoringSnapshotInput, now time.Time) []nodeScoreView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]nodeScoreView, 0, len(inputs))
	for _, in := range inputs {
		var cp scoringStats
		if st, ok := s.nodes[in.FP]; ok {
			cp = *st
			if cp.Fail != nil {
				cp.Fail = make(map[string]int, len(st.Fail))
				for k, v := range st.Fail {
					cp.Fail[k] = v
				}
			}
			cp.Trend = append([]scoreTrendPoint(nil), st.Trend...)
			cp.ProbeFailTimes = append([]time.Time(nil), st.ProbeFailTimes...)
			cp.DeadMarkTimes = append([]time.Time(nil), st.DeadMarkTimes...)
		}
		cp.decayTo(now)
		ni := nodeScoringInput{
			FP: in.FP, Name: in.Name,
			State:         in.State,
			LatencyMs:     in.LatencyMs,
			LastProbeAt:   in.LastProbeAt,
			LastError:     in.LastError,
			CooldownUntil: in.CooldownUntil,
			Attempts:      cp.Attempts,
			Success:       cp.Success,
			Fail:          cp.Fail,
			QuotaEvents:   cp.QuotaEvents,
			BlipFails:     cp.BlipFails,
			EffS:          cp.EffS,
			EffN:          cp.EffN,
			LastSuccessAt: cp.LastSuccessAt,
			LastTrafficAt: cp.LastTrafficAt,
			ProbeFail24h:  countSince(cp.ProbeFailTimes, now.Add(-24*time.Hour)),
			DeadMarks7d:   len(cp.DeadMarkTimes),
			ProbeOKStreak: cp.ProbeOKStreak,
			Trend:         cp.Trend,
		}
		view := scoreNode(ni, now, now, defaultScoringParams)
		if view.LastError == nil && in.LastError != "" {
			// 池里的 last_error 是探测判死原因（死节点才有），优先展示
			e := in.LastError
			view.LastError = &e
		}
		out = append(out, view)
	}
	return out
}

// ---------- 3.5 持久化 ----------

type scoringPersistFile struct {
	Version  int                      `json:"version"`
	SavedAt  time.Time                `json:"saved_at"`
	LastSeen time.Time                `json:"last_seen,omitempty"`
	Nodes    map[string]*scoringStats `json:"nodes"`
}

// save 原子写 scoring_state.json（写临时文件再 rename，避免读到半截文件）。
func (s *nodeScoringStore) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *nodeScoringStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	f := scoringPersistFile{
		Version:  scoringStateVersion,
		SavedAt:  s.now(),
		LastSeen: s.lastSeenTS,
		Nodes:    s.nodes,
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.dirty = false
	s.lastSave = s.now()
	return nil
}

// load 读取 scoring_state.json。文件不存在/坏 JSON/版本不符 ⇒ 空状态安全降级。
func (s *nodeScoringStore) load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f scoringPersistFile
	if err := json.Unmarshal(data, &f); err != nil {
		slog.Warn("scoring state corrupt (ignored)", "path", s.path, "error", err)
		return nil
	}
	if f.Version < 0 || f.Version > scoringStateVersion {
		slog.Warn("scoring state version unsupported (ignored)", "path", s.path, "version", f.Version)
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes = f.Nodes
	if s.nodes == nil {
		s.nodes = map[string]*scoringStats{}
	}
	s.lastSeenTS = f.LastSeen
	now := s.now()
	for _, st := range s.nodes {
		if st == nil {
			continue
		}
		st.pruneTimesLocked(now)
		if len(st.Trend) > scoringTrendCapacity {
			st.Trend = st.Trend[len(st.Trend)-scoringTrendCapacity:]
		}
	}
	s.dirty = false
	return nil
}

// maybeSave 距上次落盘超过 30s 且有变更才写（避免写放大，见 task-55 的教训）。
func (s *nodeScoringStore) maybeSave() {
	s.mu.Lock()
	need := s.dirty && s.now().Sub(s.lastSave) >= 30*time.Second
	s.mu.Unlock()
	if !need {
		return
	}
	if err := s.save(); err != nil {
		slog.Warn("scoring state save failed", "error", err)
	}
}

// isEmpty 报告是否没有任何真实调用/探测证据（首次启动 ⇒ 需要回填历史日志）。
func (s *nodeScoringStore) isEmpty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.nodes {
		if st.Attempts > 0 || st.ProbeFailStreak > 0 || len(st.Trend) > 0 {
			return false
		}
	}
	return true
}

// ---------- 3.6 历史回填（首启/停机补课） ----------

// forEachCallLogRecord 逐行读一个 call_log 段（支持 .gz），fn 返回 false 可提前停止。
func forEachCallLogRecord(seg callLogSegment, fn func(CallRecord) bool) error {
	f, err := os.Open(seg.path)
	if err != nil {
		return err
	}
	defer f.Close()
	var r *bufio.Reader
	if seg.gz {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = bufio.NewReaderSize(zr, 256*1024)
	} else {
		r = bufio.NewReaderSize(f, 256*1024)
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec CallRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if !fn(rec) {
			return nil
		}
	}
	return sc.Err()
}

// backfillFromLog 从磁盘调用日志回填归因（两遍：先算抖动窗口，再归因）。
// 只处理 ts > since 的记录（首启 since 为零值 ⇒ 全量；重启补课只补停机期间漏掉的）。
func (s *nodeScoringStore) backfillFromLog(since, cutoff, now time.Time) (int, error) {
	segs := callLog.readSegments()
	if len(segs) == 0 {
		return 0, nil
	}
	segs = scoringRelevantSegments(segs, since)
	// 第 1 遍：抖动窗口计数（只统计 since 之后的记录，与归因范围一致）
	s.mu.Lock()
	for k := range s.buckets {
		delete(s.buckets, k)
	}
	s.mu.Unlock()
	for _, seg := range segs {
		if err := forEachCallLogRecord(seg, func(rec CallRecord) bool {
			ts := parseScoringTS(rec.TS, time.Time{})
			if ts.IsZero() || !ts.After(since) || ts.After(cutoff) {
				return true
			}
			bucket := ts.Unix() / scoringBlipWindowSeconds
			s.mu.Lock()
			b := s.bucketLocked(bucket)
			b.Req++
			if rec.Status == "fail" {
				b.Fail++
			}
			s.mu.Unlock()
			return true
		}); err != nil {
			slog.Warn("scoring backfill pass1 failed", "segment", seg.name, "error", err)
		}
	}
	// 第 2 遍：逐条归因（historical=true ⇒ 按证据年龄衰减）
	// 先把已有累加量统一衰减到 now，否则「历史权重（已衰减到 now）」加到「旧 ref 的和」上会不自洽。
	s.mu.Lock()
	for _, st := range s.nodes {
		st.decayTo(now)
	}
	s.mu.Unlock()
	attributed := 0
	for _, seg := range segs {
		if err := forEachCallLogRecord(seg, func(rec CallRecord) bool {
			ts := parseScoringTS(rec.TS, time.Time{})
			if ts.IsZero() || !ts.After(since) || ts.After(cutoff) {
				return true
			}
			bucket := ts.Unix() / scoringBlipWindowSeconds
			s.mu.Lock()
			s.observeCallRecordLocked(rec, ts, bucket, true, now)
			s.mu.Unlock()
			attributed++
			return true
		}); err != nil {
			slog.Warn("scoring backfill pass2 failed", "segment", seg.name, "error", err)
		}
	}
	if attributed > 0 {
		if err := s.save(); err != nil {
			slog.Warn("scoring backfill save failed", "error", err)
		}
	}
	slog.Info("scoring backfill done", "records", attributed,
		"since", since.Format(time.RFC3339), "cutoff", cutoff.Format(time.RFC3339), "now", now.Format(time.RFC3339))
	return attributed, nil
}

// scoringRelevantSegments 过滤掉整段都早于 since 的日分文件（避免每次重启都把 100MB 全读一遍）。
func scoringRelevantSegments(segs []callLogSegment, since time.Time) []callLogSegment {
	if since.IsZero() {
		return segs
	}
	out := make([]callLogSegment, 0, len(segs))
	for _, seg := range segs {
		if seg.day == "" {
			out = append(out, seg) // legacy 单文件：无条件扫（里面有全部历史）
			continue
		}
		day, err := time.ParseInLocation("2006-01-02", seg.day, time.Local)
		if err != nil || day.Add(24*time.Hour).After(since) {
			out = append(out, seg)
		}
	}
	return out
}

// ---------- 3.7 后台轮询 + 启动 ----------

// pollCallLog 归因内存环里新出现的调用记录（最多 500 条/轮，5 秒一轮足够生产量级）。
func (s *nodeScoringStore) pollCallLog() int {
	recs := callLog.latest(500)
	if len(recs) == 0 {
		return 0
	}
	s.mu.Lock()
	cursorTS, cursorReqID := s.cursorTS, s.cursorReqID
	s.mu.Unlock()

	start := 0
	if cursorReqID != "" {
		found := -1
		for i := len(recs) - 1; i >= 0; i-- {
			if recs[i].ReqID == cursorReqID {
				found = i
				break
			}
		}
		if found >= 0 {
			start = found + 1
		} else if cursorTS != "" {
			start = len(recs)
			for i, r := range recs {
				if r.TS > cursorTS {
					start = i
					break
				}
			}
		}
	}
	n := 0
	for _, rec := range recs[start:] {
		if rec.ReqID == cursorReqID {
			continue
		}
		s.observeCallRecord(rec, s.now())
		n++
	}
	if len(recs) > 0 {
		last := recs[len(recs)-1]
		s.mu.Lock()
		s.cursorTS, s.cursorReqID = last.TS, last.ReqID
		s.mu.Unlock()
	}
	return n
}

// start 启动后台任务：轮询新调用记录 + 周期落盘 + （首次）回填历史日志。
func (s *nodeScoringStore) start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	since := s.lastSeenTS
	cutoff := s.now()
	s.mu.Unlock()

	// 回填 (lastSeen, 启动时刻] 的调用日志：首启 = 全量历史；重启 = 补停机期间的缺口。
	// 启动后的新记录由 pollCallLog 负责，两者以 cutoff 为界，不会重复计数。
	go func() {
		if _, err := s.backfillFromLog(since, cutoff, s.now()); err != nil {
			slog.Warn("scoring backfill failed", "error", err)
		}
	}()
	// 轮询游标初始化：避免把「回填已经处理过的历史记录」再算一遍
	go func() {
		if recs := callLog.latest(1); len(recs) > 0 {
			s.mu.Lock()
			s.cursorTS, s.cursorReqID = recs[0].TS, recs[0].ReqID
			s.mu.Unlock()
		}
	}()
	go func() {
		poll := time.NewTicker(5 * time.Second)
		save := time.NewTicker(30 * time.Second)
		defer poll.Stop()
		defer save.Stop()
		for {
			select {
			case <-poll.C:
				s.pollCallLog()
			case <-save.C:
				s.maybeSave()
			}
		}
	}()
}

// initNodeScoring 启动打分器：落盘路径与 config.json 同目录，加载历史 + 启动后台任务。
func initNodeScoring(configPath string) {
	base := filepath.Dir(configPath)
	nodeScoring.setPath(filepath.Join(base, scoringStateFileName))
	if err := nodeScoring.load(); err != nil {
		slog.Warn("scoring state load failed (starting empty)", "error", err)
	}
	nodeScoring.start()
}

// nodeScoringSnapshot 供 /api/nodes 调用：把节点池状态映射成打分输入并返回 fp→view。
func nodeScoringSnapshot(nodes []*ProxyNode, now time.Time) ([]nodeScoreView, map[string]nodeScoreView) {
	inputs := make([]nodeScoringSnapshotInput, 0, len(nodes))
	for _, n := range nodes {
		inputs = append(inputs, nodeScoringSnapshotInput{
			FP:            n.Fingerprint,
			Name:          n.Name,
			State:         n.State.String(),
			LatencyMs:     n.LatencyMs,
			LastProbeAt:   n.LastProbeAt,
			LastError:     n.LastError,
			CooldownUntil: n.CooldownUntil,
			LastUsedAt:    n.LastUsedAt,
		})
	}
	views := nodeScoring.scoreSnapshot(inputs, now)
	byFP := make(map[string]nodeScoreView, len(views))
	for i, v := range views {
		byFP[inputs[i].FP] = v
	}
	return views, byFP
}
