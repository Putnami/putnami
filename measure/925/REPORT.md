# Epic #925 — A/B measurement

What the epic "the orchestrator reads the workspace once and jobs do only their
work" changed in the CPU and time Putnami spends on this repository, with
Putnami's own CPU separated from the tools that do the work.

- **A** = `a9bf099de` (main just before the epic's first merge, 2026-11-24 13:33 UTC).
- **B (m1)** = `d7de1bd38` (origin/main on 2026-11-26 09:44 UTC, an intermediate point).
- Command, every run: `./putnamiw lint,test,build,validate --all --enforce-coverage`.
- Machine: 4 vCPU `Intel(R) Xeon(R) Processor @ 2.30GHz`, 15 GiB, Linux 6.18, cloud VM.
  The 1-minute load average was below 0.5 before every measured run, and no
  other process used 5% CPU or more (recorded per run in `summary.json`:
  `loadBefore`, `busyBefore`).

## Results (m1: A vs intermediate main)

Medians, with min–max. Cold: 2 runs per commit; small change and unchanged:
3 runs per commit; runs alternated A, B, A, B. Every run exited 0.

| Scenario | Commit | Wall | Total CPU | Putnami CPU | Tools CPU | Unattributed | Putnami share |
|---|---|---|---|---|---|---|---|
| Cold | A | 1397 s (1315–1478) | 4674 s (4390–4957) | 1324 s (1317–1330) | 2937 s (2683–3191) | 413 s | 31.2% (29.4–32.9) |
| Cold | B | 920 s (905–934) | 3526 s (3476–3577) | 241 s (240–241) | 2921 s (2871–2971) | 364 s | 7.6% (7.5–7.8) |
| Small change | A | 153 s (148–160) | 361 s (343–401) | 297 s (279–305) | 38 s (37–68) | 26 s | 88.2% (81.7–88.6) |
| Small change | B | 50 s (50–54) | 152 s (152–166) | 107 s (106–107) | 34 s (33–46) | 13 s | 75.8% (69.8–76.3) |
| Unchanged | A | 112 s (111–112) | 241 s (241–246) | 229 s (228–233) | 0.37 s | 12 s | 99.84% |
| Unchanged | B | 28 s (28–29) | 71 s (70–72) | 66 s (64–66) | 0.04 s | 6 s | 99.94% |

B against A:

| Scenario | Wall | Total CPU | Putnami CPU | Tools CPU |
|---|---|---|---|---|
| Cold | −34% | −25% | −82% (−1083 s) | −1% |
| Small change | −67% | −58% | −64% (−191 s) | −12% |
| Unchanged | −75% | −70% | −71% (−163 s) | — |

Tasks and scheduling, from the session report (`tasks[]` and
`scheduler.readyWait`):

| Scenario | Commit | Tasks | Executed | Served from local cache | Coalesced | Longest ready wait |
|---|---|---|---|---|---|---|
| Cold | A | 2237 | 1557 | 19 | 160 | 875 s |
| Cold | B | 2249 | 1564 | 19 | 161 | 525 s |
| Small change | A | 2237 | 379 | 1741 | 3 | 128 s |
| Small change | B | 2249 | 380 | 1864 | 3 | 36 s |
| Unchanged | A | 2237 | 59 | 2064 | 0 | 90 s |
| Unchanged | B | 2249 | 59 | 2190 | 0 | 17 s |

"Executed" is a task with status `success` and reuse `none`. The 59 tasks an
unchanged run executes are never cached: their tools CPU is under 0.4 s.
"Longest ready wait" is the largest `maxMs` over the commands of
`scheduler.readyWait`: the longest time a task waited for a free worker after
its dependencies were done. In a cold run on 4 workers, it mostly measures
queue depth.

### Where Putnami's CPU goes (process accounting, medians of the per-run sums)

| Scenario | Commit | `putnami` | `putnami-cloud` | `putnami-sdd` | `putnami-go` | `git` |
|---|---|---|---|---|---|---|
| Cold | A | 1075 s | 121 s | 48 s | 46 s | 21 s |
| Cold | B | 99 s | 18 s | 53 s | 40 s | 16 s |
| Small change | A | 241 s | 7 s | 17 s | 17 s | 11 s |
| Small change | B | 66 s | 0.1 s | 22 s | 10 s | 4 s |
| Unchanged | A | 214 s | 0.1 s | 0 s | 9 s | 6 s |
| Unchanged | B | 62 s | 0 s | 0 s | 0.3 s | 4 s |

Most of the gain is in the engine process (`putnami`): roughly 11× less CPU
cold, and 3.5× less for a small change or an unchanged run. The tools' CPU is
the same at A and B within the noise. That is expected: the epic changed what
Putnami does, not what the tools do.

### Cross-check against B's own report

B's `session.json` reports `run.orchestration.cpu`. A has no such breakdown.

| Scenario | Accounting: Putnami CPU | Session: `totalMs` (engine · extension · control · git) | Session: `sharePermille` | Session `runMs` / cgroup |
|---|---|---|---|---|
| Cold | 241 s | 219 s (85 · 118 · 1 · 15) | 61–63‰ | 3544 / 3577 s |
| Small change | 107 s | 113 s (65 · 39 · 1 · 8) | 690–753‰ | 151 / 152 s |
| Unchanged | 66 s | 69 s (61 · 1 · 0 · 7) | 996‰ | 70 / 71 s |

The two methods agree within 10%. Two differences are expected:

- Cold, the accounting is 22 s higher. It counts `putnami-cloud` (18 s) and the
  `putnamiw` bootstrap, which are not job processes, and every `git`, including
  the one `go build -buildvcs` runs.
- For small-change and unchanged runs, the accounting is 3–6 s lower. These runs
  are made of many short processes, and the accounting truncates each one (see
  below).

The session share is computed over `runMs`, the CLI and the children it waited
for. Detached processes and the unattributed CPU are left out of it, so the
session share is lower than the accounting share for a cold run.

## Method

- **Isolation.** Each commit has its own worktree (`git worktree add --detach`),
  installed with its own `./putnamiw install`. Each commit also has its own
  Putnami store (`PUTNAMI_STORE_DIR`) and its own Go build cache
  (`PUTNAMI_GO_CACHE_DIR`), so A and B never share a cache entry. By default,
  worktrees of one repository share a single store keyed by the git common
  directory (`tooling/cli/doc/10-caching.md`, Store Location).
- **Cold** empties the commit's store and its Go build cache, and runs
  `git clean -ffdx` in its worktree, which removes `node_modules`, `.gen`,
  `.putnami` and every build output. The measured run therefore includes the
  first-use auto-install. Left shared, identical for A and B:
  - the downloaded Go modules (immutable, so no network in a run);
  - the Bun package cache;
  - the toolchains under `~/.putnami/toolchains`;
  - the CLI binary and prepared extension runtimes under `~/.putnami/artifacts`,
    built by each worktree's install before the measurement.
- **Small change** appends ` Measurement edit N` to one doc-comment line of
  `go/framework/parallel/parallel.go`. That file is identical at A and B. Its
  library has 2 transitive dependents among 201 projects. Each run writes a text
  no earlier run saw (N = 1, 2, 3), so each run misses the same entries. The
  first small-change run follows the last cold run, and each later one follows
  the previous small-change run. Both are complete runs.
- **Unchanged** runs again after the third small-change run, with no change.
- **No remote cache.** `PUTNAMI_CACHE_TRUST=none`, the core switch that never
  starts a cache provider, is set for every run. A and B both implement it
  (`tooling/cli/internal/cli/cache_trust.go`). Without it, the install-written
  `.putnami/cache.json` makes the CLI negotiate with the remote cache. An
  unmeasured warm-up run showed `Remote cache: 0 hit · 2057 miss`, with lookups
  failing as `unreachable`. No measured run prints a remote-cache line, and none
  logged in to Putnami Cloud. `PUTNAMI_SESSION_REPORTER` is unset, so no session
  is uploaded.
- **Total CPU.** Each run executes in its own cgroup v2
  (`/sys/fs/cgroup/unified`, `cpu.stat` `usage_usec`). That cgroup counts every
  process of the run, detached ones included. The cgroup v1 `cpuacct.usage` of
  a twin cgroup agrees to the microsecond.
- **Putnami vs tools.** Each run is PID 1 of its own PID namespace
  (`unshare --pid --fork --mount-proc`), with BSD process accounting switched on
  inside that namespace. The accounting file then holds one record per process
  of the run, detached ones included, and no record of anything else on the
  machine. Each record is classified by command name:
  - Putnami: `putnami`, `putnamiw`, `putnami-*`, `git`, `git-*`;
  - harness: the namespace's init and `accton`;
  - tools: everything else (`compile`, `go`, `golangci-lint`, `node`, `bun`,
    `biome`, test binaries…).

  The share is Putnami ÷ (Putnami + tools).
- **Wall** is measured inside the namespace, around the `./putnamiw` command
  alone.
- **Detached processes.** Each run leaves a test Postgres server (6 processes)
  behind. The harness stops it once the command returns, so its CPU is recorded
  before the accounting file closes.

## What makes a number less trustworthy

1. **Unattributed CPU.** Linux records each process's CPU in 10 ms ticks,
   truncated. Cold runs start about 130,000 processes: 51,000 `git`, 19,000
   `go`, 9,000 `compile`. As a result, 8–9% of the cgroup CPU in a cold run, and
   4–8% in other runs, is in no record. The table shows it separately.
   - If all of it were Putnami's, Putnami's share would rise at most to 36–39%
     (A) and 17% (B) cold, and to 89% (A) and 78% (B) for a small change.
   - It is not all Putnami's: B's session report, which measures exactly, is
     within 10% of the accounting figure.
   - The losses are of the same size at A and B, so the deltas hold.
2. **Classification by command name.** `git` started by a tool, such as
   `go build -buildvcs`, counts as Putnami. Shell utilities that `putnamiw`
   runs before it starts the CLI, such as `sha256sum` or `find`, count as tools.
   Both are seconds, not tens of seconds.
3. **The two commits plan slightly different graphs**: 2237 tasks at A and 2249
   at B. A also skips 114 tasks in the small-change and unchanged runs, where B
   skips 2 or 0. The tools' work is the same within the noise, but it is not
   identical work.
4. **Cold-run spread at A.** The two A cold runs differ by 12% in wall time and
   CPU, and B's by 3%. Only two cold runs were made per commit.
5. **The test Postgres server is restarted by every run.** On a developer
   machine it would outlive the run. This is the same for A and B.
6. **Cloud VM.** The machine is a shared 4-vCPU VM. Steal time is not
   controlled. Runs were alternated so that drift affects A and B alike.
7. **`Longest ready wait` in a cold run** reflects the 4-worker queue, not
   dispatch latency.

## Files

- `harness.py`, `run-inner.sh`: the harness. Commands:
  - `harness.py sequence cold|small|unchanged <prefix>`;
  - `harness.py report`.
- `data/<run>/summary.json`: per-run metrics, including the per-command CPU
  table (`acct.byComm`).
- `data/<run>/acct.json.gz`: every accounting record of the run.
- `data/<run>/session.json.gz`, `run.log.gz`: the session report and the
  output of the run.
- `data/<run>/leftover.txt`: the processes still alive when the command
  returned.
- `data/results.json`, `data/table.md`: the aggregate.
