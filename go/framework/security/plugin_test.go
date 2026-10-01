package security

import (
	"context"
	"net/http"
	"testing"
	"time"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
)

func okHandler(_ *phttp.Context) *phttp.Response { return phttp.JSON("ok") }

func statusOf(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // test helper
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test helper
	return resp.StatusCode
}

func TestPlugin_NameAndConstruction(t *testing.T) {
	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	cfg := Config{Issuer: "https://auth.example.com"}
	p := NewPlugin(server, cfg)

	if p == nil {
		t.Fatal("NewPlugin returned nil")
	}
	if p.server != server {
		t.Error("plugin did not retain the given server")
	}
	if p.cfg.Issuer != cfg.Issuer {
		t.Errorf("plugin cfg.Issuer = %q, want %q", p.cfg.Issuer, cfg.Issuer)
	}
	if p.Name() != "security" {
		t.Errorf("Name() = %q, want security", p.Name())
	}
}

func TestPlugin_ConfigureProtectsRoutes(t *testing.T) {
	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	server.GET("/v1/data", okHandler)
	server.GET("/_/health", okHandler)

	p := NewPlugin(server, Config{
		Issuer:       "https://auth.example.com",
		ExcludePaths: []string{"/_/"},
	})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	ts := server.TestServer()
	defer ts.Close()

	if got := statusOf(t, ts.URL+"/v1/data"); got != 401 {
		t.Errorf("unauthenticated protected path status = %d, want 401", got)
	}
	if got := statusOf(t, ts.URL+"/_/health"); got != 200 {
		t.Errorf("excluded path status = %d, want 200", got)
	}
}

func TestPlugin_ConfigureAllowsAuthenticatedRequest(t *testing.T) {
	key := generateRSAKey(t)
	jwks := serveJWKS(t, rsaJWKJSON("key-1", &key.PublicKey))

	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	server.GET("/v1/data", okHandler)

	p := NewPlugin(server, Config{JWKSURL: jwks.URL})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	ts := server.TestServer()
	defer ts.Close()

	token := makeRSAJWT(t, map[string]any{
		"sub": "service-a",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "key-1", key)

	req, err := http.NewRequest("GET", ts.URL+"/v1/data", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test helper

	if resp.StatusCode != 200 {
		t.Errorf("authenticated request status = %d, want 200", resp.StatusCode)
	}
}

func TestPlugin_ConfigureFailsClosedWhenUnconfigured(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "fail-closed-configuration", "unconfigured-plugin-denies-requests")
	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	server.GET("/v1/data", okHandler)
	server.GET("/_/health", okHandler)

	p := NewPlugin(server, Config{ExcludePaths: []string{"/_/"}})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	ts := server.TestServer()
	defer ts.Close()

	if got := statusOf(t, ts.URL+"/v1/data"); got != 401 {
		t.Errorf("unconfigured protected path status = %d, want 401 (must fail closed)", got)
	}
	if got := statusOf(t, ts.URL+"/_/health"); got != 200 {
		t.Errorf("excluded path status = %d, want 200", got)
	}
}

func TestPlugin_ConfigureFailsClosedEvenWithExistingUser(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "fail-closed-configuration", "unconfigured-plugin-denies-even-with-an-existing-user")
	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	server.Use(func(ctx *phttp.Context, next func() *phttp.Response) *phttp.Response {
		ctx.User = &phttp.Claims{Subject: "preauthenticated"}
		return next()
	})
	server.GET("/v1/data", okHandler)

	p := NewPlugin(server, Config{})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	ts := server.TestServer()
	defer ts.Close()

	if got := statusOf(t, ts.URL+"/v1/data"); got != 401 {
		t.Errorf("unconfigured plugin with existing user status = %d, want 401 (must fail closed)", got)
	}
}

func TestPlugin_ConfigureAllowUnauthenticated(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "fail-closed-configuration", "allow-unauthenticated-must-be-explicit")
	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	server.GET("/v1/data", okHandler)

	p := NewPlugin(server, Config{AllowUnauthenticated: true})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	ts := server.TestServer()
	defer ts.Close()

	if got := statusOf(t, ts.URL+"/v1/data"); got != 200 {
		t.Errorf("AllowUnauthenticated status = %d, want 200 (open)", got)
	}
}

func TestPlugin_ConfigureIdempotent(t *testing.T) {
	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	server.GET("/v1/data", okHandler)

	p := NewPlugin(server, Config{Issuer: "https://auth.example.com"})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("first Configure: %v", err)
	}
	if !p.configured {
		t.Fatal("expected configured=true after Configure")
	}
	// A second Configure must be a no-op and not re-apply middleware.
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("second Configure: %v", err)
	}

	ts := server.TestServer()
	defer ts.Close()

	if got := statusOf(t, ts.URL+"/v1/data"); got != 401 {
		t.Errorf("status after repeated Configure = %d, want 401", got)
	}
}
