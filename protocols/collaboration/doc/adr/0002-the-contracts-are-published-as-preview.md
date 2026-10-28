# 0002: The contracts are published as preview

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/collaboration` (`protocols/collaboration`)
- **Amends**: [ADR 0001](0001-collaboration-provider-contracts.md), decisions 1 and 2

## Context

ADR 0001 kept the module unpublished: providers in this repository consume it
by `replace`, and publication was left to a reviewed decision. A program
outside this repository cannot use `replace`. To serve or read tasks in the
contract's shape, it needs a published version to require.

A reader that lists tasks from several sources also misses two members.
`ref.id` is opaque, so the reader cannot show the identifier people use, such
as `#123` or `ENG-42`, or match it in a commit message. And a task says when it
last changed, not when it was created, which is the order a backlog sorts by.

## Decision

1. **The module is published.** `protocols/collaboration` publishes its Go
   module with the other protocols (`"publish": ["go"]`, no archives).
   Providers in this repository keep their `replace`, like every other module
   here.
2. **Its status is `preview`.**
   [`putnami.support.json`](../../../../putnami.support.json) records it. The
   CLI route and three providers use the contracts, but all of them live in
   this repository; `stable` needs a consumer across a boundary this
   repository does not control.
3. **Tasks v1 gains `key` and `createdAt`, both optional.** `key` is the
   identifier people read and write. It is for display and for matching a
   mention in text; `ref`, not `key`, names the task in a request, and a
   provider without such an identifier leaves `key` out. `createdAt` is the
   creation time in RFC 3339. The GitHub provider sets `#<number>` and the
   issue's creation time; the local provider sets its task id and the time of
   `create`.
4. **The version 1 documents are fixed from the first published module.** The
   parser refuses a member its Go type does not declare, so an older reader
   refuses every new member. From publication on, ADR 0001 decision 2 applies
   to any member: a new one is a new contract version. This is why `key` and
   `createdAt` land before the first published module. A contract's version 1
   is fixed from the first published module that contains it: the missions
   contract, added after the first publication, gains `stage`, `usage` and the
   `ended` state in version 1 because no published module contains it yet.

## Rejected alternatives

- **Keep the module unpublished and copy its types.** A copy drifts from the
  schemas the drift tests hold, and leaves a provider outside the repository
  without `providertest`.
- **Derive the key from `ref.id`.** The id is opaque by contract. A provider
  may change its encoding, and a tracker whose id is internal, such as a UUID,
  shows people a different string.
- **Publish as `stable`.** No consumer outside the repository has used the
  contracts yet, and the support catalog promotes one step at a time.

## Consequences

- A consumer outside this repository requires a published version and gets
  the types, the validators, `Serve` and `providertest`.
- A reader shows `key` when a task has one and the reference it holds when
  not.
- A new member of a version 1 document is no longer possible: it opens
  version 2 of its contract.
