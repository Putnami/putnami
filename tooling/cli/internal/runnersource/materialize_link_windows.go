//go:build windows

package runnersource

import (
	"fmt"
	"os"

	runner "go.putnami.dev/protocol/runner"
)

// materializeSymlink refuses a manifest symlink and names the entry. On
// Windows a symbolic link needs Developer Mode; a runner that materializes
// links runs on Linux or macOS.
func materializeSymlink(_ *os.Root, entry runner.SourceEntry) error {
	return fmt.Errorf(
		"source entry %s is a symbolic link to %s, which a runner on Windows does not materialize: "+
			"run this source on a Linux or macOS runner, or replace the link with a regular file",
		entry.Path, entry.Target)
}
