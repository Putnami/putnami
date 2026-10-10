package observabilitycli

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// These tests exercise the REAL command entry points (Logs/Traces/Metrics),
// not the white-box query builders, so deleting any setServiceFilter wiring line
// in an entry point fails a test. They stand up a workspace root whose manifest
// link points at the httptest server and a PUTNAMI_HOME holding a fresh
// workspace-scoped token, then assert the outgoing request carries
// service=<app> (emit==filter parity).

// writeTestJSON writes v as indented JSON to path (0600), failing the test on
// error.
func writeTestJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// mkWorkspaceJWT builds an unsigned JWT whose scope_ref.workspace_id claim equals
// workspaceID, so WorkspaceAuth serves it from the workspace cache without a
// network round-trip (the CLI only reads the non-security-critical scope claim).
func mkWorkspaceJWT(t *testing.T, workspaceID string) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal jwt part: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header := enc(map[string]any{"alg": "none", "typ": "JWT"})
	payload := enc(map[string]any{"scope_ref": map[string]any{"workspace_id": workspaceID}})
	return header + "." + payload + ".sig"
}

// entryHarness wires a real entry point to srv: a workspace root whose manifest
// link points at srv, plus a PUTNAMI_HOME holding a fresh workspace-scoped token.
// It returns the workspaceRoot, env, and ioctx the entry points take. app is
// passed as the positional so ResolveApp returns it verbatim (no local
// putnami.json is needed under the tempdir).
func entryHarness(t *testing.T, srv *httptest.Server) (string, map[string]string, clicore.IO) {
	t.Helper()
	root := t.TempDir()
	writeTestJSON(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{
		"name": "acme-workspace",
		"options": map[string]any{
			"@putnami/cloud": map[string]any{
				"workspace": map[string]any{
					"workspace_id":      "ws-acme",
					"control_plane_url": srv.URL,
				},
			},
		},
	})

	home := t.TempDir()
	token := mkWorkspaceJWT(t, "ws-acme")
	expires := fixedNow().Add(time.Hour).Format(time.RFC3339Nano)
	writeTestJSON(t, filepath.Join(home, "auth.json"), clicore.StoredToken{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresAt:   expires,
		WorkspaceAccess: map[string]clicore.StoredWorkspaceAccess{
			"ws-acme": {AccessToken: token, TokenType: "Bearer", ExpiresAt: expires},
		},
	})

	env := map[string]string{"PUTNAMI_HOME": home, "NO_COLOR": "1"}
	ioctx := clicore.IO{
		Client: srv.Client(),
		Env:    env,
		Now:    fixedNow,
		Stdout: func(string) {},
		Stderr: func(string) {},
	}
	return root, env, ioctx
}

// jsonListServer returns a server that records the incoming path + service query
// and replies with an empty list under itemKey.
func jsonListServer(t *testing.T, gotPath, gotService *string, itemKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath = r.URL.Path
		*gotService = r.URL.Query().Get("service")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"` + itemKey + `":[]}`))
	}))
}

const entryApp = "apps/api"

// TestLogsEntryPointSendsServiceFilter pins that Logs() itself wires the service
// filter: removing setServiceFilter(base, ctx.app) from Logs fails this test.
func TestLogsEntryPointSendsServiceFilter(t *testing.T) {
	var gotPath, gotService string
	srv := jsonListServer(t, &gotPath, &gotService, "entries")
	defer srv.Close()

	root, env, ioctx := entryHarness(t, srv)
	if err := Logs(map[string]any{"output": "jsonl"}, []string{entryApp}, root, env, ioctx); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if gotPath != "/v1/workspaces/ws-acme/logs" {
		t.Fatalf("path = %q, want /v1/workspaces/ws-acme/logs", gotPath)
	}
	if gotService != entryApp {
		t.Fatalf("outgoing service = %q, want %q (Logs must wire setServiceFilter)", gotService, entryApp)
	}
}

// TestTracesEntryPointSendsServiceFilter is the traces twin of the logs test.
func TestTracesEntryPointSendsServiceFilter(t *testing.T) {
	var gotPath, gotService string
	srv := jsonListServer(t, &gotPath, &gotService, "traces")
	defer srv.Close()

	root, env, ioctx := entryHarness(t, srv)
	if err := Traces(map[string]any{"output": "jsonl"}, []string{entryApp}, root, env, ioctx); err != nil {
		t.Fatalf("Traces: %v", err)
	}
	if gotPath != "/v1/workspaces/ws-acme/traces" {
		t.Fatalf("path = %q, want /v1/workspaces/ws-acme/traces", gotPath)
	}
	if gotService != entryApp {
		t.Fatalf("outgoing service = %q, want %q (Traces must wire setServiceFilter)", gotService, entryApp)
	}
}

// TestMetricsEntryPointSendsServiceFilter is the metrics twin of the logs test.
func TestMetricsEntryPointSendsServiceFilter(t *testing.T) {
	var gotPath, gotService string
	srv := jsonListServer(t, &gotPath, &gotService, "series")
	defer srv.Close()

	root, env, ioctx := entryHarness(t, srv)
	if err := Metrics(map[string]any{"output": "jsonl"}, []string{entryApp}, root, env, ioctx); err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if gotPath != "/v1/workspaces/ws-acme/metrics" {
		t.Fatalf("path = %q, want /v1/workspaces/ws-acme/metrics", gotPath)
	}
	if gotService != entryApp {
		t.Fatalf("outgoing service = %q, want %q (Metrics must wire setServiceFilter)", gotService, entryApp)
	}
}

// TestLogsFollowEntryPointSendsServiceFilter covers the --follow tail path: the
// SSE tail query must also carry service=<app>, so removing
// setServiceFilter(tailQuery, ctx.app) from Logs fails this test. The server
// records the query then returns a clean EOF; with the reconnect budget shrunk to
// zero, followLogs exits on its own without needing an interrupt.
func TestLogsFollowEntryPointSendsServiceFilter(t *testing.T) {
	prevMax, prevBackoff := followMaxReconnects, followBackoff
	followMaxReconnects, followBackoff = 0, time.Millisecond
	defer func() { followMaxReconnects, followBackoff = prevMax, prevBackoff }()

	var gotPath, gotService string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotService = r.URL.Query().Get("service")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK) // clean EOF: no frames
	}))
	defer srv.Close()

	root, env, ioctx := entryHarness(t, srv)
	if err := Logs(map[string]any{"output": "jsonl", "follow": true}, []string{entryApp}, root, env, ioctx); err != nil {
		t.Fatalf("Logs --follow: %v", err)
	}
	if gotPath != "/v1/workspaces/ws-acme/logs/tail" {
		t.Fatalf("tail path = %q, want /v1/workspaces/ws-acme/logs/tail", gotPath)
	}
	if gotService != entryApp {
		t.Fatalf("outgoing tail service = %q, want %q (Logs --follow must wire setServiceFilter)", gotService, entryApp)
	}
}
