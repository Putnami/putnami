package turbodelegation

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

func TestADelegatedTurborepoImportsPricingAndPassesLintTestBuild(t *testing.T) {
	spectest.Proves(t, feature, "delegate-import-runs-one-project-on-putnami", "a-delegated-turborepo-imports-pricing-and-passes-lint-test-build")
	clitest.RequireShell(t)
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skipf("the import installs with bun, which is not on PATH: %v", err)
	}
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	demo := readDemo(t)
	root := turboWorkspace(t, demo, buildRuntime(t), "delegate")
	log := stubTurbo(t)
	clitest.LinkSourceExtensions(t, repoRoot, root)

	// A delegated workspace that loads @putnami/typescript and @putnami/dev
	// from this tree. pricing's scripts run what @putnami/typescript runs,
	// and its tests import the runner's globals from bun:test. pricing is
	// private, so the import names its sources as its entry and its Bun
	// build moves: a published package keeps the dist/index.js its main
	// names, which that build does not write, and its build on Turborepo.
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name": "shop", "extensions": ["/turbo-extension", "/.ext/typescript", "/.ext/dev"], "includes": [], "options": {"@putnami/turbo": {"mode": "delegate"}}}`)
	clitest.WriteFile(t, filepath.Join(root, "packages/pricing/package.json"), `{
  "name": "@demo/pricing",
  "version": "1.0.0",
  "private": true,
  "type": "module",
  "main": "dist/index.js",
  "types": "dist/index.d.ts",
  "scripts": {
    "build": "bun build ./src/index.ts --outdir dist --target node",
    "test": "bun test"
  },
  "dependencies": {
    "zod": "3.23.8"
  }
}
`)
	spec := filepath.Join(root, "packages/pricing/src/index.test.ts")
	source, err := os.ReadFile(spec)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, spec, strings.Replace(string(source), "from 'vitest'", "from 'bun:test'", 1))
	var pkg map[string]json.RawMessage
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil || json.Unmarshal(data, &pkg) != nil {
		t.Fatalf("read the root package.json: %v", err)
	}
	// Bun installs, and an empty workspaces list, which the install fills in:
	// an adopted repository that keeps no list gains none.
	pkg["packageManager"] = json.RawMessage(`"bun@1.4.0"`)
	pkg["workspaces"] = json.RawMessage(`[]`)
	data, _ = json.MarshalIndent(pkg, "", "  ")
	clitest.WriteFile(t, filepath.Join(root, "package.json"), string(data)+"\n")
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}

	// init --import previews the list and the moved tasks, and writes
	// nothing; --apply records it and synchronizes.
	before, _ := os.ReadFile(filepath.Join(root, "packages/pricing/putnami.json"))
	code, out := clitest.RunGateArgs(t, root, "init", "--from", "turbo", "--import", "/packages/pricing")
	if code != 0 || !strings.Contains(out, `options["@putnami/turbo"].import + /packages/pricing`) || !strings.Contains(out, "+++ b/packages/pricing/putnami.json") || !strings.Contains(out, "Nothing was written") {
		t.Fatalf("init --import preview exit %d:\n%s", code, out)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "packages/pricing/putnami.json")); string(after) != string(before) {
		t.Errorf("a preview wrote packages/pricing/putnami.json")
	}
	if code, out := clitest.RunGateArgs(t, root, "init", "--from", "turbo", "--import", "/packages/pricing", "--apply"); code != 0 {
		t.Fatalf("init --import --apply exit %d:\n%s", code, out)
	}
	if doc := readProject(t, root, "packages/pricing"); !slices.Equal(doc.Extensions, []string{"@putnami/turbo", "@putnami/typescript"}) || !slices.Contains(doc.Disable.Jobs, "@putnami/turbo:test") || !slices.Contains(doc.Disable.Jobs, "@putnami/turbo:build") {
		t.Errorf("pricing putnami.json = %+v, want both extensions and its Turborepo build and test disabled", doc)
	}
	if doc := readProject(t, root, "apps/api"); !slices.Equal(doc.Extensions, []string{"@putnami/turbo"}) {
		t.Errorf("api putnami.json = %+v, want Turborepo alone", doc)
	}

	clitest.PinToolchains(t, repoRoot, root, "bun", "go")
	if code, out := clitest.RunGateArgs(t, root, "install"); code != 0 {
		t.Fatalf("install exit %d:\n%s", code, out)
	}
	clitest.PinToolchains(t, repoRoot, root, "go")
	// The install put the repository's turbo in node_modules/.bin, which a
	// delegated task runs first: the stub takes its place.
	stubRepositoryTurbo(t, root, log)
	_ = os.Remove(log)
	code, out = clitest.RunGateArgs(t, root, "lint,test,build", "--all")
	if code != 0 {
		t.Fatalf("lint,test,build exit %d:\n%s", code, out)
	}
	// api ran through turbo run; pricing ran no turbo.
	logged, _ := os.ReadFile(log)
	if !strings.Contains(string(logged), "turbo run test --filter=@demo/api") || !strings.Contains(string(logged), "turbo run build --filter=@demo/api") {
		t.Errorf("api ran no turbo run:\n%s", logged)
	}
	if strings.Contains(string(logged), "--filter=@demo/pricing") {
		t.Errorf("pricing ran through turbo:\n%s", logged)
	}
	for _, task := range []string{"@demo/pricing test~test", "@demo/pricing build~types"} {
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
		delegated += strings.Count(string(data), `"delegatedTo":"turbo"`)
	}
	if delegated == 0 {
		t.Errorf("the sessions record no result delegated to turbo: %v", sessions)
	}

	// pricing's tasks are in Putnami's cache: a second run restores them,
	// and runs api through turbo again, which Turborepo's cache answers.
	code, out = clitest.RunGateArgs(t, root, "lint,test,build", "--all")
	if code != 0 || localHits(t, out) == 0 || !strings.Contains(out, "@demo/pricing test~test cached") || !strings.Contains(out, "@demo/pricing build~types cached") {
		t.Errorf("the second run exit %d restored pricing from Putnami's cache:\n%s", code, out)
	}
	if strings.Contains(out, "@demo/api test~test cached") {
		t.Errorf("a delegated api task came from Putnami's cache:\n%s", out)
	}
}

func TestAnImportThatMovesNoTaskLeavesTheLockfileAsItWas(t *testing.T) {
	spectest.Proves(t, feature, "init-installs-only-what-moves", "an-import-that-moves-no-task-leaves-the-lockfile-as-it-was")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	demo := readDemo(t)
	root := turboWorkspace(t, demo, buildRuntime(t), "delegate")
	stubTurbo(t)
	// Every script runs a tool @putnami/typescript does not run: no task
	// moves to it.
	clitest.WriteFile(t, filepath.Join(root, "packages/pricing/package.json"),
		`{"name": "@demo/pricing", "version": "1.0.0", "main": "dist/index.js", "scripts": {"build": "webpack", "test": "mocha"}, "dependencies": {"zod": "3.23.8"}}`+"\n")
	clitest.WriteFile(t, filepath.Join(root, "apps/api/package.json"),
		`{"name": "@demo/api", "version": "1.0.0", "private": true, "main": "dist/server.js", "scripts": {"build": "webpack", "test": "mocha"}, "dependencies": {"@demo/pricing": "workspace:*"}}`+"\n")
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	kept := []string{"pnpm-lock.yaml", "package.json", "packages/pricing/package.json", "apps/api/package.json"}
	before := map[string][]byte{}
	for _, rel := range kept {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		before[rel] = data
	}

	code, out := clitest.RunGateArgs(t, root, "init", "--from", "turbo", "--mode", "import", "--apply")
	if code != 0 {
		t.Fatalf("init --mode import --apply exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"No task moves to a Putnami extension: every task keeps running through turbo, and no language extension was installed.",
		"putnami init --from turbo --mode delegate gives the same result.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("init prints %q:\n%s", want, out)
		}
	}
	for _, rel := range kept {
		if after, _ := os.ReadFile(filepath.Join(root, rel)); string(after) != string(before[rel]) {
			t.Errorf("the import changed %s:\n%s", rel, after)
		}
	}
	workspace, _ := os.ReadFile(filepath.Join(root, "putnami.workspace.json"))
	if strings.Contains(string(workspace), "@putnami/typescript") || !strings.Contains(string(workspace), `"mode": "import"`) {
		t.Errorf("the workspace config declares @putnami/typescript or records no import:\n%s", workspace)
	}
	for _, dir := range []string{"packages/pricing", "apps/api"} {
		if doc := readProject(t, root, dir); !slices.Equal(doc.Extensions, []string{"@putnami/turbo"}) {
			t.Errorf("%s putnami.json = %+v, want Turborepo alone", dir, doc)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "node_modules")); !os.IsNotExist(err) {
		t.Errorf("the import installed the repository's packages: %v", err)
	}
}
