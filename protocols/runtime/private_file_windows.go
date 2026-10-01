package runtime

import "os"

// broadPermissions is always false: Windows derives mode bits from the
// read-only attribute, so they say nothing about who may read a file. Access
// follows the user profile's ACL.
func broadPermissions(os.FileMode) bool {
	return false
}
