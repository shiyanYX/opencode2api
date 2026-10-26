import os, glob, shutil, subprocess, sys

REPO = "/home/jingyx/project/opencode2api-test"
MUT = "/tmp/t67/mut"
os.makedirs(MUT, exist_ok=True)
os.makedirs("/tmp/t67/gotmp", exist_ok=True)
for f in glob.glob(os.path.join(REPO, "*.go")) + [os.path.join(REPO, "go.mod"), os.path.join(REPO, "go.sum")]:
    shutil.copy2(f, MUT)
# 包里有 //go:embed static/*.html（embed.go），缺了会 setup failed 而不是测试失败
shutil.copytree(os.path.join(REPO, "static"), os.path.join(MUT, "static"), dirs_exist_ok=True)
GOFILE = os.path.join(MUT, "calllog.go")

env = os.environ.copy()
env["PATH"] = env["PATH"] + ":" + os.path.expanduser("~/.local/opt/go/bin") + ":/usr/local/go/bin"
env.update({
    "GOCACHE": os.path.join(REPO, ".gocache"),
    "GOMODCACHE": os.path.join(REPO, ".modcache"),
    "GOTMPDIR": "/tmp/t67/gotmp",
    "GOFLAGS": "-count=1",
})

MUTATIONS = [
 dict(n="M18 [AGG] 分位桶算错（所有值塞进第 0 桶）",
      test="TestAggregatePercentilesKnownDistribution",
      old="\ti := len(bounds) - 1\n\tfor k := 0; k < len(bounds)-1; k++ {",
      new="\ti := 0\n\tfor k := 0; k < len(bounds)-1 && false; k++ {"),
 dict(n="M19 [AGG] by_model 漏算失败（Fail 恒 0）",
      test="TestAggregateTotalsAndTimeseriesMatchStats",
      old="\t\t\tFail:     a.Fail,",
      new="\t\t\tFail:     0,"),
 dict(n="M20 [AGG] attempts 把 0 与 1 合并",
      test="TestAggregateAttemptsPhasesAndErrors",
      old="\tcase r.Nodes <= 0:\n\t\tres.attempts.A0++\n\tcase r.Nodes == 1:\n\t\tres.attempts.A1++",
      new="\tcase r.Nodes <= 1:\n\t\tres.attempts.A1++"),
 dict(n="M21 [AGG] 缓存键漏 range",
      test="TestAggregateCacheKeyIncludesRangeAndDate",
      old="\treturn rng + \"|\" + now.In(time.Local).Format(\"2006-01-02\") + \"|\" + storePath + \"|\" + storeDir",
      new="\treturn now.In(time.Local).Format(\"2006-01-02\") + \"|\" + storePath + \"|\" + storeDir"),
 dict(n="M22 [AGG] by_model p95 用全局直方图（不分模型）",
      test="TestAggregateByModelP95DistinguishesModels",
      old="\t\trow.P95Ms = aggHistQuantile(scan.perModel[name], aggDurBucketsMS, 0.95, scan.perModelMax[name])",
      new="\t\trow.P95Ms = aggHistQuantile(scan.durHist, aggDurBucketsMS, 0.95, scan.maxDur)"),
 dict(n="M23 [AGG] 老行未映射不回退（只认显式 unmapped 键）",
      test="TestAggregateOldRowsUnmappedExcludedFromScanDims",
      old="\tif r.SawUnmapped {\n\t\tif r.Unmapped && r.Status != \"ok\" {\n\t\t\treturn\n\t\t}\n\t} else {\n\t\tv, hit := memo[r.Model]\n\t\tif !hit {\n\t\t\t// 与桶吸收同判据：公式 B && !everServed（曾成功过的名字永久 mapped）。\n\t\t\tv = isUnmappedByFormula(r.Model) && !isServedModel(r.Model)\n\t\t\tmemo[r.Model] = v\n\t\t}\n\t\tif v {\n\t\t\treturn\n\t\t}\n\t}",
      new="\tif r.Unmapped && r.Status != \"ok\" {\n\t\treturn\n\t}"),
 dict(n="M24 [AGG] per-model p95 仍借全局 max",
      test="TestAggregatePerModelP95UnboundedOwnMax",
      old="\t\trow.P95Ms = aggHistQuantile(scan.perModel[name], aggDurBucketsMS, 0.95, scan.perModelMax[name])",
      new="\t\trow.P95Ms = aggHistQuantile(scan.perModel[name], aggDurBucketsMS, 0.95, scan.maxDur)"),
]

orig = open(GOFILE, encoding="utf-8").read()
results = []
for m in MUTATIONS:
    src = open(GOFILE, encoding="utf-8").read()
    if src.count(m["old"]) != 1:
        results.append((m["n"], m["test"], "ANCHOR_MISS", src.count(m["old"]), ""))
        continue
    open(GOFILE, "w", encoding="utf-8").write(src.replace(m["old"], m["new"], 1))
    try:
        p = subprocess.run(["go", "test", "-run", "^" + m["test"] + "$", "-count=1", "./"],
                           cwd=MUT, env=env, capture_output=True, text=True, timeout=300)
        out = (p.stdout + p.stderr).strip().splitlines()
        tail = " | ".join([l for l in out if "--- FAIL" in l or "FAIL" in l or "build failed" in l][:2]) or (out[-1] if out else "")
        caught = p.returncode != 0 and ("FAIL" in p.stdout + p.stderr)
        results.append((m["n"], m["test"], "CAUGHT" if caught else "MISSED", p.returncode, tail))
    except subprocess.TimeoutExpired:
        results.append((m["n"], m["test"], "TIMEOUT", -1, ""))
    finally:
        open(GOFILE, "w", encoding="utf-8").write(src)

print(f"{'mutation':46s} {'test':46s} {'result':8s} rc")
for r in results:
    print(f"{r[0]:46s} {r[1]:46s} {r[2]:8s} {r[3]}  {r[4][:90]}")
sys.exit(0 if all(r[2] == "CAUGHT" for r in results) else 1)
