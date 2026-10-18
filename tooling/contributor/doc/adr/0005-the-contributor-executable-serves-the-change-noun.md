# ADR 0005 — The contributor executable serves the change noun

- **Status**: accepted
- **Scope**: `@putnami/contributor` (`tooling/contributor`): its manifest, its
  runtime and its callers
- **Supersedes in part**: [ADR 0004](0004-the-contributor-extension-ships-a-compiled-executable.md)
  §1, on the executable answering only the runtime handshake

## Context

Decision D-011 in the root `decisions.json` settles that `@putnami/contributor`
owns the change commands. D-016 names them: `change verify`, `change finalize`
and `change language` replace `tree verify`, `proposals finalize` and
`contributor language`. D-014 settles that an extension command calls the
collaboration providers through an RPC call to the core, made with an SDK
client. ADR 0004 shipped the executable with no command. This record moves the
first two commands, `change language` and `change verify`, and fixes the shape
`change finalize` follows.

## Decision

### 1. A `change` command group in the manifest

The manifest declares `commandGroups.change` with the subcommands `language`
and `verify`. Each routes to a flat command, `change-language` or
`change-verify`, whose task launches `{extensionRuntime}` with the subcommand
name as its argument and is never cached. The manifest declares every flag and
positional each subcommand accepts: `--label`, `--diff`, `--limit` and the mode
for `language`; `--record`, `--workflow`, `--snapshot`, `--ref`, `--gate`,
`--report` and `--base` for `verify`. A launch with those flags prints no
undeclared-flag warning. It keeps `cliContract` 5: nothing it adds needs a
later rung.

The `change-language` task runs in the workspace root. `verify` declares
`workspace: optional` and its task declares no `cwd`: putnami starts it in the
workspace root inside a workspace, and in the caller's directory outside one.
The core's `tree verify` runs in any Git repository, so `change verify` must
too.

### 2. The subcommands are interactive

Both subcommands declare `interactive: true`. putnami bypasses the job
renderer, gives the executable the terminal's standard input, output and error,
and sets `PUTNAMI_INTERACTIVE=1`. The executable reads the text mode's input
from standard input and writes findings or the verdict document the way the
core command does. putnami appends `--putnamiContext <file>` to every launch;
the executable removes that pair before it parses the arguments, because
neither command reads anything from the context.

### 3. The tasks mode calls the core over the host RPC channel

For an interactive launch, putnami opens a host RPC channel and names it in
`PUTNAMI_HOST_RPC` and `PUTNAMI_HOST_RPC_TOKEN`. The tasks mode dials that
channel with the SDK client (`go.putnami.dev/sdk/extension/hostrpc`) and sends
`tasks find` through `CallCollaboration`. The core routes the call through the
same capability, custody and error handling as `putnami tasks find`. The
executable reads no provider configuration and starts no `putnami` process.

`internal/host` adapts the client to the call seam of the language check. It
dials on the first call, so the text, files and commits modes never dial.

### 4. A hosted run has no channel

A build job, a provider tool and a hosted run (`--credential-fd`) receive no
channel. In those runs the tasks mode exits 2 and says it needs an interactive
launch by putnami. It does not fall back to another route to the provider.

### 5. A copy of the check, with the same behavior

`internal/language` holds a copy of the core's language check: the
French-signal evaluator, the four modes and the path exclusions. The copy keeps
the core's findings and exit codes (0 no finding, 1 findings, 2 usage or tool
failure). It differs in two places. The call seam returns an error when a call
never reaches a provider, which the core cannot report. The exclusion list also
names the copy's evaluator, whose signal vocabulary is data.

The executable runs the check in its working directory, which is the workspace
root, and reads Git state with the same read-only operations as the core. Those
operations run without the host channel variables in their environment.

Known differences from `putnami contributor language`:

- **(a) Trailing line.** The core prints a final `putnami: contributor language
  failed` line when the command exits 2. The executable does not.
- **(b) Relative paths.** A relative path in `text <file>` resolves against the
  workspace root, because the task runs in `{workspaceRoot}`. The core passes the
  caller's directory only outside a workspace.
- **(c) Messages.** Every message names `change language`, the command the user
  typed, where the core's name `contributor language`.

### 6. The core deletes `contributor language`

The core no longer serves `contributor language`: `putnami contributor …` is a
usage error that links the reference of moved commands. The core keeps its
French-signal evaluator in `internal/commands/shared`, because
`proposals finalize` applies it to the proposal it publishes. The core's
`tree verify` keeps working until a later slice deletes it.

### 7. `change verify` reads restored reports over the host channel

`internal/verify` holds a copy of the core's verifier: the five modes, the
dossier, gate and qualification checks, and the workflow record validation. It
prints the same verdict document and exits with the same code: 0 when the
evidence is verified, 1 with `{"verdict":"not-verified","reason":"..."}` on one
line otherwise. It computes the tree fingerprint with the SDK package
`go.putnami.dev/sdk/extension/treefingerprint`, the implementation the core
uses.

A workflow record can name a verification report that a gate restored from the
task cache. The core reads such a report from the cache itself. The executable
asks the putnami process that launched it, with the host RPC method
`verification.report`, which runs the same cache check for the workspace that
process loaded. `internal/host` dials on the first read, so a verification
that restores no report never dials. Outside a workspace the host refuses the
call, because there is no cache to read.

Without a channel, a restored report is not verified, with a reason that names
`putnami change verify` and the two missing variables. Every other check needs
no channel and gives the same verdict with or without one. The verifier does
not trust a restored report it cannot have the host corroborate.

The executable takes the channel out of its environment before it starts any
process: Git, and the workspace CLI that answers the native dry-run plan, never
receive the socket or its token. That plan comes from the workspace's own
`putnamiw`, which may be a build of the workspace's source.

The core marks repository code as started before it runs that plan, which
guards the core's run credential. The executable holds no run credential, so it
marks nothing.

Known differences from `putnami tree verify`:

- **(a) Help.** `--help` prints the help putnami builds from the manifest,
  where the core prints its catalog help. Run without putnami, the executable
  prints a usage line that names `change verify`.
- **(b) Undeclared flags.** The core rejects an undeclared flag with a usage
  error on stderr and exits 2. For `change verify`, putnami warns on stderr
  that undeclared flags are deprecated and launches the executable, which
  prints a not-verified verdict, `unknown argument: <flag>`, and exits 1. The
  warning says putnami will reject such flags in the next minor release.
- **(c) Working directory.** Inside a workspace the executable starts in the
  workspace root, so the Git top level comes from the workspace root. The core
  uses the caller's directory. They differ only when the caller stands in a
  nested repository inside the workspace.
- **(d) Report cache.** The host reads a restored report from the cache of the
  workspace putnami loaded. The core reads the cache under the Git top level.
  Outside a workspace the host refuses, where the core still reads the cache
  under the Git top level.
- **(e) No channel.** Without a channel only a restored report is not
  verified, and its reason names the channel. The core has no such case.
- **(f) Deadline.** The task has a 10-minute deadline. The core command has
  none.
- **(g) Run credential.** The executable marks no repository code as started.
- **(h) Child environment.** Git and the workspace CLI run without the host
  channel variables. The workspace CLI also inherits the environment putnami
  gives an interactive launch, such as `PUTNAMI_INTERACTIVE` and
  `PUTNAMI_WORKSPACE_ROOT`, where the core passes the user's environment
  unchanged.
- **(i) Internal errors.** An error that is not a verdict, such as a failed
  write to standard output, prints `change verify: <error>` where the core
  prints `putnami: <error>`. Both exit 1.
- **(j) Outside a workspace.** The command runs only when @putnami/contributor
  is installed in user scope and the global `putnami` launches it. The core
  command needs no extension.

## Invariants

- `change language` reports the same findings and exit codes as `contributor
  language` for every mode that needs no provider. Goldens recorded from the
  core command pin this.
- `change verify` prints the same verdict document and exit code as `tree
  verify` in every mode. Goldens recorded from the core command, on a
  deterministic Git fixture, pin this.
- Only the tasks mode of `change language`, and a restored report in `change
  verify`, dial the host channel.
- No process the executable starts receives the host channel variables.
- The executable never reads a file of another project and never imports
  `tooling/cli/internal`.
- The runtime's `prepare` inputs include `internal/**`, so an edit to the check
  rebuilds the runtime.

## Rejected alternatives

- **Run `putnami tasks find` as a subprocess.** D-014 settles an RPC call, and
  a subprocess starts a second putnami with its own provider resolution.
- **Import the core's packages.** The module may never import
  `tooling/cli/internal`, and the core no longer has the four modes.
- **A job-runner command instead of an interactive one.** The job renderer
  would capture the findings and the host opens no channel for a job.

## Consequences

- The check skill and its audit helper call `putnami change language`.
- The build skill and its references call `putnami change verify`. A consumer
  needs @putnami/contributor with the command; outside a workspace, it needs
  the extension installed in user scope.
- The core keeps a copy of the verifier, which `tree verify` and
  `proposals finalize` use, and a copy of the evaluator, which
  `proposals finalize` uses. A behavior change to a core copy needs the same
  change in the executable's copy.
- `change finalize` follows the same pattern: a subcommand in the `change`
  group, an interactive launch, and the host channel for anything that needs
  the core.
