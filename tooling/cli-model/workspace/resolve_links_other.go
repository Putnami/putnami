//go:build !windows

package workspace

import "path/filepath"

// resolveLinks is filepath.EvalSymlinks: outside Windows every directory link
// is a symbolic link.
func resolveLinks(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
