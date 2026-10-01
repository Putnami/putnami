// Package qualify defines the workload-qualification wire contract: the smoke
// contract the CLI derives from a workload's existing contracts, and the verdict
// it emits after executing that contract against a target.
//
// Two rules shape every type here.
//
// The contract is data, derived rather than authored. A Contract names the
// documents it was derived from and carries a digest over its requests, so two
// readers can tell whether they ran the same smoke.
//
// The verdict fails closed. State is a closed vocabulary in which exactly one
// value, StatePassed, is a pass; every other value — including the ones that
// read like an excuse, such as StateUnsupported and StateNotRun — is a non-pass
// a consumer must treat as "not proven". ParseAndValidateVerdict refuses a
// passed verdict that does not carry its proof.
package qualify

import diag "go.putnami.dev/protocol/diagnostic"

// ProtocolVersion is the version of the contract and verdict documents. A
// reader refuses any other value.
const ProtocolVersion = 1

// State is the closed outcome vocabulary shared by a verdict, its phases and
// its requests.
type State string

// State values. Only StatePassed is a pass.
const (
	// StatePassed means the step ran and proved what it was asked to prove.
	StatePassed State = "passed"
	// StateFailed means the step ran and disproved it: a request answered at or
	// above 500, a request failed in transport, or teardown left resources behind.
	StateFailed State = "failed"
	// StateUnsupported means nothing could be derived to execute: the workload
	// has no route inventory, or the inventory holds no safe request. It is never
	// a pass.
	StateUnsupported State = "unsupported"
	// StateNotRun means the step never started because an earlier step did not
	// pass.
	StateNotRun State = "not_run"
	// StateTimedOut means a deadline expired before the step could decide.
	StateTimedOut State = "timed_out"
	// StateCanceled means the caller canceled the run while the step was active.
	StateCanceled State = "canceled"
	// StateTargetUnreachable means no HTTP exchange with the target completed:
	// connection refused, DNS, TLS or a transport timeout.
	StateTargetUnreachable State = "target_unreachable"
	// StateDigestMismatch means the target does not report the expected build:
	// its /version sha differs from the expected one, or reports none.
	StateDigestMismatch State = "digest_mismatch"
	// StateCompositionFailed means a local target could not be composed.
	StateCompositionFailed State = "composition_failed"
)

// ValidStates is the closed State vocabulary, in declaration order.
var ValidStates = []State{
	StatePassed,
	StateFailed,
	StateUnsupported,
	StateNotRun,
	StateTimedOut,
	StateCanceled,
	StateTargetUnreachable,
	StateDigestMismatch,
	StateCompositionFailed,
}

// IsPass reports whether s is the single passing state.
func (s State) IsPass() bool { return s == StatePassed }

// IsValid reports whether s belongs to ValidStates.
func (s State) IsValid() bool {
	for _, valid := range ValidStates {
		if s == valid {
			return true
		}
	}
	return false
}

// Phase names, in execution order. A verdict records every one of them.
const (
	PhaseResolveTarget  = "resolve-target"
	PhaseReadiness      = "readiness"
	PhaseVersionBinding = "version-binding"
	PhaseSmoke          = "smoke"
	PhaseTeardown       = "teardown"
)

// PhaseNames is the closed, ordered set of phase names.
var PhaseNames = []string{
	PhaseResolveTarget,
	PhaseReadiness,
	PhaseVersionBinding,
	PhaseSmoke,
	PhaseTeardown,
}

// Binding kinds.
const (
	// BindingTree binds a verdict to the exact worktree a local target served.
	BindingTree = "tree"
	// BindingArtifact binds a verdict to the build a deployed target reports.
	BindingArtifact = "artifact"
)

// Target kinds.
const (
	// TargetLocal is a workload composed on this machine.
	TargetLocal = "local"
	// TargetURL is a running deployment reached over HTTP(S).
	TargetURL = "url"
)

// SourceHTTPRoutes is the only derivation source of protocol version 1: the
// putnami.http-routes.v1 inventory.
const SourceHTTPRoutes = "http-routes"

// Cleanup states.
const (
	// CleanupClean means teardown released every resource the target owned.
	CleanupClean = "clean"
	// CleanupPartial means teardown left resources behind; Leftovers names them.
	CleanupPartial = "partial"
)

// DefaultMaxStatus is the highest status a derived request accepts: anything
// below 500 proves the workload served the request without a server error.
const DefaultMaxStatus = 499

// Request is one smoke request of a contract.
type Request struct {
	// ID is "<method> <path>", unique within a contract.
	ID string `json:"id"`
	// Method is the HTTP method; version 1 derives GET or HEAD only.
	Method string `json:"method"`
	// Path is the route path, relative to the target base URL.
	Path string `json:"path"`
	// MaxStatus is the highest response status the request accepts.
	MaxStatus int `json:"maxStatus"`
	// Provenance is the source kind of the route fact the request was derived
	// from, for example "typed-api".
	Provenance string `json:"provenance"`
}

// Source names one document a contract was derived from.
type Source struct {
	// Kind is the source document kind; version 1 knows "http-routes" only.
	Kind string `json:"kind"`
	// Path is the workspace-relative, slash-separated path of the document read.
	Path string `json:"path"`
	// Digest is the digest the source document declares for itself.
	Digest string `json:"digest"`
}

// Contract is the derived smoke contract of one workload.
type Contract struct {
	// ProtocolVersion is the document version; it must equal ProtocolVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// Project is the canonical ID of the workload the contract belongs to.
	Project string `json:"project"`
	// DerivedFrom lists the documents the requests were derived from.
	DerivedFrom []Source `json:"derivedFrom"`
	// Requests are the smoke requests, in execution order. Empty means nothing
	// could be derived, which a verdict reports as unsupported.
	Requests []Request `json:"requests"`
	// Digest is "sha256:" plus the hex SHA-256 of the canonical JSON of Requests
	// (see ContractDigest).
	Digest string `json:"digest"`
}

// Target identifies what a verdict ran against.
type Target struct {
	// Kind is "local" or "url".
	Kind string `json:"kind"`
	// URL is the base URL requests were sent to. Required for a url target; it
	// never carries credentials.
	URL string `json:"url,omitempty"`
	// CompositionID identifies the local composition that served the target.
	CompositionID string `json:"compositionId,omitempty"`
}

// Binding states which build a verdict proves.
type Binding struct {
	// Kind is "tree" for a local target or "artifact" for a deployed one.
	Kind string `json:"kind"`
	// Fingerprint is the content digest of the worktree a tree binding served.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Dirty reports whether that worktree differed from HEAD.
	Dirty bool `json:"dirty,omitempty"`
	// HeadSHA is the commit the worktree sat on.
	HeadSHA string `json:"headSHA,omitempty"`
	// ExpectedSHA is the git sha an artifact binding requires the target to report.
	ExpectedSHA string `json:"expectedSHA,omitempty"`
	// ObservedSHA is the sha the target reported on /version, when it reported one.
	ObservedSHA string `json:"observedSHA,omitempty"`
	// Version is the version the target reported on /version, when it reported one.
	Version string `json:"version,omitempty"`
}

// Phase is one step of an execution.
type Phase struct {
	// Name is one of PhaseNames.
	Name string `json:"name"`
	// State is the step's outcome.
	State State `json:"state"`
	// DurationMs is the step's wall-clock duration in milliseconds.
	DurationMs int64 `json:"durationMs"`
	// Diagnostics explain a non-pass state; they carry no secret.
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
}

// RequestResult is the outcome of one contract request.
type RequestResult struct {
	// ID is the contract request's ID.
	ID string `json:"id"`
	// Status is the HTTP status the target answered, when it answered.
	Status int `json:"status,omitempty"`
	// DurationMs is the request's wall-clock duration in milliseconds.
	DurationMs int64 `json:"durationMs"`
	// State is the request's outcome.
	State State `json:"state"`
	// Reason explains a non-pass state.
	Reason string `json:"reason,omitempty"`
}

// ContractRef identifies the contract a verdict executed.
type ContractRef struct {
	// Digest is the executed contract's Digest.
	Digest string `json:"digest"`
	// Requests is the number of requests the contract holds.
	Requests int `json:"requests"`
	// DerivedFrom is the executed contract's DerivedFrom.
	DerivedFrom []Source `json:"derivedFrom"`
}

// Cleanup reports what teardown of a local target released.
type Cleanup struct {
	// State is "clean" or "partial".
	State string `json:"state"`
	// Leftovers names the resources a partial teardown left behind.
	Leftovers []string `json:"leftovers"`
}

// Verdict is the single outcome of qualifying one workload against one target.
type Verdict struct {
	// ProtocolVersion is the document version; it must equal ProtocolVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// Project is the canonical ID of the qualified workload.
	Project string `json:"project"`
	// Target identifies what the contract ran against.
	Target Target `json:"target"`
	// Binding states which build the verdict proves.
	Binding Binding `json:"binding"`
	// Contract identifies the executed contract.
	Contract ContractRef `json:"contract"`
	// Phases records every phase of PhaseNames, in order.
	Phases []Phase `json:"phases"`
	// Requests records every contract request, in contract order.
	Requests []RequestResult `json:"requests"`
	// State is the verdict: the first non-pass phase state, or passed.
	State State `json:"state"`
	// StartedAt is the RFC 3339 UTC time the run started.
	StartedAt string `json:"startedAt"`
	// FinishedAt is the RFC 3339 UTC time the run finished.
	FinishedAt string `json:"finishedAt"`
	// Cleanup reports teardown of a local target; absent for a url target.
	Cleanup *Cleanup `json:"cleanup,omitempty"`
}
