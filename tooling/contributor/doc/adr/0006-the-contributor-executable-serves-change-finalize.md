# ADR 0006 — The contributor executable serves change finalize

- **Status**: accepted
- **Scope**: `@putnami/contributor` (`tooling/contributor`): its manifest, its
  runtime and its callers
- **Follows**: [ADR 0005](0005-the-contributor-executable-serves-the-change-noun.md),
  whose last consequence fixes the shape this command takes

## Context

Decision D-016 in the root `decisions.json` names `change finalize` as the
replacement of the core's `proposals finalize`. D-011 settles that
`@putnami/contributor` serves it, and D-014 settles that an extension command
reaches the collaboration providers through an RPC call to the core. ADR 0005
moved `change language` and `change verify` into the executable.

`proposals finalize` publishes a proposal. It reads Git state, calls the
`tasks` and `proposals` providers, verifies a dossier or waits for hosted
checks, pushes the branch, and writes the proposal and the task. For a hosted
partial delivery without a workflow record, it also asks which projects own
the changed paths, so it can admit only projects that hold no requirement.

This record moves that command into the executable.

## Decision

### 1. A `finalize` subcommand in the `change` group

The manifest declares `commandGroups.change.subcommands.finalize`, which routes
to the flat command `change-finalize`. The subcommand is interactive and needs
a workspace. Its task runs `{extensionRuntime} finalize` in `{workspaceRoot}`
and is never cached. The manifest declares the core command's flags: the
string flags `--base`, `--branch`, `--title`, `--body-file`, `--task-source`,
`--task-id`, `--verification-file`, `--workflow-file`, `--gate` and `--proof`,
and the boolean flags `--draft` and `--partial-delivery`. Its description says
the command runs no local gate. The manifest keeps `cliContract` 5.

### 2. A copy of the finalizer, with the same behavior

`internal/finalize` holds a copy of the finalizer the core served: the option
parser, the publication flow, the policy and text rules, and the Git
operations with their read-only allowlist and their two writes, `commit` and
`push`. It prints the same result and exits with the same code: 2 for a usage
error, 1 for a failure and 0 for a published proposal. A failure prints
`change finalize: <error>` on stderr.

The output mode comes from `params.output` in the job context putnami hands
the executable. `json`, `jsonl` and `cloud-logging` print the result document
on one line. Any other mode prints `PROPOSAL_REF=<json>`, then
`PROPOSAL_URL=<url>` when the proposal has a URL.

The executable reads the project selection from the same context and refuses
one, as the core does. `SIGINT` and `SIGTERM` cancel the run.

### 3. One host channel serves the run

`internal/host` holds one `Channel` for the process. The executable takes the
channel variables once, when it starts, and removes them from its environment.
The channel dials on the first call, and every later call reuses that
connection. The executable closes it when it exits. A run that makes no call,
such as one that ends in a usage error, never dials.

`change language` and `change verify` use the same type. Their behavior does
not change: each still dials only for the call that needs the host.

The finalizer reaches the core through four seams, each an adapter over the
channel or over a copy that already exists:

- **Providers.** `collaboration.call` returns the envelope `putnami <contract>
  <operation> --output=json` prints. A call that never reaches the host is an
  error that names the interactive launch.
- **Ownership.** `workspace.owners` names the projects that own each changed
  path (section 4).
- **Dossier and hosted workflow.** The copy of the verifier in
  `internal/verify` checks the dossier and the hosted workflow record.
  `verification.report` reads a report a gate restored from the task cache, as
  in `change verify`.
- **Language.** The copy of the evaluator in `internal/language` applies the
  English-only rule to the proposal text.

### 4. The host names the owners of a changed path

A hosted partial delivery without `--workflow-file` is admitted only when every
changed path belongs to projects that declare `featureAuthority.none` and author
no feature. The core resolves the owners from the workspace it loaded. The
executable asks the host with `workspace.owners`, which answers with the core's
own resolution: the nearest project, every project that claims the path as an
asset, and, for a workspace-root file such as `go.work`, every project whose
provider watches it.

The executable keeps the rest of the check:

- It strips the workspace prefix that `git rev-parse --show-prefix` prints. A
  changed path outside the prefix has no owner, and the host is not asked
  about it.
- A changed path inside the workspace that is not valid UTF-8 fails the check,
  and the error names the path. The channel carries only valid workspace paths.
- For each owning project, it reads the branch state with the function the
  workspace loader calls, `LoadProjectConfigWithDiagnostics`. A project
  without a config, such as one whose `putnami.json` has a type error in any
  field, holds requirements. It also checks that no feature manifest exists on
  disk.
- It reads the base state with `git ls-tree` and `git show <rev>:./<path>`,
  decoding only `featureAuthority`, as the core does. A project new on the
  branch has only its branch state.

### 5. No process the executable starts receives the channel

`internal/childenv` builds the environment of every child process. It removes
the channel variables, and for Git it also removes the variables that could
redirect a command away from its working directory. `change language`,
`change verify` and `change finalize` share it. The core marks repository code
as started before a commit or a push, which guards its run credential. The
executable holds no run credential, so it marks nothing.

### 6. The core deletes `proposals finalize`

The core no longer serves `proposals finalize`. `putnami proposals finalize …`
and `putnami help proposals finalize` are usage errors that link the reference
of moved commands, whether or not the workspace binds a collaboration
contract. The core deletes its copies of the finalizer, the verifier and the
evaluator. `collaboration.call` answers `proposals finalize` like any
operation the contract does not define: an envelope whose outcome is
`invalid`.

### Known differences from `putnami proposals finalize`, as the core served it

- **(a) Help.** `putnami change finalize --help` prints the help putnami builds
  from the manifest. The core prints its own finalize help.
- **(b) Undeclared flags.** Both exit 2 and name the argument. For `change
  finalize`, putnami first warns on stderr that undeclared flags are
  deprecated.
- **(c) Message prefix.** A failure reads `change finalize: <error>`, where the
  core prints `putnami: proposals finalize: <error>`. A refused selection reads
  `change finalize takes no <flag> selection`, where the core prints
  `putnami: proposals finalize takes no <flag> selection`.
- **(d) No channel.** A build job, a provider tool, a hosted run
  (`--credential-fd`) and a process putnami did not launch have no channel.
  Such a run fails at its first provider call with exit 1, before it writes
  anything, and the message names `putnami change finalize`. The core needs no
  channel.
- **(e) Deadline.** The task has a 90-minute deadline. The core command has
  none. A policy whose `verification.ciGate.timeoutMinutes` is above about 85
  can reach the deadline before the hosted wait ends. When the deadline
  passes:
  - putnami sends `SIGTERM` to the executable, and the run's context cancels.
  - The hosted wait returns `context canceled`. The command prints `change
    finalize: context canceled` and exits 1.
  - The proposal stays open and ready, the state a policy timeout leaves.
  - putnami kills the executable if it still runs 5 seconds later.
  - A `git push` in flight is its own process, which putnami does not signal,
    so it completes.

  To resume, run the command again.
- **(f) Run credential.** The executable marks no repository code as started.
- **(g) Undeclared collaboration.** Without `options.collaboration`, putnami
  does not route `proposals` to the core command. `change finalize` runs, and
  its first provider call fails with the host's refusal: `this workspace
  declares no options.collaboration in putnami.workspace.json, so no
  collaboration contract is bound`. With `options.collaboration` and no
  `proposals` binding, both print `proposals binding is unbound`.
- **(h) Selection.** putnami resolves a selection before it launches the
  executable. A selection that matches no project fails with putnami's message.
  `--tag`, `--exclude-tag` and `--exclude` share one refusal that names all
  three. The core names the first flag it finds.
- **(i) Job context.** A job context file that cannot be read fails the run
  with exit 1. putnami always writes one, so only a direct run reaches this.
- **(j) Child environment.** Git runs without the channel variables. It also
  inherits the environment putnami gives an interactive launch, such as
  `PUTNAMI_INTERACTIVE`.
- **(k) Workspace load.** putnami loads the workspace before it launches the
  executable, so a workspace that does not load fails every run with
  `putnami: load workspace: <error>` and exit 1. The core needs a loadable
  workspace only where it reads one, such as the ownership check.
- **(l) Non-UTF-8 paths.** A changed path inside the workspace that is not
  valid UTF-8 fails a hosted partial delivery without a workflow record. The
  core resolves its owner from the bytes.
- **(m) Unreachable host.** A provider call that does not reach the host is an
  error that names its cause. The core calls the provider in-process and
  always has an envelope.
- **(n) Restored reports.** A dossier or a hosted workflow record can name a
  report that a gate restored from the task cache. The executable reads it
  with `verification.report`, from the cache of the workspace putnami loaded.
  The core reads the cache under the Git top level, as ADR 0005 records in its
  difference (d). Where the two caches differ, the verdicts differ. A failed
  `verification.report` call gives the core's reason, `workflow proof restored
  report has no matching native cache producer: <task>`, without the host's
  message. The no-channel reason of ADR 0005, difference (e), never appears:
  without a channel the run stops at its first provider call (d), before it
  reads the dossier.
- **(o) Interrupt.** A first `Ctrl-C` cancels both commands. The core prints
  `putnami: proposals finalize: <error>`, such as `context canceled`, and
  exits 1. For `change finalize`, the executable prints `change finalize:
  <error>`, and putnami exits 130, the code it gives every interactive command
  it cancels. A second `Ctrl-C`, or 10 seconds without an exit, makes both
  exit 130.

## Invariants

- For a draft seed, a hosted partial delivery, a refused partial delivery, a
  dossier that does not verify, a usage error and an undeclared flag, `change
  finalize` exits with the same code, prints the same stdout and the same
  message, leaves the same branch head, pushes the same refs and writes the
  same proposal and task records as goldens recorded from `proposals
  finalize`, the core command it replaced. A core test runs `change finalize`
  through putnami in twin workspaces and compares it with the goldens.
- The owners of a changed path come from the host's `workspace.owners`. The
  executable loads no workspace.
- One run opens at most one connection, and a run that makes no call opens
  none.
- No process the executable starts receives the channel variables.
- The executable never imports `tooling/cli/internal`.
- The runtime's `prepare` inputs include `internal/**`, so an edit to the
  finalizer rebuilds the runtime.

## Rejected alternatives

- **Copy the ownership rules.** The executable could load the workspace or
  copy the nearest-project and asset rules. The workspace owner rejected that:
  the rules would drift, and a provider's watched root files would be missed.
  `workspace.owners` answers with the core's resolution.
- **Run `putnami proposals finalize` as a subprocess.** D-014 settles an RPC
  call, and a subprocess starts a second putnami with its own provider
  resolution and its own workspace load.
- **Dial once for each call.** Each dial is a connection, a token check and a
  second failure mode in the middle of a publication. One client for the run
  keeps one answer to whether the host is reachable.

## Consequences

- The build skill, its references and the `fix-heavy` worker call `putnami
  change finalize`. A consumer needs @putnami/contributor with the command.
- The core deletes `proposals finalize` and its copies of the finalizer, the
  verifier and the evaluator (section 6). The executable's copies are the only
  ones.
- The requirement `change-finalize-parity` of `cli/contributor-workflows` lists
  the check the twin test proves. The requirement of the same name in
  `tooling/contributor-executable` lists the checks the executable's tests prove.
