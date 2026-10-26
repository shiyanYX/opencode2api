#!/usr/bin/env python3
"""task-56 阶段2：磁盘占用投影（新落盘格式 vs 现状）。

输入：/tmp/t56/day/*.jsonl（生产快照按本地日期切好的明文日文件）
输出：evidence/disk_projection.txt
"""
import gzip, json, os, sys, datetime, collections

DAY_DIR = "/tmp/t56/day"
OUT = os.path.join(os.path.dirname(__file__), "evidence", "disk_projection.txt")

DEFAULT_PATH = "/v1/chat/completions"
DEFAULT_ROUTE = "public"
ISSUE_TYPES = {"switch","ttft_timeout","silence_timeout","stream_interrupt",
               "stream_error","connect_error","upstream_error","all_failed"}

def has_issue(r):
    if r.get("status") != "ok":
        return True
    return any(e.get("type") in ISSUE_TYPES for e in r.get("events") or [])

def new_format_bytes(line: bytes) -> int:
    """模拟新代码落盘：返回该行新格式字节数（含换行）。"""
    r = json.loads(line)
    out = dict(r)
    # events：干净记录不动（与现状同）；有 issue 时保留但去掉 node_name
    if not has_issue(r):
        out.pop("events", None)
    else:
        evs = []
        for e in out.get("events") or []:
            e = dict(e); e.pop("node_name", None); evs.append(e)
        out["events"] = evs
    if out.get("path") == DEFAULT_PATH: out.pop("path", None)
    if out.get("route_mode") == DEFAULT_ROUTE: out.pop("route_mode", None)
    out.pop("final_status", None); out.pop("dial_ms", None)
    # 由调用方补换行：这里返回不含换行
    return len(json.dumps(out, ensure_ascii=False, separators=(",", ":")).encode())

per_day = {}
for name in sorted(os.listdir(DAY_DIR)):
    if not name.endswith(".jsonl"): continue
    day = name[:-6]
    raw = 0; new = 0; n = 0
    data = open(os.path.join(DAY_DIR, name), "rb").read()
    for line in data.splitlines():
        if not line.strip(): continue
        raw += len(line) + 1
        new += new_format_bytes(line) + 1
        n += 1
    per_day[day] = (n, raw, new)

total_files = total_n = total_raw = total_new = 0
for day,(n,raw,new) in per_day.items():
    total_files += 1; total_n += n; total_raw += raw; total_new += new

days_sorted = sorted(per_day)
recent14 = days_sorted[-14:]
r14_n = sum(per_day[d][0] for d in recent14)
r14_raw = sum(per_day[d][1] for d in recent14)
r14_new = sum(per_day[d][2] for d in recent14)

# gz 比例：对最近 7 天的「原格式字节 / 新格式字节」分别实测压缩
gz_raw_bytes = 0; gz_new_bytes = 0; raw7 = 0; new7 = 0
for d in days_sorted[-7:]:
    data = open(os.path.join(DAY_DIR, d + ".jsonl"), "rb").read()
    buf_raw = bytearray(); buf_new = bytearray()
    for line in data.splitlines():
        if not line.strip():
            continue
        buf_raw += line + b"\n"
        r = json.loads(line)
        out = dict(r)
        if not has_issue(r):
            out.pop("events", None)
        else:
            evs = []
            for e in out.get("events") or []:
                e = dict(e); e.pop("node_name", None); evs.append(e)
            out["events"] = evs
        if out.get("path") == DEFAULT_PATH: out.pop("path", None)
        if out.get("route_mode") == DEFAULT_ROUTE: out.pop("route_mode", None)
        out.pop("final_status", None); out.pop("dial_ms", None)
        buf_new += json.dumps(out, ensure_ascii=False, separators=(",", ":")).encode() + b"\n"
    raw7 += len(buf_raw); new7 += len(buf_new)
    gz_raw_bytes += len(gzip.compress(bytes(buf_raw), 6))
    gz_new_bytes += len(gzip.compress(bytes(buf_new), 6))
ratio_raw = raw7 / gz_raw_bytes if gz_raw_bytes else 0
ratio_new = new7 / gz_new_bytes if gz_new_bytes else 0

lines = []
w = lines.append
w("== task-56 阶段2 磁盘占用投影（生产快照 %s，%d 个日文件 / %d 行）==" % (days_sorted[-1], total_files, total_n))
w("现状合计: %d B (%.2f MB)" % (total_raw, total_raw/1e6))
w("新格式合计: %d B (%.2f MB)  → 全量重写省 %.1f%% (仅作估算：存量不重写)"
  % (total_new, total_new/1e6, (1-total_new/total_raw)*100))
w("最近 %d 天（%s..%s）: %d 行 现状 %.2f MB / 新格式 %.2f MB"
  % (len(recent14), recent14[0], recent14[-1], r14_n, r14_raw/1e6, r14_new/1e6))
w("  → 现状 %.3f MB/天，新格式 %.3f MB/天（−%.1f%%）"
  % (r14_raw/1e6/len(recent14), r14_new/1e6/len(recent14),
     (1-r14_new/r14_raw)*100))
w("最近 7 天 gzip 比例: 原格式 %.2f×（%.2f MB → %.2f MB） / 新格式 %.2f×（%.2f MB → %.2f MB）"
  % (ratio_raw, raw7/1e6, gz_raw_bytes/1e6, ratio_new, new7/1e6, gz_new_bytes/1e6))
gz_ratio = ratio_new if ratio_new else 9.5

new_mb_per_day = r14_new/1e6/len(recent14)
def proj(retain_days, hot_days=7):
    hot = hot_days * new_mb_per_day
    archived = (retain_days - hot_days) * new_mb_per_day / gz_ratio
    legacy = total_raw/1e6
    return legacy, hot, archived, legacy+hot+archived
w("")
w("投影（legacy %.1f MB 原样保留；热 %d 天明文；其余 gz）:" % (total_raw/1e6, 7))
for rd in (90, 180):
    legacy, hot, arch, tot = proj(rd)
    w("  保留 %3d 天: legacy %.1f + 热 %.1f + gz %.1f = %.1f MB（封顶，不再按天线性增长）"
      % (rd, legacy, hot, arch, tot))
w("")
w("对照：现状无上限增长，最近 14 天均值 %.3f MB/天 ⇒ 一年约 %.0f MB（不含 legacy）"
  % (r14_raw/1e6/len(recent14), r14_raw/1e6/len(recent14)*365))
open(OUT,"w").write("\n".join(lines)+"\n")
print("\n".join(lines))
