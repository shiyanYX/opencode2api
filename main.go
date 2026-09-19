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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// httpClient 用于流式 API 调用。不设 Client.Timeout——流式 SSE 响应可持续数分钟，
// 全局超时会误杀正常流（context deadline exceeded while reading body）。
// 改用 Transport 级超时：DialContext 保护连接阶段，ResponseHeaderTimeout 保护头部阶段，
// Body 读取阶段不设超时，由上游关闭连接自然结束。
var httpClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
	},
}

// mgmtClient 管理面专用 HTTP 客户端，使用系统默认 DNS 解析器，
// 不受 mihomo ApplyConfig 的 DNS 劫持影响。
// 用于 fetchModels / fetchModelsDevCatalog 等启动时管理请求。
// 支持通过 HTTP_PROXY / HTTPS_PROXY 环境变量指定代理。
var mgmtClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     30 * time.Second,
		Proxy:               http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 15 * time.Second,
			Resolver:  net.DefaultResolver,
		}).DialContext,
	},
}

var (
	version = "v0.3.7"
	commit  = "none"
	date    = "unknown"

	// egress 是出口客户端的唯一入口（见 egress.go）。
	egress *EgressClient
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

const socks5RR = "__round_robin__"

// legacySocks5Nodes 把 config.json 里遗留的 socks5Proxies 迁移为池内节点。
func legacySocks5Nodes() []*ProxyNode {
	if egress == nil {
		return nil
	}
	proxies, _, _ := egress.GetSocks5Config()
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
	resp, err := mgmtClient.Do(req)
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

// ======================== 按节点绑定 Session ID ========================

// nodeSessionPool 按代理节点指纹绑定 session ID，模拟正常用户行为。
// 同一节点的所有请求共享同一个 session ID，不同节点使用不同的 session ID。
var (
	nodeSessionPool   = make(map[string]string)
	nodeSessionPoolMu sync.RWMutex
)

// getOrCreateSessionByNode 获取或创建按节点指纹绑定的 session ID。
// 如果该节点已有 session ID，则返回已有的；否则创建新的。
func getOrCreateSessionByNode(nodeFP string) string {
	if nodeFP == "" {
		nodeFP = "direct"
	}
	nodeSessionPoolMu.RLock()
	session, exists := nodeSessionPool[nodeFP]
	nodeSessionPoolMu.RUnlock()
	if exists {
		return session
	}
	nodeSessionPoolMu.Lock()
	defer nodeSessionPoolMu.Unlock()
	// 双重检查，避免并发创建
	if session, exists = nodeSessionPool[nodeFP]; exists {
		return session
	}
	session = newSessionID()
	nodeSessionPool[nodeFP] = session
	slog.Debug("created session for node", "node_fp", nodeFP, "session_id", session)
	return session
}

func initOCSession() {
	ocInitMu.Lock()
	defer ocInitMu.Unlock()
	if ocInitDone {
		return
	}
	ocClientVer = fetchOCVersion()
	ocSessionID = newSessionID()
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
		ocSessionID = newSessionID()
		ocProjectID = randomHex(40)
		ocInitDone = false
		return
	}
	ocClientVer = fetchOCVersionDirect()
	ocSessionID = newSessionID()
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

	// 免费模型文档缓存
	freeModelDocsMu     sync.RWMutex
	freeModelDocsCache  map[string]bool
	freeModelDocsTime   time.Time
	freeModelsCacheFile = "free_models_cache.json"
)

// 默认免费模型列表（兜底用）
var defaultFreeModels = map[string]bool{
	"mimo-v2.5-free":                  true,
	"ling-3.0-flash-fin-free":         true,
	"nemotron-3-ultra-free":           true,
	"nemotron-3.5-lightning-free":     true,
	"big-pickle":                      true,
	"muse-spark-1.3-contributor-free": true,
}

func fetchModels() ([]ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://opencode.ai/zen/v1/models", nil)
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("x-opencode-session", ocSessionID)
	resp, err := mgmtClient.Do(req)
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
	resp, err := mgmtClient.Do(req)
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
	resp, err := mgmtClient.Do(req)
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

// ======================== 免费模型文档缓存 ========================

// loadFreeModelsCache 从本地缓存加载免费模型列表
func loadFreeModelsCache() map[string]bool {
	data, err := os.ReadFile(freeModelsCacheFile)
	if err != nil {
		return nil
	}
	var models map[string]bool
	if json.Unmarshal(data, &models) != nil {
		return nil
	}
	return models
}

// saveFreeModelsCache 保存免费模型列表到本地缓存
func saveFreeModelsCache(models map[string]bool) {
	data, err := json.MarshalIndent(models, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(freeModelsCacheFile, data, 0644)
}

// fetchFreeModelsFromDocs 从 OpenCode Zen 文档抓取免费模型列表
// 从定价表格中提取 Input 价格为 "Free" 的模型
func fetchFreeModelsFromDocs() (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET",
		"https://docs.opencode.ai/docs/zen/", nil)
	resp, err := mgmtClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	models := make(map[string]bool)

	// 从定价表格提取免费模型：格式 <tr><td>ModelName</td><td>Free</td>...
	// 匹配 Input 列为 "Free" 的行
	re := regexp.MustCompile(`<tr><td>([^<]+)</td><td>Free</td>`)
	matches := re.FindAllStringSubmatch(html, -1)

	for _, match := range matches {
		if len(match) > 1 {
			name := strings.TrimSpace(match[1])
			modelID := modelNameToID(name)
			if modelID != "" {
				models[modelID] = true
			}
		}
	}

	if len(models) == 0 {
		return nil, fmt.Errorf("no free models found in docs")
	}

	return models, nil
}

// modelNameToID 将文档模型名称转换为 API ID
func modelNameToID(name string) string {
	special := map[string]string{
		"Big Pickle":                      "big-pickle",
		"MiMo-V2.5 Free":                  "mimo-v2.5-free",
		"Ling 3.0 Flash Fin Free":         "ling-3.0-flash-fin-free",
		"Nemotron 3 Ultra Free":           "nemotron-3-ultra-free",
		"Nemotron 3.5 Lightning Free":     "nemotron-3.5-lightning-free",
		"Muse Spark 1.3 Contributor Free": "muse-spark-1.3-contributor-free",
	}
	if id, ok := special[name]; ok {
		return id
	}
	return ""
}

// loadFallbackFreeModels 加载兜底免费模型列表
// 优先级: 本地缓存 > 硬编码默认值
func loadFallbackFreeModels() map[string]bool {
	// 尝试读取本地缓存
	if cache := loadFreeModelsCache(); cache != nil && len(cache) > 0 {
		slog.Info("using cached free models", "count", len(cache))
		return cache
	}

	// 使用硬编码默认值
	slog.Info("using default free models", "count", len(defaultFreeModels))
	return defaultFreeModels
}

// refreshFreeModelsDocs 刷新文档免费模型缓存（带重试）
func refreshFreeModelsDocs() {
	var models map[string]bool
	var lastErr error

	// 重试 3 次
	for attempt := 0; attempt < 3; attempt++ {
		fetched, err := fetchFreeModelsFromDocs()
		if err == nil {
			models = fetched
			break
		}
		lastErr = err
		slog.Warn("free models fetch failed", "attempt", attempt+1, "error", err)
		time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
	}

	if models == nil {
		// 全部失败，使用兜底策略
		slog.Error("all fetch attempts failed, using fallback", "error", lastErr)
		models = loadFallbackFreeModels()
	} else {
		// 成功，更新缓存
		saveFreeModelsCache(models)
	}

	freeModelDocsMu.Lock()
	freeModelDocsCache = models
	freeModelDocsTime = time.Now()
	freeModelDocsMu.Unlock()

	slog.Info("free models loaded", "count", len(models))
}

// isFreeModelFromDocs 检查模型是否在文档免费列表中
func isFreeModelFromDocs(modelID string) bool {
	freeModelDocsMu.RLock()
	defer freeModelDocsMu.RUnlock()

	// 缓存为空或超过 1 小时，异步刷新
	if freeModelDocsCache == nil || time.Since(freeModelDocsTime) > time.Hour {
		go refreshFreeModelsDocs()
	}

	// 缓存为空时使用默认值
	if freeModelDocsCache == nil {
		return defaultFreeModels[modelID]
	}

	return freeModelDocsCache[modelID]
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
	editedFreeAliases    = map[string]string{} // 用户自定义的免费模型改名映射
	hiddenFreeAliases    = map[string]bool{}   // 已禁用的免费模型（对外不暴露）
	reasoningEffortMap   = map[string]string{}
	forceDisableThinking bool
	apiKey               string   // 统一网关密钥（config.api_key），空 = 不启用
	apiKeys              []string // 动态管理的 API 密钥列表（config.api_keys[].key）
	// promptCacheRetention 注入上游的缓存保留时长；"" = 运行时默认 "24h"，"off" 禁用注入。
	promptCacheRetention string
	// truncationStopReasonCfg 决定“流未见到合法 finish_reason 就结束”时向下游声明的
	// 停止原因（OpenAI 语义；Claude 输出经 claudeStopReasonFromOpenAI 映射）。
	// "" = 运行时默认 "length"。目的是不再把截断洗白成 end_turn/stop。
	truncationStopReasonCfg string
	cacheBreakpoints        = true                 // 是否注入 Anthropic 风格 cache_control 断点
	textOnlyModels          = []string{"deepseek"} // 只接受文本的上游模型前缀（默认 deepseek 系）
	debugMode               bool
	configMu                sync.RWMutex
	storedResponses         = map[string]StoredResponseState{}
	storedResponsesMu       sync.RWMutex
	modelRegionMap          = map[string]string{} // 模型区域限制：模型 ID → 所需区域
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
	RequestCount       int64   `json:"request_count"`
	PromptTokens       int64   `json:"prompt_tokens"`
	CompletionTokens   int64   `json:"completion_tokens"`
	TotalTokens        int64   `json:"total_tokens"`
	CacheReadTokens    int64   `json:"cache_read_tokens,omitempty"`
	CacheCreatedTokens int64   `json:"cache_created_tokens,omitempty"`
	AvgTTFTMs          float64 `json:"avg_ttft_ms,omitempty"`      // 平均首 token 延迟
	AvgOutputSpeed     float64 `json:"avg_output_speed,omitempty"` // 平均输出速度 tokens/s
	StreamReqCount     int64   `json:"stream_req_count,omitempty"` // 流式请求数
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
	HiddenFreeAliases    []string          `json:"hidden_free_aliases,omitempty"`
	EditedFreeAliases    map[string]string `json:"edited_free_aliases,omitempty"`
	ReasoningEffortMap   map[string]string `json:"reasoning_effort_map"`
	ForceDisableThinking bool              `json:"force_disable_thinking"`
	// ModelRegionMap 定义模型的区域限制：键是上游模型 ID（支持 "*" 通配），值是所需区域（如 "us"）。
	// 匹配到的模型只会路由到该区域的代理节点；未匹配的模型不受区域限制。
	ModelRegionMap map[string]string `json:"model_region_map,omitempty"`
	// ApiKey 统一网关密钥：客户端用它通过鉴权并按付费档获取全量模型；
	// 留空时退回现状（任意有效 sk- key 或免密钥免费档）。
	ApiKey string `json:"api_key,omitempty"`
	// ApiKeys 动态管理的 API 密钥列表：支持多 key 增删，优先级高于静态 api_key。
	ApiKeys []ApiKeyEntry `json:"api_keys,omitempty"`
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
	// 0 = 默认 15min；负数 = 禁用健康检查；URL 空 = 默认 https://opencode.ai/zen/v1/models
	NodeHealthIntervalMinutes int    `json:"node_health_interval_minutes,omitempty"`
	NodeHealthProbeURL        string `json:"node_health_probe_url,omitempty"`

	// ---- 缓存增强 ----
	// PromptCacheRetention 注入上游的 prompt 前缀缓存保留时长：
	// "24h"（默认，拉长 zen 网关 ~5min 的缓存 TTL）、"in_memory" 或 "off"（不注入）。
	PromptCacheRetention string `json:"prompt_cache_retention,omitempty"`
	// CacheControlBreakpoints 是否注入 Anthropic 风格 cache_control 断点
	// （{type:ephemeral, ttl:1h}）；GLM/Zhipu 等拒绝未知字段的模型自动跳过。默认 true。
	CacheControlBreakpoints *bool `json:"cache_control_breakpoints,omitempty"`
	// Socks5Sticky 为 true（默认）时，静态 socks5_proxies 轮询模式按会话固定出口：
	// 同一账号/会话始终走同一出口代理，上游按出口隔离的 prompt 缓存得以持续累积。
	// 置为 false 恢复纯轮询。
	Socks5Sticky *bool `json:"socks5_sticky,omitempty"`
	// TextOnlyModels 只接受文本输入的上游模型前缀列表（大小写不敏感前缀匹配）。
	// 请求解析到这些模型时，图片/文档内容静默降级为文本标注后继续转发。
	// 不填默认 ["deepseek"]；显式设置（含空数组）替换默认值。
	TextOnlyModels []string `json:"text_only_models,omitempty"`
	// TruncationStopReason 流未见到合法 finish_reason 就结束时的兜底停止原因
	// （OpenAI 语义：stop/length/tool_calls/content_filter，默认 "length"）。
	// Claude 输出自动映射为 max_tokens 等对应值。用于把上游静默截断如实告知下游。
	TruncationStopReason string `json:"truncation_stop_reason,omitempty"`
}

// QuotaSignalsConfig 配额耗尽判定签名（纯配置，不记运行时状态）。
type QuotaSignalsConfig struct {
	ErrorTypes      []string `json:"error_types,omitempty"`
	MessageKeywords []string `json:"message_keywords,omitempty"`
}

// ApiKeyEntry 动态管理的 API 密钥条目。
type ApiKeyEntry struct {
	Key       string `json:"key"`
	Name      string `json:"name,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

func defaultQuotaErrorTypes() []string {
	// 注意：不要加入 FreeTierError。它表示"上游拒绝我们的客户端身份"
	// （x-opencode-session 形态不合规等），换节点无法修复，
	// 归类为配额信号只会白白消耗节点切换预算并掩盖真实原因。
	// 修复见 opencode_id.go。
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

// startConfigWatcher 后台轮询 config.json 修改时间，检测到变化时自动加载并应用。
// 解析失败保留旧配置，仅打 warn 日志。
func startConfigWatcher() {
	go func() {
		var lastModTime time.Time
		if info, err := os.Stat(configPath); err == nil {
			lastModTime = info.ModTime()
		}
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			info, err := os.Stat(configPath)
			if err != nil {
				continue
			}
			if !info.ModTime().After(lastModTime) {
				continue
			}
			lastModTime = info.ModTime()
			// 验证新配置：只解析，不改运行时
			newCfg := loadConfig(configPath)
			// loadConfig 不返回 error，但空 model_alias + 空 api_key 说明解析失败
			// 用 JSON re-parse 验证格式
			raw, readErr := os.ReadFile(configPath)
			if readErr != nil {
				slog.Warn("config watcher: read failed", "error", readErr)
				continue
			}
			var verify AppConfig
			if json.Unmarshal(raw, &verify) != nil {
				slog.Warn("config watcher: invalid JSON, keeping old config")
				continue
			}
			// 验证通过，应用新配置
			applyConfig(newCfg)
			if err := saveConfig(configPath, newCfg); err != nil {
				slog.Warn("config watcher: save normalized config failed", "error", err)
			}
			slog.Info("config hot-reloaded",
				"aliases", len(newCfg.ModelAlias),
				"api_key_set", newCfg.ApiKey != "",
			)
		}
	}()
}

func applyConfig(cfg AppConfig) {
	configMu.Lock()
	defer configMu.Unlock()
	if cfg.ModelAlias != nil {
		modelAlias = cfg.ModelAlias
	}
	if cfg.EditedFreeAliases != nil {
		editedFreeAliases = cfg.EditedFreeAliases
	}
	hiddenFreeAliases = map[string]bool{}
	for _, id := range cfg.HiddenFreeAliases {
		hiddenFreeAliases[id] = true
	}
	if cfg.ModelRegionMap != nil {
		modelRegionMap = cfg.ModelRegionMap
	}
	if cfg.ReasoningEffortMap != nil {
		reasoningEffortMap = cfg.ReasoningEffortMap
	}
	forceDisableThinking = cfg.ForceDisableThinking
	apiKey = cfg.ApiKey
	// 动态 API 密钥：提取 key 值列表供认证匹配
	apiKeys = make([]string, 0, len(cfg.ApiKeys))
	for _, k := range cfg.ApiKeys {
		if k.Key != "" {
			apiKeys = append(apiKeys, k.Key)
		}
	}
	truncationStopReasonCfg = cfg.TruncationStopReason
	if cfg.PromptCacheRetention != "" {
		promptCacheRetention = cfg.PromptCacheRetention
	}
	if cfg.CacheControlBreakpoints != nil {
		cacheBreakpoints = *cfg.CacheControlBreakpoints
	}
	// text_only_models：显式设置（含空数组）替换默认值；未配置保持默认 ["deepseek"]。
	if cfg.TextOnlyModels != nil {
		textOnlyModels = cfg.TextOnlyModels
	}

	sticky := true
	if cfg.Socks5Sticky != nil {
		sticky = *cfg.Socks5Sticky
	}

	// 更新出口客户端配置
	if egress != nil {
		egress.Configure(cfg.Socks5Proxies, cfg.ActiveSocks5, cfg.Socks5PaidDirect, sticky)
	}

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
	// 0 = 默认 15min；负数 = 禁用健康检查
	if cfg.NodeHealthIntervalMinutes < 0 {
		proxyPool.probeInterval = -1 // 禁用
	} else if cfg.NodeHealthIntervalMinutes > 0 {
		proxyPool.probeInterval = time.Duration(cfg.NodeHealthIntervalMinutes) * time.Minute
	} else {
		proxyPool.probeInterval = 0 // 未设 → 默认 15min
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
	ef := editedFreeAliases
	hfa := hiddenFreeAliases
	configMu.RUnlock()
	if ok {
		return alias
	}
	// 检查用户自定义的免费模型改名：如果客户端发来的名字在 editedFree 的值中，反向映射回原始 ID
	if ef != nil {
		for originalID, editedName := range ef {
			if editedName == m {
				// 已禁用的模型不映射
				if hfa[originalID] {
					return m
				}
				return originalID
			}
		}
	}
	// 已禁用的模型不映射到 -free 版本
	if hfa[m] {
		return m
	}
	// Clients see free models without the "-free" suffix from /v1/models.
	// Map the display name back to the upstream free ID whenever that free
	// version exists in the catalog (project only serves free models, so the
	// presence of a paid twin does not prevent the free mapping).
	if m != "" && !isFreeModel(m) {
		freeID := m + "-free"
		if modelExistsInCaches(freeID) {
			return freeID
		}
	}
	return m
}

// lookupModelRegion 查找模型的区域限制。支持精确匹配和 "*" 通配符。
// 返回空字符串表示无区域限制。
func lookupModelRegion(modelID string) string {
	configMu.RLock()
	defer configMu.RUnlock()
	if len(modelRegionMap) == 0 {
		return ""
	}
	// 精确匹配
	if region, ok := modelRegionMap[modelID]; ok {
		return region
	}
	// 通配符匹配（取最长匹配前缀）
	var bestMatch string
	var bestRegion string
	for pattern, region := range modelRegionMap {
		if pattern == "*" {
			continue // "*" 作为 fallback 在最后处理
		}
		// 支持 "*" 作为尾部通配符（如 "muse-*"）
		if strings.HasSuffix(pattern, "*") {
			prefix := strings.TrimSuffix(pattern, "*")
			if strings.HasPrefix(modelID, prefix) && len(pattern) > len(bestMatch) {
				bestMatch = pattern
				bestRegion = region
			}
		}
	}
	if bestRegion != "" {
		return bestRegion
	}
	// "*" 全局通配
	if region, ok := modelRegionMap["*"]; ok {
		return region
	}
	return ""
}

// persistModelRegion 自动学习的区域限制持久化到配置文件。
func persistModelRegion(modelID, region string) {
	cfg := loadConfig(configPath)
	if cfg.ModelRegionMap == nil {
		cfg.ModelRegionMap = map[string]string{}
	}
	cfg.ModelRegionMap[modelID] = region
	if err := saveConfig(configPath, cfg); err != nil {
		slog.Warn("failed to persist model region", "model", modelID, "region", region, "error", err)
	} else {
		slog.Info("model region persisted", "model", modelID, "region", region)
	}
}

// ======================== 区域探测 ========================

// regionProbeState 记录单个模型的区域探测进度。
type regionProbeState struct {
	triedRegions map[string]bool // 已尝试的区域
	failedRegion string          // 区域已知但无节点
}

var (
	regionProbeMap   = map[string]*regionProbeState{}
	regionProbeMapMu sync.RWMutex
)

// getOrCreateProbeState 获取或创建模型的探测状态。
func getOrCreateProbeState(modelID string) *regionProbeState {
	regionProbeMapMu.Lock()
	defer regionProbeMapMu.Unlock()
	if s, ok := regionProbeMap[modelID]; ok {
		return s
	}
	s := &regionProbeState{triedRegions: map[string]bool{}}
	regionProbeMap[modelID] = s
	return s
}

// nextUntriedRegion 从未尝试的区域中返回一个有可用节点的区域；全部试过则返回 ""。
func nextUntriedRegion(modelID string) string {
	probe := getOrCreateProbeState(modelID)
	regions := proxyPool.availableRegions()
	for _, r := range regions {
		if r == "" {
			continue // 跳过无区域标记的节点（直连/未知区域）
		}
		if probe.triedRegions[r] {
			continue
		}
		// 检查该区域是否有可用节点
		if n := proxyPool.pickForRegion(false, r); n != nil {
			return r
		}
	}
	return ""
}

// markRegionTried 标记区域已尝试（成功或失败均可调用）。
func markRegionTried(modelID, region string) {
	probe := getOrCreateProbeState(modelID)
	probe.triedRegions[region] = true
}

// markRegionFailed 标记区域已知但无节点可用（不计入探测）。
func markRegionFailed(modelID, region string) {
	probe := getOrCreateProbeState(modelID)
	probe.failedRegion = region
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

// getPromptCacheRetention 返回注入上游请求的缓存保留时长。
// ""（未配置）返回运行时默认 "24h"——把 zen 网关的前缀缓存 TTL 从 ~5 分钟拉长到一天；
// "off" 完全禁用注入。
func getPromptCacheRetention() string {
	configMu.RLock()
	defer configMu.RUnlock()
	if promptCacheRetention == "" {
		return "24h"
	}
	return promptCacheRetention
}

func getCacheBreakpoints() bool {
	configMu.RLock()
	defer configMu.RUnlock()
	return cacheBreakpoints
}

// truncationStopReason 返回“流未见到合法 finish_reason 就结束”时应声明的
// OpenAI 语义停止原因。默认 "length"——语义上最接近“输出在自然停止前被切断”，
// 且多数 agent 客户端对 length 有续写/截断处理路径，而不是像 end_turn 那样
// 把残缺回复当完整结果。
func truncationStopReason() string {
	configMu.RLock()
	defer configMu.RUnlock()
	switch truncationStopReasonCfg {
	case "", "length":
		return "length"
	case "stop", "tool_calls", "content_filter":
		return truncationStopReasonCfg
	default:
		// 配了协议外的值：回退默认，不把垃圾透传给下游。
		return "length"
	}
}

// rejectsCacheControl 报告解析后的上游模型是否已知会拒绝 Anthropic 风格的
// cache_control 字段（GLM/Zhipu 对未知顶层字段报 "Extra inputs are not permitted"）。
func rejectsCacheControl(modelID string) bool {
	name := strings.ToLower(strings.TrimSpace(modelID))
	return strings.HasPrefix(name, "glm") || strings.HasPrefix(name, "zhipu") || strings.HasPrefix(name, "z-ai") || strings.HasPrefix(name, "zai")
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
	recordTokenUsageWithCache(model, promptTokens, completionTokens, totalTokens, 0, 0)
}

// recordTokenUsageWithCache 在 recordTokenUsage 基础上累计缓存读/写 token。
// cacheCreated 仅统计 Anthropic 风格的缓存写入；DeepSeek 的
// prompt_cache_miss_tokens 是普通未命中输入，不算缓存写入。
func recordTokenUsageWithCache(model string, promptTokens, completionTokens, totalTokens, cacheCreated, cacheRead int64) {
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
	if cacheCreated > 0 {
		ms.CacheCreatedTokens += cacheCreated
	}
	if cacheRead > 0 {
		ms.CacheReadTokens += cacheRead
	}
	tokenStatsMu.Unlock()
	go saveTokenStats()
}

// recordModelSpeed 在请求结束时累计模型输出速度和首 token 延迟（仅流式）。
func recordModelSpeed(model string, ttftMs int64, outputSpeed float64) {
	if model == "" || outputSpeed <= 0 {
		return
	}
	tokenStatsMu.Lock()
	ms, ok := tokenStats.Models[model]
	if !ok {
		ms = &ModelStats{}
		tokenStats.Models[model] = ms
	}
	ms.StreamReqCount++
	// 增量平均：newAvg = oldAvg + (newVal - oldAvg) / count
	n := float64(ms.StreamReqCount)
	ms.AvgTTFTMs += (float64(ttftMs) - ms.AvgTTFTMs) / n
	ms.AvgOutputSpeed += (outputSpeed - ms.AvgOutputSpeed) / n
	tokenStatsMu.Unlock()
	go saveTokenStats()
}

// recordUsageStats 把一份上游 usage 记账到 token 统计，并返回解析出的
// (输入, 输出, 缓存写入, 缓存读取) 供调用方复用于调用日志。
//
// 必须统一经 usageFromMap 解析：其返回序第三/四项就是「缓存写入」「缓存读取」。
// 历史上各调用点另外调 parseCacheUsage（返回序恰为 read, created）并当成
// (created, read) 使用，于是 cache_read 被写进 cache_created，面板缓存命中率
// 恒为 0%。统一走这里可避免再次写反。
func recordUsageStats(model string, u map[string]any) (pt, ct, cc, cr int64) {
	pt, ct, cc, cr = usageFromMap(u)
	if tt, _ := u["total_tokens"].(float64); tt > 0 {
		recordTokenUsageWithCache(model, pt, ct, int64(tt), cc, cr)
	}
	return pt, ct, cc, cr
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

// multimodalAttachedLabel / multimodalDocumentLabel 是文本-only 上游模型收到
// 图片/文档内容时的替换标注，与 Claude 转换器对 tool_result 附件使用的标注一致，
// 保证工具图片与消息图片降级行为相同。
const (
	multimodalAttachedLabel = "[image attached]"
	multimodalDocumentLabel = "[document attached]"
)

// isTextOnlyModel 报告解析后的上游模型是否只接受文本输入
// （大小写不敏感前缀匹配 text_only_models 配置，默认 ["deepseek"]）。
func isTextOnlyModel(modelID string) bool {
	name := strings.ToLower(strings.TrimSpace(modelID))
	if name == "" {
		return false
	}
	configMu.RLock()
	defer configMu.RUnlock()
	for _, prefix := range textOnlyModels {
		if strings.HasPrefix(name, strings.ToLower(strings.TrimSpace(prefix))) {
			return true
		}
	}
	return false
}

// downgradeMultimodalContent 把 image_url（"[image attached]"）和 file
// （"[document attached]"）内容部分替换为文本标注，使发往 text-only 上游模型
// （如 DeepSeek）的请求继续工作而不是报 "image not supported"。
// 文本与其他部分保留，相对顺序不变。非 text-only 或无多模态内容时原样返回。
func downgradeMultimodalContent(content []any, textOnly bool) any {
	if !textOnly {
		return content
	}
	out := make([]any, 0, len(content))
	downgraded := false
	for _, part := range content {
		pm, ok := part.(map[string]any)
		if !ok {
			out = append(out, part)
			continue
		}
		switch pm["type"] {
		case "image_url":
			downgraded = true
			out = append(out, map[string]any{"type": "text", "text": multimodalAttachedLabel})
		case "file":
			downgraded = true
			out = append(out, map[string]any{"type": "text", "text": multimodalDocumentLabel})
		default:
			out = append(out, part)
		}
	}
	if !downgraded {
		return content
	}
	return out
}

func convertMessagesForUpstream(messages []Message, textOnly bool) []map[string]any {
	converted := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		clean := map[string]any{}
		if msg.Role != "" {
			clean["role"] = msg.Role
		}
		content := normalizeContent(msg.Content)
		if arr, ok := content.([]any); ok {
			content = downgradeMultimodalContent(arr, textOnly)
		}
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
		"messages": convertMessagesForUpstream(req.Messages, isTextOnlyModel(req.Model)),
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
	// 缓存增强：向 zen 上游显式声明 prompt 前缀缓存的保留时长。
	// 上游默认约 5 分钟(in_memory)，agent 任务间歇易过期，导致缓存难命中；
	// 注入 retention 后拉长到 24h。客户端显式传入的值(extra_body)优先。
	if retention := getPromptCacheRetention(); retention != "off" {
		if _, exists := converted["prompt_cache_retention"]; !exists {
			converted["prompt_cache_retention"] = retention
		}
	}
	// Anthropic 风格 cache_control 断点：对接受该字段的模型(排除 GLM/Zhipu)
	// 显式标记缓存断点并拉长 TTL。对不支持的上游，zen 网关负责剥离；
	// DeepSeek 等自动前缀缓存不受影响。
	if getCacheBreakpoints() && !rejectsCacheControl(req.Model) {
		if _, exists := converted["cache_control"]; !exists {
			converted["cache_control"] = map[string]any{"type": "ephemeral", "ttl": "1h"}
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
		// L1：过滤别名（如 Zen 的 "sensitive"）归一为 content_filter；
		// 协议外未知值按异常终止改写为配置的截断原因，不再原样透传。
		if s, ok := choice["finish_reason"].(string); ok && s != "" {
			choice["finish_reason"] = mapUpstreamFinishReason(s)
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
				// L1：非流式响应同样归一化异常 finish 值（过滤别名/协议外值）。
				if s, ok := choice["finish_reason"].(string); ok && s != "" {
					choice["finish_reason"] = mapUpstreamFinishReason(s)
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
	dynamicKeys := make(map[string]bool, len(apiKeys))
	for _, k := range apiKeys {
		dynamicKeys[k] = true
	}
	configMu.RUnlock()
	if unified != "" && token == unified {
		return UpstreamAuth{Mode: AuthRoutePublic, Source: source}
	}
	// 动态 API 密钥列表：与静态 api_key 等效，均走 public 免费档。
	if dynamicKeys[token] {
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
		// Go-catalog-only models (e.g. ox-alpha-free) must be routed to the
		// Go endpoint regardless of auth mode; otherwise the zen endpoint
		// rejects them with "Model ... is not supported".
		return isGoCatalogOnlyModel(modelID)
	}
}

// isFreeModel 判断模型是否属于免费模型（从文档列表判断）
func isFreeModel(modelID string) bool {
	return isFreeModelFromDocs(modelID)
}

// publicFacingModelID strips the upstream "-free" suffix for client-visible catalogs.
func publicFacingModelID(modelID string) string {
	if strings.HasSuffix(modelID, "-free") {
		return strings.TrimSuffix(modelID, "-free")
	}
	return modelID
}

func modelExistsInCaches(modelID string) bool {
	modelMu.RLock()
	defer modelMu.RUnlock()
	return containsModelWithID(modelsCache, modelID) || containsModelWithID(goModelsCache, modelID)
}

func buildOCRequest(modelID string, bodyMap map[string]any, auth UpstreamAuth, nodeFP string) (*http.Request, error) {
	return buildOCRequestWithEndpoint(modelID, bodyMap, auth, auth.shouldUseGoEndpoint(modelID), nodeFP)
}

func buildOCRequestWithEndpoint(modelID string, bodyMap map[string]any, auth UpstreamAuth, useGoEndpoint bool, nodeFP string) (*http.Request, error) {
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
	req.Header.Set("x-opencode-session", getOrCreateSessionByNode(nodeFP))
	req.Header.Set("x-opencode-request", newOpenCodeID("msg"))
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
	// 配额熔断闸门：节点池无可用节点且直连出口已被上游 429 限流时，
	// 直接返回 429，不再访问上游（见 quota_halt.go）。
	if halted, haltBody := quotaHaltGuard.gate(auth); halted {
		return quotaHaltResponse(ctx, haltBody)
	}
	initOCSession()

	var bodyMap map[string]any
	if err := json.Unmarshal(upstreamBody, &bodyMap); err != nil {
		return nil, 500, nil, fmt.Errorf("invalid request body")
	}
	// 上游免费层要求 stream=true 且 tools 必须含官方工具名；本函数服务非流式
	// 客户端，因此把上游 SSE 聚合回单个 chat.completion，并把工具名映射回
	// 客户端原名。见 agent_shape.go。
	toolMapping, injectedTools := ensureAgentUpstreamShape(bodyMap)
	useGoEndpoint := auth.shouldUseGoEndpoint(modelID)
	surface := "zen"
	if useGoEndpoint {
		surface = "go"
	}
	log := reqLogger(ctx)

	// 查找模型的区域限制
	requiredRegion := lookupModelRegion(modelID)
	if requiredRegion != "" {
		log.Info("model_region_restriction", "model", modelID, "region", requiredRegion)
		// 检查是否有该区域的可用节点
		if egress.NodesActive() {
			n := proxyPool.pickForRegion(false, requiredRegion)
			if n == nil {
				return nil, http.StatusBadRequest, nil, fmt.Errorf("model %s requires region '%s' but no nodes available in that region", modelID, requiredRegion)
			}
		}
	}

	var lastErr error
	var retryCount int
	var lastBody []byte
	var lastStatus int
	var lastHeader http.Header
	maxAttempts := maxUpstreamRetries
	if max401Retries > maxAttempts {
		maxAttempts = max401Retries
	}

	// 免费额度耗尽自动切节点：独立预算（默认 5 个节点），不占用重试闸门；
	// 循环上限 = 重试上限 + 配额预算，两者各自封顶互不挤占（重试仍由 canRetry 限制）。
	nodeSwitchPending := false
	quotaSwitches := 0
	maxQuotaSwitches := effectiveMaxQuotaNodeSwitches()
	loopBudget := maxAttempts + maxQuotaSwitches

	for attempt := 0; attempt < loopBudget; attempt++ {
		egResult := egress.Get(EgressRequest{Auth: auth, ForceSwitch: nodeSwitchPending, BodyMap: bodyMap, RequiredRegion: requiredRegion})
		client, nodeFp := egResult.Client, egResult.NodeFP
		up, err := buildOCRequestWithEndpoint(modelID, bodyMap, auth, useGoEndpoint, nodeFp)
		if err != nil {
			return nil, 500, nil, err
		}
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
				"node", nodeFp,
				"node_name", proxyPool.nameOf(nodeFp),
				"error", err.Error(),
			)
			// 连接错误（EOF 等）：标记节点为 dead，避免后续请求继续命中坏节点。
			// 无论是否还有重试预算都要标记，否则最后一次失败会让坏节点长期留在池中。
			if nodeFp != "" {
				proxyPool.mark(nodeFp, NodeDead, "connect_error:"+err.Error())
			}
			if canRetry {
				if nodeFp != "" {
					callLogEvent(ctx, "switch", nodeFp, "connect_error:"+err.Error())
				}
				client.CloseIdleConnections()
				egResult.Invalidate()
				nodeSwitchPending = true
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
			// 上游现在恒为 stream=true（形态要求），非流式客户端需要把 SSE
			// 聚合回单个 chat.completion；非 SSE 响应原样透传。
			b = aggregateUpstreamSSE(b, modelID, toolMapping, injectedTools)
			if isAnthropicFormat(b) {
				b = convertAnthropicToOpenAI(b, modelID)
			}
			b = convertRawToolCallsInBody(b)
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

		// 检测区域限制错误（自动学习 + 区域探测）
		if regionRestricted, detectedRegion := classifyRegionRestriction(resp.StatusCode, errBody); regionRestricted {
			callLogEvent(ctx, "region_restriction", nodeFp, fmt.Sprintf("http %d region_detected:%s", resp.StatusCode, detectedRegion))
			log.Warn("model_region_restriction_detected",
				"model", modelID,
				"detected_region", detectedRegion,
				"status", resp.StatusCode,
			)

			// === 路径 1：从错误消息中提取到了具体区域 → 自动学习 ===
			if detectedRegion != "" && lookupModelRegion(modelID) == "" {
				configMu.Lock()
				modelRegionMap[modelID] = detectedRegion
				configMu.Unlock()
				log.Info("model_region_auto_learned",
					"model", modelID,
					"region", detectedRegion,
				)
				go persistModelRegion(modelID, detectedRegion)
				if egress.NodesActive() {
					n := proxyPool.pickForRegion(false, detectedRegion)
					if n != nil {
						nodeSwitchPending = true
						refreshOCSession()
						continue
					}
				}
			}

			// === 路径 2：无法确定区域 → 探测所有可用区域 ===
			if lookupModelRegion(modelID) == "" && egress.NodesActive() {
				// 标记当前直连（区域未知）已失败
				markRegionTried(modelID, "")
				if nextRegion := nextUntriedRegion(modelID); nextRegion != "" {
					log.Info("region_probe_try",
						"model", modelID,
						"probe_region", nextRegion,
					)
					// 临时设置区域以触发节点选择
					configMu.Lock()
					modelRegionMap[modelID] = nextRegion
					configMu.Unlock()
					nodeSwitchPending = true
					refreshOCSession()
					continue
				}
				// 所有区域都试过了，恢复 modelRegionMap 并返回错误
				configMu.Lock()
				delete(modelRegionMap, modelID)
				configMu.Unlock()
				log.Warn("region_probe_exhausted",
					"model", modelID,
					"tried_all_regions", true,
				)
			}
		}

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

		// 出口已回退直连、上游仍 429：剩余重试都打在同一个 IP 上，只会空转。
		// 立即停止重试并进入熔断，等节点池恢复可用节点（见 quota_halt.go）。
		if egResult.Direct && resp.StatusCode == http.StatusTooManyRequests && auth.tier() == TierFree {
			if quotaHaltGuard.halt("direct_429", errBody) {
				log.Warn("quota_halt_entered",
					"try_model", modelID,
					"surface", surface,
					"status", resp.StatusCode,
					"attempt_index", attempt,
					"node_pool_size", proxyPool.nodeCount(),
				)
				callLogEvent(ctx, "quota_halt", nodeFp, "enter: 直连出口 429，停止重试等待节点恢复")
			}
			lastBody = errBody
			lastStatus = resp.StatusCode
			lastHeader = resp.Header
			lastErr = fmt.Errorf("upstream error")
			break
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
		// 免费层 429 按出口 IP 限流，5xx 也可能是出口问题：
		// 重试前切断 sticky，让同一会话换到下一个出口。
		egResult.Invalidate()
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
	// 配额熔断闸门：节点池无可用节点且直连出口已被上游 429 限流时，
	// 直接返回 429，不再访问上游（见 quota_halt.go）。
	if halted, haltBody := quotaHaltGuard.gate(auth); halted {
		body, status, header, err := quotaHaltResponse(ctx, haltBody)
		if err != nil {
			return nil, status, header, err
		}
		return io.NopCloser(bytes.NewReader(body)), status, header, nil
	}
	initOCSession()

	var bodyMap map[string]any
	if err := json.Unmarshal(upstreamBody, &bodyMap); err != nil {
		return nil, 500, nil, fmt.Errorf("invalid request body")
	}
	// 上游免费层要求 stream=true 且 tools 含官方工具名；响应侧用
	// toolNameRewriter 把工具名映射回客户端原名。见 agent_shape.go。
	toolMapping, injectedTools := ensureAgentUpstreamShape(bodyMap)
	useGoEndpoint := auth.shouldUseGoEndpoint(modelID)
	surface := "zen"
	if useGoEndpoint {
		surface = "go"
	}
	log := reqLogger(ctx)

	// 查找模型的区域限制
	requiredRegion := lookupModelRegion(modelID)
	if requiredRegion != "" {
		log.Info("model_region_restriction", "model", modelID, "region", requiredRegion)
		// 检查是否有该区域的可用节点
		if egress.NodesActive() {
			n := proxyPool.pickForRegion(false, requiredRegion)
			if n == nil {
				return nil, http.StatusBadRequest, nil, fmt.Errorf("model %s requires region '%s' but no nodes available in that region", modelID, requiredRegion)
			}
		}
	}

	var lastBody []byte
	var lastStatus int
	var lastHeader http.Header
	var retryCount int
	maxAttempts := maxUpstreamRetries
	if max401Retries > maxAttempts {
		maxAttempts = max401Retries
	}

	// 免费额度耗尽自动切节点（流式：仅头部非 2xx 时可切；已吐字节不可重试）：
	// 独立预算（默认 5 个节点），不占用重试闸门；循环上限 = 重试上限 + 配额预算。
	nodeSwitchPending := false
	quotaSwitches := 0
	maxQuotaSwitches := effectiveMaxQuotaNodeSwitches()
	loopBudget := maxAttempts + maxQuotaSwitches

	for attempt := 0; attempt < loopBudget; attempt++ {
		egResult := egress.Get(EgressRequest{Auth: auth, ForceSwitch: nodeSwitchPending, BodyMap: bodyMap, RequiredRegion: requiredRegion})
		client, nodeFp := egResult.Client, egResult.NodeFP
		up, err := buildOCRequestWithEndpoint(modelID, bodyMap, auth, useGoEndpoint, nodeFp)
		if err != nil {
			return nil, 500, nil, err
		}
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
				"node", nodeFp,
				"node_name", proxyPool.nameOf(nodeFp),
				"error", err.Error(),
			)
			// 连接错误（EOF 等）：标记节点为 dead，避免后续请求继续命中坏节点。
			// 无论是否还有重试预算都要标记，否则最后一次失败会让坏节点长期留在池中。
			if nodeFp != "" {
				proxyPool.mark(nodeFp, NodeDead, "connect_error:"+err.Error())
			}
			if canRetry {
				if nodeFp != "" {
					callLogEvent(ctx, "switch", nodeFp, "connect_error:"+err.Error())
				}
				client.CloseIdleConnections()
				egResult.Invalidate()
				nodeSwitchPending = true
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
			return newToolNameRewriter(wrapRawSSE(resp.Body), toolMapping, injectedTools), resp.StatusCode, resp.Header, nil
		}
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		logUpstreamError(ctx, modelID, resp.StatusCode, errBody)

		// 检测区域限制错误（自动学习 + 区域探测）
		if regionRestricted, detectedRegion := classifyRegionRestriction(resp.StatusCode, errBody); regionRestricted {
			callLogEvent(ctx, "region_restriction", nodeFp, fmt.Sprintf("http %d region_detected:%s", resp.StatusCode, detectedRegion))
			log.Warn("model_region_restriction_detected",
				"model", modelID,
				"detected_region", detectedRegion,
				"status", resp.StatusCode,
			)

			// === 路径 1：从错误消息中提取到了具体区域 → 自动学习 ===
			if detectedRegion != "" && lookupModelRegion(modelID) == "" {
				configMu.Lock()
				modelRegionMap[modelID] = detectedRegion
				configMu.Unlock()
				log.Info("model_region_auto_learned",
					"model", modelID,
					"region", detectedRegion,
				)
				go persistModelRegion(modelID, detectedRegion)
				if egress.NodesActive() {
					n := proxyPool.pickForRegion(false, detectedRegion)
					if n != nil {
						nodeSwitchPending = true
						refreshOCSession()
						continue
					}
				}
			}

			// === 路径 2：无法确定区域 → 探测所有可用区域 ===
			if lookupModelRegion(modelID) == "" && egress.NodesActive() {
				// 标记当前直连（区域未知）已失败
				markRegionTried(modelID, "")
				if nextRegion := nextUntriedRegion(modelID); nextRegion != "" {
					log.Info("region_probe_try",
						"model", modelID,
						"probe_region", nextRegion,
					)
					// 临时设置区域以触发节点选择
					configMu.Lock()
					modelRegionMap[modelID] = nextRegion
					configMu.Unlock()
					nodeSwitchPending = true
					refreshOCSession()
					continue
				}
				// 所有区域都试过了，恢复 modelRegionMap 并返回错误
				configMu.Lock()
				delete(modelRegionMap, modelID)
				configMu.Unlock()
				log.Warn("region_probe_exhausted",
					"model", modelID,
					"tried_all_regions", true,
				)
			}
		}

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

		// 出口已回退直连、上游仍 429：剩余重试都打在同一个 IP 上，只会空转。
		// 立即停止重试并进入熔断，等节点池恢复可用节点（见 quota_halt.go）。
		if egResult.Direct && resp.StatusCode == http.StatusTooManyRequests && auth.tier() == TierFree {
			if quotaHaltGuard.halt("direct_429", errBody) {
				log.Warn("quota_halt_entered",
					"try_model", modelID,
					"surface", surface,
					"status", resp.StatusCode,
					"attempt_index", attempt,
					"node_pool_size", proxyPool.nodeCount(),
				)
				callLogEvent(ctx, "quota_halt", nodeFp, "enter: 直连出口 429，停止重试等待节点恢复")
			}
			lastBody = errBody
			lastStatus = resp.StatusCode
			lastHeader = resp.Header
			break
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
		// 免费层 429 按出口 IP 限流，5xx 也可能是出口问题：
		// 重试前切断 sticky，让同一会话换到下一个出口。
		egResult.Invalidate()
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
		stats := &streamResultStats{start: time.Now()}
		doneSeen := false
		// L2：forwardedAny 一旦置位就不可再重放——下游已经收到字节。
		forwardedAny := false
		// 记录最近一个 chunk 的标识字段，截断终止 chunk 沿用之以保持流连续性。
		lastChunkID, lastChunkModel := "", ""
		var lastCreated float64
		haveLastCreated := false
		var lastPt, lastCt, lastCc, lastCr int64
		lastReadFailed := false

		consumeChatStream := func(upResp io.ReadCloser) (readFailed bool) {
			reader := bufio.NewReader(upResp)
			for {
				line, rerr := reader.ReadString('\n')
				if rerr != nil {
					if rerr == io.EOF {
						return false
					}
					reqLogger(r.Context()).Error("stream read error", "error", rerr)
					callLogEvent(clCtx, "stream_interrupt", "", rerr.Error())
					// 读错误本身不写任何下游字节：是否可重试由外层按 forwardedAny 判定。
					return true
				}
				if doneSeen {
					continue
				}
				trimmed := strings.TrimSpace(line)
				if trimmed == "data: [DONE]" {
					doneSeen = true
					stats.doneSeen = true
					forwardedAny = true
					w.Write([]byte("data: [DONE]\n\n"))
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					continue
				}

				if strings.HasPrefix(line, "data: ") {
					callLogFirstToken(clCtx) // 记录首 token 时间
					var raw map[string]any
					if json.Unmarshal([]byte(line[6:]), &raw) == nil {
						if v, ok := raw["id"].(string); ok && v != "" {
							lastChunkID = v
						}
						if v, ok := raw["model"].(string); ok && v != "" {
							lastChunkModel = v
						}
						if v, ok := raw["created"].(float64); ok {
							lastCreated = v
							haveLastCreated = true
						}
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
						pt, ct, cc, cr := recordUsageStats(req.Model, usage)
						if tt, _ := usage["total_tokens"].(float64); tt > 0 {
							lastPt, lastCt, lastCc, lastCr = pt, ct, cc, cr
						} else if pt > 0 {
							lastPt, lastCt, lastCc, lastCr = pt, ct, cc, cr
						}
					}
					continue
				}

				// 提取 usage（已在 convertStreamChunkWithUsage 中解析）
				if usage != nil && !doneSeen {
					pt, ct, cc, cr := recordUsageStats(req.Model, usage)
					if tt, _ := usage["total_tokens"].(float64); tt > 0 {
						lastPt, lastCt, lastCc, lastCr = pt, ct, cc, cr
					} else if pt > 0 {
						lastPt, lastCt, lastCc, lastCr = pt, ct, cc, cr
					}
				}

				forwardedAny = true
				w.Write([]byte(out))
				w.Write([]byte("\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}

		headerWritten := false
		for streamAttempt := 0; ; streamAttempt++ {
			upResp, status, _, err := callOpenCodeAPIStream(clCtx, upstreamBody, req.Model, auth)
			if err != nil || status < 200 || status >= 300 {
				callLogFinish(clCtx, status, fmt.Sprintf("upstream http %d", status), 0, 0, 0, 0)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if upResp != nil {
					errBody, _ := io.ReadAll(upResp)
					if len(errBody) > 0 {
						w.Write(rewriteUpstreamError(errBody))
						return
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error", "type": "upstream_error"}})
				return
			}
			if !headerWritten {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				w.WriteHeader(http.StatusOK)
				headerWritten = true
			}
			readFailed := consumeChatStream(upResp)
			upResp.Close()
			lastReadFailed = readFailed

			// L2：仅在尚未向下游转发任何字节时补试一次——重放零损失，
			// 也避免对持续性故障无限烧钱。
			retryable := (readFailed || !stats.sawFinish) && !forwardedAny
			if retryable && streamAttempt == 0 {
				reqLogger(r.Context()).Warn("empty_stream_retry",
					"protocol", "chat",
					"model", req.Model,
					"reason", map[bool]string{true: "read_error", false: "empty_no_finish"}[readFailed],
				)
				stats = &streamResultStats{start: stats.start}
				doneSeen = false
				continue
			}
			break
		}

		// L1：上游没给合法 finish_reason 就断流——补一个如实声明截断的终止
		// chunk 和 [DONE]，让下游状态机完整闭合，且不再伪装成正常完成。
		if !stats.sawFinish {
			termID := lastChunkID
			if termID == "" {
				termID = "chatcmpl-truncated-" + randomString(16)
			}
			termModel := lastChunkModel
			if termModel == "" {
				termModel = req.Model
			}
			created := lastCreated
			if !haveLastCreated {
				created = float64(time.Now().Unix())
			}
			synth, _ := json.Marshal(map[string]any{
				"id":      termID,
				"object":  "chat.completion.chunk",
				"created": int64(created),
				"model":   termModel,
				"choices": []map[string]any{{
					"index":         0,
					"delta":         map[string]any{},
					"finish_reason": truncationStopReason(),
				}},
			})
			forwardedAny = true
			w.Write([]byte("data: " + string(synth) + "\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if !doneSeen {
			callLogEvent(clCtx, "stream_interrupt", "", "EOF without [DONE]")
			w.Write([]byte("data: [DONE]\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if lastReadFailed {
			callLogFinish(clCtx, http.StatusBadGateway, "stream read error", lastPt, lastCt, lastCc, lastCr)
		} else {
			callLogFinish(clCtx, http.StatusOK, "", lastPt, lastCt, lastCc, lastCr)
		}
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
			w.Write(rewriteUpstreamError(respBody))
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
			recordUsageStats(req.Model, u)
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
		// Also include free models from Go catalog that aren't in the regular catalog.
		modelMu.RLock()
		for _, goModel := range goModelsCache {
			if isFreeModel(goModel.ID) && !containsModelWithID(filtered, goModel.ID) {
				filtered = append(filtered, goModel)
			}
		}
		modelMu.RUnlock()
		if len(filtered) > 0 {
			combinedModels = filtered
		}
	default:
		combinedModels = models
	}
	allModels := replaceModelIDsWithAliases(combinedModels, aliases)

	// 过滤已禁用的免费模型
	configMu.RLock()
	hfa := hiddenFreeAliases
	configMu.RUnlock()
	if len(hfa) > 0 {
		visible := make([]ModelInfo, 0, len(allModels))
		for _, m := range allModels {
			if !hfa[m.ID] {
				visible = append(visible, m)
			}
		}
		allModels = visible
	}

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

	// 读取用户自定义的免费模型改名映射
	configMu.RLock()
	ef := editedFreeAliases
	configMu.RUnlock()

	result := make([]ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		visibleIDs := aliasesByUpstream[model.ID]
		if len(visibleIDs) == 0 {
			// 先检查用户改名映射
			defaultID := publicFacingModelID(model.ID)
			if editedName, ok := ef[defaultID]; ok && editedName != "" {
				visibleIDs = []string{editedName}
			} else {
				visibleIDs = []string{defaultID}
			}
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
	var allModels []ModelInfo
	appendModels := func(list []ModelInfo) {
		for _, m := range list {
			if _, ok := seen[m.ID]; ok {
				continue
			}
			seen[m.ID] = struct{}{}
			// 从 models.dev 目录填充上下文/输出上限/输入模态（与 listModelsHandler 一致）
			if limit, ok := lookupModelLimit(m.ID); ok {
				ctx, out := limit.Context, limit.Output
				m.ContextWindow, m.MaxOutputTokens = &ctx, &out
				m.InputModalities = limit.InputModalities
			} else if limit, ok := lookupModelLimit(publicFacingModelID(m.ID)); ok {
				ctx, out := limit.Context, limit.Output
				m.ContextWindow, m.MaxOutputTokens = &ctx, &out
				m.InputModalities = limit.InputModalities
			}
			allModels = append(allModels, m)
		}
	}
	appendModels(modelsCache)
	appendModels(goModelsCache)
	modelMu.RUnlock()
	if len(allModels) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "无法获取模型列表，请检查上游服务是否可用",
		})
		return
	}
	// 附带免费模型列表（从文档缓存），供前端生成完整自动映射
	freeModelDocsMu.RLock()
	freeIDs := make([]string, 0, len(freeModelDocsCache))
	for id := range freeModelDocsCache {
		freeIDs = append(freeIDs, id)
	}
	freeModelDocsMu.RUnlock()
	sort.Strings(freeIDs)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object":      "list",
		"data":        allModels,
		"free_models": freeIDs,
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
		// L1：按 finish 语义分类映射，不再把缺失/未知值默认成 end_turn。
		switch classifyFinishReason(fr) {
		case finishNormal:
			switch fr {
			case "stop":
				stopReason = "end_turn"
			case "length":
				stopReason = "max_tokens"
			case "tool_calls", "function_call":
				stopReason = "tool_use"
			}
		case finishFilter:
			stopReason = "refusal"
		default:
			// finishNone（上游没给 finish）与 finishUnknown（协议外值）：
			// 如实声明为截断，而不是伪装成正常收尾。
			stopReason = claudeStopReasonFromOpenAI(truncationStopReason())
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
	// readFromSplit marks cache_read sourced from DeepSeek/OpenAI-style
	// counters (prompt_cache_hit_tokens / prompt_tokens_details.cached_tokens),
	// whose prompt_tokens includes the hit portion. An Anthropic-style
	// cache_read_input_tokens is already exclusive of input_tokens and must
	// not be subtracted.
	readFromSplit := false
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
			readFromSplit = true
		}
	}
	// DeepSeek-style counters split the prompt into hit (read) and miss
	// (ordinary input). Miss is not a cache write, so it is intentionally
	// left out of the Claude cache fields.
	if _, exists := usage["cache_read_input_tokens"]; !exists {
		if value, ok := usageIntField(upstreamUsage, "prompt_cache_hit_tokens"); ok {
			usage["cache_read_input_tokens"] = value
			readFromSplit = true
		}
	}
	// Anthropic semantics: input_tokens excludes cache reads (input, read and
	// creation are mutually exclusive). prompt_tokens from DeepSeek/OpenAI
	// includes the hit portion, so subtract it here; otherwise a client that
	// prices input and cache reads separately would bill the hit tokens twice.
	if readFromSplit {
		if read, ok := usage["cache_read_input_tokens"].(int); ok && read > 0 {
			if input, ok := usage["input_tokens"].(int); ok {
				if read >= input {
					usage["input_tokens"] = 0
				} else {
					usage["input_tokens"] = input - read
				}
			}
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
		var clPt, clCt, clCc, clCr int64
		for attempt := 0; ; attempt++ {
			upResp, status, _, err := callOpenCodeAPIStream(clCtx, upstreamBody, chatReq.Model, auth)
			if err != nil || status < 200 || status >= 300 {
				callLogFinish(clCtx, status, fmt.Sprintf("upstream http %d", status), 0, 0, 0, 0)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if upResp != nil {
					errBody, _ := io.ReadAll(upResp)
					if len(errBody) > 0 {
						w.Write(rewriteUpstreamError(errBody))
						return
					}
				}
				json.NewEncoder(w).Encode(map[string]any{
					"type":  "error",
					"error": map[string]string{"type": "api_error", "message": "upstream error"},
				})
				return
			}
			var emptyNoFinish bool
			clPt, clCt, clCc, clCr, emptyNoFinish = claudeStreamHandler(clCtx, w, upResp, claudeReq.Model, keepReasoning)
			upResp.Close()
			// L2：整条流没吐出任何事件也没见到合法 finish——重放不会损失
			// 已发出的字节。只补试一次，避免对持续故障无限烧钱。
			if emptyNoFinish && attempt == 0 {
				reqLogger(clCtx).Warn("empty_stream_retry",
					"protocol", "claude",
					"model", chatReq.Model,
				)
				continue
			}
			break
		}
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
			w.Write(rewriteUpstreamError(respBody))
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
			recordUsageStats(claudeReq.Model, u)
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
// claudeStreamHandler 把上游 OpenAI 形状的 SSE 流转换为 Anthropic Messages SSE。
// 除用量四元组外，还返回 emptyNoFinish：整条流既没见到合法 finish_reason、
// 也没向下游发出过任何事件——这是唯一可以安全重放的形态（L2 透明重试依据）。
func claudeStreamHandler(ctx context.Context, w http.ResponseWriter, respBody io.ReadCloser, model string, keepReasoning bool) (pt, ct, cc, cr int64, emptyNoFinish bool) {
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
		lpt, lct, lcc, lcr := recordUsageStats(model, fullUsage)
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
		callLogFirstToken(ctx) // 记录首 token 时间

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

		// L1：按语义分类处理 finish。已知值照旧映射；过滤别名（sensitive 等）
		// 归为 refusal；缺失或协议外未知值不再落入默认 end_turn 的伪装，
		// 而是保持未完成状态，由流末尾统一按截断声明。
		sig := classifyFinishReason(finishReason)
		if sig == finishNormal || sig == finishFilter {
			stats.finishReason = finishReason
			stats.sawFinish = true
			finished = true
			finalizeContentBlocks()

			stopReason = "end_turn"
			if sig == finishFilter {
				stopReason = "refusal"
			} else {
				switch finishReason {
				case "length":
					stopReason = "max_tokens"
				case "tool_calls", "function_call":
					stopReason = "tool_use"
				}
			}
			// Do not emit message_delta/stop yet: OpenAI-compatible upstreams often
			// send the usage-only chunk after finish_reason when include_usage=true.
			continue
		}
		if finishReason != "" {
			stats.finishReason = finishReason
			reqLogger(ctx).Warn("unknown_upstream_finish_reason",
				"model", model,
				"raw_finish", finishReason,
			)
		}
	}

	// L2 判定必须在 ensureMessageStart 之前：一旦发出 message_start 就不可重放。
	emptyNoFinish = !stats.sawFinish && !messageStartSent
	ensureMessageStart()
	if !finished {
		finalizeContentBlocks()
		// L1：流没以合法 finish 收尾（提前 EOF / 协议外 finish 值），
		// 如实声明截断，而不是沿用默认 end_turn 伪装成完整回复。
		stopReason = claudeStopReasonFromOpenAI(truncationStopReason())
		if !stats.sawFinish {
			reqLogger(ctx).Warn("stream_ended_without_finish",
				"protocol", "claude",
				"model", model,
				"done_seen", stats.doneSeen,
				"synthetic_stop_reason", stopReason,
				"retryable", emptyNoFinish,
			)
		}
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
		var rpt, rct, rcc, rcr int64
		for attempt := 0; ; attempt++ {
			upResp, status, _, err := callOpenCodeAPIStream(clCtx, upstreamBody, chatReq.Model, auth)
			if err != nil || status < 200 || status >= 300 {
				callLogFinish(clCtx, status, fmt.Sprintf("upstream http %d", status), 0, 0, 0, 0)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if upResp != nil {
					errBody, _ := io.ReadAll(upResp)
					if len(errBody) > 0 {
						w.Write(rewriteUpstreamError(errBody))
						return
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error"}})
				return
			}

			// L2：整条流没吐出任何事件也没见到合法 finish——补试一次。
			resp := &http.Response{
				StatusCode: status,
				Body:       upResp,
				Header:     make(http.Header),
			}
			var emptyNoFinish bool
			rpt, rct, rcc, rcr, emptyNoFinish = responsesStreamHandler(w, r, resp, chatReq.Model, chatReq.Model, wantReasoning, respReq.Tools, respReq.ToolChoice, respReq)
			upResp.Close()
			if emptyNoFinish && attempt == 0 {
				reqLogger(clCtx).Warn("empty_stream_retry",
					"protocol", "responses",
					"model", chatReq.Model,
				)
				continue
			}
			break
		}
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
			w.Write(rewriteUpstreamError(respBody))
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
			recordUsageStats(chatReq.Model, u)
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
// responsesStreamHandler 把上游 OpenAI 形状的 SSE 流转换为 Responses API SSE。
// 除用量四元组外，还返回 emptyNoFinish：整条流既没见到合法 finish_reason、
// 也没向下游发出过任何事件（L2 透明重试依据）。
func responsesStreamHandler(w http.ResponseWriter, r *http.Request, resp *http.Response, model string, _ string, wantReasoning bool, tools []ResponsesTool, toolChoice any, originalReq ResponsesAPIRequest) (pt, ct, cc, cr int64, emptyNoFinish bool) {
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
			// L1：读错误不再无声返回（那会让下游永远等不到终止事件），
			// 落到收尾路径按截断声明 response.incomplete。
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
		callLogFirstToken(ctx) // 记录首 token 时间

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
			// L1：过滤别名（sensitive 等）归一为 content_filter；
			// 协议外未知值按异常终止改写为配置的截断原因。
			finishReason = mapUpstreamFinishReason(finishReason)
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

	// L2 判定：没见过合法 finish 且 response.created 都还没发出——下游零字节，可安全重放。
	emptyNoFinish = !stats.sawFinish && !createdSent

	// L1：流没以合法 finish 收尾（提前 EOF / 读错误 / 协议外 finish 值），
	// 终止事件如实声明 incomplete，而不是 completed。
	if !stats.sawFinish {
		terminalStatus = "incomplete"
		terminalEvent = "response.incomplete"
		itemStatus = "incomplete"
		reqLogger(ctx).Warn("stream_ended_without_finish",
			"protocol", "responses",
			"model", model,
			"done_seen", stats.doneSeen,
			"terminal_event", terminalEvent,
			"retryable", emptyNoFinish,
		)
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
		lpt, lct, lcc, lcr := recordUsageStats(model, totalUsage)
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
		if len(modelRegionMap) > 0 {
			cfg.ModelRegionMap = modelRegionMap
		}
		configMu.RUnlock()
		if egress != nil {
			cfg.Socks5Proxies, cfg.ActiveSocks5, cfg.Socks5PaidDirect = egress.GetSocks5Config()
		}
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
			"hidden_free_aliases":           merged.HiddenFreeAliases,
			"edited_free_aliases":           merged.EditedFreeAliases,
			"reasoning_effort_map":          merged.ReasoningEffortMap,
			"force_disable_thinking":        merged.ForceDisableThinking,
			"model_region_map":              merged.ModelRegionMap,
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
			"prompt_cache_retention":        merged.PromptCacheRetention,
			"cache_control_breakpoints":     merged.CacheControlBreakpoints,
			"text_only_models":              merged.TextOnlyModels,
			"socks5_sticky":                 merged.Socks5Sticky,
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
	HiddenFreeAliases          *[]string            `json:"hidden_free_aliases"`
	EditedFreeAliases          *map[string]string   `json:"edited_free_aliases"`
	ReasoningEffortMap         map[string]string    `json:"reasoning_effort_map"`
	ForceDisableThinking       *bool                `json:"force_disable_thinking"`
	ModelRegionMap             map[string]string    `json:"model_region_map"`
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
	PromptCacheRetention       *string              `json:"prompt_cache_retention"`
	CacheControlBreakpoints    *bool                `json:"cache_control_breakpoints"`
	TextOnlyModels             []string             `json:"text_only_models"`
	Socks5Sticky               *bool                `json:"socks5_sticky"`
	ApiKey                     *string              `json:"api_key"`
	TruncationStopReason       *string              `json:"truncation_stop_reason"`
}

// mergeAppConfig 将补丁叠加到基础配置（GET 回显与订阅保存共用，AppConfig 语义）。
func mergeAppConfig(base, patch AppConfig) AppConfig {
	out := base
	if patch.ModelAlias != nil {
		out.ModelAlias = patch.ModelAlias
	}
	if patch.HiddenFreeAliases != nil {
		out.HiddenFreeAliases = patch.HiddenFreeAliases
	}
	if patch.EditedFreeAliases != nil {
		out.EditedFreeAliases = patch.EditedFreeAliases
	}
	if patch.ReasoningEffortMap != nil {
		out.ReasoningEffortMap = patch.ReasoningEffortMap
	}
	if patch.ModelRegionMap != nil {
		out.ModelRegionMap = patch.ModelRegionMap
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
	if patch.PromptCacheRetention != "" {
		out.PromptCacheRetention = patch.PromptCacheRetention
	}
	if patch.CacheControlBreakpoints != nil {
		out.CacheControlBreakpoints = patch.CacheControlBreakpoints
	}
	if patch.TextOnlyModels != nil {
		out.TextOnlyModels = patch.TextOnlyModels
	}
	if patch.Socks5Sticky != nil {
		out.Socks5Sticky = patch.Socks5Sticky
	}
	if patch.TruncationStopReason != "" {
		out.TruncationStopReason = patch.TruncationStopReason
	}
	return out
}

// mergeConfigPatch 面板 POST：指针字段非 nil 即显式覆盖（含清零）。
func mergeConfigPatch(base AppConfig, patch configPatch) AppConfig {
	out := base
	if patch.ModelAlias != nil {
		out.ModelAlias = patch.ModelAlias
	}
	if patch.HiddenFreeAliases != nil {
		out.HiddenFreeAliases = *patch.HiddenFreeAliases
	}
	if patch.EditedFreeAliases != nil {
		out.EditedFreeAliases = *patch.EditedFreeAliases
	}
	if patch.ReasoningEffortMap != nil {
		out.ReasoningEffortMap = patch.ReasoningEffortMap
	}
	if patch.ModelRegionMap != nil {
		out.ModelRegionMap = patch.ModelRegionMap
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
	if patch.PromptCacheRetention != nil {
		out.PromptCacheRetention = *patch.PromptCacheRetention
	}
	if patch.CacheControlBreakpoints != nil {
		out.CacheControlBreakpoints = patch.CacheControlBreakpoints
	}
	if patch.TextOnlyModels != nil {
		out.TextOnlyModels = patch.TextOnlyModels
	}
	if patch.Socks5Sticky != nil {
		out.Socks5Sticky = patch.Socks5Sticky
	}
	if patch.ApiKey != nil {
		out.ApiKey = *patch.ApiKey
	}
	if patch.TruncationStopReason != nil {
		out.TruncationStopReason = *patch.TruncationStopReason
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

// adminAPIKeysHandler 管理动态 API 密钥（GET 列表 / POST 新增 / DELETE 删除）。
func adminAPIKeysHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		configMu.RLock()
		cfg := loadConfig(configPath)
		keys := cfg.ApiKeys
		configMu.RUnlock()
		// 脱敏：只返回 key 的前 6 位和后 4 位
		type maskedKey struct {
			Key       string `json:"key"`
			MaskedKey string `json:"masked_key"`
			Name      string `json:"name,omitempty"`
			CreatedAt string `json:"created_at,omitempty"`
		}
		result := make([]maskedKey, len(keys))
		for i, k := range keys {
			mk := k.Key
			if len(mk) > 10 {
				mk = mk[:6] + "..." + mk[len(mk)-4:]
			}
			result[i] = maskedKey{Key: k.Key, MaskedKey: mk, Name: k.Name, CreatedAt: k.CreatedAt}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"keys": result})
	case http.MethodPost:
		var payload struct {
			Name string `json:"name,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			// 允许空 body
		}
		newKey := "sk-" + randomHex(32)
		entry := ApiKeyEntry{
			Key:       newKey,
			Name:      payload.Name,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		cfg := loadConfig(configPath)
		cfg.ApiKeys = append(cfg.ApiKeys, entry)
		if err := saveConfig(configPath, cfg); err != nil {
			http.Error(w, `{"error":"Failed to save config"}`, http.StatusInternalServerError)
			return
		}
		applyConfig(cfg)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"key":    newKey,
			"name":   entry.Name,
		})
	case http.MethodDelete:
		var payload struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Key == "" {
			http.Error(w, `{"error":"key is required"}`, http.StatusBadRequest)
			return
		}
		cfg := loadConfig(configPath)
		found := false
		for i, k := range cfg.ApiKeys {
			if k.Key == payload.Key {
				cfg.ApiKeys = append(cfg.ApiKeys[:i], cfg.ApiKeys[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			http.Error(w, `{"error":"key not found"}`, http.StatusNotFound)
			return
		}
		if err := saveConfig(configPath, cfg); err != nil {
			http.Error(w, `{"error":"Failed to save config"}`, http.StatusInternalServerError)
			return
		}
		applyConfig(cfg)
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
	if rng != "7d" && rng != "30d" && rng != "180d" {
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
	w.Write(adminPageHTML)
}

func renderLoginPage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(loginPageHTML)
	if msg != "" {
		w.Write([]byte("<script>document.addEventListener('DOMContentLoaded',function(){var m=document.getElementById('login-msg');if(m){m.textContent='" + msg + "';m.style.display='block'}})</script>"))
	}
}

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

	// 初始化出口客户端模块
	egress = NewEgressClient(httpClient)
	sticky := true
	if cfg.Socks5Sticky != nil {
		sticky = *cfg.Socks5Sticky
	}
	egress.Configure(cfg.Socks5Proxies, cfg.ActiveSocks5, cfg.Socks5PaidDirect, sticky)

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

	// 启动时先用默认值，异步刷新文档
	freeModelDocsCache = defaultFreeModels
	go refreshFreeModelsDocs()

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
	startConfigWatcher()
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
	mux.HandleFunc("/v1/messages/count_tokens", loggingMiddleware(claudeCountTokensHandler))
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
	mux.HandleFunc("/api/apikeys", loggingMiddleware(requireAuth(adminAPIKeysHandler)))
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
