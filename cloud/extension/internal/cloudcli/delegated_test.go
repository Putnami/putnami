package cloudcli

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/features/spectest"
)

func delegatedTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// delegatedLogsWorkspace links a repository to ws-acme on a control plane
// that refuses every route, signs the machine in with a session whose
// claims are delegated through an MCP connection when delegated is true, and
// a person's own session otherwise, and returns the env and the root.
func delegatedLogsWorkspace(t *testing.T, delegated bool) (map[string]string, string) {
	t.Helper()
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Authentication required"}`))
	}))
	t.Cleanup(controlPlane.Close)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".putnami"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(root, ".putnami", "cloud-link.json"), map[string]any{
		"workspace_id": "ws-acme", "control_plane_url": controlPlane.URL, "environment": "prod", "version": 1,
	})
	home := t.TempDir()
	env := map[string]string{"PUTNAMI_HOME": home, "HOME": home, "PUTNAMI_WORKSPACE_ROOT": root, "NO_COLOR": "1"}
	claims := map[string]any{"sub": "person-1", "scope_ref": map[string]any{"workspace_id": "ws-acme"}}
	if delegated {
		claims["act"] = map[string]any{"sub": "agent-cli"}
		claims["scope"] = "mcp.read mcp.act"
	}
	if err := clicore.WriteAuth(&clicore.StoredToken{
		AccessToken: delegatedTestJWT(t, claims), RefreshToken: "refresh", TokenType: "Bearer",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), Issuer: "https://auth.example.test",
	}, env); err != nil {
		t.Fatal(err)
	}
	return env, root
}

func runLogs(t *testing.T, env map[string]string) (int, string) {
	t.Helper()
	var stderr []string
	code := RunMain([]string{"logs", "--app", "api"}, IO{
		Env:    env,
		Stdout: func(string) {},
		Stderr: func(line string) { stderr = append(stderr, line) },
	})
	return code, strings.Join(stderr, "\n")
}

func TestLogsRefusedForADelegatedSessionNamesTheReasonAndTheReadTool(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "a-delegated-session-says-what-it-reaches", "a-refused-route-names-the-delegation-and-the-read-tool")
	t.Cleanup(clicore.ForgetSentDelegation)

	env, _ := delegatedLogsWorkspace(t, true)
	code, stderr := runLogs(t, env)
	if code != clicore.ExitAuth {
		t.Fatalf("exit = %d, stderr %s", code, stderr)
	}
	for _, want := range []string{"refused (401 Unauthorized)", "delegated through an MCP connection (agent-cli) for workspace ws-acme", "MCP tool query_logs"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr misses %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "not signed in") || strings.Contains(stderr, "putnami cloud login") {
		t.Fatalf("a delegated refusal asks to sign in again:\n%s", stderr)
	}

	// A person's own session keeps today's refusal.
	env, _ = delegatedLogsWorkspace(t, false)
	code, stderr = runLogs(t, env)
	if code != clicore.ExitAuth || strings.Contains(stderr, "MCP") || !strings.Contains(stderr, "logs: 401 Unauthorized") {
		t.Fatalf("own session: exit %d, stderr %s", code, stderr)
	}
}

func TestStatusEntriesOnARefusedRouteAreUnknownForADelegatedSession(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "a-delegated-session-says-what-it-reaches", "a-status-entry-on-a-refused-route-is-unknown")
	t.Cleanup(clicore.ForgetSentDelegation)

	env, root := delegatedLogsWorkspace(t, true)
	nodes := collectStatus(map[string]any{}, root, env, IO{Env: env, Client: http.DefaultClient, Now: time.Now})
	byID := map[string]clicore.StatusNode{}
	for _, node := range nodes {
		byID[node.ID] = node
	}
	whoami := byID["whoami"]
	if whoami.State != clicore.StatusOK || !strings.Contains(whoami.Detail, "through an MCP connection (agent-cli)") {
		t.Fatalf("whoami = %+v", whoami)
	}
	for _, id := range []string{"ci", "db"} {
		node := byID[id]
		if node.State != clicore.StatusUnknown || !strings.Contains(node.Detail, "admits only the routes delegated") ||
			!strings.Contains(node.Detail, clicore.DelegatedReadTool(id)) {
			t.Fatalf("%s = %+v", id, node)
		}
	}
	for _, node := range nodes {
		if node.State == clicore.StatusFailing && node.ID != "registries" {
			t.Fatalf("%s is failing for a delegated session: %+v", node.ID, node)
		}
	}
}
