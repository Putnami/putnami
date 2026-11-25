import json,sys,re,datetime,math
d,hist=sys.argv[1],sys.argv[2]
H=json.load(open(hist))['tasks']
def charge(k):
    p,n=k.split(':',1); v=H.get(p+'|'+n)
    if not v or not v.get('wallMs'): return 1000
    return max(100,math.ceil(v['cpuMs']/v['wallMs']*1000))
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
for sec in [x/2 for x in range(0,int(end*2)+1,3)]:
    running=[k for k in st if st[k]<=sec<en.get(k,1e9)]
    print(sec,len(running),sum(charge(k) for k in running),' '.join('%s=%d'%(k.split(':')[0].split('/')[-1]+':'+k.split(':')[1][-10:],charge(k)) for k in running)[:170])
