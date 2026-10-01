# ADR 0032 — Portable execution leaves the engine at one seam and returns as a session

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/engine` placement and portable seam,
  `internal/runnersource`, `internal/runnerprovider`, `putnami sessions`)

## Context

A local run and a remote run share one engine. A remote provider owns
transport, isolated resources and authorization. Only the engine holds the
final plan, the resolved selection with its evidence and the discovered
provider set, so only the engine can hand a run over. A provider that planned
or scheduled on its own would be a second verdict producer.

The execution request, the provider RPC, the source manifest and the attempt
ops are contracts of [`protocols/runner`](../../../../protocols/runner/doc/adr/).
This ADR owns the CLI side. [ADR 0037](0037-ignored-input-admission.md) owns
what the snapshot must carry beyond tracked files.

## Decision

### 1. Placement

- `--where` is an execution policy flag. Omitted means local. `remote`
  resolves the reserved `runner-provider` command through the reserved-provider
  resolver, with no product name and no version floor.
- A remote request with no provider takes the ordinary local lifecycle exactly
  once, decided before runtime preparation, credential resolution or capture.
  Two providers are a configuration error, never a guess.
- A provider failure or an uncertain submission is never permission to run
  locally.
- Requested and actual placement live in the session's optional placement
  block, never in command parameters, run markers or task keys.

### 2. Source capture

- `internal/runnersource` is an effectful boundary, not an executor. The
  protocol stays pure, and the HEAD-bound session tree fingerprint keeps its
  own identity domain.
- Capture rejects submodules, sparse or assume-unchanged indexes, unmerged
  entries, unsafe links and unsafe paths instead of losing source silently.
  Two full content scans plus Git and index checks reject capture races.
- Capture never copies `.git`, repository hooks, local Git configuration,
  credentials or environment.
- The blob store publishes completed objects into a private namespace by
  exclusive hard link and validates a concurrent winner. Materialization
  verifies every byte, and a directory stays private until every entry passes.
  Knowing a digest grants no workspace access.

### 3. One seam, both sides

- The submitting seam is `internal/engine/portable.go`. It runs after the
  final plan and the production preflight, and before the release-set handoff
  and execution. It preserves post-hook extension discovery and preflight
  ordering. It returns the imported canonical session and the remote gate's
  exit code.
- The executing side is the same engine. The bound-request adapter in
  `internal/cli` sets `Request.Portable`. The selection stage plans the frozen
  project ids and task scopes with the recorded mode and baseline, and the
  seam refuses a re-planned graph that differs from the expected plan.
  Placement is not re-resolved there.
- No planner, scheduler or reducer exists outside the engine. The provider
  and the test harness never plan.

### 4. Import

- Import verifies every byte, validates every session and plan document
  against `protocols/cli` result-v2, requires each document to name its
  session, then stages, syncs and renames the directory into place.
- An existing session with identical content is a safe repeat. Different
  content is a collision, and nothing is written.
- A green process over a session that recorded a failure is refused. So is a
  "completed" attempt with no session and exit 0.
- Nested sessions keep their parent links. The submitter records no session
  of its own, so CPU is never counted twice.
- The executing engine writes `placement.provenance` (source digest,
  execution-input digest, submission key) into its session, only beside
  `actual: "remote"`. The importer reads the gate document from the exchange
  before it publishes anything and refuses a mismatch on any member, so a
  foreign session never reaches the store or `latest`. The block names no
  provider, attempt or machine.

### 5. A remote attempt is resolved by its record, never by running it again

- Every submission is written to `.putnami/runner/attempts/<key>.json`,
  atomically, before the submit envelope leaves the process. The key is the
  request's idempotency key. The record holds the provider, the
  execution-input and source digests, the attempt reference once acknowledged,
  the last forwarded cursor and the terminal outcome (state, exit code,
  imported session id, or the error verbatim). It is the transport's ledger:
  the verdict stays the imported session, and nothing in the record is a
  task-cache input.
- Before a submit, the client looks for an in-flight record for the same
  inputs and provider, and always sends `lookup` for the key. A hit is
  adopted; a miss is submitted under the same key. A settled record never
  blocks: running the gate again is an intentional retry with a new key. A
  lookup answer with a different execution-input digest, or a different
  attempt than the record names, is refused.
- Follow persists the cursor as records arrive and reconnects from it after a
  transport loss, at most three consecutive times. A record at or below the
  cursor is a dropped replay. A non-increasing sequence inside one answer is a
  protocol error. A full terminal answer is drained before the terminal state
  is accepted.
- An interrupt sends `cancel` through the still-open provider session (the
  provider process is not bound to the run's context), waits a bounded time
  for the acknowledgement and then for a terminal state, records what it saw
  and exits with the signal code. A cancel that raced a completion keeps the
  provider's outcome: the attempt is fetched and imported under a bounded
  detached context. Every budget fits inside the terminal adapter's
  two-signal shutdown window, and none bounds a verdict.
- An expired, corrupt or refused bundle leaves the record with the remote
  state, the exit code and the error, no local session, and the same
  reference to retry the import. Nothing is rerun or fabricated, and the exit
  code is an error until the evidence is home.

### 6. Session commands

- `putnami sessions inspect [<id>] [--run <ref>]` reattaches to a record by
  attempt reference or submission key, resumes from the cursor and imports
  through the one import path. An imported attempt is shown from the store.
- `putnami sessions list [--revision <sha>]` matches 7 to 64 lowercase hex
  characters as a prefix of the recorded tree's head commit. A ref name is
  refused because it moves. `--output=jsonl` rows carry `revision` and
  `placement` when the record states them.

### 7. Qualification

The conformance provider lives in `internal/cli/clitest` as a re-exec role of
the test binary. It keeps attempts on disk behind disposable RPC processes,
runs a detached supervisor per attempt that owns the child engine, forwards
output with cursors and turns SIGTERM into one canceled outcome. It injects a
lost acknowledgement, a stream drop, expired artifacts and a corrupt bundle.
It honors `cli.source: workspace` by launching the snapshot's own
`./putnamiw`. It is a harness, not a product, and needs no service or
credential.

## Rejected alternatives

- **A provider that plans or schedules to fill a gap**: a second verdict
  producer that can disagree with the engine.
- **ChangePlan v1 as the executable request**: it is clean-HEAD admission
  metadata with its own digest and does not carry typed parameters, pinned
  tools or resource declarations.
- **Falling back to local on provider failure**: it turns a remote failure
  into a local success nobody asked for.

## Consequences

- The engine file ceiling and the package count each carry the placement, the
  seam, `internal/runnersource` and `internal/runnerprovider`; the ratchet
  tests name them.
- A run that cannot reach its evidence exits in error with a resumable
  reference instead of a verdict.
