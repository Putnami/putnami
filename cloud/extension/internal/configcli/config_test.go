package configcli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

type configFakeServer struct {
	t                 *testing.T
	requests          []map[string]any
	configWrites      []map[string]any
	config            map[string]any
	layers            []map[string]any
	schema            map[string]any
	schemaQueryStatus int
	schemaPaths       []string
	warnings          []string
	secrets           []map[string]any
	secretStatus      []map[string]any
	secretStatusCode  int
	resolveWorkspaces []string
}

func (s *configFakeServer) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}

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
		return jsonResponse(http.StatusOK, tokenResponse(extra)), nil
	case strings.HasSuffix(req.URL.Path, "/configs/resolve") && req.Method == http.MethodPost:
		s.resolveWorkspaces = append(s.resolveWorkspaces, req.Header.Get(configWorkspaceHeader))
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)
		s.requests = append(s.requests, body)
		database := map[string]any{"host": "db.internal"}
		if body["secretsMode"] == "redacted" {
			database["password"] = "<redacted>"
		}
		config := map[string]any{
			"database": database,
			"session":  map[string]any{"ttl": "24h"},
		}
		if s.config != nil {
			config = s.config
		}
		layers := []map[string]any{
			{"dimension": "apps/auth-server/prod", "priority": 40},
		}
		if s.layers != nil {
			layers = s.layers
		}
		resp := map[string]any{
			"config":      config,
			"resolved":    len(config) > 0,
			"schemaMatch": true,
			"layers":      layers,
		}
		if s.warnings != nil {
			resp["warnings"] = s.warnings
		}
		return jsonResponse(http.StatusOK, resp), nil
	case strings.HasSuffix(req.URL.Path, "/configs") && req.Method == http.MethodPut:
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)
		s.configWrites = append(s.configWrites, body)
		return jsonResponse(http.StatusOK, body), nil
	case strings.HasSuffix(req.URL.Path, "/schemas") && req.Method == http.MethodGet:
		s.schemaPaths = append(s.schemaPaths, req.URL.EscapedPath())
		if s.schemaQueryStatus != 0 {
			return jsonResponse(s.schemaQueryStatus, map[string]any{"error": http.StatusText(s.schemaQueryStatus)}), nil
		}
		if got := req.URL.Query().Get("appName"); got != "apps/auth-server" {
			s.t.Fatalf("schema appName query = %q, want apps/auth-server", got)
		}
		schema := s.schema
		if schema == nil {
			schema = testConfigSchema()
		}
		return jsonResponse(http.StatusOK, schema), nil
	case strings.HasSuffix(req.URL.EscapedPath(), "/schemas/apps%2Fauth-server") && req.Method == http.MethodGet:
		s.schemaPaths = append(s.schemaPaths, req.URL.EscapedPath())
		schema := s.schema
		if schema == nil {
			schema = testConfigSchema()
		}
		return jsonResponse(http.StatusOK, schema), nil
	case strings.HasSuffix(req.URL.Path, "/secrets/status") && req.Method == http.MethodGet:
		if s.secretStatusCode != 0 {
			return jsonResponse(s.secretStatusCode, map[string]any{"error": http.StatusText(s.secretStatusCode)}), nil
		}
		keys := s.secretStatus
		if keys == nil {
			keys = []map[string]any{
				{"key": "database.password", "path": "database", "field": "password", "required": true, "status": "set", "updatedAt": "2026-05-19T09:00:00Z"},
				{"key": "session.cookieSecret", "path": "session", "field": "cookieSecret", "required": true, "status": "missing", "updatedAt": "2026-05-20T09:00:00Z"},
			}
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"appName": "apps/auth-server", "environment": "prod", "schemaHash": "sha256:test", "keys": keys,
		}), nil
	case strings.HasSuffix(req.URL.Path, "/secrets") && req.Method == http.MethodGet:
		entries := s.secrets
		if entries == nil {
			entries = []map[string]any{}
		}
		return jsonResponse(http.StatusOK, map[string]any{"entries": entries}), nil
	default:
		s.t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		return nil, nil
	}
}

func testConfigSchema() map[string]any {
	return map[string]any{
		"appName":    "apps/auth-server",
		"version":    "v1",
		"schemaHash": "sha256:test",
		"configs": []map[string]any{
			{
				"path": "database",
				"fields": []map[string]any{
					{"name": "host", "type": "string", "required": true},
					{"name": "user", "type": "string", "required": true},
					{"name": "password", "type": "string", "required": true, "sensitive": true},
				},
			},
			{
				"path": "session",
				"fields": []map[string]any{
					{"name": "ttl", "type": "duration", "default": "24h", "required": true},
					{"name": "cookieSecret", "type": "string", "required": true, "sensitive": true},
				},
			},
		},
	}
}

func TestConfigResolve_PrintsResolvedJSON(t *testing.T) {
	home := t.TempDir()
	workspaceRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval workspace root: %v", err)
	}
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{t: t}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"resolve", "apps/auth-server", "--env", "prod", "--json"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var out map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &out)
	if out["app"] != "apps/auth-server" || out["environment"] != "prod" {
		t.Fatalf("target = %v/%v, want apps/auth-server/prod", out["app"], out["environment"])
	}
	if !clicore.Truthy(out["resolved"]) {
		t.Fatalf("resolved = %v, want true", out["resolved"])
	}
	config, _ := out["config"].(map[string]any)
	if _, ok := config["database"]; !ok {
		t.Fatalf("config = %v, want database key", config)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fake.requests))
	}
	if _, ok := fake.requests[0]["includeSecrets"]; ok {
		t.Fatalf("includeSecrets sent by default: %v", fake.requests[0])
	}
}

func TestConfigRejectsAppFlag(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{t: t}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runConfig(ioctx, []string{"--app", "apps/auth-server", "--json"})
	if err == nil {
		t.Fatal("expected --app to be rejected")
	}
	if !strings.Contains(err.Error(), "positionally") {
		t.Fatalf("error = %q, want positional guidance", err.Error())
	}
}

func TestConfigPutWritesConfigFromExplicitFile(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "apps/auth-server", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "database", "fields": []map[string]any{
				{"name": "host", "type": "string"},
				{"name": "user", "type": "string"},
				{"name": "password", "type": "string", "sensitive": true},
			}},
		},
	})
	if err := os.WriteFile(filepath.Join(workspaceRoot, "manual.yaml"), []byte("database:\n  host: db.prod\n  user: runtime\n  password: should-not-write\n  extra: ignored\n"), 0o644); err != nil {
		t.Fatalf("write manual config: %v", err)
	}

	fake := &configFakeServer{t: t}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"put", "apps/auth-server", "--config-from", "manual.yaml"}); err != nil {
		t.Fatalf("config put: %v", err)
	}
	if len(fake.configWrites) != 1 {
		t.Fatalf("configWrites = %d, want 1", len(fake.configWrites))
	}
	write := fake.configWrites[0]
	if write["appName"] != "apps/auth-server" || write["path"] != "database" {
		t.Fatalf("write target = %v", write)
	}
	values := write["values"].(map[string]any)
	if values["host"] != "db.prod" || values["user"] != "runtime" {
		t.Fatalf("values = %v, want host/user", values)
	}
	if _, ok := values["password"]; ok {
		t.Fatalf("sensitive value was written as config: %v", values)
	}
	if _, ok := values["extra"]; ok {
		t.Fatalf("unknown value was written as config: %v", values)
	}
	if !strings.Contains(strings.Join(*stdout, "\n"), "Stored 1 config block") {
		t.Fatalf("stdout = %q, want stored summary", strings.Join(*stdout, "\n"))
	}
}

func TestConfigPutRejectsSchemaInvalidValueBeforeWrite(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "apps/auth-server", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "database", "fields": []map[string]any{
				{"name": "host", "type": "string"},
				{"name": "port", "type": "int"},
			}},
		},
	})
	if err := os.WriteFile(filepath.Join(workspaceRoot, "manual.yaml"), []byte("database:\n  host: db.prod\n  port: not-a-number\n"), 0o644); err != nil {
		t.Fatalf("write manual config: %v", err)
	}

	fake := &configFakeServer{t: t}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runConfig(ioctx, []string{"put", "apps/auth-server", "--config-from", "manual.yaml"})
	if err == nil {
		t.Fatal("config put accepted a schema-invalid value")
	}
	if !strings.Contains(err.Error(), "database") {
		t.Errorf("error missing block context: %v", err)
	}
	if len(fake.configWrites) > 0 {
		t.Fatalf("nothing should have been written; writes=%d", len(fake.configWrites))
	}
}

func TestConfigPutRejectsMissingConfigFrom(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{t: t}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runConfig(ioctx, []string{"put", "apps/auth-server"})
	if err == nil {
		t.Fatal("expected missing --config-from to fail")
	}
	if !strings.Contains(err.Error(), "--config-from") {
		t.Fatalf("error = %q, want --config-from guidance", err.Error())
	}
	if len(fake.configWrites) > 0 {
		t.Fatalf("unexpected config writes: %v", fake.configWrites)
	}
}

func TestConfigPutRejectsPlaceholder(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "apps/auth-server", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "database", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
	})
	if err := os.WriteFile(filepath.Join(workspaceRoot, "manual.yaml"), []byte("database:\n  host: "+publishConfigPlaceholder+"\n"), 0o644); err != nil {
		t.Fatalf("write manual config: %v", err)
	}

	fake := &configFakeServer{t: t}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runConfig(ioctx, []string{"put", "apps/auth-server", "--config-from", "manual.yaml"})
	if err == nil {
		t.Fatal("expected placeholder to fail")
	}
	if !strings.Contains(err.Error(), "database.host") {
		t.Fatalf("error = %q, want marker path", err.Error())
	}
	if len(fake.configWrites) > 0 {
		t.Fatalf("unexpected config writes: %v", fake.configWrites)
	}
}

func TestConfigInspect_PositionalAppShowsSchemaStatusesWhenValuesMissing(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{
		t:        t,
		config:   map[string]any{},
		warnings: []string{"database.password: required field missing", "database.host: required field missing"},
	}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"apps/auth-server"}); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "database.host") || !strings.Contains(joined, "missing") {
		t.Fatalf("stdout = %q, want missing schema key statuses", joined)
	}
	if strings.Contains(joined, "session.cookieSecret") || strings.Contains(joined, "database.password") {
		t.Fatalf("secret keys leaked in default config view: %q", joined)
	}
	if !strings.Contains(joined, "--secret-keys") {
		t.Fatalf("stdout = %q, want secret key hint", joined)
	}
}

func TestConfigInspect_SchemaFlagPrintsTableByDefault(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{t: t}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"apps/auth-server", "--schema"}); err != nil {
		t.Fatalf("schema: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "Config schema for apps/auth-server") ||
		!strings.Contains(joined, "database.host") ||
		!strings.Contains(joined, "database.password") ||
		!strings.Contains(joined, "secret") {
		t.Fatalf("stdout = %q, want schema table", joined)
	}
}

func TestConfigInspect_SchemaFlagSupportsJSONAndYAML(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{t: t}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot
	ioctx.JSON = func(any) { t.Fatal("--format json must print text, not emit a structured result") }

	if err := runConfig(ioctx, []string{"apps/auth-server", "--schema", "--format", "json"}); err != nil {
		t.Fatalf("schema json: %v", err)
	}
	var out map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &out)
	if out["appName"] != "apps/auth-server" || out["schemaHash"] != "sha256:test" {
		t.Fatalf("schema output = %v", out)
	}

	*stdout = nil
	if err := runConfig(ioctx, []string{"apps/auth-server", "--schema", "--format", "yaml"}); err != nil {
		t.Fatalf("schema yaml: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "appName: apps/auth-server") || !strings.Contains(joined, "schemaHash: sha256:test") {
		t.Fatalf("yaml = %q, want schema yaml", joined)
	}
}

func TestConfigInspect_RevealSecretsPrintsAfterApproval(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{
		t: t,
		config: map[string]any{
			"session": map[string]any{"cookieSecret": "plain-secret"},
		},
	}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot
	ioctx.Confirm = func(string) (string, bool) { return "y", true }
	ioctx.JSON = func(any) { t.Fatal("human reveal output must print text, not emit a structured result") }

	if err := runConfig(ioctx, []string{"apps/auth-server", "--reveal-secrets"}); err != nil {
		t.Fatalf("reveal secrets: %v", err)
	}
	if len(fake.requests) != 1 || fake.requests[0]["secretsMode"] != "reveal" {
		t.Fatalf("requests = %#v, want secretsMode reveal", fake.requests)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "plain-secret") {
		t.Fatalf("stdout = %q, want revealed config JSON", joined)
	}
	if len(*stdout) < 2 {
		t.Fatalf("stdout = %q, want line-oriented JSON text", joined)
	}
}

func TestConfigInspect_RevealSecretsJSONEmitsStructuredResult(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{
		t:      t,
		config: map[string]any{"session": map[string]any{"cookieSecret": "plain-secret"}},
	}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot
	ioctx.Confirm = func(string) (string, bool) { return "y", true }
	structured := false
	ioctx.JSON = func(any) { structured = true }

	if err := runConfig(ioctx, []string{"apps/auth-server", "--reveal-secrets", "--json"}); err != nil {
		t.Fatalf("reveal secrets json: %v", err)
	}
	if !structured {
		t.Fatal("expected structured JSON result")
	}
	if len(*stdout) != 0 {
		t.Fatalf("stdout = %q, want structured result only", strings.Join(*stdout, "\n"))
	}
}

func TestConfigInspect_SchemaFetchFallsBackToLegacyEscapedPath(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{t: t, schemaQueryStatus: http.StatusNotFound}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"apps/auth-server", "--schema", "--format", "json"}); err != nil {
		t.Fatalf("schema: %v", err)
	}
	var out map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &out)
	if out["appName"] != "apps/auth-server" {
		t.Fatalf("schema output = %v, want apps/auth-server", out)
	}
	if len(fake.schemaPaths) != 2 || fake.schemaPaths[1] != "/api/schemas/apps%2Fauth-server" {
		t.Fatalf("schema paths = %v, want query then escaped path fallback", fake.schemaPaths)
	}
}

func TestConfigInspect_KeyShowsDefaultOrSetValue(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{t: t, config: map[string]any{}}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"apps/auth-server", "--key", "session.ttl", "--json"}); err != nil {
		t.Fatalf("key: %v", err)
	}
	var out map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &out)
	if out["status"] != "default" || out["value"] != "24h" {
		t.Fatalf("key output = %v, want default 24h", out)
	}
}

func TestConfigInspect_KeySuggestsDeclaredSchemaKey(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{t: t, config: map[string]any{}}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runConfig(ioctx, []string{"apps/auth-server", "--key", "db.user"})
	if err == nil {
		t.Fatal("expected unknown key to fail")
	}
	if !strings.Contains(err.Error(), "Did you mean database.user?") {
		t.Fatalf("error = %q, want schema key suggestion", err.Error())
	}
}

func TestConfigInspect_SecretKeysListSchemaStatusAndSetupCommand(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{
		t: t,
	}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"apps/auth-server", "--secret-keys"}); err != nil {
		t.Fatalf("secret keys: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "database.password") || !strings.Contains(joined, "set") {
		t.Fatalf("stdout = %q, want set database secret", joined)
	}
	if !strings.Contains(joined, "session.cookieSecret") || !strings.Contains(joined, "missing") {
		t.Fatalf("stdout = %q, want missing session secret", joined)
	}
	if strings.Contains(joined, "session.cookieSecret                    missing  updated") {
		t.Fatalf("stdout = %q, missing secret must not render updatedAt", joined)
	}
	if !strings.Contains(joined, "putnami cloud secrets set apps/auth-server session.cookieSecret --env prod --from-stdin") {
		t.Fatalf("stdout = %q, want setup command", joined)
	}
}

func TestConfigInspect_SecretKeysFallbackToSchemaWhenStatusEndpointMissing(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{
		t:                t,
		secretStatusCode: http.StatusNotFound,
		secrets: []map[string]any{
			{"appName": "apps/auth-server", "environment": "prod", "path": "database", "updatedAt": "2026-05-19T09:00:00Z"},
		},
	}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"apps/auth-server", "--secret-keys"}); err != nil {
		t.Fatalf("secret keys: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "database.password") || !strings.Contains(joined, "set") {
		t.Fatalf("stdout = %q, want legacy set database secret", joined)
	}
	if !strings.Contains(joined, "session.cookieSecret") || !strings.Contains(joined, "missing") {
		t.Fatalf("stdout = %q, want legacy missing session secret", joined)
	}
}

func TestConfigInspect_RevealSecretsRequiresApproval(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{t: t}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot
	ioctx.Confirm = func(string) (string, bool) { return "", false }

	err := runConfig(ioctx, []string{"apps/auth-server", "--reveal-secrets"})
	if err == nil {
		t.Fatal("expected --reveal-secrets to require approval")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error = %q, want --yes hint", err.Error())
	}
}

func TestConfigResolve_PositionalAppAfterResolve(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{t: t}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"resolve", "apps/auth-server", "--include-secrets"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fake.requests))
	}
	if fake.requests[0]["appName"] != "apps/auth-server" {
		t.Fatalf("appName = %v, want apps/auth-server", fake.requests[0]["appName"])
	}
	if !clicore.Truthy(fake.requests[0]["includeSecrets"]) {
		t.Fatalf("includeSecrets = %v, want true", fake.requests[0]["includeSecrets"])
	}
}

func TestConfigList_PrintsFlattenedKeys(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{t: t}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"list", "apps/auth-server"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "database.host") || !strings.Contains(joined, "session.ttl") {
		t.Fatalf("stdout = %q", joined)
	}
	if fake.requests[0]["appName"] != "apps/auth-server" {
		t.Fatalf("appName = %v, want apps/auth-server", fake.requests[0]["appName"])
	}
}

func TestConfigShow_WithSecretsRequestsRedactedMode(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{t: t}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"show", "apps/auth-server", "--with-secrets"}); err != nil {
		t.Fatalf("show: %v", err)
	}
	if fake.requests[0]["secretsMode"] != "redacted" {
		t.Fatalf("secretsMode = %v, want redacted", fake.requests[0]["secretsMode"])
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "redacted") {
		t.Fatalf("stdout = %q, want redacted marker", joined)
	}
}

func TestConfigShow_SurfacesNeverPublishedDeclaredBlock(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	// Default fake config has database.host + session.ttl but omits
	// database.user, which the schema declares (non-secret) — a never-published
	// block that show must loudly surface.
	fake := &configFakeServer{t: t}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"show", "apps/auth-server"}); err != nil {
		t.Fatalf("show: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "Declared in schema but NEVER published") {
		t.Fatalf("stdout = %q, want loud never-published header", joined)
	}
	if !strings.Contains(joined, "database.user") {
		t.Fatalf("stdout = %q, want database.user listed as never published", joined)
	}
	// Secret keys must not leak into the never-published section.
	if strings.Contains(joined, "database.password") || strings.Contains(joined, "session.cookieSecret") {
		t.Fatalf("stdout = %q, secret keys leaked into never-published section", joined)
	}
}

func TestConfigResolve_MissingBlocksInStructuredOutput(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{t: t}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"resolve", "apps/auth-server", "--json"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var out map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &out)
	missing, _ := out["missingBlocks"].([]any)
	found := false
	for _, m := range missing {
		if m == "database.user" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missingBlocks = %v, want database.user", out["missingBlocks"])
	}
}

func TestConfigShow_RequireCompleteFailsWhenBlockMissing(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{t: t}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runConfig(ioctx, []string{"show", "apps/auth-server", "--require-complete"})
	if err == nil {
		t.Fatal("expected --require-complete to fail on a never-published block")
	}
	if clicore.ExitCode(err) != clicore.ExitAPI {
		t.Fatalf("exit code = %d, want %d", clicore.ExitCode(err), clicore.ExitAPI)
	}
	if !strings.Contains(err.Error(), "database.user") {
		t.Fatalf("error = %q, want the missing block named", err.Error())
	}
	// The resolved values + loud section still print; only the exit code flips.
	if !strings.Contains(strings.Join(*stdout, "\n"), "database.user") {
		t.Fatalf("stdout = %q, want surfaced section", strings.Join(*stdout, "\n"))
	}
}

func TestConfigResolve_RequireCompletePassesWhenAllPublished(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := &configFakeServer{
		t: t,
		config: map[string]any{
			"database": map[string]any{"host": "db.internal", "user": "runtime"},
			"session":  map[string]any{"ttl": "24h"},
		},
	}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"resolve", "apps/auth-server", "--require-complete"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if strings.Contains(joined, "NEVER published") {
		t.Fatalf("stdout = %q, want no never-published section when all blocks published", joined)
	}
}

// driftSchemaBlocksFixture is the local schema drift tests compare against: two
// non-secret fields plus one sensitive field (proving the projection strips it)
// and a defaulted field (proving a default never reads as drift).
func driftSchemaBlocksFixture() []map[string]any {
	return []map[string]any{
		{"path": "database", "fields": []map[string]any{
			{"name": "host", "type": "string", "required": true},
			{"name": "user", "type": "string", "required": true},
			{"name": "password", "type": "string", "required": true, "sensitive": true},
		}},
		{"path": "session", "fields": []map[string]any{
			{"name": "ttl", "type": "duration", "default": "24h"},
		}},
	}
}

func driftEntriesFromStdout(t *testing.T, stdout []string) (bool, []map[string]any) {
	t.Helper()
	var out map[string]any
	decodeJSON(t, strings.Join(stdout, "\n"), &out)
	drift := clicore.Truthy(out["drift"])
	rawEntries, _ := out["entries"].([]any)
	entries := make([]map[string]any, 0, len(rawEntries))
	for _, raw := range rawEntries {
		if entry, ok := raw.(map[string]any); ok {
			entries = append(entries, entry)
		}
	}
	return drift, entries
}

func driftHasEntry(entries []map[string]any, key, kind string) bool {
	for _, entry := range entries {
		if entry["key"] == key && entry["kind"] == kind {
			return true
		}
	}
	return false
}

func TestConfigDrift_NoDriftExitsZeroAndStripsSecrets(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "apps/auth-server", publishAppLayout{
		schemaBlocks:   driftSchemaBlocksFixture(),
		envProdVisible: "database:\n  host: db.internal\n  user: runtime\nsession:\n  ttl: 24h\n",
	})
	// Published plane matches the committed non-secret values; it also carries a
	// secret (password), which the schema projection strips on both sides so it
	// must NOT read as drift.
	fake := &configFakeServer{
		t: t,
		config: map[string]any{
			"database": map[string]any{"host": "db.internal", "user": "runtime", "password": "super-secret"},
			"session":  map[string]any{"ttl": "24h"},
		},
	}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"drift", "apps/auth-server"}); err != nil {
		t.Fatalf("drift (no drift expected): %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "No config drift") {
		t.Fatalf("stdout = %q, want clean summary", joined)
	}
	if len(fake.resolveWorkspaces) != 1 || fake.resolveWorkspaces[0] != "ws-acme" {
		t.Fatalf("resolve workspace headers = %v, want exact linked workspace", fake.resolveWorkspaces)
	}
}

func TestConfigDrift_CommittedNotPublishedFails(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "apps/auth-server", publishAppLayout{
		schemaBlocks:   driftSchemaBlocksFixture(),
		envProdVisible: "database:\n  host: db.internal\n  user: runtime\nsession:\n  ttl: 24h\n",
	})
	// The plane is missing database.user — a committed value that was never
	// published.
	fake := &configFakeServer{
		t: t,
		config: map[string]any{
			"database": map[string]any{"host": "db.internal"},
			"session":  map[string]any{"ttl": "24h"},
		},
	}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runConfig(ioctx, []string{"drift", "apps/auth-server", "--json"})
	if err == nil {
		t.Fatal("expected drift failure for a committed-but-unpublished value")
	}
	if clicore.ExitCode(err) != clicore.ExitAPI {
		t.Fatalf("exit = %d, want ExitAPI", clicore.ExitCode(err))
	}
	drift, entries := driftEntriesFromStdout(t, *stdout)
	if !drift {
		t.Fatalf("structured drift flag = false, want true; entries=%v", entries)
	}
	if !driftHasEntry(entries, "database.user", "committed-not-published") {
		t.Fatalf("entries = %v, want database.user committed-not-published", entries)
	}
}

func TestConfigDrift_PublishedNotCommittedFails(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	// Committed yaml omits database.user; the plane has it — a plane value with no
	// committed source.
	writePublishApp(t, workspaceRoot, "apps/auth-server", publishAppLayout{
		schemaBlocks:   driftSchemaBlocksFixture(),
		envProdVisible: "database:\n  host: db.internal\nsession:\n  ttl: 24h\n",
	})
	fake := &configFakeServer{
		t: t,
		config: map[string]any{
			"database": map[string]any{"host": "db.internal", "user": "runtime"},
			"session":  map[string]any{"ttl": "24h"},
		},
	}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runConfig(ioctx, []string{"drift", "apps/auth-server", "--json"})
	if err == nil {
		t.Fatal("expected drift failure for a published-but-uncommitted value")
	}
	if clicore.ExitCode(err) != clicore.ExitAPI {
		t.Fatalf("exit = %d, want ExitAPI", clicore.ExitCode(err))
	}
	drift, entries := driftEntriesFromStdout(t, *stdout)
	if !drift || !driftHasEntry(entries, "database.user", "published-not-committed") {
		t.Fatalf("entries = %v, want database.user published-not-committed", entries)
	}
}

func TestConfigDrift_ValueMismatchFailsWithBothValues(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "apps/auth-server", publishAppLayout{
		schemaBlocks:   driftSchemaBlocksFixture(),
		envProdVisible: "database:\n  host: db.local\n  user: runtime\nsession:\n  ttl: 24h\n",
	})
	fake := &configFakeServer{
		t: t,
		config: map[string]any{
			"database": map[string]any{"host": "db.internal", "user": "runtime"},
			"session":  map[string]any{"ttl": "24h"},
		},
	}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runConfig(ioctx, []string{"drift", "apps/auth-server", "--json"})
	if err == nil {
		t.Fatal("expected drift failure for a value mismatch")
	}
	drift, entries := driftEntriesFromStdout(t, *stdout)
	if !drift {
		t.Fatalf("structured drift flag = false, want true")
	}
	var mismatch map[string]any
	for _, entry := range entries {
		if entry["key"] == "database.host" && entry["kind"] == "value-mismatch" {
			mismatch = entry
		}
	}
	if mismatch == nil {
		t.Fatalf("entries = %v, want database.host value-mismatch", entries)
	}
	if mismatch["committed"] != "db.local" || mismatch["published"] != "db.internal" {
		t.Fatalf("mismatch = %v, want committed db.local / published db.internal", mismatch)
	}
}

// TestConfigDrift_ManagedLayerExitsZeroAndAuditsSuppressions runs the WHOLE
// command path, including the resolve-layer plumbing: the plane reports the
// managed/events layer, so the push binding keys it provisioned must exit 0
// AND be named in the report, while an operator key still drifts.
func TestConfigDrift_ManagedLayerExitsZeroAndAuditsSuppressions(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "apps/auth-server", publishAppLayout{
		schemaBlocks: []map[string]any{{"path": "events", "fields": []map[string]any{
			{"name": "delivery", "type": "string"},
			{"name": "transport", "type": "string"},
			{"name": "pubsub", "type": "object", "fields": []map[string]any{
				{"name": "projectId", "type": "string"},
				{"name": "topicTemplate", "type": "string"},
			}},
			{"name": "push", "type": "object", "fields": []map[string]any{
				{"name": "issuer", "type": "string"},
				{"name": "audience", "type": "string"},
			}},
			// signalMaxAge is an operator key OUTSIDE the managed binding set.
			{"name": "signalMaxAge", "type": "duration"},
		}}},
		envProdVisible: "events:\n  delivery: push\n  transport: pubsub\n  pubsub:\n    projectId: putnami\n    topicTemplate: \"events-{topic}\"\n  push: {}\n  signalMaxAge: 15m\n",
	})
	fake := &configFakeServer{
		t: t,
		config: map[string]any{"events": map[string]any{
			"delivery":  "push",
			"transport": "pubsub",
			"pubsub":    map[string]any{"projectId": "putnami", "topicTemplate": "events-{topic}"},
			"push": map[string]any{
				"issuer":   "https://accounts.google.com",
				"audience": "https://api.putnami.cloud",
			},
			"signalMaxAge": "15m",
		}},
		layers: []map[string]any{
			{"dimension": "apps/auth-server/prod", "priority": 40},
			{"dimension": "managed/events", "priority": 60},
		},
	}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"drift", "apps/auth-server"}); err != nil {
		t.Fatalf("managed-layer drift must exit 0: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "No config drift") ||
		!strings.Contains(joined, "2 plane-owned key(s) suppressed") {
		t.Fatalf("stdout = %q, want the clean line with the suppressed count", joined)
	}
	for _, key := range []string{"events.push.issuer", "events.push.audience"} {
		if !strings.Contains(joined, key+" [published-not-committed] owned by layer managed/events") {
			t.Fatalf("stdout = %q, want the suppressed key %s named with its layer", joined, key)
		}
	}

	// Same managed layer, but the plane now diverges on an operator key
	// OUTSIDE the owned binding set: still a hard failure, and the structured
	// output still carries the suppressed audit.
	fake.config["events"].(map[string]any)["signalMaxAge"] = "30m"
	ioctx2, stdout2, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx2.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot
	err := runConfig(ioctx2, []string{"drift", "apps/auth-server", "--json"})
	if err == nil {
		t.Fatal("a divergence outside the managed layer's owned keys must still fail")
	}
	if clicore.ExitCode(err) != clicore.ExitAPI {
		t.Fatalf("exit = %d, want ExitAPI", clicore.ExitCode(err))
	}
	drift, entries := driftEntriesFromStdout(t, *stdout2)
	if !drift || len(entries) != 1 || !driftHasEntry(entries, "events.signalMaxAge", "value-mismatch") {
		t.Fatalf("entries = %v, want only events.signalMaxAge value-mismatch", entries)
	}
	var out map[string]any
	decodeJSON(t, strings.Join(*stdout2, "\n"), &out)
	suppressed, _ := out["suppressed"].([]any)
	if len(suppressed) != 2 {
		t.Fatalf("structured suppressed = %v, want the 2 plane-owned keys", out["suppressed"])
	}
}

func TestConfigDrift_AuthenticatesWithMachineTokenEnv(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	// Deliberately NO writeTestAuth: there is no ~/.putnami/auth.json. Only the
	// injected PUTNAMI_CLOUD_TOKEN machine token can authenticate the drift call,
	// so a passing run proves the non-interactive CI path.
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "apps/auth-server", publishAppLayout{
		schemaBlocks:   driftSchemaBlocksFixture(),
		envProdVisible: "database:\n  host: db.internal\n  user: runtime\nsession:\n  ttl: 24h\n",
	})
	fake := &configFakeServer{
		t: t,
		config: map[string]any{
			"database": map[string]any{"host": "db.internal", "user": "runtime"},
			"session":  map[string]any{"ttl": "24h"},
		},
	}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot
	ioctx.Env["PUTNAMI_CLOUD_TOKEN"] = "pkt_ci_machine_token"

	if err := runConfig(ioctx, []string{"drift", "apps/auth-server"}); err != nil {
		t.Fatalf("drift must authenticate via PUTNAMI_CLOUD_TOKEN without a session: %v", err)
	}
}

func TestConfigResolve_UsesCwdWorkloadWhenNoAppFlag(t *testing.T) {
	home := t.TempDir()
	workspaceRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval workspace root: %v", err)
	}
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	workload := filepath.Join(workspaceRoot, "auth", "workloads", "server")
	mustMkdir(t, workload)
	writeJSONFile(t, filepath.Join(workload, "putnami.json"), map[string]any{"name": "apps/auth-server"})

	fake := &configFakeServer{t: t}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(oldwd) }()
	if err := os.Chdir(workload); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	if err := runConfig(ioctx, []string{"resolve", "--json"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var out map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &out)
	if out["app"] != "apps/auth-server" {
		t.Fatalf("app = %v, want apps/auth-server", out["app"])
	}
}
