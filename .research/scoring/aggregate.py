#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""node scoring 回算 · 第 1 步：把生产 call_log / proxy_state / 运行日志聚合成分数输入。

数据（只读拷贝，非仓库文件）：
  /tmp/scoring/call_log.jsonl      生产调用日志 233,692 行（2026-08-13 → 2026-10-20）
  /tmp/scoring/proxy_state.json    生产节点状态快照（219 节点）
  /tmp/scoring/opencode2api.log    运行日志 72MB（探测失败 / 状态变更 / 轮次汇总）

输出（本目录）：
  node_stats.json     每节点真实调用聚合 + 探测失败计数 + 当前状态
  coverage.json       归因覆盖率（有多少请求/尝试能被归因）
  probe_rounds.json   最近探测轮次（全局健康趋势；per-node 历史需新存储，见 spec §2）
"""
import json, os, re, sys, collections, statistics

DATA = "/tmp/scoring"
OUT = os.path.dirname(os.path.abspath(__file__))
HALF_LIFE_DAYS = float(os.environ.get("SCORING_HL", "7"))
BLIP_FACTOR = float(os.environ.get("SCORING_BLIP", "0.2"))  # 抖动窗口内失败只背 20% 锅
DAY = 86400.0

# ---------- 失败分类（spec §3.4：权重 = 对质量分的「失败」权重，None = 不计入质量分母） ----------
def classify_upstream(detail: str):
    d = (detail or "").lower()
    m = re.search(r"http\s+(\d{3})", d)
    code = int(m.group(1)) if m else None
    if code == 429 or "quota" in d or "freetier" in d or "feusage" in d:
        return "quota", None, True          # 额度耗尽：不计质量；冷却/原因单独记
    if code in (401, 403, 410, 426):
        return "cred", 1.0, True            # 凭据/被拒：节点侧失败
    if code and 500 <= code <= 599:
        return "http5xx", 0.5, True         # 上游抖：半权
    if code in (400, 404):
        return "req_shape", None, False     # 请求形态/模型不存在：剔除
    if code:
        return "http_other_%d" % code, 0.5, True
    if "timeout" in d or "deadline" in d or "eof" in d or "reset" in d:
        return "timeout", 1.0, True
    return "other_err", 0.5, True


def classify_event(e):
    """返回 (class, fail_weight, counts_to_quality)；class=None 表示不是「尝试结果」标记（switch 等）。"""
    t = e.get("type")
    if t == "switch":
        return None, None, None            # 换节点标记：不覆盖前一条错误归因
    if t == "connect_ok":
        return "connect_ok", 0.0, True     # 连接成功：成功证据（后续 upstream_error 会覆盖）
    if t == "connect_error":
        return "connect", 1.0, True
    if t == "stream_interrupt":
        return "stream_interrupt", 0.5, True
    if t == "upstream_error":
        return classify_upstream(e.get("detail"))
    if t == "quota_halt":
        return "quota", None, False
    if t == "freetier_node_confirmed_bad":
        return "confirmed_bad", 1.0, True
    if t == "region_restriction":
        return "region_restriction", 0.5, True
    return t or "unknown", None, False


def main():
    cp = os.path.join(DATA, "call_log.jsonl")
    if not os.path.exists(cp):
        sys.exit("缺少 %s" % cp)

    # ---------------- 第 0 遍：字段/时间范围 + fp↔name 映射 ----------------
    name_of = {}          # fp -> 最近一次看到的节点名
    ref_ts = 0.0          # 日志中最大时间戳（衰减参考点）
    n_lines = 0
    day_total = collections.Counter()
    day_fail = collections.Counter()
    st_total = collections.Counter()
    bucket_req = collections.Counter()
    bucket_fail = collections.Counter()
    for line in open(cp, encoding="utf-8", errors="replace"):
        line = line.strip()
        if not line:
            continue
        try:
            d = json.loads(line)
        except Exception:
            continue
        n_lines += 1
        day_total[(d.get("ts") or "")[:10]] += 1
        st_total[d.get("status")] += 1
        if d.get("status") == "fail":
            day_fail[(d.get("ts") or "")[:10]] += 1
        ts = parse_ts(d.get("ts"))
        if ts > ref_ts:
            ref_ts = ts
        if ts:
            bk = int(ts // 600)
            bucket_req[bk] += 1
            if d.get("status") == "fail":
                bucket_fail[bk] += 1
        fps, names = d.get("nodes") or [], d.get("node_names") or []
        for fp, nm in zip(fps, names):
            if nm:
                name_of[fp] = nm
    print("行数=%d 参考时间戳=%.3f (%s) 名字映射=%d" % (n_lines, ref_ts, fmt_ts(ref_ts), len(name_of)))

    # ---------------- 第 1 遍：真实调用逐请求归因 ----------------
    N = collections.defaultdict(lambda: {
        "name": "",
        "attempts": 0, "success": 0,
        "fail": collections.Counter(),       # class -> 次数
        "eff_s": 0.0, "eff_n": 0.0,          # 7 天半衰期加权
        "quota_events": 0,
        "blip_fails": 0,
        "excluded": 0, "unknown": 0,
        "durations": [], "ttfts": [],
        "first_ts": None, "last_ts": None, "last_success_ts": None,
        "last_fail_class": None, "last_fail_ts": None,
        "served": 0,
    })
    cov = collections.Counter()

    for line in open(cp, encoding="utf-8", errors="replace"):
        line = line.strip()
        if not line:
            continue
        try:
            d = json.loads(line)
        except Exception:
            continue
        ts = parse_ts(d.get("ts"))
        # 全局抖动抑制：同一 10 分钟窗口池级失败率 ≥50%（且 ≥5 请求）⇒ 该窗口节点失败只背 20% 锅
        blip = False
        if ts:
            bk = int(ts // 600)
            if bucket_req[bk] >= 5 and bucket_fail[bk] * 2 >= bucket_req[bk]:
                blip = True
        age_days = max(0.0, (ref_ts - ts) / DAY)
        w = 0.5 ** (age_days / HALF_LIFE_DAYS)
        fps = d.get("nodes") or []
        if not fps:
            cov["no_nodes"] += 1
            continue
        cov["with_nodes"] += 1
        status = d.get("status")
        events = d.get("events") or []

        # 每节点最后一次事件结论
        outcome = {}   # fp -> (class, weight, counts, ts, detail)
        for e in events:
            fp = e.get("node")
            if not fp:
                continue
            cls, fw, counts = classify_event(e)
            if cls is None:
                continue
            outcome[fp] = (cls, fw, counts, parse_ts(e.get("at")), e.get("detail"))
        # 无事件（10 月后 connect_ok 被裁掉的记录）：ok ⇒ 最后一个节点算成功
        if not events and status == "ok":
            cov["no_events_ok_attributed"] += 1
        elif not events:
            cov["no_events_fail_unattributed"] += 1

        serving = fps[-1] if status == "ok" else None
        for i, fp in enumerate(fps):
            n = N[fp]
            n["name"] = name_of.get(fp, fp[:8])
            if n["first_ts"] is None or ts < n["first_ts"]:
                n["first_ts"] = ts
            if n["last_ts"] is None or ts > n["last_ts"]:
                n["last_ts"] = ts
            oc = outcome.get(fp)
            if oc:
                cls, fw, counts, ets, detail = oc
                if cls == "quota":
                    n["quota_events"] += 1
                    cov["attempts_quota_excluded"] += 1
                    continue
                if not counts:
                    n["excluded"] += 1
                    cov["attempts_excluded"] += 1
                    continue
                n["attempts"] += 1
                n["eff_n"] += w
                if cls == "connect_ok":
                    n["success"] += 1
                    n["eff_s"] += w
                    n["served"] += 1
                    n["last_success_ts"] = ts
                    cov["attempts_success"] += 1
                elif cls in ("connect", "timeout", "cred", "confirmed_bad") or fw == 1.0:
                    n["fail"][cls] += 1
                    n["last_fail_class"], n["last_fail_ts"] = cls, ets
                    blame = 1.0 * (BLIP_FACTOR if blip else 1.0)
                    if blip:
                        n["blip_fails"] += 1
                        cov["attempts_failed_in_blip"] += 1
                    n["eff_s"] += w * (1.0 - blame)   # 抖动窗口内失败只背部分锅
                    cov["attempts_failed"] += 1
                elif fw and fw < 1.0:
                    n["fail"][cls] += 1
                    n["last_fail_class"], n["last_fail_ts"] = cls, ets
                    blame = fw * (BLIP_FACTOR if blip else 1.0)
                    if blip:
                        n["blip_fails"] += 1
                        cov["attempts_partial_fail_in_blip"] += 1
                    n["eff_s"] += w * (1.0 - blame)   # 半权失败 = 半权成功
                    cov["attempts_partial_fail"] += 1
                else:
                    n["success"] += 1
                    n["eff_s"] += w
                    cov["attempts_success"] += 1
            else:
                if fp == serving:
                    n["attempts"] += 1
                    n["success"] += 1
                    n["eff_n"] += w
                    n["eff_s"] += w
                    n["served"] += 1
                    n["last_success_ts"] = ts
                    cov["attempts_success"] += 1
                elif not events and status == "ok" and i == len(fps) - 1:
                    n["attempts"] += 1
                    n["success"] += 1
                    n["eff_n"] += w
                    n["eff_s"] += w
                    n["last_success_ts"] = ts
                    cov["attempts_success"] += 1
                else:
                    n["unknown"] += 1
                    cov["attempts_unknown"] += 1
        # 延迟（服务节点）
        if serving:
            dur = d.get("duration_ms")
            if isinstance(dur, (int, float)) and dur > 0:
                N[serving]["durations"].append(dur)
            ttft = d.get("ttft_ms")
            if isinstance(ttft, (int, float)) and ttft > 0:
                N[serving]["ttfts"].append(ttft)

    print("归因:", dict(cov))

    # ---------------- 第 2 遍：探测失败 / 状态变更（运行日志） ----------------
    probe = collections.defaultdict(lambda: {
        "fail_total": 0, "fail_7d": 0, "fail_24h": 0,
        "dead_marks_total": 0, "dead_marks_7d": 0,
        "last_fail_ts": None, "last_fail_err": None,
        "last_dead_ts": None, "last_state_change_ts": None, "last_state": None,
        "dead_transitions_7d": 0, "recover_transitions_7d": 0,
    })
    name2fp = {nm: fp for fp, nm in name_of.items()}
    logp = os.path.join(DATA, "opencode2api.log")
    rounds = []
    unmapped_probe_names = collections.Counter()
    ts_re = re.compile(r"^time=(\S+)")
    fp_re = re.compile(r"\bfp=([0-9a-f]{8,})")
    nm_re = re.compile(r'node=(?:"([^"]+)"|(\S+))')
    err_re = re.compile(r'error="([^"]*)"')
    reason_re = re.compile(r'reason="([^"]*)"')
    state_re = re.compile(r'\bstate=(\w+)')
    now = ref_ts
    if os.path.exists(logp):
        for line in open(logp, encoding="utf-8", errors="replace"):
            mt = ts_re.match(line)
            if mt:
                t = parse_ts(mt.group(1))
                if t > now:
                    now = t
        print("日志最新时间戳=%s（探测窗口相对此时间）" % fmt_ts(now))
        for line in open(logp, encoding="utf-8", errors="replace"):
            mt = ts_re.match(line)
            if not mt:
                continue
            t = parse_ts(mt.group(1))
            if '"node marked dead by health check"' in line:
                fp = fp_re.search(line)
                fp = fp.group(1) if fp else None
                if not fp:
                    nm = nm_re.search(line)
                    fp = name2fp.get(nm.group(1) or nm.group(2)) if nm else None
                if not fp:
                    continue
                p = probe[fp]
                p["dead_marks_total"] += 1
                if now - t <= 7 * DAY:
                    p["dead_marks_7d"] += 1
                p["last_dead_ts"] = t
                p["last_fail_ts"] = t
                m = reason_re.search(line) or err_re.search(line)
                if m:
                    p["last_fail_err"] = m.group(1)[:140]
            elif 'node health probe failed (streak, not dead yet)' in line:
                fp = fp_re.search(line)
                if not fp:
                    continue
                p = probe[fp.group(1)]
                p["fail_total"] += 1
                if now - t <= 7 * DAY:
                    p["fail_7d"] += 1
                if now - t <= DAY:
                    p["fail_24h"] += 1
                p["last_fail_ts"] = t
                m = err_re.search(line)
                if m:
                    p["last_fail_err"] = m.group(1)[:140]
            elif '"node health probe failed"' in line:
                nm = nm_re.search(line)
                fp = name2fp.get(nm.group(1) or nm.group(2)) if nm else None
                if not fp:
                    if nm:
                        unmapped_probe_names[(nm.group(1) or nm.group(2))[:24]] += 1
                    continue
                p = probe[fp]
                p["fail_total"] += 1
                if now - t <= 7 * DAY:
                    p["fail_7d"] += 1
                if now - t <= DAY:
                    p["fail_24h"] += 1
                p["last_fail_ts"] = t
                m = err_re.search(line)
                if m:
                    p["last_fail_err"] = m.group(1)[:140]
            elif '"node state changed"' in line:
                fp = fp_re.search(line)
                if not fp:
                    continue
                p = probe[fp.group(1)]
                p["last_state_change_ts"] = t
                sm = state_re.search(line)
                p["last_state"] = sm.group(1) if sm else None
                if sm and now - t <= 7 * DAY:
                    if sm.group(1) == "dead":
                        p["dead_transitions_7d"] += 1
                    elif sm.group(1) == "available":
                        p["recover_transitions_7d"] += 1
            elif "probe_round_canary" in line:
                m = re.search(r"probed=(\d+) failed=(\d+) canary_probed=(\d+) canary_failed=(\d+) canary_rate=([0-9.]+).*?verdict=(\w+)", line)
                if m:
                    rounds.append({"ts": mt.group(1), "probed": int(m.group(1)), "failed": int(m.group(2)),
                                   "canary_probed": int(m.group(3)), "canary_failed": int(m.group(4)),
                                   "canary_rate": float(m.group(5)), "verdict": m.group(6)})
            elif 'node health check done' in line:
                m = re.search(r"checked=(\d+)", line)
                if m:
                    rounds.append({"ts": mt.group(1), "checked": int(m.group(1))})
            elif "probe_round_global_blip" in line:
                rounds.append({"ts": mt.group(1), "global_blip": True})
    rounds = rounds[-40:]
    if unmapped_probe_names:
        print("未映射探测失败名(前5):", unmapped_probe_names.most_common(5))

    # ---------------- 第 3 遍：合并当前状态 ----------------
    state = json.load(open(os.path.join(DATA, "proxy_state.json")))["nodes"]
    out = {}
    allfps = set(N) | set(state) | set(k for k in probe if not k.startswith("_"))
    for fp in allfps:
        n = N.get(fp)
        p = probe.get(fp, {})
        st = state.get(fp, {})
        dur = n["durations"] if n else []
        ttft = n["ttfts"] if n else []
        rec = {
            "fp": fp,
            "name": (n and n["name"]) or st.get("name") or name_of.get(fp) or fp[:8],
            "attempts": n["attempts"] if n else 0,
            "success": n["success"] if n else 0,
            "fail": dict(n["fail"]) if n else {},
            "quota_events": n["quota_events"] if n else 0,
            "blip_fails": n["blip_fails"] if n else 0,
            "excluded": n["excluded"] if n else 0,
            "unknown": n["unknown"] if n else 0,
            "eff_s": round(n["eff_s"], 4) if n else 0.0,
            "eff_n": round(n["eff_n"], 4) if n else 0.0,
            "last_ts": fmt_ts(n["last_ts"]) if n and n["last_ts"] else None,
            "last_success_ts": fmt_ts(n["last_success_ts"]) if n and n["last_success_ts"] else None,
            "last_fail_class": n["last_fail_class"] if n else None,
            "last_fail_ts": fmt_ts(n["last_fail_ts"]) if n and n["last_fail_ts"] else None,
            "p50_ms": round(percentile(dur, 50), 1) if dur else None,
            "p95_ms": round(percentile(dur, 95), 1) if dur else None,
            "p99_ms": round(percentile(dur, 99), 1) if dur else None,
            "ttft_p50": round(percentile(ttft, 50), 1) if ttft else None,
            "sample_n": len(dur),
            "probe_fail_total": p.get("fail_total", 0),
            "probe_fail_7d": p.get("fail_7d", 0),
            "probe_fail_24h": p.get("fail_24h", 0),
            "probe_last_fail_ts": fmt_ts(p["last_fail_ts"]) if p.get("last_fail_ts") else None,
            "probe_last_fail_err": p.get("last_fail_err"),
            "dead_marks_total": p.get("dead_marks_total", 0),
            "dead_marks_7d": p.get("dead_marks_7d", 0),
            "last_dead_ts": fmt_ts(p["last_dead_ts"]) if p.get("last_dead_ts") else None,
            "last_state_change_ts": fmt_ts(p["last_state_change_ts"]) if p.get("last_state_change_ts") else None,
            "dead_transitions_7d": p.get("dead_transitions_7d", 0),
            "recover_transitions_7d": p.get("recover_transitions_7d", 0),
            "state": st.get("state"),
            "latency_ms": st.get("latency_ms"),
            "last_probe_at": st.get("last_probe_at"),
            "cooldown_until": st.get("cooldown_until"),
            "state_last_error": st.get("last_error"),
        }
        out[fp] = rec

    suffix = "" if HALF_LIFE_DAYS == 7.0 else "_hl%g" % HALF_LIFE_DAYS
    json.dump({"ref_ts": fmt_ts(ref_ts), "ref_epoch": ref_ts, "log_now_ts": fmt_ts(now),
               "half_life_days": HALF_LIFE_DAYS, "nodes": out},
              open(os.path.join(OUT, "node_stats%s.json" % suffix), "w"), ensure_ascii=False, indent=1)
    json.dump({"lines": n_lines, "coverage": dict(cov),
               "status": dict(st_total),
               "days": {k: {"total": day_total[k], "fail": day_fail.get(k, 0)}
                        for k in sorted(day_total)}},
              open(os.path.join(OUT, "coverage.json"), "w"), ensure_ascii=False, indent=1)
    json.dump({"rounds": rounds}, open(os.path.join(OUT, "probe_rounds.json"), "w"),
              ensure_ascii=False, indent=1)
    print("节点数=%d 输出: node_stats.json / coverage.json / probe_rounds.json（轮次 %d）"
          % (len(out), len(rounds)))


def parse_ts(s):
    if not s:
        return 0.0
    s = s.replace("Z", "+00:00")
    try:
        from datetime import datetime
        return datetime.fromisoformat(s).timestamp()
    except Exception:
        return 0.0


def fmt_ts(t):
    if not t:
        return None
    from datetime import datetime, timezone, timedelta
    return datetime.fromtimestamp(t, timezone(timedelta(hours=8))).strftime("%Y-%m-%dT%H:%M:%S+08:00")


def percentile(vals, q):
    if not vals:
        return 0.0
    vals = sorted(vals)
    k = (len(vals) - 1) * q / 100.0
    f = int(k)
    c = min(f + 1, len(vals) - 1)
    return vals[f] + (vals[c] - vals[f]) * (k - f)


if __name__ == "__main__":
    main()
