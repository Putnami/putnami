package git

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// indexTrailerLen is how many trailing bytes of the index a stamp keeps: the
// checksum git writes at its end, 20 bytes for a SHA-1 repository and 32 for a
// SHA-256 one.
const indexTrailerLen = 32

// sha1TrailerLen is the length of the smaller checksum. A repository with
// index.skipHash writes zeros in its place.
const sha1TrailerLen = 20

// IndexStamp is what the filesystem says about the index file of a
// repository: whether it exists, its size, its modification time and the
// checksum git writes at its end. Git writes a new checksum whenever the
// index changes, so two equal stamps of one index read the same entries,
// even when a rewrite keeps the size and lands in the same mtime tick.
type IndexStamp struct {
	exists  bool
	size    int64
	mod     time.Time
	trailer [indexTrailerLen]byte
}

// ReadIndexStamp reads the index of the work tree holding dir without starting
// git: the index of the nearest .git directory at or above dir, or of the
// directory a .git gitfile names, as a linked worktree and a submodule have.
// Outside any work tree, and for a work tree whose index git has not written
// yet, the stamp says the index does not exist.
//
// ok is false when the stamp cannot tell whether the index changed, and the
// caller then reads the index again. The git a run starts discovers its
// repository from dir without $GIT_DIR and GIT_CEILING_DIRECTORIES
// (gitread.RepositoryEnvironment), so ok is false when $GIT_DIR is set and
// when a ceiling hides the Git entry that git finds; it is false too when the
// entry names no index it can read, or when the index carries no checksum
// (index.skipHash).
func ReadIndexStamp(dir string) (stamp IndexStamp, ok bool) {
	if os.Getenv("GIT_DIR") != "" {
		return IndexStamp{}, false
	}
	entry, found := nearestGitEntry(dir)
	if !found {
		return IndexStamp{}, true
	}
	if _, visible := WorkTreeEntry(dir); !visible {
		return IndexStamp{}, false
	}
	info, err := os.Stat(entry)
	if err != nil {
		return IndexStamp{}, false
	}
	gitDir := entry
	if !info.IsDir() {
		data, err := os.ReadFile(entry)
		if err != nil {
			return IndexStamp{}, false
		}
		target, isGitfile := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
		if !isGitfile {
			return IndexStamp{}, false
		}
		gitDir = strings.TrimSpace(target)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(filepath.Dir(entry), gitDir)
		}
	}
	file, err := os.Open(filepath.Join(gitDir, "index"))
	if os.IsNotExist(err) {
		return IndexStamp{}, true
	}
	if err != nil {
		return IndexStamp{}, false
	}
	defer file.Close()
	index, err := file.Stat()
	if err != nil || index.Size() < indexTrailerLen {
		return IndexStamp{}, false
	}
	stamp = IndexStamp{exists: true, size: index.Size(), mod: index.ModTime()}
	if _, err := file.ReadAt(stamp.trailer[:], index.Size()-indexTrailerLen); err != nil && err != io.EOF {
		return IndexStamp{}, false
	}
	if bytes.Equal(stamp.trailer[indexTrailerLen-sha1TrailerLen:], make([]byte, sha1TrailerLen)) {
		return IndexStamp{}, false
	}
	return stamp, true
}

// nearestGitEntry returns the first .git directory or gitfile at or above dir,
// whatever GIT_CEILING_DIRECTORIES says, as the git a run starts finds it.
func nearestGitEntry(dir string) (string, bool) {
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
		if parent == current {
			return "", false
		}
		current = parent
	}
}
