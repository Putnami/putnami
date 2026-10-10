package identitycli

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// This file holds the shared fixtures the identity-domain command tests build
// on: an unsigned-JWT minter, a fake OIDC/OAuth2 server routed through the IO
// HTTP client, a stdout/stderr-capturing IO bundle, and a fixed clock. They
// keep the Login/WhoAmI tests hermetic — no real network, no wall-clock sleeps
// beyond the deterministic OAuth polling budget the tests pin explicitly.

// fixedNow is the deterministic clock the identity tests share.
func fixedNow() time.Time {
	return time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
}

// base64url encodes value as a raw-url-safe base64 JSON segment.
func base64url(t *testing.T, value map[string]any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal jwt segment: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

// jwt builds an unsigned JWT carrying payload. The CLI only reads display/scope
// claims, so an alg=none token is exactly what DecodeJWT expects.
func jwt(t *testing.T, payload map[string]any) string {
	t.Helper()
	return base64url(t, map[string]any{"alg": "none", "typ": "JWT"}) + "." + base64url(t, payload) + ".sig"
}

// authServer is a fake OIDC/OAuth2 endpoint set. Each route is a swappable
// handler so individual tests script the device/token/userinfo responses they
// need. Discovery deliberately 404s so AuthEndpoints falls back to the
// conventional /device/authorize, /token, and /userinfo paths.
type authServer struct {
	srv          *httptest.Server
	deviceFn     http.HandlerFunc
	tokenFn      http.HandlerFunc
	userinfoFn   http.HandlerFunc
	mu           sync.Mutex
	deviceHits   int
	tokenHits    int
	userinfoHits int
}

func newAuthServer(t *testing.T) *authServer {
	t.Helper()
	a := &authServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/device/authorize", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.deviceHits++
		fn := a.deviceFn
		a.mu.Unlock()
		fn(w, r)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.tokenHits++
		fn := a.tokenFn
		a.mu.Unlock()
		fn(w, r)
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.userinfoHits++
		fn := a.userinfoFn
		a.mu.Unlock()
		fn(w, r)
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

// decodeRequestBody decodes an incoming JSON request body into a map for
// assertions on the outgoing request shape.
func decodeRequestBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	defer func() { _ = r.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return body
}

// writeJSON is the default success responder used by most routes.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	data, _ := json.Marshal(body)
	_, _ = w.Write(data)
}

// captureIO builds an IO wired to the fake server, capturing every sink so
// tests can assert on emitted lines. home is the PUTNAMI_HOME the credential
// write/read paths resolve against.
func captureIO(t *testing.T, srv *httptest.Server, home string) (clicore.IO, map[string]string, *[]string, *[]string) {
	t.Helper()
	stdout := &[]string{}
	stderr := &[]string{}
	env := map[string]string{
		"PUTNAMI_HOME":     home,
		"PUTNAMI_AUTH_URL": srv.URL,
		"NO_COLOR":         "1",
	}
	ioctx := clicore.IO{
		Env:    env,
		Client: srv.Client(),
		Now:    fixedNow,
		Stdout: func(s string) { *stdout = append(*stdout, s) },
		Stderr: func(s string) { *stderr = append(*stderr, s) },
	}
	return ioctx, env, stdout, stderr
}

// writeAuthFile persists a StoredToken at PUTNAMI_HOME/auth.json so ReadAuth
// resolves it. It mirrors WriteAuth's on-disk layout.
func writeAuthFile(t *testing.T, home string, auth clicore.StoredToken) {
	t.Helper()
	if err := clicore.WriteAuth(&auth, map[string]string{"PUTNAMI_HOME": home}); err != nil {
		t.Fatalf("write auth: %v", err)
	}
}

// freshExpiry is an RFC3339Nano timestamp comfortably past the 30s freshness
// window relative to fixedNow.
func freshExpiry() string {
	return fixedNow().Add(time.Hour).UTC().Format(time.RFC3339Nano)
}

// authPath returns the on-disk credential path for assertions.
func authPath(home string) string {
	return filepath.Join(home, "auth.json")
}
