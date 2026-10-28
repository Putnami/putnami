# Agent Workflows

Agent workflows reach a workspace in one way: as the **agent content** of an
extension the workspace declares and opts into. The CLI materializes that
content **without ever overwriting a file you own**. Putnami's own workflows
ship as the agent content of one extension,
[`@putnami/contributor`](../../contributor/README.md); no built-in starter
declares it. A workspace asks for it with
`putnami extensions install @putnami/contributor`, which declares it in
`extensions`, opts into it with `extension:@putnami/contributor` in
`agentArtifacts` and materializes its content. This repository opts into it too.

Workflows declared as separate artifacts — a registry reference such as
`@putnami/agent-workflows:stable` or an in-tree project path such as
`/tooling/agent-workflows` — are no longer installed. Every command refuses
them and writes nothing. See
[Replacing separate artifacts](#replacing-separate-artifacts-with-an-extensions-content).

The materializer lives in `internal/agentartifacts`, and `init`, `install`,
`upgrade` and `context generate` drive it. Two documents split the
responsibility: [ADR 0004](adr/0004-agent-artifact-ownership.md) decides what
happens to a file that already exists, and
[ADR 0047](adr/0047-extension-owned-agent-content.md) decides who asks, where
the version comes from, and that an extension is the only way agent content is
installed.

## The two onboarding layers

| Layer | What it supplies | Ownership |
|---|---|---|
| The guidance block in `AGENTS.md` and `CLAUDE.md` | the workflow bootstrap: the pinned command, the selection rules, the discovery rule, and a pointer to `.agents/constraints.md` when the workspace writes one | Putnami owns the block; every other byte, and `.agents/constraints.md` itself, belongs to the workspace |
| Agent content | versioned repeatable workflow files | the extension that ships them, through its lock pin and content manifest |

The layers are independent. An agent uses `./putnamiw`, or `putnami` when no
wrapper exists, plus focused file reads. The pinned command is the required path
for discovery, build, test, lint, validation, install, dependency, and
generation work. Putnami serves no protocol server and writes no host
registration.

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
`.codex/agents/**`. `@putnami/contributor` ships the daily `build` entry,
focused `spec`, `design`, `prototype`, `review`, `adr` and `check` interventions,
and the explicit-only `content-bump` utility with its workers;
see its [contributor journey](../../contributor/doc/01-contributor-journey.md).

`build` coordinates only the requested outcome, including its issue or epic.
It plans observable delivery units, challenges material proposals independently,
implements and repairs through one loop, and derives progress from exact artifact
references and actual evidence. `putnami change verify --workflow`, which
`@putnami/contributor` serves, checks its separately versioned local record; a
planning verdict cannot establish readiness. An optional `workflow` reference
extends a v2 execution dossier without changing old dossiers or closed hosted review reports. The
[workflow record contract](../../contributor/doc/04-workflow-contract.md) describes
stages, materialization and review responsibilities.

Framework recipes reach `build/references/recipes.md` from the committed sample
indexes. `putnami change language`, which `@putnami/contributor` serves, and
the core's `putnami doctor --machine` replace workflow-owned shell
implementations; `putnami tree fingerprint` supplies the native tree binding.
`putnami change finalize`, which `@putnami/contributor` serves, is a local
publication command when a proposals provider is configured, not an operation
added to the provider contract. It does not run a local gate. Publication, merge and deployment still follow the
existing mandate and repository controls.

Excluded, permanently:

- host policy: `.codex/config.toml` and `.claude/settings*.json` — repository
  permissions and model policy are repository decisions, not shipped defaults,
- local host credentials, `.agents/constraints.md`, `.context/**`, and any private release or
  archive material,
- generated caches and workspace-specific configuration.

The content's file list is its **manifest** (`putnami.agent-artifact.json`), and
the materializer writes the manifest's list — never whatever happens to be in
the archive. A member the manifest does not declare is inert.

## How a workspace opts in

By declaring the extension in `extensions` and opting into its content in
`agentArtifacts`, in `putnami.workspace.json`, and in no other way.
`putnami extensions install <name>` writes both entries for the extension it
names, when that extension ships agent content:

```json
{
  "extensions": ["@putnami/contributor"],
  "agentArtifacts": ["extension:@putnami/contributor"]
}
```

Installing an extension that carries content activates nothing: only the
`extension:<name>` entry does. Naming the extension on `extensions install` is
the request that writes it; an extension installed only because another one
requires it gets no entry. The entry names no version — the content always
has the version the extension resolves to, so there is one version to install,
pin and upgrade. Nothing is pinned under `agentArtifacts` either: the
extension's own lock entry is the pin, and it binds the content end to end. The
lock pins the extension's manifest digest, the manifest pins the content
manifest digest, and the content manifest pins every file.

An absent or empty `agentArtifacts` means every command skips the agent phase
entirely — no lock read and no output line. `install` still drops any pin
under `agentArtifacts` in `putnami.lock.json`. There is no flag, no environment
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

### Replacing separate artifacts with an extension's content

An extension's content replaces workflows installed as separate artifacts when
its manifest names them in `agentContent.supersedes`; `@putnami/contributor`
supersedes `@putnami/agent-workflows` and `@putnami/maintainer-workflows`. This
CLI never installs a separately declared artifact: every command refuses the
declaration with nothing written. Every command also refuses an `extension:`
opt-in while this clone still records an artifact the content supersedes,
because two ownership records would claim the same files. No command moves
them for you: `putnami migrate agent-content` is deleted (decision D-013).

To replace them:

1. In `putnami.workspace.json`, declare the extension in `extensions`, and
   replace the separately declared `agentArtifacts` entries with one
   `extension:<name>` entry, such as `extension:@putnami/contributor`. The
   refusal of the old entries gives the steps that apply to your workspace,
   and names the extension when a declared extension's content or a
   first-party successor supersedes them.
2. Delete the ownership record of each superseded artifact. The refusal names
   each one, for example
   `.putnami/agent-artifacts/putnami-agent-workflows.json`.
3. Install the extension and its content:

   ```bash
   putnami install
   ```

   A file that already has the content's bytes is kept. A file with other
   bytes is a collision: the install writes nothing and names the file, so
   you move or delete it, then rerun. A file the old artifacts installed that
   the content does not ship stays where it is, and no command manages it.
   The install also drops every pin under `agentArtifacts` in
   `putnami.lock.json`: no declaration uses one.
4. Commit `putnami.workspace.json`, `putnami.lock.json` and the host files.
   Every other clone pulls the commit, deletes the records its install
   refusal names, and runs `putnami install`.

For in-tree artifacts (`/tools/workflows`), declare the local extension by
path instead; nothing is pinned, and `putnami context generate` builds its
content.

A clone whose CLI predates the `extension:` opt-in fails on the updated
workspace instead of running it without its content. It reads
`extension:@putnami/contributor` as an agent artifact named `extension`, which
no lock pins, and writes nothing. Move that clone to a CLI release that carries
the opt-in (the workspace's CLI pin).

## Where the version comes from

From the extension's lock pin, only. `putnami install` reads the content out of
the exact release `putnami.lock.json` pins for the extension, and verifies the
chain before the workspace is read: the installed manifest hashes to the
digest the lock records, the manifest's `agentContent.manifestSha256` matches
the content manifest, and every file matches the content manifest's digest. A
local extension has no pin at all; see the previous section.

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

No built-in starter (`--extension ts|go|py`) opts into agent content, so `init`
does not install `@putnami/contributor` and writes no agent-content declaration.
To ask for it, run `putnami extensions install @putnami/contributor`, which
declares it, opts into its content and materializes it; a second run, or
`putnami install` afterwards, changes nothing. When a starter does name an agent-content extension, `init` writes both
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
refuses it instead of orphaning its files.

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

### Reconcile moments

A worktree keeps the files of the lock it was installed with until something
materializes the new pin. `putnami install` and `putnami upgrade` are the
moments that reconcile them. No session start does.

### The full matrix

| Situation | What happens |
|---|---|
| `init` with a starter that opts into content | both declarations are written, the extension is installed and pinned, and its content is materialized |
| `init` with a starter that opts into nothing | nothing is added: no declaration, no files, no install |
| An existing workspace with no `agentArtifacts` | the agent phase is skipped before any lock read; `install` still drops any pin under `agentArtifacts` |
| `install` with the extension pinned | exactly the content manifest's files are materialized at that release; nothing moves in the lock |
| The same `install`, run again | byte-for-byte no-op — no file and no ownership record changes |
| `upgrade` where the content bytes did not change | no file diff |
| `upgrade` where managed bytes changed | the files it owns are replaced, dropped paths are removed, and the ownership record moves last |
| A file the content declares that no run installed | **preserved**; the run aborts before the first write and the error names the path |
| A managed file you edited since it was installed | **preserved**; your edit and the previous record both survive |
| A tampered content file or a digest that does not match the pin | refused before the workspace is read; nothing written |
| A separately declared artifact (`name`, `name:channel`, `/path`) | refused by every command with nothing written, giving the steps that replace it with an `extension:<name>` entry and naming that extension when it is known |
| An opt-in while this clone still records an artifact the content supersedes | refused with nothing written, naming each ownership record to delete |
| A pin under `agentArtifacts` in `putnami.lock.json` | dropped by `install`; a hosted install fails and names it instead |
| An agent session starts in a worktree whose record names another release than the pin | the pinned content is copied from the installed extension before the first request; a collision is a warning, never a refused session |

Errors name the owner and every conflicting path with its reason, and nothing
else: no file contents, no credentials, no registry URL, no store location.

There is no force-overwrite flag on any command. See
[ADR 0004](adr/0004-agent-artifact-ownership.md) §2 for why that is a rejected
design rather than a missing feature.

## Host and integration matrix

| Setup | Structural discovery | Workflow behavior |
|---|---|---|
| Core only | `./putnamiw` (or `putnami`) queries plus focused file reads | local plans, changes, checks, and reviews; no external account required |
| Optional integrations | the same queries for workspace facts; separately configured integrations only when requested | core stays local by default; each external action requires explicit intent, with no source-host/tracker coupling or inferred remote writes |

The queries are `putnami projects list|describe|owner|deps|why`,
`putnami context map|pack` and `putnami <verb> --impacted --dry-run`. An older
Putnami pin is still honored and therefore cannot expose capabilities
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

Putnami writes no other file into a workspace root. `.agents/constraints.md` is
a file you write yourself when you have repository rules to state, and Putnami
only reads it
([ADR 0055](adr/0055-repository-rules-live-in-agents-constraints.md)).

An earlier Putnami wrote a `putnami` entry to `.mcp.json`. `putnami install` and
`putnami upgrade` remove that entry when it is byte for byte the one Putnami
wrote, and delete the file when nothing else remains. A changed entry stays as
written, with a warning that names the file. A file they cannot parse stays
untouched, with a warning.

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
  "created": [".agents/skills/build/SKILL.md"],
  "updated": [".agents/skills/review/SKILL.md"],
  "removed": [".claude/skills/plan/SKILL.md"],
  "unchanged": [".claude/skills/spec/SKILL.md"],
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
