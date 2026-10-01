# Template Protocol

The manifest contract for a Putnami project template: `putnami.template.json`.

## Why

A template is a directory of files that becomes a project. Without a declared
manifest, everything a tool needs to know about it — its name, what it is for,
which extension the resulting project uses, what the workspace must install for
it to build — lives in conventions that only the scaffolding code knows.

This module makes that metadata explicit and strict, so a template can be
listed, validated, packaged, published, installed and rendered by automation
that never has to guess.

## What

`putnami.template.json` at the template root. Seven fields, all of them
metadata:

| Field | Required | Meaning |
|-------|----------|---------|
| `$schema` | no | Editor pointer at the published schema |
| `name` | **yes** | Template identifier — a scoped package name or a kebab-case identifier |
| `description` | **yes** | Human-readable purpose |
| `version` | no | Template version (semver); stamped at package time when absent |
| `extension` | no | The extension activated for projects created from this template |
| `workspaceDevDependencies` | no | Dev dependencies added to the workspace root when the template is used |
| `testVariables` | no | Render-variable overrides used when testing the template |

The strict pipeline mirrors the other protocol packages:

- `ParseManifestStrict()` decodes with unknown fields rejected;
- `ValidateManifest()` enforces the two required fields;
- `NormalizeManifest()` fills canonical defaults (an absent
  `workspaceDevDependencies` becomes an empty map) in place;
- `ParseAndValidateManifest()` runs all three and returns structured
  diagnostics;
- `StrictLoadManifest()` additionally prefixes each diagnostic with the manifest
  path, because the diagnostic protocol models no line or column for manifest
  validation;
- `LoadManifest()` is the lenient read used where the manifest answers an
  *identity* question and must keep answering for documents a stricter future
  gate would refuse.

**What the manifest deliberately does not describe**: the rendering syntax, the
variable vocabulary, template discovery, the archive format, or the install
layout. Those are engine behaviour owned by `@putnami/cli`, documented in
[`doc/`](doc/) as behaviour rather than as wire, and free to change without a
protocol version. The reasoning is in
[ADR 0001](doc/adr/0001-manifest-declares-identity-only.md).

## Producers and consumers

**Producers** — the seven templates committed in this repository, each a
directory with a `putnami.template.json` next to a `putnami.json` declaring
`"type": "template"` and `"publish": ["template-archives"]`:

| Template | Path |
|----------|------|
| `go-library`, `go-server` | [`go/templates/`](../../go/templates) |
| `typescript-library`, `typescript-server`, `typescript-web` | [`typescript/templates/`](../../typescript/templates) |
| `python-library`, `python-server` | [`python/templates/`](../../python/templates) |

**Consumers** — all in `@putnami/cli`:

| Consumer | Use |
|----------|-----|
| `tooling/cli/internal/template` | Discovery, rendering, packaging, install |
| `tooling/cli/internal/commands` | `templates install/list/update/remove`, `dev template validate/test/package`, `projects create` |

There is no TypeScript twin: nothing outside the CLI reads a template manifest,
so a second implementation would be a second thing to keep in sync for no
consumer.

## Versioning and compatibility

This module declares **no** `ProtocolVersion`. The manifest carries no version
member on the wire, and the strict parser accepts exactly one shape, so there is
nothing for a version token to select between. `Manifest.Version` is the
*template's* own semver, not a protocol version.

Compatibility rules in force today:

- **Unknown fields are rejected**, so adding a member is a deliberate,
  reviewable change to both the Go type and
  [`schemas/template.json`](schemas/template.json), which also declares
  `additionalProperties: false`.
- **Only `name` and `description` are required.** Every other field may be
  absent, and absence has a defined meaning (no extension to activate, no
  workspace dev dependencies, no test overrides) rather than being an error.
- **The lenient `LoadManifest` path is contract, not laziness.** Install-time
  identity probes read a manifest that a stricter gate might later refuse; if
  that read started failing, the shared artifact installer would reinstall the
  same template forever.

A future `ProtocolVersion` anchor plus a pinned conformance test is the
remaining work between this module and a `stable` support status — see below.

## Schemas and fixtures

- Schema: [`schemas/template.json`](schemas/template.json), published as
  `https://putnami.dev/schemas/putnami-template.json`. It closes the object,
  requires `name` and `description`, and constrains `name` to
  `^(@[a-z0-9-]+/[a-z0-9-]+|[a-z0-9][a-z0-9-]*)$`.
- Fixtures: [`fixtures/valid/`](fixtures/valid) (`minimal.json`, `full.json`)
  and [`fixtures/invalid/`](fixtures/invalid) (`missing-name.json`,
  `missing-description.json`), executed by
  [`conformance_test.go`](conformance_test.go).

Note one honest gap between the two: the schema constrains `name` with a
pattern, the Go validator only checks that it is non-empty. The CLI validates
against the schema separately; a name that violates the pattern therefore fails
schema validation but not `ValidateManifest`.

## Behaviour documentation

The engine behaviour that consumes this manifest is documented next to it:

- [`doc/01-manifest.md`](doc/01-manifest.md) — the manifest and template
  directory layout;
- [`doc/02-variables.md`](doc/02-variables.md) — the render syntax and the
  variable vocabulary;
- [`doc/03-scaffolding.md`](doc/03-scaffolding.md) — discovery, project
  creation, and testing a template;
- [`doc/04-distribution.md`](doc/04-distribution.md) — packaging, publishing,
  installing, and the lock entry.

## Support status, owner, and evidence

| | |
|---|---|
| **Status** | `preview` — see the `go.putnami.dev/protocol/template` entry in the workspace-root [`putnami.support.json`](../../putnami.support.json), which is the only authority for this value |
| **Owner** | The `protocols` scope (`protocols/putnami.json`). This module is the sole authority for the manifest shape; the CLI owns rendering and distribution behaviour and may not add manifest fields on its own. |

Evidence behind `preview`:

- a published JSON Schema and a valid/invalid fixture corpus executed by a
  conformance test;
- seven real producers in this repository across three languages, and a real
  consumer that validates, renders, packages and installs them.

Evidence still missing for `stable`:

- no `ProtocolVersion` constant and therefore no pinned version anchor, so a
  shape change has no migration story to attach to;
- the Go validator and the JSON Schema do not enforce the same `name` rule;
- only one implementation exists, so the contract has never been read by a
  second runtime.

## Product feature and durable decisions

**No user-facing feature is declared for this module, deliberately.** The user
outcome — "scaffold a new project that builds" — is owned by the
`projects create` and `templates` command surface of `@putnami/cli` and by the
templates themselves; this module only fixes the metadata those surfaces read.
Minting a product feature per protocol module would create one artificial
identity per technical boundary, which the
[feature protocol's authoring boundary](../features/README.md) rules out. With
no feature to detail there is no spec, since a spec details exactly one
already-authored feature and never mints one.

Durable decisions:

- [ADR 0001 — The template manifest declares identity, not behaviour](doc/adr/0001-manifest-declares-identity-only.md)
