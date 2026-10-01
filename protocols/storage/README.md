# `go.putnami.dev/protocol/storage`

The Putnami **object/blob storage resource protocol**: the provider-neutral
contract a project uses to declare the storage it needs, and the binding a
deployer hands back so the workload can construct a storage backend.

This package is the cross-language source of truth for the storage *resource
and binding* contract. It is **declaration-only** — it has no dependency on a
storage backend or a cloud server, so a project, a deployer (e.g. Putnami
Cloud), and the storage libraries (`go.putnami.dev/storage`,
`@putnami/storage`) can all depend on it without a cycle.

## Why a dedicated protocol

Putnami already has storage *backends* at the framework level, but the project
protocol did not define a first-class storage *resource* contract. The
infra-requirements protocol (`go.putnami.dev/protocol/infra`) carries a thin
`storage` requirement (`{ name, access, public, retention }`) — enough for a
deployer to create the bucket, grant the runtime identity the needed role, and
aggregate the requirement across a dependency graph. Infra derives those entries
from this protocol through `infra.StoragesFromManifest`, the same bridge pattern
the database protocol uses to feed infra its `{ name, engine, schemas }` entries
while the secret-bearing connection stays in a binding. The rest of the rich
contract — isolation scope, signed-URL capability, and the binding a workload
receives — lives here: scope is a logical→physical resolution concern and the
signer is a workload-level grant, so neither belongs in the thin per-bucket
requirement.

## Three planes

The protocol deliberately separates three concerns and owns only the first:

- **Protocol (this package).** The declaration and binding *contract*: the
  `Manifest` and `Binding` shapes, their validation, and the closed enums. It
  names no provider and moves no object bytes.
- **Provisioning (a deployer, e.g. Putnami Cloud).** Reads the declared
  requirement — name, access, and public travel in the aggregated infra manifest
  (projected from this `Manifest`); scope and signed-URL intent stay in this
  contract — creates the provider resource (a GCS/S3 bucket, or a prefix within
  one), applies the IAM grants implied by `access` / `public` / `signedUrls`,
  and injects a `Binding`.
- **Data plane (the workload).** Constructs a backend from the `Binding` and
  reads/writes objects — and, when `signedUrls` is set, mints time-limited URLs
  so clients transfer bytes **directly** to the provider. Putnami never proxies
  the bytes.

## Manifest

A project's declared storage resources (`ParseAndValidateManifest`):

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-storage.json",
  "protocolVersion": 1,
  "resources": [
    { "name": "uploads", "access": "readwrite", "scope": "runtime", "signedUrls": true, "retention": "30d" },
    { "name": "public-assets", "access": "read", "scope": "workspace", "public": true }
  ]
}
```

| Field | Type | Meaning |
| ----- | ---- | ------- |
| `name` | resource name | Logical bucket identifier (the join key back from a binding). Required. |
| `access` | `read` \| `write` \| `readwrite` | Access level the workload needs; drives the IAM grant scope. |
| `scope` | `project` \| `workspace` \| `environment` \| `runtime` | Isolation boundary the resource is owned at. |
| `signedUrls` | bool | Workload needs signed-URL capability; the deployer must grant a signer identity. |
| `public` | bool | Objects need public (unauthenticated) read access. |
| `retention` | string | Free-form, deployer-defined retention (e.g. `30d`). |

`access` and `scope` are closed enums — an out-of-set value is rejected
deterministically (`storage.invalid_access` / `storage.invalid_scope`), and an
unknown key is rejected by strict parsing (`storage.unknown_field`). Every field
except `name` is optional.

### Isolation and scope

`scope` tells a deployer whether two workloads that name the same resource share
one provider bucket or get isolated ones:

- `project` — owned by a single project.
- `workspace` — shared across a workspace.
- `environment` — one per deploy environment (e.g. staging vs production).
- `runtime` — one per ephemeral runtime project, e.g. a per-PR preview. This is
  the boundary that keeps preview/runtime workloads from sharing a bucket with
  production; a deployer must never collapse a runtime-scoped resource into a
  shared global one.

### Lifecycle

The protocol declares intent; a deployer owns the lifecycle and documents its
exact rules. The contract's expectations are: **create** the resource when
first declared; **reuse** the same resource on redeploys of the same
`(scope, name)`; **retain** objects per `retention` rather than deleting them
implicitly; **delete** only on an explicit teardown of the owning scope (e.g. a
preview's runtime project); and **roll back** a failed provisioning without
destroying pre-existing data.

## Binding

The resolved backend coordinates a deployer injects after provisioning
(`ParseAndValidateBinding`):

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-storage-binding.json",
  "protocolVersion": 1,
  "name": "uploads",
  "backend": "gcs",
  "bucket": "acme-shared-prod",
  "prefix": "preview-42/uploads/",
  "identity": "workload",
  "signedUrls": true
}
```

| Field | Type | Meaning |
| ----- | ---- | ------- |
| `name` | resource name | The logical resource the workload requested. Required. |
| `backend` | string (open) | Provider-neutral backend kind to construct (`gcs`, `s3`, `file`, …). Open so the protocol is not coupled to a fixed provider set. Required. |
| `bucket` | string | Physical bucket/container the resource resolved to. Required. |
| `prefix` | string | Optional key prefix within the bucket — e.g. a per-scope isolation prefix. |
| `identity` | `static` \| `workload` | How the workload authenticates: static credentials, or an ambient workload identity. |
| `signedUrls` | bool | Whether signed-URL capability was granted. |

A binding gives the workload exactly what it needs to construct a backend
(`go.putnami.dev/storage` / `@putnami/storage`) and then talk to the provider
directly. The on-disk encoding of the binding (env vars, a config file) is the
deployer's concern; this protocol fixes only the fields it must convey.

## Capability model

- **Direct object access.** The default: the workload reads/writes objects
  through its backend using the `bucket`/`prefix`/`identity` in the binding.
- **Signed URLs.** When a resource sets `signedUrls`, the deployer grants a
  signer identity and the binding reports the capability; the workload then
  mints time-limited GET/PUT URLs so clients transfer bytes directly to the
  provider without routing them through the workload.

## Usage

```go
import storage "go.putnami.dev/protocol/storage"

m, diags := storage.ParseAndValidateManifest(manifestBytes)
if diag.HasErrors(diags) { /* reject */ }

b, diags := storage.ParseAndValidateBinding(bindingBytes)
```

Both shapes are strict-parsed (`DisallowUnknownFields`), validated against the
closed enums and the canonical resource-name pattern, and guarded by JSON
schemas (`schemas/storage.json`, `schemas/storage-binding.json`) kept in lockstep
with the Go types by `drift_test.go`. A conformance fixture corpus under
`fixtures/` pins the accepted and rejected shapes.

## Non-goals

- **No byte proxy.** Object bytes never flow through Putnami; the data plane is
  direct workload/client ⇆ provider.
- **No provider coupling.** GCS is the first implementation, but the protocol
  pins no GCS-specific names; `backend` is an open string.
- **No cross-provider replication or migration** in this version.
- **No storage class/tier** yet (standard vs infrequent-access vs archive). When
  added it will be an optional, additive field with a provider-neutral enum, so
  it will not bump `protocolVersion`.

## Producers and consumers

| Shape | Produced by | Consumed by |
| --- | --- | --- |
| `Manifest` | a project declaring the buckets it needs, at build time | `Manifest.Project()` → `infra.StoragesFromManifest`, which puts `{name, access, public, retention}` into the deployer-facing infra requirement |
| `Binding` | a deployer or environment, after provisioning | `go.putnami.dev/storage`, which parses the bindings document and constructs a bound backend that resolves each logical name to its bucket and prefix |

Adoption is not symmetric today, and the difference matters when reading this
contract:

- **Go** consumes both halves: `go.putnami.dev/storage` builds `Manifest`
  resources for the infra projection at build time and constructs backends from
  a parsed `Binding` at runtime.
- **TypeScript** consumes the *projection* half: `@putnami/storage` emits the
  same per-project infra requirement — byte-identical to the Go producer, pinned
  by `protocols/infra/fixtures/equivalence/storage.golden.json` from both sides —
  but constructs its backends from framework configuration
  (`storage.backend`, endpoint, credentials) rather than from a protocol
  `Binding`.

A workload's bucket needs therefore travel with its declared infra requirements
instead of living in operator memory: a deployer reading the aggregated infra
manifest creates the bucket and grants the runtime identity the needed role, the
same way it provisions a database. Provisioning (resource creation, IAM binding,
config injection, lifecycle, rollback) stays the deployer's to implement.

## Versioning and compatibility

Both shapes carry `protocolVersion` and readers accept exactly
`storage.ProtocolVersion`; anything else is rejected with
`storage.invalid_protocol_version`. `access` and `scope` are closed enums, so an
out-of-set value is a deterministic diagnostic rather than a pass-through, while
`backend` is deliberately an **open** string so the contract is not coupled to a
fixed provider set. Adding an optional field to an existing shape is backwards
compatible and does not bump the version — as the storage-class non-goal above
records; adding a scope, an access level, or a required field does.

The JSON schemas in [`schemas/`](schemas) are kept in lockstep with the Go types
by `drift_test.go`, and the corpus under [`fixtures/`](fixtures)
(`manifest/{valid,invalid}`, `binding/{valid,invalid}`) is the cross-language
surface `conformance_test.go` drives.

## Product feature linkage

No user-facing product feature owns this module, and none should be minted for
it. It is a declaration contract between a project, a deployer, and the storage
libraries; what a developer experiences is `storage()` / bucket APIs in
`go.putnami.dev/storage` and `@putnami/storage`. Per the spec contract in
[`protocols/features`](../features/README.md) a spec details an already-authored
feature and never mints one. This module records no ADR of its own: its one
structural decision — the rich contract stays here while infra carries a thin,
secret-free projection — is the same rule recorded for the sibling database
contract in
[`protocols/database/doc/adr/0001-one-database-contract-with-a-secret-free-projection.md`](../database/doc/adr/0001-one-database-contract-with-a-secret-free-projection.md),
and duplicating it here would create two records of one decision.

## Support

- **Status:** `preview`, recorded as
  `{"id": "go.putnami.dev/protocol/storage", "kind": "protocol", "status": "preview"}`
  in the workspace-root [`putnami.support.json`](../../putnami.support.json)
  catalog (contract: [`protocols/support`](../support/README.md)).
- **Owner:** the Putnami maintainers, as repository owners of `protocols/`.
  Support entries carry no owner field, so the single owner is stated here.
- **Evidence:** `go.putnami.dev/storage` consumes both the `Manifest` (build
  time, through `infra.StoragesFromManifest`) and the `Binding` (runtime backend
  construction) in shipped code; the infra projection is byte-identical across
  Go and TypeScript against a shared golden; schemas, drift test, and the
  valid/invalid fixture corpus pin both shapes.
- **Why not `stable`:** the `Binding` half has exactly one implementation.
  TypeScript still builds backends from framework configuration, and no in-repo
  deployer injects a binding, so the shape has not yet been exercised by a
  second producer — which is the review a stable commitment implies.
