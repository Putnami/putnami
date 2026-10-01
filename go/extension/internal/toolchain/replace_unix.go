//go:build !windows

package toolchain

import "os"

// replaceExecutable renames staged over dest: Unix replaces a running
// executable's directory entry and leaves the running program alone.
func replaceExecutable(staged, dest string) error { return os.Rename(staged, dest) }
