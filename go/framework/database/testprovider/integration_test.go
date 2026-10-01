package testprovider

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.putnami.dev/database"
)

// TestProvision_Integration exercises the real provisioning path against an
// externally provided Postgres. It runs only when DATABASE_TEST_BINDINGS is
// injected (CI's service container or a developer's local server), so unit runs
// — including this container's — pay no database cost. With a bundle directory
// in DATABASE_TEST_BUNDLE it also applies migrations.
//
// It provisions, then proves each returned binding is usable by resolving it
// through the same runtime adapter (database.PoolConfigFromBinding) a workload
// would, connecting, and confirming the owning schema is the live search_path —
// the end-to-end contract the slice delivers.
func TestProvision_Integration(t *testing.T) {
	raw := os.Getenv(EnvTestBinding)
	if raw == "" {
		t.Skipf("%s not set; skipping live provisioning (set it to a postgres binding to run)", EnvTestBinding)
	}

	ctx := context.Background()
	opts := Options{}
	if bundleDir := os.Getenv("DATABASE_TEST_BUNDLE"); bundleDir != "" {
		opts.Bundle = os.DirFS(bundleDir)
	}

	result, err := Provision(ctx, opts)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() {
		if err := result.Cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	if len(result.Binding.Databases) == 0 {
		t.Fatal("provisioned binding has no datasources")
	}

	for name, entry := range result.Binding.Databases {
		cfg, err := database.PoolConfigFromBinding(result.Binding, name)
		if err != nil {
			t.Fatalf("PoolConfigFromBinding(%q): %v", name, err)
		}
		pool, err := database.NewPool(ctx, cfg)
		if err != nil {
			t.Fatalf("connect provisioned datasource %q: %v", name, err)
		}
		func() {
			defer pool.Close()
			var one int
			if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
				t.Fatalf("datasource %q SELECT 1 = %d, err=%v", name, one, err)
			}
			if entry.Schema != "" {
				assertSchemaOnSearchPath(ctx, t, pool, name, entry.Schema)
			}
		}()
	}
}

func assertSchemaOnSearchPath(ctx context.Context, t *testing.T, pool *database.Pool, name, schema string) {
	t.Helper()
	var searchPath string
	if err := pool.QueryRow(ctx, "SHOW search_path").Scan(&searchPath); err != nil {
		t.Fatalf("datasource %q SHOW search_path: %v", name, err)
	}
	if !containsSchema(searchPath, schema) {
		t.Errorf("datasource %q search_path = %q, want it to include the owning schema %q", name, searchPath, schema)
	}
}

// containsSchema reports whether a Postgres search_path string lists schema as
// one of its (comma-separated, possibly quoted/space-padded) entries.
func containsSchema(searchPath, schema string) bool {
	for _, part := range strings.Split(searchPath, ",") {
		if strings.Trim(strings.TrimSpace(part), `"'`) == schema {
			return true
		}
	}
	return false
}
