package main

import "testing"

// TestTelemetryPoolConfigLeavesIdentityToTheFramework pins the contract:
// the site sets NO identity hooks. At deploy the managed binding supplies a
// Cloud SQL socket without user or password, and the database package resolves
// the workload's GCP identity and per-connection IAM token natively for that
// host shape — wiring them here again would shadow that seam and silently
// bypass fixes to it.
func TestTelemetryPoolConfigLeavesIdentityToTheFramework(t *testing.T) {
	const dsn = "host=/cloudsql/project:region:instance dbname=telemetry"

	cfg := telemetryPoolConfig(dsn)

	if cfg.DSN != dsn {
		t.Fatalf("DSN = %q, want %q", cfg.DSN, dsn)
	}
	if cfg.IdentityResolver != nil {
		t.Fatal("IdentityResolver must stay nil: Cloud SQL identity is the database package's native default")
	}
	if cfg.TokenFetcher != nil {
		t.Fatal("TokenFetcher must stay nil: Cloud SQL IAM tokens are the database package's native default")
	}
}
