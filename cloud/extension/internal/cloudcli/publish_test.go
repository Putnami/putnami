package cloudcli

import (
	"os"
	"path/filepath"
	"testing"
)

// The publish-config command tests live with their source in
// internal/configcli. What remains here are the findAppDir
// resolution tests: they cover clicore.FindAppDir (aliased here as findAppDir),
// the shared workspace/app discovery used by every command, not config logic.

func TestFindAppDir_SkipsSessionWorktrees(t *testing.T) {
	// Session worktrees under .claude/worktrees/<name>/ contain a
	// full repo copy with duplicate putnami.json files. The walk must skip
	// them so resolution lands on the real workload at <root>/apps/api
	// — otherwise `putnami cloud deploy` resolves the wrong appDir and
	// fails with "no image version artifact at .../.claude/worktrees/...".
	workspaceRoot := t.TempDir()
	realApp := filepath.Join(workspaceRoot, "apps", "api")
	mustMkdir(t, realApp)
	writeJSONFile(t, filepath.Join(realApp, "putnami.json"), map[string]any{"name": "apps/api"})

	// Decoy: a worktree copy of the same workload under .claude/worktrees/.
	worktreeApp := filepath.Join(workspaceRoot, ".claude", "worktrees", "foo", "apps", "api")
	mustMkdir(t, worktreeApp)
	writeJSONFile(t, filepath.Join(worktreeApp, "putnami.json"), map[string]any{"name": "apps/api"})

	got, err := findAppDir(workspaceRoot, "api")
	if err != nil {
		t.Fatalf("findAppDir: %v", err)
	}
	// findAppDir resolves the root's symlinks, so the returned path is the
	// real one (t.TempDir on macOS lives under /var -> /private/var).
	want, err := filepath.EvalSymlinks(realApp)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got != want {
		t.Errorf("findAppDir = %s, want %s (resolved into .claude/worktrees instead of the real workload)", got, want)
	}
}

func TestFindAppDir_ResolvesSymlinkedRoot(t *testing.T) {
	// Conductor exposes branch-named symlink aliases pointing at the real
	// worktree. filepath.WalkDir does not descend a root that is itself a
	// symlink, so findAppDir must resolve the root first — otherwise running
	// publish/deploy from an alias finds no projects and fails with a
	// misleading "no workspace project named …".
	real := t.TempDir()
	realApp := filepath.Join(real, "apps", "api")
	mustMkdir(t, realApp)
	writeJSONFile(t, filepath.Join(realApp, "putnami.json"), map[string]any{"name": "apps/api"})

	alias := filepath.Join(t.TempDir(), "branch-alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := findAppDir(alias, "api")
	if err != nil {
		t.Fatalf("findAppDir(symlinked root): %v", err)
	}
	// findAppDir resolves the root to its real path, so compare against the
	// resolved app dir (t.TempDir on macOS also lives under /var -> /private/var).
	want, err := filepath.EvalSymlinks(realApp)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got != want {
		t.Errorf("findAppDir(symlinked root) = %s, want %s", got, want)
	}
}
