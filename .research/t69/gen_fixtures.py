#!/usr/bin/env python3
"""task-69 日志页本地 fixture 生成器（web/fixtures/，已 gitignore）。

数据来源与诚实说明：
- today/7d：totals/ok/fail/timeseries/by_model tokens 取自仓库内**真实**快照
  web/fixtures/stats_today.json / stats_7d.json / trends_7d.json（/api/stats 生产样本）。
- 30d/180d：totals/fail/unmapped/latency.max 取自 task-66 生产快照实测数字
  （见 docs/research/log-aggregate-api.md §12.1）；逐日曲线/模型/节点/错误分布为按画像合成。
- 所有 by_* 的 fail、p95、avg_ms、by_node/by_error 明细、cache 字段为**合成**（真实
  aggregate 接口不返回 cache；合成只为让本地页面有数据可渲染）。
- 不变量：Σby_model.requests == totals.requests、Σby_node.requests == totals.requests、
  Σby_error.count == totals.fail、timeseries 逐桶 ok+fail == requests 且总和 == totals。
"""
import json, pathlib, random
from collections import OrderedDict

ROOT = pathlib.Path(__file__).resolve().parents[2]
FIX = ROOT / "web" / "fixtures"
NOW = "2026-10-21T14:42:48+08:00"
rnd = random.Random(20261021)

def load(name):
    return json.loads((FIX / name).read_text(encoding="utf-8"))

def split_exact(total, weights, minv=0):
    """按权重把 total 精确拆成整数（最大余数法）。"""
    s = sum(weights) or 1
    raw = [total * w / s for w in weights]
    base = [max(minv, int(x)) for x in raw]
    diff = total - sum(base)
    order = sorted(range(len(raw)), key=lambda i: raw[i] - int(raw[i]), reverse=True)
    i = 0
    while diff > 0:
        base[order[i % len(order)]] += 1; diff -= 1; i += 1
    while diff < 0:
        j = max(range(len(base)), key=lambda k: base[k])
        if base[j] <= minv: break
        base[j] -= 1; diff += 1
    return base

def iso_hours_today():
    d = load("stats_today.json")
    buckets = OrderedDict()
    for r in d["trends_by_model"]:
        b = buckets.setdefault(r["ts"], [0, 0, 0])
        b[0] += r["requests"]; b[1] += r["ok"]; b[2] += r["fail"]
    return [{"ts": k, "requests": v[0], "ok": v[1], "fail": v[2]} for k, v in buckets.items()]

def models_from_stats(d, total_override, fail_total):
    models = []
    for name, m in d["models"].items():
        models.append({"model": name, "requests": m["request_count"],
                       "prompt_tokens": m["prompt_tokens"], "completion_tokens": m["completion_tokens"],
                       "total_tokens": m["total_tokens"]})
    # 快照漂移修正：把差额记到最大模型上，保证 Σrequests == totals
    diff = total_override - sum(m["requests"] for m in models)
    if diff:
        top = max(models, key=lambda m: m["requests"]); top["requests"] += diff
    # 失败数按画像分配（space-bunny 最不稳）
    weights = []
    for m in models:
        w = m["requests"] * (0.30 if m["model"].startswith("space-bunny") else
                             0.05 if "deepseek" in m["model"] else 0.12)
        weights.append(w)
    fails = split_exact(fail_total, weights)
    for m, f in zip(models, fails):
        m["fail"] = min(f, m["requests"]); m["ok"] = m["requests"] - m["fail"]
        m["success_rate"] = round(m["ok"] / m["requests"], 4) if m["requests"] else 0
        m["p95_ms"] = 120000 if m["model"].startswith("space-bunny") else \
                      61000 if "mimo" in m["model"] else 48000 if "deepseek" in m["model"] else 30000
    # 修正因 min() 截断造成的总差
    while sum(m["fail"] for m in models) != fail_total:
        need = fail_total - sum(m["fail"] for m in models)
        cand = max(models, key=lambda m: m["requests"] - m["fail"]) if need > 0 else \
               max(models, key=lambda m: m["fail"])
        cand["fail"] += 1 if need > 0 else -1
        cand["ok"] = cand["requests"] - cand["fail"]
        cand["success_rate"] = round(cand["ok"] / cand["requests"], 4) if cand["requests"] else 0
    return sorted(models, key=lambda m: -m["requests"])

NODE_NAMES = None
def node_names(n):
    global NODE_NAMES
    if NODE_NAMES is None:
        d = load("nodes.json")
        NODE_NAMES = [x["name"] for x in (d if isinstance(d, list) else d.get("nodes", []))]
    return NODE_NAMES[:n]

def make_nodes(total, fail_total, n):
    names = node_names(n)
    weights = [1.6, 1.3, 1.15, 1.0, .9, .8, .7, .6, .5, .45, .4, .35][:n]
    reqs = split_exact(total, weights)
    fails = split_exact(fail_total, [w * rnd.uniform(.5, 1.5) for w in weights])
    out = []
    for name, r, f in zip(names, reqs, fails):
        f = min(f, r)
        out.append({"name": name, "requests": r, "ok": r - f, "fail": f,
                    "success_rate": round((r - f) / r, 4) if r else 0,
                    "avg_ms": int(rnd.uniform(1800, 26000))})
    return out

ERR_WEIGHTS = [("upstream_4xx", .42), ("connect_error", .18), ("timeout", .13),
               ("stream_interrupt", .1), ("cooldown", .07), ("all_failed", .05),
               ("upstream_5xx", .03), ("other", .02)]
def make_errors(fail_total):
    counts = split_exact(fail_total, [w for _, w in ERR_WEIGHTS])
    out = []
    for (kind, _), c in zip(ERR_WEIGHTS, counts):
        out.append({"kind": kind, "count": c,
                    "sample": {"upstream_4xx": "status=fail http 429 quota_signal",
                               "connect_error": "connect_error: EOF",
                               "timeout": "ttft_timeout 30000ms",
                               "stream_interrupt": "stream_interrupt at 41.2s",
                               "cooldown": "cooldown: 节点冷却中",
                               "all_failed": "all_failed after 3 nodes",
                               "upstream_5xx": "status=fail http 502",
                               "other": "status=fail"}[kind]})
    return out

def make_unmapped(count, top=("space-bunny", "probe-143148", "probeL-143230")):
    """Top N + __other__ 合并（与 /api/logs/aggregate 的 unmapped 契约一致）。"""
    if count == 0:
        return {"count": 0, "by_model": []}
    if count <= 3:
        out = [{"model": top[i], "count": 1} for i in range(count)]
        if count > 1:
            out = out[:-1] + [{"model": "__other__", "count": 1}]
        return {"count": count, "by_model": out}
    head = [{"model": top[0], "count": count - 3},
            {"model": top[1], "count": 1},
            {"model": "__other__", "count": 2}]
    return {"count": count, "by_model": head}

def daily_series(start_day, days, total, ok, fail):
    # 用一条"周内波动 + 尾部下降"的曲线，再精确配平
    weights = []
    for i in range(days):
        w = 1.0 + .35 * ((i * 7) % 11) / 11.0
        if i >= days - 3: w *= .55
        weights.append(w)
    reqs = split_exact(total, weights)
    oks = split_exact(ok, [r for r in reqs])
    fails = split_exact(fail, [r for r in reqs])
    out = []
    for i in range(days):
        o, f = oks[i], fails[i]
        # 配平 ok+fail == requests
        if o + f != reqs[i]:
            o = reqs[i] - f
        out.append({"ts": f"{start_day}", "requests": reqs[i], "ok": o, "fail": f})
    # 修正尾差
    diff = total - sum(x["requests"] for x in out)
    if diff: out[-1]["requests"] += diff; out[-1]["ok"] += diff
    return out

def base(range_name, total, ok, fail, unmapped, latency, by_model, timeseries,
         by_node, by_error, window_start, window_end, cache_hit=False):
    return {
        "range": range_name,
        "window": {"start": window_start, "end": window_end},
        "generated_at": NOW,
        "totals": {"requests": total, "ok": ok, "fail": fail,
                   "success_rate": round(ok / total, 4) if total else 0},
        "latency": latency,
        "timeseries": timeseries,
        "by_model": by_model,
        "by_node": by_node,
        "by_error": by_error,
        "unmapped": unmapped,
        "node_agg_by": "node_name",
        "cache": {"hit": cache_hit, "ttl": 20, "range": range_name},
    }

def check(d, name):
    t = d["totals"]
    assert sum(x["requests"] for x in d["by_model"]) == t["requests"], (name, "by_model")
    assert sum(x["requests"] for x in d["by_node"]) == t["requests"], (name, "by_node")
    assert sum(x["count"] for x in d["by_error"]) == t["fail"], (name, "by_error")
    assert sum(x["requests"] for x in d["timeseries"]) == t["requests"], (name, "ts")
    for b in d["timeseries"]:
        assert b["ok"] + b["fail"] == b["requests"], (name, "ts bucket", b)
    assert t["ok"] + t["fail"] == t["requests"]
    assert d["unmapped"]["count"] == sum(x["count"] for x in d["unmapped"]["by_model"])
    print(f"OK {name}: requests={t['requests']} ok={t['ok']} fail={t['fail']} unmapped={d['unmapped']['count']} "
          f"models={len(d['by_model'])} nodes={len(d['by_node'])} ts={len(d['timeseries'])}")

def main():
    # ---------- today ----------
    d_today = load("stats_today.json")
    ts = iso_hours_today()
    total, ok, fail = 366, 344, 22
    by_model = models_from_stats(d_today, total, fail)
    by_model = [{"model": m["model"], "requests": m["requests"], "ok": m["ok"], "fail": m["fail"],
                 "success_rate": m["success_rate"], "p95_ms": m["p95_ms"],
                 "prompt_tokens": m["prompt_tokens"], "completion_tokens": m["completion_tokens"],
                 "total_tokens": m["total_tokens"]} for m in by_model]
    d = base("today", total, ok, fail, 0,
             {"p50_ms": 1450, "p95_ms": 9800, "p99_ms": 21000, "max_ms": 119974,
              "ttft_p50_ms": 620, "ttft_p95_ms": 4800},
             by_model, ts, make_nodes(total, fail, 6), make_errors(fail),
             "2026-10-21T00:00:00+08:00", NOW)
    d["unmapped"] = make_unmapped(0)
    check(d, "today")
    (FIX / "aggregate_today.json").write_text(json.dumps(d, ensure_ascii=False, indent=1) + "\n", encoding="utf-8")

    # ---------- 7d ----------
    d7 = load("stats_7d.json")
    trends = load("trends_7d.json")
    total, unm = d7["total_requests"], d7["unmapped_requests"]["count"]
    fail = sum(x["fail"] for x in trends); ok = total - fail
    ts7 = [{"ts": x["ts"], "requests": x["requests"], "ok": x["ok"], "fail": x["fail"]} for x in trends]
    # 配平：trends_7d 与 totals 同一快照（sum 27223），ok/fail 之和需相等
    assert sum(x["requests"] for x in ts7) == total, (sum(x["requests"] for x in ts7), total)
    by_model = models_from_stats(d7, total, fail)
    by_model = [{"model": m["model"], "requests": m["requests"], "ok": m["ok"], "fail": m["fail"],
                 "success_rate": m["success_rate"], "p95_ms": m["p95_ms"],
                 "prompt_tokens": m["prompt_tokens"], "completion_tokens": m["completion_tokens"],
                 "total_tokens": m["total_tokens"]} for m in by_model]
    d = base("7d", total, ok, fail, unm,
             {"p50_ms": 1850, "p95_ms": 45000, "p99_ms": 180000, "max_ms": 1119192,
              "ttft_p50_ms": 780, "ttft_p95_ms": 9200},
             by_model, ts7, make_nodes(total, fail, 8), make_errors(fail),
             "2026-10-15T00:00:00+08:00", NOW)
    um = d7["unmapped_requests"]
    d["unmapped"] = {"count": um["count"], "by_model": [{"model": k, "count": v} for k, v in um["by_model"].items()]}
    check(d, "7d")
    (FIX / "aggregate_7d.json").write_text(json.dumps(d, ensure_ascii=False, indent=1) + "\n", encoding="utf-8")

    # ---------- 30d / 180d（totals 为生产实测，明细合成） ----------
    for rng, total, fail, unm, days, start in [
            ("30d", 157769, 11120, 1352, 30, "2026-09-22"),
            ("180d", 232263, 23633, 1391, 180, "2026-04-25")]:
        ok = total - fail
        tsx = daily_series(start, days, total, ok, fail)
        # 模型画像：生产主模型 + 长尾
        names = ["deepseek-v4.1-flash-free", "space-bunny-free", "mimo-v2.6-flash-free",
                 "big-pickle", "minimax-m3.1-flash-free", "glm-5.3-flash-free", "step-5-preview-free"]
        shares = [.42, .27, .15, .08, .04, .025, .015]
        reqs = split_exact(total, shares)
        fails = split_exact(fail, [s * rnd.uniform(.6, 1.8) for s in shares])
        by_model = []
        for n, r, f in zip(names, reqs, fails):
            f = min(f, r)
            avg_in = 2600 if "space-bunny" in n else 4200 if "deepseek" in n else 3100
            avg_out = 520 if "space-bunny" in n else 780
            pt = int(r * avg_in * rnd.uniform(.9, 1.1)); ct = int(r * avg_out * rnd.uniform(.9, 1.1))
            by_model.append({"model": n, "requests": r, "ok": r - f, "fail": f,
                             "success_rate": round((r - f) / r, 4) if r else 0,
                             "p95_ms": 120000 if "space-bunny" in n else 61000 if "mimo" in n else 48000,
                             "prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct})
        while sum(m["fail"] for m in by_model) != fail:
            need = fail - sum(m["fail"] for m in by_model)
            cand = max(by_model, key=lambda m: m["requests"] - m["fail"]) if need > 0 else max(by_model, key=lambda m: m["fail"])
            cand["fail"] += 1 if need > 0 else -1; cand["ok"] = cand["requests"] - cand["fail"]
            cand["success_rate"] = round(cand["ok"] / cand["requests"], 4)
        d = base(rng, total, ok, fail, unm,
                 {"p50_ms": 2100, "p95_ms": 52000, "p99_ms": 240000, "max_ms": 2832125,
                  "ttft_p50_ms": 860, "ttft_p95_ms": 12000},
                 by_model, tsx, make_nodes(total, fail, 12 if rng == "180d" else 10),
                 make_errors(fail), f"{start}T00:00:00+08:00", NOW, cache_hit=(rng == "30d"))
        d["unmapped"] = make_unmapped(unm)
        check(d, rng)
        (FIX / f"aggregate_{rng}.json").write_text(json.dumps(d, ensure_ascii=False, indent=1) + "\n", encoding="utf-8")

    # ---------- 调用明细 fixture（供 verify 拦截 /api/call-log 使用） ----------
    nodes = node_names(12)
    models = ["deepseek-v4.1-flash-free", "mimo-v2.6-flash-free", "space-bunny-free",
              "big-pickle", "minimax-m3.1-flash-free", "gpt-4o-mini"]
    errs = [("connect_error", 502, "connect_error: EOF"), ("timeout", 504, "ttft_timeout 30000ms"),
            ("stream_interrupt", 502, "stream_interrupt at 41.2s"), ("upstream_4xx", 429, "http 429 quota_signal"),
            ("all_failed", 503, "all_failed after 3 nodes"), ("other", 500, "status=fail")]
    from datetime import datetime, timedelta
    base_ts = datetime(2026, 10, 21, 14, 42, 48)
    entries = []
    n = 450
    for i in range(n):
        seq = n - i
        ts = (base_ts - timedelta(minutes=3 * i)).strftime("%Y-%m-%dT%H:%M:%S+08:00")
        r = rnd.random()
        unmapped = r < 0.02
        fail = (not unmapped) and r < 0.12
        model = "gpt-4o-mini" if unmapped else rnd.choice(models[:5])
        k = 1 if r > .25 else (2 if r > .08 else 3)
        chain = rnd.sample(nodes, k)
        ok = not (fail or unmapped)
        dur = 80 if unmapped else rnd.randint(600, 90000)
        ttft = 0 if (unmapped or not ok) else rnd.randint(180, 4200)
        pt = rnd.randint(120, 24000); ct = 0 if not ok else rnd.randint(40, 1800)
        cr = 0 if unmapped else rnd.randint(0, pt)
        eclass, fstatus, emsg = rnd.choice(errs) if (fail or unmapped) else ("", 200, "")
        ev = []
        if unmapped:
            ev = [{"type": "unmapped", "detail": "模型名无法映射", "at": ts}]
        elif fail:
            ev = [{"type": "upstream_error", "node": chain[0], "node_name": chain[0],
                   "detail": emsg, "at": ts}]
            if k > 1:
                ev.append({"type": "switch", "node": chain[1], "node_name": chain[1],
                           "detail": "换节点重试", "at": ts})
                ev.append({"type": "all_failed", "detail": "候选全部失败", "at": ts})
        elif k > 1:
            ev = [{"type": "connect_error", "node": chain[0], "node_name": chain[0],
                   "detail": "connect_error: EOF", "at": ts},
                  {"type": "switch", "node": chain[1], "node_name": chain[1],
                   "detail": "换节点重试", "at": ts}]
        entries.append({
            "req_id": f"req_{100000 + seq}", "ts": ts,
            "path": "/v1/chat/completions" if not unmapped else "/v1/messages",
            "model": model, "stream": not unmapped, "route_mode": "public",
            "nodes": [] if unmapped else chain,
            "node_names": [] if unmapped else chain,
            "events": ev,
            "status": "fail" if (fail or unmapped) else "ok",
            "prompt_tokens": 0 if unmapped else pt,
            "completion_tokens": ct,
            "cache_creation_tokens": 0,
            "cache_read_tokens": cr,
            "duration_ms": dur, "ttft_ms": ttft,
            "output_speed": round(ct / (dur / 1000), 1) if ct and dur else 0,
            "err_msg": ("模型名无法映射到本部署可服务的模型 · upstream http 401 unauthorized"
                        if unmapped else emsg),
            "dial_ms": rnd.randint(20, 90) if not unmapped else 0,
            "final_status": 401 if unmapped else fstatus,
            "unmapped": unmapped,
        })
    (FIX / "call_log.json").write_text(json.dumps(entries, ensure_ascii=False, indent=1) + "\n", encoding="utf-8")
    print(f"OK call_log.json: {len(entries)} entries (ok={sum(1 for e in entries if e['status']=='ok')}, "
          f"fail={sum(1 for e in entries if e['status']=='fail')}, unmapped={sum(1 for e in entries if e['unmapped'])})")

if __name__ == "__main__":
    main()
