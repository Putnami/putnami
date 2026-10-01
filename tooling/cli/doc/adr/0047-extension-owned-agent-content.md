# ADR 0047 — An extension is the only way agent content is installed, and the extension's pin is its pin

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/commands/agentctx`, `internal/extension`,
  `internal/commands/lifecycle`), the `agentArtifacts` workspace declaration,
  the built-in starters

## Context

An extension ships agent content under its own version
([protocol ADR 0006](../../../../protocols/extension/doc/adr/0006-agent-content-is-an-additive-contract.md)).
The lifecycle must answer four questions that are not about any one file:

- **Who asks?** A workspace that installs an extension for its commands must
  not receive its workflows unasked.
- **Where does the version come from?** The extension already has a pin. A
  second pin for its content is a second resolution of the same release, and
  the two drift.
- **What order?** Files, ownership records, and the lock are separate durable
  state. A failure must never leave a workspace whose state disagrees.
- **Which rules write the files?** [ADR 0004](0004-agent-artifact-ownership.md)'s,
  or the content gets a second materializer with weaker guarantees.

## Decision

### 1. `extension:<name>` in `agentArtifacts` is the only opt-in

A command materializes agent content exactly when `putnami.workspace.json`
lists `extension:<name>` in `agentArtifacts`. No flag, no environment
variable, no detection of an existing `.agents/` directory. An empty or absent
array skips the phase entirely: no read, no lock access, no output line.

The entry carries no constraint and no path: the content always has the
version the extension resolves to. `<name>` resolves in discovery's order: an
`extensions` entry, else a root `package.json` `devDependencies` entry, read
from the committed manifest. Declaring an extension alone activates none of
its content. An entry fails the command before any file is read when it names
an undeclared extension or one without content.

### 2. Old entry forms are refused, and the refusal names the way out

A registry reference (`name`, `name:channel`) or an in-tree path (`/path`) is
a form this CLI no longer installs. Every agent-content command (`install`,
the agent phase of `upgrade` and `upgrade --dry-run`, `context generate`, the
first-run ensure pass, the session-start reconcile) refuses it with nothing
written. The refusal is an invalid-configuration failure (exit 2) that quotes
the entries and names `putnami migrate agent-content <extension>`
([ADR 0048](0048-migrating-agent-artifacts-to-extension-content.md)). It names
the extension in this order:

1. an unfinished migration, because only finishing or rolling it back resolves
   the entries it left;
2. a declared extension whose `agentContent.supersedes` names the artifact;
3. the first-party successor (`@putnami/agent-workflows` and
   `@putnami/maintainer-workflows` → `@putnami/contributor`), with the step
   that declares it;
4. a `<extension>` placeholder.

The ensure pass stays best-effort and the reconcile stays a warning, so an
unmigrated workspace can still run the migration. Old entries are otherwise
read only by the migration, by retirement, and by `putnami init --force`.

### 3. The extension's pin binds every byte

The content is never pinned under the lock's `agentArtifacts`. The CLI reads
it from the directory discovery loads the extension's commands from, so
commands and instructions never come from two releases:

- **Registry extension.** The lock must pin the extension with a manifest
  digest. The manifest discovery loads (behind the workspace's stable link, or
  in the pinned release's artifact directory when no link exists) must hash to
  that digest and carry the packaged contribution. The content tree must match
  `manifestSha256` and every file digest before the workspace is read.
- **npm extension** (a root `devDependencies` entry). The content is read from
  `node_modules/<name>`; the package manager's lock pins it, and the CLI does
  not read that lock. The package's `package.json` must name `<name>` (an alias
  is refused), the extension manifest `version` must equal the package version,
  and the manifest must carry the packaged contribution; a source-only package
  is refused with a message naming the path declaration.
- **Local extension** (declared by path). Its authored content is built from
  source with the SDK builder on every explicit command, staged in the worktree
  under a content-derived `0.0.0-local-<12 hex>` version, and never pinned.

One name reaches content through exactly one source. A registry `extensions`
entry and a devDependency of the same name, or a registry entry that
`node_modules/<name>` also provides, are refused with nothing written. A local
entry shadows a devDependency of the same name.

The ownership record names the extension version, the extension manifest
digest (in the record's identity-digest slot), and the content manifest digest.
The npm case reads its declaration and install directory only through
discovery's adapter (`internal/extension/discovery.go`), so content follows
discovery when that adapter changes.

### 4. Ordering: every contribution is planned before any is written

Every command that writes content runs one order across all opted-in
contributions:

```
for each contribution: resolve + verify, then plan   (nothing written)
abort if ANY plan collides, or two owners claim one path,
  or one owner's file is an ancestor of another's target
apply retirement removals (section 6), then plan again
for each contribution: apply, then write its ownership record
drop retired pins in one lock write, then delete retired records
```

A per-contribution loop would satisfy each guarantee for one contribution and
none for the run. A run whose resolved release equals the recorded one writes
nothing, not even identical bytes, because an mtime change is visible to
watchers and build inputs.

`upgrade` moves the extension pin in its extensions phase, before the agent
phase plans the content. When the new content collides with a user edit, no
content file or record moves, but the extension pin has moved: until the edit
is resolved, the workspace runs the new extension with the previous
instructions, and `install` keeps reporting the collision. Holding the pin
back would need planning content before its extension is installed.

When an opted-in extension is an npm package, the agent phase of `install`
runs after the workspace installers, and the agent phase of `upgrade` (and
`upgrade --deps`) runs after the dependency phase, because that is when
`node_modules` moves. When those phases fail, the agent phase is skipped and
reported, and the previous content and record stay. `upgrade --extensions` and
workspaces without an npm opt-in keep the phase in its normal place. Always
deferring it is rejected: a content-only workspace often has no language
extension, its installers fail, and its content would never install.

### 5. What each command does

| Command | Agent content |
|---|---|
| `install` | Reads the release the committed lock pins, or the installed npm package; resolves nothing, writes no lock entry. Repeated, it changes no byte. |
| `upgrade` | Follows the extension its extensions phase moved; retires undeclared owners (section 6). `--dry-run` reports the opt-ins and the owners it would retire, and resolves and downloads nothing. |
| `context generate` | Builds only local extensions' content. |
| `init` | Installs the starter's extension, then its content. `--force` merges the existing `agentArtifacts` by the name each entry resolves to; the committed entry wins, and old-form entries are kept. |
| First-run ensure pass | Materializes content of a lock-pinned extension or an installed npm package before a job command; skips local, unpinned, and uninstalled ones; never writes the lock; never retires. Warm path: the record names the pin and every recorded file still has its digest. Files that match with no record get a record and nothing else. |
| Session-start reconcile | See [ADR 0040](0040-register-the-mcp-server-and-reconcile-agent-workflows-at-session-start.md). |

Every built-in starter declares `@putnami/contributor` in `extensions` and
opts into its content. A failure in `init`'s agent step is reported, not
fatal: both declarations stay, no agent file is written, and `putnami install`
is the next step.

### 6. Only `upgrade` retires, and only from this clone's record

An owner whose ownership record this clone holds and that the workspace no
longer opts into is retired by `upgrade`: every unchanged file the record
manages is removed, the owner's lock pin (an old-form entry) is dropped in the
run's single lock write, and the record is deleted last, so an interruption
leaves a record that still authorizes the rest. An edited file is released:
kept, reported, and no longer recorded. The user asked for the owner to go, so
keeping the edit loses nothing. Removing the `extension:` entry retires the
content; the extension stays installed.

Retirement runs after every opted-in contribution is verified and planned, and
refuses with zero writes any collision retirement does not clear. It removes
files before the final plans, so a rename that ships the same paths (`@acme/a`
to `@acme/b`) takes them over instead of colliding. A rename whose new content
cannot land keeps the old content and record.

Retirement keys on the local record, never on the lock alone: a committed pin
with no record in this clone authorizes no removal. It also requires proof the
declarations were read: an unparseable `putnami.workspace.json` stops the
phase instead of reading as "declares nothing".

## Enforceable invariants

- An installed extension without an `extension:` entry writes no agent file.
- Content is never pinned under `agentArtifacts`; every byte written is bound
  to the extension's lock pin, its installed npm package, or its local source.
- No command installs, resolves, downloads, or builds an old-form entry; each
  one fails every agent-content command with nothing written.
- Collisions, symlinked destinations, and cross-owner overlaps abort the whole
  run with zero writes; retirement keeps edited files.
- A repeated install is a byte-for-byte no-op; an interrupted run converges on
  a rerun at the same release.
- The implicit passes never write the lock and never retire.
- This repository's committed `.agents`, `.claude`, and `.codex` trees are
  exactly what `@putnami/contributor`'s source builds, apart from hand-authored
  audit files (`TestLocalAgentContentMatchesCommittedRoot`).

## Rejected alternatives

- **Activate content when the extension is declared.** Installing an extension
  for its commands would enrol the workspace in its workflows.
- **A second opt-in field, or opt-in inside the extension's options.** Two
  surfaces for one decision disagree the moment one is edited.
- **Pin content separately under `agentArtifacts`.** Two pins for one release
  drift apart.
- **Keep installing old-form entries beside extension content.** Two install
  modes for one set of workflows is the drift this removes.
- **Ignore old entries silently.** The workspace loses its workflows with no
  message and keeps unmanaged files.
- **Write the lock first and revert on failure.** The revert is a second write
  that can fail on the same disk.
- **Read the package manager's lock for npm content.** Bun, npm, and pnpm each
  have a format; the installed package already names its version.
