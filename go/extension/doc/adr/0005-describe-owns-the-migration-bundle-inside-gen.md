# ADR 0005 — `build-generate` cedes every `.gen` subpath a later task reads, and each has one declared owner

- **Status**: accepted
- **Scope**: `@putnami/go` (`go/extension`)

## Context

`build-generate` declares `<project>/.gen` as one directory output. Other tasks
write inside it: `build-describe` runs after generate and writes contracts,
the design graph and the migration bundle; both `config-merge` tasks run
before generate and write the merged config. Later tasks read those files at
their contract paths: clientgen discovery and `go/framework/api` prefer
`.gen/schema/openapi.json` over the tracked sidecar and key on it; release-set
migration publication, deploy, `database.ApplyBundle` and the database test
provider read `.gen/migration-bundle`; `test-exec` synthesizes
`DATABASE_TEST_BINDINGS` from `.gen/conf/.env.test.yaml`.

When generate captures the whole tree, its entry depends on what an earlier
run left on disk, which breaks its `cache.deterministic` declaration. Its
restore swaps that snapshot over `.gen`, so a warm build loses files a later
task needs, and a hit can resurrect a file the current sources no longer
produce. A task that writes inside `.gen` without declaring an output
reproduces nothing on a hit.

## Decision

**1. Generate cedes each subpath another task writes; the writer declares it.**
Generate keeps `.gen` as one declared directory output and lists the ceded
subpaths in `excludes`
([protocol ADR 0003](../../../../protocols/extension/doc/adr/0003-a-declared-directory-output-may-cede-one-subpath.md)).
Each claimant declares an `optionalEmpty` output at the contract path, never a
private staging copy. `cededGenOwner` in
`go/extension/cmd/putnami-go/manifest_contract_test.go` pins this table:

| Ceded subpath | Claimed by | Why |
|---|---|---|
| `.gen/schema` | `build-describe` (`schema`) | clientgen discovery, `go/framework/api`, both clientgen cache keys |
| `.gen/clientgen` | `build-describe` (`clientgen`) | clientgen discovery, which `validate` runs |
| `.gen/design` | `build-describe` (`design`) | `@putnami/sdd` feature projection, `putnami context` |
| `.gen/migration-bundle` | `build-describe` (`migrationBundle`) | migration publication, deploy, `ApplyBundle`, test provider |
| `.gen/migrations.json` | `build-describe` (`migrations`) | CI assertions; ceded so a hit cannot resurrect it |
| `.gen/conf` | `config-merge-exec`, `config-merge-test-exec` (`merged`) | `test-exec` binding fallback, dependents' `config-merge` |
| `.gen/.describe.lock` | nobody | run-scoped `lockedfile` mutex |
| `.gen/config-deps.json` | nobody | fragment the same describe run folds into `schema/config.json` |

A new file that describe or another task writes under `.gen` needs a row. An
unclaimed row is a statement: nothing reads it after the run, and capturing it
would vary a deterministic entry with the state of the tree. The describe lock
stays at one fixed per-project path so `build~describe` and `test~describe` in
one invocation serialize on the same mutex.

**2. The framework is the only writer of what describe declares.** The
extension declares where the describe binary writes; it never copies or
mirrors the bundle. A describer that produces nothing removes its stale output
(`describeMigrationBundle` and `describeMigrations` do), and `optionalEmpty`
records the absence.

**3. `.gen/schema`: generate stages, describe converges.** It has two
producers: generate's static OpenAPI stub and the describe binary's runtime
document, merged by `collectArtifacts` with `openapiutil.Merge`. Generate writes
each contract at `.gen/schema/<rel>` and at `.gen/generate-staging/schema/<rel>`,
a mirror inside the region it still owns, and rebuilds the mirror from scratch
on every run. Describe removes `.gen/schema`, rebuilds it from the mirror, then
merges. So describe's starting tree is identical on a generate hit and a miss.
The removal is load-bearing: declared capture walks the directory, and a
producer deleted from the app (a dropped `proto.New`) never runs to drop its
own stale file. The mirror appears in neither `schemas` nor `exports` of
`generate-result.json`; `.gen/schema/<rel>` is the only path anyone resolves.

**4. A project with no describe phase may not set
`options.generate.schema=false`.** A contract then has no durable home: the
option removes the tracked sidecar, and nothing restores `.gen/schema`.
`build-generate` fails with a diagnostic naming the option, the project, the
artifact and the reason. It fails only when a contract is about to be written,
because the option is inherited and a workspace default must not fail every
library and CLI.

**5. The two `config-merge` claims are two files, never the directory.**
`config-merge-test-exec` pins `APP_ENV=test` and declares the literal
`.gen/conf/.env.test.yaml`. `config-merge-exec` reads the ambient `APP_ENV` and
declares `pathFrom: "mergedConfig"`, which resolves to
`.gen/conf/.env.<APP_ENV or local>.yaml`. Only one may use the port: the
plan-time owner check compares two port-backed outputs by port name under one
project, and both tasks are planned for every project under `build,test`. The
runtime reports `mergedConfig` as a project-relative slash path on success
only; the CLI drops an absolute `pathFrom` value. The `.manifest.json` sidecar
beside the merged file carries `generatedAt` and has no reader, so nobody
declares it. Both tasks key on `conf/.env.yaml` and `conf/.env.*.yaml` with
`from: closure`, which includes the project itself, so a dependency's config
change moves the key.

**6. Ordering.** Every command that schedules generate (`build`, `test`,
`serve`, `run`, `package`) schedules describe with
`dependsOn: ["^describe", "generate"]`, and the manifest test asserts it for
every such command. The CLI keeps a ceded subpath across the ceding task's
restore, so a generate hit leaves describe's and config-merge's files as an
executed generate would. `.gen/conf` needs no order against generate, which
neither reads nor writes it; the `test` pipeline orders `test-exec` after
`config-merge`.

**7. Two shared regions stay generate's.** Generate deletes the `.gen/infra`
fragments at the start of every run, so its entry is a function of its own
producers; the durable artifact is the committed `infra/requirements.json`.
`.gen/generate-result.json` is rewritten whole by generate and appended to by
describe, its only reader. Describe does not gain the `gen` write resource:
`reads: ["gen"]` already serializes it after generate, and `writesProjectGen`
in the CLI identifies one owner of the generated tree.

## Invariants

- After any successful `build`, `.gen/schema` holds describe's converged
  contracts and `.gen/migration-bundle` holds the current bundle, whatever
  mix of tasks executed or restored.
- After any successful `test` with something to merge,
  `.gen/conf/.env.test.yaml` holds the merge of the closure's current config.
- Generate's entry never holds bytes under a ceded subpath.
- Describe's entry holds only what the current sources produce.
- Every ceded subpath has exactly one claimant set in the table or is
  recorded as claimed by nobody; a third claimant is a plan-time error.

## Rejected alternatives

- **Stage private copies under the command output.** Cacheable but invisible:
  every consumer reads the contract path.
- **Let consumers fall back to `.gen/generate-staging` or a staged copy.** It
  is the second location protocol ADR 0003 refuses, and it moves the repair
  into every consumer.
- **Cede only describe-only artifacts and keep `openapi.json` with generate.**
  Generate's hit would restore the stub under the path consumers prefer.
- **Schedule describe for libraries so it owns `.gen/schema` everywhere.** One
  more node in five commands for every Go project, for a configuration no one
  uses. The refusal costs nothing.
- **Let describe declare `writes: ["gen"]`.** Two producers of `.gen`, and it
  changes CLI generation-entry handling for an unrelated reason.
- **`cache: false` on `config-merge`.** Recomputes on every run and leaves
  generate adopting and deleting `.gen/conf`.
- **Declare `.gen/conf` as a directory on one task.** Two tasks write inside
  it, and it would capture the timestamped `.manifest.json`.
- **Two port names, one per `config-merge` variant.** Legal, but exists only to
  dodge the check; the pinned variant has a static path.

## Consequences

- Changing a carve-out is a reviewed change to `cededGenOwner`.
- With `APP_ENV=test` ambient in a `build,test` session, both `config-merge`
  tasks resolve to the same file. They capture identical bytes, and neither the
  plan-time check (port versus literal) nor a runtime check sees the overlap.
