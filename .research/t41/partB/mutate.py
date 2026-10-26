#!/usr/bin/env python3
"""Part B：变异测试。在冻结副本上逐条注入变异，要求实现者测试**变红**。

red 判据 = 出现 baseline 没有的 `--- FAIL: TestX`（或 baseline 全绿时 exit!=0 且有 FAIL）。
基线（未变异）必须绿；否则先报 baseline 不绿，不做结论。
"""
import argparse, json, os, re, shutil, subprocess, sys, time

WORK = os.environ.get('WORK', '/tmp/t41b')
REPO = os.environ.get('REPO', '/home/jingyx/project/opencode2api-test')
RES = os.path.join(WORK, 'results')
FAIL_RE = re.compile(r'^--- FAIL: (\S+)', re.M)
PASS_RE = re.compile(r'^--- PASS: (\S+)', re.M)
SKIP_RE = re.compile(r'^--- SKIP: (\S+)', re.M)


def run_tests(src, timeout, run_filter=None):
    env = os.environ.copy()
    # 必须复用仓库里已热的 Go 缓存：冷缓存会从头编译 mihomo 等重依赖（实测 7min+ 卡住）
    # 注意：调用方若把 GOCACHE 传成空/不可写（如 "/.gocache"），这里兜底改回仓库缓存
    def _usable(p):
        return bool(p) and os.path.isdir(p) and os.access(p, os.W_OK)
    if not _usable(env.get('GOCACHE')):
        env['GOCACHE'] = os.path.join(REPO, '.gocache')
    if not _usable(env.get('GOMODCACHE')):
        env['GOMODCACHE'] = os.path.join(REPO, '.modcache')
    env.setdefault('GOTMPDIR', os.path.join(WORK, '.gotmp'))
    env['PATH'] = env.get('PATH', '') + ':/usr/local/go/bin'
    t0 = time.time()
    cmd = ['go', 'test', '-count=1', '-timeout', '30m',
           '-skip', 'TestT29PlainIsByteIdenticalToEmbeddedPanel', './']
    if run_filter:
        cmd = ['go', 'test', '-count=1', '-timeout', '30m', '-skip',
               'TestT29PlainIsByteIdenticalToEmbeddedPanel', '-run', run_filter, './']
    try:
        p = subprocess.run(cmd, cwd=src, env=env,
                           capture_output=True, text=True, timeout=timeout)
        out, code, timed = p.stdout + p.stderr, p.returncode, False
    except subprocess.TimeoutExpired as e:
        out = (e.stdout or b'').decode('utf-8', 'replace') if isinstance(e.stdout, bytes) else (e.stdout or '')
        code, timed = -9, True
    return {'exit': code, 'fail': sorted(FAIL_RE.findall(out)),
            'pass': sorted(PASS_RE.findall(out)), 'skip': sorted(SKIP_RE.findall(out)),
            'timed_out': timed, 'seconds': round(time.time() - t0, 1), 'tail': out[-4000:]}


def apply_mutation(src_dir, patches):
    touched, errs = {}, []
    for pt in patches:
        path = os.path.join(src_dir, pt['file'])
        text = open(path, encoding='utf-8').read()
        n = text.count(pt['find'])
        if n != 1:
            errs.append(f"{pt['file']}: find 命中 {n} 次（要求 1）—— MUTATION_DRIFT，需按当前代码更新")
            continue
        text = text.replace(pt['find'], pt['replace'], 1)
        open(path, 'w', encoding='utf-8').write(text)
        touched[pt['file']] = __import__('hashlib').sha256(text.encode()).hexdigest()
    return touched, errs


def copy_src(dst):
    src = os.environ.get('FROZEN', os.path.join(WORK, 'frozen'))
    assert os.path.isdir(src), f'缺冻结副本 {src}'
    shutil.rmtree(dst, ignore_errors=True)
    os.makedirs(os.path.dirname(dst), exist_ok=True)
    subprocess.run(['rsync', '-a', src + '/', dst + '/'], check=True)
    return dst


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--spec', default=os.path.join(os.path.dirname(os.path.abspath(__file__)), 'mutations.json'))
    ap.add_argument('--only', nargs='*', default=None)
    ap.add_argument('--timeout', type=int, default=1800)
    ap.add_argument('--skip-baseline', action='store_true')
    ap.add_argument('--check-only', action='store_true', help='只校验变异 find 是否唯一命中，不跑测试')
    ap.add_argument('--run', default=None, help='给 go test 的 -run 过滤（加速用，会写进结果）')
    ap.add_argument('--apply', default=None, metavar='ID', help='只把某条变异应用到 --dir，不跑测试')
    ap.add_argument('--dir', default=None, help='配合 --apply 的目标源码目录')
    ap.add_argument('--full-suite-on-green', action='store_true',
                    help='先用 -run 子集加速；若某条变异仍绿，再用全量套件复跑该条（防止子集漏测造成假绿）')
    args = ap.parse_args()
    os.makedirs(RES, exist_ok=True)
    spec = json.load(open(args.spec, encoding='utf-8'))

    if args.apply:
        spec_by_id = {m['id']: m for m in spec['mutations']}
        m = spec_by_id[args.apply]
        assert args.dir, '--apply 需要 --dir'
        touched, errs = apply_mutation(args.dir, m['patches'])
        if errs:
            print('❌ 变异应用失败:', errs); return 1
        print(f"已应用 {m['id']} 到 {args.dir}: {m['title']}")
        for k, v in touched.items(): print(f"  {k} sha256={v[:16]}…")
        return 0
    results = {'baseline': None, 'mutations': [], 'problems': []}
    if args.check_only:
        drift = 0
        for m in spec['mutations']:
            if not m.get('patches'):
                print(f"{m['id']}: 跳过（{m.get('status')}，find 待冻结时补全）")
                continue
            for pt in m['patches']:
                path = os.path.join(os.environ.get('FROZEN', os.path.join(WORK, 'frozen')), pt['file'])
                if not os.path.exists(path):
                    print(f"{m['id']}: 缺文件 {pt['file']}（先做 stage 0 冻结）"); drift += 1; continue
                n = open(path, encoding='utf-8').read().count(pt['find'])
                flag = 'OK' if n == 1 else f'DRIFT(count={n})'
                print(f"{m['id']} {pt['file']}: {flag}")
                if n != 1: drift += 1
        return 1 if drift else 0
    if not args.skip_baseline:
        print('== 基线（未变异）==', flush=True)
        base_dir = copy_src(os.path.join(WORK, 'mut', 'baseline'))
        results['baseline'] = run_tests(base_dir, args.timeout, args.run)
        b = results['baseline']
        print(f"   exit={b['exit']} fail={b['fail']} {b['seconds']}s", flush=True)
        if b['exit'] != 0 or b['fail']:
            results['problems'].append(f"基线不绿，变异结论无效：fail={b['fail']}")
    base_fail = set((results['baseline'] or {}).get('fail') or [])

    for m in spec['mutations']:
        if args.only and m['id'] not in args.only:
            continue
        rec = {'id': m['id'], 'title': m['title'], 'expect': m['expect']}
        if not m.get('patches'):
            rec['skipped'] = f"实现未落地（{m.get('status')}）：find 待冻结时补全"
            print(f"== {m['id']} 跳过：{rec['skipped']}", flush=True)
            results['mutations'].append(rec)
            continue
        print(f"== {m['id']} {m['title']} ==", flush=True)
        md = copy_src(os.path.join(WORK, 'mut', m['id']))
        touched, errs = apply_mutation(md, m['patches'])
        rec['patched'] = {k: v[:12] for k, v in touched.items()}
        if errs:
            rec['applied'] = False
            rec['errors'] = errs
            results['problems'].append(f"{m['id']}: {errs}")
            print('   ❌ 变异未应用：', errs, flush=True)
            results['mutations'].append(rec)
            continue
        rec['applied'] = True
        r = run_tests(md, args.timeout, args.run)
        rec.update(exit=r['exit'], timed_out=r['timed_out'], seconds=r['seconds'],
                   fail=r['fail'], new_fail=sorted(set(r['fail']) - base_fail),
                   tail=r['tail'] if (r['exit'] == 0) else '')
        rec['red'] = bool(rec['new_fail']) or (r['exit'] != 0 and not base_fail)
        if not rec['red'] and args.run and args.full_suite_on_green:
            print('   子集下仍绿 → 用全量套件复跑该变异', flush=True)
            r2 = run_tests(md, args.timeout, None)
            rec['full_suite'] = {'exit': r2['exit'], 'fail': r2['fail'],
                                 'new_fail': sorted(set(r2['fail']) - base_fail),
                                 'seconds': r2['seconds']}
            rec['red'] = bool(rec['full_suite']['new_fail']) or \
                         (r2['exit'] != 0 and not base_fail)
        print(f"   exit={r['exit']} 新增失败={rec['new_fail']} {r['seconds']}s → "
              f"{'RED ✅' if rec['red'] else 'GREEN ❌'}", flush=True)
        if m['expect'] == 'red' and not rec['red']:
            results['problems'].append(f"{m['id']} 期望变红但依旧绿：{m['title']}")
        results['mutations'].append(rec)
        # 清掉 worktree 副本，避免占盘；日志已进 results
        shutil.rmtree(md, ignore_errors=True)

    results['run_filter'] = args.run
    results['skip'] = 'TestT29PlainIsByteIdenticalToEmbeddedPanel（因并发面板任务，不在本次验收范围）'
    results['expected_baseline_fails'] = ['TestT30GateNowJudgesFixedSubsetDead',
                                          'TestT30CriterionCPoolEmptyReachability']
    json.dump(results, open(os.path.join(RES, 'mutations.json'), 'w'), ensure_ascii=False, indent=1)
    red = [m['id'] for m in results['mutations'] if m.get('red')]
    skipped = [m['id'] for m in results['mutations'] if m.get('skipped')]
    print(f"\n变异变红: {red}  跳过: {skipped}")
    if results['problems']:
        print('❌ 变异测试问题：')
        for p in results['problems']: print('  -', p)
        return 1
    print('✅ 变异测试通过（全部期望变红的都变红）')
    return 0


if __name__ == '__main__':
    sys.exit(main())
