package remotecache

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenSource_EnvOverrideWins(t *testing.T) {
	t.Setenv(TokenEnv, "env-token")
	// Even with a command configured, the env override takes precedence.
	got, err := TokenSource{Command: []string{"false"}}.Resolve(context.Background())
	if err != nil || got != "env-token" {
		t.Fatalf("Resolve = %q, %v; want env-token", got, err)
	}
}

func TestTokenSource_Command(t *testing.T) {
	t.Setenv(TokenEnv, "")
	script := filepath.Join(t.TempDir(), "tok.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf %s 'cmd-token'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := TokenSource{Command: []string{script}}.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "cmd-token" {
		t.Errorf("token = %q, want cmd-token", got)
	}
}

func TestTokenSource_CommandFailureSurfacesSafeAuthRecovery(t *testing.T) {
	t.Setenv(TokenEnv, "")
	command := filepath.Join(t.TempDir(), "putnami")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '%s\\n' 'stdout-is-a-bearer'\nprintf '%s\\n' 'Invalid client credentials: pkt_super_secret' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := (TokenSource{Command: []string{command, "cloud", "token", "--for", "cache"}}).Resolve(context.Background())
	if err == nil {
		t.Fatal("Resolve succeeded for a failing token command")
	}
	message := err.Error()
	for _, want := range []string{"exit status 3", "diagnostic: Invalid client credentials", "putnami cloud login", "putnami cloud setup"} {
		if !strings.Contains(message, want) {
			t.Errorf("error %q does not contain %q", message, want)
		}
	}
	for _, secret := range []string{"stdout-is-a-bearer", "pkt_super_secret"} {
		if strings.Contains(message, secret) {
			t.Errorf("error leaked token-command secret %q: %s", secret, message)
		}
	}
	var commandErr *TokenCommandError
	if !errors.As(err, &commandErr) {
		t.Fatalf("error type = %T, want *TokenCommandError", err)
	}
}

func TestTokenSource_CommandFailureWithEmptyStderrKeepsExitCause(t *testing.T) {
	t.Setenv(TokenEnv, "")
	_, err := (TokenSource{Command: []string{"sh", "-c", "exit 7"}}).Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("Resolve error = %v, want exit status without a fabricated diagnostic", err)
	}
	if strings.Contains(err.Error(), "diagnostic:") {
		t.Fatalf("empty stderr produced a diagnostic: %v", err)
	}
}

func TestTokenSource_CommandTimeoutIsExplicit(t *testing.T) {
	t.Setenv(TokenEnv, "")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := (TokenSource{Command: []string{"sh", "-c", "while :; do :; done"}}).Resolve(ctx)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Resolve error = %v, want a deadline-preserving timeout", err)
	}
}

func TestTokenSource_CommandFailureRedactsUnrecognizedStderr(t *testing.T) {
	t.Setenv(TokenEnv, "")
	const secret = "pkt_only_secret_value"
	_, err := (TokenSource{Command: []string{"sh", "-c", "printf '%s\\n' '" + secret + "' >&2; exit 4"}}).Resolve(context.Background())
	if err == nil {
		t.Fatal("Resolve succeeded for a failing token command")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked unrecognized stderr: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted: unrecognized token-command stderr]") {
		t.Fatalf("Resolve error = %v, want an explicit stderr redaction", err)
	}
}

func TestTokenSource_URL(t *testing.T) {
	t.Setenv(TokenEnv, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("url-token\n"))
	}))
	defer srv.Close()

	got, err := TokenSource{URL: srv.URL}.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "url-token" {
		t.Errorf("token = %q, want url-token (trimmed)", got)
	}
}

func TestTokenSource_URLErrorStatus(t *testing.T) {
	t.Setenv(TokenEnv, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := (TokenSource{URL: srv.URL}).Resolve(context.Background()); err == nil {
		t.Fatal("expected an error for a non-2xx token endpoint")
	}
}

func TestTokenSource_Unconfigured(t *testing.T) {
	t.Setenv(TokenEnv, "")
	got, err := TokenSource{}.Resolve(context.Background())
	if err != nil || got != "" {
		t.Fatalf("Resolve = %q, %v; want empty token, no error", got, err)
	}
}

func TestTokenSource_MetadataFallback(t *testing.T) {
	t.Setenv(TokenEnv, "")
	var gotAudience string
	source := TokenSource{
		Audience: "https://cache.putnami.cloud",
		metadataToken: func(_ context.Context, audience string) (string, error) {
			gotAudience = audience
			return "metadata-id-token", nil
		},
	}
	got, err := source.Resolve(context.Background())
	if err != nil || got != "metadata-id-token" {
		t.Fatalf("Resolve = %q, %v; want metadata-id-token", got, err)
	}
	if gotAudience != source.Audience {
		t.Fatalf("metadata audience = %q, want %q", gotAudience, source.Audience)
	}
}

func TestTokenSource_ExplicitTokenWinsOverMetadata(t *testing.T) {
	t.Setenv(TokenEnv, "explicit-token")
	called := false
	source := TokenSource{
		Audience: "https://cache.putnami.cloud",
		metadataToken: func(context.Context, string) (string, error) {
			called = true
			return "metadata-id-token", nil
		},
	}
	got, err := source.Resolve(context.Background())
	if err != nil || got != "explicit-token" || called {
		t.Fatalf("Resolve = %q, %v; metadata called=%v", got, err, called)
	}
}

func TestTokenSource_MetadataUnavailableFailsClearly(t *testing.T) {
	t.Setenv(TokenEnv, "")
	source := TokenSource{
		Audience: "https://cache.putnami.cloud",
		metadataToken: func(context.Context, string) (string, error) {
			return "", context.DeadlineExceeded
		},
	}
	if _, err := source.Resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "cache token metadata source") {
		t.Fatalf("Resolve error = %v, want clear metadata-source error", err)
	}
}

func TestFetchMetadataIDToken(t *testing.T) {
	var gotAudience, gotFormat, gotFlavor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAudience = r.URL.Query().Get("audience")
		gotFormat = r.URL.Query().Get("format")
		gotFlavor = r.Header.Get("Metadata-Flavor")
		_, _ = w.Write([]byte("metadata-jwt\n"))
	}))
	defer srv.Close()

	got, err := fetchMetadataIDTokenFrom(context.Background(), "https://cache.putnami.cloud", srv.URL, srv.Client())
	if err != nil || got != "metadata-jwt" {
		t.Fatalf("fetchMetadataIDTokenFrom = %q, %v", got, err)
	}
	if gotAudience != "https://cache.putnami.cloud" || gotFormat != "full" || gotFlavor != "Google" {
		t.Fatalf("metadata request audience/format/flavor = %q/%q/%q", gotAudience, gotFormat, gotFlavor)
	}
}

func TestFetchMetadataIDToken_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	if _, err := fetchMetadataIDTokenFrom(context.Background(), "aud", srv.URL, srv.Client()); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("error = %v, want HTTP 403", err)
	}
}
