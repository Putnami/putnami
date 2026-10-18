# ADR 0001 — One contributor extension, configured by repository policy, publishing through the collaboration contracts

- **Status**: accepted, amended by decision D-013 in the root
  `decisions.json`: `putnami migrate agent-content` is deleted. A clone that
  still holds ownership records of the superseded artifacts deletes the
  records its install refusal names.
- **Scope**: `@putnami/contributor` (`tooling/contributor`), this repository's
  `putnami.workspace.json`, the generated `.agents/`, `.claude/` and `.codex/`
  trees

## Context

The contributor workflows (build, spec, design, prototype, review, adr, check) serve this
repository and every other workspace. A public copy and a maintainer copy would
duplicate them and make "public" mean "less capable" instead of "configured
differently". Extensions can ship agent content
([`protocols/extension` ADR 0006](../../../../protocols/extension/doc/adr/0006-agent-content-is-an-additive-contract.md)),
and the collaboration contracts give every backend operation a
provider-neutral call
([collaboration ADR 0001](../../../../protocols/collaboration/doc/adr/0001-collaboration-provider-contracts.md)).

## Decision

### 1. One content-only extension (superseded in part by ADR 0004)

[ADR 0004](0004-the-contributor-extension-ships-a-compiled-executable.md)
gives the extension a compiled runtime and moves its packaging to
`@putnami/go`. The agent content below is unchanged.

`tooling/contributor` is the extension `@putnami/contributor`. It runs nothing:
its manifest declares only `agentContent` in the authored form (`source: src`,
`path: content`, `supersedes` naming `@putnami/agent-workflows` and
`@putnami/maintainer-workflows`) and `cliContract: 5`. It carries the skills,
the worker profiles and the build references; `options.agent-artifact.requiredSkills`
lists the skills. `putnami change verify`, `putnami change finalize`,
`putnami change language` and `putnami doctor --machine` provide the native
verifier, publication, language check and machine load. An active audit integration retains only a thin shell relay at
`check/scripts/english-only.sh` to the native language command; no separate
language evaluator remains. Its
content policy forbids credentials, backend client invocations, tracker labels,
hosted repository addresses, private memory identities and this repository's
paths in every emitted file.

This repository declares the extension by path and opts in with
`agentArtifacts: ["extension:@putnami/contributor"]`, so it runs exactly the
content it distributes.

### 2. Former aliases (superseded by ADR 0002)

The initial extension carried `putnami-plan`, `putnami-change`,
`putnami-check` and `putnami-review` aliases. ADR 0002 records their
owner-authorized breaking removal with the other legacy entries. No
alias invocation remains available in the current package.

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
ADR 0002 removes the entries that consumed backlog states, filters, ranking
and plan labels; retaining those stored values does not activate task selection.

### 4. Instructions name worker profiles; profiles carry the model

Skill and worker bodies never name a model. They delegate to the profile the
policy selects per tier (`workers.light|standard|heavy|analyst`). The shipped
profiles' host metadata (`claude.yaml`, `codex.toml`) carries the reference
model and effort. The content is one digest-bound tree, so it cannot vary per
workspace: a repository that needs other models names its own profiles under
`workers`, and a local edit of a shipped profile is preserved and reported by
the ownership rules.

### 5. Publication goes through the contracts

`putnami change finalize` owns the authorized proposal lifecycle through
the collaboration contracts. Build produces the local gate and dossier before
calling it; native finalization never runs or retries a local gate:

- A task is named by `--task-source <source> --task-id <id>` (optional).
  The request also gives `--base`, `--branch`, `--title` and `--body-file`.
  A draft call uses `--draft`; a ready call uses current
  `--verification-file` or explicit hosted `--gate ci`.
- A read before any write checks that the proposals contract is bound and that
  a named task exists.
- `proposals upsert` finds or creates the proposal for the exact base and head,
  with the pushed head commit. An `unresolved` answer stops the command with the
  reconciling read and no retry; a resumed run repeats the identity-keyed
  upsert, which never creates a second proposal.
- `proposals status` reads back head, base, head commit, state, assignees and
  labels.
- The verification record (gate and local proof) is a proposal comment through
  `proposals review` when the provider offers it, with the comment verdict;
  the proposal body remains the squash commit message. If review is
  unsupported, the command reports the record locally.
- `tasks transition` moves the task to `states.delivered`, with
  `expectedRevision` when the provider enforces it, even when the task already
  reads as that state: the provider decides whether its representation changes.
- `publication.taskReference`, rendered with the task id, goes in the commit
  and the body for a complete mandated outcome. An intermediate unit uses
  `--partial-delivery`, keeping a non-closing parent reference and skipping
  its delivered transition. The command also refuses a caller closing
  directive and, for a referenced workflow dossier, refuses normal parent
  close when `outcomeComplete` is false.
- The command returns the exact proposal reference and any provider URL.

Native finalization requires the `proposals status` read-back to carry every
assignee and label the upsert answer carried
([collaboration ADR 0001](../../../../protocols/collaboration/doc/adr/0001-collaboration-provider-contracts.md)),
and prints both lists (`none` when empty). A member the read-back omits is not
reported by the provider: the command says it did not verify it and
continues. A member only the upsert answer carries stops it.

Native finalization never reads provider settings. Label copying and author
assignment are provider settings, and the provider checks they took effect
before it answers (the GitHub provider answers `unresolved`,
`github.incomplete`, when GitHub dropped one). A provider that silently ignores
its own setting must be caught by its own suite.

### 6. The language rule is policy

`putnami change language` runs in `check` and native finalization only
when `verification.language` is `en`. Its `tasks` mode pages through `tasks find`.
The
proposals contract finds proposals by base and head only, so no mode lists
every open proposal.

## Consequences

- One source feeds every entry point on both hosts; the drift gate builds it
  from the extension declaration.
- A workspace that binds no proposals provider cannot finalize a proposal; it
  fails before any write and names the binding to add.
- A clone holding ownership records of the superseded artifacts migrates them
  with `putnami migrate agent-content @putnami/contributor --apply`.
- Build does not select from a backlog. A named task operation uses the
  provider's semantic state and requires its own authority.

## Rejected alternatives

- **Two artifacts with different content policies.** The workflows drift, and
  the public one never gains what the maintained one learns.
- **Template the content per workspace.** Per-workspace bytes would need a
  second resolution beside the digest-bound pin.
- **Keep `gh` in the helpers behind a flag.** Neutral content would still carry
  a backend, and a second backend would need a second flag.
- **Read `options.collaboration.proposals.settings` in finalization.** Puts
  provider vocabulary in neutral content; each provider would need a reader.
- **Declare expected labels in the repository policy.** A second copy of the
  provider's prefixes, which drifts.
