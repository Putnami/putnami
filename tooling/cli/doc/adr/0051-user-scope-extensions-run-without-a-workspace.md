# ADR 0051 — Extensions pinned in the user scope run without a workspace

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/cli`, `internal/commands/extensions`,
  `internal/extension`, `internal/jobs`, `internal/launch`,
  `internal/commandmeta`), `@putnami/cli-model` (`jobs.ScheduledJob`),
  `go.putnami.dev/protocol/job` (`userScope`)

## Context

A person can install the CLI to run one extension command in a directory that
is not a Putnami workspace. The install script
(`curl -fsSL "https://putnami.dev/install.sh?run=<command>" | bash`) calls two
commands:

1. `putnami extensions install --user --latest <@scope/name[@constraint]>`
2. `putnami <command> [args]`, in the caller's directory

That directory has no `putnami.workspace.json` and must stay exactly as it was.
The extension needs somewhere to be pinned, and the command needs somewhere to
run.

## Decision

### 1. The user scope is a workspace root without a workspace manifest

The user scope is `~/.putnami/user` (`extension.ResolveUserScopeRoot`). It
holds `putnami.lock.json` in the workspace lock format and stable links under
`.putnami/bin/extensions/<name>`, and no `putnami.workspace.json`. Extension
trees stay in the machine-wide artifact store.

`extension.Installer` runs unchanged, rooted at `~/.putnami/user`. A user-scope
pin therefore gets the same download, SHA-256 verification ladder,
`PUTNAMI_UNSAFE_INSTALL` rule, store admission and repair as a workspace pin.

### 2. `extensions install|list|remove --user` manage it

- `install --user` takes one registry extension, `@scope/name[@constraint]`
  (default constraint `latest`), through the workspace install path with an
  empty workspace config. The lock entry changes only when a requested fact
  changes; a re-run downloads nothing.
- The registry is `PUTNAMI_REGISTRY_URL` or the default. No workspace manifest
  is read.
- No extension install hook runs and no AI context is generated; both belong
  to a workspace.
- A local path, `--platform` and `--dest` are usage errors: a local path
  resolves against the current directory, and a materialization targets
  another machine.
- Every writer holds an exclusive lock on
  `~/.putnami/user/.putnami/user-scope.lock` for its read-modify-write.
- Nothing is read or written in the current directory. Inside a workspace, the
  workspace is neither read for the pin nor written.
- `extensions … --user` is exempt from the workspace CLI-pin relaunch
  (`launch.IsExemptInvocation`), because a pinned engine may predate the
  flag. The catalog records it as `WorkspaceNeed.ExemptFlags: ["--user"]`.

### 3. Outside a workspace, the user scope provides the command groups

When no workspace contains the current directory and the first argument could
name an extension command group, the CLI loads the extensions the user-scope
lock pins:

- Each pin is first repaired through `EnsureExtension`, from the store or a
  verified download. A pin that cannot be repaired or loaded is left out with
  a warning naming `putnami extensions install --user <name>` and `--latest`.
- Only the first process of a nested chain repairs. It sets
  `PUTNAMI_USER_SCOPE_REPAIRED` to the user-scope root; every process it
  spawns (such as the registry credential helper) only loads. A pin that
  cannot be downloaded fails once, not once per nesting level.
- Flag-only invocations and built-ins (`--version`, `init`,
  `upgrade --global`, `help`, `completion`) never read the user scope, so a
  broken user scope never breaks them.

Only a subcommand declared `interactive: true` and `workspace: "optional"`
([extension ADR 0008](../../../../protocols/extension/doc/adr/0008-workspace-requirement-and-group-default-need-no-contract.md))
runs. Every other command (flat extension commands, other subcommands,
built-in job commands) prints exactly
`putnami: no workspace found (looking for putnami.workspace.json)` and exits 1.
Project selection flags are usage errors (exit 2). A group's `default`
subcommand applies inside and outside a workspace.

`workspace.FindRoot` also accepts a `package.json` that declares `workspaces`.
Before the pinned-CLI relaunch, `App.Run` gives such a root to the user scope
(`claimPackageRoot`) when all of these hold:

- No `putnami.workspace.json` exists at or above the working directory.
- After aliases, the first argument is not a comma list, a built-in, or a
  command the package root's own extensions provide.
- A user-scope pin that loads provides it as a group.
- The root `package.json` does not declare that extension in
  `devDependencies`.

The run then behaves as outside any workspace: no CLI-pin relaunch, and no
bootstrap writes `.putnami/` into the monorepo. The claim reads files and
spawns nothing. The internal release-set provider and a bound runner request
never read the user scope.

When a user-scope pin does not load, the claim waits for the repair, which
runs after the relaunch and the capability capture: a repair can download, and
the credential helper it spawns must not inherit a capability transport. In a
monorepo that pins its CLI, the pinned CLI decides such a run. When the command
resolves in neither scope, the load warnings print and the package root keeps
the run.

### 4. The job runs in the caller's directory and knows it

The job runs in a synthetic workspace named `user`, rooted at
`~/.putnami/user`, with one project at `.`. Its scratch lease, context file,
output directory and caches live there or in the machine-wide caches. The
process runs in the caller's directory; a task that declares its own `cwd`
still gets it.

The job context v2 carries the optional member
`userScope: {"callerDir": "<absolute path>"}`, mirrored in
`PUTNAMI_CALLER_DIR`. A workspace job carries neither; an inherited
`PUTNAMI_CALLER_DIR` is removed. The member is additive: v1 forbids it, and an
older v2 reader ignores it and treats the user-scope directory as the
workspace.

`userScope` is run state (`ScheduledJob.UserScope`). Nothing derived from it
reaches a cache key or a session record.

### 5. Inside a workspace the user scope does not exist

A workspace discovers only its own extensions. A workspace that pins version A
runs A even when the user scope pins B. No merge, no fallback.

One exception: on a hosted run, or a run that enables the `install` provider,
the user scope's credential provider serves downloads of the locked CLI and
lock-pinned extensions
([ADR 0055](0055-run-credentials-stay-out-of-repository-processes.md)). It adds
no command and loads no workspace extension.

## Enforceable invariants

- A command run outside a workspace writes nothing in the caller's directory.
- The user scope is never read inside a workspace, except for the credential
  provider of ADR 0055.
- A user-scope pin passes the same archive verification as a workspace pin.
- Only an interactive, workspace-optional subcommand runs outside a workspace;
  everything else prints the one no-workspace message and exits 1.
- A job carries `userScope` if and only if it runs outside a workspace.

`internal/cli/e2e/userscope/userscope_e2e_test.go` proves these end to end.

## Known limits

- A task that declares a toolchain resolves it from the user-scope lock, which
  records none. Such a task, or an extension with a managed runtime, may fail.
- The registry credential helper is a nested `putnami` that runs in the
  current directory and finds no workspace there.
- Inside a workspace, `extensions … --user` still passes the generic startup
  that loads the workspace config for aliases; the handler reads nothing there.
- `extensions remove --user` prints text only.

## Rejected alternatives

- **A new lock format for the user scope**: the workspace format already
  records versions and per-platform digests and keeps installer and repair
  unchanged.
- **Trees under `~/.putnami/user`**: the machine-wide store already
  deduplicates; a second copy would diverge.
- **Running the job in the user-scope directory**: the command acts on the
  caller's directory; every extension would have to translate paths.
- **Non-interactive subcommands**: the scheduler plans over workspace projects
  and records sessions under the workspace; neither exists here.
- **The user scope filling gaps inside a workspace**: a command's meaning
  would depend on who runs it, and a workspace could not reproduce its runs.
