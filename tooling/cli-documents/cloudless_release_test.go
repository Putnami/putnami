package documents

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// This is the release-level non-regression gate for cloudless operation. The
// repository's own workspace installs @putnami/cloud, so it cannot prove that
// core workflows stay independent: a developer's artifact store, credentials,
// or discovery hooks could make an accidental dependency look healthy. This
// fixture starts with an empty store and a v3 lock containing no installed
// artifacts. Its local extensions model the released Go, TypeScript and SDD
// adapter boundary while the packages' own tests pin their real runtime startup
// behavior.
func TestCloudlessReleaseFixture_LocalDeveloperLoop(t *testing.T) {
	requireShell(t)
	wsRoot := writeCloudlessReleaseFixture(t)
	storeRoot := filepath.Join(t.TempDir(), "store")
	artifactRoot := filepath.Join(t.TempDir(), "artifacts")
	marker := filepath.Join(t.TempDir(), "jobs.log")
	configureCloudlessReleaseEnvironment(t, storeRoot, artifactRoot, marker)

	var networkRequests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		networkRequests.Add(1)
		http.Error(w, "outbound access is disabled in the cloudless fixture", http.StatusServiceUnavailable)
	}))
	t.Cleanup(endpoint.Close)
	t.Setenv("PUTNAMI_REGISTRY_URL", endpoint.URL)
	t.Chdir(wsRoot)

	app := &cli.App{}
	if output, code := runCloudlessCLI(t, app, context.Background(), "install"); code != cli.ExitSuccess {
		t.Fatalf("putnami install exit = %d\n%s", code, output)
	}
	assertCloudlessFixtureState(t, wsRoot, artifactRoot)
	if got := networkRequests.Load(); got != 0 {
		t.Fatalf("local install contacted the disabled registry %d times", got)
	}

	for _, project := range []string{"go-app", "ts-app"} {
		for _, command := range []string{"build", "test"} {
			if output, code := runCloudlessCLI(t, app, context.Background(), command, "--projects", project); code != cli.ExitSuccess {
				t.Fatalf("putnami %s %s exit = %d\n%s", command, project, code, output)
			}
		}
		// The second build is a local action-cache hit: the fixture script records
		// executions, so one line across two invocations proves no provider was
		// needed to reuse the result.
		if output, code := runCloudlessCLI(t, app, context.Background(), "build", "--projects", project); code != cli.ExitSuccess {
			t.Fatalf("second putnami build %s exit = %d\n%s", project, code, output)
		}
	}
	for _, language := range []string{"go", "typescript"} {
		if got := countCloudlessMarker(t, marker, language+":build"); got != 1 {
			t.Fatalf("%s build executed %d times across two local-cache runs, want 1", language, got)
		}
	}

	// Serve is resident by design. Each real task emits a v2 ready event and
	// writes its startup marker; cancellation then exercises the normal signal
	// path without leaving a listener or child process behind.
	runCloudlessServe(t, app, marker, "go-app", "go:serve")
	runCloudlessServe(t, app, marker, "ts-app", "typescript:serve")

	// A configured remote cache with no provider must emit exactly one notice,
	// keep the successful local path, and never contact the configured endpoint.
	t.Setenv("PUTNAMI_CACHE_URL", endpoint.URL)
	output, code := runCloudlessCLI(t, app, context.Background(), "build", "--projects", "go-app")
	if code != cli.ExitSuccess {
		t.Fatalf("configured-cache local fallback exit = %d\n%s", code, output)
	}
	if got := strings.Count(output, "remote cache"); got != 1 {
		t.Fatalf("configured cache emitted %d remote-cache notices, want exactly one\n%s", got, output)
	}
	if !strings.Contains(output, "building locally") {
		t.Fatalf("configured cache notice did not explain the local fallback:\n%s", output)
	}
	if got := networkRequests.Load(); got != 0 {
		t.Fatalf("configured cache without a provider contacted an endpoint %d times", got)
	}

	// MCP uses the same engine but deliberately has no remote authority. It must
	// complete against the local fixture and local cache under the same hostile
	// endpoint configuration.
	var result protocolcli.MCPResult
	callMCPRunJobs(t, wsRoot,
		runJobsArguments(t, []string{"build"}, []string{"go-app", "ts-app"}, false), &result)
	if result.Run == nil || result.Run.Outcome != protocolcli.RunOutcomeSuccess || result.Run.Counts.Failed != 0 {
		t.Fatalf("cloudless MCP run_jobs result = %+v", result.Run)
	}
	if got := networkRequests.Load(); got != 0 {
		t.Fatalf("local MCP run contacted an endpoint %d times", got)
	}
	assertCloudlessFixtureState(t, wsRoot, artifactRoot)
}

func TestCloudlessReleaseFixture_RemoteOnlyCommandsFailActionably(t *testing.T) {
	requireShell(t)
	wsRoot := writeCloudlessReleaseFixture(t)
	storeRoot := filepath.Join(t.TempDir(), "store")
	artifactRoot := filepath.Join(t.TempDir(), "artifacts")
	marker := filepath.Join(t.TempDir(), "jobs.log")
	configureCloudlessReleaseEnvironment(t, storeRoot, artifactRoot, marker)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")

	var networkRequests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		networkRequests.Add(1)
		http.Error(w, "outbound access is disabled in the cloudless fixture", http.StatusServiceUnavailable)
	}))
	t.Cleanup(endpoint.Close)
	t.Setenv("PUTNAMI_REGISTRY_URL", endpoint.URL)
	t.Setenv("PUTNAMI_CACHE_URL", endpoint.URL)

	// Explicit remote intent is represented by a declared-but-uninstalled cloud
	// extension. It is absent from the lock and artifact store, so the ordinary
	// missing-extension guard owns both publish and deploy failures.
	writeCloudlessFile(t, wsRoot, "putnami.workspace.json", `{
  "name": "cloudless-release",
  "includes": ["apps/go", "apps/typescript"],
  "extensions": ["/extensions/go", "/extensions/typescript", "@putnami/cloud"]
}`+"\n", 0o644)
	workspace.InvalidateLoadCache(wsRoot)
	lockBefore := readCloudlessFile(t, filepath.Join(wsRoot, "putnami.lock.json"))
	t.Chdir(wsRoot)

	app := &cli.App{}
	for _, command := range []string{"publish", "deploy"} {
		output, code := runCloudlessCLI(t, app, context.Background(), command, "--all")
		if code != cli.ExitError {
			t.Fatalf("putnami %s exit = %d, want %d\n%s", command, code, cli.ExitError, output)
		}
		for _, want := range []string{"@putnami/cloud", "run `putnami install`"} {
			if !strings.Contains(output, want) {
				t.Errorf("putnami %s output missing %q:\n%s", command, want, output)
			}
		}
		if got := strings.Count(output, "run `putnami install`"); got != 1 {
			t.Errorf("putnami %s emitted %d install hints, want exactly one:\n%s", command, got, output)
		}
		if strings.Contains(output, "putnami cloud ") || strings.Contains(output, "goroutine ") {
			t.Errorf("putnami %s used a post-install command or stack trace:\n%s", command, output)
		}
	}
	if got := networkRequests.Load(); got != 0 {
		t.Fatalf("missing-cloud negative controls contacted an endpoint %d times", got)
	}
	if lockAfter := readCloudlessFile(t, filepath.Join(wsRoot, "putnami.lock.json")); lockAfter != lockBefore {
		t.Fatal("a failed remote-only command mutated the cloudless lock")
	}
	assertArtifactPathsContainNoCloud(t, artifactRoot)
}

func TestCloudlessReleaseFixture_PublicManifestsHaveNoCloudDependency(t *testing.T) {
	t.Parallel()
	repoRoot := cloudlessRepoRoot(t)
	manifests := []string{filepath.Join(repoRoot, "tooling", "cli", "go.mod")}
	for _, shippedRoot := range []struct {
		path, filename string
	}{
		{filepath.Join(repoRoot, "go", "framework"), "go.mod"},
		{filepath.Join(repoRoot, "go", "extension"), "go.mod"},
		{filepath.Join(repoRoot, "typescript", "extension"), "go.mod"},
		{filepath.Join(repoRoot, "typescript", "framework"), "package.json"},
	} {
		manifests = append(manifests, findShippedManifests(t, shippedRoot.path, shippedRoot.filename)...)
	}
	for _, manifest := range manifests {
		assertManifestContainsNoCloud(t, manifest)
	}
}

func findShippedManifests(t *testing.T, root, filename string) []string {
	t.Helper()
	var manifests []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".putnami", "dist", "node_modules", "out", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == filename {
			manifests = append(manifests, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("discover %s files below %s: %v", filename, root, err)
	}
	if len(manifests) == 0 {
		t.Fatalf("discover %s files below %s: none found", filename, root)
	}
	sort.Strings(manifests)
	return manifests
}

func writeCloudlessReleaseFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeCloudlessFile(t, root, "putnami.workspace.json", `{
  "name": "cloudless-release",
  "includes": ["apps/go", "apps/typescript"],
  "extensions": ["/extensions/go", "/extensions/typescript", "/extensions/sdd"]
}`+"\n", 0o644)
	writeCloudlessFile(t, root, "putnami.lock.json", "{\n  \"version\": 3,\n  \"toolchains\": {},\n  \"extensions\": {},\n  \"templates\": {}\n}\n", 0o644)

	writeCloudlessFile(t, root, "apps/go/putnami.json",
		`{"name":"go-app","type":"application","extensions":["@putnami/go","@putnami/sdd"]}`+"\n", 0o644)
	writeCloudlessFile(t, root, "apps/go/go.mod", "module example.com/cloudless-go\n\ngo 1.25.7\n", 0o644)
	writeCloudlessFile(t, root, "apps/go/main.go", "package main\n\nfunc main() {}\n", 0o644)
	writeCloudlessFile(t, root, "apps/go/main_test.go", "package main\n\nimport \"testing\"\n\nfunc TestBoot(t *testing.T) {}\n", 0o644)

	writeCloudlessFile(t, root, "apps/typescript/putnami.json",
		`{"name":"ts-app","type":"application","extensions":["@putnami/typescript","@putnami/sdd"]}`+"\n", 0o644)
	writeCloudlessFile(t, root, "apps/typescript/package.json",
		`{"name":"cloudless-typescript","private":true,"dependencies":{"@putnami/runtime":"0.0.0"}}`+"\n", 0o644)
	writeCloudlessFile(t, root, "apps/typescript/src/main.ts", "export const booted = true;\n", 0o644)
	writeCloudlessFile(t, root, "apps/typescript/src/main.test.ts", "export const tested = true;\n", 0o644)

	for _, fixture := range []struct {
		path, name, language, activation, testActivation string
	}{
		{"extensions/go", "@putnami/go", "go", "go.mod", "**/*_test.go"},
		{"extensions/typescript", "@putnami/typescript", "typescript", "package.json", "**/*.test.ts"},
	} {
		writeCloudlessFile(t, root, fixture.path+"/putnami.json", fmt.Sprintf(`{"name":%q}`+"\n", fixture.name), 0o644)
		writeCloudlessFile(t, root, fixture.path+"/fixture.sh", cloudlessFixtureScript, 0o755)
		manifest := fmt.Sprintf(`{
  "name": %q,
  "version": "0.0.0-release-fixture",
  "cliContract": 4,
  "commands": {
    "workspace-install": {
      "description": "Install local dependencies without a provider.",
      "activation": "workspace",
      "run": [{"id":"install","task":"install"}]
    },
    "build": {
      "description": "Build the local fixture.",
      "activationFiles": [%q],
      "run": [{"id":"build","task":"build"}]
    },
    "lint": {
      "description": "Lint the local fixture.",
      "activationFiles": [%q],
      "run": [{"id":"lint","task":"lint"}]
    },
    "test": {
      "description": "Test the local fixture.",
      "activationFiles": [%q],
      "flags": {
        "enforce-coverage": {"type":"boolean","default":false,"description":"Validation cadence switch, declared so the documented contributor gate parses here exactly as it does on a language extension."}
      },
      "run": [{"id":"test","task":"test"}]
    },
    "serve": {
      "description": "Start the local fixture.",
      "activationFiles": [%q],
      "run": [{"id":"serve","task":"serve"}]
    }
  },
  "tasks": {
    "install": {"kind":"command","command":"{extensionRoot}/fixture.sh","args":[%q,"install"],"cache":false,"timeoutMs":10000},
    "build": {"kind":"command","command":"{extensionRoot}/fixture.sh","args":[%q,"build"],"cache":{"enabled":true,"noOutput":true},"timeoutMs":10000,"declares":{}},
    "lint": {"kind":"command","command":"{extensionRoot}/fixture.sh","args":[%q,"lint"],"cache":false,"timeoutMs":10000},
    "test": {"kind":"command","command":"{extensionRoot}/fixture.sh","args":[%q,"test"],"cache":false,"timeoutMs":10000},
    "serve": {"kind":"command","command":"{extensionRoot}/fixture.sh","args":[%q,"serve"],"cache":false,"timeoutMs":-1,"declares":{"effects":["process"]}}
  }
}
`, fixture.name, fixture.activation, fixture.activation, fixture.testActivation, fixture.activation,
			fixture.language, fixture.language, fixture.language, fixture.language, fixture.language)
		writeCloudlessFile(t, root, fixture.path+"/putnami.extension.json", manifest, 0o644)
	}
	writeCloudlessFile(t, root, "extensions/sdd/putnami.json", `{"name":"@putnami/sdd"}`+"\n", 0o644)
	writeCloudlessFile(t, root, "extensions/sdd/fixture.sh", cloudlessFixtureScript, 0o755)
	writeCloudlessFile(t, root, "extensions/sdd/putnami.extension.json", `{
  "name": "@putnami/sdd",
  "version": "0.0.0-release-fixture",
  "cliContract": 4,
  "commands": {
    "validate": {
      "description": "Validate the selected fixture project without a provider.",
      "activationFiles": ["putnami.json"],
      "alsoRuns": ["validate-workspace"],
      "run": [{"id":"validate","task":"validate"}]
    },
    "validate-workspace": {
      "description": "Validate the fixture workspace without a provider.",
      "activation": "workspace-once",
      "run": [{"id":"validate-workspace","task":"validate-workspace"}]
    }
  },
  "tasks": {
    "validate": {"kind":"command","command":"{extensionRoot}/fixture.sh","args":["sdd","validate"],"cache":false,"timeoutMs":10000},
    "validate-workspace": {"kind":"command","command":"{extensionRoot}/fixture.sh","args":["sdd","validate-workspace"],"cache":false,"timeoutMs":10000}
  }
}
`, 0o644)
	workspace.InvalidateLoadCache(root)
	return root
}

const cloudlessFixtureScript = `#!/bin/sh
set -eu

if [ -n "${PUTNAMI_CLOUD_TOKEN:-}" ] || [ -n "${CONFIG_SERVER_TOKEN:-}" ] || [ -n "${PUTNAMI_TOKEN:-}" ] || [ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ]; then
  printf '%s\n' '{"v":2,"type":"diagnostic","severity":"error","code":"CLOUDLESS001","message":"cloud credential leaked into release fixture"}'
  exit 1
fi

language="$1"
action="$2"
if [ "$action" = "serve" ]; then
  printf '%s\n' '{"v":2,"type":"ready","data":{"target":"server","endpoints":[{"scheme":"http","host":"127.0.0.1","port":0}]}}'
fi
printf '%s:%s\n' "$language" "$action" >> "$PUTNAMI_CLOUDLESS_MARKER"
if [ "$action" = "serve" ]; then
  while :; do sleep 1; done
fi
`

func configureCloudlessReleaseEnvironment(t *testing.T, storeRoot, artifactRoot, marker string) {
	t.Helper()
	hometest.Temp(t)
	t.Setenv("PUTNAMI_STORE_DIR", storeRoot)
	t.Setenv("PUTNAMI_ARTIFACT_DIR", artifactRoot)
	t.Setenv("PUTNAMI_CLOUDLESS_MARKER", marker)
	t.Setenv("PUTNAMI_NO_RELAUNCH", "1")
	t.Setenv("PUTNAMI_CACHE_URL", "")
	t.Setenv("PUTNAMI_CACHE_TOKEN", "")
	t.Setenv("PUTNAMI_CLOUD_TOKEN", "")
	t.Setenv("CONFIG_SERVER_URL", "")
	t.Setenv("CONFIG_SERVER_TOKEN", "")
	t.Setenv("PUTNAMI_TOKEN", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
}

func runCloudlessCLI(t *testing.T, app *cli.App, ctx context.Context, args ...string) (string, int) {
	t.Helper()
	code := cli.ExitError
	output := captureStdoutStderr(t, func() { code = app.Run(ctx, args) })
	return output, code
}

func runCloudlessServe(t *testing.T, app *cli.App, marker, project, wantMarker string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan bool, 1)
	go func() {
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-deadline.C:
				started <- false
				cancel()
				return
			case <-ticker.C:
				data, _ := os.ReadFile(marker)
				if strings.Contains(string(data), wantMarker) {
					started <- true
					cancel()
					return
				}
			}
		}
	}()
	output, code := runCloudlessCLI(t, app, ctx, "serve", "--projects", project)
	if !<-started {
		t.Fatalf("putnami serve %s never reached startup\n%s", project, output)
	}
	if code != cli.ExitSuccess && code != cli.ExitSignalReceived {
		t.Fatalf("canceled putnami serve %s exit = %d, want %d or %d\n%s",
			project, code, cli.ExitSuccess, cli.ExitSignalReceived, output)
	}
}

func assertCloudlessFixtureState(t *testing.T, wsRoot, artifactRoot string) {
	t.Helper()
	for _, rel := range []string{
		"putnami.workspace.json",
		"putnami.lock.json",
		"apps/go/go.mod",
		"apps/typescript/package.json",
		"AGENTS.md",
	} {
		contents := readCloudlessFile(t, filepath.Join(wsRoot, rel))
		if strings.Contains(contents, "@putnami/cloud") {
			t.Errorf("%s contains an unexpected cloud package reference", rel)
		}
	}

	ws, err := workspace.Load(wsRoot)
	if err != nil {
		t.Fatalf("load cloudless workspace: %v", err)
	}
	for _, project := range ws.Projects {
		if strings.Contains(project.Name, "cloud") {
			t.Errorf("project graph contains cloud project %q", project.Name)
		}
		for _, dependency := range project.Dependencies {
			if strings.Contains(dependency, "cloud") {
				t.Errorf("project %s has cloud dependency %q", project.Name, dependency)
			}
		}
	}
	projectPaths := make([]string, 0, len(ws.Projects))
	for _, project := range ws.Projects {
		projectPaths = append(projectPaths, project.Path)
	}
	extensions, err := extension.DiscoverExtensions(wsRoot, wsproto.Load(wsRoot), projectPaths)
	if err != nil {
		t.Fatalf("discover cloudless extensions: %v", err)
	}
	names := make([]string, 0, len(extensions))
	for _, ext := range extensions {
		names = append(names, ext.Name)
	}
	sort.Strings(names)
	want := []string{"@putnami/go", "@putnami/sdd", "@putnami/typescript"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("cloudless extension set = %v, want %v", names, want)
	}
	assertArtifactPathsContainNoCloud(t, artifactRoot)
}

func assertArtifactPathsContainNoCloud(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if strings.Contains(strings.ToLower(rel), "cloud") {
			t.Errorf("artifact store contains cloud path %s", rel)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk artifact store: %v", err)
	}
}

func countCloudlessMarker(t *testing.T, marker, want string) int {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line == want {
			count++
		}
	}
	return count
}

func writeCloudlessFile(t *testing.T, root, rel, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func readCloudlessFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func assertManifestContainsNoCloud(t *testing.T, path string) {
	t.Helper()
	contents := readCloudlessFile(t, path)
	if strings.Contains(contents, "@putnami/cloud") || strings.Contains(contents, "putnami"+"-cloud") {
		t.Errorf("public distribution manifest %s links a cloud package", path)
	}
	if strings.HasSuffix(path, "package.json") && !json.Valid([]byte(contents)) {
		t.Fatalf("parse %s: invalid JSON", path)
	}
}

func cloudlessRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "tooling", "cli", "go.mod")); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root not found")
		}
		dir = parent
	}
}
