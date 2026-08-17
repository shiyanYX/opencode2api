package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var httpClient = &http.Client{
	Timeout: 300 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	},
}

var (
	version = "v0.3.7"
	commit  = "none"
	date    = "unknown"
)

func versionString() string {
	return fmt.Sprintf("opencode2api %s (commit=%s, date=%s)", version, commit, date)
}

// ======================== SOCKS5 代理 ========================

type Socks5Proxy struct {
	Addr     string `json:"addr"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Name     string `json:"name,omitempty"`
}

func socks5Dial(proxy Socks5Proxy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, target string) (net.Conn, error) {
		conn, err := net.DialTimeout("tcp", proxy.Addr, 10*time.Second)
		if err != nil {
			return nil, fmt.Errorf("socks5 connect to %s: %w", proxy.Addr, err)
		}
		deadline := time.Now().Add(15 * time.Second)
		conn.SetDeadline(deadline)

		// 认证方法协商
		auth := byte(0x00) // no auth
		if proxy.Username != "" {
			auth = 0x02 // username/password
		}
		if _, err := conn.Write([]byte{0x05, 0x01, auth}); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 handshake write: %w", err)
		}
		buf := make([]byte, 2)
		if _, err := io.ReadFull(conn, buf); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 handshake read: %w", err)
		}
		if buf[0] != 0x05 {
			conn.Close()
			return nil, fmt.Errorf("socks5: not socks5 protocol")
		}

		// 用户名/密码认证
		if buf[1] == 0x02 {
			if proxy.Username == "" {
				conn.Close()
				return nil, fmt.Errorf("socks5: server requires auth but no credentials")
			}
			ulen := len(proxy.Username)
			plen := len(proxy.Password)
			authBuf := make([]byte, 3+ulen+plen)
			authBuf[0] = 0x01
			authBuf[1] = byte(ulen)
			copy(authBuf[2:], proxy.Username)
			authBuf[2+ulen] = byte(plen)
			copy(authBuf[3+ulen:], proxy.Password)
			if _, err := conn.Write(authBuf); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5 auth write: %w", err)
			}
			authResp := make([]byte, 2)
			if _, err := io.ReadFull(conn, authResp); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5 auth read: %w", err)
			}
			if authResp[1] != 0x00 {
				conn.Close()
				return nil, fmt.Errorf("socks5: auth failed")
			}
		} else if buf[1] != 0x00 {
			conn.Close()
			return nil, fmt.Errorf("socks5: unsupported auth method 0x%02x", buf[1])
		}

		// CONNECT 请求
		host, portStr, err := net.SplitHostPort(target)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5: invalid target %s: %w", target, err)
		}
		port := 0
		fmt.Sscanf(portStr, "%d", &port)

		req := []byte{0x05, 0x01, 0x00} // VER, CMD=CONNECT, RSV
		ip := net.ParseIP(host)
		if ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				req = append(req, 0x01) // IPv4
				req = append(req, ip4...)
			} else {
				req = append(req, 0x04) // IPv6
				req = append(req, ip.To16()...)
			}
		} else {
			if len(host) > 255 {
				conn.Close()
				return nil, fmt.Errorf("socks5: hostname too long")
			}
			req = append(req, 0x03) // Domain
			req = append(req, byte(len(host)))
			req = append(req, []byte(host)...)
		}
		req = append(req, byte(port>>8), byte(port))

		if _, err := conn.Write(req); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 connect write: %w", err)
		}

		// 读取响应
		resp := make([]byte, 4)
		if _, err := io.ReadFull(conn, resp); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 connect read: %w", err)
		}
		if resp[1] != 0x00 {
			conn.Close()
			return nil, fmt.Errorf("socks5: connect failed, status 0x%02x", resp[1])
		}

		// 读取绑定地址
		switch resp[3] {
		case 0x01: // IPv4
			if _, err := io.ReadFull(conn, make([]byte, 4+2)); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind ipv4: %w", err)
			}
		case 0x03: // Domain
			dlen := make([]byte, 1)
			if _, err := io.ReadFull(conn, dlen); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind domain len: %w", err)
			}
			if _, err := io.ReadFull(conn, make([]byte, int(dlen[0])+2)); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind domain: %w", err)
			}
		case 0x04: // IPv6
			if _, err := io.ReadFull(conn, make([]byte, 16+2)); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind ipv6: %w", err)
			}
		default:
			conn.Close()
			return nil, fmt.Errorf("socks5: unknown address type 0x%02x", resp[3])
		}

		conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

var (
	socks5Proxies    []Socks5Proxy
	activeSocks5     string // 启用的代理 Addr，空表示直连，__round_robin__ 表示轮询
	socks5PaidDirect bool   // true=带 key/付费直连；false/缺省=全部走代理
	socks5Mu         sync.RWMutex
)

const socks5RR = "__round_robin__"

var socks5RRIndex uint32

// legacySocks5Nodes 把 config.json 里遗留的 socks5Proxies 迁移为池内节点。
// 若旧配置有名字，优先用名字；否则用 Addr。
func legacySocks5Nodes() []*ProxyNode {
	socks5Mu.RLock()
	proxies := append([]Socks5Proxy(nil), socks5Proxies...)
	socks5Mu.RUnlock()
	out := make([]*ProxyNode, 0, len(proxies))
	for _, p := range proxies {
		if p.Addr == "" {
			continue
		}
		host, portStr, err := net.SplitHostPort(p.Addr)
		if err != nil {
			host, portStr = p.Addr, "1080"
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			port = 1080
		}
		name := p.Name
		if name == "" {
			name = p.Addr
		}
		out = append(out, &ProxyNode{
			Name: name, Protocol: "socks5", Address: host, Port: port,
			UserID: p.Username, Password: p.Password,
		})
	}
	return out
}

var (
	socks5Client     *http.Client // 缓存的 SOCKS5 客户端
	socks5ClientAddr string       // 缓存对应的代理地址
)

func getHTTPClient() *http.Client {
	socks5Mu.RLock()
	defer socks5Mu.RUnlock()

	// 新管线：节点池优先（refreshAll 成功后池非空）。池为空时回退旧 socks5 逻辑。
	if nodesActive() {
		n := proxyPool.pick(false)
		if n != nil {
			return proxyPool.getClient(n.Fingerprint)
		}
		return httpClient // 节点都在冷却中 → 直连
	}

	if activeSocks5 == "" {
		return httpClient
	}

	var proxy Socks5Proxy
	var useRR bool

	if activeSocks5 == socks5RR {
		if len(socks5Proxies) == 0 {
			return httpClient
		}
		idx := atomic.AddUint32(&socks5RRIndex, 1) % uint32(len(socks5Proxies))
		proxy = socks5Proxies[idx]
		useRR = true
	} else {
		if socks5Client != nil && socks5ClientAddr == activeSocks5 {
			return socks5Client
		}

		var found bool
		for i := range socks5Proxies {
			if socks5Proxies[i].Addr == activeSocks5 {
				proxy = socks5Proxies[i]
				found = true
				break
			}
		}
		if !found {
			return httpClient
		}
	}

	dial := socks5Dial(proxy)
	client := &http.Client{
		Timeout: 300 * time.Second,
		Transport: &http.Transport{
			DialContext:         dial,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	if !useRR {
		socks5Client = client
		socks5ClientAddr = activeSocks5
	}
	return client
}

// getHTTPClientForTier 按认证层级选择 HTTP 客户端。
// 默认（socks5_paid_direct 未填或 false）：只要配置了 active_socks5，付费/带 key 与 public 都走代理。
// socks5_paid_direct=true 时恢复旧行为：付费层直连，仅免费层走代理。
func getHTTPClientForTier(tier TierType) *http.Client {
	if tier == TierPaid && getSocks5PaidDirect() {
		return httpClient
	}
	return getHTTPClient()
}

func getSocks5PaidDirect() bool {
	socks5Mu.RLock()
	defer socks5Mu.RUnlock()
	return socks5PaidDirect
}

func nodesActive() bool {
	return proxyPool.nodeCount() > 0
}

// getNodeClientForTier 供配额切换链路使用：返回本次请求所用客户端及其节点指纹。
// 指纹为空表示直连（这些响应不参与节点标记）。forceSwitch=true 时强制换节点。
func getNodeClientForTier(tier TierType, forceSwitch bool) (*http.Client, string) {
	if tier == TierPaid && getSocks5PaidDirect() {
		return httpClient, ""
	}
	if nodesActive() {
		n := proxyPool.pick(forceSwitch)
		if n != nil {
			return proxyPool.getClient(n.Fingerprint), n.Fingerprint
		}
		return httpClient, ""
	}
	return getHTTPClientForTier(tier), ""
}

// ======================== 随机 ID ========================

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = letters[b[i]%byte(len(letters))]
	}
	return string(b)
}

func randomHex(n int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = hex[b[i]%byte(len(hex))]
	}
	return string(b)
}

// ======================== OpenCode 会话 ========================

var (
	ocSessionID  string
	ocProjectID  string
	ocClientVer  string
	ocInitMu     sync.Mutex
	ocInitDone   bool
	requestCount atomic.Int64
)

func fetchOCVersion() string {
	req, _ := http.NewRequest("GET", "https://registry.npmjs.org/opencode-ai/latest", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := getHTTPClient().Do(req)
	if err != nil {
		return "1.15.3"
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var info struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &info) == nil && info.Version != "" {
		return info.Version
	}
	return "1.15.3"
}

// fetchOCVersionDirect 绕过节点池直连探测版本（会话切换时用，避免占用配额路径）。
func fetchOCVersionDirect() string {
	client := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequest("GET", "https://registry.npmjs.org/opencode-ai/latest", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "1.15.3"
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var info struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &info) == nil && info.Version != "" {
		return info.Version
	}
	return "1.15.3"
}

// noSessionRefresh 测试桩：置真时 refreshOCSession 不做网络请求。
var noSessionRefresh bool

func initOCSession() {
	ocInitMu.Lock()
	defer ocInitMu.Unlock()
	if ocInitDone {
		return
	}
	ocClientVer = fetchOCVersion()
	ocSessionID = "ses_" + randomString(24)
	ocProjectID = randomHex(40)
	slog.Info("opencode version", "version", ocClientVer)
	slog.Info("session initialized", "session_id", ocSessionID)
	slog.Info("project initialized", "project_id", ocProjectID)
	ocInitDone = true
}

func refreshOCSession() {
	ocInitMu.Lock()
	defer ocInitMu.Unlock()
	if noSessionRefresh { // 测试桩：只本地轮换，不访问网络
		ocSessionID = "ses_" + randomString(24)
		ocProjectID = randomHex(40)
		ocInitDone = false
		return
	}
	ocClientVer = fetchOCVersionDirect()
	ocSessionID = "ses_" + randomString(24)
	ocProjectID = randomHex(40)
	slog.Info("session refreshed", "version", ocClientVer, "session_id", ocSessionID)
	// 恢复未初始化状态，后续 initOCSession 重新初始化
	ocInitDone = false
}

// ======================== 模型 ========================

type ModelInfo struct {
	ID              string   `json:"id"`
	Object          string   `json:"object"`
	Created         int64    `json:"created"`
	OwnedBy         string   `json:"owned_by"`
	ContextWindow   *int64   `json:"context_window,omitempty"`
	MaxOutputTokens *int64   `json:"max_output_tokens,omitempty"`
	InputModalities []string `json:"input_modalities,omitempty"`
}

type ModelLimit struct {
	Context         int64
	Output          int64
	InputModalities []string
}

var (
	modelsCache    []ModelInfo
	goModelsCache  []ModelInfo
	modelMu        sync.RWMutex
	modelsLoaded   bool
	modelsDevMu    sync.RWMutex
	modelsDevCache map[string]ModelLimit
)

func fetchModels() ([]ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://opencode.ai/zen/v1/models", nil)
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("x-opencode-session", ocSessionID)
	resp, err := getHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	var models []ModelInfo
	now := time.Now().Unix()
	for _, m := range result.Data {
		models = append(models, ModelInfo{ID: m.ID, Object: "model", Created: now, OwnedBy: "opencode"})
	}
	return models, nil
}

func fetchGoModels() ([]ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://opencode.ai/zen/go/v1/models", nil)
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("x-opencode-session", ocSessionID)
	resp, err := getHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	var models []ModelInfo
	now := time.Now().Unix()
	for _, m := range result.Data {
		models = append(models, ModelInfo{ID: m.ID, Object: "model", Created: now, OwnedBy: "opencode"})
	}
	return models, nil
}

// fetchModelsDevCatalog 抓取 models.dev 模型目录，构建模型 ID → 上下文/输出上限索引。
// opencode 上游模型的 limit 元数据（context/output）仅在 models.dev 发布，上游 models 接口不提供。
func fetchModelsDevCatalog() (map[string]ModelLimit, error) {
	req, _ := http.NewRequest("GET", "https://models.dev/api.json", nil)
	resp, err := getHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]struct {
		Models map[string]struct {
			Modalities struct {
				Input []string `json:"input"`
			} `json:"modalities"`
			Limit struct {
				Context int64 `json:"context"`
				Output  int64 `json:"output"`
			} `json:"limit"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	out := make(map[string]ModelLimit)
	for _, provider := range result {
		for id, m := range provider.Models {
			if m.Limit.Context > 0 && m.Limit.Output > 0 {
				out[id] = ModelLimit{Context: m.Limit.Context, Output: m.Limit.Output, InputModalities: m.Modalities.Input}
			}
		}
	}
	return out, nil
}

func lookupModelLimit(modelID string) (ModelLimit, bool) {
	modelsDevMu.RLock()
	defer modelsDevMu.RUnlock()
	limit, ok := modelsDevCache[modelID]
	return limit, ok
}

func refreshModelsDevCatalog(logOK bool) {
	var cat map[string]ModelLimit
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		cat, lastErr = fetchModelsDevCatalog()
		if lastErr == nil {
			break
		}
		slog.Warn("models.dev catalog fetch failed", "error", lastErr, "attempt", attempt+1)
		time.Sleep(2 * time.Second)
	}
	if lastErr != nil {
		slog.Error("models.dev catalog refresh failed", "error", lastErr)
		return
	}
	modelsDevMu.Lock()
	modelsDevCache = cat
	modelsDevMu.Unlock()
	if logOK {
		slog.Info("models.dev catalog loaded", "count", len(cat))
	}
}

func containsModelWithID(models []ModelInfo, modelID string) bool {
	for _, model := range models {
		if model.ID == modelID {
			return true
		}
	}
	return false
}

func isModelInGoCatalog(modelID string) bool {
	modelMu.RLock()
	defer modelMu.RUnlock()
	return containsModelWithID(goModelsCache, modelID)
}

func isGoCatalogOnlyModel(modelID string) bool {
	modelMu.RLock()
	defer modelMu.RUnlock()
	return containsModelWithID(goModelsCache, modelID) && !containsModelWithID(modelsCache, modelID)
}

func getModelIDs() []string {
	modelMu.RLock()
	defer modelMu.RUnlock()
	ids := make([]string, len(modelsCache))
	for i, m := range modelsCache {
		ids[i] = m.ID
	}
	return ids
}

func getGoModelIDs() []string {
	modelMu.RLock()
	defer modelMu.RUnlock()
	ids := make([]string, len(goModelsCache))
	for i, m := range goModelsCache {
		ids[i] = m.ID
	}
	return ids
}

// isNonRetryableUpstreamError reports billing/credits failures that must not
// trigger retries.
func isNonRetryableUpstreamError(status int, body []byte) bool {
	if status != http.StatusUnauthorized && status != http.StatusPaymentRequired && status != http.StatusForbidden {
		return false
	}
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	errType := strings.ToLower(strings.TrimSpace(payload.Error.Type))
	if errType == "" {
		errType = strings.ToLower(strings.TrimSpace(payload.Type))
	}
	if errType == "creditserror" || errType == "insufficient_quota" || errType == "billing_error" {
		return true
	}
	msg := strings.ToLower(payload.Error.Message)
	return strings.Contains(msg, "insufficient balance") || strings.Contains(msg, "insufficient credits")
}

// startModelRefresh 定时刷新模型列表（每 10 分钟）
func startModelRefresh() {
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			fetched, err := fetchModels()
			if err == nil && len(fetched) > 0 {
				modelMu.Lock()
				modelsCache = fetched
				modelsLoaded = true
				modelMu.Unlock()
				slog.Info("models auto-refreshed", "count", len(fetched))
			} else if err != nil {
				slog.Error("free models refresh failed", "error", err)
			}

			goFetched, goErr := fetchGoModels()
			if goErr == nil && len(goFetched) > 0 {
				modelMu.Lock()
				goModelsCache = goFetched
				modelMu.Unlock()
				slog.Info("go catalog auto-refreshed", "count", len(goFetched))
			} else if goErr != nil {
				slog.Error("go catalog refresh failed", "error", goErr)
			}
		}
	}()
}

// startModelsDevRefresh 启动时异步加载一次 models.dev 目录（上下文/输出上限元数据），不阻塞监听
func startModelsDevRefresh() {
	go func() {
		refreshModelsDevCatalog(true)
	}()
}

// ======================== 结构化日志 ========================

type contextKey string

const reqIDKey contextKey = "request_id"

func getReqID(ctx context.Context) string {
	if id, ok := ctx.Value(reqIDKey).(string); ok {
		return id
	}
	return ""
}

// ======================== 配置 ========================

var (
	port                 string
	configPath           = "config.json"
	modelAlias           = map[string]string{}
	reasoningEffortMap   = map[string]string{}
	forceDisableThinking bool
	apiKey               string // 统一网关密钥（config.api_key），空 = 不启用
	debugMode            bool
	configMu             sync.RWMutex
	storedResponses      = map[string]StoredResponseState{}
	storedResponsesMu    sync.RWMutex
)

// ======================== 管理面板认证 ========================

var (
	adminPassword string
	sessions      = map[string]struct{}{}
	sessionsMu    sync.Mutex
)

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if adminPassword == "" {
			next(w, r)
			return
		}
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value == "" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		sessionsMu.Lock()
		_, ok := sessions[cookie.Value]
		sessionsMu.Unlock()
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if adminPassword == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			renderLoginPage(w, "表单解析失败")
			return
		}
		if r.FormValue("password") != adminPassword {
			renderLoginPage(w, "密码错误")
			return
		}
		token, err := generateToken()
		if err != nil {
			renderLoginPage(w, "创建会话失败")
			return
		}
		sessionsMu.Lock()
		sessions[token] = struct{}{}
		sessionsMu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "session", Value: token, Path: "/", HttpOnly: true})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	renderLoginPage(w, "")
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	cookie, err := r.Cookie("session")
	if err == nil && cookie.Value != "" {
		sessionsMu.Lock()
		delete(sessions, cookie.Value)
		sessionsMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ======================== Token 统计 ========================

type ModelStats struct {
	RequestCount     int64 `json:"request_count"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type TokenStatsData struct {
	TotalRequests int64                  `json:"total_requests"`
	Models        map[string]*ModelStats `json:"models"`
}

var (
	tokenStats     = &TokenStatsData{Models: map[string]*ModelStats{}}
	tokenStatsMu   sync.Mutex
	tokenStatsPath = "stats.json"
)

// ======================== 数据模型 ========================

type OpenAIRequest struct {
	Model           string         `json:"model"`
	Messages        []Message      `json:"messages"`
	Stream          bool           `json:"stream"`
	Temperature     *float64       `json:"temperature,omitempty"`
	MaxTokens       *int           `json:"max_tokens,omitempty"`
	TopP            *float64       `json:"top_p,omitempty"`
	Thinking        any            `json:"thinking,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	ExtraBody       map[string]any `json:"extra_body,omitempty"`
	Tools           []Tool         `json:"tools,omitempty"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
}

type Message struct {
	Role             string     `json:"role,omitempty"`
	Content          any        `json:"content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type AppConfig struct {
	ModelAlias           map[string]string `json:"model_alias"`
	ReasoningEffortMap   map[string]string `json:"reasoning_effort_map"`
	ForceDisableThinking bool              `json:"force_disable_thinking"`
	// ApiKey 统一网关密钥：客户端用它通过鉴权并按付费档获取全量模型；
	// 留空时退回现状（任意有效 sk- key 或免密钥免费档）。
	ApiKey        string        `json:"api_key,omitempty"`
	Socks5Proxies []Socks5Proxy `json:"socks5_proxies,omitempty"`
	ActiveSocks5  string        `json:"active_socks5,omitempty"`
	// Socks5PaidDirect controls whether keyed/paid upstream calls bypass SOCKS5.
	// Omitted or false (default): all traffic uses the active proxy.
	// true: paid/keyed traffic goes direct; only public/free uses SOCKS5.
	Socks5PaidDirect bool `json:"socks5_paid_direct,omitempty"`

	// ---- 节点池（订阅）----
	Subscriptions []SubscriptionConfig `json:"subscriptions,omitempty"`
	// Webshare 通过 webshare.io API v2 拉取的 SOCKS5 代理池（并入节点池）。
	Webshare    []WebshareConfig  `json:"webshare,omitempty"`
	ManualNodes []ProxyNodeConfig `json:"manual_nodes,omitempty"`
	// QuotaErrorSignals 定义"免费额度耗尽"的判定签名（error.type 或 error.message 关键词）。
	// 命中后自动切换节点重试（免费层 K=5 次）。
	QuotaErrorSignals QuotaSignalsConfig `json:"quota_error_signals,omitempty"`
	// 节点冷却时长（0 = 默认 1h / 60s）
	NodeCooldownExhaustedHours int `json:"node_cooldown_exhausted_hours,omitempty"`
	NodeCooldownDeadMinutes    int `json:"node_cooldown_dead_minutes,omitempty"`
	// MaxQuotaNodeSwitches 配额耗尽后最多尝试的节点数（默认 5）。
	MaxQuotaNodeSwitches int `json:"max_quota_node_switches,omitempty"`

	// ---- 节点健康检查（可选）----
	// 0 = 默认 15min；URL 空 = 默认 https://www.gstatic.com/generate_204
	NodeHealthIntervalMinutes int    `json:"node_health_interval_minutes,omitempty"`
	NodeHealthProbeURL        string `json:"node_health_probe_url,omitempty"`
}

// QuotaSignalsConfig 配额耗尽判定签名（纯配置，不记运行时状态）。
type QuotaSignalsConfig struct {
	ErrorTypes      []string `json:"error_types,omitempty"`
	MessageKeywords []string `json:"message_keywords,omitempty"`
}

func defaultQuotaErrorTypes() []string {
	return []string{"FreeUsageLimitError", "insufficient_quota", "credits_error", "billing_error"}
}

func defaultQuotaMessageKeywords() []string {
	return []string{"free usage limit", "quota", "insufficient", "limit exceeded"}
}

// ======================== Claude Messages API 类型 ========================

type ClaudeRequest struct {
	Model             string          `json:"model"`
	Messages          []ClaudeMessage `json:"messages"`
	System            any             `json:"system,omitempty"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	TopK              *int            `json:"top_k,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Tools             []ClaudeTool    `json:"tools,omitempty"`
	ToolChoice        any             `json:"tool_choice,omitempty"`
	StopSequences     []string        `json:"stop_sequences,omitempty"`
	Metadata          any             `json:"metadata,omitempty"`
	Thinking          any             `json:"thinking,omitempty"`
	OutputConfig      any             `json:"output_config,omitempty"`
	ContextManagement any             `json:"context_management,omitempty"`
}

type ClaudeMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type ClaudeContent struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
}

type ClaudeTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"`
	Type        string `json:"type,omitempty"`
}

type ClaudeResponse struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Content    []ClaudeContent `json:"content"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Usage      ClaudeUsage     `json:"usage,omitempty"`
}

type ClaudeUsage map[string]any

// ======================== Responses API 类型 ========================

type ResponsesAPIRequest struct {
	Model              string          `json:"model"`
	Input              any             `json:"input"`
	Messages           []Message       `json:"messages,omitempty"`
	Instructions       string          `json:"instructions,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	MaxTokens          *int            `json:"max_output_tokens,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	FrequencyPenalty   *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty    *float64        `json:"presence_penalty,omitempty"`
	Reasoning          ReasonEffort    `json:"reasoning,omitempty"`
	Include            []string        `json:"include,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	Tools              []ResponsesTool `json:"tools,omitempty"`
	ToolChoice         any             `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	Stop               any             `json:"stop,omitempty"`
	User               string          `json:"user,omitempty"`
	StreamOptions      any             `json:"stream_options,omitempty"`
	Metadata           any             `json:"metadata,omitempty"`
}

type ResponsesTool struct {
	Type            string         `json:"type"`
	Name            string         `json:"name,omitempty"`
	Description     string         `json:"description,omitempty"`
	Parameters      map[string]any `json:"parameters,omitempty"`
	Function        *ToolFunction  `json:"function,omitempty"`
	ServerLabel     string         `json:"server_label,omitempty"`
	ServerURL       string         `json:"server_url,omitempty"`
	ConnectorID     string         `json:"connector_id,omitempty"`
	Authorization   string         `json:"authorization,omitempty"`
	AllowedTools    []string       `json:"allowed_tools,omitempty"`
	RequireApproval any            `json:"require_approval,omitempty"`
}

type ReasonEffort struct {
	Effort string `json:"effort,omitempty"`
}

type StoredResponseState struct {
	Model        string          `json:"model"`
	Instructions string          `json:"instructions,omitempty"`
	Tools        []ResponsesTool `json:"tools,omitempty"`
	ToolChoice   any             `json:"tool_choice,omitempty"`
	Output       []any           `json:"output,omitempty"`
}

// ======================== 配置管理 ========================

func loadConfig(path string) AppConfig {
	var cfg AppConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Warn("config parse failed", "error", err)
	}
	return cfg
}

func saveConfig(path string, cfg AppConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func applyConfig(cfg AppConfig) {
	configMu.Lock()
	defer configMu.Unlock()
	if cfg.ModelAlias != nil {
		modelAlias = cfg.ModelAlias
	}
	if cfg.ReasoningEffortMap != nil {
		reasoningEffortMap = cfg.ReasoningEffortMap
	}
	forceDisableThinking = cfg.ForceDisableThinking
	apiKey = cfg.ApiKey

	socks5Mu.Lock()
	if cfg.Socks5Proxies != nil {
		socks5Proxies = cfg.Socks5Proxies
	}
	if activeSocks5 != cfg.ActiveSocks5 {
		activeSocks5 = cfg.ActiveSocks5
		socks5Client = nil
		socks5ClientAddr = ""
		atomic.StoreUint32(&socks5RRIndex, 0)
	}
	socks5PaidDirect = cfg.Socks5PaidDirect
	socks5Mu.Unlock()

	// ---- 节点池配置 ----
	if cfg.NodeCooldownExhaustedHours > 0 {
		proxyPool.exhaustedCooldown = time.Duration(cfg.NodeCooldownExhaustedHours) * time.Hour
	} else {
		proxyPool.exhaustedCooldown = 0 // 回默认 1h
	}
	if cfg.NodeCooldownDeadMinutes > 0 {
		proxyPool.deadCooldown = time.Duration(cfg.NodeCooldownDeadMinutes) * time.Minute
	} else {
		proxyPool.deadCooldown = 0 // 回默认 60s
	}
	quotaSignalsMu.Lock()
	quotaErrorTypes = defaultQuotaErrorTypes()
	quotaMessageKeywords = defaultQuotaMessageKeywords()
	// 显式空数组（面板保存时文本框为空）同样回退默认，避免静默禁用配额识别。
	if len(cfg.QuotaErrorSignals.ErrorTypes) > 0 {
		quotaErrorTypes = cfg.QuotaErrorSignals.ErrorTypes
	}
	if len(cfg.QuotaErrorSignals.MessageKeywords) > 0 {
		quotaMessageKeywords = cfg.QuotaErrorSignals.MessageKeywords
	}
	maxQuotaNodeSwitches = cfg.MaxQuotaNodeSwitches
	if maxQuotaNodeSwitches <= 0 {
		maxQuotaNodeSwitches = defaultMaxQuotaNodeSwitches
	}
	quotaSignalsMu.Unlock()

	// ---- 健康检查配置 ----
	if cfg.NodeHealthIntervalMinutes > 0 {
		proxyPool.probeInterval = time.Duration(cfg.NodeHealthIntervalMinutes) * time.Minute
	} else {
		proxyPool.probeInterval = 0
	}
	proxyPool.probeURL = cfg.NodeHealthProbeURL
}

var (
	quotaSignalsMu       sync.Mutex
	quotaErrorTypes      []string
	quotaMessageKeywords []string
	maxQuotaNodeSwitches int
)

const defaultMaxQuotaNodeSwitches = 5

func resolveModel(model string) string {
	m := strings.TrimSpace(model)
	configMu.RLock()
	alias, ok := modelAlias[m]
	configMu.RUnlock()
	if ok {
		return alias
	}
	// Clients see free models without the "-free" suffix from /v1/models.
	// Map the display name back to the upstream free ID when that is the only match.
	if m != "" && !isFreeModel(m) {
		freeID := m + "-free"
		if !modelExistsInCaches(m) && modelExistsInCaches(freeID) {
			return freeID
		}
	}
	return m
}

func getForceDisableThinking() bool {
	configMu.RLock()
	defer configMu.RUnlock()
	return forceDisableThinking
}

func getReasoningEffortMap() map[string]string {
	configMu.RLock()
	defer configMu.RUnlock()
	cp := make(map[string]string, len(reasoningEffortMap))
	for k, v := range reasoningEffortMap {
		cp[k] = v
	}
	return cp
}

// ======================== Token 统计 ========================

func loadTokenStats() {
	data, err := os.ReadFile(tokenStatsPath)
	if err != nil {
		return
	}
	var st TokenStatsData
	if err := json.Unmarshal(data, &st); err != nil {
		return
	}
	tokenStatsMu.Lock()
	if st.Models == nil {
		st.Models = map[string]*ModelStats{}
	}
	tokenStats = &st
	tokenStatsMu.Unlock()
}

func saveTokenStats() {
	tokenStatsMu.Lock()
	data, err := json.MarshalIndent(tokenStats, "", "  ")
	tokenStatsMu.Unlock()
	if err != nil {
		return
	}
	os.WriteFile(tokenStatsPath, data, 0644)
}

func recordTokenUsage(model string, promptTokens, completionTokens, totalTokens int64) {
	tokenStatsMu.Lock()
	tokenStats.TotalRequests++
	ms, ok := tokenStats.Models[model]
	if !ok {
		ms = &ModelStats{}
		tokenStats.Models[model] = ms
	}
	ms.RequestCount++
	ms.PromptTokens += promptTokens
	ms.CompletionTokens += completionTokens
	ms.TotalTokens += totalTokens
	tokenStatsMu.Unlock()
	go saveTokenStats()
}

// ======================== Thinking/Reasoning 判断 ========================

func isThinkingEnabled(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		t, _ := v["type"].(string)
		// Claude Code sends adaptive thinking with --effort / CLAUDE_CODE_EFFORT_LEVEL.
		return t == "enabled" || t == "adaptive"
	case bool:
		return v
	default:
		return false
	}
}

// effortFromOutputConfig reads Claude Code's output_config.effort
// (set by --effort / CLAUDE_CODE_EFFORT_LEVEL).
func effortFromOutputConfig(value any) string {
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	effort, _ := m["effort"].(string)
	return strings.TrimSpace(effort)
}

func isThinkingDisabled(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		t, _ := v["type"].(string)
		return t == "disabled"
	case bool:
		return !v
	default:
		return false
	}
}

// buildUpstreamThinking preserves budget_tokens / effort fields when present.
func buildUpstreamThinking(value any) map[string]any {
	out := map[string]any{"type": "enabled"}
	m, ok := value.(map[string]any)
	if !ok {
		return out
	}
	for _, key := range []string{"budget_tokens", "effort"} {
		if v, exists := m[key]; exists && v != nil {
			out[key] = v
		}
	}
	return out
}

// reasoningEffortFromThinking maps Anthropic-style budget_tokens onto an
// OpenAI-compatible reasoning_effort when the client did not set one explicitly.
func reasoningEffortFromThinking(value any) string {
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if effort, ok := m["effort"].(string); ok && effort != "" {
		return effort
	}
	var budget float64
	switch v := m["budget_tokens"].(type) {
	case float64:
		budget = v
	case int:
		budget = float64(v)
	case int64:
		budget = float64(v)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return ""
		}
		budget = f
	default:
		return ""
	}
	switch {
	case budget <= 0:
		return ""
	case budget < 2048:
		return "low"
	case budget < 8192:
		return "medium"
	case budget < 16384:
		return "high"
	default:
		return "xhigh"
	}
}

func wantsReasoning(req *OpenAIRequest) bool {
	if getForceDisableThinking() {
		return false
	}
	if isThinkingDisabled(req.Thinking) {
		return false
	}
	if isThinkingEnabled(req.Thinking) {
		return true
	}
	if req.ExtraBody != nil {
		if isThinkingDisabled(req.ExtraBody["thinking"]) {
			return false
		}
		if isThinkingEnabled(req.ExtraBody["thinking"]) {
			return true
		}
	}
	return true
}

// ======================== 消息处理 ========================
// normalizeContent 是 dumb pipe 透传：保留 string 与 []any 两种入参形状
// （其它非常规类型走 json.Marshal 兜底），不解析或过滤任何 multimodal part。
// 能力协商由 opencode 客户端 + 上游负责；这里既不"硬降级"也不"补全"。
func normalizeContent(content any) any {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		return s
	}
	if arr, ok := content.([]any); ok {
		return arr
	}
	b, err := json.Marshal(content)
	if err != nil {
		return nil
	}
	return string(b)
}

func fixToolCallGaps(messages []Message) []Message {
	toolResponses := map[string]*Message{}
	for i := range messages {
		if messages[i].Role == "tool" && messages[i].ToolCallID != "" {
			toolResponses[messages[i].ToolCallID] = &messages[i]
		}
	}
	fixed := make([]Message, 0, len(messages)+len(messages)/4)
	emitted := map[string]bool{}
	for _, msg := range messages {
		if msg.Role == "tool" && msg.ToolCallID != "" {
			if emitted[msg.ToolCallID] {
				continue
			}
		}
		fixed = append(fixed, msg)
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				if resp, found := toolResponses[tc.ID]; found {
					fixed = append(fixed, *resp)
				} else {
					fixed = append(fixed, Message{Role: "tool", ToolCallID: tc.ID, Content: "Tool call result not available"})
				}
				emitted[tc.ID] = true
			}
		}
	}
	return fixed
}

func ensureReasoningContent(messages []Message, thinking bool) []Message {
	if !thinking {
		return messages
	}
	for i := range messages {
		if messages[i].Role == "assistant" && messages[i].ReasoningContent == nil {
			empty := ""
			messages[i].ReasoningContent = &empty
		}
	}
	return messages
}

func convertMessagesForUpstream(messages []Message) []map[string]any {
	converted := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		clean := map[string]any{}
		if msg.Role != "" {
			clean["role"] = msg.Role
		}
		content := normalizeContent(msg.Content)
		reasoningContent := msg.ReasoningContent
		if content != nil {
			clean["content"] = content
		}
		if reasoningContent != nil {
			clean["reasoning_content"] = *reasoningContent
		}
		if len(msg.ToolCalls) > 0 {
			clean["tool_calls"] = msg.ToolCalls
		}
		if msg.ToolCallID != "" {
			clean["tool_call_id"] = msg.ToolCallID
		}
		if msg.Name != "" {
			clean["name"] = msg.Name
		}
		converted = append(converted, clean)
	}
	return converted
}

// ======================== 完整请求转换（含 thinking/reasoning_effort/ExtraBody） ========================

func convertRequest(req *OpenAIRequest) map[string]any {
	converted := map[string]any{
		"model":    req.Model,
		"messages": convertMessagesForUpstream(req.Messages),
		"stream":   req.Stream,
	}
	if req.Temperature != nil {
		converted["temperature"] = *req.Temperature
	}
	if req.MaxTokens != nil {
		converted["max_tokens"] = *req.MaxTokens
	}
	if req.TopP != nil {
		converted["top_p"] = *req.TopP
	}
	if len(req.Tools) > 0 {
		converted["tools"] = req.Tools
	}
	if req.ToolChoice != nil {
		converted["tool_choice"] = req.ToolChoice
	}
	// 处理思维模式 — 仅当用户显式指定时才发送，避免 MiniMax 等模型报错
	if getForceDisableThinking() || isThinkingDisabled(req.Thinking) {
		converted["thinking"] = map[string]string{"type": "disabled"}
	} else if req.Thinking != nil && isThinkingEnabled(req.Thinking) {
		converted["thinking"] = buildUpstreamThinking(req.Thinking)
	} else if req.ExtraBody != nil {
		if isThinkingDisabled(req.ExtraBody["thinking"]) {
			converted["thinking"] = map[string]string{"type": "disabled"}
		} else if isThinkingEnabled(req.ExtraBody["thinking"]) {
			converted["thinking"] = buildUpstreamThinking(req.ExtraBody["thinking"])
		}
	}
	// 处理 reasoning_effort（含从 thinking.budget_tokens 推导）
	effort := req.ReasoningEffort
	if effort == "" && !isThinkingDisabled(req.Thinking) {
		effort = reasoningEffortFromThinking(req.Thinking)
	}
	if !getForceDisableThinking() && effort != "" {
		effortMap := getReasoningEffortMap()
		if mapped, ok := effortMap[effort]; ok {
			converted["reasoning_effort"] = mapped
		} else {
			converted["reasoning_effort"] = effort
		}
	}
	// 合并 ExtraBody
	if req.ExtraBody != nil {
		for k, v := range req.ExtraBody {
			if _, exists := converted[k]; !exists {
				converted[k] = v
			}
		}
	}
	return converted
}

func buildUpstreamBody(req *OpenAIRequest) []byte {
	converted := convertRequest(req)
	b, err := json.Marshal(converted)
	if err != nil {
		slog.Error("marshal upstream body failed", "error", err)
	}
	return b
}

// ======================== Anthropic 格式兼容 ========================

func isAnthropicFormat(body []byte) bool {
	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil {
		if typ, _ := obj["type"].(string); typ == "message" {
			return true
		}
	}
	lines := bytes.Split(body, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		typ, _ := event["type"].(string)
		switch typ {
		case "message_start", "content_block_start", "content_block_delta",
			"content_block_stop", "message_delta", "message_stop", "ping":
			return true
		}
		return false
	}
	return false
}

func parseAnthropicSSE(body []byte) (map[string]any, string, []map[string]any) {
	lines := bytes.Split(body, []byte("\n"))
	var anthropicMsg map[string]any
	var textBuilder, currentToolInputBuilder strings.Builder
	var currentToolUse map[string]any
	var toolUseBlocks []map[string]any
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		typ, _ := event["type"].(string)
		switch typ {
		case "message_start":
			if m, ok := event["message"].(map[string]any); ok {
				anthropicMsg = m
			}
		case "content_block_start":
			if cb, ok := event["content_block"].(map[string]any); ok {
				if cbType, _ := cb["type"].(string); cbType == "tool_use" {
					currentToolUse = cb
					currentToolInputBuilder.Reset()
				}
			}
		case "content_block_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				if t, ok := delta["text"].(string); ok {
					textBuilder.WriteString(t)
				}
				if dt, _ := delta["type"].(string); dt == "input_json_delta" {
					if partial, ok := delta["partial_json"].(string); ok {
						currentToolInputBuilder.WriteString(partial)
					}
				}
			}
		case "content_block_stop":
			if currentToolUse != nil {
				inputStr := currentToolInputBuilder.String()
				var input any = inputStr
				var parsed any
				if json.Unmarshal([]byte(inputStr), &parsed) == nil {
					input = parsed
				}
				currentToolUse["input"] = input
				toolUseBlocks = append(toolUseBlocks, currentToolUse)
				currentToolUse = nil
			}
		case "message_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				if anthropicMsg == nil {
					anthropicMsg = map[string]any{}
				}
				if stop, ok := delta["stop_reason"].(string); ok {
					anthropicMsg["stop_reason"] = stop
				}
				if usage, ok := delta["usage"].(map[string]any); ok {
					anthropicMsg["usage"] = usage
				}
			}
		case "message_stop":
		case "error":
			return nil, "", nil
		}
	}
	return anthropicMsg, textBuilder.String(), toolUseBlocks
}

func buildOpenAIResponse(anthropicMsg map[string]any, text string, toolUseBlocks []map[string]any, modelID string) []byte {
	if anthropicMsg == nil {
		return nil
	}
	now := time.Now().Unix()
	role, _ := anthropicMsg["role"].(string)
	if role == "" {
		role = "assistant"
	}
	finishReason, _ := anthropicMsg["stop_reason"].(string)
	finishReason = normalizeFinishReason(finishReason)
	choice := map[string]any{
		"index":         0,
		"message":       map[string]any{"role": role, "content": text},
		"finish_reason": finishReason,
	}
	if len(toolUseBlocks) > 0 {
		var toolCalls []map[string]any
		for _, tb := range toolUseBlocks {
			toolInput := tb["input"]
			argsJSON, _ := json.Marshal(toolInput)
			toolCalls = append(toolCalls, map[string]any{
				"id":   tb["id"],
				"type": "function",
				"function": map[string]any{
					"name":      tb["name"],
					"arguments": string(argsJSON),
				},
			})
		}
		choice["message"].(map[string]any)["tool_calls"] = toolCalls
		if text == "" {
			choice["message"].(map[string]any)["content"] = nil
		}
	}
	resp := map[string]any{
		"id":      anthropicMsg["id"],
		"object":  "chat.completion",
		"created": now,
		"model":   modelID,
		"choices": []map[string]any{choice},
	}
	if usage, ok := anthropicMsg["usage"].(map[string]any); ok {
		resp["usage"] = anthropicUsageToChat(usage)
	}
	result, _ := json.Marshal(resp)
	return result
}

func convertAnthropicMessageToOpenAI(msg map[string]any, modelID string) []byte {
	if msg["model"] == nil {
		msg["model"] = modelID
	}
	var textBuilder strings.Builder
	var toolUses []map[string]any
	if content, ok := msg["content"].([]any); ok {
		for _, c := range content {
			if block, ok := c.(map[string]any); ok {
				switch block["type"] {
				case "text":
					if t, ok := block["text"].(string); ok {
						textBuilder.WriteString(t)
					}
				case "tool_use":
					toolUses = append(toolUses, block)
				}
			}
		}
	}
	return buildOpenAIResponse(msg, textBuilder.String(), toolUses, modelID)
}

func convertAnthropicToOpenAI(body []byte, modelID string) []byte {
	var singleMsg map[string]any
	if json.Unmarshal(body, &singleMsg) == nil {
		if typ, _ := singleMsg["type"].(string); typ == "message" {
			return convertAnthropicMessageToOpenAI(singleMsg, modelID)
		}
	}
	msg, text, toolUses := parseAnthropicSSE(body)
	if msg == nil {
		return body
	}
	if msg["model"] == nil {
		msg["model"] = modelID
	}
	return buildOpenAIResponse(msg, text, toolUses, modelID)
}

// ======================== 响应清理 ========================

func cleanNulls(m map[string]any) {
	for k, v := range m {
		if v == nil {
			delete(m, k)
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			delete(m, k)
		}
	}
}

// promoteMisplacedReasoning moves reasoning_content into content when upstream
// put the visible answer in reasoning_content (opencode-go #37635). Only runs
// when content is empty and the chunk has no tool_calls, so genuine CoT that
// precedes tool calls is left alone when keepReasoning is true.
func promoteMisplacedReasoning(fields map[string]any, keepReasoning bool) bool {
	rc, _ := fields["reasoning_content"].(string)
	if rc == "" {
		return false
	}
	if raw, ok := fields["tool_calls"]; ok && raw != nil {
		if arr, ok := raw.([]any); ok && len(arr) > 0 {
			return false
		}
	}
	content, _ := fields["content"].(string)
	if content != "" {
		return false
	}
	if keepReasoning {
		// Preserve CoT for thinking blocks / clients that read reasoning_content.
		return false
	}
	fields["content"] = rc
	delete(fields, "reasoning_content")
	return true
}

func cleanStreamDelta(delta map[string]any, keepReasoning bool) {
	_ = promoteMisplacedReasoning(delta, keepReasoning)
	if v, ok := delta["content"]; ok && v == nil {
		delete(delta, "content")
	}
	if s, ok := delta["content"].(string); ok && s == "" {
		delete(delta, "content")
	}
	if !keepReasoning {
		delete(delta, "reasoning_content")
	} else {
		if v, ok := delta["reasoning_content"]; ok && v == nil {
			delete(delta, "reasoning_content")
		}
		if s, ok := delta["reasoning_content"].(string); ok && s == "" {
			delete(delta, "reasoning_content")
		}
	}
	if s, ok := delta["role"].(string); ok && s == "" {
		delete(delta, "role")
	}
}

// convertStreamChunkWithUsage 转换流式 chunk 并同时提取 usage，避免二次解析
func convertStreamChunkWithUsage(line string, keepReasoning bool) (string, map[string]any) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
		return line, nil
	}
	if !strings.HasPrefix(line, "data: ") {
		return line, nil
	}
	data := line[6:]
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return line, nil
	}

	// 提取 usage
	var usage map[string]any
	if u, ok := raw["usage"].(map[string]any); ok {
		usage = u
	}

	choices, ok := raw["choices"].([]any)
	if !ok || len(choices) == 0 {
		// Chat Completions deliberately uses an empty choices array for the
		// terminal usage chunk. It is part of the client-visible stream.
		delete(raw, "cost")
		converted, err := json.Marshal(raw)
		if err != nil {
			return line, usage
		}
		return "data: " + string(converted), usage
	}
	for i, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			cleanStreamDelta(delta, keepReasoning)
			choice["delta"] = delta
		}
		if msg, ok := choice["message"].(map[string]any); ok {
			cleanNulls(msg)
			promoteMisplacedReasoning(msg, keepReasoning)
			if !keepReasoning {
				delete(msg, "reasoning_content")
			}
			choice["message"] = msg
		}
		if v, ok := choice["logprobs"]; ok && v == nil {
			delete(choice, "logprobs")
		}
		if v, ok := choice["finish_reason"]; ok && v == nil {
			delete(choice, "finish_reason")
		}
		if s, ok := choice["finish_reason"].(string); ok && s == "" {
			delete(choice, "finish_reason")
		}
		choices[i] = choice
	}
	raw["choices"] = choices
	if v, ok := raw["usage"]; ok && v == nil {
		delete(raw, "usage")
	}
	delete(raw, "cost")
	converted, err := json.Marshal(raw)
	if err != nil {
		return line, usage
	}
	return "data: " + string(converted), usage
}

func convertResponse(data []byte, keepReasoning bool) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		slog.Warn("convertResponse unmarshal failed", "error", err)
		return data, nil
	}
	if choices, ok := raw["choices"].([]any); ok {
		for i, c := range choices {
			if choice, ok := c.(map[string]any); ok {
				if msg, ok := choice["message"].(map[string]any); ok {
					cleanNulls(msg)
					promoteMisplacedReasoning(msg, keepReasoning)
					if !keepReasoning {
						delete(msg, "reasoning_content")
					}
					choice["message"] = msg
				}
				if v, ok := choice["logprobs"]; ok && v == nil {
					delete(choice, "logprobs")
				}
				choices[i] = choice
			}
		}
		raw["choices"] = choices
	}
	delete(raw, "cost")
	return json.Marshal(raw)
}

// ======================== 认证层级 ========================

type TierType int

const (
	TierFree TierType = iota
	TierPaid
)

type AuthRouteMode int

const (
	AuthRoutePublic AuthRouteMode = iota
	AuthRouteAuto
	AuthRouteZen
	AuthRouteGo
)

type UpstreamAuth struct {
	Token  string
	Mode   AuthRouteMode
	Source string // authorization | x-api-key | none
}

func extractUpstreamAuth(r *http.Request) UpstreamAuth {
	token := ""
	source := "none"
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		token = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		source = "authorization"
	}
	if token == "" {
		if key := strings.TrimSpace(r.Header.Get("x-api-key")); key != "" {
			token = key
			source = "x-api-key"
		}
	}
	if token == "" || token == "public" {
		src := source
		if token == "" {
			src = "none"
		}
		return UpstreamAuth{Mode: AuthRoutePublic, Source: src}
	}
	// go:/zen: 前缀路由：去掉前缀后剩余部分仍需是有效 key（sk- 开头）
	if rest, ok := strings.CutPrefix(token, "go:"); ok && isValidOpenCodeKey(rest) {
		return UpstreamAuth{Token: rest, Mode: AuthRouteGo, Source: source}
	}
	if rest, ok := strings.CutPrefix(token, "zen:"); ok && isValidOpenCodeKey(rest) {
		return UpstreamAuth{Token: rest, Mode: AuthRouteZen, Source: source}
	}
	// 统一网关密钥（config.api_key）：客户端用它通过工具 UI 的密钥校验，
	// 网关只认这一把 key；上游仍按 public 免费档转发，不产生新的档位语义。
	configMu.RLock()
	unified := apiKey
	configMu.RUnlock()
	if unified != "" && token == unified {
		return UpstreamAuth{Mode: AuthRoutePublic, Source: source}
	}
	// 只有 sk- 开头的才是有效 key，其余（no-key-required 等占位符）一律走 public
	if isValidOpenCodeKey(token) {
		return UpstreamAuth{Token: token, Mode: AuthRouteAuto, Source: source}
	}
	return UpstreamAuth{Mode: AuthRoutePublic, Source: source}
}

// 只认 sk- 开头的 opencode key；Anthropic sk-ant-* 不能转发上游。
func isValidOpenCodeKey(token string) bool {
	if strings.HasPrefix(token, "sk-ant-") {
		return false
	}
	return strings.HasPrefix(token, "sk-") && len(token) > 15
}

func (auth UpstreamAuth) tier() TierType {
	if auth.Mode == AuthRoutePublic {
		return TierFree
	}
	return TierPaid
}

func (auth UpstreamAuth) authorizationHeader() string {
	if auth.Mode == AuthRoutePublic {
		return "Bearer public"
	}
	return "Bearer " + auth.Token
}

func (auth UpstreamAuth) shouldUseGoCatalog() bool {
	return auth.Mode == AuthRouteGo
}

func (auth UpstreamAuth) shouldUseGoEndpoint(modelID string) bool {
	switch auth.Mode {
	case AuthRouteGo:
		return isModelInGoCatalog(modelID)
	case AuthRouteAuto:
		return isGoCatalogOnlyModel(modelID)
	default:
		return false
	}
}

// isFreeModel 判断模型是否属于免费模型（以 -free 结尾）
func isFreeModel(modelID string) bool {
	return strings.HasSuffix(modelID, "-free")
}

// publicFacingModelID strips the upstream "-free" suffix for client-visible catalogs.
func publicFacingModelID(modelID string) string {
	if isFreeModel(modelID) {
		return strings.TrimSuffix(modelID, "-free")
	}
	return modelID
}

func modelExistsInCaches(modelID string) bool {
	modelMu.RLock()
	defer modelMu.RUnlock()
	return containsModelWithID(modelsCache, modelID) || containsModelWithID(goModelsCache, modelID)
}

func buildOCRequest(modelID string, bodyMap map[string]any, auth UpstreamAuth) (*http.Request, error) {
	return buildOCRequestWithEndpoint(modelID, bodyMap, auth, auth.shouldUseGoEndpoint(modelID))
}

func buildOCRequestWithEndpoint(modelID string, bodyMap map[string]any, auth UpstreamAuth, useGoEndpoint bool) (*http.Request, error) {
	bodyMap["model"] = modelID
	tryBody, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, err
	}
	var upstreamURL string
	if useGoEndpoint {
		upstreamURL = "https://opencode.ai/zen/go/v1/chat/completions"
	} else {
		upstreamURL = "https://opencode.ai/zen/v1/chat/completions"
	}
	req, err := http.NewRequest("POST", upstreamURL, bytes.NewReader(tryBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth.authorizationHeader())
	req.Header.Set("User-Agent", fmt.Sprintf("opencode/%s", ocClientVer))
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", ocProjectID)
	req.Header.Set("x-opencode-session", ocSessionID)
	req.Header.Set("x-opencode-request", "req_"+randomString(24))
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func shouldRetryUpstreamStatus(status int) bool {
	// 仅重试可恢复的临时性错误（始终同模型重试，不换模型）
	switch status {
	case http.StatusUnauthorized, // 401 认证过期或 token 未同步
		http.StatusTooManyRequests,    // 429 限流
		http.StatusBadGateway,         // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout:     // 504
		return true
	}
	// 其他 5xx 也重试，但 4xx 中只有 401 和 429 重试
	return status >= 500 && status < 600
}

const (
	maxUpstreamRetries = 3
	max401Retries      = 3
)

func maxAttemptsForUpstreamStatus(status int) int {
	if status == http.StatusUnauthorized {
		return max401Retries
	}
	return maxUpstreamRetries
}

func callOpenCodeAPI(ctx context.Context, upstreamBody []byte, modelID string, auth UpstreamAuth) ([]byte, int, http.Header, error) {
	initOCSession()

	var bodyMap map[string]any
	if err := json.Unmarshal(upstreamBody, &bodyMap); err != nil {
		return nil, 500, nil, fmt.Errorf("invalid request body")
	}
	useGoEndpoint := auth.shouldUseGoEndpoint(modelID)
	surface := "zen"
	if useGoEndpoint {
		surface = "go"
	}
	log := reqLogger(ctx)

	var lastErr error
	var retryCount int
	var lastBody []byte
	var lastStatus int
	var lastHeader http.Header
	maxAttempts := maxUpstreamRetries
	if max401Retries > maxAttempts {
		maxAttempts = max401Retries
	}

	// 免费额度耗尽自动切节点：独立预算（默认 5 个节点），不消耗上游重试次数
	nodeSwitchPending := false
	quotaSwitches := 0
	maxQuotaSwitches := effectiveMaxQuotaNodeSwitches()

	for attempt := 0; attempt < maxAttempts; attempt++ {
		up, err := buildOCRequestWithEndpoint(modelID, bodyMap, auth, useGoEndpoint)
		if err != nil {
			return nil, 500, nil, err
		}
		client, nodeFp := getNodeClientForTier(auth.tier(), nodeSwitchPending)
		nodeSwitchPending = false
		attemptStart := time.Now()
		resp, err := client.Do(up)
		durationMs := time.Since(attemptStart).Milliseconds()
		if err != nil {
			callLogEvent(ctx, "connect_error", nodeFp, err.Error())
			lastErr = err
			lastStatus = 0
			retryReason := "transport_error"
			canRetry := attempt+1 < maxUpstreamRetries
			if !canRetry {
				retryReason = ""
			}
			log.Info("upstream_attempt",
				"try_model", modelID,
				"surface", surface,
				"status", 0,
				"duration_ms", durationMs,
				"attempt_index", attempt,
				"retry_reason", retryReason,
				"error", err.Error(),
			)
			if canRetry {
				client.CloseIdleConnections()
				retryCount++
				continue
			}
			break
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			callLogEvent(ctx, "connect_ok", nodeFp, "")
			b, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				return nil, http.StatusBadGateway, nil, readErr
			}
			if isAnthropicFormat(b) {
				b = convertAnthropicToOpenAI(b, modelID)
			}
			log.Info("upstream_attempt",
				"try_model", modelID,
				"surface", surface,
				"status", resp.StatusCode,
				"duration_ms", durationMs,
				"attempt_index", attempt,
			)
			log.Info("upstream_result",
				"models_tried", []string{modelID},
				"retries", retryCount,
				"final_status", resp.StatusCode,
				"fallback_used", false,
			)
			return b, resp.StatusCode, resp.Header, nil
		}
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		logUpstreamError(ctx, modelID, resp.StatusCode, errBody)
		// 免费额度耗尽：标记当前节点 → 强制切下一个节点重试（预算内）
		if quota, quotaReason := classifyQuota(resp.StatusCode, errBody); quota {
			callLogEvent(ctx, "upstream_error", nodeFp, fmt.Sprintf("http %d quota_signal", resp.StatusCode))
			if nodeFp != "" && quotaSwitches < maxQuotaSwitches {
				callLogEvent(ctx, "switch", nodeFp, "quota:"+quotaReason)
				proxyPool.mark(nodeFp, NodeExhausted, "quota:"+quotaReason)
				quotaSwitches++
				client.CloseIdleConnections()
				nodeSwitchPending = true
				refreshOCSession()
				log.Info("quota_node_switch",
					"try_model", modelID,
					"surface", surface,
					"status", resp.StatusCode,
					"reason", quotaReason,
					"switches_done", quotaSwitches,
					"max_switches", maxQuotaSwitches,
				)
				continue
			}
			// 预算用尽或直连：不换，按原错误路径返回
		}
		nonRetryable := isNonRetryableUpstreamError(resp.StatusCode, errBody)
		canRetry := !nonRetryable && shouldRetryUpstreamStatus(resp.StatusCode) && attempt+1 < maxAttemptsForUpstreamStatus(resp.StatusCode)
		retryReason := ""
		if canRetry {
			retryReason = fmt.Sprintf("status_%d", resp.StatusCode)
		}
		if nonRetryable {
			retryReason = "non_retryable_upstream"
		}
		callLogEvent(ctx, "upstream_error", nodeFp, fmt.Sprintf("http %d", resp.StatusCode))
		log.Info("upstream_attempt",
			"try_model", modelID,
			"surface", surface,
			"status", resp.StatusCode,
			"duration_ms", durationMs,
			"attempt_index", attempt,
			"retry_reason", retryReason,
		)
		lastBody = errBody
		lastStatus = resp.StatusCode
		lastHeader = resp.Header
		lastErr = fmt.Errorf("upstream error")
		if !canRetry {
			break
		}
		client.CloseIdleConnections()
		retryCount++
	}
	log.Info("upstream_result",
		"models_tried", []string{modelID},
		"retries", retryCount,
		"final_status", lastStatus,
		"fallback_used", false,
	)
	if lastStatus < 200 {
		lastStatus = http.StatusBadGateway
	}
	return lastBody, lastStatus, lastHeader, lastErr
}

func callOpenCodeAPIStream(ctx context.Context, upstreamBody []byte, modelID string, auth UpstreamAuth) (io.ReadCloser, int, http.Header, error) {
	initOCSession()

	var bodyMap map[string]any
	if err := json.Unmarshal(upstreamBody, &bodyMap); err != nil {
		return nil, 500, nil, fmt.Errorf("invalid request body")
	}
	useGoEndpoint := auth.shouldUseGoEndpoint(modelID)
	surface := "zen"
	if useGoEndpoint {
		surface = "go"
	}
	log := reqLogger(ctx)

	var lastBody []byte
	var lastStatus int
	var lastHeader http.Header
	var retryCount int
	maxAttempts := maxUpstreamRetries
	if max401Retries > maxAttempts {
		maxAttempts = max401Retries
	}

	// 免费额度耗尽自动切节点（流式：仅头部非 2xx 时可切；已吐字节不可重试）
	nodeSwitchPending := false
	quotaSwitches := 0
	maxQuotaSwitches := effectiveMaxQuotaNodeSwitches()

	for attempt := 0; attempt < maxAttempts; attempt++ {
		up, err := buildOCRequestWithEndpoint(modelID, bodyMap, auth, useGoEndpoint)
		if err != nil {
			return nil, 500, nil, err
		}
		client, nodeFp := getNodeClientForTier(auth.tier(), nodeSwitchPending)
		nodeSwitchPending = false
		attemptStart := time.Now()
		resp, err := client.Do(up)
		durationMs := time.Since(attemptStart).Milliseconds()
		if err != nil {
			callLogEvent(ctx, "connect_error", nodeFp, err.Error())
			retryReason := "transport_error"
			canRetry := attempt+1 < maxUpstreamRetries
			if !canRetry {
				retryReason = ""
			}
			log.Info("upstream_attempt",
				"try_model", modelID,
				"surface", surface,
				"status", 0,
				"duration_ms", durationMs,
				"attempt_index", attempt,
				"retry_reason", retryReason,
				"error", err.Error(),
			)
			if canRetry {
				client.CloseIdleConnections()
				retryCount++
				continue
			}
			break
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			callLogEvent(ctx, "connect_ok", nodeFp, "")
			log.Info("upstream_attempt",
				"try_model", modelID,
				"surface", surface,
				"status", resp.StatusCode,
				"duration_ms", durationMs,
				"attempt_index", attempt,
			)
			log.Info("upstream_result",
				"models_tried", []string{modelID},
				"retries", retryCount,
				"final_status", resp.StatusCode,
				"fallback_used", false,
			)
			return resp.Body, resp.StatusCode, resp.Header, nil
		}
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		logUpstreamError(ctx, modelID, resp.StatusCode, errBody)
		// 免费额度耗尽：标记当前节点 → 强制切下一个节点重试（头部阶段可安全重试）
		if quota, quotaReason := classifyQuota(resp.StatusCode, errBody); quota {
			callLogEvent(ctx, "upstream_error", nodeFp, fmt.Sprintf("http %d quota_signal", resp.StatusCode))
			if nodeFp != "" && quotaSwitches < maxQuotaSwitches {
				callLogEvent(ctx, "switch", nodeFp, "quota:"+quotaReason)
				proxyPool.mark(nodeFp, NodeExhausted, "quota:"+quotaReason)
				quotaSwitches++
				client.CloseIdleConnections()
				nodeSwitchPending = true
				refreshOCSession()
				log.Info("quota_node_switch",
					"try_model", modelID,
					"surface", surface,
					"status", resp.StatusCode,
					"reason", quotaReason,
					"switches_done", quotaSwitches,
					"max_switches", maxQuotaSwitches,
				)
				continue
			}
		}
		nonRetryable := isNonRetryableUpstreamError(resp.StatusCode, errBody)
		canRetry := !nonRetryable && shouldRetryUpstreamStatus(resp.StatusCode) && attempt+1 < maxAttemptsForUpstreamStatus(resp.StatusCode)
		retryReason := ""
		if canRetry {
			retryReason = fmt.Sprintf("status_%d", resp.StatusCode)
		}
		if nonRetryable {
			retryReason = "non_retryable_upstream"
		}
		callLogEvent(ctx, "upstream_error", nodeFp, fmt.Sprintf("http %d", resp.StatusCode))
		log.Info("upstream_attempt",
			"try_model", modelID,
			"surface", surface,
			"status", resp.StatusCode,
			"duration_ms", durationMs,
			"attempt_index", attempt,
			"retry_reason", retryReason,
		)
		lastBody = errBody
		lastStatus = resp.StatusCode
		lastHeader = resp.Header
		if !canRetry {
			break
		}
		client.CloseIdleConnections()
		retryCount++
	}
	log.Info("upstream_result",
		"models_tried", []string{modelID},
		"retries", retryCount,
		"final_status", lastStatus,
		"fallback_used", false,
	)
	if lastStatus != 0 {
		return io.NopCloser(bytes.NewReader(lastBody)), lastStatus, lastHeader, nil
	}
	return nil, 500, nil, fmt.Errorf("upstream request failed")
}

// ======================== 安全响应头过滤 ========================

var safeResponseHeaders = map[string]bool{
	"Content-Type":          true,
	"X-RateLimit-Limit":     true,
	"X-RateLimit-Remaining": true,
	"X-RateLimit-Reset":     true,
}

func filterResponseHeaders(h http.Header) http.Header {
	filtered := make(http.Header)
	for k, v := range h {
		if safeResponseHeaders[k] {
			filtered[k] = v
		}
	}
	return filtered
}

// ======================== Chat Completions Handler ========================

func chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	auth := extractUpstreamAuth(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	cnt := requestCount.Add(1)
	maybeLogBodySummary(r.Context(), "chat completion request body", body)
	_ = cnt

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	modelIn := req.Model
	req.Model = resolveModel(req.Model)
	if req.Model == "" {
		modelIDs := getModelIDs()
		if len(modelIDs) > 0 {
			req.Model = modelIDs[0]
		} else {
			req.Model = "deepseek-v4-flash-free"
		}
	}

	// 多模态路由：检测到图片时转发到配置的上游

	req.Messages = fixToolCallGaps(req.Messages)
	keepReasoning := wantsReasoning(&req)
	req.Messages = ensureReasoningContent(req.Messages, keepReasoning)
	if req.Stream {
		if req.ExtraBody == nil {
			req.ExtraBody = map[string]any{}
		}
		req.ExtraBody["stream_options"] = map[string]any{"include_usage": true}
	}
	effortIn := req.ReasoningEffort
	if effortIn == "" && !isThinkingDisabled(req.Thinking) {
		effortIn = reasoningEffortFromThinking(req.Thinking)
	}
	upstreamSurface := "zen"
	if auth.shouldUseGoEndpoint(req.Model) {
		upstreamSurface = "go"
	}
	logRequestPlan(r.Context(), map[string]any{
		"protocol":             "chat",
		"model_in":             modelIn,
		"model_resolved":       req.Model,
		"auth_mode":            authModeString(auth.Mode),
		"auth_source":          auth.Source,
		"has_key":              auth.Token != "",
		"upstream_surface":     upstreamSurface,
		"stream":               req.Stream,
		"keep_reasoning":       keepReasoning,
		"thinking":             thinkingState(req.Thinking),
		"reasoning_effort_in":  effortIn,
		"reasoning_effort_out": mappedReasoningEffort(effortIn),
		"tools_count":          len(req.Tools),
		"messages_count":       len(req.Messages),
	})
	upstreamBody := buildUpstreamBody(&req)

	if req.Stream {
		clCtx := beginCallLog(r.Context(), r.URL.Path, req.Model, true, authModeString(auth.Mode))
		upResp, status, _, err := callOpenCodeAPIStream(clCtx, upstreamBody, req.Model, auth)
		if err != nil || status < 200 || status >= 300 {
			callLogFinish(clCtx, status, fmt.Sprintf("upstream http %d", status), 0, 0, 0, 0)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if upResp != nil {
				errBody, _ := io.ReadAll(upResp)
				if len(errBody) > 0 {
					w.Write(errBody)
					return
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error", "type": "upstream_error"}})
			return
		}
		defer upResp.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		reader := bufio.NewReader(upResp)
		stats := &streamResultStats{start: time.Now()}
		doneSeen := false
		var lastPt, lastCt, lastCc, lastCr int64
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					break
				}
				reqLogger(r.Context()).Error("stream read error", "error", err)
				callLogEvent(clCtx, "stream_interrupt", "", err.Error())
				callLogFinish(clCtx, 500, err.Error(), 0, 0, 0, 0)
				// 发送错误事件通知客户端
				w.Write([]byte("data: {\"error\":\"stream read error\"}\n\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				stats.log(r.Context(), "chat")
				return
			}
			if doneSeen {
				continue
			}
			trimmed := strings.TrimSpace(line)
			if trimmed == "data: [DONE]" {
				doneSeen = true
				stats.doneSeen = true
				w.Write([]byte("data: [DONE]\n\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				continue
			}

			if strings.HasPrefix(line, "data: ") {
				var raw map[string]any
				if json.Unmarshal([]byte(line[6:]), &raw) == nil {
					if choices, ok := raw["choices"].([]any); ok && len(choices) > 0 {
						if choice, ok := choices[0].(map[string]any); ok {
							if delta, ok := choice["delta"].(map[string]any); ok {
								stats.observeDelta(delta, keepReasoning)
							}
							if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
								stats.finishReason = fr
								stats.sawFinish = true
							}
						}
					}
				}
			}

			out, usage := convertStreamChunkWithUsage(line, keepReasoning)
			if out == "" {
				// 空choices chunk，但可能有 usage
				if usage != nil {
					pt, ct, cc, cr := usageFromMap(usage)
					tt, _ := usage["total_tokens"].(float64)
					if tt > 0 {
						recordTokenUsage(req.Model, pt, ct, int64(tt))
						lastPt, lastCt, lastCc, lastCr = pt, ct, cc, cr
					} else if pt > 0 {
						lastPt, lastCt, lastCc, lastCr = pt, ct, cc, cr
					}
				}
				continue
			}

			// 提取 usage（已在 convertStreamChunkWithUsage 中解析）
			if usage != nil && !doneSeen {
				pt, ct, cc, cr := usageFromMap(usage)
				tt, _ := usage["total_tokens"].(float64)
				if tt > 0 {
					recordTokenUsage(req.Model, pt, ct, int64(tt))
					lastPt, lastCt, lastCc, lastCr = pt, ct, cc, cr
				} else if pt > 0 {
					lastPt, lastCt, lastCc, lastCr = pt, ct, cc, cr
				}
			}

			w.Write([]byte(out))
			w.Write([]byte("\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if !doneSeen {
			callLogEvent(clCtx, "stream_interrupt", "", "EOF without [DONE]")
		}
		callLogFinish(clCtx, http.StatusOK, "", lastPt, lastCt, lastCc, lastCr)
		stats.log(r.Context(), "chat")
		return
	}

	clCtx := beginCallLog(r.Context(), r.URL.Path, req.Model, false, authModeString(auth.Mode))
	respBody, status, _, err := callOpenCodeAPI(clCtx, upstreamBody, req.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		callLogFinish(clCtx, status, fmt.Sprintf("upstream http %d", status), 0, 0, 0, 0)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if len(respBody) > 0 {
			w.Write(respBody)
		} else {
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error", "type": "upstream_error"}})
		}
		return
	}
	outBody := respBody
	convertedResp, err := convertResponse(respBody, keepReasoning)
	if err == nil {
		outBody = convertedResp
	}
	result := summarizeChatResult(outBody)
	if !keepReasoning {
		var before map[string]any
		if json.Unmarshal(respBody, &before) == nil {
			if choices, ok := before["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if msg, ok := choice["message"].(map[string]any); ok {
						content, _ := msg["content"].(string)
						rc, _ := msg["reasoning_content"].(string)
						if content == "" && rc != "" {
							result["promoted_reasoning"] = true
						}
					}
				}
			}
		}
	}
	logRequestResult(r.Context(), result)
	// Record token usage
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			pt, _ := u["prompt_tokens"].(float64)
			ct, _ := u["completion_tokens"].(float64)
			tt, _ := u["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(req.Model, int64(pt), int64(ct), int64(tt))
			}
		}
	}
	clPt, clCt, clCc, clCr := usageFromOpenAIBody(respBody)
	callLogFinish(clCtx, status, "", clPt, clCt, clCc, clCr)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(outBody)
}

// ======================== Models Handler ========================

func listModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	modelMu.RLock()
	loaded, models := modelsLoaded, modelsCache
	modelMu.RUnlock()
	if !loaded || len(models) == 0 {
		fetched, err := fetchModels()
		if err == nil && len(fetched) > 0 {
			modelMu.Lock()
			modelsCache = fetched
			modelsLoaded = true
			models = modelsCache
			modelMu.Unlock()
		}
	}
	if len(models) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "无法获取模型列表，请检查上游服务是否可用",
		})
		return
	}
	// 保存别名快照；目录权限仍按真实上游模型判断，最后再替换为客户端可见名称。
	configMu.RLock()
	aliases := make(map[string]string, len(modelAlias))
	for alias, upstream := range modelAlias {
		aliases[alias] = upstream
	}
	configMu.RUnlock()

	auth := extractUpstreamAuth(r)
	var combinedModels []ModelInfo
	switch {
	case auth.shouldUseGoCatalog():
		modelMu.RLock()
		combinedModels = make([]ModelInfo, 0, len(models)+len(goModelsCache))
		for _, model := range models {
			if isFreeModel(model.ID) {
				combinedModels = append(combinedModels, model)
			}
		}
		for _, goModel := range goModelsCache {
			if !containsModelWithID(combinedModels, goModel.ID) {
				combinedModels = append(combinedModels, goModel)
			}
		}
		modelMu.RUnlock()
	case auth.Mode == AuthRoutePublic:
		combinedModels = models
		filtered := make([]ModelInfo, 0, len(combinedModels))
		for _, m := range combinedModels {
			if isFreeModel(m.ID) {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) > 0 {
			combinedModels = filtered
		}
	default:
		combinedModels = models
	}
	allModels := replaceModelIDsWithAliases(combinedModels, aliases)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   allModels,
	})
}

func replaceModelIDsWithAliases(models []ModelInfo, aliases map[string]string) []ModelInfo {
	aliasesByUpstream := make(map[string][]string, len(aliases))
	for alias, upstream := range aliases {
		alias = strings.TrimSpace(alias)
		upstream = strings.TrimSpace(upstream)
		if alias == "" || upstream == "" {
			continue
		}
		aliasesByUpstream[upstream] = append(aliasesByUpstream[upstream], alias)
	}
	for upstream := range aliasesByUpstream {
		sort.Strings(aliasesByUpstream[upstream])
	}

	result := make([]ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		visibleIDs := aliasesByUpstream[model.ID]
		if len(visibleIDs) == 0 {
			visibleIDs = []string{publicFacingModelID(model.ID)}
		}
		for _, visibleID := range visibleIDs {
			if _, exists := seen[visibleID]; exists {
				continue
			}
			visibleModel := model
			visibleModel.ID = visibleID
			if visibleID != model.ID {
				visibleModel.OwnedBy = "alias"
			}
			// 从 models.dev 目录填充上下文/输出上限/输入模态（按真实上游 ID 查，alias 名兜底）
			if limit, ok := lookupModelLimit(model.ID); ok {
				ctx, out := limit.Context, limit.Output
				visibleModel.ContextWindow, visibleModel.MaxOutputTokens = &ctx, &out
				visibleModel.InputModalities = limit.InputModalities
			} else if limit, ok := lookupModelLimit(publicFacingModelID(model.ID)); ok {
				ctx, out := limit.Context, limit.Output
				visibleModel.ContextWindow, visibleModel.MaxOutputTokens = &ctx, &out
				visibleModel.InputModalities = limit.InputModalities
			}
			result = append(result, visibleModel)
			seen[visibleID] = struct{}{}
		}
	}
	return result
}

// adminModelsHandler 管理面板专用：返回面板可直接使用的真实上游模型 ID 列表
// （models 缓存 + Go 目录合并去重，不过滤免费、不套别名），供模型映射下拉框选择。
func adminModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	modelMu.RLock()
	loaded, modelsLoadedCount := modelsLoaded, len(modelsCache)
	modelMu.RUnlock()
	if !loaded || modelsLoadedCount == 0 {
		// 上游缓存为空时同步拉取一次，保证面板可用。
		if fetched, err := fetchModels(); err == nil && len(fetched) > 0 {
			modelMu.Lock()
			modelsCache = fetched
			modelsLoaded = true
			modelMu.Unlock()
		}
		if goFetched, goErr := fetchGoModels(); goErr == nil && len(goFetched) > 0 {
			modelMu.Lock()
			goModelsCache = goFetched
			modelMu.Unlock()
		}
	}
	modelMu.RLock()
	seen := make(map[string]struct{}, len(modelsCache)+len(goModelsCache))
	ids := make([]string, 0, len(modelsCache)+len(goModelsCache))
	appendIDs := func(list []ModelInfo) {
		for _, m := range list {
			if _, ok := seen[m.ID]; ok {
				continue
			}
			seen[m.ID] = struct{}{}
			ids = append(ids, m.ID)
		}
	}
	appendIDs(modelsCache)
	appendIDs(goModelsCache)
	modelMu.RUnlock()
	sort.Strings(ids)
	if len(ids) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "无法获取模型列表，请检查上游服务是否可用",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   ids,
	})
}

// ======================== Claude Messages API ========================

func extractClaudeSystemText(system any) string {
	if system == nil {
		return ""
	}
	switch v := system.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if block, ok := item.(map[string]any); ok {
				if block["type"] == "text" {
					if text, ok := block["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func cleanJsonSchema(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	clean := make(map[string]any, len(m))
	for k, v := range m {
		// Annotation-only keys are omitted for upstream compatibility. Constraint
		// keys such as additionalProperties and format are preserved.
		if k == "$schema" || k == "title" || k == "examples" {
			continue
		}
		switch child := v.(type) {
		case map[string]any:
			clean[k] = cleanJsonSchema(child)
		case []any:
			copyArray := make([]any, len(child))
			for i, elem := range child {
				copyArray[i] = cleanJsonSchema(elem)
			}
			clean[k] = copyArray
		default:
			clean[k] = v
		}
	}
	return clean
}

func claudeImageBlockToOpenAI(block map[string]any) (map[string]any, bool) {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil, false
	}
	srcType, _ := source["type"].(string)
	mediaType, _ := source["media_type"].(string)
	data, _ := source["data"].(string)
	url, _ := source["url"].(string)
	if srcType == "url" && url != "" {
		return map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}}, true
	}
	if srcType == "base64" && data != "" {
		if mediaType == "" {
			mediaType = "image/png"
		}
		return map[string]any{
			"type": "image_url",
			"image_url": map[string]string{
				"url": "data:" + mediaType + ";base64," + data,
			},
		}, true
	}
	return nil, false
}

func extractClaudeContentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] == "text" {
				if text, ok := block["text"].(string); ok && text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func claudeToOpenAIMessages(claudeMsgs []ClaudeMessage, system any) []Message {
	var systemParts []string
	if sysText := extractClaudeSystemText(system); sysText != "" {
		systemParts = append(systemParts, sysText)
	}

	var body []Message
	for _, msg := range claudeMsgs {
		if msg.Role == "system" {
			if text := extractClaudeContentText(msg.Content); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}
		switch content := msg.Content.(type) {
		case string:
			body = append(body, Message{Role: msg.Role, Content: content})
		case []any:
			var orderedContent []any
			var reasoningParts []string
			var toolCalls []ToolCall
			var toolResults []Message
			var followupImages []any
			for _, item := range content {
				block, ok := item.(map[string]any)
				if !ok {
					continue
				}
				blockType, _ := block["type"].(string)
				switch blockType {
				case "text":
					if text, ok := block["text"].(string); ok && text != "" {
						orderedContent = append(orderedContent, map[string]any{"type": "text", "text": text})
					}
				case "image":
					if part, ok := claudeImageBlockToOpenAI(block); ok {
						orderedContent = append(orderedContent, part)
					}
				case "thinking":
					if thinking, ok := block["thinking"].(string); ok && thinking != "" {
						reasoningParts = append(reasoningParts, thinking)
					}
				case "tool_use":
					id, _ := block["id"].(string)
					name, _ := block["name"].(string)
					var args string
					switch input := block["input"].(type) {
					case string:
						args = input
					default:
						if input != nil {
							b, _ := json.Marshal(input)
							args = string(b)
						}
					}
					if args == "" {
						args = "{}"
					}
					toolCalls = append(toolCalls, ToolCall{
						ID:   id,
						Type: "function",
						Function: FunctionCall{
							Name:      name,
							Arguments: args,
						},
					})
				case "tool_result":
					toolUseID, _ := block["tool_use_id"].(string)
					var resultText string
					var imageParts []any
					switch c := block["content"].(type) {
					case string:
						resultText = c
					case []any:
						var parts []string
						for _, p := range c {
							pb, ok := p.(map[string]any)
							if !ok {
								continue
							}
							switch pb["type"] {
							case "text":
								if t, ok := pb["text"].(string); ok {
									parts = append(parts, t)
								}
							case "image":
								if part, ok := claudeImageBlockToOpenAI(pb); ok {
									imageParts = append(imageParts, part)
								}
							}
						}
						resultText = strings.Join(parts, "\n")
					default:
						if c != nil {
							b, _ := json.Marshal(c)
							resultText = string(b)
						}
					}
					if len(imageParts) > 0 {
						if resultText != "" {
							resultText += "\n"
						}
						resultText += "[image attached]"
						followupImages = append(followupImages, imageParts...)
					}
					if isError, _ := block["is_error"].(bool); isError {
						resultText = "Error: " + resultText
					}
					toolResults = append(toolResults, Message{
						Role:       "tool",
						ToolCallID: toolUseID,
						Content:    resultText,
					})
				}
			}
			om := Message{Role: msg.Role}
			if len(orderedContent) > 0 {
				om.Content = orderedContent
			} else if len(toolCalls) == 0 {
				om.Content = ""
			}
			if len(reasoningParts) > 0 {
				rc := strings.Join(reasoningParts, "\n")
				om.ReasoningContent = &rc
			}
			if len(toolCalls) > 0 {
				om.ToolCalls = toolCalls
			}
			// Anthropic requires tool_result blocks to precede ordinary user
			// content. Preserve that order when translating them to Chat
			// Completions' separate tool messages.
			if msg.Role == "user" {
				body = append(body, toolResults...)
				if len(followupImages) > 0 {
					body = append(body, Message{Role: "user", Content: followupImages})
				}
			}
			if len(orderedContent) > 0 || len(reasoningParts) > 0 || len(toolCalls) > 0 || len(toolResults) == 0 {
				body = append(body, om)
			}
			if msg.Role != "user" {
				body = append(body, toolResults...)
				if len(followupImages) > 0 {
					body = append(body, Message{Role: "user", Content: followupImages})
				}
			}
		default:
			b, _ := json.Marshal(content)
			body = append(body, Message{Role: msg.Role, Content: string(b)})
		}
	}

	var messages []Message
	if len(systemParts) > 0 {
		messages = append(messages, Message{Role: "system", Content: strings.Join(systemParts, "\n\n")})
	}
	messages = append(messages, body...)
	return messages
}

func claudeToOpenAITools(claudeTools []ClaudeTool) ([]Tool, []string) {
	tools := make([]Tool, 0, len(claudeTools))
	var skipped []string
	for _, ct := range claudeTools {
		// Server tools (web_search_*, etc.) carry a vendor type and no client schema.
		// Emitting them as empty function tools would invite bogus model calls.
		if ct.Type != "" && ct.InputSchema == nil {
			skipped = append(skipped, ct.Name)
			continue
		}
		params := ct.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		params = cleanJsonSchema(params)
		paramsMap, ok := params.(map[string]any)
		if !ok {
			paramsMap = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        ct.Name,
				Description: ct.Description,
				Parameters:  paramsMap,
			},
		})
	}
	return tools, skipped
}

func countClaudeSystemParts(msgs []ClaudeMessage, system any) int {
	n := 0
	if extractClaudeSystemText(system) != "" {
		n++
	}
	for _, msg := range msgs {
		if msg.Role == "system" && extractClaudeContentText(msg.Content) != "" {
			n++
		}
	}
	return n
}

func countAnthropicBetas(header string) int {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	n := 0
	for _, part := range strings.Split(header, ",") {
		if strings.TrimSpace(part) != "" {
			n++
		}
	}
	return n
}

func countCacheControlInValue(v any) int {
	switch x := v.(type) {
	case map[string]any:
		n := 0
		if _, ok := x["cache_control"]; ok {
			n++
		}
		for _, child := range x {
			n += countCacheControlInValue(child)
		}
		return n
	case []any:
		n := 0
		for _, child := range x {
			n += countCacheControlInValue(child)
		}
		return n
	default:
		return 0
	}
}

func countClaudeCacheControlBlocks(req ClaudeRequest) int {
	n := countCacheControlInValue(req.System)
	for _, msg := range req.Messages {
		n += countCacheControlInValue(msg.Content)
	}
	for _, tool := range req.Tools {
		n += countCacheControlInValue(tool.InputSchema)
	}
	return n
}

var claudeUnsupportedBlockTypes = map[string]struct{}{
	"redacted_thinking":      {},
	"document":               {},
	"search_result":          {},
	"server_tool_use":        {},
	"web_search_tool_result": {},
	"container_upload":       {},
}

func scanClaudeUnsupportedBlocks(msgs []ClaudeMessage) map[string]int {
	counts := map[string]int{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if t, _ := x["type"].(string); t != "" {
				if _, ok := claudeUnsupportedBlockTypes[t]; ok {
					counts[t]++
				}
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	for _, msg := range msgs {
		walk(msg.Content)
	}
	if len(counts) == 0 {
		return nil
	}
	return counts
}

func openAIToClaudeResponse(chatBody []byte, model string, wantReasoning bool) []byte {
	var chat struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Message struct {
				Content          string     `json:"content"`
				ReasoningContent string     `json:"reasoning_content"`
				ToolCalls        []ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		slog.Warn("openAIToClaudeResponse unmarshal failed", "error", err)
	}

	content := []ClaudeContent{}
	stopReason := "end_turn"

	if len(chat.Choices) > 0 {
		msg := chat.Choices[0].Message
		fr := chat.Choices[0].FinishReason
		if wantReasoning && msg.ReasoningContent != "" {
			content = append(content, ClaudeContent{
				Type:     "thinking",
				Thinking: msg.ReasoningContent,
			})
		}
		text := msg.Content
		// #37635: Go gateway often puts the whole answer in reasoning_content.
		// Promote to text when content is empty so Claude Code does not see an
		// empty end_turn and exit the agent loop.
		if text == "" && msg.ReasoningContent != "" && len(msg.ToolCalls) == 0 {
			text = msg.ReasoningContent
		}
		if text != "" {
			content = append(content, ClaudeContent{
				Type: "text",
				Text: text,
			})
		}
		for _, tc := range msg.ToolCalls {
			var input any
			json.Unmarshal([]byte(tc.Function.Arguments), &input)
			if input == nil {
				input = map[string]any{}
			}
			content = append(content, ClaudeContent{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
		switch fr {
		case "stop":
			stopReason = "end_turn"
		case "length":
			stopReason = "max_tokens"
		case "tool_calls", "function_call":
			stopReason = "tool_use"
		case "content_filter":
			stopReason = "refusal"
		}
	}

	if len(content) == 0 {
		content = append(content, ClaudeContent{Type: "text", Text: ""})
	}

	resp := ClaudeResponse{
		ID:         fmt.Sprintf("msg_%s", randomString(24)),
		Type:       "message",
		Role:       "assistant",
		Content:    content,
		Model:      model,
		StopReason: stopReason,
	}
	if chat.Usage != nil {
		resp.Usage = buildClaudeMessageUsage(chat.Usage)
	}
	result, _ := json.Marshal(resp)
	return result
}

func toFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

func usageIntField(fields map[string]any, key string) (int, bool) {
	if fields == nil {
		return 0, false
	}
	value, ok := fields[key]
	if !ok || value == nil {
		return 0, false
	}
	return int(toFloat64(value)), true
}

func usageMapField(fields map[string]any, key string) (map[string]any, bool) {
	if fields == nil {
		return nil, false
	}
	value, ok := fields[key]
	if !ok || value == nil {
		return nil, false
	}
	mapped, ok := value.(map[string]any)
	return mapped, ok
}

func buildClaudeUsageCore(upstreamUsage map[string]any) ClaudeUsage {
	if len(upstreamUsage) == 0 {
		return nil
	}

	usage := ClaudeUsage{}
	if value, ok := usageIntField(upstreamUsage, "prompt_tokens"); ok {
		usage["input_tokens"] = value
	}
	if value, ok := usageIntField(upstreamUsage, "input_tokens"); ok {
		if _, exists := usage["input_tokens"]; !exists {
			usage["input_tokens"] = value
		}
	}
	if value, ok := usageIntField(upstreamUsage, "completion_tokens"); ok {
		usage["output_tokens"] = value
	}
	if value, ok := usageIntField(upstreamUsage, "output_tokens"); ok {
		if _, exists := usage["output_tokens"]; !exists {
			usage["output_tokens"] = value
		}
	}
	if value, ok := usageIntField(upstreamUsage, "cache_creation_input_tokens"); ok {
		usage["cache_creation_input_tokens"] = value
	}
	if value, ok := usageIntField(upstreamUsage, "cache_read_input_tokens"); ok {
		usage["cache_read_input_tokens"] = value
	} else if promptDetails, ok := usageMapField(upstreamUsage, "prompt_tokens_details"); ok {
		if value, ok := usageIntField(promptDetails, "cached_tokens"); ok {
			usage["cache_read_input_tokens"] = value
		}
	}
	if outputDetails, ok := usageMapField(upstreamUsage, "output_tokens_details"); ok {
		usage["output_tokens_details"] = outputDetails
	} else if outputDetails, ok := usageMapField(upstreamUsage, "completion_tokens_details"); ok {
		usage["output_tokens_details"] = outputDetails
	}
	if serverToolUse, ok := usageMapField(upstreamUsage, "server_tool_use"); ok {
		usage["server_tool_use"] = serverToolUse
	}
	if len(usage) == 0 {
		return nil
	}
	return usage
}

func buildClaudeMessageUsage(upstreamUsage map[string]any) ClaudeUsage {
	usage := buildClaudeUsageCore(upstreamUsage)
	if usage == nil {
		usage = ClaudeUsage{}
	}
	if cacheCreation, ok := usageMapField(upstreamUsage, "cache_creation"); ok {
		usage["cache_creation"] = cacheCreation
	}
	if serviceTier, ok := upstreamUsage["service_tier"].(string); ok && serviceTier != "" {
		usage["service_tier"] = serviceTier
	}
	if inferenceGeo, ok := upstreamUsage["inference_geo"].(string); ok && inferenceGeo != "" {
		usage["inference_geo"] = inferenceGeo
	}
	if _, exists := usage["input_tokens"]; !exists {
		usage["input_tokens"] = 0
	}
	if _, exists := usage["output_tokens"]; !exists {
		usage["output_tokens"] = 0
	}
	return usage
}

func buildClaudeDeltaUsage(upstreamUsage map[string]any) ClaudeUsage {
	usage := buildClaudeUsageCore(upstreamUsage)
	if usage == nil {
		usage = ClaudeUsage{}
	}
	if _, exists := usage["output_tokens"]; !exists {
		usage["output_tokens"] = 0
	}
	return usage
}

func claudeMessagesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	auth := extractUpstreamAuth(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	cnt := requestCount.Add(1)
	maybeLogBodySummary(r.Context(), "claude messages request body", body)
	_ = cnt

	var claudeReq ClaudeRequest
	if err := json.Unmarshal(body, &claudeReq); err != nil {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Invalid JSON"}}`, http.StatusBadRequest)
		return
	}
	modelIn := claudeReq.Model
	claudeReq.Model = resolveModel(claudeReq.Model)

	// 多模态路由

	chatReq, skippedServerTools := convertClaudeRequest(claudeReq)
	chatReq.Messages = fixToolCallGaps(chatReq.Messages)
	if claudeReq.Stream {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["stream_options"] = map[string]any{"include_usage": true}
	}

	// Keep CoT by default so Claude Code still sees thinking blocks. Only drop
	// reasoning when force-disabled or the client explicitly disables thinking.
	// Empty-reply protection is handled by promoteMisplacedReasoning (!keep)
	// and emitEmptyTextFallback (keep + no text/tool_use).
	wantReasoning := !getForceDisableThinking()
	if claudeReq.Thinking != nil && isThinkingDisabled(claudeReq.Thinking) {
		wantReasoning = false
	}
	keepReasoning := wantReasoning
	chatReq.Messages = ensureReasoningContent(chatReq.Messages, keepReasoning)

	effortIn := chatReq.ReasoningEffort
	if effortIn == "" && !isThinkingDisabled(claudeReq.Thinking) {
		effortIn = reasoningEffortFromThinking(claudeReq.Thinking)
	}
	upstreamSurface := "zen"
	if auth.shouldUseGoEndpoint(chatReq.Model) {
		upstreamSurface = "go"
	}
	systemMerged := countClaudeSystemParts(claudeReq.Messages, claudeReq.System) > 1
	plan := map[string]any{
		"protocol":             "claude",
		"model_in":             modelIn,
		"model_resolved":       chatReq.Model,
		"auth_mode":            authModeString(auth.Mode),
		"auth_source":          auth.Source,
		"has_key":              auth.Token != "",
		"upstream_surface":     upstreamSurface,
		"stream":               claudeReq.Stream,
		"keep_reasoning":       keepReasoning,
		"thinking":             thinkingState(claudeReq.Thinking),
		"reasoning_effort_in":  effortIn,
		"reasoning_effort_out": mappedReasoningEffort(effortIn),
		"tools_count":          len(chatReq.Tools),
		"messages_count":       len(chatReq.Messages),
		"system_merged":        systemMerged,
		"context_management":   claudeReq.ContextManagement != nil,
		"cache_control_blocks": countClaudeCacheControlBlocks(claudeReq),
		"client_beta_count":    countAnthropicBetas(r.Header.Get("anthropic-beta")),
		"unsupported_blocks":   scanClaudeUnsupportedBlocks(claudeReq.Messages),
	}
	if len(skippedServerTools) > 0 {
		plan["skipped_server_tools"] = skippedServerTools
	}
	logRequestPlan(r.Context(), plan)

	upstreamBody := buildUpstreamBody(&chatReq)

	if claudeReq.Stream {
		clCtx := beginCallLog(r.Context(), r.URL.Path, chatReq.Model, true, authModeString(auth.Mode))
		upResp, status, _, err := callOpenCodeAPIStream(clCtx, upstreamBody, chatReq.Model, auth)
		if err != nil || status < 200 || status >= 300 {
			callLogFinish(clCtx, status, fmt.Sprintf("upstream http %d", status), 0, 0, 0, 0)
			errResp := map[string]any{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": "upstream error"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(errResp)
			return
		}
		defer upResp.Close()
		clPt, clCt, clCc, clCr := claudeStreamHandler(clCtx, w, upResp, claudeReq.Model, keepReasoning)
		callLogFinish(clCtx, http.StatusOK, "", clPt, clCt, clCc, clCr)
		return
	}

	clCtx := beginCallLog(r.Context(), r.URL.Path, chatReq.Model, false, authModeString(auth.Mode))
	respBody, status, _, err := callOpenCodeAPI(clCtx, upstreamBody, chatReq.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		callLogFinish(clCtx, status, fmt.Sprintf("upstream http %d", status), 0, 0, 0, 0)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if len(respBody) > 0 {
			w.Write(respBody)
		} else {
			json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": "upstream error"}})
		}
		return
	}

	claudeRespBody := openAIToClaudeResponse(respBody, claudeReq.Model, wantReasoning)
	result := summarizeClaudeResult(claudeRespBody)
	if !wantReasoning {
		var before map[string]any
		if json.Unmarshal(respBody, &before) == nil {
			if choices, ok := before["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if msg, ok := choice["message"].(map[string]any); ok {
						content, _ := msg["content"].(string)
						rc, _ := msg["reasoning_content"].(string)
						if content == "" && rc != "" {
							result["promoted_reasoning"] = true
						}
					}
				}
			}
		}
	}
	logRequestResult(r.Context(), result)

	// Record token usage
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			pt, _ := u["prompt_tokens"].(float64)
			ct, _ := u["completion_tokens"].(float64)
			tt, _ := u["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(claudeReq.Model, int64(pt), int64(ct), int64(tt))
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	maybeLogBodySummary(r.Context(), "claude response body", claudeRespBody)
	clPt, clCt, clCc, clCr := usageFromOpenAIBody(respBody)
	callLogFinish(clCtx, http.StatusOK, "", clPt, clCt, clCc, clCr)
	w.Write(claudeRespBody)
}

// claudeStreamHandler 将上游 OpenAI 流转换为 Claude SSE 流，返回最终 输入/输出/缓存创建/缓存读取 token。
func claudeStreamHandler(ctx context.Context, w http.ResponseWriter, respBody io.ReadCloser, model string, keepReasoning bool) (pt, ct, cc, cr int64) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(respBody)
	stats := &streamResultStats{start: time.Now()}

	msgID := fmt.Sprintf("msg_%s", randomString(24))
	blockIndex := 0
	thinkingBlockOpen := false
	textBlockOpen := false
	toolCallAccumulator := map[int]map[string]string{}
	toolBlockIndices := map[int]int{}
	toolCallOrder := []int{}
	messageStartSent := false
	finished := false
	stopReason := "end_turn"
	fullUsage := map[string]any{}
	// Accumulates reasoning when keepReasoning so we can fall back to a text
	// block if the stream never produces content/tool_use (#37635).
	reasoningFallback := strings.Builder{}
	defer func() {
		lpt, lct, lcc, lcr := usageFromMap(fullUsage)
		tt, _ := fullUsage["total_tokens"].(float64)
		if tt > 0 {
			recordTokenUsage(model, lpt, lct, int64(tt))
		}
		pt, ct, cc, cr = lpt, lct, lcc, lcr
		stats.toolCallCount = len(toolCallOrder)
		stats.log(ctx, "claude")
	}()

	emitClaudeEvent := func(event string, data any) {
		jsonData, err := json.Marshal(data)
		if err != nil {
			reqLogger(ctx).Error("marshal SSE event failed", "error", err)
			return
		}
		w.Write([]byte("event: " + event + "\n"))
		w.Write([]byte("data: " + string(jsonData) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	closeThinkingBlock := func() {
		if !thinkingBlockOpen {
			return
		}
		emitClaudeEvent("content_block_stop", map[string]any{
			"type":          "content_block_stop",
			"index":         blockIndex - 1,
			"content_block": map[string]any{"type": "thinking"},
		})
		thinkingBlockOpen = false
	}

	closeTextBlock := func() {
		if !textBlockOpen {
			return
		}
		emitClaudeEvent("content_block_stop", map[string]any{
			"type":          "content_block_stop",
			"index":         blockIndex - 1,
			"content_block": map[string]any{"type": "text"},
		})
		textBlockOpen = false
	}

	ensureMessageStart := func() {
		if messageStartSent {
			return
		}
		messageStartSent = true
		emitClaudeEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":          msgID,
				"type":        "message",
				"role":        "assistant",
				"content":     []any{},
				"model":       model,
				"stop_reason": nil,
				"usage":       buildClaudeMessageUsage(fullUsage),
			},
		})
		emitClaudeEvent("ping", map[string]any{"type": "ping"})
	}

	emitTextDelta := func(contentStr string) {
		if contentStr == "" {
			return
		}
		stats.textChars += len(contentStr)
		closeThinkingBlock()
		if !textBlockOpen {
			emitClaudeEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": blockIndex,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})
			textBlockOpen = true
			blockIndex++
		}
		emitClaudeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex - 1,
			"delta": map[string]any{
				"type": "text_delta",
				"text": contentStr,
			},
		})
	}

	emitEmptyTextFallback := func() {
		if textBlockOpen || len(toolCallOrder) > 0 {
			return
		}
		fallback := reasoningFallback.String()
		if fallback == "" {
			return
		}
		stats.promotedReasoning = true
		emitTextDelta(fallback)
	}

	finalizeContentBlocks := func() {
		emitEmptyTextFallback()
		closeThinkingBlock()
		closeTextBlock()
		for _, idx := range toolCallOrder {
			acc := toolCallAccumulator[idx]
			emitClaudeEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": toolBlockIndices[idx],
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    acc["id"],
					"name":  acc["name"],
					"input": map[string]any{},
				},
			})
		}
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			reqLogger(ctx).Error("stream read error", "error", err)
			break
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
			stats.doneSeen = true
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(line[6:]), &chunk); err != nil {
			continue
		}
		if usage, ok := chunk["usage"].(map[string]any); ok {
			fullUsage = usage
		}

		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			// Usage-only trailing chunk (OpenAI stream_options.include_usage).
			continue
		}

		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		finishReason, _ := choice["finish_reason"].(string)
		stats.noteChunk()

		ensureMessageStart()

		// After finish_reason, ignore further content deltas but keep reading
		// so a later usage-only chunk can populate fullUsage.
		if finished {
			continue
		}

		if rc, ok := delta["reasoning_content"]; ok {
			rcStr, _ := rc.(string)
			if rcStr != "" {
				stats.reasoningChars += len(rcStr)
				if keepReasoning {
					reasoningFallback.WriteString(rcStr)
					closeTextBlock()
					if !thinkingBlockOpen {
						emitClaudeEvent("content_block_start", map[string]any{
							"type":  "content_block_start",
							"index": blockIndex,
							"content_block": map[string]any{
								"type":     "thinking",
								"thinking": "",
							},
						})
						thinkingBlockOpen = true
						blockIndex++
					}
					emitClaudeEvent("content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": blockIndex - 1,
						"delta": map[string]any{
							"type":     "thinking_delta",
							"thinking": rcStr,
						},
					})
				} else {
					// Thinking not requested: promote misplaced CoT to visible text (#37635).
					stats.promotedReasoning = true
					emitTextDelta(rcStr)
				}
			}
		}

		if c, ok := delta["content"]; ok && c != nil {
			contentStr, _ := c.(string)
			if contentStr != "" {
				emitTextDelta(contentStr)
			}
		}

		if rawToolCalls, ok := delta["tool_calls"].([]any); ok {
			for _, rawTC := range rawToolCalls {
				tc, ok := rawTC.(map[string]any)
				if !ok {
					continue
				}
				idxFloat, _ := tc["index"].(float64)
				upstreamIndex := int(idxFloat)

				closeThinkingBlock()
				closeTextBlock()

				if _, exists := toolCallAccumulator[upstreamIndex]; !exists {
					callID, _ := tc["id"].(string)
					if callID == "" {
						callID = "toolu_" + randomString(12)
					}
					fn, _ := tc["function"].(map[string]any)
					name, _ := fn["name"].(string)
					toolCallAccumulator[upstreamIndex] = map[string]string{
						"id":   callID,
						"name": name,
						"args": "",
					}
					toolCallOrder = append(toolCallOrder, upstreamIndex)
					toolBlockIndices[upstreamIndex] = blockIndex
					emitClaudeEvent("content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": blockIndex,
						"content_block": map[string]any{
							"type":  "tool_use",
							"id":    callID,
							"name":  name,
							"input": map[string]any{},
						},
					})
					blockIndex++
				}

				fn, _ := tc["function"].(map[string]any)
				if argDelta, ok := fn["arguments"].(string); ok && argDelta != "" {
					toolCallAccumulator[upstreamIndex]["args"] += argDelta
					emitClaudeEvent("content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": toolBlockIndices[upstreamIndex],
						"delta": map[string]any{
							"type":         "input_json_delta",
							"partial_json": argDelta,
						},
					})
				}
			}
		}

		if finishReason == "stop" || finishReason == "length" || finishReason == "tool_calls" || finishReason == "function_call" || finishReason == "content_filter" {
			stats.finishReason = finishReason
			stats.sawFinish = true
			finished = true
			finalizeContentBlocks()

			stopReason = "end_turn"
			switch finishReason {
			case "length":
				stopReason = "max_tokens"
			case "tool_calls", "function_call":
				stopReason = "tool_use"
			case "content_filter":
				stopReason = "refusal"
			}
			// Do not emit message_delta/stop yet: OpenAI-compatible upstreams often
			// send the usage-only chunk after finish_reason when include_usage=true.
			continue
		}
	}

	ensureMessageStart()
	if !finished {
		finalizeContentBlocks()
	}
	emitClaudeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason},
		"usage": buildClaudeDeltaUsage(fullUsage),
	})
	emitClaudeEvent("message_stop", map[string]any{"type": "message_stop"})
	return
}

func indexOfInt(slice []int, val int) int {
	for i, v := range slice {
		if v == val {
			return i
		}
	}
	return 0
}

// ======================== Responses API ========================

func responsesInputToMessages(input any, instructions string) []Message {
	var messages []Message
	if instructions != "" {
		messages = append(messages, Message{Role: "system", Content: instructions})
	}
	switch v := input.(type) {
	case string:
		messages = append(messages, Message{Role: "user", Content: v})
	case []any:
		functionOutputs := collectFunctionOutputs(v)
		for _, item := range v {
			switch elem := item.(type) {
			case string:
				messages = append(messages, Message{Role: "user", Content: elem})
			case map[string]any:
				itemType, _ := elem["type"].(string)
				switch itemType {
				case "function_call", "tool_call", "apply_patch_call", "shell_call":
					callID, _ := elem["call_id"].(string)
					if callID == "" {
						callID, _ = elem["id"].(string)
					}
					name, _ := elem["name"].(string)
					if name == "" {
						switch itemType {
						case "apply_patch_call":
							name = "apply_patch"
						case "shell_call":
							name = "shell"
						}
					}
					args, _ := elem["arguments"].(string)
					if name == "" {
						if tu, ok := elem["tool_use"].(map[string]any); ok {
							name, _ = tu["name"].(string)
							callID, _ = tu["id"].(string)
							if a, ok := tu["arguments"].(string); ok {
								args = a
							} else if inp, ok := tu["input"]; ok {
								b, _ := json.Marshal(inp)
								args = string(b)
							}
						}
					}
					if args == "" {
						args = buildBuiltInToolCallArguments(itemType, elem)
					}
					if args == "" {
						args = "{}"
					}
					messages = append(messages, Message{
						Role:    "assistant",
						Content: "",
						ToolCalls: []ToolCall{{
							ID:   callID,
							Type: "function",
							Function: FunctionCall{
								Name:      name,
								Arguments: args,
							},
						}},
					})
					if callID != "" {
						output := functionOutputs[callID]
						if output == "" {
							output = "[tool output missing]"
						}
						messages = append(messages, Message{Role: "tool", ToolCallID: callID, Content: output})
					}
				case "function_call_output", "tool_result", "apply_patch_call_output", "shell_call_output":
					callID, _ := elem["call_id"].(string)
					if callID == "" {
						callID, _ = elem["tool_use_id"].(string)
					}
					if callID != "" {
						output := functionOutputs[callID]
						if output == "" {
							switch o := elem["output"].(type) {
							case string:
								output = o
							default:
								if o != nil {
									b, _ := json.Marshal(o)
									output = string(b)
								}
							}
						}
						if output == "" {
							b, err := json.Marshal(elem)
							if err == nil {
								output = string(b)
							}
						}
						if output == "" {
							output = "[tool output missing]"
						}
						messages = append(messages, Message{Role: "tool", ToolCallID: callID, Content: output})
					}
					continue
				case "reasoning":
					if text := extractTextFromContentParts(elem["summary"]); text != "" {
						messages = append(messages, Message{Role: "assistant", Content: "", ReasoningContent: &text})
					}
					continue
				case "message", "":
					role := "user"
					if r, ok := elem["role"].(string); ok && r != "" {
						role = r
					}
					if role == "developer" {
						role = "system"
					}
					content := responsesContentToMessageContent(elem["content"])
					messages = append(messages, Message{Role: role, Content: content})
				default:
					role := "user"
					if r, ok := elem["role"].(string); ok && r != "" {
						role = r
					}
					content := responsesContentToMessageContent(elem["content"])
					emptyContent := false
					switch v := content.(type) {
					case nil:
						emptyContent = true
					case string:
						emptyContent = v == ""
					case []any:
						emptyContent = len(v) == 0
					}
					if emptyContent {
						b, err := json.Marshal(elem)
						if err != nil {
							continue
						}
						content = string(b)
					}
					messages = append(messages, Message{Role: role, Content: content})
				}
			default:
				b, _ := json.Marshal(elem)
				messages = append(messages, Message{Role: "user", Content: string(b)})
			}
		}
	default:
		b, _ := json.Marshal(v)
		messages = append(messages, Message{Role: "user", Content: string(b)})
	}
	return messages
}

func convertResponsesTools(tools []ResponsesTool) []Tool {
	converted := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		fn, ok := responsesToolFunction(tool)
		if !ok {
			continue
		}
		converted = append(converted, Tool{Type: "function", Function: fn})
	}
	return converted
}

func responsesToolFunction(tool ResponsesTool) (ToolFunction, bool) {
	switch tool.Type {
	case "function":
		fn := ToolFunction{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.Parameters,
		}
		if tool.Function != nil {
			fn = *tool.Function
		}
		if fn.Parameters == nil {
			fn.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		return fn, true
	case "apply_patch":
		return ToolFunction{
			Name:        "apply_patch",
			Description: "Create, update, or delete files using a structured patch operation or unified diff.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"input": map[string]any{
						"type":        "string",
						"description": "Patch diff or patch instructions to apply.",
					},
					"operation": map[string]any{
						"type":        "object",
						"description": "Structured patch operation, including file action and diff payload.",
					},
				},
			},
		}, true
	case "shell":
		return ToolFunction{
			Name:        "shell",
			Description: "Run a shell command in the local workspace and return stdout, stderr, and exit details.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "Shell command to execute.",
					},
					"timeout_ms": map[string]any{
						"type":        "integer",
						"description": "Optional timeout in milliseconds.",
					},
					"working_directory": map[string]any{
						"type":        "string",
						"description": "Optional working directory for the command.",
					},
					"max_output_tokens": map[string]any{
						"type":        "integer",
						"description": "Optional output budget hint.",
					},
				},
				"required": []string{"command"},
			},
		}, true
	default:
		return ToolFunction{}, false
	}
}

func responsesToolName(tool ResponsesTool) string {
	switch tool.Type {
	case "function":
		if tool.Function != nil && tool.Function.Name != "" {
			return tool.Function.Name
		}
		return tool.Name
	case "apply_patch":
		return "apply_patch"
	case "shell":
		return "shell"
	default:
		return ""
	}
}

func responsesToolKindMap(tools []ResponsesTool) map[string]string {
	kinds := make(map[string]string, len(tools))
	for _, tool := range tools {
		name := responsesToolName(tool)
		if name == "" {
			continue
		}
		kinds[name] = tool.Type
	}
	return kinds
}

func toolCallOutputType(name string, kinds map[string]string) string {
	switch kinds[name] {
	case "apply_patch":
		return "apply_patch_call"
	case "shell":
		return "shell_call"
	default:
		return "function_call"
	}
}

func convertResponsesToolChoice(choice any) any {
	if choice == nil {
		return nil
	}
	choiceMap, ok := choice.(map[string]any)
	if !ok {
		return choice
	}
	if choiceMap["type"] == "function" {
		if name, ok := choiceMap["name"].(string); ok && name != "" {
			return map[string]any{
				"type":     "function",
				"function": map[string]any{"name": name},
			}
		}
	}
	if choiceType, ok := choiceMap["type"].(string); ok {
		switch choiceType {
		case "apply_patch", "shell":
			return map[string]any{
				"type":     "function",
				"function": map[string]any{"name": choiceType},
			}
		}
	}
	return choice
}

func collectFunctionOutputs(items []any) map[string]string {
	outputs := map[string]string{}
	for _, item := range items {
		elem, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := elem["type"].(string)
		switch itemType {
		case "function_call_output", "apply_patch_call_output", "shell_call_output":
		default:
			continue
		}
		callID, _ := elem["call_id"].(string)
		if callID == "" {
			continue
		}
		switch v := elem["output"].(type) {
		case string:
			outputs[callID] = v
		default:
			b, _ := json.Marshal(v)
			outputs[callID] = string(b)
		}
	}
	return outputs
}

func parseJSONString(input string) any {
	var parsed any
	if input == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(input), &parsed); err != nil {
		return nil
	}
	return parsed
}

func buildBuiltInToolCallArguments(itemType string, elem map[string]any) string {
	if arguments, ok := elem["arguments"].(string); ok && arguments != "" {
		return arguments
	}

	payload := map[string]any{}
	switch itemType {
	case "apply_patch_call":
		if input, ok := elem["input"].(string); ok && input != "" {
			payload["input"] = input
		}
		if operation, ok := elem["operation"]; ok && operation != nil {
			payload["operation"] = operation
		}
	case "shell_call":
		for _, key := range []string{"command", "timeout_ms", "working_directory", "max_output_tokens"} {
			if value, ok := elem[key]; ok && value != nil {
				payload[key] = value
			}
		}
	}
	if len(payload) == 0 {
		payload = elem
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func buildResponseToolCallItem(tc ToolCall, outputType string) map[string]any {
	switch outputType {
	case "apply_patch_call":
		item := map[string]any{
			"id":      "apc_" + tc.ID,
			"type":    outputType,
			"status":  "completed",
			"call_id": tc.ID,
		}
		if parsed, ok := parseJSONString(tc.Function.Arguments).(map[string]any); ok {
			for key, value := range parsed {
				item[key] = value
			}
		} else if tc.Function.Arguments != "" {
			item["arguments"] = tc.Function.Arguments
		}
		return item
	case "shell_call":
		item := map[string]any{
			"id":      "shc_" + tc.ID,
			"type":    outputType,
			"status":  "completed",
			"call_id": tc.ID,
		}
		if parsed, ok := parseJSONString(tc.Function.Arguments).(map[string]any); ok {
			for key, value := range parsed {
				item[key] = value
			}
		} else if tc.Function.Arguments != "" {
			item["arguments"] = tc.Function.Arguments
		}
		return item
	default:
		return map[string]any{
			"id":        "fc_" + tc.ID,
			"type":      "function_call",
			"status":    "completed",
			"arguments": tc.Function.Arguments,
			"call_id":   tc.ID,
			"name":      tc.Function.Name,
		}
	}
}

func cloneJSONValue[T any](value T) T {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var cloned T
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return value
	}
	return cloned
}

func storeResponseState(response map[string]any, req ResponsesAPIRequest) {
	if req.Store != nil && !*req.Store {
		return
	}
	responseID, _ := response["id"].(string)
	if responseID == "" {
		return
	}
	output, _ := response["output"].([]any)
	storedResponsesMu.Lock()
	storedResponses[responseID] = StoredResponseState{
		Model:        req.Model,
		Instructions: req.Instructions,
		Tools:        cloneJSONValue(req.Tools),
		ToolChoice:   cloneJSONValue(req.ToolChoice),
		Output:       cloneJSONValue(output),
	}
	storedResponsesMu.Unlock()
}

func loadResponseState(responseID string) (StoredResponseState, bool) {
	storedResponsesMu.RLock()
	defer storedResponsesMu.RUnlock()
	state, ok := storedResponses[responseID]
	if !ok {
		return StoredResponseState{}, false
	}
	return cloneJSONValue(state), true
}

func extractTextFromContentParts(content any) string {
	parts, ok := content.([]any)
	if !ok {
		if s, ok := content.(string); ok {
			return s
		}
		return ""
	}
	var texts []string
	for _, p := range parts {
		if part, ok := p.(map[string]any); ok {
			if part["type"] == "input_text" || part["type"] == "output_text" {
				if t, ok := part["text"].(string); ok {
					texts = append(texts, t)
				}
			}
		}
	}
	return strings.Join(texts, "\n")
}

func convertResponsesContentPart(part map[string]any) (map[string]any, bool) {
	partType, _ := part["type"].(string)
	switch partType {
	case "input_text", "output_text", "text":
		text, _ := part["text"].(string)
		if text == "" {
			return nil, false
		}
		return map[string]any{
			"type": "text",
			"text": text,
		}, true
	case "input_image":
		imageURL, _ := part["image_url"].(string)
		if imageURL == "" {
			return nil, false
		}
		imageURLValue := map[string]any{
			"url": imageURL,
		}
		if detail, ok := part["detail"].(string); ok && detail != "" {
			imageURLValue["detail"] = detail
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": imageURLValue,
		}, true
	default:
		return nil, false
	}
}

func responsesContentToMessageContent(content any) any {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		return s
	}

	parts, ok := content.([]any)
	if !ok {
		b, err := json.Marshal(content)
		if err != nil {
			return nil
		}
		return string(b)
	}

	convertedParts := make([]any, 0, len(parts))
	texts := make([]string, 0, len(parts))
	onlyTextParts := true

	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		convertedPart, ok := convertResponsesContentPart(part)
		if !ok {
			text := extractTextFromContentParts([]any{part})
			if text == "" {
				b, err := json.Marshal(part)
				if err != nil {
					continue
				}
				text = string(b)
			}
			convertedParts = append(convertedParts, map[string]any{
				"type": "text",
				"text": text,
			})
			texts = append(texts, text)
			continue
		}

		if convertedPart["type"] != "text" {
			onlyTextParts = false
		}
		if text, ok := convertedPart["text"].(string); ok && text != "" {
			texts = append(texts, text)
		}
		convertedParts = append(convertedParts, convertedPart)
	}

	if len(convertedParts) == 0 {
		return ""
	}
	if onlyTextParts {
		return strings.Join(texts, "\n")
	}
	return convertedParts
}

func chatContentToResponsesContent(content any) ([]any, string) {
	switch v := content.(type) {
	case nil:
		return nil, ""
	case string:
		if v == "" {
			return nil, ""
		}
		return []any{map[string]any{
			"type":        "output_text",
			"text":        v,
			"annotations": []any{},
			"logprobs":    []any{},
		}}, v
	case []any:
		parts := make([]any, 0, len(v))
		texts := make([]string, 0, len(v))
		for _, rawPart := range v {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			partType, _ := part["type"].(string)
			switch partType {
			case "text", "input_text", "output_text":
				text, _ := part["text"].(string)
				if text == "" {
					continue
				}
				annotations, ok := part["annotations"]
				if !ok {
					annotations = []any{}
				}
				logprobs, ok := part["logprobs"]
				if !ok {
					logprobs = []any{}
				}
				texts = append(texts, text)
				parts = append(parts, map[string]any{
					"type":        "output_text",
					"text":        text,
					"annotations": annotations,
					"logprobs":    logprobs,
				})
			}
		}
		return parts, strings.Join(texts, "\n")
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, ""
		}
		text := string(b)
		return []any{map[string]any{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
			"logprobs":    []any{},
		}}, text
	}
}

func responsesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	auth := extractUpstreamAuth(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	cnt := requestCount.Add(1)
	maybeLogBodySummary(r.Context(), "responses request body", body)
	_ = cnt

	var respReq ResponsesAPIRequest
	if err := json.Unmarshal(body, &respReq); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	modelIn := respReq.Model
	respReq.Model = resolveModel(respReq.Model)
	previousState, hasPreviousState := StoredResponseState{}, false
	if respReq.PreviousResponseID != "" {
		previousState, hasPreviousState = loadResponseState(respReq.PreviousResponseID)
		if respReq.Model == "" && previousState.Model != "" {
			respReq.Model = previousState.Model
		}
		if len(respReq.Tools) == 0 && len(previousState.Tools) > 0 {
			respReq.Tools = previousState.Tools
		}
		if respReq.ToolChoice == nil && previousState.ToolChoice != nil {
			respReq.ToolChoice = previousState.ToolChoice
		}
	}
	if respReq.Model == "" {
		modelIDs := getModelIDs()
		if len(modelIDs) > 0 {
			respReq.Model = modelIDs[0]
		} else {
			respReq.Model = "deepseek-v4-flash-free"
		}
	}

	// 多模态路由

	messages := respReq.Messages
	if len(messages) == 0 {
		if hasPreviousState && len(previousState.Output) > 0 {
			messages = append(messages, responsesInputToMessages(previousState.Output, "")...)
		}
		messages = append(messages, responsesInputToMessages(respReq.Input, respReq.Instructions)...)
	} else if respReq.Instructions != "" {
		messages = append([]Message{{Role: "system", Content: respReq.Instructions}}, messages...)
	}

	chatReq := OpenAIRequest{
		Model:    respReq.Model,
		Messages: messages,
		Stream:   respReq.Stream,
	}
	if respReq.Stream {
		chatReq.ExtraBody = map[string]any{
			"stream_options": map[string]any{"include_usage": true},
		}
	}
	if respReq.Temperature != nil {
		chatReq.Temperature = respReq.Temperature
	}
	if respReq.MaxTokens != nil {
		chatReq.MaxTokens = respReq.MaxTokens
	}
	if respReq.TopP != nil {
		chatReq.TopP = respReq.TopP
	}
	if len(respReq.Tools) > 0 {
		chatReq.Tools = convertResponsesTools(respReq.Tools)
	}
	if respReq.ToolChoice != nil {
		chatReq.ToolChoice = convertResponsesToolChoice(respReq.ToolChoice)
	}
	if respReq.ParallelToolCalls != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["parallel_tool_calls"] = *respReq.ParallelToolCalls
	}
	if respReq.Stop != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["stop"] = respReq.Stop
	}
	if respReq.FrequencyPenalty != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["frequency_penalty"] = *respReq.FrequencyPenalty
	}
	if respReq.PresencePenalty != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["presence_penalty"] = *respReq.PresencePenalty
	}
	if respReq.User != "" {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["user"] = respReq.User
	}
	if respReq.StreamOptions != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		streamOptions, ok := respReq.StreamOptions.(map[string]any)
		if !ok {
			streamOptions = map[string]any{}
		}
		if _, exists := streamOptions["include_usage"]; !exists && respReq.Stream {
			streamOptions["include_usage"] = true
		}
		chatReq.ExtraBody["stream_options"] = streamOptions
	}
	// 将 Responses API reasoning.effort 映射到 Chat Completions
	if !getForceDisableThinking() && respReq.Reasoning.Effort != "" {
		if respReq.Reasoning.Effort != "none" {
			chatReq.ReasoningEffort = respReq.Reasoning.Effort
		}
	}

	wantReasoning := !getForceDisableThinking()
	chatReq.Messages = fixToolCallGaps(chatReq.Messages)
	keepReasoning := wantsReasoning(&chatReq)
	chatReq.Messages = ensureReasoningContent(chatReq.Messages, keepReasoning)

	effortIn := chatReq.ReasoningEffort
	if effortIn == "" {
		effortIn = respReq.Reasoning.Effort
	}
	upstreamSurface := "zen"
	if auth.shouldUseGoEndpoint(chatReq.Model) {
		upstreamSurface = "go"
	}
	logRequestPlan(r.Context(), map[string]any{
		"protocol":             "responses",
		"model_in":             modelIn,
		"model_resolved":       chatReq.Model,
		"auth_mode":            authModeString(auth.Mode),
		"auth_source":          auth.Source,
		"has_key":              auth.Token != "",
		"upstream_surface":     upstreamSurface,
		"stream":               respReq.Stream,
		"keep_reasoning":       keepReasoning,
		"thinking":             thinkingState(nil),
		"reasoning_effort_in":  effortIn,
		"reasoning_effort_out": mappedReasoningEffort(effortIn),
		"tools_count":          len(respReq.Tools),
		"messages_count":       len(chatReq.Messages),
	})

	upstreamBody := buildUpstreamBody(&chatReq)

	if respReq.Stream {
		clCtx := beginCallLog(r.Context(), r.URL.Path, chatReq.Model, true, authModeString(auth.Mode))
		upResp, status, _, err := callOpenCodeAPIStream(clCtx, upstreamBody, chatReq.Model, auth)
		if err != nil || status < 200 || status >= 300 {
			callLogFinish(clCtx, status, fmt.Sprintf("upstream http %d", status), 0, 0, 0, 0)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if upResp != nil {
				errBody, _ := io.ReadAll(upResp)
				if len(errBody) > 0 {
					w.Write(errBody)
					return
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error"}})
			return
		}
		defer upResp.Close()

		resp := &http.Response{
			StatusCode: status,
			Body:       upResp,
			Header:     make(http.Header),
		}
		rpt, rct, rcc, rcr := responsesStreamHandler(w, r, resp, chatReq.Model, chatReq.Model, wantReasoning, respReq.Tools, respReq.ToolChoice, respReq)
		callLogFinish(clCtx, http.StatusOK, "", rpt, rct, rcc, rcr)
		return
	}

	clCtx := beginCallLog(r.Context(), r.URL.Path, chatReq.Model, false, authModeString(auth.Mode))
	respBody, status, _, err := callOpenCodeAPI(clCtx, upstreamBody, chatReq.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		callLogFinish(clCtx, status, fmt.Sprintf("upstream http %d", status), 0, 0, 0, 0)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if len(respBody) > 0 {
			w.Write(respBody)
		} else {
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error"}})
		}
		return
	}

	responsesBody := convertChatToResponses(respBody, chatReq.Model, wantReasoning, respReq.Tools, respReq.ToolChoice)
	var responseMap map[string]any
	if json.Unmarshal(responsesBody, &responseMap) == nil {
		applyResponsesRequestEcho(responseMap, respReq)
		if enriched, marshalErr := json.Marshal(responseMap); marshalErr == nil {
			responsesBody = enriched
		}
		storeResponseState(responseMap, respReq)
	}

	result := summarizeChatResult(respBody)
	logRequestResult(r.Context(), result)

	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			pt, _ := u["prompt_tokens"].(float64)
			ct, _ := u["completion_tokens"].(float64)
			tt, _ := u["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(chatReq.Model, int64(pt), int64(ct), int64(tt))
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	maybeLogBodySummary(r.Context(), "responses response body", responsesBody)
	clPt, clCt, clCc, clCr := usageFromOpenAIBody(respBody)
	callLogFinish(clCtx, http.StatusOK, "", clPt, clCt, clCc, clCr)
	w.Write(responsesBody)
}

// ======================== Responses Stream Handler ========================

// responsesStreamHandler 将上游 OpenAI 流转换为 Responses SSE 流，返回最终 输入/输出/缓存创建/缓存读取 token。
func responsesStreamHandler(w http.ResponseWriter, r *http.Request, resp *http.Response, model string, _ string, wantReasoning bool, tools []ResponsesTool, toolChoice any, originalReq ResponsesAPIRequest) (pt, ct, cc, cr int64) {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(resp.Body)
	stats := &streamResultStats{start: time.Now()}

	responseID := "resp_" + time.Now().Format("20060102150405") + "_" + randomString(8)
	reasoningID := "rs_" + responseID
	msgID := "msg_" + responseID + "_0"
	createdAt := time.Now().Unix()
	seq := 0

	reasoningStarted := false
	reasoningDone := false
	messageStarted := false
	messageDone := false
	fullReasoning := ""
	fullText := ""
	totalUsage := map[string]any{}
	createdSent := false
	terminalStatus := "completed"
	terminalEvent := "response.completed"
	itemStatus := "completed"
	toolCalls := map[int]map[string]any{}
	toolOrder := []int{}
	toolKinds := responsesToolKindMap(tools)
	indexAllocator := outputIndexAllocator{}
	reasoningOutputIndex := -1
	messageIndex := -1

	defer func() {
		stats.textChars = len(fullText)
		stats.reasoningChars = len(fullReasoning)
		stats.toolCallCount = len(toolOrder)
		stats.log(ctx, "responses")
	}()

	messageOutputIndex := func() int {
		if messageIndex < 0 {
			messageIndex = indexAllocator.Allocate()
		}
		return messageIndex
	}

	reasoningItem := func(status string) map[string]any {
		item := map[string]any{
			"id":      reasoningID,
			"type":    "reasoning",
			"summary": []any{},
		}
		if status != "" {
			item["status"] = status
		}
		if status == "completed" {
			item["encrypted_content"] = ""
		}
		if fullReasoning != "" {
			item["summary"] = []any{map[string]any{"type": "summary_text", "text": fullReasoning}}
		}
		return item
	}

	messageItem := func(status string) map[string]any {
		content := []any{map[string]any{
			"type":        "output_text",
			"annotations": []any{},
			"logprobs":    []any{},
			"text":        fullText,
		}}
		return map[string]any{
			"id":      msgID,
			"type":    "message",
			"status":  status,
			"content": content,
			"role":    "assistant",
		}
	}

	emitReasoningDone := func() {
		if !reasoningStarted || reasoningDone {
			return
		}
		seq++
		emitSSEEvent(w, flusher, "response.reasoning_summary_text.done", map[string]any{
			"type":            "response.reasoning_summary_text.done",
			"sequence_number": seq,
			"item_id":         reasoningID,
			"output_index":    reasoningOutputIndex,
			"summary_index":   0,
			"text":            fullReasoning,
		})
		seq++
		emitSSEEvent(w, flusher, "response.reasoning_summary_part.done", map[string]any{
			"type":            "response.reasoning_summary_part.done",
			"sequence_number": seq,
			"item_id":         reasoningID,
			"output_index":    reasoningOutputIndex,
			"summary_index":   0,
			"part":            map[string]any{"type": "summary_text", "text": fullReasoning},
		})
		seq++
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    reasoningOutputIndex,
			"item":            reasoningItem(itemStatus),
		})
		reasoningDone = true
	}

	emitMessageDone := func() {
		if !messageStarted || messageDone {
			return
		}
		idx := messageOutputIndex()
		seq++
		emitSSEEvent(w, flusher, "response.output_text.done", map[string]any{
			"type":            "response.output_text.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"text":            fullText,
			"logprobs":        []any{},
		})
		seq++
		emitSSEEvent(w, flusher, "response.content_part.done", map[string]any{
			"type":            "response.content_part.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": fullText},
		})
		seq++
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            messageItem(itemStatus),
		})
		messageDone = true
	}

	emitToolCallDone := func(idx int, call map[string]any) {
		if done, _ := call["done"].(bool); done {
			return
		}
		call["done"] = true
		itemID, _ := call["item_id"].(string)
		callID, _ := call["call_id"].(string)
		name, _ := call["name"].(string)
		args, _ := call["arguments"].(string)
		seq++
		emitSSEEvent(w, flusher, "response.function_call_arguments.done", map[string]any{
			"type":            "response.function_call_arguments.done",
			"sequence_number": seq,
			"item_id":         itemID,
			"output_index":    idx,
			"name":            name,
			"arguments":       args,
		})
		seq++
		itemType, _ := call["item_type"].(string)
		if itemType == "" {
			itemType = "function_call"
		}
		item := buildResponseToolCallItem(ToolCall{ID: callID, Function: FunctionCall{Name: name, Arguments: args}}, itemType)
		item["status"] = itemStatus
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            item,
		})
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			reqLogger(ctx).Error("stream read error", "error", err)
			return
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
			stats.doneSeen = true
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(line[6:]), &chunk); err != nil {
			continue
		}
		stats.noteChunk()
		if !createdSent {
			if id, ok := chunk["id"].(string); ok && id != "" {
				responseID = id
				reasoningID = "rs_" + responseID + "_0"
				msgID = "msg_" + responseID + "_0"
			}
			if created, ok := chunk["created"].(float64); ok {
				createdAt = int64(created)
			}
			seq++
			emitSSEEvent(w, flusher, "response.created", map[string]any{
				"type":            "response.created",
				"sequence_number": seq,
				"response":        map[string]any{"id": responseID, "object": "response", "created_at": createdAt, "status": "in_progress", "background": false, "error": nil, "output": []any{}},
			})
			seq++
			emitSSEEvent(w, flusher, "response.in_progress", map[string]any{
				"type":            "response.in_progress",
				"sequence_number": seq,
				"response":        map[string]any{"id": responseID, "object": "response", "created_at": createdAt, "status": "in_progress"},
			})
			createdSent = true
		}
		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			if usage, ok := chunk["usage"].(map[string]any); ok {
				totalUsage = usage
			}
			continue
		}

		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		finishReason, _ := choice["finish_reason"].(string)
		if finishReason != "" {
			stats.finishReason = finishReason
			stats.sawFinish = true
		}

		if rc, ok := delta["reasoning_content"]; ok && wantReasoning {
			rcStr, _ := rc.(string)
			if rcStr != "" {
				if !reasoningStarted {
					reasoningOutputIndex = indexAllocator.Allocate()
					seq++
					emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
						"type":            "response.output_item.added",
						"sequence_number": seq,
						"output_index":    reasoningOutputIndex,
						"item":            reasoningItem("in_progress"),
					})
					seq++
					emitSSEEvent(w, flusher, "response.reasoning_summary_part.added", map[string]any{
						"type":            "response.reasoning_summary_part.added",
						"sequence_number": seq,
						"item_id":         reasoningID,
						"output_index":    reasoningOutputIndex,
						"summary_index":   0,
						"part":            map[string]any{"type": "summary_text", "text": ""},
					})
					reasoningStarted = true
				}
				fullReasoning += rcStr
				seq++
				emitSSEEvent(w, flusher, "response.reasoning_summary_text.delta", map[string]any{
					"type":            "response.reasoning_summary_text.delta",
					"sequence_number": seq,
					"item_id":         reasoningID,
					"output_index":    reasoningOutputIndex,
					"summary_index":   0,
					"delta":           rcStr,
				})
			}
		}

		contentStr := ""
		if c, ok := delta["content"]; ok && c != nil {
			contentStr, _ = c.(string)
		}
		// #37635: when thinking is not kept, promote misplaced reasoning to visible text.
		if contentStr == "" && !wantReasoning {
			if rc, ok := delta["reasoning_content"].(string); ok {
				if rc != "" {
					stats.promotedReasoning = true
				}
				contentStr = rc
			}
		}
		if contentStr != "" {
			// The terminal finish reason determines the item's final status. Keep the
			// reasoning item open until that reason is known so a truncation cannot
			// first announce it as completed.
			if !messageStarted {
				idx := messageOutputIndex()
				seq++
				emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
					"type":            "response.output_item.added",
					"sequence_number": seq,
					"output_index":    idx,
					"item":            map[string]any{"id": msgID, "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"},
				})
				seq++
				emitSSEEvent(w, flusher, "response.content_part.added", map[string]any{
					"type":            "response.content_part.added",
					"sequence_number": seq,
					"item_id":         msgID,
					"output_index":    idx,
					"content_index":   0,
					"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": ""},
				})
				messageStarted = true
			}
			fullText += contentStr
			seq++
			emitSSEEvent(w, flusher, "response.output_text.delta", map[string]any{
				"type":            "response.output_text.delta",
				"sequence_number": seq,
				"item_id":         msgID,
				"output_index":    messageOutputIndex(),
				"content_index":   0,
				"delta":           contentStr,
				"logprobs":        []any{},
			})
		}

		rawToolCalls, _ := delta["tool_calls"].([]any)
		for _, rawToolCall := range rawToolCalls {
			tc, ok := rawToolCall.(map[string]any)
			if !ok {
				continue
			}
			idxFloat, _ := tc["index"].(float64)
			upstreamIndex := int(idxFloat)
			call, exists := toolCalls[upstreamIndex]
			if !exists {
				outputIndex := indexAllocator.Allocate()
				callID, _ := tc["id"].(string)
				if callID == "" {
					callID = "call_" + randomString(12)
				}
				fn, _ := tc["function"].(map[string]any)
				name, _ := fn["name"].(string)
				itemType := toolCallOutputType(name, toolKinds)
				call = map[string]any{
					"output_index": outputIndex,
					"item_id":      "fc_" + callID,
					"call_id":      callID,
					"name":         name,
					"arguments":    "",
					"done":         false,
					"item_type":    itemType,
				}
				toolCalls[upstreamIndex] = call
				toolOrder = append(toolOrder, upstreamIndex)
				seq++
				emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
					"type":            "response.output_item.added",
					"sequence_number": seq,
					"output_index":    outputIndex,
					"item": map[string]any{
						"id":        call["item_id"],
						"type":      itemType,
						"status":    "in_progress",
						"arguments": "",
						"call_id":   callID,
						"name":      name,
					},
				})
			}
			fn, _ := tc["function"].(map[string]any)
			if name, _ := fn["name"].(string); name != "" {
				call["name"] = name
				if call["item_type"] == "function_call" {
					call["item_type"] = toolCallOutputType(name, toolKinds)
				}
			}
			if argDelta, _ := fn["arguments"].(string); argDelta != "" {
				call["arguments"] = call["arguments"].(string) + argDelta
				seq++
				emitSSEEvent(w, flusher, "response.function_call_arguments.delta", map[string]any{
					"type":            "response.function_call_arguments.delta",
					"sequence_number": seq,
					"item_id":         call["item_id"],
					"output_index":    call["output_index"],
					"delta":           argDelta,
				})
			}
		}

		if usage, ok := chunk["usage"].(map[string]any); ok {
			totalUsage = usage
		}
		if finishReason == "stop" || finishReason == "length" || finishReason == "content_filter" {
			if finishReason == "length" {
				terminalStatus = "incomplete"
				terminalEvent = "response.incomplete"
				itemStatus = "incomplete"
			}
			emitReasoningDone()
			if !messageStarted && len(toolCalls) == 0 {
				idx := messageOutputIndex()
				seq++
				emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
					"type":            "response.output_item.added",
					"sequence_number": seq,
					"output_index":    idx,
					"item":            map[string]any{"id": msgID, "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"},
				})
				seq++
				emitSSEEvent(w, flusher, "response.content_part.added", map[string]any{
					"type":            "response.content_part.added",
					"sequence_number": seq,
					"item_id":         msgID,
					"output_index":    idx,
					"content_index":   0,
					"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": ""},
				})
				messageStarted = true
			}
			emitMessageDone()
			for _, idx := range toolOrder {
				emitToolCallDone(toolCalls[idx]["output_index"].(int), toolCalls[idx])
			}
		}
	}

	emitReasoningDone()
	emitMessageDone()
	for _, idx := range toolOrder {
		emitToolCallDone(toolCalls[idx]["output_index"].(int), toolCalls[idx])
	}

	output := make([]any, indexAllocator.Len())
	if reasoningStarted {
		output[reasoningOutputIndex] = reasoningItem(itemStatus)
	}
	if messageStarted {
		output[messageIndex] = messageItem(itemStatus)
	}
	for _, idx := range toolOrder {
		call := toolCalls[idx]
		itemType, _ := call["item_type"].(string)
		if itemType == "" {
			itemType = "function_call"
		}
		item := buildResponseToolCallItem(ToolCall{
			ID: call["call_id"].(string),
			Function: FunctionCall{
				Name:      call["name"].(string),
				Arguments: call["arguments"].(string),
			},
		}, itemType)
		item["status"] = itemStatus
		output[call["output_index"].(int)] = item
	}

	completedResponse := map[string]any{
		"id":                 responseID,
		"object":             "response",
		"created_at":         createdAt,
		"status":             terminalStatus,
		"background":         false,
		"error":              nil,
		"incomplete_details": nil,
		"model":              model,
		"output":             output,
	}
	if terminalStatus == "incomplete" {
		completedResponse["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	applyResponsesRequestEcho(completedResponse, originalReq)
	if len(tools) > 0 {
		completedResponse["tools"] = tools
	}
	if toolChoice != nil {
		completedResponse["tool_choice"] = toolChoice
	}

	if len(totalUsage) > 0 {
		usage := map[string]any{}
		if v, ok := totalUsage["prompt_tokens"]; ok {
			usage["input_tokens"] = v
		}
		if v, ok := totalUsage["prompt_tokens_details"]; ok {
			usage["input_tokens_details"] = v
		} else {
			usage["input_tokens_details"] = map[string]any{"cached_tokens": 0}
		}
		if v, ok := totalUsage["completion_tokens"]; ok {
			usage["output_tokens"] = v
		}
		if v, ok := totalUsage["completion_tokens_details"]; ok {
			usage["output_tokens_details"] = v
		}
		if v, ok := totalUsage["total_tokens"]; ok {
			usage["total_tokens"] = v
		}
		if v, ok := totalUsage["input_tokens"]; ok && usage["input_tokens"] == nil {
			usage["input_tokens"] = v
		}
		if v, ok := totalUsage["output_tokens"]; ok && usage["output_tokens"] == nil {
			usage["output_tokens"] = v
		}
		completedResponse["usage"] = usage
	}

	if totalUsage != nil {
		lpt, lct, lcc, lcr := usageFromMap(totalUsage)
		tt, _ := totalUsage["total_tokens"].(float64)
		if tt > 0 {
			recordTokenUsage(model, lpt, lct, int64(tt))
		}
		pt, ct, cc, cr = lpt, lct, lcc, lcr
	}

	seq++
	emitSSEEvent(w, flusher, terminalEvent, map[string]any{
		"type":            terminalEvent,
		"sequence_number": seq,
		"response":        completedResponse,
	})

	if flusher != nil {
		flusher.Flush()
	}
	storeResponseState(completedResponse, originalReq)
	return
}

func convertChatToResponses(chatBody []byte, model string, wantReasoning bool, tools []ResponsesTool, toolChoice any) []byte {
	var chat struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          any        `json:"content"`
				Refusal          string     `json:"refusal"`
				ReasoningContent string     `json:"reasoning_content"`
				ToolCalls        []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		slog.Warn("convertChatToResponses unmarshal failed", "error", err)
	}

	reasoning := ""
	finishReason := ""
	var toolCalls []ToolCall
	messageContent := []any(nil)
	toolKinds := responsesToolKindMap(tools)
	if len(chat.Choices) > 0 {
		messageContent, _ = chatContentToResponsesContent(chat.Choices[0].Message.Content)
		if refusal := chat.Choices[0].Message.Refusal; refusal != "" {
			messageContent = []any{map[string]any{"type": "refusal", "refusal": refusal}}
		}
		rc := chat.Choices[0].Message.ReasoningContent
		if wantReasoning {
			reasoning = rc
		}
		toolCalls = chat.Choices[0].Message.ToolCalls
		finishReason = chat.Choices[0].FinishReason
		if len(messageContent) == 0 && rc != "" && len(toolCalls) == 0 {
			messageContent, _ = chatContentToResponsesContent(rc)
		}
	}

	outcome := responsesOutcome(finishReason)
	status := outcome.Status
	responses := map[string]any{
		"id":                 chat.ID,
		"object":             "response",
		"status":             status,
		"background":         false,
		"error":              nil,
		"incomplete_details": outcome.IncompleteDetails,
		"model":              model,
		"created_at":         chat.Created,
	}
	if len(tools) > 0 {
		responses["tools"] = tools
	}
	if toolChoice != nil {
		responses["tool_choice"] = toolChoice
	}
	outputID := "msg_" + chat.ID + "_0"
	output := []any{}
	if reasoning != "" {
		output = append(output, map[string]any{
			"id":                "rs_" + chat.ID,
			"type":              "reasoning",
			"encrypted_content": "",
			"summary":           []any{map[string]any{"type": "summary_text", "text": reasoning}},
		})
	}
	if len(messageContent) > 0 {
		output = append(output, map[string]any{
			"id":      outputID,
			"type":    "message",
			"status":  status,
			"role":    "assistant",
			"content": messageContent,
		})
	}
	for _, tc := range toolCalls {
		item := buildResponseToolCallItem(tc, toolCallOutputType(tc.Function.Name, toolKinds))
		item["status"] = status
		output = append(output, item)
	}
	responses["output"] = output
	if chat.Usage != nil {
		usage := map[string]any{}
		if v, ok := chat.Usage["prompt_tokens"]; ok {
			usage["input_tokens"] = v
		}
		if v, ok := chat.Usage["prompt_tokens_details"]; ok {
			usage["input_tokens_details"] = v
		} else {
			usage["input_tokens_details"] = map[string]any{"cached_tokens": 0}
		}
		if v, ok := chat.Usage["completion_tokens"]; ok {
			usage["output_tokens"] = v
		}
		if v, ok := chat.Usage["completion_tokens_details"]; ok {
			usage["output_tokens_details"] = v
		}
		if v, ok := chat.Usage["total_tokens"]; ok {
			usage["total_tokens"] = v
		}
		if v, ok := chat.Usage["input_tokens"]; ok && usage["input_tokens"] == nil {
			usage["input_tokens"] = v
		}
		if v, ok := chat.Usage["output_tokens"]; ok && usage["output_tokens"] == nil {
			usage["output_tokens"] = v
		}
		responses["usage"] = usage
	}

	result, _ := json.Marshal(responses)
	return result
}

func emitSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, data map[string]any) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error("marshal SSE event failed", "error", err)
		return
	}
	w.Write([]byte("event: " + event + "\n"))
	w.Write([]byte("data: " + string(jsonData) + "\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// ======================== Admin 管理页面 ========================

func reloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	refreshOCSession()
	fetched, err := fetchModels()
	if err == nil && len(fetched) > 0 {
		modelMu.Lock()
		modelsCache = fetched
		modelsLoaded = true
		modelMu.Unlock()
		slog.Info("free models refreshed", "count", len(fetched))
	}
	goFetched, goErr := fetchGoModels()
	if goErr == nil && len(goFetched) > 0 {
		modelMu.Lock()
		goModelsCache = goFetched
		modelMu.Unlock()
		slog.Info("go catalog refreshed", "count", len(goFetched))
	}
	refreshModelsDevCatalog(false)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"session": ocSessionID,
		"models":  len(modelsCache),
		"free":    len(modelsCache),
		"go":      len(goModelsCache),
	})

}
func adminConfigHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		configMu.RLock()
		cfg := AppConfig{ModelAlias: modelAlias, ReasoningEffortMap: reasoningEffortMap, ForceDisableThinking: forceDisableThinking}
		configMu.RUnlock()
		socks5Mu.RLock()
		cfg.Socks5Proxies = socks5Proxies
		cfg.ActiveSocks5 = activeSocks5
		cfg.Socks5PaidDirect = socks5PaidDirect
		socks5Mu.RUnlock()
		merged := mergeAppConfig(loadConfig(configPath), cfg)
		// 配额相关字段未配置时回显默认值，保证面板文本框直接可见生效值。
		quotaResp := merged.QuotaErrorSignals
		if len(quotaResp.ErrorTypes) == 0 {
			quotaResp.ErrorTypes = defaultQuotaErrorTypes()
		}
		if len(quotaResp.MessageKeywords) == 0 {
			quotaResp.MessageKeywords = defaultQuotaMessageKeywords()
		}
		maxSwitches := merged.MaxQuotaNodeSwitches
		if maxSwitches <= 0 {
			maxSwitches = defaultMaxQuotaNodeSwitches
		}
		cooldownExh := merged.NodeCooldownExhaustedHours
		if cooldownExh <= 0 {
			cooldownExh = 1 // 默认 1h（与 effectiveExhaustedCooldown 一致）
		}
		cooldownDead := merged.NodeCooldownDeadMinutes
		if cooldownDead <= 0 {
			cooldownDead = 1 // 默认 1min（与 effectiveDeadCooldown 一致）
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model_alias":                   merged.ModelAlias,
			"reasoning_effort_map":          merged.ReasoningEffortMap,
			"force_disable_thinking":        merged.ForceDisableThinking,
			"socks5_proxies":                merged.Socks5Proxies,
			"active_socks5":                 merged.ActiveSocks5,
			"socks5_paid_direct":            merged.Socks5PaidDirect,
			"subscriptions":                 merged.Subscriptions,
			"webshare":                      merged.Webshare,
			"manual_nodes":                  merged.ManualNodes,
			"quota_error_signals":           quotaResp,
			"max_quota_node_switches":       maxSwitches,
			"node_cooldown_exhausted_hours": cooldownExh,
			"node_cooldown_dead_minutes":    cooldownDead,
			"node_health_interval_minutes":  merged.NodeHealthIntervalMinutes,
			"node_health_probe_url":         merged.NodeHealthProbeURL,
			"api_key":                       merged.ApiKey,
			"log_level":                     getLogLevelString(),
			"log_bodies":                    getLogBodies(),
		})
	case http.MethodPost:
		var payload struct {
			configPatch
			LogLevel  *string `json:"log_level,omitempty"`
			LogBodies *bool   `json:"log_bodies,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
			return
		}
		// 合并保存：请求缺省的字段保留文件现存值；显式零值（false/""/0）可清零。
		next := mergeConfigPatch(loadConfig(configPath), payload.configPatch)
		if err := saveConfig(configPath, next); err != nil {
			http.Error(w, `{"error":"Failed to save config"}`, http.StatusInternalServerError)
			return
		}
		applyConfig(next)
		if payload.LogLevel != nil {
			setLogLevelString(*payload.LogLevel)
		}
		if payload.LogBodies != nil {
			setLogBodies(*payload.LogBodies)
		}
		if debugMode {
			slog.Info("config updated",
				"aliases", len(next.ModelAlias),
				"effort_map", len(next.ReasoningEffortMap),
				"subscriptions", len(next.Subscriptions),
				"webshare", len(next.Webshare),
				"manual_nodes", len(next.ManualNodes),
				"force_disable", next.ForceDisableThinking,
				"log_level", getLogLevelString(),
				"log_bodies", getLogBodies(),
			)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// mergeAppConfig 将补丁叠加到基础配置：请求里为零值/空集合的字段保留基础值。
// configPatch 是面板提交的配置补丁：指针字段区分"缺省"与"显式零值"，
// 使 false / 空串 / 0 也能被保存（清零语义）。
type configPatch struct {
	ModelAlias                 map[string]string    `json:"model_alias"`
	ReasoningEffortMap         map[string]string    `json:"reasoning_effort_map"`
	ForceDisableThinking       *bool                `json:"force_disable_thinking"`
	Socks5Proxies              []Socks5Proxy        `json:"socks5_proxies"`
	ActiveSocks5               *string              `json:"active_socks5"`
	Socks5PaidDirect           *bool                `json:"socks5_paid_direct"`
	Subscriptions              []SubscriptionConfig `json:"subscriptions"`
	Webshare                   []WebshareConfig     `json:"webshare"`
	ManualNodes                []ProxyNodeConfig    `json:"manual_nodes"`
	QuotaErrorSignals          QuotaSignalsConfig   `json:"quota_error_signals"`
	MaxQuotaNodeSwitches       *int                 `json:"max_quota_node_switches"`
	NodeCooldownExhaustedHours *int                 `json:"node_cooldown_exhausted_hours"`
	NodeCooldownDeadMinutes    *int                 `json:"node_cooldown_dead_minutes"`
	NodeHealthIntervalMinutes  *int                 `json:"node_health_interval_minutes"`
	NodeHealthProbeURL         *string              `json:"node_health_probe_url"`
	ApiKey                     *string              `json:"api_key"`
}

// mergeAppConfig 将补丁叠加到基础配置（GET 回显与订阅保存共用，AppConfig 语义）。
func mergeAppConfig(base, patch AppConfig) AppConfig {
	out := base
	if patch.ModelAlias != nil {
		out.ModelAlias = patch.ModelAlias
	}
	if patch.ReasoningEffortMap != nil {
		out.ReasoningEffortMap = patch.ReasoningEffortMap
	}
	if patch.Socks5Proxies != nil {
		out.Socks5Proxies = patch.Socks5Proxies
	}
	if patch.Subscriptions != nil {
		out.Subscriptions = patch.Subscriptions
	}
	if patch.Webshare != nil {
		out.Webshare = patch.Webshare
	}
	if patch.ManualNodes != nil {
		out.ManualNodes = patch.ManualNodes
	}
	if patch.QuotaErrorSignals.ErrorTypes != nil {
		out.QuotaErrorSignals.ErrorTypes = patch.QuotaErrorSignals.ErrorTypes
	}
	if patch.QuotaErrorSignals.MessageKeywords != nil {
		out.QuotaErrorSignals.MessageKeywords = patch.QuotaErrorSignals.MessageKeywords
	}
	if patch.Socks5PaidDirect {
		out.Socks5PaidDirect = true
	}
	if patch.ActiveSocks5 != "" {
		out.ActiveSocks5 = patch.ActiveSocks5
	}
	if patch.ForceDisableThinking {
		out.ForceDisableThinking = true
	}
	if patch.MaxQuotaNodeSwitches > 0 {
		out.MaxQuotaNodeSwitches = patch.MaxQuotaNodeSwitches
	}
	if patch.NodeCooldownExhaustedHours > 0 {
		out.NodeCooldownExhaustedHours = patch.NodeCooldownExhaustedHours
	}
	if patch.NodeCooldownDeadMinutes > 0 {
		out.NodeCooldownDeadMinutes = patch.NodeCooldownDeadMinutes
	}
	if patch.NodeHealthIntervalMinutes > 0 {
		out.NodeHealthIntervalMinutes = patch.NodeHealthIntervalMinutes
	}
	if patch.NodeHealthProbeURL != "" {
		out.NodeHealthProbeURL = patch.NodeHealthProbeURL
	}
	return out
}

// mergeConfigPatch 面板 POST：指针字段非 nil 即显式覆盖（含清零）。
func mergeConfigPatch(base AppConfig, patch configPatch) AppConfig {
	out := base
	if patch.ModelAlias != nil {
		out.ModelAlias = patch.ModelAlias
	}
	if patch.ReasoningEffortMap != nil {
		out.ReasoningEffortMap = patch.ReasoningEffortMap
	}
	if patch.Socks5Proxies != nil {
		out.Socks5Proxies = patch.Socks5Proxies
	}
	if patch.Subscriptions != nil {
		out.Subscriptions = patch.Subscriptions
	}
	if patch.Webshare != nil {
		out.Webshare = patch.Webshare
	}
	if patch.ManualNodes != nil {
		out.ManualNodes = patch.ManualNodes
	}
	if patch.QuotaErrorSignals.ErrorTypes != nil {
		out.QuotaErrorSignals.ErrorTypes = patch.QuotaErrorSignals.ErrorTypes
	}
	if patch.QuotaErrorSignals.MessageKeywords != nil {
		out.QuotaErrorSignals.MessageKeywords = patch.QuotaErrorSignals.MessageKeywords
	}
	if patch.ForceDisableThinking != nil {
		out.ForceDisableThinking = *patch.ForceDisableThinking
	}
	if patch.ActiveSocks5 != nil {
		out.ActiveSocks5 = *patch.ActiveSocks5
	}
	if patch.Socks5PaidDirect != nil {
		out.Socks5PaidDirect = *patch.Socks5PaidDirect
	}
	if patch.MaxQuotaNodeSwitches != nil {
		out.MaxQuotaNodeSwitches = *patch.MaxQuotaNodeSwitches
	}
	if patch.NodeCooldownExhaustedHours != nil {
		out.NodeCooldownExhaustedHours = *patch.NodeCooldownExhaustedHours
	}
	if patch.NodeCooldownDeadMinutes != nil {
		out.NodeCooldownDeadMinutes = *patch.NodeCooldownDeadMinutes
	}
	if patch.NodeHealthIntervalMinutes != nil {
		out.NodeHealthIntervalMinutes = *patch.NodeHealthIntervalMinutes
	}
	if patch.NodeHealthProbeURL != nil {
		out.NodeHealthProbeURL = *patch.NodeHealthProbeURL
	}
	if patch.ApiKey != nil {
		out.ApiKey = *patch.ApiKey
	}
	return out
}

// nodeAdminView API 视图：只读字段 + 当前/手动标记 + 健康探测结果。
type nodeAdminView struct {
	Name          string `json:"name"`
	Protocol      string `json:"protocol"`
	Address       string `json:"address"`
	Port          int    `json:"port"`
	Fingerprint   string `json:"fingerprint"`
	State         string `json:"state"`
	MarkedAt      string `json:"marked_at,omitempty"`
	CooldownUntil string `json:"cooldown_until,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastUsedAt    string `json:"last_used_at,omitempty"`
	LatencyMs     int64  `json:"latency_ms,omitempty"`
	LastProbeAt   string `json:"last_probe_at,omitempty"`
	Active        bool   `json:"active"`
	Manual        bool   `json:"manual"`
}

func adminNodesHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		pick, manual := proxyPool.pickState()
		nodes := proxyPool.snapshot()
		activeName, manualName := "", ""
		out := make([]nodeAdminView, 0, len(nodes))
		health := map[string]int{"available": 0, "exhausted": 0, "dead": 0}
		for _, n := range nodes {
			if n.Fingerprint == pick {
				activeName = n.Name
			}
			if n.Fingerprint == manual {
				manualName = n.Name
			}
			v := nodeAdminView{
				Name: n.Name, Protocol: n.Protocol, Address: n.Address, Port: n.Port,
				Fingerprint: n.Fingerprint, State: n.State.String(),
				LastError: n.LastError, Active: n.Fingerprint == pick,
				Manual: n.Fingerprint == manual, LatencyMs: n.LatencyMs,
			}
			health[v.State]++
			if !n.MarkedAt.IsZero() {
				v.MarkedAt = n.MarkedAt.Format(time.RFC3339)
			}
			if !n.CooldownUntil.IsZero() {
				v.CooldownUntil = n.CooldownUntil.Format(time.RFC3339)
			}
			if !n.LastUsedAt.IsZero() {
				v.LastUsedAt = n.LastUsedAt.Format(time.RFC3339)
			}
			if !n.LastProbeAt.IsZero() {
				v.LastProbeAt = n.LastProbeAt.Format(time.RFC3339)
			}
			out = append(out, v)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"active_fp":       pick,
			"active_name":     activeName,
			"manual_fp":       manual,
			"manual_name":     manualName,
			"healthy":         health["available"],
			"exhausted_count": health["exhausted"],
			"dead_count":      health["dead"],
			"nodes":           out,
			"subscriptions":   subManager.snapshot(),
		})
	case http.MethodPost:
		var payload struct {
			Action      string               `json:"action"`
			Fingerprint string               `json:"fingerprint"`
			Subs        []SubscriptionConfig `json:"subscriptions"`
			Webshare    []WebshareConfig     `json:"webshare"`
			Nodes       []ProxyNodeConfig    `json:"manual_nodes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
			return
		}
		resp := map[string]string{"status": "ok", "action": payload.Action}
		switch payload.Action {
		case "switch":
			resp["active_fp"] = proxyPool.manual(payload.Fingerprint)
		case "reset":
			if !proxyPool.unmark(payload.Fingerprint) {
				http.Error(w, `{"error":"node not found"}`, http.StatusNotFound)
				return
			}
		case "reset_all":
			for _, n := range proxyPool.snapshot() {
				proxyPool.unmark(n.Fingerprint)
			}
		case "reload":
			ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
			defer cancel()
			n, err := subManager.refreshAll(ctx, true)
			if err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadGateway)
				return
			}
			resp["nodes"] = fmt.Sprintf("%d", n)
		case "probe":
			// 手动测速：同步执行真实探测并更新节点状态。
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
			checked := proxyPool.checkNodes(ctx)
			cancel()
			resp["checked"] = fmt.Sprintf("%d", checked)
		case "save":
			// 面板保存订阅源 + webshare 源 + 手配节点：合并写盘 → 生效 → 立即刷新池
			next := mergeAppConfig(loadConfig(configPath), AppConfig{
				Subscriptions: payload.Subs,
				Webshare:      payload.Webshare,
				ManualNodes:   payload.Nodes,
			})
			if err := saveConfig(configPath, next); err != nil {
				http.Error(w, `{"error":"Failed to save config"}`, http.StatusInternalServerError)
				return
			}
			applyConfig(next)
			subManager.configure(next.Subscriptions, next.Webshare, next.ManualNodes, filepath.Join(filepath.Dir(configPath), ".subscriptions"))
			ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
			n, err := subManager.refreshAll(ctx, true)
			cancel()
			if err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadGateway)
				return
			}
			resp["nodes"] = fmt.Sprintf("%d", n)
		default:
			http.Error(w, `{"error":"unknown action"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func adminStatsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tokenStatsMu.Lock()
		data, err := json.Marshal(tokenStats)
		tokenStatsMu.Unlock()
		if err != nil {
			http.Error(w, `{"error":"marshal error"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	case http.MethodDelete:
		tokenStatsMu.Lock()
		tokenStats = &TokenStatsData{Models: map[string]*ModelStats{}}
		tokenStatsMu.Unlock()
		saveTokenStats()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// adminTrendsHandler 用量趋势：按 range=today|7d|30d 返回聚簇时间序列（来自调用日志）。
func adminTrendsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rng := r.URL.Query().Get("range")
	if rng != "7d" && rng != "30d" {
		rng = "today"
	}
	pts := trendsFromCallLog(rng)
	if pts == nil {
		pts = []TrendsPoint{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pts)
}

func adminPageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminHTML))
}

func renderLoginPage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminLoginHTML))
	if msg != "" {
		w.Write([]byte("<script>document.addEventListener('DOMContentLoaded',function(){var m=document.getElementById('login-msg');if(m){m.textContent='" + msg + "';m.style.display='block'}})</script>"))
	}
}

const adminLoginHTML = `
<!doctype html>
<html lang="zh">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>OPENCODE TO API · 管理面板</title>
<style>
/* ============================================================
   OPENCODE TO API · 管理面板 (重构版)
   设计方向：tech-utility · 深色优先 · 无外网字体依赖
   - CJK 系统字体栈（PingFang / 微软雅黑 / Noto Sans SC）
   - 语义色：accent=健康/主操作 · blue=信息 · orange=耗尽 · red=故障
   - 全部交互契约与原版一致，可直接替换 main.go 中的 adminHTML
   ============================================================ */
:root{
  --bg:#0a0f1c; --surface:#101828; --surface-2:#1a2336;
  --border:rgba(148,163,184,.14); --border-strong:rgba(148,163,184,.28);
  --line:rgba(148,163,184,.08);
  --text:#e9eef8; --text-sec:#97a3bb; --text-ter:#7b87a5;
  --accent:#6f96ff; --accent-hover:#5d88ff; --accent-dim:rgba(111,150,255,.15);
  --blue:#7ea2ff; --blue-dim:rgba(126,162,255,.12);
  --ok:#2fd489; --ok-dim:rgba(47,212,137,.13);
  --orange:#f0b04a; --orange-dim:rgba(240,176,74,.12);
  --purple:#b197f7; --purple-dim:rgba(177,151,247,.13);
  --red:#ff6b6b; --red-dim:rgba(255,107,107,.12);
  --radius:12px; --radius-sm:8px;
  --font:-apple-system,BlinkMacSystemFont,'Segoe UI','PingFang SC','Microsoft YaHei','Noto Sans SC',sans-serif;
  --mono:ui-monospace,'SF Mono','JetBrains Mono','Cascadia Code',Consolas,monospace;
  --shadow:0 10px 34px rgba(3,8,20,.55);
  --btn-text:#0a0f1e;
}
[data-theme="light"]{
  --bg:#f4f6fb; --surface:#ffffff; --surface-2:#eef1f8;
  --border:#e1e5ee; --border-strong:#c9d1e0; --line:rgba(20,30,60,.06);
  --text:#161a26; --text-sec:#59637a; --text-ter:#616d88;
  --accent:#3b63c9; --accent-hover:#3457b6; --accent-dim:rgba(59,99,201,.12);
  --blue:#3b63c9; --blue-dim:rgba(59,99,201,.1);
  --ok:#0a6f42; --ok-dim:rgba(10,111,66,.10);
  --orange:#a1530a; --orange-dim:rgba(161,83,10,.10);
  --purple:#6d4fc4; --purple-dim:rgba(109,79,196,.10);
  --red:#b42318; --red-dim:rgba(180,35,24,.09);
  --shadow:0 10px 30px rgba(30,40,70,.1);
  --btn-text:#ffffff;
}
*{margin:0;padding:0;box-sizing:border-box}
html,body{height:100%}
body{font-family:var(--font);background:var(--bg);color:var(--text);font-size:14px;line-height:1.6;-webkit-font-smoothing:antialiased;text-rendering:optimizeLegibility}
button{font-family:inherit}
input,select,textarea{font-family:inherit;color:inherit}
::selection{background:var(--accent-dim)}
:focus-visible{outline:none;box-shadow:0 0 0 2px var(--accent-dim),0 0 0 1px var(--accent)}
.muted{color:var(--text-ter)}
.mono{font-family:var(--mono)}
.num{font-variant-numeric:tabular-nums}
.lat-warn{color:var(--orange)}
.lat-high{color:var(--red)}
.empty-hint{text-align:center;color:var(--text-ter);padding:26px 0;font-size:13px}
/* 主题图标：随 data-theme 切换显示月亮/太阳 */
.ic-sun{display:none}
[data-theme="dark"] .ic-moon{display:inline-block}
[data-theme="dark"] .ic-sun{display:none}
[data-theme="light"] .ic-moon{display:none}
[data-theme="light"] .ic-sun{display:inline-block}
.theme-btn-ic{display:inline-flex;align-items:center;gap:6px}

/* ---------- 通用按钮 ---------- */
.btn{display:inline-flex;align-items:center;gap:6px;padding:7px 14px;border:1px solid transparent;border-radius:9px;font-size:13px;font-weight:600;cursor:pointer;transition:background .14s,border-color .14s,color .14s,transform .05s;white-space:nowrap;line-height:1.2}
.btn:active{transform:translateY(1px)}
.btn-primary{background:var(--accent);color:var(--btn-text)}.btn-primary:hover{background:var(--accent-hover)}
.btn-secondary{background:var(--blue);color:var(--btn-text)}.btn-secondary:hover{filter:brightness(1.07)}
.btn-ghost{background:var(--surface);color:var(--text-sec);border-color:var(--border)}.btn-ghost:hover{color:var(--text);border-color:var(--border-strong)}
.btn-danger{background:transparent;color:var(--red);border-color:color-mix(in srgb,var(--red) 45%,transparent)}.btn-danger:hover{background:var(--red-dim)}
.btn-sm{padding:4px 9px;font-size:12px;border-radius:7px}
.btn-block{width:100%;justify-content:center;padding:11px}
.btn[disabled]{opacity:.5;cursor:not-allowed}
.actions{display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin-top:14px}

/* ---------- 登录 ---------- */
.login-view{position:relative;z-index:1;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:24px}
.login-card{background:var(--surface);border:1px solid var(--border);border-radius:16px;padding:34px 38px;width:100%;max-width:384px;box-shadow:var(--shadow)}
.login-bar{display:flex;justify-content:flex-end;margin-bottom:12px}
.theme-toggle{background:transparent;border:1px solid var(--border);border-radius:9px;padding:6px 12px;cursor:pointer;font-size:13px;color:var(--text-sec);font-family:inherit;transition:all .15s}
.theme-toggle:hover{border-color:var(--border-strong);color:var(--accent)}
.brand{display:flex;align-items:center;gap:12px;margin-bottom:26px}
.logo-mark{width:36px;height:36px;border-radius:10px;background:var(--accent);display:grid;place-items:center;color:var(--btn-text);flex-shrink:0}
.logo-text{font-size:15px;font-weight:600;font-family:var(--mono);letter-spacing:-.01em;line-height:1.15}
.logo-accent{color:var(--accent);font-weight:700}
.logo-sub{font-size:9.5px;color:var(--text-ter);letter-spacing:.18em;text-transform:uppercase;margin-top:3px}
.msg{display:none;background:var(--red-dim);color:var(--red);padding:11px 14px;border-radius:9px;margin-bottom:16px;font-size:13px;text-align:center;border:1px solid color-mix(in srgb,var(--red) 30%,transparent)}
.field label{display:block;font-size:11.5px;font-weight:600;color:var(--text-sec);margin-bottom:6px;letter-spacing:.03em}
.field input,.field select{width:100%;padding:8px 11px;border:1px solid var(--border);border-radius:9px;font-size:13px;font-family:var(--mono);background:var(--bg);color:var(--text);transition:border-color .14s,box-shadow .14s}
.field input:focus,.field select:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px var(--accent-dim)}
.field .hint{font-size:11px;color:var(--text-ter);margin-top:4px;line-height:1.4}
.login-card .foot{margin-top:20px;text-align:center;font-size:11.5px;color:var(--text-ter);font-family:var(--mono)}

/* ---------- 主面板骨架 ---------- */
.app{position:relative;z-index:1;display:flex;min-height:100vh}
.sidebar{width:204px;flex-shrink:0;background:var(--surface);border-right:1px solid var(--border);display:flex;flex-direction:column;position:sticky;top:0;height:100vh;padding:18px 12px}
.sidebar .brand{padding:0 6px 16px;border-bottom:1px solid var(--border);margin-bottom:10px}
.nav{display:flex;flex-direction:column;gap:2px;flex:1;overflow-y:auto}
.nav button{position:relative;display:flex;align-items:center;gap:10px;width:100%;text-align:left;padding:9px 11px;border:none;border-radius:9px;background:transparent;color:var(--text-sec);font-size:13.5px;font-weight:500;cursor:pointer;transition:background .14s,color .14s}
.nav button:hover{background:var(--surface-2);color:var(--text)}
.nav button.active{background:var(--accent-dim);color:var(--accent);font-weight:600}
.nav button.active::before{content:"";position:absolute;left:0;top:20%;bottom:20%;width:2.5px;border-radius:2px;background:var(--accent)}
.sidebar-foot{border-top:1px solid var(--border);padding-top:10px;display:flex;flex-direction:column;gap:8px}
.conn{display:flex;align-items:center;gap:7px;padding:3px 8px;font-size:11.5px;color:var(--text-ter);min-width:0}
.conn span{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;min-width:0}
.conn .cshort{display:none}
.conn .pulse{width:7px;height:7px;border-radius:50%;background:var(--ok);position:relative;flex-shrink:0}
.conn .pulse::after{content:"";position:absolute;inset:-3px;border-radius:50%;background:var(--ok);opacity:.35;animation:pulse 2s ease-out infinite}
@keyframes pulse{0%{transform:scale(.6);opacity:.5}70%{transform:scale(1.7);opacity:0}100%{opacity:0}}
.conn.offline .pulse{background:var(--text-ter)}
.conn.offline .pulse::after{animation:none;background:var(--text-ter)}
.mini{font-size:11px;letter-spacing:.02em;color:var(--text-sec);font-family:var(--mono);padding:0 8px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.main{flex:1;min-width:0;padding:32px 36px 72px;max-width:1280px}
.topbar{display:flex;align-items:center;gap:12px;margin-bottom:24px;padding-bottom:20px;border-bottom:1px solid var(--border)}
.topbar h1{font-size:20px;font-weight:700;letter-spacing:-.4px;flex:1;display:flex;align-items:baseline;gap:10px}
.topbar .crumb{font-size:12px;color:var(--text-ter);font-weight:500;letter-spacing:.04em}

/* ---------- 统计卡 ---------- */
.stats-row{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px;margin-bottom:22px}
.stat{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:16px 18px;position:relative;overflow:hidden}
.stat .k{font-size:11px;font-weight:600;letter-spacing:.07em;text-transform:uppercase;color:var(--text-ter);margin-bottom:9px;display:flex;align-items:center;gap:7px}
.stat .v{font-size:30px;font-weight:700;font-family:var(--mono);letter-spacing:-.02em;line-height:1;font-variant-numeric:tabular-nums}
.stat .sub{font-size:11.5px;color:var(--text-ter);margin-top:7px;font-family:var(--mono)}
.stat .v.green{color:var(--ok)}.stat .v.orange{color:var(--orange)}.stat .v.red{color:var(--red)}.stat .v.blue{color:var(--blue)}

/* ---------- 卡片 ---------- */
.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:22px 24px;margin-bottom:16px}
.card h2{font-size:14.5px;font-weight:700;letter-spacing:-.2px;margin-bottom:14px;display:flex;align-items:center;gap:9px}
.card .hint{font-size:12.5px;color:var(--text-sec);margin:-8px 0 14px}
.card .card-actions{display:flex;gap:8px;align-items:center;margin:0 0 12px}

/* ---------- 表格 ---------- */
.tbl{width:100%;border-collapse:collapse;font-size:13px}
.tbl th{text-align:left;font-size:11px;font-weight:600;color:var(--text-ter);text-transform:uppercase;letter-spacing:.07em;padding:9px 12px;border-bottom:1px solid var(--border)}
.tbl td{padding:10px 12px;border-bottom:1px solid var(--line);vertical-align:middle}
.tbl tbody tr{transition:background .12s}
.tbl tbody tr:hover{background:color-mix(in srgb,var(--surface-2) 55%,transparent)}
.tbl tbody tr:last-child td{border-bottom:none}
.tbl tbody tr.hl{background:var(--accent-dim)}
.tbl th.num,.tbl td.num{text-align:right}
.tbl input[type=text],.tbl input[type=password],.tbl input:not([type]),.tbl select{padding:5px 8px;font-size:12.5px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-family:var(--mono);width:100%;min-width:60px;transition:border-color .14s,box-shadow .14s}
.tbl input:focus,.tbl select:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px var(--accent-dim)}

/* ---------- 状态徽章 / 标签 / 节点名 ---------- */
.badge{display:inline-flex;align-items:center;gap:5px;padding:2.5px 9px;border-radius:999px;font-size:11.5px;font-weight:600;letter-spacing:.02em}
.badge .bdot{width:6px;height:6px;border-radius:50%}
.badge.available{background:var(--ok-dim);color:var(--ok)}.badge.available .bdot{background:var(--ok)}
.badge.exhausted{background:var(--orange-dim);color:var(--orange)}.badge.exhausted .bdot{background:var(--orange)}
.badge.dead{background:var(--red-dim);color:var(--red)}.badge.dead .bdot{background:var(--red)}
.badge.idle{background:var(--blue-dim);color:var(--blue)}.badge.idle .bdot{background:var(--blue)}
.tag{display:inline-flex;align-items:center;padding:1px 7px;border-radius:6px;font-size:10.5px;font-weight:600;letter-spacing:.03em;vertical-align:1px}
.tag.cur{background:var(--accent-dim);color:var(--accent);margin-left:6px}
.tag.man{background:var(--blue-dim);color:var(--blue);margin-left:6px}
.node-name{font-weight:600}
.proto{font-family:var(--mono);font-size:12px;color:var(--text-sec)}
.err{max-width:280px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:11.5px;color:var(--text-ter)}
.acts{white-space:nowrap}
.acts .btn{margin-right:4px}

/* ---------- 筛选 chips ---------- */
.filter-chips{display:flex;gap:6px;margin-bottom:14px;flex-wrap:wrap}
.chip{display:inline-flex;align-items:center;gap:6px;padding:5px 13px;border-radius:999px;border:1px solid var(--border);background:var(--surface);color:var(--text-sec);font-size:12.5px;font-weight:500;cursor:pointer;transition:all .14s}
.chip:hover{border-color:var(--border-strong);color:var(--text)}
.chip.active{background:var(--accent-dim);border-color:transparent;color:var(--accent);font-weight:600}
.chip .cnt{font-family:var(--mono);font-size:11px;opacity:.7}

/* ---------- 节点编辑器 / 流量条 / 网关 ---------- */
.nedit{display:grid;grid-template-columns:110px 1fr 1fr 90px 64px;gap:8px;align-items:center;padding:9px 0;border-bottom:1px solid var(--line)}
.nedit input,.nedit select{padding:5.5px 9px;font-size:12.5px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-family:var(--mono);width:100%}
.nedit details{margin-top:8px;grid-column:1/-1;font-size:12px}
.nedit details summary{cursor:pointer;color:var(--text-sec);font-weight:500;letter-spacing:.02em}
.nedit .extra{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:8px;padding:10px 12px;background:var(--surface-2);border-radius:9px;margin-top:6px}
.check{display:inline-flex;align-items:center;gap:8px;font-size:13px;color:var(--text-sec);cursor:pointer;user-select:none;padding:6px 0}
.check input{accent-color:var(--accent);width:15px;height:15px}
.usage{display:flex;align-items:center;gap:8px;min-width:150px}
.usage-bar{flex:1;height:5px;border-radius:99px;background:var(--surface-2);overflow:hidden}
.usage-bar i{display:block;height:100%;border-radius:99px;background:var(--accent)}
.usage span{font-size:11.5px;color:var(--text-sec);font-family:var(--mono);white-space:nowrap}
.gw-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:10px}
.gw-box{border:1px solid var(--border);border-radius:9px;padding:10px 12px;background:var(--bg)}
.gw-label{font-size:11.5px;color:var(--text-ter);margin-bottom:5px;letter-spacing:.03em}
.gw-copy{display:flex;align-items:center;gap:6px;background:none;border:none;padding:0;cursor:pointer;color:var(--text);font-family:var(--mono);font-size:12.5px;transition:color .14s}
.gw-copy:hover{color:var(--accent)}
.gw-copy code{font-family:var(--mono);word-break:break-all}
.gw-copy .glyph{display:inline-flex;align-items:center;gap:4px;font-size:11px;opacity:.7}

/* ---------- 请求分布（Token 卡片） ---------- */
.model-bars{display:grid;gap:6px;margin:2px 0 16px}
.model-bars .mb-head{font-size:10.5px;font-weight:600;letter-spacing:.08em;text-transform:uppercase;color:var(--text-ter);margin-bottom:8px}
.mbar{display:grid;grid-template-columns:minmax(120px,180px) 1fr 88px;gap:12px;align-items:center;font-size:11.5px}
.mbar .lbl{font-family:var(--mono);color:var(--text-sec);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.mbar .track{height:6px;border-radius:99px;background:var(--surface-2);overflow:hidden}
.mbar .track i{display:block;height:100%;border-radius:99px;background:var(--accent)}
.mbar .val{font-family:var(--mono);color:var(--text-ter);text-align:right;font-variant-numeric:tabular-nums}

/* ---------- 延迟分布（节点页） ---------- */
.latdist{margin:-4px 0 16px;padding:12px 16px;border:1px solid var(--border);border-radius:9px;background:var(--surface)}
.latdist .row{display:flex;align-items:center;gap:12px}
.latdist .ld-title{font-size:10.5px;font-weight:600;letter-spacing:.08em;text-transform:uppercase;color:var(--text-ter);flex:none}
.latdist .ld-track{flex:1;display:flex;height:8px;border-radius:99px;overflow:hidden;background:var(--surface-2);min-width:0}
.latdist .ld-seg{height:100%}
.latdist .ld-seg.g{background:var(--ok)}
.latdist .ld-seg.b{background:var(--accent)}
.latdist .ld-seg.o{background:var(--orange)}
.latdist .ld-seg.r{background:var(--red)}
.latdist .ld-legend{display:flex;gap:16px;flex-wrap:wrap;margin-top:8px;font-size:11px;font-family:var(--mono);color:var(--text-sec)}
.latdist .ld-key{display:inline-flex;align-items:center;gap:5px}
.latdist .ld-key i{width:8px;height:8px;border-radius:2px;flex:none}
.latdist .ld-key i.g{background:var(--ok)}
.latdist .ld-key i.b{background:var(--accent)}
.latdist .ld-key i.o{background:var(--orange)}
.latdist .ld-key i.r{background:var(--red)}

/* ---------- 用量趋势（概览，数据来自调用日志） ---------- */
.ut-head{display:flex;align-items:center;justify-content:space-between;gap:12px;flex-wrap:wrap;margin-bottom:2px}
.ut-tools{display:flex;align-items:center;gap:10px;flex-wrap:wrap}
.ut-refresh{display:inline-flex;align-items:center;gap:6px;font-size:12px;color:var(--text-sec)}
.ut-refresh select{padding:4px 8px;font-size:12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-family:var(--mono)}
.ut-refresh select:focus{outline:none;border-color:var(--accent)}
.ut-chart{position:relative;margin-top:2px}
.ut-chart svg{display:block;width:100%;height:auto}
.ut-chart .ut-empty,.ut-empty{text-align:center;color:var(--text-ter);font-size:12.5px;padding:22px 0}
.ut-legend{display:flex;gap:16px;flex-wrap:wrap;font-size:11.5px;font-family:var(--mono);color:var(--text-sec);margin:7px 2px 2px}
.ut-key{display:inline-flex;align-items:center;gap:5px}
.ut-key i{width:8px;height:8px;border-radius:2px;flex:none}
.ut-key b{color:var(--text);font-variant-numeric:tabular-nums}
.ut-tip{position:fixed;pointer-events:none;background:var(--surface);border:1px solid var(--border);color:var(--text);padding:9px 13px;border-radius:9px;font-size:12px;font-family:var(--mono);line-height:1.6;z-index:999;box-shadow:var(--shadow);white-space:nowrap;min-width:150px}
.ut-tip .t{color:var(--text-ter);font-size:11px;margin-bottom:2px}
.ut-tip b{font-variant-numeric:tabular-nums;font-weight:600}
@media(max-width:560px){.ut-head{flex-direction:column;align-items:flex-start}}
@media(max-width:560px){.mbar{grid-template-columns:1fr 1fr}}

/* ---------- 页面切换 ---------- */
.page{display:none}
.page.active{display:block;animation:fadeIn .18s ease}
@keyframes fadeIn{from{opacity:0;transform:translateY(4px)}to{opacity:1;transform:none}}

/* ---------- Toast ---------- */
#toast{position:fixed;bottom:26px;left:50%;transform:translateX(-50%) translateY(16px);background:var(--text);color:var(--bg);padding:10px 22px;border-radius:10px;font-size:13.5px;font-weight:600;opacity:0;pointer-events:none;transition:all .22s cubic-bezier(.2,.7,.3,1);z-index:999;box-shadow:var(--shadow)}
#toast.show{opacity:1;transform:translateX(-50%) translateY(0)}
#toast.error{background:var(--red);color:#fff}

/* ---------- 运行日志 ---------- */
.log-wrap{max-height:520px;overflow-y:auto;font-family:var(--mono);font-size:12.5px;line-height:1.55;border:1px solid var(--border);border-radius:9px;padding:6px 0;background:var(--bg)}
.log-line{display:flex;gap:10px;align-items:baseline;padding:1px 12px;border-left:2px solid transparent}
.log-line:hover{background:color-mix(in srgb,var(--surface-2) 50%,transparent)}
.log-line.lv-error{border-left-color:var(--red);background:var(--red-dim)}
.log-line.lv-warn{border-left-color:var(--orange);background:var(--orange-dim)}
.log-time{color:var(--text-ter);white-space:nowrap;flex:none}
.log-lv{flex:none;width:48px;font-weight:700;letter-spacing:.06em;font-size:11px}
.lv-info{color:var(--ok)}.lv-warn{color:var(--orange)}.lv-error{color:var(--red)}.lv-debug{color:var(--text-ter)}
.log-msg{white-space:pre-wrap;word-break:break-all}
.log-attrs{padding:0 12px 2px 72px;color:var(--text-ter);font-size:12px}
.log-attrs b{color:var(--accent);font-weight:600}
.log-toolbar{display:flex;flex-wrap:wrap;gap:10px;align-items:center;margin:12px 0 10px}

/* ---------- 响应式 ---------- */
@media(max-width:960px){
  .sidebar{width:64px;padding:16px 8px}
  .nav button span,.brand .txt,.sidebar-foot .txt{display:none}
  .conn .ctxt{display:none}
  .conn .cshort{display:inline}
  .mini{display:none}
  .nav button{justify-content:center}
  .nav button.active::before{top:14%;bottom:14%}
  .main{padding:20px 16px 60px}
  .stats-row{grid-template-columns:repeat(2,1fr)}
  .nedit{grid-template-columns:1fr 1fr 90px 64px}
  .nedit select{grid-column:1/-1}
}
@media(max-width:560px){
  .stats-row{grid-template-columns:1fr}
}
/* 尊重系统减弱动态偏好：去掉非必要脉冲与转场动画 */
@media (prefers-reduced-motion:reduce){
  .conn .pulse::after{animation:none}
  .page.active{animation:none}
}
</style>
</head>
<body>

<!-- ============ 登录视图 ============ -->
<div id="view-login" class="login-view">
  <div class="login-card">
    <div class="login-bar"><button class="theme-toggle" onclick="toggleTheme()" aria-label="切换主题"><svg class="ic-moon" width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.8A9 9 0 1111.2 3a7 7 0 009.8 9.8z"/></svg><svg class="ic-sun" width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg></button></div>
    <div class="brand">
      <div class="logo-mark"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M8 20l6-8h4l-6 8z"/><path d="M4 20h4"/><path d="M12 10V4a2 2 0 00-2-2H4a2 2 0 00-2 2v9"/></svg></div>
      <div><div class="logo-text">opencode<span class="logo-accent">2api</span></div><div class="logo-sub">ADMIN · LOCAL PROXY</div></div>
    </div>
    <div class="msg" id="login-msg"></div>
    <form method="post" action="/login">
      <div class="field">
        <label for="pwd">管理密码</label>
        <input id="pwd" name="password" type="password" placeholder="输入管理密码" autocomplete="current-password" required>
      </div>
      <button class="btn btn-primary btn-block" type="submit">登 录</button>
    </form>
    <div class="foot">默认密码 123456 · 生产部署务必修改 -password</div>
  </div>
</div>

<!-- ============ 主面板 ============ -->

<script>
(function(){document.documentElement.setAttribute('data-theme',localStorage.getItem('theme')||'dark')})();
function toggleTheme(){var d=document.documentElement;d.setAttribute('data-theme',d.getAttribute('data-theme')==='dark'?'light':'dark');localStorage.setItem('theme',d.getAttribute('data-theme'))}
</script>
</body>
</html>
`

const adminHTML = `
<!doctype html>
<html lang="zh">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>OPENCODE TO API · 管理面板</title>
<style>
/* ============================================================
   OPENCODE TO API · 管理面板 (重构版)
   设计方向：tech-utility · 深色优先 · 无外网字体依赖
   - CJK 系统字体栈（PingFang / 微软雅黑 / Noto Sans SC）
   - 语义色：accent=健康/主操作 · blue=信息 · orange=耗尽 · red=故障
   - 全部交互契约与原版一致，可直接替换 main.go 中的 adminHTML
   ============================================================ */
:root{
  --bg:#0a0f1c; --surface:#101828; --surface-2:#1a2336;
  --border:rgba(148,163,184,.14); --border-strong:rgba(148,163,184,.28);
  --line:rgba(148,163,184,.08);
  --text:#e9eef8; --text-sec:#97a3bb; --text-ter:#7b87a5;
  --accent:#6f96ff; --accent-hover:#5d88ff; --accent-dim:rgba(111,150,255,.15);
  --blue:#7ea2ff; --blue-dim:rgba(126,162,255,.12);
  --ok:#2fd489; --ok-dim:rgba(47,212,137,.13);
  --orange:#f0b04a; --orange-dim:rgba(240,176,74,.12);
  --purple:#b197f7; --purple-dim:rgba(177,151,247,.13);
  --red:#ff6b6b; --red-dim:rgba(255,107,107,.12);
  --radius:12px; --radius-sm:8px;
  --font:-apple-system,BlinkMacSystemFont,'Segoe UI','PingFang SC','Microsoft YaHei','Noto Sans SC',sans-serif;
  --mono:ui-monospace,'SF Mono','JetBrains Mono','Cascadia Code',Consolas,monospace;
  --shadow:0 10px 34px rgba(3,8,20,.55);
  --btn-text:#0a0f1e;
}
[data-theme="light"]{
  --bg:#f4f6fb; --surface:#ffffff; --surface-2:#eef1f8;
  --border:#e1e5ee; --border-strong:#c9d1e0; --line:rgba(20,30,60,.06);
  --text:#161a26; --text-sec:#59637a; --text-ter:#616d88;
  --accent:#3b63c9; --accent-hover:#3457b6; --accent-dim:rgba(59,99,201,.12);
  --blue:#3b63c9; --blue-dim:rgba(59,99,201,.1);
  --ok:#0a6f42; --ok-dim:rgba(10,111,66,.10);
  --orange:#a1530a; --orange-dim:rgba(161,83,10,.10);
  --purple:#6d4fc4; --purple-dim:rgba(109,79,196,.10);
  --red:#b42318; --red-dim:rgba(180,35,24,.09);
  --shadow:0 10px 30px rgba(30,40,70,.1);
  --btn-text:#ffffff;
}
*{margin:0;padding:0;box-sizing:border-box}
html,body{height:100%}
body{font-family:var(--font);background:var(--bg);color:var(--text);font-size:14px;line-height:1.6;-webkit-font-smoothing:antialiased;text-rendering:optimizeLegibility}
button{font-family:inherit}
input,select,textarea{font-family:inherit;color:inherit}
::selection{background:var(--accent-dim)}
:focus-visible{outline:none;box-shadow:0 0 0 2px var(--accent-dim),0 0 0 1px var(--accent)}
.muted{color:var(--text-ter)}
.mono{font-family:var(--mono)}
.num{font-variant-numeric:tabular-nums}
.lat-warn{color:var(--orange)}
.lat-high{color:var(--red)}
.empty-hint{text-align:center;color:var(--text-ter);padding:26px 0;font-size:13px}
/* 主题图标：随 data-theme 切换显示月亮/太阳 */
.ic-sun{display:none}
[data-theme="dark"] .ic-moon{display:inline-block}
[data-theme="dark"] .ic-sun{display:none}
[data-theme="light"] .ic-moon{display:none}
[data-theme="light"] .ic-sun{display:inline-block}
.theme-btn-ic{display:inline-flex;align-items:center;gap:6px}

/* ---------- 通用按钮 ---------- */
.btn{display:inline-flex;align-items:center;gap:6px;padding:7px 14px;border:1px solid transparent;border-radius:9px;font-size:13px;font-weight:600;cursor:pointer;transition:background .14s,border-color .14s,color .14s,transform .05s;white-space:nowrap;line-height:1.2}
.btn:active{transform:translateY(1px)}
.btn-primary{background:var(--accent);color:var(--btn-text)}.btn-primary:hover{background:var(--accent-hover)}
.btn-secondary{background:var(--blue);color:var(--btn-text)}.btn-secondary:hover{filter:brightness(1.07)}
.btn-ghost{background:var(--surface);color:var(--text-sec);border-color:var(--border)}.btn-ghost:hover{color:var(--text);border-color:var(--border-strong)}
.btn-danger{background:transparent;color:var(--red);border-color:color-mix(in srgb,var(--red) 45%,transparent)}.btn-danger:hover{background:var(--red-dim)}
.btn-sm{padding:4px 9px;font-size:12px;border-radius:7px}
.btn-block{width:100%;justify-content:center;padding:11px}
.btn[disabled]{opacity:.5;cursor:not-allowed}
.actions{display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin-top:14px}

/* ---------- 登录 ---------- */
.login-view{position:relative;z-index:1;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:24px}
.login-card{background:var(--surface);border:1px solid var(--border);border-radius:16px;padding:34px 38px;width:100%;max-width:384px;box-shadow:var(--shadow)}
.login-bar{display:flex;justify-content:flex-end;margin-bottom:12px}
.theme-toggle{background:transparent;border:1px solid var(--border);border-radius:9px;padding:6px 12px;cursor:pointer;font-size:13px;color:var(--text-sec);font-family:inherit;transition:all .15s}
.theme-toggle:hover{border-color:var(--border-strong);color:var(--accent)}
.brand{display:flex;align-items:center;gap:12px;margin-bottom:26px}
.logo-mark{width:36px;height:36px;border-radius:10px;background:var(--accent);display:grid;place-items:center;color:var(--btn-text);flex-shrink:0}
.logo-text{font-size:15px;font-weight:600;font-family:var(--mono);letter-spacing:-.01em;line-height:1.15}
.logo-accent{color:var(--accent);font-weight:700}
.logo-sub{font-size:9.5px;color:var(--text-ter);letter-spacing:.18em;text-transform:uppercase;margin-top:3px}
.msg{display:none;background:var(--red-dim);color:var(--red);padding:11px 14px;border-radius:9px;margin-bottom:16px;font-size:13px;text-align:center;border:1px solid color-mix(in srgb,var(--red) 30%,transparent)}
.field label{display:block;font-size:11.5px;font-weight:600;color:var(--text-sec);margin-bottom:6px;letter-spacing:.03em}
.field input,.field select{width:100%;padding:8px 11px;border:1px solid var(--border);border-radius:9px;font-size:13px;font-family:var(--mono);background:var(--bg);color:var(--text);transition:border-color .14s,box-shadow .14s}
.field input:focus,.field select:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px var(--accent-dim)}
.field .hint{font-size:11px;color:var(--text-ter);margin-top:4px;line-height:1.4}
.login-card .foot{margin-top:20px;text-align:center;font-size:11.5px;color:var(--text-ter);font-family:var(--mono)}

/* ---------- 主面板骨架 ---------- */
.app{position:relative;z-index:1;display:flex;min-height:100vh}
.sidebar{width:204px;flex-shrink:0;background:var(--surface);border-right:1px solid var(--border);display:flex;flex-direction:column;position:sticky;top:0;height:100vh;padding:18px 12px}
.sidebar .brand{padding:0 6px 16px;border-bottom:1px solid var(--border);margin-bottom:10px}
.nav{display:flex;flex-direction:column;gap:2px;flex:1;overflow-y:auto}
.nav button{position:relative;display:flex;align-items:center;gap:10px;width:100%;text-align:left;padding:9px 11px;border:none;border-radius:9px;background:transparent;color:var(--text-sec);font-size:13.5px;font-weight:500;cursor:pointer;transition:background .14s,color .14s}
.nav button:hover{background:var(--surface-2);color:var(--text)}
.nav button.active{background:var(--accent-dim);color:var(--accent);font-weight:600}
.nav button.active::before{content:"";position:absolute;left:0;top:20%;bottom:20%;width:2.5px;border-radius:2px;background:var(--accent)}
.sidebar-foot{border-top:1px solid var(--border);padding-top:10px;display:flex;flex-direction:column;gap:8px}
.conn{display:flex;align-items:center;gap:7px;padding:3px 8px;font-size:11.5px;color:var(--text-ter);min-width:0}
.conn span{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;min-width:0}
.conn .cshort{display:none}
.conn .pulse{width:7px;height:7px;border-radius:50%;background:var(--ok);position:relative;flex-shrink:0}
.conn .pulse::after{content:"";position:absolute;inset:-3px;border-radius:50%;background:var(--ok);opacity:.35;animation:pulse 2s ease-out infinite}
@keyframes pulse{0%{transform:scale(.6);opacity:.5}70%{transform:scale(1.7);opacity:0}100%{opacity:0}}
.conn.offline .pulse{background:var(--text-ter)}
.conn.offline .pulse::after{animation:none;background:var(--text-ter)}
.mini{font-size:11px;letter-spacing:.02em;color:var(--text-sec);font-family:var(--mono);padding:0 8px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.main{flex:1;min-width:0;padding:32px 36px 72px;max-width:1280px}
.topbar{display:flex;align-items:center;gap:12px;margin-bottom:24px;padding-bottom:20px;border-bottom:1px solid var(--border)}
.topbar h1{font-size:20px;font-weight:700;letter-spacing:-.4px;flex:1;display:flex;align-items:baseline;gap:10px}
.topbar .crumb{font-size:12px;color:var(--text-ter);font-weight:500;letter-spacing:.04em}

/* ---------- 统计卡 ---------- */
.stats-row{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px;margin-bottom:22px}
.stat{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:16px 18px;position:relative;overflow:hidden}
.stat .k{font-size:11px;font-weight:600;letter-spacing:.07em;text-transform:uppercase;color:var(--text-ter);margin-bottom:9px;display:flex;align-items:center;gap:7px}
.stat .v{font-size:30px;font-weight:700;font-family:var(--mono);letter-spacing:-.02em;line-height:1;font-variant-numeric:tabular-nums}
.stat .sub{font-size:11.5px;color:var(--text-ter);margin-top:7px;font-family:var(--mono)}
.stat .v.green{color:var(--ok)}.stat .v.orange{color:var(--orange)}.stat .v.red{color:var(--red)}.stat .v.blue{color:var(--blue)}

/* ---------- 卡片 ---------- */
.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:22px 24px;margin-bottom:16px}
.card h2{font-size:14.5px;font-weight:700;letter-spacing:-.2px;margin-bottom:14px;display:flex;align-items:center;gap:9px}
.card .hint{font-size:12.5px;color:var(--text-sec);margin:-8px 0 14px}
.card .card-actions{display:flex;gap:8px;align-items:center;margin:0 0 12px}

/* ---------- 表格 ---------- */
.tbl{width:100%;border-collapse:collapse;font-size:13px}
.tbl th{text-align:left;font-size:11px;font-weight:600;color:var(--text-ter);text-transform:uppercase;letter-spacing:.07em;padding:9px 12px;border-bottom:1px solid var(--border)}
.tbl td{padding:10px 12px;border-bottom:1px solid var(--line);vertical-align:middle}
.tbl tbody tr{transition:background .12s}
.tbl tbody tr:hover{background:color-mix(in srgb,var(--surface-2) 55%,transparent)}
.tbl tbody tr:last-child td{border-bottom:none}
.tbl tbody tr.hl{background:var(--accent-dim)}
.tbl th.num,.tbl td.num{text-align:right}
.tbl input[type=text],.tbl input[type=password],.tbl input:not([type]),.tbl select{padding:5px 8px;font-size:12.5px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-family:var(--mono);width:100%;min-width:60px;transition:border-color .14s,box-shadow .14s}
.tbl input:focus,.tbl select:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px var(--accent-dim)}

/* ---------- 状态徽章 / 标签 / 节点名 ---------- */
.badge{display:inline-flex;align-items:center;gap:5px;padding:2.5px 9px;border-radius:999px;font-size:11.5px;font-weight:600;letter-spacing:.02em}
.badge .bdot{width:6px;height:6px;border-radius:50%}
.badge.available{background:var(--ok-dim);color:var(--ok)}.badge.available .bdot{background:var(--ok)}
.badge.exhausted{background:var(--orange-dim);color:var(--orange)}.badge.exhausted .bdot{background:var(--orange)}
.badge.dead{background:var(--red-dim);color:var(--red)}.badge.dead .bdot{background:var(--red)}
.badge.idle{background:var(--blue-dim);color:var(--blue)}.badge.idle .bdot{background:var(--blue)}
.tag{display:inline-flex;align-items:center;padding:1px 7px;border-radius:6px;font-size:10.5px;font-weight:600;letter-spacing:.03em;vertical-align:1px}
.tag.cur{background:var(--accent-dim);color:var(--accent);margin-left:6px}
.tag.man{background:var(--blue-dim);color:var(--blue);margin-left:6px}
.node-name{font-weight:600}
.proto{font-family:var(--mono);font-size:12px;color:var(--text-sec)}
.err{max-width:280px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:11.5px;color:var(--text-ter)}
.acts{white-space:nowrap}
.acts .btn{margin-right:4px}

/* ---------- 筛选 chips ---------- */
.filter-chips{display:flex;gap:6px;margin-bottom:14px;flex-wrap:wrap}
.chip{display:inline-flex;align-items:center;gap:6px;padding:5px 13px;border-radius:999px;border:1px solid var(--border);background:var(--surface);color:var(--text-sec);font-size:12.5px;font-weight:500;cursor:pointer;transition:all .14s}
.chip:hover{border-color:var(--border-strong);color:var(--text)}
.chip.active{background:var(--accent-dim);border-color:transparent;color:var(--accent);font-weight:600}
.chip .cnt{font-family:var(--mono);font-size:11px;opacity:.7}

/* ---------- 节点编辑器 / 流量条 / 网关 ---------- */
.nedit{display:grid;grid-template-columns:110px 1fr 1fr 90px 64px;gap:8px;align-items:center;padding:9px 0;border-bottom:1px solid var(--line)}
.nedit input,.nedit select{padding:5.5px 9px;font-size:12.5px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-family:var(--mono);width:100%}
.nedit details{margin-top:8px;grid-column:1/-1;font-size:12px}
.nedit details summary{cursor:pointer;color:var(--text-sec);font-weight:500;letter-spacing:.02em}
.nedit .extra{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:8px;padding:10px 12px;background:var(--surface-2);border-radius:9px;margin-top:6px}
.check{display:inline-flex;align-items:center;gap:8px;font-size:13px;color:var(--text-sec);cursor:pointer;user-select:none;padding:6px 0}
.check input{accent-color:var(--accent);width:15px;height:15px}
.usage{display:flex;align-items:center;gap:8px;min-width:150px}
.usage-bar{flex:1;height:5px;border-radius:99px;background:var(--surface-2);overflow:hidden}
.usage-bar i{display:block;height:100%;border-radius:99px;background:var(--accent)}
.usage span{font-size:11.5px;color:var(--text-sec);font-family:var(--mono);white-space:nowrap}
.gw-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:10px}
.gw-box{border:1px solid var(--border);border-radius:9px;padding:10px 12px;background:var(--bg)}
.gw-label{font-size:11.5px;color:var(--text-ter);margin-bottom:5px;letter-spacing:.03em}
.gw-copy{display:flex;align-items:center;gap:6px;background:none;border:none;padding:0;cursor:pointer;color:var(--text);font-family:var(--mono);font-size:12.5px;transition:color .14s}
.gw-copy:hover{color:var(--accent)}
.gw-copy code{font-family:var(--mono);word-break:break-all}
.gw-copy .glyph{display:inline-flex;align-items:center;gap:4px;font-size:11px;opacity:.7}

/* ---------- 请求分布（Token 卡片） ---------- */
.model-bars{display:grid;gap:6px;margin:2px 0 16px}
.model-bars .mb-head{font-size:10.5px;font-weight:600;letter-spacing:.08em;text-transform:uppercase;color:var(--text-ter);margin-bottom:8px}
.mbar{display:grid;grid-template-columns:minmax(120px,180px) 1fr 88px;gap:12px;align-items:center;font-size:11.5px}
.mbar .lbl{font-family:var(--mono);color:var(--text-sec);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.mbar .track{height:6px;border-radius:99px;background:var(--surface-2);overflow:hidden}
.mbar .track i{display:block;height:100%;border-radius:99px;background:var(--accent)}
.mbar .val{font-family:var(--mono);color:var(--text-ter);text-align:right;font-variant-numeric:tabular-nums}

/* ---------- 延迟分布（节点页） ---------- */
.latdist{margin:-4px 0 16px;padding:12px 16px;border:1px solid var(--border);border-radius:9px;background:var(--surface)}
.latdist .row{display:flex;align-items:center;gap:12px}
.latdist .ld-title{font-size:10.5px;font-weight:600;letter-spacing:.08em;text-transform:uppercase;color:var(--text-ter);flex:none}
.latdist .ld-track{flex:1;display:flex;height:8px;border-radius:99px;overflow:hidden;background:var(--surface-2);min-width:0}
.latdist .ld-seg{height:100%}
.latdist .ld-seg.g{background:var(--ok)}
.latdist .ld-seg.b{background:var(--accent)}
.latdist .ld-seg.o{background:var(--orange)}
.latdist .ld-seg.r{background:var(--red)}
.latdist .ld-legend{display:flex;gap:16px;flex-wrap:wrap;margin-top:8px;font-size:11px;font-family:var(--mono);color:var(--text-sec)}
.latdist .ld-key{display:inline-flex;align-items:center;gap:5px}
.latdist .ld-key i{width:8px;height:8px;border-radius:2px;flex:none}
.latdist .ld-key i.g{background:var(--ok)}
.latdist .ld-key i.b{background:var(--accent)}
.latdist .ld-key i.o{background:var(--orange)}
.latdist .ld-key i.r{background:var(--red)}

/* ---------- 用量趋势（概览，数据来自调用日志） ---------- */
.ut-head{display:flex;align-items:center;justify-content:space-between;gap:12px;flex-wrap:wrap;margin-bottom:2px}
.ut-tools{display:flex;align-items:center;gap:10px;flex-wrap:wrap}
.ut-refresh{display:inline-flex;align-items:center;gap:6px;font-size:12px;color:var(--text-sec)}
.ut-refresh select{padding:4px 8px;font-size:12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-family:var(--mono)}
.ut-refresh select:focus{outline:none;border-color:var(--accent)}
.ut-chart{position:relative;margin-top:2px}
.ut-chart svg{display:block;width:100%;height:auto}
.ut-chart .ut-empty,.ut-empty{text-align:center;color:var(--text-ter);font-size:12.5px;padding:22px 0}
.ut-legend{display:flex;gap:16px;flex-wrap:wrap;font-size:11.5px;font-family:var(--mono);color:var(--text-sec);margin:7px 2px 2px}
.ut-key{display:inline-flex;align-items:center;gap:5px}
.ut-key i{width:8px;height:8px;border-radius:2px;flex:none}
.ut-key b{color:var(--text);font-variant-numeric:tabular-nums}
.ut-tip{position:fixed;pointer-events:none;background:var(--surface);border:1px solid var(--border);color:var(--text);padding:9px 13px;border-radius:9px;font-size:12px;font-family:var(--mono);line-height:1.6;z-index:999;box-shadow:var(--shadow);white-space:nowrap;min-width:150px}
.ut-tip .t{color:var(--text-ter);font-size:11px;margin-bottom:2px}
.ut-tip b{font-variant-numeric:tabular-nums;font-weight:600}
@media(max-width:560px){.ut-head{flex-direction:column;align-items:flex-start}}
@media(max-width:560px){.mbar{grid-template-columns:1fr 1fr}}

/* ---------- 页面切换 ---------- */
.page{display:none}
.page.active{display:block;animation:fadeIn .18s ease}
@keyframes fadeIn{from{opacity:0;transform:translateY(4px)}to{opacity:1;transform:none}}

/* ---------- Toast ---------- */
#toast{position:fixed;bottom:26px;left:50%;transform:translateX(-50%) translateY(16px);background:var(--text);color:var(--bg);padding:10px 22px;border-radius:10px;font-size:13.5px;font-weight:600;opacity:0;pointer-events:none;transition:all .22s cubic-bezier(.2,.7,.3,1);z-index:999;box-shadow:var(--shadow)}
#toast.show{opacity:1;transform:translateX(-50%) translateY(0)}
#toast.error{background:var(--red);color:#fff}

/* ---------- 运行日志 ---------- */
.log-wrap{max-height:520px;overflow-y:auto;font-family:var(--mono);font-size:12.5px;line-height:1.55;border:1px solid var(--border);border-radius:9px;padding:6px 0;background:var(--bg)}
.log-line{display:flex;gap:10px;align-items:baseline;padding:1px 12px;border-left:2px solid transparent}
.log-line:hover{background:color-mix(in srgb,var(--surface-2) 50%,transparent)}
.log-line.lv-error{border-left-color:var(--red);background:var(--red-dim)}
.log-line.lv-warn{border-left-color:var(--orange);background:var(--orange-dim)}
.log-time{color:var(--text-ter);white-space:nowrap;flex:none}
.log-lv{flex:none;width:48px;font-weight:700;letter-spacing:.06em;font-size:11px}
.lv-info{color:var(--ok)}.lv-warn{color:var(--orange)}.lv-error{color:var(--red)}.lv-debug{color:var(--text-ter)}
.log-msg{white-space:pre-wrap;word-break:break-all}
.log-attrs{padding:0 12px 2px 72px;color:var(--text-ter);font-size:12px}
.log-attrs b{color:var(--accent);font-weight:600}
.log-toolbar{display:flex;flex-wrap:wrap;gap:10px;align-items:center;margin:12px 0 10px}
/* ---------- 调用日志 ---------- */
.log-tabs{display:flex;gap:4px;background:var(--surface-2);border-radius:10px;padding:4px;margin:0 0 12px;width:fit-content}
.log-tabs button{background:none;border:none;color:var(--text-sec);font-size:13px;font-weight:600;padding:6px 16px;border-radius:8px;cursor:pointer}
.log-tabs button.active{background:var(--surface);color:var(--text);box-shadow:0 1px 6px rgba(0,0,0,.25)}
.cl-card{background:var(--surface);border:1px solid var(--border);border-radius:12px;padding:12px 14px}
.cl-summary{display:flex;flex-wrap:wrap;gap:16px;font-size:13px;align-items:center}
.cl-summary b{font-weight:700}
.cl-summary .c-ok{color:var(--ok)}.cl-summary .c-fail{color:var(--red)}.cl-summary .c-issue{color:var(--orange)}
.cl-filter{display:flex;flex-wrap:wrap;gap:8px;align-items:center;margin-top:10px}
.cl-filter input[type=text],.cl-filter select{padding:5px 9px;border:1px solid var(--border);border-radius:8px;background:var(--bg);color:var(--text);font-size:13px}
.cl-list{display:flex;flex-direction:column;gap:8px;margin-top:12px;max-height:520px;overflow-y:auto;padding-right:2px}
.cl-item{border:1px solid var(--border);border-radius:10px;background:var(--surface);overflow:hidden}
.cl-item.issue{border-color:var(--orange)}
.cl-head{display:flex;align-items:center;gap:10px;padding:8px 12px;width:100%;background:none;border:none;color:inherit;font-size:13px;text-align:left;cursor:default}
.cl-item.issue .cl-head{cursor:pointer}
.cl-head:hover{background:color-mix(in srgb,var(--surface-2) 55%,transparent)}
.cl-badge{flex:none;font-size:11px;font-weight:700;border-radius:6px;padding:1px 8px;letter-spacing:.04em}
.cl-badge.ok{background:var(--ok-dim);color:var(--ok)}
.cl-badge.fail{background:var(--red-dim);color:var(--red)}
.cl-issue-tag{flex:none;font-size:11px;font-weight:600;border-radius:6px;padding:1px 8px;background:var(--orange-dim);color:var(--orange)}
.cl-time{flex:none;color:var(--text-ter);font-size:12px}
.cl-model{font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.cl-nodes{flex:1;color:var(--text-ter);font-size:12px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;text-align:right}
.cl-dur{flex:none;color:var(--text-ter);font-size:12px;font-variant-numeric:tabular-nums}
.cl-body{border-top:1px solid var(--line);padding:10px 12px;background:color-mix(in srgb,var(--surface-2) 45%,transparent);font-size:12px}
.cl-meta{color:var(--text-sec);font-family:var(--mono);word-break:break-all;margin-bottom:6px;line-height:1.6}
.cl-meta .err{color:var(--red)}
.cl-events{display:flex;flex-direction:column;gap:6px;margin-top:6px}
.cl-ev{display:flex;gap:8px;align-items:flex-start}
.cl-ev-time{flex:none;width:130px;color:var(--text-ter);font-variant-numeric:tabular-nums;white-space:nowrap}
.cl-ev-tag{flex:none;width:110px;text-align:center;font-size:11px;font-weight:600;border-radius:6px;padding:1px 6px;background:var(--surface-2);color:var(--text-sec)}
.cl-ev-tag.switch{background:var(--orange-dim);color:var(--orange)}
.cl-ev-tag.connect_ok,.cl-ev-tag.complete{background:var(--ok-dim);color:var(--ok)}
.cl-ev-tag.all_failed,.cl-ev-tag.connect_error,.cl-ev-tag.stream_error,.cl-ev-tag.upstream_error,.cl-ev-tag.stream_interrupt,.cl-ev-tag.ttft_timeout,.cl-ev-tag.silence_timeout{background:var(--red-dim);color:var(--red)}
.cl-ev-node{font-weight:700;color:var(--text)}
.cl-ev-detail{color:var(--text-sec);word-break:break-all}
.cl-chart{display:flex;align-items:flex-end;gap:3px;height:120px;margin:14px 0 6px}
.cl-chart .bar{flex:1;display:flex;flex-direction:column;justify-content:flex-end;align-items:center;height:100%;position:relative}
.cl-chart .bar .fill{width:100%;border-radius:3px 3px 0 0;background:var(--ok)}
.cl-chart .bar .fill.issue{background:var(--orange)}
.cl-chart .bar .tip{position:absolute;bottom:calc(100% + 4px);display:none;background:var(--surface-2);border:1px solid var(--border);border-radius:6px;padding:3px 7px;font-size:11px;white-space:nowrap;z-index:5}
.cl-chart .bar:hover .tip{display:block}
.cl-tbl{width:100%;border-collapse:collapse;font-size:13px}
.cl-tbl th{text-align:left;font-size:12px;color:var(--text-ter);font-weight:600;padding:6px 10px;border-bottom:1px solid var(--border)}
.cl-tbl td{padding:6px 10px;border-bottom:1px solid var(--line);color:var(--text-sec)}
.cl-tbl td.num,.cl-tbl th.num{text-align:right;font-variant-numeric:tabular-nums}
.cl-tbl .ratebar{display:inline-block;height:6px;border-radius:3px;background:var(--surface-2);overflow:hidden;vertical-align:middle;margin-left:8px;width:70px}
.cl-tbl .ratebar i{display:block;height:100%;background:var(--ok)}
.cl-tbl .ratebar i.warn{background:var(--orange)}
.cl-tbl .ratebar i.bad{background:var(--red)}
.cl-empty{text-align:center;color:var(--text-ter);padding:34px 0;font-size:13px}

/* ---------- 响应式 ---------- */
@media(max-width:960px){
  .sidebar{width:64px;padding:16px 8px}
  .nav button span,.brand .txt,.sidebar-foot .txt{display:none}
  .conn .ctxt{display:none}
  .conn .cshort{display:inline}
  .mini{display:none}
  .nav button{justify-content:center}
  .nav button.active::before{top:14%;bottom:14%}
  .main{padding:20px 16px 60px}
  .stats-row{grid-template-columns:repeat(2,1fr)}
  .nedit{grid-template-columns:1fr 1fr 90px 64px}
  .nedit select{grid-column:1/-1}
}
@media(max-width:560px){
  .stats-row{grid-template-columns:1fr}
}
/* 尊重系统减弱动态偏好：去掉非必要脉冲与转场动画 */
@media (prefers-reduced-motion:reduce){
  .conn .pulse::after{animation:none}
  .page.active{animation:none}
}
</style>
</head>
<body>

<!-- ============ 登录视图 ============ -->
<div id="view-app" class="app">
<aside class="sidebar">
  <div class="brand">
    <div class="logo-mark"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M8 20l6-8h4l-6 8z"/><path d="M4 20h4"/><path d="M12 10V4a2 2 0 00-2-2H4a2 2 0 00-2 2v9"/></svg></div>
    <div class="txt"><div class="logo-text">opencode<span class="logo-accent">2api</span></div><div class="logo-sub">ADMIN · LOCAL PROXY</div></div>
  </div>
  <nav class="nav" id="nav">
    <button data-page="overview" class="active" onclick="showPage('overview')"><svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="3" width="7" height="9" rx="1"/><rect x="14" y="3" width="7" height="5" rx="1"/><rect x="14" y="12" width="7" height="9" rx="1"/><rect x="3" y="16" width="7" height="5" rx="1"/></svg><span>概览</span></button>
    <button data-page="nodes" onclick="showPage('nodes')"><svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="6" y="6" width="12" height="12" rx="2"/><circle cx="9" cy="9" r="1.2" fill="currentColor"/><circle cx="15" cy="15" r="1.2" fill="currentColor"/><path d="M9 15l6-6"/></svg><span>节点池</span></button>
    <button data-page="subs" onclick="showPage('subs')"><svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M4 8h10l3-3h3v14H4z"/><path d="M4 8v11a1 1 0 001 1h15"/></svg><span>订阅与配额</span></button>
    <button data-page="misc" onclick="showPage('misc')"><svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><line x1="4" y1="6" x2="20" y2="6"/><circle cx="15" cy="6" r="2.4" fill="var(--surface)"/><line x1="4" y1="18" x2="20" y2="18"/><circle cx="9" cy="18" r="2.4" fill="var(--surface)"/></svg><span>代理与模型</span></button>
    <button data-page="logs" onclick="showPage('logs')"><svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M4 5h16v14H4z"/><line x1="8" y1="9" x2="16" y2="9"/><line x1="8" y1="13" x2="16" y2="13"/><line x1="8" y1="17" x2="13" y2="17"/></svg><span>运行日志</span></button>
  </nav>
  <div class="sidebar-foot">
    <div class="mini mono" id="sideAddr">-</div>
    <div class="conn" id="connState"><span class="pulse"></span><span class="ctxt">连接中…</span><span class="cshort">演示</span></div>
    <button class="btn btn-ghost btn-block theme-btn-ic" type="button" onclick="toggleTheme()" style="justify-content:center" aria-label="切换主题"><span class="txt">主题切换</span><svg class="ic-moon" width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 12.8A9 9 0 1111.2 3a7 7 0 009.8 9.8z"/></svg><svg class="ic-sun" width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg></button>
    <form method="post" action="/logout" style="margin:0"><button class="btn btn-ghost btn-block" type="submit" style="justify-content:center" aria-label="退出登录"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 21H5a2 2 0 01-2-2V5a2 2 0 012-2h4"/><path d="M16 17l5-5-5-5"/><path d="M21 12H9"/></svg><span class="txt">退出登录</span></button></form>
  </div>
</aside>

<main class="main">
  <div class="topbar">
    <h1 id="pageTitle">概览<span class="crumb" id="pageCrumb">OPENCODE TO API</span></h1>
    <button class="btn btn-ghost btn-sm" onclick="reloadConfig()"><span class="lbl">刷新会话 &amp; 模型</span></button>
    <button class="btn btn-primary btn-sm" onclick="loadAll()"><span class="lbl">刷新数据</span></button>
  </div>

  <!-- ============ 概览 ============ -->
  <section id="page-overview" class="page active" data-od-id="overview">
    <div class="stats-row">
      <div class="stat"><div class="k"><svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><rect x="6" y="6" width="12" height="12" rx="2"/><circle cx="9" cy="9" r="1.2" fill="currentColor"/><circle cx="15" cy="15" r="1.2" fill="currentColor"/></svg>节点总数</div><div class="v blue" id="ovTotal">-</div><div class="sub" id="ovActive">当前: -</div></div>
      <div class="stat"><div class="k"><svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 12H4"/><path d="M4 12l5-5M4 12l5 5"/></svg>可用</div><div class="v green" id="ovHealthy">-</div><div class="sub" id="ovManual">当前节点指纹</div></div>
      <div class="stat"><div class="k"><svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 8v4l3 3"/></svg>配额冷却</div><div class="v orange" id="ovExhausted">-</div><div class="sub">1h 自动恢复</div></div>
      <div class="stat"><div class="k"><svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 9v4M12 17h.01"/><path d="M10.3 3.9L1.8 18a2 2 0 001.7 3h17a2 2 0 001.7-3L13.7 3.9a2 2 0 00-3.4 0z"/></svg>故障</div><div class="v red" id="ovDead">-</div><div class="sub">60s 后自动重试</div></div>
    </div>
    <div class="card" data-od-id="trends">
      <div class="ut-head">
        <h2 style="margin-bottom:0">用量趋势</h2>
        <div class="ut-tools">
          <label class="ut-refresh">自动刷新
            <select id="trendRefreshSel" onchange="setTrendRefresh()">
              <option value="0">关闭</option>
              <option value="5000">5s</option>
              <option value="10000">10s</option>
              <option value="30000" selected>30s</option>
              <option value="60000">60s</option>
            </select>
          </label>
          <button class="btn btn-ghost btn-sm" onclick="loadTrends(true)">刷新</button>
          <span class="muted" id="trendStatus" style="font-size:11px"></span>
        </div>
      </div>
      <div class="filter-chips" id="rangeChips" style="margin:12px 0 2px">
        <button class="chip active" data-r="today" onclick="setTrendRange('today')">今日</button>
        <button class="chip" data-r="7d" onclick="setTrendRange('7d')">7 天</button>
        <button class="chip" data-r="30d" onclick="setTrendRange('30d')">30 天</button>
      </div>
      <p class="hint">数据来自调用日志 · 今日按小时、7/30 天按天聚簇 · 悬浮查看时段明细。</p>
      <div class="ut-chart" id="reqChart"><div class="ut-empty">正在加载…</div></div>
      <div class="ut-legend" id="reqLegend"></div>
      <div class="ut-chart" id="tokenChart"><div class="ut-empty"></div></div>
      <div class="ut-legend" id="tokenLegend"></div>
    </div>
    <div class="card" data-od-id="gateway">
      <h2>快速接入</h2>
      <p class="hint">客户端 base_url 与 API key 统一从这里复制，自动取当前访问域名 / 端口。</p>
      <div class="gw-grid">
        <div class="gw-box">
          <div class="gw-label">统一 API 地址</div>
          <button class="gw-copy" onclick="copyText(gwBase(),'统一 API 地址')" title="点击复制"><code id="gwAddress"></code><span class="glyph"><svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 012-2h10"/></svg>复制</span></button>
          <div style="font-size:11px;color:var(--text-ter);margin-top:4px">客户端 base_url 填此地址</div>
        </div>
        <div class="gw-box">
          <div class="gw-label">统一密钥</div>
          <button class="gw-copy" onclick="copyText(document.getElementById('apiKeyShow').dataset.v||'','统一密钥')" title="点击复制"><code id="apiKeyShow"></code><span class="glyph"><svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 012-2h10"/></svg>复制</span></button>
          <div style="font-size:11px;color:var(--text-ter);margin-top:4px">留空 = 不启用（无 key 访问仍可用）</div>
        </div>
      </div>
    </div>
    <div class="card" data-od-id="token-stats">
      <h2>Token 统计</h2>
      <div class="card-actions">
        <button class="btn btn-ghost btn-sm" onclick="loadStats()">刷新</button>
        <button class="btn btn-danger btn-sm" onclick="resetStats()">清空统计</button>
        <span id="resetStatus" class="muted" style="font-size:11px"></span>
      </div>
      <div class="model-bars" id="modelBars"></div>
      <div style="overflow-x:auto"><table class="tbl" id="statsTable"><thead><tr><th>模型</th><th class="num">请求数</th><th class="num">输入 Token</th><th class="num">输出 Token</th><th class="num">总计</th></tr></thead><tbody><tr><td colspan="5" class="empty-hint">加载中…</td></tr></tbody></table></div>
    </div>
    <div class="card" data-od-id="model-caps">
      <h2>模型能力</h2>
      <div style="overflow-x:auto"><table class="tbl" id="capTable"><thead><tr><th>模型</th><th class="num">上下文窗口</th><th class="num">最大输出</th><th>输入类型</th></tr></thead><tbody><tr><td colspan="4" class="empty-hint">加载中…</td></tr></tbody></table></div>
    </div>
  </section>

  <!-- ============ 节点池 ============ -->
  <section id="page-nodes" class="page" data-od-id="nodes">
    <div class="filter-chips" id="stateChips">
      <button class="chip active" data-f="all" onclick="setFilter('all')">全部 <span class="cnt" id="cntAll"></span></button>
      <button class="chip" data-f="available" onclick="setFilter('available')">可用 <span class="cnt" id="cntAvail"></span></button>
      <button class="chip" data-f="exhausted" onclick="setFilter('exhausted')">已耗尽 <span class="cnt" id="cntExh"></span></button>
      <button class="chip" data-f="dead" onclick="setFilter('dead')">故障 <span class="cnt" id="cntDead"></span></button>
    </div>
    <div class="actions">
      <button class="btn btn-primary" onclick="reloadSubs()">重新加载订阅</button>
      <button class="btn btn-ghost" onclick="resetAllMarks()">解除全部标记</button>
      <button class="btn btn-ghost" onclick="probeAll()">测速</button>
      <button class="btn btn-ghost" onclick="loadNodes()">刷新节点</button>
      <span id="nodeStatus" class="muted" style="font-size:11px"></span>
    </div>
    <div class="latdist" id="latdist"></div>
    <div class="card" style="padding:14px 0 6px">
      <div style="overflow-x:auto"><table class="tbl" id="nodeTable">
        <thead><tr><th style="width:20%">名称</th><th style="width:9%">协议</th><th style="width:9%">状态</th><th class="num" style="width:9%">延迟</th><th style="width:16%">冷却至</th><th style="width:27%">最近错误</th><th style="width:10%"></th></tr></thead>
        <tbody></tbody>
      </table></div>
    </div>
  </section>

  <!-- ============ 订阅与配额 ============ -->
  <section id="page-subs" class="page" data-od-id="subs">
    <div class="card">
      <h2>订阅源</h2>
      <p class="hint">Clash YAML / 行式 URI / base64 包裹。修改后点底部「保存订阅与节点」立即生效。</p>
      <div style="overflow-x:auto"><table class="tbl" id="subTable">
        <thead><tr><th style="width:13%">名称</th><th style="width:26%">URL</th><th style="width:8%">间隔(小时)</th><th class="num" style="width:7%">节点数</th><th style="width:18%">流量</th><th style="width:14%">上次拉取</th><th style="width:9%"></th></tr></thead>
        <tbody></tbody>
      </table></div>
      <div class="actions"><button class="btn btn-secondary" onclick="addSub()">添加订阅源</button></div>
    </div>
    <div class="card">
      <h2>Webshare 代理池</h2>
      <p class="hint">填 webshare.io API key，自动从代理列表 API 拉取 SOCKS5 代理并入节点池（失效代理自动排除，健康检查由节点池统一负责）。本机访问 webshare 网络不佳时可在「代理」列填中转（http/https/socks5，如 http://127.0.0.1:7890），留空则用环境变量 HTTP(S)_PROXY。</p>
      <div style="overflow-x:auto"><table class="tbl" id="webshareTable">
        <thead><tr><th style="width:11%">名称</th><th style="width:22%">API Key</th><th style="width:18%">代理 (拉取 API 用)</th><th style="width:9%">间隔(小时)</th><th class="num" style="width:6%">节点数</th><th style="width:12%">上次拉取</th><th style="width:15%">最近错误</th><th style="width:7%"></th></tr></thead>
        <tbody></tbody>
      </table></div>
      <div class="actions"><button class="btn btn-secondary" onclick="addWebshare()">添加 Webshare 源</button></div>
    </div>
    <div class="card">
      <h2>手动配置节点</h2>
      <p class="hint">与订阅合并进节点池（订阅节点优先按指纹去重）。</p>
      <div id="manualEditors"></div>
      <div class="actions"><button class="btn btn-secondary" onclick="addNodeEditor()">添加节点</button></div>
    </div>
    <div class="card">
      <h2>免费额度切换</h2>
      <p class="hint">免费层请求遇到配额耗尽信号时自动标记当前节点并切换下一个节点重试。</p>
      <div class="form-grid" style="display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:12px">
        <div class="field"><label>error.type 命中（逗号分隔）</label><input id="q_error_types" placeholder="FreeUsageLimitError, insufficient_quota"></div>
        <div class="field"><label>error.message 关键词（逗号分隔）</label><input id="q_message_kw" placeholder="free usage limit, quota, limit exceeded"></div>
        <div class="field"><label>单请求最大切换次数</label><input id="q_max_switches" type="number" min="0" max="20" value="5"></div>
        <div class="field"><label>耗尽冷却（小时）</label><input id="q_cooldown_h" type="number" min="0" max="168" value="1"></div>
        <div class="field"><label>故障复探间隔（分钟）</label><input id="q_cooldown_m" type="number" min="0" max="1440" value="1"></div>
      </div>
      <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:12px;margin-top:12px">
        <div class="field"><label>健康检查间隔（分钟，0=默认15）</label><input id="q_health_interval" type="number" min="1" max="1440" value="15"></div>
        <div class="field"><label>健康检查探针 URL</label><input id="q_health_url" placeholder="https://www.gstatic.com/generate_204"></div>
      </div>
      <div class="muted" style="font-size:11.5px;margin-top:8px">403 且无上述签名视为耗尽；429 无签名不视为耗尽。耗尽冷却到期自动恢复（标记清除）；故障节点每分钟复探，探测成功即恢复。</div>
    </div>
    <div class="actions" style="margin-top:4px">
      <button class="btn btn-primary" onclick="saveSubsConfig()">保存订阅与节点</button>
      <button class="btn btn-secondary" onclick="saveQuotaConfig()">保存配额设置</button>
      <span id="subSaveStatus" class="muted" style="font-size:11px"></span>
    </div>
  </section>

  <!-- ============ 代理与模型 ============ -->
  <section id="page-misc" class="page" data-od-id="misc">
    <div class="card">
      <h2>推理力度映射</h2>
      <div style="overflow-x:auto;margin-bottom:4px"><table class="tbl" id="effortTable"><thead><tr><th style="width:38%">请求值</th><th style="width:44%">映射值</th><th style="width:18%"></th></tr></thead><tbody></tbody></table></div>
      <div class="check"><input type="checkbox" id="force_disable_thinking"><label for="force_disable_thinking">强制禁用思考模式</label><span class="muted" style="font-size:11px">移除所有推理内容</span></div>
      <div class="actions"><button class="btn btn-secondary" onclick="addEffortRow()">添加映射</button><button class="btn btn-primary" onclick="saveConfig()">保存全部</button></div>
    </div>
    <div class="card">
      <h2>模型映射</h2>
      <div style="overflow-x:auto"><table class="tbl" id="aliasTable"><thead><tr><th style="width:38%">别名（请求名）</th><th style="width:44%">实际模型（上游名）</th><th style="width:18%"></th></tr></thead><tbody></tbody></table></div>
      <div class="actions"><button class="btn btn-secondary" onclick="addAliasRow()">添加别名</button><button class="btn btn-primary" onclick="saveConfig()">保存全部</button></div>
    </div>
    <div class="card">
      <h2>SOCKS5 代理</h2>
      <div style="overflow-x:auto;margin-bottom:12px"><table class="tbl" id="socks5Table"><thead><tr><th style="width:22%">名称</th><th style="width:26%">地址</th><th style="width:16%">用户名</th><th style="width:16%">密码</th><th style="width:12%"></th></tr></thead><tbody></tbody></table></div>
      <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:12px">
        <div class="field"><label>启用代理</label><select id="activeSocks5"><option value="">直连（不使用代理）</option></select></div>
        <div class="field"><label>带 key / 付费请求直连</label><select id="socks5_paid_direct"><option value="0">走代理（默认）</option><option value="1">直连</option></select></div>
      </div>
      <div class="actions"><button class="btn btn-secondary" onclick="addSocks5Row()">添加代理</button><button class="btn btn-primary" onclick="saveConfig()">保存全部</button></div>
    </div>
    <div class="card">
      <h2>统一网关</h2>
      <div class="gw-grid">
        <div class="gw-box">
          <div class="gw-label">统一 API 地址</div>
          <button class="gw-copy" onclick="copyText(gwBase(),'统一 API 地址')" title="点击复制"><code id="gwAddress2"></code><span class="glyph"><svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 012-2h10"/></svg>复制</span></button>
          <div class="muted" style="font-size:11px;margin-top:4px">客户端 base_url 填此地址（自动取当前访问域名/端口）</div>
        </div>
        <div class="gw-box">
          <div class="gw-label">统一密钥</div>
          <button class="gw-copy" onclick="copyText(document.getElementById('apiKeyShow2').dataset.v||'','统一密钥')" title="点击复制"><code id="apiKeyShow2"></code><span class="glyph"><svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 012-2h10"/></svg>复制</span></button>
          <div class="muted" style="font-size:11px;margin-top:4px">API key 填此值；留空 = 不启用（无 key 访问仍可用）</div>
        </div>
      </div>
      <div class="field" style="margin-top:12px;max-width:520px"><label>修改 api_key（统一密钥）</label><div style="display:flex;gap:8px"><input id="apiKeyInput" type="password" placeholder="留空 = 不启用" autocomplete="off"><button class="btn btn-ghost" onclick="genApiKey()" title="随机生成一把 sk- 开头的密钥">生成</button></div><span class="hint">客户端/工具统一填这把 key 即可通过密钥校验；上游仍走 public 免费档</span></div>
      <div class="actions"><button class="btn btn-primary" onclick="saveConfig()">保存全部</button></div>
    </div>
  </section>

  <!-- ============ 运行日志 ============ -->
  <section id="page-logs" class="page" data-od-id="logs">
    <div class="card">
      <h2>运行日志</h2>
      <div class="log-tabs">
        <button id="logTabRaw" class="active" onclick="showLogTab('raw')">原始日志</button>
        <button id="logTabCall" onclick="showLogTab('call')">调用日志</button>
      </div>
      <div id="rawLogView">
      <div class="filter-chips" id="logLevelChips" style="margin:0">
        <button class="chip active" data-l="all" onclick="setLogLevelFilter('all')">全部</button>
        <button class="chip" data-l="DEBUG" onclick="setLogLevelFilter('DEBUG')">Debug</button>
        <button class="chip" data-l="INFO" onclick="setLogLevelFilter('INFO')">Info</button>
        <button class="chip" data-l="WARN" onclick="setLogLevelFilter('WARN')">Warn</button>
        <button class="chip" data-l="ERROR" onclick="setLogLevelFilter('ERROR')">Error</button>
      </div>
      <div class="log-toolbar" style="margin-top:0">
        <input id="logKeyword" type="text" placeholder="关键词过滤（消息 / 字段值）" style="flex:1;min-width:180px;padding:6px 10px;border:1px solid var(--border);border-radius:8px;background:var(--bg);color:var(--text)" oninput="setLogKeyword(this.value)">
        <button class="btn btn-ghost" onclick="exportLogs()"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="vertical-align:-2px"><path d="M12 3v12"/><path d="M7 10l5 5 5-5"/><path d="M5 21h14"/></svg><span class="lbl">导出日志</span></button>
        <span id="logCount" class="muted" style="font-size:12.5px;margin-left:auto">缓冲 0 条 / 显示 0 条</span>
      </div>
      <div class="log-toolbar" style="margin-top:0">
        <label style="display:flex;align-items:center;gap:6px;font-size:13px"><input type="checkbox" id="logAutoRefresh" checked onchange="toggleLogStream(this.checked)">自动刷新</label>
        <label style="display:flex;align-items:center;gap:6px;font-size:13px">运行级别
          <select id="logRunLevel" onchange="setLogRunLevel(this.value)" style="padding:5px 8px;border-radius:7px;border:1px solid var(--border);background:var(--bg);color:var(--text)">
            <option value="debug">debug</option><option value="info">info</option><option value="warn">warn</option><option value="error">error</option>
          </select>
        </label>
      </div>
      <div class="log-wrap" id="logList"></div>
      <div style="display:flex;align-items:center;margin-top:8px">
        <span id="logStatus" class="muted" style="font-size:12px">等待日志流…</span>
        <button id="logScrollBtn" class="btn btn-ghost btn-sm" style="display:none;margin-left:auto" onclick="logScrollToBottom()">回到底部 ↓</button>
      </div>
      </div>
      <div id="callLogView" style="display:none">
        <div class="cl-card">
          <div class="cl-summary">
            <span>共 <b id="clTotal">0</b> 条</span>
            <span class="c-ok">【成功】<b id="clOk">0</b></span>
            <span class="c-fail">【失败】<b id="clFail">0</b></span>
            <span class="c-issue">异常/切换 <b id="clIssue">0</b></span>
            <span style="margin-left:auto;display:flex;gap:8px">
              <button class="btn btn-ghost btn-sm" onclick="clearCallLog()"><svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="vertical-align:-2px"><path d="M3 6h18"/><path d="M8 6V4a2 2 0 012-2h4a2 2 0 012 2v2"/><path d="M19 6l-1 14a2 2 0 01-2 2H8a2 2 0 01-2-2L5 6"/></svg> 清空</button>
              <button class="btn btn-sm" onclick="loadCallLog(true)">刷新</button>
            </span>
          </div>
          <div class="cl-filter">
            <input id="clKeyword" type="text" placeholder="关键词（模型 / 路径 / 错误 / 请求ID）" oninput="clRefreshView()">
            <select id="clDate" onchange="clRefreshView()"><option value="">全部日期</option></select>
            <label style="display:flex;align-items:center;gap:6px;font-size:13px;cursor:pointer"><input type="checkbox" id="clOnlyIssues" onchange="clRefreshView()">只看失败/切换</label>
            <span style="margin-left:auto;display:flex;gap:4px;background:var(--surface-2);border-radius:8px;padding:3px">
              <button class="btn btn-sm" id="clViewList" onclick="clSetView('list')">列表</button>
              <button class="btn btn-ghost btn-sm" id="clViewHour" onclick="clSetView('hour')">时段分析</button>
              <button class="btn btn-ghost btn-sm" id="clViewNode" onclick="clSetView('node')">节点分析</button>
            </span>
          </div>
          <div id="clList"></div>
          <div id="clHour" style="display:none"></div>
          <div id="clNode" style="display:none"></div>
          <div style="display:flex;gap:6px;color:var(--text-ter);font-size:12px;margin-top:10px">
            <span>每 5 秒自动刷新</span><span id="clStatus" style="margin-left:auto"></span>
          </div>
        </div>
      </div>
    </div>
  </section>
</main>
</div>

<div id="toast"></div>

<script>
/* ============================================================
   核心状态与工具
   ============================================================ */
const q=s=>document.querySelector(s);
const qa=s=>document.querySelectorAll(s);
let cfg={},nodeData=[],subData=[],wsData=[],wsInfo={},nodeEditors=[],modelList=[],filter='all';
let DEMO_MODE=false,demoLogTimer=null;
let trendsData=[],trendRange='today',trendTimer=null;

/* ---------- 主题：默认深色 ---------- */
function applyTheme(){
  document.documentElement.setAttribute('data-theme',localStorage.getItem('theme')||'dark');
}
function toggleTheme(){
  const cur=document.documentElement.getAttribute('data-theme');
  localStorage.setItem('theme',cur==='dark'?'light':'dark');
  applyTheme();
}

/* ---------- 导航 ---------- */
const pageTitles={overview:['概览','OPENCODE TO API'],nodes:['节点池','节点健康与切换'],subs:['订阅与配额','订阅源 / 手动节点 / 免费额度'],misc:['代理与模型','推理 / 别名 / SOCKS5 / 网关'],logs:['运行日志','实时流式日志']};
function showPage(id){
  qa('.page').forEach(p=>p.classList.remove('active'));
  q('#page-'+id).classList.add('active');
  qa('.nav button').forEach(b=>b.classList.toggle('active',b.dataset.page===id));
  q('#pageTitle').firstChild.textContent=pageTitles[id][0]+' ';
  q('#pageCrumb').textContent=pageTitles[id][1];
  if(id==='logs'&&!DEMO_MODE)initLogs();
}

/* ---------- 工具 ---------- */
function esc(s){const d=document.createElement('div');d.textContent=s==null?'':String(s);return d.innerHTML}
function fmt(n){return Number(n||0).toString().replace(/\B(?=(\d{3})+(?!\d))/g,',')}
let toastT;
function showToast(msg,t){const e=q('#toast');e.textContent=msg;e.className=t==='error'?'error show':'show';clearTimeout(toastT);toastT=setTimeout(()=>e.classList.remove('show'),2600)}
function copyText(text,label){if(!text){showToast('暂无可复制内容','error');return}const done=()=>showToast('已复制'+label,'success');if(navigator.clipboard&&window.isSecureContext){navigator.clipboard.writeText(text).then(done).catch(()=>fallbackCopy(text,done))}else{fallbackCopy(text,done)}}
function fallbackCopy(text,done){const ta=document.createElement('textarea');ta.value=text;ta.style.position='fixed';ta.style.opacity='0';document.body.appendChild(ta);ta.select();try{document.execCommand('copy');done()}catch{showToast('复制失败，请手动复制','error')}document.body.removeChild(ta)}
function fmtBytes(b){b=Number(b)||0;if(b<=0)return '0B';const u=['B','KB','MB','GB','TB'];let i=0;while(b>=1024&&i<u.length-1){b/=1024;i++}return (b>=100?b.toFixed(0):b.toFixed(1))+u[i]}
function fmtTokens(n){if(n==null||isNaN(n))return '-';if(n>=1000000)return (n/1000000).toFixed(n%1000000===0?0:1)+'M';if(n>=1000)return (n/1000).toFixed(n%1000===0?0:1)+'K';return ''+n}
function fmtTime(t){return t?String(t).replace('T',' ').slice(0,16):'-'}
function fmtModalities(list){if(!list||!list.length)return '-';const names={text:'文本',image:'图像',audio:'音频',video:'视频',pdf:'PDF'};return list.map(m=>names[m]||m).join(' / ')}
function shortFp(fp){return fp?fp.slice(0,8):''}
function latClass(ms){return ms<150?'':ms<400?'lat-warn':'lat-high'}
function setConn(online){const el=q('#connState');el.classList.toggle('offline',!online);const full=online?'已连接':'演示数据 · 未连接后端';const short=online?'已连接':'演示';const a=el.querySelector('.ctxt');if(a)a.textContent=full;const b=el.querySelector('.cshort');if(b)b.textContent=short;el.title=full}

/* ============================================================
   数据加载（真实后端 / 离线演示兜底）
   ============================================================ */
async function loadAll(){await Promise.all([loadConfig(),loadNodes(),loadStats(),loadCaps(),fetchModelList(),loadTrends()]);renderConfigEditors()}
async function loadConfig(){
  if(DEMO_MODE){cfg=JSON.parse(JSON.stringify(DEMO.cfg));return}
  try{const r=await fetch('/api/config');if(!r.ok)throw new Error(await r.text());cfg=await r.json()}catch(e){showToast('配置加载失败: '+e.message,'error')}
}
async function loadNodes(){
  if(DEMO_MODE){nodeData=DEMO.nodes.map(n=>({...n}));subData=DEMO.subscriptions.filter(s=>s.url!=='webshare API').map(s=>({...s}));wsData=DEMO.subscriptions.filter(s=>s.url==='webshare API').map(s=>({name:s.name,nodes:s.nodes,last_updated_at:s.last_updated_at}));renderOverview({nodes:nodeData,healthy:nodeData.filter(n=>n.state==='available').length,exhausted_count:nodeData.filter(n=>n.state==='exhausted').length,dead_count:nodeData.filter(n=>n.state==='dead').length,active_name:nodeData.find(n=>n.active)?.name||'',manual_name:''});renderNodeTable();renderLatencyDist(nodeData);renderSubTable();renderWebshareTable();return}
  try{const r=await fetch('/api/nodes');if(!r.ok)throw new Error(await r.text());const d=await r.json();nodeData=d.nodes||[];subData=(d.subscriptions||[]).filter(s=>s.url!=='webshare API');applyWsSnapshot(d.subscriptions||[]);renderOverview(d);renderNodeTable();renderLatencyDist(nodeData);renderSubTable();renderWebshareTable()}catch(e){q('#nodeTable tbody').innerHTML='<tr><td colspan="7" class="empty-hint">加载失败: '+esc(e.message)+' <button class="btn btn-sm btn-ghost" onclick="loadNodes()">重试</button></td></tr>'}
}
async function loadStats(){
  if(DEMO_MODE){renderStats(DEMO.stats);return}
  try{const r=await fetch('/api/stats');const d=await r.json();renderStats(d)}catch(e){const tb=q('#statsTable tbody');if(tb)tb.innerHTML='<tr><td colspan="5" class="empty-hint">加载失败 <button class="btn btn-sm btn-ghost" onclick="loadStats()">重试</button></td></tr>'}
}
async function loadCaps(){
  if(DEMO_MODE){renderCaps(DEMO.caps);return}
  try{const r=await fetch('/v1/models');if(!r.ok)throw new Error('HTTP '+r.status);const d=await r.json();renderCaps((d.data||[]).map(x=>({id:x.id,cw:x.context_window,mo:x.max_output_tokens,mod:x.input_modalities})))}
  catch(e){q('#capTable').innerHTML='<thead><tr><th>模型</th><th class="num">上下文窗口</th><th class="num">最大输出</th><th>输入类型</th></tr></thead><tbody><tr><td colspan="4" class="empty-hint">加载失败: '+esc(e.message)+' <button class="btn btn-sm btn-ghost" onclick="loadCaps()">重试</button></td></tr></tbody>'}
}
async function fetchModelList(){if(DEMO_MODE){modelList=DEMO.models.slice().sort();return}try{let m=await fetch('/api/models');if(m.ok){const d=await m.json();modelList=(d.data||[]).map(x=>typeof x==='string'?x:x.id).filter(Boolean).sort()}else if(m.status===401){const v=await fetch('/v1/models');if(v.ok){const j=await v.json();modelList=(j.data||[]).map(x=>x.id||x).filter(Boolean).sort()}}}catch(e){}}
function renderOverview(d){
  q('#ovTotal').textContent=d.nodes?d.nodes.length:0;
  q('#ovHealthy').textContent=d.healthy??0;
  q('#ovExhausted').textContent=d.exhausted_count??0;
  q('#ovDead').textContent=d.dead_count??0;
  q('#ovActive').textContent=d.active_name?('当前: '+d.active_name):(d.active_fp?'当前: '+shortFp(d.active_fp):'直连');
  q('#ovManual').textContent=d.manual_name?('手动: '+d.manual_name):(d.manual_fp?shortFp(d.manual_fp):'自动轮询');
}
function renderStats(d){
  const ms=d.models||{};const ks=Object.keys(ms);let h='';
  if(!ks.length){h='<tr><td colspan="5" class="empty-hint">暂无数据</td></tr>'}
  else{let tr=0,pt=0,ct=0,tt=0;for(const k of ks){const m=ms[k];h+='<tr><td class="mono">'+esc(k)+'</td><td class="num">'+fmt(m.request_count)+'</td><td class="num">'+fmt(m.prompt_tokens)+'</td><td class="num">'+fmt(m.completion_tokens)+'</td><td class="num">'+fmt(m.total_tokens)+'</td></tr>';tr+=m.request_count;pt+=m.prompt_tokens;ct+=m.completion_tokens;tt+=m.total_tokens}h+='<tr><td style="font-weight:700">总计</td><td class="num" style="font-weight:700">'+fmt(tr)+'</td><td class="num" style="font-weight:700">'+fmt(pt)+'</td><td class="num" style="font-weight:700">'+fmt(ct)+'</td><td class="num" style="font-weight:700">'+fmt(tt)+'</td></tr>'}
  q('#statsTable').innerHTML='<thead><tr><th>模型</th><th class="num">请求数</th><th class="num">输入 Token</th><th class="num">输出 Token</th><th class="num">总计</th></tr></thead><tbody>'+h+'</tbody>';
  renderModelBars(ms);
}
function renderCaps(rows){
  rows=(rows||[]).filter(x=>x&&x.id).sort((a,b)=>a.id.localeCompare(b.id));
  let h='';
  if(!rows.length){h='<tr><td colspan="4" class="empty-hint">暂无模型数据</td></tr>'}
  else{for(const m of rows){h+='<tr><td class="mono">'+esc(m.id)+'</td><td class="num">'+fmtTokens(m.cw)+'</td><td class="num">'+fmtTokens(m.mo)+'</td><td>'+fmtModalities(m.mod)+'</td></tr>'}}
  q('#capTable').innerHTML='<thead><tr><th>模型</th><th class="num">上下文窗口</th><th class="num">最大输出</th><th>输入类型</th></tr></thead><tbody>'+h+'</tbody>';
}

/* ---------- 请求分布：按模型占比（真实来自 /api/stats） ---------- */
function renderModelBars(ms){
  const box=q('#modelBars');if(!box)return;
  const rows=Object.keys(ms||{}).map(k=>({k,req:Number(ms[k].request_count)||0})).sort((a,b)=>b.req-a.req);
  const total=rows.reduce((s,r)=>s+r.req,0);
  if(!total||!rows.length){box.innerHTML='';return}
  const top=rows.slice(0,5);
  const restReq=rows.slice(5).reduce((s,r)=>s+r.req,0);
  if(restReq>0)top.push({k:'其他 '+(rows.length-5)+' 个模型',req:restReq});
  box.innerHTML='<div class="mb-head">请求分布</div>'+top.map(r=>{
    const pct=Math.max(1,Math.round(r.req/total*100));
    return '<div class="mbar"><span class="lbl" title="'+esc(r.k)+'">'+esc(r.k)+'</span><span class="track"><i style="width:'+pct+'%"></i></span><span class="val">'+fmt(r.req)+'</span></div>';
  }).join('');
}
/* ---------- 延迟分布：按节点延迟分档（真实来自 /api/nodes） ---------- */
function renderLatencyDist(nodes){
  const el=q('#latdist');if(!el)return;
  const lat=(nodes||[]).filter(n=>n.latency_ms!=null&&n.latency_ms>=0).map(n=>n.latency_ms);
  if(!lat.length){el.innerHTML='';return}
  const buckets=[
    {k:'<100ms',cls:'g',f:l=>l<100},
    {k:'<200ms',cls:'b',f:l=>l<200},
    {k:'<400ms',cls:'o',f:l=>l<400},
    {k:'>=400ms',cls:'r',f:l=>l>=400}
  ];
  const total=lat.length;
  let track='',legend='';
  for(const b of buckets){
    const c=lat.filter(b.f).length;
    const pct=Math.round(c/total*100);
    track+='<i class="ld-seg '+b.cls+'" style="width:'+pct+'%"></i>';
    legend+='<span class="ld-key"><i class="'+b.cls+'"></i>'+b.k+' '+c+'</span>';
  }
  el.innerHTML='<div class="row"><span class="ld-title">延迟分布</span><div class="ld-track">'+track+'</div></div><div class="ld-legend">'+legend+'</div>';
}

/* ---------- 用量趋势：请求 / Token（数据来自 /api/stats/trends） ---------- */
function utColor(ref){const v=getComputedStyle(document.documentElement).getPropertyValue(ref).trim();return v||'#6f96ff'}
function utFmtYMD(d){const p=n=>String(n).padStart(2,'0');return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())}
function utNiceMax(v){if(v<=0)return 1;const p=Math.pow(10,Math.floor(Math.log10(v)));const d=v/p;return (d<=1?1:d<=2?2:d<=5?5:10)*p}
function utLabel(ts,short){if(short){const p=ts.split('T');return p.length>1?p[1].slice(0,5):p[0].slice(5)}return ts.replace('T',' ').slice(0,16)}
function trendTotals(){
  const b=trendsData||[];const t={req:0,ok:0,fail:0,pt:0,ct:0,cc:0,cr:0};
  for(const x of b){t.req+=x.requests||0;t.ok+=x.ok||0;t.fail+=x.fail||0;t.pt+=x.prompt_tokens||0;t.ct+=x.completion_tokens||0;t.cc+=x.cache_creation_tokens||0;t.cr+=x.cache_read_tokens||0}
  return t;
}
function renderTrendLegend(){
  const t=trendTotals();
  const rl=q('#reqLegend');
  if(rl)rl.innerHTML='<span class="ut-key"><i style="background:var(--accent)"></i>请求总数 <b>'+fmt(t.req)+'</b></span><span class="ut-key"><i style="background:var(--ok)"></i>成功 <b>'+fmt(t.ok)+'</b></span><span class="ut-key"><i style="background:var(--red)"></i>失败 <b>'+fmt(t.fail)+'</b></span>';
  const tl=q('#tokenLegend');
  const hr=trendHitRate(t);
  let html='<span class="ut-key"><i style="background:var(--blue)"></i>输入 Token <b>'+fmt(t.pt)+'</b></span><span class="ut-key"><i style="background:var(--ok)"></i>输出 Token <b>'+fmt(t.ct)+'</b></span><span class="ut-key"><i style="background:var(--orange)"></i>缓存创建 <b>'+fmt(t.cc)+'</b></span><span class="ut-key"><i style="background:var(--purple)"></i>缓存命中 <b>'+fmt(t.cr)+'</b></span>';
  if(hr!=null)html+='<span class="ut-key ut-rate">缓存命中率 <b>'+hr+'</b></span>';
  if(tl)tl.innerHTML=html;
}
function trendHitRate(t){
  const tot=t.cr+t.cc;
  if(!tot)return null;
  return Math.round(t.cr/tot*100)+'%';
}
function utDrawChart(elId,defs,shortLabels){
  const el=q('#'+elId);if(!el)return;
  const n=trendsData.length;
  if(!n){el.innerHTML='<div class="ut-empty">暂无数据 · 发起一次请求后查看趋势</div>';return}
  const W=Math.max(320,el.clientWidth||640),H=176,L=46,R=10,T=10,B=24;
  const iw=W-L-R,ih=H-T-B;
  let max=0;
  for(const s of defs)for(const v of s.data)if(v>max)max=v;
  const yMax=utNiceMax(max);
  const X=i=>L+(n===1?iw/2:i*iw/(n-1));
  const Y=v=>T+ih-(v/yMax)*ih;
  const px=x=>x.toFixed(1);
  let svg='<svg width="'+W+'" height="'+H+'" role="img" aria-label="用量趋势图">';
  for(let i=0;i<=4;i++){
    const v=yMax*i/4,y=Y(v);
    svg+='<line x1="'+L+'" y1="'+y+'" x2="'+(L+iw)+'" y2="'+y+'" stroke="var(--line)" stroke-width="1"></line>';
    svg+='<text x="'+(L-7)+'" y="'+(y+3.5)+'" text-anchor="end" font-size="10" fill="var(--text-ter)" font-family="var(--mono)">'+fmtTokens(Math.round(v))+'</text>';
  }
  const step=n>10?Math.ceil(n/6):1;
  for(let i=0;i<n;i+=step){
    svg+='<text x="'+X(i)+'" y="'+(H-6)+'" text-anchor="middle" font-size="10" fill="var(--text-ter)" font-family="var(--mono)">'+utLabel(trendsData[i].ts,shortLabels)+'</text>';
  }
  for(const s of defs){
    let area='',pts='';
    for(let i=0;i<n;i++){
      const x=X(i),y=Y(s.data[i]);
      if(i===0)area='M'+px(x)+','+px(H-B);
      area+=' L'+px(x)+','+px(y);
      pts+=(i?' ':'')+px(x)+','+px(y);
    }
    if(n>1){area+=' L'+px(X(n-1))+','+px(H-B)+' Z';svg+='<path d="'+area+'" fill="'+s.color+'" opacity="0.13"></path>'}
    svg+='<polyline points="'+pts+'" fill="none" stroke="'+s.color+'" stroke-width="1.8" stroke-linejoin="round" stroke-linecap="round"></polyline>';
  }
  svg+='</svg>';
  el.innerHTML=svg;
  el._utDefs=defs;el._utShort=shortLabels;
  el._utGeom={W,H,L,R,T,B,iw,ih,yMax};
  if(el._utBound)return;
  el._utBound=true;
  el.addEventListener('mousemove',e=>{
    const rect=el.getBoundingClientRect();
    const g=el._utGeom;
    const nb=trendsData.length;
    if(!nb)return;
    let i=Math.round((e.clientX-rect.left-g.L)/g.iw*(nb-1));
    i=Math.max(0,Math.min(nb-1,i));
    utHoverMark(el,i);
    utShowTip(el,e.clientX,e.clientY,i,defs,shortLabels);
  });
  el.addEventListener('mouseleave',()=>{utHoverMark(el,-1);utHideTip()});
}
function utHoverMark(el,i){
  if(el._utMarkers){
    for(const m of el._utMarkers)m.remove();
    el._utMarkers=null;
  }
  const svg=el.querySelector('svg');if(!svg||i<0)return;
  const g=el._utGeom||{L:46,R:10,T:10,B:24,iw:0,ih:0,yMax:1};
  const defs=el._utDefs||[],n=trendsData.length;
  const X=x=>g.L+(n===1?g.iw/2:x*g.iw/(n-1));
  const Y=y=>g.T+g.ih-(y/g.yMax)*g.ih;
  const marks=[];
  const line=document.createElementNS('http://www.w3.org/2000/svg','line');
  line.setAttribute('x1',X(i));line.setAttribute('x2',X(i));line.setAttribute('y1',g.T);line.setAttribute('y2',g.T+g.ih);
  line.setAttribute('stroke','var(--text-ter)');line.setAttribute('stroke-width','1');line.setAttribute('stroke-dasharray','3 3');
  marks.push(line);
  for(const d of defs){
    const c=document.createElementNS('http://www.w3.org/2000/svg','circle');
    c.setAttribute('cx',X(i));c.setAttribute('cy',Y(d.data[i]));c.setAttribute('r','3.5');
    c.setAttribute('fill',d.color);c.setAttribute('stroke','var(--bg)');c.setAttribute('stroke-width','1.5');
    marks.push(c);
  }
  el._utMarkers=marks;
  for(const m of marks)svg.appendChild(m);
}
function utTipEl(){const el=q('#utTip');if(el)return el;const d=document.createElement('div');d.id='utTip';d.className='ut-tip';d.style.display='none';document.body.appendChild(d);return d}
function utShowTip(el,clientX,clientY,i,defs,shortLabels){
  const b=trendsData[i];if(!b)return;
  const tip=utTipEl();
  const rows=defs.map(d=>'<div>'+d.name+' <b>'+fmt(b[d.key])+'</b></div>').join('');
  const fail=b.fail?'<div style="color:var(--red)">失败 <b>'+fmt(b.fail)+'</b></div>':'';
  tip.innerHTML='<div class="t">'+utLabel(b.ts,false)+'</div>'+rows+fail;
  tip.style.display='block';
  const tw=tip.offsetWidth,th=tip.offsetHeight;
  let x=clientX+14,y=clientY-12;
  if(x+tw>window.innerWidth-8)x=clientX-tw-14;
  if(y+th>window.innerHeight-8)y=clientY-th-12;
  tip.style.left=x+'px';tip.style.top=y+'px';
}
function utHideTip(){const el=q('#utTip');if(el)el.style.display='none'}
function renderTrends(){
  const b=trendsData||[];
  utDrawChart('reqChart',[{key:'requests',name:'请求',color:utColor('--accent'),data:b.map(x=>x.requests||0)}],true);
  utDrawChart('tokenChart',[
    {key:'prompt_tokens',name:'输入 Token',color:utColor('--blue'),data:b.map(x=>x.prompt_tokens||0)},
    {key:'completion_tokens',name:'输出 Token',color:utColor('--ok'),data:b.map(x=>x.completion_tokens||0)},
    {key:'cache_creation_tokens',name:'缓存创建',color:utColor('--orange'),data:b.map(x=>x.cache_creation_tokens||0)},
    {key:'cache_read_tokens',name:'缓存命中',color:utColor('--purple'),data:b.map(x=>x.cache_read_tokens||0)}
  ],true);
  renderTrendLegend();
}
function setTrendRange(r){
  trendRange=r;
  qa('#rangeChips .chip').forEach(c=>c.classList.toggle('active',c.dataset.r===r));
  loadTrends();
}
function setTrendRefresh(){
  const v=parseInt(q('#trendRefreshSel').value||'0',10);
  if(trendTimer){clearInterval(trendTimer);trendTimer=null}
  if(v>0)trendTimer=setInterval(()=>{if(q('#page-overview').classList.contains('active')){loadStats();loadTrends()}},v);
}
function trendDemo(rng){
  const out=[],now=new Date();
  if(rng==='today'){
    for(let h=0;h<24;h++){
      const wave=Math.round(Math.sin((h-9)/24*Math.PI*2)*28+44);
      const req=Math.max(0,wave+Math.round(Math.random()*10));
      out.push({ts:utFmtYMD(now)+'T'+String(h).padStart(2,'0')+':00',requests:req,ok:Math.max(0,req-Math.round(Math.random()*2)),fail:Math.round(Math.random()*2),prompt_tokens:req*(380+Math.round(Math.random()*260)),completion_tokens:req*(180+Math.round(Math.random()*220)),cache_creation_tokens:req*90,cache_read_tokens:req*(300+Math.round(Math.random()*180))});
    }
  }else{
    const days=rng==='7d'?7:30;
    for(let i=days-1;i>=0;i--){
      const d=new Date(now);d.setDate(d.getDate()-i);
      const req=Math.max(0,56+Math.round(Math.sin((days-i)/days*Math.PI*2)*30+Math.random()*24));
      out.push({ts:utFmtYMD(d),requests:req,ok:Math.max(0,req-Math.round(Math.random()*3)),fail:Math.round(Math.random()*3),prompt_tokens:req*(360+Math.round(Math.random()*300)),completion_tokens:req*(170+Math.round(Math.random()*260)),cache_creation_tokens:req*80,cache_read_tokens:req*(280+Math.round(Math.random()*200))});
    }
  }
  return out;
}
async function loadTrends(showStatus){
  if(DEMO_MODE){trendsData=trendDemo(trendRange);renderTrends();if(showStatus)q('#trendStatus').textContent='演示数据';return}
  try{
    const r=await fetch('/api/stats/trends?range='+trendRange);
    if(!r.ok)throw new Error('HTTP '+r.status);
    trendsData=await r.json();
    if(!Array.isArray(trendsData))trendsData=[];
    renderTrends();
    if(showStatus)q('#trendStatus').textContent=clFmtTime(new Date().toISOString())+' 已更新';
  }catch(e){
    if(showStatus)q('#trendStatus').textContent='加载失败: '+e.message;
  }
}

/* ============================================================
   节点池
   ============================================================ */
function setFilter(f){filter=f;qa('#stateChips .chip').forEach(c=>c.classList.toggle('active',c.dataset.f===f));renderNodeTable()}
const badgeMap={available:'<span class="badge available"><span class="bdot"></span>可用</span>',exhausted:'<span class="badge exhausted"><span class="bdot"></span>已耗尽</span>',dead:'<span class="badge dead"><span class="bdot"></span>故障</span>',idle:'<span class="badge idle"><span class="bdot"></span>未知</span>'};
function badgeHtml(s){return badgeMap[s]||badgeMap.idle}
function renderNodeTable(){
  const tb=q('#nodeTable tbody');const rows=nodeData.filter(n=>filter==='all'||n.state===filter);
  q('#cntAll').textContent=nodeData.length;
  q('#cntAvail').textContent=nodeData.filter(n=>n.state==='available').length;
  q('#cntExh').textContent=nodeData.filter(n=>n.state==='exhausted').length;
  q('#cntDead').textContent=nodeData.filter(n=>n.state==='dead').length;
  if(!rows.length){tb.innerHTML='<tr><td colspan="7" class="empty-hint">'+(nodeData.length?'没有匹配状态的节点':'暂无节点（可在「订阅与配额」页添加）')+'</td></tr>';return}
  tb.innerHTML=rows.map(n=>{
    const lat=(n.latency_ms!=null&&n.latency_ms>=0)?'<span class="num '+latClass(n.latency_ms)+'">'+n.latency_ms+' ms</span>':(n.latency_ms!=null&&n.latency_ms<0?'<span class="muted">超时</span>':'<span class="muted">-</span>');
    return '<tr class="'+(n.active?'hl':'')+'"><td><span class="node-name">'+esc(n.name)+(n.active?'<span class="tag cur">当前</span>':'')+(n.manual?'<span class="tag man">手动</span>':'')+'</span></td><td><span class="proto">'+esc(n.protocol)+'</span></td><td>'+badgeHtml(n.state)+'</td><td class="num">'+lat+'</td><td class="num muted">'+esc(fmtTime(n.cooldown_until))+'</td><td class="err">'+esc(n.last_error||'-')+'</td><td class="acts"><button class="btn btn-sm btn-primary" onclick="nodeAction(\'switch\',\''+n.fingerprint+'\')">切换</button> <button class="btn btn-sm btn-ghost" onclick="nodeAction(\'reset\',\''+n.fingerprint+'\')">解除</button></td></tr>';
  }).join('');
}
async function nodeAction(action,fp){
  const st=q('#nodeStatus');st.textContent='操作中…';
  if(DEMO_MODE){await new Promise(r=>setTimeout(r,350));st.textContent=action==='switch'?'已切换':action==='reload'?'订阅已加载':action==='probe'?'测速完成':'已解除';showToast(st.textContent,'success');loadNodes();return}
  try{const r=await fetch('/api/nodes',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({action:action,fingerprint:fp||''})});const d=await r.json();if(!r.ok)throw new Error(d.error||'请求失败');st.textContent=action==='switch'?'已切换':action==='reload'?('订阅已加载，节点 '+d.nodes+' 个'):action==='probe'?('测速完成 '+d.checked+' 个节点'):'已解除';showToast(st.textContent,'success')}catch(e){st.textContent='失败: '+e.message;showToast('操作失败: '+e.message,'error')}finally{loadNodes()}
}
function reloadSubs(){nodeAction('reload','')}
function probeAll(){nodeAction('probe','')}
async function resetAllMarks(){if(!confirm('解除所有节点的耗尽/故障标记？'))return;nodeAction('reset_all','')}

/* ============================================================
   订阅编辑
   ============================================================ */
function subUsageHtml(s){
  if(!s.usage_total&&!s.usage_used&&!s.usage_expire)return '<span class="muted">-</span>';
  let parts=[];
  if(s.usage_total||s.usage_used){const pct=Math.min(100,Math.round((s.usage_used||0)/Math.max(1,s.usage_total)*100));parts.push('<div class="usage"><div class="usage-bar"><i style="width:'+pct+'%"></i></div><span>'+fmtBytes(s.usage_used)+' / '+fmtBytes(s.usage_total)+'</span></div>')}
  if(s.usage_expire)parts.push('<span class="muted" style="font-size:11px">到期 '+new Date(s.usage_expire*1000).toLocaleDateString()+'</span>');
  return parts.join('<span style="width:6px;display:inline-block"></span>');
}
function renderSubTable(){
  const tb=q('#subTable tbody');
  if(!subData.length){tb.innerHTML='<tr><td colspan="7" class="empty-hint">暂无订阅源，添加 Clash/URI/base64 订阅，或直接用手动配置节点</td></tr>';return}
  tb.innerHTML=subData.map((s,i)=>'<tr><td><input value="'+esc(s.name||'')+'" data-f="name" placeholder="订阅名"></td><td><input value="'+esc(s.url||'')+'" data-f="url" placeholder="https://…"></td><td><input type="number" min="0" value="'+(s.update_interval_hours||'')+'" data-f="interval" placeholder="24"></td><td class="num muted">'+(s.nodes||0)+'</td><td>'+subUsageHtml(s)+'</td><td class="num muted" style="font-size:12px">'+esc(fmtTime(s.last_updated_at))+'</td><td class="acts"><button class="btn btn-sm btn-danger" onclick="delSub('+i+')">删除</button></td></tr>'+(s.last_error?'<tr><td colspan="7" class="muted" style="color:var(--red);font-size:12px">'+esc(s.last_error)+'</td></tr>':'')).join('');
}
function addSub(){subData.push({name:'',url:'',update_interval_hours:24});renderSubTable()}
function delSub(i){subData.splice(i,1);renderSubTable()}
function collectSubs(){const rows=qa('#subTable tbody tr');const seen=new Set();const out=[];rows.forEach(tr=>{const nf=tr.querySelector('[data-f="name"]');if(!nf)return;const name=nf.value||'';const u=(tr.querySelector('[data-f="url"]').value||'').trim();const iv=parseInt(tr.querySelector('[data-f="interval"]').value||'0',10)||0;if(!u||seen.has(u))return;seen.add(u);out.push({name:name,url:u,update_interval_hours:iv})});subData=out;return subData}

/* ============================================================
   Webshare 代理池编辑
   ============================================================ */
function applyWsSnapshot(subs){
  wsInfo={};(subs||[]).forEach(s=>{if(s.url==='webshare API'&&s.name)wsInfo[s.name]=s});
}
function renderWebshareTable(){
  const tb=q('#webshareTable tbody');
  if(!wsData.length){tb.innerHTML='<tr><td colspan="8" class="empty-hint">暂无 Webshare 源，填入 API key 后自动拉取代理列表</td></tr>';return}
  tb.innerHTML=wsData.map((w,i)=>{const m=wsInfo[w.name]||{};return '<tr><td><input value="'+esc(w.name||'')+'" data-f="ws-name" placeholder="例如: webshare-主池"></td><td><input value="'+esc(w.api_key||'')+'" data-f="ws-key" type="password" placeholder="Token APIKEY" style="font-family:monospace"></td><td><input value="'+esc(w.proxy_url||'')+'" data-f="ws-proxy" placeholder="http://127.0.0.1:7890 / socks5://" style="font-family:monospace"></td><td><input type="number" min="0" value="'+(w.update_interval_hours||'')+'" data-f="ws-interval" placeholder="24"></td><td class="num muted">'+(m.nodes||0)+'</td><td class="num muted" style="font-size:12px">'+esc(fmtTime(m.last_updated_at||w.last_updated_at))+'</td><td class="muted" style="font-size:12px;color:var(--red)">'+esc(m.last_error||w.last_error||'')+'</td><td class="acts"><button class="btn btn-sm btn-danger" onclick="delWebshare('+i+')">删除</button></td></tr>'}).join('');
}
function addWebshare(){wsData.push({name:'',api_key:'',update_interval_hours:24});renderWebshareTable()}
function delWebshare(i){wsData.splice(i,1);renderWebshareTable()}
function collectWebshare(){
  const out=[];const seen=new Set();
  qa('#webshareTable tbody tr').forEach(tr=>{
    const name=(tr.querySelector('[data-f="ws-name"]').value||'').trim();
    const key=(tr.querySelector('[data-f="ws-key"]').value||'').trim();
    if(!name||!key||seen.has(name))return;
    seen.add(name);
    const iv=parseInt(tr.querySelector('[data-f="ws-interval"]').value||'0',10)||0;
    const proxy=(tr.querySelector('[data-f="ws-proxy"]').value||'').trim();
    out.push({name:name,api_key:key,proxy_url:proxy,update_interval_hours:iv});
  });
  wsData=out;return wsData;
}

/* ============================================================
   手动节点编辑
   ============================================================ */
function nodeExtras(p){
  const ck='<label class="check" style="padding:0"><input type="checkbox" data-f="insecure"> 跳过证书校验</label>';
  if(p==='vless')return '<div class="field"><label>user_id（UUID）</label><input data-f="user_id" placeholder="uuid"></div><div class="field"><label>flow（留空）</label><input data-f="flow"></div><div class="field"><label>sni / server_name</label><input data-f="sni" placeholder="example.com"></div><div class="field"><label>reality public_key</label><input data-f="rp" placeholder=""></div><div class="field"><label>reality short_id</label><input data-f="rs" placeholder=""></div><div class="field"><label>reality spider_x</label><input data-f="rx" placeholder="/"></div><div class="field" style="align-self:end">'+ck+'</div>';
  if(p==='vmess')return '<div class="field"><label>user_id（UUID）</label><input data-f="user_id" placeholder="uuid"></div><div class="field"><label>security（auto/none/aes-128-gcm）</label><input data-f="security" placeholder="auto"></div><div class="field"><label>network（tcp/ws）</label><input data-f="network" placeholder="tcp"></div><div class="field"><label>path（ws 时）</label><input data-f="path"></div><div class="field"><label>host（ws 时）</label><input data-f="host"></div><div class="field"><label>sni / server_name</label><input data-f="sni" placeholder="留空用地址"></div><div class="field"><label class="check" style="padding:0"><input type="checkbox" data-f="tls"> TLS 加密</label></div><div class="field" style="align-self:end">'+ck+'</div>';
  if(p==='trojan')return '<div class="field"><label>password</label><input data-f="password" type="password"></div><div class="field"><label>sni / server_name</label><input data-f="sni" placeholder="留空用地址"></div><div class="field"><label>network（tcp/ws）</label><input data-f="network" placeholder="tcp"></div><div class="field"><label>path（ws 时）</label><input data-f="path"></div><div class="field"><label>host（ws 时）</label><input data-f="host"></div><div class="field"><label class="check" style="padding:0"><input type="checkbox" data-f="tls"> TLS 加密</label></div><div class="field"><label>reality public_key</label><input data-f="rp" placeholder=""></div><div class="field"><label>reality short_id</label><input data-f="rs" placeholder=""></div><div class="field"><label>reality spider_x</label><input data-f="rx" placeholder="/"></div><div class="field" style="align-self:end">'+ck+'</div>';
  if(p==='ss')return '<div class="field"><label>method（加密）</label><input data-f="method" placeholder="chacha20-ietf-poly1305"></div><div class="field"><label>password</label><input data-f="password" type="password"></div>';
  if(p==='socks5')return '<div class="field"><label>用户名</label><input data-f="user_id"></div><div class="field"><label>密码</label><input data-f="password" type="password"></div>';
  if(p==='hysteria2')return '<div class="field"><label>password</label><input data-f="password" type="password"></div><div class="field"><label>sni</label><input data-f="sni" placeholder="留空用地址"></div><div class="field" style="align-self:end">'+ck+'</div>';
  if(p==='anytls')return '<div class="field"><label>user_id</label><input data-f="user_id"></div><div class="field"><label>password</label><input data-f="password" type="password"></div><div class="field"><label>sni</label><input data-f="sni"></div><div class="field" style="align-self:end">'+ck+'</div>';
  return '';
}
function renderNodeEditors(){const box=q('#manualEditors');if(!nodeEditors.length){box.innerHTML='<div class="empty-hint">暂无手动配置节点，点下方「添加节点」</div>';return}box.innerHTML=nodeEditors.map((n,i)=>nodeEditorHtml(n,i)).join('')}
function nodeEditorHtml(n,i){
  const p=n.protocol||'vless';
  const protoOpts=['vless','vmess','trojan','ss','socks5','hysteria2','anytls'].map(x=>'<option value="'+x+'"'+(p===x?' selected':'')+'>'+x+'</option>').join('');
  return '<div class="nedit" data-i="'+i+'"><select data-f="protocol" onchange="fillExtra('+i+')">'+protoOpts+'</select><input data-f="name" value="'+esc(n.name||'')+'" placeholder="节点名"><input data-f="address" value="'+esc(n.address||'')+'" placeholder="服务器地址"><input data-f="port" type="number" value="'+(n.port||'')+'" placeholder="端口"><button class="btn btn-sm btn-danger" onclick="delNodeEditor('+i+')">删除</button><details><summary>凭据与高级设置</summary><div class="extra" id="extra-'+i+'"></div></details></div>';
}
function fillExtra(i){
  const row=q('#manualEditors .nedit[data-i="'+i+'"]');if(!row)return;
  const n=nodeEditors[i]||{};const p=row.querySelector('[data-f="protocol"]').value;const box=row.querySelector('#extra-'+i);
  box.innerHTML=nodeExtras(p);
  const set=(f,v)=>{const el=box.querySelector('[data-f="'+f+'"]');if(el)el.value=v!=null?v:''};
  set('user_id',n.user_id);set('password',n.password);set('method',n.method);set('sni',n.sni);set('flow',n.flow);set('network',n.network);set('security',n.security);set('path',n.path);set('host',n.host);
  set('rp',(n.reality&&n.reality.public_key)||'');set('rs',(n.reality&&n.reality.short_id)||'');set('rx',(n.reality&&n.reality.spider_x)||'');
  const ic=box.querySelector('[data-f="insecure"]');if(ic)ic.checked=!!n.insecure;
  const tls=box.querySelector('[data-f="tls"]');if(tls)tls.checked=!!n.tls;
}
function addNodeEditor(){nodeEditors.push({protocol:'vless'});renderNodeEditors();fillExtra(nodeEditors.length-1)}
function delNodeEditor(i){nodeEditors.splice(i,1);renderNodeEditors()}
function collectNodes(){
  const out=[];
  qa('#manualEditors .nedit').forEach(row=>{
    const i=+row.dataset.i;const g=f=>row.querySelector('[data-f="'+f+'"]');const address=((g('address')||{}).value||'').trim();if(!address)return;
    const n={name:(g('name')||{}).value||'',protocol:g('protocol').value,address:address,port:parseInt((g('port')||{}).value||'0',10)||0};
    for(const f of ['user_id','password','method','sni','flow','network','security','path','host']){const el=g(f);if(el&&el.value)n[f]=el.value}
    if(g('insecure')&&g('insecure').checked)n.insecure=true;if(g('tls')&&g('tls').checked)n.tls=true;
    const rp=((g('rp')||{}).value||'').trim(),rs=((g('rs')||{}).value||'').trim(),rx=((g('rx')||{}).value||'').trim();
    if((n.protocol==='vless'||n.protocol==='trojan')&&(rp||rs)){n.reality={public_key:rp,short_id:rs,spider_x:rx||'/'}}
    nodeEditors[i]=n;out.push(n);
  });
  return out;
}

/* ============================================================
   配额
   ============================================================ */
function renderQuota(c){
  q('#q_error_types').value=(c.quota_error_signals&&c.quota_error_signals.error_types||[]).join(', ');
  q('#q_message_kw').value=(c.quota_error_signals&&c.quota_error_signals.message_keywords||[]).join(', ');
  q('#q_max_switches').value=c.max_quota_node_switches||5;
  q('#q_cooldown_h').value=c.node_cooldown_exhausted_hours||1;
  q('#q_cooldown_m').value=c.node_cooldown_dead_minutes||1;
  q('#q_health_interval').value=c.node_health_interval_minutes||15;
  q('#q_health_url').value=c.node_health_probe_url||'';
}
function collectQuota(){return{quota_error_signals:{error_types:q('#q_error_types').value.split(',').map(s=>s.trim()).filter(Boolean),message_keywords:q('#q_message_kw').value.split(',').map(s=>s.trim()).filter(Boolean)},max_quota_node_switches:parseInt(q('#q_max_switches').value||'5',10),node_cooldown_exhausted_hours:parseInt(q('#q_cooldown_h').value||'24',10),node_cooldown_dead_minutes:parseInt(q('#q_cooldown_m').value||'1',10),node_health_interval_minutes:parseInt(q('#q_health_interval').value||'15',10),node_health_probe_url:q('#q_health_url').value.trim()}}
async function saveSubsConfig(){
  const st=q('#subSaveStatus');st.textContent='保存中…';
  if(DEMO_MODE){await new Promise(r=>setTimeout(r,350));collectSubs();collectNodes();st.textContent='已保存（演示）';showToast('订阅与节点已保存','success');return}
  try{collectSubs();collectWebshare();const nodes=collectNodes();const r=await fetch('/api/nodes',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({action:'save',subscriptions:subData,webshare:wsData,manual_nodes:nodes})});const d=await r.json();if(!r.ok)throw new Error(d.error||'请求失败');st.textContent='已保存，节点 '+d.nodes+' 个';showToast('订阅与节点已保存，节点池已刷新','success')}catch(e){st.textContent='失败: '+e.message;showToast('保存失败: '+e.message,'error')}finally{loadNodes()}
}
async function saveQuotaConfig(){
  if(DEMO_MODE){showToast('配额设置已保存（演示）','success');return}
  const r=await fetch('/api/config',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(collectQuota())});if(!r.ok){showToast('保存配额失败: '+await r.text(),'error');return}showToast('配额设置已保存','success');
}

/* ============================================================
   配置编辑（代理与模型）
   ============================================================ */
function modelSelectHtml(selected){let h='<select data-field="val" class="m-select"><option value="">-- 选择模型 --</option>';for(const m of modelList){h+='<option value="'+esc(m)+'"'+(selected===m?' selected':'')+'>'+esc(m)+'</option>'}h+='</select>';return h}
function renderAliasTable(){const tb=q('#aliasTable tbody');const ks=Object.keys(cfg.model_alias||{});if(!ks.length){tb.innerHTML='<tr><td colspan="3" class="empty-hint">暂无别名配置</td></tr>';return}tb.innerHTML=ks.map(k=>'<tr><td><input value="'+esc(k)+'" data-field="key" placeholder="例如: gpt-5.5"></td><td>'+modelSelectHtml(cfg.model_alias[k])+'</td><td class="acts"><button class="btn btn-sm btn-danger" onclick="delAlias(this)">删除</button></td></tr>').join('')}
function addAliasRow(){const tb=q('#aliasTable tbody');if(tb.querySelector('.empty-hint'))tb.innerHTML='';const tr=document.createElement('tr');tr.innerHTML='<td><input placeholder="例如: gpt-5.5" data-field="key"></td><td>'+modelSelectHtml('')+'</td><td class="acts"><button class="btn btn-sm btn-danger" onclick="delAlias(this)">删除</button></td></tr>';tb.appendChild(tr)}
function delAlias(b){b.closest('tr').remove()}
function collectAliases(){const r={};qa('#aliasTable tbody tr').forEach(tr=>{const k=tr.querySelector('[data-field="key"]'),v=tr.querySelector('[data-field="val"]');if(k&&k.value.trim())r[k.value.trim()]=v?v.value.trim():''});cfg.model_alias=r;return r}
function renderEffortTable(){const tb=q('#effortTable tbody');const es=Object.keys(cfg.reasoning_effort_map||{});if(!es.length){tb.innerHTML='<tr><td colspan="3" class="empty-hint">暂无映射配置</td></tr>';return}tb.innerHTML=es.map(k=>'<tr><td><input value="'+esc(k)+'" data-field="key" placeholder="例如: low"></td><td><input value="'+esc(cfg.reasoning_effort_map[k])+'" data-field="val" placeholder="例如: high"></td><td class="acts"><button class="btn btn-sm btn-danger" onclick="delEffort(this)">删除</button></td></tr>').join('')}
function addEffortRow(){const tb=q('#effortTable tbody');if(tb.querySelector('.empty-hint'))tb.innerHTML='';const tr=document.createElement('tr');tr.innerHTML='<td><input placeholder="例如: low" data-field="key"></td><td><input placeholder="例如: high" data-field="val"></td><td class="acts"><button class="btn btn-sm btn-danger" onclick="delEffort(this)">删除</button></td></tr>';tb.appendChild(tr)}
function delEffort(b){b.closest('tr').remove()}
function collectEfforts(){const r={};qa('#effortTable tbody tr').forEach(tr=>{const k=tr.querySelector('[data-field="key"]'),v=tr.querySelector('[data-field="val"]');if(k&&k.value.trim())r[k.value.trim()]=v?v.value.trim():''});cfg.reasoning_effort_map=r;return r}
function renderSocks5Table(){const tb=q('#socks5Table tbody');const ps=cfg.socks5_proxies||[];if(!ps.length){tb.innerHTML='<tr><td colspan="5" class="empty-hint">暂无代理配置</td></tr>';return}tb.innerHTML=ps.map((p,i)=>'<tr><td><input value="'+esc(p.name||'')+'" data-field="name"></td><td><input value="'+esc(p.addr)+'" data-field="addr" placeholder="例如: 127.0.0.1:1080"></td><td><input value="'+esc(p.username||'')+'" data-field="username"></td><td><input value="'+esc(p.password||'')+'" data-field="password" type="password"></td><td class="acts"><button class="btn btn-sm btn-danger" onclick="delSocks5(this)">删除</button></td></tr>').join('')}
function addSocks5Row(){const tb=q('#socks5Table tbody');if(tb.querySelector('.empty-hint'))tb.innerHTML='';const tr=document.createElement('tr');tr.innerHTML='<td><input data-field="name"></td><td><input data-field="addr" placeholder="例如: 127.0.0.1:1080"></td><td><input data-field="username"></td><td><input data-field="password" type="password"></td><td class="acts"><button class="btn btn-sm btn-danger" onclick="delSocks5(this)">删除</button></td></tr>';tb.appendChild(tr)}
function delSocks5(b){b.closest('tr').remove()}
function collectSocks5(){const r=[];qa('#socks5Table tbody tr').forEach(tr=>{const a=tr.querySelector('[data-field="addr"]');if(a&&a.value.trim())r.push({addr:a.value.trim(),name:(tr.querySelector('[data-field="name"]')||{}).value||'',username:(tr.querySelector('[data-field="username"]')||{}).value||'',password:(tr.querySelector('[data-field="password"]')||{}).value||''})});cfg.socks5_proxies=r;return r}
function renderSocks5Select(){const sel=q('#activeSocks5');sel.innerHTML='<option value="">直连（不使用代理）</option>';(cfg.socks5_proxies||[]).forEach(p=>{if(p.addr){const opt=document.createElement('option');opt.value=p.addr;opt.textContent=p.name?p.name+' ('+p.addr+')':p.addr;sel.appendChild(opt)}});if((cfg.socks5_proxies||[]).length>=2){const opt=document.createElement('option');opt.value='__round_robin__';opt.textContent='轮询（自动切换）';sel.appendChild(opt)}sel.value=cfg.active_socks5||'';q('#socks5_paid_direct').value=cfg.socks5_paid_direct?'1':'0'}
async function saveConfig(){
  collectEfforts();collectAliases();collectSocks5();
  const body={model_alias:cfg.model_alias,reasoning_effort_map:cfg.reasoning_effort_map,force_disable_thinking:q('#force_disable_thinking').checked,socks5_proxies:cfg.socks5_proxies,active_socks5:q('#activeSocks5').value,socks5_paid_direct:q('#socks5_paid_direct').value==='1',api_key:q('#apiKeyInput').value};
  if(DEMO_MODE){cfg.api_key=body.api_key;syncGatewayBox();showToast('配置已保存（演示）','success');return}
  try{const r=await fetch('/api/config',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});if(!r.ok)throw new Error(await r.text());cfg.api_key=body.api_key;syncGatewayBox();showToast('配置已保存','success')}catch(e){showToast('保存失败: '+e.message,'error')}
}
async function reloadConfig(){
  if(DEMO_MODE){showToast('会话已刷新（演示）','success');return}
  try{const r=await fetch('/api/reload',{method:'POST'});const d=await r.json();if(!r.ok)throw new Error(d.error||'请求失败');showToast('会话已刷新，模型 '+((d.models??d.free)||0)+' 个','success');fetchModelList().then(()=>renderAliasTable());loadCaps()}catch(e){showToast('刷新失败: '+e.message,'error')}
}
async function resetStats(){
  if(!confirm('确认清空所有 Token 统计？此操作不可撤销。'))return;
  const s=q('#resetStatus');s.textContent='清空中…';
  if(DEMO_MODE){q('#statsTable').innerHTML='<tr><td colspan="5" class="empty-hint">暂无数据</td></tr>';s.textContent='已清空';setTimeout(()=>s.textContent='',2000);return}
  try{const r=await fetch('/api/stats',{method:'DELETE'});if(!r.ok)throw new Error(await r.text());q('#statsTable').innerHTML='<tr><td colspan="5" class="empty-hint">暂无数据</td></tr>';s.textContent='已清空';setTimeout(()=>s.textContent='',2000)}catch(e){s.textContent='失败: '+e.message}
}
function renderConfigEditors(){
  subData=(cfg.subscriptions||[]).map(s=>({...s}));
  wsData=(cfg.webshare||[]).map(w=>({...w}));
  nodeEditors=(cfg.manual_nodes||[]).map(n=>({...n}));
  renderAliasTable();renderEffortTable();renderSocks5Table();renderSocks5Select();renderQuota(cfg);renderSubTable();renderWebshareTable();renderNodeEditors();
  q('#force_disable_thinking').checked=!!cfg.force_disable_thinking;
  q('#apiKeyInput').value=cfg.api_key||'';syncGatewayBox();
}
function gwBase(){
  const h=(location.host||location.hostname||'').trim();
  if(h)return location.protocol+'//'+h+'/v1';
  return DEMO_MODE?'http://localhost:8000/v1':'/v1';
}
function syncGatewayBox(){
  const base=gwBase();
  const v=(cfg.api_key||q('#apiKeyInput').value||'');
  const fill=(id)=>{const el=q(id);if(el){el.textContent=base;el.dataset.v=v;el.dataset.base=base}};
  fill('#gwAddress');fill('#gwAddress2');
  const fillK=(id)=>{const el=q(id);if(el){el.dataset.v=v;el.textContent=v||'未启用（留空）';el.style.opacity=v?'1':'.5'}};
  fillK('#apiKeyShow');fillK('#apiKeyShow2');
}
function genApiKey(){
  const b=new Uint8Array(24);if(window.crypto&&crypto.getRandomValues){crypto.getRandomValues(b)}else{for(let i=0;i<b.length;i++)b[i]=Math.floor(Math.random()*256)}
  let k='';const chars='abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789';
  for(let i=0;i<b.length;i++)k+=chars[b[i]%chars.length];
  q('#apiKeyInput').value='sk-'+k;syncGatewayBox();showToast('已生成，点保存生效','info');
}

/* ============================================================
   运行日志
   ============================================================ */
let logBuf=[],logES=null,logLastSeq=0,logFilterLevel='all',logKeyword='',logStick=true;
const logListEl=q('#logList');const LOG_MAX_BUF=2000;
function setLogLevelFilter(l){logFilterLevel=l;qa('#logLevelChips .chip').forEach(c=>c.classList.toggle('active',c.dataset.l===l));renderLogs()}
function setLogKeyword(v){logKeyword=(v||'').toLowerCase();renderLogs()}
function logMatches(e){if(logFilterLevel!=='all'&&e.level!==logFilterLevel)return false;if(!logKeyword)return true;if((e.msg||'').toLowerCase().includes(logKeyword))return true;return (e.attrs||[]).some(a=>String(a.v).toLowerCase().includes(logKeyword))}
function logRowHtml(e){
  const cls=e.level==='ERROR'?'lv-error':e.level==='WARN'?'lv-warn':e.level==='DEBUG'?'lv-debug':'';
  return '<div class="log-line '+cls+'" data-seq="'+e.seq+'">'+
    '<span class="log-time">'+fmtLogTime(e.time)+'</span>'+
    '<span class="log-lv lv-'+e.level.toLowerCase()+'">'+e.level+'</span>'+
    '<span class="log-msg">'+esc(e.msg||'')+'</span>'+
    (e.attrs&&e.attrs.length?'<button class="btn btn-ghost" style="padding:0 6px;flex:none" onclick="toggleLogAttrs('+e.seq+',this)">'+e.attrs.length+' 字段</button>':'')+
    '</div>'+(e.attrs&&e.attrs.length?'<div class="log-attrs" id="la-'+e.seq+'" style="display:none">'+e.attrs.map(a=>'<span style="margin-right:10px"><b>'+esc(a.k)+'</b>='+esc(a.v)+'</span>').join('')+'</div>':'');
}
function renderLogs(){const rows=[];for(const e of logBuf){if(logMatches(e))rows.push(e)}logListEl.innerHTML=rows.map(logRowHtml).join('');const total=logBuf.length;const shown=rows.length;q('#logCount').textContent='缓冲 '+total+' 条 / 显示 '+shown+' 条';updateLogStick();logListEl.scrollTop=logListEl.scrollHeight}
function toggleLogAttrs(seq,btn){const el=q('#la-'+seq);if(!el)return;const open=el.style.display!=='none';el.style.display=open?'none':'block';btn.textContent=(open?'展开 ':'收起 ')+el.querySelectorAll('span').length+' 字段'}
function fmtLogTime(iso){const d=new Date(iso);const p=n=>String(n).padStart(2,'0');return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds())}
function onLogEntry(e){if(e.seq<=logLastSeq)return;logLastSeq=e.seq;if(logBuf.length>=LOG_MAX_BUF)logBuf.shift();logBuf.push(e);if(logMatches(e))logListEl.insertAdjacentHTML('beforeend',logRowHtml(e));updateLogCount();updateLogStick();q('#logStatus').textContent='实时 · seq '+logLastSeq}
function updateLogCount(){q('#logCount').textContent='缓冲 '+logBuf.length+' 条 / 显示 '+logListEl.querySelectorAll('.log-line').length+' 条'}
function updateLogStick(){if(logStick)logListEl.scrollTop=logListEl.scrollHeight;q('#logScrollBtn').style.display=logStick?'none':'inline-block'}
logListEl.addEventListener('scroll',()=>{const el=logListEl;logStick=(el.scrollTop+el.clientHeight>=el.scrollHeight-40);q('#logScrollBtn').style.display=logStick?'none':'inline-block'});
function logScrollToBottom(){logStick=true;logListEl.scrollTop=logListEl.scrollHeight;q('#logScrollBtn').style.display='none'}
function initLogs(){
  if(DEMO_MODE){if(!logBuf.length)applyDemoLogs();toggleLogStream(q('#logAutoRefresh').checked);return}
  fetch('/api/config').then(r=>r.json()).then(c=>{const lv=c.log_level||'info';const sel=q('#logRunLevel');if(sel.value!==lv)sel.value=lv}).catch(()=>{});
  toggleLogStream(q('#logAutoRefresh').checked);
}
function toggleLogStream(on){
  const sel=q('#logAutoRefresh');if(sel&&sel.checked!==on)sel.checked=on;
  if(on){
    if(DEMO_MODE){if(demoLogTimer)return;demoLogTimer=setInterval(()=>{const seq=++logLastSeq;const levels=['INFO','INFO','DEBUG','WARN','INFO','ERROR'];const msgs=['request_started','upstream_attempt node=hk-01','stream_result tokens='+Math.floor(300+Math.random()*1800),'node health probe ok','switching node after quota signal','upstream_error retry=2'];const i=Math.floor(Math.random()*levels.length);onLogEntry({seq,time:new Date().toISOString(),level:levels[i],msg:msgs[i],attrs:i%2?[{k:'request_id',v:'req_'+Math.floor(9000+Math.random()*900)},{k:'model',v:Math.random()>.5?'gpt-5.5':'glm-5.2'}]:[]})},2600);q('#logStatus').textContent='实时（演示）· seq '+logLastSeq;return}
    if(logES)return;
    logES=new EventSource('/api/logs/stream');
    logES.onmessage=ev=>{try{onLogEntry(JSON.parse(ev.data))}catch(_){}};
    logES.onerror=()=>{q('#logStatus').textContent='连接中断，重连中…';logES=null;setTimeout(()=>{if(sel&&sel.checked)toggleLogStream(true)},2000)};
    q('#logStatus').textContent='已连接';
  }else{
    if(DEMO_MODE){if(demoLogTimer){clearInterval(demoLogTimer);demoLogTimer=null}q('#logStatus').textContent='已暂停';return}
    if(logES){logES.close();logES=null;q('#logStatus').textContent='已暂停'}
  }
}
function setLogRunLevel(v){
  if(DEMO_MODE){showToast('日志级别已切换为 '+v+'（演示）','success');return}
  fetch('/api/config',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({log_level:v})}).then(r=>{if(!r.ok)throw new Error('HTTP '+r.status)}).then(()=>showToast('日志级别已切换为 '+v,'success')).catch(e=>showToast('切换级别失败: '+e.message,'error'));
}
function exportLogs(){
  const text=logBuf.map(e=>fmtLogTime(e.time)+' ['+e.level+'] '+(e.msg||'')).join('\n')||'(无日志)';
  const a=document.createElement('a');a.href=URL.createObjectURL(new Blob([text],{type:'text/plain'}));a.download='opencode2api-logs.txt';document.body.appendChild(a);a.click();a.remove();URL.revokeObjectURL(a.href);
}

/* ---------- 调用日志（Call Log） ---------- */
let callLogData=[],callLogExpanded={},clLogView='list',callLogTimer=null;
const CL_ISSUE_TYPES=['switch','ttft_timeout','silence_timeout','stream_interrupt','stream_error','connect_error','upstream_error','all_failed'];
function clHasIssue(r){if(r.status!=='ok')return true;return (r.events||[]).some(e=>CL_ISSUE_TYPES.includes(e.type))}
function clIssueLabel(r){for(const e of r.events||[]){switch(e.type){case 'all_failed':return '全部节点失败';case 'switch':return '已切换节点';case 'ttft_timeout':return '首字超时';case 'silence_timeout':return '静默超时';case 'stream_interrupt':return '流中断';case 'stream_error':return '流错误';case 'connect_error':return '连接失败';case 'upstream_error':return '上游错误'}}return '异常'}
function clFmtTime(ts){try{const d=new Date(ts);const p=n=>String(n).padStart(2,'0');return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds())}catch(_){return ts||''}}
function clFmtDur(ms){if(!ms||ms<=0)return '-';if(ms<1000)return ms+'ms';return (ms/1000).toFixed(1)+'s'}
function showLogTab(t){
  const raw=t==='raw',call=t==='call';
  q('#rawLogView').style.display=raw?'':'none';
  q('#callLogView').style.display=call?'':'none';
  q('#logTabRaw').className=raw?'active':'';
  q('#logTabCall').className=call?'active':'';
  if(call){loadCallLog(false);if(!callLogTimer)callLogTimer=setInterval(()=>loadCallLog(true),5000)}
  else if(callLogTimer){clearInterval(callLogTimer);callLogTimer=null}
}
function loadCallLog(showStatus){
  if(DEMO_MODE){if(!callLogData.length)applyDemoCallLog();clRefreshView();return}
  fetch('/api/call-log?limit=500').then(r=>{if(!r.ok)throw new Error('HTTP '+r.status);return r.json()}).then(recs=>{
    callLogData=recs||[];clRefreshView();
    if(showStatus)q('#clStatus').textContent=clFmtTime(new Date().toISOString())+' 刷新 '+callLogData.length+' 条';
  }).catch(e=>{if(showStatus)q('#clStatus').textContent='加载失败: '+e.message});
}
function clVisible(){
  const kw=(q('#clKeyword').value||'').trim().toLowerCase();
  const date=q('#clDate').value;
  const only=q('#clOnlyIssues').checked;
  return callLogData.filter(r=>{
    if(only&&!clHasIssue(r))return false;
    if(date&&(r.ts||'').slice(0,10)!==date)return false;
    if(kw){const hay=[r.model||'',r.path||'',r.err_msg||'',r.req_id||''].join(' ').toLowerCase();if(!hay.includes(kw))return false}
    return true;
  });
}
function clRefreshView(){
  const visible=clVisible();
  const ok=callLogData.filter(r=>r.status==='ok').length;
  const issue=callLogData.filter(clHasIssue).length;
  q('#clTotal').textContent=callLogData.length;
  q('#clOk').textContent=ok;
  q('#clFail').textContent=callLogData.length-ok;
  q('#clIssue').textContent=issue;
  const dates=new Set(callLogData.map(r=>(r.ts||'').slice(0,10)).filter(Boolean));
  const sel=q('#clDate'),cur=sel.value;
  sel.innerHTML='<option value="">全部日期</option>'+[...dates].sort().reverse().map(d=>'<option'+(d===cur?' selected':'')+'>'+d+'</option>').join('');
  q('#clList').style.display=clLogView==='list'?'':'none';
  q('#clHour').style.display=clLogView==='hour'?'':'none';
  q('#clNode').style.display=clLogView==='node'?'':'none';
  if(clLogView==='list')clRenderList(visible);
  else if(clLogView==='hour')clRenderHour(visible);
  else clRenderNode(visible);
}
function clSetView(v){
  clLogView=v;
  for(const id of ['clViewList','clViewHour','clViewNode']){const el=q('#'+id);el.className='btn btn-sm'+(id==='clView'+v.charAt(0).toUpperCase()+v.slice(1)?'':' btn-ghost')}
  clRefreshView();
}
function clRenderList(visible){
  const el=q('#clList');
  if(!visible.length){el.innerHTML='<div class="cl-empty">'+(callLogData.length?'没有匹配的日志':'暂无调用日志 · 发起一次 /v1 请求后在此查看结构化调用记录')+'</div>';return}
  el.innerHTML=visible.map(r=>{
    const issue=clHasIssue(r),exp=callLogExpanded[r.req_id];
    const nodes=(r.nodes||[]).join(' → ');
    const badge=r.status==='ok'?'<span class="cl-badge ok">【成功】</span>':'<span class="cl-badge fail">【失败】</span>';
    const tag=issue?'<span class="cl-issue-tag">'+clIssueLabel(r)+'</span>':'';
    const chev=issue?(exp?'▾':'▸'):'';
    let body='';
    if(issue&&exp){
      const evs=(r.events||[]).map(ev=>'<div class="cl-ev"><span class="cl-ev-time">'+clFmtTime(ev.at)+'</span><span class="cl-ev-tag '+(ev.type||'').replace(/\./g,'_')+'">'+esc(ev.type)+'</span><span>'+(ev.node?'<span class="cl-ev-node">'+esc(ev.node)+'</span> ':'')+(ev.detail?'<span class="cl-ev-detail">'+esc(ev.detail)+'</span>':'')+'</span></div>').join('');
      body='<div class="cl-body"><div class="cl-meta">req_id: '+esc(r.req_id||'-')+' · '+esc(r.path||'/v1/chat/completions')+' · stream: '+(r.stream?'是':'否')+' · 路由: '+esc(r.route_mode||'-')+(r.err_msg?'<span class="err"> · 错误: '+esc(r.err_msg)+'</span>':'')+'</div><div class="cl-meta">token: 输入 '+(r.prompt_tokens||0)+' / 输出 '+(r.completion_tokens||0)+' · 耗时 '+clFmtDur(r.duration_ms)+'</div><div class="cl-events">'+(evs||'<span style="color:var(--text-ter)">无事件明细</span>')+'</div></div>';
    }
    return '<div class="cl-item'+(issue?' issue':'')+'"><button class="cl-head" '+(issue?'onclick="clToggle(\''+r.req_id+'\')"':'')+'><span class="cl-badge '+(r.status==='ok'?'ok':'fail')+'">'+(r.status==='ok'?'【成功】':'【失败】')+'</span>'+tag+'<span class="cl-time">'+clFmtTime(r.ts)+'</span><span class="cl-model">'+esc(r.model||'-')+'</span>'+(nodes?'<span class="cl-nodes">'+esc(nodes)+'</span>':'')+'<span class="cl-dur">'+clFmtDur(r.duration_ms)+'</span><span style="flex:none;color:var(--text-ter)">'+chev+'</span></button>'+body+'</div>';
  }).join('');
}
function clToggle(id){callLogExpanded[id]=!callLogExpanded[id];clRefreshView()}
function clRenderHour(visible){
  const el=q('#clHour');
  const hours=Array.from({length:24},(_,h)=>({h,req:0,ok:0,totalMs:0,issue:0}));
  for(const r of visible){
    const d=new Date(r.ts);if(isNaN(d))continue;
    const s=hours[d.getHours()];s.req++;if(r.status==='ok')s.ok++;s.totalMs+=r.duration_ms||0;s.issue+=(r.events||[]).filter(e=>CL_ISSUE_TYPES.includes(e.type)).length;
  }
  const withData=hours.filter(h=>h.req>0);
  if(!withData.length){el.innerHTML='<div class="cl-empty">暂无日志数据</div>';return}
  const maxReq=Math.max(1,...hours.map(h=>h.req));
  el.innerHTML='<div style="font-size:13px;font-weight:600;margin-bottom:4px">时段分析</div><div style="font-size:12px;color:var(--text-ter);margin-bottom:8px">按小时统计请求分布与成功率（数据来自保留期内调用日志）</div>'+
    '<div class="cl-chart">'+hours.map(h=>h.req>0?'<div class="bar" title=""><div class="tip">'+String(h.h).padStart(2,'0')+' 时 · '+h.req+' 请求 · 均耗 '+clFmtDur(Math.round(h.totalMs/h.req))+'</div><div class="fill'+(h.ok/h.req>=0.9?'':' issue')+'" style="height:'+Math.max(h.req/maxReq*100,4)+'%"></div></div>':'<div class="bar"></div>').join('')+'</div>'+
    '<table class="cl-tbl"><thead><tr><th>时段</th><th class="num">请求数</th><th class="num">平均耗时</th><th class="num">失败率</th><th class="num">异常/切换</th></tr></thead><tbody>'+withData.map(h=>'<tr><td>'+String(h.h).padStart(2,'0')+':00 - '+String(h.h).padStart(2,'0')+':59</td><td class="num">'+h.req+'</td><td class="num">'+clFmtDur(Math.round(h.totalMs/h.req))+'</td><td class="num">'+Math.round((1-h.ok/h.req)*100)+'%</td><td class="num">'+h.issue+'</td></tr>').join('')+'</tbody></table>';
}
function clRenderNode(visible){
  const el=q('#clNode');
  const m=new Map();
  for(const r of visible){
    const node=(r.nodes&&r.nodes.length?r.nodes[r.nodes.length-1]:null)||'未知';
    let s=m.get(node);if(!s){s={node,req:0,ok:0,totalMs:0,issue:0};m.set(node,s)}
    s.req++;if(r.status==='ok')s.ok++;s.totalMs+=r.duration_ms||0;s.issue+=(r.events||[]).filter(e=>CL_ISSUE_TYPES.includes(e.type)).length;
  }
  const rows=[...m.values()].sort((a,b)=>b.req-a.req||b.issue-a.issue);
  if(!rows.length){el.innerHTML='<div class="cl-empty">暂无日志数据</div>';return}
  el.innerHTML='<div style="font-size:13px;font-weight:600;margin-bottom:4px">节点分析</div><div style="font-size:12px;color:var(--text-ter);margin-bottom:8px">按最终出口节点聚合请求量、成功率与稳定性</div>'+
    '<table class="cl-tbl"><thead><tr><th>节点</th><th class="num">请求数</th><th class="num">成功率</th><th class="num">平均耗时</th><th class="num">异常/切换</th></tr></thead><tbody>'+rows.map(n=>{const rate=n.req?n.ok/n.req:0;const cls=rate>=0.9?'':rate>=0.5?'warn':'bad';return '<tr><td style="font-family:var(--mono);font-size:12px">'+esc(n.node)+'<span class="ratebar"><i class="'+cls+'" style="width:'+(rate*100)+'%"></i></span></td><td class="num">'+n.req+'</td><td class="num">'+Math.round(rate*100)+'%</td><td class="num">'+clFmtDur(Math.round(n.totalMs/n.req))+'</td><td class="num">'+n.issue+'</td></tr>'}).join('')+'</tbody></table>';
}
function clearCallLog(){
  if(!confirm('确定清空全部调用日志？该操作不可恢复。'))return;
  if(DEMO_MODE){callLogData=[];clRefreshView();showToast('日志已清空（演示）','success');return}
  fetch('/api/call-log',{method:'DELETE'}).then(r=>{if(!r.ok)throw new Error('HTTP '+r.status)}).then(()=>{callLogData=[];clRefreshView();showToast('调用日志已清空','success')}).catch(e=>showToast('清空失败: '+e.message,'error'));
}
function applyDemoCallLog(){
  const now=Date.now(),models=['deepseek-v4-flash','mimo-v2.5','gpt-5.5','glm-5.2'],nodes=[['hk-01'],['hk-01','jp-02'],['us-03']],paths=['/v1/chat/completions','/v1/messages','/v1/responses'];
  callLogData=[];
  for(let i=0;i<40;i++){
    const ok=Math.random()>0.22;
    const rec={req_id:'req_demo'+String(9100+i),ts:new Date(now-(40-i)*64000).toISOString(),path:paths[i%3],model:models[i%4],stream:i%2===0,route_mode:i%3?'public':'auto',nodes:nodes[i%3],status:ok?'ok':'fail',prompt_tokens:ok?Math.floor(300+Math.random()*1200):0,completion_tokens:ok?Math.floor(120+Math.random()*900):0,duration_ms:Math.floor(400+Math.random()*3000),events:[]};
    if(ok)rec.events.push({type:'connect_ok',node:rec.nodes[0],at:rec.ts});
    else{
      rec.events.push({type:'connect_ok',node:rec.nodes[0],at:rec.ts});
      rec.events.push({type:'upstream_error',node:rec.nodes[0],detail:'http 429',at:rec.ts});
      if(rec.nodes.length>1){rec.events.push({type:'switch',node:rec.nodes[0],detail:'quota:FreeUsageLimitError',at:rec.ts});rec.events.push({type:'connect_ok',node:rec.nodes[1],at:rec.ts})}
      else{rec.events.push({type:'all_failed',node:rec.nodes[0],detail:'quota budget exhausted',at:rec.ts})}
      rec.err_msg=rec.nodes.length>1?'upstream http 429':'quota budget exhausted';
    }
    callLogData.push(rec);
  }
}
function applyDemoLogs(){
  logBuf=[];const levels=['INFO','INFO','DEBUG','INFO','WARN','INFO','ERROR','INFO','DEBUG','INFO'];
  const msgs=['request_started','upstream_attempt node=hk-01 latency=48ms','stream_result tokens=1284','request_done ok','node marked exhausted (FreeUsageLimitError)','switching node → jp-02','upstream_error retry=2 backoff=500ms','subscription refreshed nodes=14','health probe ok latency=62ms','token stats saved'];
  const now=Date.now();
  for(let i=0;i<42;i++){
    logBuf.push({seq:i+1,time:new Date(now-(42-i)*1400).toISOString(),level:levels[i%levels.length],msg:msgs[i%msgs.length],attrs:i%3===0?[{k:'request_id',v:'req_'+String(9100+i)},{k:'model',v:i%2?'gpt-5.5':'glm-5.2'}]:[]});
  }
  logLastSeq=logBuf.length;
}

/* ============================================================
   离线演示数据
   ============================================================ */
const DEMO={
  cfg:{
    subscriptions:[
      {name:'主订阅',url:'https://sub.example.com/clash',update_interval_hours:24,nodes:14,usage_total:107374182400,usage_used:39728447488,usage_expire:1754000000,last_updated_at:'2026-08-10T21:03:00Z'},
      {name:'备线路',url:'https://backup.example.com/uri',update_interval_hours:48,nodes:6,usage_total:536870912000,usage_used:12884901888,last_updated_at:'2026-08-09T08:12:00Z'}
    ],
    manual_nodes:[{protocol:'vless',name:'自建东京',address:'jp.example.com',port:443,user_id:'a3f2c9d1-…',sni:'example.com',reality:{public_key:'MIIBIjANBg…',short_id:'ab12',spider_x:'/'}}],
    model_alias:{'gpt-5.5':'gpt-5.5','glm-5.2':'glm-5.2','deepseek-r1':'deepseek-r1'},
    reasoning_effort_map:{low:'minimal',high:'high'},
    force_disable_thinking:false,
    socks5_proxies:[{name:'本地代理',addr:'127.0.0.1:7890'}],
    active_socks5:'',socks5_paid_direct:false,
    api_key:'sk-demo-xxxxxxxxxxxxxxxx',
    quota_error_signals:{error_types:['FreeUsageLimitError','insufficient_quota'],message_keywords:['free usage limit','quota','limit exceeded']},
    max_quota_node_switches:5,node_cooldown_exhausted_hours:1,node_cooldown_dead_minutes:1,
    node_health_interval_minutes:15,node_health_probe_url:'https://www.gstatic.com/generate_204',
    log_level:'info'
  },
  nodes:[
    {name:'香港 01',protocol:'vless',state:'available',latency_ms:48,cooldown_until:'',last_error:'',fingerprint:'a1b2c3d4',active:true,manual:false},
    {name:'日本 02',protocol:'vless',state:'available',latency_ms:86,cooldown_until:'',last_error:'',fingerprint:'e5f6a7b8',active:false,manual:false},
    {name:'新加坡 03',protocol:'trojan',state:'available',latency_ms:112,cooldown_until:'',last_error:'',fingerprint:'c9d0e1f2',active:false,manual:false},
    {name:'美国 04',protocol:'hysteria2',state:'exhausted',latency_ms:184,cooldown_until:'2026-08-12T06:00:00Z',last_error:'FreeUsageLimitError: free usage limit reached',fingerprint:'33445566',active:false,manual:false},
    {name:'德国 05',protocol:'ss',state:'dead',latency_ms:-1,cooldown_until:'2026-08-11T03:00:00Z',last_error:'dial tcp: i/o timeout',fingerprint:'778899aa',active:false,manual:false},
    {name:'自建东京',protocol:'vless',state:'idle',latency_ms:null,cooldown_until:'',last_error:'未探测',fingerprint:'bbccddee',active:false,manual:true},
    {name:'韩国 06',protocol:'anytls',state:'available',latency_ms:132,cooldown_until:'',last_error:'',fingerprint:'11223344',active:false,manual:false}
  ],
  subscriptions:[
    {name:'主订阅',url:'https://sub.example.com/clash',update_interval_hours:24,nodes:14,usage_total:107374182400,usage_used:39728447488,usage_expire:1754000000,last_updated_at:'2026-08-10T21:03:00Z'},
    {name:'备线路',url:'https://backup.example.com/uri',update_interval_hours:48,nodes:6,usage_total:536870912000,usage_used:12884901888,last_updated_at:'2026-08-09T08:12:00Z'},
    {name:'webshare-主池',url:'webshare API',update_interval_hours:12,nodes:5,last_updated_at:'2026-08-10T22:05:00Z'}
  ],
  stats:{models:{'gpt-4o-mini':{request_count:1280,prompt_tokens:12345678,completion_tokens:2345678,total_tokens:14691356},'gpt-5.5':{request_count:342,prompt_tokens:8912345,completion_tokens:1234567,total_tokens:10146912},'glm-5.2':{request_count:876,prompt_tokens:4567890,completion_tokens:987654,total_tokens:5555544},'deepseek-r1':{request_count:2034,prompt_tokens:22334455,completion_tokens:3344556,total_tokens:25679011}}},
  caps:[
    {id:'gpt-4o-mini',cw:128000,mo:16384,mod:['text','image']},
    {id:'gpt-5.5',cw:400000,mo:32768,mod:['text','image','pdf']},
    {id:'glm-5.2',cw:200000,mo:16384,mod:['text']},
    {id:'deepseek-r1',cw:128000,mo:8192,mod:['text']}
  ],
  models:['gpt-4o-mini','gpt-5.5','glm-5.2','deepseek-r1']
};

/* ============================================================
   启动
   ============================================================ */
async function detectBackend(){
  try{const r=await fetch('/api/config',{headers:{'Accept':'application/json'}});if(!r.ok)throw 0;const c=await r.json();if(!c||typeof c!=='object'||Array.isArray(c))throw 0;return true}catch(e){return false}
}
applyTheme();
window.addEventListener('DOMContentLoaded',async()=>{
  const online=await detectBackend();
  DEMO_MODE=!online;
  setConn(online);
  const sa=q('#sideAddr');if(sa)sa.textContent=(location.host||location.hostname)||(DEMO_MODE?'localhost:8000':'-');
  await Promise.all([fetchModelList(),loadConfig(),loadNodes(),loadStats(),loadCaps(),loadTrends()]);
  renderConfigEditors();
  initLogs();
  setTrendRefresh();
});
</script>
</body>
</html>
`

// ======================== Main ========================

func main() {
	var showVersion bool
	flag.StringVar(&port, "port", "8000", "服务端口")
	flag.StringVar(&configPath, "config", "config.json", "配置文件路径")
	flag.StringVar(&adminPassword, "password", "123456", "管理面板密码（留空则不启用登录验证）")
	flag.BoolVar(&debugMode, "debug", false, "启用调试日志")
	flag.StringVar(&logLevel, "log-level", "info", "日志级别: debug/info/warn/error")
	flag.StringVar(&logFile, "log-file", "opencode2api.log", "日志文件路径")
	flag.BoolVar(&logStdout, "log-stdout", true, "是否同时写 stdout")
	flag.IntVar(&logMaxSize, "log-max-size", 100, "单日志文件最大 MB，超过即轮换")
	flag.IntVar(&logMaxBackups, "log-max-backups", 7, "保留的旧日志文件个数")
	flag.IntVar(&logMaxAge, "log-max-age", 14, "旧日志保留天数")
	flag.BoolVar(&logCompress, "log-compress", true, "轮换后 gzip 压缩")
	flag.BoolVar(&logBodies, "log-bodies", false, "Debug 下记录截断的 body 摘要")
	flag.BoolVar(&showVersion, "version", false, "显示版本信息")
	flag.Parse()

	initLogger()
	defer closeLogRotator()

	if showVersion {
		fmt.Println(versionString())
		return
	}

	cfg := loadConfig(configPath)
	applyConfig(cfg)
	if err := saveConfig(configPath, cfg); err != nil {
		slog.Warn("failed to save config", "path", configPath, "error", err)
	}
	initCallLog(configPath)

	// 订阅管理器：缓存 + 状态与配置文件同目录
	cacheDir := filepath.Join(filepath.Dir(configPath), ".subscriptions")
	subManager.configure(cfg.Subscriptions, cfg.Webshare, cfg.ManualNodes, cacheDir)
	startSubscriptionTicker()
	startNodeHealthCheck()

	loadTokenStats()
	slog.Info("config loaded", "path", configPath)
	initOCSession()
	models, err := fetchModels()
	if err != nil {
		slog.Warn("failed to fetch models on startup", "error", err)
	} else {
		modelMu.Lock()
		modelsCache = models
		modelsLoaded = true
		modelMu.Unlock()
		slog.Info("models loaded", "count", len(models))
	}

	goModels, goErr := fetchGoModels()
	if goErr != nil {
		slog.Warn("failed to fetch go catalog on startup", "error", goErr)
	} else {
		modelMu.Lock()
		goModelsCache = goModels
		modelMu.Unlock()
		slog.Info("go catalog loaded", "count", len(goModels))
	}
	startModelRefresh()
	startModelsDevRefresh()
	slog.Info("server starting",
		"port", port,
		"log_level", getLogLevelString(),
		"models", len(getModelIDs()),
		"aliases", len(modelAlias),
	)
	if adminPassword != "" {
		slog.Info("admin panel enabled", "url", fmt.Sprintf("http://localhost:%s/", port))
	} else {
		slog.Info("admin panel disabled (no password)")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", loggingMiddleware(chatCompletionsHandler))
	mux.HandleFunc("/v1/responses", loggingMiddleware(responsesHandler))
	mux.HandleFunc("/v1/messages", loggingMiddleware(claudeMessagesHandler))
	mux.HandleFunc("/v1/models", loggingMiddleware(listModelsHandler))
	mux.HandleFunc("/api/models", loggingMiddleware(requireAuth(adminModelsHandler)))
	mux.HandleFunc("/login", loggingMiddleware(loginHandler))
	mux.HandleFunc("/logout", loggingMiddleware(logoutHandler))
	mux.HandleFunc("/api/config", loggingMiddleware(requireAuth(adminConfigHandler)))
	mux.HandleFunc("/api/nodes", loggingMiddleware(requireAuth(adminNodesHandler)))
	mux.HandleFunc("/api/stats", loggingMiddleware(requireAuth(adminStatsHandler)))
	mux.HandleFunc("/api/stats/trends", loggingMiddleware(requireAuth(adminTrendsHandler)))
	mux.HandleFunc("/api/logs", loggingMiddleware(requireAuth(adminLogsHandler)))
	mux.HandleFunc("/api/logs/stream", loggingMiddleware(requireAuth(adminLogsHandler)))
	mux.HandleFunc("/api/logs/export", loggingMiddleware(requireAuth(adminLogsHandler)))
	mux.HandleFunc("/api/call-log", loggingMiddleware(requireAuth(adminCallLogHandler)))
	mux.HandleFunc("/api/reload", loggingMiddleware(requireAuth(reloadHandler)))
	mux.HandleFunc("/health", loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	mux.HandleFunc("/", loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			requireAuth(adminPageHandler)(w, r)
			return
		}
		http.NotFound(w, r)
	}))

	addr := ":" + port
	server := &http.Server{Addr: addr, Handler: mux}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("listening", "addr", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server terminated", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}
}
