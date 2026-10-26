import os, subprocess, sys

MUT = "/tmp/t56/mut"
GOFILE = os.path.join(MUT, "calllog.go")
env = os.environ.copy()
env["PATH"] = env["PATH"] + ":/usr/local/go/bin"
env.update({
    "GOCACHE": "/home/jingyx/project/opencode2api-test/.gocache",
    "GOMODCACHE": "/home/jingyx/project/opencode2api-test/.modcache",
    "GOTMPDIR": "/tmp/t56/gotmp",
    "GOFLAGS": "-count=1",
})

MUTATIONS = [
 dict(n="M1 段排序 legacy 放最后", test="TestDailySegmentsOrderLegacyFirstThenDaysAscending",
  old="\t\tif segs[i].legacy != segs[j].legacy {\n\t\t\treturn segs[i].legacy // legacy 恒第 0 段\n\t\t}",
  new="\t\tif segs[i].legacy != segs[j].legacy {\n\t\t\treturn !segs[i].legacy\n\t\t}"),
 dict(n="M2 日切仍写最后一段（不建新天文件）", test="TestDailyRolloverNeverWritesBackToOlderSegment",
  old="\t\tif !last.legacy && last.day >= today {\n\t\t\treturn last.path // 已有今天（或时钟回拨）的段：追加\n\t\t}",
  new="\t\tif !last.legacy {\n\t\t\treturn last.path\n\t\t}"),
 dict(n="M3 events 精简不做深拷贝", test="TestDailyDiskTrimDefaultFieldsAndRestore",
  old="\t\tevs := make([]CallEvent, len(d.Events))\n\t\tcopy(evs, d.Events)",
  new="\t\tevs := d.Events"),
 dict(n="M4 非最后段半行不消费", test="TestDailyNonLastSegmentPartialLineConsumed",
  old="\t\t\t} else if !isLast {",
  new="\t\t\t} else if false && !isLast {"),
 dict(n="M5 尾部加载只读最后一段", test="TestDailyLoadCallLogTailAcrossSegments",
  old="\tfor i := len(segs) - 1; i >= 0 && need > 0; i-- {",
  new="\tfor i := len(segs) - 1; i == len(segs)-1; i-- {"),
 dict(n="M6 删除超期段整段逻辑（不 prune）", test="TestRetentionArchiveCompressesOldDaysAndPrunes",
  old="\tif pruned > 0 {\n\t\tdailySegsKey = \"\"\n\t\ts.ensureDailySegsLocked() // 重建列表并按路径保住游标\n\t}",
  new="\tpruned = 0\n\t_ = pruned"),
 dict(n="M7 before 等值边界变包含（同 ts 组按普通行处理）", test="TestCallLogHistoryFilters",
  old="""\t\t\t\tif ts.Equal(f.before) {
\t\t\t\t\tif f.beforeReqID == "" {
\t\t\t\t\t\tcontinue // 排他：整组都不返回
\t\t\t\t\t}
\t\t\t\t\tvar rec CallRecord
\t\t\t\t\tif json.Unmarshal(line, &rec) != nil || rec.ReqID == "" {
\t\t\t\t\t\tcontinue
\t\t\t\t\t}
\t\t\t\t\trestoreDiskRecord(&rec)
\t\t\t\t\ttsBuf = append(tsBuf, histHit{rec: rec, seg: i, line: j})
\t\t\t\t\tcontinue
\t\t\t\t}""",
  new="""\t\t\t\t_ = tsBuf"""),
 dict(n="M8 读取侧不还原 node_name", test="TestDailyDiskTrimDefaultFieldsAndRestore",
  old="\t\tfor j, fp := range rec.Nodes {\n\t\t\tif fp == rec.Events[i].Node && j < len(rec.NodeNames) {\n\t\t\t\trec.Events[i].NodeName = rec.NodeNames[j]\n\t\t\t\tbreak\n\t\t\t}\n\t\t}",
  new="\t\t_ = rec.Nodes"),
 dict(n="M9 clear 不删日文件", test="TestDailyClearRemovesEverySegment",
  old="\tif s.dir != \"\" {\n\t\tif ents, err := os.ReadDir(s.dir); err == nil {\n\t\t\tfor _, e := range ents {\n\t\t\t\tif e.IsDir() {\n\t\t\t\t\tcontinue\n\t\t\t\t}\n\t\t\t\tif err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil {\n\t\t\t\t\tslog.Warn(\"calllog: cannot remove day segment on clear\", \"file\", e.Name(), \"error\", err)\n\t\t\t\t}",
  new="\tif false && s.dir != \"\" {\n\t\tif ents, err := os.ReadDir(s.dir); err == nil {\n\t\t\tfor _, e := range ents {\n\t\t\t\tif e.IsDir() {\n\t\t\t\t\tcontinue\n\t\t\t\t}\n\t\t\t\tif err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil {\n\t\t\t\t\tslog.Warn(\"calllog: cannot remove day segment on clear\", \"file\", e.Name(), \"error\", err)\n\t\t\t\t}"),
 dict(n="M10 吸收时 gz 段当明文读", test="TestDailyGzSegmentAbsorb",
  old="\tif seg.gz {\n\t\tzr, zerr := gzip.NewReader(f)",
  new="\tif false && seg.gz {\n\t\tzr, zerr := gzip.NewReader(f)"),
 dict(n="M11 daily 游标推进用 parsedTo", test="TestDailyAppendWritesTodaysFileAndIncr",
  old="\t\t\tif s.dir != \"\" {\n\t\t\t\t// daily 模式：游标指向最后一段末尾（与 parsedTo == size 同构）\n\t\t\t\tif n := len(dailySegs); n > 0 && dailySegs[n-1].path == p {\n\t\t\t\t\tdailyCur = absorbCursor{idx: n - 1, off: dailyCur.off + int64(len(line))}\n\t\t\t\t\tdailySegs[n-1].size += int64(len(line))\n\t\t\t\t}\n\t\t\t} else {\n\t\t\t\tparsedTo += int64(len(line))\n\t\t\t}",
  new="\t\t\tparsedTo += int64(len(line))\n\t\t\t_ = p"),
 dict(n="M12 daily 模式不分派，走单文件吸收", test="TestDailyAbsorbReplaysAllSegmentsIdempotently",
  old="\tif dir != \"\" {\n\t\tabsorbOnceDaily()\n\t\treturn\n\t}",
  new="\tif false && dir != \"\" {\n\t\tabsorbOnceDaily()\n\t\treturn\n\t}"),
 # ---- task-63（D1/D2 修复）新增：这两个变异就是验收缺陷本身 ----
 dict(n="M13 [D1] callLogFileExists 忽略 daily 段（缺陷本体）", test="TestDailyOnlyDeploymentTrendsNotEmpty",
  old="""\tif dir != "" {
\t\t// daily / legacy+日文件布局：存在任一段即算存在
\t\treturn len(callLog.readSegments()) > 0
\t}""",
  new="""\tif false && dir != "" {
\t\t// daily / legacy+日文件布局：存在任一段即算存在
\t\treturn len(callLog.readSegments()) > 0
\t}"""),
 dict(n="M14 [D2] 同 ts 游标退回 req_id 字符串过滤（缺陷本体）", test="TestSameTSGroupPaginationNoLoss",
  old="""\t\tif pos >= 0 {""",
  new="""\t\tif false && pos >= 0 {"""),
 # ---- task-65（D3 修复）新增 ----
 dict(n="M15 [D3] 位置游标 off-by-one（游标行重复返回）", test="TestProdOrderPaginationCursorUnionEqualsFull",
  old="""\t\t\tjFrom = cursorLine - 1""",
  new="""\t\t\tjFrom = cursorLine"""),
 dict(n="M16 [D3] 段标识用文件路径（明文→gz 改名后游标失效）", test="TestCursorSurvivesGzipRename",
  old="""\tif seg.day != "" {
\t\treturn seg.day
\t}""",
  new="""\tif seg.day != "" {
\t\treturn filepath.Base(seg.path)
\t}"""),
 dict(n="M17 [D3] 位置游标不产出（Next-Cursor 缺失）", test="TestProdOrderPaginationCursorUnionEqualsFull",
  old="""\tif hasMore && lastSeg >= 0 {""",
  new="""\tif false && hasMore && lastSeg >= 0 {"""),
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

print(f"{'mutation':42s} {'test':52s} {'result':8s} rc")
for r in results:
    print(f"{r[0]:42s} {r[1]:52s} {r[2]:8s} {r[3]}  {r[4][:100]}")
sys.exit(0 if all(r[2] == "CAUGHT" for r in results) else 1)
