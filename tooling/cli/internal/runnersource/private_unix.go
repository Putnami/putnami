//go:build !windows

package runnersource

import "os"

// broadPermissions reports whether mode grants any access to the group or to
// other users, which a private source store must not.
func broadPermissions(mode os.FileMode) bool {
	return mode.Perm()&0o077 != 0
}
