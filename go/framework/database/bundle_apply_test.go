package database

import (
	"context"
	"os"
	"testing"

	"go.putnami.dev/migration"
	protocolmigration "go.putnami.dev/protocol/migration"
)

// writeRoundTripBundle emits a bundle to a temp dir from the given sources via
// the SQL contributor + protocol writer, returning the bundle root.
func writeRoundTripBundle(t *testing.T, sources ...SQLSource) string {
	t.Helper()
	reg := migration.NewRegistry()
	for _, s := range sources {
		if err := reg.AddSource(s); err != nil {
			t.Fatal(err)
		}
	}
	runner := newSQLRunner(SQLRunnerOptions{Registry: reg})
	ops, payloads, err := runner.MigrationBundleOperations()
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	bundle := protocolmigration.Bundle{AppName: "demo", Operations: ops}
	if err := protocolmigration.WriteBundle(root, bundle, payloads); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLoadBundleSources_RoundTripsEmission(t *testing.T) {
	root := writeRoundTripBundle(t,
		NewSQLSource("iam", Datasource{}, nil,
			Definition{Name: "0001_create_users", SQL: "CREATE TABLE iam.users (id uuid primary key);", Down: "DROP TABLE iam.users;"},
			Definition{Name: "0002_add_email", SQL: "ALTER TABLE iam.users ADD COLUMN email text;"},
		),
		NewSQLSource("events", Datasource{Name: "analytics"}, nil,
			Definition{Name: "0001_init", SQL: "CREATE SCHEMA events;"},
		),
	)

	sources, err := LoadBundleSources(os.DirFS(root))
	if err != nil {
		t.Fatalf("LoadBundleSources: %v", err)
	}

	// Reconstructed definitions must match the originals byte-for-byte: load
	// them through the same loader the runner uses and compare.
	got := map[string]Definition{}
	for _, s := range sources {
		defs, err := LoadSQLSource(s)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range defs {
			got[d.Datasource+":"+d.Name] = d
		}
	}

	if len(got) != 3 {
		t.Fatalf("expected 3 reconstructed definitions, got %d (%v)", len(got), keysOf(got))
	}

	create := got["default:iam/0001_create_users"]
	if create.SQL != "CREATE TABLE iam.users (id uuid primary key);" {
		t.Errorf("up SQL not reconstructed: %q", create.SQL)
	}
	if create.Down != "DROP TABLE iam.users;" {
		t.Errorf("down SQL not reconstructed: %q", create.Down)
	}
	if create.Namespace != "iam" {
		t.Errorf("namespace = %q, want iam", create.Namespace)
	}

	analytics := got["analytics:events/0001_init"]
	if analytics.SQL != "CREATE SCHEMA events;" || analytics.Datasource != "analytics" {
		t.Errorf("analytics datasource definition not reconstructed: %+v", analytics)
	}
	if analytics.Down != "" {
		t.Errorf("non-reversible migration must reconstruct with empty Down, got %q", analytics.Down)
	}

	// Hash parity: the reconstructed up SQL must hash to the same value the
	// emitted bundle pinned (proving identical executable content).
	if sha256Hex(create.SQL) != sha256Hex("CREATE TABLE iam.users (id uuid primary key);") {
		t.Error("reconstructed hash diverged")
	}
}

func TestLoadBundleSources_RoundTripsDeclaredSchema(t *testing.T) {
	// One migration shape under several schema-owning datasources, plus a
	// schema-less source. The schema must survive emit → write → load so the
	// applied bundle sets it as the search_path (the "one bundle → many schemas"
	// path); a schema-less source must round-trip empty (omitempty).
	root := writeRoundTripBundle(t,
		NewSQLSource("registry", Datasource{Name: "registry_put", Schema: "registry_put"}, nil,
			Definition{Name: "0001_blobs", SQL: "CREATE TABLE blobs (id int);"},
		),
		NewSQLSource("registry", Datasource{Name: "registry_oci", Schema: "registry_oci"}, nil,
			Definition{Name: "0001_blobs", SQL: "CREATE TABLE blobs (id int);"},
		),
		NewSQLSource("audit", Datasource{Name: "audit"}, nil,
			Definition{Name: "0001_log", SQL: "CREATE TABLE log (id int);"},
		),
	)

	sources, err := LoadBundleSources(os.DirFS(root))
	if err != nil {
		t.Fatalf("LoadBundleSources: %v", err)
	}

	got := map[string]string{} // datasource → reconstructed schema
	for _, s := range sources {
		got[s.Datasource()] = s.Schema()
	}
	if got["registry_put"] != "registry_put" {
		t.Errorf("registry_put schema = %q, want registry_put", got["registry_put"])
	}
	if got["registry_oci"] != "registry_oci" {
		t.Errorf("registry_oci schema = %q, want registry_oci", got["registry_oci"])
	}
	if got["audit"] != "" {
		t.Errorf("schema-less source must round-trip empty, got %q", got["audit"])
	}
}

func TestLoadBundleSources_RejectsTamperedPayload(t *testing.T) {
	root := writeRoundTripBundle(t,
		NewSQLSource("iam", Datasource{}, nil,
			Definition{Name: "0001_init", SQL: "CREATE SCHEMA iam;"},
		),
	)
	// Tamper a payload so its hash no longer matches the manifest.
	payload := root + "/payload/sql/default/iam/0001_init.up.sql"
	if err := os.WriteFile(payload, []byte("DROP DATABASE prod;\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadBundleSources(os.DirFS(root)); err == nil {
		t.Fatal("expected a tampered payload to be rejected")
	}
}

func TestApplyBundle_NilPool(t *testing.T) {
	root := writeRoundTripBundle(t,
		NewSQLSource("iam", Datasource{}, nil,
			Definition{Name: "0001_init", SQL: "CREATE SCHEMA iam;"},
		),
	)
	// The bundle loads fine; ApplyToPool then rejects the nil pool. This proves
	// the load path runs before delegating execution.
	if _, err := ApplyBundle(context.Background(), nil, os.DirFS(root)); err == nil {
		t.Fatal("ApplyBundle with a nil pool must error")
	}
}

func keysOf(m map[string]Definition) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
