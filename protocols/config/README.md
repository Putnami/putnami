# Config Protocol

The shared contract for application configuration: schema extraction, canonical
hashing, remote resolution, secret routing, and write operations.

## Why

Go and TypeScript applications both extract a config schema at build time and
resolve remote configuration at startup. Without one shared contract the two
languages drift on the hash algorithm, the field-type vocabulary, and the
resolve request shape — which produces incompatible manifests and a silent
mismatch at the config server rather than a loud error at build time.

This module is the single source of truth both extractors and both runtime
clients implement.

## What

### Schema contract

- **`SchemaManifest`** — produced by schema extraction, consumed at publish
  time. Carries app identity, a canonical schema hash, and an ordered list of
  config blocks with typed fields.
- **`RegisteredSchemaManifest`** — the server-facing document returned by read
  and history endpoints. Adds the server-owned `registeredAt` timestamp.
- **Field type vocabulary** — `string`, `int`, `float`, `bool`, `duration`,
  `object`, `array`, `map` (`ValidFieldTypes`). Both extractors map
  language-specific types onto exactly these:
  - `duration` — a Go `time.Duration` string: `"30s"`, `"5m"`, `"1h"`;
  - `object` — nested fields in `fields`; an opaque object has none;
  - `array` — element shape in `items`;
  - `map` — key type in `keys` (`string`, `int` or `bool` only —
    `ValidMapKeyTypes`; composite keys have no TypeScript analogue) and value
    shape in `values`;
  - `sensitive: true` — the value belongs in the envelope-encrypted secrets
    store rather than in plaintext config;
  - `constraints` — advisory validation hints (e.g.
    `"oneof=debug|info|warn|error"`). Declared, never enforced.
- **Canonical hash** — `ComputeSchemaHash` canonicalizes blocks (sorted paths,
  recursively sorted fields, recursing through `fields`/`items`/`values`),
  marshals, SHA-256s, and returns `sha256:` plus the first **16 hex
  characters**. Equivalent schemas hash identically regardless of extraction
  order.
- **`SchemaManifestJSONSchema()`** embeds
  [`schemas/schema-manifest.json`](schemas/schema-manifest.json), so the Go and
  TypeScript conformance tests assert the extractor vocabulary against one
  document.

### Resolution contract

`ResolveRequest` / `ResolveResponse`: clients send `appName`, `environment`, and
optionally `version` and `schemaHash`; the server returns the merged config with
layer metadata and optional `warnings` when values do not match the schema.

Resolution layers, later winning by deep merge:

```
*/*  →  */env  →  app/*  →  app/env  →  app/env/version (optional)
```

### Secret resolution contract

`ResolveSecretsRequest` / `ResolveSecretsResponse` have full shape parity with
the config resolve contract; the values map is named `secrets` instead of
`config`. `SealedEnvelope` is `{keyUri, wrappedDek, nonce, ciphertext}`, all
base64, so clients and tooling can parse an envelope-encrypted secret without
being able to open it.

### Write contract

| Type | Shape |
|------|-------|
| `ConfigEntry` | `{appName, environment, version?, path, values, updatedAt?}` |
| `SecretEntry` | `{appName, environment, version?, path, envelope, updatedAt?}` |
| `DeleteRequest` | `{appName, environment, version?, path}` |

### Permission scopes

`AllScopes()` is the canonical set used in JWT claims and server middleware:
`config.schema.read`, `config.schema.write`, `config.value.read`,
`config.value.write`, `config.secret.read`, `config.secret.write`.

### Sensitive-field policy

`FieldSchema.Sensitive` is the single authority for routing a value between the
plaintext config store and the encrypted secrets store — see
[ADR 0001](doc/adr/0001-sensitive-decides-the-store.md). Three helpers state the
rules:

| Situation | Verdict | Helper |
|-----------|---------|--------|
| Sensitive field written through the config store | error | `ValidateConfigEntryAgainstSchema` |
| Non-sensitive field written through the secrets store | warning | `ValidateSecretEntryAgainstSchema` |
| Any write to a path absent from the registered schema | error | `ValidatePathInSchema` |

All three are **diagnostic-only**: they return `diag.Diagnostic` slices and
never reject anything themselves. `ValidateConfigEntryAgainstSchema` is
deliberately strict for recognized blocks — unknown fields, invalid value types,
invalid map keys, and sensitive values in plaintext are all errors.

**No producer in this repository calls them yet.** They are a defined policy
surface awaiting adoption; a control-plane rollout must update request handling
and tests in lockstep before treating these diagnostics as hard rejections.

### Input validation

Naming fields share the length and required rules but use **two different
character grammars**:

- `environment`, `path`, `version` — `[a-zA-Z0-9.*_-]+`;
- `appName` — `[a-zA-Z0-9.*_@/-]+`, and additionally **no empty, `.`, or `..`
  path segments**. `appName` is more permissive because it is the project name,
  which for Go modules (`go.putnami.dev/events`) and scoped npm packages
  (`@putnami/application`) legitimately contains `/` and `@`. The traversal rule
  keeps a project name from escaping the server's per-app storage prefix, so
  `app/../etc` and `/etc/passwd` are rejected even though every character in
  them is otherwise allowed.

Common to all: non-empty (except the optional `version`), at most 256
characters, and at most 1000 top-level keys in a values map.

### Multi-tenant scoping

Every operation is tenant-scoped, and the tenant is derived from request context
(JWT claims or workload identity) rather than passed as a parameter. It is
therefore not a field on any shape in this module.

## API surface

The endpoint contract the shapes above are exchanged over:

| Method | Path | Scope |
|--------|------|-------|
| `POST` | `/api/schemas` | `config.schema.write` |
| `GET` | `/api/schemas/{app}` | `config.schema.read` |
| `GET` | `/api/schemas/{app}/history` | `config.schema.read` |
| `DELETE` | `/api/schemas/{app}` | `config.schema.write` |
| `POST` | `/api/configs/resolve` | `config.value.read` |
| `PUT` | `/api/configs` | `config.value.write` |
| `DELETE` | `/api/configs` | `config.value.write` |
| `POST` | `/api/secrets/resolve` | `config.secret.read` |
| `PUT` | `/api/secrets` | `config.secret.write` |
| `DELETE` | `/api/secrets` | `config.secret.write` |
| `GET` | `/_/health` | (none) |

`GET /api/schemas/{app}/history` returns `[]RegisteredSchemaManifest`
newest-first.

## Producers and consumers

**Producers (schema extraction)**

| Producer | Role |
|----------|------|
| `go/extension/internal/jobs/configextract` | Extracts the Go schema manifest, including the sensitivity tag |
| `typescript/framework/application/src/config/config-schema-extract.ts` | Extracts the TypeScript schema manifest |

**Consumers**

| Consumer | Use |
|----------|-----|
| `go/framework/app` | Runtime config typing and remote resolution |
| `go/extension/internal/codegen`, `go/extension/internal/toolchain` | Config type generation and toolchain wiring |
| `tooling/cli/internal/commands` | Publish-time manifest handling |
| [`protocols/contracts`](../contracts/README.md) | Lowers config fields and typed defaults into the contract IR |

**Cross-language equivalence** is proven by
`typescript/framework/application/test/config/config-schema-extract.test.ts`,
which reads this module's own [`fixtures/`](fixtures) directory directly
(`computeSchemaHash cross-language conformance` and `renderJSONSchema
cross-language conformance`). Because both sides read the same committed bytes,
the hash fixtures cannot be regenerated on one side only.

There is no `@putnami/protocol-config` package: the TypeScript side implements
the contract inside `@putnami/application` and is held to it by those shared
fixtures.

## Versioning and compatibility

This module declares **no** `ProtocolVersion` constant. None of its documents
carries a version member: the strict parser accepts exactly one shape per type
and rejects unknown fields, so there is nothing for a version token to select
between.

What plays the role of a version instead is the **schema hash**. A client sends
`schemaHash` with a resolve request and the server answers `schemaMatch`, so a
runtime whose compiled-in schema no longer matches the registered one finds out
at resolution rather than by reading a wrong value.

Compatibility rules in force:

- **Unknown fields are rejected** on every shape, so adding a member is a
  reviewed change to the Go type, the schema, and the TypeScript extractor at
  once.
- **The field-type vocabulary is closed.** An extractor that meets a type it
  cannot map must fail, not invent a ninth value.
- **The hash algorithm is frozen** while the vocabulary is: prefix `sha256:`,
  16 hex characters, recursive canonicalization. Changing any of the three
  invalidates every registered schema in every environment simultaneously.
- **`registeredAt` is server-owned.** Clients never author it, which is why it
  lives on `RegisteredSchemaManifest` rather than on `SchemaManifest`.

A `ProtocolVersion` anchor pinned by a conformance test is the main gap between
this module and a `stable` support status — see below.

## Schemas and fixtures

- Schema: [`schemas/schema-manifest.json`](schemas/schema-manifest.json),
  embedded in Go via `SchemaManifestJSONSchema()`. The resolve, secret and write
  shapes have no published JSON Schema; their strict parsers are the contract.
- Fixtures: [`fixtures/valid/`](fixtures/valid) (14 documents covering manifests,
  nested and typed shapes, versioned and unversioned entries, resolve requests
  with and without a hash, and a production-unsafe case),
  [`fixtures/invalid/`](fixtures/invalid) (12 documents covering bad field
  types, bad hash prefixes, bad map keys, misplaced slots, missing required
  members, invalid base64 envelopes, and unknown fields), and
  [`fixtures/jsonschema/`](fixtures/jsonschema) (the rendered JSON Schema
  goldens shared with TypeScript).
- Tests: [`conformance_test.go`](conformance_test.go) runs the corpus through
  every parse/validate pipeline; [`determinism_test.go`](determinism_test.go)
  pins that validation output and `ComputeSchemaHash` are stable across 100
  repetitions.

### File organization

| File | Contents |
|------|----------|
| `config.go` | `SchemaManifest`, `RegisteredSchemaManifest`, `Block`, `FieldSchema`, resolve shapes, `Dimension`, `DeepMerge` |
| `secrets.go` | `SealedEnvelope`, resolve-secrets shapes, `SecretEntry` |
| `write.go` | `ConfigEntry`, `DeleteRequest` |
| `permissions.go` | Permission scope constants and `AllScopes()` |
| `validation.go` | Naming grammars and input validation |
| `policy.go` | Sensitive routing and path-in-schema policy helpers |
| `jsonschema.go` | JSON Schema rendering shared with TypeScript |
| `strict.go` | Strict parsing and validation pipelines |
| `hash.go` | Canonical schema hash |
| `schemas/schema-manifest.json` | Manifest schema contract |

## Support status, owner, and evidence

| | |
|---|---|
| **Status** | `preview` — see the `go.putnami.dev/protocol/config` entry in the workspace-root [`putnami.support.json`](../../putnami.support.json), which is the only authority for this value |
| **Owner** | The `protocols` scope (`protocols/putnami.json`). This module is the sole authority for the field vocabulary, the hash algorithm and the wire shapes; neither extractor may extend them locally. |

Evidence behind `preview`:

- a published manifest schema, a 26-document valid/invalid fixture corpus, and
  conformance plus determinism tests over it;
- genuine cross-language equivalence: the TypeScript extractor's tests read this
  module's fixtures directly, so a one-sided regeneration fails;
- real producers and consumers on both sides (tables above).

Evidence still missing for `stable`:

- no `ProtocolVersion` constant and therefore no pinned version anchor;
- only `SchemaManifest` has a published JSON Schema — the resolve, secret and
  write shapes have none;
- the sensitive-routing policy helpers have no producer, so the module's most
  security-relevant rules are unexercised outside its own tests;
- `constraints` is declared but never enforced, so a schema can carry a hint no
  reader honours.

## Product feature and durable decisions

**No user-facing feature is declared for this module, deliberately.** The user
outcomes it serves — typed configuration that fails fast when it drifts, and
secrets that never land in plaintext — belong to the framework packages and the
config server that implement them, not to a wire contract. Declaring a feature
per protocol module would create one artificial product identity per technical
boundary, which the [feature protocol's authoring
boundary](../features/README.md) rules out. With no feature to detail there is
no spec, since a spec details exactly one already-authored feature and never
mints one.

Durable decisions:

- [ADR 0001 — `sensitive` decides the store, and the schema decides what may be written](doc/adr/0001-sensitive-decides-the-store.md)

## Known gaps

- **TypeScript `Number` → `int`.** A TypeScript `Number` maps to `"int"` by
  default, matching Go's default. A float needs an explicit `Float` descriptor.
- **`constraints` is advisory.** Min, max, pattern and enum validation is
  declared and unimplemented; implement it when a concrete requirement appears
  rather than speculatively.
- **`SCHEMA_HASH`.** Both runtime clients read this environment variable at
  startup. A pipeline that does not set it from the extracted manifest
  (`schema/config.json`, or `.gen/config-schema.json` for projects that opted
  out) leaves the runtime unable to detect schema drift.
