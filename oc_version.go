package main

// ======================== 上游 opencode 版本探测 ========================
//
// 上游免费层会校验 User-Agent 里的 opencode 版本，低于门槛直接返回
//
//	426 {"type":"error","error":{"type":"UpgradeRequired",
//	     "message":"Error from provider (Console): OpenCode 1.18.0 or newer is required to use the free tier"}}
//
// 2026-09-21 实测（其余三道闸门全部满足，只改 UA）：
//
//	opencode/1.15.3 / 1.16.0 / 1.17.0 / 1.17.9  -> 426
//	opencode/1.18.0 / 1.18.31 / 1.19.0 / 999.0.0 -> 200
//	opencode/latest/1.18.31/cli                  -> 200
//	UA 缺失                                       -> 403（另一道闸门）
//
// 因此门槛是 >= 1.18.0（注意高于社区早前记录的 1.17.0）。
//
// 版本号来自 npm registry 的 opencode-ai latest；探测失败时会回退。
// 历史回退常量是 "1.15.3"，低于门槛，导致网络抖动时全量 426——这里改为
// 「最近一次成功探测到的版本」，并以满足门槛的常量兜底。

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// minFreeTierOCVersion 是上游免费层实测要求的最低 opencode 版本。
const minFreeTierOCVersion = "1.18.0"

// fallbackOCVersion 是探测失败且没有历史成功记录时的兜底版本，
// 必须自身满足 minFreeTierOCVersion。
const fallbackOCVersion = "1.18.31"

var (
	ocVersionMu   sync.Mutex
	ocVersionGood string
)

// sanitizeOCVersion 把版本串规范化为前导的 "x.y.z"，取不到则返回空串。
// 上游拒绝非 release 形式（如 git-describe 的 1.18.31.r12.g88c6c7a），
// 因此只保留前导三段数字。
func sanitizeOCVersion(v string) string {
	v = strings.TrimSpace(v)
	parts := strings.Split(v, ".")
	if len(parts) < 3 {
		return ""
	}
	out := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		if _, err := strconv.Atoi(parts[i]); err != nil {
			return ""
		}
		out = append(out, parts[i])
	}
	return strings.Join(out, ".")
}

// semverAtLeast 判断 v 的前导 "x.y.z" 是否 >= min。解析失败视为不满足。
func semverAtLeast(v, min string) bool {
	a, ok := parseSemver(v)
	if !ok {
		return false
	}
	b, ok := parseSemver(min)
	if !ok {
		return false
	}
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

func parseSemver(v string) ([3]int, bool) {
	s := sanitizeOCVersion(v)
	if s == "" {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range strings.Split(s, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// rememberOCVersion 记录一次成功探测到的版本。低于门槛的版本不入缓存，
// 以免把「已知会被拒」的版本固化成回退值。
func rememberOCVersion(v string) {
	v = sanitizeOCVersion(v)
	if v == "" || !semverAtLeast(v, minFreeTierOCVersion) {
		return
	}
	ocVersionMu.Lock()
	ocVersionGood = v
	ocVersionMu.Unlock()
}

// lastGoodOCVersion 返回最近一次成功探测到的版本；没有则返回兜底常量。
func lastGoodOCVersion() string {
	ocVersionMu.Lock()
	defer ocVersionMu.Unlock()
	if ocVersionGood != "" {
		return ocVersionGood
	}
	return fallbackOCVersion
}

// probeOCVersion 请求 npm registry 拿 opencode-ai latest，成功时更新缓存。
func probeOCVersion(client *http.Client) string {
	req, err := http.NewRequest("GET", "https://registry.npmjs.org/opencode-ai/latest", nil)
	if err != nil {
		return lastGoodOCVersion()
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return lastGoodOCVersion()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var info struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &info) != nil {
		return lastGoodOCVersion()
	}
	v := sanitizeOCVersion(info.Version)
	if v == "" {
		return lastGoodOCVersion()
	}
	rememberOCVersion(v)
	return v
}

// ocVersionClient 是版本探测用的客户端（原 fetchOCVersionDirect 语义）。
func ocVersionClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}
