# 0005: `missions.status` reads a mission's sessions

- **Status**: accepted
- **Date**: 2026-10-29
- **Scope**: `go.putnami.dev/protocol/collaboration` (`protocols/collaboration`),
  the CLI route that calls it, and `@putnami/local-collaboration`
- **Amends**: [ADR 0003](0003-missions-contract.md), decision 1 ("There is no
  read operation: `sync` is the read path") and decision 8 (the route checks
  identity)

## Context

A session publishes its state, loop stage and token usage to its mission with
`missions.sync`. Nothing reads them back. `sync` answers the mission view and
the inbox of the calling session, never another session's state, and it is
host-only: an agent cannot call it.

A session that coordinates other sessions, such as one that runs the issues of
a larger outcome one session each, needs each child's stage and tokens to
track progress against a budget. The child sessions already report both to
their missions.

No release tag of the module contains the missions contract yet. ADR 0002
decision 4 fixes a contract's version 1 from the first release that contains
it, so an operation added before that release still belongs to version 1.

## Decision

1. **`missions` version 1 gains `status`, an optional read.** Its input names
   a mission: `{mission: {repository, key}}`. A title, when present, is for
   display and changes nothing. Its result is the mission view that `sync`
   answers (`key`, `title`, `url`, `plan`, `openDecisions`) and the mission's
   sessions. `ask`, `reply` and `sync` stay the required operations.
2. **A session in the result is the last sync the provider applied for it.**
   Each entry carries `session`, `state` and `observedAt`, and `stage`,
   `usage` and `branch` when the provider holds them. The list holds the
   sessions whose last applied sync named this mission, in every state,
   `ended` included; a session whose last sync named another mission is not
   listed. Entries are ordered by `observedAt`, newest first, then by engine
   and id; the list holds at most 50, and a session appears once. A mission
   the provider does not hold answers its empty view and no session.
3. **An agent may call it.** `status` is a read, so it is not host-only (ADR
   0003 decision 2). `missions.sync` stays the only host-only operation.
4. **A provider that does not offer it is `unsupported`.** The route refuses
   the call before it starts the provider, with reason
   `operation.unsupported`, as for any optional operation. A caller then
   reports stage and usage as unavailable; it never infers them.
5. **The route checks identity.** An ok `status` answer must name the mission
   key the request named. Any other answer is `unavailable` with reason
   `provider.identity_mismatch`, as for any read.
6. **The shared scenarios cover it.** `providertest.RunMissions` runs a
   status scenario when the target answers `status`: two sessions synced to
   one mission are listed with their state, stage, usage and branch, newest
   first; a session that moved to another mission leaves the list; and an
   unknown mission answers its empty view and no session.

## Rejected alternatives

- **Make `sync` return every session of its mission.** `sync` is host-only, so
  an agent still could not read them, and every routine sync would grow with
  the mission's history.
- **A `missions` version 2.** No release contains version 1 yet, so a new
  version would add a second document set with no reader of the first.
- **A required operation.** A provider that keeps no per-session record, or
  reaches a backend without one, could then not serve the contract at all.
- **Read the stage and tokens from the host's transcripts.** Each host stores
  them in its own format, and only on its own machine.

## Consequences

- A caller reads any mission's live sessions through the binding it already
  has, with `putnami missions status`.
- Each provider of `missions` decides whether to offer `status`; the local
  provider in this repository offers it.
- Once a release contains the missions contract, the `status` documents are
  fixed with the rest of version 1.
