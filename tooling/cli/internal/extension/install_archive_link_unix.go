//go:build !windows

package extension

import (
	"fmt"
	"os"
)

// extractSymlink recreates the archive's symbolic link at name, a path relative
// to root whose target ExtractTarGzContext already confined to root. An entry
// already at name is replaced.
func extractSymlink(root *os.Root, name, target string) error {
	_ = root.Remove(name) // remove existing
	if err := root.Symlink(target, name); err != nil {
		return fmt.Errorf("create symlink %s: %w", name, err)
	}
	return nil
}
