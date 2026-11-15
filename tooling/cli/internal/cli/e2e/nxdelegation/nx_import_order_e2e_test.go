package nxdelegation

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// taskEnd is what the latest session of the workspace at root recorded at the
// end of the task whose key is key: its input digest and its reuse.
func taskEnd(t *testing.T, root, key string) (digest, reuse string) {
	t.Helper()
	file, err := os.Open(filepath.Join(root, ".putnami", "sessions", clitest.LatestSessionID(t, root), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for scanner.Scan() {
		var record struct {
			Record   string `json:"record"`
			Identity struct {
				Key string `json:"key"`
			} `json:"identity"`
			Task struct {
				InputDigest string `json:"inputDigest"`
				Reuse       string `json:"reuse"`
			} `json:"task"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) != nil || record.Record != "task:end" || record.Identity.Key != key {
			continue
		}
		return record.Task.InputDigest, record.Task.Reuse
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("the latest session records no end of %s", key)
	return "", ""
}

// The demo's nx.json gives test dependsOn ["^build"]. With the api imported
// and pricing delegated, the api's test moves to @putnami/typescript and
// keeps that order: it waits for nx run @demo/pricing:build, and an edit to
// pricing's sources moves its key, though that build stores no Putnami cache
// entry.
func TestAnImportedNxTestWaitsForTheNxBuildAndFoldsItsKey(t *testing.T) {
	spectest.Proves(t, "cli/init-from-a-monorepo-tool", "nx-import-keeps-the-build-order", "an-imported-nx-test-waits-for-the-nx-build-and-folds-its-key")
	clitest.RequireShell(t)
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skipf("the imported test runs with bun, which is not on PATH: %v", err)
	}
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	demo := readDemo(t)
	root := nxWorkspace(t, demo, buildRuntime(t))
	log := stubNx(t, demo)
	clitest.LinkSourceExtensions(t, repoRoot, root)

	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name": "shop", "extensions": ["/nx-extension", "/.ext/typescript", "/.ext/dev"], "includes": [], "options": {"@putnami/nx": {"mode": "delegate", "import": ["/apps/api"]}}}`)
	// The api's test runs bun test, which moves; its webpack build stays on
	// Nx. Its spec reads no pricing output, which the stub nx does not build.
	clitest.WriteFile(t, filepath.Join(root, "apps/api/package.json"), `{
  "name": "@demo/api",
  "version": "1.0.0",
  "private": true,
  "scripts": {
    "build": "webpack",
    "test": "bun test"
  },
  "dependencies": {
    "@demo/pricing": "1.0.0"
  }
}
`)
	clitest.WriteFile(t, filepath.Join(root, "apps/api/src/quote.spec.ts"),
		"import { expect, it } from 'bun:test';\n\nit('adds', () => {\n  expect(1 + 1).toBe(2);\n});\n")
	var pkg map[string]json.RawMessage
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil || json.Unmarshal(data, &pkg) != nil {
		t.Fatalf("read the root package.json: %v", err)
	}
	pkg["packageManager"] = json.RawMessage(`"bun@1.4.0"`)
	data, _ = json.MarshalIndent(pkg, "", "  ")
	clitest.WriteFile(t, filepath.Join(root, "package.json"), string(data)+"\n")
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}

	var api struct {
		Commands map[string]struct {
			DependsOn []string `json:"dependsOn"`
		} `json:"commands"`
	}
	data, err = os.ReadFile(filepath.Join(root, "apps/api/putnami.json"))
	if err != nil || json.Unmarshal(data, &api) != nil {
		t.Fatalf("read apps/api/putnami.json: %v", err)
	}
	if got := api.Commands["test"].DependsOn; !reflect.DeepEqual(got, []string{"^build"}) {
		t.Fatalf("api test dependsOn = %v, want [^build]:\n%s", got, data)
	}

	clitest.PinToolchains(t, repoRoot, root, "bun", "go")
	if code, out := clitest.RunGateArgs(t, root, "install"); code != 0 {
		t.Fatalf("install exit %d:\n%s", code, out)
	}
	clitest.PinToolchains(t, repoRoot, root, "go")
	clitest.InitGitRepo(t, root)

	// The plan orders the api's moved test, which it alone selects, after
	// pricing's Nx build: a job of its test command waits for it.
	code, out := clitest.RunGateArgs(t, root, "test", "--projects", "/apps/api", "--dry-run")
	if code != 0 {
		t.Fatalf("test --dry-run exit %d:\n%s", code, out)
	}
	waits := false
	for _, line := range strings.Split(out, "\n") {
		job := strings.TrimSpace(line)
		if strings.HasPrefix(job, "test~") && strings.Contains(job, "[deps: ") && strings.Contains(job, "/packages/pricing:build~build") {
			waits = true
		}
	}
	if !waits {
		t.Fatalf("the api's test does not wait for pricing's build:\n%s", out)
	}

	_ = os.Remove(log)
	if code, out := clitest.RunGateArgs(t, root, "test", "--projects", "/apps/api"); code != 0 {
		t.Fatalf("test exit %d:\n%s", code, out)
	}
	if logged, _ := os.ReadFile(log); !strings.Contains(string(logged), "nx run @demo/pricing:build") {
		t.Errorf("pricing's build ran no nx run:\n%s", logged)
	}
	before, _ := taskEnd(t, root, "/apps/api:test~test")
	if before == "" {
		t.Fatal("the api's test recorded no key")
	}
	if digest, reuse := taskEnd(t, root, "/packages/pricing:build~build"); digest != "" || reuse != "none" {
		t.Errorf("pricing's Nx build recorded key %q and reuse %q, want no key, executed", digest, reuse)
	}

	// An edit to pricing's sources moves the key of the api's test.
	source := filepath.Join(root, "packages/pricing/src/index.ts")
	data, err = os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, source, string(data)+"\nexport const edited = true;\n")
	if code, out := clitest.RunGateArgs(t, root, "test", "--projects", "/apps/api"); code != 0 {
		t.Fatalf("test after the edit exit %d:\n%s", code, out)
	}
	after, reuse := taskEnd(t, root, "/apps/api:test~test")
	if after == before || reuse != "none" {
		t.Errorf("after an edit to pricing, the api's test key %s (reuse %q), want one other than %s, executed", after, reuse, before)
	}
}
