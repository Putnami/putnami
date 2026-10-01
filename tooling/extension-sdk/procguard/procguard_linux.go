//go:build linux

package procguard

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// DenyInspection marks the calling process non-dumpable, so a process of the
// same user without CAP_SYS_PTRACE can neither read its /proc files nor attach
// to it. Calling it again changes nothing.
func DenyInspection() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("mark the process non-dumpable: %w", err)
	}
	return nil
}
