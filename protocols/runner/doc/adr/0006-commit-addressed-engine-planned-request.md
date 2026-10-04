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

### 3. Its own digest domain

`CommitInputDigest` covers the commit, the invocation and the requested
selection under `putnami/runner/execution-input/v2`. It excludes `control`
and the negotiated capabilities, as version 1 does. The separate domain means
no version 1 input digest can name a version 2 input.

### 4. The bound-request channel carries both versions

`ParseBoundRequest` reads the root `version` once and hands the document to
that version's parser, so a version 1 document parses, encodes and digests as
it did before version 2 existed. `control.caller` accepts `ci` in version 2
only; version 1 stays the CLI's. The provider RPC's `submit` keeps carrying
version 1: a version 2 request reaches an engine through the channel alone.

### 5. The executing engine binds the checkout before anything runs

The executing engine refuses a version 2 request unless its checkout's HEAD
is `source.commit` with no modified tracked file, before the first-use
bootstrap runs repository code. It then plans through its ordinary selection,
stamps versions from Git and never resolves placement. It compares no expected
plan, because none came. A plan that publishes is held to both version 1
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
