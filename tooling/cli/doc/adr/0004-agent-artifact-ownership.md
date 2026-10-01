# ADR 0004 — Agent-artifact materialization owns only what it can prove it wrote

- **Status**: accepted
- **Scope**: `@putnami/cli` (`tooling/cli/internal/agentartifacts`),
  `protocols/workspace` (agent-artifact manifest v1)

## Context

Putnami writes agent content (`.agents/skills/**`, `.claude/**`, `.codex/**`)
into directories the user also edits and commits. Tools that do this usually
diff, merge, or overwrite behind a `--force` that becomes the documented
workflow.

Two facts make the question decidable here. The content manifest binds every
file path to an exact SHA-256, and the extension pin binds that manifest, so
"what should this file contain?" has one answer. The gitignored `.putnami/`
directory can hold bookkeeping that a fresh checkout legitimately lacks.

## Decision

### 1. Ownership is proof, not policy

The materializer owns a path only when it recorded installing it AND the bytes
on disk are still the bytes it recorded.
`.putnami/agent-artifacts/<name>.json` stores both: the managed path list and
the SHA-256 written to each path. Paths alone cannot tell "the user edited our
file" from "our file is untouched".

The table is exhaustive, and each row has a test in
`internal/agentartifacts/*_test.go`:

| Target state | Behavior |
|---|---|
| Missing | create |
| Existing and byte-identical | no-op (adopted without a write) |
| Existing, recorded, still the recorded bytes | update |
| Existing but unrecorded | preserve, collide |
| Recorded but locally modified | preserve, collide |
| Dropped from the content, unchanged since install | remove |
| Dropped from the content, locally modified | preserve, collide |

Byte-identity is checked before ownership. This makes an interrupted run
converge: a file the crashed run already wrote re-plans as unchanged, not as an
unrecorded collision.

### 2. One collision aborts everything, and there is no force

The complete plan is built before the first byte moves. Any collision returns
`ErrCollision` with a report and zero mutations. A partially applied workflow
set is a set nobody tested.

A force-overwrite option is a rejected design, at this layer and at every
command layer: it would become the documented remedy, and the ownership record
would be decoration. Merging is rejected for the same reason: it produces a
file neither side authored. The collision report names each path and its
reason, and never prints file contents, registry URLs, or store locations.

### 3. Undecidable resolves to preserve, never to overwrite

A target that cannot be read, that is a symlink or has a symlinked parent
component, or that is not a regular file is a collision. Preflight
`Lstat`-checks the target and every parent, so a canonical path such as
`.agents/skills/fix/SKILL.md` whose `fix` directory links out of the tree is
never written.

Corrupt ownership state is a hard error naming the file and the remedy, never
a fallback to "nothing is managed": the two readings differ by exactly the user
files a wrong guess would delete.

### 4. Verification finishes before the workspace is read

`internal/agentartifacts` resolves and downloads nothing. The caller hands it
a content tree and its pin (version, identity digest, content manifest
digest), read from the extension release the workspace pins
([ADR 0047](0047-extension-owned-agent-content.md)). The package checks the
manifest digest before trusting the manifest to name a path, and every declared
file's digest before inspecting the workspace. It then plans, applies, and
records.

The materializer writes the manifest's file list, never the tree's listing, so
an undeclared file in the tree is inert. Rejecting undeclared files belongs to
the packager that builds the tree.

### 5. Determinism is structural

The report and the ownership state carry no timestamp, no duration, and no
map. Every collection is a slice sorted by path, and member sets are closed.
Reports are diffed across machines and runs.

### 6. The path rule has one implementation

The CLI checks every path it writes or removes, including paths read back from
its own ownership state, with `wsproto.ValidAgentArtifactPath`, the function the
manifest validator uses. A recorded path is an authorization to delete, so a
second, looser copy of the rule is not allowed.

### 7. Publication order

Apply stages every byte, then publishes with per-file atomic renames. The
ownership record is written after the files, atomically. When recording fails,
the files are already the new bytes; a rerun at the same release re-plans every
file as unchanged and writes the record. Empty directories left by removals are
pruned up to the first non-empty directory; a staging failure removes only
directories staging created.

## Rejected alternatives

- **Own every path under a managed directory.** It overwrites or deletes user
  work.
- **Timestamps or install location as ownership evidence.** Neither proves the
  content supplied the current bytes.
- **Mix ownership with command orchestration.** Lifecycle code could weaken
  the byte-level rules; this package has no command surface.

## Consequences

- A user who edits a shipped workflow blocks its upgrade. Resolving it means
  reverting the edit or moving it to a path the content does not declare.
- A hand-maintained `.agents/` tree is adopted only where it already matches
  byte for byte; every other file reports a collision.
