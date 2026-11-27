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

// TestWriteFilesAtCommitRefusesNamesThatDifferOnlyInCase holds the refusal of
// a commit holding two files whose names differ only in case: on a file
// system that ignores case the second would overwrite the first, so the
// extraction fails whole on every platform and writes nothing (protocols/job
// ADR 0002, point 6). The entries are staged from a blob, so the commit is
// made on any volume.
func TestWriteFilesAtCommitRefusesNamesThatDifferOnlyInCase(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	write(t, filepath.Join(dir, "pkg", "a.txt"), "alpha\n")
	runGit(t, dir, "add", "pkg")
	runGit(t, dir, "commit", "-m", "add pkg")
	blob := strings.TrimSpace(runGit(t, dir, "rev-parse", ":pkg/a.txt"))
	runGit(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blob+",pkg/A.txt")
	runGit(t, dir, "commit", "-m", "add a case twin")
	commit := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

	target := t.TempDir()
	err := WriteFilesAtCommit(dir, commit, []string{"pkg"}, target)
	if err == nil || !strings.Contains(err.Error(), "differ only in case") {
		t.Fatalf("WriteFilesAtCommit of a commit holding pkg/a.txt and pkg/A.txt: err = %v, want a refusal", err)
	}
	entries, readErr := os.ReadDir(target)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("a refused extraction wrote %d entries (err=%v), want none", len(entries), readErr)
	}
}

// TestWriteFilesAtCommitRefusesANameThatIsNotUTF8 holds the refusal of a
// committed name that is not valid UTF-8, which Windows would write under
// another name: the extraction fails whole on every platform and writes
// nothing, and two names that differ only in invalid bytes are not taken for
// a case collision (protocols/job ADR 0002, point 6).
func TestWriteFilesAtCommitRefusesANameThatIsNotUTF8(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("a Windows command line cannot carry the invalid bytes")
	}
	dir := initGitRepo(t)
	write(t, filepath.Join(dir, "pkg", "a.txt"), "alpha\n")
	runGit(t, dir, "add", "pkg")
	runGit(t, dir, "commit", "-m", "add pkg")
	blob := strings.TrimSpace(runGit(t, dir, "rev-parse", ":pkg/a.txt"))
	runGit(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blob+",pkg/b\xfe.txt")
	runGit(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blob+",pkg/b\xff.txt")
	runGit(t, dir, "commit", "-m", "add names that are not UTF-8")
	commit := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

	target := t.TempDir()
	err := WriteFilesAtCommit(dir, commit, []string{"pkg"}, target)
	if err == nil || !strings.Contains(err.Error(), "is not valid UTF-8") {
		t.Fatalf("WriteFilesAtCommit of a commit holding names that are not UTF-8: err = %v, want the UTF-8 refusal", err)
	}
	entries, readErr := os.ReadDir(target)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("a refused extraction wrote %d entries (err=%v), want none", len(entries), readErr)
	}
}
