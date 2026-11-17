# ADR 0098 — Host hooks reach every session

- **Status**: accepted
- **Date**: 2026-11-17
- **Amends**: [ADR 0081](0081-the-core-writes-declared-host-hooks.md),
  decisions 2, 4, 6 and 10
- **Scope**: `@putnami/cli` (`internal/commands/agentctx`,
  `internal/commands/lifecycle`, `internal/commands/doctor`,
  `internal/launch`, the new `host-hook` command), the doctor contract
  (`protocols/doctor`, one more check code), features
  `cli/agent-workflow-lifecycle` and `cli/diagnostics-results`

## Context

ADR 0081 writes an extension's host hooks into the files of the workspace,
`.claude/settings.json` and `.codex/hooks.json`, and nowhere else. Two hosts
do not read those files from the checkout a session works in:

- In a worktree session that the Claude desktop app starts, Claude Code reads
  the project settings from the main checkout, runs the hook commands in its
  root, and points `CLAUDE_PROJECT_DIR` at it. Only the `cwd` member of the
  event names the worktree. A main checkout that lags behind the default
  branch runs the hooks of its own old commit, or none. When the hooks do run,
  that old checkout picks the CLI pin, the extension version and the
  collaboration bindings that serve the event.
- A cloud session that clones several repositories starts in their parent
  directory. That directory has no `.claude/settings.json`, so no project hook
  runs at all.

No command moves a main checkout, and a person cannot tell that their
sessions stopped reporting: a hook that never runs leaves no log. Every person
who works in a workspace that opts into contribution must report from every
session, whatever checkout the host reads its settings from.

## Decision

### 1. A relay serves a hook from the workspace of its event

`putnami host-hook <command path> [args]` is a core command. It reads the
hook event on standard input, at most 64 MiB, and its `cwd` member. When that
member is an absolute directory inside a workspace, the relay runs the
command path in that directory, through the workspace's launcher:
`<root>/putnamiw` in a source workspace, and the running binary otherwise,
which then follows that workspace's CLI pin. Every other event runs the
command path in the relay's own directory, as a hook did before.

- The relay replays the event on the child's standard input byte for byte,
  passes its standard output and standard error through, and exits with its
  exit code, so a wake hook's 75 still reaches the guard.
- The relay never follows a pin or the source-workspace rule of the directory
  the host started it in (`launch.IsExemptInvocation`): that directory is the
  stale checkout this ADR works around. The child does not inherit the
  launcher's loop guard or opt-out, so it follows its own workspace's pin.
- The relay serves only a repository the person ran Putnami in: the
  workspace's repository, named by its Git common directory so every worktree
  of a clone shares it, must be in `~/.putnami/host-hooks/repositories.json`.
  Every command that runs in a workspace, outside CI and hosted runs, records
  its repository there: it has already run the repository's code. A
  worktree's `.git` file counts only when the clone's worktree directory
  points back at it.
  A hook in the user's file runs in every session of the person, whatever
  repository it opens, and the relay would otherwise start that repository's
  code, its `putnamiw` or the CLI its lock pins, without the host's folder
  trust. Any other workspace runs nothing: the relay says why on standard
  error and exits 0.
- The relay prints nothing of its own on standard output, which the host
  reads as the hook's answer. A relay that cannot start its child says why
  on standard error and exits 1, which the hook guard turns into 0.

### 2. Every declared hook runs through the relay

A rendered extension hook is
`PUTNAMI_NO_AUTO_INSTALL=1 <launcher> host-hook <command path> <args>`,
followed by the guard of ADR 0081 decision 8. The launcher of a workspace
file stays the one decision 10 of ADR 0081 chooses. A handler is still
Putnami's by its command: a known launcher followed by a declared command
path, directly or after `host-hook`, so a run replaces a hook of the earlier
form in place.

The client restore hook of [ADR 0087](0087-a-fresh-worktree-restores-its-generated-clients.md)
is not an extension hook and keeps its form: it restores the clients of the
checkout the host starts in.

### 3. The explicit lifecycle commands also write the user's hook files

When an explicit `putnami install`, `init` or `upgrade` writes an extension's
hooks into the workspace files, it writes the same hooks into the user's own
hook file of each host:

| Host | File |
|---|---|
| Claude Code | `settings.json` in `$CLAUDE_CONFIG_DIR`, or in `~/.claude` |
| Codex | `hooks.json` in `$CODEX_HOME`, or in `~/.codex` |

- A user file always names the launcher `putnami`: it serves every workspace
  of the person, and the relay finds the workspace from the event.
- The merge rules of ADR 0081 decisions 4, 5 and 7 hold unchanged: the
  person's entries keep their bytes and place, a hook is replaced in place,
  and a file that cannot be edited safely keeps its bytes with a warning.
  `upgrade --dry-run` lists the user files it would change.
- A run never removes a hook from a user file. That file serves the person's
  other workspaces, and a hook that runs in a workspace without the opt-in or
  the binding does nothing.
- A run inside CI (`CI` set to a true value) writes no user file and records
  nothing: a runner hosts no agent session.

Claude Code runs a handler that both a user file and a project file define
only once. A workspace whose launcher is `putnami` renders the same handler in
both, so it runs once. A source workspace's handler differs, and Codex runs
every source, so `agent hook` handles an event once
([contributor ADR 0016](../../../contributor/doc/adr/0016-an-event-two-hook-sources-send-is-handled-once.md)).

### 4. `putnami doctor` reports a workstation whose host would run no hook

`doctor.agent_hooks_unreachable` is a workstation check. For a workspace
whose explicit install would write extension hooks, it reports each host whose
user hook file holds no Putnami handler for one of the events the workspace
asks for, a repository no Putnami command recorded, and `putnami` missing
from the `PATH` of the doctor process, the nearest it can see of the `PATH`
the hooks run with. Its remedy names `putnami install`, the setup
script of a cloud environment, and Codex's `/hooks` trust step. Like every
workstation finding, it is advisory, waivable, and outside the production
gate.

## Amends

- **ADR 0081, decision 2.** The explicit commands also write into the user's
  hook files, and only there outside the workspace.
- **ADR 0081, decisions 4 and 10.** A hook runs its command path through
  `host-hook`, and a handler that does so is Putnami's.
- **ADR 0081, decision 6.** Removal applies to the workspace files only.

## Consequences

- A desktop session in a worktree reports to its mission when the main
  checkout is on a commit with no host hooks, once the person ran
  `putnami install` with this release in any workspace, and any Putnami
  command in a checkout of that repository.
- A multi-repository cloud session starts in the parent directory of its
  clones, which is no workspace: the relay cannot serve its first events, and
  its session start brings no mission sheet. It reports from its first event
  whose `cwd` is inside a workspace, once its environment's setup script ran
  `putnami install` before the session started: a host reads its hooks when a
  session starts.
- A clone where no Putnami command of this release ran yet is served by no
  hook, its own project hooks included, since they run through the relay too.
  Its first command records it; `putnami doctor`, itself such a command,
  records it and then reports nothing for it.
- A user file whose `putnami` is older than this release relaunches into the
  pin of the directory the host started in. That pin knows `host-hook` once
  the workspace upgraded, so only a stale main checkout with an old global
  `putnami` stays silent. `doctor` cannot see that case; installing a current
  global `putnami` ends it.
- Codex runs a new user hook only after `/hooks` trusts it; a run that
  creates or changes `hooks.json` there says so, as for the workspace file.

## Rejected alternatives

- **Report the stale main checkout and do nothing else** (the issue's option
  2): it makes the failure visible and leaves it.
- **User hooks without the relay**: the host starts them in the main
  checkout, whose old pin, extension or binding then serves the event, or
  nothing does.
- **Serve every workspace the event names**: a user hook would then start the
  code of any repository a session opens, without the person's consent.
- **A user-scope install as the opt-in** (`putnami extensions install --user
  @putnami/contributor`): every contributor of an opted-in workspace must
  report without a step of their own.
