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

// publishFakeServer mirrors the in-memory secrets fake but for the
// publish flow: it tracks the registered schema and the ordered list of
// config writes so tests can assert ordering (schemas first, then
// configs) and idempotency.
type publishFakeServer struct {
	t                *testing.T
	registeredSchema map[string]any
	configWrites     []map[string]any
	// configWriteQueries is the raw query of each PUT /api/configs, positionally
	// aligned with configWrites. The server contract is that the CLI defers every
	// block's roll but the last (`roll=defer`), so a publish rolls exactly ONE
	// revision — that is only observable here.
	configWriteQueries  []string
	tokenRequests       int
	tokenStatus         int
	tokenErrorBody      map[string]any
	schemaPostStatus    int
	schemaPostErrorBody map[string]any
	configPutStatus     int
	configPutErrorBody  map[string]any
	// configPutRawBody replaces the JSON envelope with an arbitrary body, so a
	// test can model an infrastructure response the CLI cannot decode (a gateway
	// HTML page, a torn connection's 503).
	configPutRawBody string
}

func (s *publishFakeServer) RoundTrip(req *http.Request) (*http.Response, error) {
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
		s.tokenRequests++
		if s.tokenStatus >= 400 || s.tokenErrorBody != nil {
			status := s.tokenStatus
			if status == 0 {
				status = http.StatusBadRequest
			}
			body := s.tokenErrorBody
			if body == nil {
				body = map[string]any{"error": "invalid_grant", "error_description": "Invalid or expired refresh token"}
			}
			return jsonResponse(status, body), nil
		}
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)
		extra := map[string]any{}
		if workspaceID := clicore.StringValue(body["workspace_id"]); workspaceID != "" {
			extra["scope_ref"] = map[string]any{"workspace_id": workspaceID}
		}
		return jsonResponse(http.StatusOK, tokenResponse(extra)), nil
	case req.Method == http.MethodPost && req.URL.Path == "/api/schemas":
		status := s.schemaPostStatus
		if status == 0 {
			status = http.StatusOK
		}
		if status >= 400 {
			body := s.schemaPostErrorBody
			if body == nil {
				body = map[string]any{"error": "schema rejected"}
			}
			return jsonResponse(status, body), nil
		}
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)
		s.registeredSchema = body
		return jsonResponse(status, body), nil
	case req.Method == http.MethodPut && req.URL.Path == "/api/configs":
		status := s.configPutStatus
		if status == 0 {
			status = http.StatusOK
		}
		if s.configPutRawBody != "" {
			return &http.Response{
				StatusCode: status,
				Status:     clicore.StatusLine(status),
				Header:     http.Header{"Content-Type": []string{"text/html"}},
				Body:       io.NopCloser(strings.NewReader(s.configPutRawBody)),
			}, nil
		}
		if status >= 400 {
			body := s.configPutErrorBody
			if body == nil {
				body = map[string]any{"error": "config rejected"}
			}
			return jsonResponse(status, body), nil
		}
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)
		s.configWrites = append(s.configWrites, body)
		s.configWriteQueries = append(s.configWriteQueries, req.URL.RawQuery)
		return jsonResponse(status, body), nil
	default:
		s.t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		return nil, nil
	}
}

func newPublishFake(t *testing.T) *publishFakeServer {
	return &publishFakeServer{t: t}
}

// publishAppLayout builds an app directory shaped like the upstream
// putnami-go layout: putnami.json with the app name, schema/config.json
// with a manifest, and conf/env*.yaml or legacy conf/.env*.yaml for values.
type publishAppLayout struct {
	dir            string
	envYAML        string
	envProdYAML    string
	envVisibleYAML string
	envProdVisible string
	schemaBlocks   []map[string]any
}

func writePublishApp(t *testing.T, workspaceRoot, appName string, layout publishAppLayout) {
	t.Helper()
	appDir := workspaceRoot
	if layout.dir != "" {
		appDir = filepath.Join(workspaceRoot, layout.dir)
	}
	mustMkdir(t, appDir)
	writeJSONFile(t, filepath.Join(appDir, "putnami.json"), map[string]any{"name": appName})
	if len(layout.schemaBlocks) > 0 {
		schemaDir := filepath.Join(appDir, "schema")
		mustMkdir(t, schemaDir)
		writeJSONFile(t, filepath.Join(schemaDir, "config.json"), map[string]any{
			"appName":    appName,
			"version":    "1.0.0",
			"schemaHash": "sha256:test",
			"configs":    layout.schemaBlocks,
		})
	}
	if layout.envYAML != "" || layout.envProdYAML != "" || layout.envVisibleYAML != "" || layout.envProdVisible != "" {
		confDir := filepath.Join(appDir, "conf")
		mustMkdir(t, confDir)
		if layout.envVisibleYAML != "" {
			if err := os.WriteFile(filepath.Join(confDir, "env.yaml"), []byte(layout.envVisibleYAML), 0o644); err != nil {
				t.Fatalf("write env.yaml: %v", err)
			}
		}
		if layout.envProdVisible != "" {
			if err := os.WriteFile(filepath.Join(confDir, "env.prod.yaml"), []byte(layout.envProdVisible), 0o644); err != nil {
				t.Fatalf("write env.prod.yaml: %v", err)
			}
		}
		if layout.envYAML != "" {
			if err := os.WriteFile(filepath.Join(confDir, ".env.yaml"), []byte(layout.envYAML), 0o644); err != nil {
				t.Fatalf("write .env.yaml: %v", err)
			}
		}
		if layout.envProdYAML != "" {
			if err := os.WriteFile(filepath.Join(confDir, ".env.prod.yaml"), []byte(layout.envProdYAML), 0o644); err != nil {
				t.Fatalf("write .env.prod.yaml: %v", err)
			}
		}
	}
}

func TestNewPublishCtxHonorsControlPlaneURLOverride(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{
				{"name": "host", "type": "string"},
			}},
		},
	})

	ioctx, _, _ := commonIO(t, home, http.DefaultClient)
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot
	ctx, err := newPublishCtx(map[string]any{
		"dry-run":           true,
		"control-plane-url": "https://override.example.test/",
	}, workspaceRoot, ioctx.Env, ioctx)
	if err != nil {
		t.Fatalf("newPublishCtx: %v", err)
	}
	if ctx.controlPlane != "https://override.example.test" {
		t.Fatalf("controlPlane = %q, want override URL", ctx.controlPlane)
	}
}

func TestPublishConfig_RegistersSchemaThenWritesBlocks(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{
				{"name": "host", "type": "string"}, {"name": "port", "type": "int"},
			}},
			{"path": "database", "fields": []map[string]any{
				{"name": "dsn", "type": "string"},
			}},
		},
		envYAML:     "server:\n  host: localhost\n  port: 8080\n",
		envProdYAML: "server:\n  port: 9090\ndatabase:\n  dsn: postgres://prod/db\n",
	})

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runPublishConfig(ioctx, []string{}); err != nil {
		t.Fatalf("publish-config: %v", err)
	}
	if fake.registeredSchema == nil {
		t.Fatal("schema was not registered")
	}
	if fake.registeredSchema["appName"] != "my-app" {
		t.Errorf("schema appName = %v", fake.registeredSchema["appName"])
	}
	if len(fake.configWrites) != 2 {
		t.Fatalf("configWrites = %d, want 2", len(fake.configWrites))
	}
	// Configs must be sorted by path so the test can assert deterministic
	// order; planBlockWrites guarantees this.
	if fake.configWrites[0]["path"] != "database" || fake.configWrites[1]["path"] != "server" {
		t.Fatalf("write order = %s, %s", fake.configWrites[0]["path"], fake.configWrites[1]["path"])
	}
	serverValues := fake.configWrites[1]["values"].(map[string]any)
	// env.prod.yaml overrides env.yaml on port; host is inherited from defaults.
	if serverValues["host"] != "localhost" {
		t.Errorf("server.host = %v", serverValues["host"])
	}
	if v, ok := serverValues["port"].(float64); !ok || v != 9090 {
		t.Errorf("server.port = %v", serverValues["port"])
	}
}

func TestPublishConfig_DefersRollOnEveryBlockButTheLast(t *testing.T) {
	// The documented single-roll contract (PUT /api/configs `roll=defer`, served
	// by config-api): the CLI defers all but the last write so
	// ONE command rolls ONE revision carrying the complete config. publish-config
	// sent an empty query on every block, so an app with N blocks paid N
	// synchronous re-stamp + readiness-poll cycles inside N requests.
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "alpha", "fields": []map[string]any{{"name": "a", "type": "string"}}},
			{"path": "beta", "fields": []map[string]any{{"name": "b", "type": "string"}}},
			{"path": "gamma", "fields": []map[string]any{{"name": "c", "type": "string"}}},
		},
		envProdYAML: "alpha:\n  a: one\nbeta:\n  b: two\ngamma:\n  c: three\n",
	})

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runPublishConfig(ioctx, []string{}); err != nil {
		t.Fatalf("publish-config: %v", err)
	}
	if len(fake.configWriteQueries) != 3 {
		t.Fatalf("config writes = %d, want 3", len(fake.configWriteQueries))
	}
	for i, query := range fake.configWriteQueries[:len(fake.configWriteQueries)-1] {
		if query != "roll=defer" {
			t.Errorf("block %d (%v) query = %q, want roll=defer", i, fake.configWrites[i]["path"], query)
		}
	}
	if last := fake.configWriteQueries[len(fake.configWriteQueries)-1]; last != "" {
		t.Errorf("the LAST block must roll (empty query), got %q", last)
	}
}

func TestPublishConfig_SingleBlockRollsImmediately(t *testing.T) {
	// The single-block app (otel-server's shape): there is nothing to defer, so
	// the one write must roll. Deferring it would store config that never reaches
	// the running revision.
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "alpha", "fields": []map[string]any{{"name": "a", "type": "string"}}},
		},
		envProdYAML: "alpha:\n  a: one\n",
	})

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runPublishConfig(ioctx, []string{}); err != nil {
		t.Fatalf("publish-config: %v", err)
	}
	if len(fake.configWriteQueries) != 1 || fake.configWriteQueries[0] != "" {
		t.Fatalf("a single block must roll, got queries %#v", fake.configWriteQueries)
	}
}

func TestPublishConfig_NonJSONResponseReportsStatusAndBody(t *testing.T) {
	// The operator-facing symptom: `error invalid JSON response from
	// https://api.putnami.cloud/api/configs` and nothing else. The status code and
	// the body are what identify a server-side timeout; without them the failure
	// reads as a client parse bug.
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "alpha", "fields": []map[string]any{{"name": "a", "type": "string"}}},
		},
		envProdYAML: "alpha:\n  a: one\n",
	})

	fake := newPublishFake(t)
	fake.configPutStatus = http.StatusServiceUnavailable
	// A real Cloud Run 503: not JSON, and carrying a terminal escape to prove the
	// snippet is sanitized before it reaches an operator's log.
	fake.configPutRawBody = "upstream request timeout\x1b[0m\x00"
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runPublishConfig(ioctx, []string{})
	if err == nil {
		t.Fatal("a non-JSON response must fail the publish")
	}
	msg := err.Error()
	if !strings.Contains(msg, "503") {
		t.Errorf("error must name the HTTP status, got %q", msg)
	}
	if !strings.Contains(msg, "upstream request timeout") {
		t.Errorf("error must carry a body snippet, got %q", msg)
	}
	if strings.ContainsAny(msg, "\x1b\x00") {
		t.Errorf("body snippet must be sanitized of control characters, got %q", msg)
	}
}

func TestPublishConfig_RegistersSchemaUnderProjectPathNotArtifactAppName(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	const projectPath = "apps/auth-server"
	writePublishApp(t, workspaceRoot, projectPath, publishAppLayout{
		dir: projectPath,
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
		envYAML: "server:\n  host: localhost\n",
	})
	// The extractor stamped a divergent appName into the artifact (for the TS
	// extractor, the package.json name). publish must ignore it and register
	// under the resolved project path, or the schema lands on one CPA row while
	// config values validate against another and 412 forever.
	schemaFile := filepath.Join(workspaceRoot, projectPath, "schema", "config.json")
	writeJSONFile(t, schemaFile, map[string]any{
		"appName":    "auth.putnami.cloud",
		"version":    "1.0.0",
		"schemaHash": "sha256:test",
		"configs": []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
	})

	fake := newPublishFake(t)
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runPublishConfig(ioctx, []string{projectPath, "--json"}); err != nil {
		t.Fatalf("publish-config: %v", err)
	}
	if fake.registeredSchema == nil {
		t.Fatal("schema was not registered")
	}
	if got := clicore.StringValue(fake.registeredSchema["appName"]); got != projectPath {
		t.Fatalf("schema registered under appName %q, want project path %q", got, projectPath)
	}
	if len(fake.configWrites) != 1 {
		t.Fatalf("configWrites = %d, want 1", len(fake.configWrites))
	}
	if got := clicore.StringValue(fake.configWrites[0]["appName"]); got != projectPath {
		t.Fatalf("config write appName = %q, want %q (schema and values must share one key)", got, projectPath)
	}
	// The divergence is surfaced as a warning, not swallowed.
	var out map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &out)
	warnings, _ := out["warnings"].([]any)
	joined := ""
	for _, w := range warnings {
		joined += clicore.StringValue(w) + " "
	}
	for _, fragment := range []string{"auth.putnami.cloud", projectPath} {
		if !strings.Contains(joined, fragment) {
			t.Errorf("appName-mismatch warning missing %q: %q", fragment, joined)
		}
	}
}

func TestPublishConfig_NormalizesLegacyGeneratedTypes(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "apps/npm-server", publishAppLayout{
		dir: "apps/npm-server",
		schemaBlocks: []map[string]any{
			{"path": "npm", "fields": []map[string]any{
				{"name": "server", "type": "ServerConfig"},
				{"name": "logLevel", "type": "Level", "default": "info"},
				{"name": "bootstrap", "type": "array", "items": map[string]any{"type": "string"}},
			}},
		},
		envYAML: "npm:\n  server:\n    port: 8081\n  logLevel: debug\n  bootstrap:\n    - default\n",
	})

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runPublishConfig(ioctx, []string{"apps/npm-server"}); err != nil {
		t.Fatalf("publish-config: %v", err)
	}
	configs := fake.registeredSchema["configs"].([]any)
	fields := configs[0].(map[string]any)["fields"].([]any)
	got := map[string]string{}
	for _, raw := range fields {
		field := raw.(map[string]any)
		got[clicore.StringValue(field["name"])] = clicore.StringValue(field["type"])
	}
	if got["server"] != "object" {
		t.Fatalf("server type = %q, want object; schema=%v", got["server"], fake.registeredSchema)
	}
	if got["logLevel"] != "string" {
		t.Fatalf("logLevel type = %q, want string; schema=%v", got["logLevel"], fake.registeredSchema)
	}
	if got["bootstrap"] != "array" {
		t.Fatalf("bootstrap type = %q, want array; schema=%v", got["bootstrap"], fake.registeredSchema)
	}
}

func TestPublishConfig_SchemaMissingPointsAtConfigExtract(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	// putnami.json at root names the app, but no schema artifact exists.
	writeProjectFile(t, workspaceRoot, "my-app")

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runPublishConfig(ioctx, nil)
	if err == nil {
		t.Fatal("expected error when schema artifact is missing")
	}
	for _, fragment := range []string{"my-app", "config-extract"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q missing fragment %q", err.Error(), fragment)
		}
	}
	if fake.registeredSchema != nil || len(fake.configWrites) > 0 {
		t.Fatalf("nothing should have been sent; got schema=%v writes=%d", fake.registeredSchema, len(fake.configWrites))
	}
}

func TestPublishConfig_SchemaMissingSoftSkipsWithIfPresent(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	// The project exposes no config schema. The publish verb runs the config
	// and migration steps for every activated project and passes --if-present,
	// so a project with (say) only a migration bundle must no-op here rather
	// than fail the whole publish.
	writeProjectFile(t, workspaceRoot, "my-app")

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runPublishConfig(ioctx, []string{"--if-present"}); err != nil {
		t.Fatalf("--if-present must soft-skip a missing schema, got error: %v", err)
	}
	if fake.registeredSchema != nil || len(fake.configWrites) > 0 {
		t.Fatalf("a skip must send nothing; got schema=%v writes=%d", fake.registeredSchema, len(fake.configWrites))
	}
}

func TestPublishConfig_IfPresentSkipsMissingSchemaBeforeCloudLink(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeProjectFile(t, workspaceRoot, "my-app")

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runPublishConfig(ioctx, []string{"--if-present"}); err != nil {
		t.Fatalf("--if-present must skip a missing schema before requiring Cloud setup, got error: %v", err)
	}
	if fake.registeredSchema != nil || len(fake.configWrites) > 0 {
		t.Fatalf("a skip must send nothing; got schema=%v writes=%d", fake.registeredSchema, len(fake.configWrites))
	}
}

func TestPublishConfig_SchemaErrorSurfacesDetails(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
		envYAML: "server:\n  host: localhost\n",
	})

	fake := newPublishFake(t)
	fake.schemaPostStatus = http.StatusBadRequest
	fake.schemaPostErrorBody = map[string]any{
		"error":   "invalid schema manifest",
		"details": []any{`server.port: unknown field type "PortConfig"`},
	}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runPublishConfig(ioctx, nil)
	if err == nil {
		t.Fatal("expected schema error")
	}
	for _, fragment := range []string{"register schema", "invalid schema manifest", "server.port", "PortConfig"} {
		assertContains(t, err.Error(), fragment)
	}
}

func TestPublishConfig_InvalidRefreshTokenSuggestsCloudLogin(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
		envYAML: "server:\n  host: localhost\n",
	})

	fake := newPublishFake(t)
	fake.tokenStatus = http.StatusBadRequest
	fake.tokenErrorBody = map[string]any{
		"error":             "invalid_grant",
		"error_description": "Invalid or expired refresh token",
	}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runPublishConfig(ioctx, nil)
	if err == nil {
		t.Fatal("expected invalid refresh-token error")
	}
	for _, fragment := range []string{"Invalid or expired refresh token", "putnami cloud login", "refresh cloud credentials"} {
		assertContains(t, err.Error(), fragment)
	}
	if fake.registeredSchema != nil || len(fake.configWrites) > 0 {
		t.Fatalf("nothing should have been published; got schema=%v writes=%d", fake.registeredSchema, len(fake.configWrites))
	}
}

func TestPublishConfig_DryRunMakesNoRequests(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
		envYAML: "server:\n  host: localhost\n",
	})

	fake := newPublishFake(t)
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runPublishConfig(ioctx, []string{"--dry-run", "--json"}); err != nil {
		t.Fatalf("publish-config --dry-run: %v", err)
	}
	if fake.tokenRequests != 0 || fake.registeredSchema != nil || len(fake.configWrites) > 0 {
		t.Fatalf("--dry-run made network calls: token=%d schema=%v writes=%d", fake.tokenRequests, fake.registeredSchema, len(fake.configWrites))
	}
	var plan map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &plan)
	if plan["status"] != "dry-run" {
		t.Errorf("status = %v", plan["status"])
	}
	configs, ok := plan["configs"].([]any)
	if !ok || len(configs) != 1 {
		t.Fatalf("configs in plan = %v", plan["configs"])
	}
}

func TestPublishConfig_StrictDryRunFailsOnUnpublishedBlock(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
			// database declares only a NON-required, non-default field. A plain
			// publish/dry-run silently skips it when the committed yaml omits it
			// (the silent-skip class); strict must fail because the block is non-optional.
			{"path": "database", "fields": []map[string]any{{"name": "dsn", "type": "string"}}},
		},
		envProdVisible: "server:\n  host: localhost\n",
	})

	fake := newPublishFake(t)
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	// Non-strict --dry-run behavior is UNCHANGED: it passes despite the
	// unpublished database block.
	if err := runPublishConfig(ioctx, []string{"--dry-run", "--json"}); err != nil {
		t.Fatalf("plain --dry-run must still pass: %v", err)
	}

	*stdout = nil
	err := runPublishConfig(ioctx, []string{"--dry-run", "--strict", "--json"})
	if err == nil {
		t.Fatal("--dry-run --strict accepted a declared non-optional block with zero writes")
	}
	if clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("exit = %d, want ExitUsage", clicore.ExitCode(err))
	}
	if !strings.Contains(err.Error(), "database") {
		t.Fatalf("error = %q, want the unpublished block named", err.Error())
	}
	// Strict is a pure local read: no auth, no writes, no scaffold.
	if fake.tokenRequests != 0 || fake.registeredSchema != nil || len(fake.configWrites) > 0 {
		t.Fatalf("--strict made network calls: token=%d schema=%v writes=%d", fake.tokenRequests, fake.registeredSchema, len(fake.configWrites))
	}
	if _, statErr := os.Stat(filepath.Join(workspaceRoot, "conf", "env.prod.yaml")); statErr != nil {
		t.Fatalf("strict must not rewrite the committed env file: %v", statErr)
	}
	// The plan is still emitted before the failure so --strict doubles as a gate.
	var plan map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &plan)
	if plan["status"] != "dry-run" {
		t.Fatalf("plan status = %v, want the dry-run plan emitted before failing", plan["status"])
	}
}

func TestPublishConfig_StrictDryRunAllowsOptionalEmptyBlockAndPasses(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
			// telemetry is OPTIONAL: an empty (unpublished) optional block is not a
			// strict failure.
			{"path": "telemetry", "optional": true, "fields": []map[string]any{{"name": "endpoint", "type": "string"}}},
		},
		envProdVisible: "server:\n  host: localhost\n",
	})

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runPublishConfig(ioctx, []string{"--dry-run", "--strict"}); err != nil {
		t.Fatalf("strict must pass: the empty block is optional and the required block has values: %v", err)
	}
	if fake.tokenRequests != 0 || fake.registeredSchema != nil || len(fake.configWrites) > 0 {
		t.Fatalf("--strict made network calls: token=%d schema=%v writes=%d", fake.tokenRequests, fake.registeredSchema, len(fake.configWrites))
	}
}

func TestPublishConfig_StrictRequiresDryRun(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
		envProdVisible: "server:\n  host: localhost\n",
	})

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runPublishConfig(ioctx, []string{"--strict"})
	if err == nil {
		t.Fatal("expected --strict without --dry-run to be a usage error")
	}
	if clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "--dry-run") {
		t.Fatalf("error = %q (exit %d), want --dry-run guidance / ExitUsage", err.Error(), clicore.ExitCode(err))
	}
	if fake.tokenRequests != 0 || fake.registeredSchema != nil || len(fake.configWrites) > 0 {
		t.Fatalf("--strict guard made network calls: token=%d schema=%v writes=%d", fake.tokenRequests, fake.registeredSchema, len(fake.configWrites))
	}
}

func TestPublishConfig_StrictDryRunFailsOnPlaceholderValue(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
		envProdVisible: "server:\n  host: " + publishConfigPlaceholder + "\n",
	})

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	// Non-strict --dry-run is unchanged: an unedited placeholder passes silently
	// (a valid string, no readiness check).
	if err := runPublishConfig(ioctx, []string{"--dry-run"}); err != nil {
		t.Fatalf("plain --dry-run must still pass with a placeholder: %v", err)
	}
	err := runPublishConfig(ioctx, []string{"--dry-run", "--strict"})
	if err == nil {
		t.Fatal("--dry-run --strict accepted an unedited placeholder value")
	}
	if clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("exit = %d, want ExitUsage", clicore.ExitCode(err))
	}
	if !strings.Contains(err.Error(), "server.host") || !strings.Contains(err.Error(), publishConfigPlaceholder) {
		t.Fatalf("error = %q, want the placeholder marker path", err.Error())
	}
}

func TestPublishConfig_RejectsSchemaInvalidValueBeforeWrite(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{
				{"name": "host", "type": "string"}, {"name": "port", "type": "int"},
			}},
		},
		// port is declared int; a non-numeric string is the value class the
		// runtime could not load. It must fail client-side before
		// the schema or any config block is written.
		envYAML: "server:\n  host: localhost\n  port: not-a-number\n",
	})

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runPublishConfig(ioctx, []string{})
	if err == nil {
		t.Fatal("publish-config accepted a schema-invalid value")
	}
	if !strings.Contains(err.Error(), "server") {
		t.Errorf("error missing block context: %v", err)
	}
	if fake.registeredSchema != nil || len(fake.configWrites) > 0 {
		t.Fatalf("nothing should have been written; schema=%v writes=%d", fake.registeredSchema, len(fake.configWrites))
	}
}

func TestPublishConfig_DryRunReportsValidationDiagnostics(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{
				{"name": "host", "type": "string"}, {"name": "port", "type": "int"},
			}},
		},
		envYAML: "server:\n  host: localhost\n  port: not-a-number\n",
	})

	fake := newPublishFake(t)
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	// --dry-run must surface the diagnostics AND fail, without writing.
	err := runPublishConfig(ioctx, []string{"--dry-run", "--json"})
	if err == nil {
		t.Fatal("--dry-run did not fail on a schema-invalid value")
	}
	if fake.tokenRequests != 0 || fake.registeredSchema != nil || len(fake.configWrites) > 0 {
		t.Fatalf("--dry-run made network calls: token=%d schema=%v writes=%d", fake.tokenRequests, fake.registeredSchema, len(fake.configWrites))
	}
	var plan map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &plan)
	if plan["validation"] == nil {
		t.Fatalf("dry-run plan did not include validation diagnostics: %v", plan)
	}
}

func TestPublishConfig_DefaultsAndSensitiveFields(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "session", "fields": []map[string]any{
				{"name": "store", "type": "string", "default": "cookie"},
				{"name": "ttl", "type": "int", "default": "604800"},
				{"name": "cookieSecret", "type": "string", "sensitive": true},
			}},
		},
		envYAML: "session:\n  cookieSecret: should-not-be-config\n  extraSecret: should-also-not-be-config\n",
	})

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runPublishConfig(ioctx, nil); err != nil {
		t.Fatalf("publish-config: %v", err)
	}
	if len(fake.configWrites) != 1 {
		t.Fatalf("configWrites = %d, want 1", len(fake.configWrites))
	}
	values := fake.configWrites[0]["values"].(map[string]any)
	if _, ok := values["cookieSecret"]; ok {
		t.Fatalf("sensitive field was written as config: %v", values)
	}
	if _, ok := values["extraSecret"]; ok {
		t.Fatalf("unknown field was written as config: %v", values)
	}
	if values["store"] != "cookie" {
		t.Errorf("session.store = %v, want cookie", values["store"])
	}
	if values["ttl"] != float64(604800) && values["ttl"] != int64(604800) {
		t.Errorf("session.ttl = %#v, want numeric 604800", values["ttl"])
	}
}

func TestPublishConfig_GeneratedEnvArtifactOverridesSourceValues(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
		envProdYAML: "server:\n  host: source.example\n",
	})
	genDir := filepath.Join(workspaceRoot, ".gen", "conf")
	mustMkdir(t, genDir)
	if err := os.WriteFile(filepath.Join(genDir, ".env.prod.yaml"), []byte("server:\n  host: generated.example\n"), 0o644); err != nil {
		t.Fatalf("write generated env artifact: %v", err)
	}

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runPublishConfig(ioctx, nil); err != nil {
		t.Fatalf("publish-config: %v", err)
	}
	if len(fake.configWrites) != 1 {
		t.Fatalf("configWrites = %d, want 1", len(fake.configWrites))
	}
	values := fake.configWrites[0]["values"].(map[string]any)
	if values["host"] != "generated.example" {
		t.Errorf("server.host = %v, want generated.example", values["host"])
	}
}

func TestPublishConfig_VisibleEnvFileIsAutoDiscovered(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
		envProdVisible: "server:\n  host: committed.example\n",
	})

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runPublishConfig(ioctx, nil); err != nil {
		t.Fatalf("publish-config: %v", err)
	}
	if len(fake.configWrites) != 1 {
		t.Fatalf("configWrites = %d, want 1", len(fake.configWrites))
	}
	values := fake.configWrites[0]["values"].(map[string]any)
	if values["host"] != "committed.example" {
		t.Errorf("server.host = %v, want committed.example", values["host"])
	}
}

func TestPublishConfig_ConfigFromRejected(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runPublishConfig(ioctx, []string{"apps/auth-server", "--config-from", "conf/env.prod.yaml"})
	if err == nil {
		t.Fatal("expected --config-from to be rejected")
	}
	if !strings.Contains(err.Error(), "cloud config put") {
		t.Fatalf("error = %q, want config put guidance", err.Error())
	}
	if fake.tokenRequests != 0 || fake.registeredSchema != nil || len(fake.configWrites) > 0 {
		t.Fatalf("--config-from rejection made network calls: token=%d schema=%v writes=%d", fake.tokenRequests, fake.registeredSchema, len(fake.configWrites))
	}
}

func TestPublishConfig_CreatesEnvScaffoldAndFailsWhenValuesMissing(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "database", "fields": []map[string]any{
				{"name": "default", "type": "object", "required": true, "fields": []map[string]any{
					{"name": "host", "type": "string"},
					{"name": "database", "type": "string"},
					{"name": "ssl", "type": "bool", "default": "false"},
				}},
			}},
			{"path": "session", "fields": []map[string]any{
				{"name": "cookieSecret", "type": "string", "sensitive": true},
				{"name": "ttl", "type": "int", "default": "604800"},
			}},
		},
	})

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runPublishConfig(ioctx, nil)
	if err == nil {
		t.Fatal("expected missing values to fail publish")
	}
	if !strings.Contains(err.Error(), publishConfigPlaceholder) || !strings.Contains(err.Error(), "database.default.host") {
		t.Fatalf("error = %q, want placeholder and missing key guidance", err.Error())
	}
	scaffoldPath := filepath.Join(workspaceRoot, "conf", "env.prod.yaml")
	data, readErr := os.ReadFile(scaffoldPath)
	if readErr != nil {
		t.Fatalf("read scaffold: %v", readErr)
	}
	scaffold := string(data)
	for _, fragment := range []string{publishConfigPlaceholder, "database:", "default:", "host:", "database:"} {
		if !strings.Contains(scaffold, fragment) {
			t.Fatalf("scaffold missing %q:\n%s", fragment, scaffold)
		}
	}
	if strings.Contains(scaffold, "cookieSecret") || strings.Contains(scaffold, "ttl") {
		t.Fatalf("scaffold included sensitive/default-only fields:\n%s", scaffold)
	}
	if fake.registeredSchema != nil || len(fake.configWrites) > 0 {
		t.Fatalf("schema/config should not be published when scaffold is required: schema=%v writes=%d", fake.registeredSchema, len(fake.configWrites))
	}
}

func TestPublishConfig_PlaceholderFailsUntilEdited(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
		envProdVisible: "server:\n  host: " + publishConfigPlaceholder + "\n",
	})

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runPublishConfig(ioctx, nil)
	if err == nil {
		t.Fatal("expected placeholder to fail publish")
	}
	if !strings.Contains(err.Error(), "server.host") {
		t.Fatalf("error = %q, want marker path", err.Error())
	}
	if fake.registeredSchema != nil || len(fake.configWrites) > 0 {
		t.Fatalf("schema/config should not be published with markers: schema=%v writes=%d", fake.registeredSchema, len(fake.configWrites))
	}
}

func TestPublishConfig_TextOutputShowsMissingValuesWarning(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
	})

	fake := newPublishFake(t)
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runPublishConfig(ioctx, []string{"--dry-run"}); err != nil {
		t.Fatalf("publish-config --dry-run: %v", err)
	}
	out := strings.Join(*stdout, "\n")
	for _, fragment := range []string{"0 config block", "Warnings:", "no publishable config value files found", "conf/env.prod.yaml"} {
		if !strings.Contains(out, fragment) {
			t.Errorf("output missing %q:\n%s", fragment, out)
		}
	}
	if fake.tokenRequests != 0 || fake.registeredSchema != nil || len(fake.configWrites) > 0 {
		t.Fatalf("--dry-run made network calls: token=%d schema=%v writes=%d", fake.tokenRequests, fake.registeredSchema, len(fake.configWrites))
	}
}

func TestPublishConfig_Idempotent(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
		envYAML: "server:\n  host: localhost\n",
	})
	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	for i := range 2 {
		if err := runPublishConfig(ioctx, nil); err != nil {
			t.Fatalf("publish-config run %d: %v", i+1, err)
		}
	}
	// Two rounds: the server-side store de-dupes; the client just resends.
	// The schema field must remain stable across runs (no version bump that
	// the CLI wasn't asked to do), and each round adds one config write.
	if fake.registeredSchema["appName"] != "my-app" {
		t.Errorf("schema after re-publish = %v", fake.registeredSchema)
	}
	if len(fake.configWrites) != 2 {
		t.Fatalf("configWrites across 2 runs = %d, want 2", len(fake.configWrites))
	}
}

func TestPublishConfig_SchemaGatedConfigFailureSurfacesHint(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
		envYAML: "server:\n  host: localhost\n",
	})
	fake := newPublishFake(t)
	// Configs phase returns the schema-gating envelope. This should be
	// impossible after a successful schema POST, but if the server returns
	// it the CLI must explain WHY (so the user doesn't chase a phantom
	// `publish` re-run).
	fake.configPutStatus = http.StatusBadRequest
	fake.configPutErrorBody = map[string]any{"error": "no schema registered for app; publish schema before writing configs"}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runPublishConfig(ioctx, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, fragment := range []string{"my-app", "no schema registered", "POST"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Logf("error: %q", err.Error())
			t.Errorf("error missing fragment %q", fragment)
		}
	}
}

func TestPublishConfig_OutOfSchemaYAMLKeyWarns(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
		envYAML: "server:\n  host: localhost\nunknown:\n  thing: 1\n",
	})

	fake := newPublishFake(t)
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runPublishConfig(ioctx, []string{"--json"}); err != nil {
		t.Fatalf("publish-config: %v", err)
	}
	var out map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &out)
	warnings, _ := out["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatal("expected at least one warning")
	}
	joined := ""
	for _, w := range warnings {
		joined += clicore.StringValue(w) + " "
	}
	if !strings.Contains(joined, "unknown") {
		t.Errorf("warning missing %q: %s", "unknown", joined)
	}
}

func TestPublishConfig_SchemaFromOverride(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")
	// Schema at a non-default path → only --schema-from finds it.
	customSchema := filepath.Join(workspaceRoot, "custom", "schema.json")
	mustMkdir(t, filepath.Dir(customSchema))
	writeJSONFile(t, customSchema, map[string]any{
		"appName": "my-app", "version": "1.0.0", "schemaHash": "sha256:custom",
		"configs": []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
	})
	// conf/.env.yaml lives at the project root since writeProjectFile
	// places the app there.
	confDir := filepath.Join(workspaceRoot, "conf")
	mustMkdir(t, confDir)
	if err := os.WriteFile(filepath.Join(confDir, ".env.yaml"), []byte("server:\n  host: example.com\n"), 0o644); err != nil {
		t.Fatalf("write .env.yaml: %v", err)
	}

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runPublishConfig(ioctx, []string{"--schema-from", "custom/schema.json"}); err != nil {
		t.Fatalf("publish-config: %v", err)
	}
	if fake.registeredSchema["schemaHash"] != "sha256:custom" {
		t.Errorf("schema hash = %v", fake.registeredSchema["schemaHash"])
	}
	if len(fake.configWrites) != 1 || fake.configWrites[0]["path"] != "server" {
		t.Fatalf("configWrites = %v", fake.configWrites)
	}
}

func TestPublishConfig_RequiresLink(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
	})
	// No link file.

	fake := newPublishFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runPublishConfig(ioctx, nil)
	if err == nil {
		t.Fatal("expected link error")
	}
	if !strings.Contains(err.Error(), "putnami cloud setup") {
		t.Errorf("error = %q", err.Error())
	}
}
