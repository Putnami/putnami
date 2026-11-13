# ADR 0003 — The decision records a change concerns

- **Status**: Accepted
- **Date**: 2026-11-07

## Context

Teams keep their architecture decisions as Markdown files, most often in the
MADR format (Markdown Architectural Decision Records) under `docs/decisions`
or `docs/adr`. A decision governs part of the repository: a reviewer of a
change wants to see the decisions that govern the files it touches, and to
notice a change that edits or deletes a decision.

ADR 0001 keeps spec, architecture and task evidence out of version 1. A
decision record a change concerns is narrower: it is a file of the
repository, read the way a lockfile is, and it carries no verdict.

## Decision

1. The document takes an optional `decisions` member: one entry per decision
   record the change concerns, that is a record whose scope matches a
   changed file, or a record the change adds, modifies, renames or deletes.
   A document without the member validates as before; the protocol version
   stays 1.
2. An entry names the record's path, its title, its status, its date when the
   record states one, the path patterns of its scope, the changed files the
   scope matches, and the record's own file status when the change touches
   it.
3. The status is a closed vocabulary, the MADR statuses: proposed, accepted,
   rejected, deprecated and superseded, and unknown for a record that states
   none the producer recognizes.
4. The producer reads the records and matches the scope; the document only
   reports the result. A reader does not re-match the patterns, and no record
   is required: a repository without decision records writes no member.
5. The entries are consistent with the diff, like the dependency changes: the
   governed files and a touched record are changed files, and an entry that
   neither governs a changed file nor is touched is refused.

## Consequences

- Observe, or any other producer, can list the decisions a change concerns
  without a protocol version, and a reader validates them with the same
  parser and codes.
- Adding a status is a protocol change; a record format other than MADR is a
  producer change.
- A verdict on a decision, such as a deleted accepted decision, stays a
  producer diagnostic: the document still carries no verdict.
- Spec, architecture models and task evidence beyond the decision records a
  change concerns stay out of version 1.
