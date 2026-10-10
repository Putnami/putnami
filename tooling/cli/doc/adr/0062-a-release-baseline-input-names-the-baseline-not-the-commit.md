# ADR 0062 — A release-baseline input names the baseline, not the commit

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/git`, `internal/jobs`),
  `go.putnami.dev/protocol/extension` (runtime inputs),
  `go.putnami.dev/protocol/cli` (contract 7), `@putnami/go` (`validate-api`)
- **Amends**: [ADR 0059](0059-a-cache-key-describes-the-tree-not-the-commit.md)

## Context

The Go `validate-api` task compares a project's exported API with the API at
the last tag of its version line. An incompatible change passes only when a
commit since that tag declares a breaking change. The verdict reads git
history: the line's tags, the files at the last tag, and the messages of the
commits since. No file pattern holds any of that, so the task was never
cached. It ran on all 43 Go projects in every CI run.

Keying the task on the commit would cache it, but ADR 0059 keeps the commit
out of every key. A pull request and the `main` run after its squash merge
hold one tree at two commits. They would never share an entry. The task also
named the breaking commit in its data and its messages, so two histories with
one verdict still gave two outputs.

## Decision

1. **The CLI reads the history inputs before the task runs.** A task declares
   the runtime input `releaseBaseline`. The CLI reads the project's release
   baseline from git (`git.ReadReleaseBaseline`) and adds it to the key's
   runtime identity. The baseline has these fields:
   - the repository state: no work tree, no commit, a shallow clone, no
     reachable tag of the line, or tagged;
   - the line's tag pattern;
   - for an untagged line, whether the repository has a tag of another line;
   - the line's last tag HEAD reaches, and the tree that tag holds at the
     project directory;
   - whether a commit since that tag that touches the project declares a
     breaking change, read by the version bump's own parser
     (`ParseConventional`).

   The baseline names no commit HEAD reaches, no ref and no path. One run
   reads each project's baseline once.
2. **A git failure is a key error.** The task then runs uncached. A state read
   from a failure would key a verdict to a baseline nobody saw.
3. **The verdict names no commit.** `validate-api` reports `breakingDeclared`
   instead of the breaking commit's SHA, and says "a commit since `<tag>`
   declares the breaking change". A branch and its squash merge give the same
   output when the squash commit's message declares the same break as the
   branch's commits.
4. **The name needs CLI contract 7.** A CLI that does not know a runtime
   input name resolves nothing for it, and would key the task without the
   baseline. `ReleaseBaselineInputContract` (7) is the additive contract a
   manifest reaches by declaring the name, so an older CLI refuses the
   manifest instead. Rung 7 also covers a `git:` input, whose key holds the
   executable bit from this release on (`GitInputModeContract`,
   [ADR 0061](0061-a-git-input-keys-the-executable-bit.md)): one CLI release
   added both meanings.
5. **`validate-api` keys on what it reads.** Its inputs are the project's
   non-test Go files and the files their embed directives select, its
   `go.mod` and `.json` files, the root `putnami.support.json`, the
   `command-surface` option and `releaseBaseline`. The file patterns skip
   directories whose name starts with a dot, and the CLI's `**` walk never
   enters `node_modules`, `out`, `dist` or `vendor`. The check reads no Go
   file under any of those directories, in the working tree or at the tag,
   and it refuses a working-tree command-surface document under one. That
   document must be a `.json` file that this key reads. The task is `deterministic`, so a skip
   for a project the support catalog does not list as stable is cached too.

## Consequences

- A branch and its squash merge share the `validate-api` key when they hold
  one tree and declare the same break, whatever the commits, the ref or the
  checkout directory.
- A squash title that adds or drops the breaking marker (`!` or a
  `BREAKING CHANGE:` footer) moves the key, by design: the verdict reads that
  marker, so an incompatible change that passed on the branch fails on
  `main`, or the reverse.
- A new tag, other content at the tag, a changed breaking marker and a
  shallow clone each move the key. A shallow pull request lane and a full
  `main` lane do not share an entry: their verdicts differ, because a shallow
  clone compares nothing.
- A tag created while a run is in progress is not seen by that run's keys.
- Any runtime input makes a task's key ambient, so the change plan does not
  recompute these keys at another revision.
- An installed CLI older than contract 7 refuses the Go extension that
  declares the input.
