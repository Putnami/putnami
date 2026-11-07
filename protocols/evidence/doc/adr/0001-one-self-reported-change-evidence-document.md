# ADR 0001 — One self-reported change evidence document

- **Status**: Accepted
- **Date**: 2026-11-01

## Context

Teams that keep their own CI (they build, test, package and publish without
Putnami) want to understand and control their changes with Putnami. Putnami
must read what those pipelines already produce: the git diff, JUnit and
coverage reports, lockfiles. The analysis runs in the CI job, inside the
open-source CLI, so source code never leaves it; Putnami Cloud, or any other
reader, receives a summary.

The existing documents do not fit. `protocol/cli` sessions and
`protocol/ci` change plans identify work by Putnami project, task, extension
and cache key, which a repository outside Putnami does not have.

## Decision

1. One new document, the change evidence document, carries one change: its
   subject (code host, repository, commit, base, change request), its CI run,
   the changed files, the components they belong to and the components that
   depend on them, tests and coverage per component, the coverage of the
   changed lines, the dependency changes read from lockfiles, and the reader
   diagnostics.
2. The subject keys the repository by host and numeric project id when the
   code host has one: the path changes on a rename or a transfer, the id does
   not.
3. A component is a repository-relative directory, inferred from an
   orchestrator export, a package-manager workspace, a manifest or a fallback,
   or declared by hand. The source is recorded so a reader can weigh it.
   Ecosystems stay open slugs: new stacks arrive without a protocol version.
4. The document carries no verdict and no trust claim. Whatever produced it,
   a reader treats it as self-reported.
5. The document is strict, internally consistent and bounded, like the other
   protocols: unknown members fail closed, every reference resolves, every
   count adds up, every list has a cap.

## Consequences

- Producers outside Putnami can write the document; a reader validates it
  with the same parser and codes.
- Adding a closed vocabulary value (a code host, a CI system) is a protocol
  change; adding an ecosystem is not.
- Spec, architecture and task evidence stay out of version 1.
  [ADR 0002](0002-architecture-results-are-an-optional-member-of-version-1.md)
  amends this for architecture results, an optional member of version 1.
