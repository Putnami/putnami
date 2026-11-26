# ADR 0001 — Known-document-set gates key on their documents

- **Status**: accepted; the checks that only read text moved to `@putnami/repository-checks` ([its ADR 0001](../../../repository-checks/doc/adr/0001-text-checks-depend-on-nothing.md))
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

- The whole-tree gates keep `git:**` and run for every change. They now live
  in `@putnami/repository-checks`, which depends on no other project, together
  with the gates of this project that only read text.
- A change outside the named documents and manifests no longer runs the
  document-set gates.
