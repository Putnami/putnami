package database

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	perrors "go.putnami.dev/errors"
)

// --- entity type for tests ---

type testEntity struct {
	ID   int
	Name string
}

func scanTestEntity(row pgx.Row) (testEntity, error) {
	var e testEntity
	err := row.Scan(&e.ID, &e.Name)
	return e, err
}

// newTestRepo creates a Repository[testEntity] with a Pool whose Querier returns
// the given mock when a tx is present in context.
func newTestRepo(q *mockQuerier) (*Repository[testEntity], context.Context) {
	pool := &Pool{} // nil pgxpool — we rely on tx-in-context
	ctx := ctxWithQuerier(pool, q)
	repo := NewRepository[testEntity](pool, "users", scanTestEntity)
	return repo, ctx
}

// --- Repository method tests ---

func TestRepository_FindByID_Success(t *testing.T) {
	q := &mockQuerier{
		queryRowFn: func(_ context.Context, sql string, args ...any) pgx.Row {
			return &mockRow{scanFn: func(dest ...any) error {
				*dest[0].(*int) = 1
				*dest[1].(*string) = "Alice"
				return nil
			}}
		},
	}
	repo, ctx := newTestRepo(q)
	e, err := repo.FindByID(ctx, "id", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.ID != 1 || e.Name != "Alice" {
		t.Errorf("got %+v, want {1, Alice}", e)
	}
}

func TestRepository_FindAll(t *testing.T) {
	q := &mockQuerier{
		queryFn: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
			return &mockRows{
				items: []func(dest ...any) error{
					func(dest ...any) error {
						*dest[0].(*int) = 1
						*dest[1].(*string) = "Alice"
						return nil
					},
					func(dest ...any) error {
						*dest[0].(*int) = 2
						*dest[1].(*string) = "Bob"
						return nil
					},
				},
			}, nil
		},
	}
	repo, ctx := newTestRepo(q)
	entities, err := repo.FindAll(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entities) != 2 {
		t.Fatalf("expected 2 entities, got %d", len(entities))
	}
	if entities[0].Name != "Alice" || entities[1].Name != "Bob" {
		t.Errorf("unexpected entities: %+v", entities)
	}
}

func TestRepository_FindAllPaginated_QueryError(t *testing.T) {
	q := &mockQuerier{
		queryFn: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}
	repo, ctx := newTestRepo(q)
	_, err := repo.FindAllPaginated(ctx, 10, 0)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRepository_FindWhere(t *testing.T) {
	q := &mockQuerier{
		queryFn: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
			return &mockRows{
				items: []func(dest ...any) error{
					func(dest ...any) error {
						*dest[0].(*int) = 3
						*dest[1].(*string) = "Charlie"
						return nil
					},
				},
			}, nil
		},
	}
	repo, ctx := newTestRepo(q)
	entities, err := repo.FindWhere(ctx, "age > $1", 18)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entities) != 1 || entities[0].Name != "Charlie" {
		t.Errorf("unexpected: %+v", entities)
	}
}

func TestRepository_FindWhere_QueryError(t *testing.T) {
	q := &mockQuerier{
		queryFn: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
			return nil, fmt.Errorf("error")
		},
	}
	repo, ctx := newTestRepo(q)
	_, err := repo.FindWhere(ctx, "x = $1", 1)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRepository_FindOneWhere(t *testing.T) {
	q := &mockQuerier{
		queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return &mockRow{scanFn: func(dest ...any) error {
				*dest[0].(*int) = 5
				*dest[1].(*string) = "Eve"
				return nil
			}}
		},
	}
	repo, ctx := newTestRepo(q)
	e, err := repo.FindOneWhere(ctx, "name = $1", "Eve")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.ID != 5 || e.Name != "Eve" {
		t.Errorf("got %+v", e)
	}
}

func TestRepository_Count(t *testing.T) {
	t.Run("without where", func(t *testing.T) {
		q := &mockQuerier{
			queryRowFn: func(_ context.Context, sql string, _ ...any) pgx.Row {
				if sql != `SELECT COUNT(*) FROM "users"` {
					t.Errorf("unexpected query: %s", sql)
				}
				return &mockRow{scanFn: func(dest ...any) error {
					*dest[0].(*int64) = 42
					return nil
				}}
			},
		}
		repo, ctx := newTestRepo(q)
		count, err := repo.Count(ctx, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if count != 42 {
			t.Errorf("got %d, want 42", count)
		}
	})

	t.Run("with where", func(t *testing.T) {
		q := &mockQuerier{
			queryRowFn: func(_ context.Context, sql string, _ ...any) pgx.Row {
				if sql != `SELECT COUNT(*) FROM "users" WHERE active = $1` {
					t.Errorf("unexpected query: %s", sql)
				}
				return &mockRow{scanFn: func(dest ...any) error {
					*dest[0].(*int64) = 10
					return nil
				}}
			},
		}
		repo, ctx := newTestRepo(q)
		count, err := repo.Count(ctx, "active = $1", true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if count != 10 {
			t.Errorf("got %d, want 10", count)
		}
	})
}

func TestRepository_Exists(t *testing.T) {
	q := &mockQuerier{
		queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return &mockRow{scanFn: func(dest ...any) error {
				*dest[0].(*bool) = true
				return nil
			}}
		},
	}
	repo, ctx := newTestRepo(q)
	exists, err := repo.Exists(ctx, "id = $1", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Error("expected true")
	}
}

func TestRepository_DeleteWhere(t *testing.T) {
	q := &mockQuerier{
		execFn: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("DELETE 3"), nil
		},
	}
	repo, ctx := newTestRepo(q)
	n, err := repo.DeleteWhere(ctx, "active = $1", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 3 {
		t.Errorf("got %d, want 3", n)
	}
}

func TestRepository_DeleteWhere_Error(t *testing.T) {
	q := &mockQuerier{
		execFn: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, fmt.Errorf("error")
		},
	}
	repo, ctx := newTestRepo(q)
	_, err := repo.DeleteWhere(ctx, "id = $1", 1)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRepository_DeleteByID_Success(t *testing.T) {
	q := &mockQuerier{
		execFn: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("DELETE 1"), nil
		},
	}
	repo, ctx := newTestRepo(q)
	err := repo.DeleteByID(ctx, "id", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRepository_Query(t *testing.T) {
	q := &mockQuerier{
		queryFn: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
			return &mockRows{
				items: []func(dest ...any) error{
					func(dest ...any) error {
						*dest[0].(*int) = 1
						*dest[1].(*string) = "Alice"
						return nil
					},
				},
			}, nil
		},
	}
	repo, ctx := newTestRepo(q)
	entities, err := repo.Query(ctx, "SELECT * FROM users WHERE id = $1", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entities) != 1 {
		t.Fatalf("expected 1, got %d", len(entities))
	}
}

func TestRepository_Query_Error(t *testing.T) {
	q := &mockQuerier{
		queryFn: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
			return nil, fmt.Errorf("error")
		},
	}
	repo, ctx := newTestRepo(q)
	_, err := repo.Query(ctx, "SELECT 1")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRepository_QueryOne(t *testing.T) {
	q := &mockQuerier{
		queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return &mockRow{scanFn: func(dest ...any) error {
				*dest[0].(*int) = 99
				*dest[1].(*string) = "Zara"
				return nil
			}}
		},
	}
	repo, ctx := newTestRepo(q)
	e, err := repo.QueryOne(ctx, "SELECT * FROM users WHERE id = $1", 99)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.ID != 99 || e.Name != "Zara" {
		t.Errorf("got %+v", e)
	}
}

func TestRepository_collectRows_ScanError(t *testing.T) {
	repo := NewRepository[testEntity](nil, "users", scanTestEntity)
	rows := &mockRows{
		items: []func(dest ...any) error{
			func(_ ...any) error { return fmt.Errorf("scan error") },
		},
	}
	_, err := repo.collectRows(context.Background(), rows)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRepository_collectRows_RowsError(t *testing.T) {
	repo := NewRepository[testEntity](nil, "users", scanTestEntity)
	rows := &mockRows{err: fmt.Errorf("rows error")}
	_, err := repo.collectRows(context.Background(), rows)
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- wrapQueryError ---

func TestWrapQueryError(t *testing.T) {
	t.Run("without context error", func(t *testing.T) {
		err := wrapQueryError(context.Background(), fmt.Errorf("db error"), "find")
		if err == nil {
			t.Fatal("expected error")
		}
		var ce *perrors.Error
		if !errors.As(err, &ce) {
			t.Fatalf("expected *perrors.Error, got %T", err)
		}
	})

	t.Run("with canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := wrapQueryError(ctx, fmt.Errorf("db error"), "find")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

// --- Pool.Querier ---

func TestPool_Querier_WithTx(t *testing.T) {
	pool := &Pool{}
	tx := &mockTx{}
	ctx := contextWithTx(context.Background(), pool, tx)
	q := pool.Querier(ctx)
	if q != tx {
		t.Error("expected tx to be returned as querier")
	}
}

func TestPool_Querier_WithoutTx(t *testing.T) {
	pool := &Pool{} // not open
	q := pool.Querier(context.Background())
	// Returns the datasource-bound view (a nil *PGXPool wrapped in the Querier
	// interface when the pool is not open)
	if q == nil {
		t.Error("expected non-nil interface (typed nil)")
	}
}

// --- Transaction ---

func TestWithTx_NestedReusesExisting(t *testing.T) {
	tx := &mockTx{}
	pool := &Pool{}
	ctx := contextWithTx(context.Background(), pool, tx)
	called := false
	err := WithTx(ctx, pool, func(ctx context.Context) error {
		called = true
		// Verify same tx in context
		if TxFromContext(ctx, pool) != tx {
			t.Error("expected same tx")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("fn was not called")
	}
}

// --- Plugin ---

func TestPlugin_Name(t *testing.T) {
	p := NewPlugin(PluginConfig{})
	if p.Name() != "database" {
		t.Errorf("got %q, want database", p.Name())
	}
}

func TestPlugin_Stop_NilPool(t *testing.T) {
	p := NewPlugin(PluginConfig{})
	err := p.Stop(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPlugin_Configure_NoMigration(t *testing.T) {
	p := NewPlugin(PluginConfig{})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Plugin.Configure now registers an SQLRunner into the per-app
// migration.Registry rather than directly invoking Up. The full
// configure-through-Migrate flow is exercised by the app package's
// lifecycle tests and by the SQL-runner-specific tests below; here we
// only assert the "no migration configured → no-op" path.

// --- Migration ---

func TestNewMigrator_DefaultDatasource(t *testing.T) {
	m := NewMigrator(nil, MigrationConfig{})
	if m.dbName != "default" {
		t.Errorf("got %q, want default", m.dbName)
	}
	if len(m.definitions) != 0 {
		t.Errorf("expected no definitions, got %d", len(m.definitions))
	}
}

func TestNewMigrator_CustomDatasource(t *testing.T) {
	m := NewMigrator(nil, MigrationConfig{Datasource: "analytics", Definitions: []Definition{
		{Name: "iam/001_init", SQL: "SELECT 1"},
	}})
	if m.dbName != "analytics" {
		t.Errorf("got %q, want analytics", m.dbName)
	}
	if len(m.definitions) != 1 {
		t.Errorf("expected 1 definition, got %d", len(m.definitions))
	}
}

// --- rowAdapter ---

func TestRowAdapter_Scan(t *testing.T) {
	scanErr := fmt.Errorf("scan error")
	rows := &mockRows{
		items: []func(dest ...any) error{
			func(_ ...any) error { return scanErr },
		},
	}
	rows.Next()
	adapter := rowAdapter{rows}
	err := adapter.Scan()
	if !errors.Is(err, scanErr) {
		t.Errorf("expected scan error, got %v", err)
	}
}

func TestPlugin_Provides(t *testing.T) {
	// A primary-bearing config (including the default single-pool case)
	// registers both the *Pools registry and the unnamed *Pool.
	for _, cfg := range []PluginConfig{
		{},
		{Datasource: "core", Datasources: []DatasourceConfig{{Name: "iam"}}},
		{Pool: PoolConfig{DSN: "postgres://x/y"}, Datasources: []DatasourceConfig{{Name: "iam"}}},
	} {
		if got := len(NewPlugin(cfg).Provides()); got != 2 {
			t.Errorf("Provides() = %d registrations for %+v, want 2 (*Pools + *Pool)", got, cfg)
		}
	}

	// A Datasources-only workload with no primary registers only *Pools:
	// there is no default datasource to bind the unnamed *Pool to.
	only := NewPlugin(PluginConfig{Datasources: []DatasourceConfig{{Name: "iam"}, {Name: "billing"}}})
	if got := len(only.Provides()); got != 1 {
		t.Errorf("Provides() = %d registrations for Datasources-only, want 1 (*Pools)", got)
	}
}

func TestPlugin_Stop_WithPool(t *testing.T) {
	p := NewPlugin(PluginConfig{})
	p.pool = &Pool{} // nil inner pool — Close is now nil-safe
	err := p.Stop(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPlugin_CheckHealth_NotConfigured(t *testing.T) {
	p := NewPlugin(PluginConfig{})
	// Configure was never called: no owner, no pool. Expect a clear error
	// rather than a panic — the health endpoint surfaces it as a degraded probe.
	if err := p.CheckHealth(context.Background()); err == nil {
		t.Fatal("expected error when plugin is not configured")
	}
}

func TestPool_Close_NilPool(t *testing.T) {
	pool := &Pool{}
	pool.Close() // should not panic
}

// --- Pool convenience methods with tx in context ---

func TestPool_Exec_WithTx(t *testing.T) {
	pool := &Pool{}
	mq := &mockQuerier{
		execFn: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	tx := &mockTx{mockQuerier: *mq}
	ctx := contextWithTx(context.Background(), pool, tx)

	tag, err := pool.Exec(ctx, "UPDATE users SET name = $1", "Alice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Errorf("got %d rows affected, want 1", tag.RowsAffected())
	}
}

func TestPool_Query_WithTx(t *testing.T) {
	pool := &Pool{}
	mq := &mockQuerier{
		queryFn: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
			return &mockRows{}, nil
		},
	}
	tx := &mockTx{mockQuerier: *mq}
	ctx := contextWithTx(context.Background(), pool, tx)

	rows, err := pool.Query(ctx, "SELECT 1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rows.Close()
}

func TestPool_QueryRow_WithTx(t *testing.T) {
	pool := &Pool{}
	mq := &mockQuerier{
		queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return &mockRow{}
		},
	}
	tx := &mockTx{mockQuerier: *mq}
	ctx := contextWithTx(context.Background(), pool, tx)

	row := pool.QueryRow(ctx, "SELECT 1")
	if row == nil {
		t.Error("expected non-nil row")
	}
}

// --- WithTx full paths ---

func TestWithTx_CommitOnSuccess(t *testing.T) {
	committed := false
	tx := &mockTx{
		commitFn: func(_ context.Context) error {
			committed = true
			return nil
		},
	}
	pool := &Pool{
		beginTx: func(_ context.Context) (pgx.Tx, error) {
			return tx, nil
		},
	}
	err := WithTx(context.Background(), pool, func(ctx context.Context) error {
		if TxFromContext(ctx, pool) == nil {
			t.Error("expected tx in context")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !committed {
		t.Error("expected commit")
	}
}

func TestWithTx_RollbackOnError(t *testing.T) {
	rolledBack := false
	tx := &mockTx{
		rollbackFn: func(_ context.Context) error {
			rolledBack = true
			return nil
		},
	}
	pool := &Pool{
		beginTx: func(_ context.Context) (pgx.Tx, error) {
			return tx, nil
		},
	}
	fnErr := fmt.Errorf("something failed")
	err := WithTx(context.Background(), pool, func(_ context.Context) error {
		return fnErr
	})
	if !errors.Is(err, fnErr) {
		t.Errorf("expected fn error, got %v", err)
	}
	if !rolledBack {
		t.Error("expected rollback")
	}
}

func TestWithTx_BeginError(t *testing.T) {
	pool := &Pool{
		beginTx: func(_ context.Context) (pgx.Tx, error) {
			return nil, fmt.Errorf("begin failed")
		},
	}
	err := WithTx(context.Background(), pool, func(_ context.Context) error {
		t.Fatal("fn should not be called")
		return nil
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestWithTx_CommitError(t *testing.T) {
	tx := &mockTx{
		commitFn: func(_ context.Context) error {
			return fmt.Errorf("commit failed")
		},
	}
	pool := &Pool{
		beginTx: func(_ context.Context) (pgx.Tx, error) {
			return tx, nil
		},
	}
	err := WithTx(context.Background(), pool, func(_ context.Context) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected commit error")
	}
}

func TestWithTx_RollbackError(t *testing.T) {
	tx := &mockTx{
		rollbackFn: func(_ context.Context) error {
			return fmt.Errorf("rollback failed")
		},
	}
	pool := &Pool{
		beginTx: func(_ context.Context) (pgx.Tx, error) {
			return tx, nil
		},
	}
	err := WithTx(context.Background(), pool, func(_ context.Context) error {
		return fmt.Errorf("fn error")
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- OrderBy without direction (covers quoteOrderByExpr plain identifier path) ---

func TestSelectOrderByNoDirection(t *testing.T) {
	q, _ := Select("users").OrderBy("name").Build()
	expect(t, q, `SELECT * FROM "users" ORDER BY "name"`)
}

// --- OrderBy DESC ---

func TestSelectOrderByDesc(t *testing.T) {
	q, _ := Select("users").OrderBy("name DESC").Build()
	expect(t, q, `SELECT * FROM "users" ORDER BY "name" DESC`)
}

// --- OrderBy expression passthrough ---

func TestSelectOrderByExpression(t *testing.T) {
	q, _ := Select("users").OrderBy("LOWER(name)").Build()
	expect(t, q, `SELECT * FROM "users" ORDER BY LOWER(name)`)
}

func TestOrderByInvalidWithDirection(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for invalid ORDER BY with direction")
		}
	}()
	Select("users").OrderBy("bad;col ASC")
}

func TestOrderByInvalidWithDescDirection(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for invalid ORDER BY with DESC")
		}
	}()
	Select("users").OrderBy("bad;col DESC")
}

// --- beginTransaction fallback path ---

func TestPool_beginTransaction_WithBeginTxFunc(t *testing.T) {
	called := false
	pool := &Pool{
		beginTx: func(_ context.Context) (pgx.Tx, error) {
			called = true
			return &mockTx{}, nil
		},
	}
	_, _ = pool.beginTransaction(context.Background())
	if !called {
		t.Error("expected beginTx to be called")
	}
}

func TestPool_beginTransaction_NilBeginTxFallback(t *testing.T) {
	pool := &Pool{} // beginTx is nil and the pool is not open → a db.connection error, never a panic
	tx, err := pool.beginTransaction(context.Background())
	if err == nil || !perrors.Is(err, CodeConnection) {
		t.Fatalf("beginTransaction on a pool that is not open = (%v, %v), want a %s error", tx, err, CodeConnection)
	}
}

// --- PGXPool ---

func TestPool_PGXPool_Nil(t *testing.T) {
	pool := &Pool{}
	if pool.PGXPool() != nil {
		t.Error("expected nil pgxpool")
	}
}

// --- Select with Limit 0 (edge case for appendLimit) ---

func TestSelectLimitZero(t *testing.T) {
	q, _ := Select("users").Limit(0).Build()
	expect(t, q, `SELECT * FROM "users" LIMIT 0`)
}

func TestSelectOffsetZero(t *testing.T) {
	q, _ := Select("users").Offset(0).Build()
	expect(t, q, `SELECT * FROM "users" OFFSET 0`)
}

// --- Insert without columns ---

func TestInsertWithoutColumns(t *testing.T) {
	q, args := Insert("users").Values("Alice").Build()
	expect(t, q, `INSERT INTO "users" VALUES ($1)`)
	expectArgs(t, args, "Alice")
}

// --- Delete without where ---

func TestDeleteWithoutWhere(t *testing.T) {
	q, _ := Delete("users").Build()
	expect(t, q, `DELETE FROM "users"`)
}

// --- Update with returning ---

func TestDeleteWithReturning(t *testing.T) {
	q, args := Delete("users").Where("id = $1", 1).Returning("id", "name").Build()
	expect(t, q, `DELETE FROM "users" WHERE id = $1 RETURNING "id", "name"`)
	expectArgs(t, args, 1)
}

// --- GroupBy with expression ---

func TestSelectGroupByExpression(t *testing.T) {
	q, _ := Select("orders").Columns("DATE(created_at)", "COUNT(*)").GroupBy("DATE(created_at)").Build()
	expect(t, q, `SELECT DATE(created_at), COUNT(*) FROM "orders" GROUP BY DATE(created_at)`)
}

// --- Column expression passthrough ---

func TestSelectColumnExpression(t *testing.T) {
	q, _ := Select("users").Columns("COUNT(*)").Build()
	expect(t, q, `SELECT COUNT(*) FROM "users"`)
}
