# U2b · freellm 风格配方 + 我们页面的重画样例

> 交付三件：
> 1. **本文件** —— 照着做就能像的风格配方
> 2. `docs/mockups/u2b/style-specimen.html` —— 离线单文件样例，把我们真实的「概览」+「节点池」按 freellm 风格重画
>    （同目录另有 `verify.py` = 四条硬指标的可复现自测脚本，`README.md` = 截图逐张鉴权态标注）
> 3. `docs/mockups/u2b/*.png` —— 成对截图（**逐张鉴权态标注见 `docs/mockups/u2b/README.md`**）
>
> 边界遵守：freellm `http://127.0.0.1:47996` **只读**。**未登录**（该实例凭据不可得，且不允许猜；
> 核实过程见 §7 引用块）——只做匿名 GET 与截图，未调任何写接口，未走重置/注册流程。
> **未改** `static/admin.html`、**未改**任何 Go 文件；未碰生产容器；未用 8000 端口。

---

## 0. 三句话总结（像不像 / 差在哪）

1. **像。** 把三件事照搬过去就已经八成相似：**① 卡片 24px 圆角 + 1px 细描边 + 零阴影**、**② 数值一律 20px 半粗等宽数字、标签一律 11px 大写灰色**、**③ 页面是「顶部横条导航 + 72rem 居中列 + 一叠等宽卡片」，不是「侧栏 + 满宽主区」**。
2. **差在三处，且都是我们**主动选择**的差异**：我们的导航是 204px 左侧栏（freellm 是顶部横条）；我们的卡片带阴影、值字号 30px（freellm 是 0 阴影、20px）；我们的表格单元格 10/12px 内边距（freellm 是 8px）。这三处不改也能"像"，改了才"是"。
3. **最大的意外收获**：freellm 更像"排版克制"而不是"配色漂亮"——它的中性色是**纯灰阶、零彩度**，主色亮色近黑（`oklch(0.22 0 0)`）、暗色近白（`oklch(0.93 0 0)`），**整页唯一的彩色只出现在状态语义和图表系列上**。所以「照 freellm 改」的真正内容不是换配色，而是**把颜色管起来**。

**一句话给用户**：样例在 `docs/mockups/u2b/style-specimen.html`，双击就能开；对着 `docs/mockups/u2b/` 里 `specimen-*` 和 `freellm-*` 两组图看即可判断。

---

## 1. 页面构图配方

### P1 · 页面骨架是「横条导航 + 居中列 + 卡片流」

```
┌─ sticky 顶栏（h≈54px，1px 下边框，背景 = 页面底色 80% + 模糊）──────────┐
│ ● 品牌名       导航项 导航项 导航项…              ⌘K        头像菜单   │
└────────────────────────────────────────────────────────────────────────┘
         ┌──── max-width: 72rem (1152px)，左右内边距 24px ────┐
         │  页面标题 24px/600                                  │  ← 页头
         │  一句话描述 14px 灰                                 │
         │                              [分段控件：时间窗/标签]│
         │  ┌卡片──┐ ┌卡片──┐ ┌卡片──┐ ┌卡片──┐               │  ← 摘要行
         │  └──────┘ └──────┘ └──────┘ └──────┘               │
         │  ┌卡片────────────────────────────────┐             │  ← 主区块
         │  └────────────────────────────────────┘             │
         └─────────────────────────────────────────────────────┘
```

| 项 | 数值 | 出处 |
| --- | --- | --- |
| 内容列宽 | `max-w-6xl` = 72rem = **1152px** | `client/src/App.tsx:416`（`'mx-auto w-full max-w-6xl px-6 py-8'`） |
| 顶栏 | `sticky top-0 z-40 border-b backdrop-blur` + `bg-background/80` | `client/src/App.tsx:241` |
| 顶栏高度 | **实测 54px**（导航项 `padding:1rem .25rem` + 14px 文字） | 样例实测 `nav.h = 54`（§8） |
| 卡片间距 | `space-y-6` = **24px** | `FallbackPage.tsx:493`、`AnalyticsPage.tsx:732` |
| 页面纵向内边距 | `py-8` = **32px** | `App.tsx:416` |

**出处补充**：官方截图 `freellm-upstream-analytics-1280x989.png` / `freellm-upstream-models-1280x989.png`（见 §7）可以直接看到这个骨架。

### P2 · 一个页面 = 「页头 → 条件区块 → 摘要行 → 若干平级区块」，**不允许卡片套卡片**

freellm 的 Analytics 页从上到下（`client/src/pages/AnalyticsPage.tsx:712-900`）：

| 序 | 区块 | 角色 | 出处 |
| --- | --- | --- | --- |
| 1 | `PageHeader`（标题 + 描述 + 右侧 `SegmentedControl`） | 我是谁、现在什么口径 | `AnalyticsPage.tsx:713-728` |
| 2 | `Stat` 网格（2/4/4 列） | 一句话现状 | `AnalyticsPage.tsx:733-760` |
| 3+ | 一组平级 `Panel`（每个 = 卡头 + 图表/表格） | 明细 | `AnalyticsPage.tsx:345-358` |

- **规则**：区块是**兄弟关系**，不是父子关系。全页只有一个「容器」概念（`<main>`），卡片之间靠 `space-y-6` 分隔，**没有嵌套卡片**。
- **反例（我们自己）**：`static/admin.html:546-547` 把「热力图」和「甜甜圈」用 `style="margin:0;padding:16px"` 内联成两张并行小卡，又在外面套 `card` —— 层级靠内联样式堆出来。

### P3 · 摘要行是固定语法：**2 列（窄）→ 4 列（宽）**，绝不超过 4 列

```jsx
<div className="grid grid-cols-2 md:grid-cols-4 xl:grid-cols-4 gap-3">
```
出处：`client/src/pages/AnalyticsPage.tsx:735`（`gap-3` = 12px）

- 超过 4 个指标 → **换第二行**，不缩成 5~6 列。freellm Analytics 有 8~9 个指标，正好排两行 4 列。
- **反例（我们自己）**：`static/admin.html:162` 把 Token Hero 做成 `repeat(5,1fr)` 五格一行，还在同一屏内塞了 4 张状态卡。

### P4 · 何时用卡片、何时用表格

| 场景 | freellm 用什么 | 出处 |
| --- | --- | --- |
| 单个数字 / 一对数字 | **卡片**（`Stat`） | `AnalyticsPage.tsx:325-343` |
| 同一形状的多行对象（模型、密钥、请求记录） | **表格**（`Table`，可排序） | `ui/table.tsx`，`model-table.tsx` |
| 一段带时间的序列 | **卡片 + 图**（`Panel`） | `AnalyticsPage.tsx:765-790` |
| 需要解释的一行状态 | **可折叠卡**（`PenaltyInspector` / `ChainManager`） | `penalty-inspector.tsx:12-26` |

**规则**：表格**不额外加外框**——表头 `border-b`、行 `border-b`、最后一行无边框（`ui/table.tsx:23`：`[&_tr:last-child]:border-0`）。表格通常住在一张卡片里（`card > card-hd + card-bd`），由卡片提供外框，表格自己只画横线。

### P5 · 卡头是「标题 + 右侧槽」，不是「标题 + 换行说明」

```jsx
<div className="px-4 py-3 border-b flex flex-wrap items-center justify-between gap-2">
  <h3 className="flex items-center gap-2 text-sm font-medium"><Icon className="size-4 text-muted-foreground"/>{title}</h3>
  {actions}
</div>
<div className="p-4">{children}</div>
```
出处：`client/src/pages/AnalyticsPage.tsx:346-354`（实测：卡头 **46px** 高、内边距 `12px 16px`，见 §8）

- 图标 `size-4`(16px) **永远 muted 色**，只有文字是前景色。
- 右侧槽放：分段控件、时间范围、行内数字摘要。freellm 的预算卡把摘要写成 `剩余 266.7M · 占 381.0M 的 70%`（`token-usage-bar.tsx:47-62`），**数字加粗、单位不加粗**。
- **反例（我们自己）**：`static/admin.html:519` 用 `style="display:flex;gap:4px;background:var(--surface-2)..."` 内联画出分段控件。

---

## 2. 视觉规则

### V1 · 层级靠 **4 个东西**建立，**不靠阴影**

| 手段 | freellm 怎么用 | 数值 | 出处 |
| --- | --- | --- | --- |
| ① 字号 | 三档：11px 标签 / 14px 正文 / 20px 数值 | 见 V2 | 实测见 §8 |
| ② 颜色 | 前景色 vs muted 前景色 | `oklch(0.18 0 0)` vs `oklch(0.52 0 0)`（亮） | `index.css:57,67` |
| ③ 1px 细描边 | 卡片、表格行、输入框 | `ring-1 ring-foreground/10` | `ui/card.tsx:17` |
| ④ 背景色差 | 卡片 `#fff` / 页面 `#fdfdfd` / muted `#f5f5f5` | 实测见 §8 | `index.css:56,58,66` |
| ~~⑤ 阴影~~ | **几乎不用** | built CSS 里没有 `--shadow-*` 主题变量 | U1 §2.4 |

实测（样例，§8）：`.card` 的计算样式 `box-shadow: rgba(18,18,18,0.1) 0px 0px 0px 1px` —— 这是 `ring-1`，**不是投影**。

> **抄法**：`box-shadow: 0 0 0 1px rgba(18,18,18,.10)` 就是 freellm 的"边框"。它比 `border:1px` 好在不占布局尺寸（`box-sizing` 之外），卡片宽高不用 +2px。

### V2 · 字号只有三档，且**大字号极少**

| 角色 | 字号 / 行高 / 字重 | 出处 |
| --- | --- | --- |
| 标签（统计卡、表头之上） | **11px** / 1.4 / 400，`uppercase` + `tracking-wider` | `AnalyticsPage.tsx:329`（`text-[11px] ... uppercase tracking-wider`） |
| 正文 / 表格 / 导航 | **14px**。注意 `--text-sm--line-height = calc(1.25/.875)` = **20px**（不是 21px） | `/tmp/fl_index.css` `@layer theme` |
| 数值（统计卡） | **20px** / 1.25 / 600 | `AnalyticsPage.tsx:334`（`text-xl font-semibold`） |
| 页头标题 | **24px** / 600 / `tracking-tight` | `page-header.tsx:17`（`text-2xl font-semibold tracking-tight`） |
| 页面 body 默认 | 16px/24px | freellm 活实例实测（§8 `freellm_live_login.bodySize = 16px`） |

**全仓使用频次普查**（`client/src/pages` + `components`）：`text-xs`(12px) 362 次、`text-sm`(14px) 166 次、`text-[11px]` 89 次、`text-[10px]` 55 次；`text-lg` 3 次、`text-xl` 2 次、`text-2xl` 1 次。**≥20px 只有 3 处。**

**对照我们自己**：`.stat .v{font-size:30px;font-weight:700}`（`static/admin.html:184`）、`.uh-value{font-size:26px}`（`admin.html:156`）—— 比 freellm 粗一档。

### V3 · 数字：**等宽数字是全局默认**，不是逐处标注

- freellm：`body{font-feature-settings:'ss01','cv11','tnum'}`（`client/src/index.css:137`），**全站开**。
- 加数字的地方再叠 `font-mono` + `tabular-nums`：`Stat` 值（`AnalyticsPage.tsx:334`）、轴条数值（`model-table.tsx:35`）、告警 meta（我们的 `.alert .meta`）。
- **抄法**：给 `<body>` 开 `font-variant-numeric: tabular-nums`，把 `static/admin.html:78` 那个逐处 `.num` 类降级为"需要等宽**字形**时才加 `font-mono`"。

### V4 · 状态色只在两个地方出现，且**同档一色**

| 用途 | 色值（亮 / 暗） | 出处 |
| --- | --- | --- |
| 惩罚/健康分档 | red-600 `#dc2626` / red-400 `#f87171`；orange-600 `#ea580c`；amber-600 `#d97706` / amber-400 `#fbbf24`；emerald-600 `#059669` / emerald-400 `#34d399` | `components/penalty-inspector.tsx:70-76` |
| 三轴（可靠/速度/智能） | `#22c55e` / `#3b82f6` / `#a855f7` **硬编码，亮暗同色** | `components/model-table.tsx:102-108` |
| 日志级别 | debug=muted / info=sky-600 / warn=amber-600 / error=destructive | `pages/LogsPage.tsx:51-57` |
| 破坏性操作 | light `oklch(0.577 0.245 27.325)` = `#e7000b`；dark `oklch(0.704 0.191 22.216)` = `#ff6467` | `index.css:70,105` |

**规则**：状态色**不做连续插值**（不是 61 分和 62 分颜色不同），而是**分档**——同档一个色。freellm 的四档阈值是 `>=8 / >=5 / >=3 / >0 / =0`。

**反面**：这些色在 freellm 里是**硬编码的 Tailwind 类**，没有令牌化。我们抄的时候顺手令牌化（`.s-ok/.s-warn/.s-bad`），比它干净 —— 这一点 U1 §5.2 已建议。

### V5 · 卡片：`rounded-3xl` + `ring-1` + 无阴影，**一律 24px 圆角**

```jsx
"flex flex-col gap-4 overflow-hidden rounded-3xl bg-card py-4 ... ring-1 ring-foreground/10"
```
出处：`client/src/components/ui/card.tsx:12-19`

- 圆角刻度（`index.css:44-52`）：`--radius:0.5rem` → sm 4 / md 6 / lg 8 / xl 12 / 2xl 16 / **3xl 24** / 4xl 32。
- **卡片固定 3xl=24px**；按钮/输入 8px；表格容器 16px；徽章 999px。
- 实测（§8 样例桌面）：`.card` = `radius 24px`、`padding 0`（内边距在 `card-hd` 12×16 与 `card-bd` 16px 上）、`shadow = 0 0 0 1px rgba(18,18,18,.1)`。

**对照我们自己**：`--radius:12px`（`static/admin.html:49`），`.card` 还带两层投影（`admin.html:189`）。

### V6 · 徽章：**实心 = 状态，描边 = 元信息**

| 形态 | 用途 | 出处 |
| --- | --- | --- |
| 实心淡底 + 同色文字（`bg-*/10 text-*`） | 状态（可用 / 已耗尽 / 故障） | `ui/badge.tsx:14-18`（destructive 变体用 `bg-destructive/10 text-destructive`） |
| `outline`（`border-border text-foreground`） | 中立元信息（参数、上下文、能力） | `ui/badge.tsx:21`；截图 `freellm-upstream-models-1280x989.png` 里"0.7B params / 32K ctx / vision / tools"就是描边款 |

- 尺寸：`h-5`(**20px**) + `px-2`(8px) + `text-xs`(12px) + `rounded-4xl`(32px→视觉即胶囊)。出处 `ui/badge.tsx:8`，实测见 §8（`.badge` = 20px 高、radius 999px）。
- 状态点：`size-2`(8px) 圆点 + 文字。我们在样例里用 6px，属于我们的既有做法（`admin.html:207`）。

### V7 · 图标：16px、`muted` 色、只做**指向**不做**装饰**

- 卡头图标 `size-4`(16px) + `text-muted-foreground`（`AnalyticsPage.tsx:349`）。
- 统计卡图标 28px 方形底（`size-7`）+ `size-3.5`(14px) 图标（`AnalyticsPage.tsx:331-333`）。
- 按钮内图标 `size-3.5`(14px)（`ui/button.tsx:33`）。
- **规则**：图标永远比它旁边的文字**更淡**，不抢前景。

### V8 · 图表：**系列一律灰阶，且亮暗反转**

```css
--chart-1..5: oklch(0.28 0 0) … oklch(0.87 0 0)   /* 亮 */
--chart-1..5: oklch(0.92 0 0) … oklch(0.32 0 0)   /* 暗，反向 */
```
出处：`index.css:74-78` / `:109-113`。转 sRGB：亮 `#292929 #555555 #808080 #aeaeae #d4d4d4`；暗 `#e4e4e4 #b7b7b7 #808080 #555555 #333333`。

- 彩色只留给**失败/破坏性**那一条（`stroke="var(--destructive)"`，`AnalyticsPage.tsx:780`）。
- 网格线：`strokeDasharray="2 4"` + `stroke=var(--border)`；坐标轴线关闭（`axisLine={false}` 或 `stroke=gridStyle`）；字号 11px（`AnalyticsPage.tsx:514-517`）。
- **每个图必须能回答"这是哪个窗口、n 是多少"** —— 卡头或图例里写清（我们的样例：`窗口 今天 · n=4,532`）。

### V9 · 按钮：`rounded-lg` + `h-8` + `text-sm` + `font-medium`

出处：`client/src/components/ui/button.tsx:16-38`

| size | 高度 | 字号 | 内边距 |
| --- | --- | --- | --- |
| `default` | **h-8 (32px)** | 14px | `px-2.5` |
| `sm` | h-7 (28px) | 0.8rem (12.8px) | `px-2.5` |
| `xs` | h-6 (24px) | 12px | `px-2` |
| `icon` | 32×32 | — | — |

变体：`default` = 实心主色（近黑/近白）；`outline` = 描边 + 卡片底；`ghost` = 无边框，hover 才出 `bg-muted`；`destructive` = `bg-destructive/10 text-destructive`（**不是实心红**）。

### V10 · 分段控件（freellm 的"唯一惯用法"）

```jsx
"inline-flex gap-1 rounded-xl border p-1"  // 外框
// 激活段："bg-foreground text-background font-medium"
```
出处：`client/src/components/ui/segmented-control.tsx:24,31-33`
实测：段高 **30px**、圆角 8px、内边距 `6px 12px`、12px 字（§8）。

注释原文（`segmented-control.tsx:5-8`）：*"The dashboard's one segmented-control idiom (pill bar with a solid active segment), shared by the Keys tabs and the Analytics range toggle."* —— **整个面板只有一种分段控件**。

---

## 3. 密度与节奏（含实测值）

### D1 · 表格密度基线

| 项 | freellm | 出处 | 我们（现行） | 出处 |
| --- | --- | --- | --- | --- |
| 表头高度 | **40px**（`h-10`） | `ui/table.tsx:70` | `padding:9px 12px` + 11px 字 → ≈**37px** | `admin.html:196` |
| 表头水平内边距 | **8px**（`px-2`） | `ui/table.tsx:70` | **12px** | `admin.html:196` |
| 正文单元格 | **8px 全向**（`p-2`） | `ui/table.tsx:76-84` | `10px 12px` | `admin.html:197` |
| 正文字号 | **14px / 行高 20px** | `/tmp/fl_index.css` 的 `--text-sm--line-height` | **13px / 行高 20.8px** | `admin.html:195` + body `line-height:1.6` |
| **纯文本行高（算得）** | 20 + 8×2 + 1 = **37px** | — | 20.8 + 10×2 + 1 = **≈42px** | — |
| **含操作按钮的行（实测）** | — | — | 样例实测 **41px**（按钮 h-6=24px 决定） | §8 |
| 表头文字颜色 | **前景色**（不是灰的） | `ui/table.tsx:70`（`text-foreground`） | 三级灰 `--text-ter` | `admin.html:196` |
| 行 hover | `hover:bg-muted/50` | `ui/table.tsx:61` | `color-mix(surface-2 50%)` | `admin.html:202` |
| 行分隔 | `border-b`，末行无 | `ui/table.tsx:23` | `.tbl tbody tr:last-child td{border-bottom:none}` | `admin.html:203` |

> **差值**：切到 freellm 密度，**每行省 4~5px**。100 行省 400~500px —— 对节点池这种长表是可感知的。

**⚠️ U1 勘误**：U1 §3.4 把 `AxisBar` 写成「12px 宽 × 1.5px 高」。源码是 `h-1.5 w-12`（`model-table.tsx:32`），换算 **6px 高 × 48px 宽**。本样例用 48×6，实测 `axisBar = {w:48,h:6}`（§8）。以本节为准。

### D2 · 一行能放几列

- 摘要卡：**2 列（< 768px）→ 4 列（≥ 768px）**，`gap-3`(12px)。出处 `AnalyticsPage.tsx:735`。
- 主区块（图/表）：**1 列 → 2 列（lg=1024px）**，`gap-6`(24px)。出处 `AnalyticsPage.tsx:763`（`grid-cols-1 lg:grid-cols-2 gap-6`）。
- 我们的实测：1440px 视口、72rem 列宽下，4 张统计卡各 **267px** 宽（§8）。

### D3 · 首屏信息量（我们怎么判"该减负"）

在 1440×900 下，样例「概览」首屏（不滚动）能看到：

| 区块 | 位置 | 高度 |
| --- | --- | --- |
| 页头（含分段控件） | top 0 | ≈ 100px |
| C-5 告警横幅 | — | 90px |
| 统计卡第一行（4 张） | top 294 | 97px |
| 用量卡 | top 407 | 158px |
| 统计卡第二行（4 张） | top 581 | 96px |
| 趋势卡（开始） | top 693 | — |

出处：§8 `firstFoldBlocks`。**首屏 5 个区块、不滚动就能看到趋势图的开头** —— 这就是"该减负"的量化标准：**首屏出现的"信息容器"不超过 5 个**。

**对照我们自己**：`static/admin.html:481-585` 的概览页有 **11 个 DOM 区块、5 张表**（`grep -o "<table" static/admin.html | wc -l` = 11 全页），首屏至少要滚两屏才看得完。

### D4 · 什么时候该减负（三条判据）

1. **同屏两个数字口径不同却都没标窗口** → 减负（U1 §6 风险 ②）。样例的做法：每个卡头写 `窗口 今天 · n=4,532`。
2. **一屏超过 4 个同级指标** → 拆成两行，不要加宽网格（P3）。
3. **首屏容器数 > 5** → 把"分析类"（热力图、分布）下沉到第二个 tab，首屏只留"状态 + 用量 + 趋势"（U1 §6 第 3 件）。

### D5 · 窄屏（390px）策略

freellm 在 < 768px 做三件事（`App.tsx:252,321`）：**导航收进下拉**、**网格降列**、**表格靠 `overflow-x-auto` 横滚**（`ui/table.tsx:11-14`）。它**不做卡片化堆叠**。

我们的样例实测：
- 概览页 `scrollWidth == clientWidth == 390` ✅
- 节点池 `scrollWidth == clientWidth == 390` ✅（表宽 753px，滚在 `.tblwrap` 里，页面不溢出）
- 390 下首屏只完整看到 **1 行**节点 → 窄屏必须靠"列裁剪 + 横滚"，并**明确告诉用户被裁了什么**。

---

## 4. 明暗两套模式的规则

### T1 · 令牌**同构**：同名令牌、不同值；不是"另起一套"

`client/src/index.css:55-88`（亮）与 `:90-122`（暗）**键完全一一对应**，令牌名一个字不差。

| 令牌 | 亮（sRGB） | 暗（sRGB） |
| --- | --- | --- |
| `--background` | `#fdfdfd` | `#080808` |
| `--card` | `#ffffff` | `#101010` |
| `--foreground` | `#121212` | `#f8f8f8` |
| `--muted` | `#f5f5f5` | `#1b1b1b` |
| `--muted-foreground` | `#696969` | `#989898` |
| `--border` | `#e4e4e4`（实色） | `rgba(255,255,255,.10)`（**半透明**） |

（hex 由 `oklch(L 0 0)` 经 OKLab→sRGB 转换得到，转换脚本与结果见 §8。）

**对照我们自己**：`static/admin.html:32-54`（暗）与 `:55-66`（亮）**键不同构** —— 亮色额外定义了一整套不同的语义色（`--ok:#0a6f42` 等），每加一个新颜色要配两遍。

### T2 · 边框在暗色下是**半透明白**，亮色下是实色灰

- 亮：`--border: oklch(0.92 0 0)` = `#e4e4e4`
- 暗：`--border: oklch(1 0 0 / 10%)` = `rgba(255,255,255,.10)`；`--input: oklch(1 0 0 / 14%)`

**为什么重要**：暗色下卡片 `#101010` 与页面 `#080808` 只差 8 级亮度，**靠实色灰边根本看不出来**；半透明白边会随底层自适应。样例实测（暗）：`.card` 的 ring = `rgba(255,255,255,0.1) 0 0 0 1px`（§8）。

### T3 · 主题在**首绘之前**确定，避免闪一帧

```html
<script>/* 在 <style> 之前 */
  const theme = localStorage.getItem('theme')
  const prefersDark = matchMedia('(prefers-color-scheme: dark)').matches
  const dark = theme === 'dark' || (theme !== 'light' && prefersDark)
  document.documentElement.classList.add(...)
  document.documentElement.style.colorScheme = dark ? 'dark' : 'light'
</script>
```
出处：`client/index.html:7-24`（必须放在 `<style>` 与 `#root` 之前）。

**我们的做法**：同样的手法，但走 `[data-theme]` 属性（`static/admin.html` 的既有策略），见样例 `<head>` 里的首绘脚本。

### T4 · 深色不是"反向"，是**降低对比度**

- 亮色前景/背景对比：`oklch(0.18)` on `oklch(0.995)`
- 暗色前景/背景对比：`oklch(0.98)` on `oklch(0.135)` —— 但**卡片只到 `oklch(0.175)`**，即卡片与页面底色差 **0.04 L**，非常接近；靠 10% 白边分开。
- muted 前景在暗色下**调亮**：0.52 → 0.68（不是调暗）。

---

## 5. 动效预算

### M1 · 只有一个时长

`--default-transition-duration: .15s`，`cubic-bezier(.4,0,.2,1)`（`/tmp/fl_index.css` 的 `@layer theme`）。全项目所有 `transition` 都用它。

### M2 · 只有两个动画

| 动画 | 用途 | 出处 |
| --- | --- | --- |
| `animate-pulse`（2s） | 骨架屏 | `ui/skeleton.tsx:9` |
| `tw-animate-css` 的入场 | dialog / dropdown 出现 | `client/src/index.css:2` |

**没有页面级转场**。对照我们自己：`static/admin.html:283` 有 `.page.active{animation:fadeIn .18s ease}`。

### M3 · 必须尊重 `prefers-reduced-motion`

freellm 未显式处理（这一点**我们比它做得好**：`static/admin.html:398-401` 已有 `@media (prefers-reduced-motion:reduce){.conn .pulse::after{animation:none}.page.active{animation:none}}`）。
样例沿用了这条，并把新加的告警高亮也纳进去。

> **U1 的结论在这里复核成立**：对 freellm 说"动效过度"是不成立的。它的动效预算比我们**更小**。

---

## 6. 看得见的样例：`docs/mockups/u2b/style-specimen.html`

### 6.1 怎么用（写给用户）

1. 双击 `docs/mockups/u2b/style-specimen.html` —— **file:// 直接打开，不需要服务器**。
2. 左上角切 **概览 / 节点池**（这是我们真实的两个分区）。
3. 右上角切 **明色 / 暗色**。
4. 拉到页面最底部 **「设计配方自检」** —— **点任意一条规则，页面对应区域会高亮并滚到那儿**。

### 6.2 样例里有什么（对照我们真实面板）

| 样例区块 | 对应我们面板的哪里 | 真实字段 / 数据 |
| --- | --- | --- |
| 顶部横条导航 | `.sidebar` + `.nav`（`admin.html:431-442`） | 5 个分区 |
| 4 张节点统计卡 | `.stats-row` 的 `.stat`（`admin.html:482-487`） | 节点总数 128 / 可用 115 / 冷却 9 / 故障 4 |
| 用量卡 | `.hero-usage-card`（`admin.html:491-514`） | `total_tokens` / `cache_read_tokens` / `avg_ttft_ms` / `throughput_tps` |
| 第二行统计卡 | Token Hero 五格 | `total_requests` / 成功率 / `avg_latency` / 缓存命中率 |
| 请求趋势 | ECharts 趋势卡（`admin.html:516-541`） | 内联 SVG 替代（零外部请求） |
| 模型请求分布 | 甜甜圈（`admin.html:546-547`） | `models{request_count}`，灰阶 5 级 |
| **C-5 告警横幅** | `.node-alert`（`admin.html:471-479`，**已由并发队友上移到全局位置**） | `node_alert{active,reason,since,window_minutes,window_attempts,window_successes,window_success_rate,pool_available,pool_total}` + `run_failures/run_minutes`；数值取自 `docs/research/C5-node-alert-replay.md` 的**真实回放结果**（2026-10-19 00:10:16 触发、10 分钟 226 次尝试 0 成功） |
| 筛选 chips | `#stateChips`（`admin.html:588-592`） | 全部 128 / 可用 115 / 已耗尽 9 / 故障 4 |
| 延迟分布 | `#latdist`（`admin.html:601`） | `latency_ms`（-1 = 超时） |
| **节点表** | `#nodeTable`（`admin.html:603-610`） | `name protocol state latency_ms cooldown_until last_error fingerprint active manual` |
| **评分列（新增）** | 现行面板**没有** | U1 §8 四件套：轴条 + 分档色 + 排序入口 + 「无数据」虚线占位 |
| 直连出口细分 | 调用日志的 `clRenderNode`（`admin.html:2131-2159`） | 三分类口径一字不改：`直连·闸门拒绝` / `直连·连不上` / `直连·其他` |

> **⚠️ 一处"撞车"需要说明**：本样例把 C-5 告警横幅放在**两个视图之上（全局位置）**，理由是「整层不可用」与用户停在哪一页无关。
> 在我做样例期间（2026-10-19 15:47），**并发的 C-5 队友恰好把真实面板里的横幅从 `#page-nodes` 内
> 上移到了 `.main` 内、所有 `<section class="page">` 之外**（见 `static/admin.html:465-479` 的注释，原文：
> 「原先嵌在 #page-nodes 里，停在默认首页『概览』的运维完全看不到它 —— 那正是 2026-10-19 八小时事故
> 『后端已告警、面板看着正常』的形态」）。
> **结论：样例与真实面板在这一点上现在是一致的**，不是我的额外发明。
> 本文档所有 `admin.html` 行号已按队友改动后的 **2308 行**版本重新核对（33 处引用全部命中，方法见 §8.4）。

### 6.3 规则 → 落点 对照表（页面里可点）

| 规则 | 落点选择器 | 在样例里看什么 |
| --- | --- | --- |
| R1 顶部横条导航 | `.nav` | sticky + 1px 下边框 + 当前项底边线 |
| R3 分段控件 | `.seg` | 外框圆角 12px，激活段**实心前景色** |
| R4 统计卡网格 | `#statNodes` | 2 列 → 4 列；label 11px 大写；值 20px/600 |
| R6 卡头带右侧槽 | `#usageCard` | 标题 + 右侧行内数字摘要 |
| R8 图表灰阶 + 标窗口/n | `#trendCard` | 灰阶系列 + 红色只给失败 + 卡头写窗口 |
| R9 徽章两种形态 | `.badge.outline` | 实心=状态、描边=元信息 |
| R11 C-5 告警横幅 | `#c5` | destructive 语义 + 圆角卡语法 |
| R12 筛选 chips | `#stateChips` | 激活=实心，保留我们的计数 |
| R13 表格密度 | `#nodeCard` | th 40px / td 8px / 行 border-b |
| R14 评分四件套 | `#nodeTable` | 轴条 + 分档 + 排序表头 + **无数据虚线** |
| R16 数字带窗口与 n | `#directCard` | 每个口径都标窗口与样本量 |

铺满全页、没有单一落点的规则：R2（无阴影分层）、R5（24px 圆角 + 1px ring）、R7（等宽数字）、R10（按钮 h-8/圆角 8px）、R15（窄屏列裁剪）。

### 6.4 样例**故意**和 freellm 不一样的地方（诚实清单）

| 项 | freellm | 样例 | 为什么 |
| --- | --- | --- | --- |
| 字体 | Geist Variable（自托管 11 个 woff2，**无 CJK 字形**） | 系统 CJK 栈 | 外部字体 = 破坏"零外部请求"；且 Geist 不含中文字形，对中文界面零收益（U1 §7.3） |
| 图表库 | recharts (React) | 内联 SVG | recharts 是 React-only，搬不进单文件 |
| 三轴（可靠/速度/智能） | 有 | **没有** | 我们的节点是"免费公益节点可用性"，没有也不需要"智能度"这一轴（U1 §8.1） |
| 告警横幅 | **没有这个组件** | 有，按它的卡片语法现造 | C-5 是我们真实有的能力（`admin.html:471-479`），freellm 是缺口（U1 §3.3） |
| 密度 | td 8px | td 8px（一致） | 但我们的行动按钮 h-6 使行高到 41px |
| 圆角 | 24px | 24px（一致） | 这是个**要下决心**的改动：24px 在高密度表区会吃掉有效面积 |

---

## 7. 成对截图

全部在 `docs/mockups/u2b/`。「像不像」就看前两组。

### A 组 · 我们的重画（样例）

| 文件 | 尺寸 | 说明 |
| --- | --- | --- |
| `specimen-desktop-1440x900-fold.png` | 1440×900 | **首屏，不滚动** —— 判断密度用这张 |
| `specimen-desktop-1440x900.png` | 1440×全页 | 概览视图全页 |
| `specimen-nodes-desktop-1440x900.png` | 1440×全页 | **节点池视图**（评分列在这里） |
| `specimen-dark-desktop-1440x900.png` | 1440×全页 | 暗色概览 |
| `specimen-dark-nodes-desktop-1440x900.png` | 1440×全页 | 暗色节点池 |
| `specimen-mobile-390x844.png` | 390×全页 | 移动概览 |
| `specimen-nodes-mobile-390x844.png` | 390×全页 | 移动节点池（表内横滚） |
| `specimen-mobile-menu-390x844.png` | 390×844 | 移动导航展开 |

### B 组 · freellm 参照

| 文件 | 尺寸 | 鉴权态 | 来源 | 说明 |
| --- | --- | --- | --- | --- |
| `freellm-upstream-analytics-1280x989.png` | 1280×989 | 已登录（**上游官方图**） | 仓库自带 `repo-assets/analytics.png` | 对应我们的「概览」：统计卡网格 + 图表面板 |
| `freellm-upstream-models-1280x989.png` | 1280×989 | 已登录（**上游官方图**） | 仓库自带 `repo-assets/models.png` | 对应我们的「节点池」：大表 + 轴条 + 徽章 |
| `freellm-live-AUTHGATE-login-1440x900.png` | **1440×900** | ⚠️ **未放行（登录闸门）** | **活实例实时截图** | 与样例同尺寸，可直接并排。**画面里没有任何业务数据** |
| `freellm-live-AUTHGATE-login-390x844.png` | **390×844** | ⚠️ **未放行（登录闸门）** | **活实例实时截图** | 移动档，同上 |
| `freellm-live-AUTHGATE-login-dark-1440x900.png` | 1440×900 | ⚠️ **未放行（登录闸门）** | 活实例（`localStorage.theme=dark`） | 暗色令牌的活体证据 |

> **鉴权态逐张标注（重要，勿混）**
> - `freellm-live-AUTHGATE-*.png` 是**登录闸门页**，**不是数据态**。它的价值只有一条：这是**活体渲染**的 freellm 设计系统（字体/令牌/圆角/按钮语法都是真的）。**不要拿它当"freellm 页面长什么样"的证据**。
> - `freellm-upstream-*.png` 是**上游仓库自带的官方产品图**（真实产品界面，但**不是本机实例**，尺寸 1280×989，与样例的 1440×900 不同）。
> - **本目录里不存在"登录成功后的活实例截图"**。原因是拿不到该实例的凭据，且不允许猜（见 §9 #1）。
> - 逐张标注同时写在 `docs/mockups/u2b/README.md` 里，避免脱离本报告单独看图时被误读。

> **为什么没有登录（三条路径的结论）**：Lead 放行了「读本地默认账号」这条路，我逐条试过并确认**这条路不存在**——
> ① `/home/jingyx/freellmapi/.env` 只有 `ENCRYPTION_KEY` 与 `PORT` 两个键（按 `=<redacted>` 打印键名，未回显值）；
> ② `.env.example` 全文检索 `admin/seed/password/default/credential/login` **无任何账号或默认口令**；
> ③ `CONTRIBUTING.md`/`README*.md`/`docs/` 检索默认账号 **无命中**；
> ④ `docker-entrypoint.sh`、`scripts/`、`server/src/` 检索"首次运行自动生成/打印口令" **无此逻辑**（唯一命中 `server/src/lib/reset-code.ts:20` 是**密码重置码**流程，属写操作红线，未触碰）；
> ⑤ 只读探测 `curl http://127.0.0.1:47996/api/auth/status` → `{"needsSetup":false,"authenticated":false,"email":null}` —— 该实例**已存在账号**，但无公开凭据。
>
> **红线遵守**：未猜口令、未爆破、未调 `/api/auth/setup`、未调 `/api/auth/login`、未走 `/forgot-password`、未改 freellm 任何状态；报告与截图中**不含任何密码**。
> 因此参照侧改用「上游官方截图（真实数据态，非本机）+ 活实例登录闸门页（同尺寸，非数据态）」的组合，而不是伪造或省略。

---

## 8. Playwright 自测原始输出

**环境**：Python Playwright 1.61.0 + Chromium 1234（`/home/jingyx/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome`）。
**脚本**：`docs/mockups/u2b/verify.py`（自测，**已随交付落盘，可复现**：`python3 docs/mockups/u2b/verify.py`）、
`/tmp/u2b_measure.py`、`/tmp/u2b_measure2.py`（量尺寸，临时脚本）。

### 8.1 自测结果（原始 JSON）

```json
{
  "A_desktop_1440x900": {
    "pageerror": [],
    "console_error": [],
    "requests_total": 1,
    "requests_non_file": [],
    "overflow": { "scrollWidth": 1440, "clientWidth": 1440 }
  },
  "B_mobile_390x844": {
    "pageerror": [],
    "console_error": [],
    "requests_total": 1,
    "requests_non_file": [],
    "overflow_overview": { "scrollWidth": 390, "clientWidth": 390 },
    "elements_wider_than_viewport": [],
    "menu_expanded_items_visible": 3,
    "overflow_after_menu": { "scrollWidth": 390, "clientWidth": 390 },
    "overflow_nodes_view": { "scrollWidth": 390, "clientWidth": 390 }
  },
  "C_freellm_live": {
    "freellm-live-login-1440x900": {
      "url": "http://127.0.0.1:47996/",
      "title": "FreeLLMAPI · Unified LLM Router",
      "overflow": { "scrollWidth": 1440, "clientWidth": 1440 },
      "pageerror": []
    },
    "freellm-live-login-390x844": {
      "url": "http://127.0.0.1:47996/",
      "title": "FreeLLMAPI · Unified LLM Router",
      "overflow": { "scrollWidth": 390, "clientWidth": 390 },
      "pageerror": []
    }
  }
}
```

**逐条对照任务要求**：

| 要求 | 结果 | 判定 |
| --- | --- | --- |
| `pageerror` = 0 | `[]`（桌面）/ `[]`（移动） | ✅ |
| `console.error` = 0 | `[]`（桌面）/ `[]`（移动） | ✅ |
| 外部请求 = 0 | `requests_total: 1`（就是 `file://…/style-specimen.html` 自身），`requests_non_file: []` | ✅ |
| 390px 无页面级横向溢出 | 概览 `scrollWidth 390 == clientWidth 390`；节点池 `390 == 390`；菜单展开后 `390 == 390` | ✅ |
| 无元素宽于视口 | `elements_wider_than_viewport: []` | ✅ |

> 注：上面 JSON 里的键名 `freellm-live-login-*` 就是现在的 `freellm-live-AUTHGATE-login-*`。
> 截图产出后为消除"这是数据页吗"的歧义做了重命名，**原始输出未改动**（下面贴的就是脚本当时打印的内容）。

### 8.2 几何与计算样式实测（原始 JSON，节选）

桌面 1440×900：

```json
{
  "body": { "fontSize": "14px", "lineHeight": "21px" },
  "nav":      { "h": 54,  "bg": "color(srgb 0.992157 … / 0.8)", "shadow": "none" },
  "navItem":  { "h": 53,  "padding": "16px 4px", "fontSize": "14px" },
  "statCard": { "w": 267, "h": 96, "radius": "24px", "padding": "12px 16px",
                "shadow": "rgba(18, 18, 18, 0.1) 0px 0px 0px 1px" },
  "statValue":{ "h": 25,  "fontSize": "20px", "fontWeight": "600" },
  "statLabel":{ "fontSize": "11px", "lineHeight": "15.4px" },
  "card":     { "w": 1104, "radius": "24px", "shadow": "rgba(18, 18, 18, 0.1) 0px 0px 0px 1px" },
  "cardHd":   { "h": 46, "padding": "12px 16px" },
  "cardTitle":{ "fontSize": "14px", "fontWeight": "500" },
  "badge":    { "h": 20, "fontSize": "12px", "radius": "999px", "padding": "0px 8px" },
  "segBtn":   { "h": 30, "fontSize": "12px", "radius": "8px", "padding": "6px 12px" },
  "btn":      { "fontSize": "14px", "fontWeight": "500", "radius": "8px", "padding": "0px 10px" },
  "th":       { "h": 40, "fontSize": "14px", "padding": "0px 8px" },
  "td":       { "h": 38, "fontSize": "14px", "padding": "8px" },
  "alert":    { "h": 90, "radius": "24px", "bg": "rgba(220, 38, 38, 0.1)" }
}
```

节点池视图（表格已显示，因此行高可测）：

```json
{
  "rowHeights": [41, 41, 41, 41, 41, 41, 41],
  "rowAvg": 41,
  "table":   { "w": 1104, "h": 327 },
  "axisBar": { "w": 48, "h": 6, "radius": "999px" },
  "axisVal": { "w": 28, "h": 17, "fontSize": "11px" },
  "alert":   { "w": 1104, "h": 90, "padding": "12px 16px", "radius": "24px" }
}
```

移动 390×844：

```json
{
  "statCard": { "w": 173, "h": 96, "radius": "24px", "padding": "12px 16px" },
  "card":     { "w": 358, "radius": "24px" },
  "alert":    { "w": 358, "h": 165, "radius": "24px" },
  "table":    { "w": 753 },
  "rowHeights": [41, 41, 41, 41, 41, 41, 41],
  "首屏完整可见行数": 1
}
```

> 注：表宽 753px > 视口 390px，但**页面级 `scrollWidth` 仍是 390** —— 横滚发生在 `.tblwrap{overflow-x:auto}` 内部，符合 freellm 的做法。

暗色 1440×900：

```json
{
  "card": { "bg": "rgb(16, 16, 16)", "radius": "24px",
            "shadow": "rgba(255, 255, 255, 0.1) 0px 0px 0px 1px" },
  "stat": { "bg": "rgb(16, 16, 16)", "shadow": "rgba(255, 255, 255, 0.1) 0px 0px 0px 1px" }
}
```

freellm 活实例（登录页）实测：

```json
{
  "bodyFont": "\"Geist Variable\", ui-sans-serif, system-ui, -apple-system, sans-serif",
  "bodySize": "16px", "bodyLineHeight": "24px",
  "bodyBg": "oklch(0.995 0 0)", "bodyColor": "oklch(0.18 0 0)",
  "firstHeading": { "tag": "H1", "size": "18px", "weight": "600", "color": "oklch(0.18 0 0)" }
}
```

→ **活体证据**：它的 `bodyBg` 就是 `oklch(0.995 0 0)`，与源码 `index.css:56` 完全一致；字体确实是自托管的 Geist。

### 8.3 oklch → sRGB 换算（本报告 §4 表里的 hex 怎么来的）

用 OKLab→线性 sRGB→sRGB 标准矩阵手算（脚本内联在本次会话中，无第三方库）：

```
oklch(0.995 0 0) = #fdfdfd      oklch(0.135 0 0) = #080808
oklch(0.98  0 0) = #f8f8f8      oklch(0.175 0 0) = #101010
oklch(0.97  0 0) = #f5f5f5      oklch(0.22  0 0) = #1b1b1b
oklch(0.92  0 0) = #e4e4e4      oklch(0.24  0 0) = #1f1f1f
oklch(0.55  0 0) = #717171      oklch(0.6   0 0) = #808080
oklch(0.52  0 0) = #696969      oklch(0.68  0 0) = #989898
oklch(0.18  0 0) = #121212      oklch(0.98  0 0) = #f8f8f8
图表灰阶 亮 #292929 #555555 #808080 #aeaeae #d4d4d4
图表灰阶 暗 #e4e4e4 #b7b7b7 #808080 #555555 #333333
oklch(0.577 0.245 27.325) = #e7000b   oklch(0.704 0.191 22.216) = #ff6467
```

---

### 8.4 `admin.html` 行号复核（因为并发改动，这一步不能省）

**背景**：我做样例期间，另一名 C-5 队友正在独立改 `static/admin.html`（Lead 已说明）。
该文件在我首次读取时是 **2300 行**，到我校对时已变成 **2308 行**，并且 **C-5 告警横幅整块从
`#page-nodes` 内上移到了全局位置** —— 我报告里最初那批行号**全部失效**。

**处理**（照"rebase onto the new content"的纪律做）：
1. `git status --porcelain -- static/ '*.go'` 看到 ` M static/admin.html`、` M node_alert.go`；
   `stat -c '%y'` 显示 mtime = `2026-10-19 15:47:50`，**不是我写的**（我本会话只写过
   `docs/research/U2b-*` 与 `docs/mockups/u2b/*`，没有任何一次写入指向 `static/` 或 `.go`）。
2. 用脚本按**内容**（不是按旧行号）在当前文件里重新定位每一个被引用的锚点。
3. 把报告里 33 处 `admin.html:NNN` 全部按新行号改写。
4. 再跑一遍锚点校验：对每处引用断言该行**确实包含**我声称的内容。

**校验输出（原始，节选；共 33 处引用，文件 2308 行）**：

```
共 33 处引用，文件 2308 行
  ✅ 全部落在文件范围内
  ✅ :49    期望含 '--radius:12px'             实际 '  --radius:12px; --radius-sm:8px;'
  ✅ :78    期望含 '.num{'                     实际 '.num{font-variant-numeric:tabular-nums}'
  ✅ :156   期望含 '.uh-value{'                实际 '.uh-value{font-size:26px;font-weight:700;...'
  ✅ :181   期望含 '.stat{'                    实际 '.stat{background:var(--surface);border:1px ...'
  ✅ :184   期望含 '.stat .v{'                 实际 '.stat .v{font-size:30px;font-weight:700;...'
  ✅ :189   期望含 '.card{'                    实际 '.card{background:var(--surface);border:1px ...'
  ✅ :195   期望含 '.tbl{'                     实际 '.tbl{width:100%;border-collapse:collapse;font-size:13px}'
  ✅ :196   期望含 '.tbl th{'                  实际 '.tbl th{text-align:left;font-size:11px;font-weight:600;...'
  ✅ :197   期望含 '.tbl td{'                  实际 '.tbl td{padding:10px 12px;border-bottom:1px ...'
  ✅ :223   期望含 'C-5'                       实际 '/* C-5 节点层告警横幅。必须有别于「某个节点故障」...'
  ✅ :225   期望含 '.node-alert{'              实际 '.node-alert{display:none;align-items:flex-start;...'
  ✅ :398   期望含 'prefers-reduced-motion'    实际 '@media (prefers-reduced-motion:reduce){'
  ✅ :431   期望含 '<aside'                    实际 '<aside class="sidebar">'
  ✅ :436   期望含 '<nav class="nav"'          实际 '  <nav class="nav" id="nav">'
  ✅ :471   期望含 'id="nodeAlert"'            实际 '  <div class="node-alert" id="nodeAlert" role="alert">'
  ✅ :482   期望含 'stats-row'                 实际 '    <div class="stats-row">'
  ✅ :491   期望含 'hero-usage-card'           实际 '    <div class="card hero-usage-card" style="padding:16p...'
  ✅ :516   期望含 'data-od-id="trends"'       实际 '    <div class="card" data-od-id="trends" style="padding...'
  ✅ :546   期望含 'chartHeatmap'              实际 '      <div id="chartHeatmap" class="card" style="margin:...'
  ✅ :588   期望含 'stateChips'                实际 '    <div class="filter-chips" id="stateChips">'
  ✅ :601   期望含 'latdist'                   实际 '    <div class="latdist" id="latdist"></div>'
  ✅ :603   期望含 'nodeTable'                 实际 '      <div style="overflow-x:auto"><table class="tbl" id...'
  ✅ :1137  期望含 'function renderNodeAlert'  实际 'function renderNodeAlert(a){'
  ✅ :1671  期望含 'function renderNodeTable'  实际 'function renderNodeTable(){'
  ✅ :2131  期望含 'function clRenderNode'     实际 'function clRenderNode(visible){'
  ✅ :2217  期望含 '香港 01'                     实际 "    {name:'香港 01',protocol:'vless',state:'available'..."
  ✅ :2239  期望含 'stats:{total_requests'     实际 '  stats:{total_requests:4532,throughput_tps:36.399...'
  ✅ :123   期望含 '.sidebar{'                 实际 '.sidebar{width:204px;flex-shrink:0;background:...'
  ✅ :140   期望含 '.main{'                    实际 '.main{flex:1;min-width:0;padding:32px 36px 72px;...'
  ✅ :519   期望含 'display:flex'              实际 '          <div style="display:flex;gap:4px;background:va...'

锚点核对： 全部通过
```

> **给 Lead 的提醒**：`static/admin.html` 在本次任务期间仍有并发写入。
> 若后续还有人在改它，**本报告的行号会再次失效** —— 引用时请以"选择器/函数名"为准，
> 行号只当作定位提示。真正的锚点是 `.node-alert`、`renderNodeAlert`、`clRenderNode` 这些**稳定的标识符**。

## 9. 不确定 / 没查到的部分

| # | 项 | 影响 | 怎么补 |
| --- | --- | --- | --- |
| 1 | **freellm 的 Analytics / Models 页在活实例里需要登录**，而该实例的凭据不可得（核实过程见 §7 引用块：`.env` 只有 2 个键、`.env.example`/文档无默认账号、无自动生成口令逻辑；`/api/auth/status` 返回 `needsSetup:false` 即账号已存在）。按红线不猜、不重置、不注册 | §7 B 组的「同尺寸对照」只能是**登录闸门页**，不是数据页 | 若用户本人愿意提供该实例的账号，可补拍 Analytics/Models 的 1440×900 与 390×844；或直接以上游官方截图为准（它就是官方产品图） |
| 1b | **截图鉴权态的标注规则**：`freellm-live-AUTHGATE-*.png` = 未放行的登录闸门页（**非数据态**）；`freellm-upstream-*.png` = 上游官方产品图（真实数据态，但非本机）；本目录**不存在**"登录成功后的活实例截图" | 脱离报告单看图片时容易被误读成 freellm 数据页 | 已同时写进 `docs/mockups/u2b/README.md` 的逐张标注表 |
| 2 | `repo-assets/analytics.png` / `models.png` 的**确切版本**未标注（不知道对应哪个 release） | 若上游改版，参照图会过期 | 与 `docs/CHANGELOG.md` 对照；本次未做 |
| 3 | **freellm 移动端的实际表格行为** 我只从源码（`ui/table.tsx:11-14` 的 `overflow-x-auto`）与登录页实拍推断，**没有在移动视口打开它登录后的表格页** | §D5 的结论强度有限 | 需要账号才能验证 |
| 4 | **`h-1.5 w-12` 的换算勘误**：U1 §3.4 写的是「12px 宽 × 1.5px 高」，实际 `w-12`=48px、`h-1.5`=6px。**本报告 §D1 已更正**，样例按 48×6 实现（实测确认） | U1 该处数字有误，其余不受影响（U1 的令牌表用的是 oklch 原值，正确） | 建议 Lead 在 U1 里加一行勘误指针 |
| 5 | **我们现行面板的行高是"算"出来的（≈42px），不是实测的**。因为 `static/admin.html` 需要登录态才有数据、且我不改它 | D1 的对比有一侧是推导值 | 用 Playwright 打开运行中的面板并登录后实测；或接受 ±1px 误差 |
| 6 | **24px 圆角在高密度表格区是否划算**，没有实测结论。样例在 1440px 下看着成立，但节点表 100+ 行时 24px 圆角 vs 12px 的差异未做 A/B | 是选型风险，不是事实风险 | 让用户在样例上直接看；不满意可把卡片圆角降到 16px（一行 CSS） |
| 7 | **freellm 是否在所有页面都无阴影**：我核了 `index.css`、built CSS（无 `--shadow-*` 变量）、`card.tsx`（用 `ring-1`），但没逐页核对 dialog/popover | 低 | popover/dialog 那类浮层大概率有阴影（合理），面板本体无 |
| 8 | **样例里的节点名/数值是演示数据**（取自 `static/admin.html:2217-2224` 的 `DEMO.nodes` 与 `DEMO.stats`），**字段名与告警数值是真的** | 不影响"像不像"的判断 | 如需真实数据，用生产面板导出后替换 |

---

## 附录 · 证据索引

**freellm 源码**（`/home/jingyx/freellmapi/`）

| 主题 | 出处 |
| --- | --- |
| 令牌亮/暗 | `client/src/index.css:55-88` / `:90-122` |
| 圆角刻度 | `client/src/index.css:44-52` |
| 全局等宽数字 | `client/src/index.css:137` |
| 图表灰阶 | `client/src/index.css:74-78` / `:109-113` |
| 主题首绘脚本 | `client/index.html:7-24` |
| 顶栏 + 列宽 | `client/src/App.tsx:237-248` / `:416` |
| 移动导航 | `client/src/App.tsx:252, 321-374` |
| 卡片 = 24px + ring + 无阴影 | `client/src/components/ui/card.tsx:12-19` |
| 表格密度 | `client/src/components/ui/table.tsx:11-14, 23, 61, 67-84` |
| 按钮尺寸 | `client/src/components/ui/button.tsx:16-38` |
| 徽章变体 | `client/src/components/ui/badge.tsx:8-28` |
| 分段控件 | `client/src/components/ui/segmented-control.tsx:5-46` |
| 骨架 | `client/src/components/ui/skeleton.tsx:5-37` |
| 轴条 / 无数据 | `client/src/components/model-table.tsx:28-40` / `:47-57` |
| 三轴色 | `client/src/components/model-table.tsx:102-108` |
| 分档色 | `client/src/components/penalty-inspector.tsx:70-76` |
| 日志级别色 | `client/src/pages/LogsPage.tsx:51-57` |
| 统计卡 / Panel | `client/src/pages/AnalyticsPage.tsx:325-343` / `:345-358` |
| 摘要网格 2→4 列 | `client/src/pages/AnalyticsPage.tsx:735` |
| 图表样式常量 | `client/src/pages/AnalyticsPage.tsx:514-517` |
| 预算卡行内摘要 | `client/src/components/token-usage-bar.tsx:47-62` |
| 官方截图 | `repo-assets/analytics.png`、`repo-assets/models.png` |

**我们**（`/home/jingyx/project/opencode2api-test/`）

| 主题 | 出处 |
| --- | --- |
| 现行令牌（暗/亮） | `static/admin.html:32-54` / `:55-66` |
| 现行圆角 12px | `static/admin.html:49` |
| 现行表格密度 | `static/admin.html:195-197` |
| 现行统计卡（30px 值） | `static/admin.html:181-184` |
| 现行 Token Hero（26px） | `static/admin.html:156` |
| 侧栏导航 | `static/admin.html:123, 436-442` |
| C-5 告警横幅 CSS/HTML/JS | `static/admin.html:223-230` / `:471-479` / `:1137-1159` |
| 节点表渲染 | `static/admin.html:1671-1686` |
| 直连细分口径 | `static/admin.html:2131-2159` |
| `prefers-reduced-motion` | `static/admin.html:398-401` |
| 演示数据 | `static/admin.html:2217-2224`（节点）、`:2239+`（统计） |
| C-5 真实回放 | `docs/research/C5-node-alert-replay.md` |
| 真实字段契约 | `docs/mockups/u2/direction-a-console.html:30-52` |
| U1 研究 | `docs/research/U1-freellm-ui-study.md` |

**运行 UI（只读）**

| 事实 | 命令 |
| --- | --- |
| 活实例需登录 | Playwright 打开 `http://127.0.0.1:47996/` → 文案为「登录以管理您的密钥、路由和分析。」 |
| 活体令牌 | `bodyBg = oklch(0.995 0 0)`、`font-family = "Geist Variable"…` |
| 无横向溢出 | 1440×900 与 390×844 均 `scrollWidth == clientWidth` |
