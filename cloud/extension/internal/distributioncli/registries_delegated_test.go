package distributioncli

import (
	"context"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/features/spectest"
)

// storeDelegatedSignIn replaces the fixture's sign-in with one a person's MCP
// connection approved: an actor, a workspace, the delegated MCP scopes and
// the MCP resource as audience. auth-server refuses such a token at
// POST /apikeys.
func storeDelegatedSignIn(t *testing.T, env map[string]string, now time.Time) {
	t.Helper()
	access := workflowJWT(map[string]any{
		"sub":       "person-1",
		"aud":       "https://mcp.putnami.test",
		"azp":       "putnami-cli",
		"act":       map[string]any{"sub": "agent-cli"},
		"scope":     "mcp.read mcp.act",
		"scope_ref": map[string]any{"workspace_id": "ws-consumer"},
		"exp":       now.Add(time.Hour).Unix(),
	})
	if err := clicore.WriteAuth(&clicore.StoredToken{
		AccessToken:  access,
		RefreshToken: "delegated-refresh-token",
		TokenType:    "Bearer",
		ExpiresAt:    now.Add(time.Hour).Format(time.RFC3339Nano),
	}, env); err != nil {
		t.Fatal(err)
	}
}

// apiKeyRequests counts the POST /apikeys requests the fake auth server saw.
func apiKeyRequests(server *providerAuthServer) int {
	count := 0
	for _, request := range server.requests {
		if request == "POST /apikeys" {
			count++
		}
	}
	return count
}

func TestProviderReadCredentialIsAbsentForADelegatedSignIn(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "a-delegated-session-mints-no-registry-token", "the-credential-provider-answers-absence")
	root, env, ioctx, server := providerCredentialFixture(t)
	storeDelegatedSignIn(t, env, ioctx.Now())

	credential, err := ProviderReadCredential(context.Background(), map[string]any{}, root, env, ioctx)
	if err != nil || credential != nil {
		t.Fatalf("credential = %+v, err = %v; want absence", credential, err)
	}
	if len(server.requests) != 0 {
		t.Fatalf("a delegated sign-in sent %v; want no request", server.requests)
	}
	state, err := readRegistriesState(env)
	if err != nil {
		t.Fatal(err)
	}
	if state != nil && len(state.Access) != 0 {
		t.Fatalf("absence cached a registry credential: %+v", state.Access)
	}
}

func TestRegistryTokenSendsNoAPIKeyRequestForADelegatedSignIn(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "a-delegated-session-mints-no-registry-token", "the-registry-token-mint-sends-no-api-key-request")
	for _, params := range []map[string]any{
		{"for": "npm"},
		{"for": "put"},
		{"for": "npm", "materialize": true},
	} {
		root, env, ioctx, server := providerCredentialFixture(t)
		storeDelegatedSignIn(t, env, ioctx.Now())
		var stdout []string
		ioctx.Stdout = func(line string) { stdout = append(stdout, line) }

		err := RegistryToken(params, root, env, ioctx)
		if clicore.ExitCode(err) != clicore.ExitAuth {
			t.Fatalf("%v: error = %v, want exit %d", params, err, clicore.ExitAuth)
		}
		message := err.Error()
		if !strings.Contains(message, "delegated through an MCP connection (agent-cli) for workspace ws-consumer") {
			t.Fatalf("%v: error = %q, want it to name the delegated session", params, message)
		}
		// The registry credential seam reads an auth exit as absence; a
		// sign-in hint would tell it to sign in, which changes nothing here.
		for _, marker := range []string{"not signed in", "not logged in", "cloud login", "no session"} {
			if strings.Contains(strings.ToLower(message), marker) {
				t.Fatalf("%v: error = %q mentions %q", params, message, marker)
			}
		}
		if len(server.requests) != 0 || len(stdout) != 0 {
			t.Fatalf("%v: sent %v and printed %v; want no request and no bearer", params, server.requests, stdout)
		}
	}
}

func TestReleaseSetCredentialSendsNoAPIKeyRequestForADelegatedSignIn(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "a-delegated-session-mints-no-registry-token", "the-registry-token-mint-sends-no-api-key-request")
	root, env, ioctx, server := providerCredentialFixture(t)
	storeDelegatedSignIn(t, env, ioctx.Now())

	_, err := resolveReleaseSetProviderCredential(map[string]any{}, root, env, ioctx)
	if clicore.ExitCode(err) != clicore.ExitAuth || apiKeyRequests(server) != 0 {
		t.Fatalf("error = %v, api key requests = %d; want an auth error and none", err, apiKeyRequests(server))
	}
}

func TestAnOwnSignInStillMintsThroughOneAPIKey(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "a-delegated-session-mints-no-registry-token", "an-own-session-still-mints-one-api-key")
	t.Run("credential provider", func(t *testing.T) {
		root, env, ioctx, server := providerCredentialFixture(t)
		credential, err := ProviderReadCredential(context.Background(), map[string]any{}, root, env, ioctx)
		if err != nil || credential == nil {
			t.Fatalf("credential = %+v, err = %v; want a bearer", credential, err)
		}
		if got := apiKeyRequests(server); got != 1 {
			t.Fatalf("api key requests = %d, want 1", got)
		}
	})
	t.Run("registry token", func(t *testing.T) {
		root, env, ioctx, server := providerCredentialFixture(t)
		var stdout []string
		ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
		if err := RegistryToken(map[string]any{"for": "npm"}, root, env, ioctx); err != nil {
			t.Fatalf("RegistryToken: %v", err)
		}
		if len(stdout) != 1 || stdout[0] != server.bearer {
			t.Fatalf("stdout = %v, want the minted bearer", stdout)
		}
		if got := apiKeyRequests(server); got != 1 {
			t.Fatalf("api key requests = %d, want 1", got)
		}
	})
}
