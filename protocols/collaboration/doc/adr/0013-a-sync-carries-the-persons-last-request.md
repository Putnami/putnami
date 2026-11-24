# 0013: A sync carries the person's last request as a declared member

- **Status**: accepted
- **Date**: 2026-11-24
- **Scope**: `go.putnami.dev/protocol/collaboration` (`protocols/collaboration`),
  the CLI route that calls a provider, and the agent session hooks of
  `@putnami/contributor`
- **Applies**: [ADR 0009](0009-a-declared-request-member-may-join-version-1.md),
  decisions 1 and 3

## Context

A sync carries the agent's last message, so a mission shows what the agent
answered. It does not carry what the person asked, and a reader sees an
answer without its question.

Only the session sees the person's request: Claude Code hands it to a
`UserPromptSubmit` hook before the turn starts. A hook there must never block
or delay the request, and a delivered message the hook itself starts a turn
with is the owner's message, already on the mission, not the person's
request.

## Decision

1. **`missions.sync` declares `lastPrompt`**, an optional request member of
   version 1 (ADR 0009): the last request the person sent the session, at
   most 4096 characters. Absent keeps the request the provider holds. The
   CLI removes it from a request to a provider whose `sync` does not declare
   it. `missions.status` does not declare it.
2. **The Claude Code hook keeps the last request and sends it once.** An
   asynchronous `UserPromptSubmit` entry records the request in the session
   cache, cut at 4096 characters on a character boundary, and writes nothing
   else: no git, no sync. It replaces the request recorded before; no history
   is kept. The next sync carries it, and an ok answer counts it as sent, so
   a later sync omits the member until a new request arrives or the session
   moves to another mission, which receives it again. An empty
   request, or one of blanks only, records nothing. A request that holds the
   rendering of a message the session delivered, `(message id <id>)` with an
   id the cache counts as delivered, is that message, not the person's
   request, and records nothing. A request carries no id of its own, so the
   hook takes no event claim (contributor ADR 0016): the same request sent
   again is recorded again, and the request recorded already changes nothing,
   so two hook sources never send it twice. A Codex session sends none.
3. **A refusal never blocks the syncs.** A sync refused as invalid while it
   carries `lastPrompt` goes once more without it, as with `sessionUrl`
   (ADR 0012, decision 3). When that one passes and the refusal names
   `lastPrompt` or `last_prompt`, as a provider's own check does, the request
   counts as sent, so the syncs do not all go twice. When the refusal names
   another member, the next sync carries the request again.

## Rejected alternatives

- **Sync at `UserPromptSubmit`.** A sync is a call into the provider, inside a
  hook that must not delay the turn; the next sync is at most a turn away.
- **Keep a history of requests.** The mission needs the question next to the
  last answer, and a history grows the cache and the request for no reader.
- **Send the request with every sync.** It repeats up to 4096 characters on
  every tool call for no gain, and absent already keeps it.
- **Read the request from the transcript at Stop.** The transcript format is
  the host's, and the hook payload already names the request.

## Consequences

- A missions provider that keeps the request declares `lastPrompt` on `sync`.
  Until it does, the CLI drops the member and the mission shows no request.
  The hosted provider of `@putnami/cloud` declares it once its sync route
  accepts `last_prompt`.
- A provider that declares `lastPrompt` needs a CLI that knows it; an older
  CLI withdraws its `missions` version (ADR 0009, decision 4).
- The frozen version 1 corpus does not change: no frozen document carries the
  member.
- The hook runs in the background, so a turn shorter than the hook can end
  with a Stop sync that carries the request before; the request then goes
  with the next sync, a wait's included. Two requests sent faster than their
  hooks keep the one recorded last, and so does a request a second hook
  source records late, after a newer one.
- The person's request, as typed, leaves the machine for a provider that
  declares the member, as the agent's last message already does. A request
  that holds a secret reaches that provider; the hook does not redact it.
