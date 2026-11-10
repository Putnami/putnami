// Package rushdelegation drives a Rush repository adopted in delegate mode
// through the real CLI with @putnami/rush: the extension's runtime is built
// from this tree, the workspace is the Rush demo of putnami observe, and rush
// is a stub on PATH that logs each call. These tests swap the process streams
// and the working directory, so they run in their own test binary.
package rushdelegation

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

// commandLine declares a bulk test command and a phased deploy command.
const commandLine = `{
  "commands": [
    { "commandKind": "bulk", "name": "test", "summary": "Test" },
    { "commandKind": "phased", "name": "deploy", "summary": "Deploy", "phases": ["_phase:deploy"] }
  ]
}
`

// readDemo reads the Rush demo of putnami observe by section: base/ is the
// repository, head/ the change on top of it.
func readDemo(t *testing.T) map[string]map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "tooling/observe/internal/observe/testdata/demos/rush.txtar"))
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

// buildRuntime builds the @putnami/rush runtime from this tree, the way its
// prepare script does.
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

// rushWorkspace writes the demo's base as a workspace adopted in delegate
// mode, which loads @putnami/rush from a local copy of its manifest whose
// prepare copies the runtime built from this tree.
func rushWorkspace(t *testing.T, demo map[string]map[string]string, runtime string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range demo["base"] {
		clitest.WriteFile(t, filepath.Join(root, rel), content)
	}
	clitest.WriteFile(t, filepath.Join(root, "common/config/rush/command-line.json"), commandLine)
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), "common/temp\nnode_modules\n.putnami\n.gen\ndist\n")
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name": "shop", "extensions": ["/rush-extension"], "includes": [], "options": {"@putnami/rush": {"mode": "delegate"}}}`)

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
	ext := filepath.Join(root, "rush-extension")
	clitest.WriteFile(t, filepath.Join(ext, "putnami.extension.json"), string(data))
	prepare := filepath.Join(ext, "bin", "prepare")
	clitest.WriteFile(t, prepare, "#!/bin/sh\nset -e\nmkdir -p \"$2/compiled\"\ncp '"+runtime+"' \"$2/compiled/putnami-rush\"\n")
	if err := os.Chmod(prepare, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// stubRush puts first on PATH a rush that prints the banner of the demo's
// rushVersion for --help and logs every other call with its working
// directory. It returns the log.
func stubRush(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "rush.log")
	clitest.WriteFile(t, filepath.Join(bin, "rush"), `#!/bin/sh
if [ "$1" = "--help" ]; then echo "Rush Multi-Project Build Tool 5.140.0 - https://rushjs.io"; exit 0; fi
echo "$(pwd) rush $*" >> '`+log+`'
`)
	if err := os.Chmod(filepath.Join(bin, "rush"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// rushBuilds runs putnami build --impacted from base and returns the packages
// rush build ran on, sorted, and the run's output. Every rush call runs at
// the workspace root.
func rushBuilds(t *testing.T, root, base, log string) ([]string, string) {
	t.Helper()
	_ = os.Remove(log)
	code, out := clitest.RunGateArgs(t, root, "build", "--impacted", "--baseline", base)
	if code != 0 {
		t.Fatalf("build --impacted exit %d:\n%s", code, out)
	}
	logged, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	rootReal, _ := filepath.EvalSymlinks(root)
	var built []string
	for _, line := range strings.Split(strings.TrimSpace(string(logged)), "\n") {
		dir, call, _ := strings.Cut(line, " rush ")
		pkg, ok := strings.CutPrefix(call, "build --to ")
		if !ok {
			continue
		}
		if real, _ := filepath.EvalSymlinks(dir); real != rootReal {
			t.Errorf("rush build ran in %s, want the workspace root", dir)
		}
		built = append(built, pkg)
	}
	slices.Sort(built)
	if !strings.Contains(out, "Cache: 0 local hit · 0 miss") {
		t.Errorf("the run looked up or stored Putnami cache entries for delegated tasks:\n%s", out)
	}
	return built, out
}

func commit(t *testing.T, root, message string) string {
	t.Helper()
	clitest.RunGit(t, root, "add", "-A")
	clitest.RunGit(t, root, "commit", "-m", message)
	return strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))
}

func TestAChangeToOneRushProjectRunsRushBuildOnItAndItsDependents(t *testing.T) {
	spectest.Proves(t, "cli/init-from-a-monorepo-tool", "rush-impact-runs-through-rush", "a-change-to-one-rush-project-runs-rush-build-on-it-and-its-dependents-uncached")
	spectest.Proves(t, "cli/init-from-a-monorepo-tool", "init-rerun-shows-a-diff", "init-from-rerun-previews-and-writes-only-with-apply")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	demo := readDemo(t)
	root := rushWorkspace(t, demo, buildRuntime(t))
	log := stubRush(t)

	// Rerun in the adopted workspace: init shows the diff the sync would
	// write, and writes nothing.
	code, out := clitest.RunGateArgs(t, root, "init", "--from", "rush")
	if code != 0 {
		t.Fatalf("init --from rush exit %d:\n%s", code, out)
	}
	for _, want := range []string{"apps/api/putnami.json", "\"/libs/pricing\"", "putnami init --from rush --mode delegate --apply"} {
		if !strings.Contains(out, want) {
			t.Errorf("the rerun shows no %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "apps/api/putnami.json")); !os.IsNotExist(err) {
		t.Fatalf("the rerun without --apply wrote apps/api/putnami.json: %v", err)
	}

	// --apply writes each project's edges, and lists the unmapped command
	// and the build cache that is off.
	code, out = clitest.RunGateArgs(t, root, "init", "--from", "rush", "--apply")
	if code != 0 {
		t.Fatalf("init --from rush --apply exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "not mapped: rush deploy") || !strings.Contains(out, "build cache is off") {
		t.Errorf("the sync lists no unmapped command or cache warning:\n%s", out)
	}
	var api struct {
		Extensions   []string `json:"extensions"`
		Dependencies []string `json:"dependencies"`
	}
	data, err := os.ReadFile(filepath.Join(root, "apps/api/putnami.json"))
	if err != nil || json.Unmarshal(data, &api) != nil {
		t.Fatalf("the sync wrote no putnami.json for apps/api: %v\n%s", err, data)
	}
	if !slices.Equal(api.Extensions, []string{"@putnami/rush"}) || !slices.Equal(api.Dependencies, []string{"/libs/pricing"}) {
		t.Errorf("apps/api putnami.json = %s", data)
	}
	clitest.InitGitRepo(t, root)
	base := strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))

	// A change to pricing runs rush build on pricing and api, which depends
	// on it; web runs nothing.
	for rel, content := range demo["head"] {
		if strings.HasPrefix(rel, "libs/pricing/src/") {
			clitest.WriteFile(t, filepath.Join(root, rel), content)
		}
	}
	pricingChange := commit(t, root, "change pricing")
	built, out := rushBuilds(t, root, base, log)
	if want := []string{"@demo/api", "@demo/pricing"}; !slices.Equal(built, want) {
		t.Errorf("rush build ran on %v, want %v: the changed project and its dependent\n%s", built, want, out)
	}

	// A change to api runs api alone: pricing, its dependency, runs no rush.
	clitest.WriteFile(t, filepath.Join(root, "apps/api/src/index.ts"), demo["base"]["apps/api/src/index.ts"]+"// a change to api alone\n")
	commit(t, root, "change api")
	built, out = rushBuilds(t, root, pricingChange, log)
	if want := []string{"@demo/api"}; !slices.Equal(built, want) {
		t.Errorf("rush build ran on %v, want %v: a dependency runs no rush\n%s", built, want, out)
	}

	// An edge a manifest gains fails validate until the sync records it.
	clitest.WriteFile(t, filepath.Join(root, "apps/web/package.json"), `{"name": "@demo/web", "version": "1.0.0", "private": true, "dependencies": {"@demo/pricing": "workspace:*"}}`)
	code, out = clitest.RunGateArgs(t, root, "validate", "--all")
	if code == 0 || !strings.Contains(out, "rush.sync_stale") {
		t.Errorf("validate exit %d on a stale graph:\n%s", code, out)
	}
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	if code, out := clitest.RunGateArgs(t, root, "validate", "--all"); code != 0 {
		t.Errorf("validate exit %d after the sync:\n%s", code, out)
	}
}
