//go:build !unix

package storage

import "testing"

// setUmask does nothing on systems without a umask.
func setUmask(t *testing.T, _ int) {
	t.Helper()
}
