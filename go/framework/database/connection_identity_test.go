package database

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// applyIdentityForTests reproduces the identity/password resolution NewPool
// performs against a parsed pgxpool.Config — sidesteps the network ping that
// NewPool does, so tests can verify the resolution logic in isolation.
func applyIdentityForTests(ctx context.Context, cfg PoolConfig, poolCfg *pgxpool.Config) error {
	cfg = cfg.withDefaults()
	if !dsnSpecifiesUser(cfg.DSN) {
		if cfg.IdentityResolver == nil {
			return stderrors.New("missing identity resolver")
		}
		user, err := cfg.IdentityResolver(ctx)
		if err != nil {
			return err
		}
		poolCfg.ConnConfig.User = trimServiceAccountSuffix(strings.TrimSpace(user))
	}
	if !dsnSpecifiesPassword(cfg.DSN) {
		poolCfg.ConnConfig.Password = ""
		if isUnixSocket(poolCfg.ConnConfig.Host) {
			fetch := cfg.TokenFetcher
			if fetch == nil {
				return stderrors.New("missing token fetcher")
			}
			poolCfg.BeforeConnect = func(ctx context.Context, c *pgx.ConnConfig) error {
				token, err := fetch(ctx)
				if err != nil {
					return err
				}
				c.Password = token
				return nil
			}
		}
	}
	return nil
}

func TestNewPoolRequiresDSN(t *testing.T) {
	if _, err := NewPool(context.Background(), PoolConfig{}); err == nil {
		t.Fatal("expected error for empty DSN")
	}
}

func parseTestPoolCfg(t *testing.T, dsn string) *pgxpool.Config {
	t.Helper()
	pgCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return pgCfg
}

func TestIdentityResolutionTrimsServiceAccountSuffix(t *testing.T) {
	dsn := "host=/cloudsql/p:r:i dbname=auth sslmode=disable"
	pgCfg := parseTestPoolCfg(t, dsn)
	cfg := PoolConfig{
		DSN: dsn,
		IdentityResolver: func(_ context.Context) (string, error) {
			return "auth-server@p.iam.gserviceaccount.com", nil
		},
		TokenFetcher: func(_ context.Context) (string, error) { return "tok", nil },
	}
	if err := applyIdentityForTests(context.Background(), cfg, pgCfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got, want := pgCfg.ConnConfig.User, "auth-server@p.iam"; got != want {
		t.Errorf("user = %q, want %q", got, want)
	}
}

func TestIdentityResolutionKeepsDevLaptopEmail(t *testing.T) {
	dsn := "host=/cloudsql/p:r:i dbname=auth sslmode=disable"
	pgCfg := parseTestPoolCfg(t, dsn)
	cfg := PoolConfig{
		DSN:              dsn,
		IdentityResolver: func(_ context.Context) (string, error) { return "user@example.com", nil },
		TokenFetcher:     func(_ context.Context) (string, error) { return "tok", nil },
	}
	if err := applyIdentityForTests(context.Background(), cfg, pgCfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got, want := pgCfg.ConnConfig.User, "user@example.com"; got != want {
		t.Errorf("user = %q, want %q (no trim for non-SA emails)", got, want)
	}
}

func TestIdentityResolutionFailureSurfaced(t *testing.T) {
	want := stderrors.New("resolver unavailable")
	dsn := "host=/cloudsql/p:r:i dbname=auth sslmode=disable"
	pgCfg := parseTestPoolCfg(t, dsn)
	cfg := PoolConfig{
		DSN:              dsn,
		IdentityResolver: func(_ context.Context) (string, error) { return "", want },
	}
	err := applyIdentityForTests(context.Background(), cfg, pgCfg)
	if err == nil {
		t.Fatal("expected resolver error to surface")
	}
	if !stderrors.Is(err, want) {
		t.Errorf("error should wrap resolver error; got %v", err)
	}
}

func TestIdentityResolverRequiredWhenUserMissing(t *testing.T) {
	// A host under /cloudsql/ now defaults to the native GCP resolver (see
	// gcp_identity_test.go); the explicit-hook requirement is guarded on a
	// socket shape that implies nothing.
	_, err := PoolConfig{
		DSN: "host=/var/run/postgresql dbname=auth sslmode=disable",
	}.withDefaults().buildPoolConfig(context.Background())
	if err == nil {
		t.Fatal("expected missing IdentityResolver error")
	}
	if !strings.Contains(err.Error(), "IdentityResolver") {
		t.Errorf("error = %v, want mention IdentityResolver", err)
	}
}

func TestIdentityResolverNotCalledWhenUserSet(t *testing.T) {
	calls := 0
	dsn := "host=/cloudsql/p:r:i user=explicit dbname=auth sslmode=disable"
	pgCfg := parseTestPoolCfg(t, dsn)
	cfg := PoolConfig{
		DSN: dsn,
		IdentityResolver: func(_ context.Context) (string, error) {
			calls++
			return "should-not-be-used", nil
		},
		TokenFetcher: func(_ context.Context) (string, error) { return "tok", nil },
	}
	if err := applyIdentityForTests(context.Background(), cfg, pgCfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if calls != 0 {
		t.Errorf("IdentityResolver called %d times; expected 0 when user is set in DSN", calls)
	}
	if got, want := pgCfg.ConnConfig.User, "explicit"; got != want {
		t.Errorf("user = %q, want %q", got, want)
	}
}

func TestTokenAsPasswordOnUnixSocket(t *testing.T) {
	dsn := "host=/cloudsql/p:r:i user=sa@p.iam dbname=auth sslmode=disable"
	pgCfg := parseTestPoolCfg(t, dsn)
	calls := 0
	cfg := PoolConfig{
		DSN:              dsn,
		IdentityResolver: func(_ context.Context) (string, error) { return "unused", nil },
		TokenFetcher: func(_ context.Context) (string, error) {
			calls++
			return "iam-token", nil
		},
	}
	if err := applyIdentityForTests(context.Background(), cfg, pgCfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if pgCfg.BeforeConnect == nil {
		t.Fatal("BeforeConnect not installed for unix-socket host with empty password")
	}

	connCfg := *pgCfg.ConnConfig
	if err := pgCfg.BeforeConnect(context.Background(), &connCfg); err != nil {
		t.Fatalf("BeforeConnect: %v", err)
	}
	if got, want := connCfg.Password, "iam-token"; got != want {
		t.Errorf("password = %q, want %q", got, want)
	}
	if calls != 1 {
		t.Errorf("TokenFetcher called %d times, want 1", calls)
	}
}

func TestTokenFetcherRequiredForUnixSocketWithoutPassword(t *testing.T) {
	// Same shape rule as above: /cloudsql/ sockets default to the native GCP
	// token fetcher, so the explicit-fetcher requirement is guarded on a bare
	// Unix socket, where an absent password may mean peer auth.
	_, err := PoolConfig{
		DSN: "host=/var/run/postgresql user=sa@p.iam dbname=auth sslmode=disable",
	}.withDefaults().buildPoolConfig(context.Background())
	if err == nil {
		t.Fatal("expected missing TokenFetcher error")
	}
	if !strings.Contains(err.Error(), "TokenFetcher") {
		t.Errorf("error = %v, want mention TokenFetcher", err)
	}
}

func TestNoTokenFetchOnTcpHost(t *testing.T) {
	dsn := "host=localhost port=6543 user=user@example.com dbname=auth sslmode=disable"
	pgCfg := parseTestPoolCfg(t, dsn)
	calls := 0
	cfg := PoolConfig{
		DSN:              dsn,
		IdentityResolver: func(_ context.Context) (string, error) { return "unused", nil },
		TokenFetcher: func(_ context.Context) (string, error) {
			calls++
			return "iam-token", nil
		},
	}
	if err := applyIdentityForTests(context.Background(), cfg, pgCfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if pgCfg.BeforeConnect != nil {
		t.Error("BeforeConnect should NOT be installed for TCP host with empty password (proxy handles auth)")
	}
	if calls != 0 {
		t.Errorf("TokenFetcher called %d times, want 0", calls)
	}
}

func TestExplicitPasswordPreservedOnUnixSocket(t *testing.T) {
	dsn := "host=/cloudsql/p:r:i user=u password=secret dbname=auth sslmode=disable"
	pgCfg := parseTestPoolCfg(t, dsn)
	calls := 0
	cfg := PoolConfig{
		DSN:              dsn,
		IdentityResolver: func(_ context.Context) (string, error) { return "unused", nil },
		TokenFetcher: func(_ context.Context) (string, error) {
			calls++
			return "iam-token", nil
		},
	}
	if err := applyIdentityForTests(context.Background(), cfg, pgCfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if pgCfg.BeforeConnect != nil {
		t.Error("BeforeConnect should NOT be installed when password is explicit")
	}
	if calls != 0 {
		t.Errorf("TokenFetcher called %d times, want 0", calls)
	}
}

func TestDsnSpecifiesUser(t *testing.T) {
	cases := []struct {
		dsn  string
		want bool
	}{
		{"host=/cloudsql/x dbname=d sslmode=disable", false},
		{"host=/cloudsql/x user=foo dbname=d sslmode=disable", true},
		{"host=/cloudsql/x USER=Foo dbname=d sslmode=disable", true},
		{"postgres://localhost/db", false},
		{"postgres://user@localhost/db", true},
		{"postgres://user:pass@localhost/db", true},
		{"postgresql://localhost:5432/db", false},
		{"", false},
	}
	for _, c := range cases {
		if got := dsnSpecifiesUser(c.dsn); got != c.want {
			t.Errorf("dsnSpecifiesUser(%q) = %v, want %v", c.dsn, got, c.want)
		}
	}
}

func TestDsnSpecifiesPassword(t *testing.T) {
	cases := []struct {
		dsn  string
		want bool
	}{
		{"host=localhost user=u dbname=d sslmode=disable", false},
		{"host=localhost user=u password=secret dbname=d sslmode=disable", true},
		{"host=localhost user=u PASSWORD=Secret dbname=d sslmode=disable", true},
		{"postgres://u@localhost/db", false},
		{"postgres://u:p@localhost/db", true},
		{"postgresql://localhost/db", false},
		{"", false},
	}
	for _, c := range cases {
		if got := dsnSpecifiesPassword(c.dsn); got != c.want {
			t.Errorf("dsnSpecifiesPassword(%q) = %v, want %v", c.dsn, got, c.want)
		}
	}
}

func TestEnvPasswordClearedWhenDsnOmitsIt(t *testing.T) {
	// Simulate pgx's PGPASSWORD env fallback by pre-populating Password on
	// the parsed config. The framework must clear it because the DSN itself
	// did not specify a password.
	dsn := "host=localhost port=6543 user=user@example.com dbname=auth sslmode=disable"
	pgCfg := parseTestPoolCfg(t, dsn)
	pgCfg.ConnConfig.Password = "leaked-from-PGPASSWORD"
	cfg := PoolConfig{
		DSN:              dsn,
		IdentityResolver: func(_ context.Context) (string, error) { return "unused", nil },
		TokenFetcher:     func(_ context.Context) (string, error) { return "tok", nil },
	}
	if err := applyIdentityForTests(context.Background(), cfg, pgCfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if pgCfg.ConnConfig.Password != "" {
		t.Errorf("password = %q, want empty (env-leaked password must be cleared when DSN omits it)", pgCfg.ConnConfig.Password)
	}
	if pgCfg.BeforeConnect != nil {
		t.Error("BeforeConnect should NOT be installed for TCP host with empty password (proxy handles auth)")
	}
}

func TestEnvPasswordPreservedWhenDsnSpecifiesIt(t *testing.T) {
	// When the user explicitly set a password in the DSN, the framework must
	// not touch it — this guards against the cleanup above being too eager.
	dsn := "host=localhost port=6543 user=u password=explicit dbname=d sslmode=disable"
	pgCfg := parseTestPoolCfg(t, dsn)
	cfg := PoolConfig{DSN: dsn}
	if err := applyIdentityForTests(context.Background(), cfg, pgCfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got, want := pgCfg.ConnConfig.Password, "explicit"; got != want {
		t.Errorf("password = %q, want %q", got, want)
	}
}
