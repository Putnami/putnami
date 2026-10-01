# Contract IR Protocol

> **Status: IR + projections.** This package pins the v1 contract IR and its
> validator, lowers a manifest into Go types, TypeScript types, and JSON Schema,
> projects contract-backed HTTP schemas into OpenAPI/client generation, and
> emits checked Markdown reference documentation.

## Why

A contract compiler needs one canonical, reviewable document that describes a
project's contract vocabulary so a single tool can lower it into many artifacts
— Go and TypeScript types, JSON Schema, OpenAPI inputs, discovery metadata, and
docs — instead of each generator re-deriving the shapes from language-specific
reflection. Two of those shapes were greenfield across the codebase: **enums**
(closed value sets) and **tagged (discriminated) unions**. The closest existing
idiom is the REST client IR's mutually-exclusive `Body`/`BodyType` split; this
package generalizes that concept into a real discriminated union.

The decision to author the IR once and treat every artifact as a lowering — and
why only the JSON Schema is byte-compared across languages — is recorded in
[`doc/adr/0001-one-ir-many-lowerings.md`](doc/adr/0001-one-ir-many-lowerings.md).

## What

The IR is authored (or emitted) once per project to
`<project>/.gen/schema/contracts.json`, from which the codegen committer
promotes it into the tracked tree at `<project>/schema/contracts.json` — the
`.gen/schema/` prefix is what makes it a committed, shipped artifact rather than
ephemeral build scratch. It is versioned (`protocolVersion`, pinned by
`schemas/contracts.json`) and strict: unknown fields are rejected.

The v1 node kinds:

- `enums` — `{ name, description?, values: [{ name, value, description? }] }` — closed value sets.
- `unions` — `{ name, description?, discriminator, variants: [variant] }` — tagged unions.
  - a `variant` is `{ tag, description?, struct? , fields? }`, where `struct` (a
    reference to a declared struct) and `fields` (an inline field list) are
    **mutually exclusive** — the generalization of the REST IR's
    `Body`/`BodyType` split.
- `structs` — `{ name, description?, fields: [field] }` — named DTO shapes.
  - a `field` is `{ name, type, description?, optional?, repeated? }` where
    `type` is a primitive (`string, int, float, bool, duration`) or the name of
    a declared enum/union/struct.
- `configFields` — `{ name, type, description?, required?, default?, sensitive? }` —
  config fields whose `type` is drawn from the shared config field-type
  vocabulary (`string, int, float, bool, duration, object, array, map`) and
  whose typed `default`, when present, is a JSON-native value whose kind must
  agree with `type`.
- `scopes` — `{ name, description? }` — security scopes.
- `capabilities` — `{ name, description?, scopes? }` — authorization capabilities drawing on declared scopes.
- `grants` — `{ name, description?, capability? }` — permission grants conferring a declared capability.
- `claims` — `{ name, type, description?, required? }` — token/principal claims typed from the config vocabulary.
- `principalKinds` — `{ name, description? }` — recognized principal kinds.
- `discovery` — `{ title?, summary?, version?, tags? }` — contract-level discovery metadata.

Type names share one namespace across `enums`, `unions`, and `structs`, so a
field type reference resolves unambiguously.

All closed enums are frozen: extending any of them — including the config
field-type vocabulary or the node set — requires a `ProtocolVersion` bump so
consumers can decide how to react.

## How

Tooling consumes the IR through three entry points:

- `ParseManifest(data) → *Manifest, []Diagnostic` — strict parse (unknown fields rejected).
- `ValidateManifest(m) → []Diagnostic` — structural and semantic validation
  (protocol version, name, per-node presence/enums, duplicate detection, and
  cross-reference resolution).
- `ParseAndValidateManifest(data) → *Manifest, []Diagnostic` — parse then validate.

The emitters lower a validated manifest into concrete artifacts:

- `RenderJSONSchema(m) → map[string]any` / `MarshalJSONSchema(m) → []byte` —
  a Draft 2020-12 JSON Schema over the DTO/enum/union vocabulary. This is the
  cross-language byte-parity artifact: the TypeScript twin
  (`serializeJSONSchema` in `@putnami/application`'s `src/contracts/`) reproduces
  the `MarshalJSONSchema` bytes exactly.
- `EmitGo(m) → string` — a self-contained, gofmt-canonical Go types file (enums
  as a typed string + const block, tagged unions as an interface + one struct per
  variant, structs with json tags, config keys/defaults as constants/vars).
- `emitTypeScript(m)` (TypeScript) — the TypeScript types file (enums as
  string-literal unions + `as const` objects, tagged unions as discriminated
  unions, structs as interfaces).
- `RenderMarkdown(m) → []byte` — deterministic generated reference tables for
  enums, unions, DTOs, configuration, scopes, capabilities, grants, claims, and
  principal kinds. The CLI commits this as `schema/contracts.md`, and
  `contracts check` rejects drift.

Go OpenAPI generation accepts the manifest through `openapi.Options.Contract`
or `openapi.PluginOptions.Contract`. Named endpoint types matching a contract
node become canonical component references. TypeScript endpoints use
`contractStruct(manifest, name)` from `@putnami/application/contracts` for
`.body()` and `.returns()` DTO schemas, while nested enum/union/DTO fields are
derived automatically. `contractType(manifest, name)` remains available for
individual schema properties. Their marker is invisible on the wire and causes
the same component projection. Both client readers retain projected enums and
tagged unions in their IR and emit typed client declarations.

The generated `.go` and `.ts` type files differ by language and are pinned by
golden tests (`testdata/`, updatable with `PUTNAMI_UPDATE_GOLDEN=1`); only the
JSON Schema is compared byte-for-byte across languages.

Diagnostics use the shared `go.putnami.dev/protocol/diagnostic` shape with a
stable error-code taxonomy (`ValidErrorCodes`), e.g. `contracts.unknown_field`,
`contracts.duplicate_node`, `contracts.invalid_discriminator`,
`contracts.enum_value_conflict`, `contracts.unknown_reference`,
`contracts.invalid_field_type`, and `contracts.invalid_default`. Every code
carries a baked remediation string.

### Determinism and cross-language byte parity

Field order in every Go struct is deliberate. The canonical serialization is
`json.MarshalIndent(m, "", "  ")+"\n"`. The TypeScript twin mirrors its field
order, two-space indentation, `omitempty` behavior, HTML and JavaScript
separator escaping, and trailing newline. `fixtures/valid/full.json` is that
exact canonical form for a representative IR; `determinism_test.go` pins it and
asserts serialization is stable across 100 marshals and idempotent under
round-trip. `fixtures/equivalence/contracts.golden.json` covers every node kind
and is the shared input both emitters consume; the emitted JSON Schema is
byte-compared across languages against `testdata/contracts.schema.json`.

### Fixtures

[`fixtures/valid/`](fixtures/valid) holds IR documents that parse and validate
clean; [`fixtures/invalid/`](fixtures/invalid) holds documents that must produce
at least one coded diagnostic. This corpus is the cross-language contract —
`conformance_test.go` runs it through the Go parser and validator, and the
TypeScript emitter reads the same files by relative path. The shared emitter
input is [`fixtures/equivalence/contracts.golden.json`](fixtures/equivalence/contracts.golden.json),
and the per-language goldens live under [`testdata/`](testdata).

### Versioning

`version_test.go` asserts [`schemas/contracts.json`](schemas/contracts.json)
accepts exactly `ProtocolVersion`, pins the emit/committed path constants, and
carries the (currently empty) TypeScript-producer scan that goes live the moment
a TypeScript producer stamps this wire — the guard against a producer stamping a
version the parser would silently drop.

**Compatibility.** The node set, the config field-type vocabulary, and every
closed enum are frozen: extending one is a `ProtocolVersion` bump, not an
additive edit. Readers accept only the exact integer token `ProtocolVersion` and
reject unknown fields, so an unversioned extension fails loudly at the boundary
instead of being dropped downstream. The IR is committed, so a bump is also a
migration of every tracked `schema/contracts.json`.

## Producers and consumers

**Producers** are mostly humans: the IR is authored at
`<project>/schema/contracts.json` — [`protocols/identity`](../identity/README.md)
is the in-repo worked example — or emitted by a project's own generator into
`.gen/schema/contracts.json` for the codegen committer to promote. No producer
outside this workspace stamps the wire yet.

**Consumers** read the committed IR:

- `putnami contracts generate` lowers it into the committed Go types,
  TypeScript types, JSON Schema, and `schema/contracts.md`; `putnami contracts
  check` re-runs the lowering without writing and fails on drift or breaking
  change;
- the Go OpenAPI generator through `openapi.Options.Contract` /
  `openapi.PluginOptions.Contract`, which turns named endpoint types into
  canonical component references;
- `@putnami/application/contracts` through `contractStruct` / `contractType`,
  plus both REST client generators, which retain projected enums and tagged
  unions in their own IR.

## Ownership and support status

- **Owner**: `go.putnami.dev/protocol/contracts` (`protocols/contracts`).
- **Status**: `preview` — recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json) under the classification
  contract in
  [`protocols/support`](../support/doc/adr/0001-support-entry-ownership-and-evidence.md).
- **Evidence**: shipped `contracts generate` / `contracts check` builtins, an
  authored in-repo manifest (`protocols/identity`), consumers in the Go OpenAPI
  plugin and `@putnami/application`, a valid/invalid fixture corpus, golden
  emissions per language, a JSON Schema byte-compared across languages, and a
  schema pinned to the constant by `version_test.go`. It is not `stable`: the
  node vocabulary is still gaining lowerings and nothing outside this workspace
  stamps the wire, so a compatibility promise would be untested.
- **User-facing feature**: none. The IR is a build-time artifact; what a user
  sees is the CLI's `contracts` commands and the typed clients, OpenAPI, and
  documentation generated from it. Declaring a product feature per wire contract
  would move product intent away from the surfaces users actually operate. The
  durable decisions for this module live in [`doc/adr/`](doc/adr/), the form
  [`protocols/features`](../features/README.md#specs-and-durable-decisions)
  expects a spec to link to.
