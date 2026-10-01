//go:build unix

package oci

import (
	"path/filepath"
	"syscall"
	"testing"
)

// descriptorScan bounds the descriptor numbers assertLayoutBlobsClosed
// checks; a test process holds far fewer.
const descriptorScan = 4096

// assertLayoutBlobsClosed fails t when any of this process's descriptors
// refers to a blob of the layout at layoutDir.
func assertLayoutBlobsClosed(t *testing.T, layoutDir string) {
	t.Helper()
	blobs, err := filepath.Glob(filepath.Join(layoutDir, "blobs", "sha256", "*"))
	if err != nil || len(blobs) == 0 {
		t.Fatalf("layout blobs = %v, %v; want the written image", blobs, err)
	}
	for _, blob := range blobs {
		var want syscall.Stat_t
		if err := syscall.Stat(blob, &want); err != nil {
			t.Fatal(err)
		}
		for fd := range descriptorScan {
			var open syscall.Stat_t
			if syscall.Fstat(fd, &open) != nil {
				continue
			}
			if open.Dev == want.Dev && open.Ino == want.Ino {
				t.Errorf("%s is still open as descriptor %d", filepath.Base(blob), fd)
			}
		}
	}
}
