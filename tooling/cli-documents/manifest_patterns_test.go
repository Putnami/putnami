package documents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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

// declaredTestFilePatterns reads options.test.filePatterns of one project.
func declaredTestFilePatterns(t *testing.T, projectDir string) []string {
	t.Helper()
	manifest := filepath.Join(repositoryRoot(t), filepath.FromSlash(projectDir), "putnami.json")
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read %s: %v", manifest, err)
	}
	var parsed struct {
		Options struct {
			Test struct {
				FilePatterns []string `json:"filePatterns"`
			} `json:"test"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse %s: %v", manifest, err)
	}
	if len(parsed.Options.Test.FilePatterns) == 0 {
		t.Fatalf("%s declares no test filePatterns, so nothing is keyed", manifest)
	}
	return parsed.Options.Test.FilePatterns
}
