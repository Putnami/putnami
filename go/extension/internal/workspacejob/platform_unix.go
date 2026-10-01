//go:build !windows

package workspacejob

import "os"

// isExecutable reports whether path is a file the job may run: not a
// directory, with an execute bit set, as the script's `[ -x path ]` tested.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}
