# Epic #925 — A/B measurement

What the epic "the orchestrator reads the workspace once and jobs do only their
work" changed in the CPU and time Putnami spends on this repository, with
Putnami's own CPU separated from the tools that do the work.

- **A** = `a9bf099de` (main just before the epic's first merge, 2026-11-24 13:33 UTC).
- **B** = `d7de1bd38` (origin/main on 2026-11-26 09:44 UTC): an intermediate point, measured in series m1.
- **B2** = `e044b0c6c` (origin/main on 2026-11-26 13:53 UTC): the final main, measured in series m2.
  B2 adds GOGC=200 by default in the CLI binary. No run exports GOGC.
- Command, every run: `./putnamiw lint,test,build,validate --all --enforce-coverage`.
- Machine: 4 vCPU `Intel(R) Xeon(R) Processor @ 2.30GHz`, 15 GiB, Linux 6.18, cloud VM.
  - The 1-minute load average was below 0.5 before every measured run, and no
    other process used 5% CPU or more (recorded per run in `summary.json`:
    `loadBefore`, `busyBefore`).
  - The VM rebooted between m1 and m2. It kept the same CPU model, core count and disk.
- Each series alternates A with the B it measures, on the same machine. m1 is
  A against B; m2 is A against B2. A was measured again in m2, so the B2 deltas
  are against A runs made the same afternoon.

Medians, with min–max. Cold: 2 runs per commit; small change and unchanged: 3
runs per commit. Every run exited 0. No run failed, and none was rerun.

## Final result: A vs B2 (m2)

| Scenario | Commit | Wall | Total CPU | Putnami CPU | Tools CPU | Unattributed | Putnami share |
|---|---|---|---|---|---|---|---|
| Cold | A | 1330 s (1323–1336) | 4412 s (4393–4431) | 1326 s (1318–1335) | 2677 s (2667–2687) | 409 s | 33.1% (33.1–33.2) |
| Cold | B2 | 921 s (906–936) | 3520 s (3469–3570) | 206 s (205–207) | 2950 s (2899–3002) | 363 s | 6.5% (6.4–6.7) |
| Small change | A | 159 s (153–167) | 377 s (355–403) | 311 s (292–314) | 37 s (36–61) | 28 s | 89.0% (83.8–89.3) |
| Small change | B2 | 44 s (44–46) | 124 s (124–131) | 78 s (73–79) | 36 s (35–48) | 10 s | 68.8% (60.4–69.1) |
| Unchanged | A | 113 s (110–119) | 246 s (241–260) | 232 s (228–246) | 0.36 s | 13 s | 99.85% |
| Unchanged | B2 | 20 s (20–22) | 37 s (36–37) | 34 s (34–35) | 0.03 s | 3 s | 99.91% |

B2 against A:

| Scenario | Wall | Total CPU | Putnami CPU | Tools CPU |
|---|---|---|---|---|
| Cold | −31% | −20% | −84% (−1120 s) | +10% (see caveat 2) |
| Small change | −72% | −67% | −75% (−233 s) | −5% |
| Unchanged | −82% | −85% | −85% (−198 s) | — |

### Engine peak RSS and load (m2)

The engine is the CLI process that `putnamiw` execs into. Its `VmHWM` was
sampled every second from outside the run. The 1-minute load average was
sampled every 5 s through each run (`samples` in `summary.json`).

| Scenario | Commit | Engine peak RSS (VmHWM) | Load max | Load max ÷ nproc | Load median ÷ nproc |
|---|---|---|---|---|---|
| Cold | A | 250 MiB (238–262) | 27.4 (27.3–27.6) | 6.86 | 3.12 (3.09–3.16) |
| Cold | B2 | 564 MiB (547–581) | 31.4 (30.5–32.3) | 7.85 | 4.74 (4.63–4.84) |
| Small change | A | 283 MiB (282–296) | 3.4 | 0.85 | 0.59 |
| Small change | B2 | 595 MiB (583–610) | 2.7 | 0.66 | 0.36 |
| Unchanged | A | 295 MiB (288–300) | 2.3 | 0.58 | 0.36 |
| Unchanged | B2 | 722 MiB (720–783) | 0.9 | 0.22 | 0.14 |

- **Peak RSS:** the engine's peak RSS is 2.2–2.4× A's, in every scenario.
  This is expected from GOGC=200, which lets the heap grow to twice the live
  set before collecting.
- **Cold-run load:** a cold run stays far above 1.5× the cores, at A and at B2.
  - The median load is 3.1× nproc at A and 4.7× at B2. The peak is 6.9× and
    7.9×.
  - B2 finishes the same tool work in less wall time, so its load is higher.
  - The load counts runnable and uninterruptible tasks: Go compilers and test
    binaries run many threads each. Putnami admits subprocesses by CPU ceiling,
    not by load.
  - The epic's open question, whether a cold run's load stays within 1.5× the
    cores, is answered **no** on this machine, for both commits.

### Tasks and scheduling (m2)

| Scenario | Commit | Tasks | Executed | Served from local cache | Coalesced | Longest ready wait |
|---|---|---|---|---|---|---|
| Cold | A | 2237 | 1557 | 19 | 160 | 861 s |
| Cold | B2 | 2249 | 1564 | 19 | 161 | 532 s |
| Small change | A | 2237 | 379 | 1741 | 3 | 133 s |
| Small change | B2 | 2249 | 380 | 1864 | 3 | 30 s |
| Unchanged | A | 2237 | 59 | 2064 | 0 | 91 s |
| Unchanged | B2 | 2249 | 59 | 2190 | 0 | 9 s |

- **Executed** is a task with status `success` and reuse `none`. The 59 tasks
  an unchanged run executes are never cached; their tools CPU is under 0.4 s.
- **Longest ready wait** is the largest `maxMs` over the commands of
  `scheduler.readyWait`: the longest a task waited for a free worker after its
  dependencies were done. In a cold run on 4 workers it mostly measures queue
  depth.

### Where Putnami's CPU goes (process accounting, medians of per-run sums)

| Scenario | Commit | `putnami` | `putnami-cloud` | `putnami-sdd` | `putnami-go` | `git` |
|---|---|---|---|---|---|---|
| Cold | A (m2) | 1072 s | 124 s | 48 s | 48 s | 20 s |
| Cold | B (m1) | 99 s | 18 s | 53 s | 40 s | 16 s |
| Cold | B2 | 64 s | 18 s | 56 s | 42 s | 11 s |
| Small change | A (m2) | 253 s | 7 s | 18 s | 17 s | 11 s |
| Small change | B (m1) | 66 s | 0.1 s | 22 s | 10 s | 4 s |
| Small change | B2 | 37 s | 0.1 s | 24 s | 11 s | 2 s |
| Unchanged | A (m2) | 217 s | 0.1 s | 0 s | 9 s | 6 s |
| Unchanged | B (m1) | 62 s | 0 s | 0 s | 0.3 s | 4 s |
| Unchanged | B2 | 33 s | 0 s | 0 s | 0.3 s | 2 s |

- **The engine process carries the gain.** From A to B2, `putnami` drops 17×
  cold, 7× for a small change and 7× for an unchanged run.
- **From B to B2, the engine halves again.** Cold goes from 99 s to 64 s;
  small change and unchanged roughly halve (66 → 37 s, 62 → 33 s).
- **What remains.** At B2, extension processes such as `putnami-sdd` and
  `putnami-go` are now the larger part of Putnami's CPU in cold and
  small-change runs.

### Cross-check against B2's own report

| Scenario | Accounting: Putnami CPU | Session `totalMs` (engine · extension · control · git) | Session `sharePermille` | Session `runMs` / cgroup |
|---|---|---|---|---|
| Cold | 206 s | 183 s (52 · 122 · 1 · 9) | 51–53‰ | 3487 / 3520 s |
| Small change | 78 s | 83 s (36 · 42 · 1 · 4) | 598–679‰ | 123 / 124 s |
| Unchanged | 34 s | 35 s (32 · 1 · 0 · 2) | 992‰ | 36 / 37 s |

The two methods agree within 13%. The differences are expected:

- **Cold, the accounting is 23 s higher.** It counts `putnami-cloud` (18 s)
  and the `putnamiw` bootstrap, which are not job processes. It also counts
  every `git`, including the one `go build -buildvcs` runs.
- **Small change and unchanged, the accounting is 1–5 s lower.** It truncates
  each short process (caveat 1).
- **The session share is lower cold.** It is taken over `runMs`, which leaves
  out detached processes and the unattributed CPU.

## Intermediate point: A vs B (m1)

| Scenario | Commit | Wall | Total CPU | Putnami CPU | Tools CPU | Unattributed | Putnami share |
|---|---|---|---|---|---|---|---|
| Cold | A | 1397 s (1315–1478) | 4674 s (4390–4957) | 1324 s (1317–1330) | 2937 s (2683–3191) | 413 s | 31.2% (29.4–32.9) |
| Cold | B | 920 s (905–934) | 3526 s (3476–3577) | 241 s (240–241) | 2921 s (2871–2971) | 364 s | 7.6% (7.5–7.8) |
| Small change | A | 153 s (148–160) | 361 s (343–401) | 297 s (279–305) | 38 s (37–68) | 26 s | 88.2% (81.7–88.6) |
| Small change | B | 50 s (50–54) | 152 s (152–166) | 107 s (106–107) | 34 s (33–46) | 13 s | 75.8% (69.8–76.3) |
| Unchanged | A | 112 s (111–112) | 241 s (241–246) | 229 s (228–233) | 0.37 s | 12 s | 99.84% |
| Unchanged | B | 28 s (28–29) | 71 s (70–72) | 66 s (64–66) | 0.04 s | 6 s | 99.94% |

B against A: Putnami CPU −82% cold, −64% small change, −71% unchanged; wall
−34%, −67%, −75%.

B's session `run.orchestration.cpu`:

| Scenario | `totalMs` (engine · extension · control · git) | Accounting: Putnami CPU |
|---|---|---|
| Cold | 219 s (85 · 118 · 1 · 15) | 241 s |
| Small change | 113 s | 107 s |
| Unchanged | 69 s | 66 s |

A's figures in m1 and m2 agree:

- Putnami CPU: 1324 vs 1326 s cold, 297 vs 311 s small change, 229 vs 232 s
  unchanged.
- The machine reboot between the series did not move the baseline.

## Method

- **Isolation.** Each commit has its own worktree (`git worktree add --detach`),
  installed with its own `./putnamiw install`. Each commit also has its own
  Putnami store (`PUTNAMI_STORE_DIR`) and its own Go build cache
  (`PUTNAMI_GO_CACHE_DIR`), so two commits never share a cache entry. By
  default, worktrees of one repository share a single store keyed by the git
  common directory (`tooling/cli/doc/10-caching.md`, Store Location).
- **Cold** empties the commit's store and its Go build cache, and runs
  `git clean -ffdx` in its worktree. That removes `node_modules`, `.gen`,
  `.putnami` and every build output, so the measured run includes the
  first-use auto-install. Shared and left in place:
  - the downloaded Go modules, which are immutable, so no run needs the network;
  - the Bun package cache;
  - the toolchains under `~/.putnami/toolchains`;
  - the CLI binary and the prepared extension runtimes under
    `~/.putnami/artifacts`, built by each worktree's install before the
    measurement;
  - the third-party tool caches under `~/.cache` (see caveat 2).
- **Small change** appends ` Measurement edit N` to one doc-comment line of
  `go/framework/parallel/parallel.go`.
  - The file is identical at A, B and B2. Its library has 2 transitive
    dependents among 201 projects.
  - Each run writes a text no earlier run saw (N = 1, 2, 3), so each run misses
    the same entries.
  - The first small-change run follows the last cold run, and each later one
    follows the previous small-change run. Both are complete runs.
- **Unchanged** runs again after the third small-change run, with no change.
- **No remote cache.** `PUTNAMI_CACHE_TRUST=none`, the core switch that never
  starts a cache provider, is set for every run.
  - A, B and B2 all implement it (`tooling/cli/internal/cli/cache_trust.go`).
  - Without it, the `.putnami/cache.json` that install writes makes the CLI
    negotiate with the remote cache. An unmeasured warm-up run showed
    `Remote cache: 0 hit · 2057 miss`, with lookups failing as `unreachable`.
  - No measured run prints a remote-cache line, and none logged in to Putnami
    Cloud.
  - `PUTNAMI_SESSION_REPORTER` is unset, so no session is uploaded. `GOGC` is
    unset, so each binary keeps its own GC default.
- **Total CPU.** Each run executes in its own cgroup v2
  (`/sys/fs/cgroup/unified`, `cpu.stat` `usage_usec`). The cgroup counts every
  process of the run, detached ones included. The cgroup v1 `cpuacct.usage` of
  a twin cgroup agrees to the microsecond.
- **Putnami vs tools.** Each run is PID 1 of its own PID namespace
  (`unshare --pid --fork --mount-proc`), with BSD process accounting switched on
  inside that namespace.
  - The accounting file then holds one record per process of the run, detached
    ones included, and no record of anything else on the machine.
  - Records are classified by command name:
    - **Putnami:** `putnami`, `putnamiw`, `putnami-*`, `git`, `git-*`.
    - **Harness:** the namespace's init and `accton`.
    - **Tools:** everything else (`compile`, `go`, `golangci-lint`, `node`,
      `bun`, `biome`, test binaries…).
  - The share is Putnami ÷ (Putnami + tools).
- **Wall** is measured inside the namespace, around the `./putnamiw` command
  alone.
- **Samples.** The engine's peak RSS and the load average are read from outside
  the run's cgroup, so sampling costs the run nothing.
- **Detached processes.** Each run leaves a test Postgres server (6 processes)
  behind. The harness stops it once the command returns, so its CPU is recorded
  before the accounting file closes.

## What makes a number less trustworthy

1. **Unattributed CPU.** Linux records each process's CPU in 10 ms ticks,
   truncated. A cold run starts about 130,000 processes: 51,000 `git`, 19,000
   `go`, 9,000 `compile`.
   - As a result, 8–10% of the cgroup CPU of a cold run, and 4–8% of other
     runs, is in no record. The tables show it apart.
   - If all of it were Putnami's, Putnami's cold share would rise at most to 39%
     at A and 16% at B2. It would rise to 90% (A) and 71% (B2) for a small
     change.
   - It is not all Putnami's: B2's session report, which measures exactly, is
     within 13% of the accounting.
2. **Third-party tool caches are not emptied by a cold run.** `golangci-lint`
   and `staticcheck` keep content-keyed caches under `~/.cache`, outside
   Putnami's store.
   - The first cold run of sources that no earlier run saw pays about 330 s more
     `golangci-lint` CPU, and up to 100 s more `staticcheck` CPU.
   - **m2:** the first cold pair (`m2-cold1-A`, `m2-cold1-B2`) is kept in
     `data/` but excluded from the medians, because B2's sources were new. An
     extra pair (`m2-cold3-*`), run in the same alternation, replaces it.
     - The excluded B2 run measured 1048 s wall, 4002 s CPU, 207 s Putnami and
       3396 s tools.
     - The excluded A run measured 1329 s, 4405 s, 1324 s and 2678 s.
   - **m1:** B's lint caches were warmed by the unmeasured warm-up run. A's
     first cold run (`m1-cold1-A`) paid them, which explains the 12% spread of
     A's m1 cold runs.
   - **Residual:** `staticcheck` remains warmer for A, whose sources ran more
     often, than for B2: 34 s against 108 s in the last cold pair. This accounts
     for part of B2's higher cold tools CPU.
   - **Code growth:** B2's sources also carry more code to build and test than
     A, for example a new `orchestration.test` (29 s).
   - **Scope:** this caveat moves the tools column, never Putnami's.
3. **Classification by command name.** `git` that a tool starts, such as
   `go build -buildvcs`, counts as Putnami. Shell utilities that `putnamiw`
   runs before it starts the CLI, such as `sha256sum` or `find`, count as tools.
   Both amount to seconds.
4. **The commits plan slightly different graphs:** 2237 tasks at A and 2249 at
   B and B2. A also skips 114 tasks in small-change and unchanged runs, where B
   and B2 skip 2 or 0.
5. **Peak RSS is sampled every second.** VmHWM only grows, so the reported peak
   misses at most the last second before exit.
6. **The test Postgres server is restarted by every run.** On a developer
   machine it would outlive the run. This is the same for every commit.
7. **Cloud VM.** The machine is a shared 4-vCPU VM, and steal time is not
   controlled. Runs alternate so that drift affects both sides alike.
   - After the reboot before m2, the toolchains, module cache, artifacts and
     both worktrees were read once into the page cache.
8. **`Longest ready wait` in a cold run** reflects the 4-worker queue, not
   dispatch latency.

## Files

- `harness.py` and `run-inner.sh` are the harness. Its commands:
  - `harness.py sequence cold|small|unchanged <prefix>` for m1, and
    `cold-b2|small-b2|unchanged-b2` for m2;
  - `harness.py run <A|B|B2> <scenario> <label>`;
  - `harness.py report`.
- `data/<run>/summary.json` holds the per-run metrics:
  - `acct.byComm`, the CPU per command;
  - `samples`, the engine VmHWM and the load samples;
  - `excluded`, the reason a run is left out of the medians.
- `data/<run>/acct.json.gz` holds every accounting record of the run.
- `data/<run>/session.json.gz` and `run.log.gz` hold the session report and
  the output of the run.
- `data/<run>/leftover.txt` lists the processes alive when the command
  returned.
- `data/results.json` and `data/table.md` hold the aggregate.
