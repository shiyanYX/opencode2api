# 管理面板布局与卡片样式移植记录（Task 7）

把 test 视觉版中**两边共有的容器类**的布局/卡片样式移植到 main 的 `static/admin.html`。
范围由 `comm -12` 得出的 100 个共有类中「定义不同」的 21 个构成。

- 来源：`/tmp/admin-test-visual.html`（test 视觉版，`<style>` 619 行）
- 目标：`static/admin.html`（`<style>` 368 行，编译期经 `//go:embed` 打包，见 `embed.go:7`）
- **只改 `<style>` 块**，未动 HTML 结构、JS、Go 代码。

## 逐条决策表

| 类名 | 决策 | 理由 |
| --- | --- | --- |
| `.badge` | 移植 | letter-spacing `.02em→.03em`，纯字距微调，DOM 一致 |
| `.btn` | 移植 | `:active` 加 `scale(.98)`；新增 `:focus-visible` 2px 焦点环与 `:disabled` 禁用态，均为无障碍/可辨识性改进 |
| `.btn-danger` | 移植 | `:hover` 用既有令牌 `--red-dim`/`--red` 混色，暗色与亮色 `:root` 与 `[data-theme="light"]` 均已定义；属普通 UI 颜色，非图表取色 |
| `.btn-ghost` | 移植 | `:hover` 补 `background:var(--surface-2)`，令牌双主题已定义 |
| `.btn-secondary` | 移植 | `:hover` 由 `filter:brightness(1.07)` 改为 `color-mix(--blue 85%,--bg)`，令牌双主题已定义；test 中该 hover 重复出现两次，移植时去重为一条 |
| `.card` | 移植 | padding 22px→20px + `transition` + `box-shadow` + `:hover` 阴影；`h2` 14.5/700/-.2px → 15.5/600/0 + `line-height:1.4` + `padding-bottom:9px` + `border-bottom` + gap 9→8px + margin-bottom 14→12px；纯布局/字体，DOM 结构两边一致 |
| `.card h3` / `.h3-note` | 不移植 | main 的 HTML 里 `<h3>` 出现 **0** 次（test 有 4 处），依赖 test 独有 DOM 结构，移植即死规则 |
| `.card` 末尾间距统一两条 | 移植（**收进 `@media(max-width:960px)`**） | 见下方「偏离说明 A」 |
| `.chip` | 移植 | `transition .14s→.15s` + `:focus-visible` 焦点环。`.time-chips` 虽已被 Task 5 移除，通用 `.chip` 仍用于 `#stateChips`/`#logLevelChips`，已逐页截图确认未受影响 |
| `.cl-chart` | 不移植 | **反向差异**（test 已删、main 有）。main 保留 ECharts 实现，test 的 CSS 柱状热力图不适用 |
| `.cl-tbl` | 不移植 | **反向差异**（test 已删 `ratebar i.warn/.bad`、main 有），同上 |
| `.cl-list` | 移植 | 新增 `::-webkit-scrollbar` 系列。与 `.log-wrap` 那组内容重复（test 里 `.log-wrap` 那组已包含 `.cl-list`/`.tbody-scroll`），只保留一份 |
| `.filter-bar` | 移植（**保留 `flex-wrap`**） | 见下方「偏离说明 B」 |
| `.login-card` | 不移植 | test 用硬编码 `rgba(0,0,0,.2)` 阴影，main 用共享令牌 `var(--shadow)`（亮色主题另有取值）。移植会**丢失亮色主题适配**，与 Step 3「共享令牌优先」原则相反 |
| `.log-line` | 移植 | hover 背景 50%→45%，极微调 |
| `.logo-sub` | 移植 | letter-spacing `.18em→.2em` |
| `.logo-text` | 移植 | letter-spacing `-.01em→-.02em` |
| `.log-wrap` | 移植 | 新增 `::-webkit-scrollbar` 系列（width:5px、thumb `var(--border)` 等） |
| `.nav` | 移植 | `transition .14s→.15s` |
| `.sidebar` | 移植 | 新增 `overflow-y:auto`；与既有 `height:100vh` + `position:sticky` + flex 布局兼容，防长导航溢出 |
| `.stat` | 移植 | 新增 `transition` + `:hover` 边框/阴影；`.stat .k` 字距 `.07em→.08em`；`.stat .v` 字距 `-.02em→-.01em`（合并进原规则，未新增覆盖块） |
| `.tbl` | 部分移植 | 移植 `th` 去掉 `text-transform:uppercase` + 字距 `.01em`、`tbody tr:hover` 55%→50%、新增 `input/select:focus-visible` 焦点环；**不移植** `tbody:empty::after`（新增空态文案注入，属新功能而非样式对齐，且会与既有 `.empty-hint` 文案重复）；**保留** main 独有的 `tbody tr.hl`（反向差异，main JS 仍在用，见下） |
| `.topbar` | 移植 | `h1` letter-spacing `-.4px→0` + `line-height:1.35`；`.crumb` 字距 `.04em→-.01em` |

### 两处需要单独说明的保留（反向差异）

- **`.tbl tbody tr.hl`**：test 没有这条规则，但 main 的 JS（节点池表格渲染）会输出 `<tr class="hl">` 标记当前节点，**在用**。按「反向差异不移植」的原则保留。
- **`.cl-chart .bar .tip` / `.cl-tbl .ratebar i.warn/.bad`**：test 已删、main 在用，属 CSS 热力图实现，当前渲染器是 ECharts，不适用。

## 偏离说明（两处，均为「按 brief 字面执行会出问题」的判断）

### A. `.card{padding:16px}` 与 `.card h2{margin:0 0 10px}` 的位置

brief 描述这两条是「响应式 `@media` 内」的规则。**实际在 test 文件里它们不在任何 `@media` 内**——
是 `/tmp/admin-test-visual.html` 第 574–576 行的**全局无条件覆盖**（注释为「设计打磨: 卡片内间距统一」）；
test 的 `@media` 块（579–590 行）里根本没有 `.card` 规则。

若照搬为全局规则，级联会把 `.card` 的 `padding:20px 24px` **完全覆盖成 16px**（所有屏幕尺寸），
使本次明确要移植的「22px→20px」变得毫无效果。因此按 brief 所述的「移动端」语义处理：
收进 main 既有的 `@media(max-width:960px)` 块（与 main 现有紧凑断点一致），
桌面端保持 20px、窄屏收紧到 16px。

### B. `.filter-bar` 的 `flex-wrap`

test 的基础规则**删掉了** `flex-wrap:wrap`，改由它自己的一条
`@media(max-width:960px){.filter-bar{flex-wrap:wrap;gap:12px}}` 补回来。
main 侧没有任何 `.filter-bar` 的 media 规则，直接照搬会让 Task 5 的下拉筛选栏在 ≤960px 时不换行而溢出。

处理：移植 `gap:20px` + `margin-bottom:20px` + `padding-bottom:18px` + 底部分隔线，
**保留** 基础规则里的 `flex-wrap:wrap`，并额外补一条 `@media(max-width:960px){.filter-bar{gap:12px}}`
（对应 test 的窄屏间距收紧）。这样拿到 test 的视觉，且不比现状更容易溢出。

**页面实测**：概览页趋势卡头部右侧为 `.filter-bar`，两个下拉框与下方趋势图之间有充足留白，
未出现 brief 所担心的「与下方趋势图标题挤在一起」。

## 另外两处与 brief 描述不符的事实（已核实，决策不受影响）

1. **`.btn:disabled` 并非「main 完全没有」**：main 已有 `.btn[disabled]{opacity:.5;cursor:not-allowed}`
   （原第 95 行）。test 是在其**之后**再加一条 `.btn:disabled,.btn[disabled]{opacity:.45;…;pointer-events:none}`
   （test 第 492 行）覆盖它。移植时按 test 的先后顺序合并为一条、放在原位置，
   否则会被 main 原有的 `.btn[disabled]` 反向覆盖回 `.5`。
   （注：brief 的收集正则 `^\.c[,{: ]` 匹配不到 `.btn[disabled]` 这类以 `[` 开头的选择器。）
2. **main 已有全局 `:focus-visible`**：`:focus-visible{outline:none;box-shadow:…}`（第 71 行）。
   若只移植 test 的 `.btn:focus-visible{outline:…}` 而不动全局规则，会同时出现
   outline 环与 box-shadow 环两重焦点环。故把全局规则一并改为 test 的
   `:focus-visible{outline:2px solid var(--accent);outline-offset:2px}`（与 test 第 70 行一致），
   全站焦点环机制统一为 outline。这是一处 21 类清单之外的额外改动，特此记录。

## 验证结果

`scripts/verify_ui.py`：`baseline` 12/12、`elements` 13/13、`charts` 8/8 全部 PASS。
行为不变量（自写 Playwright 探针）与五页 × 双主题截图结论见
`.superpowers/sdd/2026-09-28-admin-visual-port/task-7-report.md`。
