package agentctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// writeCLILock writes a putnami.lock.json in dir whose CLI entry is cli.
func writeCLILock(t *testing.T, dir string, cli lockfile.LockEntry) {
	t.Helper()
	lf := lockfile.NewLockFile()
	lf.SetCLI(cli)
	if err := lockfile.WriteLockFile(dir, lf); err != nil {
		t.Fatal(err)
	}
}

// writePutnamiw writes a putnamiw wrapper at the root of dir with mode.
func writePutnamiw(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(dir, "putnamiw")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// A source workspace refuses the global putnami (ADR 0028), so it launches
// through its own ./putnamiw. Every other workspace keeps the bare putnami that
// resolves from PATH. Only committed content decides: a putnamiw without an
// executable bit still counts, so every platform agrees on the launcher.
func TestWorkspaceLauncherFollowsTheWorkspaceSource(t *testing.T) {
	source := lockfile.LockEntry{Source: lockfile.SourceWorkspace}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{"no lock", func(*testing.T, string) {}, "putnami"},
		{"a published pin with a putnamiw", func(t *testing.T, dir string) {
			writeCLILock(t, dir, lockfile.LockEntry{Version: "1.2.3", Source: "https://registry.example/putnami"})
			writePutnamiw(t, dir, 0o755)
		}, "putnami"},
		{"a source workspace without putnamiw", func(t *testing.T, dir string) {
			writeCLILock(t, dir, source)
		}, "putnami"},
		{"a source workspace whose putnamiw is a directory", func(t *testing.T, dir string) {
			writeCLILock(t, dir, source)
			if err := os.Mkdir(filepath.Join(dir, "putnamiw"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "putnami"},
		{"a source workspace with a putnamiw that is not executable", func(t *testing.T, dir string) {
			writeCLILock(t, dir, source)
			writePutnamiw(t, dir, 0o644)
		}, "./putnamiw"},
		{"a source workspace with an executable putnamiw", func(t *testing.T, dir string) {
			writeCLILock(t, dir, source)
			writePutnamiw(t, dir, 0o755)
		}, "./putnamiw"},
		{"a lock with no cli entry", func(t *testing.T, dir string) {
			if err := lockfile.WriteLockFile(dir, lockfile.NewLockFile()); err != nil {
				t.Fatal(err)
			}
			writePutnamiw(t, dir, 0o755)
		}, "putnami"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			if got, err := workspaceLauncher(dir); err != nil || got != tc.want {
				t.Fatalf("workspaceLauncher = %q (%v), want %q", got, err, tc.want)
			}
		})
	}
}

// A lock that cannot be read cannot rule a source workspace out, so no
// launcher is guessed, and the error names the lock once as the file to fix.
func TestWorkspaceLauncherFailsOverAnUnreadableLock(t *testing.T) {
	for name, content := range map[string]string{
		"not json":      `{not json`,
		"unknown shape": `{"version":99}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, lockfile.LockFilename), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := workspaceLauncher(dir)
			if err == nil || strings.Count(err.Error(), lockfile.LockFilename) != 1 {
				t.Fatalf("workspaceLauncher error = %v, want %s named once", err, lockfile.LockFilename)
			}
		})
	}
}
