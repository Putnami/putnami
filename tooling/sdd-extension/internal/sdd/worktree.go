package sdd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go.putnami.dev/sdk/extension/gitcandidate"
)

// worktree reads the workspace for the validate-workspace steps whose cache
// key is the input `git:**`: decisions, recipes and codeowners. Inside a Git
// work tree it reads the repository's candidate cut and nothing else, which is
// exactly what that key holds, so a cached verdict is never about a file the
// key does not read: an ignored file, an empty directory, and a symbolic link
// to anything outside the cut are absent, as they are from a clone. Outside a
// Git work tree it reads the disk, and no `git:` key exists to serve a
// verdict from.
type worktree struct {
	root string
	// tree is the candidate cut of root, or nil outside a Git work tree.
	tree *gitcandidate.Tree
}

// openWorktree lists the candidate cut of root once. An enumeration that fails
// inside a Git work tree is an error, never an empty workspace.
func openWorktree(root string) (*worktree, error) {
	root = filepath.Clean(root)
	tree, err := gitcandidate.Open(root)
	if err != nil {
		return nil, fmt.Errorf("list the candidate files of the workspace: %w", err)
	}
	return &worktree{root: root, tree: tree}, nil
}

func (w *worktree) path(rel string) string {
	return filepath.Join(w.root, filepath.FromSlash(rel))
}

// readRegular reads the workspace-relative slash path rel, which must be a
// regular file: a symbolic link is not followed, and the read is bounded as
// readOptionalBoundedRegularFile bounds it. An absent file is fs.ErrNotExist.
func (w *worktree) readRegular(rel string) ([]byte, error) {
	if w.tree == nil {
		return readOptionalBoundedRegularFile(w.path(rel))
	}
	mode, found := w.tree.Lstat(rel)
	if !found {
		return nil, &fs.PathError{Op: "lstat", Path: w.path(rel), Err: fs.ErrNotExist}
	}
	if !mode.IsRegular() {
		return nil, fmt.Errorf("file is not regular")
	}
	return readOptionalBoundedRegularFile(w.path(rel))
}

// isRegular reports whether rel is a regular file, not following a symbolic
// link.
func (w *worktree) isRegular(rel string) bool {
	if w.tree == nil {
		info, err := os.Lstat(w.path(rel))
		return err == nil && info.Mode().IsRegular()
	}
	mode, found := w.tree.Lstat(rel)
	return found && mode.IsRegular()
}

// isDir reports whether rel is a directory, not following a symbolic link.
// Inside a Git work tree a directory exists when it holds a candidate.
func (w *worktree) isDir(rel string) bool {
	if w.tree == nil {
		info, err := os.Lstat(w.path(rel))
		return err == nil && info.IsDir()
	}
	mode, found := w.tree.Lstat(rel)
	return found && mode.IsDir()
}

// errLeavesWorkspace is the read error of a symbolic link whose target lies
// outside the workspace's candidate cut root.
var errLeavesWorkspace = errors.New("the symbolic link leaves the workspace")

// readFollowing reads rel as os.Stat and os.ReadFile would, following symbolic
// links, and requires a regular file. The read is bounded as readBoundedFile
// bounds it. An absent file is fs.ErrNotExist.
func (w *worktree) readFollowing(rel string) ([]byte, error) {
	target := rel
	if w.tree != nil {
		resolved, kind := w.tree.Resolve(rel)
		switch kind {
		case gitcandidate.Missing:
			return nil, &fs.PathError{Op: "stat", Path: w.path(rel), Err: fs.ErrNotExist}
		case gitcandidate.Outside:
			return nil, errLeavesWorkspace
		case gitcandidate.Dir:
			return nil, errors.New("file is not regular")
		}
		target = resolved
	}
	info, err := os.Stat(w.path(target))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("file is not regular")
	}
	return readBoundedFile(w.path(target), info)
}

// fileExists reports whether rel resolves to a regular file, following
// symbolic links. A path that resolves to anything else is an error.
func (w *worktree) fileExists(rel string) (bool, error) {
	if w.tree == nil {
		info, err := os.Stat(w.path(rel))
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() {
			return false, errors.New("not a regular file")
		}
		return true, nil
	}
	switch _, kind := w.tree.Resolve(rel); kind {
	case gitcandidate.File:
		return true, nil
	case gitcandidate.Missing:
		return false, nil
	case gitcandidate.Outside:
		return false, errLeavesWorkspace
	default:
		return false, errors.New("not a regular file")
	}
}

// candidateRegularFiles lists the regular files of the candidate cut, as sorted
// slash paths relative to the root, below no directory skip excludes. A
// symbolic link is not listed.
func (w *worktree) candidateRegularFiles(skip func(name string) bool) []string {
	var files []string
	for _, rel := range w.tree.Paths() {
		if mode, _ := w.tree.Lstat(rel); !mode.IsRegular() {
			continue
		}
		excluded := false
		for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
			if skip(path.Base(dir)) {
				excluded = true
				break
			}
		}
		if !excluded {
			files = append(files, rel)
		}
	}
	return files
}

// excludedDecisionDirectory reports whether a directory named name is never
// descended into when the decision checks' files are listed.
func excludedDecisionDirectory(name string) bool {
	return excludedDecisionDirectories[name] || strings.HasPrefix(name, ".")
}
