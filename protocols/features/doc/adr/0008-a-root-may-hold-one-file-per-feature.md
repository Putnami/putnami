# ADR 0008 — A root may hold one file per feature, and a requirement text may be a list of sentences

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/features` (`protocols/features`) and
  every reader of authored features: `@putnami/sdd`, `@putnami/contributor`,
  the CLI declaration digest, `go.putnami.dev/app` and
  `@putnami/application`
- **Records**: decision `one-file-per-feature`, in `decisions/one-file-per-feature.json`

## Context

Concurrent pull requests conflict on two kinds of file. A project's
`putnami.features.json` holds every feature of the project, so two pull
requests that each add a feature to `tooling/cli` edit the same array. A spec
requirement `text` is one JSON string, often over 300 characters on one line,
so a diff of one changed sentence shows the whole line, and two edits to
different sentences of one requirement conflict.

`putnami.features.json` shipped in release v0.4.0, and released spec files
hold a string `text`. Both stay readable.

## Decision

1. A workspace or exact project root holds its authored features in one of two
   forms: the single file `putnami.features.json`, or the directory
   `features/`, where each direct `*.json` child is one feature document. A
   root that holds both contributes no feature and reports
   `features.manifest_form_conflict`.
2. A feature document carries the manifest header and one feature:
   `$schema` (`https://putnami.dev/schemas/putnami-feature.json`),
   `protocolVersion`, `namespace` and `feature`. It accepts the manifest
   versions and their gates, and reads as the one-feature manifest it stands
   for. Its findings name positions under `feature`, such as
   `features/run.json#feature.requirements[0]`.
3. The reader merges a root's documents in filename order. The filename takes
   no part in identity; the convention names it after the local part of the
   feature ID. Duplicate feature IDs across documents are the existing
   cross-source duplicate finding.
4. A `features/` directory that is itself a project root is that project,
   never a feature directory.
5. A spec requirement `text` is a string or an array of strings. The
   statement is the sentences joined by one space; the split carries no
   meaning. The joined statement keeps the 512-byte bound, and each sentence
   of an array is non-blank. A writer keeps one sentence as a string.

## Rejected alternatives

- **Move every project to the directory form.** The single file is a released
  form; a small project gains nothing from a directory.
- **A per-document header that omits `namespace` and `protocolVersion`.** The
  document would not validate on its own, and the version gates that refuse a
  forward field would need a second source of truth.
- **Wrapped Markdown for requirement text.** It needs a second parser for one
  field, and the schema can no longer validate it.
- **Derive `specs.baseline.json`.** It is the shrink-only ratchet; deriving it
  drops the review it forces.

## Consequences

- Every reader of authored features reads both forms: the `@putnami/sdd`
  discovery and its task inputs, the contributor workflow checks, the CLI
  declaration digest, CODEOWNERS spec governance, the template root
  exclusions, and the Go and TypeScript evidence producers.
- A task that keys on authored features declares `features/*.json` beside
  `putnami.features.json`.
- A spec diff can name one sentence. A reader that needs the statement calls
  `SpecText.String`.
