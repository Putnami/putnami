# Migration Protocol

## Why

The migration protocol exists so every Putnami framework applies schema changes with the same deterministic guarantees.

Without a shared contract, Go, TypeScript, and future frameworks can drift on where migration state lives, when startup should fail, how drift is detected, or which errors automation can depend on.

## What

This package defines the versioned migration contract for:

- the canonical dedicated migration state store in `migration.migrations`,
- canonical migration identity and per-datasource ordering,
- startup behavior and fail-fast expectations,
- reliability guarantees such as locking, transactional apply/rollback, and failure recording, and
- structured migration error codes that tooling can reason about.

## How

The protocol standardizes the execution contract, not the authoring format:

- Go can keep filesystem SQL migrations.
- TypeScript can keep code-registered migrations.
- frameworks normalize their native inputs into the same logical migration definition,
- frameworks converge on the same startup and reliability guarantees, and
- frameworks fail startup if migrations cannot be reconciled safely.

Use this package when implementing framework runners, startup hooks, migration-aware tooling, or platform services that need one deterministic migration boundary across languages.

### Current Conformance

- Both Go and TypeScript runners persist migration state in the canonical `migration.migrations` table.
- Both advertise `SupportsRollback`; down SQL is optional per migration but persisted at apply time so rollback works after the defining code is removed.
- The `Definition.Namespace` field labels the feature/plugin that owns each migration. Rows authored before per-feature ownership have an empty `namespace` and are still queryable.

### Canonical State Store

The protocol reserves the `migration` schema and `migrations` table as the canonical state store for full conformance:

```sql
CREATE SCHEMA IF NOT EXISTS migration;

CREATE TABLE IF NOT EXISTS migration.migrations (
  id TEXT PRIMARY KEY,
  db_name TEXT NOT NULL,
  name TEXT NOT NULL,
  hash TEXT NOT NULL,
  executed_at TEXT NOT NULL,
  execution_time_ms INTEGER NOT NULL,
  success INTEGER NOT NULL,
  error_message TEXT,
  down_sql TEXT,
  down_hash TEXT
);
```

Semantics:

- `id` is the canonical datasource-scoped identity: `${db_name}:${name}`
- `executed_at` is an RFC3339 UTC timestamp string
- `success` is `1` for applied migrations and `0` for recorded failures
- `down_sql` / `down_hash` are optional but required for reversible migrations

### Required Guarantees

- migrations execute in deterministic order by datasource, then order key, then name
- startup migration execution happens before the application is considered ready
- startup fails fast on unresolved drift, apply failures, rollback failures, or lock failures
- apply and rollback are transactional with state tracking committed atomically
- runners use advisory locking or an equivalent database-wide mutual exclusion mechanism
- already-applied migrations reject hash drift instead of silently re-running

## Migration Bundle (`migration-bundle.v1`)

The execution contract above governs how migrations run. The **bundle** contract
governs how a release packages its migrations as a self-contained, immutable
artifact that can be published once and executed later — locally or remotely —
without rebuilding the application graph.

A bundle is a directory with a canonical `bundle.json` manifest plus a `payload/`
tree of materialized migration files:

```text
.gen/migration-bundle/
  bundle.json
  payload/sql/default/iam/20260604120000_add_users.up.sql
  payload/sql/default/iam/20260604120000_add_users.down.sql
```

### Manifest

`bundle.json` carries:

- `protocol` — always `migration-bundle.v1`
- `appName` — the workload name
- `source` — project path or package coordinate (provenance only)
- `version` — effective release version
- `git`, `imageDigest`, `generatedAt` — provenance only
- `digest` — content address of the migration content (see below)
- `operations` — the migrations, each with:
  - `kind` (`sql`, `document`, `events`), `target` (datasource/system), `namespace`, `name`, `orderKey`
  - `up` and optional `down` payload references (`{ path, hash }`)
  - `safety` (`safe-online`, `long-running`, `destructive`, `requires-approval`)
  - `capabilities` hints (`transactional`, `reversible`, `async`, `resumable`,
    `compatible`)

Bundles never contain secrets, DSNs, or environment credentials. Remote
execution resolves environment bindings server-side.

### Determinism and the digest

The rule below — and the alternatives that lost — is recorded in
[`doc/adr/0001-bundle-digest-excludes-provenance.md`](doc/adr/0001-bundle-digest-excludes-provenance.md).

`ComputeBundleDigest` content-addresses a bundle: it normalizes the bundle,
sorts operations canonically (kind → target → namespace → orderKey → name), and
hashes only the migration-meaningful subset (`protocol`, `appName`, `version`,
`operations`). Provenance fields (`git`, `imageDigest`, `generatedAt`, `source`)
are excluded so two builds of the same migrations from different checkouts
produce the **same** digest. This is what makes remote publish idempotent by
digest. Payload hashes use the same lowercase SHA-256 hex semantics as
`Definition.Hash`.

### Validation

- `ParseBundle` strictly decodes the manifest, rejecting unknown fields.
- `ValidateBundle` checks the protocol identifier, identity fields, every
  operation and payload reference, rejects duplicate operations, and — when a
  digest is present — fails loudly on digest mismatch.
- `VerifyBundlePayloads` confirms every referenced payload file exists on disk
  and hashes to its pinned reference, catching missing or tampered files.

Publish tooling runs all three so a malformed, incomplete, or tampered bundle
fails before it is ever uploaded.

### Rollback across a migration

Migrations are forward-only, so moving a channel back does not move the database
back: the schema stays where the newest applied migration left it and the
**previous image** is asked to run against it. Two capabilities answer whether
that is safe, and they are independent claims about the same operation:

| Capability | Claim |
|---|---|
| `reversible` | A `down` payload exists, so a runner can undo the operation. Validation refuses the claim without the payload. |
| `compatible` | The schema after the operation stays readable and writable by the previous application image (expand/contract), so a rollback needs no `down` at all. |

`RollbackAllowed(ops)` is the predicate a deploy asks: true when every operation
is `reversible || compatible`. One operation that is neither refuses the whole
set — a rollback is atomic per workload, and a partially-rolled-back schema is
the state nobody can reason about. An empty set is allowed.

Normalization infers `reversible` from a `down` payload, because a rollback
payload is evidence of itself. It never infers `compatible`: nothing on the wire
proves the previous image can read the new schema, so only the author may claim
it. Both markers are `omitempty`, so a bundle that sets neither digests exactly
as it did before they existed. The rule and the alternatives that lost are
recorded in
[`doc/adr/0002-compatible-migrations.md`](doc/adr/0002-compatible-migrations.md).

### How bundles are produced

A migration runner opts into emission by implementing `BundleContributor`,
returning its operations plus the raw payload bytes for each. The build/describe
phase collects contributions from every runner, assembles one `Bundle`, and
calls `WriteBundle` to materialize the directory. `WriteBundle` normalizes,
fills the digest, validates, and cross-checks every payload against its manifest
reference before writing — so emission is a pure, reproducible build step and a
malformed bundle never reaches disk. The Go SQL runner
(`go.putnami.dev/database`) is the first contributor; it loads definitions from
the authoring filesystem without opening a database connection.

The TypeScript framework ships the same protocol (`@putnami/migration`'s
`computeBundleDigest`/`writeBundle`, with the SQL mapping in `@putnami/database`).
The digest algorithm is byte-for-byte identical across languages, pinned by a
shared golden fixture (`fixtures/equivalence/bundle.golden.json`) asserted from
both Go (`bundle_equivalence_test.go`) and TypeScript
(`migration/test/cross-language.test.ts`), so a bundle emitted from either
runtime addresses the same content.

### How bundles are executed

`LoadBundle` is the read-side counterpart of `WriteBundle`: it parses and
validates a bundle directory, reads every payload, verifies hashes, and returns
the normalized bundle plus a path→bytes map. A runner consumes a published
bundle through it without the application graph that produced the bundle.

The Go SQL runner builds on this: `database.LoadBundleSources` reconstructs
`SQLSource`s from a bundle directory (each `sql` operation becomes a Definition
with its payload bytes), and `database.ApplyBundle(ctx, pool, fsys)` executes the
bundle against a connection pool. The reconstruction is exact — same names, SQL,
datasources, hashes, and canonical ids — so executing a published bundle is
identical to applying the migrations from source. This is the generic-runner
core that both local `migrate up --bundle` and remote execution build on; no
service image is required.

### How bundles are packaged and published

Two sub-packages ship a bundle directory through a package registry. They add
transport only: the bundle format, its validation and its digest stay in this
package.

`go.putnami.dev/protocol/migration/bundle` imports only the standard library,
so a dependency-light publisher can use it.

- `Pack(fsys)` writes every file of a bundle directory into one tar. Entries
  are sorted by path, with mode `0600` and a zero modification time, so one
  directory always packs to the same bytes and the same blob digest.
- `Unpack(tarball, dest)` and `UnpackToDir(tarball, dir)` recreate the
  directory. They strip a leading slash, refuse an entry that escapes the
  root, and accept only regular files and directories.
- `Manifest` is the registry payload that points at the blob: `protocol`,
  `appName`, `bundleDigest`, and `artifact` (`blob`, `mediaType`, `size`).
- `BlobMediaType` (`application/vnd.putnami.migration-bundle.v1.tar`),
  `ManifestMediaType`, the default `Namespace` (`migrations`),
  `ValidNamespace`, and `PackageName`, which turns a path-style application
  name into one path segment.

`go.putnami.dev/protocol/migration/bundle/publication` packs the exact bytes a
registry stores for one publication. `Pack(Input)` loads the files with
`LoadBundle`, refuses anything outside the publication scope (`CheckScope`:
`sql` operations only, a valid safety marker and up hash, one canonical
identity per operation), and returns the tar blob, the
`putnami.data.migration.v2` manifest, their `sha256:<hex>` digests, and their
refs. The same input always packs to the same bytes, so a receiver can accept a
stored publication by repacking it and comparing bytes. Tests pin the blob,
bundle, and artifact digests of a fixture under `bundle/testdata/`.

## Producers and consumers

| Shape | Produced by | Consumed by |
| --- | --- | --- |
| Migration state (`migration.migrations`) | the Go runner (`go.putnami.dev/migration` with `go.putnami.dev/database`) and the TypeScript runner (`@putnami/migration` with `@putnami/database`) | the same runners on the next startup, plus migration-aware tooling reading applied state and drift |
| `Definition` (normalized migration identity) | each framework's authoring source — filesystem SQL in Go, code-registered migrations in TypeScript | the runners, and `BundleContributor` implementations at build time |
| Bundle (`bundle.json` + `payload/`) | the build/describe phase, from every runner implementing `BundleContributor`; written by `WriteBundle` | `LoadBundle`, `database.LoadBundleSources` / `ApplyBundle`, `migrate up --bundle`, remote execution, and the database test provider's `bundle-template` reuse |
| Bundle tar blob and registry manifest | a publisher, through `bundle.Pack` or `publication.Pack` | a migration runner, through `bundle.Unpack` / `UnpackToDir`; a receiver that repacks a stored publication to compare bytes |

This package parses, validates, normalizes, and hashes. It opens no connection
and applies no migration.

## Versioning and compatibility

Two independent versions, deliberately separate:

- `ProtocolVersion` — the execution contract (state store, identity, ordering,
  reliability guarantees, error taxonomy).
- `BundleProtocol` (`migration-bundle.v1`) plus the numeric
  `BundleProtocolVersion` — the bundle wire format, so a bundle-format change
  never churns the execution contract and vice versa. `ParseBundle` rejects an
  unknown protocol identifier outright.

Compatibility rests on three enforced rules rather than on convention: strict
decoding rejects unknown manifest fields, `ValidateBundle` fails loudly on a
digest mismatch instead of trusting the manifest, and `VerifyBundlePayloads`
re-hashes every referenced payload. Adding an operation kind, changing the
canonical operation order, or changing the hashed subset changes the digest for
every existing bundle and is therefore a breaking, cross-language change.

## Schemas, fixtures, and tests

This module publishes no JSON Schema: the Go types in [`migration.go`](migration.go)
and [`bundle.go`](bundle.go) are the source of truth. The corpus under
[`fixtures/`](fixtures) is the cross-language surface — `valid/` and `invalid/`
manifests (unknown protocol, duplicate operation, escaping path, bad payload
hash, digest mismatch, …), a materialized `payload-tree/` for the read and
verify paths, and `equivalence/bundle.golden.json`, the byte-identical digest
contract asserted from Go (`bundle_equivalence_test.go`) and from TypeScript
(`migration/test/cross-language.test.ts`). `conformance_test.go`,
`bundle_conformance_test.go`, and `determinism_test.go` drive them.

## Durable decisions

- [`doc/adr/0001-bundle-digest-excludes-provenance.md`](doc/adr/0001-bundle-digest-excludes-provenance.md)
  — why the bundle digest hashes only the migration-meaningful subset, so two
  builds of the same migrations address identically.
- [`doc/adr/0002-compatible-migrations.md`](doc/adr/0002-compatible-migrations.md)
  — why an operation may declare that the previous image can still run against
  the schema it leaves, and why only the author may claim it.

## Product feature linkage

No user-facing product feature owns this module, and none should be minted for
it. It is the execution and packaging contract behind schema migrations; what a
developer experiences is `putnami migrate` and the framework migration packages.
Per the spec contract in [`protocols/features`](../features/README.md) a spec
details an already-authored feature and never mints one, so the durable design
intent lives in
[`doc/adr/0001-bundle-digest-excludes-provenance.md`](doc/adr/0001-bundle-digest-excludes-provenance.md).
A product feature that later owns database evolution links to that record rather
than restating it.

## Support

- **Status:** `stable`, recorded as
  `{"id": "go.putnami.dev/protocol/migration", "kind": "protocol", "status": "stable"}`
  in the workspace-root [`putnami.support.json`](../../putnami.support.json)
  catalog (contract: [`protocols/support`](../support/README.md)).
- **Owner:** the Putnami maintainers, as repository owners of `protocols/`.
  Support entries carry no owner field, so the single owner is stated here.
- **Evidence:** both language runners persist state in the canonical
  `migration.migrations` table and both advertise rollback; both emit and
  execute bundles; the digest is pinned byte-for-byte across languages by the
  shared golden asserted from Go and TypeScript; and the database test provider
  depends on that digest for `bundle-template` reuse in CI.
