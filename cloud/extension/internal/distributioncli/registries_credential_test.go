package distributioncli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	regproto "go.putnami.dev/protocol/registry"
)

// providerAuthServer fakes the auth server's two-step api-key grant that
// MintWorkspaceScopedAuth drives, and records what the mint asked for.
type providerAuthServer struct {
	t             *testing.T
	bearer        string
	keyScopes     []string
	tokenScopes   []string
	tokenClients  []string
	revokedKeys   int
	refuseExpires bool
	// requests lists every request the server answered, as "METHOD /path".
	requests []string
	// answering runs inside each request, before the server answers it. An
	// error drops the answer, like a transport whose request was cut short.
	answering func(request *http.Request) error
}

func (s *providerAuthServer) client() *http.Client {
	return &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
		s.requests = append(s.requests, request.Method+" "+request.URL.Path)
		if s.answering != nil {
			if err := s.answering(request); err != nil {
				return nil, err
			}
		}
		body := map[string]any{}
		if request.Body != nil {
			data, _ := io.ReadAll(request.Body)
			if len(data) > 0 {
				if err := json.Unmarshal(data, &body); err != nil {
					s.t.Fatalf("decode %s: %v", request.URL.Path, err)
				}
			}
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/.well-known/openid-configuration":
			return workflowJSONResponse(http.StatusOK, map[string]any{"issuer": "https://auth.test", "token_endpoint": "https://auth.test/token"}), nil
		case request.Method == http.MethodPost && request.URL.Path == "/apikeys":
			scope, _ := body["allowed_scopes"].(string)
			s.keyScopes = append(s.keyScopes, scope)
			return workflowJSONResponse(http.StatusCreated, workflowAPIKeyCreated("pkt_ephemeral")), nil
		case request.Method == http.MethodDelete && request.URL.Path == "/apikeys/key-1":
			s.revokedKeys++
			return workflowJSONResponse(http.StatusOK, workflowAPIKeyRevoked), nil
		case request.Method == http.MethodPost && request.URL.Path == "/token" && body["grant_type"] == "refresh_token":
			// The sign-in refresh rotates the refresh token.
			return workflowJSONResponse(http.StatusOK, map[string]any{
				"access_token": "session-access-new", "refresh_token": "refresh-new", "token_type": "Bearer", "expires_in": 3600,
			}), nil
		case request.Method == http.MethodPost && request.URL.Path == "/token":
			scope, _ := body["scope"].(string)
			clientID, _ := body["client_id"].(string)
			s.tokenScopes = append(s.tokenScopes, scope)
			s.tokenClients = append(s.tokenClients, clientID)
			response := map[string]any{"access_token": s.bearer, "token_type": "Bearer"}
			if !s.refuseExpires {
				response["expires_in"] = 300
			}
			return workflowJSONResponse(http.StatusOK, response), nil
		}
		s.t.Fatalf("unexpected request %s %s", request.Method, request.URL)
		return nil, nil
	})}
}

func providerCredentialFixture(t *testing.T) (string, map[string]string, clicore.IO, *providerAuthServer) {
	t.Helper()
	root, env, ioctx := distributionWorkflowFixture(t)
	env["PUTNAMI_AUTH_URL"] = "https://auth.test"
	now := ioctx.Now()
	server := &providerAuthServer{
		t: t,
		bearer: workflowJWT(map[string]any{
			"aud":       DefaultRegistryTokenClientID,
			"sub":       "user:user-admin",
			"scope":     "go npm oci put",
			"exp":       now.Add(5 * time.Minute).Unix(),
			"scope_ref": map[string]any{"workspace_id": "ws-consumer"},
		}),
	}
	ioctx.Client = server.client()
	return root, env, ioctx, server
}

func TestProviderReadCredentialMintsOneBearerForEveryRegistryHost(t *testing.T) {
	root, env, ioctx, server := providerCredentialFixture(t)
	env["PUTNAMI_REGISTRY_NPM_URL"] = "https://NPM.Example.Test:8443/"

	credential, err := ProviderReadCredential(context.Background(), map[string]any{}, root, env, ioctx)
	if err != nil {
		t.Fatalf("ProviderReadCredential: %v", err)
	}
	want := &regproto.Credential{
		Bearer:    server.bearer,
		ExpiresAt: "2026-08-22T12:05:00Z",
		Hosts:     []string{"go.putnami.dev", "npm.example.test:8443", "oci.putnami.dev", "put.putnami.dev"},
	}
	if !reflect.DeepEqual(credential, want) {
		t.Fatalf("credential = %+v, want %+v", credential, want)
	}
	if !reflect.DeepEqual(server.keyScopes, []string{"go npm oci put"}) || !reflect.DeepEqual(server.tokenScopes, []string{"go npm oci put"}) {
		t.Fatalf("minted with key scopes %v and token scopes %v, want the four markers once", server.keyScopes, server.tokenScopes)
	}
	if !reflect.DeepEqual(server.tokenClients, []string{DefaultRegistryTokenClientID}) || server.revokedKeys != 1 {
		t.Fatalf("token clients %v, revoked %d", server.tokenClients, server.revokedKeys)
	}

	// The next read inside the bearer's lifetime is served from the cache.
	again, err := ProviderReadCredential(context.Background(), map[string]any{}, root, env, ioctx)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("cached read = %+v, %v", again, err)
	}
	if len(server.tokenScopes) != 1 {
		t.Fatalf("minted %d times, want 1", len(server.tokenScopes))
	}

	// Another set of registry hosts is another cache entry, never this bearer
	// answered for hosts it was not minted for.
	env["PUTNAMI_REGISTRY_OCI_URL"] = "https://oci.other.test"
	other, err := ProviderReadCredential(context.Background(), map[string]any{}, root, env, ioctx)
	if err != nil || other == nil || !strings.Contains(strings.Join(other.Hosts, " "), "oci.other.test") {
		t.Fatalf("read for other hosts = %+v, %v", other, err)
	}
	if len(server.tokenScopes) != 2 {
		t.Fatalf("minted %d times, want a second mint for the other hosts", len(server.tokenScopes))
	}
}

func TestProviderReadCredentialCacheKeyNeverCollidesWithAHost(t *testing.T) {
	for _, host := range []string{"go.putnami.dev", "localhost:8443", "@credential-provider"} {
		if strings.HasPrefix(host, providerReadAccessPrefix) {
			t.Fatalf("host %q shares the provider cache key prefix", host)
		}
	}
	if regproto.ValidCredentialHost(strings.TrimSpace(providerReadAccessPrefix)) {
		t.Fatal("the provider cache key prefix is a valid host")
	}
	if providerReadScope() != "go npm oci put" {
		t.Fatalf("scope = %q", providerReadScope())
	}
}

func TestProviderReadCredentialTakesTheExpiryFromTheBearerWhenTheServerOmitsIt(t *testing.T) {
	root, env, ioctx, server := providerCredentialFixture(t)
	server.refuseExpires = true
	credential, err := ProviderReadCredential(context.Background(), map[string]any{}, root, env, ioctx)
	if err != nil {
		t.Fatalf("ProviderReadCredential: %v", err)
	}
	if credential.ExpiresAt != "2026-08-22T12:05:00Z" {
		t.Fatalf("expiresAt = %q, want the bearer's exp", credential.ExpiresAt)
	}
}

func TestProviderReadCredentialIsAbsentWithoutASignedInLinkedCheckout(t *testing.T) {
	t.Run("not signed in", func(t *testing.T) {
		root, env, ioctx, server := providerCredentialFixture(t)
		if err := clicore.RemoveAuth(env); err != nil {
			t.Fatal(err)
		}
		credential, err := ProviderReadCredential(context.Background(), map[string]any{}, root, env, ioctx)
		if err != nil || credential != nil || len(server.tokenScopes) != 0 {
			t.Fatalf("credential = %+v, err = %v, mints = %d; want absence without a mint", credential, err, len(server.tokenScopes))
		}
	})
	t.Run("not linked", func(t *testing.T) {
		root, env, ioctx, server := providerCredentialFixture(t)
		if err := os.Remove(filepath.Join(root, clicore.LinkFileRelative)); err != nil {
			t.Fatal(err)
		}
		credential, err := ProviderReadCredential(context.Background(), map[string]any{}, root, env, ioctx)
		if err != nil || credential != nil || len(server.tokenScopes) != 0 {
			t.Fatalf("credential = %+v, err = %v, mints = %d; want absence without a mint", credential, err, len(server.tokenScopes))
		}
	})
}

func TestProviderReadCredentialReportsARefusedMint(t *testing.T) {
	root, env, ioctx, server := providerCredentialFixture(t)
	// A bearer without the four markers is a refusal of the combined scope.
	server.bearer = workflowJWT(map[string]any{
		"aud": DefaultRegistryTokenClientID, "scope": "npm",
		"exp": ioctx.Now().Add(5 * time.Minute).Unix(), "scope_ref": map[string]any{"workspace_id": "ws-consumer"},
	})
	credential, err := ProviderReadCredential(context.Background(), map[string]any{}, root, env, ioctx)
	if err == nil || credential != nil || !strings.Contains(err.Error(), "required scopes") {
		t.Fatalf("credential = %+v, err = %v; want the scope refusal", credential, err)
	}
}

// storedProviderReadAccess reports whether registries.json holds a provider
// read bearer.
func storedProviderReadAccess(t *testing.T, env map[string]string) bool {
	t.Helper()
	state, err := readRegistriesState(env)
	if err != nil {
		t.Fatalf("read registries.json: %v", err)
	}
	if state == nil {
		return false
	}
	for key := range state.Access {
		if strings.HasPrefix(key, providerReadAccessPrefix) {
			return true
		}
	}
	return false
}

func TestProviderReadCredentialDoesNothingOnceItsContextEnded(t *testing.T) {
	root, env, ioctx, server := providerCredentialFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	credential, err := ProviderReadCredential(ctx, map[string]any{}, root, env, ioctx)
	if !errors.Is(err, context.Canceled) || credential != nil {
		t.Fatalf("credential = %+v, err = %v; want the context's error", credential, err)
	}
	if len(server.requests) != 0 || storedProviderReadAccess(t, env) {
		t.Fatalf("requests %v, stored %v; want nothing", server.requests, storedProviderReadAccess(t, env))
	}
}

// TestProviderReadCredentialStopsTheMintAndStillRevokesItsKey pins that a mint
// stopped after the key exists sends no exchange, stores nothing, and still
// revokes the key.
func TestProviderReadCredentialStopsTheMintAndStillRevokesItsKey(t *testing.T) {
	root, env, ioctx, server := providerCredentialFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.answering = func(request *http.Request) error {
		if request.URL.Path == "/apikeys" && request.Method == http.MethodPost {
			cancel() // the engine stops waiting while the key is created
		}
		return nil
	}
	credential, err := ProviderReadCredential(ctx, map[string]any{}, root, env, ioctx)
	if err == nil || credential != nil {
		t.Fatalf("credential = %+v, err = %v; want the mint stopped", credential, err)
	}
	if want := []string{"POST /apikeys", "DELETE /apikeys/key-1"}; !reflect.DeepEqual(server.requests, want) {
		t.Fatalf("requests = %v, want %v: no exchange, and the key revoked", server.requests, want)
	}
	if server.revokedKeys != 1 {
		t.Fatalf("revoked %d keys, want 1", server.revokedKeys)
	}
	if storedProviderReadAccess(t, env) {
		t.Fatal("a stopped mint stored a bearer")
	}
}

func TestProviderReadCredentialStoresNothingWhenItsContextEndsAfterTheMint(t *testing.T) {
	root, env, ioctx, server := providerCredentialFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.answering = func(request *http.Request) error {
		if request.URL.Path == "/token" {
			cancel() // the bearer is minted, but nobody waits for it any more
		}
		return nil
	}
	credential, err := ProviderReadCredential(ctx, map[string]any{}, root, env, ioctx)
	if !errors.Is(err, context.Canceled) || credential != nil {
		t.Fatalf("credential = %+v, err = %v; want the context's error", credential, err)
	}
	if storedProviderReadAccess(t, env) {
		t.Fatal("a read whose context ended stored its bearer")
	}
	if server.revokedKeys != 1 {
		t.Fatalf("revoked %d keys, want 1", server.revokedKeys)
	}
}

// TestProviderReadCredentialKeepsTheRotatedSignInWhenItsContextEnds pins that
// the sign-in refresh never follows the read's context. The auth server
// rotates the refresh token and revokes the old one in the same step, so a
// refresh cut short would leave auth.json with a revoked token and sign the
// user out.
func TestProviderReadCredentialKeepsTheRotatedSignInWhenItsContextEnds(t *testing.T) {
	root, env, ioctx, server := providerCredentialFixture(t)
	if err := clicore.WriteAuth(&clicore.StoredToken{
		AccessToken: "session-access", RefreshToken: "refresh-old", TokenType: "Bearer",
		ExpiresAt: ioctx.Now().Add(-time.Minute).Format(time.RFC3339Nano), Issuer: "https://auth.test",
	}, env); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refreshing := true
	server.answering = func(request *http.Request) error {
		if request.URL.Path != "/token" || !refreshing {
			return nil
		}
		refreshing = false
		cancel() // the engine stops waiting while the auth server rotates the token
		select {
		case <-request.Context().Done():
			return context.Cause(request.Context())
		case <-time.After(100 * time.Millisecond):
			return nil
		}
	}
	credential, err := ProviderReadCredential(ctx, map[string]any{}, root, env, ioctx)
	if !errors.Is(err, context.Canceled) || credential != nil {
		t.Fatalf("credential = %+v, err = %v; want the context's error after the refresh", credential, err)
	}
	stored, err := clicore.ReadAuth(env, true)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "refresh-new" || stored.AccessToken != "session-access-new" {
		t.Fatalf("auth.json holds refresh %q and access %q, want the rotated pair", stored.RefreshToken, stored.AccessToken)
	}
	for _, request := range server.requests {
		if request == "POST /apikeys" {
			t.Fatalf("requests = %v: a stopped read created a key", server.requests)
		}
	}
	if storedProviderReadAccess(t, env) {
		t.Fatal("a stopped read stored a bearer")
	}
}

func TestRegistryAccessExpiry(t *testing.T) {
	stored, err := registryAccessExpiry(registryAccessToken{ExpiresAt: "2026-08-22T12:05:00.900+02:00"})
	if err != nil || !stored.Equal(time.Date(2026, time.August, 22, 10, 5, 0, 0, time.UTC)) {
		t.Fatalf("stored expiry = %v, %v", stored, err)
	}
	if _, err := registryAccessExpiry(registryAccessToken{AccessToken: "opaque"}); err == nil {
		t.Fatal("a bearer with no expiry was accepted")
	}
}
