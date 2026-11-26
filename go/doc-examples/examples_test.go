package docexamples

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocumentationExamplesTypeCheck type-checks every Go block of the
// documents examples.json names. A finding names the document and line of the
// failing code: fix the example, or mark its fence `nocheck` when it is not
// meant to compile.
func TestDocumentationExamplesTypeCheck(t *testing.T) {
	root := workspaceRoot(t)
	manifest, err := ReadManifest("examples.json")
	if err != nil {
		t.Fatal(err)
	}
	documents, err := manifest.Documents(root)
	if err != nil {
		t.Fatal(err)
	}
	var examples []Example
	for _, document := range documents {
		data, err := os.ReadFile(filepath.Join(root, document))
		if err != nil {
			t.Fatal(err)
		}
		for _, block := range Blocks(string(data)) {
			if block.Lang == "go" && block.Checked() {
				examples = append(examples, Example{Doc: document, Block: block})
			}
		}
	}
	if len(examples) == 0 {
		t.Fatal("the documents hold no checked Go example")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	checker, err := NewChecker(wd)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := checker.Check(examples)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		t.Error(finding)
	}
	// The test's declared inputs decide when it runs again and what its cache
	// key holds: each document it checks and each workspace package the
	// examples import must be one of them.
	inputs := testInputs(t, root, wd)
	for _, document := range documents {
		if !covered(inputs, document) {
			t.Errorf("%s is checked but no test filePatterns entry of putnami.json covers it; add one", document)
		}
	}
	for _, dir := range checker.WorkspaceDirs() {
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			t.Fatal(err)
		}
		if !covered(inputs, filepath.ToSlash(rel)+"/doc.go") {
			t.Errorf("the examples import the package in %s but no test filePatterns entry of putnami.json covers it; add one", rel)
		}
	}
	for _, file := range checker.WorkspaceEmbeds() {
		rel, err := filepath.Rel(root, file)
		if err != nil {
			t.Fatal(err)
		}
		if !covered(inputs, filepath.ToSlash(rel)) {
			t.Errorf("the examples compile a package that embeds %s but no test filePatterns entry of putnami.json covers it; add one", rel)
		}
	}
	t.Logf("%d Go examples in %d documents, %d findings", len(examples), len(documents), len(findings))
}

// testInputs returns the test filePatterns of putnami.json, relative to the
// workspace root.
func testInputs(t *testing.T, root, project string) []string {
	t.Helper()
	data, err := os.ReadFile("putnami.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Options struct {
			Test struct {
				FilePatterns []string `json:"filePatterns"`
			} `json:"test"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, project)
	if err != nil {
		t.Fatal(err)
	}
	var inputs []string
	for _, pattern := range manifest.Options.Test.FilePatterns {
		exclude := strings.HasPrefix(pattern, "!")
		pattern = path.Clean(path.Join(filepath.ToSlash(rel), strings.TrimPrefix(pattern, "!")))
		if exclude {
			pattern = "!" + pattern
		}
		inputs = append(inputs, pattern)
	}
	return inputs
}

// covered reports whether the patterns select name: an include matches it and
// no `!` exclusion does, the way the engine reads test filePatterns.
func covered(patterns []string, name string) bool {
	selected := false
	for _, pattern := range patterns {
		if exclusion, ok := strings.CutPrefix(pattern, "!"); ok {
			if Match(exclusion, name) {
				return false
			}
			continue
		}
		if Match(pattern, name) {
			selected = true
		}
	}
	return selected
}

func workspaceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no putnami.workspace.json above the test directory")
		}
		dir = parent
	}
}
