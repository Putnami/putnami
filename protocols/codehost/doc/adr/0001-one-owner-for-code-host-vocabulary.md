# ADR 0001 — One owner for the code-host vocabulary

- **Status**: Accepted
- **Date**: 2026-11-06

## Context

The code-host slugs `github`, `gitlab` and `generic` were declared three
times: in `protocols/evidence`, and twice in the Cloud workspace. The words
that go with them were scattered too. `@putnami/observe` wrote "pull request"
or "merge request" in two places, and one of them gave a GitLab merge request
the `#` prefix GitLab does not use. The Cloud console held its own copy of the
same words. The marker that tells Putnami's note on a merge request from every
other note lived in the observe publisher alone, while a Cloud provider will
need to find the same note.

## Decision

1. A protocol module, `go.putnami.dev/protocol/codehost`, owns the slug set,
   the display vocabulary of each slug (its name, its change-request noun and
   reference prefix, its diff-tab label), the public host defaults, and the
   marker of the Putnami note. Nothing else: no API client, no token, no note
   body.
2. `vocabulary.json` is the document every reader is held to. The Go package
   and the TypeScript binding `@putnami/codehost-protocol` each carry the
   table in code, and their tests compare it with the document and with one
   fixture corpus, on the precedent of `protocols/cli` and
   `@putnami/cli-protocol`.
3. `protocols/evidence` keeps its validated enum and takes its slugs from
   this module, so the evidence schema and the slug set cannot drift.
4. The marker keeps its existing value, `<!-- putnami-observe:summary -->`:
   notes already published carry it, and a new value would orphan them.

## Consequences

- A new code host is one change here, then a version bump of each consumer.
- The Cloud workspace drops its copies after a pin bump; that is tracked
  there.
- Changing the marker is a one-way door: every published note would stop
  being found.
