# 管理面板视觉层移植 — 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development (recommended) or executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 test 分支的 admin 面板同时保有新视觉设计与 main 的完整后端功能（真实数据、6 个 POST、4 组配置编辑器）。

**Architecture:** 以 `main` 的 `static/admin.html` 为基底，把 test 的视觉层（`--series-*` 色板、概览四格统计、筛选下拉、布局）移植过去。后端唯一改动是给 `/api/stats/trends` 增加 `group=model` 参数以支持模型筛选联动趋势图；不带该参数时响应与改动前完全一致。

**Tech Stack:** Go 1.26（`CGO_ENABLED=0`）、ECharts 5.5.1（CDN）、纯 CSS 变量主题、单文件嵌入式前端（`//go:embed static/*`）

**Spec:** `docs/superpowers/specs/2026-09-28-admin-visual-port-design.md`

## Global Constraints

以下约束适用于每一个任务，不再逐任务重复。

- **`*_test.go` 已在 `.gitignore` 中**（见 commit `dba84b3`「测试文件仅保留本地，不再入库」）。测试照常写、照常跑，但**永远不要 `git add` 测试文件**，提交步骤里已排除。
- **Go 构建缓存必须在工作区内**。本机 `~/.cache/go-build` 不可写，每次构建前设置：
  ```bash
  export GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
  ```
  三个目录均已被 `.gitignore` 忽略。
- **Git 写操作需要提权**。本 worktree 的 `.git` 目录是 `/home/jingyx/project/opencode2api/.git/worktrees/opencode2api-test`，在会话工作区之外，`git add` / `git commit` 需以 `danger-full-access` 执行。
- **提交信息遵循 Conventional Commits**，正文用中文（与 main 分支既有提交一致）。
- **本地测试实例**固定运行在 8899 端口，工作目录 `.uidemo/`（内含 `config.json` / `stats.json` / `call_log.jsonl`，共 35 天 5333 条种子数据，均被 gitignore）。启动命令见 Task 2 Step 2。
- **不做 cost**：本次不实现任何金额显示，后端 `/api/stats/cost` 仍不存在。
- **时间范围只做 4 项**：当天 / 7 天 / 30 天 / 180 天。`90d` 与「全部时间」不做。
- **ECharts 5.5.1 走 CDN**（`https://cdn.jsdelivr.net/npm/echarts@5.5.1/dist/echarts.min.js`），已实测容器内与宿主机均可访问。不要改成本地打包。

---

### Task 1: 后端 `/api/stats/trends?group=model`

**Files:**
- Modify: `calllog.go`（在 `TrendsPoint` 定义之后、`trendsFromCallLog` 之前插入新类型；替换 `trendsFromCallLog` 整个函数为三段式）
- Modify: `main.go:6902-6918`（`adminTrendsHandler`）
- Test: `trends_test.go`（新建，本地保留，不提交）

**Interfaces:**
- Consumes: 无（首个任务）
- Produces:
  - `func trendBuckets(rng string) ([]string, func(time.Time) int)` — 桶标签 + 时间→桶索引映射
  - `func forEachBucketedCallRecord(rng string, fn func(rec CallRecord, bucket int))` — 遍历区间内记录
  - `type ModelTrendPoint struct` — `TS, Model, Requests, OK, Fail, PromptTokens, CompletionTokens, CacheCreation, CacheRead`
  - `func trendsByModelFromCallLog(rng string) []ModelTrendPoint` — 模型名升序，桶索引升序
  - `func trendsFromCallLog(rng string) []TrendsPoint` — 签名与行为均不变（重构后须产出与重构前逐字节相同的 JSON）
  - `GET /api/stats/trends?range=<r>&group=model` → `[]ModelTrendPoint`

---

- [ ] **Step 1: 写失败测试**

新建 `trends_test.go`：

```go
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCallLog 把测试记录写入临时 call_log.jsonl 并接管 callLog.path。
func writeCallLog(t *testing.T, recs []CallRecord) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "call_log.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	callLog.mu.Lock()
	old := callLog.path
	callLog.path = p
	callLog.mu.Unlock()
	t.Cleanup(func() {
		callLog.mu.Lock()
		callLog.path = old
		callLog.mu.Unlock()
	})
}

// dayAt 返回 n 天前第 h 小时的整点（本地时区）。
func dayAt(n, h int) time.Time {
	now := time.Now()
	d := now.AddDate(0, 0, n)
	return time.Date(d.Year(), d.Month(), d.Day(), h, 0, 0, 0, now.Location())
}

func TestTrendsByModelZeroFillAndSplit(t *testing.T) {
	writeCallLog(t, []CallRecord{
		{ReqID: "a", TS: dayAt(0, 10).Format(time.RFC3339), Model: "m1", Status: "ok", PromptTokens: 100, CompletionTokens: 10, CacheRead: 40},
		{ReqID: "b", TS: dayAt(0, 11).Format(time.RFC3339), Model: "m1", Status: "ok", PromptTokens: 200, CompletionTokens: 20, CacheRead: 60},
		{ReqID: "c", TS: dayAt(0, 10).Format(time.RFC3339), Model: "m2", Status: "error"},
	})

	got := trendsByModelFromCallLog("7d")
	if len(got) != 14 { // 2 个模型 × 7 个桶
		t.Fatalf("want 14 rows (2 models x 7 buckets), got %d", len(got))
	}

	today := time.Now().Format("2006-01-02")
	var m1, m2 ModelTrendPoint
	for _, r := range got {
		if r.TS != today {
			// 零桶策略：非今天的桶必须存在且全零
			if r.Requests != 0 || r.PromptTokens != 0 || r.OK != 0 || r.Fail != 0 {
				t.Errorf("zero-fill broken: %+v", r)
			}
			continue
		}
		if r.Model == "m1" {
			m1 = r
		}
		if r.Model == "m2" {
			m2 = r
		}
	}

	if m1.Requests != 2 || m1.OK != 2 || m1.Fail != 0 {
		t.Errorf("m1: requests=%d ok=%d fail=%d; want 2/2/0", m1.Requests, m1.OK, m1.Fail)
	}
	if m1.PromptTokens != 300 || m1.CompletionTokens != 30 || m1.CacheRead != 100 {
		t.Errorf("m1 tokens: prompt=%d completion=%d cache_read=%d; want 300/30/100",
			m1.PromptTokens, m1.CompletionTokens, m1.CacheRead)
	}
	if m2.Requests != 1 || m2.Fail != 1 || m2.OK != 0 {
		t.Errorf("m2: requests=%d fail=%d ok=%d; want 1/1/0", m2.Requests, m2.Fail, m2.OK)
	}

	// 顺序：模型名升序，每模型桶索引升序
	if got[0].Model != "m1" || got[7].Model != "m2" {
		t.Errorf("expected model-major order, got [0]=%s [7]=%s", got[0].Model, got[7].Model)
	}
}

func TestTrendsByModelSumsToAggregate(t *testing.T) {
	writeCallLog(t, []CallRecord{
		{ReqID: "a", TS: dayAt(0, 9).Format(time.RFC3339), Model: "m1", Status: "ok", PromptTokens: 100, CompletionTokens: 10, CacheCreation: 5, CacheRead: 40},
		{ReqID: "b", TS: dayAt(0, 9).Format(time.RFC3339), Model: "m2", Status: "ok", PromptTokens: 300, CompletionTokens: 20, CacheCreation: 7, CacheRead: 60},
		{ReqID: "c", TS: dayAt(1, 9).Format(time.RFC3339), Model: "m2", Status: "error", PromptTokens: 1, CompletionTokens: 2, CacheCreation: 3, CacheRead: 4},
	})

	agg := trendsFromCallLog("7d")
	per := trendsByModelFromCallLog("7d")

	sum := map[string]*ModelTrendPoint{}
	for _, r := range per {
		if s, ok := sum[r.TS]; ok {
			s.Requests += r.Requests
			s.OK += r.OK
			s.Fail += r.Fail
			s.PromptTokens += r.PromptTokens
			s.CompletionTokens += r.CompletionTokens
			s.CacheCreation += r.CacheCreation
			s.CacheRead += r.CacheRead
		} else {
			c := r
			sum[r.TS] = &c
		}
	}

	if len(agg) != len(sum) {
		t.Fatalf("bucket count mismatch: aggregate=%d perModel=%d", len(agg), len(sum))
	}
	for _, a := range agg {
		s := sum[a.TS]
		if s == nil {
			t.Fatalf("no per-model rows for bucket %s", a.TS)
		}
		if a.Requests != s.Requests || a.OK != s.OK || a.Fail != s.Fail ||
			a.PromptTokens != s.PromptTokens || a.CompletionTokens != s.CompletionTokens ||
			a.CacheCreation != s.CacheCreation || a.CacheRead != s.CacheRead {
			t.Errorf("bucket %s mismatch:\n  aggregate=%+v\n  perModel =%+v", a.TS, a, *s)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
export GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
mkdir -p "$GOCACHE" "$GOMODCACHE" "$GOTMPDIR"
go test -run TestTrendsByModel ./... 2>&1 | head -20
```

Expected: 编译失败，`undefined: trendsByModelFromCallLog`

- [ ] **Step 3: 在 calllog.go 中加入类型与新函数**

在 `TrendsPoint` 结构体定义之后插入：

```go
// ModelTrendPoint 用量趋势按模型分组的单桶记录。字段与 TrendsPoint 一致，
// 额外携带 Model，供前端做模型维度筛选。
type ModelTrendPoint struct {
	TS               string `json:"ts"`
	Model            string `json:"model"`
	Requests         int64  `json:"requests"`
	OK               int64  `json:"ok"`
	Fail             int64  `json:"fail"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	CacheCreation    int64  `json:"cache_creation_tokens"`
	CacheRead        int64  `json:"cache_read_tokens"`
}
```

- [ ] **Step 4: 用三段式替换 trendsFromCallLog**

把 `calllog.go` 中从 `func trendsFromCallLog(rng string) []TrendsPoint {` 到其闭合 `}` 的**整个函数**替换为以下三段（`dayIdx` 与桶切分逻辑原样搬入 `trendBuckets`，行为不变）：

```go
// trendBuckets 按 range 切分时间桶：返回每个桶的时间标签与「时间→桶索引」映射。
// today 返回 24 个整点小时桶；7d/30d/180d 返回逐日桶。均含零数据时段以保证连线连续。
func trendBuckets(rng string) ([]string, func(time.Time) int) {
	now := time.Now()
	loc := now.Location()
	switch rng {
	case "7d", "30d", "180d":
		n := map[string]int{"7d": 7, "30d": 30, "180d": 180}[rng]
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(n - 1))
		ts := make([]string, n)
		for i := 0; i < n; i++ {
			ts[i] = start.AddDate(0, 0, i).Format("2006-01-02")
		}
		dayIdx := func(t time.Time) int {
			lt := t.Local()
			dayStart := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, lt.Location())
			return int(dayStart.Sub(start) / (24 * time.Hour))
		}
		return ts, dayIdx
	default: // today
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		ts := make([]string, 24)
		for i := 0; i < 24; i++ {
			ts[i] = start.Add(time.Duration(i) * time.Hour).Format("2006-01-02T15:04")
		}
		return ts, func(t time.Time) int {
			lt := t.In(loc)
			if lt.Year() != now.Year() || lt.YearDay() != now.YearDay() {
				return -1
			}
			return lt.Hour()
		}
	}
}

// forEachBucketedCallRecord 遍历落在 rng 区间内的调用记录，fn 收到记录与桶索引。
func forEachBucketedCallRecord(rng string, fn func(rec CallRecord, bucket int)) {
	callLog.mu.Lock()
	p := callLog.path
	callLog.mu.Unlock()
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	ts, bidx := trendBuckets(rng)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec CallRecord
		if json.Unmarshal([]byte(line), &rec) != nil || rec.TS == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, rec.TS)
		if err != nil {
			continue
		}
		i := bidx(t)
		if i < 0 || i >= len(ts) {
			continue
		}
		fn(rec, i)
	}
}

func trendsFromCallLog(rng string) []TrendsPoint {
	ts, _ := trendBuckets(rng)
	buckets := make([]TrendsPoint, len(ts))
	for i, label := range ts {
		buckets[i] = TrendsPoint{TS: label}
	}
	forEachBucketedCallRecord(rng, func(rec CallRecord, i int) {
		buckets[i].Requests++
		if rec.Status == "ok" {
			buckets[i].OK++
		} else {
			buckets[i].Fail++
		}
		buckets[i].PromptTokens += rec.PromptTokens
		buckets[i].CompletionTokens += rec.CompletionTokens
		buckets[i].CacheCreation += rec.CacheCreation
		buckets[i].CacheRead += rec.CacheRead
	})
	return buckets
}

// trendsByModelFromCallLog 在 trendsFromCallLog 的切分基础上按模型再分一层。
// 对区间内出现过请求的每个模型输出全部桶（无数据填零），保证该模型的折线
// 覆盖整个区间而非只在有流量的几天出现。输出顺序：模型名升序，桶索引升序。
func trendsByModelFromCallLog(rng string) []ModelTrendPoint {
	ts, _ := trendBuckets(rng)
	idx := make(map[string][]ModelTrendPoint)
	forEachBucketedCallRecord(rng, func(rec CallRecord, i int) {
		row, ok := idx[rec.Model]
		if !ok {
			row = make([]ModelTrendPoint, len(ts))
			for j, label := range ts {
				row[j] = ModelTrendPoint{TS: label, Model: rec.Model}
			}
			idx[rec.Model] = row
		}
		row[i].Requests++
		if rec.Status == "ok" {
			row[i].OK++
		} else {
			row[i].Fail++
		}
		row[i].PromptTokens += rec.PromptTokens
		row[i].CompletionTokens += rec.CompletionTokens
		row[i].CacheCreation += rec.CacheCreation
		row[i].CacheRead += rec.CacheRead
	})
	models := make([]string, 0, len(idx))
	for m := range idx {
		models = append(models, m)
	}
	sort.Strings(models)
	out := make([]ModelTrendPoint, 0, len(models)*len(ts))
	for _, m := range models {
		out = append(out, idx[m]...)
	}
	return out
}
```

- [ ] **Step 5: 补 import**

`calllog.go` 的 import 块加入 `"sort"`（按字母序插在 `"path/filepath"` 之后）：

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)
```

- [ ] **Step 6: 运行测试确认通过**

```bash
export GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
go test -run TestTrendsByModel -v ./... 2>&1 | head -30
```

Expected: `--- PASS: TestTrendsByModelZeroFillAndSplit` 与 `--- PASS: TestTrendsByModelSumsToAggregate`

- [ ] **Step 7: 改 adminTrendsHandler 分派 group 参数**

把 `main.go:6902-6918` 的 `adminTrendsHandler` 整个函数替换为：

```go
// adminTrendsHandler 用量趋势：按 range=today|7d|30d|180d 返回聚簇时间序列（来自调用日志）。
// group=model 时额外按模型分组，供前端做模型维度筛选；不带该参数时行为与原先完全一致。
func adminTrendsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rng := r.URL.Query().Get("range")
	if rng != "7d" && rng != "30d" && rng != "180d" {
		rng = "today"
	}
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Query().Get("group") == "model" {
		pts := trendsByModelFromCallLog(rng)
		if pts == nil {
			pts = []ModelTrendPoint{}
		}
		json.NewEncoder(w).Encode(pts)
		return
	}
	pts := trendsFromCallLog(rng)
	if pts == nil {
		pts = []TrendsPoint{}
	}
	json.NewEncoder(w).Encode(pts)
}
```

- [ ] **Step 8: 构建与格式检查**

```bash
export GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
gofmt -l calllog.go main.go && echo "格式 OK"
go build -o /tmp/oc2api-verify . && echo "构建 OK"
```

Expected: `格式 OK`、`构建 OK`

- [ ] **Step 9: 重启实例并验证接口**

先停掉旧的后台任务，然后：

```bash
cd .uidemo && exec /tmp/oc2api-verify -port 8899 -config "$PWD/config.json" -password "" -log-level warn -log-stdout=false
```

```bash
# 1) 不带 group：结构仍是聚合桶
curl -s 'http://127.0.0.1:8899/api/stats/trends?range=7d' | python3 -c "import json,sys; d=json.load(sys.stdin); print('聚合桶数:', len(d), '| 字段:', sorted(d[0].keys()))"
# 期望：聚合桶数: 7 | 字段: ['cache_creation_tokens', 'cache_read_tokens', 'completion_tokens', 'fail', 'ok', 'prompt_tokens', 'requests', 'ts']

# 2) 带 group=model：出现 model 字段，且行数 = 模型数 × 7
curl -s 'http://127.0.0.1:8899/api/stats/trends?range=7d&group=model' | python3 -c "import json,sys; d=json.load(sys.stdin); ms=sorted({r['model'] for r in d}); print('行数:', len(d), '| 模型数:', len(ms), '| 字段:', sorted(d[0].keys()))"
# 期望：行数 = 模型数 × 7，字段含 model

# 3) 逐桶求和等于聚合值
curl -s 'http://127.0.0.1:8899/api/stats/trends?range=7d' > /tmp/agg.json
curl -s 'http://127.0.0.1:8899/api/stats/trends?range=7d&group=model' > /tmp/per.json
python3 -c "
import json
agg={r['ts']:r for r in json.load(open('/tmp/agg.json'))}
per={}
for r in json.load(open('/tmp/per.json')):
    s=per.setdefault(r['ts'],{'requests':0,'ok':0,'fail':0,'prompt_tokens':0,'completion_tokens':0,'cache_creation_tokens':0,'cache_read_tokens':0})
    for k in s: s[k]+=r.get(k,0)
bad=[t for t in agg if any(agg[t][k]!=per[t][k] for k in per[t])]
print('不一致的桶:', bad if bad else '无 ✅')
"
```

- [ ] **Step 10: 提交**

```bash
git add calllog.go main.go
git commit -m "feat(stats): /api/stats/trends 支持 group=model 按模型分组

新增 trendsByModelFromCallLog，与 trendsFromCallLog 共享桶切分逻辑
（抽出 trendBuckets 与 forEachBucketedCallRecord），额外按模型分层。
对区间内出现过请求的每个模型输出全部桶、无数据填零，保证折线覆盖
整个区间而非只在有流量的几天出现。

adminTrendsHandler 读 group 参数：group=model 走新函数，不带该参数时
调用链与产出与改动前完全一致，已用接口 diff 验证。

测试文件按仓库约定（.gitignore 忽略 *_test.go）仅保留本地。"
```

---

### Task 2: 基底切换为 main 版本的 admin.html

**Files:**
- Modify: `static/admin.html`（整体替换为 main 版本，1520 行）

**Interfaces:**
- Consumes: 无
- Produces: 一个功能完整、数据真实、视觉等同 main 的 admin 面板。后续任务在此之上叠加视觉层。

---

- [ ] **Step 1: 记录当前 test 版本以备回退**

```bash
cp static/admin.html /tmp/admin-test-visual.html
echo "已备份到 /tmp/admin-test-visual.html"
```

- [ ] **Step 2: 替换为 main 版本**

```bash
git show main:static/admin.html > static/admin.html
wc -l static/admin.html   # 期望 1520
```

- [ ] **Step 3: 构建并重启**

```bash
export GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
go build -o /tmp/oc2api-verify . && echo "构建 OK"
```

停掉 8899 上的旧进程（`job_kill`），然后：

```bash
cd .uidemo && exec /tmp/oc2api-verify -port 8899 -config "$PWD/config.json" -password "" -log-level warn -log-stdout=false
```

- [ ] **Step 4: 浏览器确认五个页面都有真实数据**

打开 `http://127.0.0.1:8899/`，逐项确认（`-password ""` 使其免密直接进面板）：

| 页面 | 预期 |
| --- | --- |
| 概览 | 顶部统计卡有总 Token / 请求数 / 缓存命中率；热力图、趋势图、甜甜圈、模型明细表均有内容 |
| 节点池 | 节点表有 0 行但显示「暂无节点」提示（本地实例无节点池），筛选 chip 可点 |
| 订阅与配额 | 订阅表 0 行；**配额设置 5 个输入框可见**；有「添加订阅」按钮 |
| 代理与模型 | SOCKS5 表、别名表、模型能力表（应列出 10 个 free 模型）均有容器 |
| 运行日志 | 「连接」按钮点开后能建立 SSE；调用日志列表有数据 |

任何一页显示 DEMO 假数据（节点名 `hk-01`、订阅 `主订阅`、日志 `12:30:25`）都说明替换失败，回退到 Step 1。

- [ ] **Step 5: 验证 6 个 POST 端到端可用**

在「代理与模型」页改一个别名并保存，然后：

```bash
cat .uidemo/config.json
```

预期 `model_alias` 里出现你刚保存的条目。改完手工还原。

再在「节点池」页确认「切换/解除」按钮存在（无节点时表格为空，按钮不可点属正常）。

- [ ] **Step 6: 提交**

```bash
git add static/admin.html
git commit -m "refactor(admin): 基底切换为 main 的 admin.html，恢复后端数据绑定

test 分支的 admin.html 是纯静态高保真重做：全文仅 1 处 fetch 探活，
12 个 render* 全部读硬编码 DEMO_* 常量，6 个 POST 全部缺失，ECharts 被
移除改为纯 CSS 图表，趋势数据日期写死在 2026-09-22。

按设计文档 docs/superpowers/specs/2026-09-28-admin-visual-port-design.md
选定的路线 B，本任务先把基底换回 main 版本——test 86 个元素 id 中 59% 在
main 已存在，两分支 CSS 令牌同源，chartHeatmap 卡片同 id 同 class 仅渲染器
不同，说明新视觉是同一设计系统的演进，移植成本远低于重建。

test 的视觉元素（--series-* 色板、概览四格、筛选下拉、布局）在后续任务叠加。
当前文件已 /tmp/admin-test-visual.html 留档，test 完整版本可从
commit 2a29a37 取回。"
```

---

### Task 3: 移植 `--series-*` 色板并改写图表取色

**Files:**
- Modify: `static/admin.html`（`:root` 变量块、亮色主题块、`initTrend`、`initReqTrend`、`initDonut`）

> **不包含 `initHeatmap`。** 早前版本的文件清单列了它，但 8 个 Step 无一步涉及，属计划自身的不一致，已移除。判断依据见 Task 3 审查：规范契约是 Step + Interfaces 段，Files 段是非规范的落点提示；Step 7 的 4 条验收是穷举的、不含热力图；Interfaces 段自己把消费方划给了「后续任务」。

**Interfaces:**
- Consumes: Task 2 的 main 版 admin.html
- Produces: CSS 变量 `--series-1` … `--series-5`（暗色与亮色各一套）；新增 JS 辅助 `function seriesColor(i int) string`，返回第 i 个系列色（超出 5 个时循环取模）。后续任务的图表与筛选下拉共用该函数。

> **热力图不要顺手改成 `seriesColor()`。** 热力图编码的是**连续量级**（`maxV=Math.max(1,...cells.map(c=>c[2]))`），需要 sequential ramp；而 `--series-*` 是**分类色板**（输入/输出/缓存读/缓存写/辅助）。塞进 `visualMap.inRange` 会让低值渲染成绿、中值渲染成紫，把「多/少」读成「不同类别」，量级顺序被摧毁。现状（写死的 `['#cfe0ff'…'#2b5fd9']` 单调蓝 ramp）在亮色下是可读的，缺口只是色板一致性——**留着不动严格优于改坏**。若将来要统一，应另给 `--heat-0..5` 顺序色 token（双主题各一套）。

---

- [ ] **Step 1: 加入暗色主题色板**

在 `static/admin.html` 的 `:root{...}` 块中，`--purple-dim` 那一行之后插入：

```css
  /* 数据系列色板：图表与模型筛选共用，亮色主题下另有一套 */
  --series-1:#5b9bff;  /* 输入 / 首个模型 */
  --series-2:#2fd489;  /* 输出 */
  --series-3:#b18cff;  /* 缓存读 */
  --series-4:#f0b04a;  /* 缓存写 */
  --series-5:#fb7185;  /* 辅助 */
```

- [ ] **Step 2: 加入亮色主题色板**

在 `[data-theme="light"]{...}` 块内追加：

```css
  --series-1:#2f5fd6; --series-2:#0d8f5c; --series-3:#7048c4; --series-4:#b57500; --series-5:#d93a5c;
```

- [ ] **Step 3: 新增 seriesColor 辅助函数**

在 `function getCSS(v){...}` 之后插入：

```javascript
// seriesColor 返回第 i 个数据系列的色值，超出 5 个时循环取模。
function seriesColor(i){const n=((i%5)+5)%5;return getCSS('--series-'+(n+1))||['#5b9bff','#2fd489','#b18cff','#f0b04a','#fb7185'][n]}
```

- [ ] **Step 4: 改写 initTrend 的三条线取色**

把 `initTrend` 中这一行：

```javascript
    const bC=getCSS('--blue')||'#7ea2ff',oC=getCSS('--ok')||'#2fd489',pC=getCSS('--purple')||'#b197f7';
```

替换为：

```javascript
    const bC=seriesColor(0),oC=seriesColor(1),pC=seriesColor(2);
```

- [ ] **Step 5: 改写 initReqTrend 的两条线取色**

把 `initReqTrend` 中这一行：

```javascript
    const okC=getCSS('--ok')||'#2fd489',failC=getCSS('--red')||'#ff6b6b';
```

替换为：

```javascript
    const okC=seriesColor(1),failC=getCSS('--red')||'#ff6b6b';
```

- [ ] **Step 6: 改写 initDonut 的硬编码四色**

`initDonut` 的 `setOption` 里目前是写死的 4 色数组，真实数据有 8–9 个模型会循环撞色：

```javascript
      color:['#6f96ff','#7ea2ff','#2fd489','#b197f7'],
```

替换为：

```javascript
      color:[0,1,2,3,4].map(seriesColor),
```

- [ ] **Step 7: 构建并在浏览器验证**

```bash
export GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
go build -o /tmp/oc2api-verify . && echo "构建 OK"
```

重启实例（`job_kill` 旧任务后按 Task 2 Step 3 的命令重启），打开 `http://127.0.0.1:8899/`：

1. 概览页趋势图三条线应为 **蓝 / 绿 / 紫**（`--series-1/2/3`），不再是蓝/绿/紫的旧 `--blue/--ok/--purple`
2. 切到「请求数」tab，两条线为 **绿 / 红**
3. 甜甜圈每个扇区颜色互不相同（8 个模型 → 前 5 个用色板，后 3 个循环用前 3 色）
4. 点右上角主题切换到亮色，图表颜色**变深**而不是变成浅色不可读

- [ ] **Step 8: 提交**

```bash
git add static/admin.html
git commit -m "style(admin): 引入 --series-* 数据系列色板并改写图表取色

main 的四张图此前从 --blue/--ok/--purple 取色，甜甜圈更是写死 4 色数组，
真实数据有 8-9 个模型会循环撞色。新增 seriesColor(i) 统一从
--series-1..5 取值（超出取模），暗色与亮色主题各一套。

沿用 test 分支 2a29a37 确立的色板值。"
```

---

### Task 4: 概览统计卡合并（保留 main 三卡 + 增加四格明细）

**Files:**
- Modify: `static/admin.html`（概览页统计卡区块的 HTML、相关 CSS、渲染函数）

**Interfaces:**
- Consumes: Task 3 的 `seriesColor`
- Produces: 4 个新元素 `#gIn` `#gOut` `#gCR` `#gCW`，由 `renderHeroDetail()` 填充；`renderHeroDetail(stats)` 在 `renderStats(d)` 末尾被调用，参数为 `/api/stats` 返回的 `TokenStatsData`。

---

- [ ] **Step 1: 记录插入点**

```bash
grep -n 'tcCacheBar\|tcAvgTps\|id="tcTotalToken"\|id="tcTotalReq"' static/admin.html
```

记下统计卡区块的起止行号，下一步要在这个区块之后插入四格。

- [ ] **Step 2: 插入四格 HTML**

在 Step 1 找到的统计卡区块**闭合 `</div>` 之后**、下一张 `.card` 之前插入：

```html
    <div class="card hero-stats-card" style="margin-top:16px">
      <div class="hs-grid">
        <div class="hs-cell"><div class="hs-label">输入</div><div class="hs-val" id="gIn">—</div><div class="hs-bar"><i id="gInBar" style="background:var(--series-1)"></i></div></div>
        <div class="hs-cell"><div class="hs-label">输出</div><div class="hs-val" id="gOut">—</div><div class="hs-bar"><i id="gOutBar" style="background:var(--series-2)"></i></div></div>
        <div class="hs-cell"><div class="hs-label">缓存读</div><div class="hs-val" id="gCR">—</div><div class="hs-bar"><i id="gCRBar" style="background:var(--series-3)"></i></div></div>
        <div class="hs-cell"><div class="hs-label">缓存写</div><div class="hs-val" id="gCW">—</div><div class="hs-bar"><i id="gCWBar" style="background:var(--series-4)"></i></div></div>
      </div>
    </div>
```

- [ ] **Step 3: 插入四格 CSS**

在 `<style>` 块内、`.stats-row` 规则之后插入：

```css
.hs-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px}
.hs-cell{background:var(--surface-2);border:1px solid var(--border);border-radius:var(--radius-sm);padding:14px 16px}
.hs-label{font-size:11.5px;color:var(--text-ter);margin-bottom:6px;letter-spacing:.02em}
.hs-val{font-size:21px;font-weight:700;font-family:var(--mono);color:var(--text);line-height:1.2}
.hs-bar{height:4px;border-radius:2px;background:var(--line);margin-top:10px;overflow:hidden}
.hs-bar>i{display:block;height:100%;width:0;border-radius:2px;transition:width .3s}
@media (max-width:760px){.hs-grid{grid-template-columns:repeat(2,1fr)}}
```

- [ ] **Step 4: 新增 renderHeroDetail**

在 `function renderStats(d){...}` 之后插入：

```javascript
// renderHeroDetail 把 /api/stats 的各模型累计值汇总成四格明细。
// 口径：输入=prompt_tokens，输出=completion_tokens，缓存读/写取对应字段。
// 注意此处是全时段累计值，与统计表的 token 列（时段值）口径不同，沿用 main 既有做法。
function renderHeroDetail(d){
  const models=(d&&d.models)||{};
  const sum={in:0,out:0,cr:0,cw:0};
  for(const k in models){
    const m=models[k];
    sum.in+=m.prompt_tokens||0;sum.out+=m.completion_tokens||0;
    sum.cr+=m.cache_read_tokens||0;sum.cw+=m.cache_created_tokens||0;
  }
  const max=Math.max(sum.in,sum.out,sum.cr,sum.cw,1);
  const set=(id,v)=>{const el=q(id);if(el)el.textContent=fmtFull(v);const bar=q(id+'Bar');if(bar)bar.style.width=Math.round(v/max*100)+'%'};
  set('#gIn',sum.in);set('#gOut',sum.out);set('#gCR',sum.cr);set('#gCW',sum.cw);
}
```

- [ ] **Step 5: 在 renderStats 末尾调用**

在 `renderStats` 函数体的最后一行之后（函数闭合 `}` 之前）加：

```javascript
  renderHeroDetail(d);
```

- [ ] **Step 6: 构建并验证**

```bash
export GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
go build -o /tmp/oc2api-verify . && echo "构建 OK"
```

重启实例，打开概览页确认：

1. 原有三卡（总 Token / 请求数 / 缓存命中率）**仍在**，数值非零
2. 其下新增四格「输入 / 输出 / 缓存读 / 缓存写」，四条进度条长度按数值比例分配
3. 与 `curl -s http://127.0.0.1:8899/api/stats` 的合计一致：

```bash
curl -s http://127.0.0.1:8899/api/stats | python3 -c "
import json,sys
d=json.load(sys.stdin); ms=d['models']
print('输入  :', f\"{sum(m['prompt_tokens'] for m in ms.values()):,}\")
print('输出  :', f\"{sum(m['completion_tokens'] for m in ms.values()):,}\")
print('缓存读:', f\"{sum(m.get('cache_read_tokens',0) for m in ms.values()):,}\")
print('缓存写:', f\"{sum(m.get('cache_created_tokens',0) for m in ms.values()):,}\")
"
```

页面上四格的数字应与之一致。

- [ ] **Step 7: 提交**

```bash
git add static/admin.html
git commit -m "feat(admin): 概览页增加输入/输出/缓存读/缓存写四格明细

保留 main 原有的总 Token / 请求数 / 缓存命中率三卡，在其下增加 test 设计
里的四格模型汇总。数据来自 /api/stats 的各模型累计值。

注意口径：四格与统计表的 TTFT/速度列同为全时段累计值，而统计表的 token
列为时段值。这是沿用 main 的既有做法，本次不统一（统一需在后端按模型
分组的桶里同时聚合 ttft_ms / output_speed，属独立改动）。"
```

---

### Task 5: 筛选下拉（模型 + 时间范围）与趋势图联动

**Files:**
- Modify: `static/admin.html`（趋势卡区块 HTML、CSS、`setTrendRange`、`loadTrends`、`initTrend`、`initReqTrend`、`initDonut`、`renderStats`、`renderStatsTable`）

**Interfaces:**
- Consumes: Task 1 的 `GET /api/stats/trends?range=<r>&group=model`；Task 3 的 `seriesColor`；Task 4 的 `renderHeroDetail`
- Produces:
  - 全局状态 `var trendModel='all'`
  - `var TREND_BY_MODEL=[]` — 形如 `[{ts, model, requests, ok, fail, prompt_tokens, completion_tokens, cache_creation_tokens, cache_read_tokens}]`
  - `var trendSeq=0` — 请求序号，用于竞态防护
  - `function aggTrend() []TrendsPoint` — 按当前 `trendModel` 把 `TREND_BY_MODEL` 聚合成与原 `trendsData` 同构的数组，写入并返回 `trendsData`
  - `function buildModelOptions()` — 生成模型下拉的 `<option>`
  - `function setTrendModel(m string)` — 设置模型筛选并重绘联动视图
  - `function setTrendRange(range string)` — 替代原先的 `setTrendRange(days, btn)`，参数直接是 `'today'|'7d'|'30d'|'180d'`

---

- [ ] **Step 1: 定位趋势卡区块**

```bash
grep -n 'setTrendRange(1,this)\|setTrendRange(7,this)\|setTrendRange(30,this)\|switchTrendTab' static/admin.html
```

记下这些按钮所在的行号，Step 2 要整块替换。

- [ ] **Step 2: 替换时间范围按钮为下拉**

把 Step 1 找到的三个 `setTrendRange(1|7|30, this)` 按钮连同其容器，整体替换为：

```html
          <div class="filter-bar">
            <select id="trendRangeSel" class="input-sm" onchange="setTrendRange(this.value)">
              <option value="today">当天</option>
              <option value="7d">最近 7 天</option>
              <option value="30d" selected>最近 30 天</option>
              <option value="180d">最近半年</option>
            </select>
            <select id="trendModelSel" class="input-sm" onchange="setTrendModel(this.value)">
              <option value="all">全部模型</option>
            </select>
          </div>
```

- [ ] **Step 3: 插入 CSS**

在 `<style>` 块内追加：

```css
.filter-bar{display:flex;gap:8px;flex-wrap:wrap;align-items:center}
.input-sm{background:var(--surface-2);border:1px solid var(--border);border-radius:var(--radius-sm);color:var(--text);font-size:12px;padding:5px 9px;outline:none;cursor:pointer}
.input-sm:hover{border-color:var(--border-strong)}
.input-sm:focus{border-color:var(--accent)}
```

- [ ] **Step 4: 声明全局状态**

在 `let trendsData=[],trendRange='30d',trendTimer=null,heatmapData=[];` 这一行之后插入：

```javascript
/* 按模型分组的趋势原始数据 + 竞态防护序号 */
let TREND_BY_MODEL=[],trendModel='all',trendSeq=0;
```

- [ ] **Step 5: 替换 loadTrends**

把 `async function loadTrends(){...}` 整个函数替换为：

```javascript
async function loadTrends(){
  const mySeq=++trendSeq;
  try{
    const r=await fetch('/api/stats/trends?range='+trendRange+'&group=model');
    if(!r.ok)throw new Error('HTTP '+r.status);
    const d=await r.json();
    if(mySeq!==trendSeq)return;           // 过期响应，丢弃
    TREND_BY_MODEL=Array.isArray(d)?d:[];
  }catch(e){
    if(mySeq!==trendSeq)return;
    TREND_BY_MODEL=[];
  }
  aggTrend();
  renderModelOptions();
  renderTrends();
}
```

- [ ] **Step 6: 新增 aggTrend / setTrendModel / renderModelOptions**

在 `loadTrends` 之后插入：

```javascript
/* 按当前模型筛选把按模型分组的数据聚合成原 trendsData 的结构。
   trendModel==='all' 时跨模型求和；否则只取该模型。 */
function aggTrend(){
  const byTs=new Map();
  for(const r of TREND_BY_MODEL){
    if(trendModel!=='all'&&r.model!==trendModel)continue;
    let t=byTs.get(r.ts);
    if(!t){t={ts:r.ts,requests:0,ok:0,fail:0,prompt_tokens:0,completion_tokens:0,cache_creation_tokens:0,cache_read_tokens:0};byTs.set(r.ts,t)}
    t.requests+=r.requests||0;t.ok+=r.ok||0;t.fail+=r.fail||0;
    t.prompt_tokens+=r.prompt_tokens||0;t.completion_tokens+=r.completion_tokens||0;
    t.cache_creation_tokens+=r.cache_creation_tokens||0;t.cache_read_tokens+=r.cache_read_tokens||0;
  }
  trendsData=[...byTs.values()].sort((a,b)=>a.ts<b.ts?-1:1);
  return trendsData;
}

function setTrendModel(m){
  trendModel=m||'all';
  aggTrend();
  renderTrends();
  renderStats();
  renderDonut();
  renderHeroDetailForFilter();
}

/* 模型下拉选项：优先取 /api/stats 中真实产生过请求的模型（renderStats 缓存的
   __lastStatsModels）；全新实例无用量时回退到 /api/models 的 free_models。 */
function renderModelOptions(){
  const sel=q('#trendModelSel');if(!sel)return;
  let models=(window.__lastStatsModels||[]).slice();
  if(!models.length&&typeof freeModelIDs!=='undefined'&&freeModelIDs)models=freeModelIDs.slice();
  sel.innerHTML='<option value="all">全部模型</option>'
    +models.map(m=>'<option value="'+esc(m)+'">'+esc(m)+'</option>').join('');
  if(models.indexOf(trendModel)<0)trendModel='all';
  sel.value=trendModel;
}
```

- [ ] **Step 7: 在 renderStats 中缓存模型键供下拉使用**

在 `function renderStats(d){...}` 内找到给统计表赋值的行之后，函数闭合 `}` 之前加入：

```javascript
  window.__lastStatsModels=Object.keys((d&&d.models)||{});
```

- [ ] **Step 8: 补 renderHeroDetailForFilter**

Task 4 的 `renderHeroDetail(d)` 接受完整的 `TokenStatsData`。模型筛选后需要只统计选中模型，新建：

```javascript
/* 模型筛选后的四格：复用 renderHeroDetail 的求和逻辑，但只取选中的模型。 */
function renderHeroDetailForFilter(){
  const all=window.__lastStats||{};
  if(trendModel==='all'||!all.models){renderHeroDetail(all);return}
  renderHeroDetail({models:{[trendModel]:all.models[trendModel]||{}}});
}
```

并把 Task 4 Step 5 加入 `renderStats` 末尾的那一行，从 `renderHeroDetail(d);` 改为：

```javascript
  window.__lastStats=d;
  renderModelOptions();      // loadStats 与 loadTrends 并发返回，先后不定，双触发自愈
  renderHeroDetailForFilter();
```

- [ ] **Step 9: 替换 setTrendRange**

把 `function setTrendRange(days,btn){...}` 整个函数替换为：

```javascript
function setTrendRange(range){
  trendRange=range;
  currentTrendDays=(range==='today')?24:parseInt(range,10)||30;
  loadTrends();
}
```

- [ ] **Step 10: 构建并验证**

```bash
export GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
go build -o /tmp/oc2api-verify . && echo "构建 OK"
```

重启实例，打开概览页确认：

1. 趋势卡顶部出现两个下拉：**时间范围**（当天/7/30/半年，默认最近 30 天）与**模型**（全部模型 + 8 个真实模型名）
2. 默认「全部模型」时趋势图与 Task 2 基线**数值一致**（验证聚合正确）
3. 选「claude-sonnet-4」→ 趋势图数值下降，甜甜圈只剩一个扇区，统计表只剩一行，四格数值同步变化
4. 切回「全部模型」→ 全部恢复
5. 时间切到「当天」→ 曲线变为 24 个小时点；切到「最近半年」→ 180 个日点
6. **竞态测试**：快速连点「当天 → 7 天 → 30 天 → 180 天」，最终显示的必须是 180 天的曲线（`x` 轴有 180 个刻度）

- [ ] **Step 11: 提交**

```bash
git add static/admin.html
git commit -m "feat(admin): 新增模型与时间范围筛选下拉，联动趋势图与概览统计

main 原有的时间范围只有 1/7/30 三个按钮且无模型维度（趋势为聚合三线）。
本任务改为下拉并补 180d，新增模型下拉：

- 趋势数据改为始终拉 ?group=model，前端按当前模型聚合（全部=跨模型求和）
- 聚合逻辑同时供 initTrend（Token）与 initReqTrend（请求数）两个 tab 使用
- 甜甜圈、统计表、概览四格随模型筛选联动
- 模型选项取 /api/stats 的 models 键，全新实例无用量时回退 /api/models
  的 free_models
- loadTrends 引入请求序号，快速连点时间范围时丢弃过期响应

零桶策略由后端保证：每个有数据的模型都会返回完整区间的全部桶，
因此低频模型的折线不会断。"
```

---

### Task 6: ECharts 加载失败提示

**Files:**
- Modify: `static/admin.html`（四张图的 init 函数、新增 `chartLoadFail` 辅助）

**Interfaces:**
- Consumes: 无
- Produces: `function chartLoadFail(chartId, label)` — 在指定图表容器内显示加载失败提示。四个 init 函数在 `typeof echarts==='undefined'` 时调用它而非静默 return。

---

- [ ] **Step 1: 新增失败提示函数**

在 `function initAllCharts(){...}` 之前插入：

```javascript
/* CDN 不可用时在图表容器内显示可见提示，而不是静默留白。
   静默 return 会让用户误以为"没有数据"。 */
function chartLoadFail(chartId,label){
  const el=q(chartId);if(!el)return;
  el.innerHTML='<div style="display:flex;align-items:center;justify-content:center;height:100%;min-height:120px;text-align:center;color:var(--text-ter);font-size:12.5px;padding:20px">'
    +'<div><div style="font-size:22px;margin-bottom:8px">📉</div>'
    +label+' 加载失败<br><span style="font-size:11.5px;opacity:.75">ECharts 未能从 CDN 加载，请检查网络或改用本地资源</span></div></div>';
  console.warn('ECharts 加载失败，'+label+' 无法渲染');
}
```

- [ ] **Step 2: 替换四处的静默 return**

把以下四处（`initTrend`、`initReqTrend`、`initHeatmap`、`initDonut`）中形如

```javascript
  const el=q('#trendChart');if(!el||typeof echarts==='undefined')return;
```

的守卫，改为**先判容器、再判 echarts**：

```javascript
  const el=q('#trendChart');if(!el)return;
  if(typeof echarts==='undefined'){chartLoadFail('#trendChart','趋势图');return}
```

四处分别对应的容器与文案：

| 函数 | 容器 | 文案 |
| --- | --- | --- |
| `initTrend` | `#trendChart` | 趋势图 |
| `initReqTrend` | `#reqTrendChart` | 请求数趋势图 |
| `initHeatmap` | `#heatmapChart` | 请求热力图 |
| `initDonut` | `#donutChart` | 模型请求分布 |

- [ ] **Step 3: 构建并验证**

```bash
export GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
go build -o /tmp/oc2api-verify . && echo "构建 OK"
```

重启实例，浏览器打开开发者工具 → Network → 把 `cdn.jsdelivr.net` 设为 **block**，刷新页面。

预期：四张图各自显示「📉 趋势图 加载失败 / ECharts 未能从 CDN 加载…」，**不是空白**；Console 有 4 条 `ECharts 加载失败` 警告。取消 block 后刷新恢复正常。

- [ ] **Step 4: 提交**

```bash
git add static/admin.html
git commit -m "fix(admin): ECharts 加载失败时显示提示而非静默留白

四张图的 init 函数此前均为 if(typeof echarts==='undefined')return;，
CDN 不可用时页面只呈现空白，用户会误判为「没有数据」。改为在容器内
显示可见提示并输出 console.warn。"
```

---

### Task 7: 布局与卡片样式对齐

**Files:**
- Modify: `static/admin.html`（`<style>` 块）
- Create: `docs/admin-layout-port.md`（移植记录：逐条列出移植/未移植的规则及理由）

**Interfaces:**
- Consumes: Task 3 的 `--series-*` 色板、Task 4 的 `.hs-*` 类、Task 5 的 `.filter-bar` / `.input-sm` 类
- Produces: 一份可复核的布局移植记录；`static/admin.html` 的 `<style>` 块与 test 版本的布局规则对齐。

**背景与边界**：test 版 `<style>` 619 行、main 版 350 行，其中约 270 行是 test 独有的或改写过的。但其中一部分是 Task 3–5 已处理的内容（色板变量、`.hs-grid`、`.filter-bar`、`.input-sm`）。本任务只处理**针对两边共有的容器类的布局与卡片外观规则**。

---

- [ ] **Step 1: 提取两边的共有容器类清单**

```bash
# main 侧（当前工作区）
awk '/<style>/,/<\/style>/' static/admin.html | grep -oE '^\.[a-zA-Z][a-zA-Z0-9_-]*' | sort -u > /tmp/cls_main.txt
# test 侧
awk '/<style>/,/<\/style>/' /tmp/admin-test-visual.html | grep -oE '^\.[a-zA-Z][a-zA-Z0-9_-]*' | sort -u > /tmp/cls_test.txt
comm -12 /tmp/cls_main.txt /tmp/cls_test.txt | tee /tmp/cls_shared.txt | wc -l
```

Expected: 输出一列共有类名与数量。**这份清单就是本任务的处理范围**——只在这些类上做文章，不引入 test 独有的类（那些要么已由 Task 3–5 处理，要么属于 test 独有的组件，本就不该移植）。

- [ ] **Step 2: 找出共有类中定义不同的规则**

```bash
# 逐个类比对两份 CSS 中的定义
while read -r c; do
  a=$(awk -v c="$c" '/<style>/,/<\/style>/' static/admin.html | grep -A6 "^\Q$c\E" | md5sum | cut -c1-8)
  b=$(awk -v c="$c" '/<style>/,/<\/style>/' /tmp/admin-test-visual.html | grep -A6 "^\Q$c\E" | md5sum | cut -c1-8)
  [ "$a" != "$b" ] && echo "$c"
done < /tmp/cls_shared.txt | tee /tmp/cls_diff.txt | wc -l
```

Expected: 列出定义不同的共有类。**逐条审阅 `/tmp/cls_diff.txt`**，对每一条决定移植或不移植。

- [ ] **Step 3: 逐条决策并记录**

对 `/tmp/cls_diff.txt` 中每一个类，写下决策与理由，写入 `docs/admin-layout-port.md`。决策规则：

| 情况 | 决策 |
| --- | --- |
| 仅涉及颜色值，且颜色在 test 里已用 `--series-*` 或共享令牌表达 | 移植（但若 Task 3 已用 `seriesColor` 在 JS 侧取色，则此处的颜色规则不移植，避免两处取色冲突） |
| 布局/间距/圆角/边框/字号等非颜色属性 | 移植 |
| test 独有组件的样式（`.hm-*`、`.donut-*`、`.hs-*`、`.filter-bar`、`.input-sm`） | **不移植**（`.hs-*`/`.filter-bar`/`.input-sm` 已由 Task 4/5 引入；`.hm-*`/`.donut-*` 属 ECharts 容器，test 的 CSS 实现不适用） |
| 依赖 test 独有 DOM 结构的选择器（如 `.hero-stats-card` 下的子元素） | 不移植（DOM 不在） |
| test 为移动端新增的 media query | 移植（纯 CSS，无 DOM 依赖） |

记录格式，每条一行：

```markdown
| 类名 | 决策 | 理由 |
| --- | --- | --- |
| `.card` | 移植 | 圆角与阴影为纯样式，DOM 结构两边一致 |
| `.hm-cell` | 不移植 | test 的 CSS 热力图实现，本次保留 main 的 ECharts 版 |
```

- [ ] **Step 4: 执行移植**

按 Step 3 的决策表，把「移植」的行对应的 CSS 从 `/tmp/admin-test-visual.html` 复制进 `static/admin.html` 的 `<style>` 块。

每移植 5–8 条后构建并刷新浏览器看一眼，避免一次性堆太多导致难以定位回归。

- [ ] **Step 5: 构建并逐页回归**

```bash
export GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
go build -o /tmp/oc2api-verify . && echo "构建 OK"
```

重启实例，逐页检查五个页面**没有出现**：文字溢出容器、表格错位、卡片高度塌陷、深色模式下文字与背景对比度不足。亮色主题同样过一遍。

- [ ] **Step 6: 提交**

```bash
git add static/admin.html docs/admin-layout-port.md
git commit -m "style(admin): 对齐 test 版的布局与卡片样式

在 --series-* 色板、概览四格、筛选下拉之后，补齐 test 重设计中针对共有
容器类的布局规则（间距/圆角/边框/字号/响应式），不动颜色——颜色已由
seriesColor 统一从 --series-* 取值，两处取色会冲突。

test 独有的 CSS 组件（.hm-*、.donut-*）不移植：本次保留 main 的
ECharts 渲染器，其 CSS 实现不适用。逐条决策见 docs/admin-layout-port.md。"
```

---

### Task 8: 全量验证

**Files:**
- 无代码改动。本任务只做验证与记录。

**Interfaces:**
- Consumes: Task 1–6 的全部产出
- Produces: 一份可复核的验证记录

---

- [ ] **Step 1: 代码质量门禁**

```bash
export GOCACHE="$PWD/.gocache" GOMODCACHE="$PWD/.modcache" GOTMPDIR="$PWD/.gotmp"
gofmt -l . 2>/dev/null | grep -v '^\.modcache\|^\.gocache' && echo "↑ 有未格式化文件" || echo "gofmt 通过 ✅"
go vet ./... && echo "go vet 通过 ✅"
go test ./... 2>&1 | tail -5
go build -o /tmp/oc2api-verify . && echo "构建通过 ✅"
```

- [ ] **Step 2: 后端向后兼容回归**

重跑 Task 1 Step 9 的三条命令，确认逐桶求和仍等于聚合值、聚合响应无 `model` 字段：

```bash
curl -s 'http://127.0.0.1:8899/api/stats/trends?range=7d' > /tmp/agg.json
curl -s 'http://127.0.0.1:8899/api/stats/trends?range=7d&group=model' > /tmp/per.json
python3 -c "
import json
agg=json.load(open('/tmp/agg.json')); per=json.load(open('/tmp/per.json'))
assert 'model' not in agg[0], '聚合响应不应含 model 字段'
per_map={}
for r in per:
    s=per_map.setdefault(r['ts'],{k:0 for k in agg[0] if k!='ts'})
    for k in s: s[k]+=r.get(k,0)
bad=[t['ts'] for t in agg if any(t[k]!=per_map[t['ts']][k] for k in per_map[t['ts']])]
print('聚合响应无 model 字段 ✅' if 'model' not in agg[0] else '❌ 聚合响应被污染')
print('逐桶求和不一致的桶:', bad if bad else '无 ✅')
"
```

- [ ] **Step 3: 逐项核对 spec 验证清单**

重启实例后打开 `http://127.0.0.1:8899/`，逐条记录结果：

| # | 验证项 | 结果 |
| --- | --- | --- |
| 1 | 概览统计、趋势图、甜甜圈、模型明细表、热力图均显示真实数据 | |
| 2 | 节点表、订阅表显示空态提示而非 DEMO 假数据（`hk-01` / `主订阅` 均不应出现） | |
| 3 | 运行日志页可建立 SSE 连接 | |
| 4 | 调用日志列表有真实记录 | |
| 5 | 模型下拉切换 → 趋势图（**两个 tab**）/ 甜甜圈 / 明细表 / 四格全部联动 | |
| 6 | 时间下拉切换 → 刷新且无竞态（快速连点 4 项后停在最后一项） | |
| 7 | 仅在少数几天有流量的模型，其趋势线仍覆盖完整区间（验证零桶） | |
| 8 | 订阅 / 推理档位 / 别名 / SOCKS5 增删改后 `.uidemo/config.json` 实际变更 | |
| 9 | 节点「切换 / 解除」按钮存在（无节点时表格为空属正常） | |
| 10 | 断网刷新 → 出现 ECharts 加载失败提示条 | |

- [ ] **Step 4: 确认无残留死代码**

```bash
# 这些 test 独有的东西不应出现在当前文件里
grep -cE 'DEMO_MODE|DEMO_NODES|DEMO_STATS|DEMO_CAPS|DEMO_CALL_LOG|DEMO_LOGS|TREND_MODELS|MODEL_META|tokensPerRequest|getFilteredStats|getRangeRows|genTodayHourly|genDailyData|genApiKey|loginView|newKeyDialog|2026,8,22' static/admin.html
```

Expected: `0`

- [ ] **Step 5: 提交验证记录（若有文档更新）**

若本轮验证发现需要补充文档（例如实测差异），改完后：

```bash
git add -A
git commit -m "docs(admin): 补充视觉层移植的验证记录"
```

若无需改动则跳过本步。
