package configcli

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

// This file collects the shared test fixtures the config-domain command tests
// build on. Several duplicate helpers of internal/cloudcli's cli_test.go:
// writeTestAuth, tokenResponse, jwt, base64url, jsonResponse, writeJSONFile,
// decodeJSON, mustMkdir, assertContains, plus the testBaseURL constant. The
// remainder (commonIO, writeLinkFile, writeProjectFile, writeFile) serve the
// secrets and config tests. The run* harnesses replace the aggregator's
// RunCommand: they replicate its pre-dispatch (flag parse + IO defaulting +
// workspace-root from env) and call the exported domain handler directly, since RunCommand/RunMain
// live in the aggregator (cloudcli), not here.

const testBaseURL = "https://control.test"

// runConfig mirrors the aggregator's RunCommand("config", …): it parses the
// CLI flags, applies the IO defaults the handler relies on, resolves the
// workspace root from PUTNAMI_WORKSPACE_ROOT, and dispatches Config.
func runConfig(ioctx clicore.IO, args []string) error {
	return runDomain(Config, ioctx, args)
}

func runSecrets(ioctx clicore.IO, args []string) error {
	return runDomain(Secrets, ioctx, args)
}

func runPublishConfig(ioctx clicore.IO, args []string) error {
	return runDomain(PublishConfig, ioctx, args)
}

func runDomain(handler func(map[string]any, []string, string, map[string]string, clicore.IO) error, ioctx clicore.IO, args []string) error {
	env := ioctx.Env
	params := clicore.MergeParams(clicore.ParseFlags(args))
	workspaceRoot := clicore.EnvGet(env, "PUTNAMI_WORKSPACE_ROOT")
	if ioctx.Stdout == nil {
		ioctx.Stdout = func(string) {}
	}
	if ioctx.Stderr == nil {
		ioctx.Stderr = func(string) {}
	}
	if ioctx.Client == nil {
		ioctx.Client = http.DefaultClient
	}
	if ioctx.Now == nil {
		ioctx.Now = time.Now
	}
	return handler(params, args, workspaceRoot, env, ioctx)
}

// commonIO builds the IO bundle reused by the table-style assertions in
// these tests. Stdout/Stderr capture into the returned slices.
func commonIO(t *testing.T, home string, client *http.Client) (clicore.IO, *[]string, *[]string) { //nolint:unparam // test helper returns both stdout and stderr capture buffers for symmetry; some callers only assert stdout
	t.Helper()
	stdout := &[]string{}
	stderr := &[]string{}
	ioctx := clicore.IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL": testBaseURL,
		}),
		Stdout: func(s string) { *stdout = append(*stdout, s) },
		Stderr: func(s string) { *stderr = append(*stderr, s) },
		Client: client,
		Now:    func() time.Time { return time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC) },
	}
	return ioctx, stdout, stderr
}

func writeLinkFile(t *testing.T, workspaceRoot string) {
	t.Helper()
	writeJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), map[string]any{
		"name": "test-workspace",
		"options": map[string]any{
			"@putnami/cloud": map[string]any{
				"workspace": map[string]any{
					"version":           1,
					"control_plane_url": testBaseURL,
					"workspace_id":      "ws-acme",
					"environment":       "prod",
				},
			},
		},
	})
	mustMkdir(t, filepath.Join(workspaceRoot, ".putnami"))
	writeJSONFile(t, filepath.Join(workspaceRoot, clicore.LinkFileRelative), map[string]any{
		"version":           1,
		"control_plane_url": testBaseURL,
		"workspace_id":      "ws-acme",
		"environment":       "prod",
	})
}

func writeProjectFile(t *testing.T, workspaceRoot, name string) { //nolint:unparam // test helper keeps name parameterized to mirror the aggregator fixture; current callers all seed "my-app"
	t.Helper()
	writeJSONFile(t, filepath.Join(workspaceRoot, "putnami.json"), map[string]any{
		"name": name,
	})
}

func writeFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o600)
}

func writeTestAuth(t *testing.T, home string) {
	t.Helper()
	file := filepath.Join(home, clicore.AuthFileRelative)
	mustMkdir(t, filepath.Dir(file))
	writeJSONFile(t, file, map[string]any{
		"access_token":  tokenResponse(nil)["access_token"],
		"refresh_token": "refresh-token",
		"token_type":    "Bearer",
		"expires_at":    "2026-04-30T13:00:00.000Z",
		"issuer":        testBaseURL,
		"client_id":     "putnami-cli",
	})
}

func tokenResponse(extra map[string]any) map[string]any {
	claims := map[string]any{
		"sub":      "user-1",
		"email":    "dev@example.com",
		"name":     "Dev User",
		"provider": "github",
		"scope":    "openid profile email",
	}
	for k, v := range extra {
		claims[k] = v
	}
	return map[string]any{
		"access_token":  jwt(claims),
		"refresh_token": "refresh-token",
		"id_token":      jwt(map[string]any{"sub": "user-1", "scope_ref": map[string]any{"workspace_id": "ws-acme"}}),
		"token_type":    "Bearer",
		"expires_in":    300,
	}
}

func jwt(payload map[string]any) string {
	return base64url(map[string]any{"alg": "none", "typ": "JWT"}) + "." + base64url(payload) + ".sig"
}

func base64url(value map[string]any) string {
	data, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(data)
}

func jsonResponse(status int, body any) *http.Response {
	data, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(data)),
	}
}

func writeJSONFile(t *testing.T, file string, data any) {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("encode json: %v", err)
	}
	if err := os.WriteFile(file, encoded, 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
}

func decodeJSON(t *testing.T, text string, dest any) {
	t.Helper()
	data := []byte(text)
	if decodeResultData(t, data, dest) {
		return
	}
	if err := json.Unmarshal(data, dest); err != nil {
		t.Fatalf("decode json %q: %v", text, err)
	}
}

func decodeResultData(t *testing.T, data []byte, dest any) bool {
	t.Helper()
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	if _, ok := probe["status"]; !ok {
		return false
	}
	if _, ok := probe["exitCode"]; !ok {
		return false
	}
	payload, ok := probe["data"]
	if !ok {
		payload = []byte("null")
	}
	if err := json.Unmarshal(payload, dest); err != nil {
		t.Fatalf("decode result data %q: %v", string(payload), err)
	}
	return true
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func assertContains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("expected %q to contain %q", text, want)
	}
}
