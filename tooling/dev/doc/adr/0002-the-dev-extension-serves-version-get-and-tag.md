# ADR 0002 — The dev executable serves `version get` and `version tag`

- **Status**: accepted
- **Scope**: `@putnami/dev` (`tooling/dev`): its manifest, its runtime and its
  Go module
- **Supersedes in part**: [ADR 0001](0001-the-dev-extension-declares-the-development-verbs.md)
  §1, on the manifest declaring no command, command group, task or runtime,
  and §4, on the executable answering only the runtime handshake
- **Decisions**: D-010, D-014 and D-018 of the root `decisions.json`;
  [ADR 0071](../../../cli/doc/adr/0071-an-extension-command-calls-the-core-over-a-host-rpc-channel.md)
  of `@putnami/cli`

## Context

D-010 settles that the core CLI computes a tree's version before any cache
key, and that `version get`, `version tag` and the changelog belong to
`@putnami/dev`. D-018 makes `@putnami/dev` the owner of the `version` noun.
D-014 settles that an extension command calls the core through an RPC call
made with an SDK client. The core's host RPC channel already answers
`version.lines` (ADR 0071): each version line with its version, the release
it proposes, and the commits since its last release.

## Decision

### 1. A runtime and a `version` command group

The manifest declares a `runtime`: the executable `compiled/putnami-dev`,
which `bin/prepare` builds with the Go release `putnami.lock.json` pins, as
`@putnami/contributor` does. It declares `commandGroups.version`, whose
default is `get`, with the interactive subcommands `get` and `tag`. Each
routes to a flat command, `version-get` or `version-tag`, that declares every
flag the subcommand accepts: `--scope` for `get`; `--scope`, `--push`,
`--dry-run` and `--yes` for `tag`. `tag` declares `--dry-run` because putnami
forwards that global flag only to a command that declares it. Each command's
task runs the runtime with `version get` or `version tag` in the workspace
root, is never cached, and has a 10-minute deadline. The manifest keeps CLI
contract 11 and declares no traits.

### 2. The executable reads the version lines over the host channel

`internal/host` removes the channel variables from the environment before the
command starts any git process, and dials on the first call.
`internal/version` asks `version.lines` for the line `--scope` names, or for
every line, and prints or releases from the answer. It computes no version.
It reads the output mode and `--dry-run` from the job context putnami
appends.

### 3. The same output and exit codes, with known differences

`version get` prints `  <line> <version>` for each line, or one result
envelope under `--output=json` or `--output=jsonl`. `version tag` keeps its
order: it writes the changelog, commits it, puts the annotated tag on that
commit and, with `--push`, pushes HEAD and the tag. It keeps every refusal
of the core with the core's exit code. It exits 2 for several lines without
`--scope`, a tagged HEAD, a dirty tree, an existing tag, a tag off the line's
pattern, a run bound to another commit, and structured output. It exits 1 for
a dependency graph the host could not refresh, as the core did, and for a
failure of git. It also exits 2 when the host proposes no release and no tag
is named, which the core never met. A refusal of the host keeps the exit code
the core gave the same error: a scope that names no line and an invalid
configuration exit 2, and a failure of the host exits 1.

Known differences from the core's commands:

- **(a) Messages.** A message loses the `putnami: ` prefix of a core error.
  The refusal of structured output by `version tag` no longer lists the
  core's structured-output commands.
- **(b) Usage errors.** An extra argument or `--scope` without a value exits
  2, where the core exited 1.
- **(c) Envelope name.** `putnami version --output=json` names its command
  `version get` in the envelope, where the core named it `version`.
- **(d) A malformed `PUTNAMI_SOURCE_REVISION`.** `version get` exits 2 with
  the host's invalid-configuration message, where the core exited 1.
- **(e) Order of refusals.** `version tag` checks its arguments before the
  dependency graph, and asks for the version lines before it compares the
  bound commit with HEAD. When two refusals apply, the one it reports can
  differ from the core's.
- **(f) Help and completion.** Once the core deletes its commands (§6),
  `putnami --help` lists `version` under "Commands (from extensions)", not
  among the core's commands. Completion generated inside a workspace that
  loads `@putnami/dev` offers `version` with `get` and `tag`. Completion
  generated outside a workspace does not, because no extension is loaded
  there.

`tooling/cli/internal/cli/dev_version_parity_test.go` compares the executable,
launched through the core's dispatch with the shipped manifest, with the
answers the core's built-in commands recorded on the same fixture. It applies
differences (a) and (b) as documented substitutions.

### 4. A hosted run opens no channel

A hosted run (`--credential-fd`) opens no host RPC channel, so neither
command runs there. Without a channel, a command exits 1 and names the
interactive launch it needs. No hosted flow runs them.

### 5. Not a recovery command

The core ran `version` in a workspace whose configuration it could not fully
load. An extension command runs only after discovery loads the workspace's
extensions, so the moved commands need a loadable workspace.

### 6. The core deletes its commands

The core deletes `version get`, `version tag` and its changelog code. It keeps
the version computation and `version.lines`. In a workspace without
`@putnami/dev`, `putnami version` is a usage error that links the reference of
moved commands.

## Consequences

- A workspace that loads a language extension keeps both commands, because
  every language extension depends on `@putnami/dev`. A workspace without
  `@putnami/dev` loses them.
- A release that runs `putnami version tag` needs an interactive launch: a
  terminal or a CI step, not a hosted run.
- The changelog renderer exists only in the extension.
