# protocol/gomod

The Putnami Go module registry **write** protocol: `gomod-write/v1`.

## Why

Public Go modules are "published" by pushing a VCS tag: the module proxy reads
the tag and everything downstream follows. That mechanism cannot express two
things a private workspace needs — authenticating the publisher, and routing a
version to a release channel (`dist_tag`) without inventing a tag for it.

This module defines the smallest authenticated upload that adds those two
capabilities **and nothing else**, so the read path stays the standard Go module
proxy protocol and `go mod download` keeps working unmodified.

## What

The base publish uses two write endpoints, in order. The protocol also defines
a third, idempotent public-release operation:

```
POST /{module}/-/blobs/upload     (Content-Type: application/zip)
     <zip bytes>   → 201 {"digest": "sha256:…"}
PUT  /{module}/@v/{version}        (Content-Type: application/json)
     {"go_mod": "…", "zip_digest": "sha256:…", "dist_tag": "latest"}
     → 201
POST /{module}/@v/{version}/release
     → 200 {"module":"…","version":"…","visibility":"public"}
```

Reads use the **standard** Go module proxy protocol
(`GET /{module}/@v/{version}.info`, `.mod`, `.zip`), which this module does not
model at all — there is nothing bespoke to describe.

| Symbol | Meaning |
|--------|---------|
| `BlobUploadPath` / `VersionPath` / `ReleasePath` | The three endpoint path templates |
| `BlobContentType` / `VersionContentType` / `ReleaseContentType` | `application/zip` and `application/json` |
| `BlobUploadResponse` | `{digest}` — the content address the registry assigned the zip |
| `PublishVersionRequest` | `{go_mod, zip_digest, dist_tag?}` — the publish body |
| `ReleaseVersionResponse` | `{module, version, visibility}` — exact acknowledgement of a public release |
| `ValidErrorCodes` | The closed `gomod.*` diagnostic taxonomy |

The upload is **content-addressed and two-phase on purpose**: the registry
returns the digest it computed for the bytes it actually stored, and the publish
body echoes that digest back. A publisher therefore cannot bind a version to
bytes the registry never received, and a truncated upload fails at the blob step
rather than producing a version that resolves to a corrupt zip.

A digest is `sha256:` followed by exactly 64 lowercase hex characters. Uppercase
hex, a short digest, and a missing algorithm prefix are all rejected — see
[`fixtures/blob-upload-response/invalid/bad-digest.json`](fixtures/blob-upload-response/invalid/bad-digest.json).

`dist_tag` is optional and **unconstrained by this module**: channel naming is a
registry policy, so validation would put the same vocabulary in two places. When
absent it is omitted from the JSON rather than sent as `""`, which
[`conformance_test.go`](conformance_test.go) pins.

`POST …/release` is additive within v1 because it changes neither existing
endpoint nor message. It is an authenticated, idempotent one-way operation on
an already-published version. A successful response must name the exact module
and version and report `visibility: "public"`; clients reject unknown fields,
missing identifiers, any other visibility, and any mismatch with the request.
The operation never raises a package's public visibility ceiling, so a private
ceiling is a hard refusal rather than an implicit policy change.

The current framework publisher deliberately does not call that operation.
Full/bootstrap and sparse plans are coordination inputs, not visibility grants;
all current Go publication remains private and authenticated. Public release is
dormant until a separate server-owned attestation can authorize it explicitly.

## Producers and consumers

**Client (in this repository)** —
[`go/extension/internal/jobs/publish/gomodule.go`](../../go/extension/internal/jobs/publish/gomodule.go)
is the only consumer: it uploads the zip, PUTs the version, authenticates exact
`.zip` and `.mod` reads, and runs a private `go mod download` smoke against the
origin with public proxy and SumDB fallback disabled. That subprocess also
removes ambient proxy, preload, agent, and Putnami capability variables before
receiving its single authenticated origin. It does not infer public
visibility from a release-set plan or repository input. Its bearer comes from the
[`registry`](../registry/README.md) credential seam, not from this module or URL
userinfo; credential-bearing registry URLs are rejected before they can reach
logs, errors, or dry-run output. The publisher requires HTTPS except for an
explicit loopback development registry.

**Server** — implemented by `@putnami/cloud` in a separate repository. The
`go_mod` / `zip_digest` / `dist_tag` field names are owned by that server and
pinned here so neither side can rename them alone.

This is a **proprietary** contract: only a Putnami Go module registry implements
the write side. The read side is the public Go module proxy protocol and needs
no Putnami-specific client.

## Versioning and compatibility

`ProtocolVersion` is `1` and is pinned by
[`conformance_test.go`](conformance_test.go), which also pins all path
templates, both content types and all closed JSON field names. A bump means a
wire-visible change that would break an existing client or server, and requires
a migration story rather than an edit.

The compatibility rules that matter across the repository boundary:

- **Field names are frozen** within v1. `go_mod`, `zip_digest`, `dist_tag`,
  `module`, `version`, and `visibility`
  use snake_case because the server owns them; matching Go field names to Go
  conventions would silently break the server.
- **Strict decoding is intentional.** All messages reject unknown fields, so a
  server that starts sending a new member does not get silently ignored by an
  old client — it gets a `gomod.invalid_blob_upload` /
  `gomod.invalid_publish` / `gomod.invalid_release` diagnostic naming the mismatch.
- **Omission is contract.** `dist_tag` omitted means "no channel routing"; an
  empty string is not the same statement and is not emitted.
- **The read path is not versioned here.** Because reads are the standard proxy
  protocol, a `gomod-write/v2` would not change how anything downloads.

## Schemas and fixtures

This module has no JSON Schema: all messages are small, closed Go structs, and
the strict decoder plus the adjacent validators are the executable contract.
The corpus is:

- [`fixtures/blob-upload-response/valid/`](fixtures/blob-upload-response/valid) — `digest.json`
- [`fixtures/blob-upload-response/invalid/`](fixtures/blob-upload-response/invalid) — `bad-digest.json`, `unknown-field.json`
- [`fixtures/publish-version-request/valid/`](fixtures/publish-version-request/valid) — `full.json`, `no-disttag.json`
- [`fixtures/publish-version-request/invalid/`](fixtures/publish-version-request/invalid) — `bad-digest.json`, `missing-gomod.json`, `unknown-field.json`
- [`fixtures/release-version-response/valid/`](fixtures/release-version-response/valid) — `public.json`
- [`fixtures/release-version-response/invalid/`](fixtures/release-version-response/invalid) — missing fields, private visibility and an unknown field

`TestConformance_Fixtures` runs every file: `valid/` must produce no error
diagnostics, `invalid/` must produce at least one.

## Support status, owner, and evidence

| | |
|---|---|
| **Status** | `preview` — see the `go.putnami.dev/protocol/gomod` entry in the workspace-root [`putnami.support.json`](../../putnami.support.json), which is the only authority for this value |
| **Owner** | The `protocols` scope (`protocols/putnami.json`). This module is the sole authority for the write wire; the publish job may not spell an endpoint, content type or field name of its own. |

Evidence behind `preview`:

- `ProtocolVersion`, all endpoint templates, both content types and all closed
  wire field names are pinned by conformance tests;
- a valid/invalid fixture corpus exists for all messages and is executed on
  every run;
- there is one real producer in this repository, wired end to end through to a
  `go mod download` smoke test.

Evidence still missing for `stable`:

- the server is proprietary and lives in another repository, so no test here
  covers a real exchange;
- there is exactly one consumer, so the contract has never been proven against a
  second, independently written client.

## Product feature and durable decisions

**No user-facing feature is declared for this module, deliberately.** The user
outcome is "publish a private Go module to a channel", and that outcome is owned
by the publish command surface of `@putnami/cli` and the Go extension, not by a
wire contract. Declaring a feature per protocol module would mint one artificial
product identity per technical boundary, which the
[feature protocol's authoring boundary](../features/README.md) explicitly warns
against. With no feature to detail, there is no spec: a spec details exactly one
already-authored feature and never mints one.

Durable decisions:

- [ADR 0001 — Bespoke authenticated write, standard proxy read](doc/adr/0001-bespoke-write-standard-read.md)
