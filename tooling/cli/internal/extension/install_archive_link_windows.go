//go:build windows

package extension

import (
	"fmt"
	"os"
	"path/filepath"
)

// extractSymlink refuses the archive's symbolic link at name and names the
// entry. On Windows a symbolic link needs Developer Mode, and a directory
// junction stores an absolute path, which no longer resolves once the extracted
// tree is renamed into the artifact store.
func extractSymlink(_ *os.Root, name, target string) error {
	return fmt.Errorf(
		"archive entry %s is a symbolic link to %s: Putnami does not create links from an archive on Windows, "+
			"so the publisher must ship a regular file or a directory at that path",
		filepath.ToSlash(name), target)
}
