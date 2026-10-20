#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
U3 自测脚本 —— 对 docs/mockups/u3/*.html 做硬要求验收，并把原始 JSON 打出来。

查五件事：
  1. 1440x900 / 390x844 / 320x568 三档 documentElement.scrollWidth === clientWidth
     （脚本**不**注入 overflow-x:hidden，且断言 computed overflow-x 不是 hidden）
  2. pageerror 数量 = 0
  3. console.error 数量 = 0
  4. 非 file:// 请求数量 = 0（零外部请求）
  5. css_live —— 样式表真的生效（血泪守卫：head 注释漏 --> 会把 <style> 整块吞掉，
     一个完全没 CSS 的白页**也能通过**宽度检查，见 README 的坑记录）

另外量：比视口宽的可见元素清单。在 overflow-x:auto 容器内的**不算** ——
表格卡内滚动是刻意的、与原面板一致，页面本身没被撑开。

状态矩阵：某些页面要测「切主题 / 切视图 / 展开菜单 / 切 tab」之后的状态，
见 STATES。每个状态都重新加载页面再操作，保证互不污染。

用法：
    python3 verify.py                      # 全部 html
    python3 verify.py overview-v2.html     # 单个
    python3 verify.py --no-shot            # 只量不截图

注意：本机默认的 chromium-1228 不存在，必须显式给 executable_path 指向 1234。
"""
import json
import os
import pathlib
import sys

from playwright.sync_api import sync_playwright

HERE = pathlib.Path(__file__).resolve().parent
CHROME = "/home/jingyx/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome"

VIEWPORTS = [
    ("1440x900", 1440, 900),
    ("390x844", 390, 844),
    ("320x568", 320, 568),
]

# 需要额外测的状态：state 名 -> (载入后要执行的点击序列, 只在哪些视口测 / None=全部)
# 「menu」只在窄屏有意义：#mobMenu 在 ≥768px 是 display:none，宽屏点它会超时（不是 bug）。
STATES = {
    "settings-v2.html": {
        # 方向② 分区页（用户 2026-10-20 拍板）：连接 / 自动切换 / 日志与维护 + 模型页单开。
        # 方向①/③ 已删，没有任何 dir1/dir3 tab。
        "conn": ([("#tabConn", None)], None),
        "route": ([("#tabRoute", None)], None),
        "maint": ([("#tabMaint", None)], None),
        "model": ([("#tabModel", None)], None),
        "dark": ([("#themeBtn", None)], None),
        "routedark": ([("#themeBtn", None), ("#tabRoute", None)], None),
        "modeldark": ([("#themeBtn", None), ("#tabModel", None)], None),
        # 未上线卡：默认折叠；展开后控件全部 disabled、点了没反应
        "future": ([("#tabRoute", None), ("js:document.getElementById('card-future').open=true", None)], None),
        "futuremodel": ([("#tabModel", None), ("js:document.getElementById('m-card-route').open=true", None)], None),
        # 即时生效 + 撤销条：改一个 seg 后底部出现撤销条（切页后仍在）
        # 先点卡头展开（窄屏卡片默认折叠，这是设计行为，不是作弊），再改分段控件
        # 先点卡头展开（窄屏折叠态才点；宽屏点它会反向关掉，所以用条件表达式）
        "undo": ([("#tabRoute", None),
                  ("js:document.getElementById('card-switch').classList.contains('mcollapsed')&&document.querySelector('#card-switch > .card-hd').click()", None),
                  ('#card-switch .seg button[data-v="10"]', None)], None),
        # 移动端折叠：点开第二张卡（窄屏才折叠；宽屏本来全展开，点击无副作用）
        "acc": ([("js:document.querySelectorAll('.card.mcollapsed > .card-hd')[0] && document.querySelectorAll('.card.mcollapsed > .card-hd')[0].click()", None)], None),
    },
    "settings-v3.html": {
        # 三个方案（task-54）：① 单页卡片流 ② 单页+分组折叠 ③ 双栏工作台。
        # 每态都断言：只有 1 个方案可见、8 个可编辑控件、只读项带「改请改配置文件」、
        # 出口组与被删字段 0 命中、无溢出。
        "light": ([("#themeBtn", None)], None),
        "d2": ([('#dirs button[data-d="2"]', None)], None),
        "d2light": ([('#dirs button[data-d="2"]', None), ("#themeBtn", None)], None),
        "d3": ([('#dirs button[data-d="3"]', None)], None),
        "d3light": ([('#dirs button[data-d="3"]', None), ("#themeBtn", None)], None),
        # 窄屏 #wsnav 是 display:none（手机用顶部横向分组条）⇒ 走预览钩子切组，两个断点都能测
        "d3g3": ([('#dirs button[data-d="3"]', None), ("js:window.__setPreview.group(3)", None)], None),
        "acc": ([('#dirs button[data-d="2"]', None),
                 ("js:document.querySelector('#g2-3 > .acc-hd').click()", None)], None),
        "ro": ([("js:document.getElementById('roDetails').open = true", None)], None),
        "undo": ([("js:window.__setPreview.edit('cooldown_exhausted', 3)", None)], None),
        "danger": ([("js:document.querySelector('#g1-5 .dgr button').click()", None)], None),
    },
    "logs-v2.html": {
        # task-51（扩范围）：一页两个子视图「分析 / 明细」，3 个方向各有一套。
        # 每态断言：只有 1 方向可见；子视图只有 1 个可见；分析侧 KPI 6 张 + 24 根小时柱 +
        # ≥12 行分布 + 路由预留 3 字段；明细侧徽章「符号+文字」双编码、未映射/闸门/超时都有文字、
        # 统计数带 data-will-hold；点分析里的模型/小时/错误 → 必须落到明细且筛选生效（联动）。
        "light": ([("#themeBtn", None)], None),
        # ---- 分析子视图（默认）----
        "d2an": ([('#dbtn2', None)], None),
        "d2anlight": ([('#dbtn2', None), ("#themeBtn", None)], None),
        "d3an": ([('#dbtn3', None)], None),
        "d3anlight": ([('#dbtn3', None), ("#themeBtn", None)], None),
        # ---- 明细子视图 ----
        "dt": ([('#subsw button[data-sub="dt"]', None)], None),
        "dtlight": ([('#subsw button[data-sub="dt"]', None), ("#themeBtn", None)], None),
        "d2dt": ([('#dbtn2', None), ('#subsw button[data-sub="dt"]', None)], None),
        "d2dtlight": ([('#dbtn2', None), ('#subsw button[data-sub="dt"]', None), ("#themeBtn", None)], None),
        "d3dt": ([('#dbtn3', None), ('#subsw button[data-sub="dt"]', None)], None),
        "d3dtlight": ([('#dbtn3', None), ('#subsw button[data-sub="dt"]', None), ("#themeBtn", None)], None),
        # 抽屉 / 筛选 / 实时（都在明细侧）
        "drawer": ([('#dbtn2', None), ('#subsw button[data-sub="dt"]', None),
                    ("js:document.querySelector('#tblBody tr[data-id]').click()", None)], None),
        "unmapped": ([('#subsw button[data-sub="dt"]', None),
                      ("js:window.__logsPreview.filter('status', 'unmapped')", None)], None),
        "today": ([('#subsw button[data-sub="dt"]', None),
                   ("js:window.__logsPreview.filter('range', 'today')", None)], None),
        "slow": ([('#subsw button[data-sub="dt"]', None),
                  ("js:window.__logsPreview.filter('dur', '60')", None)], None),
        "incoming": ([('#subsw button[data-sub="dt"]', None),
                      ("js:window.__logsPreview.incoming()", None)], None),
        "filters": ([('#subsw button[data-sub="dt"]', None),
                     ("js:document.getElementById('filterBtn') && document.getElementById('filterBtn').click()", None)], None),
        # 联动：从分析点一行 → 明细 + 带筛选
        "jumpmodel": ([("js:window.__logsPreview.jump('model', 'mimo-v2.5-free')", None)], None),
        # 取一个样例数据里真的有记录的小时（19 点），否则联动后是空列表，看不出"筛选生效"
        "jumphour": ([("js:window.__logsPreview.jump('hour', '19')", None)], None),
        "jumperr": ([("js:window.__logsPreview.jump('err', 'upstream http 502')", None)], None),
    },
    "nodes-v2.html": {
        # task-58：3 个结构方向（① 表格+抽屉 ② 健康看板 ③ 卡片流），默认晨雾。
        # 每态断言：3 方向可切且只显示 1 个；状态徽章三重编码（颜色 + 形状/图形 + 文字）；
        # 判死优先排序；分页 50/页（215 总数）；看板默认只展开坏的两组、趋势占位 data-will-hold；
        # 抽屉能开（≥10 条探测）；危险操作二次确认；刷新不打断（药丸 + 抽屉还在）；
        # 卡片流选择态出批量条；预留字段只读（无控件）。
        "dark": ([("#themeBtn", None)], None),
        "d2": ([('#dirs button[data-d="2"]', None)], None),
        "d2dark": ([('#dirs button[data-d="2"]', None), ("#themeBtn", None)], None),
        "d3": ([('#dirs button[data-d="3"]', None)], None),
        "d3dark": ([('#dirs button[data-d="3"]', None), ("#themeBtn", None)], None),
        "drawer": ([("#tb1 tr", None)], None),
        "drawerDark": ([("#themeBtn", None), ("#tb1 tr", None)], None),
        "danger": ([('[data-danger="reset_all"]', None)], None),
        "dangerDark": ([("#themeBtn", None), ('[data-danger="reset_all"]', None)], None),
        "refresh": ([("#tb1 tr", None), ("js:window.__nv2Preview.simulateRefresh()", None)], None),
        "boardAll": ([('#dirs button[data-d="2"]', None),
                      ("js:document.getElementById('onlyBad2').click()", None)], None),
        "boardAllDark": ([("#themeBtn", None), ('#dirs button[data-d="2"]', None),
                          ("js:document.getElementById('onlyBad2').click()", None)], None),
        "select3": ([('#dirs button[data-d="3"]', None), ("#c3 .ncard .chk", None)], None),
        "select3Dark": ([("#themeBtn", None), ('#dirs button[data-d="3"]', None), ("#c3 .ncard .chk", None)], None),
    },
    "overview-v2.html": {
        "dark": ([("#themeBtn", None)], None),
        "dark+nodes": ([("#themeBtn", None), ('.nav .item[data-go="nodes"]', None)], None),
        "nodes": ([('.nav .item[data-go="nodes"]', None)], None),
        "reqtab": ([('#trendTab button[data-tab="req"]', None)], None),
        "menu": ([("#mobMenu", None)], {"390x844", "320x568"}),
        # 节点图空状态（0 节点）：环全灰 + 中心「暂无节点」+ 所有数变 0，且不得溢出/报错。
        # 形态已锁定为选项 1（用户拍板），页面不再有变体切换条 → 用预览钩子 window.__v2Preview 造空状态。
        "nodeempty": ([("js:window.__v2Preview.emptyNodes(true)", None)], None),
    },
}

# 要截的图：(状态名, 视口名, 输出文件)。key 用 `#2` 后缀表示同一文件的第二组，
# 避免一个文件只能挂一组。
# 说明：settings-v2.html 的 "light" 状态 = 点一次主题按钮（默认夜航）→ 晨雾。
SHOTS = {
    "theme-blue.html": [("default", "1440x900", "theme-blue-1440.png")],
    "theme-blue.html#2": [("default", "390x844", "theme-blue-390.png")],
    "overview-blue.html": [("default", "390x844", "overview-blue-390.png")],
    "overview-blue.html#2": [("default", "1440x900", "overview-blue-1440.png")],
    "shell-blue.html": [("default", "390x844", "shell-blue-390.png")],
    "shell-blue.html#2": [("default", "1440x900", "shell-blue-1440.png")],
    "settings-v2.html": [
        ("conn", "1440x900", "settings-v2-conn-1440-light.png"),
        ("dark", "1440x900", "settings-v2-conn-1440-dark.png"),
        ("route", "1440x900", "settings-v2-route-1440-dark.png"),
        ("future", "1440x900", "settings-v2-future-card-1440-dark.png"),
        ("model", "1440x900", "settings-v2-model-1440-light.png"),
        ("modeldark", "1440x900", "settings-v2-model-1440-dark.png"),
        ("futuremodel", "1440x900", "settings-v2-model-future-1440-dark.png"),
        ("conn", "390x844", "settings-v2-conn-390-light.png"),
        ("route", "390x844", "settings-v2-route-390-light.png"),
        ("model", "390x844", "settings-v2-model-390-light.png"),
        ("conn", "320x568", "settings-v2-conn-320-light.png"),
        ("route", "320x568", "settings-v2-route-320-light.png"),
        ("model", "320x568", "settings-v2-model-320-light.png"),
    ],
    "logs-v2.html": [
        # 分析子视图（默认）
        ("default", "1440x900", "logs-v2-d1-an-1440-dark.png"),
        ("light", "1440x900", "logs-v2-d1-an-1440-light.png"),
        ("d2an", "1440x900", "logs-v2-d2-an-1440-dark.png"),
        ("d3an", "1440x900", "logs-v2-d3-an-1440-dark.png"),
        ("d3anlight", "1440x900", "logs-v2-d3-an-1440-light.png"),
        # 明细子视图
        ("dtlight", "1440x900", "logs-v2-d1-dt-1440-light.png"),
        ("dt", "1440x900", "logs-v2-d1-dt-1440-dark.png"),
        ("d2dt", "1440x900", "logs-v2-d2-dt-1440-dark.png"),
        ("d3dt", "1440x900", "logs-v2-d3-dt-1440-dark.png"),
        ("drawer", "1440x900", "logs-v2-drawer-1440-dark.png"),
        ("unmapped", "1440x900", "logs-v2-unmapped-1440-dark.png"),
        # 联动（点分析 → 明细）
        ("jumpmodel", "1440x900", "logs-v2-jump-model-1440-dark.png"),
        # 手机
        ("light", "390x844", "logs-v2-d1-an-390-light.png"),
        ("dtlight", "390x844", "logs-v2-d1-dt-390-light.png"),
        ("d2anlight", "390x844", "logs-v2-d2-an-390-light.png"),
        ("d3anlight", "390x844", "logs-v2-d3-an-390-light.png"),
        ("light", "320x568", "logs-v2-d1-an-320-light.png"),
        ("dtlight", "320x568", "logs-v2-d1-dt-320-light.png"),
        ("d2anlight", "320x568", "logs-v2-d2-an-320-light.png"),
        ("d3anlight", "320x568", "logs-v2-d3-an-320-light.png"),
    ],
    "settings-v3.html": [
        ("default", "1440x900", "settings-v3-d1-1440-dark.png"),
        ("light", "1440x900", "settings-v3-d1-1440-light.png"),
        ("d2", "1440x900", "settings-v3-d2-1440-dark.png"),
        ("d2light", "1440x900", "settings-v3-d2-1440-light.png"),
        ("d3", "1440x900", "settings-v3-d3-1440-dark.png"),
        ("d3light", "1440x900", "settings-v3-d3-1440-light.png"),
        ("ro", "1440x900", "settings-v3-ro-1440-dark.png"),
        ("undo", "1440x900", "settings-v3-undo-1440-dark.png"),
        ("danger", "1440x900", "settings-v3-danger-1440-dark.png"),
        ("light", "390x844", "settings-v3-d1-390-light.png"),
        ("d2light", "390x844", "settings-v3-d2-390-light.png"),
        ("d3light", "390x844", "settings-v3-d3-390-light.png"),
        ("light", "320x568", "settings-v3-d1-320-light.png"),
        ("d2light", "320x568", "settings-v3-d2-320-light.png"),
        ("d3light", "320x568", "settings-v3-d3-320-light.png"),
    ],
    "overview-v2.html": [
        ("light", "1440x900", "overview-v2-1440-light.png"),
        ("dark", "1440x900", "overview-v2-1440-dark.png"),
        ("light", "390x844", "overview-v2-390-light.png"),
        ("dark", "390x844", "overview-v2-390-dark.png"),
        ("dark", "320x568", "overview-v2-320-dark.png"),
    ],
}


def probe(page, w, h):
    """在当前页面测量宽度是否溢出、有没有超宽元素、样式表是否生效。"""
    return page.evaluate(
        """([w,h]) => {
            const de = document.documentElement;
            const near = [];
            // 「在可横向滚动的祖先里」不算爆版 —— 表格卡内滚动是刻意的、
            // 与原面板 overflow-x:auto 一致的行为，页面本身没被撑开。
            function inScroller(el){
                let p = el.parentElement;
                while (p && p !== document.body) {
                    const cs = getComputedStyle(p);
                    if ((cs.overflowX === 'auto' || cs.overflowX === 'scroll') && p.scrollWidth > p.clientWidth) return true;
                    p = p.parentElement;
                }
                return false;
            }
            const all = document.querySelectorAll('body *');
            for (const el of all) {
                const r = el.getBoundingClientRect();
                if (r.width === 0 && r.height === 0) continue;
                const cs = getComputedStyle(el);
                if (cs.display === 'none' || cs.visibility === 'hidden') continue;
                if (inScroller(el)) continue;
                if (r.right > w + 1) {
                    near.push({
                        tag: el.tagName.toLowerCase(),
                        cls: (el.className && el.className.toString().slice(0, 60)) || '',
                        right: Math.round(r.right),
                        width: Math.round(r.width),
                    });
                }
            }
            // 横向堆叠条量化：段宽之和必须等于容器内容宽（≤0.5px），
            // 每段渲染占比与数据占比之差必须 ≤1pp（否则就是画错了，不是排版问题）。
            let bar = null;
            const tb = document.querySelector('.tbar');
            if (tb) {
                const segs = [...tb.children].filter(e => getComputedStyle(e).display !== 'none');
                const inner = tb.clientWidth;
                const ws = segs.map(e => e.getBoundingClientRect().width);
                const sum = ws.reduce((a, b) => a + b, 0);
                // inner === 0 表示条形所在视图没显示（如切到节点池）⇒ 记 null。
                // 不要写出 NaN —— 那会让 JSON 变成非标准格式，后续解析全崩。
                bar = inner > 0 ? {
                    seg_count: segs.length,
                    inner_width: Math.round(inner * 100) / 100,
                    seg_px: ws.map(w => Math.round(w * 100) / 100),
                    seg_sum: Math.round(sum * 100) / 100,
                    sum_minus_inner: Math.round((sum - inner) * 100) / 100,
                    seg_pct: ws.map(w => Math.round(w / inner * 10000) / 100),
                    sum_ok: Math.abs(sum - inner) <= 0.5,
                    aria: tb.getAttribute('aria-label') || '',
                } : null;
            }
            function nodesProbe(){
              const nc = document.querySelector('#nodeCard');
              // 只有新版节点图表带 data-will-hold 标注；老文件（overview-blue 等）同名元素不算，跳过断言
              if (!nc || !nc.querySelector('[data-will-hold]')) return null;
              const st = {total: +nc.dataset.total, ok: +nc.dataset.ok, warn: +nc.dataset.warn, dead: +nc.dataset.dead};
              const vis = nc.querySelector('.nv:not([hidden])');
              const holdEls = [...nc.querySelectorAll('[data-will-hold]')].filter(e => { const v = e.closest('.nv'); return !v || !v.hidden; });
              const holds = holdEls.map(e => e.getAttribute('data-will-hold')).sort();
              const arcs = [...(vis ? vis.querySelectorAll('.narc') : [])].map(a => {
                const cnt = +a.getAttribute('data-count');
                const len = a.tagName.toLowerCase() === 'path' ? a.getTotalLength() : 2 * Math.PI * (+a.getAttribute('r'));
                const dash = parseFloat((a.getAttribute('stroke-dasharray') || '0').split(/[\s,]+/)[0]) || 0;
                return {count: cnt, frac: len > 0 ? Math.round(dash / len * 10000) / 10000 : null};
              });
              const bars = [...(vis ? vis.querySelectorAll('.nsegbar i') : [])].map(e => +e.getAttribute('data-count'));
              const sum = st.ok + st.warn + st.dead;
              const expect = [st.ok, st.warn, st.dead];
              const arcCounts = arcs.map(a => a.count);
              const fracsOk = arcs.every((a, i) => a.frac === null || (st.total === 0
                    ? a.frac === 0
                    : Math.abs(a.frac - expect[i] / st.total) <= 0.005));
              const srcOk = arcs.length ? (JSON.stringify(arcCounts) === JSON.stringify(expect)) : (JSON.stringify(bars) === JSON.stringify(expect));
              const holdsOk = holds.length === 6 && holds.join(',') === 'ovActive,ovDead,ovExhausted,ovHealthy,ovManual,ovTotal';
              const absOk = ['0', '0', '0', '0'].every(() => true) && (st.total === 0 || (
                  nc.textContent.indexOf(String(st.total)) >= 0 &&
                  nc.textContent.indexOf(String(st.ok)) >= 0 &&
                  nc.textContent.indexOf(String(st.warn)) >= 0 &&
                  nc.textContent.indexOf(String(st.dead)) >= 0));
              return {opt: vis ? vis.getAttribute('data-opt') : null, ...st, sum_eq_total: sum === st.total,
                      hold_count: holds.length, holds, holds_ok: holdsOk,
                      arc_count: arcs.length, arc_counts: arcCounts, arc_fracs: arcs.map(a => a.frac),
                      bar_counts: bars, src_ok: srcOk, frac_ok: fracsOk, abs_ok: absOk,
                      is_empty: nc.classList.contains('is-empty')};
            }

            // 样例数据恒等式：模型明细各行总计之和 == 卡上大数字（差 1 token 就算红）；
            // 趋势图各桶之和与总量同量级（±2%，分桶取整造成）；token 图例里不许出现百分号。
            let ident = null;
            const gt = document.querySelector('#grandTotal');
            const mrows = [...document.querySelectorAll('#modelRows tr[data-tokens]')]
                .filter(tr => tr.id !== 'modelTotalRow');
            if (gt && mrows.length) {
                const grand = +gt.dataset.tokens;
                const rowsSum = mrows.reduce((a, tr) => a + (+tr.dataset.tokens), 0);
                const v2 = window.__v2 || null;
                let trendSum = null, ratio = null;
                if (v2 && v2.trend) {
                    const s2 = a => (a || []).reduce((x, y) => x + y, 0);
                    const tr = v2.trend;
                    trendSum = s2(tr.newin || tr.new) + s2(tr.cr || tr.hit) + s2(tr.ct || tr.out);
                    ratio = Math.round(trendSum / grand * 10000) / 10000;
                }
                const tlegEl = document.querySelector('#tlegMain');
                const legendPct = tlegEl ? (tlegEl.textContent.match(/%/g) || []).length : -1;
                const legendPc = tlegEl ? tlegEl.querySelectorAll('.pc').length : -1;
                // 模型明细列口径：输入 = prompt − 缓存读 − 缓存创建；逐行 输入+输出+缓存读+缓存创建 == 总计
                const cols = [...document.querySelectorAll('#modelRows tr[data-in]')].filter(tr => tr.id !== 'modelTotalRow');
                const rowOk = cols.every(tr => (+tr.dataset.in) + (+tr.dataset.ct) + (+tr.dataset.cr) + (+tr.dataset.cc) === (+tr.dataset.tokens));
                const cacheGtIn = cols.some(tr => (+tr.dataset.cr) > (+tr.dataset.in));
                const ths = [...document.querySelectorAll('#modelRows')].length ? [...document.querySelectorAll('th')].map(t => t.textContent.trim()) : [];
                const hasInCol = ths.includes('输入');
                const pageText = document.body.textContent;
                const noBadWording = pageText.indexOf('新增输入') < 0;
                ident = {
                    model_cols: cols.length,
                    model_row_identity_ok: rowOk,
                    model_cache_gt_in_rows: cols.filter(tr => (+tr.dataset.cr) > (+tr.dataset.in)).length,
                    model_cache_gt_in_ok: cacheGtIn,
                    model_header_has_in: hasInCol,
                    wording_ok: noBadWording,
                    grand_total: grand,
                    model_rows: mrows.length,
                    model_rows_sum: rowsSum,
                    model_sum_ok: rowsSum === grand,
                    model_sum_delta: rowsSum - grand,
                    trend_sum: trendSum,
                    trend_ratio: ratio,
                    trend_ok: trendSum !== null && Math.abs(trendSum - grand) / grand <= 0.02,
                    legend_percent_signs: legendPct,
                    legend_pc_nodes: legendPc,
                    legend_ok: legendPct === 0 && legendPc === 0,
                };
            }
            // ---------- 配置页（settings-v2）专项 ----------
            function cfgProbe(){
              const tabs = document.getElementById('cfgTabs');
              if (!tabs) return null;
              const cards = [...document.querySelectorAll('[data-future]')];
              const sel = '[data-future] input, [data-future] button, [data-future] select, [data-future] textarea';
              const inter = [...document.querySelectorAll(sel)];
              const before = document.body.innerHTML.length;
              inter.forEach(function(e){ try { e.click(); e.focus(); } catch (err) {} });
              const after = document.body.innerHTML.length;
              const txt = document.body.textContent;
              const undo = document.getElementById('undo');
              const fields = [...document.querySelectorAll('[data-future-field]')]
                .map(function(e){ return e.getAttribute('data-future-field'); }).sort();
              return {
                // 方向①/③ 必须删干净（旧 id 与旧文案都不许留）
                old_dirs_gone: !document.getElementById('v1') && !document.getElementById('v3')
                  && !document.getElementById('dir1') && !document.getElementById('dir3')
                  && txt.indexOf('方向①') < 0 && txt.indexOf('方向③') < 0,
                // 4 个页签：3 页 + 模型页
                tab_count: tabs.querySelectorAll('button[data-tab]').length,
                tab_ids: [...tabs.querySelectorAll('button[data-tab]')].map(b => b.id),
                pages: [...document.querySelectorAll('section[data-page]')].map(s => ({
                  id: s.id, hidden: s.hidden,
                  page_h: Math.round(s.getBoundingClientRect().height),
                })),
                // 未上线卡（路由页 1 张 + 模型页 1 张）：默认折叠 + 控件真 disabled + 点了没副作用
                future_cards: cards.length,
                future_ids: cards.map(function(c){ return c.id; }),
                future_all_closed: cards.every(function(c){ return !c.open; }),
                future_interactive: inter.length,
                future_all_disabled: inter.every(function(e){ return e.disabled === true; }),
                future_click_noop: before === after,
                future_fields: fields,
                // 措辞守卫：不许出现"看着能点"与"降级/兜底/替身"类词
                no_fake_clickable: txt.indexOf('可点击') < 0 && txt.indexOf('点击启用') < 0,
                no_fallback_wording: ['自动降级', '兜底', '替身'].every(function(w){ return txt.indexOf(w) < 0; }),
                // 撤销条：默认隐藏；点未上线控件后仍隐藏
                undo_hidden: !undo || undo.hidden,
              };
            }
            // ---------- 日志页 v2（task-51）专项 ----------
            function logsProbe(){
              if (document.body.getAttribute('data-page') !== 'logs-v2') return null;
              const dirs = document.getElementById('dirs');
              if (!dirs) return null;
              const vis = function(sel){
                const e = document.querySelector(sel);
                if (!e) return false;
                const r = e.getBoundingClientRect();
                return !e.hidden && r.width > 0 && r.height > 0;
              };
              // 每个方向 = 分析面板 + 明细面板，任一可见即该方向可见（分析态看 anN、明细态看 dirN）
              const dv = [1, 2, 3].map(function(i){ return vis('#an' + i) || vis('#dir' + i); });
              const idx = dv.indexOf(true);
              const sec = idx >= 0 ? document.getElementById('dir' + (idx + 1)) : null;
              const txt = (sec || document.body).textContent;
              // 只检查「状态徽章」：.bg.plain 是标签类（未上线字段/端点说明），不参与符号编码断言
              const badges = [...(sec ? sec.querySelectorAll('.bg:not(.plain)') : [])];
              const bTxt = badges.map(function(b){ return b.textContent.replace(/\s+/g, ' ').trim(); });
              // 四重编码：每个徽章都必须「符号 + 文字」，不能只有颜色
              const symOk = bTxt.every(function(t){ return /[✓✕⏱↻?⊘→]/.test(t); });
              const wordOk = bTxt.every(function(t){ return /[\u4e00-\u9fa5]/.test(t); });
              const kinds = {};
              ['成功','失败','超时','重试','未映射','闸门','直连','未映射'].forEach(function(k){ kinds[k] = bTxt.filter(function(t){ return t.indexOf(k) >= 0; }).length; });
              // 统计条与"预留路由位"是**跨方向共享**的（状态条在 section 外、抽屉内容按行渲染多次）
              // ⇒ 必须全文档查 + 去重，否则每态都会因为没有/重复而误判。
              const holdAll = [...document.querySelectorAll('[data-will-hold]')]
                .map(function(e){ return e.getAttribute('data-will-hold').replace(/Lab$/, ''); });
              const hold = holdAll.filter(function(v, i){ return holdAll.indexOf(v) === i; }).sort();
              const futAll = [...document.querySelectorAll('[data-future-field]')]
                .map(function(e){ return e.getAttribute('data-future-field'); });
              const future = futAll.filter(function(v, i){ return futAll.indexOf(v) === i; }).sort();
              const futureInputs = document.querySelectorAll('[data-future-field] input, [data-future-field] select, [data-future-field] textarea');
              const counts = window.__logsPreview ? window.__logsPreview.counts() : null;
              const drawer = document.getElementById('drawer');
              const subsw = document.getElementById('subsw');
              const subSel = subsw ? subsw.querySelector('button[aria-selected="true"]') : null;
              const sub = subSel ? subSel.getAttribute('data-sub') : null;
              const an = document.getElementById('an' + (idx + 1));
              const anVis = an ? (!an.hidden && an.getBoundingClientRect().height > 0) : false;
              function qa(sel){ return an ? an.querySelectorAll(sel).length : 0; }
              return {
                sub_tabs: subsw ? subsw.querySelectorAll('button[data-sub]').length : 0,
                sub: sub,
                sub_consistent: (sub === 'an') === anVis,
                an_visible: anVis,
                an_kpis: qa('.kpi'),
                an_hours: qa('.hb'),
                an_rows: qa('.drow, .dtable tr.click'),
                an_future: [...(an ? an.querySelectorAll('[data-future-field]') : [])]
                  .map(function(e){ return e.getAttribute('data-future-field'); })
                  .filter(function(v, i, a){ return a.indexOf(v) === i; }).sort(),
                link_on: (document.getElementById('linkChip') || {}).classList
                  ? document.getElementById('linkChip').classList.contains('on') : false,
                link_txt: (document.getElementById('linkChipTxt') || {}).textContent || '',
                dir_tabs: dirs.querySelectorAll('button[data-d]').length,
                dirs_visible: dv,
                one_dir: dv.filter(Boolean).length === 1,
                active_dir: idx + 1,
                badges: badges.length,
                badge_kinds: kinds,
                badge_sym_ok: symOk,
                badge_word_ok: wordOk,
                holds: hold,
                holds_ok: hold.length >= 4 && hold.join(',') === 'clFail,clIssue,clRealRate,clTotal',
                future_fields: future,
                future_readonly: futureInputs.length === 0,
                counts: counts,
                page_size: sec ? sec.querySelectorAll('#flist .row, #tblBody tr, #cards3 .card3').length : 0,
                pagination: !!(document.getElementById('pgPrev') && document.getElementById('pgNext')),
                pk_ctor: document.getElementById('pgInfo') ? document.getElementById('pgInfo').textContent.trim() : '',
                mobile_cards: (document.querySelectorAll('#mcards .mcard') || []).length,
                drawer_on: !!drawer && drawer.classList.contains('on'),
                live_toggle: !!document.getElementById('liveChk'),
                newpill: !!document.getElementById('newpill'),
                filters_open: !!(document.getElementById('filters') && document.getElementById('filters').classList.contains('open')),
                // 这些词必须在页面里以**文字**形式存在（不是只靠颜色）——
                // 用全文档统计：方向2/3 的其它徽章也在 DOM 里，足以证明编码存在。
                unmapped_words: (document.body.textContent.match(/未映射/g) || []).length,
                blocked_words: (document.body.textContent.match(/闸门拒绝/g) || []).length,
                timeout_words: (document.body.textContent.match(/超时/g) || []).length,
              };
            }
            // ---------- 设置页 v3（task-54）专项 ----------
            function sv3Probe(){
              // 只认设置页（日志页也有 #dirs 方向切换条，靠 data-page 区分）
              if (document.body.getAttribute('data-page') !== 'settings-v3') return null;
              const bar = document.getElementById('dirs');
              if (!bar) return null;
              const html = document.body.innerHTML;
              const txt = document.body.textContent;
              const vis = function(sel){
                const e = document.querySelector(sel);
                if (!e) return false;
                const r = e.getBoundingClientRect();
                return !e.hidden && r.width > 0 && r.height > 0;
              };
              const dv = [1, 2, 3].map(function(i){ return vis('#d' + i); });
              const idx = dv.indexOf(true);
              const active = idx >= 0 ? document.getElementById('d' + (idx + 1)) : null;
              const ctls = active ? [].slice.call(active.querySelectorAll('[data-ctl]')) : [];
              const roRows = active ? [].slice.call(active.querySelectorAll('.ro:not(.badge)')) : [];
              // 出口组 + 被删/写死字段：字段名与旧面板控件文案，一个都不许出现
              const banned = ['socks5', 'active_socks5', 'socks5_paid_direct', 'socks5_sticky',
                              '走哪个出口', '带 key / 付费请求直连', '直连（不使用代理）',
                              'prompt_cache_retention', 'cache_control_breakpoints',
                              'text_only_models', 'truncation_stop_reason',
                              'model_region_map', 'quota_error_signals'];
              const hits = banned.filter(function(w){ return html.toLowerCase().indexOf(w.toLowerCase()) >= 0; });
              const roInputs = active ? active.querySelectorAll('.ro input, .ro select, .ro textarea') : [];
              const dis = active ? [].slice.call(active.querySelectorAll('input:disabled, select:disabled, button:disabled')) : [];
              const modal = document.getElementById('modal');
              return {
                dirs: bar.querySelectorAll('button[data-d]').length,
                dir_note: (document.getElementById('dirNote') || {}).textContent || '',
                dirs_visible: dv,
                one_dir: dv.filter(Boolean).length === 1,
                active_dir: idx + 1,
                ctl_count: ctls.length,
                ctl_names: ctls.map(function(e){ return e.getAttribute('data-ctl'); }).sort(),
                ro_rows: roRows.length,
                ro_all_labeled: roRows.length > 0 && roRows.every(function(r){
                  return r.textContent.indexOf('改请改配置文件') >= 0;
                }),
                ro_has_input: roInputs.length,
                disabled_count: dis.length,
                disabled_all_true: dis.every(function(e){ return e.disabled === true; }),
                danger_btns: active ? active.querySelectorAll('.dgr button').length : 0,
                forbidden_hits: hits,
                modal_on: !!modal && modal.classList.contains('on'),
                undo_on: (document.getElementById('undo') || {}).classList
                  ? document.getElementById('undo').classList.contains('on') : false,
                editable_inputs: active ? [].slice.call(active.querySelectorAll(
                  'input[type="text"], input[type="password"], input[type="number"], select'))
                  .filter(function(e){ return !e.closest('.ro'); }).length : 0,
              };
            }
            function nv2Probe(){
              // 只有节点页 v2 才量这套；别的页面返回 null，不污染它们的 pass
              if (!document.getElementById('t1') && !document.getElementById('c3') && !window.__nv2Preview) return null;
              const dirs = [].slice.call(document.querySelectorAll('#dirs button'));
              const visDirs = [].slice.call(document.querySelectorAll('.dir')).filter(function(s){ return !s.hidden; });
              const badges = [].slice.call(document.querySelectorAll('.dir .st[data-state]'));
              const badgeStates = Array.from(new Set(badges.map(function(b){ return b.getAttribute('data-state'); }))).sort();
              const badgeOk = badges.length > 0 && badges.every(function(b){
                const txt = (b.textContent || '').trim();
                return !!b.querySelector('svg') && !!b.querySelector('.st-shape')
                  && /可用|判死|配额冷却/.test(txt) && !!b.getAttribute('data-glyph');
              });
              const legend = [].slice.call(document.querySelectorAll('#distLegend2 .st'));
              const rows = [].slice.call(document.querySelectorAll('#tb1 tr'));
              const pg = (document.getElementById('pgInfo1') || {}).textContent || '';
              const firstBadge = rows.length ? rows[0].querySelector('.st[data-state]') : null;
              const groups = [].slice.call(document.querySelectorAll('#gb2 .group'));
              const trend = document.getElementById('trendSvg');
              const hold = document.querySelector('[data-will-hold="nv2Trend"]');
              const future = [].slice.call(document.querySelectorAll('[data-future-field]'));
              const drawer = document.getElementById('drawer');
              const modal = document.getElementById('modal');
              const pill = document.getElementById('updatePill');
              const bb3 = document.getElementById('bb3');
              const sel3 = document.getElementById('selCnt3');
              const sub = document.querySelector('#tb1 .nodecell .sub');
              const cards = [].slice.call(document.querySelectorAll('#c3 .ncard'));
              const probes = [].slice.call(document.querySelectorAll('#drawerBody .probe'));
              return {
                dir_tabs: dirs.length,
                one_dir: visDirs.length === 1,
                active: visDirs.length === 1 ? visDirs[0].id : null,
                theme: de.getAttribute('data-theme'),
                badge_count: badges.length,
                badge_states: badgeStates,
                badge_ok: badgeOk,
                legend_ok: legend.length === 3,
                rows: rows.length,
                pager_ok: /\/ 215/.test(pg) && rows.length <= 50,
                sort_ok: !!firstBadge && firstBadge.getAttribute('data-state') === 'dead',
                groups: groups.length,
                group_states: groups.map(function(g){ return g.getAttribute('data-state'); }),
                trend_ok: !!trend && trend.children.length >= 8,
                hold_ok: !!hold,
                future: future.length,
                future_ok: future.length === 3 && future.every(function(f){
                  return !f.querySelector('input,select,button,textarea');
                }),
                drawer_on: !!drawer && drawer.classList.contains('on'),
                modal_on: !!modal && modal.classList.contains('on'),
                pill_on: !!pill && pill.classList.contains('on'),
                batch_on: !!bb3 && bb3.classList.contains('on'),
                sel: sel3 ? parseInt(sel3.textContent, 10) || 0 : 0,
                cards: cards.length,
                probes: probes.length,
                name_ok: !!sub && /:\d+/.test(sub.textContent || '')
              };
            }
            const oxBody = getComputedStyle(document.body).overflowX;
            const oxHtml = getComputedStyle(de).overflowX;
            // 样式表是否真的生效
            let cssRules = -1, cssLive = false;
            try {
                if (document.styleSheets.length > 0) {
                    cssRules = document.styleSheets[0].cssRules.length;
                    cssLive = cssRules > 30;
                }
            } catch (e) { cssRules = -2; }
            const bodyMargin = getComputedStyle(document.body).marginTop;
            return {
                scrollWidth: de.scrollWidth,
                clientWidth: de.clientWidth,
                innerWidth: window.innerWidth,
                ok: de.scrollWidth === de.clientWidth,
                overflow_x_hidden: (oxBody === 'hidden' || oxHtml === 'hidden'),
                stylesheets: document.styleSheets.length,
                css_rules: cssRules,
                css_live: cssLive,
                body_margin_top: bodyMargin,
                theme: de.getAttribute('data-theme'),
                wider_than_viewport: near.slice(0, 12),
                wider_count: near.length,
                tbar: bar,
                identity: ident,
                nodes: nodesProbe(),
                page_has_no_newin: document.body.textContent.indexOf('新增输入') < 0,
                // 预览专用控件必须为 0：节点图变体切换条（已按用户拍板删除，别再加回来）
                preview_only_controls: document.querySelectorAll('#nodeOptBar, .pvnode, #nodeEmptyBtn').length,
                cfg: cfgProbe(),
                sv3: sv3Probe(),
                logs: logsProbe(),
                nv2: nv2Probe(),
                page_h: de.scrollHeight,
                screens: Math.round(de.scrollHeight / window.innerHeight * 100) / 100,
            };
        }""",
        [w, h],
    )


def measure(browser, path, w, h, actions, sname=""):
    ctx = browser.new_context(viewport={"width": w, "height": h})
    page = ctx.new_page()
    pageerrors, conerrs, nonfile = [], [], []

    page.on("pageerror", lambda e: pageerrors.append(str(e)[:300]))
    page.on(
        "console",
        lambda m: conerrs.append(m.text[:300]) if m.type == "error" else None,
    )
    page.on(
        "request",
        lambda r: nonfile.append(r.url[:160])
        if not r.url.startswith(("file://", "data:", "blob:"))
        else None,
    )

    page.goto(path.as_uri(), wait_until="load")
    page.wait_for_timeout(280)

    auto_menu = 0
    for sel, _ in actions:
        try:
            # "js:<expr>" = 直接执行（用于没有 UI 入口的预览钩子，例如空状态）
            if sel.startswith("js:"):
                page.evaluate(sel[3:])
                page.wait_for_timeout(140)
                continue
            # 窄屏下导航项收在汉堡里 → 目标不可见时先展开菜单（这是设计行为，
            # 不是靠 force 点隐藏元素作弊）。记录 auto_menu 次数以供核对。
            if not page.is_visible(sel):
                try:
                    if page.is_visible("#mobMenu"):
                        page.click("#mobMenu", timeout=1500)
                        page.wait_for_timeout(120)
                        auto_menu += 1
                except Exception:
                    pass
            page.click(sel, timeout=2500)
            page.wait_for_timeout(140)
        except Exception as e:  # 选择器不存在 → 明确记录，不静默
            pageerrors.append("CLICK_FAILED %s: %s" % (sel, str(e)[:120]))

    pr = probe(page, w, h)
    out = {
        **pr,
        "pageerror": len(pageerrors),
        "pageerror_msgs": pageerrors,
        "console_error": len(conerrs),
        "console_error_msgs": conerrs,
        "requests_non_file": nonfile,
        "requests_non_file_count": len(nonfile),
        "clicked": [a[0] for a in actions],
        "auto_opened_menu": auto_menu,
    }
    ident = pr.get("identity")
    ident_ok = True if ident is None else bool(
        ident["model_sum_ok"] and ident["trend_ok"] and ident["legend_ok"]
        and ident.get("model_row_identity_ok", True)
        and ident.get("model_cache_gt_in_ok", True)
        and ident.get("model_header_has_in", True)
        and ident.get("wording_ok", True)
    )
    cfg = pr.get("cfg")
    opened_future = any(a[0].startswith("js:") and ("card-future" in a[0] or "m-card-route" in a[0]) for a in actions)
    # 预留字段集合：路由页 6 项（节点自动选择）+ 模型页 7 项（模型自动路由）必须都在
    FIELDS_ROUTE = {"route_enabled", "retry_unreachable", "retry_normal",
                    "rank_prefer", "node_allow_deny", "region_prefer"}
    FIELDS_MODEL = {"router_enabled", "logical_model", "candidates", "rank_by",
                    "on_fail", "sticky_model", "expose_model"}
    cfg_ok = True if cfg is None else bool(
        cfg["old_dirs_gone"]
        and cfg["tab_count"] == 4
        # 「默认折叠」只在没主动展开它的状态里要求（future/futuremodel 状态就是专门展开来测的）
        and (cfg["future_all_closed"] or opened_future)
        and cfg["future_cards"] >= 2
        and cfg["future_interactive"] >= 13
        and cfg["future_all_disabled"]
        and cfg["future_click_noop"]
        and cfg["no_fake_clickable"]
        and cfg["no_fallback_wording"]
        and FIELDS_ROUTE <= set(cfg["future_fields"])
        and FIELDS_MODEL <= set(cfg["future_fields"])
    )
    logs = pr.get("logs")
    # task-51 门禁：3 方向 × 2 子视图，任何时刻「1 个方向 + 1 个子视图」可见；
    # 分析侧：6 张 KPI + 24 根小时柱 + ≥12 行分布 + 路由预留 3 字段；
    # 明细侧：徽章「符号+文字」双编码、未映射/闸门/超时都要有文字标签、统计数带 data-will-hold；
    # 联动：点分析里的模型/小时/错误 → 子视图切到明细、显示筛选来源、且条数确实变少。
    logs_ok = True if logs is None else bool(
        logs["dir_tabs"] == 3
        and logs["one_dir"]
        and logs["sub_tabs"] == 2
        and logs["sub_consistent"]
        and logs["an_future"] == ["route_candidates", "route_pick_reason", "routed_from"]
    )
    if logs is not None and logs["sub"] == "an":
        logs_ok = logs_ok and bool(
            logs["an_visible"]
            and logs["an_kpis"] == 6
            and logs["an_hours"] == 24
            and logs["an_rows"] >= 12
            and not logs["link_on"]
        )
    elif logs is not None:
        logs_ok = logs_ok and bool(
            logs["badges"] >= 3
            and logs["badge_sym_ok"]
            and logs["badge_word_ok"]
            and logs["holds_ok"]
            and logs["future_readonly"]
            and logs["pagination"]
            and logs["live_toggle"]
            and logs["unmapped_words"] >= 1
            and logs["blocked_words"] >= 1
            and logs["timeout_words"] >= 1
        )
    if logs is not None:
        if sname == "drawer":
            logs_ok = logs_ok and logs["drawer_on"]
        else:
            logs_ok = logs_ok and (not logs["drawer_on"])
        if sname in ("unmapped", "today", "slow"):
            logs_ok = logs_ok and logs["counts"]["visible"] < logs["counts"]["total"]
        if sname == "incoming":
            logs_ok = logs_ok and logs["counts"]["total"] >= 25
        if sname in ("jumpmodel", "jumphour", "jumperr"):
            # 联动硬断言：落到明细 + 显示来源 + 条数变少
            logs_ok = logs_ok and logs["sub"] == "dt" and logs["link_on"] \
                and 0 < logs["counts"]["visible"] < logs["counts"]["total"]
    sv3 = pr.get("sv3")
    # task-54 门禁：3 方案可切、每次只显示 1 个；8 个可编辑控件；
    # 只读项 ≥4 且每条都标「改请改配置文件」、内部无控件；出口组/被删字段 0 命中；
    # 危险区 4 个按钮；默认态不许弹窗；undo 态必须出现撤销条。
    sv3_ok = True if sv3 is None else bool(
        sv3["dirs"] == 3
        and sv3["one_dir"]
        and sv3["ctl_count"] == 8
        and sv3["editable_inputs"] >= 5
        and sv3["ro_rows"] >= 4
        and sv3["ro_all_labeled"]
        and sv3["ro_has_input"] == 0
        and sv3["disabled_all_true"]
        and not sv3["forbidden_hits"]
        and sv3["danger_btns"] == 4
    )
    # 状态专属期望：danger 态必须弹出二次确认；undo 态必须出现撤销条；其余态两者都不许出现
    if sv3 is not None:
        if sname == "danger":
            sv3_ok = sv3_ok and sv3["modal_on"]
        elif sname == "undo":
            sv3_ok = sv3_ok and sv3["undo_on"]
        else:
            sv3_ok = sv3_ok and (not sv3["modal_on"])
    nv2 = pr.get("nv2")
    nv2_ok = True if nv2 is None else bool(
        nv2["dir_tabs"] == 3
        and nv2["one_dir"]
        and nv2["active"] in ("d1", "d2", "d3")
        and nv2["badge_count"] >= 3
        and nv2["badge_states"] == ["available", "dead", "exhausted"]
        and nv2["badge_ok"]
        and nv2["legend_ok"]
        and nv2["future"] == 3
        and nv2["future_ok"]
        and nv2["hold_ok"]
        and nv2["theme"] == ("dark" if sname.lower().endswith("dark") else "light")
    )
    if nv2 is not None:
        base = sname[:-4] if sname.lower().endswith("dark") else sname
        if base == "default":
            nv2_ok = nv2_ok and nv2["active"] == "d1" and nv2["pager_ok"] and nv2["sort_ok"] \
                and nv2["name_ok"] and not nv2["drawer_on"] and not nv2["modal_on"] and not nv2["pill_on"]
        elif base == "d2":
            nv2_ok = nv2_ok and nv2["active"] == "d2" and nv2["trend_ok"] \
                and ("dead" in nv2["group_states"]) and ("exhausted" in nv2["group_states"]) \
                and ("available" not in nv2["group_states"])
        elif base == "boardAll":
            nv2_ok = nv2_ok and nv2["active"] == "d2" and nv2["groups"] == 3 \
                and ("available" in nv2["group_states"])
        elif base == "d3":
            nv2_ok = nv2_ok and nv2["active"] == "d3" and nv2["cards"] >= 60 \
                and not nv2["batch_on"] and nv2["sel"] == 0
        elif base == "select3":
            nv2_ok = nv2_ok and nv2["active"] == "d3" and nv2["batch_on"] and nv2["sel"] >= 1
        elif base == "drawer":
            nv2_ok = nv2_ok and nv2["drawer_on"] and nv2["probes"] >= 10
        elif base == "danger":
            nv2_ok = nv2_ok and nv2["modal_on"]
        elif base == "refresh":
            nv2_ok = nv2_ok and nv2["pill_on"] and nv2["drawer_on"]
    nodes = pr.get("nodes")
    nodes_ok = True if nodes is None else bool(
        nodes["sum_eq_total"] and nodes["holds_ok"] and nodes["src_ok"]
        and nodes["frac_ok"] and nodes["abs_ok"]
        and (nodes["total"] != 0 or nodes["is_empty"])
    )
    out["pass"] = bool(
        pr["ok"]
        and sv3_ok
        and logs_ok
        and pr["css_live"]
        and not pr["overflow_x_hidden"]
        and not pageerrors
        and not conerrs
        and not nonfile
        and ident_ok
        and nodes_ok
        and nv2_ok
        and cfg_ok
        and pr.get("preview_only_controls", 0) == 0
    )
    return out, page, ctx


def run(files, do_shot=True):
    out = {"chrome": CHROME, "chrome_exists": os.path.exists(CHROME), "files": {}}
    with sync_playwright() as p:
        browser = p.chromium.launch(executable_path=CHROME, headless=True)
        for f in files:
            path = (HERE / f).resolve()
            rec = {"path": str(path), "exists": path.exists(), "states": {}}
            if not path.exists():
                out["files"][f] = rec
                continue

            state_defs = [("default", [], None)] + [
                (k, v[0], v[1]) for k, v in STATES.get(f, {}).items()
            ]
            for vname, w, h in VIEWPORTS:
                for sname, actions, only_vp in state_defs:
                    if only_vp and vname not in only_vp:
                        continue
                    rec_state, page, ctx = measure(browser, path, w, h, actions, sname)
                    rec["states"]["%s|%s" % (vname, sname)] = rec_state
                    # 截图：state 名 "light" 等价于默认态（页面默认就是晨雾）
                    for shot_state, shot_vp, shot_file in SHOTS.get(f, []):
                        if shot_vp != vname:
                            continue
                        if sname == shot_state or (shot_state == "light" and sname == "default"):
                            page.screenshot(path=str(HERE / shot_file), full_page=False)
                            rec_state.setdefault("shots", []).append(shot_file)
                    ctx.close()

            out["files"][f] = rec

        browser.close()

    out["ALL_PASS"] = all(
        st["pass"] for rec in out["files"].values() for st in rec.get("states", {}).values()
    )
    out["TOTAL_STATES"] = sum(len(rec.get("states", {})) for rec in out["files"].values())
    out["PASSED_STATES"] = sum(
        1 for rec in out["files"].values() for st in rec.get("states", {}).values() if st["pass"]
    )
    return out


if __name__ == "__main__":
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    do_shot = "--no-shot" not in sys.argv
    if not args:
        # 只认本任务负责的 5 个文件 —— 同目录还有别人负责的 logs.html / settings.html（旧版两方案原型，保留作对照），
        # 拿本脚本去量它们的通过率没有意义（也不该往他们的验收里混数）。
        mine = ["theme-blue.html", "overview-blue.html", "shell-blue.html",
                "overview-v2.html", "settings-v2.html", "settings-v3.html", "logs-v2.html",
                "nodes-v2.html"]
        args = [f for f in mine if (HERE / f).exists()]
    print(json.dumps(run(args, do_shot), ensure_ascii=False, indent=2))
