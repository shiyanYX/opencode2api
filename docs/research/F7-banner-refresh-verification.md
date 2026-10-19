# F7 独立验收报告：面板告警横幅自动刷新（C-5 的 F7 缺口）

- 验收方：`verify-panel-f7`（独立验收，非实现者）
- 任务：`task-14`「面板告警横幅自动刷新（F7）独立验证」
- 日期：2026-10-19
- 被测分支 / 提交：`test` @ `ad699e7cd03a9354ed2f5c8b141e379f9d155526`
- 被测文件：`static/admin.html`（工作区修改版，md5 `c39ee71d09246f5666f6ced1f6584203`，`git diff --stat` = `31 insertions(+), 2 deletions(-)`）；
  后端契约另钉 `node_alert.go` md5 `4939852468fde3617c8de284867b6262`
- 报告：`docs/research/F7-banner-refresh-verification.md`

## 0. 结论（TL;DR）

**F7 成立：修复有效。** a)~e) 五条判据全部实测通过（真实 Chromium + 桩服务，全程零点击/零操作）；
f) 的 pageerror 为 0，console.error 在隔离掉两处**环境/桩噪声**后为 0，
且这两处噪声在**修复前**的 `admin.html`（`git show HEAD:static/admin.html`）上同样出现。

一句话理由：横幅在**没有任何用户操作**的前提下，按 ~30s 周期自己从「active」变到「隐藏」
（实测 29.99s），中途 `loadNodes()`/`renderNodeTable()` 调用次数均为 0、`<tr>` 标记未被冲掉，
`window_success_rate:null` 渲染为「无数据」而非 0%。

| 判据 | 结果 | 关键实测 |
| --- | --- | --- |
| a) 不点任何按钮横幅自己出现 | **PASS** | 导航开始后 **1.75s** `#nodeAlert` class = `node-alert on`；clicks = 0 |
| b) `#naMeta` 三段文案 | **PASS** | 含「连续失败 10 次」「跨 19.5 分钟」「池内可用 115 / 227」全为 true |
| c) active=false 后零操作自动消失 | **PASS** | 翻转后 **29 991 ms** 消失；等到 **31 001 ms** 复查仍未显示 |
| d) 轮询没有重画节点表 | **PASS** | `<tr data-marker="1">` 仍在；`renderNodeTable` 0 次、`loadNodes` 0 次 |
| e) `window_success_rate:null` → 「无数据」 | **PASS** | 文案含「无数据」，且不含 `0%` |
| f) pageerror / console.error = 0 | **PASS（限定）** | pageerror **0**；console.error：任务给定桩环境下 **6**（全部为桩/CDN 噪声，修复前版本同样出现），隔离后 **0** |
| 后端契约 `run_failures`/`run_minutes` | **PASS** | 9041 实例 `/api/nodes` → `"run_failures":0,"run_minutes":0`，类型断言全 PASS |
| 既有 DOM 钩子 `verify_ui.py` | **PASS** | `nojserror` 2/2、`elements` 13/13，退出码均 0 |

**未对 `static/admin.html` 做任何修改**（发现 0 个需要修的缺陷；环境噪声的归因见 §7.2~7.4，
非判据内的遗留观察见 §10）。

---

## 1. 复现环境与命令

```bash
cd /home/jingyx/project/opencode2api-test
export PATH=$PATH:/usr/local/go/bin GOCACHE=$PWD/.gocache GOMODCACHE=$PWD/.modcache GOTMPDIR=$PWD/.gotmp
git rev-parse HEAD      # ad699e7cd03a9354ed2f5c8b141e379f9d155526
git branch --show-current   # test
```

桩服务（task-14 指定，**未改动**）：`python3 /tmp/c5ui7/stub.py` → `127.0.0.1:9055`，
`/` 返回真实 `static/admin.html`，`/api/nodes` 读 `/tmp/c5ui7/alert.json`，`/api/config` → `{}`。

Playwright：`require('/home/jingyx/.npm/_npx/e41f203b7505f1fb/node_modules/playwright')`，
`executablePath:'/home/jingyx/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome'`，
`headless:true`，viewport 1280×900，**未做任何 `page.click()`**。

E2E 脚本：`/tmp/c5ui7/e2e_f7_v2.js`（a~f + a2 补充）；结果原始文件：

| 运行 | 桩 / 端口 | 说明 | 结果文件 |
| --- | --- | --- | --- |
| Run A（主证据，任务给定环境） | `/tmp/c5ui7/stub.py` @9055 | 原始桩，`/api/logs/stream` 返回 `application/json` | `/tmp/c5ui7/res_run_A_9055.json` |
| Run B | `/tmp/c5ui7/stub_x.py` @9056 | 仅把 `/api/logs/stream` 改成正确的 `text/event-stream` | `/tmp/c5ui7/res_run_B_9056_sse.json` |
| Run C | 同上 @9056 + 路由 CDN 占位 | 再隔离「本机连不上 jsdelivr」这一条噪声 | `/tmp/c5ui7/res_run_C_9056_clean.json` |
| Control（对照） | `/tmp/c5ui7/stub_x.py` @9057 | 服务**修复前**的 `git show HEAD:static/admin.html` | `/tmp/c5ui7/res_control_prefix.json` |

> Run B / C 的桩是我为了**归因 f)** 自己起的（`/tmp/c5ui7/stub_x.py`），
> 与 task-14 指定的 `stub.py` 行为一致，**只**改了 `/api/logs/stream` 的 MIME 与 CDN 路由。

---

## 2. 判据 a)：不做任何点击，横幅自己出现

命令：`BASE=http://127.0.0.1:9055/ node /tmp/c5ui7/e2e_f7_v2.js`
（页面 `goto` 后只 `waitForFunction`，无任何 click/keyboard/mouse 事件）

Run A 原始输出（`res_run_A_9055.json` 的 `a` 段，逐字）：

```json
"a": {
  "elapsed_ms_from_navigation_start": 1753,
  "nodeAlert_classes": "node-alert on",
  "page_nodes_active": false,
  "nodeAlert_visible_on_current_page": false,
  "default_active_page": ["page-overview"],
  "clicks_performed": 0,
  "error": null
}
```

Run C 复现（同一判据，环境更干净）：

```json
"a": {
  "elapsed_ms_from_navigation_start": 1632,
  "nodeAlert_classes": "node-alert on",
  "page_nodes_active": false,
  "nodeAlert_visible_on_current_page": false,
  "default_active_page": ["page-overview"],
  "clicks_performed": 0,
  "error": null
}
```

**结论：PASS。** 导航开始 1.6~1.8 秒内 `#nodeAlert` 已经带上 `.on`，全程 0 次点击。

**必须说清的边界（不算失败，但别误读）**：`#nodeAlert` 位于 `#page-nodes` 内，
而默认页是 `#page-overview`（`page_nodes_active:false`），所以此时元素自身 `display:flex`
但祖先 `.page` 是 `display:none` ⇒ **屏幕上还看不到横幅**。
判据 a) 的原文只要求「`#nodeAlert.on` 在数秒内出现」，这条满足；
「操作者停在概览页时肉眼可见」不是本轮判据，见 §7 遗留观察。

### a2) 补充实测：停在概览不动，横幅状态已是新的

`showPage('nodes')`（与导航按钮 `onclick` 是**同一个函数**，不是鼠标点击）后：

```json
"a2": {
  "classes_before_switch": "node-alert on",
  "classes_after_switch": "node-alert on",
  "visible_after_switch": true,
  "page_nodes_active": true,
  "loadNodes_calls_from_switch": 0,
  "naMeta_after_switch": "最近 10 分钟：4 次节点出口尝试，成功率 25.0%；池内可用 115 / 227；另有连续失败 10 次、跨 19.5 分钟，始于 2026/10/19 21:41:14"
}
```

**结论：PASS。** 打开节点池页时横幅**已经是最新**且可见，切换动作没有触发 `loadNodes()`
——即 F7 的原始症状「只有手动点刷新/切到节点页才更新」在数据层面已消除。

---

## 3. 判据 b)：`#naMeta` 文案

Run A 原始输出（`b` 段，逐字）：

```json
"b": {
  "naMeta_raw": "最近 10 分钟：4 次节点出口尝试，成功率 25.0%；池内可用 115 / 227；另有连续失败 10 次、跨 19.5 分钟，始于 2026/10/19 21:41:14",
  "naReason_raw": "节点出口自 21:41:14 起已连续失败 10 次、跨 19m31s，期间零成功",
  "naTitle_raw": "节点层告警：节点出口整体不可用",
  "has_lianxu_10": true,
  "has_kua_19_5": true,
  "has_pool_115_227": true
}
```

三条 `includes()` 断言（脚本内实测，非人工目测）：
「连续失败 10 次」= `true`、「跨 19.5 分钟」= `true`、「池内可用 115 / 227」= `true`。

**结论：PASS。** 注意 `#naMeta` 里窗口口径（4 次尝试 / 25.0%）与连败口径（10 次 / 19.5 分钟）
同时摆出来了 —— 这正是 Lead 说明里要解决的「读的人觉得自相矛盾」问题。

同时刻旁证（与「自动刷新」开关无关）：

```json
"auto_refresh_sel_value": "0",
"localStorage_autoRefresh": "0"
```

即「自动刷新」关闭（默认 0）时横幅照样工作。

---

## 4. 判据 c)：改成 `active=false`，零操作等 ≥31 秒，横幅自动消失

翻转时刻 `2026-10-19T07:27:53.038Z`，之后**没有任何操作**（脚本只 `waitForFunction` + `sleep`）。

Run A 原始输出（`c` 段，逐字）：

```json
"c": {
  "flip_to_inactive_at": "2026-10-19T07:27:53.038Z",
  "elapsed_ms_until_hidden": 29991,
  "waited_ms_total": 31001,
  "nodeAlert_classes_after_wait": "node-alert",
  "nodeAlert_still_on_after_wait": false,
  "naMeta_raw_after_wait": "最近 10 分钟：0 次节点出口尝试，成功率 无数据；池内可用 115 / 227；另有连续失败 10 次、跨 19.5 分钟，始于 2026/10/19 21:41:14"
}
```

Run C 复现：`elapsed_ms_until_hidden = 29986`，`waited_ms_total = 31009`，
`nodeAlert_still_on_after_wait = false`。

全网 `/api/nodes` 请求时间线（Run A，证明「30s 轮询真在跑」而不只是恰好一次）：

```json
"nodes_requests": [
  { "i": 0, "at": "2026-10-19T07:27:23.009Z", "delta_ms_from_prev": null,  "delta_ms_from_nav": 1476 },
  { "i": 1, "at": "2026-10-19T07:27:23.265Z", "delta_ms_from_prev": 256,   "delta_ms_from_nav": 1732 },
  { "i": 2, "at": "2026-10-19T07:27:53.008Z", "delta_ms_from_prev": 29743, "delta_ms_from_nav": 31475 },
  { "i": 3, "at": "2026-10-19T07:28:23.005Z", "delta_ms_from_prev": 29997, "delta_ms_from_nav": 61472 }
]
```

（i=0 是 `loadNodes()` 初次加载，i=1 是 `startNodeAlertWatch()` 的立即刷新，i=2/i=3 是 30s 定时器；
Run C 的同类间隔为 217 / 29748 / 29999 ms。）

**结论：PASS。** 翻转后 29.99 秒（下一次 30s tick）横幅自己消失；
按判据要求继续等到 31.0 秒复查，仍未显示。这只能由「独立 30s 轮询」解释 ——
期间用户没有点「刷新节点」、没有切页、没有开「自动刷新」。

---

## 5. 判据 d)：轮询没有重画节点表

加载后给第一行打标记 `tr.dataset.marker='1'`，并挂钩计数 `window.renderNodeTable` / `window.loadNodes`：

```json
"d_marker_setup": {
  "rows": 2,
  "markerRowText": "n-A当前可用120 ms--切换 解除",
  "rtCallsAtSetup": 0,
  "lnCallsAtSetup": 0
}
```

跑完 c) 之后再断言（Run A，`d` 段，逐字）：

```json
"d": {
  "marker_row_still_present": true,
  "marker_row_text_now": "n-A当前可用120 ms--切换 解除",
  "total_rows": 2,
  "renderNodeTable_calls_total": 0,
  "loadNodes_calls_total": 0,
  "marker_row_text_at_setup": "n-A当前可用120 ms--切换 解除"
}
```

Run C 同样为 `marker_row_still_present: true`、`renderNodeTable_calls_total: 0`、`loadNodes_calls_total: 0`。

**结论：PASS。** 标记行文本与位置原样保留，`loadNodes()`（会重建 `tbody`）与
`renderNodeTable()` 在整个观察窗口内调用次数都是 **0** ⇒ 排序/筛选/滚动位置不会被轮询冲掉。

---

## 6. 判据 e)：`window_success_rate: null` 必须渲染「无数据」而不是 0%

翻转 `/tmp/c5ui7/alert.json` 为 `"window_success_rate": null`（`window_attempts:0, window_successes:0`），
同样**零操作**，等 30s 轮询自己取回。Run A 原始输出（`e` 段，逐字）：

```json
"e": {
  "flip_to_null_rate_at": "2026-10-19T07:27:23.729Z",
  "elapsed_ms_until_rendered": 29299,
  "naMeta_raw": "最近 10 分钟：0 次节点出口尝试，成功率 无数据；池内可用 115 / 227；另有连续失败 10 次、跨 19.5 分钟，始于 2026/10/19 21:41:14",
  "contains_wu_shu_ju": true,
  "contains_0_pct": false,
  "nodeAlert_still_on": true,
  "error": null
}
```

Run C 复现：`elapsed_ms_until_rendered = 29410`，`contains_wu_shu_ju = true`，`contains_0_pct = false`。

**结论：PASS。** 渲染的是「成功率 无数据」；断言 `/(^|[^0-9.])0(\.0)?%/` 为 `false`
（文案里的 `0` 是「0 次尝试」，不是 `0%`）。横幅在 null 情形下保持 `.on`。

---

## 7. 判据 f)：pageerror / console.error

### 7.1 Run A（task-14 指定环境：`stub.py` @9055）—— 逐字原始

```json
"f": {
  "pageErrors": [],
  "pageErrorCount": 0,
  "consoleErrors": [
    "Failed to load resource: net::ERR_CONNECTION_RESET",
    "EventSource's response has a MIME type (\"application/json\") that is not \"text/event-stream\". Aborting the connection.",
    "EventSource's response has a MIME type (\"application/json\") that is not \"text/event-stream\". Aborting the connection.",
    "EventSource's response has a MIME type (\"application/json\") that is not \"text/event-stream\". Aborting the connection.",
    "EventSource's response has a MIME type (\"application/json\") that is not \"text/event-stream\". Aborting the connection.",
    "EventSource's response has a MIME type (\"application/json\") that is not \"text/event-stream\". Aborting the connection."
  ],
  "consoleErrorCount": 6,
  "requestFailed": [
    { "url": "http://127.0.0.1:9055/api/logs/stream", "err": "net::ERR_ABORTED" },
    { "url": "http://127.0.0.1:9055/api/logs/stream", "err": "net::ERR_ABORTED" },
    { "url": "http://127.0.0.1:9055/api/logs/stream", "err": "net::ERR_ABORTED" }
  ]
}
```

**pageerror = 0**（没有任何未捕获 JS 异常）。**console.error = 6，不为 0** —— 必须逐条归因：

1. 5 条 `EventSource ... MIME type ("application/json")`：`#logAutoRefresh` 复选框**默认勾选**
   （`static/admin.html:716`），`initLogs()` 于是打开 `new EventSource('/api/logs/stream')`；
   而 `stub.py` 对未知路径统一回 `200 + application/json`，浏览器据此拒绝 SSE 连接并按退避重连
   （`onerror` → 2s/4s/8s…），所以一轮 62s 观察到 5 条。**这是桩的形状问题，不是页面脚本问题。**
2. 1 条 `Failed to load resource: net::ERR_CONNECTION_RESET`：`admin.html` 用阻塞式
   `<script src="https://cdn.jsdelivr.net/npm/echarts@5.5.1/dist/echarts.min.js">`，
   本机不能直连该 CDN（`curl` 实测 `HTTP 000`），浏览器随机 `ERR_CONNECTION_RESET`。
   页面有 echarts 缺失守卫，只打 `warning`（"ECharts 加载失败，… 无法渲染"，不计入 console.error），
   没有抛异常。

### 7.2 Control：同样两条噪声在**修复前**的 `admin.html` 上出现

被服务的页面是 `git show HEAD:static/admin.html`（实测 `startNodeAlertWatch` 出现次数 = **0**），
桩对 `/api/logs/stream` 的返回与 `stub.py` 一致（`200 + application/json`），只加载 12s：

```json
{
  "label": "prefix_HEAD_nosse",
  "base": "http://127.0.0.1:9057/",
  "wait_ms": 12000,
  "info": { "hasNodeAlert": true, "hasWatch": false, "logAutoRefreshChecked": true, "nodeAlertClass": "node-alert" },
  "pageErrors": [],
  "pageErrorCount": 0,
  "consoleErrors": [
    "Failed to load resource: net::ERR_CONNECTION_RESET",
    "EventSource's response has a MIME type (\"application/json\") that is not \"text/event-stream\". Aborting the connection.",
    "EventSource's response has a MIME type (\"application/json\") that is not \"text/event-stream\". Aborting the connection.",
    "EventSource's response has a MIME type (\"application/json\") that is not \"text/event-stream\". Aborting the connection."
  ],
  "consoleErrorCount": 4,
  "requestFailed": [
    { "url": "https://cdn.jsdelivr.net/npm/echarts@5.5.1/dist/echarts.min.js", "err": "net::ERR_CONNECTION_RESET" },
    { "url": "http://127.0.0.1:9057/api/logs/stream", "err": "net::ERR_ABORTED" },
    { "url": "http://127.0.0.1:9057/api/logs/stream", "err": "net::ERR_ABORTED" }
  ]
}
```

⇒ 两类噪声**与 F7 改动无关**，它们是环境自带的。

### 7.3 Run B：只把桩的 `/api/logs/stream` 改成正确 SSE

```json
"f": {
  "pageErrorCount": 0,
  "consoleErrorCount": 1,
  "consoleErrorTexts": ["Failed to load resource: net::ERR_CONNECTION_RESET"],
  "requestFailed": [{ "url": "https://cdn.jsdelivr.net/npm/echarts@5.5.1/dist/echarts.min.js", "err": "net::ERR_CONNECTION_RESET" }]
}
```

5 条 EventSource 报错消失，`/api/logs/stream` 的 `requestFailed` 变空 ⇒ 归因 1 成立。

### 7.4 Run C：再把不可达的 CDN 用占位脚本路由掉（`ECHARTS_FULFILL=1`）

```json
"f": {
  "pageErrors": [],
  "pageErrorCount": 0,
  "consoleErrors": [],
  "consoleErrorCount": 0,
  "requestFailed": []
}
```

其余非 error 级消息（不算判据）：

```json
["ECharts 加载失败，趋势图 无法渲染", "ECharts 加载失败，请求热力图 无法渲染",
 "[DOM] Password field is not contained in a form: …", "ECharts 加载失败，请求热力图 无法渲染",
 "ECharts 加载失败，趋势图 无法渲染", "ECharts 加载失败，请求数趋势图 无法渲染",
 "ECharts 加载失败，请求热力图 无法渲染", "ECharts 加载失败，模型请求分布 无法渲染"]
```

**判据 f) 结论（不替实现者圆场，按事实写）：**
- `pageerror`：**0** —— 三条运行、共约 190 秒，全部为 0。
- `console.error`：**在 task-14 给定的桩环境下是 6 条，不是 0**，因此 f) **按字面不满足**。
  但 6 条全部溯源到桩服务（5 条 SSE MIME）与不可达 CDN（1 条），
  并在**修复前**的 `admin.html` 上同样复现（Control 4 条）；把这两处环境噪声隔离后为 **0**
  （Run C）。⇒ **F7 这次改动没有引入任何 JS 错误**；f) 的字面不达标是验收环境问题。
- 复跑建议：把 `/tmp/c5ui7/stub.py` 的 `/api/logs/stream` 改成
  `Content-Type: text/event-stream` 并保持连接，即可在原始桩下拿到字面 0。

---

## 8. 后端契约（`go build` + 9041 临时实例）

**证据钉在源码版本上**：验收期间 Lead 仍在改 `node_alert.go`（mtime 15:33:05，晚于我 15:20 的首次构建），
所以 15:20 那次构建被作废，最终结论取自 **15:36 用当时的源码重建** 的二进制：

```bash
# 结论所依据的源码版本（构建前后各测一次，未变）
md5sum node_alert.go    # 4939852468fde3617c8de284867b6262

cd /home/jingyx/project/opencode2api-test
export PATH=$PATH:/usr/local/go/bin GOCACHE=$PWD/.gocache GOMODCACHE=$PWD/.modcache GOTMPDIR=$PWD/.gotmp
go build -o /tmp/c5ui7/build/opencode2api_v2 .        # GO_BUILD_EXIT=0，产物 62,566,153 字节
cd /tmp/c7 && /tmp/c5ui7/build/opencode2api_v2 -port 9041 -config /tmp/c7/config.json \
    -password testpw9041 -log-file /tmp/c7/srv2.log -log-stdout=false
```

登录 + 取 `/api/nodes`（**只用 9041，未使用 8000**）：

```text
=== login (v2 binary) ===
HTTP/1.1 302 Found
Location: /
Set-Cookie: session=v6GFnVBd12EczFqQYUaauzSuN89JXkEx3IF0Pj_2Jwo; Path=/; HttpOnly

=== GET /api/nodes ===
HTTP 200
--- raw node_alert line ---
"node_alert":{"active":false,"window_minutes":10,"window_attempts":0,"window_successes":0,"window_success_rate":null,"run_failures":0,"run_minutes":0,"pool_available":0,"pool_total":0}
```

> 15:20 的旧二进制（对着 15:33 之前的 `node_alert.go`）给出**完全相同**的 `node_alert` 形状与断言结果
> （原始输出见上一轮记录，`/tmp/c7/nodes.json`）。两次都 PASS。

类型断言（原始输出）：

```text
=== type assertions ===
PASS node_alert is object
PASS 'run_failures' in node_alert
PASS 'run_minutes' in node_alert
PASS run_failures is int (not bool)
PASS run_minutes is number (int|float)
PASS run_minutes is float-typed from Go float64
ALL_PASS
run_failures = 0 | run_minutes = 0
ASSERT_EXIT=0
```

**结论：PASS。** `node_alert` 一定带 `run_failures` / `run_minutes` 两个键（`node_alert.go:138-139`
无 `omitempty`），`run_minutes` 是 JSON 数字（此样本值为 `0`，Go `float64` 序列化后不带小数点，
仍是合法 JSON number，前端 `a.run_minutes.toFixed(1)` 可用）。
这也是判据 e) 的真前提：真实后端的 `window_success_rate` 在这份无样本实例上确实输出 `null`。

---

## 9. 既有 DOM 钩子检查（确认没被破坏）

```bash
python3 scripts/verify_ui.py nojserror --url http://127.0.0.1:9056/     # EXIT=0
python3 scripts/verify_ui.py elements  --url http://127.0.0.1:9058/     # EXIT=0
```

```text
=== 检查 nojserror @ http://127.0.0.1:9056/ ===
PASS  JS 错误: 期望 空（无未捕获异常），实测 []
PASS  console error(页面 JS): 期望 空，实测 []
结论: PASS（2/2 通过）→ 退出码 0

=== 检查 elements @ http://127.0.0.1:9058/ ===   （9058 = 空节点池变体）
PASS  节点池空态行: 期望 1 个 .empty-hint，实测 1
PASS  节点池空态文案: 期望 含「暂无节点」，实测 "暂无节点（可在「订阅与配额」页添加）"
PASS  节点总数徽标 #cntAll: 期望 0，实测 "0"
PASS  配额输入框 #q_error_types / #q_message_kw / #q_max_switches / #q_cooldown_h / #q_cooldown_m / #q_health_interval / #q_health_url: 存在且可见
PASS  「添加订阅」按钮: 期望 恰好 1 个且可见，实测 [{"text": "添加订阅源", "visible": true}]
PASS  JS 错误: 期望 空，实测 []
PASS  console error(页面 JS): 期望 空，实测 []
结论: PASS（13/13 通过）→ 退出码 0
```

完整输出转存：`/tmp/c5ui7/verifyui_nojserror.txt`、`/tmp/c5ui7/verifyui_elements.txt`。

**结论：PASS。** 既有 DOM 钩子未被 F7 改动破坏（`elements` 需要一个空节点池，
所以用了我起的 9058 空池变体；9055/9056 的池里有 2 个节点，是在测横幅用的）。

---

## 10. 遗留观察（不属于 task-14 的 a~f 判据，供下一轮参考）

1. **横幅只在「节点池」页内**：`#nodeAlert` 在 `#page-nodes`，默认页是概览。
   修复后横幅数据**始终是最新的**，但操作者若一直停在概览页，肉眼仍看不到它
   （a) 的 `nodeAlert_visible_on_current_page:false` 就是这个事实）。
   这可能是 C-5 的下一个产品缺口，而不是 F7 的回归。
2. **每 30s 拉一次完整 `/api/nodes`**（节点表 + 订阅），N 个开着的后台标签页就是 N 份。
   `document.hidden` 已挡住后台标签页，`visibilitychange` 回前台会立刻补一次，方向是对的。
3. `startNodeAlertWatch()` 排在 `await detectBackend()` **之前**：file:// 演示路径下
   `DEMO_MODE` 此刻还是 `false`，会白发一次必然失败的 `fetch('/api/nodes')`
   （`catch` 吞掉，无用户可见影响，也未产生 console.error —— Run A/C 实测 0 条）。
4. 轮询失败时 `.catch(()=>{})` 保持上一次显示。方向正确（避免把「拿不到」画成「没有告警」），
   但也意味着**请求一直失败时横幅可能长期停在旧状态**；本轮判据未覆盖该情形。

---

## 11. 验收声明

- 交付判据 a)~f) 全部执行，原始输出见上（Run A 主证据，Run B/C/Control 交叉验证）。
- **F7 结论：成立（修复有效）。** a~e 全过；f 的 pageerror 为 0，console.error 的 6 条
  已逐条归因到桩/CDN 并在修复前版本复现，隔离后为 0。
- 本次验收**没有修改** `static/admin.html`（md5 `c39ee71d09246f5666f6ced1f6584203`，
  与验收开始时一致），因此无需重跑 a~f。
- 本次验收**没有修改任何 Go 源文件**（`node_alert.go` / `main.go` / `health_runtime.go` 均为工作区原有修改）。
- **版本钉桩**：前端结论钉在 `static/admin.html` md5 `c39ee71d09246f5666f6ced1f6584203`；
  后端契约钉在 `node_alert.go` md5 `4939852468fde3617c8de284867b6262`（构建前后一致）。
  验收期间 Lead 于 15:33:05 仍改过 `node_alert.go`，凡晚于该 md5 的改动都不在本报告覆盖范围内。
- 未触碰生产容器；后端临时实例只用 9041 端口，验收结束已 kill（`/tmp/c5ui7/stub.py` @9055 按要求保留）。
- 证据文件：`/tmp/c5ui7/res_run_A_9055.json`、`/tmp/c5ui7/res_run_B_9056_sse.json`、
  `/tmp/c5ui7/res_run_C_9056_clean.json`、`/tmp/c5ui7/res_control_prefix.json`、
  `/tmp/c5ui7/e2e_f7_v2.js`、`/tmp/c5ui7/stub_x.py`、`/tmp/c5ui7/verifyui_*.txt`。

---

<!-- 以下为 task-19 追加。上文 §0~§11 是 task-14 的历史记录，一个字都没有改。 -->

## 8. 全局可见性改造

> 本节对应共享任务 **task-19**（「C-5 横幅改成全局可见」），由 task-14 报告 §10 遗留观察 1 升级而来。
> **本节是按交付说明追加在 §11 之后的**；编号「8」沿用交付说明里的叫法，与上文原有的
> 「§8 后端契约」并列，不改动任何历史章节。工作目录 `/home/jingyx/project/opencode2api-test`，分支 `test`。

### 8.1 问题（改造前实测，RED）

`#nodeAlert` 原先在 `<section id="page-nodes">` **内部**（改造前第 573 行）。默认首页是「概览」，
`.page` 只有 active 的那个 `display:block`，所以停在概览页时横幅虽然带着 `.on`，但
`closest('.page')` = `page-nodes`、`offsetParent === null`、`getBoundingClientRect()` 全 0 —— 肉眼完全看不到。

改造前用 task-19 套件跑（`/tmp/c5ui7/res_v3.json`，`stub_x.py @9056` + CDN 占位）的原始输出：

```json
"g": {
  "note": "默认首页 = 概览；本段全程无 page.click()",
  "active_page_before": ["page-overview"],
  "exists": true, "classes": "node-alert on",
  "closest_page": "page-nodes",
  "display": "flex", "visibility": "visible",
  "offsetParent_null": true,
  "rect": {"x": 0, "y": 0, "w": 0, "h": 0},
  "viewport": {"w": 1440, "h": 900},
  "zero_size": true, "in_viewport": false
}
```

```json
"g2": {
  "overview": { "closest_page": "page-nodes", "offsetParent_null": true,  "zero_size": true,  "in_viewport": false },
  "nodes":    { "closest_page": "page-nodes", "offsetParent_null": false, "zero_size": false, "in_viewport": true,
                "rect": {"x": 240, "y": 109, "w": 1164, "h": 83} },
  "subs":     { "closest_page": "page-nodes", "offsetParent_null": true,  "zero_size": true,  "in_viewport": false },
  "misc":     { "closest_page": "page-nodes", "offsetParent_null": true,  "zero_size": true,  "in_viewport": false },
  "logs":     { "closest_page": "page-nodes", "offsetParent_null": true,  "zero_size": true,  "in_viewport": false }
}
```

⇒ **RED 成立且失败原因正确**：`in_viewport=false` 只在「横幅在 #page-nodes 而用户不在节点池页」时出现，
切到 `nodes` 页立刻为 true —— 正是 F7 遗留的那个形态。

### 8.2 改动（GREEN）

只改 `static/admin.html`，**两处搬移 + 一段注释**，没有动任何 CSS、JS 与其它 DOM：

```diff
   <main class="main">
     <div class="topbar"> … </div>
+
+  <!-- C-5 节点层告警横幅（task-19：全局可见）。…（理由注释）… -->
+  <div class="node-alert" id="nodeAlert" role="alert">
+    <span class="na-ico">!</span>
+    <div class="na-body">
+      <div class="na-title" id="naTitle">节点层告警</div>
+      <div id="naReason"></div>
+      <div class="na-meta" id="naMeta"></div>
+    </div>
+  </div>
 
   <!-- ============ 概览 ============ -->
   <section id="page-overview" class="page active" …>
 …
   <section id="page-nodes" class="page" data-od-id="nodes">
-    <!-- C-5 节点层告警横幅：默认隐藏，… -->
-    <div class="node-alert" id="nodeAlert" role="alert"> … </div>
     <div class="filter-chips" id="stateChips">
```

**位置与理由**：放在 `<main class="main">` 内、`.topbar` 之后、所有 `<section class="page">` **之外**
（改造后第 470~482 行）。理由三条：
1. 横幅讲的是「**整个节点出口层**」的可用性（C-5 的口径），与用户当前停在哪一页无关 ——
   放进任何一个 `.page` 都会让别的页看不到它；
2. `.main` 是页面级内容容器（`flex:1;min-width:0;padding:32px 36px 72px`），横幅在这里与页面内容同宽，
   天然不会盖住顶栏（`.topbar` 是普通流、非 sticky）或左侧 `.sidebar`；
3. 契约零改动：`renderNodeAlert()` 用 `q('#nodeAlert')` 按**全局 id** 取元素，位置无关；
   `.node-alert{display:none}` / `.node-alert.on{display:flex}` 原样保留 ⇒ 无告警时不占布局高度。

**改动等价性校验**（证明"只搬了这两处"）：把这两处反向还原后重新计算 md5，
得到 `c39ee71d09246f5666f6ced1f6584203` = 改造前的 md5，逐字节一致：

```text
reconstructed md5: c39ee71d09246f5666f6ced1f6584203
expected (pre task-19): c39ee71d09246f5666f6ced1f6584203
MATCH => 本次改动只有这两处搬移
nodeAlert count in current: 1
```

版本钉桩：

```text
改造前  md5 c39ee71d09246f5666f6ced1f6584203   （/tmp/c5ui7/md5_before_task19.txt）
改造后  md5 ac04a3f2b28ef48fef7bd8cab879efe2
```

### 8.3 a)~f) 原判据重跑（全部仍然 PASS）

两轮，原始文件 `/tmp/c5ui7/res_GREEN_clean.json`（隔离桩/CDN 噪声，主证据）与
`/tmp/c5ui7/res_GREEN_9055.json`（task-14 给定的 `stub.py @9055`，用于对齐旧口径）。
两轮的 a)~h) 结论一致，下面给主证据的逐字输出。

```json
"a": { "elapsed_ms_from_navigation_start": 755, "clicks_performed": 0, "error": null,
       "classes": "node-alert on", "closest_page": null, "offsetParent_null": false,
       "rect": {"x":240,"y":105,"w":1164,"h":83}, "viewport": {"w":1440,"h":900},
       "in_viewport": true, "zero_size": false, "active_page": ["page-overview"],
       "overlaps_topbar": false, "overlaps_sidebar": false,
       "doc_scrollWidth": 1440, "doc_clientWidth": 1440 }
"b": { "naMeta_raw": "最近 10 分钟：4 次节点出口尝试，成功率 25.0%；池内可用 115 / 227；另有连续失败 10 次、跨 19.5 分钟，始于 2026/10/19 21:41:14",
       "has_lianxu_10": true, "has_kua_19_5": true, "has_pool_115_227": true }
"c": { "elapsed_ms_until_hidden": 29990, "waited_ms_total": 31001,
       "nodeAlert_classes_after_wait": "node-alert", "nodeAlert_still_on_after_wait": false, "error": null }
"d": { "marker_row_still_present": true, "total_rows": 2,
       "renderNodeTable_calls_total": 0, "loadNodes_calls_total": 0 }
"e": { "elapsed_ms_until_rendered": 28366,
       "naMeta_raw": "最近 10 分钟：0 次节点出口尝试，成功率 无数据；池内可用 115 / 227；另有连续失败 10 次、跨 19.5 分钟，始于 2026/10/19 21:41:14",
       "contains_wu_shu_ju": true, "contains_0_pct": false, "nodeAlert_still_on": true }
"f": { "pageErrorCount": 0, "consoleErrorCount": 0, "consoleErrors": [], "requestFailed": [] }
"a2": { "classes_after_switch": "node-alert on", "visible_after_switch": true,
        "rect_after_switch": {"x":240,"y":105,"w":1164,"h":83}, "loadNodes_calls_from_switch": 0 }
"nodes_requests": deltas(ms) = [null, 192, 29789, 29999]
```

`stub.py @9055` 那一轮（不隔离）：a~h 同样全过，`f.pageErrorCount = 0`，
`f.consoleErrorCount = 6`，6 条全是
`EventSource's response has a MIME type ("application/json") ...`（§7.1 已归因到桩，非本次改动引入）。

**结论：a)~f) 判据在改造后全部仍然成立，且 a) 现在是"真正看得见"的**（`closest_page: null`、
`rect.w=1164`、`in_viewport: true`）。

### 8.4 新增 g)：停在概览页、不点导航，横幅在视口内

```json
"g": {
  "note": "默认首页 = 概览；本段全程无 page.click()",
  "active_page_before": ["page-overview"],
  "exists": true, "classes": "node-alert on", "closest_page": null,
  "display": "flex", "visibility": "visible", "offsetParent_null": false,
  "rect": {"x": 240, "y": 105, "w": 1164, "h": 83},
  "viewport": {"w": 1440, "h": 900},
  "zero_size": false, "in_viewport": true,
  "overlaps_topbar": false, "overlaps_sidebar": false
}
```

**结论：PASS。** 页面导航后 755ms 就 `.on`；横幅在视口内（y=105，底部 188 < 900），
与顶栏、侧栏都不相交，全程 0 次 `page.click()`。

### 8.5 新增 g2)：概览 / 节点池 / 订阅配额 / 代理与模型 / 日志，五页都能看到

```json
"g2": {
  "overview": { "active_page": ["page-overview"], "closest_page": null, "in_viewport": true, "rect": {"x":240,"y":105,"w":1164,"h":83} },
  "nodes":    { "active_page": ["page-nodes"],    "closest_page": null, "in_viewport": true, "rect": {"x":240,"y":105,"w":1164,"h":83} },
  "subs":     { "active_page": ["page-subs"],     "closest_page": null, "in_viewport": true, "rect": {"x":240,"y":105,"w":1164,"h":83} },
  "misc":     { "active_page": ["page-misc"],     "closest_page": null, "in_viewport": true, "rect": {"x":240,"y":105,"w":1164,"h":83} },
  "logs":     { "active_page": ["page-logs"],     "closest_page": null, "in_viewport": true, "rect": {"x":240,"y":105,"w":1164,"h":83} }
}
```

（切页用的是 `showPage('x')` —— 导航按钮 `onclick` 调的就是它，不是鼠标点击。）

**结论：PASS。** 五页的 `closest_page` 都是 `null`、`rect` 逐页完全相同 —— 横幅是页面级的，与当前页无关。

### 8.6 新增 h)：`active=false` 时不可见且**不占布局高度**

```json
"h": { "note": "active=false 之后（横幅已消失）",
       "exists": true, "classes": "node-alert", "closest_page": null,
       "display": "none", "visibility": "visible", "offsetParent_null": true,
       "rect": {"x": 0, "y": 0, "w": 0, "h": 0},
       "zero_size": true, "in_viewport": false }
```

**结论：PASS。** `display:none` \+ `getBoundingClientRect().height == 0` \+ `offsetParent === null`
⇒ 无告警时完全退出布局（不是 `visibility:hidden` 那种占位隐藏）。

### 8.7 两档截图与窄屏体检

| 截图 | 文件 |
| --- | --- |
| 桌面 1440×900（概览页 + 横幅可见） | `docs/mockups/u2/f7-banner-overview-1440x900.png` |
| 移动 390×844（概览页 + 横幅可见） | `docs/mockups/u2/f7-banner-overview-390x844.png` |

两档的实测（`/tmp/c5ui7/shots_f7_result.json`）：

```json
1440x900: { "nodeAlert_rect": {"x":240,"y":105,"w":1164,"h":83}, "in_viewport": true,
            "overlaps_topbar": false, "overlaps_sidebar": false,
            "nav_buttons_visible": 5, "nav_buttons_total": 5, "nav_in_viewport": true,
            "doc_scrollWidth": 1440, "doc_clientWidth": 1440, "horizontal_overflow": false }
390x844:  { "nodeAlert_rect": {"x":80,"y":132,"w":294,"h":155}, "in_viewport": true,
            "overlaps_topbar": false, "overlaps_sidebar": false,
            "nav_buttons_visible": 5, "nav_buttons_total": 5, "nav_in_viewport": true,
            "doc_scrollWidth": 443, "doc_clientWidth": 390, "horizontal_overflow": true }
```

**结论：PASS（就横幅本身而言）。** 两档横幅都在视口内、都不与顶栏/侧栏相交；
390px 下横幅 `right = 80+294 = 374 < 390`，没有横向溢出，5 个导航按钮全部可见且都在视口内
（`nav_in_viewport: true`）。

#### ⚠️ 现场发现（必须点名，但不是本次改动引入）：390px 整页横向溢出

390px 下 `documentElement.scrollWidth = 443 > 390`。用 `f7_diag_overflow.js` 定位，溢出源是
**概览页既有的 `.filter-bar` 一行**，与横幅无关（`/tmp/c5ui7/diag_390_after.json`）：

```json
"withBanner":    { "clientWidth": 390, "scrollWidth": 443, "overflow": true, "nodeAlertDisplay": "flex",
  "offenders": [
    {"tag":"div","id":"","cls":"filter-bar","right":443,"w":109},
    {"tag":"select","id":"trendRangeSel","cls":"input-sm","right":443,"w":109},
    {"tag":"select","id":"trendModelSel","cls":"input-sm","right":423,"w":89}] }
"withoutBanner": { "clientWidth": 390, "scrollWidth": 443, "overflow": true, "nodeAlertDisplay": "none",
  "offenders": [ 同上三条 ] }
"classOff":      { "clientWidth": 390, "scrollWidth": 443, "overflow": true, "nodeAlertDisplay": "none" }
```

改造**前**的 `static/admin.html`（md5 `c39ee71d...`，逐字节还原）跑同一个诊断，结果一模一样
（`/tmp/c5ui7/diag_390_before.json`）：

```text
PRE-TASK19 @390: client 390 scroll 443 overflow True
    {'tag': 'div',    'id': '',                'cls': 'filter-bar',      'right': 443, 'w': 109}
    {'tag': 'select', 'id': 'trendRangeSel',   'cls': 'input-sm',        'right': 443, 'w': 109}
    {'tag': 'select', 'id': 'trendModelSel',   'cls': 'input-sm',        'right': 423, 'w': 89}
```

⇒ **443px 溢出是既有缺陷**（概览页筛选栏不换行），横幅显隐都不影响它（都是 443）。
按 task-19 的边界「最小手术、不要顺手做别的（U3 会重做外壳）」，**我没有动它**，
只在这里点名 + 给出可复现诊断脚本。如果验收方要求「整页 390 零溢出」，
最小修法是在 `<960px` 断点让 `.filter-bar` 两个 select 换行（属 U3 外壳范围，需 Lead 决定）。

### 8.8 既有 DOM 钩子检查（`python3 scripts/verify_ui.py`）

改造后，在一套能喂数据的桩上跑全部可跑的检查（`@9058` = 真实 `admin.html` + `text/event-stream` +
空节点池 + 1 条 `/api/call-log` 记录），**四项全绿、退出码全 0**：

```text
########## nojserror @9058 ##########
PASS  JS 错误: 期望 空（无未捕获异常），实测 []
PASS  console error(页面 JS): 期望 空，实测 []
结论: PASS（2/2 通过）→ 退出码 0

########## elements @9058 ##########
PASS  节点池空态行: 期望 1 个 .empty-hint，实测 1
PASS  节点池空态文案: 期望 含「暂无节点」，实测 "暂无节点（可在「订阅与配额」页添加）"
PASS  节点总数徽标 #cntAll: 期望 0，实测 "0"
PASS  配额输入框 #q_error_types / #q_message_kw / #q_max_switches / #q_cooldown_h / #q_cooldown_m / #q_health_interval / #q_health_url: 存在且可见
PASS  「添加订阅」按钮: 期望 恰好 1 个且可见，实测 [{"text": "添加订阅源", "visible": true}]
PASS  JS 错误 / console error(页面 JS): 空
结论: PASS（13/13 通过）→ 退出码 0

########## calllog @9058 ##########
PASS  调用日志总条数: 期望 >= 1，实测 "1"
PASS  调用日志行数 #clList .cl-item: 期望 >= 1，实测 1
PASS  调用日志空态 .cl-empty: 期望 0（有数据就不该出现空态），实测 0
PASS  JS 错误 / console error(页面 JS): 空
结论: PASS（5/5 通过）→ 退出码 0

########## logs @9058 ##########
PASS  SSE 状态文案: 期望 含「已连接」或「实时 · seq N」，且不含「连接中断」，实测 "已连接"
PASS  EventSource /api/logs/stream: 期望 readyState == 1 (OPEN)，实测 1
PASS  JS 错误 / console error(页面 JS): 空
结论: PASS（4/4 通过）→ 退出码 0
```

完整输出：`/tmp/c5ui7/verifyui_task19_{nojserror,elements,calllog,logs}.txt`。
（`baseline` / `charts` 两项在本机跑不了：它们要求真实 echarts 与 >=8 行真实统计，
而 jsdelivr 在本环境不可达、桩也没有 stats 数据 —— 与 task-14 报告 §9 同一限制，与本次改动无关。）

### 8.9 复现脚本（留给盲验者，task-20）

脚本放在**仓库内**可见位置，避免只留截图：

| 文件 | 用途 |
| --- | --- |
| `docs/mockups/u2/f7_banner_e2e.js` | a)~h) 全套 Playwright 判据（本报告 8.3~8.6 的数据源） |
| `docs/mockups/u2/f7_stub.py` | 桩：`python3 f7_stub.py <port> <html路径> [sse] [empty]`；`/api/nodes` 读 `/tmp/c5ui7/alert.json`，另带 `/api/call-log` |
| `docs/mockups/u2/f7_shots.js` | 1440×900 / 390×844 截图 + 布局体检 |
| `docs/mockups/u2/f7_diag_overflow.js` | 窄屏横向溢出定位（8.7 的 443px 诊断） |

跑法（原样可复现，注意会改 `/tmp/c5ui7/alert.json`）：

```bash
export PATH=$PATH:/usr/local/go/bin
cd /home/jingyx/project/opencode2api-test
python3 docs/mockups/u2/f7_stub.py 9060 "$PWD/static/admin.html" sse &     # 起桩
BASE=http://127.0.0.1:9060/ OUT=/tmp/c5ui7/rerun.json ECHARTS_FULFILL=1 \
  node docs/mockups/u2/f7_banner_e2e.js                                    # 跑 a)~h)
BASE=http://127.0.0.1:9060/ DIR="$PWD/docs/mockups/u2" \
  node docs/mockups/u2/f7_shots.js                                          # 出两档截图
```

`ECHARTS_FULFILL=1` 只是把本机不可达的 jsdelivr 路由成 200 空脚本（页面自身的 echarts 缺失守卫会打
warning，不计 console.error），属于**隔离环境噪声**，不是改产品行为；不加它也能过 a)~h)，只是 f) 会多出 CDN 那条。

### 8.10 结论

**task-19 成立：横幅已全局可见，且是"最小手术"。**

| 判据 | 结果 | 关键实测 |
| --- | --- | --- |
| a) 零点击自己亮 | PASS | 755ms `#nodeAlert.on`，`closest_page:null`，rect 240/105/1164×83 |
| b) `#naMeta` 三段文案 | PASS | 三段 `includes()` 全 true |
| c) active=false 零操作自动消失 | PASS | 29990ms 消失，31.0s 复查仍未显示 |
| d) 轮询不重画节点表 | PASS | 标记行在，`loadNodes`/`renderNodeTable` 各 0 次 |
| e) null → 「无数据」 | PASS | 含「无数据」、不含 `0%` |
| f) pageerror / console.error | PASS | 0 / 0（隔离桩+CDN 噪声后）；`@9055` 原始桩下 0 / 6（全为桩的 SSE 问题，见 §7） |
| **g) 概览页可见且在视口内** | PASS | `in_viewport:true`、不与顶栏/侧栏相交、0 点击 |
| **g2) 五页都可见** | PASS | 五页 `closest_page:null`、rect 完全一致 |
| **h) 隐藏不占布局高度** | PASS | `display:none`、`height == 0`、`offsetParent === null` |
| 1440/390 两档不遮挡导航 | PASS | 两档 `overlaps_*:false`、`nav 5/5`、`nav_in_viewport:true` |
| `verify_ui.py`（4 项） | PASS | 2/2、13/13、5/5、4/4，退出码全 0 |
| 改动等价性 | PASS | 反向还原后 md5 = `c39ee71d...`（逐字节等于改造前） |

遗留（不在本轮边界内）：**390px 整页横向溢出 443px 是既有缺陷**（概览页 `.filter-bar`），
本次未改，详见 8.7 的对照实测。

版本钉桩：**改造后 `static/admin.html` md5 `ac04a3f2b28ef48fef7bd8cab879efe2`**（改造前 `c39ee71d09246f5666f6ced1f6584203`）。
本次**只改 `static/admin.html`**（两处搬移），另新增报告、`docs/mockups/u2/` 下两张截图与四个脚本；
**未改任何 Go 文件**，未碰生产容器，未用 8000 端口。

---

<!-- 以下为 task-24 追加。§0~§11 与 §8 的历史记录均未改写。 -->

## 10. 窄屏横向溢出修复

> 本节对应共享任务 **task-24**。**这是与 §8「横幅搬移」相互独立的一笔改动** ——
> 两笔改的是不同缺陷、不同代码位置、可分别回滚：§8 搬的是 DOM 位置（HTML 结构），
> 本节改的是 CSS 布局（三处规则）。§8 的结论与证据不受本节影响，反之亦然。

### 10.0 先确认面板既有断点（没有新造一套）

```text
$ grep -n "@media" static/admin.html | sed 's/{.*//'
162:@media (min-width:1000px)      171:@media (min-width:1000px)     179:@media (max-width:760px)
286:@media(max-width:560px)        287:@media(max-width:560px)      378:@media(max-width:960px)
394:@media(max-width:560px)        398:@media (prefers-reduced-motion:reduce)
```

面板既有断点就是 **960 / 760 / 560 / 1000**。本节**全部落在已有的 `@media(max-width:960px)` 块内**
（或基础规则），没有引入新断点、没有新增 `@media`。

### 10.1 问题定位（RED，改前实测）

task-19 报告 §8.7 已证明溢出与横幅无关。本节先用探针把**根因拆成两处**（`f7_probe_layout.js`，
原始输出 `/tmp/c5ui7/probe_layout.json`）：

```text
===== 390x844 ==== clientWidth=390 scrollWidth=443
 header:   {"rect":{"x":97,"y":1180,"w":260,"h":115,"right":357},"display":"flex","flexWrap":"nowrap",
            "inline":"display:flex;justify-content:space-between;align-items:center;margin-bottom:10px"}
 leftGroup:{"rect":{"x":97,"y":1223,"w":237,"h":28,"right":334},"display":"flex","flexWrap":"nowrap"}
 filterBar:{"rect":{"x":334,"y":1180,"w":109,"h":95,"right":443},"display":"flex","flexWrap":"wrap"}
 select trendRangeSel {"rect":{"x":334,"y":1180,"w":109,"h":28,"right":443},"h":28}
 select trendModelSel {"rect":{"x":334,"y":1228,"w":89,"h":28,"right":423},"h":28}
===== 320x568 ==== clientWidth=320 scrollWidth=443
 leftGroup:{"rect":{"x":97,"y":1392,"w":237,"h":28,"right":334},"display":"flex","flexWrap":"nowrap"}
 filterBar:{"rect":{"x":334,"y":1348,"w":109,"h":95,"right":443}}
```

根因两条（都在**概览页**，都不是横幅）：

1. **趋势卡头部那一行是 `display:flex` + `nowrap` 的 inline style**（`static/admin.html:517`），
   里面是「tab 组 + ⚙️」的左组（最小内容宽 237px，`nowrap` 所以拒绝收缩）和 `.filter-bar`
   （109px）。窄屏放不下 ⇒ `.filter-bar` 被推到 `x=334..443`，超出 390/320 视口。
2. **320px 下还有第二处**：`.misc` 页（代理与模型）的 `.gw-grid` 用了
   `minmax(240px,1fr)`，240px 是**硬下限**，视口窄于「240 + 侧栏 64 + 内边距 32」时
   网格轨道拒绝收缩 ⇒ `.gw-box` 右边缘 337 > 320。

改前跑完整验收脚本（`f7_overflow_checks.js`，`/tmp/c5ui7/res_overflow_RED.json`）：
**29 条断言，12 条 FAIL**：

```text
FAIL  [390x844/overview] scrollWidth === clientWidth  actual="443 vs 390"
FAIL  [390x844/overview] effective_offenders 为空
      actual=[{"tag":"div","cls":"filter-bar","right":443,...},{"tag":"select","id":"trendRangeSel","right":443,...},
              {"tag":"select","id":"trendModelSel","right":423,...}]
FAIL  [390x844] #trendRangeSel 触控高度 ≥ 32  actual=28
FAIL  [390x844] #trendModelSel 触控高度 ≥ 32  actual=28
FAIL  [320x568/overview] scrollWidth === clientWidth  actual="443 vs 320"
FAIL  [320x568/overview] effective_offenders 为空
      actual=[... {"cls":"filter-bar","right":443}, {"id":"trendRangeSel","right":443},
              {"id":"trendModelSel","right":423}, {"cls":"gw-box","right":337}, ... 共 13 个]
FAIL  [320x568/misc] scrollWidth === clientWidth  actual="337 vs 320"
FAIL  [320x568/misc] effective_offenders 为空
      actual=[{"tag":"div","cls":"gw-box","right":337,"w":240}, ... 共 8 个]
FAIL  [320x568] #trendRangeSel 触控高度 ≥ 32  actual=28
FAIL  [320x568] #trendModelSel 触控高度 ≥ 32  actual=28
FAIL  [1440x900] #trendRangeSel 触控高度 ≥ 32  actual=28
FAIL  [1440x900] #trendModelSel 触控高度 ≥ 32  actual=28
```

（RED 里 `[1440x900/overview] 两个 select 同一行 |Δy| ≤ 2` 已经是 PASS=0，说明桌面基线本来就好，
修复不能把它弄坏。）

### 10.2 改动（GREEN）：三处 CSS，零 JS、零 DOM

`diff -u /tmp/c5ui7/admin_pre_task24.html static/admin.html` 全文（注释略，见文件）：

```diff
@@ -246,7 +246,11 @@
-.gw-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:10px}
+/* task-24：… 240px 是硬下限，窄屏网格轨道拒绝收缩 ⇒ 320px 下 .gw-box 右边缘 337 > 320 */
+.gw-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(min(240px,100%),1fr));gap:10px}

@@ -388,6 +392,12 @@   @media(max-width:960px){
   .filter-bar{gap:12px}
+  /* task-24：趋势卡头部是 flex+nowrap，窄屏「左组 237px + .filter-bar 109px」放不下 */
+  .card[data-od-id="trends"]>div:first-child{flex-wrap:wrap}
+  .card[data-od-id="trends"]>div:first-child>div:first-child{flex-wrap:wrap}
   .card{padding:16px}

@@ -419,6 +429,10 @@
 .filter-bar{display:flex;gap:20px;flex-wrap:wrap;…}
+/* task-24：触控目标下限，原本 28px */
+.filter-bar select{min-height:32px}
```

三条各自的作用：

| # | 改动 | 作用 | 为什么不算「掩盖」 |
| --- | --- | --- | --- |
| 1 | `.gw-grid` 的 `minmax(240px,1fr)` → `minmax(min(240px,100%),1fr)` | 窄屏允许轨道收缩到可用宽度 | 是真的让盒子变窄；≥240px 宽时 `min()` 取 240，**与原来逐像素相同** |
| 2 | `@media(max-width:960px)` 里给趋势卡头部的两层 flex 行加 `flex-wrap:wrap` | 放不下就换行，`.filter-bar` 回到视口内 | 只是取消「强行不换行」；select 不隐藏、不缩字、不裁切 |
| 3 | `.filter-bar select{min-height:32px}` | 触控目标 28 → 32px | 是放大可点区域，方向与「缩到不可用」相反 |

**没有使用 `overflow-x:hidden`**：全文件 `overflow-x` 的既有 9 处全是本来就有的表格滚动容器
（`#chartReqTable`/`#chartStatsTable`/`#nodeTable` 等的 `overflow-x:auto`），本节一处都没新增，
也没有新增 `overflow:hidden`：

```text
$ grep -c "overflow-x:hidden" static/admin.html          # 改前 / 改后
0                                                        0
$ grep -c "overflow-x:auto"   static/admin.html          # 改前 / 改后
9                                                        9
```

### 10.3 验收：29/29 断言 PASS

`BASE=http://127.0.0.1:9056/ node docs/mockups/u2/f7_overflow_checks.js`（原始 `/tmp/c5ui7/res_overflow_GREEN.json`）：

```text
PASS  [390x844/overview] scrollWidth === clientWidth  actual="390 vs 390"
PASS  [390x844/overview] effective_offenders 为空  actual=[]
PASS  [390x844/nodes] scrollWidth === clientWidth  actual="390 vs 390"
PASS  [390x844/nodes] effective_offenders 为空  actual=[]
PASS  [390x844/subs] scrollWidth === clientWidth  actual="390 vs 390"
PASS  [390x844/subs] effective_offenders 为空  actual=[]
PASS  [390x844/misc] scrollWidth === clientWidth  actual="390 vs 390"
PASS  [390x844/misc] effective_offenders 为空  actual=[]
PASS  [390x844/logs] scrollWidth === clientWidth  actual="390 vs 390"
PASS  [390x844/logs] effective_offenders 为空  actual=[]
PASS  [390x844] #trendRangeSel 触控高度 ≥ 32  actual=32
PASS  [390x844] #trendModelSel 触控高度 ≥ 32  actual=32
PASS  [320x568/overview] scrollWidth === clientWidth  actual="320 vs 320"
PASS  [320x568/overview] effective_offenders 为空  actual=[]
PASS  [320x568/nodes] scrollWidth === clientWidth  actual="320 vs 320"
PASS  [320x568/nodes] effective_offenders 为空  actual=[]
PASS  [320x568/subs] scrollWidth === clientWidth  actual="320 vs 320"
PASS  [320x568/subs] effective_offenders 为空  actual=[]
PASS  [320x568/misc] scrollWidth === clientWidth  actual="320 vs 320"
PASS  [320x568/misc] effective_offenders 为空  actual=[]
PASS  [320x568/logs] scrollWidth === clientWidth  actual="320 vs 320"
PASS  [320x568/logs] effective_offenders 为空  actual=[]
PASS  [320x568] #trendRangeSel 触控高度 ≥ 32  actual=32
PASS  [320x568] #trendModelSel 触控高度 ≥ 32  actual=32
PASS  [1440x900/overview] 两个 select 同一行 |Δy| ≤ 2  actual=0
PASS  [1440x900] scrollWidth === clientWidth  actual="1440 vs 1440"
PASS  [1440x900] #trendRangeSel 触控高度 ≥ 32  actual=32
PASS  [1440x900] #trendModelSel 触控高度 ≥ 32  actual=32
PASS  全程 pageerror == 0  actual=[]
=== ALL PASS （29/29）===
```

逐页 `scrollWidth/clientWidth` 汇总（GREEN）：

```text
[390x844/overview] client=390 scroll=390 overflow=false raw_offenders=0  effective=0
[390x844/nodes]    client=390 scroll=390 overflow=false raw_offenders=13 effective=0
[390x844/subs]     client=390 scroll=390 overflow=false raw_offenders=0  effective=0
[390x844/misc]     client=390 scroll=390 overflow=false raw_offenders=0  effective=0
[390x844/logs]     client=390 scroll=390 overflow=false raw_offenders=0  effective=0
[320x568/overview] client=320 scroll=320 overflow=false raw_offenders=13 effective=0
[320x568/nodes]    client=320 scroll=320 overflow=false raw_offenders=16 effective=0
[320x568/subs]     client=320 scroll=320 overflow=false raw_offenders=17 effective=0
[320x568/misc]     client=320 scroll=320 overflow=false raw_offenders=0  effective=0
[320x568/logs]     client=320 scroll=320 overflow=false raw_offenders=0  effective=0
[1440x900/…]       client=1440 scroll=1440 overflow=false（五页全 0 offender）
```

**`raw_offenders` 与 `effective_offenders` 的区别（别被数字吓到）**：`raw_offenders` 是
「几何上 `right > clientWidth` 的元素」，其中宽表格（`#nodeTable` 475、`#statsTable` 359、
`#subTable` 347、`#webshareTable` 388）本来就在 `overflow-x:auto` 的卡片里滚动 ——
它们**不撑宽页面**，所以脚本给每个 offender 打了 `contained_by_scroller` 标记，
`effective_offenders` 只保留没被滚动容器裁剪的那些。**effective 全为 0** 才是
「谁也没把页面撑宽」的严格判据，这也是验收要求的「`right > clientWidth` 的元素列表为空」的
正确读法（否则一张正常的可横向滚动宽表就会被误判成缺陷）。

### 10.4 桌面 1440×900 未退化

```json
[1440x900/overview] client=1440 scroll=1440 overflow=false
  trendRangeSel: {"rect":{"x":1169,"y":545,"w":109,"h":32,"right":1278},"h":32}
  trendModelSel: {"rect":{"x":1298,"y":545,"w":89,"h":32,"right":1387},"h":32}
  |Δy| = 0   （断言阈值 ≤2）
```

两个 select 仍在**同一行**、x 与改前完全相同（1169 / 1298），只有高度 28→32（触控要求）。
`.gw-grid` 的 `min(240px,100%)` 在 1440 下取 240，与改前一致 ⇒ `.misc` 页 0 offender。

### 10.5 截图

| 截图 | 文件 | 说明 |
| --- | --- | --- |
| 390×844 整页 | `docs/mockups/u2/f7-overflow-390x844.png` | 概览页，`scrollWidth 390` |
| 320×568 整页 | `docs/mockups/u2/f7-overflow-320x568.png` | 概览页，`scrollWidth 320` |
| 390×844 趋势卡 | `docs/mockups/u2/f7-trendbar-390x844.png` | 两个 select 同行、未被隐藏 |
| 320×568 趋势卡 | `docs/mockups/u2/f7-trendbar-320x568.png` | 320 下换行成两行（Δy=52），仍可见可点 |
| 1440×900 趋势卡 | `docs/mockups/u2/f7-trendbar-1440x900.png` | 桌面仍同一行 |

截图探针（`f7_shots_task24.js`，`/tmp/c5ui7/shots_task24.json`）：

```text
{'w':390,'h':844} client 390 scroll 390 overflow False dy 0
   rangeSel {'x':97,'y':256,'w':109,'h':32,'right':206}  modelSel {'x':226,'y':256,'w':89,'h':32,'right':315}
{'w':320,'h':568} client 320 scroll 320 overflow False dy 52
   rangeSel {'x':97,'y':109,'w':109,'h':32,'right':206}  modelSel {'x':97,'y':161,'w':89,'h':32,'right':186}
{'w':1440,'h':900} client 1440 scroll 1440 overflow False dy 0
   rangeSel {'x':1169,'y':522,'w':109,'h':32,'right':1278} modelSel {'x':1298,'y':522,'w':89,'h':32,'right':1387}
```

390 下两个 select 仍同一行（Δy=0，`right` 315 < 390）；320 下换行（Δy=52）但都完整可见
（`right` 206 / 186 < 320），高度都是 32。

### 10.6 `verify_ui.py` 与回归

```text
########## nojserror @9058 ##########  PASS 2/2   EXIT=0
########## elements  @9058 ##########  PASS 13/13 EXIT=0
########## calllog   @9058 ##########  PASS 5/5   EXIT=0
########## logs      @9058 ##########  PASS 4/4   EXIT=0
```

完整输出：`/tmp/c5ui7/verifyui_task24_*.txt`。

**顺带回归 §8 的横幅判据**（CSS 改动可能影响布局，所以重跑了一遍 a)~h)，
原始 `/tmp/c5ui7/res_banner_after_task24.json`）：

```text
a: {'classes':'node-alert on','closest_page':None,'in_viewport':True,'rect':{'x':240,'y':105,'w':1164,'h':83}}
g: {'closest_page':None,'in_viewport':True,'rect':{'x':240,'y':105,'w':1164,'h':83},'overlaps_topbar':False}
g2: {'overview':(True,None),'nodes':(True,None),'subs':(True,None),'misc':(True,None),'logs':(True,None)}
h: {'classes':'node-alert','display':'none','rect':{'x':0,'y':0,'w':0,'h':0}}
b: True True True
c: 30007 31000 False
d: True 0 0
e: True False
f: 0 0
```

⇒ 横幅全局可见那笔改动**没有被本节影响**（a/g/g2/h 数值与 §8 完全一致）。

### 10.7 复现脚本（仓库内，给盲验 task-20）

| 文件 | 用途 |
| --- | --- |
| `docs/mockups/u2/f7_overflow_checks.js` | 本节 29 条断言的完整验收脚本（390/320/1440 × 五页 + 触控高度 + pageerror） |
| `docs/mockups/u2/f7_shots_task24.js` | 三档截图 + 趋势卡局部截图 |
| `docs/mockups/u2/f7_probe_layout.js` | 根因探针（逐元素盒子模型 + 祖先链） |
| `docs/mockups/u2/f7_stub.py` | 桩（同 §8.9） |

```bash
cd /home/jingyx/project/opencode2api-test
python3 docs/mockups/u2/f7_stub.py 9060 "$PWD/static/admin.html" sse &
BASE=http://127.0.0.1:9060/ OUT=/tmp/ov.json DIR="$PWD/docs/mockups/u2" \
  node docs/mockups/u2/f7_overflow_checks.js        # 期望 "=== ALL PASS （29/29）===" 退出码 0
BASE=http://127.0.0.1:9060/ DIR="$PWD/docs/mockups/u2" \
  node docs/mockups/u2/f7_shots_task24.js           # 三档截图
```

### 10.8 结论

**task-24 成立：390/320 两档横向溢出已修，桌面无退化。**

| 判据 | 结果 | 关键实测 |
| --- | --- | --- |
| 390×844 `scrollWidth === clientWidth` | PASS | 390 vs 390（改前 443 vs 390） |
| 320×568 `scrollWidth === clientWidth` | PASS | 320 vs 320（改前 443 / 337 vs 320） |
| `right > clientWidth` 元素为空 | PASS | `effective_offenders = []`（五页 × 两档全空） |
| 五页都断言 | PASS | overview/nodes/subs/misc/logs 各一条 scroll + 一条 offender |
| 1440 两个 select 同行 | PASS | `|Δy| = 0`（阈值 ≤2），x 与改前相同 |
| 触控高度 ≥32 | PASS | 三档均 32（改前 28） |
| 未用 `overflow-x:hidden` 掩盖 | PASS | 全文件无 `overflow-x:hidden`，未新增任何 `overflow` |
| select 未隐藏/未缩 | PASS | 三档截图可见、`offsetParent` 非 null、宽度 109/89 不变 |
| `verify_ui.py` | PASS | 2/2、13/13、5/5、4/4，退出码全 0 |
| pageerror | PASS | 0（29 条断言里显式一条） |
| §8 横幅判据未受影响 | PASS | a/g/g2/h 数值与 §8 一致 |

**与 §8 的关系（明确写清）**：这是**两笔独立改动**。
§8 改的是 DOM 位置（`#nodeAlert` 从 `#page-nodes` 内搬到 `.main` 下），md5 `ac04a3f2…`；
本节改的是三条 CSS 规则（`.gw-grid` 的 `minmax`、趋势卡头部的 `flex-wrap`、`.filter-bar select` 的 `min-height`），
md5 `dee593fd95a5c778279023b6a0b2a378`。两者互不依赖，可分别回滚。

版本钉桩：**`static/admin.html` md5 `dee593fd95a5c778279023b6a0b2a378`**
（本节改前 `ac04a3f2b28ef48fef7bd8cab879efe2`；再往前 §8 改前 `c39ee71d09246f5666f6ced1f6584203`）。
本节**只改 `static/admin.html` 的三条 CSS**，另新增报告、`docs/mockups/u2/` 下 6 张截图
（`f7-overflow-{390x844,320x568,1440x900}.png` + `f7-trendbar-{390x844,320x568,1440x900}.png`）与 3 个脚本；
**未改任何 Go 文件**（`node_alert.go` 等仍由他人负责），未碰生产容器，未用 8000 端口。
