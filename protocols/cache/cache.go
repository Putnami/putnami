// Package cache defines the wire contract for the Putnami remote build cache:
// the batch "negotiate" request/response exchanged between the build-cache
// client and the cloud cache server, plus the Action Cache + CAS data shapes
// they agree on.
//
// The contract is plain HTTP+JSON. Both halves hand-maintain types that match
// these shapes; this package is the source of truth for the Go side and the
// reference shape for any implementation written in another language.
//
// # Model
//
//   - Cache keys are opaque, content-addressed sha256 hex strings computed by
//     the client from a task's inputs. They are deterministic and computable
//     for the whole build DAG before execution (see the CLI's plan-time
//     precompute), so the entire key set is negotiated in ONE round trip. The
//     server treats keys as opaque identifiers and never recomputes them.
//
//   - The store is split into an Action Cache (key -> ActionResult + Manifest)
//     and a content-addressed store, CAS (digest -> bytes). A Manifest lists a
//     task's output files with a per-file content Digest, so bytes dedup across
//     keys and are fetched lazily, only when actually needed.
//
//   - Bytes move over presigned URLs issued by the server. On a hit, the
//     negotiate response carries GET URLs to download CAS blobs. On a miss the
//     write path is a separate post-build exchange: negotiate runs before the
//     build and cannot know an artifact's digests, so the client submits the
//     built manifest to StorePath, the server replies with PUT URLs for only
//     the blobs it is missing (digests already in CAS are omitted — dedup), the
//     client uploads them, then commits the Action Cache entry at CommitPath.
//
// # Auth
//
// Every request carries a per-user bearer token in the Authorization header
// (see AuthorizationValue / BearerToken). There are no shared secrets; the
// server resolves identity and the cache namespace from the token's claims.
//
// # Eligibility
//
// Remote caching is stricter than local caching. SideEffectingTask excludes
// commands with external effects (e.g. publish) that must always run, and the
// break-even guard (WorthRemoteCaching / EligibleForRemote) skips artifacts
// whose predicted transfer time exceeds the build time they would save. Client
// and server share these helpers so they agree on which keys participate.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ProtocolVersion is the current contract version. It is echoed in every
// request and response so each side can reject a mismatch instead of
// misinterpreting fields.
const ProtocolVersion = 1

// NegotiatePath is the server route for the batch negotiate exchange.
const NegotiatePath = "/v1/cache/negotiate"

// RunMarkerLookupPath is the server route for reading the last successful
// whole-target run marker for a workspace, branch, command list, and selection
// scope. It is intentionally separate from action-cache keys: per-job hits do
// not prove that a full requested target passed together at one commit.
const RunMarkerLookupPath = "/v1/cache/run-marker/lookup"

// RunMarkerPublishPath is the server route for publishing a successful
// whole-target run marker after the client has completed the requested target.
const RunMarkerPublishPath = "/v1/cache/run-marker/publish"

// MaxKeysPerRequest bounds the number of keys in a single negotiate request.
// A whole-build key set is large but finite; servers may reject requests above
// this with CodeTooManyKeys, and clients should chunk accordingly.
const MaxKeysPerRequest = 5000

// --- auth ---

const (
	// AuthorizationHeader is the HTTP header carrying the per-user credential.
	AuthorizationHeader = "Authorization"
	// BearerScheme is the Authorization scheme used for per-user tokens.
	BearerScheme = "Bearer"
)

// AuthorizationValue formats a bearer token as an Authorization header value.
func AuthorizationValue(token string) string {
	return BearerScheme + " " + token
}

// BearerToken extracts the token from an Authorization header value. It
// reports false when the header is absent, uses another scheme, or is empty.
func BearerToken(header string) (string, bool) {
	rest, ok := strings.CutPrefix(header, BearerScheme+" ")
	if !ok {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false
	}
	return rest, true
}

// --- keys & digests ---

// KeyLength is the hex length of a cache key (a sha256 sum).
const KeyLength = 64

// ValidKey reports whether s is a well-formed cache key: lowercase sha256 hex.
func ValidKey(s string) bool {
	return isLowerHex(s, KeyLength)
}

// DigestAlgorithm is the hash used for CAS content addresses.
const DigestAlgorithm = "sha256"

// digestSep separates the algorithm from the hex sum in a Digest string.
const digestSep = ":"

// DigestOf returns the canonical CAS digest of b, formatted "sha256:<hex>".
func DigestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return DigestAlgorithm + digestSep + hex.EncodeToString(sum[:])
}

// ValidDigest reports whether s is a well-formed CAS digest of the form
// "sha256:<64 lowercase hex>".
func ValidDigest(s string) bool {
	rest, ok := strings.CutPrefix(s, DigestAlgorithm+digestSep)
	if !ok {
		return false
	}
	return isLowerHex(rest, KeyLength)
}

// isLowerHex reports whether s is exactly n lowercase hex characters.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// --- materialization mode ---

// Mode is the client's materialization intent for a build. The server may use
// it to decide how aggressively to prefetch, but it never changes hit/miss.
type Mode string

const (
	// ModeMinimal fetches only status (the CI gate): no output bytes move.
	ModeMinimal Mode = "minimal"
	// ModeToplevel materializes requested deliverables only.
	ModeToplevel Mode = "toplevel"
	// ModeFull materializes every output (local development).
	ModeFull Mode = "full"
)

// DefaultMode is assumed when a request omits the mode.
const DefaultMode = ModeMinimal

// Valid reports whether m is a recognized materialization mode.
func (m Mode) Valid() bool {
	switch m {
	case ModeMinimal, ModeToplevel, ModeFull:
		return true
	default:
		return false
	}
}

// --- transfer methods ---

const (
	// TransferGet marks a presigned download of a CAS blob (a hit).
	TransferGet = "GET"
	// TransferPut marks a presigned upload of a CAS blob the server lacks.
	TransferPut = "PUT"
)

// --- request ---

// NegotiateRequest is the single batch request sent for a whole build: every
// cacheable key, computed up front.
type NegotiateRequest struct {
	// ProtocolVersion is the contract version (ProtocolVersion); the server rejects
	// a mismatch rather than misreading fields.
	ProtocolVersion int `json:"protocolVersion"`
	// Mode is the client's materialization intent for the build.
	Mode Mode `json:"mode"`
	// Keys is the whole build's cacheable key set, computed up front.
	Keys []KeyRequest `json:"keys"`
}

// KeyRequest is one key in a negotiate request. The advisory fields are not
// part of the cache identity (the Key already captures inputs); they let the
// server apply policy (break-even, analytics) and namespace reporting.
type KeyRequest struct {
	// Key is the opaque content-addressed cache key.
	Key string `json:"key"`
	// Extension, Task, and Project identify the producing task for server-side
	// policy and reporting. Advisory only.
	Extension string `json:"extension,omitempty"`
	// Task is the producing task name (advisory; see Extension).
	Task string `json:"task,omitempty"`
	// Project is the producing project name (advisory; see Extension).
	Project string `json:"project,omitempty"`
	// DurationMs is the locally recorded build time for this task, used by the
	// server's break-even policy to avoid remote-caching artifacts that are
	// cheaper to rebuild than to transfer.
	DurationMs int64 `json:"durationMs,omitempty"`
	// SizeBytes is the expected output size when known.
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// --- successful run markers ---

// RunMarkerSelection identifies the project-selection scope a marker proves.
// The initial contract only records all-project/full-target runs.
type RunMarkerSelection string

const (
	// RunMarkerSelectionAll records a successful run whose target covered the
	// full workspace target set for the command list.
	RunMarkerSelectionAll RunMarkerSelection = "all"
)

// Valid reports whether s is a recognized marker selection scope.
func (s RunMarkerSelection) Valid() bool {
	switch s {
	case RunMarkerSelectionAll:
		return true
	default:
		return false
	}
}

// RunMarkerRequest reads the last successful run marker for a branch, command
// list, optional job-parameter hash, and full-target selection scope. Workspace
// is an opaque client-derived repository/workspace identity; the server also
// scopes markers by the authenticated cache namespace.
type RunMarkerRequest struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Workspace is the opaque client-derived repository/workspace identity that
	// scopes the marker.
	Workspace string `json:"workspace"`
	// Branch is the git branch the marker is scoped to.
	Branch string `json:"branch"`
	// Commands is the command list the marker proves ran successfully together.
	Commands []string `json:"commands"`
	// ParamsHash scopes the marker to a specific job-parameter set, when set.
	ParamsHash string `json:"paramsHash,omitempty"`
	// Selection is the project-selection scope the marker must cover.
	Selection RunMarkerSelection `json:"selection"`
}

// RunMarker is the remote value associated with a successful full-target run.
type RunMarker struct {
	// SHA is the commit hash the successful run was recorded at.
	SHA string `json:"sha"`
	// UpdatedAt is the server-recorded time the marker was written (RFC 3339).
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// RunMarkerResponse returns the marker when one exists. A nil Marker is a
// cache miss, not an error.
type RunMarkerResponse struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Marker is the found marker; nil means no marker exists (a miss, not error).
	Marker *RunMarker `json:"marker,omitempty"`
}

// PublishRunMarkerRequest publishes a successful run marker. ObservedSHA is the
// marker value the client selected from before the run, when known, so servers
// can reject stale late writers with compare-and-swap semantics.
type PublishRunMarkerRequest struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Workspace is the opaque client-derived workspace identity that scopes the
	// marker.
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
	// ObservedSHA is the marker the client selected before the run, enabling
	// compare-and-swap so a stale late writer is rejected.
	ObservedSHA string `json:"observedSha,omitempty"`
}

// PublishRunMarkerResponse reports whether the marker was advanced. Published
// may be false for benign races, for example when another run already published
// a different marker.
type PublishRunMarkerResponse struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Published reports whether this request advanced the marker; false on a
	// benign race where another run already published a different marker.
	Published bool `json:"published"`
	// Marker is the marker now in effect after the request.
	Marker *RunMarker `json:"marker,omitempty"`
}

// --- response ---

// NegotiateResponse holds one KeyResult per requested key, in request-key set.
type NegotiateResponse struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Results holds one KeyResult per requested key.
	Results []KeyResult `json:"results"`
}

// KeyResult is the per-key outcome of a negotiate.
type KeyResult struct {
	// Key echoes the requested cache key.
	Key string `json:"key"`
	// Hit reports whether the key was found in the cache.
	Hit bool `json:"hit"`
	// Result and Manifest are present on a hit: the cached status/metadata and
	// the file set with per-file digests.
	Result *ActionResult `json:"result,omitempty"`
	// Manifest is the hit's file set with per-file content digests (see Result).
	Manifest *Manifest `json:"manifest,omitempty"`
	// Downloads are presigned GET transfers for the CAS blobs needed to
	// materialize this hit. Empty in ModeMinimal.
	Downloads []BlobTransfer `json:"downloads,omitempty"`
}

// BlobTransfer is a presigned transfer of a single CAS blob.
type BlobTransfer struct {
	// Digest is the CAS content address ("sha256:<hex>").
	Digest string `json:"digest"`
	// URL is the presigned endpoint for the transfer.
	URL string `json:"url"`
	// Method is TransferGet (download) or TransferPut (upload).
	Method string `json:"method"`
	// Headers are required on the presigned request (e.g. content type).
	Headers map[string]string `json:"headers,omitempty"`
	// SizeBytes is the blob size when known.
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// Manifest is the Action Cache's file set for a key: each output file with the
// content digest that addresses its bytes in CAS.
type Manifest struct {
	// Files is the task's output file set, each with its CAS content digest.
	Files []FileEntry `json:"files"`
}

// FileEntry is one output file in a Manifest.
type FileEntry struct {
	// Path is slash-separated, relative to the task's output root.
	Path string `json:"path"`
	// Digest is the CAS content address of the file's bytes.
	Digest string `json:"digest"`
	// Mode is the unix file mode bits to restore.
	Mode uint32 `json:"mode"`
	// Size is the file size in bytes.
	Size int64 `json:"size,omitempty"`
}

// ActionResult is the cached outcome of a task: the status the client restores
// without re-running, plus policy metadata. It mirrors the client's local
// store result so a remote hit and a local hit are interchangeable.
type ActionResult struct {
	// Status is the cached terminal status ("success" or a deterministic
	// "skipped").
	Status string `json:"status"`
	// Data is the structured result payload, if any.
	Data map[string]any `json:"data,omitempty"`
	// Error carries a cached structured error (e.g. for a cached failure mode).
	Error *ActionError `json:"error,omitempty"`
	// DurationMs is the build time that produced this result.
	DurationMs int64 `json:"durationMs,omitempty"`
	// SizeBytes is the total size of the manifest's bytes.
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// Events is a compact replayable subset of the task's JSONL events used to
	// restore inline summaries on cache hits. It should contain only small,
	// result-describing events such as meta, metric, artifact, diagnostic, and
	// summary events, never streaming logs or progress.
	Events []ActionEvent `json:"events,omitempty"`
}

// ActionError is a cached structured error.
type ActionError struct {
	// Message is the human-readable cached error text.
	Message string `json:"message,omitempty"`
	// Code is the machine-readable cached error code.
	Code string `json:"code,omitempty"`
}

// ActionEvent is a compact cached JSONL event attached to ActionResult. It
// mirrors the event fields the CLI renderers consume without tying the wire
// protocol to the CLI's internal RawJobEvent type.
type ActionEvent struct {
	// Version is the event schema version, when stamped.
	Version int `json:"v,omitempty"`
	// Type is the event type (e.g. "metric", "artifact", "summary").
	Type string `json:"type"`
	// Time is the event's RFC 3339 timestamp, when recorded.
	Time string `json:"time,omitempty"`
	// Level is the event severity/level, when applicable.
	Level string `json:"level,omitempty"`
	// Message is the event's human-readable text, when present.
	Message string `json:"message,omitempty"`
	// Data is the event's structured payload.
	Data map[string]any `json:"data,omitempty"`
}

// --- store / commit (write path) ---
//
// Negotiate (the read path) runs before the build, so on a first-build miss the
// server cannot know the artifact's digests and cannot pre-issue upload URLs.
// The write path is therefore a separate post-build exchange in two steps:
//
//  1. StorePath: the client submits the freshly built Manifest (the file set
//     with per-file content digests). The server replies with presigned PUT
//     BlobTransfers for ONLY the digests not already in CAS, so a re-store or a
//     blob shared with another key uploads nothing (dedup).
//  2. The client uploads each missing blob over its presigned URL, then calls
//     CommitPath with the key, ActionResult, and Manifest to finalize the
//     Action Cache entry (key -> result + manifest). Commit carries the whole
//     entry, so it is self-contained and idempotent: re-committing a key is a
//     no-op, and the server can confirm the referenced blobs are in CAS before
//     making the entry servable.

// StorePath is the server route for the post-build store exchange (step 1).
const StorePath = "/v1/cache/store"

// CommitPath is the server route that finalizes an Action Cache entry (step 2).
const CommitPath = "/v1/cache/commit"

// StoreRequest submits a freshly built file set so the server can report which
// blobs it still needs. It carries only the Manifest (the digests); the cached
// result rides on the CommitRequest that finalizes the entry.
type StoreRequest struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Key is the opaque content-addressed key the entry will be stored under.
	Key string `json:"key"`
	// Manifest is the built file set with per-file content digests.
	Manifest *Manifest `json:"manifest"`
}

// StoreResponse tells the client which blobs to upload before it commits.
type StoreResponse struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Key echoes the request key.
	Key string `json:"key"`
	// Uploads are presigned PUT transfers for the digests not already in CAS
	// (dedup). It is empty when every blob is already present — the client then
	// skips straight to commit without uploading anything.
	Uploads []BlobTransfer `json:"uploads,omitempty"`
}

// CommitRequest finalizes an Action Cache entry after its blobs are uploaded.
// It is self-contained (it carries the full result and manifest) so it is
// idempotent and does not depend on server-side state from the store step.
type CommitRequest struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Key is the opaque content-addressed key the entry is stored under.
	Key string `json:"key"`
	// Result is the cached outcome to store under Key.
	Result *ActionResult `json:"result"`
	// Manifest is the file set the entry resolves to; its digests must already
	// be present in CAS (uploaded in the store step or deduped away).
	Manifest *Manifest `json:"manifest"`
}

// CommitResponse reports the outcome of finalizing an entry.
type CommitResponse struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Key echoes the committed key.
	Key string `json:"key"`
	// Committed is true once the entry is durably stored and servable. A commit
	// for an already-present key is a no-op and still reports true.
	Committed bool `json:"committed"`
}

// ErrorResponse is the body returned for a request the server rejects whole
// (auth, version, malformed). Per-key misses are not errors — they ride in
// NegotiateResponse.
type ErrorResponse struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Code is the machine-readable rejection code (e.g. CodeTooManyKeys).
	Code string `json:"code"`
	// Message is the human-readable rejection reason.
	Message string `json:"message"`
}

// --- batched write path & capability negotiation (additive, protocol v1.x) ---
//
// store/commit finalize one key per round trip, so a build with M built misses
// spends ~2M control round trips, drained at the end. These additive endpoints
// collapse that without changing the wire protocolVersion: the client probes
// CapabilitiesPath once, and when the server advertises the batched write path it
// uses FindMissingPath to dedup a whole window's blobs in one request and
// CommitBatchPath to finalize that window's entries in one request. A server
// that does not implement them (404) — or a client that does not probe — keeps
// using store/commit unchanged. New endpoints, never new fields on the existing
// messages, so a strict (DisallowUnknownFields) parser on either side is safe.

// CapabilitiesPath is the GET route advertising which optional endpoints and
// object-store features the server supports beyond the v1 baseline. A 404 (or
// any non-2xx) means "baseline only" and the client falls back to store/commit.
const CapabilitiesPath = "/v1/cache/capabilities"

// FindMissingPath reports which of a set of blobs the CAS still needs, batched
// across a whole window of built misses (the batched analog of StorePath).
const FindMissingPath = "/v1/cache/find-missing"

// CommitBatchPath finalizes several Action Cache entries in one request (the
// batched analog of CommitPath).
const CommitBatchPath = "/v1/cache/commit-batch"

// Capability strings advertised in CapabilitiesResponse.Capabilities.
const (
	// CapabilityFindMissing: the server implements FindMissingPath and
	// CommitBatchPath, so the client can batch the write path. The two ship
	// together; CapabilityFindMissing alone is enough to select the batched path.
	CapabilityFindMissing = "find-missing"
	// CapabilityCommitBatch is advertised alongside CapabilityFindMissing for
	// explicitness.
	CapabilityCommitBatch = "commit-batch"
	// CapabilityDirectCASPut: the server issues a direct-write grant
	// (UploadGrantPath), so the client can PUT built blobs straight to CAS — no
	// per-blob find-missing round trip. Layered on the find-missing batched path
	// (the client falls back to it when this is absent or the grant fetch fails).
	// Integrity rests on the client's content-addressing plus per-workspace CAS
	// isolation; the server does not re-validate content (the same trust model the
	// presigned find-missing upload already uses).
	CapabilityDirectCASPut = "direct-cas-put"
	// CapabilityUploadBatch: the server implements UploadBatchPath, so the client
	// can coalesce many small blobs into one request instead of a PUT each.
	CapabilityUploadBatch = "upload-batch"
	// CapabilityDownloadBatch: the server implements DownloadBatchPath, so the
	// client can fetch many small CAS blobs in one response instead of a presigned
	// GET each — the read-path analog of CapabilityUploadBatch. Materializing a hit
	// whose build emitted hundreds of tiny files is otherwise bound by per-blob
	// round trips, not bytes.
	CapabilityDownloadBatch = "download-batch"
	// CapabilityActionEvents: the server accepts and persists ActionResult.Events
	// in commit and commit-batch requests, and may return them on negotiate hits.
	// Clients should not send cached events to strict older servers unless this
	// capability is advertised.
	CapabilityActionEvents = "action-events"
)

// CapabilitiesResponse advertises the server's optional capabilities. An older
// server returns 404 for the route; the client treats that as no capabilities.
type CapabilitiesResponse struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Capabilities is the set of advertised capability strings (see Capability*).
	Capabilities []string `json:"capabilities,omitempty"`
}

// BlobRef is a CAS blob the client intends to upload: its content digest and
// size. FindMissingRequest carries a set of these so the server can dedup them
// against CAS in one round trip.
type BlobRef struct {
	// Digest is the CAS content address ("sha256:<hex>").
	Digest string `json:"digest"`
	// SizeBytes is the blob size when known.
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// FindMissingRequest submits the deduped blob set for a window of built misses.
type FindMissingRequest struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Blobs is the set of blobs the client wants to upload. The server replies
	// with presigned PUT transfers for only those not already in CAS (dedup).
	Blobs []BlobRef `json:"blobs"`
}

// FindMissingResponse tells the client which blobs to upload before committing.
type FindMissingResponse struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Uploads are presigned PUT transfers for the digests not already in CAS.
	// Empty means every requested blob is already present.
	Uploads []BlobTransfer `json:"uploads,omitempty"`
}

// CommitEntry is one Action Cache entry to finalize in a CommitBatchRequest. It
// carries the whole entry (key, result, manifest), so the batch is
// self-contained and idempotent like a single CommitRequest.
type CommitEntry struct {
	// Key is the opaque content-addressed key the entry is stored under.
	Key string `json:"key"`
	// Result is the cached outcome to store under Key.
	Result *ActionResult `json:"result"`
	// Manifest is the file set the entry resolves to; its digests must already be
	// in CAS.
	Manifest *Manifest `json:"manifest"`
}

// CommitBatchRequest finalizes several entries in one request. Each entry's
// blobs must already be in CAS (uploaded after find-missing or deduped away).
type CommitBatchRequest struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Entries are the Action Cache entries to finalize in one request.
	Entries []CommitEntry `json:"entries"`
}

// CommitResult reports one entry's commit outcome.
type CommitResult struct {
	// Key echoes the entry key.
	Key string `json:"key"`
	// Committed is true once the entry is durably stored and servable.
	Committed bool `json:"committed"`
}

// CommitBatchResponse reports the outcome of each entry in the batch.
type CommitBatchResponse struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Results holds one CommitResult per entry in the request.
	Results []CommitResult `json:"results"`
}

// --- direct-write grant (Phase 2 accelerator, additive) ---
//
// With CapabilityDirectCASPut the client fetches one UploadGrant and then PUTs
// built blobs straight to CAS, skipping the find-missing discovery round trip.
// It is backend-agnostic: the server supplies the URL template, method, and the
// headers (credential, content type, and a conditional-create header) the client
// sends verbatim — only the per-blob digest is substituted client-side. commit-
// batch still finalizes the entries. When the grant is absent the client uses
// the find-missing path, so this is a pure accelerator.

// UploadGrantPath is the route that issues a direct-write grant.
const UploadGrantPath = "/v1/cache/upload-grant"

// DigestPlaceholder is the token in UploadGrant.URLTemplate the client replaces
// with each blob's digest hex (the lowercase hex after the "sha256:" prefix) to
// form that blob's PUT URL.
const DigestPlaceholder = "{digest}"

// UploadGrant authorizes direct CAS writes without a find-missing round trip.
// The credential it carries is short-lived and scoped to the caller's workspace
// CAS prefix.
type UploadGrant struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// URLTemplate is the blob PUT URL containing DigestPlaceholder, which the
	// client replaces with the blob's digest hex.
	URLTemplate string `json:"urlTemplate"`
	// Method is the upload method (PUT).
	Method string `json:"method"`
	// Headers are sent on every direct PUT verbatim — typically a scoped
	// credential, a content type, and a conditional-create header that makes the
	// store reject (not overwrite) an already-present blob.
	Headers map[string]string `json:"headers,omitempty"`
	// ConditionalExistsStatus is the HTTP status the store returns when the blob
	// already exists (the conditional-create precondition failed); the client
	// treats it as a dedup skip, not an error. 0 means no conditional create — the
	// client uploads unconditionally and an existing blob is overwritten with
	// identical, content-addressed bytes.
	ConditionalExistsStatus int `json:"conditionalExistsStatus,omitempty"`
}

// --- batched small-blob upload (coalescing, additive) ---
//
// A find-missing/direct upload is one request per blob. A build that emits many
// tiny files (a JS bundle's hundreds of sub-kB outputs) then pays per-request
// overhead that dwarfs the bytes. UploadBatchPath coalesces several small blobs'
// bytes into ONE request — the way Bazel's BatchUpdateBlobs does — so a window
// of tiny outputs is stored in a single round trip. Large blobs keep using the
// per-blob path (find-missing or the direct grant); the client splits by size.
// Gated on CapabilityUploadBatch.

// UploadBatchPath is the route that stores several small blobs inline in one
// request.
const UploadBatchPath = "/v1/cache/upload-batch"

// MaxInlineBlobBytes is the largest blob the client sends inline via
// UploadBatchPath; larger blobs use the per-blob path. It keeps coalescing aimed
// at the many-tiny-files case and bounds a batch request's size.
const MaxInlineBlobBytes = 256 << 10 // 256 KiB

// InlineBlob is one blob carried inline in an UploadBatchRequest. Data is the
// blob bytes (base64 in JSON) and MAY be gzip-compressed — Digest is always the
// UNCOMPRESSED content hash (per the CAS convention), so the server stores Data
// opaquely under Digest exactly like a presigned upload. A server may validate
// by decompressing Data and hashing it; the trust model does not require it.
type InlineBlob struct {
	// Digest is the blob's UNCOMPRESSED CAS content address ("sha256:<hex>"),
	// even when Data is gzip-compressed.
	Digest string `json:"digest"`
	// Data is the blob bytes (base64 in JSON) and MAY be gzip-compressed.
	Data []byte `json:"data"`
}

// UploadBatchRequest carries several small blobs in one request.
type UploadBatchRequest struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Blobs are the small blobs carried inline for storage in one request.
	Blobs []InlineBlob `json:"blobs"`
}

// UploadBatchResponse lists the digests now present in CAS (stored by this
// request or already there). The client treats any of its blobs missing from
// Stored as not-yet-uploaded and falls back to the per-blob path for them.
type UploadBatchResponse struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Stored lists the digests now present in CAS; a client blob absent here is
	// treated as not-yet-uploaded and retried on the per-blob path.
	Stored []string `json:"stored,omitempty"`
}

// --- batched small-blob download (coalescing, additive) ---
//
// The read-path mirror of UploadBatchPath. A negotiate hit lists one presigned
// GET per CAS blob (KeyResult.Downloads), so materializing a build that emitted
// many tiny files (a TypeScript build's hundreds of sub-kB .d.ts outputs) pays a
// round trip per blob — latency that dwarfs the bytes. When the server advertises
// CapabilityDownloadBatch the client groups the small blobs (those whose
// BlobTransfer.SizeBytes is within MaxInlineBlobBytes) and fetches them in ONE
// DownloadBatchPath request; large blobs keep using their presigned GET. A digest
// the server cannot serve (evicted, or never present) is omitted from the
// response, not an error — the client falls back to that blob's presigned GET —
// so this is a pure accelerator. Gated on CapabilityDownloadBatch.

// DownloadBatchPath is the route that returns several small blobs inline in one
// response (the read-path analog of UploadBatchPath).
const DownloadBatchPath = "/v1/cache/download-batch"

// DownloadBatchRequest asks the server to return several small CAS blobs inline
// in one response. Digests is the set the client wants — typically the small
// blobs of a negotiate hit; large blobs stay on their presigned GET.
type DownloadBatchRequest struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Digests is the set of (typically small) CAS blobs to return inline.
	Digests []string `json:"digests"`
}

// DownloadBatchResponse returns the requested blobs inline, reusing InlineBlob:
// Data is the blob bytes (base64 in JSON) and MAY be gzip-compressed, while
// Digest is always the UNCOMPRESSED content hash (the CAS convention), so the
// client verifies each blob by content-addressing exactly as it does a presigned
// download. A requested digest the server omits from Blobs is not an error: the
// client materializes that blob from its presigned GET instead.
type DownloadBatchResponse struct {
	// ProtocolVersion is the contract version (ProtocolVersion).
	ProtocolVersion int `json:"protocolVersion"`
	// Blobs are the requested CAS blobs returned inline; a requested digest the
	// server omits is not an error (the client falls back to its presigned GET).
	Blobs []InlineBlob `json:"blobs,omitempty"`
}
