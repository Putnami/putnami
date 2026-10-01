# ADR 0001 — One memory provider, two backends

- **Status**: accepted
- **Scope**: `@putnami/memory-store` (`tooling/memory-store`)

## Context

The memory contract of `go.putnami.dev/protocol/collaboration`
([ADR 0001](../../../../protocols/collaboration/doc/adr/0001-collaboration-provider-contracts.md))
lets skills load context and checkpoint a mission without knowing where memory
lives. A workspace needs two places to keep it without a hosted service: a
directory on the machine, and a Git repository it owns, so several machines
share memory through a remote. Both must pass the same contract scenarios,
including concurrent checkpoints, and neither may bundle a repository identity
or a credential.

## Decision

1. **One extension, two backends chosen by `settings.backend`.** `file` keeps
   records in a directory; `git` keeps them on a branch of a local or remote
   Git repository. The setting is required: an absent or unknown backend is
   invalid. One engine over a two-method backend interface runs one scenario
   suite against both. Capability discovery echoes the settings, so the backend
   in use is visible. One manifest describes both, so every tool declares
   `openWorldHint: true`, and each description says only the Git backend with a
   remote reaches the network.

2. **One store layout, never a selection rule.** Both backends hold
   `store.json` (format, random store id) and `records/<id>.json` (one record
   document, format 1). A mission's id derives from its workspace, repository
   and mission name, so every backend finds a mission at the same id and two
   repositories sharing a store never share a mission. References are
   `{"source": "file:<store id>" | "git:<store id>", "id": <record id>}`. Every
   operation refuses a store holding records without `store.json`, and a
   checkpoint never gives it a new id, which would change every reference's
   source. What a context or search answer holds depends on record identity,
   kind and scope only (identity members equal or absent, scopes overlapping as
   `/`-separated paths, the orchestrator's resolved project selection), never on
   file names. Notes are record documents a store owner adds; the contract has
   no operation that writes them.

3. **Revisions are content-derived and monotonic:** `<sequence>.<digest>`, where
   the sequence counts the record's writes and the digest covers the whole
   document. The same document has the same revision on either backend, and a
   write always produces a new one. A stored record is bounded by the
   contract's 4 MiB document bound, on write as on read. The largest checkpoint
   the contract accepts stores in 1,150,434 bytes
   (`TestAMaximalCheckpointIsReadBackOnEveryBackend`). The encoder refuses a
   larger record (`invalid`, `record.too_large`) before either backend writes,
   so no write lands that a read would refuse.

4. **Compare-and-set is the backend's own primitive.** The file backend
   compares and writes under an exclusive `flock` on the store's lock file,
   then replaces the record by temporary file, sync, rename and directory sync.
   A lock held longer than 10 s is `unavailable`; a platform without `flock`
   refuses every write. The Git backend moves a local branch with
   `git update-ref <ref> <new> <old>` and a remote branch with
   `git push --force-with-lease=<ref>:<old>` (`<ref>:` for a branch that must
   not exist yet). A write whose condition failed wrote nothing: it re-reads
   the store and is rebuilt on the new head when its record is unchanged, or
   answers `conflict` when its record changed, within 8 attempts. Every command
   of a write ends within 100 s of its start, and reconciling an uncertain push
   gets 60 s more, so a checkpoint answers before the 180 s tool timeout.

5. **An uncertain write is reconciled first, and never retried.** A push that
   ends without a status line for its ref may have landed. The provider reads
   the branch again: the write landed when the branch is its commit or descends
   from it, and did not when the branch is still at the base (`unavailable`,
   retryable). When the branch cannot be read the checkpoint is `unresolved`,
   not retryable, and names the reconciliation: read the mission, then repeat.

6. **Idempotency lives in the record.** A record keeps SHA-256 digests of the
   idempotency key and of what its checkpoint asked for (identity, title,
   content, sources, evidence; not the precondition). Repeating the checkpoint
   that produced the current revision replays it; the same key with other
   content is `conflict` / `idempotency.mismatch`. Once another checkpoint
   lands, a repeat of the older one is judged by its now stale precondition, so
   nothing is written twice.

7. **The Git backend touches nothing but its branch.** Commits are built with
   plumbing in a private index. `core.hooksPath` points at the null device,
   nothing is signed, background maintenance is off, and the committer is
   `Putnami memory <memory@putnami.invalid>`; records carry their own
   provenance. Inherited repository-locating variables (`GIT_DIR`,
   `GIT_INDEX_FILE`, …) are dropped, a branch a worktree has checked out is
   never moved, and the path is never searched upward for a repository. A
   remote is a remote name, a URL or a path. One that begins with `-`, or whose
   URL carries a password, an HTTP user, a query or a fragment, is refused, and
   failure messages strip userinfo, query and fragment from every URL. Git's
   credential helpers and SSH agent authenticate; `GIT_TERMINAL_PROMPT=0` makes
   a missing credential fail instead of waiting. Reads with a remote fetch it,
   because a stale answer would look current.

8. **Memory stays contextual.** A record has no member that states a verdict;
   evidence entries are stored as given and prove nothing their records do not.

9. **Placement.** The provider is a Go extension that compiles its runtime
   through `bin/prepare`, like `@putnami/local-collaboration`.

## Consequences

- A workspace changes backend by editing `options.collaboration.memory`;
  skills and helpers are unchanged.
- The default stores (`.putnami/collaboration/memory`,
  `.putnami/collaboration/memory.git`) belong to one worktree. Memory that must
  outlive it sets an absolute `path` or a `remote`.
- Every list answer reads the whole store, so cost grows with the record count;
  one record per mission keeps it small.
