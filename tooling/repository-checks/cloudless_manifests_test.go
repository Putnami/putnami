package repositorychecks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The shipped manifests of the CLI, the Go framework and extensions, and the
// TypeScript framework and extension name no Putnami Cloud package: a public
// install works without an account.

func TestCloudlessReleaseFixture_PublicManifestsHaveNoCloudDependency(t *testing.T) {
	t.Parallel()
	repoRoot := repositoryRoot(t)
	manifests := []string{filepath.Join(repoRoot, "tooling", "cli", "go.mod")}
	for _, shippedRoot := range []struct {
		path, filename string
	}{
		{filepath.Join(repoRoot, "go", "framework"), "go.mod"},
		{filepath.Join(repoRoot, "go", "extension"), "go.mod"},
		{filepath.Join(repoRoot, "typescript", "extension"), "go.mod"},
		{filepath.Join(repoRoot, "typescript", "framework"), "package.json"},
	} {
		manifests = append(manifests, findShippedManifests(t, shippedRoot.path, shippedRoot.filename)...)
	}
	for _, manifest := range manifests {
		assertManifestContainsNoCloud(t, manifest)
	}
}

func findShippedManifests(t *testing.T, root, filename string) []string {
	t.Helper()
	var manifests []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".putnami", "dist", "node_modules", "out", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == filename {
			manifests = append(manifests, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("discover %s files below %s: %v", filename, root, err)
	}
	if len(manifests) == 0 {
		t.Fatalf("discover %s files below %s: none found", filename, root)
	}
	sort.Strings(manifests)
	return manifests
}

func assertManifestContainsNoCloud(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	contents := string(data)
	if strings.Contains(contents, "@putnami/cloud") || strings.Contains(contents, "putnami"+"-cloud") {
		t.Errorf("public distribution manifest %s links a cloud package", path)
	}
	if strings.HasSuffix(path, "package.json") && !json.Valid([]byte(contents)) {
		t.Fatalf("parse %s: invalid JSON", path)
	}
}
