package infra

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	pdb "go.putnami.dev/protocol/database"
	diag "go.putnami.dev/protocol/diagnostic"
)

// TestDatabasesFromManifest_DerivesProjection proves the infra database entries
// are derived from the canonical database protocol: the output is exactly the
// engine-mapped image of RequirementManifest.Project(), sorted by datasource
// name, one schema per datasource.
func TestDatabasesFromManifest_DerivesProjection(t *testing.T) {
	m := &pdb.RequirementManifest{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases: map[string]pdb.Requirement{
			"billing":   {Engine: pdb.EnginePostgres, Schema: "billing"},
			"auth":      {Engine: pdb.EnginePostgres, Schema: "iam"},
			"analytics": {Engine: pdb.EnginePostgres, Schema: "events"},
		},
	}

	got, diags := DatabasesFromManifest(m)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}

	want := []Database{
		{Name: "analytics", Engine: EnginePostgres, Schemas: []string{"events"}},
		{Name: "auth", Engine: EnginePostgres, Schemas: []string{"iam"}},
		{Name: "billing", Engine: EnginePostgres, Schemas: []string{"billing"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DatabasesFromManifest = %#v, want %#v", got, want)
	}

	// The result must line up one-to-one with the protocol projection it is
	// built on, so infra cannot diverge from the database protocol's view.
	projected := m.Project()
	if len(got) != len(projected) {
		t.Fatalf("len(got) = %d, len(Project()) = %d", len(got), len(projected))
	}
	for i := range projected {
		if got[i].Name != projected[i].Name {
			t.Errorf("entry %d name = %q, projection = %q", i, got[i].Name, projected[i].Name)
		}
		if !reflect.DeepEqual(got[i].Schemas, projected[i].Schemas) {
			t.Errorf("entry %d schemas = %v, projection = %v", i, got[i].Schemas, projected[i].Schemas)
		}
	}

	// The derived entries are a valid per-project manifest body.
	manifest := &PerProjectManifest{
		Schema:          PerProjectSchemaURL,
		ProtocolVersion: ProtocolVersion,
		Databases:       got,
	}
	if diags := ValidatePerProjectManifest(manifest); diag.HasErrors(diags) {
		t.Fatalf("derived manifest failed validation: %v", diags)
	}
}

// TestDatabasesFromManifest_NilAndEmpty covers the no-op inputs.
func TestDatabasesFromManifest_NilAndEmpty(t *testing.T) {
	if got, diags := DatabasesFromManifest(nil); got != nil || diags != nil {
		t.Errorf("nil manifest: got %v, diags %v; want nil, nil", got, diags)
	}
	empty := &pdb.RequirementManifest{ProtocolVersion: pdb.ProtocolVersion}
	if got, diags := DatabasesFromManifest(empty); got != nil || diags != nil {
		t.Errorf("empty manifest: got %v, diags %v; want nil, nil", got, diags)
	}
}

// TestDatabasesFromManifest_NoSchema maps a datasource that declares no schema
// to an entry with no schemas, never inventing one from the name.
func TestDatabasesFromManifest_NoSchema(t *testing.T) {
	m := &pdb.RequirementManifest{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases:       map[string]pdb.Requirement{"cache": {Engine: pdb.EnginePostgres}},
	}
	got, diags := DatabasesFromManifest(m)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	want := []Database{{Name: "cache", Engine: EnginePostgres}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DatabasesFromManifest = %#v, want %#v", got, want)
	}
}

// TestDatabasesFromManifest_UnknownEngine reports engines the infra protocol
// cannot represent instead of coercing them, and drops the offending entry.
func TestDatabasesFromManifest_UnknownEngine(t *testing.T) {
	m := &pdb.RequirementManifest{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases: map[string]pdb.Requirement{
			"auth":   {Engine: pdb.EnginePostgres, Schema: "iam"},
			"legacy": {Engine: pdb.Engine("mysql"), Schema: "app"},
		},
	}
	got, diags := DatabasesFromManifest(m)
	if !diag.HasErrors(diags) {
		t.Fatalf("expected a diagnostic for the unmapped engine, got none")
	}
	errs := diag.Errors(diags)
	if len(errs) != 1 || errs[0].Code != ErrorCodeInvalidEngine {
		t.Fatalf("diagnostics = %v, want one %s", diags, ErrorCodeInvalidEngine)
	}
	want := []Database{{Name: "auth", Engine: EnginePostgres, Schemas: []string{"iam"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DatabasesFromManifest = %#v, want %#v (offending entry dropped)", got, want)
	}
}

// TestDatabasesFromManifest_CannotLeakSecrets proves the two-part guarantee
// that secrets never reach infra through this bridge:
//
//  1. A RequirementManifest cannot even carry a connection — strict parsing of
//     the canonical protocol rejects one.
//  2. The derived infra entries serialize to nothing but name/engine/schemas;
//     no connection, dsn, host, user, password, ssl, or param survives.
func TestDatabasesFromManifest_CannotLeakSecrets(t *testing.T) {
	withConnection := []byte(`{
		"$schema": "https://putnami.dev/schemas/putnami-database.json",
		"protocolVersion": 1,
		"databases": {
			"auth": {
				"engine": "postgres",
				"schema": "iam",
				"connection": { "dsn": "postgres://app:s3cret@10.0.0.5:5432/auth" }
			}
		}
	}`)
	if _, diags := pdb.ParseAndValidateRequirementManifest(withConnection); !diag.HasErrors(diags) {
		t.Fatalf("a requirement manifest with a connection must be rejected, was accepted")
	}

	// A clean, secret-free manifest derives clean infra entries.
	clean := []byte(`{
		"$schema": "https://putnami.dev/schemas/putnami-database.json",
		"protocolVersion": 1,
		"databases": { "auth": { "engine": "postgres", "schema": "iam" } }
	}`)
	m, diags := pdb.ParseAndValidateRequirementManifest(clean)
	if diag.HasErrors(diags) {
		t.Fatalf("clean manifest rejected: %v", diags)
	}
	got, diags := DatabasesFromManifest(m)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"connection", "dsn", "host", "user", "password", "ssl", "params", "instance"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("derived infra databases leaked %q: %s", secret, encoded)
		}
	}
}
