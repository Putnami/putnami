import json,sys,re,datetime
d=sys.argv[1]
plan={t['identity']['key']:t.get('after',[]) for t in json.load(open(d+'/plan.json'))['tasks']}
st={};en={};t0=None
for l in open(d+'/events.jsonl'):
    e=json.loads(l); t=e.get('time'); k=(e.get('identity') or {}).get('key')
    if not t or not k: continue
    t=datetime.datetime.fromisoformat(re.sub(r'(\.\d{6})\d*',r'\1',t).replace('Z','+00:00'))
    if t0 is None: t0=t
    s=(t-t0).total_seconds()
    if e['record']=='task:start': st[k]=s
    elif e['record']=='task:end': en[k]=s
end=max(en.values())
for sec in range(0,int(end)+1,3):
    running=[k for k in st if st[k]<=sec<en.get(k,1e9) and en.get(k,1e9)-st[k]>0.2]
    ready=[k for k in plan if k not in st or st[k]>sec if all(en.get(a,1e9)<=sec for a in plan[k] if a in plan)]
    ready=[k for k in ready if (k not in st or st[k]>sec)]
    print(sec,'run',len(running),'ready',len(ready),' '.join(sorted(r.split(':')[0].split('/')[-1]+':'+r.split(':')[1][-12:] for r in ready))[:150])
