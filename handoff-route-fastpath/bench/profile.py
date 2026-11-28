#!/usr/bin/env python3
"""CPU-profile one subject under wrk and list the top self-time functions in us/request.

profile.py --putnami <worktree>[@ENV=V,...] | --elysia | --bun  --route health [--duration 20]
           [--interval 200] [--top 40] [--out DIR]
Run it through prof.sh, which takes the machine lock.
"""
import argparse, collections, glob, json, os, sys, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import bench  # noqa: E402


def analyze(path, requests, top, measured_us):
    prof = json.load(open(path))
    nodes = {n["id"]: n for n in prof["nodes"]}
    # sample time per node from timeDeltas
    t = collections.Counter()
    samples, deltas = prof["samples"], prof["timeDeltas"]
    for i, nid in enumerate(samples):
        d = deltas[i + 1] if i + 1 < len(deltas) else 0
        t[nid] += d
    total = sum(t.values())
    per_fn = collections.Counter()
    per_pkg = collections.Counter()
    for nid, us in t.items():
        cf = nodes[nid]["callFrame"]
        url = cf.get("url", "")
        name = cf.get("functionName") or "(anonymous)"
        short = url.split("/typescript/framework/")[-1] if "/typescript/framework/" in url else url.split("/")[-1]
        key = f"{name} {short}:{cf.get('lineNumber', 0) + 1}" if url else name
        per_fn[key] += us
        if "/typescript/framework/" in url:
            pkg = "putnami/" + url.split("/typescript/framework/")[1].split("/")[0]
        elif "node_modules/elysia" in url:
            pkg = "elysia"
        elif url.startswith("file://") or url.startswith("/"):
            pkg = "subject" if "/bench/" in url else url.split("/")[-1]
        elif url:
            pkg = "bun-builtin:" + url.split("/")[-1]
        else:
            pkg = name if name.startswith("(") else "native:" + name
        per_pkg[pkg] += us
    lines = [f"profile {os.path.basename(path)}: {total / 1e6:.2f} s sampled, {requests} requests, "
             f"{total / requests:.2f} us/req sampled on the JS thread; measured process CPU {measured_us:.2f} us/req", "",
             "| self us/req | share | package |", "|---:|---:|---|"]
    for k, v in per_pkg.most_common(25):
        lines.append(f"| {v / requests:.2f} | {100 * v / total:.1f}% | {k} |")
    lines += ["", "| self us/req | share | function |", "|---:|---:|---|"]
    for k, v in per_fn.most_common(top):
        lines.append(f"| {v / requests:.3f} | {100 * v / total:.1f}% | `{k}` |")
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--putnami")
    ap.add_argument("--elysia", action="store_true")
    ap.add_argument("--bun", action="store_true")
    ap.add_argument("--route", default="health")
    ap.add_argument("--duration", type=int, default=20)
    ap.add_argument("--warmup", type=int, default=5)
    ap.add_argument("--interval", type=int, default=200)
    ap.add_argument("--top", type=int, default=45)
    ap.add_argument("--out")
    a = ap.parse_args()
    spec = f"putnami:{a.putnami}" if a.putnami else ("elysia" if a.elysia else "bun")
    out = a.out or os.path.join(bench.HERE, "profiles", time.strftime("%Y%m%dT%H%M%S"))
    os.makedirs(out, exist_ok=True)
    s = bench.Subject(spec, 9, out)
    s.start([f"--cpu-prof", f"--cpu-prof-dir={out}", f"--cpu-prof-interval={a.interval}"])
    try:
        warm, _, _ = bench.wrk(s.port, a.route, a.warmup, 64, 2)
        c0 = s.cpu_ticks()
        reqs_measured, _, _ = bench.wrk(s.port, a.route, a.duration, 64, 2)
        c1 = s.cpu_ticks()
        load = os.getloadavg()[0]
    finally:
        s.stop()
    time.sleep(1)
    profs = sorted(glob.glob(os.path.join(out, "*.cpuprofile")), key=os.path.getmtime)
    if not profs:
        raise SystemExit("no profile written")
    # The profile covers the whole process life (start-up, warm-up and the
    # measured run): its time is divided by the requests of both wrk runs.
    measured_us = (c1 - c0) / bench.CLK * 1e6 / reqs_measured
    total_reqs = reqs_measured + warm
    report = analyze(profs[-1], total_reqs, a.top, measured_us)
    report = f"{s.label} route={a.route} load1={load:.2f}\n" + report
    open(os.path.join(out, f"{s.label}-{a.route}.md".replace("/", "_")), "w").write(report + "\n")
    print(report)


if __name__ == "__main__":
    main()
