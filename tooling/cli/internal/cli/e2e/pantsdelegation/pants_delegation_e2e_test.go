// Package pantsdelegation drives a Pants repository through the real CLI with
// @putnami/pants: the extension's runtime is built from this tree, the
// repository is the Pants demo of putnami observe, and pants is a stub on
// PATH that writes the reports the demo's CI wrote. These tests swap the
// process streams and the working directory, so they run in their own test
// binary.
package pantsdelegation

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

// readDemo reads the Pants demo of putnami observe by section: base/ is the
// repository, head/ the change on top of it, ci/ what its CI wrote.
func readDemo(t *testing.T) map[string]map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "tooling/observe/internal/observe/testdata/demos/pants.txtar"))
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

// buildRuntime builds the @putnami/pants runtime from this tree, the way its
// prepare script does.
func buildRuntime(t *testing.T) string {
	t.Helper()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the Pants runtime is built with go, which is not on PATH: %v", err)
	}
	out := filepath.Join(t.TempDir(), "putnami-pants")
	cmd := exec.Command(goBinary, "build", "-o", out, "./cmd/putnami-pants")
	cmd.Dir = filepath.Join(repoRoot, "monorepo/pants")
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "CGO_ENABLED=0")
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the Pants runtime: %v\n%s", err, data)
	}
	return out
}

// pantsWorkspace writes the demo's base as a workspace that loads
// @putnami/pants from a local copy of its manifest, whose prepare copies the
// runtime built from this tree.
func pantsWorkspace(t *testing.T, demo map[string]map[string]string, runtime string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range demo["base"] {
		clitest.WriteFile(t, filepath.Join(root, rel), content)
	}
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n.gen\ndist\n")
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name": "shop", "extensions": ["/pants-extension"], "includes": []}`)

	data, err := os.ReadFile(filepath.Join(repoRoot, "monorepo/pants/putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	// The copy needs no Go toolchain lock and no @putnami/dev: its prepare
	// copies the runtime this test built.
	delete(manifest, "extensionDependencies")
	rt := manifest["runtime"].(map[string]any)
	delete(rt, "toolchains")
	rt["prepare"] = map[string]any{"command": "{extensionRoot}/bin/prepare", "args": []any{"--output", "{runtimeOutput}"}, "inputs": []any{"bin/prepare"}}
	data, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	ext := filepath.Join(root, "pants-extension")
	clitest.WriteFile(t, filepath.Join(ext, "putnami.extension.json"), string(data))
	prepare := filepath.Join(ext, "bin", "prepare")
	clitest.WriteFile(t, prepare, "#!/bin/sh\nset -e\nmkdir -p \"$2/compiled\"\ncp '"+runtime+"' \"$2/compiled/putnami-pants\"\n")
	if err := os.Chmod(prepare, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// stubPants puts first on PATH a pants that prints the demo's version, prints
// its export for peek ::, and for test <dir>: writes the demo's JUnit reports
// of that directory and its coverage report under dist/. It logs each call
// with its working directory and returns the log.
func stubPants(t *testing.T, demo map[string]map[string]string) string {
	t.Helper()
	bin, ci := t.TempDir(), t.TempDir()
	for rel, content := range demo["ci"] {
		clitest.WriteFile(t, filepath.Join(ci, rel), content)
	}
	log := filepath.Join(t.TempDir(), "pants.log")
	clitest.WriteFile(t, filepath.Join(bin, "pants"), `#!/bin/sh
set -e
echo "$(pwd) pants $*" >> '`+log+`'
case "$1" in --version) echo 2.25.0; exit 0;; esac
for a in "$@"; do last="$a"; done
case " $* " in
  *" peek :: "*) cat '`+ci+`/pants-graph.json';;
  *" test "*)
    prefix="$(echo "${last%:}" | tr / .)."
    mkdir -p dist/test/reports dist/coverage/python
    for f in '`+ci+`'/dist/test/reports/"$prefix"*; do [ -e "$f" ] && cp "$f" dist/test/reports/; done
    cp '`+ci+`/dist/coverage/python/coverage.xml' dist/coverage/python/coverage.xml;;
esac
`)
	if err := os.Chmod(filepath.Join(bin, "pants"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// pantsTests runs putnami test --impacted from base and returns the packages
// pants test ran on, sorted, and the run's output. Every pants test call runs
// at the workspace root.
func pantsTests(t *testing.T, root, base, log string) ([]string, string) {
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
		dir, call, _ := strings.Cut(line, " pants ")
		if !strings.Contains(call, " test ") {
			continue
		}
		if real, _ := filepath.EvalSymlinks(dir); real != rootReal {
			t.Errorf("pants test ran in %s, want the workspace root", dir)
		}
		tested = append(tested, call[strings.LastIndex(call, " ")+1:])
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

func TestAChangeToOnePantsPackageRunsItAndItsDependentsThroughPants(t *testing.T) {
	spectest.Proves(t, "cli/init-from-a-monorepo-tool", "pants-impact-runs-through-pants", "a-change-to-one-pants-package-runs-it-and-its-dependents-through-pants-uncached")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	demo := readDemo(t)
	root := pantsWorkspace(t, demo, buildRuntime(t))
	log := stubPants(t, demo)

	// The sync records the packages and writes each one's edges from
	// pants peek ::.
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	var api struct {
		Extensions   []string `json:"extensions"`
		Dependencies []string `json:"dependencies"`
	}
	data, err := os.ReadFile(filepath.Join(root, "src/python/api/putnami.json"))
	if err != nil || json.Unmarshal(data, &api) != nil {
		t.Fatalf("the sync wrote no putnami.json for api: %v\n%s", err, data)
	}
	if !slices.Equal(api.Extensions, []string{"@putnami/pants"}) || !slices.Equal(api.Dependencies, []string{"/3rdparty/python", "/src/python/pricing"}) {
		t.Errorf("api putnami.json = %s", data)
	}
	clitest.InitGitRepo(t, root)
	base := strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))

	// A change to pricing runs pricing and api, which depends on it.
	for rel, content := range demo["head"] {
		if strings.HasPrefix(rel, "src/python/pricing/") {
			clitest.WriteFile(t, filepath.Join(root, rel), content)
		}
	}
	pricingChange := commit(t, root, "change pricing")
	tested, out := pantsTests(t, root, base, log)
	if want := []string{"src/python/api:", "src/python/pricing:"}; !slices.Equal(tested, want) {
		t.Errorf("pants test ran on %v, want %v: the changed package and its dependent\n%s", tested, want, out)
	}
	if strings.Contains(out, "src/python/web test~test") {
		t.Errorf("web, which nothing in the change reaches, ran:\n%s", out)
	}

	// A change to api runs api alone: pricing, its dependency, runs no pants.
	main := filepath.Join(root, "src/python/api/main.py")
	data, err = os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, main, string(data)+"\n# a change to api alone\n")
	apiChange := commit(t, root, "change api")
	tested, out = pantsTests(t, root, pricingChange, log)
	if want := []string{"src/python/api:"}; !slices.Equal(tested, want) {
		t.Errorf("pants test ran on %v, want %v: a dependency runs no pants\n%s", tested, want, out)
	}

	// A change to the requirements and the resolve lockfile runs every
	// package that depends on them.
	for rel, content := range demo["head"] {
		if strings.HasPrefix(rel, "3rdparty/") {
			clitest.WriteFile(t, filepath.Join(root, rel), content)
		}
	}
	commit(t, root, "bump fastapi")
	tested, out = pantsTests(t, root, apiChange, log)
	if want := []string{"src/python/api:", "src/python/pricing:", "src/python/web:"}; !slices.Equal(tested, want) {
		t.Errorf("pants test ran on %v, want %v: the packages that depend on the lockfile\n%s", tested, want, out)
	}
}

// rootRequirementsDemo is a repository whose requirements lie at the root and
// whose lockfile lies in a directory with no BUILD file, and whose two packages
// share a directory name.
func rootRequirementsDemo(junit, coverage string) map[string]map[string]string {
	return map[string]map[string]string{
		"base": {
			"pants.toml":                    "[GLOBAL]\npants_version = \"2.25.0\"\nbackend_packages = [\"pants.backend.python\"]\n\n[python.resolves]\npython-default = \"lockfiles/python-default.lock\"\n",
			"BUILD":                         "python_requirements(name=\"reqs\")\n",
			"requirements.txt":              "fastapi==0.110.0\n",
			"lockfiles/python-default.lock": "{}\n",
			"src/foo/BUILD":                 "python_tests()\n",
			"src/foo/foo_test.py":           "def test_foo():\n    pass\n",
			"src/own/BUILD":                 "python_requirements(name=\"reqs\")\npython_tests()\n",
			"src/own/own_test.py":           "def test_own():\n    pass\n",
			"tests/foo/BUILD":               "python_tests()\n",
			"tests/foo/foo_test.py":         "def test_foo():\n    pass\n",
		},
		"ci": {
			"pants-graph.json": `[
  {"address": "//:reqs", "target_type": "python_requirements", "dependencies": ["//:reqs#fastapi"], "sources": ["requirements.txt"]},
  {"address": "//:reqs#fastapi", "target_type": "python_requirement", "dependencies": ["requirements.txt:reqs"]},
  {"address": "requirements.txt:reqs", "target_type": "_python_requirements_file", "dependencies": [], "sources": ["requirements.txt"]},
  {"address": "src/foo/foo_test.py:tests", "target_type": "python_test", "dependencies": ["//:reqs#fastapi"], "sources": ["src/foo/foo_test.py"]},
  {"address": "tests/foo/foo_test.py:tests", "target_type": "python_test", "dependencies": ["//:reqs#fastapi"], "sources": ["tests/foo/foo_test.py"]},
  {"address": "src/own:reqs#fastapi", "target_type": "python_requirement", "dependencies": []},
  {"address": "src/own/own_test.py:tests", "target_type": "python_test", "dependencies": ["src/own:reqs#fastapi"], "sources": ["src/own/own_test.py"]}
]`,
			"dist/test/reports/src.own.own_test.py.tests.xml":   junit,
			"dist/test/reports/src.foo.foo_test.py.tests.xml":   junit,
			"dist/test/reports/tests.foo.foo_test.py.tests.xml": junit,
			"dist/coverage/python/coverage.xml":                 coverage,
		},
	}
}

func TestARootRequirementOrAStrayLockfileRunsThePackagesThatUseIt(t *testing.T) {
	spectest.Proves(t, "cli/init-from-a-monorepo-tool", "pants-impact-runs-through-pants", "a-change-to-one-pants-package-runs-it-and-its-dependents-through-pants-uncached")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	ci := readDemo(t)["ci"]
	demo := rootRequirementsDemo(ci["dist/test/reports/src.python.pricing.pricing_test.py.tests.xml"], ci["dist/coverage/python/coverage.xml"])
	root := pantsWorkspace(t, demo, buildRuntime(t))
	log := stubPants(t, demo)

	// src/foo and tests/foo share a directory name: the first sync names each
	// by its path, and passes.
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	clitest.InitGitRepo(t, root)
	base := strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))
	want := []string{"src/foo:", "tests/foo:"}

	clitest.WriteFile(t, filepath.Join(root, "requirements.txt"), "fastapi==0.111.0\n")
	requirements := commit(t, root, "bump the requirement")
	if tested, out := pantsTests(t, root, base, log); !slices.Equal(tested, want) {
		t.Errorf("a root requirements change ran %v, want %v\n%s", tested, want, out)
	}

	clitest.WriteFile(t, filepath.Join(root, "lockfiles/python-default.lock"), "{\"changed\": true}\n")
	commit(t, root, "bump the lockfile")
	// src/own declares its requirement in its own directory: the lockfile
	// selects it too.
	wantLock := []string{"src/foo:", "src/own:", "tests/foo:"}
	if tested, out := pantsTests(t, root, requirements, log); !slices.Equal(tested, wantLock) {
		t.Errorf("a lockfile change ran %v, want %v\n%s", tested, wantLock, out)
	}

	// An import Pants infers adds an edge with no BUILD change: validate
	// fails until the sync records it.
	clitest.WriteFile(t, filepath.Join(root, "pants-graph.json"), strings.Replace(demo["ci"]["pants-graph.json"],
		`"tests/foo/foo_test.py:tests", "target_type": "python_test", "dependencies": ["//:reqs#fastapi"]`,
		`"tests/foo/foo_test.py:tests", "target_type": "python_test", "dependencies": ["//:reqs#fastapi", "src/foo/foo_test.py:tests"]`, 1))
	code, out := clitest.RunGateArgs(t, root, "validate", "--all")
	if code == 0 || !strings.Contains(out, "pants.sync_stale") {
		t.Errorf("validate exit %d on a stale graph:\n%s", code, out)
	}
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	if code, out := clitest.RunGateArgs(t, root, "validate", "--all"); code != 0 {
		t.Errorf("validate exit %d after the sync:\n%s", code, out)
	}

	// A member added once a provider view is recorded still gets its path as
	// a name: a fresh clone has no view to tell src/own and tests/own apart.
	clitest.WriteFile(t, filepath.Join(root, "tests/own/BUILD"), "python_tests()\n")
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	if err := os.RemoveAll(filepath.Join(root, ".putnami")); err != nil {
		t.Fatal(err)
	}
	if code, out := clitest.RunGateArgs(t, root, "projects", "list"); code != 0 {
		t.Errorf("a fresh clone does not load, exit %d:\n%s", code, out)
	}
}
