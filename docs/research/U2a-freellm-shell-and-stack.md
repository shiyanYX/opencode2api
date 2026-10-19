# U2a · freellm App Shell 解剖 + 我们是否换栈的决断

> 作者：teammate `healthcheck-probe`（task-15）
> 工作目录：`/home/jingyx/project/opencode2api-test`（分支 `test`）
> 取证时间：**2026-10-19 15:38 ~ 16:49 (Asia/Shanghai)**（含 16:40-16:49 的复核轮：行号重锚 + 路线 A 原型实测）
> 上游研究对象：FreeLLMAPI（下称 freellm），本机实例 `http://127.0.0.1:47996`（**只读，零写操作**）
> 前置：`docs/research/U1-freellm-ui-study.md`（U1 已覆盖设计令牌 / 组件清单 / 实现代价）。
> **本文不重复 U1**，只做两件 U1 没做的事：**① 一页一页的 shell 骨架（含路由表、区域划分、三态挂载点）；② 「要不要跟着换栈」的决断。**
> ⚠️ 所有引用 `static/admin.html` 的行号已**重锚到 2026-10-19 16:42 的 2,327 行 / 192,254 B / gzip 61,037 B 版本**（sha256 `2304d22c…`）。
> **这个文件在本轮期间被另一名队友改了两次**（16:00 的 2,307 行 → 16:09 的 2,321 行 → 16:42 的 2,327 行），我每次都逐条重锚并复核。行号还会继续漂移 —— **引用时请把行号当线索、以内容串为准**。
> 同理，§3.1 的原型是基于 16:42 快照重算的（diff 结构 6 个 hunk / 净 +20 行完全不变，只是字节数随基线更新）。

---

## 0. 结论速览（大白话）

**freellm 的大框架是什么**
> 一个**顶栏**（sticky、半透明毛玻璃、底部一条细线），里面放品牌 + 6 个导航项；下面是一个**固定最大宽 1152px 居中**的内容列；每个页面顶部有一块统一的「大标题 + 一句说明 + 右侧动作按钮」的头（`PageHeader`），下面就是一张张卡片的竖向堆叠。**没有侧栏。**

**我们的现状**
> 一块 **204px 的左侧栏**（`static/admin.html:123` / :451），右边是内容列（`.main`，最大宽 1280px，`admin.html:140`）。
> 有意思的是：**我们内容列顶部其实已经有一块「标题 + 副标题 + 右侧按钮 + 底部细线」的头**（`.topbar`，`admin.html:141-143` / :472-482）—— 它和 freellm 的 `PageHeader` 是同一个东西，形态已经接近。

**所以「照 freellm 大改框架」这件事的真实工作量**
> **实测：+20 行（+0.86%）、+2,753 字节（+1.43%）、gzip +285 字节（+0.47%）**，零新增依赖、零构建、零 Docker 改动、零 Go 改动、零测试改动。
> 这不是估的 —— 我把这个改动**真做出来了**：`docs/mockups/u2a/routeA-proto-admin.html`（可直接浏览器打开）+ 4 张 Playwright 截图，桌面/移动都验过能跑。
> 主要动作只有一个：**把左侧栏换成顶栏**（其余是配色间距对齐）。这个数字是本文最重要的一个。
> **注意口径**：这是「把大框架做出来」的价格，**不是**「把整个面板重画成 freellm 的观感」的价格 —— 后者是 §4.3 的 `C1-C6`，约 +340 行（那部分是**估算**）。别把两个数字混着引用。

**我们该不该跟着换栈（引入 Vite + React + Tailwind）**
> **不换。走路线 A。** 理由不是保守，是三条硬账：
> 1. 用户要的「大框架」在无构建单文件里只要 **+20 行（+0.86%）** 就能做到 —— 已做出原型验证，换栈买不到这个框架。
> 2. 换栈把产物体积从 **61,037 字节（gzip）涨到 512,555 字节（gzip）＝ 8.4 倍**（同口径实测）。**但更贴近现实的是第三组数**：我们的 Go 服务端**根本不做 HTTP 压缩** —— 全仓库非 vendor 代码里 `gzip/brotli/flate/Content-Encoding/Accept-Encoding` **零命中**，`adminPageHandler` 只有 `w.Write(adminPageHTML)`；所以今天浏览器实际拉的是 **192,254 B 明文**，换栈后同一个服务端要拉 **≈1,786,089 B 明文 ＝ 9.29 倍**。freellm 那边看着小（506,574 B）是因为**它自己的服务器开了 brotli**，不是因为它的代码小。并且面板从「HTML 里有内容」变成「必须等 JS 跑完才有画面」。
> 3. 换栈会**打断 3 个既有测试**（不是一个）：`discev_test.go:24/29` 用正则从 `static/admin.html` **抽 `CL_ISSUE_TYPES` 常量**；另外 `zz_c5_verify_wiring_test.go:209` 和 `zz_c5v2_verify_wiring_test.go:166` 也 `os.ReadFile("static/admin.html")` 直接查字符串（`renderNodeAlert`、`.node-alert.on`、`run_failures`…）。引入构建后这个文件变成产物，这些测试要么改（动 Go 代码）要么造一份生成物 —— 这是**流程风险**，不是 UI 风险。（本轮补查：`grep -rln 'static/admin' --include=*.go .`）
>
> **翻转条件写在 §5.3**：不是「用户想要 freellm 的样子」（路线 A 已经满足），而是前端规模/状态复杂度/是否需要与别的 JS 工程共享代码这三类信号。

---

## 1. App Shell 解剖（全部来自源码，给 `文件:行号`）

> 权威来源是**源码**而不是截图：`/home/jingyx/freellmapi/client/src`（本机完整仓库，非压缩产物）。
> 压缩产物仅用于交叉验证（`/tmp/fl_index.js`，1,682,736 字节）。

### 1.1 导航骨架：**顶栏，无侧栏**

```
App                                         client/src/App.tsx:437-490
└ QueryClientProvider  (全局 staleTime 30s + 失败必弹 toast)
  └ ThemeProvider
    └ I18nProvider
      └ BrowserRouter  basename=import.meta.env.BASE_URL
        └ AuthGate                              ← 登录/初始化闸门（全局唯一的"启动中"态）
          └ AppShell                            ← ① 外壳列
            ├ Navbar                            ← ② 顶栏
            ├ PageContainer                     ← ③ 内容容器
            │  └ PageBoundary (ErrorBoundary key=pathname)
            │     └ Routes                      ← 24 条路由
            ├ Toaster       （fixed，不进流）
            ├ CommandPalette（fixed，不进流）
            └ UpdateReminder（fixed，不进流）
```

**① 外壳列 `AppShell`**（`App.tsx:427-435`）

```jsx
<div className={`flex flex-col ${fullBleed ? 'h-dvh overflow-hidden' : 'min-h-screen'} ${isMacDesktop ? 'desktop-backdrop' : 'bg-background'}`}>
```

- 普通页面：`min-h-screen`，整页文档滚动。
- **全出血页面**：只有 `/playground`（`FULL_BLEED_ROUTES`，`App.tsx:395`）→ `h-dvh overflow-hidden`，把自己钉死在可视高度，让内部某一栏自己滚。
- 注释（`App.tsx:422-426`）说明了为什么必须两套：`min-height` 只是下限不是上限。

**② 顶栏 `Navbar`**（`App.tsx:226-383`）—— 结构逐行：

| 层 | 源码 | 说明 |
|---|---|---|
| `<header>` | `App.tsx:237-243` | `sticky top-0 z-40 border-b backdrop-blur`；macOS 桌面壳用 `bg-background/45`（要透出窗口的 vibrancy），其它一律 `bg-background/80`（保证文字对比度） |
| 内层容器 | `App.tsx:244-249` | `mx-auto flex max-w-6xl items-center px-4 sm:px-6`；macOS 桌面额外 `pl-20`（给红绿灯按钮让位）+ `minHeight: 52` |
| 品牌 | `App.tsx:134-141`（`Brand`） | `Link to="/"`：一个小圆点 + 文字 "FreeLLMAPI"，`gap-2`、`font-semibold text-sm` |
| 主导航 `<nav>` | `App.tsx:251-284` | **`ms-10 hidden items-center gap-6 md:flex`** ← `md`（768px）以下整条隐藏 |
| 右侧动作区 | `App.tsx:285-320` | **`ms-auto hidden items-center gap-1 md:flex`** ← 同样 `md` 以下隐藏 |
| 移动端汉堡 | `App.tsx:321-374` | `ms-auto md:hidden`，一个图标按钮 + `w-52` 下拉，内含全部导航 + 分隔线 + 账号组 |

- **主导航 6 项**（`navItems`，`App.tsx:67-74`）：`/models` 模型、`/playground` 试验场、`/keys` 密钥、`/agents` 智能体、`/analytics` 分析、`/premium` 高级版。
- **两项是"分裂控件"**（标签本身可点着跳转，右边小箭头展开子页，`App.tsx:255-283`）：
  - `/models` → `/models/chat`、`/models/embeddings`、`/models/image`、`/models/video`、`/models/audio`、`/models/fusion`（`modelItems`，`App.tsx:79-86`）
  - `/analytics` → `/analytics`、`/logs`（`analyticsItems`，`App.tsx:90-93`）
  - ★ **`/logs` 故意不放进主导航**：注释写明"so the top bar does not grow a seventh entry"（`App.tsx:88-89`）—— **这就是"顶栏最多放 6 个"的显式设计规则**，值得我们照抄（我们现在是 5 个）。
  - 子菜单靠 `navMenus` 一张表驱动（`App.tsx:99-113`），`aria-label` 分别 `nav.modelsMenu` / `nav.analyticsMenu`。
- **导航项激活态**（`NavItem`，`App.tsx:117-132`）：文字色 + **一条 1px 下划线贴在导航项底部**（`after:-bottom-px after:h-px after:bg-foreground`）。
- **右侧动作区 2 个控件**：① 命令面板按钮（放大镜 + `<kbd>` 显示 `⌘K` 或 `Ctrl K`，`App.tsx:289-297`）；② "更多"下拉（`MoreHorizontal`，`w-48`，内含 升级/设置/改邮箱/改密码/退出，`App.tsx:298-319`）。

**实测几何**（Playwright，1440×900，走 §6 的"仅鉴权放行"口径）：

| 量 | 实测值 |
|---|---|
| `<header>` 尺寸 | `x=0 y=0 w=1440 **h=53px**`，`z-index=40`，`border-bottom-width=1px` |
| `<header>` 背景 | `oklab(0.995 0 0 / 0.8)` = `bg-background/80` |
| `<main>` 盒 | `x=**144** w=**1152**`，`max-width=1152px`，`padding-left=24px` |
| 导航项横向位置 | 品牌 x=168；第一个导航项 x=301；命令面板按钮 x=1160（宽 76）；"更多"按钮 x=1240（宽 32） |
| 1440 下可见导航项数 | 6（+2 个下拉箭头按钮，`aria-label`＝"浏览模型类型"/"分析页面"） |
| 390 下可见导航项数 | **0**（只剩品牌 x=16 和汉堡 x=342）；菜单展开后文案＝`模型 \| 试验场 \| 密钥 \| 智能体 \| 分析 \| 高级版 \| 设置 \| @ \| 修改邮箱 \| 修改密码 \| 退出登录` |

**③ 内容容器 `PageContainer`**（`App.tsx:412-420`）

```jsx
<main className={fullBleed ? 'flex min-h-0 flex-1 flex-col' : 'mx-auto w-full max-w-6xl px-6 py-8'}>
```

- `max-w-6xl` = 72rem = **1152px**；`px-6` = 24px；`py-8` = 32px。
- `App.tsx:400-411` 有一整段注释解释 **`w-full` 是承重的**：`<main>` 是列向 flex 的子项，交叉轴是横向；`mx-auto` 会把两个横向 margin 设成 auto，flex 就不再拉伸它 ⇒ 没有显式宽度时列会"按内容收缩"，Analytics 这种十个查询陆续返回的页面就会**每返回一批就横向跳一下**。**这条坑我们要照着抄注释。**

### 1.2 页面清单与路由（24 条）

`App.tsx:452-477`，逐条：

| # | path | 组件 | 类型 |
|---|---|---|---|
| 1 | `/` | `Navigate → /models/chat` | 重定向 |
| 2 | `/models` | `Navigate → /models/chat` | 重定向 |
| 3 | `/models/chat` | `FallbackPage` | **主表页**（650 行） |
| 4 | `/models/chat/:id` | `ModelDetailPage` | 详情（644 行） |
| 5 | `/models/fusion` | `FusionPage` | 模态页（333 行） |
| 6 | `/models/embeddings` | `EmbeddingsPage` | 模态页（363 行） |
| 7 | `/models/embeddings/:id` | `EmbeddingDetailPage` | 详情（103 行） |
| 8 | `/models/image` | `ImagePage` | 模态页（5 行，壳） |
| 9 | `/models/image/:id` | `MediaDetailPage modality="image"` | 详情 |
| 10 | `/models/video` | `VideoPage` | 模态页（5 行，壳） |
| 11 | `/models/video/:id` | `MediaDetailPage modality="video"` | 详情 |
| 12 | `/models/audio` | `AudioPage` | 模态页（5 行，壳） |
| 13 | `/models/audio/:id` | `MediaDetailPage modality="audio"` | 详情 |
| 14 | `/models/transcription/:id` | `MediaDetailPage modality="transcription"` | 详情 |
| 15 | `/playground` | `PlaygroundPage` | **全出血**（1,235 行） |
| 16 | `/keys` | `KeysPage` | 配置页（133 行壳 + 多个 Section 组件） |
| 17 | `/agents` | `AgentsPage` | 卡片网格（185 行） |
| 18 | `/fallback` | `Navigate → /models/chat` | 遗留别名 |
| 19 | `/analytics` | `AnalyticsPage` | **概览/观测页**（1,188 行） |
| 20 | `/logs` | `LogsPage` | 日志页（432 行） |
| 21 | `/premium` | `PremiumPage` | 升级页（248 行） |
| 22 | `/test` | `Navigate → /playground` | 遗留别名 |
| 23 | `/health` | `Navigate → /keys` | 遗留别名 |
| 24 | `*` | `NotFoundPage` | 404 |

**三条可从这张表直接抄的经验**：
1. **"模态页共用同一个详情组件"**：`/models/{image,video,audio,transcription}/:id` 全指向 `MediaDetailPage`，只换一个 `modality` prop（`App.tsx:461-466`）。**我们如果要做节点详情，应该同样用"一个详情组件 + 一个 mode 参数"，而不是 5 个页面。**
2. **遗留路径用 `<Navigate replace>` 兜住**（`/fallback`、`/test`、`/health`，`App.tsx:470/474/475`）—— 零成本保住旧书签。
3. **404 是路由表里的最后一条 `*`**，不是服务器行为（见 §1.5）。

### 1.3 页面区域划分（逐页 wireframe，源码级）

**★ 先记住一个通用模板** —— freellm 的**所有**普通页面都长这样：

```
<div>                                          ← 页面根（无 class）
  <PageHeader title description actions />      ← 统一页头（见下）
  <div className="space-y-6 | space-y-8">       ← 竖向卡片堆叠，间距 24 / 32px
    <section className="rounded-3xl border bg-card p-5"> … </section>
    <section …> … </section>
  </div>
</div>
```

**统一页头 `PageHeader`**（`components/page-header.tsx:8-25`）：

```jsx
<div className="flex flex-wrap md:flex-nowrap items-end justify-between gap-6 mb-6 pb-6 border-b">
  <div className="min-w-0">
    <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
    <p className="text-sm text-muted-foreground mt-1">{description}</p>   // 可选
  </div>
  {actions && <div className="flex items-center gap-2 shrink-0">{actions}</div>}
</div>
```

- `divider` 默认 `true`（画底部分隔线）；`FallbackPage` 传 `divider={false}`，因为它下面紧跟一排 tab。
- 关键点：**页头的右侧是"动作槽"**，页面级操作（时间窗切换、清空、新建）全挂在这里，而不是散在卡片里。

**逐页区域**（`<div>` 下的一级区块；括号里是源码行）：

| 页面 | 页头右侧动作 | 内容区一级区块（自上而下） |
|---|---|---|
| **`/models/chat`**（`FallbackPage.tsx:268-`） | `<ModelsTabs/>`（模态 tab 条） | ① `space-y-6`：`GettingStarted` 首跑清单 → ② `TokenUsageBar`（月预算条，有预算才渲染）→ ③ 策略卡片（`rounded-3xl border bg-card p-5`：标题行 + pill 组 + "更多选项"折叠）→ ④ `ChainManager` 命名回退链 → ⑤ `PenaltyInspector` 惩罚/冷却 → ⑥ 主表：**加载中 `TableSkeleton` / 空 `EmptyState`（带动作）/ 表 + 目录工具条（搜索 + 能力/上下文筛选）** |
| **`/logs`**（`LogsPage.tsx:175-`） | 清空按钮（`ConfirmButton`） | ① 工具条 `mb-4 flex flex-wrap items-center gap-2`：级别 chips（带计数）+ provider `Select` + 搜索 `Input` → ② 一行口径提示 `text-xs muted` → ③ **条件错误条**（`query.isError` 时）→ ④ 日志卡 `relative rounded-3xl border bg-card`：内部 `max-h-[65vh] min-h-[240px] overflow-y-auto p-3`，空时 `EmptyState`，底部"加载更多" + 口径提示 |
| **`/analytics`**（`AnalyticsPage.tsx:710-`） | `SegmentedControl` 时间窗（24h/7d/30d/90d） | ① `space-y-6`：KPI 网格 `grid grid-cols-2 md:grid-cols-4 gap-3` → ② 面板网格 `grid grid-cols-1 lg:grid-cols-2 gap-6`，重点面板 `lg:col-span-2`（Token 趋势、Recent calls） |
| **`/keys`**（`KeysPage.tsx:68-`） | `SegmentedControl`（分区切换） | `space-y-8`：`UnifiedKeySection` → `ClientProfilesSection` → `ProxySettingsSection` → `BackupsSection` → `QuotaSignalsSection` → `ProviderChecklistSection` → `ProviderList`；末尾挂 `AddKeyDialog` |
| **`/agents`**（`AgentsPage.tsx:80-`） | 一个按钮 | ① `section` 卡片：标题 + 说明 + 一行可复制的 API base（`code` + `CopyButton`）→ ② `grid gap-4 sm:grid-cols-2 lg:grid-cols-3` 的 agent 卡片网格 |
| **`/premium`**（`PremiumPage.tsx:68-`） | 刷新按钮 | **两种形态**：加载中＝两个 `CardSkeleton`；加载完＝`space-y-8`：目录 feed section → License section（含表单）→ 未授权时的 upsell section |
| **`/playground`** | —— | **全出血**：三栏齐边，transcript 在自成一栏里滚动（`PlaygroundPage.tsx`，1,235 行）；输入区 `max-w-3xl` 居中（:972 / :1122） |
| **`*`（404）** | —— | `NotFoundPage.tsx:12`：`flex justify-center py-16` + `w-full max-w-md rounded-3xl border bg-card p-8 text-center` 卡片（图标 + 标题 + 说明 + "回首页"按钮） |

### 1.4 全局三态（加载 / 空 / 错误）挂在哪

**★ 一句话结论：freellm 没有"全局三态"，它只有一个"全局启动态"，其余三态是**每页自己**声明的。**

| 态 | 挂载点 | 实现 | 证据 |
|---|---|---|---|
| **启动中**（唯一真正的全局态） | `AuthGate` | `/api/auth/status` 的 `isLoading` → 居中的 `Loader2` 转圈 + `auth.loading` 文案 | `components/auth-gate.tsx:443, 488-518` |
| **加载中**（每页） | 各页自己 | **骨架屏**，不是"Loading…"一行：`Skeleton`（`animate-pulse rounded-lg bg-muted`）、`TableSkeleton`（表头条 + N 行）、`CardSkeleton`（卡形） | `components/ui/skeleton.tsx:5-45` |
| **空**（每页） | 各页自己 | 统一空态组件 `EmptyState`：**虚线圆角卡** + 图标 + 标题 + 描述 + **action 槽**；注释明写"an empty state without a next step is a dead end" | `components/empty-state.tsx:8-31` |
| **错误（数据）** | 两层 | ① **页内联**：`query.isError` 时渲染一条 `border-destructive/40 bg-destructive/10 text-destructive` 的横幅；② **全局兜底**：`MutationCache.onError` ⇒ 任何写操作失败都弹 error toast（可用 `meta:{silenceToast:true}` opt-out） | `LogsPage.tsx:359-365`；`App.tsx:49-65` |
| **错误（渲染崩溃）** | 路由级 | `ErrorBoundary` **以 `pathname` 为 key**，页面抛错显示"崩溃卡"（图标 + 标题 + 说明 + 截断的 error.message + 重试/回首页），**导航离开即自动复位** | `App.tsx:385-389`；`components/error-boundary.tsx:11-57` |
| **404** | 路由 | 路由表 `*` | `App.tsx:476` |
| **Toast** | 固定层 | `fixed bottom-6 right-4 z-[60] max-w-sm flex flex-col gap-2 sm:right-6`；错误 toast 常驻不自动消失 | `App.tsx:480`；`components/toaster.tsx` |
| **字段缺失** | 约定 | **`null` 渲染 `—` 而不是 `0`**（"不许把 null 画成误导性的 0 ms"） | `AnalyticsPage.tsx:695-706` |

**这套东西对我们的直接启示**：三态不要做成一个中央状态机，而是做成**三个可复用小组件**（骨架 / 空态 / 内联错误条）+ 一个**全局 toast**。我们现在的 `showToast`（`admin.html:883`）只有单条、2.6 秒自动消失 —— 错误消息会消失，这是要改的。

### 1.5 深链与刷新策略

**deep link：能用。** freellm 是 `BrowserRouter`（`App.tsx:442`）的 SPA，服务端对**任何**路径都回同一份 1572 字节的 `index.html`（SPA 兜底）：

```bash
curl -s -o /dev/null -w "%{http_code} %{size_download}\n" http://127.0.0.1:47996/logs
curl -s -o /dev/null -w "%{http_code} %{size_download}\n" http://127.0.0.1:47996/models/chat
curl -s -o /dev/null -w "%{http_code} %{size_download}\n" http://127.0.0.1:47996/analytics/nonexistent-xyz
curl -s -o /dev/null -w "%{http_code} %{size_download}\n" http://127.0.0.1:47996/health
# 四条全部 → 200 1572   （即 index.html 本身）
```

⇒ **深链、刷新、浏览器前进后退全部可用**；未知路径由客户端 `path:*` 兜成 404 页。**代价**：查看源码时看到的是空 `<div id="root">`，内容全靠 JS。

**我们的现状（差距）**：`showPage(id)`（`admin.html:868-876`）只做 `classList` 切换 + 改标题，**完全不碰 URL**：

```js
function showPage(id){
  if(id!==currentPage){currentPage=id;applyAutoRefresh()}
  qa('.page').forEach(p=>p.classList.remove('active'));
  q('#page-'+id).classList.add('active');
  qa('.nav button').forEach(b=>b.classList.toggle('active',b.dataset.page===id));
  q('#pageTitle').firstChild.textContent=pageTitles[id][0]+' ';
  q('#pageCrumb').textContent=pageTitles[id][1];
  if(id==='logs'&&!DEMO_MODE)initLogs();
}
```

⇒ **我们没有任何深链**：刷新永远回到概览、浏览器后退不切分区、没法把"节点池"发给别人。全仓库也**没有** `location.hash` / `hashchange`（`grep -n "location.hash\|hashchange" static/admin.html` → 0 命中）。这是一个**独立于换栈的、约 8 行就能修好的缺陷**，建议随本次 shell 改造一起做（见 §4 的 `H1`）。

### 1.6 响应式断点

**freellm**：Tailwind 默认断点，**编译产物里的 media query 就是证据**：

```bash
grep -o '@media[^{]*{' /tmp/fl_index.css | sort | uniq -c | sort -rn
#  6 @media (hover:hover){          2 @media (width>=80rem){    ← xl 1280
#  2 @media (width>=64rem){         2 @media (width>=48rem){    ← lg 1024 / md 768
#  2 @media (width>=40rem){         1 @media (width>=96rem){    ← sm 640 / 2xl 1536
#  1 @media (prefers-reduced-motion:reduce)  1 @media (pointer:coarse)  1 @media (forced-colors:active)
```

源码里实际用到的响应式前缀计数（`client/src` 全量）：

| 前缀 | 断点 | 出现次数 |
|---|---|---|
| `sm:` | 640px | **49** |
| `md:` | 768px | **11** |
| `lg:` | 1024px | **13** |
| `xl:` | 1280px | 1 |
| `2xl:` | 1536px | 0 |

**两条最关键的行为**：
1. **`md`（768px）是 shell 的断点**：导航条与右侧动作区在这一档整体隐藏，换成汉堡下拉（`App.tsx:252` vs `:321`）。实测 390px 下导航项可见数 = 0。
2. **表格不卡片化**：宽表靠外层 `overflow-x-auto` 横滚（U1 §4.6）；`Tooltip` 用 portal 正是为了不被表格 overflow 裁掉。

**我们**：4 个断点、互不成体系 —— `1000`（min-width）、`960`（max）、`760`（max）、`560`（max），见 `admin.html:162/171/179/296/297/388/410`。

⇒ **建议**：把断点收敛成 freellm 的三档（`640 / 768 / 1024`），其中 **768 是 shell 折叠点**。这条即使不换栈也照做。

### 1.7 键盘与命令面板

| 项 | freellm | 证据 |
|---|---|---|
| 快捷键 | **`⌘K` / `Ctrl K`** 切换打开；`Esc` 关闭 | `components/command-palette.tsx:76-89` |
| 打开方式 ① | 顶栏按钮（**只在 `md:flex` 块里**） | `App.tsx:289-297` |
| 打开方式 ② | **全局自定义事件** `freellmapi:command-palette` | `components/command-palette-state.ts:1-5` |
| 面板内键盘 | `ArrowDown` / `ArrowUp` 移动、`Enter` 执行 | `command-palette.tsx:196-207` |
| 语义 | `role="dialog" aria-modal` + 输入框 `role="combobox" aria-expanded aria-controls="command-palette-list" aria-activedescendant="cmd-<id>"` + 列表 `role="listbox"` | `command-palette.tsx:224-249` |
| 分组 | 3 组：`pages`(11 条) / `actions` / `models`（模糊搜索按 `keywords` 命中，`models` 组空查询只取前 5、有查询取前 8） | `command-palette.tsx:109-180` |
| 空结果 | 居中一句 `palette.empty` | `command-palette.tsx`（`y.length===0` 分支） |
| 全应用其它键盘处理 | **只有 2 处**：命令面板 + tooltip 的 Esc | `grep -rn "addEventListener('keydown'" client/src` → `tooltip.tsx:55`、`command-palette.tsx:86` |

**★ 一个可复用的设计点**：命令面板不靠 React context 传递"打开"意图，而是 **`window.dispatchEvent(new CustomEvent(...))`**（`command-palette-state.ts:3-5`）。谁都能触发（顶栏按钮、桌面壳菜单、将来的快捷键），面板只需一处监听。**我们在单文件里可以用完全相同的模式**（`window.dispatchEvent` 是原生 API）。

**★ 一个缺陷**：面板的**唯一 UI 触发按钮在 `md:flex` 块里**（`App.tsx:285-320`），而 `Q_`（`openCommandPalette`）全仓库**只有一个调用点**（顶栏那个按钮）。
⇒ **< 768px 的触屏设备上，命令面板没有任何入口**（只剩硬件键盘的 ⌘K，而手机没有）。这印证 U1 §7.10 的判断：**命令面板对我们是净负担，建议放弃**。

---

## 2. 技术栈实测：freellm vs 我们

| 维度 | freellm（实测） | 我们（实测） |
|---|---|---|
| 框架 | React 19.2.4 + TypeScript ~6.0.2（U1 §5.1，`client/package.json`） | **无框架**，原生 DOM |
| 构建 | Vite ^8.0.4，`build: tsc -b && vite build` | **无构建** |
| CSS | Tailwind ^4.2.2 + `@tailwindcss/vite` + shadcn 配置 | 手写 CSS，1 个 `<style>` 块（`admin.html:16-445`，约 430 行） |
| 路由 | react-router-dom ^7.14.0（24 条路由） | **无路由**；`showPage()` 切 class（`admin.html:868`） |
| 数据层 | @tanstack/react-query ^5.97.0（全局 `staleTime 30s` + `MutationCache` 统一 toast） | 手写 `fetch` + 局部 `setInterval` |
| 图表 | recharts ^3.8.1 | **ECharts 5.5.1，从 CDN 加载**（`admin.html:15`，jsdelivr） |
| 图标 | lucide-react（按名 import，打包） | 行内内联 SVG |
| 产物 | HTML 1,572 B + 1 个 JS `1,682,736 B` + 1 个 CSS `101,781 B` | **1 个 HTML `192,254 B`**（自包含，内含 CSS+JS） |
| **gzip 后** | **512,555 B**（HTML 789 + JS 494,922 + CSS 16,844） | **61,037 B** |
| **服务端实际发送**（实测响应头） | **brotli `Content-Encoding: br`：506,574 B**（HTML 714 + JS 489,128 + CSS 16,732） | **明文 192,254 B —— 没有任何压缩中间件** |
| 首屏是否需要 JS | **是**（`<div id="root">` 空壳，全靠 module script） | **否**（HTML 里就有内容；仅数据区依赖 JS） |
| 外部依赖 | 无 CDN（自托管字体） | **有 CDN**（ECharts jsdelivr） |
| 内联事件属性 | **0**（CSP `script-src-attr 'none'`） | **75 处 `onclick=`** |
| `addEventListener` | 2 处（全局） | 4 处 |
| 页面/分区 | 24 条路由 | 5 个 `<section id="page-*">` |
| 前端源码量 | 非 locale **26,452 行**（U1 §5.1） | **2,327 行**（`<style>` `:16-445`，`<script>` `:794`–`:2325` 约 1,532 行） |
| 可复用渲染函数 | 组件 | **162 个 `function`**，其中 **21 个 `render*`**，**50 处 `innerHTML`** |
| 打包方式 | 静态资源目录 | **`go:embed static/admin-login.html static/admin.html`**（`embed.go:7`） |
| 镜像构建 | Node（前端）+ 后端 | **纯 Go 单阶段**：`golang:1.26-alpine` → `alpine:3.20`，`Dockerfile` 里**没有任何 Node/npm** |
| 测试钩子 | —— | **`discev_test.go:24` 正则抽 `CL_ISSUE_TYPES`，:29 读 `static/admin.html`** |
| CSP 响应头 | 有（`default-src 'self'` + `script-src-attr 'none'` + `style-src 'self' 'unsafe-inline'`） | **无**（全仓库 `Content-Security-Policy` 0 命中） |

> **CSP 这一行要特别说明**：我们**现在没有 CSP**，所以"freellm 的 CSP 我们学不了"这个说法要精确化 —— 不是*学不了*，而是**要采纳它就必须先把 75 个 `onclick` 全部重写**（U1 §7.1 的结论对，理由要改）。**本次 shell 改造不需要碰 CSP。**

---

## 3. 三条路线的量化代价

> 三条路线的定义（按 Lead 的任务书）：**A** 保持无构建单文件、只抄它的 shell；**B** 引入构建（Vite+React+Tailwind 之类）整体重写；**C** 混合：无构建外壳 + 局部组件化。
> 所有"行数"以 2026-10-19 16:42 的 `static/admin.html`＝**2,327 行 / 192,254 B（gzip 61,037 B）** 为基数。

### 3.1 路线 A · 保持无构建单文件 + 抄它的 shell

**要改的东西是有限且可枚举的**（这是本条路线最强的论据）：

> **这一节初稿给的是估算（+43 行）。复核时我把它真的做出来了**，所以下面全是实测值（已按 16:42 基线重算）：
> 产物 `docs/mockups/u2a/routeA-proto-admin.html`（可直接用浏览器打开对比），截图 `routeA-proto-*.png` 5 张（overview / nodes / logs 各 1440×900，mobile-closed / mobile-menu 各 390×844）。
> 做法：**6 个 hunk** —— ① `.app` 改纵向 + 侧栏 CSS 换顶栏 CSS；② `.main` 加居中；③ 960 断点里 7 行折叠规则换 4 行；④ `<aside>` 换 `<header>`；⑤ `showPage` 高亮选择器；⑥ 加 `toggleNavSheet/closeNavSheet`。**其余一行未动。**

> **逐 hunk 的实测值**（`diff -u static/admin.html routeA-proto-admin.html`，共 **6 个 hunk、+55 / −35 = 净 +20 行**）：

| # | hunk（原文件行区间） | 做了什么 | 实测 |
|---|---|---|---|
| 1 | `@@ -119,15 +119,23 @@` | `.app` 加 `flex-direction:column`；`.sidebar` 那 8 行 CSS 换成 16 行 `.appbar/.topnav/.navsheet/.burger` | **+17 / −9** |
| 2 | `@@ -137,7 +145,7 @@` | `.main` 加 `width:100%;margin:0 auto`（侧栏没了要自己居中） | **+1 / −1** |
| 3 | `@@ -386,13 +394,10 @@` | `@media(max-width:960px)` 里 7 行侧栏折叠 → 4 行（藏 `.topnav`、藏次要动作、显示汉堡） | **+4 / −7** |
| 4 | `@@ -448,25 +453,37 @@` | `<aside class="sidebar">` 19 行 → `<header class="appbar">` 顶栏（品牌 + 5 项导航 + 右侧动作 + 汉堡 + 移动抽屉） | **+29 / −17** |
| 5 | `@@ -869,7 +886,8 @@` | `showPage()` 的高亮选择器 `.nav button` → `.topnav button,.navsheet button`，并 `closeNavSheet()` | **+2 / −1** |
| 6 | `@@ -879,6 +897,8 @@` | 新增 `toggleNavSheet()` / `closeNavSheet()` 两个函数 | **+2 / −0** |
| — | **合计** | | **+55 / −35 ＝ 净 +20 行** |

**没做进原型、但仍建议后续做的两项**（都不影响"框架已可交付"这个结论）：

| 动作 | 现状 | 目标 | 估计 |
|---|---|---|---|
| 深链 | `showPage()` `:868-876` 不碰 URL | `location.hash` + `hashchange` + 首屏还原 | +8 |
| 断点收敛 | 760/960/1000/560 四档（`:162/171/179/296/297/388/410`） | 640/768/1024 三档 | ~0（改数值） |

**完全不动**：75 处 `onclick`（新顶栏沿用 `onclick`，风格一致）、Dockerfile、`go:embed`（`embed.go:7`）、`discev_test.go`（`CL_ISSUE_TYPES` 在 `:2036`，未被触碰）、依赖清单。

**实测净变化：+20 行（2,327 → 2,347，`wc -l` 口径 ＝ +0.86%）、+2,753 B（192,254 → 195,007 ＝ +1.43%）、gzip +285 B（61,037 → 61,322 ＝ +0.47%）。**
（初稿估的 +43 行偏高 —— 真实值更小，因为原侧栏的 markup 有 19 行被整块复用进顶栏。**估算方向没错，但估算值不准，这里以实测为准。**）

| 代价项 | 量化（实测） |
|---|---|
| 行数 | **+20 行（+0.86%）** |
| 字节 | **+2,753 B（+1.43%）；gzip +285 B（+0.47%）** |
| 事件绑定改写量 | **0**（新顶栏沿用 `onclick`） |
| CSP | **不变（我们无 CSP；也不需要 adopt）** |
| Docker 构建 | **不变**（无 Node 阶段） |
| 测试钩子 | **不变**（`discev_test.go` 照常抽常量；`CL_ISSUE_TYPES` 在 `:2036`，未被触碰） |
| 首屏体积 | **基本不变**（+1.43% 明文）。注：服务端本就发明文，故这条路线**不改变压缩现状** |
| 离线可用性 | **改善**（若顺手把 `admin.html:15` 的 ECharts CDN 换成本地/内联，则完全离线；**本次不做**，但记一笔） |
| 维护面 | 1 个文件、1 种语言、1 条构建（Go build） |
| 风险 | **低**：纯 markup/CSS 重排，回滚 = 还原一个文件；与 `discev_test.go` 无耦合 |
| **已验证能跑** | Playwright 实测（1440×900 / 390×844）：顶栏高 **54px**、5 个导航项可见、无侧栏、`scrollWidth == clientWidth`（**无横向溢出**）；点「运行日志」标题与高亮同步切换；390px 下 `.topnav` 隐藏、汉堡出现、抽屉展开 5 项、点选后自动收起；**零 pageerror**（仅 404，因为静态服务器没有后端 API） |

### 3.2 路线 B · 引入构建（Vite + React + Tailwind / shadcn）整体重写

| 代价项 | 量化 | 依据 |
|---|---|---|
| **重写量** | 2,327 行 → 估计 **2,500~4,000 行 TSX**（我们把 162 个函数、50 处 `innerHTML` 模板、21 个 `render*` 全部换成组件）；另外 React 运行时的"隐形成本"要按组件数算，不能按行数算 | 自有源码计数；freellm 的 24 路由/更大功能面是 26,452 行（U1 §5.1），我们功能面小但**功能类型完全不同**（节点池/订阅/日志在 freellm 里没有对应件，不能照抄） |
| **事件绑定改写量** | **75 处 `onclick` 全部重写**（React 里写不了内联字符串 handler）；50 处 `innerHTML` 全部改成 JSX | `grep -o "onclick=" static/admin.html \| wc -l` → 75 |
| **Docker 构建** | 现在是**单阶段纯 Go**（`golang:1.26-alpine` → `alpine:3.20`，`Dockerfile` 无 Node）。必须新增 Node 阶段或提交产物；`npm ci` 需要网络/锁文件，构建时间与可复现性都要重新论证 | `Dockerfile` 实测 |
| **测试钩子（硬冲突）** | **3 个测试**会失败：`discev_test.go:24` 的 `regexp.MustCompile(\`CL_ISSUE_TYPES=\[([^\]]*)\]\`)` + `:29` 的 `os.ReadFile("static/admin.html")`；以及 `zz_c5_verify_wiring_test.go:209`、`zz_c5v2_verify_wiring_test.go:166` 的字符串包含断言。修它们 = 改 Go 测试 = 超出"只改前端"的范围，需要与后端负责人协调 | `discev_test.go:24-42`、`zz_c5_verify_wiring_test.go:209`、`zz_c5v2_verify_wiring_test.go:166` |
| **首屏体积** | 同口径 gzip：**512,555 B vs 我们 61,037 B ＝ 8.4 倍**；两侧都不压缩：**1,786,089 B vs 192,254 B ＝ 9.29 倍**。**决定性的那笔账**：我们服务端没有压缩中间件，今天发 **192,254 B 明文**；换栈后同一个服务端要发 **≈1,786,089 B 明文 ＝ 9.29 倍**（freellm 实测只发 506,574 B，是因为它自己的服务器开了 brotli） | `gzip -c /tmp/fl_index.{html,js,css} \| wc -c` = 789 / 494,922 / 16,844（合计 512,555）；明文 1,572+1,682,736+1,781 = 1,786,089。我们：`stat -c%s static/admin.html` = 192,254、`gzip -c static/admin.html \| wc -c` = 61,037 |
| **首屏语义** | 从"HTML 里就有内容"变成"**必须等 1.68 MB JS 解析执行完才有画面**"（freellm 的 `#root` 是空的）。本地局域网面板也躲不过这段白屏 | `/tmp/fl_index.html`（1572 B，`<div id="root"></div>`） |
| **离线可用性** | 面板本身离线，但**构建**不再离线自洽（新增 npm 依赖树 + 锁文件） | —— |
| **CSP** | 这一条是**唯一的好处**：换栈后才有资格 adopt freellm 的 `script-src-attr 'none'`。但注意 freellm 的 CSP 里 `style-src` 仍要 `'unsafe-inline'`，且它的 bootstrap 内联脚本要用 sha256 白名单 | `curl -s -D- http://127.0.0.1:47996/` |
| **维护面** | 2 套工具链（Go + Node）、锁文件、两处版本升级、CI 时间 | —— |
| **风险** | **高且不可逆**：一次性重写 2,327 行 + 打断测试契约 + 改镜像构建；回滚粒度 = 整个前端 | —— |

### 3.3 路线 C · 混合（无构建外壳 + 局部组件化）

= 路线 A 的 shell 改动 **＋** 在同一个文件里把"组件"补齐（U1 §5.2 的清单）：

| 组件 | U1 估计行数 |
|---|---|
| 状态语义令牌（`.s-ok/.s-warn/.s-bad`） | ~20 行 CSS |
| 排序三态 + localStorage 持久化（`sortRows/nextSort/loadStoredSort/persistSort`，纯函数） | ~60 行 JS |
| 骨架屏（`.sk` + `TableSkeleton`） | ~25 CSS + ~15 JS |
| Tooltip（portal + 视口钳制，**必须支持 tap**） | ~50 行 JS |
| 二次确认按钮（3 秒 armed + Esc） | ~30 行 JS |
| Toast 队列（多条、错误常驻、hover 暂停、去重、上限 4） | ~80 JS + ~40 CSS |
| 全局写操作错误面（`apiPost` 帮助函数） | ~20 行 |
| **合计** | **≈ 340 行（+14.7%）** |

| 代价项 | 量化 |
|---|---|
| 行数 | **+20（shell，实测） + ≈340（组件，估） ≈ +360 行（+15.5%）** |
| 事件绑定 | 可**局部**改成 `data-action` 委托（U1 §7.1 建议只在改节点表时做），其余 75 处保持 |
| CSP / Docker / 测试钩子 | **全部不变** |
| 首屏体积 | 192,254 B → 约 227,000 B（gzip 约 72,000 B）；仍远小于路线 B 的 1,786,089 B（明文）/ 512,555 B（gzip） |
| 维护面 | 仍是 1 个文件；**风险点是单文件会变长到 ~2,700 行**，多人并行改同一文件容易冲突 |
| 风险 | 中低：可以按组件逐个落地、逐个回滚 |

### 3.4 三路线对照（一页看完）

| 维度 | A 抄 shell | B 换栈重写 | C 无构建+组件化 |
|---|---|---|---|
| 净增行数 | **+20（+0.86%，实测）** | 重写 2,327 → 2,500~4,000 | +360（+15.5%） |
| 事件绑定改写 | 0 | **75 处全部** | 0（可选局部） |
| 新增依赖 | **0** | npm 依赖树 + 锁文件 | 0 |
| Docker 改动 | **无** | **新增 Node 阶段** | 无 |
| 测试钩子影响 | **无** | **破坏 3 个测试**（`discev_test.go` + 2 个 `zz_*`） | 无 |
| 首屏发送字节 | **≈192 KB 明文**（gzip 61 KB） | **1.79 MB 明文（9.3×）／ 513 KB gzip（8.4×）** | ≈227 KB 明文（gzip 72 KB） |
| 首屏是否要等 JS | 否 | **是** | 否 |
| 能 adopt `script-src-attr 'none'` | 否 | **是（唯一优势）** | 否 |
| 回滚粒度 | 1 个文件 | 整个前端 | 按组件 |
| 满足"照 freellm 大改框架" | **是** | 是（但超出需求） | 是 |
| **结论** | ★ **推荐** | ✗ 否决 | 作为 A 的后续增量 |

---

## 4. freellm shell → 我们 5 个分区 的蓝图映射

### 4.1 shell 层（逐区）

| freellm shell 区域 | 源码 | 我们的对应物 | 判定 |
|---|---|---|---|
| 顶栏 `header.sticky.top-0.z-40.border-b.backdrop-blur.bg-background/80` | `App.tsx:237-243` | 新加（替换 `<aside class="sidebar">` `admin.html:451-469`） | **照搬** |
| 顶栏内层 `mx-auto flex max-w-6xl items-center px-4 sm:px-6` | `App.tsx:244-249` | 同 | **照搬**（macOS `pl-20` 那套桌面壳逻辑**放弃**，我们不做桌面壳） |
| 品牌（圆点 + 文字） | `App.tsx:134-141` | 我们已有 `.brand`（`admin.html:109-113`，含 logo-mark + 中英双行），**保留我们的**，只把它从竖排侧栏搬到顶栏 | **改造** |
| 主导航 `ms-10 hidden gap-6 md:flex` + 激活下划线 | `App.tsx:251-284`、`:117-132` | 5 个 `<button data-page>`（`admin.html:457-461`）→ 顶栏 5 项 + 768px 折叠 | **改造** |
| 分裂控件 + 子菜单（模型/分析各带 6/2 个子页） | `App.tsx:255-283`、`:99-113` | 只有「代理与模型」值得带子页（力度映射 / 模型映射 / SOCKS5 / 网关）；「订阅与配额」也可带（订阅源 / Webshare / 配额信号） | **改造**（不必照搬 6+2 的规模） |
| "顶栏最多 6 项，多的藏进子菜单"这条**规则** | `App.tsx:88-89` 注释 | 我们 5 项，**规则照抄**作为以后加页的约束 | **照搬** |
| 右侧动作区 `ms-auto hidden gap-1 md:flex`（命令面板 + 更多菜单） | `App.tsx:285-320` | 换成：主题切换 + 自动刷新下拉 + 刷新按钮（我们 `admin.html:474-482` 已有这三个） | **改造** |
| **命令面板 ⌘K**（按钮 + 全局 CustomEvent + listbox 语义） | `command-palette.tsx` 全篇 | **不做**（U1 §7.10；且它的按钮在 `md:flex` 里，手机根本点不到 —— 反面证据） | **放弃** |
| 移动端汉堡 + `w-52` 下拉（导航 + 分隔线 + 账号组） | `App.tsx:321-374` | 新加，替换现在的 64px 图标窄栏（`admin.html:388-396`） | **改造** |
| 内容容器 `mx-auto w-full max-w-6xl px-6 py-8` | `App.tsx:412-420` | `.main{flex:1;min-width:0;padding:32px 36px 72px;max-width:1280px}`（`admin.html:140`） | **改造**（宽度 1280 → 1152；`w-full` 那条注释照抄） |
| 全出血路由（`/playground` 独占视口） | `App.tsx:395`、`:427-435` | **放弃**（我们没有需要独占视口的页面；日志页是卡内滚动，用 `max-h` 就够） | **放弃** |
| 页面头 `PageHeader`（h1 + 描述 + 动作槽 + 底部分隔线） | `page-header.tsx:8-25` | **我们已经有**：`.topbar`（`admin.html:141-143`）+ `<h1 id="pageTitle">`/`<span id="pageCrumb">`（`:473`） | **照搬**（已具备，仅对齐字号/间距语义：把 `crumb` 升级成描述句） |
| 全局告警横幅槽位（`GettingStarted` / `UpdateReminder` 皆为流外或页首） | `App.tsx:480-482` | **我们已经有** `#nodeAlert`（`admin.html:491-498`，task-19 的 C-5 横幅，且注释写明"放在所有 `<section>` 之外，与停在哪一页无关"） | **照搬**（位置判断与 freellm 一致） |
| Toast 层 `fixed bottom-6 right-4 z-[60]` | `App.tsx:480` | `#toast` 单条、2.6s 自动消失（`admin.html:883`） | **改造**（见 §4.3 `C2`） |
| 路由级 `ErrorBoundary key=pathname` | `App.tsx:385-389` | 无 | **改造**（见 §4.3 `C4`） |
| 深链（SPA 兜底 + BrowserRouter） | `App.tsx:442` + `curl` 实测 | **无**（`showPage` 不碰 URL） | **改造**（见 §4.3 `H1`） |
| 60 个 locale 的 i18n | `client/src/i18n/locales`×60 | 单语言 | **放弃** |
| Geist 自托管字体（11 个 woff2） | `index.css:4-5` | 系统字体栈 | **放弃**（U1 §7.3） |
| React / Vite / Tailwind / recharts / dnd-kit 整套实现 | `client/package.json` | —— | **放弃**（U1 §7.2） |

### 4.2 我们 5 个分区 × freellm 页面模板

| 我们的分区 | 最接近的 freellm 页 | 模板复用度 | 具体动作 |
|---|---|---|---|
| **概览**（`admin.html:501-604`） | `/analytics`（`AnalyticsPage`） | **高** | 套 `PageHeader`（现在是直接进 `.stats-row`）+ 动作槽放**时间窗**（我们已有 `range` 语义）；`.stats-row` 列数改成 `2 / 4 / 4`（对齐 `grid-cols-2 md:grid-cols-4`）；图表区改两列网格、Token 趋势与用量 Hero **跨两列** |
| **节点池**（`:607-628`） | `/models/chat`（`FallbackPage` 的主表段） | **高** | 把 `filter-chips` + `actions`（`:608-620`）**上提到页头动作槽**；表格加**可排序表头 + 骨架屏 + 带动作空态**；行内展开做详情（不新增分区）；补 `EmptyState` 的 action（"去订阅页"） |
| **订阅与配额**（`:631-677`） | `/keys`（`KeysPage` 的 Section 堆叠） | **中** | 套 `space-y-8` 的 Section 堆叠；把「配额信号回显」做成只读 chips；页头动作槽放"保存全部" |
| **代理与模型**（`:680-719`） | `/keys` 的 `ProxySettingsSection` | **中** | 同上；页头动作槽放"保存全部"；这张表最像 freellm 的"设置类"页面，**适合做第一个试点** |
| **运行日志**（`:722-788`） | `/logs`（`LogsPage`） | **很高** | 几乎 1:1：工具条（级别 chips + 类型 + 搜索 + 暂停）→ 口径提示 → 卡内滚动（`max-h`）→ 加载更多；把「排序/筛选只覆盖已加载行」写进 UI 提示（freellm `en.json:analytics.sortHint`） |
| **404 / 未找到** | `NotFoundPage` | **照搬** | `max-w-md rounded-3xl border bg-card p-8 text-center` + 回首页按钮 |

### 4.3 改造清单（按性价比排序，全部在路线 A 内）

| # | 动作 | 位置 | 估计行数 | 为什么 |
|---|---|---|---|---|
| `H1` | **深链**：`showPage()` 写 `location.hash`；启动时按 hash 还原；监听 `hashchange` | `admin.html:868-876` + 启动段 | ~8 | 刷新/后退/分享全废是现存缺陷；零风险零依赖 |
| `S1` | **侧栏 → 顶栏**（含 768px 折叠 + 汉堡下拉） | markup `:451-469`；CSS `:109/123-130/389-395` | +94 / −36 | 用户要的"大框架"本体 |
| `S2` | **断点收敛**到 `640/768/1024`，768 为 shell 折叠点 | `:162/171/179/296/297/388/410` | 改数值 | 现在 4 档不成体系；且已有注释记录断点在两个版本间"打补丁"（`admin.html:435-436`：`.filter-bar` 的 `flex-wrap` 只能留在基础规则，"因为 main 没有 test 那条把它移进去的 @media(max-width:960px)"） |
| `S3` | **页面头对齐**：`#pageCrumb` 语义从"面包屑"升级为"一句描述"；`#pageTitle` 字号对齐 `text-2xl`(24px) | `:142-143`、`:473`、`:867` | ~10 | 我们已有 95% 的 `PageHeader`，补齐即可统一 5 个分区 |
| `C1` | **骨架屏**（`.sk` + `tableSkeleton()`） | 新加 | ~40 | 首屏与切页不再"闪空"；freellm 全站如此 |
| `C2` | **Toast 队列**（多条 / 错误常驻 / 去重 / hover 暂停） | 替换 `:883` `showToast` | ~120 | 现在错误消息 2.6 秒就消失，运维会漏掉 |
| `C3` | **二次确认按钮**（3 秒 armed + Esc，替代 3 处 `confirm()`） | `:1710/1931/2184` | ~30 | 原生 `confirm()` 阻塞渲染、样式不可控、无法携带上下文 |
| `C4` | **页面级错误面**（切页出错时显示"崩溃卡 + 重试"，而不是整页白屏） | 新加 | ~30 | 我们有 4 处 `addEventListener` 与 50 处 `innerHTML`，一处抛错会连带整页失效 |
| `C5` | **空态组件**（虚线卡 + 图标 + 标题 + **action**） | 新加 | ~25 | 节点表筛选到 0 行时现在是空表格 |
| `C6` | **排序三态 + localStorage 持久化**（纯函数，可逐行照搬 `lib/table-sort.ts`） | 节点表 / 订阅表 | ~60 | **唯一"复制粘贴级"的 freellm 资产**（U1 §5.3） |
| `D1` | **ECharts CDN → 本地**（现在 `admin.html:15` 依赖 jsdelivr） | `:15` | 1 行 + 静态资源 | 与离线/内网部署冲突；本次不做但必须记账 |

---

## 5. 决断

### 5.1 推荐：**路线 A**（保持无构建单文件，只抄它的 shell），随后按 §4.3 增量做路线 C 的组件

**三条最硬的理由**（都可以被上面的数字复核）：

1. **用户要的"大框架"在无构建单文件里的价格是 +20 行 / +0.86%（实测，不是估算）。**
   唯一的框架级改动是"侧栏换顶栏"，**我把原型真做出来了**：`docs/mockups/u2a/routeA-proto-admin.html`，改 6 个 hunk、净 +20 行、gzip +285 B，桌面与移动都过 Playwright 验证（顶栏 54px、5 项导航、390px 汉堡抽屉、无横向溢出、零 pageerror）。页面头我们已经有了（`.topbar`，`admin.html:141-143`），所以连"改框架"里最贵的那块都不用动。换栈买不到这个框架 —— 框架本身只有约 35 行 CSS + 20 行 markup 的体量。

2. **换栈要付 9.29 倍明文首屏（8.4 倍 gzip）+ 打断 3 个测试。**
   产物体积 gzip **61,037 B → 512,555 B**（同口径实测）；**明文 192,254 B → 1,786,089 B ＝ 9.29 倍**（我们服务端无压缩中间件，这是浏览器真实要拉的字节）；面板从"HTML 有内容"变成"等 1.68 MB JS"；
   并且**有 3 个测试**直接把 `static/admin.html` 当文本资产：`discev_test.go:24/29`（抽 `CL_ISSUE_TYPES`）、`zz_c5_verify_wiring_test.go:209`、`zz_c5v2_verify_wiring_test.go:166`（查渲染函数名与 CSS 规则串）—— 引入构建后它们**必须改 Go 代码**，而本轮的任务边界是"不许改任何 Go 文件"。这是**流程风险**，用 UI 收益无法抵消。

3. **freellm 的价值 95% 在设计、10% 在代码（U1 §5.3），而设计可以 100% 手抄。**
   我们要的是它的**信息层级、间距节奏、状态色分档、四态处理、页面头形态**；这些在路线 A 里逐条可落地（§4.3 的 `S1/S2/S3/C1-C6`）。它**搬不过来**的部分（React/Tailwind/recharts/dnd-kit/60 locale/Geist/命令面板）恰好都不是我们要的。

**为什么不是"先并列看情况"**：三条路线的**收益集合是相同的**（用户要的框架与观感），差别只在**代价**（+0.86% 已实测 vs 整体重写 vs +15.5%）与**风险**（零 vs 高 vs 中）。收益相同时选代价最低者，这是可判定的，不需要骑墙。

**更关键的一点**：路线 A 的这一小步**不是承诺、是已完成的可运行原型**。所以这个决断的剩余风险几乎为零 —— 不满意可以当场回滚一个文件。

### 5.2 实施顺序（路线 A）

```
Step 0  冻结基线：记下当前 admin.html 的 sha256（16:42 快照 = 2304d22c…，2,327 行）
        —— 另一名队友正在改这个文件，不冻结会边改边撞
Step 1  S1 侧栏→顶栏 + S2 断点收敛  ← 【已有可运行原型，直接拿来当起点】
        docs/mockups/u2a/routeA-proto-admin.html（6 个 hunk，净 +20 行）
        落地前需 rebase 到 Step 0 的最新基线（原型基于 16:42 版本）
Step 2  H1 深链（~8 行，独立可验证：刷新后停在原分区）
Step 3  S3 页面头对齐（5 个分区一次性统一）
Step 4  C1 骨架屏 + C5 空态（先做节点表与日志页，收益最直接）
Step 5  C2 Toast 队列 + C4 页面级错误面（可观测性）
Step 6  C6 排序持久化 + C3 二次确认（表格与危险操作）
Step 7  D1 ECharts 本地化（独立，随时可插）
```
每步都满足：**不新增依赖、不改 Docker、不改 Go、不改 `discev_test.go`**。

> **Step 1 不是从零开始**：原型已跑通并被 Playwright 验过（见 §3.1）。实施时只要把那份 diff 应用到最新基线即可；唯一要额外注意的是原型没做的两项 —— **深链**（Step 2）和**断点数值收敛**（可并入 Step 1）。
> 原型还暴露了一个原本没预料到的必改点：侧栏一去掉，`.main` 就失去了横向参照，**必须补 `width:100%;margin:0 auto`**，否则内容贴左不居中（`admin.html:140`）。

### 5.3 推荐会翻转的反向条件（什么时候该换栈 / 该重写）

**→ 翻转成路线 B（引入构建）的条件，满足任意两条即应重新评估：**

- **B1 · 规模**：`static/admin.html` 超过 **~4,000 行**（当前 2,327），或前端 JS 超过 **~2,500 行**（当前约 1,532，`:794-2325`）。到那时 `innerHTML` 字符串拼接（现 50 处）与 162 个函数的**隐式耦合**会让每次改动都在赌不碰坏别处。
- **B2 · 状态复杂度**：出现**跨分区共享的可变状态**（例如"实时节点地图 + 拖拽排序 + 多步向导"），单文件的手写状态同步（现在是 `currentPage` + 若干全局变量 + `setInterval`）会开始出错。
- **B3 · 多消费者**：前端不再只服务这一个面板（例如要做手机独立页、桌面壳、或把面板嵌进另一个 app）—— 那时"一份 HTML 字符串模板"无法复用。
- **B4 · 有人接管构建流水线**：团队愿意并能够拥有 Node 工具链（`Dockerfile` 加阶段、`npm ci` 可复现、`discev_test.go` 的契约改成"读生成物"）。**注意 B4 是前置条件而不是结果** —— 没人接管构建，路线 B 一定烂尾。
- **B5 · 需要 JS 生态的某件硬需求**：例如必须用 ECharts 之外的图表库、必须做虚拟滚动大表（1 万行以上）、必须做富文本/Markdown 编辑。这些在无构建下要么写不动、要么等于自己实现一个框架。

**→ 翻转成路线 C 更激进形态（保留无构建但做更强的组件抽象）的条件：**
- **C1'**：**多人并行改同一个 `admin.html`** 成为主要痛点（合并冲突 > 每周 1 次）。此时优先考虑"拆成 `admin.html` + 少量 `admin-*.js` 仍由 `go:embed` 打进去"（**注意**：这会改 `embed.go:7` 的 embed 列表，需与后端协调）。
- **C2'**：需要**真正可测试**的前端逻辑。现在 162 个函数全靠手测；若要把 `render*` / `sortRows` 之类纳入自动化测试，纯函数抽出来 + 一个极小的测试运行器即可，**仍不需要 Vite**。

**→ 明确不算翻转条件的（免得被误用）：**
- "用户说要 freellm 的样子" —— 路线 A 已满足。
- "freellm 用了 React" —— U1 §5.3 已证：可抄的是设计不是实现。
- "CSP 更安全" —— 我们**当前没有 CSP**；采纳 `script-src-attr 'none'` 需要先重写 75 个 `onclick`，这是**另一件独立的事**，不该挂在大改里（U1 §7.1）。

---

## 6. 限制与不确定性（如实标注：哪部分是实拍、哪部分是源码推导、哪部分是错误态）

### 6.1 截图的口径（**逐张标注**）

| 截图 | 口径 | 可信度 |
|---|---|---|
| `fl-unauth-login-1440x900.png` | **100% 真实**：未登录访问 `/`，无任何桩 | ★★★ 完全可信 |
| `fl-shell-models-chat-1440x900.png`、`fl-logs-1440x900.png`、`fl-analytics-1440x900.png`、`fl-nav-dropdown-1440x900.png`、`fl-shell-models-chat-390x844.png`、`fl-nav-menu-mobile-390x844.png`、`fl-logs-390x844.png` | **★ 部分真实**：**只放行了 `/api/auth/status`**（返回 `{needsSetup:false,authenticated:true}`）以通过登录闸门；**其余所有 `/api/*` 请求都真的打到了 127.0.0.1:47996 并返回真实的 401**。因此：**shell（顶栏/导航/容器/页头/断点行为）是真实的**；**页面主体是 freellm 自己的真实错误态/空态**，**不是真实数据**。截图里看到的红色错误条就是 `logs.loadFailed` 的真身 | ★★☆ shell 可信；页面体**不可**当数据态读 |
| `fl-*-light-*.png`、`fl-header-closeup-light-1440x900.png`、`fl-nav-dropdown-light-1440x900.png`、`fl-mobile-navmenu-closeup-light-390x844.png` | 同上；`colorScheme: light` | ★★☆ 同上 |
| `blueprint-*.png` | **我们自己做的蓝图**（`shell-blueprint.html`）的渲染，非 freellm | — |
| `routeA-proto-*.png`（5 张） | **我们自己做的路线 A 原型**（`routeA-proto-admin.html`）的渲染：`overview`/`nodes`/`logs` 1440×900、`mobile-closed`/`mobile-menu` 390×844 | ★★★ **纯本地**：静态服务器无后端，页面里的 404 是缺 API，不影响 shell 判断 |

**我没有做的事 / 做不到的事**：
- **没有登录**（无凭据；Lead 明确不提供、不许撞密码、不许走 `/forgot-password`），因此**拿不到任何真实数据页**。
- **没有对 freellm 做任何写操作**：全部请求为 `GET`；唯一一次非 GET 是 Playwright 渲染时 SPA 自身发出的 `/api/auth/status`，被我本地拦掉并替换为静态响应（**没有到达服务端**）。
- **拿不到** freellm 的移动端**表格**行为（是否卡片化）：390px 下页面体是错误态，无行可看。**U1 §4.6 的这条空白本轮仍未闭合**，只能引用 U1 的源码推断（`overflow-x-auto`，不堆叠）。（U1 §11 第 1 条把它列为待验证；本轮**未测到**。）

### 6.2 数字口径

- `static/admin.html` = **2,327 行 / 192,254 B / gzip 61,037 B**，复核于 2026-10-19 16:42（sha256 `2304d22c…`）。**该文件在本轮被另一名队友改了两次**：初稿 15:5x 的 2,307 行 / 190,348 B / gzip 59,987 B → 16:09 的 2,321 行 → 16:42 的 2,327 行。全部行号已重锚并逐条复核；任务书里写的 2,271 行是更早的快照。**引用行号前请先确认 sha256。**
- **HTTP 压缩：我们没有。** 在非 vendor 的 Go 代码里 grep `gzip|flate|brotli|Content-Encoding|Accept-Encoding|Vary|ServeContent` → **零命中**；`adminPageHandler`（`main.go:7301-7304`）只设 `Content-Type` 然后 `w.Write(adminPageHTML)`。因此：**我们的面板是明文 192,254 B 上线的**。freellm 的实例实测回 `Content-Encoding: br`。这是初稿漏掉的一条硬事实 —— 它让"换栈的首屏代价"从 8.4 倍（gzip 口径）变成 **9.29 倍**（明文口径）。
- freellm 产物 = `/tmp/fl_index.js` 1,682,736 B（gzip 494,922）+ `/tmp/fl_index.css` 101,781 B（gzip 16,844），是 U1 抓的 2026-10-19 09:25 版本；与当前实例 `index.html` 引用的 `index-B8n16Tcf.js` / `index-B76V_SXP.css` 一致。
- **路线 A 的 shell 改造已从"估算"升级为"实测"**：原型 `docs/mockups/u2a/routeA-proto-admin.html`，实测 **+20 行 / +2,753 B / gzip +285 B**，Playwright 在 1440×900 与 390×844 两个视口验证通过（顶栏 54px、5 项导航、390px 汉堡抽屉、`scrollWidth==clientWidth`、零 pageerror）。**初稿的 +43 行估算偏差 −53%**，原因见 §3.1 脚注。
- 路线 C 的组件行数（≈340 行）**仍是估算**；路线 B 的"2,500~4,000 行 TSX"**也是估算**。**这两项标注为估算，不要当实测引用。**
- **首屏 gzip 对比是实测**（两侧都有命令与字节数），这是本文最可靠的一组数字。

### 6.3 没查到的部分

| # | 空白 | 影响 |
|---|---|---|
| 1 | freellm 的**移动端表格**行为（卡片化？仅横滚？） | 未测到。只能沿用 U1 的源码推断 |
| 2 | freellm 的**真实数据态**观感（KPI 数字、图表形态、表格密度） | 未测到。§1.3 的 wireframe 全部来自源码结构，不是观感实测 |
| 3 | freellm 是否**按路由拆包**（我们只见到 1 个 JS 产物） | 若它其实做了 code splitting，则"512 KB gzip 首屏"这个对比会偏保守。**`/tmp/fl_index.html` 只引用 1 个 JS + 1 个 CSS，未发现其它 chunk** ⇒ 判为未拆包（但未穷尽验证网络请求） |
| 4 | 我们 `admin.html` 的 ECharts CDN 依赖在**离线/内网**下的实际表现 | 未测到；`admin.html:15` 的 `defer` 脚本加载失败时图表区行为未知 |
| 5 | ~~`discev_test.go` 之外是否还有别的依赖~~ | **本轮已补查**：`grep -rln 'static/admin' --include=*.go .` → 除 `embed.go:7/21`（生产 embed）与 `calllog.go:1170`（只是注释引用）外，还有 **3 个测试**读这个文件：`discev_test.go:29`（抽 `CL_ISSUE_TYPES`，常量现在在 `admin.html:2036`）、`zz_c5_verify_wiring_test.go:209`（查 `renderNodeAlert`/`.node-alert.on` 等字符串）、`zz_c5v2_verify_wiring_test.go:166`（查 4 个字符串）。**注意这两个 `zz_*` 文件都被 `.gitignore:26` 的 `*_test.go` 忽略、不在 git 里**，属于本地校验脚本 —— 但它们同样依赖 `admin.html` 是**纯文本资产**，路线 B 一样会打断 |
| 6 | 我们的面板在**真机上**的表现（iOS Safari / 微信内置浏览器） | 未测到。原型只在 Chromium 里验过 |

---

## 附录 A · 证据索引

**freellm 源码（权威，`/home/jingyx/freellmapi/client/src`）**

| 结论 | 出处 |
|---|---|
| shell 层次（Provider → AuthGate → AppShell → Navbar/PageContainer → Routes） | `App.tsx:437-490` |
| 外壳列（min-h-screen vs h-dvh） | `App.tsx:427-435` |
| 全出血路由集合 | `App.tsx:395` |
| 内容容器 max-w-6xl + `w-full` 承重注释 | `App.tsx:400-420` |
| 顶栏 header / 内层 / 导航 / 右侧区 / 汉堡 | `App.tsx:237-243` / `:244-249` / `:251-284` / `:285-320` / `:321-374` |
| 导航项数组、子菜单表、"最多 6 项"规则 | `App.tsx:67-74` / `:99-113` / `:88-89` |
| 导航激活下划线 | `App.tsx:117-132` |
| 品牌 | `App.tsx:134-141` |
| 路由表 24 条 | `App.tsx:452-477` |
| 路由级 ErrorBoundary（key=pathname） | `App.tsx:385-389` |
| 全局 staleTime 30s + MutationCache 统一错误 toast | `App.tsx:49-65` |
| 统一页头 PageHeader | `components/page-header.tsx:8-25` |
| 骨架屏 / TableSkeleton / CardSkeleton | `components/ui/skeleton.tsx:5-45` |
| 空态 EmptyState（带 action 槽） | `components/empty-state.tsx:8-31` |
| 崩溃卡 CrashScreen | `components/error-boundary.tsx:11-57` |
| AuthGate 启动态 | `components/auth-gate.tsx:443, 488-518` |
| 命令面板：快捷键 / 事件 / 分组 / 键盘 / aria | `command-palette.tsx:76-89, 109-180, 196-207, 224-249` |
| 命令面板全局事件 | `command-palette-state.ts:1-5` |
| 全局仅 2 处 keydown 监听 | `grep -rn "addEventListener('keydown'" client/src` → `tooltip.tsx:55`、`command-palette.tsx:86` |
| 各页区域划分 | `pages/FallbackPage.tsx:268+`、`LogsPage.tsx:175+`、`AnalyticsPage.tsx:710+`、`KeysPage.tsx:68+`、`AgentsPage.tsx:80+`、`PremiumPage.tsx:68+`、`NotFoundPage.tsx:12` |
| 断点 | `/tmp/fl_index.css` 的 `@media`；`client/src` 的 `sm:/md:/lg:/xl:` 计数 |

**freellm 实例（只读 curl / Playwright）**

| 结论 | 命令 / 产物 |
|---|---|
| 栈与 CSP | `curl -s -D- -o /dev/null http://127.0.0.1:47996/` |
| 空 `#root` + 1 JS + 1 CSS | `/tmp/fl_index.html`（1,572 B） |
| SPA 兜底（任意路径 → 200 + index.html） | `curl -s -o /dev/null -w "%{http_code} %{size_download}" .../logs` 等 4 条 |
| 顶栏实测几何（h=53 / main x=144 w=1152 / 390px 导航项=0） | Playwright（`/tmp/pw_dom.js`、`/tmp/pw_nav.js`） |
| 移动菜单文案 | Playwright 点击汉堡后的 `innerText` |
| 截图 17 张 | `docs/mockups/u2a/fl-*.png` |

**我们的仓库**

| 结论 | 出处 |
|---|---|
| `go:embed` 两个文件 | `embed.go:7` |
| Dockerfile 纯 Go、无 Node | `Dockerfile`（`golang:1.26-alpine` → `alpine:3.20`） |
| 测试契约 `CL_ISSUE_TYPES` | `discev_test.go:24`（正则）、`:29`（读文件）、`:35/:42`（失败即 Fatal） |
| 另外两个读 `admin.html` 的测试 | `zz_c5_verify_wiring_test.go:209`、`zz_c5v2_verify_wiring_test.go:166`（均被 `.gitignore:26` 忽略，仅本地） |
| 侧栏布局 CSS / markup | `admin.html:123`（`.sidebar` 204px sticky 100vh）、`:451-469`（`<aside>`） |
| 导航 CSS / markup | `admin.html:125-129`、`:456-462` |
| 内容列 | `admin.html:140`（`.main` max-width 1280） |
| 我们已有的"页面头" | `admin.html:141-143`（`.topbar`）、`:473`（`#pageTitle`/`#pageCrumb`）、`:867`（`pageTitles`） |
| 全局告警横幅 | `admin.html:491-498`（`#nodeAlert`，含位置理由注释） |
| `showPage` 无 URL | `admin.html:868-876`；`grep "location.hash\|hashchange"` → 0 |
| 断点四档 | `admin.html:162/171/179/296/297/388/410` |
| 移动端侧栏折叠 | `admin.html:389-395`（960 断点块 `:388-409`） |
| 75 处 onclick / 4 处 addEventListener / 50 处 innerHTML / 162 个函数 / 21 个 render* | 计数命令见 §2 表 |
| ECharts CDN | `admin.html:15` |
| 单条 toast 2.6s | `admin.html:883` |
| 3 处原生 `confirm()` | `admin.html:1710 / 1931 / 2184`（16:42 复核） |
| **路线 A 可运行原型**（实测 +20 行 / gzip +285 B） | `docs/mockups/u2a/routeA-proto-admin.html` + `routeA-proto-*.png`（5 张） |
| **服务端无 HTTP 压缩** | 非 vendor Go 代码 grep `gzip\|flate\|brotli\|Content-Encoding\|Accept-Encoding\|Vary` → 0；`main.go:7301-7304` 的 `adminPageHandler` |
| freellm 服务端有压缩 | `curl -H 'Accept-Encoding: gzip, br'` → `Content-Encoding: br`；三件 wire = 714+489,128+16,732 = 506,574 |

**本文的量化数字**

```bash
# 首屏体积对比
gzip -c /home/jingyx/freellmapi/client/dist/assets/index-*.js 2>/dev/null | wc -c   # 或：gzip -c /tmp/fl_index.js | wc -c  → 494922
gzip -c /tmp/fl_index.css | wc -c                                                  # → 16844
gzip -c static/admin.html | wc -c                                                  # → 59987
# 断点
grep -o '@media[^{]*{' /tmp/fl_index.css | sort | uniq -c | sort -rn
for bp in sm md lg xl 2xl; do echo -n "$bp: "; grep -roh "\b$bp:[a-z0-9-]*" /home/jingyx/freellmapi/client/src --include=*.tsx | wc -l; done
```
