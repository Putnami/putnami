package runnersource

import "os"

// broadPermissions is always false: Windows derives mode bits from the
// read-only attribute and reports 0o777 for every directory, so they say
// nothing about who may read the store. Access follows the user profile's ACL.
func broadPermissions(os.FileMode) bool {
	return false
}
