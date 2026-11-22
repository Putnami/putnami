# ADR 0005 — Version 1 changes only by addition

- **Status**: Accepted
- **Date**: 2026-11-22
- **Amends**: [ADR 0001](0001-one-self-reported-change-evidence-document.md),
  the versioning of the document

## Context

The change evidence document leaves the repository: the observe extension
writes it in a CI job this repository does not run, and Putnami Cloud, or any
other reader, validates it. Until now the protocol and the extension were
`experimental`, so a reader could not rely on a document it accepted today
being accepted tomorrow, nor a producer on a document it wrote today still
validating. Adopters outside this repository need that promise before they
keep the document as a CI artifact or build a reader on it.

ADRs 0002 and 0004 already grew version 1 by addition: an optional member that
a document without it validates as before. The rule was a habit, not a
promise, and nothing failed when a change broke it.

## Decision

1. Version 1 of the document changes only by addition. An addition is a new
   optional member of a closed object, a new value of a closed vocabulary, a
   looser bound of the schema or of the strict reader, or a validation rule
   that refuses no document version 1 accepted before.
2. Everything else is a break, and a break opens version 2 instead of
   changing version 1: removing, renaming or retyping a member, requiring a
   member or no longer requiring one, removing a vocabulary value, changing a
   pattern, a const or a reference, narrowing a bound, constraining a member
   of an open object, and a validation rule that refuses a document version 1
   accepted. The required sets stay as they are, so a reader keeps every
   member it relies on.
3. A reader still updates before its producers write an addition: the strict
   reader refuses a member or a value it does not know
   (`evidence.unknown_field` and the closed-vocabulary codes). The promise is
   that the document a producer wrote yesterday stays valid, not that an old
   reader accepts a new document.
4. A test holds the promise. `fixtures/v1/` keeps a frozen copy of the schema
   as it was promised and frozen version 1 documents, each pinned by its
   digest; the test also pins each bound of the strict reader, which may only
   grow. The current schema must keep the dialect, every member with its type,
   const, reference and pattern, the same required sets and every vocabulary
   value, and keep or loosen every bound; each frozen document must validate.
   An addition updates none of them: a frozen file changes only when a pull
   request opens version 2.
5. `go.putnami.dev/protocol/evidence` and `@putnami/observe` move to
   `preview` in the support catalog. Preview allows the Go API of the module
   and the command's flags, text summary and note to change in a minor
   release that says so; the document follows this record.

## Consequences

- A producer and a reader of version 1 can rely on a document staying valid
  through every release of version 1.
- The `cases` and `omittedCases` members of ADR 0004 are part of the frozen
  schema: the promise covers them.
- A narrower bound, such as fewer test cases per entry, waits for version 2,
  however small it looks.
- `stable` still needs what preview does not give: a consumer outside this
  repository that reads the document, an N-1 read window or a migration for
  version 2, and an entry in the compatibility budget.
