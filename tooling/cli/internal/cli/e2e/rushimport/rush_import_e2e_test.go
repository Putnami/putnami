// Package rushimport drives a Rush repository adopted in import mode through
// the real CLI: @putnami/rush, with its runtime built from this tree, writes
// each Rush project as a TypeScript project, and @putnami/typescript and
// @putnami/dev, loaded from this tree, install, lint, test and build them.
// rush is a stub on PATH that fails on any call, so no task reaches Rush.
// These tests swap the process streams and the working directory, so they
// run in their own test binary.
package rushimport

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

// commandLine declares a bulk test command and a phased deploy command.
const commandLine = `{
  "commands": [
    { "commandKind": "bulk", "name": "test", "summary": "Test" },
    { "commandKind": "phased", "name": "deploy", "summary": "Deploy", "phases": ["_phase:deploy"] }
  ]
}
`

// readBase reads the base section of the Rush demo of putnami observe.
func readBase(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "tooling/observe/internal/observe/testdata/demos/rush.txtar"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	var name string
	var body strings.Builder
	flush := func() {
		if rel, ok := strings.CutPrefix(name, "base/"); ok {
			out[rel] = body.String()
		}
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

// buildRuntime builds the @putnami/rush runtime from this tree.
func buildRuntime(t *testing.T) string {
	t.Helper()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the Rush runtime is built with go, which is not on PATH: %v", err)
	}
	out := filepath.Join(t.TempDir(), "putnami-rush")
	cmd := exec.Command(goBinary, "build", "-o", out, "./cmd/putnami-rush")
	cmd.Dir = filepath.Join(repoRoot, "integrations/rush")
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "CGO_ENABLED=0")
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the Rush runtime: %v\n%s", err, data)
	}
	return out
}

// importWorkspace writes the demo's base as a workspace adopted in import
// mode. It loads @putnami/rush from a copy of its manifest whose prepare
// copies the runtime built from this tree, and @putnami/typescript and
// @putnami/dev from this tree through links under .ext, which the project
// scan never enters.
func importWorkspace(t *testing.T, runtime string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range readBase(t) {
		clitest.WriteFile(t, filepath.Join(root, rel), content)
	}
	clitest.WriteFile(t, filepath.Join(root, "common/config/rush/command-line.json"), commandLine)
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), "common/temp\nnode_modules\n.putnami\n.gen\ndist\n")
	// The root package.json declares the Bun release this repository pins,
	// whose digests the extension ships, so the install reads no release
	// metadata from GitHub, and an empty workspaces list, which the install
	// fills in: an adopted repository that keeps no list gains none.
	clitest.WriteFile(t, filepath.Join(root, "package.json"), `{"name": "shop", "private": true, "packageManager": "bun@1.4.0", "workspaces": []}`+"\n")
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name": "shop", "extensions": ["/.ext/rush", "/.ext/typescript", "/.ext/dev"], "includes": [], "options": {"@putnami/rush": {"mode": "import"}}}`)

	for name, rel := range map[string]string{"typescript": "typescript/extension", "dev": "tooling/dev"} {
		target, err := filepath.Abs(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, ".ext"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, ".ext", name)); err != nil {
			t.Fatal(err)
		}
	}

	data, err := os.ReadFile(filepath.Join(repoRoot, "integrations/rush/putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest, rt map[string]json.RawMessage
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(manifest["runtime"], &rt); err != nil {
		t.Fatal(err)
	}
	delete(manifest, "extensionDependencies")
	delete(rt, "toolchains")
	rt["prepare"] = json.RawMessage(`{"command": "{extensionRoot}/bin/prepare", "args": ["--output", "{runtimeOutput}"], "inputs": ["bin/prepare"]}`)
	if manifest["runtime"], err = json.Marshal(rt); err != nil {
		t.Fatal(err)
	}
	if data, err = json.MarshalIndent(manifest, "", "  "); err != nil {
		t.Fatal(err)
	}
	ext := filepath.Join(root, ".ext", "rush")
	clitest.WriteFile(t, filepath.Join(ext, "putnami.extension.json"), string(data))
	prepare := filepath.Join(ext, "bin", "prepare")
	clitest.WriteFile(t, prepare, "#!/bin/sh\nset -e\nmkdir -p \"$2/compiled\"\ncp '"+runtime+"' \"$2/compiled/putnami-rush\"\n")
	if err := os.Chmod(prepare, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// pinToolchains copies this repository's pins of the toolchains into the
// workspace lock, which it starts when there is none: the extensions loaded
// from this tree compile their runtimes with the Go release it pins, which a
// published extension does not need, and the Bun pin keeps install offline.
func pinToolchains(t *testing.T, root string, names ...string) {
	t.Helper()
	var source, lock map[string]json.RawMessage
	data, err := os.ReadFile(filepath.Join(repoRoot, "putnami.lock.json"))
	if err != nil || json.Unmarshal(data, &source) != nil {
		t.Fatalf("read this repository's lock: %v", err)
	}
	lock = map[string]json.RawMessage{"version": source["version"]}
	if data, err := os.ReadFile(filepath.Join(root, "putnami.lock.json")); err == nil {
		if err := json.Unmarshal(data, &lock); err != nil {
			t.Fatal(err)
		}
	}
	var from, into map[string]json.RawMessage
	if err := json.Unmarshal(source["toolchains"], &from); err != nil {
		t.Fatal(err)
	}
	if raw, ok := lock["toolchains"]; ok {
		if err := json.Unmarshal(raw, &into); err != nil {
			t.Fatal(err)
		}
	}
	if into == nil {
		into = map[string]json.RawMessage{}
	}
	for _, name := range names {
		into[name] = from[name]
	}
	if lock["toolchains"], err = json.Marshal(into); err != nil {
		t.Fatal(err)
	}
	if data, err = json.MarshalIndent(lock, "", "  "); err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(root, "putnami.lock.json"), string(data)+"\n")
}

func TestAnImportedRushRepositoryPassesLintTestBuildWithTheGraphObserveReads(t *testing.T) {
	spectest.Proves(t, "cli/init-from-a-monorepo-tool", "rush-import-runs-through-putnami", "an-imported-rush-repository-passes-lint-test-build-with-the-graph-observe-reads")
	clitest.RequireShell(t)
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skipf("the import installs with bun, which is not on PATH: %v", err)
	}
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	root := importWorkspace(t, buildRuntime(t))

	// api builds with Bun and tests with Mocha, which @putnami/typescript
	// does not run: its test keeps running with rushx (D-070). pricing builds
	// with Bun, and web lints with Biome, which only checks: the repository
	// holds no Biome configuration, and web's files follow Biome's defaults.
	setScripts(t, root, "apps/api", `"build": "bun build src/index.ts --outdir lib", "test": "mocha"`)
	setScripts(t, root, "libs/pricing", `"build": "bun build src/index.ts --outdir lib"`)
	clitest.WriteFile(t, filepath.Join(root, "apps/web/package.json"), webManifest)

	// A rush on PATH that answers the version check and fails any other call,
	// and a rushx that logs: an import runs Rush on no task, and rushx on the
	// script it keeps.
	bin := t.TempDir()
	rushxLog := filepath.Join(t.TempDir(), "rushx.log")
	clitest.WriteFile(t, filepath.Join(bin, "rush"), "#!/bin/sh\nif [ \"$1\" = \"--help\" ]; then echo 'Rush Multi-Project Build Tool 5.140.0 - https://rushjs.io'; exit 0; fi\necho \"rush $*\" >&2\nexit 9\n")
	clitest.WriteFile(t, filepath.Join(bin, "rushx"), "#!/bin/sh\necho \"$(basename \"$(pwd)\") rushx $*\" >> '"+rushxLog+"'\n")
	for _, name := range []string{"rush", "rushx"} {
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	code, out := clitest.RunGateArgs(t, root, "projects", "sync")
	if code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "not mapped: rush deploy") || !strings.Contains(out, "not mapped: the test script of @demo/api") {
		t.Errorf("the sync lists no unmapped command or script:\n%s", out)
	}
	// Each project is a TypeScript project with the edges observe reads:
	// api depends on pricing, web and pricing on nothing. api also names the
	// Rush extension, which runs the script it keeps.
	for dir, want := range map[string]struct{ extensions, deps []string }{
		"apps/api":     {[]string{"@putnami/rush", "@putnami/typescript"}, []string{"/libs/pricing"}},
		"apps/web":     {[]string{"@putnami/typescript"}, nil},
		"libs/pricing": {[]string{"@putnami/typescript"}, nil},
	} {
		var project struct {
			Extensions   []string `json:"extensions"`
			Dependencies []string `json:"dependencies"`
		}
		data, err := os.ReadFile(filepath.Join(root, dir, "putnami.json"))
		if err != nil || json.Unmarshal(data, &project) != nil {
			t.Fatalf("the sync wrote no putnami.json for %s: %v\n%s", dir, err, data)
		}
		if !slices.Equal(project.Extensions, want.extensions) || !slices.Equal(project.Dependencies, want.deps) {
			t.Errorf("%s putnami.json = %s, want %v and %v", dir, data, want.extensions, want.deps)
		}
	}
	if code, out := clitest.RunGateArgs(t, root, "validate", "--all"); code != 0 {
		t.Errorf("validate after the sync exit %d:\n%s", code, out)
	}

	// Putnami installs: the root package.json lists the projects.
	pinToolchains(t, root, "bun", "go")
	if code, out := clitest.RunGateArgs(t, root, "install"); code != 0 {
		t.Fatalf("install exit %d:\n%s", code, out)
	}
	var pkg struct {
		Workspaces []string `json:"workspaces"`
	}
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil || json.Unmarshal(data, &pkg) != nil || !slices.Equal(pkg.Workspaces, []string{"apps/api", "apps/web", "libs/pricing"}) {
		t.Fatalf("root package.json = %s, %v; want the projects as workspaces", data, err)
	}

	// web's files after the sync and the install, before any lint.
	web := map[string]string{}
	for _, rel := range []string{"package.json", "putnami.json", "tsconfig.json", "src/index.ts"} {
		data, err := os.ReadFile(filepath.Join(root, "apps/web", rel))
		if err != nil {
			t.Fatal(err)
		}
		web[rel] = string(data)
	}

	pinToolchains(t, root, "go")
	code, out = clitest.RunGateArgs(t, root, "lint,test,build", "--all")
	if code != 0 {
		t.Fatalf("lint,test,build exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "rush ") && strings.Contains(out, "exit status 9") {
		t.Errorf("a task ran rush:\n%s", out)
	}
	// Putnami's TypeScript tasks ran on each project; api's type check
	// resolved pricing from its sources.
	for _, task := range []string{"@demo/api build~types", "@demo/pricing build~types", "@demo/web lint~check-only"} {
		if !strings.Contains(out, task) {
			t.Errorf("no %s ran:\n%s", task, out)
		}
	}
	// web's lint only checked, as its script did, with Biome's defaults: it
	// passed on the files the import wrote and rewrote none.
	if data, err := os.ReadFile(filepath.Join(root, "biome.json")); err != nil || string(data) != "{}\n" {
		t.Errorf("root biome.json = %q, %v; want one that sets nothing", data, err)
	}
	for rel, want := range web {
		if data, err := os.ReadFile(filepath.Join(root, "apps/web", rel)); err != nil || string(data) != want {
			t.Errorf("the lint rewrote apps/web/%s: %q, %v", rel, data, err)
		}
	}
	// A file that breaks Biome's defaults fails the lint, and keeps its bytes.
	const unformatted = "export const title = 'Demo'\n"
	clitest.WriteFile(t, filepath.Join(root, "apps/web/src/index.ts"), unformatted)
	if code, out := clitest.RunGateArgs(t, root, "lint", "--all"); code == 0 || !strings.Contains(out, "apps/web/src/index.ts") {
		t.Errorf("lint on an unformatted file exit %d:\n%s", code, out)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "apps/web/src/index.ts")); string(data) != unformatted {
		t.Errorf("the lint rewrote an unformatted file: %q", data)
	}
	clitest.WriteFile(t, filepath.Join(root, "apps/web/src/index.ts"), web["src/index.ts"])
	if data, err := os.ReadFile(filepath.Join(root, "libs/pricing/package.json")); err != nil || !strings.Contains(string(data), `"exports": {`) {
		t.Errorf("pricing gained no source exports: %s %v", data, err)
	}
	// api's Mocha test ran with rushx in its directory, its script as the
	// repository wrote it.
	if data, err := os.ReadFile(rushxLog); err != nil || strings.TrimSpace(string(data)) != "api rushx test" {
		t.Errorf("rushx ran %q (%v), want api's test\n%s", data, err, out)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "apps/api/package.json")); !strings.Contains(string(data), `"test": "mocha"`) {
		t.Errorf("the import rewrote api's scripts: %s", data)
	}

	// A script that changes the tool it runs leaves the graph stale: validate
	// fails in import mode too.
	setScripts(t, root, "apps/web", `"lint": "biome check .", "test": "mocha"`)
	if code, out := clitest.RunGateArgs(t, root, "validate", "--all"); code == 0 || !strings.Contains(out, "rush.sync_stale") {
		t.Errorf("validate on a stale graph exit %d:\n%s", code, out)
	}
}

// webManifest is web's package.json as Biome's defaults format it, with a
// lint script that only checks.
const webManifest = "{\n\t\"name\": \"@demo/web\",\n\t\"version\": \"1.0.0\",\n\t\"private\": true,\n\t\"scripts\": {\n\t\t\"lint\": \"biome check .\"\n\t},\n\t\"devDependencies\": {\n\t\t\"typescript\": \"~5.6.0\"\n\t}\n}\n"

// setScripts gives the package.json of the project at dir the scripts, a
// JSON object's members, after its name.
func setScripts(t *testing.T, root, dir, scripts string) {
	t.Helper()
	file := filepath.Join(root, dir, "package.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["scripts"] = json.RawMessage("{" + scripts + "}")
	if data, err = json.MarshalIndent(manifest, "", "  "); err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, file, string(data)+"\n")
}

// A project whose scripts all move to @putnami/typescript no longer names
// @putnami/rush, yet the sync writes the edges Rush reads, a devDependency
// no source imports among them. Rush's probe attributes those edges to its
// graph without claiming the project, so deps prune leaves them alone and
// the next sync finds nothing to write back.
func TestAnEdgeTheRushSyncWritesIsNoPhantom(t *testing.T) {
	spectest.Proves(t, "cli/init-from-a-monorepo-tool", "rush-import-keeps-the-edges-the-sync-writes", "an-edge-the-rush-sync-writes-is-no-phantom")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	root := importWorkspace(t, buildRuntime(t))
	// web lints with Biome, which @putnami/typescript runs, and builds with a
	// devDependency on pricing that no source of web imports, as a shared
	// build tool or configuration package is.
	setScripts(t, root, "apps/web", `"lint": "biome check ."`)
	file := filepath.Join(root, "apps/web/package.json")
	clitest.WriteFile(t, file, `{
  "name": "@demo/web",
  "version": "1.0.0",
  "private": true,
  "scripts": {"lint": "biome check ."},
  "devDependencies": {"@demo/pricing": "workspace:*", "typescript": "~5.6.0"}
}
`)

	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	web := filepath.Join(root, "apps/web/putnami.json")
	synced, err := os.ReadFile(web)
	if err != nil || !strings.Contains(string(synced), `"/libs/pricing"`) || strings.Contains(string(synced), "@putnami/rush") {
		t.Fatalf("web putnami.json = %s (%v), want the edge to pricing and no @putnami/rush", synced, err)
	}

	// prune reads the provider view, which a run that loads the workspace
	// records after the sync's writes.
	if code, out := clitest.RunGateArgs(t, root, "validate", "--all"); code != 0 {
		t.Fatalf("validate exit %d:\n%s", code, out)
	}
	code, out := clitest.RunGateArgs(t, root, "deps", "prune", "--dry-run")
	if code != 0 || !strings.Contains(out, "No declared dependency edge without an import.") {
		t.Errorf("deps prune --dry-run exit %d lists an edge the sync writes:\n%s", code, out)
	}
	if code, out := clitest.RunGateArgs(t, root, "deps", "prune"); code != 0 {
		t.Fatalf("deps prune exit %d:\n%s", code, out)
	}
	if data, _ := os.ReadFile(web); string(data) != string(synced) {
		t.Errorf("deps prune rewrote web putnami.json:\n%s", data)
	}
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	if data, _ := os.ReadFile(web); string(data) != string(synced) {
		t.Errorf("the next sync rewrote web putnami.json:\n%s", data)
	}
}

// A change that adds an edge between two imported projects selects them
// against its baseline, and validate fails: the putnami.json files no longer
// carry the edges a sync writes.
func TestValidateAgainstABaselineFailsOnANewEdgeInAnImportedRushRepository(t *testing.T) {
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	root := importWorkspace(t, buildRuntime(t))
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	commit := func(message string) {
		clitest.RunGit(t, root, "add", "-A")
		clitest.RunGit(t, root, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", message)
	}
	clitest.RunGit(t, root, "init", "-q")
	commit("import")

	// web, which depends on nothing, gains pricing.
	file := filepath.Join(root, "apps/web/package.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["dependencies"] = json.RawMessage(`{"@demo/pricing": "workspace:*"}`)
	if data, err = json.MarshalIndent(manifest, "", "  "); err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, file, string(data)+"\n")
	commit("web depends on pricing")

	code, out := clitest.RunGateArgs(t, root, "validate", "--baseline", "HEAD~1")
	if code == 0 || !strings.Contains(out, "rush.sync_stale") || !strings.Contains(out, "apps/web/putnami.json no longer matches") {
		t.Fatalf("validate --baseline HEAD~1 exit %d, want rush.sync_stale on web:\n%s", code, out)
	}
}
