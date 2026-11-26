package documentset

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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

// documentsOwnedRepositoryFiles are the repository files the document checks
// — and no test of @putnami/cli — read, each with the project whose gate reads
// it. Keeping them out of @putnami/cli's declared test inputs is what makes a
// documentation edit a cache hit for the CLI's 5 000-test suite instead of a
// full re-run.
//
// A file a gate of this project reads is attributed to this project, even when
// the whole-tree checks of @putnami/repository-checks read it too
// (CONTRIBUTING.md): each project's key must cover what its own gates read,
// whatever another project declares. The other files are read by the
// whole-tree checks alone. A file a test of @putnami/cli also reads
// (putnami.workspace.json, putnami.ci.json, putnami.support.json, the
// first-use smoke script, the agent-artifact trees) is absent here on purpose:
// it is honestly declared twice, because two readers exist.
var documentsOwnedRepositoryFiles = map[string]string{
	".github/PULL_REQUEST_TEMPLATE.md": repositoryChecksProjectDir,
	".gitignore":                       repositoryChecksProjectDir,
	"CODE_OF_CONDUCT.md":               repositoryChecksProjectDir,
	"CONTRIBUTING.md":                  documentSetProjectDir,
	"GOVERNANCE.md":                    repositoryChecksProjectDir,
	"LICENSE.md":                       repositoryChecksProjectDir,
	"README.md":                        repositoryChecksProjectDir,
	"RELEASE.md":                       repositoryChecksProjectDir,
	"RELEASING.md":                     repositoryChecksProjectDir,
	"SECURITY.md":                      repositoryChecksProjectDir,
	"tooling/extension-sdk/pkgmeta/platforms.go":                  repositoryChecksProjectDir,
	"protocols/cli/testdata/prior-releases/provenance.json":       repositoryChecksProjectDir,
	"protocols/extension/testdata/prior-releases/provenance.json": repositoryChecksProjectDir,
	"protocols/support/README.md":                                 repositoryChecksProjectDir,
	"sites/putnami.dev/doc/01-getting-started/index.md":           repositoryChecksProjectDir,
	"tooling/CHANGELOG.md":                                        repositoryChecksProjectDir,
}

// TestRepositoryDocumentsAreKeyedToThisProjectOnly is the regression guard for
// the split: re-declaring one of these files as a test input of @putnami/cli
// fails here, naming the file and the cost. A document a gate reads must also
// be a declared input of the gate's own project: a key answers for its own
// project's verdict alone.
func TestRepositoryDocumentsAreKeyedToThisProjectOnly(t *testing.T) {
	t.Parallel()
	cliPatterns := declaredTestFilePatterns(t, cliProjectDir)
	readerPatterns := map[string][]string{
		documentSetProjectDir:      declaredTestFilePatterns(t, documentSetProjectDir),
		repositoryChecksProjectDir: declaredTestFilePatterns(t, repositoryChecksProjectDir),
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
