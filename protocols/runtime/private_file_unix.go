//go:build !windows

package runtime

import "os"

// broadPermissions reports whether mode grants any access to the group or to
// other users, which a private provider file must not.
func broadPermissions(mode os.FileMode) bool {
	return mode.Perm()&0o077 != 0
}
