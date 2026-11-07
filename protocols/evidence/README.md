# Change evidence document

This package owns the **change evidence document**: one change in a repository
that keeps its own CI, as one CI job observed it. The observe extension writes
it from what the pipeline already produced (the git diff, JUnit and coverage
reports, lockfiles); a reporter uploads it; Putnami Cloud, or any other reader,
validates it with `ParseAndValidate`.

## Why

Teams that build, test, package and publish without Putnami still want to
understand what a change touches. The analysis runs in the CI job, inside the
open-source CLI, so source code never leaves it. The existing documents do not
fit: `protocol/cli` sessions and `protocol/ci` change plans identify work by
Putnami project, task, extension and cache key, which such a repository does
not have. See `doc/adr/0001-one-self-reported-change-evidence-document.md`.

## What

A document (`schemas/evidence.json`) has `protocolVersion: 1` and:

| Member | Content |
| --- | --- |
| `subject` | The code host (`github`, `gitlab`, `generic`, the slugs [`codehost`](../codehost/README.md) owns), its hostname and the repository's numeric project id (required for github and gitlab: the key that survives a rename), the repository name, the commit `sha`, the `baseSha` when one was resolved, the branch and the change request number. |
| `ci` | The CI system (`github-actions`, `gitlab-ci`, `jenkins`, `circleci`, `buildkite`, `azure-pipelines`, `bitbucket-pipelines`, `teamcity`, `generic`; `DetectCISystem` names it from the variable each CI sets in every job, and `CI=true` alone is `generic`), its run id, job id and attempt (which, with the commit, identify the document among the jobs and re-runs of one commit), and an https run URL without credentials. |
| `producer`, `generatedAt` | The program that wrote the document and when (RFC 3339 with a zone). |
| `changedFiles` | The diff: path, status (`added`, `modified`, `deleted`, `renamed` with its `previousPath`), and the component the file belongs to. |
| `components` | Repository-relative directories (`.` for the whole repository), with an open `ecosystem` slug, the `source` that found them (`orchestrator`, `workspace`, `manifest`, `fallback` for the whole repository, `fallback-codeowners` and `fallback-directories` for the directories a fallback split gives, `declared`), their `impact` and their `dependsOn` edges. |
| `tests`, `coverage` | One entry per component: passed, failed, skipped, duration and failing cases; covered and total lines. |
| `changedLineCoverage` | The coverage of the lines the change added or modified, per file, with the uncovered line numbers. |
| `dependencyChanges` | The lockfile difference: ecosystem, lockfile, package, `added`, `removed`, `upgraded`, `downgraded` or `changed`, from and to versions. From a software bill of materials (SBOM), a `before` and an `after` state: the package's licences and known vulnerabilities, each an id and a severity (`critical`, `high`, `medium`, `low`, `info`, `none`, `unknown`). |
| `sbom` | The SBOMs the states were read from: a `head` and a `base` document, each with its `format` (`cyclonedx` or `spdx`) and `specVersion`. Absent when the producer read none. Without a `base`, every base state is unknown. |
| `decisions` | The decision records the change concerns: each record's `path`, `title`, `status` (`proposed`, `accepted`, `rejected`, `deprecated`, `superseded`, `unknown`), `date`, the path patterns of its `scope`, the changed files it `governs`, and its own file status as `change` when the change adds, modifies, renames or deletes it. Absent when the producer found none. See `doc/adr/0002-the-decision-records-a-change-concerns.md`. |
| `diagnostics` | The producer's own findings (a missing report, an unresolved base). They never make the document invalid. |

## Rules

- **Self-reported.** The document carries no verdict and no trust claim.
  Whatever produced it, a reader treats its facts as reported, not proven.
- **Strict.** Bytes that are not one JSON object, or a member of the wrong
  type, are `evidence.parse_error`; an undeclared member, at any depth and
  compared case-sensitively, is `evidence.unknown_field` at its dotted path.
- **Consistent.** Every component reference resolves
  (`evidence.unknown_component`), ids and paths are unique
  (`evidence.duplicate`), every path stays in the repository
  (`evidence.invalid_path`), a changed file lies inside its component
  (`evidence.invalid_component`), and a diff needs its base
  (`evidence.invalid_subject`). A component is `changed` exactly when a changed
  file belongs to it, `dependent` exactly when it is not changed and reaches a
  changed component through `dependsOn`, and `unaffected` otherwise
  (`evidence.invalid_impact`); one walk over the reversed edges decides it, in
  linear time. Counts add up (`evidence.invalid_count`), and a
  dependency change carries the versions its kind needs
  (`evidence.invalid_dependency_change`). A `before` state needs a `base` SBOM
  and a package that was not added; an `after` state needs a `head` SBOM and a
  package that was not removed (`evidence.invalid_dependency_change`). An SBOM
  names a known format and a dotted spec version (`evidence.invalid_sbom`).
  A decision names a title, a known status and a valid date, governs only
  changed files, names the file status of its record exactly when the record
  is a changed file, and concerns the change through one or the other
  (`evidence.invalid_decision`).
- **Bounded.** At most 20000 changed files, 5000 components, 10000 dependency
  changes, 100000 dependency edges, 20000 changed-line coverage files, 500
  diagnostics, 50 failing cases per tests entry, 200 uncovered line numbers per
  file, 20 licences and 200 vulnerabilities per dependency state, 1000
  decisions with at most 100 scope patterns and 1000 governed files each, and
  4096 bytes per string (`evidence.too_large`).
- **Open where stacks grow.** Ecosystems are slugs: a new stack needs no
  protocol change. A new code host or CI system is a protocol change: it
  extends a closed vocabulary of version 1, a reader built before it refuses a
  document that names it (`evidence.invalid_subject`, `evidence.invalid_ci`),
  so a reader updates before its producers write it.
- **Optional members stay optional.** The `sbom` member, the dependency
  states and the `decisions` member are optional members of version 1: a
  document without them validates as before, and a producer that read no SBOM
  or found no decision record writes none of them. A reader
  built before they existed refuses a document that carries them
  (`evidence.unknown_field`), so a reader updates before its producers write
  them.

## Conformance

`fixtures/expectations.json` lists every fixture: each valid one validates
cleanly, each invalid one gets exactly its listed codes. The tests pin the
vocabularies, check that the schema, the Go types and the strict decoder
declare the same members, and bind each requirement of
`specs/change-evidence-document.json` to its proof.

## Support status

- **Owner**: `go.putnami.dev/protocol/evidence`.
- **Status**: `experimental`, explicit opt-in and never part of the default
  experience, recorded as
  `{"id": "go.putnami.dev/protocol/evidence", "kind": "protocol", "status": "experimental", "default": false}`
  in the workspace-root [`putnami.support.json`](../../putnami.support.json)
  catalog (contract: [`protocols/support`](../support/README.md)). The wire
  format and finding codes may change without a migration.
- **Missing for `preview`**: an active consumer in this repository, such as
  the observe extension writing the document.
