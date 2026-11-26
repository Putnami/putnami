# ADR 0001 — Whole-repository text checks depend on nothing

- **Status**: accepted
- **Date**: 2026-11-26
- **Scope**: `@putnami/repository-checks`, `@putnami/cli-documents`,
  `@putnami/cli-document-set`

## Context

The public-cut scan and the release rehearsal read every candidate file of the
repository, so their project keys its tests on `git:**` and every change runs
them. They lived in `@putnami/cli-documents`, a Go module that imported the
CLI's internal packages (`store`, `git`), the extension SDK's `gittest` and the
features protocol's `spectest`. The planner derives a project's dependencies
from its imports, so every plan also scheduled the prerequisite build and test
steps of about 33 projects: a one-line change to a Dart sample planned 35
projects and 191 tasks, of which only the document checks related to the
change.

Reading the tree is cheap: about 13 000 tracked files, read in tens of
milliseconds from the page cache. Compiling the CLI to do it is not.

## Decision

A check that reads files as text and does not depend on compilation lives in
`@putnami/repository-checks` (`tooling/repository-checks`), a Go module that
requires nothing. Its test task depends on no other project and keys on
`git:**`, so it runs on every change and costs only itself. It holds the
public-cut scan, the release rehearsal and its spec, the governance record, the
first-use block, the moved-commands reference, the spelling of every declared
pattern and the cloudless shipped manifests.

A check against the CLI reads what the CLI commits instead of importing it. The
moved-commands URL is not part of the committed command surface
(`tooling/cli/command-surface.json` states only what users type), so the check
parses the constant's declaration in
`tooling/cli/internal/commandmeta/moved_commands.go` with `go/parser`.

The spec proofs of the release rehearsal are written by a helper of the module
in the observation format of `protocol/features/spectest`, so the module needs
no dependency to prove them.

A check that needs the CLI's code stays in a project that imports it, with
inputs narrowed to the files it reads: `@putnami/cli-documents` keeps the proof
that the declaration keys exactly what the scanner reads, against the CLI's
collector, and keys on `tooling/repository-checks/putnami.json`;
`@putnami/cli-document-set` keeps the gates that run the CLI.

## Consequences

- A change outside the CLI's sources plans the repository checks without any
  build or test step of the CLI or of the protocols.
- The scanner's candidate enumeration is a `git ls-files` call of this module,
  not the CLI's. `@putnami/cli-documents` fails when the two diverge.
- The fragment writer duplicates the observation format of `spectest`, a wire
  contract the language adapters already implement more than once.
  `TestSpecFragmentFollowsTheSpectestObservation` parses the protocol's
  `Fragment` declaration and fails when the two diverge; `git:**` keys that
  source, so a change to it reruns the check.
