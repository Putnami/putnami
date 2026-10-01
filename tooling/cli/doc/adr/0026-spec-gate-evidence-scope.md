# ADR 0026 — The spec gate's evidence scope

- **Status**: accepted
- **Scope**: `@putnami/cli` spec gate (`internal/specgate`, the observation
  recovery in `internal/engine`) and the verification record of
  `protocols/features`

## Context

A `spectest.Proves(t, feature, requirement, check)` call lives where the code
it protects lives, so a project's requirement is routinely attested by another
project's test suite. Under a narrowed selection (`--impacted`, `--projects`)
the planner schedules those projects only as prerequisites (`test~generate`,
`config-merge`). Their `test~test` never runs, no observation report exists,
and a gate that reads only in-session observations fails `enforce` on a run
that regressed nothing.

## Decision

### 1. A cached observation is admissible evidence for an unselected project

A `test~test` cache entry holds the project's captured
`putnami-feature-verification.json` as a declared output. When the in-session
join leaves a requirement unresolved, the gate reads that entry. This adds no
trust assumption: `restoreReservedDeclaredArtifactEvents` already restores the
same artifact on a warm hit for a selected project.

The cache key closes the argument:

| Attester | Selection | Evidence |
|---|---|---|
| changed | `--impacted` selects it | its `test~test` runs: fresh observation |
| unchanged | not selected | its key did not move: the entry matches these inputs |

A session that also publishes keeps this table true: the release-set plan
narrows the publish alone, never the gate's own selection.

### 2. Candidates: the transitive dependency closure plus direct dependents

A **dependency** can attest because the owner's tests execute it; depth is
unbounded because the closure is exactly what one test process runs. A
**direct dependent** can attest because it is the concrete producer of the
contract the owner defines (in this repository, `tooling/sdd-extension` for
`protocols/architecture`, and `openapi`, `proto`, `grpc` for
`go/framework/api`). Beyond one hop a dependent exercises the contract only
through the layer between, which is already a candidate. The direct set is
bounded by fan-in (31 at most here), not by depth.

- **A project leaves the set only when its report producer ran.** The producer
  is the `test` job whose declaration names the report as a command-output
  file. `ProducesVerificationReport` in `cli-model/jobs` is the only copy of
  that rule, and the cache lookup uses it too. Prerequisite jobs such as
  `test~generate` carry command `test` but run no tests. A `test` pipeline with
  no v3 declaration has no producer, so its project reads as consulted and
  silent.
- **Silence is an answer.** An entry whose report output is recorded empty ran
  for these inputs and observed nothing. Only a candidate with no cache entry,
  an uncacheable test task, or `--no-cache` is unconsulted. Only the task that
  declares the report is asked; other `test` steps are ignored.
- **An exhausted scope blocks.** When the candidate set is empty, or every
  candidate answered and none reported the check, the requirement stays
  `missing` and blocks: no test proves it. The group carries a
  `features.unobserved_requirement` warning saying the scope was exhausted and
  the fix is a `spectest.Proves` call.

### 3. The lookup is lazy

Recovery runs only for a group the in-session join left unresolved. A fully
verified run does no extra planning, keying, or hashing and never touches the
store.

### 4. Nothing is restored into the working tree

The report is read in place from the entry's `files/` directory. A file under
`.putnami/out` would make a project that never ran look like it did. A
restored observation must still name a regular file of the project that
published the entry. The report reference records the digest of the bytes
read and is marked `restored`, so an auditor can tell produced evidence from
recovered evidence.

### 5. Unresolvable evidence warns; it never sanctions

A check can stay unresolved because no source existed: no planned test and no
cache entry (empty store, cold remote, `--no-cache`). The requirement then
resolves to `unobserved`, which `GroupBlocks` does not block on, and the run
warns, naming the unconsulted projects and why.

- A requirement qualifies only when every check is satisfied or missing with
  reason `check-not-observed`. A skipped, failed, stale, duplicated, or
  unreadable check rests on an observation that arrived, and keeps blocking.
- The trigger is that some candidate could not be consulted. The gate cannot
  know which project attests a check without an observation naming it.

The cost: in a run with an unreachable candidate, a requirement that has no
test is warned instead of blocked. In every warm or non-narrowed run the
unconsulted set is empty and the gate keeps full strength. Under `--no-cache`
with a narrowed selection every unresolved check warns; that combination is
too rare to justify a second mechanism with its own failure modes.

When the session's own work failed or was canceled, both scope messages
(unconsulted and exhausted) are silenced by the guard that suppresses the
sanction. A canceled `test~test` makes attested requirements look unobserved,
and advice to add a `spectest.Proves` call would be confident and wrong. The
failing job already explains the absence.

### 6. The record's protocol version does not bump

`unobserved` is a new value of an existing string field and `restored` is an
additive `omitempty` boolean, so the shape is unchanged. An older reader keeps
parsing; a bump would make it refuse the whole record. The closed vocabulary is
`features.ValidRequirementVerificationStates`, enforced by the record's strict
reader.

### 7. Layering

`internal/specgate` is the policy half: when to ask, which projects may be
asked, what an answer means. The engine owns the cache half behind the
`specgate.ObservationRecovery` interface, reusing the run's planner, params,
and `store.CacheManager`. It keys one `test` plan over the whole candidate set:
a cache key mixes declared inputs and upstream keys, never the selection.

## Rejected alternatives

- **Widen the plan by dependency.** A narrowed run would cost a full run.
- **An authored `attestedBy` on the criterion.** It duplicates what the
  `spectest.Proves` call already states; two declarations of one fact rot.
- **The whole workspace, or all transitive dependents, as candidates.** A cache
  key per project, and no attester the direct set misses.
- **Excusing an exhausted scope.** It turns "nobody wrote the test" into a
  warning on the run best placed to prove it.

## Consequences

- Recovery costs time only on a run the gate would otherwise fail: the
  `protocols/architecture` case keys 31 candidates, about 4 s.
- The DARC import `cli.verification-observations.v1` stays `fail-closed`: a
  consulted source that did not report sanctions, an absent source warns.
- `putnami specs verify` renders the `unobserved` count and marks restored
  references.
- An attester two or more hops above its contract needs a new decision with its
  own bound.

## How this is proven

The production proof is this repository:

```
./putnamiw test --projects @putnami/sdd --enforce-coverage            # prime the attester
./putnamiw test,validate --projects go.putnami.dev/protocol/architecture --enforce-coverage
./putnamiw test --projects go.putnami.dev/openapi,go.putnami.dev/proto,go.putnami.dev/grpc --enforce-coverage
./putnamiw test,validate --projects go.putnami.dev/api --enforce-coverage
```

`tooling/cli/internal/cli/e2e/specgate/spec_gate_matrix_test.go` is a harness
for the states this repository cannot produce on demand: an attester reporting
a check failed, and an entry recording the report empty. Its fixture `test`
command has the real extensions' shape: an uncacheable `generate` step waiting
on `^generate` before the step that writes the report.
