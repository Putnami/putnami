package features

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/sdk/extension/gitcandidate"
	putnamigit "go.putnami.dev/tooling/sdd/extension/internal/gitread"
)

// CutReader reads the workspace's Git candidate cut and nothing else: the
// tracked files and the untracked files no ignore rule excludes, which is what
// the task input `git:**` keys (ADR 0041 of the CLI). The cached `validate`
// step reads through it, so its verdict is never about a file its key does not
// hold: an ignored artifact, an ignored evidence fragment and an empty
// directory are absent, as they are from a clone, and a symbolic link is
// followed from its target text only to another candidate.
//
// A source binding needs no cut of its own. ProjectSourceBinding enumerates
// the same candidates with Git and records each one's path, bytes or link
// text, and executable bit, which the key holds too (ADR 0061 of the CLI). A
// submodule and an unmerged path produce no key, so a run that meets one is
// never served from the cache.
type CutReader struct {
	// root is the physical workspace root the source binding reads.
	root string
	tree *gitcandidate.Tree
}

// NewTaskReader returns the reader the cached `validate` step evaluates
// through: the candidate cut of root inside a Git work tree, and the disk
// outside one, where no `git:` key exists to serve a verdict from. A cut that
// cannot be listed inside a work tree is an error, never an empty workspace.
func NewTaskReader(root string) (Reader, error) {
	reader, err := NewOSReader(root)
	if err != nil {
		return nil, err
	}
	tree, err := gitcandidate.Open(reader.root)
	if err != nil {
		return nil, fmt.Errorf("list the candidate files of the workspace: %w", err)
	}
	if tree == nil {
		return reader, nil
	}
	return &CutReader{root: reader.root, tree: tree}, nil
}

// ReadFile reads one bounded regular candidate, following only candidate
// symbolic links that stay inside the discovery root.
func (reader *CutReader) ReadFile(root, relative string) ([]byte, error) {
	target, kind, err := reader.resolve(root, relative)
	if err != nil {
		return nil, err
	}
	if kind != gitcandidate.File {
		return nil, newReaderError(ReaderErrorUnsupportedFile, nil)
	}
	file, err := os.Open(filepath.Join(reader.root, filepath.FromSlash(target))) //nolint:gosec // a candidate path inside the root
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, newReaderError(ReaderErrorUnsupportedFile, nil)
	}
	if info.Size() > MaxReadBytes {
		return nil, newReaderError(ReaderErrorReadLimit, nil)
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxReadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxReadBytes {
		return nil, newReaderError(ReaderErrorReadLimit, nil)
	}
	return data, nil
}

// ReadDir lists the entries of one directory of the cut: each name a
// candidate path continues with below it. An entry is a directory when a
// candidate lies beneath it; a symbolic link is not one, as os.ReadDir reports
// it.
func (reader *CutReader) ReadDir(root, relative string) ([]DirEntry, error) {
	target, kind, err := reader.resolve(root, relative)
	if err != nil {
		return nil, err
	}
	if kind != gitcandidate.Dir {
		return nil, errors.New("feature reader: not a directory")
	}
	prefix := ""
	if target != "." {
		prefix = target + "/"
	}
	paths := reader.tree.Paths()
	directories := make(map[string]bool)
	var names []string
	for index := sort.SearchStrings(paths, prefix); index < len(paths) && strings.HasPrefix(paths[index], prefix); index++ {
		name, _, nested := strings.Cut(paths[index][len(prefix):], "/")
		if _, seen := directories[name]; !seen {
			names = append(names, name)
		}
		directories[name] = directories[name] || nested
	}
	if len(names) > MaxEvidenceFiles {
		return nil, newReaderError(ReaderErrorEvidenceFileLimit, nil)
	}
	entries := make([]DirEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, DirEntry{Name: name, IsDirectory: directories[name]})
	}
	return entries, nil
}

// SourceBinding recomputes source-v1 over one exact workspace-relative root of
// the cut.
func (reader *CutReader) SourceBinding(root string) (string, error) {
	resolved := reader.root
	if root != "" {
		target, kind, err := reader.resolve("", root)
		if err != nil {
			return "", err
		}
		if kind != gitcandidate.Dir {
			return "", errors.New("feature reader: source root is not a directory")
		}
		resolved = filepath.Join(reader.root, filepath.FromSlash(target))
	}
	return putnamigit.ProjectSourceBinding(reader.root, resolved)
}

// resolve validates root and relative as OSReader does, then resolves them in
// the cut: a path that is not a candidate, nor a directory holding one, does
// not exist, whatever the disk holds.
func (reader *CutReader) resolve(root, relative string) (string, gitcandidate.Kind, error) {
	if code := validateRelativePath(root, true); code != "" {
		return "", gitcandidate.Missing, newReaderError(code, nil)
	}
	if code := validateRelativePath(relative, false); code != "" {
		return "", gitcandidate.Missing, newReaderError(code, nil)
	}
	discoveryRoot := "."
	if root != "" {
		resolved, kind := reader.tree.Resolve(root)
		switch kind {
		case gitcandidate.Outside:
			return "", gitcandidate.Missing, newReaderError(ReaderErrorOutsideWorkspace, nil)
		case gitcandidate.Missing:
			return "", gitcandidate.Missing, &fs.PathError{Op: "stat", Path: root, Err: fs.ErrNotExist}
		case gitcandidate.File:
			return "", gitcandidate.Missing, errors.New("feature reader: discovery root is not a directory")
		}
		discoveryRoot = resolved
	}
	joined := path.Join(root, relative)
	target, kind := reader.tree.Resolve(joined)
	switch kind {
	case gitcandidate.Outside:
		return "", gitcandidate.Missing, newReaderError(ReaderErrorSymlinkEscape, nil)
	case gitcandidate.Missing:
		// A present leaf link whose target is not in the cut is malformed
		// rather than an optional absent artifact, as a dangling one is.
		if mode, found := reader.tree.Lstat(joined); found && mode&fs.ModeSymlink != 0 {
			return "", gitcandidate.Missing, newReaderError(ReaderErrorUnsupportedFile, fs.ErrNotExist)
		}
		return "", gitcandidate.Missing, &fs.PathError{Op: "stat", Path: joined, Err: fs.ErrNotExist}
	}
	if discoveryRoot != "." && target != discoveryRoot && !strings.HasPrefix(target, discoveryRoot+"/") {
		return "", gitcandidate.Missing, newReaderError(ReaderErrorSymlinkEscape, nil)
	}
	return target, kind, nil
}
