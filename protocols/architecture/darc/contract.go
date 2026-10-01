package darc

import (
	"errors"
	"fmt"
	"strings"
	"time"

	archproto "go.putnami.dev/protocol/architecture"
	diag "go.putnami.dev/protocol/diagnostic"
)

// The errors a component returns for a declared behavior. Each names the clause
// of the contract that produced it, so a caller can branch on the promise
// rather than on a message.
var (
	// ErrMissing reports a fact the local model does not hold, under a
	// `consistency.onMissing` of fail-closed or unavailable.
	ErrMissing = errors.New("darc: the projected fact is not available")
	// ErrStale reports a copy older than `consistency.maxStaleness` under a
	// `consistency.onStale` of fail-closed.
	ErrStale = errors.New("darc: the projected fact is older than the declared freshness bound")
	// ErrLateUpdate reports an update older than the local source version under
	// a `consistency.lateEvents` of reject.
	ErrLateUpdate = errors.New("darc: the update is older than the local source version")
	// ErrWriterClaimed reports a second attempt to obtain the writer handle. A
	// local model names ONE writer; a second one is the invariant this exists
	// to make unstatable.
	ErrWriterClaimed = errors.New("darc: the projection writer is already claimed")
	// ErrNotTheWriter reports a claim under a name the local model does not
	// name as its writer.
	ErrNotTheWriter = errors.New("darc: the local model names a different writer")
	// ErrDeletionNotApplicable reports a delete against a contract whose
	// deletion strategy is not-applicable.
	ErrDeletionNotApplicable = errors.New("darc: the contract declares no deletion strategy")
	// ErrImmutable reports an attempt to re-attach a snapshot version with
	// different content. A snapshot is replaced, never edited.
	ErrImmutable = errors.New("darc: a snapshot version is immutable")
	// ErrNotActive reports use of a contract that is not active — a planned
	// target, or one carried by a transport that is not itself usable yet.
	ErrNotActive = errors.New("darc: the contract is not active")
)

// Freshness is how a read describes the copy it returned.
//
// The vocabulary is this package's, not the protocol's: a manifest names the
// FIELD that carries freshness (`localModel.freshnessField`) and deliberately
// leaves its values to the consumer, because what counts as usable differs per
// contract. These two are the only distinction the declared staleness bound can
// support.
type Freshness string

const (
	// FreshnessFresh marks a copy inside the declared staleness bound.
	FreshnessFresh Freshness = "fresh"
	// FreshnessStale marks a copy outside it, returned because the contract
	// declares use-stale or fail-open rather than fail-closed.
	FreshnessStale Freshness = "stale"
)

// Record is one copied fact with the three metadata fields the local model is
// required to declare. They are structure rather than convention on purpose: a
// projection whose provenance or observation time lived in a comment could not
// be checked, and the contract requires them to exist.
type Record[T any] struct {
	// ID is the value of the declared source identity.
	ID string
	// Value is the projected copy.
	Value T
	// Provenance is the producer contract this copy came from — the export the
	// import names, which is what `localModel.provenanceField` records.
	Provenance string
	// ObservedAt is when the source state this copy reflects was observed.
	ObservedAt time.Time
	// Freshness is the read's verdict against the declared staleness bound.
	Freshness Freshness
	// SourceVersion is the producer state version this copy carries.
	SourceVersion string
	// Deleted reports a tombstoned record: the producer removed the fact and the
	// contract declares tombstone deletion, so the copy is retained and marked.
	Deleted bool
	// DeletedAt is when the tombstone was applied, zero for a live record.
	DeletedAt time.Time
}

// ContractError reports a contract a component refuses to run.
type ContractError struct {
	// Import is the contract's ID, empty when the contract does not have one.
	Import string
	// Diagnostics are the protocol's own findings, when the contract failed the
	// protocol's validation rather than a mode-specific requirement.
	Diagnostics []diag.Diagnostic
	// Reason explains a refusal the protocol itself cannot express — a mode
	// mismatch, or a rebuild strategy with no hook to run.
	Reason string
}

// Error renders the refusal, one line per protocol violation.
func (e *ContractError) Error() string {
	name := e.Import
	if name == "" {
		name = "<unnamed>"
	}
	if len(e.Diagnostics) == 0 {
		return fmt.Sprintf("darc: contract %s cannot be enforced: %s", name, e.Reason)
	}
	lines := make([]string, 0, len(e.Diagnostics)+1)
	lines = append(lines, fmt.Sprintf("darc: contract %s has %d protocol violation(s):", name, len(e.Diagnostics)))
	for _, d := range e.Diagnostics {
		lines = append(lines, "  - "+d.String())
	}
	return strings.Join(lines, "\n")
}

// validateContract applies the protocol's own verdict to one import, then the
// mode-specific requirement of the component being built.
//
// The import is wrapped in a synthetic one-domain manifest because
// ValidateManifest is the protocol's entry point and there is no smaller one.
// The synthetic domain and owner are valid by construction, so every diagnostic
// that can come back is about `imports[0]` — the contract the caller passed.
// Re-implementing the import half here would be a second opinion about what a
// valid contract is, which is exactly what a component enforcing a contract must
// not have.
func validateContract(contract archproto.Import, mode archproto.AccessMode) error {
	if contract.Mode != mode {
		return &ContractError{
			Import: contract.ID,
			Reason: fmt.Sprintf("declares %s access, and this component enforces %s access", contract.Mode, mode),
		}
	}
	synthetic := &archproto.Manifest{
		ProtocolVersion: archproto.ProtocolVersion,
		Domain:          "darc",
		Owner:           "darc",
		Projects:        []string{},
		Exports:         []archproto.Export{},
		Imports:         []archproto.Import{contract},
	}
	if errs := diag.Errors(archproto.ValidateManifest(synthetic)); len(errs) > 0 {
		return &ContractError{Import: contract.ID, Diagnostics: errs}
	}
	return nil
}

// requireActive refuses a contract that is not live.
//
// A planned import is a TARGET: the protocol already forbids it from claiming a
// current project binding, and running it would be the same claim in code. The
// transport is checked with it because an active import carried by a planned
// transport is a design that has not shipped, whichever half is behind.
func requireActive(contract archproto.Import, carriers ...*archproto.Transport) error {
	if contract.Status != archproto.StatusActive {
		return fmt.Errorf("%w: import %s is %s", ErrNotActive, contract.ID, contract.Status)
	}
	for _, carrier := range carriers {
		if carrier != nil && carrier.Availability != archproto.StatusActive {
			return fmt.Errorf("%w: import %s is carried by a %s transport", ErrNotActive, contract.ID, carrier.Availability)
		}
	}
	return nil
}

// staleness resolves the declared freshness bound. The protocol has already
// accepted it as a positive Go duration, so a parse failure here is impossible
// and the zero fallback is unreachable rather than a default.
func staleness(consistency *archproto.Consistency) time.Duration {
	if consistency == nil {
		return 0
	}
	bound, err := time.ParseDuration(consistency.MaxStaleness)
	if err != nil {
		return 0
	}
	return bound
}

// VersionOrder compares two producer state versions. It returns a negative
// number when left is older, zero when they are the same state, and a positive
// number when left is newer.
type VersionOrder func(left, right string) int

// lexicalVersionOrder is the default comparator.
//
// It is correct for every version shape whose byte order IS its time order —
// RFC 3339 timestamps, ULIDs, ordered UUIDs, zero-padded counters — and WRONG
// for unpadded decimal integers, where "10" sorts before "9". A contract using
// those must pass [WithVersionOrder]; the default is stated rather than guessed
// because a comparator that silently mis-orders makes the late-event rule
// report the opposite of what happened.
func lexicalVersionOrder(left, right string) int {
	return strings.Compare(left, right)
}

// stampFreshness records the read's verdict against the declared bound. It is
// separate from the refusal below because two callers need only the stamp: an
// enumeration is asking what the projection holds, and a version-addressed
// snapshot read is a caller naming the exact state it wants.
func stampFreshness[T any](record *Record[T], now time.Time, bound time.Duration) {
	record.Freshness = FreshnessFresh
	if bound <= 0 {
		return
	}
	if now.After(record.ObservedAt.Add(bound)) {
		record.Freshness = FreshnessStale
	}
}

// staleVerdict reports what a stale copy means under the declared stale
// behavior.
//
// fail-closed refuses. Every other value proceeds, and the record has already
// been stamped: `use-stale` and `fail-open` differ in intent, not in what the
// runtime can do, so the difference a consumer acts on is the stamp rather than
// the return.
func staleVerdict[T any](record Record[T], bound time.Duration, onStale archproto.FailureMode) error {
	if record.Freshness != FreshnessStale || onStale != archproto.FailureFailClosed {
		return nil
	}
	return fmt.Errorf("%w: observed at %s, bound %s",
		ErrStale, record.ObservedAt.UTC().Format(time.RFC3339), bound)
}

// missingVerdict reports what a read of an absent fact returns under the
// declared missing behavior. fail-open is the only value that lets a caller
// continue without an error, which is what it means.
func missingVerdict(contract archproto.Import) error {
	if contract.Consistency == nil || contract.Consistency.OnMissing == archproto.FailureFailOpen {
		return nil
	}
	return fmt.Errorf("%w: import %s declares onMissing %s", ErrMissing, contract.ID, contract.Consistency.OnMissing)
}
