package clicore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	mintTestWorkspace = "ws-mint"
	mintTestClient    = "distribution"
	mintTestScope     = "go npm oci put"
)

var mintTestNow = time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)

func mintTestJWT(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(claims)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

// mintAuthServer fakes the auth server's refresh grant and its two-step
// api-key grant, and records every request as "METHOD /path".
type mintAuthServer struct {
	t        *testing.T
	requests []string
	// cancelOn ends the caller's context while the server answers that
	// request ("METHOD /path"), like an engine that stops waiting mid-request.
	cancelOn string
	cancel   context.CancelFunc
	// createStatus and createAnswer replace the key create's 201 answer.
	createStatus int
	createAnswer map[string]any
	// revokeStatus answers the key revoke; 0 means auth-server's 200.
	revokeStatus int
	// keyRequest and revokeAuthorization record what the key create sent and
	// the bearer the revoke presented.
	keyAuthorization    string
	keyRequest          map[string]any
	revokeAuthorization string
	// revokeCtxErr and revokeDeadline record the revoke's request context.
	revokeCtxErr   error
	revokeDeadline time.Duration
}

func (s *mintAuthServer) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(s.roundTrip)}
}

func (s *mintAuthServer) roundTrip(request *http.Request) (*http.Response, error) {
	call := request.Method + " " + request.URL.Path
	s.requests = append(s.requests, call)
	if call == s.cancelOn {
		s.cancel()
		// A real transport drops the answer of a request whose context ends
		// while the server answers it.
		select {
		case <-request.Context().Done():
			return nil, context.Cause(request.Context())
		case <-time.After(100 * time.Millisecond):
		}
	}
	body := map[string]any{}
	if request.Body != nil {
		data, _ := io.ReadAll(request.Body)
		if len(data) > 0 {
			if err := json.Unmarshal(data, &body); err != nil {
				s.t.Errorf("decode %s: %v", call, err)
			}
		}
	}
	answer := func(status int, payload any) (*http.Response, error) {
		data, _ := json.Marshal(payload)
		return responseWith(status, string(data)), nil
	}
	switch call {
	case "GET /.well-known/openid-configuration":
		return answer(http.StatusOK, map[string]any{"issuer": "https://auth.test", "token_endpoint": "https://auth.test/token"})
	case "POST /apikeys":
		s.keyAuthorization, s.keyRequest = request.Header.Get("Authorization"), body
		if s.createStatus != 0 {
			return answer(s.createStatus, s.createAnswer)
		}
		return answer(http.StatusCreated, createdAPIKeyAnswer("key-1", "pkt_ephemeral"))
	case "DELETE /apikeys/key-1":
		s.revokeAuthorization = request.Header.Get("Authorization")
		s.revokeCtxErr = request.Context().Err()
		if deadline, ok := request.Context().Deadline(); ok {
			s.revokeDeadline = time.Until(deadline)
		}
		if s.revokeStatus != 0 {
			return answer(s.revokeStatus, map[string]any{"error": "not_found", "error_description": "api key not found"})
		}
		return answer(http.StatusOK, map[string]any{"revoked": true})
	case "POST /token":
		if body["grant_type"] == "refresh_token" {
			return answer(http.StatusOK, map[string]any{
				"access_token": "session-access-new", "refresh_token": "refresh-new", "token_type": "Bearer", "expires_in": 3600,
			})
		}
		return answer(http.StatusOK, map[string]any{
			"token_type": "Bearer", "expires_in": 300,
			"access_token": mintTestJWT(map[string]any{
				"aud": mintTestClient, "scope": mintTestScope,
				"exp": mintTestNow.Add(5 * time.Minute).Unix(), "scope_ref": map[string]any{"workspace_id": mintTestWorkspace},
			}),
		})
	}
	s.t.Errorf("unexpected request %s", call)
	return answer(http.StatusNotFound, map[string]any{})
}

// mintFixture signs a user in on a fresh home. A stale sign-in makes the mint
// refresh it first.
func mintFixture(t *testing.T, stale bool) (map[string]string, IO, *mintAuthServer, *[]string) {
	t.Helper()
	env := map[string]string{"PUTNAMI_HOME": t.TempDir(), "PUTNAMI_AUTH_URL": "https://auth.test"}
	expiresAt := mintTestNow.Add(time.Hour)
	if stale {
		expiresAt = mintTestNow.Add(-time.Minute)
	}
	if err := WriteAuth(&StoredToken{
		AccessToken: "session-access", RefreshToken: "refresh-old", TokenType: "Bearer",
		ExpiresAt: expiresAt.Format(time.RFC3339Nano), Issuer: "https://auth.test", ClientID: "putnami-cli",
	}, env); err != nil {
		t.Fatal(err)
	}
	server := &mintAuthServer{t: t}
	var warnings []string
	ioctx := IO{
		Env: env, Client: server.client(), Now: func() time.Time { return mintTestNow },
		Stderr: func(line string) { warnings = append(warnings, line) },
	}
	return env, ioctx, server, &warnings
}

func TestMintWorkspaceScopedAuthContextMintsAndRevokesTheKey(t *testing.T) {
	env, ioctx, server, warnings := mintFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	minted, err := MintWorkspaceScopedAuthContext(ctx, nil, env, ioctx, mintTestWorkspace, mintTestClient, mintTestScope)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if ClaimedWorkspaceID(DecodeJWT(minted.AccessToken)) != mintTestWorkspace {
		t.Fatalf("minted %+v", minted)
	}
	want := []string{"POST /apikeys", "GET /.well-known/openid-configuration", "POST /token", "DELETE /apikeys/key-1"}
	if !slices.Equal(server.requests, want) {
		t.Fatalf("requests = %v, want %v", server.requests, want)
	}
	if len(*warnings) != 0 {
		t.Fatalf("warnings = %v", *warnings)
	}
}

// TestMintWorkspaceScopedAuthContextKeepsTheRotatedRefreshToken pins that the
// sign-in refresh ignores the mint's context: the auth server rotates the
// refresh token and revokes the old one in the same step, so the rotated
// token must reach auth.json even when the caller stops waiting meanwhile.
func TestMintWorkspaceScopedAuthContextKeepsTheRotatedRefreshToken(t *testing.T) {
	env, ioctx, server, _ := mintFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.cancelOn, server.cancel = "POST /token", cancel

	minted, err := MintWorkspaceScopedAuthContext(ctx, nil, env, ioctx, mintTestWorkspace, mintTestClient, mintTestScope)
	if !errors.Is(err, context.Canceled) || minted != nil {
		t.Fatalf("mint = %+v, %v; want the context's error after the refresh", minted, err)
	}
	stored, err := ReadAuth(env, true)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "refresh-new" || stored.AccessToken != "session-access-new" {
		t.Fatalf("auth.json holds refresh %q and access %q, want the rotated pair", stored.RefreshToken, stored.AccessToken)
	}
	if slices.Contains(server.requests, "POST /apikeys") {
		t.Fatalf("requests = %v: a stopped mint created a key", server.requests)
	}
}

// TestMintWorkspaceScopedAuthContextRevokesTheKeyOfAStoppedMint pins that a
// mint stopped after the key exists sends no exchange and still revokes the
// key, on a request its context cannot end, within apiKeyRevokeTimeout.
func TestMintWorkspaceScopedAuthContextRevokesTheKeyOfAStoppedMint(t *testing.T) {
	env, ioctx, server, warnings := mintFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The caller stops waiting right after the key's answer arrives, so the
	// mint knows the key and stops before the exchange.
	ioctx.Client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response, err := server.roundTrip(request)
		if request.Method == http.MethodPost && request.URL.Path == "/apikeys" {
			cancel()
		}
		return response, err
	})}

	minted, err := MintWorkspaceScopedAuthContext(ctx, nil, env, ioctx, mintTestWorkspace, mintTestClient, mintTestScope)
	if err == nil || minted != nil {
		t.Fatalf("mint = %+v, %v; want it stopped", minted, err)
	}
	if want := []string{"POST /apikeys", "DELETE /apikeys/key-1"}; !slices.Equal(server.requests, want) {
		t.Fatalf("requests = %v, want %v", server.requests, want)
	}
	if server.revokeCtxErr != nil {
		t.Fatalf("the revoke ran on an ended context: %v", server.revokeCtxErr)
	}
	if server.revokeDeadline <= 0 || server.revokeDeadline > apiKeyRevokeTimeout {
		t.Fatalf("revoke deadline in %s, want within %s", server.revokeDeadline, apiKeyRevokeTimeout)
	}
	if len(*warnings) != 0 {
		t.Fatalf("warnings = %v", *warnings)
	}
}

// TestMintWorkspaceScopedAuthContextSendsTheKeyRequestAReleasedServerAccepts
// pins the wire of the generated create and revoke: the same method, path,
// body members and user bearer the hand-written requests sent.
func TestMintWorkspaceScopedAuthContextSendsTheKeyRequestAReleasedServerAccepts(t *testing.T) {
	env, ioctx, server, _ := mintFixture(t, false)
	if _, err := MintWorkspaceScopedAuth(nil, env, ioctx, mintTestWorkspace, mintTestClient, mintTestScope); err != nil {
		t.Fatalf("mint: %v", err)
	}
	want := map[string]any{
		"name":           "cli:" + mintTestClient + ":" + mintTestWorkspace,
		"allowed_scopes": mintTestScope,
		"workspace_id":   mintTestWorkspace,
		"expires_at":     mintTestNow.Add(10 * time.Minute).Format(time.RFC3339Nano),
	}
	if !maps.Equal(server.keyRequest, want) {
		t.Fatalf("key request = %v, want %v", server.keyRequest, want)
	}
	if server.keyAuthorization != "Bearer session-access" || server.revokeAuthorization != "Bearer session-access" {
		t.Fatalf("bearers = %q and %q, want the session's", server.keyAuthorization, server.revokeAuthorization)
	}
}

func TestMintWorkspaceScopedAuthContextMapsARefusedKeyToExitAuth(t *testing.T) {
	env, ioctx, server, _ := mintFixture(t, false)
	server.createStatus = http.StatusUnauthorized
	server.createAnswer = map[string]any{"error": "invalid_token", "error_description": "token expired"}
	_, err := MintWorkspaceScopedAuth(nil, env, ioctx, mintTestWorkspace, mintTestClient, mintTestScope)
	if ExitCode(err) != ExitAuth || !strings.Contains(err.Error(), "request failed for https://auth.test/apikeys: token expired") {
		t.Fatalf("mint = %v (exit %d), want an ExitAuth refusal naming the key endpoint", err, ExitCode(err))
	}
	if want := []string{"POST /apikeys"}; !slices.Equal(server.requests, want) {
		t.Fatalf("requests = %v, want %v", server.requests, want)
	}
}

func TestMintWorkspaceScopedAuthContextSurfacesTheProviderMessage(t *testing.T) {
	env, ioctx, server, _ := mintFixture(t, false)
	server.createStatus = http.StatusBadRequest
	server.createAnswer = map[string]any{"error": "invalid_request", "message": "allowed_scopes exceeds the session"}
	_, err := MintWorkspaceScopedAuth(nil, env, ioctx, mintTestWorkspace, mintTestClient, mintTestScope)
	if ExitCode(err) != ExitAPI || !strings.Contains(err.Error(), "allowed_scopes exceeds the session") {
		t.Fatalf("mint = %v (exit %d), want the provider's message", err, ExitCode(err))
	}
}

func TestMintWorkspaceScopedAuthContextRefusesAnIncompleteKey(t *testing.T) {
	env, ioctx, server, _ := mintFixture(t, false)
	server.createStatus = http.StatusCreated
	server.createAnswer = createdAPIKeyAnswer("key-1", "")
	_, err := MintWorkspaceScopedAuth(nil, env, ioctx, mintTestWorkspace, mintTestClient, mintTestScope)
	if ExitCode(err) != ExitAPI || !strings.Contains(err.Error(), "incomplete scoped-token api key") {
		t.Fatalf("mint = %v, want the incomplete-key refusal", err)
	}
	if want := []string{"POST /apikeys", "DELETE /apikeys/key-1"}; !slices.Equal(server.requests, want) {
		t.Fatalf("requests = %v, want the key revoked", server.requests)
	}
}

func TestMintWorkspaceScopedAuthContextToleratesAKeyAlreadyGone(t *testing.T) {
	env, ioctx, server, warnings := mintFixture(t, false)
	server.revokeStatus = http.StatusNotFound
	if _, err := MintWorkspaceScopedAuth(nil, env, ioctx, mintTestWorkspace, mintTestClient, mintTestScope); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(*warnings) != 0 {
		t.Fatalf("warnings = %v, want none for a key already gone", *warnings)
	}
}

func TestMintWorkspaceScopedAuthContextWarnsWhenTheRevokeFails(t *testing.T) {
	env, ioctx, server, warnings := mintFixture(t, false)
	server.revokeStatus = http.StatusInternalServerError
	if _, err := MintWorkspaceScopedAuth(nil, env, ioctx, mintTestWorkspace, mintTestClient, mintTestScope); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(*warnings) != 1 || !strings.Contains((*warnings)[0], "failed to revoke ephemeral scoped-token api key key-1") {
		t.Fatalf("warnings = %v", *warnings)
	}
}

func TestMintWorkspaceScopedAuthContextDoesNothingOnceItsContextEnded(t *testing.T) {
	env, ioctx, server, _ := mintFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := MintWorkspaceScopedAuthContext(ctx, nil, env, ioctx, mintTestWorkspace, mintTestClient, mintTestScope); !errors.Is(err, context.Canceled) {
		t.Fatalf("mint = %v, want context.Canceled", err)
	}
	if len(server.requests) != 0 {
		t.Fatalf("requests = %v, want none", server.requests)
	}
}
