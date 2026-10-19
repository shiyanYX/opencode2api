/* 探索性探针：390/320 下谁在溢出，以及它的祖先链盒子模型。 */
const { chromium } = require('/home/jingyx/.npm/_npx/e41f203b7505f1fb/node_modules/playwright');
const fs = require('fs');
const EXE = '/home/jingyx/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome';
const BASE = process.env.BASE || 'http://127.0.0.1:9056/';
const ACTIVE = {
  active: true, reason: '节点出口自 21:41:14 起已连续失败 10 次、跨 19m31s，期间零成功',
  since: '2026-10-19T21:41:14+08:00', window_minutes: 10, window_attempts: 4, window_successes: 1,
  window_success_rate: 0.25, run_failures: 10, run_minutes: 19.5, pool_available: 115, pool_total: 227
};
fs.writeFileSync('/tmp/c5ui7/alert.json', JSON.stringify(ACTIVE));

const SNAP = () => {
  const cw = document.documentElement.clientWidth;
  const desc = (e) => {
    const r = e.getBoundingClientRect();
    const cs = getComputedStyle(e);
    return {
      tag: e.tagName.toLowerCase(), id: e.id || '', cls: (e.className || '').toString().slice(0, 40),
      rect: { x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height), right: Math.round(r.right) },
      display: cs.display, flexWrap: cs.flexWrap, flexDirection: cs.flexDirection,
      flex: cs.flex, minWidth: cs.minWidth, width: cs.width, overflowX: cs.overflowX,
      inline: (e.getAttribute('style') || '').slice(0, 120),
      scrollW: e.scrollWidth, clientW: e.clientWidth
    };
  };
  const offenders = [];
  document.querySelectorAll('*').forEach(e => {
    const r = e.getBoundingClientRect();
    if (r.width > 0 && r.right > cw + 0.5) offenders.push(e);
  });
  const out = offenders.slice(0, 8).map(e => {
    const chain = [];
    let n = e;
    for (let i = 0; i < 7 && n && n !== document.documentElement; i++, n = n.parentElement) chain.push(desc(n));
    return { self: desc(e), chain };
  });
  // 趋势卡头部那两个 inline-flex 行的关键宽度
  const hdr = document.querySelector('[data-od-id="trends"] > div:first-child');
  const leftGroup = hdr ? hdr.children[0] : null;
  return {
    clientWidth: cw,
    scrollWidth: document.documentElement.scrollWidth,
    offenders: out,
    header: hdr ? desc(hdr) : null,
    leftGroup: leftGroup ? desc(leftGroup) : null,
    filterBar: (() => { const f = document.querySelector('[data-od-id="trends"] .filter-bar'); return f ? desc(f) : null; })(),
    selects: ['trendRangeSel', 'trendModelSel'].map(id => { const e = document.getElementById(id); return e ? desc(e) : null; })
  };
};

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: EXE });
  const out = {};
  for (const vp of [{ w: 390, h: 844 }, { w: 320, h: 568 }, { w: 1440, h: 900 }]) {
    const ctx = await browser.newContext({ viewport: { width: vp.w, height: vp.h } });
    const page = await ctx.newPage();
    await page.goto(BASE, { waitUntil: 'domcontentloaded' });
    await page.waitForTimeout(1500);
    out[`${vp.w}x${vp.h}`] = await page.evaluate(SNAP);
    await ctx.close();
  }
  await browser.close();
  fs.writeFileSync('/tmp/c5ui7/probe_layout.json', JSON.stringify(out, null, 2));
  for (const [k, v] of Object.entries(out)) {
    console.log(`\n===== ${k} ==== clientWidth=${v.clientWidth} scrollWidth=${v.scrollWidth}`);
    console.log(' header:', JSON.stringify(v.header && { rect: v.header.rect, display: v.header.display, flexWrap: v.header.flexWrap, inline: v.header.inline }));
    console.log(' leftGroup:', JSON.stringify(v.leftGroup && { rect: v.leftGroup.rect, display: v.leftGroup.display, flexWrap: v.leftGroup.flexWrap, minWidth: v.leftGroup.minWidth, inline: v.leftGroup.inline }));
    console.log(' filterBar:', JSON.stringify(v.filterBar && { rect: v.filterBar.rect, display: v.filterBar.display, flexWrap: v.filterBar.flexWrap, minWidth: v.filterBar.minWidth }));
    v.selects.forEach(s => s && console.log(' select', s.id, JSON.stringify({ rect: s.rect, h: s.rect.h })));
    v.offenders.forEach((o, i) => { console.log(` offender[${i}]`, JSON.stringify(o.self)); });
  }
})().catch(e => { console.error('FATAL', e); process.exit(1); });
