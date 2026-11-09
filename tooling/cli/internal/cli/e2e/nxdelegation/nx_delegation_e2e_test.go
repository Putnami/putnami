// Package nxdelegation drives a Nx repository through the real CLI with
// @putnami/nx: the extension's runtime is built from this tree, the
// repository is the Nx demo of putnami observe, and npx is a stub on PATH
// that answers for nx and writes the reports the demo's CI wrote. These tests
// swap the process streams and the working directory, so they run in their
// own test binary.
package nxdelegation

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
	os.Exit(clitest.Main(m))
}

// repoRoot is the root of this repository, from this package's directory.
const repoRoot = "../../../../../.."

// readDemo reads the Nx demo of putnami observe by section: base/ is the
// repository, head/ the change on top of it, ci/ what its CI wrote.
func readDemo(t *testing.T) map[string]map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "tooling/observe/internal/observe/testdata/demos/nx.txtar"))
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

// buildRuntime builds the @putnami/nx runtime from this tree, the way its
// prepare script does.
func buildRuntime(t *testing.T) string {
	t.Helper()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the Nx runtime is built with go, which is not on PATH: %v", err)
	}
	out := filepath.Join(t.TempDir(), "putnami-nx")
	cmd := exec.Command(goBinary, "build", "-o", out, "./cmd/putnami-nx")
	cmd.Dir = filepath.Join(repoRoot, "monorepo/nx")
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "CGO_ENABLED=0")
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the Nx runtime: %v\n%s", err, data)
	}
	return out
}

// nxWorkspace writes the demo's base as a workspace that loads @putnami/nx
// from a local copy of its manifest, whose prepare copies the runtime built
// from this tree.
func nxWorkspace(t *testing.T, demo map[string]map[string]string, runtime string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range demo["base"] {
		clitest.WriteFile(t, filepath.Join(root, rel), content)
	}
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n.gen\ndist\nnode_modules\nreports\n")
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name": "shop", "extensions": ["/nx-extension"], "includes": []}`)

	data, err := os.ReadFile(filepath.Join(repoRoot, "monorepo/nx/putnami.extension.json"))
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
	// The copy needs no Go toolchain lock and no @putnami/dev: its prepare
	// copies the runtime this test built.
	delete(manifest, "extensionDependencies")
	delete(rt, "toolchains")
	rt["prepare"] = json.RawMessage(`{"command": "{extensionRoot}/bin/prepare", "args": ["--output", "{runtimeOutput}"], "inputs": ["bin/prepare"]}`)
	if manifest["runtime"], err = json.Marshal(rt); err != nil {
		t.Fatal(err)
	}
	data, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	ext := filepath.Join(root, "nx-extension")
	clitest.WriteFile(t, filepath.Join(ext, "putnami.extension.json"), string(data))
	prepare := filepath.Join(ext, "bin", "prepare")
	clitest.WriteFile(t, prepare, "#!/bin/sh\nset -e\nmkdir -p \"$2/compiled\"\ncp '"+runtime+"' \"$2/compiled/putnami-nx\"\n")
	if err := os.Chmod(prepare, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// stubNx puts first on PATH an npx that answers npx --no nx --version with
// the demo's Nx version and, for npx --no nx run <project>:test, writes the
// demo's reports of that project. It logs each call with its working
// directory and returns the log.
func stubNx(t *testing.T, demo map[string]map[string]string) string {
	t.Helper()
	bin, ci := t.TempDir(), t.TempDir()
	for rel, content := range demo["ci"] {
		clitest.WriteFile(t, filepath.Join(ci, rel), content)
	}
	log := filepath.Join(t.TempDir(), "npx.log")
	clitest.WriteFile(t, filepath.Join(bin, "npx"), `#!/bin/sh
set -e
echo "$(pwd) npx $*" >> '`+log+`'
case "$3" in
  --version) printf 'Nx Version:\n- Local: v21.0.0\n'; exit 0;;
  run)
    case "$4" in
      @demo/api:test) dir=apps/api;;
      @demo/pricing:test) dir=packages/pricing;;
      *) exit 0;;
    esac
    mkdir -p "$dir/reports/coverage"
    cp '`+ci+`'/"$dir"/reports/junit.xml "$dir/reports/junit.xml"
    cp '`+ci+`'/"$dir"/reports/coverage/cobertura-coverage.xml "$dir/reports/coverage/cobertura-coverage.xml";;
esac
`)
	if err := os.Chmod(filepath.Join(bin, "npx"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// nxTests runs putnami test --impacted from base and returns the projects nx
// run tested, sorted, and the run's output. Every nx run call runs at the
// workspace root.
func nxTests(t *testing.T, root, base, log string) ([]string, string) {
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
		dir, call, _ := strings.Cut(line, " npx ")
		target, ok := strings.CutPrefix(call, "--no nx run ")
		if !ok {
			continue
		}
		if real, _ := filepath.EvalSymlinks(dir); real != rootReal {
			t.Errorf("nx run ran in %s, want the workspace root", dir)
		}
		target, _, _ = strings.Cut(target, " ")
		tested = append(tested, target)
	}
	slices.Sort(tested)
	// Every task of the run is uncached: no lookup, so no hit and no miss,
	// and nothing is stored.
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

func TestAChangeToOneNxProjectRunsItAndItsDependentsThroughNx(t *testing.T) {
	spectest.Proves(t, "cli/init-from-a-monorepo-tool", "nx-impact-runs-through-nx", "a-change-to-one-nx-project-runs-it-and-its-dependents-through-nx-uncached")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	demo := readDemo(t)
	root := nxWorkspace(t, demo, buildRuntime(t))
	log := stubNx(t, demo)

	// A rerun of init in the workspace previews the putnami.json files the
	// sync would write and the target it maps to no Putnami task, and
	// writes nothing.
	code, out := clitest.RunGateArgs(t, root, "init", "--from", "nx", "--mode", "delegate")
	if code != 0 || !strings.Contains(out, "Nothing was written") || !strings.Contains(out, "+++ b/apps/api/putnami.json") || !strings.Contains(out, "@demo/api:start no Putnami task runs it") {
		t.Fatalf("init preview exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "apps/api/putnami.json")); !os.IsNotExist(err) {
		t.Fatalf("the preview wrote apps/api/putnami.json: %v", err)
	}

	// --apply records the mode and writes each project's edges.
	if code, out := clitest.RunGateArgs(t, root, "init", "--from", "nx", "--mode", "delegate", "--apply"); code != 0 {
		t.Fatalf("init --apply exit %d:\n%s", code, out)
	}
	var workspace struct {
		Options map[string]struct {
			Mode string `json:"mode"`
		} `json:"options"`
	}
	data, err := os.ReadFile(filepath.Join(root, "putnami.workspace.json"))
	if err != nil || json.Unmarshal(data, &workspace) != nil || workspace.Options["@putnami/nx"].Mode != "delegate" {
		t.Fatalf("the workspace records no delegate mode: %v\n%s", err, data)
	}
	var api struct {
		Extensions   []string `json:"extensions"`
		Dependencies []string `json:"dependencies"`
	}
	data, err = os.ReadFile(filepath.Join(root, "apps/api/putnami.json"))
	if err != nil || json.Unmarshal(data, &api) != nil {
		t.Fatalf("the sync wrote no putnami.json for api: %v\n%s", err, data)
	}
	if !slices.Equal(api.Extensions, []string{"@putnami/nx"}) || !slices.Equal(api.Dependencies, []string{"/packages/pricing"}) {
		t.Errorf("api putnami.json = %s", data)
	}

	// A rerun shows the diff of a file that drifted, and writes nothing.
	synced := string(data)
	drifted := strings.Replace(synced, `"/packages/pricing"`, `"/elsewhere"`, 1)
	clitest.WriteFile(t, filepath.Join(root, "apps/api/putnami.json"), drifted)
	code, out = clitest.RunGateArgs(t, root, "init", "--from", "nx")
	if code != 0 || !strings.Contains(out, "+++ b/apps/api/putnami.json") || !strings.Contains(out, `+    "/packages/pricing"`) || strings.Contains(out, "1) import") {
		t.Errorf("init preview of a drifted file exit %d:\n%s", code, out)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "apps/api/putnami.json")); string(data) != drifted {
		t.Errorf("the preview wrote %s", data)
	}
	clitest.WriteFile(t, filepath.Join(root, "apps/api/putnami.json"), synced)
	clitest.InitGitRepo(t, root)
	base := strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))

	// A change to pricing runs pricing and api, which depends on it.
	for rel, content := range demo["head"] {
		if strings.HasPrefix(rel, "packages/pricing/") {
			clitest.WriteFile(t, filepath.Join(root, rel), content)
		}
	}
	pricingChange := commit(t, root, "change pricing")
	tested, out := nxTests(t, root, base, log)
	if want := []string{"@demo/api:test", "@demo/pricing:test"}; !slices.Equal(tested, want) {
		t.Errorf("nx run tested %v, want %v: the changed project and its dependent\n%s", tested, want, out)
	}
	// The session records each test task's result as delegated to nx.
	sessions, _ := filepath.Glob(filepath.Join(root, ".putnami", "sessions", "*", "events.jsonl"))
	delegated := 0
	for _, file := range sessions {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		delegated += strings.Count(string(data), `"delegatedTo":"nx"`)
	}
	if delegated < 2 {
		t.Errorf("the sessions record %d results delegated to nx, want the 2 test tasks: %v", delegated, sessions)
	}

	// A change to api runs api alone: pricing, its dependency, runs no nx.
	quote := filepath.Join(root, "apps/api/src/quote.ts")
	data, err = os.ReadFile(quote)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, quote, string(data)+"\n// a change to api alone\n")
	commit(t, root, "change api")
	tested, out = nxTests(t, root, pricingChange, log)
	if want := []string{"@demo/api:test"}; !slices.Equal(tested, want) {
		t.Errorf("nx run tested %v, want %v: a dependency runs no nx\n%s", tested, want, out)
	}

	// api stops depending on pricing with no change to a putnami.json:
	// validate fails until the sync records it.
	manifest := filepath.Join(root, "apps/api/package.json")
	data, err = os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, manifest, strings.Replace(string(data), `"@demo/pricing": "1.0.0"`, `"decimal.js": "10.4.3"`, 1))
	code, out = clitest.RunGateArgs(t, root, "validate", "--all")
	if code == 0 || !strings.Contains(out, "nx.sync_stale") {
		t.Errorf("validate exit %d on a stale graph:\n%s", code, out)
	}
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	if code, out := clitest.RunGateArgs(t, root, "validate", "--all"); code != 0 {
		t.Errorf("validate exit %d after the sync:\n%s", code, out)
	}
}
