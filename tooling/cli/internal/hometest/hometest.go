// Package hometest points a test's user home at a directory the test owns.
// It is test-only: nothing here may be imported by a production (non-_test.go)
// file, which keeps "testing" out of the shipped CLI binary's dependency graph.
package hometest

import "testing"

// Set makes dir the user home for the rest of t. os.UserHomeDir reads HOME on
// Unix and USERPROFILE on Windows (decision D-W7), so a test that sets only
// HOME reads and writes the real user's files on Windows.
func Set(t testing.TB, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// Temp makes a fresh temporary directory the user home for the rest of t and
// returns it.
func Temp(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	Set(t, dir)
	return dir
}
