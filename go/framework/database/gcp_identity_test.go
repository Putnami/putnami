package database

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// fakeGCPMetadata stands in for the GCP metadata server and swaps
// gcpMetadataBase for the test's lifetime.
func fakeGCPMetadata(t *testing.T, email, token string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing Metadata-Flavor header", http.StatusForbidden)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/email"):
			_, _ = w.Write([]byte(email))
		case strings.HasSuffix(r.URL.Path, "/token"):
			_, _ = w.Write([]byte(`{"access_token":"` + token + `","expires_in":3599}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	previous := gcpMetadataBase
	gcpMetadataBase = srv.URL
	t.Cleanup(func() { gcpMetadataBase = previous })
}

// TestBuildPoolConfig_CloudSQLSocketDefaultsToNativeGCPIdentity proves a Cloud
// SQL socket with no user and no password needs no explicit hooks: the pool
// resolves the workload identity and per-connection IAM token natively. This is
// the seam that lets a workload reach Cloud SQL without importing a private
// platform module.
func TestBuildPoolConfig_CloudSQLSocketDefaultsToNativeGCPIdentity(t *testing.T) {
	fakeGCPMetadata(t, "workload@project.iam.gserviceaccount.com", "iam-access-token")

	cfg, err := PoolConfig{
		DSN: "host=/cloudsql/project:region:instance dbname=telemetry",
	}.withDefaults().buildPoolConfig(context.Background())
	if err != nil {
		t.Fatalf("buildPoolConfig: %v", err)
	}

	if got, want := cfg.ConnConfig.User, "workload@project.iam"; got != want {
		t.Errorf("resolved user = %q, want %q (metadata email, service-account suffix trimmed)", got, want)
	}
	if cfg.BeforeConnect == nil {
		t.Fatal("BeforeConnect is nil: the native IAM token fetcher was not wired")
	}
	conn := &pgx.ConnConfig{}
	if err := cfg.BeforeConnect(context.Background(), conn); err != nil {
		t.Fatalf("BeforeConnect: %v", err)
	}
	if conn.Password != "iam-access-token" {
		t.Errorf("per-connection password = %q, want the metadata access token", conn.Password)
	}
}

// TestBuildPoolConfig_ExplicitHooksWinOverNativeGCP proves the auto-wire is a
// default, not an override: a caller-supplied resolver/fetcher is used as-is
// even on a Cloud SQL socket.
func TestBuildPoolConfig_ExplicitHooksWinOverNativeGCP(t *testing.T) {
	fakeGCPMetadata(t, "native@project.iam.gserviceaccount.com", "native-token")

	cfg, err := PoolConfig{
		DSN:              "host=/cloudsql/project:region:instance dbname=telemetry",
		IdentityResolver: func(context.Context) (string, error) { return "explicit@example.com", nil },
		TokenFetcher:     func(context.Context) (string, error) { return "explicit-token", nil },
	}.withDefaults().buildPoolConfig(context.Background())
	if err != nil {
		t.Fatalf("buildPoolConfig: %v", err)
	}
	if got := cfg.ConnConfig.User; got != "explicit@example.com" {
		t.Errorf("resolved user = %q, want the explicit resolver's answer", got)
	}
	conn := &pgx.ConnConfig{}
	if err := cfg.BeforeConnect(context.Background(), conn); err != nil {
		t.Fatalf("BeforeConnect: %v", err)
	}
	if conn.Password != "explicit-token" {
		t.Errorf("per-connection password = %q, want the explicit fetcher's answer", conn.Password)
	}
}

// TestBuildPoolConfig_PlainUnixSocketStillRequiresExplicitHooks proves the
// native default is scoped to the /cloudsql mount: a bare Unix socket may mean
// peer auth, so an absent user or password there stays a loud configuration
// error instead of a surprise metadata fetch.
func TestBuildPoolConfig_PlainUnixSocketStillRequiresExplicitHooks(t *testing.T) {
	fakeGCPMetadata(t, "native@project.iam.gserviceaccount.com", "native-token")

	_, err := PoolConfig{
		DSN: "host=/var/run/postgresql dbname=telemetry",
	}.withDefaults().buildPoolConfig(context.Background())
	if err == nil {
		t.Fatal("expected an error for a non-Cloud-SQL socket without user/hooks, got nil")
	}
	if !strings.Contains(err.Error(), "IdentityResolver") {
		t.Errorf("error %q does not name the missing IdentityResolver", err)
	}
}

// TestGCPIdentityResolver_MetadataThenError proves the resolver reports every
// attempted path when nothing answers, so the failure is self-diagnosing.
func TestGCPIdentityResolver_MetadataThenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no metadata here", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	previous := gcpMetadataBase
	gcpMetadataBase = srv.URL
	t.Cleanup(func() { gcpMetadataBase = previous })
	t.Setenv("PATH", t.TempDir()) // no gcloud on PATH either

	_, err := gcpIdentityResolver(context.Background())
	if err == nil {
		t.Fatal("expected an error with no metadata server and no gcloud")
	}
	for _, want := range []string{"metadata", "gcloud"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention the attempted %s path", err, want)
		}
	}
}

func TestIsCloudSQLSocket(t *testing.T) {
	cases := map[string]bool{
		"/cloudsql/project:region:instance": true,
		"/cloudsql/":                        true,
		"/var/run/postgresql":               false,
		"/cloudsql":                         false, // the mount dir itself is not an instance socket
		"localhost":                         false,
		"":                                  false,
	}
	for host, want := range cases {
		if got := isCloudSQLSocket(host); got != want {
			t.Errorf("isCloudSQLSocket(%q) = %v, want %v", host, got, want)
		}
	}
}
