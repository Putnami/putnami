# `@putnami/contributor`

The contributor workflows for agent hosts, shipped as one content-only
extension: plan, implement, verify, review independently, repair, and publish
under an authorized mandate. It replaces the separately declared
`@putnami/agent-workflows` and `@putnami/maintainer-workflows` artifacts, which
its manifest names in `agentContent.supersedes`, with one implementation that
each repository configures.

The extension runs nothing. Its manifest declares only an `agentContent`
contribution, which the CLI builds and materializes for Claude Code
(`.claude/skills`, `.claude/agents`) and Codex (`.agents/skills`,
`.codex/agents`) under the extension's own version.

## Use it

Declare the extension and opt into its content in `putnami.workspace.json`.
Declaring the extension alone activates nothing:

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
| `plan` | Explore a change and return a plan; create a task only when asked | implicit |
| `execute` | Carry an authorized change through scope collaboration, implementation, independent review, repair and current evidence | implicit |
| `fix` | Select and claim a task, then enter execute | implicit |
| `epic` | Drive an epic on one integration branch and draft-to-ready proposal | explicit only |
| `check` | Run the gate, applicable local proof, the documentation audit and the language rule | implicit |
| `code-review` | Review a fixed revision independently; publish only when asked | implicit |
| `fix-loop` | Repeat `fix` over the backlog | explicit only |
| `content-bump` | Refresh a pinned-content lock the policy declares | explicit only |
| `putnami-plan`, `putnami-change`, `putnami-check`, `putnami-review` | Deprecated aliases of `plan`, `execute`, `check`, `code-review` | as their canonical skill |

Worker profiles: `fix-light`, `fix-standard`, `fix-heavy` and `epic-analyst`,
each one body for both hosts with host metadata beside it.

Helpers, with the shell suites the `@putnami/cli` tests run against the
materialized copies:

- `fix/scripts/finalize-pr.sh` commits and pushes with Git, publishes the
  proposal through `putnami proposals upsert`, reads it back, and moves the
  task through `putnami tasks transition`. `--draft` opens the draft proposal
  when work starts; without it, the helper gates, posts the verification record
  as a proposal comment, and marks the proposal ready. On a loaded machine it
  leaves the gate to the hosted checks the policy names.
- `fix/scripts/tree-fingerprint.sh` forwards to `putnami tree fingerprint`.
- `fix/scripts/machine-load.sh` prints the one-minute load average divided by
  the CPU count.
- `fix/scripts/session-cap.sh` is an optional Claude Code `PreToolUse` hook that
  warns once at 250 of the orchestrator's own tool calls (subagent calls are
  not counted) and never blocks; wiring it is the workspace's choice.
- `check/scripts/english-only.sh` detects non-English text in a text, tracked
  files, commit messages, or the open tasks of the tasks provider.

The execute skill's dossier checker is not a helper: it is the CLI command
`putnami tree verify`, which checks a local dossier against the producer
records it references and computes `Putnami verified` for the declared local
scope only.

## What it deliberately excludes

- Backend calls, tracker labels, hosted addresses and memory storage layouts:
  providers own them, and the content policy in `putnami.json` fails the build
  on them.
- Host permissions, credentials and model access: `.claude/settings.json`,
  `.codex/config.toml` and the host's own configuration stay with the
  workspace.
- The `audit` skill and its workers, which stay hand-authored until their owner
  ships them as a contribution of their own.

## Documentation

- [Contributor journey](doc/01-contributor-journey.md)
- [Repository policy](doc/02-repository-policy.md), with this repository's
  configuration as a reference
- [Entry-point migration and evidence limits](doc/03-entry-point-migration.md)
- [ADR 0001](doc/adr/0001-one-contributor-extension.md)

## Publication

Every built-in starter declares this extension and opts into its content. The
extension is published with the framework release set (`preview`), packaged by
`@putnami/scaffold` under the `agent-content` step. It reaches `canary` first,
and `stable` and `latest` at the next promotion. Until the channel a workspace
resolves serves it, `putnami init` keeps the declarations and names
`putnami install`, and `putnami install` fails in that workspace; the
[compatibility window](doc/03-entry-point-migration.md#publication-and-compatibility-window)
states what works before publication and which CLI reads the content. The two
superseded artifacts are deleted, and no CLI from this release on installs them
([CLI ADR 0047](../cli/doc/adr/0047-extension-owned-agent-content.md)).
