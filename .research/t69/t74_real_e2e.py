#!/usr/bin/env python3
"""task-74 真二进制 e2e：明细「加载更多」必须走文件游标分页并可达历史。

- 真后端：/tmp/oc2a-panel-bin-74（当前源码 go build，embed 修复后的 static/admin.html）
- 真数据：/tmp/panel-run-74（124 MB / 23 万条 call_log.jsonl 副本）
- 浏览器：Playwright 直连真端口（不是 fixture 拦截）

断言：首屏请求带 status、响应 Source=file + Next-Cursor 非空；点两次加载更多
200→400→600，每页新增与前面零交集；UI 三条并集 == 后端同条件(?status=all)三次查询并集。
"""
import json
import sys
import urllib.parse

from playwright.sync_api import sync_playwright

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:9423"
PW = sys.argv[2] if len(sys.argv) > 2 else "t74pass"
CHROME = "/home/jingyx/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome"

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(("PASS  " if ok else "FAIL  ") + name + (("  " + str(detail)) if detail and not ok else ""))


def main():
    with sync_playwright() as pw:
        b = pw.chromium.launch(executable_path=CHROME, headless=True,
                               args=["--no-sandbox", "--disable-dev-shm-usage"])
        ctx = b.new_context(viewport={"width": 1440, "height": 900}, locale="zh-CN")
        page = ctx.new_page()
        errors, console_errors, call_reqs = [], [], []
        page.on("pageerror", lambda e: errors.append(str(e)))
        page.on("console", lambda m: console_errors.append(m.text) if m.type == "error" else None)

        def on_req(r):
            if "/api/call-log" in r.url:
                call_reqs.append(r.url)
        page.on("request", on_req)

        # 登录（面板密码）
        resp = ctx.request.post(BASE + "/login", form={"password": PW})
        check("真后端登录成功", resp.status in (200, 302), resp.status)

        # 二进制 embed 的文档必须是修复后的产物
        doc = ctx.request.get(BASE + "/")
        html = doc.text()
        check("真二进制嵌入的是修复后 admin.html", "CL_STATUS_PARAM" in html and "status: CL_STATUS_PARAM[S.dt.status]" in html)

        page.goto(BASE + "/#/logs", wait_until="load")
        page.wait_for_selector("#lgTabs", state="visible")
        page.click('#lgTabs button[data-tab="dt"]')
        page.wait_for_function("() => document.querySelectorAll('#lgDtBody tr.lg-row').length >= 200")

        def ids(page):
            return page.locator("#lgDtBody tr.lg-row").evaluate_all("els => els.map(e => e.dataset.req)")

        first_reqs = [u for u in call_reqs if "limit=200" in u and "cursor=" not in u]
        check("首屏明细请求带 status 过滤参数", bool(first_reqs) and "status=all" in first_reqs[0], str(first_reqs[:1]))
        check("首屏加载 200 行", len(ids(page)) == 200, len(ids(page)))
        check("首屏 next_cursor 非空（加载更多可用）", not page.locator("#lgDtMore").is_disabled(),
              page.locator("#lgDtMore").inner_text())
        ids1 = ids(page)
        check("首屏 req_id 无重复", len(ids1) == len(set(ids1)), f"{len(ids1)}/{len(set(ids1))}")

        page.click("#lgDtMore")
        page.wait_for_function("() => document.querySelectorAll('#lgDtBody tr.lg-row').length >= 400")
        ids2 = ids(page)
        check("第一次加载更多 200→400", len(ids2) == 400, len(ids2))
        page2 = ids2[200:]
        check("第二页新增与第一页零交集", len(set(page2) & set(ids1)) == 0, len(set(page2) & set(ids1)))
        check("第二页严格新增 200 条", len(set(page2)) == 200 and len(ids2) == 400, f"{len(set(page2))}/{len(ids2)}")

        page.click("#lgDtMore")
        page.wait_for_function("() => document.querySelectorAll('#lgDtBody tr.lg-row').length >= 600")
        ids3 = ids(page)
        check("第二次加载更多 400→600", len(ids3) == 600, len(ids3))
        page3 = ids3[400:]
        check("第三页新增与前两页零交集", len(set(page3) & set(ids3[:400])) == 0,
              len(set(page3) & set(ids3[:400])))
        check("UI 三次分页并集无重复", len(ids3) == len(set(ids3)), f"{len(ids3)}/{len(set(ids3))}")
        check("仍有更多（23 万条历史可达，按钮仍可用）", not page.locator("#lgDtMore").is_disabled())
        check("计数文案随加载增长", "600" in page.locator("#lgDtCount").inner_text(),
              page.locator("#lgDtCount").inner_text())

        # 后端同条件(status=all)同样三次查询 → 与 UI 并集对账
        be_ids, cursor, source_ok = [], "", True
        for i in range(3):
            qs = "limit=200&status=all" + ("&cursor=" + urllib.parse.quote(cursor) if cursor else "")
            r = ctx.request.get(BASE + "/api/call-log?" + qs)
            hdr = r.headers
            source_ok = source_ok and hdr.get("x-call-log-source") == "file"
            nxt = hdr.get("x-call-log-next-cursor", "")
            check(f"后端第 {i + 1} 次查询走文件分页且返回游标",
                  hdr.get("x-call-log-source") == "file" and bool(nxt),
                  f"source={hdr.get('x-call-log-source')} cursor={nxt[:16]}")
            be_ids += [e.get("req_id") for e in r.json()]
            cursor = nxt
        check("UI 三页并集 == 后端同条件三次查询并集", set(ids3) == set(be_ids),
              f"ui={len(set(ids3))} be={len(set(be_ids))}")
        check("后端三页并集本身无重复", len(be_ids) == len(set(be_ids)), f"{len(be_ids)}/{len(set(be_ids))}")

        # 用户选具体状态时不能绕过筛选：status=fail 请求带 status=fail，且展示全 fail
        call_reqs.clear()
        page.click('#lgDtStatus button[data-status="fail"]')
        # 等待服务端 status=fail 重新分页完成（行全部 fail），而不是抢旧的全量行
        page.wait_for_function(
            """() => { const rs=[...document.querySelectorAll('#lgDtBody tr.lg-row')];
                 return rs.length > 0 && rs.every(r => r.dataset.status === 'fail'); }""")
        fail_reqs = [u for u in call_reqs if "status=fail" in u]
        st = page.locator("#lgDtBody tr.lg-row").evaluate_all("els => els.map(e => e.dataset.status)")
        check("失败档请求带 status=fail（服务端筛选，未绕过）", bool(fail_reqs), str(fail_reqs[-2:]))
        check("失败档展示全部 fail", len(st) > 0 and set(st) == {"fail"}, len(st))

        check("无 pageerror", not errors, str(errors[:3]))
        check("无 console.error", not console_errors, str(console_errors[:3]))
        b.close()

    total = len(results)
    passed = sum(1 for _, ok, _ in results if ok)
    print(f"\n===== {passed}/{total} PASS =====")
    if passed != total:
        print("FAILED:")
        for name, ok, detail in results:
            if not ok:
                print("  -", name, detail)
        sys.exit(1)
    print("ALL_PASS")


if __name__ == "__main__":
    main()
