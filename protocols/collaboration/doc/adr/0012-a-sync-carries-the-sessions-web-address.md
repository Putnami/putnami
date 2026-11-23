# 0012: A sync carries the session's web address as a declared member

- **Status**: accepted
- **Date**: 2026-11-23
- **Scope**: `go.putnami.dev/protocol/collaboration` (`protocols/collaboration`),
  the CLI route that calls a provider, the agent session hooks of
  `@putnami/contributor` and the hosted missions provider of `@putnami/cloud`
- **Applies**: [ADR 0009](0009-a-declared-request-member-may-join-version-1.md),
  decisions 1 and 3

## Context

The owner follows a session from its mission, but cannot open the
conversation to read it or answer the agent. Some hosts give a running session
a web address. Claude Code does in two cases: a cloud session sets
`CLAUDE_CODE_REMOTE_SESSION_ID`, and a local session with Remote Control on
sets `CLAUDE_CODE_BRIDGE_SESSION_ID` while the connection is active. The
address is `https://claude.ai/code/<id>`, and a cloud id that starts with
`cse_` reads as `session_`. A local session without Remote Control has no
address.

Only the session knows the address, so it travels in a sync request. A
mission provider that has no page for a session has no use for it.

## Decision

1. **`missions.sync` declares `sessionUrl`**, an optional request member of
   version 1 (ADR 0009). It is an absolute `https` address of at most 512
   characters, with no credentials. Absent keeps the address the provider
   holds; an empty string clears it. The CLI removes it from a request to a
   provider whose `sync` does not declare it. `missions.status` does not
   declare it: the members of `sync` (`MissionMembers`) and of `status`
   (`MissionStatusMembers`) now differ.
2. **The Claude Code hook sends the address when it changes.** At each sync
   it reads the two variables, the cloud one first, and builds the address
   from an id of at most 256 letters, digits, `_` and `-`. It sends the
   address when it differs from the last one a sync carried, and an empty
   string once when
   the address it sent is gone, as when Remote Control disconnects. It omits
   the member when nothing changed or nothing is known. A Codex session sends
   nothing.
3. **An older CLI never blocks the syncs.** A sync refused as invalid while it
   carries `sessionUrl` goes once more without it, as with `missedSyncs` and
   the plan (ADR 0009, decision 3). When that one passes and the refusal
   names `sessionUrl` or `session_url`, the address counts as sent until it
   changes, so an older CLI does not make every sync go twice. When the
   refusal names another member, the next sync carries the address again.
4. **The hosted provider forwards it** to its sync route as `session_url`.

## Rejected alternatives

- **Send the address on every sync.** It repeats the same value on each tool
  call for no gain, and absent already keeps it.
- **Let the provider build the address from the session id.** The session id
  a hook reports is not the id of the web session, and the address belongs to
  the host, not to the provider.
- **Answer it in `missions.status`.** No caller of `status` needs it yet.
  Adding it later is a declared result member of its own.

## Consequences

- A missions provider that keeps the address declares `sessionUrl` on `sync`.
  Until it does, the CLI drops the member and the mission shows no link.
- A provider that declares `sessionUrl` needs a CLI that knows it; an older
  CLI withdraws its `missions` version (ADR 0009, decision 4).
- The frozen version 1 corpus does not change: no frozen document carries the
  member.
