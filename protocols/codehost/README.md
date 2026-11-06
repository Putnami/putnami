# Code-host vocabulary

`go.putnami.dev/protocol/codehost` owns the words every Putnami surface uses
for a code host: its slug, what it calls a change request and its diff, where
it lives by default, and the marker of the note Putnami writes on a change
request. It holds words and defaults only: API clients, tokens and note bodies
stay with the extension or service that publishes.

## Why

The slugs were declared in several places, and "pull request" versus
"merge request" was decided in one place and hardcoded in another. One owner
means a new host, or a corrected word, is one change that every consumer takes
on its next version.

## What

| Slug | Name | Change request | Diff tab | Default host | Default API |
| --- | --- | --- | --- | --- | --- |
| `github` | GitHub | pull request `#42` | Files changed | `github.com` | `https://api.github.com` |
| `gitlab` | GitLab | merge request `!42` | Changes | `gitlab.com` | `https://gitlab.com/api/v4` |
| `generic` | Git host | change request `#42` | Changes | — | — |

- `Slugs()` is the closed set, in that order; `Valid` compares
  case-sensitively.
- `Lookup(slug)` returns a slug's vocabulary and whether it is known;
  `For(slug)` falls back to `generic` for display.
- `ChangeRequestLabel(slug, n)` gives `merge request !42`; a number that is not
  positive integer gives the noun alone.
- `NoteMarker` is `<!-- putnami-observe:summary -->`, the first line of the
  Putnami note. `IsPutnamiNote(body)` is true when the body starts with it
  after leading spaces, tabs, carriage returns and line feeds. The value never
  changes: notes already published carry it.

A self-managed GitLab or GitHub Enterprise host keeps its slug and overrides
the defaults with its own host and API.

## How

[`vocabulary.json`](./vocabulary.json), described by
[`schemas/vocabulary.json`](./schemas/vocabulary.json), is the document every
reader is held to. The Go table and the TypeScript binding
[`@putnami/codehost-protocol`](../../typescript/framework/codehost-protocol/README.md)
each compare themselves with it, and give the results listed in
[`fixtures/expectations.json`](./fixtures/expectations.json). A change to the
vocabulary lands in the document, the Go table and the TypeScript table
together.

`protocols/evidence` takes its code-host slugs from this module, and
`@putnami/observe` its labels and the note marker.

## Support

`preview`, as recorded in the [reviewed support catalog](../../putnami.support.json).
The `protocols` owner maintains it. `protocols/evidence` and `@putnami/observe`
consume the Go module, and `@putnami/codehost-protocol` reads the same
vocabulary document and fixture corpus. Stable support still needs a consumer
outside this repository, the Cloud workspace, and an explicit compatibility
window.

See [ADR 0001](./doc/adr/0001-one-owner-for-code-host-vocabulary.md).
