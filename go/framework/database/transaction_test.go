package database

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.putnami.dev/protocol/features/spectest"
)

func TestTxFromContext_NoTransaction(t *testing.T) {
	ctx := context.Background()
	tx := TxFromContext(ctx, &Pool{})
	if tx != nil {
		t.Error("expected nil when no transaction in context")
	}
}

func TestTxFromContext_WrongType(t *testing.T) {
	ctx := context.WithValue(context.Background(), txContextKey{}, "not a tx set")
	tx := TxFromContext(ctx, &Pool{})
	if tx != nil {
		t.Error("expected nil when context value is wrong type")
	}
}

func TestTxFromContext_NilPool(t *testing.T) {
	// A nil pool has no identity, so it can never own a transaction.
	if tx := TxFromContext(context.Background(), nil); tx != nil {
		t.Error("expected nil tx for a nil pool")
	}
}

// TestWithTx_RollsBackOnPanic guards the connection-leak fix: a panic in
// the callback must roll the transaction back (releasing the pooled
// connection) before the panic continues to unwind, and commit must not run.
func TestWithTx_RollsBackOnPanic(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "transaction-finalization", "rolls-back-on-panic")
	rolledBack := false
	tx := &mockTx{
		rollbackFn: func(_ context.Context) error {
			rolledBack = true
			return nil
		},
		commitFn: func(_ context.Context) error {
			t.Error("commit must not be called when fn panics")
			return nil
		},
	}
	pool := &Pool{
		beginTx: func(_ context.Context) (pgx.Tx, error) { return tx, nil },
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic to propagate out of WithTx")
		}
		if !rolledBack {
			t.Error("expected rollback on panic (connection leak otherwise)")
		}
	}()

	_ = WithTx(context.Background(), pool, func(_ context.Context) error {
		panic("boom")
	})
}

// TestWithTx_NoDoubleRollbackOnError verifies the normal error path rolls
// back exactly once — the deferred safety net must not fire a second time.
func TestWithTx_NoDoubleRollbackOnError(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "transaction-finalization", "rolls-back-exactly-once-on-error")
	rollbacks := 0
	tx := &mockTx{
		rollbackFn: func(_ context.Context) error {
			rollbacks++
			return nil
		},
	}
	pool := &Pool{
		beginTx: func(_ context.Context) (pgx.Tx, error) { return tx, nil },
	}
	_ = WithTx(context.Background(), pool, func(_ context.Context) error {
		return context.Canceled
	})
	if rollbacks != 1 {
		t.Errorf("expected exactly one rollback on the error path, got %d", rollbacks)
	}
}

// TestPool_Querier_IgnoresForeignPoolTx pins the core multi-datasource
// invariant: a Querier resolved on pool B inside WithTx(ctx, poolA, …) must NOT
// join pool A's transaction — it falls back to pool B's own connection. Before
// the per-pool keying this leaked pool A's tx to every pool in scope.
func TestPool_Querier_IgnoresForeignPoolTx(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "pool-scoped-transaction", "foreign-pool-never-receives-the-transaction")
	txA := &mockTx{}
	poolA := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return txA, nil }}
	poolB := &Pool{} // a different datasource pool (distinct identity)

	err := WithTx(context.Background(), poolA, func(ctx context.Context) error {
		// Positive control: pool A joins its own tx.
		if got := poolA.Querier(ctx); got != Querier(txA) {
			t.Errorf("pool A should use its own tx, got %T", got)
		}
		// The fix: pool B must not see pool A's tx.
		if got := poolB.Querier(ctx); got == Querier(txA) {
			t.Error("pool B must not join pool A's transaction (cross-datasource leak)")
		}
		if TxFromContext(ctx, poolB) != nil {
			t.Error("expected no transaction bound to pool B")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
}

// TestWithTx_TwoPools_IndependentFinalizers proves that a nested WithTx on a
// different pool opens a distinct transaction whose commit/rollback is
// independent of the outer pool's transaction — the finalizers must not
// interfere in either direction.
func TestWithTx_TwoPools_IndependentFinalizers(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "pool-scoped-transaction", "two-pools-get-independent-finalizers")
	newCountingTx := func(commit, rollback *int) *mockTx {
		return &mockTx{
			commitFn:   func(_ context.Context) error { *commit++; return nil },
			rollbackFn: func(_ context.Context) error { *rollback++; return nil },
		}
	}

	t.Run("inner rolls back, outer commits", func(t *testing.T) {
		var aCommit, aRollback, bCommit, bRollback int
		txA := newCountingTx(&aCommit, &aRollback)
		txB := newCountingTx(&bCommit, &bRollback)
		poolA := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return txA, nil }}
		poolB := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return txB, nil }}

		innerErr := context.Canceled
		err := WithTx(context.Background(), poolA, func(ctx context.Context) error {
			got := WithTx(ctx, poolB, func(ctx context.Context) error {
				// Each pool sees only its own tx in the nested scope.
				if TxFromContext(ctx, poolA) != txA {
					t.Error("pool A tx should remain visible to pool A")
				}
				if TxFromContext(ctx, poolB) != txB {
					t.Error("pool B tx should be bound for pool B")
				}
				return innerErr
			})
			if got != innerErr {
				t.Errorf("inner WithTx should surface its error, got %v", got)
			}
			// Outer swallows the inner failure and commits its own work.
			return nil
		})
		if err != nil {
			t.Fatalf("outer WithTx: %v", err)
		}
		if aCommit != 1 || aRollback != 0 {
			t.Errorf("pool A: commit=%d rollback=%d, want commit=1 rollback=0", aCommit, aRollback)
		}
		if bCommit != 0 || bRollback != 1 {
			t.Errorf("pool B: commit=%d rollback=%d, want commit=0 rollback=1", bCommit, bRollback)
		}
	})

	t.Run("inner commits, outer rolls back", func(t *testing.T) {
		var aCommit, aRollback, bCommit, bRollback int
		txA := newCountingTx(&aCommit, &aRollback)
		txB := newCountingTx(&bCommit, &bRollback)
		poolA := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return txA, nil }}
		poolB := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return txB, nil }}

		outerErr := context.Canceled
		err := WithTx(context.Background(), poolA, func(ctx context.Context) error {
			if inner := WithTx(ctx, poolB, func(_ context.Context) error { return nil }); inner != nil {
				t.Errorf("inner WithTx should commit, got %v", inner)
			}
			// Outer fails after the inner committed independently.
			return outerErr
		})
		if err != outerErr {
			t.Fatalf("outer WithTx should surface its error, got %v", err)
		}
		if aCommit != 0 || aRollback != 1 {
			t.Errorf("pool A: commit=%d rollback=%d, want commit=0 rollback=1", aCommit, aRollback)
		}
		if bCommit != 1 || bRollback != 0 {
			t.Errorf("pool B: commit=%d rollback=%d, want commit=1 rollback=0", bCommit, bRollback)
		}
	})
}

// TestWithTx_SamePoolNested_NoNewBegin verifies the unchanged single-pool
// behavior: a nested WithTx on the SAME pool reuses the outer transaction and
// does not begin a second one (no savepoints).
func TestWithTx_SamePoolNested_NoNewBegin(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "pool-scoped-transaction", "nested-work-on-the-same-pool-reuses-the-transaction")
	begins := 0
	tx := &mockTx{}
	pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) {
		begins++
		return tx, nil
	}}

	err := WithTx(context.Background(), pool, func(ctx context.Context) error {
		return WithTx(ctx, pool, func(ctx context.Context) error {
			if TxFromContext(ctx, pool) != tx {
				t.Error("nested same-pool WithTx should reuse the outer tx")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if begins != 1 {
		t.Errorf("expected exactly one begin for same-pool nesting, got %d", begins)
	}
}

// TestWithTx_TwoPools_PanicRollsBackBoth guards the panic-safe finalizers under
// two-pool nesting: a panic in the innermost callback must roll back BOTH the
// inner (pool B) and outer (pool A) transactions — releasing both pooled
// connections — and commit neither, before the panic continues to unwind.
func TestWithTx_TwoPools_PanicRollsBackBoth(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "multi-datasource-boundary", "panic-rolls-back-every-enrolled-pool")
	var aCommit, aRollback, bCommit, bRollback int
	txA := &mockTx{
		commitFn:   func(_ context.Context) error { aCommit++; return nil },
		rollbackFn: func(_ context.Context) error { aRollback++; return nil },
	}
	txB := &mockTx{
		commitFn:   func(_ context.Context) error { bCommit++; return nil },
		rollbackFn: func(_ context.Context) error { bRollback++; return nil },
	}
	poolA := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return txA, nil }}
	poolB := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return txB, nil }}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic to propagate out of nested WithTx")
		}
		if aRollback != 1 || bRollback != 1 {
			t.Errorf("expected both txs rolled back on panic, got A=%d B=%d", aRollback, bRollback)
		}
		if aCommit != 0 || bCommit != 0 {
			t.Errorf("expected no commits on panic, got A=%d B=%d", aCommit, bCommit)
		}
	}()

	_ = WithTx(context.Background(), poolA, func(ctx context.Context) error {
		return WithTx(ctx, poolB, func(_ context.Context) error {
			panic("boom")
		})
	})
}

// TestWithTx_FailedCommitReleasesConnection asserts the commit-failure path
// still rolls the transaction back. pgx auto-aborts a transaction whose Commit
// failed, but the pooled connection stays checked out until something
// finalizes it — without this release a run of commit failures drains the pool.
func TestWithTx_FailedCommitReleasesConnection(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "transaction-finalization", "failed-commit-releases-the-connection")

	rollbacks := 0
	tx := &mockTx{
		commitFn:   func(_ context.Context) error { return fmt.Errorf("commit failed") },
		rollbackFn: func(_ context.Context) error { rollbacks++; return nil },
	}
	pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return tx, nil }}

	err := WithTx(context.Background(), pool, func(_ context.Context) error { return nil })
	if err == nil {
		t.Fatal("expected the commit failure to surface")
	}
	if !strings.Contains(err.Error(), "commit failed") {
		t.Errorf("error = %v, want the commit failure surfaced", err)
	}
	if rollbacks != 1 {
		t.Errorf("rollbacks = %d, want exactly 1 (release the connection once, no double finalize)", rollbacks)
	}
}
