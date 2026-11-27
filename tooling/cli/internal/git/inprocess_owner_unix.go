//go:build unix

package git

import (
	"os"
	"syscall"
)

// ownedByCurrentUser refuses, as unsupported, a directory another user owns:
// Git then decides by safe.directory whether it reads the repository.
func ownedByCurrentUser(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return unsupported("stat %s: %v", dir, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return unsupported("%s is owned by another user", dir)
	}
	return nil
}
