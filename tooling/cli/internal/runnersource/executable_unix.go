//go:build !windows

package runnersource

import "io/fs"

// executableSource reads the executable bit from the worktree file itself: the
// capture binds worktree bytes and modes, whatever the index records.
func executableSource(info fs.FileInfo, _ string) bool {
	return info.Mode()&0o111 != 0
}
