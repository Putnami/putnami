package database

import (
	"context"
	"fmt"
	"strconv"

	"go.putnami.dev/errors"
	"go.putnami.dev/protocol/transaction"
)

// This file adds the optimistic-concurrency repository primitives: a conditional
// UPDATE that inspects affected rows (UpdateWhere), a value compare-and-set
// (CompareAndSet), a once-only claim (ConsumeOnce), and an atomic
// predecessor→successor rotation (Rotate). Each business helper returns a typed
// transaction.Outcome (see go.putnami.dev/protocol/transaction) rather than a
// bare row count, so callers switch on a stable, cross-language taxonomy —
// applied / already-consumed-conflict / not-found / retryable-serialization —
// and a genuine I/O failure still comes back as a raw error.
//
// # Atomicity contract
//
// Every helper routes its writes through r.pool (Pool.Exec/Query), so inside an
// active WithTx or request-scoped UnitOfWork they all join that one transaction
// (see Pool.querier). The consume-once safety property — at most one caller ever
// observes OutcomeApplied for a given row — is enforced by the UPDATE's WHERE
// predicate and Postgres row locking, and therefore holds even in autocommit:
// two racing claims serialize on the row, and only the first matches the
// still-unclaimed predicate. The follow-up existence check that distinguishes
// already-consumed from not-found is snapshot-consistent with the UPDATE only
// when both run in the same transaction; run these helpers inside WithTx /
// UnitOfWork when that distinction must be exact. Rotate performs two writes and
// is atomic ONLY inside a transaction — outside one, the revoke would commit
// even if the successor insert fails.

// UpdateWhere applies the SET assignments to every row matching the WHERE
// predicate and returns the number of rows affected. It mirrors DeleteWhere: set
// and where are raw SQL fragments sharing one positional-parameter space
// ($1, $2, …) across the whole statement, with the SET args listed before the
// WHERE args in args.
//
//	n, err := repo.UpdateWhere(ctx, "status = $1", "id = $2 AND status = $3",
//	    "consumed", id, "pending")
//
// It routes through the pool's Querier, so inside a WithTx/UnitOfWork it joins
// that transaction. Inspecting the affected-row count is the building block the
// CompareAndSet / ConsumeOnce helpers use to decide their typed outcome.
func (r *Repository[T]) UpdateWhere(ctx context.Context, set, where string, args ...any) (int64, error) {
	query := "UPDATE " + r.table + " SET " + set + " WHERE " + where
	tag, err := r.pool.Exec(ctx, query, args...)
	if err != nil {
		return 0, wrapQueryError(ctx, err, "update where", errors.String("table", r.table))
	}
	return tag.RowsAffected(), nil
}

// CompareAndSet atomically transitions column from expected to next for the row
// identified by keyColumn = key, reporting the typed outcome:
//
//   - OutcomeApplied when the row matched and column went expected → next.
//   - OutcomeAlreadyConsumedConflict when the row exists but column != expected
//     (a lost update / a concurrent transition already moved it).
//   - OutcomeNotFound when no row has keyColumn = key.
//   - OutcomeRetryableSerializationFailure when the UPDATE aborted with a
//     serialization_failure/deadlock (the caller may retry).
//
// This is value-equality CAS: expected is compared with `column = $expected`, so
// a nil expected does NOT match a SQL NULL (`col = NULL` is never true). To claim
// a row guarded by an IS NULL / IS NOT NULL condition, use ConsumeOnce with an
// explicit guard expression. keyColumn and column are developer-supplied
// identifiers; an invalid one panics, consistent with FindByID/DeleteByID.
func (r *Repository[T]) CompareAndSet(ctx context.Context, keyColumn string, key any, column string, expected, next any) (transaction.Outcome, error) {
	if !validIdentifier(keyColumn) {
		panic(fmt.Sprintf("database.CompareAndSet: invalid key column name: %q", keyColumn))
	}
	if !validIdentifier(column) {
		panic(fmt.Sprintf("database.CompareAndSet: invalid column name: %q", column))
	}
	qKey := quoteIdentifier(keyColumn)
	qCol := quoteIdentifier(column)
	// UPDATE <table> SET <col> = $1 WHERE <key> = $2 AND <col> = $3
	query := "UPDATE " + r.table + " SET " + qCol + " = $1 WHERE " + qKey + " = $2 AND " + qCol + " = $3"
	return r.resolveTransition(ctx, query, []any{next, key, expected}, qKey, key)
}

// ConsumeOnce atomically claims a once-only row identified by keyColumn = key. It
// applies the SET transition `set` only while the still-unclaimed condition
// `claimGuard` holds (e.g. "consumed = false" or "consumed_at IS NULL"); the key
// match is appended automatically. It reports:
//
//   - OutcomeApplied when the row was claimed by this call (the transition ran).
//   - OutcomeAlreadyConsumedConflict when the row exists but claimGuard no longer
//     holds (it was already consumed).
//   - OutcomeNotFound when no row has keyColumn = key.
//   - OutcomeRetryableSerializationFailure on a serialization/deadlock abort.
//
// Positional params in set and claimGuard share one space, numbered $1..$N in
// the order they appear in the statement (SET first, then claimGuard); args must
// list the SET args before the guard args. The key is bound after them as the
// final parameter — callers must not reference it in set or claimGuard.
//
//	// mark a device code consumed exactly once
//	repo.ConsumeOnce(ctx, "code", code, "consumed = false", "consumed = true")
//	repo.ConsumeOnce(ctx, "id", id, "consumed_at IS NULL",
//	    "consumed_at = now(), consumed_by = $1", userID)
func (r *Repository[T]) ConsumeOnce(ctx context.Context, keyColumn string, key any, claimGuard, set string, args ...any) (transaction.Outcome, error) {
	if !validIdentifier(keyColumn) {
		panic(fmt.Sprintf("database.ConsumeOnce: invalid key column name: %q", keyColumn))
	}
	qKey := quoteIdentifier(keyColumn)
	// The key is bound as the parameter after the caller's set/guard args, so its
	// placeholder index is len(args)+1. A fresh slice avoids mutating the caller's.
	keyParam := len(args) + 1
	query := "UPDATE " + r.table + " SET " + set +
		" WHERE (" + claimGuard + ") AND " + qKey + " = $" + strconv.Itoa(keyParam)
	allArgs := append(append([]any(nil), args...), key)
	return r.resolveTransition(ctx, query, allArgs, qKey, key)
}

// RotateSpec describes an atomic predecessor→successor rotation for Rotate: the
// predecessor row to revoke (a CompareAndSet on StateColumn) and the successor
// row to insert once the revoke applies.
type RotateSpec struct {
	// KeyColumn identifies the predecessor row (KeyColumn = PredecessorKey).
	KeyColumn      string
	PredecessorKey any
	// StateColumn is the predecessor column transitioned Expected → Revoked to
	// retire it. The revoke only applies while StateColumn currently equals
	// Expected, so a concurrent rotation that already revoked it is reported as a
	// conflict rather than double-applied.
	StateColumn string
	Expected    any
	Revoked     any
	// SuccessorColumns / SuccessorValues are the row inserted into the same table
	// once the predecessor is revoked. They must be equal length.
	SuccessorColumns []string
	SuccessorValues  []any
}

// Rotate atomically retires a predecessor and installs its successor as one unit
// of work. It first CompareAndSet-transitions the predecessor's StateColumn from
// Expected to Revoked; only when that applies does it INSERT the successor row.
// It returns:
//
//   - OutcomeApplied when the predecessor was revoked and the successor inserted.
//   - OutcomeAlreadyConsumedConflict / OutcomeNotFound when the predecessor could
//     not be revoked (already rotated, or absent) — the successor is NOT inserted.
//   - OutcomeAlreadyConsumedConflict when the successor insert hits a unique
//     violation (the successor already exists).
//   - OutcomeRetryableSerializationFailure on a serialization/deadlock abort.
//
// Rotate joins the ambient transaction rather than opening its own, so callers
// MUST wrap it in WithTx / UnitOfWork for the revoke and the insert to commit or
// roll back together; outside a transaction the revoke would autocommit even if
// the insert fails. Identifiers are developer-supplied; an invalid one panics.
func (r *Repository[T]) Rotate(ctx context.Context, spec RotateSpec) (transaction.Outcome, error) {
	if len(spec.SuccessorColumns) != len(spec.SuccessorValues) {
		panic(fmt.Sprintf("database.Rotate: successor columns/values length mismatch: %d != %d",
			len(spec.SuccessorColumns), len(spec.SuccessorValues)))
	}
	if len(spec.SuccessorColumns) == 0 {
		panic("database.Rotate: successor row has no columns")
	}

	// Revoke the predecessor. Any non-applied outcome (conflict, not-found,
	// retryable) short-circuits without touching the successor.
	outcome, err := r.CompareAndSet(ctx, spec.KeyColumn, spec.PredecessorKey, spec.StateColumn, spec.Expected, spec.Revoked)
	if err != nil {
		return "", err
	}
	if outcome != transaction.OutcomeApplied {
		return outcome, nil
	}

	// Insert the successor via the query builder; both writes share the ambient
	// transaction, so a failure here rolls the revoke back with it.
	query, args := Insert(r.rawTable).
		Columns(spec.SuccessorColumns...).
		Values(spec.SuccessorValues...).
		Build()
	if _, err := r.pool.Exec(ctx, query, args...); err != nil {
		// A unique violation on the successor is a conflict, not an I/O error.
		if classified, ok := ClassifyOutcome(err); ok {
			return classified, nil
		}
		return "", wrapQueryError(ctx, err, "rotate insert successor", errors.String("table", r.table))
	}
	return transaction.OutcomeApplied, nil
}

// resolveTransition runs a conditional-transition UPDATE (query, args) and maps
// the result to a typed transaction.Outcome. When the UPDATE transitions a row it
// is OutcomeApplied. When it transitions nothing it disambiguates
// already-consumed from not-found with an existence check on keyPredicate =
// "<qKey> = $1" bound to keyArg: an existing row is OutcomeAlreadyConsumedConflict,
// an absent row is OutcomeNotFound. Both statements route through r.pool, so
// inside a WithTx/UnitOfWork the disambiguation is atomic with the transition. A
// serialization_failure/deadlock (or a unique_violation) surfaced by either
// statement is classified into its typed outcome; any other error is returned raw
// (wrapped) so genuine I/O failures are never masked as a business outcome.
func (r *Repository[T]) resolveTransition(ctx context.Context, query string, args []any, qKey string, keyArg any) (transaction.Outcome, error) {
	tag, err := r.pool.Exec(ctx, query, args...)
	if err != nil {
		if outcome, ok := ClassifyOutcome(err); ok {
			return outcome, nil
		}
		return "", wrapQueryError(ctx, err, "conditional update", errors.String("table", r.table))
	}
	if tag.RowsAffected() > 0 {
		return transaction.OutcomeApplied, nil
	}

	exists, err := r.Exists(ctx, qKey+" = $1", keyArg)
	if err != nil {
		if outcome, ok := ClassifyOutcome(err); ok {
			return outcome, nil
		}
		return "", wrapQueryError(ctx, err, "conditional update existence check", errors.String("table", r.table))
	}
	if exists {
		return transaction.OutcomeAlreadyConsumedConflict, nil
	}
	return transaction.OutcomeNotFound, nil
}
