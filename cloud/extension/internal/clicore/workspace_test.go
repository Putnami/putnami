package clicore

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestResolveWorkspaceID_FlagOverridesLink pins the --workspace override at the
// shared seam: an explicit --workspace (or its workspaceId alias) must win over the
// linked workspace, so a command run from a repo linked to ws-acme with
// --workspace ws-other targets — and mints auth for — ws-other. With no flag it
// falls back to the linked workspace. This is the ONE resolver deploy, status,
// and logs/traces/metrics now share.
func TestResolveWorkspaceID_FlagOverridesLink(t *testing.T) {
	link := map[string]any{"workspace_id": "ws-acme"}

	if got := ResolveWorkspaceID(map[string]any{"workspace": "ws-other"}, link); got != "ws-other" {
		t.Errorf("--workspace = %q, want ws-other (explicit flag must override the link)", got)
	}
	if got := ResolveWorkspaceID(map[string]any{"workspaceId": "ws-cc"}, link); got != "ws-cc" {
		t.Errorf("workspaceId alias = %q, want ws-cc", got)
	}
	if got := ResolveWorkspaceID(map[string]any{}, link); got != "ws-acme" {
		t.Errorf("no flag = %q, want ws-acme (falls back to the linked workspace)", got)
	}
	if got := ResolveWorkspaceID(map[string]any{}, map[string]any{}); got != "" {
		t.Errorf("no flag, no link = %q, want empty", got)
	}
}

// TestResolveWorkspaceID_ObjectParamExtractsWorkspaceID covers Putnami task
// parameter injection: options."@putnami/cloud".workspace is an object, not a
// scalar flag. The shared resolver must extract workspace_id instead of turning
// the whole object into a path segment like "map[workspace_id:...]".
func TestResolveWorkspaceID_ObjectParamExtractsWorkspaceID(t *testing.T) {
	link := map[string]any{"workspace_id": "ws-link"}
	param := map[string]any{
		"workspace": map[string]any{
			"workspace_id":      "ws-from-task-options",
			"control_plane_url": "https://task.example",
		},
	}

	if got := ResolveWorkspaceID(param, link); got != "ws-from-task-options" {
		t.Fatalf("workspace object = %q, want ws-from-task-options", got)
	}
}

// TestWorkspaceURL pins the shared URL builder: control-plane + versioned
// workspace path + suffix, with the workspace id path-escaped and a trailing
// slash on the control-plane base trimmed.
func TestWorkspaceURL(t *testing.T) {
	ctx := &WorkspaceContext{ControlPlane: "https://api.example/", WorkspaceID: "ws acme"}
	if got := ctx.WorkspaceURL("/deployments"); got != "https://api.example/v1/workspaces/ws%20acme/deployments" {
		t.Fatalf("WorkspaceURL = %q", got)
	}
}

// TestResolveWorkspaceControlPlaneURLHonorsOverrides pins the workspace command
// resolver's control-plane precedence: flag, env, then linked workspace. It must
// not silently inject DefaultControlPlaneURL when every source is empty because
// the workspace seam has an explicit "no control-plane URL" usage error.
func TestResolveWorkspaceControlPlaneURLHonorsOverrides(t *testing.T) {
	link := map[string]any{"control_plane_url": "https://linked.example/"}
	env := map[string]string{"PUTNAMI_CLOUD_API_URL": "https://env.example/"}
	// A nil env means the process environment, and the CI runner exports
	// PUTNAMI_CLOUD_API_URL to every publishing run; an empty map is "no env".
	noEnv := map[string]string{}

	if got := resolveWorkspaceControlPlaneURL(map[string]any{"control-plane-url": "https://flag.example/"}, env, link); got != "https://flag.example" {
		t.Fatalf("flag override = %q, want https://flag.example", got)
	}
	if got := resolveWorkspaceControlPlaneURL(map[string]any{}, env, link); got != "https://env.example" {
		t.Fatalf("env override = %q, want https://env.example", got)
	}
	if got := resolveWorkspaceControlPlaneURL(map[string]any{}, map[string]string{"PUTNAMI_CONTROL_PLANE_URL": "https://control.example/"}, link); got != "https://control.example" {
		t.Fatalf("control-plane env override = %q, want https://control.example", got)
	}
	if got := resolveWorkspaceControlPlaneURL(map[string]any{}, noEnv, link); got != "https://linked.example" {
		t.Fatalf("link fallback = %q, want https://linked.example", got)
	}
	taskParam := map[string]any{
		"workspace": map[string]any{
			"workspace_id":      "ws-from-task-options",
			"control_plane_url": "https://task.example/",
		},
	}
	if got := resolveWorkspaceControlPlaneURL(taskParam, noEnv, link); got != "https://task.example" {
		t.Fatalf("workspace object fallback = %q, want https://task.example", got)
	}
	if got := resolveWorkspaceControlPlaneURL(map[string]any{}, noEnv, map[string]any{}); got != "" {
		t.Fatalf("empty sources = %q, want empty so ResolveWorkspaceContext can return its usage error", got)
	}
}

// seedFreshSession writes a non-expiring session so ResolveWorkspaceRef can
// authenticate the workspace listing without a refresh round-trip.
func seedFreshSession(t *testing.T, env map[string]string) {
	t.Helper()
	if err := WriteAuth(&StoredToken{AccessToken: "session-token", TokenType: "Bearer", ExpiresAt: "2999-01-01T00:00:00Z"}, env); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

// TestResolveWorkspaceRef_ResolvesSlugAndName pins the --workspace DX: an id
// passes through with no network call, a slug or a display name resolves to
// the id through the caller's workspace listing, an ambiguous name is refused,
// and a workspace missing from the listing falls back to the personal-org
// ?name= lookup.
func TestResolveWorkspaceRef_ResolvesSlugAndName(t *testing.T) {
	env := map[string]string{"PUTNAMI_HOME": t.TempDir()}
	seedFreshSession(t, env)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer session-token" {
			t.Errorf("listing sent %q, want the session bearer", r.Header.Get("Authorization"))
		}
		// The listing is GET /v1/workspaces; the personal-org name lookup is
		// its own declared route, GET /v1/workspaces/lookup?name=.
		lookup := r.URL.Path == "/v1/workspaces/lookup"
		if r.URL.Path != "/v1/workspaces" && !lookup {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		name := r.URL.Query().Get("name")
		if !lookup {
			name = ""
		}
		switch name {
		case "":
			_, _ = w.Write([]byte(`{"workspaces":[
				{"id":"11111111-2222-4333-8444-555555555555","slug":"cloud","name":"acme-cloud"},
				{"id":"66666666-7777-4888-9999-aaaaaaaaaaaa","slug":"harbor","name":"Harbor"},
				{"id":"a3839d53-5b31-48d4-8924-fb29df25c743","slug":"dev","name":"harbor"}]}`))
		case "review-sandbox":
			_, _ = w.Write([]byte(`{"id":"dfd84dcb-bf5d-40c0-85db-7d8ea91eb2df","slug":"dfd-aa8a219b","name":"review-sandbox"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"workspace not found"}`))
		}
	}))
	defer server.Close()
	ioctx := IO{Client: server.Client(), Now: time.Now}
	link := map[string]any{"control_plane_url": server.URL, "workspace_id": "11111111-2222-4333-8444-555555555555"}

	cases := []struct {
		ref, want string
	}{
		{"", "11111111-2222-4333-8444-555555555555"},
		{"66666666-7777-4888-9999-aaaaaaaaaaaa", "66666666-7777-4888-9999-aaaaaaaaaaaa"},
		{"cloud", "11111111-2222-4333-8444-555555555555"},
		{"HARBOR", "66666666-7777-4888-9999-aaaaaaaaaaaa"},
		{"acme-cloud", "11111111-2222-4333-8444-555555555555"},
		{"review-sandbox", "dfd84dcb-bf5d-40c0-85db-7d8ea91eb2df"},
	}
	for _, tc := range cases {
		params := map[string]any{}
		if tc.ref != "" {
			params["workspace"] = tc.ref
		}
		got, err := ResolveWorkspaceRef(params, env, ioctx, link)
		if err != nil || got != tc.want {
			t.Fatalf("ResolveWorkspaceRef(%q) = %q, %v; want %q", tc.ref, got, err, tc.want)
		}
	}
	// Ids never hit the network: two slugs and one name cost one listing each,
	// the personal-org fallback costs the listing plus the ?name= lookup.
	if calls != 5 {
		t.Fatalf("workspace lookups = %d, want 5", calls)
	}

	if _, err := ResolveWorkspaceRef(map[string]any{"workspace": "unknown"}, env, ioctx, link); err == nil || ExitCode(err) != ExitUsage || !strings.Contains(err.Error(), `"unknown"`) {
		t.Fatalf("unknown ref: %v", err)
	}
}

func TestResolveWorkspaceRef_RefusesAmbiguousName(t *testing.T) {
	env := map[string]string{"PUTNAMI_HOME": t.TempDir()}
	seedFreshSession(t, env)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workspaces":[
			{"id":"11111111-1111-4111-8111-111111111111","slug":"a","name":"Twin"},
			{"id":"22222222-2222-4222-8222-222222222222","slug":"b","name":"twin"}]}`))
	}))
	defer server.Close()
	ioctx := IO{Client: server.Client(), Now: time.Now}
	_, err := ResolveWorkspaceRef(map[string]any{"workspace": "twin"}, env, ioctx, map[string]any{"control_plane_url": server.URL})
	if err == nil || ExitCode(err) != ExitUsage || !strings.Contains(err.Error(), "11111111-1111-4111-8111-111111111111, 22222222-2222-4222-8222-222222222222") {
		t.Fatalf("ambiguous name: %v", err)
	}
}

func TestResolveWorkspaceRef_NeedsAControlPlane(t *testing.T) {
	env := map[string]string{"PUTNAMI_HOME": t.TempDir()}
	_, err := ResolveWorkspaceRef(map[string]any{"workspace": "cloud"}, env, IO{Now: time.Now}, nil)
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("no control plane: %v", err)
	}
}
