#!/usr/bin/env python3
"""Part B：重启时序 + 写入时冻结（everServed 是唯一防线的场景）。

夹具用 nopickle：文档缓存里**没有** big-pickle（目录/网络亦不可用），
所以公式 B 判 unmapped，只有 everServed（日志里 11 条 ok）能救它。

三个探针请求（上游不可达，都会记为 fail）：
  P1 big-pickle  —— 在**任何 /api/stats 查询之前**立刻发（回填还没跑，race 窗口）
  P2 space-bunny —— 从未成功过（日志里 728 条全 fail）→ 必须冻结 unmapped=true
  P3 big-pickle  —— 先查一次 /api/stats?range=180d 强制回填后再发 → 期望 false
断言：P1/P3 的落盘 unmapped 必须是 false；P2 必须是 true；概览 models 含 big-pickle、
不含 space-bunny；且清空/重启后判定不变（同一进程内前后一致）。
"""
import argparse, json, os, shutil, socket, subprocess, sys, time

WORK = os.environ.get('WORK', '/tmp/t41b')
BIN = os.path.join(WORK, 'bin')
RES = os.path.join(WORK, 'results')
def body_for(model):
    return json.dumps({"model": model, "messages": [{"role": "user", "content": "partB"}], "stream": False}).encode()


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
        time.sleep(0.02)
    raise RuntimeError('启动超时')


def stop(p):
    p.terminate()
    try: p.wait(timeout=15)
    except subprocess.TimeoutExpired: p.kill(); p.wait()


def post(port, model):
    import urllib.request
    req = urllib.request.Request(f'http://127.0.0.1:{port}/v1/chat/completions',
                                 data=body_for(model),
                                 headers={'Content-Type': 'application/json'}, method='POST')
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, r.read()[:200]
    except urllib.error.HTTPError as e:
        return e.code, e.read()[:200]


def get_json(port, path):
    import urllib.request
    with urllib.request.urlopen(f'http://127.0.0.1:{port}{path}', timeout=300) as r:
        return json.loads(r.read().decode())


def lines(path):
    with open(path, 'rb') as f:
        return sum(1 for _ in f)


def wait_new_record(logpath, before, timeout=30.0):
    t0 = time.time()
    while time.time() - t0 < timeout:
        if os.path.exists(logpath) and lines(logpath) > before:
            with open(logpath, 'rb') as f:
                f.seek(0)
                allb = f.read()
            last = allb.rstrip(b'\n').rsplit(b'\n')[-1]
            return json.loads(last.decode())
        time.sleep(0.02)
    return None


def run_once(tag, case_dir, pre_query):
    rd = os.path.join(WORK, 'run', 'restart-' + ('warm' if pre_query else 'cold'))
    shutil.rmtree(rd, ignore_errors=True); shutil.copytree(case_dir, rd)
    logpath = os.path.join(rd, 'call_log.jsonl')
    p, port = start(tag, rd)
    rec = {'pre_query': pre_query}
    try:
        if pre_query:                                  # 强制回填完成
            t0 = time.time()
            get_json(port, '/api/stats?range=180d')
            rec['backfill_ms'] = int((time.time() - t0) * 1000)
        before = lines(logpath)
        t0 = time.time()
        rec['p1_status'], _ = post(port, 'big-pickle')  # P1
        rec['p1_after_start_ms'] = int((time.time() - t0) * 1000)
        r1 = wait_new_record(logpath, before)
        rec['p1_rec'] = r1
        before = lines(logpath)
        rec['p2_status'], _ = post(port, 'space-bunny')  # P2
        rec['p2_rec'] = wait_new_record(logpath, before)
        before = lines(logpath)
        rec['p3_status'], _ = post(port, 'big-pickle')   # P3
        rec['p3_rec'] = wait_new_record(logpath, before)
        d = get_json(port, '/api/stats?range=180d')
        rec['models'] = d['models']
        rec['total_requests'] = d['total_requests']
        tr = get_json(port, '/api/stats/trends?range=180d&group=model')
        rec['trend_models'] = sorted({p['model'] for p in tr})
    finally:
        stop(p)
    return rec


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--tag', default='new')
    ap.add_argument('--case', default=os.path.join(WORK, 'fixture', 'nopickle'))
    ap.add_argument('--repeat', type=int, default=3, help='cold 场景重复次数（race 是概率事件）')
    args = ap.parse_args()
    os.makedirs(RES, exist_ok=True)
    problems, notes = [], []

    def check(rec, tag):
        def um(r):
            return None if not r else r.get('unmapped')
        c = {'cold' if not rec['pre_query'] else 'warm': tag}
        if rec['p1_rec'] is None:
            problems.append(f'{c}: P1 未落盘（无法注入记录，需换探针方式）'); return
        if um(rec['p1_rec']) is not False:
            problems.append(f"{c}: P1 big-pickle 在回填前被冻结 unmapped={um(rec['p1_rec'])}（应在 everServed 里 → false）")
        if rec['p2_rec'] is None or um(rec['p2_rec']) is not True:
            problems.append(f"{c}: P2 space-bunny 期望 unmapped=true，实际={None if not rec['p2_rec'] else um(rec['p2_rec'])}")
        if rec['p3_rec'] is None or um(rec['p3_rec']) is not False:
            problems.append(f"{c}: P3 回填后 big-pickle 期望 unmapped=false，实际={None if not rec['p3_rec'] else um(rec['p3_rec'])}")
        if 'big-pickle' not in rec['models']:
            problems.append(f'{c}: 概览 models 缺 big-pickle（everServed 没生效）')
        if 'space-bunny' in rec['models']:
            problems.append(f'{c}: 概览 models 出现 space-bunny（从未成功的名字不该出现）')
        if set(rec.get('trend_models') or []) != set(rec['models']):
            problems.append(f"{c}: /api/stats 与 /api/stats/trends?group=model 模型集合不一致"
                            f"（{len(rec.get('trend_models') or [])} vs {len(rec['models'])}）")
        notes.append(f"{c}: backfill_ms={rec.get('backfill_ms')} P1_after_start_ms={rec['p1_after_start_ms']} "
                     f"status=({rec['p1_status']},{rec['p2_status']},{rec['p3_status']}) total={rec['total_requests']}")

    cold_runs = [run_once(args.tag, args.case, pre_query=False) for _ in range(args.repeat)]
    warm = run_once(args.tag, args.case, pre_query=True)
    for r in cold_runs:
        check(r, 'cold')
    check(warm, 'warm')

    out = {'tag': args.tag, 'cold_runs': cold_runs, 'warm': warm,
           'problems': problems, 'notes': notes}
    json.dump(out, open(os.path.join(RES, 'restart-test.json'), 'w'), ensure_ascii=False, indent=1)
    for n in notes: print('NOTE:', n)
    if problems:
        print('❌ 重启时序/冻结口径不通过：')
        for x in problems: print('  -', x)
        return 1
    print('✅ 重启时序通过：回填前/后 big-pickle 均判 mapped，space-bunny 判 unmapped')
    return 0


if __name__ == '__main__':
    sys.exit(main())
