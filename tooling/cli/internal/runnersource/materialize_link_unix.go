//go:build !windows

package runnersource

import (
	"os"

	runner "go.putnami.dev/protocol/runner"
)

// materializeSymlink recreates a manifest symlink verbatim. The manifest
// validation already resolved it inside the source tree.
func materializeSymlink(root *os.Root, entry runner.SourceEntry) error {
	return root.Symlink(entry.Target, entry.Path)
}
