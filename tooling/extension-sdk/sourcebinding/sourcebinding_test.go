package sourcebinding

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	protocaps "go.putnami.dev/protocol/capabilities"
)

func TestHostStatsExecBit(t *testing.T) {
	if HostStatsExecBit != (runtime.GOOS != "windows") {
		t.Fatalf("HostStatsExecBit = %v on %s", HostStatsExecBit, runtime.GOOS)
	}
}

// A host that stats the executable bit reads it from the permission bits, an
// unstaged chmod included. On a host that does not, a tracked file takes it
// from the index and an untracked file is regular.
func TestFileMode(t *testing.T) {
	executable, regular := protocaps.SourceModeExecutable, protocaps.SourceModeRegular
	for _, tc := range []struct {
		perm         os.FileMode
		indexMode    string
		statsExecBit bool
		want         protocaps.SourceFileMode
	}{
		{0o755, "100644", true, executable},
		{0o644, "100755", true, regular},
		{0o744, "", true, executable},
		{0o666, "100755", false, executable},
		{0o755, "100644", false, regular},
		{0o666, "", false, regular},
	} {
		if got := FileMode(tc.perm, tc.indexMode, tc.statsExecBit); got != tc.want {
			t.Errorf("FileMode(%o, %q, %v) = %s, want %s", tc.perm, tc.indexMode, tc.statsExecBit, got, tc.want)
		}
	}
}

func noGit(string, ...string) (string, error) {
	return "", errors.New("git is not called for this path")
}

func writeFile(t *testing.T, path, body string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

// A regular file records its project-relative slash path and the digest of its
// working bytes. A Windows checkout, which has no executable bit, records the
// mode the index gives a tracked file, so it matches the record a Linux or
// macOS checkout of the same commit makes from its disk.
func TestRecordOfARegularFileMatchesAcrossHosts(t *testing.T) {
	repo := t.TempDir()
	project := filepath.Join(repo, "project")
	writeFile(t, filepath.Join(project, "bin", "tool.sh"), "#!/bin/sh\n", 0o755)
	want := protocaps.SourceBindingFile{Path: "bin/tool.sh", Mode: protocaps.SourceModeExecutable, Digest: protocaps.SourceDigest([]byte("#!/bin/sh\n"))}
	// A Windows disk cannot hold the executable bit, so the record a host
	// that stats one makes is taken as known there.
	posix := want
	if HostStatsExecBit {
		var exists bool
		var err error
		posix, exists, err = Record(noGit, repo, project, "project/bin/tool.sh", "100755", "", true)
		if err != nil || !exists {
			t.Fatalf("Record on a host with an executable bit: exists %v, %v", exists, err)
		}
		if posix != want {
			t.Fatalf("Record = %+v, want %+v", posix, want)
		}
	}

	// The Windows checkout of the same commit: no executable bit on disk.
	if err := os.Chmod(filepath.Join(project, "bin", "tool.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	windows, exists, err := Record(noGit, repo, project, "project/bin/tool.sh", "100755", "", false)
	if err != nil || !exists || windows != posix {
		t.Fatalf("Record without a host executable bit = %+v (exists %v, %v), want %+v", windows, exists, err, posix)
	}
	untracked, _, err := Record(noGit, repo, project, "project/bin/tool.sh", "", "", false)
	if err != nil || untracked.Mode != protocaps.SourceModeRegular {
		t.Fatalf("an untracked file without a host executable bit = %+v, %v; want regular", untracked, err)
	}
}

func TestRecordSkipsWhatContributesNothingAndRefusesAnEscape(t *testing.T) {
	repo := t.TempDir()
	project := filepath.Join(repo, "project")
	if err := os.MkdirAll(filepath.Join(project, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, repoPath := range []string{"project/deleted.txt", "project/dir"} {
		if _, exists, err := Record(noGit, repo, project, repoPath, "100644", "", true); err != nil || exists {
			t.Errorf("Record(%s) = exists %v, %v; want nothing", repoPath, exists, err)
		}
	}
	if _, _, err := Record(noGit, repo, project, "other/file.txt", "100644", "", true); err == nil {
		t.Error("Record accepted a path outside the project root")
	}
}

func TestRecordOfASymbolicLinkHashesItsTarget(t *testing.T) {
	repo := t.TempDir()
	if err := os.Symlink("target.txt", filepath.Join(repo, "link")); err != nil {
		t.Skipf("this host cannot create a symbolic link: %v", err)
	}
	record, exists, err := Record(noGit, repo, repo, "link", "120000", "", false)
	want := protocaps.SourceBindingFile{Path: "link", Mode: protocaps.SourceModeSymlink, Digest: protocaps.SourceDigest([]byte("target.txt"))}
	if err != nil || !exists || record != want {
		t.Fatalf("Record(link) = %+v (exists %v, %v), want %+v", record, exists, err, want)
	}
}

// A gitlink records the commit its own worktree has checked out, and the
// index's commit when it is not checked out at all.
func TestRecordOfAGitlink(t *testing.T) {
	repo := t.TempDir()
	module := filepath.Join(repo, "module")
	if err := os.Mkdir(module, 0o755); err != nil {
		t.Fatal(err)
	}
	const indexCommit = "ABCDEF0123456"
	gitlink := func(runGit RunGit) (protocaps.SourceBindingFile, error) {
		record, _, err := Record(runGit, repo, repo, "module", string(protocaps.SourceModeGitlink), indexCommit, true)
		return record, err
	}

	record, err := gitlink(noGit)
	if err != nil || record.Digest != "git:abcdef0123456" || record.Mode != protocaps.SourceModeGitlink {
		t.Fatalf("an empty gitlink = %+v, %v; want the index commit", record, err)
	}

	checkedOut := func(_ string, args ...string) (string, error) {
		if args[len(args)-1] == "--show-toplevel" {
			return module + "\n", nil
		}
		return "FEDCBA9876543\n", nil
	}
	if record, err := gitlink(checkedOut); err != nil || record.Digest != "git:fedcba9876543" {
		t.Fatalf("a checked-out gitlink = %+v, %v; want its HEAD", record, err)
	}
	badHead := func(_ string, args ...string) (string, error) {
		if args[len(args)-1] == "--show-toplevel" {
			return module, nil
		}
		return "not-a-commit", nil
	}
	if _, err := gitlink(badHead); err == nil {
		t.Error("a gitlink whose HEAD is not an object name was accepted")
	}
	outer := func(string, ...string) (string, error) { return repo, nil }
	if record, err := gitlink(outer); err != nil || record.Digest != "git:abcdef0123456" {
		t.Fatalf("an empty gitlink inside the outer worktree = %+v, %v; want the index commit", record, err)
	}

	writeFile(t, filepath.Join(module, "stray.txt"), "stray\n", 0o644)
	if _, err := gitlink(noGit); err == nil {
		t.Error("a gitlink with files but no worktree was accepted")
	}
	if _, err := gitlink(outer); err == nil {
		t.Error("a gitlink with files inside the outer worktree was accepted")
	}
	writeFile(t, filepath.Join(repo, "file"), "file\n", 0o644)
	if _, _, err := Record(noGit, repo, repo, "file", string(protocaps.SourceModeGitlink), indexCommit, true); err == nil {
		t.Error("a gitlink that is a file was accepted")
	}
}
