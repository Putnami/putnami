# ADR 0001 — Known-document-set gates key on their documents

- **Status**: accepted
- **Date**: 2026-11-25
- **Scope**: `@putnami/cli-document-set`, `@putnami/cli-documents`

## Context

A cache key is per project and per command, never per task.
`@putnami/cli-documents` keys its tests on `git:**`, because the public-cut scan
and the release rehearsal read every candidate file. Its other gates read a set
of files they can name: the governance surface, the neutral-contributor recipe,
the first-use block, the documentation page the CLI cites, the project
manifests, the shipped manifests and `putnami.ci.json`. While they shared the
project, every change ran them with the whole-tree gates, and a change to one of
their documents ran the whole-tree gates with them.

## Decision

The gates that read a known set form `@putnami/cli-document-set`
(`tooling/cli-document-set`), which keys its tests on the files they read: the
named documents, the documentation folders the site publishes, every
`putnami.json` and `schema/config.json`, and the shipped `go.mod` and
`package.json` manifests. `TestPublishedDocumentationIsADeclaredInput` fails
when the site publishes a folder the key does not name.

The project is a sibling of `tooling/cli-documents`, not a child: a nested
project is in that project's scope, and the scope boundary forbids its import of
`/tooling/cli`. It joins the `cli` domain beside `@putnami/cli-documents`.

## Consequences

- `@putnami/cli-documents` keeps `git:**`, so it still runs for every change,
  with a smaller suite. Moving the whole-tree gates out of the per-change gate
  is left to the engine review.
- A change outside the named documents and manifests no longer runs the
  document-set gates.
