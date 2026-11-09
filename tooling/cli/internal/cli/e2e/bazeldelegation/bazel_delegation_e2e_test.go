// Package bazeldelegation drives a Bazel workspace through the real CLI with
// @putnami/bazel: the extension's runtime is built from this tree, the
// workspace is the Bazel demo of putnami observe, and bazel is a stub on PATH
// that writes the build events and reports the demo's CI wrote. These tests
// swap the process streams and the working directory, so they run in their
// own test binary.
package bazeldelegation

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

// readDemo reads the Bazel demo of putnami observe by section: base/ is the
// workspace, head/ the change on top of it, ci/ the query export its CI
// wrote, bazel/ the outputs of its test run.
func readDemo(t *testing.T) map[string]map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "tooling/observe/internal/observe/testdata/demos/bazel.txtar"))
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

// buildRuntime builds the @putnami/bazel runtime from this tree, the way its
// prepare script does.
func buildRuntime(t *testing.T) string {
	t.Helper()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the Bazel runtime is built with go, which is not on PATH: %v", err)
	}
	out := filepath.Join(t.TempDir(), "putnami-bazel")
	cmd := exec.Command(goBinary, "build", "-o", out, "./cmd/putnami-bazel")
	cmd.Dir = filepath.Join(repoRoot, "monorepo/bazel")
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "CGO_ENABLED=0")
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the Bazel runtime: %v\n%s", err, data)
	}
	return out
}

// bazelWorkspace writes the demo's base as a workspace that loads
// @putnami/bazel from a local copy of its manifest, whose prepare copies the
// runtime built from this tree.
func bazelWorkspace(t *testing.T, demo map[string]map[string]string, runtime string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range demo["base"] {
		clitest.WriteFile(t, filepath.Join(root, rel), content)
	}
	clitest.WriteFile(t, filepath.Join(root, ".bazelversion"), "7.4.1\n")
	// A directory whose BUILD file declares no rule is no Bazel package.
	clitest.WriteFile(t, filepath.Join(root, "tools/BUILD"), "exports_files([\"lint.sh\"])\n")
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), "/bazel-*\n.putnami\n.gen\ndist\n")
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name": "shop", "extensions": ["/bazel-extension"], "includes": []}`)

	data, err := os.ReadFile(filepath.Join(repoRoot, "monorepo/bazel/putnami.extension.json"))
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
	if data, err = json.MarshalIndent(manifest, "", "  "); err != nil {
		t.Fatal(err)
	}
	ext := filepath.Join(root, "bazel-extension")
	clitest.WriteFile(t, filepath.Join(ext, "putnami.extension.json"), string(data))
	prepare := filepath.Join(ext, "bin", "prepare")
	clitest.WriteFile(t, prepare, "#!/bin/sh\nset -e\nmkdir -p \"$2/compiled\"\ncp '"+runtime+"' \"$2/compiled/putnami-bazel\"\n")
	if err := os.Chmod(prepare, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// testResultEvent is the build event of one test run, as
// --build_event_json_file writes it.
type testResultEvent struct {
	ID struct {
		TestResult struct {
			Label string `json:"label"`
		} `json:"testResult"`
	} `json:"id"`
	TestResult struct {
		Status           string       `json:"status"`
		TestActionOutput []testOutput `json:"testActionOutput"`
	} `json:"testResult"`
}

// testOutput is one output file of a test run, by its file URI.
type testOutput struct {
	Name string `json:"name"`
	URI  string `json:"uri"`
}

// stubBazel puts first on PATH a bazel that prints its version, prints the
// demo's export for query, and for coverage //<pkg>:all writes the
// build-event file of that package's test runs, whose JUnit and coverage
// outputs lie in an output tree outside the workspace, as Bazel's do. It logs
// each call with its working directory and returns the log.
func stubBazel(t *testing.T, demo map[string]map[string]string) string {
	t.Helper()
	bin, ci, outBase := t.TempDir(), t.TempDir(), t.TempDir()
	clitest.WriteFile(t, filepath.Join(ci, "bazel-graph.json"), demo["ci"]["bazel-graph.json"])
	testlogs := filepath.Join(outBase, "testlogs")
	events := map[string][]string{}
	for rel, content := range demo["bazel"] {
		logRel, ok := strings.CutPrefix(rel, "testlogs/")
		if !ok || !strings.HasSuffix(logRel, "/test.xml") {
			continue
		}
		logDir := filepath.Dir(logRel)
		pkg, target := filepath.Dir(logDir), filepath.Base(logDir)
		clitest.WriteFile(t, filepath.Join(testlogs, logRel), content)
		var coverage strings.Builder
		for _, record := range strings.SplitAfter(demo["bazel"]["out/_coverage/_coverage_report.dat"], "end_of_record\n") {
			if strings.HasPrefix(record, "SF:"+pkg+"/") {
				coverage.WriteString(record)
			}
		}
		clitest.WriteFile(t, filepath.Join(testlogs, logDir, "coverage.dat"), coverage.String())
		uri := func(name string) string { return "file://" + filepath.ToSlash(filepath.Join(testlogs, logDir, name)) }
		var e testResultEvent
		e.ID.TestResult.Label, e.TestResult.Status = "//"+pkg+":"+target, "PASSED"
		e.TestResult.TestActionOutput = []testOutput{{"test.xml", uri("test.xml")}, {"test.lcov", uri("coverage.dat")}}
		event, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		events[pkg] = append(events[pkg], string(event))
	}
	for pkg, lines := range events {
		clitest.WriteFile(t, filepath.Join(ci, "events", strings.ReplaceAll(pkg, "/", "_")+".json"), strings.Join(lines, "\n")+"\n")
	}
	log := filepath.Join(t.TempDir(), "bazel.log")
	clitest.WriteFile(t, filepath.Join(bin, "bazel"), `#!/bin/sh
set -e
echo "$(pwd) bazel $*" >> '`+log+`'
case "$1" in --version) echo "bazel 7.4.1"; exit 0;; esac
for a in "$@"; do last="$a"; done
case "$1" in
  query) cat '`+ci+`/bazel-graph.json';;
  coverage)
    pkg="${last#//}"; pkg="${pkg%:all}"; flat="$(echo "$pkg" | tr / _)"
    for a in "$@"; do case "$a" in --build_event_json_file=*) bep="${a#--build_event_json_file=}";; esac; done
    mkdir -p "$(dirname "$bep")"
    if [ -e '`+ci+`'/events/"$flat".json ]; then cp '`+ci+`'/events/"$flat".json "$bep"; else : > "$bep"; fi;;
esac
`)
	if err := os.Chmod(filepath.Join(bin, "bazel"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// bazelTests runs putnami test --impacted from base and returns the packages
// bazel coverage ran on, sorted, and the run's output. Every bazel call runs
// at the workspace root.
func bazelTests(t *testing.T, root, base, log string) ([]string, string) {
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
		dir, call, _ := strings.Cut(line, " bazel ")
		if !strings.HasPrefix(call, "coverage ") {
			continue
		}
		if real, _ := filepath.EvalSymlinks(dir); real != rootReal {
			t.Errorf("bazel coverage ran in %s, want the workspace root", dir)
		}
		tested = append(tested, call[strings.LastIndex(call, " ")+1:])
	}
	slices.Sort(tested)
	// Every task of the run is uncached: no lookup, so no hit and no miss,
	// and nothing is stored.
	if !strings.Contains(out, "Cache: 0 local hit · 0 miss") {
		t.Errorf("the run looked up or stored Putnami cache entries for delegated tasks:\n%s", out)
	}
	// The probe attributes each Bazel edge to the Bazel graph, so the
	// declared-edge check judges them and finds every one backed.
	if strings.Contains(out, "workspace.declared_edge") {
		t.Errorf("the run warns workspace.declared_edge for a Bazel package:\n%s", out)
	}
	return tested, out
}

func commit(t *testing.T, root, message string) string {
	t.Helper()
	clitest.RunGit(t, root, "add", "-A")
	clitest.RunGit(t, root, "commit", "-m", message)
	return strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))
}

func TestAChangeToOneBazelPackageRunsItAndItsDependentsThroughBazel(t *testing.T) {
	spectest.Proves(t, "cli/init-from-a-monorepo-tool", "bazel-impact-runs-through-bazel", "a-change-to-one-bazel-package-runs-it-and-its-dependents-through-bazel-uncached")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	demo := readDemo(t)
	root := bazelWorkspace(t, demo, buildRuntime(t))
	log := stubBazel(t, demo)

	// The sync records the packages and writes each one's edges from
	// bazel query //....
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	var api struct {
		Extensions   []string `json:"extensions"`
		Dependencies []string `json:"dependencies"`
	}
	data, err := os.ReadFile(filepath.Join(root, "api/putnami.json"))
	if err != nil || json.Unmarshal(data, &api) != nil {
		t.Fatalf("the sync wrote no putnami.json for api: %v\n%s", err, data)
	}
	if !slices.Equal(api.Extensions, []string{"@putnami/bazel"}) || !slices.Equal(api.Dependencies, []string{"/pricing"}) {
		t.Errorf("api putnami.json = %s", data)
	}
	clitest.InitGitRepo(t, root)
	base := strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))

	// A change to pricing runs pricing and api, which depends on it, and the
	// summary reads the JUnit reports Bazel wrote outside the workspace.
	for rel, content := range demo["head"] {
		if strings.HasPrefix(rel, "pricing/") {
			clitest.WriteFile(t, filepath.Join(root, rel), content)
		}
	}
	pricingChange := commit(t, root, "change pricing")
	tested, out := bazelTests(t, root, base, log)
	if want := []string{"//api:all", "//pricing:all"}; !slices.Equal(tested, want) {
		t.Errorf("bazel coverage ran on %v, want %v: the changed package and its dependent\n%s", tested, want, out)
	}
	var collected []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "test.xml" {
			rel, _ := filepath.Rel(root, path)
			collected = append(collected, filepath.ToSlash(rel))
		}
		return nil
	})
	if len(collected) != 2 {
		t.Errorf("the test tasks collected %v, want the JUnit reports of pricing and api\n%s", collected, out)
	}

	// A change to api runs api alone: pricing, its dependency, runs no bazel.
	file := filepath.Join(root, "api/api.go")
	data, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, file, string(data)+"\n// a change to api alone\n")
	apiChange := commit(t, root, "change api")
	tested, out = bazelTests(t, root, pricingChange, log)
	if want := []string{"//api:all"}; !slices.Equal(tested, want) {
		t.Errorf("bazel coverage ran on %v, want %v: a dependency runs no bazel\n%s", tested, want, out)
	}

	// A change to MODULE.bazel runs every package that holds a test; web
	// holds none, so its test task is disabled, and tools, which holds no
	// rule, is a bare member that runs nothing.
	file = filepath.Join(root, "MODULE.bazel")
	data, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, file, string(data)+"bazel_dep(name = \"gazelle\", version = \"0.40.0\")\n")
	clitest.WriteFile(t, filepath.Join(root, "tools/lint.sh"), "#!/bin/sh\n")
	commit(t, root, "add gazelle and a lint script")
	tested, out = bazelTests(t, root, apiChange, log)
	if want := []string{"//api:all", "//pricing:all"}; !slices.Equal(tested, want) {
		t.Errorf("bazel coverage ran on %v, want %v: the packages that read MODULE.bazel\n%s", tested, want, out)
	}

	// An edge the BUILD files gain fails validate until the sync records it.
	clitest.WriteFile(t, filepath.Join(root, "bazel-graph.json"), strings.Replace(demo["ci"]["bazel-graph.json"],
		`"ruleInput":["//web:index.js",`, `"ruleInput":["//web:index.js","//pricing:pricing",`, 1))
	code, out := clitest.RunGateArgs(t, root, "validate", "--all")
	if code == 0 || !strings.Contains(out, "bazel.sync_stale") {
		t.Errorf("validate exit %d on a stale graph:\n%s", code, out)
	}
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	if code, out := clitest.RunGateArgs(t, root, "validate", "--all"); code != 0 {
		t.Errorf("validate exit %d after the sync:\n%s", code, out)
	}
}
