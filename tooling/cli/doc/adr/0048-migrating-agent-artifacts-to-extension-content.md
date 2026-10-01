# ADR 0048 — Separate agent artifacts move to extension content in one explicit, journaled step

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/commands/agentctx`, the `migrate`
  command), the `agentArtifacts` workspace declaration, lock format v4
  `agentArtifacts`, the ownership records under `.putnami/agent-artifacts/`

## Context

A workspace that declared its workflows as separate artifacts (a registry
reference such as `@putnami/agent-workflows:stable`, or an in-tree path such as
`/tooling/agent-workflows`) holds three pieces of state per artifact: a
`putnami.workspace.json` entry, a lock pin (registry artifacts only), and an
ownership record in each clone that installed it. The CLI no longer installs
those forms ([ADR 0047](0047-extension-owned-agent-content.md) §2). An
extension whose content names them in `agentContent.supersedes`
([protocol ADR 0006](../../../../protocols/extension/doc/adr/0006-agent-content-is-an-additive-contract.md))
ships the same workflows under its own pin.

Moving a consumer changes all three pieces and possibly the files, because
merging artifacts into one content is a real content change. Retiring by hand
is unsafe: it removes old files before the new content is planned, releases
edited files that then collide as unmanaged, and loses the ownership history.

## Decision

### 1. One explicit command, never a side effect

`putnami migrate agent-content <extension> [--check | --apply | --rollback]`
is the only thing that moves a consumer. `--check` is the default, writes
nothing, and exits 2 while a migration is pending, so CI can gate on it.

No install, upgrade, or first-run pass migrates. Each refuses an opt-in while
this clone still records a superseded artifact, and names the command.
`upgrade` is rejected as the carrier: it would fuse a version move with an
ownership move that a consumer must review separately.

### 2. Nothing is resolved

The extension must already be installed at the release the lock pins, or be
declared by path. The migration reads its content through the same verified
chain `install` uses and never contacts a registry. A declared but unpinned
extension fails with `putnami install` as the next step. No release is ever
substituted.

### 3. Two halves, both shown

The **mechanical** half:

- the superseded `agentArtifacts` entries give way to one `extension:<name>`
  entry, placed where the first of them was;
- their lock pins are dropped;
- their ownership records merge into the extension's record.

The **semantic** half is `BuildPlan` of the extension's content against the
merged ownership history, with the [ADR 0004](0004-agent-artifact-ownership.md)
table unchanged. The report lists every path added, changed, removed, or
released, with before and after digests, and counts unchanged paths.

The superseded set is the intersection of `agentContent.supersedes` with what
the workspace declares, pins, or records. A clone that pulled a teammate's
migration commit still holds the old records, so `--apply` there moves only the
ownership.

An in-tree entry is named by its project's `putnami.json`. When that project is
gone, the entry is named by the one superseded artifact whose unscoped name is
the path's last segment (`/tooling/agent-workflows` names
`@putnami/agent-workflows`). An entry that names no superseded artifact, or
several, stays declared as it is.

### 4. Ownership history is proof, merged without guessing

Each superseded artifact hands over this clone's record. An artifact this clone
never recorded hands over no ownership: the content is still planned against
the files on disk, so a file identical to what it ships is adopted by bytes, and
a file with other bytes blocks the migration with nothing written. Nothing the
migration never proved is removed.

Two owners recording one path with the same digest merge. Two owners recording
one path with different digests are refused as duplicate ownership, naming
both. A content path that an artifact outside the migration still records is
refused for the same reason.

### 5. Edited files block or are released, never overwritten

- A path the content ships that the user edited, or that nobody recorded,
  blocks the whole migration with zero writes.
- A path only a superseded artifact shipped, which the user edited, is kept,
  reported, and released, as retirement does.

There is no force flag.

### 6. A journal makes every stop recoverable

The migration is planned in full before its first write. The first write is a
journal under `.putnami/agent-content-migrations/<extension>/` that records:
the content release, the declarations and workspace config digests before and
after (with the original config bytes kept beside it), each superseded
artifact's pin, record and handed-over history, the extension's record before
and after, every path written or removed with its digests, and a verified copy
of every file the migration replaces or removes. The document is published
atomically after the copies. The pieces then move in a fixed order:

1. files (transactional apply);
2. the extension's ownership record;
3. the lock (one write);
4. the workspace declarations (one atomic write);
5. the superseded records.

The journal is marked complete last.

- **Resume.** `--apply` continues from the journal against the same content
  release. A different release is refused, naming `--rollback`.
- **Rollback.** `--rollback` drives every piece back in reverse order and
  removes the journal last. Files come from the verified copies. The workspace
  config gets its original bytes while it is still what the migration wrote;
  otherwise only `agentArtifacts` is put back. The restored old-form state is
  refused by this CLI and installed by an earlier release.
- **Recorded states only.** Both directions accept each piece only in its
  recorded before or after state. A piece changed since is refused with zero
  writes.
- **Ordinary commands** refuse to act while a journal is partway, and name the
  command that finishes or undoes it. A complete journal is inert and keeps
  rollback available.
- **One rollback point per extension.** While a complete journal exists, a new
  migration to the same extension is refused in `--check` and `--apply`, naming
  `--rollback` and the journal directory (whose removal gives up the rollback).
- **One writer.** `--apply` and `--rollback` hold an exclusive, non-blocking
  lock on `.putnami/agent-content-migrations.lock` for the whole run; a second
  run fails with nothing changed. The lock file is never deleted, because
  deleting a file another process has open would let two runs each hold "the"
  lock.
- **Versioned format.** The journal format (version 1) is compared exactly. A
  journal this CLI cannot read stops every agent-content command; the remedy is
  to remove its directory.

### 7. Compatibility

A workspace that has not migrated either migrates with this CLI or stays on an
earlier release, which installs its pins as before. `supersedes` is contract-5
vocabulary, so a CLI that predates contract 5 refuses the extension package. A CLI that predates the `extension:` opt-in
reads the entry as an unpinned registry artifact named `extension` and fails
instead of skipping the content. No reader silently drops migrated state.
After the migrated pieces change, the way back is an ordinary change on an
earlier release.

## Enforceable invariants

- A migration writes nothing until the whole plan is free of blocking
  collisions and duplicate owners.
- Ownership records and the lock move only after the files were applied.
- Every stop leaves a journal from which `--apply` finishes and `--rollback`
  restores; neither overwrites a piece outside its recorded states.
- The migration and its rollback are the only writers of lock
  `agentArtifacts`, apart from retirement dropping the pin of a retired record.
- The migration resolves no version and substitutes no release.

## Rejected alternatives

- **Migrate implicitly when an old form is found.** An install that reassigns
  ownership of files the user may have edited is the silent behavior the
  ownership rules forbid.
- **Roll back by re-materializing the old artifacts.** Their release may no
  longer be fetchable, and an in-tree source is deleted once its consumer
  migrates. The journal's copies restore exactly what was there.
- **Rely on `git revert` alone.** It restores declarations and pins but not the
  clone-local records, so every restored file reads as unmanaged.
- **Write pins or declarations first and revert on failure.** The revert is a
  second write that can fail on the same disk.
- **Keep adoption by the old release's bytes for clones without records.** It
  keeps a download path and the old builder alive, and a copy identical to the
  new content is adopted anyway.
