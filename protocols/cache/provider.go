// Provider RPC — the out-of-process cache-provider contract.
//
// The types above (NegotiateRequest, StoreRequest, …) are the HTTP wire
// contract between the cache *client* and the cloud cache *server*. This file
// adds a second, orthogonal contract: the request/response RPC the build
// scheduler (core) speaks to a long-lived **cache-provider subprocess** that
// owns that HTTP client.
//
// # Why a separate RPC
//
// Extracting the remote cache into `@putnami/cloud` means core no longer links
// the HTTP client; it launches a provider subprocess and drives it. The
// extension event stream (`protocol/runtime`) is one-way (job → core, fire and
// forget): no request framing, correlation, per-op timeout, or crash signal.
// The provider RPC is the missing bidirectional channel.
//
// # Transport (hybrid)
//
// Control messages are JSONL request/response over the subprocess's
// stdin/stdout: core writes one ProviderRequest per line to stdin and reads one
// ProviderResponse per line from stdout. Each request carries a monotonically
// increasing ID so responses may return out of order (a speculative prefetch
// need not block a restore) — core demultiplexes by ID.
//
// Large payloads never cross the pipe. The provider and core share the local
// filesystem, so bytes move through a content-addressed blob-exchange directory
// (InitializeParams.BlobExchangeDir, see BlobExchangePath): on restore the
// provider downloads a hit's blobs into it and the response carries only the
// Manifest (metadata); on upload core exports the freshly built blobs into it
// and the request carries only the Manifest. A restore that emits hundreds of
// tiny files, or an upload that streams gzip blobs, therefore costs one small
// JSONL round trip plus local file I/O, not a multi-megabyte line.
//
// The exchange directory is the provider's ONLY filesystem contact point; it is
// deliberately NOT core's live content-addressed store. Core remains the sole
// owner of its CAS write discipline (atomic stage+rename, the GC lock, the
// blobs↔cas hardlink contract): core ingests the provider's downloaded blobs
// into the CAS, and exports built blobs out to the exchange directory for the
// provider to upload. Binding the exchange directory on the same filesystem as
// the store lets core ingest/export by hardlink with no byte copy, while keeping
// the provider decoupled from the store's layout, locking, and GC.
//
// # Lifecycle & fallback
//
// One provider session is spawned per build run: initialize → [authenticate]
// → (prefetch | restore | upload | marker-lookup | marker-write)* → summary →
// shutdown, where authenticate is sent only after the provider echoed
// CapabilityRunCredential. Every op is best-effort. On a per-op timeout, a
// ProviderResponse with OK=false, or a provider crash (the pipe closes), core
// logs and builds locally — the same
// "return nil, build locally" guarantee the in-core remote cache already makes,
// so the provider can never break a build, only accelerate it.
//
// # Selection and version negotiation
//
// An extension serves the provider by declaring a command named
// ProviderCommandName; core resolves the sole declarer among the loaded
// extensions and launches it. That reserved name is the WHOLE selection rule:
// there is no product-name allowlist and no extension-version floor, because a
// provider published as a SHA-stamped prerelease has no semver order and a floor
// rejects newer builds as older (see doc/adr/0001-provider-selection-by-reserved-command.md).
//
// Compatibility is established twice, neither time by a version string core
// invents: the extension contract version is checked at discovery, and
// InitializeResult carries the provider's negotiated RPC ProtocolVersion.
// Sessions bootstrap Initialize with the v1 wire shape, then use a
// backward-compatible capability to let a v2 provider select the
// provenance-aware session version.
//
// # The object cache and its socket
//
// OpObjectGet and OpObjectPut are a generic, language-neutral object cache:
// small opaque payloads addressed by (namespace, id), with the bytes traveling
// through the same blob-exchange directory as task entries. Their first consumer
// is a compiler's own build cache (Go's GOCACHEPROG), but nothing in the
// contract knows a language.
//
// Unlike every other op, the caller is usually NOT core: the process that wants
// a compiler cache entry is a job subprocess. A provider that serves the object
// cache therefore also listens on a local Unix socket, whose absolute path it
// returns in InitializeResult.ObjectCacheSocket, and core exports that path to
// every job it spawns (ObjectCacheSocketEnv). The socket contract is:
//
//   - Framing is IDENTICAL to stdin/stdout: one ProviderRequest JSON line per
//     request, one ProviderResponse JSON line per response.
//   - IDs are scoped PER CONNECTION, not per session: each client numbers its
//     own requests from 1 and demultiplexes the responses it receives.
//   - Responses may arrive out of order, so a batched get need not block a
//     concurrent put.
//   - Many concurrent connections are allowed and expected — one per job
//     process, sometimes more — and they outlive none of them: the provider
//     serves the socket for the whole session and closes it at shutdown.
//   - ONLY OpObjectGet and OpObjectPut are valid on the socket. Every other op
//     is answered with ok=false; a job process can never drive the session's
//     lifecycle, restore a task entry, or publish a run marker.
//   - The provider creates the socket DIRECTLY under
//     InitializeParams.BlobExchangeDir, not in a subdirectory of it. Two reasons,
//     and the second is load-bearing: the path stays short (a Unix socket path is
//     capped near 104 bytes on macOS), and it is how a client finds the exchange
//     directory at all. A job process is handed the socket path and nothing else,
//     so it derives the directory bytes travel through as the socket's PARENT
//     directory. A provider that nests the socket breaks every put.
//
// The object ops are additive to provider RPC v2: they add no field to an
// existing v1 payload and no envelope shape. A provider sets
// ObjectCacheSocket only when the initialize params advertised
// CapabilityObjectCache, so a core too old to know the field never receives it
// from a strict parser's point of view.

package cache

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ProviderProtocolVersion is the latest version of the provider RPC contract.
// It is deliberately separate from ProtocolVersion (the HTTP cache wire
// contract): the two evolve independently, and a single build of
// @putnami/cloud speaks both. Provider RPC v2 adds cache-entry provenance and
// trust channels while retaining v1 parsing so channel-less providers continue
// to work under the caller's local policy.
const ProviderProtocolVersion = 2

// ProviderProtocolMinVersion is the oldest provider RPC version a current
// parser accepts. Version 1 predates entry provenance, so its restore results
// and upload params have no producer or channel fields.
const ProviderProtocolMinVersion = 1

// ProviderProtocolProvenanceVersion is the first provider RPC version that
// carries cache-entry provenance. A provider must omit provenance fields when
// speaking an older negotiated version so strict v1 consumers keep working.
const ProviderProtocolProvenanceVersion = 2

// ProviderProtocolHasProvenance reports whether a negotiated provider RPC
// version carries producer, producer identity, and trust-channel fields.
func ProviderProtocolHasProvenance(version int) bool {
	return version >= ProviderProtocolProvenanceVersion && version <= ProviderProtocolVersion
}

// CapabilityProviderProtocolV2 is sent in a v1 InitializeParams bootstrap by
// a core that can switch to provider RPC v2. It uses the existing capabilities
// array because unknown capability strings are safe for strict v1 providers to
// ignore. A v2 provider that sees it may reply with an InitializeResult whose
// ProtocolVersion is 2; core then uses v2 for subsequent envelopes and payloads.
// A provider must otherwise select v1 so an older core stays on its legacy wire
// shape.
const CapabilityProviderProtocolV2 = "provider-protocol-v2"

// CapabilityObjectCache is listed in InitializeParams.Capabilities by a core
// that can carry the generic object cache (OpObjectGet / OpObjectPut) to its job
// processes, and echoed in InitializeResult.Capabilities by a provider that
// serves it. The echo is what makes InitializeResult.ObjectCacheSocket
// meaningful: a provider must leave that field empty when the capability was not
// advertised, and core must ignore a socket a provider offered without echoing
// the capability. Both halves are required, so neither side can turn the object
// cache on alone.
const CapabilityObjectCache = "object-cache"

// CapabilityRunCredential is listed in the v1-bootstrap
// InitializeParams.Capabilities by a core that holds a hosted run's
// credential, and echoed in InitializeResult.Capabilities by a provider that
// takes it. After the echo, core sends OpAuthenticate exactly once, right after
// initialize and before any other op. Without the echo core sends nothing and
// the session goes on as before. A core without a run credential never lists
// it, so a local run's initialize is unchanged.
const CapabilityRunCredential = "run-credential" // #nosec G101 -- capability name, not a credential

// CapabilityRestoreResultOnly is listed in the v1-bootstrap
// InitializeParams.Capabilities by a core that can serve a cache hit from its
// result alone, and echoed in InitializeResult.Capabilities by a provider that
// honors RestoreParams.ResultOnly and PrefetchParams.ResultOnlyKeys. Core sends
// either field only after the echo, so a provider without the capability keeps
// receiving the wire it already parses strictly. A provider that echoes it may
// still place blobs for a result-only key; core ignores them.
const CapabilityRestoreResultOnly = "restore-result-only"

// ObjectCacheSocketEnv is the environment variable core exports to every job
// subprocess, carrying the absolute path of the provider's object-cache socket
// (InitializeResult.ObjectCacheSocket). Its ABSENCE is the off switch: a job
// that does not see it uses whatever local cache it already had. Core exports it
// only for a live provider-backed run that negotiated CapabilityObjectCache, so
// it is absent under --no-cache, under trust "none", and when no provider
// serves the run.
const ObjectCacheSocketEnv = "PUTNAMI_CACHE_OBJECT_SOCKET"

// CacheTrustEnv is the environment variable carrying the run's RESOLVED
// remote-cache trust policy ("any" or "ci") to every job subprocess. A job that
// reads objects from the socket derives its ObjectGetParams.AcceptChannels from
// it: "ci" accepts only ChannelTrusted, "any" accepts every channel.
//
// It is the same variable a user may set to choose the policy, and that is
// deliberate: exporting the resolved value means a job that calls back into the
// CLI resolves the same policy the outer run did, instead of re-deriving a
// different default from its own environment.
const CacheTrustEnv = "PUTNAMI_CACHE_TRUST"

// ProviderCommandName is the RESERVED command an extension declares to serve as
// the cache provider, in the same sense as extension.ReservedCacheCommands:
// core asks which loaded extension declares it, launches that one, and has no
// other opinion about who may serve. Any extension may — there is no allowlist
// and no version floor. Two declarers is an error, because picking silently
// would make the active provider a function of discovery order. Defining the
// name here keeps it from drifting between the two repositories.
const ProviderCommandName = "cache-provider"

// ProviderOp identifies a provider RPC method. The op set mirrors the phases of
// a build's interaction with the remote cache.
type ProviderOp string

const (
	// OpInitialize opens the session: it carries the materialization mode, the
	// blob-exchange directory for the hybrid blob path, the workspace identity
	// used for run markers, and the known-present digests handshake, and returns the
	// provider's protocol version, @putnami/cloud version, and advertised
	// capabilities. It must be the first op and is sent exactly once.
	OpInitialize ProviderOp = "initialize"
	// OpAuthenticate hands the provider the hosted run's credential
	// (AuthenticateParams). Core sends it exactly once, right after a
	// successful OpInitialize and before any other op, and only when the
	// provider echoed CapabilityRunCredential. A refused or failed authenticate
	// ends the session like a failed initialize: core builds locally.
	OpAuthenticate ProviderOp = "authenticate"
	// OpPrefetch asks the provider to speculatively pull a set of cache keys'
	// blobs into the CAS in the background, hiding their latency behind the
	// local build of the misses that depend on them. Returns promptly.
	OpPrefetch ProviderOp = "prefetch"
	// OpRestore returns the cached ActionResult and Manifest on a hit
	// (RestoreHit), a clean miss (RestoreMiss), or a provider failure
	// (RestoreError); both non-hits tell core to build locally. On a hit the
	// provider places every Manifest blob in the blob-exchange directory, and
	// core ingests them into the CAS, unless RestoreParams.ResultOnly is set.
	OpRestore ProviderOp = "restore"
	// OpUpload hands the provider a freshly built entry (key + result +
	// manifest) whose blobs core has exported into the blob-exchange directory,
	// to upload in the background. Returns promptly; the bytes are confirmed at
	// OpSummary.
	OpUpload ProviderOp = "upload"
	// OpMarkerLookup reads the last successful whole-target run marker.
	OpMarkerLookup ProviderOp = "marker-lookup"
	// OpMarkerWrite publishes a successful whole-target run marker.
	OpMarkerWrite ProviderOp = "marker-write"
	// OpObjectGet looks up a batch of objects in one namespace and returns the
	// hits, each with a digest whose bytes the provider has placed in the
	// blob-exchange directory. Misses are omitted. It is the only read op a job
	// process may issue, over the object-cache socket.
	OpObjectGet ProviderOp = "object-get"
	// OpObjectPut offers a batch of objects whose bytes the caller has already
	// written into the blob-exchange directory. It is fire-and-forget: the
	// response acknowledges queueing, and the bytes are durable at OpSummary.
	// It is the only write op a job process may issue, over the object-cache
	// socket.
	OpObjectPut ProviderOp = "object-put"
	// OpSummary drains pending background uploads and returns the run's cache
	// statistics. It is sent once, after the build, before shutdown.
	OpSummary ProviderOp = "summary"
	// OpShutdown asks the provider to flush and exit cleanly. Core sends it
	// best-effort, then closes stdin; a provider that has already exited or does
	// not answer is force-terminated after a bounded wait.
	OpShutdown ProviderOp = "shutdown"
)

// Valid reports whether op is a recognized provider op.
func (op ProviderOp) Valid() bool {
	switch op {
	case OpInitialize, OpAuthenticate, OpPrefetch, OpRestore, OpUpload,
		OpMarkerLookup, OpMarkerWrite, OpObjectGet, OpObjectPut,
		OpSummary, OpShutdown:
		return true
	default:
		return false
	}
}

// ValidOnObjectCacheSocket reports whether op may be issued over the
// object-cache socket. The socket is reachable by any job subprocess, so its op
// set is deliberately the two object ops and nothing else: the session's
// lifecycle, task entries, and run markers stay core's alone.
func (op ProviderOp) ValidOnObjectCacheSocket() bool {
	return op == OpObjectGet || op == OpObjectPut
}

// --- envelopes ---

// ProviderRequest is one core → provider message. Exactly one is written per
// line of the provider's stdin. Payload is the op-specific request body
// (InitializeParams, RestoreParams, …); it is left raw so the envelope can be
// demultiplexed by ID and dispatched by Op before the body is decoded.
type ProviderRequest struct {
	// ProtocolVersion is the provider RPC version used in this request. Current
	// parsers accept ProviderProtocolMinVersion through ProviderProtocolVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// ID correlates the response to this request. It is unique and increasing
	// within a session; responses may arrive out of order.
	ID int64 `json:"id"`
	// Op is the provider method to invoke (see Op* values).
	Op ProviderOp `json:"op"`
	// Payload is the op-specific request body, left raw so the envelope is
	// dispatched by Op before the body is decoded.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// String formats the envelope without its payload bytes, which carry the run
// credential in an authenticate request.
func (r ProviderRequest) String() string {
	return fmt.Sprintf("{protocolVersion:%d id:%d op:%s payload:<%d bytes>}", r.ProtocolVersion, r.ID, r.Op, len(r.Payload))
}

// GoString keeps %#v as redacted as String.
func (r ProviderRequest) GoString() string { return "cache.ProviderRequest" + r.String() }

// ProviderResponse is one provider → core message, written per line to the
// provider's stdout. ID echoes the request it answers. OK is false when the op
// failed; Error then carries the reason and core falls back to a local build.
// Payload is the op-specific result body (InitializeResult, RestoreResult, …).
type ProviderResponse struct {
	// ProtocolVersion is the provider RPC version used in this response and
	// normally echoes the request envelope. In particular, an initialize response
	// stays on the v1 bootstrap envelope even when its InitializeResult selects
	// v2 for subsequent messages. Current parsers accept
	// ProviderProtocolMinVersion through ProviderProtocolVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// ID echoes the ProviderRequest.ID this response answers.
	ID int64 `json:"id"`
	// OK reports whether the op succeeded; false makes core fall back to a local
	// build.
	OK bool `json:"ok"`
	// Error carries the failure reason when OK is false.
	Error *ProviderError `json:"error,omitempty"`
	// Payload is the op-specific result body, left raw until dispatched by ID.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// ProviderError describes a failed op. The harness treats every error as a
// best-effort fallback to a local build regardless of Retryable; Retryable is
// advisory metadata for logging and future policy.
type ProviderError struct {
	// Code is the machine-readable failure code.
	Code string `json:"code"`
	// Message is the human-readable failure description.
	Message string `json:"message,omitempty"`
	// Retryable is advisory metadata for logging/future policy; the harness falls
	// back to a local build regardless.
	Retryable bool `json:"retryable,omitempty"`
}

// BlobExchangePath returns the path a blob with the given digest occupies under
// the blob-exchange directory, and whether the digest is well-formed. Both core
// and the provider use it so the handoff layout cannot drift: a blob the provider
// downloads (restore) is found by core for ingest, and a blob core exports
// (upload) is found by the provider, at the same location. The layout shards by
// the first two hex characters to bound directory width —
// <exchangeDir>/<hex[0:2]>/<hex> — and uses the algorithm-stripped lowercase hex
// so the filename carries no ':' separator.
//
// This is a provider↔core handoff directory, NOT core's live CAS: core ingests
// these blobs into its store (and exports to here) with its own atomic, locked,
// content-verified write path, so the provider stays decoupled from the store's
// layout, locking, and GC.
func BlobExchangePath(exchangeDir, digest string) (string, bool) {
	hex, ok := strings.CutPrefix(digest, DigestAlgorithm+digestSep)
	if !ok || !isLowerHex(hex, KeyLength) {
		return "", false
	}
	return filepath.Join(exchangeDir, hex[:2], hex), true
}

// MarshalPayload encodes an op body into a ProviderRequest/ProviderResponse
// Payload. A nil body marshals to an absent payload.
func MarshalPayload(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// --- initialize ---

// InitializeParams opens a provider session. The first request is a v1
// bootstrap so legacy providers can parse it; the capabilities list carries any
// opt-in needed for a newer session version.
type InitializeParams struct {
	// ProtocolVersion is the provider RPC version used by this initialize
	// bootstrap. Core starts at ProviderProtocolMinVersion, then adopts the
	// negotiated version returned by InitializeResult.
	ProtocolVersion int `json:"protocolVersion"`
	// BlobExchangeDir is the absolute path of the content-addressed directory the
	// provider and core hand blobs through (see BlobExchangePath). It is the
	// hybrid transport's data channel: blobs never travel over the pipe. It is a
	// handoff directory, not core's live CAS — core ingests/exports between it and
	// the store.
	BlobExchangeDir string `json:"blobExchangeDir"`
	// Mode is the build's materialization intent (minimal/toplevel/full).
	Mode Mode `json:"mode,omitempty"`
	// Workspace is the opaque client-derived workspace identity used to scope
	// run markers (empty disables markers for the session).
	Workspace string `json:"workspace,omitempty"`
	// Branch is the current branch, used for run-marker scope.
	Branch string `json:"branch,omitempty"`
	// Capabilities are the optional behaviors core supports/requests. In
	// particular, CapabilityProviderProtocolV2 lets a v2 provider upgrade a v1
	// bootstrap without adding a field that a strict v1 provider would reject.
	Capabilities []string `json:"capabilities,omitempty"`
	// KnownDigests is the presence handshake: digests core already has locally,
	// so the provider can skip re-downloading them. A large warm set may instead
	// be owned by the provider (read from a presence file under BlobExchangeDir);
	// that ownership split is decided with the cloud provider job.
	KnownDigests []string `json:"knownDigests,omitempty"`
}

// InitializeResult answers OpInitialize and drives the runtime half of the
// capability/version gate.
type InitializeResult struct {
	// ProtocolVersion is the provider RPC version selected for the remainder of
	// this session. Core accepts the supported v1-v2 negotiation range. A
	// provider selects v2 only when the v1 bootstrap advertises
	// CapabilityProviderProtocolV2; otherwise it selects v1.
	ProtocolVersion int `json:"protocolVersion"`
	// ProviderName identifies the provider implementation (e.g. "@putnami/cloud").
	ProviderName string `json:"providerName,omitempty"`
	// ProviderVersion is the provider extension's version, for the version gate.
	ProviderVersion string `json:"providerVersion,omitempty"`
	// Capabilities advertises the optional behaviors the provider/server
	// supports, reusing the HTTP capability strings (Capability*).
	Capabilities []string `json:"capabilities,omitempty"`
	// ObjectCacheSocket is the absolute path of the Unix socket the provider
	// serves the object cache on for this session's lifetime (see the package
	// comment for the socket contract). A provider sets it only when the
	// initialize params advertised CapabilityObjectCache and it also echoes that
	// capability above; core ignores the path otherwise. It is created under
	// InitializeParams.BlobExchangeDir so the path stays short.
	//
	// Strict validation deliberately does not check the path: a socket that is
	// relative, absent, or not a socket must disable the object cache for the
	// run, never fail the initialize that carries it, so core checks it and
	// degrades on its own (and platform path rules stay out of the protocol).
	ObjectCacheSocket string `json:"objectCacheSocket,omitempty"`
	// Ready reports whether the provider is configured and able to serve the
	// remote cache. A provider that loads but has no cache URL/token reports
	// Ready=false so core stays local-only without an error. A provider that
	// echoes CapabilityRunCredential reports Ready as it will be once
	// OpAuthenticate succeeds.
	Ready bool `json:"ready"`
}

// --- authenticate ---

// MaxRunCredentialBytes bounds AuthenticateParams.Credential. It equals the
// bound of the credential-provider RPC in protocol/registry, so one run
// credential fits both.
const MaxRunCredentialBytes = 16 << 10

// AuthenticateParams carries the hosted run's credential: an opaque bearer
// (ValidRunCredential). The provider keeps it in memory only and authenticates
// the remote cache with it in place of any credential it found on its own.
// Formatting redacts it; the wire encoding carries it.
type AuthenticateParams struct {
	// Credential is the run's opaque bearer.
	Credential string `json:"credential"`
}

// String formats the params without the credential.
func (p AuthenticateParams) String() string {
	if p.Credential == "" {
		return "{credential:}"
	}
	return "{credential:<redacted>}"
}

// GoString keeps %#v as redacted as String.
func (p AuthenticateParams) GoString() string { return "cache.AuthenticateParams" + p.String() }

// AuthenticateResult answers OpAuthenticate. It carries no fields; a provider
// that cannot use the credential answers ok=false.
type AuthenticateResult struct{}

// ValidRunCredential reports whether credential is a well-formed run
// credential: non-empty, valid UTF-8, at most MaxRunCredentialBytes bytes, and
// free of every character unicode.IsSpace reports. It is the rule of
// registry.ValidRunCredential, restated so this module depends on no other
// protocol.
func ValidRunCredential(credential string) bool {
	return credential != "" && len(credential) <= MaxRunCredentialBytes && utf8.ValidString(credential) &&
		strings.IndexFunc(credential, unicode.IsSpace) < 0
}

// --- prefetch ---

// PrefetchParams lists cache keys to speculatively materialize into the CAS.
type PrefetchParams struct {
	// Keys are the cache keys to speculatively materialize into the CAS.
	Keys []string `json:"keys"`
	// ResultOnlyKeys is the subset of Keys whose files the caller will not
	// read: it restores each of them with RestoreParams.ResultOnly set. A
	// provider may look such a key up but need not download any of its blobs.
	// Every entry is a valid key, appears once, and is also listed in Keys.
	// Core sends it only to a provider that echoed CapabilityRestoreResultOnly.
	ResultOnlyKeys []string `json:"resultOnlyKeys,omitempty"`
}

// PrefetchResult reports how many keys the provider accepted for background
// prefetch.
type PrefetchResult struct {
	// Started is the number of keys accepted for background prefetch.
	Started int `json:"started,omitempty"`
}

// --- restore ---

// RestoreParams names the single key to restore.
type RestoreParams struct {
	// Key is the single cache key to restore.
	Key string `json:"key"`
	// ResultOnly reports that the caller needs the entry's result, not its
	// files. On a hit the provider still returns Status, Result, the full
	// Manifest, and the provenance fields, but it need not place any Manifest
	// blob in the blob-exchange directory, and the caller reads none. Miss and
	// error keep their meaning. Core sets it only for a provider that echoed
	// CapabilityRestoreResultOnly.
	//
	// Core does not verify a result-only hit's entry descriptor: it reads no
	// blob, so it cannot check that the entry records the requested key. The
	// provider must answer with exactly the entry stored under Key, and must
	// report a miss rather than any other entry.
	ResultOnly bool `json:"resultOnly,omitempty"`
}

// RestoreStatus is the outcome of a restore.
type RestoreStatus string

const (
	// RestoreHit means the entry was found and its blobs are now in the CAS;
	// Result and Manifest are populated.
	RestoreHit RestoreStatus = "hit"
	// RestoreMiss means the key is not cached — a normal outcome, not an error.
	RestoreMiss RestoreStatus = "miss"
	// RestoreError means the provider found (or may have found) the entry but
	// could not materialize it (network, blob fetch, content mismatch). Like a
	// miss it tells core to build locally, but it is logged as a failure, not a
	// cold miss — the restore-failure signal.
	RestoreError RestoreStatus = "error"
)

// Valid reports whether s is a recognized restore status.
func (s RestoreStatus) Valid() bool {
	switch s {
	case RestoreHit, RestoreMiss, RestoreError:
		return true
	default:
		return false
	}
}

// Producer identifies the authenticated class of principal that produced a
// cache entry. A provider derives this value from its trusted authentication
// context; it is never authoritative when asserted by a client upload.
type Producer string

const (
	// ProducerCI identifies an entry produced by a CI workload identity.
	ProducerCI Producer = "ci"
	// ProducerDeveloper identifies an entry produced by a developer identity.
	ProducerDeveloper Producer = "developer"
)

// Valid reports whether p is a recognized cache-entry producer.
func (p Producer) Valid() bool {
	switch p {
	case ProducerCI, ProducerDeveloper:
		return true
	default:
		return false
	}
}

// Channel classifies how a cache entry may be trusted by callers. Trusted
// entries are authoritative CI outputs; hint entries may warm local work but
// must not independently satisfy an authoritative execution policy.
type Channel string

const (
	// ChannelTrusted is the authoritative channel for CI-produced entries.
	ChannelTrusted Channel = "trusted"
	// ChannelHint is the non-authoritative channel for developer-produced entries.
	ChannelHint Channel = "hint"
)

// Valid reports whether c is a recognized cache-entry trust channel.
func (c Channel) Valid() bool {
	switch c {
	case ChannelTrusted, ChannelHint:
		return true
	default:
		return false
	}
}

// RestoreResult answers OpRestore. On RestoreHit, Result and Manifest are set
// and, unless the request set RestoreParams.ResultOnly, every Manifest digest is
// guaranteed present in the blob-exchange directory so core can ingest the blobs
// into its CAS and materialize the outputs locally.
type RestoreResult struct {
	// Status is the restore outcome (hit/miss/error).
	Status RestoreStatus `json:"status"`
	// Result is the cached ActionResult, set on RestoreHit.
	Result *ActionResult `json:"result,omitempty"`
	// Manifest is the hit's file set, set on RestoreHit. Its digests are present
	// in the exchange dir unless the request set RestoreParams.ResultOnly.
	Manifest *Manifest `json:"manifest,omitempty"`
	// Producer is the provider-authoritative producer class of the restored
	// entry. It is omitted by v1/channel-less providers.
	Producer Producer `json:"producer,omitempty"`
	// ProducerIdentity is an opaque provider-derived identifier for the producer
	// (for example, an OIDC subject). It is omitted by v1/channel-less providers.
	ProducerIdentity string `json:"producerIdentity,omitempty"`
	// Channel is the provider-authoritative trust channel of the restored entry.
	// It is omitted by v1/channel-less providers.
	Channel Channel `json:"channel,omitempty"`
}

// --- upload ---

// UploadParams hands the provider a freshly built entry to store. The Manifest
// digests' bytes must already be in the blob-exchange directory (core exported
// them there); the provider reads them and uploads only what the server lacks.
type UploadParams struct {
	// Key is the content-addressed key the entry is stored under.
	Key string `json:"key"`
	// Result is the freshly built ActionResult to store under Key.
	Result *ActionResult `json:"result"`
	// Manifest is the built file set; its digests' bytes must already be in the
	// blob-exchange directory (core exported them there).
	Manifest *Manifest `json:"manifest"`
	// Producer is an untrusted caller assertion about the entry's producer class.
	// The provider MUST derive and overwrite the persisted value from its
	// authenticated context; a client must never be able to self-declare CI.
	Producer Producer `json:"producer,omitempty"`
	// ProducerIdentity is an untrusted, opaque caller assertion about the entry's
	// producer identity. The provider MUST overwrite it before persisting.
	ProducerIdentity string `json:"producerIdentity,omitempty"`
	// Channel is an untrusted caller assertion about the entry's trust channel.
	// The provider MUST overwrite it before persisting.
	Channel Channel `json:"channel,omitempty"`
}

// UploadResult acknowledges that an entry was queued for background upload. The
// bytes are confirmed durable at OpSummary, not here.
type UploadResult struct {
	// Accepted reports whether the entry was queued for background upload.
	Accepted bool `json:"accepted"`
}

// --- run markers ---

// MarkerLookupParams reads the last successful whole-target run marker. The
// fields mirror RunMarkerRequest minus the HTTP protocol version (the envelope
// carries the provider version).
type MarkerLookupParams struct {
	// Workspace is the opaque workspace identity that scopes the marker.
	Workspace string `json:"workspace"`
	// Branch is the git branch the marker is scoped to.
	Branch string `json:"branch"`
	// Commands is the command list the marker must prove ran together.
	Commands []string `json:"commands"`
	// ParamsHash scopes the marker to a specific job-parameter set, when set.
	ParamsHash string `json:"paramsHash,omitempty"`
	// Selection is the project-selection scope the marker must cover.
	Selection RunMarkerSelection `json:"selection"`
}

// MarkerLookupResult reports whether a marker exists and, if so, its value.
type MarkerLookupResult struct {
	// Found reports whether a matching marker exists.
	Found bool `json:"found"`
	// Marker is the found marker's value, set when Found is true.
	Marker *RunMarker `json:"marker,omitempty"`
}

// MarkerWriteParams publishes a successful whole-target run marker.
type MarkerWriteParams struct {
	// Workspace is the opaque workspace identity that scopes the marker.
	Workspace string `json:"workspace"`
	// Branch is the git branch the marker is scoped to.
	Branch string `json:"branch"`
	// Commands is the command list this run completed successfully.
	Commands []string `json:"commands"`
	// ParamsHash scopes the marker to a specific job-parameter set, when set.
	ParamsHash string `json:"paramsHash,omitempty"`
	// Selection is the project-selection scope this run covered.
	Selection RunMarkerSelection `json:"selection"`
	// SHA is the commit hash to record as the new marker value.
	SHA string `json:"sha"`
	// ObservedSHA is the marker the client saw before the run, enabling
	// compare-and-swap against a stale late writer.
	ObservedSHA string `json:"observedSha,omitempty"`
}

// MarkerWriteResult reports whether the marker advanced (false is a benign
// compare-and-swap loss, not an error).
type MarkerWriteResult struct {
	// Published reports whether this write advanced the marker.
	Published bool `json:"published"`
	// Marker is the marker now in effect after the write.
	Marker *RunMarker `json:"marker,omitempty"`
}

// --- object cache ---

// MaxObjectNamespaceLength bounds an object-cache namespace, which is a short
// vocabulary word ("go-build"), not a path or a key.
const MaxObjectNamespaceLength = 64

// MaxObjectIDLength bounds an object id. Ids are hex sums (a Go action id is 64
// hex characters), so this leaves room for a longer digest while keeping a
// hostile peer from making one line unbounded.
const MaxObjectIDLength = 128

// ObjectGetParams looks up a batch of objects in ONE namespace. Batching is the
// point: a compiler asks for many entries at once, so one socket round trip
// answers a whole wave of lookups.
type ObjectGetParams struct {
	// Namespace scopes the id space, e.g. "go-build". It matches
	// [a-z0-9-]{1,MaxObjectNamespaceLength}; two namespaces never share an id.
	Namespace string `json:"namespace"`
	// IDs are the object ids to look up: opaque to the provider, lowercase hex,
	// unique within the namespace.
	IDs []string `json:"ids"`
	// AcceptChannels filters the hits by trust channel. An EMPTY list accepts
	// every channel, including the legacy empty channel a channel-less provider
	// reports; a run under an authoritative policy sends [ChannelTrusted] so a
	// developer-produced object can never reach it. An object on a rejected
	// channel is omitted from the result — it is a miss, not an error.
	AcceptChannels []Channel `json:"acceptChannels,omitempty"`
}

// ObjectHit is one object the provider served. Its bytes are present at
// BlobExchangePath(exchangeDir, Digest) when the response is written, exactly as
// for a restore hit, so the caller reads them from the local filesystem instead
// of the pipe.
type ObjectHit struct {
	// ID is the requested object id this hit answers.
	ID string `json:"id"`
	// Digest content-addresses the bytes (sha256:<hex>), which the provider has
	// placed at BlobExchangePath(exchangeDir, Digest). The caller verifies the
	// content against this digest, as it does for every other exchanged blob.
	Digest string `json:"digest"`
	// Size is the object's byte length.
	Size int64 `json:"size"`
	// Meta is opaque caller metadata stored with the object and returned
	// verbatim. It is never interpreted by core or the provider (Go's build
	// cache stores its output id here).
	Meta string `json:"meta,omitempty"`
	// Producer is the provider-authoritative producer class of the object. It is
	// omitted by a channel-less provider.
	Producer Producer `json:"producer,omitempty"`
	// Channel is the provider-authoritative trust channel of the object, derived
	// from the credential that stored it — never from the caller that asserted
	// it. An empty channel is the legacy signal, treated like an empty channel on
	// a task entry.
	Channel Channel `json:"channel,omitempty"`
}

// ObjectGetResult answers OpObjectGet. Misses are OMITTED rather than reported:
// a caller matches hits back to its requested ids and treats everything else as
// absent, so a filtered channel and an unknown id are indistinguishable by
// design.
type ObjectGetResult struct {
	// Objects are the hits, in any order. Requested ids with no hit are absent.
	Objects []ObjectHit `json:"objects"`
}

// ObjectPut offers one object for storage. Its bytes must already be at
// BlobExchangePath(exchangeDir, Digest) when the request is written.
type ObjectPut struct {
	// ID is the object id to store the bytes under, within the request's
	// namespace.
	ID string `json:"id"`
	// Digest content-addresses the bytes the caller already wrote into the
	// blob-exchange directory.
	Digest string `json:"digest"`
	// Size is the object's byte length.
	Size int64 `json:"size"`
	// Meta is opaque caller metadata to store with the object and return on a
	// later hit.
	Meta string `json:"meta,omitempty"`
}

// ObjectPutParams offers a batch of objects in ONE namespace. Puts are
// fire-and-forget: the provider queues them and the bytes are durable at
// OpSummary, so a compiler never waits on the network.
//
// Trust is the provider's job, exactly as for UploadParams: a caller cannot
// assert a channel here. A provider that receives puts from an untrusted
// credential still accepts them and stamps ChannelHint, so a pull-request run
// warms nothing an authoritative run will read.
type ObjectPutParams struct {
	// Namespace scopes the id space (see ObjectGetParams.Namespace).
	Namespace string `json:"namespace"`
	// Objects are the objects offered for storage.
	Objects []ObjectPut `json:"objects"`
}

// ObjectPutResult acknowledges that objects were QUEUED, not stored: the bytes
// are confirmed durable at OpSummary, like an upload.
type ObjectPutResult struct {
	// Accepted is the number of offered objects the provider queued.
	Accepted int `json:"accepted"`
}

// --- summary ---

// SummaryParams requests the end-of-run drain and statistics. It carries no
// fields today; it exists so the op has a stable, extensible body.
type SummaryParams struct{}

// SummaryResult reports the run's cache statistics after pending uploads drain.
type SummaryResult struct {
	// RestoredCount is the number of keys restored from the remote cache.
	RestoredCount int `json:"restoredCount,omitempty"`
	// RestoredBytes is the total bytes materialized from restores.
	RestoredBytes int64 `json:"restoredBytes,omitempty"`
	// UploadedCount is the number of entries uploaded to the remote cache.
	UploadedCount int `json:"uploadedCount,omitempty"`
	// UploadedBytes is the total bytes uploaded.
	UploadedBytes int64 `json:"uploadedBytes,omitempty"`
	// PrefetchedCount is the number of keys prefetched during the run.
	PrefetchedCount int `json:"prefetchedCount,omitempty"`
	// KnownDigests optionally returns the updated known-present set when the
	// provider owns presence tracking, so core can persist it for the next run.
	KnownDigests []string `json:"knownDigests,omitempty"`
}
