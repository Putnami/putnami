package pkg

import "testing"

// setUmask skips the test: Windows has no process umask, so a test that
// compares staged modes across umasks has nothing to compare.
func setUmask(t *testing.T, _ int) (restore func()) {
	t.Helper()
	t.Skip("Windows has no process umask")
	return func() {}
}
