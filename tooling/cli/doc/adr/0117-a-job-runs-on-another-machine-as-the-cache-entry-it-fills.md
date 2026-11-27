# ADR 0117 — A job runs on another machine as the cache entry it fills

- **Status**: proposed
- **Date**: 2026-11-27
- **Depends on**: ADR 0113, "An input digest whose witness holds is not
  collected again" (proposed), whose record names the files each input digest
  selected (section 2); and ADR 0116, "One machine process keeps every
  worktree warm" (proposed), whose supervisor socket and engine workers are
  both ends of this protocol (section 6). What this record asks of each is listed under "Open
  dependencies".
- **Scope**: design only, no implementation. It settles a new wire contract,
  `go.putnami.dev/protocol/execution` (`protocols/execution`, created by the
  implementation), the placement of a job in `@putnami/cli`
  (`internal/jobs/scheduler`), three changes to `protocols/cache` (sections 7
  and 8), two declarations of `protocols/extension` (section 5), and the
  executor that `@putnami/cloud` serves.

## Context

A cache key is computed for every job of the plan before any job runs, from
the job's declared inputs, its toolchain, its extension, its environment and
its dependencies' keys ([ADR 0059](0059-a-cache-key-describes-the-tree-not-the-commit.md),
`CacheKey.ComputeHashUsing`, key version `v9`). The remote cache
(`protocols/cache`) stores, at that key, an action result and a manifest of
output digests whose bytes sit in a content-addressed store (CAS). A remote
hit is restored by writing only what differs
([ADR 0108](0108-a-cache-hit-reads-its-entry-and-writes-only-what-differs.md)).
Every entry carries a provenance the cache server derives from the
authenticated identity: a producer (`ci` or `developer`, a closed set the
provider RPC validates) and a channel (`trusted` or `hint`). `--cache-trust
ci`, the CI default, lets only a trusted entry satisfy a job; `any`, the
local default, lets a hint satisfy one too.

Three contracts already move work or results across a process or a machine:

- `protocols/job`: the context the engine writes for one job subprocess, with
  its absolute paths, its staging directories, the resolved project and
  workspace facts and the tree facts it reads
  ([`protocols/job` ADR 0002](../../../../protocols/job/doc/adr/0002-a-job-reads-the-tree-facts-its-key-holds.md)).
- `protocols/runner`: a whole invocation placed on another machine with
  `--where remote` ([ADR 0032](0032-portable-runner-foundation.md)).
  The request carries a source snapshot (version 1) or a commit (version 2),
  the executing engine plans and runs the whole invocation, and a session
  comes back. A provider failure is never permission to run locally.
- `protocols/cache`: the remote cache, its provider RPC, the blob exchange
  directory and the object cache socket that compilers reach from inside a
  job.

None of them runs one job of a local run elsewhere. A cold run of this
repository is bound by the four local CPUs. Measured on 2026-11-26 and 27 on a
4-CPU container ([ADR 0100](0100-admission-charges-the-cpu-it-grants.md),
amendment of 2026-11-27): `lint,test,build,validate --all --no-cache
--enforce-coverage` took 558 to 603 s on `main`, with 3.62 to 3.68 CPUs busy
and a one-minute load of 16.6 to 17.1, four times the CPUs; decision
`a-no-cache-gate-reports-its-load-and-does-not-bound-it` keeps that load
reported, not bounded.

Some tasks cannot be served from a cache at all. Since the change that made
every uncached task state its reason, an unchanged `--all` run executes 51
tasks: 48 `test~test-env` and teardown steps, which provision a test database
and declare effects, and three sdd checks of the proposal's history and
message.

## Decision

### 1. The unit is one job, and its address is its cache key

A job runs on another machine as the **filling of its cache entry**. The
engine that planned the run stays the only planner, scheduler and verdict
producer (ADR 0032 §3). When a planned job misses every cache and is
eligible (section 5), the scheduler may hand it to an **executor** instead of
starting its subprocess. The executor materializes what the request names,
runs the job, and commits the entry at the job's address in the remote cache.
The scheduler then restores that entry as it restores a remote hit.

```
 submitting engine (CLI, or the machine process)
   plan → keys → negotiate: miss, eligible
        │ job request (key preimage, input manifests, dependency keys,
        │              context facts, env, params, toolchains, extension)
        ▼
 executor ──── materialize by digest ◄──── CAS (inputs and outputs)
   check the request, run the job in a fresh root, check its writes
   commit the entry ────────────────────► action cache (provenance:
        │                                     executor, submitter, request)
        ▼
 submitting engine: restore that entry as a hit (ADR 0108)
```

A job's result does not depend on where it ran, as far as the key names
every input the job reads; a job that reads an input its task does not
declare is a cache-key bug today, and section 3 makes it fail remotely
instead of passing. So placement is an accelerator, as the cache is: unlike
`--where remote` (ADR 0032 §1), a job the executor cannot run, an
unavailable executor and a remote failure all fall back to the ordinary local
execution of that job (section 4).

### 2. What travels

A **job request** holds, in canonical JSON under the rules of the runner's
source manifest (a fixed member order, entries sorted by path and maps by
key, Go escaping, strict decoding):

| Member | Content | Where it comes from |
|---|---|---|
| `key` | The job's cache key, and for a source-bound task its storage key | The key the scheduler negotiated |
| `preimage` | Every field `CacheKey` folds, by value: the task, its contract digest, the project and its metadata digest with the normalized project view it is computed from, the selected projects, the embedded version, the runtime identity (platform, release baseline, tree facts, the workspace-membership digest), the OS class, the source state, the emptied outputs, the parameters, the environment, the toolchain versions, the extension identity, the input digests and the keys of **every** job of `dependsOn` | The composition the scheduler already makes (`internal/jobs/cachekey`) |
| `inputs` | One manifest per input digest: entries in the runner's `SourceManifest` shape (path, kind, digest, size, mode, or a link's target), relative to the workspace root | The files each input digest selected: the record of ADR 0113 §1 holds them for a recorded digest, and the collection lists them otherwise |
| `sourceBinding` | For a source-bound task (`cache.sourceBound`), the manifest of its project's tracked source tree, which its key leaves out on purpose | The source binding the engine already reads |
| `dependencies` | For each dependency whose declared outputs the job reads (`protocols/extension` ADR 0014): its key, its address and the manifest of those outputs | The dependency's entry |
| `context` | The members of the `protocols/job` v2 context that are not paths: the project's resolved type, tags, metadata, closure and `generatedFrom`, the selection, the workspace projects with their configuration, the workspace options, the tree facts, the versions | The context the engine writes locally |
| `env` | The environment variables the task declares as inputs: unset, or set with a value (section 5, rule 4) | The run's environment |
| `extension` | Name, version and implementation digest, and for an extension loaded from source the inputs of its `prepare` (by digest, as file manifests) | The key's extension inputs |
| `toolchains` | Each toolchain by name, version and the archive integrity of the lock for the executor's platform | `putnami.lock.json` (`toolchains.<name>.integrities`) |
| `engine` | A published CLI version, or for a workspace-source CLI the manifest of its source | The running CLI |
| `platform` | `os`, `arch` | The key's host platform |
| `control` | `idempotencyKey`, `deadline` | Per submission |

The **request digest** is `sha256:` over the canonical request without
`control`, under the domain `putnami/execution/job/v1`. It identifies what
was asked; the key identifies the result.

**A field without a row fails a test.** As ADR 0113 §5 does for its witness,
a table in the new module names each field of `CacheKey` and each member of
the v2 job context with the request member that carries it and whether the
executor **verifies** it from bytes or the submitter **asserts** it. A test
reflects over both types and fails when a field has no row. A task whose key
folds a file outside the workspace (`options.generate.assets` outside the
root, hashed by absolute path) does not qualify.

Nothing else travels. No credential, no session record, no `.git`, no
repository configuration, no undeclared environment variable, and no
invocation locator (`protocols/job`: "never copied into results, events,
session records, telemetry, or cache traffic"). Bytes never cross the
request: every file travels by digest through the CAS (section 7), and the
executor verifies each byte as the runner's source store does when it
materializes (ADR 0032 §2).

### 3. What the executor checks and refuses

- **Admission.** It refuses a request whose task does not qualify under
  section 5, read from the manifest of the extension the request names; a
  request for another platform than its own (an extension built from source
  has a per-platform implementation digest, a task may key on the host
  platform, and a toolchain archive is per platform);
  a request whose engine or toolchain it cannot provide; and a request whose
  files are missing from the store or do not match their digests.
- **The key.** It recomputes the key from `preimage` with the engine the
  request names, and refuses a request whose key differs. It recomputes each
  input digest from the materialized `inputs`, the release baseline files
  from the tree object the key holds (`protocols/job` ADR 0002), and the
  source binding from `sourceBinding`, and refuses one that differs from the
  preimage. Those are **verified**. The rest of the preimage, the parameters,
  the environment, the selection, the project view and the context facts, is
  **asserted** by the submitter: section 8 says what that costs.
- **The dependencies.** For each `dependencies` entry, it reads the entry at
  that key from the cache, under the trust class of the request (section 8),
  and refuses the request when the entry is missing or when its output
  manifest differs from the one the request names. A submitter cannot pair a
  dependency key with outputs of its choosing. A dependency the submitter
  restored from its local store, with no entry in the remote cache, keeps the
  job local.
- **The extension and the engine.** It does not run a runtime it was handed.
  A published extension and a published CLI are fetched by their release
  digest; an extension loaded from source is prepared by the executor from
  its `prepare` inputs, whose digest is its implementation digest; a
  workspace-source CLI is built from its source manifest, once per digest.
- **An undeclared input.** The job runs in a fresh root that holds only the
  materialized inputs, the source binding, the dependency outputs, the
  toolchains and the extension runtime, with an empty home and an
  executor-owned directory for each toolchain cache the task declares
  (`toolchain-cache`), partitioned by trust class (section 8). A file the task
  reads without declaring it is absent there: the job fails or produces
  another output. The executor records the opens that failed on a missing
  path (seccomp user notification on Linux) and reports them with the
  failure; section 4 turns the failure into a local execution.
- **A side effect outside the outputs.** After the job, the executor compares
  the root with what it materialized. A file created, changed or removed
  outside the declared outputs' staging directories and the declared
  toolchain caches discards the result with `undeclared-write`: nothing is
  committed and the job runs locally. Network access is closed except to the
  dependency sources a lockfile among the inputs verifies (section 5).
- **A moved source binding.** A source-bound job whose execution moves its own
  binding (a write under its project's tracked tree) commits nothing, and the
  job runs locally, where the engine runs it again at the new binding as it
  does today.

### 4. What comes back, and how the scheduler treats it

An executor that ran a job successfully commits, through the cache's
existing write path (`find-missing`, `commit-batch`), the entry at the job's
address, which is its storage key for a source-bound task: the action result
with its cached events (`action-events`) and the manifest of the declared
outputs, with the provenance of section 8. The job's logs are a blob that the
executor's answer names, outside the entry: the submitter copies them into
its session, and the CAS keeps them as it keeps any upload until its
collection. The executor answers with the key, the request digest, its
identity and a terminal state: `committed`, `failed`, `refused` (with its
reason) or `canceled`. The provider RPC follows the runner's: `initialize`
with capabilities, `submit`, `lookup` by idempotency key, `follow` for the log
lines, `cancel`, `shutdown`, one JSON line each way, bytes through the
exchange directory.

The scheduler places a job only on an executor whose entries the run's trust
policy accepts (section 8), and treats the outcome as follows:

- **`committed`**: it negotiates the address again and restores the hit only
  when its provenance names that executor and that request digest. Any other
  entry at the address (an entry another producer had committed, which the
  idempotent commit kept) is restored only under the run's ordinary trust
  rules, as a hit it would have had. The job's session record says
  `placement: executor` and the executor's identity, never in its key.
- **`failed`, `refused`, `canceled`, unavailable, past the deadline, or a
  restore the trust policy refuses**: the job runs locally as it would have
  without an executor. A remote failure is never the verdict, never cached
  as a failure ([ADR 0090](0090-a-failed-task-is-cached-until-its-inputs-change.md)
  caches only a local one), and is reported beside the local result. A remote
  failure followed by a local success names the task as probably reading an
  undeclared input, with the missing paths section 3 recorded.
- **The local lease.** A job placed remotely holds the local lease of its
  task output, as a local execution does, so a second session of the same
  worktree waits for it and restores the same entry.

The deadline is twice the job's last measured duration plus the transfer of
its inputs. A job with no history is placed with a fixed deadline the
implementation sets from the measurements of section 9.

### 5. Which tasks qualify

A task qualifies for remote execution when every one of these holds, read
from its extension manifest and the plan:

1. **It is cached**: `cache` is not `false`. A task that is never cached has
   no entry to fill.
2. **It declares its outputs**: a task contract (`declares`, extension
   protocol v3). A v2 task whose outputs the engine infers by capture has
   nothing the executor can check its writes against.
3. **It has no effect beyond its outputs that a cache hit cannot replay, and
   writes nothing it does not declare**: no `process`, `registry`, `cloud` or
   `workspace-files` effect, no `mutatesSources` or `rewritesSources`, and no
   write its contract leaves out. `toolchain-cache` qualifies: the executor
   owns that cache.
4. **It reads no secret and no invocation-scoped value**: no dependency on a
   `test-env` step's bindings, no sensitive declared output, and each
   declared environment input either unset in the run or declared not secret
   by its task. No such marking exists today, so a task with a set
   environment input does not qualify yet.
5. **Every dependency it reads is pinned by digest**: each toolchain it uses
   has an archive integrity in the lock for the executor's platform, and each
   package it fetches or reads from an installed tree (Go modules, a
   `node_modules` tree) is verified by a lockfile among its inputs (`go.sum`,
   `bun.lock`), which the extension declares. No such declaration exists
   today, so a task that fetches packages (`network`) or reads an installed
   tree does not qualify yet; with it, the executor fetches or installs them
   itself, keyed by the lockfile's digest.

Two existing declarations are coarser than their tasks. Biome's `format` and
`check` declare `mutatesSources` for every run, while they rewrite only under
`fix`; a contract that names the parameter under which a task mutates lets
them qualify when `fix` is off. Go's `golangci-lint` already splits the two:
the fixing task is planned only under `fix` (on by default locally, off in
CI), and the check-only task does not mutate.

The engine computes eligibility per task from these declarations alone, and
the executor checks the same rule at admission. No list of task names is kept.

Classified on 2026-11-27 at `299343dc` (`main`) from
`./putnamiw lint,test,build,validate --all --dry-run --output=json` (2,245
planned jobs, local defaults, so `fix` on) joined with the manifests of the
extensions that provide them. The classification reads rules 1 to 3, the
test database part of rule 4, and rule 5 by provider: every job the
TypeScript extension provides reads `node_modules`, every Go job that
declares `network` fetches modules. It did not check which environment inputs
were set; the jobs that declare one are counted in the last column.

| Class | Jobs | Tasks | With an environment input |
|---|---|---|---|
| Qualifies today | 1,347 | Go's `generate`, `config-merge`, `infra`, `api` and `docs` steps, the Cloud config validation, sdd's `features` and `specs` checks, seven of the eleven workspace checks, the generated clients | 262 (`config-merge`: `CONFIG_DATA`, `APP_ENV`, `K_SERVICE`) |
| Qualifies with lockfile-verified dependencies | 494 | Go's `test` (108), `staticcheck` (131), `describe` (70) and check-only `golangci-lint` (6); every TypeScript step (179) | 126 (`DATABASE_TEST_BINDINGS`, unset outside a test database) |
| Stays local while `fix` is on; check-only with `fix` off | 125 | Go's fixing `golangci-lint` | 0 |
| Qualifies with a mutation declared under `fix`, when `fix` is off | 40 | Biome's `format` and `check` | 0 |
| Does not qualify: writes what it does not declare | 131 | Go's `tidy`, which rewrites `go.mod` and `go.sum` without declaring it (its manifest says the declaration is deferred) | 131 (`GOPROXY`, `PUTNAMI_OFFLINE_DEPENDENCIES`) |
| Does not qualify: reads test database bindings | 24 | the tests that depend on a `test-env` step | 24 |
| Does not qualify: writes workspace files | 32 | Go's `compile` (`go.work.sum`) | 0 |
| Does not qualify: always mutates | 1 | sdd's `codeowners-sync` | 0 |
| Does not qualify: never cached | 51 | the 48 `test~test-env` and teardown steps, and sdd's `breaking-marker-validate`, `agent-attribution-validate` and `feature-coverage-ratchet-validate` | 26 |

### 6. One protocol, four neighbours

| Contract | Relation |
|---|---|
| `protocols/job` | Unchanged. The executor writes the job context for the subprocess as the engine does locally, from the request's `context` and the paths of its own root. The context itself does not travel: it holds absolute paths and the invocation locator. |
| `protocols/runner` | Shares the `SourceManifest` entry shape and its canonical bytes, the typed parameters, the provider RPC envelope and the idempotency rule (one key, one attempt), and the blob exchange directory. Differs in unit and in failure: the runner places a whole invocation, its executing engine plans and its verdict is a session, and a provider failure never falls back; this protocol places one job the submitting engine planned, its result is a cache entry, and every failure falls back to the local job. `--where remote` stays as it is. |
| `protocols/cache` | The return channel. The executor commits through the existing write path, and the scheduler reads through `negotiate` and `restore`. Three changes: the `executor` producer and its provenance members behind a capability (section 8), a blob upload that grants no entry write (section 7), and the submitter-scoped channel (section 8). |
| The machine process's socket (ADR 0116) | ADR 0116 runs one supervisor per machine and user, which owns the machine's admission, and behind it one engine worker per CLI build, which runs the engine and starts the jobs. The two ends of this protocol sit there. **Submitting**: the placement seam is in the engine, so a run in a worker or in its own process hands a ready job either to its own job start, after the supervisor's grant, or to `executor-provider`. **Executing**: an executor host runs the same supervisor. ADR 0116's client ↔ supervisor protocol gains, by addition under a new version, a `job.execute` operation whose payload is this record's job request, with `job.cancel` beside it and the log lines and terminal state on its connection. The supervisor routes it to the worker of the build that `engine` names, starting one as for a run, admits it against the machine budget, and the worker runs it in a fresh root with the checks of section 3. A member's machine serves `job.execute` only to its own user (section 8); it then reads the inputs from the worktree, not the CAS, and fills the local store. |

A new module, not a version 3 of `protocols/runner`: the runner's decoders
refuse every mix of address and planner by version (runner ADR 0006), its
attempt ledger and its session bundle exist because its result is a verdict,
and a job's result is an entry. The new module imports the runner's
manifest and parameter types and the cache's action result and manifest types,
so no shape is written twice. The provider is the reserved extension command
`executor-provider`, resolved like `runner-provider` and the cache provider
(`protocols/cache` ADR 0001), with no product name.

### 7. Inputs reach the store without the right to write entries

A developer's credential is read-only for the remote cache today
(`protocols/cache` ADR 0003): after the echo, core uploads no blob and
commits no entry. Remote execution needs the submitter to upload the input
blobs the store lacks, but not to commit entries. The CAS does not validate
what the presigned and direct paths write, and its deduplication keeps the
first writer, so a blob upload is not harmless as it stands: a wrong blob
under a digest would make every entry that references it unrestorable.

The provider RPC therefore gains a `blob-upload` operation and the
`cas-write` capability: a session that echoes it uploads blobs through a
path on which the server decompresses and hashes each blob before it stores
it (`upload-batch`, or a validated staging area for large blobs), while
`read-only` still forbids every commit. Only an executor commits what it ran.

The first remote run of a tree uploads its inputs once: this repository's
tracked files are 13,196 files and 91.6 MiB at `299343dc`. Later runs upload
the blobs that changed.

### 8. Trust and attribution

- **Who runs jobs for whom.** In this design, the remote executors are the
  workspace's Putnami Cloud executors; a member's machine executes only for
  itself, through its machine process (section 6), and commits nothing
  remotely. A submitter needs a workspace credential that grants execution;
  the executor never receives it, and receives no other credential.
- **Executors by trust class.** An executor accepts submissions of one trust
  class: credentials the cache server lets write trusted entries (the CI of
  the default branches, which `protocols/cache` already distinguishes from a
  pull request's CI), or the others. Toolchain caches and every other state an
  executor keeps between jobs belong to the trusted class, or, in the other
  class, to one submitter: a job from a member cannot leave anything a
  trusted job, or another member's job, then reads.
- **Attribution.** The cache server records each executor entry with producer
  `executor`, the executor's identity, the submitter's identity and the
  request digest, all derived from authenticated identities, as it does for
  `ci` and `developer` today. A reader can tell who ran a job, for whom, and
  what was asked. The producer set is closed in the provider RPC, so the new
  producer and members travel only to a CLI that lists a `producer-executor`
  capability; to any other CLI the provider reports a trusted executor entry
  as `ci` and every other one as `developer`, which that CLI already reads.
- **Channel.** An entry is `trusted` only when the executor runs the trusted
  class, the submitter's credential may write trusted entries, and every
  dependency entry the request names is `trusted`. An entry filled for any
  other submitter is **submitter-scoped**: the cache serves it to that
  submitter's own runs, and to no one else's, under `any` as under `ci`. The
  submitter asserts part of the preimage (section 3), so an entry it asked for
  is no more shared than an entry its own machine would have built, which
  stays in its local store today.
- **One entry per address and scope.** The action cache keeps a trusted
  entry and each submitter-scoped entry at the same address side by side. A
  commit is idempotent within its scope only: a member who fills an address
  first never blocks the trusted entry CI commits there, and a negotiation
  answers with the entry the requester's scope and trust policy select.
- **Verification.** The executor verifies every byte it materializes, every
  input digest and the dependencies' manifests, and recomputes the key
  (section 3). A result is not verified by running it again: a deterministic
  task (`cache.deterministic`) can be, and the measurement plan runs that
  comparison on a sample (section 9); a standing re-execution rate is a later
  decision with those numbers.

### 9. Measurement plan

The question is which part of this repository's cold run can leave the four
local CPUs, and what wall remains.

**What one cold run shows.** Measured on 2026-11-27 on a 4-CPU Linux
container (15.7 GiB) whose Go build cache an earlier run had warmed, signed
out of Putnami Cloud: `./putnamiw lint,test,build,validate --all --no-cache
--enforce-coverage --continue-on-error` (session `20261127-074237-d52814`,
head `515e72d2`, which differs from `299343dc` by a draft of this record
only; one run, not the medians of ADR 0100). Wall 608.6 s, 2,104 s of task
CPU (3.46 CPUs busy), one-minute load 11.0 median and 16.3 peak, 1,684 jobs
run and 537 skipped, no failure. Per class of section 5, with each job's
wall from the session and its CPU from its execution (a batched execution's
CPU split evenly over its tasks):

| Class | Jobs | Task wall summed | Task CPU | Share of CPU |
|---|---|---|---|---|
| Qualifies today | 1,347 | 105 s | 78 s | 4 % |
| Lockfile-verified dependencies | 494 | 1,391 s | 1,532 s | 73 % |
| Fixing `golangci-lint` | 125 | 293 s | 296 s | 14 % |
| Biome under `fix` | 40 | 42 s | 62 s | 3 % |
| Test database bindings | 24 | 233 s | 100 s | 5 % |
| Writes workspace files, writes undeclared, always mutates, never cached | 215 | 43 s | 36 s | 2 % |

The scheduler's critical path was 203.4 s (`run.scheduler.criticalPath`),
through the Go `tidy` steps to `/cloud/extension:lint~staticcheck`. Over the
`dependsOn` edges alone the longest chain is 179.4 s and ends at
`/cloud/extension:test~test` (176 s), a test with database bindings, which
does not leave. The longest jobs are tests: `/tooling/cli-repository` 163 s,
`/tooling/cli` 122 s, `/go/extension` 94 s (239 s of CPU),
`/tooling/install-scripts` 82 s and `/typescript/extension` 69 s.

Two earlier runs the same morning, one on a cold Go build cache (792 s),
stopped on this record's own first drafts and are left out.

**Expected wall**, from that run, with a 4-CPU submitter and executors of 4
CPUs, each bound the larger of the local CPU over four, the whole CPU over
all CPUs, and the 203 s critical path, before transfer:

| What may leave | CPU that stays local | Expected wall |
|---|---|---|
| Nothing (today) | 2,104 s | 609 s measured |
| The class that qualifies today | 2,026 s | about 505 s, whatever the executors: 17 % less |
| Also the lockfile-verified dependencies, `fix` on (a local run) | 494 s | about 265 s with one executor (2,104 s over 8 CPUs), about 205 to 230 s with two or more: 62 to 66 % less |
| The same with `fix` off (CI): check-only `golangci-lint` leaves, Biome too once its mutation is declared under `fix` | 136 s | the same bounds: the critical path holds the wall |

The class that qualifies today is 4 % of the CPU: the protocol alone saves
about a sixth of the wall. The lockfile declaration of rule 5 is the
condition of its value. Below about 205 s the wall is the longest chain, set
by single test suites (`/cloud/extension`, `/tooling/cli-repository`,
`/tooling/cli`) and the serial Go `tidy` chain; splitting a suite into
several jobs is outside this record. The task durations above were measured
under a load of about 11, so the same jobs on an idle executor should take
less; the transfer of the first remote run of a tree (91.6 MiB of tracked
files, less what the store holds) adds to it, and so does the executor's
build of a workspace-source CLI and preparation of the extensions, once per
digest, which a change under `tooling`, `protocols` or `go` moves.

**What the implementation measures**, on the same 4-CPU container class,
each run alone with the load under 1.0 at its start, medians of three. The
runs keep the task history of a priming run (the deadline of section 4 reads
it) and do not pass `--no-cache`, which turns the remote cache off: each run
uses a cache namespace of its own and starts on an empty local store, so no
run hits the entries another filled.

1. **The baseline**: the local cold gate under the same conditions, wall and
   busy CPUs, as ADR 0100 reports them.
2. **The hermeticity probe**, before any network: each job of a qualifying
   class run locally in a fresh root that holds only what its request names,
   with the executor's admission, key, write and missing-path checks and no
   transfer. It counts the jobs that fail there and pass in the workspace (an
   undeclared input) and the jobs whose writes leave their outputs. Each one
   found is a declaration to fix before the task qualifies.
3. **Determinism**: each deterministic qualifying job run twice, in two roots,
   comparing the output manifests. A difference removes `deterministic` from
   the task or fixes it.
4. **Remote cold**: the gate with qualifying jobs placed on 1, 2 and 4
   executors of 4 CPUs each, warm toolchain caches on the executors (their
   steady state), and the same with cold ones. Recorded per job: queue,
   upload, materialize, engine build and extension preparation, run, commit
   and restore times; per run: wall, local
   busy CPUs, the one-minute load, uploaded and downloaded bytes, and the
   longest chain of jobs that could not leave.
5. **The lower bound** each run is compared with: the longest dependency
   chain of the plan with the measured task durations, and the CPU of the
   jobs that stay local divided by four.

The implementation is done when the measured wall is within 20 % of the
expected one or the record says what took the rest, the hermeticity probe
finds no undeclared input it did not fix, and a run without any executor
keeps every key and result, compared by a test.

## Consequences

- A cold run is no longer bound by the local CPUs for the jobs that qualify;
  the jobs that stay local, and the longest chain, bound it instead.
- Until the lockfile declaration of rule 5 lands, little leaves: 4 % of this
  repository's cold CPU.
- The CI that fills trusted entries and an executor that fills them are the
  same kind of producer. A member's remote jobs fill entries only that member
  restores.
- A task becomes eligible by declaring honestly, and loses nothing by not
  declaring: a task that does not qualify runs locally, as today. Go's `tidy`
  and its deferred `mutatesSources` declaration are the first such case.
- The undeclared inputs that the cache key misses today become visible: the
  hermeticity probe and every remote failure followed by a local success name
  them. They are cache-key bugs too.
- An executor that is wrong in a way the key does not see (a broken
  toolchain cache, a compromised machine) writes wrong entries under its
  identity. Attribution names it; the trust classes keep a member's jobs from
  reaching a trusted executor's state; a re-execution sample detects it for
  deterministic tasks.
- No key and no entry address moves. A run with no executor behaves as today.

## Migration and reversal

Placement is off unless a run enables it, and the protocol is new. The cache
changes are behind capabilities: a CLI or a provider that does not list
`cas-write` or `producer-executor` exchanges what it does today, and an older
CLI reads executor entries as `ci` or `developer` (section 8). The
submitter-scoped channel is enforced by the cache server, so an older CLI is
never served another member's executor entry. Reversal is turning placement
off; the entries executors filled stay valid entries at their keys, as
entries CI filled stay after CI stops.

## Open dependencies

- **ADR 0113, proposed.** The input manifests of section 2 read the selected
  files from its record. Without it, the collection lists them, at the cost
  ADR 0113 measures; the protocol does not change.
- **ADR 0116, the machine process, proposed.** Section 6 asks its client ↔
  supervisor protocol for a `job.execute` operation, added under a new
  protocol version, that takes this record's job request, routes it to the
  worker of the request's engine build and admits it against the machine
  budget, with `job.cancel` beside it. ADR 0116 needs no change to accept
  this record: the operation is an addition its versioning already allows. If
  its record decides otherwise, this record is amended in the same pull
  request.
- **Three declarations in `protocols/extension`** (section 5): dependencies
  verified by a lockfile, without which no `network` task and no TypeScript
  task qualifies (73 % of the measured cold CPU); a mutation under a
  parameter, for Biome; and an environment input declared not secret.

## Rejected alternatives

- **A version 3 of `protocols/runner`, one job per request.** The runner's
  unit is an invocation whose verdict is a session, and its decoders and
  attempt ledger are built for that; a job request would carry neither. Its
  shapes are reused by import instead.
- **Ship the job context (`protocols/job`) as the request.** It holds the
  submitter's absolute paths and the invocation locator, which must never
  leave the run, and not the bytes' digests.
- **Return the outputs to the submitter, not to the cache.** The submitter
  would write them, and a second run would ask for the same job again. An
  entry serves the submitter's later runs and worktrees too, and the restore
  path already exists.
- **Run a remote failure as the verdict.** An undeclared input makes a job fail
  remotely and pass locally; a verdict from that failure would be wrong, and
  caching it would make it stick.
- **Trust an entry by its executor alone.** The submitter asserts part of the
  preimage; a trusted executor running a member's request would launder an
  assertion into a trusted entry.
- **Share a member's executor entries as hints.** `any`, the local default,
  would serve every member what one member asserted, which no member's
  credential can write today.
- **Members' machines as remote executors for the workspace.** Their entries
  would be as trusted as their owner, and their toolchain caches shared with
  whoever submits; a machine executes for its owner only.
- **Sandbox every job locally first.** The hermeticity probe does it once, to
  measure; doing it on every local run would cost every run what only remote
  placement needs.
- **Race every remote job with a local one.** It doubles the CPU the
  placement exists to save; the deadline falls back instead.

## Follow-up implementation issues

Filed with the owner's agreement on 2026-11-27, one issue each, in this order
of dependency: 1 and 3 first, 2 beside them, 4 after 1 and 3, 5 after all:

1. `protocols/execution`: the job request, its preimage table and its digest,
   the provider RPC and the fixture corpus.
2. `protocols/cache`: the `blob-upload` operation and `cas-write`, the
   `executor` producer behind `producer-executor`, and the submitter-scoped
   channel with one entry per address and scope.
3. `protocols/extension`: dependencies verified by a lockfile, a mutation under
   a parameter and an environment input declared not secret; the Go and
   TypeScript extensions declare them, and Go's `tidy` declares its writes.
4. The CLI: eligibility, the request and its preimage, and the hermeticity
   probe (measurements 2 and 3) behind a measurement flag.
5. The executor in `@putnami/cloud` and the CLI's placement seam, then the
   remote cold measurement (4 and 5).
