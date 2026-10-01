package runnersource

import "io/fs"

// executableSource reads the executable bit from the Git index. A Windows
// filesystem has no executable bit, so the file's mode never carries one, and
// Git keeps a tracked file's 100755 in the index there (core.fileMode is off).
// An untracked or bound file is 0644, which is what `git add` records on
// Windows. The same committed tree therefore captures the same manifest here
// as on Linux or macOS.
func executableSource(_ fs.FileInfo, indexMode string) bool {
	return indexMode == "100755"
}
