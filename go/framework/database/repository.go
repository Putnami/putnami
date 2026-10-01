package database

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"go.putnami.dev/errors"
)

// identifierRe matches valid SQL identifiers: letters, digits, underscores,
// and optionally schema-qualified (e.g. "public.users").
var identifierRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z_][a-zA-Z0-9_]*)?$`)

// validIdentifier returns true if s is a safe SQL identifier.
func validIdentifier(s string) bool {
	return s != "" && identifierRe.MatchString(s)
}

// quoteIdentifier wraps each part of a possibly schema-qualified identifier in
// double quotes (PostgreSQL quoted identifier syntax).
func quoteIdentifier(s string) string {
	parts := strings.SplitN(s, ".", 2)
	for i, p := range parts {
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(parts, ".")
}

// Repository provides generic CRUD operations for a database table.
// T is the entity type. The entity must be scannable via pgx row scanning.
//
//	type UserRepo struct {
//	    *database.Repository[User]
//	}
//
//	func NewUserRepo(pool *database.Pool) *UserRepo {
//	    return &UserRepo{
//	        Repository: database.NewRepository[User](pool, "users", scanUser),
//	    }
//	}
type Repository[T any] struct {
	pool     *Pool
	table    string // quoted, injection-safe table identifier used in raw SQL
	rawTable string // original unquoted table name, for the query builder (Insert/Update)
	idColumn string
	scanRow  func(pgx.Row) (T, error)

	// Precomputed query prefixes (static per repository instance).
	findByIDQuery   string // SELECT * FROM <table> WHERE <idColumn> = $1
	selectAllPrefix string // SELECT * FROM <table>
	countAllQuery   string // SELECT COUNT(*) FROM <table>
	deletePrefix    string // DELETE FROM <table> WHERE
}

// RepositoryOption configures a Repository.
type RepositoryOption func(*repositoryConfig)

type repositoryConfig struct {
	idColumn string
}

// WithIDColumn sets the primary key column name used by FindByID and DeleteByID.
// Defaults to "id" if not specified.
func WithIDColumn(column string) RepositoryOption {
	return func(c *repositoryConfig) { c.idColumn = column }
}

// NewRepository creates a new generic repository.
//
// Parameters:
//   - pool: the connection pool
//   - table: the database table name
//   - scanRow: a function that scans a single row into T
//   - opts: optional configuration (e.g., WithIDColumn)
func NewRepository[T any](pool *Pool, table string, scanRow func(pgx.Row) (T, error), opts ...RepositoryOption) *Repository[T] {
	if !validIdentifier(table) {
		panic(fmt.Sprintf("database: invalid table name: %q", table))
	}
	cfg := repositoryConfig{idColumn: "id"}
	for _, opt := range opts {
		opt(&cfg)
	}
	if !validIdentifier(cfg.idColumn) {
		panic(fmt.Sprintf("database: invalid id column name: %q", cfg.idColumn))
	}
	quotedTable := quoteIdentifier(table)
	quotedID := quoteIdentifier(cfg.idColumn)
	return &Repository[T]{
		pool:            pool,
		table:           quotedTable,
		rawTable:        table,
		idColumn:        cfg.idColumn,
		scanRow:         scanRow,
		findByIDQuery:   "SELECT * FROM " + quotedTable + " WHERE " + quotedID + " = $1",
		selectAllPrefix: "SELECT * FROM " + quotedTable,
		countAllQuery:   "SELECT COUNT(*) FROM " + quotedTable,
		deletePrefix:    "DELETE FROM " + quotedTable + " WHERE ",
	}
}

// Pool returns the underlying connection pool.
func (r *Repository[T]) Pool() *Pool {
	return r.pool
}

// Table returns the table name.
func (r *Repository[T]) Table() string {
	return r.table
}

// FindByID retrieves a single entity by its primary key column.
// If idColumn is empty, the column configured via WithIDColumn (default "id") is used.
//
// Panics if idColumn is a non-empty invalid SQL identifier — a column
// name is a developer-supplied constant, never user input, so a bad one
// is a programmer error. This mirrors NewRepository and the query
// builder, which also panic on invalid identifiers.
func (r *Repository[T]) FindByID(ctx context.Context, idColumn string, id any) (T, error) {
	var query string
	if idColumn == "" || idColumn == r.idColumn {
		query = r.findByIDQuery
	} else {
		if !validIdentifier(idColumn) {
			panic(fmt.Sprintf("database.FindByID: invalid column name: %q", idColumn))
		}
		query = "SELECT * FROM " + r.table + " WHERE " + quoteIdentifier(idColumn) + " = $1"
	}
	row := r.pool.QueryRow(ctx, query, id)
	return r.scanRow(row)
}

// defaultFindAllLimit is the maximum number of rows returned by FindAll
// to prevent unbounded queries from loading entire tables into memory.
const defaultFindAllLimit = 1000

// FindAll retrieves entities from the table with a default safety limit of
// 1000 rows. Use FindAllPaginated for explicit control over pagination.
func (r *Repository[T]) FindAll(ctx context.Context) ([]T, error) {
	return r.FindAllPaginated(ctx, defaultFindAllLimit, 0)
}

// FindAllPaginated retrieves entities with explicit limit and offset.
func (r *Repository[T]) FindAllPaginated(ctx context.Context, limit, offset int) ([]T, error) {
	query := r.selectAllPrefix + fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)
	rows, err := r.pool.Query(ctx, query)
	if err != nil {
		return nil, wrapQueryError(ctx, err, "find all", errors.String("table", r.table))
	}
	defer rows.Close()

	return r.collectRows(ctx, rows)
}

// FindWhere retrieves entities matching a WHERE clause.
//
//	users, err := repo.FindWhere(ctx, "age > $1 AND active = $2", 18, true)
func (r *Repository[T]) FindWhere(ctx context.Context, where string, args ...any) ([]T, error) {
	query := r.selectAllPrefix + " WHERE " + where
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, wrapQueryError(ctx, err, "find where", errors.String("table", r.table))
	}
	defer rows.Close()

	return r.collectRows(ctx, rows)
}

// FindOneWhere retrieves a single entity matching a WHERE clause.
func (r *Repository[T]) FindOneWhere(ctx context.Context, where string, args ...any) (T, error) {
	query := r.selectAllPrefix + " WHERE " + where + " LIMIT 1"
	row := r.pool.QueryRow(ctx, query, args...)
	return r.scanRow(row)
}

// Count returns the number of rows in the table, optionally with a WHERE clause.
func (r *Repository[T]) Count(ctx context.Context, where string, args ...any) (int64, error) {
	var query string
	if where == "" {
		query = r.countAllQuery
	} else {
		query = r.countAllQuery + " WHERE " + where
	}
	var count int64
	err := r.pool.QueryRow(ctx, query, args...).Scan(&count)
	return count, err
}

// Exists returns true if at least one row matches the WHERE clause.
func (r *Repository[T]) Exists(ctx context.Context, where string, args ...any) (bool, error) {
	query := "SELECT EXISTS(SELECT 1 FROM " + r.table + " WHERE " + where + ")"
	var exists bool
	err := r.pool.QueryRow(ctx, query, args...).Scan(&exists)
	return exists, err
}

// DeleteWhere deletes rows matching a WHERE clause. Returns the number of rows deleted.
func (r *Repository[T]) DeleteWhere(ctx context.Context, where string, args ...any) (int64, error) {
	query := r.deletePrefix + where
	tag, err := r.pool.Exec(ctx, query, args...)
	if err != nil {
		return 0, wrapQueryError(ctx, err, "delete", errors.String("table", r.table))
	}
	return tag.RowsAffected(), nil
}

// DeleteByID deletes a single entity by its primary key.
// If idColumn is empty, the column configured via WithIDColumn (default "id") is used.
//
// Panics if idColumn is a non-empty invalid SQL identifier, for the same
// reason as FindByID — invalid identifiers are programmer errors and are
// handled uniformly across the package by panicking.
func (r *Repository[T]) DeleteByID(ctx context.Context, idColumn string, id any) error {
	if idColumn == "" || idColumn == r.idColumn {
		_, err := r.DeleteWhere(ctx, quoteIdentifier(r.idColumn)+" = $1", id)
		return err
	}
	if !validIdentifier(idColumn) {
		panic(fmt.Sprintf("database.DeleteByID: invalid column name: %q", idColumn))
	}
	_, err := r.DeleteWhere(ctx, quoteIdentifier(idColumn)+" = $1", id)
	return err
}

// Query executes a raw query with the given arguments, using the repository's
// scan function to collect results.
func (r *Repository[T]) Query(ctx context.Context, query string, args ...any) ([]T, error) {
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, wrapQueryError(ctx, err, "query", errors.String("table", r.table))
	}
	defer rows.Close()

	return r.collectRows(ctx, rows)
}

// QueryOne executes a raw query expected to return a single row.
func (r *Repository[T]) QueryOne(ctx context.Context, query string, args ...any) (T, error) {
	row := r.pool.QueryRow(ctx, query, args...)
	result, err := r.scanRow(row)
	if err != nil {
		return result, wrapQueryError(ctx, err, "query one", errors.String("table", r.table))
	}
	return result, nil
}

func (r *Repository[T]) collectRows(ctx context.Context, rows pgx.Rows) ([]T, error) {
	results := make([]T, 0)
	for rows.Next() {
		item, err := r.scanRow(rowAdapter{rows})
		if err != nil {
			return nil, wrapQueryError(ctx, err, "scan row", errors.String("table", r.table))
		}
		results = append(results, item)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapQueryError(ctx, err, "iterate rows", errors.String("table", r.table))
	}
	return results, nil
}

// wrapQueryError wraps a database error with context timeout information.
// If the context has timed out or been canceled, that information is included
// in the error attributes for observability.
func wrapQueryError(ctx context.Context, err error, msg string, attrs ...errors.Attr) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		attrs = append(attrs, errors.String("context", ctxErr.Error()))
	}
	return errors.Wrapf(err, CodeQuery, msg, attrs...)
}

// rowAdapter wraps pgx.Rows to implement pgx.Row (single-row scanning).
type rowAdapter struct {
	pgx.Rows
}

func (r rowAdapter) Scan(dest ...any) error {
	return r.Rows.Scan(dest...)
}
