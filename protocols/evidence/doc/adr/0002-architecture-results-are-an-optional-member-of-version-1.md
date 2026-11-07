# ADR 0002 — Architecture results are an optional member of version 1

- **Status**: Accepted
- **Date**: 2026-11-07
- **Amends**: [ADR 0001](0001-one-self-reported-change-evidence-document.md),
  its consequence "Spec, architecture and task evidence stay out of version 1"

## Context

Teams that already check their architecture with ArchUnit, deptrac or
import-linter want those results next to their tests and coverage, per
component, without rewriting a rule. ADR 0001 kept architecture
evidence out of version 1.

Two ways carry it. A version 2 makes every reader refuse every document a
new producer writes, with or without architecture results, until the reader
updates. An optional member of version 1, the way the `sbom` member and the
dependency states arrived, leaves a document without it valid for every
reader, and only a document that carries it is refused by an older reader
(`evidence.unknown_field`).

## Decision

1. The document gains an optional `architecture` member: the rules an
   architecture checker evaluated, each with its tool (an open slug, like an
   ecosystem), its rule text, its status (`kept` or `broken`, a closed
   vocabulary), its violation count and up to 50 violations. A violation
   carries its message and, when the producer located it, its file, its line
   and the component that holds the file.
2. It is a member of version 1. A producer that read no architecture result
   writes no member, and a reader updates before its producers write it.
3. The document stays self-reported: it carries the checker's result, not a
   verdict of its own.

## Consequences

- A new architecture checker needs no protocol change; a new status does.
- Spec and task evidence still stay out of version 1.
