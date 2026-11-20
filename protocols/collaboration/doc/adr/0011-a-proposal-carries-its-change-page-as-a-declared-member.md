# 0011: A proposal carries its change page as a declared member

- **Status**: accepted
- **Date**: 2026-11-20
- **Scope**: `go.putnami.dev/protocol/collaboration` (`protocols/collaboration`)
  and `change finalize` in `@putnami/contributor`
- **Applies**: [ADR 0007](0007-a-declared-result-member-may-join-version-1.md),
  decision 1

## Context

A proposal links its code host's page in `url`. A provider with its own
console also has a page for the change, where the owner approves
declarations, accepts gaps and decides findings. A build's end report cannot
link that page, because no member carries it.

## Decision

1. **`Proposal` gains `pageUrl`**, a display link to the proposal's change in
   the provider's own console. It is never an identity, and it follows the
   rule of every display link: absolute http or https, no credentials.
2. **It joins version 1 as a declared result member** (ADR 0007). The catalog
   lists it for every operation whose result carries a proposal:
   `proposals.find`, `upsert`, `status`, `merge` and `automerge`. A provider
   answers it only on an operation whose manifest declares it.
3. **A provider that declares it answers it on every proposal of that
   operation.** A provider with no console declares nothing and leaves it
   absent.
4. **`providertest` holds the promise.** A target names the member in
   `ProposalMembers`, and the proposal scenarios check that each proposal they
   read carries it exactly when it is declared.
5. **`change finalize` prints it** as `CHANGE_URL=<pageUrl>` after
   `PROPOSAL_URL`, and its result carries it as `changeUrl`.

## Rejected alternatives

- **Open `proposals` version 2.** Every provider and caller would select a new
  version for a member most providers cannot fill.
- **Reuse `url` for the console page.** `url` is the code host's link that
  readers already follow; replacing it hides the pull request.
- **List it on `status` alone.** `change finalize` reads a merged proposal
  through `find`, and a member that changes with the operation surprises a
  caller.

## Consequences

- The frozen version 1 corpus does not change.
- Filling `pageUrl` is each provider's own change; the providers in this
  repository declare nothing and are unchanged.
