/* task-19 截图 + 两档布局体检：桌面 1440×900 与移动 390×844，
   都停在默认首页「概览」且横幅可见。输出到 docs/mockups/u2/。 */
const { chromium } = require('/home/jingyx/.npm/_npx/e41f203b7505f1fb/node_modules/playwright');
const fs = require('fs');

const EXE = '/home/jingyx/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome';
const STATE = '/tmp/c5ui7/alert.json';
const BASE = process.env.BASE || 'http://127.0.0.1:9056/';
const DIR = process.env.DIR || '/home/jingyx/project/opencode2api-test/docs/mockups/u2';

const ACTIVE = {
  active: true,
  reason: '节点出口自 21:41:14 起已连续失败 10 次、跨 19m31s，期间零成功',
  since: '2026-10-19T21:41:14+08:00',
  window_minutes: 10, window_attempts: 4, window_successes: 1,
  window_success_rate: 0.25,
  run_failures: 10, run_minutes: 19.5,
  pool_available: 115, pool_total: 227
};

(async () => {
  fs.writeFileSync(STATE, JSON.stringify(ACTIVE));
  const browser = await chromium.launch({ headless: true, executablePath: EXE });
  const out = { base: BASE, dir: DIR, shots: [] };

  for (const vp of [{ w: 1440, h: 900 }, { w: 390, h: 844 }]) {
    const ctx = await browser.newContext({ viewport: { width: vp.w, height: vp.h }, deviceScaleFactor: 1 });
    const page = await ctx.newPage();
    const errs = [];
    page.on('pageerror', e => errs.push(String((e && e.message) || e)));
    page.on('console', m => { if (m.type() === 'error' && !/Failed to load resource/.test(m.text())) errs.push(m.text()); });
    await page.goto(BASE, { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => {
      const e = document.querySelector('#nodeAlert');
      return !!e && e.classList.contains('on') && e.getBoundingClientRect().height > 0;
    }, null, { timeout: 15000 });
    await page.waitForTimeout(1200);   // 等首屏 fetch/布局稳定
    const name = `f7-banner-overview-${vp.w}x${vp.h}.png`;
    const path = `${DIR}/${name}`;
    await page.screenshot({ path, fullPage: false });
    const probe = await page.evaluate(() => {
      const r = document.querySelector('#nodeAlert').getBoundingClientRect();
      const tb = document.querySelector('.topbar').getBoundingClientRect();
      const sb = document.querySelector('.sidebar').getBoundingClientRect();
      const hit = (a, b) => !(a.right <= b.left || a.left >= b.right || a.bottom <= b.top || a.top >= b.bottom);
      const nav = document.querySelectorAll('.nav button');
      const navRects = [...nav].map(b => b.getBoundingClientRect());
      return {
        nodeAlert_rect: { x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height) },
        in_viewport: r.left >= 0 && r.top >= 0 && r.right <= innerWidth && r.bottom <= innerHeight,
        overlaps_topbar: hit(r, tb),
        overlaps_sidebar: hit(r, sb),
        nav_buttons_visible: [...nav].filter(b => b.offsetParent !== null).length,
        nav_buttons_total: nav.length,
        nav_in_viewport: navRects.every(q => q.left >= 0 && q.right <= innerWidth + 0.5),
        doc_scrollWidth: document.documentElement.scrollWidth,
        doc_clientWidth: document.documentElement.clientWidth,
        horizontal_overflow: document.documentElement.scrollWidth > document.documentElement.clientWidth,
        active_page: [...document.querySelectorAll('.page.active')].map(p => p.id),
        naMeta: document.querySelector('#naMeta').textContent
      };
    });
    out.shots.push({ viewport: vp, file: path, pageErrors: errs, probe });
    await ctx.close();
  }
  await browser.close();
  fs.writeFileSync('/tmp/c5ui7/shots_f7_result.json', JSON.stringify(out, null, 2));
  console.log(JSON.stringify(out, null, 2));
})().catch(e => { console.error('FATAL', e); process.exit(1); });
