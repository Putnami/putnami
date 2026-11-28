#!/usr/bin/env python3
"""CPU per request of HTTP subjects under wrk. See README.md."""
import argparse, json, os, re, shutil, signal, statistics, subprocess, sys, time, urllib.request
from datetime import datetime, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
BUN = os.environ.get("BENCH_BUN", os.path.join(HERE, "bin", "bun"))
CLK = os.sysconf("SC_CLK_TCK")
ROUTES = {
    "health": ("/health", None),
    "get": ("/tasks/00000000-0000-4000-8000-000000000001", "get.lua"),
    "list": ("/tasks?limit=20", None),
    "insert": ("/tasks", "insert.lua"),
}
FRAMEWORK_PKGS = ["application", "database", "runtime", "migration", "utils"]


def log(*a):
    print(*a, file=sys.stderr, flush=True)


class Subject:
    def __init__(self, spec, idx, outdir):
        self.port = 39100 + idx
        self.proc = None
        kind, _, rest = spec.partition(":")
        self.kind = kind
        self.env = {}
        if kind == "putnami":
            path, _, envs = rest.partition("@")
            self.worktree = os.path.abspath(path)
            for kv in filter(None, envs.split(",")):
                k, _, v = kv.partition("=")
                self.env[k] = v
            short = os.path.basename(self.worktree.rstrip("/"))
            self.label = short + ("@" + envs if envs else "")
            self.dir = os.path.join(HERE, "run", re.sub(r"[^A-Za-z0-9_.-]", "_", self.label))
            self.stage()
            self.cmd = [BUN, "src/serve.ts"]
        elif kind in ("elysia", "bun"):
            self.label = kind
            self.dir = os.path.join(HERE, "subjects", kind)
            self.cmd = [BUN, "server.ts"]
        else:
            raise SystemExit(f"unknown subject {spec}")
        self.logf = os.path.join(outdir, f"{re.sub(r'[^A-Za-z0-9_.-]', '_', self.label)}.log")

    def stage(self):
        if not os.path.isdir(os.path.join(self.worktree, "typescript/framework/application")):
            raise SystemExit(f"{self.worktree} is not a putnami worktree")
        shutil.rmtree(self.dir, ignore_errors=True)
        shutil.copytree(os.path.join(HERE, "subjects", "putnami"), self.dir)
        nm = os.path.join(self.dir, "node_modules", "@putnami")
        os.makedirs(nm)
        for p in FRAMEWORK_PKGS:
            os.symlink(os.path.join(self.worktree, "typescript/framework", p), os.path.join(nm, p))

    def start(self, extra_args=()):
        env = dict(os.environ, NODE_ENV="production", PORT=str(self.port), PWD=self.dir)
        env.pop("PUTNAMI_PROJECT_ROOT", None)
        # This session exports BUN_OPTIONS=--smol (a smaller heap, more GC):
        # a service runs without it unless BENCH_BUN_OPTIONS names options.
        env.pop("BUN_OPTIONS", None)
        if os.environ.get("BENCH_BUN_OPTIONS"):
            env["BUN_OPTIONS"] = os.environ["BENCH_BUN_OPTIONS"]
        env.update(self.env)  # a subject's own env (after @) wins, BUN_OPTIONS included
        cmd = [self.cmd[0], *extra_args, *self.cmd[1:]]
        self.proc = subprocess.Popen(cmd, cwd=self.dir, env=env, stdout=open(self.logf, "w"),
                                     stderr=subprocess.STDOUT, start_new_session=True)
        for _ in range(200):
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{self.port}/health", timeout=1) as r:
                    if r.status == 200:
                        return
            except Exception:
                pass
            if self.proc.poll() is not None:
                raise SystemExit(f"{self.label} exited, see {self.logf}")
            time.sleep(0.1)
        raise SystemExit(f"{self.label} did not answer /health")

    def stop(self):
        if self.proc and self.proc.poll() is None:
            os.killpg(self.proc.pid, signal.SIGTERM)
            try:
                self.proc.wait(10)
            except subprocess.TimeoutExpired:
                os.killpg(self.proc.pid, signal.SIGKILL)

    def cpu_ticks(self):
        # The bun process itself (the Popen child is bun: no shell in between).
        with open(f"/proc/{self.proc.pid}/stat") as f:
            fields = f.read().rsplit(")", 1)[1].split()
        return int(fields[11]) + int(fields[12])  # utime + stime


def host_ticks():
    with open("/proc/stat") as f:
        v = [int(x) for x in f.readline().split()[1:]]
    idle = v[3] + v[4]
    return sum(v) - idle, sum(v)


def wrk(port, route, seconds, conns, threads):
    path, script = ROUTES[route]
    cmd = ["wrk", "-t", str(threads), "-c", str(conns), "-d", f"{seconds}s"]
    if script:
        cmd += ["-s", os.path.join(HERE, "wrk", script)]
    cmd.append(f"http://127.0.0.1:{port}{path}")
    out = subprocess.run(cmd, capture_output=True, text=True, check=True).stdout
    m = re.search(r"(\d+) requests in", out)
    non2xx = re.search(r"Non-2xx or 3xx responses: (\d+)", out)
    errs = re.search(r"Socket errors: (.*)", out)
    return int(m.group(1)), int(non2xx.group(1)) if non2xx else 0, errs.group(1) if errs else ""


def measure(s, route, a):
    wrk(s.port, route, a.warmup, a.connections, a.threads)
    load1 = os.getloadavg()[0]
    c0, (hb0, ht0) = s.cpu_ticks(), host_ticks()
    reqs, non2xx, errs = wrk(s.port, route, a.duration, a.connections, a.threads)
    c1, (hb1, ht1) = s.cpu_ticks(), host_ticks()
    us = (c1 - c0) / CLK * 1e6 / reqs
    busy = 100.0 * (hb1 - hb0) / max(1, ht1 - ht0)
    return {"subject": s.label, "route": route, "requests": reqs, "rps": round(reqs / a.duration),
            "cpu_us_per_req": round(us, 2), "service_cpu_s": round((c1 - c0) / CLK, 2),
            "load1": round(load1, 2), "host_busy_pct": round(busy, 1), "non2xx": non2xx, "socket_errors": errs}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--putnami", action="append", default=[], help="worktree[@ENV=V,ENV=V] (repeatable)")
    ap.add_argument("--elysia", action="store_true")
    ap.add_argument("--bun", action="store_true")
    ap.add_argument("--routes", default="health,get")
    ap.add_argument("--rounds", type=int, default=3)
    ap.add_argument("--duration", type=int, default=20)
    ap.add_argument("--warmup", type=int, default=5)
    ap.add_argument("--connections", type=int, default=64)
    ap.add_argument("--threads", type=int, default=2)
    ap.add_argument("--no-parity", action="store_true")
    ap.add_argument("--out", default=None)
    a = ap.parse_args()
    specs = [f"putnami:{p}" for p in a.putnami] + (["elysia"] if a.elysia else []) + (["bun"] if a.bun else [])
    if not specs:
        raise SystemExit("no subject")
    routes = a.routes.split(",")
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    outdir = a.out or os.path.join(HERE, "results", stamp)
    os.makedirs(outdir, exist_ok=True)
    reset = subprocess.run(["psql", "-q", "-h", "127.0.0.1", "-p", os.environ.get("BENCH_PG_PORT", "55432"),
                            "-U", "postgres", "-d", "bench", "-f", os.path.join(HERE, "subjects/common/schema.sql")],
                           capture_output=True, text=True)
    if reset.returncode:
        raise SystemExit(reset.stderr)
    subjects = [Subject(sp, i, outdir) for i, sp in enumerate(specs)]
    rows = []
    try:
        for s in subjects:
            s.start()
        if not a.no_parity and len(subjects) > 1:
            p = subprocess.run([sys.executable, "-I", os.path.join(HERE, "parity.py"),
                                *[str(s.port) for s in subjects]], capture_output=True, text=True)
            open(os.path.join(outdir, "parity.txt"), "w").write(p.stdout)
            log(p.stdout.strip().splitlines()[-1] if p.stdout else p.stderr)
            if p.returncode:
                log(p.stdout)
                raise SystemExit("parity failed (pass --no-parity to measure anyway)")
            # parity inserted rows: reset so every run starts from the same table
            subprocess.run(["psql", "-q", "-h", "127.0.0.1", "-p", os.environ.get("BENCH_PG_PORT", "55432"),
                            "-U", "postgres", "-d", "bench", "-c", "DELETE FROM tasks WHERE title NOT LIKE 'Task %'"],
                           capture_output=True)
        meta = {"bun": subprocess.run([BUN, "--version"], capture_output=True, text=True).stdout.strip(),
                "subjects": {s.label: s.__dict__.get("worktree", s.kind) for s in subjects},
                "heads": {}, "args": vars(a), "bun_options": os.environ.get("BENCH_BUN_OPTIONS", ""), "start": stamp, "nproc": os.cpu_count()}
        for s in subjects:
            if s.kind == "putnami":
                meta["heads"][s.label] = subprocess.run(["git", "-C", s.worktree, "rev-parse", "--short", "HEAD"],
                                                        capture_output=True, text=True).stdout.strip()
        json.dump(meta, open(os.path.join(outdir, "meta.json"), "w"), indent=1)
        n = len(subjects)
        for r in range(a.rounds):
            order = subjects[r % n:] + subjects[:r % n]
            for route in routes:
                for s in order:
                    row = measure(s, route, a)
                    row["round"] = r + 1
                    rows.append(row)
                    log(json.dumps(row))
                    with open(os.path.join(outdir, "rows.jsonl"), "a") as f:
                        f.write(json.dumps(row) + "\n")
    finally:
        for s in subjects:
            s.stop()
    # summary
    lines = ["| subject | route | " + " | ".join(f"r{i + 1} us" for i in range(a.rounds)) +
             " | median us | load1 (min-max) | host busy % |", "|---|---|" + "---|" * (a.rounds + 3)]
    for route in routes:
        for s in subjects:
            rs = sorted((x for x in rows if x["subject"] == s.label and x["route"] == route), key=lambda x: x["round"])
            if not rs:
                continue
            us = [x["cpu_us_per_req"] for x in rs]
            loads = [x["load1"] for x in rs]
            busy = [x["host_busy_pct"] for x in rs]
            bad = sum(x["non2xx"] for x in rs)
            lines.append(f"| {s.label} | {route} | " + " | ".join(f"{u:.1f}" for u in us) +
                         f" | **{statistics.median(us):.1f}** | {min(loads):.2f}-{max(loads):.2f} | "
                         f"{statistics.mean(busy):.0f}" + (f" (non2xx {bad})" if bad else "") + " |")
    summary = "\n".join(lines)
    open(os.path.join(outdir, "summary.md"), "w").write(summary + "\n")
    print(summary)
    print(f"\nresults: {outdir}")


if __name__ == "__main__":
    main()
