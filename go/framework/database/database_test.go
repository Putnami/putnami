package database

import (
	"testing"
)

// --- Query builder tests (no database needed) ---

func TestSelectBasic(t *testing.T) {
	q, args := Select("users").Build()
	expect(t, q, `SELECT * FROM "users"`)
	if len(args) != 0 {
		t.Fatalf("expected 0 args, got %d", len(args))
	}
}

func TestSelectColumns(t *testing.T) {
	q, _ := Select("users").Columns("id", "name", "email").Build()
	expect(t, q, `SELECT "id", "name", "email" FROM "users"`)
}

func TestSelectWhere(t *testing.T) {
	q, args := Select("users").
		Where("age > $1", 18).
		Build()
	expect(t, q, `SELECT * FROM "users" WHERE age > $1`)
	expectArgs(t, args, 18)
}

func TestSelectWhereMultiple(t *testing.T) {
	q, args := Select("users").
		Where("age > $1", 18).
		Where("active = $1", true).
		Build()
	expect(t, q, `SELECT * FROM "users" WHERE age > $1 AND active = $2`)
	expectArgs(t, args, 18, true)
}

func TestSelectOrderByLimitOffset(t *testing.T) {
	q, _ := Select("users").
		OrderBy("name ASC").
		Limit(10).
		Offset(20).
		Build()
	expect(t, q, `SELECT * FROM "users" ORDER BY "name" ASC LIMIT 10 OFFSET 20`)
}

func TestSelectGroupByHaving(t *testing.T) {
	q, args := Select("orders").
		Columns("status", "COUNT(*) as count").
		GroupBy("status").
		Having("COUNT(*) > $1", 5).
		Where("created_at > $1", "2024-01-01").
		Build()
	expect(t, q, `SELECT "status", COUNT(*) as count FROM "orders" WHERE created_at > $2 GROUP BY "status" HAVING COUNT(*) > $1`)
	expectArgs(t, args, 5, "2024-01-01")
}

func TestInsertSingleRow(t *testing.T) {
	q, args := Insert("users").
		Columns("name", "email").
		Values("Alice", "alice@example.com").
		Build()
	expect(t, q, `INSERT INTO "users" ("name", "email") VALUES ($1, $2)`)
	expectArgs(t, args, "Alice", "alice@example.com")
}

func TestInsertMultipleRows(t *testing.T) {
	q, args := Insert("users").
		Columns("name", "email").
		Values("Alice", "alice@example.com").
		Values("Bob", "bob@example.com").
		Build()
	expect(t, q, `INSERT INTO "users" ("name", "email") VALUES ($1, $2), ($3, $4)`)
	expectArgs(t, args, "Alice", "alice@example.com", "Bob", "bob@example.com")
}

func TestInsertReturning(t *testing.T) {
	q, args := Insert("users").
		Columns("name").
		Values("Alice").
		Returning("id", "created_at").
		Build()
	expect(t, q, `INSERT INTO "users" ("name") VALUES ($1) RETURNING "id", "created_at"`)
	expectArgs(t, args, "Alice")
}

// TestInsertBuildIsIdempotent guards against buildInsert mutating the
// builder's args: two consecutive Build calls on the same INSERT builder
// must return identical SQL and identical (non-duplicated) args.
func TestInsertBuildIsIdempotent(t *testing.T) {
	b := Insert("users").Columns("name", "email").Values("Alice", "alice@example.com")
	sql1, args1 := b.Build()
	sql2, args2 := b.Build()

	expect(t, sql2, sql1)
	expectArgs(t, args1, "Alice", "alice@example.com")
	expectArgs(t, args2, "Alice", "alice@example.com")
}

func TestUpdateBasic(t *testing.T) {
	q, args := Update("users").
		Set("name = $1", "Alice").
		Where("id = $1", 42).
		Build()
	expect(t, q, `UPDATE "users" SET name = $1 WHERE id = $2`)
	expectArgs(t, args, "Alice", 42)
}

func TestUpdateMultipleSets(t *testing.T) {
	q, args := Update("users").
		Set("name = $1", "Alice").
		Set("age = $1", 30).
		Where("id = $1", 42).
		Build()
	expect(t, q, `UPDATE "users" SET name = $1, age = $2 WHERE id = $3`)
	expectArgs(t, args, "Alice", 30, 42)
}

func TestUpdateReturning(t *testing.T) {
	q, args := Update("users").
		Set("name = $1", "Alice").
		Where("id = $1", 1).
		Returning("*").
		Build()
	expect(t, q, `UPDATE "users" SET name = $1 WHERE id = $2 RETURNING *`)
	expectArgs(t, args, "Alice", 1)
}

func TestDeleteBasic(t *testing.T) {
	q, args := Delete("users").
		Where("id = $1", 42).
		Build()
	expect(t, q, `DELETE FROM "users" WHERE id = $1`)
	expectArgs(t, args, 42)
}

func TestDeleteReturning(t *testing.T) {
	q, args := Delete("users").
		Where("active = $1", false).
		Returning("id").
		Build()
	expect(t, q, `DELETE FROM "users" WHERE active = $1 RETURNING "id"`)
	expectArgs(t, args, false)
}

func TestColumnsInvalidIdentifier(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for invalid column name")
		}
	}()
	Select("users").Columns("id; DROP TABLE users")
}

func TestOrderByInvalidIdentifier(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for invalid ORDER BY")
		}
	}()
	Select("users").OrderBy("id; DROP TABLE users")
}

func TestQueryBuilderInvalidTableName(t *testing.T) {
	cases := []struct {
		name string
		fn   func()
	}{
		{"Select", func() { Select("users; DROP TABLE users") }},
		{"Insert", func() { Insert("bad table") }},
		{"Update", func() { Update("") }},
		{"Delete", func() { Delete("x;y") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic for invalid table name")
				}
			}()
			tc.fn()
		})
	}
}

func TestRewriteParams(t *testing.T) {
	tests := []struct {
		input    string
		offset   int
		expected string
	}{
		{"$1", 0, "$1"},
		{"$1", 2, "$3"},
		{"$1 AND $2", 3, "$4 AND $5"},
		{"name = $1", 5, "name = $6"},
		{"$10", 0, "$10"},
		{"$10", 5, "$15"},
		{"no params here", 3, "no params here"},
	}

	for _, tt := range tests {
		result := rewriteParams(tt.input, tt.offset)
		if result != tt.expected {
			t.Errorf("rewriteParams(%q, %d) = %q, want %q", tt.input, tt.offset, result, tt.expected)
		}
	}
}

// --- PoolConfig defaults ---

func TestPoolConfigDefaults(t *testing.T) {
	cfg := PoolConfig{DSN: "postgres://localhost/test"}
	cfg = cfg.withDefaults()

	if cfg.MaxConns != 10 {
		t.Errorf("MaxConns = %d, want 10", cfg.MaxConns)
	}
	if cfg.MinConns != 2 {
		t.Errorf("MinConns = %d, want 2", cfg.MinConns)
	}
	if cfg.MaxConnLifetime == 0 {
		t.Error("MaxConnLifetime should not be zero")
	}
	if cfg.MaxConnIdleTime == 0 {
		t.Error("MaxConnIdleTime should not be zero")
	}
	if cfg.HealthCheckPeriod == 0 {
		t.Error("HealthCheckPeriod should not be zero")
	}
}

func TestPoolConfigPreservesCustomValues(t *testing.T) {
	cfg := PoolConfig{DSN: "postgres://localhost/test", MaxConns: 50}
	cfg = cfg.withDefaults()

	if cfg.MaxConns != 50 {
		t.Errorf("MaxConns = %d, want 50 (custom value)", cfg.MaxConns)
	}
}

// --- Transaction context ---

func TestTxFromContextNil(t *testing.T) {
	tx := TxFromContext(t.Context(), &Pool{})
	if tx != nil {
		t.Error("expected nil tx from empty context")
	}
}

// --- Helpers ---

// The query builder examples of go/doc/framework/07-persistence.md, with the
// output their comments show.
func TestPersistenceGuideBuilderExamples(t *testing.T) {
	q, args := Select("users").
		Columns("id", "name", "email").
		Where("age > $1", 18).
		Where("active = $1", true).
		OrderBy("name ASC").
		Limit(10).
		Offset(20).
		Build()
	expect(t, q, `SELECT "id", "name", "email" FROM "users" WHERE age > $1 AND active = $2 ORDER BY "name" ASC LIMIT 10 OFFSET 20`)
	expectArgs(t, args, 18, true)

	q, _ = Insert("users").
		Columns("id", "name", "email").
		Values("user-123", "Jane", "jane@example.com").
		Returning("id", "created_at").
		Build()
	expect(t, q, `INSERT INTO "users" ("id", "name", "email") VALUES ($1, $2, $3) RETURNING "id", "created_at"`)

	q, args = Update("users").
		Set("name = $1", "Jane Doe").
		Set("updated_at = $1", "now").
		Where("id = $1", "user-123").
		Build()
	expect(t, q, `UPDATE "users" SET name = $1, updated_at = $2 WHERE id = $3`)
	expectArgs(t, args, "Jane Doe", "now", "user-123")

	q, _ = Delete("users").
		Where("active = $1", false).
		Returning("id").
		Build()
	expect(t, q, `DELETE FROM "users" WHERE active = $1 RETURNING "id"`)
}

func expect(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("query mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

func expectArgs(t *testing.T, got []any, want ...any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("args count: got %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("arg[%d]: got %v, want %v", i, got[i], want[i])
		}
	}
}
