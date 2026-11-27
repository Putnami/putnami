package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestWriteFilesAtCommitWritesTheCommitsFiles extracts a directory of a commit
// with its executable bit, and leaves out what the commit does not hold.
func TestWriteFilesAtCommitWritesTheCommitsFiles(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	write(t, filepath.Join(dir, "pkg", "a.txt"), "alpha\n")
	write(t, filepath.Join(dir, "pkg", "run.sh"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(dir, "pkg", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "pkg")
	runGit(t, dir, "commit", "-m", "add pkg")
	commit := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

	target := t.TempDir()
	if err := WriteFilesAtCommit(dir, commit, []string{"pkg", "absent"}, target); err != nil {
		t.Fatalf("WriteFilesAtCommit: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(target, "pkg", "a.txt"))
	if err != nil || string(data) != "alpha\n" {
		t.Fatalf("pkg/a.txt = %q (err=%v), want the committed bytes", data, err)
	}
	info, err := os.Stat(filepath.Join(target, "pkg", "run.sh"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("pkg/run.sh mode = %v (err=%v), want executable", info, err)
	}
	if _, err := os.Stat(filepath.Join(target, "absent")); !os.IsNotExist(err) {
		t.Fatalf("a path the commit does not hold was written (err=%v)", err)
	}
}

// TestWriteFilesAtCommitRefusesANameWithABackslashOrAColon holds the refusal
// of a committed name that holds `\` or `:`: on Windows either one could place
// the file outside the target directory, so the extraction fails whole on
// every platform and writes nothing, and the release baseline of the line
// that holds it is left out of the job context (protocols/job ADR 0002,
// point 6).
func TestWriteFilesAtCommitRefusesANameWithABackslashOrAColon(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("a Windows file name cannot hold a backslash or a colon, so the commit cannot be made here")
	}
	for name, file := range map[string]string{
		"a backslash": `back\slash.txt`,
		"a colon":     "drive:name.txt",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := initGitRepo(t)
			write(t, filepath.Join(dir, "pkg", "a.txt"), "alpha\n")
			write(t, filepath.Join(dir, "pkg", file), "odd\n")
			runGit(t, dir, "add", "pkg")
			runGit(t, dir, "commit", "-m", "add pkg")
			commit := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

			target := t.TempDir()
			err := WriteFilesAtCommit(dir, commit, []string{"pkg"}, target)
			if err == nil || !strings.Contains(err.Error(), "leaves the target directory") {
				t.Fatalf("WriteFilesAtCommit of a commit holding %q: err = %v, want a refusal", file, err)
			}
			entries, readErr := os.ReadDir(target)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("a refused extraction wrote %d entries (err=%v), want none", len(entries), readErr)
			}
		})
	}
}
