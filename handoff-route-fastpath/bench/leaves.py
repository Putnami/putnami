#!/usr/bin/env python3
"""Leaves (self time) under the samples a given owner frame owns: leaves.py <profile> <requests> <owner substring> [--scale us]"""
import collections, json, sys
sys.path.insert(0, __import__('os').path.dirname(__file__))
from attribute import owner_key
prof, reqs, pat = sys.argv[1], int(sys.argv[2]), sys.argv[3]
scale = float(sys.argv[5]) if len(sys.argv) > 5 and sys.argv[4] == "--scale" else None
d = json.load(open(prof)); nodes = {n["id"]: n for n in d["nodes"]}; parent = {}
for n in d["nodes"]:
    for c in n.get("children", []): parent[c] = n["id"]
def owner(nid):
    cur = nid
    while cur is not None:
        k = owner_key(nodes[cur]["callFrame"])
        if k: return k
        cur = parent.get(cur)
    return "[none]"
t = collections.Counter(); total = 0
for i, nid in enumerate(d["samples"]):
    dt = d["timeDeltas"][i + 1] if i + 1 < len(d["timeDeltas"]) else 0
    total += dt
    if pat in owner(nid):
        cf = nodes[nid]["callFrame"]
        t[f"{cf.get('functionName') or '(anon)'} {cf.get('url','').split('/')[-1]}:{cf.get('lineNumber',0)+1}"] += dt
f = scale * reqs / total if scale else 1
for k, v in t.most_common(20): print(f"{v*f/reqs:6.2f} us  {k}")
