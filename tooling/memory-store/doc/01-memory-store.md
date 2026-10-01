# Memory store semantics

What `@putnami/memory-store` does for each memory operation, identically on
its `file` and `git` backends. The request and result documents are the memory
contract's ([operations](../../../protocols/collaboration/doc/01-operations.md));
this page states what this provider adds and refuses.

## Records

A record is one mission's checkpointed state (`kind: mission`) or a note a
store owner added (`kind: note`). Its reference is
`{"source": "file:<store id>" | "git:<store id>", "id": "<record id>"}`: the
store id is random and written with the first checkpoint, so two stores never
issue the same reference. A mission's id is derived from its workspace,
repository and mission name, so:

- `identity.workspace` (filled by Putnami from the workspace name),
  `identity.repository` (or the `repository` setting) and `mission` name one
  mission; the same mission name in another repository is another mission;
- `identity.scope` is an attribute: a checkpoint that names one sets it, one
  that does not keeps the previous scope. The same holds for `title`.

A checkpoint replaces the mission's `content`, `sources` and `evidence`. The
record keeps them exactly as given, with `provenance.recordedAt` (the
checkpoint time) and `provenance.recordedBy` (the calling agent's client name,
when Putnami propagates it). Evidence entries are references to gate, review,
qualification or session records; no record member states a verdict, and the
provider never adds one.

## Selection

`context` and `search` select a record when:

1. every identity member the request names is the record's, or the record does
   not name that member (a note without a repository applies to every
   repository, one without a mission to every mission);
2. the record's scope and the requested scope overlap, when both name one;
3. its kind is one of `kinds`, when the request names kinds;
4. for a narrowed selection — `projects`, or `impacted` as Putnami resolved
   it — the record names no scope, or its scope overlaps a selected project;
5. for `search`, its title or content contains `query`, ignoring case.

Scopes and project ids compare as `/`-separated paths: two overlap when one
contains the other, so a record about `/tooling` is selected for the project
`/tooling/cli`, and one about `/tooling/cli` for the scope `/tooling`. An
`impacted` selection that found nothing selects only unscoped records. Where a
backend keeps a record plays no part.

Answers are pages of at most `page.size` records (20 by default, 100 at most)
in one order — kind, then record id — and `page.next` is the key of the last
record returned. A checkpoint written between two pages therefore neither
repeats nor skips a record. Every page reads the whole store; `freshness`
says when each record changed (`updatedAt`) and when this answer read it
(`retrievedAt`).

## Revisions and checkpoints

A revision is `<sequence>.<digest>`: the number of writes that produced the
record, and a digest of the whole stored record. It is the same on both
backends and changes on every write.

A stored record is at most 4 MiB, the contract's document bound, so every
checkpoint the contract accepts is written and read back on both backends. A
record that would encode larger is refused before anything is written
(`invalid`, `record.too_large`); a record file larger than the bound is never
read.

`checkpoint` takes exactly one precondition:

| Precondition | Record | Answer |
|---|---|---|
| `mustNotExist` | absent | `ok`, sequence 1 |
| `mustNotExist` | present | `conflict`, `record.exists`, `current` |
| `expectedRevision` | at that revision | `ok`, a new revision |
| `expectedRevision` | at another revision | `conflict`, `revision.conflict`, `current` |
| `expectedRevision` | absent | `not_found`, `mission.missing` |

The comparison and the write are one step. On the file backend both happen
under an exclusive `flock` on `<path>/lock`; the record is written to a
temporary file, synced, renamed over `records/<id>.json`, and the directory is
synced. On the Git backend the new commit moves the branch only from the
commit the checkpoint read: `git update-ref <ref> <new> <old>` locally, `git
push --force-with-lease=<ref>:<old>` to a remote. Of concurrent checkpoints
from one revision exactly one lands; the others answer `conflict` with the
winner's revision. A Git write that lost the race to a checkpoint of *another*
mission is rebuilt on the new head and lands.

## Idempotency and retries

A record keeps digests of the idempotency key and of the request that produced
its current revision. Repeating that checkpoint — same key, mission, identity,
title, content, sources and evidence, whatever precondition — answers `ok`
with `replayed: true` and the landed record, writing nothing. The same key with
other content is `conflict` / `idempotency.mismatch`. Once another checkpoint
has landed, a repeat of the older one answers by its precondition, which is
stale: nothing is written twice.

| Failure | Outcome | Retry |
|---|---|---|
| Invalid settings, a path that is not a store or repository, a checked-out branch | `invalid` | no: fix the binding |
| A record that would encode above 4 MiB (`record.too_large`); nothing was written | `invalid` | no |
| The store is unreadable, the lock is held for more than 10 s, `git` is missing, the remote is unreachable | `unavailable` | as `retryable` says; nothing was written |
| The branch kept moving through 8 attempts or 100 s | `unavailable`, `store.contended` | yes; nothing was written |
| The remote refused the push (a hook or a protection) | `denied` | no |
| A push ended without the remote's answer, and the branch shows it did not land | `unavailable` | yes |
| A push ended without the remote's answer, and the branch cannot be read | `unresolved` | never: read the mission, then repeat the same checkpoint |
| The file store's rename landed and its directory sync failed | `unresolved` | never: as above |
| A stored record the contract refuses, or records without `store.json` (a checkpoint never gives such a store a new identity) | `unavailable`, `store.invalid` | no: repair the store |

After a push ends without an answer, the provider reads the remote branch
before anything else: the checkpoint landed when the branch is its commit or
descends from it. The provider itself never repeats a write. Every Git command
of a checkpoint's attempts ends within 100 s of its start, and that
reconciliation gets 60 s more, so a checkpoint answers within 160 s, before
the 180 s tool timeout.

## The stores

Both backends hold the same files: `store.json` (`{"format": 1, "id": …}`) and
`records/<id>.json` (one record document, `format: 1`). The file backend adds
its lock file `lock` and ignores temporary files. The Git backend commits the
two paths on its branch with a neutral committer (`Putnami memory
<memory@putnami.invalid>`); it runs no hook, signs nothing, never touches a
working tree, index or `HEAD`, and refuses a branch a worktree has checked out.
With a remote it fetches into `refs/putnami/memory/<branch>` of `path`.

A note is a record document a store owner adds under `records/` — for
example `records/release-policy.json` with `kind: note`, a `scope`, `content`,
`provenance.recordedAt` and `updatedAt`. A record the contract would refuse
makes every list answer `unavailable`, naming it.
