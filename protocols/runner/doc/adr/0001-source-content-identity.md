# ADR 0001: Source content has its own identity

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/runner` source manifest

## Context

An uncommitted worktree cannot be identified by its Git HEAD, and committing
unchanged source must not change its content identity. The session tree
fingerprint includes HEAD on purpose and stays as it is. A planned task may also
need a file Git ignores; omitting it silently makes the executing engine judge
different inputs than the submitting one.

## Decision

The source manifest v1 binds, per entry, the portable path, file bytes, size,
executable mode, exact relative symlink target, and the `bound` mark, in a
deterministic canonical JSON digest. Git version-stamp context stays a separate,
credential-free value. Typed canonical serialization fixes member order and
escaping; fixture bytes and digests are the conformance authority for every
implementation.

The manifest carries tracked and non-ignored untracked files. An entry carries
`bound: true` when Git ignores the path in the source worktree and a planned
task declares it as an input. The member is present only as `true`: an explicit
`false` is non-canonical and rejected. Binding a path changes the source digest,
as a content change would. Whether a path is ignored and required is a fact of
the producing engine; a receiver validates the manifest's shape, never its
truth.

Strict admission rejects ambiguous JSON and portable materialization hazards
before source execution. Manifest validation grants no authority: it does not
replace rooted, collision-safe materialization or blob digest verification, and
a bound blob travels through the same digest-addressed exchange as every other
file. Execution identity and cache keys keep their independent Git, platform,
engine and policy inputs, so content association never weakens execution
applicability or trusted cache policy.

## Consequences

Any change to the canonical form or to the bound fields changes every source
digest and moves the fixtures with it.
