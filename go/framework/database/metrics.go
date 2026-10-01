package database

import (
	"context"
	"time"
)

// QueryOp names the database operation a QueryObserver is invoked for. It is
// the low-cardinality label that lets an operator group query-duration
// measurements by kind (e.g. separate read latency from write latency).
type QueryOp string

const (
	// QueryOpExec is a statement that returns no rows (INSERT/UPDATE/DELETE/DDL).
	QueryOpExec QueryOp = "exec"
	// QueryOpQuery is a multi-row read.
	QueryOpQuery QueryOp = "query"
	// QueryOpQueryRow is a single-row read. The duration measured for this op
	// covers the subsequent Scan because pgx defers the actual server round
	// trip and any error until that point.
	QueryOpQueryRow QueryOp = "query_row"
)

// QueryObserver is an optional hook invoked once per query issued through
// Pool.Exec, Pool.Query, and Pool.QueryRow with the operation kind, the wall
// duration of the call, and the resulting error (nil on success). It is the
// additive extension point for forwarding query-duration measurements to a
// metrics backend without wrapping every call site; the framework already
// emits the same measurement through its structured logger independently of
// this hook. The observer must be cheap and non-blocking — it runs on the
// query's hot path — and must not panic. ctx is the query's context (already
// canceled when the query failed on cancellation), so an exporter can read
// trace correlation from it.
type QueryObserver func(ctx context.Context, op QueryOp, dur time.Duration, err error)

// observe records one query measurement: it always emits a structured
// query-duration event through the pool's logger (Debug for a normal query,
// escalated to Warn when the duration crosses the configured
// SlowQueryThreshold so a slow query is a first-class operational signal), and
// then forwards the same measurement to the user-supplied QueryObserver when
// one is configured. A failed query is logged at Warn as "query failed", with
// one exception: pgx.ErrNoRows — the normal not-found sentinel a QueryRow
// caller tests for — is treated like a successful query and logged at Debug, so
// "look it up, 404 if absent" lookups do not spam warnings on every miss. It is
// the single instrumentation point wrapped around the Pool query path. A nil
// logger (a bare Pool built in tests) degrades to invoking only the user hook,
// so the path stays panic-free. Every record shape it emits is built in
// database_logging.go, so the field names, severities, and messages stay the
// contract's (protocols/logging/conformance).
func (p *Pool) observe(ctx context.Context, op QueryOp, start time.Time, err error) {
	dur := time.Since(start)

	if p.log != nil {
		datasource := p.cfg.databaseName()
		switch {
		case err != nil && !IsNoRows(err):
			logQueryFailed(ctx, p.log, op, datasource, dur, err)
		case err != nil:
			// A no-rows result (pgx.ErrNoRows, which QueryRow defers to Scan) is
			// the expected not-found sentinel of a "look it up, 404 if absent"
			// lookup, not a query failure — logging it at Warn spams a warning
			// per miss. Treat it like a successful query: emit at Debug with
			// outcome success and no error. The raw err is untouched here, so it
			// still flows to the QueryObserver and back to the caller unchanged.
			logQueryExecuted(ctx, p.log, op, datasource, dur)
		case p.cfg.SlowQueryThreshold > 0 && dur >= p.cfg.SlowQueryThreshold:
			logSlowQuery(ctx, p.log, op, datasource, dur, p.cfg.SlowQueryThreshold)
		default:
			logQueryExecuted(ctx, p.log, op, datasource, dur)
		}
	}

	if p.cfg.QueryObserver != nil {
		p.cfg.QueryObserver(ctx, op, dur, err)
	}
}

// ---------------------------------------------------------------------------
// Transaction-boundary telemetry
// ---------------------------------------------------------------------------
//
// The database package emits transaction-level observability through the same
// seam as per-query observability — a structured logger event plus an optional
// forwarding hook — so it needs no telemetry/metrics dependency. An exporter
// wired via PoolConfig.TxObserver turns each observation into the canonical,
// dotted metrics below; the framework itself always carries the same
// measurement on the structured logger regardless of whether a hook is set.

// Canonical transaction metric names an exporter emits from a TxObservation.
// They mirror the TypeScript adapter's sql.tx.* names byte-for-byte so both
// runtimes report identical metric series for the same transaction boundary.
const (
	// MetricTxDuration is the transaction duration histogram, in milliseconds.
	MetricTxDuration = "sql.tx.duration"
	// MetricTxRetries is the retry-count histogram. The current WithTx /
	// UnitOfWork helpers do not retry, so this is always 0; it exists so a
	// future retry loop can populate it without renaming the series.
	MetricTxRetries = "sql.tx.retries"
	// metricTxOutcomePrefix + <outcome> is a counter incremented once per
	// finalized transaction, e.g. sql.tx.outcome.committed.
	metricTxOutcomePrefix = "sql.tx.outcome."
	// metricTxRollbackPrefix + <cause> is a counter incremented once per rolled
	// back transaction, keyed by the SECRET-FREE classified cause code (a
	// SQLSTATE, a framework code, or a sentinel), e.g. sql.tx.rollback.40001.
	metricTxRollbackPrefix = "sql.tx.rollback."
)

// TxOutcome is the low-cardinality outcome label recorded at the transaction
// boundary. WithTx / UnitOfWork know only whether the transaction committed or
// rolled back; the richer transaction.Outcome taxonomy (applied /
// already-consumed-conflict / …) is a repository-helper result that is not
// plumbed down to the boundary in v1.
type TxOutcome string

// TxOutcome values.
const (
	// TxOutcomeCommitted marks a transaction that committed successfully.
	TxOutcomeCommitted TxOutcome = "committed"
	// TxOutcomeRolledBack marks a transaction that rolled back (on error,
	// rollback-only, panic, or a failed commit).
	TxOutcomeRolledBack TxOutcome = "rolled-back"
)

// TxMetric is one named metric sample derived from a TxObservation. Emitting
// metrics by name from the observation keeps the canonical naming in one place
// and makes the emitted series independently assertable.
type TxMetric struct {
	Name  string
	Value float64
}

// TxObservation is one transaction-boundary measurement handed to a TxObserver
// and emitted on the structured logger. Every field is secret-free by
// construction: Datasource is a configured name, Outcome is a fixed label,
// Retries is a count, and RollbackCause is a classified code (see
// txRollbackCause) — NEVER a bound parameter value, a row datum, or a raw error
// message.
type TxObservation struct {
	// Datasource is the logical datasource name of the pool the transaction ran
	// on, or "" when the pool declares none.
	Datasource string
	// Outcome is committed or rolled-back.
	Outcome TxOutcome
	// Duration is the wall time from BEGIN to the commit/rollback boundary.
	Duration time.Duration
	// Retries is the number of retries the runner performed (always 0 today).
	Retries int
	// RollbackCause is the classified, secret-free cause of a rollback (a
	// SQLSTATE, a framework error code, or a sentinel like "panic"/"timeout").
	// Empty on a committed transaction.
	RollbackCause string
}

// Metrics returns the canonical named metric samples this observation emits, so
// a TxObserver can forward them to a backend without re-deriving the names.
// Secret safety holds by construction: each name is built only from fixed
// labels and the already-classified RollbackCause, so a metric name can never
// carry a bound value or row datum.
func (o TxObservation) Metrics() []TxMetric {
	metrics := []TxMetric{
		{Name: MetricTxDuration, Value: float64(o.Duration.Milliseconds())},
		{Name: MetricTxRetries, Value: float64(o.Retries)},
		{Name: metricTxOutcomePrefix + string(o.Outcome), Value: 1},
	}
	if o.RollbackCause != "" {
		metrics = append(metrics, TxMetric{Name: metricTxRollbackPrefix + o.RollbackCause, Value: 1})
	}
	return metrics
}

// TxObserver is an optional hook invoked once per transaction finalized by
// WithTx or a request-scoped UnitOfWork with the boundary measurement. Like
// QueryObserver it is the additive extension point for forwarding transaction
// metrics to a backend; the framework always emits the same measurement on the
// structured logger regardless of whether this is set. The observer must be
// cheap, non-blocking, and must not panic.
type TxObserver func(ctx context.Context, obs TxObservation)

// observeTx records one transaction-boundary measurement: it emits a structured
// event through the pool's logger (Debug on commit, Warn on rollback) and then
// forwards the observation to the optional TxObserver. It is the tx-level twin
// of observe. Crucially — and UNLIKE observe, which attaches the structured
// query error — observeTx NEVER logs the rollback error: the record carries
// only the classified, secret-free RollbackCause, so a bound parameter value or
// a row datum embedded in a driver error can never reach a log line or a metric.
// A nil logger (a bare Pool built in tests) degrades to invoking only the hook,
// so the path stays panic-free. The record shape lives in database_logging.go,
// which also maps the exported committed/rolled-back Outcome onto the contract's
// success/failure outcome vocabulary.
func (p *Pool) observeTx(ctx context.Context, obs TxObservation) {
	if p.log != nil {
		logTransaction(ctx, p.log, obs)
	}

	if p.cfg.TxObserver != nil {
		p.cfg.TxObserver(ctx, obs)
	}
}

// PoolUtilization is a point-in-time, gauge-style view of pool saturation
// derived from the raw pgx Stat snapshot. It surfaces the second primary
// database health signal — pool saturation — that the raw Stats() passthrough
// leaves an operator to compute by hand. AcquiredConns near MaxConns (a
// Utilization near 1) is the leading indicator of pool exhaustion, the failure
// mode StatementTimeout guards against.
type PoolUtilization struct {
	// AcquiredConns is the number of connections currently checked out.
	AcquiredConns int32
	// IdleConns is the number of currently idle connections.
	IdleConns int32
	// ConstructingConns is the number of connections being established.
	ConstructingConns int32
	// TotalConns is AcquiredConns + IdleConns + ConstructingConns.
	TotalConns int32
	// MaxConns is the configured ceiling on pool size.
	MaxConns int32
	// Utilization is AcquiredConns/MaxConns in [0,1]; 0 when MaxConns is 0.
	// A sustained value near 1 means the pool is saturated and callers are at
	// risk of blocking on acquisition.
	Utilization float64
	// EmptyAcquireCount is the cumulative count of acquires that had to wait
	// because the pool was empty.
	EmptyAcquireCount int64
	// CanceledAcquireCount is the cumulative count of acquires canceled by a
	// context (caller gave up waiting for a connection).
	CanceledAcquireCount int64
	// EmptyAcquireWaitTime is the cumulative time callers spent waiting for a
	// connection because the pool was empty.
	EmptyAcquireWaitTime time.Duration
}

// poolStat is the subset of *pgxpool.Stat the utilization derivation reads.
// *pgxpool.Stat satisfies it; declaring the interface lets the derivation be
// unit-tested with a synthetic snapshot, since *pgxpool.Stat has unexported
// fields and no public constructor.
type poolStat interface {
	AcquiredConns() int32
	IdleConns() int32
	ConstructingConns() int32
	TotalConns() int32
	MaxConns() int32
	EmptyAcquireCount() int64
	CanceledAcquireCount() int64
	EmptyAcquireWaitTime() time.Duration
}

// poolUtilizationFromStat derives the gauge view from a pool Stat snapshot. It
// is split out so the derivation (the part worth asserting on) is testable
// without a live pool.
func poolUtilizationFromStat(s poolStat) PoolUtilization {
	u := PoolUtilization{
		AcquiredConns:        s.AcquiredConns(),
		IdleConns:            s.IdleConns(),
		ConstructingConns:    s.ConstructingConns(),
		TotalConns:           s.TotalConns(),
		MaxConns:             s.MaxConns(),
		EmptyAcquireCount:    s.EmptyAcquireCount(),
		CanceledAcquireCount: s.CanceledAcquireCount(),
		EmptyAcquireWaitTime: s.EmptyAcquireWaitTime(),
	}
	if u.MaxConns > 0 {
		u.Utilization = float64(u.AcquiredConns) / float64(u.MaxConns)
	}
	return u
}

// PoolUtilization returns the derived pool-saturation gauge for the SHARED
// physical pool this datasource acquires from — every datasource of the same
// database contributes to it. It is computed from the same snapshot Stats()
// exposes (which stays available for callers wanting the raw pgx struct) and
// adds the utilization ratio plus the contention counters operators actually
// alert on. Returns the zero value when the pool is not open.
func (p *Pool) PoolUtilization() PoolUtilization {
	stat := p.Stats()
	if stat == nil {
		return PoolUtilization{}
	}
	return poolUtilizationFromStat(stat)
}

// LogPoolUtilization emits the current pool-utilization gauge as a structured
// logger event, so pool saturation is observable on the existing logging
// pipeline without an operator polling Stats() and computing the ratio
// themselves. The gauge is the shared physical pool's, labeled with this
// datasource. It is a no-op when the pool is closed or has no logger. Callers
// that want a periodic gauge can invoke it on a ticker; the framework does not
// spawn a background goroutine for it (serverless-friendly: no hidden timer).
func (p *Pool) LogPoolUtilization(ctx context.Context) {
	if p.log == nil {
		return
	}
	stat := p.Stats()
	if stat == nil {
		return
	}
	u := poolUtilizationFromStat(stat)
	p.log.InfoCtx(ctx, msgPoolUtilization, databaseAttr(map[string]any{
		"datasource":           resolveDatasource(p.cfg.databaseName(), ""),
		"acquiredConns":        int64(u.AcquiredConns),
		"idleConns":            int64(u.IdleConns),
		"constructingConns":    int64(u.ConstructingConns),
		"totalConns":           int64(u.TotalConns),
		"maxConns":             int64(u.MaxConns),
		"utilization":          u.Utilization,
		"emptyAcquireCount":    u.EmptyAcquireCount,
		"canceledAcquireCount": u.CanceledAcquireCount,
		"emptyAcquireWaitMs":   u.EmptyAcquireWaitTime.Milliseconds(),
	}))
}
