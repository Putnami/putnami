// Package turbodelegation drives a Turborepo repository through the real CLI
// with @putnami/turbo: the extension's runtime is built from this tree, the
// repository is the Turborepo demo of putnami observe, and turbo is a stub on
// PATH that logs each run. These tests swap the process streams and the
// working directory, so they run in their own test binary.
package turbodelegation

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	abs, err := filepath.Abs("../../../../../..")
	if err != nil {
		panic(err)
	}
	repoRoot = abs
	os.Exit(clitest.Main(m))
}

// repoRoot is the root of this repository, absolute, since the CLI runs
// change the working directory.
var repoRoot string

const feature = "cli/init-from-a-monorepo-tool"

// readDemo reads the Turborepo demo of putnami observe by section: base/ is
// the repository, head/ the change on top of it, ci/ what its CI wrote.
func readDemo(t *testing.T) map[string]map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "tooling/observe/internal/observe/testdata/demos/turbo.txtar"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]string{}
	var name string
	var body strings.Builder
	flush := func() {
		if name == "" {
			return
		}
		prefix, rel, _ := strings.Cut(name, "/")
		if out[prefix] == nil {
			out[prefix] = map[string]string{}
		}
		out[prefix][rel] = body.String()
	}
	for _, line := range strings.SplitAfter(string(data), "\n") {
		trimmed := strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(trimmed, "-- ") && strings.HasSuffix(trimmed, " --") {
			flush()
			name, body = strings.TrimSuffix(strings.TrimPrefix(trimmed, "-- "), " --"), strings.Builder{}
			continue
		}
		if name != "" {
			body.WriteString(line)
		}
	}
	flush()
	return out
}

// buildRuntime builds the @putnami/turbo runtime from this tree, the way its
// prepare script does.
func buildRuntime(t *testing.T) string {
	t.Helper()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the Turborepo runtime is built with go, which is not on PATH: %v", err)
	}
	out := filepath.Join(t.TempDir(), "putnami-turbo")
	cmd := exec.Command(goBinary, "build", "-o", out, "./cmd/putnami-turbo")
	cmd.Dir = filepath.Join(repoRoot, "integrations/turbo")
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "CGO_ENABLED=0")
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the Turborepo runtime: %v\n%s", err, data)
	}
	return out
}

// turboWorkspace writes the demo's base, with a turbo.json that also declares
// a dev task, as the workspace putnami init --from turbo writes in mode. It
// loads @putnami/turbo from a local copy of its manifest, whose prepare
// copies the runtime built from this tree.
func turboWorkspace(t *testing.T, demo map[string]map[string]string, runtime, mode string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range demo["base"] {
		clitest.WriteFile(t, filepath.Join(root, rel), content)
	}
	clitest.WriteFile(t, filepath.Join(root, "turbo.json"), `{
  "$schema": "https://turborepo.com/schema.json",
  "tasks": {
    "build": {"dependsOn": ["^build"], "outputs": ["dist/**"]},
    "test": {"dependsOn": ["^build"], "outputs": ["reports/**"]},
    "dev": {"cache": false, "persistent": true}
  }
}
`)
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n.gen\ndist\nnode_modules\n")
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name": "shop", "extensions": ["/turbo-extension"], "includes": [], "options": {"@putnami/turbo": {"mode": "`+mode+`"}}}`)

	data, err := os.ReadFile(filepath.Join(repoRoot, "integrations/turbo/putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	// The copy needs no Go toolchain lock and no @putnami/dev: its prepare
	// copies the runtime this test built.
	delete(manifest, "extensionDependencies")
	var rt map[string]json.RawMessage
	if err := json.Unmarshal(manifest["runtime"], &rt); err != nil {
		t.Fatal(err)
	}
	delete(rt, "toolchains")
	rt["prepare"] = json.RawMessage(`{"command": "{extensionRoot}/bin/prepare", "args": ["--output", "{runtimeOutput}"], "inputs": ["bin/prepare"]}`)
	if manifest["runtime"], err = json.Marshal(rt); err != nil {
		t.Fatal(err)
	}
	data, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	ext := filepath.Join(root, "turbo-extension")
	clitest.WriteFile(t, filepath.Join(ext, "putnami.extension.json"), string(data))
	prepare := filepath.Join(ext, "bin", "prepare")
	clitest.WriteFile(t, prepare, "#!/bin/sh\nset -e\nmkdir -p \"$2/compiled\"\ncp '"+runtime+"' \"$2/compiled/putnami-turbo\"\n")
	if err := os.Chmod(prepare, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// stubTurbo puts first on PATH a turbo that prints the version the demo's
// root package.json allows, and logs each call with its working directory.
// It returns the log.
func stubTurbo(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "turbo.log")
	clitest.WriteFile(t, filepath.Join(bin, "turbo"), `#!/bin/sh
set -e
echo "$(pwd) turbo $*" >> '`+log+`'
case "$1" in --version) echo 2.11.10;; esac
`)
	if err := os.Chmod(filepath.Join(bin, "turbo"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// turboTests runs putnami test --impacted from base and returns the packages
// turbo run test ran on, sorted, and the run's output. Every turbo run call
// runs at the workspace root, and no Putnami cache entry is looked up or
// stored.
func turboTests(t *testing.T, root, base, log string) ([]string, string) {
	t.Helper()
	_ = os.Remove(log)
	code, out := clitest.RunGateArgs(t, root, "test", "--impacted", "--baseline", base)
	if code != 0 {
		t.Fatalf("test --impacted exit %d:\n%s", code, out)
	}
	logged, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	rootReal, _ := filepath.EvalSymlinks(root)
	var tested []string
	for _, line := range strings.Split(strings.TrimSpace(string(logged)), "\n") {
		dir, call, _ := strings.Cut(line, " turbo ")
		pkg, ok := strings.CutPrefix(call, "run test --filter=")
		if !ok {
			continue
		}
		if real, _ := filepath.EvalSymlinks(dir); real != rootReal {
			t.Errorf("turbo run test ran in %s, want the workspace root", dir)
		}
		tested = append(tested, pkg)
	}
	slices.Sort(tested)
	if !strings.Contains(out, "Cache: 0 local hit · 0 miss") {
		t.Errorf("the run looked up or stored Putnami cache entries for delegated tasks:\n%s", out)
	}
	return tested, out
}

func commit(t *testing.T, root, message string) string {
	t.Helper()
	clitest.RunGit(t, root, "add", "-A")
	clitest.RunGit(t, root, "commit", "-m", message)
	return strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))
}

func readProject(t *testing.T, root, dir string) (doc struct {
	Extensions   []string `json:"extensions"`
	Dependencies []string `json:"dependencies"`
	Disable      struct {
		Jobs []string `json:"jobs"`
	} `json:"disable"`
}) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, dir, "putnami.json"))
	if err != nil || json.Unmarshal(data, &doc) != nil {
		t.Fatalf("the sync wrote no putnami.json for %s: %v\n%s", dir, err, data)
	}
	return doc
}

func TestAChangeToOneTurboPackageRunsItAndItsDependentsThroughTurbo(t *testing.T) {
	spectest.Proves(t, feature, "turbo-delegate-impact", "a-change-to-one-turbo-package-runs-it-and-its-dependents-through-turbo-uncached")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	demo := readDemo(t)
	root := turboWorkspace(t, demo, buildRuntime(t), "delegate")
	log := stubTurbo(t)

	code, out := clitest.RunGateArgs(t, root, "projects", "sync")
	if code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	if api := readProject(t, root, "apps/api"); !slices.Equal(api.Extensions, []string{"@putnami/turbo"}) || !slices.Equal(api.Dependencies, []string{"/packages/pricing"}) {
		t.Errorf("api putnami.json = %+v", api)
	}
	clitest.InitGitRepo(t, root)
	base := strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))

	// A change to pricing runs pricing and api, which depends on it.
	for rel, content := range demo["head"] {
		if strings.HasPrefix(rel, "packages/pricing/") {
			clitest.WriteFile(t, filepath.Join(root, rel), content)
		}
	}
	pricingChange := commit(t, root, "change pricing")
	tested, out := turboTests(t, root, base, log)
	if want := []string{"@demo/api", "@demo/pricing"}; !slices.Equal(tested, want) {
		t.Errorf("turbo run test ran on %v, want %v: the changed package and its dependent\n%s", tested, want, out)
	}

	// A change to api runs api alone: pricing, its dependency, runs no turbo.
	quote := filepath.Join(root, "apps/api/src/quote.ts")
	data, err := os.ReadFile(quote)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, quote, string(data)+"\n// a change to api alone\n")
	apiChange := commit(t, root, "change api")
	tested, out = turboTests(t, root, pricingChange, log)
	if want := []string{"@demo/api"}; !slices.Equal(tested, want) {
		t.Errorf("turbo run test ran on %v, want %v: a dependency runs no turbo\n%s", tested, want, out)
	}

	// A change to the root lockfile, which Turborepo hashes into every
	// package, runs every package.
	clitest.WriteFile(t, filepath.Join(root, "pnpm-lock.yaml"), demo["head"]["pnpm-lock.yaml"])
	commit(t, root, "bump zod in the lockfile")
	tested, out = turboTests(t, root, apiChange, log)
	if want := []string{"@demo/api", "@demo/pricing"}; !slices.Equal(tested, want) {
		t.Errorf("a lockfile change ran turbo run test on %v, want %v\n%s", tested, want, out)
	}

	// A workspace dependency api drops makes validate fail until the sync
	// records it.
	clitest.WriteFile(t, filepath.Join(root, "apps/api/package.json"), `{"name": "@demo/api", "private": true, "scripts": {"build": "tsc", "test": "vitest run"}}`)
	if code, out := clitest.RunGateArgs(t, root, "validate", "--all"); code == 0 || !strings.Contains(out, "turbo.sync_stale") {
		t.Errorf("validate exit %d on a stale graph:\n%s", code, out)
	}
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	if code, out := clitest.RunGateArgs(t, root, "validate", "--all"); code != 0 {
		t.Errorf("validate exit %d after the sync:\n%s", code, out)
	}
}

func TestAnImportedTurborepoMovesOnlyTheTasksTypeScriptRunsAndListsTheOthers(t *testing.T) {
	spectest.Proves(t, feature, "turbo-import-graph", "an-imported-turborepo-maps-typescript-packages-and-lists-unmapped-tasks")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	demo := readDemo(t)
	root := turboWorkspace(t, demo, buildRuntime(t), "delegate")
	stubTurbo(t)
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	clitest.InitGitRepo(t, root)

	// The rerun of init in import mode previews the diff and writes nothing.
	before, _ := os.ReadFile(filepath.Join(root, "apps/api/putnami.json"))
	code, out := clitest.RunGateArgs(t, root, "init", "--from", "turbo", "--mode", "import")
	if code != 0 {
		t.Fatalf("init rerun exit %d:\n%s", code, out)
	}
	for _, want := range []string{`options["@putnami/turbo"].mode delegate → import`, "--- a/apps/api/putnami.json", `+    "@putnami/typescript"`, "Nothing was written"} {
		if !strings.Contains(out, want) {
			t.Errorf("the rerun prints %q:\n%s", want, out)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(root, "apps/api/putnami.json")); string(after) != string(before) {
		t.Errorf("a rerun without --apply wrote apps/api/putnami.json")
	}

	// The import, as init --apply writes it: pricing's bun test and api's
	// vitest test scripts already run a runner @putnami/typescript runs, and
	// api's build script runs tsc alone, so it takes those tasks over; the
	// tsup bundle stays on Turborepo and is listed.
	clitest.WriteFile(t, filepath.Join(root, "packages/pricing/package.json"),
		`{"name": "@demo/pricing", "version": "1.0.0", "type": "module", "main": "dist/index.js", "scripts": {"build": "tsup src/index.ts", "test": "bun test"}, "dependencies": {"zod": "3.23.8"}}`)
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name": "shop", "extensions": ["/turbo-extension"], "includes": [], "options": {"@putnami/turbo": {"mode": "import"}}}`)
	code, out = clitest.RunGateArgs(t, root, "projects", "sync")
	if code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"dev: no Putnami task maps it",
		`@demo/pricing#build: @putnami/typescript runs bun build or tsc alone, not "tsup src/index.ts"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the sync lists %q:\n%s", want, out)
		}
	}
	for _, task := range []string{"@demo/pricing#test", "@demo/api#test"} {
		if strings.Contains(out, task) {
			t.Errorf("%s moved to @putnami/typescript, and is not listed:\n%s", task, out)
		}
	}
	for dir, c := range map[string]struct {
		deps     []string
		disabled []string
	}{
		"apps/api":         {[]string{"/packages/pricing"}, []string{"lint", "@putnami/turbo:test", "@putnami/turbo:build"}},
		"packages/pricing": {nil, []string{"lint", "@putnami/turbo:test", "@putnami/typescript:build"}},
	} {
		doc := readProject(t, root, dir)
		if !slices.Equal(doc.Extensions, []string{"@putnami/turbo", "@putnami/typescript"}) || !slices.Equal(doc.Dependencies, c.deps) || !slices.Equal(doc.Disable.Jobs, c.disabled) {
			t.Errorf("%s putnami.json = %+v, want both extensions, %v and disabled %v", dir, doc, c.deps, c.disabled)
		}
	}
}
