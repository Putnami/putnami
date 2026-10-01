package database

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	pdb "go.putnami.dev/protocol/database"
)

// buildPoolConfig is the seam where PoolConfig meets the parsed DSN, so these
// assert the resolved pgxpool config rather than PoolConfig itself.
func resolvedPoolConfig(t *testing.T, cfg PoolConfig) *pgxpool.Config {
	t.Helper()
	poolCfg, err := cfg.withDefaults().buildPoolConfig(context.Background())
	if err != nil {
		t.Fatalf("buildPoolConfig: %v", err)
	}
	return poolCfg
}

func resolvedPool(t *testing.T, cfg PoolConfig) (maxConns, minConns int32) {
	t.Helper()
	poolCfg := resolvedPoolConfig(t, cfg)
	return poolCfg.MaxConns, poolCfg.MinConns
}

// The lever a test binding needs: 23 datasources × the default 10 reserved 230
// connections to use one or two at a time, and nothing could ask for less.
func TestPoolSize_DSNValueWinsOverTheDefault(t *testing.T) {
	maxConns, minConns := resolvedPool(t, PoolConfig{
		DSN: "postgres://u:p@localhost:5432/db?pool_max_conns=2&pool_min_conns=1",
	})
	if maxConns != 2 {
		t.Errorf("MaxConns = %d, want the DSN's 2 rather than the default 10", maxConns)
	}
	if minConns != 1 {
		t.Errorf("MinConns = %d, want the DSN's 1", minConns)
	}
}

func TestPoolSize_KeywordDSNValueWins(t *testing.T) {
	maxConns, _ := resolvedPool(t, PoolConfig{
		DSN: "host=localhost port=5432 dbname=db user=u password=p pool_max_conns=3",
	})
	if maxConns != 3 {
		t.Errorf("MaxConns = %d, want the DSN's 3", maxConns)
	}
}

// The regression guard that matters: no binding emits these parameters today, so
// every existing DSN must still take exactly the value it took before.
func TestPoolSize_SilentDSNKeepsConfiguredSizing(t *testing.T) {
	maxConns, minConns := resolvedPool(t, PoolConfig{
		DSN: "postgres://u:p@localhost:5432/db",
	})
	if maxConns != 10 || minConns != 2 {
		t.Errorf("MaxConns/MinConns = %d/%d, want the unchanged defaults 10/2", maxConns, minConns)
	}

	explicit, _ := resolvedPool(t, PoolConfig{
		DSN:      "postgres://u:p@localhost:5432/db",
		MaxConns: 7,
	})
	if explicit != 7 {
		t.Errorf("MaxConns = %d, want the caller's explicit 7", explicit)
	}
}

// A DSN ceiling below the default floor of 2 would make pgxpool refuse to build
// the pool, naming neither source. The ceiling asked for wins; the floor yields.
func TestPoolSize_DSNCeilingBelowDefaultFloorIsClamped(t *testing.T) {
	maxConns, minConns := resolvedPool(t, PoolConfig{
		DSN: "postgres://u:p@localhost:5432/db?pool_max_conns=1",
	})
	if maxConns != 1 {
		t.Errorf("MaxConns = %d, want the DSN's 1", maxConns)
	}
	if minConns != 1 {
		t.Errorf("MinConns = %d, want the floor clamped to the ceiling", minConns)
	}
}

// The end-to-end path a test environment actually uses: DATABASE_TEST_BINDINGS
// carries a structured connection, and its `params` are libpq/pgx options. This
// is what lets a provider ask for a small pool WITHOUT a new protocol field —
// pdb.Connection.Params already existed, and the unconditional assignment in
// buildPoolConfig was the only thing discarding it.
func TestPoolSize_BindingParamsSizeTheTestPool(t *testing.T) {
	ssl := false
	binding := &pdb.Binding{
		ProtocolVersion: 1,
		Databases: map[string]pdb.Database{
			"main": {
				Engine: pdb.EnginePostgres,
				Schema: "public",
				Connection: &pdb.Connection{
					Host: "localhost", Port: 5432, Database: "db",
					User: "u", Password: "p", SSL: &ssl,
					Params: map[string]string{dsnPoolMaxConns: "4"},
				},
			},
		},
	}
	pc, err := PoolConfigFromBinding(binding, "main")
	if err != nil {
		t.Fatalf("PoolConfigFromBinding: %v", err)
	}
	maxConns, _ := resolvedPool(t, pc)
	if maxConns != 4 {
		t.Fatalf("MaxConns = %d, want the binding's 4 — a 23-datasource workload would "+
			"otherwise still reserve 23x10 connections to use one or two at a time", maxConns)
	}
}

// --- The connection-lifecycle bounds take the same precedence ---------------

// Sizing alone does not release anything: a pool of 2 with the default 30-minute
// idle time still holds both connections for half an hour after the last query.
// The idle bound is the half that gives them back.
func TestPoolLifecycle_DSNValueWinsOverTheDefaults(t *testing.T) {
	poolCfg := resolvedPoolConfig(t, PoolConfig{
		DSN: "postgres://u:p@localhost:5432/db" +
			"?pool_max_conn_idle_time=5s&pool_max_conn_lifetime=2m&pool_health_check_period=10s",
	})
	if poolCfg.MaxConnIdleTime != 5*time.Second {
		t.Errorf("MaxConnIdleTime = %s, want the DSN's 5s rather than the default 30m", poolCfg.MaxConnIdleTime)
	}
	if poolCfg.MaxConnLifetime != 2*time.Minute {
		t.Errorf("MaxConnLifetime = %s, want the DSN's 2m", poolCfg.MaxConnLifetime)
	}
	if poolCfg.HealthCheckPeriod != 10*time.Second {
		t.Errorf("HealthCheckPeriod = %s, want the DSN's 10s", poolCfg.HealthCheckPeriod)
	}
}

// The key=value DSN form is the one a binding actually produces: Connection.dsn
// renders host/dbname/user/password plus every param as libpq keywords.
func TestPoolLifecycle_KeywordDSNValueWins(t *testing.T) {
	poolCfg := resolvedPoolConfig(t, PoolConfig{
		DSN: "host=localhost port=5432 dbname=db user=u password=p " +
			"pool_max_conn_idle_time=5s pool_max_conn_lifetime=2m pool_health_check_period=10s",
	})
	if poolCfg.MaxConnIdleTime != 5*time.Second {
		t.Errorf("MaxConnIdleTime = %s, want the DSN's 5s", poolCfg.MaxConnIdleTime)
	}
	if poolCfg.MaxConnLifetime != 2*time.Minute {
		t.Errorf("MaxConnLifetime = %s, want the DSN's 2m", poolCfg.MaxConnLifetime)
	}
	if poolCfg.HealthCheckPeriod != 10*time.Second {
		t.Errorf("HealthCheckPeriod = %s, want the DSN's 10s", poolCfg.HealthCheckPeriod)
	}
}

// The struct fills every gap, one bound at a time: stating the idle time must
// not surrender the lifetime or the health-check period to pgxpool's own
// defaults.
func TestPoolLifecycle_StructFillsTheUnstatedBounds(t *testing.T) {
	poolCfg := resolvedPoolConfig(t, PoolConfig{
		DSN:             "postgres://u:p@localhost:5432/db?pool_max_conn_idle_time=5s",
		MaxConnLifetime: 90 * time.Minute,
	})
	if poolCfg.MaxConnIdleTime != 5*time.Second {
		t.Errorf("MaxConnIdleTime = %s, want the DSN's 5s", poolCfg.MaxConnIdleTime)
	}
	if poolCfg.MaxConnLifetime != 90*time.Minute {
		t.Errorf("MaxConnLifetime = %s, want the caller's 90m", poolCfg.MaxConnLifetime)
	}
	if poolCfg.HealthCheckPeriod != time.Minute {
		t.Errorf("HealthCheckPeriod = %s, want the unchanged default 1m", poolCfg.HealthCheckPeriod)
	}
}

// The regression guard that matters: no production binding emits these
// parameters, so a silent DSN must still take exactly the values it took before.
func TestPoolLifecycle_SilentDSNKeepsConfiguredBounds(t *testing.T) {
	poolCfg := resolvedPoolConfig(t, PoolConfig{DSN: "postgres://u:p@localhost:5432/db"})
	if poolCfg.MaxConnLifetime != time.Hour {
		t.Errorf("MaxConnLifetime = %s, want the unchanged default 1h", poolCfg.MaxConnLifetime)
	}
	if poolCfg.MaxConnIdleTime != 30*time.Minute {
		t.Errorf("MaxConnIdleTime = %s, want the unchanged default 30m", poolCfg.MaxConnIdleTime)
	}
	if poolCfg.HealthCheckPeriod != time.Minute {
		t.Errorf("HealthCheckPeriod = %s, want the unchanged default 1m", poolCfg.HealthCheckPeriod)
	}
}

// pgx has a sixth pool parameter whose name extends a fifth's. A keyword match
// that stopped at the shared prefix would read the jitter as the lifetime and
// silently drop the struct's bound.
func TestPoolLifecycle_JitterDoesNotMasqueradeAsTheLifetime(t *testing.T) {
	poolCfg := resolvedPoolConfig(t, PoolConfig{
		DSN:             "host=localhost port=5432 dbname=db user=u password=p pool_max_conn_lifetime_jitter=1s",
		MaxConnLifetime: 90 * time.Minute,
	})
	if poolCfg.MaxConnLifetime != 90*time.Minute {
		t.Errorf("MaxConnLifetime = %s, want the caller's 90m — the jitter names a different bound",
			poolCfg.MaxConnLifetime)
	}
}

// The end-to-end path a test environment uses: DATABASE_TEST_BINDINGS carries a
// structured connection whose `params` are libpq/pgx options, and
// Connection.dsn renders them into the keyword DSN this precedence reads.
func TestPoolLifecycle_BindingParamsShortenTheTestIdleTime(t *testing.T) {
	ssl := false
	binding := &pdb.Binding{
		ProtocolVersion: 1,
		Databases: map[string]pdb.Database{
			"main": {
				Engine: pdb.EnginePostgres,
				Schema: "public",
				Connection: &pdb.Connection{
					Host: "localhost", Port: 5432, Database: "db",
					User: "u", Password: "p", SSL: &ssl,
					Params: map[string]string{
						dsnPoolMaxConns:        "2",
						dsnPoolMinConns:        "0",
						dsnPoolMaxConnIdleTime: "5s",
					},
				},
			},
		},
	}
	pc, err := PoolConfigFromBinding(binding, "main")
	if err != nil {
		t.Fatalf("PoolConfigFromBinding: %v", err)
	}
	poolCfg := resolvedPoolConfig(t, pc)
	if poolCfg.MaxConns != 2 || poolCfg.MinConns != 0 {
		t.Errorf("MaxConns/MinConns = %d/%d, want the binding's 2/0", poolCfg.MaxConns, poolCfg.MinConns)
	}
	if poolCfg.MaxConnIdleTime != 5*time.Second {
		t.Fatalf("MaxConnIdleTime = %s, want the binding's 5s — a pool of 2 that holds both "+
			"connections for 30 minutes still pins 2 connections per datasource for the whole run",
			poolCfg.MaxConnIdleTime)
	}
}
