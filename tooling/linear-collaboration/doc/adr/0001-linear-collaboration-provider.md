# ADR 0001 — Linear collaboration provider

- **Status**: accepted
- **Scope**: `@putnami/linear-collaboration` (`tooling/linear-collaboration`)

## Context

The collaboration contracts
([protocols ADR 0001](../../../../protocols/collaboration/doc/adr/0001-collaboration-provider-contracts.md))
put task operations behind provider-neutral calls. A team that tracks its work
in Linear needs a `tasks` provider, so that its agents find, read, create,
update and move Linear issues through `putnami tasks …`, as they do with
GitHub issues through `@putnami/github-collaboration`.

## Decision

1. **One extension implements tasks v1 on one Linear team.** It offers `find`,
   `get`, `create`, `update` and `transition`. The settings name the team by
   its key (`{"team": "ENG"}`). `assign`, `link` and `claim` are optional in
   the contract and left out; Linear holds no change proposals.

2. **The provider speaks Linear's GraphQL API itself.** Every operation is a Go
   `net/http` request to `https://api.linear.app/graphql`, so the provider can
   tell a request that never left the process (`unavailable`) from one written
   without an answer (`unresolved`), and repeat only reads. No dependency is
   added.

3. **The credential is `LINEAR_API_KEY`, read by the provider.** The binding
   carries none, and the provider reads no other variable or file. The key goes
   only to Linear's API; the only override, `PUTNAMI_LINEAR_API_URL`, accepts a
   loopback URL alone. The key must consist of letters, digits, `_`, `.` and
   `-`, so it cannot inject a header. The runtime removes it, and every string
   shaped like a Linear key (`lin_api_…`, `lin_oauth_…`), from everything it
   writes.

4. **A reference names the organization and the issue's UUID.** A task is
   `{"source": "linear:<organization urlKey>", "id": "<issue UUID>"}`, and its
   `key` is the identifier people read, `ENG-42`. The UUID stays the same when
   an issue moves to another team, where the identifier changes. A reference
   another source issued, or an id that is not a UUID, is `not_found` without
   a request.

5. **Workflow state types are the state mapping.** Linear gives every workflow
   state one of a fixed set of types, so the binding configures no mapping:
   triage, backlog and unstarted read as `open`, started as `in_progress`,
   completed as `done`, canceled and duplicate as `canceled`. `providerState`
   is the state's name. A transition moves the issue to its team's first state
   by position of the target type, `unstarted` for `open`. Linear has no type
   for `blocked`, so a transition to it is `unsupported`.

6. **A create is keyed by the issue's own id.** Linear accepts the id of a new
   issue. `create` derives it from the SHA-256 of the team's id and the
   idempotency key, shaped as a UUID v4, and records the digest of the request
   in an HTML comment at the end of the description. A repeat reads the issue
   by that id, which Linear answers at once, unlike its lists: the same digest
   returns the issue with `created: false`, another digest is
   `idempotency.mismatch`. Two concurrent creates with one key cannot open two
   issues, because Linear refuses a second issue with an id it holds. After a
   lost answer the provider reads the id back and answers `unresolved` only
   when the read cannot settle it. `update` keeps the comment, and a task's
   body never shows it. An issue whose description lost the comment still
   answers for its key, since its id names the key, and is returned as it is.

7. **Labels are names, and a missing name is created.** The contract carries
   label names. A create or update resolves each name, ignoring case, among the
   labels the team can apply (its own and the workspace's, never a label
   group), and creates a team label for a name no label carries, as GitHub
   does for an issue label.

8. **Preconditions are checked, not atomic.** Linear has no conditional write:
   `update` and `transition` compare `expectedRevision` with the issue read
   just before the write. The revision is `v` and the first 32 hex digits of
   the SHA-256 of the JSON array of title, description, state name, sorted
   label names and sorted assignee ids. The manifest declares
   `preconditions: checked`.

9. **The live proof runs on request.** `bin/live-test <team key>` runs the
   shared task scenarios against a real team with `LINEAR_API_KEY`, then
   deletes the issues and labels it created. The ordinary gate runs the same
   scenarios against a fake Linear.

## Consequences

- A workspace binds `tasks` to Linear and `proposals` to GitHub in the same
  `options.collaboration` block, since contracts are bound independently.
- A writer between the read and the write of `update` or `transition` is not
  detected, as on GitHub.
- A label name that differs from an existing label only by case reuses that
  label, and a run that names new labels leaves them in the team.
