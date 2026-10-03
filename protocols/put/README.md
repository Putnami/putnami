# protocol/put

The Putnami Put registry **immutable write** protocol: `put-write/v1`.

## Why

A release set names Put registry members: release archives, config members,
migrations and site-content bundles. Under `publication-v1`
([registry ADR 0003](../registry/doc/adr/0003-publication-ops-behind-a-negotiated-capability.md)),
the engine uploads every managed member itself, with a publish bearer no
repository process holds, and only the provider's `release` moves a channel.
The engine, the extension SDK and the Put registry therefore need one
description of the write: which requests, which media types, which digest.

## What

A publish uploads the blobs a version references, then publishes the version's
manifest without a channel. Paths are relative to the registry base URL whose
downloads are `{base}/{namespace}/{package}/download`:

```
POST {base}/{namespace}/{package}/blobs          (Content-Type: the blob media type)
     <blob bytes>  → 201 {"id","digest","size","media_type","created_at"}
POST {base}/{namespace}/{package}/publish        (Content-Type: application/json)
     {"version","media_type","payload"}
     → 201 {"package","version":{…},"manifest":{…},"channel":null}
     → 409 when the version is already published
GET  {base}/{namespace}/{package}/versions/{version}/manifest
     → 200 {"id","package_id","media_type","payload","created_at"}
```

| Symbol | Meaning |
|--------|---------|
| `BlobUploadPath` / `PublishPath` / `ManifestPath` | The three endpoint path templates; `ManifestPath` path-escapes the version |
| `Profiles` / `ProfileFor` | The manifest media type and the blob media types of each member kind |
| `BlobReceipt` | The stored blob a blob upload answers |
| `PublishRequest` | `{version, media_type, payload}`, with no channel |
| `PublishResponse` | The published private version, its manifest, and `channel: null` |
| `Manifest` | A stored manifest, as a publish answers it and a manifest read returns it |
| `ArchivePayload` | The manifest payload of an archive member and its platforms |
| `SplitCoordinate` / `ValidVersion` | The coordinate and version rules |
| `ValidatePayload` / `Digest` | The canonical payload rule and the member digest |
| `BlobReferences` | The blob digests a manifest payload references |
| `ValidErrorCodes` | The closed `put.*` diagnostic taxonomy |

### Member kinds and media types

| Kind | Manifest media type | Blob media types |
|------|--------------------|------------------|
| `config` | `application/vnd.putnami.config.authored-member+json` | none |
| `migration` | `application/vnd.putnami.data.migration.v2+json` | `application/vnd.putnami.migration-bundle.v1.tar` |
| `doc` | `application/vnd.putnami.sitecontent.bundle+json` | `application/gzip` |
| `archive` | `application/vnd.putnami.archive+json` | `application/gzip`, `application/octet-stream` |
| `deployment` | `application/vnd.putnami.infra.deployment.v2+json` | none |

An `archive` blob is `application/gzip`, except the CLI's own release, whose
platform assets may be raw executables (`application/octet-stream`, CLI
decision D-W2). A release archive is a member of ecosystem `archive`; a config, migration,
doc or deployment member is a member of ecosystem `put`. Both live on the Put registry.

A `deployment` manifest is a workload's deployment declaration: the canonical
bytes `infra.MarshalDeployment` writes (`go.putnami.dev/protocol/infra`), which
are already in this registry's canonical payload form. The media type names the
infra protocol version, so a new version is a new media type.

### Coordinates and versions

A coordinate is `<namespace>/<package>`. Each segment is 1 to 255 lowercase
letters, digits, `.`, `_` and `-`, and starts with a letter or a digit
(`SplitCoordinate`). A version is 1 to 256 bytes with no surrounding space and
no `/`, NUL, CR or LF, and is not `.` or `..`: the manifest read names the
version as one path segment (`ValidVersion`).

### The digest

The member digest of a version is `sha256:` and the hex SHA-256 of the
manifest payload the registry stores. The registry stores a payload as Go's
`encoding/json` writes it for a `json.RawMessage`: compact, with `<`, `>`,
`&`, U+2028 and U+2029 escaped. A payload already in that form is stored byte
for byte, so `ValidatePayload` refuses any other form and a publisher knows the
digest before it uploads. The payload is one JSON object of at most 4 MiB of
valid UTF-8 with no duplicate member.

### Blob references

The registry links a blob to the package only through a published manifest
that references it: `blob_digest`, `artifact.blob`, or the `digest` of each
member of `artifacts`. `BlobReferences` reads exactly those. A payload that
names one of those members in another case, such as `BLOB_DIGEST` or
`artifact.Blob`, is refused: a reader that ignores case, as Go's
`encoding/json` does, would take it for the reference, and a reader that
matches names exactly would not. A publisher uploads exactly the blobs its
manifest references, and checks each receipt (`ValidateBlobReceiptFor`) names
the uploaded digest and size. The registry stores one blob per digest and
answers the media type of the first upload of those bytes, so a receipt's
media type may differ from the upload's.

An archive payload is `{"artifacts":{"<os>-<arch>":{"digest","size"}}}`, 1 to
32 platforms; two platforms may share a blob. `ArchivePayload.Platforms` maps
each platform to `<os>/<arch>`, the release set's platform key.

### No channel

The publish request carries no `channel`, `visibility` or `source_ref`: the
version is private and immutable, and no channel moves. A `channel`,
`visibility` or `source_ref` member is an unknown field. A publish answer whose
`channel` is not `null` is refused.

### Conflict

A 409 means the version is already published. The publisher reads the stored
manifest back with its bearer and accepts the version only when the media type
and the payload bytes equal its own (`ValidateManifestFor`): an immutable
version that holds anything else is another publisher's version.

## Producers and consumers

**Client (in this repository)** — `tooling/extension-sdk/putpublish` sends
every request; the engine's publication upload node
(`tooling/cli/internal/jobs`) calls it for every `put` and `archive` member a
publication job packed into its outbox
([extension protocol](../extension/README.md)). The bearer is the provider's
publish credential for the registry host, and no error carries it.

**Server** — the Put registry, implemented by `@putnami/cloud` in a separate
repository. Its field names are pinned here so neither side can rename them
alone.

## Versioning and compatibility

`ProtocolVersion` is `1` and is pinned by
[`conformance_test.go`](conformance_test.go), with the path templates, the
media types of each kind, the closed field names, the error codes and the
bounds. Every message decodes strictly: an unknown member, a member named in
another case than its field (`Digest` for `digest`), a duplicate member and
trailing data are refused. Adding a field to any message is a v2 change.

## Schemas and fixtures

- [`schemas/put-write-v1.json`](schemas/put-write-v1.json) describes every
  message; its description names the rules it cannot express.
  `TestConformance_SchemaTracksTheGoTypes` holds its members to the Go types.
- [`fixtures/`](fixtures) holds `valid/` and `invalid/` lines for
  `blob-receipt`, `publish-request`, `publish-response`, `manifest` and
  `archive-payload`. Every fixture is one compact line, so a payload inside it
  is in canonical form. `TestConformance_Fixtures` runs every file.

## Support status, owner, and evidence

| | |
|---|---|
| **Status** | `preview` — see the `go.putnami.dev/protocol/put` entry in the workspace-root [`putnami.support.json`](../../putnami.support.json), which is the only authority for this value |
| **Owner** | The `protocols` scope (`protocols/putnami.json`). The publish upload may not spell an endpoint, media type or field name of its own. |

Evidence:

- the version, the endpoint templates, the media types, the field names and
  the error codes are pinned by conformance tests;
- a valid and invalid fixture corpus runs on every test run;
- one client in this repository, exercised end to end by the publication
  custody test against an in-process registry.

Missing: the server lives in another repository, so no test here covers a
real exchange.

## Product feature and durable decisions

No user-facing feature is declared for this module: the outcome, publishing a
release set, belongs to the publish command of `@putnami/cli`.

- [ADR 0001 — Immutable write without a channel](doc/adr/0001-immutable-write-without-a-channel.md)
