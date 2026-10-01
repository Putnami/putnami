package database

import (
	"context"
	"testing"
)

func TestValidIdentifier(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"users", true},
		{"user_roles", true},
		{"_private", true},
		{"public.users", true},
		{"schema_1.table_2", true},
		{"Users", true},

		// Invalid
		{"", false},
		{"1table", false},
		{"table name", false},
		{"table;DROP", false},
		{"table--", false},
		{"table'", false},
		{`table"`, false},
		{"table$1", false},
		{"a.b.c", false},
		{".leading_dot", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := validIdentifier(tt.input)
			if got != tt.want {
				t.Errorf("validIdentifier(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestQuoteIdentifier(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"users", `"users"`},
		{"public.users", `"public"."users"`},
		{"user_roles", `"user_roles"`},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := quoteIdentifier(tt.input)
			if got != tt.want {
				t.Errorf("quoteIdentifier(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNewRepositoryPanicsOnInvalidTable(t *testing.T) {
	tests := []string{
		"",
		"table;DROP TABLE users",
		"table' OR '1'='1",
		"1invalid",
	}

	for _, table := range tests {
		t.Run(table, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("NewRepository(%q) did not panic", table)
				}
			}()
			NewRepository[struct{}](nil, table, nil)
		})
	}
}

func TestNewRepositoryAcceptsValidTable(t *testing.T) {
	tests := []string{
		"users",
		"public.users",
		"user_roles",
		"_internal",
	}

	for _, table := range tests {
		t.Run(table, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("NewRepository(%q) panicked: %v", table, r)
				}
			}()
			repo := NewRepository[struct{}](nil, table, nil)
			// Table should be quoted
			if repo.Table() == table && len(table) > 0 {
				t.Errorf("expected table to be quoted, got %q", repo.Table())
			}
		})
	}
}

func TestQuoteIdentifier_EscapesDoubleQuotes(t *testing.T) {
	// Identifiers containing double quotes should have them escaped
	got := quoteIdentifier(`col"name`)
	want := `"col""name"`
	if got != want {
		t.Errorf("quoteIdentifier(%q) = %q, want %q", `col"name`, got, want)
	}
}

func TestRepository_Pool(t *testing.T) {
	repo := NewRepository[struct{}](nil, "users", nil)
	if repo.Pool() != nil {
		t.Error("expected nil pool")
	}
}

func TestRepository_FindByID_InvalidColumn(t *testing.T) {
	repo := NewRepository[struct{}](nil, "users", nil)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for invalid column name")
		}
	}()
	_, _ = repo.FindByID(context.Background(), "invalid;column", "123")
}

func TestRepository_DeleteByID_InvalidColumn(t *testing.T) {
	repo := NewRepository[struct{}](nil, "users", nil)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for invalid column name")
		}
	}()
	_ = repo.DeleteByID(context.Background(), "invalid;column", "123")
}
