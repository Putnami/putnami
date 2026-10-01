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

A task is `ref`, `revision`, `url`, `title`, `body`, `state`, `providerState`,
`labels`, `assignees`, `parent`, `holder`, `updatedAt`. `state` is one of
`open`, `in_progress`, `blocked`, `done`, `canceled`; `providerState` is the
backend's own label, for display. An assignee is information, never an
exclusive lease: exclusivity is `claim`'s, offered only where the backend
enforces it.

## proposals v1

| Operation | Access | Required | Destructive | Preconditions | Request | Result |
|---|---|---|---|---|---|---|
| `find` | read | yes | — | — | `change` (`base`, `head`, optional `repository`), `states`, `page` | `items: [proposal]`, `page` |
| `upsert` | mutating | yes | yes | declared | `change` (plus optional `headCommit`), `title`, `body`, `draft`, `expectedRevision` | `proposal`, `created` |
| `status` | read | yes | — | — | `ref` | `proposal`, `checks`, `reviews` |
| `review` | mutating | yes | no | — | `ref`, `verdict`, `body`, `commit`, `idempotencyKey` | `review`, `created` |
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

`checks.state` is `pending`, `passing`, `failing`, `none` (supported, none
reported) or `unsupported` (the provider has no hosted checks; `detail` says
so, and no check is listed). A review is `ref`, `url`, `verdict` (`approve`,
`comment`, `request_changes`), `commit`, `author`, `submittedAt`. `merge`
refuses a head that moved from `expectedHeadCommit`; offering it authorizes
nobody to merge.

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
