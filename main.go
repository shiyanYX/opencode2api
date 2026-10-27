package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
// 改用 Transport 级超时：DialContext 保护连接阶段，TLSHandshakeTimeout 保护握手阶段，
// ResponseHeaderTimeout 保护头部阶段，Body 读取阶段不设超时，由上游关闭连接自然结束。
// directDialer 直连出口的拨号器（T3：建连上限 = connectTimeout，8s）。
// 抽成包级变量是为了让测试能直接断言它的 Timeout（而不是重跑一次真拨号）。
var directDialer = &net.Dialer{
	Timeout:   connectTimeout,
	KeepAlive: 30 * time.Second,
}

var httpClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           directDialer.DialContext,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: firstByteTimeout,
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
		TLSHandshakeTimeout: tlsHandshakeTimeout,
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

// ocClientVersion 读取当前上游 UA 版本（持锁，收窄既有的无锁读）。
// ⚠️ 已持有 ocInitMu 的函数（initOCSession / refreshOCSession /
// revertToPinnedOCVersion）内部【不得】调用它，否则自死锁——它们直接读 ocClientVer。
func ocClientVersion() string {
	ocInitMu.Lock()
	defer ocInitMu.Unlock()
	return ocClientVer
}

func fetchOCVersion() string {
	return probeOCVersion(mgmtClient)
}

// fetchOCVersionDirect 绕过节点池直连探测版本（会话切换时用，避免占用配额路径）。
func fetchOCVersionDirect() string {
	return probeOCVersion(ocVersionClient())
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
	// 用**已验证**的版本，而不是 npm latest：上游对版本双向校验（太旧 426、
	// 太新 403），且闸门通常落后 npm 数天。见 oc_version.go。
	ocClientVer = lastGoodOCVersion()
	ocSessionID = newSessionID()
	ocProjectID = randomHex(40)
	slog.Info("opencode version", "version", ocClientVer)
	slog.Info("session initialized", "session_id", ocSessionID)
	slog.Info("project initialized", "project_id", ocProjectID)
	ocInitDone = true
}

func refreshOCSession() {
	if noSessionRefresh { // 测试桩：只本地轮换，不访问网络
		ocInitMu.Lock()
		ocSessionID = newSessionID()
		ocProjectID = randomHex(40)
		ocInitDone = false
		ocInitMu.Unlock()
		return
	}
	// ★ 网络探测必须在临界区【外】：fetchOCVersionDirect() 最长要等 5s 超时，
	// 若持 ocInitMu 做它，会阻塞每一个读版本的请求（ocClientVersion() 也要这把锁）。
	// 这是 T7-I6 指出的新增锁点风险（顺带缓解了请求入口的既有停顿）。
	ver := fetchOCVersionDirect()
	ocInitMu.Lock()
	ocClientVer = ver
	ocSessionID = newSessionID()
	ocProjectID = randomHex(40)
	ocInitDone = false
	slog.Info("session refreshed", "version", ocClientVer, "session_id", ocSessionID)
	ocInitMu.Unlock()
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
	// InputLimit 单次 prompt 上限，与 ContextWindow（prompt+输出 的总预算）是两个量。
	// omitempty ⇒ 未知或无独立上限时**不下发**，前端不渲染任何东西。
	InputLimit *int64 `json:"input_limit,omitempty"`
}

type ModelLimit struct {
	Context         int64
	Output          int64
	InputModalities []string
	// InputLimit：单次请求 prompt 的上限。0 = 未知或没有独立输入上限
	// （models.dev 里该键**不存在**，不是 0）。只记「已知且严格小于 context」，
	// 绝不把缺失当作「等于 context」——全库 28 个带该键的模型里只有 2 个相等。
	InputLimit int64
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
	"big-pickle":                      true,
	"space-bunny-free":                true,
	"mimo-v2.6-flash-free":            true,
	"mimo-v2.5-free":                  true,
	"ling-3.0-flash-fin-free":         true,
	"nemotron-3-ultra-free":           true,
	"nemotron-3.5-lightning-free":     true,
	"muse-spark-1.3-contributor-free": true,
	"jev-1.13-free":                   true,
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
				// ★ 之前这个字段被整个丢掉，导致面板把 context 当输入上限显示
				//   （big-pickle 报 200,000、真实输入上限 160,000，偏大 20%）。
				//   models.dev 这份 5.4MB 响应本来就在拉，补一行结构体即可，
				//   **零新增网络请求**。
				Input  int64 `json:"input"`
				Output int64 `json:"output"`
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
				lim := ModelLimit{Context: m.Limit.Context, Output: m.Limit.Output, InputModalities: m.Modalities.Input}
				if m.Limit.Input > 0 && m.Limit.Input < m.Limit.Context {
					lim.InputLimit = m.Limit.Input
				}
				out[id] = lim
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
// 策略：先从 Endpoints 表格构建 "模型名称 → 模型 ID" 映射，再从 Pricing 表格获取免费模型名称，最后用映射转换
func fetchFreeModelsFromDocs() (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET",
		"https://opencode.ai/v2/docs/console/models/", nil)
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

	// 第一步：从 Endpoints 表格构建 "模型名称 → 模型 ID" 映射
	// 表格格式：<tr><td>Model Name</td><td>model-id</td><td>[<code...>https://...endpoint</code>]</td>...
	// 注意：第三列必须是 URL（允许 <code> 等标签包裹），避免匹配到 Pricing 表格的 <td>Free</td><td>Free</td>
	nameToID := make(map[string]string)
	reEndpoints := regexp.MustCompile(`<tr><td>([^<]+)</td><td>([a-zA-Z0-9._-]+)</td><td>(?:<[^>]+>)*https?://`)
	for _, match := range reEndpoints.FindAllStringSubmatch(html, -1) {
		if len(match) > 2 {
			name := strings.TrimSpace(match[1])
			id := strings.TrimSpace(match[2])
			if name != "" && id != "" {
				nameToID[name] = id
			}
		}
	}

	if len(nameToID) == 0 {
		return nil, fmt.Errorf("no model mappings found in endpoints table")
	}

	// 第二步：从 Pricing 表格提取 Input 列为 "Free" 的模型名称
	// 表格格式：<tr><td>Model Name</td><td>Free</td>...
	models := make(map[string]bool)
	rePricing := regexp.MustCompile(`<tr><td>([^<]+)</td><td>Free</td>`)
	for _, match := range rePricing.FindAllStringSubmatch(html, -1) {
		if len(match) > 1 {
			name := strings.TrimSpace(match[1])
			if id, ok := nameToID[name]; ok {
				models[id] = true
			}
		}
	}

	if len(models) == 0 {
		return nil, fmt.Errorf("no free models found in docs")
	}

	return models, nil
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

	// ★ 不再重试：这个源是"能抓到最好、抓不到无所谓的增强"，不是主源。
	//   原先重试 3 次（+2s/+4s/+6s 睡眠）再打 Error，源不可用时只会
	//   拖慢刷新并刷出噪音日志。失败一次、记一条 Warn、立刻走兜底。
	fetched, err := fetchFreeModelsFromDocs()
	if err == nil && len(fetched) > 0 {
		models = fetched
	} else {
		slog.Warn("free models docs fetch failed, using fallback (not fatal)", "error", err)
	}

	if models == nil {
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
	apiKey               string // 统一网关密钥（config.api_key），空 = 不启用
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
			renderLoginPage(w, r, "表单解析失败")
			return
		}
		if r.FormValue("password") != adminPassword {
			renderLoginPage(w, r, "密码错误")
			return
		}
		token, err := generateToken()
		if err != nil {
			renderLoginPage(w, r, "创建会话失败")
			return
		}
		sessionsMu.Lock()
		sessions[token] = struct{}{}
		sessionsMu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "session", Value: token, Path: "/", HttpOnly: true})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	renderLoginPage(w, r, "")
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
	AvgOutputSpeed     float64 `json:"avg_output_speed,omitempty"` // 平均输出速度 tokens/s（**每条速率的算术平均**，不是吞吐）
	StreamReqCount     int64   `json:"stream_req_count,omitempty"` // 流式请求数

	// ↓ 吞吐（DSH 同口径）：Σ完成token ÷ Σ生成秒，**不是** AvgOutputSpeed。
	//
	// ★ 不要叫 output_tps：名字里带 "tps" 会让人以为它是 AvgOutputSpeed 的别名，
	// 而它俩在生产上差 4.86 倍（452.70 vs 93.13）。差别只在加权方式：
	// AvgOutputSpeed 给每条请求同样的权重（0.8% 的超短请求贡献了它 69.4% 的值，
	// 却只占总时间的 0.002%）；吞吐按**耗时**加权，与 DSH 面板同口径。
	//
	// 四个字段都 omitempty：窗口内 0 条流式样本时**字段缺席**，
	// 面板据此显示 "—"，而不是把「没数据」渲染成 0 t/s。
	ThroughputTPS float64 `json:"throughput_tps,omitempty"` // Σ完成token ÷ Σ生成秒
	StreamTpsN    int64   `json:"stream_tps_n,omitempty"`   // 吞吐样本数
}

type TokenStatsData struct {
	TotalRequests int64                  `json:"total_requests"`
	Models        map[string]*ModelStats `json:"models"`

	// 全局吞吐。与 per-model 同源同口径：Σ分子 ÷ Σ分母，
	// **不是**各模型吞吐的平均（那会把秒数小的模型和秒数大的模型等权）。
	ThroughputTPS float64 `json:"throughput_tps,omitempty"`
	StreamTpsN    int64   `json:"stream_tps_n,omitempty"`
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

// ======================== 配置热加载 watcher（task-55） ========================
//
// 判据以**内容指纹（SHA-256）**为准，不看 mtime。
//
// 原实现按 mtime 判定：watcher 应用新配置后调 saveConfig() 把配置规范化写回 ⇒ 文件
// mtime 必然变 ⇒ 下一次轮询又满足「mtime 变大」⇒ 每 5 秒无限循环。生产 10-09~10-20
// 实测 67,763 条 `config hot-reloaded`（6.7 MB，占运行日志 9.1%），且 config.json 每
// 5 秒被重写一次。改成内容指纹后：
//   - 用户真实改内容         ⇒ 恰好 1 条 reload；
//   - watcher 自己写回        ⇒ 指纹与「自己写下去的字节」相同 ⇒ 0 条；
//   - 只 touch mtime/内容不变 ⇒ 0 条（没有可加载的东西）。
//
// 为什么不用「自写抑制时间窗」（task-55 候选 C）：时间窗会漏掉用户在同一窗口内的真实
// 修改 —— 那正是热加载要解决的场景，属于用一个缺陷换另一个缺陷。

// configWatcherState 记录上一次已应用/已写盘的配置内容指纹。
type configWatcherState struct {
	lastHash [sha256.Size]byte
	seeded   bool
}

// newConfigWatcherState 用当前磁盘内容做种：启动时已存在的配置不需要「热加载」。
func newConfigWatcherState(path string) *configWatcherState {
	st := &configWatcherState{}
	if raw, err := os.ReadFile(path); err == nil {
		st.lastHash = sha256.Sum256(raw)
		st.seeded = true
	}
	return st
}

// configReloadOnce 检查一次配置文件是否被「外部」修改；有则应用并返回 (cfg, true)。
// 解析失败保留旧配置，仅打 warn 日志；同一次坏编辑只警告一次（记下坏内容的指纹）。
func configReloadOnce(st *configWatcherState, path string) (AppConfig, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		// 文件暂时不存在/读不到：保持旧行为（静默跳过，等下次轮询）。
		return AppConfig{}, false
	}
	sum := sha256.Sum256(raw)
	if st.seeded && sum == st.lastHash {
		return AppConfig{}, false
	}
	var cfg AppConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		slog.Warn("config watcher: invalid JSON, keeping old config", "error", err)
		st.lastHash, st.seeded = sum, true
		return AppConfig{}, false
	}
	applyConfig(cfg)
	// 保留原有行为：热加载后把配置规范化写回（缩进 / 补默认字段）。
	// ★ 关键：指纹要记「我们自己写下去的字节」，不是「读到的字节」——否则下次轮询读到
	// 的规范化内容与记录的原始内容不一致，会再触发一次 ⇒ 自触发循环只是从 mtime 换成 hash。
	if err := saveConfig(path, cfg); err != nil {
		slog.Warn("config watcher: save normalized config failed", "error", err)
		st.lastHash, st.seeded = sum, true
	} else if normalized, rerr := os.ReadFile(path); rerr == nil {
		st.lastHash, st.seeded = sha256.Sum256(normalized), true
	} else {
		st.lastHash, st.seeded = sum, true
	}
	return cfg, true
}

// configWatchInterval 是配置轮询间隔（测试可覆盖；生产固定 5s）。
var configWatchInterval = 5 * time.Second

// startConfigWatcher 后台轮询 config.json，检测到外部修改时自动加载并应用。
func startConfigWatcher() {
	go configWatchLoop(configPath, configWatchInterval, nil)
}

// configWatchLoop 是 watcher 主循环；stop 关闭后退出（生产传 nil = 随进程结束）。
func configWatchLoop(path string, interval time.Duration, stop <-chan struct{}) {
	st := newConfigWatcherState(path)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			cfg, ok := configReloadOnce(st, path)
			if !ok {
				continue
			}
			slog.Info("config hot-reloaded",
				"aliases", len(cfg.ModelAlias),
				"api_key_set", cfg.ApiKey != "",
			)
		}
	}
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
	// ⚠️ `ensureFreeModelsFresh` 是**启动后唯一的按需刷新触发器**
	// （isFreeModelFromDocs 里的那条），不能因为下面的重构而丢掉 ——
	// 丢了之后免费表会一直停在启动那一刻，新上线的免费模型永远判不出来。
	ensureFreeModelsFresh()
	freeModelDocsMu.RLock()
	docs := freeModelDocsCache
	freeModelDocsMu.RUnlock()
	return resolveModelWithDocs(model, docs)
}

// freeModelDocsStale 是「免费模型表是否需要刷新」的**单一判据**。
// 缓存为空，或距上次刷新超过 1 小时，即视为陈旧。
//
// 抽成独立函数而不是内联，是为了让 ensureFreeModelsFresh 与测试共用同一份
// 判据：测试若自己复刻一遍 `cache == nil || since > time.Hour`，日后调参就会
// 出现「实现改了、测试还在验旧口径」的假绿。
func freeModelDocsStale() bool {
	freeModelDocsMu.RLock()
	defer freeModelDocsMu.RUnlock()
	return freeModelDocsCache == nil || time.Since(freeModelDocsTime) > time.Hour
}

// ensureFreeModelsFresh 保持 isFreeModelFromDocs 既有的按需刷新语义：
// 缓存为空或超过 1 小时就异步刷一次。**只派发、不等待、不读取结果** ——
// 本次调用的判定仍用当前快照，与重构前逐字一致。
func ensureFreeModelsFresh() {
	if freeModelDocsStale() {
		go refreshFreeModelsDocs()
	}
}

// resolveModelWithDocs 是 resolveModel 的**纯函数内核**：所有外部输入都来自
// (model, docs) 两个参数 + configMu 保护下的别名表，**不刷新、不派发 goroutine**。
//
// 拆出来的理由：回填路径要对 20 万行老记录重算判定，那里**不能**触发后台刷新
// （会改写仓库里的 free_models_cache.json，制造测试间的顺序依赖，见
// isUnmappedModel 的注释）。两条路径共用这一个内核，避免「两份实现各自演化」
// 导致口径分叉。
func resolveModelWithDocs(model string, docs map[string]bool) string {
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
	if m != "" && !isFreeModelIn(docs, m) {
		freeID := m + "-free"
		if modelExistsInCaches(freeID) {
			return freeID
		}
	}
	return m
}

// ==================== 未映射模型判定（Task 21 公式 B + Task 39 everServed） ====================
//
// 判据（task-39，用户 2026-10-20 08:30 裁决，**取代**纯公式 B）：
//
//	mapped = everServed(name) || 公式B
//	everServed(name) = 历史上任何一条 status=ok 记录里出现过的模型名
//
// 语义（用户原话：「概览的图表啥的只显示有成功的模型，至于更详细的统计才记录全部请求」）：
//   - **有过成功** ⇒ 永远算我们的模型：历史成功永久保留，它下架后的新失败照常算失败；
//   - **从未成功过** ⇒ 概览（图表 / total_requests / 成功率分母）里完全不出现，
//     但详细层（call_log.jsonl / 日志页 / `/api/call-log`）一条不少。
//
// 为什么必须从「名字长得像」升级成「曾经成功过」：公式 B 靠 `-free` 后缀与
// 当前目录判定，而**不带 `-free` 后缀的裸名**（如 `big-pickle`）只靠目录表。
// 一旦上游把它从目录摘掉，公式 B 会把它的历史成功记录追认为「未映射」并从概览删除 ——
// 实测把 `big-pickle` 从目录里摘掉就会出现 11 条成功记录被逐出概览。
// everServed 把「曾经成功过」变成永久事实，堵住这条退化路径。
func isUnmappedModel(raw string) bool {
	m := strings.TrimSpace(raw)
	if m == "" {
		// 空模型名不是「未映射」——它压根不是一次模型请求。生产日志里
		// 模型名为空只出现在上游在解析请求体之前就拒掉的极端情况；
		// 把它算进 unmapped 会让这个字段失去「客户端传了个野模型名」的语义。
		return false
	}
	// Task 39：曾经成功过 ⇒ 永久 mapped（单调，只增不减）。
	// 放在最前面：它**不依赖目录**，所以冷启动、目录不可用时也成立 ——
	// 这正是它比公式 B 更可靠的地方。
	if isServedModel(m) {
		return false
	}
	return isUnmappedByFormula(m)
}

// ---- 以下为 task-21 的公式 B（「名字长得像不像我们的免费层模型」）----
//
// 单独拆出来的唯一理由是**回填要只用它**：老日志行（没有落盘 unmapped 键）的
// 最终裁决 = 公式 B 判未映射 && !everServed，而 everServed 必须等整轮扫完才齐
// （见 calllog.go 回填段的 stagedServed / pendingServed 注释）。
// 判定路径**不再直接调它**，只经由 isUnmappedModel。
//
// // 背景：`CallRecord.Model` 存的是**客户端原样传来的名字**，不是 resolveModel
// 的结果。于是 `mimo-v2.5`、`gpt-4o-mini` 这类名字自成一个模型键，进了
// `/api/stats` 的 total_requests 与 models，作为**失败**拉低成功率分母 ——
// 而它们从一开始就不可服务。生产实测（/tmp/prod-now.jsonl，204,237 行）：
// 这类请求 661 条、**0 条成功**，占全部请求 0.32%，让成功率虚低 0.30 个百分点。
//
// 判据（Lead 裁决的 B 方案）：
//
//	mapped = isFreeModel(resolveModel(raw)) || strings.HasSuffix(resolveModel(raw), "-free")
//
// **为什么不查上游目录**（这是被否决的 A 方案，反例实测成立）：
// `modelExistsInCaches` 查的是上游 `/v1/models` 全量目录（85 条 zen + 37 条 go），
// 它有两个致命性质：
//  1. 里面**绝大多数是付费模型** —— `claude-opus-4-5`、`claude-sonnet-*`、
//     `gemini-3*`、`grok-*`、`hy3`、`mimo-v2.5` 全在。用「在目录里」当
//     「可服务」，恰好把用户真正抱怨的那批（624 条 mimo-v2.5、4 条
//     claude-opus-4-5、2 条 hy3）判成 mapped 而继续污染。
//  2. 它会**漂移**：`deepseek-v4-flash-free`（4,565 条 / 97.2% 成功）与
//     `x-preview-f-free`（1,419 条 / 89.1% 成功）至今还在跑，却已被上游从
//     目录里摘掉。按今天的目录判历史数据，会把这 5,702 条**成功**记录
//     从统计里删掉，成功率反而下降 0.09 个百分点。
//
// 而 `-free` 是**我们自己的**免费层命名约定（defaultFreeModels 与
// free_models_cache.json 同步维护），比上游目录稳定得多。实测 B 的排除集
// 与「上游返回 http 401 = 模型不被认」这个独立口径高度吻合（真·模型未知
// 663 条 vs B 的 661 条），且**一条成功记录都不删**。
//
// **纯函数性**：本函数只读 `freeModelDocsCache`（+ 别名映射），不写任何全局、
// 不派发 goroutine、不碰网络、不落盘，且**绝不触发后台刷新** —— 这是它能在
// 回填路径对 20 万行老记录安全重算的前提（见 calllog.go 回填段的调用点）。
//
// ⚠️ 这里**刻意不调 `isFreeModel`**，而是走 `isFreeModelSnapshot`：后者除了
// 判 membership，还会在「缓存为空 / 超过 1 小时」时 `go refreshFreeModelsDocs()`。
// 那个副作用在请求路径上是既有的读侧行为、可以接受；但在回填路径上不行：
//   - 它会在测试期间**改写仓库里的 free_models_cache.json**（saveFreeModelsCache），
//     于是一个用例的判定会影响后面用例的判定，制造**顺序相关的假失败**；
//   - 20 万行 × 多次派发（虽已按模型名记忆化到 ~18 次）仍是不可控的并发写；
//   - 「同输入同输出」会被它破坏 —— 刷新完成后同一个模型名会给出不同答案。
//
// 刷新本身仍由主流程（main 启动 + 读侧首次调用）驱动，这里只是不做这件事。
func isUnmappedByFormula(raw string) bool {
	m := strings.TrimSpace(raw)
	if m == "" {
		// 空模型名不是「未映射」——它压根不是一次模型请求。生产日志里
		// 模型名为空只出现在上游在解析请求体之前就拒掉的极端情况；
		// 把它算进 unmapped 会让这个字段失去「客户端传了个野模型名」的语义。
		return false
	}
	// 安全闸门：文档免费表不可用时**一律判 mapped**（维持现状）。
	// 冷启动时 freeModelDocsCache 可能是 nil，此时任何判定都不可信；
	// 宁可漏排除（数字偏保守）也不能误排除（把真实模型从统计里删掉）。
	freeModelDocsMu.RLock()
	docs := freeModelDocsCache
	docsReady := len(docs) > 0
	freeModelDocsMu.RUnlock()
	if !docsReady {
		return false
	}

	resolved := resolveModelWithDocs(m, docs)
	// 判据必须与「本部署能服务什么」对齐，而不是「上游目录里有什么」。
	if isFreeModelIn(docs, resolved) {
		return false
	}
	// `-free` 后缀：与 defaultFreeModels / free_models_cache.json 的命名约定同源。
	// 这一条覆盖了「文档表已把该模型摘掉、但它仍是免费层模型」的漂移场景
	// （生产实例：deepseek-v4-flash-free、x-preview-f-free）。
	if strings.HasSuffix(resolved, "-free") {
		return false
	}
	return true
}

// isFreeModelIn 是 isFreeModel 的**无副作用、显式快照**版本：
// 只在给定快照里查 membership，不读全局、不刷新、不派发 goroutine。
//
// 快照为 nil/空时退回 defaultFreeModels —— 与 isFreeModelFromDocs 的兜底一致。
func isFreeModelIn(docs map[string]bool, modelID string) bool {
	if len(docs) == 0 {
		return defaultFreeModels[modelID]
	}
	return docs[modelID]
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
	// ★ 2026-10-26 修正：prompt_cache_retention 是旧 /zen/v1/ 路由的私有参数，
	//   v2 /inference 路由不认识它，直接 400 unsupported_parameter（要求
	//   prompt_cache_options）。官方 2.x 客户端也不发任何缓存参数，注入本身
	//   就是形态差异。因此：模型走 v2 免费层（isFreeModel 判定，对应
	//   /inference 路由）时不再注入；Go endpoint（zen/go 私有路由）保留。
	if retention := getPromptCacheRetention(); retention != "off" && !isFreeModel(req.Model) {
		if _, exists := converted["prompt_cache_retention"]; !exists {
			converted["prompt_cache_retention"] = retention
		}
	}
	// Anthropic 风格 cache_control 断点：对接受该字段的模型(排除 GLM/Zhipu)
	// 显式标记缓存断点并拉长 TTL。对不支持的上游，zen 网关负责剥离；
	// DeepSeek 等自动前缀缓存不受影响。
	// ★ 2026-10-26 同 prompt_cache_retention 的修正：v2 /inference 路由
	//   不剥离该参数，免费模型注入会 400。官方 2.x 客户端不发它。
	if getCacheBreakpoints() && !rejectsCacheControl(req.Model) && !isFreeModel(req.Model) {
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

// writeSSELine 写出一行 SSE。data 事件必须以空行终止，否则上游不发空行时
// 多个 data 事件会被客户端 join 成一个 JSON 解析失败。
func writeSSELine(w io.Writer, line string) {
	if strings.HasPrefix(line, "data:") {
		io.WriteString(w, strings.TrimRight(line, "\r\n"))
		io.WriteString(w, "\n\n")
		return
	}
	io.WriteString(w, line)
	if !strings.HasSuffix(line, "\n") {
		io.WriteString(w, "\n")
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
	return buildOCRequestWithEndpoint(modelID, bodyMap, auth, auth.shouldUseGoEndpoint(modelID), nodeFP, ocClientVersion())
}

func buildOCRequestWithEndpoint(modelID string, bodyMap map[string]any, auth UpstreamAuth, useGoEndpoint bool, nodeFP, ver string) (*http.Request, error) {
	bodyMap["model"] = modelID
	// v2 gate 对 body 做指纹比对：encoding/json 的 HTML 转义（\u003c 等）
	// 与 JSON.stringify 字节不同，免费层 body 必须无转义编码。
	// 付费/Go 层语义等价，统一走同一路径无害。
	tryBody, err := marshalBodyBytes(bodyMap)
	if err != nil {
		return nil, err
	}
	var upstreamURL string
	if useGoEndpoint {
		upstreamURL = "https://opencode.ai/zen/go/v1/chat/completions"
	} else {
		// 2026-10-25 起 v2 lane 用 2.1 客户端的 family 路由：/inference 后
		// 按协议族分组（openai/anthropic），Chat 是 /inference/openai/v1/...。
		// 旧的扁平路由 /inference/v1/... 仍在响应，但 2.1 客户端不再发它，
		// flat 路由现在是过时客户端特征（上游 v1.4.4 同步，commit f9ef5693）。
		upstreamURL = "https://opencode.ai/inference/openai/v1/chat/completions"
	}
	req, err := http.NewRequest("POST", upstreamURL, bytes.NewReader(tryBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth.authorizationHeader())
	// 版本由调用方传入（本请求自己携带），不再在函数内二次读全局：
	// 否则「attemptVer == 实际发出的版本」不是不变量，C1 的归因守卫会失效。
	//
	// 2026-10-25 起免费层（非 Go endpoint）走 v2 头形态：四段 UA、双 session
	// 头 + affinity、无 x-opencode-request（v2 gate 拒它）。Go endpoint 维持
	// 1.x 形态（zen/go lane 的 gate 未随 2.x 演进）。ver 参数在 v2 分支不用于
	// UA（v2UserAgent 内置 2.1.1），保留形参签名以最小化调用面改动。
	if useGoEndpoint {
		req.Header.Set("User-Agent", fmt.Sprintf("opencode/%s", ver))
		req.Header.Set("x-opencode-client", "cli")
		req.Header.Set("x-opencode-project", ocProjectID)
		req.Header.Set("x-opencode-session", getOrCreateSessionByNode(nodeFP))
		req.Header.Set("x-opencode-request", newOpenCodeID("msg"))
		req.Header.Set("Accept", "application/json")
	} else {
		applyV2Headers(req, getOrCreateSessionByNode(nodeFP), ocProjectID)
	}
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

// upstreamLoopBudget 单个请求在重试循环里最多花多久（墙钟），只用于在两次 attempt
// 之间决定「还要不要再试」——绝不打断在途响应。
// 为什么不用超时 ctx：非流式耗时包含模型生成时间，实测【成功】请求里 >75s 占 2.34%
// （历史口径 8.69%）、>120s 占 1.66%。套 ctx 会在生成中途把本来会成功的请求掐成失败
// （R2 独立验收 blocker C1），代价大于收益。
// 取值 120s：把「反复失败」的病态链收在 120s 量级。
// ⚠️ 它【不】给单次 attempt 设上界：dial 60s + ResponseHeaderTimeout 60s 之外，
// 整体耗时仍可能更长（实测有 >1200s 的失败样本）。
// 「发出去了却卡住不返回」由 C-2 的【空闲读超时】兜住（idle_read.go：每读到字节就重置，
// 连续 idleReadTimeout 无数据才中止）——它已覆盖两条路径的 body 读取，见下方
// `newIdleReadCloser` 的两个调用点（非流式 / 流式）。不用总时长是因为长生成是合法的。
// 流式路径同样只做墙钟检查（body 在函数返回后继续读，由空闲读超时保护）。
var upstreamLoopBudget = 120 * time.Second

// isFirstByteTimeout 判断这次 attempt 的失败是否发生在「已建连、等响应头 / 首 token」
// 阶段（Go 原生 Transport 与 BunTransport 的错误串都含 timeout awaiting response headers）。
//
// 这类超时不是出口不通：连接其实已经建立，只是首 token 慢（本上游的响应头几乎与首个
// data: 同时到达）。节点对 ttft 的 ICC=0.0%（05-latency.md §0），换节点救不回来，所以
// 【不能】把节点判死（T3 评审 F2）。
func isFirstByteTimeout(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "timeout awaiting response headers")
}

// noteAttemptTransportError 统一一次传输层失败的事件记账与节点记账，两个重试循环共用。
// 返回 firstByteTimeout：调用方据此决定是否仍需 mark(NodeDead) 以及 switch 事件的 reason。
func noteAttemptTransportError(ctx context.Context, pool *nodePool, nodeFp string, err error) (firstByteTimeout bool) {
	firstByteTimeout = isFirstByteTimeout(err)
	if firstByteTimeout {
		// 复用既有的 ttft_timeout 事件类型（面板文案「首字超时」、聚合归 timeout），
		// 不与 connect_error 混用 —— 后者在 node_scoring 里按节点连接失败 1.0 计权。
		callLogEvent(ctx, "ttft_timeout", nodeFp, err.Error())
		return true
	}
	callLogEvent(ctx, "connect_error", nodeFp, err.Error())
	// 连接错误（EOF、建连超时等）：标记节点为 dead，避免后续请求继续命中坏节点。
	// 无论是否还有重试预算都要标记，否则最后一次失败会让坏节点长期留在池中。
	// 池级 halt 期间不 mark（与 T2「halt 期间不 mark 任何节点」的硬约束一致）。
	if nodeFp != "" && !pool.poolHalted(time.Now()) {
		pool.mark(nodeFp, NodeDead, "connect_error:"+err.Error())
	}
	return false
}

func callOpenCodeAPI(ctx context.Context, upstreamBody []byte, modelID string, auth UpstreamAuth) ([]byte, int, http.Header, error) {
	// 配额熔断闸门：节点池无可用节点且直连出口已被上游 429 限流时，
	// 直接返回 429，不再访问上游（见 quota_halt.go）。
	if halted, haltBody := quotaHaltGuard.gate(auth); halted {
		return quotaHaltResponse(ctx, modelID, haltBody)
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

	// 【不要】用超时 ctx：非流式耗时含模型生成时间，套 ctx 会把「本来会成功」的请求
	// 在生成中途掐断（R2 验收 blocker C1：成功请求 >75s 占 2.34%、>120s 占 1.66%）。
	// 预算只以墙钟 break 形式生效（见循环内），绝不打断在途响应。
	// 仍然把调用方 ctx 挂到请求上（见下方 up.WithContext）：客户端断开可取消上游。
	loopCtx := ctx

	// 免费额度耗尽自动切节点：独立预算（默认 5 个节点），不占用重试闸门；
	// 循环上限 = 重试上限 + 配额预算，两者各自封顶互不挤占（重试仍由 canRetry 限制）。
	nodeSwitchPending := false
	trialSwitchPending := false // 上一次 shape 半开试探放行后，等待本次选到新出口
	quotaSwitches := 0
	versionRefreshed := false // 426 时只刷新一次版本，避免无谓循环
	versionReverted := false  // 403(版本过新) 时只回退一次版本
	// 403 FreeTierError（出口 IP 被上游拒）的有界换节点预算。只「换」不 mark：
	// mark 必须等到换节点后成功（confirm-then-mark，见 .research/T1-403-design-v2.md）。
	// pendingVer 与 pendingBadFp 成对记录，mark 前要求两者版本一致（C1 归因守卫）。
	pendingBadFp := ""
	pendingVer := ""
	freeTierSwitches := 0
	upstreamLoopStart := time.Now()
	maxQuotaSwitches := effectiveMaxQuotaNodeSwitches()
	loopBudget := maxAttempts + maxQuotaSwitches

	for attempt := 0; attempt < loopBudget; attempt++ {
		// T2：池级 403 halt 期间不再换节点、不再发新 attempt，fail-fast。
		if proxyPool.poolHalted(time.Now()) {
			fp := proxyPool.currentFp()
			wouldBe := EgressKeyDirect
			if fp != "" {
				wouldBe = nodeEgressKey(fp)
			}
			callLogSwitchEvent(ctx, fp, "pool_halt", "pool_halt", 0)
			callLogSuppressedAttempt(ctx, modelID, fp, "pool_halt", wouldBe, nil)
			return nil, http.StatusServiceUnavailable, nil, fmt.Errorf("proxy pool halted after 403 storm")
		}
		// 请求级总预算：到点就收手，不再叠加等待（砍掉 135s 的病态尾部）。
		if attempt > 0 && time.Since(upstreamLoopStart) >= upstreamLoopBudget {
			log.Warn("upstream_loop_budget_exhausted",
				"try_model", modelID, "surface", surface,
				"attempt_index", attempt,
				"elapsed_ms", time.Since(upstreamLoopStart).Milliseconds())
			break
		}
		blockedEgress := shapeDriftBlockedEgress(modelID, time.Now())
		egResult := egress.Get(EgressRequest{Auth: auth, ForceSwitch: nodeSwitchPending, BodyMap: bodyMap, RequiredRegion: requiredRegion, ExcludeEgress: blockedEgress})
		switchCandidates := proxyPool.lastSwitchCandidates()
		if proxyPool.poolHalted(time.Now()) {
			// 另一个请求可能在本次 egress.Get 期间刚触发 halt：不能再直连/发请求。
			callLogSwitchEvent(ctx, egResult.NodeFP, "pool_halt", "pool_halt", 0)
			callLogSuppressedAttempt(ctx, modelID, egResult.NodeFP, "pool_halt", egResult.EgressKey(), nil)
			return nil, http.StatusServiceUnavailable, nil, fmt.Errorf("proxy pool halted after 403 storm")
		}
		// 免费层（zen）请求经 BunTransport 发出：TLS 指纹是 v2 gate 的硬前提
		// （2026-10-25 对照实验，见 bun_transport.go）。Go endpoint 维持原传输。
		// 付费 zen key 走免费模型时同样需要（v2 gate 按 lane 而非按 key 校验）。
		if !useGoEndpoint {
			egResult = bunEgressResult(egResult)
		}
		if trialSwitchPending {
			shapeDriftSetTrialEgress(modelID, egResult.EgressKey(), time.Now())
			trialSwitchPending = false
		}
		if auth.tier() == TierFree && proxyPool.nodeCount() > 0 && egResult.Direct &&
			!proxyPool.hasEligibleNode() {
			// 节点全部在冷却/判死：本次尝试实际落到了直连，节点侧尝试没有发出。
			callLogSuppressedAttempt(ctx, modelID, "", "node_cooldown", EgressKeyDirect, nil)
		}
		if blockedEgress[egResult.EgressKey()] {
			// 选路把所有已知 429 出口都排除了；宁可 fail-fast 也不能烧新 IP。
			egResult.Invalidate()
			callLogEvent(ctx, "shape_drift", egResult.NodeFP, "all_egress_blocked")
			callLogSuppressedAttempt(ctx, modelID, egResult.NodeFP, "shape_drift", egResult.EgressKey(), sortedBlockedKeys(blockedEgress))
			return nil, http.StatusServiceUnavailable, nil, fmt.Errorf("shape drift: all egress candidates are in the 429 set")
		}
		client, nodeFp := egResult.Client, egResult.NodeFP
		if nodeFp != "" {
			proxyPool.noteAttempt(nodeFp, time.Now())
		}
		if nodeFp == "" && proxyPool.allSubsQuarantined(time.Now()) {
			callLogSwitchEvent(ctx, "", "all_subscriptions_quarantined", "sub_quarantine", 0)
			return nil, http.StatusServiceUnavailable, nil, fmt.Errorf("all subscriptions quarantined after 403")
		}
		// I2：本 attempt 实际发出的 UA 版本由本请求自己携带（builder 不再读全局），
		// 否则 C1 的「失败 attempt 与确认 attempt 版本相同」不是不变量。
		attemptVer := ocClientVersion()
		v5Seen := false
		v5Observe := func(status int, body []byte, err error) {
			if v5Seen {
				return
			}
			v5Seen = true
			v5ObserveUpstream(ctx, modelID, egResult, status, body, err, attemptVer)
		}
		up, err := buildOCRequestWithEndpoint(modelID, bodyMap, auth, useGoEndpoint, nodeFp, attemptVer)
		if err != nil {
			return nil, 500, nil, err
		}
		// 把上下文真正挂到上游请求上（builder 用的是 http.NewRequest，Context 是
		// Background）：预算超时与客户端断开取消靠这一行才会生效。
		up = up.WithContext(loopCtx)
		nodeSwitchPending = false
		// I6：并发下 pick 可能又把刚失败的节点选回来（force 只排除 activeID），
		// 静默空转会让「换了节点还是 403」事后无法解释，故显式记一条事件。
		if pendingBadFp != "" && nodeFp == pendingBadFp {
			callLogEvent(ctx, "freetier_switch_noop", nodeFp, "same_node_returned:concurrent_pick")
			log.Warn("freetier_switch_noop", "node", proxyPool.nameOf(nodeFp), "switches_done", freeTierSwitches)
		}
		attemptStart := time.Now()
		// 挂 httptrace 记录建连耗时（dial_ms）。不改动任何既有字段，只让本次连接
		// 的真实建连成本落进调用记录 —— 此前成功侧完全没有这个维度，只有失败侧
		// 的 connect_error，能据此观测的建连分布全是失败尾部。
		resp, err := client.Do(withDialTrace(ctx, up))
		if err == nil && resp != nil {
			// C-2：body 阶段套「空闲读超时」——连续 idleReadTimeout 没有字节就中止。
			// 不用总时长上限：长生成/长流式只要有数据在走就不该被误杀。
			resp.Body = newIdleReadCloser(resp.Body, idleReadTimeout)
		}
		// C-5：喂节点层告警样本。口径是【传输层】——拿到任何上游响应（含 403/429）
		// 都说明这条出口是通的；账号级限流不该记到节点头上。direct 出口不计。
		// 首字节超时（error string 含 timeout awaiting response headers）说明连接已建好，
		// 同样是「出口可达」，不能记成传输失败（T3 复审 N2）。
		nodeAlerts.recordNodeAttempt(nodeFp != "", err == nil || isFirstByteTimeout(err))
		durationMs := time.Since(attemptStart).Milliseconds()
		if err != nil {
			firstByteTimeout := noteAttemptTransportError(ctx, proxyPool, nodeFp, err)
			v5Observe(0, nil, err)
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
			// 首字节超时已在 noteAttemptTransportError 里跳过 mark（F2）；这里保持
			// 原有的重试/换节点行为不变。
			if canRetry {
				if nodeFp != "" {
					reason := "connect_error"
					if firstByteTimeout {
						reason = "ttft_timeout"
					}
					callLogSwitchEvent(ctx, nodeFp, reason+":"+err.Error(), "switch_after_fail", switchCandidates)
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
			// 真实 2xx 是 probation 的唯一证据：拿到即转 available（其他状态无副作用）。
			proxyPool.recordNodeSuccess(nodeFp)
			v5Observe(resp.StatusCode, nil, nil)
			// 该出口真的拿到 2xx ⇒ 喂给熔断闸门的恢复滞回（连续 2 次 + 间隔 ≥60s）。
			// 这是本版解闩权的唯一来源：不再靠节点池状态推断该出口是否恢复。
			quotaHaltGuard.observeSuccess(egResult.EgressKey())
			// 429 漂移守卫：任一出口真实 2xx 立即解除该 model 的闩（半开试探成功亦然）。
			shapeDriftObserveSuccess(modelID, egResult.EgressKey(), time.Now())
			b, readErr := readAllMarkingFirstData(resp.Body, ctx)
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
			// confirm-then-mark 的【确认侧】：只有「换了节点后成功」才 mark 原节点。
			// 失败路径永不到这里 ⇒ 形态回归时池子零损伤。版本必须与失败那次一致（C1）。
			if pendingBadFp != "" {
				if pendingVer != attemptVer {
					callLogEvent(ctx, "freetier_confirm_rejected_version_changed", pendingBadFp,
						fmt.Sprintf("failed_ver=%s confirm_ver=%s", pendingVer, attemptVer))
				} else if nodeFp != "" && nodeFp != pendingBadFp {
					// T2/R2：先判定 halt/shape_latch/pool_floor；被抑制则完全不计订阅 strike。
					why, allowed := proxyPool.freeTierConfirmAllowed(time.Now())
					if allowed {
						if proxyPool.markFreeTierRejected(pendingBadFp) {
							proxyPool.confirmSub403(pendingBadFp, time.Now())
							callLogEvent(ctx, "freetier_node_confirmed_bad", pendingBadFp,
								fmt.Sprintf("confirmed_ip_rejected failed_ver=%s confirm_ver=%s switches=%d", pendingVer, attemptVer, freeTierSwitches))
							log.Warn("freetier_node_confirmed_bad",
								"bad", proxyPool.nameOf(pendingBadFp), "ok", proxyPool.nameOf(nodeFp),
								"failed_ver", pendingVer, "confirm_ver", attemptVer)
						} else {
							why = "mark_failed"
							callLogEvent(ctx, "freetier_confirm_suppressed", pendingBadFp, "suppressed:"+why)
							log.Warn("freetier_confirm_suppressed", "node", proxyPool.nameOf(pendingBadFp), "reason", why)
						}
					} else {
						callLogEvent(ctx, "freetier_confirm_suppressed", pendingBadFp, "suppressed:"+why)
						log.Warn("freetier_confirm_suppressed", "node", proxyPool.nameOf(pendingBadFp), "reason", why)
					}
				}
			}
			return b, resp.StatusCode, resp.Header, nil
		}
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		logUpstreamError(ctx, modelID, resp.StatusCode, errBody)
		v5Observe(resp.StatusCode, errBody, nil)

		// T2：每次 FreeTier 403 都进池级窗口（未确认也计数，用于 halt）；达到阈值立即 fail-fast。
		if resp.StatusCode == http.StatusForbidden && isFreeTierErrorBody(errBody) && nodeFp != "" {
			proxyPool.note403(nodeFp, time.Now())
		}
		// T2 硬约束：halt 期间不 mark、不换节点；本请求若在飞行中撞上 halt，也立即失败。
		if proxyPool.poolHalted(time.Now()) {
			callLogSwitchEvent(ctx, nodeFp, "pool_halt", "pool_halt", switchCandidates)
			return nil, http.StatusServiceUnavailable, nil, fmt.Errorf("proxy pool halted after 403 storm")
		}

		// 426 UpgradeRequired：上游提高了免费层的版本门槛。刷新一次版本号后重试
		// （npm latest 通常已满足新门槛），每个请求最多刷新一次。见 oc_version.go。
		if resp.StatusCode == http.StatusUpgradeRequired && !versionRefreshed {
			versionRefreshed = true
			log.Warn("upgrade_required_refresh_version",
				"model", modelID,
				"current_version", ocClientVer,
				"min_required", minFreeTierOCVersion,
			)
			callLogSwitchEvent(ctx, nodeFp, "upgrade_required:refresh_version", "switch_after_fail", switchCandidates)
			refreshOCSession()
			continue
		}

		// 403 FreeTierError 且当前 UA 版本比已验证版本新：很可能只是上游闸门还没跟上
		// npm 的新版本（实测 1.19.0 被 403、1.18.33 通过）。回退到已验证版本重试一次，
		// 与上面的 426 升级自愈对称。见 oc_version.go。
		if resp.StatusCode == http.StatusForbidden && !versionReverted &&
			isFreeTierErrorBody(errBody) && revertToPinnedOCVersion() {
			versionReverted = true
			log.Warn("freetier_revert_version",
				"model", modelID,
				"to", pinnedOCVersion,
			)
			callLogSwitchEvent(ctx, nodeFp, "freetier:revert_version", "switch_after_fail", switchCandidates)
			continue
		}

		// 403 FreeTierError 且版本已是「已验证可接受」版本：版本不再是可疑变量，出口 IP 可能是。
		// 先换一个节点重试，但【不 mark】——mark 必须等到换节点后成功（下方成功路径）。
		// 形态回归（所有节点都 403）时一次都不会 mark，池子零损伤。
		// 与上面的版本回退天然互斥：回退块一旦命中就 continue，走不到这里。
		if resp.StatusCode == http.StatusForbidden && isFreeTierErrorBody(errBody) &&
			attemptVer == pinnedOCVersion && nodeFp != "" && freeTierSwitches < freeTierSwitchBudget {
			freeTierSwitches++
			// 只记【第一个】被拒节点：预算=1 时等价；将来若把预算提到 ≥2，
			// 这里不会静默覆盖前一个（升级预算需要改成列表，见 T7-I5）。
			if pendingBadFp == "" {
				pendingBadFp = nodeFp
				pendingVer = attemptVer
			}
			callLogSwitchEvent(ctx, nodeFp, "freetier:probe_alt_node", "switch_after_fail", switchCandidates)
			log.Warn("freetier_node_switch",
				"try_model", modelID, "surface", surface,
				"from", proxyPool.nameOf(nodeFp), "switches_done", freeTierSwitches,
				"version", attemptVer)
			client.CloseIdleConnections()
			// 节点池路径下 Invalidate() 是 no-op（egress.go 里它依赖 key/eg）；
			// 真正让下一次 pick 排除当前节点的是 nodeSwitchPending（ForceSwitch）。
			egResult.Invalidate()
			nodeSwitchPending = true
			continue
		}

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
			// 形态漂移守卫：FreeUsageLimitError 在多个不同出口短窗内同时出现，
			// 说明是【网关形态被拒】而不是【出口配额耗尽】——继续切节点只会把
			// 坏形态请求散布到更多 IP（每 IP 挨 N 次就当日封禁，实测 ~16 次量级）。
			// 确认漂移后本次请求按原错误返回，不再消耗出口。见 shape_drift.go。
			drift := shapeDriftObserve429(modelID, egResult.EgressKey(), quotaReason, time.Now())
			if drift.Confirmed || drift.Trial {
				reason := "confirmed"
				if drift.Trial {
					reason = "half_open_trial"
				}
				lastTrial := "none"
				if !drift.LastTrial.IsZero() {
					lastTrial = drift.LastTrial.Format(time.RFC3339)
				}
				detail := fmt.Sprintf("model=%s reason=%s armed=%t distinct=%d last_trial=%s",
					modelID, reason, drift.Armed, drift.Distinct, lastTrial)
				callLogEvent(ctx, "shape_drift", nodeFp, detail)
				log.Warn("shape_drift_detected",
					"try_model", modelID, "surface", surface,
					"reason", reason, "quota_reason", quotaReason, "status", resp.StatusCode,
					"armed", drift.Armed, "distinct", drift.Distinct, "last_trial", lastTrial,
					"detail", detail,
				)
			}
			if drift.Halt {
				// 不切节点、不 mark：漂移期 mark 会把健康出口误标 exhausted。
				callLogSuppressedAttempt(ctx, modelID, nodeFp, "shape_drift", egResult.EgressKey(), sortedBlockedKeys(drift.Blocked))
				lastBody = errBody
				lastStatus = resp.StatusCode
				lastHeader = resp.Header
				lastErr = fmt.Errorf("upstream error")
				break
			}
			if nodeFp != "" && quotaSwitches < maxQuotaSwitches {
				switchReason := "quota:" + quotaReason
				if drift.Trial {
					switchReason += ":half_open_trial"
				}
				callLogSwitchEvent(ctx, nodeFp, switchReason, "switch_quota", switchCandidates)
				if !drift.Trial {
					proxyPool.mark(nodeFp, NodeExhausted, "quota:"+quotaReason)
				} else {
					trialSwitchPending = true
				}
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
		// 立即停止重试并对该【具体出口】进入熔断（见 quota_halt.go）。
		if egResult.Direct && resp.StatusCode == http.StatusTooManyRequests && auth.tier() == TierFree {
			// 按出口分桶：桶身份来自 EgressResult（NodeFP/Direct），不是全局一个 bool。
			// 同时记一次失败 ⇒ 该出口的恢复滞回计数清零。
			egressKey := egResult.EgressKey()
			quotaHaltGuard.observeFailure(egressKey)
			if quotaHaltGuard.halt(egressKey, "direct_429", errBody) {
				log.Warn("quota_halt_entered",
					"egress", egressKey,
					"try_model", modelID,
					"surface", surface,
					"status", resp.StatusCode,
					"attempt_index", attempt,
					"node_pool_size", proxyPool.nodeCount(),
				)
				callLogEvent(ctx, "quota_halt", nodeFp, "enter: 直连出口 429，停止重试等待出口恢复")
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
		body, status, header, err := quotaHaltResponse(ctx, modelID, haltBody)
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

	// 流式路径【不能】套超时 ctx：body 在函数返回后由调用方继续读，超时 context 会在
	// 中途把流掐断。这里只做墙钟检查；但仍把调用方 ctx 挂上去，修复「上游请求的
	// Context 一直是 Background ⇒ 客户端断开也不会取消上游」这个既有问题。
	loopCtx := ctx

	// 免费额度耗尽自动切节点（流式：仅头部非 2xx 时可切；已吐字节不可重试）：
	// 独立预算（默认 5 个节点），不占用重试闸门；循环上限 = 重试上限 + 配额预算。
	nodeSwitchPending := false
	trialSwitchPending := false // 上一次 shape 半开试探放行后，等待本次选到新出口
	quotaSwitches := 0
	versionRefreshed := false // 426 时只刷新一次版本，避免无谓循环
	versionReverted := false  // 403(版本过新) 时只回退一次版本
	// 403 FreeTierError（出口 IP 被上游拒）的有界换节点预算。只「换」不 mark：
	// mark 必须等到换节点后成功（confirm-then-mark，见 .research/T1-403-design-v2.md）。
	// pendingVer 与 pendingBadFp 成对记录，mark 前要求两者版本一致（C1 归因守卫）。
	pendingBadFp := ""
	pendingVer := ""
	freeTierSwitches := 0
	upstreamLoopStart := time.Now()
	maxQuotaSwitches := effectiveMaxQuotaNodeSwitches()
	loopBudget := maxAttempts + maxQuotaSwitches

	for attempt := 0; attempt < loopBudget; attempt++ {
		// T2：池级 403 halt 期间不再换节点、不再发新 attempt，fail-fast。
		if proxyPool.poolHalted(time.Now()) {
			fp := proxyPool.currentFp()
			wouldBe := EgressKeyDirect
			if fp != "" {
				wouldBe = nodeEgressKey(fp)
			}
			callLogSwitchEvent(ctx, fp, "pool_halt", "pool_halt", 0)
			callLogSuppressedAttempt(ctx, modelID, fp, "pool_halt", wouldBe, nil)
			return nil, http.StatusServiceUnavailable, nil, fmt.Errorf("proxy pool halted after 403 storm")
		}
		// 请求级总预算：到点就收手，不再叠加等待（砍掉 135s 的病态尾部）。
		if attempt > 0 && time.Since(upstreamLoopStart) >= upstreamLoopBudget {
			log.Warn("upstream_loop_budget_exhausted",
				"try_model", modelID, "surface", surface,
				"attempt_index", attempt,
				"elapsed_ms", time.Since(upstreamLoopStart).Milliseconds())
			break
		}
		blockedEgress := shapeDriftBlockedEgress(modelID, time.Now())
		egResult := egress.Get(EgressRequest{Auth: auth, ForceSwitch: nodeSwitchPending, BodyMap: bodyMap, RequiredRegion: requiredRegion, ExcludeEgress: blockedEgress})
		switchCandidates := proxyPool.lastSwitchCandidates()
		if proxyPool.poolHalted(time.Now()) {
			// 另一个请求可能在本次 egress.Get 期间刚触发 halt：不能再直连/发请求。
			callLogSwitchEvent(ctx, egResult.NodeFP, "pool_halt", "pool_halt", 0)
			callLogSuppressedAttempt(ctx, modelID, egResult.NodeFP, "pool_halt", egResult.EgressKey(), nil)
			return nil, http.StatusServiceUnavailable, nil, fmt.Errorf("proxy pool halted after 403 storm")
		}
		// 免费层（zen）请求经 BunTransport 发出（同非流式，见上方注释）。
		if !useGoEndpoint {
			egResult = bunEgressResult(egResult)
		}
		if trialSwitchPending {
			shapeDriftSetTrialEgress(modelID, egResult.EgressKey(), time.Now())
			trialSwitchPending = false
		}
		if auth.tier() == TierFree && proxyPool.nodeCount() > 0 && egResult.Direct &&
			!proxyPool.hasEligibleNode() {
			// 节点全部在冷却/判死：本次尝试实际落到了直连，节点侧尝试没有发出。
			callLogSuppressedAttempt(ctx, modelID, "", "node_cooldown", EgressKeyDirect, nil)
		}
		if blockedEgress[egResult.EgressKey()] {
			// 选路把所有已知 429 出口都排除了；宁可 fail-fast 也不能烧新 IP。
			egResult.Invalidate()
			callLogEvent(ctx, "shape_drift", egResult.NodeFP, "all_egress_blocked")
			callLogSuppressedAttempt(ctx, modelID, egResult.NodeFP, "shape_drift", egResult.EgressKey(), sortedBlockedKeys(blockedEgress))
			return nil, http.StatusServiceUnavailable, nil, fmt.Errorf("shape drift: all egress candidates are in the 429 set")
		}
		client, nodeFp := egResult.Client, egResult.NodeFP
		if nodeFp != "" {
			proxyPool.noteAttempt(nodeFp, time.Now())
		}
		if nodeFp == "" && proxyPool.allSubsQuarantined(time.Now()) {
			callLogSwitchEvent(ctx, "", "all_subscriptions_quarantined", "sub_quarantine", 0)
			return nil, http.StatusServiceUnavailable, nil, fmt.Errorf("all subscriptions quarantined after 403")
		}
		// I2：同非流式——版本由本请求携带，见上方注释。
		attemptVer := ocClientVersion()
		v5Seen := false
		v5Observe := func(status int, body []byte, err error) {
			if v5Seen {
				return
			}
			v5Seen = true
			v5ObserveUpstream(ctx, modelID, egResult, status, body, err, attemptVer)
		}
		up, err := buildOCRequestWithEndpoint(modelID, bodyMap, auth, useGoEndpoint, nodeFp, attemptVer)
		if err != nil {
			return nil, 500, nil, err
		}
		// 把上下文真正挂到上游请求上（builder 用的是 http.NewRequest，Context 是
		// Background）：预算超时与客户端断开取消靠这一行才会生效。
		up = up.WithContext(loopCtx)
		nodeSwitchPending = false
		// I6：同非流式——换节点后可能又拿到同一个坏节点，必须可观测。
		if pendingBadFp != "" && nodeFp == pendingBadFp {
			callLogEvent(ctx, "freetier_switch_noop", nodeFp, "same_node_returned:concurrent_pick")
			log.Warn("freetier_switch_noop", "node", proxyPool.nameOf(nodeFp), "switches_done", freeTierSwitches)
		}
		attemptStart := time.Now()
		// 同上：流式路径也挂 dial trace，否则 dial_ms 只覆盖非流式一半流量。
		resp, err := client.Do(withDialTrace(ctx, up))
		if err == nil && resp != nil {
			// C-2：body 阶段套「空闲读超时」——连续 idleReadTimeout 没有字节就中止。
			// 不用总时长上限：长生成/长流式只要有数据在走就不该被误杀。
			resp.Body = newIdleReadCloser(resp.Body, idleReadTimeout)
		}
		// C-5：喂节点层告警样本。口径是【传输层】——拿到任何上游响应（含 403/429）
		// 都说明这条出口是通的；账号级限流不该记到节点头上。direct 出口不计。
		// 首字节超时（error string 含 timeout awaiting response headers）说明连接已建好，
		// 同样是「出口可达」，不能记成传输失败（T3 复审 N2）。
		nodeAlerts.recordNodeAttempt(nodeFp != "", err == nil || isFirstByteTimeout(err))
		durationMs := time.Since(attemptStart).Milliseconds()
		if err != nil {
			firstByteTimeout := noteAttemptTransportError(ctx, proxyPool, nodeFp, err)
			v5Observe(0, nil, err)
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
			// 首字节超时已在 noteAttemptTransportError 里跳过 mark（F2）；这里保持
			// 原有的重试/换节点行为不变。
			if canRetry {
				if nodeFp != "" {
					reason := "connect_error"
					if firstByteTimeout {
						reason = "ttft_timeout"
					}
					callLogSwitchEvent(ctx, nodeFp, reason+":"+err.Error(), "switch_after_fail", switchCandidates)
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
			// 真实 2xx 是 probation 的唯一证据：拿到即转 available（其他状态无副作用）。
			proxyPool.recordNodeSuccess(nodeFp)
			v5Observe(resp.StatusCode, nil, nil)
			// 同上：流式路径也必须喂恢复滞回，否则只有一半流量能积累恢复证据。
			quotaHaltGuard.observeSuccess(egResult.EgressKey())
			shapeDriftObserveSuccess(modelID, egResult.EgressKey(), time.Now())
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
			// 流式侧的「成功」是【响应头级】确认（body 一行未读；实测 connect_ok 里
			// 0.67% 最终仍失败）——这是 v2 明确接受并记录在案的取舍（§6.2），不是疏漏。
			if pendingBadFp != "" {
				if pendingVer != attemptVer {
					callLogEvent(ctx, "freetier_confirm_rejected_version_changed", pendingBadFp,
						fmt.Sprintf("failed_ver=%s confirm_ver=%s", pendingVer, attemptVer))
				} else if nodeFp != "" && nodeFp != pendingBadFp {
					// T2/R2：先判定 halt/shape_latch/pool_floor；被抑制则完全不计订阅 strike。
					why, allowed := proxyPool.freeTierConfirmAllowed(time.Now())
					if allowed {
						if proxyPool.markFreeTierRejected(pendingBadFp) {
							proxyPool.confirmSub403(pendingBadFp, time.Now())
							callLogEvent(ctx, "freetier_node_confirmed_bad", pendingBadFp,
								fmt.Sprintf("confirmed_ip_rejected failed_ver=%s confirm_ver=%s switches=%d", pendingVer, attemptVer, freeTierSwitches))
							log.Warn("freetier_node_confirmed_bad",
								"bad", proxyPool.nameOf(pendingBadFp), "ok", proxyPool.nameOf(nodeFp),
								"failed_ver", pendingVer, "confirm_ver", attemptVer)
						} else {
							why = "mark_failed"
							callLogEvent(ctx, "freetier_confirm_suppressed", pendingBadFp, "suppressed:"+why)
							log.Warn("freetier_confirm_suppressed", "node", proxyPool.nameOf(pendingBadFp), "reason", why)
						}
					} else {
						callLogEvent(ctx, "freetier_confirm_suppressed", pendingBadFp, "suppressed:"+why)
						log.Warn("freetier_confirm_suppressed", "node", proxyPool.nameOf(pendingBadFp), "reason", why)
					}
				}
			}
			return newToolNameRewriter(wrapRawSSE(resp.Body), toolMapping, injectedTools), resp.StatusCode, resp.Header, nil
		}
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		logUpstreamError(ctx, modelID, resp.StatusCode, errBody)
		v5Observe(resp.StatusCode, errBody, nil)

		// T2：每次 FreeTier 403 都进池级窗口（未确认也计数，用于 halt）；达到阈值立即 fail-fast。
		if resp.StatusCode == http.StatusForbidden && isFreeTierErrorBody(errBody) && nodeFp != "" {
			proxyPool.note403(nodeFp, time.Now())
		}
		// T2 硬约束：halt 期间不 mark、不换节点；本请求若在飞行中撞上 halt，也立即失败。
		if proxyPool.poolHalted(time.Now()) {
			callLogSwitchEvent(ctx, nodeFp, "pool_halt", "pool_halt", switchCandidates)
			return nil, http.StatusServiceUnavailable, nil, fmt.Errorf("proxy pool halted after 403 storm")
		}

		// 426 UpgradeRequired：上游提高了免费层的版本门槛。刷新一次版本号后重试
		// （npm latest 通常已满足新门槛），每个请求最多刷新一次。见 oc_version.go。
		if resp.StatusCode == http.StatusUpgradeRequired && !versionRefreshed {
			versionRefreshed = true
			log.Warn("upgrade_required_refresh_version",
				"model", modelID,
				"current_version", ocClientVer,
				"min_required", minFreeTierOCVersion,
			)
			callLogSwitchEvent(ctx, nodeFp, "upgrade_required:refresh_version", "switch_after_fail", switchCandidates)
			refreshOCSession()
			continue
		}

		// 403 FreeTierError 且当前 UA 版本比已验证版本新：很可能只是上游闸门还没跟上
		// npm 的新版本（实测 1.19.0 被 403、1.18.33 通过）。回退到已验证版本重试一次，
		// 与上面的 426 升级自愈对称。见 oc_version.go。
		if resp.StatusCode == http.StatusForbidden && !versionReverted &&
			isFreeTierErrorBody(errBody) && revertToPinnedOCVersion() {
			versionReverted = true
			log.Warn("freetier_revert_version",
				"model", modelID,
				"to", pinnedOCVersion,
			)
			callLogSwitchEvent(ctx, nodeFp, "freetier:revert_version", "switch_after_fail", switchCandidates)
			continue
		}

		// 403 FreeTierError 且版本已是「已验证可接受」版本：版本不再是可疑变量，出口 IP 可能是。
		// 先换一个节点重试，但【不 mark】——mark 必须等到换节点后成功（下方成功路径）。
		// 形态回归（所有节点都 403）时一次都不会 mark，池子零损伤。
		// 与上面的版本回退天然互斥：回退块一旦命中就 continue，走不到这里。
		if resp.StatusCode == http.StatusForbidden && isFreeTierErrorBody(errBody) &&
			attemptVer == pinnedOCVersion && nodeFp != "" && freeTierSwitches < freeTierSwitchBudget {
			freeTierSwitches++
			// 只记【第一个】被拒节点：预算=1 时等价；将来若把预算提到 ≥2，
			// 这里不会静默覆盖前一个（升级预算需要改成列表，见 T7-I5）。
			if pendingBadFp == "" {
				pendingBadFp = nodeFp
				pendingVer = attemptVer
			}
			callLogSwitchEvent(ctx, nodeFp, "freetier:probe_alt_node", "switch_after_fail", switchCandidates)
			log.Warn("freetier_node_switch",
				"try_model", modelID, "surface", surface,
				"from", proxyPool.nameOf(nodeFp), "switches_done", freeTierSwitches,
				"version", attemptVer)
			client.CloseIdleConnections()
			// 节点池路径下 Invalidate() 是 no-op（egress.go 里它依赖 key/eg）；
			// 真正让下一次 pick 排除当前节点的是 nodeSwitchPending（ForceSwitch）。
			egResult.Invalidate()
			nodeSwitchPending = true
			continue
		}

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
			// 形态漂移守卫（同非流式，见 shape_drift.go）：多出口同窗 429
			// 一致出现 → 停配额切换，按原错误返回，不烧更多出口。
			drift := shapeDriftObserve429(modelID, egResult.EgressKey(), quotaReason, time.Now())
			if drift.Confirmed || drift.Trial {
				reason := "confirmed"
				if drift.Trial {
					reason = "half_open_trial"
				}
				lastTrial := "none"
				if !drift.LastTrial.IsZero() {
					lastTrial = drift.LastTrial.Format(time.RFC3339)
				}
				detail := fmt.Sprintf("model=%s reason=%s armed=%t distinct=%d last_trial=%s",
					modelID, reason, drift.Armed, drift.Distinct, lastTrial)
				callLogEvent(ctx, "shape_drift", nodeFp, detail)
				log.Warn("shape_drift_detected",
					"try_model", modelID, "surface", surface,
					"reason", reason, "quota_reason", quotaReason, "status", resp.StatusCode,
					"armed", drift.Armed, "distinct", drift.Distinct, "last_trial", lastTrial,
					"detail", detail,
				)
			}
			if drift.Halt {
				// 不切节点、不 mark：漂移期 mark 会把健康出口误标 exhausted。
				callLogSuppressedAttempt(ctx, modelID, nodeFp, "shape_drift", egResult.EgressKey(), sortedBlockedKeys(drift.Blocked))
				lastBody = errBody
				lastStatus = resp.StatusCode
				lastHeader = resp.Header
				break
			}
			if nodeFp != "" && quotaSwitches < maxQuotaSwitches {
				switchReason := "quota:" + quotaReason
				if drift.Trial {
					switchReason += ":half_open_trial"
				}
				callLogSwitchEvent(ctx, nodeFp, switchReason, "switch_quota", switchCandidates)
				if !drift.Trial {
					proxyPool.mark(nodeFp, NodeExhausted, "quota:"+quotaReason)
				} else {
					trialSwitchPending = true
				}
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
		// 立即停止重试并对该【具体出口】进入熔断（见 quota_halt.go）。
		if egResult.Direct && resp.StatusCode == http.StatusTooManyRequests && auth.tier() == TierFree {
			// 按出口分桶：桶身份来自 EgressResult（NodeFP/Direct），不是全局一个 bool。
			// 同时记一次失败 ⇒ 该出口的恢复滞回计数清零。
			egressKey := egResult.EgressKey()
			quotaHaltGuard.observeFailure(egressKey)
			if quotaHaltGuard.halt(egressKey, "direct_429", errBody) {
				log.Warn("quota_halt_entered",
					"egress", egressKey,
					"try_model", modelID,
					"surface", surface,
					"status", resp.StatusCode,
					"attempt_index", attempt,
					"node_pool_size", proxyPool.nodeCount(),
				)
				callLogEvent(ctx, "quota_halt", nodeFp, "enter: 直连出口 429，停止重试等待出口恢复")
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
				writeSSELine(w, out)
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
				if limit.InputLimit > 0 {
					il := limit.InputLimit
					visibleModel.InputLimit = &il
				}
			} else if limit, ok := lookupModelLimit(publicFacingModelID(model.ID)); ok {
				ctx, out := limit.Context, limit.Output
				visibleModel.ContextWindow, visibleModel.MaxOutputTokens = &ctx, &out
				visibleModel.InputModalities = limit.InputModalities
				if limit.InputLimit > 0 {
					il := limit.InputLimit
					visibleModel.InputLimit = &il
				}
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
				if limit.InputLimit > 0 {
					il := limit.InputLimit
					m.InputLimit = &il
				}
			} else if limit, ok := lookupModelLimit(publicFacingModelID(m.ID)); ok {
				ctx, out := limit.Context, limit.Output
				m.ContextWindow, m.MaxOutputTokens = &ctx, &out
				m.InputModalities = limit.InputModalities
				if limit.InputLimit > 0 {
					il := limit.InputLimit
					m.InputLimit = &il
				}
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
	// 附带免费模型列表（从文档缓存），供前端生成完整自动映射。
	// ★ 必须与本次返回的目录（seen）求交：文档源是「上游意图」，目录才是
	//   「上游事实」，两者刷新节奏与兜底都不同，文档里会残留目录已下线的模型
	//   （2026-10-15 实测 15 条 free_models vs 目录内只有 10 条）。前端
	//   「模型映射」直接展开 free_models 生成自动映射行、不做求交，不过滤就会
	//   多出永远不生效的映射行——用户看到映射列表比模型列表还长。
	freeModelDocsMu.RLock()
	freeIDs := make([]string, 0, len(freeModelDocsCache))
	for id := range freeModelDocsCache {
		if _, inCatalog := seen[id]; !inCatalog {
			continue
		}
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
	Name          string  `json:"name"`
	Protocol      string  `json:"protocol"`
	Address       string  `json:"address"`
	Port          int     `json:"port"`
	Fingerprint   string  `json:"fingerprint"`
	State         string  `json:"state"`
	MarkedAt      string  `json:"marked_at,omitempty"`
	CooldownUntil *string `json:"cooldown_until"` // null = 无冷却（裁定 5：零值后端归一化）
	LastError     *string `json:"last_error"`     // null = 无错误（task-67 契约：string|null）
	LastUsedAt    string  `json:"last_used_at,omitempty"`
	LatencyMs     int64   `json:"latency_ms,omitempty"`
	LastProbeAt   *string `json:"last_probe_at"` // null = 从未探测（裁定 5：零值后端归一化）
	Active        bool    `json:"active"`
	Manual        bool    `json:"manual"`

	// task-67：打分字段（字段名是前端契约，见 docs/research/node-scoring-spec.md §5.1）
	nodeScoreView
}

func adminNodesHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		pick, manual := proxyPool.pickState()
		nodes := proxyPool.snapshot()
		// task-67：打分（纯内存 O(节点数)，219 节点 <1ms；不需要缓存层）
		scoringNow := time.Now()
		_, scoreByFP := nodeScoringSnapshot(nodes, scoringNow)
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
				Active: n.Fingerprint == pick,
				Manual: n.Fingerprint == manual, LatencyMs: n.LatencyMs,
				nodeScoreView: scoreByFP[n.Fingerprint],
			}
			if n.LastError != "" {
				e := n.LastError
				v.LastError = &e
			}
			health[v.State]++
			if !n.MarkedAt.IsZero() {
				v.MarkedAt = n.MarkedAt.Format(time.RFC3339)
			}
			if !n.CooldownUntil.IsZero() {
				t := n.CooldownUntil.Format(time.RFC3339)
				v.CooldownUntil = &t
			}
			if !n.LastUsedAt.IsZero() {
				v.LastUsedAt = n.LastUsedAt.Format(time.RFC3339)
			}
			if !n.LastProbeAt.IsZero() {
				t := n.LastProbeAt.Format(time.RFC3339)
				v.LastProbeAt = &t
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
			"node_alert":      nodeAlerts.snapshot(),
			// task-67：打分快照元信息（前端显示「分数更新于 X」）
			"computed_at": scoringNow.Format(time.RFC3339),
			"scoring": map[string]any{
				"version": scoringVersion,
				"params":  scoringParamsView(defaultScoringParams),
			},
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

// statsRangeResponse 是 GET /api/stats?range=X 的响应体。
// 内嵌 TokenStatsData 使 total_requests / models 两个字段与不带 range 的响应
// **同名同层**，前端既有取值代码不需要分叉；额外的 trends_by_model 让前端
// 一次请求就同时拿到模型聚合与按模型的趋势序列（省掉单独调 /api/stats/trends）。
type statsRangeResponse struct {
	TokenStatsData
	TrendsByModel []ModelTrendPoint `json:"trends_by_model"`
	// UnmappedRequests 是**窗口内**未映射请求的计数（Task 21）。
	//
	// 为什么是对象而不是裸数字：旧后端没有这个字段，面板要能区分
	// 「这条后端不支持该指标」与「窗口内确实是 0 条」，从而显示 "—" 而不是 0。
	// 对象 + 明确的 count 键让前端一次判空即可（`if(!d.unmapped_requests)`）。
	//
	// 为什么**只加在 range 路径**、不加在 TokenStatsData 上：
	// TokenStatsData 同时也是**不带 range**那条分支的响应体，而那条分支有一条
	// 逐字节向后兼容红线（TestAdminStatsHandlerWithoutRangeIsByteIdentical 把
	// 空响应钉成 `{"total_requests":0,"models":{}}`）。把它加进 TokenStatsData
	// 会让那个 JSON 多出一个键，直接违约。而这个指标的语义本来就是**窗口内**的
	// （它由小时桶折叠而来），放在带 range 的响应里才是自洽的。
	UnmappedRequests UnmappedRequestsStats `json:"unmapped_requests"`

	// V5 是 W1 影子接线字段：只在 range 响应上出现，off 时为 nil 并被 omitempty 隐藏。
	V5 *V5Stats `json:"v5,omitempty"`
}

// UnmappedRequestsStats 是 /api/stats?range= 里 unmapped_requests 的形状。
//
// Count 只数「窗口内、被判定为未映射」的请求条数；它们**不在**同层
// total_requests / models 里 —— 这正是本次修复的目的（口径更正）。
// ByModel 是 Top N + "__other__"，见 topUnmappedModels。
type UnmappedRequestsStats struct {
	Count   int64            `json:"count"`
	ByModel map[string]int64 `json:"by_model"`
}

// statsRangeResponseFor 组装 GET /api/stats?range=X 的响应。
//
// 🔴 存在的唯一理由是**只取一次窗口**：models 与 trends_by_model 是同一份
// 响应里的两个字段，若各自调 trendBuckets（内部取 time.Now()），两次取窗口之间
// 跨过整点/午夜就会让两者落在不同窗口——面板上「合计」与「分项之和」互相矛盾，
// 而且不报错。实测（把时钟每次调用推进 2 小时）：同一响应里
// Σmodels=24 而 Σtrends_by_model=22，差 2。
//
// 所以这里只调**一次** trendBuckets，把同一份 ts/bidx 喂给两个内核。
// ensureBucketsUpToDate 也只在这里调一次（幂等，少调一次不改变语义）；
// 绝不能把它删掉——冷启动时桶是空的，删了会返回
// {"models":{},"total_requests":N}：数字在、模型全空、不报错、面板一片空白。
//
// ⚠️ 这里共享的是**窗口**而不是**快照**：两个内核各自取 RLock 读桶，
// 中间落进一次 callLog.append 仍会让两半对不上。要根治需单快照折叠
// （一次 RLock 内把桶拷出来、两个折叠都从副本算）——那是延后决策，不在本次范围。
func statsRangeResponseFor(rng string) statsRangeResponse {
	// ⚠️ 必须在这里、且在读桶【之前】调用。理由见上方注释。
	ensureBucketsUpToDate()
	// 窗口外的记录一律不计；rng 归一规则与 adminTrendsHandler 一致
	rng = normalizeTrendRange(rng)
	ts, bidx := trendBuckets(rng)

	out := statsRangeResponse{
		TokenStatsData: *modelStatsWithBuckets(ts, bidx),
		TrendsByModel:  trendsByModelWithBuckets(ts, bidx),
	}
	if out.TrendsByModel == nil {
		out.TrendsByModel = []ModelTrendPoint{}
	}
	// 未映射统计共用**同一份** ts/bidx —— 与 models / total_requests 同窗口。
	// 传进去而不是在里面再取一次窗口，理由与 trends_by_model 完全相同：
	// 两次取窗口之间跨过整点/午夜，会让 unmapped_requests 与 total_requests
	// 落在不同窗口，「合计」与「分项」互相矛盾且不报错。
	uc, ub := unmappedStatsInWindow(ts, bidx)
	out.UnmappedRequests = UnmappedRequestsStats{
		Count:   uc,
		ByModel: topUnmappedModels(ub, unmappedTopN),
	}
	out.V5 = v5StatsForResponse()
	// ---- F1（Task 39）：展示层只输出「窗口内有成功」的模型 ----
	//
	// 用户口径：「概览的图表啥的只显示有成功的模型」。判据放在**后端出口**，
	// 面板零改动（面板正在被 U3 大改，不能两个人同时改同一个文件）。
	//
	// 🔴 **只筛展示字段**：models 与 trends_by_model。
	// 绝不动 total_requests / 成功率分母 —— 窗口内 0 成功的那几条失败是
	// 真实免费模型的真实失败（生产实测 6 个模型 / 15 条 / 0 token），
	// 从分母里拿掉就是藏失败。所以本函数是「删展示键」，不是「改统计」。
	//
	// 副作用（已知且用户已选）：`Σ models.request_count` 会比 total_requests
	// 少这些条数（生产 15/233,045 = 0.0064%）。要解释「为什么少」请看
	// docs/research/overview-vs-detail-served-models.md。
	//
	// 详细层（call_log.jsonl / /api/logs / /api/call-log）一条不少。
	keep := modelsWithOKInWindow(ts, bidx)
	for m := range out.Models {
		if _, ok := keep[m]; !ok {
			delete(out.Models, m)
		}
	}
	out.TrendsByModel = filterTrendsByOK(out.TrendsByModel, keep)
	return out
}

// adminStatsHandler 令牌用量统计。
// GET 不带 range 时逐字返回内存累计计数器（向后兼容红线，勿改 json.Marshal 那一段）；
// 带 ?range=today|7d|30d|180d 时改由调用日志按窗口重算，数字随时间窗变化。
func adminStatsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if rng := r.URL.Query().Get("range"); rng != "" {
			out := statsRangeResponseFor(rng)
			data, err := json.Marshal(out)
			if err != nil {
				http.Error(w, `{"error":"marshal error"}`, http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}
		// ⚠️ 无 range 分支**有意不含** throughput_tps / stream_* 三个新字段：
		//  1. 面板对这条路径是**零读**——admin.html 只有 `/api/stats?range=` 的 GET
		//     和同一个 URL 的 DELETE，没有不带 range 的 GET；
		//  2. recordModelSpeed 的签名里**没有分子**（只有 ttftMs 和 outputSpeed），
		//     要出吞吐必须改签名 + 把每条请求的生成秒也喂进去；
		//  3. 它的数据要落进 stats.json，加字段就是**持久化 schema 变更**，
		//     而本批的承诺是「零 schema 变更」（call_log.jsonl 一个字节都不动）。
		// 要吞吐请用 `/api/stats?range=`。
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

// adminAggregateHandler 是 GET /api/logs/aggregate 的入口（task-67）。
// 方法/参数校验后交给 aggregateResponseFor（带 TTL 缓存 + singleflight）。
func adminAggregateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	out := aggregateResponseFor(r.URL.Query().Get("range"))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// adminTrendsHandler 用量趋势：按 range=today|7d|30d|180d 返回聚簇时间序列（来自调用日志）。
// group=model 时额外按模型分组，供前端做模型维度筛选；不带该参数时行为与原先完全一致。
func adminTrendsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 归一规则收敛到 normalizeTrendRange（calllog.go），与 /api/stats?range=
	// 共用同一处判定，避免两处日后各改各的导致窗口边界分叉
	rng := normalizeTrendRange(r.URL.Query().Get("range"))
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Query().Get("group") == "model" {
		// Task 39 / F1（Lead 追加裁决）：本出口必须与 `/api/stats?range=` 同一口径 ——
		// 窗口内 0 成功的模型在两边都要消失，且共用 filterTrendsByOK / modelsWithOKInWindow，
		// 不许各写一份判据（否则就是「只删 models 漏 trends」的半实现）。
		//
		// 注意必须与 trendsByModelWithBuckets **同一份** ts/bidx：所以这里内联
		// ensureBucketsUpToDate + trendBuckets，而不是调 trendsByModelFromCallLog
		// 让它自己再取一次 time.Now()（跨整点/午夜会让两边窗口分叉）。
		ensureBucketsUpToDate()
		ts, bidx := trendBuckets(rng)
		keep := modelsWithOKInWindow(ts, bidx)
		pts := filterTrendsByOK(trendsByModelWithBuckets(ts, bidx), keep)
		if pts == nil {
			pts = []ModelTrendPoint{}
		}
		json.NewEncoder(w).Encode(pts)
		return
	}
	// ---- 非模型维度分支（面板热力图：GET /api/stats/trends?range=180d）----
	// 返回的是**日总量**（[]TrendsPoint，没有 model 键），所以 F1 的「0 成功模型
	// 从展示层消失」在这里无对象可删；**必须逐字节不变** —— 把它按模型筛掉
	// 等于把那 15 条真实失败从日总量里藏掉，那是藏失败，明令禁止。
	pts := trendsFromCallLog(rng)
	if pts == nil {
		pts = []TrendsPoint{}
	}
	json.NewEncoder(w).Encode(pts)
}

// adminPageHandler 下发内嵌面板。内容协商、Vary 与 Content-Length 都在
// writeCompressedHTML 里（见 webcompress.go）；这里只负责取预压缩体。
func adminPageHandler(w http.ResponseWriter, r *http.Request) {
	writeCompressedHTML(w, r, adminPagePayload())
}

// renderLoginPage 下发登录页（可带一条错误消息）。msg 的取值集合很小，
// 每个变体在首次请求时预压缩一次并缓存（见 webcompress.go）。
func renderLoginPage(w http.ResponseWriter, r *http.Request, msg string) {
	writeCompressedHTML(w, r, loginPagePayload(msg))
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
	flag.StringVar(&callLogLayout, "calllog-layout", "daily",
		"调用日志布局：daily=按天分文件（默认），single=单文件（回滚用）")
	flag.IntVar(&callLogGzipAfterDays, "calllog-gzip-after-days", 2,
		"调用日志日文件超过该天数后 gzip 归档（0=不归档）")
	flag.IntVar(&callLogRetainDays, "calllog-retain-days", 180,
		"调用日志日文件保留天数；legacy 单文件不受影响（0=不删除）")
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
	// task-67：节点打分（加载 scoring_state.json + 回填历史调用日志 + 后台轮询）
	initNodeScoring(configPath)
	// 调用日志保留策略（gzip 归档 + 按天删除），仅 daily 模式；每小时一次。
	startCallLogArchiver()

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

	// 面板 HTML 的 gzip 在启动期一次性做完：之后每个请求零压缩 CPU。
	prewarmWebCompression()

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
	mux.HandleFunc("/api/logs/aggregate", loggingMiddleware(requireAuth(adminAggregateHandler)))
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

// ---- 实验副本专用：非流式路径的首 token 打点（逐行读）----
//
// 上游对非流式客户端【本来就是 stream=true】（ensureAgentUpstreamShape 强制），
// 响应随后由 aggregateUpstreamSSE 本地聚合。但聚合器收的是**已读完整的 body**
// 再 Split，首 token 的时刻在那之前就过去了。本函数把读取改成逐行，
// 在读到第一行 `data: ` 时调用**与流式路径完全相同**的 callLogFirstToken，
// 从而让非流式请求也能算出与流式同口径的 decode 速度。
//
// 字节等价性：逐块 append 的结果与 io.ReadAll 逐字节相同（含无换行的尾块），
// 所以 aggregateUpstreamSSE 的输入不变、其行为不变。
func readAllMarkingFirstData(r io.Reader, ctx context.Context) ([]byte, error) {
	br := bufio.NewReader(r)
	var out []byte
	marked := false
	for {
		chunk, rerr := br.ReadBytes('\n')
		if len(chunk) > 0 {
			if !marked {
				line := chunk
				if n := len(line); n > 0 && line[n-1] == '\n' {
					line = line[:n-1]
				}
				// 与 main.go:3263 的流式判据同构：任意 `data: ` 行（[DONE] 已在其前 continue）。
				if len(line) > len("data: ") && string(line[:len("data: ")]) == "data: " &&
					string(bytes.TrimSpace(line[len("data: "):])) != "[DONE]" {
					marked = true
					callLogFirstToken(ctx)
				}
			}
			out = append(out, chunk...)
		}
		if rerr != nil {
			if rerr == io.EOF {
				return out, nil
			}
			return out, rerr
		}
	}
}
