package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ======================= 配置形态 =======================

// WebshareConfig 一条 webshare.io 代理池源配置（config.json webshare[]）。
// 通过 Webshare API v2 拉取代理列表并转为 SOCKS5 节点并入节点池。
type WebshareConfig struct {
	Name                string `json:"name"`
	APIKey              string `json:"api_key"`
	Mode                string `json:"mode,omitempty"`                  // direct（默认）| backbone
	UpdateIntervalHours int    `json:"update_interval_hours,omitempty"` // 0 → 默认 24h
}

const webshareDefaultInterval = 24 * time.Hour

func (w WebshareConfig) updateInterval() time.Duration {
	if w.UpdateIntervalHours > 0 {
		return time.Duration(w.UpdateIntervalHours) * time.Hour
	}
	return webshareDefaultInterval
}

// Key 用于更新间隔与状态的持久化键（同名源视为同一状态）。
func (w WebshareConfig) Key() string {
	return "webshare:" + w.Name
}

func (w WebshareConfig) mode() string {
	m := strings.ToLower(strings.TrimSpace(w.Mode))
	if m == "" || m == "backbone" {
		return "direct" // backbone 仅部分套餐可用，默认 direct 最稳
	}
	return m
}

// ======================= API 拉取 =======================

// webshareListURL 是 Webshare 代理列表 API 地址（测试可覆盖）。
var webshareListURL = "https://proxy.webshare.io/api/v2/proxy/list/"

// webshareProxyDTO 是 /proxy/list/ 单条代理的响应形态。
// port 在 API 文档中既有字符串又有数字，用 any 兼容两种。
type webshareProxyDTO struct {
	ID           any    `json:"id"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	ProxyAddress string `json:"proxy_address"`
	Port         any    `json:"port"`
	Valid        *bool  `json:"valid"`
	CountryCode  string `json:"country_code"`
	CityName     string `json:"city_name"`
}

type webshareListResponse struct {
	Count   int                `json:"count"`
	Next    string             `json:"next"`
	Results []webshareProxyDTO `json:"results"`
}

// fetchWebshare 分页拉取 Webshare 代理列表并转为 SOCKS5 节点。
// 只保留 valid != false 的代理；非 200 或解析失败返回错误。
func fetchWebshare(ctx context.Context, w WebshareConfig) ([]*ProxyNode, error) {
	var nodes []*ProxyNode
	seen := map[string]bool{}
	page := 1
	for {
		u, err := url.Parse(webshareListURL)
		if err != nil {
			return nil, fmt.Errorf("webshare url: %w", err)
		}
		q := u.Query()
		q.Set("mode", w.mode())
		q.Set("page_size", "100")
		q.Set("page", strconv.Itoa(page))
		u.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Token "+w.APIKey)
		req.Header.Set("User-Agent", "opencode2api/"+versionString()+" (webshare source)")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
		resp.Body.Close()
		if rerr != nil {
			return nil, rerr
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("webshare API HTTP %d: %s", resp.StatusCode, truncateLog(string(body), 200))
		}
		var list webshareListResponse
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("webshare response 解析: %w", err)
		}

		added := 0
		for _, p := range list.Results {
			if p.Valid != nil && !*p.Valid {
				continue // 失效代理（被替换/封禁）不入池
			}
			n := webshareToNode(p)
			if n == nil {
				continue
			}
			if seen[n.Fingerprint] {
				continue
			}
			seen[n.Fingerprint] = true
			nodes = append(nodes, n)
			added++
		}
		slog.Debug("webshare page fetched", "source", w.Name, "page", page, "added", added)

		if list.Next == "" || list.Next == "null" || len(list.Results) == 0 {
			break
		}
		page++
		if page > 50 { // 防御：最多 5000 个代理
			slog.Warn("webshare pagination guard exceeded", "source", w.Name, "page", page)
			break
		}
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("webshare 代理列表为空")
	}
	return uniqueNodeNames(nodes), nil
}

// webshareToNode 把单条 API 记录转成 SOCKS5 节点；地址/端口非法时返回 nil。
func webshareToNode(p webshareProxyDTO) *ProxyNode {
	addr := strings.TrimSpace(p.ProxyAddress)
	port, ok := toProxyPort(p.Port)
	if addr == "" || !ok {
		return nil
	}
	name := fmt.Sprintf("%s:%d", addr, port)
	if cc := strings.ToUpper(strings.TrimSpace(p.CountryCode)); cc != "" {
		name = cc + "-" + name
	}
	n := &ProxyNode{
		Name:     name,
		Protocol: "socks5",
		Address:  addr,
		Port:     port,
		UserID:   p.Username,
		Password: p.Password,
	}
	n.Fingerprint = computeFingerprint(n)
	return n
}

// toProxyPort 兼容 API 返回数字（float64）或字符串端口。
func toProxyPort(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), t > 0 && int(t) <= 65535
	case string:
		p, err := strconv.Atoi(strings.TrimSpace(t))
		return p, err == nil && p > 0 && p <= 65535
	}
	return 0, false
}

func truncateLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
