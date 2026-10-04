# ADR 0006: A commit-addressed request is engine-planned

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/runner` execution request and the
  bound-request channel

## Context

A version 1 request ([ADR 0002](0002-execution-request-and-provider-rpc.md))
addresses a snapshot and freezes what the submitting engine resolved: the
selection, the plan, the versions and the environment. Its only caller is the
CLI, which has a worktree to capture and an engine to plan with.

A hosted CI service holds a commit and no snapshot, and runs no engine before
the run. It cannot build a version 1 request: it has no manifest digest to
name and no plan to freeze. A request that named a commit and carried a frozen
plan would bind two things that nothing ties together, because a frozen plan
names no commit it was planned from.

## Decision

### 1. Version 2 pairs a commit address with the executing engine's plan

The request has `version: 2` and five blocks: `protocol`, `source`,
`invocation`, `selection` and `control`. `source` names a full `commit` and,
for an impacted selection only, the full `base` it is measured against.
`selection` names the requested `mode` and, for the projects mode only, the
selectors. The `invocation` block and its rules are version 1's. There is no
`plan` and no `environment`: the executing engine plans its checkout of the
commit, and that checkout pins its own engine, extensions and toolchains.

A snapshot has no Git history, so an engine that receives one can neither
measure an impacted selection nor stamp versions. A commit has that history,
so the engine that checks it out does both. Each version therefore pairs one
address with one planner, and a document that mixes the two is refused with an
error that names the member it mixed in.

### 2. One spelling per request

`source.base` is present exactly when the mode is `impacted`, and
`selection.projects` is present, non-empty, exactly in the `projects` mode.
Selectors are sorted and unique, one selector per entry. An explicit empty
`base` or selector list is refused, so one request has one canonical form.

### 3. The protocol refuses unportable commands

A version 1 request comes from a CLI submitter, which refuses the commands no
portable run carries before it freezes a plan. A version 2 request has no such
submitter, so `ValidateCommitRequest` refuses them: `serve`, `run` and
`compose` stream a live workload until the deadline, `qualify` reaches one
from the executing machine, and `format` rewrites a source that never returns
to the caller. `UnportableCommands` names them, and the submitting side
refuses every one of them. Version 1 validation does not change. Declared
side effects are not refused: `invocation.publication` authorizes them, as on
the version 1 executing side.

### 4. Its own digest domain

`CommitInputDigest` covers the commit, the invocation and the requested
selection under `putnami/runner/execution-input/v2`. It excludes `control`
and the negotiated capabilities, as version 1 does. The separate domain means
no version 1 input digest can name a version 2 input.

### 5. The bound-request channel carries both versions

`ParseBoundRequest` reads the root `version` once and hands the document to
that version's parser, so a version 1 document parses, encodes and digests as
it did before version 2 existed. `control.caller` accepts `ci` in version 2
only; version 1 stays the CLI's. The provider RPC's `submit` keeps carrying
version 1: a version 2 request reaches an engine through the channel alone.

### 6. The executing engine binds the checkout before anything runs

The executing engine refuses a version 2 request unless its checkout's HEAD
is `source.commit` with no modified tracked file, before the first-use
bootstrap runs repository code. The entrypoint that runs the check comes from
the checkout, so the check binds the bootstrap, hooks and tasks, not the
entrypoint. The engine then plans through its ordinary selection, stamps
versions from Git and never resolves placement. It compares no expected plan,
because none came, and refuses a planned task whose cwd leaves the checkout,
as the submitting side does. A plan that publishes is held to both version 1
publication checks; a plan that publishes nothing runs whether or not the
request carries `invocation.publication`, because the caller authorized
publication before any plan existed.

## Consequences

- A CI service runs a gate or a publication on a commit with one typed
  request and no CLI-side planning.
- The session of a version 2 run records the remote placement, the checkout's
  own tree and no provenance: the `protocols/cli` provenance block requires a
  source digest, which a commit-addressed request does not have. A provenance
  statement for version 2 needs a `protocols/cli` change of its own.
- The version 1 fixtures, canonical bytes and digests are unchanged.
