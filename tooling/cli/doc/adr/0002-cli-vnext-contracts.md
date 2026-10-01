# ADR 0002 — CLI vNext: one contract with four surfaces, and the ratchets that hold it

- **Status**: accepted
- **Scope**: `@putnami/cli` (`tooling/cli`), `protocols/{cli,extension,runtime,job}`

## Context

An extension manifest's `cliContract` stamp is the only evidence the CLI has
about the extension's runtime half: whether its SDK reads a job context with a
typed identity, stages its declared outputs, and negotiates the runtime event
version. Adapting an older manifest by deleting the parts the CLI disagrees
with is safe for flag surface only. For runtime behavior it is a guess, and its
failures are silent: a `putnami.ready` marker read as log noise, a serve
watcher that never arms.

With one producer per wire ([ADR 0001](0001-cli-foundation-boundaries.md)),
moving a wire is one decision instead of a migration across five producers.

## Decision

### 1. The four contracts move together, and a bump ships a migration

Contract 3 moved four surfaces as one: the v3 task contract, job context v2,
runtime event v2 and lock format v2. A build speaks all of it or none of it.
The manifest stamp is the single gate (`extension.NegotiateManifest`):

- A stamp below the contract the manifest's vocabulary requires
  (`RequiredCLIContract`: `CurrentContract`, or the additive agent-content
  contract) does not load. The error names the side that moves: re-package
  the extension, or `putnami extensions update`.
- A stamp above `LatestContract` does not load; the remedy is a newer CLI.

The migration rule:

> **Every non-additive increment of `CurrentContract` MUST list what changed
> in the changelog block on the constant, and MUST ship a MIGRATION — a
> command that moves a workspace or an extension onto the new contract —
> instead of an adaptation that silently downgrades one.**

An additive increment adds vocabulary a manifest opts into and needs no
migration: a manifest without that vocabulary keeps its stamp. A migration is
reviewable, idempotent and states what it did; an adaptation is a private
downgrade. [ADR 0010](0010-compatibility-budget.md) applies the rule to every
public format.

Two structural carve-outs:

- **A manifest that declares no contract surface** (no commands, command
  groups, tools or agent content, e.g. a hook-only framework package) loads
  unstamped. The packager never stamps one. Loader and packager share one
  predicate, `extension.DeclaresContractSurface`, and the loader checks the
  higher-contract arm first, so a hook-only manifest from the future is still
  rejected.
- **`putnami migrate vnext`** is the only reader below the lock format floor
  (next section), and is exempt from the workspace-pin relaunch, because
  relaunching reads the lock it repairs.

### Lock format floor

`lockfile.ReadLockFile` accepts only lock format versions this CLI supports.
An older lock fails with an error naming `putnami migrate vnext --apply`; a
newer one fails as unsupported. `lockfile.ReadMigratableLockFile` is the one
reader below the floor, and only the migration calls it. No other command
converts a committed lock as a side effect: conversion is an explicit,
reviewable migration.

### 2. Identity is a type, not a convention

`protocolcli.TaskIdentity` is the same type in the plan, the job context, every
session-stream record, the session file, the MCP result and the result
envelope. Its `key` is derived from the structured members
(`project.id + ":" + task.name`); a key that disagrees with them is rejected.

Anything a subprocess emits then joins to the orchestrator's view without a
parse. Re-deriving an identity from `project.name + job.name`, a plan key
string or a rendered label is a defect.

Planning is pure: selection in, typed plan out. That lets watch validate,
cache and re-plan without re-deriving anything. Execution knowledge leaking
into the planner is what the planner's complexity ceiling catches.

### 3. A cache entry records what was declared (task-owned entries)

A task with a v3 `declares` block gets a task-owned entry: per declared output,
the id, file or subtree, root, root-relative path, and whether the task
produced bytes. Capture reads each output from its real location, stages it
under a task-owned staging root, and ingest validates the declaration against
what was staged.

- **The entry-format version is part of the address**:
  `address = sha256("putnami/store/entry-format\0" + <format> + "\0" + key)`.
  The cache key digests the task contract, not the payload format, and the
  machine-global store is shared with older CLIs. A new format changes the
  constant; every address moves and old entries age out through GC. No
  migration code.
- **Path selection is static**: declaration and job shape only, never run
  results, because the lookup before the run and the store after it must
  agree.
- **`package` tasks take the ordinary rule**
  ([ADR 0038](0038-package-tasks-use-ordinary-declared-capture.md)).
- **A task that rewrites its sources reuses only a proven-clean status
  entry.** Either v3 signal selects the rule (`mutatesSources` or a write of
  the `sources` resource), so it holds before strict validation runs. The
  scheduler digests the keyed sources before the run and again after a
  successful run. Unchanged bytes publish a reusable status; changed or
  indeterminate bytes publish a private mutation marker every restore path
  rejects. The detector reads its patterns from the same function as the key.
  A rewriter that declares no project-relative patterns is never proven clean,
  because a constant digest always equals itself.

The staging root is the store's boundary, not the process's. It does not
police writes outside the declaration; that needs process isolation.

### 4. One machine renderer; the rollback is a version pin, not an environment variable

`--output=json` and `--output=jsonl` are two projections of one canonical
reduction, so a streamed verdict and an aggregated one cannot disagree. Every
machine document carries `protocolVersion: 2`.

An interrupted run reports `aborted` with exit `130` and keeps its failures
visible. A run whose only failure was restored from cache fails: `run.counts`
spans every selected task, and reuse is counted apart in `run.reuse`.

**The rollback is a version pin, not an environment variable.**
`PUTNAMI_MACHINE_OUTPUT` selects nothing and is never an error; set to `v1`, it
prints one stderr notice. A lever that re-enables an old wire is a second
contract with no tests; a pin is exact, honored by the launcher, and ages out.
A second machine renderer is a rejected design.

### 4a. The live stream is bounded; the recorded stream is complete

`--output=jsonl` is a bounded view of the complete sanitized v2 task stream
stored in the session's `events.jsonl`. Normal and verbose modes use the fixed
budgets in `protocols/cli/doc/04-machine-output.md`; verbose widens the live
window only and never changes work, verdict or exit code. Separate ordinary,
failure and terminal partitions make the selection deterministic and keep
early success noise from consuming the failure reserve.

The sanitizer runs before persistence, measurement and selection. The final
line reports exact ordinary and failure elision counts and a session-relative
artifact reference, through the bounded `StreamRunSummary`. The complete
artifact is pruned with its session.

The writer never dual-writes v1. Readers keep the v1 `job:event` / `job:end`
fallback indefinitely, so sessions already on disk stay readable.

Rejected: a producer-configurable budget, truncating individual records, one
shared pool for ordinary and failure traffic, and persisting only the live
copy. Each breaks determinism, framing, late-failure evidence or post-mortem
completeness.

### 4b. A preview terminates as a plan, never as a session

`--plan` and plan-only `--dry-run` finish before a session exists. Their
machine verdict is a typed `PlanSummary`: `ResultV2.plan` in JSON, one
`plan:end` record in JSONL. A plan envelope succeeds with exit 0 and never
carries `run`, `data` or `error`; the JSONL record never carries `run` or
`machineOutput`. An empty preview is one complete document with zero task
metrics, so a consumer can enforce non-empty work without scraping text.

`plan:end` is live output only and never enters the session artifact.
`ValidateSessionStream` still requires `session:end`.

Rejected: a synthetic successful `session:end` (no scheduler ran), and parsing
the human plan table (it would make presentation text a machine contract).

### 5. The ratchets are the enforcement

Each invariant above erodes one "just read the old shape too" branch at a
time, so tests hold it:

- **AST-level absences**: no v1 machine emitter, no second reader of the
  retired selection variable, no manifest adaptation, no runtime-event version
  below the advertisement (`internal/cli/v1_bridge_ratchet_test.go`,
  `protocols/extension/adaptation_ratchet_test.go`). They read the AST because
  prose in this tree names the forbidden symbols.
- **Complexity ceilings** per unit, ceiling-only
  (`internal/cli/complexity_ceilings_test.go`).
- **Packaging gates and first-party conformance**: the publish-time contract
  gate in each language extension, and
  `internal/extension/first_party_{contract,runtime_events}_test.go`.
- **The negotiation matrix**: `TestLoadManifest_CompatibilityMatrix`
  (`protocols/extension`) is the executable form of the loader table in
  `protocols/extension/doc/04-validation.md`; changing a row means editing
  both.

**Every structural test pairs with a non-vacuity assertion.** A scan that
matches nothing passes forever and guards nothing. This binds every ratchet.

Raising a ceiling is allowed; the deliverable is the sentence saying why, not
the integer.

## Rejected alternatives

- Silent N-1 adaptation: runtime compatibility cannot be inferred by deleting
  manifest declarations.
- Identity parsed from display strings: it loses typed joins between
  subprocess and orchestrator records.
- Inspecting discovered output bytes instead of declarations: lookup and store
  would select different paths.
- Restoring source-mutating results without a clean proof: it replays
  unreviewed source changes.

## Consequences

- There is no supported downgrade. A consumer that needs the v1 wire pins an
  older build. A workspace on an old lock runs
  `putnami migrate vnext --apply`. An extension below the required contract is
  re-packaged.
- A workspace that pins an extension below the required contract is blocked
  until that extension ships at the required contract.
- A fixer that changes sources executes in every worktree; a clean one hits
  the cache.
