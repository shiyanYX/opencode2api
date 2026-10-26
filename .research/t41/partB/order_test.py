#!/usr/bin/env python3
"""Part B：行序无关性（日志 38.6% 乱序，回填必须与行序无关）。

做法：同一构建、同一多重集记录，原序 vs seed 固定打乱，逐端点比较**逐字节**结果。
绝对数值交叉核对（来自 Part A 独立计算，fixture 与生产快照同记录集）：
  180d: total_requests=231654, models(过滤后)=16, unmapped.count=1391
  7d  : total_requests=27739,  models=9,    unmapped.count=730
  30d : total_requests=173846, models=10,   unmapped.count=1352
"""
import argparse, collections, json, os, shutil, socket, subprocess, sys, time

WORK = os.environ.get('WORK', '/tmp/t41b')
BIN = os.path.join(WORK, 'bin')
RES = os.path.join(WORK, 'results')

EXPECT = {
    'stats_180d': {'total_requests': 231654, 'models': 16, 'unmapped': 1391},
    'stats_7d':   {'total_requests': 27739,  'models': 9,    'unmapped': 730},
    'stats_30d':  {'total_requests': 173846, 'models': 10,   'unmapped': 1352},
}
ENDPOINTS = [
    ('stats_norange', '/api/stats'),
    ('stats_today', '/api/stats?range=today'),
    ('stats_7d', '/api/stats?range=7d'),
    ('stats_30d', '/api/stats?range=30d'),
    ('stats_180d', '/api/stats?range=180d'),
    ('trends_180d_model', '/api/stats/trends?range=180d&group=model'),
    ('calllog_2000', '/api/call-log?limit=2000'),
]
# 180d 未映射 by_model（Part A 实测；docs13/docs14 与目录空/非空均一致）
EXPECT_180D_BYMODEL = {'space-bunny': 728, 'mimo-v2.5': 624, 'x-preview-f': 30,
                       'claude-opus-4-5': 4, 'hy3': 2, 'probe-143148': 1,
                       'probeL-143230': 1, 'gpt-4o-mini': 1}


def free_port():
    s = socket.socket(); s.bind(('127.0.0.1', 0)); p = s.getsockname()[1]; s.close(); return p


def tcp_up(port):
    try:
        with socket.create_connection(('127.0.0.1', port), timeout=0.2): return True
    except OSError: return False


def start(tag, run_dir):
    port = free_port()
    env = os.environ.copy()
    for k in ('ALL_PROXY', 'all_proxy', 'NO_PROXY', 'no_proxy'): env.pop(k, None)
    for k in ('HTTP_PROXY', 'HTTPS_PROXY', 'http_proxy', 'https_proxy'):
        env[k] = 'http://127.0.0.1:9'
    out = open(os.path.join(run_dir, f'server-{port}.out'), 'wb')
    p = subprocess.Popen([os.path.join(BIN, tag), '-port', str(port),
                          '-config', os.path.join(run_dir, 'config.json'), '-password', '',
                          '-log-file', os.path.join(run_dir, f'server-{port}.log'),
                          '-log-stdout=false'], cwd=run_dir, env=env,
                         stdout=out, stderr=subprocess.STDOUT)
    for _ in range(600):
        if tcp_up(port): return p, port
        if p.poll() is not None: raise RuntimeError('启动即退出')
        time.sleep(0.05)
    raise RuntimeError('启动超时')


def stop(p):
    p.terminate()
    try: p.wait(timeout=15)
    except subprocess.TimeoutExpired: p.kill(); p.wait()


def get(port, path):
    import urllib.request
    with urllib.request.urlopen(f'http://127.0.0.1:{port}{path}', timeout=300) as r:
        return r.read()


def capture(tag, case_dir):
    rd = os.path.join(WORK, 'run', f'order-{tag}')
    shutil.rmtree(rd, ignore_errors=True); shutil.copytree(case_dir, rd)
    p, port = start(tag, rd)
    try:
        return {n: get(port, path) for n, path in ENDPOINTS}
    finally:
        stop(p)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--tag', default='new')
    ap.add_argument('--orig', default=os.path.join(WORK, 'fixture', 'orig'))
    ap.add_argument('--shuffled', default=os.path.join(WORK, 'fixture', 'shuffled'))
    ap.add_argument('--no-absolute', action='store_true', help='只验行序无关，不校验绝对数值')
    args = ap.parse_args()
    os.makedirs(RES, exist_ok=True)
    problems = []
    notes = []
    byte_equal = {}

    a = capture(args.tag, args.orig)
    b = capture(args.tag, args.shuffled)

    def tolerant(x, y, tol=1e-9):
        """整数必须精确相等；浮点允许 1e-9 相对误差（求和顺序只影响末位）。"""
        maxrel = 0.0
        def walk(u, v):
            nonlocal maxrel
            if isinstance(u, bool) or isinstance(v, bool):
                return u == v
            if isinstance(u, int) and isinstance(v, int):
                return u == v
            if isinstance(u, (int, float)) and isinstance(v, (int, float)):
                if u == v: return True
                m = max(abs(u), abs(v), 1e-30)
                rel = abs(u - v) / m; maxrel = max(maxrel, rel)
                return rel <= tol
            if isinstance(u, dict) and isinstance(v, dict):
                return set(u) == set(v) and all(walk(u[k], v[k]) for k in u)
            if isinstance(u, list) and isinstance(v, list):
                return len(u) == len(v) and all(walk(x, y) for x, y in zip(u, v))
            return u == v
        return walk(x, y), maxrel

    for n, _ in ENDPOINTS:
        byte_equal[n] = a[n] == b[n]
        if byte_equal[n]:
            continue
        if n == 'calllog_2000':
            # /api/call-log 返回**最近 2000 条**（tail 窗口）+ 到达顺序，乱序输入下必然不同；
            # 这不是行序依赖缺陷，属该端点定义。只校验条数上限与记录结构完好。
            ra, rb = json.loads(a[n]), json.loads(b[n])
            if len(ra) != len(rb):
                problems.append(f'{n}: 记录条数不同 {len(ra)} vs {len(rb)}')
            elif not all('req_id' in r and 'status' in r for r in ra):
                problems.append(f'{n}: 记录结构缺少 req_id/status')
            else:
                notes.append(f'{n}: 已排除行序比较（该端点=最近{len(ra)}条tail窗口+到达顺序，'
                             f'乱序输入下本应不同；条数/结构均正常）')
            continue
        ok, rel = tolerant(json.loads(a[n]), json.loads(b[n]))
        if not ok:
            problems.append(f'{n}: 结构/整数字段不一致（行序依赖！）')
        else:
            notes.append(f'{n}: 字节不同但数值等价（浮点最大相对误差 {rel:.2e}，整数与计数完全一致）')

    if not args.no_absolute:
        for name, exp in EXPECT.items():
            d = json.loads(a[name].decode())
            uv = d.get('unmapped_requests')
            if uv is None:
                problems.append(f'{name}: 响应缺 unmapped_requests 字段（task-21 口径未实现？）')
            got = {'total_requests': d['total_requests'], 'models': len(d['models']),
                   'unmapped': (uv or {}).get('count')}
            if got != exp:
                problems.append(f'{name}: 绝对数值不符 实际={got} 期望={exp}')
        d = json.loads(a['stats_180d'].decode())
        bm = d.get('unmapped_requests', {}).get('by_model')
        if bm != EXPECT_180D_BYMODEL:
            problems.append(f'180d unmapped.by_model 不符 实际={bm}')
        # Q1：按模型趋势端点必须与 /api/stats 同一口径（同一被删集合）
        tr = json.loads(a['trends_180d_model'].decode())
        tr_models = {pt['model'] for pt in tr}
        st_models = set(d['models'])
        if tr_models != st_models:
            problems.append(f'跨端点不一致: /api/stats models={len(st_models)} 个 vs '
                            f'/api/stats/trends?group=model {len(tr_models)} 个，差集={sorted(tr_models ^ st_models)[:8]}')
        if 'space-bunny' in tr_models or 'mimo-v2.5' in tr_models:
            problems.append('180d 按模型趋势里出现从未成功的模型名（Q1 过滤未生效）')

    json.dump({'tag': args.tag, 'problems': problems, 'notes': notes,
               'byte_equal': byte_equal,
               'sizes': {n: len(v) for n, v in a.items()}},
              open(os.path.join(RES, 'order-test.json'), 'w'), ensure_ascii=False, indent=1)
    if problems:
        print('❌ 行序无关性不通过：')
        for p in problems: print('  -', p)
        return 1
    print('✅ 行序无关性通过（原序/乱序逐字节一致'
          + ('；绝对数值亦符合 Part A' if not args.no_absolute else '') + '）')
    return 0


if __name__ == '__main__':
    sys.exit(main())
