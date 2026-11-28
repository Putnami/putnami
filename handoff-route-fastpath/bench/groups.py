#!/usr/bin/env python3
"""Group a profile's time (scaled to unprofiled us/request) into the epic's leads.

groups.py <A.cpuprofile> <A reqs> <A us> [<B.cpuprofile> <B reqs> <B us>]
A sample goes to Bun.SQL when its leaf is Bun.SQL's own code (builtins shared/
query/pool/postgres/bun:sql, native createQuery/run); otherwise to the group
of its nearest framework/subject/Elysia frame (natives under it count for it);
otherwise to "event loop, sockets, GC".
"""
import collections, json, os, re, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from attribute import owner_key

SQL_LEAF = re.compile(r"^(shared|query|pool|postgres|bun:sql|sql)$")
RULES = [
    ("db: statement build (Repository fragments, captured-query replay)", r"sql-query\.ts:(367|395)|repository-where|buildOrderBy|identList"),
    ("db: row copies (client camelCase copy, toEntity)", r"clientResult|convertRow|camelKey|plainValue|toEntity|entity\.helper"),
    ("db: abortableQuery hooks", r"abort\.ts|onAbort|offAbort|aborted application|abortListeners|http\.plugin\.ts:(877|882|893|899|909)"),
    ("db: Repository observe/metrics/logging", r"repository\.ts|sql-metrics|database-logging|observability"),
    ("db: client, pool lease, connection", r"datasource-client|driver-pool|physical-pool|factory\.ts|transaction\.ts|sql-query\.ts|primary-datasource|sql-client"),
    ("http: deadline (ServedRequest, RequestDeadlines)", r"request-deadlines|http\.plugin\.ts:(87\d|88\d|94\d|95\d|96\d)"),
    ("http: context build (HttpContext, UrlScanner)", r"http-context\.builder|url\.scanner"),
    ("http: native route entry, serveRequest, chain, finish", r"http\.plugin\.ts|route-scope|http-methods"),
    ("http: dispatcher, negotiation, endpoint answer", r"http-dispatcher|router\.ts|endpoint-helpers|endpoint\.ts|content-negotiation"),
    ("http: validation", r"validate\.ts|validation\.ts|schema"),
    ("http: response build (HttpResponse, json)", r"http-response"),
    ("http: logger middleware", r"logger"),
    ("http: security headers", r"security-headers|minimal\.util"),
    ("http: origin guard", r"origin-guard"),
    ("runtime: context (AsyncLocalStorage), DI", r"runtime/src|async_hooks|context"),
    ("subject code", r"subject/"),
    ("elysia + hand-written protections", r"elysia/|protect\.ts"),
]


def load(path, reqs, us):
    d = json.load(open(path)); nodes = {n["id"]: n for n in d["nodes"]}; parent = {}
    for n in d["nodes"]:
        for c in n.get("children", []): parent[c] = n["id"]
    cache = {}
    def group(nid):
        if nid in cache: return cache[nid]
        cf = nodes[nid]["callFrame"]; u = cf.get("url", ""); base = u.split("/")[-1]
        if SQL_LEAF.match(base) or (not u and cf.get("functionName") in ("createQuery", "run")):
            g = "Bun.SQL (driver JS and native)"
        else:
            cur, k = nid, None
            while cur is not None and k is None:
                k = owner_key(nodes[cur]["callFrame"]); cur = parent.get(cur)
            if k is None:
                g = "event loop, sockets, GC (no framework frame)"
            else:
                g = next((name for name, rx in RULES if re.search(rx, k)), "other: " + k)
        cache[nid] = g
        return g
    t = collections.Counter(); total = 0
    for i, nid in enumerate(d["samples"]):
        dt = d["timeDeltas"][i + 1] if i + 1 < len(d["timeDeltas"]) else 0
        total += dt; t[group(nid)] += dt
    f = us * reqs / total
    return {k: v * f / reqs for k, v in t.items()}

a = sys.argv[1:]
A = load(a[0], int(a[1]), float(a[2]))
Bg = load(a[3], int(a[4]), float(a[5])) if len(a) >= 6 else {}
keys = sorted(set(A) | set(Bg), key=lambda k: -(A.get(k, 0) - Bg.get(k, 0)))
print("| group | A us | B us | A - B |\n|---|---:|---:|---:|")
for k in keys:
    va, vb = A.get(k, 0), Bg.get(k, 0)
    if max(va, vb) < 0.05: continue
    print(f"| {k} | {va:.2f} | {vb:.2f} | {va - vb:+.2f} |")
print(f"| total | {sum(A.values()):.2f} | {sum(Bg.values()):.2f} | {sum(A.values()) - sum(Bg.values()):+.2f} |")
