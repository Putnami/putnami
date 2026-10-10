package cloudcli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
)

// The config command's behavior tests live with their source in
// internal/configcli. What remains here are the
// reveal-secrets prompt tests that can only run against the aggregator: they
// drive RunMain (the --putnamiContext parsing + parent-dispatch confirm wiring)
// and the cli.go package var ttyDevice, neither of which a standalone config
// lib can host. The configFakeServer fixture travels with them.

type configFakeServer struct {
	t                 *testing.T
	requests          []map[string]any
	configWrites      []map[string]any
	config            map[string]any
	schema            map[string]any
	schemaQueryStatus int
	schemaPaths       []string
	warnings          []string
	secrets           []map[string]any
	secretStatus      []map[string]any
	secretStatusCode  int
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
		if workspaceID := stringValue(body["workspace_id"]); workspaceID != "" {
			extra["scope_ref"] = map[string]any{"workspace_id": workspaceID}
		}
		return jsonResponse(http.StatusOK, tokenResponse(extra)), nil
	case strings.HasSuffix(req.URL.Path, "/configs/resolve") && req.Method == http.MethodPost:
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
		resp := map[string]any{
			"config":      config,
			"resolved":    len(config) > 0,
			"schemaMatch": true,
			"layers": []map[string]any{
				{"dimension": "apps/auth-server/prod", "priority": 40},
			},
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

func TestRunMainConfigInspectRevealSecretsUsesConfirmIO(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeJSONFile(t, contextFile, map[string]any{"workspaceRoot": workspaceRoot, "params": map[string]any{}})

	fake := &configFakeServer{
		t: t,
		config: map[string]any{
			"session": map[string]any{"cookieSecret": "plain-secret"},
		},
	}
	var stdout []string
	var stderr []string
	confirmCalled := false

	code := RunMain([]string{"config", "apps/auth-server", "--reveal-secrets", "--putnamiContext", contextFile}, IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":    testBaseURL,
			"PUTNAMI_INTERACTIVE": "1",
		}),
		Stdout: func(line string) { stdout = append(stdout, line) },
		Stderr: func(line string) { stderr = append(stderr, line) },
		JSON:   func(any) { t.Fatal("human reveal output must print text, not emit a structured result") },
		Confirm: func(question string) (string, bool) {
			confirmCalled = true
			if !strings.Contains(question, "Reveal plaintext secrets for apps/auth-server/prod config?") {
				t.Fatalf("question = %q", question)
			}
			return "y", true
		},
		Client: &http.Client{Transport: fake},
		Now:    func() time.Time { return time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC) },
	})
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, strings.Join(stderr, "\n"))
	}
	if !confirmCalled {
		t.Fatal("expected RunMain to pass Confirm into the config command")
	}
	if len(fake.requests) != 1 || fake.requests[0]["secretsMode"] != "reveal" {
		t.Fatalf("requests = %#v, want secretsMode reveal", fake.requests)
	}
	joined := strings.Join(stdout, "\n")
	if !strings.Contains(joined, "plain-secret") {
		t.Fatalf("stdout = %q, want revealed config JSON", joined)
	}
}

// stubConfirmStdin reroutes the default confirm prompt to in-memory pipes:
// the answer comes from the given string and the returned func yields the
// question text written to stderr. Forces the os.Stdin fallback
// (ttyDevice = "") so tests never block on the real controlling terminal.
func stubConfirmStdin(t *testing.T, answer string) func() string {
	t.Helper()
	oldStdin := os.Stdin
	oldStderr := os.Stderr
	oldTTY := ttyDevice
	ttyDevice = ""

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	os.Stdin = inR
	os.Stderr = errW
	if _, err := inW.WriteString(answer); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if err := inW.Close(); err != nil {
		t.Fatalf("close stdin writer: %v", err)
	}
	t.Cleanup(func() {
		os.Stdin = oldStdin
		os.Stderr = oldStderr
		ttyDevice = oldTTY
		_ = inR.Close()
		_ = errR.Close()
		_ = errW.Close()
	})
	return func() string {
		if err := errW.Close(); err != nil {
			t.Fatalf("close stderr writer: %v", err)
		}
		prompt, err := io.ReadAll(errR)
		if err != nil {
			t.Fatalf("read stderr: %v", err)
		}
		return string(prompt)
	}
}

func TestRunMainConfigInspectRevealSecretsPromptsWhenParentDispatched(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeJSONFile(t, contextFile, map[string]any{"workspaceRoot": workspaceRoot, "params": map[string]any{}})

	fake := &configFakeServer{
		t: t,
		config: map[string]any{
			"session": map[string]any{"cookieSecret": "plain-secret"},
		},
	}
	readPrompt := stubConfirmStdin(t, "y\n")
	var stdout []string
	var stderr []string

	code := RunMain([]string{"config", "apps/auth-server", "--reveal-secrets", "--putnamiContext", contextFile}, IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":    testBaseURL,
			"PUTNAMI_INTERACTIVE": "1",
		}),
		Stdout: func(line string) { stdout = append(stdout, line) },
		Stderr: func(line string) { stderr = append(stderr, line) },
		Client: &http.Client{Transport: fake},
		Now:    func() time.Time { return time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC) },
	})
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, strings.Join(stderr, "\n"))
	}
	if !strings.Contains(readPrompt(), "Reveal plaintext secrets for apps/auth-server/prod config? [y/N]: ") {
		t.Fatal("expected the y/N question on the interactive prompt")
	}
	if len(fake.requests) != 1 || fake.requests[0]["secretsMode"] != "reveal" {
		t.Fatalf("requests = %#v, want secretsMode reveal", fake.requests)
	}
	if !strings.Contains(strings.Join(stdout, "\n"), "plain-secret") {
		t.Fatalf("stdout = %q, want revealed config JSON", strings.Join(stdout, "\n"))
	}
}

func TestRunMainConfigInspectRevealSecretsPromptDeclineCancels(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeJSONFile(t, contextFile, map[string]any{"workspaceRoot": workspaceRoot, "params": map[string]any{}})

	fake := &configFakeServer{t: t}
	stubConfirmStdin(t, "n\n")
	var stderr []string

	code := RunMain([]string{"config", "apps/auth-server", "--reveal-secrets", "--putnamiContext", contextFile}, IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":    testBaseURL,
			"PUTNAMI_INTERACTIVE": "1",
		}),
		Stdout: func(string) {},
		Stderr: func(line string) { stderr = append(stderr, line) },
		Client: &http.Client{Transport: fake},
		Now:    func() time.Time { return time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC) },
	})
	if code != ExitUsage {
		t.Fatalf("code = %d, stderr = %q; want usage", code, strings.Join(stderr, "\n"))
	}
	if len(fake.requests) != 0 {
		t.Fatalf("requests = %#v, want no config resolve call after decline", fake.requests)
	}
	if !strings.Contains(strings.Join(stderr, "\n"), "reveal canceled") {
		t.Fatalf("stderr = %q, want reveal canceled", strings.Join(stderr, "\n"))
	}
}

func TestRunMainConfigInspectRevealSecretsRequiresYesWhenNonInteractive(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeJSONFile(t, contextFile, map[string]any{"workspaceRoot": workspaceRoot, "params": map[string]any{}})

	fake := &configFakeServer{t: t}
	// No PUTNAMI_INTERACTIVE and a piped (non-char-device) stdin: JSONL job
	// mode, where the reveal must stay fail-fast instead of prompting.
	stubConfirmStdin(t, "")
	var stdout []string

	code := RunMain([]string{"config", "apps/auth-server", "--reveal-secrets", "--putnamiContext", contextFile}, IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL": testBaseURL,
		}),
		Stdout: func(line string) { stdout = append(stdout, line) },
		Stderr: func(string) {},
		Client: &http.Client{Transport: fake},
		Now:    func() time.Time { return time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC) },
	})
	if code != ExitUsage {
		t.Fatalf("code = %d, stdout = %q; want usage", code, strings.Join(stdout, "\n"))
	}
	if len(fake.requests) != 0 {
		t.Fatalf("requests = %#v, want no config resolve call without --yes", fake.requests)
	}
	if !strings.Contains(strings.Join(stdout, "\n"), "reveal requires --yes in non-interactive environments") {
		t.Fatalf("stdout = %q, want --yes guidance diagnostic", strings.Join(stdout, "\n"))
	}
}
