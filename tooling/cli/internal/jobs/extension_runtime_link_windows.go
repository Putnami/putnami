//go:build windows

package jobs

import (
	"errors"
	"syscall"
)

// symlinkPrivilegeMissing reports whether Windows refused a symbolic link
// because the user holds no symbolic-link privilege, which Developer Mode or
// an administrator grants.
func symlinkPrivilegeMissing(err error) bool {
	return errors.Is(err, syscall.ERROR_PRIVILEGE_NOT_HELD)
}
