package testprovider

import (
	"cmp"
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"go.putnami.dev/database"
	pdb "go.putnami.dev/protocol/database"
)

// liveDatabaseBinding returns a one-datasource copy of the injected test
// binding that provisions isolated databases and drops them, or skips when no
// binding is injected or its policy rules reclaiming out: schema isolation
// creates no database, and keepDatabases means the server dies with the run.
func liveDatabaseBinding(t *testing.T) (*pdb.TestBinding, pdb.Database) {
	t.Helper()
	tb, err := ResolveTestBinding(os.Getenv(EnvTestBinding))
	if err != nil {
		t.Fatalf("parse %s: %v", EnvTestBinding, err)
	}
	if tb == nil || len(tb.Databases) == 0 {
		t.Skipf("%s not set; skipping the live reclaim test", EnvTestBinding)
	}
	if isolationOf(tb) != pdb.IsolationDatabase || tb.KeepDatabases {
		t.Skip("the binding uses schema isolation or keeps its databases; nothing to reclaim")
	}
	names := make([]string, 0, len(tb.Databases))
	for name := range tb.Databases {
		names = append(names, name)
	}
	sort.Strings(names)
	entry := tb.Databases[names[0]]
	return &pdb.TestBinding{
		ProtocolVersion: pdb.ProtocolVersion,
		Mode:            pdb.TestModeRequire,
		Isolation:       pdb.IsolationDatabase,
		Databases:       map[string]pdb.Database{"reclaim": entry},
	}, entry
}

func liveAdmin(t *testing.T, conn *pdb.Connection) *database.Pool {
	t.Helper()
	admin, err := openPool(context.Background(), conn, "")
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(admin.Close)
	return admin
}

func databaseExists(t *testing.T, admin *database.Pool, name string) bool {
	t.Helper()
	var exists bool
	if err := admin.QueryRow(context.Background(),
		"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		t.Fatalf("check database %s: %v", name, err)
	}
	return exists
}

// createStamped creates a database named like a per-suite one created at
// created, and drops it when the test ends if it still exists.
func createStamped(t *testing.T, admin *database.Pool, prefix string, created time.Time) string {
	t.Helper()
	name := prefix + isolatedSuffix(created)
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+quoteIdent(name)); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quoteIdent(name)+" WITH (FORCE)") //nolint:errcheck // best-effort test cleanup
	})
	return name
}

// TestReclaim_Integration proves, against a real server, that Provision drops
// what a killed suite left behind and never a live suite's database: one with
// an open connection, or one younger than orphanAge.
func TestReclaim_Integration(t *testing.T) {
	tb, entry := liveDatabaseBinding(t)
	base, err := connectionDatabase(entry.Connection)
	if err != nil {
		t.Fatalf("connection database: %v", err)
	}
	prefix := isolatedPrefix(cmp.Or(base, "reclaim"))
	admin := liveAdmin(t, entry.Connection)
	ctx := context.Background()
	old := time.Now().Add(-2 * orphanAge)

	orphan := createStamped(t, admin, prefix, old)
	live := createStamped(t, admin, prefix, old)
	young := createStamped(t, admin, prefix, time.Now().Add(-time.Minute))

	liveConn := cloneConnection(entry.Connection)
	if err := setConnectionDatabase(liveConn, live); err != nil {
		t.Fatalf("point at %s: %v", live, err)
	}
	livePool, err := openPool(ctx, liveConn, "")
	if err != nil {
		t.Fatalf("connect %s: %v", live, err)
	}
	defer livePool.Close()
	held, err := livePool.PGXPool().Acquire(ctx)
	if err != nil {
		t.Fatalf("hold a connection on %s: %v", live, err)
	}
	defer held.Release()

	res, err := Provision(ctx, Options{Binding: tb})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	provisioned, err := connectionDatabase(res.Binding.Databases["reclaim"].Connection)
	if err != nil {
		t.Fatalf("provisioned database: %v", err)
	}

	if databaseExists(t, admin, orphan) {
		t.Errorf("orphan %s (idle, older than %v) was not reclaimed", orphan, orphanAge)
	}
	if !databaseExists(t, admin, live) {
		t.Errorf("live suite's database %s (open connection) was dropped", live)
	}
	if !databaseExists(t, admin, young) {
		t.Errorf("young database %s (idle, younger than %v) was dropped", young, orphanAge)
	}

	if err := res.Cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if databaseExists(t, admin, provisioned) {
		t.Errorf("cleanup left %s behind", provisioned)
	}
}

// TestCleanup_Integration_OutlivesACanceledContext proves the per-suite
// database is dropped when the context Provision received is canceled before
// Cleanup runs — what t.Context() does to every test that provisions with it.
func TestCleanup_Integration_OutlivesACanceledContext(t *testing.T) {
	tb, entry := liveDatabaseBinding(t)
	admin := liveAdmin(t, entry.Connection)

	ctx, cancel := context.WithCancel(context.Background())
	res, err := Provision(ctx, Options{Binding: tb})
	if err != nil {
		cancel()
		t.Fatalf("Provision: %v", err)
	}
	provisioned, err := connectionDatabase(res.Binding.Databases["reclaim"].Connection)
	if err != nil {
		t.Fatalf("provisioned database: %v", err)
	}
	cancel()

	if err := res.Cleanup(); err != nil {
		t.Fatalf("cleanup after the provisioning context was canceled: %v", err)
	}
	if databaseExists(t, admin, provisioned) {
		t.Errorf("cleanup left %s behind once the provisioning context was canceled", provisioned)
	}
}
