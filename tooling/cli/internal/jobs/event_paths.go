package jobs

import (
	"os"
	"path/filepath"
	"strings"
)

func workspacePathEventMapper(workspaceRoot, projectRoot, cwd string) eventMapper {
	return func(event RawJobEvent) RawJobEvent {
		if event.Data == nil {
			return event
		}

		if event.Type == EventTypeDiagnostic {
			normalizeDiagnosticEventPaths(event.Data, workspaceRoot, projectRoot, cwd)
		}
		if event.Type == EventTypeArtifact {
			normalizeStringPathField(event.Data, "path", workspaceRoot, projectRoot, cwd)
		}

		return event
	}
}

func normalizeDiagnosticEventPaths(data map[string]any, workspaceRoot, projectRoot, cwd string) {
	normalizeStringPathField(data, "file", workspaceRoot, projectRoot, cwd)

	loc, ok := data["location"].(map[string]any)
	if !ok || loc == nil {
		return
	}
	normalizeStringPathField(loc, "file", workspaceRoot, projectRoot, cwd)
}

func normalizeStringPathField(data map[string]any, key, workspaceRoot, projectRoot, cwd string) {
	file, ok := data[key].(string)
	if !ok || file == "" {
		return
	}
	normalized := normalizeWorkspaceDisplayPath(file, workspaceRoot, projectRoot, cwd)
	if normalized != file {
		data[key] = normalized
	}
}

func normalizeWorkspaceDisplayPath(path, workspaceRoot, projectRoot, cwd string) string {
	if shouldKeepRawPath(path) {
		return path
	}

	workspaceRoot = filepath.Clean(workspaceRoot)
	projectRoot = filepath.Clean(projectRoot)
	if cwd == "" {
		cwd = projectRoot
	}
	if !filepath.IsAbs(cwd) {
		if abs, err := filepath.Abs(cwd); err == nil {
			cwd = abs
		}
	}
	cwd = filepath.Clean(cwd)

	native := filepath.Clean(filepath.FromSlash(path))
	if filepath.IsAbs(native) {
		return workspaceRelOrClean(workspaceRoot, native)
	}

	if projectRel, ok := pathRelativeToWorkspace(workspaceRoot, projectRoot); ok {
		nativeProjectRel := filepath.FromSlash(projectRel)
		if native == nativeProjectRel || strings.HasPrefix(native, nativeProjectRel+string(filepath.Separator)) {
			return cleanDisplayPath(native)
		}
		if idx := strings.Index(native, nativeProjectRel+string(filepath.Separator)); idx > 0 {
			return cleanDisplayPath(native[idx:])
		}
	}

	if workspaceRoot != "" && relativePathExists(workspaceRoot, native) {
		return cleanDisplayPath(native)
	}

	for _, base := range []string{cwd, projectRoot} {
		if rel, ok := existingPathRelativeToWorkspace(workspaceRoot, base, native); ok {
			return rel
		}
	}

	for _, base := range []string{cwd, projectRoot} {
		if rel, ok := lexicalPathRelativeToWorkspace(workspaceRoot, base, native); ok {
			return rel
		}
	}

	return cleanDisplayPath(native)
}

func shouldKeepRawPath(path string) bool {
	if path == "" || strings.HasPrefix(path, "<") || strings.Contains(path, "://") {
		return true
	}
	switch path {
	case "-", ".", "..":
		return true
	default:
		return false
	}
}

func relativePathExists(base, rel string) bool {
	if base == "" || rel == "" || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	if filepath.IsAbs(rel) {
		return false
	}
	if _, err := filepath.Rel(base, filepath.Join(base, rel)); err != nil {
		return false
	}
	return fileExists(filepath.Join(base, rel))
}

func existingPathRelativeToWorkspace(workspaceRoot, base, rel string) (string, bool) {
	if workspaceRoot == "" || base == "" {
		return "", false
	}
	abs := filepath.Clean(filepath.Join(base, rel))
	if !fileExists(abs) {
		return "", false
	}
	return pathRelativeToWorkspace(workspaceRoot, abs)
}

func lexicalPathRelativeToWorkspace(workspaceRoot, base, rel string) (string, bool) {
	if workspaceRoot == "" || base == "" {
		return "", false
	}
	return pathRelativeToWorkspace(workspaceRoot, filepath.Clean(filepath.Join(base, rel)))
}

func pathRelativeToWorkspace(workspaceRoot, abs string) (string, bool) {
	rel, err := filepath.Rel(workspaceRoot, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", false
	}
	return cleanDisplayPath(rel), true
}

func workspaceRelOrClean(workspaceRoot, abs string) string {
	if rel, ok := pathRelativeToWorkspace(workspaceRoot, abs); ok {
		return rel
	}
	return cleanDisplayPath(abs)
}

func cleanDisplayPath(path string) string {
	clean := filepath.Clean(path)
	clean = strings.TrimPrefix(clean, "."+string(filepath.Separator))
	return filepath.ToSlash(clean)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
