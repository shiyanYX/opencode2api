#!/usr/bin/env bash
# Part B：措辞红线检查 + 面板信息性观察。
# Lead 2026-10-20 裁决：面板 static/admin.html 正被并发任务改写，已从 pin 范围排除，
# **「面板零改动」不作为本次验收项**（由 Lead 在提交时用暂存范围证明）。
# 本脚本只做：(1) 措辞红线（口径更正 ≠ 成功率提升）—— 这条仍然有效且会判失败；
#            (2) 面板哈希/依赖字段打印，仅作信息记录，不参与通过判定。
set -uo pipefail
source "$(dirname "$0")/lib.sh"
RES="$WORK/results"; mkdir -p "$RES"
OUT="$RES/panel-wording.txt"
: > "$OUT"
viol=0

echo "== 1. 面板文件 ==" | tee -a "$OUT"
PANEL="$REPO/static/admin.html"
md5sum "$PANEL" | tee -a "$OUT"
sha256sum "$PANEL" | tee -a "$OUT"
lines=$(wc -l < "$PANEL"); echo "行数=$lines" | tee -a "$OUT"
# Lead 2026-10-20：theme 任务已停手冻结在 md5=374ba326… / sha256=6364641c…；
# Part B 期间该文件若变化，那一定不是 theme 干的 → 记 finding。
for algo in md5 sha256; do
  want_file="$WORK/results/panel-freeze.$algo.txt"
  [ -f "$want_file" ] || continue
  want=$(awk '{print $1}' "$want_file")
  now=$("$algo"sum "$PANEL" | awk '{print $1}')
  if [ "$want" != "$now" ]; then
    echo "INFO: 面板 $algo 与 Part B 开始时不同（$want -> $now）—— 并发面板任务所致，不参与验收" | tee -a "$OUT"
  else
    echo "INFO: 面板 $algo 未变（$now）—— 仅记录，不作为验收项" | tee -a "$OUT"
  fi
done

echo | tee -a "$OUT"
echo "== 2. 面板是否依赖新字段语义 ==" | tee -a "$OUT"
for pat in 'unmapped\|everServed\|servedModel'; do
  n=$(grep -c -E "$pat" "$PANEL" 2>/dev/null || true)
  echo "  INFO: 面板命中 /$pat/ = ${n:-0}（仅记录；面板由并发任务负责，不参与本次验收）" | tee -a "$OUT"
done
n=$(grep -c 'trends_by_model' "$PANEL" 2>/dev/null || true)
echo "  面板使用 trends_by_model = ${n:-0}（期望 >0：过滤后仍应正常渲染）" | tee -a "$OUT"
[ "${n:-0}" = "0" ] && echo "  NOTE: 面板若已重写、可能换了字段名，需人工确认" | tee -a "$OUT"

echo | tee -a "$OUT"
echo "== 3. 措辞红线（口径更正 ≠ 成功率提升）==" | tee -a "$OUT"
RAW=$(grep -rn -E '成功率提升|提升成功率|服务改善|性能提升|服务变好' \
        "$REPO/docs/research" "$REPO/ROADMAP.md" 2>/dev/null || true)
HITS=$(printf '%s\n' "$RAW" | grep -v '禁止\|不得\|不是\|≠\|不能写\|未\|没有\|而非\|避免\|别\|口径更正' || true)
echo "  原始命中 $(printf '%s\n' "$RAW" | grep -c . || true) 行；排除否定语境后 $(printf '%s\n' "$HITS" | grep -c . || true) 行" | tee -a "$OUT"
if [ -n "$HITS" ]; then
  echo "❌ 发现疑似「提升」话术：" | tee -a "$OUT"
  echo "$HITS" | tee -a "$OUT"
  viol=$((viol+1))
else
  echo "OK: 未发现未经否定的「提升/改善」话术" | tee -a "$OUT"
fi

echo | tee -a "$OUT"
echo "== 4. 必需的「口径」表述 ==" | tee -a "$OUT"
DOC="$REPO/docs/research/overview-vs-detail-served-models.md"
if [ -f "$DOC" ]; then
  c=$(grep -c '口径' "$DOC" || true)
  echo "  实现者报告存在，出现「口径」${c} 次" | tee -a "$OUT"
  grep -n '垃圾请求\|未映射\|everServed' "$DOC" | head -8 | tee -a "$OUT"
else
  echo "NOTE: 实现者报告尚不存在（$DOC）" | tee -a "$OUT"
fi

echo | tee -a "$OUT"
if [ "$viol" -ne 0 ]; then echo "❌ 面板/措辞检查不通过（$viol 项）" | tee -a "$OUT"; exit 1; fi
echo "✅ 面板/措辞检查通过" | tee -a "$OUT"
