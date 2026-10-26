import json, pathlib, urllib.parse
from playwright.sync_api import sync_playwright
ROOT=pathlib.Path('.').resolve(); FIX=ROOT/'web'/'fixtures'; BASE='http://127.0.0.1:9310'
ENTRIES=json.loads((FIX/'call_log.json').read_text())
OUT=ROOT/'.research'/'t69'/'evidence'
with sync_playwright() as pw:
    b=pw.chromium.launch(executable_path='/home/jingyx/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome',headless=True,args=['--no-sandbox'])
    ctx=b.new_context(viewport={'width':1440,'height':900},locale='zh-CN')
    p=ctx.new_page()
    def route(rt):
        q=urllib.parse.parse_qs(urllib.parse.urlparse(rt.request.url).query)
        cur=(q.get('cursor') or [''])[0]; idx=int(cur.split('_')[-1]) if cur.startswith('cur_') else 0
        rows=ENTRIES[idx:idx+200]; more=idx+200<len(ENTRIES)
        h={'Content-Type':'application/json'}
        if more: h['X-Call-Log-Next-Cursor']=f'cur_{idx+200}'
        rt.fulfill(status=200,headers=h,body=json.dumps(rows,ensure_ascii=False))
    p.route('**/api/call-log*',route)
    def shot(name):
        p.wait_for_timeout(500); p.screenshot(path=str(OUT/f'{name}.png'),full_page=False); print('shot',name)
    p.goto(BASE+'/#/logs'); p.wait_for_selector('#lgStats .stat .v'); p.wait_for_timeout(500); shot('1440_dark_analysis')
    p.click('#lgTabs button[data-tab="dt"]'); p.wait_for_selector('#lgDtBody tr.lg-row'); shot('1440_dark_detail')
    p.click('#lgTabs button[data-tab="rt"]'); p.wait_for_selector('#lgRtList .logline'); shot('1440_dark_runtime')
    p.click('#lgTabs button[data-tab="an"]'); p.set_viewport_size({'width':390,'height':844}); p.wait_for_timeout(500); shot('390_dark_analysis')
    p.set_viewport_size({'width':320,'height':780}); shot('320_dark_detail') if False else None
    p.set_viewport_size({'width':1440,'height':900}); p.click('#themeBtn'); p.wait_for_timeout(400); shot('1440_light_analysis')
    b.close()
