//go:build windows

package oci

import (
	"os"
	"testing"
)

// assertLayoutBlobsClosed fails t when the layout at layoutDir cannot be
// removed, which is what an open blob causes on Windows.
func assertLayoutBlobsClosed(t *testing.T, layoutDir string) {
	t.Helper()
	if err := os.RemoveAll(layoutDir); err != nil {
		t.Fatalf("the layout cannot be removed: %v", err)
	}
}
