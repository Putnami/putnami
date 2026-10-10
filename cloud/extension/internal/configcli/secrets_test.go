package configcli

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// secretsFakeServer is a focused in-memory stand-in for control-plane-api.
// It only models the /api/secrets/* routes the CLI hits (now folded
// into control-plane-api after the config-server merge), plus the auth
// refresh + workspace-fetch endpoints the shared activeAuth path needs.
type secretsFakeServer struct {
	t           *testing.T
	store       map[string]map[string]string // path → field → value
	updatedAt   string
	publishHint bool
	requests    []map[string]any
}

func (s *secretsFakeServer) RoundTrip(req *http.Request) (*http.Response, error) {
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
	case strings.HasSuffix(req.URL.Path, "/secrets") && req.Method == http.MethodPut:
		if s.publishHint {
			return jsonResponse(http.StatusBadRequest, map[string]any{
				"error": "no schema registered for app; publish schema before writing secrets",
			}), nil
		}
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)
		s.requests = append(s.requests, body)
		path := clicore.StringValue(body["path"])
		values, _ := body["values"].(map[string]any)
		if s.store[path] == nil {
			s.store[path] = map[string]string{}
		}
		for k, v := range values {
			s.store[path][k] = clicore.StringValue(v)
		}
		s.updatedAt = "2026-05-19T09:00:00Z"
		return jsonResponse(http.StatusOK, map[string]any{
			"appName": body["appName"], "environment": body["environment"],
			"path": path, "status": "encrypted",
		}), nil
	case strings.HasSuffix(req.URL.Path, "/secrets") && req.Method == http.MethodGet:
		entries := make([]map[string]any, 0, len(s.store))
		appName := clicore.FirstString(req.URL.Query().Get("appName"), "my-app")
		environment := clicore.FirstString(req.URL.Query().Get("environment"), "prod")
		for path := range s.store {
			entries = append(entries, map[string]any{
				"appName":     appName,
				"environment": environment,
				"path":        path, "updatedAt": s.updatedAt,
			})
		}
		return jsonResponse(http.StatusOK, map[string]any{"entries": entries}), nil
	case strings.HasSuffix(req.URL.Path, "/secrets/resolve"):
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)
		merged := map[string]any{}
		for path, fields := range s.store {
			block := map[string]any{}
			for k, v := range fields {
				block[k] = v
			}
			merged[path] = block
		}
		// Mirror the server's resolve response shape: when secrets exist,
		// expose a layer list so the CLI can surface inheritance.
		var layers []map[string]any
		if len(merged) > 0 {
			dim := clicore.FirstString(clicore.StringValue(body["appName"]), "my-app") + "/" + clicore.FirstString(clicore.StringValue(body["environment"]), "prod")
			layers = []map[string]any{{"dimension": dim, "priority": 10}}
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"secrets": merged, "resolved": len(merged) > 0,
			"layers": layers,
		}), nil
	case strings.HasSuffix(req.URL.Path, "/secrets") && req.Method == http.MethodDelete:
		path := req.URL.Query().Get("path")
		if _, ok := s.store[path]; !ok {
			return jsonResponse(http.StatusNotFound, map[string]any{"error": "not found"}), nil
		}
		if field := req.URL.Query().Get("field"); field != "" {
			delete(s.store[path], field)
			if len(s.store[path]) == 0 {
				delete(s.store, path)
			}
		} else {
			delete(s.store, path)
		}
		return jsonResponse(http.StatusOK, map[string]any{"status": "deleted"}), nil
	default:
		s.t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		return nil, nil
	}
}

func newSecretsFake(t *testing.T) *secretsFakeServer {
	return &secretsFakeServer{
		t:         t,
		store:     map[string]map[string]string{},
		updatedAt: "2026-05-19T09:00:00Z",
	}
}

func TestSecretsPositionals_BooleanFlagsDoNotConsumeValues(t *testing.T) {
	pos := secretsPositionals([]string{"set", "--from-stdin", "apps/auth-server", "database.password"})
	if want := []string{"set", "apps/auth-server", "database.password"}; !slices.Equal(pos, want) {
		t.Fatalf("positionals = %v, want %v", pos, want)
	}

	key := secretsKeyPositional([]string{"get", "--reveal", "database.password"})
	if key != "database.password" {
		t.Fatalf("key positional = %q, want database.password", key)
	}
}

func TestSecretsSet_WritesEntry(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	fake := newSecretsFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runSecrets(ioctx, []string{"set", "database.password", "--value", "s3cret"})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if fake.store["database"]["password"] != "s3cret" {
		t.Fatalf("store = %v", fake.store)
	}
}

func TestSecretsSet_ProjectKeyForm(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := newSecretsFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runSecrets(ioctx, []string{"set", "apps/auth-server", "database.password", "--value", "s3cret"})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fake.requests))
	}
	if fake.requests[0]["appName"] != "apps/auth-server" {
		t.Fatalf("appName = %v, want apps/auth-server", fake.requests[0]["appName"])
	}
	if fake.store["database"]["password"] != "s3cret" {
		t.Fatalf("store = %v", fake.store)
	}
}

func TestSecretsRejectsAppFlag(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := newSecretsFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runSecrets(ioctx, []string{"set", "--app", "apps/auth-server", "database.password", "--value", "s3cret"})
	if err == nil {
		t.Fatal("expected --app to be rejected")
	}
	if !strings.Contains(err.Error(), "positionally") {
		t.Fatalf("error = %q, want positional guidance", err.Error())
	}
}

func TestSecretsSet_FromFile(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")
	secretFile := filepath.Join(workspaceRoot, "secret.txt")
	if err := writeFile(secretFile, "from-file-token\n"); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	fake := newSecretsFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runSecrets(ioctx, []string{"set", "database.password", "--from-file", secretFile})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if fake.store["database"]["password"] != "from-file-token" {
		t.Fatalf("store = %v", fake.store)
	}
}

func TestSecretsSet_PublishFirstHint(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	fake := newSecretsFake(t)
	fake.publishHint = true
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runSecrets(ioctx, []string{"set", "database.password", "--value", "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, fragment := range []string{"my-app", "publish", "schema"} {
		if !strings.Contains(msg, fragment) {
			t.Errorf("error %q missing fragment %q", msg, fragment)
		}
	}
}

func TestSecretsSet_RequiresKeyForm(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	fake := newSecretsFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	cases := []struct {
		name string
		args []string
	}{
		{"no key", []string{"set", "--value", "x"}},
		{"missing field", []string{"set", "database", "--value", "x"}},
		{"trailing dot", []string{"set", "database.", "--value", "x"}},
		{"only block flag", []string{"set", "--block", "database", "--value", "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := runSecrets(ioctx, tc.args); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestSecretsSet_BlockFieldFlags(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	fake := newSecretsFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runSecrets(ioctx, []string{
		"set", "--block", "outer.inner", "--field", "secret", "--value", "v",
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if fake.store["outer.inner"]["secret"] != "v" {
		t.Fatalf("store = %v", fake.store)
	}
}

func TestSecretsList_PrintsEntries(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	fake := newSecretsFake(t)
	fake.store["database"] = map[string]string{"password": "p"}
	fake.store["redis"] = map[string]string{"password": "r"}

	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runSecrets(ioctx, []string{"list"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "database") || !strings.Contains(joined, "redis") {
		t.Errorf("stdout = %q", joined)
	}
	if !strings.Contains(joined, "updated") {
		t.Errorf("missing updated-at column: %s", joined)
	}
}

func TestSecretsList_ProjectPositional(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := newSecretsFake(t)
	fake.store["database"] = map[string]string{"password": "p"}

	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runSecrets(ioctx, []string{"list", "apps/auth-server", "--json"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	var out map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &out)
	if out["app"] != "apps/auth-server" {
		t.Fatalf("app = %v, want apps/auth-server", out["app"])
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries = %v, want 1", out["entries"])
	}
}

func TestSecretsGet_NoRevealShowsMetadataOnly(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	fake := newSecretsFake(t)
	fake.store["database"] = map[string]string{"password": "s3cret"}

	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runSecrets(ioctx, []string{"get", "database.password"}); err != nil {
		t.Fatalf("get: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if strings.Contains(joined, "s3cret") {
		t.Errorf("plaintext leaked without --reveal: %s", joined)
	}
	if !strings.Contains(joined, "updated") {
		t.Errorf("metadata missing: %s", joined)
	}
}

func TestSecretsGet_ReportsLayerProvenance(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	fake := newSecretsFake(t)
	fake.store["database"] = map[string]string{"password": "s3cret"}

	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runSecrets(ioctx, []string{"get", "database.password", "--json"}); err != nil {
		t.Fatalf("get: %v", err)
	}
	var out map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &out)
	if exists, _ := out["exists"].(bool); !exists {
		t.Errorf("exists = %v", out["exists"])
	}
	layers, ok := out["layers"].([]any)
	if !ok || len(layers) != 1 || layers[0] != "my-app/prod" {
		t.Errorf("layers = %v", out["layers"])
	}
	if _, leaked := out["value"]; leaked {
		t.Errorf("non-reveal must not include value: %v", out)
	}
}

func TestSecretsGet_RevealOnNonTTYSkipsPrompt(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	fake := newSecretsFake(t)
	fake.store["database"] = map[string]string{"password": "s3cret"}

	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot
	// Confirm returns ok=false → CLI treats as non-interactive and skips
	// the prompt, printing without a confirmation challenge.
	ioctx.Confirm = func(string) (string, bool) { return "", false }

	if err := runSecrets(ioctx, []string{"get", "database.password", "--reveal"}); err != nil {
		t.Fatalf("get: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "s3cret") {
		t.Errorf("--reveal should print plaintext: %s", joined)
	}
}

func TestSecretsGet_RevealOnTTYHonorsPromptNo(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	fake := newSecretsFake(t)
	fake.store["database"] = map[string]string{"password": "s3cret"}

	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot
	prompted := false
	ioctx.Confirm = func(string) (string, bool) {
		prompted = true
		return "n", true
	}
	err := runSecrets(ioctx, []string{"get", "database.password", "--reveal"})
	if err == nil {
		t.Fatal("expected reveal-canceled error")
	}
	if !prompted {
		t.Fatal("expected Confirm to be called on TTY")
	}
	for _, line := range *stdout {
		if strings.Contains(line, "s3cret") {
			t.Errorf("plaintext leaked after declining: %s", line)
		}
	}
}

func TestSecretsReveal_ProjectKeyRequiresYesInNonTTY(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := newSecretsFake(t)
	fake.store["database"] = map[string]string{"password": "s3cret"}

	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot
	ioctx.Confirm = func(string) (string, bool) { return "", false }

	err := runSecrets(ioctx, []string{"reveal", "apps/auth-server", "database.password"})
	if err == nil {
		t.Fatal("expected non-interactive reveal to require --yes")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error = %q, want --yes hint", err.Error())
	}
}

func TestSecretsReveal_ProjectKeyWithYesPrintsPlaintext(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := newSecretsFake(t)
	fake.store["database"] = map[string]string{"password": "s3cret"}

	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot
	ioctx.Confirm = func(string) (string, bool) { return "", false }

	if err := runSecrets(ioctx, []string{"reveal", "apps/auth-server", "database.password", "--yes"}); err != nil {
		t.Fatalf("reveal: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "s3cret") {
		t.Errorf("stdout = %q, want plaintext", joined)
	}
}

func TestSecretsReveal_ProjectPrintsPlaintextTreeAsText(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	fake := newSecretsFake(t)
	fake.store["database"] = map[string]string{"password": "s3cret"}

	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot
	ioctx.Confirm = func(string) (string, bool) { return "", false }
	ioctx.JSON = func(any) { t.Fatal("human reveal output must print text, not emit a structured result") }

	if err := runSecrets(ioctx, []string{"reveal", "apps/auth-server", "--yes"}); err != nil {
		t.Fatalf("reveal: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "s3cret") {
		t.Errorf("stdout = %q, want plaintext tree", joined)
	}
}

func TestSecretsDelete_IdempotentOn404(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	fake := newSecretsFake(t)
	// No store entries — delete will 404 from the fake.
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runSecrets(ioctx, []string{"delete", "database.password"}); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "absent") {
		t.Errorf("expected absent-style message; got %q", joined)
	}
}

func TestSecretsDelete_RemovesEntry(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	fake := newSecretsFake(t)
	fake.store["database"] = map[string]string{"password": "s3cret"}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runSecrets(ioctx, []string{"delete", "database.password"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, exists := fake.store["database"]; exists {
		t.Errorf("entry still present after delete: %v", fake.store)
	}
}

func TestSecretsDelete_RemovesOneField(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	fake := newSecretsFake(t)
	fake.store["session"] = map[string]string{"cookieSecret": "secret", "cookieSalt": "salt"}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runSecrets(ioctx, []string{"delete", "session.cookieSalt"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, exists := fake.store["session"]["cookieSalt"]; exists {
		t.Errorf("cookieSalt still present after delete: %v", fake.store)
	}
	if fake.store["session"]["cookieSecret"] != "secret" {
		t.Errorf("cookieSecret not preserved after delete: %v", fake.store)
	}
}

func TestSecretsRequiresLink(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	// No link file — operation must abort with a clear message.

	fake := newSecretsFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runSecrets(ioctx, []string{"get", "database.password"})
	if err == nil {
		t.Fatal("expected link error")
	}
	if !strings.Contains(err.Error(), "putnami cloud setup") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSecretsRequiresAppHint(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	// No putnami.json at workspace root → must require an app target.

	fake := newSecretsFake(t)
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runSecrets(ioctx, []string{"get", "database.password"})
	if err == nil {
		t.Fatal("expected app targeting error")
	}
	if !strings.Contains(err.Error(), "first positional") {
		t.Errorf("error = %q", err.Error())
	}
}
