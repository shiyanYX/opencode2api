# LLM 网关 / 可观测平台前端基准调研

> 调研时间：2026-10-26。方法：GitHub API 拉取仓库 tree，逐仓库读取前端 `package.json`、路由/导航、总览页、日志页、主题文件；不依赖印象。star 数为调研当日 GitHub API 返回值。
>
> 说明：只有 Portkey、Cloudflare、OpenRouter 使用官方文档截图/文档描述（闭源）；其余项目的界面结论均来自仓库源码，未获取到截图的会明确写「源码，无截图」。分支统一为 `main`，Bifrost 为 `dev`。

---

## 1. songquanpeng/one-api

**1. 仓库与前端路径**
- 仓库：https://github.com/songquanpeng/one-api （37,248 stars）
- 同一仓库内置 3 套可切换前端，这是它最特殊的地方：
  - `web/default/`：Semantic UI + Recharts，默认后台
  - `web/berry/`：MUI + ApexCharts，仿 Berry 模板
  - `web/air/`：Semi UI + VChart，带侧边栏的现代版
- 关键文件：`web/default/src/App.js`、`web/default/src/components/Header.js`、`web/default/src/pages/Dashboard/index.js`、`web/default/src/components/LogsTable.js`；`web/berry/src/routes/MainRoutes.js`、`web/berry/src/menu-items/panel.js`、`web/berry/src/views/Dashboard/index.js`；`web/air/src/components/SiderBar.js`、`web/air/src/pages/Detail/index.js`。

**2. 技术栈**
- default：React 18 + react-router 6 + Semantic UI React + Recharts 2。
- berry：React 18 + MUI 5 + Redux + ApexCharts（`react-apexcharts`）+ Sass。
- air：React 18 + Semi UI + `@visactor/vchart` 1.8 + Semi 主题适配。
- 三套都是 CRA（`react-scripts`），无 TypeScript 强约束。

**3. 信息架构**
- default：顶栏横向菜单（渠道 / 令牌 / 兑换 / 充值 / 用户 / 数据看板 / 日志 / 设置 / 关于），移动端折叠为侧滑；`Header.js` 的 `buttons` 数组按 `isAdmin` 过滤。
- berry：左侧 Sidebar + 顶部 Header，菜单在 `menu-items/panel.js`，一级为「首页 / 渠道 / 令牌 / 日志 / 兑换 / 充值 / 用户 / 我的 / 设置」；`MainRoutes.js` 使用 `/panel` 前缀。
- air：Semi `Nav` 左侧栏，一级为「首页 / 渠道 / 聊天 / 令牌 / 兑换 / 充值 / 用户 / 日志 / 数据看板 / 绘图 / 设置」，按后端开关（`enable_data_export`、`enable_drawing`、`chat_link`）动态显隐。
- 三种导航的共同点：管理项与个人项混排，没有分组标题；渠道/用户/设置靠 `isAdmin` 直接隐藏。

**4. 总览页**
- default `pages/Dashboard/index.js`：**没有真正的 KPI 数字卡**。顶部是 3 列 `Grid`，每列一张 120px 高的折线卡（请求数 / 额度 / Token）；下方是「按模型堆叠柱状图」。汇总值 `todayRequests / todayQuota / todayTokens` 计算了但渲染被注释掉。
- berry：3 张等宽（`lg=4`）的 `StatisticalLineChartCard`，下方 `lg=8` 的柱状图 + `lg=4` 的辅助卡。
- air `pages/Detail/index.js`：VChart 饼图（模型调用次数占比，内外半径 0.8/0.5，图例在左，标签显示）+ 趋势图。
- 文字量很少：图表卡只有标题，没有解释性副标题；没有热力图/日历图。

**5. 表格 / 日志页**
- default `LogsTable.js`：筛选器在**表格上方的 `Form` 内**，不是页面级 header；字段包括 token 名、模型名、时间范围、渠道 ID、用户名，再叠一个关键词搜索。筛选按钮触发查询，没有「自动应用」。
- 列密度高但无列显隐/列宽拖拽；分页是传统页码；行内没有展开，靠跳转编辑页。
- 移动端没有卡片降级，表格横向滚动。

**6. 图表用法**
- default：Recharts；X 轴 `interval: 0` 强制显示所有日期标签、`minTickGap: 5`、左右 padding 30；**Y 轴 `hide`**；网格只保留水平线、`opacity 0.1`；折线 `dot: false`、`activeDot r=4`。Tooltip 是白底、无边框、4px 圆角 + 阴影，内容为「日期 + 值 + 指标名」。
- 饼图：VChart，`outerRadius 0.8 / innerRadius 0.5 / padAngle 0.6`，图例 `orient: 'left'`，`label.visible: true`，hover 时半径放大到 0.85。
- 明确的 10 色柱状调色板（紫/青/紫/绿/橙/粉/翠绿/淡紫/珊瑚/天蓝）。

**7. 主题与设计令牌**
- 三套前端都没有统一 token 系统：default 用 Semantic 默认主题 + 局部 CSS；berry 用 MUI theme；air 用 Semi 主题。
- 暗色能力弱/不统一，没有 CSS 变量体系；圆角、字号、间距各页各写。
- 字体走系统栈；没有暗/亮双主题的完整设计令牌。

**8. 可抄 / 不要抄**
- 可抄：
  1. 单仓多前端（default/berry/air）证明「后端 API 稳定时，前端可替换」，但只适合作为迁移期策略。
  2. 折线卡把 Y 轴隐藏、只留趋势形状，适合窄 KPI 卡。
  3. 饼图内外半径 + 左侧图例 + 标签，信息密度适合「模型占比」。
- 不要抄：
  1. 三套前端并行维护导致功能/样式漂移（default 有 Dashboard，berry 是另一套，air 又是另一套），重构应只保留一套。
  2. 筛选器塞在表格上方表单里、需要点「查询」才生效；应改为页面级 toolbar + URL 状态。

**9. 截图**
- README 截图（渠道/令牌页）：https://user-images.githubusercontent.com/39998050/233837954-ae6683aa-5c4f-429f-a949-6645a83c9490.png 、https://user-images.githubusercontent.com/39998050/233837971-dab488b7-6d96-43af-b640-a168e8d1c9bf.png
- 总览页无官方截图，以上总览结论来自源码。

---

## 2. Calcium-Ion/new-api（现 QuantumNous/new-api）

**1. 仓库与前端路径**
- 仓库：https://github.com/Calcium-Ion/new-api ，API 会 302 到 https://github.com/QuantumNous/new-api （50,701 stars）
- 前端：`web/`，源码在 `web/src/`
- 关键文件：
  - 布局：`web/src/components/layout/components/app-sidebar.tsx`、`app-header.tsx`、`authenticated-layout.tsx`
  - 导航数据：`web/src/hooks/use-sidebar-data.ts`、`web/src/lib/nav-modules.ts`
  - 总览：`web/src/features/dashboard/index.tsx`、`components/overview/overview-dashboard.tsx`、`summary-cards.tsx`、`log-stat-cards.tsx`
  - 日志：`web/src/features/usage-logs/components/common-logs-filter-bar.tsx`、`usage-logs-table.tsx`、`common-logs-stats.tsx`
  - 表格基座：`web/src/components/data-table/layout/data-table-page.tsx`
  - 主题：`web/src/styles/theme.css`、`theme-presets.css`

**2. 技术栈**
- React 19 + TanStack Router / Query / Table / Virtual + Tailwind CSS 4 + Base UI（shadcn 风格）+ VChart 2 + Recharts 3.9。
- 状态：Zustand；主题：next-themes；i18n：i18next；动画：motion；构建：Rsbuild；图标：Lucide + Hugeicons。
- 关键依赖见 `web/package.json`。

**3. 信息架构**
- 顶栏（Logo / 全局搜索 / 主题 / 用户）+ 左侧可折叠 Sidebar。
- Sidebar 分组（`use-sidebar-data.ts`）：
  - Chat：Playground、Chat
  - General：Overview、Dashboard、API Keys、Usage Logs、Audit Logs、Task Logs
  - Personal：Wallet、Profile、Security & Access
  - Admin：Channels、Models、Users、Redemption Codes、Subscriptions、System Info、System Settings
- 特色是 **Vercel/Cloudflare 式「drill-in」侧边栏**：进入 System Settings 时整个侧栏切换成该工作区的二级导航，并显示「← Back to Dashboard」，而不是把二级菜单堆在根树里（`app-sidebar.tsx` 文件头注释明确写了这个模式）。
- 侧栏模块可由后端 `SidebarModulesAdmin` + 用户级配置双层裁剪（`use-sidebar-config.ts`）。

**4. 总览页**
- `/dashboard/overview` 是「上手 + 运行状态」页，不是纯 KPI 页：`OverviewDashboard` 组合了
  - 顶部 hero / 首次接入引导（含可复制代码）
  - `SummaryCards`
  - `PerformanceHealthPanel`
  - `UptimePanel`
  - `AnnouncementsPanel`
  - `ApiInfoPanel`
  - `FAQPanel`
- `/dashboard/models` 才是数据看板：`LogStatCards` + `ModelCharts` + `ConsumptionDistributionChart`。
- KPI 数量明确：`LOG_STAT_CARD_FALLBACK_KEYS` 共 **7 个**（count / quota / tokens / cache-read / cache-creation / average-rpm / average-tpm），断点布局 `grid-cols-2 sm:grid-cols-3 lg:grid-cols-7`；每个卡片含图标+标签+大数字，数字超过 7 字符自动缩写。
- 图表：调用趋势（area/bar）、调用次数分布（环图）、调用次数排行（bar）；**没有看到日历热力图**（overview 组件集内未出现 calendar/heatmap）。
- 文案量偏大：hero、引导步骤、FAQ、公告，说明它把「新用户上手」放在总览第一屏。

**5. 表格 / 日志页**
- 筛选器在**页面内容区顶部**的 `CommonLogsFilterBar` / `LogsFilterToolbar`，不是表格卡片内部；含时间范围（`CompactDateTimeRangePicker`）、类型 tab、模型、渠道、分组、用户名、Token、请求 ID、上游请求 ID，以及仅管理员可见的字段。
- 筛选状态写入 URL `search`（TanStack Router），刷新/分享可还原；顶部还有 3 个紧凑统计徽章 `Usage / RPM / TPM`（`CommonLogsStats`）。
- 常用日志列 11 个：时间、渠道、用户、Token、模型、流式、Tokens、费用、耗时、详情（`common-logs-columns.tsx` 的 header/accessorKey）。
- 分页服务端化；支持列显隐并持久化到 localStorage（`columnVisibilityStorageKey`）；桌面表格 + 移动端卡片列表双渲染（`data-table-page.tsx`、`common-log-mobile-card.tsx`）。
- 行内没有展开，详情走对话框/详情页。

**6. 图表用法**
- VChart spec：
  - 环图：`outerRadius 0.8 / innerRadius 0.5 / padAngle 0.6`，`legends.orient: 'left'`，`label.visible: true`，hover/selected 半径 0.85 + 黑描边；tooltip 只列当前扇区。
  - 堆叠 bar/area：`seriesField: 'Model'`，`stack: true`，`legends.selectMode: 'single'`；空数据时 `outerRadius: 0`、显示「No data available」。
  - 排行 bar：`xField: 'Model'`、`yField: 'Count'`、图例单选。
- 没有显式设置坐标轴刻度数，交给 VChart 默认密度；图例在左上/左侧，不放下方。
- 图表高度由 `chart-box`（210px）/`chart-box-lg`（280px）控制。

**7. 主题与设计令牌**
- `html.dark` + CSS 变量 + `@theme inline`；`--radius: 1rem`，再由 `--radius-sm/md/lg/xl/2xl/3xl/4xl` 派生，整体偏大圆角。
- 字体：`--font-ui-sans`（默认 `sans-serif`），另有 `--font-public-sans`、`--font-serif`（Lora + 中文宋体栈）；`--font-body` 由主题字体轴切换。
- 有 3 套主题预设（`theme-presets.css`），包括字体轴、圆角、图表色；`--chart-1..5` 语义化。
- 暗色不是简单反色：chart / sidebar / overview-accent 都有独立 token。

**8. 可抄 / 不要抄**
- 可抄：
  1. **7 格 KPI 条**（2/3/7 断点）+ 数字自动缩写，适合网关这种「请求/Token/缓存/RPM/TPM」指标密集场景。
  2. **侧栏 drill-in**：设置页不把 20 个二级项塞进根侧栏，进入后整栏切换 + 返回入口。
  3. 筛选状态进 URL；列显隐持久化；桌面表格/移动卡片双渲染。
  4. 日志页顶部「统计徽章条」（Usage/RPM/TPM）紧贴筛选器，先给上下文再给表。
- 不要抄：
  1. **两个 Dashboard**（Overview 与 Models）导致指标与入口重复；4 页面板应只有一个「总览」。
  2. 主题预设/字体轴过多（Lora/Public Sans/多套预设），管理后台不需要「换字体」这种内容型站点能力，维护成本高。
  3. 总览塞入 FAQ/公告/上手文档，容易把运行状态挤出首屏。

**9. 截图**
- 仓库 README 未提供前端截图；以上均来自源码（文件路径已列）。

---

## 3. BerriAI/litellm（litellm-dashboard）

**1. 仓库与前端路径**
- 仓库：https://github.com/BerriAI/litellm （62,046 stars）
- 前端：`ui/litellm-dashboard/`（Next.js App Router）
- 关键文件：
  - 布局：`src/app/(dashboard)/layout.tsx`
  - 导航：`src/components/leftnav.tsx`、`src/components/navbar.tsx`、`src/components/DashboardHeader.tsx`
  - 首页：`src/app/(dashboard)/home/page.tsx`、`home/_components/HomePage.tsx`
  - Usage：`src/app/(dashboard)/usage/page.tsx`、`_components/components/UsagePageView.tsx`、`overview/UsageStatStrip.tsx`
  - Logs：`src/app/(dashboard)/logs/page.tsx`、`src/components/logs/request/RequestLogsPanel.tsx`、`LogsTableToolbar.tsx`
  - 图表：`src/components/shared/charts/`
  - 主题：`src/app/globals.css`

**2. 技术栈**
- Next 16 + React 19 + Tailwind CSS 4 + shadcn/Base UI + Recharts 3.9 + TanStack Table / Virtual / Query。
- URL 状态：nuqs；主题：next-themes/ThemeContext；命令面板；`openapi-react-query`；ProseMirror（prompt 编辑）；Monaco 未在此包。
- 依赖见 `ui/litellm-dashboard/package.json`。

**3. 信息架构**
- 顶部 56px sticky Navbar（Logo / 版本 / Worker 切换 / 通知 / 主题 / 用户），左侧可折叠 Sidebar。
- Sidebar 分组（`leftnav.tsx`）：
  - **AI GATEWAY**：Home、Virtual Keys、Playground、Models + Endpoints、Agentic（Agents / Workflow Runs / Memory）、MCP Servers、Skills、Guardrails、Policies、Tools（Search Tools / Vector Stores / Tool Policies）
  - **OBSERVABILITY**：Usage、Model Insights、Cost Optimization、Logs、Lens、Guardrails Monitor
  - **ACCESS CONTROL**：Teams、Projects、Internal Users、Organizations、Access Groups、Budgets
  - **DEVELOPER TOOLS**：API Reference、AI Hub、Learning Resources、Response Cache、Experimental（Prompts / API Playground / Tag Management / Old Usage）
  - **SETTINGS**：Settings（Router Settings / Logging & Alerts / Admin Settings / Cost Tracking / UI Theme / Telemetry）
- 支持嵌套折叠、角色/许可过滤、命令面板；信息架构明显是「平台」而非「单网关后台」。

**4. 总览页**
- Home 是「使用量 + 动态 + 排行榜」：
  - PageHeader + 主 CTA「Create New Virtual Key」
  - `UsageStatStrip`：**4 格**，每格一条 56×16 级 sparkline：
    1. Spend（含预算提示）
    2. Total Requests（成功/失败拆开）
    3. Total Tokens（含 cache read 提示 + 「View token breakdown」Popover，解释 input/cache read/cache write/output）
    4. Avg cost / request + Avg latency（含 success rate）
  - 「What's new in LiteLLM?」3 张博客卡
  - `Leaderboard`：按 token 占比的模型排行，带「前半段 vs 后半段」变化
  - `UpdatesFeed` 右侧动态
- Usage 页是重分析页：日期范围 + 视图选择 + Overview/Errors/Endpoint/Entity 等 tabs，图表 + 多张排行表。
- 文案量适中：KPI 有 hint/popover 解释口径，但不写长段落。

**5. 表格 / 日志页**
- Logs 页是**页面级 tabs**：Request Logs / Audit Logs / Deleted Keys / Deleted Teams；权限控制显隐。
- Request Logs 的筛选全部在页面 header/toolbar：时间范围、搜索、列筛选、Live Tail 开关、排除内部健康检查开关；URL 可深链到单个 request/session（`logDetailRouting`）。
- 表格：TanStack Table，列筛选、排序、分页；分页有 session cursor 模式；详情用 `LogDetailsDrawer` 抽屉，不跳页。
- 列密度中等，默认页大小来自 `DEFAULT_PAGE_SIZE_OPTIONS`；行内不展开，抽屉承载 raw request/response。

**6. 图表用法**
- Recharts：
  - `StackedUsageChart`：堆叠 Bar，按 series 传 labels/colors；tooltip 每行「色块 + 名称 + 格式化值」，值为 0 的行不渲染，并可额外显示「· Total」；支持 linear/log scale。
  - `area_chart` / `line_chart` / `bar_chart` / `donut_chart` / `chart_legend` / `chart_tooltip` 独立封装，统一图例和 tooltip。
  - Home/Usage 的 sparkline 是自绘小图，不带坐标轴，只表达方向。
- 10 色模型调色板（粉/紫/橙/蓝/绿/红/青/黄绿/靛/橙），与排行榜共用。
- 图表标题由卡片 header 提供，不在图内写大段注释。

**7. 主题与设计令牌**
- `.dark` class + `@custom-variant dark`；oklch 语义变量（background/foreground/card/primary/muted/destructive/border/chart-1..5）。
- `--radius: 0.5rem`；Tailwind 4 + `tw-animate-css`；组件走 shadcn/Base UI。
- 字体默认 `ui-sans-serif, system-ui`（未强绑 webfont）；字号走 Tailwind 默认 scale。
- 有 `ThemeToggle`、自定义 Logo（亮/暗两张图）。

**8. 可抄 / 不要抄**
- 可抄：
  1. **4 格 KPI + sparkline + 口径 Popover**：数字给结论，sparkline 给方向，Popover 解释「token 都包含什么」——非常适合 opencode2api 的 Token/缓存指标。
  2. 日志页「页面级 tabs + 顶部 toolbar + Live Tail 开关 + 抽屉详情 + URL 深链」整套模式。
  3. KPI 里把成功/失败、预算、success rate 作为**副行**而不是再加卡片，控制卡片数量。
- 不要抄：
  1. 5 组 Sidebar + 多层嵌套 + 大量 beta/upgrade 入口；4 页面板会被撑成 20+ 项。
  2. Home 混入博客/更新流，管理后台不需要内容运营位。
  3. 「Old Usage」「Experimental」等历史入口并存，说明 IA 缺清理；重构时不要保留兼容菜单。

**9. 截图**
- README 未提供 dashboard 截图；结论来自源码。

---

## 4. Helicone/helicone

**1. 仓库与前端路径**
- 仓库：https://github.com/Helicone/helicone （6,238 stars）
- 前端：`web/`（Next.js Pages Router）
- 关键文件：
  - 路由：`web/pages/dashboard.tsx`、`web/pages/requests.tsx`
  - 布局：`web/components/layout/auth/authLayout.tsx`、`web/components/layout/auth/Sidebar.tsx`
  - Dashboard：`web/components/templates/dashboard/dashboardPage.tsx`、`gridLayouts.tsx`、`panels/`
  - Requests：`web/components/templates/requests/RequestsPage.tsx`
  - 表格：`web/components/shared/themed/table/themedTable.tsx`
  - 主题：`web/styles/globals.css`、`web/tailwind.config.ts`

**2. 技术栈**
- Next.js（Pages Router）+ React + Tailwind CSS 3 + shadcn/Radix + Recharts + `react-grid-layout`。
- 表格自研 `themedTable`（列拖拽、列宽调整、列显隐）；Filter AST 自研（`web/filterAST`）。
- 依赖里包含 Tremor 的 Tailwind content 路径，但 Dashboard 主图是 Recharts。

**3. 信息架构**
- 左侧 Sidebar + 内容区顶部 64px Header。
- Sidebar 一级（`Sidebar.tsx`）：
  - Dashboard
  - Requests
  - Segments（Sessions / Properties / Users / Cache / HQL）
  - Improve（Prompts / Datasets / Playground）
  - Monitor（Rate Limits / Alerts）
  - 另有 Settings / Org 切换等在底部/顶部。
- 典型「可观测平台」IA：先看聚合，再下钻到请求，再按 Sessions/Users/Properties 切维度。

**4. 总览页**
- 12 列响应式 grid（`gridCols = { lg: 12, md: 12, sm: 12, xs: 4, xxs: 2 }`），布局由 `INITIAL_LAYOUT` 固定：
  - 第 0 行：**4 个 KPI**，各占 3 列、高 1：Avg Cost/Req、Avg Prompt Tokens/Req、Avg Completion Tokens/Req、Avg Total Tokens/Req。
  - 第 1–3 行：Requests（6×3）、All Errors（3×3）、Top Providers（3×3）。
  - 第 4–6 行：Costs、Users、Top Models by Requests（各 4×3）。
  - 第 7–9 行：Latency、Time to First Token、Top Models by Cost（各 4×3）。
  - 后续：Country、Scores、Quantiles、Threats、Prompt/Completion/Total per min 等，共 **14+ 个面板**。
- 图型：面积图（成功/错误双渐变）、柱状图、排行列表、分位数图、国家地图/列表。
- 没有日历热力图；文字量很低，面板只有标题 + 图/列表。
- KPI 卡结构：左上 13px muted label + 右上 24px 图标 + 左下 20px 半粗数字（`metricsPanel.tsx`）。

**5. 表格 / 日志页**
- Requests 的筛选在**页面级 Header**：`Header` 左槽放 `ThemedTimeFilter` + `FilterASTButton`，右槽放 LivePill、导出等。
- Header 是 `h-16`、`border-b`、`bg-slate-100 dark:bg-slate-900`，与内容区固定分离。
- 表格：列可拖拽排序、可调整宽度、可显隐；默认 page size 25（`page_size` query，默认 25）；传统分页，不是无限滚动。
- 筛选是 AST（可嵌套 AND/OR），支持保存筛选；复杂度高，但表达力强。
- 行详情：请求行组件 + 侧栏/抽屉（由 table row 承载）。

**6. 图表用法**
- Recharts `AreaChart`：success/error 两条面积，`linearGradient` 从 0.8 透明度到 0.1；X 轴时间，Y 轴数值；tooltip 自定义。
- KPI 是纯文本，不放 sparkline；趋势集中在下方大图。
- 图表色来自 `lib/chartColors` 与 CSS `--chart-1..10`；暗色有独立 chart 色值。
- 坐标轴刻度数未显式限制（交给 Recharts 默认），不做图内大段标注。

**7. 主题与设计令牌**
- `.dark` class；HSL 三元组 CSS 变量（`--background: 210 40% 98%` 等），Tailwind 通过 `hsl(var(--...))` 消费。
- `--radius: 0.5rem`；`--chart-1..10` 在亮/暗各一套；sidebar 也有一套独立变量。
- 字体未在 Tailwind 里强绑 webfont（系统栈）；字号以 13px 辅助文字 + 20px 数字为主。
- 颜色语义齐全：primary/secondary/muted/subdued/accent/destructive/confirmative。

**8. 可抄 / 不要抄**
- 可抄：
  1. **4 KPI + 12 列面板网格**：第一行固定 4 个核心指标，下面按「流量 / 成本 / 延迟 / 质量」分组，每组 3 个面板。
  2. KPI 卡「标签小、数字大、图标右上」的极简结构，密度高但不拥挤。
  3. 日志页把时间筛选和 AST 筛选放在**页面 header**，与表格解耦；Live 状态单独一个 pill。
  4. 列拖拽/列宽/列显隐三件套，适合请求日志这种列多且角色差异大的表。
- 不要抄：
  1. **14+ 面板一次铺满**：首屏信息过载，4 页面板应默认只显示 4–6 个面板，其余折叠/可选。
  2. HQL/Segments 这类高级查询入口对中小网关过重；会把「日志」变成 SQL IDE。
  3. 传统页码 + 固定 25 条，在长日志场景不如 cursor/虚拟滚动。

**9. 截图**
- README 未提供界面截图；以上来自源码。

---

## 5. langfuse/langfuse

**1. 仓库与前端路径**
- 仓库：https://github.com/langfuse/langfuse （36,499 stars）
- 前端：`web/`（Next.js Pages Router）
- 关键文件：
  - 路由/导航：`web/src/components/layouts/routes.tsx`
  - 项目首页：`web/src/features/dashboard/ProjectHomePage.tsx`
  - Home dashboard 定义：`packages/shared/src/domain/home-dashboard.ts`
  - 网格：`web/src/features/widgets/components/DashboardGrid.tsx`
  - 图表库：`web/src/features/widgets/chart-library/`（`BigNumber.tsx`、`AreaChartTimeSeries.tsx`、`HistogramChart.tsx`、`PieChart.tsx`、`TopListChart.tsx`、`PivotTable.tsx`）
  - 表格/筛选：`web/src/features/tracing-tables/observations/ObservationsTable.tsx`、`web/src/features/filters/`
  - 主题：`web/src/styles/globals.css`

**2. 技术栈**
- Next 16 + React 19 + Tailwind CSS 4 + Radix/shadcn + tRPC。
- **不用 Recharts/ECharts**：图表是自研 D3 组件（`d3-scale`、`d3-shape`、`d3-hierarchy`、`d3-selection`、`d3-zoom`）。
- 网格：`react-grid-layout`；表格：自研 DataTable + TanStack 风格 store；筛选：自研 filter builder + 侧栏。

**3. 信息架构**
- 左侧主 Sidebar + 顶部项目/组织切换 + 全局命令菜单。
- 路由分组（`routes.tsx` 的 `RouteGroup`/`section`）：
  - Main：Go to…、Organizations、Projects、Home、Dashboards
  - Observability：Tracing、Sessions、Users、Topics、Alerts
  - LLM 工程：Prompts、Playground
  - 评估：Scores、Evaluators、Human Annotation
  - 数据：Datasets、Experiments
  - 其他：Update、Cloud Status、V4 Preview、Upgrade、Settings、Support
- 典型「产品矩阵」IA，一级项 20+；适合可观测平台，不适合 4 页网关。

**4. 总览页**
- Project Home 不是硬编码页面，而是渲染一个 **DashboardDefinition**：
  - `LANGFUSE_HOME_DASHBOARD_DEFINITION` 定义了 12 个 widget 的 12 列网格位置。
  - 第 1 行：3 张表（Traces / Model Costs / Scores），各 4×5。
  - 第 2 行：Traces+Observations 时间序列（6×6）、Model Usage（6×6）。
  - 第 3 行：Users、Scores over time。
  - 第 4 行：3 张延迟分位表（Traces / Generations / Observations）。
  - 第 5–6 行：Generation Latency、Score Analytics 全宽。
- 图型：BigNumber、面积/折线时间序列、直方图、饼图、TopList、PivotTable。
- 文字量：每张卡有标题 + 小号副标题/口径说明；整体偏「分析工作台」，不是运营大屏。
- 可以切换/peek 不同 Home dashboard（URL `?dashboard=`），可「Set default」。

**5. 表格 / 日志页**
- Traces/Observations 页使用 `Page` 包一层 header（标题、help、tabs），表格控制项通过 `showControlsInPageHeader` 放到页面 header。
- 筛选在**左侧筛选栏**（`SearchableTableFilterLayout` + `useSidebarFilterState`），不是表格上方横条；支持保存视图（saved views）、URL 状态、字段搜索。
- 表格：服务端分页、列选择、行选择/批量操作、peek 详情（右侧抽屉）、列格式器（date/id/duration/token/tags）统一封装。
- 行内不展开，详情走 peek view；支持把 trace/observation 的 URL 直接分享。

**6. 图表用法**
- 自研 D3；时间轴有明确的 **tick 预算**：`prepareTimeAxis` 默认 `maxTicks = 6`，最少 2 个；按容器宽度做 `equidistantPreserveStart` 稀疏化，标签格式按时间跨度切「月/日/时」单一级别，避免「日期+时间」每 tick 都显示。
- 图例/颜色：widget 自带 legend；图表类型有独立图标。
- Tooltip：自研，显示时间 + 各 series + 总量；支持下载 CSV。
- 网格行高按 16:9 比例计算（`MIN_ROW_HEIGHT = 58`），小于 1024px 自动堆叠。

**7. 主题与设计令牌**
- `.dark` class + `@custom-variant dark`；HSL 变量 + `@theme inline` 映射。
- `--radius: 0.5rem`；字号 token 细到 11/12/13/14/16/20/22px 并绑定行高与字重。
- **只有两个字重**：`--font-weight-regular: 400`、`--font-weight-bold: 600`；禁止 `font-medium/semibold` 漂移。
- 字体：系统 sans + IBM Plex Mono（mono），不加载 webfont。
- 关闭了未使用的 Tailwind 调色板（lime/cyan/sky/fuchsia/rose/neutral/stone/slate/zinc/indigo），强制语义色。

**8. 可抄 / 不要抄**
- 可抄：
  1. **Dashboard-as-widget-grid**：把总览拆成可注册的 preset widget（表 / 大数字 / 时间序列 / TopList），用 12 列网格排；新增指标只加 widget，不改页面骨架。
  2. **时间轴 tick 预算（max 6，按宽度稀疏）**：直接解决 opencode2api 趋势图在窄屏标签重叠的问题。
  3. 表格「页面 header 控制 + 左侧筛选栏 + 保存视图 + peek 抽屉」。
  4. 字号/字重强约束（400/600 两档）与语义色裁剪，适合暗/亮双主题长期维护。
- 不要抄：
  1. 20+ 一级导航和 Dashboard 编辑器；4 页面板没有这么多对象。
  2. 自研整套 D3 图表库的成本极高；opencode2api 已有 ECharts，不应重写。

**9. 截图**
- README 未提供 dashboard 截图；结论来自源码 + Home dashboard 定义。

---

## 6. open-webui/open-webui

**1. 仓库与前端路径**
- 仓库：https://github.com/open-webui/open-webui （156,775 stars）
- 前端：`src/`（SvelteKit）
- 关键文件：
  - 管理布局：`src/routes/(app)/admin/+layout.svelte`
  - 分析页路由：`src/routes/(app)/admin/analytics/+page.svelte`（重定向到 `/?settings=admin%3Aanalytics`）
  - 分析面板：`src/lib/components/admin/Analytics/Dashboard.svelte`
  - 图表：`src/lib/components/admin/Analytics/UsageChart.svelte`
  - 主题：`src/app.css`、`tailwind.config.js`

**2. 技术栈**
- Svelte 5 + SvelteKit 2 + Tailwind CSS 4 + bits-ui + `svelte-sonner`。
- 依赖含 Chart.js 4.5，但 Admin Analytics 的 `UsageChart.svelte` 是**手写 SVG 路径 + 自定义 tooltip**，不是 Chart.js 封装。
- 虚拟列表：`@sveltejs/svelte-virtual-list`；动画/图标自研。

**3. 信息架构**
- 主应用是左侧聊天 Sidebar；管理后台是独立路由 + 顶部横向 tabs：
  - Users / Evaluations / Functions（按开关）/ Settings
- Analytics 不是独立一级 tab，而是 **Settings 的一个子面板**（`?settings=admin:analytics`），入口较深。
- 管理页顶部 tab 是「下划线式」小号文字导航，不是侧边栏。

**4. 总览页（Admin Analytics）**
- 顶部工具条：时间范围（24h / 7d / 30d / 90d / YTD / 1y / all / custom）、用户组筛选；自定义范围显示两个日期输入。
- **6 个 KPI**：Total Tokens、Input、Output、Messages、Chats、Users；每个有 `label / value / exact`（hover 显示完整值）。
- 主图上方：指标切换 Tokens/Messages、视图切换 Volume/Activity、对比上一周期开关、粒度选择（10m/hourly/daily）、模型筛选。
- 主图下方：模型/用户两个 tab 的排行榜，含 Messages / Users / Chats 列 + 占比条（`style:width = value / metricTotal`）。
- 没有日历热力图，但 `Activity` 视图是**按时间 × 模型的行级活跃度/热力条**（`activityRowHeight` 28px 级别）。
- 文字量低：标题 + 控件 + 数字；没有解释段落。

**5. 表格 / 日志页**
- 该仓库没有请求日志表；Analytics 的「表」是模型/用户排行榜。
- 排行榜：可排序（Messages/Users/Chats 三个方向）、Top N、占比条、行内无展开；不分页。
- 管理页其他表格（Users 等）走通用 DataTable，但不在本次重点。

**6. 图表用法**
- `UsageChart.svelte` 手写 SVG：
  - X 轴标签数量按宽度动态：`chartWidth >= 480 ? [0, mid, last] : [0, last]`，即 **3 或 2 个标签**。
  - Y 轴不画刻度文字（由图形高度 + hover 表达），降低噪音。
  - Hover：按列命中，显示 tooltip 行（模型 + 值），并高亮当前列/点。
  - 对比模式：当前周期实线 + 上一周期虚线；`comparisonChange` 显示百分比变化。
  - 图例限制可见系列数（2 行），其余归入「其他」/可滚动。
- 颜色用 `modelColor()` 按模型名稳定映射，不随排序变化。

**7. 主题与设计令牌**
- Tailwind CSS 4 + `app.css`；字体 `Inter`（含 Vazirmatn 等本地化回退）、`JetBrainsMono`。
- 暗色用 `.dark` + `gray-850` 等自定义灰阶 + 大量 `.dark .xxx` 覆盖；还支持 `high-contrast` 高对比模式。
- 不是纯语义 token 体系，颜色类直接写 `text-gray-300 dark:text-gray-600` 的情况很多；圆角/间距走 Tailwind 默认。
- 优点是可读性/高对比做得细，缺点是换主题成本高。

**8. 可抄 / 不要抄**
- 可抄：
  1. **Token 拆成 Input/Output/Total 三张卡**，比只给一个 Total 更能解释成本。
  2. **对比上一周期**（实线 vs 虚线 + 百分比变化）和 **Volume/Activity 双视图**。
  3. X 轴只留 2–3 个标签的窄屏策略；图例限行 + 稳定模型配色。
  4. 排行榜行内占比条（值 + 百分比），比纯表格更易读。
- 不要抄：
  1. 手写 SVG 图表（路径/命中/tooltip 全自研）维护成本高；opencode2api 用 ECharts 更稳。
  2. 把 Analytics 藏进 Settings 子面板，管理员很难发现；应作为一级「总览」的一部分。
  3. 颜色类硬编码 `gray-850` 等，暗色覆盖散落；应统一 token。

**9. 截图**
- README 有产品 GIF：https://community.s3.openwebui.com/uploads/users/6d849fdd-b93f-411e-a611-eb25c73f1154/2026/10/25aebd3e-1d1c-4177-98b3-317ce60f75e9.gif
- 该 GIF 是聊天主界面，不含 Admin Analytics；Analytics 结论来自源码。

---

## 7. maximhq/bifrost

**1. 仓库与前端路径**
- 仓库：https://github.com/maximhq/bifrost （9,105 stars，分支 `dev`）
- 前端：`ui/`
- 关键文件：
  - 布局：`ui/app/workspace/layout.tsx`、`ui/components/sidebar.tsx`
  - Dashboard：`ui/app/workspace/dashboard/page.tsx`、`components/overviewTab.tsx`、`components/tabViews/*`、`components/charts/*`
  - Logs：`ui/app/workspace/logs/page.tsx`、`views/logsTable.tsx`、`views/logsHeaderView.tsx`、`views/metricStrip.tsx`、`views/logsVolumeChart.tsx`、`sheets/logDetailsSheet.tsx`
  - 筛选侧栏：`ui/components/filters/logsFilterSidebar.tsx`
  - 主题：`ui/app/globals.css`

**2. 技术栈**
- React 19 + TanStack Router / Table + Recharts 3.8 + Tailwind CSS 4 + shadcn/Radix + next-themes。
- 状态/数据：Redux Toolkit + RTK Query（`@/lib/store`）；URL 状态：nuqs；数字动画：`@number-flow/react`；流程图画布：`@xyflow/react`。
- 依赖见 `ui/package.json`。

**3. 信息架构**
- 左侧 Sidebar（可折叠为图标）+ 内容区，No 顶栏大导航。
- Sidebar 分组：
  - Home
  - Observability：Dashboard、LLM Logs、MCP Logs、Agent Logs、Connectors、Logs Settings
  - Models：Model Catalog、Model Providers、Budgets & Limits、Routing Rules、Complexity Router、Circuit Breaker、Pricing Overrides、Model Settings
  - MCP Servers、Agents、Plugins、Alerting、Governance、Config、RBAC 等
- 同样是「平台级」IA，但 Observability 组内把 Dashboard 与 3 类日志并列，层级清楚。

**4. 总览页**
- Dashboard 用 tabs：**Overview / Provider Usage / Model Rankings / Dimension Rankings / MCP**，tab 存在 URL。
- 顶部筛选：时间范围（默认 1h）、provider/model/virtual key 等，筛选状态全在 URL（nuqs）。
- Overview：MetricStrip + 多张图卡，每张图卡可独立切换 **bar/line**，图卡右上显示总量 + tooltip；卡片高度 260px（sm 330px）。
- 指标覆盖：volume、token、cost、model usage、latency、overhead、throughput；另有 provider cost/latency/throughput、cache token meter。
- 文案量低：卡片标题 + 总量 chip + 图表；没有长解释。
- 排行榜/维度榜单独 tab，避免都挤在 Overview。

**5. 表格 / 日志页**
- Logs 页：**左侧筛选栏**（`LogsFilterSidebar`）+ 右侧内容；内容顶部 `LogsHeaderView`（搜索、时间、导出、列配置、实时开关）+ `MetricStrip` + `LogsVolumeChart` + 表格。
- 筛选维度非常多：provider/model/status/virtual key/team/customer/project/routing rule/stop reason/tool/cache hit/metadata/request id/content search/min-max latency/cost/tokens，且全进 URL。
- 表格：TanStack Table + 列配置（`useColumnConfig`）、服务端分页、行点击打开 `LogDetailSheet`；会话可打开 `SessionDetailsSheet`；支持 fallback chain 子行（`chainChildrenPageLimit`）。
- 有实时日志 + 直方图联动，先看 volume 峰值再下钻。

**6. 图表用法**
- MetricStrip 的 sparkline：固定 **56×16**，无坐标轴，只表达方向；hover 行高 16px、tooltip 显示时间跨度 + 值；变化值带正负色。
- 主图 Recharts：`XAxis/YAxis` + 自定义 tooltip；图卡 header 显示「总量 chip」，点击/悬浮可看口径。
- 图表类型由 `chartTypeToggle` 控制，bar/line 切换不改数据查询。
- 颜色来自 `globals.css` 的 `--chart-success/-error/-warning/-token-input/-token-output`，暗色同一套语义 token。

**7. 主题与设计令牌**
- `.dark` class；oklch 语义变量（background/card/primary/muted/border/chart-*）。
- 字体 **Geist + Geist Mono**；`--radius` 派生 sm/md/lg/xl；字号 12/13.2/15.2/18/20/24/30px，行高绑定。
- 侧栏 item 高度 28px、字号 13px；`rounded-sm` 为主，整体紧凑。
- 有 `--text-*` token 与 chart 语义色，暗色不用手写 gray 覆盖。

**8. 可抄 / 不要抄**
- 可抄：
  1. **Dashboard tabs + 每图独立 bar/line + 总量 chip**：比「所有图塞一屏」更可控。
  2. **MetricStrip + Volume histogram + 表格** 的日志三段式；直方图先定位峰值。
  3. **左侧筛选栏 + URL 状态**，适合筛选维度多的请求日志。
  4. 详情用 Sheet，支持 fallback chain 子行和 session 维度。
- 不要抄：
  1. 30+ 一级/二级菜单（Models/MCP/Agents/Governance/RBAC…），对 4 页面板是灾难。
  2. 筛选维度一次全暴露（20+），应先给 4–6 个常用筛选，其余进「更多筛选」。
  3. 每个图都可切 bar/line 会增加认知负担；只对趋势图提供切换即可。

**9. 截图**
- README 未提供 dashboard 截图；结论来自源码。

---

## 8. Portkey-AI/gateway（含 Control Plane）

**1. 仓库与前端路径**
- Gateway 仓库：https://github.com/Portkey-AI/gateway （13,274 stars）
- **Gateway 仓库不含 Control Plane 前端源码**：tree 中没有 `web/`/`ui/`/`dashboard/` 应用（只有 cookbook 示例的 `components.json`）。Control Plane 是托管闭源产品 `app.portkey.ai`。
- 可核查的 UI 证据：官方文档 Analytics / Logs 页面 + 截图。

**2. 技术栈**
- Control Plane 未开源，**未获取到**框架/组件库/图表库信息；不能从 gateway（Rust/TS 网关）推断前端栈。

**3. 信息架构**
- 根据官方文档目录（非源码）归纳，控制台的一级对象包括：Analytics、Logs、Virtual Keys、Configs、Guardrails、Prompt Library、Integrations、Settings；按组织/工作区隔离。
- Analytics 内部用 tabs：**Overview / Users / Errors / Cache / Feedback / Summary**（`portkey.ai/docs/product/observability/analytics.md`）。
- Logs 是独立一级页面，按时间倒序列出请求。

**4. 总览页（Analytics Overview）**
- 文档描述：「cost、tokens used、mean latency、requests，以及 users 和 top models 信息」的 70,000ft 视图。
- 截图 `images/product/dashboard.png`（1024×588）显示：顶部筛选/时间条，KPI 数字 + 多图表面板；具体卡片数量/排列无法从源码确认。
- 另有 Users / Errors 独立分析页（截图见下），Errors 页会显示「被 gateway 策略 rescue 的请求数」。
- 未获取到热力图/日历图证据。

**5. 表格 / 日志页**
- Logs：时间倒序列表；每条含 timestamp、request type、LLM、tokens、thinking tokens、cost；多模态会显示图片。
- 点击行打开**侧面板**，展示完整 raw request/response；每条日志有唯一 URL，可直接分享。
- Status 列把 gateway 策略状态显式化：Cache（Disabled / Miss / Refreshed / Hit / Semantic Hit）、Retry（Not Triggered / Success on x tries / Failed）、Fallback（Disabled / Active）、Loadbalance（Disabled / Active）。
- 支持在日志上打 manual feedback；Config/Prompt ID 可点击跳到对应 playground。
- 筛选位置/分页方式文档未写，**未获取到**。

**6. 图表用法**
- 文档只给截图与指标定义，未给坐标轴刻度、图例、tooltip 实现；**未获取到**具体参数。截图可见多图表面板 + 顶部时间筛选。

**7. 主题与设计令牌**
- 闭源，**未获取到**暗色实现、圆角/字号/字体。

**8. 可抄 / 不要抄**
- 可抄：
  1. **Status 列把「缓存/重试/fallback/负载均衡」变成可见状态**，而不是只记一条 error；这对 opencode2api 的节点切换/重试日志非常契合。
  2. 日志详情侧面板 + 唯一 URL 可分享 + 从日志跳到 Config/Prompt。
  3. Analytics 按 Users / Errors / Cache / Feedback 分 tab，避免一个总览承载所有问题。
- 不要抄：
  1. 产品面铺得过大（Guardrails / Prompt Library / Integrations）；4 页面板不应复制。
  2. 具体 UI 细节未知，不能照搬截图里的视觉；只能借鉴信息结构。

**9. 截图**
- 总览：https://mintcdn.com/portkey-docs/nDVSDSKLH6tq-r5C/images/product/dashboard.png
- Users：https://mintcdn.com/portkey-docs/EKxolPDRqqOR-ZgU/images/product/users-analytics.avif
- Errors：https://mintcdn.com/portkey-docs/EKxolPDRqqOR-ZgU/images/product/errors-analytics.avif
- Logs 详情：https://mintcdn.com/portkey-docs/EKxolPDRqqOR-ZgU/images/product/product-2.avif ；Status 列：https://mintcdn.com/portkey-docs/_Cb_bj7tVjxcfwsu/images/product/product-3.avif
- 来源：官方文档 https://portkey.ai/docs/product/observability/analytics.md 、https://portkey.ai/docs/product/observability/logs.md

---

## 9. Cloudflare AI Gateway

**1. 仓库与前端路径**
- 产品：Cloudflare AI Gateway，控制台内嵌在 Cloudflare Dashboard；**不开源**。
- 文档源码：`cloudflare/cloudflare-docs` 的 `src/content/docs/ai-gateway/`（例如 `observability/analytics.mdx`、`observability/logging/index.md`）。
- 文档地址：https://developers.cloudflare.com/ai-gateway/

**2. 技术栈**
- 控制台闭源，**未获取到**前端框架/组件库/图表库。

**3. 信息架构**
- 文档明确路径：Cloudflare Dashboard → **AI Gateway** 页面 → 选择 gateway → **Settings / Analytics / Logs**。
- 一级是「gateway 实例列表」，进入单个 gateway 后按 Analytics / Logs / Settings 分。
- 另有 Costs、User Insights、Log classification 等观测子页（文档目录）。

**4. 总览页**
- Analytics 指标固定 5 类：**Requests、Token Usage、Costs、Errors、Cached Responses（缓存命中率）**；支持按时间过滤。
- 文档未给卡片排布/图表类型/热力图；**未获取到**布局细节。
- 文字量：文档是说明型，控制台实际文案未获取。

**5. 表格 / 日志页**
- 日志字段：prompt、response、provider、timestamp、status、token usage、cost、duration、user agent；命中 DLP 时还记录 action/policy；Guardrails 评估调用也进日志。
- 日志默认开启；可按 gateway 设置关闭，或用 `cf-aig-collect-log` 逐请求覆盖；`cf-aig-collect-log-payload` 可只存元数据、不存 prompt/completion。
- 日志/成本/保留策略有版本差异（新客户走 Workers Logs，老客户 Legacy Logs）。
- 表格筛选位置/分页/虚拟滚动：文档未写，**未获取到**。

**6. 图表用法**
- 文档未提供图表参数；**未获取到**刻度/图例/tooltip 细节。
- 可确认的只有指标定义（请求/Token/成本/错误/缓存命中率）与 GraphQL 对应字段。

**7. 主题与设计令牌**
- 闭源；**未获取到**。

**8. 可抄 / 不要抄**
- 可抄：
  1. **「默认记录 + 逐请求 opt-out + payload 可单独关闭」**的三层日志隐私模型；网关日志很可能包含 prompt，值得直接借鉴。
  2. 成本标注为「估算值」，同时允许 custom cost 覆盖；避免把估算成本说成账单。
  3. 固定 5 个观测指标（请求/Token/成本/错误/缓存命中率），少而稳。
- 不要抄：
  1. 把 Analytics/Logs/Settings 分散在 Cloudflare 大控制台的多层路由里；独立面板应保持一级可见。
  2. 日志保留策略按客户版本分裂（Legacy/Workers Logs）会带来长期兼容负担；新项目一开始就定一套。

**9. 截图**
- 文档页未直接暴露可抓取的 dashboard 截图（`<Render file="analytics-dashboard">` 组件）；**未获取到**截图 URL。
- 依据：https://developers.cloudflare.com/ai-gateway/observability/analytics/ 、https://developers.cloudflare.com/ai-gateway/observability/logging/ 、https://developers.cloudflare.com/ai-gateway/observability/costs/

---

## 10. OpenRouter（公开界面 / 文档）

**1. 仓库与前端路径**
- 产品：https://openrouter.ai ，前端**闭源**，无公开仓库。
- 可核查材料：官方文档的 Activity / Logs 页面、公开页面导航、文档内截图。

**2. 技术栈**
- **未获取到**（前端闭源）。

**3. 信息架构**
- 公开站点顶部导航：Models / Chat / Rankings / Docs 等；登录后 dashboard 侧有 **Activity、Logs、Keys、Credits、Settings**（文档链接可验证 `/activity`、`/logs`）。
- Activity 与 Logs 是两个一级页面：前者聚合，后者逐请求。
- Activity tabs：Overview / Trends / Explore / Guardrails；Logs tabs：Generations / Upstream Requests / Sessions / Videos / Batches。
- 所有 tab 共享同一日期范围与筛选状态，且**状态存在 URL**。

**4. 总览页（Activity Overview）**
- 5 个 headline 指标：**total spend、requests、token volume、blended cost per million tokens、cache hit rate**。
- Ranked cards：Top API keys、Top apps、Top users。
- 图表：usage by model、token breakdown（prompt/completion/reasoning）、prompt caching、usage type（credits vs BYOK）。
- 另有 Trends（请求/花费/Token 时间序列 + 「Trending」对比上一周期）与 Explore（可配置图表/表格构建器）。
- 文字量中等：每个 tab 有说明，但卡片本身以数字/图为主；有截图可核对。
- 未获取到热力图/日历图证据。

**5. 表格 / 日志页**
- 筛选条在**表格上方**，可按 user/model/provider/status/region/API key/modality 筛选，也可按 generation ID 精确跳转；筛选存 URL。
- **表格上方有请求直方图**（volume over selected range），先看峰值再下钻。
- 表格列（Generations）：date、model、final provider、generation ID、status、attempts、API key、latency；组织账号多一列 Org Member。
- 行点击打开 **generation detail sheet**，分 Overview / Provider Responses（瀑布）/ Usage（成本拆分）/ Prompt / Completion / Raw JSON；可跳 Chatroom 或 Playground 重放。
- 分页方式文档未写；**未获取到**是否虚拟滚动。

**6. 图表用法**
- 文档只说明图表内容，未给坐标轴/tooltip 参数；**未获取到**。
- 可确认：Overview 有模型用量、token 拆分、缓存、usage type 四类图；Trends 是时间序列；Explore 支持 bar/line/dot plot 三种视图、1–2 个分组维度、rollup、累计和。

**7. 主题与设计令牌**
- 闭源；**未获取到**。

**8. 可抄 / 不要抄**
- 可抄：
  1. **Activity（聚合）与 Logs（逐请求）严格分开**，但共享 URL 筛选；从卡片可直接「View logs」带着筛选下钻。
  2. **表格上方请求直方图**，把「峰值发现 → 行下钻」串起来。
  3. 详情 sheet 的分段：Overview / Provider Responses 瀑布 / Usage 成本拆分 / Prompt / Raw JSON；对网关排障非常实用。
  4. 5 个 headline 指标里包含 **cache hit rate** 与 **blended $/M tokens**，比单纯请求数更能反映代理质量。
- 不要抄：
  1. Explore 这种「自助 BI 构建器」对 4 页面板过重；固定 2–3 个图即可。
  2. 5 个 Logs tab（Videos/Batches）是 OpenRouter 多业务决定的，不应照搬。

**9. 截图**
- Activity Overview：https://mintcdn.com/openrouter-d02e98a0/b-qVnKH4eKPWIhkA/assets/features/activity/overview.png
- Logs Generations：https://mintcdn.com/openrouter-d02e98a0/b-qVnKH4eKPWIhkA/assets/features/logs/generations.png
- Generation detail sheet：https://mintcdn.com/openrouter-d02e98a0/b-qVnKH4eKPWIhkA/assets/features/logs/generation-details.png
- 来源：https://openrouter.ai/docs/guides/features/activity.md 、https://openrouter.ai/docs/guides/features/logs.md

---

## 横向对比表

| 项目 | 技术栈 | 导航 | 图表库 | 总览布局 | 日志页筛选位置 | 可抄点 |
|---|---|---|---|---|---|---|
| one-api (default/berry/air) | React 18；Semantic/MUI/Semi；Recharts/ApexCharts/VChart | 顶栏（default）或左侧栏（berry/air）；管理项混排 | Recharts / ApexCharts / VChart | 3 张 120px 折线卡 + 堆叠柱；无 KPI 数字 | 表格上方 Form，点按钮查询 | 隐藏 Y 轴的窄趋势卡；饼图内外半径 |
| new-api | React 19 + TanStack + Tailwind 4 + Base UI | 顶栏 + 左侧分组栏；设置页 drill-in | VChart 2 + Recharts 3.9 | 7 格 KPI 条 / 引导 hero / 运行健康 / 环图 | 页面内容顶部 toolbar，状态进 URL | 7 格 KPI；侧栏 drill-in；列显隐；移动卡片 |
| litellm | Next 16 + React 19 + Tailwind 4 + shadcn + Recharts | 顶栏 + 5 组可折叠左侧栏 | Recharts 3.9 | 4 格 KPI + sparkline + Leaderboard + 更新流 | 页面级 tabs + header toolbar + Live Tail | KPI 口径 Popover；日志 toolbar；抽屉详情 |
| Helicone | Next Pages + Tailwind 3 + shadcn + Recharts | 左侧栏 + 64px Header | Recharts + react-grid-layout | 4 KPI + 12 列 14 面板网格 | 页面 Header 左槽时间 + FilterAST | 4 KPI + 面板网格；列拖拽/缩放/显隐 |
| Langfuse | Next 16 + React 19 + Tailwind 4 + Radix + 自研 D3 | 左侧栏 + 组织/项目切换 | 自研 D3（BigNumber/Area/Histogram/Pie/TopList） | 12 个 preset widget 的 12 列网格 | 页面 header 控制 + 左侧筛选栏 + 保存视图 | widget 化总览；时间轴 max 6 tick；peek 抽屉 |
| open-webui | Svelte 5 + SvelteKit 2 + Tailwind 4 + bits-ui | 主聊天侧栏；Admin 顶部 tabs | 手写 SVG（Analytics） | 6 KPI + Volume/Activity 主图 + 排行 | 无请求日志表；排行榜不分页 | Token 拆 Input/Output；同比上一周期；2–3 X 标签 |
| Bifrost | React 19 + TanStack Router/Table + Tailwind 4 + Recharts | 左侧可折叠栏，Observability 分组 | Recharts 3.8 + 56×16 sparkline | tabs + MetricStrip + 每图 bar/line + 总量 chip | 左侧筛选栏 + header + 直方图 | 日志三段式（MetricStrip→Volume→表）；Sheet 详情 |
| Portkey（闭源） | 未获取到 | 文档归纳：Analytics/Logs/Virtual Keys/Configs/... | 未获取到 | Analytics tabs Overview/Users/Errors/Cache/Feedback/Summary | 文档未写 | Status 列显式化 cache/retry/fallback/loadbalance；日志侧栏 + 可分享 URL |
| Cloudflare AI Gateway（闭源） | 未获取到 | Cloudflare Dashboard → AI Gateway → 单 gateway → Analytics/Logs/Settings | 未获取到 | 5 指标（请求/Token/成本/错误/缓存命中率），布局未获取 | 文档未写 | 日志默认记录 + 逐请求 opt-out + payload 单独关闭；成本标注估算 |
| OpenRouter（闭源） | 未获取到 | 顶部站点导航 + dashboard 侧栏（Activity/Logs/Keys/Credits/Settings） | 未获取到 | 5 headline 指标 + ranked cards + 4 类图；tabs Overview/Trends/Explore/Guardrails | 表格上方筛选条 + 请求直方图 | Activity/Logs 分离但共享 URL 筛选；表格上方直方图；详情 sheet 分段 |

---

## 对 opencode2api 的 5 条可执行结论

> 现状基线：`static/admin.html` 已有概览（4 张状态卡 + Token hero + 趋势 + 日历热力图 + 环图 + 请求量表/模型表）、节点池、订阅与配额、代理与模型、运行日志（原始日志 + 调用日志列表/时段/节点视图）。以下建议均可在现有单文件 + ECharts 架构内落地。

1. **把「概览」收敛成一条固定叙事，不再叠加同义指标。**
   保留并重排为：`4 张节点状态卡`（现有 `#ovTotal/#ovHealthy/#ovExhausted/#ovDead`）→ `Token hero`（现有 `#tcTotalToken` + 五格：新增输入/Output/命中/吞吐/缓存命中率）→ `1 张主趋势卡`（Token/请求 tab）→ `热力图 + 环图` → `两张明细表`。删除 hero 里重复的「总请求数」大数字或把它降为 hero 右上角的次级信息；概览页不再新增 KPI 卡。参考 Helicone 的「4 KPI + 分组面板」与 litellm 的「4 格 strip + 副行」。

2. **把所有筛选统一到页面级 sticky toolbar，并写入 URL。**
   概览的 `#trendRangeSel/#trendModelSel`、日志的 `#logLevelChips/#logKeyword/#clDate` 目前分散在卡片内。建议统一成一条 toolbar：时间范围（24h/7d/30d/180d）→ 模型/节点 → 关键词 → 「实时」开关 → 导出；切换页面保留筛选。参考 Helicone `Header` 左槽放 `ThemedTimeFilter + FilterASTButton`、Bifrost 的 URL 筛选、OpenRouter 的「共享筛选状态」。

3. **日志页改成「页面 tabs + 上方直方图 + 表格 + 详情抽屉」四段，保留现有两种日志但统一交互。**
   tabs 仍为「原始日志 / 调用日志」；调用日志在 ≥1024px 用表格（时间、节点、模型、状态、耗时、Tokens、费用、重试/切换、请求 ID），窄屏才降级为现有卡片；表格上方放 24h 请求量直方图（点击柱子过滤该时段）；行点击打开右侧抽屉展示请求/响应/重试链，URL 可深链。参考 OpenRouter Logs（直方图 + 详情 sheet 分段）、Portkey（Status 列）、litellm（tabs + Live Tail + 抽屉）。

4. **图表只保留 4 种图型，并统一刻度/图例/Tooltip 规则。**
   图型：堆叠面积（Token by 模型）、柱状（请求量）、日历热力图（现有 `#heatmapChart`）、环图（模型占比）。规则：X 轴最多 6 个标签（窄屏 3 个，参考 Langfuse `maxTicks=6` / open-webui 2–3 个）；KPI 卡内 sparkline 隐藏坐标轴；图例最多 5 项 +「其他」；Tooltip 固定「时间 + 总量 + 各 series」；环图图例左侧 + 标签显示（参考 one-api/new-api）。删除图内重复标题和 emoji 前缀。

5. **把暗/亮主题从「逐条覆盖」升级为语义 token，并补齐状态组件。**
   现有 `:root`/`.dark` 变量已起步，建议固定：`--bg/--surface-1/--surface-2/--border/--text/--text-sec/--text-ter/--accent/--ok/--warn/--danger` + `--chart-1..6`，圆角只保留 8px（卡片）/6px（控件）两档，字号只保留 11/12/13/14/20/28，字重 400/600 两档。补三套状态：加载 skeleton、空态（无日志/无节点）、错误态（ECharts CDN 失败已有 `chart-load-fail`，扩展为统一 ErrorState）。参考 Langfuse 的 400/600 字重约束与语义色裁剪、Bifrost 的 chart 语义 token。

---

### 附：证据边界

- 所有「源码」结论均对应本文件列出的 GitHub 路径；未读取到的实现细节（如闭源产品的坐标轴参数、分页方式）已写「未获取到」。
- 截图中可见但源码不可见的 UI，仅用于 Portkey / Cloudflare / OpenRouter 三节，并标注来源为官方文档。
- 未修改任何代码，未提交 commit，未操作 docker/服务。
