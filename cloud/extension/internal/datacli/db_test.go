package datacli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
)

const (
	testBaseURL     = "https://control.test"
	testWorkspace   = "ws-acme"
	testDatabase    = "mydb"
	oneTimePassword = "one-time-secret-Xy9$zqk" //nolint:gosec // G101: test fixture, not a real credential.
)

// dbFakeServer is an in-memory stand-in for the control plane's database-access
// grant/inventory/query endpoints plus the auth-refresh endpoints the shared
// workspace seam needs.
type dbFakeServer struct {
	requests []capturedRequest
	// activeGrant, when set, makes the inventory annotate the database with the
	// caller's active grant so the grant-gated `db info` metrics path is exercised.
	activeGrant bool
	// overrideEmptyInventory, when set, makes the inventory return zero databases
	// so the empty-state rendering path is exercised.
	overrideEmptyInventory bool
	// queryStatus, when non-zero and not 200, makes the Lane-0 query route reply
	// with that status and an {"error"} body (e.g. 403 for a lapsed grant, 500 for
	// a genuine failure) instead of the metrics result.
	queryStatus int
	// bindings maps "<project> <environment>" to the bindings the
	// database-bindings route answers; bindingStatus maps it to a refusal.
	bindings      map[string][]map[string]any
	bindingStatus map[string]int
	mu            sync.Mutex
}

type capturedRequest struct {
	Method string
	Path   string
	Auth   string
	Body   map[string]any
}

func (s *dbFakeServer) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	inventoryPath := "/v1/workspaces/" + testWorkspace + "/databases"
	grantsPath := inventoryPath + "/" + testDatabase + "/grants"
	queryPath := inventoryPath + "/" + testDatabase + "/query"
	switch {
	case req.URL.Path == "/.well-known/openid-configuration":
		return jsonResponse(http.StatusOK, map[string]any{
			"issuer": testBaseURL, "token_endpoint": testBaseURL + "/token",
			"userinfo_endpoint": testBaseURL + "/userinfo",
		}), nil
	case req.URL.Path == "/token":
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)
		extra := map[string]any{}
		if workspaceID := clicore.StringValue(body["workspace_id"]); workspaceID != "" {
			extra["scope_ref"] = map[string]any{"workspace_id": workspaceID}
		}
		return jsonResponse(http.StatusOK, tokenResponseWith(extra)), nil
	case req.Method == http.MethodPost && req.URL.Path == grantsPath:
		s.record(req, bodyBytes)
		return jsonResponse(http.StatusCreated, map[string]any{
			"grant_id":   "grant_ab12",
			"scope_kind": "workspace",
			"database":   testDatabase,
			"level":      "read",
			"username":   "u_dev",
			"password":   oneTimePassword,
			"expires_at": "2026-05-19T10:00:00Z",
		}), nil
	case req.Method == http.MethodGet && req.URL.Path == grantsPath:
		s.record(req, bodyBytes)
		return jsonResponse(http.StatusOK, map[string]any{
			"grants": []map[string]any{
				{
					"id": "grant_ab12", "scope_kind": "workspace", "database": testDatabase,
					"level": "read", "status": "active", "reason": "debug incident",
					"requestor_id": "user-1", "requested_at": "2026-05-19T09:00:00Z",
					"expires_at": "2026-05-19T10:00:00Z",
				},
			},
		}), nil
	case req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, grantsPath+"/"):
		s.record(req, bodyBytes)
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Status:     http.StatusText(http.StatusNoContent),
			Header:     http.Header{},
			Body:       io.NopCloser(bytes.NewReader(nil)),
		}, nil
	case req.Method == http.MethodGet && req.URL.Path == inventoryPath:
		s.record(req, bodyBytes)
		if s.overrideEmptyInventory {
			return jsonResponse(http.StatusOK, map[string]any{"databases": []map[string]any{}}), nil
		}
		item := map[string]any{
			"database":                 testDatabase,
			"engine":                   "postgres",
			"instance_connection_name": "proj:region:inst",
			"schemas":                  []string{"public", "app"},
		}
		if s.activeGrant {
			item["active_grant"] = map[string]any{
				"id": "grant_ab12", "scope_kind": "workspace", "database": testDatabase,
				"level": "read", "status": "active", "reason": "debug incident",
				"requestor_id": "user-1", "requested_at": "2026-05-19T09:00:00Z",
				"expires_at": "2026-05-19T10:00:00Z",
			}
		}
		return jsonResponse(http.StatusOK, map[string]any{"databases": []map[string]any{item}}), nil
	case req.Method == http.MethodGet && req.URL.Path == "/v1/workspaces/"+testWorkspace+"/database-bindings":
		s.record(req, bodyBytes)
		key := req.URL.Query().Get("project") + " " + req.URL.Query().Get("environment")
		if status := s.bindingStatus[key]; status != 0 {
			return jsonResponse(status, map[string]any{"error": http.StatusText(status)}), nil
		}
		bindings := s.bindings[key]
		if bindings == nil {
			bindings = []map[string]any{}
		}
		return jsonResponse(http.StatusOK, map[string]any{"bindings": bindings}), nil
	case req.Method == http.MethodPost && req.URL.Path == queryPath:
		s.record(req, bodyBytes)
		if s.queryStatus != 0 && s.queryStatus != http.StatusOK {
			return jsonResponse(s.queryStatus, map[string]any{
				"error": "no active grant for this database; open an access grant to enable queries",
			}), nil
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"columns": []map[string]any{
				{"name": "size", "data_type": "text"},
				{"name": "size_bytes", "data_type": "int8"},
				{"name": "tables", "data_type": "int8"},
				{"name": "live_rows", "data_type": "int8"},
				{"name": "connections", "data_type": "int8"},
			},
			"rows":      []map[string]any{{"cells": []any{"12 MB", "12582912", "7", "1234", "3"}}},
			"row_count": 1,
			"truncated": false,
		}), nil
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "not found"}), nil
	}
}

func (s *dbFakeServer) record(req *http.Request, bodyBytes []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var body map[string]any
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &body)
	}
	s.requests = append(s.requests, capturedRequest{
		Method: req.Method,
		Path:   req.URL.Path,
		Auth:   req.Header.Get("Authorization"),
		Body:   body,
	})
}

func newDBTestIO(t *testing.T, fake *dbFakeServer) (clicore.IO, string, string) {
	t.Helper()
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	io := clicore.IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":       testBaseURL,
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Client: &http.Client{Transport: fake},
		Now:    func() time.Time { return time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC) },
	}
	return io, home, workspaceRoot
}

func runDB(ioctx clicore.IO, args []string) error {
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
	return DB(params, args, workspaceRoot, env, ioctx)
}

// TestGrantRequestPrintsCredentialOnceNeverOnDisk is the core secret-handling
// invariant: the one-time password is printed to stdout EXACTLY ONCE and never
// written to any file under HOME or the workspace.
func TestGrantRequestPrintsCredentialOnceNeverOnDisk(t *testing.T) {
	fake := &dbFakeServer{}
	ioctx, home, workspaceRoot := newDBTestIO(t, fake)
	var out []string
	ioctx.Stdout = func(s string) { out = append(out, s) }

	if err := runDB(ioctx, []string{"grant", "request", "--database", testDatabase, "--level", "read", "--reason", "debug incident"}); err != nil {
		t.Fatalf("grant request: %v", err)
	}

	joined := strings.Join(out, "\n")
	if got := strings.Count(joined, oneTimePassword); got != 1 {
		t.Fatalf("password printed %d times, want exactly 1:\n%s", got, joined)
	}
	if !strings.Contains(joined, "shown once") {
		t.Fatalf("output missing the shown-once warning:\n%s", joined)
	}
	// The credential must never touch disk: no seeded or minted file may contain it.
	for _, root := range []string{home, workspaceRoot} {
		assertNoFileContains(t, root, oneTimePassword)
	}
	// And the request must have carried the workspace bearer + body.
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %+v, want 1", fake.requests)
	}
	got := fake.requests[0]
	if got.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", got.Method)
	}
	if !strings.HasPrefix(got.Auth, "Bearer ") {
		t.Fatalf("authorization = %q, want a bearer", got.Auth)
	}
	if clicore.StringValue(got.Body["reason"]) != "debug incident" {
		t.Fatalf("reason = %v, want 'debug incident'", got.Body["reason"])
	}
	if clicore.StringValue(got.Body["level"]) != "read" {
		t.Fatalf("level = %v, want read", got.Body["level"])
	}
}

func TestGrantRequestRequiresReason(t *testing.T) {
	fake := &dbFakeServer{}
	ioctx, _, _ := newDBTestIO(t, fake)
	err := runDB(ioctx, []string{"grant", "request", "--database", testDatabase})
	if err == nil {
		t.Fatal("expected a usage error when --reason is missing")
	}
	if clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("exit code = %d, want %d", clicore.ExitCode(err), clicore.ExitUsage)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("requests = %+v, want none", fake.requests)
	}
}

func TestGrantListRendersTableNoSecrets(t *testing.T) {
	fake := &dbFakeServer{}
	ioctx, _, _ := newDBTestIO(t, fake)
	var out []string
	ioctx.Stdout = func(s string) { out = append(out, s) }

	if err := runDB(ioctx, []string{"grant", "list", "--database", testDatabase}); err != nil {
		t.Fatalf("grant list: %v", err)
	}
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "grant_ab12") || !strings.Contains(joined, "active") {
		t.Fatalf("list output missing grant row:\n%s", joined)
	}
	if strings.Contains(joined, oneTimePassword) {
		t.Fatalf("list output leaked a secret:\n%s", joined)
	}
	if len(fake.requests) != 1 || fake.requests[0].Method != http.MethodGet {
		t.Fatalf("requests = %+v, want 1 GET", fake.requests)
	}
}

func TestGrantRevokeDeletesByID(t *testing.T) {
	fake := &dbFakeServer{}
	ioctx, _, _ := newDBTestIO(t, fake)

	if err := runDB(ioctx, []string{"grant", "revoke", "grant_ab12", "--database", testDatabase}); err != nil {
		t.Fatalf("grant revoke: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %+v, want 1", fake.requests)
	}
	got := fake.requests[0]
	wantPath := "/v1/workspaces/" + testWorkspace + "/databases/" + testDatabase + "/grants/grant_ab12"
	if got.Method != http.MethodDelete || got.Path != wantPath {
		t.Fatalf("request = %s %s, want DELETE %s", got.Method, got.Path, wantPath)
	}
}

func TestGrantRevokeRequiresID(t *testing.T) {
	fake := &dbFakeServer{}
	ioctx, _, _ := newDBTestIO(t, fake)
	err := runDB(ioctx, []string{"grant", "revoke", "--database", testDatabase})
	if err == nil {
		t.Fatal("expected a usage error when the grant id is missing")
	}
	if clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("exit code = %d, want %d", clicore.ExitCode(err), clicore.ExitUsage)
	}
}

func TestDBListRendersInventoryNoSecrets(t *testing.T) {
	fake := &dbFakeServer{activeGrant: true}
	ioctx, _, _ := newDBTestIO(t, fake)
	var out []string
	ioctx.Stdout = func(s string) { out = append(out, s) }

	if err := runDB(ioctx, []string{"list"}); err != nil {
		t.Fatalf("db list: %v", err)
	}
	joined := strings.Join(out, "\n")
	for _, want := range []string{"DATABASE", "ENGINE", "SCHEMAS", "ACTIVE GRANT", testDatabase, "postgres", "active"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("list output missing %q:\n%s", want, joined)
		}
	}
	// The two schemas render as a count, not a leaked schema dump width issue.
	if !strings.Contains(joined, "2") {
		t.Fatalf("list output missing the schema count:\n%s", joined)
	}
	if strings.Contains(joined, oneTimePassword) {
		t.Fatalf("list output leaked a secret:\n%s", joined)
	}
	if len(fake.requests) != 1 || fake.requests[0].Method != http.MethodGet {
		t.Fatalf("requests = %+v, want 1 GET", fake.requests)
	}
	wantPath := "/v1/workspaces/" + testWorkspace + "/databases"
	if fake.requests[0].Path != wantPath {
		t.Fatalf("inventory path = %q, want %q", fake.requests[0].Path, wantPath)
	}
}

func TestDBListJSONEmitsInventory(t *testing.T) {
	fake := &dbFakeServer{}
	ioctx, _, _ := newDBTestIO(t, fake)
	var out []string
	ioctx.Stdout = func(s string) { out = append(out, s) }

	if err := runDB(ioctx, []string{"list", "--output", "json"}); err != nil {
		t.Fatalf("db list --json: %v", err)
	}
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Databases []map[string]any `json:"databases"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.Join(out, "\n")), &envelope); err != nil {
		t.Fatalf("json output not decodable: %v\n%s", err, strings.Join(out, "\n"))
	}
	if envelope.Status != "success" {
		t.Fatalf("json status = %q, want success", envelope.Status)
	}
	if len(envelope.Data.Databases) != 1 || clicore.StringValue(envelope.Data.Databases[0]["database"]) != testDatabase {
		t.Fatalf("json inventory = %+v, want one %s", envelope.Data.Databases, testDatabase)
	}
}

func TestDBListEmptyInventory(t *testing.T) {
	fake := &dbFakeServer{}
	ioctx, _, _ := newDBTestIO(t, fake)
	var out []string
	ioctx.Stdout = func(s string) { out = append(out, s) }
	// A workspace with no databases must not error; it prints an empty-state line.
	fake.overrideEmptyInventory = true
	if err := runDB(ioctx, []string{"list"}); err != nil {
		t.Fatalf("db list (empty): %v", err)
	}
	if !strings.Contains(strings.Join(out, "\n"), "No databases") {
		t.Fatalf("empty inventory output = %q, want an empty-state line", strings.Join(out, "\n"))
	}
}

func TestDBHelpListsCommands(t *testing.T) {
	var lines []string
	ioctx := clicore.IO{
		Env:    map[string]string{},
		Stdout: func(s string) { lines = append(lines, s) },
		Now:    time.Now,
		Client: http.DefaultClient,
	}
	if err := DB(map[string]any{}, nil, "", ioctx.Env, ioctx); err != nil {
		t.Fatalf("help: %v", err)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"db list", "db info", "db grant request", "db grant list", "db grant revoke", "db connect"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("help output missing %q:\n%s", want, joined)
		}
	}
}

// assertNoFileContains walks root and fails if any file contains needle.
func assertNoFileContains(t *testing.T, root, needle string) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // skip unreadable entries; the walk continues.
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if bytes.Contains(data, []byte(needle)) {
			t.Fatalf("secret written to disk at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

func writeTestAuth(t *testing.T, home string) {
	t.Helper()
	file := filepath.Join(home, clicore.AuthFileRelative)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeJSONFile(t, file, map[string]any{
		"access_token":  tokenResponse()["access_token"],
		"refresh_token": "refresh-token",
		"token_type":    "Bearer",
		"expires_at":    "2026-04-30T13:00:00.000Z",
		"issuer":        testBaseURL,
		"client_id":     "putnami-cli",
	})
}

func writeLinkFile(t *testing.T, workspaceRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, ".putnami"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeJSONFile(t, filepath.Join(workspaceRoot, clicore.LinkFileRelative), map[string]any{
		"version":           1,
		"control_plane_url": testBaseURL,
		"workspace_id":      testWorkspace,
		"environment":       "prod",
	})
}

func tokenResponse() map[string]any { return tokenResponseWith(nil) }

func tokenResponseWith(extra map[string]any) map[string]any {
	claims := map[string]any{"sub": "user-1", "email": "dev@example.com", "scope": "openid profile email"}
	for k, v := range extra {
		claims[k] = v
	}
	return map[string]any{
		"access_token":  jwt(claims),
		"refresh_token": "refresh-token",
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
