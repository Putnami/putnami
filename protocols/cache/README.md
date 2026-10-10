# `go.putnami.dev/protocol/cache`

Wire contract for the Putnami **remote build cache**: the batch *negotiate*
exchange between the build-cache client and the cloud cache server, plus the
Action Cache + CAS data shapes both sides agree on. It also defines the
**provider RPC** (below) the build scheduler speaks to an out-of-process cache
provider, so the same module is the single source of truth for both the HTTP
wire types and the provider RPC, imported by the CLI and by the cache-provider
extension.

This package is the source of truth for the Go side and the reference shape for
an implementation in any other language. It is **transport-only** — it has no
dependency on the CLI store or the cloud server, so both can depend on it
without a cycle.

## Decisions baked in

- **Wire format:** plain HTTP + JSON. Types are hand-maintained and guarded by
  strict parsing (`DisallowUnknownFields`), validation, and conformance
  fixtures — matching the house style of the other `protocol/*` modules.
- **Auth:** per-user **bearer token** in the `Authorization` header
  (`AuthorizationValue` / `BearerToken`). No shared secrets; the server resolves
  identity and the cache namespace from the token's claims.

## Model

- **Keys** are opaque, content-addressed `sha256` hex strings the client
  computes from each task's inputs. They are deterministic and computable for
  the whole build DAG *before* execution, so the entire key set is negotiated in
  **one round trip**. The server treats keys as opaque and never recomputes
  them.
- The store is split into an **Action Cache** (`key → ActionResult + Manifest`)
  and a **CAS** (`digest → bytes`). A `Manifest` lists a task's output files,
  each with a content `Digest`, so bytes dedup across keys and are fetched
  lazily.
- Bytes move over **presigned URLs** issued by the server. On a hit the
  *negotiate* response carries `GET` URLs to download CAS blobs. On a miss the
  client uses the post-build *store/commit* exchange (below) to obtain `PUT`
  URLs for the blobs the server is missing — digests already present in CAS are
  omitted from the upload set (dedup).
- **CAS blobs may be stored gzip-compressed.** The client compresses a blob on
  upload when that shrinks it; the `Digest` is always the **uncompressed**
  content hash, so compression is invisible to addressing and dedup, and raw and
  gzipped objects for different blobs coexist. No `Content-Encoding` is set (to
  avoid object-store transcoding) — any reader of CAS bytes must therefore
  decompress a blob that begins with the gzip magic (`1f 8b`) before use, and
  verify the result against the (uncompressed) digest. The server stores blobs
  opaquely and never reads their contents, so it is unaffected.

## Negotiate

`POST` `/v1/cache/negotiate` (see `NegotiatePath`).

Request (`NegotiateRequest`): the materialization `Mode` plus every cacheable
`KeyRequest`. Advisory fields (`extension`, `task`, `project`, `durationMs`,
`sizeBytes`) drive server-side reporting and are not part of cache identity.
Remote eligibility reads only `task` (see below).

Response (`NegotiateResponse`): one `KeyResult` per key with `hit`, and on a hit
the `ActionResult`, `Manifest`, and any `Downloads`.

`ActionResult` can also carry compact cached JSONL `events` (`meta`, `metric`,
`artifact`, `diagnostic`, and `summary`) so a cache hit can restore the same
inline status summary as the original run: test counts, coverage, generated
files, artifacts, warning counts, and visible summaries. These events are status
metadata, not streaming output; logs, phases, and progress should not be stored.

## Successful run markers

Remote successful-run markers are separate from the Action Cache. They record
that a whole requested target completed successfully at one commit; per-job
cache hits never imply this.

- `POST` `/v1/cache/run-marker/lookup` (see `RunMarkerLookupPath`). Request
  (`RunMarkerRequest`): opaque workspace identity, branch, normalized command
  list, optional parameter hash, and selection scope (`all`). Response
  (`RunMarkerResponse`): an optional `RunMarker` with the successful HEAD SHA.
- `POST` `/v1/cache/run-marker/publish` (see `RunMarkerPublishPath`). Request
  (`PublishRunMarkerRequest`): the same key fields plus the successful `sha`
  and optional `observedSha` that the client selected from before the run, so
  servers can implement compare-and-swap and avoid stale late writers. Response
  (`PublishRunMarkerResponse`): whether the marker advanced.

Clients use these markers as a best-effort CI baseline for smart bare commands
on `main`/`master`: local marker first, remote marker fallback when remote cache
is active, then all projects if the marker is missing, stale, invalid, or
unavailable.

## Store / commit (write path)

Negotiate runs *before* the build, so on a first-build miss the server cannot
know the artifact's digests yet and cannot pre-issue upload URLs. The write path
is therefore a separate **post-build** exchange, in two steps:

1. `POST` `/v1/cache/store` (see `StorePath`). Request (`StoreRequest`): the
   `key` and the built `Manifest`. Response (`StoreResponse`): `Uploads` —
   presigned `PUT` `BlobTransfer`s for **only** the digests not already in CAS
   (dedup). An empty set means every blob is already present.
2. The client uploads each missing blob over its presigned URL, then
   `POST` `/v1/cache/commit` (see `CommitPath`). Request (`CommitRequest`): the
   `key`, `ActionResult`, and `Manifest`. Response (`CommitResponse`):
   `committed`.

`CommitRequest` carries the full entry, so commit is **self-contained and
idempotent**: re-committing a key is a no-op, and the server can confirm the
referenced blobs are in CAS before making the entry servable. Presigned `PUT`
URLs are self-authenticating — the per-user bearer token is sent on store/commit
but **not** on the uploads themselves.

The store/commit write path keeps the wire `protocolVersion` at `1`. Optional
action-result events are capability-gated below because strict parsers must know
the field before clients send it.

## Batched write path & capabilities (v1.x, additive)

store/commit finalize one key per round trip, so a build with `M` built misses
spends `~2M` control round trips, all drained at the end. These additive
endpoints collapse that **without changing `protocolVersion`** — they are new
routes, never new fields on the existing messages, so a strict
(`DisallowUnknownFields`) parser on either side keeps working.

- `GET` `/v1/cache/capabilities` (see `CapabilitiesPath`). Response
  (`CapabilitiesResponse`): the `Capabilities` the server supports beyond the v1
  baseline. The client probes once; a `404`/non-2xx means "baseline only" and it
  uses store/commit unchanged. Capability strings: `find-missing` /
  `commit-batch` (the batched write path), `direct-cas-put` (the Phase 2
  direct-write accelerator below), `upload-batch` (small-blob coalescing below),
  `download-batch` (its read-path analog below), and `action-events` (the
  additive cached-events field below).
- `POST` `/v1/cache/find-missing` (see `FindMissingPath`). Request
  (`FindMissingRequest`): the deduped `BlobRef` set for a whole window of built
  misses. Response (`FindMissingResponse`): `Uploads` — presigned `PUT`s for only
  the blobs not already in CAS. The batched analog of `store`.
- `POST` `/v1/cache/commit-batch` (see `CommitBatchPath`). Request
  (`CommitBatchRequest`): N `CommitEntry`s (each a self-contained `key` +
  `result` + `manifest`). Response (`CommitBatchResponse`): one `CommitResult`
  per entry. The batched analog of `commit`; idempotent per entry.

The client keeps uploads **streaming per-job** (overlapping the build) and only
coalesces the control round trips into per-window batches, so the end-of-build
drain holds just the final window rather than `M` jobs' worth of sync.

### Cached action events

`ActionResult.events` is an additive field on the existing action result shape.
Because both sides use strict JSON parsing, clients should send it only when the
server advertises `action-events` in `CapabilitiesResponse.Capabilities`. Servers
that support the field should persist it with the Action Cache entry and return
it on negotiate hits. Clients may restore returned events regardless of mode;
materialization mode controls output bytes, not result metadata.

### Direct-write accelerator (Phase 2)

When the server advertises `direct-cas-put`, the client fetches one
`GET` `/v1/cache/upload-grant` (`UploadGrant`) and then PUTs built blobs
**straight to CAS**, skipping `find-missing` entirely (`commit-batch` still
finalizes). The grant is backend-agnostic: it carries the `URLTemplate` (with a
`{digest}` placeholder the client substitutes), `Method`, the `Headers` to send
verbatim (credential + a conditional-create header), and `ConditionalExistsStatus`
— the status the store returns when the blob already exists, which the client
treats as a dedup skip. It's a pure accelerator: no grant (or a failed fetch) →
the client uses `find-missing`.

Integrity uses a **trust model**: the client content-addresses correctly by
construction, and the CAS is per-workspace, so a client bug can only corrupt that
workspace's own cache — the same trust the presigned `find-missing` upload
already relies on. The server does **not** re-validate content. (The original
staging + validate-and-promote design needed a HEAD-readable SHA256 to validate
cheaply; GCS lacks it, so that stricter model is not used here.)

### Small-blob coalescing (`upload-batch`)

A find-missing/direct upload is one request per blob; a build that emits hundreds
of sub-kB files pays per-request overhead that dwarfs the bytes. When the server
advertises `upload-batch`, the client coalesces several small blobs into one
`POST` `/v1/cache/upload-batch` (`UploadBatchRequest`) — the analog of Bazel's
`BatchUpdateBlobs` — carrying each blob's bytes inline (`InlineBlob.Data`, base64;
it may be gzipped, addressed by the uncompressed `Digest`). The response
(`UploadBatchResponse.Stored`) lists the digests now in CAS; anything missing
falls back to the per-blob path. Blobs over `MaxInlineBlobBytes` always use the
per-blob path, bounding request size. Unlike the presigned paths the server here
*has* the bytes, so it may validate (decompress + hash) before storing, though
the trust model does not require it.

### Small-blob coalescing (`download-batch`)

The read-path mirror of `upload-batch`, and the reason it matters: a negotiate
hit lists one presigned `GET` per CAS blob, so materializing a build that emitted
hundreds of tiny files (a TypeScript build's sub-kB `.d.ts` fan-out) is bound by
per-blob round trips, not bytes. When the server advertises `download-batch`, the
client groups the small blobs (those whose `BlobTransfer.SizeBytes` is within
`MaxInlineBlobBytes`) and fetches them in one `POST` `/v1/cache/download-batch`
(`DownloadBatchRequest` = the digest set). The response
(`DownloadBatchResponse.Blobs`) returns each blob inline (`InlineBlob.Data`,
base64; it may be gzipped, addressed by the uncompressed `Digest`, so the client
verifies by content-addressing exactly as for a presigned download). A digest the
server omits (evicted, never present) is not an error — the client materializes
that blob from its presigned `GET` — so this is a pure accelerator. Large blobs
keep using their presigned `GET`, bounding request size.

## Provider RPC (out-of-process cache provider)

`provider.go` / `provider_strict.go` define a second, orthogonal contract: the
request/response RPC the build scheduler (core) speaks to a long-lived
**cache-provider subprocess**, which owns the HTTP client above. It is distinct
from the HTTP wire contract and from the one-way `protocol/runtime` event
stream.

- **Selection.** Whichever loaded extension declares the reserved
  `ProviderCommandName` (`cache-provider`) command serves it. That is the whole
  rule: any extension may, there is no allowlist and no extension-version floor,
  and two declarers is an error rather than a discovery-order coin flip.
  `@putnami/cloud` is the reference implementation, not a privileged one. The
  reasoning, including the product-name check and the version floor that were
  removed, is recorded in
  [`doc/adr/0001-provider-selection-by-reserved-command.md`](doc/adr/0001-provider-selection-by-reserved-command.md).

- **Transport — hybrid.** Control messages are JSONL request/response over the
  subprocess stdio: one `ProviderRequest` per stdin line, one `ProviderResponse`
  per stdout line, correlated by `id` (responses may return out of order).
  **Large blobs never cross the pipe** — they move through a content-addressed
  blob-exchange directory named in `InitializeParams.BlobExchangeDir`
  (`BlobExchangePath(exchangeDir, digest)` = `<exchangeDir>/<hex[:2]>/<hex>`). So
  `restore` downloads a hit's blobs into the exchange dir and returns only the
  `Manifest`; `upload` reads blobs core exported there. The exchange dir is a
  provider↔core handoff, **not core's live CAS**: core stays the sole owner of its
  store and ingests/exports between the two, so the provider never has to
  replicate the store's layout, locking, or GC across the repo boundary. The
  alternatives considered — inline blobs on the pipe, provider writes into the
  CAS, a side channel — are recorded in
  [`doc/adr/0002-blob-exchange-directory-boundary.md`](doc/adr/0002-blob-exchange-directory-boundary.md).
- **Version and compatibility.** `ProviderProtocolVersion` is separate from the
  HTTP `ProtocolVersion`; a single `@putnami/cloud` build speaks both. Provider
  RPC v2 adds cache-entry provenance but its strict parsers accept legacy v1
  envelopes, so channel-less providers continue to work. The CLI always starts
  `initialize` with a v1 envelope and payload, and advertises
  `provider-protocol-v2` through the already-tolerant capabilities list. A v2
  provider may select v2 in `InitializeResult`; a v1 provider ignores the
  unknown capability and selects v1. The session uses that selected version for
  every later request, and the provider echoes the bootstrap version in its
  initialize response envelope. A provider emits the new fields only for a v2
  negotiation; an absent channel is deliberately a legacy signal for the CLI's
  local trust policy, not an implicit trusted entry.
- **Entry provenance.** `upload` accepts `producer` (`ci` or `developer`), an
  opaque `producerIdentity`, and `channel` (`trusted` or `hint`); these are
  syntactically validated but **untrusted assertions**. The provider/cache
  server derives and overwrites all three values from authenticated identity
  before persistence, so a developer client cannot self-declare CI. On a
  restore hit, the provider returns the persisted, provider-authoritative triple
  for the CLI's trust policy. V1/channel-less results omit all three fields.
- **Ops.** `initialize` (capabilities + known-digests handshake, returns the
  provider version for the gate), `authenticate`, `prefetch`, `restore` (`hit` /
  `miss` / `error` — the restore-failure signal), `upload`, `marker-lookup`,
  `marker-write`, `object-get`, `object-put`, `summary`, `shutdown`.
- **Run credential.** A core that holds a hosted run's credential lists
  `run-credential` in the bootstrap `InitializeParams.Capabilities`; a core
  without one never does, so a local run's `initialize` is unchanged. A provider
  that echoes it receives one `authenticate` (`AuthenticateParams{credential}`,
  empty result), right after `initialize` and before any other op. Without the
  echo core sends nothing. A refused `authenticate` ends the session like a
  failed `initialize`. The credential is 1 to 16384 bytes of UTF-8 with no
  whitespace (`ValidRunCredential`, the rule of
  `registry.ValidRunCredential`); the provider keeps it in memory only, and
  `AuthenticateParams` and `ProviderRequest` format it as redacted. The op is
  not valid on the object-cache socket. Like the batched HTTP endpoints, it is
  a new op behind a capability, never a new field on an existing message, so a
  strict provider that ignores the capability keeps working.
- **Result-only restore.** Core lists `restore-result-only` in the bootstrap
  `InitializeParams.Capabilities`. A provider that echoes it may receive
  `RestoreParams.resultOnly` and `PrefetchParams.resultOnlyKeys`; a provider
  that does not echo it never receives either field, so its strict parser sees
  the wire it already accepts. `resultOnly: true` means core reads the hit's
  result and none of its files: the provider still answers `hit` with the
  `Result`, the full `Manifest`, and the provenance fields, but it need not
  place any blob in the exchange directory. `resultOnlyKeys` is the subset of
  `keys` core will restore that way, so a prefetch need not download their
  blobs; each one is a valid key, listed once, and also listed in `keys`.
  Core does not verify a result-only hit's entry descriptor, because it reads
  no blob: it cannot check that the entry records the requested key, as it does
  on a full restore. The provider must answer with exactly the entry stored
  under the requested key, or with a miss.
- **Object cache (socket).** `object-get` / `object-put` are a generic,
  language-neutral object cache: small opaque payloads addressed by
  `(namespace, id)`, batched, with bytes travelling through the blob-exchange
  directory. Their caller is usually a JOB process rather than core, so a
  provider that serves them also listens on a local Unix socket and returns its
  absolute path in `InitializeResult.ObjectCacheSocket`; core exports that path
  to every job. Negotiation is two-sided: core lists `object-cache` in
  `InitializeParams.Capabilities` and the provider echoes it *and* sets the
  path, so neither side can turn the feature on alone. The socket uses the SAME
  JSONL envelope as stdin/stdout, with ids scoped per connection, out-of-order
  responses, and many concurrent connections; only the two object ops are valid
  on it. The socket is created DIRECTLY under `BlobExchangeDir` — that keeps the
  path under the platform's ~104-byte limit and is how a client derives the
  exchange directory (the socket's parent) from the only thing it was handed.
  Puts are fire-and-forget and drained at `summary`; the provider stamps each
  object's channel from its own credential (an `ObjectPut` carries no channel to
  assert), and a get filtered by `acceptChannels` omits non-matching objects, so
  a rejected channel is indistinguishable from a miss. All of it is additive to
  provider RPC v2: a provider that ignores the capability is unaffected.
  `objectcachetest` ships a fake socket server (in-memory store, configurable
  channel) so both sides of the contract can be tested against one
  implementation.
- **Fallback.** Every op is best-effort: a non-OK response, a per-op timeout, or
  a provider crash all surface as an error so the caller builds locally — the
  remote cache can never break a build.

Each provider message has strict parse + validate + normalize and a conformance
fixture corpus, exactly like the HTTP messages. The CLI session harness that
drives this RPC is `internal/cacheprovider`; the full design is in the CLI docs
(`tooling/cli/doc/16-cache-provider-rpc.md`).

## Materialization modes

| Mode       | Bytes moved                         | Use            |
|------------|-------------------------------------|----------------|
| `minimal`  | none (status only)                  | CI gate        |
| `toplevel` | requested deliverables              | targeted build |
| `full`     | every output                        | local dev      |

The table states each mode's intent. The CLI still downloads a hit's files in
`minimal` and `toplevel` when a job of the run reads them; its exact rule is in
`tooling/cli/doc/10-caching.md`.

## Remote eligibility policy

The remote cache admits every key whose task is not side-effecting, whatever
its build duration or output size. One shared helper decides it, used by both
client and server so they agree:

- `SideEffectingTask(task)` — excludes commands with external effects (e.g.
  `publish`) that must always run and must never be served from cache.

`EligibleForRemote(key, params)` returns `!SideEffectingTask(key.Task)` and
`WorthRemoteCaching(durationMs, sizeBytes, params)` returns `true`. Both are
deprecated, as are `BreakEvenParams` and `DefaultBreakEven`: they remain so
existing callers compile, and the duration, size and parameter inputs are
ignored.

## Usage

```go
import cache "go.putnami.dev/protocol/cache"

req, diags := cache.ParseAndValidateRequest(body)
if diag.HasErrors(diags) { /* reject */ }

resp, diags := cache.ParseAndValidateResponse(body)
```

`NormalizeRequest` / `NormalizeResponse` apply canonical defaults and ordering
for deterministic output (verified by the determinism tests).

## Producers and consumers

Two contracts, two pairs of sides:

| Contract | Producer / client | Consumer / server |
| --- | --- | --- |
| HTTP wire (`cache.go`, `strict_*.go`) | the cache-provider extension, which owns the HTTP client | the cloud cache server |
| Provider RPC (`provider.go`, `provider_strict.go`) | the CLI build scheduler (`tooling/cli/internal/jobs/remote_provider.go` over the `internal/cacheprovider` session harness) | any extension declaring the reserved `cache-provider` command |

The CLI links **no** HTTP cache client: remote caching is delegated to the
provider subprocess, and the CLI's persisted `.putnami/cache.json` only says
whether a cache is configured and in which materialization mode — never a
credential. Run markers travel the same way, through `marker-lookup` /
`marker-write` on the provider session. The RPC is exercised end to end in the
CLI test suite by a fake provider, and this module's own conformance corpus
pins every message on both contracts.

## Versioning and compatibility

- `ProtocolVersion` (HTTP) is echoed on every request and response so each side
  rejects a mismatch instead of misreading fields.
- `ProviderProtocolVersion` (RPC) is deliberately separate and evolves on its
  own clock; one provider build speaks both. `ProviderProtocolMinVersion`
  records the oldest RPC version current parsers accept, and
  `ProviderProtocolProvenanceVersion` the first that carries cache-entry
  provenance. A session bootstraps `initialize` with the v1 shape and negotiates
  up, so a channel-less v1 provider keeps working.
- Growth is **additive by route, never by field**: `find-missing`,
  `commit-batch`, `upload-grant`, `upload-batch`, and `download-batch` are new
  paths gated behind `GET /v1/cache/capabilities`, which is why they did not
  bump `ProtocolVersion`. The one additive field, `ActionResult.events`, is
  capability-gated for the same reason: both sides parse strictly, so a client
  must not send a field the server has not advertised.
- Every op is best-effort. A missing capability, an unknown route, a timeout, or
  a crashed provider degrades to a local build; the remote cache can never make
  a build wrong, only slower.

## Schemas, fixtures, and tests

This module publishes no JSON Schema: the Go types in [`cache.go`](cache.go) and
[`provider.go`](provider.go) are the source of truth, and the shared corpus under
[`fixtures/`](fixtures) is the cross-implementation surface. Each message has its
own directory (`fixtures/<message>/valid/*.json` and
`fixtures/<message>/invalid/*.json`) covering both the HTTP wire and the provider
RPC; `conformance_test.go` and `provider_conformance_test.go` drive them through
strict parse, validation, and normalization, and the determinism tests pin
canonical output.

## Durable decisions

- [`doc/adr/0001-provider-selection-by-reserved-command.md`](doc/adr/0001-provider-selection-by-reserved-command.md)
  — why a reserved command name is the whole provider-selection rule, with no
  allowlist and no extension-version floor.
- [`doc/adr/0002-blob-exchange-directory-boundary.md`](doc/adr/0002-blob-exchange-directory-boundary.md)
  — why blobs cross the provider boundary through an exchange directory that is
  deliberately not core's CAS.

## Product feature linkage

There is no user-facing product feature for this module, and one should not be
minted for it. This is an internal wire contract between a build client, a cache
provider extension, and a cache server; developers experience it as build
latency, never as a surface they use. Its durable design decisions are recorded
as ADRs under [`doc/adr/`](doc/adr) rather than as a spec, per the spec contract
in [`protocols/features`](../features/README.md), which requires a spec to detail
an already-authored feature. If a product feature later owns remote build
caching end to end, its spec links to these ADRs instead of restating them.

## Support

- **Status:** `stable`, recorded as
  `{"id": "go.putnami.dev/protocol/cache", "kind": "protocol", "status": "stable"}`
  in the workspace-root [`putnami.support.json`](../../putnami.support.json)
  catalog (contract: [`protocols/support`](../support/README.md)).
- **Owner:** the Putnami maintainers, as repository owners of `protocols/`.
  Support entries carry no owner field, so the single owner is stated here.
- **Evidence:** the CLI ships the provider-RPC client in every release and every
  remote-cache session in CI runs through it; both contracts are pinned by the
  fixture corpus above plus determinism tests; compatibility is enforced in code
  by capability gating and by v2↔v1 provider negotiation with a fake-provider
  end-to-end test.
