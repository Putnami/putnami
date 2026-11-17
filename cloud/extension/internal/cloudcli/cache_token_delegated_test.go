package cloudcli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
	"go.putnami.dev/protocol/features/spectest"
)

// delegatedCacheClaims are the claims of a CLI session a person's MCP
// connection approved: an actor, a workspace, and the delegated scopes.
func delegatedCacheClaims(workspaceID string, expires time.Time, marker string) map[string]any {
	return map[string]any{
		"sub":       "person-1",
		"aud":       "https://mcp.putnami.test",
		"azp":       "putnami-cli",
		"act":       map[string]any{"sub": "agent-cli"},
		"scope":     "mcp.read mcp.act",
		"scope_ref": map[string]any{"workspace_id": workspaceID},
		"marker":    marker,
		"iat":       fixedNow().Unix(),
		"exp":       expires.Unix(),
	}
}

// delegatedCacheWorkspace links a workspace to ws-acme and stores a delegated
// session bound to sessionWorkspace whose access token expires at expires.
func delegatedCacheWorkspace(t *testing.T, sessionWorkspace string, expires time.Time) (map[string]string, string) {
	t.Helper()
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeLinkFile(t, workspaceRoot)
	access := jwt(delegatedCacheClaims(sessionWorkspace, expires, "stored"))
	file := filepath.Join(home, AuthFileRelative)
	mustMkdir(t, filepath.Dir(file))
	writeJSONFile(t, file, map[string]any{
		"access_token":  access,
		"refresh_token": "delegated-refresh-token",
		"token_type":    "Bearer",
		"expires_at":    expires.UTC().Format(time.RFC3339Nano),
		"issuer":        testBaseURL,
		"client_id":     "putnami-cli",
	})
	env := hometest.Env(home, map[string]string{
		"PUTNAMI_AUTH_URL":       testBaseURL,
		"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
	})
	return env, access
}

func runCacheToken(t *testing.T, env map[string]string, fake *fakeTransport) ([]string, error) {
	t.Helper()
	t.Cleanup(clicore.ForgetSentDelegation)
	var output []string
	err := RunCommand("token", IO{
		Env:    env,
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--for", "cache"})
	return output, err
}

// assertNoAPIKey fails when the command created, exchanged or revoked an API
// key: a delegated session may not create one.
func assertNoAPIKey(t *testing.T, fake *fakeTransport) {
	t.Helper()
	if len(fake.mintedKeys) != 0 || len(fake.revokedIDs) != 0 {
		t.Fatalf("delegated session minted keys %v, revoked %v; want none", fake.mintedKeys, fake.revokedIDs)
	}
	for _, request := range fake.requests {
		if strings.HasPrefix(request.Path, "/apikeys") || request.Body["grant_type"] == apiKeyGrantType {
			t.Fatalf("delegated session sent %s %s; want no API key call", request.Method, request.Path)
		}
	}
}

func TestCloudTokenForCachePrintsTheDelegatedAccessToken(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "a-delegated-session-reads-the-cache-with-its-own-token", "the-recipe-prints-the-delegated-access-token")
	env, access := delegatedCacheWorkspace(t, "ws-acme", fixedNow().Add(time.Hour))
	fake := newFakeTransport(t)

	output, err := runCacheToken(t, env, fake)
	if err != nil {
		t.Fatalf("cloud token --for cache: %v", err)
	}
	if len(output) != 1 || output[0] != access {
		t.Fatalf("output = %v, want the delegated access token alone", output)
	}
	assertNoAPIKey(t, fake)
	if len(fake.requests) != 0 {
		t.Fatalf("a fresh delegated session sent %d requests, want none", len(fake.requests))
	}
}

func TestCloudTokenForCacheRefreshesAnExpiredDelegatedSession(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "a-delegated-session-reads-the-cache-with-its-own-token", "the-recipe-refreshes-an-expired-delegated-session")
	env, stale := delegatedCacheWorkspace(t, "ws-acme", fixedNow().Add(-time.Minute))
	refreshed := jwt(delegatedCacheClaims("ws-acme", fixedNow().Add(time.Hour), "refreshed"))
	fake := newFakeTransport(t)
	fake.refreshResponse = map[string]any{"access_token": refreshed, "refresh_token": "delegated-refresh-token-2"}

	output, err := runCacheToken(t, env, fake)
	if err != nil {
		t.Fatalf("cloud token --for cache: %v", err)
	}
	if len(output) != 1 || output[0] != refreshed {
		t.Fatalf("output = %v, want the refreshed delegated access token", output)
	}
	if output[0] == stale {
		t.Fatal("the recipe printed the expired access token")
	}
	refreshes := 0
	for _, request := range fake.requests {
		if request.Path == "/token" && request.Body["grant_type"] == "refresh_token" {
			refreshes++
		}
	}
	if refreshes != 1 {
		t.Fatalf("refresh requests = %d, want 1", refreshes)
	}
	assertNoAPIKey(t, fake)
	stored, err := readAuth(env, true)
	if err != nil {
		t.Fatalf("read auth: %v", err)
	}
	if stored.AccessToken != refreshed || stored.RefreshToken != "delegated-refresh-token-2" {
		t.Fatalf("stored session was not renewed: access=%q refresh=%q", stored.AccessToken, stored.RefreshToken)
	}
}

func TestCloudTokenForCacheRefusesADelegatedSessionOfAnotherWorkspace(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "a-delegated-session-reads-the-cache-with-its-own-token", "a-delegated-session-of-another-workspace-is-refused")
	env, _ := delegatedCacheWorkspace(t, "ws-other", fixedNow().Add(time.Hour))
	fake := newFakeTransport(t)

	output, err := runCacheToken(t, env, fake)
	if err == nil {
		t.Fatal("cloud token --for cache succeeded for a session delegated to another workspace")
	}
	// The cache provider's token-command classifier matches this phrase.
	for _, want := range []string{"session is delegated for workspace ws-other", "ws-acme"} {
		assertContains(t, err.Error(), want)
	}
	if clicore.ExitCode(err) != ExitAuth {
		t.Fatalf("exit code = %d, want %d", clicore.ExitCode(err), ExitAuth)
	}
	if len(output) != 0 {
		t.Fatalf("refused recipe printed %v, want no bearer", output)
	}
	assertNoAPIKey(t, fake)
}
