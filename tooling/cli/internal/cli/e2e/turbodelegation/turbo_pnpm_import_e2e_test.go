package turbodelegation

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// TestATurborepoPnpmWorkspaceImportsAndRunsItsOwnNodeTools drives the run a
// Node team makes to move from Turborepo to Putnami without leaving Node: a
// pnpm workspace whose packages build with tsc, test with Vitest and Jest,
// and lint with ESLint, installed from the npm registry with the real pnpm
// and Turborepo. Import mode moves every task to @putnami/typescript, which
// keeps pnpm and runs the repository's own tools. It runs only with
// PUTNAMI_NODE_IMPORT_E2E=1, so it proves no spec requirement: an enforced
// requirement must be verified by every gate run.
func TestATurborepoPnpmWorkspaceImportsAndRunsItsOwnNodeTools(t *testing.T) {
	clitest.RequireShell(t)
	pnpm := clitest.RequirePnpm(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	t.Setenv("TURBO_TELEMETRY_DISABLED", "1")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clitest.WritePnpmWorkspace(t, root, readArchive(t, "tooling/cli/internal/cli/e2e/testdata/pnpm-node-tools.txtar")["base"], pnpm, map[string]string{"turbo": "2.6.0"})
	clitest.WriteFile(t, filepath.Join(root, "turbo.json"),
		`{"$schema": "https://turborepo.com/schema.json", "tasks": {"build": {"dependsOn": ["^build"], "outputs": ["dist/**"]}, "test": {}, "lint": {}}}`+"\n")
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name": "demo", "extensions": ["/turbo-extension", "/.ext/typescript", "/.ext/dev"], "includes": []}`)
	writeTurboExtension(t, root, buildRuntime(t))
	clitest.LinkSourceExtensions(t, repoRoot, root)
	clitest.PnpmInstall(t, root)

	// Every build, test and lint task maps to a Putnami task: init lists
	// none it keeps on Turborepo (turbo.task_unmapped), which it names
	// <package>#<task>, or <task> for a root task.
	code, out := clitest.RunGateArgs(t, root, "init", "--from", "turbo", "--mode", "import", "--apply")
	if code != 0 {
		t.Fatalf("init --from turbo --mode import --apply exit %d:\n%s", code, out)
	}
	if unmapped := regexp.MustCompile(`(?m)^\s+(@demo/[a-z]+#)?(build|test|lint): .*$`).FindAllString(out, -1); len(unmapped) > 0 {
		t.Errorf("init keeps lifecycle tasks on Turborepo:\n%s\n\n%s", strings.Join(unmapped, "\n"), out)
	}
	for _, dir := range []string{"packages/core", "packages/utils"} {
		doc := readProject(t, root, dir)
		for _, job := range []string{"@putnami/turbo:build", "@putnami/turbo:test", "@putnami/turbo:lint"} {
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
