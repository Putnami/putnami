package database

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
)

// UnitOfWork is a DI request-scoped transaction coordinator: one transaction per
// participating datasource pool, begun lazily the first time that pool is used
// within the scope, and committed or rolled back together at the request
// boundary. It is registered as a Scoped provider by the database plugin (see
// PluginConfig.UnitOfWork) and resolved from the active scope, so every
// repository query on a given pool within one request transparently shares that
// pool's transaction (see Pool.querier) — no repository changes and no explicit
// context threading required.
//
// # Multi-datasource semantics — best-effort, NOT two-phase commit
//
// A UnitOfWork spanning a SINGLE datasource is atomic: its one transaction
// commits or rolls back as a unit. Spanning MULTIPLE datasources it is
// best-effort sequential commit and explicitly NOT two-phase commit (2PC):
// Commit commits each datasource's transaction in enrollment order, and a commit
// failure on datasource N — after datasources 0..N-1 have already committed —
// leaves those earlier datasources committed (a documented partial commit),
// rolls back the datasources not yet committed to release their connections, and
// returns an error naming how many committed. It never pretends atomicity across
// datasources; callers that need cross-datasource atomicity must not span them
// in one unit of work.
//
// # Nesting
//
// Nesting is symmetric and the boundary that first owns a pool's transaction
// keeps commit/rollback ownership. A UnitOfWork inside an outer WithTx joins its
// explicit transaction without owning it. A WithTx inside an active UnitOfWork
// enrolls and joins the unit's transaction without finalizing it. Both directions
// use join-outer semantics with no savepoints.
//
// A UnitOfWork is safe for concurrent use by the goroutines serving one request.
type UnitOfWork struct {
	mu    sync.Mutex
	txs   map[*Pool]pgx.Tx // open transactions this unit owns, keyed by pool
	order []*Pool          // enrollment order, for deterministic sequential commit
	// rollbackOnly forces a rollback at the boundary even when the handler
	// reports success (see SetRollbackOnly).
	rollbackOnly bool
	// finished guards commit/rollback so the unit is finalized at most once; a
	// second FinalizeScope/Commit/Rollback is a no-op.
	finished bool
	// timeout bounds each commit/rollback so a wedged finalizer cannot pin a
	// connection forever; zero disables the bound.
	timeout time.Duration
	// startedAt is the instant the first transaction was begun (first enroll),
	// used as the transaction-telemetry duration origin. Zero until a pool is
	// enrolled, so a unit that never touched a repository emits nothing.
	startedAt time.Time
	// rollbackCause carries the classified, secret-free cause of the boundary
	// outcome into Rollback's telemetry; set by FinalizeScope from the request
	// outcome before it delegates to Rollback. Empty falls back to
	// "explicit-rollback".
	rollbackCause string
}

// newUnitOfWork builds an empty unit of work. timeout bounds each
// commit/rollback operation (zero disables the bound).
func newUnitOfWork(timeout time.Duration) *UnitOfWork {
	return &UnitOfWork{txs: make(map[*Pool]pgx.Tx), timeout: timeout}
}

// InUnitOfWork reports whether ctx has an active DI scope with a UnitOfWork
// provider. It does not materialize the coordinator, enroll a pool, begin a
// transaction, or acquire a connection.
func InUnitOfWork(ctx context.Context) bool {
	scope := inject.ScopeFrom(ctx)
	return scope != nil && scope.Has(inject.TokenOf[*UnitOfWork]())
}

// unitOfWorkFromContext returns the request-scoped UnitOfWork bound in ctx's DI
// scope, or nil when no scope is active or none is registered. Resolving it
// through the scope materializes exactly one UnitOfWork per request (cached in
// the scope container), shared by every repository query in that request. It
// never begins a transaction — that happens lazily on enroll — so a request that
// never touches a repository resolves the unit but opens no connection.
func unitOfWorkFromContext(ctx context.Context) *UnitOfWork {
	scope := inject.ScopeFrom(ctx)
	if scope == nil {
		return nil
	}
	token := inject.TokenOf[*UnitOfWork]()
	if !scope.Has(token) {
		return nil
	}
	value, err := scope.Get(token)
	if err != nil {
		return nil
	}
	uow, ok := value.(*UnitOfWork)
	if !ok {
		return nil
	}
	return uow
}

// enroll returns the transaction this unit of work holds on pool, beginning one
// on first use and reusing it thereafter (idempotent per pool). An explicit
// WithTx transaction already open on pool in ctx wins: the unit joins it without
// beginning or owning a new transaction, so a unit of work nested inside an outer
// WithTx for the same pool shares the enclosing transaction (no savepoints).
func (u *UnitOfWork) enroll(ctx context.Context, pool *Pool) (pgx.Tx, error) {
	// Defer to an explicit transaction already open on this pool: the outer
	// WithTx owns its commit/rollback, so the unit must not begin a second
	// transaction on the same pool nor track it for commit at the boundary.
	if tx := TxFromContext(ctx, pool); tx != nil {
		return tx, nil
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	if u.finished {
		return nil, errors.Newf(CodeTransaction, "unit of work already finalized; cannot enroll a new datasource")
	}
	if tx, ok := u.txs[pool]; ok {
		return tx, nil
	}

	tx, err := pool.beginTransaction(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, CodeTransaction, "unit of work: begin tx")
	}
	if u.startedAt.IsZero() {
		u.startedAt = time.Now()
	}
	u.txs[pool] = tx
	u.order = append(u.order, pool)
	return tx, nil
}

// SetRollbackOnly marks the unit of work so the request boundary rolls back
// every enrolled transaction even when the handler reports success. It is the
// escape hatch for a handler that detects a business failure but still returns a
// non-error response. It is safe to call from any goroutine serving the request.
func (u *UnitOfWork) SetRollbackOnly() {
	u.mu.Lock()
	u.rollbackOnly = true
	u.mu.Unlock()
}

// FinalizeScope reconciles the unit of work with the request-scope outcome:
// commit when outcome is nil (and the unit was not marked rollback-only), roll
// back otherwise. It always releases every connection an enrolled transaction
// holds and is idempotent. It implements inject.ScopeFinalizer, so the
// request-scope boundary (go.putnami.dev/http) drives it automatically without
// the database package depending on http.
func (u *UnitOfWork) FinalizeScope(ctx context.Context, outcome error) error {
	if outcome != nil {
		// Classify the request outcome into a secret-free rollback cause for the
		// boundary telemetry before delegating to Rollback (which does not see the
		// outcome error itself).
		u.mu.Lock()
		u.rollbackCause = txRollbackCause(outcome)
		u.mu.Unlock()
		return u.Rollback(ctx)
	}
	return u.Commit(ctx)
}

// Commit commits every enrolled transaction. A single-datasource unit is atomic.
// A multi-datasource unit is best-effort sequential (NOT 2PC): transactions
// commit in enrollment order, and if commit N fails the datasources committed
// before it stay committed (a partial commit), the datasources not yet committed
// are rolled back to release their connections, and the returned error names how
// many committed before the failure. A unit marked rollback-only rolls back
// instead. Commit is idempotent — a second call (or a call after Rollback) is a
// no-op.
func (u *UnitOfWork) Commit(ctx context.Context) error {
	// Emit boundary telemetry AFTER releasing u.mu so a user TxObserver never
	// runs under the lock. Registered before the Unlock defer so it runs last
	// (LIFO). emissions is read at call time, capturing whatever the finalize
	// path below records.
	var emissions []txEmission
	defer func() { emitTx(ctx, emissions) }()

	u.mu.Lock()
	defer u.mu.Unlock()
	if u.finished {
		return nil
	}
	u.finished = true

	opCtx, cancel := u.opContext(ctx)
	defer cancel()

	if u.rollbackOnly {
		err := u.rollbackLocked(opCtx, u.order)
		emissions = u.buildEmissions(u.order, TxOutcomeRolledBack, "rollback-only")
		return err
	}

	for i, pool := range u.order {
		if err := u.txs[pool].Commit(opCtx); err != nil {
			// Best-effort sequential commit, NOT 2PC: datasources 0..i-1 have
			// already committed and stay committed (a documented partial commit).
			// Release every connection not yet committed — the one whose commit
			// just failed (pgx leaves it needing a rollback) and any after it — by
			// rolling them back, then surface the partial commit. Never report
			// success across a partial boundary.
			rbErr := u.rollbackLocked(opCtx, u.order[i:])
			// Telemetry mirrors the partial boundary: the committed prefix reports
			// committed, the rolled-back remainder reports rolled-back with the
			// commit error's classified (secret-free) cause.
			emissions = append(
				u.buildEmissions(u.order[:i], TxOutcomeCommitted, ""),
				u.buildEmissions(u.order[i:], TxOutcomeRolledBack, txRollbackCause(err))...,
			)
			return errors.Wrapf(err, CodeTransaction,
				"unit of work partial commit: not two-phase commit",
				errors.Any("committed", i),
				errors.Any("datasources", len(u.order)),
				errors.Any("rollbackRemaining", rbErr),
			)
		}
	}
	emissions = u.buildEmissions(u.order, TxOutcomeCommitted, "")
	return nil
}

// Rollback rolls back every enrolled transaction, releasing all connections, and
// aggregates any rollback errors. It is idempotent — a second call (or a call
// after Commit) is a no-op.
func (u *UnitOfWork) Rollback(ctx context.Context) error {
	// See Commit: emit after unlock so the TxObserver never runs under u.mu.
	var emissions []txEmission
	defer func() { emitTx(ctx, emissions) }()

	u.mu.Lock()
	defer u.mu.Unlock()
	if u.finished {
		return nil
	}
	u.finished = true

	opCtx, cancel := u.opContext(ctx)
	defer cancel()

	cause := u.rollbackCause
	if cause == "" {
		cause = "explicit-rollback"
	}
	err := u.rollbackLocked(opCtx, u.order)
	emissions = u.buildEmissions(u.order, TxOutcomeRolledBack, cause)
	return err
}

// txEmission pairs a pool with the observation to emit on it, so a unit of work
// can build every boundary observation while holding u.mu and flush them all
// after releasing it.
type txEmission struct {
	pool *Pool
	obs  TxObservation
}

// emitTx forwards each pending observation to its pool's observeTx (logger +
// optional TxObserver). Called after u.mu is released so the user hook never
// runs under the unit-of-work lock.
func emitTx(ctx context.Context, emissions []txEmission) {
	for _, e := range emissions {
		e.pool.observeTx(ctx, e.obs)
	}
}

// buildEmissions builds one TxObservation per pool with the shared unit-of-work
// duration and the given outcome/cause. The caller holds u.mu; it reads only
// u.startedAt and each pool's config, so it never blocks. Returns nil for an
// empty pool set, so a unit that enrolled no datasource emits nothing.
func (u *UnitOfWork) buildEmissions(pools []*Pool, outcome TxOutcome, cause string) []txEmission {
	if len(pools) == 0 {
		return nil
	}
	var dur time.Duration
	if !u.startedAt.IsZero() {
		dur = time.Since(u.startedAt)
	}
	emissions := make([]txEmission, 0, len(pools))
	for _, pool := range pools {
		emissions = append(emissions, txEmission{
			pool: pool,
			obs: TxObservation{
				Datasource:    pool.cfg.databaseName(),
				Outcome:       outcome,
				Duration:      dur,
				RollbackCause: cause,
			},
		})
	}
	return emissions
}

// rollbackLocked rolls back each pool's transaction in pools, releasing its
// connection, and returns an aggregated error. The caller holds u.mu. It uses
// the provided ctx, which callers derive with opContext (context.WithoutCancel)
// so a rollback still runs after the request context is canceled or timed out.
func (u *UnitOfWork) rollbackLocked(ctx context.Context, pools []*Pool) error {
	var errs []error
	for _, pool := range pools {
		if err := u.txs[pool].Rollback(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.NewAggregate("unit of work rollback", errs)
}

// opContext derives the context each commit/rollback runs under. It strips
// cancellation (context.WithoutCancel) so a boundary rollback still completes
// after the request context is canceled or timed out — the whole point of the
// boundary rollback — and a success commit is not aborted by a late client
// disconnect after the handler already returned. When a timeout is configured it
// bounds the operation so a wedged commit/rollback cannot pin the connection
// forever.
func (u *UnitOfWork) opContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(ctx)
	if u.timeout > 0 {
		return context.WithTimeout(base, u.timeout)
	}
	return base, func() {}
}
