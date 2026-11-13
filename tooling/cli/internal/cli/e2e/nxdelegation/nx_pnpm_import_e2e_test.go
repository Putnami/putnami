package nxdelegation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// TestAnNxPnpmWorkspaceImportsAndRunsItsOwnNodeTools drives the run a Node
// team makes to move from Nx to Putnami without leaving Node: a pnpm
// workspace whose packages build with tsc, test with Vitest and Jest, and
// lint with ESLint, installed from the npm registry with the real pnpm and
// Nx. Import mode moves every target to @putnami/typescript, which keeps
// pnpm and runs the repository's own tools. It runs only with
// PUTNAMI_NODE_IMPORT_E2E=1, so it proves no spec requirement: an enforced
// requirement must be verified by every gate run.
func TestAnNxPnpmWorkspaceImportsAndRunsItsOwnNodeTools(t *testing.T) {
	clitest.RequireShell(t)
	pnpm := clitest.RequirePnpm(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	// The sync runs nx graph: no daemon outlives the test, and no Nx Cloud.
	t.Setenv("NX_DAEMON", "false")
	t.Setenv("NX_NO_CLOUD", "true")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clitest.WritePnpmWorkspace(t, root, readArchive(t, "tooling/cli/internal/cli/e2e/testdata/pnpm-node-tools.txtar")["base"], pnpm, map[string]string{"nx": "22.0.3"})
	clitest.WriteFile(t, filepath.Join(root, "nx.json"),
		`{"targetDefaults": {"build": {"dependsOn": ["^build"], "outputs": ["{projectRoot}/dist"], "cache": true}, "test": {"cache": true}, "lint": {"cache": true}}}`+"\n")
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name": "demo", "extensions": ["/nx-extension", "/.ext/typescript", "/.ext/dev"], "includes": []}`)
	writeNxExtension(t, root, buildRuntime(t))
	clitest.LinkSourceExtensions(t, repoRoot, root)
	clitest.PnpmInstall(t, root)

	// Every build, test and lint target maps to a Putnami task: init warns
	// of none it keeps on Nx (nx.target_unmapped), which it names
	// <project>:<target>, or target <target> of <n> projects.
	code, out := clitest.RunGateArgs(t, root, "init", "--from", "nx", "--mode", "import", "--apply")
	if code != 0 {
		t.Fatalf("init --from nx --mode import --apply exit %d:\n%s", code, out)
	}
	if unmapped := regexp.MustCompile(`(?m)^.*(@demo/[a-z]+:(build|test|lint) |target (build|test|lint) of ).*$`).FindAllString(out, -1); len(unmapped) > 0 {
		t.Errorf("init keeps lifecycle targets on Nx:\n%s\n\n%s", strings.Join(unmapped, "\n"), out)
	}
	for _, dir := range []string{"packages/core", "packages/utils"} {
		var doc struct {
			Disable struct {
				Jobs []string `json:"jobs"`
			} `json:"disable"`
		}
		data, err := os.ReadFile(filepath.Join(root, dir, "putnami.json"))
		if err != nil || json.Unmarshal(data, &doc) != nil {
			t.Fatalf("read %s/putnami.json: %v", dir, err)
		}
		for _, job := range []string{"@putnami/nx:build", "@putnami/nx:test", "@putnami/nx:lint"} {
			if !slices.Contains(doc.Disable.Jobs, job) {
				t.Errorf("%s disables %v, want %s", dir, doc.Disable.Jobs, job)
			}
		}
	}

	clitest.PinToolchains(t, repoRoot, root, "bun", "go")
	if code, out := clitest.RunGateArgs(t, root, "install"); code != 0 {
		t.Fatalf("install exit %d:\n%s", code, out)
	}
	clitest.PinToolchains(t, repoRoot, root, "go")
	clitest.CheckPnpmKept(t, root)

	code, out = clitest.RunGateArgs(t, root, "lint,test,build", "--all")
	if code != 0 {
		t.Fatalf("lint,test,build exit %d:\n%s", code, out)
	}
	clitest.CheckNodeToolsRan(t, root)

	code, out = clitest.RunGateArgs(t, root, "lint,test,build", "--all")
	if code != 0 || !regexp.MustCompile(`Cache: [1-9]\d* local hit · 0 miss`).MatchString(out) {
		t.Errorf("the second run exit %d restored not every task from Putnami's cache:\n%s", code, out)
	}
}
