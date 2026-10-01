package database

import (
	"context"
	"maps"
	"time"

	"github.com/jackc/pgx/v5"

	"go.putnami.dev/errors"
)

// txContextKey is the context key under which the per-pool transaction set
// lives. The value is always a txSet — never a bare pgx.Tx — so a repository can
// only ever join a transaction begun on its OWN pool.
type txContextKey struct{}

// txSet maps each pool to the transaction currently open on it within a WithTx
// scope. The pool itself is the transaction's identity: a transaction cannot
// outlive the pool that began it, and two distinct pools (even for the same
// datasource name) must never share a connection, so keying by *Pool guarantees
// a foreign pool never resolves another pool's tx.
//
// A txSet is treated as immutable once stored in a context: WithTx copies it on
// write (see contextWithTx) when binding an additional pool's transaction, so a
// parent context's set is never mutated. That keeps the outer WithTx's deferred
// rollback and any concurrent readers of the parent context observing a stable
// map without locking.
type txSet map[*Pool]pgx.Tx

// txSetFromContext returns the immutable per-pool transaction set carried by
// ctx, or nil when ctx carries none.
func txSetFromContext(ctx context.Context) txSet {
	set, ok := ctx.Value(txContextKey{}).(txSet)
	if !ok {
		return nil
	}
	return set
}

// contextWithTx returns a child context that binds tx to pool, layered over any
// transactions already bound in ctx for other pools. It copies the existing set
// on write so the parent context's set is never mutated — the deferred rollback
// of an outer WithTx and concurrent readers of the parent keep seeing their own
// immutable map.
func contextWithTx(ctx context.Context, pool *Pool, tx pgx.Tx) context.Context {
	prev := txSetFromContext(ctx)
	next := make(txSet, len(prev)+1)
	maps.Copy(next, prev)
	next[pool] = tx
	return context.WithValue(ctx, txContextKey{}, next)
}

// WithTx runs fn within a database transaction on pool. It begins and owns a
// transaction only when ctx has neither an explicit transaction on pool nor a
// request-scoped UnitOfWork. A transaction owned by WithTx is committed if fn
// returns nil and rolled back otherwise.
//
// Transaction reuse is bound to pool's identity:
//
//   - A nested WithTx on the SAME pool reuses the existing transaction (no
//     savepoints), exactly as before.
//
//   - Otherwise, when ctx carries a request-scoped UnitOfWork, WithTx enrolls
//     pool in that unit and binds its transaction to the callback context. The
//     request boundary owns commit/rollback; WithTx only returns fn's result.
//
//   - A transaction on a DIFFERENT pool is never reused. That pool is resolved
//     independently by the same rules: join the request UnitOfWork when present,
//     or open and own an independent transaction otherwise.
//
//     err := database.WithTx(ctx, pool, func(ctx context.Context) error {
//     // Queries through this pool use the same transaction; a repository on a
//     // different pool still uses that pool's own connection/transaction.
//     return repo.Create(ctx, entity)
//     })
//
// When WithTx owns the transaction it is panic-safe: if fn panics, a deferred
// finalizer rolls the transaction back — releasing the pooled connection —
// before the panic continues to unwind. When it joins a UnitOfWork, the request
// scope owns the corresponding panic rollback.
func WithTx(ctx context.Context, pool *Pool, fn func(ctx context.Context) error) error {
	// If already in a transaction on THIS pool, reuse it. A transaction on a
	// different pool is ignored, so a nested WithTx resolves this datasource
	// independently below (through the request unit or a new owned transaction).
	if TxFromContext(ctx, pool) != nil {
		return fn(ctx)
	}
	// A request UnitOfWork is the ambient owner. Enroll this pool once, then bind
	// that exact transaction into the callback context so TxFromContext,
	// Querier, and repository calls all observe the same uncommitted request
	// state. WithTx must not finalize a transaction it did not create.
	if uow := unitOfWorkFromContext(ctx); uow != nil {
		tx, err := uow.enroll(ctx, pool)
		if err != nil {
			return err
		}
		return fn(contextWithTx(ctx, pool, tx))
	}

	tx, err := pool.beginTransaction(ctx)
	if err != nil {
		return errors.Wrapf(err, CodeTransaction, "begin tx")
	}

	start := time.Now()
	txCtx := contextWithTx(ctx, pool, tx)

	// Emit exactly one transaction-boundary observation for the tx this call
	// owns — on the normal commit/rollback paths, on a failed commit, and on the
	// panic path — through the same logger+observer seam as per-query telemetry.
	// The `finalized` flag doubles as the deferred rollback safety net: it flips
	// once an explicit path has recorded its outcome and rolled back / committed,
	// so the deferred net neither double-rolls-back (the old committed/rolledBack
	// guard) nor double-records. context.WithoutCancel keeps a panic/late
	// rollback usable even when ctx is already canceled.
	finalized := false
	obs := TxObservation{Datasource: pool.cfg.databaseName()}
	defer func() {
		obs.Duration = time.Since(start)
		if !finalized {
			// fn panicked before a commit/rollback ran: roll back to release the
			// pooled connection, then record a rolled-back outcome whose cause is
			// the fixed sentinel "panic" (never the panic value, which could carry
			// secret data).
			_ = tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // best-effort connection release
			obs.Outcome = TxOutcomeRolledBack
			obs.RollbackCause = "panic"
		}
		pool.observeTx(ctx, obs)
	}()

	if err := fn(txCtx); err != nil {
		finalized = true
		obs.Outcome = TxOutcomeRolledBack
		obs.RollbackCause = txRollbackCause(err)
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			return errors.Wrapf(rbErr, CodeTransaction, "rollback failed", errors.Any("original", err))
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		finalized = true
		obs.Outcome = TxOutcomeRolledBack
		obs.RollbackCause = txRollbackCause(err)
		// pgx auto-aborts the transaction on a failed Commit; the previous
		// deferred safety net also rolled back here, so release best-effort for
		// parity and to keep the connection from lingering.
		_ = tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // best-effort connection release
		return errors.Wrapf(err, CodeTransaction, "commit")
	}

	finalized = true
	obs.Outcome = TxOutcomeCommitted

	return nil
}

// TxFromContext returns the transaction begun on pool within ctx, or nil when
// ctx carries no transaction for that pool. It is pool-scoped: a transaction
// begun on a different pool is never returned, so a repository only ever joins
// the transaction opened on its own datasource pool.
func TxFromContext(ctx context.Context, pool *Pool) pgx.Tx {
	if pool == nil {
		return nil
	}
	return txSetFromContext(ctx)[pool]
}
