package database

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"go.putnami.dev/errors"
)

// txGate admits one operation at a time on the connection of a transaction a
// UnitOfWork begins. A pgx connection serves one query at a time and is not
// safe for concurrent use, while the goroutines serving one request all
// resolve the same transaction; the gate makes them take turns.
//
// Holding the token of turn is holding the connection. An operation takes it
// before it sends anything and gives it back once the connection is free
// again. A result that keeps the connection busy after its call returns (open
// rows, unread batch results) holds the token until it is closed, and is
// recorded in open so the boundary can close it.
type txGate struct {
	turn chan struct{}
	// datasource names the transaction's datasource in errors.
	datasource string

	mu sync.Mutex
	// open closes the result that holds the turn; nil while none does.
	open func()
	// closed is set once the transaction starts finishing: every later
	// operation is refused.
	closed bool
}

func newTxGate(datasource string) *txGate {
	return &txGate{turn: make(chan struct{}, 1), datasource: datasource}
}

// acquire takes the turn, waiting under ctx while another operation holds it.
// It fails when ctx ends first or when the transaction is finishing.
func (g *txGate) acquire(ctx context.Context) error {
	select {
	case g.turn <- struct{}{}:
	default:
		select {
		case g.turn <- struct{}{}:
		case <-ctx.Done():
			return errors.Wrapf(ctx.Err(), CodeTransaction,
				"wait for the unit of work transaction: another query of this request holds it until its result is closed",
				errors.String("datasource", g.datasource))
		}
	}
	g.mu.Lock()
	closed := g.closed
	g.mu.Unlock()
	if closed {
		<-g.turn
		return g.finishedError()
	}
	return nil
}

// release gives the turn back.
func (g *txGate) release() {
	g.mu.Lock()
	g.open = nil
	g.mu.Unlock()
	<-g.turn
}

// hold records closeResult as the result that keeps the turn after its call
// returned. It reports false when the transaction started finishing meanwhile:
// the caller then closes the result itself.
func (g *txGate) hold(closeResult func()) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.open = closeResult
	return true
}

// finish ends the transaction with end. It refuses every later operation,
// starts closing a result the request left open, and waits under ctx until
// that close or an operation still running frees the connection. When ctx
// ends first, a commit fails without running end and a rollback runs end
// anyway: pgx closes a connection whose rollback fails, so the busy connection
// never stays checked out.
func (g *txGate) finish(ctx context.Context, rollback bool, end func(context.Context) error) error {
	g.mu.Lock()
	g.closed = true
	open := g.open
	g.mu.Unlock()
	if open != nil {
		go open()
	}
	select {
	case g.turn <- struct{}{}:
	default:
		select {
		case g.turn <- struct{}{}:
		case <-ctx.Done():
			if rollback {
				return end(ctx)
			}
			return errors.Wrapf(ctx.Err(), CodeTransaction,
				"commit the unit of work transaction: an operation of this request still holds it",
				errors.String("datasource", g.datasource))
		}
	}
	defer func() { <-g.turn }()
	return end(ctx)
}

func (g *txGate) finishedError() error {
	return errors.Newf(CodeTransaction,
		"the unit of work transaction of datasource %q is finalized; a query must not outlive its request", g.datasource)
}

// gatedTx is the pgx.Tx a UnitOfWork hands out for a transaction it began:
// every operation goes through the transaction's txGate. root marks that
// transaction itself, whose Commit and Rollback finish the gate; a savepoint
// transaction from Begin shares the gate and ends as an ordinary operation.
//
// LargeObjects and Conn reach the connection directly and stay outside the
// gate.
type gatedTx struct {
	tx   pgx.Tx
	gate *txGate
	root bool
}

func newGatedTx(tx pgx.Tx, datasource string) *gatedTx {
	return &gatedTx{tx: tx, gate: newTxGate(datasource), root: true}
}

// Begin starts a savepoint transaction on the same connection and gate.
func (t *gatedTx) Begin(ctx context.Context) (pgx.Tx, error) {
	if err := t.gate.acquire(ctx); err != nil {
		return nil, err
	}
	defer t.gate.release()
	nested, err := t.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &gatedTx{tx: nested, gate: t.gate}, nil
}

// Commit commits a savepoint as an ordinary operation, and finishes the gate
// for the root transaction.
func (t *gatedTx) Commit(ctx context.Context) error {
	if t.root {
		return t.gate.finish(ctx, false, t.tx.Commit)
	}
	if err := t.gate.acquire(ctx); err != nil {
		return err
	}
	defer t.gate.release()
	return t.tx.Commit(ctx)
}

// Rollback rolls a savepoint back as an ordinary operation, and finishes the
// gate for the root transaction.
func (t *gatedTx) Rollback(ctx context.Context) error {
	if t.root {
		return t.gate.finish(ctx, true, t.tx.Rollback)
	}
	if err := t.gate.acquire(ctx); err != nil {
		return err
	}
	defer t.gate.release()
	return t.tx.Rollback(ctx)
}

// CopyFrom holds the transaction for the whole copy.
func (t *gatedTx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	if err := t.gate.acquire(ctx); err != nil {
		return 0, err
	}
	defer t.gate.release()
	return t.tx.CopyFrom(ctx, tableName, columnNames, rowSrc)
}

// SendBatch holds the transaction until the returned results are closed.
func (t *gatedTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	if err := t.gate.acquire(ctx); err != nil {
		return errBatchResults{err: err}
	}
	held := &gatedBatchResults{BatchResults: t.tx.SendBatch(ctx, b), gate: t.gate}
	if !t.gate.hold(held.closeOnce) {
		held.closeOnce()
		return errBatchResults{err: t.gate.finishedError()}
	}
	return held
}

// LargeObjects reaches the connection directly, outside the gate.
func (t *gatedTx) LargeObjects() pgx.LargeObjects { return t.tx.LargeObjects() }

// Prepare holds the transaction for the call.
func (t *gatedTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	if err := t.gate.acquire(ctx); err != nil {
		return nil, err
	}
	defer t.gate.release()
	return t.tx.Prepare(ctx, name, sql)
}

// Exec holds the transaction for the call.
func (t *gatedTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	if err := t.gate.acquire(ctx); err != nil {
		return pgconn.CommandTag{}, err
	}
	defer t.gate.release()
	return t.tx.Exec(ctx, sql, arguments...)
}

// Query holds the transaction until the returned rows are closed or Next
// reports no further row. On failure the returned rows carry the error, as
// pgx's do.
func (t *gatedTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := t.gate.acquire(ctx); err != nil {
		return errRows{err: err}, err
	}
	rows, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		t.gate.release()
		return rows, err
	}
	held := &gatedRows{Rows: rows, gate: t.gate}
	if !t.gate.hold(held.Close) {
		held.Close()
		err := t.gate.finishedError()
		return errRows{err: err}, err
	}
	return held, nil
}

// QueryRow sends its statement when the row is scanned and holds the
// transaction for that call, so an unscanned row never holds it.
func (t *gatedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &gatedRow{tx: t, ctx: ctx, sql: sql, args: args}
}

// Conn reaches the connection directly, outside the gate.
func (t *gatedTx) Conn() *pgx.Conn { return t.tx.Conn() }

// gatedRows gives the turn back once, when the rows close: on Close, when
// Next reports no further row, or when Scan or Values fails, since pgx closes
// rows on such an error.
type gatedRows struct {
	pgx.Rows
	gate *txGate
	once sync.Once
}

func (r *gatedRows) Close() {
	r.once.Do(func() {
		r.Rows.Close()
		r.gate.release()
	})
}

func (r *gatedRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.Close()
	return false
}

func (r *gatedRows) Scan(dest ...any) error {
	err := r.Rows.Scan(dest...)
	if err != nil {
		r.Close()
	}
	return err
}

func (r *gatedRows) Values() ([]any, error) {
	values, err := r.Rows.Values()
	if err != nil {
		r.Close()
	}
	return values, err
}

// gatedRow runs a QueryRow statement once, at its first Scan. A later Scan
// reports the first one's error, or pgx.ErrNoRows, as a pgx row does.
type gatedRow struct {
	tx      *gatedTx
	ctx     context.Context
	sql     string
	args    []any
	scanned bool
	err     error
}

func (r *gatedRow) Scan(dest ...any) error {
	if r.scanned {
		if r.err != nil {
			return r.err
		}
		return pgx.ErrNoRows
	}
	r.scanned = true
	if err := r.tx.gate.acquire(r.ctx); err != nil {
		r.err = err
		return err
	}
	defer r.tx.gate.release()
	r.err = r.tx.tx.QueryRow(r.ctx, r.sql, r.args...).Scan(dest...)
	return r.err
}

// gatedBatchResults gives the turn back once, when the results close.
type gatedBatchResults struct {
	pgx.BatchResults
	gate *txGate
	once sync.Once
	err  error
}

// Close closes the results and reports the error of the first close.
func (b *gatedBatchResults) Close() error {
	b.closeOnce()
	return b.err
}

func (b *gatedBatchResults) closeOnce() {
	b.once.Do(func() {
		b.err = b.BatchResults.Close()
		b.gate.release()
	})
}

// errRows is the closed pgx.Rows a gated Query returns when it cannot run: Err
// reports the error, Next reports no row and Scan fails with the error.
type errRows struct{ err error }

func (r errRows) Close()                                       {}
func (r errRows) Err() error                                   { return r.err }
func (r errRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r errRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r errRows) Next() bool                                   { return false }
func (r errRows) Scan(...any) error                            { return r.err }
func (r errRows) Values() ([]any, error)                       { return nil, r.err }
func (r errRows) RawValues() [][]byte                          { return nil }
func (r errRows) Conn() *pgx.Conn                              { return nil }
