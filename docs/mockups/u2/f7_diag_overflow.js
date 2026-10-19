/* 390px 横向溢出诊断：找出溢出的元素，并分别测「横幅显示 / 横幅隐藏」的 scrollWidth。 */
const { chromium } = require('/home/jingyx/.npm/_npx/e41f203b7505f1fb/node_modules/playwright');
const fs = require('fs');
const EXE = '/home/jingyx/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome';
const BASE = process.env.BASE || 'http://127.0.0.1:9056/';
const OUT = process.env.OUT || '/tmp/c5ui7/diag_390.json';
const ACTIVE = {
  active: true, reason: '节点出口自 21:41:14 起已连续失败 10 次、跨 19m31s，期间零成功',
  since: '2026-10-19T21:41:14+08:00', window_minutes: 10, window_attempts: 4, window_successes: 1,
  window_success_rate: 0.25, run_failures: 10, run_minutes: 19.5, pool_available: 115, pool_total: 227
};
fs.writeFileSync('/tmp/c5ui7/alert.json', JSON.stringify(ACTIVE));

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: EXE });
  const page = await (await browser.newContext({ viewport: { width: 390, height: 844 } })).newPage();
  await page.goto(BASE, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => document.querySelector('#nodeAlert') && document.querySelector('#nodeAlert').classList.contains('on'), null, { timeout: 15000 });
  await page.waitForTimeout(1500);

  const probe = () => page.evaluate(() => {
    const cw = document.documentElement.clientWidth;
    const off = [];
    document.querySelectorAll('*').forEach(e => {
      const r = e.getBoundingClientRect();
      if (r.width > 0 && r.right > cw + 0.5) {
        off.push({ tag: e.tagName.toLowerCase(), id: e.id || '', cls: (e.className || '').toString().slice(0, 60), right: Math.round(r.right), w: Math.round(r.width) });
      }
    });
    return {
      clientWidth: cw,
      scrollWidth: document.documentElement.scrollWidth,
      overflow: document.documentElement.scrollWidth > cw,
      bodyScrollWidth: document.body.scrollWidth,
      offenders: off.slice(0, 25),
      offenderCount: off.length,
      nodeAlertDisplay: getComputedStyle(document.querySelector('#nodeAlert')).display
    };
  });

  const withBanner = await probe();
  const withoutBanner = await page.evaluate(() => {
    const e = document.querySelector('#nodeAlert'); e.style.display = 'none';
    return null;
  }).then(probe);
  const restyledKeepDom = await page.evaluate(() => {   // 还原，改用 class 去掉 .on
    const e = document.querySelector('#nodeAlert'); e.style.display = '';
    e.classList.remove('on');
    return null;
  }).then(probe);

  // 再试：横幅显示，但让 .na-meta 允许长串换行 / 检查是否 meta 是元凶
  const withBannerAgain = await page.evaluate(() => { document.querySelector('#nodeAlert').classList.add('on'); return null; }).then(probe);

  await browser.close();
  const R = { base: BASE, viewport: { w: 390, h: 844 }, withBanner, withoutBanner, classOff: restyledKeepDom, withBannerAgain };
  fs.writeFileSync(OUT, JSON.stringify(R, null, 2));
  console.log(JSON.stringify(R, null, 2));
})().catch(e => { console.error('FATAL', e); process.exit(1); });
