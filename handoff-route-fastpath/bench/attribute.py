#!/usr/bin/env python3
"""Attribute a .cpuprofile's samples to the nearest framework/subject frame.

attribute.py <profile.cpuprofile> <requests> [--scale <unprofiled us/req>] [--top N]

Each sample's time goes to the first frame, from the leaf up, whose script is
Putnami framework code, the bench subject or Elysia; native and Bun-internal
frames under it (Response, Bun.SQL's JS, JSON.stringify, Promise) count for
it. Samples with no such frame are grouped by their leaf (event loop, socket
I/O, GC, Bun.SQL's own continuations). --scale rescales us/request so the
total equals an unprofiled measurement (the profiler inflates CPU).
"""
import argparse, collections, json


def owner_key(cf):
    url = cf.get("url", "")
    name = cf.get("functionName") or "(anonymous)"
    line = cf.get("lineNumber", 0) + 1
    if "/typescript/framework/" in url:
        return f"{name} {url.split('/typescript/framework/')[1]}:{line}"
    if "/bench/run/" in url or "/bench/subjects/" in url:
        return f"{name} subject/{url.split('/src/')[-1] if '/src/' in url else url.split('/')[-1]}:{line}"
    if "node_modules/elysia" in url:
        return f"{name} elysia/{url.split('node_modules/elysia/')[1]}:{line}"
    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("profile")
    ap.add_argument("requests", type=int)
    ap.add_argument("--scale", type=float)
    ap.add_argument("--top", type=int, default=40)
    ap.add_argument("--file", action="store_true", help="group by file instead of function")
    a = ap.parse_args()
    d = json.load(open(a.profile))
    nodes = {n["id"]: n for n in d["nodes"]}
    parent = {}
    for n in d["nodes"]:
        for c in n.get("children", []):
            parent[c] = n["id"]
    cache = {}

    def owner(nid):
        if nid in cache:
            return cache[nid]
        cur, leaf = nid, nodes[nid]["callFrame"]
        res = None
        while cur is not None:
            k = owner_key(nodes[cur]["callFrame"])
            if k:
                res = k
                break
            cur = parent.get(cur)
        if res is None:
            u = leaf.get("url", "")
            res = f"[no framework frame] {leaf.get('functionName') or '(anonymous)'} {u.split('/')[-1]}"
        if a.file and not res.startswith("["):
            name, loc = res.split(" ", 1)
            res = loc.rsplit(":", 1)[0]
        cache[nid] = res
        return res

    t = collections.Counter()
    samples, deltas = d["samples"], d["timeDeltas"]
    for i, nid in enumerate(samples):
        t[owner(nid)] += deltas[i + 1] if i + 1 < len(deltas) else 0
    total = sum(t.values())
    factor = (a.scale * a.requests / total) if a.scale else 1.0
    print(f"sampled {total / 1e6:.2f} s, {total / a.requests:.2f} us/req; scale factor {factor:.3f}")
    print("| us/req | share | owner |\n|---:|---:|---|")
    for k, v in t.most_common(a.top):
        print(f"| {v * factor / a.requests:.2f} | {100 * v / total:.1f}% | `{k}` |")


if __name__ == "__main__":
    main()
