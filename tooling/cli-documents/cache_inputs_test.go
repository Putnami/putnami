package documents

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/store"
)

// The two properties that make the split hold, checked against the
// same matcher the scheduler keys with.
//
// A project's `options.<command>.filePatterns` fold into the cache key of EVERY
// task of that command, so "which project declares this document" is the only
// lever there is. Both assertions below therefore read the committed manifests
// rather than restating a list: a copy would pass against a declaration the
// scheduler does not use.

const cliProjectDir = cliModuleDir

// documentsOwnedRepositoryFiles are the repository documents this project — and
// only this project — reads. Keeping them out of @putnami/cli's declared test
// inputs is what makes a documentation edit a cache hit for the CLI's 5 000-test
// suite instead of a full re-run.
//
// A file read by a test on BOTH sides (putnami.workspace.json, putnami.ci.json,
// putnami.support.json, decisions.json, the agent-artifact trees) is absent
// here on purpose: it is honestly declared twice, because two readers exist.
var documentsOwnedRepositoryFiles = []string{
	".github/PULL_REQUEST_TEMPLATE.md",
	".gitignore",
	"CODE_OF_CONDUCT.md",
	"CONTRIBUTING.md",
	"GOVERNANCE.md",
	"LICENSE.md",
	"README.md",
	"RELEASE.md",
	"RELEASING.md",
	"SECURITY.md",
	"tooling/extension-sdk/pkgmeta/platforms.go",
	"protocols/cli/testdata/prior-releases/provenance.json",
	"protocols/extension/testdata/prior-releases/provenance.json",
	"protocols/support/README.md",
	"tooling/CHANGELOG.md",
}

// TestRepositoryDocumentsAreKeyedToThisProjectOnly is the regression guard for
// the split. Before the split, every one of these files was a declared test input of
// @putnami/cli, so editing CONTRIBUTING.md alone re-ran the whole CLI suite
// twice per gate. Re-adding one there fails here, naming the file and the cost.
func TestRepositoryDocumentsAreKeyedToThisProjectOnly(t *testing.T) {
	t.Parallel()
	documentsPatterns := declaredTestFilePatterns(t, cliDocumentsProjectDir)
	cliPatterns := declaredTestFilePatterns(t, cliProjectDir)

	for _, file := range documentsOwnedRepositoryFiles {
		if !store.SelectsPath(projectRelativePath(cliDocumentsProjectDir, file), documentsPatterns) {
			t.Errorf("%s is read by a gate in this project, but no declared test input of "+
				"@putnami/cli-documents selects it, so a warm cache answers a changed document "+
				"with the previous verdict.", file)
		}
		if store.SelectsPath(projectRelativePath(cliProjectDir, file), cliPatterns) {
			t.Errorf("%s is a declared test input of @putnami/cli again, so editing it re-runs "+
				"the CLI's whole test suite. No test under tooling/cli reads it: move the "+
				"reader here instead of re-declaring the document there.", file)
		}
	}
}

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
	patterns := manifestFilePatterns(t, manifest)
	if len(patterns) == 0 {
		t.Fatalf("%s declares no test filePatterns, so nothing is keyed", manifest)
	}
	return patterns
}

func manifestFilePatterns(t *testing.T, manifestPath string) []string {
	t.Helper()
	return readManifestOptions(t, manifestPath)["test"].FilePatterns
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

type manifestOptionLayer struct {
	FilePatterns []string `json:"filePatterns"`
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
