# docs/mockups/u2b · 截图与样例索引

> 本目录是 **U2b** 的交付物。三件东西：**风格配方**（`docs/research/U2b-freellm-style-recipe.md`）、
> **可点的样例**（`style-specimen.html`）、**成对截图**（本目录的 PNG）。
>
> **看什么**：先双击 `style-specimen.html`（`file://` 直接开，不需要服务器），
> 再拿 `specimen-*` 与 `freellm-*` 两组图并排看。**看首屏不滚动的那张**判断密度最准。

---

## ⚠️ 关于「鉴权放行 / 错误态」的逐张标注（必读）

freellm 的活实例 `http://127.0.0.1:47996` **需要登录**才能看到 Analytics / Models 页面。
本研究**没有拿到、也没有尝试获取**该实例的账号密码（详见下方「为什么没有登录」）。
因此本目录里的 freellm 截图分成两类，**文件名已把鉴权状态写进去**，请勿把它们当成同一个东西：

| 文件名模式 | 是什么 | 数据态？ |
| --- | --- | --- |
| `freellm-live-**AUTHGATE**-login-*.png` | **活实例的登录闸门页**（鉴权未放行） | ❌ **不是数据态**。这是一张"登录表单"截图，画面里没有任何业务数据。它的价值只在于：这是**活体渲染**的 freellm 设计系统（字体、令牌、圆角、按钮语法、输入框都是真的）。 |
| `freellm-upstream-*.png` | **freellm 仓库自带的官方产品图**（`repo-assets/analytics.png`、`repo-assets/models.png`） | ✅ 是真实产品截图，但**来自上游仓库、不是本机实例**；尺寸 1280×989，与样例的 1440×900 不同尺寸。 |
| `specimen-*.png` | **我做的重画样例**（`style-specimen.html` 的渲染结果） | ✅ 是样例自身，数据是演示数据，字段名与告警数值为真（来源见配方报告 §6.2）。 |

**没有"登录成功后的活实例截图"这一类。** 本目录里不存在这种文件，请勿在其它地方假设它存在。

---

## 逐文件清单

### A 组 · 我们的重画样例（`style-specimen.html` 渲染）

| 文件 | 尺寸 | 鉴权态 | 说明 |
| --- | --- | --- | --- |
| `specimen-desktop-1440x900-fold.png` | 1440×900 | 无（本地文件） | **首屏不滚动** —— 判断信息密度用这张 |
| `specimen-desktop-1440x900.png` | 1440×全页 | 无 | 概览视图全页 |
| `specimen-nodes-desktop-1440x900.png` | 1440×全页 | 无 | **节点池视图**（评分列、状态徽章、告警横幅都在这张） |
| `specimen-dark-desktop-1440x900.png` | 1440×全页 | 无 | 暗色概览 |
| `specimen-dark-nodes-desktop-1440x900.png` | 1440×全页 | 无 | 暗色节点池 |
| `specimen-mobile-390x844.png` | 390×全页 | 无 | 移动概览 |
| `specimen-nodes-mobile-390x844.png` | 390×全页 | 无 | 移动节点池（表格在容器内横滚，页面不溢出） |
| `specimen-mobile-menu-390x844.png` | 390×844 | 无 | 移动导航展开 |

### B 组 · freellm 参照

| 文件 | 尺寸 | 鉴权态 | 来源 |
| --- | --- | --- | --- |
| `freellm-upstream-analytics-1280x989.png` | 1280×989 | 已登录（上游官方图） | 仓库 `repo-assets/analytics.png` —— 对应我们的「概览」 |
| `freellm-upstream-models-1280x989.png` | 1280×989 | 已登录（上游官方图） | 仓库 `repo-assets/models.png` —— 对应我们的「节点池」 |
| `freellm-live-AUTHGATE-login-1440x900.png` | 1440×900 | ⚠️ **未放行（登录闸门）** | 活实例实时截图；仅用于对比设计系统，**无业务数据** |
| `freellm-live-AUTHGATE-login-390x844.png` | 390×844 | ⚠️ **未放行（登录闸门）** | 同上，移动档 |
| `freellm-live-AUTHGATE-login-dark-1440x900.png` | 1440×900 | ⚠️ **未放行（登录闸门）** | 同上，`localStorage.theme=dark` 后的暗色令牌活体证据 |

### 其它

| 文件 | 说明 |
| --- | --- |
| `style-specimen.html` | 离线单文件样例。双击打开；左上切「概览 / 节点池」，右上切明暗；**页面底部点任意一条规则会高亮对应区域**。 |
| `verify.py` | 上面四条硬指标的**可复现自测脚本**（Python Playwright）。跑法：`python3 docs/mockups/u2b/verify.py`；它会重新生成全部 PNG 并打印原始 JSON 结果。**注意**：重跑会覆盖截图，且需要只读访问 `http://127.0.0.1:47996`。 |

---

## 为什么没有登录（三条路径的结论）

按 Lead 给的指引逐条试过：

1. **源码（最强证据）** —— ✅ 用了。风格类结论（构图配方、层级建立方式、表格密度、状态色用法）全部从
   `/home/jingyx/freellmapi/client/src` 抠出确切类名/数值，报告里每条都带 `文件:行号`。
   抓下来的 `/tmp/fl_index.css`（101,781 字节）也继续用了。
2. **本地默认账号** —— ❌ **不存在**，因此无法登录。核实过程（全部只读）：
   - `/home/jingyx/freellmapi/.env` 只有两个键：`ENCRYPTION_KEY`、`PORT`（脚本按 `=<redacted>` 打印键名，未回显任何值）。
   - `.env.example` 全文检索 `admin / seed / password / default / credential / login` → **无任何账号或默认口令**，只命中速率限制之类的注释。
   - `CONTRIBUTING.md`、`README.md`、`README.zh-cn.md`、`docs/` 检索默认账号/口令 → **无命中**。
   - `docker-entrypoint.sh`、`scripts/`、`server/src/` 检索「首次运行自动生成/打印口令」→ **无此逻辑**；
     唯一命中是 `server/src/lib/reset-code.ts:20` 的**密码重置码**流程 —— 那是写操作，属红线，**未触碰**。
   - 只读探测 `curl http://127.0.0.1:47996/api/auth/status` → `{"needsSetup":false,"authenticated":false,"email":null}`
     —— 说明该实例**已经存在一个账号**（不是我建的），但没有公开凭据。
3. **抓下来的 bundle 与 CSS** —— ✅ 用了（`/tmp/fl_index.css` 的令牌、字号、间距刻度都出自这里）。

**红线遵守**：未尝试猜测/爆破口令；未调用 `/api/auth/setup`（写）；未调用 `/api/auth/login`；
未走 `/forgot-password` 或任何重置流程；未修改 freellm 的任何状态；报告与截图中**不含任何密码**。

---

## 自测（四条硬指标）

Python Playwright 1.61.0 + Chromium 1234，脚本 `/tmp/u2b_verify.py`。原始输出见配方报告 §8。

| 指标 | 桌面 1440×900 | 移动 390×844 |
| --- | --- | --- |
| `pageerror` | **0** | **0** |
| `console.error` | **0** | **0** |
| 外部请求 | **0**（`requests_non_file: []`，总请求 1 = `file://` 文档自身） | **0** |
| 横向溢出 | `scrollWidth 1440 == clientWidth 1440` | `scrollWidth 390 == clientWidth 390`（概览 / 节点池 / 菜单展开三态均通过） |
| 元素宽于视口 | — | `elements_wider_than_viewport: []` |
