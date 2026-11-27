//go:build unix

package git

import (
	"fmt"
	"os"
	"syscall"
)

// fileStamp is what the file system says of path: "" when it does not exist,
// else its size, its modification time and its inode.
func fileStamp(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		return "error " + err.Error()
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Sprintf("%d %d", info.Size(), info.ModTime().UnixNano())
	}
	return fmt.Sprintf("%d %d %d", info.Size(), info.ModTime().UnixNano(), stat.Ino)
}

// deviceOf is the device that holds path.
func deviceOf(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, unsupported("stat %s: %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, unsupported("no device for %s", path)
	}
	return uint64(stat.Dev), nil //nolint:unconvert // the type differs between systems
}
