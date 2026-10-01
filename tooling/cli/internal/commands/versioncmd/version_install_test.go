package versioncmd

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func assertNoStagingLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".new") {
			t.Errorf("staging file left behind: %s", entry.Name())
		}
	}
}

func TestInstallBinary_ReplacesThroughNewInode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("new-binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	before := statThroughHandle(t, dst)

	if err := installBinary(src, dst); err != nil {
		t.Fatalf("installBinary: %v", err)
	}

	after := statThroughHandle(t, dst)
	// The destination must be a different file object: rewriting the existing
	// inode of a running executable poisons macOS code-sign validation for the
	// path, so the install has to swap inodes via rename.
	if os.SameFile(before, after) {
		t.Error("destination was rewritten in place; expected a new inode via rename")
	}
	if got, _ := os.ReadFile(dst); string(got) != "new-binary" {
		t.Errorf("content = %q, want %q", got, "new-binary")
	}
	assertInstalledBinaryMode(t, after)
	assertNoStagingLeftovers(t, dir)
}

// statThroughHandle stats path through an open handle. On Windows a FileInfo
// from os.Stat reads the file ID lazily, by path, when os.SameFile first asks
// for it, so two os.Stat results of one path always compare equal; a handle
// fixes the ID of the file the path named when the stat ran.
func statThroughHandle(t *testing.T, path string) os.FileInfo {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestInstallBinary_CreatesMissingDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := installBinary(src, dst); err != nil {
		t.Fatalf("installBinary: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "binary" {
		t.Errorf("content = %q, want %q", got, "binary")
	}
	assertNoStagingLeftovers(t, dir)
}

// runningExeOps treats the file running as a Windows executable that is
// running: it can be renamed, but deleting it or renaming another file over it
// fails.
func runningExeOps(running os.FileInfo) fileOps {
	isRunning := func(path string) bool {
		info, err := os.Lstat(path)
		return err == nil && os.SameFile(info, running)
	}
	return fileOps{
		rename: func(oldpath, newpath string) error {
			if isRunning(newpath) {
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: fs.ErrPermission}
			}
			return os.Rename(oldpath, newpath)
		},
		remove: func(name string) error {
			if isRunning(name) {
				return &os.PathError{Op: "remove", Path: name, Err: fs.ErrPermission}
			}
			return os.Remove(name)
		},
	}
}

// lstatNow returns the FileInfo of path with the identity of the file the path
// names now. On Windows, os.SameFile reads the identity of a FileInfo from
// os.Lstat at its first comparison, through the path: a FileInfo first compared
// after a switch would describe the file that replaced it.
func lstatNow(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.SameFile(info, info)
	return info
}

// asideFiles lists the files a rename-aside switch moved aside in dir.
func asideFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") && strings.Contains(entry.Name(), asideMarker) {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestVersionUpdate_ReplacesRunningCLIByRenameAside(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "one-binary-switch-on-windows", "a-running-cli-is-replaced-by-a-rename-aside")
	t.Setenv("PUTNAMI_UNSAFE_UPDATE", "")
	t.Setenv("PUTNAMI_REGISTRY_URL", "")

	// The Windows layout: putnami is a copy of the active versioned binary, and
	// it is the CLI running this upgrade.
	binDir := t.TempDir()
	oldBinary := []byte("old-binary")
	link := CLIPath(binDir)
	for _, path := range []string{filepath.Join(binDir, hostExecutable("putnami-go-1.0.0")), link} {
		if err := os.WriteFile(path, oldBinary, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	running := lstatNow(t, link)

	newBinary := []byte("#!/bin/sh\necho 'putnami 9.9.9'\n")
	archivePath := filepath.Join(t.TempDir(), "release.tar.gz")
	writeTarGz(t, archivePath, map[string][]byte{cliArchiveEntry: newBinary})
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hashFileSHA256(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "9.9.9")
		w.Header().Set("X-Integrity", "sha256:"+hash)
		_, _ = w.Write(archiveBytes)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PUTNAMI_REGISTRY_URL", srv.URL)

	windows := asideSwitch{ops: runningExeOps(running)}
	originalInstall, originalActivate := installCLIBinary, activateCLIUpdate
	t.Cleanup(func() { installCLIBinary, activateCLIUpdate = originalInstall, originalActivate })
	installCLIBinary, activateCLIUpdate = windows.install, windows.activate

	result, err := VersionUpdateWithOptions(context.Background(), "1.0.0", binDir, VersionUpdateOptions{Channel: "latest"})
	if err != nil {
		t.Fatalf("VersionUpdateWithOptions: %v", err)
	}
	if !result.Updated || result.BinaryPath != filepath.Join(binDir, hostExecutable("putnami-go-9.9.9")) {
		t.Fatalf("result = %+v, want the 9.9.9 binary installed", result)
	}
	if got, _ := os.ReadFile(link); string(got) != string(newBinary) {
		t.Errorf("active CLI = %q, want the new binary", got)
	}
	if got, want := sameContentName(binDir, link), hostExecutable("putnami-go-9.9.9"); got != want {
		t.Errorf("active CLI copies %q, want %s", got, want)
	}

	// The running CLI was only renamed: the same file, with its content, sits
	// aside until it exits.
	asides := asideFiles(t, binDir)
	if len(asides) != 1 {
		t.Fatalf("moved-aside files = %v, want exactly the running CLI", asides)
	}
	aside := filepath.Join(binDir, asides[0])
	if info, err := os.Lstat(aside); err != nil || !os.SameFile(info, running) {
		t.Fatalf("aside %s is not the running CLI (err %v)", aside, err)
	}
	if got, _ := os.ReadFile(aside); string(got) != string(oldBinary) {
		t.Errorf("running CLI content = %q, want it untouched", got)
	}
	assertNoStagingLeftovers(t, binDir)

	// Once it has exited, the next switch reclaims it.
	if err := (asideSwitch{ops: osFileOps}).activate(hostExecutable("putnami-go-1.0.0"), link); err != nil {
		t.Fatalf("switch back: %v", err)
	}
	if asides := asideFiles(t, binDir); len(asides) != 0 {
		t.Errorf("moved-aside files = %v, want them reclaimed", asides)
	}
	if got, _ := os.ReadFile(link); string(got) != string(oldBinary) {
		t.Errorf("active CLI = %q, want putnami-go-1.0.0", got)
	}
}

func TestAsideSwitch_RestoresWhenMoveInFails(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "one-binary-switch-on-windows", "a-failed-move-in-restores-the-old-binary")
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "putnami")
	if err := os.WriteFile(src, []byte("new-binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	failMoveIn := errors.New("sharing violation")
	ops := fileOps{
		rename: func(oldpath, newpath string) error {
			if strings.Contains(filepath.Base(oldpath), ".new-") {
				return failMoveIn
			}
			return os.Rename(oldpath, newpath)
		},
		remove: os.Remove,
	}

	if err := (asideSwitch{ops: ops}).install(src, dst); !errors.Is(err, failMoveIn) {
		t.Fatalf("install err = %v, want %v", err, failMoveIn)
	}
	if got, _ := os.ReadFile(dst); string(got) != "old-binary" {
		t.Errorf("content = %q, want the previous binary restored", got)
	}
	if asides := asideFiles(t, dir); len(asides) != 0 {
		t.Errorf("moved-aside files = %v, want none", asides)
	}
	assertNoStagingLeftovers(t, dir)
}

func TestAsideSwitch_KeepsAndNamesTheOldBinaryWhenRestoreFails(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "one-binary-switch-on-windows", "a-failed-restore-names-the-kept-binary")
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "putnami")
	if err := os.WriteFile(src, []byte("new-binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	failMoveIn := errors.New("sharing violation")
	failRestore := errors.New("access denied")
	ops := fileOps{
		rename: func(oldpath, newpath string) error {
			switch {
			case strings.Contains(filepath.Base(oldpath), ".new-"):
				return failMoveIn
			case strings.Contains(filepath.Base(oldpath), asideMarker):
				return failRestore
			}
			return os.Rename(oldpath, newpath)
		},
		remove: os.Remove,
	}

	err := (asideSwitch{ops: ops}).install(src, dst)
	if !errors.Is(err, failMoveIn) || !errors.Is(err, failRestore) {
		t.Fatalf("install err = %v, want both the move-in and the restore failure", err)
	}
	asides := asideFiles(t, dir)
	if len(asides) != 1 {
		t.Fatalf("moved-aside files = %v, want the previous binary kept aside", asides)
	}
	aside := filepath.Join(dir, asides[0])
	if got, _ := os.ReadFile(aside); string(got) != "old-binary" {
		t.Errorf("aside content = %q, want the previous binary", got)
	}
	if !strings.Contains(err.Error(), "the previous binary is kept at "+aside) {
		t.Errorf("install err = %q, want it to name %s", err, aside)
	}
	if _, statErr := os.Lstat(dst); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("destination stat err = %v, want it absent after a failed switch", statErr)
	}
	assertNoStagingLeftovers(t, dir)
}

func TestAsideSwitch_CreatesMissingDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "putnami-go-1.0.0")
	if err := os.WriteFile(src, []byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := (asideSwitch{ops: osFileOps}).install(src, dst); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "binary" {
		t.Errorf("content = %q, want %q", got, "binary")
	}
	if asides := asideFiles(t, dir); len(asides) != 0 {
		t.Errorf("moved-aside files = %v, want none", asides)
	}
	assertNoStagingLeftovers(t, dir)
}
