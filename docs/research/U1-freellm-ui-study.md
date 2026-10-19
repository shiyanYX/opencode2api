# U1 · freellm UI 深度学习研究与「opencode2api 面板大改」设计依据

- **研究标的**
  - 源码：`/home/jingyx/freellmapi`（npm workspaces monorepo：`shared` / `server` / `client` / `cli`，见 `/home/jingyx/freellmapi/package.json:3-8`）
  - 运行 UI：`http://127.0.0.1:47996`（只读，curl 抓取，未做任何写操作、未重启容器）
  - 对照物：`/home/jingyx/project/opencode2api-test/static/admin.html`（2237 行，`go:embed` 单文件，`embed.go:7`）
- **硬约束遵守情况**：全程只读。未修改 `/home/jingyx/freellmapi` 任何文件；未重启 `freellmapi-freellmapi-1`；未调用 freellm 任何写接口；未改 opencode2api 任何代码/文档；未 `git add/commit`。
- **证据读取命令**（可复现）
  - `curl -s http://127.0.0.1:47996/ -o /tmp/fl_index.html`
  - `curl -s http://127.0.0.1:47996/assets/index-B76V_SXP.css -o /tmp/fl_index.css`（101,781 字节）
  - `curl -s http://127.0.0.1:47996/assets/index-B8n16Tcf.js -o /tmp/fl_index.js`（1,682,736 字节）

---

## 1. 信息架构

### 1.1 结论速览

| 维度 | freellm 的做法 | 证据 |
| --- | --- | --- |
| 全局导航 | **顶部横条**（sticky），不是侧栏 | `client/src/App.tsx:237-248`（`sticky top-0 z-40 border-b backdrop-blur`，内容容器 `mx-auto flex max-w-6xl`） |
| 一级入口 | 6 个：Models / Playground / Keys / Agents / Analytics / Premium | `client/src/App.tsx:67-74` |
| 二级入口 | 用「分裂控件」（标签导航 + 小箭头下拉），**不开新的一级条目** | `client/src/App.tsx:99-113`（`navMenus`）、`App.tsx:88-89` 注释原文：*"Logs is reachable only from here — it is deliberately kept out of navItems so the top bar does not grow a seventh entry."* |
| 内容列宽 | 统一 `max-w-6xl` = 72rem = 1152px，`px-6 py-8` | `client/src/App.tsx:416` |
| 默认落地页 | `/` → `/models/chat` | `client/src/App.tsx:453` |
| 例外 | 仅 Playground 全幅（`FULL_BLEED_ROUTES`），因为它三栏贴边、转录区内部滚动 | `client/src/App.tsx:395, 414-419` |

### 1.2 「首屏第一眼给谁看什么」

`/models/chat`（FallbackPage）从上到下（`client/src/pages/FallbackPage.tsx:483-500`）：

1. `PageHeader`（标题 + 描述 + 右侧 `ModelsTabs` 分段控件）
2. `GettingStarted` 首跑清单 —— **自带隐藏逻辑**，"hides itself once the install has keys + a request"
3. `TokenUsageBar` 月度 token 预算（堆叠条 + 可折叠图例）—— **仅当 `tokenUsage.totalBudget > 0` 才渲染**，未配置预算的安装不占位
4. 路由策略选择器（`<section className="rounded-3xl border bg-card p-5">`）+ 权重摘要（`strategies.weightsSummary`）
5. `ChainManager`（具名 fallback 链）
6. `PenaltyInspector`（惩罚/冷却检查器）—— **默认折叠**，`readCollapsed()` 无存储值时 `return true`（`client/src/components/penalty-inspector.tsx:12-26`，注释：*"a fresh install (no stored value) defaults to collapsed to keep the routing page calm"*）
7. 统一模型/供应商大表（`ModelTableHead` + 行），带搜索与能力筛选

**这就是密度控制的核心手法**：把「配置类」「解释类」「异常类」区块做成**可折叠 / 条件渲染**，首屏只留「我要花多少额度 + 现在按什么策略选 + 表格」。

### 1.3 信息分层：分层维度不是「数据类型」，是「问题类型」

| 层 | 回答的问题 | freellm 落点 |
| --- | --- | --- |
| L0 导航层 | 我在这套系统的哪一块？ | 顶部 6 项 nav（`App.tsx:67-74`） |
| L1 页头层 | 这一页是什么、现在什么口径？ | `PageHeader`（标题+描述+右侧全局控件如时间窗 `SegmentedControl`，`AnalyticsPage.tsx:713-728`） |
| L2 摘要层 | 一句话现状 | Analytics 的 `Stat` 卡网格（2/4/4 列，`AnalyticsPage.tsx:733-737`） |
| L3 主表/主图 | 明细、可排序 | `Panel` 卡片（header + `p-4` body，`AnalyticsPage.tsx:345-358`） |
| L4 下钻页 | 这一行到底发生了什么 | `/models/chat/:id` → `ModelDetailPage`（`App.tsx:456`）；调用记录 → 请求详情 + 「failover ladder」（`en.json:analytics.failoverLadder` = *"Each hop this request took through the fallback chain, in order."*） |

### 1.4 信息密度控制手法（可照搬清单）

1. **条件渲染**：预算条 `totalBudget > 0` 才出现（`FallbackPage.tsx:497`）；缓存命中率卡仅 `cacheStats?.enabled`（`AnalyticsPage.tsx:755`）；`PenaltyInspector` 无行时 `return null`（`penalty-inspector.tsx:118`）。
2. **默认折叠 + 记忆**：`PenaltyInspector` / `ChainManager` / 预算图例都用 localStorage 记住折叠态（`penalty-inspector.tsx:12`、`chain-manager.tsx:108`、`FallbackPage.tsx:224`）。
3. **一句话 hint 外挂**：`Stat` 卡的 `hint` 不常显，挂在 hover tooltip 上（`AnalyticsPage.tsx:325-343`，注释：*"Optional second figure, so one card can carry two related numbers instead of spending another slot in the summary row."*，且 tooltip 开在 `side="bottom"` 因为统计行紧贴 sticky 导航条）。
4. **同类信息只留一个入口**：Logs 藏在 Analytics 下拉里，不进一级导航（`App.tsx:88-89`）。
5. **一跳以上的解释走下钻，不塞进列表**：列表中只给「分档色 + 数字 + 一个可点/可 hover 的解释」，全量明细进详情页。

### 1.5 对比：我们面板现在的 IA

`static/admin.html:428-434` 是 **左侧栏 204px**（`.sidebar{width:204px...}`，`admin.html:123`）+ 主区 `max-width:1280px`（`.main`，`admin.html:140`）：

| 分区 | id | 行锚 | 内容 |
| --- | --- | --- | --- |
| 概览 | `page-overview` | `admin.html:457-561` | 4 状态卡 + Token Hero 五格 + 趋势卡 + 热力图 + 甜甜圈 + 请求分布表 + 统计表 + 快速接入 + 模型能力 |
| 节点池 | `page-nodes` | `admin.html:563-585` | 状态 chips + 4 个操作按钮 + 延迟分布条 + 节点表 |
| 订阅与配额 | `page-subs` | `admin.html:587-635` | 订阅源表 + Webshare 池表 + 手动节点编辑 + 免费额度切换 |
| 代理与模型 | `page-misc` | `admin.html:636-677` | 推理力度映射 + 模型映射 + SOCKS5 + 统一网关 + 改 api_key |
| 运行日志 | `page-logs` | `admin.html:678-746` | 原始日志（SSE 流）+ 调用日志（列表/时段分析/节点分析） |

**主要 IA 问题（本报告 §6 会展开）**：
- 概览页一带承载 **11 个 DOM 区块 / 11 张表**（`grep -c "<table"` = 11），首屏有「4 卡 + Hero + 趋势图 + 热力图 + 甜甜圈」五层数字，**首屏不需要滚动就能看到的东西太多，反而没有重点**。
- 「节点健康」的真实数据（每个出口节点的请求数/成功率/异常数）**只存在于「运行日志 → 调用日志 → 节点分析」**（`admin.html:2064-2092`），而「节点池」页看不到它——**信息放在了用户不会去找的地方**。
- 「订阅与配额」和「代理与模型」都是配置类，却占了 2/5 个一级导航。

---

## 2. 视觉语言（含从 CSS / bundle 抠出的具体数值）

### 2.1 设计令牌总表

freellm 的令牌源文件只有 **193 行**：`/home/jingyx/freellmapi/client/src/index.css`。全部落在 Tailwind v4 的 `@theme inline` + `:root` / `.dark` 上。

**基础色（浅色主题，`index.css:55-88`）**

| 令牌 | 值 | 语义 |
| --- | --- | --- |
| `--background` | `oklch(0.995 0 0)` | 页面底 |
| `--foreground` | `oklch(0.18 0 0)` | 正文 |
| `--card` | `oklch(1 0 0)` | 卡片底 |
| `--muted` | `oklch(0.97 0 0)` | 弱底（骨架、hover） |
| `--muted-foreground` | `oklch(0.52 0 0)` | 次要文字 |
| `--border` | `oklch(0.92 0 0)` | 边框 |
| `--primary` | `oklch(0.22 0 0)` | 主色 = **近黑** |
| `--destructive` | `oklch(0.577 0.245 27.325)` | 危险 |
| `--radius` | `0.5rem` | 圆角基 |

**基础色（暗色主题，`index.css:90-122`）**

| 令牌 | 值 |
| --- | --- |
| `--background` | `oklch(0.135 0 0)` |
| `--foreground` | `oklch(0.98 0 0)` |
| `--card` | `oklch(0.175 0 0)` |
| `--muted` | `oklch(0.22 0 0)` |
| `--muted-foreground` | `oklch(0.68 0 0)` |
| `--border` | `oklch(1 0 0 / 10%)` ← **半透明白，不是实色** |
| `--input` | `oklch(1 0 0 / 14%)` |
| `--primary` | `oklch(0.93 0 0)` ← **近白** |

> **这是全篇最重要的一条视觉结论：freellm 的中性色是纯无彩度灰阶（全部 `0 0` chroma），主色在浅色下是近黑、暗色下是近白。整套 UI 没有品牌色。** 所有彩色只出现在两类地方：图表系列 + 状态语义。对我们的启示：**先把「主色/强调色」让位给状态语义**，而不是让品牌蓝到处都是（我们现在 `--accent:#6f96ff` 同时当主按钮、当前节点、激活导航、图表系列 1 用）。

**图表色板：5 级灰阶，且亮暗反转**（`index.css:74-78` / `109-113`）

| 令牌 | 浅色 | 暗色 |
| --- | --- | --- |
| `--chart-1` | `oklch(0.28 0 0)` | `oklch(0.92 0 0)` |
| `--chart-2` | `oklch(0.45 0 0)` | `oklch(0.78 0 0)` |
| `--chart-3` | `oklch(0.6 0 0)` | `oklch(0.6 0 0)` |
| `--chart-4` | `oklch(0.75 0 0)` | `oklch(0.45 0 0)` |
| `--chart-5` | `oklch(0.87 0 0)` | `oklch(0.32 0 0)` |

### 2.2 字体层级与字号

- 字体栈：`Geist Variable` / `Geist Mono Variable`，本地自托管（`index.css:11-12`，`@fontsource-variable/geist`）
- **bundle 里实测 11 个 `@font-face` 块**（Geist 5 + Geist Mono 6），全部指向 `/assets/*.woff2`，`font-display:swap`，`font-weight:100 900`（variable font）：
  `grep -o '@font-face' /tmp/fl_index.css | wc -l` → `11`
  （注意：built CSS 是压缩过的，`grep -c` 按**行**计数会得到 `1`，必须用 `grep -o | wc -l`）
- 字距特性：`html{font-feature-settings:'ss01','cv11'}`（`index.css:130`）；**`body{font-feature-settings:'ss01','cv11','tnum'}`（`index.css:137`）——全站默认开启等宽数字**。我们面板是靠 `.num{font-variant-numeric:tabular-nums}` 逐处标注（`admin.html:78`），会漏。
- 字号刻度（Tailwind v4 默认，实测于 `/tmp/fl_index.css`）：
  `--text-xs:.75rem` / `--text-sm:.875rem` / `--text-base:1rem` / `--text-lg:1.125rem` / `--text-xl:1.25rem` / `--text-2xl:1.5rem`
- **实际使用频次普查**（`grep -rho` 统计 `client/src/pages` + `client/src/components`）：

  | 工具类 | 次数 |
  | --- | --- |
  | `text-xs`（12px） | 362 |
  | `text-sm`（14px） | 166 |
  | `text-[11px]` | 89 |
  | `text-[10px]` | 55 |
  | `text-base` | 12 |
  | `text-lg` / `text-xl` / `text-2xl` | 3 / 2 / 1 |

  > 结论：**正文几乎只用 12/14px 两档，11px 给标签与辅助，10px 给极小徽章；≥20px 的字号在整份非 locale 源码里只出现 3 处**：`text-xl`（20px）用在 Analytics 的 `Stat` 值（`AnalyticsPage.tsx:334`）和 Keys 的额度展望（`keys/quota-outlook-section.tsx:41`），`text-2xl`（24px）只在页面标题 `PageHeader` 上出现一次（`page-header.tsx:17`）。我们的面板把 `.stat .v` 定在 **30px**（`admin.html:184`）、`.uh-value` 定在 **26px**（`admin.html:156`）——**比 freellm 的整个信息层级粗一档**。

### 2.3 间距体系

- 基准：`--spacing:.25rem`（4px），实测于 `/tmp/fl_index.css` 的 `@layer theme`
- 主要节奏：卡片 `p-4`(16px) / `p-5`(20px)；卡片圆角 `rounded-3xl`(24px)；卡片间距 `space-y-6`(24px)；统计卡网格 `gap-3`(12px)
- `Panel` 明确分为 header `px-4 py-3` + body `p-4`（`AnalyticsPage.tsx:347-354`）
- `Stat` 卡：`rounded-3xl border bg-card px-4 py-3`（`AnalyticsPage.tsx:326`）

### 2.4 圆角 / 阴影 / 边框

- 圆角刻度（显式注释在 `index.css:44-52`）：
  `--radius: 0.5rem` → `sm 4px · md 6px · lg 8px · xl 12px · 2xl 16px · 3xl 24px · 4xl 32px`
- **实际使用频次普查**：

  | 类 | 次数 | 用在哪 |
  | --- | --- | --- |
  | `rounded-lg`(8px) | 86 | 图标框、小控件、tooltip |
  | `rounded-full` | 85 | 徽章、状态点、进度条 |
  | `rounded-xl`(12px) | 45 | 搜索框、分段控件 |
  | `rounded-3xl`(24px) | 38 | **所有卡片/面板** |
  | `rounded-2xl`(16px) | 25 | 表格容器 |
  | `rounded-md` / `rounded-sm` / `rounded-4xl` | 15 / 6 / 4 | 杂项 |

- **阴影几乎不用**：整份 built CSS 里没有 `--shadow-*` 主题变量；`grep -o "box-shadow:[^;}]*" /tmp/fl_index.css` 只得到 Tailwind 的 ring/shadow 组合变量和 `box-shadow:none`。**面板靠 `border` + `bg-card` 分层，而不是阴影。**
- 边框：暗色下是 `oklch(1 0 0 / 10%)`，即 **10% 白**；表格分隔用 `border-b`。
- Tooltip 是唯一有阴影的元素之一：`shadow-md`（`tooltip.tsx:83`）。

对比：我们的 `.card` 有 `box-shadow:0 1px 2px rgba(0,0,0,.06)`，hover 再叠 `0 2px 8px`（`admin.html:189`），`.stat:hover` 也加阴影（`admin.html:181-182`），`#toast` 用 `--shadow:0 10px 34px rgba(3,8,20,.55)`（`admin.html:52` 深色 / `:66` 浅色；`#toast` 见 `admin.html:287`）。**我们比 freellm 更"重"一档。**

### 2.5 暗色模式

- **class 策略**：`<html class="dark">`，不是 `[data-theme]`。Tailwind v4 自定义 variant：`@custom-variant dark (&:is(.dark *))`（`index.css:7`）
- **首绘前内联脚本，零闪烁**（`client/index.html:7-24`）：读 `localStorage.theme`，`'system'` 或缺省跟随 `prefers-color-scheme`；同时设 `document.documentElement.style.colorScheme` 和 `dataset.themeChoice`
- RTL：`ar/he/fa/ur` 时给 `<html dir="rtl">`（`client/index.html:26-30`）
- 语义映射：亮暗两套都是**同构**的（同名令牌、不同值），没有「暗色下少一个令牌」的情况

对比：我们用 `[data-theme="dark"]` / `[data-theme="light"]` 属性（`admin.html:32` / `admin.html:55`），且图标切换靠 `.ic-moon/.ic-sun` 的 CSS 选择器（`admin.html:83-87`）。两者都可行；**但我们的浅色主题是"另起一套色值"（`--ok:#0a6f42` 等），而不是同构映射**——这导致亮色下每加一个新颜色就要手动配两次。

### 2.6 状态语义色（组件层，不在令牌里）

| 用途 | 值 | 证据 |
| --- | --- | --- |
| 可靠性轴 | `#22c55e` | `components/model-table.tsx:102, 245` |
| 速度轴 | `#3b82f6` | `model-table.tsx:105, 246` |
| 智能轴 | `#a855f7` | `model-table.tsx:108, 247` |
| 惩罚分档 | `>=8` red-600 / `>=5` orange-600 / `>=3` amber-600 / `>0` emerald-600 / `0` muted | `components/penalty-inspector.tsx:70-76` |
| 日志级别 | debug=muted / info=sky-600 / warn=amber-600 / error=destructive | `pages/LogsPage.tsx:51-57` |

**注意**：这些颜色是**硬编码在组件里的 Tailwind 类 / 十六进制字面量**，没有令牌化。这不是好实践，但说明了一件事：freellm 认为「状态色」可以脱离主题令牌独立演进（它在 dark/light 都用 `dark:` 前缀给一版）。我们如果照搬，建议**顺手令牌化**（`.s-ok/.s-warn/.s-bad` 三档），比它更干净。

### 2.7 动效预算（诚实评估：freellm 并不"动效过度"）

- 默认过渡：`--default-transition-duration:.15s`，`cubic-bezier(.4,0,.2,1)`（实测于 `/tmp/fl_index.css`）
- 入场动画只有 `tw-animate-css` 供 dialog/dropdown 用
- 唯一的持续动效是骨架 `animate-pulse`（`ui/skeleton.tsx:9`）
- **全项目没有页面级转场**（对照：我们有 `@keyframes fadeIn .18s`，`admin.html:283-284`）

→ 见 §7 反面清单第 9 条：**"动效过度"这条批评对 freellm 不成立，不要拿它当理由**。

---

## 3. 组件清单（我们缺什么、用在什么场景）

### 3.1 freellm 的组件库全貌

`client/src/components/ui/`（16 个，shadcn/base-ui 血统）：
`badge, button, card, dialog, dropdown-menu, field-error, input, label, popover, segmented-control, select, separator, skeleton, switch, table, textarea`
（`ls /home/jingyx/freellmapi/client/src/components/ui/`）

`client/src/components/`（应用层，节选）：
`page-header, empty-state, confirm-button, copy-button, tooltip, toaster, sortable-header, error-boundary, command-palette, update-reminder, token-usage-bar, model-table, penalty-inspector, chain-manager, getting-started, media-models, api-usage, floating-bar, models-tabs, peak-hours-controls, custom-weights-popover, agent-icons, markdown, code-block, model-combobox, settings-dialog, auth-gate`

### 3.2 「我们有 → 它也有」对照（说明我们并不缺基础盘）

| 组件 | 我们有 | freellm |
| --- | --- | --- |
| 状态徽章 | `.badge.available/.exhausted/.dead/.idle` | `admin.html:207-212` | `Badge` 6 变体 | `ui/badge.tsx:8-28` |
| 统计卡 | `.stat` | `admin.html:181-187` | `Stat` | `AnalyticsPage.tsx:325-343` |
| 表格 | `.tbl` | `admin.html:195-205` | `Table` | `ui/table.tsx` |
| 筛选 chips | `.chip` | `admin.html:224-227` | `SegmentedControl` / 筛选按钮 | `ui/segmented-control.tsx:17-46` |
| Toast | `#toast` 单条 | `admin.html:287-289` | `Toaster` 栈（最多 4 条，错误常驻） | `toaster.tsx:13-20`，`lib/toast.ts:24-30` |
| 空态 | `.empty-hint` 纯文字 | `admin.html:81` | `EmptyState`（虚线卡 + 图标 + 标题 + **动作**） | `empty-state.tsx:8-31` |
| 复制 | `copyText()` + `.gw-copy` | `admin.html:840-841`，`admin.html:479-481` | `CopyButton`（图标态 + toast） | `components/copy-button.tsx:1-10` |
| 进度条 | `.usage-bar` / `.uh-hit-bar` | `admin.html:238`（.usage-bar）, `admin.html:174-175`（.uh-hit-bar） | `AxisBar` | `model-table.tsx:28-40` |
| 危险确认 | 原生 `confirm()` ×3 | `admin.html:1621, 1842, 2095` | `ConfirmButton`（二次点击 + 3s 超时 + Esc 撤销） | `confirm-button.tsx:12-16, 47-52, 63-79` |

### 3.3 **我们确实缺的**（按价值排序）

| # | 组件 | freellm 实现 | 用在什么场景 | 我们的现状 |
| --- | --- | --- | --- | --- |
| 1 | **加载骨架** `Skeleton` / `TableSkeleton` / `CardSkeleton` | `ui/skeleton.tsx:5-37`；`TableSkeleton` 就是「表头条 + N 行占位」；被 9 个文件使用 | 首屏、切页、切换时间窗；**保持布局不抖**（注释：*"the layout keeps its shape while data arrives"*） | **零**。全仓只有一个 `加载中…` 文本（`admin.html:558`）和 `空表 + empty-hint` |
| 2 | **带动作的空状态** `EmptyState` | `empty-state.tsx:4-6` 注释原文：*"an empty state without a next step is a dead end."* | 无模型→按钮跳 Keys；无日志→提示放宽筛选；无节点→提示去订阅页 | 有 `empty-hint` 文案但**无动作**：`admin.html:1608`（"暂无节点（可在「订阅与配额」页添加）"——是纯文本，不可点） |
| 3 | **可排序表头** `SortableHeader` + `lib/table-sort.ts` | `lib/table-sort.ts:1-70`：`null → asc → desc → null` 三态循环；**null 值两个方向都排最后**；排序状态按表持久化到 localStorage | 节点表、节点分析表、调用日志表（都是 100+ 行、按请求数/成功率看） | **无**。`grep -n 'onclick="setSort\|data-sort\|sortBy' static/admin.html` → **0 命中** |
| 4 | **Tooltip（portal + 视口钳制）** | `tooltip.tsx:11-95`：`createPortal` 到 body（避免被表格 `overflow-x-auto` 裁切），按触发 rect 定位并钳到视口内，`w-56 rounded-lg bg-foreground px-2.5 py-1.5 text-xs` | 解释「这个分数怎么来的」「这个权重什么意思」，而**不占版面** | **无**。只有原生 `title=`（`grep -o 'title="' static/admin.html \| wc -l` → **14**）+ 少量 `class="hint"` 常显段落 |
| 5 | **全局错误面** | `App.tsx:46-64`：`MutationCache.onError` 统一 `toast.error`，页内已展示的可 `meta.silenceToast` 退出 | **任何写操作失败都不会静默** | 每个调用点自己 `catch → showToast`（如 `admin.html:1616`）；无统一层，新代码容易漏 |
| 6 | **错误边界** | `error-boundary.tsx` + `App.tsx:386-389`（按 pathname 加 key，切页自动复位） | 某页 JS 抛错不拖垮整个面板 | **无**（单文件面板，一个 JS 错误即白屏） |
| 7 | **二次确认按钮** | `confirm-button.tsx` | 清空统计 / 删除订阅 / 重置标记 | 原生 `confirm()`，阻塞主线程、样式不可控 |
| 8 | **分段控件** `SegmentedControl` | `ui/segmented-control.tsx:17-46` | 时间窗切换、Keys 标签页 | 我们用 `switchTrendTab()` 手写按钮 + 内联调色（`admin.html:495`），样式硬编码在 `style=` 里 |
| 9 | **首跑清单** `GettingStarted` | `getting-started.tsx`（自带隐藏、`localStorage` 标记） | 新用户第一次打开不知道干什么 | **无** |
| 10 | **命令面板** `⌘K` | `command-palette.tsx`（292 行） | 快速跳页/搜模型 | **无**（5 个页面，收益低） |
| 11 | **下钻详情页** | `/models/chat/:id`、请求详情 + failover ladder | 「这一行到底发生了什么」 | **无**。节点表只能切换/解除（`admin.html:1616`），看不到该节点的历史 |

### 3.4 两个「freellm 有、但形态值得直接偷」的小件

**(a) `AxisBar` / `AxisNoData` / `AxisRangeBar`（`model-table.tsx:28-89`）**

```jsx
// AxisBar: 12px 宽 × 1.5px 高的圆角条 + 右侧 11px 等宽数字
<div className="h-1.5 w-12 rounded-full bg-muted overflow-hidden">
  <div style={{ width: `${Math.round(v*100)}%`, backgroundColor: color }} />
</div>
<span className="font-mono text-[11px] text-muted-foreground tabular-nums w-7 text-right">
  {value === undefined ? '–' : Math.round(value * 100)}
</span>
```

`AxisNoData`（`model-table.tsx:47-57`）是**虚线空条 + 「无数据」**，`model-table.tsx:42-46` 的注释解释了为什么必须这么做：

> *"The honest replacement for prior-valued bars (#580): a measured axis value only exists once a provider has served requests. Before that the bandit uses exploration priors (0.5 reliability / 0.6 speed) — rendering those as bars reads as a real, suspiciously identical measurement, so unmeasured providers get this explicit placeholder instead."*

**(b) `RateLimitBadge`（`model-table.tsx:131+`）**：把「时间窗速率压力」压缩成一个 badge，同一个视觉在表格分组头、详情页摘要、逐供应商行三处复用（`model-table.tsx:126-130` 注释）。

---

## 4. 交互模式

### 4.1 轮询 / 刷新策略

| 页面/组件 | 间隔 | 证据 |
| --- | --- | --- |
| 全局 `staleTime` | 30s | `App.tsx:58` |
| FallbackPage（模型表） | 15s | `FallbackPage.tsx:151, 160` |
| ModelDetailPage | 15s | `ModelDetailPage.tsx:87` |
| KeysPage / ProviderList / Embeddings / Media / quota-outlook | 30s | `KeysPage.tsx:57` 等 |
| **PenaltyInspector（惩罚/冷却）** | **5s** | `penalty-inspector.tsx:89` |
| Logs | `LOG_POLL_MS`，且 `paused ? false : LOG_POLL_MS` | `LogsPage.tsx:203` |

**四条关键交互规则**（`LogsPage.tsx:203-208`）：

```ts
refetchInterval: paused ? false : LOG_POLL_MS,
refetchIntervalInBackground: false,   // 后台标签页不烧请求
refetchOnWindowFocus: !paused,        // Pause 必须真的是暂停
refetchOnReconnect: !paused,
```

**全局 `staleTime` 的理由**（`App.tsx:50-57` 注释）：
> *"the Models page alone mounts ~13 queries, several of them the same endpoint from different components, and staleTime 0 refetched every one of them on every navigation and window focus."*

→ **反射到我们的一个具体问题**：我们概览页的自动刷新是 `setInterval(loadStats)`，且 **`setAutoRefresh` 只在 `currentPage==='overview'` 时才挂定时器**（`admin.html:819`）。freellm 的做法是「**按数据的新鲜度需求分层**」：路由/模型 15s、密钥 30s、**惩罚/冷却 5s**（因为它变化最快、最需要即时性）。我们现在所有数据一个节奏。

### 4.2 排序与筛选

- **筛选**：chips 多选（日志级别 `toggleLevel`）、下拉（provider）、搜索框（**300ms 防抖**，因为服务端每次过滤都全量重跑，`LogsPage.tsx:29-31`）
- **排序**：纯客户端，对已加载行排序；`SortableHeader` 驱动；**排序状态按表存 localStorage**；空值两个方向都排最后（`lib/table-sort.ts:63-70`）
- **诚实标注排序范围**：`en.json:analytics.sortHint` = *"Sorting applies to the {count} loaded rows"* —— 因为 recent calls 只取 limit=100，排序覆盖的是这 100 行而不是全量。**这个「说清楚你排的是哪一段」的做法值得我们抄**（我们的节点分析是全量内存聚合，不涉及；但调用日志列表也有限制）。

### 4.3 下钻路径

```
/models/chat（大表）
   └─ 行 → /models/chat/:id（ModelDetailPage：逐供应商行 + 逐 key 冷却 + 覆盖字段）
/analytics（摘要 + 图）
   └─ Recent calls 行 → 请求详情（含 failover ladder：按顺序列出这一请求走过的每一跳）
/logs
   └─ 日志行 → 展开长消息（可截断 + Show more/Show less）
```

三类下钻的共同特征：**从「聚合」到「单条原始记录」，且单条记录要能回答"为什么会这样"**（failover ladder 就是「为什么最后是这个出口」的答案）。

### 4.4 错误与降级提示

| 场景 | 做法 | 证据 |
| --- | --- | --- |
| 字段缺失 | 显示 `—` 而不是 `0` | `AnalyticsPage.tsx:697-706`：*"p95 latency is likewise null when the raw window was pruned ... a null must render the same placeholder glyph instead of a misleading '0 ms'"* |
| 有数据但该维度为空 | 显式空文案，不是空图 | `AnalyticsPage.tsx:864-865`：`ttftHasData` 为假时渲染 *"No time to first token recorded yet."* |
| 缓存未启用 | **整张卡不渲染**，不显示误导性的 0% | `AnalyticsPage.tsx:754-755` 注释 |
| 图表无数据 | 图表区内居中一句 `common.noData` | `AnalyticsPage.tsx:770-772` |
| 写操作失败 | 全局 toast，错误 toast **常驻不自动消失** | `lib/toast.ts:28-30` |
| 组件崩溃 | ErrorBoundary 按路由复位 | `App.tsx:386-389` |

### 4.5 危险操作确认

`ConfirmButton`（`confirm-button.tsx`）的行为契约：
1. 第一次点击 → 进入 armed，标签变 `Confirm`，颜色转 destructive
2. **3 秒内**再点才执行（`timeout = 3000` 默认）
3. 超时 / 按 Esc → 自动解除（`useEffect` 定时器 + `onKeyDown` handler，`confirm-button.tsx:46-55, 63-67`）

对比我们：`confirm()` 三处（`admin.html:1621/1842/2095`）。原生 `confirm` 的问题不是不好用，是**它会阻塞渲染、样式不可控、且无法携带上下文**（比如"要删除的是哪条订阅"）。freellm 的分档方案（按钮内二次点击）能让破坏性操作**在按钮上携带对象**。

### 4.6 移动端适配

| 手法 | 证据 |
| --- | --- |
| 导航在 `<md` 收起为下拉菜单，二级变子菜单 | `App.tsx:252`（`hidden ... md:flex`）、`App.tsx:321-374`（`md:hidden` 分支） |
| 网格降列 | `AnalyticsPage.tsx:735`：`grid-cols-2 md:grid-cols-4 xl:grid-cols-4` |
| 元素按断点显隐 | `model-table.tsx`：`hidden h-3 w-16 sm:block`（`ui/skeleton.tsx:24`） |
| 表格：靠外层 `overflow-x-auto`，**没有做卡片化堆叠** | 全局观察；`Tooltip` 用 portal 正是为了不被表格 overflow 裁切（`tooltip.tsx:5-7`） |
| 断点 | Tailwind 默认：`sm 640 / md 768 / lg 1024 / xl 1280` |

对比我们：`@media(max-width:960px)`（`admin.html:370`）和 `@media(max-width:560px)`（`admin.html:386`）两个断点，以及 `@media (max-width:760px)` 给数字缩写（`admin.html:179`），还有 `@media (min-width:1000px)` 给 Hero 五格（`admin.html:162`）。**断点比 freellm 更碎且不成体系**（760/960/1000/560 四个）。`admin.html:410-412` 的注释还专门解释了 `.main` 的移动端处理依赖别处的一条 media 规则——**脆弱**。

### 4.7 其他值得注意的交互

- **Tail 跟随**：日志/转录区在「用户停在底部」时自动跟随，用**方向感知**（不是位置比较）判断，40px 容差（`LogsPage.tsx:34-40`）
  > *"Judging by direction (not position alone) keeps our own smooth scroll — which always travels downwards — from being mistaken for the user taking over."*
- **Toast 计时可暂停**：hover 或窗口失焦时暂停倒计时（`toaster.tsx:44-52`），错误 toast 常驻 + 手动关闭 + 相同消息去重 + 最多同屏 4 条（`lib/toast.ts:24-33`）
- **`localStorage` 持久化的 UI 状态清单**：主题、语言、每张表的排序、Analytics 时间窗、多个折叠面板、Playground 设置、侧栏开合（`grep -rn "localStorage.setItem" client/src` → 18 处）

---

## 5. 实现代价：能搬 / 要改 / 搬不了

### 5.1 它的技术栈（实测）

| 层 | 技术 | 证据 |
| --- | --- | --- |
| 框架 | React 19.2.4 + TypeScript ~6.0.2 | `client/package.json` dependencies |
| 构建 | Vite ^8.0.4，`build: tsc -b && vite build` | `client/package.json` scripts |
| CSS | Tailwind CSS ^4.2.2 + `@tailwindcss/vite`，`components.json` 有 shadcn 配置 | `client/index.css:1-5`，`client/components.json` |
| 路由 | react-router-dom ^7.14.0 | `App.tsx:2` |
| 数据 | @tanstack/react-query ^5.97.0 | `App.tsx:3` |
| 图表 | recharts ^3.8.1 | `AnalyticsPage.tsx` 全篇 |
| 基础组件 | @base-ui/react ^1.3.0 + class-variance-authority + clsx + tailwind-merge | `client/package.json` |
| 图标 | lucide-react ^1.8.0 | 全仓 import |
| 拖拽排序 | @dnd-kit/core + sortable + utilities | `FallbackPage.tsx:5-7` |
| 字体 | @fontsource-variable/geist + geist-mono（自托管 woff2） | `index.css:4-5`，11 个 `@font-face` |
| i18n | 自建 provider + **60 个 locale JSON**（每个 1196 行） | `ls client/src/i18n/locales \| wc -l` → 60 |
| 产物 | `/assets/index-*.js`（1.68 MB）+ `/assets/index-*.css`（102 KB） | curl 实测 |

**规模对照**：freellm client 非 locale 源码 **26,452 行**（`find src -type f -not -path "*/i18n/locales/*" | xargs wc -l`），我们 `static/admin.html` **2,237 行**（其中 JS 1,488 行，从 `admin.html:750` 起）。

### 5.2 逐条判断

#### ✅ 能直接搬（改写为原生 DOM/CSS，语义完全一致）

| 项 | freellm 出处 | 移植做法 | 代价 |
| --- | --- | --- | --- |
| **状态语义令牌**（ok/warn/bad 三档） | `penalty-inspector.tsx:70-76` 的阈值分档思路；`LogsPage.tsx:51-57` 的级别色 | 在 `:root` 双主题各加 `.s-ok/.s-warn/.s-bad` 三组 `bg/fg/border`，替换现有散落的 `--ok/--orange/--red` 直接引用 | 小（~20 行 CSS） |
| **排序三态 + 持久化** | `lib/table-sort.ts:1-70` | 纯函数，无依赖。`sortRows()`/`nextSort()`/`loadStoredSort()`/`persistSort()` 可逐行翻译成同名 JS 函数 | 小（~60 行 + 表头 `onclick`） |
| **空值两个方向都排最后** | `lib/table-sort.ts:63-70` | 同上 | 极小 |
| **骨架屏** | `ui/skeleton.tsx:5-37` | CSS `@keyframes pulse` + `.sk` 基类；`TableSkeleton` 就是「表头条 + N 行」的 innerHTML 模板 | 小（~25 行 CSS + ~15 行 JS） |
| **Tooltip portal + 视口钳制** | `tooltip.tsx:11-95` | 原生 `createElement` + `getBoundingClientRect` + `position:fixed` + 钳制；**但要把 hover-only 改成 hover+focus+点击**（见 §7 第 6 条） | 中（~50 行 JS） |
| **二次确认按钮** | `confirm-button.tsx` | 三行状态机（armed/timer/Esc） | 小（~30 行 JS） |
| **Toast 队列**（最多 4 条、错误常驻、hover 暂停） | `toaster.tsx` + `lib/toast.ts` | 把现在单条 `#toast` 换成容器 + 多条；去重 + 容量上限 | 中（~80 行 JS + 40 行 CSS） |
| **全局写操作错误面** | `App.tsx:46-64` | 包一个 `apiPost(url, body)` 帮助函数，统一 `catch → pushToast(err, 'error')`；调用点可 `opts.silent` 退出 | 小（~20 行），但要改所有写调用点 |
| **「排序只覆盖已加载行」的显式提示** | `en.json:analytics.sortHint` | 在表下方加一行 `.muted` 说明 | 极小 |
| **fat `null → —` 不渲染成 0** | `AnalyticsPage.tsx:697-706` | 约定：`p95/TTFT/成功率` 无样本时渲染 `—`，并给 `title` 说明为何无样本 | 极小，但要逐处审 |
| **`prefers-reduced-motion`** | 已在 `admin.html:390` 有 | 保持并扩展覆盖新加的 `pulse`/`fadeIn` | 极小 |

#### 🟡 必须改造（思路可搬，形态要重写）

| 项 | freellm 出处 | 为什么不能照搬 | 改造方案 |
| --- | --- | --- | --- |
| **`Stat` 统计卡** | `AnalyticsPage.tsx:325-343` | 它是 React 组件 + Tailwind 类 + `lucide-react` 图标组件 | 我们已经有了 `.stat`（`admin.html:181-187`），**但要改三处**：值字号 30px→20px（对齐 freellm 的 `text-xl`）；`hint` 走新 tooltip 而不是常显 `.sub`；图标从内联 SVG 改成可复用的 `<svg><use>` 或统一 14px |
| **`AxisBar` 轴条** | `model-table.tsx:28-40` | JSX + inline style 混写；`#22c55e` 等硬编码 | 抽成 `axisBar(value, color)` 返回 HTML 字符串，复用现有 `esc()`；颜色令牌化 |
| **`Panel` 卡片** | `AnalyticsPage.tsx:345-358` | 同上 | 我们已有 `.card`；补一个 `.card-hd`（`padding` + `border-b` + 标题 + 右侧 actions 槽）以支持「卡片头右侧放控件」这个高频模式（我们现在的做法是 `style="display:flex;justify-content:space-between"` 内联写死，见 `admin.html:495`） |
| **`EmptyState` 带动作** | `empty-state.tsx:8-31` | 虚线卡 + 图标 + 标题 + 描述 + **action 槽**；需要能塞按钮 | `emptyState(icon, title, desc, {label, onclick})` 返回 HTML 字符串；调用点传动作（如「去订阅页」→ `showPage('subs')`） |
| **`SegmentedControl`** | `ui/segmented-control.tsx:17-46` | 有 `role="tablist"` / `aria-selected` 等 a11y 属性要一并搬 | 抽 `.seg` / `.seg > button.active`；我们已有类似结构（`admin.html:495`）但调色硬编码在 `style=` 里，**要挪进 CSS 类** |
| **`GettingStarted` 首跑清单** | `getting-started.tsx` | 依赖它的「有 key + 有请求」判定接口 | 用我们已有的 `/api/stats` + `/api/config` 判「有节点 + 有请求」，`localStorage` 记已关闭 |
| **表格排序** | `sortable-header.tsx` | React 组件 + `aria-sort` | `th` 加 `data-col` + `onclick` + `aria-sort` 属性；`renderXxxTable()` 里先 `sortRows()` 再拼 HTML |
| **`ConfirmButton`** | `confirm-button.tsx` | React state | 用 `data-armed` 属性 + CSS 切样式，`setTimeout` 管超时，`keydown` 监听 Esc |
| **下钻详情** | `ModelDetailPage.tsx` / 请求详情 | 它是路由页；我们是单文件 hash-less 分页 | 两种低成本形态：① **行内展开**（`<tr class="expand">` + `colspan`）；② 复用现有 `showPage()` 加一个 `page-node-detail`。**推荐①**，因为不用改导航 |

#### ❌ 搬不了（依赖它的构建 / 生态 / 运行时）

| 项 | 为什么 | 证据 |
| --- | --- | --- |
| React / JSX / hooks 组件模型 | 需要 bundler + JSX 编译 | `client/package.json` build = `tsc -b && vite build` |
| Tailwind 工具类 | 需要 Tailwind 编译期扫描 content 生成 CSS；运行时没有 `text-muted-foreground` 这种类 | `/tmp/fl_index.css` 开头 `/*! tailwindcss v4.3.0 */`；我们无构建 |
| recharts 图表 | React-only | `AnalyticsPage.tsx` 全篇 `<ResponsiveContainer><LineChart>` |
| `@tanstack/react-query` 的 `refetchInterval`/`MutationCache` | React context | `App.tsx:3, 49-65` |
| `@dnd-kit` 拖拽 | 需要 React | `FallbackPage.tsx:5-7` |
| `lucide-react` 图标 | 需要按名 import + 打包 | `App.tsx:4` |
| Geist 字体（11 个 woff2） | 外网/额外静态资源；**我们已经明确选择"无外网字体依赖"** | `admin.html:33-34` 注释：*"CJK 系统字体栈（PingFang / 微软雅黑 / Noto Sans SC）"* |
| 60 个 locale 的 i18n | 体量 > 我们整个面板；且我们是单语言 | `ls client/src/i18n/locales \| wc -l` → 60 |
| `script-src-attr 'none'` 的 CSP 洁癖 | 我们有 **75 处 `onclick=`**（`grep -o "onclick=" static/admin.html \| wc -l`）；采纳等价策略要重写全部事件绑定 | `curl -s -D- http://127.0.0.1:47996/` 的 CSP 头含 `script-src-attr 'none'` |
| 桌面 Electron 壳逻辑 | 无关 | `App.tsx:146-166, 237-248` |
| Playground / Fusion / Embeddings / Image / Video / Audio / Premium / Agents | 这是多产品面；我们是单用途本地代理面板 | `App.tsx:29-44, 467-475` |

### 5.3 一条硬结论

**freellm 的视觉产出（色值、字号、间距、圆角、状态分档、信息层级）可以 100% 手抄；它的实现方式（除纯函数逻辑外）100% 不能抄。**
```
可抄比例：设计决策 ≈ 95%   代码 ≈ 10%（仅 lib/table-sort.ts 这类纯函数）
```
唯一真正「复制粘贴级」的资产是 **`lib/table-sort.ts`（70 行纯函数）**，其余都要按「先理解意图、再用原生 DOM 重写」的方式做。

---

## 6. 差距与取舍：我们面板最该改的 5 件事（按性价比排序）

> 排序依据：`收益（对核心使用场景「今天哪些节点能用、谁在拖后腿」的提升） / 代价（单文件、无构建、2237 行存量）`

### 🥇 第 1 件：把「节点池」升级为「节点健康」，并把调用日志的节点聚合搬进来

**改成什么样**
- 节点表（`admin.html:579-584`）新增 5 列：`健康分`、`真实成功率`、`请求数`、`异常/切换`、`最近失败`（后 5 列来自今天只存在于日志页的聚合）
- 表上方加一个「**失败形态细分**」条：`经过节点` / `直连·闸门拒绝` / `直连·连不上` / `直连·其他`（沿用 `admin.html:2072-2079` 现有判定逻辑，一字不改）
- 每行可展开（`<tr class="expand">` + `colspan`）显示：该节点最近 N 条失败的事件时间轴、当前冷却、成功/失败计数
- 「节点总数 / 可用 / 配额冷却 / 故障」4 张状态卡（`admin.html:459-462`）保持在概览，但**「故障」卡点击可跳到节点页并自动选中「故障」chip**

**依据 freellm 的哪里**
- 信息该放哪：`FallbackPage.tsx` 把「策略 → 惩罚检查器 → 主表」放在同一页，用户在一个页面里完成「看状态 → 看原因 → 做动作」；我们把它拆成了「概览看状态 / 日志页看原因 / 节点页做动作」
- 表内 `AxisNoData` 的诚实占位原则（`model-table.tsx:42-57`）
- `PenaltyInspector` 的「每条记录给一个原因」（`penalty-inspector.tsx:31-66`：`reasons: ('penalty'|'cooldown'|'recent_errors')[]`）
- 下钻惯例：`/models/chat/:id`（`App.tsx:456`）

**代价**：中。**数据侧零成本**——`clRenderNode()`（`admin.html:2064-2092`）已经在浏览器里算好了 req/ok/耗时/异常，只需把它的聚合逻辑抽成一个共享函数供两处调用。UI 侧约 +120 行 JS + 40 行 CSS。

**风险**
- 中：**「节点分析」和「节点表」用了两套节点标识**。日志页用 `node_names[last] || nodes[last]`（最终出口），节点表用 `fingerprint`（`admin.html:1616` 的 `nodeAction(action, n.fingerprint)`）。**两者可能对不上**。必须先确认 `/api/call-log` 是否带 fingerprint；如果只有名字，就按名字聚合并且**明确标注这是"按名称聚合"**，不要假装是精确的。
- 低：概览页的 4 张状态卡数字来自 `/api/nodes`（`admin.html:1100`），新列来自 `/api/call-log`（`admin.html:1961`），**两个接口刷新节奏不同 → 同一屏上数字可能瞬间不自洽**。缓解：给调用日志聚合结果标一个「统计区间」文案 + 显示各自的最后刷新时间。

**验收**
1. 打开节点页，任一节点的「请求数」与日志页 → 调用日志 → 节点分析里同名的数字**完全相等**（同一函数）
2. 直连记录的三类细分加总 = 直连总数（沿用 `admin.html:2073` 的「三类互斥且全覆盖」要求，写一条断言）
3. 断网/无调用日志时，新列显示 `—` 而不是 `0%`
4. Playwright 截图：桌面 1440px + 移动 390px 两档，表格不横向爆版

---

### 🥈 第 2 件：表格能力层——排序表头 + 骨架屏 + 带动作空态

**改成什么样**
- **排序**：节点表 / 节点分析 / 调用日志列表 / 订阅表加可排序 `th`（点击循环 `默认 → 升 → 降 → 默认`），排序状态按表存 `localStorage`，空值恒排最后
- **骨架**：`loadNodes()` / `loadCallLog()` / `loadStats()` 首次加载时，用**与真实表同形**的占位行（表头条 + 6 行灰条）替换现在的「加载中…」（`admin.html:558`）
- **空态**：`.empty-hint` 升级为可带动作的空态卡；节点页空态给「去添加订阅」（跳 `showPage('subs')`），日志页空态给「放宽筛选」（点一次重置 chips）

**依据 freellm 的哪里**
- `lib/table-sort.ts:1-70`（**70 行纯函数，可近逐行翻译**，含「null 双向最后」和 `nextSort` 三态）
- `ui/skeleton.tsx:5-37`，`App.tsx:490` 的 `TableSkeleton rows={8}` 用法（**加载态与完成态行数一致，避免 reflow**）
- `empty-state.tsx:4-6`：*"an empty state without a next step is a dead end."*

**代价**：小。排序 ~60 行 JS + 表头改 4 张表；骨架 ~25 行 CSS + 15 行 JS；空态 ~30 行。**全部可以在不改数据接口的前提下完成。**

**风险**
- 低：我们所有表都是 `innerHTML` 整表重绘（如 `admin.html:1602-1618`），排序只需在拼接前插一行 `rows = sortRows(rows, ...)`，不涉及 DOM diff
- 低：`<th>` 加 `onclick` 后，现有的 `th width="20%"` 内联宽度（`admin.html:580`）不变，列宽不会跑
- **中**：`discev_test.go:29-35` 会读 `static/admin.html` 并断言 `CL_ISSUE_TYPES` 常量存在。任何重构若把这行常量挪走或改名会**直接让 Go 测试失败**。改前先跑 `go test -run TestFrontEndIssueTypes`（或等价）确认基线。

**验收**
1. 点「成功率」表头两次 → 降序；刷新页面 → 排序保持；点第三次 → 回到接口顺序
2. 无 `localStorage`（隐私模式）时排序仍在本会话生效，不抛错
3. 慢网络下（Playwright `page.route` 延迟 2s）首屏出现骨架而非空白或「加载中…」
4. `go test` 全绿（尤其 `discev_test.go`）

---

### 🥉 第 3 件：概览首屏减负——分区 tab 化

**改成什么样**
- 概览页拆成两个 tab：「**概览**」与「**用量统计**」
  - 概览 = 4 状态卡 + Token Hero 五格 + **趋势图**（`admin.html:459-516`）
  - 用量统计 = 热力图 + 甜甜圈 + 请求分布表 + 统计表（`admin.html:522-536`）
- tab 切换复用日志页已有的 `#logTabRaw/#logTabCall` 分段控件样式（`admin.html:682-683`）——**已有实现，无需新造**
- tab 状态存 `localStorage`（沿用 `autoRefresh` 的存法，`admin.html:813`）

**依据 freellm 的哪里**
- Analytics 一页塞 10 个查询仍不失控，靠的是 **一个 `Stat` 摘要行 + 一组平级 `Panel`**，而不是层叠的卡片巢（`AnalyticsPage.tsx:733-900`）
- `SegmentedControl` 是 freellm 的「一套分段控件到处用」的唯一惯用法（`ui/segmented-control.tsx:5-8` 注释：*"The dashboard's one segmented-control idiom ... shared by the Keys tabs and the Analytics range toggle"*）

**代价**：小。**纯 DOM 搬移 + 一段 `switchTrendTab` 式的显示切换**，约 40 行 JS，不改任何数据接口。

**风险**
- 低：唯一风险是**「首屏看不到关键数字」**。缓解：tab 默认停在「概览」，且 tab 标签上带计数/摘要（如「用量统计 · 今日 1.2M tok」）
- 低：概览页现有 11 个 DOM 区块，搬移时容易漏 id（`admin.html:464-466` 的注释说明 Hero 五格与下方表/图**共用同一个 `/api/stats` 响应**——搬动会打断「五格跟随时间窗和模型筛选」的联动）。**必须先确认搬动后筛选器的作用域**，否则时间窗/模型筛选的联动会失效

**验收**
1. 1440×900 视口下，概览 tab **不需要滚动**就能看到趋势图标题
2. 切换 tab 后时间窗/模型筛选仍作用于「用量统计」里的全部表与图（与搬移前逐项对比）
3. 左侧导航高亮、页面标题（`#pageTitle`/`#pageCrumb`，`admin.html:445`）随 tab 正确更新

---

### 4️⃣ 第 4 件：全局错误面 + 三态（加载/空/错误）统一

**改成什么样**
- 加一个统一的写请求帮助函数：`apiWrite(url, body, {silent})` → 失败自动 `showToast(msg,'error')`，`silent` 时由调用方自己处理
- 把单条 `#toast`（`admin.html:287-289`）换成**栈式**（最多 3~4 条，错误不自动消失，hover 暂停倒计时，相同消息去重）
- 给 `index.html` 级别的 JS 顶层错误加一个兜底：`window.onerror` → 在页面顶部插一条不可关闭的告警横幅（而不是白屏）

**依据 freellm 的哪里**
- `App.tsx:46-64` 的 `MutationCache.onError` + `meta.silenceToast` 逃生舱
- `lib/toast.ts:24-33`：`success/info: 5000ms`、`error: null`（常驻）、`MAX_VISIBLE_TOASTS = 4`、消息去重
- `toaster.tsx:44-52`：hover / 窗口失焦暂停倒计时
- `App.tsx:386-389` 的按路由复位 ErrorBoundary

**代价**：中。~100 行 JS + 40 行 CSS。但**要改所有写调用点**（`nodeAction`/`saveSubs`/`resetStats`/`clearCallLog`/`saveConfig`/`resetAllMarks` 等），属于「扩散式改动」——这是它排第 4 而不是第 3 的原因。

**风险**
- 中：改 toast 会影响所有既有 `showToast` 调用点的观感；建议**保留 `showToast(msg, type)` 签名**，只换实现（多条排队），调用点零改动
- 低：加载骨架会短暂改变 DOM 结构，若 Playwright 断言按行数匹配会踩；建议断言按 `[data-testid]` 而不是行数

**验收**
1. 手工制造一次写失败（如断网/改错 URL）→ 出现常驻错误 toast，不自动消失，可手动关闭
2. 快速连点 5 次「刷新」→ 最多 4 条 toast，相同消息不重复堆叠
3. `window.onerror` 注入一次 → 顶部出现告警横幅，页面其余部分仍可用

---

### 5️⃣ 第 5 件：危险操作确认升级 + 复制反馈 + 数字格式统一

**改成什么样**
- 3 处原生 `confirm()`（`admin.html:1621, 1842, 2095`）改成**按钮内二次确认**（第一次点变「确认？」红色，3s 超时或 Esc 撤销）
- 所有可复制字段（`.gw-copy`，`admin.html:479-481, 663-667`）在复制成功后**按钮本身给出 1.5s 的 ✓ 反馈**，而不只是底部 toast
- `body` 级开启 `font-variant-numeric: tabular-nums`（我们现在是逐处 `.num`，`admin.html:78`）
- `.stat .v` 30px→22px，`.uh-value` 26px→22px（对齐 freellm 的信息层级，见 §2.2）

**依据 freellm 的哪里**
- `confirm-button.tsx:12-16, 47-52, 63-79`（二次点击 + 3s + Esc 的完整契约）
- `copy-button.tsx`（Copy ↔ Check 图标态切换）
- `index.css:137`：`body { font-feature-settings: 'ss01','cv11','tnum' }`
- `AnalyticsPage.tsx:338`：`text-xl font-semibold tabular-nums`
- `copy-button.tsx:1-10` 的注释与 toast 双反馈

**代价**：小。~50 行 JS + ~20 行 CSS。**但优先级最低**，因为原生 `confirm()` 虽然丑，功能是正确的，不阻塞核心场景。

**风险**
- 低：字号缩小会改变现有 Playwright/视觉断言的截图基线（如果有的话）
- 低：`Esc` 全局监听需要避免与现有 `keydown` 处理冲突（我们现在只有日志页的滚动逻辑）

**验收**
1. 点「清空统计」→ 按钮变红为「确认？」→ 3s 不点自动复原；按 Esc 复原；再点才真的执行
2. 复制网关地址 → 按钮出现 ✓ 1.5s 后复原，同时底部 toast（双反馈）
3. `Esc` 不会误触其他页面行为

---

## 7. 反面清单：freellm 哪些设计**不适合**我们

> 逐条给理由。**注意：有几条常见批评（"动效过度"）对 freellm 并不成立，我按事实纠正，不凑数。**

### 7.1 ❌ 它的**整套 CSP 与"无内联脚本"架构**——不能学，学了要重写全部事件绑定

CSP 响应头（`curl -s -D- http://127.0.0.1:47996/`）：
```
script-src 'self' 'sha256-4Mz/yZAENQGlTAAeE1WqXruXCripvlvl0s+Q9S1VS4A=';
script-src-attr 'none'
```
`script-src-attr 'none'` **禁止一切内联事件属性**。我们有 **75 个 `onclick=`**（`grep -o "onclick=" static/admin.html | wc -l`）。
理由：我们的单文件面板的全部交互建立在 `onclick` 上。要采纳同等 CSP，必须先做「事件委托重构」（把所有 `onclick` 换成 `data-action` + 一个委托监听器）。这**本身是个好重构**（也顺手解决 `admin.html:1616` 里字符串拼 HTML 注入 `onclick` 的脆弱写法），但**它是一件独立的事，不应该挂在大改里一起做**——风险会失控。

**结论：不要为了对齐 CSP 去改事件模型。** 但把「字符串拼 `onclick` 传参」改成「`data-action` + `data-*` 属性 + 委托」这一半，建议在改节点表时**局部**做。

### 7.2 ❌ React / Vite / Tailwind 的构建链——物理上搬不进 `go:embed` 单文件

理由（实测）：
- 产物 JS **1,682,736 字节**，CSS **101,781 字节**（`curl` 下载后 `ls -la`）。这不是"塞进 HTML"的量级。
- 构建需要 `tsc -b && vite build`（`client/package.json`），Tailwind 需要编译期扫描源文件生成 CSS，**运行时不存在 `text-muted-foreground` 这类类名**。
- `discev_test.go:29` 直接从 `static/admin.html` 读文本、正则抽 `CL_ISSUE_TYPES` 常量。**引入构建步骤会直接打断这个"前端是文本资产"的既有契约。**

### 7.3 ❌ 自托管 Geist 字体（11 个 woff2）——与我们的既有决策冲突

理由：`admin.html:33-34` 明确写了设计方向是「**无外网字体依赖 · CJK 系统字体栈（PingFang / 微软雅黑 / Noto Sans SC）**」。freellm 的 Geist 是 Latin-only variable font，11 个 `@font-face` 块全部是 Cyrillic/Greek/Vietnamese/Symbols/Latin 子集（实测 `/tmp/fl_index.css`），**根本不包含 CJK 字形**。搬过来对中文界面零收益，只增体积。
**结论：保留我们现有的系统字体栈。** 但可以学它的一点：`font-feature-settings` 开 `tnum`（等宽数字）——这条与字体无关，纯排版收益。

### 7.4 ❌ 60 个 locale 的 i18n

理由：`ls /home/jingyx/freellmapi/client/src/i18n/locales | wc -l` → **60**，每个 1196 行，合计约 7.2 万行 JSON，比我们整个面板大 30 倍。我们是单语言本地运维面板，用户就是本机操作者。**引入 i18n 层只会让单文件膨胀、让每次改文案变成改两份。**

### 7.5 ⚠️ 它的后端接口形态——**它的 UI 形状是被它的 API 塑形的，我们照抄会造出取不到数的列**

freellm 的 `GET /routing` 返回（`client/src/lib/routing.ts:74-83`）：
```ts
interface RoutingScore {
  modelDbId; reliability; speed; intelligence;
  headroom; rateLimit; score; totalRequests;
}
```
外加 `strategy / weights / customWeights / exploreEnabled / peakHoursAdjust / peakStartHour / peakEndHour / peakTimezone / peakAdjusted / keySelectionStrategy / cooldownCeilingMs`（`routing.ts:85-110`）。
**服务端的 `score` 已经是 `base × headroomFactor × rateLimitFactor` 的最终值**（`server/src/services/scoring.ts:1-20`）。

我们要注意两件事：
1. **不要把它的列名当需求。** 它的 `headroom`（月度额度余量）我们有，但我们的额度模型是「节点配额冷却」不是「token 月预算」；它的 `rateLimit` 我们有（429）。它的 `intelligence` 我们**没有也不需要**（免费节点路由不看模型智能度）。
2. **不要在 UI 里展示服务端没给的分量。** 它每个轴都有真值才画条，没值画 `AxisNoData`（`model-table.tsx:42-57`）。我们的评分机制如果第一期只输出一个标量（或 2 个分量），**表里就只放 1~2 列**，不要为了凑齐"三轴像 freellm"而造假列。

理由补充：`FallbackPage.tsx:190-209` 里 `Row = FallbackEntry & Partial<RoutingScore>`，UI 明确用 `Partial` 容忍分量缺失，并在缺的时候降级显示——**这是它敢做多轴的真正前提（后端可选地给），不是 UI 的自由**。

### 7.6 ⚠️ 把关键信息只放在 hover tooltip 里——**移动端/触屏直接丢信息**

理由：`Tooltip` 只挂 `onMouseEnter/onMouseLeave/onFocus/onBlur`（`tooltip.tsx:67-69`），**没有 tap 支持**。而 freellm 里 tooltip 承载的往往不是装饰信息，例如：
- 分数的解释：`strategies.scoreTooltip`（`model-table.tsx:116-118`）
- 权重的含义：`strategies.weightReliability` 等
- 统计卡的口径说明（`AnalyticsPage.tsx:340-342`）

在我们的面板里，**节点状态是"要不要切节点"的决策依据**，把它藏在 hover 里对手机/触屏不可接受。
**改造要求**：如果采纳 tooltip，必须同时支持 `click`/`tap` 触发，或者关键解释在**行内展开区**也有一份（我们第 1 件事的行展开正好提供了这个位置）。

### 7.7 ⚠️ 全局 `staleTime: 30_000` + 后台不轮询——为了省请求牺牲新鲜度，**不适合运维台**

理由：`App.tsx:58` 全局 staleTime 30s；`LogsPage.tsx:205` `refetchIntervalInBackground: false`（后台标签页完全不轮询）。
freellm 是路由器，多数数据是「配置/累计」，30s 陈旧无所谓。**我们的节点池是"现在哪些能用"，且用户经常把面板挂在后台标签页当监控。** 照搬这条会导致：切回标签页时看到的可能是 30 秒前的节点状态。
**结论**：保留我们的「页面离开概览即停表」逻辑（`admin.html:819`），但**不要**引入后台不刷新的规则；同时可以学它的**分层新鲜度**（惩罚/冷却 5s、节点 10~15s、统计 30s 或手动）。

### 7.8 ❌ 多产品面的导航广度（Playground / Fusion / Premium / Agents / 媒体…）

理由：`App.tsx:29-44, 467-475` 挂了 20+ 路由。我们是**单一用途的本地代理面板**，5 个分区已经覆盖全部功能。抄它的导航规模＝抄一堆点不动的入口。
**只学它的一条**：把「配置类」和「观测类」分开——它的 Models（配置）/ Analytics（观测）/ Keys（凭据）三段划分清晰。我们对应可以收敛为「概览 / 节点 / 配置 / 日志」四段（把「订阅与配额」+「代理与模型」合并为「配置」）。

### 7.9 ✅ 反例纠正：「动效过度」这条对 freellm **不成立**

事实：默认过渡 `.15s cubic-bezier(.4,0,.2,1)`；唯一持续动效是骨架 `animate-pulse`；**全项目没有页面级转场**（`index.css` + built CSS 实测）。
反过来，**我们的面板有 `@keyframes fadeIn .18s`** 的页面切换动画（`admin.html:283-284`）和 `.conn .pulse::after` 的脉冲（`admin.html:135-136`）。
**结论：不要拿"动效过度"当拒绝 freellm 的理由。** 在这件事上我们比它更花哨（虽然都属克制范围）。

### 7.10 ⚠️ 它的「首跑清单 + 命令面板 + 拖拽排序」三件套——按需取，别默认全套

- `GettingStarted`（`getting-started.tsx`）：**建议采纳**，我们新用户确实不知道该先干什么。
- `CommandPalette`（`command-palette.tsx`，292 行）：**不采纳**。5 个页面，⌘K 的收益低于它的实现与维护成本。
- `@dnd-kit` 拖拽（`FallbackPage.tsx:5-7`）：**降级采纳**。如果将来要做节点优先级排序，用「上移/下移」按钮 + `↑↓` 键盘，而不是引入拖拽（拖拽在表格里还要处理 `useSortable`、键盘可达性、触摸滚动冲突，`FallbackPage.tsx:280-307` 那段 `handleGroupedDragEnd` + `arrayMove` 就是成本证据）。

### 7.11 ⚠️ 文案风格：它写"解释性长句"，我们写"操作提示短句"

例子：`en.json:analytics.requestsHintPinned` = *"{pinned} of these requests pinned a specific model by name. {honored} were served by the pinned model; {failed} failed over to a different one. The rest were auto-routed."* —— 一句话把四个数说清楚，挂在 hover 上。
我们的注释里也有同样严谨的传统（`admin.html:2000-2002` 关于"真实成功率"的分母解释），但**这些解释大多在代码注释里，不在 UI 上**。可以学它的做法：**把口径说明从代码注释搬到 UI 的 hover/展开区**——但注意 §7.6 的触屏限制。

---

## 8. 专题：**节点评分机制应该以什么形态呈现在面板上**

> 这是本报告存在的直接理由（Lead 明确要求：评分机制排在面板信息架构之后，因为评分要展示在这个面板上）。

### 8.1 先回答"为什么不能照抄 freellm 的呈现"

freellm 的评分呈现是**三轴 + 总分**（`model-table.tsx:93-124` 的 `ModelTableHead`）：

| 列 | 视觉 | 颜色 | 依据 |
| --- | --- | --- | --- |
| 可靠性 | `AxisBar` 12×1.5px + 0-100 数字 | `#22c55e` | `model-table.tsx:245` |
| 速度 | 同上 | `#3b82f6` | `model-table.tsx:246` |
| 智能 | 同上 | `#a855f7` | `model-table.tsx:247` |
| 护栏 | `RateLimitBadge` | — | `model-table.tsx:110-114` |
| 总分 | 右对齐，3 位小数 + tooltip 展开逐供应商分 | — | `model-table.tsx:252, 367-375` |

**它之所以能这么做，前提是**：`valueOf` 三轴都有后端真值，且 `totalRequests === 0` 时显式降级为 `AxisNoData`（`model-table.tsx:242-247`），`intelligence` 本身就是目录常量不是测出来的（`routing.ts:5` 的 `intelligenceRank`）。

**我们的评分机制在语义上不同**：我们的节点是**免费公益节点的可用性**，不是"模型质量"。可用的信号是**成功率、连续失败、冷却时长、直连/走节点的形态**。**没有"智能度"这一轴，也不该有。** 所以形态要重设计，不能照抄三轴。

### 8.2 推荐形态：**「列表列 + 分档徽章 + 排序入口 + 行内下钻」四件套，第一期不做独立视图**

```
┌─ 节点健康 ─────────────────────────────────────────────────────────────┐
│  [全部 128] [可用 96] [配额冷却 22] [故障 10]     ⟳ 10s   [测速]        │
│                                                                        │
│  ┌─ 失败形态 ───────────────────────────────────────────────────────┐  │
│  │  全部分母含直连：成功 71.3%  │  经过节点 82.1%  │  直连·闸门拒绝 57.9% │  │
│  │  直连·连不上 12.4%  │  直连·其他 30.2%                            │  │
│  └──────────────────────────────────────────────────────────────────┘  │
│                                                                        │
│ 名称▲        评分 ▼   成功率(n)    请求数  异常/切换  延迟   冷却至    │
│ hk-01        92       96.4% (3.2K)  3,241     12      48ms   —        │
│ ▓▓▓▓▓▓▓▓▓░   ▓▓▓▓▓▓▓▓▓                                          │
│ jp-02        78       88.1% (1.9K)  1,902     41      62ms   14:20    │
│ ▓▓▓▓▓▓▓▓░░   ▓▓▓▓▓▓▓░░                                          │
│ us-03        41       52.0% (240)     240     58     210ms   16:05    │
│ ▓▓▓▓░░░░░░   ▓▓▓▓▓░░░                                          │
│ 直连·连不上  —        19.8% (1.2K)  1,214    512      —       —      │
│ ░░░░░░░░░░   无节点记录（点击查看原因）                          │
└────────────────────────────────────────────────────────────────────────┘
```

**四个要素各自的形态与依据**

| 要素 | 形态 | 依据 freellm | 我们的改造点 |
| --- | --- | --- | --- |
| **① 列表列** | 新增 5 列：`评分`（轴条+数字）、`成功率`（**必须带样本量 n**）、`请求数`、`异常/切换`、`冷却至` | `AxisBar`（`model-table.tsx:28-40`）＋ `AxisNoData`（`:47-57`）＋ 对齐表头 `ModelTableHead`（`:93-124`） | **样本量必须和成功率同格显示**。freellm 用独立列 `totalRequests` 并在 `=== 0` 时整列降级；我们把 `96.4% (3.2K)` 合成一格，就是为了避免"52.0% 看起来比 96.4% 差"却不知道它只有 240 个样本 |
| **② 分档徽章** | `<60` 红 / `60–79` 橙 / `≥80` 绿；**不做无级彩虹** | `penalty-inspector.tsx:70-76` 的四档阈值（`>=8/>=5/>=3/>0` → red/orange/amber/emerald），**同一档一个色，不按数值连续插值** | 我们现在的节点状态徽章是**状态**（available/exhausted/dead），评分是**连续量**，两者不能共用一个 `.badge`；建议 `.score` 类另起，用**轴条底色**表达连续，用 badge 只在行内展开区给「优秀/一般/较差」三档文字 |
| **③ 排序入口** | 表头可点，默认按评分降序；前三名/后三名可用轻量标记；排序存 localStorage | `lib/table-sort.ts` 全文 + `sortable-header.tsx` | **默认排序必须是评分降序**，不是接口顺序。节点表的默认视图应该是"最该关心的排最前" |
| **④ 行内下钻** | 点行展开：评分构成（分子/分母/样本量）、最近 N 次失败的时间轴、当前冷却来源、**"为什么是这个分"的理由列表** | `PenaltyInspectorRow.reasons: ('penalty'\|'cooldown'\|'recent_errors')[]`（`penalty-inspector.tsx:52-66`）；请求详情的 failover ladder（`en.json:analytics.failoverLadder`） | 这是**评分可信度的唯一出口**。一个不解释自己的评分，运维不会相信它 |

**⑤ 无数据必须显式（最重要的一条）**

评分机制刚上线时，**所有节点都没有历史数据**。freellm 在这个场景上踩过坑并写进了注释（`model-table.tsx:42-46`）：

> *"...rendering those as bars reads as a real, suspiciously identical measurement, so unmeasured providers get this explicit placeholder instead."*

对应到我们：
- 无样本 → `—` 或 `无数据`，**绝不显示 0 分**（0 分意味着"最差"，与"不知道"语义相反）
- 无样本 → 虚线空条（`AxisNoData` 的 `border border-dashed`，`model-table.tsx:52`）
- 无样本 → **默认排序里放在哪？** 建议：**不参与评分排序，单独成组排在末尾**（按名称），因为把"未测量"混进"差"里会误导运维去切掉一个可能很好的节点

### 8.3 明确反对：**不要做独立的「节点健康」大图视图**

理由：
1. **评分是排序工具，不是分析工具。** 运维的动作是"把差的切掉/看它为什么差"，不是"欣赏分布"。雷达图/散点图回答不了"下一个该切谁"。
2. **我们已经有足够多的图了**：热力图、甜甜圈、趋势图、延迟分布条（`admin.html:516-523, 577`）。再加一个独立视图是**信息过载的边际递减**。
3. **freellm 自己也没做**。它把评分直接放进主表的列（`model-table.tsx:93-124`），只有在单模型详情页（`ModelDetailPage.tsx`，644 行）才展开。**主表给决策，详情页给诊断**——这个分工应该学。

### 8.4 与评分机制的接口契约（给评分实现者的输入）

面板需要评分机制提供的最小接口（**这只是 UI 需求，不是设计**）：

```jsonc
// GET /api/nodes 的新增字段（建议，附在每个节点对象上）
{
  "fingerprint": "ab12cd34",
  "name": "hk-01",
  "score": {
    "value": 0.92,              // 0..1，或 null = 无数据
    "samples": 3241,            // ★ 样本量，UI 必须显示
    "components": [             // ★ 可选的成分明细，UI 按数组长度决定渲染几根条
      {"key": "success_rate", "label": "成功率", "value": 0.964, "weight": 0.6},
      {"key": "stability",    "label": "稳定性", "value": 0.88,  "weight": 0.4}
    ],
    "reasons": ["recent_errors", "cooldown"]   // ★ 借鉴 penalty-inspector 的 reasons
  }
}
```

**设计原则（从 freellm 学到的）**：
- `components` 是**数组不是固定三字段** → UI 按长度渲染，前端不假设轴的数量（对应 `routing.ts:113` 的 `Row = FallbackEntry & Partial<RoutingScore>` 容忍分量缺失）
- `samples === 0` 或 `value === null` → UI 走 `AxisNoData` 分支
- `reasons` 是**枚举不是自由文本** → UI 才能把它渲染成固定形状的提示

---

## 9. 面板大改候选方案（3 个方向 + 推荐 1 个）

### 方向 A：最小侵入 · 组件补齐

**做法**：保持现有 5 分区骨架（概览/节点池/订阅与配额/代理与模型/运行日志）与 `showPage()` 机制不动。只做 §6 第 2、4、5 件事：排序表头 + 骨架屏 + 带动作空态 + 栈式 toast + 二次确认 + 数字格式。

| 维度 | 评估 |
| --- | --- |
| 改动面 | 小（约 +250 行 JS/CSS，不动 HTML 结构） |
| 工时 | 最短 |
| 风险 | 最低 |
| **致命缺陷** | **它没有回答用户的问题。** 用户要的是"大改"，且明确要求先定信息架构——因为评分要落在上面。A 方向下，评分列要塞进现在只有 7 列的节点表（`admin.html:580`），而节点表所在的"节点池"页看不到调用日志的聚合数据，评分只能显示一个孤零零的数字，没有下钻、没有失败细分、没有"为什么"。**评分机制做完还要返工。** |

### 方向 B：IA 重排 · 分区收敛（**推荐**）

**做法**：

1. **分区从 5 → 4**：
   | 新分区 | 吸收 | 行锚 |
   | --- | --- | --- |
   | 概览 | `page-overview` 的 4 状态卡 + Hero + 趋势图（热力图/甜甜圈/明细表下沉到「用量统计」tab） | `admin.html:457-516` |
   | **节点健康**（由"节点池"升级） | `page-nodes` + **调用日志的节点聚合**（从 `admin.html:2064-2092` 抽函数） | `admin.html:563-585` |
   | **配置**（由"订阅与配额"+"代理与模型"合并） | `page-subs` + `page-misc`，内部用现有 `#logTabRaw/#logTabCall` 式分段控件分「订阅/节点源」「代理」「模型映射」「网关」 | `admin.html:587-677` |
   | 运行日志 | `page-logs`（调用日志的「节点分析」tab 保留，但**与节点健康共用同一个聚合函数**） | `admin.html:678-746` |
2. 落地 §6 全部 5 件事，顺序见 §10。
3. **第一步就冻结「评分列 + 行内下钻」的位置**（§8.2），即使评分逻辑还没写，也先把列位和空态占好。

| 维度 | 评估 |
| --- | --- |
| 改动面 | 中大（HTML 结构有搬移，`showPage()` 与 `#pageTitle` 需同步；约 +600~800 行） |
| 风险 | 中：搬移概览页会让「Hero 五格与下方表/图共用同一 `/api/stats` 响应」的联动（`admin.html:464-466`）需要重新确认作用域 |
| 收益 | **最高性价比**：IA 一次定稿，评分有落点；改完仍是单文件、无构建、无新依赖 |
| 可分批 | 是。每一步都能单独验收（见 §10） |

### 方向 C：数据驱动的单页仪表盘重写

**做法**：放弃 `showPage()` 的「section 显隐」，改成一个统一的 `render(view)` 循环：所有数据放一个 `state` 对象，`render()` 全量重绘当前视图；视图之间变成 tab；组件全部函数化（`statCard()`、`dataTable()`、`axisBar()`、`emptyState()`）。

| 维度 | 评估 |
| --- | --- |
| 改动面 | 最大（2237 行里 JS 1488 行要重构成组件层，HTML 361 行要模板化） |
| 风险 | **高**：没有 e2e 测试网（仓库里没有 Playwright 用例文件，只有历史报告 `.superpowers/sdd/2026-09-28-admin-visual-port/task-6-report.md`），一次性重写没有回归保护；且 `discev_test.go:29-35` 会从 admin.html 抽 `CL_ISSUE_TYPES` 常量，重构易踩 |
| 收益 | 长期最高（组件复用、加评分列是加 10 行） |
| 结论 | **不建议作为第一步**。它的收益方向 B 也能拿到 80%，风险却是 3 倍。可以作为方向 B 完成后的**后续演进**（B 的第 1、2 步实际上已经在铺组件层） |

### ✅ 推荐：**方向 B**，并按下列理由

1. **它直接回答用户的问题**：IA 定稿、评分有落点、不用返工。
2. **它不需要任何新技术**：不引框架、不加构建、不加 npm、不增运行时依赖，全部是 CSS + 原生 JS。
3. **代价可控且可分批**：4 个分区可以一个一个搬，每搬一个验收一次；最坏情况下中途停下，面板仍可用（不像方向 C，中途停下就是半成品）。
4. **它吸收了两个方向的长处**：B 的组件层（`statCard/axisBar/emptyState/tableSort`）就是 C 的组件化的 80%，但不承担 C 的全量重写风险。

---

## 10. 实施顺序建议（步骤 / 依赖 / 验收）

> 原则：**每一步都能独立回滚、独立验收**；先做「无争议的能力层」，再做「有争议的 IA 搬移」，最后做评分列落位。

### 前置：冻结基线（0.5 天）

- 用 Playwright（本机已装：`playwright 1.64.1`，`~/.cache/ms-playwright` 有 `chromium-1232/1234`）对**当前** admin.html 的 5 个分区各截一张桌面（1440×900）+ 移动（390×844）截图，存为基线。
- 跑一遍 `go test ./...`（至少 `discev_test.go`）记录基线绿。
- **验收**：10 张基线截图 + 测试全绿输出存档。

---

### Step 1 · 设计令牌收敛 + 组件层落地（1~1.5 天，**无依赖**，可与 Step 2 并行）

**做什么**
- CSS：新增 `.s-ok/.s-warn/.s-bad` 三档状态色（双主题）；`body` 加 `font-variant-numeric: tabular-nums`；`.sk` 骨架类；`.seg` 分段控件；`.card-hd`（卡片头 + actions 槽）；`.empty-card`（虚线空态）
- JS：抽出 `sortRows/nextSort/loadStoredSort/persistSort`（照 `lib/table-sort.ts` 翻译）、`axisBar(value,color)`、`emptyState(...)`、`pushToast`、`skeletonRows(n)`
- 收缩内联样式：把重复出现的 `style="..."` 值收敛进类（现状 **170 处 `style="`**，`grep -o 'style="' static/admin.html | wc -l`）。**不做 100% 收敛**，只收敛重复 ≥3 次的模式

**依赖**：无
**验收**
1. `static/admin.html` 仍是单文件、`go:embed` 能编译（`go build ./...`）
2. 5 个分区截图与基线**目视无变化**（除 tabular-nums 带来的数字宽度微调）
3. `grep -c 'style="' static/admin.html` 数值下降，且没有新增 `!important`

---

### Step 2 · 表格能力层：排序 + 骨架 + 空态（1 天，**依赖 Step 1 的 `sortRows` 等函数**）

**做什么**：4 张表（节点表 / 节点分析 / 调用日志列表 / 订阅表）加可排序表头、加载骨架、带动作空态；排序状态持久化。
**依赖**：Step 1
**验收**
1. 点「成功率」表头两次 → 降序；刷新页面排序保持；第三次点击回默认顺序
2. 隐私模式（无 localStorage）下不抛错、本会话内排序仍生效
3. 用 Playwright `page.route` 给 `/api/nodes` 加 2s 延迟 → 出现**与真实表同形**的骨架，且数据到达后**页面无纵向跳动**
4. 节点页在空数据时出现带「去添加订阅」按钮的空态，点击能 `showPage('subs')`
5. `go test` 全绿

---

### Step 3 · 节点池 → 节点健康（1.5~2 天，**依赖 Step 2**）

**做什么**
- 把 `clRenderNode()`（`admin.html:2064-2092`）的聚合逻辑抽成 `aggregateByExitNode(records)`，日志页与节点页共用
- 节点表新增 `真实成功率(n)` / `请求数` / `异常·切换` 三列
- 表上方加「失败形态」细分条（经过节点 / 直连·闸门拒绝 / 直连·连不上 / 直连·其他）
- 行展开（`<tr class="expand"><td colspan=N>`）显示该节点最近失败时间轴 + 当前冷却
- 节点页操作按钮（`admin.html:572-576`）保持原位，避免用户肌肉记忆失效

**依赖**：Step 2（排序、骨架、空态）＋ **一个前置确认**：`/api/call-log` 的记录里有没有 fingerprint（见 §6 第 1 件风险）。没有就按名字聚合 + 显式标注。

**验收**
1. 任一节点的「请求数」与日志页「节点分析」同名的数字**完全相等**（同一函数，用 Playwright 同时打开两页比对文本）
2. 三类直连细分加总 = 直连记录总数（写一个 `console.assert` 或临时断言）
3. 无调用日志时三列显示 `—`
4. 表格在 390px 宽下不横向爆版（或明确降级为「只显示名称+评分+成功率」的窄表）
5. 展开/收起不影响排序与筛选状态

---

### Step 4 · IA 重排：5 分区 → 4 分区 + 概览 tab 化（1.5~2 天，**依赖 Step 1**，建议在 Step 3 之后以免两次改概览）

**做什么**
- 新增导航项「节点健康」「配置」，移除「节点池」「订阅与配额」「代理与模型」
- 「配置」分区内部用 `.seg` 分段控件切「订阅/节点源 / 代理 / 模型映射 / 网关」
- 概览页 tab 化：「概览」（状态卡 + Hero + 趋势）/「用量统计」（热力图 + 甜甜圈 + 两张表）
- `showPage()` 与 `#pageTitle/#pageCrumb`（`admin.html:445`）同步更新；tab 状态存 localStorage

**依赖**：Step 1（分段控件、卡片头）
**验收**
1. 5 个旧分区的内容全部能在 4 个新分区里找到（做一张「旧 id → 新 id」映射表，逐项打勾）
2. **Hero 五格的时间窗/模型筛选联动仍然生效**（对 `admin.html:464-466` 的联动做一次手工验证：切时间窗 → 五格与两张表同步变化）
3. 1440×900 下概览 tab 无需滚动即可看到趋势图标题
4. 所有 nav 按钮的 `.active` 高亮与页面内容一致
5. 移动端（390px）导航可用

---

### Step 5 · 评分列落位（1 天，**依赖 Step 3 + Step 4**）

**做什么**
- 节点表加入 §8.2 的 `评分` 列（轴条 + 数字）与默认按评分降序
- 无数据走 `AxisNoData` 形态（虚线空条 + 「无数据」），**排序时单独成组置末**
- 行展开区新增「评分构成」（分子/分母/样本量 + `components` 明细 + `reasons` 理由）
- 若评分后端尚未就绪：**先接一个显式的 `null` 分支**，让全表显示「无数据」占位——这本身就是验收项

**依赖**：Step 3（行展开容器）＋ Step 4（表位定稿）
**验收**
1. 后端返回 `score.value = null` 时：全表显示虚线空条 + 「无数据」，**没有任何 0 分或 0%**
2. 后端返回部分节点有分、部分无分时：有分的按降序在前，无分的按名称在末尾成组
3. 展开任一行能看到「为什么是这个分」：样本量 + 各成分值 + 理由枚举的中文文案
4. 评分排序与主观判断一致（抽样 5 个节点人工核对：分数高的成功率/异常数确实更好）

---

### Step 6 · 收尾：全局错误面 + 二次确认 + 复制反馈（1 天，**无强依赖**，可随时插入）

即 §6 第 4、5 件事。放在最后是因为它是"扩散式改动"（要动所有写调用点），等前面的结构稳定后一次做完，避免重复改。
**验收**：见 §6 第 4/5 件的验收条款。

---

### 依赖图

```
[基线冻结]
    │
    ▼
[Step 1 令牌+组件层] ──┬──► [Step 2 表格能力] ──► [Step 3 节点健康] ──┐
                       │                                              ├──► [Step 5 评分列]
                       └──► [Step 4 IA 重排] ─────────────────────────┘
                                                                      │
[Step 6 错误面/确认/复制] ◄──── 可随时插入（建议最后）─────────────────┘
```

**关键路径**：Step 1 → Step 2 → Step 3 → Step 5（约 5~6.5 天）
**总工期**：约 8~10 个工作日（单人、含验收）

---

## 11. 不确定 / 没查到的部分（**不许编，以下是明确的空白**）

| # | 不确定项 | 影响 | 建议怎么补 |
| --- | --- | --- | --- |
| 1 | **freellm 的移动端表格行为**。我只从 CSS/组件推断出「靠 `overflow-x-auto`，没做卡片化堆叠」，**没有实际在移动视口打开过它的 UI**（容器只暴露 47996 端口，我未做浏览器渲染验证，避免与"只读"约束冲突） | §4.6 的结论强度有限 | 如果要照搬它的移动端做法，用 Playwright 本机访问 `http://127.0.0.1:47996` 在 390px 下截图确认（只读操作） |
| 2 | **freellm 是否有分页/虚拟滚动**。我看到日志有 `LOG_PAGE_LIMIT`/`LOG_BUFFER_LIMIT` 增量游标（`lib/logs.ts`），但**模型大表在多少行时会卡，我没实测** | 影响我们节点表在 500+ 节点时的策略 | 造 500 条假节点测我们自己的表；freellm 的表现只能靠源码推断 |
| 3 | **`/api/call-log` 的记录里是否含 fingerprint** | **直接决定 Step 3 能否精确关联节点表与调用日志**（见 §6 第 1 件风险） | 看 `adminCallLogHandler` / `calllog.go` 的返回结构；或在本机跑一次 `curl`（只读） |
| 4 | **节点评分机制的输入信号与输出形态尚未定义**（这是 Lead 的下一个任务） | §8.4 的接口契约是**我按 UI 需求反推的建议**，不是既定设计 | 由评分机制的设计者确认/修订；UI 侧只保证「数组长度自适应 + null 降级」两条 |
| 5 | **freellm 的 `#580` / `#1047` / `#919` 等 issue 的完整内容**。我只能从代码注释里读到摘要 | 对理解它的设计取舍只有二手信息 | 可 `web_fetch` 上游仓库 issue；本机 `.git` 存在，`git log --grep` 可能也查得到，但我未展开（超出本次范围） |
| 6 | **我们面板的 Playwright 用例是否有历史遗留可复用**。仓库里没有 `.spec.ts/.spec.js` 文件，但 `.superpowers/sdd/` 下有 Playwright 验证报告（如 `2026-09-28-admin-visual-port/task-6-report.md`） | 影响 §10 的验收成本估算 | 找 Lead 确认当时的脚本是否留存（可能在 `/tmp` 或另一分支） |
| 7 | **`admin.html` 内 170 处内联 `style=` 的具体收敛收益**。我只统计了总数，**没有做「重复值聚类」** | Step 1 的工时估算偏保守 | 动工前跑一次聚类统计 |
| 8 | **freellm 的 `docs/` 里是否有公开的设计规范文档**。我看到了 `docs/architecture.md`、`docs/OVERVIEW.md`，**未通读**（本次优先读了源码与 CSS，因为要求"本机源码优先"且证据要能落到行号） | 可能遗漏它自述的设计原则 | 如需，补读 `docs/architecture.md` |
| 9 | **评分列的颜色分档阈值**（我建议 `<60 红 / 60-79 橙 / ≥80 绿`） | 这是**我的建议值，没有证据来源**——freellm 的 `penalty-inspector.tsx:70-76` 是对"惩罚值"（越大越糟）分档，方向与我们的健康分（越大越好）相反，我只是借用了「四档阈值 + 同档一色」的形式 | 由评分机制定阈值；UI 只负责「同档一色」的形式 |

---

## 附录 A · 证据索引（关键文件与行号）

**freellm（`/home/jingyx/freellmapi/`）**

| 主题 | 文件:行 |
| --- | --- |
| 设计令牌（亮/暗） | `client/src/index.css:55-88` / `:90-122` |
| 圆角刻度 | `client/src/index.css:44-52` |
| tabular-nums 全局 | `client/src/index.css:137` |
| 图表灰阶 | `client/src/index.css:74-78` / `:109-113` |
| 主题首绘脚本 | `client/index.html:7-24` |
| 导航结构 | `client/src/App.tsx:67-113, 226-383` |
| 内容列宽 | `client/src/App.tsx:412-420` |
| 全局错误面 | `client/src/App.tsx:46-64` |
| ErrorBoundary | `client/src/App.tsx:386-389` |
| react-query staleTime | `client/src/App.tsx:50-58` |
| 默认落地页 | `client/src/App.tsx:453` |
| 排序纯函数 | `client/src/lib/table-sort.ts:1-70` |
| 轴条 / 无数据 / 区间 | `client/src/components/model-table.tsx:28-40 / 47-57 / 70-89` |
| 分数列与 tooltip | `client/src/components/model-table.tsx:93-124, 242-252, 361-375` |
| 空态（带动作） | `client/src/components/empty-state.tsx:8-31` |
| 骨架 | `client/src/components/ui/skeleton.tsx:5-37` |
| 二次确认 | `client/src/components/confirm-button.tsx:12-16, 47-52, 63-79` |
| Tooltip（portal） | `client/src/components/tooltip.tsx:11-95` |
| Toast 队列 | `client/src/components/toaster.tsx:13-52`，`client/src/lib/toast.ts:24-40` |
| 分段控件 | `client/src/components/ui/segmented-control.tsx:5-46` |
| Badge 变体 | `client/src/components/ui/badge.tsx:8-28` |
| Stat / Panel | `client/src/pages/AnalyticsPage.tsx:325-343 / 345-358` |
| 统计行 + 骨架等量 | `client/src/pages/AnalyticsPage.tsx:733-760` |
| 空值降级（TTFT/p95） | `client/src/pages/AnalyticsPage.tsx:697-706, 869-871` |
| 日志轮询与暂停 | `client/src/pages/LogsPage.tsx:29-40, 203-208` |
| 惩罚分档色 | `client/src/components/penalty-inspector.tsx:70-76, 52-66` |
| 评分后端语义 | `server/src/services/scoring.ts:1-20` |
| `RoutingScore` 形状 | `client/src/lib/routing.ts:74-83`（RoutingScore）/ `:85-110`（RoutingData） |

**opencode2api（`/home/jingyx/project/opencode2api-test/`）**

| 主题 | 文件:行 |
| --- | --- |
| go:embed | `embed.go:7-11` |
| 设计令牌（暗/亮） | `static/admin.html:32-54` / `:55-66` |
| 侧栏 / 主区 | `static/admin.html:123 / :140` |
| 统计卡 / 卡片 / 表格 / 徽章 / chips | `static/admin.html:181 / :189 / :195 / :207 / :224` |
| Toast | `static/admin.html:287` |
| 移动端断点 | `static/admin.html:370, 386, 162, 179, 278-279` |
| 导航（5 分区） | `static/admin.html:428-434` |
| 概览分区 | `static/admin.html:457-561` |
| 节点池分区 | `static/admin.html:563-585` |
| 订阅与配额 | `static/admin.html:587-635` |
| 代理与模型 | `static/admin.html:636-677` |
| 运行日志 | `static/admin.html:678-746` |
| 自动刷新（仅概览） | `static/admin.html:806-820` |
| 节点表渲染 | `static/admin.html:1599-1618` |
| 调用日志加载 | `static/admin.html:1961-1990` |
| 真实成功率口径 | `static/admin.html:2000-2002` |
| 时段分析 | `static/admin.html:2050-2062` |
| 节点分析（含直连细分） | `static/admin.html:2064-2092` |
| 危险操作 confirm ×3 | `static/admin.html:1621, 1842, 2095` |
| 前端常量被 Go 测试依赖 | `discev_test.go:29-35` |
| 面板视觉移植史 | `docs/admin-layout-port.md:1-30` |

**运行 UI（只读抓取）**

| 事实 | 命令 |
| --- | --- |
| 入口 HTML 指向 `index-B8n16Tcf.js` / `index-B76V_SXP.css` | `curl -s http://127.0.0.1:47996/` |
| CSP 含 `script-src-attr 'none'` | `curl -s -D- -o /dev/null http://127.0.0.1:47996/ \| grep -i content-security` |
| CSS 101,781 B / JS 1,682,736 B | `ls -la /tmp/fl_index.css /tmp/fl_index.js` |
| 11 个 `@font-face`（全部 woff2，无 CJK） | `grep -o '@font-face' /tmp/fl_index.css \| wc -l` → 11 |
| Tailwind v4.3.0 + 字号/间距刻度 | `head -c 6000 /tmp/fl_index.css` |
