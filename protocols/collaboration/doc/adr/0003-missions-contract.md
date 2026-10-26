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
   workspace binds memory. `ask` and `reply` take no session: the agent does
   not know its host's session id. A session binds a decision through
   `sync.decisions`, and claims a message through `sync.delivered` and
   `sync.replies`.
2. **Host-only operations.** `OperationSpec.HostOnly` marks an operation the
   host that runs an agent calls, never the agent. The MCP server lists no
   tool for it. `putnami <contract> <operation>` and the host channel's
   `collaboration.call` route it like any other operation, through the same
   `mcp.CallProviderOperation`, to the same envelope. Only `missions.sync` is
   host-only. Only a mutation can be: a read has no reason to hide from an
   agent. This amends ADR 0001 decision 5, under which every operation the
   binding serves was an MCP tool.

   The capabilities document marks such an operation `hostOnly: true`, so an
   agent that reads `missions.capabilities` does not try to call `sync`. The
   member is optional and absent when false, so it appears only on
   `missions.sync`. ADR 0002 decision 4 still holds: the `tasks`, `proposals`
   and `memory` capabilities documents do not change, and a reader that does
   not know `missions` already refuses its documents as an unknown contract.
3. **Natural keys for `sync` and `reply`.** ADR 0001 decision 6 asks every
   operation that adds an item for an idempotency key. Two missions
   operations use a natural key instead:
   - `sync` is keyed by the session and `observedAt`. A provider applies a
     session's state and plan only from a sync observed later than the last
     one it applied. The ids a sync carries apply whatever its age: each is
     keyed by its own id, so a repeat changes nothing. The mission view in
     the result always describes the mission the sync named, whatever its
     age, so a host can check the answer against its own request.
   - `reply` is keyed by the message. The same reply again replays. Another
     reply is a `conflict` with reason `message.closed`. An unknown message
     is `not_found` with reason `message.not_found`, and a message the
     caller may not answer is `denied` with reason `message.denied`.

   `ask` keeps the rule: `idempotencyKey` is required. An ask's content is
   `mission.repository`, `mission.key`, `question`, `context`, `options`,
   `recommendation` and `blocks`; the mission's title is not part of it. The
   same key with the same content replays in any state of the decision, with
   its answer once there is one; the same key with other content is a
   `conflict` with reason `idempotency.mismatch`. An unresolved call to any
   of the three is reconciled by repeating it.
4. **Ids are bare tokens.** A message or decision id is 1 to 64 characters of
   `[A-Za-z0-9_-]`, not a `Ref{source, id}`. The agent types these ids in
   tool arguments and in acknowledgement lines, so they must be short. One
   binding means one provider, so an id needs no source. This is the one
   exception to the rule that an item is named by a reference. An option id
   is 1 to 64 characters of `[A-Za-z0-9._-]`, and a session id, which the
   host chooses, is 1 to 128 characters of `[A-Za-z0-9._:-]`. Both stay
   safe in a line, a path or a URL.
5. **A sync never fails for one bad member.** `delivered` and `decisions`
   are sets: a repeated id counts once. A provider ignores an id in either
   list that it does not know or that is not the caller's, such as another
   person's. An id from another mission of the caller applies, so a session
   that changed mission still acknowledges what it received. A reply it
   refuses gets its own entry in the result, `refused` with a reason, and
   the sync still applies. A refused reply is final: the host drops it. The
   reason is one of `message.closed`, `message.not_found` and
   `message.denied`. `replies` still refuses a repeated message, because two
   texts for one message conflict.
6. **Bounds reuse the shared ones.** The mission's `repository` and `key`
   follow the memory identity's `repository` and `mission` rules, the title
   follows `MaxTitleLength`, and URLs use the shared display-link check.
   `repository` is required and lowercase, such as `owner/name`, and the
   pair is compared exactly; the title is for display. A plan step key is a
   token, like a mission key. The members only this contract has get their
   own bounds in `validate.go`. Lengths count characters. The inbox holds at
   most 50 messages, and their texts, questions and answers total at most
   64 KiB. A `waiting` session carries `waitUntil`; no other state does.
7. **No new envelope member.** A rate limit is `unavailable` and retryable,
   with no `retryAfter`. The result's `minIntervalMs` is the pace of routine
   syncs. A host may sync sooner at session start, at stop and during a
   wait, and a provider does not refuse those syncs.
8. **The route checks identity.** An ok `sync` answer must name the mission
   key the sync named, report only decisions the sync named, and answer each
   reply it carried exactly once. A provider that does not hold the named
   mission answers its empty view, not the session's current mission. An ok
   `reply` answer must name the message. A new `ask` answer must be an open
   decision. Any other answer is `unresolved` with reason
   `provider.identity_mismatch`.
9. **Shared conformance scenarios.** `providertest.RunMissions` runs against
   any provider that can post an inbox message and answer a decision for a
   test. It checks ask replay and `idempotency.mismatch`, that the title is
   not part of an ask's content, replay after an answer, that a delivered
   answer acknowledges its decision, that open decisions shrink when one is
   answered, reply replay, `message.closed` and `message.not_found`,
   delivery, unknown and repeated ids in a sync, an acknowledgement after
   the session changed mission, one entry for each refused reply, that an
   older sync never overwrites a newer one, and that an older sync still
   answers the mission it named. The scenarios run as one caller, because a
   `Target` has no way to call as another person. So they do not check that
   a sync ignores another caller's id, or that `reply` answers
   `message.denied`. A provider that serves more than one caller tests both
   itself.

## Rejected alternatives

- **`Ref{source, id}` for messages and decisions.** An agent would type a
  source it cannot know, and every acknowledgement line would grow.
- **Replies only through `reply`.** A host that stopped would then need one
  call per reply. A refused reply cannot block a sync, because it gets its
  own entry.
- **Refuse a whole sync for an unknown or repeated id.** One stale id would
  stop every later state change of the session.
- **A session on `ask` and `reply`.** The agent calls these tools and does
  not know its host's session id; it would guess one. The host already
  names the session on every sync, which binds what the agent asked and
  answered.
- **A free reason for a refused reply.** Each provider would pick its own
  code, and a host could not tell a closed message from an unknown one.
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
