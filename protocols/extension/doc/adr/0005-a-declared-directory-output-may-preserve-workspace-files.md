# ADR 0005 — A declared directory output may preserve workspace files

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`), the
  `preserves` member of a declared output, and the CLI's declared capture,
  restore and drift paths

## Context

A generated client directory can be a workspace project of its own. The author
commits a `putnami.json` beside the generated client; a Go client module
carries a `go.mod` (scaffolded once, then edited by `go mod tidy`) and a
`go.sum`. The workspace graph reads `putnami.json` before any task runs, and
the module files follow the consumer's toolchain, not the provider's contract.
When a generator task declares the whole directory, a cache hit deletes or
reverts those files, and the drift check
([ADR 0004](0004-a-declared-output-may-police-its-own-drift.md)) reports it.

`excludes` ([ADR 0003](0003-a-declared-directory-output-may-cede-one-subpath.md))
cannot express this. Its entries are root-relative inside a literal `path`,
while generated-client outputs are `pathFrom` outputs. An exclude also cedes
to an owner; these files have none.

## Decision

1. **A durable `directory` output may carry `preserves`**: output-relative
   subpaths the workspace owns. An entry may name a directory, which covers
   its whole subtree. The engine applies one rule on three paths:
   - capture never records a preserved path;
   - restore merges into the existing directory and leaves the bytes at a
     preserved path, or their absence, as it finds them;
   - drift judges a preserved path on neither side, executed or restored.

   The task may still write a preserved path, typically to scaffold it once.
   That write reaches the worktree and nothing else.
2. **Entries are relative to the output**, so "inside the output" holds by
   construction and the field is decidable on a `pathFrom` output too.
   Entries normalize and sort; an entry that does not normalize and a
   duplicate are validation errors.
3. **A preserved path has no owner.** It takes no part in the ownership check.
   A task that wants the bytes declares them.
4. **The field is part of the task contract.** It enters the task-contract
   digest, so declaring it moves the task's cache key once and older entries
   are never served to it.
5. **The field enters under CLI contract 4**, for the reason in
   [ADR 0002](0002-runtime-toolchains-are-lock-pinned-under-the-current-contract.md)
   decision 7.

## Rejected alternatives

- **Allow `excludes` on a `pathFrom` output, relative to the reported path.**
  One field would mean two things depending on its sibling.
- **An engine-wide rule for `putnami.json`.** It leaves `go.mod` and `go.sum`,
  which only the extension can name, with the same defect.

## Consequences

- `@putnami/go` `build-describe` and `clientgen-go` preserve `go.mod`,
  `go.sum`, `putnami.json` and `.gen` in the Go client directory.
  `@putnami/typescript` `build-generate` and `clientgen-ts` preserve
  `putnami.json`, `node_modules` and `.gen` in the TypeScript client
  directory. `.gen` is written by the engine and the client project's own
  tasks; `node_modules` holds symlinked workspace packages the capture walk
  cannot record.
- The Go describe mirror keeps these files on execution, and scaffolds
  `putnami.json` beside `go.mod` when a module path is set.
- A first-run scaffold is not drift; `git status` shows the new file.
- A cache hit on a checkout that lacks a preserved file does not create it.
  The file stays absent until the task executes and scaffolds it.
