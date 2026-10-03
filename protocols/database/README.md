# `go.putnami.dev/protocol/database`

The Putnami **canonical database protocol**: the single cross-language contract
for declaring the databases a workload needs, the resolved connection a deployer
hands back, and the extra policy a test run layers on top.

The point of this package is that `database` itself owns the shared contract.
Go, TypeScript, and (later) Python adapters consume the *same* shapes instead of
each modeling database configuration differently — removing the historical
drift between Go's `PoolConfig{Datasource, Connection, DSN}` and TypeScript's
`database.{host, port, database, user, …}`.

This package is **declaration-only**. It has no dependency on a database driver
or a cloud server, so a project, a deployer (e.g. Putnami Cloud), the framework
database libraries (`go.putnami.dev/database`, `@putnami/database`), and the
test provider can all depend on it without a cycle.

## Three planes

The protocol separates three concerns and owns only the contract:

- **Protocol (this package).** The `RequirementManifest`, `Binding`, and
  `TestBinding` shapes, their validation, and the closed enums. It names no
  driver and opens no connection.
- **Provisioning (a deployer/environment).** Reads a `RequirementManifest`,
  provisions the database, and injects a `Binding` with resolved connection
  data.
- **Data plane (the workload / test provider).** Builds a driver
  (pgx / postgres.js) from a `Binding`'s `Connection` and talks to Postgres
  directly.

## Three layered shapes

A `Binding` is a `RequirementManifest` entry plus a resolved `Connection`; a
`TestBinding` is a `Binding` plus provisioning policy. All three key their
datasources by **logical datasource name**, so multi-datasource workloads are
modelled by construction — no single-DSN convention.

### 1. Requirement manifest — `ParseAndValidateRequirementManifest`

A project's logical database needs, **secret-free**. This is the rich source of
truth that `protocol/infra` projects into its thin `{name, engine, schemas}`
requirement (see [Projection into infra](#projection-into-infra)). A requirement
has no `connection` field, so strict parsing rejects any attempt to smuggle
connection details — and therefore secrets — into `infra/requirements.json`.

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-database.json",
  "protocolVersion": 1,
  "databases": {
    "auth":    { "engine": "postgres", "schema": "iam" },
    "billing": { "engine": "postgres", "schema": "billing" }
  }
}
```

### 2. Runtime binding — `ParseAndValidateBinding`

The resolved physical connection a deployer/environment injects after
provisioning. Framework adapters convert each datasource's `connection` into a
driver configuration internally; application code stays driver-neutral
(`database.NewPlugin({Datasource: "auth"})` in Go, `sql({ datasource: 'auth' })`
in TypeScript). Bindings carry secrets and must never be written into
`infra/requirements.json`.

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-database-binding.json",
  "protocolVersion": 1,
  "databases": {
    "auth": {
      "engine": "postgres",
      "schema": "iam",
      "connection": { "host": "localhost", "port": 5432, "database": "auth", "user": "postgres", "password": "postgres", "ssl": false }
    },
    "billing": {
      "engine": "postgres",
      "schema": "billing",
      "connection": { "instance": "project:region:billing-db" }
    },
    "analytics": {
      "engine": "postgres",
      "schema": "events",
      "connection": { "dsn": "postgres://postgres:postgres@localhost:5432/analytics?sslmode=disable" }
    }
  }
}
```

### 3. Test binding — `ParseAndValidateTestBinding`

A runtime binding extended with test provisioning and isolation policy. Same
datasource/connection shape, so a workload can request migrated Postgres
bindings for several datasources at once.

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-database-test-binding.json",
  "protocolVersion": 1,
  "mode": "require",
  "isolation": "database",
  "reuse": "bundle-template",
  "applyMigrations": true,
  "databases": {
    "auth":    { "engine": "postgres", "schema": "iam",     "connection": { "host": "localhost", "port": 5432, "database": "auth_test",    "user": "postgres", "password": "postgres", "ssl": false } },
    "billing": { "engine": "postgres", "schema": "billing", "connection": { "host": "localhost", "port": 5432, "database": "billing_test", "user": "postgres", "password": "postgres", "ssl": false } }
  }
}
```

| Policy field | Values | Meaning |
| --- | --- | --- |
| `mode` | `skip` \| `require` \| `auto` | `skip` skips DB tests when no usable binding/provider exists; `require` fails loudly; `auto` allows local convenience provisioning (e.g. Docker). |
| `isolation` | `database` \| `schema` | Isolation boundary per datasource. Prefer `database` when `CREATE DATABASE` is permitted; fall back to `schema`. |
| `reuse` | `none` \| `bundle-template` | `bundle-template` may cache a migrated template database keyed by migration bundle digest. |
| `applyMigrations` | bool | When true, the provider applies and verifies migrations before returning bindings. |
| `keepDatabases` | bool | When true, the per-suite teardown closes its connections but skips `DROP DATABASE`, leaving cleanup to the server's own lifetime (e.g. a CI server that dies with the run). Every `DROP DATABASE` forces an immediate cluster-wide checkpoint, so on a busy shared server its cost tracks the whole fleet's writes. |

## The connection contract

A `connection` declares **exactly one** transport strategy. The strict validator
enforces this (`database.invalid_connection`):

| Strategy | Selector | Companion fields |
| --- | --- | --- |
| DSN | `dsn` | none — a DSN is self-contained |
| TCP | `host` | `port`, `database`, `user`, `password`, `ssl`, `params` |
| Cloud socket | `instance` | `database`, `user`, `password`, `params` |

Adapters may internally render a DSN or driver options from any strategy, but
Go/TS application code no longer exposes different public config shapes.

### Connection examples

TCP Postgres with user/password:

```json
{ "host": "db.internal", "port": 5432, "database": "auth", "user": "app", "password": "s3cret", "ssl": true }
```

Unix socket / Cloud SQL style:

```json
{ "instance": "my-project:us-central1:auth-db", "database": "auth", "user": "app", "password": "s3cret" }
```

Deployer-provided binding (DSN, e.g. injected by Putnami Cloud):

```json
{ "dsn": "postgres://app:s3cret@10.0.0.5:5432/auth?sslmode=require" }
```

Local Docker Postgres:

```json
{ "host": "localhost", "port": 5432, "database": "auth", "user": "postgres", "password": "postgres", "ssl": false }
```

## Projection into infra

`RequirementManifest.Project()` flattens the manifest into the thin, secret-free
triplet `{Name, Engine, Schemas}` — sorted by datasource name for deterministic
aggregation — that `protocol/infra` keeps in its requirements. This is how the
database protocol becomes the source of truth while `protocol/infra` continues
to aggregate a deployer-facing projection (mirroring how `protocol/events` owns
the event contract while infra carries only a thin `{publishes, subscribes}`).

`protocol/infra` consumes this projection through
`infra.DatabasesFromManifest(*database.RequirementManifest)`, which calls
`Project()` and maps each `{Name, Engine, Schemas}` entry onto an `infra.Database`
(translating the engine into infra's enum). Because a `RequirementManifest`
carries no connection, that bridge cannot leak a DSN, host, user, password, ssl
flag, or driver param into `infra/requirements.json`. The Go framework database
adapter already builds a `RequirementManifest` from its declared datasource and
emits the infra entry through this bridge during the build's describe phase.

## How the test provider uses this contract

The test provider is **not** part of this package — this package defines the
contract it reads. Two implementations realize it today:
`go.putnami.dev/database/testprovider` (Go) and `@putnami/database`'s `provision`
(TypeScript). For each named datasource a provider should:

1. Resolve the test binding.
2. Reuse or provision Postgres per `mode`.
3. Create the isolated database/schema per `isolation`.
4. Apply migrations from the workload's generated migration bundle using the
   existing migration machinery — the `go.putnami.dev/database` runtime
   library's `LoadBundleSources` / `ApplyBundle` in Go (these live in the
   runtime library, not this protocol package) and the `@putnami/migration` /
   `@putnami/database` equivalents — rather than inventing a separate mechanism.
5. Verify migration status/drift.
6. Configure `schema`/`search_path` from this protocol.
7. Return resolved bindings to the language adapter.
8. Tear down deterministically unless `reuse` keeps a template warm.

A caller can name the datasources its suite uses (`Options.Datasources` in Go,
`datasources` in TypeScript). The provider then runs these steps for those
entries only and returns a binding that carries only them; a name the binding
lacks follows `mode`, as a missing binding does. Both providers run the case
corpus in [`conformance/test-provider-datasources.json`](conformance/test-provider-datasources.json),
which pins that selection.

Performance expectations: do not start one container per test file; do not replay
migrations per test when a bundle-digest template can be reused; CI normally
provides Postgres as a service and passes a binding (Docker auto-provisioning is
a local convenience, not a CI requirement); unit tests pay no DB setup cost.

## Usage

```go
import database "go.putnami.dev/protocol/database"

m, diags := database.ParseAndValidateRequirementManifest(reqBytes)
if diag.HasErrors(diags) { /* reject */ }

b, diags := database.ParseAndValidateBinding(bindingBytes)
tb, diags := database.ParseAndValidateTestBinding(testBindingBytes)
```

All three shapes are strict-parsed (`DisallowUnknownFields`), validated against
the closed enums, the canonical datasource-name pattern, and the
exactly-one-transport connection rule, and guarded by JSON schemas
(`schemas/database.json`, `schemas/database-binding.json`,
`schemas/database-test-binding.json`) kept in lockstep with the Go types by
`drift_test.go`. A conformance fixture corpus under `fixtures/` pins the accepted
and rejected shapes.

## Non-goals

- **No driver coupling.** Postgres is the v1 engine; the package opens no
  connection and imports no driver.
- **No secrets in requirements.** A `RequirementManifest` cannot carry a
  `connection`.
- **No single-DSN test convention.** There is no `PUTNAMI_TEST_DB_DSN` protocol;
  a single DSN cannot model a multi-datasource workload. Any shorthand is CLI
  sugar that expands to this binding, not a protocol.
- **No startup schema application.** The protocol does not make `app.prepare()`
  create tables; migrations are the migration protocol's job.

## Producers and consumers

| Shape | Produced by | Consumed by |
| --- | --- | --- |
| `RequirementManifest` | a project's framework database adapter, during the build's describe phase | `infra.DatabasesFromManifest` (via `Project()`), for the deployer-facing requirement |
| `Binding` | a deployer or environment, after provisioning (e.g. the `DATABASE_BINDINGS` document) | `go.putnami.dev/database` and `@putnami/database` at runtime |
| `TestBinding` | a test runner or CI environment (`DATABASE_TEST_BINDINGS`) | `go.putnami.dev/database/testprovider` and `@putnami/database`'s `provision` |

The protocol package itself produces nothing at runtime: it parses, validates,
and projects.

## Versioning and compatibility

All three shapes carry `protocolVersion`, and readers accept exactly
`database.ProtocolVersion`; anything else is rejected with
`database.invalid_protocol_version` rather than partially understood. The enums
(`engine`, `mode`, `isolation`, `reuse`) are closed, so a value outside the set
is a diagnostic, never a pass-through. Adding an optional field inside an
existing shape is backwards compatible and does not bump the version; adding an
engine, a transport strategy, or a policy value changes what a conforming reader
must accept and does. Strict parsing (`DisallowUnknownFields`) is what makes
that promise enforceable in both directions: a producer cannot quietly ship a
field readers do not know, and the secret-free guarantee of a requirement holds
because the field it would need does not exist.

The JSON schemas under [`schemas/`](schemas) are kept in lockstep with the Go
types by `drift_test.go` (field parity, required fields, the pinned version, and
closed-enum parity), and the corpus under [`fixtures/`](fixtures) —
`requirement/`, `binding/`, `test-binding/`, each split into `valid/` and
`invalid/` — is the cross-language surface `conformance_test.go` drives.

## Durable decisions

- [`doc/adr/0001-one-database-contract-with-a-secret-free-projection.md`](doc/adr/0001-one-database-contract-with-a-secret-free-projection.md)
  — why this module owns the shared contract, why a connection declares exactly
  one transport, and why a requirement structurally cannot carry secrets.

## Product feature linkage

No user-facing product feature owns this module, and none should be minted for
it. It is an internal cross-language wire contract read by framework adapters, a
test provider, and deployers; what a developer experiences is
`database.NewPlugin({Datasource: "auth"})` or `sql({ datasource: 'auth' })`,
owned by the framework packages. Per the spec contract in
[`protocols/features`](../features/README.md) a spec details an already-authored
feature and never mints one, so the durable design intent lives in
[`doc/adr/0001-one-database-contract-with-a-secret-free-projection.md`](doc/adr/0001-one-database-contract-with-a-secret-free-projection.md)
instead. A product feature that later owns application data access links to that
record rather than restating it.

## Support

- **Status:** `stable`, recorded as
  `{"id": "go.putnami.dev/protocol/database", "kind": "protocol", "status": "stable"}`
  in the workspace-root [`putnami.support.json`](../../putnami.support.json)
  catalog (contract: [`protocols/support`](../support/README.md)).
- **Owner:** the Putnami maintainers, as repository owners of `protocols/`.
  Support entries carry no owner field, so the single owner is stated here.
- **Evidence:** both language adapters consume `Binding` in shipped code
  (`go.putnami.dev/database`, `@putnami/database`), both test providers consume
  `TestBinding`, `protocol/infra` consumes the projection through
  `infra.DatabasesFromManifest`, and the schemas, drift test, and valid/invalid
  fixture corpus pin all three shapes.

## Adoption

This package specifies the contract — the
`RequirementManifest`, `Binding`, and `TestBinding` shapes with strict parse +
validate, JSON schemas, drift and version guards, a conformance fixture corpus,
and the `Project()` projection. `protocol/infra` now consumes the projection via
`infra.DatabasesFromManifest`, making the database protocol the source of truth
for infra's database requirements. The Go runtime adapter (`go.putnami.dev/database`)
now consumes `Binding`: `database.NewPlugin(database.PluginConfig{Datasource: "auth"})`
resolves the named datasource from a deployer-injected `Binding` (the
`DATABASE_BINDINGS` env document), builds the pgx connection from its single
transport strategy, and applies the owning schema as the session `search_path`.
The TypeScript adapter (`@putnami/database`) consumes the same `Binding` for its
named datasources. The Go cross-language test provider
(`go.putnami.dev/database/testprovider`) consumes `TestBinding`: it provisions an
isolated database/schema per datasource (mode `require`), applies each
datasource's migrations, and returns a runtime `Binding`. With
`reuse: bundle-template` it applies migrations once into a template database
keyed by the migration-bundle digest and clones per-suite databases from it
(advisory-lock-guarded, deterministic teardown). The TypeScript test provider
(`@putnami/database` `provision`) consumes the same `TestBinding` with matching
semantics — isolation, per-datasource migration apply, `search_path`, and
`bundle-template` reuse — and injects the resolved runtime `Binding` so the
workload's `database(name)` reaches the isolated databases. Still planned: Docker
auto-provisioning (`mode: auto`).
