# ADR 0061 — A `git:` input keys the executable bit

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/store`), `putnami-extension-sdk`
  (`gitcandidate`), `@putnami/sdd` (`features-validate`)
- **Amends**: [ADR 0041](0041-git-candidate-file-inputs.md)

## Context

A `git:` input keyed each candidate by its path and its bytes, or a symbolic
link by its target text. A task that compares an evidence source binding reads
more than that. A source-v1 binding records each regular file's mode, regular
or executable, and each submodule's checked-out commit, and it refuses an
unmerged path. `features-validate` compares bindings, so a `chmod +x` of a
bound file turned its evidence stale while its `git:**` key stayed put: a
cached entry would have replayed a verdict about another tree. The task stayed
uncached, and it ran 53 times in every CI run.

## Decision

1. **A regular candidate's digest holds its source-v1 mode.** The mode is read
   as the binding reads it (`sourcebinding.FileMode`): from the file's
   permission bits where the host stores the executable bit, and on Windows,
   which stores none, from the mode the index records for a tracked file. An
   untracked file is regular there.
2. **The index is read once per key**, with `git ls-files -z --stage`, the
   read and the parser a source binding uses (`internal/git`). The SDK's
   `Tree.Fingerprint`, which tests use to prove a verdict moves only with its
   key, reads the index the same way.
3. **A selected unmerged candidate produces no key.** It has no single mode,
   and a binding refuses it.
4. **A submodule still produces no key.** A candidate Git lists as a directory
   was already refused (ADR 0041). Keying the submodule's checked-out commit
   would put a commit in a key that ADR 0059 keeps free of commits, so a task
   keyed on the cut runs uncached in a workspace with a submodule.
5. **The key format version stays `v9`.** The regular-file marker changed from
   `git-file` to `git-file-mode`, so every key over a regular candidate moved
   to an address no earlier entry occupies. A key over symbolic links alone
   kept its address and its meaning. ADR 0059 bumps the version only when an
   existing address could be served under another meaning, and none can.

## Consequences

- `features-validate` is cached on `git:**`, with a report that names no commit
  and a view that reads every project at the tree base version (ADR 0060).
- Every task keyed on a regular `git:` candidate misses once after the
  upgrade.
- A task keyed on the cut runs uncached during a merge conflict.
- A CLI older than this change keys a `git:` input without the executable bit.
  A manifest whose verdict reads the bit is exact only under a CLI that keys
  it.
