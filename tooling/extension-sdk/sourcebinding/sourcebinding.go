// Package sourcebinding reads one enumerated path of a project into the
// record a source-v1 binding hashes (go.putnami.dev/protocol/capabilities).
// The CLI and the extensions that compute a binding share it, because a
// binding one of them computes is compared with a binding the other recorded,
// possibly on another host: a record must not depend on which of them read it.
//
// Git enumerates the paths; the capabilities protocol owns exclusions,
// canonical ordering and hashing. This package decides only what one path
// contributes: its project-relative slash path, its source-v1 mode, and the
// digest of its working bytes, its symbolic link target, or its visible
// gitlink commit.
package sourcebinding

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	protocaps "go.putnami.dev/protocol/capabilities"
	"go.putnami.dev/sdk/extension/gitread"
)

// HostStatsExecBit reports whether the host file system stores the executable
// bit. Windows does not: every file stats without one there, so a tracked
// file takes its executable bit from the mode git records for it instead.
const HostStatsExecBit = runtime.GOOS != "windows"

// readFile and readlink read a path after Record stats it. They are variables
// so a test can remove the path between the stat and the read.
var (
	readFile = os.ReadFile
	readlink = os.Readlink
)

// RunGit runs git with args in dir and returns its standard output.
type RunGit func(dir string, args ...string) (string, error)

// Record reads repoPath, a path git enumerated under repoRoot, into the
// source-v1 record of the project at projectRoot. indexMode and objectID are
// the mode and object git's index records for a tracked path; an untracked
// path passes both empty. statsExecBit selects where a regular file's
// executable bit comes from (see FileMode); pass HostStatsExecBit. The second
// result is false for a path that contributes nothing: a directory, or a file
// that is deleted before Record reads it, even after Record stats it. runGit
// reads a gitlink's visible commit.
func Record(runGit RunGit, repoRoot, projectRoot, repoPath, indexMode, objectID string, statsExecBit bool) (protocaps.SourceBindingFile, bool, error) {
	return RecordThrough(nil, runGit, repoRoot, projectRoot, repoPath, indexMode, objectID, statsExecBit)
}

// DigestFile answers the raw SHA-256 of the regular file at absPath. read
// reads the file and returns that digest; an implementation either calls it
// or answers a digest it recorded for the same content, such as one a file
// index keeps by the file's stat. An error read returns is returned as is.
type DigestFile func(absPath string, read func() ([]byte, error)) ([]byte, error)

// RecordThrough is Record where a regular file's digest is answered by
// digest; a nil digest reads the file. The record is the same either way: a
// DigestFile answers the digest of the bytes read would read.
func RecordThrough(digest DigestFile, runGit RunGit, repoRoot, projectRoot, repoPath, indexMode, objectID string, statsExecBit bool) (protocaps.SourceBindingFile, bool, error) {
	absPath := filepath.Join(repoRoot, filepath.FromSlash(repoPath))
	projectRel, err := filepath.Rel(projectRoot, absPath)
	if err != nil || projectRel == ".." || strings.HasPrefix(projectRel, ".."+string(filepath.Separator)) {
		return protocaps.SourceBindingFile{}, false, fmt.Errorf("source path %q escapes project root", repoPath)
	}
	path := filepath.ToSlash(projectRel)
	info, err := os.Lstat(absPath)
	if os.IsNotExist(err) {
		return protocaps.SourceBindingFile{}, false, nil
	}
	if err != nil {
		return protocaps.SourceBindingFile{}, false, fmt.Errorf("inspect source path %q: %w", repoPath, err)
	}
	if indexMode == string(protocaps.SourceModeGitlink) {
		if !info.IsDir() {
			return protocaps.SourceBindingFile{}, false, fmt.Errorf("tracked gitlink %q is not a directory", repoPath)
		}
		visibleObjectID := strings.ToLower(objectID)
		top, topErr := runGit(absPath, "rev-parse", "--show-toplevel")
		if topErr == nil {
			resolvedTop, resolveErr := filepath.Abs(strings.TrimSpace(top))
			exactWorktree := resolveErr == nil && sameResolvedPath(resolvedTop, absPath)
			if !exactWorktree {
				entries, readErr := os.ReadDir(absPath)
				if readErr != nil || len(entries) != 0 {
					return protocaps.SourceBindingFile{}, false, fmt.Errorf("tracked gitlink %q does not contain its own exact Git worktree", repoPath)
				}
				return protocaps.SourceBindingFile{Path: path, Mode: protocaps.SourceModeGitlink, Digest: "git:" + visibleObjectID}, true, nil
			}
			head, headErr := runGit(absPath, "rev-parse", "HEAD")
			if headErr != nil || !gitread.ValidObjectID(strings.TrimSpace(head)) {
				return protocaps.SourceBindingFile{}, false, fmt.Errorf("resolve visible gitlink commit %q", repoPath)
			}
			visibleObjectID = strings.ToLower(strings.TrimSpace(head))
		} else {
			entries, readErr := os.ReadDir(absPath)
			if readErr != nil || len(entries) != 0 {
				return protocaps.SourceBindingFile{}, false, fmt.Errorf("tracked gitlink %q has no exact nested Git worktree", repoPath)
			}
		}
		return protocaps.SourceBindingFile{Path: path, Mode: protocaps.SourceModeGitlink, Digest: "git:" + visibleObjectID}, true, nil
	}

	if info.IsDir() {
		return protocaps.SourceBindingFile{}, false, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, readErr := readlink(absPath)
		if os.IsNotExist(readErr) {
			return protocaps.SourceBindingFile{}, false, nil
		}
		if readErr != nil {
			return protocaps.SourceBindingFile{}, false, fmt.Errorf("read source symlink %q: %w", repoPath, readErr)
		}
		return protocaps.SourceBindingFile{Path: path, Mode: protocaps.SourceModeSymlink, Digest: protocaps.SourceDigest([]byte(target))}, true, nil
	}
	if !info.Mode().IsRegular() {
		return protocaps.SourceBindingFile{}, false, fmt.Errorf("source path %q has unsupported mode %s", repoPath, info.Mode())
	}
	read := func() ([]byte, error) {
		data, err := readFile(absPath)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		return sum[:], nil
	}
	var sum []byte
	if digest == nil {
		sum, err = read()
	} else {
		sum, err = digest(absPath, read)
	}
	if os.IsNotExist(err) {
		return protocaps.SourceBindingFile{}, false, nil
	}
	if err != nil {
		return protocaps.SourceBindingFile{}, false, fmt.Errorf("read source path %q: %w", repoPath, err)
	}
	if len(sum) != sha256.Size {
		return protocaps.SourceBindingFile{}, false, fmt.Errorf("digest source path %q: %d bytes, want %d", repoPath, len(sum), sha256.Size)
	}
	mode := FileMode(info.Mode(), indexMode, statsExecBit)
	return protocaps.SourceBindingFile{Path: path, Mode: mode, Digest: "sha256:" + hex.EncodeToString(sum)}, true, nil
}

// FileMode is the source-v1 mode of a regular file. When the host stats the
// executable bit, perm decides. Otherwise a tracked file is executable exactly
// when indexMode, the mode git records for it, is executable, and an
// untracked file, which has no indexMode, is regular.
func FileMode(perm os.FileMode, indexMode string, statsExecBit bool) protocaps.SourceFileMode {
	executable := perm&0o111 != 0
	if !statsExecBit {
		executable = indexMode == string(protocaps.SourceModeExecutable)
	}
	if executable {
		return protocaps.SourceModeExecutable
	}
	return protocaps.SourceModeRegular
}

func sameResolvedPath(left, right string) bool {
	leftResolved, leftErr := filepath.EvalSymlinks(left)
	rightResolved, rightErr := filepath.EvalSymlinks(right)
	if leftErr == nil && rightErr == nil {
		return filepath.Clean(leftResolved) == filepath.Clean(rightResolved)
	}
	return filepath.Clean(left) == filepath.Clean(right)
}
