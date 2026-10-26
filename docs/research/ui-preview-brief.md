# 前端重构预览 · 统一数据简报（direction-agnostic）

> 所有方向共用同一份真实数据。**渲染必须来自这份数据，禁止任何硬编码数字或补 0。**

## 1. 页面清单（每个方向都要有这 4 页）

| 页面 | 必须回答的问题 | 主要数据 |
| --- | --- | --- |
| 总览 Overview | 现在健康吗？钱/额度花在哪？今天比昨天好还是坏 | `stats-*`、`trends-*`、`agg-*`、`trends-model-7d` |
| 请求日志 Logs | 哪些请求失败了？为什么？慢在哪 | `logs.json`（500 条真实应用日志）、`agg-*` |
| 节点池 Nodes | 哪个节点能用？分数为什么掉 | `nodes.json`（212 个真实节点，IP 已掩码） |
| 设置 Settings | 改什么会影响什么 | `config.json`、`models.json` |

## 2. 数据文件（`data/`，均可 fetch）

```
data/stats-{today,7d,30d,180d}.json   各窗口 token 汇总（by model）
data/trends-{today,7d,30d,180d}.json  各窗口时间序列（日/小时桶）
data/agg-{today,7d,30d,180d}.json     聚合：totals / latency 分位 / by_model / timeseries
data/trends-model-7d.json             按模型分组的时间序列
data/nodes.json                       212 节点 + score/tier/quality/score_reasons
data/logs.json                        {entries:[{seq,time,level,msg,attrs}]} 500 条
data/models.json                      模型目录
data/config.json                      运行配置
data/meta.json                        抓取时间与口径说明
```

`meta.captured_at` 必须显示在页面上（不要把快照时间伪装成实时）。

## 3. 指标口径（不可自行发明）

上游是 OpenAI 协议，`prompt_tokens` **已包含** cache：

```
输入       = Σ prompt_tokens − Σ cache_read_tokens − Σ cache_creation_tokens
缓存读     = Σ cache_read_tokens
输出       = Σ completion_tokens
真实消耗   = Σ (prompt_tokens + completion_tokens)
缓存命中率 = round(Σ cache_read_tokens ÷ Σ prompt_tokens × 100)%
cache_creation_tokens ≡ 0（上游不下发，不要画这一项）
```

热力图单元格 = 当日 `prompt_tokens + completion_tokens`，固定 180 天。

**缺数据 ⇒ 显示「未提供」徽标，绝不写 0、绝不画 0 线。**

## 4. 硬性约束（用户真实返工记录，违反即打回）

1. 图内 **0 句**解释性文字，**无数据标签**
2. 每轴 **≤3 个刻度**；图例 **≤3 项且不带数字**
3. 卡片备注 **≤24 个汉字**
4. 卡片内死空白 **≤10%**
5. 页面级筛选器 **只在页面 header**，不许塞进卡片
6. 控件颜色必须走主题令牌（CSS 变量），**不许硬编码 hex**
7. 正文字号 ≥13px，表格行高 ≥36px，不能靠缩小字号塞内容
8. 缺失/未提供用短徽标表达，不用灰色 0 占位

## 5. 技术约束

- 单文件 HTML（可外部引 `data/*.json` 与 ECharts CDN），无构建步骤
- 必须暗色可读；亮色可选
- 首屏 < 2s（本地），公网经隧道 < 5s
- 直接双击/静态服务器可打开，不依赖后端 API（只读快照）

## 6. 每个方向的交付

```
mockups/opt-X/
  index.html     4 页用客户端路由/tab 切换，URL 可直达 ?page=logs
  (其余资源)
```

顶部或全局必须有一个「← 返回方案列表」入口。

## 7. 预览验收（独立验证者执行）

- 页面在 1440×900 与 390×844 下不横向溢出
- 逐页对照 `data/*.json` 复算 3 个数字（KPI、表格首行、图例值），必须一致
- 扫描源码：无硬编码 token 数字、无 `: 0` 兜底显示、无图内 label
- 图表刻度数、图例项数用运行时 DOM/ECharts option 实测
- 交一份 `verify-report.md`，列出实测值与偏差
