//go:build windows

package workspacejob

import "os"

// isExecutable reports whether path is a file the job may run. Windows has no
// execute bit, so an existing file that is not a directory qualifies.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
