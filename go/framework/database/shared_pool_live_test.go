package database

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	pdb "go.putnami.dev/protocol/database"
	diag "go.putnami.dev/protocol/diagnostic"
)

// liveTestConnection returns a connection to a database the live test may
// create schemas in, or SKIPS when no test binding is injected (so the DB-free
// gate stays green without a PostgreSQL). It reads the first datasource of the
// injected test binding and honors the binding's isolation policy the way
// go.putnami.dev/database/testprovider does — which this package cannot import
// (testprovider imports it):
//
//   - isolation "schema": the bound database is the shared server every suite
//     creates its schemas in, so the bound connection is returned as is.
//   - isolation "database" (the default): the role is not granted CREATE on the
//     bound database — a CI role holds CREATEDB and nothing on the maintenance
//     database — so an isolated database is created through the bound
//     connection and a connection to it is returned. Cleanup drops it unless
//     the binding keeps databases.
//
// Either way the test ends up with two schemas in ONE database, which is what
// the shared-pool proof needs; a per-datasource isolated database would give
// it two physical pools.
func liveTestConnection(t *testing.T) *pdb.Connection {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(envTestBinding))
	if raw == "" {
		t.Skipf("%s not set; skipping the live shared-pool test", envTestBinding)
	}
	tb, diags := pdb.ParseAndValidateTestBinding([]byte(raw))
	if diag.HasErrors(diags) {
		t.Fatalf("invalid %s: %v", envTestBinding, diags)
	}
	if tb == nil || len(tb.Databases) == 0 {
		t.Skipf("%s carries no datasource; skipping the live shared-pool test", envTestBinding)
	}
	names := make([]string, 0, len(tb.Databases))
	for name := range tb.Databases {
		names = append(names, name)
	}
	sort.Strings(names)
	entry := tb.Databases[names[0]]
	if entry.Connection == nil {
		t.Skipf("%s datasource %q has no connection; skipping", envTestBinding, names[0])
	}
	if tb.Isolation == pdb.IsolationSchema {
		return entry.Connection
	}

	ctx := context.Background()
	admin := liveTestPool(t, entry.Connection)
	isolated := fmt.Sprintf("shared_pool_%d_%d", time.Now().UnixNano(), os.Getpid())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{isolated}.Sanitize()); err != nil {
		t.Fatalf("create isolated database %s through %s: %v", isolated, envTestBinding, err)
	}
	if !tb.KeepDatabases {
		t.Cleanup(func() {
			if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{isolated}.Sanitize()+" WITH (FORCE)"); err != nil {
				t.Errorf("drop isolated database %s: %v", isolated, err)
			}
		})
	}
	conn, err := connectionToDatabase(entry.Connection, isolated)
	if err != nil {
		t.Fatalf("point the test connection at %s: %v", isolated, err)
	}
	return conn
}

// liveTestPool opens a *Pool on conn with no owning schema, through the same
// PoolConfigFromBinding a workload resolves its binding with, and closes it on
// cleanup.
func liveTestPool(t *testing.T, conn *pdb.Connection) *Pool {
	t.Helper()
	binding := &pdb.Binding{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases:       map[string]pdb.Database{"_": {Engine: pdb.EnginePostgres, Connection: conn}},
	}
	cfg, err := PoolConfigFromBinding(binding, "_")
	if err != nil {
		t.Fatalf("PoolConfigFromBinding: %v", err)
	}
	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect %s: %v", envTestBinding, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// connectionToDatabase copies conn with its physical database swapped to db,
// never mutating the binding's own connection: a structured connection sets
// the Database field, a postgres:// DSN gets its path rewritten.
func connectionToDatabase(conn *pdb.Connection, db string) (*pdb.Connection, error) {
	out := *conn
	if conn.SSL != nil {
		ssl := *conn.SSL
		out.SSL = &ssl
	}
	if len(conn.Params) > 0 {
		out.Params = make(map[string]string, len(conn.Params))
		for k, v := range conn.Params {
			out.Params[k] = v
		}
	}
	if strings.TrimSpace(conn.DSN) != "" {
		u, err := url.Parse(conn.DSN)
		if err != nil {
			return nil, fmt.Errorf("parse the binding's dsn: %w", err)
		}
		u.Path = "/" + db
		out.DSN = u.String()
		return &out, nil
	}
	out.Database = db
	return &out, nil
}

// TestSharedPool_TwoDatasourcesOneDatabase_Live is the behavior proof of the
// shared physical pool: two datasources on one database, each owning a schema
// with a same-named table holding a different row, share ONE physical pool
// (PhysicalCount()==1) and — under concurrent, interleaved use that forces
// connections to change hands between the two datasources — every unqualified
// query resolves in its own datasource's schema, through autocommit, WithTx,
// PGXPool().Begin and the database/sql handle the migration runner uses. It
// deliberately carries no spectest.Proves: it skips without a database, and a
// skipped attestation would block the enforce gate.
func TestSharedPool_TwoDatasourcesOneDatabase_Live(t *testing.T) {
	conn := liveTestConnection(t)
	admin := liveTestPool(t, conn)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), os.Getpid())
	schemaA := "shared_pool_a_" + suffix
	schemaB := "shared_pool_b_" + suffix
	for schema, row := range map[string]string{schemaA: "row-of-a", schemaB: "row-of-b"} {
		if _, err := admin.Exec(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
			t.Fatalf("create schema %s: %v", schema, err)
		}
		if _, err := admin.Exec(ctx, `CREATE TABLE "`+schema+`".t (v text NOT NULL)`); err != nil {
			t.Fatalf("create table in %s: %v", schema, err)
		}
		if _, err := admin.Exec(ctx, `INSERT INTO "`+schema+`".t (v) VALUES ($1)`, row); err != nil {
			t.Fatalf("seed %s: %v", schema, err)
		}
	}
	t.Cleanup(func() {
		for _, schema := range []string{schemaA, schemaB} {
			if _, err := admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`); err != nil {
				t.Errorf("drop schema %s: %v", schema, err)
			}
		}
	})

	// Two datasources, one connection, two schemas — the runtime binding a
	// deployer injects for a workload owning two schemas of one database. The
	// pool is kept small so the goroutines below must hand connections back
	// and forth between the datasources, which is the case the per-acquire
	// search_path exists for.
	binding := &pdb.Binding{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases: map[string]pdb.Database{
			"shared_a": {Engine: pdb.EnginePostgres, Schema: schemaA, Connection: conn},
			"shared_b": {Engine: pdb.EnginePostgres, Schema: schemaB, Connection: conn},
		},
	}
	tuning := PoolConfig{MaxConns: 3, MinConns: 1}
	pools := newPools(map[string]datasourceSpec{
		"shared_a": {name: "shared_a", pool: tuning},
		"shared_b": {name: "shared_b", pool: tuning},
	}, "", connectPhysical, func() (*pdb.Binding, string, error) { return binding, "", nil })
	t.Cleanup(pools.Close)

	a, err := pools.For("shared_a")
	if err != nil {
		t.Fatalf("For(shared_a): %v", err)
	}
	b, err := pools.For("shared_b")
	if err != nil {
		t.Fatalf("For(shared_b): %v", err)
	}
	if got := pools.PhysicalCount(); got != 1 {
		t.Fatalf("PhysicalCount() = %d, want 1: two datasources on one database must share one physical pool", got)
	}
	if a.physical != b.physical || a == b {
		t.Fatal("the two datasources must be distinct logical pools over one physical pool")
	}
	if got := a.PGXPool().Config().ConnConfig.RuntimeParams["search_path"]; got != "" {
		t.Fatalf("the physical pool carries search_path=%q as a startup parameter, want none", got)
	}

	type datasource struct {
		pool   *Pool
		schema string
		row    string
	}
	sources := []datasource{{a, schemaA, "row-of-a"}, {b, schemaB, "row-of-b"}}

	// check runs one round of unqualified reads on ds through every acquire
	// path and reports the first mismatch.
	check := func(ctx context.Context, ds datasource, path string, q Querier) error {
		var v, sp string
		if err := q.QueryRow(ctx, "SELECT v FROM t").Scan(&v); err != nil {
			return fmt.Errorf("%s: SELECT v FROM t on %s: %w", path, ds.schema, err)
		}
		if v != ds.row {
			return fmt.Errorf("%s: SELECT v FROM t on %s = %q, want %q (wrong schema)", path, ds.schema, v, ds.row)
		}
		if err := q.QueryRow(ctx, "SHOW search_path").Scan(&sp); err != nil {
			return fmt.Errorf("%s: SHOW search_path on %s: %w", path, ds.schema, err)
		}
		if strings.Trim(sp, `"`) != ds.schema {
			return fmt.Errorf("%s: search_path on %s = %q, want %q", path, ds.schema, sp, ds.schema)
		}
		return nil
	}
	round := func(ctx context.Context, ds datasource) error {
		// Autocommit through the logical pool.
		if err := check(ctx, ds, "autocommit", ds.pool); err != nil {
			return err
		}
		// Inside WithTx: the transaction's connection was acquired for this
		// datasource, and the tx joins through the pool's Querier.
		if err := WithTx(ctx, ds.pool, func(txCtx context.Context) error {
			return check(txCtx, ds, "WithTx", ds.pool.Querier(txCtx))
		}); err != nil {
			return err
		}
		// Through the advanced view, the way Putnami Cloud begins its
		// transactions.
		tx, err := ds.pool.PGXPool().Begin(ctx)
		if err != nil {
			return fmt.Errorf("PGXPool().Begin on %s: %w", ds.schema, err)
		}
		if err := check(ctx, ds, "PGXPool().Begin", tx); err != nil {
			_ = tx.Rollback(ctx) //nolint:errcheck // the check error is the one to report
			return err
		}
		return tx.Commit(ctx)
	}

	const goroutines, iterations = 8, 40
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range iterations {
				ds := sources[(g+i)%2]
				if err := round(ctx, ds); err != nil {
					errs <- fmt.Errorf("goroutine %d iteration %d: %w", g, i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	// The database/sql path the migration runner and ApplyToPool use: every
	// connection the handle opens is acquired under datasource b's search_path,
	// so a schema-less statement lands in b — on the same shared physical pool
	// that just served a.
	db := newStdlibDB(b)
	if db == nil {
		t.Fatal("newStdlibDB(b) = nil, want an open handle")
	}
	defer db.Close() //nolint:errcheck // best-effort close
	var v string
	if err := db.QueryRowContext(ctx, "SELECT v FROM t").Scan(&v); err != nil {
		t.Fatalf("stdlib SELECT v FROM t on b: %v", err)
	}
	if v != "row-of-b" {
		t.Errorf("stdlib SELECT v FROM t on b = %q, want row-of-b", v)
	}
	sqlTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("stdlib BeginTx on b: %v", err)
	}
	if err := sqlTx.QueryRowContext(ctx, "SELECT v FROM t").Scan(&v); err != nil {
		t.Fatalf("stdlib tx SELECT v FROM t on b: %v", err)
	}
	if err := sqlTx.Commit(); err != nil {
		t.Fatalf("stdlib tx commit: %v", err)
	}
	if v != "row-of-b" {
		t.Errorf("stdlib tx SELECT v FROM t on b = %q, want row-of-b", v)
	}

	// The shared pool never grew past its one ceiling: the statistics are the
	// physical pool's, identical from both datasources.
	if a.Stats().MaxConns() != 3 || b.Stats().MaxConns() != 3 || a.Stats().TotalConns() > 3 {
		t.Errorf("shared pool stats: a max %d, b max %d, total %d; want one ceiling of 3", a.Stats().MaxConns(), b.Stats().MaxConns(), a.Stats().TotalConns())
	}

	// Closing one datasource keeps the other one working on the shared pool.
	a.Close()
	if err := check(ctx, sources[1], "after closing a", b); err != nil {
		t.Errorf("b after a.Close(): %v", err)
	}
	if err := a.Ping(ctx); err == nil {
		t.Error("a closed logical pool must not serve; Ping should fail")
	}
}
