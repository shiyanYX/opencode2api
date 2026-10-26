#!/usr/bin/env python3
"""task-69 变异测试：对 web/pages/logs.js 做 5 个真实回归变异，每个都必须让 web/verify/logs.py 变红。

用法：python3 .research/t69/mutate.py    （约 5 分钟）
每个变异：改源 → build.py → verify.py --no-build（不重复 build）→ 期望非 0 退出 → 还原 → 重建。
"""
import pathlib, subprocess, sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
SRC = ROOT / "web" / "pages" / "logs.js"
VERIFY = ROOT / "web" / "verify" / "logs.py"

MUTATIONS = [
    ("M1 Stat 请求数改成成功数（口径回归）",
     "C.stat('请求数', C.fmt.num(req), RANGE_LABEL[d.range] || ''),",
     "C.stat('请求数', C.fmt.num(ok), RANGE_LABEL[d.range] || ''),"),
    ("M2 加载更多丢掉游标（退化成重复取第一页）",
     "      const qs = new URLSearchParams({ limit: '200' });\n      if (cursor) qs.set('cursor', cursor);\n      const r = await fetch('/api/call-log?'",
     "      const qs = new URLSearchParams({ limit: '200' });\n      const r = await fetch('/api/call-log?'"),
    ("M3 切页不关 EventSource（连接泄漏）",
     "          S.rt.es.close();",
     "          /* M3: 故意不 close */"),
    ("M4 运行日志级别筛选失效",
     "        if (!st.levels.has(String(e.level || '').toUpperCase())) return false;",
     "        if (false) return false;"),
    ("M5 引入外部 CDN 请求（零外部请求被破坏）",
     "    /* ---------- 骨架 ---------- */",
     "    fetch('https://cdn.example.com/echarts.min.js').catch(function () {});\n    /* ---------- 骨架 ---------- */"),
]

def run(cmd):
    return subprocess.run(cmd, cwd=str(ROOT), capture_output=True, text=True, timeout=300)

def main():
    orig = SRC.read_text(encoding="utf-8")
    caught, missed = [], []
    try:
        for name, old, new in MUTATIONS:
            assert orig.count(old) == 1, f"变异锚点在当前源码里不唯一/不存在: {name} ({orig.count(old)})"
            SRC.write_text(orig.replace(old, new), encoding="utf-8")
            b = run([sys.executable, "web/build.py"])
            assert b.returncode == 0, b.stderr
            v = run([sys.executable, "web/verify/logs.py", "--no-build"])
            tail = (v.stdout.strip().splitlines()[-6:] if v.stdout.strip() else []) + \
                   (v.stderr.strip().splitlines()[-4:] if v.stderr.strip() else [])
            if v.returncode != 0:
                caught.append(name)
                print(f"CAUGHT  {name}")
                print("        " + " | ".join(t for t in tail if t)[:400])
            else:
                missed.append(name)
                print(f"MISSED  {name}")
        SRC.write_text(orig, encoding="utf-8")
        run([sys.executable, "web/build.py"])
        print(f"\n===== {len(caught)}/{len(MUTATIONS)} CAUGHT =====")
        if missed:
            print("MISSED:", missed)
            sys.exit(1)
        print("ALL_MUTATIONS_CAUGHT")
    finally:
        SRC.write_text(orig, encoding="utf-8")
        run([sys.executable, "web/build.py"])

if __name__ == "__main__":
    main()
