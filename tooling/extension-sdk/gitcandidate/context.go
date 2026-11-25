package gitcandidate

import (
	"path/filepath"
	"strings"

	job "go.putnami.dev/protocol/job"
)

// OpenFrom returns the candidate cut of root for the job whose context is ctx.
// It answers from the context's candidate cut when it covers root, the
// `candidateFiles` or `workspaceCandidateFiles` runtime input
// (go.putnami.dev/protocol/job, doc/adr/0003), and starts no Git: the cut is
// the one the task's key holds, read when the run started. Otherwise, when the
// task declared neither, the orchestrator could not read the cut or an older
// orchestrator wrote none, it is Open.
func OpenFrom(ctx *job.Context, root string) (*Tree, error) {
	root = filepath.Clean(root)
	if listed, index, ok := fromContext(ctx, root); ok {
		return newTreeIndexed(root, listed, index)
	}
	return Open(root)
}

// ContextCut returns the cut at or below root the job context carries, each
// path relative to root with its index mode, and false when the context does
// not cover root (OpenFrom then asks Git). A caller that needs the modes of the
// cut, such as a source binding, reads them here.
func ContextCut(ctx *job.Context, root string) ([]job.CandidateFile, bool) {
	files, ok := contextFiles(ctx, filepath.Clean(root))
	if !ok {
		return nil, false
	}
	return files, true
}

// fromContext returns the cut at or below root the context carries, as paths
// relative to root, and their index modes.
func fromContext(ctx *job.Context, root string) ([]string, *indexModes, bool) {
	files, ok := contextFiles(ctx, root)
	if !ok {
		return nil, nil, false
	}
	listed := make([]string, 0, len(files))
	index := &indexModes{Modes: map[string]string{}, Unmerged: map[string]bool{}}
	for _, file := range files {
		listed = append(listed, file.Path)
		switch file.Mode {
		case job.CandidateModeUntracked:
		case job.CandidateModeUnmerged:
			index.Unmerged[file.Path] = true
		default:
			index.Modes[file.Path] = file.Mode
		}
	}
	return listed, index, true
}

// contextFiles returns the cut at or below root the context carries, each
// path relative to root.
func contextFiles(ctx *job.Context, root string) ([]job.CandidateFile, bool) {
	if ctx == nil || ctx.Tree == nil || ctx.WorkspaceRoot == "" {
		return nil, false
	}
	rel, err := filepath.Rel(filepath.Clean(ctx.WorkspaceRoot), root)
	if err != nil {
		return nil, false
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return nil, false
	}
	tree := ctx.Tree
	var files []job.CandidateFile
	switch {
	case tree.CandidateFiles != nil && covers(tree.CandidateDirectories, rel):
		files = tree.CandidateFiles
	case tree.WorkspaceCandidates != nil:
		read, err := job.ReadCandidateListing(tree.WorkspaceCandidates)
		if err != nil {
			return nil, false
		}
		files = read
	default:
		return nil, false
	}
	local := []job.CandidateFile{}
	for _, file := range files {
		// A cut entry at or above the root asked (a submodule, a nested
		// repository, a link) lists no contents: only Git reads below it.
		if rel != "." && (file.Path == rel || strings.HasPrefix(rel, file.Path+"/")) {
			return nil, false
		}
		switch {
		case rel == ".":
			local = append(local, file)
		case strings.HasPrefix(file.Path, rel+"/"):
			local = append(local, job.CandidateFile{Path: file.Path[len(rel)+1:], Mode: file.Mode})
		}
	}
	return local, true
}

// covers reports whether rel lies at or below one of directories.
func covers(directories []string, rel string) bool {
	for _, directory := range directories {
		if directory == "." || rel == directory || strings.HasPrefix(rel, directory+"/") {
			return true
		}
	}
	return false
}

// newTreeIndexed is newTree, with the index modes the context gave.
func newTreeIndexed(root string, listed []string, index *indexModes) (*Tree, error) {
	tree, err := newTree(root, listed)
	if err != nil {
		return nil, err
	}
	tree.index = index
	return tree, nil
}
