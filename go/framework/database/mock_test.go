package database

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// --- Mock Row ---

type mockRow struct {
	scanFn func(dest ...any) error
}

func (r *mockRow) Scan(dest ...any) error {
	if r.scanFn != nil {
		return r.scanFn(dest...)
	}
	return nil
}

// --- Mock Rows ---

type mockRows struct {
	items  []func(dest ...any) error
	idx    int
	err    error
	closed bool
}

func (r *mockRows) Close()                                       { r.closed = true }
func (r *mockRows) Err() error                                   { return r.err }
func (r *mockRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 0") }
func (r *mockRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *mockRows) RawValues() [][]byte                          { return nil }
func (r *mockRows) Values() ([]any, error)                       { return nil, nil }
func (r *mockRows) Conn() *pgx.Conn                              { return nil }

func (r *mockRows) Next() bool {
	return r.idx < len(r.items)
}

func (r *mockRows) Scan(dest ...any) error {
	if r.idx < len(r.items) {
		fn := r.items[r.idx]
		r.idx++
		return fn(dest...)
	}
	return fmt.Errorf("no more rows")
}

// --- Mock Querier ---

type mockQuerier struct {
	execFn     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	queryFn    func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row
}

func (q *mockQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if q.execFn != nil {
		return q.execFn(ctx, sql, args...)
	}
	return pgconn.NewCommandTag(""), nil
}

func (q *mockQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if q.queryFn != nil {
		return q.queryFn(ctx, sql, args...)
	}
	return &mockRows{}, nil
}

func (q *mockQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if q.queryRowFn != nil {
		return q.queryRowFn(ctx, sql, args...)
	}
	return &mockRow{}
}

// --- Mock Tx (implements pgx.Tx) ---

type mockTx struct {
	mockQuerier
	commitFn   func(ctx context.Context) error
	rollbackFn func(ctx context.Context) error
}

func (t *mockTx) Begin(ctx context.Context) (pgx.Tx, error) { return t, nil }
func (t *mockTx) Commit(ctx context.Context) error {
	if t.commitFn != nil {
		return t.commitFn(ctx)
	}
	return nil
}
func (t *mockTx) Rollback(ctx context.Context) error {
	if t.rollbackFn != nil {
		return t.rollbackFn(ctx)
	}
	return nil
}
func (t *mockTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (t *mockTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults { return nil }
func (t *mockTx) LargeObjects() pgx.LargeObjects                             { return pgx.LargeObjects{} }
func (t *mockTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (t *mockTx) Conn() *pgx.Conn { return nil }

// ctxWithQuerier creates a context carrying a mock tx bound to pool, so that
// pool.Querier returns it. The tx is bound to the specific pool because the
// transaction context is now per-pool keyed — a foreign pool would not see it.
func ctxWithQuerier(pool *Pool, q *mockQuerier) context.Context {
	tx := &mockTx{mockQuerier: *q}
	return contextWithTx(context.Background(), pool, tx)
}
