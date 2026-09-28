# 管理面板视觉层移植 — 设计文档

- **日期**：2026-09-28
- **分支**：`test`
- **合并基点**：`dca9d43`（test 已合入 main 的 4 个提交）
- **状态**：待实现

---

## 1. 问题陈述

`test` 分支的 `static/admin.html`（2025 行）是一次纯静态高保真重做，与 `main`（1520 行）相比丢失了全部后端连接能力：

| 检查项 | `main` | `test` |
| --- | ---: | ---: |
| `fetch(` 出现次数 | 19 | **1** |
| `/api/` 出现次数 | 21 | **1**（仅探活） |
| `method:'POST'` 次数 | 6 | **0** |
| `JSON.parse` / `.json()` | 有 | **0** |
| ECharts | 有（16 处） | 无（`ECharts removed: CSS-only charts in use`） |
| 趋势数据基准日 | 滚动 | 写死 `new Date(2026,8,22)`（3 处） |

全文唯一网络请求：

```js
// static/admin.html:1867
fetch('/api/config').then(function(r){if(r.ok)DEMO_MODE=false})
```

该请求**只用于探测后端存活**以翻转 `DEMO_MODE` 标志，不取回任何数据。所有渲染函数（`loadAll()` 及下游 12 个 `render*`）只读硬编码常量 `DEMO_NODES` / `DEMO_STATS` / `DEMO_CAPS` / `DEMO_CALL_LOG` / `DEMO_LOGS` / `TREND_HOURLY` / `TREND_DAILY`。

**为什么 Docker 部署显示正常**：`docker-compose.example.yml` 使用 `image: ghcr.io/shiyanx/opencode2api:latest`（拉取远端预构建镜像，不构建本地代码），而 `.github/workflows/docker.yml` 的触发条件是 `push: branches: [main]` 或 `tags: ["v*"]`。test 分支的 push 永不触发镜像构建，因此 test 的新 UI 从未进入任何镜像。

**目标**：让 test 分支的 admin 面板既保有新视觉设计，又接回真实后端数据与全部配置管理能力。

---

## 2. 已核实的关键事实

以下均经实测确认，是本设计的依据。

### 2.1 DOM 骨架高度重叠

```
test 86 个元素 id，main 108 个，共有 51 个 → test 的 59% 在 main 中已存在
```

共有的骨架：`page-overview` `page-nodes` `page-subs` `page-misc` `page-logs` `nav` `view-app` `sideAddr` `pageTitle` `pageCrumb`；所有表格容器 `nodeTable` `subTable` `socks5Table` `capTable` `statsTable` `aliasTable` `effortTable` `logList` `clList`；节点统计 `ovTotal` `ovHealthy` `ovExhausted` `ovDead` `ovActive` `ovManual`。

### 2.2 同一套设计令牌

两分支的 CSS 变量逐值相同：`--bg:#0a0f1c` `--surface:#101828` `--surface-2:#1a2336` `--accent:#6f96ff` `--text-ter:#7b87a5` `--ok:#2fd489` 等。

差异仅两处：

- test 独有：`--series-1` … `--series-5`
- main 独有：`--font` `--mono` `--shadow` `--btn-text`

test 的重设计不是外来设计，是同一套设计系统的演进。

### 2.3 main 的图表已是令牌驱动

```js
// main:836 initTrend
const bC = getCSS('--blue') || '#7ea2ff';
const oC = getCSS('--ok')   || '#2fd489';
const pC = getCSS('--purple') || '#b197f7';
```

test 的 `--series-*` 调色板可直接替换这套取色路径。

### 2.4 同一张卡片的不同渲染器

```html
<!-- test:774 -->
<div id="chartHeatmap" class="card">
  <div class="heatmap-title">请求热力图 (近半年)</div>
  <div class="css-heatmap">175 个字面量格子</div>
</div>

<!-- main:426 -->
<div id="chartHeatmap" class="card">
  <div>📅 请求热力图 (近半年)</div>
  <div id="heatmapChart" class="chart-box"></div>
</div>
```

同 id、同 class、同标题文案。标题「近半年」对应 main 的 `range=180d`，本就正确。

### 2.5 部署环境可访问 ECharts CDN

```
宿主机:  HTTP 200  1.03 MB  1.3s
容器内:  可访问（wget 成功）
```

### 2.6 接口响应形状（已逐个核对）

| 接口 | 返回 |
| --- | --- |
| `/api/nodes` | `{nodes:[nodeAdminView×13字段], subscriptions:[SubscriptionConfig], healthy, exhausted_count, dead_count, active_name, manual_name}` |
| `/api/config` | `AppConfig`：`model_alias` `reasoning_effort_map` `socks5_proxies` `active_socks5` `webshare` `quota_error_signals` 等 |
| `/api/stats` | `{total_requests, models:{[model]:{request_count, prompt_tokens, completion_tokens, total_tokens, cache_read_tokens, cache_created_tokens, avg_ttft_ms, avg_output_speed, stream_req_count}}}` |
| `/api/stats/trends?range=` | `[TrendsPoint]`：`{ts, requests, ok, fail, prompt_tokens, completion_tokens, cache_creation_tokens, cache_read_tokens}`；range 白名单 `today\|7d\|30d\|180d` |
| `/api/call-log` | `CallRecord[]`：`{req_id, ts, path, model, stream, route_mode, nodes, node_names, events, status, prompt_tokens, completion_tokens, cache_creation_tokens, cache_read_tokens, duration_ms, ttft_ms, output_speed, err_msg}` |
| `/api/logs` | `{entries:[{seq, time, level, msg, attrs:[{k,v}]}]}`，time 为纳秒精度 RFC3339 |
| `/api/models` | `{data:[…], free_models:[…], object}`；实测 98 个模型、10 个 free |

`nodeAdminView`：`{name, protocol, address, port, fingerprint, state, marked_at, cooldown_until, last_error, last_used_at, latency_ms, last_probe_at, active, manual}`

`SubscriptionConfig`：**仅 3 字段** `{name, url, update_interval_hours}`

### 2.7 test 分支的已知缺口清单

| 项 | 状态 |
| --- | --- |
| `loadAll()` + 12 个 `render*` | 全部只读 DEMO 常量 |
| 6 个 POST（配置保存、节点操作、配额、模型刷新） | 全部缺失 |
| 4 组配置编辑器（订阅/推理档位/别名/SOCKS5） | 渲染的是带 `<input>` 和「删除」按钮的表单，无收集与保存逻辑 |
| 配额设置、Webshare 代理池、手动节点管理 | 整页缺失 |
| SSE 日志流（`/api/logs/stream`） | 缺失，main 有 `EventSource` + 断线重连 |
| 模型筛选 | 缺失（main 无此概念） |
| `genApiKey()` | **前端随机造密钥**，后端无创建密钥接口（多 Key 管理已由 `30bff23` revert） |
| 前端登录 SPA（`loginView` 等） | 与服务端 `/login` 并存会冲突 |
| 硬编码日期 `new Date(2026,8,22)` | 3 处：`genDailyData:1182`、`getRangeRows:1520`、热力图 tooltip `baseDate:1961` |
| cost 字段 | 5 处：`getFilteredStats`、`renderHeroStats`(heroCost)、趋势 tooltip、热力图 tooltip、模型明细表 |
| `MODEL_META` / `TREND_MODELS` | 双重硬编码 5 个模型；`renderDonut` 用 `TREND_MODELS.filter()` 会**静默丢弃**真实数据中的其余模型 |
| `tokensPerRequest()` | demo 产物，用 token 总量**反推**请求数，真实数据应直接用 `request_count` |
| 颜色只有 5 个 | `--series-1..5`，真实 8 模型会撞色 |

---

## 3. 方案选择

评估过三条路线：

| 路线 | 做法 | 结论 |
| --- | --- | --- |
| A. 重建功能层 | 在 test 的新 DOM 上重写 main 的全部数据层 + 4 组编辑器 + 配额/代理池 | 周期长，等于重写一个管理后台 |
| **B. 移植视觉层** | 以 main 为基底，把 test 的 CSS/布局/统计卡移植过去 | **选定** |
| C. 只接只读图表 | 仅接趋势图/概览/甜甜圈/统计表，配置页留作已知缺口 | 快速但页面一半是死的 |

**选定 B**，依据 2.1 / 2.2 / 2.3 / 2.4 的实测：DOM 骨架 59% 已有、设计令牌同源、图表已令牌驱动、卡片结构同构。移植成本远低于重建，且 main 的 12 个 `render*` 与 6 个 POST 是已验证可运行的代码。

---

## 4. 设计

### 4.1 基底

以 `git show main:static/admin.html` 为起点（保留全部 JS 与 POST），逐项移植 test 的视觉元素。

### 4.2 后端改动（1 处）

**`calllog.go`** — 新增按模型分组的趋势查询：

```
trendsByModelFromCallLog(rng string) []ModelTrendPoint
```

复用 `trendsFromCallLog` 的桶切分逻辑（`today` 24 个小时桶 / `7d` `30d` `180d` 逐日桶，含零数据时段），仅将 bucket key 从「时间」改为「(桶索引, model)」。

`ModelTrendPoint` 复用 `TrendsPoint` 全部字段并增加 `Model string \`json:"model"\``。

**零桶策略**：对每个「在本区间内出现过请求」的模型，输出该区间的**全部**桶（无数据的桶填零），而非只输出有数据的桶。理由：`trendsFromCallLog` 的既有注释写明「含零数据时段保证连线连续」，逐模型必须保持同样性质，否则某模型只在最近 3 天有流量时，趋势线会在其余 27 天断裂。响应规模因此为「有数据的模型数 × 桶数」（30d 时约 8×30=240 行），可接受。

**`main.go` `adminTrendsHandler`** — 读 `group` query 参数：

- `group=model` → 调用 `trendsByModelFromCallLog`
- 不带或非 `model` → 调用 `trendsFromCallLog`（**行为完全不变**，不影响已部署的容器）

**不做**：不补 `90d`、不补「全部时间」——时间下拉只提供后端已支持的 4 项。

### 4.3 前端改动

#### 保留（main 的功能层）

`loadAll()` / `loadConfig()` / `loadNodes()` / `loadStats()` / `loadCaps()` / `fetchModelList()` / `loadTrends()` / `loadHeatmap()` / `loadTps()` / `renderOverview()` / `renderStats()` / `renderCaps()` / `renderNodeTable()` / `renderLatencyDist()` / `renderSubTable()` / `renderCallLog` / `nodeAction()` / `collectSubs()` / `collectNodes()` / `collectQuota()` / `saveQuotaConfig()` / SSE 日志流 / 全部 6 个 POST

#### 移植（test 的视觉层）

| 项 | 做法 |
| --- | --- |
| CSS 令牌 | 引入 `--series-1` … `--series-5`；ECharts 取色由 `getCSS('--blue'/--ok'/--purple')` 改为 `getCSS('--series-N')` |
| 亮暗双主题 | 亮色主题下图表使用独立色板（test 的 `2a29a37` 已定义） |
| 概览统计 | 统计卡**合并**两版：保留 main 的「总 Token / 请求数 / 缓存命中率」（`tcTotalToken` `tcTotalReq` `tcCacheRate` + 进度条），并采用 test 的四格明细（`gIn` / `gOut` / `gCR` / `gCW` = 输入 / 输出 / 缓存读 / 缓存写）。四格数值由 `group=model` 数据按桶跨模型求和得出 |
| 趋势图外观 | 保留 ECharts 渲染，套用 test 的图例 guide dot 与配色。**main 的「Token / 请求数」双 tab（`switchTrendTab`）保留**——它是既有功能，不在本次移植范围 |
| 布局 | 619 行 CSS 中 main 没有的部分（`<style>` 块从 350 行扩到 ~620 行） |
| 筛选控件 | 新增 `filter-bar`，仅作用于趋势图与概览统计：模型下拉 + 时间范围下拉，取代 main 的 `setTrendRange(1/7/30)` 按钮组。**节点状态筛选 `stateChips` 不动**（属节点池页，与趋势无关） |

#### 不移植（test 独有的死代码）

`loginView` / `loginForm` / `loginPwd` / `loginBtn` / `loginMsg` / `newKeyDialog` / `newKeyValue`（登录走服务端 `/login`）、`genApiKey`、全部 `DEMO_*` 常量、`TREND_MODELS` / `MODEL_META` / `tokensPerRequest` / `getFilteredStats` / `getRangeRows` / `genTodayHourly` / `genDailyData`、3 处 `new Date(2026,8,22)`、5 处 cost

因基底是 main，上述函数在起点文件中本就不存在；此列表的作用是明确「移植时不要把它们带过来」。

#### 新增

**模型筛选联动**

- 模型下拉选项优先取 `/api/stats` 的 `models` 键（仅列出实际产生过请求的模型）；若 `models` 为空（全新实例无用量），回退到 `/api/models` 的 `free_models` 列表
- 趋势图始终拉 `group=model` 数据。`模型=全部` 时，前端**按桶把该桶内所有模型的 `prompt_tokens`、`completion_tokens`、`cache_read_tokens`、`cache_creation_tokens` 分别求和**，得到与现状一致的聚合结果；选具体模型时只取该模型的行
- **两个 tab 都要响应模型筛选**：`initTrend`（Token）与 `initReqTrend`（请求数，画 成功/失败 两条线）消费的是同一份 `trendsData`，因此共用同一套聚合逻辑。`initReqTrend` 聚合的是 `ok` / `fail` 两个字段
- Token tab 的「输入」序列沿用 main 的公式 `prompt_tokens - cache_read_tokens`。**该公式仅在 `prompt_tokens` 为「含缓存的输入总量」时成立**（OpenAI / DeepSeek 风格）。`usageFromMap` 对 Anthropic 走 `input_tokens` 分支，其语义是不含 `cache_read_input_tokens`，理论上会算出负值。实测生产数据无此问题：136,931 条成功记录（含 3,282 条 `/v1/messages`）中 `prompt < cache_read` 的为 0 条。本次**保持与 main 一致，不改公式**，仅在此记录该前提假设
- 甜甜圈、模型明细表随模型筛选联动（两处 main 本就按模型出数据）；概览四格随筛选重算
- 时间范围下拉提供 4 项：当天 / 7 天 / 30 天 / 180 天。替换 main 的 `setTrendRange(1/7/30)` 按钮组时需注意其内部只有 `1→today` / `7→7d` / 其余→`30d` 三分支，**缺 180d 分支**，新实现需显式处理

**ECharts 加载失败提示**

现有四处 init 均有 `if(typeof echarts==='undefined')return;`，CDN 失败时 4 张图静默空白。改为：加载失败时在图表区显示可见提示条（而非空白），并记录 console.warn。

### 4.4 数据流

```
/api/stats ──────────────┬─→ 模型下拉选项（models 键；为空时回退 /api/models 的 free_models）
                          └─→ 模型明细表的 TTFT / 速度两列（全时段累计值）

/api/stats/trends
  ?range=X&group=model ──→ TREND_BY_MODEL ─┬─→ 模型=全部：按桶跨模型求和 → 聚合三线
                                            └─→ 模型=X：取该模型的行 → 该模型三线
                          └─→ 概览四格（按桶跨模型求和）

/api/stats/trends
  ?range=180d ───────────→ 热力图（近半年，不受筛选影响）
```

`/api/stats` 是全时段累计值，`/api/stats/trends` 是时段值——模型明细表的 token 列走时段、TTFT/速度列走全时段，这是**沿用 main 的既有口径**，本次不统一（要统一需在后端按模型分组的桶里同时聚合 `ttft_ms` / `output_speed`，属独立改动）。

时间范围切换变为异步（现为同步读内存常量），需处理快速连点的竞态：每次切换递增请求序号，响应回来后仅当序号仍为最新才渲染。

### 4.5 错误处理

沿用 main 的既有模式：每个 loader 独立 `try/catch`，失败只在自己负责的表格/图表区显示「加载失败 + 重试按钮」，不影响其他区域。

---

## 5. 明确不做

| 项 | 理由 |
| --- | --- |
| cost / 金额显示 | 已决定本次不做；后端 `/api/stats/cost` 从未实现 |
| `90d` / 「全部时间」范围 | 后端不支持；`180d` 已覆盖「近半年」口径 |
| `group=model` 之外的接口变更 | 不破坏已部署容器的向后兼容 |
| SSE 日志流重写 | main 已有可用实现，原样保留 |
| 颜色超过 5 个系列 | 真实数据 8 模型时先扩 `--series-6..8`，按需追加 |
| 前端登录 SPA | 服务端 `/login` 已覆盖，留两套会冲突 |
| 多 API Key 管理 | 后端已由 `30bff23` revert，无对应接口 |

---

## 6. 验证

1. `gofmt -l` 无输出；`go build`（`CGO_ENABLED=0`）通过
2. **向后兼容回归**：改动前后各调用一次 `/api/stats/trends?range=30d`（不带 `group`），两次输出 `diff` 必须为空
3. `?range=30d&group=model` 返回按「时间×模型」分组的行；把同一时间桶内所有模型的各字段求和，结果须等于不带 `group` 时的同桶值
4. 实例启动后浏览器确认：概览统计、趋势图、甜甜圈、模型明细表、热力图、节点表、订阅表、日志、调用日志均显示真实数据
5. 模型下拉切换 → 趋势图（**Token 与请求数两个 tab**）/ 甜甜圈 / 明细表 / 概览四格全部联动
6. 时间下拉切换 → 数据刷新；快速连点 4 个范围后，最终显示的必须是最后点击的那一项（竞态验证）
7. 某模型仅在区间内少数几天有流量时，其趋势线仍覆盖完整区间（验证零桶策略）
8. 配置编辑器：订阅、推理档位、别名、SOCKS5 的增删改保存后，`config.json` 实际变更
9. 节点「切换 / 解除」按钮实际生效
10. 断网后刷新页面 → 出现 ECharts 加载失败提示条，而非空白

---

## 7. 风险

| 风险 | 缓解 |
| --- | --- |
| ECharts CDN 失效 | 已实测可达；补可见提示条，不再静默失败 |
| 真实模型数 > 5 撞色 | `--series-*` 可扩展，甜甜圈按 Top N + 「其他」聚合 |
| 时间切换竞态 | 请求序号防抖，过期响应丢弃 |
| 换皮回归 main 的既有交互 | 每移植一项即在浏览器验证，main 侧逻辑尽量不动 |
