/* task-24 截图：窄屏整页（fullPage）+ 趋势卡局部，证明 390/320 无横向溢出、select 未被隐藏。
   输出到 docs/mockups/u2/。 */
const { chromium } = require('/home/jingyx/.npm/_npx/e41f203b7505f1fb/node_modules/playwright');
const fs = require('fs');
const EXE = '/home/jingyx/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome';
const BASE = process.env.BASE || 'http://127.0.0.1:9056/';
const DIR = process.env.DIR || '/home/jingyx/project/opencode2api-test/docs/mockups/u2';
const ACTIVE = {
  active: true, reason: '节点出口自 21:41:14 起已连续失败 10 次、跨 19m31s，期间零成功',
  since: '2026-10-19T21:41:14+08:00', window_minutes: 10, window_attempts: 4, window_successes: 1,
  window_success_rate: 0.25, run_failures: 10, run_minutes: 19.5, pool_available: 115, pool_total: 227
};
fs.writeFileSync('/tmp/c5ui7/alert.json', JSON.stringify(ACTIVE));

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: EXE });
  const out = { base: BASE, shots: [] };
  for (const vp of [{ w: 390, h: 844 }, { w: 320, h: 568 }, { w: 1440, h: 900 }]) {
    const ctx = await browser.newContext({ viewport: { width: vp.w, height: vp.h } });
    const page = await ctx.newPage();
    await page.goto(BASE, { waitUntil: 'domcontentloaded' });
    await page.waitForTimeout(1800);
    const tag = `${vp.w}x${vp.h}`;
    // 整页（窄屏用 fullPage 才能看到趋势卡筛选栏）
    const full = `${DIR}/f7-overflow-${tag}.png`;
    await page.screenshot({ path: full, fullPage: true });
    // 趋势卡局部
    const trend = page.locator('[data-od-id="trends"]');
    const local = `${DIR}/f7-trendbar-${tag}.png`;
    await trend.screenshot({ path: local });
    const probe = await page.evaluate(() => {
      const cw = document.documentElement.clientWidth;
      const r = (id) => { const e = document.getElementById(id); const b = e.getBoundingClientRect(); return { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height), right: Math.round(b.right) }; };
      return {
        clientWidth: cw, scrollWidth: document.documentElement.scrollWidth,
        overflow: document.documentElement.scrollWidth > cw,
        trendRangeSel: r('trendRangeSel'), trendModelSel: r('trendModelSel'),
        dy: Math.abs(r('trendRangeSel').y - r('trendModelSel').y)
      };
    });
    out.shots.push({ viewport: vp, full, trend, probe });
    await ctx.close();
  }
  await browser.close();
  fs.writeFileSync('/tmp/c5ui7/shots_task24.json', JSON.stringify(out, null, 2));
  console.log(JSON.stringify(out, null, 2));
})().catch(e => { console.error('FATAL', e); process.exit(1); });
