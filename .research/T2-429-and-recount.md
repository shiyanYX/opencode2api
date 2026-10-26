# T2 · 上游 429 取证 + 「直连（零节点）三分类」独立复算

- 执行者：teammate `capture-429`（task-2）
- 工作区：`/home/jingyx/project/opencode2api-test`（test 分支，HEAD `a19d3a4`）
- 生产容器：`opencode2api`（`ghcr.io/shiyanyx/opencode2api:latest`，digest `sha256:112ee5659c2a…`，StartedAt `2026-10-14T17:12:23Z`）
- 数据：`/tmp/prod-now.jsonl`（204,237 行）、`/tmp/ctest/call_log.jsonl`（219,228 行）、`/tmp/ctest/cl.json`（500 条 API 尾巴）、`docker logs opencode2api`
- 只读承诺：未改任何仓库文件、未 git add/commit、未重启或改动生产容器、未故意打满配额。本文件是本次唯一写入。
- 口径（全篇）：**排除每份日志第一行**；`nodes` 缺失或空 = 零节点（= 面板说的「直连」）；时间戳按 **UTC 自然日**；成功率用 **请求级**；「最终 429」= `err_msg == "upstream http 429"`（即对外最终状态，理由见 §2.2）。

---

# 第一部分：抓真实上游 429 的完整响应头 + body

## 1.1 四条零风险来源逐个试过

| # | 来源 | 结果 |
|---|---|---|
| 1 | `docker logs --since 48h opencode2api` | ✅ **拿到 429 原始 body**（38 次主形态 + 3 次次形态），**没有任何头** |
| 2 | 代码 `logUpstreamError` / `safeResponseHeaders` | 只落 `model/status/body`（body 截断 512B）；**全代码库没有一处读上游响应头做配额判定** |
| 3 | `/tmp/ctest/call_log.jsonl`、`/tmp/ctest/cl.json` | ❌ 不存错误 body/headers（字段清单见 §1.4） |
| 4 | `/tmp/ctest/opencode2api.log` | ❌ 429/retry/ratelimit/reset 全 0 命中（旧尾巴副本） |

命令与输出：

```bash
export HTTP_PROXY=http://192.168.1.3:7890 HTTPS_PROXY=http://192.168.1.3:7890
docker logs --since 48h opencode2api > /tmp/capture429_dockerlogs.txt 2>&1   # 68,797 行
for p in retry-after x-ratelimit ratelimit rate-limit retry_after "Retry-After" remaining quota_remaining; do
  printf "%-16s %s\n" "$p" "$(grep -ic -- "$p" /tmp/capture429_dockerlogs.txt)"; done
# 输出：retry-after 0 / x-ratelimit 0 / ratelimit 0 / rate-limit 0 / retry_after 0 / Retry-After 0 / remaining 0 / quota_remaining 0
grep -c 'status=429' /tmp/capture429_dockerlogs.txt        # 82
grep -ci "header" /tmp/capture429_dockerlogs.txt           # 65 —— 全部是 Go 错误串 "timeout awaiting response headers"，不是响应头
grep -c "level=DEBUG" /tmp/capture429_dockerlogs.txt       # 0（debug 体日志开关未开）
grep -nEi "429|retry|ratelimit|reset|quota|FreeUsage" /tmp/ctest/opencode2api.log   # 0 命中
```

429 body 抽取（logfmt 里 body 是 Go 转义串，用 python 解引用）：

```bash
python3 - <<'PY'
import re,json,collections
raw=open('/tmp/capture429_dockerlogs.txt',encoding='utf-8',errors='replace').read().splitlines()
pat=re.compile(r'\bstatus=(\d+) body=("(?:\\.|[^"\\])*")')
c=collections.Counter(); ex={}
for ln in raw:
    for m in pat.finditer(ln):
        k=(int(m.group(1)),json.loads(m.group(2))); c[k]+=1; ex.setdefault(k,ln)
for (st,b),n in c.most_common(): print(f"n={n:5d} status={st} body={b!r}")
print("样例:",ex[(429,next(b for s,b in ex if s==429))][:400])
PY
```

输出（完整）：

```
n= 1023 status=403 body='{"type":"error","error":{"type":"FreeTierError","message":"OpenCode\'s free tier can only be used from within OpenCode. Download it at https://opencode.ai and keep building."}}'
n=   47 status=500 body='{"error":{"type":"server_error","message":"Upstream request failed: Endpoint is unavailable."}}'
n=   38 status=429 body='{"type":"error","error":{"type":"FreeUsageLimitError","message":"Rate limit exceeded. Please try again later."}}'
n=    3 status=429 body='{"error":{"type":"server_error","message":"Upstream request failed: Endpoint is unavailable."}}'
（另有 400/520/503 若干，见 /tmp/capture429_dockerlogs.txt）

样例原始行：
time=2026-10-16T00:09:19.891+08:00 level=ERROR msg="upstream error" request_id=9xtjs479snm9 model=space-bunny-free status=429 body="{\"type\":\"error\",\"error\":{\"type\":\"FreeUsageLimitError\",\"message\":\"Rate limit exceeded. Please try again later.\"}}"
```

## 1.2 (i) 429 是否带 Retry-After / x-ratelimit-* / reset / quota 头？

**结论：未获取（实测拿不到头）；「没有」只能算推断，不能标 ✅。**

- 【实测】48h 内 **0 处**日志出现 `retry-after` / `x-ratelimit` / `ratelimit` / `rate-limit` / `remaining` / `quota_remaining` 字样。日志只出现 "timeout awaiting response headers"（Go 错误串）。
- 【实测】`logUpstreamError`（**logging.go:574-599**）只写 3 个字段：`model`、`status`、`body`（`truncateForLog(string(body), 512)`），**从不写任何响应头**；两个调用点 `main.go:2714`、`main.go:3016` 传的是 `errBody`，头根本没进函数签名。
- 【实测】`safeResponseHeaders` 在 **main.go:3192-3197**（台账 §六 写的 `main.go:3174-3179` 是旧行号，本分支已位移），白名单 4 项：`Content-Type`、`X-RateLimit-Limit`、`X-RateLimit-Remaining`、`X-RateLimit-Reset`。它只用于 `filterResponseHeaders()`（main.go:3199-3207）**给下游透传**，与「记不记日志」无关；**白名单里没有 `Retry-After`**。
- 【实测】**全代码库没有一处读取上游响应头做配额判定**：`grep -rn "resp\.Header\|\.Header\.Get"` 的命中只有两类——上游头整体透传（main.go:2614/2710/2844/2868/3145/3168/3185，最后交给 `filterResponseHeaders`）与 **入站客户端** 头（`Authorization`、`x-api-key`、`X-Request-Id`、`anthropic-beta`）。没有任何 `Retry-After` / `X-RateLimit-*` 的解析代码。
- 【实测·旁证】配额冷却完全是本地常量，不来自任何头：`proxy_node.go:462-466` `CooldownUntil = now.Add(p.effectiveExhaustedCooldown())`；48h 内 **38 次** `state=exhausted reason=quota:type:FreeUsageLimitError` 的 `until - time` 全部是 **18,000 秒（恰好 5h，19 次 17999s 是亚秒取整）**。若冷却来自 Retry-After/reset 头，不会恰好整齐到 5×3600s。
- 【实测·易混淆项】**网关自己的** 429（旧版闸门）确实 `header == nil`：`quota_halt.go:22-25` 代码注释原文「旧版返回 429 且 header 为 nil」。这是网关→下游的头，不是上游→网关的头，**不能用来回答本问**。

⇒ 台账 §六 第 1 行「上游 429 **不给** Retry-After / reset 头 ✅」的证据是「抓 200 与 403 的完整头集」，**对 429 不成立**；§五 把它划掉标 ✅ 属于**越权外推**，应降级为 ❓/推断。台账 §六 第 2 行「代码从没解析过它 ✅」**成立**（但顺带的「⇒ 即便有也会被剥掉」不成立：透传白名单不等于日志白名单，剥掉只影响下游）。

## 1.3 (ii) 429 的 body 长什么样？

**结论：实测拿到，两种形态。**（且台账 §五「上游 429 原始 body」应可从 ❓ 改为 ✅——它就在 `docker logs` 里，不需要改代码。）

- 主形态（配额型，48h 内 38 次）：
  ```json
  {"type":"error","error":{"type":"FreeUsageLimitError","message":"Rate limit exceeded. Please try again later."}}
  ```
  结构：顶层 `type:"error"`；`error.type = "FreeUsageLimitError"`；`error.message` 固定为 `"Rate limit exceeded. Please try again later."`。**没有 quota 余量、reset 时刻、限额数值等任何附加字段。**
- 次形态（429 但无配额签名，48h 内 3 次）：
  ```json
  {"error":{"type":"server_error","message":"Upstream request failed: Endpoint is unavailable."}}
  ```
  这正好落在 `quota.go:35-37` 的分支上：「429 无签名 → 不耗尽（普通限流，同节点重试）」——即 **429 不等于配额耗尽**，网关靠 body 签名区分。
- 同端点其它签名（对照，便于识别）：403 `FreeTierError`「OpenCode's free tier can only be used from within OpenCode…」48h 内 **1,023 次**。
- 抓取完整性：日志截断阈值 512 字符（logging.go:587/597），观测到的 body 为 89/114 字符 → **完整未截断**。
- 连带事实（实测）：闸门会把上游错误体原样回放给下游（`quota_halt.go:52-53` `quotaHaltFallbackBody`、`haltState.body` / `bodyCopy()`），所以下游在熔断期看到的可能是 503 + 上游 `FreeUsageLimitError` 体。

## 1.4 (iii) 配额按【请求数】还是按 token？

**结论：无法判定（本轮无直接证据）；现有数据只能给出两条负证据 + 一条弱旁证。**

首先把「能不能从数据里看出来」的边界说清：

- 【实测·重要口径纠正】`err_msg`/事件里根本没有配额字段，`logUpstreamError` 也没有头；因此**本问题在现有日志上不可能被直接证实**。
- 【实测·负证据 1】任何「全局请求数配额」或「全局 token 配额」模型都与日聚合矛盾：

  | UTC 日 | 请求数 | token 总量 | 最终 429 |
  |---|---|---|---|
  | 2026-09-30 | 17,232 | **803,740,092** | **0** |
  | 2026-09-19 | 15,188 | 176,743,430 | **3,103** |

  09-30 的 token 是 09-19 的 **4.5 倍**、请求数还多 13%，却 **一次 429 都没有**。而且全期 6,522 条最终 429 **100% 集中在 09-19/20/21 三天**（3,103+1,097+2,322）。⇒ 429 不是由网关侧的总量维度触发的，更像 **按出口 IP 的桶**（与台账 §二「429 按出口 IP 分桶」一致），所以「请求数 vs token」在日志里被出口粒度掩盖了。
- 【实测·负证据 2（同时纠正台账 §二 的一处引用风险）】台账 §二「被 429 拒绝的请求消耗 0 token ✅」数值为真但**不具判别力**：`/tmp/prod-now.jsonl` 里 **17,052 条失败记录中只有 1 条** token 字段 >0（成功记录 186,400/187,184 才有 token）。token 字段只在成功时写入，所以「429 消耗 0 token」是**失败记录口径的产物**，不能用来推断上游如何计费。
- 【实测·弱旁证（偏向请求数）】风暴三天内，零节点路径的 token 吞吐与 429 数**反向**：09-19 / 09-20 / 09-21 的零节点 token 吞吐为 3.26M / **19.66M** / 14.91M，而同口径 429 为 2,897 / **1,044** / 2,251——放行 token 最多的那天 429 最少；429 数的排序反而跟零节点请求数（3,149 / 2,505 / 3,412）同向。⇒ **弱偏向「按请求数/频次」**，但与时间线（熔断闩、节点可用性）混淆，**不足以定论**。
- 【推断】body 里 `FreeUsageLimitError` + 「Rate limit exceeded」不含 token 计量语义；`reason=quota:type:FreeUsageLimitError` 也只记 error.type。这只能说明「上游没告诉我们」，不能说明计量维度。

⇒ 台账 §六 未决项「上游是否按请求数计费 ❓」**应保持 ❓**，我未能推进到可判定的程度。

## 1.5 最省成本、零风险的合规抓取方案（本轮「未获取」的补救）

**改一处、纯日志、无行为变更**（我没有实施，需 owner 批准后由部署方执行）：

1. 在 `logging.go:574` 的 `logUpstreamError` 里，把 `http.Header` 作为可选入参接进来（或在 `main.go:2714` / `main.go:3016` 两个调用点就地加一行），**仅在 `status == 429`（可扩到 403）时**，用 ERROR 级别打一行 `upstream_headers`，键经白名单过滤：`(?i)retry[-_]?after|ratelimit|rate[-_]limit|reset|quota|x-request-id|content-type`。
2. 影响面：约 10 行、零依赖、零行为变更、不动下游透传（`safeResponseHeaders` 保持原样）、不落库、不改 JSONL schema（`CallRecord` 不动）。
3. 成本：429 在 48h 内只有 41 次上游错误体（≈<1 次/小时）；`logUpstreamError` 已有 `(model,status)` 10 秒去重抑制，日志量可忽略。
4. 触发：**不需要造 429**。真实 429 必然复发（48h 内 38 次 FreeUsageLimitError），部署后数小时内即可自动捕获，成本近零。
5. 回滚：单 commit revert，或加一个 config 开关（默认关）。
6. 备选（零改动）：把 `Retry-After` 加进 `safeResponseHeaders`（台账 §六「后路」）——但这只改善下游透传，**仍然不会记录上游头**，无法回答本问，不推荐作为取证手段。

---

# 第二部分：「直连（零节点）三分类」独立复算

## 2.1 口径来源与可复现命令

面板代码 **static/admin.html:2074-2079**（已逐字核对）：

```js
let node;
if(last<0){                                   // last = nodes 为空 ? -1 : nodes.length-1
 const evs=r.events||[];
 const has=k=>evs.some(e=>e&&e.type===k);
 const blocked=evs.some(e=>e&&e.type==='quota_halt'&&(e.detail||'').includes('blocked'));
 node=blocked?'直连·闸门拒绝':has('connect_error')?'直连·连不上':'直连·其他';
}
```

我的复算脚本（**本报告所有第二部分数字都由它一次跑出**，原样可跑）：

```bash
python3 - <<'PY'
import json, collections
F='/tmp/prod-now.jsonl'
lines=open(F).read().splitlines()
recs=[json.loads(l) for i,l in enumerate(lines) if l.strip() and i>0]   # 排除第一行
def evs(r): return [e for e in (r.get('events') or []) if isinstance(e,dict)]
def zero(r): return not (r.get('nodes') or [])                          # nodes 缺失或空
def ok(r): return r.get('status')=='ok'
def has(r,t): return any(e.get('type')==t for e in evs(r))
def blocked(r): return any(e.get('type')=='quota_halt' and 'blocked' in (e.get('detail') or '') for e in evs(r))
def ev429(r): return any(e.get('type')=='upstream_error' and '429' in (e.get('detail') or '') for e in evs(r))
def f429(r): return r.get('err_msg')=='upstream http 429'
Z=[r for r in recs if zero(r)]; Zf=[r for r in Z if not ok(r)]; Zo=[r for r in Z if ok(r)]
print("行数",len(lines),"记录",len(recs))
print("零节点",len(Z),"成功",len(Zo),"失败",len(Zf))
cl=lambda r:'直连·闸门拒绝' if blocked(r) else ('直连·连不上' if has(r,'connect_error') else '直连·其他')
print("面板三分类(全零节点)",dict(collections.Counter(cl(r) for r in Z)))
print("面板三分类(零节点失败)",dict(collections.Counter(cl(r) for r in Zf)))
print("面板三分类(零节点成功)",dict(collections.Counter(cl(r) for r in Zo)))
print("含quota_halt: 全量",sum(1 for r in recs if has(r,'quota_halt')),"零节点",sum(1 for r in Z if has(r,'quota_halt')))
print("含blocked: 全量",sum(1 for r in recs if blocked(r)),"零节点",sum(1 for r in Z if blocked(r)))
print("quota_halt detail:",dict(collections.Counter(e.get('detail') for r in recs for e in evs(r) if e.get('type')=='quota_halt')))
print("零节点失败: 429事件",sum(1 for r in Zf if ev429(r)),"最终429",sum(1 for r in Zf if f429(r)))
print("闸门且带429事件",sum(1 for r in Zf if blocked(r) and ev429(r)))
print("最终429集合 == 闸门∪429事件集合:",{r['req_id'] for r in Zf if f429(r)}=={r['req_id'] for r in Zf if blocked(r) or ev429(r)})
print("零节点失败且无任何事件",sum(1 for r in Zf if not evs(r)))
print("connect_error: 面板桶",sum(1 for r in Z if has(r,'connect_error')),"失败",sum(1 for r in Zf if has(r,'connect_error')),"成功",sum(1 for r in Zo if has(r,'connect_error')))
print("全量最终429",sum(1 for r in recs if f429(r)))
PY
```

## 2.2 复算结果（实测输出）

```
行数 204237  记录 204236                 # 排除第一行后 204,236 条
零节点 10183  成功 3614  失败 6569
面板三分类(全零节点 10183) {'直连·其他': 6540, '直连·连不上': 359, '直连·闸门拒绝': 3284}  合计 10183 ✅覆盖全部
面板三分类(零节点失败 6569) {'直连·其他': 2936, '直连·连不上': 349, '直连·闸门拒绝': 3284}
面板三分类(零节点成功 3614) {'直连·其他': 3604, '直连·连不上': 10}
含quota_halt: 全量 3320  零节点 3289
含blocked:    全量 3284  零节点 3284
quota_halt detail: {'enter: 直连出口 429，停止重试等待节点恢复': 36,
                    'blocked: 无可用节点且直连出口 429，等待节点池恢复': 3284}
零节点失败: 429事件 2908   最终429 6192
闸门且带429事件 0
最终429集合 == 闸门∪429事件集合: True
零节点失败且无任何事件 0
connect_error: 面板桶 359   失败 349   成功 10
全量最终429 6522   （按 UTC 日：09-19 3103 / 09-20 1097 / 09-21 2322）
```

口径注释：
- **总记录数 204,236**（排除第一行）。台账 §一 的分母 204,237 含第一行；成功率两种口径都是 91.65%（187,184/204,236）。失败分母相应为 17,052（台账写 17,053）——差异 1 条，不影响任何结论。
- **「最终 429」= `err_msg == "upstream http 429"`**。`/tmp/prod-now.jsonl` 里 **没有** `final_status` 字段（全量键集：`req_id/ts/path/model/stream/route_mode/nodes/events/status/duration_ms/err_msg/prompt_tokens/completion_tokens/cache_read_tokens/node_names/ttft_ms/output_speed`；`final_status`/`dial_ms` 只在更新的 `/tmp/ctest/call_log.jsonl` 里出现 4 次）→ 只能借 `err_msg`。校验：全量 `err_msg` 含「429」的记录 **6,522 条，且 100% 都是同一取值 `upstream http 429`**，与台账 §二「429 配额 6,522 条」完全吻合。
- **「429 尝试事件」= 事件 `upstream_error` 且 `detail` 含 `429`**，取值只有两种：`http 429 quota_signal`（10,097 次）与 `http 429`（9,093 次）。

## 2.3 三类是否互斥且覆盖全部零节点记录？

- **互斥**：面板逻辑对 `last<0` 的记录只赋一个字符串（if / else 链），天然互斥。
- **覆盖**：3,284 + 359 + 6,540 = **10,183 = 全部零节点记录**，`覆盖=True`，**没有第四类残留**。
- ★ 但必须提醒口径陷阱：面板这三个桶的分母是 **全部零节点记录（含 3,614 条成功）**，所以三个桶**不是失败分类**——「直连·其他」6,540 里有 **3,604 条是成功**，「直连·连不上」359 里有 **10 条是成功**。把它读成「失败构成」会错。
- 另一个隐藏项：有 **31 条 `nodes` 非空**的记录也带 `quota_halt` 事件（36 条 `enter:` 事件 − 5 条零节点的）——按面板逻辑它们走 `else` 分支被算进**节点名桶**，不进「直连·闸门拒绝」。这是面板口径的既定行为，不是 bug，但统计「闸门影响面」时不能只看面板那个桶。

## 2.4 与 Leader 参考值的差异（如实列出，未抹平）

| 项 | Leader 参考 | 我的复算 | 差异说明 |
|---|---|---|---|
| 零节点失败 | 6,569 | **6,569** | 一致 |
| 零节点成功 | 3,614 | **3,614** | 一致 |
| 含 quota_halt 事件 | 3,289 | **3,289** | 一致。⚠️ 该值 = **零节点**子集；**全量**是 **3,320**（多 31 条带节点的 `enter:` 事件）。引用时必须写明「零节点口径」 |
| 零节点失败中含 429 | 6,192 | **6,192** | 数字一致，但**定义必须补**：6,192 是 `err_msg=="upstream http 429"`（对外最终状态）。若按「含 429 上游尝试事件」只有 **2,908**。两者差 **3,284**，正是闸门直接返回的 429（§2.5） |
| 零节点失败且无任何事件 | 0 | **0** | 一致 |
| connect_error 桶 359 vs 失败 349 的 10 条 | 待解释 | 见 §2.6 | ✅ 已定位 |

## 2.5 「quota_halt 且 detail 含 blocked」与「有任何 quota_halt 事件」的计数差

| 定义 | 全量 | 零节点 |
|---|---|---|
| 有任何 `quota_halt` 事件 | 3,320 | **3,289** |
| `quota_halt` 且 `detail` 含 `'blocked'` | 3,284 | **3,284** |
| **差** | **36** | **5** |

差异来源：`quota_halt` 事件有两种 detail——`blocked: 无可用节点且直连出口 429，等待节点池恢复`（3,284 次）与 `enter: 直连出口 429，停止重试等待节点恢复`（36 次）。面板要求 `detail 含 'blocked'`，所以：

- 零节点口径：3,289 − 3,284 = **5** 条只有 `enter:`、没有 `blocked` 的零节点记录 → 它们被面板判成「直连·其他/连不上」，**不进「直连·闸门拒绝」**。⇒ **面板「直连·闸门拒绝」应为 3,284，而不是 3,289**。
- 全量口径：差 36（36 条 `enter:` 事件中 31 条在 `nodes` 非空记录里）。

**「被闸门挡的记录里有多少同时带 429 的 upstream_error detail」= 0**（3,284 条闸门记录，带 429 尝试事件的 **0** 条）。
这正是 W6 的理由，而且现在有精确量化：闸门在**任何上游尝试之前**就返回，所以那 3,284 条记录的「429」完全是**网关自己合成的 429**（且按 `quota_halt.go:22-25` 旧版 `header == nil`），与真实上游 429（2,908 条）**零交集**。更关键的是：**6,192 = 3,284（闸门合成 429，无上游尝试）+ 2,908（真实上游 429 尝试失败）**，两个集合互斥且并集恰好是「零节点失败 + 最终 429」全体（脚本 `集合相等: True` 已验证）。⇒ W6 把闸门响应从 429 改成 503 + `Retry-After: 30`（`quota_halt.go:250-261`），修的是 **3,284 条被误标成「429 配额失败」的记录**。

## 2.6 connect_error 桶 359 比「零节点失败含 connect_error」349 多的那 10 条

**答案：那 10 条是「零节点且最终成功」但过程中出现过 `connect_error` 事件的记录。**

- 面板桶（分母 = **全部**零节点记录，含成功）`has('connect_error')` = **359**
- 其中零节点**失败** = **349**
- 其中零节点**成功** = **10** ← 就是这 10 条
- 事件级 vs 请求级的经典错位：节点连不上 → 事件记 `connect_error` → 之后走直连成功 → 请求级 `status='ok'`。面板按事件细分桶、却把成功率分母设成含成功的全部直连记录，所以两类数字天然差 10。脚本第 [13] 行可复现。

## 2.7 ★ 台账 §一「nodes 为空（被闸门挡）6,569 条，其中 6,192 是 429」措辞判定

**判定：数字对，措辞错（括号里的定义是错的）。**

1. **6,569 不是「被闸门挡」**，而是 **「零节点（直连路径）的全部失败」**。其中真正被闸门挡下的只有 **3,284 条（49.99%）**；另外 3,285 条里 **2,908 条**是真实上游 429 尝试后失败，**377 条**是 502/500/403/426 等（`err_msg` 分布：429 6,192 / 502 328 / 500 20 / 403 18 / 426 11）。
2. **6,192 的「429」需要标明是最终状态口径**，且它 = 3,284 条闸门合成 429 + 2,908 条真实上游 429；按「有 429 上游事件」只有 2,908。
3. 台账 §一 表格是「失败构成（分母 17,053）」，6,569 放在失败分母下没问题；问题只在「（被闸门挡）」这五个字把「零节点失败」偷换成「闸门拒绝」。

**建议措辞（只给建议，未改台账文件）：**

> | **nodes 为空（直连路径）失败** | **38.5%**（6,569 条；占失败 6,569/17,052 = 38.52%）**其中被闸门直接挡下 3,284 条**，另有 **2,908** 条是真实上游 429 尝试失败、377 条是 502/500/403/426 |
>
> 附注：① 6,569 是**零节点失败**，不等于被闸门挡（闸门只占 3,284 / 49.99%）；② 若按「对外最终状态 429」计为 **6,192** 条 = 3,284（闸门合成 429，无任何上游尝试）+ 2,908（真实上游 429）；③ 零节点记录共 **10,183** 条（失败 6,569 + 成功 3,614），被闸门挡的 3,284 条全部是失败。

---

# 不确定项

1. **429 响应头：未获取。**「429 不带 Retry-After / x-ratelimit-* / reset」是**推断**（依据：网关从不记录也不解析上游头 + 冷却为本地常量 5h），**不是实测**。台账 §五/§六 的 ✅ 应降级为 ❓。同时 §五「上游 429 原始 body」应升为 ✅（body 已在 `docker logs` 里）。
2. **上游按请求数还是 token 计：未获取，无法判定。** 两条负证据（全局量与 429 无关）+ 一条弱旁证（风暴内 token↑而 429↓）；出口 IP 粒度把维度掩盖了。台账 §六 该项应保持 ❓。
3. **部署版本与 HEAD `a19d3a4` 不完全一致（未获取确切版本）。** 日志字段集与 `logging.go:574-599` 吻合，但两处不符：① 数据里的 blocked detail 是「等待**节点池**恢复」，HEAD `quota_halt.go:253` 是「等待**出口**恢复」；② 实测配额冷却恰好 **18,000s（5h）**，而 HEAD 默认是 1h（`proxy_node.go:263`、`main.go:6580-6583`）。⇒ 引用行号时需注意分支差异（例如 `safeResponseHeaders` 台账写 3174-3179，HEAD 是 **3192-3197**）。
4. **5h 冷却参数的来源未查明**：`docker cp` 出来的生产 `config.json`（`/tmp/ctest/config.json`）**没有** `node_cooldown_exhausted_hours` 键（可能是运行期经面板 patch 后未落盘，或落在别处）。
5. **`docker logs` 只有约 48h**（容器 StartedAt 2026-10-14T17:12Z）。body 形态证据来自该窗口；**09-19/20/21 那三天的 6,522 次 429 日志已随重启丢失**，无法核对风暴期的 body/头是否与现在一致（错误类型签名相同，但同一性未经实测）。
6. **`/tmp/ctest/call_log.jsonl` 219,228 行 ≠ `prod-now.jsonl` 204,237 行**：两份快照时点不同，我未做对齐；第二部分全部结论只用 `/tmp/prod-now.jsonl`。
7. **`cl.json`（500 条 API 尾巴）根本不含 `events`**（键：`req_id/ts/path/model/stream/route_mode/nodes/node_names/status/prompt_tokens/completion_tokens/cache_read_tokens/duration_ms/ttft_ms/output_speed`），不能用于三分类；`call_log.jsonl` 的 `CallEvent` 只有 `type/node/node_name/detail/at`，`detail` 对 429 仅记为 `http 429 quota_signal` / `http 429`，**不存 body、不存 headers**。`CallRecord`（calllog.go:33-80）同样无 body/headers 字段——**唯一不可逆的 JSONL 变更是追加了 `dial_ms`/`final_status`，与错误体无关**。
8. **`err_msg` 是否严格等于对外最终状态**：我用它近似（它在 6,522 条上 100% 自洽、且与 `/tmp/ctest/call_log.jsonl` 里 `final_status` 的语义注释一致），但 `prod-now.jsonl` 无 `final_status` 字段可交叉验证，仅有 4 条新格式记录含该字段。
9. 未做（按硬约束）：未故意制造 429、未重启/改容器、未改配置、未改任何仓库文件、未 git 操作。
