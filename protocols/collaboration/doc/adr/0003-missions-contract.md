# 0003: The missions contract

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/collaboration` (`protocols/collaboration`)
  and the CLI route that calls it
- **Amends**: [ADR 0001](0001-collaboration-provider-contracts.md), decisions 5
  and 6

## Context

An agent session reports to a mission: its state, its plan, and the messages
it read. The mission's owner answers from elsewhere: a message for the agent,
or the answer to a decision the agent asked for. Today the host hooks speak
one backend's HTTP API. A workspace cannot choose another backend, and no
test holds the hooks and the backend to one shape.

The three contracts of ADR 0001 do not fit this traffic. A task is a work
item, not a session. Memory keeps a mission's checkpoint, not its inbox. And
the hook that publishes a session runs on every tool call, so it must be
cheap to repeat and must never show the agent a tool it should not call.

## Decision

1. **A fourth contract, `missions`, version 1.** It has three operations,
   all required and all mutations:
   - `sync` publishes a session's state and plan, reports the inbox messages
     the host gave the agent, and carries the agent's replies. Its answer is
     the mission view, the inbox and the state of the decisions the session
     asked. There is no read operation: `sync` is the read path.
   - `ask` records a decision for the mission's owner, with 2 to 5 options.
   - `reply` records the agent's reply to one inbox message.

   No operation takes preconditions or the workspace selection. No document
   carries a workspace: the binding decides it. The result of `sync` carries
   no memory checkpoint; a host that needs it reads `memory.mission` when the
   workspace binds memory.
2. **Host-only operations.** `OperationSpec.HostOnly` marks an operation the
   host that runs an agent calls, never the agent. The MCP server lists no
   tool for it. `putnami <contract> <operation>` and the host channel's
   `collaboration.call` route it like any other operation, through the same
   `mcp.CallProviderOperation`, to the same envelope. Only `missions.sync` is
   host-only. Only a mutation can be: a read has no reason to hide from an
   agent. This amends ADR 0001 decision 5, under which every operation the
   binding serves was an MCP tool.
3. **Natural keys for `sync` and `reply`.** ADR 0001 decision 6 asks every
   operation that adds an item for an idempotency key. Two missions
   operations use a natural key instead:
   - `sync` is keyed by the session and `observedAt`. A provider applies a
     session's state and plan only from a sync observed later than the last
     one it applied. The ids a sync carries apply whatever its age: each is
     keyed by its own id, so a repeat changes nothing.
   - `reply` is keyed by the message. The same reply again replays. Another
     reply is a `conflict` with reason `message.closed`. An unknown message
     is `not_found`.

   `ask` keeps the rule: `idempotencyKey` is required. The same key with the
   same content replays in any state of the decision; the same key with
   other content is a `conflict` with reason `idempotency.mismatch`. An
   unresolved call to any of the three is reconciled by repeating it.
4. **Ids are bare tokens.** A message or decision id is 1 to 64 characters of
   `[A-Za-z0-9_-]`, not a `Ref{source, id}`. The agent types these ids in
   tool arguments and in acknowledgement lines, so they must be short. One
   binding means one provider, so an id needs no source. This is the one
   exception to the rule that an item is named by a reference.
5. **A sync never fails for one bad member.** A provider ignores an id in
   `delivered` or `decisions` that it does not know or that belongs to
   another person's mission. A reply it refuses gets its own entry in the
   result, `refused` with a reason, and the sync still applies.
6. **Bounds reuse the shared ones.** The mission's `repository` and `key`
   follow the memory identity's `repository` and `mission` rules, the title
   follows `MaxTitleLength`, and URLs use the shared display-link check. The
   members only this contract has get their own bounds in `validate.go`.
   Lengths count characters. The inbox holds at most 50 messages, and their
   texts, questions and answers total at most 64 KiB.
7. **No new envelope member.** A rate limit is `unavailable` and retryable,
   with no `retryAfter`; the result's `minIntervalMs` tells a host how often
   to sync.
8. **The route checks identity.** An ok `sync` answer must name the mission
   key the sync named, report only decisions the sync named, and answer each
   reply it carried exactly once. An ok `reply` answer must name the message.
   A new `ask` answer must be an open decision. Any other answer is
   `unresolved` with reason `provider.identity_mismatch`.
9. **Shared conformance scenarios.** `providertest.RunMissions` runs against
   any provider that can post an inbox message for a test. It checks ask
   replay and `idempotency.mismatch`, reply replay, `message.closed` and
   `not_found`, delivery, unknown ids in a sync, one entry for each refused
   reply, and that an older sync never overwrites a newer one.

## Rejected alternatives

- **`Ref{source, id}` for messages and decisions.** An agent would type a
  source it cannot know, and every acknowledgement line would grow.
- **Replies only through `reply`.** A host that stopped would then need one
  call per reply. A refused reply cannot block a sync, because it gets its
  own entry.
- **Refuse a whole sync for an unknown id.** One stale id would stop every
  later state change of the session.
- **A `retryAfter` envelope member.** It would change the envelope of every
  contract. `minIntervalMs` already paces a host.

## Consequences

- A host speaks one surface, and a workspace moves its missions to another
  backend by editing `options.collaboration.missions`.
- The agent sees `missions.ask` and `missions.reply` as tools, and never
  `missions.sync`.
- ADR 0002 decision 4 fixes the version 1 documents of a contract once it is
  published. A new contract changes the vocabulary: an older CLI or reader
  refuses `missions` in `options.collaboration` and in an envelope, as an
  unknown contract. A workspace that binds `missions` needs a CLI that knows
  it.
