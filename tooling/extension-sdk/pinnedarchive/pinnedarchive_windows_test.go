//go:build windows

package pinnedarchive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// requireRefused checks a test's precondition: the operation the test tried
// while a handle held the file was refused with access denied or a sharing
// violation, the refusal the code under test waits out. When the operation went
// through, this host cannot show that refusal, so the test skips and says why
// instead of passing without proving anything. Any other error fails the test.
// release closes the holder before the test stops.
func requireRefused(t *testing.T, what string, err error, release func()) {
	t.Helper()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return
	}
	release()
	if err == nil {
		t.Skipf("precondition: %s went through while another handle held the file, so this host cannot show the refusal the test waits out. "+
			"A read goes through an exclusive hold under an elevated token with SeBackupPrivilege, because Go opens a file read-only "+
			"with FILE_FLAG_BACKUP_SEMANTICS; run the test under a non-elevated token", what)
	}
	t.Fatalf("precondition: %s = %v, want access denied or a sharing violation", what, err)
}

// requireHeldStageRenameRefused requires that this host refuses to rename a
// directory while a file inside it is open the way os.Open opens it, so the
// test proves Install's wait and not a host that lets the rename through.
func requireHeldStageRenameRefused(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	held := filepath.Join(stage, "go.exe")
	if err := os.WriteFile(held, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	scanner, err := os.Open(held)
	if err != nil {
		t.Fatal(err)
	}
	release := func() { _ = scanner.Close() }
	requireRefused(t, "renaming a directory while a file inside it is open", os.Rename(stage, filepath.Join(dir, "published")), release)
	release()
}

// An antivirus scanner that holds a freshly extracted executable open makes
// the rename of the staging directory fail until it closes the file. Install
// waits for it instead of failing the install. The scanner here is the
// Complete check, which opens the staged go/bin/go the way os.Open does and
// closes it 100 ms later.
func TestInstallPublishesAfterAScannerReleasesTheStage(t *testing.T) {
	requireHeldStageRenameRefused(t)
	data := tarGzArchive(t, goLayout()...)
	server, _ := archiveServer(t, data)
	dest := filepath.Join(t.TempDir(), "go-1.99.0")
	closed := make(chan struct{})
	scanned := false
	complete := func(dir string) bool {
		if !goComplete(dir) {
			return false
		}
		if strings.Contains(filepath.Base(dir), ".stage-") && !scanned {
			scanned = true
			scanner, err := os.Open(filepath.Join(dir, "go", "bin", "go"))
			if err != nil {
				t.Errorf("open the staged executable: %v", err)
				close(closed)
				return true
			}
			go func() {
				defer close(closed)
				time.Sleep(100 * time.Millisecond)
				_ = scanner.Close()
			}()
		}
		return true
	}

	installed, err := Install(context.Background(), Pin{URL: server.URL + "/go.tar.gz", SHA256: sha(data), Format: TarGz},
		dest, Options{Complete: complete})
	if scanned {
		<-closed
	}
	if err != nil || !installed {
		t.Fatalf("Install while a scanner held the stage: installed %v, %v", installed, err)
	}
	if !goComplete(dest) {
		t.Fatal("the published destination is not a complete install")
	}
}
