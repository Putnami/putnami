#!/usr/bin/env python3
"""Compare a run's orchestration report with what the kernel and a git shim saw.

Usage: orchestration_counts.py <strace-file> <git-shim-log> <session.json>

The strace file comes from
    strace -f -qq -v -s 4096 -e trace=execve,clone,clone3,fork,vfork -o <file> <putnami ...>
and the git shim log has one line per call: "<caller pid> <shim pid> <git
arguments>".

A process the report counts is one the CLI process started (origin engine) or
one a job process started (origin jobs): a new process whose first successful
execve happens while its parent is the CLI process or a job process. A job
process is a child of the CLI process started with --putnamiContext. A process
a tool started is not counted, nor the execve a process makes over itself.
"""

import json
import re
import sys
from collections import Counter

LINE = re.compile(r"^(\d+)\s+(.*)$")
CALL = re.compile(r"^(clone3?|fork|vfork|execve)\((.*)$")
RESUMED = re.compile(r"^<\.\.\. (clone3?|fork|vfork|execve) resumed>(.*)$")
RESULT = re.compile(r"=\s+(-?\d+)")


def parse(path):
    """Yield (pid, syscall, args, result) with unfinished calls joined."""
    pending = {}
    with open(path, encoding="utf-8", errors="replace") as handle:
        for raw in handle:
            match = LINE.match(raw.rstrip("\n"))
            if not match:
                continue
            pid, rest = int(match.group(1)), match.group(2)
            if rest.startswith("+++") or rest.startswith("---"):
                continue
            resumed = RESUMED.match(rest)
            if resumed:
                name, args = pending.pop(pid, (resumed.group(1), ""))
                rest_args = args + resumed.group(2)
                result = RESULT.search(resumed.group(2))
                yield pid, name, rest_args, int(result.group(1)) if result else None
                continue
            call = CALL.match(rest)
            if not call:
                continue
            name, args = call.group(1), call.group(2)
            if "<unfinished ...>" in args:
                pending[pid] = (name, args.replace("<unfinished ...>", ""))
                continue
            result = RESULT.search(args.rsplit(")", 1)[-1]) if ")" in args else None
            yield pid, name, args, int(result.group(1)) if result else None


def first_arg(args):
    match = re.match(r'"([^"]*)"', args)
    return match.group(1) if match else ""


def argv(args):
    match = re.search(r"\[(.*?)\](?:,|\s|$)", args)
    if not match:
        return []
    return re.findall(r'"((?:[^"\\]|\\.)*)"', match.group(1))


def main():
    strace_file, shim_log, session_file = sys.argv[1:4]
    tgid = {}      # thread id -> process id
    parent = {}    # process id -> parent process id
    image = {}     # process id -> argv of its current image
    execed = set()  # processes that made a successful execve since they started
    root = None
    engine_ready = False  # the traced root runs the CLI, not the wrapper
    starts = []    # (parent pid, child pid, argv) for each process's first execve

    events = list(parse(strace_file))
    # A vfork child runs, and execs, before its parent's clone returns, so the
    # process tree is read whole before any start is attributed.
    for pid, name, args, result in events:
        proc = tgid.setdefault(pid, pid)
        if root is None:
            root = proc
        if name in ("clone", "clone3", "fork", "vfork") and result is not None and result > 0:
            if "CLONE_THREAD" in args:
                tgid[result] = proc
            else:
                tgid[result] = result
                parent[result] = proc
    for pid, name, args, result in events:
        proc = tgid.get(pid, pid)
        if name != "execve" or result != 0:
            continue
        vector = argv(args) or [first_arg(args)]
        if proc == root and first_arg(args).rsplit("/", 1)[-1] == "putnami":
            engine_ready = True
        # A process the wrapper started before it became the CLI is not the
        # run's.
        if proc not in execed and proc != root and (parent.get(proc) != root or engine_ready):
            starts.append((parent.get(proc), proc, vector))
        execed.add(proc)
        image[proc] = vector

    # The engine is the process whose image is the putnami binary running the
    # gate: the last image the traced root took, which the wrapper execs.
    engine = root
    jobs = {child for owner, child, vector in starts if owner == engine and "--putnamiContext" in vector}

    from_engine = [s for s in starts if s[0] == engine]
    from_jobs = [s for s in starts if s[0] in jobs]

    counted = {child for _, child, _ in from_engine + from_jobs}
    shim = Counter()
    with open(shim_log, encoding="utf-8", errors="replace") as handle:
        for line in handle:
            fields = line.strip().split(" ", 2)
            if len(fields) < 2 or not fields[0].isdigit() or not fields[1].isdigit():
                continue
            caller, call = int(fields[0]), int(fields[1])
            if call not in counted:
                continue
            origin = "engine" if caller == engine else "jobs" if caller in jobs else None
            if origin:
                shim[origin] += 1

    with open(session_file, encoding="utf-8") as handle:
        orchestration = json.load(handle)["run"].get("orchestration") or {}
    reported = Counter()
    reported_git = Counter()
    for entry in orchestration.get("processes", []):
        reported[entry["origin"]] += entry["count"]
    for entry in orchestration.get("git", []):
        reported_git[entry["origin"]] += entry["count"]

    rows = []
    for label, observed, counted in (
        ("processes from the engine", len(from_engine), reported["engine"]),
        ("processes from jobs", len(from_jobs), reported["jobs"]),
        ("processes, total", len(from_engine) + len(from_jobs), reported["engine"] + reported["jobs"]),
        ("git calls from the engine", shim["engine"], reported_git["engine"]),
        ("git calls from jobs", shim["jobs"], reported_git["jobs"]),
        ("git calls, total", shim["engine"] + shim["jobs"], reported_git["engine"] + reported_git["jobs"]),
    ):
        gap = (counted - observed) / observed * 100 if observed else 0.0
        rows.append({"measure": label, "observed": observed, "reported": counted, "gapPercent": round(gap, 2)})

    kinds = Counter(vector[0].rsplit("/", 1)[-1] if vector else "?" for _, _, vector in from_engine)
    print(json.dumps({"enginePid": engine, "jobProcesses": len(jobs), "rows": rows,
                      "engineChildrenByProgram": kinds.most_common(12)}, indent=2))


if __name__ == "__main__":
    main()
