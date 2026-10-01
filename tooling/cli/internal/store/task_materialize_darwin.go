//go:build darwin

package store

import (
	"os"

	"golang.org/x/sys/unix"
)

// exchangePaths swaps the directory entries at a and b in one step, with
// renamex_np(RENAME_SWAP): at every instant each path names either its own
// inode or the other's, never nothing. Both paths must exist.
//
// APFS implements it; a volume that does not (HFS+, most network filesystems)
// answers ENOTSUP. swapIn treats every failure as "use the two-rename swap",
// which is safe because a failed exchange changes nothing.
func exchangePaths(a, b string) error {
	if err := unix.RenamexNp(a, b, unix.RENAME_SWAP); err != nil {
		return &os.LinkError{Op: "exchange", Old: a, New: b, Err: err}
	}
	return nil
}
