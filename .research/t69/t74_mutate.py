#!/usr/bin/env python3
"""task-74 变异：把 fetchCallLog 的 status 参数去掉（回到内存环无游标的旧行为），
web/verify/logs.py 必须转红；还原后必须转绿。"""
import pathlib, subprocess, sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
SRC = ROOT / "web/pages/logs.js"
OLD = "        status: CL_STATUS_PARAM[S.dt.status] || 'all',\n"
orig = SRC.read_text(encoding="utf-8")
assert orig.count(OLD) == 1, orig.count(OLD)
try:
    SRC.write_text(orig.replace(OLD, ""), encoding="utf-8")
    subprocess.run([sys.executable, "web/build.py"], cwd=str(ROOT), check=True, capture_output=True)
    v = subprocess.run([sys.executable, "web/verify/logs.py"], cwd=str(ROOT), capture_output=True, text=True, timeout=600)
    red_lines = [l for l in v.stdout.splitlines() if l.startswith("FAIL")]
    print("mutation: 去掉 status 参数（回到内存环分支）")
    print(f"verify exit={v.returncode}  FAIL行数={len(red_lines)}")
    for l in red_lines[:6]:
        print("  " + l)
    ok = v.returncode != 0 and any("首屏明细请求带过滤参数" in l or "首屏响应来自文件分页" in l for l in red_lines)
    print("CAUGHT" if ok else "MISSED")
    sys.exit(0 if ok else 1)
finally:
    SRC.write_text(orig, encoding="utf-8")
    subprocess.run([sys.executable, "web/build.py"], cwd=str(ROOT), check=True, capture_output=True)
