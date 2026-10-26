import json, collections, gzip, io, os, re
# 复用 audit.spans
import importlib.util
spec = importlib.util.spec_from_file_location('audit', '/tmp/t52/audit.py')
# audit.py 顶层会跑全量，不能 import；这里复制 spans 函数
exec(open('/tmp/t52/audit.py').read().split('stats = collections')[0])

PATH='call_log.jsonl'
per_day=collections.defaultdict(lambda: collections.Counter())
agg=collections.Counter()
event_clean_bytes=0; event_fail_bytes=0
node_names_dup=0; node_names_total=0
ev_node_dup=0; ev_node_total=0; ev_nodename_bytes=0
detail_bytes_by_type=collections.Counter()
evtype=collections.Counter()
with open(PATH,'rb') as f:
    for raw in f:
        sp=spans(raw)
        d={}
        for key,ks,ke,vs,ve in sp:
            d[key]=(ks,vs,ve)
            if key=='events':
                pass
        status = raw[d['status'][1]:d['status'][2]] if 'status' in d else b''
        day = raw[d['ts'][1]:d['ts'][2]][1:11].decode() if 'ts' in d else '?'
        isok = status == b'"ok"'
        evb = (d['events'][2]-d['events'][0]) if 'events' in d else 0
        if isok: event_clean_bytes += evb
        else: event_fail_bytes += evb
        per_day[day]['lines']+=1; per_day[day]['bytes']+=len(raw)
        per_day[day]['events_bytes']+=evb
        if isok: per_day[day]['ok']+=1
        else: per_day[day]['fail']+=1
        # 顶层 nodes / node_names
        nodes=[]; names=[]
        if 'nodes' in d:
            s=raw[d['nodes'][1]:d['nodes'][2]]
            nodes=[x.decode() for x in re.findall(rb'"([^"]*)"', s)]
        if 'node_names' in d:
            s=raw[d['node_names'][1]:d['node_names'][2]]
            names=[x.decode() for x in re.findall(rb'"([^"]*)"', s)]
        if names:
            node_names_total+=len(names)
        # events 内部：node_name 是否已存在于顶层 names
        if 'events' in d:
            evs=raw[d['events'][1]:d['events'][2]]
            for obj in re.finditer(rb'\{[^{}]*\}', evs):
                o=obj.group(0)
                m=re.search(rb'"node_name":"((?:[^"\\]|\\.)*)"', o)
                if m:
                    ev_node_total+=1; ev_nodename_bytes+=len(m.group(0))+1
                    if m.group(1).decode() in names: ev_node_dup+=1
                m2=re.search(rb'"node":"([^"]*)"', o)
                if m2:
                    if m2.group(1).decode() in nodes: ev_node_dup+=1
                m3=re.search(rb'"type":"([^"]*)"', o)
                if m3: evtype[m3.group(1).decode()]+=1
                m4=re.search(rb'"detail":"((?:[^"\\]|\\.)*)"', o)
                if m4: detail_bytes_by_type[m3.group(1).decode() if m3 else '?']+=len(m4.group(0))

json.dump({'per_day':{k:dict(v) for k,v in sorted(per_day.items())},
           'event_clean_bytes':event_clean_bytes,'event_fail_bytes':event_fail_bytes,
           'node_names_redundant':{'total_refs':node_names_total,'event_nodename_bytes':ev_nodename_bytes,
                                   'event_nodename_refs':ev_node_total,'event_nodename_in_top_names':ev_node_dup,
                                   'event_node_in_top_nodes':ev_node_dup},
           'evtype':dict(evtype),'detail_bytes_by_type':dict(detail_bytes_by_type)},
          open('/tmp/t52/deep.json','w'), ensure_ascii=False, indent=1)
print('ok 行 events 字节:',event_clean_bytes,' fail 行 events 字节:',event_fail_bytes)
print('events 内 node_name 引用数:',ev_node_total,'其中已在顶层 node_names 出现:',ev_node_dup,'字节:',ev_nodename_bytes)
print('事件类型:',dict(evtype.most_common(12)))
print('detail 字节 by type:',dict(detail_bytes_by_type.most_common(8)))
print('\n按天:')
for day,v in sorted(per_day.items()):
    print(f"{day} lines={v['lines']:6d} ok={v['ok']:6d} fail={v['fail']:5d} bytes={v['bytes']:9d} ev_bytes={v['events_bytes']:9d}")
