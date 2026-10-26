#!/usr/bin/env python3
"""构造 Part B 离线夹具（不碰生产、不碰仓库工作树）。

产物（默认 $WORK/fixture）：
  orig/       原序日志 + 生产同款 config + 文档缓存（仓库 13 条口径）
  shuffled/   同一批记录、同一多重集，行序随机打乱（seed 固定）—— 行序无关性用
  nopickle/   原序日志 + 文档缓存去掉 big-pickle —— everServed 是唯一防线的场景
              （重启时序 race 与 R3 漂移验证都用它）
  docs14/     文档缓存换成今日文档 14 条 —— 敏感性对照
每个目录都含: config.json / call_log.jsonl / free_models_cache.json
"""
import argparse, collections, json, os, random, shutil, sys

REPO_DEFAULT = '/home/jingyx/project/opencode2api-test'
SRC_DEFAULT = '/tmp/t41/live.jsonl'
CFG = {"model_alias": None, "reasoning_effort_map": None,
       "force_disable_thinking": False, "quota_error_signals": {}}


def load_rows(path):
    rows = []
    with open(path, encoding='utf-8', errors='replace') as f:
        for line in f:
            line = line.strip()
            if line:
                try:
                    rows.append(json.loads(line))
                except Exception:
                    pass
    return rows


def write_case(dst, rows, cache, extra_rows=()):
    os.makedirs(dst, exist_ok=True)
    with open(os.path.join(dst, 'call_log.jsonl'), 'w', encoding='utf-8') as f:
        for r in rows:
            f.write(json.dumps(r, ensure_ascii=False, separators=(',', ':')) + '\n')
        for r in extra_rows:
            f.write(json.dumps(r, ensure_ascii=False, separators=(',', ':')) + '\n')
    json.dump(cache, open(os.path.join(dst, 'free_models_cache.json'), 'w'),
              ensure_ascii=False, indent=2)
    json.dump(CFG, open(os.path.join(dst, 'config.json'), 'w'), ensure_ascii=False)
    return dst


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--src', default=SRC_DEFAULT)
    ap.add_argument('--repo', default=REPO_DEFAULT)
    ap.add_argument('--out', default='/tmp/t41b/fixture')
    ap.add_argument('--docs14', default='/home/jingyx/project/opencode2api-test/.research/t41/catalogs.json')
    ap.add_argument('--cases', default='orig,shuffled,nopickle,docs14',
                    help='逗号分隔；只生成需要的夹具（每个 120MB 量级）')
    ap.add_argument('--seed', type=int, default=20261020)
    args = ap.parse_args()

    rows = load_rows(args.src)
    cache13 = {k: True for k in json.load(open(os.path.join(args.repo, 'free_models_cache.json')))}
    docs = None
    if os.path.exists(args.docs14):
        docs = {k: True for k in json.load(open(args.docs14))['docs_free']}
    assert rows, '空日志'
    assert 'big-pickle' in cache13, '仓库缓存里应有 big-pickle'
    cache_nopickle = {k: v for k, v in cache13.items() if k != 'big-pickle'}

    want = {c.strip() for c in args.cases.split(',') if c.strip()}
    shutil.rmtree(args.out, ignore_errors=True)
    shuf = list(rows)
    random.Random(args.seed).shuffle(shuf)
    if 'orig' in want:
        write_case(os.path.join(args.out, 'orig'), rows, cache13)
    if 'shuffled' in want:
        write_case(os.path.join(args.out, 'shuffled'), shuf, cache13)
    if 'nopickle' in want:
        write_case(os.path.join(args.out, 'nopickle'), rows, cache_nopickle)
    if 'docs14' in want and docs:
        write_case(os.path.join(args.out, 'docs14'), rows, docs)

    # 记录每个夹具的口径预期，供后续断言交叉核对
    meta = {
        'src': args.src, 'lines': len(rows), 'seed': args.seed,
        'multi_set_equal': collections.Counter(
            json.dumps(r, sort_keys=True) for r in rows) == collections.Counter(
            json.dumps(r, sort_keys=True) for r in shuf),
        'cache13': sorted(cache13), 'cache_nopickle_n': len(cache_nopickle),
        'docs14': sorted(docs) if docs else None,
        'cases': sorted(want),
    }
    json.dump(meta, open('/tmp/t41b/fixture-meta.json', 'w'), ensure_ascii=False, indent=1)
    print(json.dumps({k: meta[k] for k in ('lines', 'multi_set_equal', 'cases')}, ensure_ascii=False))
    print('docs14 present:', bool(docs))


if __name__ == '__main__':
    main()
