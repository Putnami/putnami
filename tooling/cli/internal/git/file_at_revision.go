package git

import (
	"path"
	"path/filepath"
	"strings"
)

// FileAtRevision returns the bytes a commit records for a path relative to
// repoRoot, and false when the commit records no such file or git cannot read
// it. The bytes are the stored blob: no checkout filter or line-ending
// conversion applies, so a caller compares content, never layout.
//
// The path is read relative to repoRoot, not to the repository's top level, so
// a workspace nested inside a larger repository names its own files. A path
// that is absolute, climbs out of repoRoot, or carries a control character
// reads nothing.
func FileAtRevision(repoRoot, revision, file string) ([]byte, bool) {
	if !validRevisionArgument(revision) || strings.Contains(revision, ":") {
		return nil, false
	}
	rel, ok := revisionRelativePath(file)
	if !ok {
		return nil, false
	}
	output, err := run(repoRoot, "cat-file", "blob", revision+":./"+rel)
	if err != nil {
		return nil, false
	}
	return []byte(output), true
}

// revisionRelativePath cleans a repoRoot-relative path into the slash form a
// `<revision>:./<path>` object name reads, and refuses one that would name
// anything outside repoRoot.
func revisionRelativePath(file string) (string, bool) {
	if file == "" || filepath.IsAbs(file) || filepath.VolumeName(file) != "" {
		return "", false
	}
	for _, character := range file {
		if character < 0x20 || character == 0x7f {
			return "", false
		}
	}
	cleaned := path.Clean(filepath.ToSlash(file))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", false
	}
	return cleaned, true
}
