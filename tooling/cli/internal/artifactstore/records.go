package artifactstore

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"go.putnami.dev/tooling/cli/internal/store"
)

// recordDirs are the store root's directories of records kept between runs
// (package store), which GC removes once idle.
var recordDirs = []string{store.ToolchainProbeDir, store.InputResolveDir}

// sweepRecords removes each record of recordDirs that no run read or wrote
// for longer than maxIdle, and the shard directories it empties. A record a
// later run needs is replaced by the process it stood for. maxIdle of 0 keeps
// every record.
func (s *Store) sweepRecords(now time.Time, maxIdle time.Duration) int {
	if maxIdle <= 0 {
		return 0
	}
	swept := 0
	for _, name := range recordDirs {
		root := filepath.Join(s.root, name)
		var dirs []string
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				if path != root {
					dirs = append(dirs, path)
				}
				return nil
			}
			info, err := entry.Info()
			if err != nil || now.Sub(info.ModTime()) <= maxIdle {
				return nil
			}
			if os.Remove(path) == nil {
				swept++
			}
			return nil
		})
		for i := len(dirs) - 1; i >= 0; i-- {
			_ = os.Remove(dirs[i]) // only an empty shard is removed
		}
	}
	return swept
}
