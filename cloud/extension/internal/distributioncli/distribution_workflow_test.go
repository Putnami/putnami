package distributioncli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
)

type workflowRoundTrip func(*http.Request) (*http.Response, error)

func (f workflowRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func workflowJSONResponse(status int, payload any) *http.Response {
	data, _ := json.Marshal(payload)
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(data)),
	}
}

// workflowAPIKeyCreated is auth-server's 201 answer to POST /apikeys for key
// key-1: every member its contract requires, with the one-time raw token.
func workflowAPIKeyCreated(rawToken string) map[string]any {
	return map[string]any{
		"id": "key-1", "name": "cli:distribution:ws", "prefix": "pkt_ephe",
		"owner_principal_kind": "user", "owner_principal_id": "user-1",
		"allowed_scopes": "put", "allowed_client_ids": []string{},
		"created_at": "2026-10-02T12:00:00Z", "raw_token": rawToken,
	}
}

// workflowAPIKeyRevoked is auth-server's 200 answer to DELETE /apikeys/{id}.
var workflowAPIKeyRevoked = map[string]any{"revoked": true}

func workflowJWT(claims map[string]any) string {
	header, _ := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".test"
}

func distributionWorkflowFixture(t *testing.T) (string, map[string]string, clicore.IO) {
	t.Helper()
	home := t.TempDir()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".putnami"), 0o700); err != nil {
		t.Fatal(err)
	}
	link, _ := json.Marshal(map[string]any{
		"workspace_id":      "ws-consumer",
		"control_plane_url": "https://control.example",
	})
	if err := os.WriteFile(filepath.Join(root, clicore.LinkFileRelative), link, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	access := workflowJWT(map[string]any{
		"sub": "user-admin", "exp": now.Add(time.Hour).Unix(),
		"scope_ref": map[string]any{"workspace_id": "ws-consumer"},
	})
	env := hometest.Env(home, map[string]string{"PUTNAMI_HOME": home})
	if err := clicore.WriteAuth(&clicore.StoredToken{
		AccessToken: access,
		TokenType:   "Bearer",
		ExpiresAt:   now.Add(time.Hour).Format(time.RFC3339Nano),
	}, env); err != nil {
		t.Fatal(err)
	}
	ioctx := clicore.IO{
		Env: env, Now: func() time.Time { return now },
		Stdout: func(string) {}, Stderr: func(string) {},
	}
	return root, env, ioctx
}

// TestRegistryTokenRefusesRetiredTargetFlags pins the library-level contract:
// `cloud token`/`cloud registry-token` take a registry KIND (or the host that
// names one) and nothing else. The retired target coordinates are refused
// BEFORE any network call, so a caller that still passes them cannot receive a
// credential whose authority differs from what it asked for.
//
// Materialization and the mint itself are covered end to end against a fake
// auth server in internal/cloudcli/registries_token_test.go.
func TestRegistryTokenRefusesRetiredTargetFlags(t *testing.T) {
	for _, flag := range []string{"owner-workspace", "package", "action", "channel", "scope"} {
		t.Run(flag, func(t *testing.T) {
			root, env, ioctx := distributionWorkflowFixture(t)
			ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
				t.Fatalf("refusal must precede any request, got %s %s", request.Method, request.URL.Path)
				return nil, nil
			})}
			err := RegistryToken(map[string]any{"for": "npm", flag: "value"}, root, env, ioctx)
			if err == nil || !strings.Contains(err.Error(), flag) {
				t.Fatalf("error = %v, want a refusal naming --%s", err, flag)
			}
		})
	}
}

// TestRegistryMarkerScopeIsTheKindWord pins the one string the whole contract
// turns on: the OAuth scope a registry token requests equals the `--for` kind,
// and it is never a fleet-wide registry permission.
func TestRegistryMarkerScopeIsTheKindWord(t *testing.T) {
	want := map[Registry]string{
		RegistryNPM: "npm", RegistryGomod: "go", RegistryOCI: "oci", RegistryPut: "put",
	}
	for registry, marker := range want {
		if got := RegistryMarkerScope(registry); got != marker {
			t.Fatalf("RegistryMarkerScope(%s) = %q, want %q", registry, got, marker)
		}
		recipe := RegistryTokenRecipe(registry)
		if len(recipe) != 5 || recipe[3] != "--for" || recipe[4] != marker {
			t.Fatalf("RegistryTokenRecipe(%s) = %v", registry, recipe)
		}
	}
	if RegistryMarkerScope(Registry("pypi")) != "" {
		t.Fatal("an unknown registry must have no marker scope")
	}
}

func TestDistributionAdminPinsLinkedOwnerLifecycle(t *testing.T) {
	root, env, ioctx := distributionWorkflowFixture(t)
	type call struct {
		method, path, query, auth string
		body                      map[string]any
	}
	var calls []call
	ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
		current := call{
			method: request.Method, path: request.URL.Path, query: request.URL.RawQuery,
			auth: request.Header.Get("Authorization"),
		}
		if request.Body != nil {
			data, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if len(data) > 0 {
				if err := json.Unmarshal(data, &current.body); err != nil {
					t.Fatal(err)
				}
			}
		}
		calls = append(calls, current)
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/distribution/bindings"):
			return workflowJSONResponse(http.StatusCreated, map[string]any{"binding": map[string]any{"id": "binding-1"}}), nil
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/distribution/bindings"):
			return workflowJSONResponse(http.StatusOK, map[string]any{"bindings": []any{map[string]any{"id": "binding-1", "protocol": "npm", "namespace": "@owner"}}}), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.String())
			return nil, nil
		}
	})}

	commands := []struct {
		args   []string
		params map[string]any
	}{
		{args: []string{"bindings", "activate"}, params: map[string]any{
			"protocol": "npm", "namespace": "@owner", "idempotency-key": "binding-key",
			"workspace": map[string]any{"workspace_id": "ws-consumer"},
		}},
		{args: []string{"bindings", "list"}, params: map[string]any{}},
	}
	for _, command := range commands {
		if err := Distribution(command.params, command.args, root, env, ioctx); err != nil {
			t.Fatalf("distribution %v: %v", command.args, err)
		}
	}
	if len(calls) != len(commands) {
		t.Fatalf("calls = %+v", calls)
	}
	for _, current := range calls {
		if !strings.HasPrefix(current.path, "/v1/workspaces/ws-consumer/distribution/") || !strings.HasPrefix(current.auth, "Bearer ") {
			t.Fatalf("authority was not pinned by linked workspace: %+v", current)
		}
	}
	// The generated client carries the key in the body: a Go endpoint cannot
	// declare a request header.
	if calls[0].body["idempotency_key"] != "binding-key" || calls[0].body["namespace"] != "@owner" {
		t.Fatalf("binding activation = %+v", calls[0])
	}
}

func TestDistributionBindingActivateRejectsWorkspaceOverrides(t *testing.T) {
	root, env, ioctx := distributionWorkflowFixture(t)
	ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
		return nil, nil
	})}
	for _, override := range []string{"workspace", "owner-workspace", "owner-workspace-id"} {
		err := Distribution(map[string]any{
			"protocol": "npm", "namespace": "@owner", "idempotency-key": "binding-key", override: "ws-other",
		}, []string{"bindings", "activate"}, root, env, ioctx)
		if err == nil || !strings.Contains(err.Error(), "--"+override) || !strings.Contains(err.Error(), "derived server-side") {
			t.Fatalf("bindings activate --%s = %v, want derived-server-side usage error", override, err)
		}
	}
}

func TestRegistryWriterLifecyclePreservesOnlySupportedStores(t *testing.T) {
	// Both HOME and PUTNAMI_HOME must be redirected: the npm/gomod writers
	// exercised below touch ~/.npmrc and ~/.netrc under HOME, not PUTNAMI_HOME.
	env := hometest.Env(t.TempDir(), map[string]string{"PUTNAMI_HOME": t.TempDir()})
	for _, endpoint := range resolveRegistryEndpoints(nil, env) {
		writer := writerFor(endpoint.Registry)
		if writer == nil {
			t.Fatalf("missing writer for %s", endpoint.Registry)
		}
		if err := writer.Setup(env, endpoint.Host, "legacy-token"); err != nil {
			t.Fatalf("setup %s: %v", endpoint.Registry, err)
		}
		if err := writer.Teardown(env, endpoint.Host); err != nil {
			t.Fatalf("teardown %s: %v", endpoint.Registry, err)
		}
	}
	if writerFor(Registry("unknown")) != nil {
		t.Fatal("unknown registry unexpectedly has a writer")
	}
}
