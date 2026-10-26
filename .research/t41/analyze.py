import json, collections, sys, os

REPO='/home/jingyx/project/opencode2api-test'
cat=json.load(open('/tmp/t41/catalogs.json'))
CATALOG=set(cat['zen'])|set(cat['go'])
DOCS14=set(cat['docs_free'])
DOCS13=set(json.load(open(REPO+'/free_models_cache.json')))
DEFAULT9={'big-pickle','space-bunny-free','mimo-v2.6-flash-free','mimo-v2.5-free','ling-3.0-flash-fin-free','nemotron-3-ultra-free','nemotron-3.5-lightning-free','muse-spark-1.3-contributor-free','jev-1.13-free'}
ALIAS={}          # 生产 config.json: model_alias=null
EDITED={}         # 生产 config: 无改名
HIDDEN=set()      # 生产 config: 无禁用

def resolve(m, docs):
    """逐字复刻 resolveModel（生产别名/改名/禁用均为空）：
       TrimSpace → alias → 改名反查 → 禁用 → 若不在免费表且 m+'-free' 在上游目录则映射。"""
    m=(m or '').strip()
    if m in ALIAS: return ALIAS[m]
    for orig,edited in EDITED.items():
        if edited==m: return m if orig in HIDDEN else orig
    if m in HIDDEN: return m
    if m!='' and m not in docs:
        free=m+'-free'
        if free in CATALOG: return free
    return m

def is_free_in(docs, mid):
    if not docs: return mid in DEFAULT9
    return mid in docs

def formulaB(model, docs):
    r=resolve(model, docs)
    return is_free_in(docs, r) or r.endswith('-free')

def load(path):
    rows=[]
    with open(path,encoding='utf-8',errors='replace') as f:
        for line in f:
            line=line.strip()
            if not line: continue
            try: rows.append(json.loads(line))
            except Exception: pass
    return rows

def analyze(path, docs, docs_name, label):
    rows=load(path)
    ok=[r for r in rows if r.get('status')=='ok']
    # ---- everServed 集合（两种定义都算，主口径用任务书字面：原始记录里的模型名）----
    E_raw={r.get('model') or '' for r in ok}
    E_res={resolve(r.get('model') or '', docs) for r in ok}
    per=collections.defaultdict(lambda:{'n':0,'ok':0})
    for r in rows:
        m=r.get('model') or ''
        per[m]['n']+=1
        if r.get('status')=='ok': per[m]['ok']+=1
    total=len(rows); total_ok=len(ok)
    out={'label':label,'path':path,'docs_name':docs_name,'total':total,'total_ok':total_ok,
         'everServed_raw':len(E_raw),'everServed_res':len(E_res),'per':{}, 'rows':rows,
         'E_raw':E_raw,'E_res':E_res}
    for m in per:
        out['per'][m]=per[m]
    def verdicts(m):
        r=resolve(m, docs)
        b=formulaB(m, docs)
        e_raw=r in E_raw
        e_res=r in E_res
        return {'resolved':r,'B':b,'E_raw':e_raw,'E_res':e_res,'union_raw':b or e_raw,'union_res':b or e_res,
                'in_docs':(r in docs) if docs else None,'in_catalog':r in CATALOG,'free_suffix':m.endswith('-free')}
    out['verdict']={m:verdicts(m) for m in per}
    return out

def summary(a, key):
    excl=[m for m in a['per'] if not a['verdict'][m][key]]
    n=sum(a['per'][m]['n'] for m in excl); okn=sum(a['per'][m]['ok'] for m in excl)
    kept=a['total']-n
    rate=(a['total_ok']/(kept)*100) if kept else 0
    base=a['total_ok']/a['total']*100
    models_kept=len([m for m in a['per'] if a['verdict'][m][key]])
    return {'excl_models':sorted(excl),'excl_n':n,'excl_share':n/a['total']*100,'excl_ok':okn,
            'total_kept':kept,'total_ok':a['total_ok'],'rate':rate,'base_rate':base,'delta':rate-base,
            'models_kept':models_kept,'zero_ok_models_kept':sorted([m for m in a['per'] if a['verdict'][m][key] and a['per'][m]['ok']==0])}

if __name__=='__main__':
    mode=sys.argv[1] if len(sys.argv)>1 else 'both'
    datasets=[('prod-now','/tmp/prod-now.jsonl'),('live','/tmp/t41/live.jsonl')]
    res={}
    for name,path in datasets:
        # 主口径：docs=今天文档页(14)；并做 docs13 / docs 缺失 big-pickle 的漂移敏感性
        for docs,dname in ((DOCS14,'docs14(今天文档页)'),(DOCS13,'docs13(仓库持久化)'),(DOCS14-{'big-pickle'},'docs14-去掉big-pickle')):
            a=analyze(path,docs,dname,name)
            summ={k:summary(a,k) for k in ('B','E_raw','union_raw','E_res','union_res')}
            res[f'{name}|{dname}']={'total':a['total'],'total_ok':a['total_ok'],
                'n_models':len(a['per']),'summary':summ,'per':{m:{**a['per'][m],**a['verdict'][m]} for m in a['per']}}
    json.dump(res,open('/tmp/t41/partA.json','w'),ensure_ascii=False,indent=1)
    # 打印主口径（docs14）
    for name,_ in datasets:
        key=f'{name}|docs14(今天文档页)'
        r=res[key]
        print(f"\n===== {name}（docs14 + 上游目录 {len(CATALOG)} 条）: total={r['total']} ok={r['total_ok']} models={r['n_models']} =====")
        for k in ('B','E_raw','union_raw','E_res','union_res'):
            s=r['summary'][k]
            print(f"  {k:10s} 排除 {s['excl_n']:6d} ({s['excl_share']:.4f}%) ok={s['excl_ok']:3d} | 概览 total={s['total_kept']:6d} rate={s['rate']:.6f}% (base {s['base_rate']:.6f}%, Δ{s['delta']:+.6f}pp) | 保留模型 {s['models_kept']} | 保留但0成功 {s['zero_ok_models_kept']}")
