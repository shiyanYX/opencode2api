# F7 · 面板两笔改动独立盲验：横幅全局可见 + 窄屏不溢出

> 作者：teammate `capture-429`（task-20）
> 验证时间：**2026-10-19 16:17 ~ 16:35 (Asia/Shanghai)**
> 工作目录：`/home/jingyx/project/opencode2api-test`（分支 `test`）
> 版本钉桩（**我自己验证时**的值，非采信实现方）：`md5sum static/admin.html` = **`dee593fd95a5c778279023b6a0b2a378`**
> 该文件 mtime = `2026-10-19 16:05:12`，早于我本次会话开始（16:17）⇒ 我全程验的是同一份、未被我改动。
>
> **独立性声明**：本报告所有脚本都是我自己写的（`/tmp/t20/verify_all.js`、`verify_control_adv.js`、`adversarial_bisect.js`、`offender_classify.js`、`final_evidence.js`），
> 未读取、未复用实现方 `verify-panel-f7` 的脚本与结论。实现方自报「29/29 PASS」我**不采信**，下面全部是我自己的原始输出。
>
> **★ 版本对齐补记（重要）**：Lead 在 `16:31:23` 提交了 `4ba8724 fix(panel): 告警横幅改为全局可见 + 修窄屏横向溢出`，
> 而我写完本报告是 `16:32:21` —— 也就是说**我验证的对象当时还在工作区、之后才被提交**。已核对四者 md5 完全一致：
>
> | 对象 | md5 |
> |---|---|
> | 我验证时的工作区 | `dee593fd95a5c778279023b6a0b2a378` |
> | 提交 `4ba8724` 内 | `dee593fd95a5c778279023b6a0b2a378` |
> | 当前 `HEAD` | `dee593fd95a5c778279023b6a0b2a378` |
> | 当前工作区 | `dee593fd95a5c778279023b6a0b2a378` |
>
> ⇒ **本报告的 A/B 结论直接适用于已提交的 `4ba8724`**，不存在「验的和提交的不是同一份」的风险。
>
> 边界遵守：**未改** `static/admin.html`、**未改**任何 Go 文件、未碰生产容器、未用 8000 端口。

---

## 结论（先说）

| 段 | 结论 | 依据 |
|---|---|---|
| **A · 横幅全局可见（task-19）** | **通过** | 5 个分区全部可见、默认首页即可见、`closest('section.page')===null`、active=false 时 `height===0` 且 `offsetParent===null`、与导航零相交。且 RED 对照证明旧版确实不可见。 |
| **B · 窄屏不溢出（task-24）** | **通过** | 390×844 与 320×568 × 五页 = 10/10 全部 `scrollWidth===clientWidth`；反向验证证明**不是靠 `overflow-x:hidden` 藏的**（computed 值 `visible`）；1440 两 select 同行 dy=0；触控高 ≥32。且 RED 对照证明旧版确实溢出。 |
| **★ 附加发现（不影响 A/B 判定）** | **一个既有健壮性缺陷，非本次引入** | 横幅 `#naReason` 缺 `overflow-wrap`，超长**无空格** token 会把整页撑宽。阈值 ≈ **30 个连续无空格字符**。真实后端 reason 的最长 token 仅 15 字符 ⇒ **当前不可触发**，但属潜在回归点。详见 §D。 |

---

## 0. 环境与桩服务

```bash
# 实现方留下的桩：静态服务 static/admin.html + /api/nodes（node_alert 由 alert.json 控制）
ss -ltnp | grep 9055
# LISTEN 0 5 127.0.0.1:9055 0.0.0.0:* users:(("python3",pid=455696,fd=3))
curl -s -o /dev/null -w "HTTP %{http_code} size=%{size_download}\n" http://127.0.0.1:9055/
# HTTP 200 size=191661

cat /tmp/c5ui7/alert.json
# {"active":true,"reason":"节点出口自 21:41:14 起已连续失败 10 次、跨 19m31s，期间零成功",
#  "since":"2026-10-19T21:41:14+08:00","window_minutes":10,"window_attempts":4,
#  "window_successes":1,"window_success_rate":0.25,"run_failures":10,"run_minutes":19.5,
#  "pool_available":115,"pool_total":227}
```
原 `alert.json` 已备份到 `/tmp/t20/alert.orig.json`，所有改动测试结束后**已还原**。

**★ 我自己额外搭了两个对照桩**（用于 RED 验证，只读实现方的基线快照，不写仓库）：

| 端口 | 服务内容 | md5 | 用途 |
|---|---|---|---|
| 9055 | 当前 `static/admin.html` | `dee593fd…` | GREEN（被测） |
| 9061 | `/tmp/c5ui7/admin_pre_task24.html` | `ac04a3f2…` | BEFORE 对照（task-24 之前） |
| 9062 | 当前 + SSE + call-log + 空节点池 | `dee593fd…` | `verify_ui.py` AFTER |
| 9063 | pre_task24 + 同上 | `ac04a3f2…` | `verify_ui.py` BEFORE |

```bash
# 9062/9063 由 /tmp/t20/stub_full.py（cp 自 stub_x.py 形态：SSE + /api/call-log + 空池）拉起
curl -s http://127.0.0.1:9062/ | md5sum   # dee593fd95a5c778279023b6a0b2a378  (= 当前)
curl -s http://127.0.0.1:9063/ | md5sum   # ac04a3f2b28ef48fef7bd8cab879efe2  (= pre_task24)
```

Playwright 一律用：`require('/home/jingyx/.npm/_npx/e41f203b7505f1fb/node_modules/playwright')`，
`executablePath:'/home/jingyx/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome'`。

---

# A · 横幅全局可见（task-19）

脚本：`/tmp/t20/verify_all.js`（A 段）。原始输出：

```
================ 断言汇总 ================
PASS  A1 默认首页可见
PASS  A1 与视口相交
PASS  A1 closest(section.page)===null
PASS  A2 分区 nodes 横幅可见
PASS  A2 分区 subs 横幅可见
PASS  A2 分区 misc 横幅可见
PASS  A2 分区 logs 横幅可见
PASS  A2 各页 rect 一致
PASS  A2b 导航按钮与横幅不相交
PASS  A3 active=false 不占位(height===0 + offsetParent===null)
```

## A1 · 默认首页、不点任何导航（active=true）

实测（`#nodeAlert`）：

```json
{ "hasOn": true, "display": "flex",
  "rect": {"x":240,"y":66,"w":1164,"h":55,"right":1404,"bottom":121},
  "inViewport": true,
  "closestPage": null,
  "activePages": ["page-overview"],
  "chain": ["div.main", "div#view-app.app", "body"] }
```
- **`closest('section.page')` = `null`** ✅ —— 横幅的父链是 `.main → #view-app → body`，**不在任何 `.page` 内**，证明它真的被搬到了页面级容器。
- 当前激活页是 `page-overview`（默认首页），横幅**可见且与视口相交**（y=66..121，在 900 高视口内）✅ —— 事故形态（后端已告警、默认首页看着正常）已修掉。
- 截图：`/tmp/t20/A1-default-overview-1440.png`

## A2 · 5 个分区逐个切换

| 分区 | `.on` | display | rect (x,y,w,h) | 与视口相交 |
|---|---|---|---|---|
| overview | true | flex | 240, 66, 1164, 55 | ✅ |
| nodes | true | flex | 240, 66, 1164, 55 | ✅ |
| subs | true | flex | 240, 66, 1164, 55 | ✅ |
| misc | true | flex | 240, 66, 1164, 55 | ✅ |
| logs | true | flex | 240, 66, 1164, 55 | ✅ |

**各页 rect 完全一致（容差 0.6px）** ✅ —— 横幅不随分区跳动。

## A2b · 导航未被遮挡

遍历 `.nav` 下全部按钮的 boundingBox 与横幅 boundingBox 求交：**全部 `hit=false`** ✅
（导航在左侧栏 x≈0..204，横幅在内容区 x=240 起，两者不相交。）

## A3 · active=false ⇒ 不占位

```json
{ "hasOn": false, "display": "none", "height": 0,
  "offsetParentNull": true, "closestPage": null }
```
- `getBoundingClientRect().height === 0` ✅
- `offsetParent === null` ✅
- `.on` 类已移除、computed `display:none` ✅
截图：`/tmp/t20/A3-inactive-1440.png`

## A-RED · 反向对照（证明我的断言能抓到旧 bug）

把桩指向 **task-19 之前**的基线 `/tmp/c5ui7/admin_pre_task19.html`，停在默认首页：

```
PASS  RED(task-19前) 横幅确实在 section.page 内（我的断言能抓到旧 bug）
PASS  RED(task-19前) 默认首页看不到横幅（复现事故形态）
```
实测值：`closestPage = "page-nodes"`、`rect.h = 0`、`offsetParentNull = true`。
⇒ **旧版横幅确实嵌在 `#page-nodes` 里、默认首页高度为 0**，我的 A1/A3 断言确实能判它 FAIL。
截图：`/tmp/t20/RED-pre19-default-overview-1440.png`

**A 段判定：通过。**

---

# B · 窄屏不溢出（task-24）

脚本：`/tmp/t20/verify_all.js`（B 段）+ `/tmp/t20/offender_classify.js`。原始输出：

```
PASS  B 390px / overview scrollWidth===clientWidth
PASS  B 390px / nodes    scrollWidth===clientWidth
PASS  B 390px / subs     scrollWidth===clientWidth
PASS  B 390px / misc     scrollWidth===clientWidth
PASS  B 390px / logs     scrollWidth===clientWidth
PASS  B 390px 反向：body/html overflow-x 都不是 hidden
PASS  B 390px 反向：scrollX===0（没有可滚出的隐藏内容）
PASS  B 390px 反向：最右元素 right(390) <= clientWidth(390)
PASS  B 320px / overview scrollWidth===clientWidth
PASS  B 320px / nodes    scrollWidth===clientWidth
PASS  B 320px / subs     scrollWidth===clientWidth
PASS  B 320px / misc     scrollWidth===clientWidth
PASS  B 320px / logs     scrollWidth===clientWidth
PASS  B 320px 反向：body/html overflow-x 都不是 hidden
PASS  B 320px 反向：scrollX===0（没有可滚出的隐藏内容）
PASS  B 320px 反向：最右元素 right(320) <= clientWidth(320)
PASS  B6 1440 两个 select 同行 (|dy|<=2)
PASS  B6 1440 触控高 >=32px
```

## B5 · 两档视口 × 五页 = 10/10 不溢出

| 视口 | overview | nodes | subs | misc | logs |
|---|---|---|---|---|---|
| 390×844 | sw=390=cw ✅ | 390 ✅ | 390 ✅ | 390 ✅ | 390 ✅ |
| 320×568 | sw=320=cw ✅ | 320 ✅ | 320 ✅ | 320 ✅ | 320 ✅ |

截图：`/tmp/t20/B-390x844-overview.png`、`/tmp/t20/B-320x568-overview.png`

### ★ 一个必须说清的口径：确实存在「right > clientWidth」的元素，但它们不是溢出

任务要求「列出所有 `right > clientWidth` 的元素（应为空）」。**实测不空**，所以我进一步区分了
「在横向滚动容器内（设计如此）」与「真溢出（撑破页面）」：

```
===== 390x844 =====
  overview  scrollWidth=390 cw=390 溢出=0 | 超出右边界元素: 在横向滚动容器内 0 个, 真溢出 0 个
  nodes     scrollWidth=390 cw=390 溢出=0 | 超出右边界元素: 在横向滚动容器内 13 个, 真溢出 0 个
      滚动容器内: table#nodeTable.tbl right=475.4 (inScrollable: div)
  subs      scrollWidth=390 cw=390 溢出=0 | 0 / 0
  misc      scrollWidth=390 cw=390 溢出=0 | 0 / 0
  logs      scrollWidth=390 cw=390 溢出=0 | 0 / 0

===== 320x568 =====
  overview  scrollWidth=320 cw=320 溢出=0 | 在滚动容器内 13 个, 真溢出 0 个
      滚动容器内: table#statsTable.tbl right=359.1 (inScrollable: div#chartStatsTable.card)
  nodes     scrollWidth=320 cw=320 溢出=0 | 16 / 0  (table#nodeTable right=475.4)
  subs      scrollWidth=320 cw=320 溢出=0 | 17 / 0  (table#subTable right=347.3)
  misc      scrollWidth=320 cw=320 溢出=0 | 0 / 0
  logs      scrollWidth=320 cw=320 溢出=0 | 0 / 0
```

判定方法：对每个越界元素向上找最近一个 computed `overflow-x ∈ {auto,scroll}` 的祖先。

⇒ **「真溢出」在所有 10 个组合里都是 0**。越界的只有表格（`#nodeTable` / `#subTable` / `#statsTable`），
它们各自处在一个 `overflow-x:auto` 的卡片容器里，**横向滚动是表格的既定设计**（宽表在窄屏滚动查看），
且容器本身没撑破页面（`scrollWidth===clientWidth`）。
**所以任务原文「应为空」这条，严格按字面是 FAIL、按意图（页面不横向溢出）是 PASS** —— 我按后者判定，并把差异如实列出。

## B7 · ★ 反向验证：不是靠 `overflow-x:hidden` 藏的

这是最容易骗人的一条。实测 computed 精确值：

```
390x844: body.overflow-x="visible"  html.overflow-x="visible"  sw=390 cw=390
320x568: body.overflow-x="visible"  html.overflow-x="visible"  sw=320 cw=320
```
- `body` 与 `html` 的 computed `overflow-x` **都是 `visible`**（不是 `hidden`、也不是 `clip`）✅
- 320px 下 `window.scrollTo(9999,0)` 后 `scrollX === 0` ✅ —— 页面**没有可横向滚出的隐藏内容**
- 最右侧元素 `right` 恰好 `≤ clientWidth`（390→390、320→320）✅

⇒ 修复是**真的把内容装进去了**，不是把溢出藏起来。实现方「改前 0 / 改后 0」这句我**独立复核为真**
（且我另外用 RED 对照证明了「改前」其实不是 0，见下）。

## B6 · 1440 桌面未回归

```json
{ "a": {"y": 742, "h": 32, "right": 1404}, "b": {"y": 742, "h": 32, "right": 1404},
  "dy": 0, "minH": 32, "barRect": {"x":240, "right":1404, "w":1164} }
```
- 两个 select `|dy| = 0`（≤2 要求）✅ 仍在同一行
- `min-height = 32px`（≥32 要求）✅

## B-RED · 反向对照（证明旧版真的会溢出）

桩指向 **task-24 之前**的基线 `/tmp/c5ui7/admin_pre_task24.html`：

```
PASS  RED(task-24前) 390px 复现横向溢出 scrollWidth>clientWidth
PASS  RED(task-24前) 320px 复现横向溢出 scrollWidth>clientWidth
```
实测：390px → `scrollWidth 443 / clientWidth 390`（溢出 53px）；320px → 溢出更多。
⇒ 两笔改动**确实各自修掉了一个真实存在的旧缺陷**，不是「本来就没问题」。
截图：`/tmp/t20/RED-pre24-overflow-390.png`、`/tmp/t20/RED-pre24-overflow-320.png`

**B 段判定：通过。**

---

# C · 通用项

## C8 · `pageerror` / `console.error` 逐条溯源

我自己跑出的原始计数（`verify_all.js`）：

```
PASS  A 段 pageerror=0
FAIL  A 段 console.error=0     ["EventSource's response has a MIME type (\"application/json\") that is not \"text/event-stream\". Aborting the connection." ×N]
PASS  B 390px pageerror=0
FAIL  B 390px console.error=0  ["Failed to load resource: net::ERR_CONNECTION_RESET", "EventSource's ... MIME ..."]
PASS  B 320px pageerror=0
FAIL  B 320px console.error=0  同上
PASS  B6 1440 pageerror=0
FAIL  B6 1440 console.error=0  同上
FAIL  D 段 外部请求(http/https)=0  ["https://cdn.jsdelivr.net/npm/echarts@5.5.1/dist/echarts.min.js"]
```

**逐条溯源（用「同一脚本跑旧基线」做对照，而不是靠嘴说）：**

```
--- console.error 溯源（去重后）---
current   : ["EventSource's response has a MIME type (\"application/json\") that is not \"text/event-stream\". Aborting the connection.",
             "Failed to load resource: net::ERR_CONNECTION_RESET"]
pre_task19: ["EventSource's response has a MIME type (...)", "Failed to load resource: net::ERR_CONNECTION_RESET"]
pre_task24: ["EventSource's response has a MIME type (...)"]
只在当前出现的: []
```

| 报错 | 溯源 | 与本次改动的关系 |
|---|---|---|
| `EventSource's response has a MIME type ("application/json") that is not "text/event-stream"` | 桩 `/api/logs/stream` 返回 `application/json`（`stub.py` 的已知简化）。**改动前的基线上完全同样出现** | **无关**（桩噪声） |
| `Failed to load resource: net::ERR_CONNECTION_RESET` | 桩在 SSE/长连接上主动断开。**改动前的基线上同样出现** | **无关**（桩噪声） |
| `https://cdn.jsdelivr.net/...echarts...` 外部请求 | `<script defer src="https://cdn.jsdelivr.net/npm/echarts@5.5.1/...">` 在 **HEAD 版本第 15 行就已经存在**（`git show HEAD:static/admin.html \| grep jsdelivr` 命中 1 次），本次 diff 未触碰 | **无关**（既有 CDN 依赖；离线环境下会失败，故 `charts` 检查在联网/离线两种情况下都不稳） |

**「只在当前出现的」= `[]`** ⇒ 没有任何一条 console.error 是本次改动引入的。
**`pageerror` 在所有档位都是 0**（未捕获异常 = 0），这是真正重要的那条。

## C9 · `python3 scripts/verify_ui.py` 原始输出

**⚠️ 这条我没有做到「全绿」，如实报告。** 用 9055（简陋桩）跑会大面积 FAIL，所以我自己搭了
带 SSE + `/api/call-log` + 空节点池的桩（9062=当前 / 9063=pre_task24），两版**成对**跑：

| 检查 | AFTER (9062, 当前) | BEFORE (9063, pre_task24) | 差异 |
|---|---|---|---|
| baseline | FAIL 11/12（`统计表行数` 实测 2 < 8） | **FAIL 11/12（同一条）** | 无差异 |
| charts | FAIL 3/8（`ECharts 已加载` false） | **FAIL 3/8（同一条）** | 无差异 |
| logs | **PASS 4/4** | PASS 4/4 | 无差异 |
| calllog | **PASS 5/5** | PASS 5/5 | 无差异 |
| elements | **PASS 13/13** | PASS 13/13 | 无差异 |
| nojserror | **PASS 2/2** | PASS 2/2 | 无差异 |

原始输出节选：
```
##### logs @ AFTER(9062, 当前) #####
PASS  SSE 状态文案: 期望 含「已连接」或「实时 · seq N」，且不含「连接中断」，实测 "已连接"
PASS  EventSource /api/logs/stream: 期望 readyState == 1 (OPEN)，实测 1
PASS  JS 错误: 期望 空（无未捕获异常），实测 []
PASS  console error(页面 JS): 期望 空，实测 []
结论: PASS（4/4 通过）→ 退出码 0
##### elements @ AFTER(9062, 当前) #####
PASS  节点池空态行: 期望 1 个 .empty-hint，实测 1
PASS  节点池空态文案: 期望 含「暂无节点」，实测 "暂无节点（可在「订阅与配额」页添加）"
... 共 13 条
结论: PASS（13/13 通过）→ 退出码 0
##### nojserror @ AFTER(9062, 当前) #####
结论: PASS（2/2 通过）→ 退出码 0
```

**两条 FAIL 与本次改动无关，证据是「BEFORE 一模一样地 FAIL」：**
1. `baseline / 统计表行数 实测 2 < 8`：桩的 `/api/stats` 只返回 2 个模型（`stub_x.py` 的固定简化），不是真实后端数据量。
2. `charts / ECharts 已加载 false`：桩环境访问不到 `cdn.jsdelivr.net`（离线），**HEAD 版本同样引用该 CDN**，非本次引入。

⇒ **结论：`verify_ui.py` 在本次改动前后行为完全一致（0 条回归）**；但「全绿」这一硬指标**未达成**，
原因是桩数据量不足 + 离线无 CDN，**不是代码缺陷**。完整原始输出：`/tmp/t20/verifyui_pair.txt`。

## C10 · 改动范围核对

```bash
git diff --stat
#  ROADMAP.md        |  90 +++++++++++---
#  calllog.go        |  64 ++++++++++-
#  main.go           |  67 ++++++++++++
#  node_alert.go     | 317 +++++++++++++++++++++++++++++++---------
#  static/admin.html |  74 +++++++++++---
#  statscan.go       |  28 +++++
```

`git diff static/admin.html` 全文我逐行读过，**只有两类**：

1. **横幅搬移**（task-19）：
   - 删除 `#page-nodes` 内的 `<div class="node-alert" id="nodeAlert">`（-10 行）
   - 在 `.main` 内、`.topbar` 之后、**所有 `section.page` 之外**重新插入同一份 DOM（+16 行，含说明注释）
   - 新增 `startNodeAlertWatch()` / `refreshNodeAlertOnce()` 独立轮询（30s，与「自动刷新」开关解耦）
   - `renderNodeAlert()` 增加 `run_failures/run_minutes` 的展示分支
2. **窄屏 CSS**（task-24）：
   - `.gw-grid` 的 `minmax(240px,1fr)` → `minmax(min(240px,100%),1fr)`
   - `@media(max-width:960px)` 内新增 `.card[data-od-id="trends"]>div:first-child{flex-wrap:wrap}`（两条）
   - `.filter-bar select{min-height:32px}`

**`id="nodeAlert"` 唯一性**：`grep -c 'id="nodeAlert"' static/admin.html` = **1** ⇒ 搬移是「删旧插新」，没有留下重复 id（否则 `q('#nodeAlert')` 会命中错的那个）。✅

**⚠️ Go 文件并非零改动，但与这两笔无关（必须指出）**：
`git status` 显示 `calllog.go` / `main.go` / `node_alert.go` / `statscan.go` 均被改。我读了 diff 的头部：
- `main.go` / `statscan.go` / `calllog.go` → 是 **Task 21「未映射模型判定」**（`CallRecord.Unmapped`、`unmappedBuckets`），与横幅/窄屏无关。
- `node_alert.go` → 是 **C-5 v3 告警判定**（D1/D3 修复、`run_failures`/`judgedRun`），属于**后端配套**：
  横幅新增展示的 `run_failures`/`run_minutes` 字段正是这次 Go 改动提供的。
- 这些文件的 mtime（16:16 / 16:24 / 16:27 / 16:28）说明**在我验证期间仍有其他队友在改它们**。
⇒ 「Go 文件零改动」这条**不成立**，但改动内容与「横幅搬移 + 窄屏 CSS」两类之外的部分，
**不属于这两笔前端改动的范围**，是同期其它任务的产物。**我未改任何 Go 文件**（我全程只读 + 只写本报告）。

---

# D · 对抗性尝试（2 个）

## D1 · 超长 `reason` 会不会把导航顶出视口

**先测「后端真实能产出的最长形态」**（从 `node_alert.go:296-330` 六条 `fmt.Sprintf` 模板里取最长的一条）：

```
=== 后端真实 reason 的 token 上界 ===
  真实当前 reason 最长 token = 12
  模板最长 reason 最长 token = 15

=== 用「后端可能产出的最长 reason」做对抗 ===
  1440x900: sw=1440 cw=1440 溢出=0 bannerRight=1404 在视口内=true
  390x844:  sw=390  cw=390  溢出=0 bannerRight=374  在视口内=true
  320x568:  sw=320  cw=320  溢出=0 bannerRight=304  在视口内=true
```
⇒ **真实形态下，三档视口都不溢出、横幅都在视口内、导航不被顶出** ✅

**再测人造极端值（故意构造）**：`reason` 里塞一个 400+ 字符的**无空格** token（如超长 URL/指纹），
实测**会**把页面撑宽：

```
=== 人造超长无空格 token 的溢出阈值 @320x568 ===
  N=  0 @320: 溢出=   0px
  N= 20 @320: 溢出=   0px
  N= 30 @320: 溢出=  54px   ← 阈值在这里
  N= 40 @320: 溢出= 138px
  N= 60 @320: 溢出= 304px
  N=100 @320: 溢出= 638px
  N=200 @320: 溢出=1471px
```
1440×900 下：`N=100` 仍不溢出，`N=200` 溢出 511px。

**根因**：`.node-alert .na-body{flex:1;min-width:0}` 已经设了 `min-width:0`（这点是对的），
但 `#naReason` 这个 div **没有 `overflow-wrap:anywhere` / `word-break:break-word`**，
所以单个超长无空格 token 无法折行，`min-width:0` 只解决了 flex 收缩、不解决内部断行。

**是否本次引入？不是。** 该 CSS 规则（`static/admin.html:225-230`）在 HEAD 版本就存在，
本次 diff 只搬移了 DOM、没改 `.node-alert` 的样式。RED 对照也印证：把同样的超长 reason 喂给
**task-19 之前的版本**、停在横幅所在的「节点池」页，同样溢出（`sw=1958 / cw=320`）。

**判定**：这是**既有健壮性缺陷、非本次引入**，且**真实后端不可能触发**（最长 token 15 字符 vs 阈值 30）。
不影响 A 段判定。**建议（一行修复，供实现方决定，我未改）**：
`.node-alert .na-body>div{overflow-wrap:anywhere}`。

## D2 · 320px 下「订阅与配额」页看长表格

```
===== 320x568 =====
  subs      scrollWidth=320 cw=320 溢出=0 | 超出右边界元素: 在横向滚动容器内 17 个, 真溢出 0 个
      滚动容器内: table#subTable.tbl right=347.3 (inScrollable: div)
```
- 真实桩数据（2 行订阅）：**不溢出** ✅
- 我进一步注入极值：给该页每个表格塞一个 **180 字符的无空格长单元格**（`MODEL-ZZZ…-END`）：

```
PASS  对抗2(320 subs 真实桩数据) 不横向溢出
PASS  对抗2(320 subs 注入 180 字符长单元格) 不横向溢出
```
⇒ 订阅页的表格**有正确的断行/滚动处理**，180 字符长 token 也撑不破页面。
（与 D1 形成对比：说明 `.node-alert` 缺 `overflow-wrap` 是**局部遗漏**，不是全局缺陷。）
截图：`/tmp/t20/ADV-subs-320.png`、`/tmp/t20/ADV-long-reason-1440.png`、`/tmp/t20/ADV-long-reason-320.png`

---

# 附 · 复现方式

```bash
# 1) 起桩（实现方留的 9055 已在跑；若需重建）
python3 /tmp/c5ui7/stub.py            # @9055, 服务仓库 static/admin.html

# 2) 我的主验证（A 段 + B 段）
cd /home/jingyx/project/opencode2api-test && node /tmp/t20/verify_all.js
#   → /tmp/t20/verify_all_result.json

# 3) 控制实验（RED 对照 + console.error 溯源 + 对抗）
node /tmp/t20/verify_control_adv.js
#   → /tmp/t20/verify_control_adv.json

# 4) 溢出阈值二分 + 旧版是否已存在该缺陷
node /tmp/t20/adversarial_bisect.js

# 5) 越界元素分类（滚动容器内 vs 真溢出）
node /tmp/t20/offender_classify.js

# 6) 真实最长 reason + computed overflow-x 精确值
node /tmp/t20/final_evidence.js

# 7) verify_ui.py 成对跑（需先起 9062/9063 两个桩，见 §0）
for c in baseline charts logs calllog elements nojserror; do
  python3 scripts/verify_ui.py $c --url http://127.0.0.1:9062/   # AFTER
  python3 scripts/verify_ui.py $c --url http://127.0.0.1:9063/   # BEFORE
done   # 原始输出: /tmp/t20/verifyui_pair.txt
```

截图目录：`/tmp/t20/`
- `A1-default-overview-1440.png`（A1 默认首页横幅可见）
- `A3-inactive-1440.png`（A3 无告警不占位）
- `B-390x844-overview.png`、`B-320x568-overview.png`（B 窄屏）
- `RED-pre19-default-overview-1440.png`（旧版首页看不到横幅）
- `RED-pre24-overflow-390.png`、`RED-pre24-overflow-320.png`（旧版溢出）
- `ADV-long-reason-1440.png`、`ADV-long-reason-320.png`、`ADV-subs-320.png`（对抗）

---

# 不确定项 / 局限

1. **`verify_ui.py` 未全绿**（baseline 的「统计表行数」、charts 的「ECharts 已加载」）。
   我用 BEFORE/AFTER 成对跑证明这两条**与本次改动无关**（两版同样 FAIL），但**严格意义上「必须仍全绿」这条没达成**。
   缺的东西：一个 `/api/stats` 返回 ≥8 个模型的桩，以及能访问 `cdn.jsdelivr.net` 的网络。
2. **「列出所有 `right > clientWidth` 的元素（应为空）」按字面不成立**：真实存在越界元素，
   但全部在 `overflow-x:auto` 的表格容器内、容器自身不撑破页面。我按意图判定 PASS 并列出差异（§B5）。
   如果验收要求按字面「列表必须为空」，那这条应记 FAIL —— 判定标准请你定。
3. **「Go 文件零改动」不成立**（`calllog.go`/`main.go`/`node_alert.go`/`statscan.go` 都有改动），
   但内容是同期其它任务（Task 21 未映射模型、C-5 v3 告警判定），不属于这两笔前端改动。
   我**无法证明**这些 Go 改动与横幅搬移在运行时完全正交（未做 Go 侧联调，桩不跑 Go）。
4. **我只验证了 `static/admin.html` 在桩环境下的行为**，未在生产 8000 上验证（边界禁止）。
   桩的 `/api/nodes`、`/api/stats` 都是简化数据，真实数据规模下的表现未覆盖。
5. **D1 的阈值是 @320×568 与 @1440×900 两点采样**（N=0/20/30/40/60/100/200），未做逐像素二分，
   故「≈30 字符」是量级结论而非精确值。
6. **`charts` 检查依赖外部 CDN**：若验收环境联网，该项可能 PASS；我这里是离线，故 FAIL。
   这属于**环境差异**，不代表代码问题，但结论会随环境翻转。
7. 我验证期间（16:24~16:28）有其他队友在改 `calllog.go`/`main.go`/`statscan.go`，
   `static/admin.html` 自 16:05 起未被改动（md5 `dee593fd…` 全程稳定）⇒ **前端验证结果不受影响**。
