package workspaceclient

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/sdk/extension/gitcandidate"
)

// workspaceFiles reads the files under one root. With a tree it reads the
// repository's Git candidate cut and nothing else: the tracked files and the
// untracked files no ignore rule excludes, which is exactly what the cache
// input `git:**` holds. An ignored file, a file in a directory Git ignores and
// a symbolic link to anything outside the cut are absent, as they are from a
// clone. Without a tree it reads the disk.
type workspaceFiles struct {
	root string
	// tree is the candidate cut of root, or nil to read the disk.
	tree *gitcandidate.Tree
}

// Read returns the bytes of the workspace-relative slash path rel, following
// a symbolic link. In the cut a link is followed only to a candidate. An
// absent file is an error matching fs.ErrNotExist.
func (f workspaceFiles) Read(rel string) ([]byte, error) {
	clean, err := safeWorkspacePath(rel)
	if err != nil {
		return nil, err
	}
	if f.tree != nil {
		return f.tree.ReadFile(clean)
	}
	return os.ReadFile(filepath.Join(f.root, filepath.FromSlash(clean))) //nolint:gosec // validated workspace-relative path
}

// List returns every file at or below the workspace-relative slash path
// prefix, sorted. A symbolic link is listed and not entered, as a directory
// walk lists it.
func (f workspaceFiles) List(prefix string) ([]string, error) {
	clean, err := safeWorkspacePath(prefix)
	if err != nil {
		return nil, err
	}
	if f.tree != nil {
		var paths []string
		if mode, found := f.tree.Lstat(clean); found && !mode.IsDir() {
			paths = append(paths, clean)
		}
		return append(paths, f.candidatesBelow(clean, nil)...), nil
	}
	base := filepath.Join(f.root, filepath.FromSlash(clean))
	var paths []string
	err = filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(f.root, path)
		if relErr != nil {
			return relErr
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

// isRegularFile reports whether the workspace-relative slash path rel is a
// regular file, following a symbolic link.
func (f workspaceFiles) isRegularFile(rel string) bool {
	if f.tree != nil {
		_, kind := f.tree.Resolve(rel)
		return kind == gitcandidate.File
	}
	info, err := os.Stat(filepath.Join(f.root, filepath.FromSlash(rel))) //nolint:gosec // workspace-relative path
	return err == nil && info.Mode().IsRegular()
}

// candidatesBelow lists, in sorted order, the candidates strictly below the
// slash directory dir ("." is the root) that no directory skip names holds.
// skip is asked about the directories between dir and the file, never about
// dir itself. It reads the cut and must not be called without one.
func (f workspaceFiles) candidatesBelow(dir string, skip func(name string) bool) []string {
	paths := f.tree.Paths()
	start, prefix := 0, ""
	if dir != "." {
		prefix = dir + "/"
		start = sort.SearchStrings(paths, prefix)
	}
	var below []string
	for _, rel := range paths[start:] {
		if !strings.HasPrefix(rel, prefix) {
			break
		}
		if skip != nil && skipsADirectory(rel[len(prefix):], skip) {
			continue
		}
		below = append(below, rel)
	}
	return below
}

// skipsADirectory reports whether skip names a directory of the slash path
// inner, its file name excluded.
func skipsADirectory(inner string, skip func(name string) bool) bool {
	for dir := path.Dir(inner); dir != "."; dir = path.Dir(dir) {
		if skip(path.Base(dir)) {
			return true
		}
	}
	return false
}

// workspaceView is the workspace an inspection reads: its member projects and
// their files.
type workspaceView struct {
	workspaceFiles
	// projects holds the sorted member project paths, "." for a project at
	// the root. membershipErr says why there are none to read.
	projects      []string
	membershipErr error
}

// members returns the member project paths, or why no membership is known.
func (v workspaceView) members() ([]string, error) {
	return v.projects, v.membershipErr
}

// indexedView reads the disk and finds the members in the project index a
// Putnami run records under .putnami. It is the view of sync and adopt, which
// read what the session's builds wrote.
func indexedView(workspaceRoot string) workspaceView {
	projects, err := indexedProjectPaths(workspaceRoot)
	return workspaceView{workspaceFiles: workspaceFiles{root: workspaceRoot}, projects: projects, membershipErr: err}
}

// committedView is the check's view: the candidate cut of the workspace when
// it is a Git work tree, the disk otherwise, and the members the orchestrator
// resolved. Membership comes from putnami.workspace.json and the scope
// manifests its includes name, which the cut holds; a member directory that
// holds no candidate has nothing the check could read. An enumeration that
// fails inside a Git work tree is an error, never an empty workspace.
func committedView(workspaceRoot string, members []string) (workspaceView, error) {
	tree, err := gitcandidate.Open(workspaceRoot)
	if err != nil {
		return workspaceView{}, fmt.Errorf("list the candidate files of the workspace: %w", err)
	}
	projects, membershipErr := memberProjectPaths(members)
	return workspaceView{
		workspaceFiles: workspaceFiles{root: workspaceRoot, tree: tree},
		projects:       projects,
		membershipErr:  membershipErr,
	}, nil
}

// memberProjectPaths validates the member project paths a job context
// carries and returns them sorted. No member at all is an error: an
// orchestrator that sends no membership leaves every project unread, which is
// not a workspace without providers.
func memberProjectPaths(members []string) ([]string, error) {
	if len(members) == 0 {
		return nil, errors.New("the job context carries no workspace membership (workspaceProjects); " +
			"run the check through a Putnami CLI that sends it")
	}
	seen := map[string]bool{}
	paths := make([]string, 0, len(members))
	for _, member := range members {
		if member == "" {
			member = "."
		}
		clean, err := safeInventoryProject(member)
		if err != nil || clean != member || seen[member] {
			return nil, fmt.Errorf("the workspace membership contains the invalid or duplicate path %q", member)
		}
		seen[member] = true
		paths = append(paths, member)
	}
	sort.Strings(paths)
	return paths, nil
}
