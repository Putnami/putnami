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
first command, `change language`, and fixes the shape the other two follow.

## Decision

### 1. A `change` command group in the manifest

The manifest declares `commandGroups.change` with the subcommand `language`.
The subcommand routes to the flat command `change-language`, whose task
launches `{extensionRuntime}` with the argument `language` in the workspace
root and is never cached. The manifest declares the flags `--label`, `--diff`
and `--limit` and the positional mode, so a launch prints no undeclared-flag
warning. It keeps `cliContract` 5: nothing it adds needs a later rung.

### 2. The subcommand is interactive

`language` declares `interactive: true`. putnami bypasses the job renderer,
gives the executable the terminal's standard input, output and error, and sets
`PUTNAMI_INTERACTIVE=1`. The executable reads the text mode's input from
standard input and writes findings the way the core command does. putnami
appends `--putnamiContext <file>` to every launch; the executable removes that
pair before it parses the arguments, because the language check reads nothing
from the context.

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

### 6. The core copy stays for now

The core's `contributor language` keeps working. A later slice deletes it,
with its copy of the check, once the other change commands have moved.

## Invariants

- `change language` reports the same findings and exit codes as `contributor
  language` for every mode that needs no provider. Goldens recorded from the
  core command pin this.
- Only the tasks mode dials the host channel.
- The executable never reads a file of another project and never imports
  `tooling/cli/internal`.
- The runtime's `prepare` inputs include `internal/**`, so an edit to the check
  rebuilds the runtime.

## Rejected alternatives

- **Run `putnami tasks find` as a subprocess.** D-014 settles an RPC call, and
  a subprocess starts a second putnami with its own provider resolution.
- **Import the core's packages.** The module may never import
  `tooling/cli/internal`, and the core's copy is about to be deleted.
- **A job-runner command instead of an interactive one.** The job renderer
  would capture the findings and the host opens no channel for a job.

## Consequences

- The check skill and its audit helper call `putnami change language`.
- Two copies of the check exist until the core deletion slice. A behavior
  change to one needs the same change in the other.
- `change verify` and `change finalize` follow the same pattern: a subcommand
  in the `change` group, an interactive launch, and the host channel for
  anything that needs the core.
