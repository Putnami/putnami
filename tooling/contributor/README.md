# `@putnami/contributor`

The contributor workflows for agent hosts, shipped as one extension:
specify, design, build, verify, review independently, repair, and publish
under an authorized mandate. It replaces the separately declared
`@putnami/agent-workflows` and `@putnami/maintainer-workflows` artifacts, which
its manifest names in `agentContent.supersedes`, with one implementation that
each repository configures.

The manifest declares an `agentContent` contribution, which the CLI builds
and materializes for Claude Code (`.claude/skills`, `.claude/agents`) and Codex
(`.agents/skills`, `.codex/agents`) under the extension's own version. It also
declares a compiled runtime, `compiled/putnami-contributor`, which serves
`putnami change language`, `change verify`, `change finalize`,
`change estimate`, `agent hook`, `agent attach`, `agent stage`, `agent ask`
and `agent reply`
([ADR 0004](doc/adr/0004-the-contributor-extension-ships-a-compiled-executable.md),
[ADR 0005](doc/adr/0005-the-contributor-executable-serves-the-change-noun.md),
[ADR 0006](doc/adr/0006-the-contributor-executable-serves-change-finalize.md)).
For a path declaration, the host builds the runtime from `cmd/` with the Go
release `putnami.lock.json` pins, the first time a command needs it.

## Use it

`putnami extensions install @putnami/contributor` declares the extension and
opts into its content in `putnami.workspace.json`. Declaring the extension
alone activates nothing:

```json
{
  "extensions": ["@putnami/contributor"],
  "agentArtifacts": ["extension:@putnami/contributor"]
}
```

This repository declares it by path (`/tooling/contributor`) and runs exactly
the content it distributes; `./putnamiw context generate` rebuilds the host
copies from `src/`. Bind the providers the workflows reach
([collaboration providers](../cli/doc/25-collaboration-providers.md)), and
state the repository's conventions in its [policy](doc/02-repository-policy.md).

## What it contains

| Skill | What it does | Invocation |
|---|---|---|
| `build` | Deliver an authorized outcome through the contribution loop, from talk to merge | implicit |
| `spec` | Define outcome and acceptance in native feature/spec sources | implicit |
| `design` | Resolve a material technical choice, with a bounded prototype or an ADR when it needs one | implicit |
| `review` | Independently challenge a material artifact or frozen candidate | implicit |
| `check` | Run the gate, applicable local proof, the documentation audit and the language rule | implicit |
| `content-bump` | Refresh a pinned-content lock the policy declares | explicit only |

The former `plan`, `execute`, `fix`, `epic`, `code-review`, `adr`,
`prototype`, four `putnami-*` aliases and `fix-loop` entries are removed. This intentional breaking change
is documented in the [migration guide](doc/03-entry-point-migration.md).
`build` handles named tasks and coordinated units only within its mandate;
it does not select backlog work. It publishes its own proposal by default
([ADR 0003](doc/adr/0003-build-publishes-its-proposal-by-default.md)), and
`--local` keeps a run local. When the outcome changes a declaration, the first
checkpoint holds the declarations only
([ADR 0008](doc/adr/0008-build-pushes-its-intent-before-its-code.md)).

Workers: `fix-standard`, `fix-heavy` and `reviewer`, each one body for both
hosts with host metadata beside it. A workflow profile in the repository
policy says which stage each one runs; the default profile runs only review in
`reviewer`.

The portable helpers are commands, not scripts. `putnami change finalize`,
which this extension's executable serves, publishes a draft or a verified
ready proposal, reads it back, and applies the named task's delivered
transition. A partial delivery uses `--partial-delivery` to keep a non-closing
parent reference and skip that transition. It consumes current producer proof
and never runs or retries a local gate; `--gate ci` explicitly selects hosted
checks ([Publish a proposal](doc/06-change-finalize.md)).
`putnami change language text|files|commits|tasks`, which this
extension's executable serves, enforces the configured language rule. A file
given to `text` resolves against the directory you typed the command in;
the pathspecs of `files` are relative to the workspace root. The
core's `putnami doctor --machine` reports load for hosted-gate admission, and
`putnami tree fingerprint` supplies tree identity.
Host session limits remain the host runtime's responsibility.
The active audit integration retains `check/scripts/english-only.sh` as a
thin relay to the native language command; it carries no separate evaluator.

`putnami change estimate` reads a change plan, `.putnami/<issue>/plan.json`
by default, checks it against the plan format, and sizes it S, M or L with a
token and a duration range ([Estimate a change plan](doc/07-change-estimate.md)).

The build skill's dossier and workflow checker is `putnami change verify`,
which this extension's executable serves. Its standalone `--workflow` validates a local progress
projection. An optional workflow ref on the v2 dossier adds native
requirement/producer completeness to the existing ready check. Only the
dossier can compute `Putnami verified` for the declared local scope.

## Agent sessions

The manifest declares host hooks for Claude Code and Codex
([ADR 0007](doc/adr/0007-the-contributor-declares-agent-session-hooks.md)).
The core writes them into `.claude/settings.json` and `.codex/hooks.json` when
the workspace opts into this extension's content and binds the `missions`
contract. Each hook runs `putnami agent hook`, which the executable serves:

- At session start, the agent receives its mission sheet and the owner's
  waiting messages.
- A plan change is kept locally. So are an asked decision and a recorded
  reply, which `putnami agent ask` and `putnami agent reply` record.
- At the end of a turn, one sync publishes the state, the plan, the delivered
  messages, the decisions and the `[ack <id>] <reply>` lines of the last
  message. A waiting message continues the turn once.
- A message counts as delivered once the hook wrote it. A hook the host
  stops before that leaves the message for the next hook.
- Claude Code waits up to 10 minutes after a turn for the owner's next
  message; Codex waits up to 180 seconds for the answer to a decision the
  session asked.
- When a Claude Code session ends, one sync publishes the `ended` state.
- Every sync carries the session's token usage, counted from the host's
  transcript: for Claude Code, the session and its subagents; for Codex, the
  session's own thread.

A session reports to `branch:<branch>` until `putnami agent attach --issue <n>`
makes `issue-<n>` the mission of the checkout's branch.
`putnami agent stage <talk|declare|show|code|check|prove|validate|merge>`
records the loop stage of the current session and syncs it when the session
reports to a mission; every later sync carries it. Once it read the
transcript, it also prints the tokens the session spent so far.
`putnami agent ask --input <file|->` asks the owner a decision through
`missions.ask` for the session's mission, and the next sync binds the decision
to the session; the input is a `missions.ask` document without its `mission`.
`putnami agent reply --message <id> --reply <text>` records a reply through
`missions.reply`, and the session's next Stop then sends no `[ack <id>]` line
for that message. A hook never fails a turn: a failure leaves one line in
`hook.log` under `<Putnami home>/contributor/sessions`.

Codex needs three things from the workspace before these hooks report a
session:

- Trust. Codex runs project hooks only in a trusted project, after you
  review them with `/hooks`.
- The plan tool. Since Codex 0.152, `update_plan` is off by default, and
  without it no Codex plan reaches the mission. Turn it on in
  `.codex/config.toml`:

  ```toml
  [tools.update_plan]
  enabled = true
  ```

- A committed `.codex/hooks.json`. In a linked git worktree, Codex 0.165
  reads project hooks from the main checkout's `.codex/hooks.json`, not the
  worktree's. Commit the file, so the main checkout has it too.

## What it deliberately excludes

- Backend calls, tracker labels, hosted addresses and memory storage layouts:
  providers own them, and the content policy in `putnami.json` fails the build
  on them.
- Host permissions, credentials and model access: `.claude/settings.json`,
  `.codex/config.toml` and the host's own configuration stay with the
  workspace. The only exception is the hooks this extension declares, which
  the core writes and removes.
- The `audit` skill and its workers, which stay hand-authored until their owner
  ships them as a contribution of their own.

## Documentation

- [Contributor journey](doc/01-contributor-journey.md)
- [Repository policy](doc/02-repository-policy.md), with this repository's
  configuration as a reference
- [Entry-point migration and evidence limits](doc/03-entry-point-migration.md)
- [Workflow records and handoffs](doc/04-workflow-contract.md)
- [Publish a proposal](doc/06-change-finalize.md)
- [Estimate a change plan](doc/07-change-estimate.md)
- [ADR 0001](doc/adr/0001-one-contributor-extension.md)
- [ADR 0002](doc/adr/0002-build-and-versioned-workflow-record.md)
- [ADR 0004](doc/adr/0004-the-contributor-extension-ships-a-compiled-executable.md)
- [ADR 0006](doc/adr/0006-the-contributor-executable-serves-change-finalize.md)
- [ADR 0007](doc/adr/0007-the-contributor-declares-agent-session-hooks.md)
- [ADR 0008](doc/adr/0008-build-pushes-its-intent-before-its-code.md)
- [ADR 0009](doc/adr/0009-one-contribution-loop-from-talk-to-merge.md)

## Publication

From this release on, the contributor needs a CLI at contract 17 or later,
for its host hooks. An older CLI refuses the whole extension. A workspace
moves its CLI and contributor pins together.

No built-in starter declares this extension: `putnami init` installs nothing of
it. A workspace asks for it with
`putnami extensions install @putnami/contributor`, which declares it in
`extensions`, opts into it with `extension:@putnami/contributor` in
`agentArtifacts` and installs its content. The
extension is published with the framework release set (`preview`), packaged by
`@putnami/go` into one archive per platform that carries the content and the
executable. It reaches `canary` first,
and `stable` and `latest` at the next promotion. Until the channel a workspace
resolves serves it, `putnami extensions install @putnami/contributor` and
`putnami install` fail, declared or not; the
[compatibility window](doc/03-entry-point-migration.md#publication-and-compatibility-window)
states what works before publication and which CLI reads the content. The two
superseded artifacts are deleted, and no CLI from this release on installs them
([CLI ADR 0047](../cli/doc/adr/0047-extension-owned-agent-content.md)).

Hosted finalization naming a parent requires `--workflow-file` and native
completion eligibility before publication, except a partial delivery. A partial
delivery whose changed paths all belong to projects that declare
`featureAuthority.none` and author no feature needs no workflow. A partial
delivery whose workflow has no unit with passing native checks still publishes,
with no ready unit and no receipt: the hosted checks never stand for native
proof. A ready receipt records exact
required checks and the immutable original workflow; later units and decisions
stay incomplete until supported. Keep prior workflow files when appending a
ready revision so native receipt replay can validate their original inputs.
