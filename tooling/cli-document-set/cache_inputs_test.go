package documentset

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

// documentsOwnedRepositoryFiles are the repository files the two document
// projects — and only they among the CLI's test projects — read, each with the
// project that must key it. Keeping them out of @putnami/cli's declared test
// inputs is what makes a documentation edit a cache hit for the CLI's 5 000-test
// suite instead of a full re-run.
//
// A file a gate of this project reads is attributed to this project, even when
// a whole-tree gate of @putnami/cli-documents reads it too (CONTRIBUTING.md,
// SECURITY.md): that project leaves the per-change gate, so only this
// project's key keeps the per-change verdict fresh. The other files are read by
// the whole-tree gates alone. A file a test of @putnami/cli also reads
// (putnami.workspace.json, putnami.ci.json, putnami.support.json,
// decisions.json, the first-use smoke script, the agent-artifact trees) is
// absent here on purpose: it is honestly declared twice, because two readers
// exist.
var documentsOwnedRepositoryFiles = map[string]string{
	".github/PULL_REQUEST_TEMPLATE.md": cliDocumentsProjectDir,
	".gitignore":                       cliDocumentsProjectDir,
	"CODE_OF_CONDUCT.md":               cliDocumentsProjectDir,
	"CONTRIBUTING.md":                  documentSetProjectDir,
	"GOVERNANCE.md":                    documentSetProjectDir,
	"LICENSE.md":                       cliDocumentsProjectDir,
	"README.md":                        documentSetProjectDir,
	"RELEASE.md":                       cliDocumentsProjectDir,
	"RELEASING.md":                     documentSetProjectDir,
	"SECURITY.md":                      documentSetProjectDir,
	"tooling/extension-sdk/pkgmeta/platforms.go":                  cliDocumentsProjectDir,
	"protocols/cli/testdata/prior-releases/provenance.json":       cliDocumentsProjectDir,
	"protocols/extension/testdata/prior-releases/provenance.json": cliDocumentsProjectDir,
	"protocols/support/README.md":                                 documentSetProjectDir,
	"sites/putnami.dev/doc/01-getting-started/index.md":           documentSetProjectDir,
	"tooling/CHANGELOG.md":                                        cliDocumentsProjectDir,
}

// TestRepositoryDocumentsAreKeyedToThisProjectOnly is the regression guard for
// the split. Before the split, every one of these files was a declared test input of
// @putnami/cli, so editing CONTRIBUTING.md alone re-ran the whole CLI suite
// twice per gate. Re-adding one there fails here, naming the file and the cost.
// A document a gate of this project reads must also be a declared input of
// this project, not only of the whole-tree project: that one
// leaves the per-change gate, so its key keeps no per-change verdict fresh.
func TestRepositoryDocumentsAreKeyedToThisProjectOnly(t *testing.T) {
	t.Parallel()
	cliPatterns := declaredTestFilePatterns(t, cliProjectDir)
	readerPatterns := map[string][]string{
		documentSetProjectDir:  declaredTestFilePatterns(t, documentSetProjectDir),
		cliDocumentsProjectDir: declaredTestFilePatterns(t, cliDocumentsProjectDir),
	}

	files := make([]string, 0, len(documentsOwnedRepositoryFiles))
	for file := range documentsOwnedRepositoryFiles {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		reader := documentsOwnedRepositoryFiles[file]
		if !store.SelectsPath(projectRelativePath(reader, file), readerPatterns[reader]) {
			t.Errorf("%s is read by a gate in %s, but no declared test input of that project "+
				"selects it, so a warm cache answers a changed document with the previous verdict.",
				file, reader)
		}
		if store.SelectsPath(projectRelativePath(cliProjectDir, file), cliPatterns) {
			t.Errorf("%s is a declared test input of @putnami/cli again, so editing it re-runs "+
				"the CLI's whole test suite. No test under tooling/cli reads it: move the "+
				"reader to the document projects instead of re-declaring the document there.", file)
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

// siteDocumentationTarget is the site path under which the moved-commands
// check looks for the page the CLI cites.
const siteDocumentationTarget = "public/docs"

// TestPublishedDocumentationIsADeclaredInput keeps the declared Markdown inputs
// equal to what TestCommandsThatLeftTheCoreURLNamesAPublishedHeading reads:
// every Markdown file under every documentation folder the site copies under
// public/docs. The declaration names those folders instead of every Markdown
// file of the repository, so a folder the site starts to publish has to be
// declared here too, or a warm cache answers its edit with the previous pass.
func TestPublishedDocumentationIsADeclaredInput(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	patterns := declaredTestFilePatterns(t, documentSetProjectDir)
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(siteManifest)))
	if err != nil {
		t.Fatalf("read %s: %v", siteManifest, err)
	}
	var manifest struct {
		Options struct {
			Generate struct {
				Assets []struct {
					From string `json:"from"`
					To   string `json:"to"`
				} `json:"assets"`
			} `json:"generate"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse %s: %v", siteManifest, err)
	}
	checked := 0
	for _, asset := range manifest.Options.Generate.Assets {
		if asset.To != siteDocumentationTarget && !strings.HasPrefix(asset.To, siteDocumentationTarget+"/") {
			continue
		}
		source := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(asset.From, "/")))
		err := filepath.WalkDir(source, func(file string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || filepath.Ext(file) != ".md" {
				return err
			}
			rel, err := filepath.Rel(root, file)
			if err != nil {
				return err
			}
			checked++
			if !store.SelectsPath(projectRelativePath(documentSetProjectDir, filepath.ToSlash(rel)), patterns) {
				t.Errorf("%s is published under %s (asset %s), but no declared test input of "+
					"@putnami/cli-document-set selects it; declare its folder in options.test.filePatterns",
					filepath.ToSlash(rel), asset.To, asset.From)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", source, err)
		}
	}
	if checked == 0 {
		t.Fatalf("%s publishes no Markdown file under %s, so this check proves nothing", siteManifest, siteDocumentationTarget)
	}
}
