//go:build !windows

package workspacejob

import (
	"os"
	"path/filepath"
	"strconv"
)

// isExecutable reports whether path is a file the job may run: not a
// directory, with an execute bit set, as the script's `[ -x path ]` tested.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}

// linkManagedGo points bin/go under the extension state root at the go
// command just installed, as the script did: the compiled toolchain resolver
// of the other jobs reads that path as its workspace-managed candidate.
//
// The link is created beside bin/go and renamed over it, so a concurrent
// reader sees the previous link or the new one. A link that cannot be made is
// left out: ResolveGoBinary finds the install by version anyway.
func linkManagedGo(stateRoot, binary string) {
	binDir := filepath.Join(stateRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return
	}
	link := filepath.Join(binDir, "go")
	staged := link + ".tmp." + strconv.Itoa(os.Getpid())
	_ = os.Remove(staged)
	if err := os.Symlink(binary, staged); err != nil {
		return
	}
	if err := os.Rename(staged, link); err != nil {
		_ = os.Remove(staged)
	}
}
