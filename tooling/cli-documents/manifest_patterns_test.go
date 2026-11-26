package documents

import "path/filepath"

// projectRelativePath returns a repository-relative path in the form
// filepath.Rel produces against a project root — the form collectFiles and
// store.SelectsPath both match.
func projectRelativePath(projectDir, target string) string {
	rel, err := filepath.Rel(filepath.FromSlash(projectDir), filepath.FromSlash(target))
	if err != nil {
		return target
	}
	return filepath.ToSlash(rel)
}
