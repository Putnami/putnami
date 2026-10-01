// Package transaction defines the Putnami canonical transaction / unit-of-work
// protocol: the single cross-language contract for describing how a transactional
// unit of work runs (its propagation relative to an enclosing transaction and
// its isolation level) and for reporting the typed Outcome of running it. Go,
// TypeScript, and (later) Python adapters consume the same shapes and result
// codes instead of each modeling transaction semantics and error taxonomy
// differently.
//
// This package is declaration + validation only. It has no dependency on a
// database driver or a runtime, so a framework, a deployer, and the conformance
// corpus can all depend on it without a cycle. It defines the shapes, the closed
// enums, and the strict parse/validate helpers; it opens no connection and runs
// no transaction.
//
// Two wire shapes, deliberately split into a request-side descriptor and a
// result-side envelope:
//
//   - UnitOfWork: the request-side descriptor a caller hands to a transaction
//     runner — how the unit of work should be run (propagation, isolation,
//     read-only). It carries no result.
//
//   - Result: the result-side envelope a runner returns — the typed
//     Outcome result code plus a Retryable advisory that mirrors
//     protocol/cache and protocol/events, telling the caller whether re-running
//     the unit of work may succeed.
//
// The protocol is Postgres-first: the Isolation enum maps to Postgres isolation
// levels. Adding an enum value or changing a shape is a backwards-incompatible
// change that bumps ProtocolVersion so adapters can decide how to react. See
// README.md for the propagation/isolation semantics and the Outcome taxonomy.
package transaction

// ProtocolVersion is the current transaction-protocol version. It is stamped
// into every UnitOfWork and Result and pinned by the JSON schemas.
// Bumped on any backwards-incompatible change to the shapes, enums, or
// semantics; adding an optional field within an existing shape does not bump it.
const ProtocolVersion = 1

// Propagation describes how a unit of work composes with an enclosing
// transaction. The enum is intentionally closed; adding a value requires a
// ProtocolVersion bump so runners can decide how to react.
type Propagation string

// Propagation values.
const (
	// PropagationRequired joins the enclosing transaction if one exists,
	// otherwise starts a new one. This is the default composition.
	PropagationRequired Propagation = "required"
	// PropagationRequiresNew always starts a new, independent transaction,
	// suspending any enclosing one until the inner unit of work completes.
	PropagationRequiresNew Propagation = "requires-new"
	// PropagationNested joins the enclosing transaction as an outer participant
	// WITHOUT establishing a savepoint — a rollback of the nested unit of work
	// rolls back the whole enclosing transaction. This "join-outer, no
	// savepoints" behavior matches the Putnami frameworks' current runtime
	// semantics; true savepoint-scoped nesting is not part of v1.
	PropagationNested Propagation = "nested"
)

// Valid reports whether p is a recognized propagation mode.
func (p Propagation) Valid() bool {
	switch p {
	case PropagationRequired, PropagationRequiresNew, PropagationNested:
		return true
	default:
		return false
	}
}

// Isolation is the transaction isolation level, mapped one-to-one onto the
// Postgres isolation levels. The enum is intentionally closed.
type Isolation string

// Isolation values map to Postgres isolation levels.
const (
	// IsolationReadCommitted maps to Postgres READ COMMITTED.
	IsolationReadCommitted Isolation = "read-committed"
	// IsolationRepeatableRead maps to Postgres REPEATABLE READ.
	IsolationRepeatableRead Isolation = "repeatable-read"
	// IsolationSerializable maps to Postgres SERIALIZABLE.
	IsolationSerializable Isolation = "serializable"
)

// Valid reports whether i is a recognized isolation level.
func (i Isolation) Valid() bool {
	switch i {
	case IsolationReadCommitted, IsolationRepeatableRead, IsolationSerializable:
		return true
	default:
		return false
	}
}

// Outcome is the typed result code of running a unit of work. The enum is
// intentionally closed so callers can switch exhaustively on a stable taxonomy
// across languages.
type Outcome string

// Outcome values.
const (
	// OutcomeApplied means the unit of work committed successfully.
	OutcomeApplied Outcome = "applied"
	// OutcomeAlreadyConsumedConflict means the work targeted a resource that was
	// already consumed (e.g. an idempotency key or a once-only token), so it was
	// rejected as a conflict rather than re-applied.
	OutcomeAlreadyConsumedConflict Outcome = "already-consumed-conflict"
	// OutcomeNotFound means a required target row/resource did not exist, so the
	// unit of work could not be applied.
	OutcomeNotFound Outcome = "not-found"
	// OutcomeRetryableSerializationFailure means the transaction aborted with a
	// serialization/deadlock failure that the caller may safely retry. This is
	// the only outcome for which Retryable is true.
	OutcomeRetryableSerializationFailure Outcome = "retryable-serialization-failure"
)

// Valid reports whether o is a recognized outcome.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeApplied, OutcomeAlreadyConsumedConflict, OutcomeNotFound, OutcomeRetryableSerializationFailure:
		return true
	default:
		return false
	}
}

// Retryable reports whether re-running the unit of work may succeed. Only a
// serialization failure is inherently retryable; every other outcome is
// terminal. Result.Retryable must agree with this taxonomy — the
// strict validator enforces it so the advisory can never contradict the code.
func (o Outcome) Retryable() bool {
	return o == OutcomeRetryableSerializationFailure
}

// UnitOfWork is the request-side descriptor a caller hands to a transaction
// runner: how the unit of work should be run. It carries no result and no
// connection. Propagation is required; Isolation defaults to the runner's
// configured level when omitted.
type UnitOfWork struct {
	Schema          string `json:"$schema,omitempty"`
	ProtocolVersion int    `json:"protocolVersion"`
	// Name is an optional logical identifier for the unit of work, used for
	// diagnostics and tracing. When present it must match the canonical name
	// pattern.
	Name string `json:"name,omitempty"`
	// Propagation describes how the unit of work composes with an enclosing
	// transaction. Required.
	Propagation Propagation `json:"propagation"`
	// Isolation is the requested isolation level. Optional; when omitted the
	// runner uses its configured default.
	Isolation Isolation `json:"isolation,omitempty"`
	// ReadOnly, when true, declares the unit of work performs no writes, letting
	// the runner open a read-only transaction.
	ReadOnly bool `json:"readOnly,omitempty"`
}

// Result is the result-side envelope a transaction runner returns for
// a UnitOfWork. Outcome is the canonical, closed result code; Retryable is
// advisory metadata mirroring protocol/cache and protocol/events, telling the
// caller whether re-running the unit of work may succeed. Retryable must agree
// with Outcome.Retryable() (enforced by the strict validator).
type Result struct {
	Schema          string `json:"$schema,omitempty"`
	ProtocolVersion int    `json:"protocolVersion"`
	// Outcome is the typed result code. Required.
	Outcome Outcome `json:"outcome"`
	// Retryable is advisory metadata: true iff the caller may safely retry the
	// unit of work. It must equal Outcome.Retryable().
	Retryable bool `json:"retryable,omitempty"`
	// Message is an optional human-readable detail for logging/diagnostics. It is
	// never load-bearing — callers switch on Outcome, not Message.
	Message string `json:"message,omitempty"`
}
