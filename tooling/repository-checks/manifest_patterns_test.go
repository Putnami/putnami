package repositorychecks

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestDeclaredFilePatternsAreInTheFormTheCacheKeyMatches pins the spelling of a
// pattern that reaches outside its project.
//
// collectFiles matches a "**" pattern against the CLEANED relative path
// filepath.Rel produces, so a sibling project is ONE level up: a pattern
// spelled "../../tooling/contributor/src/**" from tooling/cli selects
// nothing at all, while "../contributor/src/**" selects the tree. The two
// spellings denote the same directory, which is exactly why the broken one
// survives review — and an input that selects nothing is a cache that answers a
// changed tree with a stale pass.
func TestDeclaredFilePatternsAreInTheFormTheCacheKeyMatches(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)

	inspected := 0
	var findings []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".putnami", "node_modules", "out", "dist", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "putnami.json" {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return nil
		}
		for _, pattern := range allManifestFilePatterns(t, path) {
			if !strings.HasPrefix(pattern, "..") {
				continue
			}
			inspected++
			canonical, err := filepath.Rel(rel, filepath.Join(rel, filepath.FromSlash(pattern)))
			if err != nil {
				continue
			}
			if canonical := filepath.ToSlash(canonical); canonical != pattern {
				findings = append(findings, filepath.ToSlash(rel)+": "+pattern+" selects nothing; write "+canonical)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the workspace: %v", err)
	}
	if inspected == 0 {
		t.Fatal("no project declares a pattern reaching outside its root, so this guard proved nothing")
	}
	if len(findings) > 0 {
		sort.Strings(findings)
		t.Fatalf("declared cache-key inputs that the matcher never selects:\n  %s",
			strings.Join(findings, "\n  "))
	}
}

// manifestOptionLayer is the part of one option layer of a project manifest
// these checks read.
type manifestOptionLayer struct {
	FilePatterns []string `json:"filePatterns"`
}

// allManifestFilePatterns returns the patterns of EVERY option layer, because
// the layer a project keys its declaration under (options.test,
// options."/go/extension", options."@putnami/go:test") is its choice and the
// spelling rule is the same for all of them.
func allManifestFilePatterns(t *testing.T, manifestPath string) []string {
	t.Helper()
	var patterns []string
	options := readManifestOptions(t, manifestPath)
	for _, layer := range sortedLayers(options) {
		patterns = append(patterns, options[layer].FilePatterns...)
	}
	return patterns
}

func readManifestOptions(t *testing.T, manifestPath string) map[string]manifestOptionLayer {
	t.Helper()
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read %s: %v", manifestPath, err)
	}
	var manifest struct {
		Options map[string]manifestOptionLayer `json:"options"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse %s: %v", manifestPath, err)
	}
	return manifest.Options
}

func sortedLayers(options map[string]manifestOptionLayer) []string {
	layers := make([]string, 0, len(options))
	for layer := range options {
		layers = append(layers, layer)
	}
	sort.Strings(layers)
	return layers
}
