package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ======================== inferRegionFromName 测试 ========================

func TestInferRegionFromName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		// 标准格式
		{"US prefix", "US-38.153.152.244:9594", "us"},
		{"JP prefix", "JP-tokyo-01", "jp"},
		{"HK prefix", "HK-central", "hk"},
		{"SG prefix", "SG-01", "sg"},
		{"TW prefix", "TW-taipei", "tw"},
		{"KR prefix", "KR-seoul", "kr"},
		{"AU prefix", "AU-sydney", "au"},
		{"CA prefix", "CA-toronto", "ca"},
		{"EU prefix", "EU-frankfurt", "eu"},
		{"DE prefix", "DE-berlin", "eu"},
		{"FR prefix", "FR-paris", "eu"},
		{"UK prefix", "UK-london", "eu"},

		// 订阅名::节点名 格式
		{"subscription format", "webshare-main::US-38.153.152.244:9594", "us"},
		{"subscription JP", "my-sub::JP-tokyo-01", "jp"},

		// 完整名称
		{"united states", "united-states-node-01", "us"},
		{"america", "america-west-01", "us"},
		{"europe", "europe-central-01", "eu"},
		{"germany", "germany-frankfurt", "eu"},
		{"japan", "japan-tokyo-01", "jp"},
		{"hong kong", "hong-kong-01", "hk"},
		{"singapore", "singapore-01", "sg"},
		{"taiwan", "taiwan-taipei", "tw"},
		{"korea", "korea-seoul", "kr"},
		{"australia", "australia-sydney", "au"},
		{"canada", "canada-toronto", "ca"},

		// 无区域信息
		{"no region", "node-01", ""},
		{"empty", "", ""},
		{"ip only", "192.168.1.1:8080", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferRegionFromName(tt.input)
			if got != tt.expected {
				t.Errorf("inferRegionFromName(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// ======================== lookupModelRegion 测试 ========================

func TestLookupModelRegion(t *testing.T) {
	// 保存原始状态
	oldRegionMap := modelRegionMap
	configMu.Lock()
	modelRegionMap = map[string]string{
		"muse-spark-1.2-contributor": "us",
		"muse-*":                     "us",
		"eu-model":                   "eu",
	}
	configMu.Unlock()
	t.Cleanup(func() {
		configMu.Lock()
		modelRegionMap = oldRegionMap
		configMu.Unlock()
	})

	tests := []struct {
		name     string
		modelID  string
		expected string
	}{
		// 精确匹配
		{"exact match", "muse-spark-1.2-contributor", "us"},
		{"exact match eu", "eu-model", "eu"},

		// 通配符匹配
		{"wildcard match", "muse-something-else", "us"},
		{"wildcard match 2", "muse-another-model", "us"},

		// 无匹配
		{"no match", "other-model", ""},
		{"no match empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lookupModelRegion(tt.modelID)
			if got != tt.expected {
				t.Errorf("lookupModelRegion(%q) = %q, want %q", tt.modelID, got, tt.expected)
			}
		})
	}
}

func TestLookupModelRegionEmpty(t *testing.T) {
	// 空的 modelRegionMap
	oldRegionMap := modelRegionMap
	configMu.Lock()
	modelRegionMap = map[string]string{}
	configMu.Unlock()
	t.Cleanup(func() {
		configMu.Lock()
		modelRegionMap = oldRegionMap
		configMu.Unlock()
	})

	if got := lookupModelRegion("any-model"); got != "" {
		t.Errorf("lookupModelRegion with empty map = %q, want empty", got)
	}
}

func TestLookupModelRegionWildcardOnly(t *testing.T) {
	// 只有 * 通配符
	oldRegionMap := modelRegionMap
	configMu.Lock()
	modelRegionMap = map[string]string{
		"*": "us",
	}
	configMu.Unlock()
	t.Cleanup(func() {
		configMu.Lock()
		modelRegionMap = oldRegionMap
		configMu.Unlock()
	})

	if got := lookupModelRegion("any-model"); got != "us" {
		t.Errorf("lookupModelRegion with * = %q, want us", got)
	}
}

// ======================== classifyRegionRestriction 测试 ========================

func TestClassifyRegionRestriction(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		body           string
		expectedBool   bool
		expectedRegion string
	}{
		// 区域限制错误
		{"region restricted", 403, `{"error":{"type":"access_error","message":"region restricted"}}`, true, ""},
		{"not available in your region", 403, `{"error":{"message":"This model is not available in your region"}}`, true, ""},
		{"access denied from this region", 403, `{"error":{"message":"Access denied from this region"}}`, true, ""},
		{"unavailable in your region", 403, `{"error":{"message":"Model unavailable in your region"}}`, true, ""},
		{"geo-restricted", 403, `{"error":{"message":"Geo-restricted model"}}`, true, ""},
		{"location restricted", 403, `{"error":{"message":"Location restricted"}}`, true, ""},
		{"area restricted", 403, `{"error":{"message":"Area restricted"}}`, true, ""},
		{"not supported in your region", 403, `{"error":{"message":"Not supported in your region"}}`, true, ""},

		// 包含区域信息的错误
		{"only available in US", 403, `{"error":{"message":"This model is only available in US"}}`, true, "us"},
		{"restricted to EU", 403, `{"error":{"message":"Restricted to EU"}}`, true, "eu"},
		{"available in JP only", 403, `{"error":{"message":"Available in JP only"}}`, true, "jp"},

		// 非区域限制错误（不应匹配）
		{"quota error", 403, `{"error":{"type":"FreeUsageLimitError","message":"free usage limit exceeded"}}`, false, ""},
		{"not found", 404, `{"error":{"message":"model not found"}}`, false, ""},
		{"rate limit", 429, `{"error":{"message":"rate limited"}}`, false, ""},
		{"server error", 500, `{"error":{"message":"internal error"}}`, false, ""},

		// 403 无签名（应视为区域限制）
		{"403 no signature", 403, `{}`, true, ""},
		{"403 empty body", 403, ``, false, ""},

		// 非 403 状态码
		{"200 ok", 200, `{"error":{"message":"region restricted"}}`, false, ""},
		{"400 bad request", 400, `{"error":{"message":"region restricted"}}`, false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBool, gotRegion := classifyRegionRestriction(tt.status, []byte(tt.body))
			if gotBool != tt.expectedBool {
				t.Errorf("classifyRegionRestriction(%d, %q) bool = %v, want %v", tt.status, tt.body, gotBool, tt.expectedBool)
			}
			if gotRegion != tt.expectedRegion {
				t.Errorf("classifyRegionRestriction(%d, %q) region = %q, want %q", tt.status, tt.body, gotRegion, tt.expectedRegion)
			}
		})
	}
}

// ======================== pickForRegion 测试 ========================

func TestPickForRegion(t *testing.T) {
	oldPool := proxyPool
	t.Cleanup(func() { proxyPool = oldPool })

	n1 := &ProxyNode{Name: "us-node-1", Protocol: "socks5", Address: "1.2.3.4", Port: 1080, Region: "us"}
	n2 := &ProxyNode{Name: "us-node-2", Protocol: "socks5", Address: "5.6.7.8", Port: 1080, Region: "us"}
	n3 := &ProxyNode{Name: "eu-node-1", Protocol: "socks5", Address: "9.10.11.12", Port: 1080, Region: "eu"}
	n4 := &ProxyNode{Name: "no-region", Protocol: "socks5", Address: "13.14.15.16", Port: 1080, Region: ""}

	proxyPool = newProxyPool("")
	proxyPool.setNodes([]*ProxyNode{n1, n2, n3, n4})

	// 测试区域过滤
	t.Run("pick US region", func(t *testing.T) {
		node := proxyPool.pickForRegion(false, "us")
		if node == nil {
			t.Fatal("pickForRegion(us) = nil, want a node")
		}
		if node.Region != "us" {
			t.Errorf("pickForRegion(us) region = %q, want us", node.Region)
		}
	})

	t.Run("pick EU region", func(t *testing.T) {
		node := proxyPool.pickForRegion(false, "eu")
		if node == nil {
			t.Fatal("pickForRegion(eu) = nil, want a node")
		}
		if node.Region != "eu" {
			t.Errorf("pickForRegion(eu) region = %q, want eu", node.Region)
		}
	})

	t.Run("pick non-existent region", func(t *testing.T) {
		node := proxyPool.pickForRegion(false, "jp")
		if node != nil {
			t.Errorf("pickForRegion(jp) = %v, want nil", node)
		}
	})

	t.Run("pick empty region (fallback)", func(t *testing.T) {
		node := proxyPool.pickForRegion(false, "")
		if node == nil {
			t.Fatal("pickForRegion('') = nil, want a node")
		}
	})
}

// ======================== ProxyNodeConfig Region 测试 ========================

func TestProxyNodeConfigToNodeWithRegion(t *testing.T) {
	config := ProxyNodeConfig{
		Name:     "US-38.153.152.244:9594",
		Protocol: "socks5",
		Address:  "38.153.152.244",
		Port:     9594,
		Region:   "",
	}

	node := config.toNode()
	// 应该从名称推断区域
	if node.Region != "us" {
		t.Errorf("toNode() Region = %q, want us (inferred from name)", node.Region)
	}
}

func TestProxyNodeConfigToNodeWithExplicitRegion(t *testing.T) {
	config := ProxyNodeConfig{
		Name:     "my-node",
		Protocol: "socks5",
		Address:  "1.2.3.4",
		Port:     1080,
		Region:   "eu",
	}

	node := config.toNode()
	// 显式指定的区域应该保留
	if node.Region != "eu" {
		t.Errorf("toNode() Region = %q, want eu (explicit)", node.Region)
	}
}

// ======================== Admin Config API Region 测试 ========================

func TestAdminConfigRegionMapEcho(t *testing.T) {
	oldPath := configPath
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.json")
	t.Cleanup(func() { configPath = oldPath })

	configData := `{"model_region_map":{"muse-spark-1.2-contributor":"us"}}`
	if err := os.WriteFile(configPath, []byte(configData), 0644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	adminConfigHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp struct {
		ModelRegionMap map[string]string `json:"model_region_map"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ModelRegionMap["muse-spark-1.2-contributor"] != "us" {
		t.Fatalf("model_region_map = %v, want {muse-spark-1.2-contributor: us}", resp.ModelRegionMap)
	}
}

func TestAdminConfigRegionMapSave(t *testing.T) {
	oldPath := configPath
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.json")
	t.Cleanup(func() { configPath = oldPath })

	if err := os.WriteFile(configPath, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// 保存 region map
	patch := `{"model_region_map":{"muse-spark-1.2-contributor":"us","muse-*":"us"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(patch))
	rec := httptest.NewRecorder()
	adminConfigHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	// 验证保存结果
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		ModelRegionMap map[string]string `json:"model_region_map"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.ModelRegionMap["muse-spark-1.2-contributor"] != "us" {
		t.Fatalf("saved model_region_map = %v, want {muse-spark-1.2-contributor: us}", saved.ModelRegionMap)
	}
}

// ======================== applyConfig Region 测试 ========================

func TestApplyConfigRegionMap(t *testing.T) {
	oldRegionMap := modelRegionMap
	configMu.Lock()
	modelRegionMap = map[string]string{}
	configMu.Unlock()
	t.Cleanup(func() {
		configMu.Lock()
		modelRegionMap = oldRegionMap
		configMu.Unlock()
	})

	cfg := AppConfig{
		ModelRegionMap: map[string]string{
			"test-model": "us",
		},
	}
	applyConfig(cfg)

	configMu.RLock()
	got := modelRegionMap["test-model"]
	configMu.RUnlock()

	if got != "us" {
		t.Errorf("after applyConfig, modelRegionMap[test-model] = %q, want us", got)
	}
}

// ======================== mergeAppConfig Region 测试 ========================

func TestMergeAppConfigRegionMap(t *testing.T) {
	base := AppConfig{
		ModelRegionMap: map[string]string{
			"base-model": "eu",
		},
	}
	patch := AppConfig{
		ModelRegionMap: map[string]string{
			"patch-model": "us",
		},
	}

	merged := mergeAppConfig(base, patch)
	if merged.ModelRegionMap["patch-model"] != "us" {
		t.Errorf("merged.ModelRegionMap[patch-model] = %q, want us", merged.ModelRegionMap["patch-model"])
	}
	if merged.ModelRegionMap["base-model"] != "" {
		t.Errorf("merged.ModelRegionMap[base-model] = %q, want empty (patch replaces)", merged.ModelRegionMap["base-model"])
	}
}

// ======================== configPatch Region 测试 ========================

func TestMergeConfigPatchRegionMap(t *testing.T) {
	base := AppConfig{
		ModelRegionMap: map[string]string{
			"base-model": "eu",
		},
	}
	patch := configPatch{
		ModelRegionMap: map[string]string{
			"patch-model": "us",
		},
	}

	merged := mergeConfigPatch(base, patch)
	if merged.ModelRegionMap["patch-model"] != "us" {
		t.Errorf("merged.ModelRegionMap[patch-model] = %q, want us", merged.ModelRegionMap["patch-model"])
	}
}

func TestMergeConfigPatchRegionMapNil(t *testing.T) {
	base := AppConfig{
		ModelRegionMap: map[string]string{
			"base-model": "eu",
		},
	}
	patch := configPatch{
		ModelRegionMap: nil, // 不修改
	}

	merged := mergeConfigPatch(base, patch)
	if merged.ModelRegionMap["base-model"] != "eu" {
		t.Errorf("merged.ModelRegionMap[base-model] = %q, want eu (nil patch preserves base)", merged.ModelRegionMap["base-model"])
	}
}

// ======================== extractRegionFromMessage 测试 ========================

func TestExtractRegionFromMessage(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"only available in US", "this model is only available in us", "us"},
		{"restricted to EU", "restricted to eu", "eu"},
		{"available in JP only", "available in jp only", "jp"},
		{"only available in Singapore", "only available in singapore", "sg"},
		{"no region info", "this is a generic error", ""},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRegionFromMessage(tt.input)
			if got != tt.expected {
				t.Errorf("extractRegionFromMessage(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// ======================== availableRegions 测试 ========================

func TestAvailableRegions(t *testing.T) {
	oldPool := proxyPool
	t.Cleanup(func() { proxyPool = oldPool })

	n1 := &ProxyNode{Name: "us-1", Protocol: "socks5", Address: "1.2.3.4", Port: 1080, Region: "us"}
	n2 := &ProxyNode{Name: "us-2", Protocol: "socks5", Address: "5.6.7.8", Port: 1080, Region: "us"}
	n3 := &ProxyNode{Name: "eu-1", Protocol: "socks5", Address: "9.10.11.12", Port: 1080, Region: "eu"}
	n4 := &ProxyNode{Name: "no-region", Protocol: "socks5", Address: "127.0.0.1", Port: 1080, Region: ""}

	proxyPool = newProxyPool("")
	proxyPool.setNodes([]*ProxyNode{n1, n2, n3, n4})

	regions := proxyPool.availableRegions()
	if len(regions) != 3 { // us, eu, "" (127.0.0.1 是 loopback, GeoIP 不解析)
		t.Errorf("availableRegions() returned %d regions, want 3; got %v", len(regions), regions)
	}
}

// ======================== 探测状态管理测试 ========================

func TestRegionProbeState(t *testing.T) {
	// 清空探测状态
	regionProbeMapMu.Lock()
	regionProbeMap = map[string]*regionProbeState{}
	regionProbeMapMu.Unlock()
	t.Cleanup(func() {
		regionProbeMapMu.Lock()
		regionProbeMap = map[string]*regionProbeState{}
		regionProbeMapMu.Unlock()
	})

	// 初始状态：无已尝试区域
	state := getOrCreateProbeState("test-model")
	if len(state.triedRegions) != 0 {
		t.Errorf("new probe state should have 0 tried regions, got %d", len(state.triedRegions))
	}

	// 标记已尝试
	markRegionTried("test-model", "us")
	markRegionTried("test-model", "eu")
	state = getOrCreateProbeState("test-model")
	if len(state.triedRegions) != 2 {
		t.Errorf("after marking 2 regions, triedRegions should have 2 entries, got %d", len(state.triedRegions))
	}
	if !state.triedRegions["us"] {
		t.Error("us should be marked as tried")
	}
	if !state.triedRegions["eu"] {
		t.Error("eu should be marked as tried")
	}
}

func TestNextUntriedRegion(t *testing.T) {
	oldPool := proxyPool
	t.Cleanup(func() { proxyPool = oldPool })

	n1 := &ProxyNode{Name: "us-1", Protocol: "socks5", Address: "1.2.3.4", Port: 1080, Region: "us"}
	n2 := &ProxyNode{Name: "eu-1", Protocol: "socks5", Address: "5.6.7.8", Port: 1080, Region: "eu"}
	n3 := &ProxyNode{Name: "jp-1", Protocol: "socks5", Address: "9.10.11.12", Port: 1080, Region: "jp"}

	proxyPool = newProxyPool("")
	proxyPool.setNodes([]*ProxyNode{n1, n2, n3})

	// 清空探测状态
	regionProbeMapMu.Lock()
	regionProbeMap = map[string]*regionProbeState{}
	regionProbeMapMu.Unlock()
	t.Cleanup(func() {
		regionProbeMapMu.Lock()
		regionProbeMap = map[string]*regionProbeState{}
		regionProbeMapMu.Unlock()
	})

	// 第一个未尝试的区域
	next := nextUntriedRegion("probe-model")
	if next == "" {
		t.Fatal("nextUntriedRegion() returned empty, want a region")
	}

	// 标记该区域已尝试
	markRegionTried("probe-model", next)

	// 第二个未尝试的区域
	next2 := nextUntriedRegion("probe-model")
	if next2 == "" {
		t.Fatal("nextUntriedRegion() returned empty after marking one, want another region")
	}
	if next2 == next {
		t.Errorf("nextUntriedRegion() returned same region %q twice", next)
	}

	// 标记所有区域
	markRegionTried("probe-model", next2)
	// 找第三个
	next3 := nextUntriedRegion("probe-model")
	if next3 == "" {
		t.Fatal("nextUntriedRegion() returned empty after marking 2, want a third region")
	}
	markRegionTried("probe-model", next3)

	// 全部试完
	next4 := nextUntriedRegion("probe-model")
	if next4 != "" {
		t.Errorf("nextUntriedRegion() = %q after all tried, want empty", next4)
	}
}

// ======================== classifyRegionRestriction 新关键词测试 ========================

func TestClassifyRegionRestrictionCountry(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		expected bool
	}{
		{"not available in your country", 403, `{"type":"error","error":{"type":"RegionError","message":"This model is not available in your country."}}`, true},
		{"unavailable in your country", 403, `{"error":{"message":"Model unavailable in your country"}}`, true},
		{"not supported in your country", 403, `{"error":{"message":"Not supported in your country"}}`, true},
		{"generic 403 with RegionError type", 403, `{"type":"error","error":{"type":"RegionError"}}`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := classifyRegionRestriction(tt.status, []byte(tt.body))
			if got != tt.expected {
				t.Errorf("classifyRegionRestriction(%d, %q) = %v, want %v", tt.status, tt.body, got, tt.expected)
			}
		})
	}
}

// ======================== 探测完整流程模拟 ========================

func TestProbeFlowSimulate(t *testing.T) {
	oldPool := proxyPool
	t.Cleanup(func() { proxyPool = oldPool })

	// 设置3个不同区域的节点
	n1 := &ProxyNode{Name: "us-1", Protocol: "socks5", Address: "1.2.3.4", Port: 1080, Region: "us"}
	n2 := &ProxyNode{Name: "eu-1", Protocol: "socks5", Address: "5.6.7.8", Port: 1080, Region: "eu"}
	n3 := &ProxyNode{Name: "jp-1", Protocol: "socks5", Address: "9.10.11.12", Port: 1080, Region: "jp"}

	proxyPool = newProxyPool("")
	proxyPool.setNodes([]*ProxyNode{n1, n2, n3})

	// 清空探测状态
	regionProbeMapMu.Lock()
	regionProbeMap = map[string]*regionProbeState{}
	regionProbeMapMu.Unlock()
	t.Cleanup(func() {
		regionProbeMapMu.Lock()
		regionProbeMap = map[string]*regionProbeState{}
		regionProbeMapMu.Unlock()
	})

	modelID := "test-region-model"

	// 模拟流程：先直连失败，然后依次探测各个区域

	// Step 1: 直连失败 → 标记 "" 已试
	markRegionTried(modelID, "")
	probe := getOrCreateProbeState(modelID)
	if len(probe.triedRegions) != 1 {
		t.Fatalf("after marking empty, triedRegions should have 1, got %d", len(probe.triedRegions))
	}

	// Step 2: 找到第一个未尝试的区域
	r1 := nextUntriedRegion(modelID)
	if r1 == "" {
		t.Fatal("nextUntriedRegion should return a region, got empty")
	}
	t.Logf("probe region 1: %s", r1)

	// Step 3: 模拟该区域也失败
	markRegionTried(modelID, r1)

	// Step 4: 找第二个未尝试的区域
	r2 := nextUntriedRegion(modelID)
	if r2 == "" {
		t.Fatal("nextUntriedRegion should return a second region, got empty")
	}
	if r2 == r1 {
		t.Fatalf("second region should differ from first, got %q twice", r2)
	}
	t.Logf("probe region 2: %s", r2)

	// Step 5: 模拟该区域也失败
	markRegionTried(modelID, r2)

	// Step 6: 找第三个未尝试的区域
	r3 := nextUntriedRegion(modelID)
	if r3 == "" {
		t.Fatal("nextUntriedRegion should return a third region, got empty")
	}
	if r3 == r1 || r3 == r2 {
		t.Fatalf("third region %q should differ from first two (%q, %q)", r3, r1, r2)
	}
	t.Logf("probe region 3: %s", r3)

	// Step 7: 模拟该区域成功 → 标记已试
	markRegionTried(modelID, r3)

	// Step 8: 所有区域都试完了
	r4 := nextUntriedRegion(modelID)
	if r4 != "" {
		t.Errorf("nextUntriedRegion after all tried should return empty, got %q", r4)
	}

	// 验证 modelRegionMap 应该被设置为成功的区域
	configMu.Lock()
	modelRegionMap[modelID] = r3 // 模拟成功后设置
	configMu.Unlock()
	if lookupModelRegion(modelID) != r3 {
		t.Errorf("lookupModelRegion should return %q, got %q", r3, lookupModelRegion(modelID))
	}
}

// ======================== probeState 模拟多次请求 ========================

func TestProbeStateMultipleRequests(t *testing.T) {
	oldPool := proxyPool
	t.Cleanup(func() { proxyPool = oldPool })

	n1 := &ProxyNode{Name: "us-1", Protocol: "socks5", Address: "1.2.3.4", Port: 1080, Region: "us"}
	n2 := &ProxyNode{Name: "eu-1", Protocol: "socks5", Address: "5.6.7.8", Port: 1080, Region: "eu"}
	proxyPool = newProxyPool("")
	proxyPool.setNodes([]*ProxyNode{n1, n2})

	// 清空探测状态
	regionProbeMapMu.Lock()
	regionProbeMap = map[string]*regionProbeState{}
	regionProbeMapMu.Unlock()
	t.Cleanup(func() {
		regionProbeMapMu.Lock()
		regionProbeMap = map[string]*regionProbeState{}
		regionProbeMapMu.Unlock()
	})

	modelID := "multi-request-model"

	// 模拟第一次请求的探测过程
	markRegionTried(modelID, "")
	r1 := nextUntriedRegion(modelID)
	if r1 == "" {
		t.Fatal("first probe should find a region")
	}
	markRegionTried(modelID, r1)

	// 模拟第二次请求（新的请求，但模型已知区域限制）
	// 不应该再次触发探测（因为 lookupModelRegion 已有值）
	configMu.Lock()
	modelRegionMap[modelID] = r1
	configMu.Unlock()

	// 验证 lookupModelRegion 返回已知区域
	if got := lookupModelRegion(modelID); got != r1 {
		t.Errorf("lookupModelRegion should return %q for subsequent request, got %q", r1, got)
	}

	// 清理
	configMu.Lock()
	delete(modelRegionMap, modelID)
	configMu.Unlock()
}
