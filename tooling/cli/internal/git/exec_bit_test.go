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

// Without a host executable bit, the index mode the snapshot enumerates
// decides for a tracked file, and an untracked file is regular there. With
// one, the disk decides, as it always does on Linux and macOS.
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
