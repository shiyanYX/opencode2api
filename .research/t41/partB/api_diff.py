#!/usr/bin/env python3
"""Part B /api/stats 差异验收（F1 裁决后的收窄口径）。

两种基线，用途不同，不能混：
  --mode isolation  基线 = 同一冻结代码、仅把 F1 过滤中和掉的构建（变异 M5）。
                    只允许「窗口内 OK==0 的模型」从 models/trends_by_model 消失，
                    **total_requests 等一切其它字段逐字面不变**。← F1 的主验收
  --mode e2e        基线 = git HEAD（线上口径：无公式 B、无 everServed、无过滤）。
                    变化 = task-21 未映射口径 + task-39 everServed + F1 过滤 的合集：
                    total_requests 恰好减少 Σ(被删模型请求数)，保留模型数值不变，
                    /api/stats 新增 unmapped_requests 字段（task-21 已批）。
  --selftest        不碰实现：对 old 响应做「合成过滤」，验证比较器本身能判对错。

oracle 不依赖实现：被删集合 = old 的 trends_by_model 中窗口 Σok==0 的模型。
"""
import argparse, collections, hashlib, json, os, shutil, socket, subprocess, sys, time

WORK = os.environ.get('WORK', '/tmp/t41b')
BIN = os.path.join(WORK, 'bin')
RES = os.path.join(WORK, 'results')

ENDPOINTS = [
    ('stats_norange', '/api/stats'),
    ('stats_today', '/api/stats?range=today'),
    ('stats_7d', '/api/stats?range=7d'),
    ('stats_30d', '/api/stats?range=30d'),
    ('stats_180d', '/api/stats?range=180d'),
    ('stats_bad_range', '/api/stats?range=whatever'),
    ('trends_7d_model', '/api/stats/trends?range=7d&group=model'),
    ('trends_180d_model', '/api/stats/trends?range=180d&group=model'),
    ('trends_7d', '/api/stats/trends?range=7d'),
    ('trends_180d', '/api/stats/trends?range=180d'),
    ('calllog_50', '/api/call-log?limit=50'),
]
RANGE_EPS = {'stats_today', 'stats_7d', 'stats_30d', 'stats_180d', 'stats_bad_range'}
# Q1 裁决（Lead 2026-10-20）：按模型趋势端点与 /api/stats 同一判据、同一出口语义——
# 只许「窗口内 OK==0 的模型」的序列消失，其余逐字面相等；且与同窗口 /api/stats 的被删集合一致。
MODEL_TRENDS = {'trends_7d_model': 'stats_7d', 'trends_180d_model': 'stats_180d'}
# 面板热力图分支（非 model 维度）：F1 无对象可删，隔离模式下必须逐字节不变
AGG_TRENDS = {'trends_7d': 'stats_7d', 'trends_180d': 'stats_180d'}
# e2e 模式下允许新增的顶层字段（task-21 已批准）
E2E_NEW_FIELDS = {'unmapped_requests'}


def free_port():
    s = socket.socket(); s.bind(('127.0.0.1', 0)); p = s.getsockname()[1]; s.close(); return p


def tcp_up(port):
    try:
        with socket.create_connection(('127.0.0.1', port), timeout=0.2): return True
    except OSError: return False


def start_server(bin_tag, run_dir):
    port = free_port()
    env = os.environ.copy()
    for k in ('ALL_PROXY', 'all_proxy', 'NO_PROXY', 'no_proxy'): env.pop(k, None)
    for k in ('HTTP_PROXY', 'HTTPS_PROXY', 'http_proxy', 'https_proxy'):
        env[k] = 'http://127.0.0.1:9'   # 黑洞代理：文档/目录抓取确定性失败，判据只用本地缓存
    out = open(os.path.join(run_dir, f'server-{port}.out'), 'wb')
    p = subprocess.Popen([os.path.join(BIN, bin_tag), '-port', str(port),
                          '-config', os.path.join(run_dir, 'config.json'), '-password', '',
                          '-log-file', os.path.join(run_dir, f'server-{port}.log'),
                          '-log-stdout=false'], cwd=run_dir, env=env,
                         stdout=out, stderr=subprocess.STDOUT)
    for _ in range(600):
        if p.poll() is not None:
            raise RuntimeError(f'{bin_tag} 启动即退出: {open(os.path.join(run_dir, f"server-{port}.out")).read()[-600:]}')
        if tcp_up(port): return p, port
        time.sleep(0.05)
    p.kill(); raise RuntimeError(f'{bin_tag} 启动超时')


def stop_server(p):
    p.terminate()
    try: p.wait(timeout=10)
    except subprocess.TimeoutExpired: p.kill(); p.wait()


def get_raw(port, path):
    import urllib.request
    with urllib.request.urlopen(f'http://127.0.0.1:{port}{path}', timeout=300) as r:
        return r.status, r.read()


def capture(bin_tag, case_dir, tag):
    rd = os.path.join(WORK, 'run', f'apidiff-{tag}')
    shutil.rmtree(rd, ignore_errors=True); shutil.copytree(case_dir, rd)
    p, port = start_server(bin_tag, rd)
    hour = time.strftime('%Y%m%d%H')
    bodies = {}
    try:
        _, b1 = get_raw(port, '/api/stats?range=today')
        _, b2 = get_raw(port, '/api/stats?range=today')
        bodies['today_repeat_ok'] = b1 == b2
        for name, path in ENDPOINTS:
            st, body = get_raw(port, path)
            bodies[name] = {'status': st, 'body': body}
    finally:
        stop_server(p)
    return {'hour': hour, 'bodies': bodies, 'run_dir': rd}


def jl(b):
    return json.loads(b.decode('utf-8') if isinstance(b, bytes) else b,
                      parse_float=str, parse_int=str)


def as_int(x): return int(x)


def ok_sums(resp):
    c = collections.Counter()
    for pt in resp['trends_by_model']:
        c[pt['model']] += as_int(pt['ok'])
    return c


def diff_endpoints(old_cap, new_cap):
    """返回 isolated 输出里那些「除批准范围外发生字节变化」的端点。"""
    bad = []
    for name, _ in ENDPOINTS:
        if name in RANGE_EPS:
            continue
        if old_cap['bodies'][name]['body'] != new_cap['bodies'][name]['body']:
            bad.append(name)
    return bad


def compare(old_body, new_body, mode, ck, name, panel_windows=None):
    old, new = jl(old_body), jl(new_body)
    oks = ok_sums(old)
    removed = sorted(m for m in old['models'] if oks[m] == 0)
    kept = sorted(m for m in old['models'] if oks[m] != 0)

    def eq(cond, msg):
        if not cond: ck['problems'].append(f'{name}: {msg}')

    eq(sorted(new['models'].keys()) == kept,
       f"models 键集不符 多={sorted(set(new['models']) - set(kept))} 少={sorted(set(kept) - set(new['models']))}")
    for m in kept:
        if m in new['models']:
            eq(new['models'][m] == old['models'][m], f'保留模型 {m} 的数值被改动')
    exp_trends = [pt for pt in old['trends_by_model'] if pt['model'] not in removed]
    eq(new['trends_by_model'] == exp_trends, 'trends_by_model 不是「仅删除零成功模型」的结果')

    old_total, new_total = as_int(old['total_requests']), as_int(new['total_requests'])
    removed_req = sum(as_int(old['models'][m]['request_count']) for m in removed)
    if mode == 'isolation':
        eq(new_total == old_total, f'total_requests 被改动 {old_total} → {new_total}（隔离模式必须不变）')
        exp_sigma = old_total - removed_req
    else:  # e2e：total 只减少「被 task-21 判为未映射」的那部分；F1 删掉的 15 条仍在分母里
        ub = set((new.get('unmapped_requests') or {}).get('by_model') or {})
        ub.discard('__other__')
        exp_drop = sum(as_int(old['models'][m]['request_count']) for m in removed if m in ub)
        eq(new_total == old_total - exp_drop,
           f'total_requests 应减少未映射请求数 {exp_drop}（{old_total}→{old_total - exp_drop}），实际 {new_total}')
        eq(exp_drop == removed_req - sum(as_int(old['models'][m]['request_count'])
                                         for m in removed if m not in ub),
           'F1 保留在分母里的零成功模型请求数算错（内部一致性）')
        label0 = {'stats_7d': '7d(日历日)', 'stats_30d': '30d(日历日)',
                  'stats_180d': '180d(日历日)'}.get(name)
        if panel_windows and label0 and panel_windows.get(label0, {}).get('excluded'):
            ex_sum = sum(panel_windows[label0]['excluded'].values())
            eq(exp_drop == ex_sum, f'total 减少量 {exp_drop} 与 Part A 独立算出的排除量 {ex_sum} 不符')
        exp_sigma = new_total - (removed_req - exp_drop)
    sigma = sum(as_int(v['request_count']) for v in new['models'].values())
    eq(sigma == exp_sigma, f'Σmodels.request_count={sigma} 期望 {exp_sigma}')

    # 其它顶层字段
    for k in old:
        if k in ('models', 'trends_by_model', 'total_requests'):
            continue
        if mode == 'e2e' and k in E2E_NEW_FIELDS:
            continue
        eq(k in new and new[k] == old[k], f'顶层字段 {k} 被改动/消失')
    for k in new:
        if k in old:
            continue
        eq(mode == 'e2e' and k in E2E_NEW_FIELDS, f'新增了未批准的顶层字段 {k}')

    detail = []
    for m in removed:
        st = old['models'][m]
        tok = sum(as_int(st.get(k, '0')) for k in
                  ('prompt_tokens', 'completion_tokens', 'cache_read_tokens', 'cache_created_tokens'))
        detail.append({'model': m, 'requests': as_int(st['request_count']), 'ok': oks[m],
                       'prompt': as_int(st.get('prompt_tokens', '0')),
                       'completion': as_int(st.get('completion_tokens', '0')),
                       'token_sum': tok,
                       'stream_req': as_int(st.get('stream_req_count', '0')),
                       'trend_points': sum(1 for pt in old['trends_by_model'] if pt['model'] == m)})
        eq(tok == 0, f'被删模型 {m} token 影响非 0（{tok}）')
        eq(as_int(st.get('stream_req_count', '0')) == 0, f'被删模型 {m} stream_req_count 非 0')

    label = {'stats_7d': '7d(日历日)', 'stats_30d': '30d(日历日)',
             'stats_180d': '180d(日历日)'}.get(name)
    if panel_windows and label and label in panel_windows:
        exp = sorted(panel_windows[label]['zero_ok_kept'])
        eq(removed == exp, f'被删集合与 Part A 独立算出的零成功集合不一致: {removed} vs {exp}')

    return {'removed': removed, 'removed_detail': detail, 'kept_n': len(kept),
            'old_total': old_total, 'new_total': new_total, 'removed_requests': removed_req,
            'dropped_from_total': (exp_drop if mode != 'isolation' else 0),
            'f1_kept_requests': (removed_req - exp_drop if mode != 'isolation' else removed_req)}


def compare_model_trends(old_body, new_body, ck, name, expected_removed=None):
    old, new = jl(old_body), jl(new_body)
    oks = collections.Counter()
    for pt in old:
        oks[pt['model']] += as_int(pt['ok'])
    all_models = list(dict.fromkeys(pt['model'] for pt in old))
    removed = sorted(m for m in all_models if oks[m] == 0)
    kept = [m for m in all_models if oks[m] != 0]
    exp = [pt for pt in old if pt['model'] not in removed]
    if new != exp:
        if sorted(new, key=lambda p: (p['model'], p['ts'])) == \
           sorted(exp, key=lambda p: (p['model'], p['ts'])):
            ck['problems'].append(f'{name}: 仅序列顺序变化（内容集合正确但顺序变了）')
        else:
            nm = sorted({p['model'] for p in new})
            ck['problems'].append(f'{name}: 不是「仅删除零成功模型序列」的结果（现 {len(nm)} 个模型 / 期望 {len(kept)} 个，差集={sorted(set(nm) - set(kept))[:6]}）')
    if kept:
        new_models = {p['model'] for p in new}
        if set(kept) - new_models:
            ck['problems'].append(f'{name}: 有成功模型在趋势出口消失 {sorted(set(kept) - new_models)[:6]}')
        if new_models - set(kept):
            ck['problems'].append(f'{name}: 出现不该存在的模型 {sorted(new_models - set(kept))[:6]}')
        ck['notes'].append(f'{name}: 双向模型集一致（{len(kept)} 个：保留都在、无多余）')
    if expected_removed is not None and sorted(expected_removed) != removed:
        ck['problems'].append(f'{name}: 被删集合与 /api/stats 同窗口不一致 {removed} vs {sorted(expected_removed)}（同一判据必须同一出口语义）')
    return {'removed': removed, 'series_n': len(all_models), 'kept_n': len(kept)}


def synth_model_trends(old_body):
    """按 oracle 合成「正确过滤」的按模型趋势序列（比较器自检用）。"""
    old = json.loads(old_body.decode())
    oks = collections.Counter()
    for pt in old:
        oks[pt['model']] += pt['ok']
    removed = {m for m in {pt['model'] for pt in old} if oks[m] == 0}
    return json.dumps([pt for pt in old if pt['model'] not in removed],
                      ensure_ascii=False).encode()


def synth_filtered(old_body):
    """按 oracle 合成「正确实现 F1 过滤」后的响应（用于比较器自检）。"""
    d = json.loads(old_body.decode())
    oks = collections.Counter()
    for pt in d['trends_by_model']:
        oks[pt['model']] += pt['ok']
    removed = {m for m in d['models'] if oks[m] == 0}
    d['models'] = {k: v for k, v in d['models'].items() if k not in removed}
    d['trends_by_model'] = [p for p in d['trends_by_model'] if p['model'] not in removed]
    return json.dumps(d, ensure_ascii=False).encode()


def synth_e2e(old_body):
    """合成「隔离过滤 + total_requests 收缩 + 新增 unmapped_requests」的 e2e 期望响应。"""
    d = json.loads(old_body.decode())
    oks = collections.Counter()
    for pt in d['trends_by_model']:
        oks[pt['model']] += pt['ok']
    removed = {m for m in d['models'] if oks[m] == 0}
    shrunk = sum(d['models'][m]['request_count'] for m in removed)
    d['models'] = {k: v for k, v in d['models'].items() if k not in removed}
    d['trends_by_model'] = [p for p in d['trends_by_model'] if p['model'] not in removed]
    d['total_requests'] -= shrunk
    d['unmapped_requests'] = {'count': 0, 'by_model': {}}
    return json.dumps(d, ensure_ascii=False).encode()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--old', default='nofilter')
    ap.add_argument('--new', default='new')
    ap.add_argument('--case', default=os.path.join(WORK, 'fixture', 'orig'))
    ap.add_argument('--mode', choices=['isolation', 'e2e'], default='isolation')
    ap.add_argument('--panel-windows',
                    default='/home/jingyx/project/opencode2api-test/.research/t41/panel_windows.json')
    ap.add_argument('--selftest', action='store_true')
    args = ap.parse_args()
    os.makedirs(RES, exist_ok=True)
    ck = {'problems': [], 'notes': []}

    if args.selftest:
        cap = capture(args.old, args.case, 'selftest')
        problems = []
        for name, _ in ENDPOINTS:
            if name not in RANGE_EPS:
                continue
            body = cap['bodies'][name]['body']
            sub = {'problems': [], 'notes': []}
            compare(body, synth_filtered(body), 'isolation', sub, name + '(合成过滤)')
            problems += sub['problems']
            sub2 = {'problems': []}
            compare(body, body, 'isolation', sub2, name + '(未过滤)')
            if not sub2['problems']:
                problems.append(f'{name}: 比较器未发现「未过滤」——自检失败')
            if name in ('stats_7d', 'stats_180d'):
                e2e_ok = synth_e2e(body)
                sub3 = {'problems': []}
                compare(body, e2e_ok, 'e2e', sub3, name + '(合成 e2e)')
                problems += sub3['problems']
                bad = {'problems': []}
                compare(body, body, 'e2e', bad, name + '(e2e 未变化)')
                if not bad['problems']:
                    problems.append(f'{name}: e2e 比较器未发现「口径未变化」——自检失败')
        tn = cap['bodies']['trends_7d_model']['body']
        sub = {'problems': []}
        compare_model_trends(tn, synth_model_trends(tn), sub, 'trends_7d_model(合成过滤)')
        problems += sub['problems']
        bad = {'problems': []}
        compare_model_trends(tn, tn, bad, 'trends_7d_model(未过滤)')
        if not bad['problems']:
            problems.append('trends_7d_model: 比较器未发现「未过滤」——自检失败')
        json.dump(problems, open(os.path.join(RES, 'api-diff-selftest.json'), 'w'),
                  ensure_ascii=False, indent=1)
        if problems:
            print('❌ 比较器自检不通过：')
            for p in problems: print('  -', p)
            return 1
        print('✅ 比较器自检通过（合成过滤→PASS；未过滤→被检出）')
        return 0

    old = capture(args.old, args.case, 'old')
    new = capture(args.new, args.case, 'new')
    ck['problems'] += [] if old['hour'] == new['hour'] else \
        [f"抓取跨小时（{old['hour']} vs {new['hour']}），today 窗口不可比，请重跑"]
    if not new['bodies']['today_repeat_ok']:
        ck['problems'].append('new 构建同一小时内两次 today 响应不一致（窗口不确定）')

    pw = json.load(open(args.panel_windows)) if os.path.exists(args.panel_windows) else None
    ranges = {}
    for name, _ in ENDPOINTS:
        ob, nb = old['bodies'][name], new['bodies'][name]
        if ob['status'] != 200 or nb['status'] != 200:
            ck['problems'].append(f"{name}: HTTP {ob['status']}/{nb['status']}")
            continue
        if name == 'stats_norange':
            if ob['body'] != nb['body']:
                ck['problems'].append('stats_norange: 无 range 响应发生字节变化（红线，绝对不允许）')
            continue
        if name in RANGE_EPS:
            ranges[name] = compare(ob['body'], nb['body'], args.mode, ck, name,
                                    pw if args.mode == 'isolation' else None)
            continue
        if name in MODEL_TRENDS:
            exp_removed = ranges.get(MODEL_TRENDS[name], {}).get('removed')
            tinfo = compare_model_trends(ob['body'], nb['body'], ck, name, exp_removed)
            ck['notes'].append(f"{name}: 模型序列 {tinfo['series_n']}→{tinfo['kept_n']}，"
                               f"删 {len(tinfo['removed'])} 个={tinfo['removed']}")
            continue
        # 非 range 端点：隔离模式要求逐字节；e2e 模式按语义比较（detail 层新增 unmapped 键是 task-21 已批）
        if args.mode == 'isolation':
            if ob['body'] != nb['body']:
                ck['problems'].append(f'{name}: 不在批准范围内却发生字节变化')
        else:
            if name == 'calllog_50':
                a, b = jl(ob['body']), jl(nb['body'])
                if len(a) != len(b):
                    ck['problems'].append(f'calllog_50: 详细层条数变化 {len(a)}→{len(b)}（详细层必须一条不少）')
                else:
                    for ra, rb in zip(a, b):
                        ra = {k: v for k, v in ra.items() if k != 'unmapped'}
                        rb = {k: v for k, v in rb.items() if k != 'unmapped'}
                        if ra != rb:
                            ck['problems'].append('calllog_50: 详细层记录内容被改动')
                            break
                    ck['notes'].append('calllog_50: 详细层仅多出 unmapped 键（task-21 已批），其余字段一致')
            else:
                # e2e 基线：聚合趋势（非按模型）在 task-21 口径下会少掉被排除的请求，
                # 断言差值 == 同窗口被删模型请求数；形态本身不比。
                a, b = jl(ob['body']), jl(nb['body'])
                sa = sum(as_int(p['requests']) for p in a)
                sb = sum(as_int(p['requests']) for p in b)
                exp_drop = ranges.get(AGG_TRENDS.get(name, 'stats_7d'), {}).get('dropped_from_total')
                if exp_drop is not None and sa - sb != exp_drop:
                    ck['problems'].append(f'{name}: 聚合趋势请求数差 {sa - sb} ≠ 被删请求数 {exp_drop}')
                ck['notes'].append(f'{name}: e2e 聚合趋势请求数 {sa}→{sb}（差 {sa - sb}）')

    st7 = jl(new['bodies']['stats_7d']['body'])
    tr7 = jl(new['bodies']['trends_7d_model']['body'])
    tr_models = sorted({pt['model'] for pt in tr7})
    ck['notes'].append(
        f"Q1 一致性（7d）：/api/stats models={len(st7['models'])} 个；"
        f"/api/stats/trends?group=model 仍含 {len(tr_models)} 个，差集={sorted(set(tr_models) - set(st7['models']))}")

    out = {'mode': args.mode, 'old': args.old, 'new': args.new, 'hour': old['hour'],
           'problems': ck['problems'], 'notes': ck['notes'], 'ranges': ranges,
           'norange_sha_old': hashlib.sha256(old['bodies']['stats_norange']['body']).hexdigest(),
           'norange_sha_new': hashlib.sha256(new['bodies']['stats_norange']['body']).hexdigest()}
    json.dump(out, open(os.path.join(RES, f'api-diff-{args.mode}.json'), 'w'),
              ensure_ascii=False, indent=1)

    print(f"=== /api/stats 差异验收（mode={args.mode}）===")
    print(f"抓取小时 {old['hour']}（同一小时={old['hour'] == new['hour']}）")
    for name, r in ranges.items():
        print(f"\n[{name}] total {r['old_total']}→{r['new_total']} 保留模型 {r['kept_n']} "
              f"删除模型 {len(r['removed'])}（共 {r['removed_requests']} 条请求）")
        for d in r['removed_detail']:
            print(f"    - {d['model']:34s} requests={d['requests']:4d} ok={d['ok']} "
                  f"tokens={d['token_sum']} stream={d['stream_req']} trend_pts={d['trend_points']}")
    for n in ck['notes']: print('NOTE:', n)
    if ck['problems']:
        print('\n❌ 不通过：')
        for p in ck['problems']: print('  -', p)
        return 1
    print('\n✅ 全部断言通过')
    return 0


if __name__ == '__main__':
    sys.exit(main())
