//go:build linux

package store

import (
	"os"

	"golang.org/x/sys/unix"
)

// exchangePaths swaps the directory entries at a and b in one step, with
// renameat2(RENAME_EXCHANGE): at every instant each path names either its own
// inode or the other's, never nothing. Both paths must exist.
//
// Not every host can do it. Kernels older than 3.15 answer ENOSYS, filesystems
// without the operation answer EINVAL, and overlayfs answers EXDEV for a
// directory it would have to copy up. swapIn treats every failure as "use the
// two-rename swap", which is safe because a failed exchange changes nothing.
func exchangePaths(a, b string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE); err != nil {
		return &os.LinkError{Op: "exchange", Old: a, New: b, Err: err}
	}
	return nil
}
