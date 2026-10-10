package distributioncli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

func TestWorkspaceGrantCLIUsesNativePutAndLinkedOwner(t *testing.T) {
	root, env, ioctx := distributionWorkflowFixture(t)
	env["PUTNAMI_REGISTRY_PUT_URL"] = "https://registry.example"
	registryToken := workflowJWT(map[string]any{
		"sub": "user-admin", "aud": "distribution", "scope": "put",
		"exp": ioctx.Now().Add(time.Minute).Unix(),
	})
	if err := storeRegistryAccessToken(env, "registry.example", registryAccessToken{
		AccessToken: registryToken, TokenType: "Bearer", ClientID: DefaultRegistryTokenClientID,
		Scope: "put", ExpiresAt: ioctx.Now().Add(time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	type call struct {
		method, path, query string
		body                map[string]any
	}
	var calls []call
	ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "registry.example" || request.Header.Get("Authorization") != "Bearer "+registryToken ||
			!strings.HasPrefix(request.URL.Path, "/put/_/release-sets/workspaces/ws-consumer/grants") {
			t.Fatalf("workspace grant bypassed native Put authority: %s %s", request.Method, request.URL)
		}
		current := call{method: request.Method, path: request.URL.Path, query: request.URL.RawQuery}
		if request.Method == http.MethodPost {
			if err := json.NewDecoder(request.Body).Decode(&current.body); err != nil {
				t.Fatal(err)
			}
		}
		calls = append(calls, current)
		if request.Method == http.MethodGet {
			return workflowJSONResponse(http.StatusOK, map[string]any{"grants": []any{
				map[string]any{"id": "grant-1", "owner_workspace_id": "ws-consumer", "grantee_workspace_id": "ws-reader"},
			}}), nil
		}
		status := http.StatusOK
		if request.Method == http.MethodPost {
			status = http.StatusCreated
		}
		return workflowJSONResponse(status, map[string]any{
			"id": "grant-1", "owner_workspace_id": "ws-consumer", "grantee_workspace_id": "ws-reader",
		}), nil
	})}
	for _, command := range []struct {
		args   []string
		params map[string]any
	}{
		{[]string{"grants", "create"}, map[string]any{
			"grantee-workspace-id": "ws-reader", "channel": "latest", "idempotency-key": "latest-read",
			"workspace": map[string]any{"workspace_id": "ambient-cannot-replace-link"},
		}},
		{[]string{"grants", "create"}, map[string]any{"grantee-workspace-id": "ws-reader", "idempotency-key": "all-read"}},
		{[]string{"grants", "list"}, map[string]any{"include-revoked": true}},
		{[]string{"grants", "list"}, nil},
		{[]string{"grants", "revoke", "grant-1"}, nil},
	} {
		if err := Distribution(command.params, command.args, root, env, ioctx); err != nil {
			t.Fatalf("%v: %v", command.args, err)
		}
	}
	if len(calls) != 5 {
		t.Fatalf("grant lifecycle made %d requests, want 5", len(calls))
	}
	for index, count := range []int{3, 2} {
		if len(calls[index].body) != count || calls[index].body["grantee_workspace_id"] != "ws-reader" {
			t.Fatalf("grant carries caller-selected authority: %#v", calls[index].body)
		}
	}
	if calls[0].body["channel"] != "latest" || calls[0].body["idempotency_key"] != "latest-read" ||
		calls[1].body["idempotency_key"] != "all-read" || calls[2].query != "include_revoked=true" || calls[3].query != "" ||
		calls[4].method != http.MethodDelete || calls[4].path != "/put/_/release-sets/workspaces/ws-consumer/grants/grant-1" {
		t.Fatalf("workspace grant lifecycle changed the requested tuple: %#v", calls)
	}
}

func TestWorkspaceGrantCLIRejectsRetiredAuthorityBeforeCredentials(t *testing.T) {
	for _, operation := range []string{"create", "list", "revoke"} {
		for _, flag := range []string{
			"owner-workspace", "owner-workspace-id", "workspace", "namespace", "protocol", "package", "resource-kind",
			"purpose", "action", "grantee-kind", "grantee-id", "creator", "creator-id",
			"principal", "principal-kind", "principal-id", "scope", "scopes", "audience", "ttl", "expires-in",
		} {
			params := map[string]any{"grantee-workspace-id": "ws-reader", "idempotency-key": "key", flag: "retired"}
			err := Distribution(params, []string{"grants", operation, "grant-1"}, "", nil, clicore.IO{})
			if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "--"+flag) {
				t.Fatalf("grants %s --%s did not fail before credentials: %v", operation, flag, err)
			}
		}
	}
	for _, value := range []any{"", false} {
		if err := Distribution(map[string]any{"action": value}, []string{"grants", "create"}, "", nil, clicore.IO{}); err == nil || !strings.Contains(err.Error(), "--action") {
			t.Fatalf("an explicitly empty retired flag was accepted: %v", err)
		}
	}
}

func TestWorkspaceGrantCLINeedsGranteeAndStableCommandKey(t *testing.T) {
	for _, params := range []map[string]any{
		nil, {"grantee-workspace-id": "ws-reader"}, {"idempotency-key": "key"},
		{"grantee-workspace-id": "ws-reader", "idempotency-key": " key "},
	} {
		err := Distribution(params, []string{"grants", "create"}, "", nil, clicore.IO{})
		if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "--grantee-workspace-id") {
			t.Fatalf("incomplete command reached credential resolution: %v", err)
		}
	}
}
