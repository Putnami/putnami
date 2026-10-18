//go:build !windows

package scheduler

// symlinkPrivilegeMissing is false: a Unix user needs no privilege to create
// a symbolic link.
func symlinkPrivilegeMissing(error) bool {
	return false
}
