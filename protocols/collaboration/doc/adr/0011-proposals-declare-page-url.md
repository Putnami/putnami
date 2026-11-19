# 0011: Proposals declare a page link in version 1

- **Status**: accepted
- **Date**: 2026-11-19
- **Scope**: `go.putnami.dev/protocol/collaboration` (`protocols/collaboration`),
  the CLI that reads provider declarations, `change finalize` of
  `@putnami/contributor`, and every proposals provider
- **Applies**: [ADR 0007](0007-a-declared-result-member-may-join-version-1.md),
  decision 1, to the proposals contract

## Context

A proposal names its pull request in `url`. A platform that hosts the change
has a page of its own for it, where the owner approves the declarations,
accepts the gaps of the hosted verdict and decides the review findings. The
build workflow ends with a link to the pull request only, so the owner finds
that page by hand. Proposals version 1 is stable (ADR 0006); ADR 0007 lets a
result gain an optional member a provider declares.

## Decision

1. **`pageUrl` is a declared result member of the proposals operations that
   answer a proposal**: `find`, `status`, `upsert`, `merge` and `automerge`.
   It is a display link to the proposal's change in the provider's own
   console, an absolute `http` or `https` link without credentials, like
   `url`, and never an identity.
2. **All or none.** A provider declares `pageUrl` on every one of those
   operations it offers, or on none; a declaration on some of them withdraws
   its proposals version. A provider that declares it answers it on every
   proposal it returns.
3. **`pageUrl` is outside the proposal's revision.** The page belongs to the
   provider's console, which may move it, and a provider that starts
   answering it does not change the proposal. An `expectedRevision` does not
   detect a change of the link.
4. **`change finalize` prints it.** Its result carries `changeUrl`, and its
   text output prints `CHANGE_URL=<url>` after `PROPOSAL_URL`; the build
   workflow's end report leads with it.

## Rejected alternatives

- **An unconditional optional member.** A CLI that does not know it refuses
  the answer that carries it, after the provider wrote the proposal; ADR 0007
  moves that refusal to the declaration, before any call.
- **Declare it on `status` only.** A caller that finds or upserts a proposal
  would need one more read for a link the provider already has.
- **Derive the page from `url`.** The page's address is the provider's own;
  a caller cannot build it.

## Consequences

- A CLI older than this version withdraws the whole proposals version 1 of
  a provider that declares `pageUrl`, and declaring members needs
  `cliContract` 24. A provider declares it only once its consumers' CLI pins
  know the member.
- The `@putnami/cloud` provider fills it in a change of its own: its Source
  API schema gains `pageUrl` once Source answers it, its manifest declares it
  on the five operations, and a `providertest.RunProposals` target checks it.
- The frozen version 1 corpus does not change: no frozen document carries the
  member.
