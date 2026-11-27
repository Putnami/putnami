package docslinks

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/gitcandidate"
)

// SelectedDocuments returns the documents a workspace configures by pattern:
// every regular file the reader reads that patterns select, as absolute paths,
// sorted. patterns use the workspace file-pattern grammar, relative to the
// reader's root, with "!" exclusions (wsproto.SelectsPath).
//
// Unlike ProjectDocuments and WorkspaceDocuments, nothing is skipped by name
// or by project: the patterns are the whole selection. Inside a Git work tree
// the candidates are those of the cut, so an ignored file is never selected;
// outside one the disk is walked, less the .git directory and every tree an
// exclusion removes whole.
func (r *Reader) SelectedDocuments(patterns []string) ([]string, error) {
	var files []string
	if r.tree != nil {
		for _, rel := range r.tree.Paths() {
			if !wsproto.SelectsPath(rel, patterns) {
				continue
			}
			if _, kind := r.tree.Resolve(rel); kind == gitcandidate.File {
				files = append(files, filepath.Join(r.root, filepath.FromSlash(rel)))
			}
		}
		sort.Strings(files)
		return files, nil
	}
	_, excludes := wsproto.SplitFilePatterns(patterns)
	err := filepath.WalkDir(r.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A directory that vanished or that this process cannot read holds
			// no document it could check.
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				if entry != nil && entry.IsDir() && path != r.root {
					return filepath.SkipDir
				}
				return nil
			}
			return err
		}
		if entry.IsDir() {
			if path == r.root {
				return nil
			}
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			// A tree an exclusion removes whole is never walked.
			if rel, inside := within(r.root, path); inside && wsproto.ExcludesTree(filepath.ToSlash(rel), excludes) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, inside := within(r.root, path)
		if !inside || !wsproto.SelectsPath(filepath.ToSlash(rel), patterns) {
			return nil
		}
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}
