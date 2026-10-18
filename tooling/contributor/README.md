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
`putnami change language`, `change verify` and `change finalize`
([ADR 0004](doc/adr/0004-the-contributor-extension-ships-a-compiled-executable.md),
[ADR 0005](doc/adr/0005-the-contributor-executable-serves-the-change-noun.md),
[ADR 0006](doc/adr/0006-the-contributor-executable-serves-change-finalize.md)).
For a path declaration, the host builds the runtime from `cmd/` with the Go
release `putnami.lock.json` pins, the first time a command needs it.

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
| `build` | Coordinate an authorized outcome through one implementation, review, evidence and delivery loop | implicit |
| `spec` | Define outcome and acceptance in native feature/spec sources | implicit |
| `design` | Resolve a material technical choice against native contracts | implicit |
| `prototype` | Run one bounded inspectable experiment | implicit |
| `review` | Independently challenge a material artifact or frozen candidate | implicit |
| `adr` | Record a durable choice in its owning project | implicit |
| `check` | Run the gate, applicable local proof, the documentation audit and the language rule | implicit |
| `content-bump` | Refresh a pinned-content lock the policy declares | explicit only |

The former `plan`, `execute`, `fix`, `epic`, `code-review`, four `putnami-*`
aliases and `fix-loop` entries are removed. This intentional breaking change
is documented in the [migration guide](doc/03-entry-point-migration.md).
`build` handles named tasks and coordinated units only within its mandate;
it does not select backlog work. It publishes its own proposal by default
([ADR 0003](doc/adr/0003-build-publishes-its-proposal-by-default.md)), and
`--local` keeps a run local.

Worker profiles: `fix-light`, `fix-standard`, `fix-heavy` and `epic-analyst`,
each one body for both hosts with host metadata beside it.

The portable helpers are commands, not scripts. `putnami change finalize`,
which this extension's executable serves, publishes a draft or a verified
ready proposal, reads it back, and applies the named task's delivered
transition. A partial delivery uses `--partial-delivery` to keep a non-closing
parent reference and skip that transition. It consumes current producer proof
and never runs or retries a local gate; `--gate ci` explicitly selects hosted
checks ([Publish a proposal](doc/06-change-finalize.md)).
`putnami change language text|files|commits|tasks`, which this
extension's executable serves, enforces the configured language rule, the
core's `putnami doctor --machine` reports load for hosted-gate admission, and
`putnami tree fingerprint` supplies tree identity.
Host session limits remain the host runtime's responsibility.
The active audit integration retains `check/scripts/english-only.sh` as a
thin relay to the native language command; it carries no separate evaluator.

The build skill's dossier and workflow checker is `putnami change verify`,
which this extension's executable serves. Its standalone `--workflow` validates a local progress
projection. An optional workflow ref on the v2 dossier adds native
requirement/producer completeness to the existing ready check. Only the
dossier can compute `Putnami verified` for the declared local scope.

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
- [Workflow records and handoffs](doc/04-workflow-contract.md)
- [Publish a proposal](doc/06-change-finalize.md)
- [ADR 0001](doc/adr/0001-one-contributor-extension.md)
- [ADR 0002](doc/adr/0002-build-and-versioned-workflow-record.md)
- [ADR 0004](doc/adr/0004-the-contributor-extension-ships-a-compiled-executable.md)
- [ADR 0006](doc/adr/0006-the-contributor-executable-serves-change-finalize.md)

## Publication

No built-in starter declares this extension: `putnami init` installs nothing of
it. A workspace asks for it by declaring it in `extensions` and
`extension:@putnami/contributor` in `agentArtifacts`, then running
`putnami install`. The
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
completion eligibility before publication, except a partial delivery whose
changed paths all belong to projects that declare `featureAuthority.none` and
author no feature. Its ready receipt records exact
required checks and the immutable original workflow; later units and decisions
stay incomplete until supported. Keep prior workflow files when appending a
ready revision so native receipt replay can validate their original inputs.
