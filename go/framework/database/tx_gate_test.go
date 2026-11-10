package database

import (
	"context"
	stderrors "errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/protocol/features/spectest"
)

// exclusiveTx is a mockTx whose Exec, Query and QueryRow fail the test when two
// of them use the connection at once, the way a pgx connection fails with
// "conn busy". A Query's rows keep the connection busy until they close.
type exclusiveTx struct {
	mockTx
	busy     atomic.Int32
	overlaps atomic.Int32
	calls    atomic.Int32
}

func newExclusiveTx(commit, rollback *atomic.Int32) *exclusiveTx {
	tx := &exclusiveTx{}
	tx.commitFn = func(context.Context) error { commit.Add(1); return nil }
	tx.rollbackFn = func(context.Context) error { rollback.Add(1); return nil }
	return tx
}

func (x *exclusiveTx) enter() {
	x.calls.Add(1)
	if x.busy.Add(1) > 1 {
		x.overlaps.Add(1)
	}
	time.Sleep(time.Millisecond)
}

func (x *exclusiveTx) leave() { x.busy.Add(-1) }

func (x *exclusiveTx) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	x.enter()
	defer x.leave()
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (x *exclusiveTx) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	x.enter()
	return &busyRows{mockRows: mockRows{items: []func(...any) error{func(...any) error { return nil }}}, leave: x.leave}, nil
}

func (x *exclusiveTx) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	x.enter()
	return &mockRow{scanFn: func(...any) error { x.leave(); return nil }}
}

// busyRows frees its connection once, on Close.
type busyRows struct {
	mockRows
	leave func()
	once  sync.Once
}

func (r *busyRows) Close() {
	r.mockRows.Close()
	r.once.Do(r.leave)
}

func (r *busyRows) Next() bool {
	if r.mockRows.Next() {
		return true
	}
	r.Close()
	return false
}

// TestTxGate_ConcurrentQueriesTakeTurns is the issue's reproduction on the
// request path: goroutines of one request send Exec, Query and QueryRow on the
// same pool at once. Every query succeeds, none overlaps another on the
// connection, the pool enrolls once, and the boundary commits.
func TestTxGate_ConcurrentQueriesTakeTurns(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "concurrent-request-queries", "concurrent-queries-take-turns-on-the-transaction")
	var commit, rollback, begins atomic.Int32
	tx := newExclusiveTx(&commit, &rollback)
	pool := &Pool{beginTx: func(context.Context) (pgx.Tx, error) { begins.Add(1); return tx, nil }}

	cc, scope := scopedUoW(t)
	defer cc.Close()
	ctx := scope.Context(context.Background())

	const goroutines = 24
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 3 {
			case 0:
				_, err := pool.Exec(ctx, "insert into t values (1)")
				errs <- err
			case 1:
				errs <- pool.QueryRow(ctx, "select 1").Scan()
			default:
				rows, err := pool.Query(ctx, "select 1")
				if err != nil {
					errs <- err
					return
				}
				for rows.Next() {
					if err := rows.Scan(); err != nil {
						errs <- err
						return
					}
				}
				errs <- rows.Err()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent query: %v", err)
		}
	}
	if n := tx.overlaps.Load(); n != 0 {
		t.Fatalf("%d queries used the transaction's connection at once", n)
	}
	if n := tx.calls.Load(); n != goroutines {
		t.Fatalf("connection served %d queries, want %d", n, goroutines)
	}
	if n := begins.Load(); n != 1 {
		t.Fatalf("pool began %d transactions, want 1", n)
	}

	if err := scope.Finalize(ctx, nil); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if commit.Load() != 1 || rollback.Load() != 0 {
		t.Fatalf("commit=%d rollback=%d, want 1/0", commit.Load(), rollback.Load())
	}
}

// TestTxGate_ResultHoldsTheTransactionUntilClosed proves an open Rows keeps the
// transaction: a concurrent Exec waits until the rows close, then runs; and
// rows that Next has drained free it without an explicit Close.
func TestTxGate_ResultHoldsTheTransactionUntilClosed(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "concurrent-request-queries", "a-result-holds-the-transaction-until-closed")
	var commit, rollback atomic.Int32
	tx := newGatedTx(newExclusiveTx(&commit, &rollback), "orders")
	ctx := context.Background()

	rows, err := tx.Query(ctx, "select 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		if _, err := tx.Exec(ctx, "update t set v = 1"); err != nil {
			t.Errorf("exec after rows closed: %v", err)
		}
	}()
	select {
	case <-ran:
		t.Fatal("Exec ran while the rows still held the transaction")
	case <-time.After(30 * time.Millisecond):
	}
	rows.Close()
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("Exec did not run after the rows closed")
	}

	drained, err := tx.Query(ctx, "select 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for drained.Next() {
		_ = drained.Scan()
	}
	if _, err := tx.Exec(ctx, "update t set v = 2"); err != nil {
		t.Fatalf("exec after drained rows: %v", err)
	}
}

// TestTxGate_WaitingQueryEndsWithItsContext proves a query waiting for its turn
// fails under its own context with a db.transaction error that still matches
// the context's error.
func TestTxGate_WaitingQueryEndsWithItsContext(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "concurrent-request-queries", "a-waiting-query-ends-with-its-context")
	var commit, rollback atomic.Int32
	tx := newGatedTx(newExclusiveTx(&commit, &rollback), "orders")

	rows, err := tx.Query(context.Background(), "select 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = tx.Exec(ctx, "update t set v = 1")
	if !perrors.Is(err, CodeTransaction) || !stderrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Exec error = %v, want a db.transaction error wrapping the deadline", err)
	}
	row := tx.QueryRow(ctx, "select 1")
	if err := row.Scan(); !perrors.Is(err, CodeTransaction) {
		t.Fatalf("QueryRow Scan error = %v, want db.transaction", err)
	}
	if err := row.Scan(); !perrors.Is(err, CodeTransaction) {
		t.Fatalf("second Scan error = %v, want the first Scan's error again", err)
	}
	failed, err := tx.Query(ctx, "select 1")
	if !perrors.Is(err, CodeTransaction) || failed == nil || !perrors.Is(failed.Err(), CodeTransaction) || failed.Next() {
		t.Fatalf("Query = (%v, %v), want closed rows carrying the db.transaction error", failed, err)
	}
	if _, err := tx.SendBatch(ctx, &pgx.Batch{}).Exec(); !perrors.Is(err, CodeTransaction) {
		t.Fatalf("SendBatch Exec error = %v, want db.transaction", err)
	}
}

// TestTxGate_BoundaryClosesAResultLeftOpen proves the boundary does not wait on
// rows the request never closed: it closes them, then commits.
func TestTxGate_BoundaryClosesAResultLeftOpen(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "concurrent-request-queries", "the-boundary-closes-a-result-left-open")
	var commit, rollback atomic.Int32
	inner := newExclusiveTx(&commit, &rollback)
	pool := &Pool{beginTx: func(context.Context) (pgx.Tx, error) { return inner, nil }}

	cc, scope := scopedUoW(t)
	defer cc.Close()
	ctx := scope.Context(context.Background())

	rows, err := pool.Query(ctx, "select 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- scope.Finalize(ctx, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the boundary waited on rows the request left open")
	}
	if commit.Load() != 1 {
		t.Fatalf("commit=%d, want 1", commit.Load())
	}
	if inner.busy.Load() != 0 {
		t.Fatal("the rows the request left open were not closed")
	}
	rows.Close()
}

// TestTxGate_BoundaryRefusesLaterQueries proves a query after the boundary, or
// one whose result arrives while the boundary runs, fails with a
// db.transaction error instead of reaching a finished transaction.
func TestTxGate_BoundaryRefusesLaterQueries(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "concurrent-request-queries", "the-boundary-refuses-later-queries")
	t.Run("after the boundary", func(t *testing.T) {
		var commit, rollback int
		pool := &Pool{beginTx: func(context.Context) (pgx.Tx, error) { return countingTx(&commit, &rollback), nil }}
		uow := newUnitOfWork(0)
		ctx := context.Background()
		tx, err := uow.enroll(ctx, pool)
		if err != nil {
			t.Fatalf("enroll: %v", err)
		}
		if err := uow.FinalizeScope(ctx, nil); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if _, err := tx.Exec(ctx, "insert into t values (1)"); !perrors.Is(err, CodeTransaction) {
			t.Fatalf("Exec after the boundary = %v, want db.transaction", err)
		}
		if err := tx.QueryRow(ctx, "select 1").Scan(); !perrors.Is(err, CodeTransaction) {
			t.Fatalf("QueryRow after the boundary = %v, want db.transaction", err)
		}
		if commit != 1 {
			t.Fatalf("commit=%d, want 1", commit)
		}
	})

	t.Run("rows that arrive while the boundary runs", func(t *testing.T) {
		var committed atomic.Bool
		sent := make(chan struct{})
		proceed := make(chan struct{})
		rows := &mockRows{}
		inner := &mockTx{
			mockQuerier: mockQuerier{queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
				close(sent)
				<-proceed
				return rows, nil
			}},
			commitFn: func(context.Context) error { committed.Store(true); return nil },
		}
		tx := newGatedTx(inner, "orders")

		result := make(chan error, 1)
		go func() {
			_, err := tx.Query(context.Background(), "select 1")
			result <- err
		}()
		<-sent
		finished := make(chan error, 1)
		go func() { finished <- tx.Commit(context.Background()) }()
		waitFor(t, func() bool {
			tx.gate.mu.Lock()
			defer tx.gate.mu.Unlock()
			return tx.gate.closed
		})
		close(proceed)

		if err := <-result; !perrors.Is(err, CodeTransaction) {
			t.Fatalf("Query during the boundary = %v, want db.transaction", err)
		}
		if !rows.closed {
			t.Fatal("rows that arrived during the boundary were not closed")
		}
		if err := <-finished; err != nil {
			t.Fatalf("commit: %v", err)
		}
		if !committed.Load() {
			t.Fatal("the boundary did not commit")
		}
	})
}

// TestTxGate_BoundaryRollsBackWhenItCannotWait proves a boundary bounded by
// UnitOfWorkTimeout does not commit around an operation that outlived the
// request: the commit fails and the rollback still runs, releasing the
// connection.
func TestTxGate_BoundaryRollsBackWhenItCannotWait(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "concurrent-request-queries", "the-boundary-rolls-back-when-it-cannot-wait")
	var commits, rollbacks atomic.Int32
	running := make(chan struct{})
	unblock := make(chan struct{})
	inner := &mockTx{
		mockQuerier: mockQuerier{execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			close(running)
			<-unblock
			return pgconn.CommandTag{}, nil
		}},
		commitFn:   func(context.Context) error { commits.Add(1); return nil },
		rollbackFn: func(context.Context) error { rollbacks.Add(1); return nil },
	}
	pool := &Pool{beginTx: func(context.Context) (pgx.Tx, error) { return inner, nil }}
	uow := newUnitOfWork(20 * time.Millisecond)
	ctx := context.Background()
	tx, err := uow.enroll(ctx, pool)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	execDone := make(chan struct{})
	go func() {
		defer close(execDone)
		_, _ = tx.Exec(ctx, "update t set v = 1")
	}()
	<-running

	err = uow.FinalizeScope(ctx, nil)
	close(unblock)
	<-execDone
	if !perrors.Is(err, CodeTransaction) || !stderrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("finalize = %v, want a db.transaction error wrapping the deadline", err)
	}
	if commits.Load() != 0 || rollbacks.Load() != 1 {
		t.Fatalf("commit=%d rollback=%d, want 0/1", commits.Load(), rollbacks.Load())
	}
}

// TestTxGate_SavepointsShareTheGate proves a savepoint transaction from Begin
// takes turns with its parent and ends as an ordinary operation, leaving the
// parent open.
func TestTxGate_SavepointsShareTheGate(t *testing.T) {
	var commits, rollbacks atomic.Int32
	tx := newGatedTx(newExclusiveTx(&commits, &rollbacks), "orders")
	ctx := context.Background()
	nested, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := nested.Exec(ctx, "insert into t values (1)"); err != nil {
		t.Fatalf("nested exec: %v", err)
	}
	if err := nested.Rollback(ctx); err != nil {
		t.Fatalf("nested rollback: %v", err)
	}
	if err := nested.Commit(ctx); err != nil {
		t.Fatalf("nested commit: %v", err)
	}
	if _, err := tx.Exec(ctx, "insert into t values (2)"); err != nil {
		t.Fatalf("exec after the savepoint ended: %v", err)
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"t"}, []string{"v"}, pgx.CopyFromRows(nil)); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if _, err := tx.Prepare(ctx, "s", "select 1"); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if commits.Load() != 2 || rollbacks.Load() != 1 {
		t.Fatalf("commit=%d rollback=%d, want 2/1 (savepoint commit, root commit, savepoint rollback)", commits.Load(), rollbacks.Load())
	}
	if tx.Conn() != nil || (tx.LargeObjects() != pgx.LargeObjects{}) {
		t.Fatal("Conn and LargeObjects must reach the inner transaction")
	}
}

// TestAutocommit_SkipsTheUnitOfWork proves a query on an Autocommit context
// never enrolls the request's pool: it reaches the pool view (which, for this
// unopened pool, reports "pool is not open") and begins no transaction.
func TestAutocommit_SkipsTheUnitOfWork(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "autocommit-context", "autocommit-query-skips-the-unit-of-work")
	var begins int
	pool := &Pool{beginTx: func(context.Context) (pgx.Tx, error) { begins++; return &mockTx{}, nil }}
	cc, scope := scopedUoW(t)
	defer cc.Close()
	ctx := scope.Context(context.Background())
	auto := Autocommit(ctx)

	if !InUnitOfWork(ctx) || InUnitOfWork(auto) {
		t.Fatalf("InUnitOfWork(request)=%v InUnitOfWork(autocommit)=%v, want true/false", InUnitOfWork(ctx), InUnitOfWork(auto))
	}
	if _, err := pool.Exec(auto, "select 1"); !perrors.Is(err, CodeConnection) {
		t.Fatalf("Exec on Autocommit = %v, want the pool view's db.connection error", err)
	}
	if begins != 0 {
		t.Fatalf("an Autocommit query began %d transactions, want 0", begins)
	}
	if err := scope.Finalize(ctx, nil); err != nil {
		t.Fatalf("finalize: %v", err)
	}
}

// TestAutocommit_SkipsAWithTxTransaction proves an Autocommit context drops
// every WithTx transaction it carries, while the parent context keeps it.
func TestAutocommit_SkipsAWithTxTransaction(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "autocommit-context", "autocommit-query-skips-a-withtx-transaction")
	pool := &Pool{}
	ctx := ctxWithQuerier(pool, &mockQuerier{})
	if TxFromContext(ctx, pool) == nil {
		t.Fatal("the parent context lost its transaction")
	}
	auto := Autocommit(ctx)
	if TxFromContext(auto, pool) != nil {
		t.Fatal("an Autocommit context still carries the WithTx transaction")
	}
	if _, isTx := pool.Querier(auto).(pgx.Tx); isTx {
		t.Fatal("Querier on an Autocommit context returned a transaction")
	}
	if _, err := pool.querier(auto); !perrors.Is(err, CodeConnection) {
		t.Fatalf("querier on Autocommit = %v, want the pool view's db.connection error", err)
	}
}

// TestAutocommit_WithTxOwnsItsTransaction proves WithTx under an Autocommit
// context of a request begins, binds and commits its own transaction instead
// of joining the request's unit of work.
func TestAutocommit_WithTxOwnsItsTransaction(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "autocommit-context", "withtx-under-autocommit-owns-its-transaction")
	var commit, rollback, begins int
	owned := countingTx(&commit, &rollback)
	pool := &Pool{beginTx: func(context.Context) (pgx.Tx, error) { begins++; return owned, nil }}
	cc, scope := scopedUoW(t)
	defer cc.Close()
	ctx := scope.Context(context.Background())

	err := WithTx(Autocommit(ctx), pool, func(txCtx context.Context) error {
		if TxFromContext(txCtx, pool) != owned {
			t.Error("the callback does not carry the transaction WithTx owns")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if begins != 1 || commit != 1 || rollback != 0 {
		t.Fatalf("begins=%d commit=%d rollback=%d, want 1/1/0 before the boundary", begins, commit, rollback)
	}
	if err := scope.Finalize(ctx, nil); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if commit != 1 {
		t.Fatalf("the boundary finalized the owned transaction again: commit=%d", commit)
	}
}

// waitFor polls cond until it holds, failing the test after a bound.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(time.Millisecond)
	}
}
