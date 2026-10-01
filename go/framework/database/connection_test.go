package database

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestNewPool_ConnectTimeoutBoundsUnboundedContext(t *testing.T) {
	// A listener that accepts TCP but never completes the Postgres handshake:
	// the ping hangs forever under an unbounded context. ConnectTimeout must
	// bound pool creation even though the caller passes context.Background().
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	dsn := fmt.Sprintf("postgres://user:pass@%s/db?sslmode=disable", ln.Addr().String())

	start := time.Now()
	_, err = NewPool(context.Background(), PoolConfig{
		DSN:            dsn,
		ConnectTimeout: 200 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a connect-timeout error")
	}
	if elapsed > 3*time.Second {
		t.Errorf("ConnectTimeout not enforced: NewPool took %v under an unbounded context", elapsed)
	}
}

func TestPoolConfigWithDefaults(t *testing.T) {
	t.Run("empty config gets all defaults", func(t *testing.T) {
		cfg := PoolConfig{}.withDefaults()

		if cfg.MaxConns != 10 {
			t.Errorf("MaxConns = %d, want 10", cfg.MaxConns)
		}
		if cfg.MinConns != 2 {
			t.Errorf("MinConns = %d, want 2", cfg.MinConns)
		}
		if cfg.MaxConnLifetime != time.Hour {
			t.Errorf("MaxConnLifetime = %v, want %v", cfg.MaxConnLifetime, time.Hour)
		}
		if cfg.MaxConnIdleTime != 30*time.Minute {
			t.Errorf("MaxConnIdleTime = %v, want %v", cfg.MaxConnIdleTime, 30*time.Minute)
		}
		if cfg.HealthCheckPeriod != time.Minute {
			t.Errorf("HealthCheckPeriod = %v, want %v", cfg.HealthCheckPeriod, time.Minute)
		}
		if cfg.ConnectTimeout != 10*time.Second {
			t.Errorf("ConnectTimeout = %v, want %v", cfg.ConnectTimeout, 10*time.Second)
		}
		if cfg.StatementTimeout != 30*time.Second {
			t.Errorf("StatementTimeout = %v, want %v", cfg.StatementTimeout, 30*time.Second)
		}
		if cfg.IdentityResolver != nil {
			t.Error("IdentityResolver default should be nil")
		}
		if cfg.TokenFetcher != nil {
			t.Error("TokenFetcher default should be nil")
		}
	})

	t.Run("custom values are preserved", func(t *testing.T) {
		cfg := PoolConfig{
			DSN:               "postgres://localhost/test",
			MaxConns:          20,
			MinConns:          5,
			MaxConnLifetime:   2 * time.Hour,
			MaxConnIdleTime:   15 * time.Minute,
			HealthCheckPeriod: 30 * time.Second,
			ConnectTimeout:    5 * time.Second,
			StatementTimeout:  45 * time.Second,
		}.withDefaults()

		if cfg.DSN != "postgres://localhost/test" {
			t.Errorf("DSN = %q, want postgres://localhost/test", cfg.DSN)
		}
		if cfg.MaxConns != 20 {
			t.Errorf("MaxConns = %d, want 20", cfg.MaxConns)
		}
		if cfg.MinConns != 5 {
			t.Errorf("MinConns = %d, want 5", cfg.MinConns)
		}
		if cfg.MaxConnLifetime != 2*time.Hour {
			t.Errorf("MaxConnLifetime = %v, want %v", cfg.MaxConnLifetime, 2*time.Hour)
		}
		if cfg.MaxConnIdleTime != 15*time.Minute {
			t.Errorf("MaxConnIdleTime = %v, want %v", cfg.MaxConnIdleTime, 15*time.Minute)
		}
		if cfg.HealthCheckPeriod != 30*time.Second {
			t.Errorf("HealthCheckPeriod = %v, want %v", cfg.HealthCheckPeriod, 30*time.Second)
		}
		if cfg.ConnectTimeout != 5*time.Second {
			t.Errorf("ConnectTimeout = %v, want %v", cfg.ConnectTimeout, 5*time.Second)
		}
		if cfg.StatementTimeout != 45*time.Second {
			t.Errorf("StatementTimeout = %v, want %v", cfg.StatementTimeout, 45*time.Second)
		}
	})

	t.Run("partial config fills missing defaults", func(t *testing.T) {
		cfg := PoolConfig{
			MaxConns: 50,
		}.withDefaults()

		if cfg.MaxConns != 50 {
			t.Errorf("MaxConns = %d, want 50", cfg.MaxConns)
		}
		if cfg.MinConns != 2 {
			t.Errorf("MinConns = %d, want 2 (default)", cfg.MinConns)
		}
		if cfg.MaxConnLifetime != time.Hour {
			t.Errorf("MaxConnLifetime = %v, want %v (default)", cfg.MaxConnLifetime, time.Hour)
		}
	})
}

// TestBuildPoolConfig_StatementTimeoutRuntimeParam proves the configured
// StatementTimeout reaches the pgx config as the statement_timeout runtime
// parameter (milliseconds, as a string) — a server-side per-statement bound
// so a runaway query cannot pin a pooled connection indefinitely. No live
// database is required: buildPoolConfig renders the config without connecting.
func TestBuildPoolConfig_StatementTimeoutRuntimeParam(t *testing.T) {
	t.Run("configured value is applied in milliseconds", func(t *testing.T) {
		cfg, err := PoolConfig{
			DSN:              "postgres://app:p@localhost:5432/test",
			StatementTimeout: 5 * time.Second,
		}.withDefaults().buildPoolConfig(context.Background())
		if err != nil {
			t.Fatalf("buildPoolConfig: %v", err)
		}
		if got := cfg.ConnConfig.RuntimeParams["statement_timeout"]; got != "5000" {
			t.Errorf("statement_timeout runtime param = %q, want 5000", got)
		}
	})

	t.Run("default secure bound is applied when unset", func(t *testing.T) {
		cfg, err := PoolConfig{
			DSN: "postgres://app:p@localhost:5432/test",
		}.withDefaults().buildPoolConfig(context.Background())
		if err != nil {
			t.Fatalf("buildPoolConfig: %v", err)
		}
		if got := cfg.ConnConfig.RuntimeParams["statement_timeout"]; got != "30000" {
			t.Errorf("statement_timeout runtime param = %q, want 30000 (default)", got)
		}
	})

	t.Run("negative value disables the bound", func(t *testing.T) {
		cfg, err := PoolConfig{
			DSN:              "postgres://app:p@localhost:5432/test",
			StatementTimeout: -1,
		}.withDefaults().buildPoolConfig(context.Background())
		if err != nil {
			t.Fatalf("buildPoolConfig: %v", err)
		}
		if _, ok := cfg.ConnConfig.RuntimeParams["statement_timeout"]; ok {
			t.Errorf("statement_timeout runtime param set, want unset for disabled bound")
		}
	})
}
