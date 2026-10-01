package database

import (
	"context"
	"testing"
)

func TestConnectionDSN_CloudSQLSocket(t *testing.T) {
	dsn, err := Connection{Instance: "proj:region:inst"}.dsn("platform")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	if want := "host=/cloudsql/proj:region:inst dbname=platform"; dsn != want {
		t.Fatalf("dsn = %q, want %q", dsn, want)
	}
	// IAM connection: no user/password, so NewPool must resolve the principal
	// and (on a socket) fetch a token. The built DSN must drive both.
	if dsnSpecifiesUser(dsn) {
		t.Error("socket DSN without user should not specify a user")
	}
	if dsnSpecifiesPassword(dsn) {
		t.Error("socket DSN without password should not specify a password")
	}
	pgCfg := parseTestPoolCfg(t, dsn)
	if got := pgCfg.ConnConfig.Host; got != "/cloudsql/proj:region:inst" {
		t.Errorf("parsed host = %q", got)
	}
	if got := pgCfg.ConnConfig.Database; got != "platform" {
		t.Errorf("parsed database = %q", got)
	}
}

func TestConnectionDSN_TCPWithCredentials(t *testing.T) {
	dsn, err := Connection{
		Host:     "db.example.com",
		Port:     6543,
		User:     "app",
		Password: "s3cret",
	}.dsn("platform")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	if want := "host=db.example.com dbname=platform port=6543 user=app password=s3cret"; dsn != want {
		t.Fatalf("dsn = %q, want %q", dsn, want)
	}
	if !dsnSpecifiesUser(dsn) {
		t.Error("DSN with user should specify a user")
	}
	if !dsnSpecifiesPassword(dsn) {
		t.Error("DSN with password should specify a password")
	}
	pgCfg := parseTestPoolCfg(t, dsn)
	if got := pgCfg.ConnConfig.Port; got != 6543 {
		t.Errorf("parsed port = %d, want 6543", got)
	}
	if got := pgCfg.ConnConfig.User; got != "app" {
		t.Errorf("parsed user = %q", got)
	}
}

func TestConnectionDSN_HostBeatsInstanceAndPortOmittedWhenZero(t *testing.T) {
	dsn, err := Connection{Instance: "proj:region:inst", Host: "127.0.0.1"}.dsn("d")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	if want := "host=127.0.0.1 dbname=d"; dsn != want {
		t.Fatalf("dsn = %q, want %q", dsn, want)
	}
}

func TestConnectionDSN_PortIgnoredOnSocket(t *testing.T) {
	dsn, err := Connection{Instance: "p:r:i", Port: 6543}.dsn("d")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	if want := "host=/cloudsql/p:r:i dbname=d"; dsn != want {
		t.Fatalf("dsn = %q, want %q (port must be dropped for a socket host)", dsn, want)
	}
}

func TestConnectionDSN_ParamsSortedAndReservedFiltered(t *testing.T) {
	dsn, err := Connection{
		Host: "h",
		Params: map[string]string{
			"sslmode":          "require",
			"application_name": "svc",
			"dbname":           "ignored", // reserved: dedicated field owns it
			"USER":             "ignored", // reserved (case-insensitive)
		},
	}.dsn("d")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	if want := "host=h dbname=d application_name=svc sslmode=require"; dsn != want {
		t.Fatalf("dsn = %q, want %q", dsn, want)
	}
}

func TestConnectionDSN_QuotesValues(t *testing.T) {
	dsn, err := Connection{Host: "h", Password: `p a'\b`}.dsn("d")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	if want := `host=h dbname=d password='p a\'\\b'`; dsn != want {
		t.Fatalf("dsn = %q, want %q", dsn, want)
	}
	// The quoted DSN must still parse and recover the original value.
	pgCfg := parseTestPoolCfg(t, dsn)
	if got := pgCfg.ConnConfig.Password; got != `p a'\b` {
		t.Errorf("parsed password = %q, want %q", got, `p a'\b`)
	}
}

func TestConnectionDSN_Errors(t *testing.T) {
	if _, err := (Connection{User: "u"}).dsn("d"); err == nil {
		t.Error("expected error when neither Host nor Instance is set")
	}
	if _, err := (Connection{Host: "h"}).dsn("  "); err == nil {
		t.Error("expected error when database name is empty")
	}
}

func TestQuoteDSNValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"plain", "plain"},
		{"proj:region:inst", "proj:region:inst"},
		{"with space", "'with space'"},
		{`back\slash`, `'back\\slash'`},
		{"quote's", `'quote\'s'`},
	}
	for _, c := range cases {
		if got := quoteDSNValue(c.in); got != c.want {
			t.Errorf("quoteDSNValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPoolConfigResolveDSN(t *testing.T) {
	t.Run("explicit DSN wins outright over Connection", func(t *testing.T) {
		cfg := PoolConfig{
			DSN:        "postgres://u:p@localhost/explicit",
			Connection: Connection{Instance: "p:r:i"},
			Datasource: Datasource{Name: "platform", Schema: "public"},
		}
		dsn, err := cfg.resolveDSN()
		if err != nil {
			t.Fatalf("resolveDSN: %v", err)
		}
		if dsn != "postgres://u:p@localhost/explicit" {
			t.Errorf("dsn = %q, want the explicit DSN", dsn)
		}
	})

	t.Run("builds from Connection using the declared datasource name", func(t *testing.T) {
		cfg := PoolConfig{
			Connection: Connection{Instance: "p:r:i"},
			Datasource: Datasource{Name: "platform", Schema: "public"},
		}
		dsn, err := cfg.resolveDSN()
		if err != nil {
			t.Fatalf("resolveDSN: %v", err)
		}
		if want := "host=/cloudsql/p:r:i dbname=platform"; dsn != want {
			t.Errorf("dsn = %q, want %q", dsn, want)
		}
	})

	t.Run("falls back to the legacy Database name", func(t *testing.T) {
		cfg := PoolConfig{
			Connection: Connection{Host: "h"},
			Database:   "legacy",
		}
		dsn, err := cfg.resolveDSN()
		if err != nil {
			t.Fatalf("resolveDSN: %v", err)
		}
		if want := "host=h dbname=legacy"; dsn != want {
			t.Errorf("dsn = %q, want %q", dsn, want)
		}
	})

	t.Run("errors when neither DSN nor Connection is set", func(t *testing.T) {
		if _, err := (PoolConfig{}).resolveDSN(); err == nil {
			t.Error("expected error for an empty PoolConfig")
		}
	})

	t.Run("errors when Connection is set without a database name", func(t *testing.T) {
		if _, err := (PoolConfig{Connection: Connection{Instance: "p:r:i"}}).resolveDSN(); err == nil {
			t.Error("expected error when no datasource name or Database is available")
		}
	})
}

func TestNewPool_BuildsConnectionDSNAndResolvesIdentity(t *testing.T) {
	// No live database: a Cloud SQL socket host fails the ping, but reaching
	// the ping proves the Connection-built DSN parsed and the identity
	// resolver ran (it must, since no user was supplied).
	called := false
	_, err := NewPool(context.Background(), PoolConfig{
		Connection: Connection{Instance: "p:r:i"},
		Datasource: Datasource{Name: "platform", Schema: "public"},
		IdentityResolver: func(context.Context) (string, error) {
			called = true
			return "svc@p.iam.gserviceaccount.com", nil
		},
		TokenFetcher: func(context.Context) (string, error) { return "tok", nil },
	})
	if err == nil {
		t.Fatal("expected a connection error against the non-existent socket")
	}
	if !called {
		t.Error("identity resolver should run for a Connection DSN without an explicit user")
	}
}

func TestDatasourceValidate(t *testing.T) {
	if err := (Datasource{Name: "platform", Schema: "public"}).Validate(); err != nil {
		t.Errorf("complete datasource should validate, got %v", err)
	}
	for _, ds := range []Datasource{{Name: "platform"}, {Schema: "public"}, {}} {
		if err := ds.Validate(); err == nil {
			t.Errorf("incomplete datasource %+v should not validate", ds)
		}
	}
}

func TestDatasourceIsZero(t *testing.T) {
	if !(Datasource{}).isZero() {
		t.Error("empty datasource should be zero")
	}
	if (Datasource{Name: "x"}).isZero() {
		t.Error("datasource with a name is not zero")
	}
	if (Datasource{Schema: "y"}).isZero() {
		t.Error("datasource with a schema is not zero")
	}
}
