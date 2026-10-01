# Agent Workflows

Agent workflows reach a workspace in one way: as the **agent content** of an
extension the workspace declares and opts into. The CLI materializes that
content **without ever overwriting a file you own**. Putnami's own workflows
ship as the agent content of one extension,
[`@putnami/contributor`](../../contributor/README.md); every built-in starter
declares it and opts into its content, and this repository opts into it too.

Workflows declared as separate artifacts — a registry reference such as
`@putnami/agent-workflows:stable` or an in-tree project path such as
`/tooling/agent-workflows` — are no longer installed. Every command refuses
them, writes nothing, and names `putnami migrate agent-content`, which moves
them to the extension whose content supersedes them. See
[Migrating separate artifacts](#migrating-separate-artifacts-to-an-extensions-content).

The materializer lives in `internal/agentartifacts`, and `init`, `install`,
`upgrade` and `context generate` drive it. Three documents split the
responsibility: [ADR 0004](adr/0004-agent-artifact-ownership.md) decides what
happens to a file that already exists,
[ADR 0047](adr/0047-extension-owned-agent-content.md) decides who asks, where
the version comes from, and that an extension is the only way agent content is
installed, and
[ADR 0048](adr/0048-migrating-agent-artifacts-to-extension-content.md) moves a
workspace off separately declared artifacts.

## The three onboarding layers

| Layer | What it supplies | Ownership |
|---|---|---|
| The guidance block in `AGENTS.md` and `CLAUDE.md` | the workflow bootstrap: the pinned command, the selection rules, the MCP routing rule, and a pointer to `.agents/constraints.md` when the workspace writes one | Putnami owns the block; every other byte, and `.agents/constraints.md` itself, belongs to the workspace |
| Host MCP registration and `.mcp.json` | local Putnami structural tools and lock-resolved extension tools for Claude Code and Codex | machine installer; `init`, `install` and `upgrade` add a missing `.mcp.json` entry, and `putnami mcp install` repairs one; host trust remains authoritative |
| Agent content | versioned repeatable workflow files | the extension that ships them, through its lock pin and content manifest |

The layers are independent. A host without MCP uses `./putnamiw`, or `putnami`
when no wrapper exists, plus focused file reads. The pinned command remains the
required path for build, test, lint, validation, install, dependency, and
generation work even on an MCP-aware host.

These entrypoints apply before read-only analysis as well as before a change.
They remain in force when a host resumes a session or delegates to a subagent;
the host supplies the active session/worktree to the stable launcher instead of
Putnami storing a first repository globally. When a lock-resolved extension
advertises Intelligence, structural questions use those tools without a
workspace argument, accept only indexed freshness for the revision being
reasoned about, and fall back to the local Putnami tools in one turn. Exact
literal text and constants still use focused file search. The user does not need
to remind an agent to use Intelligence or copy a workspace slug into a prompt.

## What the content carries

An extension's agent content (`agentContent` in its manifest; see the
[extension manifest](../../../protocols/extension/doc/01-manifest.md#agent-content))
is skills, worker profiles, references and helpers, written for every supported
host: `.agents/skills/**`, `.claude/skills/**`, `.claude/agents/**` and
`.codex/agents/**`. `@putnami/contributor` ships `plan`, `execute`, `fix`,
`epic`, `check`, `code-review`, `fix-loop`, `content-bump` and their workers;
see its [contributor journey](../../contributor/doc/01-contributor-journey.md).

Excluded, permanently:

- host policy: `.codex/config.toml` and `.claude/settings*.json` — repository
  permissions and model policy are repository decisions, not shipped defaults,
- local MCP credentials, `.agents/constraints.md`, `.context/**`, and any private release or
  archive material,
- generated caches and workspace-specific configuration.

The content's file list is its **manifest** (`putnami.agent-artifact.json`), and
the materializer writes the manifest's list — never whatever happens to be in
the archive. A member the manifest does not declare is inert.

## How a workspace opts in

By declaring the extension in `extensions` and opting into its content in
`agentArtifacts`, in `putnami.workspace.json`, and in no other way:

```json
{
  "extensions": ["@putnami/contributor"],
  "agentArtifacts": ["extension:@putnami/contributor"]
}
```

Installing an extension that carries content activates nothing: only the
`extension:<name>` entry does. The entry names no version — the content always
has the version the extension resolves to, so there is one version to install,
pin and upgrade. Nothing is pinned under `agentArtifacts` either: the
extension's own lock entry is the pin, and it binds the content end to end. The
lock pins the extension's manifest digest, the manifest pins the content
manifest digest, and the content manifest pins every file.

An absent or empty `agentArtifacts` means every command skips agent content
entirely — no lock read and no output line. There is no flag, no environment
variable, and no directory sniffing that turns the feature on: one declaration,
in a file you commit, that every command agrees about.

Removing the `extension:` entry is how you remove the content. The next
`putnami upgrade` removes every file the content still manages and that you
have not changed, keeps any you edited — reported, and no longer managed — then
drops the ownership record; the extension itself stays installed. It retires
only what this clone recorded. `putnami install` and the implicit first-run
pass never retire anything, and `putnami upgrade --dry-run` lists what would go.

An `extension:` entry for an extension the workspace does not declare fails the
command, as does one naming an extension that ships no content. Content from
several extensions composes: disjoint paths coexist, and an overlap aborts the
whole run before the first write.

When an upgraded extension's content collides with a file you edited, the
extension itself has already moved in the extensions phase, but none of its
content is written: your files and the ownership record stay at the previous
release, the command fails naming the file, and the next `putnami install`
reports the same collision until you revert or move the edit.

### Content this workspace authors

An extension declared by path (`/tooling/contributor`) is this workspace's own
source. Its authored content is built on every run with the packager's builder,
and nothing is fetched or pinned: the committed source is the pin. The built
tree is staged in this worktree under a version derived from the emitted files
(`0.0.0-local-<12 hex>`), so an unchanged source rebuilds into the same
directory and the ownership record moves exactly when the content does.

`putnami context generate` rebuilds the content of local extensions, so an
author who edited the source regenerates the workspace copy with the command
that regenerates the rest of the generated guidance. A hand-edited generated
copy is preserved and blocks the run like any other managed file. The Putnami
repository works this way: it edits `tooling/contributor/src`, runs
`./putnamiw context generate`, and commits both.

### Migrating separate artifacts to an extension's content

An extension's content replaces workflows installed as separate artifacts when
its manifest names them in `agentContent.supersedes`; `@putnami/contributor`
supersedes `@putnami/agent-workflows` and `@putnami/maintainer-workflows`. This
CLI never installs a separately declared artifact: every command refuses the
declaration with nothing written and names the migration. Every command also
refuses an `extension:` opt-in while this clone still records an artifact the
content supersedes, because two ownership records would claim the same files.

To migrate:

1. Declare the extension in `extensions` in `putnami.workspace.json` and
   install it. `putnami install` pins it at an exact release; the migration
   itself never resolves or downloads anything. The install fails at its agent
   step while the old declarations remain — that failure names the next step.

   ```bash
   putnami install
   ```

2. Review the plan. Nothing is written, and the command exits 2 while a
   migration is pending.

   ```bash
   putnami migrate agent-content @putnami/contributor
   ```

   The plan has two halves:

   - **Moves** (mechanical): each superseded `agentArtifacts` entry gives way
     to one `extension:@putnami/contributor` entry, its lock pin is dropped, and
     its ownership record merges into the extension's record.
   - **Content** (semantic): every file the extension's content adds, changes
     or no longer ships, and every edited file it releases. Merging two
     artifacts into one content can change bytes, and this list shows each
     change.

3. Apply it.

   ```bash
   putnami migrate agent-content @putnami/contributor --apply
   ```

4. Commit `putnami.workspace.json`, `putnami.lock.json` and the host files.

5. In every other clone, pull the commit and run the same `--apply`. The
   declarations and pins have already moved, so it only moves that clone's
   ownership records. A clone of the Putnami repository runs
   `./putnamiw migrate agent-content @putnami/contributor --apply` once after
   pulling the change that removed `tooling/agent-workflows` and
   `tooling/maintainer-workflows`.

What the migration protects:

- **Your edits.** A file you edited that the content still ships blocks the
  migration with nothing written: revert or move it, then rerun. A file you
  edited that the content no longer ships is kept, reported as `released`, and
  no longer managed.
- **Ownership.** Ownership moves only with a record. A clone without records
  (one that commits its generated files) hands over none: the content adopts
  the files already identical to what it ships, and a file with other bytes
  blocks the migration as yours until you move it. Two records that claim one
  path with different bytes are refused as duplicate ownership, naming both.
- **Interruption.** The whole migration is planned before its first write.
  That write is a journal under `.putnami/agent-content-migrations/`, holding
  every piece's before and after state and a copy of every file it replaces.
  The pieces then move in order: files, the extension's record, the lock, the
  declarations, the old records. If a run stops, rerun `--apply` to finish.
  Until you do, other agent-content commands refuse to act. `--apply` and
  `--rollback` hold `.putnami/agent-content-migrations.lock`, so a second run
  started meanwhile changes nothing and says so.
- **Rollback.** `putnami migrate agent-content @putnami/contributor --rollback`
  restores the previous declarations, pins, ownership records and files,
  byte for byte. It works in the clone that ran the migration, until something
  it moved changes. It never overwrites a change made since. The restored state
  is the separately declared one, so this CLI refuses it again; an earlier
  release that still installs separately declared artifacts installs it. The journal of a completed migration stays as that rollback
  point, so a second migration to the same extension is refused until you roll
  the first one back, which lets the next `--apply` move everything at once,
  or remove its directory under `.putnami/agent-content-migrations/` to give
  the rollback up.
- **Older CLIs.** A clone whose CLI predates the `extension:` opt-in fails on
  the migrated workspace instead of running it without its content. It reads
  `extension:@putnami/contributor` as an agent artifact named `extension` on
  the channel `@putnami/contributor`, which no lock pins, and writes nothing:

  ```text
  putnami: agent workflows install: extension: workspace lock pins no agent artifact: putnami.lock.json records no extension entry
  ```

  Its `putnami upgrade` looks that artifact up in the registry and fails the
  same phase, because the registry serves none. A CLI before contract 5 also
  refuses the extension package itself with `extension requires a newer
  putnami`. Every one of these messages means the same thing: move that
  clone to a CLI release that carries the opt-in (the workspace's CLI pin),
  then pull and run the migration's `--apply`.

The same command moves in-tree artifacts (`/tools/workflows`) to a local
extension declared by path. Nothing is pinned before or after, and
`putnami context generate` regenerates the result as a no-op. An in-tree entry
whose project was already deleted, for example by a pulled commit, is named by
its last path segment: `/tooling/agent-workflows` names the superseded
`@putnami/agent-workflows`. An entry that names no superseded artifact stays
declared as it is and does not block the migration. See
[ADR 0048](adr/0048-migrating-agent-artifacts-to-extension-content.md).

## Where the version comes from

From the extension's lock pin, only. `putnami install` reads the content out of
the exact release `putnami.lock.json` pins for the extension, and verifies the
chain before the workspace is read: the installed manifest hashes to the
digest the lock records, the manifest's `agentContent.manifestSha256` matches
the content manifest, and every file matches the content manifest's digest. A
local extension has no pin at all; see the previous section.

An npm extension is the exception, because a package manager installs it. The
root `package.json` declares it in `devDependencies`, the package manager's lock
pins it, and discovery loads its commands and tools from `node_modules/<name>`.
Its content is read from that same package, after the package manager ran:
`putnami install` materializes it after the workspace installers, and
`putnami upgrade` after its dependency phase. The package's `package.json` must
carry the extension's name and the version its manifest declares; the rest of
the chain is the same. Declare such an extension only in `devDependencies`: an
`extensions` entry of the same name is refused, because the two can be
different releases.

A fresh clone or worktree does not need an explicit install either. The
implicit pass a job command (`build`, `test`, `lint`, `serve`, …) runs before
its first job — the same one that materializes lock-pinned extensions and
templates — materializes the agent content of every opted-in extension the
committed lock pins. Built-in commands such as `projects list` or
`context generate` do not run it. It is lock-authoritative and best-effort: an
unpinned extension is left to `install`, a collision writes nothing, the lock
is never written, and it never retires content — though it does apply the
pinned release's own plan, so a file a newer release stopped shipping is
removed. Once the files match it records ownership once, and after that it only
reads and hashes them.

## Lifecycle: init, install, upgrade

| | `putnami install` | `putnami upgrade` |
|---|---|---|
| Version source | the extension's committed lock pin, only | the extension release its extensions phase moved to |
| Rewrites the lock for agent content | never | never — the extension's own pin moves in the extensions phase |
| Extension declared but unpinned | installed and pinned by the extensions phase first | resolved and pinned by the extensions phase first |

### `putnami init`

Every built-in starter (`--extension ts|go|py`) declares the
`@putnami/contributor` extension and opts into its content. `init` writes both
declarations into the new `putnami.workspace.json`, installs the extension —
which pins it — and materializes its content. If the extension cannot be
installed, both declarations stay, no agent file is written, and `init` reports
the failure and carries on — your dependencies and project are still created —
with `putnami install` as the command that installs it later. A starter that
opts into nothing adds nothing: `init` never enrols a workspace beyond what its
starter names.

`init --force` re-writes the workspace config of a directory that may already be
a workspace. It **merges** the starter's opt-in with the declarations already
there, by what each entry names, and your committed entry wins, so
re-initializing cannot un-declare content whose files are installed. A
separately declared artifact survives the merge too; the next command then
names the migration instead of orphaning its files.

### `putnami install`

The committed lock is authoritative. `install` never resolves a version for
agent content and never rewrites the lock for it, so a clone, a fresh worktree
and a CI runner all reproduce exactly the files the lock describes.

### `putnami upgrade`

`upgrade` (or `upgrade --extensions`) moves each extension in its extensions
phase, then plans the content of every opted-in extension at the new release,
and only then writes. The order is the guarantee:

```
verify           → nothing in the workspace has been read
plan             → nothing in the workspace has been written
abort on collision → nothing at all has changed
apply            → files published, transactionally
record ownership → the managed-path proof, last
```

So a collision or a digest mismatch leaves the previous files and the previous
ownership record. `--dry-run` reports the opt-ins and what would be retired
without resolving or downloading anything, exactly like every other upgrade
phase.

### At agent session start

A worktree keeps the files of the lock it was installed with until something
materializes the new pin. So the MCP server, which a host starts when an agent
session starts, reconciles before it answers its first request: for each
opted-in extension whose ownership record names another release than the
committed pin, or no record, it materializes that release's content from the
installed extension. A record that names the pin ends the pass for that
extension after one small read. The pass never downloads, never resolves, never
writes the lock, and never builds a local extension's content. A collision with
a file you edited, or any other failure, is a warning on the server's stderr,
and the session starts with the files it had; `putnami install` finishes the
job. See
[ADR 0040](adr/0040-register-the-mcp-server-and-reconcile-agent-workflows-at-session-start.md).

### The full matrix

| Situation | What happens |
|---|---|
| `init` with a starter that opts into content | both declarations are written, the extension is installed and pinned, and its content is materialized |
| `init` with a starter that opts into nothing | nothing is added: no declaration, no files, no install |
| An existing workspace with no `agentArtifacts` | untouched by every command; the phase is skipped before any lock read |
| `install` with the extension pinned | exactly the content manifest's files are materialized at that release; nothing moves in the lock |
| The same `install`, run again | byte-for-byte no-op — no file and no ownership record changes |
| `upgrade` where the content bytes did not change | no file diff |
| `upgrade` where managed bytes changed | the files it owns are replaced, dropped paths are removed, and the ownership record moves last |
| A file the content declares that no run installed | **preserved**; the run aborts before the first write and the error names the path |
| A managed file you edited since it was installed | **preserved**; your edit and the previous record both survive |
| A tampered content file or a digest that does not match the pin | refused before the workspace is read; nothing written |
| A separately declared artifact (`name`, `name:channel`, `/path`) | refused by every command with nothing written, naming `putnami migrate agent-content <extension>` |
| An opt-in while this clone still records an artifact the content supersedes | refused with nothing written, naming `putnami migrate agent-content <extension> --apply` |
| An agent session starts in a worktree whose record names another release than the pin | the pinned content is copied from the installed extension before the first request; a collision is a warning, never a refused session |

Errors name the owner and every conflicting path with its reason, and nothing
else: no file contents, no credentials, no registry URL, no store location.

There is no force-overwrite flag on any command. See
[ADR 0004](adr/0004-agent-artifact-ownership.md) §2 for why that is a rejected
design rather than a missing feature.

## Host and integration matrix

| Setup | Structural discovery | Workflow behavior |
|---|---|---|
| MCP-aware, core only | read-only Putnami MCP tools first | local plans, changes, checks, and reviews; no external account required |
| Non-MCP, core only | pinned wrapper/CLI plus focused file reads | the same local workflows and verification contract |
| MCP-aware with optional integrations | Putnami tools for workspace facts; host capabilities only when requested | core stays local by default; each external action requires explicit intent |
| Non-MCP with optional integrations | wrapper/CLI for Putnami facts; separately configured integration | no source-host/tracker coupling or inferred remote writes |

The public installer registers user-scoped stdio servers for Claude Code and
Codex when their CLIs are present. It preserves an existing `putnami` definition
instead of repairing or replacing human JSON/TOML. Claude project registrations
remain subject to the one-time `.mcp.json` approval; Codex project configuration
loads only for a trusted project. A running host keeps its current inventory
until the server is reconnected or a new session starts. The qualified host
versions for this path are Claude Code `2.1.260` and Codex CLI `0.153.4`.

The server receives the active directory per session. Claude supplies
`CLAUDE_PROJECT_DIR`; Codex inherits the session cwd and stores no configured
`cwd`. Putnami enters that directory before the lock-pinned launcher replaces
the process, so a host launched directly in a worktree does not reuse the
repository that was open during installation. Ordinary subagents share that
session root. Claude keeps an already-spawned server's project root stable when
the session later enters or resumes another worktree; reconnect in that root,
or use the local fallback for an isolated subagent whose host shares the parent
server. Before analysis, resume, or delegation, compare the root announced by
MCP initialization with the active cwd/worktree; a mismatch skips that server
immediately and uses local Putnami in the active root.
An older Putnami pin is still honored and therefore cannot expose capabilities
introduced by a newer CLI until the workspace explicitly moves that pin.

Putnami inserts one block into your `AGENTS.md` and `CLAUDE.md`; everything
else in those files is yours. The block sits between
`<!-- putnami:guidance v2 begin sha256:… -->` and `<!-- putnami:guidance v2 end -->`,
and the checksum covers the lines between the two markers:

| File | Absent | Present, no block | Present with an intact block |
|---|---|---|---|
| `AGENTS.md` | created as a heading plus the block | block inserted after a leading `# ` heading (or at the top); every other byte kept | block refreshed in place |
| `CLAUDE.md` | created as `# CLAUDE.md` plus `@AGENTS.md` | block inserted the same way, unless the file already imports `@AGENTS.md` | block refreshed in place |

An edited block is preserved and reported, never overwritten. These rules are
independent of the ownership records described below.

`putnami context generate --force` is the way back. It rewrites an edited block;
it never changes a byte outside the block.

Besides the block, Putnami writes one file into a workspace root: the
`putnami` entry of `.mcp.json`. `.agents/constraints.md` is a file you write
yourself when you have repository rules to state, and Putnami only reads it
([ADR 0055](adr/0055-repository-rules-live-in-agents-constraints.md)).

`putnami init`, `putnami install` and `putnami upgrade` add the `putnami` entry
to `.mcp.json` when it is missing, so a new agent session finds the server with
no manual step. They merge into an existing file, preserve other servers and
unknown keys, keep a diverged `putnami` entry as written, and leave a file they
cannot parse untouched with a warning. `putnami context generate` and the
first-use install that runs before another command never write it.
`putnami mcp install` is the explicit form: it also rewrites a diverged entry,
and it refuses a file it cannot parse
([ADR 0040](adr/0040-register-the-mcp-server-and-reconcile-agent-workflows-at-session-start.md)).

## Ownership: what happens to files that already exist

The materializer records what it installed — the managed paths **and** the
SHA-256 of the bytes written to each — under `.putnami/agent-artifacts/`, which
is gitignored. That record is what makes "you edited this" decidable.

| Your file | What happens |
|---|---|
| Does not exist | created |
| Already byte-identical to the content | left alone |
| Installed by a previous run, untouched since | updated |
| Exists but no previous run installed it | **preserved**, reported as a collision |
| Installed by a previous run, edited since | **preserved**, reported as a collision |
| Dropped by the new release, untouched since install | removed |
| Dropped by the new release, edited since install | **preserved**, reported as a collision |

A target whose path (or whose parent directory) is a symlink, or which is not a
regular file, is also a collision: a symlink is never followed for a write.

**Any collision aborts the whole run before the first write.** Nothing is
partially applied, and no ownership state is recorded. There is deliberately no
force-overwrite flag — see
[ADR 0004](adr/0004-agent-artifact-ownership.md) for why that is a rejected
design rather than a missing feature.

"The whole run" means every opted-in extension's content, not just the one that
collided. If your workspace opts into two extensions and the second one's
content hits a collision, the first is not materialized either: all of them are
planned before any of them is written.

To resolve a collision, look at the file the report names:

- if the difference does not matter, delete or revert your version and rerun —
  the path then reports as created or updated;
- if it does matter, move your version to a path the content does not declare.
  Workflow trees are additive; a file the manifest never mentions is never
  touched.

## Reports

Every run produces one report, in two renderings that list the same paths in the
same order. The machine rendering is a closed set of members with no timestamp
and no duration, so two runs over the same inputs produce byte-identical bytes:

```json
{
  "name": "@putnami/contributor",
  "version": "1.4.2",
  "archiveDigest": "…",
  "manifestHash": "…",
  "applied": true,
  "created": [".agents/skills/plan/SKILL.md"],
  "updated": [".agents/skills/execute/SKILL.md"],
  "removed": [".claude/skills/check/SKILL.md"],
  "unchanged": [".claude/skills/code-review/SKILL.md"],
  "collided": []
}
```

`applied` is false only when the run aborted on a collision. A no-op run is
applied — it simply had nothing to change.

## Interruption and recovery

Writes are staged first (every byte, to a temp sibling of its target) and only
then published, one atomic rename per file. A run killed at any point leaves a
mix of old and new bytes plus a few temp siblings, and **rerunning converges**:
files that were already published re-plan as unchanged, files that were not
re-plan as create or update. The ownership record is committed last and only on
success, so a crashed run never claims a file it did not write.

Removals run after the publishes, and prune the directories they empty — the
prune stops at the first directory that still holds anything, so a directory you
put your own file into survives.

### If recording ownership fails

Publication is atomic per file, which means that once files are published there
is nothing to roll back *to* — the previous bytes are gone. A failure after that
point (a full disk, a read-only `.putnami/`) is reported rather than hidden, and
the workspace is left with the new files and the old ownership record, a state
that **converges on a rerun**. Rerun the same command at the same release.
Every published file re-plans as `unchanged` — content equality is checked
before ownership — so the rerun writes no file bytes and simply records them.

## Private registries and credentials

If `PUTNAMI_REGISTRY_URL` carries credentials — userinfo (`https://user:token@…`)
or a signed query token — neither appears in an agent-content error message:
failure text keeps the host and drops the rest.

## Corrupt ownership state

If `.putnami/agent-artifacts/<name>.json` is unreadable, unparseable, or records
something this CLI would not write, the run stops with a diagnostic that names
the file and states the remedy. It does **not** fall back to "nothing is
managed": that reading is quieter, and it resolves every subsequent question in
the direction of touching your files.

Deleting the state file is the documented recovery. After that every path it
covered is treated as user-owned — preserved and reported as a collision — until
you confirm each one.
