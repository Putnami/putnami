package cloudcli

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type distributionAdminCall struct {
	method         string
	host           string
	path           string
	rawQuery       string
	authorization  string
	idempotencyKey string
	body           map[string]any
}

func distributionAdminClient(t *testing.T, calls *[]distributionAdminCall, tokenCapture *userTokenCapture) *http.Client {
	t.Helper()
	authClient := userTokenClient(t, tokenCapture, "put")
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(req.URL.Path, "/v1/workspaces/") && !strings.HasPrefix(req.URL.Path, "/put/_/release-sets/workspaces/") {
			return authClient.Transport.RoundTrip(req)
		}
		call := distributionAdminCall{
			method: req.Method, host: req.URL.Host, path: req.URL.Path, rawQuery: req.URL.RawQuery,
			authorization: req.Header.Get("Authorization"), idempotencyKey: req.Header.Get("Idempotency-Key"),
		}
		if req.Body != nil {
			data, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			if len(data) > 0 {
				if err := json.Unmarshal(data, &call.body); err != nil {
					t.Fatal(err)
				}
			}
		}
		*calls = append(*calls, call)
		switch {
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/distribution/bindings"):
			return jsonResponse(http.StatusCreated, map[string]any{"binding": map[string]any{"id": "binding-1", "protocol": "npm", "namespace": "@owner"}}), nil
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/distribution/bindings"):
			return jsonResponse(http.StatusOK, map[string]any{"bindings": []any{map[string]any{"id": "binding-1", "protocol": "npm", "namespace": "@owner"}}}), nil
		case req.Method == http.MethodPost && req.URL.Path == "/put/_/release-sets/workspaces/ws-consumer/grants":
			return jsonResponse(http.StatusCreated, map[string]any{"id": "grant-1", "grantee_workspace_id": "ws-reader", "channel": "stable"}), nil
		case req.Method == http.MethodGet && req.URL.Path == "/put/_/release-sets/workspaces/ws-consumer/grants":
			return jsonResponse(http.StatusOK, map[string]any{"grants": []any{map[string]any{"id": "grant-1", "grantee_workspace_id": "ws-reader", "channel": "stable"}}}), nil
		case req.Method == http.MethodDelete && req.URL.Path == "/put/_/release-sets/workspaces/ws-consumer/grants/grant-1":
			return jsonResponse(http.StatusOK, map[string]any{"id": "grant-1", "revoked_at": "2026-09-06T00:00:00Z"}), nil
		default:
			t.Fatalf("unexpected distribution admin request: %s %s", req.Method, req.URL.String())
			return nil, nil
		}
	})}
}

func TestDistributionAdminDerivesOwnerAndPinsExactRequests(t *testing.T) {
	_, env := distributionLeaseFixture(t)
	env["PUTNAMI_REGISTRY_PUT_URL"] = "https://registry.example"
	var calls []distributionAdminCall
	var tokenCapture userTokenCapture
	client := distributionAdminClient(t, &calls, &tokenCapture)
	run := func(args ...string) {
		t.Helper()
		if err := RunCommand("distribution", IO{Env: env, Client: client, Now: fixedNow, Stdout: func(string) {}, Stderr: func(string) {}}, args); err != nil {
			t.Fatalf("distribution %v: %v", args, err)
		}
	}
	run("bindings", "activate", "--protocol", "npm", "--namespace", "@owner", "--idempotency-key", "npm-owner")
	run("bindings", "list")
	run("grants", "create", "--grantee-workspace-id", "ws-reader", "--channel", "stable", "--idempotency-key", "stable-read")
	run("grants", "list", "--include-revoked")
	run("grants", "revoke", "grant-1")

	if len(calls) != 5 {
		t.Fatalf("calls = %+v", calls)
	}
	for index, call := range calls {
		prefix := "/v1/workspaces/ws-consumer/distribution/"
		if index >= 2 {
			prefix = "/put/_/release-sets/workspaces/ws-consumer/grants"
			if call.host != "registry.example" {
				t.Fatalf("grant operation did not reach the native Put host: %s", call.host)
			}
			if call.authorization != "Bearer "+tokenCapture.bearer {
				t.Fatalf("grant operation did not use its native Put credential: %s", call.path)
			}
		}
		if !strings.HasPrefix(call.path, prefix) {
			t.Errorf("owner route was not derived from linked workspace: %s", call.path)
		}
		if call.authorization == "" {
			t.Errorf("missing human session auth on %s %s", call.method, call.path)
		}
	}
	// The generated client carries the idempotency key in the body: a Go
	// endpoint cannot declare a request header.
	if calls[0].idempotencyKey != "" || mustJSON(t, calls[0].body) != `{"idempotency_key":"npm-owner","namespace":"@owner","protocol":"npm"}` {
		t.Fatalf("binding activation = key %q body %s", calls[0].idempotencyKey, mustJSON(t, calls[0].body))
	}
	grantBody := mustJSON(t, calls[2].body)
	wantGrantBody := `{"channel":"stable","grantee_workspace_id":"ws-reader","idempotency_key":"stable-read"}`
	if calls[2].idempotencyKey != "" || grantBody != wantGrantBody {
		t.Fatalf("grant create = key %q body %s, want %s", calls[2].idempotencyKey, grantBody, wantGrantBody)
	}
	for _, forbidden := range []string{"owner_workspace_id", "namespace", "creator", "workspace_id", "protocol", "package", "action"} {
		if _, found := calls[2].body[forbidden]; found {
			t.Fatalf("grant body carries server-owned %q: %s", forbidden, grantBody)
		}
	}
	if calls[3].rawQuery != "include_revoked=true" {
		t.Fatalf("grant list query = %q", calls[3].rawQuery)
	}
	if tokenCapture.allowedScopes != "put" || tokenCapture.requestedScope != "put" || tokenCapture.tokenRequests != 1 {
		t.Fatalf("grant administration requested authority beyond its native protocol marker: %+v", tokenCapture)
	}
}

func TestDistributionAdminRejectsOwnerOverrideBeforeRequest(t *testing.T) {
	_, env := distributionLeaseFixture(t)
	var calls []distributionAdminCall
	var tokenCapture userTokenCapture
	err := RunCommand("distribution", IO{
		Env: env, Client: distributionAdminClient(t, &calls, &tokenCapture), Now: fixedNow,
		Stdout: func(string) {}, Stderr: func(string) {},
	}, []string{"grants", "create", "--grantee-workspace-id", "ws-reader", "--idempotency-key", "key", "--workspace", "ws-attacker"})
	if err == nil || !strings.Contains(err.Error(), "owner is the linked workspace") {
		t.Fatalf("owner override error = %v", err)
	}
	if len(calls) != 0 || tokenCapture.tokenRequests != 0 || tokenCapture.keyRequests != 0 {
		t.Fatalf("owner override reached control plane: %+v", calls)
	}
}
