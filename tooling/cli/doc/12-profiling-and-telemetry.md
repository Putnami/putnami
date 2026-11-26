# Profiling and Telemetry

## Profiling

The CLI supports performance profiling via the Chrome Trace Event Format. This produces a JSON file that can be visualized in `chrome://tracing` or compatible tools (Perfetto, Speedscope).

### Usage

```bash
putnami build --all --trace-profile trace.json
```

This generates `trace.json` alongside normal execution. The file contains timing data for every job and phase.

> `--profile` selects the deployment profile (`dev`/`test`/`production`).
> Passing a path to `--profile` is a hard usage error that points you at
> `--trace-profile`.

`--trace-profile` also prints a scheduler tuning report to the console — the selected parallelism, the critical path (longest dependency chain by duration), and ready wait per command — so common questions can be answered without loading the trace or running external analysis scripts. The same report is available in `--debug` runs and is persisted to each session's `session.json` under `scheduler`.

### What Gets Profiled

| Event | Category | Description |
|-------|----------|-------------|
| Job execution | `job` | Start and end of each job subprocess, with status and duration |
| Phases | `phase` | Named phases within jobs (e.g., `compile`, `link`, `test`) |
| Cache hits | `cache` | Instant events marking cache hits |
| Coalesced misses | `cache` | `cache-coalesced` instant events marking work published by a sibling process |
| Errors | `error` | Instant events for failures |

### Chrome Trace Format

The output follows the Chrome Trace Event Format (JSON Array):

```json
[
  {"name":"my-lib:build~generate","cat":"job","ph":"B","ts":0,"pid":1,"tid":1,"args":{}},
  {"name":"compile","cat":"phase","ph":"B","ts":50000,"pid":1,"tid":1},
  {"name":"compile","cat":"phase","ph":"E","ts":300000,"pid":1,"tid":1},
  {"name":"my-lib:build~generate","cat":"job","ph":"E","ts":800000,"pid":1,"tid":1,"args":{"status":"success","duration":"800ms"}},
  {"name":"cache-hit","cat":"cache","ph":"i","ts":100000,"pid":1,"tid":2,"s":"t","args":{"job":"my-app:build~generate"}},
  {"name":"process_name","ph":"M","pid":1,"args":{"name":"putnami"}},
  {"name":"thread_name","ph":"M","pid":1,"tid":1,"args":{"name":"my-lib:build~generate"}}
]
```

### Phase Types

| Phase | Code | Description |
|-------|------|-------------|
| Begin | `B` | Start of a duration event |
| End | `E` | End of a duration event |
| Complete | `X` | Duration event with `dur` field |
| Instant | `i` / `I` | Point-in-time event |
| Metadata | `M` | Process/thread naming |

### Thread Assignment

Each job gets a unique thread ID for clear parallel visualization. Thread IDs are stable per job key — the same job always appears on the same trace lane. Metadata events provide human-readable thread names.

### Viewing Traces

1. Open `chrome://tracing` in Chrome.
2. Click "Load" and select the trace JSON file.
3. Use the timeline to zoom, pan, and inspect events.
4. Click events to see detailed metadata (status, duration, args).

Alternative: open the trace in [Perfetto UI](https://ui.perfetto.dev/).

### Analyzing Performance

Common patterns to look for:

- **Long critical path** — The longest chain of sequential dependencies determines minimum build time.
- **Idle workers** — Gaps between jobs indicate insufficient parallelism or unbalanced pipelines.
- **Cache effectiveness** — Instant `cache-hit` events show skipped work. Few hits suggest cache key issues.
- **Cross-process deduplication** — `cache-coalesced` events and job-end status `coalesced` show cold misses computed by a sibling process instead of duplicated locally.
- **Slow phases** — Drill into job phases to find bottlenecks (compilation, type-checking, linking).

## Per-task Fixed-cost Instrumentation

Every terminal job has a compact row in `session.json` under `jobs`. The same
rows are returned by `putnami sessions inspect <id> --output=jsonl` at
`metadata.jobs`; text inspection renders the task kind, task wall, and startup
overhead without requiring callers to reconstruct them from the event stream.

| Field | Unit | Meaning |
|---|---:|---|
| `jobs[].taskKind` | label | Underlying extension task (`lint-format`, `build-generate`, `test-run`, etc.), stable across projects that run the same task. |
| `jobs[].taskWallMs` | ms wall | Scheduler wall around the whole task: lookup/restore/coalescing, hooks, every subprocess attempt, and result storage/publication. This is not startup overhead or CPU time. |
| `jobs[].spawnToFirstEventMs` | ms wall | Immediately before subprocess `Start` through parsing its first valid runtime JSONL event, before path mapping or renderer work. Measures fork/exec, wrapper, runtime startup, and extension initialization before useful protocol output. |

`spawnToFirstEventMs` is omitted when the job did not produce a first event:
cache hits and coalesced results do not spawn, and an eventless or pre-spawn
failure has no latency to report. Zero is therefore a real sub-millisecond
observation after millisecond rounding, not the missing-value sentinel.
Instrumentation uses one monotonic timestamp at spawn and one at the first
event; it performs no per-event work after that first observation. When retries
occur, the row retains the first attempt that emitted a valid event, even if a
later attempt is eventless; `taskWallMs` covers all attempts. Every planned job
gets exactly one terminal row, including drained, stuck-DAG, and never-started
cancellations.

## Orchestration Cost

A run reports where its orchestration time and CPU went, in `session.json` and
in its `--output=json` result under `run.orchestration`. The report is
information, in any workspace: it fails nothing.

**Orchestration** is the CPU a run spends on anything but the tools its jobs
run:

- the CLI process that runs the engine, its own CPU;
- each extension process without its children: a job built on the extension
  SDK reports its own CPU in its result (`processReport.selfCpuMs`), apart from
  the CPU of the processes it started;
- the control processes: an extension process the engine starts for anything
  but a job, such as a runtime handshake, the workspace probe, a hook or a
  provider, with the descendants it waited for;
- the CLI starts made by jobs, such as a registry credential;
- git, started by the engine or by an SDK job.

The tools a job runs, `go`, `bun`, `tsc` and what they start, are not
orchestration, and neither is the git a tool runs itself, such as
`go build -buildvcs`.

| Member | Meaning |
|---|---|
| `run.orchestration.phases[]` | Wall of each engine phase the run entered: `workspace-load`, `runtime-sync`, `probe`, `selection`, `plan`. |
| `run.orchestration.processes[]` | Processes started by `origin` (`engine`, or `jobs` built on the SDK) and `kind` (`job`, `batch`, `control`, `cli`, `git`, `other`), with the CPU of those that exited before the run ended. |
| `run.orchestration.git[]` | Git calls by origin and subcommand (`rev-parse`, `ls-files`). |
| `run.orchestration.cpu` | `engineMs`, `extensionMs`, `controlMs`, `cliMs`, `gitMs`, their sum `totalMs`, the run's whole CPU `runMs` (the CLI process and every child it waited for), `sharePermille` (`totalMs` over `runMs`, in thousandths, present exactly when `runMs` is above 0) and `perTaskMs` (`totalMs` over the selected tasks). `unreportedProcesses` counts the job processes that reported no CPU of their own: a runtime not built on the SDK or on a version of it that reports, an interactive job, or an SDK job that exited before writing its result. |
| `run.orchestration.capacity` | `cpus` (the CPU capacity admission charges ceilings against), `workers`, and `samples[]` every `intervalMs` from the run's start to its end: `grantedCpus`, the sum of the CPU ceilings of the subprocesses running (serve steps aside, since they reserve no CPU), and `loadCenti`, the one-minute load average times 100 (absent where the platform reports none). `peakGrantedCpus` and `peakLoadCenti` are the peaks; a long run keeps at most 512 samples by keeping every other one and doubling `intervalMs`. The load is the machine's, so it counts what else runs there. |
| `tasks[].orchestration` | Per task: `keyMs` (computing its cache key), `lockWaitMs` (waiting for a cache lease another process held, or for its task-output locks) and `restoreMs` (restoring a cache entry; for a remote entry, its lookup and the wait for the remote lease too). A step the task never entered is absent. |

The counts come from the processes themselves rather than from an outside
poller: the engine and the SDK start every process through one ledger
(`go.putnami.dev/sdk/extension/procstat`), and an SDK job writes its ledger in
its result. The first run of a CLI process covers the process since it started,
its bootstrap included; a later run in the same process, as `cache verify` and
an impact plan start, covers its own share only. Process CPU is measured where the platform reports
it (`getrusage` on Unix); elsewhere `cpu` is absent. A long-lived process still
running when the run ends, such as a cache provider, is counted but its CPU is
not. The `sysctl` probes the CLI runs on macOS to read the machine go around
the ledger and are not counted.

`--trace-profile`, and `--debug`, print the report after the scheduler summary:

```text
  Orchestration CPU: 23s of 1m35s (24.4%) · 1s per task (engine 14s · extensions 425ms · control 2s · cli 4s · git 466ms)
  Engine phases: workspace-load 49ms · runtime-sync 15s · probe 3s · selection 4ms · plan 14ms
  Processes (engine): job 12 · control 56 · git 35 · other 65
    git: diff 1 · ls-files 12 · rev-parse 18 · show 1 · status 2 · symbolic-ref 1
  Processes (jobs): cli 7 · git 1 · other 12
    git: ls-files 1
```

### Checking the counts

`tooling/cli/scripts/check-orchestration-counts.sh <putnami arguments>` runs
`./putnamiw` under `strace -f -ttt`, with a `git` shim first on `PATH` that logs each
call and runs the real git, and prints the counts each side saw:

```bash
tooling/cli/scripts/check-orchestration-counts.sh lint,test,build --projects /protocols/runtime --no-cache
```

From the trace it counts the processes whose first `execve` happens while their
parent is the CLI process or a job process (a child of the CLI started with
`--putnamiContext`), and compares them with `processes[]`; those whose
program is `git` it compares with `git[]`, and the shim log bounds the engine's
git calls from above. A process is a pid and the clone that returned it, since
a long run reuses pids. A process a tool started is in neither. The processes the CLI starts after the
session is settled, such as the git reads of the run report, are left out by
their trace time.

### Garbage collection

Every `putnami` process, the engine and the ones it starts, collects garbage at
200 percent (`debug.SetGCPercent`) unless `GOGC` is set, which then decides as
it does for any Go program; `GOMEMLIMIT` keeps its meaning. Other programs the
engine starts are not affected, but a `GOGC` a user exports reaches every Go
program among them. On unchanged runs of `lint,test,build,validate --all` of
this repository, against Go's default of 100, the engine's CPU fell from 35.8 s
to 31.3 s and its peak resident memory rose from 474 MB to 742 MB. A machine
short of memory sets `GOGC=100`, or a `GOMEMLIMIT`. See
[ADR 0111](adr/0111-the-cli-collects-garbage-at-200-percent.md), which gives
the commits, the runs and where each number was read.

## Orchestration Counts

The tests in `tooling/cli/internal/cli/e2e/orchestration` hold the orchestration
counts of this repository under ceilings. They are tests of this repository: they run
when Putnami changes, never in a consumer workspace, and no consumer pull request
fails because of them. Wall times are not tested; they are read from the tooling
bench.

Each test runs the real CLI in a fixture workspace and reads the counts from its
`run.orchestration` report (`clitest.RunCounts`). The engine's fixture
(`clitest.CountsFixture`) runs trivial `/bin/sh` tasks, so what it starts is the
engine's alone. Its writing variant (`clitest.WritingCountsFixture`) keys each
task on its project's Git candidates and writes a report under the ignored
`.putnami`, so its git calls show that such a write lists no candidate again
([ADR 0106](adr/0106-a-write-under-an-excluded-putnami-keeps-the-candidate-digests.md)).
Each extension's fixture loads the extension from this tree with
one small project it serves, and runs on one worker from no task history
(`clitest.ForgetTaskHistory`), so its tasks run in the same order every time:
with a history, ready tasks dispatch longest first by the durations earlier
runs measured under the machine's load. The Go and TypeScript fixtures declare this repository's
registries: the engine, or a job that resolves a registry credential itself,
starts the CLI of the run, which answers with no token, so the start is counted
and nothing leaves the host. The Go fixture's engine materializes the module
origin's credential once for the run (`engine cli 1`), and its jobs start no CLI
([ADR 0101](adr/0101-the-engine-materializes-a-native-registry-credential-once-per-origin.md)).

The counts are the same on a developer's machine, in a Putnami job and in CI:

- Each test unsets every variable an outer run may set, each `PUTNAMI_*`
  variable but `PUTNAMI_HOME`, `CI` and the `GITHUB_*` variables
  (`clitest.UnsetHostRunEnvironment`). The extension cases set the values an
  outer run would first, so a variable the helper misses fails them.
- The engine probes each candidate of a pinned toolchain until one answers
  with the pinned release, and each probe is an `other` process. An extension
  case sets `GOROOT` to the Go release this repository builds with and puts its
  pinned tools first on `PATH`, so the first candidate answers whatever else
  the host's `PATH` holds. The cases run in a Putnami home of their own,
  with empty `toolchains`, whose artifact store keeps the runs' records. The
  machine's pinned Go and Bun stay where it keeps them: each names its own
  executable through its probe's `self`, so it answers from a record once
  probed, and the runs a case counts after its first run probe nothing
  ([ADR 0105](adr/0105-a-toolchain-that-names-its-own-executable-is-recorded.md)).
  A dispatcher, which names another file, is probed once per run, so a case
  is skipped when the host's Go or Bun is a script or a hard link.
- Nothing leaves the host. A case is skipped when a program it runs is not on
  `PATH`, when a path of this tree it reads is not installed, or when a tool its
  extensions would otherwise download is neither where they look for it first
  nor under the machine's Putnami home: Go's lint tools, Rust's
  `cargo-nextest` and `cargo-llvm-cov` (on `PATH` first, as `@putnami/rust`
  looks for them), and the pinned Bun (the `bun` on `PATH` when it is that
  release). The integrations read the graph export their tool writes in CI
  (`bazel-graph.json`, `pants-graph.json`, `moon-graph.json`) or the
  manifests, so no case runs Bazel, Pants, moon, Turborepo, Nx or Rush.

Where the cases run: Contributor CI provisions Go (`actions/setup-go`), the
pinned Bun (`oven-sh/setup-bun`), uv (`astral-sh/setup-uv`), from its
restored cache the Go lint tools under `~/.putnami/tools/go`, and the
toolchains `putnami.lock.json` pins, which `putnami install` installs; it
fetches no language package offline. The gate selects these tests when a pull
request impacts `@putnami/cli`; then the engine tests run, and so do the sdd,
cloud, shell (`bash` and `make` of the runner), Bazel, Pants, moon,
Turborepo, Nx and Rush cases; the Go case runs when the restored tool cache
holds `~/.putnami/tools/go`; the TypeScript, clientgen and web cases run once
the gate's install has provisioned the pinned Bun and the repository's
`node_modules`; the Rust case is skipped, since that workflow installs no Rust
toolchain. It runs where `cargo` and the Rust test tools are on the machine.

Every extension built here is either the subject of a case, by its
`extensions`, or listed in `skippedExtensions` with the reason it holds no
count; `TestEveryExtensionBuiltHereIsCounted` walks the `putnami.extension.json`
files of this tree and fails on an extension in neither list. The cases count
Go, TypeScript, sdd, clientgen,
Rust, `@putnami/application` and `@putnami/web` (the hooks of a TypeScript
build), `@putnami/cloud` (`validate`), the sample `sample/shell-extension`
(`test`), and the Bazel, Pants, moon, Turborepo, Nx and Rush integrations
(`validate`, their graph check). The skipped ones and why:

- `@putnami/python`, `@putnami/java`, `@putnami/dotnet`, `@putnami/dart`,
  `@putnami/swift` and `@putnami/ruby`: their tasks run a toolchain
  Contributor CI does not provision (Python's resolve ruff and pytest from the
  package index).
- `@putnami/php`: the pinned PHP is installed, but its lint and test tasks run
  PHPStan and PHPUnit from `vendor/bin`, which Composer fetches from Packagist.
- `@putnami/dev`: its commands are interactive and print no run report, and it
  declares no hook or provider, so the fixtures that load it hold no process it
  starts.
- `@putnami/github-collaboration`, `@putnami/gitlab-collaboration`,
  `@putnami/jira-collaboration`, `@putnami/linear-collaboration`,
  `@putnami/local-collaboration` and `@putnami/memory-store`: providers with no
  command or task, which no run plans.
- `@putnami/events`: no command, task or hook.
- `@putnami/analytics`: its manifest names its extension dependencies under no
  CLI contract, so a workspace that loads it from this tree skips it.
- `@putnami/scaffold`: its task compiles its runtime with this repository's
  `go.work`, which a fixture workspace does not have.
- `@putnami/contributor` and `@putnami/observe`: their commands print their own
  output in place of the run's result, so no orchestration report is read.

The sources, manifests, prepare scripts and embedded profiles of the counted
extensions, the framework sources the web hooks import, and every extension
manifest, are inputs of `@putnami/cli`'s test task, so a pull request that changes one runs these tests again.

| Count | What it is | How it is lowered |
|---|---|---|
| Git calls per job | The engine's git calls on runs of 6 and 15 tasks, each on the default store under a home of its own, whose difference, what 9 more jobs cost, is held under a ceiling of 0; the git calls of the SDK jobs, by subcommand. | The engine reads the tree once per run and hands its facts to jobs in the job context; a job reads them there instead of running git. |
| Processes started by planning an unchanged workspace | Every process `--dry-run` starts on the workspace: git, toolchain probes, control processes. A plan reports no `run.orchestration`, so the test reads the process-wide ledger around it (`clitest.PlanCounts`). The engine's fixture also holds a whole run of the unchanged workspace, every task found in the cache. Each extension's plan is its git calls alone: a runtime handshake, a toolchain probe and an input-resolve answer are read from what the runs before recorded, and the workspace snapshot answers for the workspace probe ([ADR 0102](adr/0102-planning-reads-recorded-facts.md)), whether its toolchains are Putnami home releases or system toolchains that name their own executable ([ADR 0105](adr/0105-a-toolchain-that-names-its-own-executable-is-recorded.md)). | A probe or a read that does not depend on the change is answered from what the run already knows, or from state kept between runs. |
| Tasks a second run executes | The tasks a run of the unchanged workspace executes once a first run, on every worker in the order its tasks take there, filled the cache, read from its task records (`clitest.ExecutedTasks`). The Go, TypeScript, sdd and clientgen cases execute none, but the tasks a case names as never served from the cache, each with its reason: the sdd checks of the proposed squash message, which read an environment variable and Git history no key holds. A library's `build~infra` skip is cached as the manifest's absence. A `--no-cache` run stores nothing, so a run after it is served from what the runs before it stored. | A key reads only what the tree holds: a fixture ignores `.gen` as `putnami init` writes it, which also ignores Python's `__pycache__` and `*.egg-info`, no lint rewrites what another task generated, and a fixture's sources are written as its lint formats them, since a lint that fixes a source changes the tree the next run reads. |
| CLI starts per job | The `putnami` processes the SDK jobs start, such as a registry credential a job resolves itself. The Go fixture's jobs start none: a task declares `nativeRegistryCredential`, and the engine materializes it once per origin and per 4-minute window, which its own ledger counts. | The engine materializes what jobs ask the CLI for once per run and hands it to them. |
| Extension processes per run | The job, batch and control processes the engine starts. | Ready tasks share a batch process, or one extension process runs many jobs. |
| Engine CPU per task | The CLI process's own CPU per task on 300 and on 1,200 trivial batchable tasks run with `--no-cache` on four workers, the least of three runs after a first one. The test fails when it grows 1.5 times from the smaller plan to the larger, and at twice its recorded baseline on the larger. | Work the engine repeats per task or per pending candidate is done once per run, or once per job. |

Each extension case holds two ceiling ledgers, one line each: the run with
`--no-cache`, every task executed, and the plan of the unchanged workspace. A
case then runs the unchanged workspace once more, from the cache, and holds the
tasks it executes. A
ledger names every process kind the engine started, its git calls by
subcommand, then the CLI starts and git calls of the jobs. The tools jobs run
(`go`, `bun`, `cargo`) are the jobs' work and are left out.

Each count of a ledger is a ceiling: a run passes when every count is at or
below it, and a count the ceiling does not name has a ceiling of 0. A pull
request that lowers a count edits no test; a ledger check logs a ledger below
its ceiling, which a later change may lower. A pull request that raises a count
raises its ceiling in the same pull request and says why next to it, in one
short comment. When a test fails, it prints the ledger it got and each count
above its ceiling.
The growth of the engine's CPU per task does not depend on the host: an engine
whose work per task is the same on any plan reads below 1.4 on a 4-core Linux
container, most often below 1.0, and work repeated for each pending task reads
1.6 to 2.3. The test runs the engine with `GOMAXPROCS` 4, since a Putnami job
sets it to its CPU budget and the engine's CPU depends on it. The recorded baseline is measured with
coverage on a 4-core Linux container. The test fails when the engine's CPU per
task on the larger plan is twice the baseline or more: a slower host costs
every task alike and stays within that margin, a regression of the engine does
not. A change that raises the baseline says why next to it. The race detector multiplies the CPU, so a run with `-race`
skips that test.

## Cache Economics Benchmark

`tooling/cli/scripts/bench-cache.sh` measures how the shared build CAS changes
the time to a green impacted gate. It is deliberately a matrix rather than a
single warm-cache number: a cached current worktree and a new developer
worktree answer different questions.

### Run the Matrix

The harness creates and removes temporary worktrees; it does not alter the
checkout from which it is invoked. It requires an S0 namespace provisioner and
an explicitly read-only view of the CI cache:

```bash
CACHE_BENCH_S0_NAMESPACE_COMMAND="$HOME/bin/putnami-bench-s0-namespace" \
CACHE_BENCH_S1_READONLY_CACHE_URL='https://cache.example.test/trusted-ci-main' \
CACHE_BENCH_S1_READONLY_CACHE_TOKEN='reader-only-token' \
tooling/cli/scripts/bench-cache.sh
```

For every S0 class/trial, the executable named by
`CACHE_BENCH_S0_NAMESPACE_COMMAND` receives one namespace ID argument and must
create or reset that **empty** namespace. It writes one JSON object to standard
output with exactly the matching namespace identity and its short-lived cache
access configuration:

```json
{"namespaceID":"<requested-id>","url":"https://cache.example.test/...","token":"..."}
```

The harness rejects a mismatched or incomplete response and never records its
URL or token. A separate namespace is mandatory because a cache URL alone does
not reliably scope entries: providers commonly derive the namespace from the
credential.

`CACHE_BENCH_S1_READONLY_CACHE_URL` and
`CACHE_BENCH_S1_READONLY_CACHE_TOKEN` identify the CI namespace populated for
the selected merge base. The token must be unable to upload, commit, or advance
run markers. The harness uses this read-only configuration for **both** each
S1/S2 merge-base seed and its measurement, so a platform-specific miss cannot
pollute the trusted CI namespace. If CI data must first be prepared, do that in
CI or a separate writable staging namespace, never from this benchmark. When
measuring against a live provider, run the S1/S2 cells with the trust policy
pinned (`CACHE_BENCH_COMMAND='lint,test,build --cache-trust=ci'`) so the
published numbers reflect the authoritative CI channel rather than untrusted
hints.

S1/S2 trials share one merge-base seed store per selection profile (the
`e2e-sample` class seeds a different selection than every other class). Each
trial still varies an otherwise inert comment, so its changed jobs key on a
trial-unique marker: the only entries a later trial can find in the shared
store are merge-base-keyed — exactly what its own seed would have produced —
which keeps trials independent while making every seed after the first a fast
local hydrate instead of a full remote restore. Each S1 trial runs a merge-base
seed in its own measurement worktree against that shared store. An S2
measurement starts from a deliberately empty local store and must restore from
the read-only CI cache, so S2 trials skip seeding entirely once the profile's
one-time provider verification has run. Every temporary worktree runs
`putnami deps install` before its measured window: worktrees do not share
`node_modules` with the primary checkout, and the fresh-worktree install tax is
deliberately excluded from this matrix (it is tracked by the zero-init
bootstrap work instead). Neither credentials nor endpoint URLs are written to
the dataset.

The default is five trials per cell. Use a smaller count only for a local smoke
test, and label it as such:

```bash
CACHE_BENCH_ALLOW_FEWER_TRIALS=1 \
CACHE_BENCH_TRIALS=1 \
CACHE_BENCH_S0_NAMESPACE_COMMAND="$HOME/bin/putnami-bench-s0-namespace" \
CACHE_BENCH_S1_READONLY_CACHE_URL='https://cache.example.test/trusted-ci-main' \
CACHE_BENCH_S1_READONLY_CACHE_TOKEN='reader-only-token' \
tooling/cli/scripts/bench-cache.sh
```

The command under test is `putnami lint,test,build --impacted --baseline
<merge-base>` by default. Override the command words (not shell syntax) with
`CACHE_BENCH_COMMAND`, the baseline with `CACHE_BENCH_BASELINE`, and the durable
machine label with `CACHE_BENCH_MACHINE`. `CACHE_BENCH_OUTPUT_DIR` chooses the
output directory. The default output is `.putnami/cache-economics/<UTC timestamp>/`.

Each run writes:

- `runs.jsonl` — one self-contained, labeled record per trial.
- `report.json` — nearest-rank p50/p95 summaries for every matrix cell.
- `report.md` — the per-cell table plus S0→S1 and S2 target comparisons.
- `logs/` — command, session-inspection, and `/usr/bin/time -l` diagnostics.

Share `runs.jsonl`, `report.json`, and `report.md` together, and only after
verifying that the machine, date, base ref, and both cache namespaces are
appropriate to share.

### Matrix

| State | Meaning |
|-------|---------|
| S0 | Empty, isolated CAS namespace and an empty local store. |
| S1 | A working tree after a full trusted merge-base CI seed. |
| S2 | A fresh developer worktree with an empty local store, restoring from the CI cache. |

| Change class | Synthetic change made only in the temporary worktree |
|---|---|
| `no-change` | No tracked change; measures gate/selection overhead. |
| `docs-only` | Comment in this profiling document. |
| `ts-leaf` | Comment in `@putnami/utils`. |
| `go-leaf` | Comment in `go.putnami.dev/logger`. |
| `protocol-wide` | Comment in the cache protocol hub consumed by tooling. |
| `e2e-sample` | Comment in the Go sample application, with the normally excluded `e2e` tag explicitly enabled. |

### Metric and Label Contract

Every record has `date`, `machine`, `cacheState`, `changeClass`, `baseline`,
`runID`, and `trial` labels. Do not merge datasets whose machine or baseline
labels differ; report them as separate cohorts.

| Field | Unit | Source | Rule |
|---|---:|---|---|
| `wallToGreenMs` | ms | `/usr/bin/time -l` real time | End-to-end elapsed time, including CLI startup. |
| `osCpuUserMs`, `osCpuSystemMs` | ms | `/usr/bin/time -l` | Keep user and system CPU separate; `osCpuTotalMs` is their explicit sum. |
| `summedJobWallMs` | ms | Σ `session.tasks[].durationMs` over locally executed tasks | Sum of locally executed job durations (excluding cached and coalesced outcomes); never place this in a CPU column. `session.run.durationMs` is the session WALL, not this sum — do not substitute one for the other. |
| `taskEconomics[].spawnToFirstEventMs` | ms wall | `session.tasks[].spawnToFirstEventMs` | Startup overhead for one locally executed task; aggregate p50/p95 by `identity.task.kind`, never add it to or place it in the task-wall column. |
| `taskEconomics[].taskWallMs` | ms wall | `session.tasks[].taskWallMs` | Scheduler task wall across reuse, hooks, retries, subprocess attempts, and publication, retained beside but structurally separate from startup overhead. |
| `cacheHitRate` | ratio | `(session.run.reuse.localCache + .remoteCache) / session.run.counts.total` | Covers the tasks the session marks as reused from a cache. There is no `cached` verdict bucket in v2: `run.counts` spans every selected task and reuse is counted separately in `run.reuse`. |
| `coalescedJobs` | jobs | `session.run.reuse.coalesced` | Counts cold misses satisfied by a sibling process. |
| `coalescedRate` | ratio | `session.run.reuse.coalesced / session.run.counts.total` | Measures cross-process miss deduplication directly. |
| scheduler fields | mixed, named fields | `session.scheduler` | Keep critical-path, ready-wait, and worker data structurally separate. |
| `bytesDown`, `bytesUp` | bytes | cache-provider terminal `summary` operation | Uses the persisted provider-summary totals, not parsed console text. |

Per-task economics are contaminated by neighbor CPU contention when other jobs
run concurrently: a parallel run inflates `spawnToFirstEventMs` and per-task
wall against a serial run of the same baseline. Datasets that
feed per-task fixed-cost or per-`taskKind` decisions must therefore be measured
at `--max-parallel 1`, with the worker count recorded as a label; parallel runs
remain valid for run-level `wallToGreenMs`, CPU, and memory only. Per-task
conclusions must also generalize beyond this repository's project composition —
this workspace is only one consumer of the tooling, so scope them on the
extension and tool mechanics, with project counts recorded as sample context,
never as decision inputs.

The harness requires a working provider summary for every non-control S0 and
S2 row — both states measure against stores with no local entries, so a silent
local fallback would record a cold recompute mislabeled as its declared state —
and verifies the provider summary on the first merge-base seed of each
selection profile. Only a fully local S1 measurement may correctly have no
provider process; its record says
`available: false` with zero transfer bytes rather than inventing a summary.
Setting `CACHE_BENCH_REQUIRE_REMOTE=0` is only for local-cache diagnostics;
such output is not a publishable CAS-economics dataset because the provider byte
counters are unavailable. Percentiles use the nearest-rank definition, so with
five trials p50 is the third sorted observation and p95 is the fifth.

The report compares S1 p50 against S0 p50 against the savings target: the
target is met at 50% or better, with 50–80% recorded as the forecast band and
higher savings reported as exceeding the forecast rather than failing it. It
also tests whether S2 p95 is below one minute. This local harness does not
re-claim savings observed on CI; compare those against their own CI cohort.

## Qualifying memory across runner substrates

`session.json` supplies comparable memory facts; it does not declare what backs
runner scratch space. Substrate qualification belongs to the runner owner; the
CLI owns only the session facts. Use this procedure so the qualification itself
is repeatable:

1. Create an immutable campaign manifest containing the Putnami commit, merge
   base, runner image and toolchain revisions, cache seed, the literal workload
   command and selection, and the complete list of `--max-parallel` widths. The
   list must include width `1` as the attribution control and every candidate
   production width. Use the same manifest bytes for both substrate cohorts.
2. Have the runner owner declare each cohort `block-backed` or `memory-backed`
   from its trusted infrastructure configuration. Do not derive the declaration
   from mount names, filesystem type, `memory.stat`, PSI, or another heuristic;
   this CLI contract contains no scratch-backing field.
3. For every declared substrate × width cell, start from the same pinned cache
   state and run the same literal command, changing only the runner cohort and
   width. For example, a gate workload keeps the same `<jobs>` and `<selection>`:

   ```bash
   ./putnamiw <jobs> <selection> --max-parallel <width> --enforce-coverage
   ```

   Alternate substrate order and retain at least five completed trials per cell
   so one warm host or noisy neighbor does not become the conclusion. Record
   failed and aborted trials; do not silently replace them with successful ones.
4. Preserve the campaign manifest and the complete
   `.putnami/sessions/<id>/` directory for every trial, including `plan.json`,
   `events.jsonl`, and `session.json`. This keeps workload identity, outcome,
   execution MaxRSS, scheduler decision, and environment facts reviewable
   together instead of copying selected numbers into an untraceable table.
5. Compare like-for-like cells by width. Report run wall and outcome;
   `executions[].maxRssBytes`; capacity and its provenance; closing
   current/anon/file/shmem gauges; the cgroup-lifetime peak; windowed memory
   events; and scoped PSI deltas. Keep `fileBytes` inclusive of `shmemBytes`,
   keep host PSI separate from cgroup PSI, and label absent facts as unknown.

The decision rule is conservative: session facts are observation, not a new
scheduler input. Do not change admission, worker sizing, disk/output
reclamation, or scratch policy until the paired campaign demonstrates a
reproducible defect under the pinned workload. If it does, a separate reviewed
issue owns the policy change, success threshold, rollout, and rollback; the
qualification campaign remains its evidence rather than enforcement hidden in
the telemetry producer.

## Telemetry

Putnami collects a minimal, anonymous usage record to improve the CLI. It never
collects code, paths, project names, environment variables, error text, or user
identity. The public disclosure and rights information live at
[`putnami.dev/docs/concepts/cli-telemetry`](https://putnami.dev/docs/concepts/cli-telemetry).

### Enablement and opt-out

Telemetry is eligible by default for interactive CLI use. The first eligible
interactive job prints a one-line notice and records its timestamp locally. That
notice run records events locally but never sends a buffer. It is not a consent
prompt: the documented basis is legitimate interest, and users can object at any
time.

The effective state is resolved in this order:

1. An explicit `putnami telemetry on` or `putnami telemetry off` setting.
2. `DO_NOT_TRACK=1` or `PUTNAMI_TELEMETRY=off|0|false` to disable; `PUTNAMI_TELEMETRY=on` to enable.
3. A set `CI` environment variable disables telemetry.
4. Otherwise, telemetry is enabled by default.

`putnami telemetry status` prints the effective state and the rule that decided
it. `putnami telemetry off` deletes buffered events (including a temporary
delivery handoff) and the random device ID. `putnami telemetry on` records an
explicit opt-in but leaves the ID lazy until an eligible event is actually
recorded.

Non-TTY runs (including agents, scripts, and other automation) never print the
notice. They collect only after the same machine has completed an interactive
notice run, and their events carry `interactive=false`. CI is disabled unless an
explicit `telemetry on` setting wins the precedence order.

### What is collected

| Event | Closed, collected fields |
|-------|--------------------------|
| `session:start` | Names of the requested commands that Putnami's own extensions declare; project and job counts; presence-only booleans for `impacted`, `coverage`, `output`, `no-cache`, `projects`, and `watch`; `interactive` |
| `session:end` | Success; duration in milliseconds; `interactive`; a failure-only `errorCategory` of `usage`, `auth`, `api`, or `failure` |

The OTLP representation adds the event name, CLI version, OS, architecture, and
a monthly-rotating random device ID. Every attribute is checked against a
compiled-in closed vocabulary before it is written or exported.

The command vocabulary (`cliusage.Commands` in `protocols/telemetry`) lists the
eleven development verbs `@putnami/dev` declares and every public command that
an extension of the `@putnami` scope declares. The CLI records a requested
command only when it is in that vocabulary and a loaded `@putnami` extension
declares it public, as a command or as a verb. A command that only an
extension whose name is outside the `@putnami` scope declares is omitted, even
when its name matches one in the vocabulary. Only a name in the closed
vocabulary can leave the machine. A structured subcommand
(`putnami <group> <sub>`, such as `putnami cloud deploy`) runs without an
observer and records nothing; only a flat command, such as
`putnami cloud-deploy`, is recorded. The receiver drops a record that names a
command outside the vocabulary, so a receiver that knows a new command must be
deployed before a CLI that sends it.

Runner memory capacity, cgroup gauges/events, PSI, execution MaxRSS, workload
widths, and scratch-substrate declarations are **not** part of this anonymous
OTLP vocabulary. They remain local in `.putnami/sessions/<id>/session.json`
unless an operator explicitly preserves that session directory as a campaign
artifact.

### Local storage and delivery

Telemetry configuration and recorded events are stored in the user's home
directory:

```
~/.putnami-telemetry.json         # explicit state, notice timestamp, and device-ID month
~/.putnami-telemetry-buffer.jsonl # up to 1000 local events awaiting a drain
```

Existing buffers from the prior opt-in-only implementation are deleted when the
first notice is recorded; they are never sent under the new model. Each event
retains the ID created in its UTC month, avoiding a relabel of old events when a
later drain occurs.

The CLI sends eligible buffered events to the managed receiver at
`https://telemetry.putnami.dev`. `PUTNAMI_TELEMETRY_ENDPOINT` remains an
explicit override for controlled testing. Delivery is fail-silent, uses one
OTLP/JSON request with a two-second limit, and never delays the CLI result. It
uses **at-most-once delivery**: immediately before dispatch, the CLI atomically
hands the exact buffered batch off by removing it from local storage while
preserving events appended concurrently by the active run. This prevents a
receiver-accepted batch from being replayed after a fast CLI exit. The explicit
trade-off is bounded loss (at most the 1000-event local buffer) if the process
or transport fails after handoff and before the receiver accepts the request;
the CLI never retries that handed-off batch.
Sanitized raw events are stored in a dedicated Google Cloud Logging bucket in
`europe-west1` (Belgium) with 1-day retention. Cloud SQL retains aggregate
product counts plus a 35-day privacy-control membership projection keyed by
monthly rotating device ID. That projection exists only to count distinct
contributors; it contains no raw events and is never exposed by the read API.

### Inspecting telemetry

```bash
putnami telemetry status
putnami telemetry show
putnami telemetry show --output=jsonl
putnami telemetry off
```

With `--output=jsonl`, `telemetry show` puts the buffered events under `data`
as an array at every count: `[]` when none is buffered.

### Aggregate ownership and the private read contract

Aggregation belongs to the receiver workload, not to the CLI and not to the
operator dashboard. The CLI may reduce facts that exist only inside one command
or session, then emits the minimized closed-vocabulary event.
`telemetry.putnami.dev` owns everything after that: cross-session, cross-device,
and windowed aggregation, persistence, privacy suppression, and the read
contract. The operator dashboard owns only its authorization adapter and the
display; it never rebuilds aggregates from raw logs and never queries the
workload database.

The workload persists aggregate counts, device-day uniqueness, and a bounded
35-day contributor-membership projection (UTC day × dimension × key × rotating
device ID). The membership rows are privacy-control data used only to decide
whether a cohort has five distinct contributors. Operational admission,
overflow, and replay ledgers keep write/read work bounded. Only aggregate
results are exposed through `GET /v1/cli-usage/aggregate`:

- **Bounded.** The only parameter is `window`, and the only accepted values are
  `1d`, `7d`, and `30d` — fixed UTC calendar windows ending with the current,
  still-filling day. The response returns its exact `start`/`end` bounds and
  whether the last bucket is partial. There is no raw-event endpoint, export,
  query language, free-text filter, cursor, or caller-selected datasource,
  workspace, or project: no such parameter exists, and an unknown one is a 400.
- **Aggregate-only response.** Device IDs are used only inside distinct-count
  queries. No identifier, membership row, device set, or lookup capability
  crosses the boundary, and no request value ever reaches a query.
- **Privacy-suppressed.** Every nonzero daily or breakdown cohort requires at
  least 5 distinct rotating device IDs; five repeats by one device remain
  withheld. Totals and residuals are published only when they cannot be
  differenced back into a withheld cohort.
- **Explicit about what it does not know.** Every count carries an `available`,
  `unavailable`, or `suppressed` state. Ingest-health fields have no
  authoritative persisted counter, so they report `unavailable` — never an
  inferred zero.
- **Authenticated and fail-closed.** The route requires a service identity: a
  JWKS-verified bearer token whose audience, issuer, and caller subject/email are
  all pinned by configuration (`receiver.aggregateAudience`,
  `receiver.aggregateIssuer`, `receiver.aggregateCallers`). If any of the three is
  unset, the route denies every request. Anonymous `POST /v1/logs` is unchanged
  and remains the only anonymous route.

The dashboard's authorization adapter presents a service-identity ID token
that the receiver accepts; it holds no raw-event reader, database contract, or
caller-selected target. Deploy the adapter before the telemetry configuration
that sets the three pins.

To roll back the reader, first deploy telemetry configuration with all three
`receiver.aggregate*` values empty, then roll back the adapter if needed.
That incomplete configuration keeps the route mounted but returns `401` to every
caller; it neither fails open nor changes anonymous `POST /v1/logs`.

The [Legitimate Interests Assessment](cli-telemetry-lia.md) records the purpose,
necessity, and balancing tests for this field set. It must be reviewed whenever
the allowlisted telemetry vocabulary changes.
