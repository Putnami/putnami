# Extension Protocol

The contract for `putnami.extension.json`: how a Putnami extension declares
commands, tasks, pipelines, cache policy, lifecycles, and agent tools as **data**
instead of as CLI code.

## Why

Before this contract, adding a language to Putnami meant editing the CLI: a
table of command pipelines, a table of marker filenames, a language-specific
cache collector, a launcher per known extension name. Every one of those tables
had to be edited in lock-step, and an unknown language had no path at all.

Declaring behaviour as data inverts that. The CLI stops recognizing extensions
by name and asks the manifest instead — which means the manifest can be
validated, tested, cached on, and reasoned about by tooling in any language.

## What

`putnami.extension.json` models:

- extension identity, dependencies, and workspace dev dependencies;
- user-facing **commands** and command groups, with their flags;
- **pipeline steps** and input/output bindings;
- executable **tasks** with their subprocess conventions;
- the **task contract** — a task's exact outputs, its effects beyond them, and
  whether it rewrites the sources it reads (`declares`, protocol v3; additive,
  so a task without it keeps v2's inferred semantics);
- the three **lifecycle primitives** (protocol v3, additive): the extension's own
  `runtime` and how to prepare it, the `workspace` adapter that owns project
  discovery, and invocation-scoped sensitive outputs with explicit
  `runOn: "finally"` / `finalizes` relations — see
  [`doc/07-lifecycles.md`](doc/07-lifecycles.md);
- **ecosystem profiles** — the ecosystems this extension owns (`ecosystems`) and
  the ones it publishes to without owning (`uses`), each carrying its coordinate
  and version rules, its channel projection, the schema of its workspace
  registry entry, and its publish job;
- **cache policy** and cache-key inputs, including the reserved cache-lifecycle
  commands, the per-extension machine cache root, and core's shared OCI layer
  cache handoff;
- manifest **hooks**;
- embedded **JSON-schema contracts** for task I/O;
- namespaced **MCP tool** descriptors and their one-request subprocess contract.

### How tooling consumes it

- `LoadManifest()` parses a manifest and negotiates its declared `cliContract`
  against the CLI's `CurrentContract` (see *Versioning* below).
- `BuildTemplateVars()` / `ExpandTemplateVars()` resolve manifest path variables
  such as `{workspaceRoot}` and `{outputRoot}`.
- `DeriveTaskCacheKey()` converts declared inputs into deterministic cache-key
  components; `task_digest.go` hashes one task's contract so a cache key misses
  — rather than falsely hits — when that contract changes.
- `ManifestProtocolVersion()` reports which contract version a manifest actually
  exercises. The v3 vocabulary is self-identifying: the `runtime` and
  `workspace` sections and lifecycle step relations count as much as a task's
  `declares` block.
- `OutputsOverlap()` is the single "one owner per output" predicate, applied both
  to root-relative declarations and to refs resolved to absolute directories.
- `ValidateRuntime()` and `ValidateWorkspaceAdapter()` are the two
  manifest-level lifecycle harnesses. `ValidateManifest()` runs both, and both
  are inert for a manifest declaring neither section.
- `NormalizeRelativePath()` and `NormalizeInputPattern()` are the shared path
  rules every declared path answers to — no absolute paths, no escapes above the
  declared root, no template expansion inside a path.
- `ValidateTaskContracts()` is the task-contract conformance harness: it proves
  exact paths, one owner per output, and honest effects across a whole manifest.
  `ValidateManifest()` runs it, so `putnami dev extension validate`, the
  package-time gate, and the extension SDK's build-time authoring gate all reach
  the same verdict from one implementation.
- `ValidLifecycleFailureCodes` is the closed, typed vocabulary the runtime,
  probe, and sensitive-artifact lifecycles report failures through, so an SDK in
  any language can switch on them exhaustively.
  `ValidLifecycleRecoveryCodes` separately names successful recovery such as
  orphan lease reaping, so recovery can never also classify an invocation as
  failed.
- `ToolCallRequest` / `ToolCallResult` define the stdin/stdout boundary for
  extension-contributed MCP tools. The CLI owns MCP JSON-RPC; extensions retain
  their own execution, authentication and network behaviour. An opted-in
  `ToolCallRequest.agent` object carries bounded MCP client/model provenance,
  and `AgentIdentity.ApplyHTTPHeaders` applies its stable `User-Agent`,
  `Putnami-Agent-Harness` and `Putnami-Agent-Model` headers safely.
- `ToolDefinition.workspaceSelection` is how a tool that answers ABOUT THE
  WORKSPACE asks the orchestrator for the view it already holds. The CLI then
  puts two members on the request: `workspaceProjects` (`ToolProjectRef`, the
  complete resolved membership with each project's resolved direct edges,
  version, type and authored `putnami.json`) and `selection` (`ToolSelection`,
  the projection the canonical `projects` / `impacted` / `baseline` arguments
  resolved to). Both are omitted for a tool that does not declare it, so the
  addition changes nothing for a tool that never asked.

  It is opt-in for one reason: those three argument names are the CLI's
  vocabulary, not every extension's, and a tool that means something else by
  `projects` must not have its arguments rejected as unknown project selectors.
  It exists because the alternative is worse — an extension that resolved
  `impacted` itself would be a second definition of what the workspace contains,
  and the two would diverge on the first scope, alias or transparent group
  folder. `ToolSelection` marshals byte-identically to `protocols/job`'s
  `Selection`; the equality is pinned by a drift test there, since that package
  already imports this one.
- `ToolCallRequest.provider` (`ToolProviderCall`) is present exactly when the
  CLI calls a tool as the implementation of a collaboration contract operation
  the workspace bound to the extension: it names the contract, version and
  operation, and carries the binding's settings. The contracts, the
  `_meta["putnami.dev/provider"]` declaration and the routing belong to
  [`protocols/collaboration`](../collaboration/README.md); this package does
  not import it.

Detailed contract documentation lives in [`doc/`](doc):
[01 manifest](doc/01-manifest.md), [02 commands and tasks](doc/02-commands-and-tasks.md),
[03 pipelines](doc/03-pipelines.md), [04 validation](doc/04-validation.md),
[05 distribution](doc/05-distribution.md), [06 verbs](doc/06-verbs.md),
[07 lifecycles](doc/07-lifecycles.md).

## Ecosystem profiles

An extension declares what an ecosystem **is**. Before profiles, the release-set
protocol named the five ecosystems it knew and the CLI hard-coded the two it
coordinated, so adding a language meant changing the protocol, the CLI and the
distribution backend at once — and the shape of a registry entry lived in prose
nobody could validate. An extension already owns the publish job of its
ecosystem and the metadata it emits, so it owns the definition too
([ADR 0001](doc/adr/0001-ecosystem-profiles-declared-by-extensions.md), D12/D29).

A profile, under `ecosystems`:

| Field | Meaning |
|---|---|
| `id` | Ecosystem identifier, matching `EcosystemIDPattern` (`^[a-z][a-z0-9-]{0,31}$`). It is the key of the ecosystem's entry in the workspace `registries` section and the value of a release-set member's `ecosystem`. |
| `coordinate.pattern` | RE2 pattern a member's coordinate — its package name in this ecosystem — must match. |
| `version.pattern` / `version.ordering` | RE2 pattern a version must match, and how two versions compare: `semver` or `string`. |
| `channel` | `native` when the registry has a channel projection (an npm dist-tag, an OCI tag), `none` otherwise. An ecosystem with no native channel is followed by `--release` or `--version` only, and `upgrade --channel` skips it. |
| `channelEncoding` | Optional RE2 pattern narrowing `PortableChannelPattern` (`^[a-z0-9][a-z0-9._-]{0,63}$`) for this ecosystem. It may only be **stricter**: one distribution namespace holds every ecosystem's channels, so a name must match both. |
| `registries` | JSON Schema of this ecosystem's entry in the workspace `registries` section. It must describe an object, so the workspace validates a registry entry without knowing the ecosystem. |
| `publish` | The command, in the same manifest, that publishes a member and emits the `published-member` event. |

**One owner per profile.** An extension that publishes to an ecosystem it does
not define lists the id under `uses` and never redeclares it. `ResolveProfiles`
refuses a second owner — byte-identical or not, since two copies diverge the
first time one is edited and the other is not — refuses a `uses` entry no
installed extension and no built-in declares, and refuses a manifest that
`uses` an id it declares itself. Its diagnostics name both manifests, because
the fix is always in one of them.

`ValidateProfile` decides everything one file can decide alone: the id, that
both patterns and any `channelEncoding` compile as RE2, the closed `ordering`
and `channel` vocabularies, that `registries` is an object schema, and that
`publish` names a command of the same manifest — a publish job that does not
exist would otherwise validate here and fail at release time, on the one path
with no recovery. `ValidateManifest` runs it and is inert for a manifest that
declares neither `ecosystems` nor `uses`.

Diagnostic codes: `invalid-ecosystem-profile`, `duplicate-ecosystem-profile`,
`unknown-ecosystem-use`, `ecosystem-use-of-own-profile`,
`invalid-published-member`.

**The `published-member` event.** A publish job emits one `PublishedMember`
(`PublishedMemberEventKind`) per artifact it produced — a project may produce
several members in one ecosystem, so the event carries its own coordinate rather
than inheriting the project's identity. It states the `ecosystem`, the
`coordinate` and `version` actually published, the `artifactDigest`
(`sha256:` plus 64 lowercase hex characters), and for a multi-platform set a
`platforms` map from `os/arch` to that platform's digest.
`ParsePublishedMember` is strict: an unknown field is a rejection, because a
field this build does not know would otherwise be truncated silently out of the
release set.

## Producers and consumers

**Producers** — every extension that ships a `putnami.extension.json`:

| Producer | Path |
|----------|------|
| `@putnami/go` | `go/extension` |
| `@putnami/typescript` | `typescript/extension` |
| `@putnami/python` | `python/extension` |
| `@putnami/scaffold` | `tooling/scaffold` |
| `@putnami/clientgen` | `tooling/clientgen-extension` |

Authors do not hand-write the `cliContract` stamp: the package/publish job
validates the staged manifest strictly and stamps it on success, so the field is
**earned, not claimed**.

**Consumers**

| Consumer | Use |
|----------|-----|
| `tooling/cli/internal/extension`, `.../cli`, `.../engine`, `.../jobs` | Manifest loading, contract negotiation, planning and scheduling |
| `tooling/cli/internal/commandmeta`, `.../commands`, `.../output` | Command surface, help, and rendering |
| `tooling/cli/internal/hooks`, `.../store`, `.../workspace` | Hooks, per-extension cache roots, project discovery |
| `tooling/cli/internal/mcp`, `.../machine`, `.../useragent` | Tool descriptors, machine output, agent provenance headers |
| `tooling/extension-sdk/manifest`, `.../mcp` | The authoring-side gate extension authors build against |
| `go/extension/internal/jobs/{lint,pkg}` | Package-time validation and stamping |
| [`protocols/job`](../job/README.md) | Job context derived from a task declaration |

## Versioning and compatibility

Two version axes, and they are not the same thing.

### Manifest protocol version

`ProtocolVersion` is `ProtocolVersionV3` (`3`). The manifest carries **no**
version member on the wire — the strict parser accepts a single shape and
rejects unknown fields — so this constant is the single anchor a bump must move,
and it is pinned by [`conformance_test.go`](conformance_test.go).

- **v2** is the inferred-output contract: a task's filesystem footprint is
  discovered by capturing a directory and subtracting a baseline, and source
  mutation is a cache-private result marker.
- **v3** adds the task contract (declared outputs, effects, source mutation) plus
  the runtime and workspace lifecycle declarations. It is **additive**: a
  manifest without the v3 vocabulary behaves exactly as v2 did, and tasks may
  migrate their declarations one at a time.

`Manifest.Version` is the extension's own semver, never the protocol version.

### CLI ↔ extension contract

`cliContract` records the contract version the manifest was validated against.
Since contract 3 the negotiation ladder has exactly **two** outcomes — the
contract matches, or the extension does not load:

| Declared `cliContract` | Outcome |
|------------------------|---------|
| Higher than the CLI's `CurrentContract` | Hard error — a future contract is never half-interpreted; the remedy is a newer `putnami` |
| Equal | Enforce strictly — the packager stamped compliance, so a reserved global-flag shadow is a hard error |
| Absent (`0`) or lower | Hard error naming the fix — re-package the extension, or `putnami extensions update` |

A manifest that declares **no contract surface at all** (no commands, no command
groups, no tools — a hook-only framework package) is outside the ladder, because
the packager deliberately never stamps one and there is nothing for the contract
to govern. `DeclaresContractSurface()` is the loader's half of that rule.

Contract 3 is where four contracts move together, and a contract-3 extension
must speak all four: the v3 task contract, job context v2, runtime event
protocol v2, and lock format v2. Adaptation of contract ≤ 2 manifests is
**deleted, not deprecated** —
[`adaptation_ratchet_test.go`](adaptation_ratchet_test.go) parses this package's
own source and fails if an adaptation function reappears, so the middle tier
cannot come back by accident. The reasoning is recorded in
[ADR 0002 of `@putnami/cli`](../../tooling/cli/doc/adr/0002-cli-vnext-contracts.md),
whose scope explicitly covers this module; the boundaries it extends are in
[ADR 0001](../../tooling/cli/doc/adr/0001-cli-foundation-boundaries.md).

Contract 4 adds `sessionPrerequisites`. Their selection, projected parameters
and functional verification gates are part of the command's execution meaning,
so a contract-3 reader must reject the manifest instead of silently planning
only the dependent command. Re-package an authored extension, or run
`putnami extensions update` for an installed one.

## Schemas and fixtures

- Schema: [`schemas/extension.json`](schemas/extension.json).
  [`drift_test.go`](drift_test.go) pins each Go struct's JSON field set against
  the schema in both directions, so a field can never exist on only one side —
  including negative assertions such as "the schema must not declare `when`",
  the runtime-expression field contract v3 deleted.
- Fixtures: [`fixtures/valid/`](fixtures/valid) — `minimal`, `full`,
  `multi-command`, `command-groups`, `mcp-tool`, `with-contracts`,
  `lifecycle-v3`, `task-contract-v3`, `task-contract-v3-ownership`,
  `ecosystem-owner`, `ecosystem-uses`;
  [`fixtures/invalid/`](fixtures/invalid) — 37 counter-examples covering output
  overlap, path escapes, effect conflicts, pipeline cycles, reserved global-flag
  shadows, finalizer misuse, unresolved schema refs, workspace-adapter errors,
  and ecosystem-profile errors.
- Golden: [`testdata/task_digests.golden.json`](testdata/task_digests.golden.json)
  pins task-contract digests, so a change to how a contract hashes is visible as
  a golden diff rather than as a silent cache-key shift.
- Tests: `conformance_test.go` (fixture corpus and version anchor),
  `determinism_test.go`, `manifest_matrix_test.go` (the executable `cliContract`
  compatibility matrix), `ecosystem_test.go` (the profile corpus and its
  fixture→code table), and `adaptation_ratchet_test.go`.

## Support status, owner, and evidence

| | |
|---|---|
| **Status** | `stable` — see the `go.putnami.dev/protocol/extension` entry in the workspace-root [`putnami.support.json`](../../putnami.support.json), which is the only authority for this value |
| **Owner** | The `protocols` scope (`protocols/putnami.json`). This module is the sole authority for the manifest vocabulary and the contract ladder; neither the CLI nor an SDK may accept a manifest shape this package rejects. |

Evidence behind `stable`:

- a pinned `ProtocolVersion` anchor plus an explicit, additive v2 → v3 story, and
  an executable compatibility matrix for the `cliContract` ladder;
- a published JSON Schema held against every Go wire struct by a two-way drift
  test, including negative assertions for deleted fields;
- a 48-document valid/invalid fixture corpus and a task-digest golden;
- a source-parsing ratchet that prevents the deleted adaptation tier from
  returning;
- five real producers in this repository across three languages, and consumers
  in the CLI, the extension SDK, and the job protocol;
- one implementation of validation, reached identically by the CLI's validate
  command, the package-time gate and the SDK's authoring gate.

## Product feature and durable decisions

**No user-facing feature is declared for this module, deliberately.** The user
outcomes it enables — "my language works in Putnami", "my extension's tasks are
cached correctly" — belong to the extensions and CLI commands that deliver them.
A feature per protocol module would create one artificial product identity per
technical boundary, which the [feature protocol's authoring
boundary](../features/README.md) rules out. With no feature to detail there is
no spec, since a spec details exactly one already-authored feature and never
mints one.

Durable decisions (both scope `protocols/extension` explicitly):

- [ADR 0001 — CLI foundation boundaries: catalog, result, engine](../../tooling/cli/doc/adr/0001-cli-foundation-boundaries.md)
- [ADR 0002 — CLI vNext: one contract with four surfaces, and the ratchets that hold it](../../tooling/cli/doc/adr/0002-cli-vnext-contracts.md)
