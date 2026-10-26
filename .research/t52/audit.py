#!/usr/bin/env python3
"""全量字段体积盘查：对每一行做「顶层键精确字节归属」（不重新序列化，直接按原始字节切片）。"""
import json, sys, collections, hashlib

PATH = sys.argv[1] if len(sys.argv) > 1 else '/tmp/t52/call_log.jsonl'
WS = b' \t\n\r'


def spans(buf):
    """返回 [(key, start, end)]：顶层键及其「key:value」原始字节区间。buf 是 {...} 的 bytes。"""
    n = len(buf); i = 0
    while i < n and buf[i] in b'{, \n\t': i += 1
    out = []
    while i < n:
        if buf[i] != 0x22:  # "
            i += 1; continue
        ks = i; i += 1
        while i < n:
            if buf[i] == 0x5c: i += 2; continue
            if buf[i] == 0x22: break
            i += 1
        ke = i; i += 1
        while i < n and buf[i] in WS: i += 1
        if i >= n or buf[i] != 0x3a:  # :
            i += 1; continue
        i += 1
        while i < n and buf[i] in WS: i += 1
        vs = i
        depth = 0; instr = False
        while i < n:
            c = buf[i]
            if instr:
                if c == 0x5c: i += 2; continue
                if c == 0x22: instr = False
                i += 1; continue
            if c == 0x22: instr = True; i += 1; continue
            if c in b'{[': depth += 1; i += 1; continue
            if c in b'}':
                if depth == 0: break
                depth -= 1; i += 1; continue
            if c == 0x5d:  # ]
                depth -= 1; i += 1; continue
            if c == 0x2c and depth == 0: break
            i += 1
        ve = i
        key = buf[ks + 1:ke].decode('utf-8', 'replace')
        out.append((key, ks, ke, vs, ve))
        i = ve
        while i < n and buf[i] in WS: i += 1
        if i < n and buf[i] == 0x2c: i += 1
        while i < n and buf[i] in WS: i += 1
    return out


stats = collections.defaultdict(lambda: {'n': 0, 'bytes': 0, 'empty': 0, 'null': 0,
                                         'vals': collections.Counter()})
nested = collections.defaultdict(lambda: {'n': 0, 'bytes': 0, 'empty': 0, 'null': 0})
days = collections.defaultdict(lambda: {'lines': 0, 'bytes': 0, 'ok': 0, 'fail': 0,
                                        'events_bytes': 0, 'models': collections.Counter()})
total_lines = total_bytes = 0
ts_min = ts_max = None
events_elems = collections.Counter()
events_elem_bytes = 0
line_lens = []
bad = 0
h = hashlib.sha256()
with open(PATH, 'rb') as f:
    for raw in f:
        total_lines += 1; total_bytes += len(raw)
        h.update(raw)
        line_lens.append(len(raw))
        if total_lines % 50000 == 0:
            print(f'  …{total_lines}', file=sys.stderr, flush=True)
        try:
            sp = spans(raw)
        except Exception:
            bad += 1; continue
        d = {}
        for key, ks, ke, vs, ve in sp:
            blen = ve - ks  # "key":value 的原始字节数（不含分隔逗号与空白）
            st = stats[key]; st['n'] += 1; st['bytes'] += blen
            val = raw[vs:ve]
            if val in (b'null',):
                st['null'] += 1
            elif val in (b'""', b'[]', b'{}'):
                st['empty'] += 1
            if key in ('status', 'path', 'model', 'route_mode', 'stream', 'final_status'):
                st['vals'][val.decode('utf-8', 'replace')[:40]] += 1
            if key == 'ts':
                try:
                    d['ts'] = json.loads(val)
                except Exception: pass
            if key == 'events':
                events_elems[raw[vs:ve].count(b'{')] += 1
                events_elem_bytes += blen
                # 事件元素内部字段
                inner = raw[vs:ve]
                if inner.startswith(b'['):
                    i = inner.find(b'{')
                    while i >= 0:
                        # 找到该对象的结束（括号配平）
                        depth = 0; j = i; instr = False
                        while j < len(inner):
                            c = inner[j]
                            if instr:
                                if c == 0x5c: j += 2; continue
                                if c == 0x22: instr = False
                                j += 1; continue
                            if c == 0x22: instr = True; j += 1; continue
                            if c == 0x7b: depth += 1; j += 1; continue
                            if c == 0x7d:
                                depth -= 1; j += 1
                                if depth == 0: break
                                continue
                            j += 1
                        obj = inner[i:j + 1]
                        for key2, ks2, ke2, vs2, ve2 in spans(obj):
                            blen2 = ve2 - ks2
                            ns = nested[key2]; ns['n'] += 1; ns['bytes'] += blen2
                            v2 = obj[vs2:ve2]
                            if v2 == b'null': ns['null'] += 1
                            elif v2 in (b'""', b'[]', b'{}'): ns['empty'] += 1
                        i = inner.find(b'{', j)
        st = d.get('ts')
        day = (st or '')[:10] or '?'
        dd = days[day]; dd['lines'] += 1; dd['bytes'] += len(raw)
        if b'"status":"ok"' in raw: dd['ok'] += 1
        elif b'"status":"fail"' in raw: dd['fail'] += 1
        ts = d.get('ts')
        if ts:
            if ts_min is None or ts < ts_min: ts_min = ts
            if ts_max is None or ts > ts_max: ts_max = ts

print(json.dumps({
    'path': PATH, 'lines': total_lines, 'bytes': total_bytes, 'sha256': h.hexdigest(),
    'avg_line': total_bytes / total_lines,
    'ts_min': ts_min, 'ts_max': ts_max, 'bad_lines': bad,
    'events_elems': dict(events_elems),
    'line_len_p50': sorted(line_lens)[len(line_lens) // 2],
    'line_len_p95': sorted(line_lens)[int(len(line_lens) * 0.95)],
    'line_len_max': max(line_lens),
}, ensure_ascii=False, indent=1), file=open('/tmp/t52/summary.json', 'w'))

json.dump({'fields': {k: {**v, 'vals': dict(v['vals'].most_common(12))} for k, v in stats.items()},
           'nested_events': dict(nested)}, open('/tmp/t52/fields.json', 'w'),
          ensure_ascii=False, indent=1)
json.dump({'days': {d: {**v, 'models': dict(v['models'])} for d, v in sorted(days.items())}},
          open('/tmp/t52/days.json', 'w'), ensure_ascii=False, indent=1)

print(f"{'field':26s} {'count':>8s} {'null%':>7s} {'empty%':>7s} {'bytes':>12s} {'byte%':>6s} {'B/行':>7s}")
for k, v in sorted(stats.items(), key=lambda kv: -kv[1]['bytes']):
    if not v['n']: continue
    print(f"{k:26s} {v['n']:8d} {100*v['null']/v['n']:6.1f}% {100*v['empty']/v['n']:6.1f}% "
          f"{v['bytes']:12d} {100*v['bytes']/total_bytes:5.1f}% {v['bytes']/total_lines:7.1f}")
print(f"{'合计(字段)':26s} {total_lines:8d} {'':7s} {'':7s} {sum(v['bytes'] for v in stats.values()):12d}")
print(f"{'文件真实字节':26s} {'':8s} {'':7s} {'':7s} {total_bytes:12d}")
print('\n--- events 内部（每个事件对象内的字段，按总字节）---')
for k, v in sorted(nested.items(), key=lambda kv: -kv[1]['bytes']):
    print(f"{k:26s} {v['n']:8d} {'':7s} {'':7s} {v['bytes']:12d} {100*v['bytes']/events_elem_bytes:5.1f}% {v['bytes']/total_lines:7.1f}")
print('\n--- 按天 ---')
for d, v in sorted(days.items()):
    print(f"{d}  lines={v['lines']:7d} bytes={v['bytes']:10d} ({v['bytes']/1e6:6.1f} MB) ok={v['ok']:7d} fail={v['fail']:7d}")
