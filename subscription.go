package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ======================= 配置形态 =======================

// SubscriptionConfig 一条订阅源配置（config.json subscriptions[]）。
type SubscriptionConfig struct {
	Name                string `json:"name"`
	URL                 string `json:"url"`
	UpdateIntervalHours int    `json:"update_interval_hours,omitempty"` // 0 → 默认 24h
}

func (s SubscriptionConfig) updateInterval() time.Duration {
	if s.UpdateIntervalHours > 0 {
		return time.Duration(s.UpdateIntervalHours) * time.Hour
	}
	return 24 * time.Hour
}

// ======================= 订阅管理器 =======================

// SubscriptionMeta 订阅响应头元信息（subscription-userinfo / Content-Disposition）。
type SubscriptionMeta struct {
	Upload     int64  `json:"upload,omitempty"`
	Download   int64  `json:"download,omitempty"`
	Total      int64  `json:"total,omitempty"`
	Expire     int64  `json:"expire,omitempty"`
	RemoteName string `json:"remote_name,omitempty"`
}

// subscriptionManager 负责拉取订阅、解析文本、与自定义/遗留节点合并且写入节点池。
type subscriptionManager struct {
	mu       sync.Mutex
	subs     []SubscriptionConfig
	customs  []ProxyNodeConfig // 面板手填节点
	cacheDir string

	lastUpdated map[string]time.Time
	lastError   map[string]string
	lastCount   map[string]int
	lastMeta    map[string]SubscriptionMeta
}

var subManager = &subscriptionManager{
	lastUpdated: map[string]time.Time{},
	lastError:   map[string]string{},
	lastCount:   map[string]int{},
	lastMeta:    map[string]SubscriptionMeta{},
}

// SubInfo 订阅源运行时快照（面板展示用）。
type SubInfo struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	IntervalHours int    `json:"interval_hours,omitempty"`
	Nodes         int    `json:"nodes"`
	LastUpdatedAt string `json:"last_updated_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	RemoteName    string `json:"remote_name,omitempty"`
	UsageTotal    int64  `json:"usage_total,omitempty"`
	UsageUsed     int64  `json:"usage_used,omitempty"`
	UsageExpire   int64  `json:"usage_expire,omitempty"`
}

// snapshot 返回订阅源当前状态（URL/间隔/节点数/上次拉取/错误/流量）。
func (m *subscriptionManager) snapshot() []SubInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SubInfo, 0, len(m.subs))
	for _, s := range m.subs {
		info := SubInfo{
			Name:          s.Name,
			URL:           s.URL,
			IntervalHours: s.UpdateIntervalHours,
			Nodes:         m.lastCount[s.URL],
			LastError:     m.lastError[s.URL],
		}
		if t, ok := m.lastUpdated[s.URL]; ok && !t.IsZero() {
			info.LastUpdatedAt = t.Format(time.RFC3339)
		}
		if meta, ok := m.lastMeta[s.URL]; ok {
			info.RemoteName = meta.RemoteName
			info.UsageTotal = meta.Total
			info.UsageUsed = meta.Upload + meta.Download
			info.UsageExpire = meta.Expire
		}
		out = append(out, info)
	}
	return out
}

// configure 初始化订阅源与缓存目录（可在面板中重复调用以更新配置）。
func (m *subscriptionManager) configure(subs []SubscriptionConfig, customs []ProxyNodeConfig, cacheDir string) {
	m.mu.Lock()
	m.subs = subs
	m.customs = customs
	m.cacheDir = cacheDir
	m.mu.Unlock()
}

// refreshAll 拉取全部订阅并重新合并节点池。force=true 时忽略更新间隔。
// 返回合并后的节点总数；订阅全部失败但仍有手填/遗留节点时不算错误。
func (m *subscriptionManager) refreshAll(ctx context.Context, force bool) (int, error) {
	m.mu.Lock()
	subs := make([]SubscriptionConfig, len(m.subs))
	copy(subs, m.subs)
	customs := make([]ProxyNodeConfig, len(m.customs))
	copy(customs, m.customs)
	cacheDir := m.cacheDir
	lastUpdated := make(map[string]time.Time, len(m.lastUpdated))
	lastError := make(map[string]string, len(m.lastError))
	for k, v := range m.lastUpdated {
		lastUpdated[k] = v
	}
	for k, v := range m.lastError {
		lastError[k] = v
	}
	m.mu.Unlock()

	var all []*ProxyNode
	var errs []string

	// 手填/遗留节点始终并入（不因订阅全部失败而丢失出口）
	for _, c := range customs {
		if n := c.toNode(); n != nil && n.Address != "" {
			all = append(all, n)
		}
	}
	all = append(all, legacySocks5Nodes()...)

	// 并发拉取订阅
	type result struct {
		sub   SubscriptionConfig
		nodes []*ProxyNode
		meta  SubscriptionMeta
		err   error
	}
	results := make(chan result, len(subs))
	now := time.Now()
	for _, s := range subs {
		go func(s SubscriptionConfig) {
			if prev, ok := lastUpdated[s.URL]; !force && ok && now.Sub(prev) < s.updateInterval() {
				results <- result{sub: s}
				return
			}
			nodes, meta, err := m.fetchOnce(ctx, s.URL, cacheDir)
			results <- result{sub: s, nodes: nodes, meta: meta, err: err}
		}(s)
	}
	for range subs {
		r := <-results
		m.mu.Lock()
		key := r.sub.URL
		if r.err != nil {
			m.lastError[key] = r.err.Error()
			errs = append(errs, fmt.Sprintf("订阅 %s(%s): %s", r.sub.Name, r.sub.URL, r.err))
		} else {
			m.lastUpdated[key] = time.Now()
			m.lastCount[key] = len(r.nodes)
			m.lastMeta[key] = r.meta
			delete(m.lastError, key)
			for _, n := range r.nodes {
				if r.sub.Name != "" {
					n.Name = r.sub.Name + "::" + n.Name
				}
				all = append(all, n)
			}
		}
		m.mu.Unlock()
	}

	if len(all) == 0 {
		return 0, fmt.Errorf("没有任何可用节点: %s", strings.Join(errs, "; "))
	}
	proxyPool.setNodes(all)
	if err := mihomoMgr.apply(all); err != nil {
		slog.Error("mihomo 配置重建失败", "error", err)
	}
	slog.Info("节点池已刷新", "total", len(all), "subscriptions", len(subs), "errors", len(errs))
	return len(all), nil
}

func (s SubscriptionConfig) Key() string { return s.URL }

// fetchOnce 拉取单个订阅；网络失败回退磁盘缓存。
func (m *subscriptionManager) fetchOnce(ctx context.Context, rawURL, cacheDir string) ([]*ProxyNode, SubscriptionMeta, error) {
	text, meta, err := m.fetch(ctx, rawURL)
	if err != nil {
		if cached, cerr := readCachedSubscription(cacheDir, rawURL); cerr == nil {
			slog.Warn("订阅拉取失败，使用缓存", "url", rawURL, "error", err)
			return parseSubscriptionText(cached), SubscriptionMeta{}, nil
		}
		return nil, SubscriptionMeta{}, err
	}
	_ = writeCachedSubscription(cacheDir, rawURL, text)
	return parseSubscriptionText(text), meta, nil
}

// fetch 获取订阅原始文本（http/https/data URI），并解析响应头元信息。
func (m *subscriptionManager) fetch(ctx context.Context, rawURL string) (string, SubscriptionMeta, error) {
	if strings.HasPrefix(rawURL, "data:") {
		body := rawURL[strings.Index(rawURL, ",")+1:]
		dec, err := base64DecodeLoose(body)
		if err != nil {
			return "", SubscriptionMeta{}, fmt.Errorf("data URI 解码: %w", err)
		}
		return string(dec), SubscriptionMeta{}, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", SubscriptionMeta{}, err
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", SubscriptionMeta{}, fmt.Errorf("不支持的订阅协议 %q", u.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", SubscriptionMeta{}, err
	}
	req.Header.Set("User-Agent", "opencode2api/"+versionString()+" (node subscription)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", SubscriptionMeta{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", SubscriptionMeta{}, fmt.Errorf("订阅 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return "", SubscriptionMeta{}, err
	}
	return string(body), parseSubscriptionMeta(resp.Header), nil
}

// parseSubscriptionMeta 解析订阅响应头：
//   - 任意以 "subscription-userinfo" 结尾的 header（如 x-amz-meta-subscription-userinfo）
//     携带 upload/download/total/expire 流量与到期信息；
//   - Content-Disposition 提供订阅文件名（filename*= 优先，percent-decode）。
func parseSubscriptionMeta(h http.Header) SubscriptionMeta {
	var meta SubscriptionMeta
	for name, vals := range h {
		if strings.HasSuffix(strings.ToLower(name), "subscription-userinfo") {
			for _, v := range vals {
				for _, kv := range strings.Fields(v) {
					key, val, ok := strings.Cut(kv, "=")
					if !ok {
						continue
					}
					n, _ := strconv.ParseInt(val, 10, 64)
					switch strings.ToLower(key) {
					case "upload":
						meta.Upload = n
					case "download":
						meta.Download = n
					case "total":
						meta.Total = n
					case "expire":
						meta.Expire = n
					}
				}
			}
		}
	}
	if cd := h.Get("Content-Disposition"); cd != "" {
		for _, part := range strings.Split(cd, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "filename*=") {
				if name := parseExtValue(part[len("filename*="):]); name != "" {
					meta.RemoteName = name
					break
				}
			}
			if strings.HasPrefix(part, "filename=") {
				meta.RemoteName = strings.Trim(strings.TrimSpace(part[len("filename="):]), `"`)
			}
		}
	}
	return meta
}

// parseExtValue 解析 RFC 5987 扩展值：UTF-8”<percent-encoded>。
func parseExtValue(v string) string {
	rest := v
	if _, after, ok := strings.Cut(v, "'"); ok {
		if _, after2, ok := strings.Cut(after, "'"); ok {
			rest = after2
		} else {
			rest = after
		}
	}
	if dec, err := url.QueryUnescape(rest); err == nil {
		return dec
	}
	return rest
}

// ======================= 订阅缓存 =======================

func subscriptionCachePath(cacheDir, rawURL string) string {
	return filepath.Join(cacheDir, "sub-"+fmt.Sprintf("%x", sha256.Sum256([]byte(rawURL)))[:16]+".txt")
}

func readCachedSubscription(cacheDir, rawURL string) (string, error) {
	if cacheDir == "" {
		return "", os.ErrNotExist
	}
	b, err := os.ReadFile(subscriptionCachePath(cacheDir, rawURL))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func writeCachedSubscription(cacheDir, rawURL, text string) error {
	if cacheDir == "" {
		return nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(subscriptionCachePath(cacheDir, rawURL), []byte(text), 0o644)
}

// ======================= 解析：文本 → 节点 =======================

// parseSubscriptionText 解析订阅文本：兼容 Clash YAML、URI 列表、整体 base64。
// 不支持的节点类型（vmess/trojan/ws 等）被跳过。
func parseSubscriptionText(text string) []*ProxyNode {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	// 1) Clash YAML
	if strings.Contains(text, "proxies:") {
		if clash := parseClashYAML(text); len(clash) > 0 {
			return uniqueNodeNames(clash)
		}
	}
	// 2) 整文 base64（常见订阅格式）
	if dec, err := base64DecodeLoose(text); err == nil && len(dec) > 20 && strings.Contains(string(dec), "://") {
		if inner := parseLines(string(dec)); len(inner) > 0 {
			return uniqueNodeNames(inner)
		}
	}
	// 3) 逐行 URI
	if nodes := parseLines(text); len(nodes) > 0 {
		return uniqueNodeNames(nodes)
	}
	return nil
}

func parseLines(chunk string) []*ProxyNode {
	var out []*ProxyNode
	for _, line := range strings.Split(chunk, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		n := parseNodeURI(line)
		if n == nil || isInfoPseudoNode(n.Name) {
			continue
		}
		out = appendNodeUnique(out, n)
	}
	return out
}

func appendNodeUnique(list []*ProxyNode, n *ProxyNode) []*ProxyNode {
	fp := computeFingerprint(n)
	for _, ex := range list {
		if ex.Fingerprint == fp {
			return list
		}
	}
	n.Fingerprint = fp
	return append(list, n)
}

// ======================= Clash YAML =======================

type clashProxyEntry struct {
	Name              string `yaml:"name"`
	Type              string `yaml:"type"`
	Server            string `yaml:"server"`
	Port              int    `yaml:"port"`
	UUID              string `yaml:"uuid"`
	Password          string `yaml:"password"`
	Username          string `yaml:"username"`
	Cipher            string `yaml:"cipher"`
	Method            string `yaml:"method"`
	AlterID           int    `yaml:"alterId"`
	Network           string `yaml:"network"`
	TLS               bool   `yaml:"tls"`
	ServerName        string `yaml:"servername"`
	SNI               string `yaml:"sni"`
	Flow              string `yaml:"flow"`
	SkipVerify        bool   `yaml:"skip-cert-verify"`
	Insecure          bool   `yaml:"insecure"`
	Auth              string `yaml:"auth"`
	ClientFingerprint string `yaml:"client-fingerprint"`
	WSOpts            struct {
		Path                string            `yaml:"path"`
		Headers             map[string]string `yaml:"headers"`
		MaxEarlyData        int               `yaml:"max-early-data"`
		EarlyDataHeaderName string            `yaml:"early-data-header-name"`
	} `yaml:"ws-opts"`
	RealityOpts struct {
		PublicKey   string `yaml:"public-key"`
		ShortID     string `yaml:"short-id"`
		SpiderX     string `yaml:"spider-x"`
		Fingerprint string `yaml:"fingerprint"`
	} `yaml:"reality-opts"`
}

func parseClashYAML(text string) []*ProxyNode {
	var doc struct {
		Proxies []clashProxyEntry `yaml:"proxies"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		slog.Debug("clash yaml unmarshal failed", "error", err)
		return nil
	}
	var nodes []*ProxyNode
	for _, p := range doc.Proxies {
		n := clashEntryToNode(p)
		if n != nil {
			if name, err := url.QueryUnescape(n.Name); err == nil && name != "" {
				n.Name = name
			}
			if isInfoPseudoNode(n.Name) {
				continue
			}
			nodes = appendNodeUnique(nodes, n)
		} else {
			slog.Warn("跳过不支持的 Clash 代理类型", "name", p.Name, "type", p.Type)
		}
	}
	return nodes
}

func clashEntryToNode(p clashProxyEntry) *ProxyNode {
	switch strings.ToLower(p.Type) {
	case "socks5":
		return &ProxyNode{
			Name: p.Name, Protocol: "socks5", Address: p.Server, Port: p.Port,
			UserID: p.Username, Password: p.Password,
		}
	case "ss":
		method := p.Cipher
		if method == "" {
			method = p.Method
		}
		return &ProxyNode{
			Name: p.Name, Protocol: "ss", Address: p.Server, Port: p.Port,
			Method: method, Password: p.Password,
		}
	case "vless":
		n := &ProxyNode{
			Name: p.Name, Protocol: "vless", Address: p.Server, Port: p.Port,
			UserID: p.UUID, Flow: p.Flow,
			Insecure: p.SkipVerify || p.Insecure, TLS: p.TLS,
			Network: strings.ToLower(p.Network),
			Path:    p.WSOpts.Path,
		}
		for k, v := range p.WSOpts.Headers {
			if strings.EqualFold(k, "Host") {
				n.Host = v
				break
			}
		}
		n.SNI = firstNonEmpty(p.ServerName, p.SNI)
		if p.TLS && p.RealityOpts.PublicKey != "" {
			n.Reality = &RealityConfig{
				PublicKey:   p.RealityOpts.PublicKey,
				ShortID:     p.RealityOpts.ShortID,
				SpiderX:     p.RealityOpts.SpiderX,
				Fingerprint: p.RealityOpts.Fingerprint,
			}
		}
		return n
	case "vmess":
		n := &ProxyNode{
			Name: p.Name, Protocol: "vmess", Address: p.Server, Port: p.Port,
			UserID: p.UUID, AlterIDs: uint16(p.AlterID),
			Insecure: p.SkipVerify || p.Insecure, TLS: p.TLS,
			Network: strings.ToLower(p.Network),
			Path:    p.WSOpts.Path,
		}
		for k, v := range p.WSOpts.Headers {
			if strings.EqualFold(k, "Host") {
				n.Host = v
				break
			}
		}
		n.SNI = firstNonEmpty(p.ServerName, p.SNI)
		if p.Cipher != "" {
			n.Security = p.Cipher
		}
		if n.Network == "" {
			n.Network = "tcp"
		}
		return n
	case "trojan":
		n := &ProxyNode{
			Name: p.Name, Protocol: "trojan", Address: p.Server, Port: p.Port,
			Password: p.Password, Insecure: p.SkipVerify || p.Insecure,
			TLS:     p.TLS || p.RealityOpts.PublicKey == "",
			Network: strings.ToLower(p.Network),
			Path:    p.WSOpts.Path,
		}
		for k, v := range p.WSOpts.Headers {
			if strings.EqualFold(k, "Host") {
				n.Host = v
				break
			}
		}
		n.SNI = firstNonEmpty(p.ServerName, p.SNI)
		if n.Network == "" {
			n.Network = "tcp"
		}
		if p.RealityOpts.PublicKey != "" {
			n.Reality = &RealityConfig{
				PublicKey:   p.RealityOpts.PublicKey,
				ShortID:     p.RealityOpts.ShortID,
				SpiderX:     p.RealityOpts.SpiderX,
				Fingerprint: firstNonEmpty(p.RealityOpts.Fingerprint, p.ClientFingerprint),
			}
		}
		return n
	case "hysteria2", "hy2":
		password := p.Password
		if password == "" {
			password = p.Auth
		}
		return &ProxyNode{
			Name: p.Name, Protocol: "hy2", Address: p.Server, Port: p.Port,
			Password: password, Insecure: p.SkipVerify || p.Insecure,
		}
	case "anytls":
		return &ProxyNode{
			Name: p.Name, Protocol: "anytls", Address: p.Server, Port: p.Port,
			Password: p.Password, Insecure: p.SkipVerify || p.Insecure,
			SNI: firstNonEmpty(p.ServerName, p.SNI),
		}
	default:
		return nil // 暂不支持（trojan/vmess/ssr 等）
	}
}
