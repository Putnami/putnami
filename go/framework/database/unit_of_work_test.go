package database

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"go.putnami.dev/inject"
	"go.putnami.dev/protocol/features/spectest"
)

// countingTx builds a mockTx that increments the given counters on
// commit/rollback, so a test can assert exactly how each pool's transaction was
// finalized.
func countingTx(commit, rollback *int) *mockTx {
	return &mockTx{
		commitFn:   func(_ context.Context) error { *commit++; return nil },
		rollbackFn: func(_ context.Context) error { *rollback++; return nil },
	}
}

// TestUnitOfWork_SamePool_TwoReposAtomic pins the single-datasource invariant:
// two repositories on the SAME pool enroll once (one begin, one shared tx) and
// commit atomically on success; a failure rolls the single transaction back once
// with no commit. No partial write is observable because both repos ran in one
// transaction.
func TestUnitOfWork_SamePool_TwoReposAtomic(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "unit-of-work", "one-transaction-per-enrolled-pool")
	t.Run("success commits once", func(t *testing.T) {
		var commit, rollback, begins int
		tx := countingTx(&commit, &rollback)
		pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { begins++; return tx, nil }}

		uow := newUnitOfWork(0)
		ctx := context.Background()

		// Two repositories on the same pool share one transaction.
		txA, err := uow.enroll(ctx, pool)
		if err != nil {
			t.Fatalf("enroll repo A: %v", err)
		}
		txB, err := uow.enroll(ctx, pool)
		if err != nil {
			t.Fatalf("enroll repo B: %v", err)
		}
		if txA != txB {
			t.Error("two repos on the same pool must share one transaction")
		}
		if begins != 1 {
			t.Errorf("expected exactly one begin for a shared pool, got %d", begins)
		}

		if err := uow.FinalizeScope(ctx, nil); err != nil {
			t.Fatalf("finalize success: %v", err)
		}
		if commit != 1 || rollback != 0 {
			t.Errorf("commit=%d rollback=%d, want commit=1 rollback=0", commit, rollback)
		}
	})

	t.Run("error rolls back both, no commit", func(t *testing.T) {
		var commit, rollback int
		tx := countingTx(&commit, &rollback)
		pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return tx, nil }}

		uow := newUnitOfWork(0)
		ctx := context.Background()
		if _, err := uow.enroll(ctx, pool); err != nil {
			t.Fatalf("enroll: %v", err)
		}
		if _, err := uow.enroll(ctx, pool); err != nil {
			t.Fatalf("enroll: %v", err)
		}

		if err := uow.FinalizeScope(ctx, context.Canceled); err != nil {
			t.Fatalf("finalize error: %v", err)
		}
		if commit != 0 || rollback != 1 {
			t.Errorf("commit=%d rollback=%d, want commit=0 rollback=1 (no partial write)", commit, rollback)
		}
	})
}

// TestUnitOfWork_TwoPools_BothCommit verifies the happy multi-datasource path:
// two distinct pools both commit on success.
func TestUnitOfWork_TwoPools_BothCommit(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "multi-datasource-boundary", "two-pools-commit-sequentially-on-success")
	var aCommit, aRollback, bCommit, bRollback int
	txA := countingTx(&aCommit, &aRollback)
	txB := countingTx(&bCommit, &bRollback)
	poolA := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return txA, nil }}
	poolB := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return txB, nil }}

	uow := newUnitOfWork(0)
	ctx := context.Background()
	if _, err := uow.enroll(ctx, poolA); err != nil {
		t.Fatalf("enroll A: %v", err)
	}
	if _, err := uow.enroll(ctx, poolB); err != nil {
		t.Fatalf("enroll B: %v", err)
	}

	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if aCommit != 1 || bCommit != 1 {
		t.Errorf("commits A=%d B=%d, want 1/1", aCommit, bCommit)
	}
	if aRollback != 0 || bRollback != 0 {
		t.Errorf("rollbacks A=%d B=%d, want 0/0", aRollback, bRollback)
	}
}

// TestUnitOfWork_TwoPools_PartialCommitSurfaced pins the documented non-atomic
// multi-datasource boundary: a commit failure on the SECOND datasource after the
// first already committed leaves the first committed, surfaces an error, and
// releases the failed datasource's connection. It is best-effort sequential
// commit, explicitly NOT 2PC.
func TestUnitOfWork_TwoPools_PartialCommitSurfaced(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "multi-datasource-boundary", "partial-commit-reports-prefix-and-rolls-back-remainder")
	var aCommit, aRollback, bCommit, bRollback int
	txA := countingTx(&aCommit, &aRollback)
	txB := &mockTx{
		commitFn:   func(_ context.Context) error { bCommit++; return fmt.Errorf("datasource B commit failed") },
		rollbackFn: func(_ context.Context) error { bRollback++; return nil },
	}
	poolA := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return txA, nil }}
	poolB := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return txB, nil }}

	uow := newUnitOfWork(0)
	ctx := context.Background()
	// Enrollment order is A then B, so A commits first.
	if _, err := uow.enroll(ctx, poolA); err != nil {
		t.Fatalf("enroll A: %v", err)
	}
	if _, err := uow.enroll(ctx, poolB); err != nil {
		t.Fatalf("enroll B: %v", err)
	}

	err := uow.Commit(ctx)
	if err == nil {
		t.Fatal("expected the partial-commit failure to be surfaced, got nil")
	}
	// Datasource A committed and STAYS committed — not undone.
	if aCommit != 1 {
		t.Errorf("A commit=%d, want 1 (stays committed across the partial boundary)", aCommit)
	}
	if aRollback != 0 {
		t.Errorf("A rollback=%d, want 0 (already committed, never rolled back)", aRollback)
	}
	// Datasource B failed to commit and was rolled back to release its connection.
	if bCommit != 1 {
		t.Errorf("B commit attempts=%d, want 1", bCommit)
	}
	if bRollback != 1 {
		t.Errorf("B rollback=%d, want 1 (release the failed datasource's connection)", bRollback)
	}
}

// TestUnitOfWork_FinalizeScope_ReconcilesOutcome checks the request-boundary
// contract directly: a nil outcome commits, a non-nil outcome rolls back.
func TestUnitOfWork_FinalizeScope_ReconcilesOutcome(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "unit-of-work", "finalize-scope-reconciles-the-request-outcome")
	t.Run("success outcome commits", func(t *testing.T) {
		var commit, rollback int
		tx := countingTx(&commit, &rollback)
		pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return tx, nil }}
		uow := newUnitOfWork(0)
		uow.enroll(context.Background(), pool) //nolint:errcheck // begin cannot fail on the mock
		if err := uow.FinalizeScope(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if commit != 1 || rollback != 0 {
			t.Errorf("commit=%d rollback=%d, want 1/0", commit, rollback)
		}
	})

	t.Run("failure outcome rolls back", func(t *testing.T) {
		var commit, rollback int
		tx := countingTx(&commit, &rollback)
		pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return tx, nil }}
		uow := newUnitOfWork(0)
		uow.enroll(context.Background(), pool) //nolint:errcheck // begin cannot fail on the mock
		if err := uow.FinalizeScope(context.Background(), fmt.Errorf("handler failed")); err != nil {
			t.Fatal(err)
		}
		if commit != 0 || rollback != 1 {
			t.Errorf("commit=%d rollback=%d, want 0/1", commit, rollback)
		}
	})
}

// TestUnitOfWork_RollbackOnly_RollsBackDespiteSuccess verifies the rollback-only
// escape hatch: a unit marked rollback-only rolls back even when the boundary
// reports success.
func TestUnitOfWork_RollbackOnly_RollsBackDespiteSuccess(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "unit-of-work", "rollback-only-rolls-back-despite-success")
	var commit, rollback int
	tx := countingTx(&commit, &rollback)
	pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return tx, nil }}
	uow := newUnitOfWork(0)
	uow.enroll(context.Background(), pool) //nolint:errcheck // begin cannot fail on the mock
	uow.SetRollbackOnly()

	if err := uow.FinalizeScope(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if commit != 0 || rollback != 1 {
		t.Errorf("commit=%d rollback=%d, want 0/1 (rollback-only overrides success)", commit, rollback)
	}
}

// TestUnitOfWork_RollbackRunsOnCanceledContext pins the cancellation/timeout
// safety: a boundary rollback must still run — and release the connection — after
// the request context is canceled, so opContext strips cancellation and the
// transaction's Rollback sees a live context.
func TestUnitOfWork_RollbackRunsOnCanceledContext(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "unit-of-work", "rollback-runs-on-a-canceled-context")
	rolledBack := false
	var rollbackCtxErr error
	tx := &mockTx{
		rollbackFn: func(ctx context.Context) error {
			rolledBack = true
			rollbackCtxErr = ctx.Err()
			return nil
		},
		commitFn: func(_ context.Context) error {
			t.Error("commit must not run on the cancellation path")
			return nil
		},
	}
	pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return tx, nil }}

	uow := newUnitOfWork(0)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := uow.enroll(ctx, pool); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	cancel() // the request context is canceled before the boundary runs

	if err := uow.FinalizeScope(ctx, context.Canceled); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !rolledBack {
		t.Fatal("expected rollback to run despite the canceled request context")
	}
	if rollbackCtxErr != nil {
		t.Errorf("rollback context must not be canceled (context.WithoutCancel), got %v", rollbackCtxErr)
	}
}

// TestUnitOfWork_Timeout_BoundsFinalizer verifies a configured timeout bounds the
// commit/rollback so a wedged finalizer cannot pin a connection forever.
func TestUnitOfWork_Timeout_BoundsFinalizer(t *testing.T) {
	var hadDeadline bool
	tx := &mockTx{
		commitFn: func(ctx context.Context) error {
			_, hadDeadline = ctx.Deadline()
			return nil
		},
	}
	pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return tx, nil }}

	uow := newUnitOfWork(50 * time.Millisecond)
	uow.enroll(context.Background(), pool) //nolint:errcheck // begin cannot fail on the mock
	if err := uow.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !hadDeadline {
		t.Error("expected the commit context to carry the configured timeout deadline")
	}
}

// TestUnitOfWork_JoinsOuterWithTx pins the nesting rule: a unit of work enrolling
// a pool that already carries an explicit WithTx transaction joins that
// transaction (no new begin) and does NOT take ownership, so finalizing the unit
// never commits or rolls back the outer transaction.
func TestUnitOfWork_JoinsOuterWithTx(t *testing.T) {
	var outerCommit, outerRollback, uowBegins int
	outerTx := countingTx(&outerCommit, &outerRollback)
	pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) {
		uowBegins++
		return &mockTx{}, nil
	}}

	// The outer WithTx has bound its transaction for pool in ctx.
	ctx := contextWithTx(context.Background(), pool, outerTx)

	uow := newUnitOfWork(0)
	got, err := uow.enroll(ctx, pool)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if got != outerTx {
		t.Error("unit of work must join the outer WithTx transaction")
	}
	if uowBegins != 0 {
		t.Errorf("unit of work must not begin its own tx when joining, begins=%d", uowBegins)
	}

	// Finalizing the unit is a no-op on the outer transaction — the outer WithTx
	// still owns commit/rollback.
	if err := uow.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if outerCommit != 0 || outerRollback != 0 {
		t.Errorf("outer tx must be untouched by the joining unit, commit=%d rollback=%d", outerCommit, outerRollback)
	}
}

// TestWithTx_JoinsAmbientUnitOfWork pins the symmetric nesting rule: after a
// request unit has enrolled a pool, WithTx must reuse that exact transaction,
// expose it through the callback context, and leave commit/rollback ownership
// with the request boundary.
func TestWithTx_JoinsAmbientUnitOfWork(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "unit-of-work", "withtx-joins-ambient-unit-of-work")
	tests := []struct {
		name         string
		callbackErr  error
		wantCommit   int
		wantRollback int
	}{
		{name: "success commits at request boundary", wantCommit: 1},
		{name: "error rolls back at request boundary", callbackErr: fmt.Errorf("handler failed"), wantRollback: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var begins, ambientQueries, ambientCommit, ambientRollback int
			var secondQueries, secondCommit, secondRollback int
			ambientTx := countingTx(&ambientCommit, &ambientRollback)
			ambientTx.execFn = func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
				ambientQueries++
				return pgconn.NewCommandTag("INSERT 0 1"), nil
			}
			secondTx := countingTx(&secondCommit, &secondRollback)
			secondTx.execFn = func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
				secondQueries++
				return pgconn.NewCommandTag("INSERT 0 1"), nil
			}
			pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) {
				begins++
				if begins == 1 {
					return ambientTx, nil
				}
				return secondTx, nil
			}}

			cc, scope := scopedUoW(t)
			defer cc.Close()
			ctx := scope.Context(context.Background())
			if !InUnitOfWork(ctx) {
				t.Fatal("expected request context to report an active UnitOfWork")
			}

			// Enroll the request transaction before entering WithTx, matching a
			// handler that has already written uncommitted request data.
			if _, err := pool.Exec(ctx, "insert request row"); err != nil {
				t.Fatalf("request exec: %v", err)
			}

			gotErr := WithTx(ctx, pool, func(txCtx context.Context) error {
				if got := TxFromContext(txCtx, pool); got != ambientTx {
					t.Errorf("WithTx callback transaction = %T, want ambient UnitOfWork transaction", got)
				}
				if _, err := pool.Exec(txCtx, "read request row and write related row"); err != nil {
					return err
				}
				return tt.callbackErr
			})
			if gotErr != tt.callbackErr {
				t.Fatalf("WithTx error = %v, want %v", gotErr, tt.callbackErr)
			}

			if begins != 1 {
				t.Errorf("transaction begins = %d, want 1 (WithTx must not open a second connection)", begins)
			}
			if ambientQueries != 2 || secondQueries != 0 {
				t.Errorf("queries ambient=%d second=%d, want 2/0", ambientQueries, secondQueries)
			}
			if ambientCommit != 0 || ambientRollback != 0 || secondCommit != 0 || secondRollback != 0 {
				t.Errorf("WithTx finalized before request boundary: ambient commit/rollback=%d/%d second=%d/%d",
					ambientCommit, ambientRollback, secondCommit, secondRollback)
			}

			if err := scope.Finalize(ctx, tt.callbackErr); err != nil {
				t.Fatalf("request finalize: %v", err)
			}
			if ambientCommit != tt.wantCommit || ambientRollback != tt.wantRollback {
				t.Errorf("ambient commit/rollback=%d/%d, want %d/%d",
					ambientCommit, ambientRollback, tt.wantCommit, tt.wantRollback)
			}
			_ = scope.Close()
		})
	}
}

// TestUnitOfWork_Idempotentfinalize verifies the unit finalizes at most once: a
// second FinalizeScope (e.g. the panic path after the normal path) is a no-op.
func TestUnitOfWork_Idempotentfinalize(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "unit-of-work", "finalize-is-idempotent")
	var commit, rollback int
	tx := countingTx(&commit, &rollback)
	pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return tx, nil }}
	uow := newUnitOfWork(0)
	uow.enroll(context.Background(), pool) //nolint:errcheck // begin cannot fail on the mock

	if err := uow.FinalizeScope(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	// A second finalize (even with the opposite outcome) must not touch the tx.
	if err := uow.FinalizeScope(context.Background(), fmt.Errorf("late failure")); err != nil {
		t.Fatal(err)
	}
	if commit != 1 || rollback != 0 {
		t.Errorf("commit=%d rollback=%d, want 1/0 — finalize must run at most once", commit, rollback)
	}
	// enroll after finalize is rejected rather than silently opening a leaked tx.
	if _, err := uow.enroll(context.Background(), pool); err == nil {
		t.Error("expected enroll after finalize to be rejected")
	}
}

// scopedUoW registers a Scoped *UnitOfWork provider and returns a started
// container context plus a fresh request scope — mirroring what database/plugin.go
// registers when PluginConfig.UnitOfWork is enabled.
func scopedUoW(t *testing.T) (*inject.ContainerContext, *inject.DetachedScope) {
	t.Helper()
	cc := inject.NewContainerContext("uow-test")
	if err := cc.Register(inject.Provide(
		inject.TokenOf[*UnitOfWork](),
		func(_ inject.Resolver) (any, error) { return newUnitOfWork(0), nil },
		inject.WithScope(inject.Scoped),
	)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	scope, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	return cc, scope
}

// TestUnitOfWork_TransparentEnrollment_ViaPoolExec is the DI integration test:
// with a request scope carrying the Scoped UnitOfWork provider, ordinary
// pool.Exec queries transparently enroll the pool (one begin, one shared tx), and
// the scope boundary commits on success — no repository changes, no explicit
// context threading.
func TestUnitOfWork_TransparentEnrollment_ViaPoolExec(t *testing.T) {
	var commit, rollback, begins int
	tx := countingTx(&commit, &rollback)
	pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { begins++; return tx, nil }}

	cc, scope := scopedUoW(t)
	defer cc.Close()
	ctx := scope.Context(context.Background())

	// Two "repository" queries on the same pool within one request.
	if _, err := pool.Exec(ctx, "insert into t values (1)"); err != nil {
		t.Fatalf("exec 1: %v", err)
	}
	if _, err := pool.Exec(ctx, "insert into t values (2)"); err != nil {
		t.Fatalf("exec 2: %v", err)
	}
	if begins != 1 {
		t.Errorf("expected exactly one begin for two same-pool queries, got %d", begins)
	}

	// Boundary success commits the shared transaction once.
	if err := scope.Finalize(ctx, nil); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if commit != 1 || rollback != 0 {
		t.Errorf("commit=%d rollback=%d, want 1/0", commit, rollback)
	}
	_ = scope.Close()
}

// TestUnitOfWork_TransparentEnrollment_ErrorRollsBack verifies the boundary rolls
// back the transparently-enrolled transaction when the request outcome is a
// failure.
func TestUnitOfWork_TransparentEnrollment_ErrorRollsBack(t *testing.T) {
	var commit, rollback int
	tx := countingTx(&commit, &rollback)
	pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return tx, nil }}

	cc, scope := scopedUoW(t)
	defer cc.Close()
	ctx := scope.Context(context.Background())

	if _, err := pool.Exec(ctx, "insert into t values (1)"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := scope.Finalize(ctx, fmt.Errorf("handler error")); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if commit != 0 || rollback != 1 {
		t.Errorf("commit=%d rollback=%d, want 0/1", commit, rollback)
	}
	_ = scope.Close()
}

// TestUnitOfWork_EnrollBeginFailure_SurfacedByExec verifies a failed transaction
// begin during transparent enrollment is surfaced by pool.Exec instead of
// silently running the statement outside the unit of work (which would break
// atomicity).
func TestUnitOfWork_EnrollBeginFailure_SurfacedByExec(t *testing.T) {
	pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) {
		return nil, fmt.Errorf("connection refused")
	}}

	cc, scope := scopedUoW(t)
	defer cc.Close()
	ctx := scope.Context(context.Background())

	if _, err := pool.Exec(ctx, "insert into t values (1)"); err == nil {
		t.Fatal("expected the enrollment begin failure to be surfaced by Exec")
	}
	_ = scope.Close()
}

// TestUnitOfWork_NoScope_UsesAutocommit verifies the query path is unchanged when
// no DI scope (and therefore no unit of work) is active: pool.Querier falls back
// to the raw pool as before, so non-UnitOfWork workloads keep autocommit.
func TestUnitOfWork_NoScope_ByPassesUnitOfWork(t *testing.T) {
	// A background context carries no scope, so querier must not attempt to
	// resolve or enroll a unit of work.
	if InUnitOfWork(context.Background()) {
		t.Fatal("expected InUnitOfWork to be false without an active scope")
	}
	if uow := unitOfWorkFromContext(context.Background()); uow != nil {
		t.Fatal("expected no unit of work without an active scope")
	}
}
