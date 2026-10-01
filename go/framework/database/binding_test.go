package database

import (
	"context"
	"strings"
	"testing"

	pdb "go.putnami.dev/protocol/database"
	"go.putnami.dev/protocol/features/spectest"
)

func boolPtr(b bool) *bool { return &b }

// TestPoolConfigFromBinding_TCP resolves a structured TCP datasource into a
// libpq DSN with the physical database name and maps the ssl flag to sslmode,
// and carries the owning schema as the search_path.
func TestPoolConfigFromBinding_TCP(t *testing.T) {
	b := &pdb.Binding{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases: map[string]pdb.Database{
			"auth": {
				Engine: pdb.EnginePostgres,
				Schema: "iam",
				Connection: &pdb.Connection{
					Host: "db.internal", Port: 5432, Database: "auth_db",
					User: "app", Password: "s3cret", SSL: boolPtr(true),
				},
			},
		},
	}

	pc, err := PoolConfigFromBinding(b, "auth")
	if err != nil {
		t.Fatalf("PoolConfigFromBinding: %v", err)
	}
	if pc.SearchPath != "iam" {
		t.Errorf("SearchPath = %q, want iam", pc.SearchPath)
	}
	dsn, err := pc.resolveDSN()
	if err != nil {
		t.Fatalf("resolveDSN: %v", err)
	}
	want := "host=db.internal dbname=auth_db port=5432 user=app password=s3cret sslmode=require"
	if dsn != want {
		t.Errorf("dsn = %q, want %q", dsn, want)
	}
}

// TestPoolConfigFromBinding_TCPSSLDisabled maps ssl:false to sslmode=disable.
func TestPoolConfigFromBinding_TCPSSLDisabled(t *testing.T) {
	b := &pdb.Binding{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases: map[string]pdb.Database{
			"auth": {Engine: pdb.EnginePostgres, Schema: "public", Connection: &pdb.Connection{
				Host: "localhost", Port: 5432, Database: "auth", User: "postgres", Password: "postgres", SSL: boolPtr(false),
			}},
		},
	}
	pc, err := PoolConfigFromBinding(b, "auth")
	if err != nil {
		t.Fatalf("PoolConfigFromBinding: %v", err)
	}
	dsn, err := pc.resolveDSN()
	if err != nil {
		t.Fatalf("resolveDSN: %v", err)
	}
	if !strings.Contains(dsn, "sslmode=disable") {
		t.Errorf("dsn = %q, want sslmode=disable", dsn)
	}
}

// TestPoolConfigFromBinding_ExplicitSSLModeWins keeps a deployer-set sslmode
// param even when the ssl flag is also present.
func TestPoolConfigFromBinding_ExplicitSSLModeWins(t *testing.T) {
	b := &pdb.Binding{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases: map[string]pdb.Database{
			"auth": {Engine: pdb.EnginePostgres, Schema: "iam", Connection: &pdb.Connection{
				Host: "h", Database: "auth", SSL: boolPtr(true),
				Params: map[string]string{"sslmode": "verify-full"},
			}},
		},
	}
	pc, err := PoolConfigFromBinding(b, "auth")
	if err != nil {
		t.Fatalf("PoolConfigFromBinding: %v", err)
	}
	dsn, err := pc.resolveDSN()
	if err != nil {
		t.Fatalf("resolveDSN: %v", err)
	}
	if !strings.Contains(dsn, "sslmode=verify-full") || strings.Contains(dsn, "sslmode=require") {
		t.Errorf("dsn = %q, want explicit sslmode=verify-full to win", dsn)
	}
}

// TestPoolConfigFromBinding_DSN passes a self-contained DSN through untouched
// while still applying the schema as search_path.
func TestPoolConfigFromBinding_DSN(t *testing.T) {
	const dsn = "postgres://app:s3cret@10.0.0.5:5432/analytics?sslmode=require"
	b := &pdb.Binding{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases: map[string]pdb.Database{
			"analytics": {Engine: pdb.EnginePostgres, Schema: "events", Connection: &pdb.Connection{DSN: dsn}},
		},
	}
	pc, err := PoolConfigFromBinding(b, "analytics")
	if err != nil {
		t.Fatalf("PoolConfigFromBinding: %v", err)
	}
	if pc.SearchPath != "events" {
		t.Errorf("SearchPath = %q, want events", pc.SearchPath)
	}
	got, err := pc.resolveDSN()
	if err != nil {
		t.Fatalf("resolveDSN: %v", err)
	}
	if got != dsn {
		t.Errorf("dsn = %q, want the verbatim DSN %q", got, dsn)
	}
}

// TestPoolConfigFromBinding_Instance resolves a Cloud SQL instance into the
// /cloudsql Unix-socket DSN, decoupled from the logical datasource name.
func TestPoolConfigFromBinding_Instance(t *testing.T) {
	b := &pdb.Binding{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases: map[string]pdb.Database{
			"billing": {Engine: pdb.EnginePostgres, Schema: "billing", Connection: &pdb.Connection{
				Instance: "my-project:us-central1:billing-db", Database: "billing_db", User: "app",
			}},
		},
	}
	pc, err := PoolConfigFromBinding(b, "billing")
	if err != nil {
		t.Fatalf("PoolConfigFromBinding: %v", err)
	}
	dsn, err := pc.resolveDSN()
	if err != nil {
		t.Fatalf("resolveDSN: %v", err)
	}
	want := "host=/cloudsql/my-project:us-central1:billing-db dbname=billing_db user=app"
	if dsn != want {
		t.Errorf("dsn = %q, want %q", dsn, want)
	}
}

// TestPoolConfigFromBinding_SearchPathIsNotAStartupParameter proves the
// resolved schema reaches the logical pool as its SearchPath but is NOT sent as
// a search_path startup parameter of the physical pool: that pool may be shared
// by several datasources of one database, so search_path is set per acquire by
// the PrepareConn hook the config carries instead (see search_path.go).
// statement_timeout stays a startup parameter — it is part of the tuning every
// datasource sharing the pool must agree on.
func TestPoolConfigFromBinding_SearchPathIsNotAStartupParameter(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "acquire-time-search-path", "the-physical-pool-carries-no-startup-search-path")
	b := &pdb.Binding{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases: map[string]pdb.Database{
			"auth": {Engine: pdb.EnginePostgres, Schema: "iam", Connection: &pdb.Connection{
				DSN: "postgres://app:p@localhost:5432/auth",
			}},
		},
	}
	pc, err := PoolConfigFromBinding(b, "auth")
	if err != nil {
		t.Fatalf("PoolConfigFromBinding: %v", err)
	}
	if pc.SearchPath != "iam" {
		t.Errorf("PoolConfig.SearchPath = %q, want iam (the binding's schema is the logical pool's search_path)", pc.SearchPath)
	}
	cfg, err := pc.withDefaults().buildPoolConfig(context.Background())
	if err != nil {
		t.Fatalf("buildPoolConfig: %v", err)
	}
	if got, ok := cfg.ConnConfig.RuntimeParams["search_path"]; ok {
		t.Errorf("search_path runtime param = %q, want none: a shared physical pool must not start with one datasource's search_path", got)
	}
	if got := cfg.ConnConfig.RuntimeParams["statement_timeout"]; got != "30000" {
		t.Errorf("statement_timeout runtime param = %q, want the default 30000", got)
	}
	if cfg.PrepareConn == nil {
		t.Error("buildPoolConfig must install the PrepareConn hook that sets search_path per acquire")
	}
}

// TestPoolConfigFromBinding_MissingDatasource fails with a diagnostic that
// lists the datasources the binding does declare.
func TestPoolConfigFromBinding_MissingDatasource(t *testing.T) {
	b := &pdb.Binding{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases: map[string]pdb.Database{
			"auth":    {Engine: pdb.EnginePostgres, Schema: "iam", Connection: &pdb.Connection{Host: "h", Database: "auth"}},
			"billing": {Engine: pdb.EnginePostgres, Schema: "b", Connection: &pdb.Connection{Host: "h", Database: "billing"}},
		},
	}
	_, err := PoolConfigFromBinding(b, "ledger")
	if err == nil {
		t.Fatal("expected an error for a datasource missing from the binding")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ledger") || !strings.Contains(msg, "auth") || !strings.Contains(msg, "billing") {
		t.Errorf("error %q should name the missing datasource and list the available ones", msg)
	}
}

// TestPoolConfigFromBinding_Errors covers the remaining guard rails.
func TestPoolConfigFromBinding_Errors(t *testing.T) {
	t.Run("nil binding", func(t *testing.T) {
		if _, err := PoolConfigFromBinding(nil, "auth"); err == nil {
			t.Error("expected error for a nil binding")
		}
	})
	t.Run("empty datasource name", func(t *testing.T) {
		b := &pdb.Binding{ProtocolVersion: pdb.ProtocolVersion}
		if _, err := PoolConfigFromBinding(b, "  "); err == nil {
			t.Error("expected error for an empty datasource name")
		}
	})
	t.Run("nil connection", func(t *testing.T) {
		b := &pdb.Binding{ProtocolVersion: pdb.ProtocolVersion, Databases: map[string]pdb.Database{
			"auth": {Engine: pdb.EnginePostgres, Schema: "iam"},
		}}
		if _, err := PoolConfigFromBinding(b, "auth"); err == nil {
			t.Error("expected error for a datasource with no connection")
		}
	})
}

// TestParseBinding_RejectsInvalid surfaces protocol validation errors and
// rejects a connection that smuggles multiple transport strategies.
func TestParseBinding_RejectsInvalid(t *testing.T) {
	raw := []byte(`{
		"$schema": "https://putnami.dev/schemas/putnami-database-binding.json",
		"protocolVersion": 1,
		"databases": { "auth": { "engine": "postgres", "schema": "iam",
			"connection": { "dsn": "postgres://x/auth", "host": "h" } } }
	}`)
	if _, err := ParseBinding(raw); err == nil {
		t.Fatal("expected ParseBinding to reject a connection with two strategies")
	}
}

// TestParseBinding_RoundTrip parses a valid multi-datasource binding and
// resolves one of its datasources.
func TestParseBinding_RoundTrip(t *testing.T) {
	raw := []byte(`{
		"$schema": "https://putnami.dev/schemas/putnami-database-binding.json",
		"protocolVersion": 1,
		"databases": {
			"auth":    { "engine": "postgres", "schema": "iam",     "connection": { "host": "localhost", "port": 5432, "database": "auth", "user": "postgres", "password": "postgres", "ssl": false } },
			"billing": { "engine": "postgres", "schema": "billing", "connection": { "instance": "p:r:billing-db", "database": "billing" } }
		}
	}`)
	b, err := ParseBinding(raw)
	if err != nil {
		t.Fatalf("ParseBinding: %v", err)
	}
	pc, err := PoolConfigFromBinding(b, "billing")
	if err != nil {
		t.Fatalf("PoolConfigFromBinding: %v", err)
	}
	dsn, err := pc.resolveDSN()
	if err != nil {
		t.Fatalf("resolveDSN: %v", err)
	}
	if !strings.HasPrefix(dsn, "host=/cloudsql/p:r:billing-db") {
		t.Errorf("dsn = %q, want a cloudsql socket host", dsn)
	}
}

// TestResolveBinding_EnvUnset returns ok=false so a caller can fall back to an
// explicit connection.
func TestResolveBinding_EnvUnset(t *testing.T) {
	t.Setenv(EnvBinding, "")
	cfg, ok, err := resolveBinding("auth")
	if err != nil {
		t.Fatalf("resolveBinding: %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false when %s is unset", EnvBinding)
	}
	if cfg.DSN != "" || cfg.SearchPath != "" || cfg.Connection.isSet() {
		t.Errorf("cfg = %+v, want a zero connection", cfg)
	}
}

// TestResolveBinding_FromEnv reads the injected binding and resolves a named
// datasource end to end.
func TestResolveBinding_FromEnv(t *testing.T) {
	t.Setenv(EnvBinding, `{
		"protocolVersion": 1,
		"databases": { "auth": { "engine": "postgres", "schema": "iam",
			"connection": { "host": "db", "port": 5432, "database": "auth", "user": "u", "password": "p", "ssl": false } } }
	}`)
	cfg, ok, err := resolveBinding("auth")
	if err != nil {
		t.Fatalf("resolveBinding: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true when the binding is injected")
	}
	if cfg.SearchPath != "iam" {
		t.Errorf("SearchPath = %q, want iam", cfg.SearchPath)
	}
}
