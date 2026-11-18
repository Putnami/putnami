# 0007: A result member a provider declares may join version 1

- **Status**: accepted, amended by
  [ADR 0009](0009-a-declared-request-member-may-join-version-1.md): decision 1.
  A request member a provider declares may join version 1.
- **Date**: 2026-11-16
- **Scope**: `go.putnami.dev/protocol/collaboration` (`protocols/collaboration`),
  the `members` of a provider operation in `go.putnami.dev/protocol/extension`,
  and the CLI that reports capabilities
- **Amends**: [ADR 0006](0006-version-1-is-stable.md), decision 2 ("A new
  member … opens version 2")

## Context

A task names its parent and nothing else about the work around it. A reader
that shows an issue cannot say how far its epic is, which open issues block it
or which ones it blocks, or how much discussion it has, without calling each
tracker's own API.

ADR 0006 decision 2 opens a new contract version for any new member, because
the documents are strict: a reader that does not know a member refuses the
document that carries it. A `tasks` version 2 that only adds optional members to
one result would make every provider and every caller declare and select a new
version for members most providers cannot fill.

The danger decision 2 guards against is a document an older reader misreads or
refuses. A member that a provider sends only after it declares it, in a
declaration an older reader refuses as a whole, never reaches an older reader.

## Decision

1. **An operation's result may gain an optional member in version 1 when the
   catalog lists it as a declared member of that operation.** A provider
   returns the member only when its manifest names it in the operation's
   `members`. A request member, a required member, a narrowed value and a new
   outcome still open a new version, and every version 1 document stays valid
   with the same meaning.
2. **`tasks.get` declares four such members**: `parentProgress` (the
   parent's key, title, link, and how many of its children are done out of
   how many), `blockedBy` and `blocking` (the linked tasks, each with its
   reference, key, title, link and provider-neutral state) and `comments` (the
   comment count, the last comment's author and time, and a link to the
   comments; no comment body). Absent means the provider does not report the
   member; an empty list or a zero count means there is none.
3. **The members describe other items, so they are outside the task's
   revision.** They may change without a revision change, and an
   `expectedRevision` does not detect such a change, as for `parent` on a
   provider whose `link` says so.
4. **A provider operation's `members` raises the manifest's contract to 24.**
   A CLI that reads contract 24 validates each declared member against the
   catalog and reports it in the operation's capabilities; an older CLI
   refuses the stamp instead of loading a provider whose answers it would
   refuse.
5. **`providertest` holds the promise.** A provider's test names the members
   its manifest declares: `get` returns each of them and never another, and
   no other operation returns any.

## Rejected alternatives

- **Open `tasks` version 2.** It costs every provider and caller a version
  selection for members that are optional in every provider.
- **State the members in the operation's description, as `parent` is.**
  Prose is not something a caller or a conformance test can read.
- **Let any operation that answers a task return them.** A list of tasks with
  the context of each one costs a provider several backend reads per item, and
  a reader that needs the context reads the one task it shows.

## Consequences

- A caller reads `capabilities` to know which context members a provider
  fills, and reads `tasks.get` to get them.
- The frozen version 1 corpus does not change: no frozen document carries a
  new member.
- Filling the members in a provider is that provider's own change.
