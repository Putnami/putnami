package database

import (
	"reflect"
	"testing"
	"testing/fstest"

	"go.putnami.dev/migration"
	"go.putnami.dev/protocol/infra"
)

func TestSQLSource_ImplementsMigrationSource(t *testing.T) {
	var _ migration.Source = SQLSource{}
	var _ migration.SchemaContributor = SQLSource{}
}

func TestSQLSource_AccessorsAndDefaults(t *testing.T) {
	fsys := fstest.MapFS{}
	src := NewSQLSource("iam", Datasource{Name: "primary", Schema: "identity"}, fsys,
		Definition{Name: "001", SQL: "X"},
	)
	if src.Kind() != migration.KindSQL {
		t.Errorf("Kind = %s, want sql", src.Kind())
	}
	if src.Namespace() != "iam" {
		t.Errorf("Namespace = %s", src.Namespace())
	}
	if src.Datasource() != "primary" {
		t.Errorf("Datasource = %s", src.Datasource())
	}
	if src.Schema() != "identity" {
		t.Errorf("Schema = %s, want identity", src.Schema())
	}
	if src.FS() == nil {
		t.Error("FS should not be nil")
	}
	if got := src.Inline(); len(got) != 1 {
		t.Errorf("Inline len = %d, want 1", len(got))
	}
}

func TestSQLSource_EmptyDatasourceResolvesToDefault(t *testing.T) {
	src := NewSQLSource("iam", Datasource{}, nil)
	if src.Datasource() != "default" {
		t.Errorf("empty datasource should resolve to default, got %q", src.Datasource())
	}
	if src.Schema() != "" {
		t.Errorf("unset schema should stay empty, got %q", src.Schema())
	}
}

func TestSQLSource_InlineIsCopied(t *testing.T) {
	defs := []Definition{{Name: "001", SQL: "X"}}
	src := NewSQLSource("iam", Datasource{Name: "default"}, nil, defs...)
	defs[0].Name = "MUTATED"
	if src.Inline()[0].Name != "001" {
		t.Fatal("inline definitions must be copied by NewSQLSource")
	}
}

func TestSQLSource_InfraDatabasesUsesDeclaredSchema(t *testing.T) {
	// The schema is the declared one, NOT the namespace — that decoupling is
	// the whole point: "iam" is identity, "identity" is the schema the SQL owns.
	src := NewSQLSource("iam", Datasource{Name: "primary", Schema: "identity"}, nil)
	got := src.InfraDatabases()
	want := []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"identity"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("InfraDatabases() = %+v, want %+v", got, want)
	}
}

func TestSQLSource_InfraDatabasesOmitsEmptySchema(t *testing.T) {
	src := NewSQLSource("iam", Datasource{Name: "primary"}, nil)
	got := src.InfraDatabases()
	if len(got) != 1 || got[0].Name != "primary" || len(got[0].Schemas) != 0 {
		t.Errorf("InfraDatabases() = %+v, want one primary entry with no schemas", got)
	}
}

func TestSQLSource_WithSchema(t *testing.T) {
	src := NewSQLSource("iam", Datasource{Name: "primary", Schema: "identity"}, nil,
		Definition{Name: "001", SQL: "X"},
	)
	// Re-target the schema; everything else (datasource, namespace, inline) is
	// preserved, and the original is untouched (value receiver).
	retargeted := src.WithSchema("  tenant_b  ")
	if retargeted.Schema() != "tenant_b" {
		t.Errorf("WithSchema schema = %q, want trimmed tenant_b", retargeted.Schema())
	}
	if retargeted.Datasource() != "primary" || retargeted.Namespace() != "iam" || len(retargeted.Inline()) != 1 {
		t.Errorf("WithSchema must preserve datasource/namespace/inline, got %+v", retargeted)
	}
	if src.Schema() != "identity" {
		t.Errorf("WithSchema must not mutate the receiver, got %q", src.Schema())
	}
	// The neutralizing form the test provider uses.
	if src.WithSchema("").Schema() != "" {
		t.Error(`WithSchema("") must clear the schema`)
	}
}
