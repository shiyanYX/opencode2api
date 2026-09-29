#!/usr/bin/env python3
"""
admin 面板的浏览器验证工具（Task 2–7 的唯一验收工具，会真正判 FAIL）。

用法:
    python3 scripts/verify_ui.py <检查名> [--url URL]
    python3 scripts/verify_ui.py <检查名> --url=URL

检查名:
    baseline    main 版基线：页面可加载、五个 #page-* 区块存在、ECharts 已加载、
                统计表有真实数据行（>= 8）、未落入 DEMO 离线兜底、无 JS 错误
    charts      四张 ECharts 图已渲染出 canvas、ECharts 已加载、无 JS 错误
    logs        运行日志页：SSE（/api/logs/stream）连接已建立
    calllog     调用日志页签：容器有真实数据行（不是空态）
    elements    节点池空态 / 配额 7 个输入框 / 「添加订阅源」按钮
    nojserror   无 pageerror、无页面脚本抛出的 console error

退出码:
    0  全部断言 PASS
    1  至少一条断言 FAIL（含浏览器/网络异常导致跑不完）
    2  用法错误（检查名未知、--url 解析不出 URL、缺少检查名）

为什么必须显式指定 executable_path：本机 Playwright Python 包期望 chromium 构建 1228，
而 ~/.cache/ms-playwright 里只有 1232/1234，直接 launch 会报 "Executable doesn't exist"。
浏览器需 --no-sandbox（容器/受限环境）。

为什么必须用 wait_until="domcontentloaded"：日志页常驻 EventSource（/api/logs/stream），
SSE 连接永不结束，wait_until="networkidle" 永远不触发，只会等到 60s 超时。
所以导航用 domcontentloaded，再显式 wait_for_timeout 等 ECharts 与首屏 fetch 落地。
"""
import json
import re
import sys

from playwright.sync_api import sync_playwright

CHROME = "/home/jingyx/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome"
LAUNCH_ARGS = ["--no-sandbox", "--disable-dev-shm-usage"]
DEFAULT_URL = "http://127.0.0.1:8899/"
PAGES = ["overview", "nodes", "subs", "misc", "logs"]
SETTLE_MS = 5000       # 导航后固定等待：ECharts CDN + 首屏 fetch
SHOT_PATH = "/tmp/ui-{name}.png"

# DEMO 离线兜底的标记，取自 static/admin.html 的 DEMO 常量。三个都必须真实命中，
# 否则这张检查就是摆设（历史上的 "12:30:25" 在 admin.html 里 0 命中，永远测不出问题）：
#   hk-01              DEMO 节点指纹，出现在 DEMO 日志消息与调用日志节点链里
#   主订阅             DEMO.cfg.subscriptions[0].name，渲染在 <input value> 里，
#                      innerText 看不到，所以扫文本时必须把输入框 value 也算进去
#   2026-08-12 06:00   DEMO.nodes[3].cooldown_until='2026-08-12T06:00:00Z'，
#                      经 fmtTime()（slice(0,16)）截到分钟后的实际渲染文本
DEMO_MARKS = ["hk-01", "主订阅", "2026-08-12 06:00"]
QUOTA_INPUT_IDS = ["q_error_types", "q_message_kw", "q_max_switches",
                   "q_cooldown_h", "q_cooldown_m", "q_health_interval", "q_health_url"]

# innerText 不含 <input>/<textarea> 的 value，DEMO 订阅名之类只存在于输入框里。
PAGE_TEXT_JS = """
() => {
  const inputs = Array.from(document.querySelectorAll('input,textarea'))
    .map(e => e.value || '').join('\\n');
  return document.body.innerText + '\\n' + inputs;
}
"""


class UsageError(Exception):
    """命令行用法错误 → 退出码 2。"""


# ---- 结果收集 ----

class Check:
    """一次检查的实测值 + 断言结果。"""

    def __init__(self, name):
        self.name = name
        self.data = {}       # 实测值，原样 dump，供人工核对
        self.asserts = []    # {label, expected, actual, ok}

    def m(self, key, value):
        """记录一个实测值并返回它。"""
        self.data[key] = value
        return value

    def a(self, label, expected, actual, ok):
        """记录一条断言。ok 必须是已经算好的布尔值，不要在这里做判断。"""
        self.asserts.append({"label": label, "expected": expected,
                             "actual": actual, "ok": bool(ok)})

    @property
    def failed(self):
        return [x for x in self.asserts if not x["ok"]]


def js_errors(ctx):
    return list(ctx["page_errors"])


def console_js_errors(ctx):
    """页面脚本自己抛出的 console error（location 指向页面 URL）。

    浏览器自己产生的资源加载报错（典型：本机 /favicon.ico 404，admin.html 没有声明
    图标，浏览器仍会自动请求）不算 JS 错误，只作为诊断打印，不参与判定——
    否则工具会因为一个与 Task 2–7 无关的静态资源缺口随机变红。
    """
    page_url = ctx["url"].rstrip("/")
    out = []
    for c in ctx["console"]:
        if c["type"] != "error":
            continue
        text = c["text"]
        loc = (c.get("location") or {}).get("url") or ""
        # 资源加载报错（"Failed to load resource: … 404"）不是 JS 错误：本机
        # /favicon.ico 就 404（admin.html 没声明图标，浏览器仍会自动请求）。
        if "Failed to load resource" in text:
            continue
        # 来自外部资源（CDN 等）的报错也不算本面板的 JS 错误
        if loc and not loc.startswith(page_url):
            continue
        out.append(c)
    return out


def resource_errors(ctx):
    """浏览器层面的资源加载报错（不进断言，只打印，避免被误读成 JS 错误）。"""
    out = []
    for c in ctx["console"]:
        if c["type"] == "error" and "Failed to load resource" in c["text"]:
            out.append(c)
    return out


def switch_page(page, name, wait_ms=700):
    page.evaluate("showPage && showPage('%s')" % name)
    page.wait_for_timeout(wait_ms)


def check_no_page_error(c, ctx, label="JS 错误"):
    errs = c.m(label, js_errors(ctx))
    c.a(label, "空（无未捕获异常）", errs, not errs)
    cerrs = c.m("console error(页面 JS)", [f"{x['text']}" for x in console_js_errors(ctx)])
    c.a("console error(页面 JS)", "空", cerrs, not cerrs)


# ---- 检查实现 ----

def check_baseline(page, ctx, c):
    """Task 2 用：main 版基线。页面可加载、五个页面区块存在、ECharts 已加载、
    统计表有真实数据行、未落入 DEMO 离线兜底、无 JS 错误。

    注意 section 的真实 id 是 page-overview 等，不存在独立的 id="overview"；
    data-od-id="overview" 里含有子串 id="overview"，用 #overview 选择器会误判为 0。
    这里一律用 #page-* 选择器。
    """
    title = c.m("标题", page.title())
    c.a("页面标题", "非空", title, bool(title and title.strip()))

    echarts = c.m("ECharts 已加载", page.evaluate("typeof echarts !== 'undefined'"))
    c.a("ECharts 已加载", "True", echarts, echarts is True)

    blocks = {p: page.eval_on_selector_all(f"#page-{p}", "e=>e.length") for p in PAGES}
    c.m("页面区块 #page-*", blocks)
    for p, n in blocks.items():
        c.a(f"页面区块 #page-{p}", ">= 1", n, n >= 1)

    # 真实数据判别：真实后端下 /api/stats 有 8 个模型，明细表 = 8 个模型行 + 1 个「总计」行 = 9；
    # 落入 DEMO 模式时是硬编码的 4 个模型 → 5 行。行数比查字符串可靠。
    rows = c.m("统计表行数", page.eval_on_selector_all("#statsTable tbody tr", "e=>e.length"))
    c.a("统计表行数", ">= 8", rows, rows >= 8)

    demo = c.m("DEMO 模式", page.evaluate(
        "typeof DEMO_MODE !== 'undefined' ? DEMO_MODE : 'undefined'"))
    c.a("DEMO 模式", "False（不是字符串 \"undefined\"）", demo, demo is False)

    # DEMO 标记只在节点/订阅/日志页渲染，必须逐页查，不能只查默认的概览页
    found = {}
    for p in PAGES:
        switch_page(page, p)
        txt = page.evaluate(PAGE_TEXT_JS)
        hit = [m for m in DEMO_MARKS if m in txt]
        if hit:
            found[p] = hit
    c.m("DEMO 残留(逐页)", found)
    c.a("DEMO 残留(逐页)", f"空（不含 {DEMO_MARKS} 任何一个）", found, not found)

    switch_page(page, "overview", 300)
    check_no_page_error(c, ctx)


def check_charts(page, ctx, c):
    """Task 3/4/5/6 用：四张 ECharts 图已渲染（canvas 已生成）、无 JS 错误、ECharts 真加载了。"""
    echarts = c.m("ECharts 已加载", page.evaluate("typeof echarts !== 'undefined'"))
    c.a("ECharts 已加载", "True", echarts, echarts is True)

    for cid in ("trendChart", "heatmapChart", "donutChart"):
        n = c.m(f"{cid} canvas", page.eval_on_selector_all(f"#{cid} canvas", "e=>e.length"))
        c.a(f"{cid} canvas", ">= 1", n, n >= 1)

    # #reqTrendChart 默认 display:none，隐藏容器里 ECharts 画不出 canvas
    # （默认状态下实测就是 0）。所以先切到「请求次数」tab，再断言——
    # 这样断言的是「tab 切过去真的能画出图」，而不是「一个看不见的容器里有没有 canvas」。
    hidden = c.m("reqTrendChart 默认 display",
                 page.eval_on_selector("#reqTrendChart", "e=>e.style.display"))
    c.m("reqTrendChart 默认 canvas", page.eval_on_selector_all(
        "#reqTrendChart canvas", "e=>e.length"))
    page.evaluate("switchTrendTab && switchTrendTab('req', document.getElementById('trendTabReq'))")
    page.wait_for_timeout(2000)
    disp = c.m("reqTrendChart 切 tab 后 display",
               page.eval_on_selector("#reqTrendChart", "e=>e.style.display"))
    n = c.m("reqTrendChart canvas(切到请求次数 tab)", page.eval_on_selector_all(
        "#reqTrendChart canvas", "e=>e.length"))
    c.a("reqTrendChart 切 tab 后 display", "非 none", disp, disp != "none")
    c.a("reqTrendChart canvas(切到请求次数 tab)", ">= 1", n, n >= 1)

    check_no_page_error(c, ctx)


def check_logs(page, ctx, c):
    """运行日志页：切到 logs 页会触发 initLogs() 建立 EventSource(/api/logs/stream)。
    SSE 建连失败不产生 pageerror，所以必须显式断言连接已建立。
    """
    switch_page(page, "logs", 2500)
    status = c.m("logStatus 文本", page.eval_on_selector("#logStatus", "e=>e.textContent.trim()"))
    # onLogEntry() 会把「已连接」覆盖成「实时 · seq N」，所以两种文案都算连上了；
    # 真正的失败信号是 onerror 写入的「连接中断，重连中…」。
    connected = ("已连接" in status) or ("实时" in status)
    c.a("SSE 状态文案", "含「已连接」或「实时 · seq N」，且不含「连接中断」",
        status, connected and "连接中断" not in status)

    ready = c.m("EventSource readyState", page.evaluate("logES ? logES.readyState : null"))
    c.a("EventSource /api/logs/stream", "readyState == 1 (OPEN)", ready, ready == 1)

    lines = c.m("日志行数 #logList .log-line", page.eval_on_selector_all(
        "#logList .log-line", "e=>e.length"))
    c.m("logCount 文案", page.eval_on_selector("#logCount", "e=>e.textContent"))

    check_no_page_error(c, ctx)


def check_calllog(page, ctx, c):
    """调用日志页签（showLogTab('call') → loadCallLog → clRenderList）：必须有真实数据行。"""
    switch_page(page, "logs", 800)
    page.evaluate("showLogTab && showLogTab('call')")
    page.wait_for_timeout(2500)
    total = c.m("调用日志总条数 #clTotal", page.eval_on_selector("#clTotal", "e=>e.textContent"))
    items = c.m("调用日志行数 #clList .cl-item", page.eval_on_selector_all(
        "#clList .cl-item", "e=>e.length"))
    empty = c.m("调用日志空态 .cl-empty", page.eval_on_selector_all(
        "#clList .cl-empty", "e=>e.length"))
    c.a("调用日志总条数", ">= 1", total, total.strip().isdigit() and int(total) >= 1)
    c.a("调用日志行数 #clList .cl-item", ">= 1", items, items >= 1)
    c.a("调用日志空态 .cl-empty", "0（有数据就不该出现空态）", empty, empty == 0)

    check_no_page_error(c, ctx)


def check_elements(page, ctx, c):
    """节点池空态 / 配额 7 个输入框 / 「添加订阅源」按钮。

    注意：配额输入框与「添加订阅源」在 #page-subs（订阅与配额页），
    节点池空态在 #page-nodes（节点池页）——review 里统称「概览页」，实际分属两页。
    """
    switch_page(page, "nodes", 800)
    hints = c.m("节点表空态行 #nodeTable tbody .empty-hint", page.eval_on_selector_all(
        "#nodeTable tbody .empty-hint", "e=>e.length"))
    text = c.m("节点表 tbody 文案", page.eval_on_selector(
        "#nodeTable tbody", "e=>e.innerText.trim()"))
    cnt = c.m("节点总数徽标 #cntAll", page.eval_on_selector("#cntAll", "e=>e.textContent"))
    c.a("节点池空态行", "1 个 .empty-hint", hints, hints == 1)
    c.a("节点池空态文案", "含「暂无节点」", text, "暂无节点" in text)
    c.a("节点总数徽标 #cntAll", "0", cnt, cnt.strip() == "0")

    switch_page(page, "subs", 800)
    vis = {i: page.eval_on_selector(f"#{i}", "e=>e.offsetParent !== null")
           for i in QUOTA_INPUT_IDS}
    c.m("配额输入框可见性", vis)
    for i, v in vis.items():
        c.a(f"配额输入框 #{i}", "存在且可见", v, v is True)

    btn = page.evaluate(
        "() => Array.from(document.querySelectorAll('#page-subs button'))"
        ".filter(b => b.textContent.includes('添加订阅'))"
        ".map(b => ({text: b.textContent.trim(), visible: b.offsetParent !== null}))")
    c.m("「添加订阅」按钮", btn)
    c.a("「添加订阅」按钮", "恰好 1 个且可见", btn, len(btn) == 1 and btn[0]["visible"] is True)

    check_no_page_error(c, ctx)


def check_no_js_error(page, ctx, c):
    """通用：任何任务收尾都能跑一次，确认没有引入 JS 错误。"""
    check_no_page_error(c, ctx)


CHECKS = {
    "baseline": check_baseline,
    "charts": check_charts,
    "logs": check_logs,
    "calllog": check_calllog,
    "elements": check_elements,
    "nojserror": check_no_js_error,
}


# ---- 命令行 ----

def parse_args(argv):
    """同时接受 --url URL 与 --url=URL；解析不出 URL 就报错，绝不静默回落到默认值
    （静默验证了另一个地址，比验证失败更危险）。"""
    check_name = None
    url = DEFAULT_URL
    url_given = False
    i = 0
    while i < len(argv):
        a = argv[i]
        if a == "--url":
            if i + 1 >= len(argv) or argv[i + 1].startswith("--"):
                raise UsageError("--url 缺少 URL；写成 --url http://… 或 --url=http://…")
            url, url_given = argv[i + 1], True
            i += 2
            continue
        if a.startswith("--url="):
            url, url_given = a.split("=", 1)[1], True
            i += 1
            continue
        if a.startswith("-"):
            raise UsageError(f"未知参数: {a}")
        if check_name is None:
            check_name = a
        else:
            raise UsageError(f"多余的位置参数: {a}")
        i += 1
    if check_name is None:
        raise UsageError("缺少检查名")
    if check_name not in CHECKS:
        raise UsageError(f"未知检查名: {check_name}（可用: {'|'.join(CHECKS)}）")
    if url_given:
        url = url.strip()
        if not re.match(r"^https?://\S+$", url):
            raise UsageError(f"--url 不是合法的 http(s) URL: {url!r}")
    return check_name, url


def usage():
    return (f"用法: {sys.argv[0]} <{'|'.join(CHECKS)}> [--url URL]\n"
            f"  退出码: 0=全部 PASS  1=有 FAIL  2=用法错误")


def run_check(check_name, url):
    console, page_errors, failed = [], [], []
    c = Check(check_name)
    with sync_playwright() as p:
        browser = p.chromium.launch(executable_path=CHROME, args=LAUNCH_ARGS)
        try:
            pg = browser.new_page(viewport={"width": 1440, "height": 900})
            pg.on("console", lambda m: console.append(
                {"type": m.type, "text": m.text, "location": m.location}))
            pg.on("pageerror", lambda e: page_errors.append(str(e)))
            pg.on("response", lambda r: failed.append(
                f"{r.status} {r.url}") if r.status >= 400 else None)

            # SSE 常驻，networkidle 永远不触发 → domcontentloaded + 显式等待。
            # 超时给到 60s：head 里那个 ECharts <script> 是同步阻塞的，DOMContentLoaded
            # 得等它，而本机到 cdn.jsdelivr.net 实测要 1.5–5s，偶尔会卡过 30s。
            pg.goto(url, wait_until="domcontentloaded", timeout=60000)
            pg.wait_for_timeout(SETTLE_MS)

            ctx = {"console": console, "page_errors": page_errors,
                   "failed_requests": failed, "url": url}
            CHECKS[check_name](pg, ctx, c)
            c.m("HTTP >=400 请求", failed)
            c.m("console 资源加载报错(非 JS 错误，仅诊断)",
                [f"{x['text']}" for x in resource_errors(ctx)])
            try:
                pg.screenshot(path=SHOT_PATH.format(name=check_name), full_page=True)
            except Exception as e:            # 截图失败不该改判定结论
                print(f"（截图失败: {e}）")
        finally:
            browser.close()
    return c


def report(c, url):
    print(f"=== 检查 {c.name} @ {url} ===")
    print("--- 实测值 ---")
    print(json.dumps(c.data, ensure_ascii=False, indent=2))
    print("--- 断言 ---")
    for a in c.asserts:
        tag = "PASS" if a["ok"] else "FAIL"
        actual = json.dumps(a["actual"], ensure_ascii=False)
        print(f"{tag}  {a['label']}: 期望 {a['expected']}，实测 {actual}")
    print("--- 诊断（不参与判定）---")
    print(f"HTTP >=400 请求: {c.data.get('HTTP >=400 请求') or '无'}")
    res = c.data.get("console 资源加载报错(非 JS 错误，仅诊断)") or []
    print(f"console 资源加载报错: {res if res else '无'}")
    print(f"截图: {SHOT_PATH.format(name=c.name)}")
    total, failed = len(c.asserts), len(c.failed)
    if failed:
        print(f"结论: FAIL（{total - failed}/{total} 通过，{failed} 条失败）→ 退出码 1")
    else:
        print(f"结论: PASS（{total}/{total} 通过）→ 退出码 0")
    return 1 if failed else 0


def main():
    try:
        check_name, url = parse_args(sys.argv[1:])
    except UsageError as e:
        print(f"参数错误: {e}", file=sys.stderr)
        print(usage(), file=sys.stderr)
        return 2
    try:
        c = run_check(check_name, url)
    except Exception as e:
        print(f"=== 检查 {check_name} @ {url} ===")
        print(f"检查无法完成: {type(e).__name__}: {e}")
        print("结论: FAIL（检查没能跑完，不能算通过）→ 退出码 1")
        return 1
    return report(c, url)


if __name__ == "__main__":
    sys.exit(main())
