#!/usr/bin/env python3
"""A/B measurement harness for epic #925.

Measures `./putnamiw lint,test,build,validate --all --enforce-coverage` on two
worktrees (A = before the epic, B = a later main) in three scenarios:

  cold       empty Putnami store, empty Go build cache, clean worktree
  small      one comment edited in a Go file of a leaf library
  unchanged  no change since the previous complete run

Each run executes inside its own cgroup (v2 cpu.stat usage_usec, with the v1
cpuacct controller as a cross-check) and as PID 1 of its own PID namespace with
BSD process accounting switched on in that namespace: every process of the run,
detached ones included, leaves one accounting record, and no process outside
the run does.

Usage:
  harness.py run <A|B> <cold|small|unchanged> <label>
  harness.py sequence <name>       # see SEQUENCES
  harness.py report                # aggregate data/ into results.json and table.md
"""

import glob
import gzip
import json
import os
import shutil
import statistics
import subprocess
import sys
import threading
import time

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.environ.get("M925_ROOT", "/home/user/m925")
WORKTREES = {"A": f"{ROOT}/A", "B": f"{ROOT}/B", "B2": f"{ROOT}/B2"}
STATE = f"{ROOT}/state"
DATA = os.environ.get("M925_DATA", f"{ROOT}/data")
SHARED_GO_MOD = os.path.expanduser("~/.putnami/cache/go/mod")
ARGS = os.environ.get("M925_ARGS", "lint,test,build,validate --all --enforce-coverage").split()
EDIT_FILE = "go/framework/parallel/parallel.go"
EDIT_ANCHOR = "// before invoking user work."
IDLE_LOAD = 0.5
IDLE_MAX_WAIT = 300

HARNESS_COMMS = {"run-inner.sh", "accton"}


def sh(cmd, **kw):
    return subprocess.run(cmd, check=True, text=True, capture_output=True, **kw).stdout


def run_env(commit):
    env = dict(os.environ)
    # No session upload: the measurement must not publish anything.
    env.pop("PUTNAMI_SESSION_REPORTER", None)
    # Each binary keeps its own GC default (B2 sets GOGC=200 unless GOGC is set).
    env.pop("GOGC", None)
    # No remote cache: the provider is never started (documented core switch).
    env["PUTNAMI_CACHE_TRUST"] = "none"
    env["PUTNAMI_STORE_DIR"] = f"{STATE}/{commit}/store"
    env["PUTNAMI_GO_CACHE_DIR"] = f"{STATE}/{commit}/go"
    return env


def reset_cold(commit):
    """Empty every local cache this run could reuse, the same way for A and B."""
    wt = WORKTREES[commit]
    shutil.rmtree(f"{STATE}/{commit}", ignore_errors=True)
    os.makedirs(f"{STATE}/{commit}/store")
    os.makedirs(f"{STATE}/{commit}/go/build")
    # Downloaded module versions are immutable and shared: no network in a run.
    os.symlink(SHARED_GO_MOD, f"{STATE}/{commit}/go/mod")
    sh(["git", "-C", wt, "checkout", "--", "."])
    sh(["git", "-C", wt, "clean", "-ffdxq"])


def edit_count(commit):
    path = f"{WORKTREES[commit]}/{EDIT_FILE}"
    with open(path) as f:
        for line in f:
            if line.startswith(EDIT_ANCHOR):
                rest = line[len(EDIT_ANCHOR):].strip()
                return int(rest.split()[-1]) if rest else 0
    raise SystemExit(f"anchor not found in {path}")


def apply_edit(commit):
    """Edit one comment line; each edit writes a text no earlier run saw."""
    path = f"{WORKTREES[commit]}/{EDIT_FILE}"
    n = edit_count(commit) + 1
    with open(path) as f:
        lines = f.readlines()
    for i, line in enumerate(lines):
        if line.startswith(EDIT_ANCHOR):
            lines[i] = f"{EDIT_ANCHOR} Measurement edit {n}\n"
    with open(path, "w") as f:
        f.writelines(lines)
    return n


def loadavg():
    with open("/proc/loadavg") as f:
        return [float(x) for x in f.read().split()[:3]]


def wait_idle():
    t0 = time.time()
    while loadavg()[0] > IDLE_LOAD and time.time() - t0 < IDLE_MAX_WAIT:
        time.sleep(5)
    return round(time.time() - t0, 1)


def machine():
    model = ""
    with open("/proc/cpuinfo") as f:
        for line in f:
            if line.startswith("model name"):
                model = line.split(":", 1)[1].strip()
                break
    return {"nproc": os.cpu_count(), "cpuModel": model}


def busy_processes():
    """Processes that used CPU in the second before the run, outside the run."""
    out = sh(["ps", "-eo", "pid=,pcpu=,comm="])
    rows = []
    for line in out.splitlines():
        parts = line.split(None, 2)
        if len(parts) == 3 and float(parts[1]) >= 5.0:
            rows.append({"pid": int(parts[0]), "pcpu": float(parts[1]), "comm": parts[2]})
    return rows


def read_kv(path):
    out = {}
    with open(path) as f:
        for line in f:
            k, v = line.split()
            out[k] = int(v)
    return out


def parse_acct(path):
    out = sh(["dump-acct", path])
    recs = []
    for line in out.splitlines():
        f = [x.strip() for x in line.split("|")]
        if len(f) < 15:
            continue
        recs.append({
            "comm": f[0], "utime": float(f[2]), "stime": float(f[3]),
            "etime": float(f[4]), "pid": int(f[9]), "ppid": int(f[10]),
            "exitcode": int(f[12]),
        })
    return recs


def classify(rec):
    c = rec["comm"]
    if rec["pid"] == 1 or c in HARNESS_COMMS:
        return "harness"
    if rec["ppid"] == 1 and c in ("ps", "sleep"):
        return "harness"
    if c in ("putnami", "putnamiw") or c.startswith("putnami-"):
        return "putnami"
    if c == "git" or c.startswith("git-"):
        return "putnami"
    return "tools"


def acct_summary(recs):
    # Linux accounts CPU in AHZ=100 ticks per process, truncated.
    sides = {"putnami": 0.0, "tools": 0.0, "harness": 0.0}
    by = {}
    for r in recs:
        cpu = (r["utime"] + r["stime"]) / 100.0
        side = classify(r)
        sides[side] += cpu
        key = (side, r["comm"])
        e = by.setdefault(key, {"side": side, "comm": r["comm"], "count": 0, "cpuS": 0.0})
        e["count"] += 1
        e["cpuS"] += cpu
    top = sorted(by.values(), key=lambda e: -e["cpuS"])
    for e in top:
        e["cpuS"] = round(e["cpuS"], 2)
    measured = sides["putnami"] + sides["tools"]
    return {
        "putnamiS": round(sides["putnami"], 2),
        "toolsS": round(sides["tools"], 2),
        "harnessS": round(sides["harness"], 2),
        "sharePct": round(100.0 * sides["putnami"] / measured, 2) if measured else None,
        "processes": len(recs),
        "byComm": top,
    }


def latest_session(wt, since):
    best = None
    for p in glob.glob(f"{wt}/.putnami/sessions/*/session.json"):
        m = os.path.getmtime(p)
        if m >= since and (best is None or m > best[0]):
            best = (m, p)
    return best[1] if best else None


def children(pid):
    try:
        with open(f"/proc/{pid}/task/{pid}/children") as f:
            return [int(x) for x in f.read().split()]
    except OSError:
        return []


def comm(pid):
    try:
        with open(f"/proc/{pid}/comm") as f:
            return f.read().strip()
    except OSError:
        return None


def vm_hwm_kib(pid):
    try:
        with open(f"/proc/{pid}/status") as f:
            for line in f:
                if line.startswith("VmHWM:"):
                    return int(line.split()[1])
    except OSError:
        pass
    return None


class Sampler(threading.Thread):
    """Samples from outside the run's cgroup, so it costs the run nothing.

    - the engine's peak RSS (VmHWM of the CLI process that putnamiw execs
      into, a child of the namespace's init), every second; VmHWM only grows,
      so the last sample is the peak up to one second before exit;
    - the 1-minute load average, every 5 seconds.
    """

    def __init__(self, unshare_pid):
        super().__init__(daemon=True)
        self.unshare_pid = unshare_pid
        self.stop = threading.Event()
        self.engine_pid = None
        self.engine_hwm = None
        self.load = []

    def run(self):
        t0 = time.time()
        next_load = t0
        while not self.stop.is_set():
            now = time.time()
            if now >= next_load:
                self.load.append([round(now - t0, 1), loadavg()[0]])
                next_load += 5
            if self.engine_pid is None:
                for init in children(self.unshare_pid):
                    for c in children(init):
                        if comm(c) == "putnami":
                            self.engine_pid = c
            if self.engine_pid is not None:
                v = vm_hwm_kib(self.engine_pid)
                if v is not None:
                    self.engine_hwm = v
            self.stop.wait(1.0)


def run(commit, scenario, label):
    wt = WORKTREES[commit]
    sha = sh(["git", "-C", wt, "rev-parse", "HEAD"]).strip()
    prep = {}
    if scenario == "cold":
        reset_cold(commit)
    elif scenario == "small":
        prep["edit"] = apply_edit(commit)
    elif scenario != "unchanged":
        raise SystemExit(f"unknown scenario {scenario}")
    rid = f"{label}-{commit}-{scenario}"
    out = f"{DATA}/{rid}"
    shutil.rmtree(out, ignore_errors=True)
    os.makedirs(out)
    idle_wait = wait_idle()
    busy = busy_processes()
    load = loadavg()
    cg2 = f"/sys/fs/cgroup/unified/m925-{rid}"
    cg1 = f"/sys/fs/cgroup/cpuacct/m925-{rid}"
    for cg in (cg2, cg1):
        if os.path.isdir(cg):
            os.rmdir(cg)
        os.mkdir(cg)
    t0 = time.time()
    inner = os.path.join(HERE, "run-inner.sh")
    script = (f'echo $$ > {cg2}/cgroup.procs && echo $$ > {cg1}/cgroup.procs && '
              f'exec unshare --pid --fork --mount-proc {inner} "$@"')
    proc = subprocess.Popen(["bash", "-c", script, "bash", wt, out, *ARGS],
                            env=run_env(commit))
    sampler = Sampler(proc.pid)
    sampler.start()
    proc.wait()
    outer_wall = time.time() - t0
    sampler.stop.set()
    sampler.join()
    loads = [x[1] for x in sampler.load]
    ncpu = os.cpu_count()
    samples = {
        "engineVmHWMKiB": sampler.engine_hwm,
        "loadSamples": sampler.load,
        "loadMax": max(loads) if loads else None,
        "loadMaxPerCpu": round(max(loads) / ncpu, 3) if loads else None,
        "loadMedianPerCpu": round(statistics.median(loads) / ncpu, 3) if loads else None,
    }
    stat = read_kv(f"{cg2}/cpu.stat")
    with open(f"{cg1}/cpuacct.usage") as f:
        v1_ns = int(f.read())
    with open(f"{cg2}/cgroup.procs") as f:
        left_in_cg = f.read().split()
    for cg in (cg2, cg1):
        try:
            os.rmdir(cg)
        except OSError:
            pass
    with open(f"{out}/timing.json") as f:
        timing = json.load(f)
    recs = parse_acct(f"{out}/pacct")
    with open(f"{out}/acct.json", "w") as f:
        json.dump(recs, f)
    os.remove(f"{out}/pacct")
    remote_lines = []
    with open(f"{out}/run.log", errors="replace") as f:
        for line in f:
            if "remote cache" in line.lower():
                remote_lines.append(line.strip()[:300])
    sess_path = latest_session(wt, t0)
    if sess_path:
        shutil.copy(sess_path, f"{out}/session.json")
    summary = {
        "id": rid, "commit": commit, "sha": sha, "scenario": scenario, "label": label,
        "prep": prep, "machine": machine(), "loadBefore": load,
        "idleWaitS": idle_wait, "busyBefore": busy,
        "rc": timing["rc"], "wallS": round(timing["end"] - timing["start"], 2),
        "outerWallS": round(outer_wall, 2), "unshareRc": proc.returncode,
        "cgroup": {"usageS": stat["usage_usec"] / 1e6, "userS": stat["user_usec"] / 1e6,
                   "systemS": stat["system_usec"] / 1e6, "v1UsageS": v1_ns / 1e9,
                   "leftInCgroup": left_in_cg},
        "leftover": timing["leftover"],
        "samples": samples,
        "remoteCacheLines": remote_lines,
        "acct": acct_summary(recs),
        "session": session_summary(f"{out}/session.json") if sess_path else None,
    }
    a = summary["acct"]
    # CPU the cgroup saw that no accounting record carries: Linux truncates
    # each process's CPU to 10 ms ticks, which loses most of a short process.
    a["unattributedS"] = round(summary["cgroup"]["usageS"] - a["putnamiS"] - a["toolsS"] - a["harnessS"], 2)
    tot = summary["cgroup"]["usageS"]
    a["shareOfTotalPct"] = round(100.0 * a["putnamiS"] / tot, 2) if tot else None
    a["shareUpperPct"] = round(100.0 * (a["putnamiS"] + max(a["unattributedS"], 0)) / tot, 2) if tot else None
    with open(f"{out}/summary.json", "w") as f:
        json.dump(summary, f, indent=1)
    a = summary["acct"]
    print(f"{rid}: rc={summary['rc']} wall={summary['wallS']}s cpu={summary['cgroup']['usageS']:.1f}s "
          f"putnami={a['putnamiS']}s tools={a['toolsS']}s share={a['sharePct']}% "
          f"load={load[0]} leftover={summary['leftover']} hwm={samples['engineVmHWMKiB']}KiB "
          f"loadmax={samples['loadMax']}", flush=True)
    return summary


def dig(d, *path):
    for p in path:
        if not isinstance(d, dict) or p not in d:
            return None
        d = d[p]
    return d


def session_summary(path):
    with open(path) as f:
        s = json.load(f)
    tasks = s.get("tasks") or []
    status = {}
    for t in tasks:
        k = t.get("status") or t.get("state") or "?"
        status[k] = status.get(k, 0) + 1
    sched = s.get("scheduler") or dig(s, "run", "scheduler") or {}
    ready = sched.get("readyWait") or []
    run = s.get("run") or {}
    return {
        "sessionId": s.get("sessionId") or s.get("id"),
        "taskStatus": status,
        "counts": run.get("counts"),
        "reuse": run.get("reuse"),
        "durationMs": run.get("durationMs"),
        "maxReadyWaitMs": max((r.get("maxMs", 0) for r in ready), default=None) if ready else None,
        "orchestrationCpu": dig(s, "run", "orchestration", "cpu"),
        "readyWait": ready,
        "remoteCache": s.get("remoteCache") or dig(s, "run", "remoteCache") or dig(s, "cache", "remote"),
    }


# Alternation A, B, A, B on the same machine.
SEQUENCES = {
    "cold": [("A", "cold"), ("B", "cold"), ("A", "cold"), ("B", "cold")],
    # A complete run precedes each one: the last cold run, then each previous run.
    "small": [("A", "small"), ("B", "small")] * 3,
    "unchanged": [("A", "unchanged"), ("B", "unchanged")] * 3,
    # Final main (B2) against A, same protocol.
    "cold-b2": [("A", "cold"), ("B2", "cold"), ("A", "cold"), ("B2", "cold")],
    "small-b2": [("A", "small"), ("B2", "small")] * 3,
    "unchanged-b2": [("A", "unchanged"), ("B2", "unchanged")] * 3,
}


def sequence(name, prefix):
    counts = {}
    for commit, scenario in SEQUENCES[name]:
        counts[commit] = counts.get(commit, 0) + 1
        run(commit, scenario, f"{prefix}{scenario}{counts[commit]}")


def median_range(xs):
    xs = [x for x in xs if x is not None]
    if not xs:
        return None
    return {"median": round(statistics.median(xs), 2), "min": round(min(xs), 2),
            "max": round(max(xs), 2), "n": len(xs)}


def task_reuse(path):
    """Tasks the run executed, against those a cache served, from tasks[]."""
    opener = gzip.open if path.endswith(".gz") else open
    with opener(path, "rt") as f:
        tasks = json.load(f).get("tasks") or []
    out = {"executed": 0, "localCache": 0, "coalesced": 0, "skipped": 0, "failed": 0, "total": len(tasks)}
    for t in tasks:
        reuse, status = t.get("reuse"), t.get("status")
        if reuse == "local-cache":
            out["localCache"] += 1
        elif reuse == "coalesced":
            out["coalesced"] += 1
        elif status == "success":
            out["executed"] += 1
        elif status == "skipped":
            out["skipped"] += 1
        else:
            out["failed"] += 1
    return out


def report():
    runs = []
    for p in sorted(glob.glob(f"{DATA}/*/summary.json")):
        with open(p) as f:
            r = json.load(f)
        sp = os.path.join(os.path.dirname(p), "session.json")
        if not os.path.exists(sp):
            sp += ".gz"
        if r.get("session") and os.path.exists(sp):
            r["session"]["tasks"] = task_reuse(sp)
        runs.append(r)
    groups = {}
    for r in runs:
        if r.get("excluded"):
            continue
        key = (r["label"].rstrip("0123456789"), r["commit"])
        groups.setdefault(key, []).append(r)
    table = []
    for (series, commit), rs in sorted(groups.items()):
        table.append({
            "series": series, "commit": commit, "sha": rs[0]["sha"], "runs": [r["id"] for r in rs],
            "wallS": median_range([r["wallS"] for r in rs]),
            "cpuS": median_range([r["cgroup"]["usageS"] for r in rs]),
            "putnamiS": median_range([r["acct"]["putnamiS"] for r in rs]),
            "toolsS": median_range([r["acct"]["toolsS"] for r in rs]),
            "sharePct": median_range([r["acct"]["sharePct"] for r in rs]),
            "unattributedS": median_range([r["acct"].get("unattributedS") for r in rs]),
            "shareUpperPct": median_range([r["acct"].get("shareUpperPct") for r in rs]),
            "executed": median_range([dig(r, "session", "tasks", "executed") for r in rs]),
            "localCacheHits": median_range([dig(r, "session", "tasks", "localCache") for r in rs]),
            "maxReadyWaitS": median_range([(dig(r, "session", "maxReadyWaitMs") or 0) / 1000
                                           if dig(r, "session", "maxReadyWaitMs") is not None else None for r in rs]),
            "orchestrationTotalS": median_range([
                (dig(r, "session", "orchestrationCpu", "totalMs") or 0) / 1000
                if dig(r, "session", "orchestrationCpu") else None for r in rs]),
            "rc": [r["rc"] for r in rs],
        })
    with open(f"{DATA}/results.json", "w") as f:
        json.dump({"runs": runs, "table": table}, f, indent=1)

    def fmt(m, unit="s"):
        if not m:
            return "n/a"
        return f"{m['median']:g}{unit} ({m['min']:g}–{m['max']:g})"
    lines = ["| Scenario | Commit | Wall | Total CPU (cgroup) | Putnami CPU | Tools CPU | Unattributed | Putnami share | Orchestration (session) | Executed / cache hits | Max ready wait | Exit codes |",
             "|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for t in table:
        ex = t["executed"]["median"] if t["executed"] else "n/a"
        hit = t["localCacheHits"]["median"] if t["localCacheHits"] else "n/a"
        lines.append(f"| {t['series']} | {t['commit']} | {fmt(t['wallS'])} | {fmt(t['cpuS'])} | "
                     f"{fmt(t['putnamiS'])} | {fmt(t['toolsS'])} | {fmt(t['unattributedS'])} | {fmt(t['sharePct'], '%')} | "
                     f"{fmt(t['orchestrationTotalS'])} | {ex} / {hit} | {fmt(t['maxReadyWaitS'])} | {t['rc']} |")
    with open(f"{DATA}/table.md", "w") as f:
        f.write("\n".join(lines) + "\n")
    print("\n".join(lines))


def main():
    if len(sys.argv) >= 2 and sys.argv[1] == "run":
        run(sys.argv[2], sys.argv[3], sys.argv[4])
    elif len(sys.argv) >= 2 and sys.argv[1] == "sequence":
        sequence(sys.argv[2], sys.argv[3] if len(sys.argv) > 3 else "")
    elif len(sys.argv) >= 2 and sys.argv[1] == "report":
        report()
    else:
        print(__doc__)
        sys.exit(2)


if __name__ == "__main__":
    main()
