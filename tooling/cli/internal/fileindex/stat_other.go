//go:build !linux && !darwin && !freebsd

package fileindex

import "os"

// statDecoded is false: this platform's stat has no inode or status-change
// time the index can compare, so it records nothing and every file is read.
const statDecoded = false

func statOf(os.FileInfo) (Stat, bool) { return Stat{}, false }

func localFilesystem(string) bool { return false }
