# Infrastructure Requirements Protocol

## Why

Every deployment target — `putnami/cloud`, Pulumi, on-prem k8s, anything that comes next — needs to know what infrastructure a workload requires: databases, event topics, object storage, secrets, scheduled jobs, and workload-level runtime concerns (ingress, scaling).

Today each target reinvents the manifest, and workload owners have to restate the same requirements per deployer. The application code already knows what it needs (it opens a Postgres connection, fetches an events topic, reads a bucket); the deployer should consume that knowledge, not duplicate it.

This protocol fixes the shared contract so:

- **producers** (Putnami generators) declare semantic needs locally, next to the code that uses them, and sync a committed per-project contract,
- **humans** declare workload runtime intent separately, in a file that generators preserve,
- **consumers** (deployers) read one ephemeral aggregated artifact per workload, with full provenance,
- **drift** between code and infrastructure declarations is impossible because the aggregator walks the workload's dependency graph at build time.

The protocol is deliberately **library-first**: a feature library declares its own needs, the workload imports the library, and `putnami build` emits the aggregated manifest. Workload-level concerns that can't be derived from libraries (ingress, scaling, domain, service/job shape) live in workload-only runtime intent.

## What

This package defines the versioned contract for these shapes:

| Shape | Path | Producer | Consumer |
| ----- | ---- | -------- | -------- |
| Generated project requirements | `<project>/infra/requirements.json` | Putnami language/framework generators | deploy activation, the build aggregator |
| Runtime intent | `<workload>/infra/runtime.json` | workload owner | deploy activation, the build aggregator |
| Per-producer scratch fragment | `<project>/.gen/infra/<slug>.json` | framework `generate()`/`describe()` hooks, one slug per producer | language generator sync only |
| Workload runtime defaults | `<workload>/.gen/infra/runtime.json` | the build aggregator (`infra.DefaultRuntime()`) | the build aggregator |
| Workload overrides | `<workload>/infra/overrides.json` | developer | the build aggregator |
| Aggregated manifest | `<workload>/.gen/requirements.json` | the build aggregator | deployers |
| Deployment declaration | `<workload>/.gen/deployment.json` | the `deployment` package step | release-set publishers, deployers |

`infra/requirements.json` and `infra/runtime.json` are committed deployability
markers. `.gen/requirements.json` is never committed: it is a release/build
artifact produced from committed requirements across the workload dependency
graph, then runtime intent, then final validation/overrides.

The v2 resource kinds are:

- `databases` — `{ name, engine ∈ {postgres, mysql, sqlite, firestore}, schemas? }`
- `events` — `{ publishes: [topic], subscribes: [topic | { topic, delivery ∈ {pull, stream, push} }] }` — a subscribe is a bare topic string (delivery `pull`, the default) or an object carrying a non-default delivery; `push` tells the deployer to provision a provider push subscription. The bare-string default keeps pull/stream workloads byte-for-byte unchanged.
- `storage` — `{ name, access? ∈ {read, write, readwrite}, public?, retention? }` — the deployer-facing projection of [`go.putnami.dev/protocol/storage`](../storage); `retention` is a free-form deployer-defined string
- `secrets` — `[secret-name]`
- `scheduledJobs` — `{ name, schedule (5-field Cloud Scheduler cron), entrypoint? }`
- `runtime` — workload-only — `{ ingress?: { domain?, public? }, scaling?: { max?, concurrency? }, protocols?: { http2? } }`

Minimum residency and billing/CPU-allocation posture are deliberately absent.
They are deployer-owned cost policy, not workload intent. A strict parser rejects
`scaling.min`, `billing`, `requestBased`, `cpuIdle`, and any equivalent unknown
property rather than allowing a workload to impose fixed infrastructure cost.

The runtime block is rejected by strict-parsing per-project manifests (it carries the typed error code `infra.runtime_in_library`).

`runtime.security.eventsGateway`, when true, declares that the workload acts as an event pull/stream gateway and requires the deployer to grant its runtime identity event subscriber access.

### Migrating a workspace from v1 to v2

Protocol v2 removed `runtime.scaling.min`. Readers accept `protocolVersion: 2`
only, so every manifest written before the bump has to move:

1. **Committed `<project>/infra/requirements.json`** — set `"protocolVersion": 2`.
   Framework-generated v1 fragments and committed manifests are accepted at a
   narrow compatibility boundary and normalized by the next `putnami build`;
   strict protocol readers and hand-authored manifests are not relaxed.
2. **Authored `<workload>/infra/runtime.json`** — delete `scaling.min`. This
   file carries no `protocolVersion` and is never regenerated, so it is the one
   a build cannot fix for you.

Get this wrong and the build still succeeds. Aggregation findings are warnings
by contract (see `infraagg.Job`), so a manifest the reader cannot parse
contributes nothing and the workload's `.gen/requirements.json` is emitted
*without* it — no database, no secret, or, when it is `runtime.json` that fails,
no ingress domain and no security settings. Read the `infra requirements:`
warnings a build prints before shipping an upgrade; both diagnostics name the
file and the fix.

## How

Tooling consumes manifests through three entry points:

- `ParsePerProjectManifest(data) → *PerProjectManifest, []Diagnostic` — strict parse (unknown fields rejected).
- `ParseAggregatedManifest(data) → *AggregatedManifest, []Diagnostic` — strict parse for the build-emitted artifact.
- `Merge(workload, contributions) → AggregatedManifest, []Diagnostic` — pure aggregation of per-project contributions into the workload's aggregated manifest. Output is byte-deterministic.
- `ApplyOverrides(manifest, overrides) → AggregatedManifest, []Diagnostic` — drops requirements named in the workload's `overrides.json` from an already-merged manifest (see [Overrides](#overrides-suppression)).

Convenience wrappers `ParseAndValidate*` and `Load*` chain parse + validate + file read (including `ParseOverrides` / `LoadOverrides`).

`ContributorID` identifies who produced an entry. Current generated committed requirements are attributed as `framework:requirements`; legacy/manual manifests may still appear as `manual`, and framework scratch fragments use `framework:<slug>` internally before they are folded into `infra/requirements.json`.

### Deriving database requirements from the database protocol

The `databases` block is a thin projection of the canonical [`go.putnami.dev/protocol/database`](../database) `RequirementManifest`. `DatabasesFromManifest(*database.RequirementManifest) → []Database, []Diagnostic` is the single bridge: it flattens the manifest with `RequirementManifest.Project()` — the secret-free `{name, engine, schemas}` triplet, sorted by datasource name — and maps each entry onto an infra `Database`, translating the database-protocol engine into the infra engine enum (postgres is the only mapping today; an unrecognized engine yields an `infra.invalid_engine` diagnostic and drops the entry).

Because a `RequirementManifest` has no `connection` field at all (strict parsing rejects one), no DSN, host, user, password, ssl flag, driver param, or test policy can ever reach an infra requirement through this path — infra stays the deployer-facing projection while the database protocol owns the rich, secret-bearing shapes.

### Generated project requirements example

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-infra.json",
  "protocolVersion": 2,
  "databases": [
    { "name": "primary", "engine": "postgres", "schemas": ["iam"] }
  ],
  "events": {
    "publishes": ["workspace.deploy.completed"],
    "subscribes": []
  },
  "secrets": ["jwks_signing_key"]
}
```

### Aggregated release artifact example

`workload` is the project ID, the workspace-relative path the release set
names: the canonical project id without its leading slash. It is not the
project name, and it drops a grouping folder such as `(internal)/`. A deployer
that matches the declaration to its release-set member compares the two values
directly. Each `sources[].project` uses the same form.

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-infra.json",
  "protocolVersion": 2,
  "workload": "services/api",
  "databases": [
    {
      "name": "primary", "engine": "postgres", "schemas": ["audit", "iam"],
      "sources": [
        { "project": "libs/audit", "contributor": "framework:requirements" },
        { "project": "libs/iam",   "contributor": "framework:requirements" }
      ]
    }
  ],
  "runtime": {
    "ingress": { "domain": "api.example.com", "public": true },
    "scaling": { "max": 100, "concurrency": 80 },
    "protocols": { "http2": false }
  }
}
```

### Merge identity rules

| Kind | Merge identity | Conflict handling |
| ---- | -------------- | ----------------- |
| databases | `(name, engine)` | `schemas` form a sorted-union set (additive — never a conflict) |
| events.publishes / subscribes | `name` | dedup as a set |
| storage | `name` | `access` and `public` are additive capabilities — the grant is the union of what each contributor needs, so a shared bucket is never under-granted. `retention` is different: empty means "unspecified, fill in if anyone declares one" and the first non-empty value wins additively, while two different non-empty values are a conflict resolved by contributor precedence (see below) and raise `infra.conflicting_value` |
| secrets | `name` | dedup as a set |
| scheduledJobs | `name` | conflicting `schedule` or `entrypoint` resolves by contributor precedence (see below) and raises `infra.conflicting_value` |

Source lists are always sorted by `(project, contributor)` and deduped on the same key.

**Contributor precedence.** When two contributors disagree on a scalar value (`storage.retention`, `scheduledJobs.schedule`/`entrypoint`), legacy developer-authored `manual` declarations win over `framework:*` generators. Current workloads should prefer `infra/overrides.json` for human suppression and `infra/runtime.json` for human runtime intent. Same-rank disagreements are unresolved conflicts: the first entry by sorted `(project, contributor)` is kept and reported as an **error**-severity `infra.conflicting_value`.

### Overrides (suppression)

Precedence lets a workload owner *replace* a value, but not *remove* a requirement a library or framework generator declared. A workload-only overrides file at `<workload>/infra/overrides.json` closes that gap:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-infra-overrides.json",
  "ignore": {
    "databases": [{ "name": "primary", "engine": "postgres" }],
    "storage": ["legacy-assets"],
    "secrets": ["unused_key"],
    "events": { "publishes": ["deprecated.topic"] },
    "scheduledJobs": ["nightly-rollup"]
  }
}
```

`ApplyOverrides` runs on the **already-merged** manifest and drops every entry that matches an `ignore` rule by merge identity (databases by `(name, engine)`, everything else by name). An ignore rule that matches nothing is reported as a **warning**-severity `infra.unused_override` so stale rules stay visible. The `runtime` block is not suppressible.

Like `runtime.json`, overrides are a workload-root concern and never appear in a library's per-project manifest.

### Merge Order

`putnami build` merges infrastructure in this order:

1. Committed generated requirements from the workload project and every in-workspace dependency: `<project>/infra/requirements.json`.
2. Human runtime intent from the workload root: `<workload>/infra/runtime.json` (or generated defaults when no runtime file exists).
3. The final ephemeral release artifact: `<workload>/.gen/requirements.json`.

Deploy activation must use the committed markers in `infra/` (`runtime.json`
and/or `requirements.json`). It must not use `.gen/requirements.json`, because
`.gen` may be absent in a clean checkout and is regenerated by build/publish.

### Workload runtime defaults

Every workload's aggregated manifest carries a `runtime` block, even when the developer never authored `<workload>/infra/runtime.json`. The build aggregator synthesizes defaults via `infra.DefaultRuntime()` and writes a visible sidecar at `<workload>/.gen/infra/runtime.json` so operators can see the values their workload will run under. The current defaults (tuned for Putnami's serverless-first target) are:

```json
{
  "ingress": { "public": false },
  "scaling": { "max": 1, "concurrency": 500 }
}
```

Resolution is developer-first: when `<workload>/infra/runtime.json` exists, it wins, and an aggregator run removes the defaults sidecar at `<workload>/.gen/infra/runtime.json` so it does not compete with the authored file. A cache hit of the build step that aggregates restores no sidecar and removes none, so a sidecar an earlier build left stays until the aggregator runs again; the runtime block of `<workload>/.gen/requirements.json` is the value that applies. The defaults sidecar is re-emitted on every build, so changes to `infra.DefaultRuntime()` propagate without any developer action.

Language runtime compatibility defaults may further specialize the runtime block
before it is emitted. TypeScript/Bun workloads set `runtime.protocols.http2` to
`false` because deploy-target HTTP/2-to-container commonly means h2c, which the
TypeScript runtime does not serve today. That statement is made by the
TypeScript extension itself (`cmd/putnami-ts/build_infra.go`); the CLI does not
recognize a TypeScript project from its tags and does not specialize the runtime
block on any language's behalf.

The rule that human intent is never overwritten by a generator — this defaults
sidecar, `runtime.json`, and the overrides file below — is recorded in
[`doc/adr/0002-human-intent-survives-every-generation.md`](doc/adr/0002-human-intent-survives-every-generation.md).

### Who aggregates

The "build aggregator" named throughout this document is a task in each
language's `build` pipeline (`build~infra`), implemented once in
`go.putnami.dev/sdk/extension/infraagg` and registered by the Go and TypeScript
extensions. The orchestrator supplies the resolved facts the aggregation needs —
the project's type and its in-workspace dependency closure, on the job context
(`protocols/job`: `project.type`, `project.dependencyClosure`) — and schedules
the task after the steps that make the workload's committed requirements
current. It decides nothing about the artifact's content.

The task is **cached**. Its key holds every file it reads: the committed
`infra/requirements.json` of every project in the dependency closure, with that
project's path, and the workload's `infra/runtime.json` and
`infra/overrides.json`. A change to a library's requirements, or a project that
joins or leaves the closure, moves the key. The aggregated manifest is the
task's required output: a run that writes none (a library, a workload that
declares nothing, a failed write) caches nothing, so a cache hit never replays a
manifest the current files do not produce. It is also **workload-only**: a
project whose resolved type is not `application` is skipped, because a library
is consumed, never deployed.

### Deployment declaration

A workload's deployment declaration is its aggregated manifest in the one byte
form a content-addressed registry stores. A release set can carry it as a member
of kind `deployment`, so a deployer that holds only the release set reads what
the workload needs to run without a build in the loop.

- **Content.** The aggregated manifest exactly as the build aggregator resolves
  it: every closure contribution with its sources, the workload's overrides
  applied, and the runtime from `infra/runtime.json` or `infra.DefaultRuntime()`
  after the language compatibility hook.
- **Bytes.** `infra.MarshalDeployment` writes what `encoding/json` writes for the
  typed manifest: members in struct field order, no insignificant space, `<`,
  `>` and `&` escaped, no trailing newline, and no `$schema` member. `$schema` is
  an editor hint; carrying it would tie every digest to a URL. These bytes are
  also the canonical payload form of the Put registry, so they publish as they
  are.
- **Reader.** `infra.ParseDeployment` strict-parses and validates the bytes as an
  aggregated manifest, then refuses any spelling that is not the canonical one:
  indented, reordered or newline-terminated bytes, a repeated member, or a
  `$schema` member are `infra.non_canonical`. An unknown member is
  `infra.unknown_field`, as for every strict reader.
- **Version.** The payload carries `protocolVersion`, and the Put media type
  `application/vnd.putnami.infra.deployment.v2+json` names it. A new protocol
  version is a new media type.
- **Producer.** The `deployment` package step of the Go and TypeScript
  extensions (`package-deployment`) writes `<workload>/.gen/deployment.json`.
  The step is off until a project turns on the `deployment` package channel
  (the `--deployment` flag, a `deployment` entry in its `publish` list, or the
  `deployment` option of its language extension). Like
  `build~infra`, it is **cached**: its key covers `infra/requirements.json` of
  every project in the dependency closure and the workload's `infra/runtime.json`
  and `infra/overrides.json`, so a change to a library's requirements moves the
  key. A library writes no declaration and loses a stale one. An aggregate with
  an error finding writes nothing, removes a stale declaration, and reports its
  findings as warnings, as `build~infra` does: a publisher that needs the
  declaration refuses its absence.

### Generator scratch fragments

Framework producers each own one scratch file at `<project>/.gen/infra/<slug>.json`. The slug names the producer — `database.json`, `storage.json`, `events.json`, `migration.json`, `document.json`, `secrets.json`. Language generators clear this scratch directory at the start of generation, producers rewrite their fragments, then the generator folds every fragment into the committed `<project>/infra/requirements.json`.

Per-producer files eliminate write contention: two producers in the same project never share a file, so no read-modify-write merge is needed at write time. The build aggregator does not read these scratch fragments directly.

**Contract for `generate()`/`describe()` hook authors:** the scratch fragment **must be written atomically** (temp file + rename). The same generate task runs for both the `build` and `test` pipelines and may execute concurrently; an atomic rename guarantees the sync step always observes a complete file and never a torn write.

Go producers call `infra.WriteSidecar(projectRoot, slug, manifest)`, which atomically writes the file and removes it when the manifest declares nothing. `infra.RemoveSidecar(projectRoot, slug)` clears a stale sidecar explicitly.

A producer running in the framework's **describe phase** does not get the project root — it gets the workload's generated-artifact directory (`<project>/.gen`) as `app.DescribeContext.OutputDir`. Those producers call the directory-relative `infra.WriteSidecarIn(outputDir, slug, manifest)` / `infra.RemoveSidecarIn(outputDir, slug)` (and `infra.SidecarPathIn` to locate it). Passing `OutputDir` to the project-root `WriteSidecar` would nest the file at `<project>/.gen/.gen/infra/<slug>.json`, one `.gen` too deep, where the sync step never finds it.

The Go and TypeScript build generators call `infra.SyncGeneratedRequirements`
after their producers run. That sync writes deterministic JSON to
`infra/requirements.json`, or removes a stale committed file when no producers
declare requirements. It never touches `infra/runtime.json`.

`SyncGeneratedRequirements` renders the committed file to match the repo's JSON
formatter (Biome, `expand: "auto"`) instead of `encoding/json`'s `MarshalIndent`
(the decision and its alternatives are in
[`doc/adr/0001-committed-requirements-are-a-formatter-fixed-point.md`](doc/adr/0001-committed-requirements-are-a-formatter-fixed-point.md)):
objects and arrays-of-objects stay expanded one element per line, but a scalar
array (`schemas`, `secrets`, an events topic list) is collapsed onto a single
line when it fits the 120-column print width and expanded otherwise. That makes
the file a fixed point — `putnami lint` never reflows it and the generator never
re-churns it, so it changes only when a requirement changes, and no per-file
formatter override is needed. The collapse rule lives in `marshalCommitted`
(committed_format.go); its width constant must track Biome's
`json.formatter.lineWidth`. The scratch fragments under `.gen/infra` stay in
`MarshalIndent` form — they are ephemeral, never committed, and never formatted.

### Error taxonomy

| Code | When |
| ---- | ---- |
| `infra.parse_error` | JSON-level parse failure or nil manifest. |
| `infra.unknown_field` | Strict parse rejected an unknown field (per-project or aggregated). A field this protocol retired (`RemovedRuntimeFields`) is named as retired, with the reason and the migration, rather than reported as a typo. |
| `infra.invalid_name` | Resource name violates the canonical regex `^[a-z0-9][a-z0-9_./-]{0,63}$`. |
| `infra.invalid_engine` | `databases[].engine` is not an accepted engine. The message lists the accepted set, built from `ValidEngines` so it cannot drift. |
| `infra.invalid_protocol_version` | Manifest declares a `protocolVersion` this parser does not support. A manifest left at an *earlier* version gets the migration steps in the message, because the reader drops the contribution and the aggregator only warns. |
| `infra.invalid_contributor` | `Source.Contributor` is not `manual` or `framework:<name>`, or `Source.Project` is empty. |
| `infra.runtime_in_library` | Per-project manifest has a top-level `runtime` key. |
| `infra.missing_workload` | Aggregated manifest is missing the `workload` identifier. |
| `infra.missing_sources` | Aggregated entry has no contributing sources (every resource must carry provenance). |
| `infra.invalid_scaling` | `runtime.scaling.{max,concurrency}` is negative. |
| `infra.empty_schedule` | `scheduledJobs[].schedule` is empty (or whitespace-only). |
| `infra.invalid_schedule` | `scheduledJobs[].schedule` is non-empty but is not a well-formed 5-field Cloud Scheduler cron expression (wrong field count, out-of-range value, unknown name, or a `@macro`). |
| `infra.conflicting_value` | Merge encountered a conflicting non-empty value (e.g. two storage retentions, two scheduled-job schedules). Severity is `warning` when resolved by contributor precedence (a `manual` override of a `framework:*` value) and `error` when the conflict is between same-precedence contributors. |
| `infra.unused_override` | An `overrides.json` ignore rule matched no aggregated requirement (likely stale or a typo). Warning severity. |
| `infra.non_canonical` | A deployment declaration parses and validates but is not in its canonical byte form, or carries a `$schema` member (see [Deployment declaration](#deployment-declaration)). |
| `infra.build_invalidated` | **Retired: no producer emits this today.** It was the CLI's own gate reporting that it had removed a workload's aggregated manifest because a `build~*` step failed after that gate emitted it. The aggregating task now runs at the END of the workload's build pipeline, so a build that does not complete never reaches it and never writes a manifest for an unfinished build; a manifest from the last successful build is left in place instead of being deleted by an unrelated failure. The code stays in the taxonomy for consumers that still hold older diagnostics. Warning severity. |

### Compatibility

The protocol carries one version knob:

- **`ProtocolVersion`** — the current version. Every in-tree producer stamps it and both JSON schemas pin it. Bumped on any backwards-incompatible change — adding a database engine, dropping a resource kind, renaming a field, changing merge identity, etc. Adding an optional field within an existing kind does not bump the version.

The strict parser accepts exactly `ProtocolVersion` and rejects everything else with `infra.invalid_protocol_version`. A rejected manifest is dropped before aggregation, so a producer that stamps an unsupported version loses its requirements downstream. A bump from `vN` to `vN+1` therefore lands in one change: set `ProtocolVersion = N+1`, update both schema files, and move every producer (Go producers track `infra.ProtocolVersion` automatically; each TypeScript producer bumps its local constant).

A guard test (`version_test.go`) enforces these invariants: both schemas accept exactly `ProtocolVersion`, and every producer stamps it. The producer scan is what catches a producer in one language stamping a version the parser in another no longer accepts — at build time instead of as a silent drop downstream.

### Schemas, fixtures, and tests

The JSON schemas under [`schemas/`](schemas) cover the three wire shapes:
`infra.json` (per-project requirements), `infra-aggregated.json` (the release
artifact), and `infra-overrides.json` (workload suppression). The shared corpus
under [`fixtures/`](fixtures) is the cross-language surface: `valid/` and
`invalid/` documents drive `conformance_test.go` and `strict_test.go`, and
[`fixtures/equivalence/`](fixtures/equivalence) holds the per-producer goldens
(`database`, `document`, `events`, `migration`, `secrets`, `storage`) that pin
byte-identical emission from the Go and TypeScript producers. `drift_test.go`
keeps the schemas in lockstep with the Go types, `determinism_test.go` pins
byte-stable merge output, and `merge_test.go` covers the identity and precedence
rules above.

### Inspecting the manifest

A reference consumer ships with the CLI:

```
putnami infra plan              # human-readable plan for every workload
putnami infra plan <workload>   # plan for one named workload
putnami infra plan --output jsonl
```

The command loads `<workload>/.gen/requirements.json` via `LoadAggregatedManifest`, validates it through the same diagnostic pipeline a deployer would use, and renders every resource kind plus runtime block. It is useful as a build-time sanity check and as a worked example for downstream deployer adapters. Run `putnami build` first to regenerate the ephemeral artifact from committed `infra/` markers.

### What's next (out of scope for this package)

- Deployer adapters (`putnami cloud deploy`, Pulumi, k8s operators) that consume `.gen/requirements.json`. The bundled `putnami infra plan` consumer is a worked reference; real deployers fan out from there.
- Payload-schema attachments on event topics (deferred to a future protocol version).
- `scheduler` framework module that emits `.gen/infra/scheduler.json` for cron jobs declared in code (today `scheduledJobs` is developer-authored only).

## Durable decisions

- [`doc/adr/0001-committed-requirements-are-a-formatter-fixed-point.md`](doc/adr/0001-committed-requirements-are-a-formatter-fixed-point.md)
  — why the committed requirements file is rendered in the repository
  formatter's exact shape instead of `MarshalIndent`.
- [`doc/adr/0002-human-intent-survives-every-generation.md`](doc/adr/0002-human-intent-survives-every-generation.md)
  — why runtime intent and suppression live in files generators never rewrite.
- [`doc/adr/0003-deployers-own-runtime-cost-policy.md`](doc/adr/0003-deployers-own-runtime-cost-policy.md)
  — why minimum residency and billing posture left the workload contract.

## Product feature linkage

No user-facing product feature owns this module, and none should be minted for
it. It is the deployability contract between framework producers, the build
aggregator, and deployers; a developer experiences it as "my workload declares
what it needs", which the framework packages own. Per the spec contract in
[`protocols/features`](../features/README.md) a spec details an already-authored
feature and never mints one, so the durable design intent lives in the ADRs
above. A product feature that later owns deployment links to them rather than
restating them.

## Support

- **Status:** `stable`, recorded as
  `{"id": "go.putnami.dev/protocol/infra", "kind": "protocol", "status": "stable"}`
  in the workspace-root [`putnami.support.json`](../../putnami.support.json)
  catalog (contract: [`protocols/support`](../support/README.md)).
- **Owner:** the Putnami maintainers, as repository owners of `protocols/`.
  Support entries carry no owner field, so the single owner is stated here.
- **Evidence:** producers in both languages emit it in shipped code
  (`go.putnami.dev/app`, `database`, `events`, `migration`, `storage`, the Go
  extension's codegen, and the TypeScript extension's `cmd/putnami-ts`), the
  build aggregator (`go.putnami.dev/sdk/extension/infraagg`) and the CLI's
  `putnami infra plan` consume it, committed `infra/requirements.json` markers
  live in the repository today, and the version guard, drift test, determinism
  test, and cross-language equivalence goldens pin the contract on every run.
