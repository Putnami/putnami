# Cache-provider RPC

Status: **delegated remote cache**. This chapter records the design of the
out-of-process **cache provider**: the request/response protocol, the CLI session
harness that drives it, the capability/version gate, and the live scheduler
delegation path.

The former in-core HTTP remote-cache client has been removed. Core now owns the
local content-addressed store, cache-key policy, scheduling, materialization,
and the provider RPC harness; `@putnami/cloud` owns the server client,
credentials, and cloud-specific cache behaviour.

## Why a new subsystem

Extracting the remote cache into `@putnami/cloud` means core stops linking the
HTTP cache client and instead launches a provider subprocess and drives it. The
pieces to do that did not exist:

- `tooling/extension-sdk/jsonl` and `protocol/runtime` are a **one-way** event
  stream (job → core): no request framing, no correlation, no per-op timeout, no
  crash signal, no stdin command channel.
- `protocol/cache` (negotiate/store/commit/…) is the **HTTP** contract between
  the cache client and the cloud server — not a provider RPC.
- `internal/extension` discovery has **no** version check or capability probe,
  so "core and `@putnami/cloud` ship on the same release train" was an
  unenforceable assumption across two independently-versioned repositories.

The provider RPC is the bidirectional channel between core and the cloud-owned
cache client; the gate replaces the release-train assumption with a real check.

## Transport: hybrid (control over stdio, blobs through a shared exchange dir)

The control plane is **JSONL request/response over the subprocess's
stdin/stdout**: core writes one `ProviderRequest` per line to stdin and reads
one `ProviderResponse` per line from stdout. Every request carries a
monotonically increasing `id`; responses may return **out of order** (a
speculative prefetch must not block a restore), and a single reader goroutine
demultiplexes by `id`.

Large payloads never cross the pipe. A restore can materialize hundreds of tiny
files and an upload streams gzip blobs — putting those bytes on a JSONL line
would mean multi-megabyte lines and double-buffering. Instead, **bytes move
through a content-addressed blob-exchange directory**, because the provider and
core run on the same machine and share the filesystem:

- `InitializeParams.BlobExchangeDir` names the exchange directory.
- **restore** downloads the hit's blobs into the exchange dir and returns only
  the `Manifest` (metadata); **core ingests** those blobs into its CAS and
  materializes the outputs.
- **upload** is handed a `Manifest` whose blobs **core has exported** into the
  exchange dir; the provider reads them and uploads what the server lacks.

`cache.BlobExchangePath(exchangeDir, digest)` is the single helper both
repositories use, so the handoff layout cannot drift:
`<exchangeDir>/<hex[:2]>/<hex>` (algorithm-stripped lowercase hex, two-character
shard).

**The exchange dir is not core's live CAS** — and that distinction is the whole
point. An out-of-process, separately-versioned provider writing directly into the
store's CAS would have to replicate the store's write discipline across the repo
boundary (atomic stage+rename, the shared/exclusive GC lock, the `blobs/`↔`cas/`
hardlink contract) — exactly the coupling the extraction exists to remove, and it
would break silently the next time the store's layout, locking, or GC changed.
Instead the exchange dir is a dumb provider↔core handoff: the provider only ever
touches it, and **core remains the sole owner of its CAS**, ingesting (restore)
and exporting (upload) between the store and the exchange dir with its own
atomic, locked, content-verified write path. Binding the exchange dir on the same
filesystem as the store lets core ingest/export by hardlink with no byte copy, so
the decoupling costs essentially nothing.

**Core owns the exchange dir's lifecycle.** Each session creates its own
`cache-provider-exchange-*` directory under the store root (or the workspace's
`.putnami` directory when there is no store) and holds an advisory
lock on its `owner.lock` file until it has deleted the directory at shutdown.
Exported blobs stay until then: an upload response only means the provider queued
the upload, and the bytes are confirmed at `summary`, which comes right before
shutdown. A run that is killed never reaches shutdown, so every session start
sweeps its siblings in the background: it deletes a directory only when it is
older than `PUTNAMI_STORE_GC_GRACE` (1h by default, never less than one minute)
and its owner lock is free. A live session's directory is never swept, however
long the session runs. Shutdown stops the sweep before its next directory, and
the next run's sweep deletes the rest. On platforms without `flock`, the sweep
deletes nothing.

Other transports were considered and rejected: length-prefixed binary framing on
the same fds (re-implements a byte protocol the filesystem already gives us for
free between co-located processes, and still copies every byte through both
processes) and a side-channel Unix socket / loopback HTTP (a second connection to
manage and secure, plus the same extra copies). An earlier draft of this design
pointed the provider directly at the store's live CAS; the handoff-directory model
above replaced it precisely to avoid that cross-repo coupling. Revisit only if the
provider ever runs off-host.

## Envelopes and ops

```
ProviderRequest  { protocolVersion, id, op, payload }
ProviderResponse { protocolVersion, id, ok, error?, payload }
```

`payload` is the op-specific body, left raw so the envelope is demuxed by `id`
and dispatched by `op` before the body is decoded. `ProviderProtocolVersion` is
**separate** from the HTTP `ProtocolVersion`: the two evolve independently and a
single build of `@putnami/cloud` speaks both.

| Op | Request → Result | Notes |
|----|------------------|-------|
| `initialize` | `InitializeParams` → `InitializeResult` | First op, once. Carries the blob-exchange dir, mode, workspace identity, capabilities, and the known-digests handshake; returns the provider protocol version, `@putnami/cloud` version, capabilities, and `ready`. |
| `authenticate` | `AuthenticateParams` → `AuthenticateResult` | Hands the provider a hosted run's credential. Sent once, right after `initialize` and before any other op, only when the provider echoed `run-credential`. A refusal ends the session like a failed `initialize`. Not valid on the socket. |
| `prefetch` | `PrefetchParams` → `PrefetchResult` | Speculatively pull keys' blobs into the exchange dir in the background. `resultOnlyKeys` (only after a `restore-result-only` echo) names the keys whose blobs core will not read. |
| `restore` | `RestoreParams` → `RestoreResult` | `hit` (blobs in the exchange dir + manifest; core ingests them into its CAS), `miss`, or `error` — the **restore-failure signal**, distinct from a cold miss but both meaning "build locally". A `resultOnly` restore (only after a `restore-result-only` echo) answers the same statuses without placing any blob. |
| `upload` | `UploadParams` → `UploadResult` | Store a freshly built entry whose blobs core has exported to the exchange dir. |
| `marker-lookup` | `MarkerLookupParams` → `MarkerLookupResult` | Read the last successful whole-target run marker. |
| `marker-write` | `MarkerWriteParams` → `MarkerWriteResult` | Publish a run marker (compare-and-swap). |
| `object-get` | `ObjectGetParams` → `ObjectGetResult` | Look up a batch of objects in one namespace; hits carry a digest whose bytes are in the exchange dir, misses are omitted. **Socket only.** |
| `object-put` | `ObjectPutParams` → `ObjectPutResult` | Offer a batch of objects whose bytes the caller staged in the exchange dir. Fire-and-forget; durable at `summary`. **Socket only.** |
| `summary` | `SummaryParams` → `SummaryResult` | Drain pending uploads; return run statistics. |
| `shutdown` | — | Best-effort graceful exit before core closes stdin. |

The wire types live in `protocol/cache` (`provider.go`, `provider_strict.go`)
alongside the HTTP types they reuse (`ActionResult`, `Manifest`, `RunMarker`,
capability strings), imported by **both** repositories so the protocol cannot
drift. Each message has strict parse + validate + normalize and a conformance
fixture corpus, matching every other `protocol/*` module.

## The object cache and its socket

`object-get` / `object-put` are a generic, language-neutral **object cache**:
small opaque payloads addressed by `(namespace, id)`, with the bytes travelling
through the same blob-exchange directory as task entries. Their first consumer is
a compiler's own build cache; nothing in the contract knows a language.

They differ from every other op in WHO calls them. The process that wants a
compiler cache entry is a **job subprocess**, not core. So a provider that serves
the object cache also listens on a local **Unix socket** and returns its absolute
path in `InitializeResult.ObjectCacheSocket`; core exports that path to every job
(`PUTNAMI_CACHE_OBJECT_SOCKET`) together with the run's resolved trust policy
(`PUTNAMI_CACHE_TRUST`).

**Negotiation.** Core lists `object-cache` in `InitializeParams.Capabilities`. A
provider that serves it echoes the capability in `InitializeResult.Capabilities`
**and** sets the socket path. Core uses the path only when both hold, the path is
absolute, and it is a live socket on disk; anything else leaves the object cache
off for the run without failing the session. No protocol version bump: the ops
and the field are additive to v2, and a provider that ignores the capability
answers exactly as before.

**Socket framing.** Identical to stdin/stdout — one `ProviderRequest` JSON line
per request, one `ProviderResponse` per line back. Request ids are scoped **per
connection**, responses may return out of order, and many concurrent connections
are expected (one or more per job process). Only the two object ops are valid
there: a session op is answered with `ok=false`, so a job can never drive the
session's lifecycle, restore a task entry, or publish a run marker.

**Where the bytes go.** The provider creates the socket **directly under**
`BlobExchangeDir`. That keeps the path short (a Unix socket path is capped near
104 bytes on macOS) and — load-bearing — it is how a client finds the exchange
directory at all: a job is handed the socket path and nothing else, so it derives
the directory as the socket's parent.

**Trust.** The provider stamps each stored object's channel from its own
credential, exactly as it does for task entries; an `ObjectPut` has no channel
field, so a caller cannot claim one. A get filtered by `acceptChannels` omits
non-matching objects, making them indistinguishable from a miss. An empty filter
accepts every channel, including the legacy empty one.

**Failure.** Best-effort like every other op: a caller that cannot reach the
socket, or whose request fails, falls back to whatever local cache it already had.

## The run credential

A hosted run (`--credential-fd`) authenticates the remote cache with the run's
credential instead of `PUTNAMI_CACHE_TOKEN`, which the engine removes from its
environment. The credential never enters the provider's launch environment,
arguments or files; it travels over the RPC.

**Negotiation.** Core lists `run-credential` (`CapabilityRunCredential`) in the
v1-bootstrap `InitializeParams.Capabilities` only when it holds a credential,
so a local run's `initialize` never lists it. A provider that can
take the credential echoes the capability in `InitializeResult.Capabilities`;
it reports `ready` as it will be once authenticated.

**The op.** After the echo, core sends one `authenticate` with
`AuthenticateParams{credential}`, right after `initialize` returns and before
any other op; the result is empty. The credential is 1 to 16384 bytes of UTF-8
with no whitespace (`ValidRunCredential`). Without the echo core sends nothing
and the session goes on. A refused or failed `authenticate` is handled like a
failed `initialize`: core closes the session and the run builds locally. A
malformed credential fails `initialize` before core writes a line.

**Handling.** The provider keeps the credential in memory only. Every Go type
that holds it (`AuthenticateParams`, `ProviderRequest`) formats as redacted,
and core removes the credential from a refusal message before it logs it.

## Result-only restore

A run often reads no file of a cache hit: in `minimal` mode a hit that no job
of the run waits for only reports a status, and in `toplevel` mode such a hit
outside the requested projects does too. Downloading those blobs costs bytes
no job reads.

**Negotiation.** Core lists `restore-result-only`
(`CapabilityRestoreResultOnly`) in the v1-bootstrap
`InitializeParams.Capabilities` of every session. A provider that honors the
two fields below echoes it in `InitializeResult.Capabilities`. Core sends
either field only after the echo, so a provider that does not echo it receives
the same `prefetch` and `restore` payloads as before and keeps parsing them
strictly.

**The fields.** `RestoreParams.resultOnly: true` means core reads the hit's
result and none of its files. On a hit the provider still answers `hit` with
`Result`, the full `Manifest`, and the provenance fields, but it need not place
any blob in the exchange directory, and core ingests none. `miss` and `error`
keep their meaning. `PrefetchParams.resultOnlyKeys` is the subset of `keys`
that core will restore that way, so a prefetch need not download their blobs.
Each one is a valid key, listed once, and also listed in `keys`.

**What the provider guarantees.** Core does not verify a result-only hit's
entry descriptor, because it reads no blob: it cannot check that the entry
records the requested key, as a full restore does. The provider must answer
with exactly the entry stored under the requested key, or with `miss`.

**What core does with a result-only hit.** Core decides per task whether the
run reads its files (the rule is in `10-caching.md`, "Materialization modes").
When it does not, a trusted result-only hit records the task's result in a
local result-only entry, which holds the result and its metadata but no file,
and the task reports its cached status without writing to the workspace. A
`hint` hit restored this way is a miss: there is no blob to warm. A later run
that needs the files ignores the result-only entry and restores the full entry
from the provider; if that fails, the task runs.

## Session lifecycle and fallback

`internal/cacheprovider.Session` spawns one provider per build run:

```
Spawn → Initialize [→ authenticate] → ( Prefetch | Restore | Upload | LookupMarker | WriteMarker )* → Summary → Close
```

Every op is **best-effort**. The harness surfaces an error — and the caller
falls back to a local build — on any of:

- **per-op timeout** — a call whose context deadline (or the session default)
  elapses returns a deadline error. A single timed-out op does **not** kill the
  session; a genuinely wedged provider simply times out every op, each falling
  back locally.
- **non-OK response** — surfaced as a typed `*OpError` (the provider answered,
  but the op failed), distinct from a transport failure.
- **crash** — when the pipe closes (provider exits/dies), the reader marks the
  session dead, fails every in-flight op, and short-circuits every later op, so
  the build never hangs waiting on a corpse.

This preserves the old remote-cache guarantee: **the remote cache can never break
a build, only accelerate it.** `Close` sends a best-effort `shutdown`, closes
stdin, and waits a bounded time before force-killing the process group —
mirroring the job runner's SIGTERM-then-SIGKILL teardown.

## Provider selection

Selection is one question, asked once: **which loaded extension declares the
reserved `cache.ProviderCommandName` (`"cache-provider"`) command?** No new
manifest-schema field is needed — the extension simply declares a command of
that name — and `extension.ResolveReservedProvider(exts, command)` answers it.
Three outcomes:

| Outcome | Meaning | Caller behavior |
|---------|---------|-----------------|
| `(provider, nil)` | exactly one extension declares the command | delegate to it |
| `(nil, nil)` | none does | local-only **with a one-line notice** when remote cache is configured (carrying discovery's skip records when an extension failed to LOAD, so a fixable fault is never silent); silent when remote cache is not configured |
| `(nil, err)` wrapping `extension.ErrProviderAmbiguous` | several declare it | surface the error; picking silently would make the active provider a function of discovery order |

Discovery checks neither a provider's product name nor an extension-version
floor. Neither establishes compatibility, and a version floor cannot order a
prerelease identifier that carries no semver order. Compatibility is
established by the extension contract version at discovery and by the
negotiated handshake below. Publishing has no provider gate: advanced
publishing is selected by the tasks an extension's manifest declares.

## Version negotiation

The `initialize` handshake deliberately starts at
`ProviderProtocolMinVersion` (v1) in both its envelope and payload, so a
deployed strict-v1 provider can parse the first message. Core adds
`provider-protocol-v2` to the existing capabilities list; that field is already
forward-tolerant, so a v1 provider ignores it and returns v1. A v2 provider that
recognizes the capability returns v2 in `InitializeResult` (while echoing the
v1 bootstrap in the response envelope), and the session uses the selected v1 or
v2 version for every later operation. Values outside the supported range are
rejected. This negotiated handshake — not any version string core reads off a
manifest — is what establishes RPC compatibility.

## Division of responsibility

The op set is intentionally thin. The split is:

- **Core owns** local cache keys, dependency hit/miss accounting, materialization
  into the local CAS, upload export from the local CAS, run-marker workspace
  identity, scheduler integration, and best-effort fallback.
- **The provider owns** the cloud HTTP client, credential resolution, server-side
  cache protocol, remote run-marker storage, and cloud-specific readiness.
- **The exchange directory** is the only shared filesystem contract. The
  provider writes restored blobs there and reads upload blobs from there; core is
  still the sole writer to its live CAS.

## Live scheduler path

`jobs.LoadRemoteCache` first checks whether remote cache is explicitly active
(`.putnami/cache.json` or `PUTNAMI_CACHE_URL`). If it is inactive, the run stays
local-only and silent, even when `@putnami/cloud` is installed. If it is active,
core detects a compatible provider command and prepares the extension runtime
that command references, as for a task (see
[Runtime preparation](07-extensions.md#runtime-preparation)). A runtime that
cannot be prepared leaves the build local with a one-line notice. Core then
starts one provider subprocess on the first remote operation and passes the
configured materialization mode to `initialize`.

Provider startup is triggered by either half of the run's needs: a key set to
warm, **or** at least one planned job that will execute locally and could use the
object cache. A fully warm rebuild — every planned job a local hit — starts no
provider. Under `--no-cache` the provider stays down too.

When no capable provider is available for an active remote-cache config, core
prints one user-facing notice and builds locally. A too-old or non-provider
`@putnami/cloud` build is an actionable gate failure; an absent provider is an
install hint. Provider runtime failures remain warning-level best-effort
fallbacks.

## Testing

`internal/cacheprovider` re-execs the test binary as a **real** fake-provider
subprocess (the `TestMain` helper-process pattern), so the round-trip,
timeout, and crash paths exercise real stdio and a real process exit rather than
in-memory simulation. The suite round-trips every op (including the hybrid
blob-exchange path — restore writes a blob into the exchange dir, upload reads it
back), and asserts graceful
fallback on per-op timeout, provider crash, a non-OK op response, and an
unsupported `initialize` protocol version. The protocol package adds a
conformance corpus for every provider message.

The object cache is tested against a **real socket**, not a simulation:
`protocol/cache/objectcachetest` ships a fake provider-side server (in-memory
store, configurable trust channel, real concurrent connections) that both this
repository and a provider implementation can drive. `internal/jobs` wires it into
the fake provider and runs a real job subprocess that dials the socket it was
handed, so the negotiation, the exported variables and the round trip are proved
end to end — including that the two variables never move a cache key.
