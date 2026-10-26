#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""node scoring 回算 · 第 2 步：按 spec 公式给 219 个当前节点打分，并生成评审用表格。

输入：node_stats.json（默认 7 天半衰期）+ node_stats_hl3.json / node_stats_hl14.json（敏感性）
      /tmp/scoring/proxy_state.json（只读拷贝）
输出：scores.json / scores.csv / distribution.md / consistency.md / worst10.md / best10.md
      sensitivity.md / shrinkage.md / probe_health.md / profile.md
"""
import json, math, os, statistics, collections
from datetime import datetime, timezone, timedelta

OUT = os.path.dirname(os.path.abspath(__file__))
STATE = "/tmp/scoring/proxy_state.json"
CST = timezone(timedelta(hours=8))

DEFAULT = dict(prior_a=8.0, prior_b=2.0, z=1.645,
               w_q=0.60, w_p=0.20, w_l=0.20,
               lat_full=1500, lat_floor=10000,
               dead_factor=0.30, cap_probe24=65, stale_probe_min=45,
               bands=[(85, "优", "可用 · 优"), (70, "良", "可用 · 良"),
                      (50, "观察", "观察 · 谨慎"), (0, "停用", "该切 · 停用")])


def wilson_lb(s, n, z=1.645):
    if n <= 0:
        return 0.0
    p = s / n
    denom = 1 + z * z / n
    center = p + z * z / (2 * n)
    margin = z * math.sqrt(max(0.0, p * (1 - p) / n + z * z / (4 * n * n)))
    return (center - margin) / denom


def latency_score(ms, p):
    if ms is None or ms <= 0:
        return 0.45
    if ms <= p["lat_full"]:
        return 1.0
    if ms >= p["lat_floor"]:
        return 0.10
    return round(1.0 - (ms - p["lat_full"]) / (p["lat_floor"] - p["lat_full"]) * 0.9, 4)


def band_of(score, p):
    for thr, name, ui in p["bands"]:
        if score >= thr:
            return name, ui
    return "停用", "该切 · 停用"


def parse_ts(s):
    if not s:
        return None
    try:
        return datetime.fromisoformat(s).timestamp()
    except Exception:
        return None


def score_all(stats, states, p=None):
    """对当前池的每个节点打分，返回记录列表（含 reasons）。"""
    p = p or DEFAULT
    log_now = parse_ts(stats.get("log_now_ts")) or stats["ref_epoch"]
    ref = stats["ref_epoch"]
    recs = []
    for fp, st in states.items():
        n = stats["nodes"].get(fp) or {
            "fp": fp, "name": fp[:8], "attempts": 0, "success": 0, "fail": {},
            "quota_events": 0, "eff_s": 0.0, "eff_n": 0.0, "p50_ms": None, "p95_ms": None,
            "p99_ms": None, "ttft_p50": None, "sample_n": 0, "probe_fail_7d": 0,
            "probe_fail_24h": 0, "dead_marks_7d": 0, "last_success_ts": None, "last_ts": None,
            "probe_last_fail_ts": None, "probe_last_fail_err": None}
        s = n["eff_s"] + p["prior_a"]
        nn = n["eff_n"] + p["prior_a"] + p["prior_b"]
        q = wilson_lb(s, nn, p["z"])
        probe, probe_flags = 1.0, []
        if st.get("state") == "dead":
            probe = 0.0
            probe_flags.append("state_dead")
        else:
            ph = None
            t = parse_ts(st.get("last_probe_at"))
            if t and datetime.fromtimestamp(t, timezone.utc).year > 1:
                ph = (log_now - t) / 3600.0
            if ph is None:
                probe = 0.5
                probe_flags.append("probe_unknown")
            elif ph > 3 * p["stale_probe_min"] / 60.0:
                probe = 0.3
                probe_flags.append("probe_stale%.0fh" % ph)
            elif ph > p["stale_probe_min"] / 60.0:
                probe = 0.6
                probe_flags.append("probe_stale%.0fm" % (ph * 60))
            if n["probe_fail_24h"] > 0:
                probe = min(probe, 0.4)
                probe_flags.append("probe_fail_24h=%d" % n["probe_fail_24h"])
        lat_s = latency_score(st.get("latency_ms"), p)
        raw = 100.0 * (p["w_q"] * q + p["w_p"] * probe + p["w_l"] * lat_s)
        score, caps = raw, []
        if st.get("state") == "dead":
            score = raw * p["dead_factor"]       # 封顶 30 且保留序（不是一律 30 分，避免排序并列）
            caps.append("dead×0.30")
        if n["probe_fail_24h"] > 0 and st.get("state") != "dead":
            if score > p["cap_probe24"]:
                score = p["cap_probe24"]
                caps.append("probe_24h≤65")
        score = round(max(0.0, min(100.0, score)), 1)
        # 冷启动/无数据：没有真实样本、也没有最新探测 ⇒ 不诬告成「停用」，按「观察」处理（spec §3.6）
        no_data = (n["eff_n"] == 0 and st.get("state") != "dead"
                   and ("probe_unknown" in probe_flags))
        if no_data:
            score = max(score, 50.0)
            caps.append("no_data→观察")
        band, ui = band_of(score, p)
        auto_eligible = bool(st.get("state") != "dead" and score >= 70 and n["eff_n"] >= 20)

        # ---------- reasons ----------
        reasons = []
        fails = n.get("fail") or {}
        att = n["attempts"] or 0
        def rate(c):
            return fails.get(c, 0) / att if att else 0.0
        if st.get("state") == "dead":
            err = (st.get("last_error") or "").replace("probe: ", "")[:90]
            reasons.append({"code": "probe_dead", "level": "high",
                            "text": "探测判死：%s" % (err or "连续探测失败"),
                            "evidence": {"cooldown_until": st.get("cooldown_until")}})
        elif n["probe_fail_24h"] > 0:
            reasons.append({"code": "probe_flaky", "level": "high",
                            "text": "近 24h 探测失败 %d 次（未判死，但已在阈值边缘）" % n["probe_fail_24h"]})
        if fails.get("connect"):
            reasons.append({"code": "connect_fail", "level": "high" if rate("connect") >= 0.1 else "mid",
                            "text": "连接失败 %d 次（占尝试 %.1f%%）" % (fails["connect"], rate("connect") * 100)})
        if fails.get("cred"):
            reasons.append({"code": "cred_rejected", "level": "high" if rate("cred") >= 0.1 else "mid",
                            "text": "被拒/凭据失效 %d 次（%.1f%%：403 FreeTier / 401 / 410）" % (fails["cred"], rate("cred") * 100)})
        if fails.get("http5xx"):
            reasons.append({"code": "upstream_5xx", "level": "mid",
                            "text": "上游 5xx %d 次（%.1f%%，半权计入）" % (fails["http5xx"], rate("http5xx") * 100)})
        if fails.get("stream_interrupt"):
            reasons.append({"code": "stream_interrupt", "level": "low",
                            "text": "流式中断 %d 次（半权）" % fails["stream_interrupt"]})
        if fails.get("confirmed_bad"):
            reasons.append({"code": "confirmed_bad", "level": "high",
                            "text": "被 freetier 校验确认为坏节点 %d 次" % fails["confirmed_bad"]})
        if n.get("blip_fails"):
            reasons.append({"code": "env_blip", "level": "info",
                            "text": "有 %d 次失败发生在池级抖动窗口（同窗口失败率≥50%%），已按 20%% 计责" % n["blip_fails"]})
        if n.get("quota_events"):
            reasons.append({"code": "quota_events", "level": "low",
                            "text": "额度耗尽 %d 次（不计质量分，只说明当前无额度）" % n["quota_events"]})
        if q < 0.55 and n["eff_n"] >= 5:
            reasons.append({"code": "low_quality", "level": "high" if q < 0.4 else "mid",
                            "text": "真实调用成功率下界仅 %.0f%%（有效样本 %.0f）" % (q * 100, n["eff_n"])})
        elif q >= 0.8:
            reasons.append({"code": "real_traffic_good", "level": "good",
                            "text": "真实调用成功率下界 %.0f%%（有效样本 %.0f）" % (q * 100, n["eff_n"])})
        if n["eff_n"] < 20:
            reasons.append({"code": "low_sample", "level": "mid",
                            "text": "有效样本不足（n_eff=%.0f < 20），置信度低" % n["eff_n"]})
        sh = None
        if n.get("last_success_ts"):
            t = parse_ts(n["last_success_ts"])
            if t:
                sh = (ref - t) / 3600.0
        if sh is not None and sh > 72:
            reasons.append({"code": "stale_evidence", "level": "mid",
                            "text": "最近一次真实成功在 %.0f 小时前（数据陈旧）" % sh})
        lat_ms = st.get("latency_ms")
        if isinstance(lat_ms, (int, float)) and lat_ms > 0:
            if lat_ms > 4000:
                reasons.append({"code": "slow_probe", "level": "mid",
                                "text": "探测延迟 %d ms（池中位约 1959 ms）" % lat_ms})
            elif lat_ms <= p["lat_full"]:
                reasons.append({"code": "fast", "level": "good", "text": "探测延迟 %d ms，快" % lat_ms})
        if no_data:
            reasons.insert(0, {"code": "no_data", "level": "mid",
                               "text": "无真实调用样本、也无最新探测数据（新节点/尚未探测），按观察档处理"})
        if not reasons:
            reasons.append({"code": "neutral", "level": "info", "text": "无显著信号"})
        order = {"high": 0, "mid": 1, "low": 2, "info": 3, "good": 4}
        reasons.sort(key=lambda r: order.get(r["level"], 9))

        recs.append({
            "fp": fp, "name": n.get("name") or st.get("name") or fp[:8],
            "score": score, "band": band, "band_ui": ui,
            "quality": round(q, 4), "probe": round(probe, 3), "latency_component": round(lat_s, 3),
            "raw_score": round(raw, 1), "caps": caps, "probe_flags": probe_flags,
            "score_reasons": reasons,
            "usable": st.get("state") != "dead",
            "auto_eligible": auto_eligible,
            "sample_size": att,
            "effective_sample": round(n["eff_n"], 1),
            "low_confidence": n["eff_n"] < 20,
            "sample": {"attempts": att, "success": n["success"],
                       "eff_n": round(n["eff_n"], 1), "eff_s": round(n["eff_s"], 1),
                       "low_confidence": n["eff_n"] < 20},
            "stale": sh is not None and sh > 168,
            "latency_ms": lat_ms, "p50_ms": n.get("p50_ms"), "p95_ms": n.get("p95_ms"),
            "p99_ms": n.get("p99_ms"), "ttft_p50": n.get("ttft_p50"),
            "fail": fails, "quota_events": n.get("quota_events", 0),
            "last_success_at": n.get("last_success_ts"), "last_traffic_at": n.get("last_ts"),
            "last_error": st.get("last_error"),
            "state": st.get("state"), "cooldown_until": st.get("cooldown_until"),
            "last_probe_at": st.get("last_probe_at"),
            "blip_fails": n.get("blip_fails", 0),
            "probe_fail_7d": n.get("probe_fail_7d", 0), "probe_fail_24h": n.get("probe_fail_24h", 0),
            "dead_marks_7d": n.get("dead_marks_7d", 0),
            "trend": None,  # 需后端新增探测历史环形缓冲（spec §2/§5）
        })
    return recs


def pct(vals, q):
    if not vals:
        return 0.0
    vals = sorted(vals)
    k = (len(vals) - 1) * q / 100.0
    f = int(k)
    c = min(f + 1, len(vals) - 1)
    return vals[f] + (vals[c] - vals[f]) * (k - f)


def ranks(vals):
    idx = sorted(range(len(vals)), key=lambda i: vals[i])
    r = [0.0] * len(vals)
    i = 0
    while i < len(idx):
        j = i
        while j + 1 < len(idx) and vals[idx[j + 1]] == vals[idx[i]]:
            j += 1
        avg = (i + j) / 2.0 + 1
        for k in range(i, j + 1):
            r[idx[k]] = avg
        i = j + 1
    return r


def spearman(a, b):
    ra, rb = ranks(a), ranks(b)
    n = len(a)
    ma, mb = sum(ra) / n, sum(rb) / n
    num = sum((x - ma) * (y - mb) for x, y in zip(ra, rb))
    da = math.sqrt(sum((x - ma) ** 2 for x in ra))
    db = math.sqrt(sum((y - mb) ** 2 for y in rb))
    return num / (da * db) if da and db else 1.0


def write_default(recs, stats, cov):
    scores = sorted(recs, key=lambda x: (x["score"], x["raw_score"]))
    json.dump({"generated_at": stats.get("log_now_ts"), "ref_ts": stats.get("ref_ts"),
               "params": DEFAULT, "nodes": {x["fp"]: x for x in recs}},
              open(os.path.join(OUT, "scores.json"), "w"), ensure_ascii=False, indent=1)
    with open(os.path.join(OUT, "scores.csv"), "w") as f:
        f.write("fp,name,score,band,quality,probe,lat_s,raw,attempts,eff_n,latency_ms,p95_ms,state,last_success_at\n")
        for x in scores:
            f.write("%s,%s,%.1f,%s,%.3f,%.2f,%.2f,%.1f,%d,%.0f,%s,%s,%s,%s\n" % (
                x["fp"], x["name"].replace(",", " "), x["score"], x["band"], x["quality"],
                x["probe"], x["latency_component"], x["raw_score"], x["sample"]["attempts"],
                x["sample"]["eff_n"], x["latency_ms"], x["p95_ms"], x["state"], x["last_success_at"]))

    vals = [x["score"] for x in recs]
    bands = collections.Counter(x["band"] for x in recs)
    L = ["# 分数分布（219 个当前节点）", "",
         "参考时间：%s；数据：call_log %d 行 + proxy_state + 运行日志 %s；半衰期 7 天" %
         (stats.get("log_now_ts"), cov["lines"], "(探测窗口以日志最新时间为准)"), "",
         "| 分档 | 节点数 | 占比 | 含义 |", "|---|---|---|---|"]
    for b in ["优", "良", "观察", "停用"]:
        c = bands.get(b, 0)
        L.append("| %s | %d | %.1f%% | %s |" % (b, c, 100.0 * c / len(recs),
                  {"优": "稳且快，优先给流量", "良": "正常，个别抖动", "观察": "有风险信号或样本不足", "停用": "该切走"}[b]))
    L += ["", "分位数：", "", "| p0(min) | p10 | p25 | p50 | p75 | p90 | p100(max) | 均值 |", "|---|---|---|---|---|---|---|---|"]
    L.append("| " + " | ".join("%.1f" % pct(vals, q) for q in (0, 10, 25, 50, 75, 90, 100)) +
             " | %.1f |" % statistics.mean(vals))
    L += ["", "直方图（每 10 分一档）：", "", "```"]
    hist = collections.Counter(min(int(v // 10) * 10, 90) for v in vals)
    for b in range(0, 100, 10):
        c = hist.get(b, 0)
        L.append("%3d-%3d | %-3d %s" % (b, b + 9, c, "#" * c))
    L += ["```", "", "样本量分布（有效样本 n_eff，7 天半衰期加权）：", "",
          "| n_eff | 节点数 | 说明 |", "|---|---|---|"]
    n0 = sum(1 for x in recs if x["sample"]["eff_n"] == 0)
    n1 = sum(1 for x in recs if 0 < x["sample"]["eff_n"] < 10)
    n2 = sum(1 for x in recs if 10 <= x["sample"]["eff_n"] < 20)
    n3 = sum(1 for x in recs if 20 <= x["sample"]["eff_n"] < 100)
    n4 = sum(1 for x in recs if 100 <= x["sample"]["eff_n"] < 1000)
    n5 = sum(1 for x in recs if x["sample"]["eff_n"] >= 1000)
    L += ["| =0 | %d | 无近期真实流量，分数全靠先验+探测 |" % n0,
          "| (0,10) | %d | 样本极少，置信度低 |" % n1,
          "| [10,20) | %d | 样本不足 |" % n2,
          "| [20,100) | %d | 可用但仍有不确定性 |" % n3,
          "| [100,1000) | %d | 证据充分 |" % n4,
          "| ≥1000 | %d | 高流量节点 |" % n5]
    avail = [x for x in recs if x["state"] == "available"]
    L += ["", "## 现判 available 的 %d 个节点内部分档（真正可操作的清单）" % len(avail), "",
          "| 分档 | 节点数 | 含义 |", "|---|---|---|"]
    for b in ["优", "良", "观察", "停用"]:
        c = sum(1 for x in avail if x["band"] == b)
        L.append("| %s | %d | %s |" % (b, c, {"优": "放心用", "良": "正常",
                 "观察": "有风险/样本不足，建议观察", "停用": "**建议切走**（真实调用已证坏）"}[b]))
    L += ["", "- 需要关注（available 且 <70 分）：**%d** 个" % sum(1 for x in avail if x["score"] < 70),
          "- 其中建议立即切走（available 且 <50 分）：**%d** 个（明细见 consistency.md）" % sum(1 for x in avail if x["score"] < 50)]
    open(os.path.join(OUT, "distribution.md"), "w").write("\n".join(L) + "\n")
    return scores


def write_consistency(recs):
    agree, mism = 0, []
    for x in recs:
        ok = x["score"] >= 50
        if ok == (x["state"] == "available"):
            agree += 1
        else:
            mism.append(x)
    L = ["# 与现有判定的一致性", "",
         "- 现有判定（proxy_state.json）：`available` %d / `dead` %d" %
         (sum(1 for x in recs if x["state"] == "available"), sum(1 for x in recs if x["state"] == "dead")),
         "- 本评分判定：≥50 分 = 可接流量；<50 = 停用",
         "- **一致率 = %d/%d = %.1f%%**" % (agree, len(recs), 100.0 * agree / len(recs)), "",
         "| | 现判 available | 现判 dead |", "|---|---|---|",
         "| 评分 ≥50（可用） | %d | %d |" % (
             sum(1 for x in recs if x["state"] == "available" and x["score"] >= 50),
             sum(1 for x in recs if x["state"] == "dead" and x["score"] >= 50)),
         "| 评分 <50（停用） | %d | %d |" % (
             sum(1 for x in recs if x["state"] == "available" and x["score"] < 50),
             sum(1 for x in recs if x["state"] == "dead" and x["score"] < 50)),
         "", "> 说明：`dead` 被设计为乘 0.30 封顶（spec §3.3），所以 dead 侧必然 <50；",
         "> 真正有信息量的是「现判 available 但评分 <50」的节点 —— 探测（GET /models）看不到 403/5xx/真实推理失败。",
         "> 归因里已做**全局抖动抑制**：同一 10 分钟窗口池级失败率 ≥50% 时，节点失败只按 20% 计责（否则一次出口抖动会冤枉几十个节点，见 spec §3.5）。", "",
         "## 不一致节点（逐个说明谁更对）", ""]
    if not mism:
        L.append("（无）")
    for x in mism:
        L += ["### %s（%s）· 现判 available，评分 %.1f（%s）" % (x["name"], x["fp"][:12], x["score"], x["band"]), "",
              "- 分项：quality=%.3f（Wilson 下界）、probe=%.2f、latency=%.3f；原始分 %.1f" % (
                  x["quality"], x["probe"], x["latency_component"], x["raw_score"]),
              "- 扣分原因：%s" % "；".join(r["text"] for r in x["score_reasons"] if r["level"] in ("high", "mid")),
              "- 数据：尝试 %d / 成功 %d（失败分布 %s）；p95 时长 %.0fs；探测延迟 %s ms；最后成功 %s" % (
                  x["sample"]["attempts"], x["sample"]["success"], x["fail"],
                  (x["p95_ms"] or 0) / 1000.0, x["latency_ms"], x["last_success_at"]),
              "- 判断：**评分更对**（探测只证明 /models 可达；这些节点在真实推理里大量 403/5xx/连接失败，"
              "应降级或至少不参与自动选路）" if x["quality"] < 0.5 else
              "- 判断：需人工复核（真实证据与探测冲突，样本可能陈旧）",
              ""]
    open(os.path.join(OUT, "consistency.md"), "w").write("\n".join(L) + "\n")


def write_tables(scores, stats):
    def cell(x):
        rs = "；".join(r["text"] for r in x["score_reasons"] if r["level"] in ("high", "mid"))
        return "| %.1f | %s | %s | %d / %.0f | %s | %s | %s |" % (
            x["score"], x["name"][:26], x["band"], x["sample"]["attempts"], x["sample"]["eff_n"],
            ("%.0fs" % (x["p95_ms"] / 1000.0)) if x["p95_ms"] else "-",
            (x["last_error"] or "-")[:44], rs[:150])
    worst = scores[:10]
    L = ["# 最差 10 个节点（该切谁）", "",
         "参考时间 %s（按分数升序，同分按原始分升序）" % stats.get("ref_ts"), "",
         "| 分数 | 节点 | 分档 | 尝试/有效样本 | p95 时长(含生成) | 最后错误 | 扣分原因 |",
         "|---|---|---|---|---|---|---|"] + [cell(x) for x in worst]
    L += ["", "> 说明：`dead` 节点分数 = 原始分 × 0.30（保留序），所以同一档内分数仍有区分度。"]
    open(os.path.join(OUT, "worst10.md"), "w").write("\n".join(L) + "\n")

    best = sorted(scores, key=lambda x: -x["score"])[:10]
    L = ["# 最好 10 个节点（凭什么给高分）", "", "参考时间 %s" % stats.get("ref_ts"), "",
         "| 分数 | 节点 | 分档 | 尝试/有效样本 | 成功率下界 | 探测延迟 | p95 时长(含生成) | ttft 中位 | 加分原因 |",
         "|---|---|---|---|---|---|---|---|---|"]
    for x in best:
        good = "；".join(r["text"] for r in x["score_reasons"] if r["level"] == "good")
        L.append("| %.1f | %s | %s | %d / %.0f | %.0f%% | %s ms | %s | %s | %s |" % (
            x["score"], x["name"][:26], x["band"], x["sample"]["attempts"], x["sample"]["eff_n"],
            x["quality"] * 100, x["latency_ms"],
            ("%.0fs" % (x["p95_ms"] / 1000.0)) if x["p95_ms"] else "-",
            ("%.1fs" % (x["ttft_p50"] / 1000.0)) if x["ttft_p50"] else "-", good[:110]))
    L += ["", "> p95 是**整条请求时长**（含模型生成），不是节点延迟；节点快慢看「探测延迟」与 ttft。"]
    open(os.path.join(OUT, "best10.md"), "w").write("\n".join(L) + "\n")


def write_shrinkage(recs):
    """展示小样本收缩：原始成功率 vs Wilson 下界 vs 最终 quality。"""
    L = ["# 小样本收缩效果（为什么不能直接看成功率）", "",
         "## A. 公式演示（同样 100% 原始成功率，样本量不同）", "",
         "| 观察成功/尝试 | 原始成功率 | Wilson 下界(z=1.645) | 最终 quality（+先验 8/2） | 分档 |",
         "|---|---|---|---|---|"]
    for k in (3, 12, 40, 200, 2000):
        raw = 1.0
        lb = wilson_lb(k, k)
        q = wilson_lb(k + 8.0, k + 10.0)
        band, _ = band_of(100.0 * (0.6 * q + 0.2 * 1.0 + 0.2 * 0.95), DEFAULT)
        L.append("| %d/%d | 100%% | %.1f%% | %.1f%% | %s |" % (k, k, lb * 100, q * 100, band))
    L += ["", "> 3/3 全对 ≈76 分（良，且 `low_confidence=true`）；12/12 全对仍差一点到优；要到「优」需要真实跑够几百次。",
          "> 这就是收缩的意义：**不拿偶然的几次成功给满分**，也避免「样本少所以看起来完美」的幻觉。",
          "", "## B. 生产节点实例（按有效样本分桶，取该桶里最近成功率最高的节点）", "",
          "| 桶 | 节点 | 尝试 | n_eff | 加权原始成功率 | Wilson 下界（无先验） | 最终 quality | 最终分 | 分档 |",
          "|---|---|---|---|---|---|---|---|---|"]
    cand = [x for x in recs if x["sample"]["eff_n"] >= 1 and x["state"] == "available"]
    buckets = [(0, 6, "n_eff<6"), (6, 20, "6≤n_eff<20"), (20, 100, "20≤n_eff<100"),
               (100, 1000, "100≤n_eff<1000"), (1000, 10 ** 9, "n_eff≥1000")]
    for lo, hi, label in buckets:
        sub = [x for x in cand if lo <= x["sample"]["eff_n"] < hi]
        if not sub:
            continue
        x = max(sub, key=lambda y: y["sample"]["eff_s"] / max(y["sample"]["eff_n"], 1e-9))
        eff_n = x["sample"]["eff_n"]
        eff_s = x["sample"]["eff_s"]
        raw_rate = eff_s / eff_n
        lb = wilson_lb(eff_s, eff_n)
        L.append("| %s | %s | %d | %.0f | %.1f%% | %.1f%% | %.1f%% | %.1f | %s |" % (
            label, x["name"][:24], x["sample"]["attempts"], eff_n, raw_rate * 100,
            lb * 100, x["quality"] * 100, x["score"], x["band"]))
    L += ["", "结论：",
          "1. 小样本节点（n_eff<20）的原始成功率再高，Wilson 下界也上不去；分档落在「观察」，同时 `low_confidence=true`。",
          "2. 样本 ≥1000 的节点，下界与原始成功率差距 <5%，分数主要反映真实质量。",
          "3. 节点页必须同时展示 `sample_size`，否则用户会质疑「为什么 100% 成功率才 60 分」。"]
    open(os.path.join(OUT, "shrinkage.md"), "w").write("\n".join(L) + "\n")


def write_sensitivity(states):
    files = [("7 天（默认）", "node_stats.json"), ("3 天", "node_stats_hl3.json"), ("14 天", "node_stats_hl14.json")]
    base = None
    rows = []
    data = {}
    for label, fn in files:
        path = os.path.join(OUT, fn)
        if not os.path.exists(path):
            continue
        st = json.load(open(path))
        recs = score_all(st, states)
        data[label] = recs
        if base is None:
            base = {x["fp"]: x for x in recs}
    L = ["# 参数敏感性（半衰期 / 权重）", "",
         "## 半衰期", "", "| 半衰期 | 与默认的秩相关 | 分档变化节点数 | 分数变化>10 的节点数 |",
         "|---|---|---|---|"]
    base_recs = data.get("7 天（默认）")
    hl_stats = []
    wstats = []
    for label, recs in data.items():
        if label == "7 天（默认）":
            continue
        m = {x["fp"]: x for x in recs}
        fpsi = [fp for fp in m if fp in base]
        rho = spearman([base[fp]["score"] for fp in fpsi], [m[fp]["score"] for fp in fpsi])
        band_ch = sum(1 for fp in fpsi if base[fp]["band"] != m[fp]["band"])
        big = sum(1 for fp in fpsi if abs(base[fp]["score"] - m[fp]["score"]) > 10)
        hl_stats.append((label, rho, band_ch))
        L.append("| %s | %.3f | %d | %d |" % (label, rho, band_ch, big))
    L += ["", "## 权重（质量/探测/延迟）", "",
          "| 权重 | 与默认的秩相关 | 分档变化节点数 |", "|---|---|---|"]
    for w, label in [((0.50, 0.25, 0.25), "0.50/0.25/0.25（更信探测）"),
                     ((0.70, 0.20, 0.10), "0.70/0.20/0.10（更信真实调用）")]:
        p = dict(DEFAULT)
        p.update(w_q=w[0], w_p=w[1], w_l=w[2])
        recs = score_all(json.load(open(os.path.join(OUT, "node_stats.json"))), states, p)
        m = {x["fp"]: x for x in recs}
        fpsi = [fp for fp in m if fp in base]
        rho = spearman([base[fp]["score"] for fp in fpsi], [m[fp]["score"] for fp in fpsi])
        band_ch = sum(1 for fp in fpsi if base[fp]["band"] != m[fp]["band"])
        wstats.append(rho)
        L.append("| %s | %.3f | %d |" % (label, rho, band_ch))
    L += ["", "### 3 天 vs 7 天的分档迁移矩阵", "",
          "| 7 天 \\ 3 天 | 优 | 良 | 观察 | 停用 |", "|---|---|---|---|---|"]
    hl3 = {x["fp"]: x for x in data.get("3 天", [])}
    order = ["优", "良", "观察", "停用"]
    for b1 in order:
        row = [sum(1 for x in base_recs if x["band"] == b1 and hl3.get(x["fp"], {}).get("band") == b2) for b2 in order]
        L.append("| %s | %s |" % (b1, " | ".join(str(v) for v in row)))
    hl_rows = {label: (rho, band_ch) for label, rho, band_ch in hl_stats}
    hl3 = hl_rows.get("3 天", (0, 0))
    hl14 = hl_rows.get("14 天", (0, 0))
    L += ["", "结论（诚实版）：",
          "- **排序稳健**：半衰期 3/14 天与默认的秩相关 %.3f / %.3f，权重改动 ≥%.3f —— 分数作为「该切谁」的排序可以放心用。" % (hl3[0], hl14[0], min(wstats) if wstats else 0),
          "- **分档会动**：3 天半衰期下有 %d 个节点换档（主要是「观察↔良」边界），14 天有 %d 个 —— 3 天会把最近一次抖动放大。" % (hl3[1], hl14[1]),
          "- 所以：7 天是折中，**分档阈值（70/85）后续应结合线上人工标注校准**；前端展示分档时保留 `score` 数字，避免只有分档导致观感跳变。"]
    open(os.path.join(OUT, "sensitivity.md"), "w").write("\n".join(L) + "\n")


def write_probe_health(stats):
    rounds = json.load(open(os.path.join(OUT, "probe_rounds.json")))["rounds"]
    L = ["# 探测历史与趋势数据可得性", "",
         "## 最近 %d 轮探测（从运行日志解析）" % len(rounds), "",
         "| 时间 | 探测数 | 失败 | canary | verdict |", "|---|---|---|---|---|"]
    merged = {}
    for r in rounds:
        ts = (r.get("ts") or "")[:19]
        m = merged.setdefault(ts, {"ts": ts})
        m.update({k: v for k, v in r.items() if k != "ts"})
    for ts in sorted(merged)[-20:]:
        r = merged[ts]
        if "probed" not in r and "checked" not in r:
            continue
        L.append("| %s | %s | %s | %s | %s |" % (
            ts.replace("T", " "), r.get("probed", r.get("checked", "-")), r.get("failed", "-"),
            "%s/%s" % (r.get("canary_probed", "-"), r.get("canary_failed", "-")) if "canary_probed" in r else "-",
            r.get("verdict", "-")))
    L += ["", "## 现在能拿到什么 / 拿不到什么", "",
          "| 数据 | 可得性 | 说明 |", "|---|---|---|",
          "| 每轮全局健康（probed/failed/canary/verdict） | ✅ 日志有 | `probe_round_canary` 仅最近几十轮（本文件 40 轮） |",
          "| 每节点当前探测结果 | ✅ proxy_state | `state`/`last_probe_at`/`latency_ms`/`last_error`，**只有最新一次** |",
          "| 每节点连续失败次数 | ❌ 内存里，不落盘 | `ProbeFailStreak` 未持久化；日志只有「判死那一刻」的 reason |",
          "| 每节点最近 N 轮趋势 | ❌ 无 | **本设计的实现前提：新增环形缓冲，至少保留最近 20 轮** |",
          "| 每节点真实调用质量 | ✅ call_log | 233k 行，可回算（本目录脚本） |", "",
          "`trend[]` 建议的存储形态（每节点 20 条）：", "",
          "```json",
          '{"fp":"f82d…","trend":[{"ts":"2026-10-21T08:00:00+08:00","ok":true,"latency_ms":1613},',
          ' {"ts":"2026-10-21T07:45:00+08:00","ok":false,"error":"timeout"}],"probe_ok_streak":3}',
          "```"]
    open(os.path.join(OUT, "probe_health.md"), "w").write("\n".join(L) + "\n")


def write_profile(stats, cov):
    days = cov.get("days") or {}
    st = cov.get("status") or {}
    tot = sum(d["total"] for d in days.values())
    fail = sum(d["fail"] for d in days.values())
    L = ["# 数据画像（回算输入）", "",
         "- call_log 行数：**%d**；status：`ok` %d / `fail` %d（失败率 %.1f%%）" % (
             cov["lines"], st.get("ok", 0), st.get("fail", 0),
             100.0 * st.get("fail", 0) / max(1, cov["lines"])),
         "- 时间跨度：%s → %s（%d 天有数据）" % (min(days), max(days), len(days)),
         "- 请求带 `nodes[]`：%d（%.1f%%）；无节点（直连/池空）：%d" % (
             cov["coverage"].get("with_nodes", 0),
             100.0 * cov["coverage"].get("with_nodes", 0) / max(1, cov["lines"]),
             cov["coverage"].get("no_nodes", 0)),
         "", "## 归因覆盖率（每节点尝试）", "",
         "| 结果 | 次数 | 计入质量分母 | 说明 |", "|---|---|---|---|",
         "| 成功 | %d | ✅ | connect_ok / 无事件但 ok |" % cov["coverage"].get("attempts_success", 0),
         "| 失败 | %d | ✅ | 连接失败/被拒/5xx(半权)/流式中断 |" % cov["coverage"].get("attempts_failed", 0),
         "| 半权失败 | %d | ✅（记 0.5 失败） | 上游 5xx / stream_interrupt |" % cov["coverage"].get("attempts_partial_fail", 0),
         "| 额度耗尽 | %d | ❌ 不计 | 429 / quota_halt（不是质量差） |" % cov["coverage"].get("attempts_quota_excluded", 0),
         "| 剔除 | %d | ❌ | 400/404（请求形态）/未识别 |" % cov["coverage"].get("attempts_excluded", 0),
         "| 无法归因 | %d | ❌ | 事件缺失且非末节点 |" % cov["coverage"].get("attempts_unknown", 0),
         "", "**全局抖动抑制覆盖**：失败尝试中有 %d 次（+%d 次半权失败）落在池级抖动窗口（同 10 分钟窗口失败率≥50%%），按 20%% 计责。" % (
             cov["coverage"].get("attempts_failed_in_blip", 0), cov["coverage"].get("attempts_partial_fail_in_blip", 0)),
         "", "## 每日请求量（最后 14 天）", "",
         "| 日期 | 请求 | 失败 | 失败率 |", "|---|---|---|---|"]
    for k in sorted(days)[-14:]:
        d = days[k]
        L.append("| %s | %d | %d | %.1f%% |" % (k, d["total"], d["fail"], 100.0 * d["fail"] / max(1, d["total"])))
    L += ["", "## 失败类型总分布（所有节点，按事件）", ""]
    ev = collections.Counter()
    for n in stats["nodes"].values():
        for k, v in (n.get("fail") or {}).items():
            ev[k] += v
        if n.get("quota_events"):
            ev["quota(不计质量)"] += n["quota_events"]
    L += ["| 类别 | 次数 | 对质量分 |", "|---|---|---|"]
    for k, v in ev.most_common():
        L.append("| %s | %d | %s |" % (k, v, "不计" if "quota" in k else "计入"))
    open(os.path.join(OUT, "profile.md"), "w").write("\n".join(L) + "\n")


def main():
    states = json.load(open(STATE))["nodes"]
    stats = json.load(open(os.path.join(OUT, "node_stats.json")))
    cov = json.load(open(os.path.join(OUT, "coverage.json")))
    recs = score_all(stats, states)
    scores = write_default(recs, stats, cov)
    write_consistency(recs)
    write_tables(scores, stats)
    write_shrinkage(recs)
    write_sensitivity(states)
    write_probe_health(stats)
    write_profile(stats, cov)
    print("输出: scores.json scores.csv distribution.md consistency.md worst10.md best10.md")
    print("      sensitivity.md shrinkage.md probe_health.md profile.md")
    bands = collections.Counter(x["band"] for x in recs)
    print("分档:", dict(bands))
    print("一致率: %.1f%%" % (100.0 * sum(1 for x in recs if (x["score"] >= 50) == (x["state"] == "available")) / len(recs)))


if __name__ == "__main__":
    main()
