import json,sys,datetime
def ts(s): return datetime.datetime.fromisoformat(s.replace('Z','+00:00')[:26]+'+00:00') if '.' in s else datetime.datetime.fromisoformat(s.replace('Z','+00:00'))
recs=collections=None
start={};spans=[];t0=None
recs=set()
for l in open(sys.argv[1]):
    e=json.loads(l); r=e['record']; recs.add(r)
    t=e.get('time')
    if not t: continue
    import re
    t=re.sub(r'(\.\d{6})\d*','\\1',t).replace('Z','+00:00'); t=datetime.datetime.fromisoformat(t)
    if t0 is None: t0=t
    k=(e.get('identity') or {}).get('key')
    if not k: continue
    if r=='task:start': start[k]=t
    elif r in('task:end','task:result','task:finish') and k in start:
        spans.append(((start.pop(k)-t0).total_seconds(),(t-t0).total_seconds(),k,e.get('event',{}).get('reuse') if isinstance(e.get('event'),dict) else None))
print(recs, len(spans))
end=max(s[1] for s in spans)
for sec in range(0,int(end)+1,3):
    run=[s for s in spans if s[0]<=sec<s[1] and s[1]-s[0]>0.2]
    print(sec,len(run),' '.join(sorted(x[2].split(':')[0].split('/')[-1]+':'+x[2].split(':')[1][-14:] for x in run))[:200])
