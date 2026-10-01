# Compatibility and Migration

This document is the **compatibility budget**: for every artifact format a user
can hold on disk, it states the versions this build reads, what migrates, what
is rejected on purpose, and the exact command the CLI tells you to run.

It is the operational half of the
[First Public-Release Contract](../../../README.md#first-public-release-contract).
That section says pre-1.0 compatibility is *migration-based*. This document says
what that means per format, and
[ADR 0010](adr/0010-compatibility-budget.md) records why the numbers are what
they are.

Every number below is pinned by a test. Moving a number without editing this
document fails the build.

## The budget at a glance

| Format | Where it lives | Read window | Outside the window |
|---|---|---|---|
| Lock file | `putnami.lock.json` | 2 – 4 | Older: migrate. Newer: upgrade the CLI. |
| Machine result envelope | `--output=json` / `--output=jsonl`, session files, MCP results | 2 emitted, 1 readable | No downgrade. Pin a pre-removal CLI build. |
| Extension contract stamp | `cliContract` in `putnami.extension.json` | 4 – 5, at least what the manifest's vocabulary requires (5 for agent content) | Re-package the extension, or upgrade the CLI. |
| Support catalog | `putnami.support.json` | exactly 1 | Author the file at version 1. |
| Pre-release version suffix | Published npm/Go versions, image tags, locks, release sets | `<sha>[-<dirty>]` (prior releases) and `<commitTime>-<sha>[-<dirty>]` (current) both read | Nothing to migrate: every reader treats the suffix as an opaque semver identifier; only native `@latest` ordering needs the current shape. |
| Infra requirements | `<project>/infra/requirements.json`, `<workload>/infra/runtime.json` | exactly 1 (currently 2) | Migrate the manifest. See [protocols/infra](../../../protocols/infra/README.md#migrating-a-workspace-from-v1-to-v2). |

Two of these windows are ranges and three are single values. That is a
deliberate split, not an inconsistency:

- a **range** is affordable when the newer version's vocabulary is *additive*
  and the reader can tell which fields it may trust from the version alone;
- an **exact match** is required when the version asserts something about
  behavior the reader cannot verify from the file — an extension's runtime
  halves, or a hand-authored catalog's meaning. Reading those leniently means
  guessing, and a wrong guess is silent.

Infra requirements take the exact match for a third reason: the version marks
which *ownership boundary* the manifest was written under. v1 let a workload
pick its own minimum instance count; v2 made residency deployer-owned. A reader
that accepted both could not tell whether a missing `scaling.min` meant "this
workload wants scale-to-zero" or "this workload predates the question", so the
rejection is the boundary. The cost is that every existing manifest has to be
migrated — and because infra aggregation reports findings as warnings and still
emits a manifest, a manifest nobody migrated loses its requirements without
failing a build. The migration guide is carried in the diagnostic text for
exactly that reason.

## Pre-public compatibility inventory

The first public-release demolition pass applies the same budget to
command and framework APIs. Putnami-owned applications, samples, tests, and
documentation now use the canonical spellings before the old paths are
deleted. Negative tests remain where they prove a removed CLI spelling is
rejected; they are ratchets, not compatibility support.

| Surface | Current producer / consumer evidence | Decision | Owner and removal trigger |
|---|---|---|---|
| CLI `--putnami-version`, `upgrade --local`, and `pin --unpin` | No current public producer or owned invocation; catalog, help, completion, and parser tests use `--version`, `--from-source`, and `--remove`. | **Removed.** | `@putnami/cli`; rejection tests prevent reintroduction. |
| Go client `With*` builder methods and `errors.ToHttpError` | Owned Go tests and docs use `Retry`, `CircuitBreaker`, `Interceptors`, `Transport`, and `ToError`. | **Removed.** | Go framework packages; canonical API tests are the surviving contract. |
| TypeScript runtime cloud re-exports | The cloud package owns the config, secrets, and token-source implementations; `@putnami/runtime` had forwarding exports only. | **Removed.** | `@putnami/runtime`; cloud consumers import `@putnami/cloud/runtime`. |
| TypeScript `health()`, `statics()`, events `devMode`, and HTTP `trustProxy` aliases | All owned samples and the docs site use `platform()`, `staticFiles()`, `simulateDuplicates`, and `trustedProxies`. The unsafe blanket proxy-trust branch had no supported security semantics. | **Removed.** | `@putnami/application` / `@putnami/events`; canonical package tests own the replacements. |
| Go/TypeScript `FeatureEvidenceContributor` metadata writers | Capability evidence is now derived only from concrete plugin registrations, module graphs, package inventory, and authored `putnami.features.json`; no owned workload needs a second metadata-writing API. | **Removed.** Hand-authored contributor identities and provenance overrides no longer enter generated capability manifests. | Go `app` and `@putnami/application`; capability producer tests prove native and authored evidence remain sufficient. |
| Forwarding APIs (TypeScript `grpc.plugin` status helpers, default `ThemeProvider`, `RouteTreeNode`, and extension `RunPreBuildHooks`) | Every owned import/call already reaches the gRPC protocol helper directly, uses the named `ThemeProvider`, consumes `BaseRouteNode`, or calls the hook-kind-aware `RunHooks`. | **Removed.** | `@putnami/application`, `@putnami/ui`, `@putnami/web`, and the TypeScript extension; public-surface and package tests reject reintroduction. |
| Config-extraction compatibility calls (`extractFromFile`, positional `extractConfigSchema`, and `PUTNAMI_CONFIG_SCHEMA_PATH`) | Go and TypeScript extension hooks now receive the project contract and use the configured `configSchema` path or the canonical `schema/config.json` default. No producer sets the environment variable. | **Removed.** | Go/TypeScript extensions and `@putnami/application`; hook and extraction tests cover project-configured and default paths. |
| Extension task `cache.restoreMode` | No in-tree manifest sets it (`TestNoPackageTaskDeclaresARestoreMode` in the Go and TypeScript extensions); published manifests stamped before the field was dropped still carry it, and `ParseManifest` rejects unknown fields. | **Retained and ignored.** Cache eligibility is the task's own declaration, in every command (ADR 0038). | Extension protocol. Remove the field once no pinned manifest declares it; removing it earlier makes a CLI one release ahead of its extensions refuse to load them. |
| Build-result alias and Docker host-flat fallback (`binaryPath` and Docker consumption of `bin/<artifact>`) | Ordinary host builds still write `bin/<artifact>` and emit `binaryPaths`. Docker packaging consumes only the selected `bin/<platform>/<artifact>` cross-compile output; its flat-layout rescue path is gone. | **Removed:** the singular `binaryPath` result and Docker's flat-layout fallback. **Retained:** the ordinary host `bin/<artifact>` output. | `@putnami/go`; build-result and Docker packaging ratchets reject the singular field and packaging fallback, while host-build tests pin the flat output. |
| Extension/site misspellings (`fromStep.path` and `/dl` query `taget`) | Extension manifests use `fromStep` + `output`; download clients send the required `target` query member. No current producer emits either old spelling. | **Removed and rejected.** | Extension protocol / CLI model and `putnami.dev`; schema, strict-reader, and route tests are the rejection boundary. |
| Framework behavioral adapters (rate-limit `get`/`set` stores, binding-only events factories, anonymous `*` HTTP wildcards, database `CodeMigration`, OAuth `retrieveToken`, and PEM-array public keys) | Owned consumers use atomic `consume`, `RegisterBindingTransportFactory`, named `{param...}` catch-alls, current database codes, `getToken`, and JWKS. | **Removed.** | Go and TypeScript framework packages; replacement-focused unit tests and negative syntax/key-shape tests pin the canonical paths. |
| Capability manifest v1/v2 readers and historical fields (`provenance.version`, `sourceBinding`, `packageVersions`, and the complete pre-source-binding scheduler stamp) | CLI doctor/context generation plus Go and TypeScript dependency-manifest mergers still ingest committed manifests produced by earlier framework/scheduler builds. Current producers emit the canonical manifest, package collection, and source binding. A complete earlier scheduler stamp remains recognized during mixed-version builds: Go removes stale output and publishes no v2 manifest, while TypeScript emits a v1 manifest. | **Retain temporarily, read-only.** | Capability protocol, CLI, Go `app`, and `@putnami/application`. Remove only after the minimum supported CLI/framework pair emits source-bound canonical manifests, persisted earlier manifests have an explicit migration/rejection path, and the rolling-version support window has ended. |
| Infra event subscription string/object wire forms | Current Go and TypeScript producers emit a bare topic string for the default pull delivery and an object for explicit stream/push delivery; committed manifests contain both supported forms, and the strict reader normalizes them to one model. | **Canonical behavior, not a deprecated adapter.** Both forms are current protocol-v2 output. | Infra/events protocol owners. Change or remove a form only with a protocol-version bump, migrated committed manifests, cross-language producer updates, and an explicit old-version rejection path. |
| `@putnami/runtime` schema re-export facade (`src/schema`, re-exported through the package root — the package declares no `./schema` subpath export) | Framework packages, samples, templates, and generated code import the schema symbols from the `@putnami/runtime` root entrypoint; it is not an unreferenced forwarding file. | **Retained public API.** | `@putnami/runtime`. Remove only in an approved public-surface change that migrates every owned and generated import to `@putnami/utils`. |
| TypeScript web eager layout/page branches | The SSR generator intentionally imports layouts eagerly so security middleware is registered synchronously; error/not-found registries and direct routes also pass eager page modules. | **Canonical behavior, not compatibility.** | `@putnami/web`; security, loader/action, error, and generator tests own both eager and lazy paths. |
| Go `http.HealthPlugin` and `/_/health` | The Go server template, the simple-api and migration samples, framework docs, and generated capability fixture still mount or describe the single-endpoint plugin. New workloads use `platform` and `/healthz`. | **Retain temporarily.** | Go HTTP/platform owners. Remove after all owned workloads, templates, deployment probes, docs, and generated capability fixtures migrate to `platform`, with `/_/health` either redirected or explicitly rejected. |
| CLI gzip+tar executable payload reader | Earlier pinned CLI releases can resolve to registry payloads packaged as gzip+tar, while current publication uses raw binaries; lock integrity still verifies the extracted executable. | **Retain temporarily, read-only.** | CLI binary launcher/store. Remove when the minimum pinnable release has raw-binary payloads or old pins receive an explicit migration/rejection path. |
| Whole-output cache entries and v1 remote-provider envelopes | Tasks without declared outputs and supported older CLIs still share legacy cache addresses/providers; task-owned entries deliberately do not reinterpret legacy payloads. | **Retain temporarily, isolated from the new format.** | CLI scheduler/store/cache-provider. Remove when every supported task declares outputs, every supported provider speaks the current envelope, and legacy addresses are guaranteed miss-only. |
| Persisted security readers (hex session cookies) | Existing browser sessions survive process upgrades. Cookie verification remains fail-closed; no current writer emits the historical form. | **Retain temporarily, read-only and security-gated.** | TypeScript session owner. Remove after the maximum cookie lifetime has elapsed. |
| Deploy/config binding environment readers and workspace-managed toolchains | Current deploy outputs can still provide database/storage bindings through environment documents, and existing workspaces can point at previously installed toolchain locations. | **Retain temporarily.** | Go database/storage and CLI/toolchain owners. Remove after every deploy target emits canonical config sections, recovered deployment fixtures pass without environment transport, and current workspaces have migrated their toolchain records. |
| OAuth `callbackRoute ?? loginRoute` single-handler form | The single-route form is still documented and tested; it is a defaulted configuration shape, not an unused export. | **Retain temporarily.** | `@putnami/application/oauth`. Remove only after owned configurations explicitly set `callbackRoute` and a release migration rejects the omitted member. |
| Go database `PoolConfig.Database` / `Schemas` fields | Current DSN construction tests and owned database samples still exercise these fields alongside the canonical datasource document. | **Retain temporarily.** | Go database owner. Remove the pair together after samples, tests, and deployment binding semantics use `Datasource` exclusively. |
| TypeScript client-generator standalone/Proto discovery | Direct `postGenerate()` tests still discover committed or `.gen` OpenAPI files without a `GenerateResult`; Proto is consulted only when OpenAPI is absent. | **Retain temporarily, OpenAPI-first.** | `@putnami/client`. Remove when every caller supplies generated OpenAPI asset metadata and Proto/Connect either joins a versioned client-generation contract or is explicitly rejected. |
| TypeScript extension `workspace-builder-image` parameter | The shared package command can forward the Go-only parameter to the TypeScript extension, which currently accepts and ignores it instead of failing a mixed-language package invocation. | **Retain temporarily as an explicit no-op.** | TypeScript extension/CLI command catalog. Remove when package options are specialized per extension runtime or TypeScript implements the workspace-builder path. |
| TypeScript extension hook defaults (`order: 0` and statusless config-extract summaries) | Installed dependency manifests can omit hook order, and hooks from before the config-extract status convention emit a successful summary without `status`. The reader never invents a positive status for an explicit `empty` or `skipped` result. | **Retain temporarily, read-only.** | TypeScript extension hook runner. Remove after the minimum supported dependency manifest writes `order` and every supported config-extract hook emits an explicit status, with prior fixtures rejected or migrated. |
| TypeScript job runtime-events protocol v1 (`PUTNAMI_RUNTIME_EVENTS` negotiation) | `JobEventEmitter` in `@putnami/runtime` stamps the version the invoking CLI advertises; an absent, unparsable or lower advertisement yields v1, which a workspace pinned to an older CLI still receives. The resolution mirrors `protocols/runtime/negotiation.go`, and `typescript/framework/runtime/test/jobs/events.test.ts` pins it. | **Retained by protocol negotiation.** | `@putnami/runtime` with the runtime protocol owners. Retire v1 in one cross-language change, once the minimum pinnable CLI advertises v2 and prior-release fixtures prove the rejection. |
| TypeScript client credential source alias `forwarded-user-token` | `CredentialSourceKind` still accepts the source spelling that predates Go's `forwarded-user` and marks it `@deprecated`. This is the binding's source kind only: the client-contract profile kind `forwarded-user-token` (`protocols/clientcontract`) is canonical and unaffected. | **Retain temporarily.** | `@putnami/client`. Remove after a release rejects the alias with a message that names `forwarded-user`. |
| `@putnami/application` generate asset keys outside `options.generate.assets` (`options["@putnami/application:generate"].assets`, package.json `putnami.application.assets` and `putnami.build.assets`) | `bin/generate.ts` reads them after the canonical key. No owned project writes them, and the documentation describes only `options.generate.assets`. | **Retain temporarily, read-only.** | `@putnami/application`. Remove with the extension-local `.putnamirc.json` readers, once a recovered prior-workspace fixture proves the migration or an explicit rejection. |
| `@putnami/web` flat React block in package.json `putnami` | The web generate hook (`bin/generate-config.ts`, `resolveGenerationConfig`) still reads the flat React keys from package.json `putnami`, while project `putnami.json` requires the nested `options["@putnami/web:generate"].react` shape and rejects misplaced keys. `typescript/framework/web/test/ssr/generate-config.test.ts` pins both. | **Retain temporarily, read-only.** | `@putnami/web`. Remove with the other package.json configuration readers, once a recovered prior-workspace fixture proves the migration or an explicit rejection. |
| `putnami.dev` legacy framework-doc redirects | Public requests under `/docs/framework/*` are redirected to the current `/docs/frameworks/{typescript,go}/*` routes and are pinned by route tests. | **Retain temporarily.** | Documentation site. Remove after the published redirect support window expires and route analytics show no supported inbound links. |
| Extension-local `.putnamirc.json` and workspace `projects` readers | Go install/upgrade shell jobs, Go/TypeScript project config helpers, and the experimental Python workspace sync still read authored pre-canonical workspaces directly instead of receiving fully resolved job context. | **Retain temporarily.** These readers protect persisted workspace inputs, and deleting them independently would make extension behavior disagree. | `@putnami/go`, `@putnami/typescript`, and experimental `@putnami/python`. Remove as one change when every extension consumer receives canonical `workspaceProjects` / `putnami*.json` data and a recovered prior-workspace fixture proves the migration or explicit rejection path. |
| Lock v2–v4, result v1 reads, extension contract 4 with the additive contract 5, and support catalog 1 | Persisted artifacts and prior-release fixtures described below exercise the readers or rejection boundaries. | **Retained by budget.** | Protocol and CLI owners below; change only with a versioned migration and prior-release evidence. |
| Python extension | Explicitly installed in this workspace but marked experimental and omitted from the supported public core. | **Canonical experimental behavior.** No Go/TypeScript parity promise. | `@putnami/python`; remove from the workspace only through an explicit maturity decision. |

A removal migrates every owned consumer in the same change. It never
renumbers earned wire, lock, extension-contract, or cache identifiers.

## Lock file — `putnami.lock.json`

This CLI accepts lock format versions 2 – 4 and writes new locks at version 4.

| Version | Added |
|---|---|
| 1 | Resolved versions, archive integrity digests, manifest hashes, source URLs. **No longer read.** |
| 2 | `taskContract` per entry — the pinned extension's task-contract level, readable without resolving the manifest. |
| 3 | The `toolchains` section, and `cli.protocolVersion` — the machine-output protocol the pinned CLI emits. |
| 4 | The `agentArtifacts` section — platform-independent pins of separately declared agent workflows. No current command adds one; `putnami migrate agent-content` removes them and its rollback restores them. |

### What migrates

- **v1, v2, or v3 → v4** is the migration a user runs by hand:
  `putnami migrate vnext --apply`. It is mechanical and idempotent. For v1 it
  records the task contract each *installed* manifest already declares; for
  v2/v3 it preserves the existing vocabulary and initializes the v4
  `agentArtifacts` map. It never resolves a version, contacts a registry, or
  changes a pin. An extension that is not installed is *reported*, not guessed
  at. `putnami migrate vnext` (without `--apply`) is the CI check: exit 0 when
  clean, exit 2 when a migration is pending.
- Ordinary reads, writes, and artifact installs preserve the version the
  workspace committed, so no unrelated command promotes the lock as a side
  effect.

### What a migration must not lose

A lock is a **security artifact**: the per-platform SHA-256 digests under
`integrities` (and `cli.integrities`) are the only thing standing between a
download and execution, and the CLI verifies them fail-closed. Every migration
therefore carries every recorded platform's digest through unchanged. Regenerating
the map from the machine running the migration would silently drop the digests
for every other platform your team and CI use, and the failure would not surface
until someone on that platform ran a command.

One field is deliberately *not* carried: the legacy scalar `integrity` on an
extension entry is dropped when that entry also has an `integrities` map,
because the scalar only ever matched the platform that generated it. An entry
that has only the scalar keeps it — it is that entry's sole verification datum
until a verified install supplies the map.

### What is rejected

| Your lock | What happens | What you run |
|---|---|---|
| version 1, or no `version` member | `putnami.lock.json is lock format version 1, but this putnami requires version 2` | `putnami migrate vnext --apply` |
| version 5 or higher | `putnami.lock.json is lock format version 5, but this putnami reads at most version 4` | `putnami upgrade` |

Refusal never rewrites the file. A reader that silently upgraded a lock would
convert your workspace as a side effect of an unrelated command, and a reader
that treated a v1 lock as v2 would have to invent the one thing v2 adds.

`putnami migrate vnext` is the single reader allowed below the floor — it is
what moves a workspace onto the floor, so it is also exempt from the
workspace-pin relaunch (relaunching would read the lock it exists to repair).

## Machine result envelope

Every machine document this CLI emits carries machine result protocol version 2.
Version 1 documents carry **no** `protocolVersion` member, and that absence is
the only way to tell the two apart — a consumer that must open both branches on
`typeof doc.protocolVersion === 'number'` and needs nothing else.

Version 1 is **read-only**. Its Go type and JSON schema are retained so a
consumer can decode documents recorded by CLI builds published before the
removal; the v1 constructor and writer are deleted, so nothing in this
repository can produce one. See
[`protocols/cli/doc/02-result-v2.md`](../../../protocols/cli/doc/02-result-v2.md)
§ Migrating from version 1 for the field-by-field mapping.

**The rollback is a version pin, not a flag.** There is no environment variable
or option that re-enables the v1 wire; `PUTNAMI_MACHINE_OUTPUT` is inert and
selects nothing. A consumer that cannot move yet pins a pre-removal CLI build
with `putnami pin <version>`, which is exact, honored by the launcher, and ages
out on its own. A second machine renderer is a rejected design, not a missing
feature.

## Extension contract stamp — `cliContract`

This CLI implements CLI ↔ extension contract 4 and the additive contract 5,
which covers the extension `agentContent` contribution. It loads a manifest
stamped at least at the contract its vocabulary requires — 4, or 5 for a
manifest that declares agent content — and at most at 5. The stamp is *earned*
at package time — the packager validates the staged manifest strictly and
stamps the lowest contract that covers it — so it is evidence, never a claim.

| Manifest stamp | What happens | What you run |
|---|---|---|
| absent, or below what the manifest requires | `declares CLI contract 0 but this putnami requires 4` (or `requires 5 for its agent-content contribution`) | re-package the extension, or `putnami extensions update` |
| from the required contract to 5 | Loads, validated strictly. | — |
| above 5 | `extension requires a newer putnami` | `putnami upgrade` |

Contract 5 is additive: only a manifest that declares `agentContent` is stamped
5, so every other extension keeps contract 4 and keeps loading in a contract-4
CLI. A contract-4 CLI refuses a contract-5 package with `extension requires a
newer putnami`; its permissive decode would otherwise drop the agent content
silently. See the protocol's [agent-content ADR](../../../protocols/extension/doc/adr/0006-agent-content-is-an-additive-contract.md).

One manifest is outside the ladder: a manifest declaring **no contract surface**
— no commands, no command groups, no tools, for example a framework package that
ships only a `preBuild` hook. The packager deliberately leaves it unstamped
because there is nothing for the contract to govern, so the loader must not
demand a stamp. Loader and packager share one predicate
(`extension.DeclaresContractSurface`), and the exemption is evaluated *after*
the higher-contract arm, so a hook-only manifest from the future is still
rejected.

Contract 4 adds `sessionPrerequisites`, whose selection, local parameters and
verification gates alter the execution DAG. A contract-3 reader would ignore
that field and could run the dependent command without its gates, so there is
no compatibility adapter: re-package authored extensions, or run
`putnami extensions update` for installed ones.

Contract 3 retired the older rule that every increment ships adaptation for
contract N-1. Adaptation loaded an extension after deleting the part of its
manifest the CLI disagreed with, which is safe only while the disagreement is
confined to flag surface — and contract 3 asserts things about an extension's
runtime halves that no adapter can verify. An increment now ships a
**migration** instead: a command that moves a workspace or an extension onto the
new contract. See [ADR 0002](adr/0002-cli-vnext-contracts.md) §1 and
[ADR 0010](adr/0010-compatibility-budget.md).

## Support catalog — `putnami.support.json`

A support catalog is accepted only at support catalog protocol version 1. The
parser rejects any other `protocolVersion` token — including `"1"`, `1.0`, and
`01` — with the diagnostic `support.invalid_protocol_version`.

There is no migration and no read window, because the file is **authored**, not
generated: it is a reviewed statement about public support commitments, and
mechanically rewriting one would change what the project promises. If a future
version exists, you edit the file. See
[`protocols/support/README.md`](../../../protocols/support/README.md).

## If your artifact is outside the window

Work down this list; each row is self-contained.

1. **"lock format version 1, but this putnami requires version 2"** — run
   `putnami migrate vnext` to see the pending changes, then
   `putnami migrate vnext --apply`. Commit the result. If the report lists
   extensions under *Not recorded*, run `putnami install` first so their
   manifests are on disk, then re-run `--apply`; the migration records what an
   installed manifest declares rather than guessing.
2. **"lock format version N, but this putnami reads at most version M"** — the
   lock was written by a newer CLI than the one you are running. Run
   `putnami upgrade`. If a workspace pin is what is blocking you, prefix the
   command: `PUTNAMI_NO_RELAUNCH=1 putnami upgrade`.
3. **"declares CLI contract N but this putnami requires M"** — the extension
   predates this CLI. Run `putnami extensions update` to pull a build stamped at
   the current contract. If you author the extension, re-package it; the
   packager stamps the current contract on a successful strict validation.
4. **"extension requires a newer putnami"** — the extension is ahead of your
   CLI. Run `putnami upgrade`, or pin the extension back to a version your CLI
   loads.
5. **`support.invalid_protocol_version`** — edit `putnami.support.json` so
   `protocolVersion` is the bare integer `1`.
6. **A machine consumer that only parses version 1** — either branch on
   `protocolVersion` (recommended; the mapping table is in
   `protocols/cli/doc/02-result-v2.md`), or pin the CLI to a pre-removal build
   with `putnami pin <version>` while you migrate.

### Rolling back

Rollback is always a **version pin**, never a compatibility flag:

```bash
putnami pin 1.2.3        # run exactly this CLI in this workspace
putnami pin --remove     # drop the pin
```

`putnami pin` is never relaunched, so it works even when the pinned engine is
broken. The pin records the binary's SHA-256 per `os/arch` and is verified
fail-closed on every launch — see
[Version Management](14-version-management.md#pinning-the-cli-per-workspace).

Two rollbacks are **not** available, and both absences are decisions:

- there is no way to make this CLI read a v1 lock; the migration is the way
  forward, and it is idempotent, so re-running it costs nothing;
- there is no way to make this CLI emit a v1 machine document.

## Agent artifacts superseded by extension content

Agent content is installed only by an extension, through an
`extension:<name>` entry in `agentArtifacts`. The separately declared forms
earlier releases installed — `agentArtifacts` entries with a lock pin
(`name`, `name:channel`) and in-tree `/path` entries — are no longer
installed, and the compatibility window for them has ended:

- **An unmigrated workspace** fails every agent-content command (`install`,
  `upgrade`, `context generate`, the first-run and session-start passes) with
  nothing written. The error names `putnami migrate agent-content <extension>`
  and the extension whose content supersedes the artifact. A workspace that
  cannot migrate yet stays on a CLI release before this one, which installs
  its pins exactly as before.
- **The migration** is `putnami migrate agent-content <extension> --apply`. It
  is the one command that still reads the old declarations, lock pins and
  ownership records, and it only moves them. It resolves and downloads
  nothing: the extension must already be installed at a pinned release, or be
  declared by path. An extension release that declares no `supersedes` offers
  no migration, and the command says so.
- **A clone that pulled a migration** still holds its own records of the old
  artifacts. Every command refuses the opt-in there until the same `--apply`
  moves them; it keeps every file you edited.
- **Rollback** is `putnami migrate agent-content <extension> --rollback`. It
  restores the previous declarations, pins, ownership records and files byte
  for byte, in the clone that ran the migration, until something the
  migration moved changes. The restored state is the separately declared one:
  this CLI refuses it again, and a release before this one installs it.
- **Older CLIs** fail on a migrated workspace instead of skipping its content.
  A CLI before contract 5 refuses the extension package with `extension
  requires a newer putnami`. A CLI that predates the `extension:` opt-in reads
  it as an agent artifact named `extension`, which no lock pins, and its
  install fails with nothing written: `agent workflows install: extension:
  workspace lock pins no agent artifact: putnami.lock.json records no
  extension entry`. The remedy for both is a CLI release that carries the
  opt-in; see the
  [migration guide](18-agent-workflows.md#migrating-separate-artifacts-to-an-extensions-content).

See [ADR 0047](adr/0047-extension-owned-agent-content.md),
[ADR 0048](adr/0048-migrating-agent-artifacts-to-extension-content.md) and
the [migration guide](18-agent-workflows.md#migrating-separate-artifacts-to-an-extensions-content).

## Evidence

| Claim | Where it is proven |
|---|---|
| Lock window, rejection types, remedy strings | `tooling/cli/internal/lockfile/prior_release_test.go`, `lockfile_v2_test.go` |
| Migration preserves every per-platform digest | `tooling/cli/internal/lockfile/prior_release_test.go` |
| Prior-release lock bytes are immutable and match their recorded SHA-256 | `tooling/cli/internal/lockfile/testdata/prior-releases/` |
| `cliContract` ladder × surface cross product | `protocols/extension/manifest_matrix_test.go` |
| `cliContract` rejection against a genuine pre-registry manifest | `protocols/extension/prior_release_test.go` |
| Result v1 is readable and unwritable | `protocols/cli/result_test.go`, `tooling/cli/internal/cli/v1_bridge_ratchet_test.go` |
| Support catalog exact-version rejection | `protocols/support/support_test.go` |
| Superseded artifacts install until migrated; the migration resumes and rolls back | `tooling/cli/internal/commands/lifecycle/agent_workflows_lifecycle_test.go` (`TestAgentContentMigration_*`) |
| A CLI that predates the `extension:` opt-in fails closed on a migrated workspace | `protocols/workspace/agent_content_opt_in_test.go` (`TestAgentContentOptIn_OlderReaderFailsClosed`) |

The lock fixtures are **recovered bytes**, not regenerated ones: each is a
`putnami.lock.json` as committed at the commit that shipped its format version,
with its source commit and SHA-256 recorded beside it. Regenerating an old
fixture with today's writer would make the reader tests pass by construction.
