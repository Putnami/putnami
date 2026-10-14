package cachekey

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// writeExecutable writes a fixture program a test then starts directly.
//
// A fork in another parallel test inherits every descriptor open at that
// moment, until its child calls exec; while that copy of the write descriptor
// lives, starting the script fails with "text file busy".
// Go takes syscall.ForkLock for writing around each fork, so holding it for
// reading from open to close keeps the descriptor out of every child. Every
// test that writes a program it starts goes through this helper, which
// TestExecutableFixturesAreWrittenUnderTheForkLock enforces.
func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	syscall.ForkLock.RLock()
	err := os.WriteFile(path, []byte(contents), 0o755) //nolint:gosec // an executable test fixture
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
