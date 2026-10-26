import re, collections
lines=open('run.log','rb').readlines()
tot=sum(len(l) for l in lines)
print(f'行数={len(lines)} 字节={tot} 平均B/行={tot/len(lines):.1f}')
print('首行:',lines[0].decode('utf8','replace')[:130])
print('末行:',lines[-1].decode('utf8','replace')[:130])
ts=re.compile(rb'time=(\S+)'); lv=re.compile(rb'level=(\w*)')
msg=re.compile(rb'msg=("(?:[^"\\]|\\.)*"|[\w.]+)')
times=[ts.search(l).group(1).decode() for l in lines if ts.search(l)]
print('最早',times[0],'最新',times[-1],'带time行数',len(times))
bymsg=collections.defaultdict(lambda:[0,0]); bylv=collections.defaultdict(lambda:[0,0])
for l in lines:
    m=msg.search(l)
    k=(m.group(1).decode()[1:-1] if m.group(1).startswith(b'"') else m.group(1).decode()) if m else '(none)'
    bymsg[k][0]+=1; bymsg[k][1]+=len(l)
    g=lv.search(l); k2=g.group(1).decode() if g else '(none)'
    bylv[k2][0]+=1; bylv[k2][1]+=len(l)
print('\n--- level 分布 ---')
for k,(n,b) in sorted(bylv.items(), key=lambda kv:-kv[1][1]):
    print(f'{k:8s} n={n:7d} bytes={b:10d} 占比={100*b/tot:5.1f}% B/行={b/n:6.1f}')
print('\n--- msg 体积 Top20 ---')
for k,(n,b) in sorted(bymsg.items(), key=lambda kv:-kv[1][1])[:20]:
    print(f'{k:45s} n={n:7d} bytes={b:10d} 占比={100*b/tot:5.1f}% B/行={b/n:6.1f}')
print(f'\nmsg 种类数={len(bymsg)}')
