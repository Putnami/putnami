package jobs

import (
	"os"
	"path/filepath"
	"testing"
)

// findJobsRepoRoot locates the workspace root so the inventory can read the REAL
// manifests. It skips rather than fails when the tree is unavailable, so a
// packaged-source test run stays green.
func findJobsRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skipf("could not find the Putnami repo root from %s", start)
		}
		dir = parent
	}
}
