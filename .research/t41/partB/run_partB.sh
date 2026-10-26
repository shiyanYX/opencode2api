#!/usr/bin/env bash
# Part B 总驱动（Lead 2026-10-20 放行版）。
# pin 以 Lead 的 /tmp/t41b/pin_lead.txt 为唯一权威（81 行：*.go 80 + free_models_cache.json；面板已排除）。
# 任何 .go / cache 变化 ⇒ INVALID 并立刻报 Lead。全程照实记录，失败不静默。
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
source "$HERE/lib.sh"
RES="$WORK/results"; LOGS="$WORK/logs"
mkdir -p "$RES" "$LOGS"
FAIL=(); declare -A STATUS
say() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "$RES/run.log"; }
stage() { say "===== $* ====="; }
record() { STATUS[$1]=$2; [ "$2" = ok ] || FAIL+=("$1"); }
runstage() { local name=$1; shift; stage "$name"
  if "$@" >>"$RES/run.log" 2>&1; then record "$name" ok; else record "$name" fail; say "❌ $name 失败"; fi; }

if [ ! -f "$WORK/START" ]; then echo "⏸ 缺 $WORK/START"; exit 9; fi
say "Part B 开跑 REPO=$REPO"

# ---------- stage 0：Lead 权威 pin 校验 + 冻结 + 构建 ----------
stage "0. Lead 权威 pin 校验"
pin_file_ok && record "0a-pin-file" ok || { record "0a-pin-file" fail; say "❌ pin_lead.txt 指纹不匹配 ${LEAD_PIN_SHA}"; }
( pin_check > "$RES/pin-check.txt" 2>&1 ) && record "0b-pin-check" ok || { record "0b-pin-check" fail; say "❌ 逐文件 pin 校验失败（见 results/pin-check.txt）"; }
grep -c "OK$\|成功$" "$RES/pin-check.txt" | sed 's/^/逐文件通过行数: /' | tee -a "$RES/run.log"
pin_save freeze; pin_show | tee "$RES/pin-freeze.txt" >/dev/null
( cd "$REPO" && md5sum static/admin.html > "$RES/panel-freeze.md5.txt"; sha256sum static/admin.html > "$RES/panel-freeze.sha256.txt" ) 2>/dev/null
rsync -a --delete --exclude .git --exclude .gocache --exclude .modcache --exclude .gotmp \
      --exclude 'server-*' "$REPO/" "$FROZEN/" && record "0c-frozen-copy" ok || record "0c-frozen-copy" fail
if diff -q <(pin_show) <(cd "$FROZEN" && awk '{print $2}' "$LEAD_PIN" | xargs sha256sum | sort -k2) >/dev/null; then
  record "0d-frozen==worktree" ok
else
  record "0d-frozen==worktree" fail; say "❌ 冻结副本与工作树 pin 不一致（拷贝期间有改动）"
fi
runstage "0e-build-old(HEAD)" build_old_from_head
runstage "0f-build-new(frozen)" build_from "$FROZEN" new
stage "0g. 隔离基线构建（冻结树 + M5 中和 F1 过滤）"
rsync -a --delete "$FROZEN/" "$WORK/nofilter-src/" \
  && python3 "$HERE/mutate.py" --apply M5 --dir "$WORK/nofilter-src" >>"$RES/run.log" 2>&1 \
  && build_from "$WORK/nofilter-src" nofilter >>"$RES/run.log" 2>&1 \
  && record "0g-build-nofilter" ok || record "0g-build-nofilter" fail

# ---------- stage 1：夹具 ----------
stage "1. 夹具"
python3 "$HERE/mkfixture.py" --cases orig,shuffled,nopickle --src "${SNAPSHOT:-/tmp/t41/live.jsonl}" --out "$WORK/fixture" >>"$RES/run.log" 2>&1 \
  && record "1-fixtures" ok || record "1-fixtures" fail

# ---------- stage 2：/api/stats 双模式差异验收 ----------
runstage "2a-api-diff-isolation" python3 "$HERE/api_diff.py" --mode isolation --old nofilter --new new || true
rm -rf "$WORK/run"/*
runstage "2b-api-diff-e2e" python3 "$HERE/api_diff.py" --mode e2e --old old --new new
rm -rf "$WORK/run"/*

# ---------- stage 3：行序无关 ----------
runstage "3-order" python3 "$HERE/order_test.py" --tag new
rm -rf "$WORK/run"/*

# ---------- stage 4：重启时序 ----------
runstage "4-restart" python3 "$HERE/restart_test.py" --tag new --repeat 3
rm -rf "$WORK/run"/*

# ---------- stage 5：变异 ----------
runstage "5-mutations" python3 "$HERE/mutate.py" --full-suite-on-green \
  --run 'T39|Unmapped|StatsRange|ModelStats|Trends|TodayRange|Window' 
rm -rf "$WORK/mut"/*

# ---------- stage 6：实现者测试（跳过并发面板相关的那条） ----------
stage "6. go test（-skip TestT29…）"
( cd "$FROZEN" && go test -count=1 -timeout 30m -skip 'TestT29PlainIsByteIdenticalToEmbeddedPanel' ./ >"$LOGS/gotest.log" 2>&1 )
gt=$?; [ $gt -eq 0 ] && record "6a-go-test" ok || record "6a-go-test" known-red
fails=$(grep -E '^--- FAIL' "$LOGS/gotest.log" | sed 's/^--- FAIL: //; s/ .*//' | sort)
echo "失败用例:" | tee -a "$RES/run.log"; printf '%s\n' "$fails" | tee -a "$RES/run.log"
exp=$'TestT30CriterionCPoolEmptyReachability\nTestT30GateNowJudgesFixedSubsetDead'
if [ "$fails" = "$exp" ]; then record "6b-baseline-fails-expected" ok
else record "6b-baseline-fails-expected" fail; say "❌ 预期失败集合不符（可能是新引入失败）"; fi
# 独立复核：这两条与 task-39 无关（断言内容里不含 unmapped/served/stats 口径）
grep -l "TestT30GateNowJudgesFixedSubsetDead" zz_t30_*.go 2>/dev/null | head -2 | tee -a "$RES/run.log"
grep -nE "unmapped|everServed|modelsWithOKInWindow|filterTrendsByOK" zz_t30_*.go 2>/dev/null | head -3 | tee -a "$RES/run.log" || true
stage "6c. go test -race"
# 本轮取舍：全量 -race 在 4 核机上需 10min+，且与变异/服务测试抢 CPU 会互相拖慢；
# 它不是 Lead 列出的交付项（交付项=全量 go test + 变异必红 + 双出口一致性）。
say "SKIP: -race 未纳入本轮（非交付项；理由与替代证据见报告 §8）"
STATUS[6c-go-test-race]=skipped

# ---------- stage 7：措辞（面板信息性） ----------
runstage "7-wording" bash "$HERE/panel_wording_check.sh"

# ---------- stage 8：pin 复核 ----------
stage "8. pin 复核"
if pin_verify freeze; then record "8-pin-stable" ok; else record "8-pin-stable" fail; say "❌ 运行期间 pin 漂移 → INVALID"; fi
pin_show > "$RES/pin-end.txt"

{ echo "# Part B 结果汇总"; echo
  echo "- 开始: $(cat "$WORK/pin.freeze.stamp" 2>/dev/null)"; echo "- 结束: $(date -Is)"
  echo "- Lead pin 指纹: $(sha256sum "$LEAD_PIN" | awk '{print $1}')（期望 ${LEAD_PIN_SHA}）"
  echo "- pin 稳定: ${STATUS[8-pin-stable]:-unknown}"; echo
  echo "| 阶段 | 结果 |"; echo "|---|---|"
  for k in $(printf '%s\n' "${!STATUS[@]}" | sort); do echo "| $k | ${STATUS[$k]} |"; done; echo
  if [ ${#FAIL[@]} -gt 0 ]; then echo "## ❌ 未通过"; for f in "${FAIL[@]}"; do echo "- $f"; done
  else echo "## ✅ 全部阶段通过"; fi; } > "$RES/summary.md"
cat "$RES/summary.md"
[ ${#FAIL[@]} -eq 0 ]
