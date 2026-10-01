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
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	protocaps "go.putnami.dev/protocol/capabilities"
)

// HostStatsExecBit reports whether the host file system stores the executable
// bit. Windows does not: every file stats without one there, so a tracked
// file takes its executable bit from the mode git records for it instead.
const HostStatsExecBit = runtime.GOOS != "windows"

// RunGit runs git with args in dir and returns its standard output.
type RunGit func(dir string, args ...string) (string, error)

// Record reads repoPath, a path git enumerated under repoRoot, into the
// source-v1 record of the project at projectRoot. indexMode and objectID are
// the mode and object git's index records for a tracked path; an untracked
// path passes both empty. statsExecBit selects where a regular file's
// executable bit comes from (see FileMode); pass HostStatsExecBit. The second
// result is false for a path that contributes nothing: a deleted file or a
// directory. runGit reads a gitlink's visible commit.
func Record(runGit RunGit, repoRoot, projectRoot, repoPath, indexMode, objectID string, statsExecBit bool) (protocaps.SourceBindingFile, bool, error) {
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
			if headErr != nil || !validObjectID(strings.TrimSpace(head)) {
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
		target, readErr := os.Readlink(absPath)
		if readErr != nil {
			return protocaps.SourceBindingFile{}, false, fmt.Errorf("read source symlink %q: %w", repoPath, readErr)
		}
		return protocaps.SourceBindingFile{Path: path, Mode: protocaps.SourceModeSymlink, Digest: protocaps.SourceDigest([]byte(target))}, true, nil
	}
	if !info.Mode().IsRegular() {
		return protocaps.SourceBindingFile{}, false, fmt.Errorf("source path %q has unsupported mode %s", repoPath, info.Mode())
	}
	data, err := os.ReadFile(absPath) //nolint:gosec // path is constrained to the exact project root
	if err != nil {
		return protocaps.SourceBindingFile{}, false, fmt.Errorf("read source path %q: %w", repoPath, err)
	}
	mode := FileMode(info.Mode(), indexMode, statsExecBit)
	return protocaps.SourceBindingFile{Path: path, Mode: mode, Digest: protocaps.SourceDigest(data)}, true, nil
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

// validObjectID accepts an abbreviated or full SHA-1 or SHA-256 object name.
func validObjectID(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}
