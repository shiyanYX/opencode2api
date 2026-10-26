import json, collections, re
exec(open('/tmp/t52/audit.py').read().split('stats = collections')[0])
ISSUE={'switch','ttft_timeout','silence_timeout','stream_interrupt','stream_error','connect_error','upstream_error','all_failed'}
PATH='call_log.jsonl'
clean_events=0; issue_events=0; n_clean_ev_recs=0; n_issue_recs=0
ev_nodename_bytes=0; ev_nodename_in_top=0; ev_nodename_total=0
ev_node_bytes=0; ev_node_in_top=0; ev_node_total=0
top_names_bytes=0; top_names_total_bytes=0
ev_at_bytes=0; ev_type_bytes=0; ev_detail_bytes=0
for raw in open(PATH,'rb'):
    sp=spans(raw); d={}
    for key,ks,ke,vs,ve in sp: d[key]=(ks,vs,ve)
    status = raw[d['status'][1]:d['status'][2]]
    evb = (d['events'][2]-d['events'][0]) if 'events' in d else 0
    has_issue = status!=b'"ok"'
    if 'events' in d:
        evs=raw[d['events'][1]:d['events'][2]]
        for obj in re.finditer(rb'\{[^{}]*\}', evs):
            o=obj.group(0)
            m3=re.search(rb'"type":"([^"]*)"', o)
            t=m3.group(1).decode() if m3 else ''
            if t in ISSUE: has_issue=True
            m4=re.search(rb'"detail":', o)
            if m4:
                mm=re.search(rb'"detail":"((?:[^"\\]|\\.)*)"', o)
                ev_detail_bytes += len(mm.group(1)) if mm else 0
            mm=re.search(rb'"at":"([^"]*)"', o)
            if mm: ev_at_bytes += len(mm.group(1))
            mm=re.search(rb'"node":"((?:[^"\\]|\\.)*)"', o)
            if mm:
                ev_node_bytes += len(mm.group(1)); ev_node_total+=1
    if has_issue:
        n_issue_recs+=1; issue_events+=evb
    else:
        n_clean_ev_recs+=1; clean_events+=evb
    # 顶层 names 冗余检查
    names=[]; nodes=[]
    if 'node_names' in d:
        s=raw[d['node_names'][1]:d['node_names'][2]]; names=[x.decode() for x in re.findall(rb'"([^"]*)"', s)]
    if 'nodes' in d:
        s=raw[d['nodes'][1]:d['nodes'][2]]; nodes=[x.decode() for x in re.findall(rb'"([^"]*)"', s)]
    if names: top_names_total_bytes+=d['node_names'][2]-d['node_names'][0]
    if 'events' in d:
        evs=raw[d['events'][1]:d['events'][2]]
        for obj in re.finditer(rb'\{[^{}]*\}', evs):
            o=obj.group(0)
            mm=re.search(rb'"node_name":"((?:[^"\\]|\\.)*)"', o)
            if mm:
                ev_nodename_total+=1; ev_nodename_bytes+=len(mm.group(0))
                if mm.group(1).decode() in names: ev_nodename_in_top+=1
            mm=re.search(rb'"node":"((?:[^"\\]|\\.)*)"', o)
            if mm:
                if mm.group(1).decode() in nodes: ev_node_in_top+=1
print(json.dumps({'clean_records':n_clean_ev_recs,'issue_records':n_issue_recs,
 'clean_events_bytes':clean_events,'issue_events_bytes':issue_events,
 'ev_nodename_bytes':ev_nodename_bytes,'ev_nodename_total':ev_nodename_total,'ev_nodename_in_top':ev_nodename_in_top,
 'ev_node_bytes':ev_node_bytes,'ev_node_total':ev_node_total,'ev_node_in_top':ev_node_in_top,
 'top_node_names_bytes':top_names_total_bytes,
 'ev_at_bytes':ev_at_bytes,'ev_detail_bytes':ev_detail_bytes},ensure_ascii=False,indent=1))
