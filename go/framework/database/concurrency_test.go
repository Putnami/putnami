package database

import (
	"context"
	stdsql "database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"

	"go.putnami.dev/migration"
)

// TestSQLRunner_MaterializeMigrators_ConcurrentSingleInit exercises the
// lazy-init guard in materializeMigrators from many goroutines at once.
// Run under -race it proves the mutex discipline; the OpenDB call count
// proves the check-then-populate happens exactly once.
func TestSQLRunner_MaterializeMigrators_ConcurrentSingleInit(t *testing.T) {
	reg := migration.NewRegistry()
	_ = reg.AddSource(NewSQLSource("iam", Datasource{Name: "default"}, fstest.MapFS{
		"001_init.up.sql": &fstest.MapFile{Data: []byte("SELECT 1")},
	}))

	db := newTestMigratorDB(t)
	var opens int64
	runner := newSQLRunner(SQLRunnerOptions{
		Registry: reg,
		OpenDB: func() (*stdsql.DB, error) {
			atomic.AddInt64(&opens, 1)
			return db, nil
		},
		AutoApply: true,
	})

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	results := make([]map[string]*Migrator, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = runner.materializeMigrators()
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, errs[i])
		}
	}
	if got := atomic.LoadInt64(&opens); got != 1 {
		t.Errorf("OpenDB called %d times; expected exactly one materialization", got)
	}
	for i := 1; i < n; i++ {
		if len(results[i]) != len(results[0]) {
			t.Errorf("goroutines observed inconsistent migrator maps: %d vs %d",
				len(results[i]), len(results[0]))
		}
	}
}

// TestWithTx_ConcurrentTransactions drives WithTx from many goroutines,
// each with its own pool and transaction. Run under -race it verifies the
// transaction-in-context plumbing carries no shared mutable state.
func TestWithTx_ConcurrentTransactions(t *testing.T) {
	const n = 16
	var committed int64

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			tx := &mockTx{
				commitFn: func(_ context.Context) error {
					atomic.AddInt64(&committed, 1)
					return nil
				},
			}
			pool := &Pool{
				beginTx: func(_ context.Context) (pgx.Tx, error) { return tx, nil },
			}
			err := WithTx(context.Background(), pool, func(ctx context.Context) error {
				if TxFromContext(ctx, pool) == nil {
					t.Error("expected tx in context")
				}
				return nil
			})
			if err != nil {
				t.Errorf("WithTx: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&committed); got != n {
		t.Errorf("expected %d commits, got %d", n, got)
	}
}
