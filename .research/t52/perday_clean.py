import json, re, collections
exec(open('/tmp/t52/audit.py').read().split('stats = collections')[0])
ISSUE={'switch','ttft_timeout','silence_timeout','stream_interrupt','stream_error','connect_error','upstream_error','all_failed'}
per=collections.defaultdict(lambda: collections.Counter())
for raw in open('call_log.jsonl','rb'):
    sp=spans(raw); d={k:(ks,vs,ve) for k,ks,ke,vs,ve in sp}
    day = raw[d['ts'][1]:d['ts'][2]][1:11].decode()
    status = raw[d['status'][1]:d['status'][2]]
    evb = (d['events'][2]-d['events'][0]) if 'events' in d else 0
    has=False
    if 'events' in d:
        evs=raw[d['events'][1]:d['events'][2]]
        for m3 in re.finditer(rb'"type":"([^"]*)"', evs):
            if m3.group(1).decode() in ISSUE: has=True; break
    p=per[day]; p['lines']+=1; p['bytes']+=len(raw); p['ev']+=evb
    if status!=b'"ok"' or has: p['issue_lines']+=1; p['issue_ev']+=evb
    else: p['clean_lines']+=1; p['clean_ev']+=evb
json.dump({k:dict(v) for k,v in sorted(per.items())}, open('perday_clean.json','w'), indent=1)
print(f"{'day':12s} {'lines':>7s} {'MB':>7s} {'cleanEv MB':>10s} {'issueEv MB':>10s} {'S2 MB':>7s} {'S3 MB':>7s}")
for day,p in sorted(per.items()):
    print(f"{day:12s} {p['lines']:7d} {p['bytes']/1e6:7.2f} {p['clean_ev']/1e6:10.2f} {p['issue_ev']/1e6:10.2f} {(p['bytes']-p['clean_ev'])/1e6:7.2f} {(p['bytes']-p['clean_ev']-p['issue_ev']*0.447)/1e6:7.2f}")
