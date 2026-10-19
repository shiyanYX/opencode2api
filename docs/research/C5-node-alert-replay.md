# C-5 回放验收：用生产数据验证节点层告警

目的：证明 `node_alert.go` 的判据在**真实事故数据**上会响、且不在正常时段误报。
复现步骤（数据是生产导出，放 /tmp，不进仓库）：

```bash
# 1) 从生产容器取调用日志（123MB）
docker cp opencode2api:/data/call_log.jsonl /tmp/prod_call_log.jsonl

# 2) 抽出「节点出口尝试」时间线：at<TAB>ok
python3 - <<'PY'
import json, datetime
rows=[]
with open('/tmp/prod_call_log.jsonl') as f:
    for line in f:
        try: r=json.loads(line)
        except: continue
        for e in (r.get('events') or []):
            if not (e.get('node') or e.get('node_name')): continue
            t=e.get('type')
            if t=='connect_error': ok='0'                       # 传输层失败
            elif t in ('connect_ok','upstream_error'): ok='1'    # 拿到上游响应 = 出口是通的
            else: continue                                      # switch/freetier_* 不是一次尝试
            at=e.get('at') or e.get('time')
            if not at: continue
            try: dt=datetime.datetime.fromisoformat(at.replace('Z','+00:00'))
            except: continue
            rows.append((dt.isoformat(), ok))
rows.sort()
with open('/tmp/prod_node_events.tsv','w') as g:
    for at,ok in rows: g.write(f"{at}\t{ok}\n")
print('样本', len(rows))
PY

# 3) 跑回放（真引擎，每分钟评估一次，池判据关闭以隔离"成功率 0%"这条）
go test -run TestReplayProductionNodeOutage -count=1 -v .
```

## 2026-10-19 结果

- 样本 224,425 条（成功 198,331 / 失败 26,094），窗口 08-14 01 .. 10-19 11（本地时区）。
- 告警共触发 **37** 次（半年内约 1.5 天一次，每次一条 ERROR；30 分钟冷却生效）。
- **第一次与本次事故吻合的触发：2026-10-19 00:10:16**
  —— 逐分钟数据显示节点出口从 **00:01 起零成功**（00:01–00:13 共 226 次尝试 0 成功），
  窗口滑满 10 分钟后立即触发。人工发现是次日上午 ⇒ **提前约 11 小时**。
- 最近一次触发：2026-10-19 11:02:04（27 次尝试 0 成功），即调查时那一段。
- 10-18 09:22 → 10-04 之间无触发（那段时间节点成功率正常）⇒ 正常时段不误报。

## 口径说明（结论的保守方向）

- 回放把 `upstream_error`（403/429/5xx）算作**成功**：与生产接线一致 —— 拿到响应就说明出口是通的。
- 池状态未知，一律传 `total=1/available=1` ⇒ 回放**只考验**「成功率 0% 持续」这一条判据。
- 每分钟才评估一次（= 真实健康循环的节奏），所以触发时刻不会早于真实部署。
