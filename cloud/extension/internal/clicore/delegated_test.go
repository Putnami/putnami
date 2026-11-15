package clicore

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func delegatedTestToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func delegatedTestClaims() map[string]any {
	return map[string]any{
		"sub":       "person-1",
		"scope":     "mcp.read mcp.act",
		"act":       map[string]any{"sub": "agent-cli"},
		"scope_ref": map[string]any{"workspace_id": "ws-acme"},
	}
}

func TestDelegationFromClaims(t *testing.T) {
	delegation, ok := DelegationFromClaims(delegatedTestClaims())
	if !ok || delegation.Actor != "agent-cli" || delegation.WorkspaceID != "ws-acme" || delegation.Subject != "person-1" ||
		strings.Join(delegation.Scopes, " ") != "mcp.read mcp.act" {
		t.Fatalf("delegation = %+v, %v", delegation, ok)
	}
	own := delegatedTestClaims()
	delete(own, "act")
	if _, ok := DelegationFromClaims(own); ok {
		t.Fatal("a workspace token without an actor is a person's own session")
	}
	unscoped := delegatedTestClaims()
	delete(unscoped, "scope_ref")
	if _, ok := DelegationFromClaims(unscoped); ok {
		t.Fatal("an actor without a workspace scope is not a delegated session")
	}
	if _, ok := DelegationOf(nil); ok {
		t.Fatal("no stored session is not delegated")
	}
}

func TestDelegatedRefusalNamesTheReasonAndTheReadTool(t *testing.T) {
	t.Cleanup(ForgetSentDelegation)
	refused := unexpectedStatusError("https://api.example.test/v1/workspaces/ws-acme/logs", http.StatusUnauthorized, "", "Authentication required")

	recordDelegation(&StoredToken{AccessToken: delegatedTestToken(t, map[string]any{"sub": "person-1"})})
	if got := DelegatedRefusal(refused, "logs"); !errors.Is(got, refused) {
		t.Fatalf("a person's own session keeps its refusal: %v", got)
	}

	recordDelegation(&StoredToken{AccessToken: delegatedTestToken(t, delegatedTestClaims())})
	got := DelegatedRefusal(refused, "logs")
	if !IsDelegatedRefusal(got) || ExitCode(got) != ExitAuth {
		t.Fatalf("refusal = %v (exit %d)", got, ExitCode(got))
	}
	for _, want := range []string{"refused (401 Unauthorized)", "delegated through an MCP connection (agent-cli) for workspace ws-acme", "MCP tool query_logs, or list_errors for the errors"} {
		if !strings.Contains(got.Error(), want) {
			t.Fatalf("refusal %q misses %q", got.Error(), want)
		}
	}
	if strings.Contains(got.Error(), "not signed in") || !errors.Is(DelegatedRefusal(got, "logs"), got) {
		t.Fatalf("refusal = %v", got)
	}
	var apiErr *APIError
	if !errors.As(got, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the refusal keeps its cause: %v", got)
	}

	forbidden := NewError("ci runs: 403 Forbidden", ExitAPI)
	got = DelegatedRefusal(forbidden, "ci")
	if !strings.Contains(got.Error(), "refused (403 Forbidden)") || !strings.Contains(got.Error(), "MCP tool list_runs or get_run") || ExitCode(got) != ExitAPI {
		t.Fatalf("forbidden = %v", got)
	}
	got = DelegatedRefusal(NewError("secrets: 403 Forbidden", ExitAPI), "secrets")
	if !strings.Contains(got.Error(), "a person's own `putnami cloud login` session reaches it") {
		t.Fatalf("no read tool = %v", got)
	}

	for _, other := range []error{
		NewError("request failed: 500 Internal Server Error", ExitAPI),
		NewOAuthTokenError("invalid_grant", "refresh token revoked", ExitAuth),
		&refreshError{err: unexpectedStatusError("https://auth.example.test/token", http.StatusUnauthorized, "", "invalid_client")},
		nil,
	} {
		if got := DelegatedRefusal(other, "logs"); !errors.Is(got, other) {
			t.Fatalf("%v rewritten as %v", other, got)
		}
	}

	recordDelegation(nil)
	if got := DelegatedRefusal(refused, "logs"); !errors.Is(got, refused) {
		t.Fatalf("PUTNAMI_CLOUD_TOKEN keeps its refusal: %v", got)
	}
}

func TestDelegatedReadTool(t *testing.T) {
	for name, want := range map[string]string{
		"logs": "query_logs, or list_errors for the errors", "errors": "", "metrics": "query_metrics", "traces": "query_traces",
		"ci": "list_runs or get_run", "env": "list_environments or list_releases", "env.production": "list_environments or list_releases",
		"config": "get_configuration", "db": "list_databases", "secrets": "", "": "",
	} {
		if got := DelegatedReadTool(name); got != want {
			t.Fatalf("DelegatedReadTool(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestADelegatedStatusEntryIsUnknownWithTheReason(t *testing.T) {
	t.Cleanup(ForgetSentDelegation)
	recordDelegation(&StoredToken{AccessToken: delegatedTestToken(t, delegatedTestClaims())})
	refused := NewError("status: 403 Forbidden", ExitAPI)
	node := UnknownStatus("db", "db", refused, "putnami cloud db status")
	if node.State != StatusUnknown || node.Fix != "putnami cloud db status" || !strings.Contains(node.Detail, "MCP tool list_databases") {
		t.Fatalf("node = %+v", node)
	}
	node = UserSessionStatus("token", "token", NewError("request failed: 401 Unauthorized", ExitAuth), map[string]string{"PUTNAMI_HOME": t.TempDir()})
	if node.State != StatusUnknown || node.Fix != "" || !strings.Contains(node.Detail, "admits only the routes delegated") {
		t.Fatalf("user session node = %+v", node)
	}
}

func TestAFailedRefreshOfADelegatedSessionIsNoRouteRefusal(t *testing.T) {
	t.Cleanup(ForgetSentDelegation)
	ForgetSentDelegation()
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"blocked"}`))
	}))
	t.Cleanup(tokenEndpoint.Close)
	home := t.TempDir()
	env := map[string]string{"PUTNAMI_HOME": home, "PUTNAMI_AUTH_URL": tokenEndpoint.URL}
	now := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	if err := WriteAuth(&StoredToken{
		AccessToken: delegatedTestToken(t, delegatedTestClaims()), RefreshToken: "refresh", TokenType: "Bearer",
		ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339Nano), Issuer: tokenEndpoint.URL,
	}, env); err != nil {
		t.Fatal(err)
	}
	ioctx := IO{Client: tokenEndpoint.Client(), Now: func() time.Time { return now }}

	_, err := WorkspaceAuth(nil, env, ioctx, "ws-acme")
	if err == nil {
		t.Fatal("a refused refresh returned a session")
	}
	if got := DelegatedRefusal(err, "logs"); !errors.Is(got, err) {
		t.Fatalf("a refused refresh was read as a route refusal: %v", got)
	}
	if _, ok := SentDelegation(); ok {
		t.Fatal("a refused refresh recorded the session as sent")
	}
	_, err = ActiveAuth(nil, env, ioctx)
	if err == nil || IsDelegatedRefusal(DelegatedRefusal(err, "logs")) {
		t.Fatalf("ActiveAuth refresh failure = %v", err)
	}
}

func TestADelegatedRefusalNamesTheCredentialWhenBothAreHeld(t *testing.T) {
	t.Cleanup(ForgetSentDelegation)
	t.Cleanup(func() { usedCredential.Store(nil) })
	recordDelegation(&StoredToken{AccessToken: delegatedTestToken(t, delegatedTestClaims())})
	usedCredential.Store(&Credential{TokenSet: true, Stored: true})
	got := DelegatedRefusal(NewError("logs: 401 Unauthorized", ExitAuth), "logs")
	if !strings.Contains(got.Error(), "credential: the signed-in session, which wins over PUTNAMI_CLOUD_TOKEN") {
		t.Fatalf("refusal = %v", got)
	}
	if !errors.Is(NameCredential(got), got) {
		t.Fatal("the credential was named twice")
	}
}
