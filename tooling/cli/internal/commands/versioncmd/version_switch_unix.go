//go:build !windows

package versioncmd

import (
	"os"
	"path/filepath"
)

// installBinary stages src next to dst and renames it into place. The
// destination must never be rewritten through its existing inode: on macOS,
// modifying an executable while any process still runs the old image
// invalidates the kernel's cached code signature for that file, after which
// every exec of the path is killed or hangs uninterruptibly until reboot.
func installBinary(src, dst string) error {
	stagingPath, err := stageBinary(src, dst)
	if err != nil {
		return err
	}
	defer os.Remove(stagingPath)
	return os.Rename(stagingPath, dst)
}

// activateCLI points the putnami symlink link at targetName, a binary in the
// same directory.
func activateCLI(targetName, link string) error {
	return replaceSymlink(targetName, link)
}

// activeCLIName names the binary in binDir the putnami symlink points at.
func activeCLIName(binDir string) string {
	target, _ := os.Readlink(filepath.Join(binDir, "putnami"))
	return target
}
