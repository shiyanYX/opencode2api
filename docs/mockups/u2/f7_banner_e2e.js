/* task-19 验收套件：task-14 的 a)~f) + 新增 g) 全局可见 / g2) 任意页可见 / h) 隐藏不占位。
   （RED 阶段先用它跑未改造的 admin.html：g/g2 必须 FAIL。）
   环境变量：BASE OUT TAG ECHARTS_FULFILL=1（隔离不可达 CDN 造成的非页面噪声） */
const { chromium } = require('/home/jingyx/.npm/_npx/e41f203b7505f1fb/node_modules/playwright');
const fs = require('fs');

const EXE = '/home/jingyx/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome';
const STATE = '/tmp/c5ui7/alert.json';
const BASE = process.env.BASE || 'http://127.0.0.1:9056/';
const OUT = process.env.OUT || '/tmp/c5ui7/res_v3.json';
const TAG = process.env.TAG || 'v3';
const VP = { width: parseInt(process.env.VW || '1440', 10), height: parseInt(process.env.VH || '900', 10) };

const ACTIVE = {
  active: true,
  reason: '节点出口自 21:41:14 起已连续失败 10 次、跨 19m31s，期间零成功',
  since: '2026-10-19T21:41:14+08:00',
  window_minutes: 10, window_attempts: 4, window_successes: 1,
  window_success_rate: 0.25,
  run_failures: 10, run_minutes: 19.5,
  pool_available: 115, pool_total: 227
};
const NULLRATE = Object.assign({}, ACTIVE, { window_success_rate: null, window_successes: 0, window_attempts: 0 });
const INACTIVE = Object.assign({}, ACTIVE, { active: false, reason: '', since: '' });
const setState = o => fs.writeFileSync(STATE, JSON.stringify(o));
const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  const R = { tag: TAG, base: BASE, viewport: VP, start: new Date().toISOString() };
  setState(ACTIVE);

  const browser = await chromium.launch({ headless: true, executablePath: EXE });
  const ctx = await browser.newContext({ viewport: VP });
  const page = await ctx.newPage();
  const pageErrors = [], consoleErrors = [], consoleAll = [], reqFailed = [], nodesReq = [];
  page.on('pageerror', e => pageErrors.push(String((e && e.message) || e)));
  page.on('console', m => {
    consoleAll.push({ type: m.type(), text: m.text(), loc: (m.location() || {}).url || '' });
    if (m.type() === 'error') consoleErrors.push({ text: m.text(), loc: (m.location() || {}).url || '' });
  });
  page.on('requestfailed', r => reqFailed.push({ url: r.url(), err: r.failure() && r.failure().errorText }));
  page.on('request', r => { if (r.url().includes('/api/nodes')) nodesReq.push({ t: Date.now(), method: r.method() }); });

  if (process.env.ECHARTS_FULFILL === '1') {
    await page.route('**/cdn.jsdelivr.net/**', route => route.fulfill({
      status: 200, contentType: 'application/javascript',
      body: '/* harness: CDN 在本环境不可达，用空脚本占位 */'
    }));
  }

  const t0 = Date.now();
  await page.goto(BASE, { waitUntil: 'domcontentloaded' });
  R.t_goto_done_ms = Date.now() - t0;

  /* 通用：元素可见性 / 视口内 / 是否与顶栏·侧栏重叠
     注意必须是真函数，不能传字符串 —— page.evaluate(string) 按「表达式」求值，
     返回函数对象时不可序列化，会静默得到 undefined。 */
  const VIS = () => {
    const e = document.querySelector('#nodeAlert');
    if (!e) return { exists:false };
    const r = e.getBoundingClientRect();
    const vw = innerWidth, vh = innerHeight;
    const hit = (a,b) => !(a.right<=b.left||a.left>=b.right||a.bottom<=b.top||a.top>=b.bottom);
    const topbar = document.querySelector('.topbar');
    const sidebar = document.querySelector('.sidebar');
    const tb = topbar ? topbar.getBoundingClientRect() : null;
    const sb = sidebar ? sidebar.getBoundingClientRect() : null;
    return {
      exists:true,
      classes:e.className,
      closest_page:(e.closest('.page')||{}).id||null,
      display:getComputedStyle(e).display,
      visibility:getComputedStyle(e).visibility,
      offsetParent_null:e.offsetParent===null,
      rect:{x:Math.round(r.x),y:Math.round(r.y),w:Math.round(r.width),h:Math.round(r.height)},
      viewport:{w:vw,h:vh},
      zero_size: r.width===0 && r.height===0,
      in_viewport: r.width>0 && r.height>0 && r.left>=0 && r.top>=0 && r.right<=vw && r.bottom<=vh,
      overlaps_topbar: tb ? hit(r,tb) : null,
      overlaps_sidebar: sb ? hit(r,sb) : null,
      active_page:[...document.querySelectorAll('.page.active')].map(p=>p.id),
      scrollY: Math.round(scrollY),
      doc_scrollWidth: document.documentElement.scrollWidth,
      doc_clientWidth: document.documentElement.clientWidth
    };
  };

  /* ---- (a) 零点击，横幅自己亮 ---- */
  let aErr = null;
  try {
    await page.waitForFunction(
      () => { const e = document.querySelector('#nodeAlert'); return !!e && e.classList.contains('on'); },
      null, { timeout: 10000 });
  } catch (e) { aErr = String(e.message); }
  R.a = Object.assign({
    elapsed_ms_from_navigation_start: Date.now() - t0,
    clicks_performed: 0,
    error: aErr
  }, await page.evaluate(VIS));

  /* ---- (b) #naMeta 文案 ---- */
  R.b = {
    naMeta_raw: await page.evaluate(() => document.querySelector('#naMeta').textContent),
    naReason_raw: await page.evaluate(() => document.querySelector('#naReason').textContent),
    has_lianxu_10: await page.evaluate(() => document.querySelector('#naMeta').textContent.includes('连续失败 10 次')),
    has_kua_19_5: await page.evaluate(() => document.querySelector('#naMeta').textContent.includes('跨 19.5 分钟')),
    has_pool_115_227: await page.evaluate(() => document.querySelector('#naMeta').textContent.includes('池内可用 115 / 227')),
  };
  R.auto_refresh_sel_value = await page.evaluate(() => {
    const el = document.querySelector('#autoRefreshSel'); return el ? el.value : '(missing)';
  });

  /* ---- (g) 停在概览页、不点导航，横幅肉眼可见且在视口内 ---- */
  R.g = Object.assign({
    note: '默认首页 = 概览；本段全程无 page.click()',
    active_page_before: await page.evaluate(() => [...document.querySelectorAll('.page.active')].map(p => p.id))
  }, await page.evaluate(VIS));

  /* ---- (g2) 五个页面切过去都要看得到（切页用 showPage()，与导航按钮同一函数） ---- */
  R.g2 = {};
  for (const p of ['overview', 'nodes', 'subs', 'misc', 'logs']) {
    R.g2[p] = await page.evaluate((pg) => { showPage(pg); return null; }, p)
      .then(() => page.evaluate(VIS));
    if (p === 'logs') await sleep(600);   // initLogs 建 SSE，给它一点时间，别把噪声算成布局问题
  }
  await page.evaluate(() => showPage('overview'));
  await sleep(200);

  /* ---- a2 补充：跳到节点池时横幅已是最新，且不触发 loadNodes ---- */
  await page.waitForFunction(() => document.querySelectorAll('#nodeTable tbody tr').length > 0, null, { timeout: 15000 });
  R.a2 = await page.evaluate(() => {
    window.__lnCalls = 0; window.__rtCalls = 0;
    const o1 = window.renderNodeTable, o2 = window.loadNodes;
    window.renderNodeTable = function () { window.__rtCalls++; return o1.apply(this, arguments); };
    window.loadNodes = function () { window.__lnCalls++; return o2.apply(this, arguments); };
    const before = document.querySelector('#nodeAlert').className;
    showPage('nodes');
    const e = document.querySelector('#nodeAlert');
    const r = e.getBoundingClientRect();
    const out = {
      classes_before_switch: before, classes_after_switch: e.className,
      visible_after_switch: e.offsetParent !== null && r.height > 0,
      rect_after_switch: { x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height) },
      loadNodes_calls_from_switch: window.__lnCalls,
      naMeta_after_switch: document.querySelector('#naMeta').textContent
    };
    showPage('overview');
    return out;
  });
  R.d_marker_setup = await page.evaluate(() => {
    const rows = [...document.querySelectorAll('#nodeTable tbody tr')];
    rows[0].dataset.marker = '1';
    rows[0].dataset.markerText = rows[0].textContent.slice(0, 60);
    return { rows: rows.length, markerRowText: rows[0].dataset.markerText, rtCallsAtSetup: window.__rtCalls, lnCallsAtSetup: window.__lnCalls };
  });

  /* ---- (e) window_success_rate=null ⇒ 「无数据」而不是 0% ---- */
  setState(NULLRATE);
  const eFlip = Date.now();
  let eErr = null;
  try {
    await page.waitForFunction(
      () => (document.querySelector('#naMeta') || {}).textContent.includes('无数据'),
      null, { timeout: 45000 });
  } catch (e) { eErr = String(e.message); }
  R.e = {
    flip_to_null_rate_at: new Date(eFlip).toISOString(),
    elapsed_ms_until_rendered: Date.now() - eFlip,
    naMeta_raw: await page.evaluate(() => document.querySelector('#naMeta').textContent),
    contains_wu_shu_ju: await page.evaluate(() => document.querySelector('#naMeta').textContent.includes('无数据')),
    contains_0_pct: await page.evaluate(() => /(^|[^0-9.])0(\.0)?%/.test(document.querySelector('#naMeta').textContent)),
    nodeAlert_still_on: await page.evaluate(() => document.querySelector('#nodeAlert').classList.contains('on')),
    error: eErr
  };

  /* ---- (c) active=false，零操作，等 ≥31s，横幅自己消失 ---- */
  setState(INACTIVE);
  const cFlip = Date.now();
  R.c = { flip_to_inactive_at: new Date(cFlip).toISOString() };
  let cErr = null;
  try {
    await page.waitForFunction(
      () => !document.querySelector('#nodeAlert').classList.contains('on'),
      null, { timeout: 45000 });
  } catch (e) { cErr = String(e.message); }
  R.c.elapsed_ms_until_hidden = Date.now() - cFlip;
  const remain = 31000 - (Date.now() - cFlip);
  if (remain > 0) await sleep(remain);
  R.c.waited_ms_total = Date.now() - cFlip;
  R.c.nodeAlert_classes_after_wait = await page.evaluate(() => document.querySelector('#nodeAlert').className);
  R.c.nodeAlert_still_on_after_wait = await page.evaluate(() => document.querySelector('#nodeAlert').classList.contains('on'));
  R.c.error = cErr;

  /* ---- (h) 隐藏时不可见且不占布局高度 ---- */
  R.h = Object.assign({ note: 'active=false 之后（横幅已消失）' }, await page.evaluate(VIS));

  /* ---- (d) 轮询没有重画节点表 ---- */
  R.d = await page.evaluate(() => {
    const m = document.querySelector('#nodeTable tbody tr[data-marker="1"]');
    return {
      marker_row_still_present: !!m,
      marker_row_text_now: m ? m.textContent.slice(0, 60) : null,
      total_rows: document.querySelectorAll('#nodeTable tbody tr').length,
      renderNodeTable_calls_total: window.__rtCalls,
      loadNodes_calls_total: window.__lnCalls
    };
  });
  R.d.marker_row_text_at_setup = R.d_marker_setup.markerRowText;

  /* ---- (f) 错误收集 ---- */
  R.f = {
    pageErrors, pageErrorCount: pageErrors.length,
    consoleErrors, consoleErrorCount: consoleErrors.length,
    consoleErrorTexts: consoleErrors.map(x => x.text),
    requestFailed: reqFailed
  };
  R.console_non_error = consoleAll.filter(x => x.type !== 'error').map(x => x.type + ': ' + x.text);
  R.nodes_requests = nodesReq.map((r, i) => ({
    i, at: new Date(r.t).toISOString(),
    delta_ms_from_prev: i ? r.t - nodesReq[i - 1].t : null
  }));

  R.end = new Date().toISOString();
  R.total_elapsed_ms = Date.now() - t0;
  await browser.close();
  fs.writeFileSync(OUT, JSON.stringify(R, null, 2));
  console.log(JSON.stringify(R, null, 2));
})().catch(e => { console.error('FATAL', e); process.exit(1); });
