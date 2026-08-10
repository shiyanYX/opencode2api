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

// subscriptionManager 负责拉取订阅、解析文本、与自定义/遗留节点合并且写入节点池。
type subscriptionManager struct {
	mu       sync.Mutex
	subs     []SubscriptionConfig
	customs  []ProxyNodeConfig // 面板手填节点
	cacheDir string

	lastUpdated map[string]time.Time
	lastError   map[string]string
	lastCount   map[string]int
}

var subManager = &subscriptionManager{
	lastUpdated: map[string]time.Time{},
	lastError:   map[string]string{},
	lastCount:   map[string]int{},
}

// SubInfo 订阅源运行时快照（面板展示用）。
type SubInfo struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	IntervalHours int    `json:"interval_hours,omitempty"`
	Nodes         int    `json:"nodes"`
	LastUpdatedAt string `json:"last_updated_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
}

// snapshot 返回订阅源当前状态（URL/间隔/节点数/上次拉取/错误）。
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
			nodes, err := m.fetchOnce(ctx, s.URL, cacheDir)
			results <- result{sub: s, nodes: nodes, err: err}
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
	slog.Info("节点池已刷新", "total", len(all), "subscriptions", len(subs), "errors", len(errs))
	return len(all), nil
}

func (s SubscriptionConfig) Key() string { return s.URL }

// fetchOnce 拉取单个订阅；网络失败回退磁盘缓存。
func (m *subscriptionManager) fetchOnce(ctx context.Context, rawURL, cacheDir string) ([]*ProxyNode, error) {
	text, err := m.fetch(ctx, rawURL)
	if err != nil {
		if cached, cerr := readCachedSubscription(cacheDir, rawURL); cerr == nil {
			slog.Warn("订阅拉取失败，使用缓存", "url", rawURL, "error", err)
			return parseSubscriptionText(cached), nil
		}
		return nil, err
	}
	_ = writeCachedSubscription(cacheDir, rawURL, text)
	return parseSubscriptionText(text), nil
}

// fetch 获取订阅原始文本（http/https/data URI）。
func (m *subscriptionManager) fetch(ctx context.Context, rawURL string) (string, error) {
	if strings.HasPrefix(rawURL, "data:") {
		body := rawURL[strings.Index(rawURL, ",")+1:]
		dec, err := base64DecodeLoose(body)
		if err != nil {
			return "", fmt.Errorf("data URI 解码: %w", err)
		}
		return string(dec), nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("不支持的订阅协议 %q", u.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "opencode2api/"+versionString()+" (node subscription)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("订阅 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return "", err
	}
	return string(body), nil
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
			return clash
		}
	}
	// 2) 整文 base64（常见订阅格式）
	if dec, err := base64DecodeLoose(text); err == nil && len(dec) > 20 && strings.Contains(string(dec), "://") {
		if inner := parseLines(string(dec)); len(inner) > 0 {
			return inner
		}
	}
	// 3) 逐行 URI
	if nodes := parseLines(text); len(nodes) > 0 {
		return nodes
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
		if n := parseNodeURI(line); n != nil {
			out = appendNodeUnique(out, n)
		}
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
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`
	Server      string `yaml:"server"`
	Port        int    `yaml:"port"`
	UUID        string `yaml:"uuid"`
	Password    string `yaml:"password"`
	Username    string `yaml:"username"`
	Cipher      string `yaml:"cipher"`
	Method      string `yaml:"method"`
	Network     string `yaml:"network"`
	TLS         bool   `yaml:"tls"`
	ServerName  string `yaml:"servername"`
	SNI         string `yaml:"sni"`
	Flow        string `yaml:"flow"`
	SkipVerify  bool   `yaml:"skip-cert-verify"`
	Insecure    bool   `yaml:"insecure"`
	Auth        string `yaml:"auth"`
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
			Insecure: p.SkipVerify || p.Insecure,
		}
		n.SNI = firstNonEmpty(p.SNI, p.ServerName)
		if p.TLS && p.RealityOpts.PublicKey != "" {
			n.Reality = &RealityConfig{
				PublicKey:   p.RealityOpts.PublicKey,
				ShortID:     p.RealityOpts.ShortID,
				SpiderX:     p.RealityOpts.SpiderX,
				Fingerprint: p.RealityOpts.Fingerprint,
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
			SNI: firstNonEmpty(p.SNI, p.ServerName),
		}
	default:
		return nil // 暂不支持（trojan/vmess/ssr 等）
	}
}
