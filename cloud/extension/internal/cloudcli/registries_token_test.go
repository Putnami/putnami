package cloudcli

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
)

// The user contract for a registry credential is one word: the registry KIND.
// `putnami cloud token --for npm|go|oci|put`, or the same thing spelled by host
// with `cloud registry-token --host <host>`. The tests below pin exactly that,
// plus the three things that must NOT happen: no target flags, no bare global
// scope requested, and nothing but the bearer on stdout.

type userTokenCapture struct {
	keyRequests    int
	tokenRequests  int
	allowedScopes  string
	requestedScope string
	clientID       string
	workspaceID    string
	revoked        []string
	// bearer is the token the fake auth server minted, so assertions compare
	// against the exact value instead of a literal.
	bearer string
}

// userTokenClient fakes the two-step api-key grant MintWorkspaceScopedAuth
// drives: create a short-lived key bounded to the requested scope, exchange it,
// then revoke it.
func userTokenClient(t *testing.T, capture *userTokenCapture, marker string) *http.Client {
	t.Helper()
	// MintWorkspaceScopedAuth verifies the returned token's audience equals the
	// client it minted for, so the fake must sign a real-shaped JWT.
	capture.bearer = jwt(map[string]any{
		"aud":       distributioncli.DefaultRegistryTokenClientID,
		"sub":       "user:user-1",
		"scope":     marker,
		"exp":       fixedNow().Add(5 * time.Minute).Unix(),
		"scope_ref": map[string]any{"workspace_id": "ws-consumer"},
	})
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := map[string]any{}
		if req.Body != nil {
			data, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request: %v", err)
			}
			if len(data) > 0 {
				if err := json.Unmarshal(data, &body); err != nil {
					t.Fatalf("decode request %s: %v", req.URL.Path, err)
				}
			}
		}
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/.well-known/openid-configuration":
			return jsonResponse(http.StatusOK, map[string]any{
				"issuer": testBaseURL, "token_endpoint": testBaseURL + "/token",
			}), nil
		case req.Method == http.MethodPost && req.URL.Path == "/apikeys":
			capture.keyRequests++
			capture.allowedScopes, _ = body["allowed_scopes"].(string)
			capture.workspaceID, _ = body["workspace_id"].(string)
			return jsonResponse(http.StatusCreated, map[string]any{
				"id": "key-1", "name": stringValue(body["name"]), "prefix": "pkt_ephe",
				"owner_principal_kind": "user", "owner_principal_id": "user-1",
				"allowed_scopes": capture.allowedScopes, "allowed_client_ids": []string{},
				"created_at": "2026-04-30T12:00:00.000Z", "raw_token": "pkt_ephemeral",
			}), nil
		case req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, "/apikeys/"):
			capture.revoked = append(capture.revoked, strings.TrimPrefix(req.URL.Path, "/apikeys/"))
			return jsonResponse(http.StatusOK, map[string]any{"revoked": true}), nil
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/token"):
			capture.tokenRequests++
			capture.requestedScope, _ = body["scope"].(string)
			capture.clientID, _ = body["client_id"].(string)
			return jsonResponse(http.StatusOK, map[string]any{
				"access_token": capture.bearer, "token_type": "Bearer", "expires_in": 300,
			}), nil
		}
		t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
		return nil, nil
	})}
}

// distributionLeaseFixture is retained under its original name for the sibling
// admin tests; registryTokenFixture is the same fixture read as what it is.
func distributionLeaseFixture(t *testing.T) (string, map[string]string) {
	t.Helper()
	return registryTokenFixture(t)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

func registryTokenFixture(t *testing.T) (string, map[string]string) {
	t.Helper()
	home := t.TempDir()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".putnami"), 0o700); err != nil {
		t.Fatal(err)
	}
	link, _ := json.Marshal(map[string]any{
		"workspace_id":      "ws-consumer",
		"control_plane_url": testBaseURL,
	})
	if err := os.WriteFile(filepath.Join(root, LinkFileRelative), link, 0o600); err != nil {
		t.Fatal(err)
	}
	access := jwt(map[string]any{
		"sub":       "user:user-1",
		"exp":       fixedNow().Add(time.Hour).Unix(),
		"scope_ref": map[string]any{"workspace_id": "ws-consumer"},
	})
	// PUTNAMI_HOME is set explicitly to $HOME/.putnami, matching how the
	// framework CLI actually launches this extension (PUTNAMI_HOME=~/.putnami).
	// A native-credential writer that still resolved ~/.npmrc or ~/.netrc
	// through PUTNAMI_HOME instead of HOME would land the file one directory
	// off and fail TestRegistryTokenMaterializesNativeCredentials below — the
	// exact 2026-09-03 regression this fixture now pins against.
	env := hometest.Env(home, map[string]string{"PUTNAMI_HOME": filepath.Join(home, ".putnami"), "PUTNAMI_WORKSPACE_ROOT": root})
	if err := writeAuth(&storedToken{
		AccessToken: access, TokenType: "Bearer",
		ExpiresAt: fixedNow().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	}, env); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	return home, env
}

// TestRegistryTokenMintsMarkerScopeForEachKind is the contract: one kind in, one
// bare bearer out, minted against the `distribution` client with the PROTOCOL
// MARKER as its only scope. The marker names the surface; it authorizes nothing
// by itself, which is why no owner workspace or package appears anywhere.
func TestRegistryTokenMintsMarkerScopeForEachKind(t *testing.T) {
	for _, test := range []struct{ kind, marker string }{
		{"npm", "npm"}, {"go", "go"}, {"oci", "oci"}, {"put", "put"},
		// "registry" is the historical spelling of the put kind; it must keep
		// resolving so existing recipes on disk do not break.
		{"registry", "put"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			_, env := registryTokenFixture(t)
			capture := &userTokenCapture{}
			var stdout []string
			err := RunCommand("token", IO{
				Env: env, Client: userTokenClient(t, capture, test.marker), Now: fixedNow,
				Stdout: func(line string) { stdout = append(stdout, line) }, Stderr: func(string) {},
			}, []string{"--for", test.kind})
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			if got := strings.Join(stdout, "\n"); got != capture.bearer {
				t.Fatalf("stdout = %q, want exactly the bare bearer", got)
			}
			if capture.requestedScope != test.marker || capture.allowedScopes != test.marker {
				t.Fatalf("scope = %q / allowed %q, want the bare marker %q",
					capture.requestedScope, capture.allowedScopes, test.marker)
			}
			if capture.clientID != distributioncli.DefaultRegistryTokenClientID {
				t.Fatalf("client_id = %q, want %q", capture.clientID, distributioncli.DefaultRegistryTokenClientID)
			}
			if len(capture.revoked) != 1 || capture.revoked[0] != "key-1" {
				t.Fatalf("ephemeral backing key was not revoked: %v", capture.revoked)
			}
		})
	}
}

// TestRegistryTokenNeverRequestsAGlobalRegistryScope is the regression guard for
// the vulnerability this change closes: the command used to mint through a
// client allowed "registry.package.read registry.package.publish
// registry.package.promote", which the registries read as GLOBAL permissions —
// one user token read every private package in the fleet.
func TestRegistryTokenNeverRequestsAGlobalRegistryScope(t *testing.T) {
	_, env := registryTokenFixture(t)
	capture := &userTokenCapture{}
	err := RunCommand("token", IO{
		Env: env, Client: userTokenClient(t, capture, "npm"), Now: fixedNow,
		Stdout: func(string) {}, Stderr: func(string) {},
	}, []string{"--for", "npm"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	for _, forbidden := range []string{
		"registry.package.read", "registry.package.publish",
		"registry.package.promote", "registry.package.manage", "registry.namespace.admin",
	} {
		if strings.Contains(capture.requestedScope, forbidden) || strings.Contains(capture.allowedScopes, forbidden) {
			t.Fatalf("mint requested the global scope %q (scope=%q allowed=%q)",
				forbidden, capture.requestedScope, capture.allowedScopes)
		}
	}
}

// TestRegistryTokenRejectsRetiredTargetFlags proves the target coordinates are
// REFUSED, not ignored. Ignoring them would hand a caller a token with wider
// authority than the flags it passed described.
func TestRegistryTokenRejectsRetiredTargetFlags(t *testing.T) {
	for _, test := range []struct{ name, flag, value string }{
		{"owner_workspace", "--owner-workspace", "ws-owner"},
		{"package", "--package", "@acme/widget"},
		{"action", "--action", "publish"},
		{"channel", "--channel", "pr-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, env := registryTokenFixture(t)
			capture := &userTokenCapture{}
			err := RunCommand("token", IO{
				Env: env, Client: userTokenClient(t, capture, "npm"), Now: fixedNow,
				Stdout: func(string) {}, Stderr: func(string) {},
			}, []string{"--for", "npm", test.flag, test.value})
			if err == nil || !strings.Contains(err.Error(), strings.TrimPrefix(test.flag, "--")) {
				t.Fatalf("error = %v, want a refusal naming %s", err, test.flag)
			}
			if capture.keyRequests != 0 || capture.tokenRequests != 0 {
				t.Fatalf("refusal must happen before any mint: %+v", capture)
			}
		})
	}
}

func TestRegistryTokenRejectsOpaqueAndUnknownKind(t *testing.T) {
	for _, test := range []struct {
		name, want string
		args       []string
	}{
		{"opaque", "--opaque is not supported", []string{"--for", "npm", "--opaque"}},
		{"unknown_kind", "expected cache, npm, go, oci, or put", []string{"--for", "pypi"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, env := registryTokenFixture(t)
			capture := &userTokenCapture{}
			err := RunCommand("token", IO{
				Env: env, Client: userTokenClient(t, capture, "npm"), Now: fixedNow,
				Stdout: func(string) {}, Stderr: func(string) {},
			}, test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if capture.keyRequests != 0 {
				t.Fatalf("refusal must happen before minting a backing key")
			}
		})
	}
}

// TestRegistryTokenHostFormMatchesKindForm pins that `registry-token --host` is
// the same command spelled differently: same client, same marker scope.
func TestRegistryTokenRejectsAnUnknownHost(t *testing.T) {
	_, env := registryTokenFixture(t)
	capture := &userTokenCapture{}
	err := RunCommand("registry-token", IO{
		Env: env, Client: userTokenClient(t, capture, "npm"), Now: fixedNow,
		Stdout: func(string) {}, Stderr: func(string) {},
	}, []string{"--host", "npm.example.test"})
	if err == nil || !strings.Contains(err.Error(), "unknown registry host") {
		t.Fatalf("error = %v, want an unknown-host refusal", err)
	}
	if capture.keyRequests != 0 {
		t.Fatalf("refusal must happen before minting a backing key")
	}
}

func TestRegistryTokenHostFormMatchesKindForm(t *testing.T) {
	for host, marker := range map[string]string{
		"npm.putnami.dev": "npm", "go.putnami.dev": "go",
		"oci.putnami.dev": "oci", "put.putnami.dev": "put",
	} {
		t.Run(host, func(t *testing.T) {
			_, env := registryTokenFixture(t)
			capture := &userTokenCapture{}
			var stdout []string
			err := RunCommand("registry-token", IO{
				Env: env, Client: userTokenClient(t, capture, marker), Now: fixedNow,
				Stdout: func(line string) { stdout = append(stdout, line) }, Stderr: func(string) {},
			}, []string{"--host", host})
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			if capture.requestedScope != marker {
				t.Fatalf("scope = %q, want %q", capture.requestedScope, marker)
			}
			if got := strings.Join(stdout, "\n"); got != capture.bearer {
				t.Fatalf("stdout = %q, want exactly the bare bearer", got)
			}
		})
	}
}

// TestRegistryTokenMaterializesNativeCredentials covers --materialize for each
// registry's native store, and pins that the bearer never reaches stdout.
func TestRegistryTokenMaterializesNativeCredentials(t *testing.T) {
	for _, test := range []struct {
		kind   string
		verify func(t *testing.T, home string, env map[string]string, token string)
	}{
		{"npm", func(t *testing.T, home string, _ map[string]string, token string) {
			data, err := os.ReadFile(filepath.Join(home, ".npmrc"))
			if err != nil {
				t.Fatalf("read npmrc: %v", err)
			}
			assertContains(t, string(data), "//npm.putnami.dev/:_authToken="+token)
		}},
		{"go", func(t *testing.T, home string, _ map[string]string, token string) {
			data, err := os.ReadFile(filepath.Join(home, ".netrc"))
			if err != nil {
				t.Fatalf("read netrc: %v", err)
			}
			assertContains(t, string(data), "machine go.putnami.dev login _token password "+token)
		}},
		{"oci", func(t *testing.T, home string, _ map[string]string, token string) {
			data, err := os.ReadFile(filepath.Join(home, ".docker", "config.json"))
			if err != nil {
				t.Fatalf("read docker config: %v", err)
			}
			var config struct {
				Auths map[string]struct {
					Auth string `json:"auth"`
				} `json:"auths"`
			}
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			decoded, _ := base64.StdEncoding.DecodeString(config.Auths["oci.putnami.dev"].Auth)
			if string(decoded) != "_token:"+token {
				t.Fatalf("docker credential = %q", decoded)
			}
		}},
		{"put", func(t *testing.T, _ string, env map[string]string, token string) {
			var state distributioncli.RegistriesState
			readJSONFile(t, filepath.Join(env["HOME"], ".putnami/registries.json"), &state)
			if state.PutAuth["put.putnami.dev"] != token {
				t.Fatalf("put credential = %+v", state.PutAuth)
			}
		}},
	} {
		t.Run(test.kind, func(t *testing.T) {
			home, env := registryTokenFixture(t)
			capture := &userTokenCapture{}
			var stdout []string
			err := RunCommand("token", IO{
				Env: env, Client: userTokenClient(t, capture, test.kind), Now: fixedNow,
				Stdout: func(line string) { stdout = append(stdout, line) }, Stderr: func(string) {},
			}, []string{"--for", test.kind, "--materialize"})
			if err != nil {
				t.Fatalf("materialize: %v", err)
			}
			if strings.Contains(strings.Join(stdout, "\n"), capture.bearer) {
				t.Fatalf("--materialize leaked the bearer to stdout: %v", stdout)
			}
			test.verify(t, home, env, capture.bearer)
		})
	}
}

// TestRegistryTokenReusesTheCachedBearer pins that resolving the same recipe
// twice costs one mint. A publish resolves the token source once per package;
// minting an ephemeral api key each time would hammer the auth server.
func TestRegistryTokenReusesTheCachedBearer(t *testing.T) {
	_, env := registryTokenFixture(t)
	capture := &userTokenCapture{}
	for i := range 3 {
		err := RunCommand("token", IO{
			Env: env, Client: userTokenClient(t, capture, "npm"), Now: fixedNow,
			Stdout: func(string) {}, Stderr: func(string) {},
		}, []string{"--for", "npm"})
		if err != nil {
			t.Fatalf("invocation %d: %v", i, err)
		}
	}
	if capture.tokenRequests != 1 {
		t.Fatalf("token exchanges = %d, want 1 (the cache should answer the rest)", capture.tokenRequests)
	}
}
