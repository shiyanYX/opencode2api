#!/usr/bin/env python3
"""补充证据：两个构建对同一夹具的响应 sha256 对照（无 range / 热力图聚合分支 / 模型趋势分支）。"""
import hashlib, json, os, shutil, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import api_diff as A

PATHS = [('stats_norange', '/api/stats'),
         ('heatmap_180d', '/api/stats/trends?range=180d'),
         ('heatmap_7d', '/api/stats/trends?range=7d'),
         ('model_trends_180d', '/api/stats/trends?range=180d&group=model')]


def cap(tag):
    rd = os.path.join(A.WORK, 'run', f'sha-{tag}')
    shutil.rmtree(rd, ignore_errors=True)
    shutil.copytree(os.path.join(A.WORK, 'fixture', 'orig'), rd)
    p, port = A.start_server(tag, rd)
    try:
        out = {}
        for name, path in PATHS:
            st, body = A.get_raw(port, path)
            out[name] = {'status': st, 'sha256': hashlib.sha256(body).hexdigest(),
                         'bytes': len(body), 'models': None}
            if 'group=model' in path:
                out[name]['models'] = len({pt['model'] for pt in json.loads(body)})
        return out
    finally:
        A.stop_server(p)


res = {t: cap(t) for t in ('nofilter', 'new')}
res['same'] = {n: res['nofilter'][n]['sha256'] == res['new'][n]['sha256'] for n, _ in PATHS}
json.dump(res, open(os.path.join(A.RES, 'sha-evidence.json'), 'w'), ensure_ascii=False, indent=1)
for n, _ in PATHS:
    a, b = res['nofilter'][n], res['new'][n]
    print(f"{n:20s} nofilter={a['sha256'][:16]}… new={b['sha256'][:16]}… "
          f"{'相同' if res['same'][n] else '不同'} bytes={a['bytes']}→{b['bytes']}"
          + (f" models={a['models']}→{b['models']}" if a['models'] else ''))
print('nofilter==new 分支数:', sum(res['same'].values()), '/', len(PATHS))
