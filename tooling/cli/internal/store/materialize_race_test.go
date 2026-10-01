package store

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/sdk/extension/dirlink"
)

// staleJunctionInfo is what os.Lstat reports for a Windows junction:
// fs.ModeIrregular, neither a directory nor a symbolic link.
type staleJunctionInfo struct{ name string }

func (i staleJunctionInfo) Name() string       { return i.name }
func (i staleJunctionInfo) Size() int64        { return 0 }
func (i staleJunctionInfo) Mode() fs.FileMode  { return fs.ModeIrregular }
func (i staleJunctionInfo) ModTime() time.Time { return time.Time{} }
func (i staleJunctionInfo) IsDir() bool        { return false }
func (i staleJunctionInfo) Sys() any           { return nil }

// replayLstat answers the first len(answers) reads of MaterializeDirSymlink
// with the given answers, then reads the file system. Not for parallel tests.
func replayLstat(t *testing.T, answers ...func(string) (fs.FileInfo, error)) *int {
	t.Helper()
	saved := lstatMaterialized
	calls := 0
	lstatMaterialized = func(path string) (fs.FileInfo, error) {
		calls++
		if calls <= len(answers) {
			return answers[calls-1](path)
		}
		return os.Lstat(path)
	}
	t.Cleanup(func() { lstatMaterialized = saved })
	return &calls
}

func staleJunction(path string) (fs.FileInfo, error) {
	return staleJunctionInfo{name: filepath.Base(path)}, nil
}

func deletePending(path string) (fs.FileInfo, error) {
	return nil, &os.PathError{Op: "CreateFile", Path: path, Err: errHeldOpen}
}

func linkedOutput(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "artifact"), []byte("cached\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "build")
	if err := dirlink.Create(src, output); err != nil {
		t.Fatal(err)
	}
	return output
}

func assertDetached(t *testing.T, output string) {
	t.Helper()
	info, err := os.Lstat(output)
	if err != nil {
		t.Fatal(err)
	}
	if dirlink.IsLink(output, info) || !info.IsDir() {
		t.Fatalf("output mode = %v, want a real directory", info.Mode())
	}
	if data, err := os.ReadFile(filepath.Join(output, "artifact")); err != nil || string(data) != "cached\n" {
		t.Fatalf("detached tree lost its content: %q %v", data, err)
	}
}

// A junction's Lstat answer goes stale when a sibling detaches the path before
// dirlink.IsLink reads the link: the mode says irregular, the link is gone. The
// run on Windows failed with "output path is not a directory"; it is a lost
// race, so the path is read again.
func TestMaterializeDirSymlinkRereadsAStaleJunctionAnswer(t *testing.T) {
	output := linkedOutput(t)
	calls := replayLstat(t, staleJunction)

	if err := MaterializeDirSymlink(output); err != nil {
		t.Fatalf("a stale junction answer failed the detach: %v", err)
	}
	if *calls < 2 {
		t.Fatalf("the path was read %d times, want a re-read after the stale answer", *calls)
	}
	assertDetached(t, output)
}

// The same stale answer after the copy, when the detach checks the path before
// its swap, is a lost race too.
func TestMaterializeDirSymlinkRereadsAStaleJunctionAnswerBeforeTheSwap(t *testing.T) {
	output := linkedOutput(t)
	replayLstat(t, os.Lstat, staleJunction)

	if err := MaterializeDirSymlink(output); err != nil {
		t.Fatalf("a stale junction answer before the swap failed the detach: %v", err)
	}
	assertDetached(t, output)
}

// Windows refuses to open a path whose delete is pending with
// ERROR_ACCESS_DENIED until the remover's handle closes. The run on Windows
// failed with "CreateFile ...\build: Access is denied."; the read waits it out.
func TestMaterializeDirSymlinkWaitsOutADeletePendingPath(t *testing.T) {
	waitOnHeld(t, heldByTest, 2*time.Second)
	output := linkedOutput(t)
	replayLstat(t, deletePending, deletePending, os.Lstat, deletePending)

	if err := MaterializeDirSymlink(output); err != nil {
		t.Fatalf("a delete-pending path failed the detach: %v", err)
	}
	assertDetached(t, output)
}

// The re-read is bounded: a path that keeps answering irregular is reported.
// The path is a real directory, as a sibling's detach leaves it: over a live
// junction the irregular answer would be true on Windows (see the next test).
func TestMaterializeDirSymlinkReportsAPathThatStaysIrregular(t *testing.T) {
	output := t.TempDir()
	calls := replayLstat(t, staleJunctions(64)...)

	err := MaterializeDirSymlink(output)
	if err == nil || !strings.Contains(err.Error(), "output path is not a directory") {
		t.Fatalf("error = %v, want the path reported as not a directory", err)
	}
	if *calls != 8 {
		t.Fatalf("the path was read %d times, want one read per attempt, 8", *calls)
	}
}

// On Windows a junction is the directory link (D-W6), and Lstat reports it as
// irregular: an irregular answer that os.Readlink confirms is the link itself,
// which is detached however often it is read. Elsewhere an irregular answer
// is never a link, and the path is reported as above.
func TestMaterializeDirSymlinkIrregularAnswerOverALiveLink(t *testing.T) {
	output := linkedOutput(t)
	replayLstat(t, staleJunctions(64)...)

	err := MaterializeDirSymlink(output)
	if runtime.GOOS != "windows" {
		if err == nil || !strings.Contains(err.Error(), "output path is not a directory") {
			t.Fatalf("error = %v, want the path reported as not a directory", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("a live junction that answers irregular failed the detach: %v", err)
	}
	assertDetached(t, output)
}

func staleJunctions(n int) []func(string) (fs.FileInfo, error) {
	answers := make([]func(string) (fs.FileInfo, error), n)
	for i := range answers {
		answers[i] = staleJunction
	}
	return answers
}

// A regular file is not a lost race: it is reported on the first read.
func TestMaterializeDirSymlinkReportsARegularFileAtOnce(t *testing.T) {
	output := filepath.Join(t.TempDir(), "build")
	if err := os.WriteFile(output, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := replayLstat(t)

	err := MaterializeDirSymlink(output)
	if err == nil || !strings.Contains(err.Error(), "output path is not a directory") {
		t.Fatalf("error = %v, want the path reported as not a directory", err)
	}
	if *calls != 1 {
		t.Fatalf("the path was read %d times, want 1", *calls)
	}
}
