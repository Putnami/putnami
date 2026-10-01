//go:build windows

package workspacejob

import "os"

// isExecutable reports whether path is a file the job may run. Windows has no
// execute bit, so an existing file that is not a directory qualifies.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// linkManagedGo creates no bin/go.exe link on Windows. A symbolic link needs a
// privilege most accounts lack, and a copy of the go command outside its
// GOROOT would not run. The toolchain resolver of the other jobs looks for the
// managed installs under libs/ instead, and ResolveGoBinary finds them by
// version.
func linkManagedGo(string, string) {}
