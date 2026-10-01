package database

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"go.putnami.dev/logger"
)

// attrByKey returns the slog.Attr with the given key from a log entry, and
// whether it was found.
func attrByKey(entry *logger.LogEntry, key string) (slog.Attr, bool) {
	for _, a := range entry.Attrs {
		if a.Key == key {
			return a, true
		}
	}
	return slog.Attr{}, false
}

// debugPoolWithSink builds a bare Pool (nil inner *pgxpool.Pool) wired to a
// memory log sink at Debug level, so the query-duration events observe emits
// are assertable without a live database. cfg supplies the observability
// fields under test (QueryObserver, SlowQueryThreshold, Datasource).
func debugPoolWithSink(cfg PoolConfig) (*Pool, *logger.MemorySink) {
	sink := logger.NewMemorySink()
	return &Pool{
		log: logger.New("database", logger.LevelDebug, sink),
		cfg: cfg,
	}, sink
}

// TestPool_QueryObserver_InvokedPerOperation proves the optional QueryObserver
// hook fires once per Pool.Exec/Query/QueryRow with the right op label and the
// operation's error, and that duration is non-negative.
func TestPool_QueryObserver_InvokedPerOperation(t *testing.T) {
	type observation struct {
		op  QueryOp
		dur time.Duration
		err error
	}
	var got []observation

	wantErr := fmt.Errorf("boom")
	cfg := PoolConfig{
		QueryObserver: func(_ context.Context, op QueryOp, dur time.Duration, err error) {
			got = append(got, observation{op, dur, err})
		},
	}
	pool, _ := debugPoolWithSink(cfg)

	q := &mockQuerier{
		execFn: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("DELETE 1"), wantErr
		},
		queryFn: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
			return &mockRows{}, nil
		},
		queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return &mockRow{}
		},
	}
	ctx := ctxWithQuerier(pool, q)

	_, _ = pool.Exec(ctx, "DELETE FROM users")
	rows, _ := pool.Query(ctx, "SELECT 1")
	rows.Close()
	_ = pool.QueryRow(ctx, "SELECT 1").Scan()

	if len(got) != 3 {
		t.Fatalf("observer called %d times, want 3", len(got))
	}
	if got[0].op != QueryOpExec || got[0].err != wantErr {
		t.Errorf("exec observation = %+v, want op=exec err=boom", got[0])
	}
	if got[1].op != QueryOpQuery || got[1].err != nil {
		t.Errorf("query observation = %+v, want op=query err=nil", got[1])
	}
	if got[2].op != QueryOpQueryRow || got[2].err != nil {
		t.Errorf("queryRow observation = %+v, want op=query_row err=nil", got[2])
	}
	for i, o := range got {
		if o.dur < 0 {
			t.Errorf("observation %d duration = %v, want >= 0", i, o.dur)
		}
	}
}

func TestPool_QueryRowObserver_RecordsScanError(t *testing.T) {
	wantErr := fmt.Errorf("no rows")
	var gotErr error
	pool, sink := debugPoolWithSink(PoolConfig{
		QueryObserver: func(_ context.Context, op QueryOp, _ time.Duration, err error) {
			if op == QueryOpQueryRow {
				gotErr = err
			}
		},
	})
	q := &mockQuerier{
		queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return &mockRow{scanFn: func(_ ...any) error { return wantErr }}
		},
	}
	ctx := ctxWithQuerier(pool, q)

	err := pool.QueryRow(ctx, "SELECT 1").Scan()
	if err != wantErr {
		t.Fatalf("Scan error = %v, want %v", err, wantErr)
	}
	if gotErr != wantErr {
		t.Fatalf("observer err = %v, want %v", gotErr, wantErr)
	}
	entry := sink.Last()
	if entry == nil || entry.Level != logger.LevelWarn || entry.Message != "query failed" {
		t.Fatalf("log entry = %+v, want Warn query failed", entry)
	}
	if info := errorInfoAttrOf(t, entry); info.Message != wantErr.Error() {
		t.Errorf("structured error message = %q, want %q", info.Message, wantErr.Error())
	}
	wantField(t, databaseGroupOf(t, entry), "outcome", outcomeFailure)
}

// TestPool_observe_LogsDebugOnSuccess proves a normal query emits a Debug
// "query executed" event carrying the op and a duration, and includes the
// datasource label when one is configured.
func TestPool_observe_LogsDebugOnSuccess(t *testing.T) {
	pool, sink := debugPoolWithSink(PoolConfig{Database: "orders"})
	q := &mockQuerier{}
	ctx := ctxWithQuerier(pool, q)

	_, _ = pool.Exec(ctx, "INSERT INTO t VALUES (1)")

	if sink.Len() != 1 {
		t.Fatalf("expected 1 log entry, got %d", sink.Len())
	}
	entry := sink.Last()
	if entry.Level != logger.LevelDebug {
		t.Errorf("level = %v, want Debug", entry.Level)
	}
	if entry.Message != "query executed" {
		t.Errorf("message = %q, want %q", entry.Message, "query executed")
	}
	// Domain fields travel as one closed, camelCase "database" group.
	group := databaseGroupOf(t, entry)
	wantField(t, group, "operation", string(QueryOpExec))
	wantField(t, group, "datasource", "orders")
	wantField(t, group, "outcome", outcomeSuccess)
	wantHasField(t, group, "durationMs")
	wantHasField(t, group, "durationUs")
}

// TestPool_observe_LogsWarnOnError proves a failed query is logged at Warn as
// "query failed" with the error attached, regardless of any slow-query
// threshold.
func TestPool_observe_LogsWarnOnError(t *testing.T) {
	pool, sink := debugPoolWithSink(PoolConfig{})
	q := &mockQuerier{
		execFn: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, fmt.Errorf("connection refused")
		},
	}
	ctx := ctxWithQuerier(pool, q)

	_, _ = pool.Exec(ctx, "DELETE FROM t")

	entry := sink.Last()
	if entry == nil || entry.Level != logger.LevelWarn {
		t.Fatalf("expected Warn entry, got %+v", entry)
	}
	if entry.Message != "query failed" {
		t.Errorf("message = %q, want %q", entry.Message, "query failed")
	}
	if info := errorInfoAttrOf(t, entry); info.Message != "connection refused" {
		t.Errorf("structured error message = %q, want connection refused", info.Message)
	}
	wantField(t, databaseGroupOf(t, entry), "outcome", outcomeFailure)
}

// TestPool_observe_NoRowsLogsDebug proves the pgx.ErrNoRows not-found sentinel
// is treated as a normal outcome — logged at Debug as "query executed" with no
// error attribute, NOT at Warn "query failed" — so "look it up, 404 if absent"
// lookups do not spam warnings on every miss. The raw ErrNoRows must still reach
// both the QueryObserver and the caller unchanged.
func TestPool_observe_NoRowsLogsDebug(t *testing.T) {
	var gotErr error
	pool, sink := debugPoolWithSink(PoolConfig{
		QueryObserver: func(_ context.Context, op QueryOp, _ time.Duration, err error) {
			if op == QueryOpQueryRow {
				gotErr = err
			}
		},
	})
	q := &mockQuerier{
		queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return &mockRow{scanFn: func(_ ...any) error { return pgx.ErrNoRows }}
		},
	}
	ctx := ctxWithQuerier(pool, q)

	err := pool.QueryRow(ctx, "SELECT 1").Scan()
	if !IsNoRows(err) {
		t.Fatalf("Scan error = %v, want pgx.ErrNoRows to flow to caller unchanged", err)
	}
	if !IsNoRows(gotErr) {
		t.Fatalf("observer err = %v, want pgx.ErrNoRows to flow to observer unchanged", gotErr)
	}

	entry := sink.Last()
	if entry == nil || entry.Level != logger.LevelDebug {
		t.Fatalf("log entry = %+v, want Debug (no-rows must not be a Warn)", entry)
	}
	if entry.Message != "query executed" {
		t.Errorf("message = %q, want %q", entry.Message, "query executed")
	}
	if hasErrorAttr(entry) {
		t.Error("no-rows event carries an error attr, want none")
	}
	// A not-found lookup is a successful query, not a failure.
	wantField(t, databaseGroupOf(t, entry), "outcome", outcomeSuccess)
}

// TestPool_observe_SlowQueryEscalatesToWarn proves a successful query whose
// duration crosses SlowQueryThreshold is logged at Warn as "slow query",
// while a fast query under the threshold stays at Debug.
func TestPool_observe_SlowQueryEscalatesToWarn(t *testing.T) {
	t.Run("over threshold escalates to Warn", func(t *testing.T) {
		pool, sink := debugPoolWithSink(PoolConfig{SlowQueryThreshold: time.Millisecond})
		q := &mockQuerier{
			queryFn: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
				time.Sleep(3 * time.Millisecond) // deterministically exceed 1ms
				return &mockRows{}, nil
			},
		}
		ctx := ctxWithQuerier(pool, q)

		rows, _ := pool.Query(ctx, "SELECT pg_sleep(1)")
		rows.Close()

		entry := sink.Last()
		if entry == nil || entry.Level != logger.LevelWarn {
			t.Fatalf("expected Warn entry, got %+v", entry)
		}
		if entry.Message != "slow query" {
			t.Errorf("message = %q, want %q", entry.Message, "slow query")
		}
		// The threshold travels as a field, never interpolated into the message,
		// and the query itself succeeded.
		group := databaseGroupOf(t, entry)
		wantField(t, group, "thresholdMs", int64(1))
		wantField(t, group, "outcome", outcomeSuccess)
	})

	t.Run("under threshold stays Debug", func(t *testing.T) {
		pool, sink := debugPoolWithSink(PoolConfig{SlowQueryThreshold: time.Hour})
		q := &mockQuerier{}
		ctx := ctxWithQuerier(pool, q)

		_, _ = pool.Exec(ctx, "SELECT 1")

		entry := sink.Last()
		if entry == nil || entry.Level != logger.LevelDebug {
			t.Fatalf("expected Debug entry, got %+v", entry)
		}
		if entry.Message != "query executed" {
			t.Errorf("message = %q, want %q", entry.Message, "query executed")
		}
	})

	t.Run("zero threshold disables escalation", func(t *testing.T) {
		pool, sink := debugPoolWithSink(PoolConfig{}) // SlowQueryThreshold == 0
		q := &mockQuerier{
			queryFn: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
				time.Sleep(2 * time.Millisecond)
				return &mockRows{}, nil
			},
		}
		ctx := ctxWithQuerier(pool, q)

		rows, _ := pool.Query(ctx, "SELECT 1")
		rows.Close()

		entry := sink.Last()
		if entry == nil || entry.Level != logger.LevelDebug {
			t.Fatalf("expected Debug entry (escalation disabled), got %+v", entry)
		}
	})
}

// TestPool_observe_NilLoggerNoPanic proves the instrumentation path is
// panic-free on a bare Pool with no logger, still invoking the user hook.
func TestPool_observe_NilLoggerNoPanic(t *testing.T) {
	called := false
	pool := &Pool{cfg: PoolConfig{
		QueryObserver: func(_ context.Context, _ QueryOp, _ time.Duration, _ error) {
			called = true
		},
	}}
	q := &mockQuerier{}
	ctx := ctxWithQuerier(pool, q)

	_, _ = pool.Exec(ctx, "SELECT 1") // must not panic on nil p.log

	if !called {
		t.Error("expected QueryObserver to be invoked even without a logger")
	}
}

// fakeStat is a synthetic poolStat used to drive the utilization derivation
// without a live pool (*pgxpool.Stat has unexported fields and no public
// constructor).
type fakeStat struct {
	acquired, idle, constructing, total, max int32
	emptyAcquire, canceledAcquire            int64
	emptyWait                                time.Duration
}

func (f fakeStat) AcquiredConns() int32                { return f.acquired }
func (f fakeStat) IdleConns() int32                    { return f.idle }
func (f fakeStat) ConstructingConns() int32            { return f.constructing }
func (f fakeStat) TotalConns() int32                   { return f.total }
func (f fakeStat) MaxConns() int32                     { return f.max }
func (f fakeStat) EmptyAcquireCount() int64            { return f.emptyAcquire }
func (f fakeStat) CanceledAcquireCount() int64         { return f.canceledAcquire }
func (f fakeStat) EmptyAcquireWaitTime() time.Duration { return f.emptyWait }

// TestPoolUtilizationFromStat exercises the gauge derivation that turns a raw
// pool Stat snapshot into the pool-saturation view, without a live pool.
func TestPoolUtilizationFromStat(t *testing.T) {
	t.Run("utilization is acquired over max and counters are carried", func(t *testing.T) {
		// 3 acquired of a 10-conn ceiling → 0.3 utilization.
		u := poolUtilizationFromStat(fakeStat{
			acquired: 3, idle: 5, constructing: 1, total: 9, max: 10,
			emptyAcquire: 7, canceledAcquire: 2, emptyWait: 250 * time.Millisecond,
		})
		if u.MaxConns != 10 {
			t.Errorf("MaxConns = %d, want 10", u.MaxConns)
		}
		if u.AcquiredConns != 3 {
			t.Errorf("AcquiredConns = %d, want 3", u.AcquiredConns)
		}
		if u.IdleConns != 5 {
			t.Errorf("IdleConns = %d, want 5", u.IdleConns)
		}
		if u.ConstructingConns != 1 || u.TotalConns != 9 {
			t.Errorf("ConstructingConns/TotalConns = %d/%d, want 1/9", u.ConstructingConns, u.TotalConns)
		}
		if u.EmptyAcquireCount != 7 || u.CanceledAcquireCount != 2 {
			t.Errorf("EmptyAcquireCount/CanceledAcquireCount = %d/%d, want 7/2", u.EmptyAcquireCount, u.CanceledAcquireCount)
		}
		if u.EmptyAcquireWaitTime != 250*time.Millisecond {
			t.Errorf("EmptyAcquireWaitTime = %v, want 250ms", u.EmptyAcquireWaitTime)
		}
		if u.Utilization < 0.299 || u.Utilization > 0.301 {
			t.Errorf("Utilization = %v, want ~0.3", u.Utilization)
		}
	})

	t.Run("zero MaxConns yields zero utilization, not NaN", func(t *testing.T) {
		u := poolUtilizationFromStat(fakeStat{})
		if u.Utilization != 0 {
			t.Errorf("Utilization = %v, want 0 for zero MaxConns", u.Utilization)
		}
	})

	t.Run("saturated pool reports utilization 1", func(t *testing.T) {
		u := poolUtilizationFromStat(fakeStat{acquired: 4, max: 4})
		if u.Utilization != 1 {
			t.Errorf("Utilization = %v, want 1 for a saturated pool", u.Utilization)
		}
	})
}

// TestPool_PoolUtilization_NilPool proves PoolUtilization is safe on a pool
// that was never opened.
func TestPool_PoolUtilization_NilPool(t *testing.T) {
	p := &Pool{}
	if got := p.PoolUtilization(); got != (PoolUtilization{}) {
		t.Errorf("PoolUtilization on nil pool = %+v, want zero value", got)
	}
}

// TestPool_LogPoolUtilization_NilPoolNoPanic proves the gauge emitter is a
// no-op (no panic) when the pool is not open.
func TestPool_LogPoolUtilization_NilPoolNoPanic(t *testing.T) {
	p := &Pool{}
	p.LogPoolUtilization(context.Background()) // must not panic
}
