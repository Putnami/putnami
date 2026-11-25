# ADR 0003: A job keeps its own process

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/runtime` (`protocols/runtime`) and the
  extension SDK entry point (`tooling/extension-sdk/cli`)

## Context

A job is one process: `{extensionRuntime} <verb> --putnamiContext <file>`, with
its events as JSONL on stdout. The SDK entry point reads `os.Args`, ties one
event stream to one process and calls `os.Exit`, so a runtime serves one job.

The option under review was a job worker: a reserved verb
`__putnami job-worker` that reads one request per line (a correlation id and
the path of a context document), frames its events by that id, runs one job at
a time, and exits at the end of stdin; the engine would keep a pool of workers
per extension. It removes one thing per job: the start of the runtime process.

## Measurement

Cost of a process start. This program runs a runtime N times on a context
with no project, times each start and reads the CPU of its children:

```python
import os, resource, statistics, subprocess, sys, time
n, cmd = int(sys.argv[1]), sys.argv[2:]
env = {**os.environ, "PUTNAMI_RUNTIME_EVENTS": "2"}
walls, before = [], resource.getrusage(resource.RUSAGE_CHILDREN)
for _ in range(n):
    t = time.perf_counter()
    subprocess.run(cmd, stdout=subprocess.DEVNULL, env=env)
    walls.append((time.perf_counter() - t) * 1000)
after = resource.getrusage(resource.RUSAGE_CHILDREN)
cpu = (after.ru_utime - before.ru_utime
       + after.ru_stime - before.ru_stime) * 1000 / n
walls.sort()
print(f"median {statistics.median(walls):.2f} ms,",
      f"p90 {walls[int(n * 0.9)]:.2f} ms, cpu {cpu:.2f} ms")
```

It runs as `python3 bench.py 300 "$RUNTIME" build --putnamiContext ctx.json`,
with `ctx.json` holding `{"project":{"name":""},"workspace":{"root":"<repo>"}}`;
the four-stream variant starts four such commands at once with 200 runs each.
`$RUNTIME` is `runtime/compiled/putnami-go` or `putnami-ts` of the extension
artifact `./putnamiw` prepares. `build` is no lifecycle verb of either, and a
context with no project answers `SKIP`. Each start does exec, Go runtime
initialization, context parsing and one `result` line, which is what a worker
would no longer repeat. Commit `e1b76bac`, a 4-core Linux 6.18 container
(Intel Xeon @ 2.10 GHz, no cgroup quota), on 2026-11-25:

- `putnami-go`, idle machine, two samples: wall median 12.0 to 12.2 ms (p90
  13.8 to 14.7 ms), CPU 15.9 to 16.3 ms per start.
- `putnami-go`, four streams at once: wall median 14.8 to 15.3 ms, CPU 12.5
  to 13.2 ms per start.
- `putnami-ts`, idle machine: wall median 10.9 ms, CPU 14.8 ms per start.
- `/bin/true`, as the floor: wall 1.5 ms, CPU 1.1 ms.

Job processes per run. A no-cache gate of this workspace,
`./putnamiw lint,test,build,validate --all --no-cache --enforce-coverage
--trace-profile trace.json`, ran on 2026-11-24 at commit `e62e30e8`, whose
orchestration report reached `main` in `1475337f`, on a 4-core Linux container
(Intel Xeon @ 2.10 GHz, no cgroup quota). Its numbers are read from
`run.orchestration` of the session's `session.json` (session
`20261124-165832-efacce`, the run that
[CLI ADR 0101](../../../../tooling/cli/doc/adr/0101-the-engine-materializes-a-native-registry-credential-once-per-origin.md)
cites): 1,747 job and 58 batch processes, 1,805 runtime starts. The run spent
4,250,872 ms of CPU, 859,166 ms of it orchestration, over 1,293,843 ms of wall
time.

Share. A worker removes at most 1,805 × 16.3 ms = 29.4 s of CPU, with the
highest idle sample: 0.69 % of the run's CPU and 3.4 % of its orchestration
CPU. On the wall clock, with the highest contended median, 15.3 ms, it removes
at most 1,805 × 15.3 ms = 27.6 s, 2.1 % of the run, if no start overlapped
another; with the four slots of that machine busy, about 6.9 s, 0.53 %. The
extension processes cost 160,345 ms without their children: 97 ms per process
over the 1,649 that reported their CPU, 156 reporting none. The rest of it is
the job's own work, which a worker keeps.

Not measured: macOS and Windows, for want of a host. A process start is
expected to cost more there than on Linux, on Windows above all. A
measurement on either that puts the runtime starts of a gate above the
threshold below reopens this decision.

## Decision

**A job keeps its own process.** The runtime protocol has no worker verb and
no request framing, and the SDK entry point keeps one job per process. A
worker is worth its protocol, its pool and its per-job isolation only when
runtime starts exceed 5 % of a gate's CPU, or 5 % of its wall time by the
serial bound, on a measured platform; on this workspace on Linux they are
under 1 % of its CPU and under 3 % of its wall time.

## Rejected alternatives

- **A job worker per extension, one job at a time, in a pool.** It removes
  under 1 % of the run's CPU and under 3 % of its wall time, and it adds a
  framed request protocol, a manifest capability and handshake, pool lifecycle
  in the engine, and an SDK that must restore the environment, the working
  directory and the resource accounting between jobs.
- **A worker that runs jobs concurrently.** It saves no more starts and loses
  the per-job environment, working directory and resource accounting.

## Consequences

- A stream stays one job's process. The version that
  [ADR 0001](0001-one-stream-one-negotiated-version.md) negotiates through the
  process environment, which a long-lived worker would share across its jobs,
  keeps belonging to one job.
- The orchestration cost of that run lies elsewhere: the CPU of the engine
  (235,316 ms) and of the CLI starts made by jobs (406,192 ms), and the wall
  time of key computation summed over the tasks (181,661 ms), each exceed every
  runtime start of the run.
