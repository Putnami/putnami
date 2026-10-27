# ADR 0007 — The contributor declares agent session hooks

- **Status**: accepted
- **Scope**: `@putnami/contributor` (`tooling/contributor`): its manifest, its
  runtime and the host files the core writes for it
- **Follows**: [ADR 0006](0006-the-contributor-executable-serves-change-finalize.md),
  for the executable and its host channel
- **Reverses**: the README rule that host files stay outside this extension,
  for hooks only

## Context

An agent session in Claude Code or Codex works on a mission: an issue or a
branch of a repository. The owner of the mission wants to see the session's
state and plan, send it messages, and answer the decisions it asks. The
`missions` collaboration contract carries that exchange
([ADR 0003 of the collaboration contracts](../../../../protocols/collaboration/doc/adr/0003-missions-contract.md)):
`missions.sync` publishes a session and reads its inbox, and the
`missions.ask` and `missions.reply` tools let the agent ask and reply.

Only the host sees the events of a session: its start, each tool call, each
turn end. So a host hook has to call `missions.sync`. The README of this
extension kept host files out of its authority: `.claude/settings.json` and
`.codex/config.toml` stay with the workspace. The core now writes the hooks an
extension declares, under rules the CLI owns
([CLI ADR 0081](../../../cli/doc/adr/0081-the-core-writes-declared-host-hooks.md)).

## Decision

### 1. The extension declares its hooks; the core writes them

The manifest declares a `hostHooks` member. It requires the `missions`
contract, runs the command path `agent hook`, and lists one entry per host
event:

- Claude Code: `SessionStart`, `PostToolUse` after `TodoWrite`, `TaskCreate`,
  `TaskUpdate`, `missions.ask` and `missions.reply`, `Stop`, a second `Stop`
  with `--idle-wake` that may wake the session, and `StopFailure`.
- Codex: `SessionStart`, `PostToolUse` after `update_plan`, `missions.ask`
  and `missions.reply`, and `Stop`.

The extension never edits a host file. The core writes the entries when the
workspace opts into this extension's content and binds `missions`, and removes
them when either goes away. The manifest's `cliContract` is 17.

### 2. Two commands in the `agent` group

`agent hook --engine <claude-code|codex> [--idle-wake]` handles one event read
on stdin. `agent attach --issue <n>` makes `issue-<n>` the mission of the
checkout's current branch. Both are interactive subcommands that the
executable serves, in `internal/agentsession`. Without an attached issue, a
session reports to `branch:<branch>`. The repository is the lowercase
`owner/name` of the `origin` remote; the hook keeps no host and no credential.

### 3. What each event does

- `SessionStart` syncs, reads the mission's checkpoint through
  `memory.mission` when memory is bound, and prints the mission sheet: the
  repository and key that `missions.ask` takes, the title, the plan, the open
  decisions and the checkpoint, at most 4 KiB. Every text an agent or the
  owner wrote is quoted with `> `. The waiting messages follow, whole.
- `PostToolUse` records a plan change, the id of an asked decision, or a
  marker for a reply `missions.reply` recorded. It runs no git and calls no
  provider. When another hook holds the session lock, it spools the call.
- `Stop` sends the state, the plan, the delivered message ids, the bound
  decisions and the `[ack <id>] <reply>` lines of the last message in one
  sync. A waiting message continues the turn once. When the sync itself
  answers a new message, a second sync publishes that the session works
  again. A Codex `Stop` waits up to 180 seconds for the answer to an open
  decision that this session asked. A decision another session asked never
  holds it.
- `StopFailure` publishes the error.
- The idle wake waits up to 10 minutes after a turn whose `Stop` did not
  continue it. It exits 75 with the message on stderr when one arrives, and
  the core turns that code into the host's wake.

### 4. A message counts as delivered once the hook wrote it

The host reads a hook's output when the hook exits, and drops it when the
hook runs past its timeout. So a hook takes a message from its inbox only
after it wrote the text that holds it, and the next sync reports the message
as delivered. A hook stopped before the write leaves the message for the next
hook: a message reaches the agent at least once. Until that sync, another
session of the mission may receive it too. The case left is a host that stops
the hook after the write, while the launcher ends; each budget leaves the
launcher 5 seconds for that, and a hook never runs the first-use install
(CLI ADR 0081, decision 10).

### 5. A hook never fails a turn

Every other path exits 0. A failure, a missing host channel and an outcome
other than `ok` leave one line in `hook.log`. The session store lives under
`<Putnami home>/contributor/sessions`, never in the checkout. What a sync
did not deliver stays queued for the next one; a refused reply is final.

### 6. Each event has a budget

A call goes through the host channel to the bound provider, which may start
cold, so a call may take up to 10 seconds. Each event stops 5 seconds before
the timeout its entry declares: 30 seconds for an ordinary hook, 240 for the
Codex `Stop` and 660 for the idle wake. The common longest path of each event
fits its budget, and a test holds the manifest to these numbers.

## Consequences

- The hooks run only in a workspace that binds `missions`. Without it, the
  core writes none.
- A session reports nothing when the checkout has no `origin` that names an
  `owner/name` repository.
- The framework names no provider. Any provider that serves `missions` v1
  receives the sessions.
- Host permissions, credentials and model access still stay with the
  workspace.
- From this release on, the contributor needs a CLI at contract 17 or later:
  an older CLI refuses the whole extension, `change finalize` included,
  whether the workspace binds `missions` or not. A workspace moves its CLI
  and contributor pins together.
