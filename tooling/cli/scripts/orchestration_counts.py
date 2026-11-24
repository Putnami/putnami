#!/usr/bin/env python3
"""Compare a run's orchestration report with what the kernel and a git shim saw.

Usage: orchestration_counts.py <strace-file> <git-shim-log> <session.json>

The strace file comes from
    strace -f -qq -ttt -v -s 4096 -e trace=execve,clone,clone3,fork,vfork -o <file> <putnami ...>
and the git shim log has one line per call: "<caller pid> <shim pid> <git
arguments>".

A process the report counts is one the CLI process started (origin engine) or
one a job process started (origin jobs): a new process whose first successful
execve happens while its parent is the CLI process or a job process, before
the session was settled (its endTime, read against the trace's -ttt
timestamps). A job process is a child of the CLI process started with
--putnamiContext. A process a tool started is not counted, nor the execve a
process makes over itself. A git call is such a start whose program is git;
the shim log, which has no time, gives an upper bound for the engine's.
"""

import bisect
import json
import re
import sys
from collections import Counter
from datetime import datetime

LINE = re.compile(r"^(\d+)\s+(?:(\d+\.\d+)\s+)?(.*)$")
CALL = re.compile(r"^(clone3?|fork|vfork|execve)\((.*)$")
RESUMED = re.compile(r"^<\.\.\. (clone3?|fork|vfork|execve) resumed>(.*)$")
RESULT = re.compile(r"=\s+(-?\d+)")


def parse(path):
    """Yield (pid, time, syscall, args, result) with unfinished calls joined.

    The time is the call's start, in seconds since the epoch, when the trace
    has -ttt timestamps, and None otherwise."""
    pending = {}
    with open(path, encoding="utf-8", errors="replace") as handle:
        for raw in handle:
            match = LINE.match(raw.rstrip("\n"))
            if not match:
                continue
            pid, rest = int(match.group(1)), match.group(3)
            stamp = float(match.group(2)) if match.group(2) else None
            if rest.startswith("+++") or rest.startswith("---"):
                continue
            resumed = RESUMED.match(rest)
            if resumed:
                name, args, started = pending.pop(pid, (resumed.group(1), "", stamp))
                rest_args = args + resumed.group(2)
                result = RESULT.search(resumed.group(2))
                yield pid, started, name, rest_args, int(result.group(1)) if result else None
                continue
            call = CALL.match(rest)
            if not call:
                continue
            name, args = call.group(1), call.group(2)
            if "<unfinished ...>" in args:
                pending[pid] = (name, args.replace("<unfinished ...>", ""), stamp)
                continue
            result = RESULT.search(args.rsplit(")", 1)[-1]) if ")" in args else None
            yield pid, stamp, name, args, int(result.group(1)) if result else None


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
    with open(session_file, encoding="utf-8") as handle:
        session = json.load(handle)
    settled = None
    if session.get("endTime"):
        settled = datetime.fromisoformat(session["endTime"].replace("Z", "+00:00")).timestamp()

    # A long run reuses process ids, so a process is a pid and its birth: the
    # clone that returned that pid. A vfork child runs, and execs, before its
    # parent's clone returns, so a birth starts at the clone call, and the
    # births are read whole before any execve is attributed.
    births = {}    # pid -> [(clone start, caller thread id, is a thread)]
    execs = []     # (pid, time, argv) for each successful execve
    root = None
    for pid, stamp, name, args, result in parse(strace_file):
        if root is None:
            root = pid
        if name in ("clone", "clone3", "fork", "vfork") and result is not None and result > 0:
            births.setdefault(result, []).append((stamp or 0.0, pid, "CLONE_THREAD" in args))
        elif name == "execve" and result == 0:
            execs.append((pid, stamp or 0.0, argv(args) or [first_arg(args)]))
    for entries in births.values():
        entries.sort(key=lambda entry: entry[0])
    starts_of = {pid: [entry[0] for entry in entries] for pid, entries in births.items()}

    def identity(tid, when):
        """The process (pid, birth index) thread tid belonged to at a time."""
        while True:
            index = bisect.bisect_right(starts_of.get(tid, []), when) - 1
            if index < 0:
                return (tid, -1)
            stamp, caller, thread = births[tid][index]
            if not thread:
                return (tid, index)
            tid, when = caller, stamp

    def parent_of(proc):
        pid, index = proc
        if index < 0:
            return None
        stamp, caller, _ = births[pid][index]
        return identity(caller, stamp)

    root_proc = (root, -1)
    engine_ready = False  # the traced root runs the CLI, not the wrapper
    execed = set()
    starts = []    # (parent, child, argv) for each process's first execve
    for tid, stamp, vector in execs:
        proc = identity(tid, stamp)
        # What the CLI starts after the session is settled, such as the git
        # reads of the run report, is not in the session's report.
        if settled is not None and stamp > settled:
            continue
        if proc == root_proc and vector and vector[0].rsplit("/", 1)[-1] == "putnami":
            engine_ready = True
        owner = parent_of(proc)
        # A process the wrapper started before it became the CLI is not the
        # run's, nor the execve a process makes over itself.
        if proc not in execed and proc != root_proc and (owner != root_proc or engine_ready):
            starts.append((owner, proc, vector))
        execed.add(proc)

    # The engine is the traced root: the wrapper execs the putnami binary.
    engine = root_proc
    jobs = {child for owner, child, vector in starts if owner == engine and "--putnamiContext" in vector}
    from_engine = [s for s in starts if s[0] == engine]
    from_jobs = [s for s in starts if s[0] in jobs]

    def is_git(vector):
        return bool(vector) and vector[0].rsplit("/", 1)[-1] == "git"

    git_engine = sum(1 for _, _, vector in from_engine if is_git(vector))
    git_jobs = sum(1 for _, _, vector in from_jobs if is_git(vector))
    # The shim log has no time, so it bounds the engine's git calls from above.
    shim_engine = 0
    with open(shim_log, encoding="utf-8", errors="replace") as handle:
        for line in handle:
            fields = line.split(" ", 2)
            if fields and fields[0].isdigit() and int(fields[0]) == root:
                shim_engine += 1

    orchestration = session["run"].get("orchestration") or {}
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
        ("git calls from the engine", git_engine, reported_git["engine"]),
        ("git calls from jobs", git_jobs, reported_git["jobs"]),
        ("git calls, total", git_engine + git_jobs, reported_git["engine"] + reported_git["jobs"]),
    ):
        gap = (counted - observed) / observed * 100 if observed else 0.0
        rows.append({"measure": label, "observed": observed, "reported": counted, "gapPercent": round(gap, 2)})

    kinds = Counter(vector[0].rsplit("/", 1)[-1] if vector else "?" for _, _, vector in from_engine)
    print(json.dumps({"enginePid": root, "jobProcesses": len(jobs), "rows": rows,
                      "engineGitCallsInShimLog": shim_engine,
                      "engineChildrenByProgram": kinds.most_common(12)}, indent=2))


if __name__ == "__main__":
    main()
