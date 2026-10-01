# ADR 0028 — The `putnami` repository is the source of its own CLI

- **Status**: accepted
- **Scope**: root `putnami.lock.json`, root `putnamiw`, `@putnami/cli` (`internal/lockfile`, `internal/launch`)

## Context

A workspace's lock can pin the CLI it runs. The launcher (`launch.Relaunch`)
reads that pin and re-execs into the exact binary. Without a pin it runs
whatever binary it already is, so a cold runner's baked CLI and a warm
runner's newer one can regenerate committed output differently, and only one
of them goes green.

This repository does not consume a published CLI. It produces one. A published
pin here means the code under test is never the code in the commit: every CLI
change needs a publish and then a separate lock bump, and between the two the
gate judges a commit with another commit's engine. Consumers pin; a producer
builds.

## Decision

### 1. A published pin is exact and fails closed

This section binds every workspace that pins a published CLI.

- A pin names an exact version, never a channel. A channel moves
  independently of the workspace content, so it cannot guarantee a CLI whose
  generators agree with the committed output.
- The pin records a SHA-256 in `integrities` for every platform that runs the
  workspace. A platform with no digest is a hard error, never a fallback.
- Once pinned, an unreachable registry, a digest mismatch or a failed exec
  stops the run. Each error names its recovery: `putnami pin <version>` or
  `PUTNAMI_NO_RELAUNCH=1 putnami …`. Without a pin, the launcher runs the
  current binary: there is no pin to betray.
- The launcher-exempt invocations (`pin`, `version list/use`,
  `upgrade --from-source`, `migrate vnext`, `extensions … --user`) never enter
  the launcher, so a broken pin is always repairable.
- An ordinary install or context generation never promotes the lock format
  (`WriteLockFile` keeps the file's own version) and never publishes or moves a
  release channel.
- When a generator whose output is committed changes, the pin must move. The
  disagreement shows as a red run on the commit that caused it.

### 2. This workspace's lock carries a source sentinel, not a version

The root lock's `cli` entry is exactly:

```json
"cli": { "source": "workspace" }
```

The literal is `lockfile.SourceWorkspace`, and `LockEntry.IsWorkspaceSource()`
is the only supported test for it. `version` and `integrities` are absent by
construction: a workspace that builds its own engine pins no artifact.
`LockEntry.Version` is `omitempty`, so the sentinel round-trips byte for byte.
Reading, refreshing or writing the lock leaves it unchanged:
`RefreshLockMetadata` stamps no `protocolVersion` on it.

### 3. A source workspace fails closed

In a source workspace the launcher runs a binary only when it proves it came
from this tree. The proof is a pair:

- `PUTNAMI_FROM_SOURCE` names this workspace root, compared as resolved paths,
  because worktrees and symlinked checkouts give one tree several spellings;
- the running binary is the engine `./putnamiw` selected, compared by inode:
  the store blob `$PUTNAMI_HOME/artifacts/cli-source/<PUTNAMI_FROM_SOURCE_KEY>/putnami`,
  or `.putnami/bin/putnami`, which is the only proof on a checkout that has no
  content key.

`./putnamiw` exports both variables and execs the store blob, not the symlink.
The blob is content-addressed and first-writer-wins, so the proof holds for the
whole process tree even when another `./putnamiw` run re-points
`.putnami/bin/putnami`. The launcher resolves the store root as `$PUTNAMI_HOME`,
else `.putnami` under the user home directory, and not through
`store.ResolveArtifactStoreRoot`, which ignores `PUTNAMI_HOME`. CI sets
`PUTNAMI_HOME` per run. The two resolutions are a coupling, commented on both
sides. `Restart` drops both variables, because the CLI it hands control to is
not the pinned blob.

Only two binaries run without the proof:

- a Go test binary, which `go test` built from this tree;
- a launcher-exempt invocation, so `putnami pin <version>` always leaves
  self-hosting.

`PUTNAMI_NO_RELAUNCH` is not an exception. It is the escape from a broken
published pin, and this workspace has none. Every other binary is refused with
an error that names `./putnamiw <the same command>`.

The pair guards against a mistake, not an adversary. `PUTNAMI_FROM_SOURCE` and
`PUTNAMI_HOME` are caller-settable, so a user who plants a binary in a store
they control passes. That takes deliberate filesystem writes, not a command
prefix. Only rebuilding the key on every invocation would close it, at the cost
of a build per command.

### 4. `./putnamiw` builds from the tree, cached by content

The wrapper computes a SHA-256 `SOURCE_KEY` over `git ls-tree HEAD` for
`go.work go.work.sum tooling protocols go`, the dirty overlay from
`git status --porcelain=v1 --untracked-files=all -z` over the same pathspec
with `git hash-object` of each file it names, and the Go toolchain identity and
ldflags. The build lands in `$PUTNAMI_HOME/artifacts/cli-source/<SOURCE_KEY>/putnami`
through staging, a `mkdir` lock and an atomic rename, and is stamped for the
artifact GC. `.putnami/bin/putnami` is an absolute symlink to it.

- The pathspec over-approximates on purpose: over-invalidating costs a relink,
  under-invalidating runs a stale engine that claims to be the commit's own.
- A failed `go build` is a hard failure, with no fallback to a cached or
  downloaded binary: a working stale binary looks exactly like success.
- With no git, no work tree or no SHA-256 tool, no key is computed and an mtime
  gate builds `.putnami/bin/putnami` directly. A missing key never fails a run.
- `--download` is refused in this workspace before it fetches anything; the
  binary it returns is not built from the tree.
- Progress goes to stderr, so a cold build cannot corrupt an MCP handshake or a
  `--output=json` pipeline.

## Rejected alternatives

- **Removing the `cli` entry.** The launcher reads absence as "no pin to
  betray" and runs any binary, so a baked or stale global CLI would gate a
  commit whose CLI source was never compiled. A sentinel is distinguishable
  from silence; absence is not.
- **Keeping a published pin and making the archive public or credentialing
  runners.** It fixes the bootstrap download but keeps the second commit, so
  the code under test is still not the code in the commit.
- **Pinning a channel.** Channel movement is independent of workspace content.
- **Skipping generation of committed context.** It hides generator drift
  instead of making the workspace reproducible.
- **Keying the build cache on mtimes.** A checkout, a rebase, a `touch` or clock
  skew fakes freshness. Content cannot.
- **Letting an admitted engine vouch for its children.** An environment marker
  is forgeable, so it needs a nonce file or an inherited descriptor for no wider
  guarantee.
- **Accepting recent link targets.** It makes the proof time-dependent and still
  fails after two re-links.

## Consequences

- A `tooling/cli` change is tested by the CLI it changes, with no lock change.
- `./putnamiw` is the only supported entrypoint here. A runner that invokes a
  checkout's `./putnamiw` when one exists works; one that invokes a CLI directly
  is refused with an actionable message.
- This workspace cannot run hosted: its extensions are local sources, and a
  hosted runner starts the CLI directly
  ([ADR 0055](0055-run-credentials-stay-out-of-repository-processes.md)).
- Agent hosts must start the MCP server as `./putnamiw mcp`. `.codex/config.toml`
  does. `.mcp.json` keeps `"command": "putnami"`, because the `@putnami/cloud`
  installer requires that exact value and `putnami install` fails otherwise, so
  a Claude MCP server started from the global binary is refused here.
- Consumer workspaces keep published pins under section 1. A workspace without
  `tooling/cli` computes no key and writes nothing to the from-source store.
- A first build costs a compile per tree state per machine. Sibling worktrees at
  the same tree state share one blob.
