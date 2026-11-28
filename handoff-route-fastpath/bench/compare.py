#!/usr/bin/env python3
"""Side-by-side leaf self time (us/request, scaled to unprofiled CPU) of two profiles.

compare.py <A.cpuprofile> <A requests> <A unprofiled us> <B.cpuprofile> <B requests> <B unprofiled us> [--top N] [--owner]
--owner groups by the nearest framework/subject/Elysia frame (attribute.py) instead of the leaf.
"""
import collections, json, sys, os
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from attribute import owner_key

def load(path, reqs, us, by_owner):
    d = json.load(open(path)); nodes = {n["id"]: n for n in d["nodes"]}; parent = {}
    for n in d["nodes"]:
        for c in n.get("children", []): parent[c] = n["id"]
    t = collections.Counter(); total = 0
    for i, nid in enumerate(d["samples"]):
        dt = d["timeDeltas"][i + 1] if i + 1 < len(d["timeDeltas"]) else 0
        total += dt
        if by_owner:
            cur, k = nid, None
            while cur is not None and k is None:
                k = owner_key(nodes[cur]["callFrame"]); cur = parent.get(cur)
            key = k or "[no framework frame]"
        else:
            cf = nodes[nid]["callFrame"]; u = cf.get("url", "")
            f = u.split("/typescript/framework/")[-1] if "/typescript/framework/" in u else u.split("/")[-1]
            key = f"{cf.get('functionName') or '(anon)'} {f}:{cf.get('lineNumber', 0) + 1}" if u else (cf.get('functionName') or '(anon)')
        t[key] += dt
    f = us * reqs / total
    return {k: v * f / reqs for k, v in t.items()}

a = sys.argv[1:7]; top = int(sys.argv[sys.argv.index("--top") + 1]) if "--top" in sys.argv else 30
by_owner = "--owner" in sys.argv
A = load(a[0], int(a[1]), float(a[2]), by_owner); Bp = load(a[3], int(a[4]), float(a[5]), by_owner)
keys = sorted(set(A) | set(Bp), key=lambda k: -max(A.get(k, 0), Bp.get(k, 0)))[:top]
print("| A us | B us | function |\n|---:|---:|---|")
for k in keys: print(f"| {A.get(k, 0):.2f} | {Bp.get(k, 0):.2f} | `{k}` |")
print(f"| {sum(A.values()):.2f} | {sum(Bp.values()):.2f} | total |")
