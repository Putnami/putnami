# ADR 0068 — An emptied optional project output is emptied on execution and on an empty restore

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/jobs`), `@putnami/go`
  (`build-describe`), `@putnami/clientgen` (`clientgen-go`, `clientgen-ts`)
- **Amends**: [ADR 0064](0064-an-executing-task-starts-from-an-empty-owned-directory.md)
  point 7, and
  [ADR 0065](0065-an-optional-command-output-directory-is-emptied-on-execution-and-on-an-empty-restore.md)
  point 2

## Context

A run computes every cache key before any task runs, and each job computes its
key again when it starts. The `clientgen-go` and `clientgen-ts` keys hashed
`.gen/clientgen/config.json`, `.gen/schema/openapi.json` and
`.gen/schema/openapi.json.gz`. The Go `build-describe` writes all three during
the run, and so does the TypeScript `build-generate`. On a fresh checkout none
exists when the run computes its keys, so the two keys of each clientgen job
differed, and the job missed the entry the run looked up for it.

A key may leave out bytes another task writes only when a key it folds fixes
them. The clientgen keys fold `build-infra`, which folds `build-describe` and
`build-generate`. The TypeScript `build-generate` owns `.gen` as a required
output, so ADR 0064 already empties it and its key fixes what it holds. The Go
`build-describe` declares `.gen/schema` and `.gen/clientgen` as `optionalEmpty`
project outputs, which ADR 0064 point 7 and ADR 0065 point 2 leave as they
stand. A file an earlier run left there survives a describe that executes and
writes nothing, such as one that skips for a project with no main package, and
a restore of an entry that records the output empty. Those bytes are not a
function of describe's key.

ADR 0064 left these outputs alone because an optional output can hold a file
no task writes. Each `cloud/clients` provider slice committed
`.gen/clientgen/config.json`, which `clientgen` reads, inside describe's
`.gen/clientgen` output.

## Decision

1. **A declared output can ask to be emptied.** The protocol member `emptied`
   ([protocol ADR 0009](../../../../protocols/extension/doc/adr/0009-an-optional-project-output-may-ask-to-be-emptied.md))
   is valid on an `optionalEmpty` durable directory output with a literal path
   under the project or workspace root.
2. **The scheduler empties such an output** before its task executes, and on a
   restore of an entry that records it empty, with the keep set ADR 0064
   defines: the ceded and preserved subpaths and the build stamp. Nothing else
   changes: `emptiedOutputDeclarations` admits the output, and the execution,
   the restore of an empty record (`emptiedByRestore`) and the key
   (`emptiedOutputIDs`) follow that one predicate (ADR 0065 point 6).
3. **The Go `build-describe` declares its `schema` and `clientgen` outputs
   emptied.** It also removes `.gen/clientgen` after its skip checks, next to
   the `.gen/schema` rebuild, so a describe whose app configures no client
   generator leaves no earlier contract behind.
4. **The committed generation contract leaves `.gen`.** Each `cloud/clients`
   slice commits it as `clientgen.json` at the project root. The clientgen
   discovery, the generator tasks and both language emitters read
   `.gen/clientgen/config.json` first and `clientgen.json` when no build wrote
   one, and each takes the OpenAPI document of the same origin. With the built
   contract, that is `.gen/schema/openapi.json` or its `.gz`, then
   `schema/openapi.json`. With `clientgen.json`, it is `schema/openapi.json`
   only: a `.gen/schema` beside it is whatever an earlier build left, and no
   key covers it when `build-describe` is not planned, because the Go
   `build-generate` cedes `.gen/schema` and does not restore it on a hit. A
   provider that commits `clientgen.json` and no `schema/openapi.json` fails
   generation with an error that names `schema/openapi.json`. A file
   committed under `.gen/clientgen` or `.gen/schema` is no longer supported,
   and the `@putnami/clientgen` workspace check reports each one in the Git
   candidate set as `clientgen.committed-generated-contract`.
5. **The clientgen keys hash no file a build of the run writes.**
   `clientgen-go` and `clientgen-ts` drop the three `.gen` entries and hash
   `clientgen.json` and `schema/openapi.json`. What a build writes under
   `.gen` is fixed by the producer's key, which they fold. Each key also reads
   `.gen/clientgen/config.json` as a Git candidate
   (`git:.gen/clientgen/config.json`), so a project that has not migrated
   still moves its key when it edits the contract it commits there. A
   workspace ignores `.gen`, so the one a build writes is no candidate and
   never moves the key during the run; the workspace check reports a
   workspace that does not ignore it.
6. **The engine never empties an emptied output that holds a Git-tracked
   file.** Before the scheduler empties an output that declares `emptied`, and
   before a restore writes anything for its task, it asks the Git index once
   per such output whether it tracks a file under the output's path outside
   the kept subpaths. A tracked file deleted from the worktree counts. When
   one does, the execution fails before its subprocess starts, as a failure to
   empty does (ADR 0064 point 6), with an error that names the output, its
   path and the first tracked file, and says that an emptied output owns its
   path whole, so the committed files move out of it. A restore, which would
   empty or replace the output, is refused the same way: that is a restore
   failure (ADR 0065 point 5), so the task executes and fails. Outside a Git
   work tree there is no index, and the output is emptied as before. A
   required or command-output directory is emptied without the question.

With this decision, point 7 of ADR 0064 and point 2 of ADR 0065 read as
follows. An `optionalEmpty` project or workspace output is emptied when it
declares `emptied`, and is not otherwise.

## Consequences

- The precomputed clientgen key equals the key the job computes when it
  starts whenever no file the job reads changed, so the job is served the
  entry the run looked up for it.
- An entry captured from a `.gen/schema` or `.gen/clientgen` that held an
  earlier run's files is never served: from the same inputs, an execution and
  a restore leave the same tree.
- For a project that has a describe phase and no main package, such as a
  library whose `go.mod` names `go.putnami.dev/app`, the static schema
  `build-generate` writes to `.gen/schema` is removed when describe executes
  or is restored. The committed sidecar `schema/openapi.json` is the durable
  copy, and every reader falls back to it.
- Keys move once. `build-describe`'s key moves because its key names two more
  emptied outputs and its contract digest changes, and so does every key that
  folds it: `build-compile`, `build-infra`, `test-exec` and every dependent
  closure behind `^describe`. The clientgen keys move by their own file list.
  The selection fingerprint of each Go release-set member whose key moved
  moves once, so the next publication republishes those members once. The
  store format stays `v9`: keys move only to addresses no earlier entry
  occupies, the rule
  [ADR 0063](0063-a-git-input-keys-the-executable-bit.md) applies.
- **This is a breaking change.** A Go provider that commits a file under
  `.gen/clientgen` or `.gen/schema` fails at describe's next execution
  (decision 6). A TypeScript provider already lost such a file, since its
  `build-generate` owns `.gen` as a required output. To migrate, move
  `.gen/clientgen/config.json` to `clientgen.json` at the project root and
  `.gen/schema/openapi.json` to `schema/openapi.json`, then delete both from
  `.gen`. Under D-004 the change ships in a minor release, whose notes name
  this step.
- An unmigrated workspace fails at its first build: describe fails with the
  engine error and leaves the committed file in place. When describe is not
  planned, `validate` reports each such file with the migration
  (`clientgen.committed-generated-contract`), because the file stays in the
  Git candidate set the workspace check reads. A tracked file an older CLI
  already deleted is not in that set, and that tree's `git:**` key equals the
  key of a tree that never tracked it, so the check does not report it;
  describe still fails on it, because the index tracks it.
- Each execution and restore of a task with an emptied output runs
  `git ls-files` once per such output: twice for `build-describe`.
- A `git:` entry needs a Git work tree. Outside one, `clientgen-go` and
  `clientgen-ts` run uncached, as every task with a `git:` input does.
- An optional project or workspace output that does not declare `emptied`
  keeps the behavior ADR 0064 states, including a committed file inside it.
