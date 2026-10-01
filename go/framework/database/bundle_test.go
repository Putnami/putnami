package database

import (
	"testing"

	"go.putnami.dev/migration"
	protocolmigration "go.putnami.dev/protocol/migration"
)

func TestSQLRunner_MigrationBundleOperations(t *testing.T) {
	reg := migration.NewRegistry()
	src := NewSQLSource("iam", Datasource{}, nil,
		Definition{Name: "0001_create_users", SQL: "CREATE TABLE iam.users (id uuid primary key);", Down: "DROP TABLE iam.users;"},
		Definition{Name: "0002_add_email", SQL: "ALTER TABLE iam.users ADD COLUMN email text;"},
	)
	if err := reg.AddSource(src); err != nil {
		t.Fatal(err)
	}
	runner := newSQLRunner(SQLRunnerOptions{Registry: reg})

	ops, payloads, err := runner.MigrationBundleOperations()
	if err != nil {
		t.Fatalf("MigrationBundleOperations: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("expected 2 operations, got %d", len(ops))
	}

	// First op: reversible, transactional, namespaced name, default target.
	create := ops[0]
	if create.Name != "iam/0001_create_users" {
		t.Errorf("name = %q", create.Name)
	}
	if create.Target != protocolmigration.DefaultDatasource {
		t.Errorf("target = %q, want %q", create.Target, protocolmigration.DefaultDatasource)
	}
	if create.Up.Path != "payload/sql/default/iam/0001_create_users.up.sql" {
		t.Errorf("up path = %q", create.Up.Path)
	}
	if create.Up.Hash != sha256Hex("CREATE TABLE iam.users (id uuid primary key);") {
		t.Error("up hash must match the runner's sha256Hex of the SQL")
	}
	if create.Down == nil || create.Down.Path != "payload/sql/default/iam/0001_create_users.down.sql" {
		t.Errorf("down path = %v", create.Down)
	}
	if !create.Capabilities.Transactional || !create.Capabilities.Reversible {
		t.Errorf("capabilities = %+v, want transactional+reversible", create.Capabilities)
	}

	// Second op: no down payload, not reversible.
	addEmail := ops[1]
	if addEmail.Down != nil {
		t.Error("migration without down SQL must not have a down payload")
	}
	if addEmail.Capabilities.Reversible {
		t.Error("migration without down SQL must not be reversible")
	}

	// The contributed operations + payloads assemble into a valid bundle.
	bundle := protocolmigration.Bundle{AppName: "demo", Operations: ops}
	root := t.TempDir()
	if err := protocolmigration.WriteBundle(root, bundle, payloads); err != nil {
		t.Fatalf("contributed bundle must be writable/valid: %v", err)
	}
}

func TestSQLRunner_MigrationBundleOperations_EmptyRegistry(t *testing.T) {
	runner := newSQLRunner(SQLRunnerOptions{Registry: migration.NewRegistry()})
	ops, payloads, err := runner.MigrationBundleOperations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 0 || len(payloads) != 0 {
		t.Fatalf("empty registry must contribute nothing, got %d ops / %d payloads", len(ops), len(payloads))
	}
}
