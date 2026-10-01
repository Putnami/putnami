# ADR 0001 — One contributor extension, configured by repository policy, publishing through the collaboration contracts

- **Status**: accepted
- **Scope**: `@putnami/contributor` (`tooling/contributor`), this repository's
  `putnami.workspace.json`, the generated `.agents/`, `.claude/` and `.codex/`
  trees

## Context

The contributor workflows (plan, execute, check, review, fix, epic) serve this
repository and every other workspace. A public copy and a maintainer copy would
duplicate them and make "public" mean "less capable" instead of "configured
differently". Extensions can ship agent content
([`protocols/extension` ADR 0006](../../../../protocols/extension/doc/adr/0006-agent-content-is-an-additive-contract.md)),
and the collaboration contracts give every backend operation a
provider-neutral call
([collaboration ADR 0001](../../../../protocols/collaboration/doc/adr/0001-collaboration-provider-contracts.md)).

## Decision

### 1. One content-only extension

`tooling/contributor` is the extension `@putnami/contributor`. It runs nothing:
its manifest declares only `agentContent` in the authored form (`source: src`,
`path: content`, `supersedes` naming `@putnami/agent-workflows` and
`@putnami/maintainer-workflows`) and `cliContract: 5`. It carries the skills,
the worker profiles, the execute references and the helper scripts with their
test suites; `options.agent-artifact.requiredSkills` lists the skills. The
helpers are Bash scripts only: the execute verifier is the CLI command
`putnami tree verify`, so the extension needs no JavaScript runtime on the
contributor's machine. Its
content policy forbids credentials, backend client invocations, tracker labels,
hosted repository addresses, private memory identities and this repository's
paths in every emitted file.

This repository declares the extension by path and opts in with
`agentArtifacts: ["extension:@putnami/contributor"]`, so it runs exactly the
content it distributes.

### 2. `putnami-*` skills are deprecated aliases

`putnami-plan`, `putnami-change`, `putnami-check` and `putnami-review` are
aliases of `plan`, `execute`, `check` and `code-review`. Each names its
canonical skill and keeps its local restriction (a local plan, a local change,
local evidence, a local review). An alias is removed only after the migration
window the entry-point migration document announces. No alias carries an asset.

### 3. Repository conventions are policy

Everything that differs between repositories is a member of
`options["@putnami/contributor"]` in `putnami.workspace.json` (format version
1): backlog states, filters and ranking labels, tier labels, plan labels, claim
assignees, the claimed and delivered task states, worker profiles per tier, the
language rule, extra documentation roots, publication defaults and the optional
`contentBump` integration. Every member is optional. The block holds no
credentials and no provider settings: a state-to-label mapping or a repository
name belongs to the provider's binding. Putnami's own values are documented as a
reference, not a default.

### 4. Instructions name worker profiles; profiles carry the model

Skill and worker bodies never name a model. They delegate to the profile the
policy selects per tier (`workers.light|standard|heavy|analyst`). The shipped
profiles' host metadata (`claude.yaml`, `codex.toml`) carries the reference
model and effort. The content is one digest-bound tree, so it cannot vary per
workspace: a repository that needs other models names its own profiles under
`workers`, and a local edit of a shipped profile is preserved and reported by
the ownership rules.

### 5. Publication goes through the contracts

`finalize-pr.sh` owns Git, the gate, trailers and the dossier, and calls no
backend directly:

- A task is named by `--task-source <source> --task-id <id>` (optional).
  `--issue` fails with a message naming the replacement.
- A read before any write checks that the proposals contract is bound and that
  a named task exists.
- `proposals upsert` finds or creates the proposal for the exact base and head,
  with the pushed head commit. An `unresolved` answer stops the helper with the
  reconciling read and no retry; a resumed run repeats the identity-keyed
  upsert, which never creates a second proposal.
- `proposals status` reads back head, base, head commit, state, assignees and
  labels.
- The verification record (changed files, gate, local proof) is a
  `## Verification` section of the proposal body, because tasks v1 has no
  comment operation.
- `tasks transition` moves the task to `states.delivered`, with
  `expectedRevision` when the provider enforces it, even when the task already
  reads as that state: the provider decides whether its representation changes.
- `publication.taskReference`, rendered with the task id, goes in the commit
  and the body.
- The helper prints `PROPOSAL_REF=` and, when the provider has one,
  `PROPOSAL_URL=`.

The finalizer requires the `proposals status` read-back to carry every
assignee and label the upsert answer carried
([collaboration ADR 0001](../../../../protocols/collaboration/doc/adr/0001-collaboration-provider-contracts.md)),
and prints both lists (`none` when empty). A member the read-back omits is not
reported by the provider: the finalizer says it did not verify it and
continues. A member only the upsert answer carries stops it.

The finalizer never reads provider settings. Label copying and author
assignment are provider settings, and the provider checks they took effect
before it answers (the GitHub provider answers `unresolved`,
`github.incomplete`, when GitHub dropped one). A provider that silently ignores
its own setting must be caught by its own suite.

### 6. The language rule is policy

The English-only detector runs in `check` and in the finalizer only when
`verification.language` is `en`. Its `tasks` mode pages through `tasks find`;
`github` is a deprecated alias that says what it no longer covers. The
proposals contract finds proposals by base and head only, so no mode lists
every open proposal.

## Consequences

- One source feeds every entry point on both hosts; the drift gate builds it
  from the extension declaration.
- A workspace that binds no proposals provider cannot use the finalizer; it
  fails before any write and names the binding to add.
- A clone holding ownership records of the superseded artifacts migrates them
  with `putnami migrate agent-content @putnami/contributor --apply`.
- Backlog selection reads semantic states: on GitHub, an open issue without a
  state label is `open`.

## Rejected alternatives

- **Two artifacts with different content policies.** The workflows drift, and
  the public one never gains what the maintained one learns.
- **Template the content per workspace.** Per-workspace bytes would need a
  second resolution beside the digest-bound pin.
- **Keep `gh` in the helpers behind a flag.** Neutral content would still carry
  a backend, and a second backend would need a second flag.
- **Read `options.collaboration.proposals.settings` in the finalizer.** Puts
  provider vocabulary in neutral content; each provider would need a reader.
- **Declare expected labels in the repository policy.** A second copy of the
  provider's prefixes, which drifts.
