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
| `tests`, `coverage` | One entry per component: passed, failed, skipped, duration and failing cases; covered and total lines. A tests entry may list its `cases`, each test case that ran with its `suite`, its `name` and its `outcome` (`passed`, `failed`, `skipped`), and count in `omittedCases` the cases left out to keep within the bounds or because their report gave their id to another case. A case's suite and name are its stable id, so a reader compares two documents case by case and tells a test that vanished from one that still passes: a case vanished only when the later entry carries `cases` and omits none (`ComponentTests.ListsEveryCase`); an entry without `cases` says nothing of its cases. See `doc/adr/0004-test-cases-are-an-optional-member-of-version-1.md`. |
| `architecture` | The rules an architecture checker evaluated: its `tool` (an open slug: `archunit`, `deptrac`, `import-linter`), the `rule` as the checker names it, its `status` (`kept` or `broken`), the `violationCount` it reported and up to 50 `violations`, each with its message and, when the producer located it, its `file`, `line` and `component`. Absent when the producer read none. |
| `changedLineCoverage` | The coverage of the lines the change added or modified, per file, with the uncovered line numbers. |
| `dependencyChanges` | The lockfile difference: ecosystem, lockfile, package, `added`, `removed`, `upgraded`, `downgraded` or `changed`, from and to versions. From a software bill of materials (SBOM), a `before` and an `after` state: the package's licences and known vulnerabilities, each an id and a severity (`critical`, `high`, `medium`, `low`, `info`, `none`, `unknown`). |
| `sbom` | The SBOMs the states were read from: a `head` and a `base` document, each with its `format` (`cyclonedx` or `spdx`) and `specVersion`. Absent when the producer read none. Without a `base`, every base state is unknown. |
| `decisions` | The decision records the change concerns: each record's `path`, `title`, `status` (`proposed`, `accepted`, `rejected`, `deprecated`, `superseded`, `unknown`), `date`, the path patterns of its `scope`, the changed files it `governs`, and its own file status as `change` when the change adds, modifies, renames or deletes it. Absent when the producer found none. See `doc/adr/0003-the-decision-records-a-change-concerns.md`. |
| `declarations` | The declaration files the change adds, modifies, renames or deletes in a Putnami workspace: specs, `specs.baseline.json`, `putnami.features.json`, `features/*.json`, `adr/*.md`, `decisions.json`, `decisions/*.json`, `putnami.architecture.json`, `putnami.ci.json`, `putnami.verdict-policy.json` and `CODEOWNERS`. Each carries its `path`, its `status` as the changed file has it, its `previousPath` when renamed, and its whole text `before` and `after` the change (no `before` when added, no `after` when deleted). An empty list when the change touches none; absent when the repository declares nothing or the whole diff could not be read. |
| `specVerification` | The spec verification record the workspace's own test run wrote, carried whole as one JSON object. Absent when the producer read none. |
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
  An architecture rule names its tool and rule; its status is `kept` or
  `broken` and a line needs a file (`evidence.invalid_architecture`); a kept
  rule counts no violation and a broken one at least one and its listed ones
  (`evidence.invalid_count`); a violation's component holds its file
  (`evidence.invalid_component`).
  A test case names its test (`evidence.required`), once per suite and name in
  its entry (`evidence.duplicate`), with a known outcome
  (`evidence.invalid_test_case`); an entry lists no more cases of an outcome
  than its count of it, and its listed and omitted cases are at most the
  cases it counts (`evidence.invalid_count`).
- **Bounded.** At most 20000 changed files, 5000 components, 10000 dependency
  changes, 100000 dependency edges, 20000 changed-line coverage files, 500
  diagnostics, 50 failing cases and 10000 test cases per tests entry, 50000
  test cases per document, 1000 architecture rules, 50
  violations per rule, 200 uncovered line numbers per file, 20 licences and
  200 vulnerabilities per dependency state, 1000 decisions with at most 100
  scope patterns and 1000 governed files each, 1024 declarations of at most
  256 KiB per document, a spec verification record of at most 512 KiB, and
  4096 bytes per other string (`evidence.too_large`).
- **Declarations follow the diff.** A declaration names a declaration file
  the change touches, with the same status; a renamed one names its previous
  path, an added one has no `before` and a deleted one no `after`
  (`evidence.invalid_declaration`). The spec verification record is one JSON
  object (`evidence.invalid_spec_verification`).
- **Open where stacks grow.** Ecosystems are slugs: a new stack needs no
  protocol change. A new code host, CI system or component source is a
  protocol change: it extends a closed vocabulary of version 1, a reader built
  before it refuses a document that names it (`evidence.invalid_subject`,
  `evidence.invalid_ci`, `evidence.invalid_component`), so a reader updates
  before its producers write it.
- **Optional members stay optional.** The `sbom` member, the dependency
  states, the `decisions`, `architecture`, `declarations` and
  `specVerification` members and the `cases` and `omittedCases` of a tests
  entry are optional
  members of version 1
  (`doc/adr/0002-architecture-results-are-an-optional-member-of-version-1.md`):
  a document without them validates as before, and a producer that read no
  SBOM, found no decision record or read no architecture result writes none
  of them. A reader
  built before they existed refuses a document that carries them
  (`evidence.unknown_field`), so a reader updates before its producers write
  them.

## Evidence fragments

A task states what it observed about a change as **evidence fragments**: one
JSON object per line of `putnami-evidence.jsonl` in its command output
(`schemas/evidence-fragment.json`, `ParseFragments`, `EncodeFragments`). The
task declares the file as a required command-output file and writes it on
every run, empty when it finds nothing, so the cache keeps it with the task and
a cache hit gives back the same file. The reserved artifact identity
`putnami-evidence` names it. A fragment states no
project, task, commit or verdict: the reader stamps the project that owns its
path and applies its policy (the CLI's evidence verdict, ADR 0134 of
`@putnami/cli`).

| Member | Content |
| --- | --- |
| `protocolVersion` | `1` |
| `kind` | `finding`, the only kind of this version; a later one adds kinds by addition |
| `source` | `format`, an open lowercase slug naming the shape the producer translated (`putnami-sdd`, `golangci-lint`), and an optional `tool` |
| `subject` | `path`, a clean slash path relative to the task's project that never leaves it, and an optional `symbol`, `line` and `column` (from 1; a column needs its line) |
| `rule`, `severity`, `message` | For a finding: the rule as its checker names it, `error`, `warning` or `info`, and what it reports |

The reader is strict: a blank or malformed line, another version or kind, an
undeclared, null or repeated member, or a member of the wrong type fails the
whole file, naming the line as `fragments[<index>]`. A file holds at most
50,000 fragments and 16 MiB, and a string at most 4 KiB (`evidence.too_large`;
the CLI's bounded file reader refuses a larger file before it parses it). A
value that breaks its kind's rules, such as a line or column of 0, is
`evidence.invalid_fragment`. `EncodeFragments` reads its own output back, so it
never writes a file the reader refuses.

## Conformance

`fixtures/expectations.json` lists every fixture: each valid one validates
cleanly, each invalid one gets exactly its listed codes. The tests pin the
vocabularies, check that the schema, the Go types and the strict decoder
declare the same members, hold the schema and the frozen version 1
documents of `fixtures/v1/` to the promise below, and bind each requirement of
`specs/change-evidence-document.json` to its proof.

## Support status

- **Owner**: `go.putnami.dev/protocol/evidence`.
- **Status**: `preview`, not part of the default experience, recorded as
  `{"id": "go.putnami.dev/protocol/evidence", "kind": "protocol", "status": "preview", "default": false}`
  in the workspace-root [`putnami.support.json`](../../putnami.support.json)
  catalog (contract: [`protocols/support`](../support/README.md)).
- **Promise**: version 1 changes only by addition, a new optional member of
  a closed object, a new vocabulary value or a looser bound, so a document that validates today
  keeps validating through every release of version 1; anything else opens
  version 2 (`doc/adr/0005-version-1-changes-only-by-addition.md`, decision
  `evidence-version-1-changes-only-by-addition`). A reader still updates
  before its producers write an addition. The Go API of the module may change
  in a minor release whose notes say so.
- **Evidence**: the observe extension writes the document and the cloud
  extension uploads it; the strict reader, the schema and the fixture corpus
  above agree; `fixtures/v1/` freezes the promised schema and version 1
  documents, each pinned by its digest, and `frozen_v1_test.go` fails on a
  removed, retyped, newly required or no longer required member, a removed
  vocabulary value, a changed pattern, const or reference, a constrained
  member of an open object, a narrower schema bound and a smaller bound of
  the strict reader. Go is the only implementation, so no
  cross-implementation parity is promised.
- **Missing for `stable`**: a reader outside this repository that relies on
  the document, an N-1 read window or a documented migration for version 2,
  and an entry in the
  [compatibility budget](../../tooling/cli/doc/21-compatibility-and-migration.md).
