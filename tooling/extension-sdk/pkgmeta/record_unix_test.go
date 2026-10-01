//go:build !windows

package pkgmeta

import (
	"os"
	"testing"
)

// denyWrites makes dir read-only to the test's user until the test ends.
func denyWrites(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permission this case needs")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}
