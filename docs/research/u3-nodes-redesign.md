# U3 节点页风格化改造 · 设计方案（task-58）

> 范围：**只出设计方案与高保真预览，不写实现**。
> 产出：`docs/mockups/u3/nodes-v2.html`（3 方向可切 · 夜航/晨雾双主题 · 1440/390/320 三档）+
> 23 张截图（`docs/mockups/u3/nodes-v2-*.png`）+ 本报告。
> 边界：`static/admin.html`、`*.go`、`ROADMAP.md`、`settings-v3.html`、`logs-v2.html` 一律未动；
> `verify.py` 只做追加（新增 `nodes-v2.html` 的 STATES 与 `nv2Probe` 断言，未改任何既有断言）。

---

## 0. 结论速览

**推荐：方向 ② 健康看板做节点页默认视图，方向 ① 表格+抽屉保留为「桌面排查」入口，方向 ③ 卡片流作为窄屏/手机形态。**
三者不是互斥风格，而是同一套数据的不同「排障姿势」：

| | ① 表格 + 详情抽屉 | ② 健康看板为主 | ③ 节点卡片流 |
|---|---|---|---|
| 一句话 | 表格效率派：判死优先排序 + 右侧抽屉看死因/探测史 | 先看「坏在哪」：KPI + 可用率趋势 + 按状态分组折叠 | 一节点一卡：扫一眼、点一下、手机友好 |
| 215 节点默认第一屏 | 50 行表格（判死优先） | **判死 61 组默认展开，可用 154 折叠** | 默认只看有问题的 61 张卡 |
| 找「所有死节点+死因」 | 2 步 / 2.6 屏（1440） | **2 步 / 2.3 屏**（若②为默认视图则 1 步） | 2 步 / 10.1 屏 |
| 固定某节点为出口 | **2 步** | 4 步 | 3 步 |
| 看最近 10 次探测 | **1 步**（抽屉） | 2 步 | 2 步 |
| 看可用率变化 | 0 步（迷你趋势药丸） | 0 步（完整 24h 曲线） | 0 步（迷你趋势药丸） |
| 落地成本（本版可做） | 中（表格重写+分页/搜索+抽屉） | **低-中（分组/KPI/分布先做，趋势待后端）** | 中（卡片 + 复用抽屉） |

关键判断：**IP-3 修复后「大量红的」是真实新常态，节点页的第一屏不应再是「全绿的表格」**。方向 ② 用状态分组把「61 个判死」变成第一屏的主语，是本轮最该先落地的结构；①/③ 是它的下钻与手机形态。

---

## 1. 事实基线（设计依据，来自任务书 + 代码盘查）

1. **节点会被真正判死（IP-3 修复后的新事实）**
   - 生产 10-20 实测：**215 个节点 → 可用 154 / 判死 61**（≈28.4% 判死）。
   - 这不是 bug，是 `proxy_node.go` 的判死逻辑第一次真实生效：连续 2 次探测失败才判死（`probeFailStreakThreshold = 2`，`proxy_node.go:631-633`），修复前「坏 IP 永远 available」。
   - ⇒ 页面如果还按「默认全是绿的」设计，用户看到的 61 个红点会被当成「面板坏了」；必须**默认把问题作为第一屏主语**，并说明「为什么红」。
2. **状态跨重启保留（`loadState()` 生效）**
   - `nodePool.setNodes`（`proxy_node.go:1241`）在装入节点后调用 `restoreStateLocked`（`:1283`），后者读 `/data/proxy_state.json`（`readPersistentState` `:1369`、`restoreStateLocked` `:1397`）把 `state/marked_at/cooldown_until/last_error/gate_state` 合并回来；`saveState`（`:1340`）落盘。
   - ⇒ 打开面板时「一片红」是**持久化的真实状态**，不是本次探测瞬时抖动；页面要展示 `marked_at`（什么时候判的）与 `cooldown_until`（冷却到什么时候）。
3. **三个状态是互斥完备的**：`available(0) / exhausted(1) / dead(2)`（`proxy_node.go:25-42`）；`eligible` 只认 `available`（`:318-320`）；`updated` 统计口径见 `panel-metrics-spec.md:107`（总数 = 可用 + 耗尽 + 判死）。
4. **探测历史/可用率历史目前不存在**：`GET /api/nodes` 每节点只有 `latency_ms / last_probe_at / last_error / marked_at / cooldown_until`（`main.go:7264-7279`），没有逐次探测记录；也没有可用率时间序列（`node_alert` 只有窗口聚合，`node_alert.go:148-164`）。所以预览里的「最近 10 次探测」「24h 可用率曲线」是 **design placeholder**，标了 `data-will-hold`，落地必须后端补（见 §6）。

---

## 2. 现状盘查（不许凭空设计）

### 2.1 页面在哪、长什么样

| 项 | 现状 | 位置 |
|---|---|---|
| 节点页容器 | `section#page-nodes`，单页 SPA 的一页 | `static/admin.html:714` |
| 概览统计 | `ovTotal/ovHealthy/ovExhausted/ovDead/ovActive/ovManual` 六格 | `static/admin.html:1280` `renderOverview` |
| 状态筛选 | `#stateChips`：全部/可用/已耗尽/故障 + `.cnt` 计数 | `:715-720`，`setFilter` `:1857` |
| 操作区 | 重新加载订阅 / 解除全部标记 / 测速 / 刷新节点 + `#nodeStatus` | `:721-727` |
| 延迟分布 | `#latdist`，**纯 CSS 分段条**（<100/<200/<400/≥400），不是 ECharts | `:728`，`renderLatencyDist` `:1411` |
| 节点表 | `#nodeTable` 7 列：名称/协议/状态/延迟/冷却至/最近错误/操作 | `:730-736`，`renderNodeTable` `:1860` |
| 自动刷新 | 只有概览页会 `setInterval`，默认关；节点页只有 30s 的 `node_alert` 横幅轮询 | `setAutoRefresh` `:976`、`refreshNodeAlertOnce` `:1320`、`startNodeAlertWatch` `:1327` |
| 危险操作确认 | 只有 `resetAllMarks` 用原生 `confirm('解除所有节点的耗尽/故障标记？')` | `:1879` |
| 行内切换/解除 | `nodeAction` POST `/api/nodes`（switch/reload/probe/reset/reset_all/save） | `nodeAction` `:1872`，接口 `main.go:7281` |

**现状的 6 个结构性问题**（这轮要解的）：

1. **默认全绿假设**：没有「只看有问题的」；61 个判死散在 215 行里，靠肉眼扫。
2. **没有搜索/排序/分页/虚拟滚动**：215 行一次 `innerHTML` 重建整表；「找某个节点」只能浏览器 Ctrl+F 或翻。
3. **没有详情**：一处死因只有「最近错误」一列，`last_error` 长了被 CSS 截断；看不到 `marked_at`、`fingerprint`、`address:port`、冷却剩余、探测历史。
4. **没有可用率/趋势**：只有当前计数，「是不是正在变坏」看不出来。
5. **批量操作挤在右上角且危险操作只有原生 confirm**：`reset_all` 无数量提示、无二次确认样式。
6. **轮询会重建表格**：`refreshNodeAlertOnce` 只更横幅是刻意的（注释说避免重建表格/filter/scroll），但这也意味着节点页没有「安全刷新」机制；一旦加自动刷新，必须解决「不打断筛选/详情/滚动」。

### 2.2 `/api/nodes` 字段与页面展示

GET 响应（`main.go:7281` `adminNodesHandler`）：

```
顶层：active_fp active_name manual_fp manual_name healthy exhausted_count dead_count
      nodes[] subscriptions[] node_alert{}
node：name protocol address port fingerprint state marked_at cooldown_until
      last_error last_used_at latency_ms last_probe_at active manual
```

| 字段 | 现状页面 | 本设计里怎么用 |
|---|---|---|
| `name` | 表格「名称」列（长名被截断） | 拆 `订阅名::节点名` → 主名 + 来源标签；见 §5.4 |
| `protocol` | 有（小标签） | 保留 |
| `address` / `port` | **不展示** | 副行 `address:port`（等宽）+ 详情；搜索可命中 |
| `fingerprint` | **不展示** | 详情展示短指纹/复制；作为「同一节点」的稳定身份（diff/刷新/选择都用它） |
| `state` | 徽章 available/exhausted/dead/idle | 三态徽章：颜色 + 形状 + 图形 + 文字 + 底纹，见 §5.5 |
| `latency_ms` | 有（`-1` → 超时） | 保留；死节点显示「超时」 |
| `last_probe_at` | 「冷却至」列旁没有；只在部分处用 `fmtTime` | 表格加「最近探测」（几分钟前） |
| `marked_at` | **不展示** | 详情「标记时间」；判死排序/趋势定位 |
| `cooldown_until` | 「冷却至」列 | 详情展示剩余时间；死节点冷却只是展示，不等于会复活 |
| `last_error` | 「最近错误」列（截断） | 表格截断 + 详情「错误原文」等宽块；T1 的主信息 |
| `last_used_at` | 不展示 | 详情「最近使用」 |
| `active` / `manual` | 行内「当前/手动」tag | 卡片/表格 tag；固定动作反馈 |
| `healthy` / `exhausted_count` / `dead_count` | 概览六格 | KPI 直接用（154/61/0） |
| `node_alert{}` | 顶部横幅 | 详情/看板可做「近 30 分钟成功率」真数据（窗口聚合） |
| `subscriptions[]` | 独立订阅表 | 方向 ② 分组可按订阅做二级折叠（后续） |

### 2.3 状态机（设计必须说清「为什么红」）

```
                    连续 2 次探测失败（probeFailStreakThreshold=2, :631-633）
   available ────────────────────────────────────────────────► dead
       ▲                                                          │
       │  recordProbeSuccess (:1145)  探测成功                     │
       └──────────────────────────────────────────────────────────┘
       │
       │ 免费层配额拒绝（markFreeTierRejected :616 / gate=rejected）
       ▼
   exhausted ── sweepExpiredLocked (:326) 冷却到期（默认 1h）──► available
```

| 转移 | 触发 | 结果/细节 | 代码 |
|---|---|---|---|
| available → dead | 连续 2 次探测失败 | `markLocked`（`:506`）写 state/marked_at/last_error，`LatencyMs=-1`，dead 冷却 1m（默认），清空 active 连接，落盘 | `markProbeDead` `:1096` → `markLocked` `:506`、`saveState` `:1340` |
| dead → available | **探测成功** | `recordProbeSuccess`（`:1145`）清死标记；**dead 不会因为时间到了自动复活** | `:1145` |
| available → exhausted | 免费层配额耗尽 / 上游按 IP 拒绝 | gate_state=rejected，冷却 1h（默认） | `markFreeTierRejected` `:616`、`:618` |
| exhausted → available | 冷却到期 | `sweepExpiredLocked`（`:326`）翻回 available；若 gate 要求探针成功（C3）则等探测成功 | `:326-346` |
| 手动固定 | `/api/nodes` action=switch / `manual` | 手动优先于轮询，但只有 available 节点可固定 | `manual` `:447`、`pick` `:379-441` |
| 自动选择 | `pick` 无可用节点 | 返回 nil = 直连（不是随便挑一个坏的） | `pick` `:379` |

设计约束：**详情里要显示「这个节点怎么才能恢复」**——dead 写「复探成功才恢复，冷却时间不复活」，exhausted 写「冷却到期自动恢复；被上游按 IP 拒绝的需探针成功」。这正是排障用户最想知道的。

### 2.4 现有其他事实（卡片/看板数据来源）

- 配置默认值：`node_cooldown_exhausted_hours=1`、`node_cooldown_dead_minutes=1`、`node_health_interval_minutes=15`（`main.go:1058-1065`）；UI 字段 `q_cooldown_h/q_cooldown_m/q_health_interval`。
- `node_alert` 快照字段（`node_alert.go:148-164`）：`active/reason/since/window_minutes/window_attempts/window_successes/window_success_rate(*float64, null=无数据)/run_failures/run_minutes/pool_available/pool_total` —— 方向 ② 的「近 N 分钟成功率」可以直接用，不用等历史表。
- 节点名双口径：订阅节点 `Name` 常见 `订阅名::节点名`，手填节点是自由名；前端只显示 `Name` 原样，导致同一节点在日志/订阅/节点页出现 `机场A::HK-03` 与 `HK-03` 两种叫法。

---

## 3. 设计目标与硬约束

来自 task-58 + U3 既有约束（`docs/mockups/u3/README.md`、`static/admin.html` 的令牌）：

1. **U3 风格 + 夜航/晨雾双主题**：令牌与最终 `admin.html` 同源（亮 `--text-ter:#5c6b80` / 暗 `--text-ter:#7b8da8`，小号蓝字 `--brand-ink`），首绘前 head 脚本定主题、无闪白。
2. **图表复用**：落地时走 `reuseChart()` / `applyChartTheme()`（预览用内联 SVG，零外部请求；不用 jsdelivr）。
3. **三态不只靠颜色**：颜色 + 形状（圆/方/菱形）+ 图形 icon（✓/×/时钟）+ 文字（可用/判死/配额冷却）+ 底纹（判死斜纹/冷却斜纹）。
4. **主线任务是排障**：哪个节点死了、为什么（原始错误）、什么时候、还剩多少可用、可用率在变好还是变坏。
5. **215 个节点不吓人**：摘要 + 分组折叠 + 分页/窗口化 + 搜索筛选 + 移动端替代形态。
6. **移动端 390/320 不横向铺宽表**：表格在窄屏转两行卡片；抽屉变全屏 sheet；批量条底部浮出。
7. **未来占位**：ROADMAP §2b 打分/固定策略、节点成功率/耗时排名——只读占位，明确「未上线」。
8. **自动刷新不打断**：有筛选/搜索/抽屉时暂停重绘，出现「有 N 条更新 · 应用」药丸；按 `fingerprint` 对账，不重建 DOM、不丢滚动与选择。
9. **危险操作二次确认**：重新加载订阅、清除全部死标记、批量复测都走自绘 modal（显示影响数量），不用原生 `confirm`。

---

## 4. 三个方向（结构不同，不是换皮）

预览文件：`docs/mockups/u3/nodes-v2.html`（顶栏「方向 ①②③」切换；`[data-d]` + `aria-pressed`；同时只显示一个 `.dir`）。
样例数据：215 = **154 可用 / 58 判死 / 3 配额冷却**；生产实测 154/61/0（多放 3 个冷却只为展示第三态，页脚有说明）。

### 方向 ① 表格 + 详情抽屉（桌面效率派）

- **结构**：工具栏（搜索 / 状态 chips / 自动刷新 / 迷你趋势药丸）→ 批量条（全选本页、固定、清除、危险操作）→ 7+1 列表格（判死优先排序，每页 50）→ 分页器；点行/「详情」开右侧 520px 抽屉。
- **抽屉内容**：状态 + 恢复条件说明、基础信息（协议/address:port/完整 fingerprint/延迟/最近探测/标记时间/冷却剩余）、**最近 10 次探测**（时间/成功失败/耗时/错误，标 `data-will-hold`）、错误原文等宽块、出口选择规则；底部动作（固定/复测/清除/关闭）。
- **关键取舍**：表格保留「扫 50 行」的效率；错误列截断用 `title` + 抽屉看全文；分页而不是无限滚动（排障需要「我在第几页、还剩多少」）。

### 方向 ② 健康看板为主（推荐默认）

- **结构**：4 张 KPI（总数 / 可用 / 判死 / 配额冷却，带可用率）→ 24h 可用率曲线（`data-will-hold="nv2Trend"`）+ 状态构成条 + 「只看有问题的」开关 → 状态分组：**判死 58 / 配额冷却 3 / 可用 154**，坏的两组默认展开、可用组默认折叠；每组头有数量 + 一句「为什么/怎么恢复」。
- **默认第一屏**：判死组展开前 8 行 + 「展开全部 58 行」；KPI 直接告诉用户「215 里 61 个坏的」。
- **比 ① 强的地方**：默认视图已经把「问题」当主语；折叠可用组 = 一次点击关掉 154 行噪音；趋势/构成给「在变坏还是恢复」的判断。

### 方向 ③ 卡片流（手机/扫一眼）

- **结构**：搜索 + chips + 「只看有问题的」+ 排序 → 320/390 单列、1440 三列的卡片；一卡 = 状态色带（左）+ 徽章（图标+形状+文字）+ 节点名 + 来源 + address:port + 延迟/最近探测/协议 + 错误摘要 + 固定/详情；勾选后底部浮出批量条（固定所选/清除标记/取消/危险清除全部）。
- **移动端**：天然单列，无横向滚动；批量条固定底部；详情复用同一抽屉（窄屏全宽）。
- **弱点**：58 张死卡 = 10.1 屏（1440）/10.9 屏（390），批量排查不如 ②；适合「扫一眼/手机现场处理/单节点操作」。

### 4.1 四任务实测：步数 & 滚动屏数

定义：**步** = 一次点击/输入/翻页（不含「看」）；**屏** = 需要滚动的内容高度 ÷ 视口高（1440×900 / 390×844）。测量从节点页默认视图（方向 ①）开始，**切到②或③各计 1 步**；若把 ② 定为默认视图，它各任务减 1 步。数据由 Playwright 实测布局高度与交互序列得出（可复跑）。

**1440 桌面：**

| 任务 | ① 表格+抽屉 | ② 看板 | ③ 卡片流 |
|---|---|---|---|
| T1 找出所有判死节点及死因 | 2 步 / 2.6 屏（点「判死」+ 翻到第 2 页） | **2 步 / 2.3 屏**（切看板+展开判死组；默认视图则 1 步） | 2 步 / 10.1 屏（切卡片+点判死 chip + 滚 58 卡） |
| T2 固定某节点为出口 | **2 步** / 0 屏（可用 chip + 固定） | 4 步 / 0 屏（切看板+取消只看坏+展开可用+固定） | 3 步 / 0 屏（切卡片+取消只看坏+固定） |
| T3 看某节点最近 10 次探测 | **1 步** / 0 屏（抽屉内 0.1 屏） | 2 步 / 0 屏（切看板+详情） | 2 步 / 0 屏（切卡片+详情） |
| T4 看可用率变化 | 0 步（工具栏迷你趋势）/ 1 步看完整曲线 | 1 步（切看板）/ 0 步若默认即② | 1 步（切卡片）/ 0 步若默认即③ |

**390 手机：**

| 任务 | ① 表格+抽屉 | ② 看板 | ③ 卡片流 |
|---|---|---|---|
| T1 | 2 步 / **17 屏**（表格转卡片后 50 行很长） | **2 步 / 7 屏**（默认视图则 1 步） | 2 步 / **10.9 屏** |
| T2 | 2 步 / 0 屏 | 4 步 / 0 屏 | 3 步 / 0 屏 |
| T3 | 1 步 / 0.1 屏 | 2 步 / 0.1 屏 | 2 步 / 0.1 屏 |
| T4 | 0 步 | 1 步（默认视图 0 步） | 1 步（默认视图 0 步） |

结论：**T1（批量排障）方向 ② 的滚动成本最低（桌面 2.3 屏、手机 7 屏），① 桌面次之、手机最差（17 屏），③ 视觉最友好但滚动最多（10+ 屏）**；T2/T3 ① 的步数最少（表格直出动作），③ 次之，② 因分组要先展开可用组而多步。若把 ② 定为默认视图（省掉切方向那 1 步），它在 T1/T4 上同时最优——这就是推荐「② 默认 + ① 桌面下钻 + ③ 手机形态」的量化依据。

### 4.2 可行性：本版可做 vs 需后端补

| 能力 | ① | ② | ③ | 说明 |
|---|---|---|---|---|
| 判死优先排序 / 状态筛选 / 计数 | ✅ 本版可做 | ✅ | ✅ | 现有字段够 |
| 每页 50 分页 | ✅ 前端切片 | — | — | 215 数组，无需后端 |
| 搜索 name/address/fingerprint | ✅ 本版可做 | 可选 | ✅ | 前端过滤 |
| 详情抽屉（当前字段） | ✅ | ✅（复用） | ✅（复用） | marked_at/cooldown/last_error 都有 |
| 探测历史（最近 10 次） | ⚠️ 需后端 | ⚠️ 需后端 | ⚠️ 需后端 | 现在只存「最后一次」；见 §6 |
| 24h 可用率趋势 | ⚠️ 需后端 | ⚠️ 需后端 | ⚠️ 需后端 | 无时间序列；可先用 `node_alert` 窗口成功率做「近 30 分钟」真数据 |
| 状态构成条 | ✅ | ✅ | — | 前端算 |
| 批量固定/清除/复测 | ✅ | ✅ | ✅ | 现有 POST action 够 |
| 危险操作自绘二次确认 | ✅ | ✅ | ✅ | 纯前端 |
| 自动刷新不打断 | ✅ | ✅ | ✅ | 需前端 reconcile（按 fingerprint） |
| 移动端卡片化 | ✅（窄屏转卡） | ✅（折叠组） | ✅ | CSS 媒体查询 |

---

## 5. 必答五问

### 5.1 215 个节点怎么不吓人？

四层，不靠「一页塞 215 行」：

1. **摘要先说话**：方向 ② 的 4 张 KPI（215/154/61/0 + 可用率 71.6%）让用户 0 步知道「坏 61 个」；方向 ①/③ 工具栏有 `/ 215` 与「第 x/y 页」「显示 61 / 215」。
2. **默认只看问题**：② 判死/冷却组默认展开、可用组折叠；③ 默认「只看有问题的」；① 默认判死优先排序（第一页就是死的）。
3. **折叠/分页/窗口化**：② 每组「展开全部 N 行」按需加载；① 每页 50 + 搜索/chips；③ 卡片按需「显示更多」（实现建议每批 30，虚拟滚动可选）。
4. **拒绝垃圾铺满**：不把 154 个可用节点画成 154 张同质卡；可用组默认收起，搜索/导出再展开。

### 5.2 批量操作放哪？哪些要二次确认？

- **①**：表格上方 sticky bulkbar（全选本页/固定/清除）+ 危险操作（清除全部死标记 / 重新加载订阅 / 批量复测）同区但用 `btn-danger` 分隔。
- **②**：看板 KPI 下方的操作条（清除全部死标记 / 重新加载订阅）；分组头可「只看这组」。
- **③**：默认不占位；勾选后底部浮出 sticky 批量条（移动端不遮挡内容，`bottom:10px`）。
- **二次确认**（自绘 `#modal`，不是原生 `confirm`）：
  - `reset_all` 显示「将把 **61** 个判死节点改回可用并清空 gate 状态；下次探测仍失败会再次判死」；
  - `reload` 显示「重建 **215** 个节点池，当前出口/固定可能变化」；
  - `probe_all` 显示「对 **215** 个节点发起真实探测（约 215 次上游请求，数分钟），失败节点可能被判死」。
- 断言：`verify.py` 的 `danger` 态检查 `#modal.on` + 取消不执行；预览里确认后只 toast「已提交（预览不执行）」。

### 5.3 可用率趋势图怎么做？数据源？

- **现状：没有历史**。每节点只有 `last_probe_at/latency_ms/last_error`，`node_alert` 是窗口聚合（`node_alert.go:148-164`），所以 24h 曲线**不能假装有**。
- **预览处理**：方向 ② 画 24h 曲线（25 个点：205 → 14:0x 判死后 154），但标 `data-will-hold="nv2Trend"`「需后端补历史」；方向 ①/③ 工具栏用**迷你趋势药丸**（72×22 SVG + 「71.6% ↓」，点开 ② 看完整曲线），同样标占位。
- **落地数据源建议**（按成本排序）：
  1. **先上「近 30 分钟成功率」真数据**：直接用 `node_alert.window_success_rate/window_attempts`（已有），不依赖历史表 —— 能回答「现在是不是在恶化」。
  2. **再加采样历史**：后端每 5 分钟把 `{t, available, dead, exhausted, total}` 追加到 `/data/node_history.jsonl`（或复用 `proxy_state.json` 旁边），`GET /api/nodes/history?hours=24` 返回数组；前端用 `reuseChart()` 画面积图 + 判死阶跃。
  3. 若不想加文件：在 `node_alert` 里维护最近 N 个窗口快照（环形数组，`node_alert.history[]`），内存级、重启丢，但成本最低。
- 验收：曲线必须有真实数据源才能上线；预览中的曲线明确标注为样例。

### 5.4 节点名的两种口径怎么统一？

- 现状：`name` 订阅节点常为 `订阅名::节点名`，手填节点是自由名；页面只显示 `name`，于是同一节点在不同页面有 `机场A::HK-03` 与 `HK-03` 两种叫法；`address:port` 完全不显示。
- **设计决策**（①/③ 已实现，② 沿用）：
  - **主名** = 去前缀的短名（`HK-03`），旁边用灰 tag 标来源（`机场A`），手填节点标「手动」；hover `title` 显示完整 `name`。
  - **副行** = 等宽 `address:port`（如 `hk-03.edge.example.net:443`）；详情再给完整 `fingerprint`（等宽 + 可复制）。
  - **搜索**同时匹配 主名 / 完整 name / address:port / fingerprint 前 8 位。
  - **身份键**统一用 `fingerprint`（稳定的出口身份），不用 name（会变/会重复）。
- 效果：同一节点在「订阅/日志/节点页」都能对上 `address:port`，`HK-03` 这种短名只做展示。

### 5.5 自动刷新不打断 + 三态不只靠颜色

**刷新策略（预览已做可交互演示）**：
- 轮询只在「无交互」时静默更新数据 + KPI/计数；**抽屉打开、筛选非「全部」、搜索框有内容**时暂停重绘，只把变更数累计成药丸「有 3 条更新 · 应用」；点击药丸才应用。
- 应用时按 `fingerprint` 对账（相同 fingerprint 原地改行/卡片，新节点插入、消失节点移除），保留滚动位置、当前筛选、选择集、抽屉内容；隐藏 tab 不轮询（沿用 `refreshNodeAlertOnce` 的 visible 判断，`static/admin.html:1320`）。
- 工具栏显示「自动刷新 30s / 手动刷新 / 自动刷新暂停（详情/筛选）」。
- 断言：`verify.py` 的 `refresh` 态 = 打开抽屉 + 触发刷新 → 药丸 on、抽屉仍 on。

**三态冗余编码（≥3 重，不止颜色）**：

| 状态 | 颜色 | 形状 | 图形 icon | 文字 | 底纹 |
|---|---|---|---|---|---|
| 可用 available | 绿 `--ok` | 圆点 (`border-radius:50%`) | ✓ 对勾 | 可用 | 无 |
| 判死 dead | 红 `--danger` | 方形 (`border-radius:2px`) | × 叉 | 判死 | 斜纹（`repeating-linear-gradient`） |
| 配额冷却 exhausted | 琥珀 `--warn` | 菱形（旋转 45°） | 时钟 | 配额冷却 | 斜纹 |

趋势图里判死用「阶跃/虚线」区分；表格行判死加浅红底 + 左侧色条。`verify.py` 断言：每个 `.st` 都有 `svg` + `.st-shape` + `aria/文字` + `data-glyph`，且三状态都出现（`badge_states == ["available","dead","exhausted"]`）。

---

## 6. 推荐与落地拆分

### 6.1 推荐

**节点页 = 方向 ② 默认（健康看板）+ ① 作为「表格视图」下钻 + ③ 作为 ≤760px 的窄屏形态**。

理由：
- 生产新事实下，用户打开节点页的第一个问题是「有多少坏的、为什么」——② 的 KPI + 判死组默认展开正好回答，且量化上 T1 最优（1 步/2.3 屏）。
- ① 保留表格的批量效率与「固定/切换」直出动作（T2 2 步），是排障熟手的下钻视图。
- ③ 解决手机端「17 屏」问题（表格转卡片仍 10.9 屏，但单卡操作/扫读更自然），且复用了抽屉与状态编码。
- 三方向共用：状态编码、抽屉内容、危险确认组件、刷新 reconcile、双主题令牌——实现不是三套，而是一套组件 + 三种排布。

### 6.2 落地拆分（本版可做 → 需后端）

**P0（纯前端，现有 `/api/nodes` 字段即可）**
1. 状态分组/KPI/状态构成条（②）+ 判死组默认展开；
2. 判死优先排序 + 搜索（name/address/fingerprint 前 8）+ 每页 50 分页（①）；
3. 详情抽屉（现有字段：marked_at/cooldown_until/last_error/latency_ms/last_probe_at/active/manual）；
4. 批量操作 + 自绘二次确认（reset_all/reload/probe_all 显示数量）；
5. 窄屏卡片形态（③ 或 ① 的卡片化）；
6. 自动刷新「暂停 + 药丸 + fingerprint 对账」；
7. 迷你趋势药丸（显示当前可用率，曲线标占位）。

**P1（需后端小改）**
8. `/api/nodes/history?hours=24`（采样落盘）→ 24h 可用率真曲线；
9. 每节点 `probe_history`（环形 10-50 条，含 at/ok/latency/error）→ 详情「最近 10 次探测」；建议单独 `GET /api/nodes/probe-history?fp=` 按需拉，避免 `/api/nodes` 响应膨胀；
10. `node_alert` 暴露「近 30 分钟成功率」到卡片/详情（现在有 `window_success_rate`，前端展示即可，属于 P0/P1 边界）。

**P2（ROADMAP §2b，先占位）**
11. 节点打分权重（延迟/成功率/区域）、自动选择/固定策略可视化；
12. 节点成功率 / 耗时排名（需要按节点滚动统计，后端加聚合）。

### 6.3 需要后端配合的字段/接口草案

```jsonc
// GET /api/nodes/history?hours=24
{ "points": [ { "t": "2026-10-20T08:00:00+08:00",
                "available": 205, "dead": 10, "exhausted": 0, "total": 215 } ] }

// GET /api/nodes/probe-history?fp=ab12cd34&limit=10   （只拉详情节点，避免全量膨胀）
{ "fingerprint": "ab12cd34", "probes": [
  { "at": "2026-10-20T15:02:11+08:00", "ok": false,
    "latency_ms": -1, "error": "dial tcp 103.71.24.18:443: i/o timeout" } ] }
```

存储建议：`probe_history` 写 `/data/proxy_state.json` 的每节点字段（需兼容旧版：缺字段=空数组）；`history` 用独立 JSONL/滚动桶，注意 215 节点 × 10 条的体积（约几十 KB，可接受）。

---

## 7. 自测与证据

- **预览**：`docs/mockups/u3/nodes-v2.html`，1396 行 / 84 KB（md5 `2fdad982b284bbdb8b1055f319f26508`），单文件零外部请求（无 CDN、无图片、favicon 为 data URI；`verify.py` 断言 `requests_non_file_count === 0`）。
- **`verify.py`（追加，未改既有断言）**：新增 `nodes-v2.html` 的 14 个状态（`dark/d2/d2dark/d3/d3dark/drawer/drawerDark/danger/dangerDark/refresh/boardAll/boardAllDark/select3/select3Dark`），每个状态跑 1440/390/320 三档，`nv2Probe` 断言：
  - 3 方向可切、同一时刻只显示 1 个；状态徽章三重编码（svg + 形状 + 文字 + glyph）且三态齐全；
  - 默认判死优先、分页 `/ 215`、每页 ≤50；看板默认只展开坏的两组、`data-will-hold` 存在；
  - 抽屉 on + ≥10 条探测；危险操作 modal on；刷新药丸 on 且抽屉仍在；卡片选择态批量条 on；
  - 预留字段 3 个且只读（无 input/select/button）；三视口无溢出、无 pageerror/console.error/非 file 请求、CSS 生效。
- **结果**：`nodes-v2.html` **45/45 状态 PASS**；整套 `python3 docs/mockups/u3/verify.py` **206/206 PASS**（含既有 8 个页面，无回归）。
- **截图**：23 张（3 方向 × 2 主题 × 3 视口 = 18 + 抽屉/危险确认/选择态/看板全量/手机抽屉 = 5）。命名与索引见下。
- **边界自检**：`static/admin.html` md5 仍为 `4fe4f101470f9284ea7d1edbbc04d7e5`；`settings-v3.html`/`logs-v2.html`/Go/ROADMAP 未改；预览为独立文件，无 git 操作、无容器、未碰 8000 端口。

### 7.1 截图索引（`docs/mockups/u3/`）

| 文件 | 内容 |
|---|---|
| `nodes-v2-d1-1440-light/dark.png` | ① 表格（1440，晨雾/夜航） |
| `nodes-v2-d1-390-light/dark.png` | ① 窄屏转卡片（390） |
| `nodes-v2-d1-320-light/dark.png` | ① 320 最小档 |
| `nodes-v2-d2-1440-light/dark.png` | ② 看板（KPI + 趋势 + 分组，1440） |
| `nodes-v2-d2-390-light/dark.png` | ② 看板 390（KPI 2×2、趋势、折叠组） |
| `nodes-v2-d2-320-light/dark.png` | ② 320 |
| `nodes-v2-d3-1440-light/dark.png` | ③ 卡片流（1440） |
| `nodes-v2-d3-390-light/dark.png` | ③ 390 单列 |
| `nodes-v2-d3-320-light/dark.png` | ③ 320 |
| `nodes-v2-d1-drawer-1440-dark.png` | ① 详情抽屉（错误原文 + 10 次探测 + 恢复条件） |
| `nodes-v2-d1-drawer-390-light.png` | 手机全宽抽屉 |
| `nodes-v2-danger-1440-dark.png` | 「清除全部死标记」二次确认（显示 58 个、说明后果） |
| `nodes-v2-d3-select-1440-dark.png` | ③ 勾选后底部批量条 |
| `nodes-v2-d2-all-1440-dark.png` | ② 取消「只看有问题的」→ 三组都在（可用 154 折叠） |

渲染说明：本机 chromium-1234 在 **headless** 下对本页有大表格 raster 伪影（y≈600px 以下只画首列；DOM/rect 全正常，headed 正常），因此**证据截图用 xvfb + headed Chromium 生成**；`verify.py` 的 STATES 断言仍用 headless 跑（不受影响）。

---

## 8. 风险与已知限制

1. **样例数据**：154/58/3 是样例，生产是 154/61/0（页脚已注明）；冷却态数量只为展示第三态。
2. **趋势/探测历史是占位**：`data-will-hold` 明确标注；没有后端历史前不得把曲线当真实数据上线。
3. **215 行性能**：① 每页 50 无压力；若要「无限表 / 搜索即显」建议后续加窗口化渲染（`IntersectionObserver` 或虚拟列表），本版先分页。
4. **状态实时性**：自动刷新默认关；打开后 30s 轮询，需按 §5.5 的 reconcile 实现，否则会重演「刷新把筛选/滚动冲掉」。
5. **前端依赖**：真实面板落地需在 `static/admin.html` 内实现（内联 JS + 现有 `api()`/`toast()`/`fmtTime()` 等工具），本预览的组件结构与 CSS 可直接迁；ECharts 图表复用 `reuseChart()/applyChartTheme()`。

---

## 9. 给 Lead 的决策点

1. **方向选择**：是否按「② 默认 + ① 表格视图 + ③ 窄屏形态」推进？还是只选一个方向实现？
2. **后端排期**：24h 趋势 + 探测历史是否本轮一起做（P1）？若否，先上 `node_alert` 的近 30 分钟成功率作为真实指标。
3. **实现范围**：确认后我按 `verification-before-completion` 走实现（`static/admin.html` 内联，不新增构建步骤），并用既有 113 条断言 + `verify.py` 回归。

---

*报告：task-58 / 2026-10-20 / 未 commit；预览与截图均在 `docs/mockups/u3/`，本报告在 `docs/research/`。*
