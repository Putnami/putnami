package documents

import (
	"os"
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// The gates in this project read the repository itself, so every one of them
// needs the same two anchors: where the repository root is, and where the CLI
// module it reasons about lives. Both are derived by walking up from the test's
// working directory — the project root — rather than from a relative literal,
// so a test that moves inside the project keeps working.

// cliDocumentsProjectDir is this project's path from the repository root. It is
// the form the cache key's file selection is relative to, so it is stated once
// and derived from, never spelled again.
const cliDocumentsProjectDir = "tooling/cli-documents"

// cliModuleDir is the CLI module this project reads. The gates read its
// committed release plan, its recorded rehearsal verdict and its manifest, all
// of which stay where the documentation links point at them.
const cliModuleDir = "tooling/cli"

// repositoryRoot walks up to the directory holding putnami.workspace.json.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, wsproto.WorkspaceConfigFilename)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no %s above the test working directory", wsproto.WorkspaceConfigFilename)
		}
		dir = parent
	}
}

// cliModuleRoot returns the root of the CLI module these gates read.
func cliModuleRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(repositoryRoot(t), filepath.FromSlash(cliModuleDir))
}

// projectRoot returns this project's own root — the directory holding the
// manifest whose declared test inputs key these gates.
func projectRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(repositoryRoot(t), filepath.FromSlash(cliDocumentsProjectDir))
}

// wholeTreeGatesEnv names the variable that turns on the gates reading the
// whole candidate tree: the public-cut release gate and the release rehearsal's
// recorded verdict. The manifest declares it under options.test.envInputs, so a
// run with it set never shares a cache key with a run without it.
const wholeTreeGatesEnv = "PUTNAMI_WHOLE_TREE_GATES"

// wholeTreeGatesWorkflowPath is the workflow that runs those gates on every
// push to main, from the repository root.
const wholeTreeGatesWorkflowPath = ".github/workflows/whole-tree-gates.yml"

// requireWholeTreeGates skips a gate that reads the whole candidate tree unless
// wholeTreeGatesEnv is set. No file pattern short of the whole tree keys such a
// gate, and a whole-tree pattern selects this project for every change of the
// repository. The gate therefore runs where the variable is set, on main, and
// that run uses no cache.
func requireWholeTreeGates(t *testing.T) {
	t.Helper()
	if os.Getenv(wholeTreeGatesEnv) == "" {
		t.Skipf("%s is not set: this gate reads the whole candidate tree and runs on main (%s). "+
			"Run it here with %s=1 ./putnamiw test --projects @putnami/cli-documents --no-cache.",
			wholeTreeGatesEnv, wholeTreeGatesWorkflowPath, wholeTreeGatesEnv)
	}
}
