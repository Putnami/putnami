// Package moonadoption drives a moon workspace through the real CLI with
// @putnami/moon, in delegate and in import mode: the extension's runtime is
// built from this tree, the workspace is the moon demo of putnami observe,
// and moon is a stub on PATH that logs each run. These tests swap the process
// streams and the working directory, so they run in their own test binary.
package moonadoption

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

// readDemo reads the moon demo of putnami observe by section: base/ is the
// workspace, head/ the change on top of it, ci/ the project graph export its
// CI wrote.
func readDemo(t *testing.T) map[string]map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "tooling/observe/internal/observe/testdata/demos/moon.txtar"))
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

// buildRuntime builds the @putnami/moon runtime from this tree, the way its
// prepare script does.
func buildRuntime(t *testing.T) string {
	t.Helper()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the moon runtime is built with go, which is not on PATH: %v", err)
	}
	out := filepath.Join(t.TempDir(), "putnami-moon")
	cmd := exec.Command(goBinary, "build", "-o", out, "./cmd/putnami-moon")
	cmd.Dir = filepath.Join(repoRoot, "monorepo/moon")
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "CGO_ENABLED=0")
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the moon runtime: %v\n%s", err, data)
	}
	return out
}

// moonWorkspace writes the demo's base as a workspace in the mode that loads
// @putnami/moon from a local copy of its manifest, whose prepare copies the
// runtime built from this tree.
func moonWorkspace(t *testing.T, demo map[string]map[string]string, runtime, mode string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range demo["base"] {
		clitest.WriteFile(t, filepath.Join(root, rel), content)
	}
	clitest.WriteFile(t, filepath.Join(root, ".prototools"), "moon = \"1.30.4\"\n")
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".moon/cache\n.putnami\n.gen\ndist\n")
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name": "shop", "extensions": ["/moon-extension"], "options": {"@putnami/moon": {"mode": "`+mode+`"}}, "includes": []}`)

	data, err := os.ReadFile(filepath.Join(repoRoot, "monorepo/moon/putnami.extension.json"))
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
	ext := filepath.Join(root, "moon-extension")
	clitest.WriteFile(t, filepath.Join(ext, "putnami.extension.json"), string(data))
	prepare := filepath.Join(ext, "bin", "prepare")
	clitest.WriteFile(t, prepare, "#!/bin/sh\nset -e\nmkdir -p \"$2/compiled\"\ncp '"+runtime+"' \"$2/compiled/putnami-moon\"\n")
	if err := os.Chmod(prepare, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// stubMoon puts first on PATH a moon that prints its version and logs each
// call with its working directory, and returns the log.
func stubMoon(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "moon.log")
	clitest.WriteFile(t, filepath.Join(bin, "moon"), `#!/bin/sh
echo "$(pwd) moon $*" >> '`+log+`'
case "$1" in --version) echo "moon 1.30.4";; esac
exit 0
`)
	if err := os.Chmod(filepath.Join(bin, "moon"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// moonRuns runs putnami <verb> --impacted from base and returns the moon
// targets it ran, sorted, and the run's output. Every moon run is at the
// workspace root, and the run looks up and stores no Putnami cache entry.
func moonRuns(t *testing.T, root, verb, base, log string) ([]string, string) {
	t.Helper()
	_ = os.Remove(log)
	code, out := clitest.RunGateArgs(t, root, verb, "--impacted", "--baseline", base)
	if code != 0 {
		t.Fatalf("%s --impacted exit %d:\n%s", verb, code, out)
	}
	logged, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	rootReal, _ := filepath.EvalSymlinks(root)
	var ran []string
	for _, line := range strings.Split(strings.TrimSpace(string(logged)), "\n") {
		dir, call, _ := strings.Cut(line, " moon ")
		target, ok := strings.CutPrefix(call, "run ")
		if !ok {
			continue
		}
		if real, _ := filepath.EvalSymlinks(dir); real != rootReal {
			t.Errorf("moon run ran in %s, want the workspace root", dir)
		}
		ran = append(ran, target)
	}
	slices.Sort(ran)
	if !strings.Contains(out, "Cache: 0 local hit · 0 miss") {
		t.Errorf("the run looked up or stored Putnami cache entries for delegated tasks:\n%s", out)
	}
	return ran, out
}

func commit(t *testing.T, root, message string) string {
	t.Helper()
	clitest.RunGit(t, root, "add", "-A")
	clitest.RunGit(t, root, "commit", "-m", message)
	return strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))
}

// projectFile is the part of a putnami.json the sync writes.
type projectFile struct {
	Tags         []string `json:"tags"`
	Extensions   []string `json:"extensions"`
	Dependencies []string `json:"dependencies"`
	Disable      struct {
		Jobs []string `json:"jobs"`
	} `json:"disable"`
}

func readProject(t *testing.T, root, dir string) projectFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, dir, "putnami.json"))
	var p projectFile
	if err != nil || json.Unmarshal(data, &p) != nil {
		t.Fatalf("the sync wrote no putnami.json for %s: %v\n%s", dir, err, data)
	}
	return p
}

func TestAChangeToOneMoonProjectRunsItAndItsDependentsThroughMoon(t *testing.T) {
	spectest.Proves(t, "cli/init-from-a-monorepo-tool", "moon-delegate-runs-through-moon", "a-change-to-one-moon-project-runs-it-and-its-dependents-through-moon-uncached")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	demo := readDemo(t)
	root := moonWorkspace(t, demo, buildRuntime(t), "delegate")
	log := stubMoon(t)

	// The sync records the projects and writes each one's edges, and lists
	// the task no Putnami command runs.
	code, out := clitest.RunGateArgs(t, root, "projects", "sync")
	if code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "api:dev") {
		t.Errorf("the sync lists no unmapped task:\n%s", out)
	}
	if api := readProject(t, root, "apps/api"); !slices.Equal(api.Extensions, []string{"@putnami/moon"}) || !slices.Equal(api.Dependencies, []string{"/packages/pricing"}) {
		t.Errorf("api putnami.json = %+v", api)
	}
	if web := readProject(t, root, "apps/web"); !slices.Equal(web.Disable.Jobs, []string{"build", "test"}) {
		t.Errorf("web putnami.json = %+v", web)
	}
	clitest.InitGitRepo(t, root)
	base := strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))

	// A change to pricing runs pricing and api, which depends on it; pricing
	// runs the task it tags test.
	for rel, content := range demo["head"] {
		clitest.WriteFile(t, filepath.Join(root, rel), content)
	}
	pricingChange := commit(t, root, "change pricing")
	ran, out := moonRuns(t, root, "test", base, log)
	if want := []string{"api:test", "pricing:unit"}; !slices.Equal(ran, want) {
		t.Errorf("moon ran %v, want %v: the changed project and its dependent\n%s", ran, want, out)
	}
	// Each result records the delegation.
	if code, jsonl := clitest.RunGateArgs(t, root, "test", "--impacted", "--baseline", base, "--output=jsonl"); code != 0 || strings.Count(jsonl, `"delegatedTo":"moon"`) < 2 {
		t.Errorf("the results record no delegatedTo moon (exit %d):\n%s", code, jsonl)
	}

	// A change to api runs api alone: pricing, its dependency, runs no moon.
	clitest.WriteFile(t, filepath.Join(root, "apps/api/src/index.ts"), "export const api = 2;\n")
	commit(t, root, "change api")
	ran, out = moonRuns(t, root, "test", pricingChange, log)
	if want := []string{"api:test"}; !slices.Equal(ran, want) {
		t.Errorf("moon ran %v, want %v: a dependency runs no moon\n%s", ran, want, out)
	}

	// An edge moon gains fails validate until the sync records it.
	clitest.WriteFile(t, filepath.Join(root, "apps/web/moon.yml"), "dependsOn: ['pricing']\n")
	code, out = clitest.RunGateArgs(t, root, "validate", "--all")
	if code == 0 || !strings.Contains(out, "moon.sync_stale") {
		t.Errorf("validate exit %d on a stale graph:\n%s", code, out)
	}
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	if code, out := clitest.RunGateArgs(t, root, "validate", "--all"); code != 0 {
		t.Errorf("validate exit %d after the sync:\n%s", code, out)
	}
}
