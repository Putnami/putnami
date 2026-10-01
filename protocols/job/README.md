# `go.putnami.dev/protocol/job`

The **job execution context** contract: the JSON document the Putnami
orchestrator writes for every job subprocess and passes via
`--putnamiContext <path>`, plus the `PUTNAMI_*` environment variable
mirror used by shell extensions.

The orchestrator (the CLI) is the producer; extension SDKs in every
language are consumers. Before this package the shape lived only in the
Go extension SDK, so any non-Go SDK had to reverse-engineer it from Go
structs — and the producer and consumer had already drifted on field
sets.

## What

- `Context` — workspace, project, selected-project metadata for
  workspace-aggregate jobs, the invocation's resolved project selection,
  extension, job identity, output and cache paths, merged `params`, file
  patterns, and version metadata.
- `Params` — the parameter map plus the **coercion rules** every SDK
  must implement identically: string fallback keys, bool synonyms
  (`"1"`, `"yes"`, `"on"`), and numeric strings resolving like JSON
  numbers so CLI flags and config defaults behave the same.
- `Parse` / `ParseFile` — the lenient consumer entry points: unknown
  fields are ignored so an older extension binary keeps working when a
  newer orchestrator adds fields.
- `ParseStrict` / `ParseAndValidate` — the strict producer/conformance
  entry points: unknown fields are rejected and required fields are
  enforced.

## How

- The CLI builds the context in
  `tooling/cli/internal/jobs/context.go` and proves conformance with a
  test that round-trips its output through `ParseAndValidate`.
- The Go extension SDK (`go.putnami.dev/sdk/extension/context`) aliases
  these types, so all extensions consume the protocol shapes directly.
- Fixtures under `fixtures/{valid,invalid}` are the cross-language
  corpus: a non-Go SDK validates its parser against the same files.

## Versions

A document **without** a `protocolVersion` member is version 1 — that is
the only way to tell the two apart. This package parses and validates
both, and the v1 corpus is unchanged.

| Version | Members | Corpus |
| --- | --- | --- |
| 1 | everything above | `fixtures/{valid,invalid}` |
| 2 | v1 plus `identity`, `staging`, `extension.runtimePath`, `extension.cacheRoot`, `project.metadata`, `project.type`, `project.dependencyClosure`, `invocation`, `selection`, `workspaceProjects`, `userScope`, `ProjectRef.config` and `ProjectRef.extensions` | `fixtures/v2/{valid,invalid}` |

**Putnami's orchestrator produces v2 only.** The CLI requires an extension
contract whose SDK reads v2, so a subprocess that reaches this context is
already stamped for it; emitting v1 "to be safe" would mean withholding the
typed identity from every job in order to accommodate an SDK the manifest
loader has already rejected. v1 stays specified and parseable here for
documents recorded by earlier builds and for non-Putnami producers.

The compatibility rules — optional within v2, forbidden at v1, lenient
consumers and strict producers, frozen v1 corpus — are recorded with their
rejected alternatives in
[`doc/adr/0001-v2-optional-members.md`](doc/adr/0001-v2-optional-members.md).

- **`identity`** (required at v2) is the typed task identity, reused
  **as the same type** from `protocols/cli` (`TaskIdentity`): the value a
  subprocess reads is the one the plan, the session records and the
  result envelope carry, so anything it emits joins to the
  orchestrator's view of the task. Its `key` is a derived view of the
  structured members (`project.id + ":" + task.name`) and a key that
  disagrees with them is rejected (`job.invalid_key`).
- **`staging`** (optional at v2) names the task-owned directories the job
  must write its declared outputs into — one per declared-output root of
  the extension task contract v3 (`project`, `workspace`,
  `command-output`), so resolving a declared output is exactly
  `Staging.PathFor(output.EffectiveRoot())` joined with the declared
  path. All four paths are absolute, the three roots live inside
  `staging.root`, and they may not nest or coincide: the task contract's
  "one owner per output" rule holds across roots only because two roots
  are two different trees.

Four further v2 members are pre-declared for lifecycle-specific work. They are
optional within v2: a producer emits each only for work that declares the
corresponding lifecycle, and later work implements this shape rather than
reshaping the contract under consumers already reading it.

- **`extension.runtimePath`** — the absolute path to the resolved runtime
  executable the orchestrator prepared for this extension. The generic
  replacement for the language-specific wrapper paths a subprocess used to
  rebuild from `extension.root`.
- **`extension.cacheRoot`** — the absolute path to the machine-global cache
  directory this extension owns, replacing one environment variable per
  ecosystem. Distinct from the top-level `cacheRoot`, which is the workspace's
  shared mutable scratch.
- **`project.metadata`** — provider-owned project metadata, namespaced by the
  contributing extension's name and kept raw. It is the derived counterpart of
  `project.options`: `options` is authored in the project's `putnami.json`,
  `metadata` is produced by the owning extension's workspace probe. Namespacing
  is what lets several providers contribute to one project without a merge rule
  per field. It is a writer-ownership rule, not read isolation: the full map is
  delivered and each consumer selects its own namespace. Each block must be a
  JSON object under a non-empty key (`job.invalid_metadata`).
- **`invocation`** — the typed, non-secret invocation locator: an opaque `id`
  and absolute `artifactRoot`. The orchestrator includes it only in contexts
  for the producer, every listed consumer, and the finalizer of one
  `finalizes` relation. Invocation-scoped declared-output paths resolve under
  this root. Extension commands receive the same location explicitly as
  `{invocationArtifactRoot}` in their manifest arguments; no task derives a
  scratch path from the current directory. The locator is never copied into
  results, events, session records, telemetry, or cache traffic.

Two more optional v2 members exist for the same reason and are of the same
kind: a fact the orchestrator has already RESOLVED, handed to the task that
needs it instead of being re-derived across the process boundary.

- **`project.type`** — the project's resolved classification (`application`,
  `library`, …) after the authored-over-probed merge. Empty means the workspace
  classified nothing, which consumers read as `application`.
- **`project.dependencyClosure`** — the project **plus** every workspace
  project reachable through its dependency edges, in canonical project-id
  order, each located by `name`, `path` and `fullPath` exactly like a selected
  project. The seed is included because a task aggregating over a project's
  whole graph must read the project's own contribution beside its
  dependencies'. The graph stays the orchestrator's: a task that rebuilt it
  from manifests would answer a different question (scope includes, name→id
  translation and out-of-workspace edges are decided during resolution).

They exist because extension-owned infra aggregation replaced a core gate that
walked the graph itself: the policy (what a workload declares, what its runtime
defaults are) is the language extension's, the graph is core's.

- **`selection`** — how the command invocation chose the projects in scope:
  `mode` (`all`, `projects` or `impacted`), `scoped`, the `baseline` and
  `baselineSource` `--impacted` resolved to, the sorted `projects` ids, and
  `emptyImpact`. `selectedProjects` answers *which* projects, and only for the
  jobs that receive it; this answers *what the user asked for and what it
  resolved to*, for every job. A validator told it ran over three projects
  cannot otherwise tell a deliberate `--projects` narrowing from a
  whole-workspace run of a three-project tree, and the two license different
  conclusions.

  The members must agree with each other, because a consumer acts on them:
  `scoped` is false exactly at mode `all`, `projects` is a non-null sorted array
  free of duplicates and blank ids, and `emptyImpact` — the legitimate
  `--impacted` no-op — is reportable only at mode `impacted` and only with an
  empty project list (`job.invalid_value`, `job.missing_field`).
  `baseline`/`baselineSource` are evidence about a resolution rather than a
  claim about scope, so they carry no rule beyond being meaningful only at mode
  `impacted`.

  `releaseSetProjects` is the half of `projects` whose publish and package steps
  a release-set plan owns, sorted, duplicate-free and always a SUBSET of
  `projects` (`job.invalid_value`). A session that names `publish` beside other
  commands plans two things at once: the publication the channel head decided,
  and the verification the caller's own selection asked for. `projects` stays
  what the run plans and executes — the union — and `mode`, `baseline` and
  `baselineSource` keep describing how the verification half was chosen, so the
  two are never confused. It is absent when the session coordinates no release
  set, and when the plan selected no member.

  The shape is the CLI's own `ResolvedSelection`
  (`tooling/cli/internal/commands/shared/selection.go`) member for member and
  **byte for byte**, so one resolver answers both the interactive command
  surfaces and the planned jobs. `releaseSetProjects` is the one exception, and
  it is one because a read-only selection surface coordinates no publication: it
  is produced by the orchestrator alone, which is why the CLI type omits it and
  every document that type can produce still marshals identically. The type is
  not imported — it carries resolved workspace projects and would drag the
  CLI's workspace loader into this protocol — so the equality is pinned by a
  drift test on the CLI side, the same arrangement the staging roots use against
  the manifest protocol.

- **`workspace.options`** — the committed workspace-level extension option
  blocks, raw and optional at v2. A workspace-scoped task reads policy from the
  same config the orchestrator loaded rather than growing a second workspace
  loader. A present value is a non-null object of non-null named objects. A task
  that reads one must name `putnami.workspace.json` in its cache inputs.

- **`workspaceProjects`** — the **complete** resolved workspace membership, in
  canonical project-id order, whatever this run selected. It reaches every job:
  a project-scoped rule can make a narrow claim while resolving an identity or
  reviewed declaration against the whole workspace.

  It is not a longer `selectedProjects`. That member is what the run acts on;
  this is what the workspace CONTAINS, and a validator whose subject is the
  workspace gets both possible wrong answers from the selection alone. Under
  `--impacted` a project outside the selection reads as absent — a false
  violation — and an edge into it is never walked — a missed one. Both were
  measured on `architecture validate` before this member existed: a narrowed run
  reported `architecture.unknown_project` for a real member, and an unnarrowed
  one reported zero observed edges over a workspace that had one undeclared
  cross-domain dependency.

  Every entry carries `id`, `name`, `path` and `fullPath`, and the listing is
  sorted by id and free of duplicates (`job.missing_field`,
  `job.invalid_value`): a membership answer must be the same bytes for the same
  workspace whichever run produced it. It also carries the resolved direct edges
  (`dependencies`, below), so a producer that publishes this member has resolved
  the graph. Its `config` member carries each parsed authored `putnami.json`, raw,
  so a validator can honor a sibling's workspace-protocol declarations without
  rediscovering manifests.

- **`userScope`** — present exactly when the command runs outside any
  workspace, from an extension pinned in the user scope (`putnami extensions
  install --user`). Its one member, `callerDir`, is the absolute directory the
  user ran the command from: the job process starts there, and it is the
  directory the job acts on. It has no workspace manifest, and the orchestrator
  writes nothing in it.

  When `userScope` is present, `workspaceRoot`, `workspace.rootPath` and
  `project.fullPath` name the user-scope directory (`~/.putnami/user`) the
  orchestrator runs the job in, so `outputPath` and `cacheRoot` live there
  too. That directory is not a workspace the user owns: a job must not read
  project files from it, and should write only under `outputPath` and
  `cacheRoot`. A job inside a workspace never carries the member, so its
  absence means "a workspace". A present member is a non-null object with an
  absolute `callerDir` (`job.invalid_value`, `job.missing_field`).

A v1 document may not carry v2 members (`job.unexpected_field`): a v1
consumer would ignore exactly the members a v2 consumer would trust.

## Project references

`selectedProjects[]`, `project.dependencyClosure[]` and `workspaceProjects[]`
share one shape. Beyond `id`, `name`, `path` and `fullPath`, five optional
members carry facts the orchestrator already owns, so a task never re-derives
them:

- **`outputPath`** — the absolute directory this project's job writes its
  structured outputs to, so a batched extension process captures each project's
  outputs into that project's own cache entry.
- **`sourceName`** (v2 only) — the identity the project **declared**: its own
  `putnami.json` name, or the name its provider read out of its native manifest,
  **before** a scope `namePattern` override. `name` is the resolved answer after
  that override, and the two differ only when nothing declared a name and a
  scope supplied one. A v1 document carrying it is rejected
  (`job.unexpected_field`), like every other v2 member.

  `sourceName` exists so a provider's workspace-sync task acts on exactly the
  divergence set the orchestrator reports. A task that aligns native manifest
  names **must not** rename a manifest whose `sourceName` already equals `name`:
  that manifest is where the resolved identity came from, so it is not out of
  alignment, and rewriting it orphans every sibling reference that still spells
  the old name — a bun `workspace:*` dependency, say. Because the member is
  optional, a task that receives no `sourceName` must decline the rename: not
  renaming is recoverable, renaming is not.
- **`version`** (v2 only) — the project's **resolved** effective version: its
  own authored version, else the nearest scope's, else the workspace's. A v1
  document carrying it is rejected (`job.unexpected_field`), like every other v2
  member.

  A package reference is a **pair**. A selector that locates a contribution by
  package names both the package and its version, and a consumer handed only the
  name either widens the match or fails it — and failing is the quiet outcome:
  the reference degrades to "source unavailable" while every versionless
  reference beside it still resolves, so the narrowing surfaces as a missing
  verdict rather than as an error. It is resolved for the same reason
  `project.type` is: the authored-over-scope-over-workspace fallback is the
  orchestrator's, and a task re-deriving it would need the scope chain this
  contract does not carry. Because the member is optional, a task that receives
  no `version` must match by **name alone** — an unversioned match is wider than
  intended, a match against an invented version is wrong.

- **`dependencies`** (v2 only) — the ids of the projects this one depends on
  **directly**, in the order the orchestrator resolved them. A v1 document
  carrying it is rejected (`job.unexpected_field`), like every other v2 member.

  Direct is what `project.dependencyClosure` is not. The closure answers
  "everything this project transitively reaches", which is what an aggregation
  over a graph needs; this answers "what does this project itself declare",
  which is what a rule ABOUT the graph needs. An architecture contract
  authorizes exact producer-to-consumer edges, so a consumer handed only a
  closure cannot tell an edge it must justify from one its dependency already
  justified. Ids rather than names, because an id is the workspace's stable
  identity and the name→id table is not on this wire; out-of-workspace edges are
  omitted, since there is no member to point at.

  Absence is read from the **containing** member. Inside `workspaceProjects`,
  whose producer has resolved the graph by definition, an entry without
  `dependencies` declares none. Anywhere else absence says nothing, and a
  consumer whose verdict is about edges must decline rather than read "I was
  told nothing" as "there is nothing".

- **`config`** (v2 only) — the project's authored `putnami.json`, parsed by the
  orchestrator and re-encoded raw because `protocols/workspace` owns its shape.
  It is populated inside `workspaceProjects`, where a task can need a reviewed
  fact about a sibling such as `featureAuthority`; the selection and dependency
  closure carriers omit it to avoid repeating the same document. When present
  it must be a non-null JSON object (`job.invalid_value`).

- **`extensions`** (v2 only) — the extension references the project resolves
  to: its `putnami.json` list, else the provider's view, else its scope's. A
  reference is an extension name or a workspace-relative path such as
  `/go/extension`. It is populated inside `workspaceProjects`, where a
  workspace task needs to know which extension's option blocks apply to a
  sibling: a block keyed by `@putnami/go` applies to Go projects only.

All six are omitempty, so a producer that has nothing to say emits none.

## Environment variable mirror

Shell extensions that cannot parse JSON read these variables, set by
the orchestrator alongside the context file:

| Variable | Source field |
| --- | --- |
| `PUTNAMI_WORKSPACE_ROOT` | `workspaceRoot` |
| `PUTNAMI_WORKSPACE_NAME` | `workspace.name` |
| `PUTNAMI_EXTENSION_NAME` | `extension.name` |
| `PUTNAMI_JOB_NAME` | `job.name` |
| `PUTNAMI_OUTPUT_PATH` | `outputPath` |
| `PUTNAMI_CACHE_ROOT` | `cacheRoot` |
| `PUTNAMI_PROJECT_ROOT` / `PUTNAMI_PROJECT_PATH` | `project.fullPath` |
| `PUTNAMI_PROJECT_NAME` | `project.name` |
| `PUTNAMI_SELECTED_PROJECTS` | `selectedProjects[].name` |
| `PUTNAMI_SELECTED_PROJECT_IDS` | `selectedProjects[].id` |
| `PUTNAMI_SELECTED_PROJECT_PATHS` | `selectedProjects[].path` |
| `PUTNAMI_SELECTED_PROJECT_ROOTS` | `selectedProjects[].fullPath` |
| `PUTNAMI_CALLER_DIR` | `userScope.callerDir`; unset inside a workspace |

The orchestrator also sets `PUTNAMI_CLI_USER_AGENT` to the invoking CLI's
User-Agent (`putnami-cli/<version>`, or the agent identity the CLI was given).
An extension that calls a registry or another HTTP service sends it as its
`User-Agent`, so the request is attributed to the CLI that started the job.
Without it, the TypeScript and Go extensions send `putnami-cli/dev`.

## Known producer gaps

- `filePatterns` is part of the contract and read by consumers, but the
  current orchestrator does not populate it.
- `project.compile` is part of the contract and may be consumed by
  extensions, but the current orchestrator does not populate it.

## Schemas and fixtures

This module has **no JSON Schema**. The contract is the Go types plus the
fixture corpus, and the strict parser is the executable specification — a
schema would be a third description of the same shape with nothing keeping it
honest.

- v1 corpus: [`fixtures/valid`](fixtures/valid) (3) and
  [`fixtures/invalid`](fixtures/invalid) (8).
- v2 corpus: [`fixtures/v2/valid`](fixtures/v2/valid) (10) and
  [`fixtures/v2/invalid`](fixtures/v2/invalid) (46).

## Support status

- **Subject**: `go.putnami.dev/protocol/job`, kind `protocol`.
- **Status**: `stable`, recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json) — the only reviewed
  authority for support status (contract:
  [`protocols/support/README.md`](../support/README.md)).
- **Owner**: this project (`protocols/job`). The CLI is the sole first-party
  producer and the extension SDK the sole first-party consumer; neither owns the
  shape.
- **Evidence**: both versions are pinned by named tests —
  `TestConformance_ProtocolVersion` and `TestConformance_ProtocolVersion2` — and
  the v1 corpus is frozen byte-for-byte by `TestV1DocumentBytesUnchanged`, with
  `TestV1FixturesStayV1` asserting no v1 fixture carries a v2 member.
  `TestInvalidFixtureCoverage` and `TestV2InvalidFixtureCoverage` assert every
  required-field and version reject branch is triggered by a fixture, so a
  relaxed check turns a red fixture green and fails. The producer proves
  conformance by round-tripping its own output through `ParseAndValidate`
  (`tooling/cli/internal/jobs/context_protocol_test.go`).
- **Cross-implementation parity is claimed by nothing.** The fixture corpus is
  written to be language-neutral and is the file set a non-Go SDK would validate
  against, but today the only parser is this Go package (every first-party
  extension binary is Go and consumes it through
  `tooling/extension-sdk/context`). `stable` here is a commitment about the wire
  shape's evolution, not a statement that a second implementation exists.
- **`default` and `parity`**: no claim is recorded on either axis. Omission is
  not a denial — see the support vocabulary.

## Specs and durable decisions

There is deliberately **no user-facing feature or spec for this module**. Its
whole content is the handshake between the orchestrator and a job subprocess: a
user never authors it, reads it, or asks for it, and a product feature per
technical wire contract would be a promise with no user on the other end.

The user-facing surface that owns the outcome is **every task-running CLI
command** — `putnami build`, `putnami test`, `putnami lint` and the rest — and
the extension-authoring experience documented by
[`tooling/extension-sdk`](../../tooling/extension-sdk). A spec belongs with the
change one of those is made for.

Durable decisions recorded here:

- [`doc/adr/0001-v2-optional-members.md`](doc/adr/0001-v2-optional-members.md)
  — v2 members may be optional but never unconstrained; a v1 document may not
  carry one; consumers parse leniently while producers and conformance parse
  strictly; the v1 corpus is frozen.
