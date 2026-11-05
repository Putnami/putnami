package git

import (
	"os"
	"path/filepath"
)

// WorkTreeEntry returns the nearest Git entry at or above dir: $GIT_DIR when it
// is set, else the first .git directory or gitfile on the way up, which is
// where Git's own discovery looks. Like Git, it never walks up into a
// directory GIT_CEILING_DIRECTORIES names. It reads the filesystem alone, so it
// answers when no git program can: a caller that must not take a repository
// for a plain directory when git is missing asks it.
func WorkTreeEntry(dir string) (string, bool) {
	if gitDir := os.Getenv("GIT_DIR"); gitDir != "" {
		return gitDir, true
	}
	ceilings := map[string]bool{}
	for _, ceiling := range filepath.SplitList(os.Getenv("GIT_CEILING_DIRECTORIES")) {
		if ceiling != "" {
			ceilings[filepath.Clean(ceiling)] = true
		}
	}
	current, err := filepath.Abs(dir)
	if err != nil {
		current = filepath.Clean(dir)
	}
	for {
		entry := filepath.Join(current, ".git")
		if _, err := os.Lstat(entry); err == nil {
			return entry, true
		}
		parent := filepath.Dir(current)
		if parent == current || ceilings[parent] {
			return "", false
		}
		current = parent
	}
}
