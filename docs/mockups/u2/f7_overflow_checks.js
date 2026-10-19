/* task-24 验收脚本：窄屏横向溢出 + 桌面不退化 + 触控高度。
   用法：
     BASE=http://127.0.0.1:9060/ OUT=/tmp/out.json DIR=<repo>/docs/mockups/u2 \
       node docs/mockups/u2/f7_overflow_checks.js
   断言：
     A) 390×844 / 320×568：每个页面 documentElement.scrollWidth === clientWidth
     B) 同两档：effective_offenders（未被带 overflow-x 的祖先裁剪的 right>clientWidth 元素）为空
     C) 1440×900：.filter-bar 里两个 select 仍在同一行（|Δy| ≤ 2）
     D) 390/320：两个 select 的触控高度 ≥ 32px
     E) 全程 pageerror == 0
   说明：raw_offenders 会把「宽表在 overflow-x:auto 卡片内」的元素也算进来（例如 #statsTable
   在 320px 下比卡片宽，但被卡片裁剪/滚动，不贡献页面滚动宽度），因此额外给出 contained 标记
   与 effective_offenders —— 后者才是「谁把页面撑宽了」。 */
const { chromium } = require('/home/jingyx/.npm/_npx/e41f203b7505f1fb/node_modules/playwright');
const fs = require('fs');

const EXE = '/home/jingyx/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome';
const STATE = '/tmp/c5ui7/alert.json';
const BASE = process.env.BASE || 'http://127.0.0.1:9056/';
const OUT = process.env.OUT || '/tmp/c5ui7/res_overflow.json';
const DIR = process.env.DIR || '/home/jingyx/project/opencode2api-test/docs/mockups/u2';
const PAGES = ['overview', 'nodes', 'subs', 'misc', 'logs'];

const ACTIVE = {
  active: true, reason: '节点出口自 21:41:14 起已连续失败 10 次、跨 19m31s，期间零成功',
  since: '2026-10-19T21:41:14+08:00', window_minutes: 10, window_attempts: 4, window_successes: 1,
  window_success_rate: 0.25, run_failures: 10, run_minutes: 19.5, pool_available: 115, pool_total: 227
};
fs.writeFileSync(STATE, JSON.stringify(ACTIVE));

const MEASURE = () => {
  const cw = document.documentElement.clientWidth;
  const contained = (el) => {
    let n = el.parentElement;
    while (n && n !== document.documentElement) {
      const ox = getComputedStyle(n).overflowX;
      if (ox !== 'visible') return true;      // 被祖先裁剪/滚动 ⇒ 不把页面撑宽
      n = n.parentElement;
    }
    return false;
  };
  const rect = (e) => { const r = e.getBoundingClientRect(); return { x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height), right: Math.round(r.right) }; };
  const raw = [];
  document.querySelectorAll('body *').forEach(e => {
    const r = e.getBoundingClientRect();
    if (r.width > 0 && r.right > cw + 0.5) {
      raw.push({
        tag: e.tagName.toLowerCase(), id: e.id || '', cls: (e.className || '').toString().slice(0, 40),
        right: Math.round(r.right), w: Math.round(r.width), h: Math.round(r.height),
        contained_by_scroller: contained(e)
      });
    }
  });
  const sel = (id) => { const e = document.getElementById(id); return e ? { rect: rect(e), h: Math.round(e.getBoundingClientRect().height) } : null; };
  const headerButtons = [...document.querySelectorAll('[data-od-id="trends"] button, [data-od-id="trends"] select')]
    .map(b => ({ id: b.id || '', cls: (b.className || '').toString().slice(0, 30), text: (b.textContent || '').trim().slice(0, 14), h: Math.round(b.getBoundingClientRect().height), visible: b.offsetParent !== null }));
  return {
    clientWidth: cw,
    scrollWidth: document.documentElement.scrollWidth,
    bodyScrollWidth: document.body.scrollWidth,
    overflow: document.documentElement.scrollWidth > cw,
    active_page: [...document.querySelectorAll('.page.active')].map(p => p.id),
    raw_offenders: raw,
    effective_offenders: raw.filter(o => !o.contained_by_scroller),
    trendRangeSel: sel('trendRangeSel'),
    trendModelSel: sel('trendModelSel'),
    trend_header_controls: headerButtons,
    nodeAlert: (() => { const e = document.getElementById('nodeAlert'); if (!e) return null; const r = e.getBoundingClientRect(); return { classes: e.className, h: Math.round(r.height), in_viewport: r.width > 0 && r.left >= 0 && r.right <= cw }; })()
  };
};

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: EXE });
  const R = { base: BASE, start: new Date().toISOString(), shots: [], runs: {}, asserts: [] };
  const pageErrors = [];
  const A = (label, ok, actual, expected) => R.asserts.push({ label, ok: !!ok, expected, actual });

  for (const vp of [{ w: 390, h: 844 }, { w: 320, h: 568 }, { w: 1440, h: 900 }]) {
    const ctx = await browser.newContext({ viewport: { width: vp.w, height: vp.h } });
    const page = await ctx.newPage();
    page.on('pageerror', e => pageErrors.push(`${vp.w}x${vp.h}: ${String((e && e.message) || e)}`));
    await page.goto(BASE, { waitUntil: 'domcontentloaded' });
    await page.waitForTimeout(1500);
    const key = `${vp.w}x${vp.h}`;
    R.runs[key] = {};
    for (const p of PAGES) {
      await page.evaluate((pg) => { if (typeof showPage === 'function') showPage(pg); }, p);
      await page.waitForTimeout(p === 'logs' ? 900 : 500);
      const m = await page.evaluate(MEASURE);
      R.runs[key][p] = m;
      if (vp.w !== 1440) {
        A(`[${key}/${p}] scrollWidth === clientWidth`, m.scrollWidth === m.clientWidth, `${m.scrollWidth} vs ${m.clientWidth}`, 'equal');
        A(`[${key}/${p}] effective_offenders 为空`, m.effective_offenders.length === 0, m.effective_offenders, []);
      }
      if (p === 'overview') {
        if (vp.w === 1440) {
          const dy = (m.trendRangeSel && m.trendModelSel) ? Math.abs(m.trendRangeSel.rect.y - m.trendModelSel.rect.y) : 999;
          A('[1440x900/overview] 两个 select 同一行 |Δy| ≤ 2', dy <= 2, dy, '<=2');
          A('[1440x900] scrollWidth === clientWidth', m.scrollWidth === m.clientWidth, `${m.scrollWidth} vs ${m.clientWidth}`, 'equal');
        } else {
          // 截图（模型明细表等要滚动才看得到，但溢出判定在整页）
          const file = `${DIR}/f7-overflow-${vp.w}x${vp.h}.png`;
          await page.evaluate(() => scrollTo(0, 0));
          await page.screenshot({ path: file, fullPage: false });
          R.shots.push({ viewport: vp, file, page: 'overview' });
        }
      }
    }
    // 触控高度断言：只看这次改造涉及的 .filter-bar 两个 select
    const ov = R.runs[key].overview;
    if (ov.trendRangeSel && ov.trendModelSel) {
      A(`[${key}] #trendRangeSel 触控高度 ≥ 32`, ov.trendRangeSel.h >= 32, ov.trendRangeSel.h, '>=32');
      A(`[${key}] #trendModelSel 触控高度 ≥ 32`, ov.trendModelSel.h >= 32, ov.trendModelSel.h, '>=32');
    }
    await ctx.close();
  }

  R.pageErrors = pageErrors;
  A('全程 pageerror == 0', pageErrors.length === 0, pageErrors, []);
  R.failed = R.asserts.filter(x => !x.ok);
  R.pass = R.failed.length === 0;
  R.end = new Date().toISOString();
  await browser.close();
  fs.writeFileSync(OUT, JSON.stringify(R, null, 2));
  console.log(`=== ${R.pass ? 'ALL PASS' : 'FAILED'} （${R.asserts.length - R.failed.length}/${R.asserts.length}）===`);
  for (const a of R.asserts) console.log(`${a.ok ? 'PASS' : 'FAIL'}  ${a.label}  actual=${JSON.stringify(a.actual)}`);
  console.log('shots:', JSON.stringify(R.shots));
  for (const k of Object.keys(R.runs)) {
    for (const p of PAGES) {
      const m = R.runs[k][p];
      console.log(`[${k}/${p}] client=${m.clientWidth} scroll=${m.scrollWidth} overflow=${m.overflow} raw_offenders=${m.raw_offenders.length} effective=${m.effective_offenders.length}`);
      if (m.effective_offenders.length) console.log('   effective:', JSON.stringify(m.effective_offenders));
      if (m.raw_offenders.length && !m.effective_offenders.length) console.log('   raw(contained):', JSON.stringify(m.raw_offenders.map(o => `${o.tag}#${o.id}.${o.cls} right=${o.right}`)));
    }
  }
  process.exit(R.pass ? 0 : 1);
})().catch(e => { console.error('FATAL', e); process.exit(1); });
