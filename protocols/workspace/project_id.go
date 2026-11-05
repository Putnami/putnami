package workspace

import (
	"path/filepath"
	"strings"
)

// ProjectIDFromPath derives a project's canonical logical ID from its physical
// workspace-relative path: "/" followed by the cleaned path without its
// grouping folders, and "/" for the workspace root. A grouping folder is a
// complete, non-empty parenthesized segment, such as "(web)": it stays in the
// project's path and never appears in its ID.
func ProjectIDFromPath(projectPath string) string {
	physicalPath := CleanWorkspacePath(projectPath)
	if physicalPath == "" {
		return "/"
	}

	segments := strings.Split(physicalPath, "/")
	logicalSegments := make([]string, 0, len(segments))
	for _, segment := range segments {
		if isTransparentGroupSegment(segment) {
			continue
		}
		logicalSegments = append(logicalSegments, segment)
	}
	return "/" + strings.Join(logicalSegments, "/")
}

func isTransparentGroupSegment(segment string) bool {
	return len(segment) > 2 && segment[0] == '(' && segment[len(segment)-1] == ')'
}

// CleanWorkspacePath normalizes a workspace-relative path to its canonical
// slash form: surrounding space and one leading "/" removed, "." and ".."
// segments and doubled separators folded, and "" for the workspace root.
func CleanWorkspacePath(path string) string {
	path = strings.TrimSpace(strings.TrimPrefix(path, "/"))
	if path == "" {
		return ""
	}
	path = filepath.Clean(filepath.FromSlash(path))
	if path == "." {
		return ""
	}
	return filepath.ToSlash(path)
}
