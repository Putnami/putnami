package cloudcli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	configcli "go.putnami.dev/cloud/extension/internal/configcli"
	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/releaseset"
)

func TestManagedConfigPublishUsesExactPackagedSelectionAndEmitsMember(t *testing.T) {
	root, prepared := configMemberPackageFixture(t)
	bearer := jwt(map[string]any{"aud": "distribution", "scope": "put", "sub": "fixture-publisher"})
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		if got := request.Header.Get("Authorization"); got != "Bearer "+bearer {
			t.Fatalf("authorization = %q", got)
		}
		if request.URL.Path != "/put/workspace-native/my-app-config/publish" {
			t.Fatalf("request path = %s", request.URL.Path)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["channel"]) != `""` || string(body["media_type"]) != `"application/vnd.putnami.config.authored-member+json"` ||
			string(body["version"]) != `"1.2.3"` || string(body["payload"]) != string(prepared.Member.Bytes) {
			t.Fatalf("publish body = %s", mustJSONBytes(t, body))
		}
		writeCreatedJSONResponse(t, w, map[string]any{
			"package": prepared.Coordinate,
			"version": map[string]any{
				"id": "version-1", "package_id": "package-1", "version": prepared.Version,
				"manifest_id": "manifest-1", "state": "published", "visibility": "private",
			},
			"manifest": map[string]any{
				"id": "manifest-1", "package_id": "package-1",
				"media_type": "application/vnd.putnami.config.authored-member+json", "payload": body["payload"],
			},
			"channel": nil,
		})
	}))
	defer server.Close()
	env := configMemberPutCredential(t, server.URL, bearer)
	planFingerprint := "sha256:" + strings.Repeat("f", 64)
	if planFingerprint == prepared.Member.Descriptor.SelectionFingerprint {
		t.Fatal("test needs independent Config and package-step fingerprints")
	}
	params := configMemberPlan(prepared, planFingerprint)
	params["registry-put-url"] = server.URL
	var event map[string]any
	err := publishConfig(params, nil, root, env, IO{Client: server.Client(), Artifact: func(id, name, kind, _ string, data map[string]any) error {
		if id != "config-workspace-native-my-app-config" || name != prepared.Coordinate || kind != extensionproto.PublishedMemberEventKind {
			t.Fatalf("event envelope = %s/%s/%s", id, name, kind)
		}
		event = data
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || event["artifactDigest"] != prepared.Member.Descriptor.ContentDigest {
		t.Fatalf("calls=%v event=%+v", calls, event)
	}
}

func TestManagedConfigPublishRejectsSelectionBeforeRegistryAccess(t *testing.T) {
	root, prepared := configMemberPackageFixture(t)
	requests := 0
	params := configMemberPlan(prepared, "sha256:"+strings.Repeat("e", 64))
	plan := params[releaseset.ContextParamName].(*releaseset.Plan)
	plan.Members[0].SourceRevision = strings.Repeat("0", 40)
	err := publishConfig(params, nil, root, nil, IO{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, nil
	})}})
	if err == nil || !strings.Contains(err.Error(), "selected immutable source") || requests != 0 {
		t.Fatalf("error=%v requests=%d", err, requests)
	}
}

func TestConfigMemberSelectionKeepsIndependentCanonicalFingerprints(t *testing.T) {
	_, prepared := configMemberPackageFixture(t)
	planFingerprint := "sha256:" + strings.Repeat("d", 64)
	selected, err := configMemberSelection(configMemberPlan(prepared, planFingerprint), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if selected.SelectionFingerprint != planFingerprint || prepared.Member.Descriptor.SelectionFingerprint == planFingerprint {
		t.Fatalf("plan fingerprint=%q Config fingerprint=%q", selected.SelectionFingerprint, prepared.Member.Descriptor.SelectionFingerprint)
	}
}

func TestValidateConfigUsesLocalArtifactsWithoutPackagingOrNetwork(t *testing.T) {
	root := configValidationFixture(t)
	requests := 0
	artifacts := 0
	var stdout []string
	err := validateConfig(map[string]any{}, []string{"my-app"}, root, IO{
		Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests++
			return nil, nil
		})},
		Artifact: func(_, _, _, _ string, _ map[string]any) error {
			artifacts++
			return nil
		},
		Stdout: func(line string) { stdout = append(stdout, line) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 0 || artifacts != 0 {
		t.Fatalf("validation requests=%d artifacts=%d", requests, artifacts)
	}
	memberPath := configcli.AuthoredConfigMemberPath(root, "my-app")
	if _, statErr := os.Stat(memberPath); !os.IsNotExist(statErr) {
		t.Fatalf("validation created package artifact %s: %v", memberPath, statErr)
	}
	if got := strings.Join(stdout, "\n"); !strings.Contains(got, "Validated authored Config workspace-native/my-app-config@1.2.3 for prod.") {
		t.Fatalf("stdout = %q", got)
	}
}

func TestValidateConfigRejectsUndeclaredOpaqueDescendants(t *testing.T) {
	root := configValidationFixture(t)
	writeJSONFile(t, filepath.Join(root, "schema", "config.json"), map[string]any{
		"appName": "my-app",
		"configs": []any{map[string]any{"path": "auth", "fields": []any{
			map[string]any{"name": "putnami", "type": "object"},
		}}},
	})
	if err := os.WriteFile(filepath.Join(root, "conf", "env.prod.yaml"), []byte("auth:\n  putnami:\n    Issuer: https://issuer.example\n    JWKSURL: https://issuer.example/.well-known/jwks.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	err := validateConfig(map[string]any{}, []string{"my-app"}, root, IO{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, nil
	})}})
	if err == nil || !strings.Contains(err.Error(), "authored descendants do not match the normalized schema") {
		t.Fatalf("undeclared opaque descendants error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("validation made %d HTTP requests before rejecting opaque descendants", requests)
	}
	if _, statErr := os.Stat(configcli.AuthoredConfigMemberPath(root, "my-app")); !os.IsNotExist(statErr) {
		t.Fatalf("rejected validation created a package artifact: %v", statErr)
	}
}

func TestValidateConfigFailsClosedForMissingSchemaAndNamespace(t *testing.T) {
	t.Run("schema", func(t *testing.T) {
		root := configValidationFixture(t)
		if err := os.Remove(filepath.Join(root, "schema", "config.json")); err != nil {
			t.Fatal(err)
		}
		err := validateConfig(map[string]any{"if-present": true}, []string{"my-app"}, root, IO{})
		if err == nil || !strings.Contains(err.Error(), "no schema artifact") {
			t.Fatalf("missing schema error = %v", err)
		}
	})
	t.Run("namespace", func(t *testing.T) {
		root := configValidationFixture(t)
		writeJSONFile(t, filepath.Join(root, "putnami.json"), map[string]any{"name": "my-app"})
		err := validateConfig(map[string]any{"ifPresent": true}, []string{"my-app"}, root, IO{})
		if err == nil || !strings.Contains(err.Error(), "declares no native Config namespace") {
			t.Fatalf("missing namespace error = %v", err)
		}
	})
}

func TestValidateConfigManifestIsPublicUncachedAndLocalOnly(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	groups := manifest["commandGroups"].(map[string]any)
	cloud := groups["cloud"].(map[string]any)
	subcommands := cloud["subcommands"].(map[string]any)
	if node, ok := subcommands["validate-config"].(map[string]any); !ok || node["command"] != "cloud-validate-config" {
		t.Fatalf("cloud validate-config tree node = %#v", subcommands["validate-config"])
	}
	commands := manifest["commands"].(map[string]any)
	command := commands["cloud-validate-config"].(map[string]any)
	run := command["run"].([]any)
	if len(run) != 1 || run[0].(map[string]any)["task"] != "cloud-validate-config" {
		t.Fatalf("cloud-validate-config run = %#v", run)
	}
	tasks := manifest["tasks"].(map[string]any)
	task := tasks["cloud-validate-config"].(map[string]any)
	if task["cache"] != false || task["cwd"] != "{workspaceRoot}" {
		t.Fatalf("cloud-validate-config task = %#v", task)
	}
	for _, forbidden := range []string{"inputs", "writes", "declares"} {
		if _, present := task[forbidden]; present {
			t.Fatalf("local validation task declares %s = %#v", forbidden, task[forbidden])
		}
	}
}

func TestValidateConfigProjectChecksDeclaredMemberAndSkipsUnrelatedProjects(t *testing.T) {
	t.Run("declared production member", func(t *testing.T) {
		root := configValidationFixture(t)
		t.Chdir(root)
		if err := validateConfigProject(map[string]any{"env": "dev"}, []string{"my-app"}, root, IO{Stdout: func(string) {}}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(configcli.AuthoredConfigMemberPath(root, "my-app")); !os.IsNotExist(err) {
			t.Fatalf("validation wrote an artifact: %v", err)
		}
	})
	t.Run("missing declared schema fails", func(t *testing.T) {
		root := configValidationFixture(t)
		if err := os.Remove(filepath.Join(root, "schema", "config.json")); err != nil {
			t.Fatal(err)
		}
		t.Chdir(root)
		err := validateConfigProject(map[string]any{"if-present": true}, []string{"my-app"}, root, IO{})
		if err == nil || !strings.Contains(err.Error(), "no schema artifact") {
			t.Fatalf("missing schema error = %v", err)
		}
	})
	t.Run("unrelated project needs no cloud link", func(t *testing.T) {
		root := t.TempDir()
		writeJSONFile(t, filepath.Join(root, "putnami.json"), map[string]any{"name": "library"})
		t.Chdir(root)
		var output []string
		err := validateConfigProject(map[string]any{}, []string{"library"}, root, IO{Stdout: func(line string) { output = append(output, line) }})
		if err != nil || !strings.Contains(strings.Join(output, "\n"), "skipping authored Config validation") {
			t.Fatalf("unrelated project: err=%v output=%v", err, output)
		}
	})
}

func TestOrdinaryValidateContributesLocalConfigCheck(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Commands map[string]struct {
			Activation      string   `json:"activation"`
			ActivationFiles []string `json:"activationFiles"`
			Run             []struct {
				ID   string `json:"id"`
				Task string `json:"task"`
			} `json:"run"`
		} `json:"commands"`
		Tasks map[string]struct {
			Args  []string         `json:"args"`
			CWD   string           `json:"cwd"`
			Cache taskCacheSetting `json:"cache"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	command := manifest.Commands["validate"]
	if command.Activation != "workspace" || len(command.ActivationFiles) != 1 || command.ActivationFiles[0] != "putnami.json" || len(command.Run) != 2 || command.Run[0].ID != "cloud-config-member" || command.Run[0].Task != "cloud-validate-config-project" {
		t.Fatalf("ordinary validation contribution = %+v", command)
	}
	task := manifest.Tasks[command.Run[0].Task]
	// The check is cached; validate_config_cache_test.go pins its key.
	if !task.Cache.Enabled || task.CWD != "{projectRoot}" || len(task.Args) != 1 || task.Args[0] != "validate-config-project" {
		t.Fatalf("project validation task = %+v", task)
	}
}

func configValidationFixture(t *testing.T) string {
	t.Helper()
	return configMemberSourceFixture(t)
}

func configMemberPackageFixture(t *testing.T) (string, *configcli.PreparedAuthoredConfig) {
	t.Helper()
	root := configMemberSourceFixture(t)
	params := map[string]any{"app": "my-app", "namespace": "workspace-native", "env": "prod"}
	prepared, err := configcli.PackageAuthoredConfig(params, nil, root, IO{})
	if err != nil {
		t.Fatal(err)
	}
	return root, prepared
}

func configMemberSourceFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{".putnami", ".gen", "schema"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeJSONFile(t, filepath.Join(root, "putnami.json"), map[string]any{
		"name":    "my-app",
		"options": map[string]any{"publish": map[string]any{"namespace": "workspace-native"}},
	})
	writeJSONFile(t, filepath.Join(root, ".putnami", "cloud-link.json"), map[string]any{
		"version": 1, "workspace_id": "ws-acme", "environment": "prod", "control_plane_url": "https://control.test",
	})
	writeJSONFile(t, filepath.Join(root, "schema", "config.json"), map[string]any{
		"appName": "my-app",
		"configs": []any{map[string]any{"path": "server", "fields": []any{
			map[string]any{"name": "port", "type": "int", "required": true},
			map[string]any{"name": "token", "type": "string", "required": true, "sensitive": true},
		}}},
	})
	if err := os.MkdirAll(filepath.Join(root, "conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "conf", "env.prod.yaml"), []byte("server:\n  port: 8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(root, ".gen", "version.json"), map[string]any{"version": "1.2.3"})
	isolateGit(t)
	gitCommand(t, root, "init", "-q")
	gitCommand(t, root, "config", "user.email", "test@example.com")
	gitCommand(t, root, "config", "user.name", "Test")
	gitCommand(t, root, "remote", "add", "origin", "git@github.com:acme/storefront.git")
	gitCommand(t, root, "add", ".")
	gitCommand(t, root, "commit", "-qm", "fixture")
	return root
}

func configMemberPlan(prepared *configcli.PreparedAuthoredConfig, fingerprint string) map[string]any {
	return map[string]any{
		"app": "my-app", "namespace": prepared.Namespace, "env": "prod",
		releaseset.ContextParamName: &releaseset.Plan{
			ProtocolVersion: distributionproto.ProtocolVersion, Namespace: "release", Channels: []string{"stable"},
			Heads: map[string]*distributionproto.ChannelHead{"stable": nil},
			Members: []releaseset.PlannedMember{{
				Ecosystem: "put", Coordinate: prepared.Coordinate, Version: prepared.Version,
				SourceRevision: prepared.Member.Descriptor.SourceProvenance.Revision, SelectionFingerprint: fingerprint,
				Selected: true, ProjectID: prepared.ProjectID,
			}},
		},
	}
}

func configMemberPutCredential(t *testing.T, baseURL, bearer string) map[string]string {
	t.Helper()
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PUTNAMI_HOME": t.TempDir(), "PUTNAMI_REGISTRY_PUT_URL": baseURL}
	if err := distributioncli.WriteRegistriesState(env, &distributioncli.RegistriesState{
		Version: 1,
		Keys:    []distributioncli.KeyRef{{Registry: distributioncli.RegistryPut, Host: parsed.Host, URL: baseURL}},
		PutAuth: map[string]string{parsed.Host: bearer},
	}); err != nil {
		t.Fatal(err)
	}
	return env
}

// gitRepositoryEnv names the variables that point git at a repository other
// than the one in its working directory: the output of
// `git rev-parse --local-env-vars`, plus GIT_NAMESPACE and the discovery
// variables.
// `git -C <dir>` overrides none of them.
var gitRepositoryEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_SHALLOW_FILE", "GIT_GRAFT_FILE",
	"GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX", "GIT_NAMESPACE",
	"GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
	"GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
}

// isolateGit unsets gitRepositoryEnv until the test ends. Every git process
// the test starts, its own or one the code under test starts, then acts on the
// repository in its working directory. A test run by `git bisect run` or a
// hook inherits GIT_DIR: without this, `git init` rewrites that repository.
func isolateGit(t *testing.T) {
	t.Helper()
	for _, key := range gitRepositoryEnv {
		t.Setenv(key, "") // restores the inherited value at cleanup
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

// gitCommand runs git in a scratch repository. The fixture that builds the
// repository calls isolateGit first.
func gitCommand(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}
