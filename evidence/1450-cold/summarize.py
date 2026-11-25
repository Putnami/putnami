#!/usr/bin/env python3
"""Summarize runs/*/ into a per-run table and per-series medians (stdout, Markdown)."""
import gzip, json, os, statistics, sys
D = os.path.dirname(os.path.abspath(__file__))
ORDER = ["warmup", "cold", "sel-cold", "sel-seed", "sel-warm", "warm", "unchanged"]

def load_stats(path):
    v = []
    for line in open(path):
        p = line.split()
        if len(p) >= 2:
            v.append(float(p[1]))
    if not v:
        return None
    s = sorted(v)
    return dict(n=len(v), mean=sum(v) / len(v), p90=s[min(len(s) - 1, int(0.9 * len(s)))], peak=max(v))

rows = []
for label in os.listdir(os.path.join(D, "runs")):
    d = os.path.join(D, "runs", label)
    if not os.path.exists(os.path.join(d, "run.json")):
        continue
    r = json.load(open(os.path.join(d, "run.json")))
    sp = os.path.join(d, "session.json.gz")
    sess = json.load(gzip.open(sp)) if os.path.exists(sp) else {}
    run = sess.get("run", {})
    orch = run.get("orchestration", {})
    cap = orch.get("capacity") or {}
    cpu = orch.get("cpu") or {}
    rows.append(dict(
        label=label, series=label.rstrip("0123456789").rsplit("-", 1)[0].split(":")[0], commit=r["commit"][:8],
        side="A" if r["commit"].startswith("00810ec6") else "B",
        session=r["session"], start=r["start"], end=r["end"], exit=r["exitCode"],
        outcome=run.get("outcome", "?"), durationS=(run.get("durationMs") or 0) / 1000,
        runCpuS=(cpu.get("runMs") or 0) / 1000, orchCpuS=(cpu.get("totalMs") or 0) / 1000,
        peakGranted=cap.get("peakGrantedCpus"), peakLoadCenti=cap.get("peakLoadCenti"),
        capSamples=len(cap.get("samples", [])) if cap else None,
        load=load_stats(os.path.join(d, "load.txt"))))
rows.sort(key=lambda x: x["start"])

def fmt(x, f="{:.1f}"):
    return "–" if x is None else f.format(x)

print("| run | commit | session | start (UTC) | outcome | duration s | run CPU s | orch. CPU s | peakGrantedCpus | peakLoadCenti | load1 mean / p90 / peak (n) |")
print("|---|---|---|---|---|---:|---:|---:|---:|---:|---|")
for x in rows:
    l = x["load"]
    print(f"| {x['label']} | {x['commit']} | `{x['session']}` | {x['start'][11:19]} | {x['outcome']} (exit {x['exit']}) | {x['durationS']:.1f} | {x['runCpuS']:.0f} | {x['orchCpuS']:.0f} | {fmt(x['peakGranted'], '{}')} | {fmt(x['peakLoadCenti'], '{}')} | "
          + (f"{l['mean']:.1f} / {l['p90']:.1f} / {l['peak']:.1f} ({l['n']})" if l else "–") + " |")

print()
print("| series | side | runs | duration s median [min–max] | load1 mean median | load1 p90 median | load1 peak median | peakGrantedCpus median |")
print("|---|---|---:|---|---:|---:|---:|---:|")
med = lambda v: statistics.median(v) if v else None
for s in sorted({x["series"] for x in rows}):
    for side in "AB":
        g = [x for x in rows if x["series"] == s and x["side"] == side]
        if not g:
            continue
        d = [x["durationS"] for x in g]
        pg = [x["peakGranted"] for x in g if x["peakGranted"] is not None]
        print(f"| {s} | {side} | {len(g)} | {med(d):.1f} [{min(d):.1f}–{max(d):.1f}] | {med([x['load']['mean'] for x in g]):.1f} | {med([x['load']['p90'] for x in g]):.1f} | {med([x['load']['peak'] for x in g]):.1f} | {fmt(med(pg), '{}')} |")
