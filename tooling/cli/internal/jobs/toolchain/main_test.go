package toolchain

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points the artifact store at a directory of its own, so the probe
// records a test writes never reach the machine's store.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "putnami-toolchain-artifacts-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Setenv("PUTNAMI_ARTIFACT_DIR", dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	removeSelfTool()
	os.Exit(code)
}
