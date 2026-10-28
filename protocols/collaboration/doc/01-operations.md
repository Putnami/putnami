# Operations

Every operation of every contract version this package implements. The
request document is the operation's `<operation>Input` definition in the
contract schema (`schemas/<contract>.v1.json`); the result, carried by an `ok`
envelope, is its `<operation>Result` definition. Shared definitions (`ref`,
`pageRequest`, `page`, `idempotencyKey`, …) live in `schemas/collaboration.json`.

**Access** is what a provider's `readOnlyHint` must say. **Destructive**
operations may modify existing state, so their `destructiveHint` is true.
**Preconditions** says what the provider declares: *declared* operations accept
an optional `expectedRevision` and the provider states `atomic`, `checked` or
`none`; *atomic* operations are offered only by a provider that enforces them
atomically.

## Common shapes

- **Reference** — `{"source": "<kind>:<locator>", "id": "<opaque>"}`. The
  source names the backend (`github:acme/app`, `local:3f9a0c1d`); the id is
  meaningful only to it. Store the pair and pass it back; never parse it.
- **Display URL** — `url`, optional, absolute `http`/`https`, never an identity
  and never carrying credentials.
- **Revision** — `revision`, an opaque token that changes whenever the item
  changes; `expectedRevision` compares against it. A task's `parent` is the
  one member a provider may leave outside it; such a provider says so in its
  `link` tool's description, which `capabilities` reports, and an
  `expectedRevision` then does not detect a concurrent `link`.
- **Page** — a list request takes `page: {"size", "cursor"}` (size 0–100;
  0 or absent means the default, 20); a list result carries `items` (never
  null; an empty page is `[]`) and `page.next`, absent on the last page. Items
  keep one stable provider-defined order across the pages of one traversal.
  A cursor names a position, not a page number, so a caller may change `size`
  between the pages of one traversal. A page holds at most `size` items, and
  fewer, with `page.next`, when `size` items would make the response larger
  than the 4 MiB document bound; `Serve` shortens such a page for a Go
  provider. The CLI refuses a page above the bound from a provider that does
  not shorten it (`unavailable`, `provider.invalid_response`) and says to ask
  for a smaller `page.size`. Every request, and every result of one item,
  whose members are within their bounds fits the document bound.
- **Idempotency key** — `idempotencyKey` (`[A-Za-z0-9._:/-]{1,128}`) on every
  operation that adds an item. A provider returns the item an earlier call
  with the same key produced (`created: false`, or `replayed: true`), and
  refuses the same key with other content as a `conflict` with reason
  `idempotency.mismatch`.

## tasks v1

| Operation | Access | Required | Destructive | Preconditions | Request | Result |
|---|---|---|---|---|---|---|
| `find` | read | yes | — | — | `query`, `states`, `labels` (all must match), `page` | `items: [task]`, `page` |
| `get` | read | yes | — | — | `ref` | `task` |
| `create` | mutating | yes | no | — | `title`, `body`, `labels`, `state` (default `open`), `idempotencyKey` | `task`, `created` |
| `update` | mutating | yes | yes | declared | `ref`, `expectedRevision`, `title`, `body`, `labels` (replaces the set); at least one change | `task` |
| `transition` | mutating | yes | yes | declared | `ref`, `state`, `expectedRevision`, `reason` | `task` (in the requested state) |
| `assign` | mutating | no | yes | declared | `ref`, `assignees`, `expectedRevision` | `task` |
| `link` | mutating | no | yes | — | `ref`, and `parent` or `unlink: true` | `task` |
| `claim` | mutating | no | yes | atomic | `ref`, `holder`, `release` | `task` (with `holder`), `held` |

A task is `ref`, `key`, `revision`, `url`, `title`, `body`, `state`,
`providerState`, `labels`, `assignees`, `parent`, `holder`, `createdAt`,
`updatedAt`. `state` is one of `open`, `in_progress`, `blocked`, `done`,
`canceled`; `providerState` is the backend's own label, for display. `key` is
the identifier people read and write, such as `#123` or `ENG-42`: it is for
display and for matching a mention in text, and `ref`, not `key`, names the
task in a request. An assignee is information, never an
exclusive lease: exclusivity is `claim`'s, offered only where the backend
enforces it.

## proposals v1

| Operation | Access | Required | Destructive | Preconditions | Request | Result |
|---|---|---|---|---|---|---|
| `find` | read | yes | — | — | `change` (`base`, `head`, optional `repository` and `headCommit`), `states`, `page` | `items: [proposal]`, `page` |
| `upsert` | mutating | yes | yes | declared | `change` (plus optional `headCommit`), `title`, `body`, `draft`, `expectedRevision` | `proposal`, `created` |
| `status` | read | yes | — | — | `ref` | `proposal`, `checks`, `reviews` |
| `review` | mutating | yes | no | — | `ref`, `verdict`, `body`, `commit`, `idempotencyKey` | `review`, `created` |
| `comments` | read | no | — | — | `ref`, `page` | `items: [comment]`, `page` |
| `merge` | mutating | no | yes | — | `ref`, `expectedHeadCommit`, `method` | `proposal` (state `merged`) |

A proposal is `ref`, `revision`, `url`, `change` (`repository`, `base`,
`head`, `headCommit`), `title`, `body`, `state` (`draft`, `open`, `merged`,
`closed`), `providerState`, `labels`, `assignees`, `updatedAt`. A request may
omit `repository`; the provider applies its configured one, and every
proposal it returns names it. `labels` and `assignees` include those the
provider applies from its own settings; a provider reports them on every
answer about a proposal (`[]` when there are none) or on none, and absent
means it does not report them. An assignee is information, never a lease.
`upsert` keeps at most one open proposal per exact repository/base/head:
repeating it never creates a second.
A `headCommit` on `find` names the commit the caller pushed to the head. A
provider whose proposal head follows the pushed ref answers an open proposal
still behind a head ref that already points at that commit only once its head
catches up, so its revision covers that commit; one that stays behind is
`unavailable` and retryable. A provider that records the head only on `upsert`
answers the proposal as recorded.

`checks.state` is `pending`, `passing`, `failing`, `none` (supported, none
reported) or `unsupported` (the provider has no hosted checks; `detail` says
so, and no check is listed). A review is `ref`, `url`, `verdict` (`approve`,
`comment`, `request_changes`), `commit`, `author`, `submittedAt`. `merge`
refuses a head that moved from `expectedHeadCommit`; offering it authorizes
nobody to merge.

`comments` lists what people wrote on one proposal. A comment is `ref` and
`body`, plus, when known, `url`, `author`, `submittedAt`, `review` (the ref of
the review it belongs to) and the file line it is anchored to: `path`, `line`
(the first line is 1) and `commit`, the commit whose file `line` refers to,
which is not necessarily the commit the comment was written on. A comment on
no file has no `path`. A comment on a whole file, and one whose line was
deleted or is no longer in the change, has a `path` and no `line`. A `line`
never comes without its `path`. The comment and its review are in the source
of the proposal. A provider that offers `comments` lists the body of a
published review once across pages, naming that review.

## memory v1

| Operation | Access | Required | Destructive | Preconditions | Request | Result |
|---|---|---|---|---|---|---|
| `context` | read | yes | — | — | `identity`, `kinds`, `projects` or `impacted`/`baseline`, `page` | `items: [record]`, `page` |
| `mission` | read | yes | — | — | `mission`, `identity` | `record` |
| `checkpoint` | mutating | yes | yes | atomic | `mission`, `identity`, `precondition`, `idempotencyKey`, `title`, `content`, `sources`, `evidence` | `record`, `replayed` |
| `search` | read | no | — | — | `query`, and what `context` takes | `items: [record]`, `page` |

A record is `ref`, `revision`, `kind` (`mission`, `note`), `identity`
(`workspace`, `repository`, `scope`, `mission`), `title`, `content` (at most
64 KiB), `sources` (references), `evidence`, `provenance` (`recordedAt`,
`recordedBy`) and `freshness` (`updatedAt`, `retrievedAt`). The orchestrator
fills `identity.workspace` from the workspace it serves, so a skill never
names storage.

`precondition` is exactly one of `expectedRevision` (the mission must be at
this revision) and `mustNotExist: true` (create it). A write without one is
refused before the provider runs. A successful checkpoint returns a new
revision; a stale one is a `conflict` with reason `revision.conflict`, and a
`mustNotExist` checkpoint of a mission that exists is a `conflict` with reason
`record.exists`; both carry the current revision. `evidence`
entries (`kind`: `gate`, `review`, `qualify`, `session`, `other`; `locator`;
optional `digest`) reference records; memory never carries a verdict, and a
checkpoint proves nothing the referenced record does not.

`context` and `search` take the canonical selection arguments. The
orchestrator resolves them through its own selection projection and hands the
provider the resolved `selection` and the workspace membership; the provider
tool must declare `workspaceSelection`.

## missions v1

| Operation | Access | Required | Destructive | Preconditions | Host-only | Request | Result |
|---|---|---|---|---|---|---|---|
| `sync` | mutating | yes | yes | — | yes | `session`, `mission`, `event`, `state`, `observedAt`, `model`, `branch`, `lastMessage`, `error`, `waitUntil`, `plan`, `delivered`, `decisions`, `replies`, `stage`, `usage` | `session`, `mission`, `inbox`, `decisions`, `replies`, `minIntervalMs`, `now` |
| `ask` | mutating | yes | no | — | no | `mission`, `question`, `context`, `options`, `recommendation`, `blocks`, `idempotencyKey` | `decision`, `replayed` |
| `reply` | mutating | yes | no | — | no | `message`, `reply` | `message`, `replayed` |

A mission is named by `{repository, key, title}`. `repository` and `key` are
the memory identity's `repository` and `mission`, and both are required.
Together they are the mission's identity, compared exactly. `repository` is
lowercase, such as `owner/name`: a host sends it so, and a name with an
uppercase letter is refused. `title` is for display, is not part of the
identity, and follows the task title bound. A session is `{engine, id}`:
`engine` is `[a-z0-9-]{1,32}` and names the host that runs the agent, and `id`
is that host's own session id, `[A-Za-z0-9._:-]{1,128}`.

A **host-only** operation is one the host that runs an agent calls, never the
agent. The MCP server lists no tool for it, and `missions.capabilities` marks
it `hostOnly: true`. `putnami missions sync` and the host channel's
`collaboration.call` route it like any other operation. The agent sees
`missions.ask` and `missions.reply` as tools. It takes the mission's
repository and key, and the id of each message, from the session sheet that
the session hook prints.

A message or decision id is a bare token, 1 to 64 characters of
`[A-Za-z0-9_-]`, not a reference. One binding means one provider, so an id
needs no source, and an agent types it. An option id is 1 to 64 characters of
`[A-Za-z0-9._-]`.

**sync** publishes a session and reads the mission back. `state` is
`working`, `handed_back`, `waiting`, `failed` or `ended`: `ended` means the
host closed the session or the user quit. `waitUntil` is required with
`waiting` and refused with any other state; `error` goes only with `failed`.
`event` names the host event that caused the sync, `[a-z_]{1,32}`; a provider
records it and never branches on it. `observedAt` is an RFC 3339 UTC time. A
provider applies `state`, `stage`, `usage` and `plan` only from a sync
observed later than the last one it applied for the session, so an older sync
never overwrites a newer one. An absent `plan` leaves the plan unchanged; a
plan with no steps clears it. A plan holds at most 100 steps, each `{key,
status, text}`: `key` is a token, unique in its plan, and `status` is `todo`,
`doing` or `done`.

`stage` is the stage of the contribution loop the session's change is at:
`talk`, `declare`, `show`, `code`, `check`, `prove`, `validate` or `merge`.
`usage` is the tokens the session used so far, cumulative for the whole
session, its subagents included: `{inputTokens, outputTokens,
cacheWriteTokens, cacheReadTokens}`, each an integer from 0 to 10^12.
`inputTokens` counts the input read neither from nor into the prompt cache.
A provider keeps the `stage` and the `usage` of the last applied sync that
carried them: an absent one keeps what it holds, and a present `usage`
replaces it whole.

- `delivered` (at most 100) lists the inbox messages the host gave the agent;
  they leave the inbox. Delivering an answer message moves its decision to
  `acknowledged`.
- `decisions` (at most 20) lists the decisions the session asked. Sending them
  binds them to the session, and the result reports their state.
- `replies` (at most 20) carries the agent's replies, `{message, reply}`, one
  per message.

`delivered` and `decisions` are sets: a repeated id counts once. A provider
ignores an id in either list that it does not know or that is not the
caller's, such as another person's. An id from another mission of the caller
applies, so a session that changed mission still acknowledges what it
received before. The ids a sync carries apply whatever its age.

The result's `replies` has one entry per reply the sync carried: `replied`,
or `refused` with a `reason`. A refused reply is final: the host drops it,
and it never refuses the sync. The reason is `message.closed` (the message
holds another reply), `message.not_found` (the provider does not know the
message) or `message.denied` (the caller may not answer it). The result's
`decisions` has one entry for each decision the sync named that the provider
holds for the caller, as `open`, `answered` or `acknowledged`.

The result's `mission` is `{key, title, url, plan, openDecisions}`. It always
describes the mission the sync named, whatever the sync's age. A provider
that does not hold that mission answers `{key, title: "", plan: {steps: []},
openDecisions: []}`. `plan` is the mission's current plan: the plan that the
session that published last sent, and empty when none did. `openDecisions`
holds at most 50 decisions that no one answered yet. `inbox` holds at most 50
messages, oldest first, each `{id, kind, text, createdAt}`. `kind` is `info`,
`prompt` or `answer`, and an `answer` carries `decision: {id, question,
answer, option}`, where `option` is the id of the option the owner chose, when
the answer chose one. On an answer, `decision.answer` is the answer and `text`
is the provider's display of it. The texts, questions and answers of the
inbox total at most 64 KiB. `minIntervalMs`, from 5000 to 600000, is the pace
of routine syncs: a host may sync sooner at session start, at stop and during
a wait, and a provider does not refuse those syncs. `now` is the provider's
clock. The result carries no memory checkpoint: a host reads `memory.mission`
for it.

**ask** records a decision for the mission's owner: a `question` (at most 500
characters), `context` (at most 4000), 2 to 5 `options` `{id, label,
consequences}`, an optional `recommendation` `{option, reason}` that names an
option, and `blocks`, what the decision holds up. The result is `decision:
{id, state, url, answer, option}`; a new decision is `open` with no answer.
A decision moves from `open` to `answered` when the owner answers, and to
`acknowledged` when a sync delivers the message that carries the answer.
`idempotencyKey` is required. An ask's content is `mission.repository`,
`mission.key`, `question`, `context`, `options`, `recommendation` and
`blocks`; `mission.title` is not part of it. The same key with the same
content returns the decision that key recorded, with `replayed: true`, in any
state and with its answer once there is one; the same key with other content
is a `conflict` with reason `idempotency.mismatch`. `ask` takes no session: a
session binds a decision through `sync.decisions`.

**reply** records the agent's reply to one inbox message, at most 4000
characters. The message is its natural key: the same reply again replays, and
another reply is a `conflict` with reason `message.closed`. An unknown message
is `not_found` with reason `message.not_found`, and a message the caller may
not answer is `denied` with reason `message.denied`. The result is `message:
{id, state: "replied"}`. `reply` takes no session: a session claims a message
through `sync.delivered` and `sync.replies`.

A rate limit is `unavailable` and retryable. An unresolved `sync`, `ask` or
`reply` is reconciled by repeating the same document.

## Failures

| Outcome | Written? | Retryable | Carries |
|---|---|---|---|
| `not_found` | no | no | — |
| `conflict` | no | after re-reading | `current` revision when known |
| `unsupported` | nothing ran | no | — |
| `invalid` | nothing ran | no | — |
| `denied` | no | no | — |
| `unavailable` | no | as stated | — |
| `unresolved` | unknown | never | `reconcile` |

A read never answers `conflict` or `unresolved`. A mutation whose provider
process started and did not deliver a valid answer is `unresolved`; the
`reconcile` hint names the read (or the idempotent repeat) that settles it:
prose that spells the operation `<contract>.<operation>` of the same
contract (`run proposals.find for the same change`), so automation finds it
with `collab.ReconcileOperation` and a person reads the rest. A hint that
names no operation of the contract is refused.
