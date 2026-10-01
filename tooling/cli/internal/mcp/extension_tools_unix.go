//go:build !windows

package mcp

import "os"

// extensionToolFilePresent reports whether a resolved command path names a
// file, which is the file exec.Command runs.
func extensionToolFilePresent(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
