package nxdelegation

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// localHits reads the local cache hits a run's summary reports.
func localHits(t *testing.T, out string) int {
	t.Helper()
	m := regexp.MustCompile(`Cache: (\d+) local hit`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the run reports no cache summary:\n%s", out)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func TestADelegatedNxWorkspaceImportsPricingAndPassesLintTestBuild(t *testing.T) {
	spectest.Proves(t, "cli/init-from-a-monorepo-tool", "delegate-import-runs-one-project-on-putnami", "a-delegated-nx-workspace-imports-pricing-and-passes-lint-test-build")
	clitest.RequireShell(t)
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skipf("the import installs with bun, which is not on PATH: %v", err)
	}
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	demo := readDemo(t)
	root := nxWorkspace(t, demo, buildRuntime(t))
	log := stubNx(t, demo)
	clitest.LinkSourceExtensions(t, repoRoot, root)

	// A delegated workspace that loads @putnami/typescript and @putnami/dev
	// from this tree. pricing's scripts run what @putnami/typescript runs.
	// pricing is private, so the import names its sources as its entry and
	// its Bun build moves: a published package keeps the dist/index.js its
	// main names, which that build does not write, and its build on Nx.
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name": "shop", "extensions": ["/nx-extension", "/.ext/typescript", "/.ext/dev"], "includes": [], "options": {"@putnami/nx": {"mode": "delegate"}}}`)
	clitest.WriteFile(t, filepath.Join(root, "packages/pricing/package.json"), `{
  "name": "@demo/pricing",
  "version": "1.0.0",
  "private": true,
  "main": "dist/index.js",
  "types": "dist/index.d.ts",
  "scripts": {
    "build": "bun build ./src/index.ts --outdir dist --target node",
    "test": "bun test"
  },
  "dependencies": {
    "decimal.js": "10.4.3"
  }
}
`)
	// Its tests import the runner's globals from bun:test, as a move to bun
	// test asks.
	spec := filepath.Join(root, "packages/pricing/src/index.spec.ts")
	source, err := os.ReadFile(spec)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, spec, "import { describe, expect, it } from 'bun:test';\n"+string(source))
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

	// init --import previews the list and the moved targets, and writes
	// nothing; --apply records it and synchronizes.
	code, out := clitest.RunGateArgs(t, root, "init", "--from", "nx", "--import", "/packages/pricing")
	if code != 0 || !strings.Contains(out, `options["@putnami/nx"].import + /packages/pricing`) || !strings.Contains(out, "+++ b/packages/pricing/putnami.json") || !strings.Contains(out, "Nothing was written") {
		t.Fatalf("init --import preview exit %d:\n%s", code, out)
	}
	if code, out := clitest.RunGateArgs(t, root, "init", "--from", "nx", "--import", "/packages/pricing", "--apply"); code != 0 {
		t.Fatalf("init --import --apply exit %d:\n%s", code, out)
	}
	disabled := func(dir string) []string {
		var doc struct {
			Disable struct {
				Jobs []string `json:"jobs"`
			} `json:"disable"`
		}
		data, err := os.ReadFile(filepath.Join(root, dir, "putnami.json"))
		if err != nil || json.Unmarshal(data, &doc) != nil {
			t.Fatalf("read %s/putnami.json: %v", dir, err)
		}
		return doc.Disable.Jobs
	}
	if got := disabled("packages/pricing"); !slices.Contains(got, "@putnami/nx:build") || !slices.Contains(got, "@putnami/nx:test") {
		t.Errorf("pricing disables %v, want its Nx build and test", got)
	}
	if got := disabled("apps/api"); !slices.Contains(got, "@putnami/typescript:build") || !slices.Contains(got, "@putnami/typescript:test") {
		t.Errorf("api disables %v, want its TypeScript build and test", got)
	}

	clitest.PinToolchains(t, repoRoot, root, "bun", "go")
	if code, out := clitest.RunGateArgs(t, root, "install"); code != 0 {
		t.Fatalf("install exit %d:\n%s", code, out)
	}
	clitest.PinToolchains(t, repoRoot, root, "go")
	_ = os.Remove(log)
	code, out = clitest.RunGateArgs(t, root, "lint,test,build", "--all")
	if code != 0 {
		t.Fatalf("lint,test,build exit %d:\n%s", code, out)
	}
	// api ran through nx run; pricing ran no nx.
	logged, _ := os.ReadFile(log)
	if !strings.Contains(string(logged), "nx run @demo/api:test") || !strings.Contains(string(logged), "nx run @demo/api:build") {
		t.Errorf("api ran no nx run:\n%s", logged)
	}
	if strings.Contains(string(logged), "@demo/pricing:") {
		t.Errorf("pricing ran through nx:\n%s", logged)
	}
	for _, task := range []string{"@demo/pricing test", "@demo/pricing build"} {
		if !strings.Contains(out, task) {
			t.Errorf("no %s ran:\n%s", task, out)
		}
	}
	sessions, _ := filepath.Glob(filepath.Join(root, ".putnami", "sessions", "*", "events.jsonl"))
	delegated := 0
	for _, file := range sessions {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		delegated += strings.Count(string(data), `"delegatedTo":"nx"`)
	}
	if delegated == 0 {
		t.Errorf("the sessions record no result delegated to nx: %v", sessions)
	}

	// pricing's tasks are in Putnami's cache: a second run restores them,
	// and runs api through nx again, which Nx's cache answers.
	code, out = clitest.RunGateArgs(t, root, "lint,test,build", "--all")
	if code != 0 || localHits(t, out) == 0 || !strings.Contains(out, "@demo/pricing test~test cached") || !strings.Contains(out, "@demo/pricing build~types cached") {
		t.Errorf("the second run exit %d restored pricing from Putnami's cache:\n%s", code, out)
	}
	if strings.Contains(out, "@demo/api test~test cached") {
		t.Errorf("a delegated api task came from Putnami's cache:\n%s", out)
	}
}
