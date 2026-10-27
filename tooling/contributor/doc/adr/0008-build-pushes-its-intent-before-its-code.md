# ADR 0008 — Build pushes its intent before its code

- **Status**: accepted
- **Scope**: `@putnami/contributor` `build` skill and its loop
- **Follows**: [ADR 0003](0003-build-publishes-its-proposal-by-default.md),
  which opens the draft proposal and pushes a checkpoint at each milestone

## Context

ADR 0003 opens the proposal as a draft once the plan is settled and pushes a
checkpoint at each milestone. Nothing fixes what the first checkpoint holds.
The declarations of an outcome (its feature declarations, specs,
architecture manifests and decision records) and the code that implements
them usually arrive together. A reviewer first sees a spec beside the code
that already implements it, so correcting the intent costs a rewritten
change.

The agent also had no way to read what a reviewer wrote. `proposals.status`
reports each review's verdict, commit and author, not its text.

## Decision

### 1. The first checkpoint holds the declarations only

When the outcome changes a declaration, `build` commits the declarations
before any implementation and pushes that commit to the draft proposal as
its first checkpoint. A declaration is a file of one of these kinds:

- `putnami.features.json`
- `specs/*.json` and `specs.baseline.json`
- `putnami.architecture.json`
- `decisions.json`
- a decision record (ADR)

A change that touches no declaration keeps the ADR 0003 flow.

### 2. The code goes on; the review is read at each checkpoint

`build` does not wait for an approval. Before each later checkpoint, it reads
the reviews with `proposals.status` and what reviewers wrote with
`proposals.comments`. It handles a comment on a declaration before the next
code checkpoint and answers it on the proposal with `proposals.review`, verdict
`comment`.

### 3. A declaration changed after its approval gets its own commit

An approval names the commit it reviewed. A declaration that changes after an
approval is pushed in a commit that touches declarations only, so the reviewer
sees what changed since the approval.

### 4. The proposals contract lists comments

`proposals.comments` is an optional read operation of the proposals contract.
It lists, in bounded pages, the review bodies, line comments and conversation
comments of one proposal, each with the review and the file line it belongs
to when known. The GitHub provider offers it. A provider that does not offer
it answers `unsupported`, and `build` reports that it cannot read comments.

## Consequences

- A drift in intent costs one declaration edit, not a rewritten change.
- The squash merge still yields one commit: the order of commits on the
  branch is for review only.
- The ready finalization is unchanged: the proposal turns ready for review
  once the final local gate and dossier pass.

## Alternatives rejected

- Blocking the code on an approval: a reviewer's delay would stall the run,
  and most intents are approved as written.
- A separate intent proposal: two proposals for one intent cost the reviewer
  more than one commit order does.
- Carrying review text in `proposals.status`: a status answer holds up to 100
  reviews in one document, so their text would not fit its 4 MiB bound
  without truncation. A paged operation has no such limit.

## Reversal

Remove the intent-first rule from `build` and its loop. `proposals.comments`
stays: it is an optional operation with its own uses.
