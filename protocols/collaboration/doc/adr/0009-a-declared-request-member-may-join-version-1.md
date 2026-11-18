# 0009: A request member a provider declares may join version 1

- **Status**: accepted
- **Date**: 2026-11-18
- **Scope**: `go.putnami.dev/protocol/collaboration` (`protocols/collaboration`),
  the CLI route that calls a provider, `@putnami/local-collaboration` and the
  agent session hooks of `@putnami/contributor`
- **Amends**: [ADR 0006](0006-version-1-is-stable.md), decision 2, and
  [ADR 0007](0007-a-declared-result-member-may-join-version-1.md), decision 1
  ("A request member … still open a new version")

## Context

A session hook publishes the session to its mission with `missions.sync`. The
hook never fails a turn, so a sync that fails leaves one line in the hook log
and nothing else. A machine logged 83 such failures in three weeks: refused
for want of a signed-in CLI, timed out, or refused by a provider of another
version. None showed on the mission, whose owner saw a session stop moving and
could not tell why.

The session can say what it lost only on the next sync that succeeds, so the
count must travel in a sync request. ADR 0007 lets a result gain a member a
provider declares, and keeps a new request member for a new contract version:
a provider that does not know a member refuses the request that carries it.
A `missions` version 2 for one optional member would make every missions
provider and caller select a new version, and the provider most sessions use
lives in another repository with its own releases.

The danger is a provider that receives a member it does not know. The CLI
reads every provider's declaration before it routes a call, so it can keep
the member from a provider that does not declare it.

## Decision

1. **An operation's request may gain an optional member in version 1 when
   the catalog lists it as a declared member of that operation.** The CLI
   removes it from a request to a provider whose manifest does not name it
   in the operation's `members`, and sends the rest of the request
   unchanged. A provider reads it only when it declares it. A required
   request member, a narrowed value and a new outcome still open a new
   version, and every version 1 document stays valid with the same meaning.
   `OperationSpec.Members` lists an operation's declared members, request
   or result, each as its operation's document defines it.
2. **`missions.sync` declares `missedSyncs`, and `missions.status` declares
   it too.** In a sync it reports the syncs of the session that failed since
   the last one that succeeded: `count`, at least 1, the `reason` of the last
   one, one line, and `lastAt`, when the last one failed. A sync with no
   failure before it carries none. A provider keeps, for each session, the
   `missedSyncs` of the last applied sync that carried one, as it keeps the
   stage and the usage; in `status`, each session carries the one it keeps.
3. **The hook counts, the provider keeps.** The session cache counts the
   failed syncs and their last reason; the next sync that succeeds carries
   them and clears the count. A sync refused as invalid while it carries
   `missedSyncs` or a plan goes once more without either, so an older CLI
   that does not know the member never blocks every later sync; the count it
   refused is dropped.
4. **No manifest contract rung.** A CLI that does not know a member a
   provider declares withdraws that contract version of the provider, with
   its reason, as for any undeclared member (ADR 0007).

## Rejected alternatives

- **Open `missions` version 2.** It costs every provider and caller a
  version selection for one optional member.
- **Send the count only when `capabilities` declares it.** It costs every
  session start one more call into the provider's process, inside a hook
  budget that holds two, for what the CLI already knows when it routes.
- **Report the failures in `error` or `event`.** `error` belongs to a failed
  session and changes its state; `event` is for display, and a provider never
  reads meaning from it.
- **Keep the failures in the hook log only.** Nobody reads it, which is the
  problem.

## Consequences

- A missions provider that keeps `missedSyncs` declares it on `sync` and,
  when it offers `status`, on `status`. Until it does, the CLI drops the
  member and a failure stays visible only in the hook log and in
  `putnami doctor`.
- A provider that declares `missedSyncs` needs a CLI that knows it; an older
  CLI withdraws its `missions` version.
- The frozen version 1 corpus does not change: no frozen document carries the
  member.
