package distributioncli

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	regproto "go.putnami.dev/protocol/registry"
)

// A materialized credential must stay valid for the window the engine tells
// its jobs (CLI ADR 0101): a stored token that expires inside it serves a
// bearer, but a materialization mints a fresh one.
func TestMaterializeMintsWhenTheStoredTokenExpiresInsideTheWindow(t *testing.T) {
	endpoint := RegistryEndpoint{Registry: RegistryNPM, URL: "https://npm.example", Host: "npm.example"}
	for name, tc := range map[string]struct {
		left     time.Duration
		freshFor time.Duration
		reused   bool
	}{
		"a bearer reuses a token with two minutes left":         {left: 2 * time.Minute, reused: true},
		"a materialization mints over a token two minutes left": {left: 2 * time.Minute, freshFor: regproto.NativeCredentialValidity},
		"a materialization reuses a token that outlives it":     {left: regproto.NativeCredentialValidity + time.Minute, freshFor: regproto.NativeCredentialValidity, reused: true},
	} {
		t.Run(name, func(t *testing.T) {
			root, env, ioctx := distributionWorkflowFixture(t)
			stored := "stored-registry-token"
			if err := storeRegistryAccessToken(env, endpoint.Host, registryAccessToken{
				AccessToken: stored, TokenType: "Bearer", ClientID: DefaultRegistryTokenClientID,
				Scope: RegistryMarkerScope(endpoint.Registry), ExpiresAt: ioctx.Now().Add(tc.left).Format(time.RFC3339Nano),
			}); err != nil {
				t.Fatal(err)
			}
			requests := 0
			ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(*http.Request) (*http.Response, error) {
				requests++
				return nil, errors.New("no auth server in this test")
			})}
			token, err := mintUserRegistryToken(nil, root, env, ioctx, endpoint, tc.freshFor)
			if tc.reused {
				if err != nil || token != stored || requests != 0 {
					t.Fatalf("token = %q, err = %v, %d requests; want the stored token and no request", token, err, requests)
				}
				return
			}
			if token == stored || requests == 0 {
				t.Fatalf("token = %q, %d requests; want a mint, not the stored token", token, requests)
			}
		})
	}
}

// Only `--materialize --for-run` asks for the run window: every other shape
// keeps a stored token until it is about to expire.
func TestRegistryTokenAsksForTheWindowOnlyForARun(t *testing.T) {
	for name, tc := range map[string]struct {
		params map[string]any
		reused bool
	}{
		"materialize":                 {params: map[string]any{"materialize": true}, reused: true},
		"materialize for-run":         {params: map[string]any{"materialize": true, regproto.SeamForRunFlag: true}},
		"materialize forRun":          {params: map[string]any{"materialize": true, "forRun": true}},
		"for-run without materialize": {params: map[string]any{regproto.SeamForRunFlag: true}, reused: true},
	} {
		t.Run(name, func(t *testing.T) {
			root, env, ioctx := distributionWorkflowFixture(t)
			endpoint, err := resolveRegistryTokenEndpoint(map[string]any{"for": "npm"}, env)
			if err != nil {
				t.Fatal(err)
			}
			if err := storeRegistryAccessToken(env, endpoint.Host, registryAccessToken{
				AccessToken: "stored-registry-token", TokenType: "Bearer", ClientID: DefaultRegistryTokenClientID,
				Scope: RegistryMarkerScope(endpoint.Registry), ExpiresAt: ioctx.Now().Add(2 * time.Minute).Format(time.RFC3339Nano),
			}); err != nil {
				t.Fatal(err)
			}
			requests := 0
			ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(*http.Request) (*http.Response, error) {
				requests++
				return nil, errors.New("no auth server in this test")
			})}
			params := map[string]any{"for": "npm"}
			for key, value := range tc.params {
				params[key] = value
			}
			err = RegistryToken(params, root, env, ioctx)
			if tc.reused && (err != nil || requests != 0) {
				t.Fatalf("err = %v, %d requests; want the stored token reused", err, requests)
			}
			if !tc.reused && requests == 0 {
				t.Fatalf("err = %v, no request; want a fresh mint for the run window", err)
			}
		})
	}
}

// The engine sends registry.SeamForRunFlag to `cloud registry-token`, so the
// command declares it: an undeclared flag is refused, and every run would fall
// back to one CLI start per job.
func TestRegistryTokenDeclaresTheRunWindowFlag(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Commands map[string]struct {
			Flags map[string]struct{ Type string } `json:"flags"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	flag, ok := manifest.Commands["cloud-registry-token"].Flags[regproto.SeamForRunFlag]
	if !ok || flag.Type != "boolean" {
		t.Fatalf("cloud-registry-token declares %s as %+v (declared %v), want a boolean flag", regproto.SeamForRunFlag, flag, ok)
	}
}

// A freshly minted token that expires inside the run window is refused for a
// run, so the engine names no host and each job refreshes its credential; a
// token that outlives the window is used.
func TestMaterializeForARunRefusesAFreshTokenThatExpiresInsideTheWindow(t *testing.T) {
	for name, tc := range map[string]struct {
		expiresIn int
		ok        bool
	}{
		"five minutes": {expiresIn: 300, ok: true},
		"two minutes":  {expiresIn: 120},
	} {
		t.Run(name, func(t *testing.T) {
			root, env, ioctx := distributionWorkflowFixture(t)
			env["PUTNAMI_AUTH_URL"] = "https://auth.test"
			endpoint := RegistryEndpoint{Registry: RegistryNPM, URL: "https://npm.example", Host: "npm.example"}
			bearer := workflowJWT(map[string]any{"aud": "distribution", "scope": RegistryMarkerScope(endpoint.Registry), "sub": "user-admin",
				"scope_ref": map[string]any{"workspace_id": "ws-consumer"}})
			ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/.well-known/openid-configuration":
					return workflowJSONResponse(http.StatusOK, map[string]any{"issuer": "https://auth.test", "token_endpoint": "https://auth.test/token"}), nil
				case request.Method == http.MethodPost && request.URL.Path == "/apikeys":
					return workflowJSONResponse(http.StatusCreated, workflowAPIKeyCreated("pkt_ephemeral")), nil
				case request.Method == http.MethodPost && request.URL.Path == "/token":
					return workflowJSONResponse(http.StatusOK, map[string]any{"access_token": bearer, "token_type": "Bearer", "expires_in": tc.expiresIn}), nil
				case request.Method == http.MethodDelete && request.URL.Path == "/apikeys/key-1":
					return workflowJSONResponse(http.StatusOK, workflowAPIKeyRevoked), nil
				}
				t.Fatalf("unexpected request %s %s", request.Method, request.URL)
				return nil, nil
			})}
			token, err := mintUserRegistryToken(nil, root, env, ioctx, endpoint, regproto.NativeCredentialValidity)
			if tc.ok && (err != nil || token != bearer) {
				t.Fatalf("token = %q, err = %v; want the minted token", token, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("token = %q, want a refusal: it expires inside the run window", token)
			}
		})
	}
}

func TestMintedExpiry(t *testing.T) {
	stated := "2026-08-22T12:05:00Z"
	if got := mintedExpiry("opaque", stated); got != stated {
		t.Errorf("stated expiry = %q, want %q", got, stated)
	}
	exp := time.Date(2026, 8, 22, 12, 10, 0, 0, time.UTC)
	if got := mintedExpiry(workflowJWT(map[string]any{"exp": exp.Unix()}), ""); got != exp.Format(time.RFC3339Nano) {
		t.Errorf("JWT exp expiry = %q, want %q", got, exp.Format(time.RFC3339Nano))
	}
	if got := mintedExpiry("opaque", ""); got != "" {
		t.Errorf("unknown expiry = %q, want empty: a run window cannot be promised", got)
	}
}
