package git

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	protocaps "go.putnami.dev/protocol/capabilities"
	"go.putnami.dev/sdk/extension/sourcebinding"
)

func TestHostStatsExecBit(t *testing.T) {
	if hostStatsExecBit != (runtime.GOOS != "windows") {
		t.Fatalf("hostStatsExecBit = %v on %s", hostStatsExecBit, runtime.GOOS)
	}
}

// A host that stats the executable bit keeps the disk's kind. On a host that
// does not, a regular file's kind follows git's worktree mode, and links and
// directories keep their kind.
func TestTrackedKind(t *testing.T) {
	for _, tc := range []struct {
		diskKind, worktreeMode string
		statsExecBit           bool
		want                   string
	}{
		{treeEntryFile, executableMode, true, treeEntryFile},
		{treeEntryExec, "100644", true, treeEntryExec},
		{treeEntryFile, executableMode, false, treeEntryExec},
		{treeEntryExec, "100644", false, treeEntryFile},
		{treeEntryFile, "100644", false, treeEntryFile},
		{treeEntryLink, "120000", false, treeEntryLink},
		{treeEntryDirectory, executableMode, false, treeEntryDirectory},
	} {
		if got := trackedKind(tc.diskKind, tc.worktreeMode, tc.statsExecBit); got != tc.want {
			t.Errorf("trackedKind(%q, %q, %v) = %q, want %q", tc.diskKind, tc.worktreeMode, tc.statsExecBit, got, tc.want)
		}
	}
}

// indexExecutableRepo commits tool.sh and plain.txt at 0644 with
// core.fileMode=false, the setting Git for Windows gives every clone, then marks
// tool.sh executable in the index only and edits both files, so git lists both
// as changed while the disk still carries no executable bit.
func indexExecutableRepo(t *testing.T) string {
	t.Helper()
	repo := initGitRepo(t)
	runGit(t, repo, "config", "core.fileMode", "false")
	tool, plain := filepath.Join(repo, "tool.sh"), filepath.Join(repo, "plain.txt")
	write(t, tool, "#!/bin/sh\n")
	write(t, plain, "plain\n")
	runGit(t, repo, "add", "tool.sh", "plain.txt")
	runGit(t, repo, "commit", "-m", "add files")
	runGit(t, repo, "update-index", "--chmod=+x", "tool.sh")
	write(t, tool, "#!/bin/sh\nexit 0\n")
	write(t, plain, "plain, edited\n")
	for _, p := range []string{tool, plain} {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

// Without a host executable bit, the fingerprint classifies a tracked file from
// the worktree mode `git status --porcelain=v2` reports, which mirrors the index
// under core.fileMode=false. With one, the disk decides, as it always does on
// Linux and macOS.
func TestTrackedChangesTakesTheExecutableBitFromGitWithoutAHostExecBit(t *testing.T) {
	repo := indexExecutableRepo(t)
	kinds := func(statsExecBit bool) map[string]string {
		entries, err := trackedChanges(repo, 0, statsExecBit)
		if err != nil {
			t.Fatalf("trackedChanges: %v", err)
		}
		got := map[string]string{}
		for _, entry := range entries {
			got[entry.path] = entry.kind
		}
		return got
	}

	if got := kinds(false); got["tool.sh"] != treeEntryExec || got["plain.txt"] != treeEntryFile {
		t.Errorf("without a host executable bit: kinds = %v, want tool.sh exec and plain.txt file", got)
	}
	if got := kinds(true); got["tool.sh"] != treeEntryFile || got["plain.txt"] != treeEntryFile {
		t.Errorf("with a host executable bit: kinds = %v, want both file, as the disk says", got)
	}
}

// The same rule for the source binding: the index mode the snapshot enumerates
// decides for a tracked file without a host executable bit, and an untracked
// file is regular there.
func TestSourceBindingRecordTakesTheExecutableBitFromTheIndexWithoutAHostExecBit(t *testing.T) {
	repo := indexExecutableRepo(t)
	untracked := filepath.Join(repo, "new.sh")
	write(t, untracked, "#!/bin/sh\n")
	if err := os.Chmod(untracked, 0o755); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := ReadSourceBindingSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	indexMode := ""
	for _, entry := range snapshot.tracked {
		if entry.path == "tool.sh" {
			indexMode = entry.mode
		}
	}
	if indexMode != string(protocaps.SourceModeExecutable) {
		t.Fatalf("index mode of tool.sh = %q, want %s", indexMode, protocaps.SourceModeExecutable)
	}
	mode := func(repoPath, indexMode string, statsExecBit bool) protocaps.SourceFileMode {
		record, exists, err := sourcebinding.Record(run, snapshot.repoRoot, snapshot.repoRoot, repoPath, indexMode, "", statsExecBit)
		if err != nil || !exists {
			t.Fatalf("sourcebinding.Record(%s): exists %v, %v", repoPath, exists, err)
		}
		return record.Mode
	}

	if got := mode("tool.sh", indexMode, false); got != protocaps.SourceModeExecutable {
		t.Errorf("tracked tool.sh without a host executable bit = %s, want %s", got, protocaps.SourceModeExecutable)
	}
	if got := mode("new.sh", "", false); got != protocaps.SourceModeRegular {
		t.Errorf("untracked new.sh without a host executable bit = %s, want %s", got, protocaps.SourceModeRegular)
	}
	if got := mode("tool.sh", indexMode, true); got != protocaps.SourceModeRegular {
		t.Errorf("tracked tool.sh with a host executable bit = %s, want %s from the disk", got, protocaps.SourceModeRegular)
	}
	// Windows stores no executable bit on disk, so the chmod above left new.sh
	// regular there and the disk has no executable bit to report.
	if !hostStatsExecBit {
		return
	}
	if got := mode("new.sh", "", true); got != protocaps.SourceModeExecutable {
		t.Errorf("untracked new.sh with a host executable bit = %s, want %s from the disk", got, protocaps.SourceModeExecutable)
	}
}
