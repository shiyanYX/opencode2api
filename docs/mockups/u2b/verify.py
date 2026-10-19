import json
from playwright.sync_api import sync_playwright

CHROME="/home/jingyx/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome"
SPEC="file:///home/jingyx/project/opencode2api-test/docs/mockups/u2b/style-specimen.html"
OUT="/home/jingyx/project/opencode2api-test/docs/mockups/u2b"
rep={}

def wire(p):
    d={"pageerror":[], "console_error":[], "requests":[]}
    p.on("pageerror", lambda e: d["pageerror"].append(str(e)))
    p.on("console", lambda m: d["console_error"].append(m.text) if m.type=="error" else None)
    p.on("request", lambda r: d["requests"].append(r.url))
    return d

with sync_playwright() as pw:
    b=pw.chromium.launch(executable_path=CHROME)

    # ---- A. 桌面 1440x900 ----
    p=b.new_page(viewport={"width":1440,"height":900}); d=wire(p)
    p.goto(SPEC, wait_until="load"); p.wait_for_timeout(600)
    rep["A_desktop_1440x900"]={
      "pageerror":d["pageerror"], "console_error":d["console_error"],
      "requests_total":len(d["requests"]),
      "requests_non_file":[u for u in d["requests"] if not u.startswith("file://")],
      "overflow":p.evaluate("()=>({scrollWidth:document.documentElement.scrollWidth,clientWidth:document.documentElement.clientWidth})"),
    }
    p.screenshot(path=f"{OUT}/specimen-desktop-1440x900.png", full_page=True)
    p.screenshot(path=f"{OUT}/specimen-desktop-1440x900-fold.png")
    p.click('#rlist .rchip[data-hl="#nodeCard"]') if False else None
    p.click('a[data-go="nodes"]'); p.wait_for_timeout(400)
    p.screenshot(path=f"{OUT}/specimen-nodes-desktop-1440x900.png", full_page=True)
    p.click("#themeBtn"); p.wait_for_timeout(400)
    p.screenshot(path=f"{OUT}/specimen-dark-nodes-desktop-1440x900.png", full_page=True)
    p.click('a[data-go="overview"]'); p.wait_for_timeout(300)
    p.screenshot(path=f"{OUT}/specimen-dark-desktop-1440x900.png", full_page=True)
    p.close()

    # ---- B. 移动 390x844 ----
    m=b.new_page(viewport={"width":390,"height":844}, device_scale_factor=2); d2=wire(m)
    m.goto(SPEC, wait_until="load"); m.wait_for_timeout(600)
    rep["B_mobile_390x844"]={
      "pageerror":d2["pageerror"], "console_error":d2["console_error"],
      "requests_total":len(d2["requests"]),
      "requests_non_file":[u for u in d2["requests"] if not u.startswith("file://")],
      "overflow_overview":m.evaluate("()=>({scrollWidth:document.documentElement.scrollWidth,clientWidth:document.documentElement.clientWidth})"),
      "elements_wider_than_viewport":m.evaluate("""()=>{const o=[],vw=document.documentElement.clientWidth;
        document.querySelectorAll('*').forEach(el=>{const r=el.getBoundingClientRect();
        if(r.width>vw+1&&r.width>0)o.push({tag:el.tagName,cls:String(el.className).slice(0,50),w:Math.round(r.width)})});return o.slice(0,10)}"""),
    }
    m.screenshot(path=f"{OUT}/specimen-mobile-390x844.png", full_page=True)
    m.click("#mobMenu"); m.wait_for_timeout(250)
    rep["B_mobile_390x844"]["menu_expanded_items_visible"]=m.evaluate("""()=>Array.from(document.querySelectorAll('.nav a.desk')).filter(a=>getComputedStyle(a).display!=='none').length""")
    rep["B_mobile_390x844"]["overflow_after_menu"]=m.evaluate("()=>({scrollWidth:document.documentElement.scrollWidth,clientWidth:document.documentElement.clientWidth})")
    m.screenshot(path=f"{OUT}/specimen-mobile-menu-390x844.png")
    m.click("#mobMenu"); m.wait_for_timeout(200)
    m.click('a[data-go="nodes"]'); m.wait_for_timeout(400)
    rep["B_mobile_390x844"]["overflow_nodes_view"]=m.evaluate("()=>({scrollWidth:document.documentElement.scrollWidth,clientWidth:document.documentElement.clientWidth})")
    m.screenshot(path=f"{OUT}/specimen-nodes-mobile-390x844.png", full_page=True)
    m.close()

    # ---- C. freellm 活实例（只读）1440x900 / 390x844 ----
    fl={}
    for name,vp in [("freellm-live-AUTHGATE-login-1440x900",{"width":1440,"height":900}),
                    ("freellm-live-AUTHGATE-login-390x844",{"width":390,"height":844})]:
        x=b.new_page(viewport=vp); dx=wire(x)
        x.goto("http://127.0.0.1:47996/", wait_until="networkidle", timeout=30000)
        x.wait_for_timeout(1500)
        x.screenshot(path=f"{OUT}/{name}.png")
        fl[name]={"url":x.url, "title":x.title(),
                  "overflow":x.evaluate("()=>({scrollWidth:document.documentElement.scrollWidth,clientWidth:document.documentElement.clientWidth})"),
                  "pageerror":dx["pageerror"]}
        x.close()
    rep["C_freellm_live"]=fl
    b.close()

open("/tmp/u2b_verify_out.json","w").write(json.dumps(rep,ensure_ascii=False,indent=2))
print(json.dumps(rep, ensure_ascii=False, indent=2))
