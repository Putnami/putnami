# 0008: A session leaves no empty mission behind

- **Status**: accepted
- **Date**: 2026-11-18
- **Scope**: `go.putnami.dev/protocol/collaboration` (`protocols/collaboration`),
  `providertest.RunMissions`, `@putnami/local-collaboration` and the agent
  session hooks of `@putnami/contributor`

## Context

A session reports to one mission at a time: the one its last applied
`missions.sync` named. The contributor hooks name `branch:<branch>` from the
first sync, at session start, because the agent does not know its issue yet.
`putnami agent attach --issue <n>` then moves the session to `issue-<n>`. The
branch mission stays behind with no session, no plan and no stage, and a
mission list shows the same work twice.

The provider already knows when a session moves: ADR 0005 lists a session
under the mission its last applied sync named, and only there.

## Decision

1. **A provider retires the mission a session leaves when nothing else holds
   it.** When an applied sync names another mission than the session's last
   applied sync, the session leaves that mission. The provider retires it when
   no other session's last applied sync names it, and it holds no decision and
   no message. A provider that also serves memory keeps a mission whose memory
   holds a record. A missions provider bound beside a separate memory provider
   cannot see that record; retiring the mission leaves the record where it is.
2. **A retired mission reads as one the provider does not hold.** `status`
   answers its key, an empty title, an empty plan, no open decision and no
   session, as ADR 0005 decision 2 says for an unknown mission. A later sync
   that names it holds it again, as any first sync does.
3. **No document changes.** The rule needs no request member: the provider
   reads the left mission from the session it holds. Every version 1 document
   stays valid with the same meaning (ADR 0006 decision 2).
4. **A host sends its plan to the mission it moves to.** The plan the session
   published may leave with the retired mission, so the contributor hooks send
   the session's plan on the first sync that names the new mission.
5. **The shared scenarios cover it.** When the target answers `status`,
   `providertest.RunMissions` moves a session and reads the mission it left:
   empty when nothing else held it, and kept, with its title, when another
   session, a decision or a message holds it, or a memory record when the
   target also serves memory.

## Rejected alternatives

- **The sync names the previous mission key.** The caller would send a new
  request member, which opens missions version 2 (ADR 0006 decision 2), and
  the provider would trust the caller about a fact it already holds.
- **The contributor declares the issue before the first sync.** The agent
  learns its issue after the session starts, and a session that never attaches
  one still needs a mission.

## Consequences

- A session that starts on a branch and attaches an issue leaves one mission:
  the issue mission.
- A provider outside this repository that runs `providertest.RunMissions`
  fails the new scenario once it requires this protocol version, until it
  retires the left mission.
