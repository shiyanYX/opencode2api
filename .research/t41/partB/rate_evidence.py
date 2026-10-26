import json, os, shutil, sys
sys.path.insert(0, '/tmp/t41b'); import api_diff as A
res = {}
for tag in ('nofilter', 'new'):
    rd = os.path.join(A.WORK, 'run', f'rate-{tag}')
    shutil.rmtree(rd, ignore_errors=True); shutil.copytree(os.path.join(A.WORK, 'fixture', 'orig'), rd)
    p, port = A.start_server(tag, rd)
    try:
        out = {}
        for rng in ('7d', '30d', '180d', 'today'):
            st, body = A.get_raw(port, f'/api/stats?range={rng}')
            d = json.loads(body)
            req = d['total_requests']
            ok = sum(pt['ok'] for pt in d['trends_by_model'])
            out[rng] = {'total_requests': req, 'ok': ok, 'rate_pct': round(ok * 100.0 / req, 6) if req else None,
                        'models': len(d['models']), 'unmapped': (d.get('unmapped_requests') or {}).get('count')}
        res[tag] = out
    finally:
        A.stop_server(p)
json.dump(res, open(os.path.join(A.RES, 'rate-evidence.json'), 'w'), ensure_ascii=False, indent=1)
for rng in ('today', '7d', '30d', '180d'):
    a, b = res['nofilter'][rng], res['new'][rng]
    print(f"{rng:6s} nofilter total={a['total_requests']} ok={a['ok']} rate={a['rate_pct']}% models={a['models']} unmapped={a['unmapped']}")
    print(f"       new      total={b['total_requests']} ok={b['ok']} rate={b['rate_pct']}% models={b['models']} unmapped={b['unmapped']}"
          f"  Δrate={'' if a['rate_pct'] is None or b['rate_pct'] is None else round(b['rate_pct']-a['rate_pct'],6)}")
