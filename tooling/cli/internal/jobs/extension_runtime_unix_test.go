//go:build unix

package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// On Unix an irregular entry of a replaced module, here a FIFO, stays out of
// the prepared-runtime digest and the staged view, as it always has: the
// Windows refusal of reparse points does not reach Unix, so no Unix key moves.
func TestRuntimeReplacementIrregularEntryStaysOutOfDigestAndStagingOnUnix(t *testing.T) {
	root := t.TempDir()
	extensionRoot, sdkRoot := writeRuntimeReplacementFixture(t, root)
	ext := runtimeTestExtension(extensionRoot)
	ext.Runtime.Prepare.Inputs = append(ext.Runtime.Prepare.Inputs, "go.mod")
	without, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}

	if err := syscall.Mkfifo(filepath.Join(sdkRoot, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	with, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatalf("digest refused an irregular entry on Unix: %v", err)
	}
	if with != without {
		t.Fatalf("a FIFO moved the Unix digest: %s, want %s", with, without)
	}
	stagedRoot := stageRuntimeReplacementView(t, extensionRoot, ext)
	if _, err := os.Lstat(filepath.Join(stagedRoot, "sdk", "pipe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a FIFO reached the staged view: %v", err)
	}
}
