# Workspace Protocol

The typed model for how a Putnami workspace describes itself: configuration,
project identity, pinned artifacts, and the wire an extension answers project
discovery over.

## Why

The CLI, three language extensions, and automation all need to agree about which
workspace file wins, how project configuration is loaded, which extension owns a
directory, and exactly which version of every artifact is installed. Without one
shared contract they would each answer those questions slightly differently, and
the difference would show up as a cache miss, a mis-scoped run, or an install
that is not reproducible.

This module owns those answers. It is deliberately a *contract*, not an engine:
it parses, validates, normalizes and merges, and it runs nothing.

## What

Five contracts live here.

### 1. Workspace and project configuration

`putnami.workspace.json` (`Config`) and `putnami.json` (`ProjectConfig`,
`ScopeConfig`). Extension declarations, aliases, project aliases, groups, hooks,
disable lists, command defaults, store tuning, template and agent-artifact
declarations.

- `Load(workspaceRoot)` merges exactly two layers, in precedence order:
  `~/.putnami/config.json`, then `<workspaceRoot>/putnami.workspace.json`. The
  **current directory's `putnami.json` is deliberately not merged**: a project's
  options are applied per project at plan time, because merging them into the
  workspace-wide config leaked one project's options (for instance
  `options.package.docker`) onto every project in the run.
- `LoadProjectConfig()` / `LoadProjectConfigWithDiagnostics()` read a
  project-local `putnami.json`.
- `LoadScopeChain()` and `ScanAllScopes()` resolve the scope files a project
  inherits from.
- `GetCommandDefaults(command, extension)` merges option defaults in a fixed
  order: `*` → `<command>` → `<extension>` → `<extension>:<command>`.
- `ExtensionsConfig` accepts both the array and the map spelling and keeps
  deterministic output either way.
- `ResolveFile()` and `IsWorkspaceConfig()` centralize file discovery, so no
  consumer re-implements "which file is the workspace root".

`ProjectConfig` also carries two shapes with two authored spellings each —
`bin` (string or object) and `exports` (string, object of strings, or object of
conditions). Both round-trip through custom `UnmarshalJSON`/`MarshalJSON` that
**preserve the authored form**, so reading and rewriting a project file never
rewrites the author's spelling.

#### `visibility` — who may import a project

`ProjectConfig.visibility` is `scope` (the default) or `public`. `scope` means
the project is importable by the projects of its own scope: the nearest ancestor
directory whose `putnami.json` contributes scope configuration, which is the
same chain `LoadScopeChain()` resolves. `public` means the whole workspace.
A generated client that declares no visibility, and whose
`client.putnami.json` names a provider in the workspace, is importable from
every scope.

The boundary is stated by the project that is IMPORTED, never by the importer,
and it governs real imports — what `dependencySources` attributes to a go.mod or
a package.json — never a `dependencies` entry a `putnami.json` declares. An
unknown value is a validation error rather than a fallback: a typo must not
widen a boundary. `ResolveVisibility` applies the default, `DefaultVisibility`
names it, and `@putnami/cli` owns what a violation means
([ADR 0045](../../tooling/cli/doc/adr/0045-a-project-declares-who-may-import-it.md)).

#### `distribution` — who may pull what a project publishes

`ProjectConfig.distribution.visibility` is `internal`, `private`, or `public`:
the member level of the release-set visibility chain for every member the
project publishes. `ScopeConfig.distribution` states the same block for every
project below the scope; `LoadScopeChain()` keeps the deepest one, and a
project's own block wins over it. It is unrelated to `visibility` above, which
is an import boundary.

The three levels restate the vocabulary `go.putnami.dev/protocol/distribution`
owns, so a project file validates without that module. A block without a level
is `required-field`; an unknown level is `invalid-distribution-visibility`.
`@putnami/cli` owns how the level meets `putnami.ci.json`
`distribution.members[]`.

#### Version lines

A **line** is a scope that declares a `line` block. Its projects are versioned
and tagged together, and the version of a commit is derived from the line's git
tags rather than from any declared field.

```jsonc
// typescript/putnami.json
{ "includes": ["extension", "framework/web"], "line": { "tag": "ts/v{version}" } }
```

- `tag` carries **exactly one** `{version}` placeholder and is otherwise made of
  `[A-Za-z0-9._/{}-]`. Omitted, it defaults to `<scope path>/v{version}`; at the
  workspace root that is `v{version}`.
- A line is **not inherited**. `LoadScopeChain` never carries a `line` block
  down, and a project belongs to its **nearest ancestor** line — exactly one.
  A workspace where no scope declares a line is one line, `v{version}`.
- `ScanAllScopes` refuses a line declared under another line: every project
  below the inner one would otherwise have two.
- A line on a scope with `activate: true` is refused: the scope-self project
  would be both the line and a member of it.
- `LineTagPattern`, `RenderLineTag` and `ParseLineTag` render a version into a
  tag and read it back. `ParseLineTag` resolves the **most specific** pattern
  first, so `ts/v0.3.0` belongs to `ts/v{version}` even when the root line
  `v{version}` is declared too.

Codes: `invalid-line` at `line` or `line.tag`.

#### Registries

`registries` is a workspace section keyed by **ecosystem id**
(`^[a-z][a-z0-9-]{0,31}$`). It is the single source for every registry endpoint
the CLI and the extensions need; they generate the native files, `.npmrc` among
them, from it.

```jsonc
// putnami.workspace.json
{
  "registries": {
    "npm": { "publish": "https://npm.putnami.dev", "scopes": { "@putnami": "https://npm.putnami.dev" } },
    "go":  { "origin": "https://go.putnami.dev", "proxy": ["https://proxy.golang.org", "direct"] },
    "oci": { "publish": "oci.putnami.dev/putnami" },
    "put": { "registry": "https://put.putnami.dev" }
  }
}
```

This module validates only the **outer map** — the key is an ecosystem id, the
value is a JSON object — and keeps each entry raw. What is inside belongs to
that ecosystem's profile, declared by the extension that owns it
([`protocols/extension`](../extension/README.md)); re-stating it here would
create a second authority for one shape. `RegistryEntry(ecosystem)` reads one
entry back.

A project may override an entry in its own `putnami.json`. The override
**replaces the entry whole**, per ecosystem, for the same reason: a deep merge
would synthesize a document no profile validates.

Code: `invalid-registries` at `registries.<ecosystem>`.

#### Selection vocabulary

File inputs use the shared `filepatterns.go` grammar for cache collection and
change impact. Patterns are project-relative, with `*`, `**` and leading `!`
exclusions. A `git:` prefix selects the containing repository's tracked and
non-ignored untracked candidates; `git:**` covers the whole repository.
The prefix changes collection, not path matching: deleted paths still select
their readers. Configuration keeps its existing array-of-strings shape.

`ParseSelector` is the one grammar every Putnami document uses to select
projects, so a selection means the same thing wherever it is authored:
`tag:<tag>`, `group:<group>`, `scope:<path>`, or a bare project id. The
vocabulary is closed — a prefix invented downstream parses as a project id, not
as a fourth kind — and `Selector.String()` renders back to the authored form.

### 2. Agent-artifact manifest v1

`putnami.agent-artifact.json` is the platform-independent manifest for one
registry-distributed set of agent workflows. It carries only identity and the
files the artifact is authorized to materialize. Every file uses a canonical,
slash-separated path relative to the workspace root and an exact lowercase
SHA-256. The strict parser rejects unknown fields, path traversal, duplicate
paths, malformed hashes, and trailing documents. Canonical serialization sorts
files by path and never mutates authored order.

Workspaces opt in with `agentArtifacts`, an array of registry references. The
resolved exact versions live only in the lock.

`ValidAgentArtifactPath` is the exported form of the path rule the manifest
validator applies, and it is a security control rather than a formatting one:
a consumer that materializes an artifact must re-check every path it is about
to write or remove — including the paths it reads back out of its own ownership
bookkeeping — against this same function. A privately re-spelled copy of the
rule is how a writer ends up accepting what the validator rejected.

### 3. Lock format v4

`putnami.lock.json` is a versioned machine contract pinning the CLI, toolchains,
extensions, templates, and agent-workflow artifacts.

- **v3** introduced an optional `cli.protocolVersion` and a `toolchains` map
  whose entries reuse the standard exact-version, per-platform
  SHA-256 integrity, and release-source shape. Automation can therefore
  preflight the CLI wire and materialize verified runtimes without reparsing
  ecosystem manifests. Current writers generate only Go and Bun; historical
  Node entries remain readable for v3/v4 compatibility and disappear on the
  next explicit toolchain metadata refresh.
- **v4** adds `agentArtifacts`. Each platform-independent entry requires an
  exact version, an archive SHA-256 and a manifest SHA-256; the manifest then
  binds every materialized file.

The `cli` entry has two shapes, and a reader must branch on them rather than
read `cli.version` unconditionally. A **published pin** records an exact version
and the per-platform executable digests a launcher verifies before running those
bytes. A **source workspace** records `"source": "workspace"` and nothing else:
it builds its own CLI from the tree in the working copy, so there is no
published version to name. Each shape rejects the other's fields, because a pin
without a version is unreproducible and a sentinel with one claims an artifact
the workspace does not have. Omitting the entry entirely still means "no
opinion" — a launcher keeps whatever binary is running.

New locks are authored as v4. The accepted read window is `LockMinVersion` 2
through `LockVersion` 4: v2 and v3 parse at their recorded version, and v4
vocabulary appearing in an older lock is rejected by the strict validator and
discarded by the version-projecting CLI reader. `MigrateLockToCurrent` is the
explicit, non-mutating v2/v3 → v4 projection — **parsing never migrates a lock
implicitly**.

### 4. Workspace probe v1

Core owns identity; providers own language knowledge. A provider answers a
`ProbeRequest` with a `ProbeResult` describing the project directories it
recognizes: source identity, type and tags, derived dependency paths,
publish/runs-with metadata, requested extension references, watched files, and
opaque provider-owned metadata.

Project-level `watchedFiles` may include files at the workspace root: providers
own those filenames and the project mapping, while core only exact-matches the
paths for snapshot invalidation and impacted selection. Reusing this v1 member
keeps newer providers readable by older strict consumers.

Three rules make the wire usable as a cache-key input:

- **Key on paths.** Every project, dependency, source file and watched file is a
  repo-relative, slash-separated path. A project root is *not* assumed to be the
  provider's own unit root: `sourceFile` may sit arbitrarily deep below `path`,
  and dependency edges are project paths core resolves — never module
  identifiers the provider resolved.
- **Deterministic digest.** `NormalizeProbeResult` collapses authoring order,
  path spelling, duplicate list entries and metadata key order into one
  canonical form; `ProbeResultDigest` / `ProbeWorkspaceDigest` hash that form.
  Nothing that varies between two runs over the same tree enters the digest — no
  timestamps, no absolute paths, no map iteration order — and advisory
  `diagnostics` are excluded so a reworded warning cannot cold a cache key.
- **Content is the oracle.** The protocol carries no modification times and no
  file sizes. Consumers may use stat data to *prioritize* work; validity is
  decided by content digests alone, so a same-size, same-second rewrite still
  invalidates.

Merge rules, in force order (`MergeProbeResults`):

1. Core alone assigns canonical project paths and IDs.
2. Explicit `putnami.json` values override probe values, and resolve conflicts.
3. Two providers reporting different non-empty scalars for one project is a hard
   error unless rule 2 already decided the field.
4. Explicit and probe list members are unioned and normalized, never replaced.
5. Provider-owned metadata lands under `metadata[<extension>]`.

`ResolveProjectName` states the identity precedence — explicit `putnami.json`
name > probe source identity > scope `namePattern` > directory basename — and
`FillScopeTags` states that scope tags fill an empty tag set but never overwrite
one the project curated.

A failed probe is a typed `ProbeFailure` (`provider-unavailable`, `transport`,
`timeout`, `invalid-result`, `merge-conflict`, `visibility-violation`), so a
consumer can fail graph-dependent commands with the cause attached instead of a
flattened string. The last kind is not a transport fault: the answer is usable
and the graph it describes is one the workspace refuses to plan over (see
`visibility` below), and it travels here for the policy this type carries —
every graph-dependent command fails, the repair commands stay reachable.

`dependencySources` attributes each reported edge to the manifest family it was
derived from: `go-module` for a go.mod `require` or `replace`, `package-json`
for a package.json dependency on a workspace package, `declared` for an edge a
declaration states. It never changes WHICH edges a provider reports — a provider
whose answer folds a declared list into the same list as its real imports
reports both here, so a consumer that must act on imports alone can tell them
apart without re-reading the provider's manifests. A dependency the map does not
name is `declared`; `contract` is core's own derivation and no provider may
claim it. The merge unions the maps, and an edge two providers describe
differently resolves to the import.

The reasoning behind the core/provider split and the digest rules is in
[ADR 0001](doc/adr/0001-core-owns-identity-providers-own-language.md).

### 5. Probe transport

Core spawns the provider's executable with the reserved control call
`<executable> __putnami workspace-probe`, writes ONE `ProbeRequest` document to
its stdin, and reads ONE `ProbeResult` document from its stdout. The reserved
verb is the namespace the runtime handshake already uses, so an extension that
later declares a `workspace-probe` task cannot shadow the probe.

```go
// provider side, before ordinary subcommand dispatch
handled, err := workspace.ServeProbe(os.Args[1:], os.Stdin, os.Stdout, answer)
```

Three transport rules:

- **Stdout carries the result and nothing else.** A provider that logs to stdout
  produces a document with trailing data, which is rejected rather than
  truncated at the first brace. Human output belongs on stderr, which core
  attaches to the failure message.
- **Bounded in both directions** (`MaxProbeDocumentBytes`, 8 MiB). An unbounded
  read is how a provider that never terminates becomes an orchestrator that
  never terminates.
- **Nothing on failure.** `ServeProbe` validates its own answer against the
  contract before a byte reaches stdout, and writes nothing when the provider
  errors — so core sees "no result document" rather than a half-truth it might
  merge.

### `featureAuthority` — why a project declares no feature

A published project is normally expected to declare a user-facing feature, and
`putnami specs validate` reports one that does not. Two kinds of project answer
that question differently, and both answers used to live only in prose that no
check can read:

```json
{
  "featureAuthority": {
    "none": "A support status is product policy about other subjects, not a feature a user enables."
  }
}
```

```json
{
  "featureAuthority": { "owner": "go/api-contracts" }
}
```

`none` states the reason a module has no user-facing feature — every
`protocols/*` module carries one, because a wire contract is not something a
user enables. `owner` names the authored feature this project contributes to
when the declaration lives elsewhere: `go.putnami.dev/{openapi,proto}` both
serve the `go/api-contracts` outcome declared by `go.putnami.dev/api`.

Exactly one member is set, and validation rejects an empty block, a blank
reason, or both members at once. An `owner` that names no authored feature is
reported as `specs.unresolved_feature_authority` **and** still counts as a
missing link — an answer nobody can resolve is worse than no answer, and the
field must never become the cheapest way to silence completeness.

The field is optional and additive: a project that omits it is assessed exactly
as before.

## Producers and consumers

**Providers (probe producers)** — each answers `__putnami workspace-probe` for
the languages it owns:

| Provider | Path |
|----------|------|
| Go extension | `go/extension/cmd/putnami-go` |
| TypeScript extension | `typescript/extension/cmd/putnami-ts` |
| Python extension | `python/extension/cmd/putnami-python` |

**Consumers** — `@putnami/cli` reads every contract in this module:

| Consumer | Use |
|----------|-----|
| `tooling/cli/internal/workspace` | Workspace/project/scope loading and merging |
| `tooling/cli/internal/extension` | Extension declarations, install and lock entries |
| `tooling/cli/internal/jobs`, `.../engine` | Plan-time option resolution and project selection |
| `tooling/cli/internal/watch` | Watched-file invalidation |
| `tooling/cli/internal/store` | Store tuning from workspace config |
| `tooling/cli/internal/mcp`, `.../features`, `.../mapgen`, `.../hooks`, `.../cli` | Project identity, hooks, and generated views |

**Authored producers** — every `putnami.workspace.json`, `putnami.json` and
`putnami.lock.json` in this repository, including the workspace root's own.

## Versioning and compatibility

Four independent version anchors, all pinned by tests:

| Anchor | Value | Where |
|--------|-------|-------|
| `ProtocolVersion` (workspace/project config) | `1` | `conformance_test.go` |
| `ProbeProtocolVersion` | `1` | probe fixtures and drift tests |
| `AgentArtifactManifestProtocolVersion` | `1` | `agent_artifact_test.go`, `drift_test.go` |
| Lock read window | `LockMinVersion` 2 … `LockVersion` 4 | `lock_test.go` |

The workspace and project configs carry **no** protocol version member on the
wire: the strict parser accepts a single shape and rejects unknown fields, so
`ProtocolVersion` is the single anchor a bump has to move. `Config.Version` is a
free-form authored string (a workspace's own semver), deliberately left a
passthrough so existing valid files keep parsing.

Compatibility rules that consumers depend on:

- **Strict decoding everywhere.** An unknown field is a diagnostic, not a
  silently ignored member, so a newer producer cannot half-work against an older
  reader.
- **The lock read window is explicit and asymmetric.** Older locks are read at
  their recorded version; newer vocabulary in an older lock is an error rather
  than an upgrade. Migration is a call the caller makes.
- **Probe v1 members are reused, not superseded.** Adding a member for a new
  provider capability would make every older strict consumer reject the result,
  so a provider expresses new facts through the existing `metadata[<extension>]`
  slot.
- **Authored spellings are preserved.** `bin`, `exports`, and the two
  `extensions` forms round-trip in the form the author wrote, so a tool that
  rewrites a project file produces no incidental diff.
- **The digest excludes advisory content** (`diagnostics`), so a reworded
  warning is not a cache-key change.

## Schemas and fixtures

Six published schemas in [`schemas/`](schemas):
[`workspace.json`](schemas/workspace.json),
[`project.json`](schemas/project.json), [`scope.json`](schemas/scope.json),
[`lock.json`](schemas/lock.json), [`probe.json`](schemas/probe.json), and
[`agent-artifact.json`](schemas/agent-artifact.json).

Fixtures under [`fixtures/`](fixtures), grouped by shape:

| Corpus | Covers |
|--------|--------|
| `fixtures/{valid,invalid}` | Workspace config, including the registries section |
| `fixtures/project/{valid,invalid}` | Project config, including task-timeout rules |
| `fixtures/scope/{valid,invalid}` | Scope activation, inheritance, and line blocks |
| `fixtures/lock/{valid,invalid}` | v2, v3 and v4 locks, plus cross-version vocabulary leaks |
| `fixtures/probe/{valid,invalid}` and `fixtures/probe-request/{valid,invalid}` | Probe results and requests, including path escapes and duplicates |
| `fixtures/agent-artifact/{valid,invalid}` | Agent-artifact manifests |

Three test families hold them together:

- `conformance_test.go` and `shapes_conformance_test.go` run every fixture
  through its strict pipeline;
- `probe_conformance_test.go` replays each probe fixture **as a provider** and
  asserts the four properties a real provider is held to — strict, canonical,
  path-keyed, and mergeable with itself;
- `drift_test.go` pins each Go struct against its published schema, and
  `invalid_fixture_coverage_test.go` proves every reject branch that protects
  the digest has a fixture triggering it.

## Support status, owner, and evidence

| | |
|---|---|
| **Status** | `stable` — see the `go.putnami.dev/protocol/workspace` entry in the workspace-root [`putnami.support.json`](../../putnami.support.json), which is the only authority for this value |
| **Owner** | The `protocols` scope (`protocols/putnami.json`). This module is the sole authority for workspace/project identity, the lock format, and the probe wire; neither the CLI nor a provider may extend them locally. |

Evidence behind `stable`:

- four pinned version anchors, each asserted by a conformance test, and an
  explicit lock read window with a matrix test across v2/v3/v4;
- six published JSON Schemas, each held against its Go type by a drift test, so
  schema and implementation cannot disagree;
- a fixture corpus spanning six shapes with valid and invalid cases, plus a
  coverage test proving each digest-protecting reject branch is exercised;
- a provider conformance suite that replays fixtures as real providers;
- determinism tests over validation output, extension-name ordering, scope
  name-pattern resolution and agent-artifact canonical bytes;
- three independent producers (Go, TypeScript and Python extensions) and a
  consumer that reads every contract here.

## Product feature and durable decisions

**No user-facing feature is declared for this module, deliberately.** The user
outcomes it underpins — a workspace whose projects are discovered correctly, and
an install that is reproducible — are owned by the CLI commands and the language
extensions that deliver them. A feature per protocol module would create one
artificial product identity per technical boundary, which the
[feature protocol's authoring boundary](../features/README.md) rules out. With no
feature to detail there is no spec, since a spec details exactly one
already-authored feature and never mints one.

Durable decisions:

- [ADR 0001 — Core owns identity, providers own language knowledge](doc/adr/0001-core-owns-identity-providers-own-language.md)
- [ADR 0002 — Lines are scopes, registries are keyed by ecosystem, versions come from git](doc/adr/0002-lines-are-scopes-registries-by-ecosystem.md)
